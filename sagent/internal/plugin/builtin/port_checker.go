package builtin

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
)

// PortChecker 网络探测插件：TCP/HTTP/ping（HA-3：支持主端点 + 等效副本的故障切换）
type PortChecker struct {
	targets  []PortTarget
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu             sync.Mutex
	active         map[string]int // target.Name -> 当前活跃端点下标（0=主）
	primaryHealthy map[string]int // 激活在备端时，主端连续健康探测计数（防抖回切）
}

// PortTarget 探测目标
type PortTarget struct {
	Name     string
	Type     string   // tcp, http
	Address  string   // host:port or http://host:port/path；主探测端点
	Replicas []string // HA-3：等效备端（如同一服务在 A/B 两机监听同端口）；空=单端点原行为
	Timeout  time.Duration
	Labels   map[string]string
}

// endpoints 返回全部等效端点（主端点在前）。
func (t PortTarget) endpoints() []string {
	eps := make([]string, 0, 1+len(t.Replicas))
	eps = append(eps, t.Address)
	eps = append(eps, t.Replicas...)
	return eps
}

// NewPortChecker 创建网络探测插件
func NewPortChecker(targets []PortTarget, interval time.Duration) *PortChecker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &PortChecker{
		targets:        targets,
		interval:       interval,
		stopCh:         make(chan struct{}),
		active:         make(map[string]int),
		primaryHealthy: make(map[string]int),
	}
}

// activeIndex 当前活跃端点下标（默认主端点 0）。
func (p *PortChecker) activeIndex(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i, ok := p.active[name]; ok {
		return i
	}
	return 0
}

// Name 插件名
func (p *PortChecker) Name() string {
	return "port_checker"
}

// Start 启动探测
func (p *PortChecker) Start(metricsCh chan<- MetricBatch) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.check(metricsCh)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.check(metricsCh)
			case <-p.stopCh:
				return
			}
		}
	}()
}

// Stop 停止探测
func (p *PortChecker) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

// check 执行一次所有目标的探测（HA-3：多等效端点时支持故障切换与回切）
func (p *PortChecker) check(metricsCh chan<- MetricBatch) {
	for _, g := range p.targets {
		timeout := g.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		eps := g.endpoints()
		multi := len(eps) > 1

		if g.Type != "http" && g.Type != "tcp" {
			source := SnapshotSource(p.Name(), g.Name, g.Type, eps[0], g.Labels["resource_id"])
			if !sendBatch(metricsCh, p.stopCh, snapshotBatch(source, nil, p.interval, fmt.Errorf("invalid probe type %q", g.Type))) {
				return
			}
			continue
		}

		idx := p.activeIndex(g.Name)
		if idx < 0 || idx >= len(eps) {
			idx = 0
		}

		latency, success := p.probeTarget(g, eps[idx], timeout)

		// —— 探活 + 切换决策 ——
		if success > 0 {
			// 活跃健康：若激活在备端且主端(0)恢复，按防抖回切主端
			if multi && idx != 0 && p.probeTargetOK(g, eps[0], timeout) {
				if p.bumpPrimary(g.Name) >= failbackThreshold {
					p.switchEndpoint(metricsCh, g, eps, idx, 0)
					idx = 0
					p.resetPrimary(g.Name)
					latency, success = p.probeTarget(g, eps[idx], timeout) // 回切主端后探主端
				}
			} else {
				p.resetPrimary(g.Name)
			}
		} else {
			// 活跃失败：探等效备端，切到第一个健康者（优先低序号）
			p.resetPrimary(g.Name)
			if multi {
				for i, u := range eps {
					if i == idx {
						continue
					}
					if p.probeTargetOK(g, u, timeout) {
						p.switchEndpoint(metricsCh, g, eps, idx, i)
						idx = i
						latency, success = p.probeTarget(g, eps[idx], timeout)
						break
					}
				}
			}
			// 全部端点失败：保持原活跃 idx，success=0 即为失败结果
		}

		source := SnapshotSource(p.Name(), g.Name, g.Type, eps[idx], g.Labels["resource_id"])
		labels := map[string]string{
			"target": g.Address, "type": g.Type, "name": g.Name,
			"endpoint": eps[idx], "endpoint_index": fmt.Sprint(idx),
		}
		for k, v := range g.Labels {
			labels[k] = v
		}

		metrics := []Metric{
			{Name: "port_check_up", Value: success, Help: "Port check status (1=up 0=down)", Type: constants.MetricTypeGauge, Labels: labels},
			{Name: "port_check_latency_seconds", Value: latency, Help: "Port check latency in seconds", Type: constants.MetricTypeGauge, Labels: labels},
		}
		if !sendBatch(metricsCh, p.stopCh, snapshotBatch(source, metrics, p.interval, nil)) {
			return
		}
	}
}

// probeTarget 探测单个端点并返回（延迟, 成功率）。仅处理 http/tcp，调用前需校验类型。
func (p *PortChecker) probeTarget(g PortTarget, address string, timeout time.Duration) (latency, success float64) {
	if g.Type == "http" {
		return p.checkHTTP(address, timeout)
	}
	return p.checkTCP(address, timeout)
}

// probeTargetOK 单端点健康判定（探活用，丢弃延迟）。
func (p *PortChecker) probeTargetOK(g PortTarget, address string, timeout time.Duration) bool {
	_, success := p.probeTarget(g, address, timeout)
	return success > 0
}

// switchEndpoint 切换活跃端点并记录切换事件（计数器快照）。
// 单端点目标不会触发（multi 才调用），故输出快照数不受影响。
func (p *PortChecker) switchEndpoint(metricsCh chan<- MetricBatch, g PortTarget, eps []string, from, to int) {
	if from == to {
		return
	}
	p.active[g.Name] = to

	labels := map[string]string{
		"target": g.Name,
		"from":   fmt.Sprintf("%d", from),
		"to":     fmt.Sprintf("%d", to),
	}
	switchMetrics := []Metric{
		{Name: "port_checker_endpoint_switch_total", Value: 1, Type: constants.MetricTypeCounter,
			Help: "Port checker endpoint failover switch count (HA-3)", Labels: labels},
	}
	switchSource := SnapshotSource(p.Name()+"_switch", g.Name, eps[to], g.Labels["resource_id"])
	_ = sendBatch(metricsCh, p.stopCh, snapshotBatch(switchSource, switchMetrics, p.interval, nil))
}

// bumpPrimary 主端连续健康计数 +1；返回当前计数。
func (p *PortChecker) bumpPrimary(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.primaryHealthy[name]++
	return p.primaryHealthy[name]
}

// resetPrimary 清零主端连续健康计数。
func (p *PortChecker) resetPrimary(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.primaryHealthy, name)
}

func (p *PortChecker) checkTCP(address string, timeout time.Duration) (latency, success float64) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return time.Since(start).Seconds(), 0
	}
	conn.Close()
	return time.Since(start).Seconds(), 1
}

func (p *PortChecker) checkHTTP(url string, timeout time.Duration) (latency, success float64) {
	start := time.Now()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return time.Since(start).Seconds(), 0
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return time.Since(start).Seconds(), 1
	}
	return time.Since(start).Seconds(), 0
}
