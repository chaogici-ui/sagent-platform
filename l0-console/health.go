package main

// 平台健康自检汇总端点（可运维·可观测）。
// 架构文档 3.3 可运维：「SAgent 提供 /health（已有），L0 采集健康页提供 /api/health 汇总（规划新增）」。
// 本端点把 L0 控制面、目录存储、Agent 心跳、VM 数据链、三方对账聚合为一个整体健康视图，
// 供前端采集健康页/自监控消费。纯只读，无副作用，不下发任何指令。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// HealthSnapshot 一次整体健康汇总
type HealthSnapshot struct {
	Status    string     `json:"status"`              // ok | degraded | down
	Summary   HealthSum  `json:"summary"`             // 关键计数
	Generated time.Time  `json:"generated_at"`        // 生成时间（可观测：反映新鲜度）
	Components []HealthComponent `json:"components"`  // 分层组件明细，逐项可下钻
}

// HealthSum 关键健康计数（缩略面，多数派定 status）
type HealthSum struct {
	Agents   int `json:"agents"`   // 内存台账 Agent 数
	Online   int `json:"online"`   // 心跳新鲜（120s 内）Agent 数
	Zombies  int `json:"zombies"`  // 心跳型但超时（对账僵尸）
	Targets  int `json:"targets"`  // 目录采集目标数
	Stale    int `json:"stale"`    // 断采目标数
	Wild     int `json:"wild"`     // 野指标数
}

// HealthComponent 一个健康组件（L0 自身 / 存储 / Agent / 数据链 / 对账）
type HealthComponent struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Status    string `json:"status"` // ok | degraded | fail
	Detail    string `json:"detail"` // 人类可读说明
}

// 近因探活阈值（与 recon/selfmon 保持一致：120s 心跳新鲜）
const healthBeatTTL = int64(120)

// healthSnapshot 聚合一次整体健康视图。任何子项探活失败都不阻断——降级为该组件 fail，
// 由多数派汇总出 status，保证故障时 L0 自身仍可被健康观测（可观测性自身不因故障失灵）。
func healthSnapshot(store *AgentStore, catDB *storepkg.DB) *HealthSnapshot {
	now := time.Now().Unix()
	h := &HealthSnapshot{
		Generated: time.Now(),
		Status:    "ok",
	}

	// ---- 1) L0 控制面自身（进程存在即 ok）----
	h.Components = append(h.Components, HealthComponent{
		Key: "l0", Name: "L0 控制面",
		Status: "ok", Detail: "在线 · " + cfgListenAddr,
	})

	// ---- 2) 目录存储（catalog 可达）----
	storeStatus, storeDetail := "ok", "可达"
	if catDB == nil {
		storeStatus, storeDetail = "fail", "catalog DB 未初始化"
	} else if _, err := catDB.ListPlugins(); err != nil {
		storeStatus, storeDetail = "fail", "catalog 查询失败: "+err.Error()
	}
	h.Components = append(h.Components, HealthComponent{
		Key: "catalog", Name: "目录存储", Status: storeStatus, Detail: storeDetail,
	})

	// ---- 3) Agent 心跳面 ----
	all := []*Agent{}
	online := 0
	if store != nil {
		all = store.List()
		for _, a := range all {
			if a != nil && a.LastSeen > 0 && now-a.LastSeen <= healthBeatTTL {
				online++
			}
		}
	}
	beatStatus, beatDetail := "ok", "心跳正常"
	if len(all) > 0 && online == 0 {
		beatStatus, beatDetail = "fail", "全部 Agent 失去心跳"
	} else if len(all) > 0 && online < len(all) {
		beatStatus, beatDetail = "degraded", "部分 Agent 心跳超时"
	}
	h.Components = append(h.Components, HealthComponent{
		Key: "agents", Name: "Agent 心跳", Status: beatStatus,
		Detail: beatDetail + fmtCount(online, len(all)),
	})

	// ---- 4) VM 数据链（vmstorage 可达）/ 对账摘要 ----
	vmStatus, vmDetail := "ok", "可达"
	if _, err := fetchVMOneName(); err != nil {
		vmStatus, vmDetail = "fail", "VM 查询失败: "+err.Error()
	}
	h.Components = append(h.Components, HealthComponent{
		Key: "vmpath", Name: "VM 数据链", Status: vmStatus, Detail: vmDetail + " · " + vmBaseURL(),
	})

	// ---- 5) 三方对账摘要（断采/野指标反映数据面异常面）----
	recon := runRecon()
	sum := HealthSum{Agents: len(all), Online: online}
	if s, ok := recon["summary"].(map[string]interface{}); ok {
		sum.Targets = intOf(s["targets"])
		sum.Stale = intOf(s["stale"])
		sum.Wild = intOf(s["wild"])
		sum.Zombies = intOf(s["zombie"])
	}
	h.Summary = sum

	var failComp, degradedComp int
	for _, c := range h.Components {
		switch c.Status {
		case "fail":
			failComp++
		case "degraded":
			degradedComp++
		}
	}
	switch {
	case failComp > 0:
		h.Status = "down"
	case degradedComp > 0 || sum.Stale > 0 || sum.Wild > 0 || sum.Zombies > 0:
		h.Status = "degraded"
	}
	return h
}

// fetchVMOneName 拉取 VM 任一个指标名，用于判断 VM 链路可达（比全量更轻）
func fetchVMOneName() (string, error) {
	end := time.Now().Unix()
	u := vmBaseURL() + "/api/v1/label/__name__/values?start=0&end=" + itoa64(end)
	resp, err := http.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var d struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", err
	}
	if len(d.Data) > 0 {
		return d.Data[0], nil
	}
	return "", nil
}

// ---- 小工具（避免在本文件引入额外依赖）----
func fmtCount(online, total int) string {
	return "(" + strconv.Itoa(online) + "/" + strconv.Itoa(total) + ") "
}

func intOf(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func registerHealthRoute(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, healthSnapshot(store, catDB))
	})
}