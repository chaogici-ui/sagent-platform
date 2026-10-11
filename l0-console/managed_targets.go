package main

// ============================================================
// SPEC-Agent托管目标与预热接管 R0-2/R0-3/R0-4（2026-09-27 立项）
//
//   R0-3 能力协商：L0 按 Agent 能力位/版本 gate targets 段下发。
//     老版本 Agent（<0.6.0 且无 capabilities 声明）收到的配置不含 targets，
//     回落静态配置——防止 targets/desired_state 新字段被老 Agent 的
//     DisallowUnknownFields 整份拒收（配置打挂）。
//
//   R0-4 standby 预置下发：池内健康非属主采集机对 mysql_probe 目标获得
//     desired_state=standby 预置（配置副本，非进程副本）。派生口径 =
//     candidateList（与漂移同一把尺子：同租户+同池+健康+规则候选集），
//     单机预置数量受 SA_STANDBY_PRESET_MAX 上限约束。
//     预置保鲜：后台按 relocateAutoInt 周期比对派生指纹，变化才 syncAgentConfig
//     （成员上下线/候选集调整自动跟进 = R6-2 预置失效回收的自然语义）。
// ============================================================

import (
	"strconv"
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// managedTargetsMinVersion 无能力位声明时的版本兜底线（0.6.0 = targets+desired_state 全支持）
const managedTargetsMinVersion = "0.6.0"

// parseVersionTriplet 解析 "0.6.0" / "v0.6.0" / "0.6.0-rc1" 形态为主版本三元组；
// 解析失败全零返回（调用方按"低于兜底线"处理，宁可不给 targets 也不打挂老 Agent）。
func parseVersionTriplet(v string) (int, int, int) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := [3]int{}
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return 0, 0, 0
		}
		out[i] = n
	}
	return out[0], out[1], out[2]
}

// versionAtLeast 版本 >= 兜底线判定（R0-3 版本兜底口径）
func versionAtLeast(version, min string) bool {
	a1, a2, a3 := parseVersionTriplet(version)
	b1, b2, b3 := parseVersionTriplet(min)
	if a1 == 0 && a2 == 0 && a3 == 0 {
		return false // 版本不可考 = 不放行（fail-closed：宁可少下发，不可打挂老 Agent）
	}
	if a1 != b1 {
		return a1 > b1
	}
	if a2 != b2 {
		return a2 > b2
	}
	return a3 >= b3
}

// agentSupportsManagedTargets R0-3 能力判定：
// ① 显式能力位（Agent 注册/心跳声明的 capabilities，权威口径）；
// ② 版本兜底（老客户端不带能力位，>= 0.6.0 视为支持）。
// 两者皆不满足 → 不下发 targets（fail-closed）。
func agentSupportsManagedTargets(version string, stats map[string]interface{}) bool {
	if stats != nil {
		switch caps := stats["capabilities"].(type) {
		case []interface{}:
			for _, c := range caps {
				if s, ok := c.(string); ok && s == "managed_targets" {
					return true
				}
			}
		case []string:
			for _, s := range caps {
				if s == "managed_targets" {
					return true
				}
			}
		}
	}
	return versionAtLeast(version, managedTargetsMinVersion)
}

// agentManagedTargetsCapable 按 id 判定 Agent 能力（DB 台账行优先，内存注册表兜底）。
func agentManagedTargetsCapable(store *AgentStore, catDB *storepkg.DB, agentID string) bool {
	if agentID == "" {
		return false
	}
	if row, err := catDB.GetAgentRow(agentID); err == nil && row != nil {
		return agentSupportsManagedTargets(row.Version, row.Stats)
	}
	if a := store.Get(agentID); a != nil {
		return agentSupportsManagedTargets(a.Version, a.Stats)
	}
	return false
}

// deriveStandbyPresets R0-4 预置派生（与漂移同一把尺子）：
// 返回目标 agentID 应预置 standby 的 mysql_probe 目标清单 = 该目标是别人属主且
// agentID ∈ candidateList（同租户+同池+健康+规则候选集）。按 targets.id 稳定序，
// 超出 max 截断（预置面有边界）。
func deriveStandbyPresets(agents []*storepkg.AgentRow, targets []*storepkg.TargetRow, agentID string, max int) []*storepkg.TargetRow {
	if agentID == "" || max <= 0 {
		return nil
	}
	out := []*storepkg.TargetRow{}
	for _, t := range targets {
		if len(out) >= max {
			break
		}
		if t.Plugin != "mysql_probe" || t.AgentID == "" || t.AgentID == agentID {
			continue
		}
		for _, c := range candidateList(agents, t, t.AgentID) {
			if c == agentID {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// ---------- R0-4 预置保鲜：派生指纹比对，变化才下发 ----------

var standbyPresetSync = struct {
	sync.Mutex
	synced map[string]string // agentID → 最近一次下发成功的预置指纹（目标 id 逗号串）
}{synced: map[string]string{}}

// presetFingerprint 预置集合指纹（确定性：按目标 id 升序逗号串）
func presetFingerprint(presets []*storepkg.TargetRow) string {
	ids := make([]string, 0, len(presets))
	for _, t := range presets {
		ids = append(ids, strconv.FormatInt(t.ID, 10))
	}
	// targets 按 id 序遍历而来，天然有序；兜底再排一次防调用方乱序
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return strings.Join(ids, ",")
}

// reconcileStandbyPresets R0-4 保鲜一轮：重算每台有能力采集机的预置指纹，
// 与上次下发成功指纹不一致才 syncAgentConfig（成员健康变化/候选集调整自动跟进；
// 预置失效回收 = 派生语义的自然结果，无需独立回收任务）。
// 独立于 RELOCATE_AUTO 开关常驻（预置是预热接管的数据面基础，与自动漂移开关正交）。
func reconcileStandbyPresets(store *AgentStore, catDB *storepkg.DB) {
	refreshSimDownSet(catDB) // 演练下线即时生效（agentHealthy 同源）
	agents, err := catDB.ListAgentRows()
	if err != nil {
		return
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return
	}
	desired := map[string]string{}
	for _, a := range agents {
		if a.ID == "" || a.Source != "heartbeat" || !isCollectorAgent(a) {
			continue
		}
		if !agentSupportsManagedTargets(a.Version, a.Stats) {
			continue
		}
		desired[a.ID] = presetFingerprint(deriveStandbyPresets(agents, targets, a.ID, cfgStandbyPresetMax))
	}
	standbyPresetSync.Lock()
	defer standbyPresetSync.Unlock()
	for agentID, fp := range desired {
		if standbyPresetSync.synced[agentID] == fp {
			continue
		}
		syncAgentConfig(store, catDB, agentID)
		standbyPresetSync.synced[agentID] = fp
	}
	// 已下线 agent 的指纹清理（重装重接入时从零同步）
	for agentID := range standbyPresetSync.synced {
		if _, ok := desired[agentID]; !ok {
			delete(standbyPresetSync.synced, agentID)
		}
	}
}

// startStandbyPresetReconciler R0-4 保鲜循环（relocateAutoInt 同周期，默认 10s）
func startStandbyPresetReconciler(store *AgentStore, catDB *storepkg.DB) {
	go func() {
		for {
			func() {
				defer func() { recover() }()
				reconcileStandbyPresets(store, catDB)
			}()
			time.Sleep(time.Duration(relocateAutoInt) * time.Second)
		}
	}()
}
