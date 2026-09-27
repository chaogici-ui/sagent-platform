package store

// pkgdownload.go —— 版本包取用事件留痕（证据链 P2，2026-09-27）。
//
// 修复"PackageCache 取包零留痕"断点（.workbuddy/evidence/chain-evidence-20260927.md）：
// install 回执只有"成功走完"任务的 tag/sha256，resolve 阶段的事件流（何时取包、
// 来源是缓存命中还是回源重建、被拒绝的 resolve）没有独立留痕，无法机器查询、无法装配成链。
// 本表 = 取包事件的唯一持久化载体，供 /api/package-downloads 查询与 /api/trace 装配。
//
// 写法沿用本项目惯例：SQL 以 ? 占位符书写，执行前由 dialect 层统一 rebind 为 PG 的 $N。

import (
	"strings"
	"time"
)

// PackageDownloadRow package_downloads 表行（一次取包/拒绝 resolve 事件）。
type PackageDownloadRow struct {
	ID         int64
	Ts         int64  // 事件时刻（epoch 秒）
	Caller     string // 取包方：install-flow:<flowID> / runner:<taskID> / resolve-api:<ip>
	ResourceID string // 关联资源（可空：如 package-cache resolve API 裸调用）
	Tag        string // 版本 tag
	SHA256     string // 实际二进制 sha256（hex；被拒绝时为期望值或空）
	Verified   bool   // sha256 与 L0 权威清单一致
	Source     string // cache-hit | resync | remote-resolve | rejected
	Note       string
}

// InitPackageDownloads 建 package_downloads 表（迁移式，IF NOT EXISTS；随 console 启动引导调用）。
func (s *DB) InitPackageDownloads() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS package_downloads (
			id BIGSERIAL PRIMARY KEY,
			ts BIGINT DEFAULT 0,
			caller TEXT DEFAULT '',
			resource_id TEXT DEFAULT '',
			tag TEXT DEFAULT '',
			sha256 TEXT DEFAULT '',
			verified BOOLEAN DEFAULT FALSE,
			source TEXT DEFAULT '',
			note TEXT DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pkg_dl_tag ON package_downloads(tag, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_pkg_dl_resource ON package_downloads(resource_id, ts)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// RecordPackageDownload 落一条取包事件（尽力而为：证据留痕失败不阻断安装主流程）。
func (s *DB) RecordPackageDownload(e PackageDownloadRow) error {
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	_, err := s.db.Exec(
		`INSERT INTO package_downloads (ts, caller, resource_id, tag, sha256, verified, source, note)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Ts, e.Caller, e.ResourceID, e.Tag, e.SHA256, e.Verified, e.Source, e.Note,
	)
	return err
}

// ListPackageDownloads 按时间倒序列出取包事件（tag/resourceID 为空串 = 不过滤该维度）。
func (s *DB) ListPackageDownloads(tag, resourceID string, limit int) ([]*PackageDownloadRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, ts, caller, resource_id, tag, sha256, verified, source, note
	      FROM package_downloads WHERE 1=1`
	var args []any
	if tag != "" {
		q += ` AND tag = ?`
		args = append(args, tag)
	}
	if resourceID != "" {
		q += ` AND resource_id = ?`
		args = append(args, resourceID)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PackageDownloadRow
	for rows.Next() {
		var r PackageDownloadRow
		if err := rows.Scan(&r.ID, &r.Ts, &r.Caller, &r.ResourceID, &r.Tag,
			&r.SHA256, &r.Verified, &r.Source, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ShortSHA 截断 sha256 供回执/展示（前 12 位），空串安全。
func ShortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
