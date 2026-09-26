package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/logger"
)

func TestRegisterDoesNotAcknowledgeUnappliedConfig(t *testing.T) {
	c := testConfigResponseClient(t)
	if err := c.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.appliedVer != 0 {
		t.Fatalf("received-only config was acknowledged as applied version %d", c.appliedVer)
	}
}

func TestHeartbeatDoesNotAdvanceWithoutRuntimeApply(t *testing.T) {
	c := testConfigResponseClient(t)
	c.appliedVer = 3
	if !c.beat() {
		t.Fatal("heartbeat transport failed")
	}
	if c.appliedVer != 3 {
		t.Fatalf("runtime did not apply config, but applied version advanced to %d", c.appliedVer)
	}
}

func TestHeartbeatAcknowledgesOnlySuccessfulRuntimeApply(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "failure", false: "success"}[fail], func(t *testing.T) {
			c := testConfigResponseClient(t)
			c.appliedVer = 3
			c.sent["config_applied"] = true
			called := false
			c.SetConfigApplier(func(ctx context.Context, version int, content string) error {
				called = true
				if version != 7 || !json.Valid([]byte(content)) {
					t.Errorf("invalid application arguments: version=%d", version)
				}
				if fail {
					return errors.New("runtime switch failed")
				}
				return nil
			})
			if !c.beat() || !called {
				t.Fatal("successful heartbeat did not invoke runtime application")
			}
			want := 7
			if fail {
				want = 3
			}
			if c.appliedVer != want {
				t.Fatalf("applied=%d want=%d", c.appliedVer, want)
			}
			if !fail && c.sent["config_applied"] {
				t.Fatal("new applied version retained old report deduplication")
			}
		})
	}
}

func testConfigResponseClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "config_version": 7, "config_changed": true,
			"config": `{"targets":[],"host_metrics":{"enabled":true,"interval":"15s"}}`,
		})
	}))
	t.Cleanup(srv.Close)
	log, err := logger.New(t.TempDir(), "control-test.log", logger.INFO)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.Close)
	return NewClient(srv.URL, "resource-test", "test-version", nil, "", SelfDesc{}, log)
}

// hostPortFor 端口推导：无端口按 scheme 补 80/443，非法输入返回空串（不 panic）
func TestHostPortFor(t *testing.T) {
	cases := map[string]string{
		"http://l0-console:8080":       "l0-console:8080",
		"http://l0-console":            "l0-console:80",
		"https://l0.example.com":       "l0.example.com:443",
		"https://l0.example.com:8443/": "l0.example.com:8443",
		"http://10.1.2.3":              "10.1.2.3:80",
		"":                             "",
		"not a url":                    "",
		"http://":                      "",
	}
	for in, want := range cases {
		if got := hostPortFor(in); got != want {
			t.Errorf("hostPortFor(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 自描述缺省：plugins 归一为空切片（注册体应是 [] 而非 null）、type 空回落 edge
func TestSelfDescDefaults(t *testing.T) {
	c := NewClient("http://127.0.0.1:59999", "r1", "0.4.1", nil, "http://127.0.0.1:19090", SelfDesc{}, nil)
	if c.desc.Plugins == nil {
		t.Errorf("plugins 应为空切片而不是 nil")
	}
	if got := c.selfType(); got != "edge" {
		t.Errorf("type 缺省应回落 edge，实际 %q", got)
	}
	if got := NewClient("http://127.0.0.1:59999", "r1", "v", nil, "", SelfDesc{Type: "host"}, nil).selfType(); got != "host" {
		t.Errorf("type 应取配置值，实际 %q", got)
	}
}

// 心跳间隔分档（R5）：显式配置优先；未配时采集机（proxy）取 10s 短心跳，其余取 30s。
func TestHeartbeatEvery(t *testing.T) {
	mk := func(d SelfDesc) *Client {
		return NewClient("http://127.0.0.1:59999", "r1", "v", nil, "", d, nil)
	}
	if got := mk(SelfDesc{}).heartbeatEvery(); got != constants.DefaultHeartbeatInterval {
		t.Errorf("缺省应 30s，实际 %v", got)
	}
	if got := mk(SelfDesc{Type: "host"}).heartbeatEvery(); got != constants.DefaultHeartbeatInterval {
		t.Errorf("host 应 30s，实际 %v", got)
	}
	if got := mk(SelfDesc{Type: "proxy"}).heartbeatEvery(); got != constants.CollectorHeartbeatInterval {
		t.Errorf("proxy 应 10s 短心跳，实际 %v", got)
	}
	if got := mk(SelfDesc{Type: "collector"}).heartbeatEvery(); got != constants.CollectorHeartbeatInterval {
		t.Errorf("collector 应 10s 短心跳，实际 %v", got)
	}
	// 显式配置优先于类型默认
	if got := mk(SelfDesc{Type: "host", HeartbeatInterval: 15 * time.Second}).heartbeatEvery(); got != 15*time.Second {
		t.Errorf("显式配置应优先，实际 %v", got)
	}
}

// outboundIP 不依赖任何写死的网关地址：空 URL / 不可解析 URL 也不得 panic
func TestOutboundIPNoHardcodedGateway(t *testing.T) {
	for _, in := range []string{"", "not a url", "http://127.0.0.1:59999"} {
		ip := outboundIP(in)
		for _, r := range ip {
			if r == ' ' {
				t.Errorf("outboundIP(%q) 返回了空白：%q", in, ip)
			}
		}
	}
}

// splitURLs：逗号/空格/分号分隔的多端点（HA），空项剔除，空输入返回空切片
func TestSplitURLs(t *testing.T) {
	cases := map[string]int{
		"http://l0-a:8080":                1,
		"http://a:8080, http://b:8080 ":   2,
		"http://a:8080;http://b:8080,, ":  2,
		"":                                 0,
		"   ":                              0,
	}
	for in, want := range cases {
		if got := len(splitURLs(in)); got != want {
			t.Errorf("splitURLs(%q) = %d 项，期望 %d", in, got, want)
		}
	}
}

// 多端点 HA failover：主端点故障时自动切到备用端点，成功请求后回切主端点
func TestPostFailoverAcrossEndpoints(t *testing.T) {
	var primaryHits, backupHits int
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(backup.Close)

	log, err := logger.New(t.TempDir(), "failover-test.log", logger.ERROR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.Close)

	c := NewClient("http://127.0.0.1:1,"+backup.URL, "r1", "v", nil, "", SelfDesc{}, log)
	// 主端点(127.0.0.1:1)不可连→应成功后端点 failover
	var out map[string]any
	if err := c.post(context.Background(), "/api/heartbeat", []byte(`{}`), &out); err != nil {
		t.Fatalf("post should fail over to backup: %v", err)
	}
	if backupHits == 0 {
		t.Fatal("expected backup endpoint to be hit after primary failure")
	}
	if c.curIdx != 1 {
		t.Fatalf("curIdx=%d want 1 (selected backup)", c.curIdx)
	}

	// 主端点恢复后，下一次应回切到主端点
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(primary.Close)
	c.endpoints[0] = primary.URL
	out = map[string]any{}
	if err := c.post(context.Background(), "/api/heartbeat", []byte(`{}`), &out); err != nil {
		t.Fatalf("post should return to primary: %v", err)
	}
	if primaryHits == 0 {
		t.Fatal("expected primary endpoint to be hit after recovery")
	}
	if c.curIdx != 0 {
		t.Fatalf("curIdx=%d want 0 (back to primary)", c.curIdx)
	}
}

// 无端点时 post 应报错而非 panic
func TestPostNoEndpoints(t *testing.T) {
	log, err := logger.New(t.TempDir(), "noep-test.log", logger.ERROR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.Close)
	c := NewClient("", "r1", "v", nil, "", SelfDesc{}, log)
	if err := c.post(context.Background(), "/api/hb", []byte(`{}`), &map[string]any{}); err == nil {
		t.Fatal("expected error when no endpoints configured")
	}
}
