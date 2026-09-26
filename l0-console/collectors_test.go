package main

// collectors_test.go —— R2 采集机选机单测。
// 覆盖：池聚合与承载数、健康过滤与排序、自动挑机（负载最低 / 并列取 id 最小）、
// 人工改选端点（拒绝非采集机与跨租户、改绑已建采集目标）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// newCollectorTestDB 建立隔离 schema 的 catalog DB（fleet + onboard + relocations 齐备）。
func newCollectorTestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	return openTestCatalog(t)
}

// seedRemoteTarget 登记一个远端采集目标（占用某采集机的承载数）。
func seedRemoteTarget(t *testing.T, db *storepkg.DB, name, agentID, tenant, region string) int64 {
	t.Helper()
	return seedFlowRemoteTarget(t, db, name, agentID, tenant, region, 0)
}

// seedFlowRemoteTarget 同上，并标记由哪条接入流水线创建（改选承载机时按 FlowID 改绑）。
func seedFlowRemoteTarget(t *testing.T, db *storepkg.DB, name, agentID, tenant, region string, flowID int64) int64 {
	t.Helper()
	id, err := db.InsertTarget(&storepkg.TargetRow{
		Name: name, Type: "mysql", Address: "10.0.0.9:3306", Plugin: "mysql_probe",
		AgentID: agentID, TenantID: tenant, Region: region, FlowID: flowID,
	})
	if err != nil {
		t.Fatalf("insert target %s: %v", name, err)
	}
	return id
}

// TestCollectorPoolViews 池聚合：按池分组、只收采集机、只收同租户、承载数按可迁目标计、健康数达标判定。
func TestCollectorPoolViews(t *testing.T) {
	db := newCollectorTestDB(t)
	now := time.Now().Unix()
	stale := now - int64(relocateFailSec) - 10
	seedCollector(t, db, "sagent-c1", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-c2", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-c3", "tenant-a", "cn-west", "cn-west", stale)
	seedCollector(t, db, "sagent-c4", "tenant-a", "cn-west", "cn-west", now) // 池内仅 1 台健康 → 仍判不足
	seedEdgeAgent(t, db, "sagent-edge", "tenant-a", "cn-east", now)   // 非采集机：不入候选
	seedCollector(t, db, "sagent-b1", "tenant-b", "cn-east", "cn-east", now) // 跨租户：不入候选

	// c1 承载 2 个远端目标；c2 承载 0 个；edge 上的目标不计入采集机承载
	seedRemoteTarget(t, db, "db1", "sagent-c1", "tenant-a", "cn-east")
	seedRemoteTarget(t, db, "db2", "sagent-c1", "tenant-a", "cn-east")
	seedRemoteTarget(t, db, "db3", "sagent-edge", "tenant-a", "cn-east")
	// 本地能力插件目标不计入承载数（不可跨机迁移，不占采集机产能）
	if _, err := db.InsertTarget(&storepkg.TargetRow{
		Name: "hm", Type: "host_metrics", Address: "10.0.0.9", Plugin: "host_metrics",
		AgentID: "sagent-c2", TenantID: "tenant-a", Region: "cn-east",
	}); err != nil {
		t.Fatal(err)
	}

	views := collectorPoolViews(db, "tenant-a")
	if len(views) != 2 {
		t.Fatalf("expect 2 pools (cn-east/cn-west), got %d: %+v", len(views), views)
	}
	byPool := map[string]CollectorPoolView{}
	for _, v := range views {
		byPool[v.Pool] = v
	}
	east := byPool["cn-east"]
	if east.HealthyCount != 2 || east.Insufficient {
		t.Fatalf("cn-east should be healthy (2), got healthy=%d insufficient=%v", east.HealthyCount, east.Insufficient)
	}
	if len(east.Candidates) != 2 {
		t.Fatalf("cn-east should hold 2 collectors, got %d: %+v", len(east.Candidates), east.Candidates)
	}
	// 健康者在前、同组按负载升序 → c2(0) 在 c1(2) 之前
	if east.Candidates[0].AgentID != "sagent-c2" || east.Candidates[0].Load != 0 {
		t.Fatalf("least-loaded healthy collector must lead, got %+v", east.Candidates[0])
	}
	if east.Candidates[1].AgentID != "sagent-c1" || east.Candidates[1].Load != 2 {
		t.Fatalf("c1 load should count only relocatable targets, got %+v", east.Candidates[1])
	}
	west := byPool["cn-west"]
	if west.HealthyCount != 1 || !west.Insufficient {
		t.Fatalf("cn-west with 1 healthy collector must be flagged insufficient, got %+v", west)
	}
}

// TestCollectorPoolViewsExcludesDemoSeedRows R2：source=docker 的演示种子行（如 sagent-proxy，
// 与真实采集机同容器、无心跳）不得进入池清单——否则会出现"永不健康的幽灵候选"与空池名。
func TestCollectorPoolViewsExcludesDemoSeedRows(t *testing.T) {
	db := newCollectorTestDB(t)
	now := time.Now().Unix()
	seedCollector(t, db, "sagent-real", "tenant-a", "cn-east", "cn-east", now)
	if err := db.UpsertAgentRow(&storepkg.AgentRow{
		ID: "sagent-proxy", Type: "proxy", Source: "docker",
		LastSeen: now, Labels: map[string]string{"idc": "idc-a"}, TenantID: "tenant-a",
	}); err != nil {
		t.Fatal(err)
	}

	views := collectorPoolViews(db, "tenant-a")
	if len(views) != 1 {
		t.Fatalf("demo seed row must not create a pool, got %d pools: %+v", len(views), views)
	}
	if views[0].Pool != "cn-east" || len(views[0].Candidates) != 1 || views[0].Candidates[0].AgentID != "sagent-real" {
		t.Fatalf("only the real heartbeat collector may appear, got %+v", views[0])
	}
}

// TestAutoPickCollectorLeastLoadedHealthy 自动挑机：健康 + 采集机 + 同租户，负载最低；并列取 id 最小；无健康端不硬塞。
func TestAutoPickCollectorLeastLoadedHealthy(t *testing.T) {
	db := newCollectorTestDB(t)
	now := time.Now().Unix()
	stale := now - int64(relocateFailSec) - 10
	seedCollector(t, db, "sagent-busy", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-free", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-dead", "tenant-a", "cn-east", "cn-east", stale)
	seedEdgeAgent(t, db, "sagent-edge", "tenant-a", "cn-east", now)

	seedRemoteTarget(t, db, "db1", "sagent-busy", "tenant-a", "cn-east")
	seedRemoteTarget(t, db, "db2", "sagent-busy", "tenant-a", "cn-east")
	seedRemoteTarget(t, db, "db3", "sagent-dead", "tenant-a", "cn-east")

	got, pool, reason := autoPickCollector(db, "tenant-a")
	if got != "sagent-free" {
		t.Fatalf("auto pick must choose least-loaded healthy collector, got %q (%s)", got, reason)
	}
	if pool != "cn-east" {
		t.Fatalf("pool should be cn-east, got %q", pool)
	}

	// 并列负载 0：取 id 最小（确定性，便于取证复现）
	seedCollector(t, db, "sagent-a0", "tenant-a", "cn-east", "cn-east", now)
	if got, _, _ := autoPickCollector(db, "tenant-a"); got != "sagent-a0" {
		t.Fatalf("tie must break on smallest id, got %q", got)
	}

	// 全池无健康采集机：不硬塞（返回空串，交人工改选）
	db2 := newCollectorTestDB(t)
	seedCollector(t, db2, "sagent-dead", "tenant-a", "cn-east", "cn-east", stale)
	if got, _, _ := autoPickCollector(db2, "tenant-a"); got != "" {
		t.Fatalf("no healthy collector must yield empty pick, got %q", got)
	}
}

// TestCollectorSelectionEndpoint 人工改选端点：拒绝非采集机 / 跨租户；合法选择落库并改绑已建目标。
func TestCollectorSelectionEndpoint(t *testing.T) {
	testOnboardCfg(t)
	if _, err := loadFlowTemplates(defaultFlowTemplateDir); err != nil {
		t.Fatalf("load flow templates: %v", err)
	}
	db := newCollectorTestDB(t)
	store := NewAgentStore()
	now := time.Now().Unix()
	seedCollector(t, db, "sagent-old", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-new", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-other", "tenant-b", "cn-east", "cn-east", now)
	seedEdgeAgent(t, db, "sagent-edge", "tenant-a", "cn-east", now)

	if err := db.UpsertResource(&storepkg.Resource{
		ID: "10.1.2.3", Name: "mysql-a", ResourceType: "host", Role: "business",
		Source: "manual", TenantID: "tenant-a",
	}); err != nil {
		t.Fatal(err)
	}
	// UpsertResource 不写租户（租户走独立入口，架构 D3），这里显式落 tenant-a
	if err := db.UpdateResourceTenant("10.1.2.3", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	flowID, err := db.CreateFlow(&storepkg.Flow{
		ResourceID: "10.1.2.3", ResourceIP: "10.1.2.3", Mode: "remote", TemplateID: "remote",
		AgentID: "sagent-old", Status: "running",
		Steps: []storepkg.FlowStepSnapshot{{ID: "pick_proxy", Title: "选择代理机", Atom: "pick_agent", Scope: "platform"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 目标由本流水线创建（改选时按 FlowID 改绑）
	tid := seedFlowRemoteTarget(t, db, "db-a", "sagent-old", "tenant-a", "cn-east", flowID)

	mux := http.NewServeMux()
	registerCollectorRoutes(mux, store, db)
	post := func(body string) map[string]interface{} {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/onboard/flow/collector", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		out := map[string]interface{}{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
		return out
	}

	// 非采集机（普通 edge Agent）→ 拒绝
	if out := post(`{"flow_id":` + itoa64(flowID) + `,"agent_id":"sagent-edge"}`); out["ok"] == true {
		t.Fatalf("non-collector must be rejected, got %+v", out)
	}
	// 跨租户采集机 → 拒绝
	if out := post(`{"flow_id":` + itoa64(flowID) + `,"agent_id":"sagent-other"}`); out["ok"] == true {
		t.Fatalf("cross-tenant collector must be rejected, got %+v", out)
	}
	// 合法改选 → 落库 + 已建目标改绑
	out := post(`{"flow_id":` + itoa64(flowID) + `,"agent_id":"sagent-new","reason":"旧机维护"}`)
	if out["ok"] != true {
		t.Fatalf("valid selection must succeed, got %+v", out)
	}
	f, _ := db.GetFlow(flowID)
	if f.AgentID != "sagent-new" {
		t.Fatalf("flow.agent_id should be rebound, got %q", f.AgentID)
	}
	tgt, _ := db.GetTarget(tid)
	if tgt.AgentID != "sagent-new" {
		t.Fatalf("existing target should follow the new collector, got %q", tgt.AgentID)
	}
	if rebound, _ := out["rebound_targets"].(float64); rebound != 1 {
		t.Fatalf("response should report 1 rebound target, got %+v", out["rebound_targets"])
	}
}