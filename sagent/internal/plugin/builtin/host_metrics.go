package builtin

import (
	"fmt"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
)

// 本文件是 host_metrics 采集器的编排层：
//   - 指标分组以 metricGroup 注册表方式声明（演进点：新增分组只需追加注册项 + 实现 collectHost<组名>，不改核心逻辑）
//   - 分组开关 / 指标例外清单 / 采集间隔全部来自配置（平台下发或 yaml），不在代码里写死
//   - 指标命名口径与《sagent-host-kpi》标准表对齐：node_* 为主，平台自定义扩展用 host_node_* 与 host_* 前缀
//   - 平台差异（Linux 全集 / macOS 降级子集）由各组采集器内部处理，文件不存在时静默跳过，绝不输出假 0

// hostGroupDef 分组注册项
type hostGroupDef struct {
	Key     string                   // 配置/协议里的分组 key（ASCII，平台协议用）
	Label   string                   // 中文名（日志与界面展示）
	Collect func() ([]Metric, error) // 采集函数；返回错误表示该组提供方故障，快照判定为失败并丢弃组内部分产出
}

// hostMetricGroups 分组注册表（顺序即界面展示顺序）
var hostMetricGroups = []hostGroupDef{
	{Key: "cpu", Label: "CPU", Collect: collectHostCPU},
	{Key: "memory", Label: "内存", Collect: collectHostMemory},
	{Key: "disk", Label: "磁盘IO", Collect: collectHostDisk},
	{Key: "filesystem", Label: "文件系统", Collect: collectHostFilesystem},
	{Key: "network", Label: "网络设备", Collect: collectHostNetwork},
	{Key: "netstack", Label: "网络协议栈", Collect: collectHostNetstack},
	{Key: "loadproc", Label: "负载与进程", Collect: collectHostLoadProc},
	{Key: "kernel", Label: "内核统计", Collect: collectHostKernel},
	{Key: "system", Label: "系统与时间", Collect: collectHostSystem},
	{Key: "custom", Label: "主机扩展", Collect: collectHostCustom},
}

// HostGroupLabels 供外部（/plugins、心跳 stats、平台协议）查询分组中文名
func HostGroupLabels() map[string]string {
	m := make(map[string]string, len(hostMetricGroups))
	for _, g := range hostMetricGroups {
		m[g.Key] = g.Label
	}
	return m
}

// HostMetricGroupKeys 分组 key 列表
func HostMetricGroupKeys() []string {
	keys := make([]string, 0, len(hostMetricGroups))
	for _, g := range hostMetricGroups {
		keys = append(keys, g.Key)
	}
	return keys
}

// HostMetricsCollector 主机指标采集器
type HostMetricsCollector struct {
	interval time.Duration
	enabled  map[string]bool // 分组开关（未出现的组默认开启）
	exclude  map[string]bool // 指标例外清单（精确匹配指标名）
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu          sync.Mutex
	lastStats   map[string]float64 // 分组 -> 最近一轮采集条数（心跳/排障用）
	lastTotal   int
	lastDurMs   float64
	lastSuccess float64
}

// NewHostMetricsCollector 创建采集器；interval<=0 时取默认值，低于下限收敛到下限
func NewHostMetricsCollector(interval time.Duration, groups map[string]bool, exclude []string) *HostMetricsCollector {
	if interval <= 0 {
		interval = constants.DefaultHostMetricsInterval
	}
	if min := constants.MinHostMetricsInterval; interval < min {
		interval = min
	}
	enabled := make(map[string]bool, len(groups))
	for k, v := range groups {
		enabled[k] = v
	}
	ex := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		ex[n] = true
	}
	return &HostMetricsCollector{
		interval:  interval,
		enabled:   enabled,
		exclude:   ex,
		stopCh:    make(chan struct{}),
		lastStats: map[string]float64{},
	}
}

// Name 插件名
func (h *HostMetricsCollector) Name() string {
	return constants.PluginNameHostMetrics
}

// Interval 生效采集间隔（收敛后的值，日志/界面展示用）
func (h *HostMetricsCollector) Interval() time.Duration {
	return h.interval
}

// Stats 最近一轮采集统计（心跳携带，排障展示）。分组值为 -1 表示该组本轮 panic
func (h *HostMetricsCollector) Stats() (map[string]float64, int, float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]float64, len(h.lastStats))
	for k, v := range h.lastStats {
		out[k] = v
	}
	return out, h.lastTotal, h.lastDurMs, h.lastSuccess > 0
}

// groupEnabled 判断分组是否启用（配置未声明默认启用）
func (h *HostMetricsCollector) groupEnabled(key string) bool {
	if v, ok := h.enabled[key]; ok {
		return v
	}
	return true
}

// filter 例外清单过滤
func (h *HostMetricsCollector) filter(in []Metric) []Metric {
	if len(h.exclude) == 0 {
		return in
	}
	out := make([]Metric, 0, len(in))
	for _, m := range in {
		if h.exclude[m.Name] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Start 启动采集循环
func (h *HostMetricsCollector) Start(metricsCh chan<- MetricBatch) {
	h.wg.Add(1)
	var cycle uint64
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()

		h.runOnce(metricsCh)
		log.Printf("[host_metrics] initial cycle done platform=%s", runtime.GOOS)
		cycle++

		for {
			select {
			case <-ticker.C:
				h.runOnce(metricsCh)
				log.Printf("[host_metrics] cycle=%d total=%d durMs=%.1f", cycle, h.lastTotal, h.lastDurMs)
				cycle++
			case <-h.stopCh:
				return
			}
		}
	}()
}

// runOnce 执行一轮采集：遍历启用的分组，例外过滤，追加周期自检指标
func (h *HostMetricsCollector) runOnce(metricsCh chan<- MetricBatch) {
	start := time.Now()
	var out []Metric
	var collectErr error
	stats := make(map[string]float64, len(hostMetricGroups))

	for _, g := range hostMetricGroups {
		if !h.groupEnabled(g.Key) {
			continue
		}
		func() {
			// 单分组 panic 不拖垮整个采集器（产品化要求：采集隔离）
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[host_metrics] group %s panic: %v", g.Key, r)
					stats[g.Key] = -1
					collectErr = fmt.Errorf("host group %s panic: %v", g.Key, r)
				}
			}()
			ms, err := g.Collect()
			if err != nil {
				// 提供方故障：组标记为失败（-1），丢弃组内部分产出，快照判定为失败
				stats[g.Key] = -1
				collectErr = err
				return
			}
			ms = h.filter(ms)
			stats[g.Key] = float64(len(ms))
			out = append(out, ms...)
		}()
	}

	dur := time.Since(start)
	// 周期自检指标（归属 system 组语义；组被关闭或被排除时同样过滤）
	if h.groupEnabled("system") {
		success := 1.0
		for _, v := range stats {
			if v < 0 {
				success = 0
			}
		}
		durS := dur.Seconds()
		self := h.filter([]Metric{
			{Name: "host_up", Value: 1, Help: "Host collector up", Type: constants.MetricTypeGauge},
			{Name: "host_scrape_duration_seconds", Value: durS, Help: "Host collection duration in seconds", Type: constants.MetricTypeGauge},
			{Name: "node_scrape_collector_duration_seconds", Value: durS, Help: "Duration of the last collection cycle in seconds", Type: constants.MetricTypeGauge},
			{Name: "node_scrape_collector_success", Value: success, Help: "Whether the last collection cycle succeeded", Type: constants.MetricTypeGauge},
		})
		out = append(out, self...)
	}

	h.mu.Lock()
	h.lastStats = stats
	h.lastTotal = len(out)
	h.lastDurMs = float64(dur.Microseconds()) / 1000.0
	h.lastSuccess = 1
	if collectErr != nil {
		h.lastSuccess = 0
	}
	h.mu.Unlock()

	sendBatch(metricsCh, h.stopCh, snapshotBatch(SnapshotSource(h.Name(), "local"), out, h.interval, collectErr))
}

// Stop 停止采集
func (h *HostMetricsCollector) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}

// trimHostDevice 过滤虚拟/无意义磁盘设备（对齐 node_exporter 行为，跨平台共用）
func trimHostDevice(dev string) bool {
	if dev == "" {
		return true
	}
	for _, p := range []string{"loop", "ram", "zram", "fd", "dm-", "md"} {
		if strings.HasPrefix(dev, p) {
			return true
		}
	}
	return false
}

// sortedKeys 工具：map key 排序（保证输出稳定，便于 diff 校验）
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
