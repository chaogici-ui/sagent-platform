package builtin

import (
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/cpu"
)

// collectHostCPU CPU 组：node_cpu_seconds_total / node_cpu_frequency_* / node_cpu_scaling_frequency_hertz
// 口径：node_cpu_seconds_total 与 node_exporter 完全同源（gopsutil 读 /proc/stat，jiffies→秒）；
// 频率为全局最大/最小值（KPI 标准表按无标签数值口径使用）。
func collectHostCPU() []Metric {
	var out []Metric

	// 每核每模式累计秒（counter）
	if times, err := cpu.Times(true); err == nil {
		for _, t := range times {
			cpuLabel := strings.TrimPrefix(t.CPU, "cpu")
			modes := map[string]float64{
				"user": t.User, "system": t.System, "idle": t.Idle, "nice": t.Nice,
				"iowait": t.Iowait, "irq": t.Irq, "softirq": t.Softirq, "steal": t.Steal,
				"guest": t.Guest, "guest_nice": t.GuestNice,
			}
			for _, mode := range sortedKeys(modes) {
				v := modes[mode]
				if v == 0 {
					continue // 未计入的模式不输出（node_exporter 同行为）
				}
				out = append(out, Metric{
					Name: "node_cpu_seconds_total", Value: v,
					Help: "Seconds the CPUs spent in each mode",
					Type: constants.MetricTypeCounter,
					Labels: map[string]string{"cpu": cpuLabel, "mode": mode},
				})
			}
		}
	}

	// 频率：Linux /sys cpufreq（kHz → Hz）；macOS 无此文件，静默跳过
	var minFreq, maxFreq uint64
	haveFreq := false
	if entries, err := os.ReadDir("/sys/devices/system/cpu"); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "cpu") || !isCPUDirNum(strings.TrimPrefix(name, "cpu")) {
				continue
			}
			base := "/sys/devices/system/cpu/" + name + "/cpufreq"
			if v, ok := readSysUint(base + "/scaling_cur_freq"); ok {
				haveFreq = true
				out = append(out, Metric{
					Name: "node_cpu_scaling_frequency_hertz", Value: float64(v) * 1000,
					Help: "Current scaled CPU frequency in hertz",
					Type: constants.MetricTypeGauge, Labels: map[string]string{"cpu": strings.TrimPrefix(name, "cpu")},
				})
				if v, ok := readSysUint(base + "/cpuinfo_cur_freq"); ok {
					out = append(out, Metric{
						Name: "node_cpu_frequency_hertz", Value: float64(v) * 1000,
						Help: "Current CPU frequency in hertz",
						Type: constants.MetricTypeGauge, Labels: map[string]string{"cpu": strings.TrimPrefix(name, "cpu")},
					})
				}
				if v, ok := readSysUint(base + "/cpuinfo_min_freq"); ok && (minFreq == 0 || v < minFreq) {
					minFreq = v
				}
				if v, ok := readSysUint(base + "/cpuinfo_max_freq"); ok && v > maxFreq {
					maxFreq = v
				}
			}
		}
	}
	if haveFreq {
		if minFreq > 0 {
			out = append(out, Metric{Name: "node_cpu_frequency_min_hertz", Value: float64(minFreq) * 1000,
				Help: "Minimum CPU frequency in hertz", Type: constants.MetricTypeGauge})
		}
		if maxFreq > 0 {
			out = append(out, Metric{Name: "node_cpu_frequency_max_hertz", Value: float64(maxFreq) * 1000,
				Help: "Maximum CPU frequency in hertz", Type: constants.MetricTypeGauge})
		}
	}
	return out
}

func isCPUDirNum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readSysUint 读取 /sys 数值文件（容错：负数/空值返回 false）
func readSysUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
