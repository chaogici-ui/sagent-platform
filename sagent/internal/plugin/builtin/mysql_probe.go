package builtin

import (
	"fmt"
	"net/http"
	"net/url"
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
	exporterBin string             // 存储以便 restart
	metricsCh   chan<- MetricBatch // 存储以便 restart
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
		client:   &http.Client{Timeout: 10 * time.Second},
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
func (m *MySQLProbe) StartWith(exporterBin string, metricsCh chan<- MetricBatch) error {
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
func (m *MySQLProbe) scrape(metricsCh chan<- MetricBatch) {
	for _, target := range m.targets {
		hostPort := dsnHostPort(target.DSN)
		source, healthSource := MySQLSnapshotSources(target)
		var metrics []Metric
		var health []Metric
		var err error
		if hostPort == "" {
			err = fmt.Errorf("invalid mysql target address")
		} else {
			var body string
			body, err = fetchPrometheusText(m.client, "http://127.0.0.1:19104/probe?target="+url.QueryEscape(hostPort))
			if err == nil {
				metrics, err = parseMySQLMetrics(body, target.ResourceID)
			}
		}
		business := make([]Metric, 0, len(metrics))
		for _, metric := range metrics {
			if metric.Name == "mysql_up" {
				health = append(health, metric)
				if metric.Value != 1 {
					err = fmt.Errorf("mysql collection failed: mysql_up=%v", metric.Value)
				}
			} else {
				business = append(business, metric)
			}
		}
		if err != nil {
			health = []Metric{{Name: "mysql_up", Value: 0, Type: constants.MetricTypeGauge, Help: "Whether the MySQL server is up", Labels: map[string]string{
				"resource_id": target.ResourceID, "remote_resource_id": target.ResourceID, "resource_type": "mysql",
			}}}
		}
		if !sendBatch(metricsCh, m.stopCh, snapshotBatch(source, business, m.interval, err)) {
			return
		}
		if !sendBatch(metricsCh, m.stopCh, snapshotBatch(healthSource, health, m.interval, nil)) {
			return
		}
	}
}

func MySQLSnapshotSources(target config.MySQLTarget) (business, health string) {
	hostPort := dsnHostPort(target.DSN)
	return SnapshotSource("mysql_probe", target.ResourceID, hostPort), SnapshotSource("mysql_probe_health", target.ResourceID, hostPort)
}

func parseMySQLMetrics(body string, remoteResourceID string) ([]Metric, error) {
	metrics, err := parsePrometheusText(body)
	if err != nil {
		return nil, err
	}
	for i := range metrics {
		metrics[i].Labels["resource_id"] = remoteResourceID
		metrics[i].Labels["resource_type"] = "mysql"
		metrics[i].Labels["remote_resource_id"] = remoteResourceID
		if strings.HasSuffix(metrics[i].Name, "_total") {
			metrics[i].Type = constants.MetricTypeCounter
		}
	}
	return metrics, nil
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
