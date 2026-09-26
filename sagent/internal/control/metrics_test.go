package control

// metrics_test.go —— OBS-1：控制通道通信可观测指标（Meter）计数/时延/切换正确性。
//
// 覆盖：心跳成功率与时延、任务执行计费、上报字节累计、端点切换防抖、
// PrometheusText 输出含 sagent_ctl_* 系列且不因空态 panic。

import (
	"strings"
	"testing"
	"time"
)

func TestMeterHeartbeatCounters(t *testing.T) {
	m := &Meter{}
	m.addBeat(true, 50*time.Millisecond)
	m.addBeat(true, 100*time.Millisecond)
	m.addBeat(false, 0)

	text := m.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_heartbeat_total 3") {
		t.Fatalf("heartbeat_total 应为 3，实际：\n%s", text)
	}
	if !strings.Contains(text, "sagent_ctl_heartbeat_fail_total 1") {
		t.Fatalf("heartbeat_fail_total 应为 1")
	}
	// 成功率 = (3-1)/3 = 0.6667
	if !strings.Contains(text, "sagent_ctl_heartbeat_success_ratio 0.6667") {
		t.Fatalf("成功率应为 0.6667，实际：\n%s", text)
	}
	// 平均时延 = (50+100)/2 = 75ms = 0.0750s
	if !strings.Contains(text, "sagent_ctl_heartbeat_latency_seconds 0.0750") {
		t.Fatalf("平均时延应为 0.0750s，实际：\n%s", text)
	}
}

func TestMeterTaskCounters(t *testing.T) {
	m := &Meter{}
	m.addTask(true, 200*time.Millisecond)
	m.addTask(true, 400*time.Millisecond)
	m.addTask(false, 0)

	text := m.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_task_total 3") {
		t.Fatalf("task_total 应为 3")
	}
	if !strings.Contains(text, "sagent_ctl_task_fail_total 1") {
		t.Fatalf("task_fail_total 应为 1")
	}
	// 成功任务平均时延 = (200+400)/2 = 300ms = 0.3000s
	if !strings.Contains(text, "sagent_ctl_task_latency_average_seconds 0.3000") {
		t.Fatalf("平均任务时延应为 0.3000s，实际：\n%s", text)
	}
	// 失败任务不进入时延均值
	if strings.Contains(text, "sagent_ctl_task_latency_average_seconds 0.1333") {
		t.Fatalf("失败任务不应计入时延均值")
	}
}

func TestMeterReportBytes(t *testing.T) {
	m := &Meter{}
	m.addReportBytes(128)
	m.addReportBytes(512)
	text := m.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_report_bytes_total 640") {
		t.Fatalf("report_bytes_total 应为 640，实际：\n%s", text)
	}
}

func TestMeterEndpointSwitchDedup(t *testing.T) {
	m := &Meter{}
	m.setEndpoint("http://l0-a:8080")
	m.setEndpoint("http://l0-a:8080") // 同端点不应重复计数
	if got := m.PrometheusText(); !strings.Contains(got, "sagent_ctl_endpoint_switch_total 1") {
		t.Fatalf("切换初始化为 1（无→主），同端点不增：\n%s", got)
	}
	m.setEndpoint("http://l0-b:8080") // 切备端点 → +1
	if got := m.PrometheusText(); !strings.Contains(got, "sagent_ctl_endpoint_switch_total 2") {
		t.Fatalf("切备端后应为 2")
	}
}

func TestMeterRegisterCounters(t *testing.T) {
	m := &Meter{}
	m.addRegister(true)
	m.addRegister(false)
	text := m.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_register_total 2") {
		t.Fatalf("register_total 应为 2")
	}
	if !strings.Contains(text, "sagent_ctl_register_fail_total 1") {
		t.Fatalf("register_fail_total 应为 1")
	}
}

// 空态输出不 panic，且始终含 online 标记。
func TestMeterPrometheusEmptyNoPanic(t *testing.T) {
	m := &Meter{}
	text := m.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_online 1") {
		t.Fatalf("空态应含 online 标记，实际：\n%s", text)
	}
	// 空态不应输出需要非零前置的指标（latency/ratio）
	if strings.Contains(text, "heartbeat_success_ratio 0") || strings.Contains(text, "heartbeat_latency_seconds 0.") {
		t.Fatalf("空态不应输出其余指标，实际：\n%s", text)
	}
}

// client 埋点不改变既有语义：失败心跳 task 计数与成功区分，且 /metrics 文本可聚合。
func TestClientBeatMetersWire(t *testing.T) {
	c := testConfigResponseClient(t)
	c.meter = &Meter{}
	if !c.beat() {
		t.Fatal("heartbeat transport failed")
	}
	text := c.meter.PrometheusText()
	if !strings.Contains(text, "sagent_ctl_heartbeat_total 1") || !strings.Contains(text, "sagent_ctl_heartbeat_fail_total 0") {
		t.Fatalf("beat 后应 1 成功 0 失败：\n%s", text)
	}
	if !strings.Contains(text, "sagent_ctl_endpoint_switch_total 1") {
		t.Fatalf("beat 命中端点应计切换基数 1")
	}
}