package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
)

// ExecScripts 自定义脚本执行器（exec 插件）
type ExecScripts struct {
	scripts   []config.ExecScript
	stopCh    chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	running   bool
	metricsCh chan<- []Metric // 保存以便重启
}

// NewExecScripts 创建脚本执行器
func NewExecScripts(cfg config.CustomScriptsConfig) *ExecScripts {
	return &ExecScripts{
		scripts: cfg.Scripts,
		stopCh:  make(chan struct{}),
	}
}

// Name 插件名
func (e *ExecScripts) Name() string {
	return "custom_scripts"
}

// PluginName 实现 server.PluginHandle 接口
func (e *ExecScripts) PluginName() string {
	return "custom_scripts"
}

// Signal 实现 server.PluginHandle 接口（reload = 停旧 → 重新拉起所有脚本）
func (e *ExecScripts) Signal(sig os.Signal) error {
	e.Stop()
	return e.Start()
}

// Running 实现 server.PluginHandle 接口
func (e *ExecScripts) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// SetMetricsCh 设置指标通道（在首次 Start 前调用）
func (e *ExecScripts) SetMetricsCh(ch chan<- []Metric) {
	e.metricsCh = ch
}

// Start 启动所有脚本（实现 server.PluginHandle 接口）
func (e *ExecScripts) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return nil
	}
	if e.metricsCh == nil {
		return fmt.Errorf("custom_scripts: not initialized, call SetMetricsCh first")
	}
	e.running = true
	e.doStart()
	return nil
}

func (e *ExecScripts) doStart() {
	e.stopCh = make(chan struct{})

	for _, script := range e.scripts {
		s := script // capture
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.runLoop(s, e.metricsCh)
		}()
	}
}

// Stop 停止所有脚本（可重入，支持后续 Start 重新拉起）
func (e *ExecScripts) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	close(e.stopCh)
	e.mu.Unlock()

	e.wg.Wait()
	return nil
}

// runLoop 单个脚本的执行循环
func (e *ExecScripts) runLoop(script config.ExecScript, metricsCh chan<- []Metric) {
	interval := script.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timeout := script.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// 立即执行一次
	e.executeOnce(script, metricsCh, timeout)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.executeOnce(script, metricsCh, timeout)
		case <-e.stopCh:
			return
		}
	}
}

// executeOnce 执行一次脚本，解析 stdout 中的 Prometheus 指标
func (e *ExecScripts) executeOnce(script config.ExecScript, metricsCh chan<- []Metric, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", script.Command)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		fmt.Printf("exec_scripts: %s failed: %v\n", script.Name, err)
		return
	}

	// 解析 Prometheus 文本格式
	metrics := parsePrometheusText(stdout.String())

	// 注入标签
	resourceID := script.ResourceID
	for i := range metrics {
		if metrics[i].Labels == nil {
			metrics[i].Labels = make(map[string]string)
		}
		metrics[i].Labels["resource_id"] = resourceID
		for k, v := range script.Labels {
			metrics[i].Labels[k] = v
		}
	}

	metricsCh <- metrics
}

// parsePrometheusText 解析 Prometheus exposition 格式
func parsePrometheusText(text string) []Metric {
	var metrics []Metric
	lines := strings.Split(text, "\n")
	var currentHelp string
	var currentType string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "# HELP ") {
			currentHelp = strings.TrimPrefix(line, "# HELP ")
			if idx := strings.Index(currentHelp, " "); idx > 0 {
				currentHelp = currentHelp[idx+1:]
			}
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			typeInfo := strings.TrimPrefix(line, "# TYPE ")
			parts := strings.Fields(typeInfo)
			if len(parts) >= 2 {
				currentType = parts[1]
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		// 解析: name{labels} value [timestamp]
		metric, err := parseMetricLine(line)
		if err != nil {
			continue
		}
		if metric.Help == "" {
			metric.Help = currentHelp
		}
		if metric.Type == "" {
			metric.Type = constants.MetricType(currentType)
		}
		if metric.Type == "" {
			metric.Type = constants.MetricTypeGauge
		}
		metrics = append(metrics, metric)
	}

	return metrics
}

// parseMetricLine 解析单行 Prometheus 指标
func parseMetricLine(line string) (Metric, error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return Metric{}, fmt.Errorf("parse error")
	}

	namePart := parts[0]
	valStr := parts[1]

	metric := Metric{}

	// 分离 name 和 labels
	if idx := strings.Index(namePart, "{"); idx >= 0 {
		metric.Name = namePart[:idx]
		labelStr := namePart[idx+1:]
		if lastBrace := strings.LastIndex(labelStr, "}"); lastBrace >= 0 {
			labelStr = labelStr[:lastBrace]
		}
		metric.Labels = parseKeyValueLabels(labelStr)
	} else {
		metric.Name = namePart
		metric.Labels = make(map[string]string)
	}

	fmt.Sscanf(valStr, "%f", &metric.Value)

	return metric, nil
}

// parseKeyValueLabels 解析 key="value" 格式的标签
func parseKeyValueLabels(s string) map[string]string {
	labels := make(map[string]string)
	if s == "" {
		return labels
	}

	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			key := kv[0]
			val := strings.Trim(kv[1], "\"")
			labels[key] = val
		}
	}
	return labels
}
