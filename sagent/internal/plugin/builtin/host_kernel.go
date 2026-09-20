package builtin

import (
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
)

// collectHostKernel 内核统计组：node_vmstat_* / node_entropy / node_filefd_* / node_nf_conntrack_* /
// node_pressure_*（5 条，node_exporter 标准 PSI 命名）/ node_arp_entries
// 口径：/proc/vmstat、/proc/sys/*、/proc/pressure/*、/proc/net/arp，与 node_exporter 同源；
// PSI 映射：cpu=some、io=waiting(some)+stalled(full)、memory=waiting(some)+stalled(full)。
func collectHostKernel() []Metric {
	var out []Metric

	// /proc/vmstat
	if vm := parseKVFile("/proc/vmstat"); vm != nil {
		for k, name := range map[string]string{
			"pgfault":    "node_vmstat_pgfault",
			"pgmajfault": "node_vmstat_pgmajfault",
			"pswpin":     "node_vmstat_pswpin",
			"pswpout":    "node_vmstat_pswpout",
			"oom_kill":   "node_vmstat_oom_kill",
		} {
			if v, ok := vm[k]; ok {
				out = append(out, Metric{Name: name, Value: v, Help: "vmstat counter " + k, Type: constants.MetricTypeCounter})
			}
		}
	}

	// 熵
	if v, ok := readProcNum("/proc/sys/kernel/random/entropy_avail"); ok {
		out = append(out, Metric{Name: "node_entropy_available_bits", Value: v, Help: "Available entropy in bits", Type: constants.MetricTypeGauge})
	}

	// 文件描述符：/proc/sys/fs/file-nr → allocated, free, max
	if b, err := os.ReadFile("/proc/sys/fs/file-nr"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 {
			alloc, _ := strconv.ParseFloat(fields[0], 64)
			max, _ := strconv.ParseFloat(fields[2], 64)
			out = append(out,
				Metric{Name: "node_filefd_allocated", Value: alloc, Help: "File descriptor statistics: allocated", Type: constants.MetricTypeGauge},
				Metric{Name: "node_filefd_maximum", Value: max, Help: "File descriptor statistics: maximum", Type: constants.MetricTypeGauge},
			)
		}
	}

	// conntrack（模块未加载时文件不存在，跳过）
	if v, ok := readProcNum("/proc/sys/net/netfilter/nf_conntrack_count"); ok {
		out = append(out, Metric{Name: "node_nf_conntrack_entries", Value: v, Help: "Number of conntrack entries", Type: constants.MetricTypeGauge})
	}
	if v, ok := readProcNum("/proc/sys/net/netfilter/nf_conntrack_max"); ok {
		out = append(out, Metric{Name: "node_nf_conntrack_entries_limit", Value: v, Help: "Conntrack entries limit", Type: constants.MetricTypeGauge})
	}

	// PSI（内核 4.20+；文件不存在时跳过）。
	// 命名对齐 node_exporter v1.8 实测：cpu=waiting(some)、io/memory=waiting(some)+stalled(full)，
	// KPI 表原文 io/memory 的 stall 为口径偏差，以 node_exporter 为准（CP2 结论）
	if v, ok := psiTotal("/proc/pressure/cpu", "some"); ok {
		out = append(out, Metric{Name: "node_pressure_cpu_waiting_seconds_total", Value: v, Help: "Total CPU waiting time in seconds", Type: constants.MetricTypeCounter})
	}
	if v, ok := psiTotal("/proc/pressure/io", "some"); ok {
		out = append(out, Metric{Name: "node_pressure_io_waiting_seconds_total", Value: v, Help: "Total IO waiting time in seconds", Type: constants.MetricTypeCounter})
	}
	if v, ok := psiTotal("/proc/pressure/io", "full"); ok {
		out = append(out, Metric{Name: "node_pressure_io_stalled_seconds_total", Value: v, Help: "Total IO stall time in seconds", Type: constants.MetricTypeCounter})
	}
	if v, ok := psiTotal("/proc/pressure/memory", "some"); ok {
		out = append(out, Metric{Name: "node_pressure_memory_waiting_seconds_total", Value: v, Help: "Total memory waiting time in seconds", Type: constants.MetricTypeCounter})
	}
	if v, ok := psiTotal("/proc/pressure/memory", "full"); ok {
		out = append(out, Metric{Name: "node_pressure_memory_stalled_seconds_total", Value: v, Help: "Total memory stall time in seconds", Type: constants.MetricTypeCounter})
	}

	// ARP 表项数
	if v, ok := countARPPentries("/proc/net/arp"); ok {
		out = append(out, Metric{Name: "node_arp_entries", Value: v, Help: "ARP entries by device", Type: constants.MetricTypeGauge})
	}
	return out
}

// parseKVFile 解析 "key value" 行式文件
func parseKVFile(path string) map[string]float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
			out[fields[0]] = v
		}
	}
	return out
}

// readProcNum 读单数值 /proc 文件
func readProcNum(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(firstField(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// psiTotal 解析 /proc/pressure/<res>：返回指定行（some/full）的 total（µs→s）
func psiTotal(path, kind string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, kind+" ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "total=") {
				us, err := strconv.ParseFloat(strings.TrimPrefix(f, "total="), 64)
				if err != nil {
					return 0, false
				}
				return us / 1e6, true
			}
		}
	}
	return 0, false
}

// countARPPentries /proc/net/arp 表项数（去表头）
func countARPPentries(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 1 {
		return 0, false
	}
	return float64(len(lines) - 1), true
}
