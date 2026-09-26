package builtin

import (
	"strings"
	"testing"
)

// procs_running / procs_blocked 必须取 /proc/stat 的内核计数（node_exporter 同源）。
// 原实现用 /proc/loadavg 的「总数 - 运行数」冒充 blocked，那是含 sleeping 的差值，
// 会把"阻塞 IO 进程数"长期顶到几百上千（真实值通常是个位数）
func TestProcStatMetricsUseKernelCounters(t *testing.T) {
	st := parseProcStatText(strings.Join([]string{
		"cpu  1 2 3 4 5 6 7 8 9 10",
		"intr 100",
		"ctxt 200",
		"btime 1700000000",
		"processes 300",
		"procs_running 3",
		"procs_blocked 1",
	}, "\n"))
	if st["procs_running"] != 3 || st["procs_blocked"] != 1 {
		t.Fatalf("procs 字段解析错误：running=%v blocked=%v", st["procs_running"], st["procs_blocked"])
	}
	if st["processes"] != 300 || st["ctxt"] != 200 {
		t.Fatalf("既有字段解析回归：processes=%v ctxt=%v", st["processes"], st["ctxt"])
	}

	byName := map[string]Metric{}
	for _, m := range procStatMetrics(st) {
		byName[m.Name] = m
	}
	mb, ok := byName["node_procs_blocked"]
	if !ok {
		t.Fatalf("必须产出 node_procs_blocked")
	}
	if mb.Value != 1 {
		t.Errorf("node_procs_blocked 必须取内核 procs_blocked（阻塞 IO）计数，实际 %v", mb.Value)
	}
	if !strings.Contains(mb.Help, "disk IO") {
		t.Errorf("Help 必须与口径一致（blocking for disk IO）：%q", mb.Help)
	}
	if mr := byName["node_procs_running"]; mr.Value != 3 {
		t.Errorf("node_procs_running 应取 procs_running，实际 %v", mr.Value)
	}
}

// 未采到 procs_blocked 时不得凭空产 0：假数据比缺数据更糟
func TestProcStatMetricsSkipMissingFields(t *testing.T) {
	st := parseProcStatText("processes 300\n")
	names := map[string]bool{}
	for _, m := range procStatMetrics(st) {
		names[m.Name] = true
	}
	if names["node_procs_blocked"] || names["node_procs_running"] {
		t.Errorf("字段缺失时不得产出 procs 指标（不可用 0 冒充）：%v", names)
	}
	if !names["node_forks_total"] {
		t.Errorf("已采到的字段仍要产出：%v", names)
	}
}
