package pipeline

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

// Pipeline 保存采集运行态；事件与来源快照使用独立缓存。
type Pipeline struct {
	mu        sync.Mutex
	metrics   map[string]*MetricEntry
	sources   map[string]sourceSnapshot
	resLabels resource.Labels
	now       func() time.Time
	sequence  uint64
}

type sourceSnapshot struct {
	metrics     map[string]*MetricEntry
	lastSuccess time.Time
	expiresAt   time.Time
}

// MetricEntry 带标签的指标条目。
type MetricEntry struct {
	Name     string
	Family   string
	Value    float64
	Help     string
	Type     constants.MetricType
	Labels   map[string]string
	sequence uint64
}

func New(labels resource.Labels) *Pipeline {
	return NewWithClock(labels, time.Now)
}

func NewWithClock(labels resource.Labels, now func() time.Time) *Pipeline {
	if now == nil {
		now = time.Now
	}
	return &Pipeline{
		metrics: make(map[string]*MetricEntry), sources: make(map[string]sourceSnapshot),
		resLabels: labels, now: now,
	}
}

// Ingest 保留 bus 事件的增量覆盖行为，不应用快照有效期。
func (p *Pipeline) Ingest(metrics []builtin.Metric) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	for key, entry := range p.entries(metrics) {
		p.metrics[key] = entry
	}
}

// IngestBatch 成功（含空批次）替换来源；失败不刷新有效期。成功批次须有来源和正 Freshness。
func (p *Pipeline) IngestBatch(batch builtin.MetricBatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.expire(now)
	if batch.Source == "" || batch.Err != nil || batch.Freshness <= 0 {
		return
	}
	observed := batch.ObservedAt
	if observed.IsZero() {
		observed = now
	}
	p.sequence++
	p.sources[batch.Source] = sourceSnapshot{
		metrics: p.entries(batch.Metrics), lastSuccess: observed, expiresAt: observed.Add(batch.Freshness),
	}
	p.expire(now)
}

// RemoveSource 供配置应用在停用或删除目标后显式移除来源。
func (p *Pipeline) RemoveSource(source string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sources, source)
}

func (p *Pipeline) entries(metrics []builtin.Metric) map[string]*MetricEntry {
	entries := make(map[string]*MetricEntry, len(metrics))
	for _, m := range metrics {
		labels := mergeLabels(p.resLabels.ToMap(), m.Labels)
		entryType := m.Type
		if entryType == "" {
			entryType = constants.MetricTypeGauge
		}
		family := m.Family
		if family == "" {
			family = m.Name
		}
		entries[metricKey(m.Name, labels)] = &MetricEntry{
			Name: m.Name, Family: family, Value: m.Value, Help: m.Help, Type: entryType, Labels: labels, sequence: p.sequence,
		}
	}
	return entries
}

func (p *Pipeline) expire(now time.Time) {
	for source, snapshot := range p.sources {
		if !now.Before(snapshot.expiresAt) {
			delete(p.sources, source)
		}
	}
}

func (p *Pipeline) visible() map[string]*MetricEntry {
	p.expire(p.now())
	out := make(map[string]*MetricEntry, len(p.metrics))
	for key, entry := range p.metrics {
		out[key] = entry
	}
	for _, snapshot := range p.sources {
		for key, entry := range snapshot.metrics {
			// 相同有效标签只暴露最新接收样本；移除来源不影响其他来源的快照。
			if previous, ok := out[key]; !ok || entry.sequence > previous.sequence {
				out[key] = entry
			}
		}
	}
	return out
}

func (p *Pipeline) PrometheusText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.visible()
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := entries[keys[i]], entries[keys[j]]
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		return keys[i] < keys[j]
	})
	var sb strings.Builder
	for i := 0; i < len(keys); {
		entry := entries[keys[i]]
		end, help := i+1, entry.Help
		for end < len(keys) && entries[keys[end]].Family == entry.Family {
			if help == "" {
				help = entries[keys[end]].Help
			}
			end++
		}
		if help != "" {
			fmt.Fprintf(&sb, "# HELP %s %s\n", entry.Family, helpEscaper.Replace(help))
		}
		fmt.Fprintf(&sb, "# TYPE %s %s\n", entry.Family, entry.Type)
		for ; i < end; i++ {
			m := entries[keys[i]]
			sb.WriteString(formatMetric(m.Name, m.Value, m.Labels))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// metricKey 对标签排序并编码值，避免顺序变化或分隔符导致键碰撞。
func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, name)
	for _, k := range keys {
		parts = append(parts, strconv.Quote(k)+"="+strconv.Quote(labels[k]))
	}
	return strings.Join(parts, "\x00")
}

// 仅显式非空的远程 resource_id 可覆盖 Core；其他身份标签保持 Core 优先。
func mergeLabels(resLabels, metricLabels map[string]string) map[string]string {
	merged := make(map[string]string, len(resLabels)+len(metricLabels))
	for k, v := range resLabels {
		merged[k] = v
	}
	for k, v := range metricLabels {
		if _, exists := merged[k]; !exists || k == resource.TagResourceID && v != "" {
			merged[k] = v
		}
	}
	return merged
}

var labelEscaper = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
var helpEscaper = strings.NewReplacer("\\", "\\\\", "\n", "\\n")

func formatMetric(name string, value float64, labels map[string]string) string {
	if len(labels) == 0 {
		return fmt.Sprintf("%s %s", name, formatFloat(value))
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	labelParts := make([]string, 0, len(keys))
	for _, key := range keys {
		labelParts = append(labelParts, fmt.Sprintf(`%s="%s"`, key, labelEscaper.Replace(labels[key])))
	}
	return fmt.Sprintf("%s{%s} %s", name, strings.Join(labelParts, ","), formatFloat(value))
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.6g", v)
}

func (p *Pipeline) IngestOTLP(_ int) {}

func (p *Pipeline) MetricCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.visible())
}
