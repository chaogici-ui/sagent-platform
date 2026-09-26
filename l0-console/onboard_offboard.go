package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  卸载前置检查链：扫描采集物 → 人工确认 → 卸采集插件 → 清自愈自启 → 卸 SAgent
//
//  为什么要多这四步：现实里的主机不是"只有 SAgent 一个进程"。
//  它可能挂着若干采集插件（有的还是独立子进程）、可能被 run.sh 守护着、
//  可能有一条 crontab 定时把它拉起来。直接删安装目录的后果是：
//    插件子进程变孤儿继续采集、守护把 SAgent 重新拉起来、定时任务再拉一次 ——
//  运维看到的是"卸了又活"，会以为卸载功能坏了。
//  所以顺序固定为：先说清楚会停掉什么、人同意了，再按依赖反序拆。
//
//  这里的每一步都是可观测流水线的一环：有状态、有证据、有分级诊断，
//  失败必须暴露成失败，不许"看着成功"。
// ===================================================================

//go:embed data/playbooks/scan_collectors.yml data/playbooks/uninstall_plugins.yml data/playbooks/cleanup_autostart.yml
var playbookSeedFS embed.FS

// loadSeedPlaybook 渲染兜底 playbook：优先磁盘上的外置文件（改卸载动作不动代码），
// 外置文件缺失或被改坏时用编译进来的副本。
// 与安装/卸载两份早期的"内外置各写一遍、靠单测钉同步"不同：这里的兜底副本
// 直接来自同一份文件（go:embed 在构建期读取），结构上不可能走样。
func loadSeedPlaybook(name, destHome string) string {
	tpl := ""
	if b, err := os.ReadFile(filepath.Join("data", "playbooks", name)); err == nil &&
		strings.Contains(string(b), "@DEST_HOME@") {
		tpl = string(b)
	} else if b, err := playbookSeedFS.ReadFile("data/playbooks/" + name); err == nil {
		tpl = string(b)
	}
	if tpl == "" {
		return ""
	}
	return strings.NewReplacer("@DEST_HOME@", destHome).Replace(tpl)
}

// ===================================================================
//  扫描结果的解析与结构（输出契约见 data/playbooks/scan_collectors.yml 头部）
// ===================================================================

// scanPlugin 目标机上扫描到的一个采集插件
type scanPlugin struct {
	Name    string `json:"name"`
	Form    string `json:"form"`    // builtin / subprocess / exec / unknown
	Source  string `json:"source"`  // config（配置里启用）/ dir（plugins 目录里实存）
	Enabled bool   `json:"enabled"` // 配置里是否 enabled
	Process string `json:"process"` // yes / no / unknown（是否有独立进程在跑）
	Path    string `json:"path"`    // 相对安装目录的产物路径，无则为 -
}

// scanAutostart 目标机上扫描到的一处自启 / 自愈来源
type scanAutostart struct {
	Kind  string `json:"kind"`  // run_sh / cron_user / cron_system
	State string `json:"state"` // running / present / absent / unknown
	Hits  int    `json:"hits"`
	Note  string `json:"note"`
	PID   string `json:"pid,omitempty"`
}

// scanPayload 扫描步骤的结论，随成功事件落库供确认环节与界面读取
type scanPayload struct {
	Plugins        []scanPlugin    `json:"plugins"`
	Autostart      []scanAutostart `json:"autostart"`
	PluginsTotal   int             `json:"plugins_total"`
	ProcessCount   int             `json:"process_count"`
	AutostartCount int             `json:"autostart_count"`
	CronUnverified bool            `json:"cron_unverified"`
	ScannedAt      string          `json:"scanned_at"`
}

// taskOutputLines 汇总一次 ansible 运行里所有任务的输出行。
// 三处都要取：shell 任务的 stdout 落在 task.Stdout，debug 任务的 msg 落在 task.Msg，
// 被截断/格式异常时的原始行在 task.Lines —— 只取一处会漏，而扫描契约要求逐行可解析
//
// 但**必须滤掉 ansible 自己的框线行**（TASK [...] / PLAY [...] / PLAY RECAP / handler）。
// 那几行里装的是任务名，而任务名是人写的散文 —— 散文里提一句判据标记
// （曾经真的这么写："断言…（残余只告警不阻断，见 AUTOSTART_PARTIAL）"）
// 就会被当成目标机回报的结论，把 CLEAN 判成 PARTIAL。
// 判据只能来自任务的输出体，不能来自我们自己的标题
func taskOutputLines(r ansibleRunResult) []string {
	var out []string
	for _, t := range r.Tasks {
		for _, s := range []string{t.Stdout, t.Msg} {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.Split(s, "\n")...)
			}
		}
		for _, ln := range t.Lines {
			if isAnsibleFramingLine(ln) {
				continue
			}
			out = append(out, ln)
		}
	}
	return out
}

// isAnsibleFramingLine 判断是否 ansible 输出的框线（只管"任务名/播放名"这几类）。
// 刻意只排除框线：结果行（ok:/changed:/fatal: …）不携带任务名，留着无害
func isAnsibleFramingLine(ln string) bool {
	s := strings.TrimSpace(ln)
	for _, p := range []string{"TASK [", "PLAY [", "PLAY RECAP", "RUNNING HANDLER ["} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// parseKV 解析 "k=v k2=v2" 形式的行。
// 值里不含空格由 playbook 输出契约保证（run_sh 的 pid 用逗号连接正为此）
func parseKV(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Fields(s) {
		if i := strings.Index(f, "="); i > 0 {
			m[f[:i]] = f[i+1:]
		}
	}
	return m
}

// parseScanOutput 从扫描 playbook 的输出里还原结构化结论
func parseScanOutput(r ansibleRunResult) scanPayload {
	out := scanPayload{Plugins: []scanPlugin{}, Autostart: []scanAutostart{}}
	for _, raw := range taskOutputLines(r) {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "PLUGIN "):
			kv := parseKV(strings.TrimPrefix(line, "PLUGIN "))
			if kv["name"] == "" {
				continue
			}
			out.Plugins = append(out.Plugins, scanPlugin{
				Name: kv["name"], Form: kv["form"], Source: kv["source"],
				Enabled: kv["enabled"] == "yes", Process: kv["process"], Path: kv["path"],
			})
		case strings.HasPrefix(line, "AUTOSTART "):
			kv := parseKV(strings.TrimPrefix(line, "AUTOSTART "))
			if kv["kind"] == "" {
				continue
			}
			hits, _ := strconv.Atoi(kv["hits"])
			out.Autostart = append(out.Autostart, scanAutostart{
				Kind: kv["kind"], State: kv["state"], Hits: hits, Note: kv["note"], PID: kv["pid"],
			})
		case strings.HasPrefix(line, "SCAN "):
			kv := parseKV(strings.TrimPrefix(line, "SCAN "))
			out.PluginsTotal, _ = strconv.Atoi(kv["plugins_total"])
			out.ProcessCount, _ = strconv.Atoi(kv["plugins_process"])
			out.AutostartCount, _ = strconv.Atoi(kv["autostart"])
		}
	}
	out.ScannedAt = time.Now().Format("2006-01-02 15:04:05")
	return out
}

// ===================================================================
//  人工确认原子（scope=platform）
// ===================================================================

// atomConfirmUninstall 卸载前的人工确认卡点。
//
// 它不是"点一下就过"的装饰：确认内容 = 上一步扫描出来的实测清单，
// 平台把「会删什么 / 会留什么」摊开给操作人员看，人点头之后才继续。
// 停等语义沿用 pick_version 那一套（blocked + detail.waiting_for），
// 由 /api/onboard/flow/offboard/confirm 放行。
func (r *flowRun) atomConfirmUninstall(catDB *storepkg.DB) (string, string, string) {
	last, _ := catDB.LatestEvent(r.flow.ID, "scan_collectors")
	if last == nil || last.Status != stOK {
		// 扫描没成，就没有可确认的内容。不许拿"默认认为没有插件"顶上去 ——
		// 确认的意义全在于它基于实测，凭空的确认等于没确认
		st := "无事件"
		if last != nil {
			st = last.Status
		}
		return stBlocked, "等待采集物扫描完成后再确认（当前扫描环节：" + st + "）",
			detailJSON(map[string]any{
				"waiting_for": "scan",
				"note":        "确认环节依赖扫描的实测结果，扫描未完成前没有可确认的清单",
			})
	}
	var d struct {
		Scan *scanPayload `json:"scan"`
	}
	_ = json.Unmarshal([]byte(last.Detail), &d)
	scan := d.Scan
	if scan == nil {
		// 扫描事件在、结论不在：数据契约被破坏。宁可阻断也不能让人对着一份空清单点确认
		return stFail, "扫描环节缺少结构化结论（detail.scan 缺失），无法呈报影响范围",
			detailJSON(map[string]any{"scan_missing": true, "source": "scan_collectors"})
	}

	enabled, withProc := 0, 0
	for _, p := range scan.Plugins {
		if p.Enabled {
			enabled++
		}
		if p.Process == "yes" {
			withProc++
		}
	}
	liveAuto := []string{}
	for _, a := range scan.Autostart {
		if a.State == "running" || a.State == "present" {
			liveAuto = append(liveAuto, autostartKindLabel(a.Kind))
		}
	}

	// 零值自适应文案（2026-09-22 用户评审）：0 插件 + 0 自愈来源时说"会连带停掉它们"
	// 是自相矛盾——没东西可停还吓唬人。此时把话说实：卸载只影响 SAgent 本体
	if scan.PluginsTotal == 0 && enabled == 0 && withProc == 0 && len(liveAuto) == 0 {
		return stBlocked, "未发现采集插件与自愈来源：卸载只影响 SAgent 本体，请人工确认", detailJSON(map[string]any{
			"waiting_for":      "human_confirm_offboard",
			"resource_id":      r.flow.ResourceID,
			"ip":               r.flow.ResourceIP,
			"scan_at":          scan.ScannedAt,
			"plugins":          scan.Plugins,
			"autostart":        scan.Autostart,
			"plugins_total":    scan.PluginsTotal,
			"plugins_enabled":  enabled,
			"process_count":    withProc,
			"autostart_count":  scan.AutostartCount,
			"cron_unverified":  scan.CronUnverified,
			"will_stop":        willStopTexts(scan),
			"will_remove":      willRemoveTexts(),
			"will_keep":        willKeepTexts(),
			"approve_endpoint": "/api/onboard/flow/offboard/confirm",
		})
	}
	summary := fmt.Sprintf("扫描到 %d 个采集插件（配置启用 %d 个，含独立进程 %d 个）", scan.PluginsTotal, enabled, withProc)
	if len(liveAuto) > 0 {
		summary += "、" + fmt.Sprint(len(liveAuto)) + " 处自愈/自启"
	}
	summary += " —— 继续卸载会连带停掉它们，请人工确认"

	return stBlocked, summary, detailJSON(map[string]any{
		"waiting_for":      "human_confirm_offboard",
		"resource_id":      r.flow.ResourceID,
		"ip":               r.flow.ResourceIP,
		"scan_at":          scan.ScannedAt,
		"plugins":          scan.Plugins,
		"autostart":        scan.Autostart,
		"plugins_total":    scan.PluginsTotal,
		"plugins_enabled":  enabled,
		"process_count":    withProc,
		"autostart_count":  scan.AutostartCount,
		"cron_unverified":  scan.CronUnverified,
		"will_stop":        willStopTexts(scan),
		"will_remove":      willRemoveTexts(),
		"will_keep":        willKeepTexts(),
		"approve_endpoint": "/api/onboard/flow/offboard/confirm",
	})
}

// willStopTexts 明确告诉操作人员"点同意之后哪些东西会停"
func willStopTexts(scan *scanPayload) []string {
	out := []string{}
	for _, p := range scan.Plugins {
		if !p.Enabled && p.Process != "yes" {
			continue
		}
		switch p.Form {
		case "subprocess":
			out = append(out, "插件 "+p.Name+"：停止其独立进程（"+p.Path+"）")
		case "exec":
			out = append(out, "插件 "+p.Name+"：停止脚本采集（"+p.Path+"）")
		case "builtin":
			out = append(out, "插件 "+p.Name+"：随 SAgent 进程一起退出（进程内建，无独立进程）")
		default:
			out = append(out, "插件 "+p.Name+"：停止其采集进程")
		}
	}
	for _, a := range scan.Autostart {
		if a.State != "running" && a.State != "present" {
			continue
		}
		switch a.Kind {
		case "run_sh":
			out = append(out, "自愈守护 run.sh：终止守护进程并删除守护脚本（它会在 SAgent 退出后 3 秒把它拉起来）")
		case "cron_user":
			out = append(out, fmt.Sprintf("用户 crontab：删除 %d 条指向 SAgent 的定时任务", a.Hits))
		case "cron_system":
			out = append(out, fmt.Sprintf("系统级 crontab：发现 %d 处指向 SAgent 的条目（本次约定的清理范围不含系统级文件，需人工处理）", a.Hits))
		}
	}
	if len(out) == 0 {
		out = append(out, "目标机上未发现启用的采集插件与自愈来源：卸载只影响 SAgent 本体")
	}
	return out
}

func willRemoveTexts() []string {
	return []string{
		"目标机：SAgent 进程、安装目录（含 conf / data / logs / plugins）、自愈守护 run.sh",
		"目标机：用户 crontab 中指向 SAgent 的条目（其余条目一律不动）",
		"平台：Agent 台账、期望配置、vmagent 抓取登记、该资源的采集目标",
	}
}

func willKeepTexts() []string {
	return []string{
		"资源对象本身与 SSH 凭据：卸载后可原样重新接入，无需重新登记",
		"系统级 crontab / systemd unit 等本平台不管理的系统配置",
	}
}

func autostartKindLabel(kind string) string {
	switch kind {
	case "run_sh":
		return "自愈守护 run.sh"
	case "cron_user":
		return "用户 crontab"
	case "cron_system":
		return "系统级 crontab"
	}
	return kind
}

// ===================================================================
//  卸载侧「平台代执行」作业：一个骨架 + 每步各自的判据
// ===================================================================

// offboardStepSpec 卸载侧步骤之间的差异点。
// 骨架（语法门禁 → 流式执行 → 逐任务回报 → 汇总证据）完全相同，
// 差别只有：跑哪份 playbook、拿什么标记判成功、失败怎么归因。
// 判据分叉，骨架不分叉 —— 避免三份几乎一样的作业函数各自漂移
type offboardStepSpec struct {
	Playbook  string // data/playbooks/<name>
	Title     string // 中文名，用于摘要与审计
	Source    string // 事件 source（供前端与审计鉴别来源）
	Audit     string // 审计动作名
	CheckLine string // 核对行包含的子串（取出来原样回传界面）
	FailCode  string // 失败诊断 code
	FailHint  string // 失败时给操作人员的下一步
	// Verdict 判据裁定：rc==0 之后由它决定"这次算不算成功"。
	// 约定：返回的诊断里出现 fatal 即失败（用其 Message 作失败摘要），否则成功
	Verdict func(pr ansibleRunResult) (extra map[string]any, ds []diag)
}

// firstFatal 取第一条 fatal 诊断（无则返回 nil）
func firstFatal(ds []diag) *diag {
	for i := range ds {
		if ds[i].Level == "fatal" {
			return &ds[i]
		}
	}
	return nil
}

// hasOutMarker 输出行里是否出现过某个标记（判定 playbook 自证结论用）
func hasOutMarker(r ansibleRunResult, marker string) bool {
	if marker == "" {
		return false
	}
	for _, ln := range taskOutputLines(r) {
		if strings.Contains(ln, marker) {
			return true
		}
	}
	return false
}

// outCheckLine 取核对行（含 marker 的那一行），取不到返回空串——不编造核对结论
func outCheckLine(r ansibleRunResult, marker string) string {
	if marker == "" {
		return ""
	}
	for _, ln := range taskOutputLines(r) {
		if strings.Contains(ln, marker) {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}

// ansibleOffboardJob 卸载侧步骤的统一作业实现。
//
// 与安装/卸载作业同构：语法门禁（不碰目标机）→ 流式执行（逐任务回报）→
// 终态带逐任务结果、PLAY RECAP、阶段记录与全量原文。
// "成功"的定义更严：必须由目标机自己核对（playbook 里的断言）通过，
// 平台不替它圆场——核对行拿不到就报出来，不假装干净。
func ansibleOffboardJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred, sp offboardStepSpec) {

	st := newStepRunState(catDB, flowID, step, cred)
	defer st.startHeartbeat(catDB)()
	myAttempt := st.attempt
	hook := st.taskHook(catDB)

	fail := func(summary string, detail map[string]any, ds []diag) {
		if !jobStillCurrent(catDB, flowID, step.ID, myAttempt) {
			return
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["attempt"] = myAttempt
		// attempts 已由收口函数（offboardFinish）写入时不再追加：同一次尝试在历史里只应有一行。
		// 早退路径（流水线/资源/playbook/临时目录/语法门禁）没经过收口，这里兜底补一条
		if _, ok := detail["attempts"]; !ok {
			detail["attempts"] = st.endAttempt("fail", summary)
		}
		detail["tasks"] = compactTasks(st.tasksSnapshot())
		failStep(catDB, agentStore, flowID, step, "ansible "+sp.Title+"失败", summary, detail, cred, ds)
	}

	flow, _ := catDB.GetFlow(flowID)
	if flow == nil {
		fail("流水线不存在", nil, []diag{fatalDiag("flow_not_found", "流水线不存在", "")})
		return
	}
	res, _ := catDB.GetResource(flow.ResourceID)
	if res == nil {
		fail("资源对象不存在", nil, []diag{fatalDiag("resource_not_found", "资源对象不存在", "检查资源台账")})
		return
	}

	// 家目录口径与安装/卸载完全一致（/home/<ssh_user>）：装在哪就动哪，
	// 不允许两侧各写一套推导逻辑，否则会"看着成功其实什么都没碰到"
	destHome := destHomeFor(cred)
	pbBody := loadSeedPlaybook(sp.Playbook, destHome)
	if strings.TrimSpace(pbBody) == "" {
		fail("生成 "+sp.Title+" playbook 失败：外置文件与内置副本都取不到",
			nil, []diag{fatalDiag("playbook_missing",
				"取不到 "+sp.Playbook+" 的内容（外置文件缺失且内置副本为空）",
				"检查 data/playbooks/"+sp.Playbook+" 是否存在；该文件损坏时平台会回落内置副本")})
		return
	}
	work, err := os.MkdirTemp("", "l0-offboard-*")
	if err != nil {
		fail("创建临时目录失败："+err.Error(), nil,
			[]diag{fatalDiag("offboard_tmp_failed", "创建临时目录失败："+err.Error(), "检查平台容器内 /tmp 是否可写")})
		return
	}
	defer os.RemoveAll(work)
	pbPath := filepath.Join(work, sp.Playbook)
	if err := os.WriteFile(pbPath, []byte(pbBody), 0o644); err != nil {
		fail("生成 playbook 失败："+err.Error(), nil,
			[]diag{fatalDiag("playbook_render_failed", "生成 playbook 失败："+err.Error(), "")})
		return
	}
	inv, cleanupInv, err := writeInventory(cred)
	if err != nil {
		fail("生成 ansible inventory 失败："+err.Error(), nil,
			[]diag{fatalDiag("inventory_write_failed", "生成 ansible inventory 失败："+err.Error(),
				"检查平台容器内 /tmp 是否可写")})
		return
	}
	defer cleanupInv()

	phases := []map[string]any{}

	// 语法门禁：语法错误在碰目标机前就拦下（与安装/卸载同一原则）
	st.setPhase("syntax")
	st.flush(catDB, "平台 ansible 启动 · "+sp.Title+" playbook 语法校验中（尚未触碰目标机）")
	synCmd := exec.Command("ansible-playbook", "-i", inv, pbPath, "--syntax-check")
	synCmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False")
	synOut, synErr := synCmd.CombinedOutput()
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synErr != nil],
		"output": clip(string(synOut), 1500),
	})
	if synErr != nil {
		fail(sp.Title+" playbook 语法校验未过（未触碰目标机）", map[string]any{
			"syntax_output": clip(string(synOut), 2000), "phases": phases,
		}, []diag{fatalDiag(sp.FailCode+"_syntax", sp.Title+" playbook 语法校验未过：检查 data/playbooks/"+sp.Playbook,
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")})
		return
	}

	// 流式执行：每个 TASK 落一次进度事件，界面实时可见
	st.setPhase("playbook")
	st.flush(catDB, "平台 ansible 执行中 · "+sp.Title+" 开始（逐任务回报）")
	pbBudget := execBudget(st.spec, budgetPlaybook)
	ctx, cancel := context.WithTimeout(context.Background(), pbBudget)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, hook)
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})

	// L0 收口：与 L1 回执对账桥共用同一份实现（SPEC-D5-IN5 B4）
	offboardFinish(catDB, agentStore, flowID, step, cred, sp, pr, phases, destHome,
		myAttempt, pbBudget, st.endAttempt, st.setPhase, fail, nil)
}

// offboardSpecForKind 卸载侧 kind → 步骤定义（投递端与对账端共用一份映射，
// 避免两处各写一套"kind 对哪个 playbook / 哪条判据"，一处漏改就出现"投递 A、裁定 B"）。
func offboardSpecForKind(kind string) *offboardStepSpec {
	switch kind {
	case "scan_collectors":
		return &offboardScanSpec
	case "uninstall_plugins":
		return &offboardPluginsSpec
	case "cleanup_autostart":
		return &offboardAutostartSpec
	}
	return nil
}

// offboardFinish 卸载侧步骤的 L0 收口：判定成败 → 写步骤终态 → 审计。
//
// 为什么抽出来（SPEC-D5-IN5 B4）：执行端搬到 L1 后，步骤终态改由回执对账桥驱动，
// 但"这次算不算成功"的判据只能有一套——否则切了开关结论就变了。
// 两条路径的差别只有"尝试历史从哪来"（进程内 st.endAttempt / 桥从 running 事件承接）
// 与"要不要补记通道元信息"（seed），故用回调 + seed 注入；
// pbBudget 只用于超时文案，必须与执行端拿到的是同一个数。
func offboardFinish(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred, sp offboardStepSpec,
	pr ansibleRunResult, phases []map[string]any, destHome string, myAttempt int,
	pbBudget time.Duration,
	endAttempt func(result, note string) []attemptRec,
	setPhase func(string),
	fail func(summary string, detail map[string]any, ds []diag),
	seed map[string]any) {

	detail := map[string]any{
		"dest_home": destHome, "conn": credState(cred), "target": cred.target(),
		"rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases":     phases, "attempt": myAttempt,
		"evidence": execEvidence(pr),
	}
	if pr.TimedOut {
		detail["attempts"] = endAttempt("timeout_run", fmt.Sprintf("playbook 超过 %ds 未返回", budgetSec(pbBudget)))
		fail(fmt.Sprintf("%s playbook 超过 %ds 未返回（平台已终止）", sp.Title, budgetSec(pbBudget)), detail,
			withPortSource(cred, []diag{fatalDiag("ansible_phase_timeout",
				fmt.Sprintf("%s playbook 超过 %ds 未返回", sp.Title, budgetSec(pbBudget)),
				"目标机负载过高或磁盘卡住；核对目标机后点「↻ 重试该步」")}))
		return
	}
	if pr.RC != 0 {
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		d := classifyOffboardFailure(failText, sp)
		detail["attempts"] = endAttempt("fail", d.Message)
		fail(sp.Title+"失败："+d.Message+"（rc="+fmt.Sprint(pr.RC)+"）", detail,
			withPortSource(cred, []diag{d}))
		return
	}

	// 收口：由步骤自己的判据裁定成败（playbook 自证结论 + 目标机核对行）
	setPhase("verify")
	extra, ds := sp.Verdict(pr)
	if f := firstFatal(ds); f != nil {
		detail["verify_line"] = outCheckLine(pr, sp.CheckLine)
		detail["attempts"] = endAttempt("fail", f.Message)
		fail(f.Message, detail, withPortSource(cred, ds))
		return
	}

	checkLine := outCheckLine(pr, sp.CheckLine)
	detail["verify_line"] = checkLine
	if checkLine == "" && sp.CheckLine != "" {
		ds = append(ds, warnDiag("verify_line_missing",
			"未从 playbook 输出中取到目标机核对行（断言已通过，但核对数值不可见）",
			"确认 data/playbooks/"+sp.Playbook+" 的核对任务未被删改"))
	}
	reason := fmt.Sprintf("ansible %s完成 目标=%s 家目录=%s", sp.Title, cred.target(), destHome)
	if checkLine != "" {
		reason += " 核对=" + checkLine
	}
	if n := countWarn(ds); n > 0 {
		reason += fmt.Sprintf(" ⚠ %d 项待复核（见步骤告警）", n)
	}
	attempts := endAttempt("ok", reason)

	outExtra := map[string]any{}
	// seed 先落：桥侧补记的通道元信息（l1_task_id / exec_channel 等）不在下面的计算字段里，
	// 先铺底再由计算字段覆盖，保证 seed 永远盖不掉"谁执行、结论是什么"这类权威字段
	for k, v := range seed {
		outExtra[k] = v
	}
	outExtra["source"] = sp.Source
	outExtra["detail"] = detail
	outExtra["conn"] = credState(cred)
	outExtra["target"] = cred.target()
	outExtra["tasks"] = compactTasks(pr.Tasks)
	outExtra["recap"] = pr.Recap
	outExtra["task_total"] = len(pr.Tasks)
	outExtra["recap_line"] = recapLine(pr.Recap, pr.RecapSeq)
	// phases 必须平铺在顶层：界面按 dd.phases 取，藏在 detail 里等于没回传
	outExtra["phases"] = phases
	outExtra["attempt"] = myAttempt
	outExtra["attempts"] = attempts
	outExtra["verify_line"] = checkLine
	outExtra["diagnosis"] = diagJSON(ds)
	outExtra["evidence"] = execEvidence(pr)
	for k, v := range extra {
		outExtra[k] = v
	}
	if msg := autoReportStep(catDB, agentStore, flowID, step.ID, reason, outExtra); msg != "" {
		fail(sp.Title+"回报被拒："+msg, detail,
			withPortSource(cred, []diag{fatalDiag(sp.FailCode+"_report_rejected", sp.Title+"回报被拒："+msg,
				"步骤可能已被人工重置；点「↻ 重试该步」重新执行")}))
		return
	}
	resourceID := ""
	if flow, _ := catDB.GetFlow(flowID); flow != nil {
		resourceID = flow.ResourceID
	}
	addAudit(sp.Audit, resourceID, "接入中心", reason)
}

// classifyOffboardFailure 卸载侧步骤的失败归因：第一判据永远是"是否真的连上了"。
// 连不上时的处置动作（查网络/凭据）与"跑完了但没清干净"（上机排查残留）完全不同，
// 归错会把人带偏
func classifyOffboardFailure(out string, sp offboardStepSpec) diag {
	low := strings.ToLower(out)
	if strings.Contains(low, "unreachable!") || strings.Contains(low, "failed to connect to the host via ssh") {
		return classifySSHFailure(out, 2)
	}
	if strings.Contains(strings.ToUpper(out), strings.ToUpper(strings.TrimPrefix(sp.FailCode, "offboard_"))+"_DIRTY") {
		return fatalDiag(sp.FailCode+"_incomplete",
			sp.Title+"未完成：目标机核对未通过", sp.FailHint)
	}
	return fatalDiag(sp.FailCode+"_failed",
		sp.Title+" playbook 执行失败（rc≠0）", sp.FailHint)
}

// ===================================================================
//  三个步骤各自的作业入口与判据
// ===================================================================

// offboardScanSpec 扫描步骤：只读，成功判据是"拿到了可解析的汇总行"。
// 拿不到汇总行必须判失败 —— 否则会以"扫到 0 个"收场，
// 那是最危险的一种谎报：运维据此以为主机上什么都没有
var offboardScanSpec = offboardStepSpec{
	Playbook:  "scan_collectors.yml",
	Title:     "扫描采集物",
	Source:    "ansible_scan_collectors",
	Audit:     "ansible 扫描采集物（只读）",
	CheckLine: "SCAN plugins_total=",
	FailCode:  "scan_collectors",
	FailHint:  "该步骤只读，失败不会改动目标机；确认目标机可连、可执行 shell 后点「↻ 重试该步」",
	Verdict: func(pr ansibleRunResult) (map[string]any, []diag) {
		scan := parseScanOutput(pr)
		// 判据锚在汇总行上，不锚在计数上：干净主机与"输出没解析出来"都会是 0 个，
		// 只有汇总行在，才证明 playbook 真的跑完了它的输出契约。
		// 缺了汇总行就 fatal —— 默认"一个都没有"是最危险的一种谎报
		if !hasOutMarker(pr, "SCAN plugins_total=") {
			return map[string]any{"scan": scan}, []diag{fatalDiag("scan_output_unparsable",
				"扫描输出里没有可解析的结论行（SCAN/PLUGIN/AUTOSTART），无法呈报影响范围",
				"确认 data/playbooks/scan_collectors.yml 的输出契约未被改动")}
		}
		ds := []diag{}
		for _, a := range scan.Autostart {
			switch {
			case a.Kind == "cron_user" && a.State == "unknown":
				scan.CronUnverified = true
				ds = append(ds, warnDiag("cron_unverified",
					"用户 crontab 读不到（可能是 crontab 命令非 suid，或权限不足）：无法确认是否残留指向 SAgent 的定时任务",
					"在目标机上以安装用户手工执行 crontab -l 核对；确认无残留即可继续"))
			case a.Kind == "cron_system" && a.State == "present":
				ds = append(ds, warnDiag("cron_system_present",
					fmt.Sprintf("系统级 crontab 中发现 %d 处指向 SAgent 的条目（/etc/crontab 或 /etc/cron.d）", a.Hits),
					"系统级文件不在本平台的清理范围内，需要人工确认后清理"))
			case a.Kind == "cron_system" && a.State == "unknown":
				ds = append(ds, warnDiag("cron_system_unreadable",
					"系统级 crontab 文件存在但不可读：无法确认是否残留自启条目",
					"以具备权限的账号手工检查 /etc/crontab 与 /etc/cron.d"))
			}
		}
		for _, p := range scan.Plugins {
			if p.Process == "unknown" {
				ds = append(ds, infoDiag("plugin_process_unknown",
					"插件 "+p.Name+" 是否有独立进程无法按路径判定（脚本型插件拉起的后台进程不落在安装目录下）"))
			}
			if !p.Enabled && p.Source == "dir" {
				ds = append(ds, infoDiag("plugin_deployed_not_enabled",
					"插件 "+p.Name+" 的产物存在但配置未启用（"+p.Path+"）：可能是历史残留，卸载会一并清除"))
			}
			if p.Enabled && p.Form == "subprocess" && p.Process == "no" {
				ds = append(ds, warnDiag("plugin_configured_not_running",
					"插件 "+p.Name+" 已启用但没找到对应进程（"+p.Path+" 未在运行）",
					"确认该插件是否本就启动失败；不影响卸载，但说明这台机器的采集可能已不完整"))
			}
		}
		if scan.PluginsTotal == 0 && scan.AutostartCount == 0 {
			ds = append(ds, infoDiag("nothing_extra_found",
				"目标机上未发现采集插件与自愈来源：卸载只影响 SAgent 本体"))
		}
		return map[string]any{
			"scan":            scan,
			"plugins_total":   scan.PluginsTotal,
			"process_count":   scan.ProcessCount,
			"autostart_count": scan.AutostartCount,
		}, ds
	},
}

// offboardPluginsSpec 卸载插件步骤：目标机自证 PLUGINS_CLEAN 才算过
var offboardPluginsSpec = offboardStepSpec{
	Playbook:  "uninstall_plugins.yml",
	Title:     "卸载采集插件",
	Source:    "ansible_uninstall_plugins",
	Audit:     "ansible 卸载 SAgent 采集插件",
	CheckLine: "plugin_process_left=",
	FailCode:  "uninstall_plugins",
	FailHint:  "上机核对插件进程残留（ps -ef 与 /proc/<pid>/exe），确认后点「↻ 重试该步」",
	Verdict: func(pr ansibleRunResult) (map[string]any, []diag) {
		if hasOutMarker(pr, "PLUGINS_CLEAN") {
			return map[string]any{"plugins_verdict": "clean"}, []diag{}
		}
		return map[string]any{"plugins_verdict": "dirty"}, []diag{fatalDiag("uninstall_plugins_incomplete",
			"插件停止核对未通过：目标机上仍有插件进程在运行",
			"上机查看残留进程（ps -ef；exe 落在安装目录 plugins/ 下即为插件进程），手工终止后点「↻ 重试该步」")}
	},
}

// offboardAutostartSpec 清理自启步骤。
// 判据刻意分两档：守护必须清掉（硬失败），crontab 读不到只告警（PARTIAL 放行但留痕）——
// 读不到 ≠ 有残留，把环境限制当成卸载失败会制造假失败；
// 但也绝不能静默放过，所以升成告警让人去确认
var offboardAutostartSpec = offboardStepSpec{
	Playbook:  "cleanup_autostart.yml",
	Title:     "清理自愈与自启",
	Source:    "ansible_cleanup_autostart",
	Audit:     "ansible 清理 SAgent 自愈守护与 crontab",
	CheckLine: "guard_exists=",
	FailCode:  "cleanup_autostart",
	FailHint:  "上机核对该守护进程与 crontab 条目（run.sh / crontab -l），处理残留后点「↻ 重试该步」",
	Verdict: func(pr ansibleRunResult) (map[string]any, []diag) {
		if hasOutMarker(pr, "AUTOSTART_DIRTY") {
			return map[string]any{"autostart_verdict": "dirty"}, []diag{fatalDiag("cleanup_autostart_incomplete",
				"自启清理核对未通过：自愈守护仍在运行，或 crontab 中仍有指向 SAgent 的条目",
				"上机核对后清理残留，再点「↻ 重试该步」。注意守护进程会反复拉起 SAgent，必须优先处理")}
		}
		ds := []diag{}
		extra := map[string]any{"autostart_verdict": "clean"}
		if hasOutMarker(pr, "AUTOSTART_PARTIAL") {
			extra["autostart_verdict"] = "partial"
			ds = append(ds, warnDiag("autostart_partial",
				"自愈守护已清理，但 crontab 未能核实（不可读或命令不可用）：可能仍残留指向 SAgent 的定时任务",
				"以安装用户在目标机上执行 crontab -l 核对；确认无 SAgent 条目即可"))
		}
		return extra, ds
	},
}

func ansibleScanJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	ansibleOffboardJob(catDB, agentStore, flowID, step, cred, offboardScanSpec)
}

func ansibleUninstallPluginsJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	ansibleOffboardJob(catDB, agentStore, flowID, step, cred, offboardPluginsSpec)
}

func ansibleCleanupAutostartJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	ansibleOffboardJob(catDB, agentStore, flowID, step, cred, offboardAutostartSpec)
}
