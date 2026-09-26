package main

// ===================================================================
//  升级 / 启停 SAgent（2026-09-22 用户需求：把升级、启停流程跑通）
//  与卸载同构：internal 模板 + 行内按钮拉起 + 决策卡停等 + ansible 作业 + 证据留痕。
//  复用件：pick_version 选版范式、run.sh 守护启停协议（run/stopped 文件）、
//  control.sock status/stop/start、offboard 通用作业骨架（语法门禁→流式执行→判据收口）。
//  用户拍板：允许降级（留痕）；启停粒度=进程级+插件级；停止后资源标维护态；
//  升级前备份旧二进制（SAgent.bak-<旧版本>）
// ===================================================================

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

//go:embed data/playbooks/preflight_upgrade.yml data/playbooks/upgrade.yml data/playbooks/preflight_service.yml data/playbooks/agent_service.yml
var svcPlaybookFS embed.FS

// opsModesList 运维流水线模式集合：互斥判据（拉起时）与入口收敛判据（列表页）共用
var opsModesList = map[string]bool{"offboard": true, "upgrade": true, "service": true}

// loadSvcPlaybook 渲染升级/启停 playbook：优先磁盘外置文件（改动作不动代码），
// 缺失或被改坏时用编译期副本（go:embed，结构上不可能走样）。
// extra 为业务占位符（@BIN_SRC@ / @SVC_ACTION@ 等），值全部经平台白名单校验后注入
func loadSvcPlaybook(name, destHome string, extra map[string]string) string {
	tpl := ""
	if b, err := os.ReadFile(filepath.Join("data", "playbooks", name)); err == nil &&
		strings.Contains(string(b), "@DEST_HOME@") {
		tpl = string(b)
	} else if b, err := svcPlaybookFS.ReadFile("data/playbooks/" + name); err == nil {
		tpl = string(b)
	}
	if tpl == "" {
		return ""
	}
	rep := map[string]string{"@DEST_HOME@": destHome}
	for k, v := range extra {
		rep[k] = v
	}
	pairs := make([]string, 0, len(rep)*2)
	for k, v := range rep {
		pairs = append(pairs, k, v)
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}

// ===================================================================
//  预检输出解析（纯函数，输出契约见 playbook 头部）
// ===================================================================

// upgradePre 升级前预检结论
type upgradePre struct {
	SHA        string `json:"sha"`          // 当前二进制 sha256（"-"=未安装）
	Guard      bool   `json:"guard"`        // run.sh 守护在跑（决定停/启分支）
	Stopped    bool   `json:"stopped"`      // 守护暂停标记存在
	PID        string `json:"pid"`          // SAgent 进程 pid（"-"=不在跑）
	PluginsRaw string `json:"plugins_raw"`  // ctl status 原文（"-"=不可用）
	DiskFreeMB int    `json:"disk_free_mb"` // 安装目录所在分区可用空间
	CurrentTag string `json:"current_tag"`  // 由 sha 反查版本清单所得（查不到为空）
	CurrentVer string `json:"current_ver"`
}

// parsePreflightUpgrade 从 playbook 输出行解析预检结论（缺 UPGRADE_PRE 行 → nil）
func parsePreflightUpgrade(lines []string) *upgradePre {
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "UPGRADE_PRE ") {
			continue
		}
		kv := parseKV(strings.TrimPrefix(ln, "UPGRADE_PRE "))
		if kv == nil {
			return nil
		}
		pre := &upgradePre{
			SHA:        kv["sha"],
			Guard:      kv["guard"] == "yes",
			Stopped:    kv["stopped"] == "yes",
			PID:        kv["pid"],
			PluginsRaw: kv["plugins"],
		}
		pre.DiskFreeMB, _ = strconv.Atoi(kv["disk_free_mb"])
		if pre.SHA != "" && pre.SHA != "-" {
			if v := versionBySHA(pre.SHA); v != nil {
				pre.CurrentTag, pre.CurrentVer = v.Tag, v.Version
			}
		}
		return pre
	}
	return nil
}

// servicePre 启停前预检结论
type servicePre struct {
	PID        string `json:"pid"`         // "-"=进程不在跑
	Guard      bool   `json:"guard"`       // run.sh 守护在跑
	Stopped    bool   `json:"stopped"`     // 守护暂停标记存在（上次是"协议停止"）
	PluginsRaw string `json:"plugins_raw"` // ctl status 原文（"-"=control 不可用）
}

// parsePreflightService 从 playbook 输出行解析预检结论（缺 SVC_PRE 行 → nil）
func parsePreflightService(lines []string) *servicePre {
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "SVC_PRE ") {
			continue
		}
		kv := parseKV(strings.TrimPrefix(ln, "SVC_PRE "))
		if kv == nil {
			return nil
		}
		return &servicePre{
			PID:        kv["pid"],
			Guard:      kv["guard"] == "yes",
			Stopped:    kv["stopped"] == "yes",
			PluginsRaw: kv["ctl"],
		}
	}
	return nil
}

// servicePluginStatus 从 ctl status 原文解析插件状态表（{"n":"running",...}）
func servicePluginStatus(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" || raw == "-" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		return m
	}
	return out
}

// versionBySHA 按二进制 sha256 反查版本清单（升级预检"当前跑的是哪版"的唯一可信来源）
func versionBySHA(sha string) *SAVersion {
	if sha == "" || sha == "-" {
		return nil
	}
	versionMu.RLock()
	defer versionMu.RUnlock()
	for i := range versionCatalog {
		if versionCatalog[i].SHA256 == sha {
			v := versionCatalog[i]
			return &v
		}
	}
	return nil
}

// semverLess 比较点分版本号（"0.3.9" < "0.4.0"）；非数字段按字典序兜底
func semverLess(a, b string) bool {
	as, bs := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			if an != bn {
				return an < bn
			}
			continue
		}
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

// ===================================================================
//  flow.ParamsJSON 里的启停参数（拉起时选定、决策卡复述、执行时渲染）
// ===================================================================

type svcParams struct {
	Action string `json:"action"` // stop / start / restart
	Target string `json:"target"` // process / plugin:<name>
}

func (p svcParams) valid() bool {
	switch p.Action {
	case "stop", "start", "restart":
	default:
		return false
	}
	if p.Target == "process" {
		return true
	}
	name, ok := strings.CutPrefix(p.Target, "plugin:")
	return ok && validPluginName(name)
}

func (p svcParams) targetLabel() string {
	if p.Target == "process" {
		return "整个 SAgent（进程级）"
	}
	name, _ := strings.CutPrefix(p.Target, "plugin:")
	return "插件 " + name + "（插件级）"
}

// validPluginName 插件名白名单：小写字母/数字/下划线/连字符。
// 该名字会被渲染进 playbook 的 when 与 shell，必须钉死字符集（注入防线）
func validPluginName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func svcParamsOf(flow *storepkg.Flow) svcParams {
	var p svcParams
	_ = json.Unmarshal([]byte(flow.ParamsJSON), &p)
	return p
}

// ===================================================================
//  执行规格与通用作业（骨架与 ansibleOffboardJob 同构）
// ===================================================================

type svcSpec struct {
	Playbook string
	Title    string
	Source   string
	Audit    string
	FailCode string
	FailHint string
	// Extra 额外占位符（渲染前逐项算出；返回 error 则作业失败并落诊断）
	Extra func(catDB *storepkg.DB, agentStore *AgentStore, flowID int64, cred *sshCred) (map[string]string, error)
	// Verdict rc==0 后由它裁定成败并产出事件附加字段
	Verdict func(pr ansibleRunResult) (map[string]any, []diag)
	// PostOK 成功收尾副作用（维护态登记等）；返回的 diag 并入成功诊断
	PostOK func(catDB *storepkg.DB, flowID int64) []diag
}

var (
	svcPreflightUpgradeSpec = svcSpec{
		Playbook: "preflight_upgrade.yml", Title: "升级前预检", Source: "ansible_svc_preflight",
		Audit: "SAgent 升级前预检", FailCode: "upgrade_preflight",
		FailHint: "预检取不到结论：确认目标机安装目录完整、python/ssh 可用后重试",
		Verdict:  preflightUpgradeVerdict,
	}
	svcUpgradeSpec = svcSpec{
		Playbook: "upgrade.yml", Title: "升级 SAgent", Source: "ansible_upgrade",
		Audit: "ansible 升级 SAgent", FailCode: "upgrade_agent",
		FailHint: "核对目标机安装目录与 run/stopped 标记后点「↻ 重试该步」；旧二进制备份（SAgent.bak-*）仍在，可手工回滚",
		Extra:    upgradeExtras, Verdict: upgradeVerdict,
	}
	svcPreflightServiceSpec = svcSpec{
		Playbook: "preflight_service.yml", Title: "启停前预检", Source: "ansible_svc_preflight",
		Audit: "SAgent 启停前预检", FailCode: "service_preflight",
		FailHint: "预检取不到结论：确认目标机安装目录完整、python/ssh 可用后重试",
		Verdict:  preflightServiceVerdict,
	}
	svcExecuteSpec = svcSpec{
		Playbook: "agent_service.yml", Title: "启停 SAgent", Source: "ansible_service",
		Audit: "ansible 启停 SAgent", FailCode: "service_execute",
		FailHint: "核对目标机进程与 run/stopped 标记后点「↻ 重试该步」；守护若卡在暂停态可上机删除 ~/SAgent/run/stopped 恢复",
		Extra:    serviceExtras, Verdict: serviceVerdict, PostOK: servicePostOK,
	}
)

func svcSpecForKind(kind string) *svcSpec {
	switch kind {
	case "preflight_upgrade":
		return &svcPreflightUpgradeSpec
	case "upgrade_agent":
		return &svcUpgradeSpec
	case "preflight_service":
		return &svcPreflightServiceSpec
	case "service_execute":
		return &svcExecuteSpec
	}
	return nil
}

// upgradeExtras 升级 playbook 的业务占位符：版本取人工选定事件（与安装同一来源），
// 二进制取平台版本库并核对清单 sha，备份名带当前版本（预检 sha 反查，查不到用时间戳兜底）
func upgradeExtras(catDB *storepkg.DB, agentStore *AgentStore, flowID int64, cred *sshCred) (map[string]string, error) {
	tag := pickedVersionTag(catDB, flowID)
	if tag == "" {
		return nil, fmt.Errorf("未找到人工选定版本（pick_version 无确认记录）")
	}
	ver := versionByTag(tag)
	if ver == nil {
		return nil, fmt.Errorf("版本清单中无 %s", tag)
	}
	binPath, err := resolveVersionBinary(tag)
	if err != nil {
		return nil, err
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("平台版本库无该二进制（%s）", binPath)
	}
	sum := sha256.Sum256(binData)
	fileSHA := hex.EncodeToString(sum[:])
	if ver.SHA256 != "" && ver.SHA256 != fileSHA {
		return nil, fmt.Errorf("二进制 sha256 与版本清单不一致（清单=%s 实际=%s）", ver.SHA256, fileSHA)
	}
	// 备份名：预检反查出的当前版本优先，查不到（清单外版本）用时间戳兜底
	bak := "SAgent.bak-" + time.Now().Format("20060102-150405")
	if pre := svcLatestPreflightUpgrade(catDB, flowID); pre != nil && pre.CurrentTag != "" {
		bak = "SAgent.bak-" + pre.CurrentTag
	}
	return map[string]string{
		"@BIN_SRC@":  filepath.Join(mustAbs(binPath)),
		"@BIN_SHA@":  fileSHA,
		"@BAK_NAME@": bak,
	}, nil
}

// serviceExtras 启停 playbook 的业务占位符：动作与目标来自拉起时选定（白名单已校验）
func serviceExtras(catDB *storepkg.DB, agentStore *AgentStore, flowID int64, cred *sshCred) (map[string]string, error) {
	flow, _ := catDB.GetFlow(flowID)
	if flow == nil {
		return nil, fmt.Errorf("流水线不存在")
	}
	p := svcParamsOf(flow)
	if !p.valid() {
		return nil, fmt.Errorf("启停参数非法（action=%q target=%q）", p.Action, p.Target)
	}
	return map[string]string{"@SVC_ACTION@": p.Action, "@SVC_TARGET@": p.Target}, nil
}

// svcLatestPreflightUpgrade 取本流水线最近一次升级预检的解析结论
func svcLatestPreflightUpgrade(catDB *storepkg.DB, flowID int64) *upgradePre {
	e, _ := catDB.LatestEvent(flowID, "preflight_upgrade")
	if e == nil || e.Status != stOK {
		return nil
	}
	var d struct {
		Pre *upgradePre `json:"pre"`
	}
	if err := json.Unmarshal([]byte(e.Detail), &d); err != nil || d.Pre == nil {
		return nil
	}
	return d.Pre
}

// ---- 判据（Verdict）：playbook 自证结论裁定成败，缺结论行 = 失败 ----

func preflightUpgradeVerdict(pr ansibleRunResult) (map[string]any, []diag) {
	pre := parsePreflightUpgrade(taskOutputLines(pr))
	if pre == nil {
		return nil, []diag{fatalDiag("upgrade_preflight_no_contract",
			"预检输出缺 UPGRADE_PRE 结论行（playbook 可能被改坏或目标机异常）",
			"核对 data/playbooks/preflight_upgrade.yml 完整性后重试")}
	}
	ds := []diag{}
	if pre.SHA == "" || pre.SHA == "-" {
		ds = append(ds, fatalDiag("upgrade_target_not_installed",
			"目标机未发现 SAgent 二进制——升级前必须先接入安装",
			"先对该资源走「边缘采集」接入流程"))
	}
	if pre.DiskFreeMB > 0 && pre.DiskFreeMB < 200 {
		ds = append(ds, warnDiag("disk_low",
			fmt.Sprintf("安装目录所在分区仅剩 %d MB（新二进制+备份至少需要约 100MB）", pre.DiskFreeMB),
			"清理磁盘后再升级，或接受空间风险继续"))
	}
	extra := map[string]any{"pre": pre}
	return extra, ds
}

func upgradeVerdict(pr ansibleRunResult) (map[string]any, []diag) {
	if hasOutMarker(pr, "UPGRADE_DIRTY") {
		return nil, []diag{fatalDiag("upgrade_dirty",
			"升级自证核对未通过："+outCheckLine(pr, "UPGRADE_DIRTY"),
			"目标机保留了旧二进制备份（SAgent.bak-*）可手工回滚；排查后重试")}
	}
	if hasOutMarker(pr, "UPGRADE_GUARD_TIMEOUT") {
		return nil, []diag{fatalDiag("upgrade_guard_timeout",
			"停进程等待超时：run.sh 守护未按协议暂停（可能守护脚本被改坏）",
			"上机核查守护进程后重试；必要时手工 kill 守护")}
	}
	line := outCheckLine(pr, "UPGRADE_OK")
	if line == "" {
		return nil, []diag{fatalDiag("upgrade_no_contract",
			"升级输出缺 UPGRADE_OK 结论行（playbook 可能被改坏）",
			"核对 data/playbooks/upgrade.yml 完整性后重试")}
	}
	return map[string]any{"verify_line": line}, []diag{}
}

func preflightServiceVerdict(pr ansibleRunResult) (map[string]any, []diag) {
	pre := parsePreflightService(taskOutputLines(pr))
	if pre == nil {
		return nil, []diag{fatalDiag("service_preflight_no_contract",
			"预检输出缺 SVC_PRE 结论行（playbook 可能被改坏或目标机异常）",
			"核对 data/playbooks/preflight_service.yml 完整性后重试")}
	}
	return map[string]any{"pre": pre}, []diag{}
}

func serviceVerdict(pr ansibleRunResult) (map[string]any, []diag) {
	if hasOutMarker(pr, "SVC_DIRTY") {
		return nil, []diag{fatalDiag("service_dirty",
			"启停自证核对未通过："+outCheckLine(pr, "SVC_DIRTY"),
			"上机核查进程与 run/stopped 标记后重试")}
	}
	for _, m := range []string{"SVC_STOPPED", "SVC_STARTED", "SVC_RESTARTED", "SVC_ALREADY", "SVC_PLUGIN_STOPPED", "SVC_PLUGIN_STARTED"} {
		if line := outCheckLine(pr, m); line != "" {
			return map[string]any{"verify_line": line}, []diag{}
		}
	}
	return nil, []diag{fatalDiag("service_no_contract",
		"启停输出缺结论行（playbook 可能被改坏）",
		"核对 data/playbooks/agent_service.yml 完整性后重试")}
}

// servicePostOK 成功收尾副作用：进程级 stop → 资源标维护态；start/restart → 恢复运行态。
// 心跳消失是维护态的预期表现，不算失联（用户拍板 2026-09-22）
func servicePostOK(catDB *storepkg.DB, flowID int64) []diag {
	flow, _ := catDB.GetFlow(flowID)
	if flow == nil {
		return nil
	}
	p := svcParamsOf(flow)
	if p.Target != "process" {
		return nil
	}
	state := map[string]string{"stop": "stopped", "start": "running", "restart": "running"}[p.Action]
	if err := catDB.SetResourceSvcState(flow.ResourceID, state); err != nil {
		return []diag{warnDiag("svc_state_write_failed", "维护态登记失败："+err.Error(),
			"目标机操作已成功，仅平台状态标记未更新；刷新资源列表核实")}
	}
	return nil
}

// ansibleSvcJob 升级/启停通用作业：骨架与 ansibleOffboardJob 一致
// （语法门禁 → 流式执行 → 判据收口 → 成功副作用），差异只在占位符与判据
func ansibleSvcJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred, sp *svcSpec) {

	st := newStepRunState(catDB, flowID, step, cred)
	defer st.startHeartbeat(catDB)()
	myAttempt := st.attempt
	hook := st.taskHook(catDB)

	fail := func(summary string, detail map[string]any, ds []diag) {
		if !jobStillCurrent(catDB, flowID, step.ID, myAttempt) {
			return
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["attempt"] = myAttempt
		detail["attempts"] = st.endAttempt("fail", summary)
		detail["tasks"] = compactTasks(st.tasksSnapshot())
		failStep(catDB, agentStore, flowID, step, "ansible "+sp.Title+"失败", summary, detail, cred, ds)
	}

	flow, _ := catDB.GetFlow(flowID)
	if flow == nil {
		fail("流水线不存在", nil, []diag{fatalDiag("flow_not_found", "流水线不存在", "")})
		return
	}
	res, _ := catDB.GetResource(flow.ResourceID)
	if res == nil {
		fail("资源对象不存在", nil, []diag{fatalDiag("resource_not_found", "资源对象不存在", "检查资源台账")})
		return
	}

	destHome := destHomeFor(cred)
	var extra map[string]string
	if sp.Extra != nil {
		var err error
		extra, err = sp.Extra(catDB, agentStore, flowID, cred)
		if err != nil {
			fail(sp.Title+"参数准备失败："+err.Error(), nil,
				[]diag{fatalDiag(sp.FailCode+"_params", sp.Title+"参数准备失败："+err.Error(), sp.FailHint)})
			return
		}
	}
	pbBody := loadSvcPlaybook(sp.Playbook, destHome, extra)
	if strings.TrimSpace(pbBody) == "" {
		fail("生成 "+sp.Title+" playbook 失败：外置文件与内置副本都取不到",
			nil, []diag{fatalDiag("playbook_missing",
				"取不到 "+sp.Playbook+" 的内容（外置文件缺失且内置副本为空）",
				"检查 data/playbooks/"+sp.Playbook+" 是否存在；该文件损坏时平台会回落内置副本")})
		return
	}
	work, err := os.MkdirTemp("", "l0-svc-*")
	if err != nil {
		fail("创建临时目录失败："+err.Error(), nil,
			[]diag{fatalDiag("svc_tmp_failed", "创建临时目录失败："+err.Error(), "检查平台容器内 /tmp 是否可写")})
		return
	}
	defer os.RemoveAll(work)
	pbPath := filepath.Join(work, sp.Playbook)
	if err := os.WriteFile(pbPath, []byte(pbBody), 0o644); err != nil {
		fail("生成 playbook 失败："+err.Error(), nil,
			[]diag{fatalDiag("playbook_render_failed", "生成 playbook 失败："+err.Error(), "")})
		return
	}
	inv, cleanupInv, err := writeInventory(cred)
	if err != nil {
		fail("生成 ansible inventory 失败："+err.Error(), nil,
			[]diag{fatalDiag("inventory_write_failed", "生成 ansible inventory 失败："+err.Error(),
				"检查平台容器内 /tmp 是否可写")})
		return
	}
	defer cleanupInv()

	phases := []map[string]any{}

	// 语法门禁：语法错误在碰目标机前就拦下
	st.setPhase("syntax")
	st.flush(catDB, "平台 ansible 启动 · "+sp.Title+" playbook 语法校验中（尚未触碰目标机）")
	synCmd := exec.Command("ansible-playbook", "-i", inv, pbPath, "--syntax-check")
	synCmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False")
	synOut, synErr := synCmd.CombinedOutput()
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synErr != nil],
		"output": clip(string(synOut), 1500),
	})
	if synErr != nil {
		fail(sp.Title+" playbook 语法校验未过（未触碰目标机）", map[string]any{
			"syntax_output": clip(string(synOut), 2000), "phases": phases,
		}, []diag{fatalDiag(sp.FailCode+"_syntax", sp.Title+" playbook 语法校验未过：检查 data/playbooks/"+sp.Playbook,
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")})
		return
	}

	st.setPhase("playbook")
	st.flush(catDB, "平台 ansible 执行中 · "+sp.Title+" 开始（逐任务回报）")
	pbBudget := execBudget(st.spec, budgetPlaybook)
	ctx, cancel := context.WithTimeout(context.Background(), pbBudget)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, hook)

	evidence := map[string]any{
		"output": clip(pr.Output, 20000), "output_bytes": len(pr.Output),
		"truncated": len(pr.Output) > 20000,
	}
	if ft := taskFailures(pr.Tasks); len(ft) > 0 {
		lines := []string{}
		for _, t := range ft {
			lines = append(lines, t.Lines...)
		}
		evidence["failed_task_lines"] = clip(strings.Join(lines, "\n"), 6000)
	}
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})

	detail := map[string]any{
		"dest_home": destHome, "conn": credState(cred), "target": cred.target(),
		"rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases":     phases, "attempt": myAttempt,
		"evidence": evidence,
	}
	if pr.TimedOut {
		detail["attempts"] = st.endAttempt("timeout_run", fmt.Sprintf("playbook 超过 %ds 未返回", budgetSec(pbBudget)))
		fail(fmt.Sprintf("%s playbook 超过 %ds 未返回（平台已终止）", sp.Title, budgetSec(pbBudget)), detail,
			withPortSource(cred, []diag{fatalDiag("ansible_phase_timeout",
				fmt.Sprintf("%s playbook 超过 %ds 未返回", sp.Title, budgetSec(pbBudget)),
				"目标机负载过高或停进程等待卡住；核对目标机后点「↻ 重试该步」")}))
		return
	}
	if pr.RC != 0 {
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		d := classifySvcFailure(failText, sp)
		detail["attempts"] = st.endAttempt("fail", d.Message)
		fail(sp.Title+"失败："+d.Message+"（rc="+fmt.Sprint(pr.RC)+"）", detail,
			withPortSource(cred, []diag{d}))
		return
	}

	st.setPhase("verify")
	extraOut, ds := sp.Verdict(pr)
	if f := firstFatal(ds); f != nil {
		detail["attempts"] = st.endAttempt("fail", f.Message)
		fail(f.Message, detail, withPortSource(cred, ds))
		return
	}
	if sp.PostOK != nil {
		ds = append(ds, sp.PostOK(catDB, flowID)...)
	}

	checkLine, _ := extraOut["verify_line"].(string)
	if checkLine == "" {
		checkLine = svcCheckLine(pr)
	}
	reason := fmt.Sprintf("ansible %s完成 目标=%s 家目录=%s", sp.Title, cred.target(), destHome)
	if checkLine != "" {
		reason += " 核对=" + checkLine
		detail["verify_line"] = checkLine
	}
	if n := countWarn(ds); n > 0 {
		reason += fmt.Sprintf(" ⚠ %d 项待复核（见步骤告警）", n)
	}
	attempts := st.endAttempt("ok", reason)

	outExtra := map[string]any{
		"source": sp.Source, "detail": detail,
		"conn": credState(cred), "target": cred.target(),
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"task_total": len(pr.Tasks), "recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases": phases, "attempt": myAttempt, "attempts": attempts,
		"verify_line": checkLine,
		"diagnosis":   diagJSON(ds), "evidence": evidence,
	}
	for k, v := range extraOut {
		outExtra[k] = v
	}
	if msg := autoReportStep(catDB, agentStore, flowID, step.ID, reason, outExtra); msg != "" {
		fail(sp.Title+"回报被拒："+msg, detail,
			withPortSource(cred, []diag{fatalDiag(sp.FailCode+"_report_rejected", sp.Title+"回报被拒："+msg,
				"步骤可能已被人工重置；点「↻ 重试该步」重新执行")}))
		return
	}
	addAudit(sp.Audit, flow.ResourceID, "接入中心", reason)
}

// svcCheckLine 兜底核对行：判据没给时取第一个 SVC_*/UPGRADE_* 标记行
func svcCheckLine(pr ansibleRunResult) string {
	for _, ln := range taskOutputLines(pr) {
		if strings.HasPrefix(ln, "SVC_") || strings.HasPrefix(ln, "UPGRADE_") {
			return ln
		}
	}
	return ""
}

// classifySvcFailure 升级/启停失败归因：第一判据永远是"是否真的连上了"
func classifySvcFailure(out string, sp *svcSpec) diag {
	low := strings.ToLower(out)
	if strings.Contains(low, "unreachable!") || strings.Contains(low, "failed to connect to the host via ssh") {
		return classifySSHFailure(out, 2)
	}
	if strings.Contains(low, strings.ToLower(sp.FailCode+"_dirty")) || strings.Contains(out, "UPGRADE_DIRTY") || strings.Contains(out, "SVC_DIRTY") {
		return fatalDiag(sp.FailCode+"_incomplete", sp.Title+"未完成：目标机核对未通过", sp.FailHint)
	}
	return fatalDiag(sp.FailCode+"_failed", sp.Title+" playbook 执行失败（rc≠0）", sp.FailHint)
}

// ===================================================================
//  平台原子：两个决策卡停等点
// ===================================================================

// confirm_upgrade 摊开预检实测与升级方案，等人工放行（决策留痕到人）
func (r *flowRun) atomConfirmUpgrade(catDB *storepkg.DB) (string, string, string) {
	pre := svcLatestPreflightUpgrade(catDB, r.flow.ID)
	if pre == nil {
		return stFail, "升级预检结论缺失，无法确认影响范围", detailJSON(map[string]any{
			"reason": "preflight_missing",
			"diagnosis": diagJSON([]diag{fatalDiag("preflight_missing",
				"取不到升级前预检结论（preflight_upgrade 未成功）", "先完成预检环节")}),
		})
	}
	tag := pickedVersionTag(catDB, r.flow.ID)
	ver := versionByTag(tag)
	if ver == nil {
		return stFail, "人工选定版本不在版本清单中，无法确认升级", detailJSON(map[string]any{
			"target_tag": tag, "reason": "version_not_in_catalog",
		})
	}
	downgrade := pre.CurrentVer != "" && semverLess(ver.Version, pre.CurrentVer)
	sameVersion := pre.SHA == ver.SHA256
	summary := fmt.Sprintf("当前版本 %s → 目标版本 %s（%s），待人工确认升级影响",
		pre.CurrentVerOr("未知"), ver.Version, tag)
	if downgrade {
		summary = fmt.Sprintf("⚠ 降级操作：当前 %s → 目标 %s，待人工确认", pre.CurrentVerOr("未知"), ver.Version)
	}
	if sameVersion {
		summary = fmt.Sprintf("目标版本与当前版本相同（%s，重装同版本），待人工确认", ver.Version)
	}
	return stBlocked, summary, detailJSON(map[string]any{
		"waiting_for":     "human_confirm_upgrade",
		"current_version": pre.CurrentVer, "current_tag": pre.CurrentTag, "current_sha": pre.SHA,
		"target_tag": tag, "target_version": ver.Version, "target_sha": ver.SHA256,
		"downgrade": downgrade, "same_version": sameVersion,
		"guard": pre.Guard, "pid": pre.PID, "disk_free_mb": pre.DiskFreeMB,
		"note": "升级期间采集中断约数十秒；conf/data/logs/plugins 不动；旧二进制备份留目标机",
	})
}

// confirm_service 摊开预检实测与启停操作：核对通过即自动放行（启停不设人工闸门）
func (r *flowRun) atomConfirmService(catDB *storepkg.DB) (string, string, string) {
	e, _ := catDB.LatestEvent(r.flow.ID, "preflight_service")
	if e == nil || e.Status != stOK {
		return stFail, "启停预检结论缺失，无法确认操作", detailJSON(map[string]any{
			"reason": "preflight_missing",
			"diagnosis": diagJSON([]diag{fatalDiag("preflight_missing",
				"取不到启停前预检结论（preflight_service 未成功）", "先完成预检环节")}),
		})
	}
	var d struct {
		Pre *servicePre `json:"pre"`
	}
	_ = json.Unmarshal([]byte(e.Detail), &d)
	pre := d.Pre
	if pre == nil {
		return stFail, "启停预检结论解析失败", detailJSON(map[string]any{"reason": "preflight_parse_failed"})
	}
	p := svcParamsOf(r.flow)
	if !p.valid() {
		return stFail, "启停参数非法（拉起时校验缺失，属平台缺陷）", detailJSON(map[string]any{
			"action": p.Action, "target": p.Target, "reason": "invalid_params",
		})
	}
	// 插件级操作：目标插件必须真实存在（以 control.sock status 实测为准，不猜配置）
	if name, ok := strings.CutPrefix(p.Target, "plugin:"); ok {
		st := servicePluginStatus(pre.PluginsRaw)
		if len(st) == 0 {
			return stFail, "无法核实插件清单（control 通道不可用或 Agent 未运行），拒绝插件级操作", detailJSON(map[string]any{
				"reason": "plugins_unverifiable", "target_plugin": name,
				"diagnosis": diagJSON([]diag{fatalDiag("plugins_unverifiable",
					"插件级操作需要 control.sock 可用且 Agent 在运行", "先启动 Agent，或改用进程级操作")}),
			})
		}
		if _, exist := st[name]; !exist {
			names := make([]string, 0, len(st))
			for k := range st {
				names = append(names, k)
			}
			return stFail, "目标插件不存在：" + name, detailJSON(map[string]any{
				"reason": "plugin_not_found", "target_plugin": name, "known_plugins": names,
				"diagnosis": diagJSON([]diag{fatalDiag("plugin_not_found",
					"目标插件 "+name+" 不在 control.sock 实测清单中",
					"核对插件名后重新发起启停；可用插件："+strings.Join(names, ", "))}),
			})
		}
	}
	impact := map[string]string{
		"stop":    "采集中断，直到再次启动",
		"start":   "恢复采集，心跳与数据自行回来",
		"restart": "短暂中断后自动恢复",
	}[p.Action]
	summary := p.actionLabel() + p.targetLabel() + " · " + impact
	if pre.PID == "-" && p.Action == "stop" {
		summary = "目标机 SAgent 未在运行，停止操作将幂等命中（不做改动）"
	}
	// 启停**不设人工闸门**（2026-09-24 用户评审："停用，需要我确认？确认什么东西？？？"）：
	// 闸门只是把流程卡在半路——前端从来没有启停决策卡（obServiceConfirm 从未被挂上），
	// 人点开只看到"以下方决策卡为准"而下方空无一物，无从确认，于是 service_execute 永不执行，
	// 目标机一次都没被停过（实测 flow-9/10/23 三条全卡死）。且动作与目标在拉起时已选定并留痕，
	// 停→启随时可回，本就不需要二次点头。本环节保留为**核对**：把预检实测摊开留痕，
	// 并挡掉不可能成功的操作（插件不存在 / control 通道不可用），核对通过即自动放行到执行
	return stOK, summary + "（预检核对通过，自动放行）", detailJSON(map[string]any{
		"action":      p.Action, "target": p.Target, "target_label": p.targetLabel(),
		"impact":      impact,
		"pid":         pre.PID, "guard": pre.Guard, "stopped": pre.Stopped,
		"plugins":     servicePluginStatus(pre.PluginsRaw),
		"auto_passed": true,
	})
}

// actionLabel 动作人话（拼接进确认摘要）
func (p svcParams) actionLabel() string {
	switch p.Action {
	case "stop":
		return "停止 "
	case "start":
		return "启动 "
	case "restart":
		return "重启 "
	}
	return p.Action + " "
}

// CurrentVerOr 空值兜底（模板占位）
func (p *upgradePre) CurrentVerOr(fallback string) string {
	if p != nil && p.CurrentVer != "" {
		return p.CurrentVer
	}
	return fallback
}
