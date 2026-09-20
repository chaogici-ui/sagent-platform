package pipeline

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

// Pipeline 流水线引擎：接收指标 → 注入标签 → 暴露为 Prometheus 格式
type Pipeline struct {
	mu        sync.RWMutex
	metrics   map[string]*MetricEntry
	resLabels resource.Labels
}

// MetricEntry 带标签的指标条目
type MetricEntry struct {
	Name   string
	Value  float64
	Help   string
	Type   constants.MetricType
	Labels map[string]string
}

// New 创建流水线
func New(labels resource.Labels) *Pipeline {
	return &Pipeline{
		metrics:   make(map[string]*MetricEntry),
		resLabels: labels,
	}
}

// Ingest 从 builtin 采集器接收指标
func (p *Pipeline) Ingest(metrics []builtin.Metric) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, m := range metrics {
		key := metricKey(m.Name, m.Labels)
		entryType := m.Type
		if entryType == "" {
			entryType = constants.MetricTypeGauge
		}
		p.metrics[key] = &MetricEntry{
			Name:   m.Name,
			Value:  m.Value,
			Help:   m.Help,
			Type:   entryType,
			Labels: m.Labels,
		}
	}
}

// PrometheusText 输出 Prometheus exposition 格式
func (p *Pipeline) PrometheusText() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var sb strings.Builder

	for _, entry := range p.metrics {
		// HELP
		if entry.Help != "" {
			sb.WriteString(fmt.Sprintf("# HELP %s %s\n", entry.Name, entry.Help))
		}
		// TYPE
		sb.WriteString(fmt.Sprintf("# TYPE %s %s\n", entry.Name, entry.Type))

		// 构造标签：资源标签 + 指标自身标签
		labels := mergeLabels(p.resLabels.ToMap(), entry.Labels)
		sb.WriteString(formatMetric(entry.Name, entry.Value, labels))
		sb.WriteString("\n")
	}

	return sb.String()
}

// metricKey 生成指标唯一键
func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	var parts []string
	parts = append(parts, name)
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, "\x00")
}

// mergeLabels 合并标签，资源标签优先
func mergeLabels(resLabels, metricLabels map[string]string) map[string]string {
	merged := make(map[string]string, len(resLabels)+len(metricLabels))
	for k, v := range resLabels {
		merged[k] = v
	}
	for k, v := range metricLabels {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// formatMetric 格式化单条指标为 Prometheus 行
func formatMetric(name string, value float64, labels map[string]string) string {
	if len(labels) == 0 {
		return fmt.Sprintf("%s %s", name, formatFloat(value))
	}

	var labelParts []string
	for k, v := range labels {
		labelParts = append(labelParts, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return fmt.Sprintf("%s{%s} %s", name, strings.Join(labelParts, ","), formatFloat(value))
}

// formatFloat 格式化浮点数，避免科学计数法
func formatFloat(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.6g", v)
}

// IngestOTLP 记录 OTLP 数据接收（简化实现）
func (p *Pipeline) IngestOTLP(_ int) {}

// MetricCount 返回当前指标数量
func (p *Pipeline) MetricCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.metrics)
}
