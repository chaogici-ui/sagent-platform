package main

// onboard_l1_ack_test.go —— IN5 B2：L1 任务回执对账桥语义。
//
// 覆盖的是「回执 → 步骤终态」这条翻译链路的边界，而不是某次请求的返回：
//   - done/failed 两种终态的驱动结果与证据落库
//   - 旧尝试回执必须丢弃（超时重试后不得覆盖新尝试的进展）
//   - 无法对账的任务（流水线/步骤不存在、旧尝试）也必须标记消费，否则每轮清扫反复捞
//   - 未绑定步骤的任务（心跳/探针类）不参与对账且不报错

import (
	"strings"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// l1AckTestFlow 建一条只有「安装」一步的流水线并推进到 running
// （external 步骤下发后停在 running 等回报）。
func l1AckTestFlow(t *testing.T, db *storepkg.DB, resID string) int64 {
	t.Helper()
	testOnboardCfg(t)
	id := makeFlow(t, db, resID, []storepkg.FlowStepSnapshot{
		{ID: "install_agent", Atom: "install_agent", Title: "安装 SAgent", Scope: "external"},
	})
	// B5 恒走 L1 Runner：无凭据会让投递当场 fail，步骤到不了 running
	seedSSHCred(t, db, resID)
	// install 投递要求人工选定版本（buildInstallPayload 门禁，B5 后无 pick_version 当场拒投）
	seedHumanPick(t, db, id, "v9")
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	if st := latestStatuses(t, db, id); st["install_agent"] != stRunning {
		t.Fatalf("install_agent 应为 running，实际 %q", st["install_agent"])
	}
	return id
}

// queueBoundTask 投递一条绑定到流水线步骤的任务（等价 L0 下发 install 任务给 L1 执行端）。
func queueBoundTask(t *testing.T, db *storepkg.DB, id string, flowID int64, stepID string, attempt int) {
	t.Helper()
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{
		ID: id, ControllerID: "l1-ansible-runner-1", Kind: "install",
		FlowID: flowID, StepID: stepID, Attempt: attempt,
	}); err != nil {
		t.Fatalf("queue %s: %v", id, err)
	}
}

// TestReconcileAckDoneDrivesStepOK done 回执 → 步骤 ok + 结构化结果并入证据 + 任务标记消费。
func TestReconcileAckDoneDrivesStepOK(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1AckTestFlow(t, db, "ip-10-7-0-1")

	queueBoundTask(t, db, "t-ok-1", id, "install_agent", 1)
	if _, err := db.AckL1Task("t-ok-1", "ansible", "done", "SAgent 已安装并自证通过",
		`{"installed_tag":"linux-arm64-0.4.2-dev"}`); err != nil {
		t.Fatalf("ack: %v", err)
	}

	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["install_agent"] != stOK {
		t.Fatalf("install_agent = %q, want ok", st["install_agent"])
	}
	ev, _ := db.LatestEvent(id, "install_agent")
	if ev == nil || !strings.Contains(ev.Detail, "linux-arm64-0.4.2-dev") {
		t.Fatalf("结构化结果未并入证据: %+v", ev)
	}
	if r, _ := db.GetL1Task("t-ok-1"); r == nil || r.ReconciledAt == 0 {
		t.Fatalf("任务未标记消费: %+v", r)
	}

	// 幂等：再跑一轮不得改动已终态的步骤
	reconcileL1TaskAcks(db, nil)
	if st := latestStatuses(t, db, id); st["install_agent"] != stOK {
		t.Fatalf("二次对账改动了步骤状态: %q", st["install_agent"])
	}
}

// TestReconcileAckFailedDrivesStepFail failed 回执 → 步骤 fail，且诊断兜底不为空。
func TestReconcileAckFailedDrivesStepFail(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1AckTestFlow(t, db, "ip-10-7-0-2")

	queueBoundTask(t, db, "t-fail-1", id, "install_agent", 1)
	if _, err := db.AckL1Task("t-fail-1", "ansible", "failed", "sha256 与 L0 权威不一致", ""); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["install_agent"] != stFail {
		t.Fatalf("install_agent = %q, want fail", st["install_agent"])
	}
	ev, _ := db.LatestEvent(id, "install_agent")
	if ev == nil || !strings.Contains(ev.Detail, "l1_exec_failed") {
		t.Fatalf("缺分级诊断兜底（failStep 三件套必须齐备）: %+v", ev)
	}
}

// TestReconcileAckIgnoresStaleAttempt 旧尝试回执必须丢弃：
// 超时重试已把步骤推到第 2 次尝试，第 1 次任务此时回执不得覆盖新尝试。
func TestReconcileAckIgnoresStaleAttempt(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1AckTestFlow(t, db, "ip-10-7-0-3")

	// 模拟清扫器重试：把 running 事件的 progress.attempt 推到 2
	now := time.Now().Format("2006-01-02 15:04:05")
	step := storepkg.FlowStepSnapshot{ID: "install_agent", Atom: "install_agent",
		Title: "安装 SAgent", Scope: "external"}
	if err := appendStepEvent(db, id, step, stRunning, "执行中（第 2 次尝试）",
		detailJSON(map[string]any{"progress": stepProgress{Attempt: 2, StartedAt: now, LastActivityAt: now}}), 0); err != nil {
		t.Fatalf("appendStepEvent: %v", err)
	}

	queueBoundTask(t, db, "t-stale-1", id, "install_agent", 1)
	if _, err := db.AckL1Task("t-stale-1", "ansible", "done", "旧尝试完成", ""); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["install_agent"] != stRunning {
		t.Fatalf("旧尝试回执覆盖了新尝试：状态 = %q，want running", st["install_agent"])
	}
	// 无法对账的任务也必须标记消费，否则每轮清扫都会重复捞它
	if r, _ := db.GetL1Task("t-stale-1"); r == nil || r.ReconciledAt == 0 {
		t.Fatalf("旧尝试任务未标记消费: %+v", r)
	}
}

// TestReconcileAckSkipsUnboundTask 未绑定步骤的任务（心跳/探针类）不参与对账且不报错。
func TestReconcileAckSkipsUnboundTask(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}

	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{
		ID: "unbound-1", ControllerID: "l1-controller-1", Kind: "controller-ping",
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := db.AckL1Task("unbound-1", "controller", "done", "pong", ""); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if r, _ := db.GetL1Task("unbound-1"); r == nil || r.ReconciledAt != 0 {
		t.Fatalf("未绑定任务不应进入对账: %+v", r)
	}
}

// ===================================================================
//  IN5 B4：卸载侧回执的 L0 收口（判据只能有一套）
// ===================================================================

// l1OffboardFlow 建一条只有「扫描采集物」一步的卸载侧流水线并推进到 running。
func l1OffboardFlow(t *testing.T, db *storepkg.DB, resID string) int64 {
	t.Helper()
	testOnboardCfg(t)
	id := makeFlow(t, db, resID, []storepkg.FlowStepSnapshot{
		{ID: "scan_collectors", Atom: "scan_collectors", Title: "扫描采集物", Scope: "external"},
	})
	// B5 恒走 L1 Runner：无凭据会让投递当场 fail，步骤到不了 running
	seedSSHCred(t, db, resID)
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	if st := latestStatuses(t, db, id); st["scan_collectors"] != stRunning {
		t.Fatalf("scan_collectors 应为 running，实际 %q", st["scan_collectors"])
	}
	return id
}

// queueOffboardTask 投递一条卸载侧任务（kind 必须随任务带，桥按 kind 选收口口径）。
func queueOffboardTask(t *testing.T, db *storepkg.DB, id string, flowID int64, stepID, kind string, attempt int) {
	t.Helper()
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{
		ID: id, ControllerID: "l1-ansible-runner-1", Kind: kind,
		FlowID: flowID, StepID: stepID, Attempt: attempt,
	}); err != nil {
		t.Fatalf("queue %s: %v", id, err)
	}
}

// TestOffboardReconcileDoneNeedsSelfCertification 卸载侧 done 回执**不等于成功**：
// rc=0 只说明 playbook 跑完了，成功与否由目标机自证核对行决定。
// 缺核对标记必须判失败——这是 B4 的核心：裁定不能跟着执行端搬走。
func TestOffboardReconcileDoneNeedsSelfCertification(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1OffboardFlow(t, db, "ip-10-7-0-21")

	// rc=0 但输出里没有 SCAN 汇总行 → 判"输出不可解析"，绝不能以"扫到 0 个"收场
	queueOffboardTask(t, db, "off-nomarker", id, "scan_collectors", "scan_collectors", 1)
	if _, err := db.AckL1Task("off-nomarker", "ansible-runner", "done", "扫描完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":900,"tasks":[{"index":1,"name":"扫描","status":"ok","stdout":"PLUGIN name=mysql\n"}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["scan_collectors"] != stFail {
		t.Fatalf("缺自证核对行应判失败，实际 %q", st["scan_collectors"])
	}
	ev, _ := db.LatestEvent(id, "scan_collectors")
	if ev == nil || !strings.Contains(ev.Detail, "scan_output_unparsable") {
		t.Fatalf("失败应带扫描输出不可解析诊断: %+v", ev)
	}
}

// TestOffboardReconcileDoneWithMarkerDrivesOK 自证核对行齐备时 done → 步骤 ok，
// 且核对行与结构化扫描结论并入证据（与进程内同形）。
func TestOffboardReconcileDoneWithMarkerDrivesOK(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1OffboardFlow(t, db, "ip-10-7-0-22")

	queueOffboardTask(t, db, "off-marker", id, "scan_collectors", "scan_collectors", 1)
	if _, err := db.AckL1Task("off-marker", "ansible-runner", "done", "扫描完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":900,"recap":{"ok":3},`+
			`"tasks":[{"index":1,"name":"扫描采集物","status":"ok","stdout":"SCAN plugins_total=1 plugins_process=0 autostart=1\n"}],`+
			`"phases":[{"phase":"playbook","rc":0}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["scan_collectors"] != stOK {
		t.Fatalf("自证齐备应判 ok，实际 %q", st["scan_collectors"])
	}
	ev, _ := db.LatestEvent(id, "scan_collectors")
	if ev == nil {
		t.Fatal("缺事件")
	}
	// 核对行 + 结构化结论 + 通道元信息三样都要在：界面/审计按这些字段渲染
	for _, want := range []string{"SCAN plugins_total=1", `"scan"`, execChannelL1, "off-marker"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("成功事件缺 %q: %s", want, ev.Detail)
		}
	}
	if r, _ := db.GetL1Task("off-marker"); r == nil || r.ReconciledAt == 0 {
		t.Fatalf("任务未标记消费: %+v", r)
	}
}

// TestOffboardReconcileFailedKeepsRunnerDiagnosis failed 回执必须保留执行端的归因：
// 执行端已用同一批 classify 函数判过（语法/超时/rc/SSH），桥再跑一遍裁定
// 会把"语法未过"误报成"输出不可解析"。
func TestOffboardReconcileFailedKeepsRunnerDiagnosis(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1OffboardFlow(t, db, "ip-10-7-0-23")

	queueOffboardTask(t, db, "off-syntax", id, "scan_collectors", "scan_collectors", 1)
	if _, err := db.AckL1Task("off-syntax", "ansible-runner", "failed", "扫描采集物 playbook 语法校验未过（未触碰目标机）",
		`{"rc":4,"dest_home":"/home/deploy","budget_sec":900,`+
			`"diagnosis":[{"level":"fatal","code":"scan_collectors_syntax","message":"扫描采集物 playbook 语法校验未过（未触碰目标机）","hint":"修正后重试"}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["scan_collectors"] != stFail {
		t.Fatalf("failed 回执应驱动 fail，实际 %q", st["scan_collectors"])
	}
	ev, _ := db.LatestEvent(id, "scan_collectors")
	if ev == nil || !strings.Contains(ev.Detail, "scan_collectors_syntax") {
		t.Fatalf("执行端归因被覆盖/丢失: %+v", ev)
	}
	if strings.Contains(ev.Detail, "scan_output_unparsable") {
		t.Fatalf("桥重跑了裁定，把语法失败误报成输出不可解析: %s", ev.Detail)
	}
	// 失败也要带上原始事实（rc / dest_home / evidence），不能只留一句 note
	if !strings.Contains(ev.Detail, `"rc": 4`) || !strings.Contains(ev.Detail, "/home/deploy") {
		t.Fatalf("失败事件缺原始事实: %s", ev.Detail)
	}
}

// ===================================================================
//  B4b-4：升级/启停四步回执的 L0 收口（判据只能有一套）
// ===================================================================

// l1SvcFlow 建一条仅含指定升级/启停步骤的流水线（edge + SSH 凭据齐备）并推进到 running。
// paramsJSON 非空时随流水线写入（service_execute 的动作/目标从参数读，PostOK 也按它登记维护态）。
// tag 非空时（upgrade_agent 用例）先注入版本清单与 pick_version 人工选版事件——
// B5 恒走 L1 Runner 后投递要过 buildSvcPayload 门禁：升级缺人工选定版本会当场拒投，步骤落 fail。
func l1SvcFlow(t *testing.T, db *storepkg.DB, resID, atom, paramsJSON, tag string) int64 {
	t.Helper()
	testOnboardCfg(t)
	if err := db.UpsertResource(&storepkg.Resource{
		ID: resID, Name: resID, IP: "10.7.0.9", ResourceType: "host", Role: "business", Source: "manual",
		SSHPort: 22, SSHUser: "deploy", SSHPassword: "secret",
	}); err != nil {
		t.Fatalf("UpsertResource(cred): %v", err)
	}
	id, err := db.CreateFlow(&storepkg.Flow{
		ResourceID: resID, Mode: "edge", TemplateID: "edge",
		ParamsJSON: paramsJSON,
		Steps: []storepkg.FlowStepSnapshot{
			{ID: atom, Atom: atom, Title: svcSpecForKind(atom).Title, Scope: "external"},
		},
		Status: "running",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	if tag != "" {
		seedHumanPick(t, db, id, tag)
	}
	if err := advanceFlow(db, nil, id); err != nil {
		t.Fatalf("advanceFlow: %v", err)
	}
	if st := latestStatuses(t, db, id); st[atom] != stRunning {
		t.Fatalf("%s 应为 running，实际 %q", atom, st[atom])
	}
	return id
}

// TestSvcReconcileServiceStopDrivesOKAndSvcState service_execute done + SVC_STOPPED 自证行
// → 步骤 ok + 维护态登记（stop → 资源标 stopped）+ 通道元信息/原始事实并入证据。
func TestSvcReconcileServiceStopDrivesOKAndSvcState(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-31", "service_execute", `{"action":"stop","target":"process"}`, "")

	queueOffboardTask(t, db, "svc-stop-1", id, "service_execute", "service_execute", 1)
	if _, err := db.AckL1Task("svc-stop-1", "ansible-runner", "done", "停止完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":900,`+
			`"tasks":[{"index":1,"name":"启停","status":"ok","lines":["SVC_STOPPED guard=yes"]}],`+
			`"phases":[{"phase":"syntax","status":"ok"},{"phase":"playbook","rc":0}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["service_execute"] != stOK {
		t.Fatalf("service_execute = %q, want ok", st["service_execute"])
	}
	if res, _ := db.GetResource("ip-10-7-0-31"); res == nil || res.SvcState != "stopped" {
		t.Fatalf("PostOK 应把资源标成 stopped: %+v", res)
	}
	ev, _ := db.LatestEvent(id, "service_execute")
	if ev == nil {
		t.Fatal("缺事件")
	}
	// 自证核对行 + 通道元信息 + 原始事实三样都要在：界面/审计按这些字段渲染
	for _, want := range []string{"SVC_STOPPED", execChannelL1, "svc-stop-1", "/home/deploy"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("成功事件缺 %q: %s", want, ev.Detail)
		}
	}
	if r, _ := db.GetL1Task("svc-stop-1"); r == nil || r.ReconciledAt == 0 {
		t.Fatalf("任务未标记消费: %+v", r)
	}
}

// TestSvcReconcileUpgradePreflightProducesPre 预检 done → 步骤 ok，解析出的 pre 结论并入事件
// ——confirm_upgrade 的人工确认卡靠它摊开升级影响，缺了 pre 确认卡就开天窗。
func TestSvcReconcileUpgradePreflightProducesPre(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-32", "preflight_upgrade", "", "")

	queueOffboardTask(t, db, "svc-pre-1", id, "preflight_upgrade", "preflight_upgrade", 1)
	if _, err := db.AckL1Task("svc-pre-1", "ansible-runner", "done", "预检完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":600,`+
			`"tasks":[{"index":1,"name":"预检","status":"ok","lines":[`+
			`"UPGRADE_PRE sha=abc123 guard=yes guard_pid=- stopped=no pid=4242 plugins={\"host_metrics\":\"running\"} disk_free_mb=828",`+
			`"UPGRADE_PRE_END"]}],`+
			`"phases":[{"phase":"playbook","rc":0}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["preflight_upgrade"] != stOK {
		t.Fatalf("preflight_upgrade = %q, want ok", st["preflight_upgrade"])
	}
	ev, _ := db.LatestEvent(id, "preflight_upgrade")
	if ev == nil {
		t.Fatal("缺事件")
	}
	for _, want := range []string{`"pre"`, "abc123", "4242", "host_metrics"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("预检事件缺 %q（confirm_upgrade 会开天窗）: %s", want, ev.Detail)
		}
	}
}

// TestSvcReconcileUpgradeDoneDrivesOKWithReceipt 升级 done + UPGRADE_OK 自证行 → ok，
// runner 回执里的取包留痕（tag/sha_verified/备份名）并入事件。
func TestSvcReconcileUpgradeDoneDrivesOKWithReceipt(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-33", "upgrade_agent", "", "linux-arm64-0.4.2-dev")

	queueOffboardTask(t, db, "svc-up-1", id, "upgrade_agent", "upgrade_agent", 1)
	if _, err := db.AckL1Task("svc-up-1", "ansible-runner", "done", "升级完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":900,`+
			`"tag":"linux-arm64-0.4.2-dev","sha256":"deadbeef","sha_verified":true,"bak_name":"SAgent.bak-0.4.1",`+
			`"tasks":[{"index":1,"name":"升级","status":"ok","lines":["UPGRADE_OK pid=100 ver_sha=deadbeef bak=SAgent.bak-0.4.1 ctl={}"]}],`+
			`"phases":[{"phase":"playbook","rc":0}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["upgrade_agent"] != stOK {
		t.Fatalf("upgrade_agent = %q, want ok", st["upgrade_agent"])
	}
	ev, _ := db.LatestEvent(id, "upgrade_agent")
	if ev == nil {
		t.Fatal("缺事件")
	}
	for _, want := range []string{"UPGRADE_OK", "linux-arm64-0.4.2-dev", "SAgent.bak-0.4.1"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("升级事件缺 %q: %s", want, ev.Detail)
		}
	}
	if !strings.Contains(ev.Detail, `"sha_verified": true`) {
		t.Fatalf("取包断言结论没随事件: %s", ev.Detail)
	}
}

// TestSvcReconcileDirtyFails done 回执但自证核对未通过（SVC_DIRTY）→ fail + service_dirty 归因
// ——rc=0 只说明 playbook 跑完了，成败由 sp.Verdict 裁定（判据不能跟着执行端搬走）。
func TestSvcReconcileDirtyFails(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-34", "service_execute", `{"action":"stop","target":"process"}`, "")

	queueOffboardTask(t, db, "svc-dirty-1", id, "service_execute", "service_execute", 1)
	if _, err := db.AckL1Task("svc-dirty-1", "ansible-runner", "done", "执行完成 rc=0",
		`{"rc":0,"dest_home":"/home/deploy","budget_sec":900,`+
			`"tasks":[{"index":1,"name":"启停","status":"ok","lines":["SVC_DIRTY 仍有 1 个 SAgent 进程"]}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["service_execute"] != stFail {
		t.Fatalf("SVC_DIRTY 应判失败，实际 %q", st["service_execute"])
	}
	ev, _ := db.LatestEvent(id, "service_execute")
	if ev == nil || !strings.Contains(ev.Detail, "service_dirty") {
		t.Fatalf("失败应带 service_dirty 归因: %+v", ev)
	}
}

// TestSvcReconcileTimedOutFails done 回执但 timed_out=true → 按超时判死（状态≠事实，以回执为准）。
func TestSvcReconcileTimedOutFails(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-35", "upgrade_agent", "", "linux-arm64-0.4.2-dev")

	queueOffboardTask(t, db, "svc-timeout-1", id, "upgrade_agent", "upgrade_agent", 1)
	if _, err := db.AckL1Task("svc-timeout-1", "ansible-runner", "done", "执行完成",
		`{"rc":-1,"timed_out":true,"dest_home":"/home/deploy","budget_sec":900,`+
			`"tasks":[{"index":1,"name":"升级","status":"ok","lines":["UPGRADE_OK pid=100"]}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["upgrade_agent"] != stFail {
		t.Fatalf("timed_out=true 应判失败（哪怕带了 UPGRADE_OK 行），实际 %q", st["upgrade_agent"])
	}
	ev, _ := db.LatestEvent(id, "upgrade_agent")
	if ev == nil || !strings.Contains(ev.Detail, "ansible_phase_timeout") {
		t.Fatalf("失败应带超时诊断: %+v", ev)
	}
}

// TestSvcReconcileFailedKeepsRunnerDiagnosis failed 回执保留执行端归因，桥不重跑裁定
// ——重跑会把"语法未过"误报成"输出不可解析"。
func TestSvcReconcileFailedKeepsRunnerDiagnosis(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks: %v", err)
	}
	id := l1SvcFlow(t, db, "ip-10-7-0-36", "service_execute", `{"action":"stop","target":"process"}`, "")

	queueOffboardTask(t, db, "svc-fail-1", id, "service_execute", "service_execute", 1)
	if _, err := db.AckL1Task("svc-fail-1", "ansible-runner", "failed", "启停 SAgent playbook 语法校验未过（未触碰目标机）",
		`{"rc":4,"dest_home":"/home/deploy","budget_sec":900,`+
			`"diagnosis":[{"level":"fatal","code":"service_execute_syntax","message":"启停 SAgent playbook 语法校验未过（未触碰目标机）","hint":"修正后重试"}]}`); err != nil {
		t.Fatalf("ack: %v", err)
	}
	reconcileL1TaskAcks(db, nil)

	if st := latestStatuses(t, db, id); st["service_execute"] != stFail {
		t.Fatalf("failed 回执应驱动 fail，实际 %q", st["service_execute"])
	}
	ev, _ := db.LatestEvent(id, "service_execute")
	if ev == nil || !strings.Contains(ev.Detail, "service_execute_syntax") {
		t.Fatalf("执行端归因被覆盖/丢失: %+v", ev)
	}
	if strings.Contains(ev.Detail, "service_no_contract") {
		t.Fatalf("桥重跑了裁定，把语法失败误判成缺结论行: %s", ev.Detail)
	}
	if !strings.Contains(ev.Detail, `"rc": 4`) {
		t.Fatalf("失败事件缺原始事实 rc: %s", ev.Detail)
	}
}
