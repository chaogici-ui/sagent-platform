package store

// store_pg_test.go —— PostgreSQL 方言与助手回归（架构 D1）。
//
// 目录库已全面 PG：store 各文件的 SQL 一律用 ? 占位符书写，执行前经 rebind 翻成 $N。
// 本文件验的就是这条翻译链（含字面量内的 ? 不误翻）、insertID 的 RETURNING 取 id、
// tableColumns 的 current_schema() 限定，以及 l1_tasks 任务源在 PG 的持久化闭环。
// 载体与其他用例同源（openTestDB → testpg.Provision 独立 schema）。
// 执行：CATALOG_TEST_DSN=... go test ./store/ -run PG -v

import (
	"testing"
)

// TestPGCrossDriverL1Tasks 覆盖 l1_tasks 任务源在 PG 的方言翻译与持久化闭环。
func TestPGCrossDriverL1Tasks(t *testing.T) {
	db := openTestDB(t)

	// 幂等投递（TEXT 主键去重）
	_, created, err := db.QueueL1Task(L1TaskRow{ID: "pg-task-1", ControllerID: "l1-controller-pg", Kind: "install", PayloadJSON: `{"tag":"linux-arm64-0.4.0"}`})
	if err != nil || !created {
		t.Fatalf("Queue first created=%v err=%v, want true/nil", created, err)
	}
	_, dup, err := db.QueueL1Task(L1TaskRow{ID: "pg-task-1", ControllerID: "l1-controller-pg", Kind: "install", PayloadJSON: `{"tag":"x"}`})
	if err != nil || dup {
		t.Fatalf("Queue dup created=%v err=%v, want false/nil (idempotent)", dup, err)
	}

	// 拉取一次性 + 状态翻转
	tasks, err := db.PullL1Tasks("l1-controller-pg")
	if err != nil || len(tasks) != 1 || tasks[0].Status != "dispatched" {
		t.Fatalf("Pull = %d err=%v (status=%v), want 1/dispatched/nil", len(tasks), err, statusOf(tasks))
	}
	again, _ := db.PullL1Tasks("l1-controller-pg")
	if len(again) != 0 {
		t.Fatalf("re-pull = %d, want 0 (must not re-dispatch)", len(again))
	}

	// 回执
	hit, err := db.AckL1Task("pg-task-1", "ansible", "done", "PG ok", "")
	if err != nil || !hit {
		t.Fatalf("Ack hit=%v err=%v, want true/nil", hit, err)
	}
	row, err := db.GetL1Task("pg-task-1")
	if err != nil || row == nil || row.Status != "done" || row.Executor != "ansible" {
		t.Fatalf("after ack row=%+v err=%v, want done/ansible", row, err)
	}
}

// TestPGCrossDriverPullAgentActions 覆盖 G2 通道 A 交付端点（PullAgentActions）在 PG 的
// kind 过滤 + 一次性 + 跨 Agent 隔离方言路径。
func TestPGCrossDriverPullAgentActions(t *testing.T) {
	db := openTestDB(t)

	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "pg-aa-1", ControllerID: "pg-agent", Kind: "agent-action", PayloadJSON: `{"action":"restart"}`}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "pg-controller-task", ControllerID: "pg-agent", Kind: "install"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "pg-aa-other", ControllerID: "pg-agent-2", Kind: "agent-action"}); err != nil {
		t.Fatal(err)
	}

	tasks, err := db.PullAgentActions("pg-agent")
	if err != nil || len(tasks) != 1 || tasks[0].ID != "pg-aa-1" {
		t.Fatalf("PullAgentActions = %d err=%v, want only pg-aa-1", len(tasks), err)
	}
	if tasks[0].Status != "dispatched" {
		t.Fatalf("status = %v, want dispatched", tasks[0].Status)
	}
	if again, _ := db.PullAgentActions("pg-agent"); len(again) != 0 {
		t.Fatalf("re-pull = %d, want 0", len(again))
	}
	cp, _ := db.GetL1Task("pg-controller-task")
	if cp.Status != "pending" {
		t.Fatalf("controller task status = %v, want pending (kind filter broken)", cp.Status)
	}
	other, _ := db.GetL1Task("pg-aa-other")
	if other.Status != "pending" {
		t.Fatalf("other-agent task status = %v, want pending (isolation broken)", other.Status)
	}
}

func statusOf(ts []*L1TaskRow) string {
	if len(ts) == 0 {
		return "<none>"
	}
	return ts[0].Status
}

// TestPGCrossDriverCRUD 覆盖方言翻译与 insertID 助手（审计/采集目标/租户/接入流水线四条 insertID 路径）。
func TestPGCrossDriverCRUD(t *testing.T) {
	db := openTestDB(t)

	// 审计（InsertAudit 直接用 ? 与 COALESCE，验 ?→$N 与字面量不误翻）
	if err := db.InsertAudit("2026-09-23 10:00:00", "admin", "发凭证", "tenant-a", "scope", "成功"); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	rows, err := db.ListAudit(5)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(rows) != 1 || rows[0].Target != "tenant-a" {
		t.Fatalf("audit PG miswritten: %+v", rows)
	}

	// 采集目标（InsertTarget 走 insertID，验 target 自增主键）
	tid, err := db.InsertTarget(&TargetRow{Name: "pg-mysql", Type: "mysql", Address: "localhost:3306", Plugin: "mysql"})
	if err != nil {
		t.Fatalf("InsertTarget: %v", err)
	}
	if tid <= 0 {
		t.Fatalf("target id should be >0, got %d", tid)
	}

	// 租户（CreateTenant 走 insertID）
	t1, err := db.CreateTenant("pg-tenant", "PG测试租户", "d1")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if t1.ID <= 0 || t1.Code != "pg-tenant" {
		t.Fatalf("tenant wrong: %+v", t1)
	}
	tenants, err := db.ListTenants()
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	found := false
	for _, tt := range tenants {
		if tt.Code == "pg-tenant" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pg-tenant not listed: %+v", tenants)
	}

	// 接入流水线（CreateFlow/AppendEvent 走 insertID，验 event 自增）
	flowID, err := db.CreateFlow(&Flow{ResourceID: "res-1", ResourceIP: "10.0.0.1", Mode: "wizard", TemplateID: "host", Status: "running", CurrentStep: "probe"})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	evtID, err := db.AppendEvent(&FlowEvent{FlowID: flowID, Step: "probe", Title: "探路", Status: "done", Scope: "platform"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if evtID <= 0 {
		t.Fatalf("event id should be >0, got %d", evtID)
	}
	evts, err := db.ListEvents(flowID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evts) != 1 || evts[0].Title != "探路" {
		t.Fatalf("events PG miswritten: %+v", evts)
	}
}