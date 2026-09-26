package main

// tenant_token_test.go — 架构 D3 凭证鉴权单测：
//  1) 租户 Token 的落库/取回/反查/吊销（只存哈希）；
//  2) 带合法租户 token 请求 /api/agents 时，服务端以"令牌内置租户"强制限定作用域，
//     即使客户端 ?tenant= 指向别的租户也只返回本租户数据（凭证来源可信）；
//  3) 伪 token 一律 401，不许伪装成任意租户读取数据。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

func newTokenTestDB(t *testing.T) *storepkg.DB {
	t.Helper()
	return openTestCatalog(t)
}

func TestTenantTokenHashStore(t *testing.T) {
	db := newTokenTestDB(t)
	if _, err := db.CreateTenant("tenant-a", "Alibaba", ""); err != nil {
		t.Fatalf("create tenant-a: %v", err)
	}
	if err := db.SetTenantTokenHash("tenant-a", "hashX"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if h, _ := db.GetTenantTokenHash("tenant-a"); h != "hashX" {
		t.Fatalf("get hash = %q, want hashX", h)
	}
	code, _ := db.TenantByTokenHash("hashX")
	if code != "tenant-a" {
		t.Fatalf("by hash = %q, want tenant-a", code)
	}
	if c, _ := db.TenantByTokenHash("no-such-hash"); c != "" {
		t.Fatalf("pseudo hash => %q, want empty", c)
	}
	// 吊销后不可再反查
	if err := db.SetTenantTokenHash("tenant-a", ""); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if c, _ := db.TenantByTokenHash("hashX"); c != "" {
		t.Fatalf("revoked hash still resolvable: %q", c)
	}
}

// issueTenantToken 经 /api/tenants/{code}/token 签发（同时覆盖签发端点的易用性），返回明文 token。
func issueTenantToken(t *testing.T, mux *http.ServeMux, db *storepkg.DB, code string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tenants/"+code+"/token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("issue %s: status=%d body=%s", code, rec.Code, rec.Body.String())
	}
	var out struct {
		APIToken string `json:"api_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.APIToken == "" {
		t.Fatalf("issue %s: bad body: %v %s", code, err, rec.Body.String())
	}
	// 明文只应出现在本次响应，落库的必须是对应哈希而非明文
	h, _ := db.GetTenantTokenHash(code)
	if h == out.APIToken || h == "" {
		t.Fatalf("tenant %s stored hash must be a non-plaintext hash, got=%q token=%q", code, h, out.APIToken)
	}
	return out.APIToken
}

func TestTenantTokenIsolatesAgentsOnProvenance(t *testing.T) {
	db := newTokenTestDB(t)
	for _, c := range []string{"tenant-a", "tenant-b"} {
		if _, err := db.CreateTenant(c, c, ""); err != nil {
			t.Fatalf("create %s: %v", c, err)
		}
	}
	as := NewAgentStore()
	as.Put(&Agent{ID: "a1", TenantID: "tenant-a", Name: "a1"})
	as.Put(&Agent{ID: "a2", TenantID: "tenant-b", Name: "a2"})
	as.Put(&Agent{ID: "a3", TenantID: "default", Name: "a3"})

	mux := http.NewServeMux()
	registerAgentRoutes(mux, as, db)
	registerTenantTokenRoutes(mux, db)

	tokA := issueTenantToken(t, mux, db, "tenant-a")

	// 关键隔离断言：带 tenant-a 的 token 去问 ?tenant=tenant-b，只应得到 tenant-a 的 Agent。
	req := httptest.NewRequest(http.MethodGet, "/api/agents?tenant=tenant-b", nil)
	req.Header.Set("X-Tenant-Token", tokA)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []*Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("tenant-a token + ?tenant=tenant-b returned %+v, want ONLY [a1]", ids(got))
	}

	// 反向：带 tenant-a token 不带 ?tenant=，也应只有 a1（默认也限定到内置租户）
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req2.Header.Set("X-Tenant-Token", tokA)
	mux.ServeHTTP(rec2, req2)
	var got2 []*Agent
	_ = json.Unmarshal(rec2.Body.Bytes(), &got2)
	if len(got2) != 1 || got2[0].ID != "a1" {
		t.Fatalf("tenant-a token no ?tenant returned %+v, want [a1]", ids(got2))
	}

	// 伪 token 一律 401，即使 ?tenant= 未带
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req3.Header.Set("X-Tenant-Token", "sagent_t_forged_xxx")
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("forged token status=%d body=%s, want 401", rec3.Code, rec3.Body.String())
	}
}

func ids(as []*Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a != nil {
			out = append(out, a.ID)
		}
	}
	return out
}