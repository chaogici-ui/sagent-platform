package pipeline

import (
	"fmt"
	"runtime"
	"time"
)

// SelfMetrics 自监控模块：暴露 Core 自身运行指标
type SelfMetrics struct {
	startTime time.Time
}

// NewSelfMetrics 创建自监控
func NewSelfMetrics() *SelfMetrics {
	return &SelfMetrics{startTime: time.Now()}
}

// Uptime returns the start time for uptime calculation
func (s *SelfMetrics) Uptime() time.Time {
	return s.startTime
}

// Collect 采集自监控指标
func (s *SelfMetrics) Collect() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	goroutines := runtime.NumGoroutine()
	uptime := time.Since(s.startTime).Seconds()

	// Prometheus exposition 格式
	return fmt.Sprintf(`# HELP sagent_uptime_seconds SAgent Core uptime in seconds
# TYPE sagent_uptime_seconds gauge
sagent_uptime_seconds %.0f
# HELP sagent_goroutines Number of goroutines
# TYPE sagent_goroutines gauge
sagent_goroutines %d
# HELP sagent_memory_alloc_bytes Memory allocated in bytes
# TYPE sagent_memory_alloc_bytes gauge
sagent_memory_alloc_bytes %.0f
# HELP sagent_memory_sys_bytes Total memory obtained from OS
# TYPE sagent_memory_sys_bytes gauge
sagent_memory_sys_bytes %.0f
# HELP sagent_gc_pause_ns_total Total GC pause in nanoseconds
# TYPE sagent_gc_pause_ns_total counter
sagent_gc_pause_ns_total %.0f
`, uptime, goroutines, float64(m.Alloc), float64(m.Sys), float64(m.PauseTotalNs))
}
