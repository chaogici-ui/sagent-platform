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

func TestPlatformConfigurationRejectsUnimplementedTargets(t *testing.T) {
	app := &App{hostRuntime: newHostRuntime(pipeline.New(resource.Labels{}), config.HostMetricsConfig{Interval: 30 * time.Second})}
	_, err := app.applyPlatformConfig(context.Background(), json.RawMessage(`{"host_metrics":{"enabled":false,"interval":"30s"},"targets":[{"plugin":"unsupported"}]}`))
	if err == nil {
		t.Fatal("configuration with unimplemented target application was falsely accepted")
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
