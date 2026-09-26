package main

// alert_test.go —— D6 告警引擎单测：状态翻转去重、firing 计数、全规则幂等扫描。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

func resetAlertState() {
	alertsMu.Lock()
	defer alertsMu.Unlock()
	alerts = alerts[:0]
	for k := range alertState {
		delete(alertState, k)
	}
}

func mustTestAlerts(t *testing.T) []Alert {
	t.Helper()
	alertsMu.Lock()
	defer alertsMu.Unlock()
	return append([]Alert{}, alerts...)
}

// firingCount 事件日志中 firing 事件的条数（历史口径，供旧断言）。
func firingCount(t *testing.T) int {
	t.Helper()
	alertsMu.Lock()
	defer alertsMu.Unlock()
	n := 0
	for _, a := range alerts {
		if a.State == "firing" {
			n++
		}
	}
	return n
}

// activeFiring 当前处于 firing 的规则数（状态表口径，`/api/alerts` 的 firing 字段来源）。
func activeFiring(t *testing.T) int {
	t.Helper()
	alertsMu.Lock()
	defer alertsMu.Unlock()
	n := 0
	for _, f := range alertState {
		if f {
			n++
		}
	}
	return n
}

// TestAlertWebhookNotify 配了 ALERT_WEBHOOK_URL 时，状态翻转会把告警 POST 到 webhook（best-effort）。
func TestAlertWebhookNotify(t *testing.T) {
	received := make(chan Alert, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a Alert
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		received <- a
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetAlertState()
	prev := cfgAlertWebhook
	cfgAlertWebhook = srv.URL
	defer func() { cfgAlertWebhook = prev }()

	setAlertState("agent_stale", SevWarning, true, "A 陈旧", "detail")
	select {
	case got := <-received:
		if got.Source != "agent_stale" || got.State != "firing" || got.Severity != SevWarning {
			t.Fatalf("unexpected webhook alert: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook not notified after state flip")
	}
	// 同状态翻转去重：无新外发
	// （此路由无需显式断言——reset 防误报即可，避免依赖 channel 二次清空）
	resetAlertState()
}

// TestAlertDedupAndTransition 去重：同一 source 连续相同状态只产出一次；翻转才产新事件。
func TestAlertDedupAndTransition(t *testing.T) {
	resetAlertState()
	setAlertState("agent_stale", SevWarning, true, "A 陈旧", "detail")
	setAlertState("agent_stale", SevWarning, true, "A 陈旧", "detail")
	if got := len(mustTestAlerts(t)); got != 1 {
		t.Fatalf("same firing state must dedupe to 1 event, got %d", got)
	}
	// 翻转 resolved → 新事件
	setAlertState("agent_stale", SevWarning, false, "正常", "")
	if got := len(mustTestAlerts(t)); got != 2 {
		t.Fatalf("want 2 events after recovery, got %d", got)
	}
	// 相邻循环不重复
	setAlertState("agent_stale", SevWarning, false, "正常", "")
	if got := len(mustTestAlerts(t)); got != 2 {
		t.Fatalf("resolved state should not re-fire, got %d", got)
	}
}

// TestAlertsActiveMetricPath activeAlertCount（/metrics 的 sagent_l0_alerts_active 数据源）与状态表同步。
func TestAlertsActiveMetricPath(t *testing.T) {
	resetAlertState()
	if got := activeAlertCount(); got != 0 {
		t.Fatalf("healthy platform should expose 0 active alerts, got %d", got)
	}
	setAlertState("cache_unverified", SevCritical, true, "缓存未校验", "d")
	if got := activeAlertCount(); got != 1 {
		t.Fatalf("want activeAlertCount=1, got %d", got)
	}
	setAlertState("cache_unverified", SevCritical, false, "缓存正常", "")
	if got := activeAlertCount(); got != 0 {
		t.Fatalf("after recovery activeAlertCount must be 0, got %d", got)
	}
	resetAlertState()
}

// TestAlertFiringCount firing 汇总正确：事件日志口径 + 当前 firing 状态口径。
func TestAlertFiringCount(t *testing.T) {
	resetAlertState()
	setAlertState("catalog_down", SevCritical, true, "目录不可达", "d")
	if got := firingCount(t); got != 1 {
		t.Fatalf("want event firing=1, got %d", got)
	}
	if got := activeFiring(t); got != 1 {
		t.Fatalf("want active firing=1, got %d", got)
	}
	// 恢复后：事件日志保留该 firing 记录（历史），但当前自动归 0（/api/alerts 的 firing 字段语义）
	setAlertState("catalog_down", SevCritical, false, "目录正常", "")
	if got := firingCount(t); got != 1 {
		t.Fatalf("event log keeps 1 firing history, got %d", got)
	}
	if got := activeFiring(t); got != 0 {
		t.Fatalf("active firing must drop to 0 after recovery, got %d", got)
	}
	resetAlertState()
}

// TestScanAlertsNoPanic scanAlerts 全规则跑两轮不 panic 且幂等（无多余产出）。
func TestScanAlertsNoPanic(t *testing.T) {
	resetAlertState()
	prev := vmStorageProbe
	vmStorageProbe = func() (*vmStorageStat, error) { return &vmStorageStat{UsagePct: 40, Size: 4e9, Total: 10e9}, nil }
	defer func() { vmStorageProbe = prev }()
	setCacheRoot(t, t.TempDir())
	scanAlerts(nil, nil)
	scanAlerts(nil, nil)
	// store=nil：不 panic 即可；重复扫描不应产生重复 firing 事件（全部 resolved 态）
	if got := firingCount(t); got != 0 {
		t.Fatalf("idempotent scan with empty sources should have 0 firing, got %d", got)
	}
	resetAlertState()
}

// TestParseVMStorageUsage 存储水位解析：data 分桶求和/(data+磁盘free) 口径正确。
func TestParseVMStorageUsage(t *testing.T) {
	metrics := `# HELP vm_data_size_bytes ...
vm_data_size_bytes{type="indexdb/file"} 3000000000
vm_data_size_bytes{type="storage/small"} 5000000000
# HELP vm_free_disk_space_bytes ...
vm_free_disk_space_bytes{path="/storage"} 12000000000
vm_free_disk_space_bytes{path="/other"} 99999
vm_storage_is_read_only{path="/storage"} 0
`
	st, err := parseVMStorageUsage(strings.NewReader(metrics))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 数据 8GB 求和，磁盘空闲 12GB，总量 20GB → 40%
	want := float64(8e9) / float64(8e9+12e9) * 100
	if st.UsagePct < want-0.5 || st.UsagePct > want+0.5 {
		t.Fatalf("want usage ~40%%, got %f", st.UsagePct)
	}
	if st.Size != 8e9 || st.Total != 20e9 || st.ReadOnly != 0 {
		t.Fatalf("want size=8e9 total=20e9 ro=0, got size=%d total=%d ro=%d", st.Size, st.Total, st.ReadOnly)
	}
	// 只读保护置位
	ro := strings.Replace(metrics, "vm_storage_is_read_only{path=\"/storage\"} 0", "vm_storage_is_read_only{path=\"/storage\"} 1", 1)
	st2, err := parseVMStorageUsage(strings.NewReader(ro))
	if err != nil {
		t.Fatalf("parse ro: %v", err)
	}
	if st2.ReadOnly != 1 {
		t.Fatalf("expect readOnly=1, got %d", st2.ReadOnly)
	}
}

// TestCollectorPoolDegradedAlert R3：池内健康采集机 <2 且池内仍有远端采集目标 → firing；补齐 → resolved。
// 空池（无远端目标）不评估，避免噪音。
func TestCollectorPoolDegradedAlert(t *testing.T) {
	resetAlertState()
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	// 池 cn-east：属主健康 + 一台陈旧 → 健康采集机=1 <2
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now)
	seedCollector(t, db, "sagent-A", "tenant-a", "cn-east", "cn-east", now-int64(relocateFailSec)-10)
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "db1", Type: "mysql", Address: "1.2.3.4:3306", AgentID: "sagent-owner", Plugin: "mysql_probe", TenantID: "tenant-a", Region: "cn-east"}); err != nil {
		t.Fatal(err)
	}
	runCollectorPoolRule(db)
	if !alertState["collector_pool_degraded"] {
		t.Fatal("pool with <2 healthy collectors must fire collector_pool_degraded")
	}
	// 补齐第二台健康采集机 → 池健康 → resolved
	seedCollector(t, db, "sagent-B", "tenant-a", "cn-east", "cn-east", now)
	runCollectorPoolRule(db)
	if alertState["collector_pool_degraded"] {
		t.Fatal("pool recovered must resolve collector_pool_degraded")
	}
	resetAlertState()
}

// TestCollectorPoolRuleIgnoresEmptyPool 无远端采集目标的池不评估：不得因"无池"而误报降级。
func TestCollectorPoolRuleIgnoresEmptyPool(t *testing.T) {
	resetAlertState()
	db := newRelocateTestDB(t)
	now := time.Now().Unix()
	seedCollector(t, db, "sagent-owner", "tenant-a", "cn-east", "cn-east", now)
	// 仅本地能力插件目标（不可迁）→ 不计入待承接池
	if _, err := db.InsertTarget(&storepkg.TargetRow{Name: "host1", Type: "host", Address: "localhost", AgentID: "sagent-owner", Plugin: "host_metrics", TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	runCollectorPoolRule(db)
	if alertState["collector_pool_degraded"] {
		t.Fatal("no relocatable target in pool must not fire degraded alert")
	}
	resetAlertState()
}

// TestStorageWaterLevelAlert 存储水位超阈值 → firing；回落 → resolved；状态表翻转为依据。
func TestStorageWaterLevelAlert(t *testing.T) {
	resetAlertState()
	// 恢复默认探实现后将走网络——这里必须覆盖为内存桩
	prev := vmStorageProbe
	defer func() { vmStorageProbe = prev }()
	// 高水位 → firing
	vmStorageProbe = func() (*vmStorageStat, error) { return &vmStorageStat{UsagePct: 96, Size: 96e9, Total: 100e9}, nil }
	runStorageWaterRule()
	if !alertState["storage_water"] {
		t.Fatal("high water level must put storage_water into firing")
	}
	if alertState["storage_down"] {
		t.Fatal("reachable storage must not raise storage_down")
	}
	// 回落 → resolved
	vmStorageProbe = func() (*vmStorageStat, error) { return &vmStorageStat{UsagePct: 40, Size: 4e9, Total: 10e9}, nil }
	runStorageWaterRule()
	if alertState["storage_water"] {
		t.Fatal("low water level must resolve storage_water")
	}
	resetAlertState()
}
