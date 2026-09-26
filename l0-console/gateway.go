package main

// gateway.go —— L1 控制通道中继（架构 D5 IN2）与 L1 Controller 出口中继（网络域收敛）。
//
// 控制面出口唯一：**只有 L1 Controller 双挂 l0-net + l1-net**（架构红线：控制面流量经 L1
// 采集机汇聚后再与 L0 通信）。因此这里有两个中继角色、同一套转发实现：
//
//   - Gateway（`l1-gateway`，只挂 l1-net）：面向 SAgent 的汇聚中继。本资源池 SAgent 只打通到
//     本 L1 Gateway（`L1_GATEWAY_LISTEN`），Gateway 把注册/心跳/接入证据转发给上游 Controller
//     （`L1_CONTROLLER_URL`）——Gateway 自身**不持有 L0 可达**，面向半可信 SAgent 的中继与
//     面向 L0 的出口分离，攻击面更小。
//   - Controller（`l1-controller`，双挂）：控制面唯一出口。既自拉任务/回执（`L0_CONSOLE_URL`），
//     也把 Gateway 汇聚来的 SAgent 流量、以及 L1 内部组件（AnsibleRunner）的任务拉取转发到 L0。
//
// 形态：无状态反向中继（仅瞬态 buffer，不落库），只转发控制通道白名单路径，杜绝把数据面
// /metrics 等暴露给本池之外。

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// cfgGatewayListen 独立 Gateway 进程监听地址（L1_GATEWAY_LISTEN，默认 :8460）。
var cfgGatewayListen = envOr("L1_GATEWAY_LISTEN", ":8460")

// cfgGatewayUpstream Gateway 中继的上游：L1 Controller（L1_CONTROLLER_URL，默认 http://l1-controller:8461）。
// 不是 L0——控制面出口已收敛到 Controller，Gateway 只做采集机内网汇聚。
var cfgGatewayUpstream = envOr("L1_CONTROLLER_URL", "http://l1-controller:8461")

// gatewayControlPaths Gateway 可中继的 SAgent 控制通道白名单（SAgent control/client.go 的调用面）。
// 只收 SAgent 通道：L1 内部组件（Controller/AnsibleRunner）直连 Controller，不经 Gateway。
var gatewayControlPaths = map[string]bool{
	"/api/agent/register":            true, // 首装后回调注册
	"/api/agent/heartbeat":           true, // 周期心跳 + 配置/任务拉取
	"/api/onboard/flow/agent-report": true, // 接入证据回报（接入中心批 C）
	"/api/l1/task/ack":               true, // SAgent 消费心跳下发任务后的回执（control/client.go handleTask）
}

// controllerControlPaths Controller 作为控制面唯一出口可中继的白名单：
// SAgent 通道（经 Gateway 汇聚而来）+ L1 内部任务通道（Controller/AnsibleRunner 的拉取与回执）。
var controllerControlPaths = func() map[string]bool {
	m := map[string]bool{}
	for k := range gatewayControlPaths {
		m[k] = true
	}
	m["/api/l1/task/pull"] = true // L1 Controller/AnsibleRunner 拉任务（架构 D5 IN3）：只走 Controller
	return m
}()

var gatewayHTTPClient = &http.Client{Timeout: 8 * time.Second}

// runGatewayDaemon 独立 Gateway 进程入口：不起 DB、不起 console，仅起 SAgent 通道中继并阻塞。
// 供 `l0-console -gateway` 模式与 compose `l1-gateway` 服务使用。
func runGatewayDaemon() {
	fmt.Printf("L1 Gateway daemon starting on %s (upstream=%s)\n", cfgGatewayListen, cfgGatewayUpstream)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "role": "l1-gateway", "upstream": cfgGatewayUpstream})
	})
	mux.HandleFunc("/", gatewayRelayHandler)
	log.Fatal(http.ListenAndServe(cfgGatewayListen, mux))
}

// gatewayRelayHandler Gateway 侧中继：只放 SAgent 控制通道，上游为 L1 Controller。
func gatewayRelayHandler(w http.ResponseWriter, r *http.Request) {
	relayControlUpstream(w, r, "gateway", gatewayControlPaths, cfgGatewayUpstream)
}

// controllerRelayHandler Controller 侧中继（控制面唯一出口）：上游为 L0 Console。
func controllerRelayHandler(w http.ResponseWriter, r *http.Request) {
	relayControlUpstream(w, r, "controller", controllerControlPaths, cfgControllerUpstream)
}

// relayControlUpstream 控制通道反向中继：白名单内路径原样转发上游并回落，其余拒绝。
// role 仅用于日志前缀，便于在两个进程上分辨是哪一跳。
func relayControlUpstream(w http.ResponseWriter, r *http.Request, role string, paths map[string]bool, upstreamBase string) {
	path := r.URL.Path
	if !paths[path] {
		http.Error(w, "only L1 control channel paths are relayed", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": role + " read body: " + err.Error()})
		return
	}
	if len(body) > 4<<20 {
		writeJSON(w, map[string]any{"ok": false, "error": role + " body too large"})
		return
	}

	upstream := strings.TrimSuffix(upstreamBase, "/") + path
	req, err := http.NewRequest(http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": role + " build upstream request: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gatewayHTTPClient.Do(req)
	if err != nil {
		// 上游不可达 → 中继侧判为不可中继，返回 JSON ok:false（SAgent 视为端点故障走 HA failover）
		log.Printf("[%s] upstream unreachable %s: %v", role, upstream, err)
		writeJSON(w, map[string]any{"ok": false, "error": role + " upstream unreachable"})
		return
	}
	defer resp.Body.Close()

	// 原样回落：状态码 + Content-Type + 上游响应体（含 L0 语义 ok/error）
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("[%s] relay write %s aborted: %v", role, path, err)
	}
	log.Printf("[%s] relay %s -> %d (%s)", role, path, resp.StatusCode, r.RemoteAddr)
}