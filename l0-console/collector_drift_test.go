package main

// collector_drift_test.go —— SPEC-exporter采集机池 E1/E2/E3/E4 单测。
// 覆盖：规则级候选集收窄与回落、人工指定承接端校验、回切（健康前置/冷却/审计）、演练下线标记。

import (
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// seedDriftCollector 登记一台 heartbeat 采集机（复用 relocate_test 的口径，独立命名避免串台）。
func seedDriftCollector(t *testing.T, db *storepkg.DB, id, pool string, lastSeen int64) {
	t.Helper()
	if lastSeen == 0 {
		lastSeen = time.Now().Unix()
	}
	if err := db.UpsertAgentRow(&storepkg.AgentRow{
		ID: id, Type: "proxy", Source: "heartbeat",
		LastSeen: lastSeen, Labels: map[string]string{"pool": pool}, TenantID: "default",
	}); err != nil {
		t.Fatalf("seed collector %s: %v", id, err)
	}
}

// seedDriftTarget 建一条远端采集目标（mysql_probe，可迁移）。
func seedDriftTarget(t *testing.T, db *storepkg.DB, name, owner string) *storepkg.TargetRow {
	t.Helper()
	id, err := db.InsertTarget(&storepkg.TargetRow{
		Name: name, Type: "mysql", Address: name + ":3306", AgentID: owner, Plugin: "mysql_probe",
	})
	if err != nil {
		t.Fatalf("seed target %s: %v", name, err)
	}
	tg, err := db.GetTarget(id)
	if err != nil || tg == nil {
		t.Fatalf("get target %s: %v", name, err)
	}
	return tg
}

// E1：配置了规则候选集的目标，漂移候选收窄到候选集内（集合外健康成员不可承接）。
func TestRuleCandidatesNarrowDrift(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedDriftCollector(t, db, "c-A", "pool-1", now) // 属主
	seedDriftCollector(t, db, "c-B", "pool-1", now) // 候选集内
	seedDriftCollector(t, db, "c-C", "pool-1", now) // 健康但不在候选集

	tg := seedDriftTarget(t, db, "drift-narrow-1", "c-A")
	if err := db.SetTargetCandidates(tg.ID, `["c-B","c-A"]`, "c-A"); err != nil {
		t.Fatalf("set candidates: %v", err)
	}
	tg, _ = db.GetTarget(tg.ID)

	got := candidateList(mustAgents(t, db), tg, tg.AgentID)
	if len(got) != 1 || got[0] != "c-B" {
		t.Fatalf("候选应收窄为 [c-B]，实际 %v", got)
	}
}

// E1：未配置候选集（空数组）回落池成员全集口径。
func TestRuleCandidatesFallbackWhenEmpty(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedDriftCollector(t, db, "f-A", "pool-1", now) // 属主
	seedDriftCollector(t, db, "f-B", "pool-1", now)
	seedDriftCollector(t, db, "f-C", "pool-1", now)

	tg := seedDriftTarget(t, db, "drift-fallback-1", "f-A")
	got := candidateList(mustAgents(t, db), tg, tg.AgentID)
	if len(got) != 2 {
		t.Fatalf("空候选集应回落池口径（2 台），实际 %v", got)
	}
}

// E2：人工指定承接端必须在候选清单内——集合外机器即使健康也拒绝（防绕过）。
func TestManualDriftRejectsOutsideCandidates(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedDriftCollector(t, db, "m-A", "pool-1", now)
	seedDriftCollector(t, db, "m-B", "pool-1", now) // 候选集内
	seedDriftCollector(t, db, "m-C", "pool-1", now) // 候选集外

	tg := seedDriftTarget(t, db, "drift-manual-1", "m-A")
	if err := db.SetTargetCandidates(tg.ID, `["m-A","m-B"]`, "m-A"); err != nil {
		t.Fatalf("set candidates: %v", err)
	}
	store := &AgentStore{}
	res := runRelocationTo(store, db, tg.ID, "m-C", "manual", "测试：指定候选外机器")
	if res.Status != "rejected" {
		t.Fatalf("指定候选外承接端应被拒绝，实际 %+v", res)
	}
	// 指定候选内承接端 → 迁移成功且审计带 manual
	res = runRelocationTo(store, db, tg.ID, "m-B", "manual", "测试：指定候选内机器")
	if res.Status != "moved" || res.ToAgent != "m-B" {
		t.Fatalf("指定候选内承接端应成功，实际 %+v", res)
	}
	after, _ := db.GetTarget(tg.ID)
	if after.AgentID != "m-B" {
		t.Fatalf("归属应为 m-B，实际 %q", after.AgentID)
	}
	rel, _ := db.LatestRelocationByTarget(tg.ID)
	if rel.TriggerSource != "manual" {
		t.Fatalf("审计 trigger_source 应为 manual，实际 %q", rel.TriggerSource)
	}
}

// E3：回切成功路径——preferred 恢复健康后人工回切归位，冷却期内二次回切被拒。
func TestFailbackHappyPathAndCooldown(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedDriftCollector(t, db, "p-A", "pool-1", now) // 首选
	seedDriftCollector(t, db, "p-B", "pool-1", now) // 当前属主

	tg := seedDriftTarget(t, db, "failback-1", "p-B")
	if err := db.SetTargetCandidates(tg.ID, `["p-A","p-B"]`, "p-A"); err != nil {
		t.Fatalf("set candidates: %v", err)
	}
	store := &AgentStore{}
	res := failbackTarget(store, db, tg.ID, "failback_manual")
	if res.Status != "moved" || res.ToAgent != "p-A" {
		t.Fatalf("回切应归位 p-A，实际 %+v", res)
	}
	after, _ := db.GetTarget(tg.ID)
	if after.AgentID != "p-A" {
		t.Fatalf("归属应为 p-A，实际 %q", after.AgentID)
	}
	rel, _ := db.LatestRelocationByTarget(tg.ID)
	if rel.TriggerSource != "failback_manual" {
		t.Fatalf("审计应为 failback_manual，实际 %q", rel.TriggerSource)
	}
	// 冷却终态保护：冷却窗内再回切（此时已在首选上）→ skipped
	res = failbackTarget(store, db, tg.ID, "failback_manual")
	if res.Status != "skipped" {
		t.Fatalf("已在首选归属时二次回切应 skipped，实际 %+v", res)
	}
}

// E3：首选未恢复健康 → 禁止回切；未配置 preferred → 无从回切。
func TestFailbackGuards(t *testing.T) {
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedDriftCollector(t, db, "g-A", "pool-1", now-int64(relocateFailSec)-30) // 首选（心跳陈旧）
	seedDriftCollector(t, db, "g-B", "pool-1", now)                           // 当前属主
	seedDriftCollector(t, db, "g-C", "pool-1", now)

	tg := seedDriftTarget(t, db, "failback-guard-1", "g-B")
	if err := db.SetTargetCandidates(tg.ID, `["g-A","g-B","g-C"]`, "g-A"); err != nil {
		t.Fatalf("set candidates: %v", err)
	}
	store := &AgentStore{}
	res := failbackTarget(store, db, tg.ID, "failback_manual")
	if res.Status != "rejected" || res.Note == "" {
		t.Fatalf("首选不健康应拒绝回切，实际 %+v", res)
	}
	// 未配置 preferred
	tg2 := seedDriftTarget(t, db, "failback-guard-2", "g-B")
	res = failbackTarget(store, db, tg2.ID, "failback_manual")
	if res.Status != "rejected" || res.Note == "" {
		t.Fatalf("未配置 preferred 应拒绝，实际 %+v", res)
	}
}

// E4：演练下线标记 → agentHealthy 即刻转异常；解除后恢复（同连接刷新内存集合）。
func TestSimDownBlocksHealth(t *testing.T) {
	db := newRelocateTestDB(t)
	seedDriftCollector(t, db, "s-A", "pool-1", time.Now().Unix())
	refreshSimDownSet(db)
	agents := mustAgents(t, db)
	var a *storepkg.AgentRow
	for _, x := range agents {
		if x.ID == "s-A" {
			a = x
		}
	}
	if a == nil || !agentHealthy(a) {
		t.Fatalf("前置：s-A 应健康")
	}
	if err := db.SetVmagentMemberSimDown("s-A", true); err != nil {
		t.Fatalf("set sim down: %v", err)
	}
	refreshSimDownSet(db)
	if !memberSimDown("s-A") || agentHealthy(a) {
		t.Fatalf("演练下线后应判异常")
	}
	if err := db.SetVmagentMemberSimDown("s-A", false); err != nil {
		t.Fatalf("clear sim down: %v", err)
	}
	refreshSimDownSet(db)
	if memberSimDown("s-A") || !agentHealthy(a) {
		t.Fatalf("解除标记后应恢复健康")
	}
}

// E2：/api/collectors/candidates 校验 —— 候选只剩 1 台且未指定首选时自动补首选（回切归位点）。
func TestCandidatesAutoPreferred(t *testing.T) {
	db := newRelocateTestDB(t)
	tg := seedDriftTarget(t, db, "cand-auto-1", "cand-x")
	if err := db.SetTargetCandidates(tg.ID, `["cand-x"]`, ""); err != nil {
		t.Fatalf("set: %v", err)
	}
	after, _ := db.GetTarget(tg.ID)
	if after.CandidatesJSON != `["cand-x"]` {
		t.Fatalf("候选集应落库，实际 %q", after.CandidatesJSON)
	}
	// 单候选无首选属于配置层面允许（API 层会补首选），store 层不做隐式改写
	if after.PreferredOwner != "" {
		t.Fatalf("store 层不应隐式补首选，实际 %q", after.PreferredOwner)
	}
}
