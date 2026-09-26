package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// ===================================================================
//  平台 ansible 执行器
//  设计依据：PLAN-采集接入中心与流程引擎.md §4.1；用户流程修正（2026-09-21）：
//
//  「主机探路 / 安装 SAgent」不再由人工复制命令执行——平台持有资源对象的
//  SSH 凭据（模拟资源台账下发），直接调 ansible 到目标机执行：
//    preflight_host → ansible ping(联通性) + setup(事实采集) → 自动回填台账
//    install_agent  → ansible playbook（分发二进制/sha256 校验/写配置/启动）
//  人工交互收敛到两个决策点：① 输入资源对象+凭据 ② 选 SAgent 版本。
//  凭据缺失或 ansible 不可用时回落旧「执行包+人工回报」路径（external 原语义）。
//  失败处理与人工路径一致：fail + 原始输出留痕 → 人工重试/强制。
//
//  证据契约（2026-09-21 用户要求：探测/安装过程中的异常、报错必须回到界面）：
//   - 成功也留证据：每步 detail 必带 conn（连接目标 + 端口来源）与 evidence（原始输出）
//   - 诊断分级：diagnosis[{level,code,message,hint}]，fatal 阻断 / warn 不阻断但要给人看
//   - 端口来源可追溯：台账登记值 vs 平台回落默认值，回落必须留痕
//   - 值缺失不许静默填 0：拿不到就是拿不到，标 warn 而不是伪造"实测 0"
// ===================================================================

// ansibleJobs 防并发：同一 flow:step 的异步作业在进程内只跑一份。
// 进程重启后作业丢失的兜底：running 事件超过 staleLimit 视为孤儿，允许重新拉起
var ansibleJobs sync.Map

const ansibleStaleLimit = 15 * time.Minute

// l0AgentURL 安装配置里下发给 SAgent 的平台地址（目标机 → 平台回传通道）。
// lab 拓扑下目标机与平台同网段，用容器 DNS 名；生产用 .env 覆盖
func l0AgentURL() string {
	if v := strings.TrimSpace(os.Getenv("L0_PUBLIC_URL")); v != "" {
		return v
	}
	return "http://l0-console:8080"
}

// ansibleAvailable 本机是否具备 ansible 执行条件
func ansibleAvailable() bool {
	for _, b := range []string{"ansible", "ansible-playbook", "sshpass"} {
		if _, err := exec.LookPath(b); err != nil {
			return false
		}
	}
	return true
}

// ansibleFallbackReason 平台自动执行未接管的原因（空串=已接管/无需接管）。
// 必须给人看见：否则用户只看到"怎么又要我手工执行"，却不知道是凭据缺失还是环境缺 ansible
func ansibleFallbackReason(res *storepkg.Resource) string {
	st := credState(sshCredOf(res))
	if ok, _ := st["valid"].(bool); !ok {
		reason, _ := st["reason"].(string)
		return "平台自动执行未接管：" + reason + "；本环节回落「人工复制执行包 + 回报结果」模式"
	}
	if !ansibleAvailable() {
		return "平台自动执行未接管：运行环境缺少 ansible / ansible-playbook / sshpass；本环节回落「人工复制执行包 + 回报结果」模式"
	}
	return ""
}

// sshCred 目标机 SSH 凭据（来自资源台账）
type sshCred struct {
	Host string
	Port int
	User string
	Pass string
	// PortSource 端口来源（可追溯）：ledger = 资源台账登记值；default_22 = 台账未登记端口，
	// 平台按惯例回落 22。回落必须留痕——「连上了」不等于「连对了」，端口来源是排障第一现场
	PortSource string
}

// 端口来源取值
const (
	portSourceLedger  = "ledger"
	portSourceDefault = "default_22"
)

// valid 凭据齐备性：缺一项就走不了 ansible 自动路径
func (c *sshCred) valid() bool {
	return c != nil && c.Host != "" && c.User != "" && c.Pass != ""
}

// target ansible 实际连接目标（user@host:port）——写进每一步证据，
// 不再只活在 running 事件里被后续事件覆盖掉。
// 资源行缺失（mustResource 返回 nil）时必须给可读占位：调用点含后台 goroutine（超时清扫），
// 那里没有 recover，nil 解引用会带崩整个控制台
func (c *sshCred) target() string {
	if c == nil {
		return "目标机未登记（资源台账无此行）"
	}
	return fmt.Sprintf("%s@%s:%d", c.User, c.Host, c.Port)
}

// credMissing 凭据缺哪几项（诊断/提示用）
func (c *sshCred) credMissing() []string {
	miss := []string{}
	if c == nil || c.Host == "" {
		miss = append(miss, "host")
	}
	if c == nil || c.User == "" {
		miss = append(miss, "ssh_user")
	}
	if c == nil || c.Pass == "" {
		miss = append(miss, "ssh_password")
	}
	return miss
}

// credState 凭据状态（界面直显：连的哪台机、哪个端口、端口哪来的）
func credState(c *sshCred) map[string]any {
	if miss := c.credMissing(); len(miss) > 0 {
		return map[string]any{"valid": false, "reason": "SSH 凭据不齐备，缺少：" + strings.Join(miss, ", ")}
	}
	src := c.PortSource
	if src == "" {
		src = portSourceLedger
	}
	note := fmt.Sprintf("资源台账登记端口 %d", c.Port)
	if src == portSourceDefault {
		note = fmt.Sprintf("资源台账未登记端口，平台回落默认 %d（未经核实）", c.Port)
	}
	return map[string]any{
		"valid": true, "target": c.target(), "host": c.Host, "port": c.Port, "user": c.User,
		"port_source": src, "port_source_note": note,
	}
}

// sshCredOf 从资源读凭据
func sshCredOf(res *storepkg.Resource) *sshCred {
	if res == nil {
		return nil
	}
	port, src := res.SSHPort, portSourceLedger
	if port <= 0 {
		// 不再静默回落：回落要留痕，由 portSourceDiag 在探路时把异常摆到台面上
		port, src = 22, portSourceDefault
	}
	return &sshCred{Host: res.IP, Port: port, User: res.SSHUser, Pass: res.SSHPassword, PortSource: src}
}

// writeInventory 生成临时 inventory（连接参数全部内联，跑完即删）
func writeInventory(c *sshCred) (string, func(), error) {
	dir, err := os.MkdirTemp("", "l0-ansible-*")
	if err != nil {
		return "", nil, err
	}
	inv := filepath.Join(dir, "inventory")
	// 组名 target（playbook 的 hosts 靠它匹配）与主机名 l0node 必须不同名：
	// 同名时 ansible 每条命令都会打印 "Found both group and host with same name"，
	// 把原始输出污染成一片 WARNING，异常反而被淹没
	content := fmt.Sprintf(`[target]
l0node ansible_host=%s ansible_port=%d ansible_user=%s ansible_ssh_pass=%s ansible_ssh_common_args='-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8'
`, c.Host, c.Port, c.User, c.Pass)
	if err := os.WriteFile(inv, []byte(content), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	return inv, cleanup, nil
}

// runAnsible 执行一条 ansible 命令，返回合并输出与退出码（超时由 ctx 控制）
func runAnsible(ctx context.Context, inv string, args ...string) (string, int) {
	full := append([]string{"all", "-i", inv}, args...)
	cmd := exec.CommandContext(ctx, "ansible", full...)
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

// ansibleKindOfAtom 步骤原子 → 执行类型（kind）。空串 = 该 atom 不走平台代执行通道。
//
// 单独抽出来是因为这个映射有两个消费方：进程内分发（按 kind 选作业）与 L1 对账桥
// （按 kind 选收口口径）。两处各写一套 switch，迟早出现"投递按 A、裁定按 B"。
func ansibleKindOfAtom(atom string) string {
	switch atom {
	case "preflight_host":
		return "probe"
	case "install_agent":
		return "install"
	case "uninstall_agent":
		// 卸载与安装同一套执行通道：平台持有凭据 + ansible 可用即自动执行。
		// 卸载比安装更依赖"真跑"——卸不干净却报成功，会让运维以为机器已经交还
		return "uninstall"
	case "scan_collectors":
		// 卸载前的只读扫描：同样要求"真跑"。人工手抄的清单不可信，
		// 而且这一步的结论正是后面人工确认的依据，必须来自目标机实测
		return "scan_collectors"
	case "uninstall_plugins":
		return "uninstall_plugins"
	case "cleanup_autostart":
		return "cleanup_autostart"
	case "preflight_upgrade", "upgrade_agent", "preflight_service", "service_execute":
		// 升级/启停四步与卸载同一套通道（kind 名 = 步骤原子名，分发处按名取 spec）
		return atom
	}
	return ""
}

// tryAnsibleExecutor 在 external 分支尝试平台代执行。
// 返回 true 表示已接管（步骤落 running、异步作业已拉起）；
// 返回 false 表示走旧的「执行包 + 人工回报」路径。
func tryAnsibleExecutor(catDB *storepkg.DB, agentStore *AgentStore, r *flowRun, step storepkg.FlowStepSnapshot) bool {
	cred := sshCredOf(r.resource)
	if !cred.valid() {
		return false
	}
	kind := ansibleKindOfAtom(step.Atom)
	if kind == "" {
		return false
	}
	// 防并发 + 孤儿兜底。
	// 超时自动重试时会先登记一次性强制重拉标记：旧作业可能仍挂在进程里，
	// 此时必须清掉占用并直接重拉，否则"重试"指令永远发不出去
	// （标记在两条执行路径上都要消费掉，否则会残留到下次进程内调用时被误当作绕过保护）
	forced := takeForcedRestart(r.flow.ID, step.ID)

	// D8 灰度：支持集内的 atom 走 L1 Ansible Runner（B3 install；B4 卸载侧四步）。
	// 该路径不依赖 L0 本机 ansible——执行端在 L1，L0 只是投递方；
	// 这正是「L0 不持有被管主机可达」的前置。其余 atom 仍走进程内，故此处才判 ansible 可用性。
	if l1ExecExecutor() == execRunner && l1ExecSupportedKind(kind) {
		return deliverStepToRunner(catDB, agentStore, r, step, cred, forced, kind)
	}
	if !ansibleAvailable() {
		return false
	}

	jobKey := fmt.Sprintf("%d:%s", r.flow.ID, step.ID)
	if forced {
		ansibleJobs.Delete(jobKey)
	} else {
		if _, busy := ansibleJobs.Load(jobKey); busy {
			return true // 已有作业在跑：保持 running，不重复拉起也不下发执行包
		}
		if last, _ := catDB.LatestEvent(r.flow.ID, step.ID); last != nil && last.Status == stRunning {
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", last.CreatedAt, time.Local); err == nil {
				if time.Since(t) < ansibleStaleLimit {
					return true
				}
			}
		}
	}

	detail := map[string]any{
		"executor": "ansible",
		"runner":   "platform",
		"conn":     credState(cred),
		"target":   cred.target(),
		"note":     "平台持有资源 SSH 凭据，ansible 自动执行；完成后自动回报，无需人工操作",
	}
	if cred.PortSource == portSourceDefault {
		detail["diagnosis"] = diagJSON([]diag{portSourceDiag(cred)})
	}
	_ = appendStepEvent(catDB, r.flow.ID, step, stRunning,
		"平台 ansible 执行中（"+cred.target()+"）", detailJSON(detail), 0)

	ansibleJobs.Store(jobKey, true)
	go func() {
		defer ansibleJobs.Delete(jobKey)
		switch kind {
		case "probe":
			ansibleProbeJob(catDB, agentStore, r.flow.ID, step, cred)
		case "uninstall":
			ansibleUninstallJob(catDB, agentStore, r.flow.ID, step, cred)
		case "scan_collectors":
			ansibleScanJob(catDB, agentStore, r.flow.ID, step, cred)
		case "uninstall_plugins":
			ansibleUninstallPluginsJob(catDB, agentStore, r.flow.ID, step, cred)
		case "cleanup_autostart":
			ansibleCleanupAutostartJob(catDB, agentStore, r.flow.ID, step, cred)
		case "preflight_upgrade":
			ansibleSvcJob(catDB, agentStore, r.flow.ID, step, cred, svcSpecForKind("preflight_upgrade"))
		case "upgrade_agent":
			ansibleSvcJob(catDB, agentStore, r.flow.ID, step, cred, svcSpecForKind("upgrade_agent"))
		case "preflight_service":
			ansibleSvcJob(catDB, agentStore, r.flow.ID, step, cred, svcSpecForKind("preflight_service"))
		case "service_execute":
			ansibleSvcJob(catDB, agentStore, r.flow.ID, step, cred, svcSpecForKind("service_execute"))
		default:
			ansibleInstallJob(catDB, agentStore, r.flow.ID, step, cred)
		}
	}()
	return true
}

// ===================================================================
//  证据与分级诊断
//  平台执行器不是「成功就静默、失败才吭声」，而是每步都留两类东西：
//    evidence  —— 原始输出（ansible 原文，截断但保头保尾，绝不吞）
//    diagnosis —— 分级诊断：fatal 阻断 / warn 异常但不阻断 / info 提示
//  分级的意义：端口来源可疑、facts 缺字段这类问题不该阻断流程，
//  但也绝不该被静默吞掉——它们正是"假成功"的来源。
// ===================================================================

// diag 一条分级诊断
type diag struct {
	Level   string `json:"level"` // fatal / warn / info
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func fatalDiag(code, msg, hint string) diag { return diag{"fatal", code, msg, hint} }
func warnDiag(code, msg, hint string) diag  { return diag{"warn", code, msg, hint} }
func infoDiag(code, msg string) diag        { return diag{"info", code, msg, ""} }

// envCheckOut 环境预检解析结果（纯函数输出，便于回归测试三态语义）
type envCheckOut struct {
	python   string // ""=未解析 / "none"=无 Python / 其他=版本串
	connCode string // "200"=可达 / "FAIL"=双工具实测不通 / "NO_TOOL"=无探测工具 / ""=未返回
	connTool string // curl / wget / none
	skewSec  int64  // 目标机与平台的时间偏差（秒）
}

// parseEnvCheck 解析探路环境预检的输出行（PY:/EPOCH:/CONN:/TOOL:）。
// 三态语义的关键：CONN:FAIL（curl+wget 双工具都探不通）与 CONN:NO_TOOL（工具缺失）
// 必须是两个不同的结论——把「工具不在」当成「不可达」会误伤所有精简系统目标机，
// 把「不可达」当成「通过」则装完 Agent 必然卡死在等回报。宁可报未核实，不谎报任何一边。
func parseEnvCheck(output string, now int64) envCheckOut {
	out := envCheckOut{}
	for _, ln := range strings.Split(output, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.Contains(ln, "PY:NONE"):
			out.python = "none"
		case strings.Contains(ln, "PY:"):
			out.python = strings.TrimSpace(ln[strings.Index(ln, "PY:")+3:])
		case strings.Contains(ln, "CONN:"):
			// 探测行形如 "CONN:200 TOOL:curl"——结论与工具在同一行，
			// 必须一起解析；拆成两个 case 会因 switch 命中 TOOL 而吞掉 CONN（真实踩过）
			rest := ln[strings.Index(ln, "CONN:")+5:]
			if i := strings.Index(rest, "TOOL:"); i >= 0 {
				out.connTool = strings.TrimSpace(rest[i+5:])
				rest = strings.TrimSpace(rest[:i])
			}
			out.connCode = strings.TrimSpace(rest)
		case strings.Contains(ln, "EPOCH:"):
			raw := strings.TrimSpace(ln[strings.Index(ln, "EPOCH:")+6:])
			if v, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
				out.skewSec = now - v
				if out.skewSec < 0 {
					out.skewSec = -out.skewSec
				}
			}
		}
	}
	return out
}

// diagJSON 诊断数组序列化：空数组也返回 []，前端判空不为 null
func diagJSON(list []diag) []diag {
	if list == nil {
		return []diag{}
	}
	return list
}

// countWarn 统计 warn/fatal 条数（摘要文案与列表徽标用）
func countWarn(list []diag) int {
	n := 0
	for _, d := range list {
		if d.Level == "warn" || d.Level == "fatal" {
			n++
		}
	}
	return n
}

// clip 截断长文本：保头保尾并标注截掉多少字节。
// 只留尾会丢失败首因、只留头会丢最终结论，两头都要留
func clip(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max || max <= 0 {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	return s[:head] + fmt.Sprintf("\n…[已截断 %d 字节]…\n", len(s)-max) + s[len(s)-tail:]
}

// portSourceDiag 端口来源告警：台账未登记端口时平台回落 22，这属于必须让人看见的异常
func portSourceDiag(c *sshCred) diag {
	return warnDiag("port_source_default",
		fmt.Sprintf("SSH 端口 %d 来自平台默认值（资源台账未登记），未经核实", c.Port),
		"核对该主机 sshd 实际监听端口后，把登记值补进资源台账的 SSH 端口字段")
}

// withPortSource 端口来源告警前置（来源=台账登记值时无告警）
func withPortSource(c *sshCred, ds []diag) []diag {
	if c != nil && c.PortSource != portSourceLedger {
		return append([]diag{portSourceDiag(c)}, ds...)
	}
	return ds
}

// classifySSHFailure 把 SSH 层报错归类成看得懂的诊断，而不是只报 rc=N。
// 调用方必须已确认这是 SSH 传输层故障（ansible 报 UNREACHABLE!）。
//
// 「认证失败」只认 ssh 自己给出的措辞：permission denied (publickey/password/...)。
// 裸 "Permission denied" 不能算——目标机文件系统的 [Errno 13] Permission denied
// 长得一模一样，把它当认证问题会让人跑去核查台账密码，方向完全错。
func classifySSHFailure(out string, rc int) diag {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "permission denied (publickey") ||
		strings.Contains(low, "permission denied (password") ||
		strings.Contains(low, "permission denied (keyboard-interactive") ||
		strings.Contains(low, "authentication failed"):
		return fatalDiag("ssh_auth_failed", "SSH 认证失败（账号或密码被拒）",
			"核对资源台账里的 ssh_user / ssh_password")
	case strings.Contains(low, "host key verification failed"):
		return fatalDiag("ssh_hostkey", "SSH 主机密钥校验失败",
			"平台已关闭 StrictHostKeyChecking，仍报此错说明目标机密钥异常，请检查 sshd 侧配置")
	case strings.Contains(low, "connection refused"):
		return fatalDiag("ssh_conn_refused", "SSH 端口拒绝连接（该端口没有服务在监听）",
			"核对 SSH 端口登记值；该主机 sshd 很可能不在这个端口")
	case strings.Contains(low, "no route to host"):
		return fatalDiag("ssh_no_route", "网络不可达（无路由到目标主机）",
			"核对资源 IP 与平台到目标机的网络策略")
	case strings.Contains(low, "timed out") || strings.Contains(low, "timeout"):
		return fatalDiag("ssh_timeout", "SSH 连接超时",
			"目标机不可达或防火墙丢包；端口不对也可能表现为挂起超时")
	case strings.Contains(low, "unreachable"):
		return fatalDiag("ssh_unreachable", fmt.Sprintf("目标机不可达（ansible 报 UNREACHABLE，rc=%d）", rc),
			"核对 IP / 端口 / 网络策略")
	}
	return fatalDiag("ansible_rc", fmt.Sprintf("ansible 执行失败（rc=%d）", rc), "查看下方原始输出定位首因")
}

// classifyPlaybookFailure 安装 playbook 失败归因（任务名 + 报错特征）
//
// 第一判据永远是「playbook 是否真的连上了目标机」：ansible 只在 SSH 传不到时
// 报 UNREACHABLE!。只要输出里出现过 TASK 执行记录，后面的任何失败都发生在
// 目标机内部（文件权限、命令返回码、断言不过），不能倒推成 SSH 层问题。
func classifyPlaybookFailure(out string, rc int) diag {
	low := strings.ToLower(out)
	if strings.Contains(low, "unreachable!") || strings.Contains(low, "failed to connect to the host via ssh") {
		return classifySSHFailure(out, rc)
	}
	switch {
	case strings.Contains(low, "assert binary sha256") || strings.Contains(low, "sha256 matches catalog"):
		return fatalDiag("binary_sha256_mismatch", "二进制 sha256 与版本清单不一致，安装被拒",
			"重新分发二进制，或修正 data/versions.yaml 登记的 sha256")
	case strings.Contains(low, "start agent"):
		return fatalDiag("agent_start_failed", "SAgent 启动失败（进程未能存活）",
			"查看原始输出中 start agent 任务段；常见原因：二进制架构不符 / 端口占用 / 权限不足")
	case strings.Contains(low, "errno 13") || strings.Contains(low, "permission denied"):
		return targetPermissionDiag(out)
	}
	return fatalDiag("playbook_rc", fmt.Sprintf("安装 playbook 执行失败（rc=%d）", rc),
		"查看下方原始输出，定位第一个 FAILED! 的任务")
}

// targetPermissionDiag 目标机内的文件/目录权限不足。
// 这一类必须与 SSH 认证失败严格区分：凭据是对的，是部署目录登录用户写不进去
func targetPermissionDiag(out string) diag {
	path := targetPathFromErr(out)
	msg := "目标机权限不足：登录用户无法创建/写入部署目录"
	if path != "" {
		msg = "目标机权限不足：" + path + " 不允许当前登录用户创建/写入"
	}
	return fatalDiag("target_permission_denied", msg,
		"凭据正常，是部署目录的属主/权限问题。二选一："+
			"① 用 root 在目标机预建该目录并把属主改成登录用户；"+
			"② 改 data/playbooks/install.yml 的 @DEST_HOME@（默认取登录用户家目录 /home/<ssh_user>）到可写路径")
}

// 权限报错的两种常见措辞，优先取 ansible file 模块的「创建目标」，
// 其次取 errno 里的实际路径——两者都可能出现，前者更贴近"该改哪个目录"
var (
	reIssueCreating = regexp.MustCompile(`issue creating (/[^\s]+) as requested`)
	rePermDeniedDir = regexp.MustCompile(`Permission denied: '(/[^']+)'`)
)

// targetPathFromErr 从 ansible 权限报错里抽出目标机真实路径（抽不到返回空，不编造）
func targetPathFromErr(out string) string {
	if m := reIssueCreating.FindStringSubmatch(out); len(m) > 1 {
		return m[1]
	}
	if m := rePermDeniedDir.FindStringSubmatch(out); len(m) > 1 {
		return m[1]
	}
	return ""
}

// failStep 步骤失败统一收口：executor / conn / diagnosis 三件套必带，
// 原始输出由调用方放进 evidence（不在这里兜底，避免"看起来有证据其实为空"）
func failStep(catDB *storepkg.DB, agentStore *AgentStore, flowID int64, step storepkg.FlowStepSnapshot,
	auditAction, summary string, detail map[string]any, cred *sshCred, ds []diag) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["executor"] = "ansible"
	if _, ok := detail["conn"]; !ok {
		detail["conn"] = credState(cred)
	}
	detail["diagnosis"] = diagJSON(ds)
	_ = appendStepEvent(catDB, flowID, step, stFail, summary, detailJSON(detail), 0)
	addAudit(auditAction, step.ID, "接入中心", summary)
	_ = advanceFlow(catDB, agentStore, flowID)
}

// ---------------- 目标机 sshd 端口核对 ----------------
// 「连上了」不等于「连对了」：22 与自定义端口（如 22022）可能同时可连，
// 纯连通性测试发现不了"连错端口"。唯一手段是拿目标机自己的口径比对：
//  ① sshd_config 里显式声明的 Port（服务端口径）
//  ② ss/netstat 的本机监听端口（内核口径）
// 命令刻意不含引号（awk 程序会与 ansible 的参数解析打架），解析放在 Go 里做

// parseSSHDConfigPorts 解析 sshd_config 的 Port 声明（忽略注释行）
func parseSSHDConfigPorts(out string) []int {
	seen := map[int]bool{}
	ports := []int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "Port") {
			continue
		}
		if p, err := strconv.Atoi(strings.TrimSpace(f[1])); err == nil && p > 0 && p <= 65535 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

// parseListenPorts 解析 ss -ltn / netstat -ltn 的本机监听端口（第 4 列 Local Address）
func parseListenPorts(out string) []int {
	seen := map[int]bool{}
	ports := []int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "State") || strings.HasPrefix(line, "Proto") ||
			strings.HasPrefix(line, "Active") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		addr := f[3]
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			addr = addr[i+1:]
		}
		if p, err := strconv.Atoi(strings.TrimSpace(addr)); err == nil && p > 0 && p <= 65535 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// sshdPortCheck 读目标机 sshd 口径并与本次连接端口比对，返回分级诊断 + 原始证据。
// 三条命令都走流式执行器：命令原文与逐条结果一起进 evidence，人工可复核。
// 每条命令的超时由步骤配置推导（budgetPortCmd × RunSec），不写死秒数
func sshdPortCheck(inv string, spec StepTimeout, cred *sshCred, onTask func(ansibleTask, int)) ([]diag, map[string]any) {
	cap := phaseBudget(spec, budgetPortCmd)

	ctx1, c1 := context.WithTimeout(context.Background(), cap)
	defer c1()
	r1 := runAnsibleStream(ctx1, inv, onTask, "-m", "shell", "-a", "grep -h Port /etc/ssh/sshd_config")

	ctx2, c2 := context.WithTimeout(context.Background(), cap)
	defer c2()
	r2 := runAnsibleStream(ctx2, inv, onTask, "-m", "shell", "-a", "ss -ltn")
	tool := "ss"
	if r2.RC != 0 || strings.TrimSpace(r2.Output) == "" {
		// 精简系统（alpine/busybox）常常只有 netstat
		ctx3, c3 := context.WithTimeout(context.Background(), cap)
		defer c3()
		r2 = runAnsibleStream(ctx3, inv, onTask, "-m", "shell", "-a", "netstat -ltn")
		tool = "netstat"
	}

	configured := parseSSHDConfigPorts(r1.Output)
	listening := parseListenPorts(r2.Output)
	ev := map[string]any{
		"configured_ports": configured, "listening_ports": listening,
		"listen_tool":        tool,
		"sshd_config_output": clip(r1.Output, 1500),
		"listen_output":      clip(r2.Output, 1500),
		"sshd_config_rc":     r1.RC, "listen_rc": r2.RC,
	}
	return portCheckDiags(cred, configured, listening, r1.RC, r2.RC), ev
}

// portCheckDiags 端口一致性判定（纯函数，便于单测）：
// 连接口必须落在目标机本机监听端口里，否则就是"连上了但不是本机 sshd"
func portCheckDiags(cred *sshCred, configured, listening []int, cfgRC, listRC int) []diag {
	ds := []diag{}
	switch {
	case listRC != 0 || len(listening) == 0:
		ds = append(ds, warnDiag("sshd_listen_unreadable",
			"读不到目标机监听端口（ss/netstat 不可用或输出为空），无法自动核对端口",
			"请人工在目标机执行 ss -ltn 或 netstat -ltn，核对 sshd 实际监听端口与平台登记值"))
	case !containsInt(listening, cred.Port):
		ds = append(ds, warnDiag("port_not_listening_on_target",
			fmt.Sprintf("本次连接端口 %d 不在目标机本机监听端口 %v 中", cred.Port, listening),
			"连接可能经 NAT / 端口转发才落到目标机，或该端口由别的 sshd 提供——请核对 SSH 端口登记值"))
	default:
		ds = append(ds, infoDiag("port_verified",
			fmt.Sprintf("端口核对通过：本次连接端口 %d 就在目标机 sshd 的监听列表中（sshd_config 声明 %v）", cred.Port, configured)))
	}
	if cfgRC != 0 || len(configured) == 0 {
		// sshd_config 在多数发行版是 0600 root-only，普通运维账号读不到。
		// 读不到就说读不到——不能因为"没读到"就假装端口核对通过了
		ds = append(ds, infoDiag("sshd_config_unreadable",
			"提示（非错误）：运维账号非 root，读不到目标机 /etc/ssh/sshd_config 的 Port 声明；端口核对已退化为「比对实际监听端口」，结论仍然有效"))
	} else if !containsInt(configured, cred.Port) {
		ds = append(ds, warnDiag("port_not_in_sshd_config",
			fmt.Sprintf("sshd_config 声明端口 %v 不含本次连接端口 %d", configured, cred.Port),
			"sshd 可能由 sshd_config.d / 启动参数改了端口；若本次端口是默认回落值，请尽快把真实端口登记进资源台账"))
	}
	return ds
}

// ===================================================================
//  能力可行性检查（2026-09-21：原「能力探路」环节并入主机探路）
//
//  为什么合并：能力探路原本是「装完 Agent 之后由 Agent 自证」的一个独立环节，但它要回答的
//  问题有一半在装机之前就有答案——目标不可达、依赖不在，装了也采不到；而 Agent 侧那条自证
//  当时是写死的常量（端口/配置/依赖恒 true），既拦不住人也不构成证据。
//  合并后：主机探路一次跑完「主机事实 + 能力可行性」，不达标的在装机之前就被拦住。
//
//  检查项由「所选能力 + 用户填的参数 + 插件包 params.yaml 的 probe 绑定」生成——
//  一处声明三处复用（表单渲染 / 检查项生成 / 字段映射），引擎里不出现任何插件名。
//  检查在目标机上执行：从真正要发采集流量的那一侧探，平台侧只负责解释结果。
//  拿不到结论的一律记 unknown 并告警——不许把"没查"写成"通过"。
// ===================================================================

// abilityProbe 一条能力可行性检查项
type abilityProbe struct {
	Ability  string `json:"ability"`            // 能力 id（插件名，来自流水线勾选）
	Kind     string `json:"kind"`               // tcp / http / delegated
	Target   string `json:"target"`             // 地址:端口 / URL / 交由 Agent 侧验证的项
	Verdict  string `json:"verdict"`            // ok / fail / unknown / delegated
	Observed string `json:"observed,omitempty"` // 实测原文（失败原因等），供人复核
	Level    string `json:"level"`              // blocker 未过 → 就地拦截；info 只呈报
	Note     string `json:"note,omitempty"`
}

// abilityProbeChecks 由能力声明生成检查项（纯函数，回归有断言）。
//   - probe=tcp：从目标机侧探「地址:端口」是否可达（地址/端口取用户填的参数）
//   - probe=http：从目标机侧取一次 URL（抓取端点必须真的能返回）
//   - 其他 probe（如需要口令的账号类）：不代跑——明文口令不进探路日志与证据，
//     记 delegated，由 Agent 侧在配置下发后验证
//   - 参数缺失：记 unknown——collect_params 已校验必填，走到这里说明声明与参数不一致，
//     这种情况必须让人看见，不许静默跳过检查
func abilityProbeChecks(all []*Ability, selected []string, params map[string]map[string]string) []abilityProbe {
	out := []abilityProbe{}
	for _, a := range all {
		// 勾选的能力 + 地基能力（locked）都要查：地基能力是强制开启的，同样要能采到
		if !containsStr(selected, a.ID) && !a.Locked {
			continue
		}
		vals := params[a.ID]
		hasTCP, hasHTTP := false, false
		delegated := []string{}
		for _, f := range a.Params {
			switch f.Probe {
			case "tcp":
				hasTCP = true
			case "http":
				hasHTTP = true
			case "":
				// 该字段不参与探路核对
			default:
				delegated = append(delegated, firstNonEmpty(f.Label, f.Name))
			}
		}
		switch {
		case hasHTTP:
			target := firstNonEmpty(vals["url"], vals["address"], vals["endpoint"])
			p := abilityProbe{Ability: a.ID, Kind: "http", Target: target, Level: "blocker"}
			if target == "" {
				p.Verdict, p.Note = "unknown", "参数里没有抓取地址（url），检查项无法生成"
			}
			out = append(out, p)
		case hasTCP:
			host, port := tcpTargetOf(vals)
			p := abilityProbe{Ability: a.ID, Kind: "tcp", Target: host + ":" + port, Level: "blocker"}
			if host == "" || port == "" {
				p.Verdict, p.Note = "unknown", "参数里缺目标地址或端口，检查项无法生成"
			}
			out = append(out, p)
		}
		if len(delegated) > 0 {
			out = append(out, abilityProbe{
				Ability: a.ID, Kind: "delegated", Level: "info", Verdict: "delegated",
				Target: strings.Join(delegated, "/"),
				Note: "需要凭据或本机客户端才能验证（账号/权限类）：由 Agent 侧在配置下发后验证；" +
					"平台探路不代跑——口令不进探路日志与证据",
			})
		}
	}
	return out
}

// tcpTargetOf 从参数里取目标地址与端口：优先 address/host；address 里自带 :端口 时按端口用
func tcpTargetOf(vals map[string]string) (string, string) {
	host := firstNonEmpty(vals["address"], vals["host"], vals["endpoint"])
	port := firstNonEmpty(vals["port"])
	if host == "" {
		return "", ""
	}
	if i := strings.LastIndex(host, ":"); i > 0 {
		if tail := host[i+1:]; tail != "" && strings.Trim(tail, "0123456789") == "" {
			if port == "" {
				port = tail
			}
			host = host[:i]
		}
	}
	return strings.Trim(host, "[]"), port
}

// splitProbeTarget 把检查项里的「地址:端口」拆回来（IPv6 字面量按最后一个冒号切）
func splitProbeTarget(t string) (string, string) {
	if i := strings.LastIndex(t, ":"); i > 0 {
		return strings.Trim(t[:i], "[]"), t[i+1:]
	}
	return t, ""
}

// interpretAbilityCheck 把一次检查的执行结果翻译成结论（纯函数，回归有断言）。
// 关键：拿不到结论 ≠ 目标不可达，两者不许混为一谈——前者是 unknown（告警放行），
// 后者才是 fail（就地拦截）
func interpretAbilityCheck(kind string, rc int, timedOut bool, out string) (string, string, string) {
	observed := clip(strings.TrimSpace(out), 400)
	switch {
	case timedOut:
		return "unknown", observed, "本地预算超时：检查未跑完，结论不可用（点「↻ 重试该步」重跑）"
	case rc == 0:
		return "ok", observed, ""
	}
	if kind == "http" && strings.Contains(out, "Status code was 40") {
		// 401/403：端点确实可达，只是要鉴权——这不是"不可达"
		return "ok", observed, "端点可达但有鉴权要求（HTTP 401/403），抓取时需带凭据"
	}
	if strings.Contains(out, "No module named") {
		return "unknown", observed, "目标机缺少检查所需的解释器，检查未能执行"
	}
	return "fail", observed, ""
}

// countProbeVerdict 统计某一结论的检查项条数（台账与回报摘要共用）
func countProbeVerdict(checks []abilityProbe, verdict string) int {
	n := 0
	for _, c := range checks {
		if c.Verdict == verdict {
			n++
		}
	}
	return n
}

// execAbilityChecks 在目标机侧逐条执行检查项，返回结论 + 原文证据 + 各次 recap。
// pyFound=="none" 时不执行：ansible 的 wait_for/uri 都要目标机 Python，
// 此时"探不通"与"工具不在"无法区分——一律记 unknown 并告警，绝不当作通过
func execAbilityChecks(inv string, st *stepRunState, hook func(ansibleTask, int),
	checks []abilityProbe, pyFound string) ([]abilityProbe, map[string]any, []map[string]int) {
	out := make([]abilityProbe, 0, len(checks))
	raw := map[string]any{}
	recaps := []map[string]int{}
	for _, p := range checks {
		// 生成期就定了结论的（delegated / 参数缺失）不执行，原样呈报
		if p.Kind == "delegated" || p.Verdict == "unknown" {
			out = append(out, p)
			continue
		}
		if pyFound == "none" {
			p.Verdict, p.Observed = "unknown", ""
			p.Note = "目标机没有 Python（python3/python 均无），检查未能执行——这是「没查到」而不是「有问题」"
			out = append(out, p)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), phaseBudget(st.spec, budgetCheck))
		var res ansibleRunResult
		switch p.Kind {
		case "http":
			res = runAnsibleStream(ctx, inv, hook, "-m", "uri", "-a",
				fmt.Sprintf("url=%s timeout=5", p.Target))
		default: // tcp
			host, port := splitProbeTarget(p.Target)
			res = runAnsibleStream(ctx, inv, hook, "-m", "wait_for", "-a",
				fmt.Sprintf("host=%s port=%s timeout=5", host, port))
		}
		cancel()
		p.Verdict, p.Observed, p.Note = interpretAbilityCheck(p.Kind, res.RC, res.TimedOut, res.Output)
		raw[p.Kind+":"+p.Target] = clip(res.Output, 600)
		if len(res.Recap) > 0 {
			recaps = append(recaps, res.Recap)
		}
		out = append(out, p)
	}
	ev := map[string]any{"raw": raw}
	if pyFound == "none" && len(checks) > 0 {
		ev["note"] = "目标机无 Python，检查项未能执行"
	}
	return out, ev, recaps
}

// accessPointVerdict 接入点可达性三态判定（纯函数，回归有断言）。
// 返回 (诊断列表, 是否致命)。三态语义（2026-09-21 用户评审）：
//
//	200        → 可达，info 留痕放行
//	FAIL/非200 → 真不可达（curl+wget 双工具都探不通）→ fatal：装机之前就地拦住，
//	             不许「装完才发现回连不上、流程卡死在等回报」
//	NO_TOOL/空 → 未核实（工具缺失 / 输出异常）→ 仅告警。「没查到」不是「有问题」，
//	             把工具缺失当成不可达会误伤所有精简系统（alpine 常无 curl）
func accessPointVerdict(connCode, connTool, consoleURL string) ([]diag, bool) {
	switch {
	case connCode == "":
		return []diag{warnDiag("access_point_unverified",
			"目标机侧未返回接入点探测结论（命令执行异常）——回连可达性未核实，不当作通过",
			"点「↻ 重试该步」重跑；若复现，上机手工验证 "+consoleURL+"/api/onboard/templates")}, false
	case connCode == "NO_TOOL":
		return []diag{warnDiag("access_point_unverified",
			"目标机没有 curl 也没有 wget，接入点可达性未能核实——「没查到」而不是「有问题」",
			"人工在目标机执行 curl -s -o /dev/null -w %{http_code} --connect-timeout 5 "+consoleURL+"/api/onboard/templates 核实（200 才算通）")}, false
	case connCode != "200":
		return []diag{fatalDiag("access_point_unreachable",
			fmt.Sprintf("Agent 接入点 %s 从目标机不可达（探测结论 %s，工具 %s）——装好后 Agent 无法回连平台，流程会卡在「等待回报」。装机之前就地拦截", consoleURL, connCode, connTool),
			"两类目标环境对号入座：① 本机 Docker 容器目标 → 接入点填 http://host.docker.internal:8080（或留空走平台默认）；② 跨网段真实主机 → 平台侧跑 deploy/docker/start-crossnet-tunnel.sh 建反向隧道，接入点填 http://127.0.0.1:<远端端口>；同内网主机 → 填平台内网可达地址。修好后点「↻ 重试该步」；确认要带病接入走「强制通过」（留痕审计）")}, true
	default:
		return []diag{infoDiag("access_point_reachable",
			fmt.Sprintf("Agent 接入点 %s 从目标机可达（HTTP 200，工具 %s）——装好后 Agent 可回连平台", consoleURL, connTool))}, false
	}
}

// ---------------- 接入点兜底自诊断 ----------------
// 两类目标环境（2026-09-21 用户要求双兼容）：本机 Docker 容器 / 跨网段真实主机。
// 配置的接入点探不通时，从目标机把各形态的候选地址实测一遍，把「哪个地址可达」
// 直接告诉操作者——门禁不放松（仍是 fatal），但修复路径从"猜"变成"选"。

// accessPointCandidate 兜底候选接入点
type accessPointCandidate struct {
	URL   string
	Scene string // 面向操作者的场景说明
	Tool  string // 实测可达时用的探测工具（留证据）
}

// accessPointCandidates 按平台两类部署形态给出的候选清单（纯函数，回归有断言）。
// 台账现值与候选重复时剔除——再探一遍配置过且已失败的地址没有信息量
func accessPointCandidates(configured string) []accessPointCandidate {
	cfg := strings.TrimSpace(configured)
	all := []accessPointCandidate{
		{"http://host.docker.internal:8080", "本机 Docker 容器目标（经宿主机端口映射回连平台）", ""},
		{"http://l0-console:8080", "与平台同一 Docker 网络的目标机（容器 DNS 直连）", ""},
	}
	out := []accessPointCandidate{}
	for _, c := range all {
		if c.URL != cfg {
			out = append(out, c)
		}
	}
	return out
}

// candidateProbeCmd 生成候选探测命令：与主接入点同一条 curl→wget 探测链
// （同一套"缺工具不误报"语义），输出 CAND:<url>:<code>:<tool> 供 parseCandidateProbe 解析
func candidateProbeCmd(urls []string) string {
	return fmt.Sprintf(`sh -c 'for U in %[1]s; do C=$(curl -s -o /dev/null -w "%%{http_code}" --connect-timeout 5 "$U/api/onboard/templates" 2>/dev/null); RC=$?; if [ "$RC" = 0 ] && [ "$C" != "000" ]; then echo "CAND:$U:$C:curl"; elif command -v wget >/dev/null 2>&1; then if wget -q -O /dev/null -T 5 "$U/api/onboard/templates" 2>/dev/null; then echo "CAND:$U:200:wget"; else echo "CAND:$U:FAIL:wget"; fi; else echo "CAND:$U:NO_TOOL:none"; fi; done'`,
		strings.Join(urls, " "))
}

// parseCandidateProbe 解析 CAND 行，只回收 200（可达）的候选。
// URL 自带冒号（http://host:8080），所以按"最后两个冒号"切：CAND:<url>:<code>:<tool>
func parseCandidateProbe(output string) []accessPointCandidate {
	out := []accessPointCandidate{}
	for _, ln := range strings.Split(output, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "CAND:") {
			continue
		}
		rest := ln[len("CAND:"):]
		i1 := strings.LastIndex(rest, ":")
		if i1 < 0 {
			continue
		}
		tool := rest[i1+1:]
		head := rest[:i1]
		i2 := strings.LastIndex(head, ":")
		if i2 < 0 {
			continue
		}
		if code, url := head[i2+1:], head[:i2]; code == "200" {
			out = append(out, accessPointCandidate{URL: url, Tool: tool})
		}
	}
	return out
}

// ---------------- 接入点自动接管（2026-09-22 用户拍板：只给 IP + 类型 + 凭据） ----------------

// autoResolveAccessPoint 探路发现接入点不可达时的平台自愈：
// ① 从目标机实测候选地址（host.docker.internal / l0-console:8080），可达即自动写入台账并采用；
// ② 非 docker（host/auto）→ 平台用台账凭据自动建反向隧道，再从目标机实测 127.0.0.1:18080；
// ③ 全部不通 → 返回空串，由调用方按一句话结论 fail。
// 返回（最终接入点、诊断、证据）。host 场景候选探测也执行——真实主机若恰好与平台同内网，直连地址同样能被自动选中
func autoResolveAccessPoint(st *stepRunState, inv string, catDB *storepkg.DB, flowID int64,
	cred *sshCred, consoleURL, connCode string) (string, []diag, map[string]any) {
	diags := []diag{}
	evidence := map[string]any{"probed_console_url": consoleURL, "conn_code": connCode}

	res, _ := catDB.GetResource(flowResourceID(catDB, flowID))
	kind := ""
	if res != nil {
		kind = strings.TrimSpace(res.TargetKind)
	}

	// ① 候选地址实测：docker 与 auto 场景的通路；host 明确登记也不放弃——
	//    真实主机也可能与平台同内网，直连地址试一次代价极小
	if cands := accessPointCandidates(consoleURL); len(cands) > 0 && connCode != "NO_TOOL" {
		st.setPhase("ap_auto_candidates")
		urls := make([]string, 0, len(cands))
		for _, c := range cands {
			urls = append(urls, c.URL)
		}
		ctxFB, cancelFB := context.WithTimeout(context.Background(), phaseBudget(st.spec, budgetPing))
		fb := runAnsibleStream(ctxFB, inv, st.taskHook(catDB), "-m", "shell", "-a", candidateProbeCmd(urls))
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
			if res != nil {
				res.AgentConsoleURL = url
				if err := catDB.UpsertResource(res); err != nil {
					diags = append(diags, warnDiag("access_point_adopt_failed",
						"候选地址 "+url+" 可达，但写台账失败："+err.Error(), ""))
				}
			}
			diags = append(diags, infoDiag("access_point_auto_selected",
				fmt.Sprintf("平台已自动选定接入点 %s（目标机实测 HTTP 200，工具 %s）并写入资源台账——无需人工填写", url, tool)))
			return url, diags, evidence
		}
	}

	// ② 反向隧道自动建立（host / 未登记类型；docker 明确登记时不走隧道）
	if kind != "docker" {
		if cred == nil || cred.Pass == "" {
			diags = append(diags, warnDiag("tunnel_no_credential",
				"候选地址不可达，且资源缺少 SSH 凭据——平台无法自动建反向隧道", ""))
		} else {
			st.setPhase("ap_auto_tunnel")
			started, err := startReverseTunnel(res.ID, cred)
			if err != nil {
				diags = append(diags, warnDiag("tunnel_start_failed",
					"平台自动建反向隧道失败："+err.Error(), ""))
			} else {
				// 隧道刚建立，给 sshd 监听一点落地时间再从目标机实测
				time.Sleep(2 * time.Second)
				tunnelURL := fmt.Sprintf("http://127.0.0.1:%d", tunnelRemotePort())
				ctxT, cancelT := context.WithTimeout(context.Background(), phaseBudget(st.spec, budgetPing))
				tr := runAnsibleStream(ctxT, inv, st.taskHook(catDB), "-m", "shell", "-a", candidateProbeCmd([]string{tunnelURL}))
				cancelT()
				tunnelOK := ""
				for _, c := range parseCandidateProbe(tr.Output) {
					if c.URL == tunnelURL {
						tunnelOK = c.Tool
					}
				}
				evidence["tunnel"] = map[string]any{
					"started": started, "url": tunnelURL, "reachable": tunnelOK != "",
				}
				if tunnelOK != "" {
					if res != nil {
						res.AgentConsoleURL = tunnelURL
						if err := catDB.UpsertResource(res); err != nil {
							diags = append(diags, warnDiag("access_point_adopt_failed",
								"隧道可达，但写台账失败："+err.Error(), ""))
						}
					}
					note := "平台已自动建立反向隧道"
					if !started {
						note = "平台复用已在跑的反向隧道"
					}
					diags = append(diags, infoDiag("access_point_auto_selected",
						fmt.Sprintf("%s：目标机 127.0.0.1:%d → 平台 %d，实测 HTTP 200（工具 %s），接入点已写入台账——无需人工填写",
							note, tunnelRemotePort(), platformPort(), tunnelOK)))
					return tunnelURL, diags, evidence
				}
				diags = append(diags, warnDiag("tunnel_established_but_unreachable",
					"反向隧道已建立，但目标机实测 127.0.0.1:"+fmt.Sprint(tunnelRemotePort())+" 仍不可达——目标机侧监听未生效或 sshd 转发被禁",
					"核对目标机 sshd_config 的 AllowTcpForwarding 是否为 yes；点「↻ 重试该步」让平台再试"))
			}
		}
	}

	// ③ 全部不通 → 一句话结论（原始证据已在 evidence，界面「原始输出」可复核）
	evidence["resolved"] = false
	return "", diags, evidence
}

// firstReachableCandidate 候选里第一个实测可达的地址（保持声明顺序 = 优先级）
func firstReachableCandidate(cands []accessPointCandidate, reachable map[string]string) (string, string, bool) {
	for _, c := range cands {
		if tool, ok := reachable[c.URL]; ok {
			return c.URL, tool, true
		}
	}
	return "", "", false
}

// ---------------- 主机探路作业 ----------------

// ansibleProbeJob 联通性测试 + 端口核对 + 事实采集 + 能力可行性检查，
// 自动回填台账并回报 preflight_host（含原「能力探路」的检查内容）。
// 全过程信息回传（2026-09-21 用户要求）：每个 ansible 任务的状态/变更/输出/失败原因
// 随进度事件回到界面；终态同样带证据——不存在"黑盒成功"。
// 任何一步失败 → 步骤 fail（全量原文 + 分级诊断留痕），人工可重试/强制。
func ansibleProbeJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	st := newStepRunState(catDB, flowID, step, cred)
	defer st.startHeartbeat(catDB)()
	myAttempt := st.attempt
	hook := st.taskHook(catDB)

	// fail 收口：已被超时重试接管的旧尝试，其结果一律丢弃——
	// 否则旧作业晚到的失败会把新尝试的进展覆盖掉
	fail := func(summary string, detail map[string]any, ds []diag) {
		if !jobStillCurrent(catDB, flowID, step.ID, myAttempt) {
			return
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["attempt"] = myAttempt
		detail["attempts"] = st.endAttempt("fail", summary)
		detail["tasks"] = st.tasksSnapshot()
		failStep(catDB, agentStore, flowID, step, "ansible 探路失败", summary, detail, cred, ds)
	}

	inv, cleanup, err := writeInventory(cred)
	if err != nil {
		fail("生成 ansible inventory 失败："+err.Error(), nil,
			[]diag{fatalDiag("inventory_write_failed", "生成 ansible inventory 失败："+err.Error(),
				"检查平台容器内 /tmp 是否可写")})
		return
	}
	defer cleanup()

	// ① 联通性测试（预算 = RunSec × budgetPing，随配置走）
	st.setPhase("ping")
	pingBudget := phaseBudget(st.spec, budgetPing)
	ctxPing, cancelPing := context.WithTimeout(context.Background(), pingBudget)
	defer cancelPing()
	rp := runAnsibleStream(ctxPing, inv, hook, "-m", "ping")
	if rp.TimedOut {
		fail(fmt.Sprintf("联通性测试超时（%ds 未返回）", budgetSec(pingBudget)), map[string]any{
			"phase": "ping", "evidence": probePhaseEvidence("ping", rp),
		}, []diag{fatalDiag("ansible_phase_timeout",
			fmt.Sprintf("ansible ping 超过 %ds 未返回（预算由 preflight_host.run_sec 推导）", budgetSec(pingBudget)),
			"目标机不可达或响应极慢：核对网络连通性与目标机负载后，点「↻ 重试该步」")})
		return
	}
	if rp.RC != 0 {
		d := classifySSHFailure(rp.Output, rp.RC)
		fail("联通性测试失败："+d.Message, map[string]any{
			"phase": "ping", "evidence": probePhaseEvidence("ping", rp),
		}, withPortSource(cred, []diag{d}))
		return
	}

	// ② sshd 端口核对：连上了 ≠ 连对了（"端口登记 22 / 实际 22022" 只能靠这里暴露）
	st.setPhase("port_check")
	portDiags, portEvidence := sshdPortCheck(inv, st.spec, cred, hook)
	portDiags = withPortSource(cred, portDiags)
	st.setDiags(portDiags) // 端口异常提前进进度事件：不用等探路跑完才知道

	// ③ 事实采集（setup：OS/架构/内核/内存/磁盘一次拿全）
	st.setPhase("setup")
	treeDir, err := os.MkdirTemp("", "l0-facts-*")
	if err != nil {
		fail("创建 facts 目录失败："+err.Error(), nil,
			[]diag{fatalDiag("facts_dir_failed", "创建 facts 临时目录失败："+err.Error(), "检查平台容器内 /tmp 是否可写")})
		return
	}
	defer os.RemoveAll(treeDir)
	setupBudget := phaseBudget(st.spec, budgetSetup)
	ctxSetup, cancelSetup := context.WithTimeout(context.Background(), setupBudget)
	defer cancelSetup()
	rs := runAnsibleStream(ctxSetup, inv, hook, "-m", "setup", "--tree", treeDir)
	if rs.TimedOut {
		fail(fmt.Sprintf("事实采集超时（%ds 未返回）", budgetSec(setupBudget)), map[string]any{
			"phase": "setup", "evidence": probePhaseEvidence("setup", rs),
		}, append(portDiags, fatalDiag("ansible_phase_timeout",
			fmt.Sprintf("ansible setup 超过 %ds 未返回（预算由 preflight_host.run_sec 推导）", budgetSec(setupBudget)),
			"目标机负载过高或 facts 采集被阻塞；确认后点「↻ 重试该步」")))
		return
	}
	if rs.RC != 0 {
		ev := probePhaseEvidence("setup", rs)
		ev["ping_output"] = clip(rp.Output, 1500)
		fail("事实采集失败（ansible setup rc="+fmt.Sprint(rs.RC)+"）",
			map[string]any{"phase": "setup", "evidence": ev},
			append(portDiags, classifySSHFailure(rs.Output, rs.RC)))
		return
	}

	// ④ 解析 facts + 磁盘兜底采集：alpine 等精简系统没有 lsblk，setup 的 ansible_mounts
	//    会是空的，disk_free_gb 会恒 0。df -k 各发行版（含 busybox）都支持，独立采一次覆盖
	facts, err := parseFactsTree(treeDir)
	if err != nil {
		fail("解析 facts 失败："+err.Error(), map[string]any{
			"phase": "parse", "evidence": probePhaseEvidence("setup", rs),
		}, append(portDiags, fatalDiag("facts_parse_failed", "解析 setup 输出失败："+err.Error(),
			"setup --tree 未产出可解析文件；目标机可能不可达或 facts 被裁剪（gather_subset）")))
		return
	}
	st.setPhase("df")
	ctxDisk, cancelDisk := context.WithTimeout(context.Background(), phaseBudget(st.spec, budgetDisk))
	defer cancelDisk()
	rd := runAnsibleStream(ctxDisk, inv, hook, "-m", "shell", "-a", "df -k / | tail -1")
	diskFromDF := false
	if fields := strings.Fields(strings.TrimSpace(rd.Output)); len(fields) >= 4 {
		if kb, err := strconv.ParseFloat(fields[len(fields)-4], 64); err == nil && kb > 0 {
			facts.DiskFreeGB = kb / 1048576
			diskFromDF = true
		}
	}

	// ⑤ 环境预检（装机前置条件，探路阶段一次查完）：Python 依赖 / 平台接入点可达性 / 时间同步。
	//    「装完才发现 Agent 回连不通、流程卡在等回报」是真实踩过的坑——可达性必须在这里提前暴露
	st.setPhase("env_check")
	envRes, _ := catDB.GetResource(flowResourceID(catDB, flowID))
	consoleURL := l0AgentURL()
	if envRes != nil && strings.TrimSpace(envRes.AgentConsoleURL) != "" {
		consoleURL = strings.TrimSpace(envRes.AgentConsoleURL)
	}
	envCmd := fmt.Sprintf(
		// 探测链（2026-09-21 用户评审后加固）：curl 失败/缺失时回落 wget——
		// alpine 等精简系统常没有 curl，只认 curl 会把「工具不在」误报成「不可达」。
		// 两个工具都给不出 200 才算真不可达；一个都没有记 NO_TOOL（未核实，不阻断）
		`sh -c 'command -v python3 >/dev/null 2>&1 && echo PY:$(python3 -V 2>&1) || (command -v python >/dev/null 2>&1 && echo PY:$(python -V 2>&1) || echo PY:NONE); echo EPOCH:$(date +%%s); C=$(curl -s -o /dev/null -w "%%{http_code}" --connect-timeout 5 %s/api/onboard/templates 2>/dev/null); RC=$?; if [ "$RC" = 0 ] && [ "$C" != "000" ]; then echo CONN:$C TOOL:curl; elif command -v wget >/dev/null 2>&1; then if wget -q -O /dev/null -T 5 %s/api/onboard/templates 2>/dev/null; then echo CONN:200 TOOL:wget; else echo CONN:FAIL TOOL:wget; fi; else echo CONN:NO_TOOL TOOL:none; fi%s'`,
		consoleURL, consoleURL, glibcProbeShell())
	ctxEnv, cancelEnv := context.WithTimeout(context.Background(), phaseBudget(st.spec, budgetDisk))
	defer cancelEnv()
	re := runAnsibleStream(ctxEnv, inv, hook, "-m", "shell", "-a", envCmd)
	envDiags := []diag{}
	envEvidence := map[string]any{
		"console_url": consoleURL, "rc": re.RC, "output": clip(re.Output, 1200),
	}
	envOut := parseEnvCheck(re.Output, time.Now().Unix())
	glibcVer := parseGlibcVersion(re.Output)
	envEvidence["glibc"] = glibcVer
	pyFound, connCode, connTool, epochSkew := envOut.python, envOut.connCode, envOut.connTool, envOut.skewSec
	envEvidence["python"] = pyFound
	envEvidence["access_point_code"] = connCode
	envEvidence["access_point_tool"] = connTool
	envEvidence["time_skew_sec"] = epochSkew
	if pyFound == "none" {
		envDiags = append(envDiags, warnDiag("env_no_python",
			"目标机未找到 Python（python3/python 均无）——ansible 的 file/copy 模块依赖目标机 Python，安装环节会失败",
			"先在目标机安装 python3（CentOS：yum install -y python3；Ubuntu：apt install -y python3），再重试"))
	}
	apDiags, apFatal := accessPointVerdict(connCode, connTool, consoleURL)
	envDiags = append(envDiags, apDiags...)
	// 门禁语义：真不可达 → 平台先自动接管（2026-09-22：候选自动选定 + 反向隧道自动建立），
	// 全部通路都不通才 fail——不再把"平台自己能解决的事"甩给运维
	if apFatal {
		resolvedURL, resDiags, resEvidence := autoResolveAccessPoint(st, inv, catDB, flowID, cred, consoleURL, connCode)
		envDiags = append(envDiags, resDiags...)
		envEvidence["auto_resolve"] = resEvidence
		if resolvedURL != "" {
			// 自动选定成功：换地址继续流程（可达性已由目标机实测 HTTP 200 确认）
			consoleURL = resolvedURL
			envEvidence["console_url"] = consoleURL
			envEvidence["access_point_code"] = "200"
		} else {
			// 一句话结论；候选/隧道明细与原始输出都在 evidence，界面「原始输出」可复核
			fail(fmt.Sprintf("目标机无法回连平台（已自动尝试直连候选地址与反向隧道，均不可达）——请确认目标类型（docker 容器 / 真实主机）与网络策略后点「↻ 重试该步」"),
				map[string]any{"phase": "env_check", "evidence": map[string]any{"env_check": envEvidence}},
				envDiags)
			return
		}
	}
	if epochSkew > 300 {
		envDiags = append(envDiags, warnDiag("time_skew",
			fmt.Sprintf("目标机与平台时间偏差 %d 秒（>300s）——影响指标时间对齐与心跳判定", epochSkew),
			"在目标机校准时间（chronyd/ntpd）后重试探路"))
	}

	// ⑥ 能力可行性检查（2026-09-21：原「能力探路」环节并入本步骤）：按所选能力的
	//    参数声明，从目标机侧核对目标可达性。放在装机之前——装完才发现采不到，
	//    要多付一次"装 + 卸"的代价，而这一半的答案在探路阶段就能拿到
	st.setPhase("ability_check")
	flowRow, _ := catDB.GetFlow(flowID)
	probes := []abilityProbe{}
	if flowRow != nil {
		probes = abilityProbeChecks(loadAbilities(onboardCfg, integrationsDir), flowRow.Abilities, flowRow.Params())
	}
	abilityDiags := []diag{}
	abilityEvidence := map[string]any{}
	abilityRecaps := []map[string]int{}
	if len(probes) == 0 {
		abilityEvidence["note"] = "所选能力未声明可自动核对的检查项（params.yaml 的 probe 绑定），本步只做主机事实采集"
	} else {
		probes, abilityEvidence, abilityRecaps = execAbilityChecks(inv, st, hook, probes, pyFound)
		abilityEvidence["checks"] = probes
	}
	blockerFail := []abilityProbe{}
	for _, p := range probes {
		switch {
		case p.Verdict == "fail" && p.Level == "blocker":
			blockerFail = append(blockerFail, p)
			abilityDiags = append(abilityDiags, fatalDiag("ability_unreachable",
				fmt.Sprintf("能力 %s 的检查未通过：%s 不可达（%s）", p.Ability, p.Target, firstNonEmpty(p.Note, "目标机侧实测失败")),
				"先在目标机一侧确认地址/端口/网络策略；确认是临时不可达时点「↻ 重试该步」，确认要带病接入时走「强制通过」并填写理由（留痕审计）"))
		case p.Verdict == "unknown":
			abilityDiags = append(abilityDiags, warnDiag("ability_unverified",
				fmt.Sprintf("能力 %s 的检查项 %s 未能得出结论：%s", p.Ability, p.Target, firstNonEmpty(p.Note, "检查未执行")),
				"这是「没查到」而不是「有问题」——请人工在目标机核实后重试；平台不会把未核实写成通过"))
		}
	}
	abilityDiags = append(abilityDiags, infoDiag("ability_checked",
		fmt.Sprintf("能力可行性检查：%d 项（通过 %d / 未通过 %d / 未核实 %d / 交由 Agent 侧验证 %d）；未通过项在装机之前就地拦住，交由 Agent 侧验证的是需要凭据的账号类检查",
			len(probes), countProbeVerdict(probes, "ok"), len(blockerFail),
			countProbeVerdict(probes, "unknown"), countProbeVerdict(probes, "delegated"))))

	// 门禁语义：能力可行性不过 → 本步 fail（就在装机之前拦住，不生成配置、不下发）
	if len(blockerFail) > 0 {
		b := blockerFail[0]
		fail(fmt.Sprintf("能力可行性检查未通过：%s 目标 %s 不可达", b.Ability, b.Target),
			map[string]any{
				"phase": "ability_check", "ability": map[string]any{"checks": probes, "blocker_fail": blockerFail},
			},
			append(append([]diag{}, portDiags...), append(envDiags, abilityDiags...)...))
		return
	}

	// ⑦ 缺字段不许静默填 0：0 值必须被标注成"未采到"，否则台账里就留下了假数据
	factDiags := []diag{}
	if facts.MemTotalMB <= 0 {
		factDiags = append(factDiags, warnDiag("facts_mem_missing",
			"未采集到内存总量（ansible_memtotal_mb 缺失），台账记为 0 表示未采到",
			"请人工在目标机执行 free -m 核对后修正台账"))
	}
	if facts.DiskFreeGB <= 0 {
		factDiags = append(factDiags, warnDiag("facts_disk_missing",
			"未采集到根分区可用磁盘（ansible_mounts 无 / 且 df 兜底也未取到）",
			"请人工在目标机执行 df -BG / 核对；disk_free_gb=0 表示未采到，不代表真实为 0"))
	}
	diags := append(append(append(append([]diag{}, portDiags...), envDiags...), abilityDiags...), factDiags...)
	st.setDiags(diags)

	// ⑧ 回填台账（实测值权威）
	osName := normOS(facts.System)
	arch := normArch(facts.Machine)
	probe := map[string]any{
		"os": osName, "arch": arch, "kernel": facts.Kernel,
		"mem_mb": facts.MemTotalMB, "disk_free_gb": facts.DiskFreeGB,
		"mem_measured":  facts.MemTotalMB > 0,
		"disk_measured": diskFromDF || facts.DiskFreeGB > 0,
		"distribution":  facts.Distribution + " " + facts.DistVersion,
		"probed_by":     "ansible_platform",
		"probed_at":     time.Now().Format("2006-01-02 15:04:05"),
		"probe_port":    cred.Port,
		"port_source":   cred.PortSource,
		"python":        pyFound,
		"console_url":   consoleURL,
		"console_reach": connCode,
		// libc 版本：自动选版的兼容判据（含 glibc 依赖组件的版本在 musl 目标上不可装）
		"glibc":         glibcVer,
		"time_skew_sec": epochSkew,
		// 能力可行性结论只留计数进台账：逐项结论属流水线证据（见步骤 detail.ability），
		// 台账是"这台机器是什么"的档案，不该塞一次探路的明细
		"ability_checked": countProbeVerdict(probes, "ok"),
		"ability_failed":  len(blockerFail),
		"ability_unknown": countProbeVerdict(probes, "unknown"),
		"warn_count":      countWarn(diags),
		// 资源信息反哺（2026-09-22 用户拍板：全量采集保持现状，结构化落库供资源档案用）。
		// 空值就存空——台账要"未采到"可见，不要假 0
		"hostname":       facts.Hostname,
		"fqdn":           facts.FQDN,
		"cpu_vcpus":      facts.CPUvCPUs,
		"cpu_model":      facts.CPUModel,
		"ipv4_all":       facts.IPv4s,
		"ipv4_default":   facts.DefaultIPv4,
		"virt":           strings.TrimSpace(facts.VirtType + " " + facts.VirtRole),
		"product":        strings.TrimSpace(facts.Vendor + " " + facts.ProductName),
		"python_version": facts.PythonVersion,
		"uptime_sec":     int64(facts.UptimeSec),
	}
	pj, _ := json.Marshal(probe)
	if err := catDB.UpdateResourceProbe(flowResourceID(catDB, flowID), string(pj), osName, arch, facts.Kernel); err != nil {
		fail("探路回写台账失败："+err.Error(), map[string]any{"probe": probe},
			append(diags, fatalDiag("probe_persist_failed", "探路结果回写资源台账失败："+err.Error(),
				"检查目录库（PostgreSQL）连接与写入是否正常")))
		return
	}
	reason := fmt.Sprintf("ansible 实测 OS=%s(%s) arch=%s kernel=%s mem=%.0fMB disk_free=%.1fGB",
		osName, probe["distribution"], arch, facts.Kernel, facts.MemTotalMB, facts.DiskFreeGB)
	if n := len(probes); n > 0 {
		reason += fmt.Sprintf(" · 能力可行性 %d 项（未通过 %d / 未核实 %d）",
			n, len(blockerFail), countProbeVerdict(probes, "unknown"))
	}
	if n := countWarn(diags); n > 0 {
		reason += fmt.Sprintf(" ⚠ %d 项待复核（已展开在步骤告警，请人工确认）", n)
	}
	// ⑨ 成功回报：任务清单（逐条状态）+ recap + 各阶段原文 + 尝试记录，一起回到界面
	extra := map[string]any{
		"source":    "ansible_probe",
		"conn":      credState(cred),
		"facts":     probe,
		"ping":      "ok",
		"diagnosis": diagJSON(diags),
		"tasks":     st.tasksSnapshot(),
		"recap":     mergeRecaps(append([]map[string]int{rp.Recap, rs.Recap, rd.Recap}, abilityRecaps...)...),
		"attempt":   myAttempt,
		"attempts":  st.endAttempt("ok", reason),
		// 能力可行性逐项结论（含未通过/未核实/委派给 Agent 侧的），界面按项呈报
		"ability": map[string]any{
			"checks": probes, "blocker_fail": blockerFail,
			"counts": map[string]int{
				"total": len(probes), "ok": countProbeVerdict(probes, "ok"),
				"fail": len(blockerFail), "unknown": countProbeVerdict(probes, "unknown"),
				"delegated": countProbeVerdict(probes, "delegated"),
			},
		},
		"evidence": map[string]any{
			"ping_output": clip(rp.Output, 3000),
			// setup 原文是几百 KB 的 facts dump（实测 297KB），结构化结果已单列在 facts 字段；
			// 这里只留一段指纹，逐任务原文在 tasks[].lines 里
			"setup_output":  clip(rs.Output, 2500),
			"df_output":     clip(rd.Output, 1000),
			"df_rc":         rd.RC,
			"df_used":       diskFromDF,
			"port_check":    portEvidence,
			"env_check":     envEvidence,
			"ability_check": abilityEvidence,
		},
	}
	if msg := autoReportStep(catDB, agentStore, flowID, step.ID, reason, extra); msg != "" {
		fail("探路回报被拒："+msg, map[string]any{"probe": probe, "tasks": st.tasksSnapshot()},
			append(diags, fatalDiag("probe_report_rejected", "探路回报被拒："+msg,
				"步骤可能已被人工重置；点「↻ 重试该步」重新执行")))
		return
	}
	addAudit("ansible 探路完成", step.ID, "接入中心", reason)
}

// facts 目标机关键事实（setup 采集）。
// 前半组是接入门禁用的判定字段；后半组（Hostname 起）是资源信息反哺档案字段——
// 2026-09-22 用户拍板：全量采集保持现状不改速度，结构化落库供资源台账后续使用
type facts struct {
	System       string
	Distribution string
	DistVersion  string
	Machine      string
	Kernel       string
	MemTotalMB   float64
	DiskFreeGB   float64
	// 资源信息反哺字段
	Hostname      string
	FQDN          string
	CPUvCPUs      int
	CPUModel      string
	IPv4s         []string
	DefaultIPv4   string
	VirtType      string
	VirtRole      string
	ProductName   string
	Vendor        string
	PythonVersion string
	UptimeSec     float64
}

// parseFactsTree 读取 ansible setup --tree 输出（每主机一个 JSON 文件）
func parseFactsTree(treeDir string) (*facts, error) {
	entries, err := os.ReadDir(treeDir)
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("tree 目录为空")
	}
	raw, err := os.ReadFile(filepath.Join(treeDir, entries[0].Name()))
	if err != nil {
		return nil, err
	}
	var doc struct {
		AnsibleFacts map[string]any `json:"ansible_facts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	f := doc.AnsibleFacts
	get := func(k string) string {
		if v, ok := f[k].(string); ok {
			return v
		}
		return ""
	}
	out := &facts{
		System:       get("ansible_system"),
		Distribution: get("ansible_distribution"),
		DistVersion:  get("ansible_distribution_version"),
		Machine:      get("ansible_machine"),
		Kernel:       get("ansible_kernel"),
		// 资源信息反哺字段：全部来自同一次 setup 树，零额外开销
		Hostname:      get("ansible_hostname"),
		FQDN:          get("ansible_fqdn"),
		VirtType:      get("ansible_virtualization_type"),
		VirtRole:      get("ansible_virtualization_role"),
		ProductName:   get("ansible_product_name"),
		Vendor:        get("ansible_system_vendor"),
		PythonVersion: get("ansible_python_version"),
	}
	if v, ok := f["ansible_processor_vcpus"].(float64); ok {
		out.CPUvCPUs = int(v)
	}
	if procs, ok := f["ansible_processor"].([]any); ok && len(procs) > 1 {
		// processor[0] 通常是计数说明，[1] 起才是型号；取第一条非空型号
		for _, p := range procs[1:] {
			if s, ok := p.(string); ok && s != "" {
				out.CPUModel = s
				break
			}
		}
	}
	if ips, ok := f["ansible_all_ipv4_addresses"].([]any); ok {
		for _, ip := range ips {
			if s, ok := ip.(string); ok && s != "" {
				out.IPv4s = append(out.IPv4s, s)
			}
		}
	}
	if dip, ok := f["ansible_default_ipv4"].(map[string]any); ok {
		out.DefaultIPv4, _ = dip["address"].(string)
	}
	if up, ok := f["ansible_uptime_seconds"].(float64); ok {
		out.UptimeSec = up
	}
	if v, ok := f["ansible_memtotal_mb"].(float64); ok {
		out.MemTotalMB = v
	}
	if mounts, ok := f["ansible_mounts"].([]any); ok {
		for _, m := range mounts {
			mm, _ := m.(map[string]any)
			if mp, _ := mm["mountpoint"].(string); mp == "/" {
				if av, ok := mm["size_available"].(float64); ok {
					out.DiskFreeGB = av / 1e9
				}
			}
		}
	}
	if out.System == "" || out.Machine == "" {
		return nil, fmt.Errorf("facts 缺少 system/machine 字段")
	}
	return out, nil
}

// flowResourceID 反查流水线资源 ID（探路回填用）
func flowResourceID(catDB *storepkg.DB, flowID int64) string {
	f, err := catDB.GetFlow(flowID)
	if err != nil || f == nil {
		return ""
	}
	return f.ResourceID
}

// ---------------- 安装作业 ----------------

// sagentConfigTpl 安装时渲染的目标机 SAgent 配置（host_metrics 地基能力 + L0 回传通道）。
// 资源身份 = 流水线资源 ID（与 pick_agent 的预期 agent_id 对齐）；
// 监听端口来自 AGENT_HTTP_PORT——与 SD 登记、卸载端口核对同一来源，避免三处各说一套。
// 采集机（远程采集承载端）三处不同：resource.type=proxy（R1 采集机身份，决定漂移候选资格）、
// l0_console.heartbeat_interval=10s（R5 短心跳：故障检测从 150s 压到 ~40s）、
// 以及可选的 pool/region（池归属＝漂移边界，同池采集机互为承接方）。
const sagentConfigTpl = `# 由 L0 接入中心 ansible 执行器渲染下发
server:
  listen: ":%s"

resource:
  id: "%s"
  type: "%s"
  business_system: "onboarded"
  env: "prod"
  idc: "lab"
  cluster: "vm-host"
%s
plugins:
  host_metrics:
    enabled: true
    interval: 30s

l0_console:
  url: "%s"
  heartbeat_interval: %s
`

// renderSagentConfig 渲染目标机 SAgent 配置（占位符顺序：端口、资源 ID、资源类型、池归属块、接入点、心跳间隔）。
// collector=true 表示承载端是采集机（远程采集）：type=proxy + 10s 短心跳，使 R1 身份与 R5 提速同时生效。
// pool 仅对采集机且非空时写入——池是漂移边界，写空串等于"已登记空池"，会误导池内承接判定。
func renderSagentConfig(agentID, consoleURL string, collector bool, pool string) string {
	resType, hb := "host", "30s"
	poolBlock := ""
	if collector {
		resType, hb = "proxy", "10s"
		if pool != "" {
			poolBlock = "  pool: \"" + pool + "\"\n  region: \"" + pool + "\"\n"
		}
	}
	return fmt.Sprintf(sagentConfigTpl, cfgAgentHTTPPort, agentID, resType, poolBlock, consoleURL, hb)
}

// installPlaybookTpl 内置兜底模板：外置 data/playbooks/install.yml 缺失时回落用。
// 占位符 @BIN_SRC@/@BIN_SHA@/@CFG_SRC@/@DEST_HOME@ 由平台渲染（不用 Go text/template，
// 避免 {{ }} 与 ansible jinja 冲突）
const installPlaybookTpl = `- hosts: target
  gather_facts: false
  tasks:
    - name: 停止旧 Agent 进程（如存在）
      shell: "pkill -f 'bin/SAgent --config' || true"
      failed_when: false

    - name: 创建目录骨架
      file:
        path: "{{ item }}"
        state: directory
        mode: "0755"
      loop:
        - "@DEST_HOME@/SAgent/bin"
        - "@DEST_HOME@/SAgent/conf"
        - "@DEST_HOME@/SAgent/data"
        - "@DEST_HOME@/SAgent/logs"
        - "@DEST_HOME@/SAgent/run"

    - name: 分发 SAgent 二进制
      copy:
        src: "@BIN_SRC@"
        dest: "@DEST_HOME@/SAgent/bin/SAgent"
        mode: "0755"

    - name: 校验二进制 sha256（目标机实测）
      shell: "sha256sum @DEST_HOME@/SAgent/bin/SAgent"
      register: binsum

    - name: 断言 sha256 与版本清单一致
      command: /bin/true
      changed_when: false
      failed_when: binsum.stdout.split()[0] != "@BIN_SHA@"

    - name: 下发 SAgent 配置
      copy:
        src: "@CFG_SRC@"
        dest: "@DEST_HOME@/SAgent/conf/SAgent.yaml"
        mode: "0644"

    - name: 启动 Agent 并验证进程存活
      shell: |
        cd @DEST_HOME@/SAgent
        nohup ./bin/SAgent --config conf/SAgent.yaml >> logs/stdout.log 2>&1 &
        echo $! > run/SAgent.pid
        sleep 2
        kill -0 $(cat run/SAgent.pid)
`

// loadInstallPlaybook 优先加载外置 playbook（data/playbooks/install.yml），
// 缺失或不完整时回落内置模板——改安装动作只改文件，不动代码
func loadInstallPlaybook(binSrc, binSHA, cfgSrc, destHome string) string {
	tpl := installPlaybookTpl
	if b, err := os.ReadFile(filepath.Join("data", "playbooks", "install.yml")); err == nil &&
		strings.Contains(string(b), "@BIN_SRC@") {
		tpl = string(b)
	}
	return strings.NewReplacer(
		"@BIN_SRC@", binSrc, "@BIN_SHA@", binSHA,
		"@CFG_SRC@", cfgSrc, "@DEST_HOME@", destHome,
	).Replace(tpl)
}

// registerScrapeTarget 装机成功后把新主机的 /metrics 登记进 vmagent file_sd
// （共享卷挂载，vmagent 自动热加载，无需重启）。文件名=资源 ID，重复安装覆盖写。
// 端口取 AGENT_HTTP_PORT（与渲染给目标机的 listen 同源）
func registerScrapeTarget(resourceID, host string) (string, error) {
	dir := filepath.Join("data", "scrape-sd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	doc := []map[string]any{{
		"targets": []string{host + ":" + cfgAgentHTTPPort},
		"labels":  map[string]string{"resource_id": resourceID, "source": "onboard_flow"},
	}}
	b, _ := json.Marshal(doc)
	p := filepath.Join(dir, resourceID+".json")
	return p, os.WriteFile(p, b, 0o644)
}

// unregisterScrapeTarget 从 vmagent file_sd 里摘除该资源的抓取目标（卸载：不再抓一台
// 已经卸干净的机器，否则目标会长期处于 down 状态，把真实故障淹没在噪声里）。
// 幂等：文件本来就不存在（从未登记 / 已撤过）同样算成功。
// 返回 SD 文件路径，便于把"撤的是哪个文件"写进证据
func unregisterScrapeTarget(resourceID string) (string, error) {
	p := filepath.Join("data", "scrape-sd", resourceID+".json")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, nil
}

// ansibleInstallJob 按人工选定的版本安装 SAgent：分发二进制（平台本地版本库）→
// sha256 校验 → 写配置 → 启动 → 进程存活验证。
//
// 安装全过程回传（2026-09-21 用户要求）：playbook 每个 TASK 的名称/状态/变更/输出摘要
// 随进度事件实时回到界面，终态带逐任务结果、PLAY RECAP、阶段记录与全量原文——
// 运维看到的是"第 3/7 个任务 make dirs 失败：Permission denied: /home/deploy"，
// 而不是一坨需要自己翻的日志
func ansibleInstallJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	st := newStepRunState(catDB, flowID, step, cred)
	defer st.startHeartbeat(catDB)()
	myAttempt := st.attempt
	hook := st.taskHook(catDB)

	// fail 收口：已被超时重试接管的旧尝试不再写库（避免旧结果覆盖新尝试的进展）
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
		failStep(catDB, agentStore, flowID, step, "ansible 安装失败", summary, detail, cred, ds)
	}

	phases := []map[string]any{}

	// ① 版本取自人工选定事件（pick_version OK 事件 detail.tag）——安装只装人选的版本
	tag := pickedVersionTag(catDB, flowID)
	if tag == "" {
		fail("未找到人工选定版本（pick_version 无确认记录），拒绝安装", map[string]any{"reason": "no_human_pick"},
			[]diag{fatalDiag("no_human_pick", "未找到人工选定版本，拒绝安装",
				"先在「选择 SAgent 版本」环节由人工确认版本；平台不使用 latest 兜底")})
		return
	}
	ver := versionByTag(tag)
	if ver == nil {
		fail("版本清单中无 "+tag+"，拒绝安装", map[string]any{"tag": tag},
			[]diag{fatalDiag("version_not_in_catalog", "版本清单中无 "+tag+"，拒绝安装",
				"检查 data/versions.yaml 登记项，或重新人工选版")})
		return
	}

	// ② 平台本地版本库取二进制 + sha256 与清单核对
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
	// ② 就近取包（L1 Package Cache，架构 D5）：优先校验通过的缓存，失效回源 L0 重建，
	//    重建后仍不一致则拒绝安装（不允许降级用旧包）。配置 L1_PACKAGE_CACHE_URL 时经独立
	//    Package Cache 进程 HTTP resolve，缺省进程内 resolveInstallBinary（红线语义一致）。
	binPath, err := resolveInstallBinaryAny(tag)
	if err != nil {
		fail("L1 包缓存就近取包失败/拒绝（"+tag+"）", map[string]any{"tag": tag, "cache": packageCacheRoot()},
			[]diag{fatalDiag("binary_not_found", "L1 包缓存就近取包失败/拒绝："+err.Error(),
				"确认平台版本库有该版本的二进制（data/binaries/<tag>/SAgent 随镜像分发），且缓存/清单 sha256 一致；平台不作旧包降级兜底")})
		return
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		fail("平台版本库/缓存无该二进制（"+binPath+"），请随镜像分发", map[string]any{"tag": tag, "path": binPath},
			[]diag{fatalDiag("binary_not_found", "平台版本库/缓存无 "+tag+" 的二进制："+err.Error(),
				"把二进制放进 data/binaries/"+tag+"/ 并同步 Dockerfile 的 COPY（工作目录 + /opt/l0-seed 两处），或触发 /api/package-cache/sync")})
		return
	}
	sum := sha256.Sum256(binData)
	fileSHA := hex.EncodeToString(sum[:])
	if ver.SHA256 != "" && ver.SHA256 != fileSHA {
		fail("二进制 sha256 与版本清单不一致（清单登记的版本可能已被篡改或未同步），拒绝安装",
			map[string]any{"tag": tag, "catalog": ver.SHA256, "actual": fileSHA},
			[]diag{fatalDiag("binary_sha256_mismatch", "二进制 sha256 与版本清单不一致，拒绝安装",
				"清单登记="+ver.SHA256+"，实际="+fileSHA+"；重新分发二进制或修正 versions.yaml")})
		return
	}

	// ③ 本地临时产物：配置渲染 + playbook
	work, err := os.MkdirTemp("", "l0-install-*")
	if err != nil {
		fail("创建安装临时目录失败："+err.Error(), nil,
			[]diag{fatalDiag("install_tmp_failed", "创建安装临时目录失败："+err.Error(), "检查平台容器内 /tmp 是否可写")})
		return
	}
	defer os.RemoveAll(work)
	agentID := flow.AgentID
	if agentID == "" {
		agentID = flow.ResourceID
	}
	// 控制通道接入点：资源台账的下发值优先（按资源网络分区给可达接入点），无则用平台默认
	consoleURL := res.AgentConsoleURL
	if strings.TrimSpace(consoleURL) == "" {
		consoleURL = l0AgentURL()
	}
	// 采集机池归属：复用/重装既有采集机时沿用其已登记池标签（池是漂移边界，重装不得丢失）
	colPool := ""
	if isCollectorMode(flow.Mode) {
		if ca := agentStore.Get(agentID); ca != nil {
			colPool = ca.Labels["pool"]
		}
	}
	cfgContent := renderSagentConfig(agentID, consoleURL, isCollectorMode(flow.Mode), colPool)
	cfgPath := filepath.Join(work, "SAgent.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		fail("渲染 SAgent 配置失败："+err.Error(), nil,
			[]diag{fatalDiag("config_render_failed", "渲染 SAgent 配置失败："+err.Error(), "")})
		return
	}
	pbPath := filepath.Join(work, "install.yml")
	destHome := destHomeFor(cred)
	pb := loadInstallPlaybook(filepath.Join(mustAbs(binPath)), fileSHA, cfgPath, destHome)
	if err := os.WriteFile(pbPath, []byte(pb), 0o644); err != nil {
		fail("生成安装 playbook 失败："+err.Error(), nil,
			[]diag{fatalDiag("playbook_render_failed", "生成安装 playbook 失败："+err.Error(), "")})
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

	// ④ 语法门禁：playbook 先过 --syntax-check，语法错误在碰目标机前就拦下
	st.setPhase("syntax")
	st.flush(catDB, "平台 ansible 启动 · playbook 语法校验中（尚未触碰目标机）")
	synCmd := exec.Command("ansible-playbook", "-i", inv, pbPath, "--syntax-check")
	synCmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False")
	synOut, synErr := synCmd.CombinedOutput()
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synErr != nil],
		"output": clip(string(synOut), 1500),
	})
	if synErr != nil {
		fail("playbook 语法校验未过（未触碰目标机）", map[string]any{
			"syntax_output": clip(string(synOut), 2000), "phases": phases,
		}, []diag{fatalDiag("playbook_syntax_failed", "playbook 语法校验未过：检查 data/playbooks/install.yml",
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")})
		return
	}

	// ⑤ 执行安装（流式：每个 TASK 落一次进度事件，界面实时可见）
	st.setPhase("playbook")
	st.flush(catDB, "平台 ansible 执行中 · 安装 playbook 开始（逐任务回报）")
	// 兜底 ctx 带 execGrace：清扫器（按 install_agent.run_sec 判定，含自动重试）
	// 必须先到，否则执行层会抢在清扫器前面把步骤判死，重试额度白白浪费
	pbBudget := execBudget(st.spec, budgetPlaybook)
	ctx, cancel := context.WithTimeout(context.Background(), pbBudget)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, hook)
	rc := pr.RC

	evidence := map[string]any{
		"output": clip(pr.Output, 20000), "output_bytes": len(pr.Output),
		"truncated": len(pr.Output) > 20000,
	}
	// 失败任务的原行单独留一份：总原文被截断时，它仍是最直接的证据
	if ft := taskFailures(pr.Tasks); len(ft) > 0 {
		lines := []string{}
		for _, t := range ft {
			lines = append(lines, t.Lines...)
		}
		evidence["failed_task_lines"] = clip(strings.Join(lines, "\n"), 6000)
	}
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": rc, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})

	detail := map[string]any{
		"tag": tag, "sha256": fileSHA, "sha_verified": true, "version": ver.Version,
		"conn": credState(cred), "target": cred.target(),
		"config": cfgContent, "rc": rc, "timed_out": pr.TimedOut,
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases":     phases, "attempt": myAttempt,
		"evidence": evidence,
	}
	if pr.TimedOut {
		detail["attempts"] = st.endAttempt("timeout_run", fmt.Sprintf("playbook 超过 %ds 未返回", budgetSec(pbBudget)))
		fail(fmt.Sprintf("安装 playbook 超过 %ds 未返回（平台已终止）", budgetSec(pbBudget)), detail,
			withPortSource(cred, []diag{fatalDiag("ansible_phase_timeout",
				fmt.Sprintf("安装 playbook 超过 %ds 未返回（install_agent.run_sec 推导 + %ds 宽限）",
					budgetSec(pbBudget), int(execGrace.Seconds())),
				"目标机负载过高或 playbook 卡住；核对目标机后点「↻ 重试该步」")}))
		return
	}
	if rc != 0 {
		// 归因优先用「失败任务自己的原文」——比整段输出更准，首个 FAILED! 就是真因所在
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		d := classifyPlaybookFailure(failText, rc)
		detail["attempts"] = st.endAttempt("fail", d.Message)
		fail("安装失败："+d.Message+"（rc="+fmt.Sprint(rc)+"）", detail,
			withPortSource(cred, []diag{d}))
		return
	}

	// ⑥ 采集目标登记：写 vmagent file_sd（共享卷热加载）——装机完成即入采集网，观察环节才有据可核
	st.setPhase("register")
	diags := withPortSource(cred, []diag{})
	reason := fmt.Sprintf("ansible 安装完成 tag=%s sha256=前8位%s 目标=%s 配置已下发 agent_id=%s",
		tag, fileSHA[:8], cred.target(), agentID)
	if sdPath, sdErr := registerScrapeTarget(flow.ResourceID, cred.Host); sdErr != nil {
		detail["scrape_sd_error"] = sdErr.Error()
		diags = append(diags, warnDiag("scrape_sd_failed",
			"采集目标登记 vmagent file_sd 失败："+sdErr.Error(),
			"检查 data/scrape-sd 目录是否可写；不登记则采集观察环节无法确认入库"))
		reason += " ⚠ 采集目标登记失败（vmagent file_sd 写入失败，采集观察环节将无法确认入库）"
	} else {
		detail["scrape_sd"] = sdPath
		phases = append(phases, map[string]any{"phase": "register", "status": "ok", "path": sdPath})
		reason += " 采集目标已登记 vmagent"
	}
	if n := countWarn(diags); n > 0 {
		reason += fmt.Sprintf(" ⚠ %d 项待复核（见步骤告警）", n)
	}
	detail["phases"] = phases
	// 成功路径同样要留尝试记录：运维需要看到"这一版是第几次尝试装成的、跑了多久"。
	// 曾经这里用 historySnapshot()（只读不追加）——成功时永远是空数组，
	// 而失败路径走 endAttempt 有记录，同一张卡片成功/失败显示内容不对称
	attempts := st.endAttempt("ok", reason)
	extra := map[string]any{
		"source": "ansible_install", "detail": detail,
		"conn": credState(cred), "target": cred.target(),
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"task_total": len(pr.Tasks), "recap_line": recapLine(pr.Recap, pr.RecapSeq),
		// phases 必须平铺在顶层：界面按 dd.phases 取，藏在 detail 里等于没回传
		"phases":    phases,
		"attempt":   myAttempt,
		"attempts":  attempts,
		"diagnosis": diagJSON(diags), "evidence": evidence,
	}
	if msg := autoReportStep(catDB, agentStore, flowID, step.ID, reason, extra); msg != "" {
		fail("安装回报被拒："+msg, detail,
			withPortSource(cred, []diag{fatalDiag("install_report_rejected", "安装回报被拒："+msg,
				"步骤可能已被人工重置；点「↻ 重试该步」重新执行")}))
		return
	}
	// 台账记下"这台机装的是哪个 tag"：Agent 自报版本是编译常量，不能当安装版本用
	if err := catDB.SetResourceInstalledTag(flow.ResourceID, tag); err != nil {
		log.Printf("[onboard] 记录 installed_tag 失败 resource=%s tag=%s: %v", flow.ResourceID, tag, err)
	}
	addAudit("ansible 安装 SAgent", flow.ResourceID, "接入中心", reason)
}

// uninstallPlaybookTpl 内置兜底模板：外置 data/playbooks/uninstall.yml 缺失时回落用。
// 占位符 @DEST_HOME@ 由平台渲染（不用 Go text/template，避免 {{ }} 与 ansible jinja 冲突）。
// 与外置文件内容保持一致（有断言钉住）：两处不一致时，无法判定"改配置生效了没有"。
//
// 顺序不可颠倒（实测踩过）：先确认进程真的退出，再删目录 —— SAgent 收 SIGTERM 后要优雅
// 关停几秒，期间它的写盘（日志/配置落盘都带 MkdirAll）会把刚删掉的目录重新建出来
const uninstallPlaybookTpl = `- hosts: target
  gather_facts: false
  tasks:
    - name: 停止 SAgent 并等待进程真正退出（先 SIGTERM，超时再 SIGKILL）
      shell: |
        # 进程存活判定：/proc/<pid>/exe 精确匹配安装路径。
        # 不用 pkill -f —— 它的 -f 会匹配到执行本脚本的 sh 自身（历史踩坑）；
        # 不按 pid 文件盲杀 —— pid 可能已被回收成别的进程；
        # 也不用 kill -0 单判 —— 僵尸进程 kill -0 仍返回 0，但它已经不是 SAgent 了。
        # 判定口径与末尾「卸载结果核对」保持一致：同一个口径，不允许两处各说一套
        alive() {
          for exe in /proc/[0-9]*/exe; do
            tgt=$(readlink "$exe" 2>/dev/null || true)
            case "$tgt" in @DEST_HOME@/SAgent/bin/SAgent*) return 0 ;; esac
          done
          return 1
        }
        kill_all() {
          sig="$1"
          for exe in /proc/[0-9]*/exe; do
            tgt=$(readlink "$exe" 2>/dev/null || true)
            case "$tgt" in
              @DEST_HOME@/SAgent/bin/SAgent*)
                p=${exe#/proc/}
                kill "$sig" "${p%/exe}" 2>/dev/null || true
                ;;
            esac
          done
        }
        wait_gone() {
          i=0
          while [ "$i" -lt "$1" ]; do
            alive || return 0
            i=$((i+1)); sleep 1
          done
          return 1
        }
        if ! alive; then echo "SAgent 进程：卸载前未在运行"; exit 0; fi
        kill_all TERM
        if wait_gone 15; then
          echo "SAgent 进程：已优雅退出"
        else
          kill_all KILL
          if wait_gone 10; then echo "SAgent 进程：优雅退出超时，已强杀"; else echo "SAgent 进程：强杀后仍未消失"; fi
        fi
        exit 0

    - name: 删除 SAgent 安装目录（进程已确认退出后才执行）
      file:
        path: "@DEST_HOME@/SAgent"
        state: absent

    - name: 卸载结果核对（目录 / 进程 / 监听端口 三项目标机口径）
      shell: |
        # 取值语义：yes = 仍然存在 / 仍在监听（脏），no = 已消失（净）。
        # 与字段名 dir_exists / process_exists / port_agent_listening 的字面意思严格对齐——
        # 曾经写反过（干净也判成脏，卸载永远失败），别再玩反话。
        # 进程判定口径与上面「停止 SAgent」任务里的 alive() 完全一致：同一个口径，不允许两处各说一套。
        dir=no;  [ -d "@DEST_HOME@/SAgent" ] && dir=yes
        proc=no
        for exe in /proc/[0-9]*/exe; do
          tgt=$(readlink "$exe" 2>/dev/null || true)
          case "$tgt" in @DEST_HOME@/SAgent/bin/SAgent*) proc=yes ;; esac
        done
        port=no
        (ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null || true) | grep -q ':@AGENT_PORT@' && port=yes
        echo "dir_exists=$dir process_exists=$proc port_agent_listening=$port"
        # 硬判据只有两条：安装目录、安装路径下的进程。两者都消失才叫卸干净。
        if [ "$dir" = no ] && [ "$proc" = no ]; then
          echo "UNINSTALL_CLEAN"
        else
          echo "UNINSTALL_DIRTY"
        fi
        # 端口只作旁证，不参与成败判定：目录与进程都没了它还占着 @AGENT_PORT@，
        # 那是别的程序在用这个端口，不代表本次卸载没干净（误判会把干净的卸载判成失败）
        if [ "$port" = yes ] && [ "$proc" = no ]; then
          echo "NOTE: @AGENT_PORT@ 仍被监听，但占用者已不是 SAgent 进程（仅供参考）"
        fi
        exit 0
      register: uninstall_check
      changed_when: false

    - name: 输出卸载核对结果（界面逐任务可见）
      debug:
        msg: "卸载核对 {{ uninstall_check.stdout }}"

    - name: 断言卸载结果（目录与进程均须消失）
      command: /bin/true
      changed_when: false
      failed_when: "'UNINSTALL_CLEAN' not in uninstall_check.stdout"
`

// loadUninstallPlaybook 优先加载外置 playbook（data/playbooks/uninstall.yml），
// 缺失或不完整时回落内置模板——改卸载动作只改文件，不动代码。
// 占位符缺失即视为不可用：否则会把一份没渲染的 playbook 发到目标机（路径写死在文件里）
func loadUninstallPlaybook(destHome string) string {
	tpl := uninstallPlaybookTpl
	if b, err := os.ReadFile(filepath.Join("data", "playbooks", "uninstall.yml")); err == nil &&
		strings.Contains(string(b), "@DEST_HOME@") {
		tpl = string(b)
	}
	return strings.NewReplacer(
		"@DEST_HOME@", destHome,
		"@AGENT_PORT@", cfgAgentHTTPPort,
	).Replace(tpl)
}

// uninstallCheckLine 从卸载 playbook 的结构化结果里取出「目标机自证」那一行
// （目录 / 进程 / 端口的实测值）。取不到就返回空串——不编造核对结论
func uninstallCheckLine(r ansibleRunResult) string {
	// 核对行只取一行：debug 的 msg 里核对行后面还跟着断言标记（UNINSTALL_CLEAN/DIRTY），
	// 整段回传会让界面把断言标记当成核对内容多显示一行 —— 核对的是机器状态，不是断言结论
	oneLine := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexAny(s, "\r\n"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		return s
	}
	for _, t := range r.Tasks {
		if strings.Contains(t.Msg, "dir_exists=") {
			return oneLine(t.Msg)
		}
		for _, ln := range t.Lines {
			if strings.Contains(ln, "dir_exists=") {
				return oneLine(ln)
			}
		}
	}
	return ""
}

// classifyUninstallFailure 卸载 playbook 失败归因。
// 与安装侧的判据同源（第一判据永远是"是否真的连上了"），但结论文案必须区分：
// 卸载失败的处置动作是「上机排查残留」，安装失败的处置动作是「修配置重装」，混用会把人带偏
func classifyUninstallFailure(out string, rc int) diag {
	low := strings.ToLower(out)
	if strings.Contains(low, "unreachable!") || strings.Contains(low, "failed to connect to the host via ssh") {
		return classifySSHFailure(out, rc)
	}
	switch {
	case strings.Contains(out, "UNINSTALL_CLEAN") || strings.Contains(out, "断言卸载结果") ||
		strings.Contains(low, "uninstall_check"):
		return fatalDiag("uninstall_incomplete", "卸载核对未通过：目标机仍存在 SAgent 安装目录或进程",
			"查看下方原始输出里「卸载结果核对」任务那行（dir_exists / process_exists / port_agent_listening）；"+
				"目录删不掉通常是属主/权限问题（换有删除权限的账号），进程杀不掉通常是权限或进程被守护拉起")
	case strings.Contains(low, "errno 13") || strings.Contains(low, "permission denied"):
		return fatalDiag("target_permission_denied", "目标机权限不足：登录用户无法删除 SAgent 安装目录",
			"凭据正常，是目录属主/权限问题：改用具备删除权限的账号，或由 root 在目标机手工删除安装目录后重试")
	}
	return fatalDiag("uninstall_rc", fmt.Sprintf("卸载 playbook 执行失败（rc=%d）", rc),
		"查看下方原始输出，定位第一个 FAILED! 的任务")
}

// ansibleUninstallJob 卸载 SAgent：按 pid 停进程 → 删安装目录 → 目标机自证核对。
//
// 与安装作业同构：语法门禁（不碰目标机）→ 流式执行（逐任务回报）→ 终态带逐任务结果、
// PLAY RECAP、阶段记录与全量原文。区别在"成功"的定义更严——卸载必须由目标机自己
// 核对（目录 / 进程 / 端口）通过，playbook 里的断言不过就是 fail，平台不替它圆场。
func ansibleUninstallJob(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred) {
	st := newStepRunState(catDB, flowID, step, cred)
	defer st.startHeartbeat(catDB)()
	myAttempt := st.attempt
	hook := st.taskHook(catDB)

	// fail 收口：已被超时重试接管的旧尝试不再写库（避免旧结果覆盖新尝试的进展）
	fail := func(summary string, detail map[string]any, ds []diag) {
		if !jobStillCurrent(catDB, flowID, step.ID, myAttempt) {
			return
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["attempt"] = myAttempt
		// attempts 已由收口函数（uninstallFinish）写入时不再追加：同一次尝试在历史里只应有一行。
		// 早退路径（流水线/资源/临时目录/语法门禁）没经过收口，这里兜底补一条
		if _, ok := detail["attempts"]; !ok {
			detail["attempts"] = st.endAttempt("fail", summary)
		}
		detail["tasks"] = compactTasks(st.tasksSnapshot())
		failStep(catDB, agentStore, flowID, step, "ansible 卸载失败", summary, detail, cred, ds)
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

	phases := []map[string]any{}
	work, err := os.MkdirTemp("", "l0-uninstall-*")
	if err != nil {
		fail("创建卸载临时目录失败："+err.Error(), nil,
			[]diag{fatalDiag("uninstall_tmp_failed", "创建卸载临时目录失败："+err.Error(), "检查平台容器内 /tmp 是否可写")})
		return
	}
	defer os.RemoveAll(work)
	// 卸载目录口径与安装完全一致（destHomeFor）：装在哪就卸哪，
	// 不允许两侧各写一套推导逻辑——否则会"装到 A 却去删 B"，看着成功其实什么都没卸
	destHome := destHomeFor(cred)
	pbPath := filepath.Join(work, "uninstall.yml")
	if err := os.WriteFile(pbPath, []byte(loadUninstallPlaybook(destHome)), 0o644); err != nil {
		fail("生成卸载 playbook 失败："+err.Error(), nil,
			[]diag{fatalDiag("playbook_render_failed", "生成卸载 playbook 失败："+err.Error(), "")})
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

	// 语法门禁：语法错误在碰目标机前就拦下（与安装同一原则）
	st.setPhase("syntax")
	st.flush(catDB, "平台 ansible 启动 · 卸载 playbook 语法校验中（尚未触碰目标机）")
	synCmd := exec.Command("ansible-playbook", "-i", inv, pbPath, "--syntax-check")
	synCmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False")
	synOut, synErr := synCmd.CombinedOutput()
	phases = append(phases, map[string]any{
		"phase": "syntax", "status": map[bool]string{true: "failed", false: "ok"}[synErr != nil],
		"output": clip(string(synOut), 1500),
	})
	if synErr != nil {
		fail("卸载 playbook 语法校验未过（未触碰目标机）", map[string]any{
			"syntax_output": clip(string(synOut), 2000), "phases": phases,
		}, []diag{fatalDiag("playbook_syntax_failed", "卸载 playbook 语法校验未过：检查 data/playbooks/uninstall.yml",
			"外置 playbook 语法错误；修正后点「↻ 重试该步」")})
		return
	}

	// 执行卸载（流式：每个 TASK 落一次进度事件，界面实时可见）
	st.setPhase("playbook")
	st.flush(catDB, "平台 ansible 执行中 · 卸载 playbook 开始（逐任务回报）")
	pbBudget := execBudget(st.spec, budgetPlaybook)
	ctx, cancel := context.WithTimeout(context.Background(), pbBudget)
	defer cancel()
	pr := runPlaybookStream(ctx, inv, pbPath, hook)
	phases = append(phases, map[string]any{
		"phase": "playbook", "rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": len(pr.Tasks), "recap": recapLine(pr.Recap, pr.RecapSeq),
	})

	// L0 收口：与 L1 回执对账桥共用同一份实现（SPEC-D5-IN5 B4）
	uninstallFinish(catDB, agentStore, flowID, step, cred, pr, phases, destHome,
		myAttempt, pbBudget, st.endAttempt, st.setPhase, fail, nil)
}

// uninstallFinish 卸载作业的 L0 收口：判定成败 → 写步骤终态 → 审计。
//
// 与 offboardFinish 同构（SPEC-D5-IN5 B4）：执行端搬到 L1 后，步骤终态改由回执对账桥驱动，
// 但"这次算不算卸干净"的判据只能有一套——否则切了开关结论就变了。
// 差别只在收口口径：卸载认的是目标机三口径自证核对行（目录 / 进程 / 端口）。
func uninstallFinish(catDB *storepkg.DB, agentStore *AgentStore, flowID int64,
	step storepkg.FlowStepSnapshot, cred *sshCred,
	pr ansibleRunResult, phases []map[string]any, destHome string, myAttempt int,
	pbBudget time.Duration,
	endAttempt func(result, note string) []attemptRec,
	setPhase func(string),
	fail func(summary string, detail map[string]any, ds []diag),
	seed map[string]any) {

	detail := map[string]any{
		"dest_home": destHome, "conn": credState(cred), "target": cred.target(),
		"rc": pr.RC, "timed_out": pr.TimedOut,
		"tasks": compactTasks(pr.Tasks), "recap": pr.Recap,
		"recap_line": recapLine(pr.Recap, pr.RecapSeq),
		"phases":     phases, "attempt": myAttempt,
		"evidence": execEvidence(pr),
	}
	if pr.TimedOut {
		detail["attempts"] = endAttempt("timeout_run", fmt.Sprintf("playbook 超过 %ds 未返回", budgetSec(pbBudget)))
		fail(fmt.Sprintf("卸载 playbook 超过 %ds 未返回（平台已终止）", budgetSec(pbBudget)), detail,
			withPortSource(cred, []diag{fatalDiag("ansible_phase_timeout",
				fmt.Sprintf("卸载 playbook 超过 %ds 未返回（uninstall_agent.run_sec 推导 + %ds 宽限）",
					budgetSec(pbBudget), int(execGrace.Seconds())),
				"目标机负载过高或删除大目录卡住；核对目标机后点「↻ 重试该步」")}))
		return
	}
	if pr.RC != 0 {
		// 归因优先用「失败任务自己的原文」——卸载核对断言失败时，它就是结论本身
		failText := taskFirstFailureText(pr.Tasks)
		if strings.TrimSpace(failText) == "" {
			failText = pr.Output
		}
		d := classifyUninstallFailure(failText, pr.RC)
		detail["attempts"] = endAttempt("fail", d.Message)
		fail("卸载失败："+d.Message+"（rc="+fmt.Sprint(pr.RC)+"）", detail,
			withPortSource(cred, []diag{d}))
		return
	}

	// 成功收口：核对行本身就是"卸干净了"的实测证据，随成功事件一起回传界面
	setPhase("verify")
	diags := withPortSource(cred, []diag{})
	checkLine := uninstallCheckLine(pr)
	detail["verify_line"] = checkLine
	if checkLine == "" {
		diags = append(diags, warnDiag("verify_line_missing",
			"未从 playbook 输出中取到目标机核对行（断言已通过，但核对数值不可见）",
			"确认 data/playbooks/uninstall.yml 的「输出卸载核对结果」任务未被删改"))
	}
	if strings.Contains(checkLine, "port_agent_listening=yes") {
		diags = append(diags, warnDiag("port_still_listening",
			fmt.Sprintf("目标机 %s 端口仍有进程在监听（SAgent 进程已消失，可能是别的程序占用该端口）", cfgAgentHTTPPort),
			"确认占用者；若确实是遗留的 SAgent 进程，需人工终止后再重装"))
	}
	reason := fmt.Sprintf("ansible 卸载完成 目标=%s 家目录=%s 核对=%s", cred.target(), destHome, checkLine)
	if n := countWarn(diags); n > 0 {
		reason += fmt.Sprintf(" ⚠ %d 项待复核（见步骤告警）", n)
	}
	attempts := endAttempt("ok", reason)
	outExtra := map[string]any{}
	// seed 先落：桥侧补记的通道元信息（l1_task_id / exec_channel 等）不在下面的计算字段里，
	// 先铺底再由计算字段覆盖，保证 seed 永远盖不掉"谁执行、结论是什么"这类权威字段
	for k, v := range seed {
		outExtra[k] = v
	}
	outExtra["source"] = "ansible_uninstall"
	outExtra["detail"] = detail
	outExtra["conn"] = credState(cred)
	outExtra["target"] = cred.target()
	outExtra["tasks"] = compactTasks(pr.Tasks)
	outExtra["recap"] = pr.Recap
	outExtra["task_total"] = len(pr.Tasks)
	outExtra["recap_line"] = recapLine(pr.Recap, pr.RecapSeq)
	// phases 必须平铺在顶层：界面按 dd.phases 取，藏在 detail 里等于没回传
	outExtra["phases"] = phases
	outExtra["attempt"] = myAttempt
	outExtra["attempts"] = attempts
	outExtra["verify_line"] = checkLine
	outExtra["diagnosis"] = diagJSON(diags)
	outExtra["evidence"] = execEvidence(pr)
	if msg := autoReportStep(catDB, agentStore, flowID, step.ID, reason, outExtra); msg != "" {
		fail("卸载回报被拒："+msg, detail,
			withPortSource(cred, []diag{fatalDiag("uninstall_report_rejected", "卸载回报被拒："+msg,
				"步骤可能已被人工重置；点「↻ 重试该步」重新执行")}))
		return
	}
	resourceID := ""
	if flow, _ := catDB.GetFlow(flowID); flow != nil {
		resourceID = flow.ResourceID
	}
	addAudit("ansible 卸载 SAgent", resourceID, "接入中心", reason)
}

// pickedVersionTag 读 pick_version 最近一条人工确认事件里的 tag
func pickedVersionTag(catDB *storepkg.DB, flowID int64) string {
	last, _ := catDB.LatestEvent(flowID, "pick_version")
	if last == nil || last.Status != stOK {
		return ""
	}
	var d struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(last.Detail), &d); err != nil {
		return ""
	}
	return d.Tag
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
