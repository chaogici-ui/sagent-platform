package builtin

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

// TestHostMetricsSmoke 冒烟：跑一轮采集，验证无 panic、输出结构与声明分组一致
func TestHostMetricsSmoke(t *testing.T) {
	// 模拟界面配置：关掉 custom 组做开关验证，排除 host_fd_used 做例外清单验证
	c := NewHostMetricsCollector(30*time.Second,
		map[string]bool{"custom": false, "netstack": false, "kernel": false},
		[]string{"host_fd_used"})
	out := make(chan MetricBatch, 8)
	done := make(chan [][]Metric, 1)
	go func() {
		var batches [][]Metric
		for ms := range out {
			batches = append(batches, ms.Metrics)
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

func TestHostEmptyCycleEmits(t *testing.T) {
	groups := map[string]bool{}
	for _, key := range HostMetricGroupKeys() {
		groups[key] = false
	}
	c := NewHostMetricsCollector(time.Second, groups, nil)
	out := make(chan MetricBatch, 1)
	c.runOnce(out)
	select {
	case batch := <-out:
		if len(batch.Metrics) != 0 || batch.Err != nil || batch.Source == "" || batch.ObservedAt.IsZero() || batch.Freshness <= 0 {
			t.Fatalf("invalid empty snapshot: %+v", batch)
		}
		c.runOnce(out)
		if next := <-out; next.Source != batch.Source {
			t.Fatal("host source changed between cycles")
		}
	default:
		t.Fatal("successful empty cycle emitted no snapshot")
	}
}

func TestHostPanicFailsWholeSnapshot(t *testing.T) {
	original := hostMetricGroups
	hostMetricGroups = []hostGroupDef{
		{Key: "ok", Collect: func() ([]Metric, error) { return []Metric{{Name: "node_load1"}}, nil }},
		{Key: "broken", Collect: func() ([]Metric, error) { panic("group failed") }},
	}
	defer func() { hostMetricGroups = original }()
	c := NewHostMetricsCollector(time.Second, map[string]bool{"system": false}, nil)
	out := make(chan MetricBatch, 1)
	c.runOnce(out)
	batch := <-out
	if batch.Err == nil || len(batch.Metrics) != 0 {
		t.Fatalf("partial collection reported success: %+v", batch)
	}
	stats, _, _, success := c.Stats()
	if success || stats["broken"] != -1 {
		t.Fatalf("panic stats indicate success: %+v %v", stats, success)
	}
}

func TestHostFilteredCycleEmitsEmptyReplacement(t *testing.T) {
	original := hostMetricGroups
	hostMetricGroups = []hostGroupDef{{Key: "loadproc", Collect: func() ([]Metric, error) { return []Metric{{Name: "node_load1"}}, nil }}}
	defer func() { hostMetricGroups = original }()
	c := NewHostMetricsCollector(time.Second, map[string]bool{"system": false}, nil)
	out := make(chan MetricBatch, 1)
	c.runOnce(out)
	first := <-out
	c.exclude["node_load1"] = true
	c.runOnce(out)
	next := <-out
	if len(first.Metrics) != 1 || len(next.Metrics) != 0 || next.Err != nil || first.Source != next.Source {
		t.Fatalf("filter did not emit empty replacement: %+v -> %+v", first, next)
	}
}

func hostGroupCycleForTest(key string) (MetricBatch, *HostMetricsCollector) {
	groups := map[string]bool{}
	for _, name := range HostMetricGroupKeys() {
		groups[name] = name == key
	}
	c := NewHostMetricsCollector(time.Second, groups, nil)
	out := make(chan MetricBatch, 1)
	c.runOnce(out)
	return <-out, c
}

func TestHostMemoryProviderFailuresInvalidateSnapshot(t *testing.T) {
	oldOpen, oldVirtual, oldSwap, oldDarwin := openMemoryProc, hostVirtualMemory, hostSwapMemory, isDarwin
	t.Cleanup(func() { openMemoryProc, hostVirtualMemory, hostSwapMemory, isDarwin = oldOpen, oldVirtual, oldSwap, oldDarwin })
	failure := errors.New("memory provider failed")
	for _, name := range []string{"linux-proc", "virtual-memory", "swap-memory"} {
		t.Run(name, func(t *testing.T) {
			isDarwin = name != "linux-proc"
			openMemoryProc = func(string) (*os.File, error) { return nil, failure }
			hostVirtualMemory = func() (*mem.VirtualMemoryStat, error) {
				if name == "virtual-memory" { return nil, failure }
				return &mem.VirtualMemoryStat{Total: 1024}, nil
			}
			hostSwapMemory = func() (*mem.SwapMemoryStat, error) {
				if name == "swap-memory" { return nil, failure }
				return &mem.SwapMemoryStat{}, nil
			}
			batch, collector := hostGroupCycleForTest("memory")
			if !errors.Is(batch.Err, failure) || len(batch.Metrics) != 0 {
				t.Fatalf("ordinary memory error published a successful snapshot: %+v", batch)
			}
			stats, _, _, success := collector.Stats()
			if success || stats["memory"] != -1 { t.Fatalf("error stats lost: %+v", stats) }
		})
	}
}

func TestHostNetworkProviderFailuresInvalidateSnapshot(t *testing.T) {
	oldCounters, oldInterfaces := hostNetIOCounters, hostNetInterfaces
	t.Cleanup(func() { hostNetIOCounters, hostNetInterfaces = oldCounters, oldInterfaces })
	failure := errors.New("network provider failed")
	for _, name := range []string{"counters", "interfaces"} {
		t.Run(name, func(t *testing.T) {
			hostNetIOCounters = func(bool) ([]gnet.IOCountersStat, error) {
				if name == "counters" { return nil, failure }
				return []gnet.IOCountersStat{{Name: "eth-test", BytesRecv: 7}}, nil
			}
			hostNetInterfaces = func() (gnet.InterfaceStatList, error) { return nil, failure }
			batch, collector := hostGroupCycleForTest("network")
			if !errors.Is(batch.Err, failure) || len(batch.Metrics) != 0 {
				t.Fatalf("ordinary network error published a successful snapshot: %+v", batch)
			}
			stats, _, _, success := collector.Stats()
			if success || stats["network"] != -1 { t.Fatalf("error stats lost: %+v", stats) }
		})
	}
}

func TestHostNetworkEmptyDevicesAreSuccessfulSnapshot(t *testing.T) {
	oldCounters, oldInterfaces := hostNetIOCounters, hostNetInterfaces
	t.Cleanup(func() { hostNetIOCounters, hostNetInterfaces = oldCounters, oldInterfaces })
	hostNetIOCounters = func(bool) ([]gnet.IOCountersStat, error) { return nil, nil }
	hostNetInterfaces = func() (gnet.InterfaceStatList, error) { return nil, nil }
	batch, collector := hostGroupCycleForTest("network")
	_, _, _, success := collector.Stats()
	if batch.Err != nil || len(batch.Metrics) != 0 || !success || batch.Source != SnapshotSource(collector.Name(), "local") {
		t.Fatalf("legal empty device set treated as failure: %+v", batch)
	}
}

func TestHostUnsupportedGroupsAreSuccessfulEmptySnapshot(t *testing.T) {
	old := isDarwin
	isDarwin = true
	t.Cleanup(func() { isDarwin = old })
	for _, group := range []string{"netstack", "kernel"} {
		t.Run(group, func(t *testing.T) {
			batch, collector := hostGroupCycleForTest(group)
			_, _, _, success := collector.Stats()
			if batch.Err != nil || len(batch.Metrics) != 0 || !success {
				t.Fatalf("unsupported platform must succeed with empty snapshot: %+v", batch)
			}
		})
	}
}
