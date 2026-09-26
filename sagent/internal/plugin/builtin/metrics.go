package builtin

import (
	"strconv"
	"strings"
	"time"

	"github.com/sagent/core/internal/constants"
)

// Metric 采集器通用指标结构。
type Metric struct {
	Name   string
	Family string
	Value  float64
	Help   string
	Type   constants.MetricType
	Labels map[string]string
}

// MetricBatch 是单一来源的完整快照；Err 非空时不能替换旧样本。
type MetricBatch struct {
	Source     string
	Metrics    []Metric
	ObservedAt time.Time
	Freshness  time.Duration
	Err        error
}

// SnapshotSource 使用配置身份生成稳定来源，不包含轮次或凭据。
func SnapshotSource(collector string, identity ...string) string {
	var b strings.Builder
	b.WriteString(collector)
	for _, part := range identity {
		b.WriteByte(':')
		b.WriteString(strconv.Quote(part))
	}
	return b.String()
}

func snapshotBatch(source string, metrics []Metric, interval time.Duration, err error) MetricBatch {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if err != nil {
		metrics = nil
	}
	return MetricBatch{Source: source, Metrics: metrics, ObservedAt: time.Now(), Freshness: 3 * interval, Err: err}
}

func sendBatch(ch chan<- MetricBatch, stop <-chan struct{}, batch MetricBatch) bool {
	select {
	case <-stop:
		return false
	default:
	}
	select {
	case ch <- batch:
		return true
	case <-stop:
		return false
	}
}
