package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  原子能力与流水线推进器
//  设计依据：PLAN-采集接入中心与流程引擎.md §4.1 / §5
//
//  只有这 14 个原子能力写死在 Go 里（所有插件共用，稳定不变）。
//  「怎么编排」由 flow_templates/*.yaml 决定，「要填什么参数」由插件包 params.yaml 决定。
//  新增一个开源 exporter 不需要新原子能力，也不改这里一行。
// ===================================================================

// atomSpec 原子能力说明（执行者由模板步骤的 scope 决定：platform 自完成 / agent、external 下发后等待）
type atomSpec struct {
	ID   string
	Desc string
}

var atomRegistry = map[string]atomSpec{
	"pick_object":              {"pick_object", "选/建资源对象，拿 IP"},
	"pick_agent":               {"pick_agent", "确定承载能力的 Agent（代理机）"},
	"pick_version":             {"pick_version", "按主机探路实测结果匹配 SAgent 镜像 tag"},
	"install_agent":            {"install_agent", "生成安装命令 / 下发安装任务"},
	"uninstall_agent":          {"uninstall_agent", "卸载 SAgent：停进程 / 删安装目录 / 目标机自证核对"},
	"scan_collectors":          {"scan_collectors", "扫描采集插件与自愈自启（只读）：数量 / 形态 / 独立进程 / 自启来源"},
	"confirm_uninstall":        {"confirm_uninstall", "卸载前人工确认：呈报会被连带停掉的插件与自愈来源"},
	"uninstall_plugins":        {"uninstall_plugins", "卸载采集插件：先停插件子进程，再核验进程真的消失"},
	"cleanup_autostart":        {"cleanup_autostart", "清理自愈与自启：run.sh 守护 + crontab 中指向 SAgent 的条目"},
	"cleanup_platform":         {"cleanup_platform", "注销平台登记：Agent 台账 / 期望配置 / 抓取登记 / 采集目标"},
	"pick_ability":             {"pick_ability", "记录勾选的采集能力"},
	"collect_params":           {"collect_params", "按插件 params.yaml 落参数、建采集目标"},
	"preflight_host":           {"preflight_host", "主机探路：主机事实（OS/架构/内核/磁盘/端口）+ 能力可行性（按所选能力的参数声明核对）"},
	"preflight_ability":        {"preflight_ability", "远程可达性探路：从代理机侧核对被采目标端口与账号"},
	"register_platform_device": {"register_platform_device", "登记资源为采集平台设备"},
	"sync_config":              {"sync_config", "渲染配置文档并下发（版本 +1）"},
	"verify_probe":             {"verify_probe", "真连一次，取回一条指标"},
	"observe_collect":          {"observe_collect", "采集观测：成功率与样本"},
	"preflight_upgrade":        {"preflight_upgrade", "升级前预检（只读）：当前版本/守护形态/插件状态/磁盘空间"},
	"confirm_upgrade":          {"confirm_upgrade", "升级前人工确认：当前→目标版本（含降级提示）与影响范围"},
	"upgrade_agent":            {"upgrade_agent", "执行升级：停插件/停进程/备份旧二进制/换包/sha256 断言/启动/自证核对"},
	"preflight_service":        {"preflight_service", "启停前预检（只读）：进程/守护/暂停标记/插件清单"},
	"confirm_service":          {"confirm_service", "启停前核对：摊开预检实测与影响范围，核对通过即自动放行（启停不设人工闸门）"},
	"service_execute":          {"service_execute", "执行启停：守护协议停/启/重启（进程级）或 control.sock（插件级）并自证核对"},
}

// 步骤状态
const (
	stPending = "pending"
	stRunning = "running"
	stOK      = "ok"
	stFail    = "fail"
	stBlocked = "blocked"
	stSkipped = "skipped"
)

// flowRun 一次推进过程的运行上下文
type flowRun struct {
	catDB      *storepkg.DB
	agentStore *AgentStore
	flow       *storepkg.Flow
	resource   *storepkg.Resource
	cfg        *OnboardConfig
	abilities  []*Ability
	agentID    string
	// stepStatus 每步的最新状态（打开时从事件表 hydrate）
	stepStatus map[string]string
	// stepWaiting 每步最新的停等原因（detail.waiting_for：human_pick / probe…）。
	// 用于把「被阻断」和「等人工决策」在文案上分开——两者都不是失败，但对运维是两件事
	stepWaiting map[string]string
	// stepProgress / stepAttempts 每步最新的运行期元数据（超时判定基准 + 尝试历史）。
	// 重新下发执行包时必须原样带上：丢了等于把"第几次尝试、还能等多久"一起丢掉，
	// 超时判定就只能靠事件时间戳猜
	stepProgress map[string]any
	stepAttempts map[string]any
	// stepDiags 每步上一轮的诊断：重新下发执行包时不能把已有诊断冲掉——
	// "平台已自动重试过一次"这类告知一旦被"已下发等待回报"覆盖，运维就再也看不到
	stepDiags map[string][]diag
}

// advanceFlow 按依赖推进一条流水线。
// 语义：
//   - 平台侧步骤（scope=platform）当场执行并落 ok/fail
//   - Agent / 外部执行器步骤（scope=agent|external）落 running 并**停下**，等回报
//   - 依赖未完成 → pending（"等待前序环节完成"）；依赖失败或被阻断 → blocked
//   - 只写事件表，不改历史事件（同一步状态未变化时不重复追加，避免轮询把表写爆）
func advanceFlow(catDB *storepkg.DB, agentStore *AgentStore, flowID int64) error {
	flow, err := catDB.GetFlow(flowID)
	if err != nil || flow == nil {
		return err
	}
	res, _ := catDB.GetResource(flow.ResourceID)
	abilities := loadAbilities(onboardCfg, integrationsDir)
	run := &flowRun{
		catDB: catDB, agentStore: agentStore,
		flow: flow, resource: res, cfg: onboardCfg, abilities: abilities,
		agentID: flow.AgentID, stepStatus: map[string]string{}, stepWaiting: map[string]string{},
		stepProgress: map[string]any{}, stepAttempts: map[string]any{},
		stepDiags: map[string][]diag{},
	}
	if run.agentID == "" {
		run.agentID = flow.ResourceID
	}

	// hydrate：每步取最近一条事件的状态与停等原因（事件按 id 升序，后写覆盖前写）
	events, err := catDB.ListEvents(flowID)
	if err != nil {
		return err
	}
	for _, e := range events {
		run.stepStatus[e.Step] = e.Status
		run.stepWaiting[e.Step] = waitingForKey(e.Detail)
		if pr := rawFieldOf(e.Detail, "progress"); pr != nil {
			run.stepProgress[e.Step] = pr
		}
		if at := rawFieldOf(e.Detail, "attempts"); at != nil {
			run.stepAttempts[e.Step] = at
		}
		if ds := diagsOf(e.Detail); len(ds) > 0 {
			run.stepDiags[e.Step] = ds
		}
	}

	flags := run.flags()

	for i := range flow.Steps {
		step := flow.Steps[i]
		cur, seen := run.stepStatus[step.ID]
		if seen && (cur == stOK || cur == stSkipped || cur == stFail) {
			// 失败也是终态：必须走显式「重试」清态后才会重跑。
			// 否则任意一次推进（哪怕是重试别的步骤）都会把失败步骤偷偷重放一遍。
			continue
		}

		// 依赖判定先行：when 只在依赖就绪后求值。
		// 反例（曾踩）：when: agent_exists 的步骤若在「装 Agent 之前」被判 false，会落
		// skipped；而 skipped 是终态 —— 等 Agent 真装完、agent_exists 变 true 时，这条
		// 步骤再也不会重跑。装完 Agent 才成立的步骤必须先 pending、后求值。
		depFail, depWait := false, false
		for _, dep := range step.Requires {
			switch run.stepStatus[dep] {
			case stFail, stBlocked:
				depFail = true
			case stOK, stSkipped:
				// 已就绪
			default:
				depWait = true
			}
		}
		whenOK := evalWhen(step.When, flags, flow.Mode)
		// markStep 同时维护「状态 + 停等原因」两个视图变量。
		// 必须同步更新：只在入口 hydrate 一次是不够的——同一轮推进里被重跑的步骤
		// （如探路成功后重新判定 pick_version）其 waiting_for 是新写出来的，
		// 不更新的话下游拿到的还是上一轮的旧语义（曾因此让"等人工选版"显示成"门禁未通过"）
		markStep := func(id, status, waiting string) {
			run.stepStatus[id] = status
			run.stepWaiting[id] = waiting
		}
		skipInapplicable := func() {
			markStep(step.ID, stSkipped, "")
			// 跳过必须说人话（2026-09-21 用户评审：「不适用当前模式与前置状态」
			// 看不出为什么跳过——界面上"安装显示不适用、下一步又在等 Agent 回报"
			// 就是这么来的。把 when 的语义翻译成具体原因）
			_ = appendStepEvent(catDB, flowID, step, stSkipped,
				skipReason(step.When, flags), detailJSON(map[string]any{"when": step.When, "result": "false"}), 0)
		}

		if depFail {
			if !whenOK {
				skipInapplicable()
				continue
			}
			// 「前置依赖没就绪」要区分两种完全不同的处境：
			//   等人工决策（人还没选版本）→ 这不是异常，文案不能吓人
			//   前置失败/真被门禁挡住   → 这才是需要排查的阻断
			depWaiting := ""
			for _, dep := range step.Requires {
				if run.stepStatus[dep] == stBlocked && run.stepWaiting[dep] != "" {
					depWaiting = run.stepWaiting[dep]
					break
				}
			}
			markStep(step.ID, stBlocked, depWaiting)
			// 文案必须对接入/卸载都成立（2026-09-22 用户评审）：
			// 「不生成配置、不下发」是安装侧术语，出现在卸载流水线里让人看不懂
			summary := "前置环节未通过，本环节暂不执行"
			if depWaiting != "" {
				summary = "前置环节" + waitingLabel(depWaiting) + "，本环节暂不执行"
			}
			_ = appendStepEvent(catDB, flowID, step, stBlocked,
				summary, detailJSON(map[string]any{
					"blocked_by":  step.Requires,
					"gate":        step.Gate,
					"waiting_for": depWaiting,
				}), 0)
			continue
		}
		if depWait {
			markStep(step.ID, stPending, "")
			_ = appendStepEvent(catDB, flowID, step, stPending, "等待前序环节完成",
				detailJSON(map[string]any{"pending_deps": run.pendingDeps(step)}), 0)
			continue
		}
		if !whenOK {
			skipInapplicable()
			continue
		}

		// 依赖就绪：平台侧当场执行，其它下发后等待
		if step.Scope != "platform" {
			// 平台 ansible 执行器优先接管（探路/安装：凭据齐备 + ansible 可用）——
			// 步骤落 running、异步作业拉起后自动回报；凭据缺失或 ansible 不可用时
			// 回落旧的「执行包 + 人工回报」路径
			if tryAnsibleExecutor(catDB, agentStore, run, step) {
				continue
			}
			run.stepStatus[step.ID] = stRunning
			run.stepWaiting[step.ID] = ""
			detail := map[string]any{
				"scope": step.Scope,
				"atom":  step.Atom,
				"hint":  step.Hint,
			}
			// 为什么没走平台自动执行，必须给人看见（凭据缺失 / 环境缺 ansible）。
			// 已有诊断（端口来源可疑、超时自动重试…）先原样保留再追加，不做替换：
			// 界面上"平台救过一次"的信息不该被一条 fallback 提示冲掉
			dsList := append([]diag{}, run.stepDiags[step.ID]...)
			if why := ansibleFallbackReason(run.resource); why != "" {
				detail["ansible_fallback"] = why
				detail["cred_state"] = credState(sshCredOf(run.resource))
				dsList = append(dsList, warnDiag("ansible_fallback", why,
					"在资源台账补齐 SSH 凭据后，重试本环节即可改为平台自动执行"))
			}
			if len(dsList) > 0 {
				detail["diagnosis"] = diagJSON(dsList)
			}
			// 执行包：实时取当前模板（模板改命令即时生效），快照字段兜底。
			// {{.ip}} 渲染为目标资源 IP；前端对带 instruction 的 running 步骤
			// 渲染可复制命令 + 结构化回报表单
			insTpl := step.Instruction
			if tpl := templateInstructionFor(flow.Mode, step.ID); tpl != "" {
				insTpl = tpl
			}
			if ins := renderInstruction(insTpl, flow.ResourceIP, credPortOrDefault(run.resource)); ins != "" {
				detail["instruction"] = ins
			}
			// 沿用已有的运行期元数据（超时重试后重新下发执行包时，attempt 计数与
			// 超时基准必须延续，不能被重置成"第一次尝试"）
			if pr, ok := run.stepProgress[step.ID]; ok && pr != nil {
				detail["progress"] = pr
			}
			if at, ok := run.stepAttempts[step.ID]; ok && at != nil {
				detail["attempts"] = at
			}
			_ = appendStepEvent(catDB, flowID, step, stRunning,
				"已下发，等待 "+scopeLabel(step.Scope)+" 回报", detailJSON(detail), 0)
			// 不 return：依赖它的步骤自然落 pending，而与它无关的并行分支（混合场景的
			// 边缘/远程两支）仍应继续推进；收口状态由循环末尾统一计算。
			// 早期实现在这里直接 return，导致流水线状态停在旧值（步骤已 running、流程还显示 failed）。
			continue
		}

		t0 := time.Now()
		status, summary, detail := runPlatformAtom(catDB, agentStore, run, step)
		ms := time.Since(t0).Milliseconds()
		run.stepStatus[step.ID] = status
		// 平台原子能力的停等语义就写在它返回的 detail 里（pick_version 的 waiting_for），
		// 取出来同步给同一轮的下游判定，避免"等人工"被降级成"门禁未通过"
		run.stepWaiting[step.ID] = waitingForKey(detail)
		_ = appendStepEvent(catDB, flowID, step, status, summary, detail, ms)
		if status == stFail {
			// 失败即终态；本次循环继续走完，让下游落 blocked，收口时统一写流程状态
			continue
		}
	}

	// 全部步骤终态 → 收口
	final := computeFlowStatus(flow.Steps, run.stepStatus)
	return catDB.UpdateFlowStep(flowID, currentStepOf(flow.Steps, run.stepStatus), final)
}

// renderInstruction 渲染执行包模板：{{.ip}} → 目标资源 IP，{{.port}} → SSH 端口。
// 端口必须一起渲染——人工路径下 `ssh user@ip` 会默认走 22，正是"连错端口"的根源。
// 刻意不用 text/template——探测命令里的 awk/sed 语法与模板引擎转义互相干扰，
// 占位符替换足够且可预测
func renderInstruction(tpl, ip string, port int) string {
	if tpl == "" {
		return ""
	}
	out := strings.ReplaceAll(tpl, "{{.ip}}", ip)
	if port > 0 {
		return strings.ReplaceAll(out, "{{.port}}", fmt.Sprint(port))
	}
	return out
}

// credPortOrDefault 资源台账登记的 SSH 端口；未登记时按平台惯例回落 22
func credPortOrDefault(res *storepkg.Resource) int {
	if res == nil || res.SSHPort <= 0 {
		return 22
	}
	return res.SSHPort
}

// waitingForKey 取步骤 detail 里的 waiting_for（停等语义字段，空串=不是停等态）
func waitingForKey(detail string) string {
	var d struct {
		WaitingFor string `json:"waiting_for"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return ""
	}
	return d.WaitingFor
}

// waitingLabel 停等语义人话化：waiting_for 是机器字段，界面不该直接看它
func waitingLabel(k string) string {
	switch k {
	case "human_pick":
		return "待人工选定 SAgent 版本"
	case "probe":
		return "待主机探路实测结果"
	case "human_confirm_upgrade":
		return "待人工确认升级"
	case "human_confirm_service":
		return "待人工确认启停操作"
	}
	return "待人工确认"
}

// currentStepOf 取「当前卡在哪一步」：优先在跑的，其次还没开始的，再次失败/被阻断的
func currentStepOf(steps []storepkg.FlowStepSnapshot, st map[string]string) string {
	for _, s := range steps {
		if st[s.ID] == stRunning {
			return s.ID
		}
	}
	for _, s := range steps {
		if v := st[s.ID]; v == stPending || v == "" {
			return s.ID
		}
	}
	for _, s := range steps {
		if v := st[s.ID]; v == stFail || v == stBlocked {
			return s.ID
		}
	}
	if len(steps) > 0 {
		return steps[len(steps)-1].ID
	}
	return ""
}

// flags 计算 when 求值用的变量表
func (r *flowRun) flags() map[string]bool {
	agentExists := false
	if r.agentID != "" {
		if r.agentStore != nil && r.agentStore.Get(r.agentID) != nil {
			agentExists = true
		} else if r.catDB != nil {
			if src, _ := r.catDB.GetAgentSource(r.agentID); src != "" {
				agentExists = true
			}
		}
	}
	return map[string]bool{
		"agent_exists":              agentExists,
		"target_agent_exists":       agentExists,
		"abilities_requires_params": abilitiesRequireParams(r.abilities, r.flow.Abilities),
		"preflight_passed":          r.stepStatus["preflight_host"] == stOK,
	}
}

// pendingDeps 列出还没就绪的依赖步骤
func (r *flowRun) pendingDeps(step storepkg.FlowStepSnapshot) []string {
	out := []string{}
	for _, dep := range step.Requires {
		if d := r.stepStatus[dep]; d != stOK && d != stSkipped {
			out = append(out, dep)
		}
	}
	return out
}

// computeFlowStatus 汇总流水线状态
func computeFlowStatus(steps []storepkg.FlowStepSnapshot, st map[string]string) string {
	allDone := true
	for _, s := range steps {
		switch st[s.ID] {
		case stFail:
			return "failed"
		case stBlocked:
			return "blocked"
		case stOK, stSkipped:
			// 已终态
		default:
			allDone = false
		}
	}
	if allDone {
		return "done"
	}
	return "running"
}

// appendStepEvent 追加步骤事件；同一步的最新状态未变化时不重复写（幂等，防轮询写爆）
func appendStepEvent(catDB *storepkg.DB, flowID int64, step storepkg.FlowStepSnapshot,
	status, summary, detail string, ms int64) error {
	if last, _ := catDB.LatestEvent(flowID, step.ID); last != nil {
		if last.Status == status && last.Summary == summary {
			return nil
		}
		// 耗时回填：终态事件且前一状态是 running 时，从 running 的 started_at 起算。
		// external/agent 步骤的回报路径（report/force/agent-report）没有 t0 可传，
		// 不在这里算的话这类步骤的 duration_ms 永远是 0，前端无耗时可见
		if ms == 0 && isTerminalStatus(status) && last.Status == stRunning {
			if t0, err := time.ParseInLocation("2006-01-02 15:04:05", last.StartedAt, time.Local); err == nil {
				if d := time.Since(t0).Milliseconds(); d > 0 {
					ms = d
				}
			}
		}
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	ev := &storepkg.FlowEvent{
		FlowID: flowID, Step: step.ID, Title: step.Title, Status: status,
		Scope: step.Scope, Summary: summary, Detail: detail, DurationMs: ms,
		StartedAt: now, FinishedAt: now,
	}
	_, err := catDB.AppendEvent(ev)
	return err
}

// isTerminalStatus 终态判定：终态事件才携带耗时（running/pending 是中间态）
func isTerminalStatus(s string) bool {
	switch s {
	case stOK, stFail, stBlocked, stSkipped:
		return true
	}
	return false
}

func scopeLabel(s string) string {
	switch s {
	case "agent":
		return "Agent"
	case "external":
		return "外部执行器"
	default:
		return "平台"
	}
}

func detailJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ===================================================================
//  平台侧原子能力实现
//  只有 scope=platform 的步骤会走到这里；agent/external 步骤由下发+回报驱动
// ===================================================================

// runPlatformAtom 平台侧原子能力分发（只有 scope=platform 的步骤会走到这里）
func runPlatformAtom(catDB *storepkg.DB, agentStore *AgentStore,
	r *flowRun, step storepkg.FlowStepSnapshot) (status, summary, detail string) {
	switch step.Atom {
	case "pick_object":
		return r.atomPickObject(catDB)
	case "pick_agent":
		return r.atomPickAgent(catDB, agentStore)
	case "pick_version":
		return r.atomPickVersion(catDB)
	case "confirm_uninstall":
		return r.atomConfirmUninstall(catDB)
	case "confirm_upgrade":
		return r.atomConfirmUpgrade(catDB)
	case "confirm_service":
		return r.atomConfirmService(catDB)
	case "cleanup_platform":
		return r.atomCleanupPlatform(catDB, agentStore)
	case "pick_ability":
		return r.atomPickAbility()
	case "collect_params":
		return r.atomCollectParams(catDB)
	case "register_platform_device":
		return r.atomRegisterPlatformDevice(catDB, agentStore)
	case "sync_config":
		return r.atomSyncConfig(catDB, agentStore)
	default:
		return stFail, "未实现的原子能力：" + step.Atom,
			detailJSON(map[string]any{"atom": step.Atom, "scope": step.Scope})
	}
}

// pick_object 资源对象就绪（创建时已保证存在，这里做确认与回填）
func (r *flowRun) atomPickObject(catDB *storepkg.DB) (string, string, string) {
	if r.resource == nil {
		return stFail, "资源对象不存在", detailJSON(map[string]any{"resource_id": r.flow.ResourceID})
	}
	known := r.resource.OS != "" && r.resource.Arch != ""
	summary := "资源对象已就绪：" + r.resource.ID
	if !known {
		summary += "（OS/架构待主机探路实测回填）"
	}
	// 第一步就把"能不能自动执行、端口是不是核实过"讲清楚：
	// 免得到最后一步才发现凭据缺失/端口来源可疑，还要人工倒推原因
	cred := sshCredOf(r.resource)
	ds := []diag{}
	if st := credState(cred); st["valid"] == false {
		reason, _ := st["reason"].(string)
		ds = append(ds, warnDiag("cred_incomplete", reason,
			"不补凭据时，主机探路 / 安装 SAgent 将回落「人工复制执行包 + 回报结果」模式"))
	} else if r.resource.SSHPort <= 0 {
		ds = append(ds, portSourceDiag(cred))
		summary += " ⚠ 端口未登记"
	}
	return stOK, summary, detailJSON(map[string]any{
		"resource_id": r.resource.ID,
		"ip":          r.resource.IP,
		"os":          r.resource.OS,
		"arch":        r.resource.Arch,
		"role":        r.resource.Role,
		"source":      r.resource.Source,
		"os_known":    known,
		"cred_state":  credState(cred),
		"diagnosis":   diagJSON(ds),
	})
}

// pick_agent 确定承载采集能力的机器。
//   边缘模式：承载机就是资源自身（本机采集）——维持原语义。
//   远程/混合（R2）：承载机是"采集机"。已选定（flow.agent_id 确为采集机）→ 直接采用；
//     否则从池内健康采集机中自动挑负载最低者（如实记录 source=auto_pick，可在④闸门改选）；
//     池内无健康采集机 → 回落资源自身并给出诊断，不假装知道。
func (r *flowRun) atomPickAgent(catDB *storepkg.DB, agentStore *AgentStore) (string, string, string) {
	if !isCollectorMode(r.flow.Mode) {
		agentID := r.agentID
		if agentID == "" {
			agentID = r.flow.ResourceID
		}
		_ = catDB.UpdateFlowAgent(r.flow.ID, agentID)

		a := agentStore.Get(agentID)
		summary := "承载机：" + agentID
		d := map[string]any{"agent_id": agentID, "registered": a != nil, "mode": r.flow.Mode}
		if a != nil {
			summary += "（已接入，复用）"
			d["version"] = a.Version
			d["status"] = a.Status
		} else {
			summary += "（尚未接入，进入安装环节）"
		}
		return stOK, summary, detailJSON(d)
	}
	return r.pickCollectorAgent(catDB, agentStore)
}

// pickCollectorAgent 远程/混合模式的承载采集机判定（R2）。
// 候选池视图随 detail 下发（pools），前端选机卡据此渲染——候选清单不在前端写死。
func (r *flowRun) pickCollectorAgent(catDB *storepkg.DB, agentStore *AgentStore) (string, string, string) {
	tenant := ""
	if res, _ := catDB.GetResource(r.flow.ResourceID); res != nil {
		tenant = res.TenantID
	}
	views := collectorPoolViews(catDB, tenant)

	// 已选定（人工改选 / 创建时指定）：复核身份——仍是采集机才认，否则当未选定重挑
	if a, load, healthy := collectorFactOf(catDB, r.agentID); a != nil && isCollectorAgent(a) {
		pool := agentPool(a)
		_ = catDB.UpdateFlowAgent(r.flow.ID, a.ID)
		ds := []diag{}
		if !healthy {
			ds = append(ds, warnDiag("collector_stale",
				"承载采集机 "+a.ID+" 心跳陈旧", "可在④闸门改选池内其它健康采集机"))
		}
		return stOK, "承载采集机：" + a.ID + "（池 " + poolOrDash(pool) + "，承载 " + strconv.Itoa(load) + " 个远端采集目标）",
			detailJSON(map[string]any{
				"agent_id": a.ID, "pool": pool, "load": load, "healthy": healthy,
				"registered": agentStore != nil && agentStore.Get(a.ID) != nil,
				"source":     "selected", "mode": r.flow.Mode, "pools": views,
				"diagnosis": diagJSON(ds),
			})
	}

	rec, pool, reason := autoPickCollector(catDB, tenant)
	if rec == "" {
		// 池内无健康采集机：如实回落资源自身，不假装知道——并说明这样做的代价
		_ = catDB.UpdateFlowAgent(r.flow.ID, r.flow.ResourceID)
		return stOK, "未登记可用采集机，暂按资源自身承载：" + r.flow.ResourceID,
			detailJSON(map[string]any{
				"agent_id": r.flow.ResourceID, "source": "fallback_self", "mode": r.flow.Mode,
				"pools": views,
				"diagnosis": diagJSON([]diag{warnDiag("no_collector",
					"租户内未登记健康采集机（type=proxy 或 labels.role=collector）",
					"远端采集应承载在采集机上：登记采集机后可在本环节改选；按资源自身承载时无法享受故障漂移")}),
			})
	}
	markRecommended(views, rec, reason)
	_, load, healthy := collectorFactOf(catDB, rec)
	_ = catDB.UpdateFlowAgent(r.flow.ID, rec)
	return stOK, "承载采集机（自动挑机）：" + rec + "（池 " + poolOrDash(pool) + "，承载 " + strconv.Itoa(load) + " 个远端采集目标）",
		detailJSON(map[string]any{
			"agent_id": rec, "pool": pool, "load": load, "healthy": healthy,
			"registered": agentStore != nil && agentStore.Get(rec) != nil,
			"source":     "auto_pick", "reason": reason, "mode": r.flow.Mode, "pools": views,
		})
}

// pick_version 按主机探路实测环境自动匹配版本。
// 2026-09-22 起不再停等人工：有兼容候选即选定并落 ok（detail.tag 供安装作业取值），
// 无候选才落 fail 并给出"差哪一项"；人工可用步骤 API 的「指定版本（需理由）」覆盖
func (r *flowRun) atomPickVersion(catDB *storepkg.DB) (string, string, string) {
	res := r.resource
	if res == nil {
		return stFail, "资源对象缺失，无法定版本", "{}"
	}
	osName, arch := strings.ToLower(res.OS), strings.ToLower(res.Arch)
	verSrc := "resource_ledger"
	if r.flow.Mode == "upgrade" {
		// 升级流的权威判据是目标机二进制本身：预检 sha 反查版本清单得到真实 os/arch，
		// 不信任资源台账——台账可能过期或被误回报污染（2026-09-22 真机验收实测踩到：
		// 一次 executor_report 把 arm64 覆盖成 x86_64，候选清单随之全错）
		if pre := svcLatestPreflightUpgrade(catDB, r.flow.ID); pre != nil {
			if v := versionBySHA(pre.SHA); v != nil {
				osName, arch = strings.ToLower(v.OS), strings.ToLower(v.Arch)
				verSrc = "target_binary_sha"
			}
		}
	}
	if osName == "" || arch == "" {
		// 不猜版本：探路实测落台账后再推进本步骤（人工回报探路结果后重跑）
		return stBlocked, "等待主机探路实测 OS/架构", detailJSON(map[string]any{
			"waiting_for": "probe",
			"note":        "版本必须基于实测环境匹配，不使用 latest 兜底",
		})
	}
	candidates := compatibleVersions(osName, arch)
	env := targetEnvOf(res)
	env.Arch = normArch(arch)
	chosen, reason, ds := selectVersion(candidates, env)
	if chosen == nil {
		return stFail, "没有与目标机环境兼容的 SAgent 版本", detailJSON(map[string]any{
			"os": osName, "arch": arch, "version_source": verSrc, "env": env,
			"candidates": len(candidates), "diagnosis": diagJSON(ds),
		})
	}
	return stOK, reason, detailJSON(map[string]any{
		"tag": chosen.Tag, "version": chosen.Version, "source": "auto_match",
		"version_source": verSrc, "env": env, "reason": reason,
		"diagnosis": diagJSON(ds),
	})
}

// pick_ability 记录勾选能力；locked 的能力强制补入（地基不可关）
// 补入结果回写流水线，保证后续 collect_params / register_platform_device 看到的是同一份清单
func (r *flowRun) atomPickAbility() (string, string, string) {
	orig := append([]string{}, r.flow.Abilities...)
	selected := append([]string{}, orig...)
	forced := []string{}
	for _, a := range r.abilities {
		if a.Locked && !containsStr(selected, a.ID) {
			selected = append(selected, a.ID)
			forced = append(forced, a.ID)
		}
	}
	if len(forced) > 0 {
		_ = r.catDB.UpdateFlowAbilities(r.flow.ID, selected)
		r.flow.Abilities = selected
	}
	items := []map[string]any{}
	for _, id := range selected {
		a := abilityByID(r.abilities, id)
		if a == nil {
			items = append(items, map[string]any{"id": id, "known": false})
			continue
		}
		items = append(items, map[string]any{
			"id": a.ID, "name": a.Name, "scope": a.Scope,
			"locked": a.Locked, "has_params": a.HasParams,
		})
	}
	summary := fmt.Sprintf("已选 %d 项采集能力", len(selected))
	if len(forced) > 0 {
		summary += "（强制补入地基能力：" + strings.Join(forced, ", ") + "）"
	}
	return stOK, summary, detailJSON(map[string]any{"abilities": items, "forced_locked": forced})
}

// collect_params 落参数并建采集目标
func (r *flowRun) atomCollectParams(catDB *storepkg.DB) (string, string, string) {
	params := r.flow.Params()
	need := []*Ability{}
	for _, id := range r.flow.Abilities {
		if a := abilityByID(r.abilities, id); a != nil && a.HasParams {
			need = append(need, a)
		}
	}

	missing := []map[string]any{}
	created := []string{}
	for _, a := range need {
		vals, ok := params[a.ID]
		if !ok {
			vals = map[string]string{}
		}
		miss := []string{}
		for _, f := range a.Params {
			if f.Required && strings.TrimSpace(vals[f.Name]) == "" {
				miss = append(miss, f.Name)
			}
		}
		if len(miss) > 0 {
			missing = append(missing, map[string]any{"ability": a.ID, "fields": miss})
			continue
		}
		// 参数齐全 → 建/复用采集目标（按「资源 + 能力」命名，幂等）
		name := r.flow.ResourceID + "-" + strings.TrimSuffix(strings.TrimSuffix(a.ID, "_probe"), "_exporter")
		valsJSON := "{}"
		if b, err := json.Marshal(vals); err == nil {
			valsJSON = string(b)
		}
		addr := firstNonEmpty(vals["address"], vals["url"], r.resource.IP)
		t := &storepkg.TargetRow{
			Name: name, Type: a.ID, Address: addr, AgentID: r.agentID,
			Plugin: a.ID, Note: "由接入流水线创建", ParamsJSON: valsJSON,
			ResourceID: r.flow.ResourceID, FlowID: r.flow.ID,
		}
		if old, _ := catDB.FindTargetByName(name); old != nil {
			t.ID = old.ID
			_ = catDB.UpdateTarget(t)
			created = append(created, name+"（更新）")
		} else if _, err := catDB.InsertTarget(t); err == nil {
			created = append(created, name)
		}
	}

	if len(missing) > 0 {
		return stRunning, fmt.Sprintf("待填参数：%d 项能力", len(missing)),
			detailJSON(map[string]any{
				"missing":        missing,
				"abilities":      abilitiesBrief(need),
				"note":           "在接入向导里补齐必填参数后重新推进；字段定义来自插件包 params.yaml",
				"created_so_far": created,
			})
	}
	if len(created) == 0 {
		// 选中的能力都无需参数：这一步没有实际工作，不算失败
		return stOK, "所选能力无需参数表单", detailJSON(map[string]any{"abilities": abilitiesBrief(need)})
	}
	return stOK, "已落参数并创建采集目标：" + strings.Join(created, ", "),
		detailJSON(map[string]any{"targets": created, "params": params})
}

// register_platform_device 代理机/承载机登记为平台设备（强制带主机自监控）
func (r *flowRun) atomRegisterPlatformDevice(catDB *storepkg.DB, agentStore *AgentStore) (string, string, string) {
	if r.resource == nil {
		return stFail, "资源对象缺失", "{}"
	}
	if err := catDB.UpdateResourceRole(r.resource.ID, "platform_device"); err != nil {
		return stFail, "登记平台设备失败：" + err.Error(), "{}"
	}
	// 地基能力强制开启（红线：承载平台能力的机器必须自带主机自监控）。
	// 「哪些能力是地基」由 onboard_config.json 的 locked 声明决定——引擎里不写能力名。
	abilities := append([]string{}, r.flow.Abilities...)
	forced := []string{}
	for _, a := range r.abilities {
		if a.Locked && !containsStr(abilities, a.ID) {
			abilities = append(abilities, a.ID)
			forced = append(forced, a.ID)
		}
	}
	if len(forced) > 0 {
		_ = catDB.UpdateFlowAbilities(r.flow.ID, abilities)
		r.flow.Abilities = abilities
	}
	return stOK, "已登记为采集平台设备（主机自监控强制开启）", detailJSON(map[string]any{
		"resource_id":         r.resource.ID,
		"role":                "platform_device",
		"host_metrics_forced": len(forced) > 0,
		"forced_locked":       forced,
		"abilities":           abilities,
	})
}

// sync_config 渲染配置文档并下发（版本 +1）
func (r *flowRun) atomSyncConfig(catDB *storepkg.DB, agentStore *AgentStore) (string, string, string) {
	agentID := r.agentID
	if agentID == "" {
		agentID = r.flow.ResourceID
	}
	syncAgentConfig(agentStore, catDB, agentID)
	ver, content, err := catDB.GetAgentConfig(agentID)
	if err != nil {
		return stFail, "读取下发配置失败：" + err.Error(), "{}"
	}
	rendered := renderAgentConfigSection(catDB, agentID)
	out, _ := json.MarshalIndent(map[string]any{
		"platform_doc": json.RawMessage(content),
		"rendered":     rendered,
	}, "", "  ")
	// 措辞如实（2026-09-21 用户问询）：这一步不连目标机，只写平台侧期望配置轨道——
	// Agent 装好注册后凭心跳比对自动生效；"生效与否"由 verify_probe/observe_collect 实测
	return stOK, fmt.Sprintf("期望配置已生成（cfg-%d）——Agent 装好注册后自动生效", ver), string(out)
}

// cleanup_platform 卸载的平台侧收口：把该资源在平台上的**接入痕迹**清干净。
//
// 语义（用户确认的"全清"）：注销 Agent 台账（内存 + 目录库 + 期望配置轨道）→
// 删除 vmagent file_sd 抓取登记 → 删除该资源的采集目标。
//
// 刻意保留：资源对象本身与其 SSH 凭据。卸载是「采集接入」的逆操作，不是删 CMDB 记录；
// 凭据保留后可以立刻原样重新起一条接入流水线——这是"能闭环重测"的前提。
//
// 每一项都「删后复查」：卸载在目标机侧必须自证（playbook 断言），平台侧同样不能
// 只说"我删了"——不复查就无从区分"真删掉"和"删除语句静默失败"。
func (r *flowRun) atomCleanupPlatform(catDB *storepkg.DB, agentStore *AgentStore) (string, string, string) {
	if r.resource == nil {
		return stFail, "资源对象缺失，无法清理平台登记", detailJSON(map[string]any{"resource_id": r.flow.ResourceID})
	}
	resID := r.flow.ResourceID
	agentID := r.agentID
	if agentID == "" {
		agentID = resID
	}
	ds := []diag{}
	done := []map[string]any{}

	// ① Agent 台账：内存 + 目录库，删后复查（复查看库，不看返回值）
	inMemBefore := agentStore.Get(agentID) != nil
	agentStore.Remove(agentID)
	dbDeleted, err := catDB.DeleteAgent(agentID)
	if err != nil {
		return stFail, "注销 Agent 台账失败：" + err.Error(), detailJSON(map[string]any{"agent_id": agentID})
	}
	if src, _ := catDB.GetAgentSource(agentID); src != "" {
		ds = append(ds, fatalDiag("agent_row_remains",
			"Agent 台账行未能删除（复查仍在库中）："+agentID, "检查目录库（PostgreSQL）连接与写入是否正常"))
	}
	done = append(done, map[string]any{
		"item": "agent_ledger", "agent_id": agentID,
		"in_memory_before": inMemBefore, "db_row_deleted": dbDeleted,
		"verified_absent": true,
	})

	// ② 期望配置轨道：只删 agents 行会留下悬挂的期望配置，重装时 sync_config
	// 因"内容没变"不再版本 +1，界面会看到"下发成功但版本没动"
	cfgDeleted, err := catDB.DeleteAgentConfig(agentID)
	if err != nil {
		ds = append(ds, warnDiag("agent_config_delete_failed",
			"删除期望配置失败："+err.Error(),
			"不影响本次卸载的目标机结果；但重装后配置版本号不会从 1 重新开始"))
	}
	done = append(done, map[string]any{"item": "agent_config", "deleted": cfgDeleted})

	// ③ vmagent file_sd 抓取登记：不撤则 vmagent 会一直抓一台已经卸干净的机器
	sdPath, sdErr := unregisterScrapeTarget(resID)
	if sdErr != nil {
		ds = append(ds, warnDiag("scrape_sd_remove_failed",
			"删除 vmagent 抓取登记失败："+sdErr.Error(),
			"人工删除 "+sdPath+"；不删则该目标会一直处于 down 状态"))
	}
	done = append(done, map[string]any{"item": "scrape_sd", "path": sdPath, "removed": sdErr == nil})

	// ④ 该资源的采集目标：只按 resource_id 匹配——
	// 按 agent_id 删会误伤共用承载机（proxy/remote 模式下同一 Agent 上有别的资源的采集目标）
	targets, err := catDB.ListTargets()
	if err != nil {
		ds = append(ds, warnDiag("list_targets_failed", "读取采集目标失败："+err.Error(), ""))
	}
	removed, keptOthers := []string{}, 0
	for _, t := range targets {
		if t.ResourceID != resID {
			if t.AgentID == agentID {
				keptOthers++
			}
			continue
		}
		if derr := catDB.DeleteTarget(t.ID); derr != nil {
			ds = append(ds, warnDiag("target_delete_failed",
				"删除采集目标失败："+t.Name+"："+derr.Error(), "在「采集目标」页人工删除该条"))
			continue
		}
		removed = append(removed, t.Name)
	}
	done = append(done, map[string]any{"item": "collect_targets", "removed": removed, "kept_other_resources": keptOthers})

	// ④' 启停维护态清空：卸载后资源回到常规语义（不存在"已卸载但还标着维护中"）
	if err := catDB.SetResourceSvcState(resID, ""); err != nil {
		ds = append(ds, warnDiag("svc_state_clear_failed",
			"清空启停维护态失败："+err.Error(), "资源已卸载，该标记仅影响界面显示；可在资源列表核实"))
	}
	// ④'' 安装 tag 清空：机器上已经没有 SAgent 了，台账再挂着 tag 会让资源页
	// 显示"已装 0.4.1"——卸载是"这台机没东西"，不是"装了但没跑"
	if err := catDB.SetResourceInstalledTag(resID, ""); err != nil {
		ds = append(ds, warnDiag("installed_tag_clear_failed",
			"清空安装 tag 失败："+err.Error(), "资源页可能仍显示旧版本号；重新接入时会覆盖"))
	}

	// ⑤ 前置提醒：目标机侧若不是平台 ansible 实测成功（人工回报 / 强制通过），
	// 卸载没真发生时 Agent 会在下一个心跳周期重新注册，平台会冒出一台幽灵 Agent。
	// 这是"清完台账却又自己长回来"的唯一原因，必须提前说清，别让人以为是平台乱写数据
	if ev, _ := catDB.LatestEvent(r.flow.ID, "uninstall_agent"); ev != nil {
		var d struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal([]byte(ev.Detail), &d)
		if d.Source != "ansible_uninstall" {
			ds = append(ds, warnDiag("uninstall_not_verified",
				"目标机侧卸载不是由平台 ansible 实测确认的（人工回报或强制通过）",
				"若目标机上 SAgent 进程仍在运行，它会在 ≤30s 内重新心跳注册，平台将再次出现该 Agent"))
		}
	}

	for _, d := range ds {
		if d.Level == "fatal" {
			return stFail, "平台登记清理未完成：" + d.Message, detailJSON(map[string]any{
				"resource_id": resID, "agent_id": agentID, "cleaned": done, "diagnosis": diagJSON(ds),
			})
		}
	}

	summary := "已注销 Agent 台账与期望配置，撤除抓取登记，删除 " + fmt.Sprint(len(removed)) + " 个采集目标"
	if keptOthers > 0 {
		summary += fmt.Sprintf("（保留 %d 个归属其他资源的采集目标）", keptOthers)
	}
	summary += "；资源对象与 SSH 凭据保留，可直接重新接入"
	if n := countWarn(ds); n > 0 {
		summary += fmt.Sprintf(" ⚠ %d 项待复核（见步骤告警）", n)
	}
	addAudit("注销平台登记（卸载）", resID, "接入中心", summary)
	return stOK, summary, detailJSON(map[string]any{
		"resource_id": resID, "agent_id": agentID,
		"cleaned": done, "diagnosis": diagJSON(ds),
		"kept": map[string]any{
			"resource_object": true, "ssh_credentials": true,
			"note": "资源对象与凭据保留是刻意设计：卸载后可原样重新起接入流水线",
		},
	})
}

// ---------- 小工具 ----------

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func abilitiesBrief(list []*Ability) []map[string]any {
	out := []map[string]any{}
	for _, a := range list {
		fields := make([]string, 0, len(a.Params))
		for _, f := range a.Params {
			fields = append(fields, f.Name)
		}
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "params_path": a.ParamsPath, "fields": fields,
		})
	}
	return out
}
