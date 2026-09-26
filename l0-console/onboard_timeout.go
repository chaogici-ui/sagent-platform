package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  步骤时间治理：超时判定 + 有限自动重试 + 执行心跳
//
//  用户要求（2026-09-21）：「每个步骤都要有超时时间，不能一直无底线的等待，
//  但可以增加一点点重试机会」。
//
//  三条设计约束：
//   ① 判定不能依赖"有人打开页面"：后台定时清扫（sweepOnboardTimeouts），
//      进程重启后遗留的 running 步骤同样会被收敛
//   ② 双阈值而不是单阈值：
//      run  —— 单次尝试总时长（作业卡死、连接挂着慢慢磨）
//      idle —— 无任何进展/心跳（进程重启后孤儿、人工不回报）
//   ③ 重试是"一点"：次数来自配置（默认 1 次），用完就判死并给出可执行建议；
//      重试期间旧作业的回报一律丢弃，否则会把新尝试的结果覆盖成旧的
//
//  判定粒度按「执行路径」而不是只看 atom：
//  平台 ansible 代执行 → 用 atom 规格（紧）
//  回落人工执行      → 用 default 规格（松。人手操作慢是常态，但同样不允许无限期挂着）
// ===================================================================

const (
	// stepHeartbeatInterval 执行心跳间隔：平台代执行的步骤在跑就持续打点，
	// 让 idle 判定只对"真的没人在干活"生效（长命令静默 ≠ 卡死）
	stepHeartbeatInterval = 15 * time.Second
	// sweepInterval 后台清扫间隔
	sweepInterval = 20 * time.Second
)

// ===================================================================
//  执行层预算：config 是超时唯一真源
//
//  清扫器（sweepOnboardTimeouts）是"超时权威"，配置里的 RunSec/IdleSec 就是它
//  的判据。但执行层还有一层 context.WithTimeout 兜底——它一度写死成常量，
//  于是出现「界面倒计时按 900s 走、实际 300s 就被判死」的谎报：运维盯着
//  「剩余 12m」却看到步骤猝死，第一反应是平台在撒谎。
//
//  规则（回归有断言）：
//   ① 任何单个子命令的预算都由 RunSec 按比例推导，改配置即改全局
//   ② 步骤级兜底不得早于清扫器上限——execBudget 带 execGrace 宽限，
//      保证"谁先判"永远是清扫器（它才带重试语义）
// ===================================================================
const (
	// 各阶段占 RunSec 的比例（探路各段合计约 0.68，留足余量给调度抖动）
	budgetPing     = 0.10 // 联通性测试
	budgetPortCmd  = 0.075 // 端口核对（3 条命令，各占一份）
	budgetSetup    = 0.20 // facts 采集
	budgetDisk     = 0.05 // 磁盘兜底采集
	budgetCheck    = 0.04 // 能力可行性检查：每条检查项一份预算（探不通也要等连接超时）
	budgetPlaybook = 1.00 // 安装 playbook 就是本步骤主体，吃满 RunSec

	// execGrace 执行层 ctx 相对清扫器的宽限。清扫器先到 → 由它决定重试与否；
	// 执行层 ctx 只是进程内兜底（清扫器线程被卡住时的最后一道保险）
	execGrace = 30 * time.Second
)

// phaseBudget 子命令预算 = RunSec × weight（下限 5s，避免配置成 0 时秒退）
func phaseBudget(spec StepTimeout, weight float64) time.Duration {
	sec := float64(spec.RunSec) * weight
	if sec < 5 {
		sec = 5
	}
	return time.Duration(sec) * time.Second
}

// execBudget 步骤级兜底 ctx：RunSec × weight + 宽限
func execBudget(spec StepTimeout, weight float64) time.Duration {
	return phaseBudget(spec, weight) + execGrace
}

// budgetSec 预算的人话秒数（诊断文案用，保证文案里的数字与实际 ctx 同源）
func budgetSec(d time.Duration) int { return int(d.Seconds()) }

// timeoutSpecFor 取步骤的超时规格：atom 优先，缺省回落 default。
// 空 atom（人工路径调用）直接取 default
func timeoutSpecFor(cfg *OnboardConfig, atom string) StepTimeout {
	return timeoutSpecForStep(cfg, "", atom)
}

// timeoutSpecForStep 在 atom 之上再加一层「步骤 ID」覆盖（2026-09-21 五阶段评审）：
// 同一 atom 的不同步骤允许不同节奏——self_metrics 与 observe_collect 共用 observe_collect
// atom（都是等 Agent 心跳回报），但注册回报是分钟级（180s 无进展就该失败），
// 采集入库要观察一个采集周期（600s）。解析顺序：步骤 ID > atom > default
func timeoutSpecForStep(cfg *OnboardConfig, stepID, atom string) StepTimeout {
	def := StepTimeout{RunSec: 3600, IdleSec: 3600, Retry: 0, RetryDelaySec: 15}
	if cfg != nil && cfg.StepTimeouts != nil {
		if v, ok := cfg.StepTimeouts["default"]; ok {
			def = v
		}
		if atom != "" {
			if v, ok := cfg.StepTimeouts[atom]; ok {
				def = v
			}
		}
		if stepID != "" {
			if v, ok := cfg.StepTimeouts[stepID]; ok {
				def = v
			}
		}
	}
	if def.RunSec <= 0 {
		def.RunSec = 3600
	}
	if def.IdleSec <= 0 {
		def.IdleSec = def.RunSec
	}
	if def.Retry < 0 {
		def.Retry = 0
	}
	return def
}

// stepProgress 写在 running 事件 detail.progress 里的运行期元数据。
// 超时判定不靠事件时间戳猜：attempt 起点、最后活动时间、当前任务都在这里，
// 界面也直接拿它渲染「第几次尝试 / 跑到哪了 / 还剩多久」
type stepProgress struct {
	Attempt        int         `json:"attempt"`
	MaxAttempts    int         `json:"max_attempts"`
	StartedAt      string      `json:"attempt_started_at"`
	LastActivityAt string      `json:"last_activity_at"`
	Phase          string      `json:"phase,omitempty"`
	CurrentTask    string      `json:"current_task,omitempty"`
	TasksDone      int         `json:"tasks_done"`
	TasksTotal     int         `json:"tasks_total,omitempty"`
	DoneTasks      []string    `json:"done_tasks,omitempty"`
	Timeout        StepTimeout `json:"timeout"`
}

// attemptRec 一次尝试的结局（第几次、跑了多久、怎么结束的）。
// 超时重试的每一步都留痕：运维能看见"已经自动试过 2 次、每次卡在什么上限"
type attemptRec struct {
	No        int    `json:"no"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Result    string `json:"result,omitempty"` // ok / fail / timeout_idle / timeout_run
	WaitedSec int64  `json:"waited_sec,omitempty"`
	Note      string `json:"note,omitempty"`
}

// progressOf 从事件 detail 里取进度元数据（老事件没有 → nil，判定走时间戳兜底）
func progressOf(detail string) *stepProgress {
	if detail == "" {
		return nil
	}
	var d struct {
		Progress *stepProgress `json:"progress"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return nil
	}
	return d.Progress
}

// attemptsOf 取历史尝试记录
func attemptsOf(detail string) []attemptRec {
	if detail == "" {
		return nil
	}
	var d struct {
		Attempts []attemptRec `json:"attempts"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return nil
	}
	return d.Attempts
}

// platformManaged 该 running 事件是否由平台 ansible 代执行产生（决定用紧规格还是松规格）
func platformManaged(detail string) bool {
	if detail == "" {
		return false
	}
	var d struct {
		Executor string `json:"executor"`
		Runner   string `json:"runner"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return false
	}
	return d.Executor == "ansible" && d.Runner == "platform"
}

// ===================================================================
//  一次步骤尝试的共享状态：作业更新它，心跳与进度事件读它落库
// ===================================================================

type stepRunState struct {
	mu      sync.Mutex
	flowID  int64
	step    storepkg.FlowStepSnapshot
	cred    *sshCred
	attempt int
	history []attemptRec
	spec    StepTimeout
	started time.Time
	lastAct time.Time
	phase   string
	current string
	tasks   []ansibleTask
	diags   []diag
	lastLog time.Time
}

// newStepRunState 开启一次尝试：attempt 从事件里续（超时重试会先写 attempt+1 的 running 事件）
func newStepRunState(catDB *storepkg.DB, flowID int64, step storepkg.FlowStepSnapshot, cred *sshCred) *stepRunState {
	attempt, history := 1, []attemptRec{}
	if ev, _ := catDB.LatestEvent(flowID, step.ID); ev != nil {
		if pr := progressOf(ev.Detail); pr != nil && pr.Attempt > 0 {
			attempt = pr.Attempt
		}
		history = attemptsOf(ev.Detail)
	}
	spec := timeoutSpecFor(onboardCfg, step.Atom)
	now := time.Now()
	return &stepRunState{
		flowID: flowID, step: step, cred: cred,
		attempt: attempt, history: history, spec: spec,
		started: now, lastAct: now,
	}
}

// phase 标记当前阶段（探路有 ping / 端口核对 / setup / df 四段，界面按阶段分组看）
func (s *stepRunState) setPhase(phase string) {
	s.mu.Lock()
	s.phase = phase
	s.current = ""
	s.touchLocked()
	s.mu.Unlock()
}

func (s *stepRunState) touchLocked() { s.lastAct = time.Now() }

// addDiag 累积诊断（同一份诊断在进度与终态事件里都要带上）
func (s *stepRunState) addDiag(ds ...diag) {
	s.mu.Lock()
	s.diags = append(s.diags, ds...)
	s.mu.Unlock()
}

func (s *stepRunState) setDiags(ds []diag) {
	s.mu.Lock()
	s.diags = ds
	s.mu.Unlock()
}

// taskHook 给流式执行器的任务回调：把每个任务（含原文、状态、失败原因）并入状态并落一次进度。
// 这就是「安装过程中的所有信息回到界面」的落点——不是跑完给一坨文本，而是逐任务可见
func (s *stepRunState) taskHook(catDB *storepkg.DB) func(ansibleTask, int) {
	return func(t ansibleTask, done int) {
		s.mu.Lock()
		t.Phase = s.phase
		s.tasks = append(s.tasks, t)
		s.current = t.Name
		s.touchLocked()
		s.mu.Unlock()
		s.flush(catDB, s.progressSummary(done, t))
	}
}

// progressSummary 进度摘要：任务序号 + 当前 TASK 名 + 状态
func (s *stepRunState) progressSummary(done int, t ansibleTask) string {
	stage := s.phaseLabel()
	if t.Name != "" {
		return fmt.Sprintf("平台 ansible 执行中 · %s · %d 个任务完成 · TASK [%s] %s",
			stage, done, t.Name, taskStatusLabel(t.Status))
	}
	return fmt.Sprintf("平台 ansible 执行中 · %s · %d 个任务完成", stage, done)
}

func (s *stepRunState) phaseLabel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case "ping":
		return "① 联通性测试"
	case "port_check":
		return "② 端口核对"
	case "setup":
		return "③ 事实采集"
	case "df":
		return "④ 磁盘兜底采集"
	case "syntax":
		return "playbook 语法校验"
	case "playbook":
		return "安装 playbook"
	case "register":
		return "采集目标登记"
	}
	return "执行中"
}

// snapshot 取进度快照（锁内构造，避免并发改）
func (s *stepRunState) snapshot() (stepProgress, []ansibleTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := stepProgress{
		Attempt: s.attempt, MaxAttempts: s.spec.Retry + 1,
		StartedAt:      s.started.Format("2006-01-02 15:04:05"),
		LastActivityAt: s.lastAct.Format("2006-01-02 15:04:05"),
		Phase:          s.phase, CurrentTask: s.current,
		TasksDone: len(s.tasks), TasksTotal: len(s.tasks),
		Timeout: s.spec,
	}
	names := make([]string, 0, len(s.tasks))
	for _, t := range s.tasks {
		names = append(names, t.Name)
	}
	pr.DoneTasks = names
	return pr, append([]ansibleTask{}, s.tasks...)
}

// flush 落一条 running 进度事件（界面实时可见）。
// 任务快照用精简版（不带逐任务原文行），否则心跳会把事件表撑大
func (s *stepRunState) flush(catDB *storepkg.DB, summary string) {
	pr, tasks := s.snapshot()
	s.mu.Lock()
	hist := append([]attemptRec{}, s.history...)
	ds := append([]diag{}, s.diags...)
	s.mu.Unlock()

	detail := map[string]any{
		"executor": "ansible", "runner": "platform",
		"conn": credState(s.cred), "target": s.cred.target(),
		"progress": pr, "tasks": compactTasks(tasks), "attempts": hist,
		"note": "平台持有资源 SSH 凭据，ansible 自动执行；完成后自动回报，无需人工操作",
	}
	if len(ds) > 0 {
		detail["diagnosis"] = diagJSON(ds)
	}
	_ = appendStepEvent(catDB, s.flowID, s.step, stRunning, summary, detailJSON(detail), 0)
}

// heartbeat 心跳：作业活着就持续打点（长命令静默不等于卡死），
// idle 判定因此只对"真的没人在干活"生效
func (s *stepRunState) heartbeat(catDB *storepkg.DB) {
	s.mu.Lock()
	s.touchLocked()
	pr, _ := s.snapshotLocked()
	elapsed := int64(time.Since(s.started).Seconds())
	s.mu.Unlock()

	stage := "执行中"
	if pr.CurrentTask != "" {
		stage = "TASK [" + pr.CurrentTask + "]"
	}
	summary := fmt.Sprintf("平台 ansible 执行中 · %d 个任务完成 · %s · 已运行 %ds（心跳）",
		len(pr.DoneTasks), stage, elapsed)
	// 已运行秒数写进文案：心跳文案必须每次不同，否则会被幂等去重（同 summary 不重复写），
	// 超时判定就读不到新鲜的 last_activity
	s.flush(catDB, summary)
}

// snapshotLocked 调用方已持锁时使用
func (s *stepRunState) snapshotLocked() (stepProgress, []ansibleTask) {
	pr := stepProgress{
		Attempt: s.attempt, MaxAttempts: s.spec.Retry + 1,
		StartedAt:      s.started.Format("2006-01-02 15:04:05"),
		LastActivityAt: s.lastAct.Format("2006-01-02 15:04:05"),
		Phase:          s.phase, CurrentTask: s.current,
		TasksDone: len(s.tasks), TasksTotal: len(s.tasks),
		Timeout: s.spec,
	}
	names := make([]string, 0, len(s.tasks))
	for _, t := range s.tasks {
		names = append(names, t.Name)
	}
	pr.DoneTasks = names
	return pr, append([]ansibleTask{}, s.tasks...)
}

// startHeartbeat 起心跳 goroutine，返回停止函数
func (s *stepRunState) startHeartbeat(catDB *storepkg.DB) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(stepHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.heartbeat(catDB)
			}
		}
	}()
	return func() { close(done) }
}

// endAttempt 记录本次尝试的结局（终态时调用），返回完整历史
func (s *stepRunState) endAttempt(result, note string) []attemptRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.history = append(s.history, attemptRec{
		No: s.attempt, StartedAt: s.started.Format("2006-01-02 15:04:05"),
		EndedAt: now.Format("2006-01-02 15:04:05"), Result: result,
		WaitedSec: int64(now.Sub(s.started).Seconds()), Note: note,
	})
	return append([]attemptRec{}, s.history...)
}

// tasksSnapshot 当前已收集的任务（含逐任务原文），终态事件用它把全过程带回去
func (s *stepRunState) tasksSnapshot() []ansibleTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ansibleTask{}, s.tasks...)
}

// history 当前历史（不追加）
func (s *stepRunState) historySnapshot() []attemptRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attemptRec{}, s.history...)
}

// compactTasks 精简任务列表（去掉逐任务原文行，用于进度事件）
func compactTasks(tasks []ansibleTask) []ansibleTask {
	out := make([]ansibleTask, 0, len(tasks))
	for _, t := range tasks {
		c := t
		c.Lines = nil
		if len(c.Msg) > 300 {
			c.Msg = c.Msg[:300] + " …"
		}
		if len(c.Stdout) > 300 {
			c.Stdout = c.Stdout[:300] + " …"
		}
		out = append(out, c)
	}
	return out
}

// taskStatusLabel 任务状态人话化（界面/摘要共用）
func taskStatusLabel(s string) string {
	switch s {
	case "ok":
		return "成功"
	case "changed":
		return "已变更"
	case "failed":
		return "失败"
	case "unreachable":
		return "不可达"
	case "skipping":
		return "跳过"
	}
	return "未知"
}

// ===================================================================
//  超时判定
// ===================================================================

// timeoutVerdict 超时判定结果
type timeoutVerdict struct {
	Kind     string // idle / run
	Waited   time.Duration
	LimitSec int
}

// specForEvent 按执行路径选规格（2026-09-21 五阶段评审细化）：
//   - 平台 ansible 代执行：用 步骤ID/atom 规格（自动化执行，节奏是登记值）
//   - Agent 域步骤（self_metrics 等）：同样是自动化进程的心跳/回报，按 步骤ID/atom 规格——
//     self_metrics 必须按 180s 无进展失败，不能落 default 3600 傻等
//   - 人工执行路径（external 无凭据兜底）：回落 default——人的回报节奏不可预估，
//     不因安装步的紧阈值误杀（install_agent 人工路径保留宽松判定）
func specForEvent(step storepkg.FlowStepSnapshot, detail string) StepTimeout {
	if platformManaged(detail) || step.Scope == "agent" {
		return timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	}
	return timeoutSpecFor(onboardCfg, "")
}

// timeoutVerdictFor 判定某 running 步骤是否已超时（纯函数，便于单测）。
// 无进度元数据的老事件用事件时间戳兜底——升级前遗留的 running 步骤同样要收敛
func timeoutVerdictFor(step storepkg.FlowStepSnapshot, ev *storepkg.FlowEvent, now time.Time) *timeoutVerdict {
	if ev == nil || ev.Status != stRunning {
		return nil
	}
	spec := specForEvent(step, ev.Detail)
	pr := progressOf(ev.Detail)

	started := parseFlowTS(ev.CreatedAt)
	last := parseFlowTS(ev.CreatedAt)
	if pr != nil {
		if t := parseFlowTS(pr.StartedAt); !t.IsZero() {
			started = t
		}
		if t := parseFlowTS(pr.LastActivityAt); !t.IsZero() {
			last = t
		}
	}
	if started.IsZero() {
		return nil // 时间戳不可读：不猜，交给人工
	}

	if waited := now.Sub(last); waited > time.Duration(spec.IdleSec)*time.Second {
		return &timeoutVerdict{Kind: "idle", Waited: waited, LimitSec: spec.IdleSec}
	}
	if waited := now.Sub(started); waited > time.Duration(spec.RunSec)*time.Second {
		return &timeoutVerdict{Kind: "run", Waited: waited, LimitSec: spec.RunSec}
	}
	return nil
}

func parseFlowTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// ===================================================================
//  后台清扫：判定 → 自动重试（有限次） → 判死并落 fatal
// ===================================================================

var sweepMu sync.Mutex

// sweepOnboardTimeouts 扫描所有运行中的流水线，对 running 步骤做超时处置。
// 由定时器驱动（20s）：不需要有人开着页面，"不能无底线等待"才成立。
// 顺序有讲究：先对账 L1 回执（把已回执的步骤转终态），再续在飞任务的活跃，
// 最后判超时——顺序反了会把刚回执的步骤误判为超时，或把正在跑的步骤误判为无响应。
func sweepOnboardTimeouts(catDB *storepkg.DB, agentStore *AgentStore) {
	sweepMu.Lock()
	defer sweepMu.Unlock()

	reconcileL1TaskAcks(catDB, agentStore)
	refreshL1TaskLiveness(catDB)

	flows, err := catDB.ListRunningFlows()
	if err != nil {
		log.Printf("timeout sweep: list flows: %v", err)
		return
	}
	now := time.Now()
	for _, f := range flows {
		for _, s := range f.Steps {
			ev, _ := catDB.LatestEvent(f.ID, s.ID)
			v := timeoutVerdictFor(s, ev, now)
			if v == nil {
				continue
			}
			verdict := *v
			// 单步兜底：清扫跑在后台 goroutine（全仓无 recover），
			// 任何一步的意外 panic 都会带崩整个控制台，链路整体停摆
			runGuarded(fmt.Sprintf("flow-%d %s", f.ID, s.ID), func() {
				handleStepTimeout(catDB, agentStore, f, s, ev, verdict)
			})
		}
	}
}

// runGuarded 执行单步处置并兜住 panic：单步异常只记录并跳过，下一轮巡检仍会重试；
// 正常执行不受影响
func runGuarded(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[sweeper] %s panic recovered: %v", what, r)
		}
	}()
	fn()
}

// handleStepTimeout 超时处置：还有重试额度就自动重试，否则判死（fatal 诊断 + 下游 blocked）
func handleStepTimeout(catDB *storepkg.DB, agentStore *AgentStore, f *storepkg.Flow,
	step storepkg.FlowStepSnapshot, ev *storepkg.FlowEvent, v timeoutVerdict) {

	cred := sshCredOf(mustResource(catDB, f.ResourceID))
	spec := specForEvent(step, ev.Detail)
	pr := progressOf(ev.Detail)
	attempt, history := 1, []attemptRec{}
	if pr != nil {
		attempt = pr.Attempt
		history = attemptsOf(ev.Detail)
	}
	if attempt < 1 {
		attempt = 1
	}
	startedAt := ev.CreatedAt
	if pr != nil && pr.StartedAt != "" {
		startedAt = pr.StartedAt
	}
	now := time.Now()
	rec := attemptRec{
		No: attempt, StartedAt: startedAt, EndedAt: now.Format("2006-01-02 15:04:05"),
		Result: "timeout_" + v.Kind, WaitedSec: int64(v.Waited.Seconds()),
		Note: fmt.Sprintf("%s 上限 %ds 内未完成", timeoutKindLabel(v.Kind), v.LimitSec),
	}
	history = append(history, rec)

	maxAttempts := spec.Retry + 1
	if attempt < maxAttempts {
		// 还有额度：自动重试一次。给运维的观感是"平台自己救了一次，仍然失败才找人"
		next := attempt + 1
		ds := []diag{warnDiag("step_timeout_retry",
			fmt.Sprintf("第 %d/%d 次尝试%s（%s 上限 %ds，已等待 %s），平台自动重试",
				attempt, maxAttempts, timeoutKindLabel(v.Kind), timeoutKindLabel(v.Kind), v.LimitSec, humanDur(v.Waited)),
			"无需人工介入。若连续超时，请核对目标机连通性、SSH 端口与负载；在资源台账更正后重试该步")}
		detail := map[string]any{
			"executor": "ansible", "runner": "platform",
			"conn": credState(cred), "target": cred.target(),
			"progress": stepProgress{
				Attempt: next, MaxAttempts: maxAttempts,
				StartedAt:      now.Format("2006-01-02 15:04:05"),
				LastActivityAt: now.Format("2006-01-02 15:04:05"),
				Timeout:        spec,
			},
			"attempts":  history,
			"diagnosis": diagJSON(ds),
			"note":      fmt.Sprintf("上一次尝试（第 %d 次）已超时，本次为平台自动重试", attempt),
		}
		_ = appendStepEvent(catDB, f.ID, step, stRunning,
			fmt.Sprintf("第 %d 次尝试%s，平台自动重试中（第 %d/%d 次）",
				attempt, timeoutKindLabel(v.Kind), next, maxAttempts), detailJSON(detail), 0)
		addAudit("步骤超时自动重试", step.ID, "接入中心",
			fmt.Sprintf("flow-%d 第 %d 次尝试%s，自动重试第 %d 次", f.ID, attempt, timeoutKindLabel(v.Kind), next))
		restartStep(catDB, agentStore, f, step)
		return
	}

	// 额度用尽：判死。诊断必须能直接指路，否则运维只知道"超时了"
	hint := "① 在目标机确认 sshd 端口/账号与资源台账一致且可达；" +
		"② 确认 ansible-playbook 能在目标机正常执行（磁盘空间、sudo 权限、负载）；" +
		"③ 也可以在资源台账修正后点「↻ 重试该步」，或走「强制继续（需留痕）」推进"
	if v.Kind == "idle" {
		hint = "目标机/执行器已 " + humanDur(v.Waited) + " 无任何响应：① 先确认主机是否在线、SSH 端口是否仍可达；" +
			"② 确认平台容器到目标机网络是否中断；③ 处理后点「↻ 重试该步」重新执行"
	}
	ds := []diag{fatalDiag("step_timeout",
		fmt.Sprintf("步骤超时：%s（%s 上限 %ds），累计等待 %s，已自动重试 %d 次仍未完成",
			timeoutKindLabel(v.Kind), timeoutKindLabel(v.Kind), v.LimitSec, humanDur(v.Waited), attempt-1),
		hint)}
	detail := map[string]any{
		"executor": "ansible", "runner": "platform",
		"conn": credState(cred), "target": cred.target(),
		"progress": stepProgress{
			Attempt: attempt, MaxAttempts: maxAttempts,
			StartedAt: startedAt, LastActivityAt: now.Format("2006-01-02 15:04:05"),
			Timeout: spec,
		},
		"attempts":  history,
		"diagnosis": diagJSON(ds),
	}
	// 沿用最近一次的任务快照：运维最需要看到的是"卡在哪一步"
	if pr != nil && len(pr.DoneTasks) > 0 {
		detail["last_progress"] = pr
	}
	if tasks := progressTasksOf(ev.Detail); len(tasks) > 0 {
		detail["tasks"] = tasks
	}
	summary := fmt.Sprintf("步骤超时（%s 上限 %ds），已自动重试 %d 次仍未完成，需人工介入",
		timeoutKindLabel(v.Kind), v.LimitSec, attempt-1)
	addAudit("步骤超时判死", step.ID, "接入中心",
		fmt.Sprintf("flow-%d %s，累计等待 %s", f.ID, summary, humanDur(v.Waited)))
	failStep(catDB, agentStore, f.ID, step, "步骤超时（未完成）", summary, detail, cred, ds)
}

// progressTasksOf 取事件 detail 里的任务快照
func progressTasksOf(detail string) []ansibleTask {
	if detail == "" {
		return nil
	}
	var d struct {
		Tasks []ansibleTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return nil
	}
	return d.Tasks
}

// restartStep 超时后重新拉起：清掉作业占用标记并强制重拉
func restartStep(catDB *storepkg.DB, agentStore *AgentStore, f *storepkg.Flow, step storepkg.FlowStepSnapshot) {
	markForcedRestart(f.ID, step.ID)
	_ = advanceFlow(catDB, agentStore, f.ID)
}

// forcedRestart 一次性强制重拉标记：超时重试必须绕过
// 「作业已存在 / running 未过期」两道保护，否则重试指令发不出去
var forcedRestart sync.Map

func forcedRestartKey(flowID int64, stepID string) string {
	return fmt.Sprintf("%d:%s", flowID, stepID)
}

func markForcedRestart(flowID int64, stepID string) {
	forcedRestart.Store(forcedRestartKey(flowID, stepID), true)
}

// takeForcedRestart 消费一次性标记
func takeForcedRestart(flowID int64, stepID string) bool {
	key := forcedRestartKey(flowID, stepID)
	if _, ok := forcedRestart.LoadAndDelete(key); ok {
		return true
	}
	return false
}

// jobStillCurrent 旧作业是否仍是该步骤的"当前尝试"。
// 超时重试会拉起新尝试；旧作业若在其后返回，回报/失败一律丢弃——
// 否则新尝试的进展会被旧结果覆盖（含"回报被拒 → 标 fail"这类误伤）
func jobStillCurrent(catDB *storepkg.DB, flowID int64, stepID string, myAttempt int) bool {
	ev, _ := catDB.LatestEvent(flowID, stepID)
	if ev == nil {
		return true
	}
	if ev.Status != stRunning {
		return false
	}
	pr := progressOf(ev.Detail)
	if pr == nil {
		return true
	}
	return pr.Attempt == myAttempt
}

// mustResource 读资源对象（失败返回 nil，调用方各自兜底）
func mustResource(catDB *storepkg.DB, id string) *storepkg.Resource {
	r, err := catDB.GetResource(id)
	if err != nil {
		return nil
	}
	return r
}

// timeoutKindLabel 超时类型人话化
func timeoutKindLabel(kind string) string {
	if kind == "idle" {
		return "无响应"
	}
	return "超时"
}

// humanDur 时长人话化（诊断文案用）
func humanDur(d time.Duration) string {
	sec := int64(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%dm", sec/3600, (sec%3600)/60)
}

// startTimeoutSweeper 启动后台清扫（main 调用一次）
func startTimeoutSweeper(catDB *storepkg.DB, agentStore *AgentStore) {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for range t.C {
			sweepOnboardTimeouts(catDB, agentStore)
		}
	}()
	fmt.Printf("Onboard timeout sweeper started (interval %s)\n", sweepInterval)
}

// diagsOf 解析事件 detail 里的分级诊断数组（搬运上一轮诊断用）
func diagsOf(detail string) []diag {
	if detail == "" {
		return nil
	}
	var d struct {
		Diagnosis []diag `json:"diagnosis"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return nil
	}
	return d.Diagnosis
}

// rawFieldOf 取事件 detail 里某个顶层字段的原始值（progress / attempts 原样搬运）
func rawFieldOf(detail, key string) any {
	if detail == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		return nil
	}
	return m[key]
}

// buildTimeoutView 构造某步骤的超时视图（界面直接渲染，避免前端自己算时间）。
// 只对 running 步骤有意义：blocked（等人工决策）不参与超时判定，也不该显示倒计时吓人
func buildTimeoutView(step storepkg.FlowStepSnapshot, ev *storepkg.FlowEvent) map[string]any {
	if ev == nil || ev.Status != stRunning {
		return nil
	}
	spec := specForEvent(step, ev.Detail)
	pr := progressOf(ev.Detail)
	now := time.Now()
	started, last := parseFlowTS(ev.CreatedAt), parseFlowTS(ev.CreatedAt)
	attempt, maxAttempts := 1, spec.Retry+1
	if pr != nil {
		if t := parseFlowTS(pr.StartedAt); !t.IsZero() {
			started = t
		}
		if t := parseFlowTS(pr.LastActivityAt); !t.IsZero() {
			last = t
		}
		if pr.Attempt > 0 {
			attempt = pr.Attempt
		}
		if pr.MaxAttempts > 0 {
			maxAttempts = pr.MaxAttempts
		}
	}
	if started.IsZero() {
		return nil
	}
	elapsed := int64(now.Sub(started).Seconds())
	idle := int64(now.Sub(last).Seconds())
	// 两个上限取"先到者"：谁先到谁决定剩余时间
	remainRun, remainIdle := int64(spec.RunSec)-elapsed, int64(spec.IdleSec)-idle
	remain, by := remainRun, "run"
	if remainIdle < remainRun {
		remain, by = remainIdle, "idle"
	}
	out := map[string]any{
		"run_limit_sec": spec.RunSec, "idle_limit_sec": spec.IdleSec,
		"attempt": attempt, "max_attempts": maxAttempts, "auto_retry": spec.Retry > 0,
		"elapsed_sec": elapsed, "idle_sec": idle,
		"remain_sec": remain, "remain_by": by,
		"managed": platformManaged(ev.Detail),
	}
	if remain <= 0 {
		out["overdue"] = true
	}
	return out
}
