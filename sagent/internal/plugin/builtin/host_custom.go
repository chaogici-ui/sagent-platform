package builtin

import (
	"os"
	"os/exec"
	"strings"

	"github.com/sagent/core/internal/constants"
)

// collectHostCustom 主机扩展组：host_fd_used / host_nfs_mount_status / host_chronyd_status 共 3 项
// 原则：有环境才采、没有自动跳过，绝不输出假 0（host_ha_switch_status 口径未定，暂留 custom_scripts）。
func collectHostCustom() []Metric {
	var out []Metric

	// fd 已用数：/proc/sys/fs/file-nr 第一字段（Linux）
	if v, ok := readProcNum("/proc/sys/fs/file-nr"); ok {
		out = append(out, Metric{Name: "host_fd_used", Value: v, Help: "Allocated file descriptors (host level)", Type: constants.MetricTypeGauge})
	}

	// NFS 挂载状态：/proc/mounts 存在 nfs/nfs4 挂载
	if b, err := os.ReadFile("/proc/mounts"); err == nil {
		hasNFS := false
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && (fields[2] == "nfs" || fields[2] == "nfs4") {
				hasNFS = true
				break
			}
		}
		out = append(out, Metric{Name: "host_nfs_mount_status", Value: b2f(hasNFS),
			Help: "Whether any NFS mount is active (1=yes)", Type: constants.MetricTypeGauge})
	}

	// chronyd 状态：chronyc 可用且 tracking 成功
	if path, err := exec.LookPath("chronyc"); err == nil {
		cmd := exec.Command(path, "tracking")
		if err := cmd.Run(); err == nil {
			out = append(out, Metric{Name: "host_chronyd_status", Value: 1,
				Help: "chronyd tracking status (1=ok)", Type: constants.MetricTypeGauge})
		} else {
			out = append(out, Metric{Name: "host_chronyd_status", Value: 0,
				Help: "chronyd tracking status (1=ok)", Type: constants.MetricTypeGauge})
		}
	}
	return out
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
