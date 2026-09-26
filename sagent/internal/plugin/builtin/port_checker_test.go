package builtin

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockPortServer 返回一个可动态切换 up/down 的 http 探测 server（http 类型，2xx=up）。
func mockPortServer(t *testing.T) (*httptest.Server, func(bool)) {
	var mu sync.Mutex
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := down
		mu.Unlock()
		if d {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return srv, func(d bool) {
		mu.Lock()
		down = d
		mu.Unlock()
	}
}

// portActiveIndex 从一轮快照中读取 port_check_up 的 endpoint_index。
func portActiveIndex(bs []MetricBatch) string {
	for _, b := range bs {
		for _, m := range b.Metrics {
			if m.Name == "port_check_up" {
				return m.Labels["endpoint_index"]
			}
		}
	}
	return ""
}

// TestPortCheckFailoverToReplicaAndFailback HA-3：主端点故障→切备端，主恢复后防抖回切。
func TestPortCheckFailoverToReplicaAndFailback(t *testing.T) {
	primary, setPrimary := mockPortServer(t)
	defer primary.Close()
	replica, setReplica := mockPortServer(t)
	defer replica.Close()
	_ = setReplica // 本用例只让主端故障/恢复，备端全程健康

	p := NewPortChecker([]PortTarget{{
		Name: "order-api", Type: "http", Address: primary.URL, Replicas: []string{replica.URL},
		Labels: map[string]string{"resource_id": "remote"},
	}}, time.Second)
	out := make(chan MetricBatch, 32)

	drain := func() []MetricBatch {
		outN := make([]MetricBatch, 0, 3)
		timeout := time.After(250 * time.Millisecond)
		for len(outN) < 3 {
			select {
			case b := <-out:
				outN = append(outN, b)
			case <-timeout:
				return outN
			}
		}
		return outN
	}

	// ① 主备都健康：活跃=主端点(0)，无切换
	p.check(out)
	if idx := portActiveIndex(drain()); idx != "0" {
		t.Fatalf("all-healthy should be on primary(0), got %q", idx)
	}

	// ② 主端点故障 → 切到备端(1)
	setPrimary(true)
	p.check(out)
	round := drain()
	if _, ok := lastValue(round, "port_checker_endpoint_switch_total"); !ok {
		t.Fatalf("expected failover switch metric, samples=%v", round)
	}
	if idx := portActiveIndex(round); idx != "1" {
		t.Fatalf("after failover active should be replica(1), got %q", idx)
	}

	// ③ 主端点恢复：防抖回切（连续 failbackThreshold 个健康周期后切回主）
	setPrimary(false)
	switchedBack := false
	for i := 0; i <= failbackThreshold+2; i++ {
		p.check(out)
		round = drain()
		if _, ok := lastValue(round, "port_checker_endpoint_switch_total"); ok {
			switchedBack = true
		}
	}
	if !switchedBack || portActiveIndex(round) != "0" {
		t.Fatalf("expected failback to primary(0) after recovery debounce, active=%q switched=%v",
			portActiveIndex(round), switchedBack)
	}
}

// TestPortCheckFailoverAllDownKeepsUp0 HA-3：主备全部故障时 up=0，且不产生虚假切换。
func TestPortCheckFailoverAllDownKeepsUp0(t *testing.T) {
	primary, setPrimary := mockPortServer(t)
	defer primary.Close()
	replica, setReplica := mockPortServer(t)
	defer replica.Close()

	p := NewPortChecker([]PortTarget{{
		Name: "order-api", Type: "http", Address: primary.URL, Replicas: []string{replica.URL},
		Labels: map[string]string{"resource_id": "remote"},
	}}, time.Second)
	out := make(chan MetricBatch, 32)

	setPrimary(true)
	setReplica(true)
	p.check(out)
	var val float64
	got := false
	select {
	case b := <-out:
		for _, m := range b.Metrics {
			if m.Name == "port_check_up" {
				val, got = m.Value, true
			}
		}
	case <-time.After(2 * time.Second):
	}
	if !got || val != 0 {
		t.Fatalf("all endpoints down should report up=0, got got=%v val=%v", got, val)
	}
	if _, ok := lastValue([]MetricBatch{{}}, "port_checker_endpoint_switch_total"); ok {
		t.Fatalf("all-down must not emit a switch")
	}
}

// TestPortCheckFailoverSingleEndpointNoSwitch HA-3：单端点目标不触发切换路径（回归护栏）。
func TestPortCheckFailoverSingleEndpointNoSwitch(t *testing.T) {
	up, setDown := mockPortServer(t)
	defer up.Close()

	p := NewPortChecker([]PortTarget{{Name: "single", Type: "http", Address: up.URL, Labels: map[string]string{"resource_id": "x"}}}, time.Second)
	out := make(chan MetricBatch, 16)

	p.check(out)
	b := <-out
	if b.Err != nil || len(b.Metrics) == 0 || b.Metrics[0].Name != "port_check_up" || b.Metrics[0].Value != 1 {
		t.Fatalf("single healthy: up=1 expected, got %+v", b)
	}
	setDown(true)
	p.check(out)
	bfail := <-out
	if len(bfail.Metrics) == 0 || bfail.Metrics[0].Name != "port_check_up" || bfail.Metrics[0].Value != 0 {
		t.Fatalf("single down: up=0 expected, got %+v", bfail)
	}
	if _, ok := lastValue([]MetricBatch{bfail}, "port_checker_endpoint_switch_total"); ok {
		t.Fatalf("single endpoint must not switch")
	}
	_ = b
}