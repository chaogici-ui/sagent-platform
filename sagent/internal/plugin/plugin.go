package plugin

import "github.com/sagent/core/internal/constants"

// Metric 统一指标模型
type Metric struct {
	Name   string
	Value  float64
	Help   string
	Type   constants.MetricType
	Labels map[string]string
}

// Plugin 所有插件必须实现的接口
type Plugin interface {
	// Name 返回插件名
	Name() string

	// Type 返回插件类型
	Type() constants.PluginType

	// Start 启动插件，通过 channel 上报指标
	Start(metricsCh chan<- []Metric) error

	// Stop 停止插件
	Stop() error

	// Health 返回健康状态
	Health() Health
}

// Health 插件健康状态
type Health struct {
	OK      bool
	Status  string // "running", "stopped", "error"
	Message string
}

// PluginInfo 插件信息（用于 /plugins 端点）
type PluginInfo struct {
	Name    string               `json:"name"`
	Type    constants.PluginType `json:"type"`
	Status  string               `json:"status"`
	Healthy bool                 `json:"healthy"`
}
