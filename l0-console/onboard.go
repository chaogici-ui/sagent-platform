package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// ==================== 接入配置（产品语义数据化：Agent 类型捆绑 / 接入向导插件清单 / 能力声明） ====================
// 红线 2 延伸：插件捆绑关系与可选能力清单是产品策略，禁止写死在前端——由后端下发，
// 可在 data/onboard_config.json 覆盖（演进路径：产品策略 DB 化）。文件缺失时回退内置默认。
//
// 与 params.yaml 的分工（见 PLAN §2.1）：
//   - 有插件包的采集能力（MySQL/Redis/Kafka…）：参数声明放插件包内 params.yaml（L3 层，随包走）
//   - 无插件包的系统内置能力（host_metrics/custom_scripts…）：参数声明内联于此（L1 层，平台自带）

// AgentType Agent 部署模板（安装部署页的「选模板」来源）
type AgentType struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Desc    string   `json:"desc"`
	Plugins []string `json:"plugins"`
}

// OnboardPlugin 可接入的采集能力（远程采集为主，也含日志转指标等）
type OnboardPlugin struct {
	Name   string `json:"name"`   // 展示名（与插件包目录名一致）
	Probe  string `json:"probe"`  // 能力 id，即 targets.plugin 的取值（如 mysql_probe）
	Port   string `json:"port"`   // 默认端口
	Scope  string `json:"scope"`  // agent（装 Agent 的机器上采）/ remote（远程拉取）
	Params string `json:"params"` // 参数声明文件（相对 data/integrations/），空表示无参数表单
}

// AgentAbility Agent 侧内置能力（无插件包，参数声明内联）
type AgentAbility struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Desc         string       `json:"desc"`
	Scope        string       `json:"scope"`
	Locked       bool         `json:"locked"` // 地基能力（host_metrics）：强制开启、不可关闭
	ParamsInline []ParamField `json:"params_inline"`
}

// StepTimeout 单个原子能力（原子=步骤）的超时与重试策略。
// 数据驱动：默认值在 data/onboard_config.json 的 step_timeouts 里调整，引擎里不写死任何时长。
//
//   - RunSec  单次尝试的总时长上限：从「本次尝试开始」起算，超了就判死。
//     存在的意义是"不能无底线的等待"——无论作业卡死、回报丢失还是进程重启，都必须在有限时间内收敛
//   - IdleSec 静默上限：从「最近一次有实际进展」起算（新任务输出 / 收到回报）。
//     比 RunSec 更早发现问题——连接挂着但目标机毫无反应的场景只能靠它暴露
//   - Retry   允许的**额外**尝试次数（0 = 不自动重试，总共执行 1 次）。
//     只给"一点"重试机会：瞬断、端口竞态值得再试；真配置错了重试 100 次也不会对
//   - RetryDelaySec 重试前的冷却时间（秒），避免对端还没缓过来就被二次打上来
type StepTimeout struct {
	RunSec        int `json:"run_sec"`
	IdleSec       int `json:"idle_sec"`
	Retry         int `json:"retry"`
	RetryDelaySec int `json:"retry_delay_sec"`
}

// OnboardConfig 接入向导、安装模板与能力清单的运行时配置
type OnboardConfig struct {
	AgentTypes        []AgentType     `json:"agent_types"`
	ManageablePlugins []string        `json:"manageable_plugins"`
	OnboardPlugins    []OnboardPlugin `json:"onboard_plugins"`
	AgentAbilities    []AgentAbility  `json:"agent_abilities"`
	// StepTimeouts 步骤超时策略：key = atom id，另有 "default" 兜底
	StepTimeouts map[string]StepTimeout `json:"step_timeouts"`
}

// defaultOnboardConfig 内置默认（与 data/onboard_config.json 保持一致；文件缺失时兜底）
// 注意：默认里的插件名与能力名是「产品策略默认值」，与数据文件同源，不做逻辑分支。
func defaultOnboardConfig() *OnboardConfig {
	return &OnboardConfig{
		AgentTypes: []AgentType{
			{ID: "edge", Name: "📡 边缘采集 Agent", Desc: "部署在主机上，采集 CPU/内存/磁盘/网络、日志指标、运行巡检脚本", Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"}},
			{ID: "proxy", Name: "🔗 Collector Proxy", Desc: "部署在 Proxy 节点，远程采集 MySQL/Redis/Kafka 等中间件指标", Plugins: []string{"mysql_probe", "prometheus_scrape"}},
			{ID: "custom", Name: "⚙️ 自定义", Desc: "按需选择组件，适用于特殊场景", Plugins: []string{}},
		},
		ManageablePlugins: []string{"log_metrics", "mysql_probe", "custom_scripts"},
		OnboardPlugins: []OnboardPlugin{
			{Name: "MySQL", Probe: "mysql_probe", Port: "3306", Scope: "remote", Params: "MySQL/params.yaml"},
			{Name: "Redis", Probe: "redis_probe", Port: "6379", Scope: "remote", Params: "Redis/params.yaml"},
			{Name: "Kafka", Probe: "kafka_exporter", Port: "9092", Scope: "remote", Params: "Kafka/params.yaml"},
			{Name: "Elasticsearch", Probe: "elasticsearch_exporter", Port: "9200", Scope: "remote", Params: "Elasticsearch/params.yaml"},
			{Name: "ClickHouse", Probe: "clickhouse_exporter", Port: "9363", Scope: "remote", Params: "ClickHouse/params.yaml"},
			{Name: "HTTP 拨测", Probe: "http_response", Port: "443", Scope: "remote", Params: "HTTP_Response/params.yaml"},
			{Name: "日志转指标", Probe: "log_metrics", Port: "", Scope: "agent", Params: "log_metrics/params.yaml"},
		},
		AgentAbilities: []AgentAbility{
			{ID: "host_metrics", Name: "主机指标", Desc: "CPU / 内存 / 磁盘 / 网络 / 进程，平台设备的地基指标", Scope: "agent", Locked: true},
			{
				ID: "custom_scripts", Name: "自定义脚本", Scope: "agent",
				Desc: "定时执行脚本，stdout 按行协议产出指标（脚本本体在「采集插件 → 自定义脚本」维护）",
				ParamsInline: []ParamField{
					{Name: "script_name", Label: "脚本标识", Type: "string", Required: true, Help: "对应采集插件里维护的脚本名"},
					{Name: "interval", Label: "执行间隔", Type: "duration", Default: "30s"},
					{Name: "timeout", Label: "超时时间", Type: "duration", Default: "10s"},
					{Name: "output_format", Label: "输出协议", Type: "enum", Options: []string{"line", "json"}, Default: "line", Help: "line：metric_name{label=value} 1.0；json：{\"name\":..,\"value\":..}"},
				},
			},
			{
				ID: "port_checker", Name: "端口探测", Scope: "agent",
				Desc: "周期性探测目标端口可用性，产出 up 指标",
				ParamsInline: []ParamField{
					{Name: "targets", Label: "探测目标", Type: "textarea", Required: true, Help: "每行一个 host:port"},
					{Name: "interval", Label: "探测间隔", Type: "duration", Default: "30s"},
				},
			},
			{
				ID: "prometheus_scrape", Name: "抓取 exporter", Scope: "agent",
				Desc: "拉取外部 Prometheus 端点（通用 exporter 接入方式）",
				ParamsInline: []ParamField{
					{Name: "url", Label: "抓取地址", Type: "string", Required: true, Placeholder: "http://10.20.30.50:9100/metrics", Probe: "http"},
					{Name: "interval", Label: "抓取间隔", Type: "duration", Default: "30s"},
				},
			},
		},
		// 步骤超时默认：平台代执行的探路/安装给足但有限（探路 ≤10min、安装 ≤15min），
		// 静默上限收得更紧（连接挂死、目标机无响应要尽早暴露）；回落人工路径的步骤
		// 不自动重试（人操作没有"自动重试"的语义，重试=再由人执行一次）
		StepTimeouts: map[string]StepTimeout{
			"default":           {RunSec: 3600, IdleSec: 3600, Retry: 0, RetryDelaySec: 15},
			"preflight_host":    {RunSec: 600, IdleSec: 150, Retry: 1, RetryDelaySec: 10},
			"install_agent":     {RunSec: 900, IdleSec: 240, Retry: 1, RetryDelaySec: 15},
			"uninstall_agent":   {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			// 卸载前置三件套：扫描是只读快操作，卸插件与清自启都要等进程真正退出，
			// 给足但有限（各自 ≤5min）；都允许自动重试一次——这类失败多为瞬时占用，
			// 重试一次能救回来，而卸载本身是幂等的，重跑不会造成二次破坏
			"scan_collectors":   {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			"uninstall_plugins": {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			"cleanup_autostart": {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			// 远程可达性探路（atom preflight_ability；原「能力探路」环节已并入主机探路，
			// 该 atom 现由 remote/hybrid 的 preflight_remote 步骤使用）：探目标端口与账号，给足但有限
			"preflight_ability": {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			// 配置渲染与平台设备登记是快步骤：显式登记，不落到 default 3600s
			// （2026-09-21 用户评审：每个环节都要有显式超时，总时长只做兜底）
			"sync_config":              {RunSec: 600, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			"register_platform_device": {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			"verify_probe":             {RunSec: 300, IdleSec: 120, Retry: 1, RetryDelaySec: 10},
			"observe_collect":          {RunSec: 900, IdleSec: 600, Retry: 1, RetryDelaySec: 30},
			// self_metrics 与 observe_collect 共用 observe_collect atom，但 Agent 注册回报是
			// 分钟级动作：180s 无进展就该失败，不许按采集观察周期（600s）傻等（五阶段评审）
			"self_metrics":             {RunSec: 300, IdleSec: 180, Retry: 1, RetryDelaySec: 10},
		},
	}
}

// loadOnboardConfig 加载接入配置：data/onboard_config.json 覆盖 → 内置默认兜底
func loadOnboardConfig(path string) *OnboardConfig {
	cfg := defaultOnboardConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("onboard config not found (%s), 使用内置默认", path)
		return cfg
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		log.Printf("onboard config parse warning (使用内置默认): %v", err)
		return cfg
	}
	fmt.Println("Onboard config loaded from " + path)
	return cfg
}
