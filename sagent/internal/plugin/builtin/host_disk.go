package builtin

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/disk"
)

// sectorSize /proc/diskstats 的 sectors 字段以 512 字节为一扇区（内核约定）
const sectorSize = 512

// collectHostDisk 磁盘 IO 组：node_disk_* 共 11 项，per-device 标签
// 口径：Linux 直读 /proc/diskstats（与 node_exporter 同源：sectors×512=bytes，ms→s）；
// macOS 用 gopsutil disk.IOCounters 降级输出存在的子集。
func collectHostDisk() ([]Metric, error) {
	if ms, ok := collectDiskProc(); ok {
		return ms, nil
	}
	// 非 Linux 降级
	counters, err := disk.IOCounters()
	if err != nil {
		return nil, err
	}
	var out []Metric
	for _, name := range sortedKeys(counters) {
		c := counters[name]
		if trimHostDevice(name) {
			continue
		}
		out = append(out,
			dm("node_disk_reads_completed_total", float64(c.ReadCount), "Total disk reads completed", name),
			dm("node_disk_read_bytes_total", float64(c.ReadBytes), "Total disk read bytes", name),
			dm("node_disk_read_time_seconds_total", float64(c.ReadTime), "Total disk read time in seconds", name),
			dm("node_disk_writes_completed_total", float64(c.WriteCount), "Total disk writes completed", name),
			dm("node_disk_write_bytes_total", float64(c.WriteBytes), "Total disk write bytes", name),
			dm("node_disk_write_time_seconds_total", float64(c.WriteTime), "Total disk write time in seconds", name),
		)
		if c.IoTime > 0 {
			out = append(out, dm("node_disk_io_time_seconds_total", float64(c.IoTime), "Total disk IO time in seconds", name))
		}
	}
	return out, nil
}

func dm(name string, v float64, help, device string) Metric {
	return Metric{Name: name, Value: v, Help: help, Type: constants.MetricTypeCounter,
		Labels: map[string]string{"device": device}}
}

func collectDiskProc() ([]Metric, bool) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, false
	}
	defer f.Close()

	var out []Metric
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// 至少 14 列；带 discard 的设备有 18 列
		if len(fields) < 14 {
			continue
		}
		dev := fields[2]
		if trimHostDevice(dev) {
			continue
		}
		num := func(i int) float64 {
			v, _ := strconv.ParseFloat(fields[i], 64)
			return v
		}
		reads := num(3)
		sectorsRead := num(5)
		msReading := num(6)
		writes := num(7)
		sectorsWritten := num(9)
		msWriting := num(10)
		inFlight := num(11)
		msIO := num(12)
		msWeighted := num(13)

		L := map[string]string{"device": dev}
		out = append(out,
			Metric{Name: "node_disk_reads_completed_total", Value: reads, Help: "Total disk reads completed successfully", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_read_bytes_total", Value: sectorsRead * sectorSize, Help: "Total disk read bytes", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_read_time_seconds_total", Value: msReading / 1000, Help: "Total disk read time in seconds", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_writes_completed_total", Value: writes, Help: "Total disk writes completed successfully", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_write_bytes_total", Value: sectorsWritten * sectorSize, Help: "Total disk write bytes", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_write_time_seconds_total", Value: msWriting / 1000, Help: "Total disk write time in seconds", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_io_now", Value: inFlight, Help: "Disk IOs currently in progress", Type: constants.MetricTypeGauge, Labels: L},
			Metric{Name: "node_disk_io_time_seconds_total", Value: msIO / 1000, Help: "Total seconds spent doing disk IO", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_disk_io_time_weighted_seconds_total", Value: msWeighted / 1000, Help: "Weighted seconds spent doing disk IO", Type: constants.MetricTypeCounter, Labels: L},
		)
		// discard 统计（内核 4.18+，字段可缺省）
		if len(fields) >= 17 {
			discards := num(14)
			sectorsDiscarded := num(16)
			out = append(out,
				Metric{Name: "node_disk_discards_completed_total", Value: discards, Help: "Total disk discards completed successfully", Type: constants.MetricTypeCounter, Labels: L},
				Metric{Name: "node_disk_discarded_sectors_total", Value: sectorsDiscarded * sectorSize, Help: "Total discarded sectors", Type: constants.MetricTypeCounter, Labels: L},
			)
		}
	}
	return out, len(out) > 0
}
