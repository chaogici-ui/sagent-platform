package main

// l1_task.go —— L1 任务源（架构 D5 IN3：Controller 拉取/分流/回执 传输层）。
//
// 正式落地形态：任务源 PG 持久化——投递/拉取/回执均以 catalog 库
// l1_tasks 表为唯一真相源（store.QueueL1Task/PullL1Tasks/AckL1Task），
// 满足「任务源 PG 持久化任务表 + 切执行通道」随 G2 退役一次立项收口的落库要求：
//   - 投递幂等：同 id 不再入队（store 层靠主键去重）
//   - 拉取一次性：事务内 pending→dispatched，防重复执行
//   - 回执：落库终态 + 分流 executor
//   - 重启不丢：任务状态持久化于 catalog 库
//
// 三个接口（queue/pull/ack）供 L1 Controller/AnsibleRunner 经直连或 Gateway 白名单中继命中。

import (
	"encoding/json"
	"fmt"
	"net/http"

	store "github.com/sagent/l0-console/store"
)

// L1Task L1 Controller 可拉取任务（JSON 回传载荷层）。
type L1Task struct {
	ID           string         `json:"id"`
	ControllerID string         `json:"controller_id"`
	Kind         string         `json:"kind"`
	Payload      map[string]any `json:"payload"`
	Status       string         `json:"status"`
	Executor     string         `json:"executor"`
	Note         string         `json:"note"`
	// 流水线步骤绑定（IN5 B2）：执行端搬到 L1 后，L0 靠这三项把回执翻译回步骤终态。
	FlowID  int64  `json:"flow_id,omitempty"`
	StepID  string `json:"step_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// Result L1 执行端回传的结构化结果（facts/tasks/recap/diagnosis），随回执一并落库。
	Result    map[string]any `json:"result,omitempty"`
	CreatedAt int64          `json:"created_at"`
	AckedAt   int64          `json:"acked_at,omitempty"`
}

// rowToL1Task store 行 → 回传载荷（payload/result JSON→map）。
func rowToL1Task(r *store.L1TaskRow) L1Task {
	return L1Task{
		ID:           r.ID,
		ControllerID: r.ControllerID,
		Kind:         r.Kind,
		Payload:      store.UnmarshalPayload(r.PayloadJSON),
		Status:       r.Status,
		Executor:     r.Executor,
		Note:         r.Note,
		FlowID:       r.FlowID,
		StepID:       r.StepID,
		Attempt:      r.Attempt,
		Result:       store.UnmarshalPayload(r.ResultJSON),
		CreatedAt:    r.CreatedAt,
		AckedAt:      r.AckedAt,
	}
}

// registerL1TaskRoutes 注册 L1 任务源三接口（持久化 task 表驱动）。
// catDB 即 catalog 库（PG），queue/pull/ack 均落表。
func registerL1TaskRoutes(mux *http.ServeMux, catDB *store.DB) {
	// queue：投递一个面向 L1 Controller 的任务（幂等 by id，重复投递 SKIP 不退队列）
	mux.HandleFunc("/api/l1/task/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ID           string         `json:"id"`
			ControllerID string         `json:"controller_id"`
			Kind         string         `json:"kind"`
			Payload      map[string]any `json:"payload"`
			// 可选：绑定流水线步骤（IN5 B2）。带绑定的任务回执由对账桥翻译成步骤终态。
			FlowID  int64  `json:"flow_id"`
			StepID  string `json:"step_id"`
			Attempt int    `json:"attempt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if req.ControllerID == "" || req.Kind == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "controller_id and kind required"})
			return
		}
		row, created, err := catDB.QueueL1Task(store.L1TaskRow{
			ID:           req.ID,
			ControllerID: req.ControllerID,
			Kind:         req.Kind,
			PayloadJSON:  store.MarshalPayload(req.Payload),
			Status:       "pending",
			FlowID:       req.FlowID,
			StepID:       req.StepID,
			Attempt:      req.Attempt,
		})
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		addAudit("投递 L1 任务", row.ID, "L1 任务源", map[bool]string{true: "已入队", false: "重复投递跳过"}[created])
		writeJSON(w, map[string]any{"ok": true, "id": row.ID, "status": row.Status})
	})

	// pull：Controller 拉取自己名下的 pending 任务（一次性命中，事务内置 dispatched 防重）
	mux.HandleFunc("/api/l1/task/pull", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ControllerID string `json:"controller_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ControllerID == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "controller_id required"})
			return
		}
		rows, err := catDB.PullL1Tasks(req.ControllerID)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		tasks := make([]L1Task, 0, len(rows))
		for _, rw := range rows {
			tasks = append(tasks, rowToL1Task(rw))
		}
		writeJSON(w, map[string]any{"ok": true, "tasks": tasks})
	})

	// ack：Controller 回执任务终态（done/failed/executor_pending + 分流 executor）
	mux.HandleFunc("/api/l1/task/ack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ID           string         `json:"id"`
			ControllerID string         `json:"controller_id"`
			Status       string         `json:"status"`
			Executor     string         `json:"executor"`
			Note         string         `json:"note"`
			Result       map[string]any `json:"result"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "id required"})
			return
		}
		hit, err := catDB.AckL1Task(req.ID, req.Executor, req.Status, req.Note, store.MarshalPayload(req.Result))
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if !hit {
			writeJSON(w, map[string]any{"ok": false, "error": "task not found"})
			return
		}
		addAudit("L1 任务回执", req.ID, "L1 任务源", fmt.Sprintf("%s/%s", req.Status, req.Executor))
		writeJSON(w, map[string]any{"ok": true, "status": req.Status, "executor": req.Executor})
	})
}