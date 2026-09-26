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
		Targets     []json.RawMessage `json:"targets"`
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
	if len(doc.Targets) != 0 {
		return nil, errors.New("managed target application is not supported yet; existing collectors were not changed")
	}
	if doc.HostMetrics == nil || doc.HostMetrics.Enabled == nil {
		return nil, errors.New("host_metrics.enabled is required")
	}
	interval, err := time.ParseDuration(doc.HostMetrics.Interval)
	if err != nil {
		return nil, errors.New("host_metrics.interval must be a duration")
	}
	return app.hostRuntime.Apply(ctx, config.HostMetricsConfig{
		Enabled: *doc.HostMetrics.Enabled, Interval: interval,
		Groups: doc.HostMetrics.Groups, ExcludeMetrics: doc.HostMetrics.ExcludeMetrics,
	})
}
