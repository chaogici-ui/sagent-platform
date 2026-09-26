package builtin

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/sagent/core/internal/constants"
	"github.com/shirou/gopsutil/v4/load"
)

// collectHostLoadProc 负载与进程组：node_load1 / node_procs_* / node_processes* / node_forks_total /
// node_context_switches_total / node_intr_total 共 9 项
// 口径：/proc/loadavg、/proc/stat、/proc/<pid>/stat 遍历，与 node_exporter 同源。
// 说明：node_processes / node_processes_state / node_processes_threads 为 KPI 标准表自定义口径
//（node_exporter 无此三项），实现为进程总数 / 按状态分布 / 线程总数。
func collectHostLoadProc() ([]Metric, error) {
	var out []Metric

	// 负载
	if avg, err := load.Avg(); err == nil {
		out = append(out, Metric{Name: "node_load1", Value: avg.Load1, Help: "Load average over 1 minute", Type: constants.MetricTypeGauge})
	}

	// /proc/stat：forks / ctxt / intr / procs_running / procs_blocked
	out = append(out, procStatMetrics(parseProcStat())...)

	// 进程总数/状态分布/线程总数（Linux 遍历 /proc/[pid]/stat）
	if total, threads, states, ok := procProcessStats(); ok {
		out = append(out,
			Metric{Name: "node_processes", Value: total, Help: "Total number of processes", Type: constants.MetricTypeGauge},
			Metric{Name: "node_processes_threads", Value: threads, Help: "Total number of process threads", Type: constants.MetricTypeGauge},
		)
		for _, state := range sortedKeys(states) {
			out = append(out, Metric{Name: "node_processes_state", Value: states[state],
				Help: "Number of processes in each state", Type: constants.MetricTypeGauge,
				Labels: map[string]string{"state": state}})
		}
	}
	return out, nil
}

// parseProcStat 读 /proc/stat（读取失败返回 nil，调用方按缺数据处置）
func parseProcStat() map[string]float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	return parseProcStatText(string(b))
}

// parseProcStatText 解析 /proc/stat 关键字段（纯函数，可单测）。
// procs_running / procs_blocked 是 node_exporter 同源口径：procs_blocked = 阻塞在
// 磁盘 IO 上的进程数。不能用 /proc/loadavg 的「总数 - 运行数」冒充——那含 sleeping 进程，
// 会把"阻塞"长期顶到几百上千
func parseProcStatText(text string) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "processes", "ctxt", "intr", "btime", "procs_running", "procs_blocked":
			if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
				out[fields[0]] = v
			}
		}
	}
	return out
}

// procStatMetrics /proc/stat 派生指标（纯函数，可单测）。
// 未采到的字段不产出——缺数据可见，但绝不用 0 冒充（假数据比缺数据更糟）
func procStatMetrics(st map[string]float64) []Metric {
	var out []Metric
	if v, ok := st["processes"]; ok {
		out = append(out, Metric{Name: "node_forks_total", Value: v, Help: "Total number of forks", Type: constants.MetricTypeCounter})
	}
	if v, ok := st["ctxt"]; ok {
		out = append(out, Metric{Name: "node_context_switches_total", Value: v, Help: "Total number of context switches", Type: constants.MetricTypeCounter})
	}
	if v, ok := st["intr"]; ok {
		out = append(out, Metric{Name: "node_intr_total", Value: v, Help: "Total number of interrupts serviced", Type: constants.MetricTypeCounter})
	}
	if v, ok := st["procs_running"]; ok {
		out = append(out, Metric{Name: "node_procs_running", Value: v, Help: "Number of processes in runnable state", Type: constants.MetricTypeGauge})
	}
	if v, ok := st["procs_blocked"]; ok {
		out = append(out, Metric{Name: "node_procs_blocked", Value: v, Help: "Number of processes blocked waiting for disk IO", Type: constants.MetricTypeGauge})
	}
	return out
}

// procProcessStats 遍历 /proc/[pid]/stat 统计进程总数、线程总数、状态分布
func procProcessStats() (total, threads float64, states map[string]float64, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, nil, false
	}
	states = map[string]float64{}
	for _, e := range entries {
		if !e.IsDir() || !isCPUDirNum(e.Name()) { // 纯数字目录即 PID（复用数字判断）
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		// state 是第二个括号后的第一个字符：comm 可含空格/括号，从最后一个 ')' 后解析
		rp := strings.LastIndex(s, ")")
		if rp < 0 || rp+2 >= len(s) {
			continue
		}
		rest := strings.Fields(s[rp+2:]) // rest[0]=state, rest[17]=num_threads (字段20，去掉 comm+state 偏移)
		if len(rest) < 1 {
			continue
		}
		state := rest[0]
		states[state]++
		total++
		if len(rest) >= 18 {
			if t, err := strconv.ParseFloat(rest[17], 64); err == nil {
				threads += t
			}
		}
	}
	return total, threads, states, total > 0
}
