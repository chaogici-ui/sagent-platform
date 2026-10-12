package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/logger"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
)

// port_checker_managed.go —— Agent 托管目标运行时（http_response / port_checker）。
//
// 与 mysql/redis/kafka 托管运行时同构：平台 targets 段是唯一期望源，本运行时把
// http_response（HTTP 拨测）与 port_checker（端口探测）条目 reconcile 到数据面。
// 与前三者的差异：探测是 SAgent 内置能力（builtin.PortChecker），**无 exporter 二进制**，
// 不涉及插件超市——Apply 直接重建探测目标集。
//   - http_response → type=http，address 是完整 URL（如 http://10.1.207.156:8080）；
//   - 指标名沿用内置 port_check_up / port_check_latency_seconds，注入 resource_id /
//     remote_resource_id 标签（remote 采集 observe_collect 依此在 VM 里核验序列）。
type portCheckerRuntime struct {
	mu        sync.Mutex
	checker   *builtin.PortChecker
	staticCfg config.PortCheckerCfg
	metricsCh chan builtin.MetricBatch
	log       *logger.Logger
	current   []builtin.PortTarget
	managed   bool
	pipeline  *pipeline.Pipeline
}

func newPortCheckerRuntime(cfg *config.Config, metricsCh chan builtin.MetricBatch, pl *pipeline.Pipeline, log *logger.Logger) *portCheckerRuntime {
	return &portCheckerRuntime{
		staticCfg: cfg.Plugins.PortChecker,
		metricsCh: metricsCh,
		log:       log,
		pipeline:  pl,
	}
}

func (r *portCheckerRuntime) PluginName() string { return constants.PluginNamePortChecker }

func (r *portCheckerRuntime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checker != nil
}

// StartStatic 静态引导（yaml 配置驱动）：平台配置到达前的行为
func (r *portCheckerRuntime) StartStatic() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.staticCfg.Enabled || len(r.staticCfg.Targets) == 0 {
		return nil
	}
	interval := r.staticCfg.Interval
	if interval <= 0 {
		interval = constants.DefaultPortCheckInterval
	}
	targets := make([]builtin.PortTarget, 0, len(r.staticCfg.Targets))
	for _, t := range r.staticCfg.Targets {
		targets = append(targets, builtin.PortTarget{
			Name: t.Name, Type: t.Type, Address: t.Address,
			Replicas: t.Replicas, Timeout: t.Timeout, Labels: t.Labels,
		})
	}
	r.current = clonePortTargets(targets)
	return r.startCheckerLocked(targets, interval)
}

// Start 实现 PluginHandle（ctl start 恢复场景）
func (r *portCheckerRuntime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.current) == 0 {
		return errors.New("port_checker: no managed targets to start")
	}
	return r.startCheckerLocked(r.current, constants.DefaultPortCheckInterval)
}

// Stop 实现 PluginHandle
func (r *portCheckerRuntime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCheckerLocked()
}

// Signal 实现 PluginHandle（reload = 按当前目标集重启探测）
func (r *portCheckerRuntime) Signal(sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopCheckerLocked(); err != nil {
		return err
	}
	if len(r.current) == 0 {
		return nil
	}
	return r.startCheckerLocked(r.current, constants.DefaultPortCheckInterval)
}

// Apply 平台托管目标 reconcile（applyPlatformConfig 调用）。
// port_checker 无 exporter 二进制、无 standby 预热物，目标集变化即重建探测循环。
func (r *portCheckerRuntime) Apply(ctx context.Context, next []builtin.PortTarget) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := clonePortTargets(r.current)
	prevManaged := r.managed
	r.managed = true
	r.current = clonePortTargets(next)

	if err := r.reconcileLocked(next); err != nil {
		r.current = prev
		r.managed = prevManaged
		return nil, err
	}
	r.log.Info("port_checker 托管目标已生效（%d 个）", len(next))
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.current = prev
		r.managed = prevManaged
		return r.reconcileLocked(prev)
	}, nil
}

// reconcileLocked 目标集落地：空集 → 全停；非空 → 重建探测循环
func (r *portCheckerRuntime) reconcileLocked(next []builtin.PortTarget) error {
	if r.checker != nil {
		r.stopCheckerLocked()
	}
	if len(next) == 0 {
		return nil
	}
	return r.startCheckerLocked(next, constants.DefaultPortCheckInterval)
}

// startCheckerLocked 创建并启动探测（幂等：已在跑则先停）
func (r *portCheckerRuntime) startCheckerLocked(targets []builtin.PortTarget, interval time.Duration) error {
	if r.checker != nil {
		r.stopCheckerLocked()
	}
	if len(targets) == 0 {
		return nil
	}
	checker := builtin.NewPortChecker(targets, interval)
	checker.Start(r.metricsCh)
	r.checker = checker
	r.log.Info("port_checker started（%d targets）", len(targets))
	return nil
}

func (r *portCheckerRuntime) stopCheckerLocked() error {
	if r.checker == nil {
		return nil
	}
	r.checker.Stop()
	r.checker = nil
	return nil
}

// ObservedTargets observed_state 回报口径（心跳 stats.observed_targets）：
// 探测目标是内置能力，无独立进程——统一报 active + 运行态。
func (r *portCheckerRuntime) ObservedTargets() map[string]map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]map[string]any{}
	procRunning := r.checker != nil
	for _, t := range r.current {
		rid := ""
		if t.Labels != nil {
			rid = t.Labels["resource_id"]
		}
		if rid == "" {
			rid = t.Name
		}
		out[rid] = map[string]any{"state": "active", "proc": procRunning}
	}
	return out
}

func clonePortTargets(targets []builtin.PortTarget) []builtin.PortTarget {
	out := make([]builtin.PortTarget, len(targets))
	copy(out, targets)
	for i := range out {
		if out[i].Labels != nil {
			l := make(map[string]string, len(out[i].Labels))
			for k, v := range out[i].Labels {
				l[k] = v
			}
			out[i].Labels = l
		}
	}
	return out
}
