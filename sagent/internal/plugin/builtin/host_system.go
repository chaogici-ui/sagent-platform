package builtin

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sagent/core/internal/constants"
)

// collectHostSystem 系统与时间组：node_uname_info / node_time_seconds / node_boot_time_seconds /
// node_timex_*（仅 Linux）4 项 / node_temperature_celsius / node_hwmon_temp_celsius
// （周期自检 4 项由编排层在 system 组启用时追加）
func collectHostSystem() []Metric {
	var out []Metric

	// uname（信息型指标，value=1，字段在标签）
	if m, ok := hostUnameInfo(); ok {
		out = append(out, m)
	}
	// 启动时间
	if v, ok := hostBootTime(); ok {
		out = append(out, Metric{Name: "node_boot_time_seconds", Value: v, Help: "Node boot time in unixtime", Type: constants.MetricTypeGauge})
	}

	// 时间
	now := time.Now()
	out = append(out, Metric{Name: "node_time_seconds", Value: float64(now.Unix()) + float64(now.Nanosecond())/1e9,
		Help: "System time in seconds since epoch", Type: constants.MetricTypeGauge})

	// timex（NTP 同步状态；Linux adjtimex 实现，macOS 存根返回 false 跳过）
	if offset, maxerr, sync, tai, ok := adjtimexStats(); ok {
		out = append(out,
			Metric{Name: "node_timex_offset_seconds", Value: offset, Help: "Time offset between local clock and NTP in seconds", Type: constants.MetricTypeGauge},
			Metric{Name: "node_timex_maxerror_seconds", Value: maxerr, Help: "Maximum error in seconds", Type: constants.MetricTypeGauge},
			Metric{Name: "node_timex_sync_status", Value: sync, Help: "Is clock synchronized to a reliable server (1=synced)", Type: constants.MetricTypeGauge},
			Metric{Name: "node_timex_tai_offset_seconds", Value: tai, Help: "TAI offset in seconds", Type: constants.MetricTypeGauge},
		)
	}

	// 温度：/sys/class/hwmon（有才采，虚拟机/桌面机普遍没有则跳过）
	if temps, ok := hwmonTemps(); ok {
		max := 0.0
		for _, m := range temps {
			out = append(out, m)
			if m.Value > max {
				max = m.Value
			}
		}
		out = append(out, Metric{Name: "node_temperature_celsius", Value: max, Help: "Node temperature in celsius (max of sensors)", Type: constants.MetricTypeGauge})
	}
	return out
}

// hwmonTemps 遍历 /sys/class/hwmon 温度传感器（非 Linux 时目录不存在返回 false）
func hwmonTemps() ([]Metric, bool) {
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil {
		return nil, false
	}
	var out []Metric
	for _, e := range entries {
		chip := e.Name()
		base := "/sys/class/hwmon/" + chip
		for i := 1; i <= 16; i++ {
			b, err := os.ReadFile(fmt.Sprintf("%s/temp%d_input", base, i))
			if err != nil {
				continue
			}
			mv, err := strconv.ParseFloat(firstField(string(b)), 64)
			if err != nil {
				continue
			}
			sensor := fmt.Sprintf("temp%d", i)
			if label := strings.TrimSpace(firstLine(fmt.Sprintf("%s/temp%d_label", base, i))); label != "" {
				sensor = label
			}
			out = append(out, Metric{Name: "node_hwmon_temp_celsius", Value: mv / 1000,
				Help: "Hardware monitor temperature in celsius", Type: constants.MetricTypeGauge,
				Labels: map[string]string{"chip": chip, "sensor": sensor}})
		}
	}
	return out, len(out) > 0
}

func firstLine(s string) string {
	if idx := strings.Index(s, "\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
