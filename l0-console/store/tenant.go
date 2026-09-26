package store

// tenant.go — 多租户（架构 D3）数据层：租户维度骨架。
//
// 用途：提供租户目录的持久化读写。本阶段仅建立"租户清单"这一管理骨架——
// 存量 Agent/资源默认归属 default 租户，触达层的 tenant_id 隔离留待 D3 后续阶段
// 全链路接入（避免一次性大改破坏既有单租户行为）。

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tenant 租户
type Tenant struct {
	ID        int64     `json:"id"`
	Code      string    `json:"code"` // 唯一标识，如 default / tenant-a
	Name      string    `json:"name"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// ListTenants 列出全部租户（按 id 升序，默认租户恒在首位）。
func (s *DB) ListTenants() ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT id, code, name, note, created_at FROM tenants ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Code, &t.Name, &t.Note, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTenant 新增租户（code 唯一冲突返回错误）。
func (s *DB) CreateTenant(code, name, note string) (*Tenant, error) {
	id, err := s.insertID(`INSERT INTO tenants (code, name, note) VALUES (?, ?, ?)`, code, name, note)
	if err != nil {
		return nil, err
	}
	var t Tenant
	err = s.db.QueryRow(`SELECT id, code, name, note, created_at FROM tenants WHERE id = ?`, id).
		Scan(&t.ID, &t.Code, &t.Name, &t.Note, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RenameTenant 更新租户显示名（code 保持不变；返回未找到错误）。
func (s *DB) RenameTenant(code, name string) error {
	res, err := s.db.Exec(`UPDATE tenants SET name = ? WHERE code = ?`, name, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("tenant %q not found", code)
	}
	return nil
}

// token 存 SHA-256 哈希，不落明文。SetTenantTokenHash 传空串=吊销。

// SetTenantTokenHash 签发/覆盖某租户 API Token 的哈希（code 不存在即报错，防误写）。
func (s *DB) SetTenantTokenHash(code, hash string) error {
	res, err := s.db.Exec(`UPDATE tenants SET api_token_hash = ? WHERE code = ?`, hash, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("tenant %q not found", code)
	}
	return nil
}

// GetTenantTokenHash 返回某租户当前 Token 哈希（空=未签发/已吊销）。
func (s *DB) GetTenantTokenHash(code string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT COALESCE(api_token_hash,'') FROM tenants WHERE code = ?`, code).Scan(&h)
	return h, err
}

// TenantByTokenHash 按 Token 哈希反查归属租户（凭证来源：令牌的内置租户，非客户端自报）。
// 未命中返回 code=""（伪 token）。
func (s *DB) TenantByTokenHash(hash string) (string, error) {
	var code string
	err := s.db.QueryRow(`SELECT code FROM tenants WHERE api_token_hash = ? AND api_token_hash != ''`, hash).Scan(&code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return code, nil
}
