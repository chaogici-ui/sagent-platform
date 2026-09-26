package control

// g2_test.go —— G2 通道退役（通道 A：心跳拉取任务）在 SAgent 侧的执行/回执。
//
// 覆盖：localActionAt 的 start/stop/restart 标记语义（临时目录隔离），
// 以及 handleTask 消费一条 agent-action 于 /api/l1/task/ack 回执 done/sagent、
// 未知任务类回执 failed/controller。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sagent/core/internal/logger"
)

func TestLocalActionAtLifecycleMarkers(t *testing.T) {
	dir := t.TempDir()
	stop := filepath.Join(dir, "stopped")
	pid := filepath.Join(dir, "SAgent.pid")
	os.WriteFile(pid, []byte("99999"), 0o644) // 无该进程组，signalGroup best-effort 不报错

	// start（无标记文件→IsNotExist 视为成功）
	if st, note := localActionAt("start", stop, pid); st != "done" {
		t.Fatalf("start = %s (%s)", st, note)
	}
	os.WriteFile(stop, []byte("1"), 0o644)
	if st, _ := localActionAt("start", stop, pid); st != "done" {
		t.Fatalf("start with existing marker = %s, want done", st)
	}
	if _, err := os.Stat(stop); !os.IsNotExist(err) {
		t.Fatalf("start 应移除 stopped 标记，实际 err=%v", err)
	}

	// stop → 添加 stopped 标记
	if st, _ := localActionAt("stop", stop, pid); st != "done" {
		t.Fatalf("stop = %s", st)
	}
	if _, err := os.Stat(stop); err != nil {
		t.Fatalf("stop 应留下 stopped 标记，实际 err=%v", err)
	}

	// restart → 翻转标记（先置后清）
	if st, _ := localActionAt("restart", stop, pid); st != "done" {
		t.Fatalf("restart = %s", st)
	}
	if _, err := os.Stat(stop); !os.IsNotExist(err) {
		t.Fatalf("restart 应清除 stopped 标记，实际 err=%v", err)
	}

	// 未知动作 → failed
	if st, _ := localActionAt("deploy", stop, pid); st != "failed" {
		t.Fatalf("deploy = %s, want failed", st)
	}
}

// handleTask 消费 agent-action：本机执行（start）→ 回执 /api/l1/task/ack done/sagent。
func TestHandleTaskAgentActionAcceptsAndAcks(t *testing.T) {
	var ackBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/l1/task/ack" {
			buf := make([]byte, 0, 256)
			tmp := make([]byte, 256)
			for {
				n, err := r.Body.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			ackBody = buf
			_ = r.Body.Close()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)

	c := newG2Client(t, srv.URL)
	c.handleTask(context.Background(), map[string]any{
		"id": "task-1", "kind": "agent-action", "status": "dispatched",
		"payload": map[string]any{"action": "start"},
	})

	var ack struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Executor string `json:"executor"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatalf("ack body decode: %v (%s)", err, ackBody)
	}
	if ack.ID != "task-1" || ack.Status != "done" || ack.Executor != "sagent" || ack.Note == "" {
		t.Fatalf("ack = %+v, want id=task-1 done/sagent", ack)
	}
}

// 未知任务类 → 回执 failed/controller，避免任务滞留。
func TestHandleTaskUnknownKindAcksFailed(t *testing.T) {
	var acked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/l1/task/ack" {
			acked.Add(1)
			_ = r.Body.Close()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)

	c := newG2Client(t, srv.URL)
	c.handleTask(context.Background(), map[string]any{"id": "t2", "kind": "controller-ping"})
	if acked.Load() != 1 {
		t.Fatalf("ack calls = %d, want 1", acked.Load())
	}
}

func newG2Client(t *testing.T, l0 string) *Client {
	t.Helper()
	log, err := logger.New(t.TempDir(), "g2-test.log", logger.ERROR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.Close)
	return NewClient(l0, "g2-agent", "0.4.0", nil, "", SelfDesc{}, log)
}