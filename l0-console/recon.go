package main

// M1-⑥ 三方对账引擎
// 目录声明应采（targets × plugin 指标）× VM 实际在报（24h 指标名 / 目标探测）× Agent 心跳
// 输出三类异常：断采目标（声明了但 VM 无数据）、野指标（VM 有但目录未注册）、僵尸 Agent（心跳超时）

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	reconMu      sync.Mutex
	reconCache   map[string]interface{}
	reconCacheAt time.Time
)

// 合法 Prom 指标名（用于从 catalog 提取可探测的英文 key）
var metricKeyRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func registerReconRoute(mux *http.ServeMux) {
	mux.HandleFunc("/api/recon", handleRecon)
}

// InvalidateReconCache 目标/Agent 增删改后调用，强制下轮对账重算
func InvalidateReconCache() {
	reconMu.Lock()
	reconCache = nil
	reconMu.Unlock()
}

// handleRecon GET /api/recon 三方对账结果（60s 缓存）
func handleRecon(w http.ResponseWriter, r *http.Request) {
	reconMu.Lock()
	if reconCache != nil && time.Since(reconCacheAt) < 60*time.Second {
		out := reconCache
		reconMu.Unlock()
		writeJSON(w, out)
		return
	}
	reconMu.Unlock()

	out := runRecon()
	reconMu.Lock()
	reconCache = out
	reconCacheAt = time.Now()
	reconMu.Unlock()
	writeJSON(w, out)
}

// fetchVMNames 拉取 VM 近 24h 出现过的指标名集合
func fetchVMNames() ([]string, error) {
	end := time.Now().Unix()
	u := fmt.Sprintf("%s/api/v1/label/__name__/values?start=%d&end=%d", vmBaseURL(), end-86400, end)
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return d.Data, nil
}

// vmQueryInstance 查询某指标在目标主机（instance 含 host）近 10min 是否有数据
func vmQueryInstance(key, host string) bool {
	host = strings.ReplaceAll(host, ".", "\\.")
	query := fmt.Sprintf(`count_over_time(%s{instance=~".*%s.*"}[10m])`, key, host)
	u := fmt.Sprintf("%s/api/v1/query?query=%s", vmBaseURL(), url.QueryEscape(query))
	resp, err := http.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var d struct {
		Status string `json:"status"`
		Data   struct {
			Result []interface{} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return false
	}
	return d.Status == "success" && len(d.Data.Result) > 0
}

// runRecon 执行一轮对账
func runRecon() map[string]interface{} {
	now := time.Now().Unix()
	out := map[string]interface{}{
		"generated_at": time.Now().Format("2006-01-02 15:04:05"),
	}

	// ---- 1) 僵尸 Agent：心跳型且 90s 无心跳（docker 演示容器不参与心跳，不参与此判定）----
	zombies := []map[string]interface{}{}
	agRows, _ := catalogDB.ListAgentRows()
	for _, a := range agRows {
		if a.Source == "heartbeat" && a.LastSeen > 0 && now-a.LastSeen > 90 {
			zombies = append(zombies, map[string]interface{}{
				"id": a.ID, "last_seen_age": now - a.LastSeen,
			})
		}
	}
	out["zombie"] = zombies

	// ---- 2) 断采目标：目录声明的哨兵指标在 VM 近 10min 无数据 ----
	targets, _ := catalogDB.ListTargets()
	stale := []map[string]interface{}{}
	okTargets := 0
	for _, t := range targets {
		if t.Plugin == "" || t.Address == "" {
			continue
		}
		pid := catalogDB.ResolvePluginID(t.Plugin)
		if pid == 0 {
			// 探针名带后缀时再试去掉后缀
			pid = catalogDB.ResolvePluginID(strings.SplitN(t.Plugin, "_", 2)[0])
		}
		if pid == 0 {
			continue
		}
		keys, _ := catalogDB.ListPluginMetricExprs(pid)
		// 只保留合法指标名，哨兵优先级：_up 结尾 > 短名（避免 uptime 误中 "up"）
		var valid []string
		for _, k := range keys {
			if metricKeyRe.MatchString(k) {
				valid = append(valid, k)
			}
		}
		sort.Slice(valid, func(i, j int) bool {
			ui, uj := strings.HasSuffix(valid[i], "_up"), strings.HasSuffix(valid[j], "_up")
			if ui != uj {
				return ui
			}
			return len(valid[i]) < len(valid[j])
		})
		if len(valid) > 6 {
			valid = valid[:6]
		}
		if len(valid) == 0 {
			continue
		}
		host := t.Address
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		reporting := false
		for _, k := range valid {
			if vmQueryInstance(k, host) {
				reporting = true
				break
			}
		}
		if reporting {
			okTargets++
		} else {
			stale = append(stale, map[string]interface{}{
				"id": t.ID, "name": t.Name, "type": t.Type, "address": t.Address,
				"plugin": t.Plugin, "agent_id": t.AgentID, "probe_keys": valid,
			})
		}
	}
	out["stale"] = stale

	// ---- 3) 野指标：VM 在报但「应报口径」未覆盖 ----
	// D8：应报口径 = 目录注册集 ∪ 各 Agent 生效配置展开集（目录被治理清理后，Agent 按配置仍在采的指标不算野）
	vmNames, err := fetchVMNames()
	wild := []string{}
	wildTotal := 0
	known := map[string]bool{}
	expandedAgents := 0
	if err == nil {
		expandCatalogKnown(known)
		// 各 Agent 生效配置展开（有版本化配置的才参与）
		for _, a := range agRows {
			if ver, content, _ := catalogDB.GetAgentConfig(a.ID); ver > 0 && content != "" {
				n := expandAgentConfigKnown(content, known)
				if n > 0 {
					expandedAgents++
				}
			}
		}
		for _, n := range vmNames {
			if !known[n] {
				wildTotal++
				if len(wild) < 50 {
					wild = append(wild, n)
				}
			}
		}
	}
	out["wild"] = wild
	out["wild_total"] = wildTotal

	out["summary"] = map[string]interface{}{
		"agents":          len(agRows),
		"zombie":          len(zombies),
		"targets":         len(targets),
		"checked":         okTargets + len(stale),
		"stale":           len(stale),
		"wild":            wildTotal,
		"vm_reachable":    err == nil,
		"known_metrics":   len(known),
		"config_expanded": expandedAgents,
	}
	return out
}

// expandCatalogKnown 目录注册口径：全部指标 name + expression 均视为已知
func expandCatalogKnown(known map[string]bool) {
	all, _ := catalogDB.ListAllMetrics("", "")
	for _, m := range all {
		if m.Name != "" {
			known[m.Name] = true
		}
		if m.Expression != "" {
			known[m.Expression] = true
		}
	}
}

// agentConfigDoc 结构化期望配置（D3/D8）：targets 段 + host_metrics 段
type agentConfigDoc struct {
	Targets []struct {
		Plugin string `json:"plugin"`
		Target string `json:"target"`
		Type   string `json:"type"`
		Address string `json:"address"`
	} `json:"targets"`
	HostMetrics *struct {
		Enabled        *bool           `json:"enabled"`
		Interval       string          `json:"interval"`
		Groups         map[string]bool `json:"groups"`
		ExcludeMetrics []string        `json:"exclude_metrics"`
	} `json:"host_metrics"`
}

// expandAgentConfigKnown 把单个 Agent 的生效配置展开为应报指标集，并入 known。
// 返回展开的指标数。兼容旧格式（纯 targets 数组 JSON）。
func expandAgentConfigKnown(content string, known map[string]bool) int {
	addPlugin := func(pluginName string) {
		if pluginName == "" || pluginName == "host_metrics" {
			return // host_metrics 走分组展开
		}
		if pid := catalogDB.ResolvePluginID(pluginName); pid > 0 {
			keys, _ := catalogDB.ListPluginMetricExprs(pid)
			for _, k := range keys {
				if metricKeyRe.MatchString(k) {
					known[k] = true
				}
			}
		}
	}

	// 旧格式：纯数组 [{plugin,target,...}]
	var legacy []map[string]interface{}
	if err := json.Unmarshal([]byte(content), &legacy); err == nil && legacy != nil {
		n := 0
		for _, t := range legacy {
			if p, _ := t["plugin"].(string); p != "" {
				addPlugin(p)
				n++
			}
		}
		return n
	}

	// 新格式：结构化 doc
	var doc agentConfigDoc
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return 0
	}
	n := 0
	for _, t := range doc.Targets {
		addPlugin(t.Plugin)
		n++
	}
	if hm := doc.HostMetrics; hm != nil && (hm.Enabled == nil || *hm.Enabled) {
		// 分组开关：未声明的组默认开启（与 Agent 注册表约定一致）
		excl := map[string]bool{}
		for _, e := range hm.ExcludeMetrics {
			excl[e] = true
		}
		ngs, err := catalogDB.ListMetricNameGrpByPluginName("host_metrics")
		if err == nil {
			for _, ng := range ngs {
				if ng.Name == "" || excl[ng.Name] {
					continue
				}
				if on, declared := hm.Groups[ng.Grp]; declared && !on && ng.Grp != "" {
					continue // 显式关闭的分组
				}
				if metricKeyRe.MatchString(ng.Name) {
					known[ng.Name] = true
					n++
				}
			}
		}
	}
	return n
}
