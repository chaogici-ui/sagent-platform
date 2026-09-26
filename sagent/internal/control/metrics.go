package control

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Meter 控制通道通信可观测指标（OBS-1：L1 侧心跳/task/上报/端点时延·成功率·切换进 VM，供 Grafana）。
// 全部计数为并发安全原子量或 mutex 快照；不引入探测链，仅统计自身回传动作。
type Meter struct {
	registerTotal   uint64
	registerFail    uint64

	beatTotal       uint64
	beatFail        uint64
	beatLatencySumN uint64 // 毫秒和（配 count 求平均时延）
	beatLatencyCnt  uint64

	taskTotal       uint64
	taskFail        uint64
	taskLatencySumN uint64
	taskLatencyCnt  uint64

	reportBytes     uint64

	endpointSwitch  uint64 // 端点 failover 切换次数（主↔备）

	mu          sync.RWMutex
	curEndpoint string
	lastBeatAt  int64 // unix 秒，最近一次成功心跳
}

// endpoint setter（在 post 命中端点时刷新）
func (m *Meter) setEndpoint(ep string) {
	m.mu.Lock()
	if ep != m.curEndpoint {
		atomic.AddUint64(&m.endpointSwitch, 1)
		m.curEndpoint = ep
	}
	m.mu.Unlock()
}

// PrometheusText 输出 Prometheus exposition 格式文本。
func (m *Meter) PrometheusText() string {
	regTotal := atomic.LoadUint64(&m.registerTotal)
	regFail := atomic.LoadUint64(&m.registerFail)
	beatTotal := atomic.LoadUint64(&m.beatTotal)
	beatFail := atomic.LoadUint64(&m.beatFail)
	taskTotal := atomic.LoadUint64(&m.taskTotal)
	taskFail := atomic.LoadUint64(&m.taskFail)
	sw := atomic.LoadUint64(&m.endpointSwitch)

	var sb strings.Builder
	sb.WriteString("# HELP sagent_ctl_heartbeat_total Control channel heartbeat attempts\n# TYPE sagent_ctl_heartbeat_total counter\n")
	sb.WriteString("sagent_ctl_heartbeat_total " + strconv.FormatUint(beatTotal, 10) + "\n")
	sb.WriteString("# HELP sagent_ctl_heartbeat_fail_total Control channel heartbeat failures\n# TYPE sagent_ctl_heartbeat_fail_total counter\n")
	sb.WriteString("sagent_ctl_heartbeat_fail_total " + strconv.FormatUint(beatFail, 10) + "\n")

	if beatTotal > 0 {
		// 心跳成功率（0..1）。失败率=1-成功率，由消费者按需取差。
		sb.WriteString("# HELP sagent_ctl_heartbeat_success_ratio Control channel heartbeat success ratio (0..1)\n# TYPE sagent_ctl_heartbeat_success_ratio gauge\n")
		sb.WriteString("sagent_ctl_heartbeat_success_ratio " + strconv.FormatFloat(float64(beatTotal-beatFail)/float64(beatTotal), 'f', 4, 64) + "\n")
	}

	lsum := atomic.LoadUint64(&m.beatLatencySumN)
	lcnt := atomic.LoadUint64(&m.beatLatencyCnt)
	if lcnt > 0 {
		// 心跳平均往返时延（毫秒→秒）
		sb.WriteString("# HELP sagent_ctl_heartbeat_latency_seconds Control channel heartbeat average round-trip (seconds)\n# TYPE sagent_ctl_heartbeat_latency_seconds gauge\n")
		sb.WriteString("sagent_ctl_heartbeat_latency_seconds " + strconv.FormatFloat(float64(lsum)/1000.0/float64(lcnt), 'f', 4, 64) + "\n")
	}

	tlsum := atomic.LoadUint64(&m.taskLatencySumN)
	tlcnt := atomic.LoadUint64(&m.taskLatencyCnt)
	sb.WriteString("# HELP sagent_ctl_task_total Control channel tasks received via heartbeat\n# TYPE sagent_ctl_task_total counter\n")
	sb.WriteString("sagent_ctl_task_total " + strconv.FormatUint(taskTotal, 10) + "\n")
	sb.WriteString("# HELP sagent_ctl_task_fail_total Control channel tasks failing locally\n# TYPE sagent_ctl_task_fail_total counter\n")
	sb.WriteString("sagent_ctl_task_fail_total " + strconv.FormatUint(taskFail, 10) + "\n")
	if tlcnt > 0 {
		sb.WriteString("# HELP sagent_ctl_task_latency_average_seconds Control channel average task execution time (seconds)\n# TYPE sagent_ctl_task_latency_average_seconds gauge\n")
		sb.WriteString("sagent_ctl_task_latency_average_seconds " + strconv.FormatFloat(float64(tlsum)/1000.0/float64(tlcnt), 'f', 4, 64) + "\n")
	}

	sb.WriteString("# HELP sagent_ctl_report_bytes_total Control channel payload bytes uploaded\n# TYPE sagent_ctl_report_bytes_total counter\n")
	sb.WriteString("sagent_ctl_report_bytes_total " + strconv.FormatUint(atomic.LoadUint64(&m.reportBytes), 10) + "\n")
	sb.WriteString("# HELP sagent_ctl_endpoint_switch_total Control channel HA endpoint failover switches\n# TYPE sagent_ctl_endpoint_switch_total counter\n")
	sb.WriteString("sagent_ctl_endpoint_switch_total " + strconv.FormatUint(sw, 10) + "\n")

	m.mu.RLock()
	defer m.mu.RUnlock()
	sb.WriteString("# HELP sagent_ctl_register_total Control channel registrations attempted\n# TYPE sagent_ctl_register_total counter\n")
	sb.WriteString("sagent_ctl_register_total " + strconv.FormatUint(regTotal, 10) + "\n")
	sb.WriteString("# HELP sagent_ctl_register_fail_total Control channel registration failures\n# TYPE sagent_ctl_register_fail_total counter\n")
	sb.WriteString("sagent_ctl_register_fail_total " + strconv.FormatUint(regFail, 10) + "\n")
	sb.WriteString("# HELP sagent_ctl_online Control channel online marker\n# TYPE sagent_ctl_online gauge\n")
	sb.WriteString("sagent_ctl_online 1\n")
	return sb.String()
}

// 埋点辅助：时延以毫秒计，原子累加，避免浮点 + 锁开销。
func (m *Meter) addBeat(ok bool, latency time.Duration) {
	if ok {
		atomic.AddUint64(&m.beatTotal, 1)
		atomic.AddUint64(&m.beatLatencySumN, uint64(latency.Milliseconds()))
		atomic.AddUint64(&m.beatLatencyCnt, 1)
		atomic.StoreInt64(&m.lastBeatAt, time.Now().Unix())
	} else {
		atomic.AddUint64(&m.beatTotal, 1)
		atomic.AddUint64(&m.beatFail, 1)
	}
}

func (m *Meter) addRegister(ok bool) {
	atomic.AddUint64(&m.registerTotal, 1)
	if !ok {
		atomic.AddUint64(&m.registerFail, 1)
	}
}

func (m *Meter) addTask(ok bool, latency time.Duration) {
	atomic.AddUint64(&m.taskTotal, 1)
	if !ok {
		atomic.AddUint64(&m.taskFail, 1)
		return
	}
	atomic.AddUint64(&m.taskLatencySumN, uint64(latency.Milliseconds()))
	atomic.AddUint64(&m.taskLatencyCnt, 1)
}

func (m *Meter) addReportBytes(n int) {
	atomic.AddUint64(&m.reportBytes, uint64(n))
}