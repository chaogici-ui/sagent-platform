package builtin

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/subprocess"
)

// RedisProbe 远程 Redis 采集插件。
//
// 与 MySQLProbe 同构：SAgent 自身不解析 Redis 协议，采集一律交给第三方 redis_exporter；
// 本插件只做编排——起 exporter 子进程 + 按目标集抓 /scrape?target= 并注入资源标签。
//   - exporter 子进程与「目标集」解耦：redis_exporter 以多目标模式常驻，
//     目标态在 SAgent 侧 scrape 循环里——任务增删（ApplyTargets）不动 exporter 进程；
//   - 任务清零 → 连 exporter 子进程一起回收（旧属主机不残留孤儿进程）。
type RedisProbe struct {
	targets      []config.RedisTarget
	exporter     *subprocess.Plugin
	exporterBin  string
	exporterArgs []string // 当前 exporter 进程启动参数（口令变更时比对重建进程）
	metricsCh    chan<- MetricBatch
	interval     time.Duration
	client       *http.Client
	stopCh       chan struct{}
	scrapeStop   chan struct{}
	wg           sync.WaitGroup
	mu           sync.Mutex
	running      bool
	removeSource func(name string)
}

// NewRedisProbe 创建 Redis 采集器
func NewRedisProbe(cfg config.RedisProbeConfig) *RedisProbe {
	return &RedisProbe{
		targets:  cfg.Targets,
		interval: 30 * time.Second,
		stopCh:   make(chan struct{}),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Name 插件名
func (m *RedisProbe) Name() string { return constants.PluginNameRedisProbe }

// PluginName 实现 server.PluginHandle 接口
func (m *RedisProbe) PluginName() string { return constants.PluginNameRedisProbe }

// Signal 实现 server.PluginHandle 接口（reload = 停旧 exporter → 重新拉起）
func (m *RedisProbe) Signal(sig os.Signal) error {
	m.Stop()
	return m.Start()
}

// Running 实现 server.PluginHandle 接口
func (m *RedisProbe) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// SetSourceRemover 注入快照源清理回调（运行时在 StartWith 前调用）
func (m *RedisProbe) SetSourceRemover(fn func(name string)) { m.removeSource = fn }

// StartWith 首次启动采集（存入参数供后续 restart 使用）
func (m *RedisProbe) StartWith(exporterBin string, metricsCh chan<- MetricBatch) error {
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
func (m *RedisProbe) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}
	if m.exporterBin == "" || m.metricsCh == nil {
		return fmt.Errorf("redis_probe: not initialized, call StartWith(bin, ch) first")
	}
	m.running = true
	m.stopCh = make(chan struct{})
	return m.doStart()
}

func (m *RedisProbe) doStart() error {
	if len(m.targets) == 0 {
		return nil
	}
	if err := m.ensureExporterLocked(); err != nil {
		return err
	}
	m.startScrapeLoopLocked()
	return nil
}

// ensureExporterLocked 确保 redis_exporter 子进程在跑。
// 多目标模式：exporter 不带 --redis.addr，目标由 /scrape?target= 逐个指定；
// 已在跑且启动参数一致则原样复用（「同类进程复用」的数据面基础）。
// 口令经 --redis.password 全局传入——redis_exporter 多目标模式不解析 target URL
// 内嵌凭据（实测 NOAUTH，官方 README：/scrape 全实例共用 --redis.password）。
// 目标集口令必须统一，否则 fail-closed 报错（绝不能拿 A 口令静默连 B 实例）。
// 口令变更（args 变化）时连进程一起重建——启动参数不重启不生效。
func (m *RedisProbe) ensureExporterLocked() error {
	args, err := m.buildExporterArgs()
	if err != nil {
		return err
	}
	if m.exporter != nil {
		if sameArgList(m.exporterArgs, args) {
			return nil
		}
		m.exporter.Stop()
		m.exporter = nil
	}
	m.exporter = subprocess.New("redis_exporter", m.exporterBin, args)
	if err := m.exporter.Start(); err != nil {
		return fmt.Errorf("start redis_exporter: %w", err)
	}
	m.exporterArgs = args
	m.client = &http.Client{Timeout: 10 * time.Second}
	return nil
}

// buildExporterArgs 组装 exporter 启动参数（含目标集统一口令校验）
func (m *RedisProbe) buildExporterArgs() ([]string, error) {
	args := []string{"--web.listen-address=127.0.0.1:19105"}
	pass, ok := redisUniformPassword(m.targets)
	if !ok {
		return nil, fmt.Errorf(
			"redis_exporter 多目标模式不支持 per-target 口令（/scrape 全实例共用 --redis.password），" +
				"目标集存在多个不同口令：请统一口令或拆分到不同采集机")
	}
	if pass != "" {
		args = append(args, "--redis.password="+pass)
	}
	return args, nil
}

// redisUniformPassword 目标集统一口令：全部一致（含全空）返回 (pass,true)；
// 存在多个不同口令返回 (_,false)
func redisUniformPassword(targets []config.RedisTarget) (string, bool) {
	pass := ""
	for i, t := range targets {
		if i == 0 {
			pass = t.Password
			continue
		}
		if t.Password != pass {
			return "", false
		}
	}
	return pass, true
}

// sameArgList 启动参数逐项等价（顺序敏感：args 顺序即语义）
func sameArgList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// startScrapeLoopLocked 启动（或重启）目标抓取循环；目标态只在这里，增删目标不必动进程
func (m *RedisProbe) startScrapeLoopLocked() {
	m.scrapeStop = make(chan struct{})
	stop := m.scrapeStop
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// 等 exporter 启动
		time.Sleep(3 * time.Second)
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
func (m *RedisProbe) stopScrapeLoopLocked() {
	if m.scrapeStop != nil {
		close(m.scrapeStop)
		m.scrapeStop = nil
	}
	m.wg.Wait()
}

// ApplyTargets 平台托管目标热更：目标集 diff 生效（幂等；增删只重启 scrape 循环；
// 清零则连 exporter 子进程一起回收）。
func (m *RedisProbe) ApplyTargets(next []config.RedisTarget) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sameRedisTargets(m.targets, next) {
		return false, nil
	}
	removed := removedRedisTargets(m.targets, next)
	m.targets = cloneRedisTargets(next)
	if !m.running {
		m.purgeRemovedSources(removed)
		return true, nil
	}
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(removed)
	if len(next) == 0 {
		if m.exporter != nil {
			m.exporter.Stop()
			m.exporter = nil
		}
		return true, nil
	}
	// 无条件走 ensure（内部幂等）：同口令纯增删目标复用进程（args 不变），
	// 口令变更时 args 变化会连 exporter 进程一起重建（--redis.password 是启动参数）
	if err := m.ensureExporterLocked(); err != nil {
		return true, err
	}
	m.startScrapeLoopLocked()
	return true, nil
}

// Stop 停止采集（可重入）：scrape 循环 + exporter 子进程全停
func (m *RedisProbe) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	close(m.stopCh)
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(m.targets)
	exp := m.exporter
	m.exporter = nil
	m.mu.Unlock()
	if exp != nil {
		exp.Stop()
	}
	return nil
}

// scrape 抓取 redis_exporter 指标，为每个 target 注入 resource_id
func (m *RedisProbe) scrape(metricsCh chan<- MetricBatch, stop <-chan struct{}) {
	for _, target := range m.targets {
		source, healthSource := RedisSnapshotSources(target)
		var metrics []Metric
		var health []Metric
		var err error
		targetURL := redisTargetURL(target)
		if targetURL == "" {
			err = fmt.Errorf("invalid redis target address")
		} else {
			var body string
			body, err = fetchPrometheusText(m.client, "http://127.0.0.1:19105/scrape?target="+url.QueryEscape(targetURL))
			if err == nil {
				metrics, err = parseRedisMetrics(body, target.ResourceID)
			}
		}
		business := make([]Metric, 0, len(metrics))
		for _, metric := range metrics {
			if metric.Name == "redis_up" {
				health = append(health, metric)
				if metric.Value != 1 {
					err = fmt.Errorf("redis collection failed: redis_up=%v", metric.Value)
				}
			} else {
				business = append(business, metric)
			}
		}
		if err != nil {
			health = []Metric{{Name: "redis_up", Value: 0, Type: constants.MetricTypeGauge, Help: "Whether the Redis server is up", Labels: map[string]string{
				"resource_id": target.ResourceID, "remote_resource_id": target.ResourceID, "resource_type": "redis",
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

// RedisSnapshotSources 业务面 / 健康面的稳定快照来源
func RedisSnapshotSources(target config.RedisTarget) (business, health string) {
	hp := RedisTargetHostPort(target)
	return SnapshotSource(constants.PluginNameRedisProbe, target.ResourceID, hp),
		SnapshotSource(constants.PluginNameRedisProbe+"_health", target.ResourceID, hp)
}

func parseRedisMetrics(body string, remoteResourceID string) ([]Metric, error) {
	metrics, err := parsePrometheusText(body)
	if err != nil {
		return nil, err
	}
	for i := range metrics {
		metrics[i].Labels["resource_id"] = remoteResourceID
		metrics[i].Labels["resource_type"] = "redis"
		metrics[i].Labels["remote_resource_id"] = remoteResourceID
		if strings.HasSuffix(metrics[i].Name, "_total") {
			metrics[i].Type = constants.MetricTypeCounter
		}
	}
	return metrics, nil
}

// RedisTargetHostPort 目标 host:port（端口空则 6379）；无法组成返回空串
func RedisTargetHostPort(t config.RedisTarget) string {
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return ""
	}
	port := strings.TrimSpace(t.Port)
	if port == "" {
		port = "6379"
	}
	return addr + ":" + port
}

// redisTargetURL 构造 redis_exporter /scrape?target= 需要的 redis:// URL。
// 口令不内嵌——redis_exporter 多目标模式不解析 target URL 内嵌凭据（实测 NOAUTH，
// 官方 README：凭据统一经 --redis.password 启动参数传入）
func redisTargetURL(t config.RedisTarget) string {
	hp := RedisTargetHostPort(t)
	if hp == "" {
		return ""
	}
	return "redis://" + hp
}

// removedRedisTargets 计算被移除的目标（旧集有、新集无；按 address:port+ResourceID 判等）
func removedRedisTargets(oldSet, newSet []config.RedisTarget) []config.RedisTarget {
	out := []config.RedisTarget{}
	for _, o := range oldSet {
		found := false
		for _, n := range newSet {
			if RedisTargetHostPort(o) == RedisTargetHostPort(n) && o.ResourceID == n.ResourceID {
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

// purgeRemovedSources 清理被移除目标的 pipeline 快照源（业务面 + 健康面）
func (m *RedisProbe) purgeRemovedSources(removed []config.RedisTarget) {
	if m.removeSource == nil || len(removed) == 0 {
		return
	}
	for _, t := range removed {
		biz, health := RedisSnapshotSources(t)
		m.removeSource(biz)
		m.removeSource(health)
	}
}

func cloneRedisTargets(targets []config.RedisTarget) []config.RedisTarget {
	out := make([]config.RedisTarget, len(targets))
	copy(out, targets)
	return out
}

// sameRedisTargets 目标集等价判断（按 address:port+ResourceID+Password 排序后逐项比较，与顺序无关）。
// Password 必须参与判等：口令变更是有效目标变更（如旧 cfg 无口令、新版补口令），
// 判等漏掉它会跳过 scrape 循环重启，exporter 永远以旧凭据裸连（NOAUTH 类错误无法自愈）。
// removedRedisTargets 仍按 host:port+ResourceID 判等——口令变更不清快照源，只重启循环。
func sameRedisTargets(a, b []config.RedisTarget) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(t config.RedisTarget) string {
		return RedisTargetHostPort(t) + "|" + t.ResourceID + "|" + t.Password
	}
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
