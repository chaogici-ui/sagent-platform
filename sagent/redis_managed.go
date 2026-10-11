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

// redis_managed.go —— Agent 托管目标运行时（redis_probe）。
//
// 与 mysql_managed.go 完全同构：平台 targets 段是采集任务的唯一期望源，
// 本运行时把 redis_probe 条目 reconcile 到数据面——起第三方 redis_exporter 子进程
// （二进制从 L0 插件超市取，sha256 校验后落 package-cache）+ 增删 scrape 目标。
//   - SAgent 自身不解析 Redis 协议；采集一律由第三方 exporter 负责（架构红线）；
//   - 单归属边界不变：active 唯一性由 L0 保证，Agent 只忠实执行；
//   - R0-2 standby 预置：只做版本包就位 + 连通性验证，不启动采集任务。
type redisProbeRuntime struct {
	mu        sync.Mutex
	probe     *builtin.RedisProbe
	staticCfg config.RedisProbeConfig
	metricsCh chan builtin.MetricBatch
	log       *logger.Logger
	current   []config.RedisTarget
	managed   bool
	l0Base    string
	dataDir   string
	workDir   string
	pipeline  *pipeline.Pipeline
	standby   map[string]*standbyState
}

func newRedisProbeRuntime(cfg *config.Config, metricsCh chan builtin.MetricBatch, pl *pipeline.Pipeline, log *logger.Logger) *redisProbeRuntime {
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
	return &redisProbeRuntime{
		staticCfg: cfg.Plugins.RedisProbe,
		metricsCh: metricsCh,
		log:       log,
		l0Base:    strings.TrimRight(l0, "/"),
		dataDir:   dataDir,
		workDir:   ".",
		pipeline:  pl,
	}
}

func (r *redisProbeRuntime) PluginName() string { return constants.PluginNameRedisProbe }

func (r *redisProbeRuntime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probe != nil && r.probe.Running()
}

// StartStatic 静态引导（yaml 配置驱动）：平台配置到达前的行为
func (r *redisProbeRuntime) StartStatic() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.staticCfg.Enabled || len(r.staticCfg.Targets) == 0 {
		return nil
	}
	r.current = cloneRedisTargetsB(r.staticCfg.Targets)
	return r.startProbeLocked(activeRedisTargets(r.current))
}

// Start 实现 PluginHandle（ctl start redis_probe 恢复场景）
func (r *redisProbeRuntime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := activeRedisTargets(r.current)
	if len(active) == 0 {
		return errors.New("redis_probe: no managed targets to start")
	}
	return r.startProbeLocked(active)
}

// Stop 实现 PluginHandle
func (r *redisProbeRuntime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopProbeLocked()
}

// Signal 实现 PluginHandle（reload = 按当前目标集重启采集）
func (r *redisProbeRuntime) Signal(sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopProbeLocked(); err != nil {
		return err
	}
	active := activeRedisTargets(r.current)
	if len(active) == 0 {
		return nil
	}
	return r.startProbeLocked(active)
}

// Apply 平台托管目标 reconcile（applyPlatformConfig 调用）。语义同 mysqlProbeRuntime.Apply。
func (r *redisProbeRuntime) Apply(ctx context.Context, next []config.RedisTarget) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := cloneRedisTargetsB(r.current)
	prevManaged := r.managed
	prevStandby := r.standby
	r.managed = true
	r.current = cloneRedisTargetsB(next)
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
			return nil, fmt.Errorf("redis_probe exporter 二进制不可用: %w", err)
		}
	}
	for i := range next {
		if !next[i].IsStandby() {
			continue
		}
		st := r.standby[next[i].ResourceID]
		if hp := builtin.RedisTargetHostPort(next[i]); hp == "" {
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

	active := activeRedisTargets(next)
	if err := r.reconcileLocked(active); err != nil {
		r.rollbackLocked(prev, prevManaged, prevStandby)
		return nil, err
	}
	r.log.Info("redis_probe 托管目标已生效（active %d 个 / standby 预置 %d 个）", len(active), len(r.standby))
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.current = prev
		r.managed = prevManaged
		r.standby = prevStandby
		return r.reconcileLocked(activeRedisTargets(prev))
	}, nil
}

func (r *redisProbeRuntime) rollbackLocked(prev []config.RedisTarget, prevManaged bool, prevStandby map[string]*standbyState) {
	r.current = prev
	r.managed = prevManaged
	r.standby = prevStandby
	if rbErr := r.reconcileLocked(activeRedisTargets(prev)); rbErr != nil {
		r.log.Error("redis_probe 回滚上一版目标集失败: %v", rbErr)
	}
}

// reconcileLocked 目标集落地（入参为 active 子集）：空集 → 全停；非空 → 起进程（幂等）/ 热更目标
func (r *redisProbeRuntime) reconcileLocked(next []config.RedisTarget) error {
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
func (r *redisProbeRuntime) startProbeLocked(targets []config.RedisTarget) error {
	if len(targets) == 0 {
		return nil
	}
	bin, err := r.ensureBinary()
	if err != nil {
		return fmt.Errorf("redis_probe exporter 二进制不可用: %w", err)
	}
	probe := builtin.NewRedisProbe(config.RedisProbeConfig{Enabled: true, Targets: cloneRedisTargetsB(targets)})
	if r.pipeline != nil {
		probe.SetSourceRemover(r.pipeline.RemoveSource)
	}
	if err := probe.StartWith(bin, r.metricsCh); err != nil {
		return fmt.Errorf("start redis_probe: %w", err)
	}
	r.probe = probe
	r.log.Info("redis_probe started（bin=%s, %d targets）", bin, len(targets))
	return nil
}

func (r *redisProbeRuntime) stopProbeLocked() error {
	if r.probe == nil {
		return nil
	}
	err := r.probe.Stop()
	r.probe = nil
	return err
}

// ---------- 插件包获取：静态路径 → 本地 package-cache → L0 插件超市 ----------

func (r *redisProbeRuntime) ensureBinary() (string, error) {
	for _, p := range []string{r.staticCfg.ExporterBin, constants.DefaultRedisExportBin} {
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
	pkgName, ok := pluginPackageNames[constants.PluginNameRedisProbe]
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

// ObservedTargets observed_state 回报口径（心跳 stats.observed_targets），语义同 mysql。
func (r *redisProbeRuntime) ObservedTargets() map[string]map[string]any {
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

// activeRedisTargets 过滤出 desired_state=active 的目标
func activeRedisTargets(targets []config.RedisTarget) []config.RedisTarget {
	out := make([]config.RedisTarget, 0, len(targets))
	for _, t := range targets {
		if !t.IsStandby() {
			out = append(out, t)
		}
	}
	return out
}

func cloneRedisTargetsB(targets []config.RedisTarget) []config.RedisTarget {
	out := make([]config.RedisTarget, len(targets))
	copy(out, targets)
	return out
}
