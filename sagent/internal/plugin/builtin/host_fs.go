package builtin

import (
	"os"
	"strings"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/disk"
)

// collectHostFilesystem 文件系统组：node_filesystem_* 共 6 项，device/mountpoint/fstype 标签
// 口径：statfs（gopsutil），伪文件系统过滤清单对齐 node_exporter 默认行为；
// macOS 复用已有的 APFS 快照过滤逻辑。
func collectHostFilesystem() []Metric {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil
	}
	// 容器等无块设备环境：Partitions(false) 按物理设备过滤后为空，
	// 回退全量挂载表（伪文件系统过滤清单仍兜底，overlay 等真实可 statfs 的保留）
	if len(parts) == 0 {
		parts, err = disk.Partitions(true)
		if err != nil {
			return nil
		}
	}
	var out []Metric
	seen := map[string]bool{}
	for _, p := range parts {
		if pseudoFSType(p.Fstype) || seen[p.Mountpoint] {
			continue
		}
		if runtimeIsDarwin() && !isRealDarwinMount(p.Device, p.Mountpoint) {
			continue
		}
		seen[p.Mountpoint] = true
		usage, err := disk.Usage(p.Mountpoint)
		L := map[string]string{"device": p.Device, "mountpoint": p.Mountpoint, "fstype": p.Fstype}
		if err != nil {
			// statfs 失败：device_error=1（KPI 口径），其余字段跳过
			out = append(out, Metric{Name: "node_filesystem_device_error", Value: 1,
				Help: "Whether an error occurred while getting statistics for the filesystem", Type: constants.MetricTypeGauge, Labels: L})
			continue
		}
		ro := 0.0
		for _, opt := range p.Opts {
			if strings.TrimSpace(opt) == "ro" {
				ro = 1
			}
		}
		out = append(out,
			Metric{Name: "node_filesystem_avail_bytes", Value: float64(usage.Free), Help: "Filesystem space available to non-root users in bytes", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_filesystem_size_bytes", Value: float64(usage.Total), Help: "Filesystem size in bytes", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_filesystem_files", Value: float64(usage.InodesTotal), Help: "Filesystem total file nodes", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_filesystem_files_free", Value: float64(usage.InodesFree), Help: "Filesystem total free file nodes", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_filesystem_readonly", Value: ro, Help: "Filesystem read-only status (1=readonly)", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_filesystem_device_error", Value: 0, Help: "Whether an error occurred while getting statistics for the filesystem", Type: constants.MetricTypeGauge, Labels: L},
		)
	}
	return out
}

// pseudoFSType 伪文件系统过滤（产品化考虑：清单集中一处，后续可配置化）
var pseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "mqueue": true, "shm": true, "hugetlbfs": true,
	"ramfs": true, "nsfs": true, "tracefs": true, "debugfs": true, "bpf": true,
	"fusectl": true, "configfs": true, "pstore": true, "securityfs": true,
	"binfmt_misc": true, "autofs": true, "squashfs": true, "efivarfs": true,
	"overlay": false, // 容器根文件系统保留
}

func pseudoFSType(t string) bool { return pseudoFSTypes[t] }

func runtimeIsDarwin() bool { return os.Getenv("SAGENT_NO_DARWIN_FILTER") != "1" && isDarwin }

// isRealDarwinMount macOS 挂载过滤（沿用原 host_metrics 的 APFS 快照逻辑）
func isRealDarwinMount(device, mountpoint string) bool {
	if mountpoint == "/" {
		return true
	}
	if strings.HasPrefix(mountpoint, "/System/Volumes/") && !strings.HasPrefix(mountpoint, "/System/Volumes/Data") {
		return false
	}
	if strings.HasPrefix(mountpoint, "/Volumes/") {
		return false
	}
	switch mountpoint {
	case "/dev", "/proc", "/sys", "/run", "/tmp":
		return false
	}
	if strings.HasPrefix(device, "map ") || strings.HasPrefix(device, "snap") {
		return false
	}
	return true
}
