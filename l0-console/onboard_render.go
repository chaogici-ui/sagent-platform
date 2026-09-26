package main

import (
	"encoding/json"
	"strconv"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  配置渲染器：平台文档 → SAgent 配置段（展示 / 验证视图）
//  设计依据：PLAN-采集接入中心与流程引擎.md §4.5（2026-09-21 审计后收口职责）
//
//  【职责边界】本渲染器的产物只用于两处：
//    1. 接入中心 sync_config 步骤的事件 detail——给运维看「这台 Agent 最终会跑成什么样」
//    2. 批 C 之前的链路自检——验证参数收集与目标登记的正确性
//  它 **不是** 实际下发通道：真正下发的 agent_config doc 由 syncAgentConfig 生成（简版意图 doc），
//  「意图 doc → SAgent.yaml 各插件段」的翻译独占在 sagent/internal/control/apply.go（批 C）。
//  这样 SAgent 配置 schema 升级时只改 agent 侧一处，平台不必跟着发版。
//
//  映射规则只有一条泛化规则 + 三个特例，因此**任何开源 exporter 都自动被覆盖**：
//
//    - 有专门采集语义的（mysql_probe / custom_scripts / log_metrics / 端口探测）→ 各自配置段
//    - 其余一律视为「Prometheus 端点」→ 走 plugins.prometheus_scrape（通用抓取）
//
//  新增插件不需要在本文件加分支（除非它有专门的采集语义，那才登记一行）。
// ===================================================================

// sectionOf 目标 plugin → SAgent 配置段归属
func sectionOf(plugin string) string {
	switch plugin {
	case "mysql_probe":
		return "mysql_probe"
	case "custom_scripts":
		return "custom_scripts"
	case "log_metrics":
		return "log_metrics"
	case "port_checker", "http_response":
		return "port_checker"
	default:
		// 通用 exporter：一律按 Prometheus 端点抓取
		return "prometheus_scrape"
	}
}

// renderAgentConfigSection 把该 Agent 名下的采集目标渲染成 SAgent 配置段
func renderAgentConfigSection(catDB *storepkg.DB, agentID string) map[string]any {
	plugins := map[string]any{}
	if catDB == nil || agentID == "" {
		return map[string]any{"plugins": plugins}
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return map[string]any{"plugins": plugins, "error": err.Error()}
	}

	mysqlTargets := []map[string]any{}
	scriptTargets := []map[string]any{}
	portTargets := []map[string]any{}
	scrapeTargets := []map[string]any{}
	logCfg := map[string]any{}

	for _, t := range targets {
		if t.AgentID != agentID {
			continue
		}
		p := paramsOf(t)
		switch sectionOf(t.Plugin) {
		case "mysql_probe":
			dsn := ""
			host := firstNonEmpty(p["address"], t.Address)
			if host != "" {
				port := firstNonEmpty(p["port"], "3306")
				user := firstNonEmpty(p["username"], "exporter")
				pass := p["password"]
				dsn = user + ":" + pass + "@tcp(" + host + ":" + port + ")/"
			}
			mysqlTargets = append(mysqlTargets, map[string]any{
				"dsn": dsn, "resource_id": firstNonEmpty(t.ResourceID, t.Name),
			})
		case "custom_scripts":
			scriptTargets = append(scriptTargets, map[string]any{
				"name":          firstNonEmpty(p["script_name"], t.Name),
				"command":       p["command"],
				"interval":      firstNonEmpty(p["interval"], "30s"),
				"timeout":       firstNonEmpty(p["timeout"], "10s"),
				"resource_id":   firstNonEmpty(t.ResourceID, t.Name),
				"output_format": firstNonEmpty(p["output_format"], "line"),
			})
		case "log_metrics":
			logCfg = map[string]any{
				"enabled":       true,
				"metric_prefix": firstNonEmpty(p["metric_prefix"], "log_"),
				"level_field":   firstNonEmpty(p["level_field"], "level"),
			}
		case "port_checker":
			typ := "tcp"
			if t.Plugin == "http_response" {
				typ = "http"
			}
			portTargets = append(portTargets, map[string]any{
				"name": t.Name, "type": typ,
				"address":  firstNonEmpty(p["address"], p["url"], t.Address),
				"timeout":  firstNonEmpty(p["timeout"], "3s"),
				"interval": firstNonEmpty(p["interval"], "30s"),
			})
		default:
			// 通用 exporter：拼出抓取 URL
			url := firstNonEmpty(p["url"])
			if url == "" {
				host := firstNonEmpty(p["address"], t.Address)
				port := firstNonEmpty(p["port"], defaultExporterPort(t.Plugin))
				if host != "" {
					scheme := "http"
					if strings.HasPrefix(host, "https://") || strings.HasPrefix(host, "http://") {
						url = host
						if !strings.HasSuffix(url, "/metrics") {
							url = strings.TrimSuffix(url, "/") + "/metrics"
						}
					} else {
						url = scheme + "://" + host + ":" + port + "/metrics"
					}
				}
			}
			scrapeTargets = append(scrapeTargets, map[string]any{
				"name": t.Name, "url": url,
				"timeout":  firstNonEmpty(p["timeout"], "10s"),
				"interval": firstNonEmpty(p["interval"], "30s"),
			})
		}
	}

	if len(mysqlTargets) > 0 {
		plugins["mysql_probe"] = map[string]any{"enabled": true, "targets": mysqlTargets}
	}
	if len(scriptTargets) > 0 {
		plugins["custom_scripts"] = map[string]any{"enabled": true, "scripts": scriptTargets}
	}
	if len(portTargets) > 0 {
		plugins["port_checker"] = map[string]any{"enabled": true, "targets": portTargets}
	}
	if len(scrapeTargets) > 0 {
		plugins["prometheus_scrape"] = map[string]any{"enabled": true, "targets": scrapeTargets}
	}
	if len(logCfg) > 0 {
		plugins["log_metrics"] = logCfg
	}
	return map[string]any{"plugins": plugins}
}

// paramsOf 解析采集目标上存的插件参数
func paramsOf(t *storepkg.TargetRow) map[string]string {
	out := map[string]string{}
	s := strings.TrimSpace(t.ParamsJSON)
	if s == "" || s == "null" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		return map[string]string{}
	}
	return out
}

// defaultExporterPort 未填端口时的兜底：从 onboard_config.json 的产品语义取
func defaultExporterPort(plugin string) string {
	for _, p := range onboardCfg.OnboardPlugins {
		if p.Probe == plugin && p.Port != "" {
			return p.Port
		}
	}
	return "9100"
}

// exporterURL 供安装命令与探路复用：把 target 的抓取地址拼出来
func exporterURL(host, port, plugin string) string {
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return host
	}
	if port == "" {
		port = defaultExporterPort(plugin)
	}
	return "http://" + host + ":" + port + "/metrics"
}

// atoiSafe 宽松解析整数（渲染器里用于端口等）
func atoiSafe(s, def string) string {
	if _, err := strconv.Atoi(strings.TrimSpace(s)); err != nil {
		return def
	}
	return s
}

// skillVersionTag 由实测 OS/架构生成镜像 tag（与 pick_version 同一套规则）
func skillVersionTag(osName, arch string) string {
	if osName == "" || arch == "" {
		return "latest"
	}
	return strings.ToLower(osName) + "-" + strings.ToLower(arch) + "-" + cfgSAVersion
}
