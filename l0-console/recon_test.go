package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// expandAgentConfigKnown：D8 对账展开核心（红线 7 的裁判逻辑）。
// 语义：known = 目录注册集 ∪ Agent 配置展开集；未声明组默认开启、显式关闭的组剔除、
// exclude 例外剔除、host_metrics 不走插件级展开（走分组）、legacy 纯数组兼容。

func newReconTestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	db := openTestCatalog(t)

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

// vmQueryTargetData 对账口径（2026-09-27 断采误报修复）：
// ResourceID 非空 → resource_id/remote_resource_id/exported_resource_id 三标签 OR + instance 兜底；
// ResourceID 空 → 仅 instance 匹配（老目标行为不变）。
func TestVMQueryTargetDataSelectors(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		writeJSON(w, map[string]interface{}{
			"status": "success",
			"data":   map[string]interface{}{"result": []interface{}{map[string]interface{}{}}},
		})
	}))
	defer srv.Close()
	t.Setenv("VM_URL", srv.URL)

	// ① 有 ResourceID：三口径 + instance 兜底全部出现在查询里
	if !vmQueryTargetData("mysql_up", "mysql-1:3306", "order.prod.mysql.db-01") {
		t.Fatal("hit by resource identity should return true")
	}
	for _, want := range []string{
		`mysql_up{resource_id="order.prod.mysql.db-01"}`,
		`mysql_up{remote_resource_id="order.prod.mysql.db-01"}`,
		`mysql_up{exported_resource_id="order.prod.mysql.db-01"}`,
		`mysql_up{instance=~".*mysql-1.*"}`,
	} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query missing %s:\n%s", want, gotQuery)
		}
	}
	if !strings.Contains(gotQuery, " or ") {
		t.Fatalf("selectors must be OR-joined:\n%s", gotQuery)
	}

	// ② 无 ResourceID：仅 instance 匹配（兼容老目标）
	if !vmQueryTargetData("mysql_up", "10.20.0.9:3306", "") {
		t.Fatal("instance-only fallback should return true when VM reports")
	}
	if strings.Contains(gotQuery, "resource_id") {
		t.Fatalf("empty resourceID must not add identity selectors:\n%s", gotQuery)
	}
	if !strings.Contains(gotQuery, `instance=~".*10\.20\.0\.9.*"`) {
		t.Fatalf("host dots must be regex-escaped:\n%s", gotQuery)
	}

	// ③ VM 无结果 → false（断采判定口径）
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"status": "success", "data": map[string]interface{}{"result": []interface{}{}}})
	}))
	defer srv2.Close()
	t.Setenv("VM_URL", srv2.URL)
	if vmQueryTargetData("mysql_up", "mysql-1:3306", "order.prod.mysql.db-01") {
		t.Fatal("empty VM result must mean not-reporting")
	}
}

