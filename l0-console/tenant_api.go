package main

// tenant_api.go — 多租户（架构 D3）管控 API（骨架）。
//
// 提供租户目录的管理接口：清单 / 新建。默认租户保护不可删除。只读写 tenants 表，
// 不触碰既有 Agent/资源/容量，无破坏性。API 级 tenant_id 数据隔离留待 D3 后续阶段。

import (
	"encoding/json"
	"net/http"

	storepkg "github.com/sagent/l0-console/store"
)

type tenantReq struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

func registerTenantRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	mux.HandleFunc("/api/tenants", func(w http.ResponseWriter, r *http.Request) {
		if catDB == nil {
			writeJSON(w, map[string]any{"error": "store unavailable"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			list, err := catDB.ListTenants()
			if err != nil {
				writeJSON(w, map[string]any{"error": "list tenants: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"tenants": list})
		case http.MethodPost:
				var req tenantReq
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" || req.Name == "" {
					writeJSON(w, map[string]any{"error": "invalid body: need code & name"})
					return
				}
				// 默认租户保留（唯一性保护），不允许经 API 新建同名
				if req.Code == "default" {
					writeJSON(w, map[string]any{"error": "『default』为系统保留租户"})
					return
				}
				t, err := catDB.CreateTenant(req.Code, req.Name, req.Note)
				if err != nil {
					writeJSON(w, map[string]any{"error": "create tenant: " + err.Error()})
					return
				}
				addAudit("tenant.create", req.Code, "default", "ok")
				writeJSON(w, map[string]any{"ok": true, "tenant": t})
			case http.MethodPatch:
				var req tenantReq
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" || req.Name == "" {
					writeJSON(w, map[string]any{"error": "invalid body: need code & name"})
					return
				}
				// 默认租户名称保留，不允许经 API 改名
				if req.Code == "default" {
					writeJSON(w, map[string]any{"error": "『default』为系统保留租户，不允许改名"})
					return
				}
				if err := catDB.RenameTenant(req.Code, req.Name); err != nil {
					writeJSON(w, map[string]any{"error": "rename tenant: " + err.Error()})
					return
				}
				addAudit("tenant.rename", req.Code, "default", "ok")
				writeJSON(w, map[string]any{"ok": true})
			default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}