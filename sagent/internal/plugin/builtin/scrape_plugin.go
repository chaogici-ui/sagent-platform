package builtin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/constants"
)

// ScrapePlugin prometheus_scrape 通用抓取插件
type ScrapePlugin struct {
	targets  []ScrapeTarget
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// ScrapeTarget 抓取目标
type ScrapeTarget struct {
	Name    string
	URL     string // http://127.0.0.1:port/metrics 或 unix:///path/to/socket
	Timeout time.Duration
	Labels  map[string]string
}

// NewScrapePlugin 创建抓取插件
func NewScrapePlugin(targets []ScrapeTarget, interval time.Duration) *ScrapePlugin {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &ScrapePlugin{
		targets:  targets,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Name 插件名
func (s *ScrapePlugin) Name() string {
	return "prometheus_scrape"
}

// Start 启动抓取
func (s *ScrapePlugin) Start(metricsCh chan<- []Metric) {
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

// scrape 抓取所有目标
func (s *ScrapePlugin) scrape(metricsCh chan<- []Metric) {
	for _, t := range s.targets {
		timeout := t.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}

		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(t.URL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scrape %s: %v\n", t.Name, err)
			// 上报 up=0
			metricsCh <- []Metric{{
				Name: "up", Value: 0, Type: constants.MetricTypeGauge,
				Help:   "Target up/down status",
				Labels: map[string]string{"target": t.Name, "url": t.URL},
			}}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		metrics := parseScrapeMetrics(string(body))
		for i := range metrics {
			if metrics[i].Labels == nil {
				metrics[i].Labels = make(map[string]string)
			}
			metrics[i].Labels["target"] = t.Name
			for k, v := range t.Labels {
				metrics[i].Labels[k] = v
			}
		}
		// up=1
		metrics = append(metrics, Metric{
			Name: "up", Value: 1, Type: constants.MetricTypeGauge,
			Help:   "Target up/down status",
			Labels: map[string]string{"target": t.Name, "url": t.URL},
		})
		metricsCh <- metrics
	}
}

// parseScrapeMetrics 解析 Prometheus 文本格式
func parseScrapeMetrics(body string) []Metric {
	var metrics []Metric
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		metric, err := parseMetricLine(line)
		if err != nil {
			continue
		}
		if metric.Type == "" {
			metric.Type = constants.MetricTypeGauge
		}
		metrics = append(metrics, metric)
	}
	return metrics
}

// ScrapeConfig 抓取插件配置
type ScrapeConfig struct {
	Enabled  bool              `yaml:"enabled"`
	Interval time.Duration     `yaml:"interval"`
	Targets  []ScrapeTargetRaw `yaml:"targets"`
}

type ScrapeTargetRaw struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Timeout time.Duration     `yaml:"timeout"`
	Labels  map[string]string `yaml:"labels"`
}

func ToScrapeTargets(raw []ScrapeTargetRaw) []ScrapeTarget {
	targets := make([]ScrapeTarget, len(raw))
	for i, r := range raw {
		targets[i] = ScrapeTarget{
			Name: r.Name, URL: r.URL,
			Timeout: r.Timeout, Labels: r.Labels,
		}
	}
	return targets
}
