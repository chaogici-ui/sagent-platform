package constants

import "time"

// ====== 版本 ======
const Version = "0.4.0"

// ====== 网络端口 ======
const (
	DefaultMetricsPort   = 19090
	DefaultL0ConsolePort = 8080
)

// ====== 路径（相对于 ~/SAgent/）=====
const (
	DefaultConfigPath     = "conf/SAgent.yaml"
	DefaultBusSock        = "run/bus.sock"
	DefaultControlSock    = "run/control.sock"
	DefaultPIDFile        = "run/SAgent.pid"
	DefaultStopFile       = "run/stopped"
	DefaultLogDir         = "logs"
	DefaultLogFile        = "SAgent.log"
	DefaultDataDir        = "data"
	DefaultWALDir         = "data/wal"
	DefaultCheckpointDir  = "data/checkpoints"
	DefaultPluginDir      = "plugins"
	DefaultConfDir        = "conf"
	DefaultVectorBin      = "plugins/vector/bin/vector"
	DefaultMysqlExportBin = "plugins/mysql_exporter/mysqld_exporter"
	DefaultVectorConfDir  = "conf/plugins/vector"
)

// ====== 时间默认值 ======
const (
	DefaultHostMetricsInterval = 30 * time.Second
	MinHostMetricsInterval     = 5 * time.Second
	DefaultScrapeInterval      = 30 * time.Second
	DefaultPortCheckInterval   = 30 * time.Second
	DefaultExecScriptInterval  = 30 * time.Second
	DefaultExecScriptTimeout   = 10 * time.Second
	DefaultCrashLimit          = 10
	DefaultCrashWindow         = 60 * time.Second
	DefaultStartupWaitTimeout  = 5 * time.Second
	DefaultHeartbeatInterval   = 30 * time.Second
)

// ====== 指标类型枚举 ======
type MetricType string

const (
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeCounter   MetricType = "counter"
	MetricTypeHistogram MetricType = "histogram"
)

// ====== 插件类型枚举 ======
type PluginType string

const (
	PluginTypeBuiltin    PluginType = "builtin"
	PluginTypeSubprocess PluginType = "subprocess"
	PluginTypeScrape     PluginType = "scrape"
	PluginTypeExec       PluginType = "exec"
)

// ====== 插件名称枚举 ======
const (
	PluginNameHostMetrics   = "host_metrics"
	PluginNameLogMetrics    = "log_metrics"    // 日志转指标（当前实现；真日志采集 log_collector 规划中，届时另立插件）
	PluginNameMySQLProbe    = "mysql_probe"
	PluginNameCustomScripts = "custom_scripts"
	PluginNamePortChecker   = "port_checker"
	PluginNameScrape        = "prometheus_scrape"
)

// ====== Agent 形态枚举 ======
type AgentRole string

const (
	AgentRoleEdge  AgentRole = "edge"
	AgentRoleProxy AgentRole = "proxy"
)

// ====== 资源标签常量 ======
const (
	TagResourceID     = "resource_id"
	TagResourceType   = "resource_type"
	TagBusinessSystem = "business_system"
	TagEnv            = "env"
	TagIDC            = "idc"
	TagCluster        = "cluster"
)

// ====== HTTP 端点 ======
const (
	EndpointMetrics = "/metrics"
	EndpointHealth  = "/health"
	EndpointPlugins = "/plugins"
	EndpointOTLP    = "/otlp/v1/metrics"
)
