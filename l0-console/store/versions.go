package store

import "database/sql"

// VersionRow SAgent 版本登记行（目录库 PG 为权威；data/versions.yaml 只作种子）。
// 兼容性判据：arch + min_kernel + 包内组件（components_json）要求
type VersionRow struct {
	Tag            string `json:"tag"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SHA256         string `json:"sha256"`
	Status         string `json:"status"` // stable / deprecated / disabled
	Released       string `json:"released"`
	Notes          string `json:"notes"`
	MinKernel      string `json:"min_kernel"`
	ComponentsJSON string `json:"components_json"`
	Source         string `json:"source"` // seed / ui
	UpdatedAt      string `json:"updated_at"`
	UpdatedBy      string `json:"updated_by"`
}

// initVersions 建版本登记表（幂等；老库直接补表，无数据迁移）
func (s *DB) initVersions() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sa_versions (
		tag             TEXT PRIMARY KEY,
		version         TEXT NOT NULL DEFAULT '',
		os              TEXT NOT NULL DEFAULT '',
		arch            TEXT NOT NULL DEFAULT '',
		sha256          TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL DEFAULT 'stable',
		released        TEXT NOT NULL DEFAULT '',
		notes           TEXT NOT NULL DEFAULT '',
		min_kernel      TEXT NOT NULL DEFAULT '',
		components_json TEXT NOT NULL DEFAULT '[]',
		source          TEXT NOT NULL DEFAULT 'seed',
		updated_at      TEXT NOT NULL DEFAULT '',
		updated_by      TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// ListVersions 全量版本行（版本号降序，便于直接给界面用）
func (s *DB) ListVersions() ([]VersionRow, error) {
	rows, err := s.db.Query(`SELECT tag, version, os, arch, sha256, status, released, notes,
		min_kernel, COALESCE(components_json,'[]'), COALESCE(source,'seed'),
		COALESCE(updated_at,''), COALESCE(updated_by,'')
		FROM sa_versions ORDER BY version DESC, tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionRow{}
	for rows.Next() {
		var r VersionRow
		if err := rows.Scan(&r.Tag, &r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Status, &r.Released,
			&r.Notes, &r.MinKernel, &r.ComponentsJSON, &r.Source, &r.UpdatedAt, &r.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertVersion 写入/更新版本行；source 标识来源（ui = 人工编辑，seed = 清单导入）
func (s *DB) UpsertVersion(r VersionRow, source string) error {
	if r.ComponentsJSON == "" {
		r.ComponentsJSON = "[]"
	}
	if r.Status == "" {
		r.Status = "stable"
	}
	_, err := s.db.Exec(`INSERT INTO sa_versions
		(tag, version, os, arch, sha256, status, released, notes, min_kernel, components_json,
		 source, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?, to_char(now(), 'YYYY-MM-DD HH24:MI:SS'), ?)
		ON CONFLICT(tag) DO UPDATE SET
			version=excluded.version, os=excluded.os, arch=excluded.arch, sha256=excluded.sha256,
			status=excluded.status, released=excluded.released, notes=excluded.notes,
			min_kernel=excluded.min_kernel, components_json=excluded.components_json,
			source=excluded.source, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS'), updated_by=excluded.updated_by`,
		r.Tag, r.Version, r.OS, r.Arch, r.SHA256, r.Status, r.Released, r.Notes,
		r.MinKernel, r.ComponentsJSON, source, r.UpdatedBy)
	return err
}

// UpsertVersionSeed 镜像种子导入：不存在则插入；已存在且 source=seed 才更新——
// 界面改过（source=ui）的行不被镜像覆盖，否则每次重建镜像都会吞掉人工维护的兼容矩阵
func (s *DB) UpsertVersionSeed(r VersionRow) error {
	var src string
	err := s.db.QueryRow(`SELECT COALESCE(source,'seed') FROM sa_versions WHERE tag=?`, r.Tag).Scan(&src)
	if err == sql.ErrNoRows {
		return s.UpsertVersion(r, "seed")
	}
	if err != nil {
		return err
	}
	if src == "ui" {
		return nil
	}
	return s.UpsertVersion(r, "seed")
}
