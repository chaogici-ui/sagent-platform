package builtin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestScrapeBodyAndProbeSnapshots(t *testing.T) {
	var mu sync.Mutex
	status, body := http.StatusOK, "node_load1 123\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	s := NewScrapePlugin([]ScrapeTarget{{Name: "target", URL: srv.URL, Labels: map[string]string{"resource_id": "remote"}}}, time.Second)
	out := make(chan MetricBatch, 4)
	s.scrape(out)
	first, probe := <-out, <-out
	if first.Err != nil || len(first.Metrics) != 1 || first.Metrics[0].Labels["resource_id"] != "remote" || first.Source == probe.Source || probe.Metrics[0].Value != 1 {
		t.Fatalf("body/probe not separate snapshots: %+v %+v", first, probe)
	}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		failure bool
	}{
		{"non2xx", http.StatusServiceUnavailable, "node_load1 999\n", true},
		{"invalid-value", http.StatusOK, "node_load1 not-a-number\n", true},
		{"partial-body", http.StatusOK, "node_load1 1\nbroken\n", true},
		{"empty-success", http.StatusOK, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, body = tc.status, tc.body
			mu.Unlock()
			s.scrape(out)
			batch, health := <-out, <-out
			if (batch.Err != nil) != tc.failure || len(batch.Metrics) != 0 || batch.Source != first.Source || health.Source != probe.Source {
				t.Fatalf("invalid body snapshot: %+v %+v", batch, health)
			}
			want := 1.0
			if tc.failure {
				want = 0
			}
			if health.Err != nil || len(health.Metrics) != 1 || health.Metrics[0].Name != "up" || health.Metrics[0].Value != want {
				t.Fatalf("invalid independent probe snapshot: %+v", health)
			}
		})
	}
	srv.Close()
	s.scrape(out)
	if batch, health := <-out, <-out; batch.Err == nil || health.Err != nil || health.Metrics[0].Value != 0 {
		t.Fatalf("transport failure not distinguished: %+v %+v", batch, health)
	}
}

// mockScrapeServer 返回一个可动态切换 up/down 的抓取 server
func mockScrapeServer(t *testing.T) (*httptest.Server, func(bool)) {
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
		fmt.Fprint(w, "ha_node_load1 42\n")
	}))
	return srv, func(d bool) {
		mu.Lock()
		down = d
		mu.Unlock()
	}
}

// drainBatch 消费一个抓取周期（单端点=body+probe，多端点若切换则 +1 switch 快照）。
func drainBatch(ch chan MetricBatch, n int) []MetricBatch {
	out := make([]MetricBatch, 0, n)
	for i := 0; i < n; i++ {
		select {
		case b := <-ch:
			out = append(out, b)
		case <-time.After(2 * time.Second):
		}
	}
	return out
}

// lastValue scans drained batches for the first value of a metric name.
func lastValue(bs []MetricBatch, metric string) (float64, bool) {
	for _, b := range bs {
		if b.Err != nil {
			continue
		}
		for _, m := range b.Metrics {
			if m.Name == metric {
				return m.Value, true
			}
		}
	}
	return 0, false
}

// activeEndpointIndex 从一轮快照中读取 up 探测的 endpoint_index（若存在）。
func activeEndpointIndex(bs []MetricBatch) string {
	for _, b := range bs {
		for _, m := range b.Metrics {
			if m.Name == "up" {
				return m.Labels["endpoint_index"]
			}
		}
	}
	return ""
}

// TestScrapeFailoverToReplicaAndFailback HA-3：主端点故障→切备端，主恢复后防抖回切。
func TestScrapeFailoverToReplicaAndFailback(t *testing.T) {
	primary, setPrimary := mockScrapeServer(t)
	defer primary.Close()
	replica, setReplica := mockScrapeServer(t)
	defer replica.Close()
	_ = setReplica // 本用例只让主端故障/恢复，备端全程健康

	s := NewScrapePlugin([]ScrapeTarget{{
		Name: "order", URL: primary.URL, Replicas: []string{replica.URL},
		Labels: map[string]string{"resource_id": "remote"},
	}}, time.Second)
	out := make(chan MetricBatch, 32)

	drain := func() []MetricBatch {
		outN := make([]MetricBatch, 0, 4)
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
	s.scrape(out)
	if idx := activeEndpointIndex(drain()); idx != "0" {
		t.Fatalf("all-healthy should be on primary(0), got %q", idx)
	}

	// ② 主端点故障 → 切到备端(1)
	setPrimary(true)
	s.scrape(out)
	round := drain()
	if _, ok := lastValue(round, "exporter_endpoint_switch_total"); !ok {
		t.Fatalf("expected failover switch metric, samples=%v", round)
	}
	if idx := activeEndpointIndex(round); idx != "1" {
		t.Fatalf("after failover active should be replica(1), got %q", idx)
	}

	// ③ 主端点恢复：防抖回切（连续 failbackThreshold 个健康周期后切回主）
	setPrimary(false)
	switchedBack := false
	for i := 0; i <= failbackThreshold+2; i++ {
		s.scrape(out)
		round = drain()
		if _, ok := lastValue(round, "exporter_endpoint_switch_total"); ok {
			switchedBack = true
		}
	}
	if !switchedBack || activeEndpointIndex(round) != "0" {
		t.Fatalf("expected failback to primary(0) after recovery debounce, active=%q switched=%v",
			activeEndpointIndex(round), switchedBack)
	}
}

// TestScrapeFailoverAllDownKeepsUp0 HA-3：主备全部故障时 up=0，且不产生虚假切换。
func TestScrapeFailoverAllDownKeepsUp0(t *testing.T) {
	primary, setPrimary := mockScrapeServer(t)
	defer primary.Close()
	replica, setReplica := mockScrapeServer(t)
	defer replica.Close()

	s := NewScrapePlugin([]ScrapeTarget{{
		Name: "order", URL: primary.URL, Replicas: []string{replica.URL},
		Labels: map[string]string{"resource_id": "remote"},
	}}, time.Second)
	out := make(chan MetricBatch, 32)

	setPrimary(true)
	setReplica(true)
	s.scrape(out)
	<-out // body(失败)
	up := false
	var val float64
	select {
	case b := <-out:
		for _, m := range b.Metrics {
			if m.Name == "up" {
				up, val = true, m.Value
			}
		}
	case <-time.After(2 * time.Second):
	}
	if !up || val != 0 {
		t.Fatalf("all endpoints down should report up=0, got up=%v val=%v", up, val)
	}
	if _, ok := lastValue([]MetricBatch{{}}, "exporter_endpoint_switch_total"); ok {
		t.Fatalf("all-down must not emit a switch")
	}
}

// TestScrapeFailoverSingleEndpointNoSwitch HA-3：单端点目标不触发切换路径（回归护栏）。
func TestScrapeFailoverSingleEndpointNoSwitch(t *testing.T) {
	up, setDown := mockScrapeServer(t)
	defer up.Close()

	s := NewScrapePlugin([]ScrapeTarget{{Name: "single", URL: up.URL, Labels: map[string]string{"resource_id": "x"}}}, time.Second)
	out := make(chan MetricBatch, 16)

	s.scrape(out)
	first, probe := <-out, <-out
	if first.Err != nil || probe.Metrics[0].Name != "up" || probe.Metrics[0].Value != 1 {
		t.Fatalf("single healthy: body+probe(up=1) expected, got %+v %+v", first, probe)
	}
	setDown(true)
	s.scrape(out)
	bfail, pdown := <-out, <-out
	if bfail.Err == nil || pdown.Metrics[0].Value != 0 {
		t.Fatalf("single down: body err + up=0 expected, got %+v %+v", bfail, pdown)
	}
	_ = first
}
