package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ===================================================================
//  ansible 流式执行 + 输出解析
//
//  用户要求（2026-09-21）：安装过程中的**所有**信息都要回到界面。
//  原文一坨 6000 字符丢给运维不算"信息"，运维要的是：
//    - 一共几步、每步叫什么、哪步失败、失败原因是什么、变了什么
//    - 跑到哪了（执行中就能看见，不是结束后才知道）
//  所以这里把 ansible 输出解析成结构化任务列表，并在任务边界回调进度。
//
//  为什么不接 ansible 的 json callback 插件：回调插件属于"目标机侧/ansible 版本侧"
//  的依赖，容器里装的是哪个版本、有没有该插件都不由平台掌握；default callback 的
//  输出格式十年没变，自解析稳定且零依赖，原文同时保留给人看。
// ===================================================================

// ansibleTask 一个 TASK 的执行结果（结构化，界面直接渲染）
type ansibleTask struct {
	Index      int      `json:"index"`
	Play       string   `json:"play,omitempty"`
	Phase      string   `json:"phase,omitempty"` // 执行阶段（探路分 ping/端口核对/setup/df；安装分语法校验/playbook）
	Name       string   `json:"name"`
	Status     string   `json:"status"` // ok / changed / failed / unreachable / skipping / unknown
	Host       string   `json:"host,omitempty"`
	Changed    bool     `json:"changed"`
	Items      []string `json:"items,omitempty"` // loop / with_items 的 item（哪个子路径失败了）
	Msg        string   `json:"msg,omitempty"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
	RC         *int     `json:"rc,omitempty"`
	Lines      []string `json:"lines,omitempty"` // 该任务的原始输出行：即使总原文被截，任务级原文仍在
	DurationMs int64    `json:"duration_ms,omitempty"`
}

// ansibleRunResult 一次 ansible 调用（命令或 playbook）的完整结果
type ansibleRunResult struct {
	Output   string         // 合并原文（stdout+stderr 按到达顺序）——全量，裁剪交给调用方
	RC       int            // 退出码（-1 = 未正常退出，如被 ctx kill）
	TimedOut bool           // 是否因 ctx 超时被杀（与"命令自己报错"是两回事）
	Err      error          // 非超时的执行错误
	Tasks    []ansibleTask  // 逐任务结构化结果
	Recap    map[string]int // PLAY RECAP 汇总（ok/changed/unreachable/failed/skipped/rescued/ignored）
	RecapSeq []string       // recap 出现顺序（前端固定列序用）
}

// ---------- 解析 ----------

var (
	reTaskLine   = regexp.MustCompile(`^TASK \[(.*)\]\s*\*+`)
	rePlayLine   = regexp.MustCompile(`^PLAY \[(.*)\]\s*\*+`)
	reResultLine = regexp.MustCompile(`^(ok|changed|failed|fatal|unreachable|skipping|included|ignoring):\s*\[([^\]]*)\]\s*(?:\(item=(.*?)\))?\s*(?::\s*FAILED!)?\s*(?:=>)?\s*(.*)$`)
	reRecapLine  = regexp.MustCompile(`^(\S+)\s*:\s*ok=(\d+)\s+changed=(\d+)\s+unreachable=(\d+)\s+failed=(\d+)\s+skipped=(\d+)\s+rescued=(\d+)\s+ignored=(\d+)`)
	// loop 项有两种落位：`failed: [h] (item=/x) => {…}`（旧）与
	// `ok: [h] => (item=/x)`（2.8+ 常见形态）。前者 reResultLine 直接捕获，
	// 后者留在 body 文本里——不单独摘出来，loop 子项会整批丢失，
	// 界面「受影响子项（哪个子路径失败了）」就永远是空的
	reLeadingItem = regexp.MustCompile(`^\(item=(.*?)\)\s*(?:=>)?\s*`)
)

// ansibleParser 逐行状态机：把 default callback 的输出切成结构化任务。
// 刻意做成纯数据结构（无 IO），便于单测直接喂行验证
type ansibleParser struct {
	play      string
	tasks     []ansibleTask
	cur       *ansibleTask
	curLines  []string
	pending   *strings.Builder // 多行 JSON 累积（长 msg 时 ansible 会换行缩进）
	pendingTo *ansibleResultHit
	inRecap   bool
	recap     map[string]int
	recapSeq  []string
	onTask    func(ansibleTask, int) // 任务收尾回调（进度上报用）
}

// ansibleResultHit 一行结果行（`failed: [host] (item=x) => {…}`）的解析产物
type ansibleResultHit struct {
	kind string
	host string
	item string
	body string
}

func newAnsibleParser(onTask func(ansibleTask, int)) *ansibleParser {
	return &ansibleParser{
		recap: map[string]int{}, onTask: onTask,
		recapSeq: []string{"ok", "changed", "unreachable", "failed", "skipped", "rescued", "ignored"},
	}
}

// Feed 喂入一行（不含换行符）
func (p *ansibleParser) Feed(line string) {
	raw := strings.TrimRight(line, "\r")

	// 多行 JSON 累积中：继续拼，直到能解析出完整对象
	if p.pending != nil {
		p.pending.WriteString(raw)
		p.pending.WriteByte('\n')
		if body, rest, ok := parseJSONHead(p.pending.String()); ok {
			hit := p.pendingTo
			p.pending, p.pendingTo = nil, nil
			p.applyResult(hit, body)
			if strings.TrimSpace(rest) != "" {
				p.Feed(rest)
			}
		}
		return
	}

	trimmed := strings.TrimSpace(raw)

	// 任务边界
	if m := reTaskLine.FindStringSubmatch(trimmed); m != nil {
		p.closeTask()
		p.cur = &ansibleTask{Play: p.play, Name: strings.TrimSpace(m[1]), Status: "unknown"}
		p.curLines = []string{trimmed}
		return
	}
	if m := rePlayLine.FindStringSubmatch(trimmed); m != nil {
		p.closeTask()
		p.play = strings.TrimSpace(m[1])
		return
	}
	if strings.HasPrefix(trimmed, "PLAY RECAP") {
		p.closeTask()
		p.inRecap = true
		return
	}

	// recap 计数行
	if p.inRecap {
		if m := reRecapLine.FindStringSubmatch(trimmed); m != nil {
			keys := []string{"ok", "changed", "unreachable", "failed", "skipped", "rescued", "ignored"}
			for i, k := range keys {
				p.recap[k] += parseCount(m[i+2])
			}
			return
		}
	}

	// 结果行
	if m := reResultLine.FindStringSubmatch(trimmed); m != nil {
		hit := &ansibleResultHit{kind: m[1], host: m[2], item: m[3], body: strings.TrimSpace(m[4])}
		// loop 项落在 `=>` 之后的形态：摘出来再解析 body，
		// 否则 "(item=…)" 会被当成正文（既丢子项、又污染 JSON 解析入口）
		if hit.item == "" {
			if mi := reLeadingItem.FindStringSubmatch(hit.body); mi != nil {
				hit.item = mi[1]
				hit.body = strings.TrimSpace(hit.body[len(mi[0]):])
			}
		}
		if p.cur == nil {
			// 没有 TASK 头的裸结果（如 ansible -m ping）：自造一个任务承载，信息不能丢
			p.cur = &ansibleTask{Play: p.play, Name: "(未命名任务)", Status: "unknown"}
		}
		p.curLines = append(p.curLines, trimmed)
		if strings.HasPrefix(hit.body, "{") || strings.HasPrefix(hit.body, "[") {
			if body, rest, ok := parseJSONHead(hit.body); ok {
				p.applyResult(hit, body)
				if strings.TrimSpace(rest) != "" {
					p.Feed(rest)
				}
			} else {
				p.pending = &strings.Builder{}
				p.pendingTo = hit
				p.pending.WriteString(hit.body)
				p.pending.WriteByte('\n')
			}
		} else {
			p.applyResult(hit, nil)
		}
		return
	}

	// 其它行：归入当前任务的原文（版本提示、deprecation、跳过的日志等都要留痕）
	if p.cur != nil {
		p.curLines = append(p.curLines, raw)
	}
}

// applyResult 把一行结果并入当前任务
func (p *ansibleParser) applyResult(hit *ansibleResultHit, body map[string]any) {
	if p.cur == nil {
		return
	}
	if hit.host != "" {
		p.cur.Host = hit.host
	}
	if hit.item != "" && !containsStr(p.cur.Items, hit.item) {
		p.cur.Items = append(p.cur.Items, hit.item)
	}
	p.cur.Status = mergeTaskStatus(p.cur.Status, hit.kind)
	if hit.kind == "changed" {
		p.cur.Changed = true
	}
	if body == nil {
		return
	}
	if v, ok := body["changed"].(bool); ok && v {
		p.cur.Changed = true
	}
	if s, ok := body["msg"].(string); ok && strings.TrimSpace(s) != "" && p.cur.Msg == "" {
		p.cur.Msg = strings.TrimSpace(s)
	}
	if s, ok := body["stdout"].(string); ok && strings.TrimSpace(s) != "" {
		p.cur.Stdout = strings.TrimSpace(s)
	}
	if s, ok := body["stderr"].(string); ok && strings.TrimSpace(s) != "" {
		p.cur.Stderr = strings.TrimSpace(s)
	}
	if v, ok := body["rc"].(float64); ok {
		rc := int(v)
		p.cur.RC = &rc
	}
}

// closeTask 收尾当前任务并入列（任务边界/结束时调用）
func (p *ansibleParser) closeTask() {
	if p.cur == nil {
		return
	}
	// 多行 JSON 没解析完（被截断/格式异常）也要收尾，原始行已经在 curLines 里，不丢信息
	if p.pending != nil {
		p.curLines = append(p.curLines, strings.TrimRight(p.pending.String(), "\n"))
		p.pending, p.pendingTo = nil, nil
	}
	// 兜底：只看到结果行没看到 TASK 头的场景（ansible -m ping 输出 ok: [host] => {...}）
	if p.cur.Status == "unknown" && len(p.curLines) > 0 {
		p.cur.Status = "ok"
	}
	p.cur.Lines = p.curLines
	p.cur.Index = len(p.tasks) + 1
	t := *p.cur
	p.tasks = append(p.tasks, t)
	p.cur, p.curLines = nil, nil
	if p.onTask != nil {
		p.onTask(t, len(p.tasks))
	}
}

// finish 收尾并返回全部任务
func (p *ansibleParser) finish() []ansibleTask {
	p.closeTask()
	return p.tasks
}

// mergeTaskStatus 同一任务多行结果时取"最严重"的那个（failed > unreachable > changed > ok > skipping）
func mergeTaskStatus(cur, next string) string {
	rank := func(s string) int {
		switch s {
		case "failed", "fatal":
			return 5
		case "unreachable":
			return 4
		case "changed":
			return 3
		case "ok":
			return 2
		case "skipping", "included", "ignoring":
			return 1
		}
		return 0
	}
	norm := func(s string) string {
		if s == "fatal" {
			return "failed"
		}
		if s == "included" || s == "ignoring" {
			return "ok"
		}
		return s
	}
	if rank(next) >= rank(cur) {
		return norm(next)
	}
	return norm(cur)
}

// parseJSONHead 从字符串头部解析一个 JSON 对象，返回对象、剩余内容与是否成功。
// 用于多行 JSON 的边界判定：ansible 的 msg 很长时会换行缩进输出
func parseJSONHead(s string) (map[string]any, string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, "", false
	}
	return v, s[dec.InputOffset():], true
}

func parseCount(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ---------- 执行 ----------

// runAnsibleStream 流式执行 ansible 命令（-m 模块形式），边跑边解析并回调进度。
// ctx 超时会 kill 进程并置 TimedOut —— 作业不允许无限挂住
func runAnsibleStream(ctx context.Context, inv string, onTask func(ansibleTask, int), args ...string) ansibleRunResult {
	full := append([]string{"all", "-i", inv}, args...)
	return streamExec(ctx, "ansible", full, onTask)
}

// runPlaybookStream 流式执行 ansible-playbook（安装作业用）
func runPlaybookStream(ctx context.Context, inv, pbPath string, onTask func(ansibleTask, int)) ansibleRunResult {
	return streamExec(ctx, "ansible-playbook", []string{"-i", inv, pbPath}, onTask)
}

// streamExec 流式执行：stdout/stderr 合并后逐行读，解析 + 原文累积
func streamExec(ctx context.Context, bin string, args []string, onTask func(ansibleTask, int)) ansibleRunResult {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_NOCOLOR=1", "ANSIBLE_FORCE_COLOR=0")
	pr, pw, err := os.Pipe()
	if err != nil {
		return ansibleRunResult{RC: -1, Err: err}
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return ansibleRunResult{RC: -1, Err: err}
	}
	pw.Close() // 父进程不持有写端，子进程退出后读端自然 EOF

	p := newAnsibleParser(onTask)
	var sb strings.Builder
	br := bufio.NewReaderSize(pr, 1<<20)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			sb.WriteString(line)
			p.Feed(line)
		}
		if rerr != nil {
			break
		}
	}
	pr.Close()

	werr := cmd.Wait()
	rc := 0
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			rc = -1
		}
	}
	out := ansibleRunResult{
		Output: sb.String(), RC: rc, Tasks: p.finish(),
		Recap: p.recap, RecapSeq: p.recapSeq, TimedOut: ctx.Err() != nil,
	}
	if werr != nil && !out.TimedOut {
		out.Err = werr
	}
	return out
}

// ---------- 结果呈现 ----------

// recapLine 把 PLAY RECAP 汇总渲染成一行（界面/摘要直接用）
func recapLine(recap map[string]int, seq []string) string {
	parts := []string{}
	for _, k := range seq {
		if v, ok := recap[k]; ok {
			parts = append(parts, k+"="+itoa(v))
		}
	}
	return strings.Join(parts, " ")
}

// taskFailures 摘出失败任务（诊断与摘要用）：第一个失败任务就是真因所在
func taskFailures(tasks []ansibleTask) []ansibleTask {
	out := []ansibleTask{}
	for _, t := range tasks {
		if t.Status == "failed" || t.Status == "unreachable" {
			out = append(out, t)
		}
	}
	return out
}

// taskFirstFailureText 第一个失败任务的原始行拼文本（归因解析的输入）
func taskFirstFailureText(tasks []ansibleTask) string {
	for _, t := range tasks {
		if t.Status == "failed" || t.Status == "unreachable" {
			if len(t.Lines) > 0 {
				return strings.Join(t.Lines, "\n")
			}
			return t.Msg
		}
	}
	return ""
}

// probePhaseEvidence 探路某阶段的证据包（原文保头保尾 + 字节数 + 是否超时）
func probePhaseEvidence(phase string, r ansibleRunResult) map[string]any {
	ev := map[string]any{
		"rc": r.RC, "output_bytes": len(r.Output), "timed_out": r.TimedOut,
	}
	switch phase {
	case "ping":
		ev["ping_output"] = clip(r.Output, 4000)
	case "setup":
		ev["setup_output"] = clip(r.Output, 4000)
	case "df":
		ev["df_output"] = clip(r.Output, 1000)
	default:
		ev[phase+"_output"] = clip(r.Output, 4000)
	}
	return ev
}

// mergeRecaps 合并多次 ansible 调用的 PLAY RECAP 计数（探路分四个阶段跑）
func mergeRecaps(ms ...map[string]int) map[string]int {
	out := map[string]int{}
	for _, m := range ms {
		for k, v := range m {
			out[k] += v
		}
	}
	return out
}
