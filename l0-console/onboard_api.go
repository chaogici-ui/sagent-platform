package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  接入中心 API
//  设计依据：PLAN-采集接入中心与流程引擎.md §5.2
//
//  路由一律挂在这里，不再往 agent_api / misc_api 堆。
//  前端零写死：模板、能力清单、参数表单字段、检查项全部由这些端点下发。
//
//  批次说明：探路包相关三条（/preflight/spec、/preflight/package、/preflight/report）
//  随批 D 落地——它们要读插件包内的 checks.yaml，而 checks.yaml 是批 D 的产物。
//  在此之前不注册占位端点（避免出现「有接口没实现」的假完成）。
// ===================================================================

// registerOnboardRoutes 注册接入中心 API
func registerOnboardRoutes(mux *http.ServeMux, agentStore *AgentStore, catDB *storepkg.DB) {
	// ---- 模板与能力：接入向导的数据来源（前端零写死）----
	mux.HandleFunc("/api/onboard/templates", func(w http.ResponseWriter, r *http.Request) {
		// 向导只应该看到"接入类"模式；卸载（offboard）是既有接入的逆操作，
		// 由资源行内按钮拉起，不出现在模式单选里（见 FlowTemplate.Internal 注释）
		tpls := flowTemplateListForWizard()
		abilities := loadAbilities(onboardCfg, integrationsDir)
		writeJSON(w, map[string]interface{}{
			"templates": tpls,
			"abilities": abilities,
		})
	})

	// ---- 能力清单（单独端点，供「采集目标」编辑等场景复用）----
	mux.HandleFunc("/api/onboard/abilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"abilities": loadAbilities(onboardCfg, integrationsDir),
		})
	})

	// ---- 流水线列表 + 顶部统计 ----
	mux.HandleFunc("/api/onboard/flows", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		flows, err := catDB.ListFlows(status, 200)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		views := make([]map[string]interface{}, 0, len(flows))
		summary := map[string]int{"total": 0, "running": 0, "done": 0, "failed": 0, "blocked": 0, "canceled": 0, "stalled": 0}
		for _, f := range flows {
			v := flowView(catDB, agentStore, f)
			views = append(views, v)
			summary["total"]++
			if s, _ := v["status"].(string); s != "" {
				summary[s]++
			}
			if st, _ := v["stall_sec"].(int64); st > 0 {
				summary["stalled"]++
			}
		}
		writeJSON(w, map[string]interface{}{"summary": summary, "flows": views})
	})

	// ---- 单条流水线 + 全部步骤事件（每步取最新一条，时间线不重复）----
	mux.HandleFunc("/api/onboard/flow", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleOnboardFlowCreate(w, r, agentStore, catDB)
			return
		}
		id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if id <= 0 {
			writeJSON(w, map[string]interface{}{"error": "id required"})
			return
		}
		f, err := catDB.GetFlow(id)
		if err != nil || f == nil {
			writeJSON(w, map[string]interface{}{"error": "flow not found"})
			return
		}
		events, _ := catDB.ListEvents(id)
		v := flowView(catDB, agentStore, f)
		v["events"] = timelineOf(f, events)
		writeJSON(w, map[string]interface{}{"flow": v, "events": v["events"], "raw_event_count": len(events)})
	})

	// ---- 推进/重试/强制通过某一步 ----
	mux.HandleFunc("/api/onboard/flow/step", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID     int64  `json:"flow_id"`
			Step       string `json:"step"`
			Action     string `json:"action"` // retry | force | skip
			Reason     string `json:"reason"`
			VersionTag string `json:"version_tag"` // force 时指定版本（仅 pick_version，需理由）
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		f, err := catDB.GetFlow(req.FlowID)
		if err != nil || f == nil {
			writeJSON(w, map[string]interface{}{"error": "flow not found"})
			return
		}
		var step storepkg.FlowStepSnapshot
		found := false
		for _, s := range f.Steps {
			if s.ID == req.Step {
				step, found = s, true
				break
			}
		}
		if !found {
			writeJSON(w, map[string]interface{}{"error": "step not found: " + req.Step})
			return
		}
		prevStatus := stPending
		wf := ""
		if last, _ := catDB.LatestEvent(req.FlowID, req.Step); last != nil {
			prevStatus = last.Status
			// waiting_for 非空 = 该环节在停等（等人工决策/等上游实测），
			// 没有任何"可跳过的工作"。对停等环节 force 会凭空造一个 OK
			// （曾可借此绕过卸载确认门禁），retry 也只会原样落回停等——都拒绝并说明去处
			wf = waitingForKey(last.Detail)
		}

		switch req.Action {
		case "report":
			// 执行器/Agent 带证据回报：与强制通过在审计上区分（source=executor_report ≠ forced）
			if strings.TrimSpace(req.Reason) == "" {
				writeJSON(w, map[string]interface{}{"error": "回报必须附执行结果/证据说明（将写入审计）"})
				return
			}
			_ = appendStepEvent(catDB, req.FlowID, step, stOK,
				"执行器回报："+req.Reason,
				detailJSON(map[string]any{"source": "executor_report", "reason": req.Reason, "operator": cfgAuditOperator}),
				durationSinceLast(catDB, req.FlowID, req.Step, stRunning))
			addAudit("执行器回报接入环节", req.Step, "接入中心", req.Reason)
		case "force":
			// 指定版本例外通道（pick_version 专用）：自动匹配之外的人工决定，留痕到人。
			// tag 必须在清单里（防手滑写错），理由必填；不新增端点，复用本步 API
			if strings.TrimSpace(req.VersionTag) != "" {
				if req.Step != "pick_version" {
					writeJSON(w, map[string]interface{}{"error": "指定版本只用于 pick_version 环节（当前步骤：" + req.Step + "）"})
					return
				}
				tag := strings.TrimSpace(req.VersionTag)
				if versionByTag(tag) == nil {
					writeJSON(w, map[string]interface{}{"error": "版本清单中无 " + tag})
					return
				}
				if strings.TrimSpace(req.Reason) == "" {
					writeJSON(w, map[string]interface{}{"error": "指定版本必须填写理由（审计留痕）"})
					return
				}
				_ = appendStepEvent(catDB, req.FlowID, step, stOK,
					"人工指定版本："+tag+"（理由："+req.Reason+"）",
					detailJSON(map[string]any{"tag": tag, "source": "human_force",
						"reason": req.Reason, "operator": cfgAuditOperator}),
					durationSinceLast(catDB, req.FlowID, req.Step, stRunning))
				addAudit("人工指定接入版本", req.Step, "接入中心", tag+"，理由："+req.Reason)
				if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, map[string]interface{}{"ok": true, "step": req.Step, "action": "force", "tag": tag})
				return
			}
			if wf != "" {
				writeJSON(w, map[string]interface{}{"error": "该环节在等待人工决策（" + wf + "），没有可跳过的工作，不能强制通过；请在上游环节完成决策"})
				return
			}
			if strings.TrimSpace(req.Reason) == "" {
				writeJSON(w, map[string]interface{}{"error": "强制通过必须填写理由（审计留痕）"})
				return
			}
			_ = appendStepEvent(catDB, req.FlowID, step, stOK,
				"人工强制通过："+req.Reason,
				detailJSON(map[string]any{"forced": true, "reason": req.Reason, "operator": cfgAuditOperator}),
				durationSinceLast(catDB, req.FlowID, req.Step, stRunning))
			addAudit("强制通过接入环节", req.Step, "接入中心", "理由："+req.Reason)
		case "skip":
			_ = appendStepEvent(catDB, req.FlowID, step, stSkipped,
				"人工跳过", detailJSON(map[string]any{"operator": cfgAuditOperator}), 0)
			addAudit("跳过接入环节", req.Step, "接入中心", "")
		case "retry":
			// 重试 = 显式清掉该步终态（fail/ok/skipped），让推进器重新做依赖判定与执行。
			// 不做数据改写，清态靠一条 pending 事件留痕（原始失败事件仍在库里可查）。
			// running 禁止 retry：平台异步作业（ansible 探路/安装）执行中，重置会孤儿化作业——
			// 作业完成后回报会被 pending 拒绝，状态机与真实执行从此脱节
			if prevStatus == stRunning {
				writeJSON(w, map[string]interface{}{"error": "该环节平台作业执行中，禁止重置；等待其完成（ok/fail）后再重试"})
				return
			}
			if wf != "" {
				writeJSON(w, map[string]interface{}{"error": "该环节在等待人工决策（" + wf + "），重试不会改变任何事；请在上游环节完成决策"})
				return
			}
			_ = appendStepEvent(catDB, req.FlowID, step, stPending,
				"人工重试：重置该环节，等待重新执行",
				detailJSON(map[string]any{"operator": cfgAuditOperator, "prev_status": prevStatus}), 0)
			addAudit("重试接入环节", req.Step, "接入中心", "")
		default:
			writeJSON(w, map[string]interface{}{"error": "unknown action: " + req.Action})
			return
		}
		if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "step": req.Step, "action": req.Action})
	})

	// ---- 取消接入 ----
	mux.HandleFunc("/api/onboard/flow/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FlowID int64 `json:"flow_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"error": "flow not found"})
			return
		}
		if err := catDB.UpdateFlowStep(req.FlowID, f.CurrentStep, "canceled"); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		addAudit("取消接入", f.ResourceID, "接入中心", fmt.Sprintf("flow-%d", req.FlowID))
		writeJSON(w, map[string]interface{}{"ok": true})
	})

	// ---- 卸载 SAgent：对已接入的资源起一条卸载流水线（接入的逆操作）----
	// 与「新建接入」同构：卸载同样是一条可观测的流水线（每一步有状态/证据/诊断），
	// 不是"点一下就当卸干净了"。入口在接入中心的资源行内按钮，模式模板 offboard 为 internal
	mux.HandleFunc("/api/onboard/flow/offboard", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ResourceID string `json:"resource_id"`
			FlowID     int64  `json:"flow_id"` // 从哪条接入流水线发起的（仅用于留痕与追溯）
			Reason     string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		resID := strings.TrimSpace(req.ResourceID)
		if resID == "" && req.FlowID > 0 {
			if f, _ := catDB.GetFlow(req.FlowID); f != nil {
				resID = f.ResourceID
			}
		}
		if resID == "" {
			writeJSON(w, map[string]interface{}{"error": "resource_id 必填（或给出来源的 flow_id）"})
			return
		}
		res, _ := catDB.GetResource(resID)
		if res == nil {
			writeJSON(w, map[string]interface{}{"error": "资源对象不存在：" + resID})
			return
		}
		tpl := getFlowTemplate("offboard")
		if tpl == nil {
			writeJSON(w, map[string]interface{}{"error": "未找到卸载模式模板 offboard，请检查 data/flow_templates/offboard.yaml 是否随镜像就位"})
			return
		}
		// 同一资源不允许并发两条卸载流水线：两条并发删同一目录，结论互相污染
		if flows, err := catDB.ListRunningFlows(); err == nil {
			for _, fl := range flows {
				if fl.Mode == "offboard" && fl.ResourceID == resID {
					writeJSON(w, map[string]interface{}{"error": fmt.Sprintf("该资源已有卸载流水线在执行中（flow-%d），请等它结束或先取消", fl.ID)})
					return
				}
			}
		}
		f := &storepkg.Flow{
			ResourceID: res.ID, ResourceIP: res.IP, Mode: tpl.ID, TemplateID: tpl.ID,
			Abilities: []string{}, AgentID: res.ID, ParamsJSON: "{}",
			Steps: tpl.snapshots(), CurrentStep: "", Status: "running",
		}
		id, err := catDB.CreateFlow(f)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		note := "资源=" + res.ID
		if req.FlowID > 0 {
			note += fmt.Sprintf("，来源接入流水线 flow-%d", req.FlowID)
		}
		if s := strings.TrimSpace(req.Reason); s != "" {
			note += "，说明：" + s
		}
		addAudit("发起卸载", res.ID, "接入中心", note)
		_ = advanceFlow(catDB, agentStore, id)
		writeJSON(w, map[string]interface{}{"ok": true, "flow_id": id, "resource_id": res.ID, "mode": tpl.ID})
	})

	// ---- 人工确认卸载影响范围（confirm_uninstall 停等人工的放行入口）----
	// 与「选版本」同一套范式：扫描完步骤落 blocked（waiting_for=human_confirm_offboard），
	// 界面把实测到的插件与自愈清单摊开，人工点「确认卸载」后才继续。
	//
	// 取消也走这条通道（decision=cancel）：把"谁在什么时候决定不卸了"留痕，
	// 而不是让流水线无声挂在 blocked 上——挂着不动和被人取消，运维看到的应是两件事
	mux.HandleFunc("/api/onboard/flow/offboard/confirm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID   int64  `json:"flow_id"`
			Decision string `json:"decision"` // proceed（确认卸载）/ cancel（取消）
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		if req.Decision != "proceed" && req.Decision != "cancel" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "decision 必须是 proceed 或 cancel"})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "flow not found"})
			return
		}
		// 只有「正停在等确认」才可放行：防止自动化脚本对着任意流水线重复提交，
		// 也防止人工重复点确认把同一步写成两条终态事件
		last, _ := catDB.LatestEvent(req.FlowID, "confirm_uninstall")
		if last == nil || last.Status != stBlocked {
			cur := "无事件"
			if last != nil {
				cur = last.Status
			}
			writeJSON(w, map[string]interface{}{"ok": false,
				"error": "confirm_uninstall 不在等待人工确认状态（当前: " + cur + "）"})
			return
		}
		var step storepkg.FlowStepSnapshot
		for _, s := range f.Steps {
			if s.ID == "confirm_uninstall" {
				step = s
				break
			}
		}
		// 影响范围数字取自扫描结论本身（而不是再查一次），保证"人看到的"与"记录下来批准的"是同一份
		var approved struct {
			PluginsTotal   int `json:"plugins_total"`
			ProcessCount   int `json:"process_count"`
			AutostartCount int `json:"autostart_count"`
		}
		_ = json.Unmarshal([]byte(last.Detail), &approved)
		note := strings.TrimSpace(req.Reason)

		if req.Decision == "cancel" {
			if note == "" {
				note = "操作人员未填写原因"
			}
			summary := "人工取消卸载：目标机与平台登记均未做任何改动（" + note + "）"
			_ = appendStepEvent(catDB, req.FlowID, step, stFail, summary,
				detailJSON(map[string]any{
					"source": "human_confirm", "decision": "cancel", "operator": cfgAuditOperator,
					"reason": note, "scope": "platform",
					"diagnosis": diagJSON([]diag{infoDiag("offboard_cancelled",
						"卸载在人工确认环节被取消——这不是故障，是操作人员的决定")}),
				}), durationSinceLast(catDB, req.FlowID, "confirm_uninstall", stBlocked))
			addAudit("人工取消卸载", f.ResourceID, "接入中心", note)
			_ = advanceFlow(catDB, agentStore, req.FlowID)
			writeJSON(w, map[string]interface{}{"ok": true, "decision": "cancel"})
			return
		}

		summary := fmt.Sprintf("人工确认卸载：操作者 admin 已确认影响范围（%d 个采集插件、%d 处自愈/自启），继续执行卸载",
			approved.PluginsTotal, approved.AutostartCount)
		if note != "" {
			summary += "，说明：" + note
		}
		_ = appendStepEvent(catDB, req.FlowID, step, stOK, summary,
			detailJSON(map[string]any{
				"source": "human_confirm", "decision": "proceed", "operator": cfgAuditOperator,
				"reason": note, "scope": "platform",
				"approved_plugins":   approved.PluginsTotal,
				"approved_processes": approved.ProcessCount,
				"approved_autostart": approved.AutostartCount,
			}), durationSinceLast(catDB, req.FlowID, "confirm_uninstall", stBlocked))
		addAudit("人工确认卸载影响范围", f.ResourceID, "接入中心", summary)
		if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "decision": "proceed"})
	})

	// ---- 升级 / 启停 SAgent：既有接入的运维操作（同 internal 模板 + 行内按钮范式）----
	// 共用拉起助手：同一资源同时只允许一条运维流水线（升级/启停/卸载互斥）——
	// 三者都动同一台机器的同一份安装，并发执行结论互相污染
	opsModes := map[string]bool{"offboard": true, "upgrade": true, "service": true}
	launchOpsFlow := func(resID, mode, paramsJSON, note string) (int64, string) {
		res, _ := catDB.GetResource(resID)
		if res == nil {
			return 0, "资源对象不存在：" + resID
		}
		tpl := getFlowTemplate(mode)
		if tpl == nil {
			return 0, "未找到模式模板 " + mode + "，请检查 data/flow_templates/" + mode + ".yaml 是否随镜像就位"
		}
		// 前提：Agent 真的装着（台账登记）。没装过就无所谓升级/启停——
		// 判据与卸载入口一致（台账驱动，不靠前端记忆）
		if agentStore.Get(resID) == nil {
			if src, _ := catDB.GetAgentSource(resID); src == "" {
				return 0, "该资源没有已接入的 SAgent（Agent 台账无登记），请先走接入流程"
			}
		}
		if flows, err := catDB.ListRunningFlows(); err == nil {
			for _, fl := range flows {
				if fl.ResourceID == resID && opsModes[fl.Mode] {
					return 0, fmt.Sprintf("该资源已有运维流水线在执行中（flow-%d · %s），请等它结束或先取消", fl.ID, fl.Mode)
				}
			}
		}
		f := &storepkg.Flow{
			ResourceID: res.ID, ResourceIP: res.IP, Mode: tpl.ID, TemplateID: tpl.ID,
			Abilities: []string{}, AgentID: res.ID, ParamsJSON: paramsJSON,
			Steps: tpl.snapshots(), CurrentStep: "", Status: "running",
		}
		id, err := catDB.CreateFlow(f)
		if err != nil {
			return 0, err.Error()
		}
		addAudit("发起"+tpl.Name, res.ID, "接入中心", note)
		_ = advanceFlow(catDB, agentStore, id)
		return id, ""
	}

	// ---- 升级 SAgent ----
	mux.HandleFunc("/api/onboard/flow/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ResourceID string `json:"resource_id"`
			FlowID     int64  `json:"flow_id"`
			Reason     string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		resID := strings.TrimSpace(req.ResourceID)
		if resID == "" && req.FlowID > 0 {
			if f, _ := catDB.GetFlow(req.FlowID); f != nil {
				resID = f.ResourceID
			}
		}
		if resID == "" {
			writeJSON(w, map[string]interface{}{"error": "resource_id 必填（或给出来源的 flow_id）"})
			return
		}
		note := "资源=" + resID
		if s := strings.TrimSpace(req.Reason); s != "" {
			note += "，说明：" + s
		}
		id, errMsg := launchOpsFlow(resID, "upgrade", "{}", note)
		if errMsg != "" {
			writeJSON(w, map[string]interface{}{"error": errMsg})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "flow_id": id, "resource_id": resID, "mode": "upgrade"})
	})

	// ---- 启停 SAgent（动作+目标在拉起时选定并留痕）----
	mux.HandleFunc("/api/onboard/flow/service", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ResourceID string `json:"resource_id"`
			FlowID     int64  `json:"flow_id"`
			Action     string `json:"action"` // stop / start / restart
			Target     string `json:"target"` // process / plugin:<name>
			Reason     string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		resID := strings.TrimSpace(req.ResourceID)
		if resID == "" && req.FlowID > 0 {
			if f, _ := catDB.GetFlow(req.FlowID); f != nil {
				resID = f.ResourceID
			}
		}
		if resID == "" {
			writeJSON(w, map[string]interface{}{"error": "resource_id 必填（或给出来源的 flow_id）"})
			return
		}
		// 参数白名单在拉起时就钉死：playbook 渲染的是白名单产物，注入进不去
		p := svcParams{Action: strings.TrimSpace(req.Action), Target: strings.TrimSpace(req.Target)}
		if !p.valid() {
			writeJSON(w, map[string]interface{}{"error": "启停参数非法：action 必须是 stop/start/restart，target 必须是 process 或 plugin:<插件名小写字母数字_->"})
			return
		}
		pj, _ := json.Marshal(p)
		note := "资源=" + resID + "，动作=" + p.Action + "，目标=" + p.targetLabel()
		if s := strings.TrimSpace(req.Reason); s != "" {
			note += "，说明：" + s
		}
		id, errMsg := launchOpsFlow(resID, "service", string(pj), note)
		if errMsg != "" {
			writeJSON(w, map[string]interface{}{"error": errMsg})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "flow_id": id, "resource_id": resID, "mode": "service"})
	})

	// ---- 升级 / 启停的人工确认放行（与卸载确认同一范式）----
	confirmOpsFlow := func(w http.ResponseWriter, r *http.Request, stepID, waitingKey, auditProceed, auditCancel string) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID   int64  `json:"flow_id"`
			Decision string `json:"decision"` // proceed / cancel
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		if req.Decision != "proceed" && req.Decision != "cancel" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "decision 必须是 proceed 或 cancel"})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "flow not found"})
			return
		}
		last, _ := catDB.LatestEvent(req.FlowID, stepID)
		if last == nil || last.Status != stBlocked {
			cur := "无事件"
			if last != nil {
				cur = last.Status
			}
			writeJSON(w, map[string]interface{}{"ok": false,
				"error": stepID + " 不在等待人工确认状态（当前: " + cur + "）"})
			return
		}
		var step storepkg.FlowStepSnapshot
		for _, s := range f.Steps {
			if s.ID == stepID {
				step = s
				break
			}
		}
		note := strings.TrimSpace(req.Reason)
		if req.Decision == "cancel" {
			if note == "" {
				note = "操作人员未填写原因"
			}
			summary := "人工取消" + auditCancel + "：目标机未做任何改动（" + note + "）"
			_ = appendStepEvent(catDB, req.FlowID, step, stFail, summary,
				detailJSON(map[string]any{
					"source": "human_confirm", "decision": "cancel", "operator": cfgAuditOperator,
					"reason": note, "scope": "platform",
					"diagnosis": diagJSON([]diag{infoDiag(stepID+"_cancelled",
						auditCancel+"在人工确认环节被取消——这不是故障，是操作人员的决定")}),
				}), durationSinceLast(catDB, req.FlowID, stepID, stBlocked))
			addAudit(auditCancel+"（取消）", f.ResourceID, "接入中心", note)
			_ = advanceFlow(catDB, agentStore, req.FlowID)
			writeJSON(w, map[string]interface{}{"ok": true, "decision": "cancel"})
			return
		}
		// 放行时把"人批准的是什么"原样落库（决策卡上的数字/版本即批准内容）
		var approved map[string]any
		_ = json.Unmarshal([]byte(last.Detail), &approved)
		summary := "人工确认" + auditProceed + "（操作者 admin），继续执行"
		if note != "" {
			summary += "，说明：" + note
		}
		_ = appendStepEvent(catDB, req.FlowID, step, stOK, summary,
			detailJSON(map[string]any{
				"source": "human_confirm", "decision": "proceed", "operator": cfgAuditOperator,
				"reason": note, "scope": "platform", "approved": approved,
			}), durationSinceLast(catDB, req.FlowID, stepID, stBlocked))
		addAudit(auditProceed+"（确认）", f.ResourceID, "接入中心", summary)
		if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "decision": "proceed"})
	}

	mux.HandleFunc("/api/onboard/flow/upgrade/confirm", func(w http.ResponseWriter, r *http.Request) {
		confirmOpsFlow(w, r, "confirm_upgrade", "human_confirm_upgrade", "升级影响确认", "升级")
	})

	// 启停**没有**确认端点：confirm_service 只做核对（预检实测摊开留痕）并自动放行，
	// 从不落 blocked，也就没有需要人来点的东西（2026-09-24 用户评审）

	// ---- 探路结果回填（外部执行器通道）----
	// ansible/ssh 实测 OS/架构后回写资源台账，并作为 preflight_host 步骤的回报证据。
	// 修复链路断点：此前探路结果进不了台账，pick_version 只能落 latest 兜底
	mux.HandleFunc("/api/onboard/flow/probe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID     int64   `json:"flow_id"`
			OS         string  `json:"os"`
			Arch       string  `json:"arch"`
			Kernel     string  `json:"kernel"`
			MemMB      float64 `json:"mem_mb"`
			DiskFreeGB float64 `json:"disk_free_gb"` // 实测值带小数（如 828.7），不按整数约束
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if strings.TrimSpace(req.OS) == "" || strings.TrimSpace(req.Arch) == "" {
			writeJSON(w, map[string]interface{}{"error": "os/arch 为必填（探路的用途就是定安装版本）"})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"error": "flow not found"})
			return
		}
		probe := map[string]any{
			"os": req.OS, "arch": req.Arch, "kernel": req.Kernel,
			"mem_mb": req.MemMB, "disk_free_gb": req.DiskFreeGB,
			"probed_by": "executor_report",
		}
		pj, _ := json.Marshal(probe)
		if err := catDB.UpdateResourceProbe(f.ResourceID, string(pj), req.OS, req.Arch, req.Kernel); err != nil {
			writeJSON(w, map[string]interface{}{"error": "探路回写失败: " + err.Error()})
			return
		}
		reason := fmt.Sprintf("OS=%s arch=%s kernel=%s mem=%.0fMB disk_free=%.1fGB",
			req.OS, req.Arch, req.Kernel, req.MemMB, req.DiskFreeGB)
		if msg := autoReportStep(catDB, agentStore, req.FlowID, "preflight_host", reason,
			map[string]any{"source": "executor_report", "probe": probe}); msg != "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": msg})
			return
		}
		addAudit("探路结果回填", f.ResourceID, "接入中心", reason)
		writeJSON(w, map[string]interface{}{"ok": true})
	})

	// ---- 人工选版本（pick_version 停等人工的确认入口）----
	// pick_version atom 拿到探路结果后落 blocked（waiting_for=human_pick），
	// 界面渲染兼容清单供人工点选；确认必须命中清单内且与实测环境兼容的 tag——
	// 清单外没有通道（例外走「强制通过」并留痕审计）
	mux.HandleFunc("/api/onboard/flow/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			FlowID int64  `json:"flow_id"`
			Tag    string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		if strings.TrimSpace(req.Tag) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "tag 必填"})
			return
		}
		f, _ := catDB.GetFlow(req.FlowID)
		if f == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "flow not found"})
			return
		}
		res, _ := catDB.GetResource(f.ResourceID)
		if res == nil || strings.TrimSpace(res.OS) == "" || strings.TrimSpace(res.Arch) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "主机探路结果缺失：先回报探路结果，再选版本"})
			return
		}
		osName, arch := strings.ToLower(res.OS), strings.ToLower(res.Arch)
		// 升级流与 pick_version 原子同一判据：目标机二进制 sha 反查版本清单得到权威
		// os/arch（台账可能过期或被误回报污染，2026-09-22 真机验收实测踩到）
		verSrc := "resource_ledger"
		if f.Mode == "upgrade" {
			if pre := svcLatestPreflightUpgrade(catDB, f.ID); pre != nil {
				if v := versionBySHA(pre.SHA); v != nil {
					osName, arch = strings.ToLower(v.OS), strings.ToLower(v.Arch)
					verSrc = "target_binary_sha"
				}
			}
		}
		found := false
		for _, v := range compatibleVersions(osName, arch) {
			if v.Tag == req.Tag {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "版本 " + req.Tag + " 不在兼容清单内（实测 " + osName + "/" + arch + "，判据 " + verSrc + "）"})
			return
		}
		last, _ := catDB.LatestEvent(req.FlowID, "pick_version")
		if last == nil || last.Status != stBlocked {
			cur := "无事件"
			if last != nil {
				cur = last.Status
			}
			writeJSON(w, map[string]interface{}{"ok": false, "error": "pick_version 不在等待人工选择状态（当前: " + cur + "）"})
			return
		}
		var step storepkg.FlowStepSnapshot
		for _, s := range f.Steps {
			if s.ID == "pick_version" {
				step = s
				break
			}
		}
		ver := versionByTag(req.Tag)
		verLabel := req.Tag
		if ver != nil {
			verLabel = "SAgent " + ver.Version + "（发布包 " + req.Tag + "）"
		}
		_ = appendStepEvent(catDB, req.FlowID, step, stOK,
			"人工选定版本："+verLabel+" · 操作者 admin（界面确认按钮提交，留痕审计）", detailJSON(map[string]any{
				"tag": req.Tag, "source": "human_pick", "operator": cfgAuditOperator,
				"probed_env": res.OS + "/" + res.Arch,
			}), durationSinceLast(catDB, req.FlowID, "pick_version", stBlocked))
		addAudit("人工选择 SAgent 版本", f.ResourceID, "接入中心", req.Tag)
		if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	})

	// ---- Agent 侧自动回报（批 C 精简版）----
	// SAgent 控制客户端按 kind 上报实测证据，平台按 agent_id 反查 running 流水线自动置 OK。
	// verify_probe 交叉核对心跳回写的 cfg_effective；observe_collect 平台直查 VM 独立验证
	mux.HandleFunc("/api/onboard/flow/agent-report", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			AgentID string         `json:"agent_id"`
			Kind    string         `json:"kind"`
			Data    map[string]any `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if req.AgentID == "" || req.Kind == "" {
			writeJSON(w, map[string]interface{}{"error": "agent_id/kind 必填"})
			return
		}
		flows, err := catDB.ListRunningFlows()
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		var f *storepkg.Flow
		for _, fl := range flows {
			if fl.AgentID == req.AgentID {
				f = fl
				break
			}
		}
		if f == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "该 agent 无待回报的 running 流水线"})
			return
		}
		stepByKind := map[string]string{
			"self_metrics":      "self_metrics",
			"preflight_ability": "preflight_ability",
			"config_applied":    "verify_probe",
			"metrics_confirmed": "observe_collect",
		}
		stepID, ok := stepByKind[req.Kind]
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "unknown kind: " + req.Kind})
			return
		}
		// verify_probe：不信 Agent 单方陈述，cfg_effective 必须已由心跳回写且与上报版本一致
		if req.Kind == "config_applied" {
			ver, _ := req.Data["applied_version"].(float64)
			expected := fmt.Sprintf("cfg-%d", int(ver))
			if a := agentStore.Get(req.AgentID); a != nil && a.CfgEffective != "" && a.CfgEffective != expected {
				writeJSON(w, map[string]interface{}{"ok": false,
					"error": "平台 cfg_effective=" + a.CfgEffective + " 与上报 " + expected + " 不一致，下个心跳后重试"})
				return
			}
		}
		// observe_collect：平台直查 VM 独立验证，Agent 的 series_count 仅作辅助证据
		if req.Kind == "metrics_confirmed" {
			// 采集目标幂等补登记：install 因 agent_exists 被跳过时（重装/重试场景），
			// file_sd 可能尚未写过——核对 VM 前确保登记；vmagent 热加载需一个刷新周期，
			// 本轮 VM 可能仍无序列，Agent 下个心跳会自动重试
			if res, _ := catDB.GetResource(f.ResourceID); res != nil && res.IP != "" {
				_, sdErr := registerScrapeTarget(f.ResourceID, res.IP)
				_ = sdErr // 登记失败不阻断核对：VM 有序列照样通过，没有则等下轮重试
			}
			n := vmSeriesCount(f.ResourceID)
			if n <= 0 {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "VM 中尚未查到该资源的序列，疑似未入库"})
				return
			}
			if req.Data == nil {
				req.Data = map[string]any{}
			}
			req.Data["vm_series_count"] = n
			req.Data["vm_verified"] = true
		}
		reason := fmt.Sprintf("Agent 自动回报 %s：%s", req.Kind, jsonCompact(req.Data))
		if msg := autoReportStep(catDB, agentStore, f.ID, stepID, reason,
			map[string]any{"source": "agent_report", "kind": req.Kind, "data": req.Data}); msg != "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": msg})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	})

	// ---- 资源对象 ----
	mux.HandleFunc("/api/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				Resources       []string `json:"resources"`
				ID              string   `json:"id"`
				IP              string   `json:"ip"`
				Name            string   `json:"name"`
				Type            string   `json:"resource_type"`
				SSHPort         int      `json:"ssh_port"`
				SSHUser         string   `json:"ssh_user"`
				SSHPassword     string   `json:"ssh_password"`
				AgentConsoleURL string   `json:"agent_console_url"`
				TargetKind      string   `json:"target_kind"` // docker / host（接入点平台全自动，2026-09-22）
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			// 两种入参：批量清单 resources[]（接入向导用）或单条 {}（资源管理用）
			list := req.Resources
			if len(list) == 0 && (req.ID != "" || req.IP != "") {
				id := firstNonEmpty(req.ID, req.IP)
				list = []string{id}
			}
			created := []string{}
			for _, raw := range list {
				res := resourceFromInput(raw)
				if req.IP != "" && req.ID != "" {
					res.ID, res.IP = req.ID, req.IP
				}
				if req.Name != "" {
					res.Name = req.Name
				}
				if req.Type != "" {
					res.ResourceType = req.Type
				}
				res.SSHPort, res.SSHUser, res.SSHPassword = req.SSHPort, strings.TrimSpace(req.SSHUser), req.SSHPassword
				if req.AgentConsoleURL != "" {
					res.AgentConsoleURL = strings.TrimSpace(req.AgentConsoleURL)
				}
				if tk := strings.TrimSpace(req.TargetKind); tk != "" {
					res.TargetKind = tk
				}
				if err := catDB.UpsertResource(res); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
				created = append(created, res.ID)
			}
			addAudit("新建资源对象", strings.Join(created, ","), "接入中心", fmt.Sprintf("%d 个", len(created)))
			writeJSON(w, map[string]interface{}{"ok": true, "created": created})
			return
		}
		if r.Method == http.MethodDelete {
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				writeJSON(w, map[string]interface{}{"error": "id required"})
				return
			}
			// 守卫：名下还有未终态流水线时拒绝——删了会让跑到一半的流水线
			// 在下一步取资源时拿不到台账（SSH 凭据/接入点全丢），属于制造半失败
			if n, err := catDB.CountActiveFlowsByResource(id); err == nil && n > 0 {
				writeJSON(w, map[string]interface{}{"error": fmt.Sprintf(
					"该资源名下还有 %d 条未终态流水线（running/blocked/stalled），先取消或走完再删除", n)})
				return
			}
			deleted, err := catDB.DeleteResource(id)
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			if !deleted {
				writeJSON(w, map[string]interface{}{"error": "资源不存在（可能已被删除）"})
				return
			}
			addAudit("删除资源对象", id, "接入中心", "资源台账删除（SSH 凭据随行删除）")
			writeJSON(w, map[string]interface{}{"ok": true})
			return
		}
		// PATCH：调整资源归属租户（架构 D3 隔离，行级分配）。校验租户真实存在，
		// 防把资源误归到不存在的租户导致触达层"永久隐身"
		if r.Method == http.MethodPatch {
			var p struct {
				ID       string `json:"id"`
				TenantID string `json:"tenant_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			p.ID, p.TenantID = strings.TrimSpace(p.ID), strings.TrimSpace(p.TenantID)
			if p.ID == "" {
				writeJSON(w, map[string]interface{}{"error": "id required"})
				return
			}
			if p.TenantID == "" {
				writeJSON(w, map[string]interface{}{"error": "tenant_id required"})
				return
			}
			existing, err := catDB.GetResource(p.ID)
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			if existing == nil {
				writeJSON(w, map[string]interface{}{"error": "资源不存在"})
				return
			}
			// 租户合法性校验：只能归属到已存在的租户（default 恒在）
			known := map[string]bool{}
			if ts, terr := catDB.ListTenants(); terr == nil {
				for _, t := range ts {
					known[t.Code] = true
				}
			}
			if !known[p.TenantID] {
				writeJSON(w, map[string]interface{}{"error": "租户不存在：" + p.TenantID})
				return
			}
			if err := catDB.UpdateResourceTenant(p.ID, p.TenantID); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			from := existing.TenantID
			if from == "" {
				from = "default"
			}
			addAudit("调整资源归属租户", p.ID, "多租户", from+" → "+p.TenantID)
			writeJSON(w, map[string]interface{}{"ok": true, "id": p.ID, "tenant_id": p.TenantID})
			return
		}
		list, err := catDB.ListResources(r.URL.Query().Get("q"), r.URL.Query().Get("role"))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		// 密码不回传明文：只标记是否已录入（界面显示「凭据已登记」）
		masked := make([]*storepkg.Resource, 0, len(list))
		for _, res := range list {
			if res.SSHPassword != "" {
				res.SSHPassword = "••••••"
			}
			// installed_tag 回填：列是后加的，早先装成的机器只有事件里有证据。
			// 只在"台账为空 + 该资源当前确有 Agent 登记"时回填——有 Agent 说明机器上还装着；
			// 卸干净的机器不回填（否则资源页会显示一个并不存在的版本）
			if res.InstalledTag == "" && agentStore.Get(res.ID) != nil {
				if tag, err := catDB.InstalledTagFromEvents(res.ID); err == nil && tag != "" {
					res.InstalledTag = tag
				}
			}
			masked = append(masked, res)
		}
		// 多租户(D3) 数据隔离：伪 token 401；带合法 token 只返回其内置租户的资源，
		// 防止跨租户读到别家台账（含 SSH 凭据标记）
		scope, claim, valid := effectiveTenantParam(r, catDB)
		if claim && !valid {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]interface{}{"error": "unauthorized: invalid tenant token"})
			return
		}
		if scope != "" {
			keep := masked[:0]
			for _, res := range masked {
				tid := res.TenantID
				if tid == "" {
					tid = "default"
				}
				if tid == scope {
					keep = append(keep, res)
				}
			}
			writeJSON(w, map[string]interface{}{"resources": keep})
			return
		}
		writeJSON(w, map[string]interface{}{"resources": masked})
	})
}

// handleOnboardFlowCreate 新建接入流水线（支持一次多个资源）
func handleOnboardFlowCreate(w http.ResponseWriter, r *http.Request, agentStore *AgentStore, catDB *storepkg.DB) {
	var req struct {
		Resources       []string                     `json:"resources"`
		Resource        string                       `json:"resource"`
		Mode            string                       `json:"mode"`
		Abilities       []string                     `json:"abilities"`
		Params          map[string]map[string]string `json:"params"`
		AgentID         string                       `json:"agent_id"`
		SSHPort         int                          `json:"ssh_port"`
		SSHUser         string                       `json:"ssh_user"`
		SSHPassword     string                       `json:"ssh_password"`
		AgentConsoleURL string                       `json:"agent_console_url"`
		TargetKind      string                       `json:"target_kind"` // docker / host（接入点平台全自动，2026-09-22）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	inputs := req.Resources
	if len(inputs) == 0 && req.Resource != "" {
		inputs = []string{req.Resource}
	}
	if len(inputs) == 0 {
		writeJSON(w, map[string]interface{}{"error": "请至少提供一个资源对象（IP 或资源 ID）"})
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "edge"
	}
	tpl := getFlowTemplate(mode)
	if tpl == nil {
		writeJSON(w, map[string]interface{}{"error": "未找到采集模式模板：" + mode})
		return
	}

	paramsJSON := "{}"
	if len(req.Params) > 0 {
		if b, err := json.Marshal(req.Params); err == nil {
			paramsJSON = string(b)
		}
	}

	created := []map[string]interface{}{}
	conflicts := []map[string]interface{}{}
	for _, raw := range inputs {
		// 台账已有该资源 → 直接引用（凭据随资源 get，绝不覆盖已录入的 IP/凭据）；
		// 没有才按新录入创建（此时凭据从本次请求带入）
		res, _ := catDB.GetResource(strings.TrimSpace(raw))
		if res == nil {
			res = resourceFromInput(raw)
			// SSH 凭据随资源录入（现阶段人工填写，模拟资源台账 get；接 CMDB 后由资源服务下发）
			res.SSHPort = req.SSHPort
			res.SSHUser = strings.TrimSpace(req.SSHUser)
			res.SSHPassword = req.SSHPassword
			// 目标类型（docker/host）：接入点不再人工填写——探路时平台自动选定并回写台账
			res.TargetKind = strings.TrimSpace(req.TargetKind)
			if err := catDB.UpsertResource(res); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
		} else {
			// 台账是权威（不覆盖已录入凭据），但向导填写的值绝不能静默丢弃——
			// 「向导填了 22022、平台却按台账的 22 去连」正是"假成功"的来源，必须说出来
			if note := credConflictNote(req.SSHPort, strings.TrimSpace(req.SSHUser), res); note != "" {
				conflicts = append(conflicts, map[string]interface{}{"resource_id": res.ID, "note": note})
				addAudit("接入向导凭据与资源台账不一致", res.ID, "接入中心", note)
			}
			// 台账未登记端口（0）时，用向导填的值补齐——这是补空缺，不是覆盖
			if res.SSHPort <= 0 && req.SSHPort > 0 {
				res.SSHPort = req.SSHPort
				if err := catDB.UpsertResource(res); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
			}
			// 目标类型同语义：台账未登记（空）时用向导填的值补齐——这是补空缺，不是覆盖
			if tk := strings.TrimSpace(req.TargetKind); tk != "" && strings.TrimSpace(res.TargetKind) == "" {
				res.TargetKind = tk
				if err := catDB.UpsertResource(res); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
			}
		}
		agentID := req.AgentID
		if agentID == "" {
			// 边缘/混合：承载机就是资源自身；远程：未指定代理机时同样先按资源自身推进，
			// 由 pick_agent 步骤如实记录「承载机是谁」，不在这里假装知道
			agentID = res.ID
		}
		f := &storepkg.Flow{
			ResourceID: res.ID, ResourceIP: res.IP, Mode: mode, TemplateID: tpl.ID,
			Abilities: req.Abilities, AgentID: agentID, ParamsJSON: paramsJSON,
			Steps: tpl.snapshots(), CurrentStep: "", Status: "running",
		}
		id, err := catDB.CreateFlow(f)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		created = append(created, map[string]interface{}{"id": id, "resource_id": res.ID, "mode": mode})
	}
	// 全部创建完再推进：避免前一条的平台侧动作影响后一条的依赖判定
	for _, c := range created {
		if id, ok := c["id"].(int64); ok {
			_ = advanceFlow(catDB, agentStore, id)
		}
	}
	addAudit("新建接入流水线", mode, "接入中心", fmt.Sprintf("%d 条", len(created)))
	writeJSON(w, map[string]interface{}{"ok": true, "created": len(created), "flows": created, "cred_conflicts": conflicts})
}

// credConflictNote 向导填写的 SSH 凭据与资源台账不一致时的说明。
// 台账是权威（不覆盖已录入值），但用户输入不能静默失效——否则"我明明填了 22022"无从解释
func credConflictNote(inPort int, inUser string, res *storepkg.Resource) string {
	parts := []string{}
	if inPort > 0 && res.SSHPort > 0 && inPort != res.SSHPort {
		parts = append(parts, fmt.Sprintf("向导填写的 SSH 端口 %d 与资源台账登记值 %d 不一致", inPort, res.SSHPort))
	}
	if inUser != "" && res.SSHUser != "" && inUser != res.SSHUser {
		parts = append(parts, "向导填写的账号 "+inUser+" 与台账登记账号 "+res.SSHUser+" 不一致")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；") + "。平台按台账登记值执行；若台账有误，请先在资源台账更正端口/账号后重试该环节"
}

// resourceFromInput 把一行输入（IP 或资源 ID）转成资源对象
func resourceFromInput(raw string) *storepkg.Resource {
	v := strings.TrimSpace(raw)
	res := &storepkg.Resource{
		ID: v, Name: v, ResourceType: "host", Role: "business", Source: "manual",
		LabelsJSON: "{}",
	}
	if isIPv4(v) {
		res.ID = "ip-" + strings.ReplaceAll(v, ".", "-")
		res.Name = v
		res.IP = v
	}
	return res
}

// isIPv4 是否点分十进制 IPv4
func isIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// flowView 流水线展示视图：补步骤状态、当前环节标题、停滞秒数
func flowView(catDB *storepkg.DB, agentStore *AgentStore, f *storepkg.Flow) map[string]interface{} {
	events, _ := catDB.ListEvents(f.ID)
	latest := map[string]*storepkg.FlowEvent{}
	for _, e := range events {
		latest[e.Step] = e
	}
	stepIDs := make([]string, 0, len(f.Steps))
	stepStatus := map[string]string{}
	currentTitle := ""
	stallSec := int64(0)
	warnCount := 0                     // 步骤级 warn/fatal 诊断条数合计：列表页据此挂警示徽标
	runningScopes := map[string]bool{} // 并行分支可能多步同时 running，收集全部执行域
	var runningEv *storepkg.FlowEvent
	var timeoutView map[string]any // 列表页也要能看见"还能等多久/会不会超时"
	for _, s := range f.Steps {
		stepIDs = append(stepIDs, s.ID)
		st := stPending
		if e, ok := latest[s.ID]; ok {
			st = e.Status
			warnCount += diagWarnCount(e.Detail)
			if e.Status == stRunning {
				runningEv = e
				runningScopes[s.Scope] = true
				if timeoutView == nil {
					timeoutView = buildTimeoutView(s, e)
				}
			}
		}
		stepStatus[s.ID] = st
		if s.ID == f.CurrentStep {
			currentTitle = s.Title
		}
	}
	if f.Status == "running" && runningEv != nil {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", runningEv.CreatedAt, time.Local); err == nil {
			if d := int64(time.Since(t).Seconds()); d > 0 {
				stallSec = d
			}
		}
	}
	// Agent 绑定语义：安装完成前 agent_id 只是「预期 ID」（资源 ID 推导，批 B 安装命令按它
	// 下发 --agent-id），是否已真实注册以 agent 台账为准——前端据此区分「已绑定」和「预期未接入」
	agentRegistered := false
	if f.AgentID != "" && agentStore != nil && agentStore.Get(f.AgentID) != nil {
		agentRegistered = true
	}
	// 平台是否持有该资源的 SSH 凭据（凭据齐备 + 本机 ansible 可用 → 探路/安装自动执行）
	sshReady := false
	if res, _ := catDB.GetResource(f.ResourceID); sshCredOf(res).valid() && ansibleAvailable() {
		sshReady = true
	}
	// 卸载入口（前端行内按钮的显示依据）：
	//   is_offboard  —— 这条自己就是卸载流水线（逆操作，不再提供卸载入口）
	//   can_offboard —— 该资源"现在真的装着"才给入口，判据只有两条：
	//     ① Agent 台账里有登记（最权威的"装着"证据）；
	//     ② 本流水线还在推进中且安装环节已落终态 —— 装完到 Agent 首次心跳登记之间有几秒空窗，
	//        窗口内不能让入口闪一下又没有。
	// 台账驱动而非"装过一次就给按钮"（实测踩过）：早先用流程历史里 install_agent==ok 当依据，
	// 于是**卸载完成后那条旧安装流水线的行内按钮还挂着**——人刚点完卸载，回头又看见「卸载」，
	// 只会以为没卸掉。卸载会把 Agent 台账注销，台账没了按钮就该跟着消失
	isOffboard := f.Mode == "offboard"
	installSettled := false
	if e, ok := latest["install_agent"]; ok && (e.Status == stOK || e.Status == stSkipped) {
		installSettled = true
	}
	canOffboard := !isOffboard && (agentRegistered || (f.Status == "running" && installSettled))
	// 升级/启停入口（2026-09-22 新增）：与卸载同一台账驱动语义——Agent 登记着才给按钮；
	// 运维流水线自身（升级/启停/卸载）不再嵌套同类入口
	isOpsFlow := opsModesList[f.Mode]
	canUpgrade := canOffboard && !isOpsFlow
	canService := canOffboard && !isOpsFlow
	// linear 内部流水线（卸载/升级/启停）：前端用同一条线性版面渲染（完成步折叠+决策卡+自动链）
	isLinear := isOffboard || f.Mode == "upgrade" || f.Mode == "service"
	// is_onboard 接入向导版面（edge/remote/hybrid 等「新建接入」类模板）。
	// 与卸载向导共用同一套「步骤式操作台」版面（2026-09-24 用户评审：参考卸载界面重设计接入界面），
	// 判据取模板的 Internal 标志——与「新建接入」向导的模式单选同一口径，
	// 将来 data/flow_templates/ 新增接入模板不必改前端
	isOnboard := false
	if !isOpsFlow {
		if t := getFlowTemplate(f.Mode); t != nil && !t.Internal {
			isOnboard = true
		}
	}
	// 维护态：来自资源台账（stop 成功置 stopped，start/restart 置 running，卸载清空）
	svcState := ""
	if res, _ := catDB.GetResource(f.ResourceID); res != nil {
		svcState = res.SvcState
	}
	// mode_name：列表页显示人话（"边缘采集"/"卸载 SAgent"），而不是内部模板 id
	modeName := f.Mode
	if t := getFlowTemplate(f.Mode); t != nil && t.Name != "" {
		modeName = t.Name
	}
	out := map[string]interface{}{
		"id": f.ID, "resource_id": f.ResourceID, "resource_ip": f.ResourceIP,
		"mode": f.Mode, "template_id": f.TemplateID, "abilities": f.Abilities,
		"mode_name": modeName,
		"agent_id":  f.AgentID, "agent_registered": agentRegistered, "ssh_ready": sshReady,
		"is_offboard": isOffboard, "can_offboard": canOffboard,
		"can_upgrade": canUpgrade, "can_service": canService,
		"is_linear": isLinear, "is_onboard": isOnboard, "is_service": f.Mode == "service",
		"svc_state": svcState,
		"status": f.Status, "current_step": f.CurrentStep,
		"current_title": currentTitle, "step_ids": stepIDs, "step_status": stepStatus,
		"started_at": f.StartedAt, "updated_at": f.UpdatedAt, "stall_sec": stallSec,
		"warn_count": warnCount,
	}
	// 超时视图：running 步骤的剩余时间、第几次尝试、会不会自动重试——列表页直接显示
	if timeoutView != nil {
		out["timeout_view"] = timeoutView
	}
	// 等待语义（与阈值无关，前端据此选横幅样式）：任一 running 分支是 external
	// 就是「合法人工等待」，不是卡住——探路/安装由人执行，耗时以小时计
	if runningScopes["external"] {
		out["wait_kind"] = "external"
	}
	// 停等人工 ≠ 被阻断：blocked 只描述「依赖没满足」，把等待原因抬到流程级，
	// 前端据此显示「待人工决策」徽章，而不是刺眼的「被阻断」
	if f.Status == "blocked" {
		if e := latest[f.CurrentStep]; e != nil {
			if w := waitingForKey(e.Detail); w != "" {
				out["wait_kind"] = "human"
				out["waiting_for"] = w
				out["waiting_label"] = waitingLabel(w)
			}
		}
	}
	// 总耗时：done/failed 用 起点→终点，running 用 起点→现在；前端页头渲染
	if t0, err := time.ParseInLocation("2006-01-02 15:04:05", f.StartedAt, time.Local); err == nil {
		end := time.Now()
		if f.Status == "done" || f.Status == "failed" || f.Status == "cancelled" {
			if t1, err := time.ParseInLocation("2006-01-02 15:04:05", f.UpdatedAt, time.Local); err == nil {
				end = t1
			}
		}
		if d := int64(end.Sub(t0).Seconds()); d >= 0 {
			out["elapsed_sec"] = d
		}
	}
	// 卡住判定分执行域（PLAN §5.1 修正）：platform/agent 步骤超阈值判 stalled；
	// 任一 running 分支是 external（人工 ansible 探路/安装，合法耗时以小时计）就不判卡住——
	// 注意不能只看 current_step 的域：并行分支下 current 是最后一个 running 步，会互相覆盖
	if stallSec > int64(cfgOnboardStallSec) && !runningScopes["external"] {
		out["stalled"] = true
		out["stall_hint"] = "等待 " + scopeOfStep(f, f.CurrentStep) + " 回报超时"
	} else if stallSec > int64(cfgOnboardStallSec) {
		out["stall_hint"] = "外部执行器执行中（人工操作合法耗时较长，不判卡住）"
	}
	return out
}

// scopeOfStep 取某步的执行者，用于停滞提示
func scopeOfStep(f *storepkg.Flow, stepID string) string {
	for _, s := range f.Steps {
		if s.ID == stepID {
			return scopeLabel(s.Scope)
		}
	}
	return "平台"
}

// timelineOf 生成时间线：按步骤顺序，每步只保留最新一条事件（原始历史仍在库里）
func timelineOf(f *storepkg.Flow, events []*storepkg.FlowEvent) []map[string]interface{} {
	latest := map[string]*storepkg.FlowEvent{}
	for _, e := range events {
		latest[e.Step] = e
	}
	out := []map[string]interface{}{}
	for _, s := range f.Steps {
		e, ok := latest[s.ID]
		if !ok {
			out = append(out, map[string]interface{}{
				"step": s.ID, "title": s.Title, "scope": s.Scope, "status": stPending,
				"summary": "等待前序环节完成", "hint": s.Hint, "gate": s.Gate,
				"requires": s.Requires,
			})
			continue
		}
		out = append(out, map[string]interface{}{
			"step": e.Step, "title": e.Title, "scope": e.Scope, "status": e.Status,
			"summary": e.Summary, "detail": e.Detail, "duration_ms": e.DurationMs,
			"started_at": e.StartedAt, "hint": s.Hint, "gate": s.Gate, "requires": s.Requires,
			"timeout_view": buildTimeoutView(s, e),
		})
	}
	return out
}

// diagWarnCount 统计步骤 detail 里的 warn/fatal 诊断条数（列表页警示徽标用）。
// info 级不算异常——"一切正常"的提示不该被算成告警
func diagWarnCount(detail string) int {
	if strings.TrimSpace(detail) == "" {
		return 0
	}
	var d struct {
		Diagnosis []struct {
			Level string `json:"level"`
		} `json:"diagnosis"`
	}
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return 0
	}
	n := 0
	for _, x := range d.Diagnosis {
		if x.Level == "warn" || x.Level == "fatal" {
			n++
		}
	}
	return n
}

// attemptDurationBase 取"本次尝试起点"的时间戳字符串：优先用进度元数据里的
// attempt_started_at（权威），缺失时回退事件自身 StartedAt（升级前遗留事件）。
func attemptDurationBase(last *storepkg.FlowEvent) string {
	if last == nil {
		return ""
	}
	if pr := progressOf(last.Detail); pr != nil && pr.StartedAt != "" {
		return pr.StartedAt
	}
	return last.StartedAt
}

// durationSinceLast 从该步骤"本次尝试的起点"起算耗时（毫秒）。
// 报告通道（探路回填/Agent 自动回报/执行器回报/人工选版）写 OK 事件时都用它补真实
// 耗时，不再传 0——每个环节在界面上都要能看到花了多久。人工选版用 blocked 起算
// （停等人工的时长），其余用 running（下发给执行器/Agent 的时长）。
//
// 基准为什么不能用"最近一条 running 事件的 StartedAt"：流式执行器在每个 TASK 边界和
// 心跳（15s）都会写一条 running 进度事件，最近一条往往距终态只有几百毫秒——照此计算，
// 一次跑了 24s 的安装会在界面上显示成「耗时 1.0s」，与同屏的尝试记录（第 1 次 · 24s）
// 自相矛盾，运维据此判断性能会得出错误结论。进度元数据里的 attempt_started_at 才是
// 本次尝试的权威起点；升级前遗留的老事件没有该字段，回退到事件时间戳（原行为）。
func durationSinceLast(catDB *storepkg.DB, flowID int64, stepID, fromStatus string) int64 {
	last, _ := catDB.LatestEvent(flowID, stepID)
	if last == nil || last.Status != fromStatus {
		return 0
	}
	base := attemptDurationBase(last)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", base, time.Local)
	if err != nil {
		return 0
	}
	ms := time.Since(t).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// autoReportStep 自动回报通道：把 running 状态的步骤置 OK（执行器探路回填 / Agent 侧上报共用）。
// 只允许覆盖 running 状态——不允许跳过依赖、不允许重复终态。返回错误信息，空串=成功。
func autoReportStep(catDB *storepkg.DB, agentStore *AgentStore, flowID int64, stepID, reason string, extra map[string]any) string {
	f, err := catDB.GetFlow(flowID)
	if err != nil || f == nil {
		return "flow not found"
	}
	var step storepkg.FlowStepSnapshot
	found := false
	for _, s := range f.Steps {
		if s.ID == stepID {
			step, found = s, true
			break
		}
	}
	if !found {
		return "step not found: " + stepID
	}
	last, _ := catDB.LatestEvent(flowID, stepID)
	if last == nil || last.Status != stRunning {
		cur := "无事件"
		if last != nil {
			cur = last.Status
		}
		return "步骤 " + stepID + " 不在 running 状态（当前: " + cur + "），不接受自动回报"
	}
	detail := map[string]any{"source": "auto_report", "reason": reason}
	for k, v := range extra {
		detail[k] = v
	}
	_ = appendStepEvent(catDB, flowID, step, stOK, "自动回报："+reason, detailJSON(detail),
		durationSinceLast(catDB, flowID, stepID, stRunning))
	addAudit("自动回报接入步骤", stepID, "接入中心", reason)
	if err := advanceFlow(catDB, agentStore, flowID); err != nil {
		return err.Error()
	}
	return ""
}

// vmSeriesCount 平台侧直查 VictoriaMetrics：某资源 ID 的活跃序列数。
// 采集入库的独立证据——不信 Agent 自报，observe_collect 验收以这里为准
func vmSeriesCount(resourceID string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	q := url.QueryEscape(`{resource_id="` + resourceID + `"}`)
	resp, err := client.Get(vmBase() + "/api/v1/query?query=" + q)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0
	}
	return len(out.Data.Result)
}

// jsonCompact 紧凑 JSON（审计摘要用，不缩进）
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
