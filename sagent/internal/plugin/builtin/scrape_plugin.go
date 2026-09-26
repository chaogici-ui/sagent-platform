package builtin

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
)

// ScrapePlugin prometheus_scrape 通用抓取插件（HA-3：支持主端点 + 等效副本的故障切换）
type ScrapePlugin struct {
	targets  []ScrapeTarget
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu             sync.Mutex
	active         map[string]int // target.Name -> 当前活跃端点下标（0=主）
	primaryHealthy map[string]int // 激活在备端时，主端连续健康探测计数（防抖回切）
}

// ScrapeTarget 抓取目标
type ScrapeTarget struct {
	Name     string
	URL      string   // 主抓取端点 http://.../metrics 或 unix:///path，等价端点名单首
	Replicas []string // HA-3：等效备端（如 mysql exporter 部署在 A/B 两机双写同一库）；空=单端点原行为
	Timeout  time.Duration
	Labels   map[string]string
}

// endpoints 返回全部等效端点（主端点在前）。
func (t ScrapeTarget) endpoints() []string {
	eps := make([]string, 0, 1+len(t.Replicas))
	eps = append(eps, t.URL)
	eps = append(eps, t.Replicas...)
	return eps
}

// NewScrapePlugin 创建抓取插件
func NewScrapePlugin(targets []ScrapeTarget, interval time.Duration) *ScrapePlugin {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &ScrapePlugin{
		targets:        targets,
		interval:       interval,
		stopCh:         make(chan struct{}),
		active:         make(map[string]int),
		primaryHealthy: make(map[string]int),
	}
}

// activeIndex 当前活跃端点下标（默认主端点 0）。
func (s *ScrapePlugin) activeIndex(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.active[name]; ok {
		return i
	}
	return 0
}

// Name 插件名
func (s *ScrapePlugin) Name() string {
	return "prometheus_scrape"
}

// Start 启动抓取
func (s *ScrapePlugin) Start(metricsCh chan<- MetricBatch) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.scrape(metricsCh)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.scrape(metricsCh)
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止抓取
func (s *ScrapePlugin) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

// HA-3 故障切换参数。failbackThreshold：主端点恢复后连续健康 N 个抓取周期才回切，
// 避免主备都在线时在两个端点间来回抖动（防抖）。
const failbackThreshold = 2

// scrape 抓取所有目标（HA-3：多等效端点时支持故障切换与回切）
func (s *ScrapePlugin) scrape(metricsCh chan<- MetricBatch) {
	for _, t := range s.targets {
		timeout := t.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		eps := t.endpoints()
		multi := len(eps) > 1

		idx := s.activeIndex(t.Name)
		if idx < 0 || idx >= len(eps) {
			idx = 0
		}

		client := &http.Client{Timeout: timeout}
		body, err := fetchPrometheusText(client, eps[idx])

		// —— 探活 + 切换决策 ——
		if err == nil {
			// 活跃健康：若激活在备端且主端(0)恢复，按防抖回切主端
			if multi && idx != 0 && probeUp(client, eps[0], timeout) {
				if s.bumpPrimary(t.Name) >= failbackThreshold {
					s.switchEndpoint(metricsCh, t, eps, idx, 0)
					idx = 0
					s.resetPrimary(t.Name)
					body, err = fetchPrometheusText(client, eps[idx]) // 回切主端后抓主端
				}
			} else {
				s.resetPrimary(t.Name)
			}
		} else {
			// 活跃失败：探等效备端，切到第一个健康者（优先低序号）
			s.resetPrimary(t.Name)
			if multi {
				for i, u := range eps {
					if i == idx {
						continue
					}
					if probeUp(client, u, timeout) {
						s.switchEndpoint(metricsCh, t, eps, idx, i)
						idx = i
						body, err = fetchPrometheusText(client, eps[idx])
						break
					}
				}
			}
			// 全部端点失败：保持原活跃 idx，body/err 即为失败结果
		}

		// —— 产出业务 body 快照（来自当前活跃端点） ——
		var metrics []Metric
		if err == nil {
			metrics, err = parsePrometheusText(body)
		}
		for i := range metrics {
			metrics[i].Labels["target"] = t.Name
			for k, v := range t.Labels {
				metrics[i].Labels[k] = v
			}
		}
		source := SnapshotSource(s.Name(), t.Name, eps[idx], t.Labels["resource_id"])
		if !sendBatch(metricsCh, s.stopCh, snapshotBatch(source, metrics, s.interval, err)) {
			return
		}

		// 探测状态独立成来源；up=0 不能替换或续期上次成功的业务 body。
		// endpoint 标签披露当前活跃端点，便于观测故障切换落点
		up := 1.0
		if err != nil {
			up = 0
		}
		labels := map[string]string{"target": t.Name, "url": eps[idx], "endpoint_index": fmt.Sprint(idx)}
		for k, v := range t.Labels {
			labels[k] = v
		}
		probe := []Metric{{Name: "up", Value: up, Type: constants.MetricTypeGauge, Help: "Target up/down status (active endpoint)", Labels: labels}}
		probeSource := SnapshotSource(s.Name()+"_probe", t.Name, eps[idx], t.Labels["resource_id"])
		if !sendBatch(metricsCh, s.stopCh, snapshotBatch(probeSource, probe, s.interval, nil)) {
			return
		}
	}
}

// probeUp 轻量探活单个端点（GET 成功 2xx，丢弃 body）。
func probeUp(client *http.Client, address string, timeout time.Duration) bool {
	_, err := fetchPrometheusText(client, address)
	return err == nil
}

// switchEndpoint 切换活跃端点并记录切换事件（计数器 + 活跃端点 gauge 快照）。
// 单端点目标不会触发（multi 才调用），故输出快照数不受影响。
func (s *ScrapePlugin) switchEndpoint(metricsCh chan<- MetricBatch, t ScrapeTarget, eps []string, from, to int) {
	if from == to {
		return
	}
	s.active[t.Name] = to

	labels := map[string]string{
		"target": t.Name,
		"from":   fmt.Sprintf("%d", from),
		"to":     fmt.Sprintf("%d", to),
	}
	switchMetrics := []Metric{
		{Name: "exporter_endpoint_switch_total", Value: 1, Type: constants.MetricTypeCounter,
			Help: "Exporter endpoint failover switch count (HA-3)", Labels: labels},
	}
	switchSource := SnapshotSource(s.Name()+"_switch", t.Name, eps[to], t.Labels["resource_id"])
	_ = sendBatch(metricsCh, s.stopCh, snapshotBatch(switchSource, switchMetrics, s.interval, nil))
}

// bumpPrimary 主端连续健康计数 +1；返回当前计数。
func (s *ScrapePlugin) bumpPrimary(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primaryHealthy[name]++
	return s.primaryHealthy[name]
}

// resetPrimary 清零主端连续健康计数。
func (s *ScrapePlugin) resetPrimary(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.primaryHealthy, name)
}

func fetchPrometheusText(client *http.Client, address string) (string, error) {
	resp, err := client.Get(address)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
