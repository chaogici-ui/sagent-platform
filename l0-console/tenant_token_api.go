package main

// tenant_token_api.go — 租户 API Token（架构 D3 凭证鉴权）。
//
// 隔离缺口：此前 /api/agents?tenant=X 允许任意人手工带参去读任意租户，无身份、无隔离。
// 本层补上"穷证来源可验证"：租户级 API Token 由 L0 管理端签发/吊销（存 SHA-256 哈希，
// 明文仅签发瞬间展示一次）；携带合法 token 的请求由服务端内置租户强制限定作用域，
// 客户端自报的 ?tenant= 被忽略/校验——有凭证的消费方无法跨租户读到别家数据。
//
// 边界（职责划分）：
//  - 带 token 的请求 → 强隔离（只读得到本租户数据，伪 token 一律 401）。
//  - 不带 token 的请求 → 维持运维控制面现状（此 L0 控制面本身受网络可达性保护，
//    属管理员视图，不受本凭证层约束）。本层解决"凭证消费方不越租户"，控制台不倒退。
//  - 本文件只负责签发/吊销/校验语义，不触碰具体业务数据。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// headerTenantToken 凭证入口：请求头携带租户 API Token。
const headerTenantToken = "X-Tenant-Token"

// registerTenantTokenRoutes 注册 /api/tenants/{code}/token 签发/吊销/状态。
func registerTenantTokenRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	mux.HandleFunc("/api/tenants/", func(w http.ResponseWriter, r *http.Request) {
		if catDB == nil {
			writeJSON(w, map[string]any{"error": "store unavailable"})
			return
		}
		// 仅处理 /api/tenants/{code}/token
		rest := strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/api/tenants/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[1] != "token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		code := strings.TrimSpace(parts[0])
		if code == "" {
			writeJSON(w, map[string]any{"error": "tenant code required"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			h, err := catDB.GetTenantTokenHash(code)
			if err != nil {
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "tenant": code, "has_token": h != ""})
		case http.MethodPost:
			// 签发/轮换：明文仅此一次返回，库内只存哈希。先行校验租户存在。
			if _, err := catDB.GetTenantTokenHash(code); err != nil {
				writeJSON(w, map[string]any{"error": "tenant " + code + " 不存在"})
				return
			}
			tok, err := newTenantToken()
			if err != nil {
				writeJSON(w, map[string]any{"error": "token gen: " + err.Error()})
				return
			}
			if err := catDB.SetTenantTokenHash(code, sha256Hex(tok)); err != nil {
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			addAudit("tenant.token.issue", code, "多租户", "issue/rotate")
			writeJSON(w, map[string]any{
				"ok": true, "tenant": code, "api_token": tok,
				"note": "明文仅本次返回，库内只存哈希；请立即保存",
			})
		case http.MethodDelete:
			if _, err := catDB.GetTenantTokenHash(code); err != nil {
				writeJSON(w, map[string]any{"error": "tenant " + code + " 不存在"})
				return
			}
			if err := catDB.SetTenantTokenHash(code, ""); err != nil {
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			addAudit("tenant.token.revoke", code, "多租户", "revoke")
			writeJSON(w, map[string]any{"ok": true, "tenant": code, "has_token": false})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// tenantScopeFromRequest 由请求解析"凭证派生租户"。返回三值：
//   - code：有效 token 对应的租户 code（无 token 或伪 token 时为空）
//   - claim：请求是否带 token（true=带）
//   - valid：token 是否有且通过校验
//
// 调用方：claim==true 时，valid==false → 必须 401（伪 token 不许伪装成任意租户）；
// valid==true → 用 code 作为强制租户作用域，忽略客户端 ?tenant=。
// 无 token（claim==false）→ 走管理端路径。
func tenantScopeFromRequest(r *http.Request, catDB *storepkg.DB) (code string, claim bool, valid bool) {
	tok := strings.TrimSpace(r.Header.Get(headerTenantToken))
	if tok == "" {
		return "", false, true
	}
	c, err := catDB.TenantByTokenHash(sha256Hex(tok))
	if err != nil || c == "" {
		return "", true, false
	}
	return c, true, true
}

// effectiveTenantParam 计算本请求生效的租户作用域：
// 带有效 token → 凭证派生租户（强隔离）；否则回退客户端 ?tenant= 参数（管理端视图）。
func effectiveTenantParam(r *http.Request, catDB *storepkg.DB) (scope string, claim, valid bool) {
	code, claim, valid := tenantScopeFromRequest(r, catDB)
	if claim && valid {
		return code, claim, valid
	}
	return r.URL.Query().Get("tenant"), claim, valid
}

// newTenantToken 生成 32 字节随机 API Token（前缀标识来源）。
func newTenantToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sagent_t_" + strings.TrimRight(strings.ReplaceAll(encodeBase64(b), "=", ""), ""), nil
}

func encodeBase64(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	sb := &strings.Builder{}
	var val uint
	var bits int
	for _, c := range b {
		val = val<<8 | uint(c)
		bits += 8
		for bits >= 6 {
			sb.WriteByte(alphabet[(val>>(bits-6))&63])
			bits -= 6
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(val<<(6-bits))&63])
	}
	return sb.String()
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
