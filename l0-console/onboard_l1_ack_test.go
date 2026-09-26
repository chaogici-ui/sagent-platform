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