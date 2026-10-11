package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/logger"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/resource"
)

// newStandbyTestRuntime 构造可跑 Apply 的 mysql 托管运行时：
// exporter_bin 指向测试自造的可执行文件（ensureBinary ① 静态路径命中，不触 L0）。
// 注意：本测试只验证 standby 预置语义（不启动采集任务），不真正拉起 exporter 子进程。
func newStandbyTestRuntime(t *testing.T) *mysqlProbeRuntime {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mysqld_exporter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New("", "", logger.ERROR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cfg := &config.Config{}
	cfg.Plugins.MySQLProbe.ExporterBin = bin
	return newMySQLProbeRuntime(cfg, make(chan builtin.MetricBatch, 1), pipeline.New(resource.Labels{}), log)
}

// TestApplyStandbyTargetsDoesNotStartProbe R0-2 核心：standby 预置只做就绪动作，不起采集任务。
func TestApplyStandbyTargetsDoesNotStartProbe(t *testing.T) {
	rt := newStandbyTestRuntime(t)
	targets := []config.MySQLTarget{
		{DSN: "u:p@tcp(127.0.0.1:1)/", ResourceID: "res-standby", DesiredState: "standby"},
	}
	_, err := rt.Apply(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Running() {
		t.Fatal("standby preset must not start the collection probe")
	}
	if rt.probe != nil {
		t.Fatal("standby preset must not create probe handle")
	}
	obs := rt.ObservedTargets()
	entry, ok := obs["res-standby"]
	if !ok {
		t.Fatalf("standby target missing from observed_state: %#v", obs)
	}
	if entry["state"] != "standby" {
		t.Fatalf("observed state = %v, want standby", entry["state"])
	}
	// 127.0.0.1:1 不可达：连通性验证如实报 false，且带结论 note（不静默）
	if r, _ := entry["reachable"].(bool); r {
		t.Fatalf("unreachable standby target must report reachable=false: %#v", entry)
	}
	if n, _ := entry["note"].(string); n == "" {
		t.Fatalf("standby connectivity failure must carry a note: %#v", entry)
	}
}

// TestApplyEmptyConfigIsNoOp 空目标集（含纯 standby 清空）不触 ensureBinary、不报错——
// 平台把预置全部回收时，Agent 侧必须安静收敛而不是拿"L0 不可达"拒绝配置。
func TestApplyEmptyConfigIsNoOp(t *testing.T) {
	log, err := logger.New("", "", logger.ERROR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	rt := newMySQLProbeRuntime(&config.Config{}, make(chan builtin.MetricBatch, 1), pipeline.New(resource.Labels{}), log)
	if _, err := rt.Apply(context.Background(), nil); err != nil {
		t.Fatalf("empty target set must apply cleanly: %v", err)
	}
	if rt.Running() {
		t.Fatal("empty target set left probe running")
	}
}

// TestActiveTargetsPartition active/standby 分区口径（R0-2 reconcile 只吃 active 子集）
func TestActiveTargetsPartition(t *testing.T) {
	in := []config.MySQLTarget{
		{DSN: "a@tcp(h1:3306)/", ResourceID: "r1"},
		{DSN: "b@tcp(h2:3306)/", ResourceID: "r2", DesiredState: "standby"},
		{DSN: "c@tcp(h3:3306)/", ResourceID: "r3", DesiredState: "active"},
	}
	active := activeTargets(in)
	if len(active) != 2 || active[0].ResourceID != "r1" || active[1].ResourceID != "r3" {
		t.Fatalf("active partition wrong: %+v", active)
	}
	if mysqlDSNHostPort("u:p@tcp(10.0.0.8:3306)/db") != "10.0.0.8:3306" {
		t.Fatal("mysqlDSNHostPort failed on canonical DSN")
	}
	if mysqlDSNHostPort("u:p@tcp(bad)/db") != "" {
		t.Fatal("mysqlDSNHostPort must reject host without port")
	}
}
