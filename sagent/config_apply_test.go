package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

func TestHostRuntimeAppliesFrequencyAndRemovesDisabledSource(t *testing.T) {
	pl := pipeline.New(resource.Labels{})
	groups := map[string]bool{}
	for _, name := range builtin.HostMetricGroupKeys() {
		groups[name] = false
	}
	groups["system"] = true
	runtime := newHostRuntime(pl, config.HostMetricsConfig{Enabled: false, Interval: 30 * time.Second})
	t.Cleanup(func() { _ = runtime.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runtime.Apply(ctx, config.HostMetricsConfig{Enabled: true, Interval: 15 * time.Second, Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Running() || runtime.current.Interval != 15*time.Second || pl.MetricCount() == 0 {
		t.Fatalf("configuration did not affect live collector: running=%t interval=%s count=%d", runtime.Running(), runtime.current.Interval, pl.MetricCount())
	}
	undo, err := runtime.Apply(ctx, config.HostMetricsConfig{Enabled: false, Interval: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Running() || pl.MetricCount() != 0 {
		t.Fatal("disabled collector kept running or exposing queued old samples")
	}
	if err := undo(); err != nil || !runtime.Running() || runtime.current.Interval != 15*time.Second {
		t.Fatalf("rollback did not restore last running configuration: %v", err)
	}
}

func TestHostRuntimeInvalidSettingsDoNotChangeCurrentCollector(t *testing.T) {
	runtime := newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Enabled: false, Interval: 30 * time.Second})
	for _, candidate := range []config.HostMetricsConfig{
		{Enabled: true, Interval: time.Second},
		{Enabled: true, Interval: 15 * time.Second, Groups: map[string]bool{"unknown-group": true}},
	} {
		if _, err := runtime.Apply(context.Background(), candidate); err == nil {
			t.Fatalf("invalid settings accepted: %+v", candidate)
		}
		if runtime.Running() || runtime.current.Interval != 30*time.Second {
			t.Fatal("invalid settings modified runtime")
		}
	}
}

// R0-1 新语义：不支持插件的托管目标跳过不失败（前向兼容，能力协商 R0-3 另行立项），
// 其余段（host_metrics）照常应用。
func TestPlatformConfigurationSkipsUnsupportedTargets(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second})}
	_, err := app.applyPlatformConfig(context.Background(), json.RawMessage(`{"host_metrics":{"enabled":false,"interval":"30s"},"targets":[{"plugin":"unsupported"}]}`))
	if err != nil {
		t.Fatalf("unsupported plugin targets must be skipped, not rejected: %v", err)
	}
}

// R0-1：mysql_probe 托管目标缺 dsn/resource_id → 拒绝整份配置（不带半态）。
// App 未接 mysqlRuntime（单测环境）时该段被跳过，因此用缺字段校验走解析失败路径。
func TestPlatformConfigurationRejectsInvalidMysqlTargets(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second}), log: nil}
	_, err := app.applyPlatformConfig(context.Background(), json.RawMessage(`{"host_metrics":{"enabled":false,"interval":"30s"},"targets":[{"plugin":"mysql_probe","name":"db-1"}]}`))
	if err == nil {
		t.Fatal("mysql_probe target without dsn/resource_id must be rejected")
	}
}

func TestPlatformConfigurationAppliesHostSettings(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second})}
	_, err := app.applyPlatformConfig(context.Background(), json.RawMessage(`{"host_metrics":{"enabled":false,"interval":"15s","groups":{"disk":false},"exclude_metrics":[]},"targets":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if app.hostRuntime.current.Interval != 15*time.Second || app.hostRuntime.current.Groups["disk"] {
		t.Fatal("platform configuration was not applied to the runtime controller")
	}
}

// R0-2：desired_state 非法值拒绝整份配置（只认 active/standby，防手滑字段静默变形）
func TestPlatformConfigurationRejectsInvalidDesiredState(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second}), log: nil}
	_, err := app.applyPlatformConfig(context.Background(), json.RawMessage(
		`{"host_metrics":{"enabled":false,"interval":"30s"},"targets":[{"plugin":"mysql_probe","name":"db-1","address":"u:p@tcp(h:3306)/","resource_id":"r1","desired_state":"paused"}]}`))
	if err == nil {
		t.Fatal("invalid desired_state must be rejected")
	}
}

// R0-2：desired_state=standby 条目解析通过（App 未接 mysqlRuntime 时该段跳过，不失败）
func TestPlatformConfigurationAcceptsStandbyTargetSchema(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second}), log: nil}
	if _, err := app.applyPlatformConfig(context.Background(), json.RawMessage(
		`{"host_metrics":{"enabled":false,"interval":"30s"},"targets":[{"plugin":"mysql_probe","name":"db-1","address":"u:p@tcp(h:3306)/","resource_id":"r1","desired_state":"standby"}]}`)); err != nil {
		t.Fatalf("standby target schema must parse: %v", err)
	}
}
