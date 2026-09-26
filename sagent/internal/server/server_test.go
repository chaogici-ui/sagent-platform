package server

import (
	"net/http/httptest"
	"testing"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/resource"
)

type nopLogger struct{}

func (n *nopLogger) Info(_ string, _ ...interface{}) {}
func (n *nopLogger) Warn(_ string, _ ...interface{}) {}

// authOK 单测：Bearer header / auth_key 查询参数 / 错误密钥
func TestAuthOK(t *testing.T) {
	cases := []struct {
		name string
		path string
		auth string
		want bool
	}{
		{"无凭据", "/metrics", "", false},
		{"错误Bearer", "/metrics", "Bearer wrong", false},
		{"正确Bearer", "/metrics", "Bearer s3cr3t", true},
		{"auth_key查询", "/metrics?auth_key=s3cr3t", "", true},
		{"错误auth_key", "/metrics?auth_key=bad", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if got := authOK(r, "s3cr3t"); got != c.want {
			t.Errorf("%s: authOK=%v want %v", c.name, got, c.want)
		}
	}
}

// 配置 auth_key 后 handleMetrics 未鉴权一律 401 且不泄露指标；带正确 Bearer 返回指标
func TestHandleMetricsRequiresAuth(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.MetricsAuthKey = "lock-key"
	srv := New(cfg, pipeline.New(resource.Labels{}), NewPluginManager(), &nopLogger{})

	for _, c := range []struct {
		name string
		path string
		auth string
		want int
	}{
		{"无凭据401", "/metrics", "", 401},
		{"错误Bearer401", "/metrics", "Bearer bad", 401},
		{"正确Bearer200", "/metrics", "Bearer lock-key", 200},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		srv.handleMetrics(w, r)
		if w.Code != c.want {
			t.Errorf("%s: code=%d want %d", c.name, w.Code, c.want)
		}
		if c.want == 401 && w.Body.Len() > 20 {
			t.Errorf("%s: 401 泄漏了指标体：%q", c.name, w.Body.String())
		}
	}
}

// 未配置 auth_key（存量/演示）时开放访问，不阻断
func TestHandleMetricsOpenWhenNoKey(t *testing.T) {
	cfg := &config.Config{}
	srv := New(cfg, pipeline.New(resource.Labels{}), NewPluginManager(), &nopLogger{})
	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, r)
	if w.Code != 200 {
		t.Errorf("未配 key 应放行，got %d", w.Code)
	}
}
