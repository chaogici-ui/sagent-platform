package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/logger"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
)

// kafka_managed.go —— Agent 托管目标运行时（kafka_exporter）。
//
// 与 mysql_managed.go / redis_managed.go 同构：平台 targets 段是采集任务的唯一期望源，
// 本运行时把 kafka_exporter 条目 reconcile 到数据面——从 L0 插件超市取 kafka_exporter
// 二进制（sha256 校验后落 package-cache），每个目标起一个 exporter 子进程
// （danielqsj/kafka_exporter 不支持 /scrape 多目标，一个进程只连一个 broker 集群）。
//   - SAgent 自身不解析 Kafka 协议；采集一律由第三方 exporter 负责（架构红线）；
//   - 单归属边界不变：active 唯一性由 L0 保证，Agent 只忠实执行；
//   - R0-2 standby 预置：只做版本包就位 + 连通性验证，不启动采集任务。
type kafkaProbeRuntime struct {
	mu        sync.Mutex
	probe     *builtin.KafkaProbe
	staticCfg config.KafkaExporterConfig
	metricsCh chan builtin.MetricBatch
	log       *logger.Logger
	current   []config.KafkaTarget
	managed   bool
	l0Base    string
	dataDir   string
	workDir   string
	pipeline  *pipeline.Pipeline
	standby   map[string]*standbyState
}

func newKafkaProbeRuntime(cfg *config.Config, metricsCh chan builtin.MetricBatch, pl *pipeline.Pipeline, log *logger.Logger) *kafkaProbeRuntime {
	l0 := cfg.L0Console.Endpoints
	if l0 == "" {
		l0 = cfg.L0Console.URL
	}
	if i := strings.IndexAny(l0, ",; "); i > 0 {
		l0 = l0[:i]
	}
	dataDir := cfg.L0Console.DataDir
	if dataDir == "" {
		dataDir = constants.DefaultDataDir
	}
	return &kafkaProbeRuntime{
		staticCfg: cfg.Plugins.KafkaExporter,
		metricsCh: metricsCh,
		log:       log,
		l0Base:    strings.TrimRight(l0, "/"),
		dataDir:   dataDir,
		workDir:   ".",
		pipeline:  pl,
	}
}

func (r *kafkaProbeRuntime) PluginName() string { return constants.PluginNameKafkaExporter }

func (r *kafkaProbeRuntime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probe != nil && r.probe.Running()
}

// StartStatic 静态引导（yaml 配置驱动）：平台配置到达前的行为
func (r *kafkaProbeRuntime) StartStatic() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.staticCfg.Enabled || len(r.staticCfg.Targets) == 0 {
		return nil
	}
	r.current = cloneKafkaTargetsB(r.staticCfg.Targets)
	return r.startProbeLocked(activeKafkaTargets(r.current))
}

// Start 实现 PluginHandle（ctl start kafka_exporter 恢复场景）
func (r *kafkaProbeRuntime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := activeKafkaTargets(r.current)
	if len(active) == 0 {
		return errors.New("kafka_exporter: no managed targets to start")
	}
	return r.startProbeLocked(active)
}

// Stop 实现 PluginHandle
func (r *kafkaProbeRuntime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopProbeLocked()
}

// Signal 实现 PluginHandle（reload = 按当前目标集重启采集）
func (r *kafkaProbeRuntime) Signal(sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopProbeLocked(); err != nil {
		return err
	}
	active := activeKafkaTargets(r.current)
	if len(active) == 0 {
		return nil
	}
	return r.startProbeLocked(active)
}

// Apply 平台托管目标 reconcile（applyPlatformConfig 调用）。语义同 mysqlProbeRuntime.Apply。
func (r *kafkaProbeRuntime) Apply(ctx context.Context, next []config.KafkaTarget) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := cloneKafkaTargetsB(r.current)
	prevManaged := r.managed
	prevStandby := r.standby
	r.managed = true
	r.current = cloneKafkaTargetsB(next)
	r.standby = map[string]*standbyState{}
	for i := range next {
		if next[i].IsStandby() {
			r.standby[next[i].ResourceID] = &standbyState{}
		}
	}

	// standby 预置就绪：① 版本包就位（active/standby 共用同一 exporter 包）② TCP 连通性验证
	if len(next) > 0 {
		if _, err := r.ensureBinary(); err != nil {
			r.rollbackLocked(prev, prevManaged, prevStandby)
			return nil, fmt.Errorf("kafka_exporter exporter 二进制不可用: %w", err)
		}
	}
	for i := range next {
		if !next[i].IsStandby() {
			continue
		}
		st := r.standby[next[i].ResourceID]
		if hp := builtin.KafkaTargetHostPort(next[i]); hp == "" {
			st.Note = "目标地址无法解析出 host:port，跳过连通性验证"
		} else {
			conn, derr := net.DialTimeout("tcp", hp, 3*time.Second)
			if derr != nil {
				st.Reachable = false
				st.Note = "连通性验证失败: " + derr.Error()
			} else {
				conn.Close()
				st.Reachable = true
			}
		}
		st.CheckedAt = time.Now().Unix()
	}

	active := activeKafkaTargets(next)
	if err := r.reconcileLocked(active); err != nil {
		r.rollbackLocked(prev, prevManaged, prevStandby)
		return nil, err
	}
	r.log.Info("kafka_exporter 托管目标已生效（active %d 个 / standby 预置 %d 个）", len(active), len(r.standby))
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.current = prev
		r.managed = prevManaged
		r.standby = prevStandby
		return r.reconcileLocked(activeKafkaTargets(prev))
	}, nil
}

func (r *kafkaProbeRuntime) rollbackLocked(prev []config.KafkaTarget, prevManaged bool, prevStandby map[string]*standbyState) {
	r.current = prev
	r.managed = prevManaged
	r.standby = prevStandby
	if rbErr := r.reconcileLocked(activeKafkaTargets(prev)); rbErr != nil {
		r.log.Error("kafka_exporter 回滚上一版目标集失败: %v", rbErr)
	}
}

// reconcileLocked 目标集落地（入参为 active 子集）：空集 → 全停；非空 → 起进程（幂等）/ 热更目标
func (r *kafkaProbeRuntime) reconcileLocked(next []config.KafkaTarget) error {
	if r.probe == nil {
		if len(next) == 0 {
			return nil
		}
		return r.startProbeLocked(next)
	}
	_, err := r.probe.ApplyTargets(next)
	return err
}

// startProbeLocked 解析二进制（package-cache get）→ 创建并启动 probe
func (r *kafkaProbeRuntime) startProbeLocked(targets []config.KafkaTarget) error {
	if len(targets) == 0 {
		return nil
	}
	bin, err := r.ensureBinary()
	if err != nil {
		return fmt.Errorf("kafka_exporter exporter 二进制不可用: %w", err)
	}
	probe := builtin.NewKafkaProbe(config.KafkaExporterConfig{Enabled: true, Targets: cloneKafkaTargetsB(targets)})
	if r.pipeline != nil {
		probe.SetSourceRemover(r.pipeline.RemoveSource)
	}
	if err := probe.StartWith(bin, r.metricsCh); err != nil {
		return fmt.Errorf("start kafka_exporter: %w", err)
	}
	r.probe = probe
	r.log.Info("kafka_exporter started（bin=%s, %d targets）", bin, len(targets))
	return nil
}

func (r *kafkaProbeRuntime) stopProbeLocked() error {
	if r.probe == nil {
		return nil
	}
	err := r.probe.Stop()
	r.probe = nil
	return err
}

// ensureBinary 插件包获取：静态路径 → 本地 package-cache → L0 插件超市（同 mysql/redis 语义）
func (r *kafkaProbeRuntime) ensureBinary() (string, error) {
	for _, p := range []string{r.staticCfg.ExporterBin, constants.DefaultKafkaExportBin} {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.workDir, p)
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	pkgName, ok := pluginPackageNames[constants.PluginNameKafkaExporter]
	if !ok || r.l0Base == "" {
		return "", errors.New("静态路径无二进制且未配置 L0 插件超市地址")
	}
	mani, err := fetchPluginManifest(r.l0Base, pkgName)
	if err != nil {
		return "", fmt.Errorf("获取插件包清单失败: %w", err)
	}
	cachePath := filepath.Join(r.dataDir, "packages", "plugins", pkgName, mani.Version, mani.Filename)
	if sha, err := sha256FileB(cachePath); err == nil && sha == mani.SHA256 {
		return cachePath, nil
	}
	if err := downloadPluginPackage(r.l0Base, pkgName, mani, cachePath); err != nil {
		return "", err
	}
	r.log.Info("插件包 %s v%s 已入本地 package-cache（sha256 校验通过）", pkgName, mani.Version)
	return cachePath, nil
}

// ObservedTargets observed_state 回报口径（心跳 stats.observed_targets），语义同 mysql/redis。
func (r *kafkaProbeRuntime) ObservedTargets() map[string]map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]map[string]any{}
	procRunning := r.probe != nil && r.probe.Running()
	for _, t := range r.current {
		entry := map[string]any{"state": "active"}
		if t.IsStandby() {
			entry["state"] = "standby"
			if st := r.standby[t.ResourceID]; st != nil {
				entry["reachable"] = st.Reachable
				entry["checked_at"] = st.CheckedAt
				if st.Note != "" {
					entry["note"] = st.Note
				}
			}
		} else {
			entry["proc"] = procRunning
		}
		out[t.ResourceID] = entry
	}
	return out
}

// activeKafkaTargets 过滤出 desired_state=active 的目标
func activeKafkaTargets(targets []config.KafkaTarget) []config.KafkaTarget {
	out := make([]config.KafkaTarget, 0, len(targets))
	for _, t := range targets {
		if !t.IsStandby() {
			out = append(out, t)
		}
	}
	return out
}

func cloneKafkaTargetsB(targets []config.KafkaTarget) []config.KafkaTarget {
	out := make([]config.KafkaTarget, len(targets))
	copy(out, targets)
	return out
}
