package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// ==================== 接入配置（产品语义数据化：Agent 类型捆绑 / 接入向导插件清单） ====================
// 红线 2 延伸：插件捆绑关系是产品策略，禁止写死在前端——由后端下发，可在 data/onboard_config.json
// 覆盖（演进路径：插件捆绑包 DB 化）。文件缺失时回退内置默认，行为与历史版本一致。

// OnboardConfig 接入向导与 Agent 注册模板的运行时配置
type OnboardConfig struct {
	AgentTypes []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Desc    string   `json:"desc"`
		Plugins []string `json:"plugins"`
	} `json:"agent_types"`
	ManageablePlugins []string `json:"manageable_plugins"`
	OnboardPlugins    []struct {
		Name  string `json:"name"`
		Probe string `json:"probe"`
		Port  string `json:"port"`
	} `json:"onboard_plugins"`
}

// defaultOnboardConfig 内置默认（与 data/onboard_config.json 保持一致）
func defaultOnboardConfig() *OnboardConfig {
	return &OnboardConfig{
		AgentTypes: []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Desc    string   `json:"desc"`
			Plugins []string `json:"plugins"`
		}{
			{ID: "edge", Name: "📡 边缘采集 Agent", Desc: "部署在主机上，采集 CPU/内存/磁盘/网络、日志指标、运行巡检脚本", Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"}},
			{ID: "proxy", Name: "🔗 Collector Proxy", Desc: "部署在 Proxy 节点，远程采集 MySQL/Redis/Kafka 等中间件指标", Plugins: []string{"mysql_probe", "prometheus_scrape"}},
			{ID: "custom", Name: "⚙️ 自定义", Desc: "按需选择组件，适用于特殊场景", Plugins: []string{}},
		},
		ManageablePlugins: []string{"log_metrics", "mysql_probe", "custom_scripts"},
		OnboardPlugins: []struct {
			Name  string `json:"name"`
			Probe string `json:"probe"`
			Port  string `json:"port"`
		}{
			{Name: "MySQL", Probe: "mysql_probe", Port: "3306"},
			{Name: "Redis", Probe: "redis_probe", Port: "6379"},
			{Name: "Kafka", Probe: "kafka_exporter", Port: "9092"},
			{Name: "Elasticsearch", Probe: "elasticsearch_exporter", Port: "9200"},
			{Name: "ClickHouse", Probe: "clickhouse_exporter", Port: "9363"},
			{Name: "HTTP 拨测", Probe: "http_response", Port: "443"},
			{Name: "自定义脚本", Probe: "custom_scripts", Port: ""},
		},
	}
}

// loadOnboardConfig 加载接入配置：data/onboard_config.json 覆盖 → 内置默认兜底
func loadOnboardConfig(path string) *OnboardConfig {
	cfg := defaultOnboardConfig()
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Printf("onboard config parse warning (使用内置默认): %v", err)
		} else {
			fmt.Println("Onboard config loaded from " + path)
		}
	}
	return cfg
}
