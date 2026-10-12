package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
)

type hostRuntime struct {
	mu        sync.Mutex
	pipeline  *pipeline.Pipeline
	current   config.HostMetricsConfig
	collector *builtin.HostMetricsCollector
	batches   chan builtin.MetricBatch
	done      chan struct{}
}

// targetEntry 平台 targets 条目（syncAgentConfig 下发口径）
type targetEntry struct {
	ID           int64  `json:"id"`
	Plugin       string `json:"plugin"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Target       string `json:"target"`
	Address      string `json:"address"`
	ResourceID   string `json:"resource_id"`
	ParamsJSON   string `json:"params_json"`
	DesiredState string `json:"desired_state"` // R0-2：active（默认）/ standby（预置不启动）
}

// targetName 平台 targets 条目的显示名（target 优先，name 兼容）
func targetName(t targetEntry) string {
	if t.Target != "" {
		return t.Target
	}
	return t.Name
}

// logWarn nil 安全告警：单测构造的 App 可能没有 logger
func (app *App) logWarn(format string, args ...any) {
	if app.log != nil {
		app.log.Warn(format, args...)
	}
}

func newHostRuntime(pl *pipeline.Pipeline, initial config.HostMetricsConfig) *hostRuntime {
	return &hostRuntime{pipeline: pl, current: cloneHostConfig(initial)}
}

func cloneHostConfig(cfg config.HostMetricsConfig) config.HostMetricsConfig {
	cfg.Groups = maps.Clone(cfg.Groups)
	cfg.ExcludeMetrics = slices.Clone(cfg.ExcludeMetrics)
	return cfg
}

func (h *hostRuntime) PluginName() string { return constants.PluginNameHostMetrics }

func (h *hostRuntime) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.collector != nil
}

func (h *hostRuntime) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.current.Enabled {
		return errors.New("host metrics are disabled by configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultStartupWaitTimeout)
	defer cancel()
	return h.startLocked(ctx)
}

func (h *hostRuntime) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopLocked()
	return nil
}

func (h *hostRuntime) Signal(sig os.Signal) error {
	if sig != syscall.SIGHUP {
		return errors.New("unsupported host metrics signal")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.current.Enabled {
		return errors.New("host metrics are disabled by configuration")
	}
	h.stopLocked()
	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultStartupWaitTimeout)
	defer cancel()
	return h.startLocked(ctx)
}

func (h *hostRuntime) Apply(ctx context.Context, next config.HostMetricsConfig) (func() error, error) {
	if next.Interval < constants.MinHostMetricsInterval {
		return nil, fmt.Errorf("host metrics interval must be at least %s", constants.MinHostMetricsInterval)
	}
	known := builtin.HostGroupLabels()
	for group := range next.Groups {
		if _, ok := known[group]; !ok {
			return nil, fmt.Errorf("unknown host metrics group %q", group)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	previous := cloneHostConfig(h.current)
	err := h.replaceLocked(ctx, next)
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultStartupWaitTimeout)
		defer cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.replaceLocked(ctx, previous)
	}, nil
}

func (h *hostRuntime) replaceLocked(ctx context.Context, next config.HostMetricsConfig) error {
	previous := h.current
	h.stopLocked()
	h.current = cloneHostConfig(next)
	if !next.Enabled {
		return nil
	}
	if err := h.startLocked(ctx); err != nil {
		h.current = previous
		if previous.Enabled {
			recovery, cancel := context.WithTimeout(context.Background(), constants.DefaultStartupWaitTimeout)
			defer cancel()
			return errors.Join(err, h.startLocked(recovery))
		}
		return err
	}
	return nil
}

func (h *hostRuntime) startLocked(ctx context.Context) error {
	if h.collector != nil {
		return nil
	}
	batches := make(chan builtin.MetricBatch)
	done := make(chan struct{})
	first := make(chan error, 1)
	h.batches, h.done = batches, done
	h.collector = builtin.NewHostMetricsCollector(h.current.Interval, h.current.Groups, h.current.ExcludeMetrics)
	go func() {
		defer close(done)
		initial := true
		for batch := range batches {
			h.pipeline.IngestBatch(batch)
			if initial {
				first <- batch.Err
				initial = false
			}
		}
	}()
	h.collector.Start(batches)
	select {
	case err := <-first:
		if err != nil {
			h.stopLocked()
			return fmt.Errorf("initial host collection failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		h.stopLocked()
		return ctx.Err()
	}
}

func (h *hostRuntime) stopLocked() {
	if h.collector != nil {
		h.collector.Stop()
		close(h.batches)
		<-h.done
		h.collector = nil
	}
	// Drain the previous producer before removal so queued samples cannot resurrect a disabled source.
	h.pipeline.RemoveSource(builtin.SnapshotSource(constants.PluginNameHostMetrics, "local"))
}

func (app *App) applyPlatformConfig(ctx context.Context, content json.RawMessage) (func() error, error) {
	var doc struct {
		Targets     []targetEntry `json:"targets"`
		HostMetrics *struct {
			Enabled        *bool           `json:"enabled"`
			Interval       string          `json:"interval"`
			Groups         map[string]bool `json:"groups"`
			ExcludeMetrics []string        `json:"exclude_metrics"`
		} `json:"host_metrics"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.New("invalid platform configuration schema")
	}
	if doc.HostMetrics == nil || doc.HostMetrics.Enabled == nil {
		return nil, errors.New("host_metrics.enabled is required")
	}
	interval, err := time.ParseDuration(doc.HostMetrics.Interval)
	if err != nil {
		return nil, errors.New("host_metrics.interval must be a duration")
	}

	// ---- R0-1/R0-2：平台托管采集目标（targets 段）----
	// 平台 targets 段是采集任务期望态（syncAgentConfig 随归属变更实时刷新）。
	// 本版运行时接 mysql_probe 与 redis_probe；其余插件类型跳过并告警（不失败，保持前向兼容）。
	// 单归属边界：属主唯一性由 L0 保证，Agent 忠实 reconcile。
	// R0-2 standby 预置：desired_state=standby 的条目做校验 + 版本包就位 + 连通性验证，
	// 但不启动采集任务（预热接管的配置副本；单归属不变，standby 不是进程副本）。
	var mysqlTargets []config.MySQLTarget
	var redisTargets []config.RedisTarget
	var kafkaTargets []config.KafkaTarget
	var portTargets []builtin.PortTarget
	for _, t := range doc.Targets {
		state := t.DesiredState
		if state == "" {
			state = "active" // 老配置无该字段按 active，向后兼容
		}
		switch t.Plugin {
		case constants.PluginNameMySQLProbe:
			// 托管 DSN 构造：平台 params 是 address/port/username/password（params.yaml 声明），
			// 与 redis_probe 同构拼 DSN——只读 dsn/address 会把 user/pass 丢掉（实测 156：
			// my.cnf 的 user/password 为空 → exporter 无凭据 → mysql_up=0）
			var pj struct {
				Address  string `json:"address"`
				Port     string `json:"port"`
				Username string `json:"username"`
				Password string `json:"password"`
				DSN      string `json:"dsn"`
				Mode     string `json:"mode"`
				Interval string `json:"interval"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &pj)
			}
			dsn := pj.DSN
			if dsn == "" {
				addr := pj.Address
				if addr == "" {
					addr = t.Address // 兼容：直接把 DSN 填在 address 的登记方式
				}
				port := pj.Port
				if port == "" {
					port = "3306"
				}
				if pj.Username != "" || pj.Password != "" {
					dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/", pj.Username, pj.Password, addr, port)
				} else {
					dsn = addr
				}
			}
			if dsn == "" || t.ResourceID == "" {
				return nil, fmt.Errorf("managed mysql_probe target %q: dsn 与 resource_id 均必填", targetName(t))
			}
			if state != "active" && state != "standby" {
				return nil, fmt.Errorf("managed mysql_probe target %q: 非法 desired_state %q（仅 active/standby）", targetName(t), state)
			}
			mysqlTargets = append(mysqlTargets, config.MySQLTarget{DSN: dsn, ResourceID: t.ResourceID, DesiredState: state})
		case constants.PluginNameRedisProbe:
			var pj struct {
				Address  string `json:"address"`
				Port     string `json:"port"`
				Password string `json:"password"`
				Mode     string `json:"mode"`
				Interval string `json:"interval"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &pj)
			}
			addr := pj.Address
			if addr == "" {
				addr = t.Address
			}
			port := pj.Port
			if port == "" {
				port = "6379"
			}
			if addr == "" || t.ResourceID == "" {
				return nil, fmt.Errorf("managed redis_probe target %q: address 与 resource_id 均必填", targetName(t))
			}
			if state != "active" && state != "standby" {
				return nil, fmt.Errorf("managed redis_probe target %q: 非法 desired_state %q（仅 active/standby）", targetName(t), state)
			}
			redisTargets = append(redisTargets, config.RedisTarget{
				Address: addr, Port: port, Password: pj.Password, Mode: pj.Mode, Interval: pj.Interval,
				ResourceID: t.ResourceID, DesiredState: state,
			})
		case constants.PluginNameKafkaExporter:
			// 托管目标解析：平台 params 是 address/port（params.yaml 声明），拼成 KafkaTarget。
			// address 允许填 host:port 或单独 host；port 缺省 9092（由内置 KafkaTargetHostPort 兜底）。
			var pj struct {
				Address  string `json:"address"`
				Port     string `json:"port"`
				Mode     string `json:"mode"`
				Interval string `json:"interval"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &pj)
			}
			addr := pj.Address
			if addr == "" {
				addr = t.Address
			}
			port := pj.Port
			if port == "" {
				port = "9092"
			}
			if addr == "" || t.ResourceID == "" {
				return nil, fmt.Errorf("managed kafka_exporter target %q: address 与 resource_id 均必填", targetName(t))
			}
			if state != "active" && state != "standby" {
				return nil, fmt.Errorf("managed kafka_exporter target %q: 非法 desired_state %q（仅 active/standby）", targetName(t), state)
			}
			kafkaTargets = append(kafkaTargets, config.KafkaTarget{
				Address: addr, Port: port, Mode: pj.Mode, Interval: pj.Interval,
				ResourceID: t.ResourceID, DesiredState: state,
			})
		case constants.PluginNameHTTPResponse:
			// http_response 托管目标：address 是完整 URL（SAgent 内置探测，无 exporter 二进制）。
			// 指标走 port_check_up / port_check_latency_seconds，注入 resource_id / remote_resource_id
			// 标签——remote 采集 observe_collect 在 VM 里按这些标签核验序列。
			var pj struct {
				Address  string `json:"address"`
				Port     string `json:"port"`
				Interval string `json:"interval"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &pj)
			}
			addr := pj.Address
			if addr == "" {
				addr = t.Address
			}
			if addr == "" || t.ResourceID == "" {
				return nil, fmt.Errorf("managed http_response target %q: address 与 resource_id 均必填", targetName(t))
			}
			if state != "active" && state != "standby" {
				return nil, fmt.Errorf("managed http_response target %q: 非法 desired_state %q（仅 active/standby）", targetName(t), state)
			}
			if state == "active" {
				portTargets = append(portTargets, builtin.PortTarget{
					Name: targetName(t), Type: "http", Address: addr, Timeout: 5 * time.Second,
					Labels: map[string]string{
						"resource_id": t.ResourceID, "remote_resource_id": t.ResourceID, "resource_type": "http",
					},
				})
			}
		default:
			if t.Plugin != "" {
				app.logWarn("托管目标 %q 的插件类型 %q 本版不支持，已跳过（不影响其他目标）", targetName(t), t.Plugin)
			}
		}
	}

	// host_metrics 段（原语义不动）→ mysql_probe 托管段；任一失败按逆序回滚已应用段
	var rollbacks []func() error
	hmRollback, err := app.hostRuntime.Apply(ctx, config.HostMetricsConfig{
		Enabled: *doc.HostMetrics.Enabled, Interval: interval,
		Groups: doc.HostMetrics.Groups, ExcludeMetrics: doc.HostMetrics.ExcludeMetrics,
	})
	if err != nil {
		return nil, err
	}
	rollbacks = append(rollbacks, hmRollback)
	if app.mysqlRuntime != nil {
		mpRollback, err := app.mysqlRuntime.Apply(ctx, mysqlTargets)
		if err != nil {
			for i := len(rollbacks) - 1; i >= 0; i-- {
				_ = rollbacks[i]()
			}
			return nil, err
		}
		rollbacks = append(rollbacks, mpRollback)
	}
	if app.redisRuntime != nil {
		rpRollback, err := app.redisRuntime.Apply(ctx, redisTargets)
		if err != nil {
			for i := len(rollbacks) - 1; i >= 0; i-- {
				_ = rollbacks[i]()
			}
			return nil, err
		}
		rollbacks = append(rollbacks, rpRollback)
	}
	if app.kafkaRuntime != nil {
		kfRollback, err := app.kafkaRuntime.Apply(ctx, kafkaTargets)
		if err != nil {
			for i := len(rollbacks) - 1; i >= 0; i-- {
				_ = rollbacks[i]()
			}
			return nil, err
		}
		rollbacks = append(rollbacks, kfRollback)
	}
	if app.portRuntime != nil {
		ptRollback, err := app.portRuntime.Apply(ctx, portTargets)
		if err != nil {
			for i := len(rollbacks) - 1; i >= 0; i-- {
				_ = rollbacks[i]()
			}
			return nil, err
		}
		rollbacks = append(rollbacks, ptRollback)
	}
	return func() error {
		var join []error
		for i := len(rollbacks) - 1; i >= 0; i-- {
			if err := rollbacks[i](); err != nil {
				join = append(join, err)
			}
		}
		return errors.Join(join...)
	}, nil
}
