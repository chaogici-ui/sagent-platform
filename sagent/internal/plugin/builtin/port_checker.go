package builtin

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
)

// PortChecker 网络探测插件：TCP/HTTP/ping
type PortChecker struct {
	targets  []PortTarget
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// PortTarget 探测目标
type PortTarget struct {
	Name    string
	Type    string // tcp, http
	Address string // host:port or http://host:port/path
	Timeout time.Duration
	Labels  map[string]string
}

// NewPortChecker 创建网络探测插件
func NewPortChecker(targets []PortTarget, interval time.Duration) *PortChecker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &PortChecker{
		targets:  targets,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Name 插件名
func (p *PortChecker) Name() string {
	return "port_checker"
}

// Start 启动探测
func (p *PortChecker) Start(metricsCh chan<- []Metric) {
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

// check 执行一次所有目标的探测
func (p *PortChecker) check(metricsCh chan<- []Metric) {
	for _, t := range p.targets {
		timeout := t.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}

		var success float64 = 1
		var latency float64

		switch t.Type {
		case "http":
			latency, success = p.checkHTTP(t.Address, timeout)
		case "tcp":
			latency, success = p.checkTCP(t.Address, timeout)
		default:
			continue
		}

		labels := map[string]string{"target": t.Address, "type": t.Type, "name": t.Name}
		for k, v := range t.Labels {
			labels[k] = v
		}

		metricsCh <- []Metric{
			{Name: "port_check_up", Value: success, Help: "Port check status (1=up 0=down)", Type: constants.MetricTypeGauge, Labels: labels},
			{Name: "port_check_latency_seconds", Value: latency, Help: "Port check latency in seconds", Type: constants.MetricTypeGauge, Labels: labels},
		}
	}
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

// ParsePortTargetsFromConfig 从配置解析探测目标
type PortCheckerConfig struct {
	Enabled  bool            `yaml:"enabled"`
	Interval time.Duration   `yaml:"interval"`
	Targets  []PortTargetRaw `yaml:"targets"`
}

type PortTargetRaw struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`
	Address string            `yaml:"address"`
	Timeout time.Duration     `yaml:"timeout"`
	Labels  map[string]string `yaml:"labels"`
}

func ToPortTargets(raw []PortTargetRaw) []PortTarget {
	targets := make([]PortTarget, len(raw))
	for i, r := range raw {
		targets[i] = PortTarget{
			Name: r.Name, Type: r.Type, Address: r.Address,
			Timeout: r.Timeout, Labels: r.Labels,
		}
	}
	return targets
}

// Ensure import fmt is used
var _ = fmt.Sprintf
