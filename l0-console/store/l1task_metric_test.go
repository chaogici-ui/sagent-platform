package store

// l1task_metric_test.go —— OBS-1(L0 半边)/PLAN 3.3：L1 任务队列可观测汇总
// L1TaskMetrics 的队列深度(pending+dispatched)、done/failed 计数、有回执任务平均时延。
// 环境隔离：openTestDB 每用例独立 PG schema（见 store_test.go），不与真实库互相污染。

import (
	"testing"
	"time"
)

func mustQueue(t *testing.T, db *DB, id, status string, createdAt int64) {
	t.Helper()
	_, _, err := db.QueueL1Task(L1TaskRow{
		ID: id, ControllerID: "ctl-1", Kind: "agent-action",
		PayloadJSON: `{"id":"` + id + `"}`,
		Status:      status, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("queue %s: %v", id, err)
	}
}

func TestL1TaskMetrics(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Unix()

	// 2 个待回执：1 pending + 1 dispatched → 队列深度 2
	mustQueue(t, db, "t1", "pending", now)
	mustQueue(t, db, "t2", "dispatched", now)
	// 1 个 done（创建于 5s 前，回执在 now 写 acked_at → 平均时延约 5000ms）
	mustQueue(t, db, "t3", "pending", now-5)
	if ok, err := db.AckL1Task("t3", "sagent-1", "done", "", ""); err != nil || !ok {
		t.Fatalf("ack t3: ok=%v err=%v", ok, err)
	}
	// 1 个 failed
	mustQueue(t, db, "t5", "failed", now-1)
	if ok, err := db.AckL1Task("t5", "sagent-1", "failed", "boom", ""); err != nil || !ok {
		t.Fatalf("ack t5: ok=%v err=%v", ok, err)
	}

	m, err := db.L1TaskMetrics()
	if err != nil {
		t.Fatalf("L1TaskMetrics: %v", err)
	}
	if m.Pending != 2 {
		t.Fatalf("queue depth want 2 (pending+dispatched), got %d", m.Pending)
	}
	if m.OK != 1 {
		t.Fatalf("done want 1, got %d", m.OK)
	}
	if m.Failed != 1 {
		t.Fatalf("failed want 1, got %d", m.Failed)
	}
	// 有回执任务：t3(done 5000ms) + t5(failed 1000ms) → avg = 3000ms
	if m.AvgAckMs < 2500 || m.AvgAckMs > 6000 {
		t.Fatalf("avg ack latency want ~3000ms, got %d", m.AvgAckMs)
	}
	if m.AckedCount != 2 {
		t.Fatalf("acked want 2, got %d", m.AckedCount)
	}
}

func TestL1TaskMetricsEmpty(t *testing.T) {
	db := openTestDB(t)
	m, err := db.L1TaskMetrics()
	if err != nil {
		t.Fatalf("empty metrics: %v", err)
	}
	if m.Pending != 0 || m.OK != 0 || m.Failed != 0 || m.AvgAckMs != 0 {
		t.Fatalf("empty table should all be 0, got %+v", m)
	}
}