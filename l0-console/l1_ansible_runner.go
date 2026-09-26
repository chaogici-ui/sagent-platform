package main

// l1_ansible_runner.go —— L1 Ansible Runner 执行器（架构 D5 IN4）。
//
// 职责：作为独立的 L1 执行进程（进程 #4），「反向经 L1 Controller 拉安装任务 → 本地执行
// playbook → 回执终态」。与 Controller 共享同一套任务传输原语（l1PullTasks /
// l1AckTask），按 kind 分派：install 走版本包路径，卸载侧四步
// （uninstall / scan_collectors / uninstall_plugins / cleanup_autostart）走 playbook 路径，
// 把首装与卸载的执行端从 L0 进程内搬到 L1 采集机，达成「4 进程独立 + 1 tar.gz 打包」体裁。
//
// 网络域：只挂 l1-net——上游是 L1 Controller（控制面唯一出口），且需从 l1-net 直连
// 被管主机执行安装。
//
// 执行语义与 L0 进程内安装保持一致（复用同一批原语）：
//   - 就近取包（resolveInstallBinaryAny：L1 Package Cache 命中 → 回源 L0 → 仍不一致拒绝）
//   - 配置渲染 + playbook 渲染 + inventory 生成
//   - --syntax-check 语法门禁（碰目标机前拦下）
//   - 流式执行 + 逐任务结构化回执（recapLine / tasks / phases / evidence，随 ack 的 result 回传）
//   - 执行预算由 L0 随载荷下发（budget_sec），执行端不自设时限
//   - 采集目标登记不在执行端做（scrape-sd 是 L0 侧台账，执行端写自己的卷不生效），
//     由 L0 对账桥在步骤转 ok 时落笔
//
// 供 `l0-console -ansible-runner` 模式与 compose `l1-ansible-runner` 服务使用。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	cfgRunnerID     = envOr("L1_ANSIBLE_RUNNER_ID", "l1-ansible-runner-1")
	cfgRunnerListen = envOr("L1_ANSIBLE_RUNNER_LISTEN", ":8462")
	cfgRunnerLoopMS = envOr("L1_ANSIBLE_RUNNER_LOOP_MS", "3000")
)

// runAnsibleRunnerDaemon 独立 Ansible Runner 进程：拉任务 → 执行 → 回执，阻塞。
// 不认识的 kind 一律回执 failed（执行端宁可显式拒绝，也不能把任务吞掉——吞掉 = 步骤卡到超时）。
func runAnsibleRunnerDaemon() {
	loopMS, _ := strconv.Atoi(cfgRunnerLoopMS)
	if loopMS <= 0 {
		loopMS = 3000
	}
	fmt.Printf("L1 Ansible Runner daemon starting id=%s upstream=%s loop=%dms\n", cfgRunnerID, cfgRunnerUpstream(), loopMS)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true, "role": "l1-ansible-runner", "id": cfgRunnerID,
			"upstream": cfgRunnerUpstream(), "ansible_available": ansibleAvailable(),
		})
	})
	go func() {
		log.Fatal(http.ListenAndServe(cfgRunnerListen, mux))
	}()

	base := strings.TrimSuffix(cfgRunnerUpstream(), "/")
	pullURL := base + "/api/l1/task/pull"
	ackURL := base + "/api/l1/task/ack"

	ticker := time.NewTicker(time.Duration(loopMS) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		tasks := l1PullTasks(pullURL, cfgRunnerID)
		if len(tasks) == 0 {
			continue
		}
		log.Printf("[ansible-runner] pulled %d pending task(s)", len(tasks))
		for _, t := range tasks {
			status, note, result := runnerExecuteTask(t)
			l1AckTask(ackURL, cfgRunnerID, t.ID, status, "ansible-runner", note, result)
			log.Printf("[ansible-runner] task %s kind=%s -> status=%s note=%q", t.ID, t.Kind, status, note)
		}
	}
}

// cfgRunnerUpstream Runner 的上游：L1 Controller（L1_CONTROLLER_URL，默认 http://l1-controller:8461）。
// 控制面出口唯一——Runner 与 Gateway 一样不持有 L0 可达，任务拉取/回执经 Controller 出网
// （`/api/l1/task/{pull,ack}` 在 controllerControlPaths 白名单内）。
func cfgRunnerUpstream() string {
	return envOr("L1_CONTROLLER_URL", "http://l1-controller:8461")
}

// runnerInstallReq 安装任务载荷：完整自足（独立执行端不能读 L0 的流水线/资源台账，
// 所有执行所必需的字段必须随任务携带）。
// 渲染所需 tag/sha 随载荷（D3 薄执行端）：执行端只按载荷渲染，不查 L0 的版本清单。
type runnerInstallReq struct {
	Tag        string `json:"tag"`                 // 版本 tag（如 0.4.0）
	SHA256     string `json:"sha256,omitempty"`    // 该 tag 在 L0 版本清单里登记的 sha256（执行端取包后核对，不降级）
	Version    string `json:"version,omitempty"`   // 版本号（回执展示用）
	AgentID    string `json:"agent_id"`            // 资源 ID（SAgent 写入 identity / 抓取目标标签）
	ConsoleURL string `json:"console_url"`         // SAgent 控制通道接入点（域名/可达地址）
	SSHHost    string `json:"ssh_host"`            //
	SSHPort    int    `json:"ssh_port"`            //
	SSHUser    string `json:"ssh_user"`            //
	SSHPass    string `json:"ssh_pass"`            //
	DestHome   string `json:"dest_home,omitempty"` // 目标机安装根目录（缺省由 AGENT_HOME_ROOT + user 推导）
	Collector  bool   `json:"collector,omitempty"` // 承载端是采集机（远程采集）：渲染 type=proxy + 10s 短心跳
	Pool       string `json:"pool,omitempty"`      // 采集机池归属（漂移边界）：非空时随配置下发，保证重装不丢池
	// BudgetSec L0 下发的执行预算秒数（D5）：执行端按此设 ctx，不再硬编码时限。
	// 双超时口径会让"超时判死"互相矛盾（L0 按预算倒计时、执行端却提前 10min 杀），故由 L0 单点决定。
	BudgetSec int   `json:"budget_sec"`
	FlowID    int64 `json:"flow_id,omitempty"` // 绑定信息（冗余，日志/回执自证归属）
	StepID    string `json:"step_id,omitempty"`
	Attempt   int   `json:"attempt,omitempty"`
}

// runnerExecuteTask 按 kind 分派执行：install 走版本包路径，卸载侧四步走 playbook 路径。
// 未知 kind 显式拒绝（回执 failed）——静默吞掉会让步骤一直卡到超时判死，问题被藏起来。
func runnerExecuteTask(t L1Task) (string, string, map[string]any) {
	switch t.Kind {
	case "install":
		return runnerExecuteInstall(t)
	case "uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart":
		return runnerExecuteOffboard(t)
	}
	if strings.HasPrefix(t.Kind, "ansible-") {
		return runnerExecuteInstall(t)
	}
	return "failed", fmt.Sprintf("kind=%q 非执行端任务，Runner 拒绝执行", t.Kind), nil
}

// runnerExecuteInstall 执行一个安装任务，返回 (status, note, result)。
//
// result 是结构化回执（SPEC D2）：tasks/recap/phases/sha/evidence/diagnosis 全带上，
// L0 侧对账桥直接把它并进步骤证据——只回 note 字符串会让逐任务回执与安装结论断链。
//
// 复用的执行链路与 L0 进程内 ansibleInstallJob 同一语义，但独立进程不依赖
// L0 的 flowRun 状态机——载荷自足，失败即回执 failed（由 L0 侧决定重试与否）。
func runnerExecuteInstall(t L1Task) (string, string, map[string]any) {
	if t.Kind != "install" && !strings.HasPrefix(t.Kind, "ansible-") {
		return "failed", fmt.Sprintf("kind=%q 非安装任务，Runner 拒绝执行", t.Kind), nil
	}

	req, err := parseRunnerReq(t.Payload)
	if err != nil {
		return "failed", "安装载荷解析失败：" + err.Error(), nil
	}
	// 预算（D5）：缺预算即载荷不完整。宁可当场拒绝，也不用执行端自编的时限去杀作业——
	// 那正是"界面按 900s 倒计时、实际 600s 就被判死"这类谎报的根源。
	if req.BudgetSec <= 0 {
		return "failed", "载荷缺 budget_sec（L0 未下发执行预算），拒绝执行", nil
	}

	// 前置门禁：ansible 可用性 + 凭据齐备（与平台执行器同一套判定）
	if !ansibleAvailable() {
		return "failed", "运行环境缺少 ansible-playbook/sshpass，无法执行安装（回落人工包装）", nil
	}
	cred := &sshCred{Host: req.SSHHost, Port: req.SSHPort, User: req.SSHUser, Pass: req.SSHPass}
	if !cred.valid() {
		return "failed", "SSH 凭据不齐备（缺 host/user/pass），拒绝安装：" + strings.Join(cred.credMissing(), ","), nil
	}

	// 就近取包：L1 Package Cache 命中校验 → 失效回源 L0 → 仍不一致拒绝（红线，不降级用旧包）
	binPath, err := resolveInstallBinaryAny(req.Tag)
	if err != nil {
		return "failed", "就近取包失败/拒绝（" + req.Tag + "）：" + err.Error(), nil
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return "failed", "读取包文件失败（" + binPath + "）：" + err.Error(), nil
	}
	fileSHA := sha256HexBytes(binData)

	// 载荷 sha 核对（红线：不降级）：L0 已按版本清单核对过一次，执行端再核一次——
	// 确认"取到的包"就是"L0 认定的那个包"（缓存被篡改 / 清单漂移都拦在这里）。
	if req.SHA256 != "" && req.SHA256 != fileSHA {
		return "failed", fmt.Sprintf("就近取包 sha256 与 L0 下发不一致，拒绝安装（L0=%s 本地=%s）",
				clip(req.SHA256, 16), clip(fileSHA, 16)),
			map[string]any{
				"tag": req.Tag, "sha256": fileSHA, "sha_expected": req.SHA256, "sha_verified": false,
				"diagnosis": []diag{fatalDiag("binary_sha256_mismatch", "就近取包 sha256 与 L0 下发不一致，拒绝安装",
					"L0 清单=" + req.SHA256 + "，执行端实测=" + fileSHA + "；触发 /api/package-cache/sync 重建缓存后重试")},
			}
	}

	// 渲染配置 + playbook + inventory
	work, err := os.MkdirTemp("", "l1-install-*")
	if err != nil {
		return "failed", "创建临时目录失败：" + err.Error(), nil
	}
	defer os.RemoveAll(work)

	agentID := req.AgentID
	if agentID == "" {
		agentID = req.SSHHost
	}
	consoleURL := req.ConsoleURL
	if strings.TrimSpace(consoleURL) == "" {
		consoleURL = l0AgentURL()
	}
	cfgContent := renderSagentConfig(agentID, consoleURL, req.Collector, req.Pool)
	cfgPath := filepath.Join(work, "SAgent.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		return "failed", "渲染 SAgent 配置失败：" + err.Error(), nil
	}

	destHome := req.DestHome
	if destHome == "" {
		destHome = destHomeFor(cred)
	}
	pb := loadInstallPlaybook(mustAbs(binPath), fileSHA, cfgPath, destHome)
	pbPath := filepath.Join(work, "install.yml")
	if err := os.WriteFile(pbPath, []byte(pb), 0o644); err != nil {
		return "failed", "生成安装 playbook 失败：" + err.Error(), nil
	}

	inv, cleanupInv, err := writeInventory(cred)
	if err != nil {
		return "failed", "生成 ansible inventory 失败：" + err.Error(), nil
	}
	defer cleanupInv()

	// 结构化回执骨架：成功/失败两条路径共用，保证界面拿到的字段对称
	res := map[string]any{
		"tag": req.Tag, "version": req.Version, "sha256": fileSHA, "sha_verified": true,
		"config": cfgContent, "dest_home": destHome, "target": cred.target(),
		"budget_sec": req.BudgetSec,
	}
	phases := []map[string]any{}

	// 语法门禁：playbook 语法错误在碰目标机前拦下
	synOut, synRC := runSyntaxCheck(inv, pbPath)
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synRC != 0],
		"output": clip(synOut, 1500),
	})
	if synRC != 0 {
		res["phases"] = phases
		res["diagnosis"] = []diag{fatalDiag("playbook_syntax_failed",
			"playbook 语法校验未过："+clip(synOut, 600),
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")}
		return "failed", "playbook 语法校验未过（未触碰目标机）：" + clip(synOut, 1000), res
	}

	// 流式执行：ctx 由 L0 下发的预算决定（D5），执行端不自设时限
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.BudgetSec)*time.Second)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, nil)

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
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})
	res["rc"] = pr.RC
	res["timed_out"] = pr.TimedOut
	res["tasks"] = compactTasks(pr.Tasks)
	res["recap"] = pr.Recap
	res["recap_line"] = recapLine(pr.Recap, pr.RecapSeq)
	res["phases"] = phases
	res["evidence"] = evidence

	if pr.TimedOut {
		res["diagnosis"] = []diag{fatalDiag("ansible_phase_timeout",
			fmt.Sprintf("安装 playbook 超过 %ds（L0 下发预算）未返回，执行端已终止", req.BudgetSec),
			"目标机负载过高或 playbook 卡住；核对目标机后点「↻ 重试该步」")}
		return "failed", fmt.Sprintf("安装 playbook 超过 %ds 未返回（已终止）；recap=%s",
			req.BudgetSec, recapLine(pr.Recap, pr.RecapSeq)), res
	}
	if pr.RC != 0 {
		text := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(text) == "" {
			text = pr.Output
		}
		d := classifyPlaybookFailure(text, pr.RC)
		res["diagnosis"] = []diag{d}
		return "failed", fmt.Sprintf("安装失败（rc=%d）：%s；recap=%s",
			pr.RC, clip(text, 800), recapLine(pr.Recap, pr.RecapSeq)), res
	}

	// 成功：采集目标登记不在这里做——scrape-sd 是 L0 侧台账（vmagent 读的是 L0 挂的宿主目录），
	// 执行端写的是自己的卷，写了也不生效。由 L0 对账桥在步骤转 ok 时登记（谁持账本谁落笔）。
	return "done", fmt.Sprintf("安装成功 tag=%s target=%s agent_id=%s sha256=前8位%s recap=%s",
		req.Tag, cred.target(), agentID, fileSHA[:8], recapLine(pr.Recap, pr.RecapSeq)), res
}

// runnerOffboardReq 卸载侧任务载荷（uninstall / scan_collectors / uninstall_plugins / cleanup_autostart）。
// 自足：渲染哪份 playbook、连哪台机、装到哪个家目录、预算多少全随载荷——
// 执行端读不到 L0 的流水线/资源台账（D3 薄执行端）。
type runnerOffboardReq struct {
	Kind      string `json:"kind"`
	Playbook  string `json:"playbook"` // data/playbooks/<name>（uninstall 走专用模板，见下）
	Title     string `json:"title"`    // 中文名（日志/回执自证）
	SSHHost   string `json:"ssh_host"`
	SSHPort   int    `json:"ssh_port"`
	SSHUser   string `json:"ssh_user"`
	SSHPass   string `json:"ssh_pass"`
	DestHome  string `json:"dest_home,omitempty"` // 目标机安装根目录（缺省由 AGENT_HOME_ROOT + user 推导）
	BudgetSec int    `json:"budget_sec"`
	FlowID    int64  `json:"flow_id,omitempty"`
	StepID    string `json:"step_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
}

// parseRunnerOffboardReq JSON payload → runnerOffboardReq。
// 前置校验只做"载荷是否自足"：kind 必须落在卸载侧支持集、playbook 名必填。
func parseRunnerOffboardReq(payload map[string]any) (*runnerOffboardReq, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req runnerOffboardReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	if req.Kind == "" {
		return nil, fmt.Errorf("kind 必填（卸载侧执行类型）")
	}
	if req.Playbook == "" {
		return nil, fmt.Errorf("playbook 必填（卸载侧渲染哪份 playbook）")
	}
	return &req, nil
}

// runnerExecuteOffboard 执行一个卸载侧任务（渲染 → 语法门禁 → 流式执行 → 结构化回执）。
//
// 与 runnerExecuteInstall 同构，但**不在这里裁定成败**：卸载侧的成功判据是目标机自证核对行
// （dir_exists= / PLUGINS_CLEAN / AUTOSTART_DIRTY …），那属于 L0 的状态机与账本职责。
// 执行端只回原始事实（rc/timed_out/tasks/recap/evidence），由 L0 对账桥重建 ansibleRunResult
// 后复用进程内同一套 offboardFinish/uninstallFinish 裁定——判据只能有一套，
// 否则切了开关结论就变了（SPEC §2.3）。
func runnerExecuteOffboard(t L1Task) (string, string, map[string]any) {
	req, err := parseRunnerOffboardReq(t.Payload)
	if err != nil {
		return "failed", "卸载载荷解析失败：" + err.Error(), nil
	}
	// 预算（D5）：缺预算即载荷不完整，宁可当场拒绝也不自编时限（与 install 同一原则）
	if req.BudgetSec <= 0 {
		return "failed", "载荷缺 budget_sec（L0 未下发执行预算），拒绝执行", nil
	}
	if !ansibleAvailable() {
		return "failed", "运行环境缺少 ansible-playbook/sshpass，无法执行" + req.Title + "（回落人工包装）", nil
	}
	cred := &sshCred{Host: req.SSHHost, Port: req.SSHPort, User: req.SSHUser, Pass: req.SSHPass}
	if !cred.valid() {
		return "failed", "SSH 凭据不齐备（缺 host/user/pass），拒绝执行：" + strings.Join(cred.credMissing(), ","), nil
	}

	// 家目录口径与安装/卸载完全一致（destHomeFor）：装在哪就动哪，
	// 不允许两侧各写一套推导逻辑，否则会"看着成功其实什么都没碰到"
	destHome := req.DestHome
	if destHome == "" {
		destHome = destHomeFor(cred)
	}

	// 渲染 playbook：uninstall 走专用模板（含 @AGENT_PORT@），卸载侧三步走 seed playbook
	pbBody := ""
	if req.Kind == "uninstall" {
		pbBody = loadUninstallPlaybook(destHome)
	} else {
		pbBody = loadSeedPlaybook(req.Playbook, destHome)
	}
	if strings.TrimSpace(pbBody) == "" {
		return "failed", "取不到 " + req.Playbook + " 的内容（外置文件缺失且内置副本为空），拒绝执行", nil
	}

	work, err := os.MkdirTemp("", "l1-offboard-*")
	if err != nil {
		return "failed", "创建临时目录失败：" + err.Error(), nil
	}
	defer os.RemoveAll(work)
	pbPath := filepath.Join(work, req.Playbook)
	if err := os.WriteFile(pbPath, []byte(pbBody), 0o644); err != nil {
		return "failed", "生成 playbook 失败：" + err.Error(), nil
	}
	inv, cleanupInv, err := writeInventory(cred)
	if err != nil {
		return "failed", "生成 ansible inventory 失败：" + err.Error(), nil
	}
	defer cleanupInv()

	phases := []map[string]any{}

	// 语法门禁：playbook 语法错误在碰目标机前拦下（与安装/卸载同一原则）
	synOut, synRC := runSyntaxCheck(inv, pbPath)
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synRC != 0],
		"output": clip(synOut, 1500),
	})
	if synRC != 0 {
		// 语法失败用"未执行"的结果形状回执（rc=synRC、output=语法原文），
		// L0 侧据此归类为执行失败，不落进"输出不可解析"那类误判
		failPr := ansibleRunResult{Output: synOut, RC: synRC}
		res := l1ExecResultExtra(failPr, phases, req.BudgetSec, destHome, cred)
		res["diagnosis"] = []diag{fatalDiag(req.Kind+"_syntax",
			req.Title + " playbook 语法校验未过（未触碰目标机）",
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")}
		return "failed", req.Title + " playbook 语法校验未过（未触碰目标机）：" + clip(synOut, 1000), res
	}

	// 流式执行：ctx 由 L0 下发的预算决定（D5），执行端不自设时限
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.BudgetSec)*time.Second)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, nil)
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})
	res := l1ExecResultExtra(pr, phases, req.BudgetSec, destHome, cred)

	if pr.TimedOut {
		res["diagnosis"] = []diag{fatalDiag("ansible_phase_timeout",
			fmt.Sprintf("%s playbook 超过 %ds（L0 下发预算）未返回，执行端已终止", req.Title, req.BudgetSec),
			"目标机负载过高或磁盘卡住；核对目标机后点「↻ 重试该步」")}
		return "failed", fmt.Sprintf("%s playbook 超过 %ds 未返回（已终止）；recap=%s",
			req.Title, req.BudgetSec, recapLine(pr.Recap, pr.RecapSeq)), res
	}
	if pr.RC != 0 {
		// 归因与进程内同源（同一批 classify 函数）：失败结论不因执行端切换而变
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		var d diag
		if req.Kind == "uninstall" {
			d = classifyUninstallFailure(failText, pr.RC)
		} else {
			d = classifyOffboardFailure(failText, *offboardSpecForKind(req.Kind))
		}
		res["diagnosis"] = []diag{d}
		return "failed", fmt.Sprintf("%s失败：%s（rc=%d）；recap=%s",
			req.Title, clip(d.Message, 800), pr.RC, recapLine(pr.Recap, pr.RecapSeq)), res
	}

	// 成功：只回原始事实，成败裁定由 L0 对账桥按目标机自证核对行落笔
	return "done", fmt.Sprintf("%s 执行完成 rc=0 目标=%s 家目录=%s；结论由平台核对裁定",
		req.Title, cred.target(), destHome), res
}

// parseRunnerReq JSON payload → runnerInstallReq。
func parseRunnerReq(payload map[string]any) (*runnerInstallReq, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req runnerInstallReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	if req.Tag == "" {
		return nil, fmt.Errorf("tag 必填（安装哪个版本的 SAgent）")
	}
	return &req, nil
}

// sha256HexBytes 计算制御二进制 sha256（hex 串，用于与版本清单核对/回执）。
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runSyntaxCheck 单独跑 playbook 语法校验（独立执行端直接取退出码，不依赖 flow 状态机）。
func runSyntaxCheck(inv, pbPath string) (string, int) {
	cmd := exec.Command("ansible-playbook", "-i", inv, pbPath, "--syntax-check")
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False")
	out, err := cmd.CombinedOutput()
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			rc = -1
		}
	}
	return string(out), rc
}