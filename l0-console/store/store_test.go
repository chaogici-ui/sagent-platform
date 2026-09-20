package store

import (
	"path/filepath"
	"testing"
)

// openTestDB 打开临时 SQLite（随进程结束自动清理目录）
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// fleet 三表（agents/targets/agent_config）由 InitFleet 显式建表（main 启动引导同款路径）
	if err := db.InitFleet(); err != nil {
		t.Fatalf("InitFleet: %v", err)
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
