package main

// g2_test.go —— G2 通道退役（控制通道收敛到通道 A：心跳拉取任务）的交付级测试。
//
// 覆盖：store 层 PullAgentActions 的 kind 过滤/一次性/隔离，
// 以及 /api/action 对心跳型 SAgent 走通道 A（投递 agent-action 任务）→
// 心跳响应 tasks 注入 → 回执 done 的完整闭环。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// g2TestDB 建隔离 schema 的 PG catalog（Fleet+Onboard+L1Tasks 齐备，供 agents + 任务源共用）。
func g2TestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	return openTestCatalog(t)
}

func g2gServer(t *testing.T, db *storepkg.DB, store *AgentStore) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerAgentRoutes(mux, store, db)
	registerL1TaskRoutes(mux, db)
	return httptest.NewServer(mux)
}

// TestStorePullAgentActions 验证 PullAgentActions 只取 kind='agent-action'、一次性、跨 Agent 隔离。
func TestStorePullAgentActions(t *testing.T) {
	db := g2TestDB(t)

	// 同 agent 两种任务：agent-action + 其它 kind（控制器任务）
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{ID: "aa-1", ControllerID: "agent-x", Kind: "agent-action", PayloadJSON: `{"action":"restart"}`, Status: "pending"}); err != nil {
		t.Fatalf("Queue aa-1: %v", err)
	}
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{ID: "cp-1", ControllerID: "agent-x", Kind: "controller-ping", PayloadJSON: `{}`, Status: "pending"}); err != nil {
		t.Fatalf("Queue cp-1: %v", err)
	}
	// 其它 agent 的 agent-action（不得串扰）
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{ID: "aa-other", ControllerID: "agent-y", Kind: "agent-action", PayloadJSON: `{"action":"stop"}`, Status: "pending"}); err != nil {
		t.Fatalf("Queue aa-other: %v", err)
	}

	tasks, err := db.PullAgentActions("agent-x")
	if err != nil {
		t.Fatalf("PullAgentActions: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "aa-1" {
		t.Fatalf("pulled = %+v, want only aa-1", tasks)
	}
	if tasks[0].Status != "dispatched" {
		t.Fatalf("status = %v, want dispatched", tasks[0].Status)
	}

	// 一次性：重复拉取不重发
	if again, _ := db.PullAgentActions("agent-x"); len(again) != 0 {
		t.Fatalf("re-pull = %d, want 0", len(again))
	}
	// controller-ping 仍 pending（未被 agent-action 拉取误带走），其它 agent 任务不受影响
	cp, _ := db.GetL1Task("cp-1")
	if cp.Status != "pending" {
		t.Fatalf("cp-1 status = %v, want pending (kind filter broken)", cp.Status)
	}
	other, _ := db.GetL1Task("aa-other")
	if other.Status != "pending" {
		t.Fatalf("aa-other status = %v, want pending (cross-agent isolation broken)", other.Status)
	}
}

// TestG2ActionRoutesHeartbeatToChannelA 验证 /api/action 对心跳型 SAgent 走通道 A 全闭环：
// 投递 agent-action → 心跳 tasks 注入（dispatched）→ 回执 done → 落库终态。
func TestG2ActionRoutesHeartbeatToChannelA(t *testing.T) {
	db := g2TestDB(t)
	store := NewAgentStore()

	// 预置一个心跳型真实 SAgent（DB source=heartbeat + 内存台账）
	const agentID = "g2-heartbeat-agent"
	if err := db.UpsertAgentRow(&storepkg.AgentRow{ID: agentID, Name: agentID, Source: "heartbeat", LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("UpsertAgentRow: %v", err)
	}
	store.Put(&Agent{ID: agentID, Name: agentID, Type: "edge", Status: "healthy", Source: "heartbeat"})

	srv := g2gServer(t, db, store)
	defer srv.Close()

	// ① /api/action → 心跳型走通道 A：queued，不 docker exec
	act := postJSON(t, srv.URL+"/api/action", map[string]any{"agent_id": agentID, "action": "restart"})
	if act["channel"] != "a" || act["queued"] != true {
		t.Fatalf("/api/action = %v, want channel=a queued=true", act)
	}
	taskID, _ := act["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task_id empty")
	}

	// ② 心跳 → tasks 注入该 agent-action，状态 dispatched（一次带走，不重发）
	hb := postJSON(t, srv.URL+"/api/agent/heartbeat", map[string]any{"id": agentID, "version": "0.4.0", "stats": map[string]any{}})
	tasks, _ := hb["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("heartbeat tasks len = %d, want 1", len(tasks))
	}
	t0 := tasks[0].(map[string]any)
	if t0["id"] != taskID || t0["kind"] != "agent-action" || t0["status"] != "dispatched" {
		t.Fatalf("task = %v, want id=%s kind=agent-action dispatched", t0, taskID)
	}

	// 再心跳：无新任务（一次性）
	hb2 := postJSON(t, srv.URL+"/api/agent/heartbeat", map[string]any{"id": agentID})
	if n := len(hb2["tasks"].([]any)); n != 0 {
		t.Fatalf("re-heartbeat tasks len = %d, want 0", n)
	}

	// ③ 回执 done/sagent → 落库终态
	ack := postJSON(t, srv.URL+"/api/l1/task/ack", map[string]any{
		"id": taskID, "controller_id": agentID, "status": "done", "executor": "sagent", "note": "SAgent restart",
	})
	if ack["status"] != "done" || ack["executor"] != "sagent" {
		t.Fatalf("ack = %v, want done/sagent", ack)
	}
	row, err := db.GetL1Task(taskID)
	if err != nil || row == nil {
		t.Fatalf("GetL1Task(%s) err=%v row=%v", taskID, err, row)
	}
	if row.Status != "done" || row.Executor != "sagent" {
		t.Fatalf("persisted = %s/%s, want done/sagent", row.Status, row.Executor)
	}
}

// TestG2ActionRoutesDockerKeepsChannelB 验证 docker 源演示 Agent 仍在 /api/action 走通道 B（executeAction 路径保留）。
// docker exec 在 CI 无 docker 环境会失败，此处仅断言未走通道 A（未产生 agent-action 任务）。
func TestG2ActionRoutesDockerKeepsChannelB(t *testing.T) {
	t.Parallel()
	db := g2TestDB(t)
	store := NewAgentStore()

	const agentID = "sagent-demo-1"
	if err := db.UpsertAgentRow(&storepkg.AgentRow{ID: agentID, Name: agentID, Source: "docker", LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("UpsertAgentRow: %v", err)
	}
	store.Put(&Agent{ID: agentID, Name: agentID, Type: "edge", Status: "healthy", Source: "docker"})

	srv := g2gServer(t, db, store)
	defer srv.Close()

	_ = postJSON(t, srv.URL+"/api/action", map[string]any{"agent_id": agentID, "action": "start"})

	// docker 源不得产生心跳拉取任务（通道 A 是心跳型专属）
	rows, err := db.ListL1Tasks(agentID, 10)
	if err != nil {
		t.Fatalf("ListL1Tasks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("docker agent produced %d agent-action tasks, want 0", len(rows))
	}
}

// TestStorePullAgentActionsPersist helper 防误用（占位编译守卫，无需语义）。
func TestStorePullAgentActionsJSON(t *testing.T) {
	db := g2TestDB(t)
	if _, _, err := db.QueueL1Task(storepkg.L1TaskRow{ID: "aa-json", ControllerID: "a1", Kind: "agent-action", PayloadJSON: storepkg.MarshalPayload(map[string]any{"action": "start"}), Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.PullAgentActions("a1")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pull = %d err=%v", len(tasks), err)
	}
	payload := storepkg.UnmarshalPayload(tasks[0].PayloadJSON)
	if payload["action"] != "start" {
		t.Fatalf("action = %v, want start", payload["action"])
	}
}