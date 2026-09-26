package pipeline

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

const testMetric = "node_network_info"

func testLabels() map[string]string {
	return map[string]string{"device": "eth0", "iface": "eth0", "proto": "tcp"}
}

// metricKey 必须与 map 遍历顺序无关：否则同一序列每轮采集都会生成新 key，
// metrics 表只增不减、/metrics 同一序列输出多行（Prometheus 报 duplicate sample）
func TestMetricKeyIsOrderIndependent(t *testing.T) {
	first := metricKey(testMetric, testLabels())
	for i := 0; i < 32; i++ {
		if got := metricKey(testMetric, testLabels()); got != first {
			t.Fatalf("同一组标签必须得到同一 key：第 %d 次 %q ≠ %q（key 依赖了 map 遍历顺序）", i, got, first)
		}
	}
}

// 无标签指标 key 即指标名（不回退成空串），不同标签集合不得撞成同一 key
func TestMetricKeyDistinguishesLabelSets(t *testing.T) {
	if got := metricKey("node_load1", nil); got != "node_load1" {
		t.Errorf("无标签时 key 应为指标名，实际 %q", got)
	}
	a := metricKey(testMetric, map[string]string{"device": "eth0"})
	b := metricKey(testMetric, map[string]string{"device": "eth1"})
	if a == b {
		t.Errorf("不同标签值不得撞 key：%q", a)
	}
}

// 同一序列重复采集必须收敛为一条：原实现按随机 key 累积，
// 采集循环跑 N 轮就留 N 条重复（内存单调增长 + 输出重复行）
func TestIngestIsIdempotentPerSeries(t *testing.T) {
	p := New(resource.Labels{ResourceID: "order.prod.host.sagent-1"})
	batch := []builtin.Metric{{
		Name: testMetric, Value: 1, Help: "network info",
		Labels: map[string]string{"device": "eth0", "iface": "eth0", "proto": "tcp"},
	}}
	for i := 0; i < 20; i++ {
		p.Ingest(batch)
	}
	if n := p.MetricCount(); n != 1 {
		t.Fatalf("同一序列重复采集应收敛为 1 条，实际 %d 条（key 随机化导致重复累积）", n)
	}
	if c := strings.Count(p.PrometheusText(), testMetric+"{"); c != 1 {
		t.Fatalf("输出应只含 1 行该序列，实际 %d 行", c)
	}
}

func TestIngestCopiesLabels(t *testing.T) {
	p := New(resource.Labels{})
	labels := map[string]string{"device": "original"}
	p.Ingest([]builtin.Metric{{Name: "node_network_info", Labels: labels}})
	labels["device"] = "changed"
	if text := p.PrometheusText(); !strings.Contains(text, `device="original"`) {
		t.Fatalf("caller changed cached labels: %s", text)
	}
}

func TestRemoteResourceAndProtectedIdentity(t *testing.T) {
	p := New(resource.Labels{ResourceID: "local", ResourceType: "host", Env: "prod"})
	p.Ingest([]builtin.Metric{{Name: "mysql_up", Labels: map[string]string{
		"resource_id": "remote", "resource_type": "mysql", "env": "untrusted",
	}}})
	text := p.PrometheusText()
	for _, want := range []string{`resource_id="remote"`, `resource_type="host"`, `env="prod"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s: %s", want, text)
		}
	}
}

func TestEffectiveLabelCollision(t *testing.T) {
	p := New(resource.Labels{Env: "prod"})
	p.Ingest([]builtin.Metric{
		{Name: "node_load1", Value: 1, Labels: map[string]string{"env": "first"}},
		{Name: "node_load1", Value: 2, Labels: map[string]string{"env": "second"}},
	})
	if n := p.MetricCount(); n != 1 {
		t.Errorf("effective-label collision left %d samples", n)
	}
	if text := p.PrometheusText(); strings.Count(text, "node_load1{") != 1 {
		t.Fatalf("duplicate effective series: %s", text)
	}
}

func TestDeterministicEscapedExposition(t *testing.T) {
	got := formatMetric("node_load1", 1, map[string]string{"z": "back\\slash\"quote\nline", "a": "first"})
	want := `node_load1{a="first",z="back\\slash\"quote\nline"} 1`
	if got != want {
		t.Errorf("exposition = %q, want %q", got, want)
	}
	p := New(resource.Labels{})
	p.Ingest([]builtin.Metric{
		{Name: "node_load5", Value: 5},
		{Name: "node_load1", Value: 1, Help: "load\\help\nline", Labels: map[string]string{"cpu": "1"}},
		{Name: "node_load1", Value: 2, Help: "load\\help\nline", Labels: map[string]string{"cpu": "0"}},
	})
	text := p.PrometheusText()
	if strings.Count(text, "# HELP node_load1 ") != 1 || strings.Count(text, "# TYPE node_load1 ") != 1 {
		t.Errorf("duplicate family metadata: %s", text)
	}
	if !strings.Contains(text, `# HELP node_load1 load\\help\nline`+"\n") {
		t.Errorf("HELP not escaped: %q", text)
	}
	if strings.Index(text, "node_load1{") > strings.Index(text, "node_load5{") {
		t.Errorf("families not sorted: %s", text)
	}
	for i := 0; i < 20; i++ {
		if next := p.PrometheusText(); next != text {
			t.Fatalf("exposition changed between reads")
		}
	}
}

func TestSnapshotsReplaceOnlyTheirSource(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewWithClock(resource.Labels{}, func() time.Time { return now })
	put := func(source string, metrics ...builtin.Metric) {
		p.IngestBatch(builtin.MetricBatch{Source: source, Metrics: metrics, ObservedAt: now, Freshness: time.Minute})
	}
	put("host", builtin.Metric{Name: "node_load1"}, builtin.Metric{Name: "node_load5"})
	put("mysql", builtin.Metric{Name: "mysql_up"})
	put("host", builtin.Metric{Name: "node_load1", Value: 2})
	if p.MetricCount() != 2 || strings.Contains(p.PrometheusText(), "node_load5") {
		t.Fatalf("snapshot did not replace source: %s", p.PrometheusText())
	}
	put("host")
	if p.MetricCount() != 1 || !strings.Contains(p.PrometheusText(), "mysql_up") {
		t.Fatalf("empty success cleared wrong source: %s", p.PrometheusText())
	}
	p.RemoveSource("mysql")
	p.RemoveSource("missing")
	if p.MetricCount() != 0 || p.PrometheusText() != "" {
		t.Fatal("explicit source removal left samples")
	}
}

func TestFailureDoesNotRefreshExpiry(t *testing.T) {
	for _, textFirst := range []bool{false, true} {
		t.Run(fmtBool(textFirst), func(t *testing.T) {
			now := time.Unix(100, 0)
			p := NewWithClock(resource.Labels{}, func() time.Time { return now })
			p.IngestBatch(builtin.MetricBatch{Source: "host", Metrics: []builtin.Metric{{Name: "node_load1", Value: 1}}, ObservedAt: now, Freshness: 10 * time.Second})
			now = now.Add(9 * time.Second)
			p.IngestBatch(builtin.MetricBatch{Source: "host", Metrics: []builtin.Metric{{Name: "node_load1", Value: 999}}, ObservedAt: now, Freshness: time.Hour, Err: errors.New("failed")})
			if p.MetricCount() != 1 || strings.Contains(p.PrometheusText(), "999") {
				t.Fatal("failure replaced last successful samples")
			}
			now = now.Add(time.Second)
			if textFirst && p.PrometheusText() != "" {
				t.Fatal("expired samples still exposed")
			}
			if p.MetricCount() != 0 || p.PrometheusText() != "" {
				t.Fatal("count/text retained expired samples")
			}
			if len(p.sources) != 0 {
				t.Fatal("expired source cache not reclaimed")
			}
		})
	}
}

func fmtBool(value bool) string {
	if value {
		return "text-first"
	}
	return "count-first"
}

func TestBatchLabelsAreCopiedAndEventIngestRemainsIncremental(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewWithClock(resource.Labels{ResourceID: "local"}, func() time.Time { return now })
	metrics := []builtin.Metric{{Name: "mysql_up", Value: 1, Labels: map[string]string{"resource_id": "remote"}}}
	p.IngestBatch(builtin.MetricBatch{Source: "mysql", Metrics: metrics, ObservedAt: now, Freshness: time.Second})
	metrics[0].Labels["resource_id"] = "mutated"
	metrics[0].Name = "mutated"
	if text := p.PrometheusText(); !strings.Contains(text, `resource_id="remote"`) || strings.Contains(text, "mutated") {
		t.Fatalf("batch was not deeply copied: %s", text)
	}
	p.Ingest([]builtin.Metric{{Name: "log_events_total", Value: 4}})
	p.Ingest(nil)
	now = now.Add(time.Second)
	if p.MetricCount() != 1 || !strings.Contains(p.PrometheusText(), "log_events_total") {
		t.Fatal("snapshot expiry affected event ingress")
	}
}

func TestSnapshotEffectiveCollisionsAndFallback(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewWithClock(resource.Labels{Env: "prod"}, func() time.Time { return now })
	for i, source := range []string{"first", "second"} {
		p.IngestBatch(builtin.MetricBatch{Source: source, ObservedAt: now, Freshness: time.Minute, Metrics: []builtin.Metric{
			{Name: "node_load1", Value: float64(i + 1), Labels: map[string]string{"env": source}},
		}})
	}
	if text := p.PrometheusText(); p.MetricCount() != 1 || strings.Count(text, "node_load1{") != 1 || !strings.HasSuffix(text, " 2\n") {
		t.Fatalf("collision winner not newest batch: %s", text)
	}
	p.RemoveSource("second")
	if text := p.PrometheusText(); p.MetricCount() != 1 || !strings.HasSuffix(text, " 1\n") {
		t.Fatalf("removing winner cleared other source: %s", text)
	}
}
