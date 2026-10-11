package builtin

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/subprocess"
)

// MySQLProbe 远程 MySQL 采集插件
// 生命周期（R0-1 托管目标语义）：
//   - exporter 子进程与「目标集」解耦：mysqld-exporter 以 /probe?target= 多目标模式运行，
//     目标态在 SAgent 侧 scrape 循环里——任务增删（ApplyTargets）不动 exporter 进程；
//   - 任务清零 → 连 exporter 子进程一起回收（旧属主机不残留孤儿进程）；
//   - 凭证变化（首个目标 DSN 的 user/pass）才需要重启 exporter 重读 my.cnf。
type MySQLProbe struct {
	targets     []config.MySQLTarget
	exporter    *subprocess.Plugin
	exporterBin string             // 存储以便 restart
	metricsCh   chan<- MetricBatch // 存储以便 restart
	interval    time.Duration
	client      *http.Client
	stopCh      chan struct{} // 全局生命周期（Stop 后不可 StartWith 前不得复用）
	scrapeStop  chan struct{} // scrape 循环级开关（ApplyTargets 部分停止用）
	wg          sync.WaitGroup
	mu          sync.Mutex
	running     bool
	// removeSource 目标被移除/清零时清理其在 pipeline 中的残留快照源（防旧属主残留序列双写）。
	// 由运行时注入（pipeline.RemoveSource）；nil 时跳过清理（仅停止未来抓取）。
	removeSource func(name string)
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

// SetSourceRemover 注入快照源清理回调（运行时在 StartWith 前调用）
func (m *MySQLProbe) SetSourceRemover(fn func(name string)) { m.removeSource = fn }

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
	if err := m.ensureExporterLocked(); err != nil {
		return err
	}
	m.startScrapeLoopLocked()
	return nil
}

// ensureExporterLocked 确保 mysqld_exporter 子进程在跑（凭证来自首个目标 DSN）。
// 已在跑且凭证未变 → 原样复用（「同类进程复用」的数据面基础）。
func (m *MySQLProbe) ensureExporterLocked() error {
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
	return nil
}

// startScrapeLoopLocked 启动（或重启）目标抓取循环；目标态只在这里，增删目标不必动进程
func (m *MySQLProbe) startScrapeLoopLocked() {
	m.scrapeStop = make(chan struct{})
	stop := m.scrapeStop
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// 等 exporter 启动
		time.Sleep(5 * time.Second)
		m.scrape(m.metricsCh, stop)

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.scrape(m.metricsCh, stop)
			case <-stop:
				return
			}
		}
	}()
}

// stopScrapeLoopLocked 只停抓取循环（exporter 进程保持——任务增删零进程抖动）
func (m *MySQLProbe) stopScrapeLoopLocked() {
	if m.scrapeStop != nil {
		close(m.scrapeStop)
		m.scrapeStop = nil
	}
	m.wg.Wait()
}

// ApplyTargets 平台托管目标热更（R0-1）：目标集 diff 生效。
//   - 集合未变 → no-op（幂等，重复下发不产生进程抖动）；
//   - 增/删部分目标 → 只重启 scrape 循环，exporter 进程复用；
//   - 目标清零 → scrape 循环停 + exporter 子进程回收（任务清零即进程下线）；
//   - 凭证变化 → exporter 需重读 my.cnf，此时才重启子进程。
//
// 返回是否发生实质变化。要求已 StartWith（未初始化时仅更新目标集，下次 Start 生效）。
func (m *MySQLProbe) ApplyTargets(next []config.MySQLTarget) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sameTargets(m.targets, next) {
		return false, nil
	}
	// 凭证是否变化（首个目标的 user/pass；变更才需重启 exporter 重读 my.cnf）
	credsChanged := false
	if len(next) > 0 {
		ou, op := parseDSN(firstDSN(m.targets))
		nu, np := parseDSN(next[0].DSN)
		credsChanged = ou != nu || op != np
	}
	// 计算被移除的目标（旧集 - 新集），稍后清理其 pipeline 快照源
	removed := removedTargets(m.targets, next)
	m.targets = cloneTargets(next)
	if !m.running {
		m.purgeRemovedSources(removed)
		return true, nil // 未运行：目标集已更新，下次 StartWith/Start 生效
	}
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(removed)
	if len(next) == 0 {
		// 任务清零：连 exporter 进程一起回收
		if m.exporter != nil {
			m.exporter.Stop()
			m.exporter = nil
		}
		return true, nil
	}
	if credsChanged && m.exporter != nil {
		m.exporter.Stop()
		m.exporter = nil
	}
	if m.exporter == nil {
		if err := m.ensureExporterLocked(); err != nil {
			return true, err
		}
	}
	m.startScrapeLoopLocked()
	return true, nil
}

// Stop 停止采集（可重入）：scrape 循环 + exporter 子进程全停（进程组清理语义）
func (m *MySQLProbe) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	close(m.stopCh)
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(m.targets) // 全停：清掉全部目标的快照源
	exp := m.exporter
	m.exporter = nil
	m.mu.Unlock()

	if exp != nil {
		exp.Stop()
	}
	return nil
}

// scrape 抓取 mysqld_exporter 指标，为每个 target 注入 resource_id
func (m *MySQLProbe) scrape(metricsCh chan<- MetricBatch, stop <-chan struct{}) {
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
		if !sendBatch(metricsCh, stop, snapshotBatch(source, business, m.interval, err)) {
			return
		}
		if !sendBatch(metricsCh, stop, snapshotBatch(healthSource, health, m.interval, nil)) {
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

// removedTargets 计算被移除的目标（旧集有、新集无；按 DSN+ResourceID 判等）
func removedTargets(oldSet, newSet []config.MySQLTarget) []config.MySQLTarget {
	out := []config.MySQLTarget{}
	for _, o := range oldSet {
		found := false
		for _, n := range newSet {
			if o.DSN == n.DSN && o.ResourceID == n.ResourceID {
				found = true
				break
			}
		}
		if !found {
			out = append(out, o)
		}
	}
	return out
}

// purgeRemovedSources 清理被移除目标的 pipeline 快照源（业务面 + 健康面两个源）。
// 不清的后果：旧属主 /metrics 永远挂着交出去的目标残留序列，VM 双写。
func (m *MySQLProbe) purgeRemovedSources(removed []config.MySQLTarget) {
	if m.removeSource == nil || len(removed) == 0 {
		return
	}
	for _, t := range removed {
		biz, health := MySQLSnapshotSources(t)
		m.removeSource(biz)
		m.removeSource(health)
	}
}

// firstDSN 取目标集中首个 DSN（空集返回空串）
func firstDSN(targets []config.MySQLTarget) string {
	if len(targets) == 0 {
		return ""
	}
	return targets[0].DSN
}

// cloneTargets 深拷贝目标集（Apply/回滚各自持有独立副本，防串改）
func cloneTargets(targets []config.MySQLTarget) []config.MySQLTarget {
	out := make([]config.MySQLTarget, len(targets))
	copy(out, targets)
	return out
}

// sameTargets 目标集等价判断（按 DSN+ResourceID 排序后逐项比较，与顺序无关）
func sameTargets(a, b []config.MySQLTarget) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(t config.MySQLTarget) string { return t.DSN + "|" + t.ResourceID }
	ka := make([]string, 0, len(a))
	kb := make([]string, 0, len(b))
	for i := range a {
		ka = append(ka, key(a[i]))
		kb = append(kb, key(b[i]))
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}
