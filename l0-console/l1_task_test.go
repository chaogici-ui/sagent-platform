package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	store "github.com/sagent/l0-console/store"
)

// newL1TaskTestDB 开隔离 schema 的 PG catalog，并建 l1_tasks 表。
func newL1TaskTestDB(t *testing.T) *store.DB {
	t.Helper()
	return openTestCatalog(t)
}

func l1TaskServer(t *testing.T, db *store.DB) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerL1TaskRoutes(mux, db)
	return httptest.NewServer(mux)
}

func postJSON(t *testing.T, url string, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// TestL1TaskPullAckRoundtrip 验证「投递→拉取(一次性)→回执→拉取为空」完整闭环（DB 持久化）。
func TestL1TaskPullAckRoundtrip(t *testing.T) {
	db := newL1TaskTestDB(t)
	srv := l1TaskServer(t, db)
	defer srv.Close()

	q := postJSON(t, srv.URL+"/api/l1/task/queue", map[string]any{
		"controller_id": "l1-controller-1", "kind": "controller-ping", "payload": map[string]any{"echo": "hi"},
	})
	if q["status"] != "pending" {
		t.Fatalf("queue status = %v, want pending", q["status"])
	}

	// 拉取：拿到 1 个，status 变 dispatched
	p := postJSON(t, srv.URL+"/api/l1/task/pull", map[string]any{"controller_id": "l1-controller-1"})
	tasks, _ := p["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("pull len = %d, want 1", len(tasks))
	}
	if tasks[0].(map[string]any)["status"] != "dispatched" {
		t.Fatalf("pulled task status = %v, want dispatched", tasks[0].(map[string]any)["status"])
	}

	// 回执：控制器自处理 controller-ping → done/controller
	id := tasks[0].(map[string]any)["id"].(string)
	a := postJSON(t, srv.URL+"/api/l1/task/ack", map[string]any{
		"id": id, "controller_id": "l1-controller-1", "status": "done", "executor": "controller",
	})
	if a["status"] != "done" || a["executor"] != "controller" {
		t.Fatalf("ack = %v, want done/controller", a)
	}

	// 再拉取：已回执/已分发的任务不可重复拉取（幂等，避免重复执行）
	p2 := postJSON(t, srv.URL+"/api/l1/task/pull", map[string]any{"controller_id": "l1-controller-1"})
	if n := len(p2["tasks"].([]any)); n != 0 {
		t.Fatalf("re-pull len = %d, want 0 (must not re-dispatch)", n)
	}
}

// TestL1TaskQueueIdempotent 验证重复投递同一 id 不重复入队（DB 主键去重）。
func TestL1TaskQueueIdempotent(t *testing.T) {
	db := newL1TaskTestDB(t)
	srv := l1TaskServer(t, db)
	defer srv.Close()

	// 显式指定同 id 重复投递 → 只应有一条，不重复入队
	for i := 0; i < 3; i++ {
		_ = postJSON(t, srv.URL+"/api/l1/task/queue", map[string]any{
			"id": "task-dedup-1", "controller_id": "l1-controller-1", "kind": "install",
		})
	}
	rows, err := db.ListL1Tasks("l1-controller-1", 100)
	if err != nil {
		t.Fatalf("ListL1Tasks: %v", err)
	}
	if n := len(rows); n != 1 {
		t.Fatalf("duplicate-id queue created %d rows, want 1", n)
	}
}

// TestL1TaskPullIsolatesController 验证不同 Controller 只拉到自己的任务（DB where controller_id 隔离）。
func TestL1TaskPullIsolatesController(t *testing.T) {
	db := newL1TaskTestDB(t)
	srv := l1TaskServer(t, db)
	defer srv.Close()

	postJSON(t, srv.URL+"/api/l1/task/queue", map[string]any{"controller_id": "c1", "kind": "install"})
	postJSON(t, srv.URL+"/api/l1/task/queue", map[string]any{"controller_id": "c2", "kind": "controller-ping"})

	p := postJSON(t, srv.URL+"/api/l1/task/pull", map[string]any{"controller_id": "c1"})
	if n := len(p["tasks"].([]any)); n != 1 {
		t.Fatalf("c1 pull len = %d, want 1 (must not see c2 tasks)", n)
	}
}

// TestL1TaskPersistNoID 验证未显式指定 ID 时也能自动生成 ID 并入队（DB 真相源）。
func TestL1TaskPersistNoID(t *testing.T) {
	db := newL1TaskTestDB(t)
	srv := l1TaskServer(t, db)
	defer srv.Close()

	q := postJSON(t, srv.URL+"/api/l1/task/queue", map[string]any{
		"controller_id": "l1-ansible-runner-1", "kind": "install",
		"payload": map[string]any{"tag": "linux-amd64-0.4.0"},
	})
	if id, _ := q["id"].(string); id == "" {
		t.Fatalf("queue did not assign id")
	}
	// 落表校验
	rows, err := db.ListL1Tasks("l1-ansible-runner-1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("persisted rows = %d err=%v, want 1", len(rows), err)
	}
	if rows[0].Kind != "install" || rows[0].Status != "pending" {
		t.Fatalf("persisted kind/status = %s/%s, want install/pending", rows[0].Kind, rows[0].Status)
	}
	if got := store.UnmarshalPayload(rows[0].PayloadJSON)["tag"]; got != "linux-amd64-0.4.0" {
		t.Fatalf("persisted payload tag = %v, want linux-amd64-0.4.0", got)
	}
}

// TestControllerDispatchRouting 验证分流决策：self→controller/done，install→ansible，agent-*→gateway。
func TestControllerDispatchRouting(t *testing.T) {
	if exec, status, note := controllerDispatch("controller-ping", map[string]any{"echo": "ok"}); exec != "controller" || status != "done" {
		t.Fatalf("controller-ping -> %s/%s (note %s), want controller/done", exec, status, note)
	}
	if exec, status, _ := controllerDispatch("install", nil); exec != "ansible" || status != "executor_pending" {
		t.Fatalf("install -> %s/%s, want ansible/executor_pending", exec, status)
	}
	if exec, status, _ := controllerDispatch("ansible-install", nil); exec != "ansible" || status != "executor_pending" {
		t.Fatalf("ansible-install -> %s/%s, want ansible/executor_pending", exec, status)
	}
	if exec, status, _ := controllerDispatch("agent-action", nil); exec != "gateway" || status != "executor_pending" {
		t.Fatalf("agent-action -> %s/%s, want gateway/executor_pending", exec, status)
	}
	if exec, status, _ := controllerDispatch("bogus", nil); exec != "unknown" || status != "failed" {
		t.Fatalf("bogus -> %s/%s, want unknown/failed", exec, status)
	}
}