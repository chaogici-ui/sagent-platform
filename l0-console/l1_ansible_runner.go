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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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
	BudgetSec int    `json:"budget_sec"`
	FlowID    int64  `json:"flow_id,omitempty"` // 绑定信息（冗余，日志/回执自证归属）
	StepID    string `json:"step_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
}

// runnerExecuteTask 按 kind 分派执行：install 走版本包路径，卸载侧四步走 playbook 路径，
// 探路两步（B4b）走探路内核 / 远程协议核验，升级/启停四步（B4b-4）走 svc playbook 路径。
// 未知 kind 显式拒绝（回执 failed）——静默吞掉会让步骤一直卡到超时判死，问题被藏起来。
func runnerExecuteTask(t L1Task) (string, string, map[string]any) {
	switch t.Kind {
	case "install":
		return runnerExecuteInstall(t)
	case "uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart":
		return runnerExecuteOffboard(t)
	case "probe":
		return runnerExecuteProbe(t)
	case "probe_remote":
		return runnerExecuteProbeRemote(t)
	case "preflight_upgrade", "upgrade_agent", "preflight_service", "service_execute":
		return runnerExecuteSvc(t)
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
// 复用的执行链路与 L0 投递载荷（buildInstallPayload）同一语义，但独立进程不依赖
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
					"L0 清单="+req.SHA256+"，执行端实测="+fileSHA+"；触发 /api/package-cache/sync 重建缓存后重试")},
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
			req.Title+" playbook 语法校验未过（未触碰目标机）",
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

// ===================================================================
//  升级/启停执行端（SPEC-B4b-4：preflight_upgrade / upgrade_agent /
//  preflight_service / service_execute）
//
//  与卸载侧同一分工：执行端只回**原始事实**（rc/timed_out/tasks/recap/evidence），
//  成败由 L0 对账桥 l1SvcReconcile 按 svcSpecForKind(kind).Verdict 裁定——
//  判据（UPGRADE_OK / SVC_STOPPED / 版本清单反查 / 维护态登记）只在 L0 有一份。
//  升级的新二进制在执行端就近取包（resolveInstallBinaryAny，与安装同一红线）：
//  ansible copy 的控制端是 Runner，BIN_SRC 必须落在 Runner 本地磁盘。
// ===================================================================

// runnerSvcReq 升级/启停任务载荷。自足：连哪台机、渲染哪份 playbook、家目录、预算全随载荷；
// 升级的业务占位符（备份名）由 L0 投递端算好随载荷（预检结论在 L0 事件里，执行端读不到）；
// 启停的动作/目标随载荷（拉起时已过白名单，执行端再校验一道——名字要进 shell 与 when）。
type runnerSvcReq struct {
	Kind      string `json:"kind"`
	Playbook  string `json:"playbook"` // data/playbooks/<name>（loadSvcPlaybook：磁盘优先，编译期副本兜底）
	Title     string `json:"title"`    // 中文名（日志/回执自证）
	SSHHost   string `json:"ssh_host"`
	SSHPort   int    `json:"ssh_port"`
	SSHUser   string `json:"ssh_user"`
	SSHPass   string `json:"ssh_pass"`
	DestHome  string `json:"dest_home,omitempty"` // 目标机安装根目录（缺省由 AGENT_HOME_ROOT + user 推导）
	BudgetSec int    `json:"budget_sec"`
	// 升级专用（upgrade_agent）
	Tag     string `json:"tag,omitempty"`      // 人工选定版本 tag（就近取包用）
	SHA256  string `json:"sha256,omitempty"`   // L0 版本清单 sha256（取包后断言，不降级）
	Version string `json:"version,omitempty"`  // 版本号（回执展示用）
	BakName string `json:"bak_name,omitempty"` // 旧二进制备份文件名（SAgent.bak-<旧版本>）
	// 启停专用（service_execute）
	SvcAction string `json:"svc_action,omitempty"` // stop / start / restart
	SvcTarget string `json:"svc_target,omitempty"` // process / plugin:<name>
	FlowID    int64  `json:"flow_id,omitempty"`
	StepID    string `json:"step_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
}

// parseRunnerSvcReq JSON payload → runnerSvcReq。前置校验只做"载荷是否自足"：
// kind 必须落在升级/启停支持集、playbook/title 必填（渲染与归因都靠它们）。
func parseRunnerSvcReq(payload map[string]any) (*runnerSvcReq, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req runnerSvcReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	if svcSpecForKind(req.Kind) == nil {
		return nil, fmt.Errorf("kind=%q 非升级/启停执行类型", req.Kind)
	}
	if req.Playbook == "" {
		return nil, fmt.Errorf("playbook 必填（渲染哪份 playbook）")
	}
	if req.Title == "" {
		return nil, fmt.Errorf("title 必填（失败摘要与审计归因）")
	}
	return &req, nil
}

// runnerExecuteSvc 执行一个升级/启停任务（渲染 → 语法门禁 → 流式执行 → 结构化回执）。
//
// 与 runnerExecuteOffboard 同构，差异只有两处业务占位符：
//   - upgrade_agent：就近取包（L1 Package Cache 命中 → 回源 L0 → 仍不一致拒绝，不降级）
//     后把 Runner 本地路径填进 @BIN_SRC@，落盘 sha 再与 L0 清单断言一道
//   - service_execute：@SVC_ACTION@ / @SVC_TARGET@ 白名单再校验（与 L0 svcParams.valid 同一口径）
func runnerExecuteSvc(t L1Task) (string, string, map[string]any) {
	req, err := parseRunnerSvcReq(t.Payload)
	if err != nil {
		return "failed", "升级/启停载荷解析失败：" + err.Error(), nil
	}
	// 预算（D5）：缺预算即载荷不完整，宁可当场拒绝也不自编时限（与安装/卸载同一原则）
	if req.BudgetSec <= 0 {
		return "failed", "载荷缺 budget_sec（L0 未下发执行预算），拒绝执行", nil
	}
	// 载荷自足校验（纯逻辑，先于环境检查）：缺什么当场拒，不等到跑起来才炸
	switch req.Kind {
	case "upgrade_agent":
		if req.Tag == "" {
			return "failed", "载荷缺 tag（L0 未下发人工选定版本），拒绝升级", nil
		}
	case "service_execute":
		// 白名单校验（与 L0 svcParams.valid 同一套口径）：名字会渲染进 shell 与 when，
		// 双端各拦一道，注入防线不因执行端切换而变
		p := svcParams{Action: req.SvcAction, Target: req.SvcTarget}
		if !p.valid() {
			return "failed", fmt.Sprintf("启停参数非法（action=%q target=%q），拒绝执行", req.SvcAction, req.SvcTarget), nil
		}
	}
	if !ansibleAvailable() {
		return "failed", "运行环境缺少 ansible-playbook/sshpass，无法执行" + req.Title + "（回落人工包装）", nil
	}
	cred := &sshCred{Host: req.SSHHost, Port: req.SSHPort, User: req.SSHUser, Pass: req.SSHPass}
	if !cred.valid() {
		return "failed", "SSH 凭据不齐备（缺 host/user/pass），拒绝执行：" + strings.Join(cred.credMissing(), ","), nil
	}

	// 家目录口径与安装/卸载完全一致（destHomeFor）：装在哪就动哪
	destHome := req.DestHome
	if destHome == "" {
		destHome = destHomeFor(cred)
	}

	// 业务占位符（与进程内 svcSpec.Extra 同语义，判定同码同文案）。
	// 升级取包结论暂存局部变量：回执载荷在 l1ExecResultExtra 统一拼装，避免字段被覆盖
	extras := map[string]string{}
	var upTag, upVer, upSHA, upBak string
	switch req.Kind {
	case "upgrade_agent":
		// 就近取包：与安装同一红线——缓存命中校验 → 失效回源 L0 → 仍不一致拒绝，不降级
		binPath, err := resolveInstallBinaryAny(req.Tag)
		if err != nil {
			return "failed", "就近取包失败/拒绝（" + req.Tag + "）：" + err.Error(), nil
		}
		binData, err := os.ReadFile(binPath)
		if err != nil {
			return "failed", "读取包文件失败（" + binPath + "）：" + err.Error(), nil
		}
		fileSHA := sha256HexBytes(binData)
		// 载荷 sha 断言（红线：不降级）：确认"取到的包"就是"L0 认定的那个包"
		if req.SHA256 != "" && req.SHA256 != fileSHA {
			return "failed", fmt.Sprintf("就近取包 sha256 与 L0 下发不一致，拒绝升级（L0=%s 本地=%s）",
					clip(req.SHA256, 16), clip(fileSHA, 16)),
				map[string]any{
					"tag": req.Tag, "sha256": fileSHA, "sha_expected": req.SHA256, "sha_verified": false,
					"diagnosis": []diag{fatalDiag("binary_sha256_mismatch", "就近取包 sha256 与 L0 下发不一致，拒绝升级",
						"L0 清单="+req.SHA256+"，执行端实测="+fileSHA+"；触发 /api/package-cache/sync 重建缓存后重试")},
				}
		}
		// 备份名兜底：L0 已按预检反查填好；载荷缺失时时间戳兜底（与进程内 upgradeExtras 同形）
		bak := req.BakName
		if bak == "" {
			bak = "SAgent.bak-" + time.Now().Format("20060102-150405")
		}
		extras["@BIN_SRC@"] = mustAbs(binPath)
		extras["@BIN_SHA@"] = fileSHA
		extras["@BAK_NAME@"] = bak
		upTag, upVer, upSHA, upBak = req.Tag, req.Version, fileSHA, bak
	case "service_execute":
		extras["@SVC_ACTION@"] = req.SvcAction
		extras["@SVC_TARGET@"] = req.SvcTarget
	}

	// 渲染 playbook：loadSvcPlaybook 磁盘外置优先、编译期副本兜底（升级/启停动作改文件不改代码）
	pbBody := loadSvcPlaybook(req.Playbook, destHome, extras)
	if strings.TrimSpace(pbBody) == "" {
		return "failed", "取不到 " + req.Playbook + " 的内容（外置文件缺失且内置副本为空），拒绝执行", nil
	}

	work, err := os.MkdirTemp("", "l1-svc-*")
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
		failPr := ansibleRunResult{Output: synOut, RC: synRC}
		res := l1ExecResultExtra(failPr, phases, req.BudgetSec, destHome, cred)
		res["diagnosis"] = []diag{fatalDiag(req.Kind+"_syntax",
			req.Title+" playbook 语法校验未过（未触碰目标机）",
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
	if upTag != "" {
		// 升级取包留痕随回执：对账桥据此在 L0 侧记包下载事件（同 install 的 runner-resolve 口径）
		res["tag"], res["version"] = upTag, upVer
		res["sha256"], res["sha_verified"], res["bak_name"] = upSHA, true, upBak
	}

	if pr.TimedOut {
		res["diagnosis"] = []diag{fatalDiag("ansible_phase_timeout",
			fmt.Sprintf("%s playbook 超过 %ds（L0 下发预算）未返回，执行端已终止", req.Title, req.BudgetSec),
			"目标机负载过高或停进程等待卡住；核对目标机后点「↻ 重试该步」")}
		return "failed", fmt.Sprintf("%s playbook 超过 %ds 未返回（已终止）；recap=%s",
			req.Title, req.BudgetSec, recapLine(pr.Recap, pr.RecapSeq)), res
	}
	if pr.RC != 0 {
		// 归因与进程内同源（classifySvcFailure）：失败结论不因执行端切换而变
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		d := classifySvcFailure(failText, svcSpecForKind(req.Kind))
		res["diagnosis"] = []diag{d}
		return "failed", fmt.Sprintf("%s失败：%s（rc=%d）；recap=%s",
			req.Title, clip(d.Message, 800), pr.RC, recapLine(pr.Recap, pr.RecapSeq)), res
	}

	// 成功：只回原始事实，成败裁定由 L0 对账桥按自证核对行 + 版本清单落笔
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

// ===================================================================
//  探路执行端（SPEC-B4b B4b-1 / B4b-2）
//
//  执行端只回**原始事实**，成败由 L0 裁定（SPEC §2.3）：
//    probe        —— SSH 到承载机跑五阶段（与 L0 探路内核共用 runProbePhases）
//    probe_remote —— 从本机（采集机）对「被采对象」做协议级+认证级核验（不 SSH 到任何主机）
// ===================================================================

// runnerProbeReq 主机探路载荷（kind=probe，B4b-1）。
// SSH 落点 = 承载机（L0 侧按 flow.AgentID 解析后下发，SPEC-B4b §零.三 G1），执行端只按载荷连。
// 能力可行性检查项由 L0 生成后随载荷——引擎里不出现任何插件名（一处声明三处复用）。
type runnerProbeReq struct {
	SSHHost    string         `json:"ssh_host"`
	SSHPort    int            `json:"ssh_port"`
	SSHUser    string         `json:"ssh_user"`
	SSHPass    string         `json:"ssh_pass"`
	PortSource string         `json:"port_source,omitempty"`
	ConsoleURL string         `json:"console_url"`
	TargetKind string         `json:"target_kind,omitempty"`
	Probes     []abilityProbe `json:"probes,omitempty"`
	RunSec     int            `json:"run_sec"`
	BudgetSec  int            `json:"budget_sec"`
	FlowID     int64          `json:"flow_id,omitempty"`
	StepID     string         `json:"step_id,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
}

// parseRunnerProbeReq JSON payload → runnerProbeReq。
// run_sec 必填：各阶段预算由它推导（D5 单点），缺了就只能自编时限——那正是谎报的根源。
func parseRunnerProbeReq(payload map[string]any) (*runnerProbeReq, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req runnerProbeReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	if req.RunSec <= 0 {
		return nil, fmt.Errorf("run_sec 必填（探路各阶段预算由它推导）")
	}
	return &req, nil
}

// runnerExecuteProbe 执行主机探路（B4b-1）：从采集机侧 SSH 到承载机跑五阶段。
// 与 L0 探路内核共用 runProbePhases（判据只能有一套），但**不在这里裁定成败**：
// blockerFail 门禁 / 台账回写 / 回报都回 L0（对账桥 l1ProbeReconcile → probeFinish）。
func runnerExecuteProbe(t L1Task) (string, string, map[string]any) {
	if t.Kind != "probe" {
		return "failed", fmt.Sprintf("kind=%q 非主机探路任务，Runner 拒绝执行", t.Kind), nil
	}
	req, err := parseRunnerProbeReq(t.Payload)
	if err != nil {
		return "failed", "探路载荷解析失败：" + err.Error(), nil
	}
	if req.BudgetSec <= 0 {
		return "failed", "载荷缺 budget_sec（L0 未下发执行预算），拒绝执行", nil
	}
	if !ansibleAvailable() {
		return "failed", "运行环境缺少 ansible-playbook/sshpass，无法执行探路（回落人工包装）", nil
	}
	// PortSource 随载荷透传（端口来源判定只能有一套）：不带它，withPortSource 会把
	// 台账登记值误判成平台默认 22，产出 port_source_default 假告警。
	cred := &sshCred{Host: req.SSHHost, Port: req.SSHPort, User: req.SSHUser, Pass: req.SSHPass, PortSource: req.PortSource}
	if !cred.valid() {
		return "failed", "SSH 凭据不齐备（缺 host/user/pass），拒绝探路：" + strings.Join(cred.credMissing(), ","), nil
	}
	inv, cleanupInv, err := writeInventory(cred)
	if err != nil {
		return "failed", "生成 ansible inventory 失败：" + err.Error(), nil
	}
	defer cleanupInv()

	// 阶段预算由 L0 下发的 run_sec 推导（D5）；执行端无 stepRunState，阶段机不在本进程落库
	spec := StepTimeout{RunSec: req.RunSec}
	var tasks []ansibleTask
	hook := func(at ansibleTask, done int) { tasks = append(tasks, at) }
	po, ab := runProbePhases(inv, spec, cred, hook, req.ConsoleURL, req.Probes,
		func(string) {}, nil,
		func(url, code string) (string, []diag, map[string]any) {
			return runnerResolveAccessPoint(inv, spec, cred, req.TargetKind, url, code)
		})
	if ab != nil {
		res := map[string]any{
			"phase": ab.detail["phase"], "evidence": ab.detail["evidence"],
			"budget_sec": req.BudgetSec, "diagnosis": ab.diags, "tasks": compactTasks(tasks),
		}
		return "failed", ab.summary, res
	}
	res := l1ProbeResultExtra(po, tasks, nil, req.BudgetSec)
	return "done", fmt.Sprintf("探路完成 target=%s（结论由平台裁定）", cred.target()), res
}

// runnerResolveAccessPoint 接入点兜底（B4b-3：autoResolveAccessPoint 下沉到执行端）。
//
// 与进程内同源：候选实测仍从**目标机侧**发起（同一套候选/探测原语），
// 差别只在隧道——ssh 客户端是采集机（Runner），故 -R 的转发目标必须指向 L0 console
// （l0ConsoleForward()；L0↔L1 直连允许，SPEC P3），不能沿用 127.0.0.1（那是采集机自身）。
// **不写台账**：选定与落笔是 L0 的账本职责（谁持账本谁落笔），回执带 auto_resolve 由对账桥落。
func runnerResolveAccessPoint(inv string, spec StepTimeout, cred *sshCred, targetKind, consoleURL, connCode string) (string, []diag, map[string]any) {
	diags := []diag{}
	evidence := map[string]any{"probed_console_url": consoleURL, "conn_code": connCode}

	// ① 候选地址实测：从目标机侧探「平台默认接入点」，可达即采用（与进程内同一条 curl→wget 链）
	if cands := accessPointCandidates(consoleURL); len(cands) > 0 && connCode != "NO_TOOL" {
		urls := make([]string, 0, len(cands))
		for _, c := range cands {
			urls = append(urls, c.URL)
		}
		ctxFB, cancelFB := context.WithTimeout(context.Background(), phaseBudget(spec, budgetPing))
		fb := runAnsibleStream(ctxFB, inv, nil, "-m", "shell", "-a", candidateProbeCmd(urls))
		cancelFB()
		reachable := map[string]string{}
		for _, c := range parseCandidateProbe(fb.Output) {
			reachable[c.URL] = c.Tool
		}
		rows := []map[string]string{}
		for _, c := range cands {
			row := map[string]string{"url": c.URL, "verdict": "unreachable"}
			if tool, ok := reachable[c.URL]; ok {
				row["verdict"], row["tool"] = "reachable", tool
			}
			rows = append(rows, row)
		}
		evidence["candidates"] = rows
		if url, tool, ok := firstReachableCandidate(cands, reachable); ok {
			diags = append(diags, infoDiag("access_point_auto_selected",
				fmt.Sprintf("采集机侧实测候选接入点 %s 可达（HTTP 200，工具 %s），自动采用；资源台账由平台回写", url, tool)))
			evidence["resolved"], evidence["resolved_url"], evidence["resolved_via"] = true, url, "candidate"
			return url, diags, evidence
		}
	}

	// ② 反向隧道自动建立：ssh 客户端 = 采集机，转发目标 = L0 console（B4b-3 的参数化）
	if targetKind != "docker" {
		fwdHost, fwdPort := l0ConsoleForward()
		started, err := startReverseTunnelTo(cred.Host, cred, fwdHost, fwdPort)
		if err != nil {
			diags = append(diags, warnDiag("tunnel_start_failed", "采集机侧建立反向隧道失败："+err.Error(), ""))
		} else {
			// 隧道刚建立，给 sshd 监听一点落地时间再从目标机实测
			time.Sleep(2 * time.Second)
			tunnelURL := fmt.Sprintf("http://127.0.0.1:%d", tunnelRemotePort())
			ctxT, cancelT := context.WithTimeout(context.Background(), phaseBudget(spec, budgetPing))
			tr := runAnsibleStream(ctxT, inv, nil, "-m", "shell", "-a", candidateProbeCmd([]string{tunnelURL}))
			cancelT()
			tunnelOK := ""
			for _, c := range parseCandidateProbe(tr.Output) {
				if c.URL == tunnelURL {
					tunnelOK = c.Tool
				}
			}
			evidence["tunnel"] = map[string]any{
				"started": started, "url": tunnelURL, "reachable": tunnelOK != "",
				"forward": fmt.Sprintf("%s:%d", fwdHost, fwdPort),
			}
			if tunnelOK != "" {
				note := "平台已自动建立反向隧道"
				if !started {
					note = "平台复用已在跑的反向隧道"
				}
				diags = append(diags, infoDiag("access_point_auto_selected",
					fmt.Sprintf("%s（采集机侧发起）：目标机 127.0.0.1:%d → %s:%d，实测 HTTP 200（工具 %s）；资源台账由平台回写",
						note, tunnelRemotePort(), fwdHost, fwdPort, tunnelOK)))
				evidence["resolved"], evidence["resolved_url"], evidence["resolved_via"] = true, tunnelURL, "tunnel"
				return tunnelURL, diags, evidence
			}
			diags = append(diags, warnDiag("tunnel_established_but_unreachable",
				"反向隧道已建立，但目标机实测 127.0.0.1:"+fmt.Sprint(tunnelRemotePort())+" 仍不可达——目标机侧监听未生效或 sshd 转发被禁",
				"核对目标机 sshd_config 的 AllowTcpForwarding 是否为 yes；点「↻ 重试该步」让平台再试"))
		}
	}

	evidence["resolved"] = false
	return "", diags, evidence
}

// runnerProbeRemoteReq 远程可达性探路载荷（kind=probe_remote，B4b-2）。
// **不含 SSH**：被采对象不是 SSH 落点（SPEC-B4b §零.二）。口令来自采集参数，随载荷下发，
// 明文只用于 AUTH 核验，回执里只带结论与错误摘要，不带口令。
type runnerProbeRemoteReq struct {
	Plugin    string `json:"plugin,omitempty"`
	Address   string `json:"address"`
	Port      string `json:"port"`
	Password  string `json:"password,omitempty"`
	BudgetSec int    `json:"budget_sec"`
	FlowID    int64  `json:"flow_id,omitempty"`
	StepID    string `json:"step_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
}

// parseRunnerProbeRemoteReq JSON payload → runnerProbeRemoteReq。
// 地址/端口必填：没有落点就无从核验，宁可当场拒绝，也不回一条"看起来通过"的空结论。
func parseRunnerProbeRemoteReq(payload map[string]any) (*runnerProbeRemoteReq, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var req runnerProbeRemoteReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Address) == "" || strings.TrimSpace(req.Port) == "" {
		return nil, fmt.Errorf("载荷缺被采对象地址/端口")
	}
	return &req, nil
}

// runnerExecuteProbeRemote 执行远程可达性探路（B4b-2）：从采集机侧对「被采对象」做
// 真实连接核验（Redis：AUTH+PING 期待 +PONG；其余类型 TCP 兜底）。
// 只回原始事实（tcp_ok/protocol_ok/auth_ok/reply），成败由 L0 对账桥裁定。
func runnerExecuteProbeRemote(t L1Task) (string, string, map[string]any) {
	if t.Kind != "probe_remote" {
		return "failed", fmt.Sprintf("kind=%q 非远程探路任务，Runner 拒绝执行", t.Kind), nil
	}
	req, err := parseRunnerProbeRemoteReq(t.Payload)
	if err != nil {
		return "failed", "远程探路载荷解析失败：" + err.Error(), nil
	}
	if req.BudgetSec <= 0 {
		return "failed", "载荷缺 budget_sec（L0 未下发执行预算），拒绝执行", nil
	}
	res, note := probeRemoteTarget(req.Plugin, req.Address, req.Port, req.Password,
		time.Duration(req.BudgetSec)*time.Second)
	res["budget_sec"] = req.BudgetSec
	return "done", note, res
}

// isRedisEndpoint 是否按 Redis 协议核验：插件名含 redis，或端口为 Redis 默认 6379。
func isRedisEndpoint(plugin, port string) bool {
	if strings.Contains(strings.ToLower(plugin), "redis") {
		return true
	}
	return strings.TrimSpace(port) == "6379"
}

// redisRESP 编码一条 RESP 数组命令（内联，不引入 redis 客户端依赖）。
func redisRESP(args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.String()
}

// redisReadLine 读一行 RESP 简单回复（+OK / +PONG / -ERR ...）并去掉行尾。
func redisReadLine(br *bufio.Reader) string {
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "读取回复失败：" + err.Error()
	}
	return strings.TrimRight(line, "\r\n")
}

// probeRemoteTarget 从本机（采集机）对被采对象做一次协议级+认证级核验。
// 起因：裸 TCP 探通 ≠ 能采到——口令错、库不可用都能让端口通而采集失败（SPEC-B4b §零.二），
// 故 Redis 必须走 AUTH+PING 实际握手，只有真的拿到 +PONG 才算通过。
// 返回的 map 是**原始事实**，判定由 L0 落笔；note 是人话摘要（回执说明用）。
func probeRemoteTarget(plugin, addr, port, pass string, budget time.Duration) (map[string]any, string) {
	host := net.JoinHostPort(addr, port)
	res := map[string]any{
		"plugin": plugin, "address": addr, "port": port, "target": host,
		"tcp_ok": false, "protocol_ok": false, "auth_ok": false,
	}
	if budget <= 0 || budget > 30*time.Second {
		budget = 15 * time.Second
	}
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: budget}).Dial("tcp", host)
	res["elapsed_ms"] = time.Since(start).Milliseconds()
	if err != nil {
		res["reply"] = err.Error()
		return res, fmt.Sprintf("被采对象 %s 不可达：%v", host, err)
	}
	defer conn.Close()
	res["tcp_ok"] = true

	// 非 Redis：TCP 端口可达即视为通过（不做协议级核验，故结论里如实标注）
	if !isRedisEndpoint(plugin, port) {
		res["protocol_ok"], res["auth_ok"] = true, true
		res["tool"] = "tcp"
		res["reply"] = "TCP 端口可达"
		return res, fmt.Sprintf("被采对象 %s TCP 可达（类型 %s 未做协议级核验）", host, firstNonEmpty(plugin, "未知"))
	}

	res["tool"] = "redis-resp"
	_ = conn.SetDeadline(time.Now().Add(budget))
	br := bufio.NewReader(conn)

	// 认证级核验：提供口令就必须 AUTH 成功；未提供口令则跳过（无认证可核，不因此判失败）
	if strings.TrimSpace(pass) != "" {
		res["auth_required"] = true
		if _, err := io.WriteString(conn, redisRESP("AUTH", pass)); err != nil {
			res["reply"] = err.Error()
			return res, "Redis AUTH 发送失败：" + err.Error()
		}
		reply := redisReadLine(br)
		res["reply"] = reply
		if !strings.HasPrefix(reply, "+OK") {
			return res, fmt.Sprintf("Redis %s 口令核验未通过：%s", host, clip(reply, 200))
		}
		res["auth_ok"] = true
	}

	// 协议级核验：PING 必须返回 +PONG（未认证时 Redis 回 -NOAUTH，同样算未通过）
	if _, err := io.WriteString(conn, redisRESP("PING")); err != nil {
		res["reply"] = err.Error()
		return res, "Redis PING 发送失败：" + err.Error()
	}
	reply := redisReadLine(br)
	res["reply"] = reply
	if strings.HasPrefix(reply, "+PONG") {
		res["protocol_ok"] = true
		if strings.TrimSpace(pass) == "" {
			res["auth_ok"] = true
		}
		return res, fmt.Sprintf("Redis %s 协议级+认证级核验通过（AUTH+PING 返回 +PONG）", host)
	}
	return res, fmt.Sprintf("Redis %s PING 未返回 +PONG：%s", host, clip(reply, 200))
}
