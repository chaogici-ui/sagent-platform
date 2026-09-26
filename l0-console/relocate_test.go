package main

// relocate_test.go —— HA-3 跨 L1 归属迁移单测。
// 覆盖决策点：可迁边界（仅远端采集）、候选选择（同租户+同池+健康 heartbeat+是采集机）、
// 池内不足不迁移（G6）、冷却终态保护、先新后旧防双写、迁移取证与审计落库。

import (
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// newRelocateTestDB 建立隔离 schema 的 catalog DB（fleet + relocations 齐备）。
func newRelocateTestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	return openTestCatalog(t)
}

// seedCollector 登记一台健康 heartbeat 采集机（身份 type=proxy + 显式 labels.pool）。
func seedCollector(t *testing.T, db *storepkg.DB, id, tenant, pool, region string, lastSeen int64) {
	t.Helper()
	if lastSeen == 0 {
		lastSeen = time.Now().Unix()
	}
	labels := map[string]string{"pool": pool}
	if region != "" {
		labels["region"] = region
	}
	if err := db.UpsertAgentRow(&storepkg.AgentRow{
		ID: id, Type: "proxy", Source: "heartbeat",
		LastSeen: lastSeen, Labels: labels, TenantID: tenant,
	}); err != nil {
		t.Fatalf("seed collector %s: %v", id, err)
	}
}

// seedEdgeAgent 登记一台健康 heartbeat 普通 SAgent（type=edge，非采集机，不入漂移候选）。
func seedEdgeAgent(t *testing.T, db *storepkg.DB, id, tenant, region string, lastSeen int64) {
	t.Helper()
	if lastSeen == 0 {
		lastSeen = time.Now().Unix()
	}
	if err := db.UpsertAgentRow(&storepkg.AgentRow{
		ID: id, Type: "edge", Source: "heartbeat",
		LastSeen: lastSeen, Labels: map[string]string{"region": region}, TenantID: tenant,
	}); err != nil {
		t.Fatalf("seed edge agent %s: %v", id, err)
	}
}

// seedRelocAgent 登记一台健康 heartbeat 采集机（池与地域同名，覆盖既有用例口径）。
func seedRelocAgent(t *testing.T, db *storepkg.DB, id, tenant, region string, lastSeen int64) {
	t.Helper()
	seedCollector(t, db, id, tenant, region, region, lastSeen)
}

// mustAgents 取 agent 快照（候选筛选的输入）。
func mustAgents(t *testing.T, db *storepkg.DB) []*storepkg.AgentRow {
	t.Helper()
	agents, err := db.ListAgentRows()
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	return agents
}

// TestIsRelocatableTarget 决策点5：仅远端采集可跨 L1，本地能力插件不可迁；未分派不可迁。
func TestIsRelocatableTarget(t *testing.T) {
	// 远端采集（如 mysql_probe）：已分派 agent 可迁
	remote := &storepkg.TargetRow{ID: 1, Name: "t", AgentID: "sagent-A", Plugin: "mysql_probe", Type: "mysql"}
	if !isRelocatableTarget(remote) {
		t.Fatal("remote-plugin assigned target should be relocatable")
	}
	// 本地能力插件：不可迁
	for _, lp := range []string{"host_metrics", "docker_metrics", "sagent", "sagent_health", "port_checker"} {
		if isRelocatableTarget(&storepkg.TargetRow{AgentID: "sagent-A", Plugin: lp}) {
			t.Fatalf("local-only plugin %s must not be relocatable", lp)
		}
	}
	// 未分派：不可迁
	if isRelocatableTarget(&storepkg.TargetRow{Plugin: "mysql_probe"}) {
		t.Fatal("unassigned target must not be relocatable")
	}
}

// TestPickCandidateFiltersByTenantRegionAndHealth 候选集收窄：同租户+同地域+健康 heartbeat，排除属主。
func TestPickCandidateFiltersByTenantRegionAndHealth(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	// 同租户同地域健康：A
	seedRelocAgent(t, db, "sagent-A", "tenant-a", "cn-east", now)
	// 同租户但不同地域：B（region 不匹配，排除）
	seedRelocAgent(t, db, "sagent-B", "tenant-a", "cn-west", now)
	// 同地域但不同租户：C（排除）
	seedRelocAgent(t, db, "sagent-C", "tenant-b", "cn-east", now)
	// 同租户同地域但陈旧（last_seen 超阈值）：D（排除）
	seedRelocAgent(t, db, "sagent-D", "tenant-a", "cn-east", now-int64(relocateFailSec)-10)
	// 非 heartbeat 源：E（排除）
	seedRelocAgent(t, db, "sagent-E", "tenant-a", "cn-east", now)
	if err := db.UpsertAgentRow(&storepkg.AgentRow{ID: "sagent-E", Type: "edge", Source: "docker", LastSeen: now, Labels: map[string]string{"region": "cn-east"}, TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}

	tgt := &storepkg.TargetRow{ID: 9, Name: "db1", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}
	to, err := pickCandidate(db, tgt, tgt.AgentID)
	if err != nil {
		t.Fatalf("pickCandidate: %v", err)
	}
	if to != "sagent-A" {
		t.Fatalf("want sole candidate sagent-A, got %q", to)
	}
}

// TestPickCandidateNoneWhenNoHealthyMatch 无同租户同地域健康候选时返回空串。
func TestPickCandidateNoneWhenNoHealthyMatch(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedRelocAgent(t, db, "sagent-A", "tenant-a", "cn-east", now-int64(relocateFailSec)-5) // 陈旧
	tgt := &storepkg.TargetRow{ID: 1, AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}
	if to, _ := pickCandidate(db, tgt, tgt.AgentID); to != "" {
		t.Fatalf("want no candidate, got %q", to)
	}
}

// TestRunRelocationMovesOwnershipAndRecordsHistory 执行迁移：归属切换 + 历史取证 + 审计。
func TestRunRelocationMovesOwnershipAndRecordsHistory(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	seedRelocAgent(t, db, "sagent-A", "tenant-a", "cn-east", now)
	seedRelocAgent(t, db, "sagent-owner", "tenant-a", "cn-east", now)
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	var tid int64 = 1

	res := runRelocation(store, db, tid, "manual test")
	if res.Status != "moved" || res.ToAgent != "sagent-A" {
		t.Fatalf("want moved → sagent-A, got %+v", res)
	}
	// 归属已切换
	tgt, _ := db.GetTarget(tid)
	if tgt.AgentID != "sagent-A" {
		t.Fatalf("owner should now be sagent-A, got %q", tgt.AgentID)
	}
	// 新端 config 轨已接管（目标并入 sagent-A 的 agent_config）
	_, content, _ := db.GetAgentConfig("sagent-A")
	if content == "" {
		t.Fatal("new owner agent_config should contain target after takeover")
	}
	// 历史已取证
	rows, _ := db.ListRelocationsByTarget(tid, 10)
	if len(rows) != 1 || rows[0].Status != "moved" || rows[0].ToAgent != "sagent-A" {
		t.Fatalf("want 1 moved record, got %+v", rows)
	}
}

// TestCooldownSkipsDuplicateMigration 冷却期内同目标不再迁移（终态保护）。
func TestCooldownSkipsDuplicateMigration(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	seedRelocAgent(t, db, "sagent-A", "tenant-a", "cn-east", now)
	seedRelocAgent(t, db, "sagent-owner", "tenant-a", "cn-east", now)
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}); err != nil {
		t.Fatal(err)
	}
	// 记录一条刚发生（created_at=now）的迁移 → 处于冷却期
	if _, err := db.RecordRelocation(&storepkg.RelocationRow{TargetID: 1, ToAgent: "sagent-A", FromAgent: "sagent-owner", Status: "moved", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	res := runRelocation(store, db, 1, "duplicate attempt")
	if res.Status != "skipped" {
		t.Fatalf("cooling period should skip, got status=%s", res.Status)
	}
	// 归属未被改动
	tgt, _ := db.GetTarget(1)
	if tgt.AgentID != "sagent-owner" {
		t.Fatalf("owner must remain sagent-owner during cooldown, got %q", tgt.AgentID)
	}
}

// TestCandidateListExcludesNonCollectors R1：普通 SAgent（type=edge）不入漂移候选，即便健康且同租户同地域。
func TestCandidateListExcludesNonCollectors(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedCollector(t, db, "sagent-C", "tenant-a", "cn-east", "cn-east", now)
	// 两台健康普通 SAgent：不是采集机，不得成为承接端
	seedEdgeAgent(t, db, "sagent-E1", "tenant-a", "cn-east", now)
	seedEdgeAgent(t, db, "sagent-E2", "tenant-a", "cn-east", now)

	tgt := &storepkg.TargetRow{ID: 1, AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}
	got := candidateList(mustAgents(t, db), tgt, tgt.AgentID)
	if len(got) != 1 || got[0] != "sagent-C" {
		t.Fatalf("only collector should be candidate, got %v", got)
	}
}

// TestRelocationStaysWithinPool R1：候选收窄到同池，跨池采集机（同租户同健康）不入候选。
func TestRelocationStaysWithinPool(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	// 属主与候选同在 cn-east 池；cn-west 池的采集机不得被选（漂移不跨池）
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-A", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-B", "tenant-a", "cn-west", "cn-west", now)

	// Region 留空以隔离池边界（只按 pool 收窄）
	tgt := &storepkg.TargetRow{ID: 1, AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a"}
	got := candidateList(mustAgents(t, db), tgt, tgt.AgentID)
	if len(got) != 1 || got[0] != "sagent-A" {
		t.Fatalf("candidate must stay within pool cn-east, got %v", got)
	}
}

// TestPoolInsufficientBlocksRelocation R3/G6：池内健康采集机 <2 时不迁移（即便存在健康候选），
// 补齐第二台后同一目标即可迁移。
func TestPoolInsufficientBlocksRelocation(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	// 属主陈旧 + 池内仅 1 台健康采集机 → 池内健康数=1 <2
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now-int64(relocateFailSec)-10)
	seedCollector(t, db, "sagent-A", "tenant-a", "cn-east", "cn-east", now)
	tid, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"})
	if err != nil {
		t.Fatal(err)
	}
	// 确有健康候选——证明拦截来自池内不足保护，而非无候选
	if got := candidateList(mustAgents(t, db), &storepkg.TargetRow{AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}, "sagent-owner"); len(got) != 1 {
		t.Fatalf("precondition: healthy candidate should exist, got %v", got)
	}
	res := runRelocation(store, db, tid, "pool insufficient")
	if res.Status != "rejected" || res.PoolHealthy != 1 {
		t.Fatalf("pool<2 must reject relocation, got %+v", res)
	}
	if tgt, _ := db.GetTarget(tid); tgt.AgentID != "sagent-owner" {
		t.Fatalf("owner must remain unchanged when pool insufficient, got %q", tgt.AgentID)
	}
	// 补齐第二台健康采集机 → 池内健康数=2 → 迁移放行
	seedCollector(t, db, "sagent-B", "tenant-a", "cn-east", "cn-east", now)
	res2 := runRelocation(store, db, tid, "pool recovered")
	if res2.Status != "moved" || res2.PoolHealthy != 2 {
		t.Fatalf("pool recovered must allow relocation, got %+v", res2)
	}
}

// TestCollectorTypeAliases R1：采集机身份认别名——演示环境既有取值 collector_proxy 与规范值 proxy 同等对待；
// edge 非采集机，labels.role=collector 亦可认定。
func TestCollectorTypeAliases(t *testing.T) {
	for _, ty := range []string{"proxy", "collector", "collector_proxy"} {
		if !isCollectorAgent(&storepkg.AgentRow{ID: "x", Type: ty}) {
			t.Errorf("type=%s 应认作采集机", ty)
		}
	}
	if isCollectorAgent(&storepkg.AgentRow{ID: "e", Type: "edge"}) {
		t.Error("edge 不得认作采集机")
	}
	if !isCollectorAgent(&storepkg.AgentRow{ID: "l", Type: "edge", Labels: map[string]string{"role": "collector"}}) {
		t.Error("labels.role=collector 应认作采集机")
	}
}

// TestAgentHealthyThresholdByRole R5：失效阈值按身份分档——同为 40s 无心跳，
// 采集机（10s 心跳）判失效、普通 SAgent（30s 心跳）仍健康。提速只作用于采集机，不误伤普通 SAgent。
func TestAgentHealthyThresholdByRole(t *testing.T) {
	now := time.Now().Unix()
	col := &storepkg.AgentRow{ID: "c1", Type: "proxy", Source: "heartbeat", LastSeen: now - 40}
	edge := &storepkg.AgentRow{ID: "e1", Type: "edge", Source: "heartbeat", LastSeen: now - 40}
	if agentHealthy(col) {
		t.Errorf("采集机 40s 无心跳应判失效（阈值 %ds）", relocateFailSec)
	}
	if !agentHealthy(edge) {
		t.Errorf("普通 SAgent 40s 无心跳应仍健康（阈值 %ds）", relocateEdgeSec)
	}
}

// TestAutoRelocateMigratesStaleOwnerCollector R4：属主采集机失效、池内 ≥2 台健康承接端 →
// 自动调度迁移该目标到池内健康采集机（无人干预）。
func TestAutoRelocateMigratesStaleOwnerCollector(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	// 属主陈旧失效；同池另有两台健康采集机（池内健康数=2，满足承接条件）
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now-int64(relocateFailSec)-10)
	seedCollector(t, db, "sagent-A", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-B", "tenant-a", "cn-east", "cn-east", now)
	tid, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"})
	if err != nil {
		t.Fatal(err)
	}
	autoRelocate(store, db)
	tgt, _ := db.GetTarget(tid)
	if tgt.AgentID != "sagent-A" {
		t.Fatalf("失效属主采集机的目标应自动漂移到池内健康采集机 sagent-A，实际 %q", tgt.AgentID)
	}
}

// TestAutoRelocateSkipsWhenPoolInsufficient R4/G6：属主失效但池内健康采集机 <2 → 自动调度不迁移，
// 目标留在原属主（转由 collector_pool_degraded 告警承担）。
func TestAutoRelocateSkipsWhenPoolInsufficient(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now-int64(relocateFailSec)-10)
	seedCollector(t, db, "sagent-A", "tenant-a", "cn-east", "cn-east", now) // 池内唯一健康端
	tid, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"})
	if err != nil {
		t.Fatal(err)
	}
	autoRelocate(store, db)
	if tgt, _ := db.GetTarget(tid); tgt.AgentID != "sagent-owner" {
		t.Fatalf("池内不足时不得迁移，实际 %q", tgt.AgentID)
	}
}

// TestRunRelocationRejectsWhenNoCandidateOrLocalOnly 无候选可迁时 rejected；本地能力插件 rejected。
func TestRunRelocationRejectsWhenNoCandidateOrLocalOnly(t *testing.T) {
	db := newRelocateTestDB(t)
	store := NewAgentStore()
	// 无同租户候选
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}); err != nil {
		t.Fatal(err)
	}
	if res := runRelocation(store, db, 1, "no cand"); res.Status != "rejected" {
		t.Fatalf("no-candidate case want rejected, got %s", res.Status)
	}
	// 本地能力插件
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "host1", Type: "host", Address: "localhost", AgentID: "sagent-owner", Plugin: "host_metrics", TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	if res := runRelocation(store, db, 2, "local-only"); res.Status != "rejected" || res.Relocatable {
		t.Fatalf("local-only want rejected+not relocatable, got %+v", res)
	}
}
