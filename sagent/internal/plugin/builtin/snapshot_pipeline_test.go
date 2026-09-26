package builtin_test

import (
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

type textTransport func(*http.Request) (*http.Response, error)

func (f textTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMySQLDownPreservesBusinessUntilOriginalTTL(t *testing.T) {
	now := time.Unix(100, 0)
	p := pipeline.NewWithClock(resource.Labels{}, func() time.Time { return now })
	target := config.MySQLTarget{ResourceID: "db-1", DSN: "user@tcp(db:3306)/"}
	body := "# TYPE mysql_up gauge\nmysql_up 1\nmysql_global_status_threads_connected 7\n"
	client := &http.Client{Transport: textTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("target") != "db:3306" {
			t.Errorf("unexpected target: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	collect := func(failed bool) []builtin.MetricBatch {
		batches := builtin.CollectMySQLForTest(target, client)
		if len(batches) != 2 {
			t.Fatalf("expected separate business and health batches, got %+v", batches)
		}
		business, health := batches[0], batches[1]
		if business.Source != builtin.SnapshotSource("mysql_probe", "db-1", "db:3306") || health.Source != builtin.SnapshotSource("mysql_probe_health", "db-1", "db:3306") {
			t.Fatalf("unstable source identity: %+v", batches)
		}
		if (business.Err != nil) != failed || health.Err != nil || len(health.Metrics) != 1 || health.Metrics[0].Name != "mysql_up" {
			t.Fatalf("wrong business/health status: %+v", batches)
		}
		for _, metric := range business.Metrics {
			if metric.Name == "mysql_up" {
				t.Fatal("health leaked into retained business snapshot")
			}
		}
		wantUp := 1.0
		if failed {
			wantUp = 0
		}
		if health.Metrics[0].Value != wantUp {
			t.Fatalf("health=%v want %v", health.Metrics, wantUp)
		}
		for _, batch := range batches {
			batch.ObservedAt = now
			p.IngestBatch(batch)
		}
		return batches
	}
	initial := collect(false)
	expiry := now.Add(initial[0].Freshness)
	now = expiry.Add(-time.Second)
	body = "mysql_up 0\nmysql_global_status_threads_connected 999\n"
	collect(true)
	text := p.PrometheusText()
	if !strings.Contains(text, "} 7\n") || strings.Contains(text, "999") || strings.Count(text, "\nmysql_up{") != 1 || strings.Contains(text, "} 1\n") {
		t.Fatalf("down probe replaced business or exposed stale/duplicate health: %s", text)
	}
	now = expiry
	if text = p.PrometheusText(); strings.Contains(text, "mysql_global_status_threads_connected") || !strings.Contains(text, "} 0\n") || p.MetricCount() != 1 {
		t.Fatalf("failure renewed business TTL or lost fresh health: %s", text)
	}
	body = "mysql_up 1\nmysql_global_status_threads_connected 8\n"
	collect(false)
	if text = p.PrometheusText(); !strings.Contains(text, "} 8\n") || p.MetricCount() != 2 {
		t.Fatalf("recovery failed: %s", text)
	}
	for _, batch := range initial {
		p.RemoveSource(batch.Source)
	}
	if p.MetricCount() != 0 {
		t.Fatalf("explicit removal left samples: %s", p.PrometheusText())
	}
}

func TestPrometheusFamiliesRoundTripThroughPipeline(t *testing.T) {
	body := `# HELP request_seconds Request duration\nseconds
# TYPE request_seconds histogram
request_seconds_bucket{route="/a",le="0.5"} 2
request_seconds_bucket{route="/a",le="+Inf"} 3
request_seconds_sum{route="/a"} 1.2
request_seconds_count{route="/a"} 3
# HELP response_seconds Response duration
# TYPE response_seconds summary
response_seconds{quantile="0.5"} 0.2
response_seconds{quantile="0.9"} 0.8
response_seconds_sum 1.5
response_seconds_count 4
# HELP queue_count Queue gauge
# TYPE queue_count gauge
queue_count 7
# TYPE standalone gauge
standalone 2
standalone_count 6
# TYPE explicit histogram
explicit_bucket{le="+Inf"} 1
# TYPE explicit_count gauge
explicit_count 9
`
	metrics, err := builtin.ParsePrometheusForTest(body)
	if err != nil {
		t.Fatal(err)
	}
	p := pipeline.New(resource.Labels{})
	p.IngestBatch(builtin.MetricBatch{Source: "script", Metrics: metrics, Freshness: time.Minute})
	text := p.PrometheusText()
	for _, metadata := range []string{"# TYPE request_seconds histogram\n", "# TYPE response_seconds summary\n", "# HELP request_seconds Request duration\\nseconds\n", "# HELP response_seconds Response duration\n", "# TYPE queue_count gauge\n", "# TYPE standalone_count gauge\n", "# TYPE explicit_count gauge\n"} {
		if strings.Count(text, metadata) != 1 {
			t.Errorf("missing/duplicate family metadata %q: %s", metadata, text)
		}
	}
	for _, name := range []string{"request_seconds_bucket", "request_seconds_sum", "request_seconds_count", "response_seconds_sum", "response_seconds_count"} {
		if strings.Contains(text, "# TYPE "+name+" ") || strings.Contains(text, "# HELP "+name+" ") {
			t.Errorf("sample incorrectly exposed as family: %s", name)
		}
	}
	for _, metric := range metrics {
		if strings.HasPrefix(metric.Name, "request_seconds_") && (metric.Type != "histogram" || metric.Help != "Request duration\nseconds") {
			t.Errorf("histogram metadata lost: %+v", metric)
		}
		if strings.HasPrefix(metric.Name, "response_seconds") && metric.Type != "summary" {
			t.Errorf("summary metadata lost: %+v", metric)
		}
	}
	again, err := builtin.ParsePrometheusForTest(text)
	if err != nil {
		t.Fatal(err)
	}
	order := func(ms []builtin.Metric) {
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].Name != ms[j].Name { return ms[i].Name < ms[j].Name }
			if ms[i].Labels["le"] != ms[j].Labels["le"] { return ms[i].Labels["le"] < ms[j].Labels["le"] }
			return ms[i].Labels["quantile"] < ms[j].Labels["quantile"]
		})
	}
	for i := range metrics {
		for key, value := range (resource.Labels{}).ToMap() {
			metrics[i].Labels[key] = value
		}
	}
	order(metrics)
	order(again)
	if !reflect.DeepEqual(metrics, again) {
		t.Fatalf("round trip changed samples, labels or metadata:\n%+v\n%+v", metrics, again)
	}
	if text != p.PrometheusText() {
		t.Fatal("family output is nondeterministic")
	}
}
