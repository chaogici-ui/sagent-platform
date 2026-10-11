package main

// onboard_l1_dispatch_test.go —— IN5 B3：install 投递端（取号 + 重入闸）语义。
//
// 覆盖的是「投递一次真执行」这条链路的边界，而不是某次请求的返回：
//   - 重入闸：advanceFlow 对 running 步骤会反复进入，重复投递必须被挡（一次投递 = 一个任务 + 一条 running）
//   - 取号：人工「重试该步」不递增 attempt，必须跳过被已消费任务占用的号，否则 ID 相撞 → 步骤卡死
//   - 超时强拉：forced 必须绕过重入闸，把新尝试真发出去（否则"自动重试"只改事件不换执行）

import (
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// l1DispatchTestRun 造一条含「选版本 + 安装」的流水线并构造投递所需的 flowRun。
// 安装只装人选的版本，故先写一条 pick_version 的 OK 事件（detail.tag）。
func l1DispatchTestRun(t *testing.T, db *storepkg.DB, resID, tag string) (*flowRun, storepkg.FlowStepSnapshot) {
	t.Helper()
	testOnboardCfg(t)
	// 版本清单是包级全局：本次用例临时置入该 tag，用完还原（避免污染其它用例）
	versionMu.Lock()
	saved := versionCatalog
	versionCatalog = []SAVersion{{Tag: tag, Version: "9.0", OS: "linux", Arch: "amd64", SHA256: "deadbeef"}}
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		versionCatalog = saved
		versionMu.Unlock()
	})

	step := storepkg.FlowStepSnapshot{
		ID: "install_agent", Atom: "install_agent", Title: "安装 SAgent", Scope: "external",
	}
	id := makeFlow(t, db, resID, []storepkg.FlowStepSnapshot{
		{ID: "pick_version", Atom: "pick_version", Title: "选版本", Scope: "platform"},
		step,
	})
	// 补齐 SSH 凭据（投递载荷自足的前提；进程内路径同样要求凭据齐备）
	if err := db.UpsertResource(&storepkg.Resource{
		ID: resID, Name: resID, IP: "10.7.0.9", ResourceType: "host", Role: "business", Source: "manual",
		SSHPort: 22, SSHUser: "deploy", SSHPassword: "secret",
	}); err != nil {
		t.Fatalf("UpsertResource(cred): %v", err)
	}
	if err := appendStepEvent(db, id, storepkg.FlowStepSnapshot{ID: "pick_version"}, stOK,
		"人工指定版本："+tag, detailJSON(map[string]any{"tag": tag}), 0); err != nil {
		t.Fatalf("pick_version event: %v", err)
	}

	f, err := db.GetFlow(id)
	if err != nil || f == nil {
		t.Fatalf("GetFlow: %v", err)
	}
	res, _ := db.GetResource(resID)
	return &flowRun{
		catDB: db, flow: f, resource: res, cfg: onboardCfg, agentID: resID,
		stepStatus: map[string]string{}, stepWaiting: map[string]string{},
		stepProgress: map[string]any{}, stepAttempts: map[string]any{},
		stepDiags: map[string][]diag{},
	}, step
}

// installTasksOf 取该步骤已投递的任务（按 runner 身份定向）。
func installTasksOf(t *testing.T, db *storepkg.DB) []*storepkg.L1TaskRow {
	t.Helper()
	rows, err := db.ListL1Tasks(cfgInstallRunnerID, 0)
	if err != nil {
		t.Fatalf("ListL1Tasks: %v", err)
	}
	return rows
}

// runningEventsOf 取某步骤的 running 事件条数（重复投递会在这里露馅）。
func runningEventsOf(t *testing.T, db *storepkg.DB, flowID int64, stepID string) int {
	t.Helper()
	events, err := db.ListEvents(flowID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Step == stepID && e.Status == stRunning {
			n++
		}
	}
	return n
}

// TestFreeInstallAttemptSkipsConsumed 取号语义：空位直取，被已消费任务占用的号跳过，在飞任务不算占用。
func TestFreeInstallAttemptSkipsConsumed(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	const flowID, stepID = int64(7), "install_agent"

	if got := freeL1Attempt(db, flowID, stepID, 1); got != 1 {
		t.Fatalf("无任务时应返回原号 1，实际 %d", got)
	}

	// 已消费（done + 已对账）的任务占用 1 号 → 应顺延到 2
	queueBoundTask(t, db, "flow-7-install_agent-a1", flowID, stepID, 1)
	if _, err := db.AckL1Task("flow-7-install_agent-a1", "ansible", "done", "已装", ""); err != nil {
		t.Fatalf("AckL1Task: %v", err)
	}
	if err := db.MarkL1TaskReconciled("flow-7-install_agent-a1"); err != nil {
		t.Fatalf("MarkL1TaskReconciled: %v", err)
	}
	if got := freeL1Attempt(db, flowID, stepID, 1); got != 2 {
		t.Fatalf("1 号已被消费，应顺延到 2，实际 %d", got)
	}

	// 在飞（pending/dispatched 未对账）不算占用：它还会回执，不该被跳过
	queueBoundTask(t, db, "flow-7-install_agent-a2", flowID, stepID, 2)
	if got := freeL1Attempt(db, flowID, stepID, 1); got != 2 {
		t.Fatalf("在飞任务不应算占用，应仍返回 2，实际 %d", got)
	}
}

// TestL1InstallAlreadyRunningGuard 重入闸：投递后本步本尝试应被识别为"已投递"，别的尝试不命中。
func TestL1InstallAlreadyRunningGuard(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	run, step := l1DispatchTestRun(t, db, "ip-10-7-0-11", "v9")
	cred := sshCredOf(run.resource)

	if !deliverStepToRunner(db, nil, run, step, cred, false, "install") {
		t.Fatal("首次投递应被接管")
	}
	if !l1StepAlreadyRunning(db, run.flow.ID, step.ID, 1) {
		t.Fatal("投递后应识别为已投递（attempt=1）")
	}
	if l1StepAlreadyRunning(db, run.flow.ID, step.ID, 2) {
		t.Fatal("别的尝试号不应命中重入闸")
	}
}

// TestDeliverInstallIsIdempotentOnReentry 重复推进不得重复投递、不得重复追加 running 事件。
func TestDeliverInstallIsIdempotentOnReentry(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	run, step := l1DispatchTestRun(t, db, "ip-10-7-0-12", "v9")
	cred := sshCredOf(run.resource)

	for i := 0; i < 3; i++ {
		if !deliverStepToRunner(db, nil, run, step, cred, false, "install") {
			t.Fatalf("第 %d 次投递应被接管", i+1)
		}
	}
	if n := len(installTasksOf(t, db)); n != 1 {
		t.Fatalf("重复投递产生了 %d 个任务，want 1", n)
	}
	if n := runningEventsOf(t, db, run.flow.ID, step.ID); n != 1 {
		t.Fatalf("重复投递产生了 %d 条 running 事件，want 1", n)
	}
}

// TestDeliverInstallAfterManualRetryUsesNewAttempt 人工重试（重置回 pending 且不递增 attempt）
// 后必须拿到新任务 ID——沿用旧号会撞上已消费任务，表现为"投递成功却永远等不到回执"。
func TestDeliverInstallAfterManualRetryUsesNewAttempt(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	run, step := l1DispatchTestRun(t, db, "ip-10-7-0-13", "v9")
	cred := sshCredOf(run.resource)

	// 第一次投递 → 回执 done → 对账消费（等价一次成功安装）
	deliverStepToRunner(db, nil, run, step, cred, false, "install")
	if _, err := db.AckL1Task("flow-"+itoa64(run.flow.ID)+"-install_agent-a1", "ansible", "done", "已装", ""); err != nil {
		t.Fatalf("AckL1Task: %v", err)
	}
	if err := db.MarkL1TaskReconciled("flow-" + itoa64(run.flow.ID) + "-install_agent-a1"); err != nil {
		t.Fatalf("MarkL1TaskReconciled: %v", err)
	}
	// 人工「重试该步」：把步骤重置回 pending（onboard_api.go retry 分支同口径）
	if err := appendStepEvent(db, run.flow.ID, step, stPending, "人工重试：重置该环节，等待重新执行",
		detailJSON(map[string]any{"operator": "tester", "prev_status": stOK}), 0); err != nil {
		t.Fatalf("retry event: %v", err)
	}

	deliverStepToRunner(db, nil, run, step, cred, false, "install")

	rows := installTasksOf(t, db)
	if len(rows) != 2 {
		t.Fatalf("人工重试后应有 2 个任务（a1 已消费 + 新尝试），实际 %d", len(rows))
	}
	ev, _ := db.LatestEvent(run.flow.ID, step.ID)
	pr := progressOf(ev.Detail)
	if ev == nil || ev.Status != stRunning || pr == nil || pr.Attempt != 2 {
		t.Fatalf("重试后 running 事件应落在 attempt=2：%+v", ev)
	}
}

// TestDeliverInstallForcedRetryDispatchesNewAttempt 超时强拉（forced）必须绕过重入闸，
// 把新 attempt 真发出去——否则"自动重试"只改了事件，执行还是旧的。
func TestDeliverInstallForcedRetryDispatchesNewAttempt(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	run, step := l1DispatchTestRun(t, db, "ip-10-7-0-14", "v9")
	cred := sshCredOf(run.resource)

	deliverStepToRunner(db, nil, run, step, cred, false, "install") // a1 在飞

	// 超时自动重试：先写一条 attempt=2 的 running 事件（handleStepTimeout 同口径，无 exec_channel），再强拉
	if err := appendStepEvent(db, run.flow.ID, step, stRunning, "第 1 次尝试超时，平台自动重试中（第 2/2 次）",
		detailJSON(map[string]any{
			"executor": "ansible", "runner": "platform",
			"progress": stepProgress{Attempt: 2, MaxAttempts: 2},
		}), 0); err != nil {
		t.Fatalf("retry running event: %v", err)
	}

	if !deliverStepToRunner(db, nil, run, step, cred, true, "install") {
		t.Fatal("强拉投递应被接管")
	}
	rows := installTasksOf(t, db)
	if len(rows) != 2 {
		t.Fatalf("强拉后应有 2 个任务（旧 a1 + 新 a2），实际 %d", len(rows))
	}
	found := false
	for _, r := range rows {
		if r.Attempt == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("强拉应产生 attempt=2 的新任务：%+v", rows)
	}
}

// ===================================================================
//  B4b-4：升级/启停四步的投递端（载荷自足，D3）
// ===================================================================

// useVersionCatalog 临时置入单 tag 版本清单，用完还原（清单是包级全局，防用例间污染）。
func useVersionCatalog(t *testing.T, tag, sha string) {
	t.Helper()
	versionMu.Lock()
	saved := versionCatalog
	versionCatalog = []SAVersion{{Tag: tag, Version: "9.0", OS: "linux", Arch: "amd64", SHA256: sha}}
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		versionCatalog = saved
		versionMu.Unlock()
	})
}

// l1SvcDispatchRun 造一条仅含指定升级/启停步骤的流水线（edge + SSH 凭据齐备）。
// paramsJSON 非空时随流水线写入（service_execute 的动作/目标从参数读）。
func l1SvcDispatchRun(t *testing.T, db *storepkg.DB, resID, atom, paramsJSON string) (*flowRun, storepkg.FlowStepSnapshot) {
	t.Helper()
	testOnboardCfg(t)
	if err := db.UpsertResource(&storepkg.Resource{
		ID: resID, Name: resID, IP: "10.7.0.9", ResourceType: "host", Role: "business", Source: "manual",
		SSHPort: 22, SSHUser: "deploy", SSHPassword: "secret",
	}); err != nil {
		t.Fatalf("UpsertResource(cred): %v", err)
	}
	step := storepkg.FlowStepSnapshot{ID: atom, Atom: atom, Title: svcSpecForKind(atom).Title, Scope: "external"}
	id, err := db.CreateFlow(&storepkg.Flow{
		ResourceID: resID, Mode: "edge", TemplateID: "edge",
		ParamsJSON: paramsJSON, Steps: []storepkg.FlowStepSnapshot{step}, Status: "running",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	f, err := db.GetFlow(id)
	if err != nil || f == nil {
		t.Fatalf("GetFlow: %v", err)
	}
	res, _ := db.GetResource(resID)
	return &flowRun{
		catDB: db, flow: f, resource: res, cfg: onboardCfg, agentID: resID,
		stepStatus: map[string]string{}, stepWaiting: map[string]string{},
		stepProgress: map[string]any{}, stepAttempts: map[string]any{},
		stepDiags: map[string][]diag{},
	}, step
}

// TestBuildSvcPayloadUpgrade 升级载荷自足：人工选定版本/清单 sha/备份名必须随载荷
// ——执行端读不到 L0 的 pick_version 事件、预检事件与版本清单，缺一样升级就没法做。
func TestBuildSvcPayloadUpgrade(t *testing.T) {
	db := testOnboardDB(t)
	useVersionCatalog(t, "v9", "deadbeef")
	run, step := l1SvcDispatchRun(t, db, "ip-10-7-0-31", "upgrade_agent", "")
	if err := appendStepEvent(db, run.flow.ID, storepkg.FlowStepSnapshot{ID: "pick_version"}, stOK,
		"人工指定版本：v9", detailJSON(map[string]any{"tag": "v9"}), 0); err != nil {
		t.Fatalf("pick_version event: %v", err)
	}
	// 预检结论在 L0 事件里：备份名按预检反查出的当前版本（升级失败可手工回滚）
	if err := appendStepEvent(db, run.flow.ID, storepkg.FlowStepSnapshot{ID: "preflight_upgrade"}, stOK,
		"升级前预检完成", detailJSON(map[string]any{"pre": upgradePre{SHA: "cafe01", CurrentTag: "v8", CurrentVer: "8.0"}}), 0); err != nil {
		t.Fatalf("preflight event: %v", err)
	}

	payload, derr := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "upgrade_agent")
	if derr != nil {
		t.Fatalf("buildSvcPayload: %+v", derr)
	}
	if payload["playbook"] != "upgrade.yml" || payload["title"] != "升级 SAgent" {
		t.Fatalf("playbook/title 不对: %v / %v", payload["playbook"], payload["title"])
	}
	if payload["tag"] != "v9" || payload["sha256"] != "deadbeef" || payload["version"] != "9.0" {
		t.Fatalf("版本三元组没随载荷: %v", payload)
	}
	if got := payload["bak_name"]; got != "SAgent.bak-v8" {
		t.Fatalf("备份名应按预检反查的当前版本: %v", got)
	}
	if payload["ssh_host"] != "10.7.0.9" || payload["ssh_user"] != "deploy" {
		t.Fatalf("SSH 凭据没随载荷: %v", payload)
	}
	if payload["dest_home"] == "" || intOf(payload["budget_sec"]) <= 0 {
		t.Fatalf("dest_home/budget_sec 缺失: %v", payload)
	}
	if payload["flow_id"].(int64) != run.flow.ID || payload["step_id"] != "upgrade_agent" || payload["attempt"].(int) != 1 {
		t.Fatalf("绑定信息没随载荷: %v", payload)
	}
}

// TestBuildSvcPayloadUpgradeGuards 无人工选版 / 版本不在清单 → 当场拒绝投递，
// 诊断与进程内 upgradeExtras 同码同文案（两条执行路径的失败结论必须一致）。
func TestBuildSvcPayloadUpgradeGuards(t *testing.T) {
	db := testOnboardDB(t)
	useVersionCatalog(t, "v9", "deadbeef")
	run, step := l1SvcDispatchRun(t, db, "ip-10-7-0-32", "upgrade_agent", "")

	if _, derr := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "upgrade_agent"); derr == nil || derr.reason != "no_human_pick" {
		t.Fatalf("无 pick_version 应拒投（no_human_pick）: %+v", derr)
	}
	// 选了清单外的版本
	if err := appendStepEvent(db, run.flow.ID, storepkg.FlowStepSnapshot{ID: "pick_version"}, stOK,
		"人工指定版本：vX", detailJSON(map[string]any{"tag": "vX"}), 0); err != nil {
		t.Fatalf("pick_version event: %v", err)
	}
	if _, derr := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "upgrade_agent"); derr == nil || derr.reason != "version_not_in_catalog" {
		t.Fatalf("清单外版本应拒投（version_not_in_catalog）: %+v", derr)
	}
}

// TestBuildSvcPayloadService 启停载荷：动作/目标来自拉起时选定（白名单校验后随载荷）；
// 参数缺失/非法 → 当场拒投（svcParams.valid 同一口径）。
func TestBuildSvcPayloadService(t *testing.T) {
	db := testOnboardDB(t)
	run, step := l1SvcDispatchRun(t, db, "ip-10-7-0-33", "service_execute", `{"action":"stop","target":"process"}`)
	payload, derr := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "service_execute")
	if derr != nil {
		t.Fatalf("buildSvcPayload: %+v", derr)
	}
	if payload["playbook"] != "agent_service.yml" || payload["svc_action"] != "stop" || payload["svc_target"] != "process" {
		t.Fatalf("启停载荷不对: %v", payload)
	}

	// 参数缺失（ParamsJSON 为空对象）→ 白名单不过 → 拒投
	run2, step2 := l1SvcDispatchRun(t, db, "ip-10-7-0-34", "service_execute", "")
	if _, derr := buildSvcPayload(db, run2, step2, sshCredOf(run2.resource), 1, "service_execute"); derr == nil || derr.reason != "invalid_params" {
		t.Fatalf("参数非法应拒投（invalid_params）: %+v", derr)
	}
}

// TestBuildSvcPayloadPreflight 两步预检是纯 playbook 路径：无业务占位符（tag/svc_* 不应出现）。
func TestBuildSvcPayloadPreflight(t *testing.T) {
	db := testOnboardDB(t)
	run, step := l1SvcDispatchRun(t, db, "ip-10-7-0-35", "preflight_upgrade", "")
	payload, derr := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "preflight_upgrade")
	if derr != nil {
		t.Fatalf("preflight_upgrade: %+v", derr)
	}
	if payload["playbook"] != "preflight_upgrade.yml" {
		t.Fatalf("playbook 不对: %v", payload["playbook"])
	}
	for _, no := range []string{"tag", "sha256", "svc_action", "svc_target"} {
		if _, ok := payload[no]; ok {
			t.Fatalf("预检载荷不应携带业务字段 %q: %v", no, payload)
		}
	}

	run2, step2 := l1SvcDispatchRun(t, db, "ip-10-7-0-36", "preflight_service", "")
	payload2, derr2 := buildSvcPayload(db, run2, step2, sshCredOf(run2.resource), 1, "preflight_service")
	if derr2 != nil || payload2["playbook"] != "preflight_service.yml" {
		t.Fatalf("preflight_service: derr=%+v playbook=%v", derr2, payload2["playbook"])
	}

	// 支持集外的 kind：显式拒投（unknown_kind），不做"尽力而为"
	if _, derr3 := buildSvcPayload(db, run, step, sshCredOf(run.resource), 1, "restart"); derr3 == nil || derr3.reason != "unknown_kind" {
		t.Fatalf("支持集外 kind 应拒投（unknown_kind）: %+v", derr3)
	}
}
