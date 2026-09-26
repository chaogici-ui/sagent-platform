package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// registerAgentRoutes 注册 Agent 生命周期与采集目标相关 API
func registerAgentRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	// API: Agent 列表
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		// 刷新状态
		refreshAgentStatuses(store)
		all := store.List()
		// 多租户(D3) 数据隔离：先做凭证校验（伪 tenant token 一律 401，不许伪装租户），
		// 再取生效作用域——带合法 token 时用其内置租户强制限定（忽略客户端 ?tenant=），
		// 无 token 走管理端视图，?tenant= 仅作界面过滤（与前端 tenantVisible 口径一致）。
		scope, claim, valid := effectiveTenantParam(r, catDB)
		if claim && !valid {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": "unauthorized: invalid tenant token"})
			return
		}
		if scope != "" {
			filtered := make([]*Agent, 0, len(all))
			for _, a := range all {
				tid := a.TenantID
				if tid == "" {
					tid = "default"
				}
				if tid == scope {
					filtered = append(filtered, a)
				}
			}
			writeJSON(w, filtered)
			return
		}
		writeJSON(w, all)
	})

	// API: 单个 Agent 详情 + 配置管理
	mux.HandleFunc("/api/agents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/agents/")

		// /api/agents/{id}/config  GET 读（结构化 parsed）/ PUT 改（host_metrics 配置轨道）
		if strings.HasSuffix(path, "/config") {
			id := strings.TrimSuffix(path, "/config")
			handleAgentConfigRoute(w, r, id, store, catDB)
			return
		}

		// /api/agents/{id}/plugin-status
		if strings.HasSuffix(path, "/plugin-status") {
			id := strings.TrimSuffix(path, "/plugin-status")
			handlePluginStatus(w, r, id)
			return
		}

		// /api/agents/{id}/metrics
		if strings.HasSuffix(path, "/metrics") {
			id := strings.TrimSuffix(path, "/metrics")
			handleAgentMetrics(w, r, id)
			return
		}

		// /api/agents/{id}/logs
		if strings.HasSuffix(path, "/logs") {
			id := strings.TrimSuffix(path, "/logs")
			handleAgentLogs(w, r, id)
			return
		}

		// /api/agents/{id}
		if r.Method == http.MethodDelete {
			// 注销 Agent：清内存台账 + DB 行（重装/换机重新接入前清身份）
			store.Remove(path)
			deleted, err := catDB.DeleteAgent(path)
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			addAudit("注销 Agent", path, "Agent 台账", map[bool]string{true: "已删除", false: "记录不存在"}[deleted])
			writeJSON(w, map[string]interface{}{"ok": true, "deleted": deleted})
			return
		}
		if agent := store.Get(path); agent != nil {
			writeJSON(w, agent)
		} else {
			http.NotFound(w, r)
		}
	})

	// M0-①：Agent 注册（安装引导后回调）
	mux.HandleFunc("/api/agent/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		// 可选静态 token 校验（SA_REG_TOKEN 设置后强制）
		if tok := os.Getenv("SA_REG_TOKEN"); tok != "" && r.Header.Get("X-Reg-Token") != tok {
			writeJSON(w, map[string]interface{}{"error": "invalid registration token"})
			return
		}
		var req struct {
			ID      string            `json:"id"`
			Type    string            `json:"type"`
			Version string            `json:"version"`
			IP      string            `json:"ip"`
			Labels  map[string]string `json:"labels"`
			Plugins []string          `json:"plugins"`
			CfgHash string            `json:"config_hash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if req.Type == "" {
			req.Type = "edge"
		}
		if req.ID == "" {
			req.ID = fmt.Sprintf("%s-%d", req.Type, time.Now().Unix()%100000)
		}
		if src, _ := catDB.GetAgentSource(req.ID); src == "docker" {
			writeJSON(w, map[string]interface{}{"error": "该 ID 为本机演示容器保留，请换一个 id"})
			return
		}
		row := &storepkg.AgentRow{
			ID: req.ID, Name: req.ID, Type: req.Type, Host: req.IP,
			Plugins: req.Plugins, Version: req.Version, Labels: req.Labels,
			Source: "heartbeat", LastSeen: time.Now().Unix(), CfgEffective: req.CfgHash,
		}
		if err := catDB.UpsertAgentRow(row); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		ver, content, _ := catDB.GetAgentConfig(req.ID)
		store.Put(rowToAgent(row))
		if a := store.Get(req.ID); a != nil {
			a.CfgDesired = ver
		}
		addAudit("注册 Agent", req.ID, "心跳接入", "成功")
		writeJSON(w, map[string]interface{}{"ok": true, "agent_id": req.ID, "config_version": ver, "config": content})
	})

	// M0-①：Agent 心跳（30s 一次），携带版本/生效配置/采集成败；响应携带待下发配置
	mux.HandleFunc("/api/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ID         string                 `json:"id"`
			Status     string                 `json:"status"`
			Version    string                 `json:"version"`
			CfgVersion int                    `json:"config_version"`
			CfgHash    string                 `json:"config_hash"`
			Stats      map[string]interface{} `json:"stats"`
			// —— 接入中心回报字段（可选，向后兼容：旧 Agent 不发这些也能正常心跳）——
			AppliedVersion int   `json:"applied_version"` // 已生效的配置版本（修实测的「期望 cfg-N / 生效 None」）
			ApplyMs        int64 `json:"apply_ms"`        // 配置应用耗时
			Targets        []struct {
				Target  string                 `json:"target"`
				Probe   map[string]interface{} `json:"probe"`
				Collect map[string]interface{} `json:"collect"`
				Samples map[string]interface{} `json:"samples"`
			} `json:"targets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		src, _ := catDB.GetAgentSource(req.ID)
		if src == "" {
			writeJSON(w, map[string]interface{}{"error": "unregistered agent", "need_register": true})
			return
		}
		// 生效配置版本：优先用显式上报的 applied_version，回退 hash（兼容旧 Agent）
		effective := req.CfgHash
		if req.AppliedVersion > 0 {
			effective = fmt.Sprintf("cfg-%d", req.AppliedVersion)
		}
		if err := catDB.AgentHeartbeat(req.ID, req.Version, effective, req.Stats); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if a := store.Get(req.ID); a != nil {
			a.LastSeen = time.Now().Unix()
			a.CfgEffective = effective
			a.Stats = req.Stats
			if req.Version != "" {
				a.Version = req.Version
			}
			a.Status = "healthy"
		}
		ver, content, _ := catDB.GetAgentConfig(req.ID)
		// tasks：G2 通道退役后，「下发式执行器」统一走通道 A（心跳拉取任务）。
		// 事务内把该 SAgent 名下待执行的 agent-action 任务（pending→dispatched）一次性取回，
		// 注入心跳响应 tasks 字段；SAgent 拉到后本机执行并回执 /api/l1/task/ack（幂等防重）。
		rows, _ := catDB.PullAgentActions(req.ID)
		tasks := make([]interface{}, 0, len(rows))
		for _, rw := range rows {
			tasks = append(tasks, rowToL1Task(rw))
		}
		resp := map[string]interface{}{"ok": true, "config_version": ver, "interval": 30, "tasks": tasks}
		if ver > 0 && req.CfgVersion != ver {
			resp["config"] = content
			resp["config_changed"] = true
		}
		writeJSON(w, resp)
	})

	// M0-②：采集目标 CRUD
	mux.HandleFunc("/api/targets", func(w http.ResponseWriter, r *http.Request) {
		type TargetReq struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Address string `json:"address"`
			AgentID string `json:"agent_id"`
			Plugin  string `json:"plugin"`
			Note    string `json:"note"`
			Region  string `json:"region"`
		}
		switch r.Method {
		case http.MethodGet:
			list, err := catDB.ListTargets()
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			// 多租户(D3) 数据隔离：伪 token 401；带合法 token 只返回其内置租户的目标，
			// 防止跨租户读到别家采集目标
			scope, claim, valid := effectiveTenantParam(r, catDB)
			if claim && !valid {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]interface{}{"error": "unauthorized: invalid tenant token"})
				return
			}
			if scope != "" {
				scoped := make([]*storepkg.TargetRow, 0, len(list))
				for _, t := range list {
					tid := t.TenantID
					if tid == "" {
						tid = "default"
					}
					if tid == scope {
						scoped = append(scoped, t)
					}
				}
				writeJSON(w, scoped)
				return
			}
			writeJSON(w, list)
		case http.MethodPost:
			var q TargetReq
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Name == "" || q.Address == "" {
				writeJSON(w, map[string]interface{}{"error": "name 和 address 必填"})
				return
			}
			// 目标分派只允许绑定真实 Agent（未分派允许 agent_id 为空）
			if q.AgentID != "" && !agentExists(store, catDB, q.AgentID) {
				writeJSON(w, map[string]interface{}{"error": "agent not found: " + q.AgentID})
				return
			}
			scope, claim, valid := effectiveTenantParam(r, catDB)
			if claim && !valid {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"error": "unauthorized: invalid tenant token"})
				return
			}
			rowTenant := "default"
			if scope != "" {
				rowTenant = scope
			}
			id, err := catDB.InsertTarget(&storepkg.TargetRow{Name: q.Name, Type: q.Type, Address: q.Address, AgentID: q.AgentID, Plugin: q.Plugin, Note: q.Note, TenantID: rowTenant, Region: q.Region})
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, q.AgentID)
			addAudit("新增采集目标", q.Name, q.AgentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true, "id": id})
		case http.MethodPut:
			var q TargetReq
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.ID == 0 {
				writeJSON(w, map[string]interface{}{"error": "id 必填"})
				return
			}
			// 目标分派只允许绑定真实 Agent（未分派允许 agent_id 为空）
			if q.AgentID != "" && !agentExists(store, catDB, q.AgentID) {
				writeJSON(w, map[string]interface{}{"error": "agent not found: " + q.AgentID})
				return
			}
			// 找出旧分派，双端同步配置版本；保留目标既有租户不变（归属租户由 owner 侧 D3 管）
			oldAgent := ""
			itemTenant := "default"
			if list, _ := catDB.ListTargets(); list != nil {
				for _, t := range list {
					if t.ID == q.ID {
						oldAgent = t.AgentID
						if t.TenantID != "" {
							itemTenant = t.TenantID
						}
					}
				}
			}
			if err := catDB.UpdateTarget(&storepkg.TargetRow{ID: q.ID, Name: q.Name, Type: q.Type, Address: q.Address, AgentID: q.AgentID, Plugin: q.Plugin, Note: q.Note, TenantID: itemTenant, Region: q.Region}); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, q.AgentID)
			if oldAgent != q.AgentID {
				syncAgentConfig(store, catDB, oldAgent)
			}
			addAudit("编辑采集目标", q.Name, q.AgentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true})
		case http.MethodDelete:
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if id == 0 {
				writeJSON(w, map[string]interface{}{"error": "id 必填"})
				return
			}
			agentID := ""
			if list, _ := catDB.ListTargets(); list != nil {
				for _, t := range list {
					if t.ID == id {
						agentID = t.AgentID
					}
				}
			}
			if err := catDB.DeleteTarget(id); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, agentID)
			addAudit("删除采集目标", fmt.Sprintf("id=%d", id), agentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// M0-②：采集目标连通性测试（真实 TCP dial）
	mux.HandleFunc("/api/targets/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/targets/")
		if !strings.HasSuffix(path, "/test") || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		idStr := strings.TrimSuffix(path, "/test")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "bad id"})
			return
		}
		list, _ := catDB.ListTargets()
		var tgt *storepkg.TargetRow
		for _, t := range list {
			if t.ID == id {
				tgt = t
			}
		}
		if tgt == nil {
			writeJSON(w, map[string]interface{}{"error": "target not found"})
			return
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", tgt.Address, 3*time.Second)
		result := ""
		if err != nil {
			result = "失败: " + err.Error()
		} else {
			conn.Close()
			result = fmt.Sprintf("连通 (%d ms)", time.Since(start).Milliseconds())
		}
		_ = catDB.UpdateTargetTest(id, result)
		writeJSON(w, map[string]interface{}{"ok": err == nil, "result": result})
	})

	// API: 执行操作
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
			Action  string `json:"action"` // start, stop, restart, upgrade, deploy
			Version string `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}

		// G2 通道退役：心跳型真实 SAgent 的动作统一走通道 A（投递 agent-action 任务，
		// 由 SAgent 心跳拉取、本机执行并回执）；docker 源演示 Agent 保留通道 B（executeAction）。
		if src, _ := catDB.GetAgentSource(req.AgentID); src == "heartbeat" {
			tid := fmt.Sprintf("agent-action-%s-%d", req.AgentID, time.Now().UnixNano())
			row, _, err := catDB.QueueL1Task(storepkg.L1TaskRow{
				ID:   tid,
				ControllerID: req.AgentID,
				Kind: "agent-action",
				PayloadJSON: storepkg.MarshalPayload(map[string]any{
					"action": req.Action, "version": req.Version,
				}),
				Status: "pending",
			})
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			addAudit("投递 Agent 动作", req.AgentID, "通道A(心跳拉取)", req.Action+" → "+row.ID)
			writeJSON(w, map[string]any{"ok": true, "queued": true, "channel": "a", "task_id": row.ID, "action": req.Action, "status": row.Status})
			return
		}

		result := executeAction(req.AgentID, req.Action, req.Version)
		if req.Action == "start" || req.Action == "restart" {
			store.Update(req.AgentID, "running", req.Version)
		} else if req.Action == "stop" {
			store.Update(req.AgentID, "stopped", "")
		} else if req.Action == "upgrade" {
			store.Update(req.AgentID, "running", req.Version)
		}
		writeJSON(w, result)
	})

	// API: 插件级操作
	mux.HandleFunc("/api/plugin-action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
			Plugin  string `json:"plugin"`
			Action  string `json:"action"` // reload, stop, start, restart
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		result := executePluginAction(req.AgentID, req.Plugin, req.Action)
		writeJSON(w, result)
	})

}

// normalizeTargets 保证 targets 段为数组（legacy 数组内容 / 解析失败时兜底空数组）
func normalizeTargets(v interface{}) interface{} {
	if arr, ok := v.([]interface{}); ok && arr != nil {
		return arr
	}
	return []interface{}{}
}

// defaultHostMetricsCfg 默认 host_metrics 配置段（MVP：全组默认开启、30s、无例外清单）
func defaultHostMetricsCfg() map[string]interface{} {
	return map[string]interface{}{
		"enabled":         true,
		"interval":        "30s",
		"groups":          map[string]interface{}{},
		"exclude_metrics": []string{},
	}
}

// parseAgentConfigDoc 解析期望配置 content 为结构化 doc，兼容旧格式（纯 targets 数组）；
// host_metrics 段缺失时补默认值（未声明组默认开启，与 Agent 注册表约定一致）
func parseAgentConfigDoc(content string) map[string]interface{} {
	doc := map[string]interface{}{}
	var legacy []map[string]interface{}
	if err := json.Unmarshal([]byte(content), &legacy); err == nil && legacy != nil {
		doc["targets"] = legacy
		doc["host_metrics"] = defaultHostMetricsCfg()
		return doc
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(content), &m); err == nil && m != nil {
		doc["targets"] = m["targets"]
		if hm, ok := m["host_metrics"].(map[string]interface{}); ok {
			doc["host_metrics"] = hm
		} else {
			doc["host_metrics"] = defaultHostMetricsCfg()
		}
		return doc
	}
	doc["targets"] = []interface{}{}
	doc["host_metrics"] = defaultHostMetricsCfg()
	return doc
}

// handleAgentConfigRoute GET /api/agents/{id}/config 读取（结构化 parsed 视图）；
// PUT 结构化更新 host_metrics 配置段（配置轨道：config_version+1，心跳热生效，不涉及 Agent 代码版本）
// agentExists 校验 Agent 真实存在（内存注册表或持久层任一命中）。
// 配置轨道/目标分派只允许绑定真实 Agent，杜绝幽灵数据（红线 4 的前提约束）。
func agentExists(store *AgentStore, catDB *storepkg.DB, id string) bool {
	if id == "" {
		return false
	}
	if store.Get(id) != nil {
		return true
	}
	if srcRow, _ := catDB.GetAgentSource(id); srcRow != "" {
		return true
	}
	return false
}

func handleAgentConfigRoute(w http.ResponseWriter, r *http.Request, agentID string, store *AgentStore, catDB *storepkg.DB) {
	// 健壮性门禁：此前幽灵 Agent PUT 会 200 静默落库，GET 还会触发 nil 解引用 panic——双缺陷一并封堵
	if !agentExists(store, catDB, agentID) {
		http.Error(w, "agent not found: "+agentID, http.StatusNotFound)
		return
	}
	ver, content, _ := catDB.GetAgentConfig(agentID)
	a := store.Get(agentID)
	src := ""
	if a != nil {
		src = a.Source
	}

	switch r.Method {
	case http.MethodGet:
		if ver > 0 && content != "" {
			writeJSON(w, map[string]interface{}{
				"agent_id":      agentID,
				"version":       ver,
				"content":       content,
				"parsed":        parseAgentConfigDoc(content),
				"cfg_desired":   ver,
				"cfg_effective": a.CfgEffective,
				"source":        src,
			})
			return
		}
		// 无版本化配置：本机演示容器回退展示文件配置；其余回默认 doc（version=0，可直接 PUT 初始化）
		configPath := fmt.Sprintf("%s/%s.yaml", strings.TrimRight(cfgAgentConfDir, "/"), agentID)
		if data, err := os.ReadFile(configPath); err == nil && src == "docker" {
			writeJSON(w, map[string]interface{}{
				"agent_id": agentID, "version": 0, "content": string(data),
				"parsed": parseAgentConfigDoc(""), "file_fallback": true, "source": src,
			})
			return
		}
		writeJSON(w, map[string]interface{}{
			"agent_id": agentID, "version": 0, "content": "",
			"parsed": parseAgentConfigDoc(""), "source": src,
		})

	case http.MethodPut:
		var req struct {
			HostMetrics *map[string]interface{} `json:"host_metrics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostMetrics == nil {
			writeJSON(w, map[string]interface{}{"error": "host_metrics 配置段必填"})
			return
		}
		// 无版本化配置先引导出一份（targets 展开 + host_metrics 默认段）
		if ver == 0 {
			syncAgentConfig(store, catDB, agentID)
			ver, content, _ = catDB.GetAgentConfig(agentID)
		}
		// 兼容旧格式（纯数组 content）：统一解析保底，避免 targets 段丢失
		prev := parseAgentConfigDoc(content)
		prev["targets"] = normalizeTargets(prev["targets"])
		prev["host_metrics"] = *req.HostMetrics
		newContent, _ := json.MarshalIndent(prev, "", "  ")
		newVer, changed, err := catDB.SetAgentConfig(agentID, string(newContent))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if a := store.Get(agentID); a != nil {
			a.CfgDesired = newVer
		}
		if changed {
			addAudit("配置变更", agentID, "指标配置调整", fmt.Sprintf("host_metrics 段更新，版本 cfg-%d", newVer))
			InvalidateReconCache()
		}
		writeJSON(w, map[string]interface{}{"ok": true, "version": newVer, "changed": changed})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handlePluginStatus 查询 Agent 的插件运行状态
func handlePluginStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	containerName := agentContainerName(agentID)
	out, err := runCmd(".", "docker", "exec", containerName,
		"/home/deploy/SAgent/bin/SAgent", "-ctl", "status")
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error(), "plugins": map[string]string{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(strings.TrimSpace(out)))
}

// handleAgentMetrics 获取 Agent 的 Prometheus 指标
func handleAgentMetrics(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	out, err := runCmd(".", "docker", "exec", agentContainerName(agentID),
		"wget", "-qO-", "--timeout=3", "http://localhost:"+cfgAgentHTTPPort+"/metrics")
	if err != nil {
		http.Error(w, "failed to fetch metrics: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(out))
}

// handleAgentLogs 获取 Agent 的日志
func handleAgentLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	containerName := agentContainerName(agentID)
	out, err := runCmd(".", "docker", "exec", containerName,
		"tail", "-50", "/home/deploy/SAgent/logs/SAgent.log")
	if err != nil {
		http.Error(w, "failed to fetch logs: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(out))
}

// executePluginAction 执行插件级操作（通过 control.sock 或直接信号）
func executePluginAction(agentID, plugin, action string) map[string]interface{} {
	result := map[string]interface{}{
		"agent_id": agentID,
		"plugin":   plugin,
		"action":   action,
		"success":  true,
	}
	containerName := agentContainerName(agentID)
	sagentBin := "/home/deploy/SAgent/bin/SAgent"

	checkResult := func(out string, err error) {
		if err != nil {
			result["success"] = false
			result["error"] = err.Error()
			return
		}
		out = strings.TrimSpace(out)
		result["output"] = out
		if strings.HasPrefix(out, "ERROR") {
			result["success"] = false
			result["error"] = out
		}
	}

	switch action {
	case "reload":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("reload %s", plugin)))

	case "stop":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("stop %s", plugin)))

	case "start":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("start %s", plugin)))

	case "restart":
		executePluginAction(agentID, plugin, "stop")
		time.Sleep(1 * time.Second)
		return executePluginAction(agentID, plugin, "start")

	default:
		result["success"] = false
		result["error"] = "unknown plugin action: " + action
	}
	return result
}

// syncAgentConfig 目标变更后重算分派 Agent 的期望配置（结构化 doc：targets 段 + host_metrics 段）。
// host_metrics 段属于配置轨道（界面编辑产生），重算时原样保留；targets 段随采集目标变更刷新。
// 内容有变化则版本 +1（心跳热生效）。
func syncAgentConfig(store *AgentStore, catDB *storepkg.DB, agentID string) {
	if agentID == "" {
		return
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return
	}
	list := []map[string]interface{}{}
	for _, t := range targets {
		if t.AgentID == agentID {
			list = append(list, map[string]interface{}{
				"id": t.ID, "plugin": t.Plugin, "target": t.Name, "type": t.Type,
				"address": t.Address, "resource_id": t.ResourceID, "params_json": t.ParamsJSON,
			})
		}
	}
	doc := map[string]interface{}{"targets": list}
	// 配置轨道独立：保留既有 host_metrics 段，避免目标变更把界面下发的分组/例外冲掉
	// （parseAgentConfigDoc 兼容旧格式纯数组 content，避免解析失败导致配置段丢失）
	if _, content, err := catDB.GetAgentConfig(agentID); err == nil && content != "" {
		if hm, ok := parseAgentConfigDoc(content)["host_metrics"]; ok && hm != nil {
			doc["host_metrics"] = hm
		}
	}
	// 平台设备硬规则（PLAN §2.6）：承载平台能力的机器 host_metrics 强制开启、不可关闭——
	// 界面配置轨道里被关掉的，重算时在这里兜底拉起
	if rid, err := catDB.GetAgentResourceID(agentID); err == nil && rid != "" {
		if res, err := catDB.GetResource(rid); err == nil && res != nil && res.Role == "platform_device" {
			hm, ok := doc["host_metrics"].(map[string]interface{})
			if !ok || hm == nil {
				hm = defaultHostMetricsCfg()
			}
			hm["enabled"] = true
			doc["host_metrics"] = hm
		}
	}
	// 从未配置过的 Agent 补显式默认段，让下发配置自描述（不依赖 Agent 端隐式约定）
	if _, ok := doc["host_metrics"]; !ok {
		doc["host_metrics"] = defaultHostMetricsCfg()
	}
	content, _ := json.MarshalIndent(doc, "", "  ")
	ver, changed, err := catDB.SetAgentConfig(agentID, string(content))
	if err != nil {
		return
	}
	if a := store.Get(agentID); a != nil {
		a.CfgDesired = ver
	}
	if changed {
		addAudit("配置变更", agentID, "目标变更触发", fmt.Sprintf("版本 cfg-%d", ver))
	}
	InvalidateReconCache()
}

// refreshAgentStatuses 通过 Docker 查询 Agent 状态
func refreshAgentStatuses(store *AgentStore) {
	for _, agent := range store.List() {
		// 心跳型 Agent：状态由 last_seen 推导（90s 内在线），不做 docker 探测
		if agent.Source == "heartbeat" {
			if agent.LastSeen > 0 && time.Now().Unix()-agent.LastSeen > 90 {
				agent.Status = "offline"
			} else if agent.LastSeen > 0 {
				agent.Status = "healthy"
			}
			continue
		}
		out, err := runCmd(".", "docker", "inspect", "-f", "{{.State.Status}}", agentContainerName(agent.ID))
		if err != nil {
			agent.Status = "stopped"
		} else {
			status := strings.TrimSpace(out)
			if status == "running" {
				// 先检查 stopped 标记文件
				stoppedOut, _ := runCmd(".", "docker", "exec", agentContainerName(agent.ID),
					"cat", "/home/deploy/SAgent/run/stopped")
				// 再检查 SAgent health
				healthOut, _ := runCmd(".", "docker", "exec", agentContainerName(agent.ID),
					"wget", "-qO-", "--timeout=2", "http://localhost:"+cfgAgentHTTPPort+"/health")
				if strings.TrimSpace(healthOut) == `{"status":"healthy"}` {
					agent.Status = "healthy"
					// 探测真实版本
					if v := detectVersion(agent.ID); v != "" {
						agent.Version = v
					}
				} else if strings.TrimSpace(stoppedOut) != "" {
					// stopped 标记文件存在 → agent 已被管控停止
					agent.Status = "stopped"
				} else {
					agent.Status = "stopped"
				}
			} else {
				agent.Status = status
			}
		}
		agent.UpdatedAt = time.Now()
	}
}
