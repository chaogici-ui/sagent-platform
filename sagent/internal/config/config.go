package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sagent/core/internal/constants"
	"gopkg.in/yaml.v3"
)

// 环境变量名
const (
	EnvResourceID         = "SAGENT_RESOURCE_ID"
	EnvResourceType       = "SAGENT_RESOURCE_TYPE"
	EnvBusinessSystem     = "SAGENT_BUSINESS_SYSTEM"
	EnvEnv                = "SAGENT_ENV"
	EnvIDC                = "SAGENT_IDC"
	EnvCluster            = "SAGENT_CLUSTER"
	EnvL0ConsoleURL       = "SAGENT_L0_CONSOLE_URL"
	EnvL0ConsoleEndpoints = "SAGENT_L0_CONSOLE_ENDPOINTS" // HA 多端点，逗号分隔（主+备用）
	EnvMetricsAuthKey     = "SAGENT_METRICS_AUTH_KEY"     // /metrics 访问鉴权 key（D4：防 curl 直接抓取）
)

// Config SAgent Core 主配置
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Resource  ResourceConfig  `yaml:"resource"`
	Plugins   PluginsConfig   `yaml:"plugins"`
	L0Console L0ConsoleConfig `yaml:"l0_console"` // L0 接入中心回传通道；URL 为空则不启用
}

type L0ConsoleConfig struct {
	URL       string `yaml:"url"`       // 主 L0 接入点（兼容单点部署）；也作为多端点 HA 的第一优先级
	Endpoints string `yaml:"endpoints"` // HA 多端点列表，逗号分隔；非空时优先于 URL 使用
	DataDir   string `yaml:"data_dir"`
	// HeartbeatInterval 控制通道心跳间隔；0 = 按资源类型取默认
	// （采集机 type=proxy → 10s 短心跳，其余 → 30s）。采集机安装时由平台显式下发 10s
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Listen         string `yaml:"listen"`
	MetricsAuthKey string `yaml:"metrics_auth_key"` // /metrics 鉴权 key；勿通过 yaml 提交，优先环境变量注入（D4）
}

// ResourceConfig 资源标识配置
type ResourceConfig struct {
	ID             string `yaml:"id"`
	Type           string `yaml:"type"`
	BusinessSystem string `yaml:"business_system"`
	Env            string `yaml:"env"`
	IDC            string `yaml:"idc"`
	Cluster        string `yaml:"cluster"`
	// Pool / Region 采集机池归属（HA 漂移边界）：采集机（type=proxy/collector*）登记后，
	// 同池采集机互为故障承接方；普通 SAgent 可留空。
	Pool   string `yaml:"pool"`
	Region string `yaml:"region"`
}

// PluginsConfig 插件配置
type PluginsConfig struct {
	HostMetrics      HostMetricsConfig   `yaml:"host_metrics"`
	LogMetrics       LogMetricsConfig    `yaml:"log_metrics"` // 日志转指标（原 log_collector，按产出物正名）
	MySQLProbe       MySQLProbeConfig    `yaml:"mysql_probe"`
	CustomScripts    CustomScriptsConfig `yaml:"custom_scripts"`
	PortChecker      PortCheckerCfg      `yaml:"port_checker"`
	PrometheusScrape PrometheusScrapeCfg `yaml:"prometheus_scrape"`
}

// HostMetricsConfig 主机指标采集配置
// 分组开关与例外清单由平台下发或 yaml 配置驱动；未声明的分组默认开启（对齐「默认全采、按需关闭」的产品语义）
type HostMetricsConfig struct {
	Enabled        bool            `yaml:"enabled"`
	Interval       time.Duration   `yaml:"interval"`
	Groups         map[string]bool `yaml:"groups"`          // 分组开关（key 见 builtin.HostMetricGroupKeys）
	ExcludeMetrics []string        `yaml:"exclude_metrics"` // 指标例外清单（精确匹配指标名）
}

// LogMetricsConfig 日志转指标配置（Vector 引擎）
type LogMetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	BinPath string `yaml:"bin_path"` // Vector 二进制路径，默认 plugins/vector/bin/vector
	ConfDir string `yaml:"conf_dir"` // Vector 配置目录，默认 conf/plugins/vector
}

// MySQLProbeConfig MySQL 采集配置
type MySQLProbeConfig struct {
	Enabled     bool          `yaml:"enabled"`
	ExporterBin string        `yaml:"exporter_bin"` // mysqld_exporter 二进制路径
	Targets     []MySQLTarget `yaml:"targets"`
}

// MySQLTarget 单个 MySQL 目标
type MySQLTarget struct {
	DSN        string `yaml:"dsn"`         // user:pass@tcp(host:port)/
	ResourceID string `yaml:"resource_id"` // 远程资源 ID
}

// CustomScriptsConfig 自定义脚本采集配置
type CustomScriptsConfig struct {
	Enabled bool         `yaml:"enabled"`
	Scripts []ExecScript `yaml:"scripts"`
}

// ExecScript 单个自定义脚本
type ExecScript struct {
	Name       string            `yaml:"name"`
	Command    string            `yaml:"command"`     // 脚本路径
	Interval   time.Duration     `yaml:"interval"`    // 执行间隔
	Timeout    time.Duration     `yaml:"timeout"`     // 超时时间
	ResourceID string            `yaml:"resource_id"` // 资源 ID
	Labels     map[string]string `yaml:"labels"`      // 额外标签
}

// PortCheckerCfg 端口探测配置
type PortCheckerCfg struct {
	Enabled  bool              `yaml:"enabled"`
	Interval time.Duration     `yaml:"interval"`
	Targets  []PortCheckTarget `yaml:"targets"`
}

// PortCheckTarget 探测目标
type PortCheckTarget struct {
	Name     string            `yaml:"name"`
	Type     string            `yaml:"type"` // tcp, http
	Address  string            `yaml:"address"`
	Replicas []string          `yaml:"replicas"` // HA-3：等效备端（主 Address 故障时切换，防抖回切）
	Timeout  time.Duration     `yaml:"timeout"`
	Labels   map[string]string `yaml:"labels"`
}

// PrometheusScrapeCfg 通用抓取配置
type PrometheusScrapeCfg struct {
	Enabled  bool              `yaml:"enabled"`
	Interval time.Duration     `yaml:"interval"`
	Targets  []ScrapeCfgTarget `yaml:"targets"`
}

// ScrapeCfgTarget 抓取目标
type ScrapeCfgTarget struct {
	Name     string            `yaml:"name"`
	URL      string            `yaml:"url"`
	Replicas []string          `yaml:"replicas"` // HA-3：等效备端（主 URL 故障时切换，防抖回切）
	Timeout  time.Duration     `yaml:"timeout"`
	Labels   map[string]string `yaml:"labels"`
}

// Load 从文件路径加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	// 默认值
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":19090"
	}
	// /metrics 鉴权 key（D4）：环境变量优先，覆盖 yaml，避免密钥入库
	if cfg.Server.MetricsAuthKey == "" {
		cfg.Server.MetricsAuthKey = strings.TrimSpace(os.Getenv(EnvMetricsAuthKey))
	}

	// 环境变量覆盖资源标签（fallback）
	cfg.Resource.applyEnvOverrides()
	// L0 接入中心地址：配置文件未声明时用环境变量兜底——
	// 容器编排只改 env 就能接入，不必给每个实例维护一份配置副本
	if cfg.L0Console.URL == "" {
		cfg.L0Console.URL = strings.TrimSpace(os.Getenv(EnvL0ConsoleURL))
	}
	// HA 多端点：环境变量 SAGENT_L0_CONSOLE_ENDPOINTS 逗号分隔；未配置时回落到 URL（单点兼容）
	if cfg.L0Console.Endpoints == "" {
		cfg.L0Console.Endpoints = strings.TrimSpace(os.Getenv(EnvL0ConsoleEndpoints))
	}
	if cfg.L0Console.DataDir == "" {
		cfg.L0Console.DataDir = constants.DefaultDataDir
	}

	return cfg, nil
}

// EnabledPluginNames 已启用的插件名（注册上报的本机自描述，与 plugins 配置段同源）。
// 不再写死 host_metrics——实际跑了什么就报什么，平台看到的清单与 Agent 真实运行态一致
func (c *Config) EnabledPluginNames() []string {
	p := c.Plugins
	names := []string{}
	if p.HostMetrics.Enabled {
		names = append(names, constants.PluginNameHostMetrics)
	}
	if p.LogMetrics.Enabled {
		names = append(names, constants.PluginNameLogMetrics)
	}
	if p.MySQLProbe.Enabled {
		names = append(names, constants.PluginNameMySQLProbe)
	}
	if p.CustomScripts.Enabled {
		names = append(names, constants.PluginNameCustomScripts)
	}
	if p.PortChecker.Enabled {
		names = append(names, constants.PluginNamePortChecker)
	}
	if p.PrometheusScrape.Enabled {
		names = append(names, constants.PluginNameScrape)
	}
	return names
}

// applyEnvOverrides 用环境变量覆盖空值
func (r *ResourceConfig) applyEnvOverrides() {
	if r.ID == "" {
		r.ID = os.Getenv(EnvResourceID)
	}
	if r.Type == "" {
		r.Type = os.Getenv(EnvResourceType)
	}
	if r.BusinessSystem == "" {
		r.BusinessSystem = os.Getenv(EnvBusinessSystem)
	}
	if r.Env == "" {
		r.Env = os.Getenv(EnvEnv)
	}
	if r.IDC == "" {
		r.IDC = os.Getenv(EnvIDC)
	}
	if r.Cluster == "" {
		r.Cluster = os.Getenv(EnvCluster)
	}
}

// validate 校验配置
func (c *Config) validate() error {
	if c.Resource.ID == "" {
		return fmt.Errorf("resource.id is required")
	}
	return nil
}
