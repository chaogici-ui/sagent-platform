package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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
	metricsCh chan<- MetricBatch // 保存以便重启
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
func (e *ExecScripts) SetMetricsCh(ch chan<- MetricBatch) {
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
func (e *ExecScripts) runLoop(script config.ExecScript, metricsCh chan<- MetricBatch) {
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
func (e *ExecScripts) executeOnce(script config.ExecScript, metricsCh chan<- MetricBatch, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", script.Command)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	source := SnapshotSource(e.Name(), script.Name, script.ResourceID)
	if err := cmd.Run(); err != nil {
		fmt.Printf("exec_scripts: %s failed: %v\n", script.Name, err)
		sendBatch(metricsCh, e.stopCh, snapshotBatch(source, nil, script.Interval, err))
		return
	}

	metrics, err := parsePrometheusText(stdout.String())

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

	sendBatch(metricsCh, e.stopCh, snapshotBatch(source, metrics, script.Interval, err))
}

// parsePrometheusText 拒绝不完整或非法样本，避免部分 body 刷新快照。
func parsePrometheusText(text string) ([]Metric, error) {
	var metrics []Metric
	helps := map[string]string{}
	types := map[string]constants.MetricType{}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			name, help, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !ok || !metricNamePattern.MatchString(name) {
				return nil, fmt.Errorf("invalid HELP at line %d", n+1)
			}
			helps[name] = strings.NewReplacer("\\n", "\n", "\\\\", "\\").Replace(help)
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			parts := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(parts) != 2 || !metricNamePattern.MatchString(parts[0]) {
				return nil, fmt.Errorf("invalid TYPE at line %d", n+1)
			}
			switch parts[1] {
			case "counter", "gauge", "histogram", "summary", "untyped":
				types[parts[0]] = constants.MetricType(parts[1])
			default:
				return nil, fmt.Errorf("invalid metric type at line %d", n+1)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		metric, err := parseMetricLine(line)
		if err != nil {
			return nil, fmt.Errorf("metric line %d: %w", n+1, err)
		}
		metrics = append(metrics, metric)
	}
	for i := range metrics {
		metric := &metrics[i]
		family := metric.Name
		if _, declared := types[family]; !declared {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if !strings.HasSuffix(metric.Name, suffix) {
					continue
				}
				base := strings.TrimSuffix(metric.Name, suffix)
				kind := types[base]
				if kind == "histogram" || kind == "summary" && suffix != "_bucket" {
					family, metric.Family = base, base
					break
				}
			}
		}
		metric.Help, metric.Type = helps[family], types[family]
		if metric.Type == "" {
			metric.Type = constants.MetricTypeGauge
		}
	}
	return metrics, nil
}

var metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func parseMetricLine(line string) (Metric, error) {
	idx := strings.IndexAny(line, "{ \t")
	if idx <= 0 || !metricNamePattern.MatchString(line[:idx]) {
		return Metric{}, fmt.Errorf("invalid metric name or missing value")
	}
	metric := Metric{Name: line[:idx], Labels: map[string]string{}}
	rest := line[idx:]
	if rest[0] == '{' {
		quoted, escaped, end := false, false, -1
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if escaped {
				escaped = false
			} else if quoted && c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = !quoted
			} else if !quoted && c == '}' {
				end = i
				break
			}
		}
		if end < 0 {
			return Metric{}, fmt.Errorf("unterminated labels")
		}
		labels, err := parseKeyValueLabels(rest[1:end])
		if err != nil {
			return Metric{}, err
		}
		metric.Labels, rest = labels, rest[end+1:]
	}
	if len(rest) == 0 || rest[0] != ' ' && rest[0] != '\t' {
		return Metric{}, fmt.Errorf("missing value separator")
	}
	parts := strings.Fields(rest)
	if len(parts) < 1 || len(parts) > 2 {
		return Metric{}, fmt.Errorf("invalid sample fields")
	}
	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return Metric{}, fmt.Errorf("invalid sample value")
	}
	if len(parts) == 2 {
		if _, err := strconv.ParseInt(parts[1], 10, 64); err != nil {
			return Metric{}, fmt.Errorf("invalid sample timestamp")
		}
	}
	metric.Value = value
	return metric, nil
}

func parseKeyValueLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		key, rest, ok := strings.Cut(s, "=")
		key, rest = strings.TrimSpace(key), strings.TrimSpace(rest)
		if !ok || !labelNamePattern.MatchString(key) || len(rest) == 0 || rest[0] != '"' {
			return nil, fmt.Errorf("invalid label")
		}
		if _, exists := labels[key]; exists {
			return nil, fmt.Errorf("duplicate label %s", key)
		}
		var value strings.Builder
		end := -1
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if c == '"' {
				end = i
				break
			}
			if c == '\\' {
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("unfinished label escape")
				}
				c = rest[i]
				switch c {
				case 'n':
					c = '\n'
				case '\\', '"':
				default:
					return nil, fmt.Errorf("invalid label escape")
				}
			}
			value.WriteByte(c)
		}
		if end < 0 {
			return nil, fmt.Errorf("unterminated label")
		}
		labels[key] = value.String()
		s = strings.TrimSpace(rest[end+1:])
		if s != "" {
			if s[0] != ',' {
				return nil, fmt.Errorf("missing label separator")
			}
			s = s[1:]
		}
	}
	return labels, nil
}
