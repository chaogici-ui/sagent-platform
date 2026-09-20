//go:build linux

package builtin

import (
	"github.com/sagent/core/internal/constants"
	"golang.org/x/sys/unix"
)

// Linux 专属：uname（syscall）与 adjtimex 时钟状态

// utsChars utsname 定长 byte 字节数组 → 字符串（去尾部 \0）
func utsChars(a []byte) string {
	for i, c := range a {
		if c == 0 {
			return string(a[:i])
		}
	}
	return string(a)
}

// hostUnameInfo uname 信息型指标
func hostUnameInfo() (Metric, bool) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return Metric{}, false
	}
	return Metric{Name: "node_uname_info", Value: 1, Help: "System information",
		Type: constants.MetricTypeGauge, Labels: map[string]string{
			"sysname":    utsChars(uts.Sysname[:]),
			"nodename":   utsChars(uts.Nodename[:]),
			"release":    utsChars(uts.Release[:]),
			"machine":    utsChars(uts.Machine[:]),
			"domainname": utsChars(uts.Domainname[:]),
		}}, true
}

// hostBootTime /proc/stat btime
func hostBootTime() (float64, bool) {
	if st := parseProcStat(); st != nil {
		if v, ok := st["btime"]; ok {
			return v, true
		}
	}
	return 0, false
}

// adjtimexStats adjtimex 系统调用取时钟状态（单位换算：µs→s）
func adjtimexStats() (offset, maxerror, syncStatus, tai float64, ok bool) {
	var t unix.Timex
	if _, err := unix.Adjtimex(&t); err != nil {
		return 0, 0, 0, 0, false
	}
	offset = float64(t.Offset) / 1e6
	maxerror = float64(t.Maxerror) / 1e6
	syncStatus = 1
	if t.Status&unix.STA_UNSYNC != 0 {
		syncStatus = 0
	}
	tai = float64(t.Tai)
	return offset, maxerror, syncStatus, tai, true
}
