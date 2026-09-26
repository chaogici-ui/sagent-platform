package store

import (
	"database/sql"
	"fmt"
	"time"
)

// RelocationRow HA-3 跨 L1 归属迁移的一条记录（L0 侧证据 + 冷却判定来源）。
type RelocationRow struct {
	ID         int64
	TargetID   int64
	TargetName string
	FromAgent  string
	ToAgent    string
	Region     string
	TenantID   string
	Reason     string
	Status     string // moved | rejected | skipped（decision 结果）
	Note       string
	CreatedAt  int64 // unix 秒
}

const createRelocations = `CREATE TABLE IF NOT EXISTS relocations (
  id SERIAL PRIMARY KEY,
  target_id INTEGER DEFAULT 0,
  target_name TEXT DEFAULT '',
  from_agent TEXT DEFAULT '',
  to_agent TEXT DEFAULT '',
  region TEXT DEFAULT '',
  tenant_id TEXT DEFAULT 'default',
  reason TEXT DEFAULT '',
  status TEXT DEFAULT 'moved',
  note TEXT DEFAULT '',
  created_at INTEGER DEFAULT 0
)`

// InitRelocations 建迁移记录表（幂等：CREATE TABLE IF NOT EXISTS）。
func (s *DB) InitRelocations() error {
	if _, err := s.db.Exec(createRelocations); err != nil {
		return err
	}
	return nil
}

// RecordRelocation 落一条迁移记录，返回新 id。
func (s *DB) RecordRelocation(r *RelocationRow) (int64, error) {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	if r.TenantID == "" {
		r.TenantID = "default"
	}
	if r.Status == "" {
		r.Status = "moved"
	}
	// PG 无 LastInsertId：统一走 insertID（追加 RETURNING id）
	return s.insertID(`INSERT INTO relocations(target_id,target_name,from_agent,to_agent,region,tenant_id,reason,status,note,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.TargetID, r.TargetName, r.FromAgent, r.ToAgent, r.Region, r.TenantID, r.Reason, r.Status, r.Note, r.CreatedAt)
}

// ListRelocationsByTarget 某目标的历史迁移记录（从新到旧）。
func (s *DB) ListRelocationsByTarget(targetID int64, limit int) ([]*RelocationRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,target_id,COALESCE(target_name,''),COALESCE(from_agent,''),COALESCE(to_agent,''),COALESCE(region,''),COALESCE(tenant_id,'default'),COALESCE(reason,''),COALESCE(status,''),COALESCE(note,''),COALESCE(created_at,0) FROM relocations WHERE target_id=? ORDER BY id DESC LIMIT ?`, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RelocationRow{}
	for rows.Next() {
		r := &RelocationRow{}
		if err := rows.Scan(&r.ID, &r.TargetID, &r.TargetName, &r.FromAgent, &r.ToAgent, &r.Region, &r.TenantID, &r.Reason, &r.Status, &r.Note, &r.CreatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// LatestRelocationByTarget 某目标最近一次迁移记录（用于冷却判定；无则 nil）。
func (s *DB) LatestRelocationByTarget(targetID int64) (*RelocationRow, error) {
	var r RelocationRow
	err := s.db.QueryRow(`SELECT id,target_id,COALESCE(target_name,''),COALESCE(from_agent,''),COALESCE(to_agent,''),COALESCE(region,''),COALESCE(tenant_id,'default'),COALESCE(reason,''),COALESCE(status,''),COALESCE(note,''),COALESCE(created_at,0) FROM relocations WHERE target_id=? ORDER BY id DESC LIMIT 1`, targetID).
		Scan(&r.ID, &r.TargetID, &r.TargetName, &r.FromAgent, &r.ToAgent, &r.Region, &r.TenantID, &r.Reason, &r.Status, &r.Note, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRelocationsAll 最近迁移记录（从新到旧，空 target_id 聚合展示用）。
func (s *DB) ListRelocationsAll(limit int) ([]*RelocationRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,target_id,COALESCE(target_name,''),COALESCE(from_agent,''),COALESCE(to_agent,''),COALESCE(region,''),COALESCE(tenant_id,'default'),COALESCE(reason,''),COALESCE(status,''),COALESCE(note,''),COALESCE(created_at,0) FROM relocations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RelocationRow{}
	for rows.Next() {
		r := &RelocationRow{}
		if err := rows.Scan(&r.ID, &r.TargetID, &r.TargetName, &r.FromAgent, &r.ToAgent, &r.Region, &r.TenantID, &r.Reason, &r.Status, &r.Note, &r.CreatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// RelocationAge 供日志/证据展示。
func (r *RelocationRow) RelocationAge() string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%ds", time.Now().Unix()-r.CreatedAt)
}
