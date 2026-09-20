package builtin

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/mem"
)

// collectHostMemory 内存组：host_node_memory_* 共 17 项（KPI 标准表命名，host_ 前缀为平台扩展）
// 口径：Linux 直读 /proc/meminfo（kB→×1024 bytes），与 node_exporter 的 MemTotal 等字段同源同名；
// macOS 用 gopsutil 降级输出存在的子集。
func collectHostMemory() []Metric {
	if ms, ok := collectMemoryProc(); ok {
		return ms
	}
	// 非 Linux 降级：gopsutil 子集
	v, err := mem.VirtualMemory()
	if err != nil {
		return nil
	}
	out := []Metric{
		{Name: "host_node_memory_memtotal_bytes", Value: float64(v.Total), Help: "Total memory in bytes", Type: constants.MetricTypeGauge},
		{Name: "host_node_memory_memavailable_bytes", Value: float64(v.Available), Help: "Available memory in bytes", Type: constants.MetricTypeGauge},
	}
	if v.Free > 0 {
		out = append(out, Metric{Name: "host_node_memory_memfree_bytes", Value: float64(v.Free), Help: "Free memory in bytes", Type: constants.MetricTypeGauge})
	}
	if sw, err := mem.SwapMemory(); err == nil {
		out = append(out,
			Metric{Name: "host_node_memory_swaptotal_bytes", Value: float64(sw.Total), Help: "Total swap in bytes", Type: constants.MetricTypeGauge},
			Metric{Name: "host_node_memory_swapfree_bytes", Value: float64(sw.Free), Help: "Free swap in bytes", Type: constants.MetricTypeGauge},
		)
	}
	return out
}

// meminfoNameMap /proc/meminfo 字段 → host_node_memory_* 指标名
var meminfoNameMap = map[string]string{
	"MemTotal":     "host_node_memory_memtotal_bytes",
	"MemFree":      "host_node_memory_memfree_bytes",
	"MemAvailable": "host_node_memory_memavailable_bytes",
	"Buffers":      "host_node_memory_buffers_bytes",
	"Cached":       "host_node_memory_cached_bytes",
	"AnonPages":    "host_node_memory_anonpages_bytes",
	"Inactive":     "host_node_memory_inactive_bytes",
	"Committed_AS": "host_node_memory_committed_as_bytes",
	"Dirty":        "host_node_memory_dirty_bytes",
	"Writeback":    "host_node_memory_writeback_bytes",
	"PageTables":   "host_node_memory_pagetables_bytes",
	"SReclaimable": "host_node_memory_sreclaimable_bytes",
	"Slab":         "host_node_memory_slab_bytes",
	"VmallocUsed":  "host_node_memory_vmallocused_bytes",
	"SwapTotal":    "host_node_memory_swaptotal_bytes",
	"SwapFree":     "host_node_memory_swapfree_bytes",
	"SwapCached":   "host_node_memory_swapcached_bytes",
}

func collectMemoryProc() ([]Metric, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, false
	}
	defer f.Close()

	// 先收集全字段再输出，保证顺序稳定（便于 diff 校验）
	vals := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		name, ok := meminfoNameMap[key]
		if !ok {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		vals[name] = kb * 1024 // /proc/meminfo 数值单位为 kB
	}
	out := make([]Metric, 0, len(vals))
	helps := map[string]string{
		"host_node_memory_memtotal_bytes":     "Total memory in bytes",
		"host_node_memory_memfree_bytes":      "Free memory in bytes",
		"host_node_memory_memavailable_bytes": "Available memory in bytes",
		"host_node_memory_buffers_bytes":      "Buffers memory in bytes",
		"host_node_memory_cached_bytes":       "Cached memory in bytes",
		"host_node_memory_anonpages_bytes":    "Anonymous pages in bytes",
		"host_node_memory_inactive_bytes":     "Inactive memory in bytes",
		"host_node_memory_committed_as_bytes": "Committed address space in bytes",
		"host_node_memory_dirty_bytes":        "Dirty pages in bytes",
		"host_node_memory_writeback_bytes":    "Writeback pages in bytes",
		"host_node_memory_pagetables_bytes":   "Page tables in bytes",
		"host_node_memory_sreclaimable_bytes": "Reclaimable slab in bytes",
		"host_node_memory_slab_bytes":         "Slab memory in bytes",
		"host_node_memory_vmallocused_bytes":  "Vmalloc used in bytes",
		"host_node_memory_swaptotal_bytes":    "Total swap in bytes",
		"host_node_memory_swapfree_bytes":     "Free swap in bytes",
		"host_node_memory_swapcached_bytes":   "Swap cached in bytes",
	}
	for _, name := range sortedKeys(vals) {
		out = append(out, Metric{Name: name, Value: vals[name], Help: helps[name], Type: constants.MetricTypeGauge})
	}
	return out, len(out) > 0
}
