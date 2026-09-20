package builtin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/subprocess"
)

// MySQLProbe 远程 MySQL 采集插件
type MySQLProbe struct {
	targets     []config.MySQLTarget
	exporter    *subprocess.Plugin
	exporterBin string          // 存储以便 restart
	metricsCh   chan<- []Metric // 存储以便 restart
	interval    time.Duration
	client      *http.Client
	stopCh      chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	running     bool
}

// NewMySQLProbe 创建 MySQL 采集器
func NewMySQLProbe(cfg config.MySQLProbeConfig) *MySQLProbe {
	return &MySQLProbe{
		targets:  cfg.Targets,
		interval: 30 * time.Second,
		stopCh:   make(chan struct{}),
	}
}

// Name 插件名
func (m *MySQLProbe) Name() string {
	return "mysql_probe"
}

// PluginName 实现 server.PluginHandle 接口
func (m *MySQLProbe) PluginName() string {
	return "mysql_probe"
}

// Signal 实现 server.PluginHandle 接口（reload = 停旧 exporter → 重新拉起）
func (m *MySQLProbe) Signal(sig os.Signal) error {
	m.Stop()
	return m.Start()
}

// Running 实现 server.PluginHandle 接口
func (m *MySQLProbe) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// StartWith 首次启动采集（存入参数供后续 restart 使用）
func (m *MySQLProbe) StartWith(exporterBin string, metricsCh chan<- []Metric) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}
	if len(m.targets) == 0 {
		return nil
	}

	m.exporterBin = exporterBin
	m.metricsCh = metricsCh
	m.running = true
	m.stopCh = make(chan struct{})
	return m.doStart()
}

// Start 无参版本，实现 server.PluginHandle 接口（用于启停恢复）
func (m *MySQLProbe) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}
	if m.exporterBin == "" || m.metricsCh == nil {
		return fmt.Errorf("mysql_probe: not initialized, call Start(bin, ch) first")
	}
	m.running = true
	m.stopCh = make(chan struct{})
	return m.doStart()
}

func (m *MySQLProbe) doStart() error {
	if len(m.targets) == 0 {
		return nil
	}

	// 多目标模式：提取第一个目标的凭证写入 my.cnf
	// /probe?target= 只接受 host:port，凭证从 my.cnf 读取
	var user, pass string
	if len(m.targets) > 0 {
		user, pass = parseDSN(m.targets[0].DSN)
	}
	tmpFile := filepath.Join(os.TempDir(), "sagent-my.cnf")
	os.WriteFile(tmpFile, []byte(fmt.Sprintf("[client]\nuser=%s\npassword=%s\n", user, pass)), 0600)

	exporterArgs := []string{
		fmt.Sprintf("--config.my-cnf=%s", tmpFile),
		"--web.listen-address=127.0.0.1:19104",
		"--collect.global_status",
		"--collect.global_variables",
		"--collect.slave_status",
	}
	m.exporter = subprocess.New("mysqld_exporter", m.exporterBin, exporterArgs)
	if err := m.exporter.Start(); err != nil {
		return fmt.Errorf("start mysqld_exporter: %w", err)
	}

	// HTTP 客户端
	m.client = &http.Client{
		Timeout: 10 * time.Second,
	}

	// 定期抓取
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// 等 exporter 启动
		time.Sleep(5 * time.Second)
		m.scrape(m.metricsCh)

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.scrape(m.metricsCh)
			case <-m.stopCh:
				return
			}
		}
	}()

	return nil
}

// Stop 停止采集（可重入）
func (m *MySQLProbe) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	close(m.stopCh)
	m.mu.Unlock()

	m.wg.Wait()
	if m.exporter != nil {
		m.exporter.Stop()
	}
	return nil
}

// scrape 抓取 mysqld_exporter 指标，为每个 target 注入 resource_id
func (m *MySQLProbe) scrape(metricsCh chan<- []Metric) {
	for _, target := range m.targets {
		hostPort := dsnHostPort(target.DSN)
		if hostPort == "" {
			continue
		}

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:19104/probe?target=%s", hostPort))
		if err != nil {
			fmt.Printf("mysql_probe: scrape %s: %v\n", target.ResourceID, err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Printf("mysql_probe: read %s: %v\n", target.ResourceID, err)
			continue
		}

		// 解析 Prometheus 格式，注入 remote resource_id
		metrics := parseMySQLMetrics(string(body), target.ResourceID)
		metricsCh <- metrics
	}
}

// parseMySQLMetrics 解析 mysqld_exporter 输出的 Prometheus 格式
func parseMySQLMetrics(body string, remoteResourceID string) []Metric {
	var metrics []Metric
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析: metric_name{labels} value
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}
		name := parts[0]
		valStr := parts[1]

		// 分离 metric name 和 labels
		var labels map[string]string
		if idx := strings.Index(name, "{"); idx >= 0 {
			labelStr := name[idx+1 : len(name)-1]
			name = name[:idx]
			labels = parseLabels(labelStr)
		}
		if labels == nil {
			labels = make(map[string]string)
		}

		// 注入远程资源 ID
		labels["resource_id"] = remoteResourceID
		labels["resource_type"] = "mysql"

		var value float64
		fmt.Sscanf(valStr, "%f", &value)

		metricType := string(constants.MetricTypeGauge)
		if strings.HasSuffix(name, "_total") {
			metricType = string(constants.MetricTypeCounter)
		}

		// 注入远程资源 ID
		if labels == nil {
			labels = make(map[string]string)
		}
		labels["remote_resource_id"] = remoteResourceID

		metrics = append(metrics, Metric{
			Name:   name,
			Value:  value,
			Help:   "",
			Type:   constants.MetricType(metricType),
			Labels: labels,
		})
	}
	return metrics
}

// parseLabels 解析 Prometheus 标签字符串
func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	pairs := strings.Split(s, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			key := strings.TrimSpace(kv[0])
			val := strings.Trim(kv[1], "\"")
			labels[key] = val
		}
	}
	return labels
}

// buildExporterConfig 生成 mysqld_exporter 的 my.cnf 多目标配置
func buildExporterConfig(targets []config.MySQLTarget) string {
	// 生成临时 my.cnf 内容
	// mysqld_exporter 支持 [client] 段，但多目标需要每个目标一个 section
	// 使用 [client] + 环境变量切换，或者直接生成配置文件
	// 简化：使用默认 [client] 段
	var sb strings.Builder
	sb.WriteString("[client]\n")
	if len(targets) > 0 {
		// 取第一个目标的 DSN 解析
		dsn := targets[0].DSN
		// 格式: root:pass@tcp(host:port)/
		if userPass, after, ok := strings.Cut(dsn, "@"); ok {
			if user, pass, ok2 := strings.Cut(userPass, ":"); ok2 {
				sb.WriteString(fmt.Sprintf("user=%s\n", user))
				sb.WriteString(fmt.Sprintf("password=%s\n", pass))
			}
			// 解析 host:port，正确处理 tcp(xxx) 格式
			if hostPart, _, ok3 := strings.Cut(after, "/"); ok3 {
				hostPart = strings.TrimPrefix(hostPart, "tcp(")
				hostPart = strings.TrimSuffix(hostPart, ")")
				sb.WriteString(fmt.Sprintf("host=%s\n", strings.Split(hostPart, ":")[0]))
				if h := strings.Split(hostPart, ":"); len(h) > 1 {
					sb.WriteString(fmt.Sprintf("port=%s\n", h[1]))
				}
			}
		}
	}
	return sb.String()
}

// parseDSN 从 DSN 中提取 user:password
func parseDSN(dsn string) (user, pass string) {
	if before, _, ok := strings.Cut(dsn, "@"); ok {
		user, pass, _ = strings.Cut(before, ":")
	}
	return
}

// dsnHostPort 从 DSN 中提取 host:port
func dsnHostPort(dsn string) string {
	_, after, ok := strings.Cut(dsn, "@")
	if !ok {
		return ""
	}
	// after = tcp(host:port)/...
	hostPart, _, ok := strings.Cut(after, "/")
	if !ok {
		return ""
	}
	hostPart = strings.TrimPrefix(hostPart, "tcp(")
	hostPart = strings.TrimSuffix(hostPart, ")")
	return hostPart
}
