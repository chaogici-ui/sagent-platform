package builtin

import (
	"fmt"
	"testing"
	"time"
)

// TestHostMetricsSmoke 冒烟：跑一轮采集，验证无 panic、输出结构与声明分组一致
func TestHostMetricsSmoke(t *testing.T) {
	// 模拟界面配置：关掉 custom 组做开关验证，排除 host_fd_used 做例外清单验证
	c := NewHostMetricsCollector(30*time.Second,
		map[string]bool{"custom": false, "netstack": false, "kernel": false},
		[]string{"host_fd_used"})
	out := make(chan []Metric, 8)
	done := make(chan [][]Metric, 1)
	go func() {
		var batches [][]Metric
		for ms := range out {
			batches = append(batches, ms)
			if len(batches) >= 1 {
				done <- batches
				return
			}
		}
		done <- batches
	}()
	c.runOnce(out)

	total := 0
	byName := map[string]Metric{}
	var batches [][]Metric
	select {
	case batches = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("runOnce did not produce metrics within 10s")
	}
	for _, ms := range batches {
		for _, m := range ms {
			total++
			byName[m.Name] = m
			if m.Name == "" {
				t.Fatalf("empty metric name")
			}
			if m.Help == "" {
				t.Fatalf("metric %s missing help", m.Name)
			}
		}
	}
	if _, ok := byName["host_fd_used"]; ok {
		t.Fatalf("exclude_metrics did not filter host_fd_used")
	}
	if _, ok := byName["node_sockstat_sockets_used"]; ok {
		t.Fatalf("disabled group netstack still emitted metrics")
	}
	stats, total2, durMs, ok := c.Stats()
	if !ok || total2 != total || durMs <= 0 {
		t.Fatalf("stats mismatch: statsTotal=%d collected=%d durMs=%f ok=%v", total2, total, durMs, ok)
	}
	fmt.Printf("platform=%s total=%d durMs=%.1f\n", "darwin/linux", total, durMs)
	for g, n := range stats {
		fmt.Printf("  group %-12s %.0f\n", g, n)
	}
	// 抽查关键指标存在性
	for _, want := range []string{"node_cpu_seconds_total", "node_load1", "node_time_seconds", "host_up", "node_scrape_collector_success"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected metric %s missing on this platform", want)
		}
	}
}

// TestHostGroupRegistry 验证注册表完整性（演进点回归保障）
func TestHostGroupRegistry(t *testing.T) {
	labels := HostGroupLabels()
	keys := HostMetricGroupKeys()
	if len(labels) != len(hostMetricGroups) || len(keys) != len(hostMetricGroups) {
		t.Fatalf("registry size mismatch")
	}
	for _, g := range hostMetricGroups {
		if labels[g.Key] == "" {
			t.Errorf("group %s missing label", g.Key)
		}
		if g.Collect == nil {
			t.Errorf("group %s missing collector", g.Key)
		}
	}
}
