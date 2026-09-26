package builtin

import (
	"os"
	"strconv"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/net"
)

// collectHostNetwork 网络设备组：node_network_* 共 10 项，device 标签
// 口径：与 node_exporter 同源（/proc/net/dev）；MTU/链路/网卡信息 Linux 读 /sys/class/net，
// macOS 用 gopsutil net.Interfaces 降级（无 carrier）。
var hostNetIOCounters = net.IOCounters
var hostNetInterfaces = net.Interfaces

func collectHostNetwork() ([]Metric, error) {
	counters, err := hostNetIOCounters(true)
	if err != nil {
		return nil, err
	}
	var out []Metric
	for _, c := range counters {
		if c.Name == "" {
			continue
		}
		L := map[string]string{"device": c.Name}
		out = append(out,
			Metric{Name: "node_network_receive_bytes_total", Value: float64(c.BytesRecv), Help: "Network device receive bytes", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_receive_packets_total", Value: float64(c.PacketsRecv), Help: "Network device receive packets", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_receive_errs_total", Value: float64(c.Errin), Help: "Network device receive errors", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_receive_drop_total", Value: float64(c.Dropin), Help: "Network device receive dropped packets", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_transmit_bytes_total", Value: float64(c.BytesSent), Help: "Network device transmit bytes", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_transmit_packets_total", Value: float64(c.PacketsSent), Help: "Network device transmit packets", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_transmit_errs_total", Value: float64(c.Errout), Help: "Network device transmit errors", Type: constants.MetricTypeCounter, Labels: L},
			Metric{Name: "node_network_transmit_drop_total", Value: float64(c.Dropout), Help: "Network device transmit dropped packets", Type: constants.MetricTypeCounter, Labels: L},
		)
	}

	// MTU / carrier / info：Linux 优先 /sys/class/net，缺失时 gopsutil 降级
	infos, err := hostNetInterfaces()
	if err != nil {
		return nil, err
	}
	for _, ifc := range infos {
		L := map[string]string{"device": ifc.Name}
		if !isDarwin {
			if mtu, ok := readSysInt("/sys/class/net/" + ifc.Name + "/mtu"); ok {
				out = append(out, Metric{Name: "node_network_mtu_bytes", Value: float64(mtu), Help: "Network device MTU in bytes", Type: constants.MetricTypeGauge, Labels: L})
			}
			if carrier, ok := readSysInt("/sys/class/net/" + ifc.Name + "/carrier"); ok {
				out = append(out, Metric{Name: "node_network_carrier", Value: float64(carrier), Help: "Network device carrier state (1=linked)", Type: constants.MetricTypeGauge, Labels: L})
			}
			operstate := sysFirstLine("/sys/class/net/" + ifc.Name + "/operstate")
			info := Metric{Name: "node_network_info", Value: 1, Help: "Network device information", Type: constants.MetricTypeGauge,
				Labels: map[string]string{
					"device":    ifc.Name,
					"address":   ifc.HardwareAddr,
					"operstate": operstate,
					"mtu":       strconv.Itoa(ifc.MTU),
				}}
			out = append(out, info)
		} else {
			out = append(out,
				Metric{Name: "node_network_mtu_bytes", Value: float64(ifc.MTU), Help: "Network device MTU in bytes", Type: constants.MetricTypeGauge, Labels: L},
				Metric{Name: "node_network_info", Value: 1, Help: "Network device information", Type: constants.MetricTypeGauge,
					Labels: map[string]string{"device": ifc.Name, "address": ifc.HardwareAddr, "mtu": strconv.Itoa(ifc.MTU)}},
			)
		}
	}
	return out, nil
}

// readSysInt 读取 /sys 有符号数值
func readSysInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := firstField(string(b))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// sysFirstLine 读文件第一行（trim 后）
func sysFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return firstField(string(b))
}

// firstField 取文本第一个空白分隔字段
func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return trimSpaceStr(s[:i])
		}
	}
	return trimSpaceStr(s)
}

func trimSpaceStr(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if s == "unknown" {
		return ""
	}
	return s
}
