package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  接入中心 / 流程引擎回归测试
//  设计依据：PLAN-采集接入中心与流程引擎.md §8（验收标准）
//
//  这里覆盖的是「引擎语义」，不是「某次请求的返回」：
//    - when 白名单求值器（未知变量不 panic、不求值任意表达式）
//    - 模板结构自洽（原子能力全覆盖、依赖前向、id 不重复）
//    - 推进器语义（平台侧当场完成 / 外部执行器下发后停下 / when 不满足即跳过）
//    - 门禁语义（前置失败 → 下游 blocked 而非 pending；失败是终态，必须显式重试）
//    - 能力清单（参数声明来自插件包 params.yaml，路径写错必须报错而不是静默为空）
// ===================================================================

// ---------- 脚手架 ----------

func testOnboardDB(t *testing.T) *storepkg.DB {
	t.Helper()
	return openTestCatalog(t)
}

// 引擎依赖包级 onboardCfg（main 启动时装载），测试里用真实配置文件装载，
// 这样配置与数据文件一旦脱节（少能力、路径错）测试立刻红。
func testOnboardCfg(t *testing.T) *OnboardConfig {
	t.Helper()
	cfg := loadOnboardConfig("data/onboard_config.json")
	if cfg == nil {
		t.Fatal("loadOnboardConfig returned nil")
	}
	onboardCfg = cfg
	return cfg
}

// latestStatuses 把事件表 hydrate 成 {步骤: 最新状态}
func latestStatuses(t *testing.T, db *storepkg.DB, flowID int64) map[string]string {
	t.Helper()
	events, err := db.ListEvents(flowID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := map[string]string{}
	for _, e := range events {
		out[e.Step] = e.Status
	}
	return out
}

func makeFlow(t *testing.T, db *storepkg.DB, resID string, steps []storepkg.FlowStepSnapshot) int64 {
	t.Helper()
	if err := db.UpsertResource(&storepkg.Resource{
		ID: resID, Name: resID, ResourceType: "host", Role: "business", Source: "manual",
	}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	id, err := db.CreateFlow(&storepkg.Flow{
		ResourceID: resID, Mode: "edge", TemplateID: "edge",
		Steps: steps, Status: "running",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	return id
}

// ---------- 1. when 白名单求值器 ----------

func TestEvalWhenWhitelist(t *testing.T) {
	env := map[string]bool{"agent_exists": true, "abilities_requires_params": false}
	cases := []struct {
		expr string
		mode string
		want bool
	}{
		{"", "edge", true}, // 空 = 无条件生效
		{"agent_exists", "edge", true},
		{"!agent_exists", "edge", false},
		{"!agent_exists && abilities_requires_params", "edge", false},
		{"!agent_exists || abilities_requires_params", "edge", false},
		{"agent_exists && !abilities_requires_params", "edge", true},
		{"mode == 'edge'", "edge", true},
		{"mode != 'edge'", "edge", false},
		{"mode == 'remote'", "remote", true},
		{"no_such_flag", "edge", false}, // 未声明变量 = false，绝不 panic
		{"!no_such_flag", "edge", true},
		{"  agent_exists  ", "edge", true}, // 容忍空白
	}
	for _, c := range cases {
		if got := evalWhen(c.expr, env, c.mode); got != c.want {
			t.Errorf("evalWhen(%q, mode=%s) = %v, want %v", c.expr, c.mode, got, c.want)
		}
	}
}

// ---------- 2. 模板结构自洽 ----------

func TestFlowTemplatesStructure(t *testing.T) {
	tpls, err := loadFlowTemplates(defaultFlowTemplateDir)
	if err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	if len(tpls) != 6 {
		t.Fatalf("want 6 mode templates, got %d", len(tpls))
	}
	ids := []string{}
	for _, tp := range tpls {
		ids = append(ids, tp.ID)
	}
	if strings.Join(ids, ",") != "edge,hybrid,offboard,remote,service,upgrade" {
		t.Fatalf("template ids = %v, want edge,hybrid,offboard,remote,service,upgrade", ids)
	}

	for _, tp := range tpls {
		seen := map[string]bool{}
		for i, s := range tp.Steps {
			if s.ID == "" || s.Atom == "" {
				t.Errorf("[%s] step #%d 缺 id/atom: %+v", tp.ID, i, s)
				continue
			}
			if _, ok := atomRegistry[s.Atom]; !ok {
				t.Errorf("[%s] 步骤 %s 用了未注册的原子能力 %q（引擎不认识 → 这条流水线必然卡住）", tp.ID, s.ID, s.Atom)
			}
			if seen[s.ID] {
				t.Errorf("[%s] 步骤 id 重复: %q", tp.ID, s.ID)
			}
			for _, dep := range s.Requires {
				if !seen[dep] {
					t.Errorf("[%s] 步骤 %s 的依赖 %q 不是更早的步骤（依赖必须前向，否则永远等不到）", tp.ID, s.ID, dep)
				}
			}
			seen[s.ID] = true
		}
	}
}

// 门禁接线：blocker 必须落在「外部探路」这类会失败的步骤上，且配置下发必须等它
func TestEdgeTemplateGateWiring(t *testing.T) {
	tpls, err := loadFlowTemplates(defaultFlowTemplateDir)
	if err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	var edge *FlowTemplate
	for _, tp := range tpls {
		if tp.ID == "edge" {
			edge = tp
		}
	}
	if edge == nil {
		t.Fatal("edge template missing")
	}
	byID := map[string]FlowTemplateSt{}
	for _, s := range edge.Steps {
		byID[s.ID] = s
	}
	pf, ok := byID["preflight_host"]
	if !ok {
		t.Fatal("edge 模板缺 preflight_host")
	}
	if pf.Gate != "blocker" {
		t.Errorf("preflight_host 应为 blocker 门禁，实际 %q", pf.Gate)
	}
	sc, ok := byID["sync_config"]
	if !ok {
		t.Fatal("edge 模板缺 sync_config")
	}
	if !containsStr(sc.Requires, "preflight_host") {
		t.Errorf("sync_config 必须等探路通过（能力检查已并入 preflight_host），实际 requires=%v", sc.Requires)
	}
	// 2026-09-21 合并：edge 模板不许再出现独立的 preflight_ability 环节
	if _, exists := byID["preflight_ability"]; exists {
		t.Errorf("edge 模板不应再有独立 preflight_ability（已并入 preflight_host）")
	}
}

// ---------- 2.5 探路环境预检：接入点可达性三态（2026-09-21 用户评审） ----------

func TestParseEnvCheckThreeStates(t *testing.T) {
	now := int64(1789000000)

	// curl 可达：200 + 工具名，且时间偏差取绝对值
	got := parseEnvCheck("PY:Python 3.11.4\nEPOCH:1788999990\nCONN:200 TOOL:curl\n", now)
	if got.connCode != "200" || got.connTool != "curl" || got.python != "Python 3.11.4" || got.skewSec != 10 {
		t.Errorf("curl 可达解析错误: %+v", got)
	}

	// 靶机无 curl（alpine 常态）回落 wget，wget 也探不通（隧道断/地址错）→ FAIL：真不可达
	got = parseEnvCheck("PY:Python 3.11.4\nEPOCH:1788999990\nCONN:FAIL TOOL:wget\n", now)
	if got.connCode != "FAIL" || got.connTool != "wget" {
		t.Errorf("wget 失败应解析为 FAIL/wget: %+v", got)
	}

	// curl+wget 都没有 → NO_TOOL：未核实。绝不能把「工具不在」当成「不可达」误伤精简系统
	got = parseEnvCheck("PY:NONE\nEPOCH:1788999990\nCONN:NO_TOOL TOOL:none\n", now)
	if got.connCode != "NO_TOOL" || got.python != "none" {
		t.Errorf("无探测工具应解析为 NO_TOOL/无 Python: %+v", got)
	}

	// CONN 与 TOOL 同行——switch 拆成两个 case 会因先命中 TOOL 吞掉 CONN（真实踩过）
	got = parseEnvCheck("EPOCH:1788999990\nCONN:404 TOOL:curl\n", now)
	if got.connCode != "404" || got.connTool != "curl" {
		t.Errorf("同行 CONN+TOOL 必须都解析出来: %+v", got)
	}

	// 空输出 → 结论为空 → 判「未核实」，不许默认通过
	if got := parseEnvCheck("", now); got.connCode != "" {
		t.Errorf("空输出应得到空 connCode: %+v", got)
	}
}

func TestAccessPointVerdictGate(t *testing.T) {
	// 可达：info 留痕，放行
	ds, fatal := accessPointVerdict("200", "curl", "http://x:8080")
	if fatal || len(ds) != 1 || ds[0].Level != "info" {
		t.Errorf("200 应 info 放行，实际 fatal=%v ds=%+v", fatal, ds)
	}
	// 双工具探不通：fatal，装机之前就地拦住
	ds, fatal = accessPointVerdict("FAIL", "wget", "http://x:8080")
	if !fatal || len(ds) != 1 || ds[0].Level != "fatal" || ds[0].Code != "access_point_unreachable" {
		t.Errorf("FAIL 应 fatal 拦截，实际 fatal=%v ds=%+v", fatal, ds)
	}
	// 非 200（端点错/网关错）同样 fatal
	if _, fatal = accessPointVerdict("404", "curl", "http://x:8080"); !fatal {
		t.Errorf("404 应 fatal 拦截")
	}
	// 无探测工具：未核实，只告警不误伤
	ds, fatal = accessPointVerdict("NO_TOOL", "none", "http://x:8080")
	if fatal || len(ds) != 1 || ds[0].Code != "access_point_unverified" {
		t.Errorf("NO_TOOL 应未核实告警，实际 fatal=%v ds=%+v", fatal, ds)
	}
	// 输出异常（空结论）：同样只告警
	if _, fatal = accessPointVerdict("", "", "http://x:8080"); fatal {
		t.Errorf("空结论应只告警不拦截")
	}
}

// 接入点兜底候选（2026-09-21 用户：本机容器目标与跨网段真实主机双兼容）：
// 候选清单要剔除已配置的地址（再探一遍已知失败的地址没有信息量），CAND 行解析要扛住 URL 自带冒号
func TestAccessPointCandidatesAndProbe(t *testing.T) {
	// 默认候选：两类目标环境各一条
	cands := accessPointCandidates("")
	if len(cands) != 2 {
		t.Fatalf("应有 2 个默认候选（本机容器 / 同 Docker 网络），实际 %d", len(cands))
	}
	// 配置值与候选重复时剔除
	if cands := accessPointCandidates(" http://host.docker.internal:8080 "); len(cands) != 1 || cands[0].URL != "http://l0-console:8080" {
		t.Errorf("配置了 host.docker.internal 时应只剩 l0-console 候选，实际 %+v", cands)
	}

	// CAND 行解析：URL 带 :// 和 :8080，按"最后两个冒号"切
	out := "CAND:http://host.docker.internal:8080:200:wget\n" +
		"CAND:http://l0-console:8080:FAIL:wget\n" +
		"其他噪声行\n" +
		"CAND:残缺行"
	got := parseCandidateProbe(out)
	if len(got) != 1 || got[0].URL != "http://host.docker.internal:8080" || got[0].Tool != "wget" {
		t.Errorf("只应回收可达候选且 URL/工具切分正确，实际 %+v", got)
	}
	if got := parseCandidateProbe(""); len(got) != 0 {
		t.Errorf("空输出应无候选，实际 %+v", got)
	}

	// 探测命令必须包含全部候选 URL，且与主接入点同一条 curl→wget 链
	cmd := candidateProbeCmd([]string{"http://a:1", "http://b:2"})
	if !strings.Contains(cmd, "http://a:1") || !strings.Contains(cmd, "http://b:2") ||
		!strings.Contains(cmd, "command -v wget") || !strings.Contains(cmd, "NO_TOOL") {
		t.Errorf("候选探测命令缺 URL 或缺 curl→wget 回落链：%s", cmd)
	}
}

// ---------- 接入点自动接管（2026-09-22：只给 IP + 类型 + 凭据，接入点平台全自动） ----------

// 反向隧道命令：必须只转发不执行（-N）、目标机侧端口固定 18080 → 平台 8080、
// 端口被占立刻退出（ExitOnForwardFailure，keeper 重试才能感知）
func TestReverseTunnelCmdShape(t *testing.T) {
	cmd := reverseTunnelCmd(&sshCred{Host: "10.1.207.156", Port: 22022, User: "ibomc", Pass: "p@ss w0rd"})
	if !strings.Contains(cmd.Path, "sshpass") {
		t.Errorf("必须经 sshpass 传密码，实际 %s", cmd.Path)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-N", "-R 18080:127.0.0.1:8080", "-p 22022",
		"ibomc@10.1.207.156", "ExitOnForwardFailure=yes", "StrictHostKeyChecking=no"} {
		if !strings.Contains(args, want) {
			t.Errorf("隧道命令缺 %s：%s", want, args)
		}
	}
}

// 隧道语义接入点识别：只有 127.0.0.1:18080 前缀才走隧道守护
func TestIsTunnelAccessPoint(t *testing.T) {
	yes := []string{"http://127.0.0.1:18080", "http://127.0.0.1:18080/"}
	no := []string{"", "http://l0-console:8080", "http://127.0.0.1:8080",
		"http://host.docker.internal:8080", "http://127.0.0.1:18080.evil.com"}
	for _, u := range yes {
		if !isTunnelAccessPoint(u) {
			t.Errorf("%s 应识别为隧道接入点", u)
		}
	}
	for _, u := range no {
		if isTunnelAccessPoint(u) {
			t.Errorf("%s 不应识别为隧道接入点", u)
		}
	}
}

// 候选采用顺序 = 声明顺序（第一个实测可达者优先）
func TestFirstReachableCandidate(t *testing.T) {
	cands := []accessPointCandidate{
		{URL: "http://host.docker.internal:8080"},
		{URL: "http://l0-console:8080"},
	}
	url, tool, ok := firstReachableCandidate(cands, map[string]string{"http://l0-console:8080": "wget"})
	if !ok || url != "http://l0-console:8080" || tool != "wget" {
		t.Errorf("应按声明顺序取第一个可达候选，实际 %q %q %v", url, tool, ok)
	}
	if _, _, ok := firstReachableCandidate(cands, map[string]string{}); ok {
		t.Errorf("无可达候选应返回 false")
	}
}

// target_kind 落库往返：写入后 GetResource/ListResources 都能读回；空值不覆盖已登记值
func TestResourceTargetKindRoundtrip(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.UpsertResource(&storepkg.Resource{ID: "r1", IP: "10.0.0.9", TargetKind: "host"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r, err := db.GetResource("r1")
	if err != nil || r == nil {
		t.Fatalf("get: %v %v", r, err)
	}
	if r.TargetKind != "host" {
		t.Errorf("target_kind 应为 host，实际 %q", r.TargetKind)
	}
	// 不带 target_kind 的部分更新不抹掉已登记值
	if err := db.UpsertResource(&storepkg.Resource{ID: "r1", AgentConsoleURL: "http://127.0.0.1:18080"}); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	r, _ = db.GetResource("r1")
	if r.TargetKind != "host" || r.AgentConsoleURL != "http://127.0.0.1:18080" {
		t.Errorf("部分更新不应抹掉已登记值：kind=%q url=%q", r.TargetKind, r.AgentConsoleURL)
	}
}

// when 跳过必须说人话：跳过原因要能看出「为什么跳过」而不是「不适用」一句话打发
func TestSkipReasonPlainLanguage(t *testing.T) {
	env := map[string]bool{"agent_exists": true}
	if got := skipReason("!agent_exists", env); got != "目标机已有 SAgent——本步跳过，复用现有安装" {
		t.Errorf("agent 已存在时的跳过原因应说清是复用，实际 %q", got)
	}
	if got := skipReason("abilities_requires_params", env); got == "" || got == "不适用当前模式与前置状态" {
		t.Errorf("无参数能力的跳过原因应是具体文案，实际 %q", got)
	}
	if got := skipReason("unknown_cond", env); !strings.Contains(got, "unknown_cond") || !strings.Contains(got, "不成立") {
		t.Errorf("未识别 when 应退回「条件不成立」并带出表达式，实际 %q", got)
	}
}

// ---------- 3. 推进器语义：平台侧自完成 / 外部停下等回报 / when 不满足即跳过 ----------

func TestAdvanceFlowRunsPlatformWaitsExternalSkipsWhen(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)

	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "conditional", Atom: "install_agent", Title: "装 Agent", Scope: "external", When: "agent_exists"},
		{ID: "preflight_host", Atom: "preflight_host", Title: "主机探路", Scope: "external",
			Requires: []string{"pick_object"}, Gate: "blocker"},
	}
	id := makeFlow(t, db, "ip-10-9-9-9", steps)

	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	st := latestStatuses(t, db, id)

	if st["pick_object"] != stOK {
		t.Errorf("platform 步骤应当场完成，实际 %q", st["pick_object"])
	}
	if st["conditional"] != stSkipped {
		t.Errorf("when=agent_exists 且无 Agent，应跳过，实际 %q", st["conditional"])
	}
	if st["preflight_host"] != stRunning {
		t.Errorf("external 步骤应下发后停在 running 等回报，实际 %q", st["preflight_host"])
	}

	flow, _ := db.GetFlow(id)
	if flow == nil || flow.Status != "running" {
		t.Errorf("流水线状态应为 running，实际 %+v", flow)
	}
}

// ---------- 4. 推进器语义：失败是终态，门禁阻断下游 ----------

func TestAdvanceFlowFailureIsTerminalAndBlocksDownstream(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)

	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "preflight_host", Atom: "preflight_host", Title: "主机探路", Scope: "external",
			Requires: []string{"pick_object"}, Gate: "blocker"},
		{ID: "sync_config", Atom: "pick_version", Title: "生成并下发配置", Scope: "platform",
			Requires: []string{"preflight_host"}},
	}
	id := makeFlow(t, db, "ip-10-9-9-8", steps)

	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow #1: %v", err)
	}
	if st := latestStatuses(t, db, id); st["preflight_host"] != stRunning {
		t.Fatalf("前置步骤应停在 running，实际 %q", st["preflight_host"])
	}

	// 模拟外部执行器回报失败（真实路径是探路包结果回填，这里直接落事件）
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "preflight_host", Title: "主机探路", Scope: "external",
		Status: stFail, Summary: "目标机 SSH 不可达", StartedAt: "2026-09-21 10:00:00",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow #2: %v", err)
	}

	// 失败是终态：再推进一次不能把它偷偷重放成 running
	for i := 0; i < 3; i++ {
		if err := advanceFlow(db, nil, id); err != nil {
			t.Fatalf("advanceFlow #%d: %v", 3+i, err)
		}
	}
	st := latestStatuses(t, db, id)
	if st["preflight_host"] != stFail {
		t.Errorf("失败步骤被静默重放成了 %q（重试语义被架空）", st["preflight_host"])
	}
	if st["sync_config"] != stBlocked {
		t.Errorf("前置失败时下游应 blocked（不生成配置、不下发），实际 %q", st["sync_config"])
	}
	// 状态优先级：存在硬失败时流程状态取根因 failed（blocked 是 fail 的派生结果）
	flow, _ := db.GetFlow(id)
	if flow == nil || flow.Status != "failed" {
		t.Errorf("流水线状态应体现根因 failed，实际 %+v", flow)
	}
	if flow != nil && flow.CurrentStep != "preflight_host" {
		t.Errorf("当前卡点应为失败的前置步骤，实际 %q（步骤状态=%v）", flow.CurrentStep, st)
	}

	// 显式重试：清掉终态 → 前置步骤重新下发 → 流水线回到 running
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "preflight_host", Title: "主机探路", Scope: "external",
		Status: stPending, Summary: "人工重试：重置该环节", StartedAt: "2026-09-21 10:05:00",
	}); err != nil {
		t.Fatalf("AppendEvent(retry): %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow(retry): %v", err)
	}
	if st := latestStatuses(t, db, id); st["preflight_host"] != stRunning {
		t.Errorf("重试后前置步骤应重新下发（running），实际 %q", st["preflight_host"])
	}
	if flow, _ := db.GetFlow(id); flow == nil || (flow.Status != "running" && flow.Status != "blocked") {
		t.Errorf("重试后流水线状态异常：%+v", flow)
	}
}

// ---------- 5. when 必须后置求值（依赖就绪后才判条件） ----------

func TestWhenEvaluatedOnlyAfterDepsReady(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)

	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "install_agent", Atom: "install_agent", Title: "装 Agent", Scope: "external",
			Requires: []string{"pick_object"}, When: "!agent_exists"},
		{ID: "register_device", Atom: "register_platform_device", Title: "登记平台设备", Scope: "platform",
			Requires: []string{"install_agent"}, When: "agent_exists"},
	}
	id := makeFlow(t, db, "ip-10-9-9-7", steps)
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	st := latestStatuses(t, db, id)
	if st["install_agent"] != stRunning {
		t.Errorf("install_agent 应下发等回报，实际 %q", st["install_agent"])
	}
	// 核心断言：依赖还没就绪时，when 条件步骤必须 pending。
	// 若在这里提前判成 skipped（skipped 是终态），Agent 装完后这一步就再也不会跑。
	if st["register_device"] != stPending {
		t.Errorf("依赖未就绪的 when 条件步骤应为 pending，实际 %q（提前落 skipped 即永不再跑）", st["register_device"])
	}

	// Agent 未注册：依赖就绪后条件为假 → 跳过（如实反映「复用分支不适用」）
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "install_agent", Title: "装 Agent", Scope: "external",
		Status: stOK, Summary: "安装完成", StartedAt: "2026-09-21 10:00:00",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	if st := latestStatuses(t, db, id); st["register_device"] != stSkipped {
		t.Errorf("Agent 不存在时条件步骤应跳过，实际 %q", st["register_device"])
	}
}

// 复用分支：Agent 已在台账 → 装 Agent 跳过、登记平台设备执行，且地基能力由配置声明推导（非引擎写死）
func TestReusePathRegistersPlatformDeviceFromConfig(t *testing.T) {
	cfg := testOnboardCfg(t)
	db := testOnboardDB(t)

	resID := "ip-10-9-9-6"
	id := makeFlow(t, db, resID, []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "install_agent", Atom: "install_agent", Title: "装 Agent", Scope: "external",
			Requires: []string{"pick_object"}, When: "!agent_exists"},
		{ID: "register_device", Atom: "register_platform_device", Title: "登记平台设备", Scope: "platform",
			Requires: []string{"install_agent"}, When: "agent_exists"},
	})

	// Agent 已在台账（安装那一步会跳过，走复用分支）
	if err := db.UpsertAgentRow(&storepkg.AgentRow{ID: resID, Name: resID, Type: "edge", Source: "agent"}); err != nil {
		t.Fatalf("UpsertAgentRow: %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	st := latestStatuses(t, db, id)
	if st["install_agent"] != stSkipped {
		t.Errorf("Agent 已存在时安装步骤应跳过，实际 %q", st["install_agent"])
	}
	if st["register_device"] != stOK {
		t.Errorf("复用分支应执行登记平台设备，实际 %q（详情：%v）", st["register_device"], st)
	}

	// 资源角色落库
	if res, _ := db.GetResource(resID); res == nil || res.Role != "platform_device" {
		t.Errorf("资源应登记为 platform_device，实际 %+v", res)
	}

	// 地基能力由 onboard_config.json 的 locked 声明推导，引擎不得写死能力名
	var lockedIDs []string
	for _, a := range cfg.AgentAbilities {
		if a.Locked {
			lockedIDs = append(lockedIDs, a.ID)
		}
	}
	if len(lockedIDs) == 0 {
		t.Fatal("配置里没有任何 locked 地基能力，本用例失去意义")
	}
	f, _ := db.GetFlow(id)
	if f == nil {
		t.Fatal("flow 读不回来")
	}
	for _, want := range lockedIDs {
		if !containsStr(f.Abilities, want) {
			t.Errorf("地基能力 %s 未强制补入流水线，实际 %v", want, f.Abilities)
		}
	}
}

// ---------- 6. 模板纪律：依赖 install_agent 的步骤不得挂 when ----------

// 依赖 install_agent 的步骤不得再挂 when 条件。
// 原因（实战踩坑）：install_agent 由外部执行器上报完成，而 Agent 注册是异步的；
// 上报早于注册落地时 agent_exists 仍为 false，挂 when: agent_exists 的步骤会被判
// skipped —— skipped 是终态，等 Agent 真注册上来也不会再跑，地基能力就此静默丢失。
func TestTemplatesNoWhenOnInstallAgentDependents(t *testing.T) {
	tpls, err := loadFlowTemplates(defaultFlowTemplateDir)
	if err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	for _, tp := range tpls {
		for _, s := range tp.Steps {
			if containsStr(s.Requires, "install_agent") && strings.TrimSpace(s.When) != "" {
				t.Errorf("[%s] 步骤 %s 依赖 install_agent 又挂了 when=%q：执行器上报可能早于 Agent 注册，"+
					"会把该步误判为 skipped 且不可重来（时序应由 requires 表达）", tp.ID, s.ID, s.When)
			}
		}
	}
}

// ---------- 7. 能力清单：参数声明来自插件包 params.yaml ----------

func TestLoadAbilitiesFromConfigAndParamsYaml(t *testing.T) {
	cfg := testOnboardCfg(t)
	abs := loadAbilities(cfg, integrationsDir)
	if len(abs) == 0 {
		t.Fatal("能力清单为空")
	}

	// 能力 id 不得重复（同名能力声明两处 = 参数声明会被前一处静默遮蔽）
	seen := map[string]int{}
	for _, a := range abs {
		seen[a.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("能力 id %q 声明了 %d 次（应在 onboard_plugins / agent_abilities 中二选一）", id, n)
		}
	}

	// host_metrics 是地基能力：必须 locked
	hm := abilityByID(abs, "host_metrics")
	if hm == nil || !hm.Locked {
		t.Errorf("host_metrics 必须存在且 locked=true，实际 %+v", hm)
	}

	// 声明了 params 路径的能力必须真的把字段读出来（路径写错不能静默变成「无参数」）
	for _, a := range abs {
		if a.ParamsPath != "" && len(a.Params) == 0 {
			t.Errorf("能力 %s 声明了 params_path=%s 但读到 0 个字段（路径写错？）", a.ID, a.ParamsPath)
		}
	}

	// MySQL 参数表单来自插件包 params.yaml（L3 层，随包走）
	mysql := abilityByID(abs, "mysql_probe")
	if mysql == nil {
		t.Fatal("mysql_probe 能力缺失")
	}
	if !mysql.HasParams || len(mysql.Params) == 0 {
		t.Fatalf("mysql_probe 参数未从 params.yaml 装载：%+v", mysql)
	}
	// 参数表单必须有必填项声明（接入向导靠它做校验）
	required := 0
	for _, f := range mysql.Params {
		if f.Required {
			required++
		}
	}
	if required == 0 {
		t.Error("mysql_probe 没有任何 required 字段，接入向导无法做必填校验")
	}
}

// ---------- 8. 渲染器：通用 exporter 自动走 prometheus_scrape ----------

func TestRenderAgentConfigSectionGenericExporter(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)

	// 一个从没见过的开源 exporter 名字（既不是 mysql 也不是已知特例）
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: "redis-insight-hz", Type: "redis_probe", Plugin: "redis_probe",
		Address: "10.20.30.40", AgentID: "agent-r1", ResourceID: "redis-01",
		ParamsJSON: `{"port":"9121"}`,
	}); err != nil {
		t.Fatalf("InsertTarget: %v", err)
	}

	out := renderAgentConfigSection(db, "agent-r1")
	plugins, ok := out["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺 plugins 段：%+v", out)
	}
	scrape, ok := plugins["prometheus_scrape"].(map[string]any)
	if !ok {
		t.Fatalf("通用 exporter 应走 prometheus_scrape，实际 plugins=%+v", plugins)
	}
	targets, _ := scrape["targets"].([]map[string]any)
	if len(targets) != 1 {
		t.Fatalf("应渲染 1 个抓取目标，实际 %d", len(targets))
	}
	if url, _ := targets[0]["url"].(string); !strings.Contains(url, "9121") {
		t.Errorf("抓取 URL 未使用声明的端口：%q", url)
	}

	// 别的 Agent 名下不应被渲染进来
	if other := renderAgentConfigSection(db, "agent-other"); len(other["plugins"].(map[string]any)) != 0 {
		t.Errorf("配置串台：agent-other 拿到了 %+v", other)
	}
}

// ===================================================================
//  异常暴露回归（2026-09-21）：探测/安装过程中的异常必须能被平台自己发现并回到界面
//  真实缺陷背景：资源 ip-10-1-207-156 实际 sshd 在 22022，平台按默认 22 连上后
//  ansible ping rc=0「假成功」，同时在监听端口列表里读不到 22 —— 端口登记错了却没人知道
// ===================================================================

func TestSSHPortSourceIsTraceable(t *testing.T) {
	// 台账登记了端口 → 来源 ledger，无告警
	ledger := sshCredOf(&storepkg.Resource{IP: "10.1.207.156", SSHPort: 22022, SSHUser: "ibomc", SSHPassword: "x"})
	if ledger.PortSource != portSourceLedger {
		t.Fatalf("台账登记端口的来源应为 %q，实际 %q", portSourceLedger, ledger.PortSource)
	}
	if got := ledger.target(); got != "ibomc@10.1.207.156:22022" {
		t.Errorf("连接目标应含台账端口，实际 %q", got)
	}
	if n := countWarn(withPortSource(ledger, nil)); n != 0 {
		t.Errorf("台账登记端口不该有端口来源告警，实际 %d 条", n)
	}

	// 台账没登记端口 → 回落 22 必须留痕（不再是静默行为）
	fallback := sshCredOf(&storepkg.Resource{IP: "10.0.0.9", SSHPort: 0, SSHUser: "root", SSHPassword: "x"})
	if fallback.Port != 22 || fallback.PortSource != portSourceDefault {
		t.Fatalf("未登记端口应回落 22 且来源 default_22，实际 port=%d src=%q", fallback.Port, fallback.PortSource)
	}
	ds := withPortSource(fallback, nil)
	if len(ds) != 1 || ds[0].Code != "port_source_default" || ds[0].Level != "warn" {
		t.Fatalf("未登记端口必须产出 warn 级 port_source_default 诊断，实际 %+v", ds)
	}

	// 凭据缺项要能说清缺哪个（界面直显凭据状态）
	st := credState(sshCredOf(&storepkg.Resource{IP: "10.0.0.9", SSHPort: 22}))
	if ok, _ := st["valid"].(bool); ok {
		t.Fatalf("缺 user/pass 不该判定为有效凭据：%+v", st)
	}
	if why := ansibleFallbackReason(&storepkg.Resource{IP: "10.0.0.9", SSHPort: 22}); !strings.Contains(why, "ssh_user") {
		t.Errorf("回落原因应点名缺失字段，实际 %q", why)
	}
}

func TestParseSSHDPortEvidence(t *testing.T) {
	// sshd_config：注释行必须忽略（CentOS 默认带 "#Port 22"），噪声行不认
	cfg := "target | CHANGED | rc=0 >>\n#Port 22\n#AddressFamily any\nPort 22022\nPort 2222\n"
	got := parseSSHDConfigPorts(cfg)
	if len(got) != 2 || got[0] != 2222 || got[1] != 22022 {
		t.Fatalf("sshd_config Port 声明解析错误：%v（#Port 22 是注释不该算）", got)
	}

	// ss -ltn：第 4 列是 Local Address，IPv6 形式要能取到端口
	ss := "target | CHANGED | rc=0 >>\nState Recv-Q Send-Q Local Address:Port Peer Address:Port\n" +
		"LISTEN 0 128 0.0.0.0:22022 0.0.0.0:*\nLISTEN 0 128 [::]:19090 [::]:*\n"
	lp := parseListenPorts(ss)
	if len(lp) != 2 || lp[0] != 19090 || lp[1] != 22022 {
		t.Fatalf("ss -ltn 监听端口解析错误：%v", lp)
	}

	// netstat -ltn：同样第 4 列
	ns := "Proto Recv-Q Send-Q Local Address Foreign Address State\ntcp 0 0 0.0.0.0:22 0.0.0.0:* LISTEN\n"
	if got := parseListenPorts(ns); len(got) != 1 || got[0] != 22 {
		t.Fatalf("netstat -ltn 监听端口解析错误：%v", got)
	}
}

func TestPortCheckExposesWrongPort(t *testing.T) {
	cred := &sshCred{Host: "10.1.207.156", Port: 22, User: "ibomc", PortSource: portSourceDefault}

	// 现场场景：连的 22，目标机本机只监听 22022 → 必须报出来，不能静默通过
	ds := portCheckDiags(cred, []int{22022}, []int{22022}, 0, 0)
	if len(ds) == 0 || ds[0].Code != "port_not_listening_on_target" {
		t.Fatalf("连错端口必须产出 port_not_listening_on_target，实际 %+v", ds)
	}
	if !strings.Contains(ds[0].Message, "22022") {
		t.Errorf("诊断消息应带上目标机真实监听端口，实际 %q", ds[0].Message)
	}
	// sshd_config 声明 22022 而本次连 22 → 追加一条口径不符的告警
	found := false
	for _, d := range ds {
		if d.Code == "port_not_in_sshd_config" {
			found = true
		}
	}
	if !found {
		t.Errorf("sshd_config 声明端口与连接端口不符时应告警，实际 %+v", ds)
	}

	// 端口对得上 → 只留 info，不制造噪声
	ok := portCheckDiags(&sshCred{Port: 22022}, []int{22022}, []int{22022, 19090}, 0, 0)
	if countWarn(ok) != 0 {
		t.Errorf("端口一致时不该有 warn，实际 %+v", ok)
	}

	// 读不到监听端口 → 也要说清"读不到"，而不是当作通过
	blind := portCheckDiags(cred, nil, nil, 1, 1)
	if len(blind) == 0 || blind[0].Code != "sshd_listen_unreadable" {
		t.Fatalf("读不到监听端口必须显式告警，实际 %+v", blind)
	}
	if countWarn(blind) != 1 {
		t.Errorf("读不到监听端口时只应有 1 条 warn（伴随 1 条 info 说明核对不完整），实际 %+v", blind)
	}

	// sshd_config 读到但声明端口对不上 → warn；读不到 → 只给 info 说明核对口径不完整
	noDecl := portCheckDiags(&sshCred{Port: 22}, nil, []int{22}, 2, 0)
	if countWarn(noDecl) != 0 || len(noDecl) != 2 || noDecl[1].Code != "sshd_config_unreadable" {
		t.Errorf("sshd_config 不可读时应给 info 说明（不误报），实际 %+v", noDecl)
	}
}

func TestClassifyAnsibleFailures(t *testing.T) {
	cases := []struct {
		name string
		out  string
		rc   int
		code string
	}{
		{"认证失败", "target | UNREACHABLE! => {\"msg\": \"Failed to connect: Permission denied (publickey,password).\"}", 4, "ssh_auth_failed"},
		{"端口拒绝", "target | UNREACHABLE! => {\"msg\": \"Failed to connect to the host via ssh: ssh: connect to host x port 22099: Connection refused\"}", 4, "ssh_conn_refused"},
		{"超时", "target | UNREACHABLE! => {\"msg\": \"Failed to connect: ssh: connect to host x port 22: Connection timed out\"}", 4, "ssh_timeout"},
		{"兜底", "ERROR! Unexpected error", 2, "ansible_rc"},
	}
	for _, c := range cases {
		if d := classifySSHFailure(c.out, c.rc); d.Code != c.code || d.Level != "fatal" {
			t.Errorf("[%s] 归因错误：got=%s level=%s want=%s", c.name, d.Code, d.Level, c.code)
		}
	}

	// playbook 失败要能指到具体任务，而不是只给一个 rc
	pb := []struct {
		out  string
		code string
	}{
		{"TASK [assert binary sha256 matches catalog] *****\nfatal: [target]: FAILED! => {\"msg\": \"non-zero return code\"}", "binary_sha256_mismatch"},
		{"TASK [start agent] *****\nfatal: [target]: FAILED! => {\"msg\": \"non-zero return code\"}", "agent_start_failed"},
		{"target | UNREACHABLE! => {\"msg\": \"Failed to connect: Connection refused\"}", "ssh_conn_refused"},
		{"fatal: [target]: FAILED! => {\"msg\": \"whatever\"}", "playbook_rc"},
	}
	for _, c := range pb {
		if d := classifyPlaybookFailure(c.out, 2); d.Code != c.code {
			t.Errorf("安装失败归因错误：got=%s want=%s（原文=%q）", d.Code, c.code, c.out)
		}
	}
}

func TestClipKeepsHeadAndTail(t *testing.T) {
	long := strings.Repeat("A", 500) + "失败原因在中间" + strings.Repeat("B", 500)
	got := clip(long, 300)
	if len(got) > 300+80 {
		t.Errorf("截断后长度失控：%d", len(got))
	}
	if !strings.HasPrefix(got, "AAA") {
		t.Error("截断丢了头部（失败首因常在开头）")
	}
	if !strings.HasSuffix(got, "BBB") {
		t.Error("截断丢了尾部（最终结论常在结尾）")
	}
	if !strings.Contains(got, "已截断") {
		t.Error("截断必须显式标注，不能让人以为这就是全文")
	}
	// 短文本原样返回
	if s := clip("short", 300); s != "short" {
		t.Errorf("短文本不应被改动，实际 %q", s)
	}
	// 空诊断数组序列化后是 []，前端判空不为 null
	if s, _ := json.Marshal(diagJSON(nil)); string(s) != "[]" {
		t.Errorf("空诊断应为 []，实际 %s", s)
	}
}

func TestWaitingSemanticsAreHumanReadable(t *testing.T) {
	if got := waitingLabel("human_pick"); got != "待人工选定 SAgent 版本" {
		t.Errorf("停等语义未人话化：%q", got)
	}
	if got := waitingLabel("probe"); got != "待主机探路实测结果" {
		t.Errorf("停等语义未人话化：%q", got)
	}
	if got := waitingForKey(`{"waiting_for":"human_pick"}`); got != "human_pick" {
		t.Errorf("waitingForKey 解析失败：%q", got)
	}
	if got := waitingForKey(`not-json`); got != "" {
		t.Errorf("非 JSON detail 应返回空串，实际 %q", got)
	}
	// 渲染执行包必须带端口：人工照抄 ssh user@ip 会默认走 22，正是连错端口的根源
	ins := renderInstruction("ssh -p {{.port}} <user>@{{.ip}} true", "10.1.207.156", 22022)
	if !strings.Contains(ins, "-p 22022") || !strings.Contains(ins, "10.1.207.156") {
		t.Errorf("执行包未渲染端口：%q", ins)
	}
}

func TestDiagWarnCountOnlyCountsRealAnomalies(t *testing.T) {
	detail := `{"diagnosis":[{"level":"info","code":"port_verified"},{"level":"warn","code":"port_source_default"},{"level":"fatal","code":"x"}]}`
	if n := diagWarnCount(detail); n != 2 {
		t.Errorf("warn_count 应只算 warn/fatal（info 不算异常），实际 %d", n)
	}
	if n := diagWarnCount(`{"a":1}`); n != 0 {
		t.Errorf("无诊断时应为 0，实际 %d", n)
	}
	if n := diagWarnCount(""); n != 0 {
		t.Errorf("空 detail 应为 0，实际 %d", n)
	}
}

// 步骤级 warn 必须冒泡到流程视图的 warn_count —— 列表页要让运维不点进去就看见异常
func TestFlowViewSurfacesWarnCount(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
	}
	id := makeFlow(t, db, "ip-10-1-207-156", steps)
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "pick_object", Title: "选对象", Scope: "platform", Status: stOK,
		Summary: "资源对象已就绪",
		Detail:  `{"diagnosis":[{"level":"warn","code":"port_source_default","message":"端口来自默认值"}]}`,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	f, _ := db.GetFlow(id)
	v := flowView(db, nil, f)
	if n, _ := v["warn_count"].(int); n != 1 {
		t.Fatalf("流程视图应透出 warn_count=1，实际 %v", v["warn_count"])
	}
}

// 同一轮推进内被"重跑"的步骤，其 waiting_for 必须同步给下游判定。
// 探路成功后 pick_version 必须在同一次推进内自动定版（2026-09-22 起：不再停等人工）：
// 有兼容候选即落 ok 并带 detail.tag（安装作业据此取版本），下游不再显示成门禁拦截
func TestPickVersionAutoSelectsWithinSameAdvance(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	loadVersionCatalog("data/versions.yaml", db)

	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "preflight_host", Atom: "preflight_host", Title: "主机探路", Scope: "external",
			Requires: []string{"pick_object"}},
		{ID: "pick_version", Atom: "pick_version", Title: "选版本", Scope: "platform",
			Requires: []string{"preflight_host"}},
		{ID: "install_agent", Atom: "install_agent", Title: "装 Agent", Scope: "external",
			Requires: []string{"pick_version"}},
	}
	id := makeFlow(t, db, "ip-10-1-207-156", steps)
	// 探路已回填实测 OS/架构（真实路径由 ansible 作业写台账）
	if err := db.UpdateResourceProbe("ip-10-1-207-156", "{}", "linux", "amd64", "5.4.237-1.el7.elrepo.x86_64"); err != nil {
		t.Fatalf("UpdateResourceProbe: %v", err)
	}

	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow #1: %v", err)
	}
	// 执行器回报探路成功 → 同一次推进里 pick_version 自动定版
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "preflight_host", Title: "主机探路", Scope: "external",
		Status: stOK, Summary: "自动回报：ansible 实测 linux/amd64",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow #2: %v", err)
	}

	latest := map[string]*storepkg.FlowEvent{}
	events, _ := db.ListEvents(id)
	for _, e := range events {
		latest[e.Step] = e
	}
	if latest["pick_version"] == nil || latest["pick_version"].Status != stOK {
		t.Fatalf("pick_version 应在同一次推进内自动选定并落 ok，实际 %+v", latest["pick_version"])
	}
	if tag := pickedVersionTag(db, id); tag == "" {
		t.Fatalf("自动选定必须产出 detail.tag（安装作业据此取版本）：%s", latest["pick_version"].Detail)
	}
	if !strings.Contains(latest["pick_version"].Summary, "自动选定") {
		t.Errorf("摘要要说清是自动选定：%q", latest["pick_version"].Summary)
	}
	if !hasDiagCode(latest["pick_version"].Detail, "version_auto_selected") {
		t.Errorf("必须带 version_auto_selected 诊断：%s", latest["pick_version"].Detail)
	}
	down := latest["install_agent"]
	if down == nil {
		t.Fatal("install_agent 无事件")
	}
	if strings.Contains(down.Summary, "前置门禁未通过") {
		t.Errorf("自动定版后下游不该显示成门禁拦截，实际 %q", down.Summary)
	}
}

// 无兼容候选时必须 fail 并聚合"差哪一项"，绝不硬选最新
func TestPickVersionFailsWhenNoCompatible(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	saved := versionCatalog
	defer func() { versionCatalog = saved }()
	versionCatalog = []SAVersion{{Tag: "v9", Version: "9.0", OS: "linux", Arch: "amd64",
		Status: "stable", MinKernel: "99.0"}}
	id := makeFlow(t, db, "res-noc", []storepkg.FlowStepSnapshot{
		{ID: "pick_version", Atom: "pick_version", Title: "选版本", Scope: "platform"},
	})
	if err := db.UpdateResourceProbe("res-noc", `{}`, "linux", "amd64", "3.10.0"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	ev, _ := db.LatestEvent(id, "pick_version")
	if ev == nil || ev.Status != stFail {
		t.Fatalf("应判 fail，实际 %+v", ev)
	}
	if !hasDiagCode(ev.Detail, "version_no_compatible") {
		t.Fatalf("必须带「差哪一项」诊断：%s", ev.Detail)
	}
}

// 现场回归（2026-09-21 19:42，flow 7 / ip-10-1-207-156）：
// ibomc 在目标机建 /home/deploy 被拒（Errno 13），平台却报「SSH 认证失败」，
// 把人引去核查台账密码——方向完全错。凭据是对的，playbook 已经跑出过 TASK 记录，
// 说明 SSH 认证必然通过；裸 "Permission denied" 绝不能当认证失败。
func TestFileSystemPermissionDeniedIsNotSSHAuthFailure(t *testing.T) {
	// 与库里 onboard_event#234 的 evidence.output 同构
	out := "\nPLAY [target] ******************************************************************\n\n" +
		"TASK [stop old agent if any] ***************************************************\nchanged: [l0node]\n\n" +
		"TASK [make dirs] ***************************************************************\n" +
		"failed: [l0node] (item=/home/deploy/SAgent/bin) => {\"ansible_loop_var\": \"item\", \"changed\": false, " +
		"\"item\": \"/home/deploy/SAgent/bin\", \"msg\": \"There was an issue creating /home/deploy as requested: " +
		"[Errno 13] Permission denied: '/home/deploy'\", \"path\": \"/home/deploy/SAgent/bin\"}\n\n" +
		"PLAY RECAP *********************************************************************\n" +
		"l0node                     : ok=1    changed=1    unreachable=0    failed=1    skipped=0    rescued=0    ignored=0   \n"

	d := classifyPlaybookFailure(out, 2)
	if d.Code == "ssh_auth_failed" {
		t.Fatal("目标机目录权限不足被误判成 SSH 认证失败——会把人引向核查台账密码")
	}
	if d.Code != "target_permission_denied" {
		t.Errorf("应归因为目标机权限不足，实际 code=%s", d.Code)
	}
	if d.Level != "fatal" {
		t.Errorf("安装失败必须是 fatal 级，实际 %s", d.Level)
	}
	if !strings.Contains(d.Message, "/home/deploy") {
		t.Errorf("诊断要带上目标机真实路径，实际 %q", d.Message)
	}
	if got := targetPathFromErr(out); got != "/home/deploy" {
		t.Errorf("路径提取应取「创建目标」而非 item 里的子路径，实际 %q", got)
	}

	// 抽不到路径时不能编造，只能给通用措辞
	if got := targetPathFromErr("fatal: [x]: FAILED! => {\"msg\": \"Permission denied\"}"); got != "" {
		t.Errorf("无路径可提取时应返回空串，实际 %q", got)
	}
	if d := targetPermissionDiag("Permission denied"); strings.Contains(d.Message, "  ") {
		t.Errorf("无路径时文案不该出现空占位：%q", d.Message)
	}
}

// SSH 认证失败仍然要认得出来——收窄判据不能把真认证问题一起吞掉
func TestRealSSHAuthFailureStillClassified(t *testing.T) {
	// CentOS ssh 的典型措辞（探路 ping 阶段）
	ping := "l0node | UNREACHABLE! => {\"msg\": \"Failed to connect to the host via ssh: " +
		"ibomc@10.1.207.156: Permission denied (publickey,gssapi-keyex,gssapi-with-mic,password).\"}"
	if d := classifySSHFailure(ping, 4); d.Code != "ssh_auth_failed" {
		t.Errorf("ssh 自身的 Permission denied (publickey...) 仍应判定认证失败，实际 %s", d.Code)
	}
	if d := classifyPlaybookFailure(ping, 4); d.Code != "ssh_auth_failed" {
		t.Errorf("playbook 连不上目标机时应走 SSH 层归因，实际 %s", d.Code)
	}

	// playbook 已在目标机执行过 → 同一条输出里的 UNREACHABLE 才说明传输层断了
	midway := "TASK [Gathering Facts] ***\nfatal: [l0node]: UNREACHABLE! => {\"msg\": \"Failed to connect: ssh: connect to host 10.1.207.156 port 22022: Connection refused\"}"
	if d := classifyPlaybookFailure(midway, 2); d.Code != "ssh_conn_refused" {
		t.Errorf("执行中断连应归因端口拒绝，实际 %s", d.Code)
	}
}

// ===================================================================
//  安装过程信息回传 + 步骤超时/自动重试（2026-09-21 用户要求）
//  ① 安装过程中所有信息都要回到界面：逐任务、状态、变更、失败原因、原文
//  ② 每个步骤都要有超时，不能无限等待；但给一点点重试机会
// ===================================================================

// 现场输出照抄（flow 7 / ip-10-1-207-156 安装失败那次）：解析器必须逐任务还原，
// 而不是把它当成一坨文本
func TestAnsibleStreamParserExtractsEveryTask(t *testing.T) {
	lines := []string{
		"",
		"PLAY [target] ******************************************************************",
		"",
		"TASK [Gathering Facts] *********************************************************",
		"ok: [l0node]",
		"",
		"TASK [stop old agent if any] ***************************************************",
		"changed: [l0node]",
		"",
		"TASK [make dirs] ***************************************************************",
		`failed: [l0node] (item=/home/deploy/SAgent/bin) => {"ansible_loop_var": "item", "changed": false, "item": "/home/deploy/SAgent/bin", "msg": "There was an issue creating /home/deploy as requested: [Errno 13] Permission denied: '/home/deploy'", "path": "/home/deploy/SAgent/bin"}`,
		`failed: [l0node] (item=/home/deploy/SAgent/run) => {"item": "/home/deploy/SAgent/run", "msg": "There was an issue creating /home/deploy as requested: [Errno 13] Permission denied: '/home/deploy'"}`,
		"",
		"TASK [start agent] *************************************************************",
		"skipping: [l0node]",
		"",
		"PLAY RECAP *********************************************************************",
		"l0node                     : ok=1    changed=1    unreachable=0    failed=1    skipped=1    rescued=0    ignored=0   ",
	}
	p := newAnsibleParser(nil)
	for _, l := range lines {
		p.Feed(l)
	}
	tasks := p.finish()

	if len(tasks) != 4 {
		t.Fatalf("应解析出 4 个任务，实际 %d：%+v", len(tasks), tasks)
	}
	if tasks[0].Name != "Gathering Facts" || tasks[0].Status != "ok" || tasks[0].Play != "target" {
		t.Errorf("任务 1 解析错误：%+v", tasks[0])
	}
	if tasks[1].Status != "changed" || !tasks[1].Changed {
		t.Errorf("changed 状态未识别：%+v", tasks[1])
	}
	if tasks[2].Status != "failed" {
		t.Errorf("failed 状态未识别：%+v", tasks[2])
	}
	if len(tasks[2].Items) != 2 {
		t.Errorf("loop 的两个失败 item 都要留住，实际 %v", tasks[2].Items)
	}
	if !strings.Contains(tasks[2].Msg, "Errno 13") {
		t.Errorf("失败原因（msg）必须带回来，实际 %q", tasks[2].Msg)
	}
	if tasks[3].Status != "skipping" {
		t.Errorf("skipping 状态未识别：%+v", tasks[3])
	}
	if len(tasks[2].Lines) < 3 {
		t.Errorf("逐任务原文行必须留住（总输出被截时它是最后证据），实际 %d 行", len(tasks[2].Lines))
	}
	if p.recap["failed"] != 1 || p.recap["ok"] != 1 || p.recap["changed"] != 1 || p.recap["skipped"] != 1 {
		t.Errorf("PLAY RECAP 解析错误：%v", p.recap)
	}
	if got := recapLine(p.recap, p.recapSeq); !strings.Contains(got, "failed=1") {
		t.Errorf("recap 单行渲染错误：%q", got)
	}
}

// loop 项落在 `=>` 之后的形态（2.8+ 常见）：`ok: [h] => (item=/path)`。
// 漏掉这一种，界面「受影响子项（哪个子路径失败了）」永远为空——运维只能自己数行。
func TestAnsibleParserLoopItemAfterArrow(t *testing.T) {
	// 夹具取自 2026-09-21 真实安装输出（ip-10-1-207-156 装成功那次）
	p := newAnsibleParser(nil)
	for _, l := range []string{
		"PLAY [target] ******************************************************************",
		"TASK [make dirs] ***************************************************************",
		"ok: [l0node] => (item=/home/ibomc/SAgent/bin)",
		"ok: [l0node] => (item=/home/ibomc/SAgent/conf)",
		"ok: [l0node] => (item=/home/ibomc/SAgent/run)",
		"TASK [deploy binary] ***********************************************************",
		"ok: [l0node]",
		"PLAY RECAP *********************************************************************",
		"l0node                     : ok=7    changed=3    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0   ",
	} {
		p.Feed(l)
	}
	tasks := p.finish()
	if len(tasks) != 2 {
		t.Fatalf("应解析出 2 个任务，实际 %d：%+v", len(tasks), tasks)
	}
	if len(tasks[0].Items) != 3 {
		t.Fatalf("=> 之后的 loop 子项必须全部留住，实际 %v", tasks[0].Items)
	}
	if tasks[0].Items[0] != "/home/ibomc/SAgent/bin" || tasks[0].Items[2] != "/home/ibomc/SAgent/run" {
		t.Errorf("loop 子项内容解析错误：%v", tasks[0].Items)
	}
	// 非 loop 任务不得凭空多出子项
	if len(tasks[1].Items) != 0 {
		t.Errorf("无 loop 的任务不该有子项，实际 %v", tasks[1].Items)
	}
	if tasks[1].Status != "ok" {
		t.Errorf("=> 之后无 item 的行必须仍被识别为结果行，实际 %+v", tasks[1])
	}
}

// 长 msg 时 ansible 会换行缩进输出 JSON——多行必须能拼回一个完整对象
func TestAnsibleParserMultiLineJSON(t *testing.T) {
	p := newAnsibleParser(nil)
	for _, l := range []string{
		"TASK [copy binary] ************************************************************",
		"fatal: [l0node]: FAILED! => {",
		`    "changed": false,`,
		`    "msg": "Destination /home/ibomc/SAgent not writable"`,
		"}",
	} {
		p.Feed(l)
	}
	tasks := p.finish()
	if len(tasks) != 1 {
		t.Fatalf("应解析出 1 个任务，实际 %d", len(tasks))
	}
	if tasks[0].Status != "failed" {
		t.Errorf("多行块的任务状态应判 failed，实际 %s", tasks[0].Status)
	}
	if tasks[0].Msg != "Destination /home/ibomc/SAgent not writable" {
		t.Errorf("多行 msg 未提取：%q", tasks[0].Msg)
	}
}

// 流式：任务边界即回调（界面要能看到"跑到第几个任务了"，不是跑完才有）
func TestParserStreamsProgressAtTaskBoundary(t *testing.T) {
	var seen []string
	p := newAnsibleParser(func(tk ansibleTask, done int) {
		seen = append(seen, itoa(done)+":"+tk.Name+":"+tk.Status)
	})
	p.Feed("TASK [Gathering Facts] ********")
	p.Feed("ok: [l0node]")
	if len(seen) != 0 {
		t.Fatalf("任务还没结束时不该回调：%v", seen)
	}
	p.Feed("TASK [make dirs] ********") // 出现下一个任务头 → 上一个任务收尾并回调
	if len(seen) != 1 || seen[0] != "1:Gathering Facts:ok" {
		t.Fatalf("任务边界应回调一次，实际 %v", seen)
	}
	p.finish()
	if len(seen) != 2 {
		t.Fatalf("finish 应补一次回调（最后一个任务），实际 %v", seen)
	}
}

func TestTimeoutSpecFromConfig(t *testing.T) {
	testOnboardCfg(t)
	spec := timeoutSpecFor(onboardCfg, "install_agent")
	if spec.RunSec != 900 || spec.IdleSec != 240 || spec.Retry != 1 {
		t.Fatalf("install_agent 规格应为 900/240/1，实际 %+v", spec)
	}
	// 未登记的 atom → default（人工慢操作给宽松上限，但同样必须有界）
	d := timeoutSpecFor(onboardCfg, "collect_params")
	if d.RunSec <= 0 || d.IdleSec <= 0 {
		t.Fatalf("未登记 atom 必须回落到有界的 default，实际 %+v", d)
	}
	if d.RunSec < spec.RunSec {
		t.Errorf("人工路径上限不该比自动执行还紧：default=%+v，install=%+v", d, spec)
	}
}

// 执行层预算必须从配置推导。曾经这里是写死的常量（安装 300s、ping 60s），
// 而配置写的是 900s/600s——界面倒计时按配置走、实际却提前判死，等于向运维谎报。
func TestExecBudgetDerivesFromConfig(t *testing.T) {
	testOnboardCfg(t)

	// ① 步骤级兜底不得早于清扫器上限：否则重试额度永远用不上
	spec := timeoutSpecFor(onboardCfg, "install_agent")
	pb := execBudget(spec, budgetPlaybook)
	if pb <= time.Duration(spec.RunSec)*time.Second {
		t.Fatalf("安装兜底 ctx=%s 必须晚于 run_sec=%ds（清扫器要能先到并触发重试）", pb, spec.RunSec)
	}

	// ② 探路各阶段预算合计不得超过 run_sec：配置标注的上限必须仍是真实上界
	ps := timeoutSpecFor(onboardCfg, "preflight_host")
	sum := phaseBudget(ps, budgetPing) + 3*phaseBudget(ps, budgetPortCmd) +
		phaseBudget(ps, budgetSetup) + phaseBudget(ps, budgetDisk)
	if sum > time.Duration(ps.RunSec)*time.Second {
		t.Fatalf("探路阶段预算合计 %s 超过 run_sec=%ds，配置不再是真实上界", sum, ps.RunSec)
	}

	// ③ 单一真源：改 run_sec 必须同步改预算，否则仍是两套数字
	tight := spec
	tight.RunSec = 120
	if phaseBudget(tight, budgetPing) >= phaseBudget(spec, budgetPing) {
		t.Fatalf("预算未随 run_sec 变化，配置不是唯一真源")
	}

	// ④ 零/负配置回落下限，不允许"0s 秒退"或"负数预算"
	if got := phaseBudget(StepTimeout{RunSec: 0}, budgetPing); got < 5*time.Second {
		t.Fatalf("零 run_sec 应回落下限 5s，实际 %s", got)
	}
}

// 终态事件必须带尝试记录。成功路径曾误用 historySnapshot()（只读不追加），
// 于是成功卡片上「第 N 次尝试 / 跑了多久」永远为空，与失败卡片不对称——
// 运维看到的是"装成功了但查不到这次是怎么试出来的"。
func TestEndAttemptRecordsTerminalAttempt(t *testing.T) {
	st := &stepRunState{attempt: 2, spec: timeoutSpecFor(nil, "install_agent")}
	if got := st.historySnapshot(); len(got) != 0 {
		t.Fatalf("未结束前 historySnapshot 应为空，实际 %+v", got)
	}
	h := st.endAttempt("ok", "安装完成")
	if len(h) != 1 || h[0].Result != "ok" || h[0].No != 2 {
		t.Fatalf("endAttempt 必须追加本次终态记录，实际 %+v", h)
	}
	if got := st.historySnapshot(); len(got) != 1 {
		t.Fatalf("endAttempt 之后快照必须含该记录（成功路径就靠它回传），实际 %+v", got)
	}
	if h[0].StartedAt == "" || h[0].EndedAt == "" {
		t.Fatalf("尝试记录必须带起止时间，实际 %+v", h[0])
	}
}

// 超时策略是数据驱动的：文件与内置默认必须一致，否则"配置文件丢了"会静默改变行为
func TestTimeoutConfigFileAlignedWithDefault(t *testing.T) {
	fileCfg := loadOnboardConfig("data/onboard_config.json")
	defCfg := defaultOnboardConfig()
	for k, v := range defCfg.StepTimeouts {
		fv, ok := fileCfg.StepTimeouts[k]
		if !ok || fv != v {
			t.Errorf("step_timeouts[%s] 文件=%+v 内置默认=%+v，两者必须一致", k, fv, v)
		}
	}
}

func TestTimeoutVerdicts(t *testing.T) {
	testOnboardCfg(t)
	step := storepkg.FlowStepSnapshot{ID: "install_agent", Atom: "install_agent", Scope: "external"}
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02 15:04:05") }

	managed := func(startedAgo, idleAgo time.Duration) *storepkg.FlowEvent {
		return &storepkg.FlowEvent{Status: stRunning, Detail: detailJSON(map[string]any{
			"executor": "ansible", "runner": "platform",
			"progress": map[string]any{
				"attempt": 1, "max_attempts": 2,
				"attempt_started_at": ts(startedAgo), "last_activity_at": ts(idleAgo),
			},
		})}
	}

	if v := timeoutVerdictFor(step, managed(60*time.Second, 5*time.Second), now); v != nil {
		t.Errorf("还在正常执行不该判超时：%+v", v)
	}
	// 静默上限比总时长更早发现问题（连接挂着但目标机毫无反应）
	if v := timeoutVerdictFor(step, managed(100*time.Second, 300*time.Second), now); v == nil || v.Kind != "idle" {
		t.Errorf("静默超限应判 idle，实际 %+v", v)
	}
	// 有心跳但总时长超限
	if v := timeoutVerdictFor(step, managed(1000*time.Second, 5*time.Second), now); v == nil || v.Kind != "run" {
		t.Errorf("总时长超限应判 run，实际 %+v", v)
	}
	// 停等人工（blocked）不判超时：选版本停三天也不算故障
	blk := managed(9999*time.Second, 9999*time.Second)
	blk.Status = stBlocked
	if v := timeoutVerdictFor(step, blk, now); v != nil {
		t.Errorf("blocked 不该判超时：%+v", v)
	}
	// 回落人工执行：按 default 规格，不因安装步的紧阈值被误杀
	human := &storepkg.FlowEvent{Status: stRunning, CreatedAt: ts(500 * time.Second),
		Detail: detailJSON(map[string]any{"scope": "external", "instruction": "ssh ..."})}
	if v := timeoutVerdictFor(step, human, now); v != nil {
		t.Errorf("人工路径不该用 install_agent 的 900/240 判超时：%+v", v)
	}
}

// 超时 → 自动重试（一次）→ 再超时 → 判死；每步都要留痕、给出可执行建议
func TestSweepRetriesThenFails(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	steps := []storepkg.FlowStepSnapshot{
		{ID: "install_agent", Atom: "install_agent", Title: "安装 SAgent", Scope: "external"},
		{ID: "verify_probe", Atom: "verify_probe", Title: "连通验证", Scope: "agent", Requires: []string{"install_agent"}},
	}
	id := makeFlow(t, db, "ip-10-1-207-156", steps)
	spec := timeoutSpecFor(onboardCfg, "install_agent")
	past := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")

	// 第 1 次尝试：静默超限（模拟作业卡死/进程重启后没人回报）
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "install_agent", Title: "安装 SAgent", Scope: "external", Status: stRunning,
		Summary: "平台 ansible 执行中", StartedAt: past, FinishedAt: past,
		Detail: detailJSON(map[string]any{
			"executor": "ansible", "runner": "platform",
			"progress": map[string]any{
				"attempt": 1, "max_attempts": spec.Retry + 1,
				"attempt_started_at": past, "last_activity_at": past, "timeout": spec,
			},
		}),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	sweepOnboardTimeouts(db, nil)

	ev, _ := db.LatestEvent(id, "install_agent")
	if ev.Status != stRunning {
		t.Fatalf("第一次超时应自动重试而不是判死，实际状态 %s", ev.Status)
	}
	pr := progressOf(ev.Detail)
	if pr == nil || pr.Attempt != 2 {
		t.Fatalf("重试后 attempt 应推进到 2，实际 %+v", pr)
	}
	if !hasDiagCode(ev.Detail, "step_timeout_retry") {
		t.Errorf("自动重试必须带 warn 诊断（运维要知道平台自己救过一次）：%s", ev.Detail)
	}
	if n := len(attemptsOf(ev.Detail)); n != 1 {
		t.Errorf("应留下 1 笔尝试记录，实际 %d", n)
	}

	// 第 2 次尝试也超时 → 重试额度用尽，判死
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "install_agent", Title: "安装 SAgent", Scope: "external", Status: stRunning,
		Summary: "平台 ansible 执行中（自动重试）", StartedAt: past, FinishedAt: past,
		Detail: detailJSON(map[string]any{
			"executor": "ansible", "runner": "platform",
			"progress": map[string]any{
				"attempt": 2, "max_attempts": spec.Retry + 1,
				"attempt_started_at": past, "last_activity_at": past, "timeout": spec,
			},
			"attempts": []attemptRec{{No: 1, Result: "timeout_run", WaitedSec: 900}},
			"tasks":    []map[string]any{{"index": 1, "name": "make dirs", "status": "ok"}},
		}),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	sweepOnboardTimeouts(db, nil)

	ev2, _ := db.LatestEvent(id, "install_agent")
	if ev2.Status != stFail {
		t.Fatalf("重试用尽应判死（fail），实际 %s：%s", ev2.Status, ev2.Summary)
	}
	if !strings.Contains(ev2.Summary, "超时") {
		t.Errorf("判死摘要要说清是超时：%q", ev2.Summary)
	}
	if !hasDiagCode(ev2.Detail, "step_timeout") {
		t.Errorf("判死必须带 fatal 诊断（可执行建议）：%s", ev2.Detail)
	}
	if n := len(attemptsOf(ev2.Detail)); n != 2 {
		t.Errorf("尝试历史应累计 2 笔（运维要看平台救过几次），实际 %d", n)
	}
	if len(progressTasksOf(ev2.Detail)) == 0 {
		t.Errorf("判死时要沿用最后的任务快照（卡在哪一步最关键）：%s", ev2.Detail)
	}
	// 依赖它的下游应落 blocked，而不是继续假装能跑
	if st := latestStatuses(t, db, id)["verify_probe"]; st != stBlocked {
		t.Errorf("下游应被阻断，实际 %s", st)
	}
}

// 超时重试接管后，旧作业晚到的结果必须被丢弃（否则新尝试的进展会被旧结果覆盖）
func TestJobStillCurrentDropsStaleAttempt(t *testing.T) {
	db := testOnboardDB(t)
	id := makeFlow(t, db, "res-a", []storepkg.FlowStepSnapshot{
		{ID: "install_agent", Atom: "install_agent", Scope: "external"},
	})
	if !jobStillCurrent(db, id, "install_agent", 1) {
		t.Error("无事件时视为当前尝试")
	}
	write := func(status string, attempt int) {
		_ = func() error {
			_, err := db.AppendEvent(&storepkg.FlowEvent{
				FlowID: id, Step: "install_agent", Status: status, Summary: status + itoa(attempt),
				Detail: detailJSON(map[string]any{
					"progress": map[string]any{"attempt": attempt, "max_attempts": 2},
				}),
			})
			return err
		}()
	}
	write(stRunning, 1)
	if !jobStillCurrent(db, id, "install_agent", 1) {
		t.Error("attempt 一致时应视为当前尝试")
	}
	write(stRunning, 2)
	if jobStillCurrent(db, id, "install_agent", 1) {
		t.Error("attempt 已推进到 2，旧作业（attempt=1）必须被判定为过期")
	}
	write(stOK, 2)
	if jobStillCurrent(db, id, "install_agent", 2) {
		t.Error("步骤已终态（人工重置/已收口），任何作业都不该再写库")
	}
}

func TestCompactTasksDropsRawLines(t *testing.T) {
	long := strings.Repeat("X", 500)
	tasks := []ansibleTask{{Index: 1, Name: "make dirs", Status: "failed",
		Lines: []string{"a", "b"}, Stdout: long}}
	out := compactTasks(tasks)
	if len(out) != 1 {
		t.Fatalf("长度应保持，实际 %d", len(out))
	}
	if out[0].Lines != nil {
		t.Error("进度事件里的任务快照不该带逐任务原文行（会把事件表撑大）")
	}
	if len(out[0].Stdout) > 320 {
		t.Errorf("长字段应截断，实际 %d 字节", len(out[0].Stdout))
	}
	if tasks[0].Lines == nil {
		t.Error("不得修改原对象（终态事件还要用完整原文）")
	}
}

// 步骤「耗时」必须以"本次尝试起点"为基准。
// 流式执行器在每个 TASK 边界和心跳（15s）都会写一条 running 进度事件，若按"最近一条
// running 事件"起算，一次跑了 24s 的安装会在界面上显示成「耗时 1.0s」，与同屏渲染的
// 尝试记录（第 1 次 · 等待 24s）自相矛盾——运维据此判断性能会得出错误结论。
func TestStepDurationBaseUsesAttemptStart(t *testing.T) {
	now := time.Now()
	attemptStart := now.Add(-30 * time.Second).Format("2006-01-02 15:04:05")
	lastFlush := now.Add(-1 * time.Second).Format("2006-01-02 15:04:05")

	withProgress := &storepkg.FlowEvent{
		Status:    stRunning,
		StartedAt: lastFlush, // 最后一次 TASK 边界刷新的时间
		Detail:    `{"progress":{"attempt":1,"attempt_started_at":"` + attemptStart + `"}}`,
	}
	if got := attemptDurationBase(withProgress); got != attemptStart {
		t.Errorf("应取本次尝试起点 %q，实际 %q（会被算成 1s 而非 30s）", attemptStart, got)
	}

	legacy := &storepkg.FlowEvent{Status: stRunning, StartedAt: lastFlush}
	if got := attemptDurationBase(legacy); got != lastFlush {
		t.Errorf("无进度元数据的老事件应回退事件时间戳 %q，实际 %q", lastFlush, got)
	}
	if got := attemptDurationBase(nil); got != "" {
		t.Errorf("空事件应返回空串，实际 %q", got)
	}
}

// ===================================================================
//  卸载接入（offboard）：接入的逆操作
//  2026-09-21 用户要求「实现 SAgent 的删除卸载，以便闭环测试」。
//  这里钉住四件事：
//    ① 卸载模板自洽且不进"新建接入"向导（internal）
//    ② 卸载 playbook 渲染无残留占位符，且内置兜底与外置文件结果态一致
//    ③ 目标机自证核对行能被解析出来（解析不到不许编造结论）
//    ④ 平台侧收口真的把登记清干净，且不误伤其他资源
// ===================================================================

func offboardTemplate(t *testing.T) *FlowTemplate {
	t.Helper()
	tpls, err := loadFlowTemplates(defaultFlowTemplateDir)
	if err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	for _, tp := range tpls {
		if tp.ID == "offboard" {
			return tp
		}
	}
	t.Fatal("offboard 模板缺失：data/flow_templates/offboard.yaml 未随镜像就位")
	return nil
}

func TestOffboardTemplateStructure(t *testing.T) {
	tpl := offboardTemplate(t)
	if !tpl.Internal {
		t.Errorf("offboard 必须是 internal 模板（否则会出现在「新建接入」的模式单选里，语义矛盾）")
	}
	wantOrder := []string{"pick_object", "scan_collectors", "confirm_uninstall",
		"uninstall_plugins", "cleanup_autostart", "uninstall_agent", "cleanup_platform"}
	got := []string{}
	byID := map[string]FlowTemplateSt{}
	for _, s := range tpl.Steps {
		got = append(got, s.ID)
		byID[s.ID] = s
	}
	if strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("offboard 步骤顺序 = %v, want %v", got, wantOrder)
	}
	// 顺序不是排版问题，是依赖问题：这三个"必须在前"的关系一旦被调换，
	// 卸载就会出现"删了又活""插件变孤儿"这类假失败
	idx := map[string]int{}
	for i, id := range got {
		idx[id] = i
	}
	if idx["scan_collectors"] > idx["confirm_uninstall"] {
		t.Errorf("扫描必须在人工确认之前：确认的内容就是扫描的实测结果")
	}
	if idx["uninstall_plugins"] > idx["uninstall_agent"] {
		t.Errorf("卸插件必须在卸 SAgent 之前：SAgent 先死会让插件子进程变成孤儿继续采集")
	}
	if idx["cleanup_autostart"] > idx["uninstall_agent"] {
		t.Errorf("清自愈自启必须在卸 SAgent 之前：run.sh 守护会在 SAgent 退出后 3 秒把它拉回来")
	}
	if idx["cleanup_platform"] < idx["uninstall_agent"] {
		t.Errorf("注销平台登记必须在卸 SAgent 之后：反过来会在中途失败时留下「平台已注销、机器仍在采集」的黑洞")
	}
	// 扫描是只读步骤，不该带 blocker（它失败的意义是"没看成"，不是"门禁拦住"）
	if sc := byID["scan_collectors"]; sc.Scope != "external" || sc.Gate == "blocker" {
		t.Errorf("scan_collectors 应为 external 且不带 blocker（只读扫描失败只阻断自身下游），实际 scope=%q gate=%q", sc.Scope, sc.Gate)
	}
	// 人工确认是平台侧决策点
	if cf := byID["confirm_uninstall"]; cf.Scope != "platform" || !containsStr(cf.Requires, "scan_collectors") {
		t.Errorf("confirm_uninstall 应为 platform 且依赖 scan_collectors，实际 scope=%q requires=%v", cf.Scope, cf.Requires)
	}
	// 卸插件与清自启是破坏性动作，必须带 blocker：它们没成功就不能往下走
	for _, id := range []string{"uninstall_plugins", "cleanup_autostart"} {
		s := byID[id]
		if s.Scope != "external" {
			t.Errorf("%s 必须是 external（由平台 ansible / 人工执行器执行），实际 %q", id, s.Scope)
		}
		if s.Gate != "blocker" {
			t.Errorf("%s 必须带 gate=blocker：它没清干净时下游不得继续", id)
		}
		if !strings.Contains(s.Instruction, "{{.port}}") {
			t.Errorf("%s 兜底执行包必须含 {{.port}}（否则会默认走 22，连错端口却看着像成功）", id)
		}
		if strings.Contains(s.Instruction, "pkill -f") {
			t.Errorf("%s 兜底执行包不得使用 pkill -f（会杀掉执行本行的 shell 自身）：\n%s", id, s.Instruction)
		}
	}
	// 兜底执行包的两条硬道理：先让 SAgent 自己停插件；守护必须 KILL 而不是 TERM
	if ins := byID["uninstall_plugins"].Instruction; !strings.Contains(ins, "control.sock") {
		t.Errorf("卸插件兜底包必须走 control.sock 让 SAgent 自己停插件——直接 kill 会被崩溃自愈在 2 秒后拉回来：\n%s", ins)
	}
	if ins := byID["cleanup_autostart"].Instruction; !strings.Contains(ins, "kill -9") || strings.Contains(ins, "kill -TERM") {
		t.Errorf("清自启兜底包对守护必须用 kill -9（run.sh 的 TERM trap 会拿 pid 文件当进程组误杀无关进程）：\n%s", ins)
	}
	un := byID["uninstall_agent"]
	if un.Scope != "external" {
		t.Errorf("uninstall_agent 必须是 external（由平台 ansible / 人工执行器执行），实际 %q", un.Scope)
	}
	if un.Gate != "blocker" {
		t.Errorf("uninstall_agent 必须带 gate=blocker：卸载失败时下游「清平台登记」不得继续，否则会把「没卸掉」当成卸掉了")
	}
	// 兜底执行包里端口必须显式带上：`ssh user@ip` 默认走 22，正是"连错端口却看着像成功"的根源
	if !strings.Contains(un.Instruction, "{{.port}}") {
		t.Errorf("uninstall_agent 兜底执行包必须含 {{.port}}，实际：%q", un.Instruction)
	}
	// 兜底执行包与平台 playbook 同序：先停进程（等它退出）再删目录。
	// 顺序反了就是那个真实踩过的竞态——SAgent 优雅关停期间的写盘会把删掉的目录重建出来
	if kill, rm := strings.Index(un.Instruction, "readlink"), strings.Index(un.Instruction, "rm -rf"); kill < 0 || rm < 0 || kill > rm {
		t.Errorf("兜底执行包必须先停进程再删目录（kill=%d, rm=%d）：\n%s", kill, rm, un.Instruction)
	}
	// pkill -f 会匹配到执行本行的 sh 自身（历史踩坑），兜底包里不许出现
	if strings.Contains(un.Instruction, "pkill -f") {
		t.Errorf("兜底执行包不得使用 pkill -f（会杀掉执行本行的 shell 自身）：\n%s", un.Instruction)
	}
	cl := byID["cleanup_platform"]
	if cl.Scope != "platform" {
		t.Errorf("cleanup_platform 是平台侧收口，必须是 platform，实际 %q", cl.Scope)
	}
	if !containsStr(cl.Requires, "uninstall_agent") {
		t.Errorf("cleanup_platform 必须依赖 uninstall_agent，实际 %v", cl.Requires)
	}
}

// 向导模式清单不含 internal 模板；但按 id 仍必须取得到（创建入口在资源行内按钮）
func TestWizardTemplatesExcludeInternal(t *testing.T) {
	if _, err := loadFlowTemplates(defaultFlowTemplateDir); err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	for _, tp := range flowTemplateListForWizard() {
		if tp.Internal {
			t.Errorf("向导模式清单里出现了 internal 模板：%s", tp.ID)
		}
	}
	found := false
	for _, tp := range flowTemplateListForWizard() {
		if tp.ID == "offboard" {
			found = true
		}
	}
	if found {
		t.Errorf("offboard 不应出现在「新建接入」向导的模式清单里")
	}
	if getFlowTemplate("offboard") == nil {
		t.Errorf("offboard 模板必须仍能按 id 取到（/api/onboard/flow/offboard 靠它创建流水线）")
	}
}

// 卸载 playbook 渲染：占位符必须全部替换；内置兜底与外置文件结果态一致
func TestUninstallPlaybookRendering(t *testing.T) {
	pb := loadUninstallPlaybook("/home/ibomc")
	if strings.Contains(pb, "@DEST_HOME@") {
		t.Fatalf("渲染后仍残留 @DEST_HOME@（会把未渲染的路径发到目标机）：\n%s", pb)
	}
	if !strings.Contains(pb, "/home/ibomc/SAgent") {
		t.Fatalf("渲染后应指向 /home/ibomc/SAgent（装在哪卸哪，口径必须与安装一致）：\n%s", pb)
	}
	for _, must := range []string{"UNINSTALL_CLEAN", "断言卸载结果", "卸载结果核对", "删除 SAgent 安装目录"} {
		if !strings.Contains(pb, must) {
			t.Errorf("卸载 playbook 缺少关键内容 %q（删掉它等于放弃目标机自证）", must)
		}
	}
	// 内置兜底（外置文件丢失时的回落）必须与之一致：否则"改配置生效了没有"无法判定
	builtin := strings.NewReplacer("@DEST_HOME@", "/home/ibomc").Replace(uninstallPlaybookTpl)
	if strings.Contains(builtin, "@DEST_HOME@") {
		t.Errorf("内置兜底模板渲染后残留占位符")
	}
	for _, must := range []string{"UNINSTALL_CLEAN", "断言卸载结果", "删除 SAgent 安装目录"} {
		if !strings.Contains(builtin, must) {
			t.Errorf("内置兜底模板缺少 %q——外置文件丢失时会退化成一份不核对、不删目录的 playbook", must)
		}
	}
}

// 内置兜底模板与外置 playbook 必须同源：不一致时，"外置文件到底生效没有"无法判定，
// 而且同一份卸载动作会随部署形态（文件在/不在）表现不同——这类漂移最难排
func TestUninstallPlaybookBuiltinAlignedWithFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("data", "playbooks", "uninstall.yml"))
	if err != nil {
		t.Fatalf("读取外置卸载 playbook 失败：%v", err)
	}
	normalize := func(s string) string {
		out := []string{}
		for _, ln := range strings.Split(s, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "#") {
				continue
			}
			out = append(out, strings.TrimRight(ln, " \t"))
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	if got, want := normalize(string(raw)), normalize(uninstallPlaybookTpl); got != want {
		t.Errorf("data/playbooks/uninstall.yml 与内置兜底模板不一致（两份卸载动作必须同源）\n--- 外置 ---\n%s\n--- 内置 ---\n%s", got, want)
	}
}

// 目标机自证核对行的解析：从 debug 任务的 msg 里取；取不到返回空串（不许编造结论）
func TestUninstallCheckLineExtraction(t *testing.T) {
	r := ansibleRunResult{Tasks: []ansibleTask{
		{Name: "删除 SAgent 安装目录", Status: "changed"},
		{Name: "输出卸载核对结果", Status: "ok",
			Msg: "卸载核对 dir_exists=no process_exists=no port_agent_listening=no\n"},
		{Name: "断言卸载结果", Status: "ok"},
	}}
	if got := uninstallCheckLine(r); !strings.Contains(got, "dir_exists=no") {
		t.Errorf("应从 debug msg 里取出核对行，实际 %q", got)
	}
	// msg 里核对行后面常跟着断言标记：只许取核对那一行，别把断言结论当核对内容带进界面
	r3 := ansibleRunResult{Tasks: []ansibleTask{
		{Name: "输出卸载核对结果", Status: "ok",
			Msg: "卸载核对 dir_exists=no process_exists=no port_agent_listening=no\nUNINSTALL_CLEAN"},
	}}
	if got := uninstallCheckLine(r3); strings.Contains(got, "\n") || strings.Contains(got, "UNINSTALL_CLEAN") {
		t.Errorf("核对行只该是一行、且不含断言标记，实际 %q", got)
	}
	// 核对行只在原始输出行里出现时也要能取到
	r2 := ansibleRunResult{Tasks: []ansibleTask{
		{Lines: []string{"changed: [l0node]", "dir_exists=yes process_exists=no"}},
	}}
	if got := uninstallCheckLine(r2); got != "dir_exists=yes process_exists=no" {
		t.Errorf("应从原始输出行里取出核对行，实际 %q", got)
	}
	if got := uninstallCheckLine(ansibleRunResult{Tasks: []ansibleTask{{Name: "x"}}}); got != "" {
		t.Errorf("取不到核对行时必须返回空串（不编造结论），实际 %q", got)
	}
}

// 核对判据写在 playbook 的 shell 里，编译器和结构体检查都看不见它——
// 曾经因此漏过一处致命写反：取值语义与字段名相反（dir_exists=yes 其实代表"不存在"）、
// clean 条件又反着写，结果**卸得再干净也判失败**。结构体测试全绿，真机一跑就废。
// 所以这里把上线的那份脚本原文抽出来真跑一遍，用「目录在 / 目录不在」两种状态把语义钉死。
func TestUninstallCheckScriptJudgesBothStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("核对脚本是 POSIX sh，Windows 上跳过")
	}
	raw, err := os.ReadFile(filepath.Join("data", "playbooks", "uninstall.yml"))
	if err != nil {
		t.Fatalf("读取外置卸载 playbook 失败：%v", err)
	}
	run := func(destHome string) string {
		t.Helper()
		script := uninstallCheckScript(t, string(raw), destHome)
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("核对脚本执行失败：%v\n%s", err, out)
		}
		return string(out)
	}
	// 净：目标机没有安装目录 → 必须判净（这条就是被写反逻辑害死的场景）
	clean := run(t.TempDir())
	if !strings.Contains(clean, "dir_exists=no") {
		t.Errorf("目录不存在时 dir_exists 应为 no（字段名即字面意思），实际输出：\n%s", clean)
	}
	if !strings.Contains(clean, "UNINSTALL_CLEAN") || strings.Contains(clean, "UNINSTALL_DIRTY") {
		t.Errorf("目录不存在时必须判 UNINSTALL_CLEAN，实际输出：\n%s", clean)
	}
	// 脏：安装目录还在 → 必须判脏（不许把"没卸掉"糊成"卸好了"）
	dirtyHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirtyHome, "SAgent", "bin"), 0o755); err != nil {
		t.Fatalf("造脏状态目录失败：%v", err)
	}
	dirty := run(dirtyHome)
	if !strings.Contains(dirty, "dir_exists=yes") {
		t.Errorf("目录存在时 dir_exists 应为 yes，实际输出：\n%s", dirty)
	}
	if !strings.Contains(dirty, "UNINSTALL_DIRTY") || strings.Contains(dirty, "UNINSTALL_CLEAN") {
		t.Errorf("目录还在时必须判 UNINSTALL_DIRTY，实际输出：\n%s", dirty)
	}
}

// uninstallCheckScript 从 playbook 原文里抽出「卸载结果核对」任务的 shell 脚本并渲染占位符——
// 测的就是真正发到目标机的那份脚本，不是另抄一份（抄一份就又会漂移）
func uninstallCheckScript(t *testing.T, raw, destHome string) string {
	t.Helper()
	head := strings.Index(raw, "- name: 卸载结果核对")
	if head < 0 {
		t.Fatal("playbook 里找不到「卸载结果核对」任务")
	}
	tail := strings.Index(raw[head:], "register: uninstall_check")
	if tail < 0 {
		t.Fatal("「卸载结果核对」任务缺少 register: uninstall_check")
	}
	blk := raw[head : head+tail]
	i := strings.Index(blk, "shell: |")
	if i < 0 {
		t.Fatal("「卸载结果核对」任务缺少 shell 块")
	}
	var sb strings.Builder
	for _, ln := range strings.Split(blk[i+len("shell: |"):], "\n") {
		sb.WriteString(strings.TrimPrefix(ln, "        "))
		sb.WriteString("\n")
	}
	return strings.NewReplacer("@DEST_HOME@", destHome, "@AGENT_PORT@", cfgAgentHTTPPort).Replace(sb.String())
}

func TestClassifyUninstallFailures(t *testing.T) {
	cases := []struct {
		name, out string
		rc        int
		wantCode  string
	}{
		{"SSH 传不到走 SSH 层归因",
			"UNREACHABLE! => {\"msg\": \"Failed to connect to the host via ssh\", \"unreachable\": true}", 4, "ssh_unreachable"},
		{"核对断言不过 = 卸载没卸干净",
			"TASK [断言卸载结果（目录与进程均须消失）] ***\nfatal: [l0node]: FAILED! => {\"msg\": \"assertion failed\"}", 2, "uninstall_incomplete"},
		{"目录权限不足（Errno 13 不是 SSH 认证问题）",
			"TASK [删除 SAgent 安装目录] ***\nfatal: [l0node]: FAILED! => {\"msg\": \"[Errno 13] Permission denied: '/home/ibomc/SAgent'\"}", 2, "target_permission_denied"},
		{"其它失败落到通用归因",
			"TASK [停止 SAgent 并等待进程真正退出（先 SIGTERM，超时再 SIGKILL）] ***\nfatal: [l0node]: FAILED! => {\"msg\": \"boom\"}", 2, "uninstall_rc"},
	}
	for _, c := range cases {
		if got := classifyUninstallFailure(c.out, c.rc); got.Code != c.wantCode {
			t.Errorf("[%s] code = %q, want %q（msg=%s）", c.name, got.Code, c.wantCode, got.Message)
		}
	}
}

// 平台侧收口：Agent 台账（内存 + DB + 期望配置）、抓取登记、采集目标都要清掉；
// 资源对象与凭据保留；归属其他资源的采集目标不得误删
func TestAtomCleanupPlatformClearsRegistrations(t *testing.T) {
	testOnboardCfg(t)
	t.Chdir(t.TempDir()) // SD 登记用相对路径 data/scrape-sd，测试在干净工作目录里跑
	db := testOnboardDB(t)
	store := NewAgentStore()

	const resID, agentID = "ip-10-20-0-9", "ip-10-20-0-9"
	const otherRes = "ip-10-20-0-10"
	if err := db.UpsertResource(&storepkg.Resource{
		ID: resID, Name: resID, IP: "10.20.0.9", ResourceType: "host", Role: "business",
		Source: "manual", SSHPort: 22022, SSHUser: "deploy", SSHPassword: "p",
	}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	if err := db.UpsertResource(&storepkg.Resource{
		ID: otherRes, Name: otherRes, IP: "10.20.0.10", ResourceType: "host", Role: "business", Source: "manual",
	}); err != nil {
		t.Fatalf("UpsertResource(other): %v", err)
	}
	// Agent 台账：内存 + DB + 期望配置
	store.Put(&Agent{ID: agentID, Name: agentID, Type: "edge", Status: "healthy", Host: "10.20.0.9", Port: 19090})
	if err := db.UpsertAgentRow(&storepkg.AgentRow{ID: agentID, Name: agentID, Source: "heartbeat", LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("UpsertAgentRow: %v", err)
	}
	if _, _, err := db.SetAgentConfig(agentID, `{"plugins":{}}`); err != nil {
		t.Fatalf("SetAgentConfig: %v", err)
	}
	// 采集目标：本资源的 1 条 + 别的资源的 1 条（后者不得被删）
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: resID + "-mysql", Type: "mysql_probe", Address: "10.20.0.9", AgentID: agentID,
		Plugin: "mysql_probe", ResourceID: resID,
	}); err != nil {
		t.Fatalf("InsertTarget: %v", err)
	}
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: otherRes + "-mysql", Type: "mysql_probe", Address: "10.20.0.10", AgentID: agentID,
		Plugin: "mysql_probe", ResourceID: otherRes,
	}); err != nil {
		t.Fatalf("InsertTarget(other): %v", err)
	}
	// 抓取登记（file_sd）
	sdPath, err := registerScrapeTarget(resID, "10.20.0.9")
	if err != nil {
		t.Fatalf("registerScrapeTarget: %v", err)
	}

	res, _ := db.GetResource(resID)
	f := &storepkg.Flow{ID: 42, ResourceID: resID, Mode: "offboard", TemplateID: "offboard", AgentID: agentID}
	r := &flowRun{catDB: db, agentStore: store, flow: f, resource: res, agentID: agentID, cfg: onboardCfg}
	status, summary, detail := r.atomCleanupPlatform(db, store)
	if status != stOK {
		t.Fatalf("清理平台登记应成功，实际 %s：%s", status, summary)
	}
	if !strings.Contains(summary, "资源对象与 SSH 凭据保留") {
		t.Errorf("摘要必须写明保留了什么（否则会被读成「资源也被删了」）：%s", summary)
	}
	// ① Agent 台账
	if store.Get(agentID) != nil {
		t.Errorf("内存台账仍有该 Agent")
	}
	if src, _ := db.GetAgentSource(agentID); src != "" {
		t.Errorf("DB 台账仍有该 Agent（source=%s）", src)
	}
	// ② 期望配置
	if ver, _, _ := db.GetAgentConfig(agentID); ver != 0 {
		t.Errorf("期望配置未清除（version=%d）——重装时配置版本不会重新从 1 开始", ver)
	}
	// ③ 抓取登记
	if _, err := os.Stat(sdPath); !os.IsNotExist(err) {
		t.Errorf("vmagent 抓取登记未撤除：%s", sdPath)
	}
	// ④ 采集目标：本资源的删掉、别的资源保留
	if old, _ := db.FindTargetByName(resID + "-mysql"); old != nil {
		t.Errorf("本资源的采集目标未删除：%s", old.Name)
	}
	if old, _ := db.FindTargetByName(otherRes + "-mysql"); old == nil {
		t.Errorf("误删了归属其他资源的采集目标（共用承载机时按 agent_id 删就会这样）")
	}
	// ⑤ 资源对象与凭据保留 → 可原样重新接入
	after, _ := db.GetResource(resID)
	if after == nil || after.SSHPort != 22022 || after.SSHUser != "deploy" {
		t.Errorf("资源对象/凭据必须保留（卸载后要能原样重新接入），实际 %+v", after)
	}
	if !strings.Contains(detail, "resource_object") {
		t.Errorf("detail 应显式说明保留项，实际：%s", detail)
	}
}

// 目标机侧不是平台 ansible 实测成功时必须给告警：否则台账清完后 Agent 会在下个心跳
// 重新注册，平台凭空冒出一台幽灵 Agent，看起来像"平台乱写数据"
func TestCleanupPlatformWarnsWhenUninstallNotVerified(t *testing.T) {
	testOnboardCfg(t)
	t.Chdir(t.TempDir())
	db := testOnboardDB(t)
	store := NewAgentStore()
	const resID = "ip-10-20-0-11"
	if err := db.UpsertResource(&storepkg.Resource{ID: resID, Name: resID, IP: "10.20.0.11", Source: "manual"}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	steps := []storepkg.FlowStepSnapshot{
		{ID: "uninstall_agent", Atom: "uninstall_agent", Title: "卸载 SAgent", Scope: "external"},
		{ID: "cleanup_platform", Atom: "cleanup_platform", Title: "注销平台登记", Scope: "platform"},
	}
	fID := makeFlow(t, db, resID, steps)
	// 人工强制通过：不是 ansible 实测成功
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: fID, Step: "uninstall_agent", Status: stOK, Summary: "人工强制通过",
		Detail: detailJSON(map[string]any{"forced": true, "reason": "目标机已销毁"}),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	f, _ := db.GetFlow(fID)
	res, _ := db.GetResource(resID)
	r := &flowRun{catDB: db, agentStore: store, flow: f, resource: res, agentID: resID, cfg: onboardCfg}
	status, summary, detail := r.atomCleanupPlatform(db, store)
	if status != stOK {
		t.Fatalf("应仍成功（告警不阻断），实际 %s：%s", status, summary)
	}
	if !hasDiagCode(detail, "uninstall_not_verified") {
		t.Errorf("应给出 uninstall_not_verified 告警，实际 detail=%s", detail)
	}
	if !strings.Contains(summary, "项待复核") {
		t.Errorf("告警必须出现在摘要里（列表页徽标靠它），实际：%s", summary)
	}

	// 平台 ansible 真跑成功时不该出现该告警
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: fID, Step: "uninstall_agent", Status: stOK, Summary: "ansible 卸载完成",
		Detail: detailJSON(map[string]any{"source": "ansible_uninstall"}),
	}); err != nil {
		t.Fatalf("AppendEvent#2: %v", err)
	}
	f2, _ := db.GetFlow(fID)
	r2 := &flowRun{catDB: db, agentStore: store, flow: f2, resource: res, agentID: resID, cfg: onboardCfg}
	_, _, detail2 := r2.atomCleanupPlatform(db, store)
	if hasDiagCode(detail2, "uninstall_not_verified") {
		t.Errorf("平台 ansible 实测成功时不该报该告警，实际 detail=%s", detail2)
	}
}

// flowView 的卸载入口语义：装过的接入流水线给入口；卸载流水线自身不给；
// 卸载完成后（台账已注销）入口自动消失
func TestFlowViewCanOffboard(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	store := NewAgentStore()
	const resID, agentID = "ip-10-20-0-12", "ip-10-20-0-12"
	steps := []storepkg.FlowStepSnapshot{
		{ID: "pick_object", Atom: "pick_object", Title: "选对象", Scope: "platform"},
		{ID: "install_agent", Atom: "install_agent", Title: "装 Agent", Scope: "external", Requires: []string{"pick_object"}},
	}
	fID := makeFlow(t, db, resID, steps)
	f, _ := db.GetFlow(fID)
	// 未装过（无台账、安装未落终态）→ 不给入口
	if v := flowView(db, store, f); v["can_offboard"] != false {
		t.Errorf("未装过的资源不该给卸载入口，实际 can_offboard=%v", v["can_offboard"])
	}
	// 安装环节 ok 且流水线仍在推进 → 给入口（装完到 Agent 首次心跳登记的空窗期，
	// 窗口内入口不该闪一下又没有）
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: fID, Step: "install_agent", Status: stOK, Summary: "安装完成",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	f2, _ := db.GetFlow(fID)
	if v := flowView(db, store, f2); v["can_offboard"] != true {
		t.Errorf("安装完成且流水线还在推进时应给卸载入口，实际 can_offboard=%v", v["can_offboard"])
	}
	// 台账里有 Agent → 给入口（与流水线是否终态无关：这是"现在真的装着"的权威证据）
	store.Put(&Agent{ID: agentID, Name: agentID, Type: "edge", Status: "healthy"})
	if v := flowView(db, store, f2); v["can_offboard"] != true {
		t.Errorf("有 Agent 台账时应给入口，实际 can_offboard=%v", v["can_offboard"])
	}
	// 卸载完成后（台账被注销 + 安装流水线已终态）入口必须消失：
	// 否则人刚点完卸载，回头又看见「卸载」按钮，只会以为没卸掉（实测踩过）
	store.Remove(agentID)
	if err := db.UpdateFlowStep(fID, "install_agent", "done"); err != nil {
		t.Fatalf("UpdateFlowStep: %v", err)
	}
	f4, _ := db.GetFlow(fID)
	if v := flowView(db, store, f4); v["can_offboard"] != false {
		t.Errorf("卸载后（台账已注销、安装流水线已终态）不该再挂卸载入口，实际 can_offboard=%v", v["can_offboard"])
	}
	// 卸载流水线自身不给入口
	off := &storepkg.Flow{ID: fID + 1, ResourceID: resID, Mode: "offboard", TemplateID: "offboard"}
	v := flowView(db, store, off)
	if v["is_offboard"] != true || v["can_offboard"] != false {
		t.Errorf("卸载流水线自身不该再给卸载入口：is_offboard=%v can_offboard=%v", v["is_offboard"], v["can_offboard"])
	}
}

// hasDiagCode 诊断里是否含某 code（按 code 断言而不是按数量，避免被别的告警干扰）
func hasDiagCode(detail, code string) bool {
	var d struct {
		Diagnosis []struct {
			Code  string `json:"code"`
			Level string `json:"level"`
		} `json:"diagnosis"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return false
	}
	for _, x := range d.Diagnosis {
		if x.Code == code {
			return true
		}
	}
	return false
}

// ===================================================================
//  卸载前置检查链：扫描 → 确认 → 卸插件 → 清自启
// ===================================================================

// scanFixture 造一次「扫描 playbook 的真实输出」。
// 行内容照抄 data/playbooks/scan_collectors.yml 的输出契约——契约改了这里会先红
func scanFixture(lines ...string) ansibleRunResult {
	return ansibleRunResult{Tasks: []ansibleTask{{
		Index: 1, Name: "扫描采集插件与自愈守护（只读，不改动目标机）", Status: "ok",
		Stdout: strings.Join(lines, "\n"),
	}}}
}

func TestParseScanOutput(t *testing.T) {
	got := parseScanOutput(scanFixture(
		"PLUGIN name=host_metrics form=builtin source=config enabled=yes process=no path=-",
		"PLUGIN name=log_metrics form=subprocess source=config enabled=yes process=yes path=plugins/vector/bin/vector",
		"PLUGIN name=custom_scripts form=exec source=dir enabled=no process=unknown path=plugins/custom_scripts",
		"AUTOSTART kind=run_sh state=running hits=1 pid=1588 note=SAgent-package-supervisor",
		"AUTOSTART kind=cron_user state=absent hits=0 note=read",
		"AUTOSTART kind=cron_system state=present hits=2 unreadable=0 note=report-only",
		"SCAN plugins_total=3 plugins_process=1 autostart=2",
		"SCAN_END",
	))
	if len(got.Plugins) != 3 {
		t.Fatalf("插件条数 = %d, want 3（解析：%+v）", len(got.Plugins), got.Plugins)
	}
	if p := got.Plugins[0]; p.Name != "host_metrics" || p.Form != "builtin" || !p.Enabled || p.Process != "no" {
		t.Errorf("第一条插件解析错误：%+v", p)
	}
	if p := got.Plugins[1]; p.Process != "yes" || p.Path != "plugins/vector/bin/vector" {
		t.Errorf("第二条插件解析错误（独立进程/产物路径必须拿到）：%+v", p)
	}
	if p := got.Plugins[2]; p.Enabled || p.Source != "dir" || p.Process != "unknown" {
		t.Errorf("第三条插件解析错误（目录实存未启用 / 进程无法判定）：%+v", p)
	}
	if len(got.Autostart) != 3 {
		t.Fatalf("自启条数 = %d, want 3", len(got.Autostart))
	}
	if a := got.Autostart[0]; a.Kind != "run_sh" || a.State != "running" || a.PID != "1588" {
		t.Errorf("守护条解析错误（pid 值里带空格会把按空格切分打乱）：%+v", a)
	}
	if a := got.Autostart[2]; a.Hits != 2 {
		t.Errorf("系统级 crontab 命中数解析错误：%+v", a)
	}
	if got.PluginsTotal != 3 || got.ProcessCount != 1 || got.AutostartCount != 2 {
		t.Errorf("汇总解析错误：total=%d proc=%d autostart=%d", got.PluginsTotal, got.ProcessCount, got.AutostartCount)
	}
}

// 输出里没有可解析的结论行时必须判失败 —— 否则会以"扫到 0 个"收场，
// 而"这机器上什么都没有"是最危险的一种谎报
func TestScanVerdictRejectsUnparsableOutput(t *testing.T) {
	extra, ds := offboardScanSpec.Verdict(ansibleRunResult{
		Tasks: []ansibleTask{{Name: "扫描", Status: "ok", Stdout: "something unexpected"}},
	})
	if extra["scan"] == nil {
		t.Errorf("不可解析时仍应回传空的 scan 结构，便于前端判断：%+v", extra)
	}
	if f := firstFatal(ds); f == nil {
		t.Fatalf("输出不可解析必须给 fatal（不能默认「一个插件都没有」），实际诊断：%+v", ds)
	}
	if !hasDiagCode(detailJSON(map[string]any{"diagnosis": diagJSON(ds)}), "scan_output_unparsable") {
		t.Errorf("失败诊断应带 scan_output_unparsable 码：%+v", ds)
	}
}

// crontab 读不到（非 suid、权限不足）必须升级成告警而不是静默当"没有残留"
func TestScanVerdictWarnsWhenCrontabUnreadable(t *testing.T) {
	extra, ds := offboardScanSpec.Verdict(scanFixture(
		"PLUGIN name=host_metrics form=builtin source=config enabled=yes process=no path=-",
		"AUTOSTART kind=run_sh state=absent hits=1 pid= note=SAgent-package-supervisor",
		"AUTOSTART kind=cron_user state=unknown hits=0 note=unreadable",
		"AUTOSTART kind=cron_system state=absent hits=0 unreadable=0 note=report-only",
		"SCAN plugins_total=1 plugins_process=0 autostart=0",
	))
	sc, _ := extra["scan"].(scanPayload)
	if !sc.CronUnverified {
		t.Errorf("cron_state=unknown 时必须置 CronUnverified（供确认面板提示人工复核）")
	}
	if f := firstFatal(ds); f != nil {
		t.Errorf("crontab 读不到是环境限制、不是卸载失败，不该给 fatal：%+v", f)
	}
	if !hasDiagCode(detailJSON(map[string]any{"diagnosis": diagJSON(ds)}), "cron_unverified") {
		t.Errorf("应出现 cron_unverified 告警：%+v", ds)
	}
}

func TestUninstallPluginsVerdict(t *testing.T) {
	ok := func(out string) ansibleRunResult {
		return ansibleRunResult{Tasks: []ansibleTask{{Status: "ok", Stdout: out}}}
	}
	if _, ds := offboardPluginsSpec.Verdict(ok("plugin_process_left=0\nPLUGINS_CLEAN")); firstFatal(ds) != nil {
		t.Errorf("PLUGINS_CLEAN 应判成功，实际 %+v", ds)
	}
	// 有残留：必须 fatal，且核对行要能取到（界面要看得出还剩几个）
	dirtyOut := "plugin_process_left=2\nPLUGINS_DIRTY"
	if f := firstFatal(mustVerdict(offboardPluginsSpec, ok(dirtyOut))); f == nil {
		t.Errorf("PLUGINS_DIRTY 必须判失败")
	}
	if ln := outCheckLine(ok(dirtyOut), offboardPluginsSpec.CheckLine); ln != "plugin_process_left=2" {
		t.Errorf("核对行应取到，实际 %q", ln)
	}
	// 两个标记都没有（输出被截断 / playbook 被改）：同样必须失败，不许当成功
	if f := firstFatal(mustVerdict(offboardPluginsSpec, ok(""))); f == nil {
		t.Errorf("拿不到 PLUGINS_CLEAN 时必须判失败——宁可报失败也不许默认干净")
	}
}

func TestCleanupAutostartVerdict(t *testing.T) {
	ok := func(out string) ansibleRunResult {
		return ansibleRunResult{Tasks: []ansibleTask{{Status: "ok", Stdout: out}}}
	}
	// 全清：成功且无告警
	if _, ds := offboardAutostartSpec.Verdict(ok("guard_exists=no cron_state=read cron_sagent_lines=0\nAUTOSTART_CLEAN")); firstFatal(ds) != nil {
		t.Errorf("AUTOSTART_CLEAN 应判成功，实际 %+v", ds)
	}
	// 守护还在 / crontab 有残留：硬失败
	if f := firstFatal(mustVerdict(offboardAutostartSpec, ok("guard_exists=yes\nAUTOSTART_DIRTY"))); f == nil {
		t.Errorf("AUTOSTART_DIRTY 必须判失败——守护还在的话 SAgent 会被重新拉起来")
	}
	// crontab 读不到：放行但必须告警（读不到 ≠ 有残留，把环境限制当卸载失败会造假失败；
	// 但也绝不能静默放过）
	extra, ds := offboardAutostartSpec.Verdict(ok("guard_exists=no cron_state=unreadable cron_sagent_lines=0\nAUTOSTART_PARTIAL"))
	if f := firstFatal(ds); f != nil {
		t.Errorf("AUTOSTART_PARTIAL 不该硬失败：%+v", f)
	}
	if extra["autostart_verdict"] != "partial" {
		t.Errorf("PARTIAL 结论要透出给界面，实际 %v", extra["autostart_verdict"])
	}
	if !hasDiagCode(detailJSON(map[string]any{"diagnosis": diagJSON(ds)}), "autostart_partial") {
		t.Errorf("PARTIAL 必须带 autostart_partial 告警：%+v", ds)
	}
}

// mustVerdict 跑一次判据并只取诊断（断言用）
func mustVerdict(sp offboardStepSpec, r ansibleRunResult) []diag {
	_, ds := sp.Verdict(r)
	return ds
}

// 内置兜底副本必须与外置文件逐字节同源。
// 这条能成立是靠 go:embed 在构建期读同一份文件——不是靠人肉同步两处文本
func TestSeedPlaybooksEmbeddedAlignedWithFiles(t *testing.T) {
	for _, name := range []string{"scan_collectors.yml", "uninstall_plugins.yml", "cleanup_autostart.yml"} {
		raw, err := os.ReadFile(filepath.Join("data", "playbooks", name))
		if err != nil {
			t.Fatalf("读取外置 playbook %s 失败：%v", name, err)
		}
		emb, err := playbookSeedFS.ReadFile("data/playbooks/" + name)
		if err != nil {
			t.Fatalf("内置副本 %s 缺失：%v", name, err)
		}
		if string(emb) != string(raw) {
			t.Errorf("%s 的内置副本与外置文件不一致（go:embed 本应保证同源）", name)
		}
		// 渲染后必须带目标家目录：漏渲染会让脚本去动错误的路径
		got := loadSeedPlaybook(name, "/home/probe")
		if !strings.Contains(got, "/home/probe/SAgent") {
			t.Errorf("%s 渲染后未出现目标机家目录，占位符替换可能失效", name)
		}
		if strings.Contains(got, "@DEST_HOME@") {
			t.Errorf("%s 渲染后仍残留 @DEST_HOME@ 占位符", name)
		}
	}
}

// playbookTaskShell 从 playbook 原文里抽出指定任务的 shell 脚本体并渲染占位符。
// 测的就是真正发到目标机的那份脚本，不另抄一份——抄一份就又会漂移
func playbookTaskShell(t *testing.T, raw, taskName, destHome string) string {
	t.Helper()
	marker := "- name: " + taskName
	head := strings.Index(raw, marker)
	if head < 0 {
		t.Fatalf("playbook 里找不到任务「%s」", taskName)
	}
	rest := raw[head+len(marker):]
	if end := strings.Index(rest, "\n    - name: "); end >= 0 {
		rest = rest[:end]
	}
	i := strings.Index(rest, "shell: |")
	if i < 0 {
		t.Fatalf("任务「%s」缺少 shell 块", taskName)
	}
	var sb strings.Builder
	for _, ln := range strings.Split(rest[i+len("shell: |"):], "\n") {
		sb.WriteString(strings.TrimPrefix(ln, "        "))
		sb.WriteString("\n")
	}
	return strings.NewReplacer("@DEST_HOME@", destHome, "@AGENT_PORT@", cfgAgentHTTPPort).Replace(sb.String())
}

// crontabStub 造一个可编程的 crontab 命令桩：-l 读、- 写都由环境变量指到同一个后备文件。
// 用桩而不是真 crontab：真命令依赖 setuid，CI 与精简容器里跑不动，
// 而这里要验的是「我们怎么过滤」而不是「crontab 能不能用」
func crontabStub(t *testing.T, dir string) string {
	t.Helper()
	body := "#!/bin/sh\nif [ \"$1\" = \"-l\" ]; then cat \"$CRONTAB_STUB_FILE\"; exit 0; fi\n" +
		"if [ \"$1\" = \"-\" ]; then cat > \"$CRONTAB_STUB_FILE\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "crontab"), []byte(body), 0o755); err != nil {
		t.Fatalf("写 crontab 桩失败：%v", err)
	}
	return dir
}

// 真跑 crontab 过滤脚本：只许删含 SAgent 的行，别人的定时任务一字不动。
// 卸载脚本里"顺手清干净 crontab"是最容易误伤别人的一步，必须有真执行级的回归
func TestCleanupAutostartRemovesOnlySAgentCronLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("过滤脚本是 POSIX sh，Windows 上跳过")
	}
	raw, err := os.ReadFile(filepath.Join("data", "playbooks", "cleanup_autostart.yml"))
	if err != nil {
		t.Fatalf("读取 cleanup_autostart.yml 失败：%v", err)
	}
	destHome := t.TempDir()
	script := playbookTaskShell(t, string(raw), "清理 crontab 中指向 SAgent 的条目", destHome)

	binDir := crontabStub(t, t.TempDir())
	cronFile := filepath.Join(t.TempDir(), "crontab")
	before := "*/5 * * * * /home/deploy/SAgent/run.sh >> /dev/null 2>&1\n" +
		"0 1 * * * /usr/local/bin/backup.sh\n" +
		"@reboot /opt/other/agent --daemon\n"
	if err := os.WriteFile(cronFile, []byte(before), 0o644); err != nil {
		t.Fatalf("写 crontab 后备文件失败：%v", err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"CRONTAB_STUB_FILE="+cronFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("清理脚本执行失败：%v\n%s", err, out)
	}
	after, _ := os.ReadFile(cronFile)
	if strings.Contains(string(after), "SAgent") {
		t.Errorf("含 SAgent 的行应被删除，实际残留：\n%s", after)
	}
	for _, keep := range []string{"backup.sh", "/opt/other/agent"} {
		if !strings.Contains(string(after), keep) {
			t.Errorf("别人的定时任务 %q 被误删了（只许删含 SAgent 的行）：\n%s", keep, after)
		}
	}
	if !strings.Contains(string(out), "cron_state=removed") {
		t.Errorf("应报告 cron_state=removed，实际输出：\n%s", out)
	}
	// crontab 里本来就没有 SAgent 条目时不许改动内容
	before2 := "0 1 * * * /usr/local/bin/backup.sh\n"
	_ = os.WriteFile(cronFile, []byte(before2), 0o644)
	// exec.Cmd 不可复用（CombinedOutput 会把 Stdout 占住，第二次直接报错返回空），
	// 所以要新建一个命令对象，否则这里测的是"没有输出"而不是"脚本对不对"
	cmd2 := exec.Command("sh", "-c", script)
	cmd2.Env = cmd.Env
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil {
		t.Fatalf("第二次执行清理脚本失败：%v\n%s", err2, out2)
	}
	after2, _ := os.ReadFile(cronFile)
	if string(after2) != before2 {
		t.Errorf("无 SAgent 条目时不得改动 crontab，actual=%q", after2)
	}
	if !strings.Contains(string(out2), "cron_state=clean") {
		t.Errorf("无命中时应报告 cron_state=clean，实际：\n%s", out2)
	}
}

// 核对脚本的三态判定必须真跑验证：CLEAN / PARTIAL / DIRTY 分不清的话，
// 要么把"读不到"当成"有残留"（造假失败），要么把"还有残留"当成干净（假成功）
func TestCleanupAutostartCheckScriptThreeStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("核对脚本是 POSIX sh，Windows 上跳过")
	}
	raw, err := os.ReadFile(filepath.Join("data", "playbooks", "cleanup_autostart.yml"))
	if err != nil {
		t.Fatalf("读取 cleanup_autostart.yml 失败：%v", err)
	}
	destHome := t.TempDir()
	script := playbookTaskShell(t, string(raw), "自启清理结果核对（守护是否消失 / crontab 是否还有 SAgent 条目）", destHome)
	binDir := crontabStub(t, t.TempDir())
	cronFile := filepath.Join(t.TempDir(), "crontab")

	run := func(t *testing.T) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"CRONTAB_STUB_FILE="+cronFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("核对脚本执行失败：%v\n%s", err, out)
		}
		return string(out)
	}

	// A. crontab 干净 + 无守护文件 → CLEAN
	_ = os.WriteFile(cronFile, []byte("0 1 * * * /usr/local/bin/backup.sh\n"), 0o644)
	if out := run(t); !strings.Contains(out, "AUTOSTART_CLEAN") {
		t.Errorf("干净状态应判 AUTOSTART_CLEAN，实际：\n%s", out)
	}
	// B. crontab 里还有 SAgent 条目 → DIRTY
	_ = os.WriteFile(cronFile, []byte("*/5 * * * * /home/x/SAgent/run.sh\n"), 0o644)
	if out := run(t); !strings.Contains(out, "AUTOSTART_DIRTY") {
		t.Errorf("crontab 仍有 SAgent 条目时必须判 AUTOSTART_DIRTY，实际：\n%s", out)
	}
	// C. 守护脚本文件还在（进程已停）→ 仍必须 DIRTY：文件在，随时能被重新触发
	_ = os.WriteFile(cronFile, []byte("0 1 * * * /usr/local/bin/backup.sh\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(destHome, "SAgent"), 0o755); err != nil {
		t.Fatalf("造守护文件目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(destHome, "SAgent", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("造 run.sh 失败：%v", err)
	}
	if out := run(t); !strings.Contains(out, "guard_exists=yes") || !strings.Contains(out, "AUTOSTART_DIRTY") {
		t.Errorf("run.sh 文件还在时必须判 DIRTY，实际：\n%s", out)
	}
	_ = os.Remove(filepath.Join(destHome, "SAgent", "run.sh"))
	// D. crontab 读不到 → PARTIAL（放行但要求人工核实），既不许说 CLEAN 也不许说 DIRTY
	noCron := t.TempDir()
	_ = os.WriteFile(filepath.Join(noCron, "crontab"),
		[]byte("#!/bin/sh\necho 'crontab: must be suid to work properly' >&2\nexit 1\n"), 0o755)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+noCron+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("核对脚本执行失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "AUTOSTART_PARTIAL") {
		t.Errorf("crontab 读不到时应判 AUTOSTART_PARTIAL，实际：\n%s", out)
	}
	if strings.Contains(string(out), "AUTOSTART_CLEAN") {
		t.Errorf("crontab 未核实的情况下不许宣布 AUTOSTART_CLEAN：\n%s", out)
	}
}

// 判据标记只能来自目标机的回报，不能来自我们自己的任务名。
// 真机上踩过一次：断言任务的名字写成「…（残余只告警不阻断，见 AUTOSTART_PARTIAL）」，
// 于是 ansible 的 TASK [...] 框线把 AUTOSTART_PARTIAL 带进输出行，
// 干净的机器被判成 PARTIAL —— 一次"自己念错自己结论"的假告警
func TestTaskOutputLinesSkipsAnsibleFraming(t *testing.T) {
	r := ansibleRunResult{Tasks: []ansibleTask{{
		Name:   "断言自愈守护与 crontab 已清理",
		Status: "ok",
		Lines: []string{
			"TASK [断言自愈守护与 crontab 已清理（残余只告警不阻断，见 AUTOSTART_PARTIAL）] ***",
			"ok: [l0node]",
			"PLAY RECAP *********************************************************************",
		},
		Msg: "自启核对 guard_exists=no cron_state=read cron_sagent_lines=0\nAUTOSTART_CLEAN",
	}}}
	if hasOutMarker(r, "AUTOSTART_PARTIAL") {
		t.Errorf("任务名里的 AUTOSTART_PARTIAL 不该被当成判据：自己念错自己的结论")
	}
	if !hasOutMarker(r, "AUTOSTART_CLEAN") {
		t.Errorf("任务输出体里的 AUTOSTART_CLEAN 必须能被判出来")
	}
	// 结论应落在 CLEAN：目标机自证过了，不该因为一句散文降级成 PARTIAL
	extra, ds := offboardAutostartSpec.Verdict(r)
	if firstFatal(ds) != nil || hasDiagCode(detailJSON(map[string]any{"diagnosis": diagJSON(ds)}), "autostart_partial") {
		t.Errorf("守护已清、crontab 已核实无残留时应判 clean，实际诊断 %+v", ds)
	}
	if extra["autostart_verdict"] != "clean" {
		t.Errorf("autostart_verdict 应为 clean，实际 %v", extra["autostart_verdict"])
	}
}

// 上线的那几份 playbook，任务名/备注里一律不许出现判据标记 —— 反射式的防复发：
// 上一条测试挡的是平台侧解析，这一条挡的是写 playbook 的人
func TestPlaybookTaskNamesFreeOfVerdictMarkers(t *testing.T) {
	markers := []string{"AUTOSTART_CLEAN", "AUTOSTART_PARTIAL", "AUTOSTART_DIRTY",
		"PLUGINS_CLEAN", "PLUGINS_DIRTY", "UNINSTALL_CLEAN", "UNINSTALL_DIRTY"}
	files, err := filepath.Glob(filepath.Join("data", "playbooks", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("扫不到 playbook：%v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", f, err)
		}
		for i, ln := range strings.Split(string(raw), "\n") {
			// 只看任务名行：判据是 shell 里 echo 出来的，YAML 顶层的 name: 才是会进框线的那个
			if !strings.Contains(ln, "- name:") {
				continue
			}
			for _, m := range markers {
				if strings.Contains(ln, m) {
					t.Errorf("%s:%d 任务名里出现判据标记 %s（会随 TASK 框线污染输出行）：%s",
						f, i+1, m, strings.TrimSpace(ln))
				}
			}
		}
	}
}

// 守护判定必须是双路：只认 cwd 会在 cwd 读不到时整条漏判。
// 真机踩过：run.sh 守护被 reparent 到 PID 1 之后 /proc/<pid>/cwd 变成 Permission denied，
// 于是"停守护"这一步静默不生效，下一步删掉 SAgent 又被它拉回来 —— 表现为卸载功能坏了的假失败。
// 所以这里把两条判据钉死：父子关系（不依赖 cwd）+ cwd（覆盖守护的 sleep 空窗期）
func TestGuardDetectionUsesBothParentAndCwdRules(t *testing.T) {
	for _, f := range []string{"scan_collectors.yml", "cleanup_autostart.yml"} {
		raw, err := os.ReadFile(filepath.Join("data", "playbooks", f))
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", f, err)
		}
		s := string(raw)
		if !strings.Contains(s, "ppid_of") {
			t.Errorf("%s 缺少 ppid_of：守护判定会退化成只认 cwd", f)
		}
		if !strings.Contains(s, `"$AG"/bin/SAgent*`) {
			t.Errorf("%s 缺少「守护是 SAgent 本体的父进程」这条判据（cwd 不可读时就靠它）", f)
		}
		if !strings.Contains(s, `readlink "$d/cwd"`) {
			t.Errorf("%s 缺少 cwd 判据：SAgent 已退出、守护在 sleep 的那几秒会漏判", f)
		}
	}
	// 扫描侧还得先收到 SAgent 本体的 pid，否则父子关系无从谈起
	scan, _ := os.ReadFile(filepath.Join("data", "playbooks", "scan_collectors.yml"))
	if !strings.Contains(string(scan), "AGENT_PIDS") {
		t.Errorf("扫描侧未收集 SAgent 本体 pid，父子关系判据无从执行")
	}
	// kill 与核对必须同源：判据不一致会出现"kill 说已清、核对说还在"的自相矛盾
	clean, _ := os.ReadFile(filepath.Join("data", "playbooks", "cleanup_autostart.yml"))
	body := string(clean)
	if n := strings.Count(body, `case "$pcl" in *run.sh*`); n < 2 {
		t.Errorf("清理与核对两处都要有父子关系判据，实际只找到 %d 处", n)
	}
	if n := strings.Count(body, `readlink "$d/cwd"`); n < 2 {
		t.Errorf("清理与核对两处都要有 cwd 判据，实际只找到 %d 处", n)
	}
}

// 人工确认环节：没有扫描结果时必须等扫描，不许拿默认值顶上
func TestConfirmUninstallRequiresScanFirst(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	steps := []storepkg.FlowStepSnapshot{
		{ID: "scan_collectors", Atom: "scan_collectors", Title: "扫描", Scope: "external"},
		{ID: "confirm_uninstall", Atom: "confirm_uninstall", Title: "确认", Scope: "platform", Requires: []string{"scan_collectors"}},
	}
	fID := makeFlow(t, db, "ip-1-1-1-1", steps)
	flow, _ := db.GetFlow(fID)
	if flow == nil {
		t.Fatal("GetFlow 返回空：流水线未落库")
	}
	r := &flowRun{catDB: db, flow: flow}
	// 扫描结论必须记在 scan_collectors 步骤下（确认环节读的是那一步的 LatestEvent）。
	scanStep := storepkg.FlowStepSnapshot{ID: "scan_collectors", Atom: "scan_collectors", Scope: "external"}

	// ① 尚无扫描事件：落 blocked 且 waiting_for=scan（不是 human_confirm_offboard）
	status, _, detail := r.atomConfirmUninstall(db)
	if status != stBlocked {
		t.Fatalf("扫描未完成时应 blocked，实际 %q", status)
	}
	if waitingForKey(detail) != "scan" {
		t.Errorf("应等扫描，实际 waiting_for=%q（detail=%s）", waitingForKey(detail), detail)
	}

	// ② 扫描成功但缺结构化结论：必须 fail，不许让人对着空清单点确认
	_ = appendStepEvent(db, fID, scanStep, stOK, "扫描完成但结论缺失",
		detailJSON(map[string]any{"source": "ansible_scan_collectors"}), 0)
	status2, _, detail2 := r.atomConfirmUninstall(db)
	if status2 != stFail {
		t.Errorf("扫描事件缺 detail.scan 时必须 fail（不能让人对空清单确认），实际 %q：%s", status2, detail2)
	}

	// ③ 扫描带结论：落 blocked 且把影响范围摊出来
	scan := scanPayload{
		Plugins: []scanPlugin{
			{Name: "host_metrics", Form: "builtin", Source: "config", Enabled: true, Process: "no", Path: "-"},
			{Name: "log_metrics", Form: "subprocess", Source: "config", Enabled: true, Process: "yes", Path: "plugins/vector/bin/vector"},
		},
		Autostart:      []scanAutostart{{Kind: "run_sh", State: "running", Hits: 1}},
		PluginsTotal:   2,
		ProcessCount:   1,
		AutostartCount: 1,
		ScannedAt:      "2026-09-21 22:00:00",
	}
	_ = appendStepEvent(db, fID, scanStep, stOK, "扫描完成",
		detailJSON(map[string]any{"source": "ansible_scan_collectors", "scan": scan}), 0)
	status3, summary3, detail3 := r.atomConfirmUninstall(db)
	if status3 != stBlocked {
		t.Fatalf("扫描就绪后应停在人工确认（blocked），实际 %q", status3)
	}
	if waitingForKey(detail3) != "human_confirm_offboard" {
		t.Errorf("waiting_for 应为 human_confirm_offboard，实际 %q", waitingForKey(detail3))
	}
	for _, want := range []string{"采集插件", "确认", "2 个采集插件"} {
		if !strings.Contains(summary3, want) {
			t.Errorf("摘要应含 %q（要让人一眼看出会动到什么），实际 %q", want, summary3)
		}
	}
	// 确认面板必须有"会停什么 / 会删什么 / 会留什么"三份清单，缺一操作人员就无法判断
	for _, key := range []string{"will_stop", "will_remove", "will_keep", "plugins", "autostart"} {
		if !strings.Contains(detail3, "\""+key+"\"") {
			t.Errorf("确认详情缺少 %q 字段：%s", key, detail3)
		}
	}
	// 会停清单里必须点名会拦下 SAgent 重启的自愈守护，否则操作人员不知道"为什么还要清守护"
	if !strings.Contains(detail3, "自愈守护") {
		t.Errorf("will_stop 应明确指出 run.sh 守护会被终止：%s", detail3)
	}
	// 保留清单必须写明凭据与资源对象保留，否则没人敢点确认
	if !strings.Contains(detail3, "SSH 凭据") {
		t.Errorf("will_keep 应写明资源对象与 SSH 凭据保留：%s", detail3)
	}
}

// 零值自适应文案（2026-09-22 用户评审）：扫描结果为空时确认摘要必须说
// "卸载只影响 SAgent 本体"，不许再拼"继续卸载会连带停掉它们"——没东西可停
// 还吓唬人，操作人员对着自相矛盾的话不敢点确认
func TestConfirmUninstallZeroImpactSummary(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	steps := []storepkg.FlowStepSnapshot{
		{ID: "scan_collectors", Atom: "scan_collectors", Title: "扫描", Scope: "external"},
		{ID: "confirm_uninstall", Atom: "confirm_uninstall", Title: "确认", Scope: "platform", Requires: []string{"scan_collectors"}},
	}
	fID := makeFlow(t, db, "ip-1-1-1-2", steps)
	flow, _ := db.GetFlow(fID)
	if flow == nil {
		t.Fatal("GetFlow 返回空：流水线未落库")
	}
	r := &flowRun{catDB: db, flow: flow}
	scanStep := storepkg.FlowStepSnapshot{ID: "scan_collectors", Atom: "scan_collectors", Scope: "external"}
	// 实测 zero-shot 场景（flow37 phbomc156）：0 插件 + 0 进程 + 0 自愈来源
	scan := scanPayload{
		Plugins:        []scanPlugin{},
		Autostart:      []scanAutostart{{Kind: "run_sh", State: "absent", Hits: 0}},
		PluginsTotal:   0,
		ProcessCount:   0,
		AutostartCount: 0,
		CronUnverified: true,
		ScannedAt:      "2026-09-22 03:10:45",
	}
	_ = appendStepEvent(db, fID, scanStep, stOK, "扫描完成",
		detailJSON(map[string]any{"source": "ansible_scan_collectors", "scan": scan}), 0)
	_, summary, detail := r.atomConfirmUninstall(db)
	if !strings.Contains(summary, "只影响 SAgent 本体") {
		t.Errorf("零插件时摘要应说明卸载只影响本体，实际 %q", summary)
	}
	if strings.Contains(summary, "连带停掉") {
		t.Errorf("零插件时摘要不许再说'连带停掉它们'（没东西可停），实际 %q", summary)
	}
	// 待复核标记必须透传：crontab 未能核实不能被零值分支吞掉
	if waitingForKey(detail) != "human_confirm_offboard" {
		t.Errorf("waiting_for 应为 human_confirm_offboard，实际 %q", waitingForKey(detail))
	}
	if !strings.Contains(detail, "\"cron_unverified\": true") {
		t.Errorf("crontab 未核实状态应透传给前端决策卡：%s", detail)
	}
}

// 停等环节禁 force/retry（2026-09-22 用户评审）：waiting_for 非空的 blocked 步骤
// 没有任何"可跳过的工作"——前端已按 fail-only 出按钮，后端 handler 同步拒绝
// waiting 类步骤的 force/retry（读 LatestEvent 的 waiting_for 判定，本测试锁契约）
func TestWaitingBlockedWaitingKeyContract(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	steps := []storepkg.FlowStepSnapshot{
		{ID: "confirm_uninstall", Atom: "confirm_uninstall", Title: "确认", Scope: "platform"},
		{ID: "uninstall_agent", Atom: "uninstall_agent", Title: "卸载", Scope: "external", Requires: []string{"confirm_uninstall"}},
	}
	fID := makeFlow(t, db, "ip-1-1-1-3", steps)
	step := steps[1]
	// 下游步骤因上游停等而 blocked，事件 detail 带 waiting_for（handler 据此拒绝 force/retry）
	_ = appendStepEvent(db, fID, step, stBlocked, "前置环节待人工决策，本环节暂不执行",
		detailJSON(map[string]any{"waiting_for": "human_confirm_offboard"}), 0)
	last, _ := db.LatestEvent(fID, "uninstall_agent")
	if last == nil {
		t.Fatal("LatestEvent 返回空：事件未落库")
	}
	if wf := waitingForKey(last.Detail); wf != "human_confirm_offboard" {
		t.Errorf("waiting_for 应为 human_confirm_offboard，实际 %q", wf)
	}
	// 对照组：正常 blocked（门禁拦截，无 waiting_for）不受影响，仍可 force
	_ = appendStepEvent(db, fID, step, stBlocked, "前置环节未通过，本环节暂不执行",
		detailJSON(map[string]any{"blocked_by": []string{"confirm_uninstall"}}), 0)
	last2, _ := db.LatestEvent(fID, "uninstall_agent")
	if wf2 := waitingForKey(last2.Detail); wf2 != "" {
		t.Errorf("门禁类 blocked 不应带 waiting_for，实际 %q", wf2)
	}
}

// 步骤级超时覆盖（2026-09-21 五阶段评审）：self_metrics 与 observe_collect 共用
// observe_collect atom，但注册回报必须 180s 失败、采集入库按 600s 周期观察——
// 覆盖顺序 步骤ID > atom > default，错一层就会让 Agent 注册傻等 10 分钟
func TestTimeoutSpecStepOverridesAtom(t *testing.T) {
	cfg := testOnboardCfg(t)

	sm := timeoutSpecForStep(cfg, "self_metrics", "observe_collect")
	if sm.IdleSec != 180 {
		t.Errorf("self_metrics 应按步骤 ID 取 180s idle（注册回报分钟级），实际 %+v", sm)
	}
	oc := timeoutSpecForStep(cfg, "observe_collect", "observe_collect")
	if oc.IdleSec != 600 {
		t.Errorf("observe_collect 应按 atom 取 600s idle（采集观察周期），实际 %+v", oc)
	}
	// 未登记步骤仍落 default，不许 0 值穿透
	df := timeoutSpecForStep(cfg, "no_such_step", "")
	if df.IdleSec <= 0 || df.RunSec <= 0 {
		t.Errorf("未登记步骤应落 default 正值，实际 %+v", df)
	}
}

// parseFactsTree 资源信息反哺字段（2026-09-22 用户拍板：全量采集保持现状，结构化落库）：
// 门禁判定字段（system/machine/mem）之外，hostname/CPU/IP/虚拟化/硬件指纹必须进档案——
// 这些就是"资源信息反哺"的原料，漏一个字段台账档案就缺一块
func TestParseFactsTreeEnrichedFields(t *testing.T) {
	tree := t.TempDir()
	doc := map[string]any{
		"ansible_facts": map[string]any{
			"ansible_system":                 "Linux",
			"ansible_distribution":           "CentOS",
			"ansible_distribution_version":   "7.9",
			"ansible_machine":                "x86_64",
			"ansible_kernel":                 "5.4.237",
			"ansible_memtotal_mb":            float64(257407),
			"ansible_hostname":               "l0node",
			"ansible_fqdn":                   "l0node.bomc.local",
			"ansible_processor_vcpus":        float64(64),
			"ansible_processor":              []any{"64 x Intel Xeon", "Intel(R) Xeon(R) Gold 6248R"},
			"ansible_all_ipv4_addresses":     []any{"10.1.207.156", "172.17.0.1", "172.25.0.1"},
			"ansible_default_ipv4":           map[string]any{"address": "10.1.207.156", "interface": "bond0"},
			"ansible_virtualization_type":    "kvm",
			"ansible_virtualization_role":    "host",
			"ansible_system_vendor":          "SuperCloud",
			"ansible_product_name":           "X11DPi-N",
			"ansible_python_version":         "3.6.8",
			"ansible_uptime_seconds":         float64(86400),
			"ansible_mounts":                 []any{map[string]any{"mountpoint": "/", "size_available": float64(76585693184)}},
		},
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(tree, "l0node"), b, 0o644); err != nil {
		t.Fatalf("写 tree 夹具失败：%v", err)
	}
	fc, err := parseFactsTree(tree)
	if err != nil {
		t.Fatalf("parseFactsTree 失败：%v", err)
	}
	// 门禁判定字段不回归
	if fc.System != "Linux" || fc.MemTotalMB != 257407 || fc.DiskFreeGB <= 76 {
		t.Errorf("既有判定字段解析回归：system=%s mem=%v disk=%v", fc.System, fc.MemTotalMB, fc.DiskFreeGB)
	}
	// 反哺字段逐项断言
	if fc.Hostname != "l0node" || fc.FQDN != "l0node.bomc.local" {
		t.Errorf("hostname/fqdn 未采到：%s / %s", fc.Hostname, fc.FQDN)
	}
	if fc.CPUvCPUs != 64 || fc.CPUModel != "Intel(R) Xeon(R) Gold 6248R" {
		t.Errorf("CPU 档案字段未采到：vcpus=%d model=%q", fc.CPUvCPUs, fc.CPUModel)
	}
	if len(fc.IPv4s) != 3 || fc.DefaultIPv4 != "10.1.207.156" {
		t.Errorf("IP 档案字段未采到：%v default=%s", fc.IPv4s, fc.DefaultIPv4)
	}
	if fc.VirtType != "kvm" || fc.VirtRole != "host" || fc.Vendor != "SuperCloud" || fc.ProductName != "X11DPi-N" {
		t.Errorf("虚拟化/硬件指纹未采到：%s/%s %s/%s", fc.VirtType, fc.VirtRole, fc.Vendor, fc.ProductName)
	}
	if fc.PythonVersion != "3.6.8" || fc.UptimeSec != 86400 {
		t.Errorf("python/uptime 未采到：%s %v", fc.PythonVersion, fc.UptimeSec)
	}
}

// ---------- 第一批 A 级修正回归（2026-09-22 代码审查） ----------

// 隧道判活必须反映真实进程状态：原实现 Process.Signal(nil) 恒返回
// "unsupported signal type" → 恒判死 → keeper 每 30s 重复拉起一条新隧道（僵尸堆积）
func TestTunnelAliveReflectsProcessState(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动测试进程失败：%v", err)
	}
	liveTunnels.Store("t-alive", cmd)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		liveTunnels.Delete("t-alive")
	}()
	if !tunnelAlive("t-alive") {
		t.Errorf("运行中的隧道进程应判活（判死会让 keeper 每轮重建，隧道与僵尸无限增长）")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if tunnelAlive("t-alive") {
		t.Errorf("已退出的隧道进程不应判活（keeper 要据此重建）")
	}
}

// 凭据缺失（资源行被删/读库失败 → mustResource 返回 nil）不得空指针崩溃：
// 控制台全仓无 recover，后台 goroutine 一崩就是整进程退出
func TestCredTargetNilSafe(t *testing.T) {
	var c *sshCred
	if got := c.target(); got == "" {
		t.Errorf("nil 凭据的 target 必须给出可读占位（如「目标机未登记」），而不是空串")
	}
}

// 资源行缺失时清扫器仍要能处置超时：原实现在这一步 cred.target() 空指针 panic，
// 控制台进程整体退出（全链路停摆）
func TestSweepToleratesMissingResource(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	// 故意不登记资源行：模拟资源被删或台账缺失
	id, err := db.CreateFlow(&storepkg.Flow{
		ResourceID: "not-in-ledger", Mode: "edge", TemplateID: "edge",
		Steps:  []storepkg.FlowStepSnapshot{{ID: "install_agent", Atom: "install_agent", Title: "安装 SAgent", Scope: "external"}},
		Status: "running",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	spec := timeoutSpecFor(onboardCfg, "install_agent")
	past := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.AppendEvent(&storepkg.FlowEvent{
		FlowID: id, Step: "install_agent", Title: "安装 SAgent", Scope: "external", Status: stRunning,
		Summary: "平台 ansible 执行中", StartedAt: past, FinishedAt: past,
		Detail: detailJSON(map[string]any{
			"executor": "ansible", "runner": "platform",
			"progress": map[string]any{
				"attempt": 2, "max_attempts": spec.Retry + 1,
				"attempt_started_at": past, "last_activity_at": past, "timeout": spec,
			},
			"attempts": []attemptRec{{No: 1, Result: "timeout_run", WaitedSec: 900}},
		}),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	sweepOnboardTimeouts(db, nil) // 原实现此处 panic，测试进程直接挂

	ev, _ := db.LatestEvent(id, "install_agent")
	if ev == nil || ev.Status != stFail {
		t.Fatalf("额度用尽的超时仍应判死（资源缺失不能成为崩溃理由），实际 %+v", ev)
	}
}

// 隧道进程退出后必须自动从台账摘除并收尸：Start 之后无人 Wait 会留僵尸，
// 台账不摘则 keeper 永远认为隧道健在（或反过来反复重建）
func TestTunnelExitReapsAndRemovesEntry(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动测试进程失败：%v", err)
	}
	liveTunnels.Store("t-reap", cmd)
	go watchTunnelExit("t-reap", cmd)
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := liveTunnels.Load("t-reap"); !ok {
			if cmd.ProcessState == nil {
				t.Errorf("摘除台账前必须先 Wait 收尸（ProcessState 未落定）")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("隧道进程退出后应自动从台账摘除（现仍留在 liveTunnels，keeper 无法感知断线）")
}

// 清扫兜底：单步处置的意外 panic 不得带崩整轮清扫（控制台全仓无 recover，
// 后台 goroutine 一崩即整进程退出）；正常执行不得被吞
func TestRunGuardedRecoversPanic(t *testing.T) {
	runGuarded("测试步骤", func() { panic("boom") })
	ran := false
	runGuarded("测试步骤", func() { ran = true })
	if !ran {
		t.Errorf("兜底不得吞掉正常执行")
	}
}

// 版本候选排序必须按语义版本：字符串比较会得出 "0.9.0" > "0.10.0"，
// 界面默认选中的"最新版"会选错
func TestCompatibleVersionsSemverOrder(t *testing.T) {
	saved := versionCatalog
	defer func() { versionCatalog = saved }()
	versionCatalog = []SAVersion{
		{Tag: "v0.4.0", Version: "0.4.0", OS: "linux", Arch: "amd64", Status: "stable"},
		{Tag: "v0.9.0", Version: "0.9.0", OS: "linux", Arch: "amd64", Status: "stable"},
		{Tag: "v0.10.0", Version: "0.10.0", OS: "linux", Arch: "amd64", Status: "stable"},
		{Tag: "v0.11.0", Version: "0.11.0", OS: "linux", Arch: "amd64", Status: "deprecated"},
	}
	got := compatibleVersions("linux", "amd64")
	want := []string{"v0.10.0", "v0.9.0", "v0.4.0", "v0.11.0"}
	if len(got) != len(want) {
		t.Fatalf("候选数量应为 %d，实际 %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Tag != want[i] {
			tags := make([]string, 0, len(got))
			for _, v := range got {
				tags = append(tags, v.Tag)
			}
			t.Fatalf("候选顺序应为 stable 优先 + 版本新在前 %v，实际 %v", want, tags)
		}
	}
}

// ---------- B 级：硬编码清零回归（端口/家目录/路径必须跟随配置） ----------

// 渲染给目标机的 SAgent 配置必须用 AGENT_HTTP_PORT，不得残留写死端口
func TestRenderSagentConfigUsesConfiguredPort(t *testing.T) {
	saved := cfgAgentHTTPPort
	defer func() { cfgAgentHTTPPort = saved }()
	cfgAgentHTTPPort = "19099"
	got := renderSagentConfig("res-x", "http://l0:8080", false, "")
	if !strings.Contains(got, `listen: ":19099"`) {
		t.Errorf("listen 应取配置端口：\n%s", got)
	}
	if strings.Contains(got, "19090") {
		t.Errorf("渲染结果不得残留写死端口：\n%s", got)
	}
	if !strings.Contains(got, `id: "res-x"`) || !strings.Contains(got, `url: "http://l0:8080"`) {
		t.Errorf("占位符顺序错位（端口在最前）：\n%s", got)
	}
}

// 采集机（远程采集承载端）渲染必须落 type=proxy + 10s 短心跳（R1 身份 / R5 提速）；
// 普通资源维持 type=host + 30s——身份与心跳两处都不许串档。池归属仅采集机且非空才写（漂移边界）。
func TestRenderSagentConfigCollectorIdentity(t *testing.T) {
	edge := renderSagentConfig("res-edge", "http://l0:8080", false, "")
	if !strings.Contains(edge, `type: "host"`) || !strings.Contains(edge, "heartbeat_interval: 30s") {
		t.Errorf("普通资源应为 host + 30s：\n%s", edge)
	}
	col := renderSagentConfig("sagent-proxy", "http://l0:8080", true, "cn-east")
	if !strings.Contains(col, `type: "proxy"`) {
		t.Errorf("采集机应落 type=proxy（决定漂移候选资格）：\n%s", col)
	}
	if !strings.Contains(col, "heartbeat_interval: 10s") {
		t.Errorf("采集机应落 10s 短心跳：\n%s", col)
	}
	if !strings.Contains(col, `pool: "cn-east"`) || !strings.Contains(col, `region: "cn-east"`) {
		t.Errorf("采集机应落池归属 pool/region（漂移边界）：\n%s", col)
	}
	// 池为空时不得写入空池（否则平台会误认为"已登记空池"）
	if noPool := renderSagentConfig("sagent-proxy", "http://l0:8080", true, ""); strings.Contains(noPool, "pool:") {
		t.Errorf("池未登记时不得渲染 pool 行：\n%s", noPool)
	}
}

// 安装/卸载/启停的家目录必须同源于 AGENT_HOME_ROOT（root 等自定义家目录场景）
func TestDestHomeForUsesConfiguredRoot(t *testing.T) {
	saved := cfgAgentHomeRoot
	defer func() { cfgAgentHomeRoot = saved }()
	cfgAgentHomeRoot = "/opt/agents/"
	if got := destHomeFor(&sshCred{User: "ibomc"}); got != "/opt/agents/ibomc" {
		t.Errorf("家目录根应取配置并去尾斜杠：%q", got)
	}
	if got := destHomeFor(nil); got != "/opt/agents/" {
		t.Errorf("nil 凭据不应崩且给出根目录：%q", got)
	}
}

// 隧道两侧端口都来自配置：目标机侧 TUNNEL_REMOTE_PORT、平台侧 LISTEN_ADDR
func TestReverseTunnelCmdUsesConfiguredPorts(t *testing.T) {
	savedRemote, savedListen := cfgTunnelRemotePort, cfgListenAddr
	defer func() { cfgTunnelRemotePort, cfgListenAddr = savedRemote, savedListen }()
	cfgTunnelRemotePort, cfgListenAddr = 28080, ":9090"
	args := strings.Join(reverseTunnelCmd(&sshCred{Host: "h", Port: 22, User: "u", Pass: "p"}).Args, " ")
	if !strings.Contains(args, "-R 28080:127.0.0.1:9090") {
		t.Errorf("隧道两侧端口应来自配置：%s", args)
	}
	if !isTunnelAccessPoint("http://127.0.0.1:28080") {
		t.Errorf("隧道接入点识别应跟随 TUNNEL_REMOTE_PORT")
	}
	if isTunnelAccessPoint("http://127.0.0.1:18080") {
		t.Errorf("改了配置端口后，旧端口不应再被识别为隧道接入点")
	}
}

// 平台端口从 LISTEN_ADDR 解析（":8080" / "0.0.0.0:8080" / 非法值兜底）
func TestPlatformPortFromListenAddr(t *testing.T) {
	saved := cfgListenAddr
	defer func() { cfgListenAddr = saved }()
	cases := map[string]int{":9090": 9090, "0.0.0.0:8081": 8081, "127.0.0.1:18080": 18080, "garbage": 8080, "": 8080}
	for addr, want := range cases {
		cfgListenAddr = addr
		if got := platformPort(); got != want {
			t.Errorf("LISTEN_ADDR=%q 应解析出 %d，实际 %d", addr, want, got)
		}
	}
}

// 卸载 playbook（外置文件与内置模板二选一）的端口核对必须渲染配置端口，
// 且字段名不再把端口号写进字段名
func TestUninstallPlaybookPortPlaceholderRendered(t *testing.T) {
	saved := cfgAgentHTTPPort
	defer func() { cfgAgentHTTPPort = saved }()
	cfgAgentHTTPPort = "19099"
	pb := loadUninstallPlaybook("/home/u")
	if strings.Contains(pb, "@AGENT_PORT@") {
		t.Fatalf("占位符未渲染：\n%s", pb)
	}
	if !strings.Contains(pb, "grep -q ':19099'") {
		t.Errorf("端口核对必须用配置端口（找不到 grep -q ':19099'）")
	}
	if !strings.Contains(pb, "port_agent_listening=") {
		t.Errorf("核对字段应改名 port_agent_listening（不把端口号写进字段名）")
	}
}

// 版本清单改为"yaml 种子 → 目录库权威"：种子补入未登记行、不覆盖界面改过的行；
// 内存缓存从库里读回（编辑后即时生效）
func TestVersionCatalogSeedsIntoCatalog(t *testing.T) {
	saved := versionCatalog
	defer func() { versionCatalog = saved }()
	db := testOnboardDB(t)
	seed := filepath.Join(t.TempDir(), "versions.yaml")
	body := "versions:\n  - tag: v-seed-1\n    version: \"0.4.0\"\n    os: linux\n    arch: amd64\n" +
		"    status: stable\n    min_kernel: \"3.10\"\n"
	if err := os.WriteFile(seed, []byte(body), 0o644); err != nil {
		t.Fatalf("写种子失败：%v", err)
	}
	loadVersionCatalog(seed, db)
	if len(versionCatalog) != 1 || versionCatalog[0].Tag != "v-seed-1" || versionCatalog[0].MinKernel != "3.10" {
		t.Fatalf("种子应导入并读回内存缓存，实际 %+v", versionCatalog)
	}
	if err := db.UpsertVersion(storepkg.VersionRow{Tag: "v-seed-1", Version: "0.4.0", Arch: "amd64",
		OS: "linux", Status: "stable", MinKernel: "4.19"}, "ui"); err != nil {
		t.Fatalf("upsert ui: %v", err)
	}
	loadVersionCatalog(seed, db)
	if versionCatalog[0].MinKernel != "4.19" {
		t.Fatalf("人工改动应保留，实际 %+v", versionCatalog[0])
	}
}
