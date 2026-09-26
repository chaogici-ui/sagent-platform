package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// ============================================================
// R2 采集机池候选与选机（"选承载主机"这一步）
// 设计依据：SPEC-远程采集集中部署与漂移提速-边界与实施计划.md
//
// 两步语义（用户 2026-09-24 补充）：
//   ① 选承载主机（xx 采集机）——本文件：池候选 + 承载数 + 自动挑机 + 人工改选
//   ② 配被采对象（IP / 账号 / 口令）——collect_params 原子，字段由插件 params.yaml 声明
//
// 只读复用 agents / targets 台账，不新增表、不新增探测链。
// 池与健康口径与 HA-3 漂移（relocate.go）完全同一份函数——"选谁承载"与"故障漂到谁"
// 必须用同一把尺子，否则会出现"选的时候说健康、漂的时候说不可用"。
// ============================================================

// isCollectorMode 需要"选承载采集机"的模式：仅远程采集。
// 混合采集（hybrid）的承载机就是被采主机自身——同一台机器既要采本机（边缘能力）又要采远端，
// 若被自动挑到别的采集机上，"采本机"就无处落地，因此 hybrid 不做跨机挑机，维持资源自身承载。
func isCollectorMode(mode string) bool {
	return mode == "remote"
}

// CollectorCandidate 一台候选采集机（选机卡与自动挑机共用同一份事实）。
type CollectorCandidate struct {
	AgentID     string `json:"agent_id"`
	Pool        string `json:"pool"`
	Region      string `json:"region"`
	Healthy     bool   `json:"healthy"`
	Load        int    `json:"load"` // 已承载的远端采集目标数（越小越空闲）
	Version     string `json:"version,omitempty"`
	LastSeen    int64  `json:"last_seen"`
	Recommended bool   `json:"recommended"`
	Reason      string `json:"reason,omitempty"`
}

// CollectorPoolView 一个采集机池的候选视图。池内健康采集机 <2 时故障后无承接方。
type CollectorPoolView struct {
	Pool         string               `json:"pool"`
	HealthyCount int                  `json:"healthy_count"`
	Insufficient bool                 `json:"insufficient"`
	Candidates   []CollectorCandidate `json:"candidates"`
}

// collectorLoads 各采集机当前承载的远端采集目标数（只读台账聚合）。
func collectorLoads(catDB *storepkg.DB) map[string]int {
	loads := map[string]int{}
	targets, err := catDB.ListTargets()
	if err != nil {
		return loads
	}
	for _, t := range targets {
		if !isRelocatableTarget(t) {
			continue
		}
		loads[t.AgentID]++
	}
	return loads
}

// collectorPoolViews 按池聚合候选采集机（同租户 + 是采集机）。
// 组内健康者在前，再按负载升序、id 升序（确定性，便于取证复现）。
func collectorPoolViews(catDB *storepkg.DB, tenant string) []CollectorPoolView {
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return nil
	}
	loads := collectorLoads(catDB)
	tenant = tenantOr(tenant)
	groups := map[string]*CollectorPoolView{}
	for _, a := range agents {
		if a.ID == "" || !isCollectorAgent(a) || tenantOr(a.TenantID) != tenant {
			continue
		}
		// 只收心跳注册的真实采集机（与 relocate.go 的健康口径同源）：source=docker 的演示种子行
		// （如 sagent-proxy）不是真机，agentHealthy 对其恒为 false——留在池清单里只会多出
		// "永不健康的幽灵候选"和一个空池名，误导运维以为该池有成员。
		if a.Source != "heartbeat" {
			continue
		}
		pool := agentPool(a)
		g := groups[pool]
		if g == nil {
			g = &CollectorPoolView{Pool: pool}
			groups[pool] = g
		}
		healthy := agentHealthy(a)
		if healthy {
			g.HealthyCount++
		}
		g.Candidates = append(g.Candidates, CollectorCandidate{
			AgentID: a.ID, Pool: pool, Region: agentRegion(a),
			Healthy: healthy, Load: loads[a.ID],
			Version: a.Version, LastSeen: a.LastSeen,
		})
	}
	out := []CollectorPoolView{}
	for _, g := range groups {
		sort.SliceStable(g.Candidates, func(i, j int) bool {
			if g.Candidates[i].Healthy != g.Candidates[j].Healthy {
				return g.Candidates[i].Healthy
			}
			if g.Candidates[i].Load != g.Candidates[j].Load {
				return g.Candidates[i].Load < g.Candidates[j].Load
			}
			return g.Candidates[i].AgentID < g.Candidates[j].AgentID
		})
		g.Insufficient = g.Pool != "" && g.HealthyCount < 2
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	return out
}

// autoPickCollector 自动挑机：同租户池内健康采集机中负载最低者（并列取 id 最小）。
// 池内无健康采集机 → 返回空串（不硬塞，如实回落并交人工改选）。
func autoPickCollector(catDB *storepkg.DB, tenant string) (agentID, pool, reason string) {
	views := collectorPoolViews(catDB, tenant)
	best := CollectorCandidate{}
	found := false
	for _, v := range views {
		for _, c := range v.Candidates {
			if !c.Healthy {
				continue
			}
			if !found || c.Load < best.Load || (c.Load == best.Load && c.AgentID < best.AgentID) {
				best, found = c, true
			}
		}
	}
	if !found {
		return "", "", ""
	}
	return best.AgentID, best.Pool, "池内健康采集机中负载最低（当前承载 " + strconv.Itoa(best.Load) + " 个远端采集目标）"
}

// markRecommended 在池视图上标出推荐项（选机卡据此给默认勾选）。
func markRecommended(views []CollectorPoolView, rec, reason string) {
	if rec == "" {
		return
	}
	for i := range views {
		for j := range views[i].Candidates {
			if views[i].Candidates[j].AgentID == rec {
				views[i].Candidates[j].Recommended = true
				views[i].Candidates[j].Reason = reason
			}
		}
	}
}

// collectorFactOf 取一台采集机的事实（台账行 / 承载数 / 是否健康）。台账无此机时行返回 nil。
func collectorFactOf(catDB *storepkg.DB, agentID string) (*storepkg.AgentRow, int, bool) {
	if agentID == "" {
		return nil, 0, false
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return nil, 0, false
	}
	loads := collectorLoads(catDB)
	for _, a := range agents {
		if a.ID == agentID {
			return a, loads[agentID], agentHealthy(a)
		}
	}
	return nil, loads[agentID], false
}

// poolOrDash 池未登记时显示占位，避免界面出现空白字段。
func poolOrDash(pool string) string {
	if pool == "" {
		return "未登记"
	}
	return pool
}

// registerCollectorRoutes R2 采集机选机 API（候选清单 + 人工改选）。
func registerCollectorRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	// ---- 候选采集机清单：按池聚合，带承载数与健康态（前端零写死）----
	mux.HandleFunc("/api/onboard/collectors", func(w http.ResponseWriter, r *http.Request) {
		tenant := r.URL.Query().Get("tenant")
		if tenant == "" {
			if rid := r.URL.Query().Get("resource_id"); rid != "" {
				if res, _ := catDB.GetResource(rid); res != nil {
					tenant = res.TenantID
				}
			}
		}
		views := collectorPoolViews(catDB, tenant)
		rec, pool, reason := autoPickCollector(catDB, tenant)
		markRecommended(views, rec, reason)
		writeJSON(w, map[string]interface{}{
			"pools": views, "recommended": rec, "recommended_pool": pool,
			"recommended_reason": reason, "auto": rec != "",
		})
	})

	// ---- 人工改选承载采集机 ----
	// 校验"确为采集机 + 同租户"，落 flow.agent_id，并把本流水线已建的采集目标一并改绑
	// （否则"界面改了承载机、目标还指向旧机"，实际采集仍在旧机上跑）。
	mux.HandleFunc("/api/onboard/flow/collector", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID  int64  `json:"flow_id"`
			AgentID string `json:"agent_id"`
			Reason  string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		agentID := strings.TrimSpace(req.AgentID)
		if agentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "agent_id 必填"})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "flow not found"})
			return
		}
		if !isCollectorMode(f.Mode) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "仅远程采集模式需要选择承载采集机（当前模式：" + f.Mode + "）"})
			return
		}
		a, load, healthy := collectorFactOf(catDB, agentID)
		if a == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "采集机 " + agentID + " 不在台账中"})
			return
		}
		if !isCollectorAgent(a) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "机器 " + agentID + " 不是采集机（需 type=proxy 或 labels.role=collector）——远端采集只承载在采集机上"})
			return
		}
		// 跨租户承载一律拒绝：远端采集的账号口令要落在承载机上，跨租户等于把凭据送错域
		tenant := ""
		if res, _ := catDB.GetResource(f.ResourceID); res != nil {
			tenant = res.TenantID
		}
		if tenantOr(tenant) != tenantOr(a.TenantID) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "采集机与资源不属同一租户，拒绝跨租户承载"})
			return
		}
		pool := agentPool(a)
		// 不健康仍可选（人有权拍板），但必须把话说清楚——不让"选了台心跳陈旧的机器"变成静默决定
		warn := ""
		if !healthy {
			warn = "该采集机当前心跳陈旧（最近 " + strconv.FormatInt(a.LastSeen, 10) + "），选择后安装环节可能失败"
		}
		prev := f.AgentID
		if err := catDB.UpdateFlowAgent(f.ID, agentID); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "承载机落库失败：" + err.Error()})
			return
		}
		// 已建采集目标随承载机改绑
		rebound := 0
		if targets, err := catDB.ListTargets(); err == nil {
			for _, t := range targets {
				if t.FlowID != f.ID || t.AgentID == agentID {
					continue
				}
				if _, err := catDB.UpdateTargetOwner(t.ID, agentID); err == nil {
					rebound++
				}
			}
		}
		agents, _ := catDB.ListAgentRows()
		healthyN := poolHealthyCollectors(agents, tenantOr(tenant), pool)

		var step storepkg.FlowStepSnapshot
		hasStep := false
		for _, s := range f.Steps {
			if s.ID == "pick_proxy" {
				step, hasStep = s, true
				break
			}
		}
		if hasStep {
			prevStatus := stPending
			if last, _ := catDB.LatestEvent(f.ID, "pick_proxy"); last != nil {
				prevStatus = last.Status
			}
			ds := []diag{}
			if warn != "" {
				ds = append(ds, warnDiag("collector_stale", warn, "可在该采集机恢复心跳后重选；或改选池内其它健康采集机"))
			}
			if pool != "" && healthyN < 2 {
				ds = append(ds, warnDiag("pool_insufficient",
					"池 "+pool+" 内健康采集机不足（"+strconv.Itoa(healthyN)+"<2）",
					"该池故障后无承接方，采集目标无法自动漂移——建议补足池内采集机"))
			}
			_ = appendStepEvent(catDB, f.ID, step, stOK,
				"承载采集机（人工改选）："+agentID+"（池 "+poolOrDash(pool)+"，承载 "+strconv.Itoa(load)+" 个远端采集目标）",
				detailJSON(map[string]any{
					"agent_id": agentID, "pool": pool, "load": load, "healthy": healthy,
					"pool_healthy": healthyN, "source": "human_pick",
					"operator": cfgAuditOperator, "reason": strings.TrimSpace(req.Reason),
					"rebound_targets": rebound,
					"diagnosis":       diagJSON(ds),
				}), durationSinceLast(catDB, f.ID, "pick_proxy", prevStatus))
		}
		if rebound > 0 {
			syncAgentConfig(store, catDB, agentID)
			if prev != "" && prev != agentID {
				syncAgentConfig(store, catDB, prev)
			}
			InvalidateReconCache()
		}
		addAudit("人工改选承载采集机", f.ResourceID, "接入中心", agentID+"（池 "+poolOrDash(pool)+"）")
		if err := advanceFlow(catDB, store, f.ID); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "agent_id": agentID, "pool": pool, "load": load,
			"healthy": healthy, "pool_healthy": healthyN, "warning": warn,
			"rebound_targets": rebound,
		})
	})
}