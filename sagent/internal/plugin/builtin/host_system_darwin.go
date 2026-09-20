//go:build darwin

package builtin

import (
	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/host"
)

// macOS 降级：uname/启动时间用 gopsutil；timex 无 adjtimex 封装，跳过

func hostUnameInfo() (Metric, bool) {
	hi, err := host.Info()
	if err != nil {
		return Metric{}, false
	}
	return Metric{Name: "node_uname_info", Value: 1, Help: "System information",
		Type: constants.MetricTypeGauge, Labels: map[string]string{
			"sysname":   hi.OS,
			"nodename":  hi.Hostname,
			"release":   hi.KernelVersion,
			"machine":   hi.KernelArch,
			"domainname": "",
		}}, true
}

func hostBootTime() (float64, bool) {
	bt, err := host.BootTime()
	if err != nil {
		return 0, false
	}
	return float64(bt), true
}

// adjtimexStats macOS 无 x/sys/unix.Adjtimex 封装，不支持（返回 false 跳过）
func adjtimexStats() (offset, maxerror, syncStatus, tai float64, ok bool) {
	return 0, 0, 0, 0, false
}
