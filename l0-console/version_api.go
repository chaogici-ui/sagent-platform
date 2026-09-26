package main

import (
	"encoding/json"
	"net/http"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// registerVersionRoutes 版本与兼容矩阵 API。
// 前端零硬编码：字段、状态枚举、匹配结论全部由这里下发
func registerVersionRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	// 版本包上传/下载（上传落 UI 目录、登记 draft；下载受控读文件流）
	mux.HandleFunc("/api/versions/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		handleVersionUpload(w, r, catDB)
	})
	mux.HandleFunc("/api/versions/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		handleVersionDownload(w, r, catDB)
	})

	mux.HandleFunc("/api/versions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rows, err := catDB.ListVersions()
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				comps := []VersionComponent{}
				if strings.TrimSpace(row.ComponentsJSON) != "" {
					_ = json.Unmarshal([]byte(row.ComponentsJSON), &comps)
				}
				out = append(out, map[string]any{
					"tag": row.Tag, "version": row.Version, "os": row.OS, "arch": row.Arch,
					"sha256": row.SHA256, "status": row.Status, "released": row.Released,
					"notes": row.Notes, "min_kernel": row.MinKernel, "components": comps,
					"source": row.Source, "updated_at": row.UpdatedAt, "updated_by": row.UpdatedBy,
				})
			}
			writeJSON(w, map[string]interface{}{
				"versions":       out,
				"status_options": []string{"draft", "stable", "deprecated", "disabled"},
			})
		case http.MethodPut:
			// 局部更新语义：只改请求里出现的字段——前端「发布/弃用」只发 status，
			// 不能把兼容矩阵与说明一起清掉（指针区分"未提供"与"显式置空"）
			var req struct {
				Tag        string              `json:"tag"`
				MinKernel  *string             `json:"min_kernel"`
				Status     *string             `json:"status"`
				Notes      *string             `json:"notes"`
				Components *[]VersionComponent `json:"components"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Tag) == "" {
				writeJSON(w, map[string]interface{}{"error": "tag 必填"})
				return
			}
			rows, _ := catDB.ListVersions()
			var cur *storepkg.VersionRow
			for i := range rows {
				if rows[i].Tag == req.Tag {
					cur = &rows[i]
				}
			}
			if cur == nil {
				writeJSON(w, map[string]interface{}{"error": "版本清单中无 " + req.Tag + "（新增版本请先放进 data/binaries 并登记种子）"})
				return
			}
			changes := []string{}
			if req.MinKernel != nil {
				cur.MinKernel = strings.TrimSpace(*req.MinKernel)
				changes = append(changes, "内核≥"+cur.MinKernel)
			}
			if req.Status != nil {
				if s := strings.TrimSpace(*req.Status); s != "" {
					cur.Status = s
				}
				changes = append(changes, "状态="+cur.Status)
			}
			if req.Notes != nil {
				cur.Notes = *req.Notes
				changes = append(changes, "说明")
			}
			if req.Components != nil {
				cj, _ := json.Marshal(*req.Components)
				cur.ComponentsJSON = string(cj)
				changes = append(changes, "组件要求")
			}
			cur.UpdatedBy = cfgAuditOperator
			if err := catDB.UpsertVersion(*cur, "ui"); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			// 刷新内存缓存：编辑立即生效，不必重启控制台
			refreshVersionCache(catDB)
			addAudit("编辑版本兼容矩阵", req.Tag, "版本管理",
				"内核≥"+cur.MinKernel+"，状态="+cur.Status)
			writeJSON(w, map[string]interface{}{"ok": true, "tag": req.Tag})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// 匹配预览：给一台资源（用其实测 facts）跑一次自动选版，回答"会选哪个、其它为什么不选"
	mux.HandleFunc("/api/versions/match", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ResourceID string `json:"resource_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		res, _ := catDB.GetResource(strings.TrimSpace(req.ResourceID))
		if res == nil {
			writeJSON(w, map[string]interface{}{"error": "资源不存在：" + req.ResourceID})
			return
		}
		env := targetEnvOf(res)
		cands := compatibleVersions(strings.ToLower(res.OS), strings.ToLower(res.Arch))
		chosen, reason, ds := selectVersion(cands, env)
		considered := make([]map[string]any, 0, len(cands))
		for _, v := range cands {
			ok, why, un := versionCompatible(v, env)
			// 未发布/已停用的条目要如实标成"不可选"并把原因写进 why：
			// 否则预览表会出现"兼容=True 却没被选中"的自我矛盾（draft 不参与自动选版）
			switch strings.ToLower(strings.TrimSpace(v.Status)) {
			case "draft":
				ok, why = false, "未发布（draft）—— 在版本页点「发布」后才参与自动选版"
			case "disabled":
				ok, why = false, "已停用（disabled）"
			}
			considered = append(considered, map[string]any{
				"tag": v.Tag, "version": v.Version, "status": v.Status,
				"compatible": ok, "why": why, "unverified": un,
			})
		}
		out := map[string]any{
			"resource_id": res.ID, "env": env, "candidates": considered,
			"compatible": chosen != nil, "diagnosis": diagJSON(ds),
		}
		if chosen != nil {
			out["chosen"] = map[string]any{"tag": chosen.Tag, "version": chosen.Version, "status": chosen.Status}
			out["reason"] = reason
		}
		writeJSON(w, out)
	})
}
