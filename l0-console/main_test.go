package main

import (
	"testing"
)

// parseAgentConfigDoc：三种输入形态的解析兜底（legacy 数组 / 结构化 doc / 坏内容）
// 这是配置轨道的入口解析，曾因 legacy "[]" 数组 Unmarshal 到 map 失败丢 targets 段。

func TestParseAgentConfigDocLegacyArray(t *testing.T) {
	doc := parseAgentConfigDoc(`[{"plugin":"mysql_probe","target":"mysql-01","type":"mysql","address":"1.2.3.4:3306"}]`)
	// legacy 分支的 targets 实际类型是 []map[string]interface{}（json 数组直通），非 []interface{}
	targets, ok := doc["targets"].([]map[string]interface{})
	if !ok || len(targets) != 1 {
		t.Fatalf("legacy array should map to targets, got %#v", doc["targets"])
	}
	// legacy 无 host_metrics 段 → 补默认（下发配置自描述，不依赖 Agent 端隐式约定）
	if _, ok := doc["host_metrics"]; !ok {
		t.Fatal("legacy doc must carry default host_metrics section")
	}
}

func TestParseAgentConfigDocStructured(t *testing.T) {
	in := `{"targets":[{"plugin":"mysql_probe","target":"m1","type":"mysql","address":"1.2.3.4:3306"}],
	        "host_metrics":{"enabled":true,"interval":"60s","groups":{"disk":false},"exclude_metrics":["host_x"]}}`
	doc := parseAgentConfigDoc(in)
	hm, ok := doc["host_metrics"].(map[string]interface{})
	if !ok {
		t.Fatalf("host_metrics section lost, got %#v", doc["host_metrics"])
	}
	if hm["interval"] != "60s" {
		t.Fatalf("host_metrics fields not preserved: %#v", hm)
	}
	if _, ok := doc["targets"].([]interface{}); !ok {
		t.Fatalf("targets lost, got %#v", doc["targets"])
	}
}

func TestParseAgentConfigDocStructuredMissingHostMetrics(t *testing.T) {
	doc := parseAgentConfigDoc(`{"targets":[{"plugin":"mysql_probe","target":"m1","type":"mysql","address":"1.2.3.4:3306"}]}`)
	if _, ok := doc["host_metrics"]; !ok {
		t.Fatal("structured doc without host_metrics must get default section (syncAgentConfig 兜底语义)")
	}
}

func TestParseAgentConfigDocEmptyAndGarbage(t *testing.T) {
	for _, in := range []string{"", "not-json-at-all", "null"} {
		doc := parseAgentConfigDoc(in)
		if arr, ok := doc["targets"].([]interface{}); !ok || len(arr) != 0 {
			t.Fatalf("input %q: want empty targets, got %#v", in, doc["targets"])
		}
		if _, ok := doc["host_metrics"]; !ok {
			t.Fatalf("input %q: want default host_metrics", in)
		}
	}
}

func TestNormalizeTargets(t *testing.T) {
	arr := []interface{}{map[string]interface{}{"plugin": "mysql_probe"}}
	if got := normalizeTargets(arr); len(got.([]interface{})) != 1 {
		t.Fatal("valid array should pass through")
	}
	// 非法形态（nil / map / 字符串）一律归一为空数组，杜绝下游 targets:null
	for _, bad := range []interface{}{nil, map[string]interface{}{"a": 1}, "junk"} {
		if got := normalizeTargets(bad); got == nil {
			t.Fatalf("normalizeTargets(%#v) must not return nil", bad)
		}
	}
}
