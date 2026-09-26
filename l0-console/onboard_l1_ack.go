package main

// onboard_l1_ack.go —— L1 任务回执对账桥（SPEC-D5-IN5 B2）
//
// 执行端搬到 L1 后，步骤终态不再由 L0 进程内的作业函数直接落库，而是由
// 「L1 执行端回执 → L0 对账」驱动。本文件是这条链路的唯一翻译层：
//
//	done   → autoReportStep（步骤 ok + 推进流水线）
//	failed → failStep（步骤 fail + 分级诊断）
//
// 三条与进程内执行同口径的约束：
//  ① 只认「当前尝试」：超时重试拉起新尝试后，旧任务的回执一律丢弃（jobStillCurrent），
//     否则新尝试的进展会被旧结果覆盖
//  ② 只驱动 running 步骤：人工重置/已终态的步骤不接受回执（autoReportStep 自身把关）
//  ③ 对账幂等：消费后置 reconciled_at，重复扫描不再驱动（每轮清扫都会扫到同一批终态任务）
//
// 超时判定不在这里——沿用清扫器既有阈值（RunSec/IdleSec + 有限重试），桥只处理回执终态。
// 两套判据并存会互相打架：桥若自带超时，界面倒计时与实际判死就会再次说谎。
//
// 对账桥先于超时扫描执行：刚回执的步骤当轮即转终态，不会被同轮清扫误判为超时。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// reconcileL1TaskAcks 扫描「已绑定步骤、已终态、未消费」的 L1 任务并驱动步骤终态。
// 由清扫器每轮调用一次（20s）：不依赖有人开页面，进程重启后遗留的回执同样会被收敛。
func reconcileL1TaskAcks(catDB *storepkg.DB, agentStore *AgentStore) {
	rows, err := catDB.ListReconcilableL1Tasks(0)
	if err != nil {
		log.Printf("l1 ack reconcile: list tasks: %v", err)
		return
	}
	for _, r := range rows {
		t := r
		// 与清扫器同套兜底：单条任务异常只记录并跳过，下一轮仍会重试
		runGuarded(fmt.Sprintf("l1task %s", t.ID), func() {
			driveStepFromL1Task(catDB, agentStore, t)
		})
	}
}

// driveStepFromL1Task 把一条任务回执翻译成步骤终态。
// 任何"无法对账"的情形（流水线/步骤已不存在、旧尝试回执）都只标记消费、不动状态——
// 标记消费是必须的，否则这些任务会被每轮清扫反复捞出来。
func driveStepFromL1Task(catDB *storepkg.DB, agentStore *AgentStore, t *storepkg.L1TaskRow) {
	f, err := catDB.GetFlow(t.FlowID)
	if err != nil || f == nil {
		_ = catDB.MarkL1TaskReconciled(t.ID)
		return
	}
	step, found := flowStepByID(f, t.StepID)
	if !found {
		_ = catDB.MarkL1TaskReconciled(t.ID)
		return
	}
	if !jobStillCurrent(catDB, t.FlowID, t.StepID, t.Attempt) {
		_ = catDB.MarkL1TaskReconciled(t.ID)
		return
	}

	cred := sshCredOf(mustResource(catDB, f.ResourceID))
	// 结构化结果整体并入证据：facts/tasks/recap 等结论不能只活在 L1 侧
	extra := storepkg.UnmarshalPayload(t.ResultJSON)
	extra["source"] = "l1_runner"
	extra["l1_task_id"] = t.ID
	extra["executor"] = "ansible"
	extra["exec_channel"] = execChannelL1
	extra["conn"] = credState(cred)
	extra["target"] = cred.target()
	extra["attempt"] = t.Attempt

	// 尝试历史承接：超时重试会把历史写在 running 事件里，这里必须原样接过来再追加本次结局。
	// 不承接的话，重试后的卡片只剩最后一条——与进程内"每次尝试都留痕"不对称
	// （用户明确要求过成功/失败展示内容对称）。
	if ev, _ := catDB.LatestEvent(t.FlowID, t.StepID); ev != nil {
		extra["attempts"] = l1AttemptsForward(ev, t.Attempt, t.Status == "done", l1NoteOr(t, ""))
	}

	diags := l1ResultDiagnosis(t)
	// install 的两件 L0 侧收口：证据整形（与进程内同形）+ 采集目标登记（scrape-sd 是 L0 台账）
	if step.Atom == "install_agent" {
		l1InstallExtraShape(t, extra)
		if t.Status == "done" {
			diags = registerInstallScrapeTarget(catDB, f, extra, diags)
		}
		diags = withPortSource(cred, diags)
	}

	// 卸载侧（uninstall / scan_collectors / uninstall_plugins / cleanup_autostart）：
	// 不能走下面的通用分支——卸载的成功判据是**目标机自证核对行**
	// （dir_exists= / PLUGINS_CLEAN / AUTOSTART_DIRTY …），
	// done 必须交给 L0 侧共享裁定（offboardFinish/uninstallFinish），否则"切了开关结论就变了"。
	if t.Kind != "install" && !strings.HasPrefix(t.Kind, "ansible-") && l1ExecSupportedKind(t.Kind) {
		l1OffboardReconcile(catDB, agentStore, t, step, cred, extra)
		_ = catDB.MarkL1TaskReconciled(t.ID)
		return
	}

	switch t.Status {
	case "done":
		if len(diags) > 0 {
			extra["diagnosis"] = diags
		}
		if msg := autoReportStep(catDB, agentStore, t.FlowID, t.StepID,
			l1NoteOr(t, "L1 执行端完成"), extra); msg != "" {
			// 回报被拒（步骤已不在 running：人工重置/已终态）→ 落 fail，与进程内执行同口径
			failStep(catDB, agentStore, t.FlowID, step, "L1 执行回报被拒", msg, extra, cred, l1ResultDiags(t))
		}
	case "failed":
		failStep(catDB, agentStore, t.FlowID, step, "L1 执行失败",
			"L1 执行端失败："+l1NoteOr(t, "未提供失败原因"), extra, cred, l1ResultDiags(t))
	}
	_ = catDB.MarkL1TaskReconciled(t.ID)
}

// l1InstallExtraShape 把 L1 回执的安装结果整成与进程内 ansibleInstallJob 同形的证据。
// 同形是硬要求：界面/证据渲染只认一套字段，两条执行路径给出不同形状 = "切了开关界面就变了"。
// 进程内形状 = 顶层平铺（phases/tasks/recap/recap_line/evidence）+ 嵌套 detail（tag/sha/config/rc）。
func l1InstallExtraShape(t *storepkg.L1TaskRow, extra map[string]any) {
	nested := map[string]any{"attempt": t.Attempt}
	for _, k := range []string{
		"tag", "version", "sha256", "sha_verified", "config", "dest_home",
		"rc", "timed_out", "tasks", "recap", "recap_line", "phases", "evidence", "budget_sec",
	} {
		if v, ok := extra[k]; ok {
			nested[k] = v
		}
	}
	extra["detail"] = nested
	if tasks, ok := extra["tasks"].([]any); ok {
		extra["task_total"] = len(tasks)
	}
}

// registerInstallScrapeTarget install 成功后把新主机登记进 vmagent file_sd。
// 为什么在 L0 侧落笔：vmagent 读的是 L0 挂的宿主目录（configs/scrape-sd），
// 执行端容器写的是自己的卷，写了不生效——"谁持账本谁落笔"。
// 登记失败只加 warn，绝不把"已经装好了"改判为失败（否则运维会去重装一台装好的机器）。
func registerInstallScrapeTarget(catDB *storepkg.DB, f *storepkg.Flow, extra map[string]any, ds []diag) []diag {
	cred := sshCredOf(mustResource(catDB, f.ResourceID))
	if cred == nil || cred.Host == "" {
		return append(ds, warnDiag("scrape_sd_failed",
			"采集目标登记缺少目标机地址（资源台账无 host），未登记 vmagent",
			"核对资源台账的 SSH 主机字段后，点「↻ 重试该步」重新登记"))
	}
	sdPath, err := registerScrapeTarget(f.ResourceID, cred.Host)
	if err != nil {
		return append(ds, warnDiag("scrape_sd_failed",
			"采集目标登记 vmagent file_sd 失败："+err.Error(),
			"检查 data/scrape-sd 目录是否可写；不登记则采集观察环节无法确认入库"))
	}
	extra["scrape_sd"] = sdPath
	// phases 必须平铺在顶层：界面按 dd.phases 取，藏在 detail 里等于没回传
	if phases, ok := extra["phases"].([]any); ok {
		extra["phases"] = append(phases, map[string]any{"phase": "register", "status": "ok", "path": sdPath})
	} else {
		extra["phases"] = []any{map[string]any{"phase": "register", "status": "ok", "path": sdPath}}
	}
	if nested, ok := extra["detail"].(map[string]any); ok {
		nested["phases"] = extra["phases"]
	}
	return ds
}

// l1OffboardReconcile 卸载侧回执的 L0 收口：重建 ansibleRunResult 后走**共享裁定**。
//
// 两条分支的差别只在"这次执行有没有走完 playbook"：
//   - failed：执行端已用同一批 classify 函数给出归因（语法/超时/rc/SSH），直接落 fail，
//     不重跑裁定——重跑会把"语法未过"误报成"输出不可解析"
//   - done  ：rc=0 只说明"playbook 跑完了"，**不等于卸载成功**。
//     成功与否由目标机自证核对行决定，故交给 offboardFinish/uninstallFinish 裁定落笔
//
// 这正是 SPEC §2.3 的分工：执行端回原始事实，L0 持状态机与账本、持唯一判据。
func l1OffboardReconcile(catDB *storepkg.DB, agentStore *AgentStore, t *storepkg.L1TaskRow,
	step storepkg.FlowStepSnapshot, cred *sshCred, extra map[string]any) {

	pr := l1RunResultFromExtra(extra)
	phases := l1PhasesFromExtra(extra)
	destHome, _ := extra["dest_home"].(string)
	if destHome == "" {
		destHome = destHomeFor(cred)
	}
	// 预算只用于超时文案：必须与执行端拿到的是同一个数，故从回执里取而不是重算
	pbBudget := time.Duration(l1BudgetFromExtra(extra)) * time.Second

	// 尝试历史承接：超时重试把历史写在 running 事件里，这里原样接过来再追加本次结局
	endAttempt := func(result, note string) []attemptRec {
		ev, _ := catDB.LatestEvent(t.FlowID, t.StepID)
		return l1AttemptsForwardRes(ev, t.Attempt, result, note)
	}
	// 桥侧没有进程内 stepRunState 的阶段机；阶段已随 phases 回执，故 setPhase 只作占位
	// （不写库，避免凭空补一条无来源的进度事件）
	setPhase := func(string) {}

	// fail 收口：与进程内 fail 闭包同形。attempts 由收口函数写入，仅缺失时兜底——
	// 同一次尝试在历史里只应出现一行（进程内曾因 fail 里再 endAttempt 一次而重复）
	fail := func(summary string, detail map[string]any, ds []diag) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["attempt"] = t.Attempt
		if _, ok := detail["attempts"]; !ok {
			detail["attempts"] = endAttempt("fail", summary)
		}
		if _, ok := detail["tasks"]; !ok {
			detail["tasks"] = compactTasks(pr.Tasks)
		}
		failStep(catDB, agentStore, t.FlowID, step, "L1 执行失败", summary, detail, cred, ds)
	}

	if t.Status != "done" {
		note := l1NoteOr(t, "未提供失败原因")
		detail := l1FailureDetail(extra, pr, destHome)
		failStep(catDB, agentStore, t.FlowID, step, "L1 执行失败",
			"L1 执行端失败："+note, detail, cred, l1ResultDiags(t))
		return
	}

	// seed：桥侧补记的通道元信息。收口函数会先铺 seed 再由计算字段覆盖，
	// 保证"谁执行、结论是什么"这类权威字段不被 seed 盖掉
	seed := map[string]any{
		"executor": "ansible", "exec_channel": execChannelL1,
		"l1_task_id": t.ID, "source": "l1_runner",
	}
	if t.Kind == "uninstall" {
		uninstallFinish(catDB, agentStore, t.FlowID, step, cred, pr, phases, destHome,
			t.Attempt, pbBudget, endAttempt, setPhase, fail, seed)
		return
	}
	sp := offboardSpecForKind(t.Kind)
	if sp == nil {
		// 不该发生：投递端只投支持集内的 kind。真出现即契约漂移，显式失败而非静默消费
		failStep(catDB, agentStore, t.FlowID, step, "L1 执行失败",
			"L1 回执 kind="+t.Kind+" 无对应步骤定义，无法裁定",
			map[string]any{
				"executor": "ansible", "exec_channel": execChannelL1, "l1_task_id": t.ID,
				"conn": credState(cred), "target": cred.target(), "attempt": t.Attempt,
			}, cred, []diag{fatalDiag("l1_unknown_kind",
				"执行类型 "+t.Kind+" 无对应步骤定义，无法裁定",
				"核对 SPEC-D5-IN5 的支持集清单；该 kind 不应被投递到卸载侧")})
		return
	}
	offboardFinish(catDB, agentStore, t.FlowID, step, cred, *sp, pr, phases, destHome,
		t.Attempt, pbBudget, endAttempt, setPhase, fail, seed)
}

// l1FailureDetail 失败回执的证据整形：与进程内失败事件的 detail 同形
// （rc/timed_out/tasks/recap/phases/evidence/dest_home），界面渲染只认这一套。
func l1FailureDetail(extra map[string]any, pr ansibleRunResult, destHome string) map[string]any {
	detail := map[string]any{
		"dest_home": destHome, "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases":     l1PhasesFromExtra(extra), "evidence": execEvidence(pr),
	}
	if conn, ok := extra["conn"]; ok {
		detail["conn"] = conn
	}
	if target, ok := extra["target"]; ok {
		detail["target"] = target
	}
	return detail
}

// l1AttemptsForward 承接尝试历史并追加本次结局（与进程内 endAttempt 同口径）。
func l1AttemptsForward(ev *storepkg.FlowEvent, attempt int, ok bool, note string) []attemptRec {
	res := "fail"
	if ok {
		res = "ok"
	}
	return l1AttemptsForwardRes(ev, attempt, res, note)
}

// l1AttemptsForwardRes 承接尝试历史并追加一条指定结局。
// ev 为 nil（running 事件已被覆盖/缺失）时仍给出本次一条，不留空白历史。
func l1AttemptsForwardRes(ev *storepkg.FlowEvent, attempt int, result, note string) []attemptRec {
	hist := []attemptRec{}
	started := ""
	if ev != nil {
		hist = attemptsOf(ev.Detail)
		started = ev.CreatedAt
		if pr := progressOf(ev.Detail); pr != nil && pr.StartedAt != "" {
			started = pr.StartedAt
		}
	}
	if started == "" {
		started = time.Now().Format("2006-01-02 15:04:05")
	}
	waited := int64(0)
	if t0 := parseFlowTS(started); !t0.IsZero() {
		if d := time.Since(t0); d > 0 {
			waited = int64(d.Seconds())
		}
	}
	hist = append(hist, attemptRec{
		No: attempt, StartedAt: started,
		EndedAt: time.Now().Format("2006-01-02 15:04:05"),
		Result:  result, WaitedSec: waited, Note: note,
	})
	return hist
}

// flowStepByID 在流水线快照里按步骤 id 找步骤。
func flowStepByID(f *storepkg.Flow, stepID string) (storepkg.FlowStepSnapshot, bool) {
	for _, s := range f.Steps {
		if s.ID == stepID {
			return s, true
		}
	}
	return storepkg.FlowStepSnapshot{}, false
}

// l1NoteOr 回执说明（空则给默认文案，避免界面上出现空结论）。
func l1NoteOr(t *storepkg.L1TaskRow, def string) string {
	if t.Note == "" {
		return def
	}
	return t.Note
}

// l1ResultDiagnosis 取回执结果里的分级诊断（原样返回，不合成）——
// 成功路径要在它基础上追加 L0 侧诊断（如采集目标登记 warn），合成兜底会污染成功结论。
func l1ResultDiagnosis(t *storepkg.L1TaskRow) []diag {
	var d struct {
		Diagnosis []diag `json:"diagnosis"`
	}
	if t.ResultJSON != "" {
		_ = json.Unmarshal([]byte(t.ResultJSON), &d)
	}
	return d.Diagnosis
}

// l1ResultDiags 取回执结果里的分级诊断；没有则合成一条——
// failStep 的三件套（executor/conn/diagnosis）必须齐备，不能"看起来有诊断其实为空"。
func l1ResultDiags(t *storepkg.L1TaskRow) []diag {
	if d := l1ResultDiagnosis(t); len(d) > 0 {
		return d
	}
	return []diag{{
		Level:   "fatal",
		Code:    "l1_exec_failed",
		Message: l1NoteOr(t, "L1 执行端未提供失败原因"),
		Hint:    "按 l1_task_id=" + t.ID + " 在 L1 执行端（l1-ansible-runner）日志中定位原始输出",
	}}
}