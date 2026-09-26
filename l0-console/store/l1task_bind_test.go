package store

// l1task_bind_test.go —— IN5 B2：任务与步骤绑定 + 结构化结果 + 对账消费标记。
// 环境隔离：每用例独立 PG schema（testpg.Provision），不与真实库互相污染。

import (
	"testing"

	"github.com/sagent/l0-console/internal/testpg"
)

// TestInitL1TasksMigratesLegacyTable 存量表（无绑定列）跑 InitL1Tasks 必须补齐新列——
// 生产库走 ALTER 而非重建，漏了这一步会让回执对账桥在真库上直接报 no such column。
func TestInitL1TasksMigratesLegacyTable(t *testing.T) {
	db, err := OpenPostgres(testpg.Provision(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// IN5 之前的表结构（无 flow_id/step_id/attempt/result/reconciled_at）
	if _, err := db.db.Exec(`CREATE TABLE l1_tasks (
		id TEXT PRIMARY KEY,
		controller_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		payload TEXT DEFAULT '',
		status TEXT DEFAULT 'pending',
		executor TEXT DEFAULT '',
		note TEXT DEFAULT '',
		created_at INTEGER DEFAULT 0,
		acked_at INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks on legacy table: %v", err)
	}
	// 幂等：重复启动不报错（探测列后再 ALTER）
	if err := db.InitL1Tasks(); err != nil {
		t.Fatalf("InitL1Tasks second run: %v", err)
	}

	row, created, err := db.QueueL1Task(L1TaskRow{
		ID: "legacy-1", ControllerID: "c1", Kind: "install",
		FlowID: 42, StepID: "install_agent", Attempt: 3,
	})
	if err != nil || !created {
		t.Fatalf("queue on migrated table: created=%v err=%v", created, err)
	}
	if row.FlowID != 42 || row.StepID != "install_agent" || row.Attempt != 3 {
		t.Fatalf("绑定未落库: %+v", row)
	}
}

// TestL1TaskBindingAndReconcileScan 绑定与结果落库，且对账扫描只捞「已绑定 + 已终态 + 未消费」。
func TestL1TaskBindingAndReconcileScan(t *testing.T) {
	db := openTestDB(t)

	// ① 未绑定（心跳/探针类）→ 不参与对账
	mustQueue(t, db, "unbound", "pending", 0)
	if _, err := db.AckL1Task("unbound", "controller", "done", "ping ok", ""); err != nil {
		t.Fatalf("ack unbound: %v", err)
	}
	// ② 已绑定 + done + 带结构化结果
	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "bound-done", ControllerID: "l1-ansible-runner-1",
		Kind: "install", FlowID: 7, StepID: "install_agent", Attempt: 1}); err != nil {
		t.Fatalf("queue bound-done: %v", err)
	}
	if _, err := db.AckL1Task("bound-done", "ansible", "done", "装好了", `{"installed_tag":"v1"}`); err != nil {
		t.Fatalf("ack bound-done: %v", err)
	}
	// ③ 已绑定但 executor_pending（下游执行器未收口）→ 不驱动步骤终态
	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "bound-pending", ControllerID: "l1-ansible-runner-1",
		Kind: "install", FlowID: 7, StepID: "install_agent", Attempt: 1}); err != nil {
		t.Fatalf("queue bound-pending: %v", err)
	}
	if _, err := db.AckL1Task("bound-pending", "ansible", "executor_pending", "已转发", ""); err != nil {
		t.Fatalf("ack bound-pending: %v", err)
	}
	// ④ 已绑定 + failed
	if _, _, err := db.QueueL1Task(L1TaskRow{ID: "bound-failed", ControllerID: "l1-ansible-runner-1",
		Kind: "install", FlowID: 8, StepID: "install_agent", Attempt: 2}); err != nil {
		t.Fatalf("queue bound-failed: %v", err)
	}
	if _, err := db.AckL1Task("bound-failed", "ansible", "failed", "sha 不一致", `{"diagnosis":[]}`); err != nil {
		t.Fatalf("ack bound-failed: %v", err)
	}

	rows, err := db.ListReconcilableL1Tasks(0)
	if err != nil {
		t.Fatalf("ListReconcilableL1Tasks: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if len(rows) != 2 || !got["bound-done"] || !got["bound-failed"] {
		t.Fatalf("reconcilable = %v, want {bound-done, bound-failed}", got)
	}

	// 结构化结果落库可读
	r, _ := db.GetL1Task("bound-done")
	if r == nil || UnmarshalPayload(r.ResultJSON)["installed_tag"] != "v1" {
		t.Fatalf("result 未落库: %+v", r)
	}

	// 消费后不再被捞；标记本身幂等（时间戳只置一次，重复调用不报错）
	if err := db.MarkL1TaskReconciled("bound-done"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := db.MarkL1TaskReconciled("bound-done"); err != nil {
		t.Fatalf("mark again: %v", err)
	}
	rows2, _ := db.ListReconcilableL1Tasks(0)
	if len(rows2) != 1 || rows2[0].ID != "bound-failed" {
		t.Fatalf("消费后 = %d 行（%v），want 仅 bound-failed", len(rows2), rows2)
	}
	if r2, _ := db.GetL1Task("bound-done"); r2.ReconciledAt == 0 {
		t.Fatalf("reconciled_at 未置位")
	}
}