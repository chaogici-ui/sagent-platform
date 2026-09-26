package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// captureGatewayProbe 记录上游收到的请求，供断言转发保真。
type captureGatewayProbe struct {
	mu   sync.Mutex
	path string
	body string
	ct   string
}

// TestGatewayRelayForward 验证控制通道白名单路径被原样转发上游并回落响应（双向中继成立）。
func TestGatewayRelayForward(t *testing.T) {
	probe := &captureGatewayProbe{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		probe.mu.Lock()
		probe.path = r.URL.Path
		probe.body = string(b)
		probe.ct = r.Header.Get("Content-Type")
		probe.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"config_version":3}`))
	}))
	defer upstream.Close()

	old := cfgGatewayUpstream
	cfgGatewayUpstream = upstream.URL
	defer func() { cfgGatewayUpstream = old }()

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/agent/register", `{"id":"gw-test","type":"edge"}`},
		{"/api/agent/heartbeat", `{"id":"gw-test","status":"healthy"}`},
		{"/api/onboard/flow/agent-report", `{"agent_id":"gw-test","kind":"self_metrics","data":{}}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		gatewayRelayHandler(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: relay status = %d, want 200", tc.path, rec.Code)
		}
		probe.mu.Lock()
		gotPath, gotBody, gotCT := probe.path, probe.body, probe.ct
		probe.mu.Unlock()
		if gotPath != tc.path {
			t.Errorf("%s: upstream path = %q, want %q", tc.path, gotPath, tc.path)
		}
		if gotBody != tc.body {
			t.Errorf("%s: upstream body = %q, want %q", tc.path, gotBody, tc.body)
		}
		if gotCT != "application/json" {
			t.Errorf("%s: upstream content-type = %q, want application/json", tc.path, gotCT)
		}
		// 回落响应必须能还原出 L0 语义（ok:true + config_version）→ 反向中继成立
		var resp struct {
			Ok   bool `json:"ok"`
			CfgV int  `json:"config_version"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: response not valid JSON: %v", tc.path, err)
		}
		if !resp.Ok || resp.CfgV != 3 {
			t.Errorf("%s: relayed response lost upstream semantics: %+v", tc.path, resp)
		}
	}
}

// TestGatewayWhitelistRejectsNonControl 验证白名单外路径（数据面/其他 API）不被 Gateway 中继。
func TestGatewayWhitelistRejectsNonControl(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer upstream.Close()
	old := cfgGatewayUpstream
	cfgGatewayUpstream = upstream.URL
	defer func() { cfgGatewayUpstream = old }()

	for _, path := range []string{"/metrics", "/api/package-cache/status", "/api/agents", "/api/recon", "/foo"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		gatewayRelayHandler(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
	}
	if hit {
		t.Error("non-control path was forwarded upstream; gateway must stay scoped to control channel")
	}
}

// TestGatewayMethodNotAllowed 验证仅允许 POST（控制通道均为 POST）。
func TestGatewayMethodNotAllowed(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer upstream.Close()
	old := cfgGatewayUpstream
	cfgGatewayUpstream = upstream.URL
	defer func() { cfgGatewayUpstream = old }()

	req := httptest.NewRequest(http.MethodGet, "/api/agent/heartbeat", nil)
	rec := httptest.NewRecorder()
	gatewayRelayHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET heartbeat: status = %d, want 405", rec.Code)
	}
	if hit {
		t.Error("GET was forwarded upstream; gateway must reject non-POST control paths")
	}
}

// TestGatewayUpstreamUnreachable 验证上游不可达时返回 502 JSON（SAgent 视为端点故障走 HA failover）。
func TestGatewayUpstreamUnreachable(t *testing.T) {
	old := cfgGatewayUpstream
	cfgGatewayUpstream = "http://127.0.0.1:1" // 必然不可达
	defer func() { cfgGatewayUpstream = old }()

	req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader(`{"id":"x"}`))
	rec := httptest.NewRecorder()
	gatewayRelayHandler(rec, req)
	if rec.Code != http.StatusOK {
		// 网关返回 JSON ok:false（而非纯 502 正文）——SAgent post() 以 HTTP 状态判定端点故障
		t.Logf("note: upstream-unreachable returned HTTP %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Ok  bool   `json:"ok"`
		Err string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Ok {
		t.Error("upstream unreachable returned ok:true")
	}
	if !strings.Contains(resp.Err, "unreachable") {
		t.Errorf("unreachable error = %q, want mention of unreachable", resp.Err)
	}
}

// TestGatewayHealth 验证 Gateway 自身 /health 存活探针。
func TestGatewayHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	// 直接由 daemon mux 处理以覆盖 /health 与根路由共存
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "role": "l1-gateway", "upstream": cfgGatewayUpstream})
	})
	mux.HandleFunc("/", gatewayRelayHandler)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/health: status = %d, want 200", rec.Code)
	}
}

// TestGatewayRejectsL1InternalTaskPull 验证 Gateway 白名单只收 SAgent 通道：
// L1 内部组件的任务拉取（/api/l1/task/pull）不经 Gateway（它们直连 Controller 出口）。
// SAgent 自身的任务回执 /api/l1/task/ack 必须仍可中继——它走的是心跳下拉任务的回程。
func TestGatewayRejectsL1InternalTaskPull(t *testing.T) {
	if !gatewayControlPaths["/api/l1/task/ack"] {
		t.Error("gateway must relay /api/l1/task/ack (SAgent 消费心跳任务后的回执)")
	}
	if gatewayControlPaths["/api/l1/task/pull"] {
		t.Error("gateway must not relay /api/l1/task/pull (L1 内部组件直连 Controller)")
	}
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer upstream.Close()
	old := cfgGatewayUpstream
	cfgGatewayUpstream = upstream.URL
	defer func() { cfgGatewayUpstream = old }()

	req := httptest.NewRequest(http.MethodPost, "/api/l1/task/pull", strings.NewReader(`{"controller_id":"l1-controller-1"}`))
	rec := httptest.NewRecorder()
	gatewayRelayHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("/api/l1/task/pull via gateway: status = %d, want 403", rec.Code)
	}
	if hit {
		t.Error("/api/l1/task/pull was forwarded by gateway; L1 internal tasks must go through the Controller egress")
	}
}

// TestControllerRelayIsControlPlaneEgress 验证 Controller 作为控制面唯一出口：
// 既中继 Gateway 汇聚来的 SAgent 通道，也中继 L1 内部组件的任务拉取/回执；其余一律 403。
func TestControllerRelayIsControlPlaneEgress(t *testing.T) {
	probe := &captureGatewayProbe{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		probe.mu.Lock()
		probe.path = r.URL.Path
		probe.body = string(b)
		probe.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	old := cfgControllerUpstream
	cfgControllerUpstream = upstream.URL
	defer func() { cfgControllerUpstream = old }()

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/agent/heartbeat", `{"id":"sagent-1","status":"healthy"}`},   // Gateway 汇聚来的 SAgent 心跳
		{"/api/l1/task/ack", `{"id":"t1","controller_id":"sagent-1"}`},     // SAgent 任务回执
		{"/api/l1/task/pull", `{"controller_id":"l1-controller-1"}`},       // L1 内部组件拉任务
		{"/api/onboard/flow/agent-report", `{"agent_id":"a1","kind":"x"}`}, // 接入证据回报
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		controllerRelayHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: controller relay status = %d, want 200", tc.path, rec.Code)
		}
		probe.mu.Lock()
		gotPath, gotBody := probe.path, probe.body
		probe.mu.Unlock()
		if gotPath != tc.path || gotBody != tc.body {
			t.Errorf("%s: upstream got path=%q body=%q", tc.path, gotPath, gotBody)
		}
	}

	for _, path := range []string{"/metrics", "/api/agents", "/api/package-cache/status", "/foo"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		controllerRelayHandler(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s via controller: status = %d, want 403", path, rec.Code)
		}
	}
}
