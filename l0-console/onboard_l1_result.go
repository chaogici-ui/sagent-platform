package main

// onboard_l1_result.go —— L1 执行端「结构化结果」与 L0 侧裁定之间的契约（SPEC-D5-IN5 B4）
//
// 执行端搬到 L1 后，步骤的成败裁定**不能跟着搬**——状态机与账本都在 L0（SPEC §2.3）。
// 所以两边只交换原始事实：L1 回 rc/timed_out/tasks/recap/evidence，L0 重建 ansibleRunResult
// 后复用进程内同一套 Verdict / checkLine / 台账回写。
//
// 为什么用"扁平 extra + 重建"而不是让 L1 判完回结论：
//   ① 裁定要读 L0 的流水线上下文（人工选版事件、预检结论、资源台账），执行端读不到；
//   ② 两条执行路径共用同一份判据代码，才不会"切了开关结论就变了"；
//   ③ task 级 Lines/Stdout/Msg 随 DTO 回传，故 outCheckLine/hasOutMarker 这类
//      从任务输出里取自证标记的裁定，在 L0 侧重建后依然成立。

import (
	"encoding/json"
	"strings"
)

// l1RunResultFromExtra 从 L1 回执的 result 载荷重建 ansibleRunResult。
//
// 缺字段一律取零值：宁可让裁定看到"空输出"（多半会判失败并给出诊断），
// 也不能凭空补一个看起来成功的默认值——那正是"假成功"的来源。
func l1RunResultFromExtra(extra map[string]any) ansibleRunResult {
	var pr ansibleRunResult
	if extra == nil {
		return pr
	}
	pr.RC = intOf(extra["rc"])
	pr.TimedOut, _ = extra["timed_out"].(bool)

	pr.Recap = map[string]int{}
	if m, ok := extra["recap"].(map[string]any); ok {
		for k, v := range m {
			pr.Recap[k] = intOf(v)
		}
	}
	if seq, ok := extra["recap_seq"].([]any); ok {
		for _, s := range seq {
			if str, ok := s.(string); ok {
				pr.RecapSeq = append(pr.RecapSeq, str)
			}
		}
	}
	if ev, ok := extra["evidence"].(map[string]any); ok {
		pr.Output, _ = ev["output"].(string)
	}
	if raw, ok := extra["tasks"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var ts []ansibleTask
			if json.Unmarshal(b, &ts) == nil {
				pr.Tasks = ts
			}
		}
	}
	return pr
}

// execEvidence 一次执行的原始证据（原文 + 失败任务原文）。
// 抽成公共函数：进程内作业与 L1 回执载荷必须给出同一形状的证据，
// 否则"界面看到的原文"会随执行端切换而变。
func execEvidence(pr ansibleRunResult) map[string]any {
	evidence := map[string]any{
		"output": clip(pr.Output, 20000), "output_bytes": len(pr.Output),
		"truncated": len(pr.Output) > 20000,
	}
	if ft := taskFailures(pr.Tasks); len(ft) > 0 {
		lines := []string{}
		for _, x := range ft {
			lines = append(lines, x.Lines...)
		}
		evidence["failed_task_lines"] = clip(strings.Join(lines, "\n"), 6000)
	}
	return evidence
}

// l1VerdictTasks 回执里随带的逐任务结果：**保留 Lines/Stdout/Msg**（各自截断到安全上限）。
//
// 为什么不复用 compactTasks：L0 侧裁定要读任务输出里的自证标记
// （SCAN plugins_total= / dir_exists= / PLUGINS_CLEAN / AUTOSTART_DIRTY …）。
// Lines 被剥掉、Msg/Stdout 被截到 300 字节，判据就丢了——卸载侧会整片误判成
// "输出不可解析/核对未通过"，把一次成功的卸载报成失败。
// 上限取 8000（而非 300）：clip 保头保尾，核对行落在任务输出尾部时也不会被截掉。
// 界面展示用 L0 侧再 compact 一次，故回执侧给"全量判据"不影响卡片体积。
func l1VerdictTasks(tasks []ansibleTask) []ansibleTask {
	out := make([]ansibleTask, 0, len(tasks))
	for _, t := range tasks {
		c := t
		if len(c.Lines) > 0 {
			c.Lines = strings.Split(clip(strings.Join(c.Lines, "\n"), 8000), "\n")
		}
		c.Stdout = clip(c.Stdout, 8000)
		c.Msg = clip(c.Msg, 8000)
		out = append(out, c)
	}
	return out
}

// l1ExecResultExtra 把一次卸载侧执行的结果整成回执载荷（执行端出，L0 侧重建的输入）。
// 顶层平铺 rc/timed_out/tasks/recap/recap_line/phases/evidence，
// 界面与证据渲染只认这一套字段，两条执行路径给不同形状 = "切了开关界面就变了"。
//
// tasks 用 l1VerdictTasks（带判据）而不是 compactTasks：本载荷是给 L0 裁定的输入，
// 不是给界面渲染的成品——成品由 L0 侧 offboardFinish/uninstallFinish 重新 compact。
func l1ExecResultExtra(pr ansibleRunResult, phases []map[string]any, budgetSec int, destHome string, cred *sshCred) map[string]any {
	res := map[string]any{
		"rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": l1VerdictTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq), "recap_seq": pr.RecapSeq,
		"phases": phases, "evidence": execEvidence(pr),
		"dest_home": destHome, "budget_sec": budgetSec,
	}
	if cred != nil {
		res["target"] = cred.target()
	}
	return res
}

// l1PhasesFromExtra 取回执载荷里的阶段记录（L0 收口把它原样带回界面）。
func l1PhasesFromExtra(extra map[string]any) []map[string]any {
	raw, ok := extra["phases"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// l1BudgetFromExtra 取 L0 下发的执行预算（收口只用它写超时文案，必须与执行端拿到的是同一个数）。
func l1BudgetFromExtra(extra map[string]any) int {
	return intOf(extra["budget_sec"])
}