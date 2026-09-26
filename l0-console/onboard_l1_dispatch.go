package main

// onboard_l1_dispatch.go —— 步骤投递 L1 Ansible Runner（SPEC-D5-IN5 B3/B4）
//
// 执行动作由「进程内起 goroutine 跑 ansible」改为「投递 L1 任务 + 等回执」，
// 步骤终态由 B2 的回执对账桥驱动。B3 只搬 install；B4 把卸载侧四步
// （scan_collectors / uninstall_plugins / cleanup_autostart / uninstall）纳入同一支持集，
// 由总开关 L1_EXEC_EXECUTOR 统一控制、按 kind 路由（载荷与收口各走各的，投递链路共用）。
//
// 三条边界（SPEC §2.3）：
//   - L0 只做投递与判死：构造自足载荷（D3）、定任务 ID（D6）、下发预算（D5）、
//     在载荷无法构造时按同名诊断当场失败；不替执行端渲染、不替它重试
//   - 载荷自足（D3/D4）：渲染所需的 tag/sha、控制通道接入点、池归属、SSH 凭据全部随载荷，
//     执行端读不到 L0 的流水线/资源台账；凭据与 resources 表同密级，回执后由 store 层置空
//   - 灰度可回滚（D8）：L1_INSTALL_EXECUTOR 默认 inprocess，切回即走原进程内路径
//
// 为什么 install 的活跃信号要单独续命：进程内路径有 15s 心跳写 progress，清扫器据此判"无响应"。
// 执行端搬到 L1 后 L0 侧没有心跳源，长安装会被 idle 阈值误判为卡死并触发重试——
// 重试换 attempt 就是**第二次真安装**，会在目标机上并发跑两份。故清扫器判定前，
// 先按「任务仍被 L1 持有且未回执」续活跃（硬上限 RunSec 不变，执行端真死照样判死）。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// 灰度开关取值（D8）
const (
	execInProcess = "inprocess"
	execRunner    = "runner"
)

// execChannelL1 执行通道标记：写进 running 事件 detail。
// 超时判定认的是 executor/runner 两个字段（平台代执行 vs 人工），
// 通道只用于界面与对账分辨"这一跳在哪执行"，不影响超时口径。
const execChannelL1 = "l1"

// cfgInstallRunnerID 接收安装任务的 L1 执行端身份。
// 投递按 controller_id 定向、Runner 按同一身份拉取——两端默认值必须一致，
// 否则任务会永远停在 pending（灰度期最容易踩的坑，故共用同名环境变量与同一默认值）。
var cfgInstallRunnerID = envOr("L1_ANSIBLE_RUNNER_ID", "l1-ansible-runner-1")

// l1ExecExecutor 执行端总开关（D8）：L1_EXEC_EXECUTOR=inprocess|runner。
//
// B4 起由 install 单点扩为「支持集」总开关：开关只说"敢不敢把支持集里的 atom 交给 L1"，
// 具体哪些 atom 能走由 l1ExecSupportedKind 决定——两件事分开，新增 atom 不必再动开关语义。
// L1_INSTALL_EXECUTOR 是 B3 期的旧名，仅在总开关未设时兜底，避免两处开关同时生效互相打架。
// 非法值/未设置一律回落 inprocess：开关的语义是"敢不敢切"，不是"猜你想切"。
func l1ExecExecutor() string {
	v := strings.TrimSpace(os.Getenv("L1_EXEC_EXECUTOR"))
	if v == "" {
		v = strings.TrimSpace(os.Getenv("L1_INSTALL_EXECUTOR"))
	}
	if strings.EqualFold(v, execRunner) {
		return execRunner
	}
	return execInProcess
}

// l1ExecSupportedKind 支持集：已迁移到 L1 Runner 的执行类型（B3 install；B4 卸载侧四步）。
// 新增一个 atom = 支持集加一行。支持集是白名单——不在内的一律回落进程内路径，不做"尽力而为"。
func l1ExecSupportedKind(kind string) bool {
	switch kind {
	case "install", "uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart":
		return true
	}
	return false
}

// l1TaskID 任务 ID（D6）：flow-<id>-<stepID>-a<attempt>。
// 带 attempt 是重试安全的关键：重试换 attempt → 新 ID → 新任务；
// 同 ID 重复投递由 store 层幂等 SKIP，不会重复执行。
func l1TaskID(flowID int64, stepID string, attempt int) string {
	return fmt.Sprintf("flow-%d-%s-a%d", flowID, stepID, attempt)
}

// currentAttemptOf 当前尝试序号：与 stepRunState 同源（读最近事件的 progress.attempt，缺省 1）。
// 超时重试会先写一条 attempt+1 的 running 事件再重拉，故这里读到的就是"本次是第几次"。
func currentAttemptOf(catDB *storepkg.DB, flowID int64, stepID string) int {
	if ev, _ := catDB.LatestEvent(flowID, stepID); ev != nil {
		if pr := progressOf(ev.Detail); pr != nil && pr.Attempt > 0 {
			return pr.Attempt
		}
	}
	return 1
}

// l1StepAlreadyRunning 重入闸：本步本尝试是否已由 L1 投递过（最近事件 = L1 running）。
//
// 为什么必须有这道闸：advanceFlow 只跳过终态（ok/skipped/fail），running 步骤每次推进都会
// 重新进入 external 分支。进程内路径靠 ansibleJobs 占用标记挡住重复拉起，L1 路径没有这个
// 内存标记——不挡的话每次轮询都会重投一次并追加一条 running 事件（事件洪水 + 时间线抖动）。
//
// 判据取"最近事件"而不是"任务在飞"：任务回执 done 到对账桥消费之间存在一个窗口，
// 此窗口内任务已不在飞但步骤仍是 running——只看任务会误判为"没投过"从而二次投递（= 二次真执行）。
// 最近事件仍是本尝试的 L1 running，就说明"这一跳已经发出去了"，无论是待拉取、执行中还是刚回执。
func l1StepAlreadyRunning(catDB *storepkg.DB, flowID int64, stepID string, attempt int) bool {
	ev, _ := catDB.LatestEvent(flowID, stepID)
	if ev == nil || ev.Status != stRunning {
		return false
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
		return false
	}
	if s, _ := d["exec_channel"].(string); s != execChannelL1 {
		return false
	}
	if pr := progressOf(ev.Detail); pr != nil && pr.Attempt != attempt {
		return false
	}
	return true
}

// freeL1Attempt 取一个「未被已消费任务占用」的尝试号。
//
// D6 的任务 ID 编码 attempt，而 attempt 只在超时自动重试时递增；人工「重试该步」只把步骤
// 重置回 pending（onboard_api.go retry 分支），attempt 不变。此时沿用同 attempt 会撞上
// 已被消费的旧任务，QueueL1Task 幂等 SKIP → 步骤卡死。故这里向后取第一个空位：
// 已被"不会再回执"的任务占用的号跳过，没被占用的号直接用。
//
// 在飞任务不算占用（它还会回执）：那种情形由重入闸拦下，不会走到这里。
func freeL1Attempt(catDB *storepkg.DB, flowID int64, stepID string, attempt int) int {
	if attempt < 1 {
		attempt = 1
	}
	for i := 0; i < 64; i++ {
		taken, err := catDB.L1TaskAttemptTaken(flowID, stepID, attempt)
		if err != nil || !taken {
			return attempt
		}
		attempt++
	}
	return attempt
}

// dispatchErr 载荷构造失败：摘要 + 分级诊断。
// 诊断码/文案与进程内同名作业的判定保持一致——
// 两条执行路径的失败结论必须一模一样，否则"切了开关结论就变了"。
type dispatchErr struct {
	summary string
	reason  string
	diag    diag
}

// buildInstallPayload 构造自足安装载荷（D3 薄执行端）。
// 只做「取数与判定」，不碰目标机：这里的失败都是前置条件不成立（无人选版/版本不在清单/
// 流水线或资源不存在），与进程内执行的前置判定同码同文案。
func buildInstallPayload(catDB *storepkg.DB, agentStore *AgentStore, r *flowRun,
	step storepkg.FlowStepSnapshot, cred *sshCred, attempt int) (map[string]any, *dispatchErr) {

	// ① 版本取自人工选定事件（pick_version OK 事件 detail.tag）——安装只装人选的版本
	tag := pickedVersionTag(catDB, r.flow.ID)
	if tag == "" {
		return nil, &dispatchErr{
			summary: "未找到人工选定版本（pick_version 无确认记录），拒绝安装",
			reason:  "no_human_pick",
			diag: fatalDiag("no_human_pick", "未找到人工选定版本，拒绝安装",
				"先在「选择 SAgent 版本」环节由人工确认版本；平台不使用 latest 兜底"),
		}
	}
	ver := versionByTag(tag)
	if ver == nil {
		return nil, &dispatchErr{
			summary: "版本清单中无 " + tag + "，拒绝安装",
			reason:  "version_not_in_catalog",
			diag: fatalDiag("version_not_in_catalog", "版本清单中无 "+tag+"，拒绝安装",
				"检查 data/versions.yaml 登记项，或重新人工选版"),
		}
	}
	if r.flow == nil {
		return nil, &dispatchErr{
			summary: "流水线不存在", reason: "flow_not_found",
			diag: fatalDiag("flow_not_found", "流水线不存在", ""),
		}
	}
	if r.resource == nil {
		return nil, &dispatchErr{
			summary: "资源对象不存在", reason: "resource_not_found",
			diag: fatalDiag("resource_not_found", "资源对象不存在", "检查资源台账"),
		}
	}

	// ② 渲染入参（D3：渲染所需数据随载荷，执行端不读 L0 台账）
	agentID := r.flow.AgentID
	if agentID == "" {
		agentID = r.flow.ResourceID
	}
	// 控制通道接入点：资源台账的下发值优先（按资源网络分区给可达接入点），无则用平台默认
	consoleURL := r.resource.AgentConsoleURL
	if strings.TrimSpace(consoleURL) == "" {
		consoleURL = l0AgentURL()
	}
	// 采集机池归属：复用/重装既有采集机时沿用其已登记池标签（池是漂移边界，重装不得丢失）
	colPool := ""
	if isCollectorMode(r.flow.Mode) {
		if ca := agentStore.Get(agentID); ca != nil {
			colPool = ca.Labels["pool"]
		}
	}

	// ③ 执行预算（D5）：与进程内 playbook 阶段同源（execBudget 带 execGrace，
	//    保证清扫器先到、由它决定重试；执行端 ctx 只是最后一道保险）
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	budget := budgetSec(execBudget(spec, budgetPlaybook))

	return map[string]any{
		"tag": tag, "sha256": ver.SHA256, "version": ver.Version,
		"agent_id": agentID, "console_url": consoleURL,
		"collector": isCollectorMode(r.flow.Mode), "pool": colPool,
		"ssh_host": cred.Host, "ssh_port": cred.Port,
		"ssh_user": cred.User, "ssh_pass": cred.Pass,
		"dest_home": destHomeFor(cred),
		"budget_sec": budget,
		// 绑定信息随载荷冗余一份：执行端日志/回执能自证"这条任务属于哪一步的哪次尝试"
		"flow_id": r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}, nil
}

// runnerStepMeta 卸载侧投递的执行元数据：执行端按 Playbook 渲染加载，L0 侧按 Title 归因。
type runnerStepMeta struct {
	Kind     string // 执行类型（= L1 任务 kind，执行端按它分派）
	Playbook string // data/playbooks/<name>
	Title    string // 中文名（失败摘要与审计）
}

// runnerStepMetaFor 卸载侧 kind → 执行元数据（nil = 该 kind 不属于卸载侧）。
// playbook 名从 offboardSpecForKind 同源取，避免"投递的 playbook"与"裁定的 spec"指向不同文件。
func runnerStepMetaFor(kind string) *runnerStepMeta {
	if sp := offboardSpecForKind(kind); sp != nil {
		return &runnerStepMeta{Kind: kind, Playbook: sp.Playbook, Title: sp.Title}
	}
	if kind == "uninstall" {
		return &runnerStepMeta{Kind: kind, Playbook: "uninstall.yml", Title: "卸载 SAgent"}
	}
	return nil
}

// buildOffboardPayload 构造自足卸载侧载荷（D3 薄执行端）。
// 卸载侧不依赖版本清单，前置判定只有"流水线/资源存在"——与进程内作业的判定同码同文案。
func buildOffboardPayload(r *flowRun, step storepkg.FlowStepSnapshot, cred *sshCred,
	attempt int, meta *runnerStepMeta) (map[string]any, *dispatchErr) {

	if r.flow == nil {
		return nil, &dispatchErr{
			summary: "流水线不存在", reason: "flow_not_found",
			diag: fatalDiag("flow_not_found", "流水线不存在", ""),
		}
	}
	if r.resource == nil {
		return nil, &dispatchErr{
			summary: "资源对象不存在", reason: "resource_not_found",
			diag: fatalDiag("resource_not_found", "资源对象不存在", "检查资源台账"),
		}
	}
	// 执行预算（D5）：与进程内 playbook 阶段同源（execBudget 带 execGrace，
	// 保证清扫器先到、由它决定重试；执行端 ctx 只是最后一道保险）
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	return map[string]any{
		"kind": meta.Kind, "playbook": meta.Playbook, "title": meta.Title,
		"ssh_host": cred.Host, "ssh_port": cred.Port,
		"ssh_user": cred.User, "ssh_pass": cred.Pass,
		"dest_home":  destHomeFor(cred),
		"budget_sec": budgetSec(execBudget(spec, budgetPlaybook)),
		// 绑定信息随载荷冗余一份：执行端日志/回执能自证"这条任务属于哪一步的哪次尝试"
		"flow_id": r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}, nil
}

// buildStepPayload 按 kind 分派构造自足载荷：install 走版本清单路径，卸载侧走 playbook 路径。
func buildStepPayload(catDB *storepkg.DB, agentStore *AgentStore, r *flowRun,
	step storepkg.FlowStepSnapshot, cred *sshCred, attempt int, kind string) (map[string]any, *dispatchErr) {
	if kind == "install" {
		return buildInstallPayload(catDB, agentStore, r, step, cred, attempt)
	}
	meta := runnerStepMetaFor(kind)
	if meta == nil {
		return nil, &dispatchErr{
			summary: "执行类型 " + kind + " 无对应步骤定义，拒绝投递", reason: "unknown_kind",
			diag: fatalDiag("l1_unknown_kind", "执行类型 "+kind+" 无对应步骤定义，拒绝投递",
				"该 atom 尚未纳入 L1 支持集；核对 SPEC-D5-IN5 的支持集清单"),
		}
	}
	return buildOffboardPayload(r, step, cred, attempt, meta)
}

// l1ExecActionName 执行类型的短名（失败摘要与审计动作名用）。
// install 固定"安装"两字：B3 已发布的文案不因支持集扩容而漂移。
func l1ExecActionName(kind string) string {
	if kind == "install" {
		return "安装"
	}
	if meta := runnerStepMetaFor(kind); meta != nil {
		return meta.Title
	}
	return kind
}

// payloadDesc 投递审计里的一句话摘要（install 记 tag，卸载侧记 playbook）。
func payloadDesc(payload map[string]any) string {
	if v, ok := payload["tag"]; ok {
		return fmt.Sprintf("tag=%v", v)
	}
	if v, ok := payload["playbook"]; ok {
		return fmt.Sprintf("playbook=%v", v)
	}
	return ""
}

// deliverStepToRunner 投递一个支持集内的步骤给 L1 Ansible Runner 并落 running。
// 返回 true 表示已接管（无论投递成功还是当场判失败）——调用方不再回落进程内/人工路径。
//
// forced = 超时自动重试的一次性强拉标记（与进程内路径共用）：重试已经写好了新 attempt 的
// running 事件，必须绕过重入闸把新尝试真发出去，否则"自动重试"只是改了事件没换执行。
func deliverStepToRunner(catDB *storepkg.DB, agentStore *AgentStore, r *flowRun,
	step storepkg.FlowStepSnapshot, cred *sshCred, forced bool, kind string) bool {

	action := l1ExecActionName(kind)
	attempt := currentAttemptOf(catDB, r.flow.ID, step.ID)

	// 重入闸：本步本尝试已经投出去过 → 保持 running，不重投也不重写事件
	if !forced && l1StepAlreadyRunning(catDB, r.flow.ID, step.ID, attempt) {
		return true
	}

	// 取号：人工重试不递增 attempt，需跳过已被消费任务占用的号（否则 ID 相撞 → 卡死）
	attempt = freeL1Attempt(catDB, r.flow.ID, step.ID, attempt)

	payload, derr := buildStepPayload(catDB, agentStore, r, step, cred, attempt, kind)
	if derr != nil {
		failStep(catDB, agentStore, r.flow.ID, step, action+"未启动", derr.summary, map[string]any{
			"executor": "ansible", "runner": "platform", "exec_channel": execChannelL1,
			"conn": credState(cred), "target": cred.target(), "reason": derr.reason,
		}, cred, withPortSource(cred, []diag{derr.diag}))
		return true
	}

	taskID := l1TaskID(r.flow.ID, step.ID, attempt)
	row, created, err := catDB.QueueL1Task(storepkg.L1TaskRow{
		ID: taskID, ControllerID: cfgInstallRunnerID, Kind: kind,
		PayloadJSON: storepkg.MarshalPayload(payload), Status: "pending",
		FlowID: r.flow.ID, StepID: step.ID, Attempt: attempt,
	})
	if err != nil {
		// 投递失败必须显式失败：静默会让步骤永远停在 pending，直到清扫器按超时误报
		failStep(catDB, agentStore, r.flow.ID, step, action+"任务投递失败",
			"投递 L1 "+action+"任务失败："+err.Error(), map[string]any{
				"executor": "ansible", "runner": "platform", "exec_channel": execChannelL1,
				"conn": credState(cred), "target": cred.target(),
			}, cred, withPortSource(cred, []diag{fatalDiag("l1_task_enqueue_failed",
				"投递 L1 "+action+"任务失败："+err.Error(),
				"确认 catalog 库可写（l1_tasks 表），再点「↻ 重试该步」")}))
		return true
	}
	if !created {
		// 任务已存在：并发投递或"入队成功但事件未写成"的补写场景。
		// 事件已在 → 什么都不用做；事件不在 → 落到下面补写 running（自愈）。
		if l1StepAlreadyRunning(catDB, r.flow.ID, step.ID, attempt) {
			return true
		}
	}

	// 落 running 事件。executor/runner 保持"平台代执行"口径（超时仍用紧规格），
	// 通道另记 exec_channel=l1；progress 必须写全——对账（jobStillCurrent）与
	// 活跃续命都按 progress.attempt 认当前尝试。
	now := time.Now().Format("2006-01-02 15:04:05")
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	detail := map[string]any{
		"executor": "ansible", "runner": "platform", "exec_channel": execChannelL1,
		"l1_task_id": row.ID, "l1_executor_id": cfgInstallRunnerID,
		"conn": credState(cred), "target": cred.target(),
		"note": "平台持有资源 SSH 凭据，经 L1 Ansible Runner 执行（" + step.Title + "）；完成后自动回报，无需人工操作",
		"progress": stepProgress{
			Attempt: attempt, MaxAttempts: spec.Retry + 1,
			StartedAt: now, LastActivityAt: now, Timeout: spec,
		},
	}
	if cred.PortSource == portSourceDefault {
		detail["diagnosis"] = diagJSON([]diag{portSourceDiag(cred)})
	}
	_ = appendStepEvent(catDB, r.flow.ID, step, stRunning,
		"L1 Ansible Runner 执行中（"+cred.target()+"）", detailJSON(detail), 0)
	if created {
		addAudit("投递 L1 "+action+"任务", taskID, "接入中心",
			fmt.Sprintf("flow-%d %s 第 %d 次尝试 → %s（%s）", r.flow.ID, step.ID, attempt, cfgInstallRunnerID, payloadDesc(payload)))
	}
	return true
}

// refreshL1TaskLiveness 续活跃：把「仍被 L1 持有、尚未回执」的任务对应的 running 事件
// 的 progress.last_activity_at 刷到当前时刻（见文件头注释：不续命会把长安装误判为卡死并重试）。
//
// 只刷当前尝试（pr.Attempt == 任务 attempt）：重试后旧任务可能还在飞，
// 拿它给新尝试续命会让新尝试的超时判定永远不生效。
func refreshL1TaskLiveness(catDB *storepkg.DB) {
	rows, err := catDB.ListInflightL1Tasks(0)
	if err != nil {
		log.Printf("l1 liveness: list inflight tasks: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	for _, t := range rows {
		ev, _ := catDB.LatestEvent(t.FlowID, t.StepID)
		if ev == nil || ev.Status != stRunning {
			continue
		}
		pr := progressOf(ev.Detail)
		if pr == nil || pr.Attempt != t.Attempt {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
			continue
		}
		pr.LastActivityAt = now
		d["progress"] = pr
		if err := catDB.UpdateEventDetail(ev.ID, detailJSON(d)); err != nil {
			log.Printf("l1 liveness: touch event %d (flow-%d %s): %v", ev.ID, t.FlowID, t.StepID, err)
		}
	}
}