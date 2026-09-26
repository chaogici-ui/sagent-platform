package main

// l1_ansible_runner_test.go —— IN5 B3：L1 Ansible Runner 执行端语义。
//
// 覆盖的是「载荷门禁 + 结构化回执」这条链路的边界：
//   - 非安装类任务拒绝（Runner 只搬 install 一个 atom）
//   - 载荷自足性：缺 tag / 缺预算 / 缺凭据一律当场拒绝，不猜、不降级
//   - 预算（D5）由 L0 单点下发：执行端不自设时限，缺预算即拒
//   - 结构化回执（D2）：失败路径也要带上 diagnosis，不能只回一句 note

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRunnerExecuteRefusesNonInstall 验证 Runner 只认安装类任务，非安装载荷拒绝并回执 failed。
func TestRunnerExecuteRefusesNonInstall(t *testing.T) {
	for _, kind := range []string{"controller-ping", "agent-action", "bogus"} {
		status, note, res := runnerExecuteInstall(L1Task{Kind: kind})
		if status != "failed" {
			t.Fatalf("kind=%q status=%v, want failed", kind, status)
		}
		if note == "" {
			t.Fatalf("kind=%q note empty", kind)
		}
		if res != nil {
			t.Fatalf("kind=%q 拒绝路径不应有结构化回执: %+v", kind, res)
		}
	}
}

// TestParseRunnerReq 验证安装载荷解析：缺 tag 拒绝；完整载荷解析正确。
func TestParseRunnerReq(t *testing.T) {
	if _, err := parseRunnerReq(map[string]any{"ssh_host": "10.0.0.5"}); err == nil {
		t.Fatal("missing tag should error")
	}
	req, err := parseRunnerReq(map[string]any{
		"tag": "0.4.0", "agent_id": "a1", "console_url": "http://gw:8460",
		"ssh_host": "10.0.0.5", "ssh_port": 22, "ssh_user": "deploy", "ssh_pass": "p",
		"budget_sec": 900,
	})
	if err != nil {
		t.Fatalf("parse full req: %v", err)
	}
	if req.Tag != "0.4.0" || req.SSHHost != "10.0.0.5" || req.SSHUser != "deploy" {
		t.Fatalf("parsed req mismatch: %+v", req)
	}
	if req.SSHPort != 22 {
		t.Fatalf("ssh_port = %d, want 22", req.SSHPort)
	}
	if req.BudgetSec != 900 {
		t.Fatalf("budget_sec = %d, want 900", req.BudgetSec)
	}
}

// TestRunnerExecuteMissingBudget 验证缺执行预算即拒（D5）：
// 预算由 L0 单点决定，执行端绝不拿自编时限去杀作业（否则界面倒计时与实际判死互相说谎）。
func TestRunnerExecuteMissingBudget(t *testing.T) {
	status, note, res := runnerExecuteInstall(L1Task{Kind: "install", Payload: map[string]any{
		"tag": "0.4.0", "ssh_host": "10.0.0.5", "ssh_port": 22,
		"ssh_user": "deploy", "ssh_pass": "p",
	}})
	if status != "failed" {
		t.Fatalf("status=%v, want failed (missing budget_sec)", status)
	}
	if note == "" {
		t.Fatal("note empty")
	}
	if res != nil {
		t.Fatalf("门禁拒绝路径不应有结构化回执: %+v", res)
	}
}

// TestRunnerExecuteMissingCred 验证凭据不齐备时拒绝安装（载荷自足但缺 SSH 口令）。
// 预算齐备后，缺口令会被凭据门禁拦下（与平台执行器同一套 cred.valid 判定）。
func TestRunnerExecuteMissingCred(t *testing.T) {
	status, note, _ := runnerExecuteInstall(L1Task{Kind: "install", Payload: map[string]any{
		"tag": "0.4.0", "budget_sec": 900,
		"ssh_host": "10.0.0.5", "ssh_user": "deploy",
	}})
	if status != "failed" {
		t.Fatalf("status=%v, want failed (missing pass)", status)
	}
	if note == "" {
		t.Fatal("note empty")
	}
}

// ===================================================================
//  IN5 B4：卸载侧四步搬 L1 Runner
// ===================================================================

// TestL1ExecSupportedKindCoversOffboard 支持集是白名单：卸载侧四步 + install 在内，
// 未迁移的 atom（升级/启停/探路）必须在外——否则会被投递到"还没实现"的执行端分支上。
func TestL1ExecSupportedKindCoversOffboard(t *testing.T) {
	for _, k := range []string{"install", "uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart"} {
		if !l1ExecSupportedKind(k) {
			t.Errorf("支持集应包含 %q", k)
		}
	}
	for _, k := range []string{"probe", "preflight_upgrade", "upgrade_agent", "preflight_service", "service_execute", "bogus"} {
		if l1ExecSupportedKind(k) {
			t.Errorf("支持集不应包含 %q（尚未迁移，必须回落进程内）", k)
		}
	}
}

// TestRunnerExecuteTaskDispatch 分派：卸载侧四步走 playbook 路径，未知 kind 显式拒绝。
// 未知 kind 不能静默吞掉——吞掉 = 步骤一直卡到超时判死，问题被藏起来。
func TestRunnerExecuteTaskDispatch(t *testing.T) {
	// 空载荷 → 载荷解析失败（证明走了 offboard 分支而不是 install 分支）
	for _, k := range []string{"uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart"} {
		status, note, res := runnerExecuteTask(L1Task{Kind: k})
		if status != "failed" || note == "" {
			t.Fatalf("kind=%q 空载荷应拒绝：status=%v note=%q", k, status, note)
		}
		if res != nil {
			t.Fatalf("kind=%q 载荷门禁拒绝路径不应有结构化回执", k)
		}
	}
	status, note, _ := runnerExecuteTask(L1Task{Kind: "controller-ping"})
	if status != "failed" || !strings.Contains(note, "拒绝执行") {
		t.Fatalf("未知 kind 应显式拒绝：status=%v note=%q", status, note)
	}
}

// TestParseRunnerOffboardReq 载荷自足性门禁：缺 kind / 缺 playbook 一律拒绝。
func TestParseRunnerOffboardReq(t *testing.T) {
	if _, err := parseRunnerOffboardReq(map[string]any{"playbook": "scan_collectors.yml"}); err == nil {
		t.Fatal("缺 kind 应报错")
	}
	if _, err := parseRunnerOffboardReq(map[string]any{"kind": "scan_collectors"}); err == nil {
		t.Fatal("缺 playbook 应报错")
	}
	req, err := parseRunnerOffboardReq(map[string]any{
		"kind": "scan_collectors", "playbook": "scan_collectors.yml", "title": "扫描采集物",
		"ssh_host": "10.0.0.5", "ssh_port": 22, "ssh_user": "deploy", "ssh_pass": "p",
		"dest_home": "/home/deploy", "budget_sec": 900, "attempt": 2,
	})
	if err != nil {
		t.Fatalf("parse full offboard req: %v", err)
	}
	if req.Kind != "scan_collectors" || req.Playbook != "scan_collectors.yml" || req.Attempt != 2 {
		t.Fatalf("parsed req mismatch: %+v", req)
	}
}

// TestRunnerOffboardMissingBudget 缺预算即拒（D5）：执行端绝不自编时限。
func TestRunnerOffboardMissingBudget(t *testing.T) {
	status, note, res := runnerExecuteOffboard(L1Task{Kind: "uninstall", Payload: map[string]any{
		"kind": "uninstall", "playbook": "uninstall.yml",
		"ssh_host": "10.0.0.5", "ssh_port": 22, "ssh_user": "deploy", "ssh_pass": "p",
	}})
	if status != "failed" || note == "" {
		t.Fatalf("缺预算应拒绝：status=%v note=%q", status, note)
	}
	if res != nil {
		t.Fatalf("门禁拒绝路径不应有结构化回执: %+v", res)
	}
}

// TestL1VerdictTasksKeepsMarkers 回执任务必须保住判据（Lines/Msg/Stdout 不被剥空）。
//
// 这是 B4 最容易踩的坑：复用 compactTasks 会把 Lines 清掉、Msg/Stdout 截到 300 字节，
// L0 侧 hasOutMarker/outCheckLine 便读不到目标机自证标记 → 一次成功的卸载被判成"输出不可解析"。
func TestL1VerdictTasksKeepsMarkers(t *testing.T) {
	pr := ansibleRunResult{
		RC:    0,
		Recap: map[string]int{"ok": 3},
		Tasks: []ansibleTask{
			{Index: 1, Name: "扫描采集物", Status: "ok", Lines: []string{
				"TASK [扫描采集物] ***", "ok: [10.0.0.5]",
				"SCAN plugins_total=1 plugins_process=0 autostart=1",
			}},
		},
	}
	extra := l1ExecResultExtra(pr, []map[string]any{{"phase": "playbook", "rc": 0}}, 900,
		"/home/deploy", &sshCred{Host: "10.0.0.5", Port: 22, User: "deploy", Pass: "p"})

	// 模拟 HTTP JSON 传输（回执经 L1 Controller 转 L0，类型全部退化为 any）
	b, err := json.Marshal(extra)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr2 := l1RunResultFromExtra(got)
	if !hasOutMarker(pr2, "SCAN plugins_total=") {
		t.Fatal("重建后的结果丢了目标机自证标记（判据断链）")
	}
	if line := outCheckLine(pr2, "SCAN plugins_total="); !strings.Contains(line, "plugins_total=1") {
		t.Fatalf("核对行取不到或内容不对：%q", line)
	}
	if pr2.RC != 0 || pr2.Recap["ok"] != 3 {
		t.Fatalf("rc/recap 未正确重建：rc=%d recap=%v", pr2.RC, pr2.Recap)
	}
}