package builtin

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/plugin/subprocess"
)

// KafkaProbe 远程 Kafka 采集插件。
//
// 与 MySQLProbe / RedisProbe 同构的第三方 exporter 编排语义（SAgent 自身不解析
// Kafka 协议），但与两者有一个关键差异：danielqsj/kafka_exporter 不支持
// /scrape 多目标模式（一个进程只连一个 broker 集群），因此这里采用
// 「每个目标一个 exporter 子进程」——进程与目标一一对应：
//   - 每个 target 起一个 `kafka_exporter --kafka.server=<host:port>` 子进程，
//     listen 在 127.0.0.1:<base+index>（SAgent 侧内部端口，不对外）；
//   - scrape 循环逐个抓本地 exporter /metrics，注入 resource_id 标签后送 pipeline；
//   - 目标增删（ApplyTargets）：先按目标键 diff，停掉移除目标的进程、
//     复用参数未变的进程、重建参数变化的进程（镜像 redis 的 args 比对语义）；
//   - 任务清零 → 全部 exporter 子进程一起回收（不残留孤儿进程）。
type KafkaProbe struct {
	targets      []config.KafkaTarget
	exporters    map[string]*kafkaExporterProc // key = kafkaTargetKey
	exporterBin  string
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

// kafkaExporterProc 一个目标对应的 exporter 子进程（含其启动参数与监听端口）
type kafkaExporterProc struct {
	target   config.KafkaTarget
	exporter *subprocess.Plugin
	args     []string
	port     string // 127.0.0.1:<port> 监听端口
}

// kafkaExporterBasePort 本机 exporter 内部监听端口基址（127.0.0.1 私网，与
// SAgent 19090 及平台 8427 等互不冲突）。端口按目标在有序目标集中的序号分配。
const kafkaExporterBasePort = 19106

// NewKafkaProbe 创建 Kafka 采集器
func NewKafkaProbe(cfg config.KafkaExporterConfig) *KafkaProbe {
	return &KafkaProbe{
		targets:   cfg.Targets,
		exporters: map[string]*kafkaExporterProc{},
		interval:  30 * time.Second,
		stopCh:    make(chan struct{}),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Name 插件名
func (m *KafkaProbe) Name() string { return constants.PluginNameKafkaExporter }

// PluginName 实现 server.PluginHandle 接口
func (m *KafkaProbe) PluginName() string { return constants.PluginNameKafkaExporter }

// Signal 实现 server.PluginHandle 接口（reload = 停旧 exporter → 重新拉起）
func (m *KafkaProbe) Signal(sig os.Signal) error {
	m.Stop()
	return m.Start()
}

// Running 实现 server.PluginHandle 接口
func (m *KafkaProbe) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// SetSourceRemover 注入快照源清理回调（运行时在 StartWith 前调用）
func (m *KafkaProbe) SetSourceRemover(fn func(name string)) { m.removeSource = fn }

// StartWith 首次启动采集（存入参数供后续 restart 使用）
func (m *KafkaProbe) StartWith(exporterBin string, metricsCh chan<- MetricBatch) error {
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
func (m *KafkaProbe) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}
	if m.exporterBin == "" || m.metricsCh == nil {
		return fmt.Errorf("kafka_exporter: not initialized, call StartWith(bin, ch) first")
	}
	m.running = true
	m.stopCh = make(chan struct{})
	return m.doStart()
}

func (m *KafkaProbe) doStart() error {
	if len(m.targets) == 0 {
		return nil
	}
	if err := m.ensureExportersLocked(); err != nil {
		return err
	}
	m.startScrapeLoopLocked()
	return nil
}

// kafkaSortedTargets 按目标键稳定排序（端口分配与比对都基于此序，保证可复现）
func kafkaSortedTargets(targets []config.KafkaTarget) []config.KafkaTarget {
	out := make([]config.KafkaTarget, len(targets))
	copy(out, targets)
	sort.Slice(out, func(i, j int) bool { return kafkaTargetKey(out[i]) < kafkaTargetKey(out[j]) })
	return out
}

// ensureExportersLocked 把 exporter 子进程集 reconcile 到当前目标集：
// 每个目标一个进程；参数未变的进程复用，参数变化/新增的重建，移除目标的进程停止。
// 端口 = base + 目标在有序集里的序号（目标键稳定排序，序号变更即参数变化 → 重建）。
func (m *KafkaProbe) ensureExportersLocked() error {
	if len(m.targets) == 0 {
		return nil
	}
	ordered := kafkaSortedTargets(m.targets)
	desired := map[string]*kafkaExporterProc{}
	for i, t := range ordered {
		port := strconv.Itoa(kafkaExporterBasePort + i)
		args := kafkaExporterArgs(t, port)
		key := kafkaTargetKey(t)
		if proc, ok := m.exporters[key]; ok {
			if sameArgList(proc.args, args) && proc.port == port {
				desired[key] = proc
				continue
			}
			proc.exporter.Stop()
			delete(m.exporters, key)
		}
		exp := subprocess.New("kafka_exporter", m.exporterBin, args)
		if err := exp.Start(); err != nil {
			return fmt.Errorf("start kafka_exporter for %s: %w", key, err)
		}
		desired[key] = &kafkaExporterProc{target: t, exporter: exp, args: args, port: port}
	}
	// 停止被移除目标的 exporter
	for key, proc := range m.exporters {
		if _, keep := desired[key]; !keep {
			proc.exporter.Stop()
		}
	}
	m.exporters = desired
	m.client = &http.Client{Timeout: 10 * time.Second}
	return nil
}

// kafkaExporterArgs 组装单个目标的 exporter 启动参数：
// kafka_exporter 只连一个 broker 集群（--kafka.server），listen 在本机内部端口。
func kafkaExporterArgs(t config.KafkaTarget, port string) []string {
	hp := KafkaTargetHostPort(t)
	args := []string{
		"--kafka.server=" + hp,
		"--web.listen-address=127.0.0.1:" + port,
	}
	return args
}

// startScrapeLoopLocked 启动（或重启）目标抓取循环；目标态只在这里
func (m *KafkaProbe) startScrapeLoopLocked() {
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

// stopScrapeLoopLocked 只停抓取循环（exporter 进程保持）
func (m *KafkaProbe) stopScrapeLoopLocked() {
	if m.scrapeStop != nil {
		close(m.scrapeStop)
		m.scrapeStop = nil
	}
	m.wg.Wait()
}

// ApplyTargets 平台托管目标热更：目标集 diff 生效（幂等；增删只重启 scrape 循环
// 与受影响 exporter，清零则连 exporter 子进程一起回收）。
func (m *KafkaProbe) ApplyTargets(next []config.KafkaTarget) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sameKafkaTargets(m.targets, next) {
		return false, nil
	}
	removed := removedKafkaTargets(m.targets, next)
	m.targets = cloneKafkaTargets(next)
	if !m.running {
		m.purgeRemovedSources(removed)
		return true, nil
	}
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(removed)
	if len(next) == 0 {
		for _, proc := range m.exporters {
			proc.exporter.Stop()
		}
		m.exporters = map[string]*kafkaExporterProc{}
		return true, nil
	}
	if err := m.ensureExportersLocked(); err != nil {
		return true, err
	}
	m.startScrapeLoopLocked()
	return true, nil
}

// Stop 停止采集（可重入）：scrape 循环 + 全部 exporter 子进程全停
func (m *KafkaProbe) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	close(m.stopCh)
	m.stopScrapeLoopLocked()
	m.purgeRemovedSources(m.targets)
	exps := m.exporters
	m.exporters = map[string]*kafkaExporterProc{}
	m.mu.Unlock()
	for _, proc := range exps {
		proc.exporter.Stop()
	}
	return nil
}

// scrape 逐个抓取本机 kafka_exporter /metrics，为每个 target 注入 resource_id
func (m *KafkaProbe) scrape(metricsCh chan<- MetricBatch, stop <-chan struct{}) {
	for _, t := range m.targets {
		proc, ok := m.exporters[kafkaTargetKey(t)]
		if !ok || proc.exporter == nil {
			continue // exporter 尚未就绪（重建窗口），下轮再抓
		}
		source, healthSource := KafkaSnapshotSources(t)
		body, err := fetchPrometheusText(m.client, "http://127.0.0.1:"+proc.port+"/metrics")
		var business []Metric
		if err == nil {
			business, err = parseKafkaMetrics(body, t.ResourceID)
		}
		// kafka_exporter（danielqsj v1.x）不产出 kafka_up：健康面按「是否拿到 kafka_* 业务指标」
		// 推导——有 kafka_brokers 等业务序列即视为与 broker 连通（kafka_up=1），否则 0。
		// 绝不能"缺失就兜底 0"：exporter 连通良好但无 kafka_up 指标时会被误报为挂（实测踩坑）。
		hasKafka := err == nil
		if hasKafka {
			hasKafka = false
			for _, metric := range business {
				if strings.HasPrefix(metric.Name, "kafka_") {
					hasKafka = true
					break
				}
			}
		}
		health := []Metric{{
			Name: "kafka_up", Value: 0, Type: constants.MetricTypeGauge,
			Help: "Whether the Kafka cluster is reachable",
			Labels: map[string]string{
				"resource_id": t.ResourceID, "remote_resource_id": t.ResourceID, "resource_type": "kafka",
			},
		}}
		if hasKafka {
			health[0].Value = 1
		}
		if !sendBatch(metricsCh, stop, snapshotBatch(source, business, m.interval, err)) {
			return
		}
		if !sendBatch(metricsCh, stop, snapshotBatch(healthSource, health, m.interval, nil)) {
			return
		}
	}
}

// KafkaSnapshotSources 业务面 / 健康面的稳定快照来源
func KafkaSnapshotSources(t config.KafkaTarget) (business, health string) {
	hp := KafkaTargetHostPort(t)
	return SnapshotSource(constants.PluginNameKafkaExporter, t.ResourceID, hp),
		SnapshotSource(constants.PluginNameKafkaExporter+"_health", t.ResourceID, hp)
}

func parseKafkaMetrics(body string, remoteResourceID string) ([]Metric, error) {
	metrics, err := parsePrometheusText(body)
	if err != nil {
		return nil, err
	}
	for i := range metrics {
		metrics[i].Labels["resource_id"] = remoteResourceID
		metrics[i].Labels["resource_type"] = "kafka"
		metrics[i].Labels["remote_resource_id"] = remoteResourceID
		if strings.HasSuffix(metrics[i].Name, "_total") {
			metrics[i].Type = constants.MetricTypeCounter
		}
	}
	return metrics, nil
}

// KafkaTargetHostPort 目标 host:port（端口空则 9092）；无法组成返回空串
func KafkaTargetHostPort(t config.KafkaTarget) string {
	addr := strings.TrimSpace(t.Address)
	if addr == "" {
		return ""
	}
	port := strings.TrimSpace(t.Port)
	if port == "" {
		port = "9092"
	}
	// address 已自带 :port（kafka 参数允许填 host:port）时不重复拼
	if i := strings.LastIndex(addr, ":"); i > 0 {
		if tail := addr[i+1:]; tail != "" && strings.Trim(tail, "0123456789") == "" {
			return addr
		}
	}
	return addr + ":" + port
}

// kafkaTargetKey 目标键（host:port + resource_id）：进程归属的唯一标识
func kafkaTargetKey(t config.KafkaTarget) string {
	return KafkaTargetHostPort(t) + "|" + t.ResourceID
}

// removedKafkaTargets 计算被移除的目标（旧集有、新集无；按目标键判等）
func removedKafkaTargets(oldSet, newSet []config.KafkaTarget) []config.KafkaTarget {
	out := []config.KafkaTarget{}
	for _, o := range oldSet {
		found := false
		for _, n := range newSet {
			if kafkaTargetKey(o) == kafkaTargetKey(n) {
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
func (m *KafkaProbe) purgeRemovedSources(removed []config.KafkaTarget) {
	if m.removeSource == nil || len(removed) == 0 {
		return
	}
	for _, t := range removed {
		biz, health := KafkaSnapshotSources(t)
		m.removeSource(biz)
		m.removeSource(health)
	}
}

func cloneKafkaTargets(targets []config.KafkaTarget) []config.KafkaTarget {
	out := make([]config.KafkaTarget, len(targets))
	copy(out, targets)
	return out
}

// sameKafkaTargets 目标集等价判断（按目标键+端口排序后逐项比较，与顺序无关）。
// 顺序无关的判等只判断「目标集是否真的变了」——本实现端口随排序序号分配，
// 顺序变化虽会重建 exporter，但仍属目标集变更，应触发 reconcile。
func sameKafkaTargets(a, b []config.KafkaTarget) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(t config.KafkaTarget) string { return kafkaTargetKey(t) }
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
