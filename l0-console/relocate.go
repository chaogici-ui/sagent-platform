package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ============================================================
// HA-3 跨 L1 归属迁移（Exporter 归属故障漂移——跨机承接）
// 目标(owner 采集机)采集中断超时 → 迁到「同租户 + 同池 + 健康 heartbeat」的**池内采集机**，
// 走 config 轨接管：改 targets.agent_id + 新端 syncAgentConfig(起 exporter) + 旧端 syncAgentConfig(停)。
// 幂等 + 冷却终态保护 + 仅远端采集类型可迁移 + 仅读信号驱动(不新增探测链)。
// R1/R3：候选池收窄为采集机池成员（普通 SAgent 不入候选）；池内健康成员 <2 不迁移（转告警）。
// ============================================================

// env 化阈值（红线 2/3：所有魔数走 main.go env 登记）
// R4/R5 提速档位 B：采集机用 10s 短心跳（install 渲染），故失效阈值取 30s（≈3 个心跳）、
// 扫描 10s，自动调度默认打开。检测最坏 ≈ 心跳 10s×3 + 扫描 10s ≈ 40s（对比旧档 150s）。
// 阈值 30s 只作用在采集机（候选与可迁目标的属主都是采集机），普通 SAgent 不参与漂移判定。
var (
	relocateCoolSec = envIntOr("RELOCATE_COOLDOWN_SEC", 300)     // 同目标迁移后冷却，期间不再迁
	relocateFailSec = envIntOr("RELOCATE_FAIL_SEC", 30)          // 采集机失效判定阈值(秒)：10s 心跳×3
	relocateEdgeSec = envIntOr("RELOCATE_EDGE_FAIL_SEC", 120)    // 非采集机（30s 心跳）失效阈值：维持旧口径，防误判
	relocateAutoOn  = envOr("RELOCATE_AUTO", "1") != ""          // "1" 启用自动调度（R4 默认打开；置空可关）
	relocateAutoInt = envIntOr("RELOCATE_AUTO_INTERVAL_SEC", 10) // 自动调度轮询间隔
)

// localOnlyPlugins 本地能力插件：这类目标(exporter 随 agent 任一机本地跑)无法跨机迁移，
// 不入迁移评审/候选（决策点5：仅远端采集可跨 L1）。
var localOnlyPlugins = map[string]bool{
	"host_metrics":   true,
	"docker_metrics": true,
	"sagent":         true,
	"sagent_health":  true,
	"port_checker":   true,
}

// isRelocatableTarget 仅当已分派且非本地能力插件才可跨 L1。
func isRelocatableTarget(t *storepkg.TargetRow) bool {
	if t == nil || t.AgentID == "" {
		return false
	}
	return !localOnlyPlugins[t.Type] && !localOnlyPlugins[t.Plugin]
}

func agentRegion(a *storepkg.AgentRow) string {
	if a == nil || a.Labels == nil {
		return ""
	}
	if r := a.Labels["region"]; r != "" {
		return r
	}
	return a.Labels["availability_zone"]
}

// agentHealthy heartbeat 源且 last_seen 在失效阈值内视为健康承接端。
// 阈值按身份分档（R5）：采集机用 30s（配 10s 短心跳，≈3 拍），其余用 120s（配 30s 心跳）——
// 只给采集机提速，避免普通 SAgent 因单拍抖动被误判失效。
func agentHealthy(a *storepkg.AgentRow) bool {
	if a == nil || a.Source != "heartbeat" {
		return false
	}
	if a.LastSeen <= 0 {
		return false
	}
	limit := relocateEdgeSec
	if isCollectorAgent(a) {
		limit = relocateFailSec
	}
	return time.Now().Unix()-a.LastSeen <= int64(limit)
}

// ---------- R1：采集机池建模（零新增表，复用 agents.type / labels_json） ----------
// 约定：身份 = agents.type ∈ {proxy, collector, collector_proxy}（Collector Proxy，兼容演示环境既有
//      取值 collector_proxy）或 labels.role=='collector'；
//      池归属 = labels.pool，未登记回落 region/availability_zone。
// 语义：只有采集机才是远端采集的承载端；普通 SAgent（万台级 edge）不入漂移候选池。

// collectorTypeAliases 采集机类型别名：规范值 proxy；collector/collector_proxy 为既有取值，一并认作采集机。
var collectorTypeAliases = map[string]bool{
	"proxy":           true,
	"collector":       true,
	"collector_proxy": true,
}

// isCollectorType 类型串是否为采集机（供 AgentRow 与内存 Agent 共用同一口径）。
func isCollectorType(t string) bool { return collectorTypeAliases[t] }

// isCollectorAgent 是否采集机（远端采集承载端）。
func isCollectorAgent(a *storepkg.AgentRow) bool {
	if a == nil {
		return false
	}
	if isCollectorType(a.Type) {
		return true
	}
	return a.Labels != nil && a.Labels["role"] == "collector"
}

// agentPool 采集机池归属：labels.pool 优先，未登记回落 region。
func agentPool(a *storepkg.AgentRow) string {
	if a == nil || a.Labels == nil {
		return ""
	}
	if p := a.Labels["pool"]; p != "" {
		return p
	}
	return agentRegion(a)
}

// tenantOr 空租户归一为 default（与 store 侧 COALESCE(tenant_id,'default') 同口径）。
func tenantOr(t string) string {
	if t == "" {
		return "default"
	}
	return t
}

// targetPool 目标所属池：以当前属主的池归属为准，保证漂移不跨池；
// 属主不可考（已注销）时回落 targets.region。两者皆空返回空串（池未登记，无从判定）。
func targetPool(agents []*storepkg.AgentRow, t *storepkg.TargetRow) string {
	for _, a := range agents {
		if a.ID != "" && a.ID == t.AgentID {
			if p := agentPool(a); p != "" {
				return p
			}
			break
		}
	}
	return t.Region
}

// candidateList 枚举可承接的采集机：同租户 + 健康 heartbeat + 是采集机 + 同池(池已登记时)
// + region 匹配(已登记时) + 非属主。稳定排序（确定性，便于取证复现）。
func candidateList(agents []*storepkg.AgentRow, t *storepkg.TargetRow, owner string) []string {
	tenant := tenantOr(t.TenantID)
	pool := targetPool(agents, t)
	out := []string{}
	for _, a := range agents {
		if a.ID == "" || a.ID == owner {
			continue
		}
		if tenantOr(a.TenantID) != tenant || !agentHealthy(a) || !isCollectorAgent(a) {
			continue
		}
		if pool != "" && agentPool(a) != pool {
			continue
		}
		if t.Region != "" && agentRegion(a) != t.Region {
			continue
		}
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return out
}

// poolHealthyCollectors 池内健康采集机数（同租户 + 同池 + 健康 + 是采集机）。
// 属主失效时自身不计入，故该值即"可供承接的池内健康端"数量。
func poolHealthyCollectors(agents []*storepkg.AgentRow, tenant, pool string) int {
	n := 0
	for _, a := range agents {
		if a.ID == "" || tenantOr(a.TenantID) != tenant || !agentHealthy(a) || !isCollectorAgent(a) {
			continue
		}
		if pool != "" && agentPool(a) != pool {
			continue
		}
		n++
	}
	return n
}

// poolInsufficient 池内健康采集机不足（<2）：不迁移，转告警（G6）。
// 池降到唯一幸存者时继续漂移只会把负载堆到它身上，宁可告警转人工。
// 池未登记（pool==""）时无从判定池健康，不做此保护（回落旧行为）。
func poolInsufficient(agents []*storepkg.AgentRow, tenant, pool string) bool {
	if pool == "" {
		return false
	}
	return poolHealthyCollectors(agents, tenant, pool) < 2
}

// pickCandidateFrom 在给定 agent 快照上选承接端：候选清单内稳定排序取最小 id。无匹配返回空串。
func pickCandidateFrom(agents []*storepkg.AgentRow, t *storepkg.TargetRow, owner string) string {
	cands := candidateList(agents, t, owner)
	if len(cands) == 0 {
		return ""
	}
	return cands[0]
}

// pickCandidate 选承接端（自取 agent 快照）。
func pickCandidate(catDB *storepkg.DB, t *storepkg.TargetRow, owner string) (string, error) {
	if owner == "" {
		return "", nil
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return "", err
	}
	return pickCandidateFrom(agents, t, owner), nil
}

// cooldownActive 同目标最近一次迁移仍在冷却期内且本就有归属 → 拒绝再次迁移（终态保护）。
func cooldownActive(catDB *storepkg.DB, targetID int64) bool {
	r, err := catDB.LatestRelocationByTarget(targetID)
	if err != nil || r == nil {
		return false
	}
	return time.Now().Unix()-r.CreatedAt < int64(relocateCoolSec)
}

// RelocateResult 一次迁移的结论（供 API 回显与自动调度日志）。
type RelocateResult struct {
	TargetID    int64    `json:"target_id"`
	TargetName  string   `json:"target_name"`
	FromAgent   string   `json:"from_agent"`
	ToAgent     string   `json:"to_agent"`
	Region      string   `json:"region"`
	Pool        string   `json:"pool,omitempty"`         // 采集机池（漂移不跨池）
	PoolHealthy int      `json:"pool_healthy"`           // 池内健康采集机数（<2 则不迁移）
	Reason      string   `json:"reason"`
	Status      string   `json:"status"` // moved|rejected|skipped|plan
	Note        string   `json:"note"`
	Relocatable bool     `json:"relocatable"`
	Candidates  []string `json:"candidates,omitempty"`
}

// planRelocation 只评审不下发：返回是否可迁、候选清单与选定的承接端。
func planRelocation(catDB *storepkg.DB, targetID int64) *RelocateResult {
	t, err := catDB.GetTarget(targetID)
	if err != nil || t == nil {
		return &RelocateResult{TargetID: targetID, Status: "rejected", Note: "target not found"}
	}
	res := &RelocateResult{
		TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID,
		Region: t.Region, Relocatable: isRelocatableTarget(t),
	}
	if !res.Relocatable {
		res.Status = "rejected"
		res.Note = "本地能力插件不可跨 L1 迁移"
		return res
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		res.Status = "rejected"
		res.Note = "候选评估失败: " + err.Error()
		return res
	}
	tenant := tenantOr(t.TenantID)
	res.Pool = targetPool(agents, t)
	res.PoolHealthy = poolHealthyCollectors(agents, tenant, res.Pool)
	res.Candidates = candidateList(agents, t, t.AgentID)
	if poolInsufficient(agents, tenant, res.Pool) {
		res.Status = "rejected"
		res.Note = fmt.Sprintf("池内健康采集机不足（%d<2），不迁移，转告警", res.PoolHealthy)
		return res
	}
	to := pickCandidateFrom(agents, t, t.AgentID)
	if to == "" {
		res.Status = "rejected"
		res.Note = "池内无健康采集机候选"
		return res
	}
	res.ToAgent = to
	res.Status = "plan"
	return res
}

// runRelocation 执行一次目标归属迁移（先新后旧：新端起 config → 旧端停），带池边界/池内不足/冷却/审计。
func runRelocation(store *AgentStore, catDB *storepkg.DB, targetID int64, reason string) *RelocateResult {
	t, err := catDB.GetTarget(targetID)
	if err != nil || t == nil {
		return &RelocateResult{TargetID: targetID, Status: "rejected", Note: "target not found"}
	}
	if !isRelocatableTarget(t) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Status: "rejected", Note: "本地能力插件不可跨 L1 迁移", Relocatable: false}
	}
	if cooldownActive(catDB, t.ID) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Status: "skipped", Note: "冷却期内，跳过重复迁移（终态保护）", Relocatable: true}
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Status: "rejected", Note: "候选评估失败: " + err.Error(), Relocatable: true}
	}
	tenant := tenantOr(t.TenantID)
	pool := targetPool(agents, t)
	healthy := poolHealthyCollectors(agents, tenant, pool)
	// G6：池内健康采集机不足 → 不迁移（转告警由 collector_pool_degraded 规则承担）
	if poolInsufficient(agents, tenant, pool) {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Pool: pool, PoolHealthy: healthy,
			Status: "rejected", Note: fmt.Sprintf("池内健康采集机不足（%d<2），不迁移，转告警", healthy), Relocatable: true}
	}
	to := pickCandidateFrom(agents, t, t.AgentID)
	if to == "" {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: t.AgentID, Pool: pool, PoolHealthy: healthy,
			Status: "rejected", Note: "池内无健康采集机候选", Relocatable: true}
	}
	from := t.AgentID
	// 归属迁移（仅改 agent_id）
	if _, err := catDB.UpdateTargetOwner(t.ID, to); err != nil {
		return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: to, Status: "rejected", Note: "迁移落库失败: " + err.Error(), Relocatable: true}
	}
	// 先新后旧（4A）：新端 config 加入目标起 exporter；旧端 config 移出目标停 exporter
	syncAgentConfig(store, catDB, to)
	if from != to && from != "" {
		syncAgentConfig(store, catDB, from)
	}
	_, _ = catDB.RecordRelocation(&storepkg.RelocationRow{
		TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: to,
		Region: t.Region, TenantID: t.TenantID, Reason: reason, Status: "moved",
	})
	addAudit("HA-3 归属迁移", t.Name, from+" → "+to+"（池 "+pool+"）", "成功")
	InvalidateReconCache()
	return &RelocateResult{TargetID: t.ID, TargetName: t.Name, FromAgent: from, ToAgent: to, Region: t.Region,
		Pool: pool, PoolHealthy: healthy, Status: "moved", Note: "已迁到池内健康采集机，config 轨接管", Relocatable: true}
}

// autoRelocate 自动调度（RELOCATE_AUTO=1 启用，默认关）：扫描可迁目标，
// 属主采集机失效超过阈值且池内存在健康承接端 → 迁移。每轮一次目标、带冷却、池内不足不迁。
func autoRelocate(store *AgentStore, catDB *storepkg.DB) {
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return
	}
	// 属主失效判定：属主 agent 不存在或 last_seen 超阈值
	ownerHealthy := map[string]bool{}
	for _, a := range agents {
		ownerHealthy[a.ID] = agentHealthy(a)
	}
	for _, t := range targets {
		if !isRelocatableTarget(t) {
			continue
		}
		if ownerHealthy[t.AgentID] {
			continue // 属主本身健康，不迁移
		}
		if cooldownActive(catDB, t.ID) {
			continue
		}
		// 池内健康采集机不足：不迁移（collector_pool_degraded 规则已告警），避免堆到唯一幸存者
		if poolInsufficient(agents, tenantOr(t.TenantID), targetPool(agents, t)) {
			continue
		}
		if pickCandidateFrom(agents, t, t.AgentID) == "" {
			continue
		}
		runRelocation(store, catDB, t.ID, "auto: owner offline")
		return // 每轮只迁一个目标，避免连锁风暴
	}
}

// startRelocateAuto HA-3 自动调度：RELOCATE_AUTO=1 时后台按间隔扫描失效属主目标并迁移。
// 默认关，避免未授权自动变更归属；启用后受冷却终态保护。
func startRelocateAuto(store *AgentStore, catDB *storepkg.DB) {
	if !relocateAutoOn {
		return
	}
	go func() {
		for {
			func() {
				defer func() { recover() }()
				autoRelocate(store, catDB)
			}()
			time.Sleep(time.Duration(relocateAutoInt) * time.Second)
		}
	}()
}

// registerRelocateRoutes HA-3 归属迁移评审/执行/历史证据 API。
func registerRelocateRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	mux.HandleFunc("/api/relocate/plan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		id, _ := strconv.ParseInt(r.URL.Query().Get("target_id"), 10, 64)
		if id == 0 {
			writeJSON(w, map[string]interface{}{"error": "target_id 必填"})
			return
		}
		writeJSON(w, planRelocation(catDB, id))
	})
	mux.HandleFunc("/api/relocate/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetID int64  `json:"target_id"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.TargetID == 0 {
			writeJSON(w, map[string]interface{}{"error": "target_id 必填"})
			return
		}
		writeJSON(w, runRelocation(store, catDB, q.TargetID, q.Reason))
	})
	mux.HandleFunc("/api/relocations", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("target_id"), 10, 64)
		if id == 0 {
			// 未指定 target_id → 聚合展示（从新到旧）
			rows, _ := catDB.ListRelocationsAll(50)
			writeJSON(w, map[string]interface{}{"relocations": rows})
			return
		}
		rows, _ := catDB.ListRelocationsByTarget(id, 100)
		writeJSON(w, map[string]interface{}{"relocations": rows})
	})
}
