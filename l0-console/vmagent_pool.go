package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ============================================================
// vmagent 池：抓取目标单归属 + 故障接管 + 自动回切
// 与 relocate.go（跨 L1 采集机漂移）平行的第二套调度，镜像其机制：
//   单归属（同一目标同一时刻只归一个成员）· G6 池保护（健康成员 <2 不迁移）
//   · 冷却防抖 · 每轮至多一个动作。新增回切：preferred_owner 恢复健康且稳定
//   满 BACK_STABLE 秒后自动归位（先加后删，切换无断点）。
// 生产红线（2026-09-27 用户约定）：零 docker 依赖——判活只读 VM 数据面
// （replica 标签序列新鲜度），接管只写 PG + sd 文件，任何环境（裸机 systemd）
// 行为一致。docker.sock 仅存在于既有 l1-* 演示探测，本模块不碰。
// ============================================================

var (
	vmpAutoOn     = envOr("VMAGENT_POOL_AUTO", "1") != ""            // "1" 启用自动调度；置空可关
	vmpScanSec    = envIntOr("VMAGENT_POOL_SCAN_SEC", 10)            // 调度轮询间隔
	vmpFailSec    = envIntOr("VMAGENT_POOL_FAIL_SEC", 60)            // 成员失效判定：数据面新鲜度超此值判死。
	// 注意：VM 默认 -search.latencyOffset=30s，instant 查询看到的样本时间恒比真实晚 ≤30s，
	// 活成员的可见数据龄本底就在 30~45s（15s 抓取间隔）。阈值若取 30s 会把全员永久误判为死
	// （2026-09-27 实测踩坑）。取 60s = 真实停更 ~30s 触发（与采集机池 RELOCATE_FAIL_SEC 同效），误判余量 15s。
	vmpBackStable = envIntOr("VMAGENT_POOL_BACK_STABLE_SEC", 120)    // 回切稳定窗：preferred 持续健康多久才准回切
	vmpBackCool   = envIntOr("VMAGENT_POOL_BACK_COOLDOWN_SEC", 300)  // 回切冷却：归属变更后多久内不回切
	vmpSdDir      = envOr("VMAGENT_SD_DIR", "data/vmagent-sd")       // sd 根目录（按成员分子目录）
)

// vmpMemberHealth 单成员健康快照（数据面）。
type vmpMemberHealth struct {
	Member   string  `json:"member"`
	HasData  bool    `json:"has_data"`  // VM 里存在该 replica 的 up 序列
	FreshSec float64 `json:"fresh_sec"` // 最新样本距今秒数（-1 = 无数据）
	Healthy  bool    `json:"healthy"`
	SimDown  bool    `json:"sim_down"` // 演练宕机标记（界面按钮置位，调度器按死亡处理）
	Load     int     `json:"load"`     // 当前承载目标数
}

// vmpState 调度器跨轮状态（内存）：preferred 成员持续健康的起点（回切稳定窗判据）。
type vmpState struct {
	mu             sync.Mutex
	firstHealthyAt map[string]int64 // member → 首次转健康的 unix 秒；不健康即删除
	health         map[string]*vmpMemberHealth
}

var vmpSt = &vmpState{firstHealthyAt: map[string]int64{}, health: map[string]*vmpMemberHealth{}}

// vmpQueryMemberFreshness 数据面判活：一次查询拿到全部成员的最新写入时间。
// up 是 vmagent 为每个抓取目标合成的存活序列，随 remoteWrite 落库时带 replica 标签；
// 成员自身还静态抓 localhost:8429（vmagent 自监控，scrape.yml 固定），保证零目标的
// 空闲成员也有 up 序列——否则"空闲"会被误判成"死了"，接管时 G6 会错杀唯一承接端。
func vmpQueryMemberFreshness() (map[string]float64, error) {
	body, err := vmQuery(`max by (replica) (timestamp(up))`)
	if err != nil {
		return nil, err
	}
	return vmpParseMemberFreshness(body)
}

// vmpParseMemberFreshness 解析 VM instant query 响应（replica → 最新样本距今秒数）。
// value[1] 是字符串数字（VM 行为），解析失败跳过该行——脏样本不推翻整份健康快照。
func vmpParseMemberFreshness(body []byte) (map[string]float64, error) {
	var v struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []interface{}     `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("VM 响应非 JSON：%w", err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	out := map[string]float64{}
	for _, r := range v.Data.Result {
		rep := r.Metric["replica"]
		if rep == "" || len(r.Value) < 2 {
			continue
		}
		raw, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		ts, err := parseUnixFloat(raw)
		if err != nil {
			continue
		}
		age := now - ts
		if cur, seen := out[rep]; !seen || age < cur {
			out[rep] = age
		}
	}
	return out, nil
}

// parseUnixFloat VM 时间戳是秒（带小数），独立小解析避免依赖 collect_stats 的行解析口径。
func parseUnixFloat(raw string) (float64, error) {
	var f float64
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return 0, err
	}
	return f, nil
}

// vmpMemberSet 成员全集 = 目标表的归属值 ∪ VM 里报上来的 replica（防"新成员未接目标
// 就被当不存在"）。稳定排序保证调度决策可复现。
func vmpMemberSet(targets []*storepkg.VmagentTargetRow, fresh map[string]float64) []string {
	seen := map[string]bool{}
	for _, t := range targets {
		if t.CurrentOwner != "" {
			seen[t.CurrentOwner] = true
		}
		if t.PreferredOwner != "" {
			seen[t.PreferredOwner] = true
		}
	}
	for m := range fresh {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// vmpHealthyMembers 健康成员 = VM 有数据且新鲜度在失效阈值内，且未被打演练宕机标记。
// 演练标记（界面「模拟宕机」按钮）语义 = 计划内停机：数据面照常在跑，调度器按死亡处理。
func vmpHealthyMembers(fresh map[string]float64, simDown map[string]bool) []string {
	out := []string{}
	for m, age := range fresh {
		if age >= 0 && age <= float64(vmpFailSec) && !simDown[m] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// vmpPickOwner 接管选主：preferred（若健康）优先，否则负载最低（承载目标数并列取名字最小，
// 确定性可复现）。loads 为各成员当前承载的目标数。
func vmpPickOwner(preferred string, healthy []string, loads map[string]int) string {
	healthySet := map[string]bool{}
	for _, m := range healthy {
		healthySet[m] = true
	}
	if preferred != "" && healthySet[preferred] {
		return preferred
	}
	best := ""
	for _, m := range healthy {
		if best == "" || loads[m] < loads[best] || (loads[m] == loads[best] && m < best) {
			best = m
		}
	}
	return best
}

// vmpSanitizeKey 目标键作文件名：冒号/斜杠替换，防路径逃逸与文件系统歧义。
func vmpSanitizeKey(k string) string {
	return strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(k)
}

// vmpWriteMemberSd 把某成员的 sd 目录同步为"它当前拥有的目标"（desired state 全量对齐）：
// 有主且启用的目标写 <job>/<key>.json，其余文件删除。临时文件+rename 原子落笔，
// vmagent file_sd 热加载不会读到半行 JSON。目录不存在时自动创建（首次部署零手工步骤）。
func vmpWriteMemberSd(targets []*storepkg.VmagentTargetRow, member string) error {
	if member == "" {
		return nil
	}
	base := filepath.Join(vmpSdDir, member)
	desired := map[string][]byte{}
	for _, t := range targets {
		if !t.Enabled || t.CurrentOwner != member {
			continue
		}
		doc := []map[string]any{{
			"targets": []string{t.TargetKey},
			"labels": map[string]string{
				"resource_id": t.DisplayName,
				"source":      "vmagent_pool",
			},
		}}
		if t.DisplayName == "" {
			doc[0]["labels"].(map[string]string)["resource_id"] = t.TargetKey
		}
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		desired[filepath.Join(t.JobName, vmpSanitizeKey(t.TargetKey)+".json")] = b
	}
	// 删除目录里不在 desired 集合内的旧文件（含 job 子目录整体清理）
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		if _, keep := desired[rel]; !keep {
			_ = os.Remove(p)
		}
		return nil
	})
	for rel, b := range desired {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
	}
	return nil
}

// vmpRewriteAllSd 全成员 sd 对齐（归属变更/目标增删后调用）。
func vmpRewriteAllSd(targets []*storepkg.VmagentTargetRow) {
	members := map[string]bool{}
	for _, t := range targets {
		if t.CurrentOwner != "" {
			members[t.CurrentOwner] = true
		}
		if t.PreferredOwner != "" {
			members[t.PreferredOwner] = true
		}
	}
	for m := range members {
		if err := vmpWriteMemberSd(targets, m); err != nil {
			log.Printf("vmagent-pool: sd rewrite failed for %s: %v", m, err)
		}
	}
}

// vmpCooldownActive 目标最近一次事件在冷却期内（防抖：接管/回切都不在冷却期内重复动作）。
func vmpCooldownActive(catDB *storepkg.DB, targetKey string) bool {
	e, err := catDB.LatestVmagentPoolEventByTarget(targetKey)
	if err != nil || e == nil {
		return false
	}
	return time.Now().Unix()-e.CreatedAt < int64(vmpBackCool)
}

// vmpTick 调度一轮：失败检测 → 接管 → 回切，至多执行一个动作（镜像 autoRelocate 的
// 每轮单目标纪律，避免连锁风暴）。返回本轮执行的事件（无动作返回 nil）。
// fresh 由调用方注入（生产=VM 实查，测试=构造值），决策全部纯函数化保证可测。
func vmpTick(catDB *storepkg.DB, fresh map[string]float64) *storepkg.VmagentPoolEvent {
	targets, err := catDB.ListVmagentTargets()
	if err != nil {
		log.Printf("vmagent-pool: list targets: %v", err)
		return nil
	}
	now := time.Now().Unix()
	simDown, err := catDB.ListVmagentMemberSimDown()
	if err != nil {
		log.Printf("vmagent-pool: list sim_down flags: %v", err)
		simDown = map[string]bool{} // 标记读不到不扩大故障面：按无演练处理
	}
	healthy := vmpHealthyMembers(fresh, simDown)
	healthySet := map[string]bool{}
	for _, m := range healthy {
		healthySet[m] = true
	}
	loads := map[string]int{}
	for _, t := range targets {
		if t.Enabled {
			loads[t.CurrentOwner]++
		}
	}

	// 稳定窗记账：健康起点 / 掉线清零
	vmpSt.mu.Lock()
	for _, m := range healthy {
		if _, ok := vmpSt.firstHealthyAt[m]; !ok {
			vmpSt.firstHealthyAt[m] = now
		}
	}
	for m := range vmpSt.firstHealthyAt {
		if !healthySet[m] {
			delete(vmpSt.firstHealthyAt, m)
		}
	}
	snap := map[string]*vmpMemberHealth{}
	for _, m := range vmpMemberSet(targets, fresh) {
		age, has := fresh[m]
		h := &vmpMemberHealth{Member: m, Load: loads[m], FreshSec: -1, SimDown: simDown[m]}
		if has {
			h.FreshSec = age
			h.HasData = true
			h.Healthy = age >= 0 && age <= float64(vmpFailSec) && !simDown[m]
		}
		snap[m] = h
	}
	vmpSt.health = snap
	vmpSt.mu.Unlock()

	// ---- 接管：属主死亡 / 未分派 ----
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		ownerDead := t.CurrentOwner == "" || !healthySet[t.CurrentOwner]
		if !ownerDead {
			continue
		}
		if len(healthy) < 1 {
			// 无健康承接端：接无可接（与采集机池 G6 的 ≥2 保护不同口径——vmagent 池按
			// 场景定义只有两个成员，A 挂后健康数=1 正是接管时机；若沿用 ≥2 会把
			// 「B 接管」整个堵死。这里只挡"全池皆墨"的空转，并审计留痕）。
			log.Printf("vmagent-pool: 池内无健康成员，目标 %s 无从接管，转告警", t.TargetKey)
			continue
		}
		if vmpCooldownActive(catDB, t.TargetKey) {
			continue
		}
		to := vmpPickOwner(t.PreferredOwner, healthy, loads)
		if to == "" || to == t.CurrentOwner {
			continue
		}
		from := t.CurrentOwner
		if err := catDB.UpdateVmagentTargetOwner(t.TargetKey, to); err != nil {
			log.Printf("vmagent-pool: owner update: %v", err)
			return nil
		}
		freshTargets, _ := catDB.ListVmagentTargets()
		vmpRewriteAllSd(freshTargets)
		ev := &storepkg.VmagentPoolEvent{
			TargetKey: t.TargetKey, FromOwner: from, ToOwner: to,
			Kind: takeoverKind(t, from), Reason: "auto: owner 数据面失效(>" + fmt.Sprint(vmpFailSec) + "s) 或未分派", Status: "moved",
		}
		if from == "" {
			ev.Reason = "register: 首次分派"
		}
		if _, err := catDB.RecordVmagentPoolEvent(ev); err != nil {
			log.Printf("vmagent-pool: record event: %v", err)
		}
		addAudit("vmagent池接管", t.TargetKey, fromOrNone(from)+" → "+to, "成功")
		return ev
	}

	// ---- 回切：preferred 恢复健康且稳定满窗 + 冷却已过 ----
	for _, t := range targets {
		if !t.Enabled || t.PreferredOwner == "" || t.PreferredOwner == t.CurrentOwner {
			continue
		}
		if !healthySet[t.PreferredOwner] {
			continue
		}
		fh, ok := vmpSt.firstHealthyAt[t.PreferredOwner]
		if !ok || now-fh < int64(vmpBackStable) {
			continue // 稳定窗未满：防"恢复即崩溃"抖动
		}
		if now-t.OwnerSince < int64(vmpBackCool) || vmpCooldownActive(catDB, t.TargetKey) {
			continue
		}
		from := t.CurrentOwner
		if err := catDB.UpdateVmagentTargetOwner(t.TargetKey, t.PreferredOwner); err != nil {
			log.Printf("vmagent-pool: failback update: %v", err)
			return nil
		}
		freshTargets, _ := catDB.ListVmagentTargets()
		vmpRewriteAllSd(freshTargets)
		ev := &storepkg.VmagentPoolEvent{
			TargetKey: t.TargetKey, FromOwner: from, ToOwner: t.PreferredOwner,
			Kind: "failback", Reason: "auto: preferred 恢复健康且稳定 " + fmt.Sprint(vmpBackStable) + "s，自动回切", Status: "moved",
		}
		if _, err := catDB.RecordVmagentPoolEvent(ev); err != nil {
			log.Printf("vmagent-pool: record event: %v", err)
		}
		addAudit("vmagent池回切", t.TargetKey, fromOrNone(from)+" → "+t.PreferredOwner, "成功")
		return ev
	}
	return nil
}

// takeoverKind 事件类型：首次分派 vs 故障接管。
func takeoverKind(t *storepkg.VmagentTargetRow, from string) string {
	if from == "" {
		return "register"
	}
	return "takeover"
}

func fromOrNone(s string) string {
	if s == "" {
		return "（未分派）"
	}
	return s
}

// startVmagentPoolAuto 后台调度循环：VM 实查新鲜度 → vmpTick。零 docker 依赖。
func startVmagentPoolAuto(catDB *storepkg.DB) {
	if !vmpAutoOn {
		return
	}
	go func() {
		for {
			func() {
				defer func() { recover() }()
				if fresh, err := vmpQueryMemberFreshness(); err == nil {
					vmpTick(catDB, fresh)
				} else {
					log.Printf("vmagent-pool: freshness query: %v", err)
				}
			}()
			time.Sleep(time.Duration(vmpScanSec) * time.Second)
		}
	}()
}

// registerVmagentPoolRoutes 池 API：状态总览 / 事件时间线 / 目标注册与摘除 / 手工改派。
func registerVmagentPoolRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	// GET /api/vmagent-pool —— 界面池视图数据源：成员健康（数据面实测）+ 目标归属 + 负载
	mux.HandleFunc("/api/vmagent-pool", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		targets, err := catDB.ListVmagentTargets()
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		vmpSt.mu.Lock()
		health := make([]*vmpMemberHealth, 0, len(vmpSt.health))
		for _, h := range vmpSt.health {
			health = append(health, h)
		}
		vmpSt.mu.Unlock()
		sort.Slice(health, func(i, j int) bool { return health[i].Member < health[j].Member })
		writeJSON(w, map[string]interface{}{"members": health, "targets": targets})
	})
	// GET /api/vmagent-pool/events —— 切换时间线（界面轮询即可看到接管/回切效果）
	mux.HandleFunc("/api/vmagent-pool/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		rows, _ := catDB.ListVmagentPoolEvents(50)
		writeJSON(w, map[string]interface{}{"events": rows})
	})
	// POST /api/vmagent-pool/members/drill —— 演练宕机开关（界面「模拟宕机/恢复上线」按钮落点）
	// {member:"pool-a", down:true}。语义=计划内停机演练：调度器下一轮（≤10s）把该成员按
	// 死亡处理 → 目标自动接管；down=false 解除后数据面确认健康 → 走正常自动回切。
	// 零 docker 依赖：标记只落 PG，生产同构可用。
	mux.HandleFunc("/api/vmagent-pool/members/drill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			Member string `json:"member"`
			Down   bool   `json:"down"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.Member == "" {
			writeJSON(w, map[string]interface{}{"error": "member 必填"})
			return
		}
		if err := catDB.SetVmagentMemberSimDown(q.Member, q.Down); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		kind := "drill_recover"
		reason := "演练: 解除模拟宕机，等待数据面确认后自动回切"
		if q.Down {
			kind = "drill_down"
			reason = "演练: 界面模拟成员宕机，目标将被健康成员接管"
		}
		_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
			TargetKey: q.Member, Kind: kind, Reason: reason, Status: "moved",
		})
		addAudit("vmagent池演练", q.Member, map[bool]string{true: "模拟宕机", false: "恢复上线"}[q.Down], "成功")
		writeJSON(w, map[string]interface{}{"ok": true})
	})
	// POST /api/vmagent-pool/targets —— 注册目标 {target_key,display_name,job_name,preferred_owner}
	mux.HandleFunc("/api/vmagent-pool/targets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetKey      string `json:"target_key"`
			DisplayName    string `json:"display_name"`
			JobName        string `json:"job_name"`
			PreferredOwner string `json:"preferred_owner"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.TargetKey == "" {
			writeJSON(w, map[string]interface{}{"error": "target_key 必填"})
			return
		}
		if q.JobName == "" {
			q.JobName = "default"
		}
		t := &storepkg.VmagentTargetRow{
			TargetKey: q.TargetKey, DisplayName: q.DisplayName, JobName: q.JobName,
			PreferredOwner: q.PreferredOwner, Enabled: true,
		}
		if _, err := catDB.UpsertVmagentTarget(t); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		fresh, _ := catDB.ListVmagentTargets()
		vmpRewriteAllSd(fresh)
		_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
			TargetKey: q.TargetKey, ToOwner: q.PreferredOwner, Kind: "register",
			Reason: "api 注册", Status: "moved",
		})
		addAudit("vmagent池注册目标", q.TargetKey, "preferred="+orDash(q.PreferredOwner), "成功")
		writeJSON(w, map[string]interface{}{"ok": true})
	})
	// POST /api/vmagent-pool/targets/remove —— 摘除目标 {target_key}
	mux.HandleFunc("/api/vmagent-pool/targets/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetKey string `json:"target_key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.TargetKey == "" {
			writeJSON(w, map[string]interface{}{"error": "target_key 必填"})
			return
		}
		if err := catDB.DeleteVmagentTarget(q.TargetKey); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		fresh, _ := catDB.ListVmagentTargets()
		vmpRewriteAllSd(fresh)
		_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
			TargetKey: q.TargetKey, Kind: "remove", Reason: "api 摘除", Status: "moved",
		})
		addAudit("vmagent池摘除目标", q.TargetKey, "-", "成功")
		writeJSON(w, map[string]interface{}{"ok": true})
	})
	// POST /api/vmagent-pool/assign —— 手工改派 {target_key, owner}（kind=assign，同样受冷却保护）
	mux.HandleFunc("/api/vmagent-pool/assign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var q struct {
			TargetKey string `json:"target_key"`
			Owner     string `json:"owner"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.TargetKey == "" || q.Owner == "" {
			writeJSON(w, map[string]interface{}{"error": "target_key / owner 必填"})
			return
		}
		if vmpCooldownActive(catDB, q.TargetKey) {
			writeJSON(w, map[string]interface{}{"error": "冷却期内，稍后再试"})
			return
		}
		t, err := catDB.GetVmagentTarget(q.TargetKey)
		if err != nil || t == nil {
			writeJSON(w, map[string]interface{}{"error": "target not found"})
			return
		}
		if err := catDB.UpdateVmagentTargetOwner(q.TargetKey, q.Owner); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		fresh, _ := catDB.ListVmagentTargets()
		vmpRewriteAllSd(fresh)
		_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
			TargetKey: q.TargetKey, FromOwner: t.CurrentOwner, ToOwner: q.Owner,
			Kind: "assign", Reason: "手工改派", Status: "moved",
		})
		addAudit("vmagent池改派", q.TargetKey, fromOrNone(t.CurrentOwner)+" → "+q.Owner, "成功")
		writeJSON(w, map[string]interface{}{"ok": true})
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// vmpRegisterOnboarded 接入装机后的池登记：目标进归属表（preferred 空 → 调度器
// 下一轮自动分派给负载最低的健康成员），sd 文件由 vmpRewriteAllSd 落笔。
// 取代旧的"直写共享 scrape-sd 目录"路径——池化后 file_sd 账本由调度器独占落笔，
// 其他写入者只进归属表，杜绝两边写互相覆盖。
func vmpRegisterOnboarded(catDB *storepkg.DB, resourceID, addr string) error {
	if catDB == nil {
		return nil
	}
	t := &storepkg.VmagentTargetRow{TargetKey: addr, DisplayName: resourceID, JobName: "onboarded", Enabled: true}
	if _, err := catDB.UpsertVmagentTarget(t); err != nil {
		return err
	}
	targets, _ := catDB.ListVmagentTargets()
	vmpRewriteAllSd(targets)
	_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
		TargetKey: addr, Kind: "register", Reason: "onboard: 装机成功自动登记", Status: "moved",
	})
	return nil
}

// vmpUnregisterOnboarded 卸载侧池摘除：按资源名找 onboarded 目标并删除，sd 同步清理。
func vmpUnregisterOnboarded(catDB *storepkg.DB, resourceID string) error {
	if catDB == nil {
		return nil
	}
	targets, err := catDB.ListVmagentTargets()
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.JobName == "onboarded" && t.DisplayName == resourceID {
			if err := catDB.DeleteVmagentTarget(t.TargetKey); err != nil {
				return err
			}
			fresh, _ := catDB.ListVmagentTargets()
			vmpRewriteAllSd(fresh)
			_, _ = catDB.RecordVmagentPoolEvent(&storepkg.VmagentPoolEvent{
				TargetKey: t.TargetKey, Kind: "remove", Reason: "onboard: 卸载摘除", Status: "moved",
			})
			return nil
		}
	}
	return nil
}
