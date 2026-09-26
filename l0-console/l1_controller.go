package main

// l1_controller.go —— L1 Controller 任务拉取/分流/回执 daemon（架构 D5 IN3 传输层）。
//
// 职责：作为独立的 L1 控制面进程（进程 #3），「反向连接 L0 拉任务 + 回执」（3.2-A 免入站）。
// 每周期直连 L0（`L0_CONSOLE_URL`）拉取自己名下的 pending 任务，按 kind 分流决策
// （ansible→AnsibleRunner / agent→经 Gateway 下发 / controller→自处理），并回执终态。
// 执行器（AnsibleRunner/Gateway 对 SAgent 的独立接线）属 IN4/G2 独立交付；
// 本进程完成 Controller 骨架 + 任务传输 + 分流决策，避免半成品。
//
// 网络域：**控制面唯一双挂进程**（l0-net + l1-net）。控制面流量在 L1 侧经 Gateway 汇聚后
// 由本进程出网，因此它还挂一个中继入口（`controllerRelayHandler`，白名单见 gateway.go）
// 承接 Gateway 转来的 SAgent 通道与 L1 内部组件的任务拉取——面向 SAgent 的中继进程不持有
// L0 可达，出口只有一个。
//
// 供 `l0-console -controller` 模式与 compose `l1-controller` 服务使用。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	cfgControllerID       = envOr("L1_CONTROLLER_ID", "l1-controller-1")
	cfgControllerListen   = envOr("L1_CONTROLLER_LISTEN", ":8461")
	cfgControllerLoopMS   = envOr("L1_CONTROLLER_LOOP_MS", "5000")
	cfgControllerUpstream = envOr("L0_CONSOLE_URL", "http://l0-console:8080")
)

var controllerClient = &http.Client{Timeout: 8 * time.Second}

// runControllerDaemon 独立 L1 Controller 进程：拉任务 → 分流 → 回执 循环 + /health，阻塞。
func runControllerDaemon() {
	loopMS, _ := strconv.Atoi(cfgControllerLoopMS)
	if loopMS <= 0 {
		loopMS = 5000
	}
	fmt.Printf("L1 Controller daemon starting id=%s upstream=%s loop=%dms\n", cfgControllerID, cfgControllerUpstream, loopMS)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "role": "l1-controller", "id": cfgControllerID, "upstream": cfgControllerUpstream})
	})
	// 控制面唯一出口：承接 Gateway 汇聚来的 SAgent 通道 + L1 内部组件的任务拉取，转发 L0。
	mux.HandleFunc("/", controllerRelayHandler)
	go func() {
		log.Fatal(http.ListenAndServe(cfgControllerListen, mux))
	}()

	base := strings.TrimSuffix(cfgControllerUpstream, "/")
	pullURL := base + "/api/l1/task/pull"
	ackURL := base + "/api/l1/task/ack"

	ticker := time.NewTicker(time.Duration(loopMS) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		tasks := controllerPull(pullURL)
		if len(tasks) == 0 {
			continue
		}
		log.Printf("[controller] pulled %d pending task(s)", len(tasks))
		for _, t := range tasks {
			exec, status, note := controllerDispatch(t.Kind, t.Payload)
			controllerAck(ackURL, t.ID, status, exec, note)
			log.Printf("[controller] task %s kind=%s -> executor=%s status=%s note=%q", t.ID, t.Kind, exec, status, note)
		}
	}
}

// controllerPull 拉取自己名下的 pending 任务。
func controllerPull(url string) []L1Task {
	return l1PullTasks(url, cfgControllerID)
}

// controllerAck 回执任务终态到 L0（Controller 只做分流决策，无结构化结果）。
func controllerAck(url, id, status, exec, note string) {
	l1AckTask(url, cfgControllerID, id, status, exec, note, nil)
}

// l1PullTasks 通用：以某 L1 身份 pull 名下待处理任务（供 Controller/AnsibleRunner 复用）。
func l1PullTasks(url, identity string) []L1Task {
	body, _ := json.Marshal(map[string]any{"controller_id": identity})
	resp, err := controllerClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[l1:%s] pull error: %v", identity, err)
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool     `json:"ok"`
		Tasks []L1Task `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.OK == false {
		return nil
	}
	return out.Tasks
}

// l1AckTask 通用：以某 L1 身份回执任务终态（供 Controller/AnsibleRunner 复用）。
// result 为执行端回传的结构化结果（facts/tasks/recap/diagnosis…）——
// 只回 note 字符串会让探路 facts、逐任务回执这类结构化结论断链（SPEC D2）。
func l1AckTask(url, identity, id, status, exec, note string, result map[string]any) {
	body, _ := json.Marshal(map[string]any{
		"id": id, "controller_id": identity,
		"status": status, "executor": exec, "note": note, "result": result,
	})
	resp, err := controllerClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[l1:%s] ack %s error: %v", identity, id, err)
		return
	}
	resp.Body.Close()
}

// controllerDispatch 分流决策：kind → {executor, status, note}。
func controllerDispatch(kind string, payload map[string]any) (exec, status, note string) {
	e := payload["echo"]
	switch {
	case kind == "controller-ping":
		// 本进程可自处理的诊断型任务：真实执行并回执 done
		return "controller", "done", fmt.Sprintf("pong: %v", e)
	case strings.HasPrefix(kind, "ansible-") || kind == "install":
		// 分流到 Ansible Runner（IN4 独立执行器接线后执行，本进程只做传输+分流决策）
		return "ansible", "executor_pending", "AnsibleRunner 独立进程(IN4)接线后执行"
	case strings.HasPrefix(kind, "agent-") || kind == "action":
		// 分流到经 Gateway 下发 SAgent（G2 接线后执行）
		return "gateway", "executor_pending", "经 Gateway 下发 SAgent(G2)接线后执行"
	default:
		return "unknown", "failed", fmt.Sprintf("unknown kind %q", kind)
	}
}
