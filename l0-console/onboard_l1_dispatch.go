package main

// onboard_l1_dispatch.go —— 步骤投递 L1 Ansible Runner（SPEC-D5-IN5 B3/B4/B5）
//
// 执行动作由「投递 L1 任务 + 等回执」完成，步骤终态由 B2 的回执对账桥驱动。
// B3 只搬 install；B4 把卸载侧四步（scan_collectors / uninstall_plugins /
// cleanup_autostart / uninstall）纳入同一支持集；B5 起执行端恒为 L1 Runner，
// 进程内 ansible 作业与灰度开关（L1_EXEC_EXECUTOR）已删除，不再有进程内回落路径。
//
// 三条边界（SPEC §2.3）：
//   - L0 只做投递与判死：构造自足载荷（D3）、定任务 ID（D6）、下发预算（D5）、
//     在载荷无法构造时按同名诊断当场失败；不替执行端渲染、不替它重试
//   - 载荷自足（D3/D4）：渲染所需的 tag/sha、控制通道接入点、池归属、SSH 凭据全部随载荷，
//     执行端读不到 L0 的流水线/资源台账；凭据与 resources 表同密级，回执后由 store 层置空
//   - 执行端恒定（B5）：支持集内的 atom 一律投递 L1 Runner；凭据缺失等前置不成立时
//     当场显式失败（可执行诊断），绝不静默回落
//
// 为什么 install 的活跃信号要单独续命：L0 侧没有执行端心跳（执行端在 L1），清扫器只能
// 靠事件判"无响应"。长安装会被 idle 阈值误判为卡死并触发重试——
// 重试换 attempt 就是**第二次真安装**，会在目标机上并发跑两份。故清扫器判定前，
// 先按「任务仍被 L1 持有且未回执」续活跃（硬上限 RunSec 不变，执行端真死照样判死）。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// execChannelL1 执行通道标记：写进 running 事件 detail。
// 超时判定认的是 executor/runner 两个字段（平台代执行 vs 人工），
// 通道只用于界面与对账分辨"这一跳在哪执行"，不影响超时口径。
const execChannelL1 = "l1"

// cfgInstallRunnerID 接收安装任务的 L1 执行端身份。
// 投递按 controller_id 定向、Runner 按同一身份拉取——两端默认值必须一致，
// 否则任务会永远停在 pending（灰度期最容易踩的坑，故共用同名环境变量与同一默认值）。
var cfgInstallRunnerID = envOr("L1_ANSIBLE_RUNNER_ID", "l1-ansible-runner-1")

// l1ExecSupportedKind 支持集：已迁移到 L1 Runner 的执行类型（B3 install；B4 卸载侧四步；
// B4b 探路两步 + 升级/启停四步）。
// 新增一个 atom = 支持集加一行。支持集是白名单——B5 起执行端恒为 L1 Runner；
// 不在支持集内的 atom 即非 ansible 类步骤（agent 回报驱动，见 tryAnsibleExecutor 的 kind 映射）。
//
// probe / probe_remote（B4b）：探路必须在采集机侧执行（P1/P2/P5）——L0 只挂 l0-net，
// 被管主机在 l1-net，L0 直连必然断链。两条探路都走同一投递/回执/对账通道，只是载荷与收口不同。
//
// preflight_upgrade / upgrade_agent / preflight_service / service_execute（B4b-4）：
// 升级与启停同 P1——L0 不越级直碰主机，执行端一律搬到采集机侧 Runner。
func l1ExecSupportedKind(kind string) bool {
	switch kind {
	case "install", "uninstall", "scan_collectors", "uninstall_plugins", "cleanup_autostart",
		"probe", "probe_remote",
		"preflight_upgrade", "upgrade_agent", "preflight_service", "service_execute":
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
// 重新进入 external 分支。执行端恒为 L1 后没有进程内占用标记——不挡的话每次轮询都会
// 重投一次并追加一条 running 事件（事件洪水 + 时间线抖动）。
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
		"dest_home":  destHomeFor(cred),
		"budget_sec": budget,
		// 绑定信息随载荷冗余一份：执行端日志/回执能自证"这条任务属于哪一步的哪次尝试"
		"flow_id": r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}, nil
}

// buildProbePayload 构造自足探路载荷（B4b-1，kind=probe）。
// 执行端 = L1 Runner（采集机侧）；SSH 落点 = 承载机（flow.AgentID，见 flowSSHCred）。
// 自足（D3）：SSH 凭据、控制通道接入点、能力可行性检查项、执行预算全部随载荷——
// 执行端读不到 L0 的流水线/资源台账/能力目录。
//
// 能力可行性检查项由 L0 生成（插件 params.yaml 的 probe 绑定 + 用户填的参数），
// 执行端只按项执行——引擎里不出现任何插件名（一处声明三处复用）。
func buildProbePayload(catDB *storepkg.DB, r *flowRun, step storepkg.FlowStepSnapshot,
	cred *sshCred, attempt int) (map[string]any, *dispatchErr) {

	if r.flow == nil {
		return nil, &dispatchErr{
			summary: "流水线不存在", reason: "flow_not_found",
			diag: fatalDiag("flow_not_found", "流水线不存在", ""),
		}
	}
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	consoleURL, targetKind := "", ""
	if res := r.resource; res != nil {
		consoleURL = strings.TrimSpace(res.AgentConsoleURL)
		targetKind = strings.TrimSpace(res.TargetKind)
	}
	if consoleURL == "" {
		consoleURL = l0AgentURL()
	}
	probes := abilityProbeChecks(loadAbilities(onboardCfg, integrationsDir), r.flow.Abilities, r.flow.Params())
	return map[string]any{
		"ssh_host": cred.Host, "ssh_port": cred.Port,
		"ssh_user": cred.User, "ssh_pass": cred.Pass,
		"port_source": cred.PortSource,
		"console_url": consoleURL, "target_kind": targetKind,
		"probes":     probes,
		"run_sec":    spec.RunSec,
		"budget_sec": budgetSec(execBudget(spec, budgetPlaybook)),
		"flow_id":    r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}, nil
}

// buildProbeRemotePayload 构造远程可达性探路载荷（B4b-2，kind=probe_remote）。
// 语义与 probe 分开：**不 SSH 到主机**，而是从采集机侧对「被采对象」做一次协议级+认证级核验
// （Redis：AUTH+PING 期待 +PONG；其余类型 TCP 兜底）。被采对象的口令来自 collect_params 参数，
// 随载荷下发，明文不进探路日志与证据。
func buildProbeRemotePayload(catDB *storepkg.DB, r *flowRun, step storepkg.FlowStepSnapshot,
	cred *sshCred, attempt int) (map[string]any, *dispatchErr) {

	if r.flow == nil {
		return nil, &dispatchErr{
			summary: "流水线不存在", reason: "flow_not_found",
			diag: fatalDiag("flow_not_found", "流水线不存在", ""),
		}
	}
	params := r.flow.Params()
	plugin, addr, port, pass := "", "", "", ""
	for _, id := range r.flow.Abilities {
		vals, ok := params[id]
		if !ok {
			continue
		}
		h, p := tcpTargetOf(vals)
		if h == "" || p == "" {
			continue
		}
		plugin, addr, port = id, h, p
		pass = firstNonEmpty(vals["password"], vals["passwd"], vals["auth"], vals["redis_password"])
		break
	}
	if addr == "" || port == "" {
		return nil, &dispatchErr{
			summary: "被采对象地址/端口缺失，无法做远程可达性探路", reason: "no_target",
			diag: fatalDiag("no_target", "被采对象地址/端口缺失，无法做远程可达性探路",
				"先在「采集参数」环节补齐被采对象地址与端口后点「↻ 重试该步」"),
		}
	}
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	return map[string]any{
		"plugin": plugin, "address": addr, "port": port, "password": pass,
		"budget_sec": budgetSec(execBudget(spec, budgetPlaybook)),
		"flow_id":    r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}, nil
}

// buildSvcPayload 构造自足升级/启停载荷（B4b-4，kind ∈ preflight_upgrade / upgrade_agent /
// preflight_service / service_execute）。
//
// 与进程内 svcSpec.Extra 同一套前置判定（同码同文案——两条执行路径的失败结论必须一致）：
//   - upgrade_agent：版本取人工选定事件（与安装同一来源），清单 sha 随载荷；备份名带当前版本
//     （预检 sha 反查，查不到用时间戳兜底）。**不读 L0 版本库二进制**——执行端读不到 L0 文件系统，
//     新二进制由执行端就近取包（resolveInstallBinaryAny，与安装同一红线），BIN_SRC 落在 Runner 本地
//   - service_execute：动作与目标来自拉起时选定（svcParams 白名单校验），随载荷下发
//   - 预检两步：纯 playbook 路径，无业务占位符
func buildSvcPayload(catDB *storepkg.DB, r *flowRun, step storepkg.FlowStepSnapshot,
	cred *sshCred, attempt int, kind string) (map[string]any, *dispatchErr) {

	sp := svcSpecForKind(kind)
	if sp == nil {
		return nil, &dispatchErr{
			summary: "执行类型 " + kind + " 无对应步骤定义，拒绝投递", reason: "unknown_kind",
			diag: fatalDiag("l1_unknown_kind", "执行类型 "+kind+" 无对应步骤定义，拒绝投递",
				"该 atom 尚未纳入 L1 支持集；核对 SPEC-D5-IN5 的支持集清单"),
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
	spec := timeoutSpecForStep(onboardCfg, step.ID, step.Atom)
	payload := map[string]any{
		"kind": kind, "playbook": sp.Playbook, "title": sp.Title,
		"ssh_host": cred.Host, "ssh_port": cred.Port,
		"ssh_user": cred.User, "ssh_pass": cred.Pass,
		"dest_home":  destHomeFor(cred),
		"budget_sec": budgetSec(execBudget(spec, budgetPlaybook)),
		// 绑定信息随载荷冗余一份：执行端日志/回执能自证"这条任务属于哪一步的哪次尝试"
		"flow_id": r.flow.ID, "step_id": step.ID, "attempt": attempt,
	}
	switch kind {
	case "upgrade_agent":
		// 版本取人工选定事件（与进程内 upgradeExtras 同一来源、同一报错文案）
		tag := pickedVersionTag(catDB, r.flow.ID)
		if tag == "" {
			return nil, &dispatchErr{
				summary: "未找到人工选定版本（pick_version 无确认记录）",
				reason:  "no_human_pick",
				diag: fatalDiag("no_human_pick", "未找到人工选定版本，拒绝升级",
					"先在「选定目标版本」环节由人工确认版本；平台不使用 latest 兜底"),
			}
		}
		ver := versionByTag(tag)
		if ver == nil {
			return nil, &dispatchErr{
				summary: "版本清单中无 " + tag, reason: "version_not_in_catalog",
				diag: fatalDiag("version_not_in_catalog", "版本清单中无 "+tag+"，拒绝升级",
					"检查 data/versions.yaml 登记项，或重新人工选版"),
			}
		}
		// 备份名：预检反查出的当前版本优先（升级失败可手工回滚），查不到（清单外版本）用时间戳兜底。
		// 预检结论在 L0 事件里，执行端读不到——必须在投递端算好随载荷
		bak := "SAgent.bak-" + time.Now().Format("20060102-150405")
		if pre := svcLatestPreflightUpgrade(catDB, r.flow.ID); pre != nil && pre.CurrentTag != "" {
			bak = "SAgent.bak-" + pre.CurrentTag
		}
		payload["tag"] = tag
		payload["sha256"] = ver.SHA256
		payload["version"] = ver.Version
		payload["bak_name"] = bak
	case "service_execute":
		// 动作与目标：拉起时已选定（svcParams 白名单校验，插件名钉死字符集——要进 shell 与 when）
		p := svcParamsOf(r.flow)
		if !p.valid() {
			return nil, &dispatchErr{
				summary: fmt.Sprintf("启停参数非法（action=%q target=%q）", p.Action, p.Target),
				reason:  "invalid_params",
				diag: fatalDiag("invalid_params",
					fmt.Sprintf("启停参数非法（action=%q target=%q）", p.Action, p.Target),
					"重新从资源页行内按钮发起启停；参数在拉起时校验"),
			}
		}
		payload["svc_action"] = p.Action
		payload["svc_target"] = p.Target
	}
	return payload, nil
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
	// B4b-1：主机事实+能力可行性探路（SSH 到承载机，1:1 复用安装路径）
	if kind == "probe" {
		return buildProbePayload(catDB, r, step, cred, attempt)
	}
	// B4b-2：远程可达性探路（不 SSH，从采集机侧对「被采对象」做协议级+认证级核验）
	if kind == "probe_remote" {
		return buildProbeRemotePayload(catDB, r, step, cred, attempt)
	}
	// B4b-4：升级/启停四步（升级含版本与备份名，启停含动作与目标，预检两步纯 playbook）
	if svcSpecForKind(kind) != nil {
		return buildSvcPayload(catDB, r, step, cred, attempt, kind)
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
	if kind == "probe" {
		return "探路"
	}
	if kind == "probe_remote" {
		return "远程可达性探路"
	}
	// B4b-4：升级/启停四步——标题与进程内 svcSpec 同源，失败摘要不因执行端切换而变
	if sp := svcSpecForKind(kind); sp != nil {
		return sp.Title
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
	// B4b：探路两类载荷的摘要（probe 记事实口径，probe_remote 记被采对象落点）
	if _, ok := payload["probes"]; ok {
		return "主机事实+能力可行性"
	}
	if v, ok := payload["address"]; ok {
		return fmt.Sprintf("对象=%v:%v", v, payload["port"])
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
	// 继承既有 running 事件的诊断与尝试历史：投递重写事件不能把「平台自己救过一次」
	// 这类运维提示与超时尝试留痕冲掉（超时自动重试先由 handleStepTimeout 落
	// step_timeout_retry warn + attempts 记录，重投时若丢了对运维不可见）
	ds := []diag{}
	if cred.PortSource == portSourceDefault {
		ds = append(ds, portSourceDiag(cred))
	}
	if ev, _ := catDB.LatestEvent(r.flow.ID, step.ID); ev != nil && ev.Status == stRunning {
		if prev := diagsOf(ev.Detail); len(prev) > 0 {
			ds = append(ds, prev...)
		}
		if hist := attemptsOf(ev.Detail); len(hist) > 0 {
			detail["attempts"] = hist
		}
	}
	if len(ds) > 0 {
		detail["diagnosis"] = diagJSON(ds)
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
