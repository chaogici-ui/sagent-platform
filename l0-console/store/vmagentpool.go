package store

import (
	"database/sql"
	"time"
)

// ============================================================
// vmagent 池（单归属 + 故障接管 + 自动回切）
// 与 relocations（跨 L1 采集机漂移）平行的第二套归属账本：
// 那套管「L1 采集机承载远端 exporter」，这套管「L0 vmagent 抓取目标归属」。
// 生产约束：全部逻辑只依赖 PG + VM HTTP API + sd 文件，零 docker 依赖。
// ============================================================

// VmagentTargetRow 一条抓取目标的归属记录。
// CurrentOwner / PreferredOwner 取值 = vmagent 池成员标识（与该成员 remoteWrite
// 的 replica 标签同值，如 pool-a / pool-b）——数据面判活直接按 replica 标签查 VM。
type VmagentTargetRow struct {
	ID             int64
	TargetKey      string // 唯一标识 = 抓取地址（如 mysql-exp-1:9104）
	DisplayName    string // 展示名（如 mysql-1）
	JobName        string // 所属 scrape job（决定 sd 子目录与抓取参数）
	PreferredOwner string // 归属偏好：回切的落点（谁“应该”采它）
	CurrentOwner   string // 当前归属：实际在采的池成员
	OwnerSince     int64  // 当前归属生效时间（unix 秒，冷却判定依据）
	Enabled        bool   // false = 暂停采集（不写任何成员的 sd 文件）
	CreatedAt      int64
	UpdatedAt      int64
}

// VmagentPoolEvent 归属变更事件（接管/回切/手工改派/注册），界面时间线数据源。
type VmagentPoolEvent struct {
	ID        int64
	TargetKey string
	FromOwner string
	ToOwner   string
	Kind      string // takeover | failback | assign | register | remove | drill_down | drill_recover
	Reason    string
	Status    string // moved | skipped | rejected
	Note      string
	CreatedAt int64
}

const createVmagentTargets = `CREATE TABLE IF NOT EXISTS vmagent_targets (
  id SERIAL PRIMARY KEY,
  target_key TEXT UNIQUE NOT NULL,
  display_name TEXT DEFAULT '',
  job_name TEXT DEFAULT '',
  preferred_owner TEXT DEFAULT '',
  current_owner TEXT DEFAULT '',
  owner_since INTEGER DEFAULT 0,
  enabled BOOLEAN DEFAULT TRUE,
  created_at INTEGER DEFAULT 0,
  updated_at INTEGER DEFAULT 0
)`

// vmagent_pool_member_flags 成员演练标记（界面「模拟宕机」按钮的持久化落点）。
// 语义 = 计划内停机演练：标记生效后调度器把该成员按死亡处理（数据面照常在跑），
// 目标漂给健康成员；解除后数据面确认健康 → 走正常回切流程。PG 存储保证零 docker 依赖，
// 生产同构可用（重启后标记仍在，防"演练标记悄悄失效"）。
const createVmagentPoolMemberFlags = `CREATE TABLE IF NOT EXISTS vmagent_pool_member_flags (
  member TEXT PRIMARY KEY,
  sim_down BOOLEAN DEFAULT FALSE,
  updated_at INTEGER DEFAULT 0
)`

const createVmagentPoolEvents = `CREATE TABLE IF NOT EXISTS vmagent_pool_events (
  id SERIAL PRIMARY KEY,
  target_key TEXT DEFAULT '',
  from_owner TEXT DEFAULT '',
  to_owner TEXT DEFAULT '',
  kind TEXT DEFAULT '',
  reason TEXT DEFAULT '',
  status TEXT DEFAULT 'moved',
  note TEXT DEFAULT '',
  created_at INTEGER DEFAULT 0
)`

// InitVmagentPool 建池归属表/事件表/成员演练标记表（幂等：CREATE TABLE IF NOT EXISTS）。
func (s *DB) InitVmagentPool() error {
	for _, ddl := range []string{createVmagentTargets, createVmagentPoolEvents, createVmagentPoolMemberFlags} {
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// SetVmagentMemberSimDown 设置/解除成员演练宕机标记（幂等）。
func (s *DB) SetVmagentMemberSimDown(member string, down bool) error {
	_, err := s.db.Exec(`INSERT INTO vmagent_pool_member_flags(member,sim_down,updated_at) VALUES(?,?,?)
		ON CONFLICT(member) DO UPDATE SET sim_down=EXCLUDED.sim_down, updated_at=EXCLUDED.updated_at`,
		member, down, time.Now().Unix())
	return err
}

// ListVmagentMemberSimDown 全部处于演练宕机态的成员集合（无标记返回空 map，非 nil）。
// 只 SELECT member 单列：列数必须与 Scan 目标数一致，多列会静默漏行（2026-09-27 实测踩坑）。
func (s *DB) ListVmagentMemberSimDown() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT member FROM vmagent_pool_member_flags WHERE COALESCE(sim_down,FALSE)`)
	if err != nil {
		return map[string]bool{}, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return out, err
		}
		out[m] = true
	}
	return out, rows.Err()
}

const vmagentTargetCols = `id,target_key,COALESCE(display_name,''),COALESCE(job_name,''),COALESCE(preferred_owner,''),COALESCE(current_owner,''),COALESCE(owner_since,0),COALESCE(enabled,TRUE),COALESCE(created_at,0),COALESCE(updated_at,0)`

func scanVmagentTarget(rows *sql.Rows) (*VmagentTargetRow, error) {
	t := &VmagentTargetRow{}
	if err := rows.Scan(&t.ID, &t.TargetKey, &t.DisplayName, &t.JobName, &t.PreferredOwner,
		&t.CurrentOwner, &t.OwnerSince, &t.Enabled, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return t, nil
}

// UpsertVmagentTarget 注册/更新抓取目标（target_key 唯一）。首次注册时
// current_owner=preferred_owner=preferredOwner（空则首次调度时再分派）。
func (s *DB) UpsertVmagentTarget(t *VmagentTargetRow) (int64, error) {
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	t.UpdatedAt = time.Now().Unix()
	if t.JobName == "" {
		t.JobName = "default"
	}
	return s.insertID(`INSERT INTO vmagent_targets(target_key,display_name,job_name,preferred_owner,current_owner,owner_since,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(target_key) DO UPDATE SET
		  display_name=EXCLUDED.display_name, job_name=EXCLUDED.job_name,
		  preferred_owner=EXCLUDED.preferred_owner, enabled=EXCLUDED.enabled, updated_at=EXCLUDED.updated_at`,
		t.TargetKey, t.DisplayName, t.JobName, t.PreferredOwner, t.PreferredOwner,
		time.Now().Unix(), t.Enabled, t.CreatedAt, t.UpdatedAt)
}

// ListVmagentTargets 全量目标（按 id 稳定序）。
func (s *DB) ListVmagentTargets() ([]*VmagentTargetRow, error) {
	rows, err := s.db.Query(`SELECT ` + vmagentTargetCols + ` FROM vmagent_targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*VmagentTargetRow{}
	for rows.Next() {
		t, err := scanVmagentTarget(rows)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetVmagentTarget 按唯一键取单条（无则 nil）。
func (s *DB) GetVmagentTarget(targetKey string) (*VmagentTargetRow, error) {
	rows, err := s.db.Query(`SELECT `+vmagentTargetCols+` FROM vmagent_targets WHERE target_key=?`, targetKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanVmagentTarget(rows)
}

// UpdateVmagentTargetOwner 归属变更落库（接管/回切/手工改派共用），刷新 owner_since。
func (s *DB) UpdateVmagentTargetOwner(targetKey, owner string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE vmagent_targets SET current_owner=?, owner_since=?, updated_at=? WHERE target_key=?`,
		owner, now, now, targetKey)
	return err
}

// RecordVmagentPoolEvent 落一条归属变更事件，返回新 id。
func (s *DB) RecordVmagentPoolEvent(e *VmagentPoolEvent) (int64, error) {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	if e.Status == "" {
		e.Status = "moved"
	}
	return s.insertID(`INSERT INTO vmagent_pool_events(target_key,from_owner,to_owner,kind,reason,status,note,created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		e.TargetKey, e.FromOwner, e.ToOwner, e.Kind, e.Reason, e.Status, e.Note, e.CreatedAt)
}

// LatestVmagentPoolEventByTarget 某目标最近一次事件（回切冷却判定来源；无则 nil）。
func (s *DB) LatestVmagentPoolEventByTarget(targetKey string) (*VmagentPoolEvent, error) {
	var e VmagentPoolEvent
	err := s.db.QueryRow(`SELECT id,target_key,COALESCE(from_owner,''),COALESCE(to_owner,''),COALESCE(kind,''),
		COALESCE(reason,''),COALESCE(status,''),COALESCE(note,''),COALESCE(created_at,0)
		FROM vmagent_pool_events WHERE target_key=? ORDER BY id DESC LIMIT 1`, targetKey).
		Scan(&e.ID, &e.TargetKey, &e.FromOwner, &e.ToOwner, &e.Kind, &e.Reason, &e.Status, &e.Note, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListVmagentPoolEvents 最近事件（从新到旧，聚合展示）。
func (s *DB) ListVmagentPoolEvents(limit int) ([]*VmagentPoolEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,target_key,COALESCE(from_owner,''),COALESCE(to_owner,''),COALESCE(kind,''),
		COALESCE(reason,''),COALESCE(status,''),COALESCE(note,''),COALESCE(created_at,0)
		FROM vmagent_pool_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*VmagentPoolEvent{}
	for rows.Next() {
		e := &VmagentPoolEvent{}
		if err := rows.Scan(&e.ID, &e.TargetKey, &e.FromOwner, &e.ToOwner, &e.Kind,
			&e.Reason, &e.Status, &e.Note, &e.CreatedAt); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteVmagentTarget 摘除目标（连同 sd 文件由调度器同步清理）。
func (s *DB) DeleteVmagentTarget(targetKey string) error {
	_, err := s.db.Exec(`DELETE FROM vmagent_targets WHERE target_key=?`, targetKey)
	return err
}

// BackdateVmagentTargetOwnerSince 测试辅助：回拨归属生效时间（模拟时间流逝过冷却窗）。
func (s *DB) BackdateVmagentTargetOwnerSince(targetKey string, ts int64) error {
	_, err := s.db.Exec(`UPDATE vmagent_targets SET owner_since=? WHERE target_key=?`, ts, targetKey)
	return err
}

// BackdateVmagentPoolEvents 测试辅助：回拨某目标事件时间（模拟冷却窗已过）。
func (s *DB) BackdateVmagentPoolEvents(targetKey string, ts int64) error {
	_, err := s.db.Exec(`UPDATE vmagent_pool_events SET created_at=? WHERE target_key=?`, ts, targetKey)
	return err
}
