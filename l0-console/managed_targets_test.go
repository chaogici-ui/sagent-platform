package main

// managed_targets_test.go —— R0-2/R0-3/R0-4 单测。
//   R0-3：能力判定矩阵（能力位权威 / 版本兜底 / fail-closed）+ syncAgentConfig 老.Agent gate
//   R0-4：standby 预置派生口径（池内非属主健康 mysql_probe 目标 + 上限截断）

import (
	"encoding/json"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

func TestAgentSupportsManagedTargetsMatrix(t *testing.T) {
	cases := []struct {
		version string
		stats   map[string]interface{}
		want    bool
		note    string
	}{
		{"0.6.0", nil, true, "版本兜底达标"},
		{"v0.6.1", nil, true, "v 前缀容忍"},
		{"0.5.9", nil, false, "版本不足"},
		{"0.4.0", nil, false, "老 Agent"},
		{"", nil, false, "版本不可考 fail-closed"},
		{"not-a-version", nil, false, "版本非法 fail-closed"},
		{"0.4.0", map[string]interface{}{"capabilities": []interface{}{"managed_targets"}}, true, "能力位权威（低版本声明也放行）"},
		{"", map[string]interface{}{"capabilities": []string{"managed_targets"}}, true, "能力位 []string 形态"},
		{"0.6.0", map[string]interface{}{"capabilities": []interface{}{"other"}}, true, "无能力位但版本达标"},
	}
	for _, c := range cases {
		if got := agentSupportsManagedTargets(c.version, c.stats); got != c.want {
			t.Fatalf("%s: version=%q want %t got %t", c.note, c.version, c.want, got)
		}
	}
}

// R0-3 核心：老 Agent（0.4.0 无能力位）的期望配置 targets 置空——绝不打挂老 Agent
func TestSyncAgentConfigGateOldAgent(t *testing.T) {
	db := testOnboardDB(t)
	if err := db.UpsertAgentRow(&storepkg.AgentRow{
		ID: "collector-old", Type: "proxy", Source: "heartbeat",
		LastSeen: time.Now().Unix(), Version: "0.4.0", TenantID: "default",
		Labels: map[string]string{"pool": "p1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: "db-old", Type: "mysql", Address: "u:p@tcp(h:3306)/",
		AgentID: "collector-old", Plugin: "mysql_probe", ResourceID: "res-old",
		ParamsJSON: `{"dsn":"u:p@tcp(h:3306)/"}`,
	}); err != nil {
		t.Fatal(err)
	}
	syncAgentConfig(NewAgentStore(), db, "collector-old")
	_, content, err := db.GetAgentConfig("collector-old")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Targets []map[string]interface{} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Targets) != 0 {
		t.Fatalf("old agent must receive empty targets, got %d entries", len(doc.Targets))
	}
}

// R0-4：有能力采集机的 targets 段 = active 属主目标 + standby 预置（池内非属主）
func TestSyncAgentConfigIncludesStandbyPresets(t *testing.T) {
	db := testOnboardDB(t)
	seedCapable := func(id string) {
		t.Helper()
		if err := db.UpsertAgentRow(&storepkg.AgentRow{
			ID: id, Type: "proxy", Source: "heartbeat",
			LastSeen: time.Now().Unix(), Version: "0.6.0", TenantID: "default",
			Labels: map[string]string{"pool": "p1"},
			Stats:  map[string]interface{}{"capabilities": []string{"managed_targets"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCapable("collector-a")
	seedCapable("collector-b")
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: "db-shared", Type: "mysql", Address: "u:p@tcp(h:3306)/",
		AgentID: "collector-a", Plugin: "mysql_probe", ResourceID: "res-shared",
		ParamsJSON: `{"dsn":"u:p@tcp(h:3306)/"}`,
	}); err != nil {
		t.Fatal(err)
	}
	readTargets := func(agentID string) []map[string]interface{} {
		t.Helper()
		syncAgentConfig(NewAgentStore(), db, agentID)
		_, content, err := db.GetAgentConfig(agentID)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Targets []map[string]interface{} `json:"targets"`
		}
		if err := json.Unmarshal([]byte(content), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Targets
	}
	// 属主：1 条 active，无 standby（自己是属主不预置）
	aTargets := readTargets("collector-a")
	if len(aTargets) != 1 || aTargets[0]["desired_state"] != "active" {
		t.Fatalf("owner should hold exactly 1 active target, got %#v", aTargets)
	}
	// 候选：0 active + 1 standby（同池健康非属主）
	bTargets := readTargets("collector-b")
	if len(bTargets) != 1 || bTargets[0]["desired_state"] != "standby" {
		t.Fatalf("candidate should hold exactly 1 standby preset, got %#v", bTargets)
	}
	if bTargets[0]["resource_id"] != "res-shared" {
		t.Fatalf("standby preset must carry resource identity, got %#v", bTargets[0])
	}
}

// R0-4 派生口径：非 mysql_probe 不预置 / 属主不预置 / 上限截断
func TestDeriveStandbyPresetsFiltersAndBounds(t *testing.T) {
	db := testOnboardDB(t)
	seedCollector(t, db, "c-owner", "default", "p1", "", 0)
	seedCollector(t, db, "c-cand", "default", "p1", "", 0)
	seedCollector(t, db, "c-extra", "default", "p1", "", 0)
	agents, _ := db.ListAgentRows()
	// 3 台全部按 0.6.0 能力处理（测试内直接改口径不等价，这里只测派生函数本体）
	mkAgent := func(id, version string) *storepkg.AgentRow {
		return &storepkg.AgentRow{ID: id, Type: "proxy", Source: "heartbeat",
			LastSeen: time.Now().Unix(), Version: version, TenantID: "default",
			Labels: map[string]string{"pool": "p1"}}
	}
	_ = agents
	pool := []*storepkg.AgentRow{mkAgent("c-owner", "0.6.0"), mkAgent("c-cand", "0.6.0")}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.InsertTarget(&storepkg.TargetRow{Name: "t-mysql", Type: "mysql", Address: "x:3306",
		AgentID: "c-owner", Plugin: "mysql_probe", ResourceID: "r-mysql"})
	must(err)
	_, err = db.InsertTarget(&storepkg.TargetRow{Name: "t-prom", Type: "prometheus", Address: "http://x:9100/metrics",
		AgentID: "c-owner", Plugin: "prometheus_scrape", ResourceID: "r-prom"})
	must(err)
	targets, _ := db.ListTargets()
	got := deriveStandbyPresets(pool, targets, "c-cand", 8)
	if len(got) != 1 || got[0].ResourceID != "r-mysql" {
		t.Fatalf("only mysql_probe targets presettable, got %#v", got)
	}
	if g := deriveStandbyPresets(pool, targets, "c-owner", 8); len(g) != 0 {
		t.Fatalf("owner must not preset own targets, got %#v", g)
	}
	// 上限截断：max=0 → 不预置
	if g := deriveStandbyPresets(pool, targets, "c-cand", 0); len(g) != 0 {
		t.Fatalf("max=0 must preset nothing, got %#v", g)
	}
}
