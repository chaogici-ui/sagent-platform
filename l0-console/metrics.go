package main

// metrics.go — 平台自身 Prometheus 指标出口（架构 3.3「可监控」+ 前端效果设计第 6 章自监控）。
//
// selfmon 端点给前端 JSON（人类可读）；本端点给 VictoriaMetrics（L0 侧 vmstorage/vmagent）
// 提供 Prometheus text 格式指标，使 L0 自身也被纳入平台统一监控抓取，实现"平台监控自己"。
// 纯只读、无副作用，不参与任何业务请求。
//
// 指标命名遵循 Prometheus 惯例（小写下划线），统一前缀 sagent_l0_。

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// registerMetricsRoute 注册平台自身 /metrics。暴露面与 /api/selfmon 相同（平台内部网络可抓）。
func registerMetricsRoute(mux *http.ServeMux, store *AgentStore, catDB *storepkg.DB) {
	mux.HandleFunc("/metrics", handleMetrics(store, catDB))
}

func handleMetrics(store *AgentStore, catDB *storepkg.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		snap := selfmonSnapshot(store, catDB)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_ = writeMText(w, snap, catDB)
	}
}

// writeMText 渲染 Prometheus text 格式指标。
func writeMText(w io.Writer, snap *SelfMonSnapshot, catDB *storepkg.DB) error {
	var b strings.Builder

	// 基础/进程
	b.WriteString("# HELP sagent_l0_up 1 if l0-console is running\n")
	b.WriteString("sagent_l0_up 1\n")
	b.WriteString("# HELP sagent_l0_start_time_seconds Unix start time\n")
	fmt.Fprintf(&b, "sagent_l0_start_time_seconds %d\n", snap.StartedAt.Unix())
	b.WriteString("# HELP sagent_l0_uptime_seconds Seconds since start\n")
	fmt.Fprintf(&b, "sagent_l0_uptime_seconds %d\n", snap.UptimeSec)
	b.WriteString("# HELP sagent_l0_goroutines Number of goroutines\n")
	fmt.Fprintf(&b, "sagent_l0_goroutines %d\n", snap.Goroutines)
	b.WriteString("# HELP sagent_l0_go_info A constant '1' gauge with Go version\n")
	fmt.Fprintf(&b, "sagent_l0_go_info{version=%q} 1\n", snap.GoVersion)

	// 后端依赖（可达性 0/1 + 规模量）
	b.WriteString("# HELP sagent_l0_catalog_reachable Catalog db reachable (1 yes)\n")
	fmt.Fprintf(&b, "sagent_l0_catalog_reachable %d\n", bool01(snap.Catalog.Reachable))
	b.WriteString("# HELP sagent_l0_catalog_plugins Number of catalog plugins\n")
	fmt.Fprintf(&b, "sagent_l0_catalog_plugins %d\n", snap.Catalog.Plugins)
	b.WriteString("# HELP sagent_l0_catalog_metrics Number of catalog metrics\n")
	fmt.Fprintf(&b, "sagent_l0_catalog_metrics %d\n", snap.Catalog.Metrics)
	b.WriteString("# HELP sagent_l0_catalog_audit_rows Number of persisted audit rows\n")
	fmt.Fprintf(&b, "sagent_l0_catalog_audit_rows %d\n", snap.Catalog.AuditRows)
	b.WriteString("# HELP sagent_l0_tenants_total Number of tenants (D3 skeleton)\n")
	fmt.Fprintf(&b, "sagent_l0_tenants_total %d\n", snap.Catalog.Tenants)

	// Agent 规模
	b.WriteString("# HELP sagent_l0_agents_total Total registered agents\n")
	fmt.Fprintf(&b, "sagent_l0_agents_total %d\n", snap.Agents.Total)
	b.WriteString("# HELP sagent_l0_agents_online Online agents (heartbeat within TTL)\n")
	fmt.Fprintf(&b, "sagent_l0_agents_online %d\n", snap.Agents.Online)
	b.WriteString("# HELP sagent_l0_agents_edge Edge agents\n")
	fmt.Fprintf(&b, "sagent_l0_agents_edge %d\n", snap.Agents.Edge)
	b.WriteString("# HELP sagent_l0_agents_proxy Proxy agents\n")
	fmt.Fprintf(&b, "sagent_l0_agents_proxy %d\n", snap.Agents.Proxy)

	// 内存审计队列
	b.WriteString("# HELP sagent_l0_audit_queue In-memory audit entries pending\n")
	fmt.Fprintf(&b, "sagent_l0_audit_queue %d\n", snap.AuditQueue)

	// 当前处于 firing 的告警规则数（D6 告警中心 → 可观测/可监控闭环：Grafana/vmalert 可对平台自身告警状态触发规则）
	b.WriteString("# HELP sagent_l0_alerts_active Currently firing alert rules (D6)\n")
	fmt.Fprintf(&b, "sagent_l0_alerts_active %d\n", activeAlertCount())

	// 处理中流水线（非终态；读取失败记 -1 表明"未知"而非假 0）
	b.WriteString("# HELP sagent_l0_onboard_pending_flows Flows in non-terminal state\n")
	fmt.Fprintf(&b, "sagent_l0_onboard_pending_flows %d\n", pendingFlowsCount(catDB))

	// 稳定配置指纹：当前 SAgent 版本（便于告警对账版本对齐）
	if v := snap.Config["sagent_version"]; v != "" {
		b.WriteString("# HELP sagent_l0_config_sagent_version SAgent version in use\n")
		fmt.Fprintf(&b, "sagent_l0_config_sagent_version{%q} 1\n", v)
	}

	// L1 任务队列可观测（PLAN 3.3 L0 侧：队列深度/失败/下发回执时延，进 VM 供 Grafana）。
	// 读取失败记 -1 表明"未知"，不输假 0。队列深度=待回执任务（pending+dispatched）。
	if tm, err := l1TaskMetricsOrNil(catDB); tm != nil {
		b.WriteString("# HELP sagent_l0_l1task_queue_depth L1 tasks awaiting ack (queue depth)\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_queue_depth %d\n", tm.Pending)
		b.WriteString("# HELP sagent_l0_l1task_done_total L1 tasks completed\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_done_total %d\n", tm.OK)
		b.WriteString("# HELP sagent_l0_l1task_failed_total L1 tasks failed (exec / timeout / rejected)\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_failed_total %d\n", tm.Failed)
		b.WriteString("# HELP sagent_l0_l1task_ack_latency_ms Avg create-to-ack latency of acked tasks\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_ack_latency_ms %d\n", tm.AvgAckMs)
		b.WriteString("# HELP sagent_l0_l1task_acked_total L1 tasks with ack received\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_acked_total %d\n", tm.AckedCount)
	} else if err != nil {
		b.WriteString("# HELP sagent_l0_l1task_queue_depth L1 tasks awaiting ack (queue depth)\n")
		fmt.Fprintf(&b, "sagent_l0_l1task_queue_depth -1\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// l1TaskMetricsOrNil 取 L1 任务可观测汇总；失败返回 (nil, err)，供调用方按未知处理。
func l1TaskMetricsOrNil(catDB *storepkg.DB) (*storepkg.L1TaskMetric, error) {
	if catDB == nil {
		return nil, nil
	}
	return catDB.L1TaskMetrics()
}

// bool01 布尔转 0/1。
func bool01(v bool) int {
	if v {
		return 1
	}
	return 0
}

// pendingFlowsCount 统计非终态流水线数。-- 复用 store 层（catDB.ListRunningFlows），
// 失败返回 -1 表示"未知"，不输假 0。
func pendingFlowsCount(catDB *storepkg.DB) int {
	if catDB == nil {
		return -1
	}
	flows, err := catDB.ListRunningFlows()
	if err != nil {
		return -1
	}
	return len(flows)
}
