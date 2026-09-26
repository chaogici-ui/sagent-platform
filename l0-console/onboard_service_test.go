package main

// 升级 / 启停流程回归测试（2026-09-22）：
// 纯函数判据（预检解析 / 版本比较 / 参数白名单）+ 模板结构 + 维护态登记。
// 侧重点与卸载侧一致：结论行缺失=失败、幂等与降级语义、渲染占位符白名单

import (
	"strings"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

func ansibleRunOf(lines ...string) ansibleRunResult {
	// taskOutputLines 从逐任务结构化结果取输出行——把契约行放进单个任务的 Lines
	return ansibleRunResult{Tasks: []ansibleTask{{Index: 1, Name: "t", Status: "ok", Lines: lines}}}
}

// ---------- 预检解析 ----------

func TestParsePreflightUpgrade(t *testing.T) {
	pre := parsePreflightUpgrade([]string{
		"TASK 行会混进来但绝不能被误读为结论",
		"UPGRADE_PRE sha=abc123 guard=yes guard_pid=- stopped=no pid=4242 plugins={\"host_metrics\":\"running\"} disk_free_mb=828",
		"UPGRADE_PRE_END",
	})
	if pre == nil {
		t.Fatal("应解析出预检结论")
	}
	if pre.SHA != "abc123" || !pre.Guard || pre.Stopped || pre.PID != "4242" || pre.DiskFreeMB != 828 {
		t.Errorf("字段解析不对: %+v", pre)
	}
	if pre.PluginsRaw != "{\"host_metrics\":\"running\"}" {
		t.Errorf("plugins 原文应原样保留: %q", pre.PluginsRaw)
	}
	if parsePreflightUpgrade([]string{"UPGRADE_PRE_END"}) != nil {
		t.Error("缺 UPGRADE_PRE 结论行应返回 nil（判据缺失=失败）")
	}
}

func TestParsePreflightService(t *testing.T) {
	pre := parsePreflightService([]string{
		"SVC_PRE pid=- guard=yes stopped=yes ctl=-",
		"SVC_PRE_END",
	})
	if pre == nil {
		t.Fatal("应解析出预检结论")
	}
	if pre.PID != "-" || !pre.Guard || !pre.Stopped {
		t.Errorf("字段解析不对: %+v", pre)
	}
}

func TestServicePluginStatus(t *testing.T) {
	st := servicePluginStatus("{\"a\":\"running\",\"b\":\"stopped\"}")
	if st["a"] != "running" || st["b"] != "stopped" {
		t.Errorf("插件状态解析不对: %v", st)
	}
	if len(servicePluginStatus("-")) != 0 || len(servicePluginStatus("")) != 0 {
		t.Error("ctl 不可用（-）或空时应返回空表，不应猜")
	}
}

// ---------- 版本比较与反查 ----------

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.9", "0.4.0", true},
		{"0.4.0", "0.4.0", false},
		{"0.10.0", "0.9.0", false}, // 数值比较，不是字典序
		{"1.0", "1.0.1", true},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionBySHA(t *testing.T) {
	if versionBySHA("") != nil || versionBySHA("-") != nil {
		t.Error("空/未测 sha 不应反查出任何版本")
	}
	if versionBySHA("0000000000000000000000000000000000000000000000000000000000000000") != nil {
		t.Error("清单外 sha 不应反查出任何版本")
	}
}

// ---------- 启停参数白名单（拉起时钉死，playbook 渲染的是白名单产物）----------

func TestSvcParamsValid(t *testing.T) {
	good := []svcParams{
		{Action: "stop", Target: "process"},
		{Action: "start", Target: "process"},
		{Action: "restart", Target: "process"},
		{Action: "stop", Target: "plugin:log_metrics"},
		{Action: "restart", Target: "plugin:custom_scripts-2"},
	}
	for _, p := range good {
		if !p.valid() {
			t.Errorf("合法参数被拒: %+v", p)
		}
	}
	bad := []svcParams{
		{}, {Action: "stop"}, {Action: "stop", Target: "plugin:"},
		{Action: "stop", Target: "plugin:Bad Name"},      // 空格
		{Action: "stop", Target: "plugin:$(rm -rf ~)"},   // 注入尝试
		{Action: "stop", Target: "plugin:UPPER"},         // 大写
		{Action: "pause", Target: "process"},             // 动作白名单外
		{Action: "stop", Target: "process;reboot"},       // 分号
	}
	for _, p := range bad {
		if p.valid() {
			t.Errorf("非法参数被放行: %+v", p)
		}
	}
}

func TestSvcParamsTargetLabel(t *testing.T) {
	if got := (svcParams{Action: "stop", Target: "process"}).targetLabel(); got != "整个 SAgent（进程级）" {
		t.Errorf("targetLabel 进程级 = %q", got)
	}
	if got := (svcParams{Action: "stop", Target: "plugin:log_metrics"}).targetLabel(); got != "插件 log_metrics（插件级）" {
		t.Errorf("targetLabel 插件级 = %q", got)
	}
}

// ---------- 判据（Verdict）：结论行缺失=失败，这是"自证"的底线 ----------

func TestUpgradeVerdictMarkers(t *testing.T) {
	if _, ds := upgradeVerdict(ansibleRunOf("UPGRADE_DIRTY 启动后进程未存活 pid=-")); firstFatal(ds) == nil {
		t.Error("UPGRADE_DIRTY 应判失败")
	}
	if _, ds := upgradeVerdict(ansibleRunOf("UPGRADE_GUARD_TIMEOUT")); firstFatal(ds) == nil {
		t.Error("UPGRADE_GUARD_TIMEOUT 应判失败")
	}
	if _, ds := upgradeVerdict(ansibleRunOf("PLAY RECAP ok=2")); firstFatal(ds) == nil {
		t.Error("缺 UPGRADE_OK 结论行应判失败（不能把没核对当成功）")
	}
	extra, ds := upgradeVerdict(ansibleRunOf("UPGRADE_OK pid=100 ver_sha=abc bak=SAgent.bak-x ctl={}"))
	if firstFatal(ds) != nil || extra["verify_line"] == "" {
		t.Errorf("UPGRADE_OK 应判成功且带核对行: extra=%v ds=%v", extra, ds)
	}
}

func TestServiceVerdictMarkers(t *testing.T) {
	if _, ds := serviceVerdict(ansibleRunOf("SVC_DIRTY 仍有 1 个 SAgent 进程")); firstFatal(ds) == nil {
		t.Error("SVC_DIRTY 应判失败")
	}
	if _, ds := serviceVerdict(ansibleRunOf("PLAY RECAP ok=1")); firstFatal(ds) == nil {
		t.Error("缺结论行应判失败")
	}
	for _, okLine := range []string{
		"SVC_STOPPED guard=yes", "SVC_STARTED pid=100", "SVC_RESTARTED pid=101",
		"SVC_ALREADY already-stopped", "SVC_PLUGIN_STOPPED log_metrics", "SVC_PLUGIN_STARTED log_metrics",
	} {
		if _, ds := serviceVerdict(ansibleRunOf(okLine)); firstFatal(ds) != nil {
			t.Errorf("%q 应判成功", okLine)
		}
	}
}

func TestPreflightUpgradeVerdictNotInstalled(t *testing.T) {
	pr := ansibleRunOf("UPGRADE_PRE sha=- guard=no guard_pid=- stopped=no pid=- plugins=- disk_free_mb=500", "UPGRADE_PRE_END")
	_, ds := preflightUpgradeVerdict(pr)
	if firstFatal(ds) == nil {
		t.Error("目标机无二进制（sha=-）时预检判据应 fatal——没装过就无所谓升级")
	}
	prOK := ansibleRunOf("UPGRADE_PRE sha=abc guard=no guard_pid=- stopped=no pid=1 plugins=- disk_free_mb=50", "UPGRADE_PRE_END")
	_, ds2 := preflightUpgradeVerdict(prOK)
	if firstFatal(ds2) != nil {
		t.Errorf("正常预检不应 fatal: %v", ds2)
	}
}

// ---------- 模板接线 ----------

func TestOpsTemplatesWired(t *testing.T) {
	tpls, err := loadFlowTemplates(defaultFlowTemplateDir)
	if err != nil {
		t.Fatalf("loadFlowTemplates: %v", err)
	}
	for _, tp := range flowTemplateListForWizard() {
		if tp.ID == "upgrade" || tp.ID == "service" {
			t.Errorf("内部模板 %s 不应出现在向导可选模式里", tp.ID)
		}
	}
	byID := map[string]*FlowTemplate{}
	for _, tp := range tpls {
		byID[tp.ID] = tp
	}
	up, svc := byID["upgrade"], byID["service"]
	if up == nil || svc == nil {
		t.Fatal("upgrade/service 模板缺失")
	}
	if !up.Internal || !svc.Internal {
		t.Error("upgrade/service 必须是 internal（由行内按钮拉起）")
	}
	wantSteps := map[string][]string{
		"upgrade": {"pick_object", "preflight_upgrade", "pick_version", "confirm_upgrade", "upgrade_agent"},
		"service": {"pick_object", "preflight_service", "confirm_service", "service_execute"},
	}
	for mode, steps := range wantSteps {
		tp := byID[mode]
		if len(tp.Steps) != len(steps) {
			t.Fatalf("[%s] 步骤数 %d != 期望 %d", mode, len(tp.Steps), len(steps))
		}
		for i, s := range steps {
			if tp.Steps[i].ID != s {
				t.Errorf("[%s] 第 %d 步 = %q, want %q", mode, i, tp.Steps[i].ID, s)
			}
		}
	}
	// 升级是不可逆动作 → 执行环节保留 blocker 人工闸门（确认没过不能动目标机）
	if last := up.Steps[len(up.Steps)-1]; last.Gate != "blocker" {
		t.Errorf("[upgrade] 执行环节 %s 应设 gate: blocker（升级不可逆）", last.ID)
	}
	// 启停是可逆轻动作 → 核对即放行，执行环节不得设人工闸门（2026-09-24 评审拍板）
	if last := svc.Steps[len(svc.Steps)-1]; last.Gate != "" {
		t.Errorf("[service] 执行环节 %s 不应设 gate（核对即放行），got %q", last.ID, last.Gate)
	}
}

// ---------- playbook 渲染：占位符必须被替换干净（残留 @TOKEN@ = 渲染失败进目标机）----------

func TestLoadSvcPlaybookRendersAllTokens(t *testing.T) {
	body := loadSvcPlaybook("agent_service.yml", "/home/ibomc", map[string]string{
		"@SVC_ACTION@": "stop", "@SVC_TARGET@": "plugin:log_metrics",
	})
	if body == "" {
		t.Fatal("取不到 agent_service.yml（磁盘与内置副本都缺失）")
	}
	if strings.Contains(body, "@DEST_HOME@") || strings.Contains(body, "@SVC_ACTION@") || strings.Contains(body, "@SVC_TARGET@") {
		t.Error("playbook 存在未渲染的占位符")
	}
	if !strings.Contains(body, "'stop' == 'stop'") {
		t.Error("when 条件渲染后应为字面量比较（渲染前的模板写法见文件头注释）")
	}
}

// ---------- 维护态登记 ----------

func TestSetResourceSvcState(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.UpsertResource(&storepkg.Resource{ID: "svc-res", IP: "10.0.0.1"}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	for _, c := range []struct{ state, want string }{
		{"stopped", "stopped"}, {"running", "running"}, {"", ""},
	} {
		if err := db.SetResourceSvcState("svc-res", c.state); err != nil {
			t.Fatalf("SetResourceSvcState(%q): %v", c.state, err)
		}
		res, _ := db.GetResource("svc-res")
		if res == nil || res.SvcState != c.want {
			t.Errorf("SetResourceSvcState(%q) 后读回 = %+v", c.state, res)
		}
	}
	// 不存在的资源不报错也不创建（幂等 UPDATE）
	if err := db.SetResourceSvcState("no-such-res", "stopped"); err != nil {
		t.Errorf("对不存在资源登记维护态不应报错: %v", err)
	}
}
