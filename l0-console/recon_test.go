package main

import (
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// expandAgentConfigKnown：D8 对账展开核心（红线 7 的裁判逻辑）。
// 语义：known = 目录注册集 ∪ Agent 配置展开集；未声明组默认开启、显式关闭的组剔除、
// exclude 例外剔除、host_metrics 不走插件级展开（走分组）、legacy 纯数组兼容。

func newReconTestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	db, err := storepkg.Open(t.TempDir() + "/recon-test.db")
	if err != nil {
		t.Fatalf("open recon test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 目录夹具：host_metrics 3 条（cpu×2 + memory×1，phase=1）+ test 插件 2 条。
	// 插件名模拟真实约定：目录行不带 _probe/_exporter 后缀（ResolvePluginID 会剥后缀匹配），
	// Agent targets 里的 plugin 字段写 "test_probe"，展开时应解析到 "test" 插件。
	hostMetrics := []*storepkg.Metric{
		{PluginName: "host_metrics", Name: "node_cpu_seconds_total", Grp: "cpu", Phase: 1},
		{PluginName: "host_metrics", Name: "node_pressure_cpu_waiting_seconds_total", Grp: "cpu", Phase: 1},
		{PluginName: "host_metrics", Name: "node_memory_memtotal_bytes", Grp: "memory", Phase: 1},
	}
	for _, m := range hostMetrics {
		if err := db.SeedMetric(m); err != nil {
			t.Fatalf("seed host metric: %v", err)
		}
	}
	pid, err := db.UpsertPlugin(&storepkg.Plugin{Name: "test", DisplayName: "Test Probe", Category: "测试", Type: "exporter"})
	if err != nil {
		t.Fatalf("upsert plugin: %v", err)
	}
	for _, name := range []string{"mysql_up", "mysql_threads_running"} {
		if _, err := db.InsertMetric(&storepkg.Metric{PluginID: pid, PluginName: "test", Name: name, Source: "builtin"}); err != nil {
			t.Fatalf("insert probe metric: %v", err)
		}
	}
	return db
}

func TestExpandLegacyTargets(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	n := expandAgentConfigKnown(`[{"plugin":"test_probe","target":"m1","type":"mysql","address":"1.2.3.4:3306"}]`, known)
	if n != 1 || len(known) != 2 {
		t.Fatalf("legacy targets: want n=1 known=2, got n=%d known=%v", n, known)
	}
	if !known["mysql_up"] || !known["mysql_threads_running"] {
		t.Fatal("probe plugin metrics should expand from catalog")
	}
	if known["node_cpu_seconds_total"] {
		t.Fatal("legacy targets have no host_metrics section — host metrics must not expand")
	}
}

func TestExpandHostMetricsGroupsAllOpen(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	// 未声明 groups → 全部组默认开启（与 Agent 注册表约定一致）
	n := expandAgentConfigKnown(`{"targets":[],"host_metrics":{"enabled":true,"interval":"30s"}}`, known)
	if n != 3 {
		t.Fatalf("all-open: want 3 host metrics expanded, got n=%d known=%v", n, known)
	}
	if !known["node_memory_memtotal_bytes"] {
		t.Fatal("undeclared group must default to ON")
	}
}

func TestExpandHostMetricsGroupClosed(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	// 显式关闭 memory 组（CP4 语义：关闭分组不产生野指标）
	n := expandAgentConfigKnown(`{"targets":[],"host_metrics":{"enabled":true,"interval":"30s","groups":{"memory":false}}}`, known)
	if n != 2 {
		t.Fatalf("memory group closed: want 2, got n=%d known=%v", n, known)
	}
	if known["node_memory_memtotal_bytes"] {
		t.Fatal("explicitly closed group must not expand")
	}
	if !known["node_cpu_seconds_total"] {
		t.Fatal("cpu group (declared true) must expand")
	}
}

func TestExpandHostMetricsExclude(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	n := expandAgentConfigKnown(`{"targets":[],"host_metrics":{"enabled":true,"interval":"30s","exclude_metrics":["node_memory_memtotal_bytes"]}}`, known)
	if n != 2 {
		t.Fatalf("exclude one metric: want 2, got n=%d known=%v", n, known)
	}
	if known["node_memory_memtotal_bytes"] {
		t.Fatal("excluded metric must not expand")
	}
}

func TestExpandHostMetricsDisabled(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	n := expandAgentConfigKnown(`{"targets":[],"host_metrics":{"enabled":false,"interval":"30s"}}`, known)
	if n != 0 {
		t.Fatalf("host_metrics disabled: want 0 expansion, got n=%d known=%v", n, known)
	}
}

func TestExpandCombinedTargetsAndHostMetrics(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	content := `{"targets":[{"plugin":"test_probe","target":"m1","type":"mysql","address":"1.2.3.4:3306"}],
	             "host_metrics":{"enabled":true,"interval":"30s","groups":{"memory":false}}}`
	// n 的口径 = targets 条数(1) + host 展开条数(2)；known 集合 = probe 2 条 + cpu 2 条
	n := expandAgentConfigKnown(content, known)
	if n != 3 {
		t.Fatalf("combined: want n=3, got n=%d known=%v", n, known)
	}
	if len(known) != 4 {
		t.Fatalf("known set size: want 4, got %d (%v)", len(known), known)
	}
	if known["node_memory_memtotal_bytes"] {
		t.Fatal("closed memory group must not be in known set")
	}
}

func TestExpandGarbageContent(t *testing.T) {
	db := newReconTestDB(t)
	catalogDB = db
	known := map[string]bool{}

	if n := expandAgentConfigKnown("garbage-not-json", known); n != 0 || len(known) != 0 {
		t.Fatalf("garbage content: want 0/empty, got n=%d known=%v", n, known)
	}
}
