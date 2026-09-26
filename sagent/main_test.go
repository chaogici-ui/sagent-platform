package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

// listen 地址 → 本机可达 URL：0.0.0.0 / [::] / 空前缀一律换 127.0.0.1。
// 直接拼 "http://127.0.0.1"+listen 在 listen=0.0.0.0:19090 时会生成非法 URL
func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		":19090":          "http://127.0.0.1:19090",
		"0.0.0.0:19090":   "http://127.0.0.1:19090",
		"[::]:19090":      "http://127.0.0.1:19090",
		"127.0.0.1:19090": "http://127.0.0.1:19090",
		"10.1.2.3:19090":  "http://10.1.2.3:19090",
	}
	for in, want := range cases {
		if got := loopbackURL(in); got != want {
			t.Errorf("loopbackURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestBuiltinConsumerUsesSnapshotReplacement(t *testing.T) {
	app := &App{pipeline: pipeline.New(resource.Labels{}), metricsCh: make(chan builtin.MetricBatch, 3)}
	app.pipeline.Ingest([]builtin.Metric{{Name: "log_events_total", Value: 4}})
	app.metricsCh <- builtin.MetricBatch{Source: "host", ObservedAt: time.Now(), Freshness: time.Minute, Metrics: []builtin.Metric{{Name: "node_load1"}}}
	app.metricsCh <- builtin.MetricBatch{Source: "mysql", ObservedAt: time.Now(), Freshness: time.Minute, Metrics: []builtin.Metric{{Name: "mysql_up"}}}
	app.metricsCh <- builtin.MetricBatch{Source: "host", ObservedAt: time.Now(), Freshness: time.Minute}
	close(app.metricsCh)
	app.consumeMetricBatches()
	if text := app.pipeline.PrometheusText(); app.pipeline.MetricCount() != 2 || strings.Contains(text, "node_load1") || !strings.Contains(text, "log_events_total") || !strings.Contains(text, "mysql_up") {
		t.Fatalf("main consumer did not use snapshots: %s", text)
	}
}

func TestScrapeFailureThroughBuiltinConsumerDoesNotRenewBusiness(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprintln(w, "node_load1 12")
	}))
	defer srv.Close()
	now := time.Now()
	app := &App{pipeline: pipeline.NewWithClock(resource.Labels{ResourceID: "local"}, func() time.Time { return now })}
	collect := func() builtin.MetricBatch {
		out := make(chan builtin.MetricBatch, 2)
		collector := builtin.NewScrapePlugin([]builtin.ScrapeTarget{{Name: "remote", URL: srv.URL}}, time.Hour)
		collector.Start(out)
		defer collector.Stop()
		app.metricsCh = make(chan builtin.MetricBatch, 2)
		var first builtin.MetricBatch
		for i := 0; i < 2; i++ {
			select {
			case batch := <-out:
				if i == 0 {
					first = batch
				}
				app.metricsCh <- batch
			case <-time.After(2 * time.Second):
				t.Fatal("collector did not emit body and probe batches")
			}
		}
		close(app.metricsCh)
		app.consumeMetricBatches()
		return first
	}
	first := collect()
	if app.pipeline.MetricCount() != 2 {
		t.Fatal("initial body/probe missing")
	}
	mu.Lock()
	status = http.StatusServiceUnavailable
	mu.Unlock()
	now = first.ObservedAt.Add(first.Freshness - time.Nanosecond)
	failure := collect()
	if failure.Err == nil || failure.Source != first.Source || app.pipeline.MetricCount() != 2 {
		t.Fatal("failure discarded business snapshot early")
	}
	now = first.ObservedAt.Add(first.Freshness)
	text := app.pipeline.PrometheusText()
	if app.pipeline.MetricCount() != 1 || strings.Contains(text, "node_load1") || !strings.Contains(text, "up{") || !strings.HasSuffix(text, " 0\n") {
		t.Fatalf("failure renewed business or lost independent probe: %s", text)
	}
}
