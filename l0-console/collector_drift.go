package main

// ============================================================
// SPEC-exporter采集机池人工触发漂移与回切（2026-09-27 立项，E2/E3/E4）
//   - E2 人工触发工具：/api/collectors/drift（按钮硬触发，trigger_source=manual 全审计）
//   - E3 回切：preferred_owner 归位（人工按钮 + 可选自动：稳定窗+冷却+先下新后停旧）
//   - E4 演练模式：采集机「模拟下线」标记（复用 vmagent 池标记表，零 Docker 依赖）
// 诊断层按用户口径后移（SPEC 3.5），本期迁移依据=人工判断，系统只保留冷却/单归属/候选集保护。
// ============================================================

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ---------- E3 回切：自动回切的稳定窗状态（人工回切不需要稳定窗，人已判断） ----------

var (
	fbStableMu    sync.Mutex
	fbFirstHealth = map[int64]int64{} // targetID → preferred 首次转健康的 unix 秒
)

// failbackTarget 执行一次回切：目标归属从当前采集机归位 preferred_owner。
// 校验：已配置 preferred / 未在首选上 / preferred 健康 / 冷却窗外；顺序先下新后停旧。
func failbackTarget(store *AgentStore, catDB *storepkg.DB, targetID int64, triggerSource string) *RelocateResult {
	t, err := catDB.GetTarget(targetID)
	if err != nil || t == nil {
		return &RelocateResult{TargetID: targetID, Status: "rejected", Note: "target not found"}
	}
	if !isRelocatableTarget(t) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Status: "rejected", Note: "本地能力插件不可迁移", Relocatable: false}
	}
	if t.PreferredOwner == "" {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Status: "rejected", Note: "未配置首选采集机（preferred_owner），无从回切", Relocatable: true}
	}
	if t.AgentID == t.PreferredOwner {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, ToAgent: t.PreferredOwner, Status: "skipped", Note: "已处于首选归属，无需回切", Relocatable: true}
	}
	if cooldownActive(catDB, t.ID) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, ToAgent: t.PreferredOwner, Status: "skipped", Note: "冷却期内，稍后再回切（终态保护）", Relocatable: true}
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, Status: "rejected", Note: "健康评估失败: " + err.Error(), Relocatable: true}
	}
	var pref *storepkg.AgentRow
	for _, a := range agents {
		if a.ID == t.PreferredOwner {
			pref = a
			break
		}
	}
	if pref == nil || !agentHealthy(pref) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, ToAgent: t.PreferredOwner,
			Status: "rejected", Note: "首选采集机 " + t.PreferredOwner + " 未恢复健康，禁止回切", Relocatable: true}
	}
	from := t.AgentID
	if _, err := catDB.UpdateTargetOwner(t.ID, t.PreferredOwner); err != nil {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: t.PreferredOwner, Status: "rejected", Note: "回切落库失败: " + err.Error(), Relocatable: true}
	}
	// 先下新后停旧：首选机先把任务接起来，再从旧属主摘除
	syncAgentConfig(store, catDB, t.PreferredOwner)
	if from != "" && from != t.PreferredOwner {
		syncAgentConfig(store, catDB, from)
	}
	_, _ = catDB.RecordRelocation(&storepkg.RelocationRow{
		TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: t.PreferredOwner,
		Region: t.Region, TenantID: t.TenantID, Reason: "failback: 首选采集机已恢复，归位 preferred_owner",
		Status: "moved", TriggerSource: triggerSource,
	})
	addAudit("HA-3 归属回切（"+triggerSource+"）", t.Name, from+" → "+t.PreferredOwner, "成功")
	InvalidateReconCache()
	return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: t.PreferredOwner,
		Status: "moved", Note: "已回切归位首选采集机", Relocatable: true}
}

// startRelocateFailback 自动回切调度（RELOCATE_AUTO_FAILBACK=1 启用，默认关）：
// preferred 恢复健康并持续稳定窗 + 冷却窗外 → 归位。人工回切不受稳定窗约束（人已拍板）。
func startRelocateFailback(store *AgentStore, catDB *storepkg.DB) {
	if !relocateBackAuto {
		return
	}
	go func() {
		for {
			func() {
				defer func() { recover() }()
				refreshSimDownSet(catDB)
				targets, err := catDB.ListTargets()
				if err != nil {
					return
				}
				agents, err := catDB.ListAgentRows()
				if err != nil {
					return
				}
				healthy := map[string]bool{}
				for _, a := range agents {
					healthy[a.ID] = agentHealthy(a)
				}
				now := time.Now().Unix()
				for _, t := range targets {
					if !isRelocatableTarget(t) || t.PreferredOwner == "" || t.AgentID == t.PreferredOwner {
						delete(fbFirstHealth, t.ID)
						continue
					}
					if !healthy[t.PreferredOwner] {
						delete(fbFirstHealth, t.ID) // 又挂了：稳定窗清零重计（防"恢复即崩"反复横跳）
						continue
					}
					fbStableMu.Lock()
					first, seen := fbFirstHealth[t.ID]
					if !seen {
						fbFirstHealth[t.ID] = now
						fbStableMu.Unlock()
						continue
					}
					fbStableMu.Unlock()
					if now-first < int64(relocateBackStable) || cooldownActive(catDB, t.ID) {
						continue
					}
					if res := failbackTarget(store, catDB, t.ID, "failback_auto"); res.Status == "moved" {
						delete(fbFirstHealth, t.ID)
						return // 每轮一个目标，防连锁
					}
				}
			}()
			time.Sleep(time.Duration(relocateAutoInt) * time.Second)
		}
	}()
}

// ---------- E2/E4/E5：池总览与人工操作 API ----------

// poolTargetView 界面池卡的一条采集任务（归属/候选/冷却一目了然）。
type poolTargetView struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	AgentID        string   `json:"agent_id"`
	ResourceID     string   `json:"resource_id"`
	PreferredOwner string   `json:"preferred_owner"`
	Candidates     []string `json:"candidates"`       // 规则级候选集（空=池口径）
	DriftTo        []string `json:"drift_to"`         // 此刻实际可漂移的目标（候选∩健康）
	Cooldown       bool     `json:"cooldown"`         // 冷却期内（按钮置灰依据）
	CanFailback    bool     `json:"can_failback"`     // 可回切（preferred 已恢复健康）
	ProcState      string   `json:"proc_state"`       // R0-2 observed_state：running|stopped|unknown（属主侧进程态）
	ProcNote       string   `json:"proc_note"`        // 进程态补充说明（standby 预置连通性等）
	StandbyHolders []string `json:"standby_holders"`  // R0-4：持有本目标 standby 预置的采集机（派生口径）
	StandbyReady   []string `json:"standby_ready"`    // 其中连通性验证通过（真正"预置就绪"）的
}

// poolMemberView 界面池卡的一台采集机。
type poolMemberView struct {
	ID      string `json:"id"`
	Pool    string `json:"pool"`
	Healthy bool   `json:"healthy"`
	SimDown bool   `json:"sim_down"`
	LastSeen int64 `json:"last_seen"`
	Load    int    `json:"load"`
}

// observedIndex 聚合各采集机心跳 stats.observed_targets（R0-2）：
// agentID → resourceID → 观测条目。读不到（老 Agent / 尚未回报）返回空——
// 界面按"未知"渲染，不假造进程态。
func observedIndex(agents []*storepkg.AgentRow) map[string]map[string]map[string]interface{} {
	idx := map[string]map[string]map[string]interface{}{}
	for _, a := range agents {
		if a.Stats == nil {
			continue
		}
		obs, ok := a.Stats["observed_targets"].(map[string]interface{})
		if !ok {
			continue
		}
		per := map[string]map[string]interface{}{}
		for rid, entry := range obs {
			if m, ok := entry.(map[string]interface{}); ok {
				per[rid] = m
			}
		}
		if len(per) > 0 {
			idx[a.ID] = per
		}
	}
	return idx
}

// standbyHolderIndex R0-4 反向索引：targetID → 持有其 standby 预置的采集机清单
// （预置派生口径逐台采集机计算后聚合）。
func standbyHolderIndex(agents []*storepkg.AgentRow, targets []*storepkg.TargetRow) map[int64][]string {
	out := map[int64][]string{}
	for _, a := range agents {
		if a.ID == "" || a.Source != "heartbeat" || !isCollectorAgent(a) {
			continue
		}
		if !agentSupportsManagedTargets(a.Version, a.Stats) {
			continue
		}
		for _, t := range deriveStandbyPresets(agents, targets, a.ID, cfgStandbyPresetMax) {
			out[t.ID] = append(out[t.ID], a.ID)
		}
	}
	return out
}

// collectorPoolOverview 池总览：成员态 + 任务归属 + 最近迁移时间线，前端单请求渲染。
func collectorPoolOverview(catDB *storepkg.DB, tenant string) map[string]interface{} {
	refreshSimDownSet(catDB)
	agents, _ := catDB.ListAgentRows()
	targets, _ := catDB.ListTargets()
	tenant = tenantOr(tenant)
	loads := map[string]int{}
	for _, t := range targets {
		if isRelocatableTarget(t) {
			loads[t.AgentID]++
		}
	}
	members := []poolMemberView{}
	for _, a := range agents {
		if a.ID == "" || !isCollectorAgent(a) || tenantOr(a.TenantID) != tenant || a.Source != "heartbeat" {
			continue
		}
		members = append(members, poolMemberView{
			ID: a.ID, Pool: agentPool(a), Healthy: agentHealthy(a),
			SimDown: memberSimDown(a.ID), LastSeen: a.LastSeen, Load: loads[a.ID],
		})
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	// R0-2/R0-4：进程态与预置就绪度（看进程不再需要进主机 ps）
	obs := observedIndex(agents)
	holders := standbyHolderIndex(agents, targets)
	ownerRow := map[string]*storepkg.AgentRow{}
	for _, a := range agents {
		ownerRow[a.ID] = a
	}
	tViews := []poolTargetView{}
	for _, t := range targets {
		if !isRelocatableTarget(t) || tenantOr(t.TenantID) != tenant {
			continue
		}
		agentsSnap := agents
		cands := candidateList(agentsSnap, t, t.AgentID)
		var pref *storepkg.AgentRow
		if t.PreferredOwner != "" {
			for _, a := range agents {
				if a.ID == t.PreferredOwner {
					pref = a
					break
				}
			}
		}
		// 属主侧进程态：优先 observed_state（新 Agent 心跳回报）；无回报按"未知"如实渲染
		procState, procNote := "unknown", ""
		if o, ok := obs[t.AgentID][t.ResourceID]; ok {
			state, _ := o["state"].(string)
			switch {
			case state == "active":
				if p, _ := o["proc"].(bool); p {
					procState = "running"
				} else {
					procState = "stopped"
				}
			case state == "standby":
				procState = "standby"
				if n, _ := o["note"].(string); n != "" {
					procNote = n
				}
			}
		} else if ownerRow[t.AgentID] != nil && !agentHealthy(ownerRow[t.AgentID]) {
			procState, procNote = "unknown", "属主心跳离线，进程态不可考"
		}
		// standby 预置就绪度：持有机里连通性验证通过（reachable=true）的才算就绪
		var ready, holderList []string
		for _, h := range holders[t.ID] {
			holderList = append(holderList, h)
			if o, ok := obs[h][t.ResourceID]; ok {
				if r, _ := o["reachable"].(bool); r {
					ready = append(ready, h)
				}
			}
		}
		tViews = append(tViews, poolTargetView{
			ID: t.ID, Name: t.Name, Address: t.Address, AgentID: t.AgentID,
			ResourceID: t.ResourceID,
			PreferredOwner: t.PreferredOwner, Candidates: ruleCandidates(t),
			DriftTo: cands, Cooldown: cooldownActive(catDB, t.ID),
			CanFailback: t.PreferredOwner != "" && t.AgentID != t.PreferredOwner && pref != nil && agentHealthy(pref),
			ProcState: procState, ProcNote: procNote,
			StandbyHolders: holderList, StandbyReady: ready,
		})
	}
	rels, _ := catDB.ListRelocationsAll(20)
	return map[string]interface{}{
		"members": members, "targets": tViews, "relocations": rels,
		"auto":    map[string]bool{"relocate": relocateAutoOn, "failback": relocateBackAuto},
		"params":  map[string]int{"fail_sec": relocateFailSec, "cooldown_sec": relocateCoolSec, "back_stable_sec": relocateBackStable},
	}
}

// registerCollectorDriftRoutes SPEC E2/E3/E4 人工漂移/回切/演练 API。
func registerCollectorDriftRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	// GET /api/collectors/poolview —— 池总览（前端 5s 轮询数据源）
	mux.HandleFunc("/api/collectors/poolview", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, collectorPoolOverview(catDB, r.URL.Query().Get("tenant")))
	})

	// POST /api/collectors/sim —— 演练：模拟采集机下线/恢复 {member, down}
	mux.HandleFunc("/api/collectors/sim", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			Member string `json:"member"`
			Down   bool   `json:"down"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Member == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "member 必填"})
			return
		}
		if err := catDB.SetVmagentMemberSimDown(q.Member, q.Down); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		refreshSimDownSet(catDB)
		status := "drill_up"
		note := "演练：模拟采集机恢复上线"
		if q.Down {
			status = "drill_down"
			note = "演练：模拟采集机下线（指标异常场景）"
		}
		_, _ = catDB.RecordRelocation(&storepkg.RelocationRow{
			TargetName: "[drill] " + q.Member, FromAgent: q.Member, ToAgent: q.Member,
			Reason: note, Status: status, TriggerSource: "drill",
		})
		addAudit("采集机演练（"+status+"）", q.Member, "人工触发", "已标记")
		writeJSON(w, map[string]interface{}{"ok": true, "member": q.Member, "down": q.Down})
	})

	// POST /api/collectors/candidates —— 配置规则级候选集与首选归属 {target_id, candidates, preferred_owner}
	mux.HandleFunc("/api/collectors/candidates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetID       int64    `json:"target_id"`
			Candidates     []string `json:"candidates"`
			PreferredOwner string   `json:"preferred_owner"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.TargetID == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "target_id 必填"})
			return
		}
		clean := []string{}
		seen := map[string]bool{}
		for _, c := range q.Candidates {
			if c != "" && !seen[c] {
				seen[c] = true
				clean = append(clean, c)
			}
		}
		if len(clean) == 1 && q.PreferredOwner == "" {
			// 候选只剩 1 台且未指定首选：允许（单机兜底），但首选默认即它，回切才有归位点
			q.PreferredOwner = clean[0]
		}
		b, _ := json.Marshal(clean)
		if err := catDB.SetTargetCandidates(q.TargetID, string(b), q.PreferredOwner); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		addAudit("规则候选集配置", strconv.FormatInt(q.TargetID, 10), "人工配置", "候选 "+strconv.Itoa(len(clean))+" 台 / 首选 "+orDash(q.PreferredOwner))
		writeJSON(w, map[string]interface{}{"ok": true, "candidates": clean, "preferred_owner": q.PreferredOwner})
	})
	// POST /api/collectors/drift —— 人工硬触发漂移 {target_id, to_agent, reason}
	mux.HandleFunc("/api/collectors/drift", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetID int64  `json:"target_id"`
			ToAgent  string `json:"to_agent"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.TargetID == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "target_id 必填"})
			return
		}
		reason := q.Reason
		if reason == "" {
			reason = "manual: 界面按钮硬触发（人工判断指标异常）"
		}
		writeJSON(w, runRelocationTo(store, catDB, q.TargetID, q.ToAgent, "manual", reason))
	})

	// POST /api/collectors/failback —— 人工回切 {target_id}
	mux.HandleFunc("/api/collectors/failback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetID int64 `json:"target_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.TargetID == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "target_id 必填"})
			return
		}
		writeJSON(w, failbackTarget(store, catDB, q.TargetID, "failback_manual"))
	})
}

