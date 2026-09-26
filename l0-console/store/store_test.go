package store

import (
	"fmt"
	"testing"

	"github.com/sagent/l0-console/internal/testpg"
)

// openTestDB 打开隔离 schema 的 PG 目录库（架构 D1：目录库唯一载体 PG）。
// 每个用例一个独立 schema，等价于 SQLite 时代的 t.TempDir() 隔离口径：
// 用例乱序/并发/重复跑都不互相污染。CATALOG_TEST_DSN 未设置时用例跳过。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenPostgres(testpg.Provision(t))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// 建齐全部业务表（main 启动引导同款路径）；单测不挑表，避免漏建造成偶发红
	for _, init := range []func() error{db.InitFleet, db.InitOnboard, db.InitL1Tasks, db.InitRelocations} {
		if err := init(); err != nil {
			t.Fatalf("init test db: %v", err)
		}
	}
	return db
}

// ---- 审计落库（M2-⑪）：写入 / 最近 N 条 / 时间正序 ----

func TestInsertAuditAndListAudit(t *testing.T) {
	db := openTestDB(t)

	// 覆盖度检查：时间戳必须原样落库（杜绝空数组/零填充占位符）
	entries := []struct{ time, action, target string }{
		{"2026-09-20 10:00:01", "新增采集目标", "mysql-01"},
		{"2026-09-20 10:00:02", "指标配置调整", "sagent-1"},
		{"2026-09-20 10:00:03", "重启 Agent", "sagent-2"},
	}
	for _, e := range entries {
		if err := db.InsertAudit(e.time, "admin", e.action, e.target, "scope-x", "成功"); err != nil {
			t.Fatalf("InsertAudit: %v", err)
		}
	}

	rows, err := db.ListAudit(2)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (limit), got %d", len(rows))
	}
	// ListAudit 语义：id DESC 取最近 N 条后反转为时间正序
	if rows[0].Action != "指标配置调整" || rows[1].Action != "重启 Agent" {
		t.Fatalf("recent-first ordering broken: %v / %v", rows[0].Action, rows[1].Action)
	}
	if rows[0].Time != "2026-09-20 10:00:02" {
		t.Fatalf("time not persisted as-is: %q", rows[0].Time)
	}
	if rows[0].Operator != "admin" || rows[0].Target != "sagent-1" || rows[0].Result != "成功" {
		t.Fatalf("fields corrupted: %+v", rows[0])
	}
}

// ---- Agent 配置版本链（配置轨道的持久化语义） ----

func TestAgentConfigVersionChain(t *testing.T) {
	db := openTestDB(t)

	// 未配置过的 Agent：ver=0、content 空
	ver, content, err := db.GetAgentConfig("agent-x")
	if err != nil || ver != 0 || content != "" {
		t.Fatalf("fresh agent should be ver=0/empty, got ver=%d content=%q err=%v", ver, content, err)
	}

	// 首次 Set → ver=1；再次 Set → ver=2（轨道只前进）
	if _, _, err := db.SetAgentConfig("agent-x", `{"targets":[]}`); err != nil {
		t.Fatalf("SetAgentConfig #1: %v", err)
	}
	ver2, content2, err := db.SetAgentConfig("agent-x", `{"targets":[],"host_metrics":{}}`)
	if err != nil {
		t.Fatalf("SetAgentConfig #2: %v", err)
	}
	if ver2 != 2 {
		t.Fatalf("want ver 2 after second set, got %d", ver2)
	}
	_ = content2
	gotVer, gotContent, _ := db.GetAgentConfig("agent-x")
	if gotVer != 2 || gotContent != `{"targets":[],"host_metrics":{}}` {
		t.Fatalf("latest config not readable: ver=%d content=%q", gotVer, gotContent)
	}

	// GetAgentSource：未注册返回空，注册后可读
	if src, _ := db.GetAgentSource("agent-x"); src != "" {
		t.Fatalf("unregistered agent should have empty source, got %q", src)
	}
}

func TestAgentConfigConcurrentWritesHaveUniqueVersions(t *testing.T) {
	db := openTestDB(t)
	if _, _, err := db.SetAgentConfig("agent-concurrent", `{"initial":true}`); err != nil {
		t.Fatal(err)
	}
	const writers = 32
	type result struct {
		version int
		changed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, writers)
	for i := range writers {
		go func() {
			<-start
			version, changed, err := db.SetAgentConfig("agent-concurrent", fmt.Sprintf(`{"sequence":%d}`, i))
			results <- result{version, changed, err}
		}()
	}
	close(start)
	versions := make(map[int]bool, writers)
	for range writers {
		r := <-results
		if r.err != nil || !r.changed {
			t.Errorf("distinct write: version=%d changed=%t err=%v", r.version, r.changed, r.err)
		}
		if versions[r.version] {
			t.Errorf("different contents received the same version %d", r.version)
		}
		versions[r.version] = true
	}
	version, _, err := db.GetAgentConfig("agent-concurrent")
	if err != nil || version != writers+1 {
		t.Fatalf("latest version=%d, want %d; err=%v", version, writers+1, err)
	}
}

func TestAgentConfigConcurrentIdenticalCreationIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	const writers = 16
	type result struct {
		version int
		changed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, writers)
	for range writers {
		go func() {
			<-start
			version, changed, err := db.SetAgentConfig("agent-new", `{"targets":[]}`)
			results <- result{version, changed, err}
		}()
	}
	close(start)
	changes := 0
	for range writers {
		r := <-results
		if r.err != nil || r.version != 1 {
			t.Errorf("identical write: version=%d err=%v", r.version, r.err)
		}
		if r.changed {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("content changed %d times, want 1", changes)
	}
}

// 资源删除（2026-09-21 用户要求补 API）：删行如实回报；活跃流水线守卫计数只算未终态
func TestDeleteResourceAndActiveFlowGuard(t *testing.T) {
	db := openTestDB(t)
	if err := db.InitOnboard(); err != nil {
		t.Fatalf("InitOnboard: %v", err)
	}

	if err := db.UpsertResource(&Resource{ID: "res-a", IP: "10.0.0.1"}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	// 判空守卫的边界：先建一条终态流水线，不应阻止删除
	if _, err := db.CreateFlow(&Flow{ResourceID: "res-a", Mode: "edge", Status: "done"}); err != nil {
		t.Fatalf("CreateFlow(done): %v", err)
	}
	if n, _ := db.CountActiveFlowsByResource("res-a"); n != 0 {
		t.Fatalf("终态流水线不应计入活跃数，实际 %d", n)
	}

	// blocked（未终态）流水线计入活跃数
	if _, err := db.CreateFlow(&Flow{ResourceID: "res-a", Mode: "edge", Status: "blocked"}); err != nil {
		t.Fatalf("CreateFlow(blocked): %v", err)
	}
	if n, _ := db.CountActiveFlowsByResource("res-a"); n != 1 {
		t.Fatalf("blocked 流水线应计入活跃数，实际 %d", n)
	}

	// 真删除：RowsAffected 如实回报
	ok, err := db.DeleteResource("res-a")
	if err != nil || !ok {
		t.Fatalf("DeleteResource 应成功，实际 ok=%v err=%v", ok, err)
	}
	// 再删同一 id：必须报"不存在"，不许静默成功
	if ok, _ := db.DeleteResource("res-a"); ok {
		t.Fatalf("重复删除应返回 false（不存在）")
	}
}
