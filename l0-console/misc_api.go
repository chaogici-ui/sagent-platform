package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// registerMiscRoutes 注册站点配置/审计/任务/集成包/VM 代理等杂项 API
func registerMiscRoutes(mux *http.ServeMux, onboardCfg *OnboardConfig) {
	// API: 站点配置（前端外部跳转地址统一从后端取，禁止在 JS 里写死环境地址）
	// vm_url 用 VM_PUBLIC_URL（浏览器可达），与服务端代理用的 VM_URL（docker service name）是两个概念
	mux.HandleFunc("/api/site-config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{
			"vm_url":      os.Getenv("VM_PUBLIC_URL"), // 为空时前端按 window.location 推导
			"grafana_url": cfgGrafanaPublicURL,
		})
	})

	// API: 接入配置（Agent 类型捆绑 / 接入向导插件清单，产品语义数据化下发）
	mux.HandleFunc("/api/onboard/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, onboardCfg)
	})
	// API: 操作审计（M2-⑪：读落库数据，重启不丢；DB 不可用时回退内存镜像）
	mux.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		if auditDB != nil {
			if rows, err := auditDB.ListAudit(200); err == nil {
				writeJSON(w, rows)
				return
			}
		}
		auditMu.Lock()
		defer auditMu.Unlock()
		writeJSON(w, auditLog)
	})

	// API: 从 VictoriaMetrics 反查已采集指标
	mux.HandleFunc("/api/vm/metrics", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		vmURL := vmBase() + "/api/v1/label/__name__/values"
		resp, err := http.Get(vmURL)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		var result struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		if prefix != "" {
			var filtered []string
			for _, name := range result.Data {
				if strings.HasPrefix(name, prefix) {
					filtered = append(filtered, name)
				}
			}
			result.Data = filtered
		}
		writeJSON(w, result)
	})

	// 以下死端点已删除（前端零引用，历史遗留，详见回归 lint「死端点清零」）：
	// - metrics/synced           旧内存 map 存储，重启即丢，违反红线 1
	// - vm 侧按 target 聚合指标  无消费方
	// - plugins                  旧 JSON 文件存储，插件目录统一走 SQLite catalog 命名空间

	// API: 任务历史（从审计日志聚合）
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		auditMu.Lock()
		defer auditMu.Unlock()
		type TaskItem struct {
			Time     string `json:"time"`
			Operator string `json:"operator"`
			Action   string `json:"action"`
			Target   string `json:"target"`
			Scope    string `json:"scope"`
			Result   string `json:"result"`
		}
		var tasks = make([]TaskItem, 0)
		for _, e := range auditLog {
			if e.Action == "启动 Agent" || e.Action == "停止 Agent" || e.Action == "重启 Agent" || e.Action == "部署 Agent" || e.Action == "升级 Agent" || e.Action == "sync_metrics" {
				tasks = append(tasks, TaskItem(e))
			}
		}
		writeJSON(w, tasks)
	})

	// API: 插件文件上传（图标 + 脚本）
	mux.HandleFunc("/api/plugin/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		pluginID := r.FormValue("plugin_id")
		fileType := r.FormValue("type") // script or icon
		if pluginID == "" || fileType == "" {
			writeJSON(w, map[string]interface{}{"error": "plugin_id and type required"})
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer file.Close()
		// Ensure upload directory exists
		uploadDir := fmt.Sprintf("data/plugins/upload/%s/%s", pluginID, fileType)
		os.MkdirAll(uploadDir, 0755)
		// Save file
		dst, err := os.Create(fmt.Sprintf("%s/%s", uploadDir, hdr.Filename))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer dst.Close()
		io.Copy(dst, file)
		addAudit("上传插件文件", pluginID, fileType, "成功: "+hdr.Filename)
		writeJSON(w, map[string]interface{}{"success": true, "filename": hdr.Filename})
	})

	// Serve uploaded files
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("data/plugins/upload"))))
	// API: 集成列表（来自 Nightingale integrations）
	mux.HandleFunc("/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		integDir := "data/integrations"
		entries, err := os.ReadDir(integDir)
		if err != nil {
			writeJSON(w, []interface{}{})
			return
		}
		type Integration struct {
			Name       string `json:"name"`
			Icon       string `json:"icon"`
			HasCollect bool   `json:"has_collect"`
			HasMetrics bool   `json:"has_metrics"`
			HasDash    bool   `json:"has_dash"`
			HasAlerts  bool   `json:"has_alerts"`
		}
		var list []Integration
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			integ := Integration{Name: name}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/icon", integDir, name)); err == nil {
				if icons, _ := os.ReadDir(fmt.Sprintf("%s/%s/icon", integDir, name)); len(icons) > 0 {
					integ.Icon = fmt.Sprintf("/uploads/integrations/%s/icon/%s", name, icons[0].Name())
				}
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/collect", integDir, name)); err == nil {
				integ.HasCollect = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/metrics", integDir, name)); err == nil {
				integ.HasMetrics = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/dashboards", integDir, name)); err == nil {
				integ.HasDash = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/alerts", integDir, name)); err == nil {
				integ.HasAlerts = true
			}
			list = append(list, integ)
		}
		writeJSON(w, list)
	})

	// API: 集成详情（collect/metrics/dashboards）
	mux.HandleFunc("/api/integrations/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/integrations/")
		integDir := "data/integrations"
		// Case-insensitive directory lookup
		entries, _ := os.ReadDir(integDir)
		realName := ""
		for _, e := range entries {
			if strings.EqualFold(e.Name(), name) {
				realName = e.Name()
				break
			}
		}
		if realName == "" {
			writeJSON(w, map[string]interface{}{"error": "not found"})
			return
		}
		result := map[string]interface{}{
			"name":       name,
			"collect":    []interface{}{},
			"metrics":    []interface{}{},
			"dashboards": []interface{}{},
		}
		// Read collect templates
		collectDir := fmt.Sprintf("%s/%s/collect", integDir, realName)
		if dirs, err := os.ReadDir(collectDir); err == nil {
			var templates []string
			for _, d := range dirs {
				if d.IsDir() {
					if files, err := os.ReadDir(fmt.Sprintf("%s/%s", collectDir, d.Name())); err == nil {
						for _, f := range files {
							if data, err := os.ReadFile(fmt.Sprintf("%s/%s/%s", collectDir, d.Name(), f.Name())); err == nil {
								templates = append(templates, string(data))
							}
						}
					}
				}
			}
			result["collect"] = templates
		}
		// Read metrics
		metricsDir := fmt.Sprintf("%s/%s/metrics", integDir, realName)
		if files, err := os.ReadDir(metricsDir); err == nil {
			for _, f := range files {
				if data, err := os.ReadFile(fmt.Sprintf("%s/%s", metricsDir, f.Name())); err == nil {
					var metrics []interface{}
					json.Unmarshal(data, &metrics)
					result["metrics"] = metrics
				}
			}
		}
		// Read dashboards
		dashDir := fmt.Sprintf("%s/%s/dashboards", integDir, realName)
		if files, err := os.ReadDir(dashDir); err == nil {
			var dashList []map[string]interface{}
			for _, f := range files {
				if data, err := os.ReadFile(fmt.Sprintf("%s/%s", dashDir, f.Name())); err == nil {
					var dash map[string]interface{}
					json.Unmarshal(data, &dash)
					if dash != nil {
						dash["file"] = f.Name()
						dashList = append(dashList, dash)
					}
				}
			}
			result["dashboards"] = dashList
		}
		writeJSON(w, result)
	})

	// API: VM PromQL 查询代理
	mux.HandleFunc("/api/vm/query", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if query == "" {
			writeJSON(w, map[string]interface{}{"error": "query required"})
			return
		}
		vmURL := fmt.Sprintf("%s/api/v1/query?query=%s", vmBase(), url.QueryEscape(query))
		resp, err := http.Get(vmURL)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// Serve integration icons
	mux.Handle("/uploads/integrations/", http.StripPrefix("/uploads/integrations/", http.FileServer(http.Dir("data/integrations"))))
}
