package main

// vmagent_pool_test.go —— 池调度决策单测：判死/接管/回切/防抖全部走注入式新鲜度
// （vmpTick 的 fresh 参数），不依赖真实 VM。目录库用 openTestCatalog（PG 隔离 schema）。

import (
	"os"
	"testing"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

func seedTarget(t *testing.T, catDB *storepkg.DB, key, job, preferred string) {
	t.Helper()
	if _, err := catDB.UpsertVmagentTarget(&storepkg.VmagentTargetRow{
		TargetKey: key, DisplayName: key, JobName: job, PreferredOwner: preferred, Enabled: true,
	}); err != nil {
		t.Fatalf("seed target %s: %v", key, err)
	}
}

func ownerOf(t *testing.T, catDB *storepkg.DB, key string) string {
	t.Helper()
	tg, err := catDB.GetVmagentTarget(key)
	if err != nil || tg == nil {
		t.Fatalf("get target %s: %v", key, err)
	}
	return tg.CurrentOwner
}

func setOwnerForTest(t *testing.T, catDB *storepkg.DB, key, owner string) {
	t.Helper()
	if err := catDB.UpdateVmagentTargetOwner(key, owner); err != nil {
		t.Fatalf("set owner: %v", err)
	}
}

// 1) 判死接管：A 挂（freshness 超阈值）→ B 接管其目标。
func TestVmagentPoolTakeoverOnOwnerDeath(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	seedTarget(t, catDB, "mysql-exp-2:9104", "mysql", "pool-b")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-2:9104", "pool-b")

	// pool-a 数据面停更 75s（>60s 判死阈值，阈值含 VM latencyOffset 30s 补偿），pool-b 新鲜
	fresh := map[string]float64{"pool-a": 75, "pool-b": 5}
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.Kind != "takeover" || ev.TargetKey != "mysql-exp-1:9104" {
		t.Fatalf("应接管 mysql-exp-1，实际 %+v", ev)
	}
	if ev.ToOwner != "pool-b" {
		t.Fatalf("承接端应为 pool-b，实际 %q", ev.ToOwner)
	}
	if got := ownerOf(t, catDB, "mysql-exp-1:9104"); got != "pool-b" {
		t.Fatalf("归属应已切到 pool-b，实际 %q", got)
	}
}

// 2) preferred 优先：A 挂且 A 是 preferred → preferred 不健康，应选健康成员而非空等。
func TestVmagentPoolTakeoverFallsBackWhenPreferredDead(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "sagent-1:19090", "sagent", "pool-a")
	setOwnerForTest(t, catDB, "sagent-1:19090", "pool-a")
	fresh := map[string]float64{"pool-a": 90, "pool-b": 3}
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.ToOwner != "pool-b" {
		t.Fatalf("preferred 死亡时应由 pool-b 承接，实际 %+v", ev)
	}
}

// 3) 未分派目标：首次注册（owner 空）→ 调度器自动分派给健康成员。
func TestVmagentPoolAssignsUnassigned(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "l0-console:8080", "l0-console", "pool-a")
	setOwnerForTest(t, catDB, "l0-console:8080", "") // 模拟空归属注册
	fresh := map[string]float64{"pool-a": 5, "pool-b": 6}
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.Kind != "register" || ev.ToOwner != "pool-a" {
		t.Fatalf("未分派目标应按 preferred 分派给 pool-a，实际 %+v", ev)
	}
}

// 4) 全池皆墨：无健康成员 → 不动作（接无可接），归属不变。
func TestVmagentPoolNoActionWhenAllDead(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	fresh := map[string]float64{"pool-a": 99, "pool-b": 99}
	if ev := vmpTick(catDB, fresh); ev != nil {
		t.Fatalf("全池皆墨不应有任何动作，实际 %+v", ev)
	}
}

// 5) 空闲成员不算死：成员没有承载目标时仍有自监控 up 序列（hasData），接管不被 G6 误杀。
//    数据面判活按 replica 标签查 up，与承载目标数无关——此用例锁定该语义。
func TestVmagentPoolIdleMemberStillHealthy(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	// pool-b 无任何归属目标，但自监控 up 新鲜
	fresh := map[string]float64{"pool-a": 75, "pool-b": 4}
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.ToOwner != "pool-b" {
		t.Fatalf("空闲健康成员应可承接，实际 %+v", ev)
	}
}

// 6) 自动回切：A 恢复且稳定满窗 + 冷却过 → mysql-1 归位 A，B 摘任务。
func TestVmagentPoolFailbackAfterStableWindow(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	// 先制造一次接管（事件落地 → 冷却起点）
	fresh := map[string]float64{"pool-a": 75, "pool-b": 5}
	if ev := vmpTick(catDB, fresh); ev == nil || ev.Kind != "takeover" {
		t.Fatalf("前置接管失败: %+v", ev)
	}
	// 稳定窗未满（A 刚恢复 10s）→ 不回切
	freshStableShort := map[string]float64{"pool-a": 5, "pool-b": 5}
	vmpTick(catDB, freshStableShort)
	if got := ownerOf(t, catDB, "mysql-exp-1:9104"); got != "pool-b" {
		t.Fatalf("稳定窗未满不应回切，实际归属 %q", got)
	}
	// 冷却未过（刚接管 1s）→ 也不回切
	vmpSt.mu.Lock()
	vmpSt.firstHealthyAt["pool-a"] = time.Now().Unix() - int64(vmpBackStable) - 10 // 稳定窗已满
	vmpSt.mu.Unlock()
	if ev := vmpTick(catDB, freshStableShort); ev != nil {
		t.Fatalf("冷却期内不应回切，实际 %+v", ev)
	}
	// 冷却/稳定窗都过 → 回切到 preferred
	if err := catDB.UpdateVmagentTargetOwner("mysql-exp-1:9104", "pool-b"); err != nil {
		t.Fatalf("restore owner: %v", err)
	}
	// owner_since 回拨到冷却窗之外（模拟时间流逝）
	if err := catDB.BackdateVmagentTargetOwnerSince("mysql-exp-1:9104", time.Now().Unix()-int64(vmpBackCool)-10); err != nil {
		t.Fatalf("backdate owner_since: %v", err)
	}
	// 目标最近事件也要冷却窗外
	if err := catDB.BackdateVmagentPoolEvents("mysql-exp-1:9104", time.Now().Unix()-int64(vmpBackCool)-10); err != nil {
		t.Fatalf("backdate events: %v", err)
	}
	ev := vmpTick(catDB, freshStableShort)
	if ev == nil || ev.Kind != "failback" || ev.ToOwner != "pool-a" {
		t.Fatalf("稳定窗+冷却过应自动回切，实际 %+v", ev)
	}
	if got := ownerOf(t, catDB, "mysql-exp-1:9104"); got != "pool-a" {
		t.Fatalf("回切后归属应为 pool-a，实际 %q", got)
	}
}

// 7) preferred 死亡时不回切：目标留在当前健康承接端。
func TestVmagentPoolNoFailbackWhenPreferredDead(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-2:9104", "mysql", "pool-b")
	setOwnerForTest(t, catDB, "mysql-exp-2:9104", "pool-a") // 已漂到 A
	vmpSt.mu.Lock()
	vmpSt.firstHealthyAt["pool-b"] = 0
	delete(vmpSt.firstHealthyAt, "pool-b")
	vmpSt.mu.Unlock()
	fresh := map[string]float64{"pool-a": 5, "pool-b": 88} // preferred(pool-b) 仍死
	if ev := vmpTick(catDB, fresh); ev != nil {
		t.Fatalf("preferred 死亡不应回切，实际 %+v", ev)
	}
	if got := ownerOf(t, catDB, "mysql-exp-2:9104"); got != "pool-a" {
		t.Fatalf("目标应留在 pool-a，实际 %q", got)
	}
}

// 8) sd 文件同步：成员目录只含自己名下的目标文件，摘除/改派后旧文件清理。
func TestVmagentPoolSdSyncDesiredState(t *testing.T) {
	t.Chdir(t.TempDir()) // sd 根目录落进临时工作目录（vmpSdDir=data/vmagent-sd 相对路径）
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	seedTarget(t, catDB, "mysql-exp-2:9104", "mysql", "pool-b")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-2:9104", "pool-b")
	targets, _ := catDB.ListVmagentTargets()
	if err := vmpWriteMemberSd(targets, "pool-a"); err != nil {
		t.Fatalf("write sd pool-a: %v", err)
	}
	if err := vmpWriteMemberSd(targets, "pool-b"); err != nil {
		t.Fatalf("write sd pool-b: %v", err)
	}
	aFile := vmpSdDir + "/pool-a/mysql/mysql-exp-1_9104.json"
	bFile := vmpSdDir + "/pool-b/mysql/mysql-exp-2_9104.json"
	for _, p := range []string{aFile, bFile} {
		if _, err := 	os.Stat(p); err != nil {
			t.Fatalf("sd 文件缺失: %s", p)
		}
	}
	if _, err := 	os.Stat(vmpSdDir + "/pool-a/mysql/mysql-exp-2_9104.json"); err == nil {
		t.Fatalf("pool-a 不应有 pool-b 的目标文件")
	}
	// 改派后旧文件清理 + 新文件生成
	if err := catDB.UpdateVmagentTargetOwner("mysql-exp-1:9104", "pool-b"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	targets, _ = catDB.ListVmagentTargets()
	if err := vmpWriteMemberSd(targets, "pool-a"); err != nil {
		t.Fatalf("rewrite sd pool-a: %v", err)
	}
	if err := vmpWriteMemberSd(targets, "pool-b"); err != nil {
		t.Fatalf("rewrite sd pool-b: %v", err)
	}
	if _, err := 	os.Stat(aFile); err == nil {
		t.Fatalf("改派后 pool-a 的旧 sd 文件应被清理")
	}
	if _, err := 	os.Stat(vmpSdDir + "/pool-b/mysql/mysql-exp-1_9104.json"); err != nil {
		t.Fatalf("改派后 pool-b 应有新 sd 文件")
	}
}

// 8) 演练宕机接管：pool-a 数据面完全健康，但界面打了「模拟宕机」标记 → 调度器按死亡
//    处理，目标接管给 pool-b。锁定语义：演练标记 = 计划内停机，与真实故障同一接管路径。
func TestVmagentPoolDrillDownTakeover(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-1:9104", "mysql", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-1:9104", "pool-a")
	if err := catDB.SetVmagentMemberSimDown("pool-a", true); err != nil {
		t.Fatalf("set sim_down: %v", err)
	}
	// 两成员数据面都新鲜——接管只应由 sim_down 标记触发
	fresh := map[string]float64{"pool-a": 5, "pool-b": 5}
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.Kind != "takeover" || ev.ToOwner != "pool-b" {
		t.Fatalf("演练宕机应触发接管到 pool-b，实际 %+v", ev)
	}
	if got := ownerOf(t, catDB, "mysql-exp-1:9104"); got != "pool-b" {
		t.Fatalf("演练宕机后归属应为 pool-b，实际 %q", got)
	}
}

// 9) 演练恢复回切：解除标记后，preferred 恢复健康 → 稳定窗+冷却过自动归位 pool-a；
//    且解除前（标记仍在）即使数据面新鲜也绝不回切。
func TestVmagentPoolDrillRecoverFailback(t *testing.T) {
	catDB := openTestCatalog(t)
	seedTarget(t, catDB, "mysql-exp-3:9104", "mysql", "pool-a")
	setOwnerForTest(t, catDB, "mysql-exp-3:9104", "pool-a")
	if err := catDB.SetVmagentMemberSimDown("pool-a", true); err != nil {
		t.Fatalf("set sim_down: %v", err)
	}
	fresh := map[string]float64{"pool-a": 5, "pool-b": 5}
	if ev := vmpTick(catDB, fresh); ev == nil || ev.Kind != "takeover" {
		t.Fatalf("前置演练接管失败: %+v", ev)
	}
	// 标记未解除：数据面新鲜也不许回切（演练中成员就是"死"的）
	if ev := vmpTick(catDB, fresh); ev != nil && ev.Kind == "failback" {
		t.Fatalf("演练标记未解除不应回切，实际 %+v", ev)
	}
	// 解除标记 + 冷却窗回拨（模拟时间流逝）
	if err := catDB.SetVmagentMemberSimDown("pool-a", false); err != nil {
		t.Fatalf("clear sim_down: %v", err)
	}
	if err := catDB.BackdateVmagentTargetOwnerSince("mysql-exp-3:9104", time.Now().Unix()-int64(vmpBackCool)-10); err != nil {
		t.Fatalf("backdate owner_since: %v", err)
	}
	if err := catDB.BackdateVmagentPoolEvents("mysql-exp-3:9104", time.Now().Unix()-int64(vmpBackCool)-10); err != nil {
		t.Fatalf("backdate events: %v", err)
	}
	vmpSt.mu.Lock()
	vmpSt.firstHealthyAt["pool-a"] = time.Now().Unix() - int64(vmpBackStable) - 10
	vmpSt.mu.Unlock()
	ev := vmpTick(catDB, fresh)
	if ev == nil || ev.Kind != "failback" || ev.ToOwner != "pool-a" {
		t.Fatalf("演练恢复+稳定窗满应自动回切，实际 %+v", ev)
	}
	if got := ownerOf(t, catDB, "mysql-exp-3:9104"); got != "pool-a" {
		t.Fatalf("回切后归属应为 pool-a，实际 %q", got)
	}
}
