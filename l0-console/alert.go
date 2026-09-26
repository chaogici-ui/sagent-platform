package main

// alert.go —— D6 告警通知（架构 D6：默认 L0 内部告警中心；外发 webhook 按 env 启用）。
// 对应差距 G5「心跳/状态可观测是查询式，非主动告警」。
// 原则：
//   · 纯只读信号源（复用 selfmon/agent/package-cache 的既有口径，不引入新探测链）
//   · 告警事件内存态暂存（仿 auditLog 环形），默认不外发；仅 ALERT_WEBHOOK_URL 非空时外发（best-effort，失败不阻断）
//   · 去重：同一 source 状态翻转才产生新事件（firing→resolved），不刷屏

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// AlertSeverity 告警级别。
type AlertSeverity string

const (
	SevWarning  AlertSeverity = "warning"
	SevCritical AlertSeverity = "critical"
)

// Alert 一条告警事件（firing 或 resolved）。
type Alert struct {
	Time     string        `json:"time"`
	Source   string        `json:"source"` // 规则标识（agent_stale / catalog_down / cache_unverified）
	Severity AlertSeverity `json:"severity"`
	State    string        `json:"state"` // firing / resolved
	Title    string        `json:"title"`
	Detail   string        `json:"detail"`
}

var (
	alerts     []Alert
	alertsMu   sync.Mutex
	alertState = map[string]bool{} // source -> 当前是否 firing
)

// cfgAlertWebhook Webhook 外发地址（ALERT_WEBHOOK_URL；空 = 纯内部告警中心，不外发）。
var cfgAlertWebhook = strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL"))

// alertTTL Agent 心跳新鲜度阈值（秒），与 selfmon 的 online TTL 对齐（120s）。
const alertTTL = 120

// ---- P2 VM-HA（决策点6）：存储水位入告警（只读探 VM 自述 /metrics，不新增探测链） ----
// vmStorageStat VM 存储水位快照。
type vmStorageStat struct {
	UsagePct float64 // 逻辑水位 % = data_size/(data_size+磁盘free)
	Size     uint64  // 已存数据字节（sum vm_data_size_bytes*）
	Total    uint64  // data+磁盘free（逻辑总量）
	ReadOnly int     // vm_storage_is_read_only（1=已触发只读水位，强 critical）
}

// cfgStorageWarnPct / cfgStorageCritPct 存储水位告警阈值（%）。
var (
	cfgStorageWarnPct = envFloatOr("ALERT_STORAGE_WARN_PCT", 85)
	cfgStorageCritPct = envFloatOr("ALERT_STORAGE_CRIT_PCT", 95)
)

// vmStorageProbe 探存储水位的函数（可注入，便于单测确定性；默认走配置 VM 基址）。
var vmStorageProbe = func() (*vmStorageStat, error) {
	return probeVMStorage(vmBase())
}

// probeVMStorage 拉取 VM /metrics（只读自述），解析存储水位。
func probeVMStorage(base string) (*vmStorageStat, error) {
	resp, err := http.Get(strings.TrimRight(base, "/") + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage /metrics http %d", resp.StatusCode)
	}
	return parseVMStorageUsage(resp.Body)
}

// parseVMStorageUsage 从 VM metrics 文本解析水位。口径（v1.9x 实测）：
//   - vm_data_size_bytes{type=...} 按 type 分桶 → 求和为数据量
//   - vm_free_disk_space_bytes{path="/storage"} → 磁盘空闲（实际是大盘，水位极低）
//   - vm_storage_is_read_only{path="/storage"} → 1 表示已触发只读保护（强 critical）
//
// 纯函数，便于单测暴露/注入。
func parseVMStorageUsage(r io.Reader) (*vmStorageStat, error) {
	var dataSize, diskFree uint64
	var readOnly int
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "vm_data_size_bytes{"):
			if v, ok := metricValue(line); ok {
				dataSize += v
			}
		case strings.HasPrefix(line, "vm_free_disk_space_bytes{"):
			if strings.Contains(line, `path="/storage"`) {
				if v, ok := metricValue(line); ok {
					diskFree = v
				}
			}
		case strings.HasPrefix(line, "vm_storage_is_read_only{"):
			if strings.Contains(line, `path="/storage"`) {
				if v, ok := metricValue(line); ok {
					readOnly = int(v)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	total := dataSize + diskFree
	if total == 0 {
		return nil, fmt.Errorf("无有效存储指标(data_size 与磁盘free 均 0)")
	}
	return &vmStorageStat{
		UsagePct: float64(dataSize) / float64(total) * 100,
		Size:     dataSize, Total: total, ReadOnly: readOnly,
	}, nil
}

// metricValue 从 metrics 行末尾取整型值（支持 `name{labels} 123` 形态）。
func metricValue(line string) (uint64, bool) {
	i := strings.LastIndexByte(line, ' ')
	if i < 0 || i == len(line)-1 {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(line[i+1:]), 10, 64)
	return v, err == nil
}

// envFloatOr 读浮点环境变量，缺失/非法用默认。
func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func recordAlert(source string, sev AlertSeverity, firing bool, title, detail string) {
	state := "firing"
	if !firing {
		state = "resolved"
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	alertsMu.Lock()
	alerts = append(alerts, Alert{
		Time: ts, Source: source, Severity: sev, State: state, Title: title, Detail: detail,
	})
	if len(alerts) > 1000 {
		alerts = alerts[len(alerts)-1000:]
	}
	alertsMu.Unlock()
	// 外发：仅配置了 webhook 才 POST（best-effort，失败只记日志，不阻断告警中心）
	if cfgAlertWebhook != "" {
		notifyWebhook(Alert{Time: ts, Source: source,
			Severity: sev, State: state, Title: title, Detail: detail})
	}
}

// setAlertState 登记 source 的 current firing 状态；状态翻转时产出一条事件。返回是否翻转。
func setAlertState(source string, sev AlertSeverity, firing bool, title, detail string) {
	alertsMu.Lock()
	prev := alertState[source]
	alertsMu.Unlock()
	if prev == firing {
		return // 无翻转，去重
	}
	mark := firing
	alertsMu.Lock()
	alertState[source] = mark
	alertsMu.Unlock()
	recordAlert(source, sev, firing, title, detail)
}

// notifyWebhook 外发告警到配置的 webhook URL（结构 JSON），非 2xx 记日志。空 URL 不再调用。
func notifyWebhook(a Alert) {
	body, err := json.Marshal(a)
	if err != nil {
		log.Printf("alert webhook marshal: %v", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, cfgAlertWebhook, bytes.NewReader(body))
	if err != nil {
		log.Printf("alert webhook build: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		log.Printf("alert webhook send: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("alert webhook non-2xx: %d", resp.StatusCode)
	}
}

// scanAlerts 跑一轮告警规则：仅信号翻转时产出事件。幂等、只读源。
func scanAlerts(store *AgentStore, catDB *storepkg.DB) {
	// ① Agent 心跳陈旧（heartbeat 型，即真实接入 SAgent）
	stale := []string{}
	now := time.Now().Unix()
	if store != nil {
		for _, a := range store.List() {
			if a == nil {
				continue
			}
			// 仅「真实接入 SAgent」的 heartbeat 型参与心跳告警；docker/种子里仅作容器状态
			// 探测的非心跳条目不误报（其 last_seen 不刷新，纳入会在每次重启后恒 false-positive）
			if a.Source != "heartbeat" {
				continue
			}
			if a.LastSeen > 0 && now-a.LastSeen > alertTTL {
				stale = append(stale, a.Name+"("+a.Host+")")
			} else if a.LastSeen <= 0 {
				stale = append(stale, a.Name+"(从未心跳)")
			}
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		setAlertState("agent_stale", SevWarning, true, fmt.Sprint(len(stale))+" 个 Agent 心跳陈旧",
			"超过 "+fmt.Sprint(alertTTL)+"s 未心跳："+strings.Join(stale, ", "))
	} else {
		setAlertState("agent_stale", SevWarning, false, "Agent 心跳全部正常", "全部 heartbeat Agent 在新鲜窗口内")
	}

	// ② L0 目录/依赖不可达（catalog 探测失败）
	if catDB != nil {
		if _, err := catDB.ListPlugins(); err != nil {
			setAlertState("catalog_down", SevCritical, true, "L0 目录库不可达",
				"ListPlugins 失败："+err.Error())
		} else {
			setAlertState("catalog_down", SevCritical, false, "L0 目录库正常", "catalog 可达")
		}
	}

	// ③ 包缓存被污染（存在缓存但 sha 与 L0 权威不一致 → 就近取包准入被破坏，红线告警）
	badCache := []string{}
	for _, e := range describePackageCache() {
		if e.Cached && !e.Verified {
			badCache = append(badCache, e.Tag)
		}
	}
	if len(badCache) > 0 {
		setAlertState("cache_unverified", SevCritical, true, fmt.Sprint(len(badCache))+" 个版本缓存未校验通过",
			"就近取包准入被破坏："+strings.Join(badCache, ", ")+"（L0 权威 sha 与缓存实测不符）")
	} else {
		setAlertState("cache_unverified", SevCritical, false, "包缓存校验通过", "所有已缓存版本 sha 与 L0 权威一致")
	}

	// ④ 存储水位（P2 VM-HA 决策点6）：只读探配置 VM 存储自述水位，超阈值告警；存储不可达升 critical。
	runStorageWaterRule()

	// ⑤ 采集机池降级（R3）：池内健康采集机 <2 且池内仍有待承接的远端采集目标 → 漂移无可靠承接方。
	runCollectorPoolRule(catDB)
}

// runCollectorPoolRule 采集机池降级规则（R3）：池内健康采集机 < 2 时漂移无可靠承接方，
// 直接告警（与「池内不足不迁移」配套）。只读扫描既有 agents/targets，不新增探测链。
// 空池不评估：只有"确有远端采集目标落在该池"时才告警，避免噪音。
func runCollectorPoolRule(catDB *storepkg.DB) {
	if catDB == nil {
		return
	}
	agents, err := catDB.ListAgentRows()
	if err != nil {
		setAlertState("collector_pool_degraded", SevWarning, false, "采集机池健康", "agent 台账不可读，暂不评估池健康")
		return
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return
	}
	type poolKey struct{ tenant, pool string }
	pools := map[poolKey]bool{}
	for _, t := range targets {
		if !isRelocatableTarget(t) {
			continue
		}
		p := targetPool(agents, t)
		if p == "" {
			continue // 池未登记，无从判定池健康
		}
		pools[poolKey{tenantOr(t.TenantID), p}] = true
	}
	degraded := []string{}
	for k := range pools {
		if poolHealthyCollectors(agents, k.tenant, k.pool) < 2 {
			degraded = append(degraded, k.pool)
		}
	}
	if len(degraded) > 0 {
		sort.Strings(degraded)
		setAlertState("collector_pool_degraded", SevWarning, true,
			fmt.Sprint(len(degraded))+" 个采集机池健康采集机不足",
			"池内健康采集机 <2，漂移无可靠承接方（不迁移）："+strings.Join(degraded, ", "))
	} else {
		setAlertState("collector_pool_degraded", SevWarning, false,
			"采集机池健康", "承载远端采集的池内健康采集机均 ≥2")
	}
}

// runStorageWaterRule 存储水位规则（可独立调用便于单测注入桩；探 URL 为 vmBase()）。
func runStorageWaterRule() {
	// 探 URL 为 vmBase()（容器部署注入 VM_URL，本机默认 localhost:8428）；配置空根即视为不适用不告警。
	if base := strings.TrimSpace(vmBase()); base != "" {
		st, err := vmStorageProbe()
		if err != nil {
			setAlertState("storage_down", SevCritical, true, "L0 指标存储不可达",
				"fetch "+base+" /metrics: "+err.Error())
			// 存储都不可达时水位无从谈起，resolved 该源，避免双 firing 混淆归因
			setAlertState("storage_water", SevCritical, false, "存储水位正常", "存储不可达，先解决连通性再评估水位")
		} else {
			setAlertState("storage_down", SevCritical, false, "L0 指标存储正常", "storage /metrics 可达")
			pct := st.UsagePct
			switch {
			case st.ReadOnly != 0:
				setAlertState("storage_water", SevCritical, true,
					"指标存储已触发只读保护（磁盘满）",
					fmt.Sprintf("vm_storage_is_read_only=1，数据量≈%.1fGB / 逻辑总量≈%.1fGB", float64(st.Size)/1e9, float64(st.Total)/1e9))
			case pct >= cfgStorageCritPct:
				setAlertState("storage_water", SevCritical, true,
					fmt.Sprintf("指标存储高水位 %d%%(≥%d%%)", int(pct), int(cfgStorageCritPct)),
					fmt.Sprintf("vm_data_size_bytes≈%.1fGB / 总量≈%.1fGB", float64(st.Size)/1e9, float64(st.Total)/1e9))
			case pct >= cfgStorageWarnPct:
				setAlertState("storage_water", SevWarning, true,
					fmt.Sprintf("指标存储水位偏高 %d%%(≥%d%%)", int(pct), int(cfgStorageWarnPct)),
					fmt.Sprintf("vm_data_size_bytes≈%.1fGB / 总量≈%.1fGB", float64(st.Size)/1e9, float64(st.Total)/1e9))
			default:
				setAlertState("storage_water", SevWarning, false,
					"指标存储水位正常", fmt.Sprintf("当前水位 %d%%", int(pct)))
			}
		}
	}
}

// startAlertScanner 后台告警扫描：每 30s 跑一轮。
func startAlertScanner(store *AgentStore, catDB *storepkg.DB) {
	go func() {
		// 启动后立即跑一轮，接着按周期
		scanAlerts(store, catDB)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			scanAlerts(store, catDB)
		}
	}()
}

// activeAlertCount 当前处于 firing 的规则数（状态表口径），供 /api/alerts 与 /metrics 共用。
// 纯只读、加锁取快照。
func activeAlertCount() int {
	alertsMu.Lock()
	defer alertsMu.Unlock()
	n := 0
	for _, f := range alertState {
		if f {
			n++
		}
	}
	return n
}

// handleAlerts 告警中心只读列表（最新在前）。`firing` 表示当前仍处于 firing 的规则数
// （取自状态表，不受历史已 resolved 事件检索影响）；`alerts` 为完整事件日志（firing+resolved）。
func handleAlerts(w http.ResponseWriter, r *http.Request) {
	alertsMu.Lock()
	rev := make([]Alert, 0, len(alerts))
	for i := len(alerts) - 1; i >= 0; i-- {
		rev = append(rev, alerts[i])
	}
	alertsMu.Unlock()
	writeJSON(w, map[string]any{
		"ok": true, "firing": activeAlertCount(), "alerts": rev,
		"webhook_gated": cfgAlertWebhook == "",
	})
}

// registerAlertRoutes 告警中心路由。
func registerAlertRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	mux.HandleFunc("/api/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		handleAlerts(w, r)
	})
	startAlertScanner(store, catDB)
}
