package main

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// 平台自监控端点（可运维·可观测）。
// 架构文档 3.3「可运维·可观测」要求平台自身能被观测；前端效果设计第 6 章「自监控」
// 需要后端提供自身运行态。本端点只读、无副作用，不参与任何业务请求。

// SelfMonSnapshot 一次自监控采样的完整视图（JSON 序列化给前端，均来自运行态，无造假）
type SelfMonSnapshot struct {
	UptimeSec  int64     `json:"uptime_sec"`
	StartedAt  time.Time `json:"started_at"`
	ListenAddr string    `json:"listen_addr"`
	GoVersion  string    `json:"go_version"`
	Goroutines int       `json:"goroutines"`

	// 后端依赖状态（探测失败置 reachable=false，不阻断返回）
	Catalog struct {
		Reachable bool `json:"reachable"`
		Plugins   int  `json:"plugins"`
		Metrics   int  `json:"metrics"`
		AuditRows int  `json:"audit_rows"`
		Tenants   int  `json:"tenants"` // 多租户（架构D3）租户总数
	} `json:"catalog"`

	// Agent 注册规模（内存态）
	Agents struct {
		Total  int `json:"total"`
		Online int `json:"online"`
		Edge   int `json:"edge"`
		Proxy  int `json:"proxy"`
	} `json:"agents"`

	AuditQueue int               `json:"audit_queue"`
	Config     map[string]string `json:"config"`
}

var selfmonStart = time.Now()

// selfmonSnapshot 采集一次自监控快照。任何子项探测失败都不会让整个端点失败——
// 降级为 reachable=false / 0，保证 L0 自身在部分依赖故障时仍可被健康观测。
func selfmonSnapshot(store *AgentStore, catDB *storepkg.DB) *SelfMonSnapshot {
	snap := &SelfMonSnapshot{}
	snap.StartedAt = selfmonStart
	snap.UptimeSec = int64(time.Since(selfmonStart).Seconds())
	snap.ListenAddr = cfgListenAddr
	snap.GoVersion = runtime.Version()
	snap.Goroutines = runtime.NumGoroutine()

	if catDB != nil {
		if plugins, err := catDB.ListPlugins(); err == nil {
			snap.Catalog.Reachable = true
			snap.Catalog.Plugins = len(plugins)
		}
		if n, err := catDB.CountMetrics(); err == nil {
			snap.Catalog.Metrics = n
		}
		if rows, err := catDB.ListAudit(1); err == nil {
			snap.Catalog.AuditRows = len(rows)
		}
		if list, err := catDB.ListTenants(); err == nil {
			snap.Catalog.Tenants = len(list)
		}
	}

	ttl := int64(120)
	if store != nil {
		now := time.Now().Unix()
		all := store.List()
		snap.Agents.Total = len(all)
		for _, a := range all {
			if a == nil {
				continue
			}
			if isCollectorType(a.Type) {
				snap.Agents.Proxy++
			} else {
				snap.Agents.Edge++
			}
			if a.LastSeen > 0 && now-a.LastSeen <= ttl {
				snap.Agents.Online++
			}
		}
	}

	auditMu.Lock()
	snap.AuditQueue = len(auditLog)
	auditMu.Unlock()

	snap.Config = map[string]string{
		"listen_addr":         cfgListenAddr,
		"agent_http_port":     cfgAgentHTTPPort,
		"agent_container_pre": cfgAgentContainerPre,
		"sagent_version":      cfgSAVersion,
		"onboard_stall_sec":   strconv.Itoa(cfgOnboardStallSec),
		"tunnel_remote_port":  strconv.Itoa(cfgTunnelRemotePort),
		"vm_url":              vmBase(),
		"seed_demo_agents":    strconv.FormatBool(cfgSeedDemoAgents),
		"cors_origin":         cfgCORSOrigin,
	}
	return snap
}

func handleSelfMon(store *AgentStore, catDB *storepkg.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, selfmonSnapshot(store, catDB))
	}
}

// registerSelfMonRoutes 注册自监控路由（纯新增，不影响既有 route）
func registerSelfMonRoutes(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	mux.HandleFunc("/api/selfmon", handleSelfMon(store, catDB))
}
