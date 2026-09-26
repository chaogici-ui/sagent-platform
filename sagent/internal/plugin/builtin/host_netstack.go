package builtin

import (
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
)

// collectHostNetstack 网络协议栈组：host_node_netstat_* 21 项 + host_node_sockstat_* 6 项 + node_sockstat_sockets_used
// 口径：/proc/net/netstat（IpExt/TcpExt/Ip/Udp）、/proc/net/snmp（Ip6 十六进制）、/proc/net/sockstat；
// 与 node_exporter 同源同字段名（TCP_mem 页数 ×4096 转字节）。
func collectHostNetstack() ([]Metric, error) {
	var out []Metric

	// ---- /proc/net/netstat（Ip / Tcp / TcpExt / IpExt）----
	sections := parseNetstatSections("/proc/net/netstat")
	ip := sections["Ip"]
	tcp := sections["Tcp"]
	tcpExt := sections["TcpExt"]
	ipExt := sections["IpExt"]

	if v, ok := ip["Forwarding"]; ok {
		out = append(out, Metric{Name: "host_node_netstat_ip_forwarding_status", Value: v,
			Help: "IP forwarding status (1=enabled)", Type: constants.MetricTypeGauge})
	}
	if v, ok := tcp["ActiveOpens"]; ok {
		out = append(out, cn("host_node_netstat_tcp_activeopens", v, "TCP active opens"))
	}
	if v, ok := tcp["PassiveOpens"]; ok {
		out = append(out, cn("host_node_netstat_tcp_passiveopens", v, "TCP passive opens"))
	}
	if v, ok := tcp["InErrs"]; ok {
		out = append(out, cn("host_node_netstat_tcp_inerrs_total", v, "TCP input errors"))
	}
	if v, ok := tcp["OutSegs"]; ok {
		out = append(out, cn("host_node_netstat_tcp_outsegs_total", v, "TCP segments sent"))
	}
	if v, ok := tcp["RetransSegs"]; ok {
		out = append(out, cn("host_node_netstat_tcp_retranssegs_total", v, "TCP segments retransmitted"))
	}
	if v, ok := tcp["CurrEstab"]; ok {
		out = append(out, Metric{Name: "host_node_netstat_tcp_currestab", Value: v, Help: "TCP connections currently established", Type: constants.MetricTypeGauge})
	}
	if v, ok := tcpExt["ListenDrops"]; ok {
		out = append(out, Metric{Name: "host_node_netstat_tcpext_listendrops", Value: v, Help: "SYNs dropped due to listen queue overflow", Type: constants.MetricTypeGauge})
	}
	if v, ok := tcpExt["ListenOverflows"]; ok {
		out = append(out, Metric{Name: "host_node_netstat_tcpext_listenoverflows", Value: v, Help: "SYNs dropped due to listen socket overflow", Type: constants.MetricTypeGauge})
	}
	if v, ok := tcpExt["SyncookiesSent"]; ok {
		out = append(out, cn("host_node_netstat_tcpext_syncookiessent_total", v, "SYN cookies sent"))
	}
	if v, ok := tcpExt["SyncookiesRecv"]; ok {
		out = append(out, cn("host_node_netstat_tcpext_syncookiesrecv_total", v, "SYN cookies received"))
	}
	if v, ok := tcpExt["SyncookiesFailed"]; ok {
		out = append(out, cn("host_node_netstat_tcpext_syncookiesfailed", v, "SYN cookies failed"))
	}
	if v, ok := tcpExt["TCPSynRetrans"]; ok {
		out = append(out, Metric{Name: "host_node_netstat_tcpext_tcp_synretrans", Value: v, Help: "TCP SYN retransmissions", Type: constants.MetricTypeGauge})
	}
	if v, ok := ipExt["InOctets"]; ok {
		out = append(out, cn("host_node_netstat_ipext_inoctets_bytes", v, "IP incoming octets"))
	}
	if v, ok := ipExt["OutOctets"]; ok {
		out = append(out, cn("host_node_netstat_ipext_outoctets_bytes", v, "IP outgoing octets"))
	}

	// ---- /proc/net/snmp Ip6 段（十六进制计数）----
	ip6 := parseSnmpSections("/proc/net/snmp")["Ip6"]
	if v, ok := ip6["InOctets"]; ok {
		out = append(out, cn("host_node_netstat_ip6_inoctets_bytes", v, "IPv6 incoming octets"))
	}
	if v, ok := ip6["OutOctets"]; ok {
		out = append(out, cn("host_node_netstat_ip6_outoctets_bytes", v, "IPv6 outgoing octets"))
	}

	// ---- /proc/net/netstat Udp 段 ----
	udp := sections["Udp"]
	if v, ok := udp["InDatagrams"]; ok {
		out = append(out, cn("host_node_netstat_udp_indatagrams", v, "UDP datagrams received"))
	}
	if v, ok := udp["OutDatagrams"]; ok {
		out = append(out, cn("host_node_netstat_udp_outdatagrams", v, "UDP datagrams sent"))
	}
	if v, ok := udp["InErrors"]; ok {
		out = append(out, cn("host_node_netstat_udp_inerrors_total", v, "UDP receive errors"))
	}
	if v, ok := udp["NoPorts"]; ok {
		out = append(out, cn("host_node_netstat_udp_noports", v, "UDP packets to unknown port"))
	}

	// ---- /proc/net/sockstat ----
	sock := parseSockstat("/proc/net/sockstat")
	if v, ok := sock["sockets_used"]; ok {
		out = append(out, Metric{Name: "node_sockstat_sockets_used", Value: v, Help: "Number of sockets in use", Type: constants.MetricTypeGauge})
	}
	sockMetric := map[string]string{
		"tcp_inuse":    "host_node_sockstat_tcp_inuse",
		"tcp_alloc":    "host_node_sockstat_tcp_alloc",
		"tcp_orphan":   "host_node_sockstat_tcp_orphan",
		"tcp_tw":       "host_node_sockstat_tcp_tw",
		"udp_inuse":    "host_node_sockstat_udp_inuse",
	}
	for _, k := range sortedKeys(sockMetric) {
		if v, ok := sock[k]; ok {
			out = append(out, Metric{Name: sockMetric[k], Value: v, Help: "Socket statistics: " + k, Type: constants.MetricTypeGauge})
		}
	}
	if v, ok := sock["tcp_mem_pages"]; ok {
		out = append(out, Metric{Name: "host_node_sockstat_tcp_mem_bytes", Value: v * 4096, Help: "TCP socket memory in bytes", Type: constants.MetricTypeGauge})
	}
	return out, nil
}

func cn(name string, v float64, help string) Metric {
	return Metric{Name: name, Value: v, Help: help, Type: constants.MetricTypeCounter}
}

// parseKVSectionPairs 通用解析：奇偶成对行（字段名行 + 数值行），返回 段名->字段->数值
func parseKVSectionPairs(path string, hexValue bool) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		hLine := strings.TrimSpace(lines[i])
		vLine := strings.TrimSpace(lines[i+1])
		hc := strings.Index(hLine, ":")
		vc := strings.Index(vLine, ":")
		if hc < 0 || vc < 0 || hLine[:hc] != vLine[:vc] {
			continue
		}
		sec := hLine[:hc]
		keys := strings.Fields(hLine[hc+1:])
		vals := strings.Fields(vLine[vc+1:])
		if len(vals) == 0 {
			// 该行本身是字段名行？继续（保证滑窗正确）
			i--
			continue
		}
		m := out[sec]
		if m == nil {
			m = map[string]float64{}
			out[sec] = m
		}
		for j, k := range keys {
			if j >= len(vals) {
				break
			}
			if v, err := strconv.ParseFloat(vals[j], 64); err == nil {
				m[k] = v
			} else if hexValue {
				if v, err := strconv.ParseUint(vals[j], 16, 64); err == nil {
					m[k] = float64(v)
				}
			}
		}
	}
	return out
}

// parseNetstatSections 解析 /proc/net/netstat：偶数行为字段名、奇数行为数值
func parseNetstatSections(path string) map[string]map[string]float64 {
	return parseKVSectionPairs(path, false)
}

// parseSnmpSections 解析 /proc/net/snmp（Ip6 段为十六进制计数）
func parseSnmpSections(path string) map[string]map[string]float64 {
	return parseKVSectionPairs(path, true)
}

// parseSockstat 解析 /proc/net/sockstat：sockets: used 2 / TCP: inuse 3 orphan 0 tw 0 alloc 4 mem 1 / UDP: inuse 2 ...
func parseSockstat(path string) map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		proto := strings.TrimSuffix(fields[0], ":")
		for i := 1; i+1 < len(fields); i += 2 {
			key := strings.ToLower(proto) + "_" + strings.ToLower(fields[i])
			if proto == "sockets" || proto == "Sockets" {
				key = "sockets_" + strings.ToLower(fields[i])
			}
			if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
				out[key] = v
			}
		}
		// TCP mem 行的单位是页
		if proto == "TCP" {
			for i := 1; i+1 < len(fields); i += 2 {
				if fields[i] == "mem" {
					if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
						out["tcp_mem_pages"] = v
					}
				}
			}
		}
	}
	return out
}
