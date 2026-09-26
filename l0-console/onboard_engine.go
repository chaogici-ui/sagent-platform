package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	storepkg "github.com/sagent/l0-console/store"
	"gopkg.in/yaml.v3"
)

// ===================================================================
//  流程模板引擎（L2 模式模板 + L3 插件声明）
//  设计依据：PLAN-采集接入中心与流程引擎.md §4
//
//  边界（红线）：原子能力（atom）写死在 Go 里；流程编排（steps/when/requires）、
//  参数表单（params.yaml）、检查项（checks.yaml）全部数据化，随插件包走。
//  验收标准：接入一个新的开源 exporter = data/integrations/ 加 1 个目录 + 5 个文件，
//  不改一行 Go、不改一行 JS。若引擎里出现具体插件名，即为退化。
// ===================================================================

// ---------- 模板结构 ----------

// FlowTemplate 模式模板（data/flow_templates/<mode>.yaml）
type FlowTemplate struct {
	ID          string           `yaml:"id" json:"id"`
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description" json:"description"`
	// Internal 内部编排模板：不由「新建接入」向导展示为可选模式。
	// 用于「既有接入的逆操作」这类模板（如 offboard 卸载）——它们由对应资源上的
	// 操作按钮拉起，混进向导的模式单选里会让"新建接入"出现语义矛盾的选项
	Internal bool             `yaml:"internal" json:"internal"`
	Steps    []FlowTemplateSt `yaml:"steps" json:"steps"`
}

// FlowTemplateSt 模板中的单个步骤
type FlowTemplateSt struct {
	ID       string   `yaml:"id" json:"id"`
	Atom     string   `yaml:"atom" json:"atom"`
	Title    string   `yaml:"title" json:"title"`
	Scope    string   `yaml:"scope" json:"scope"`
	When     string   `yaml:"when" json:"when"`
	Gate     string   `yaml:"gate" json:"gate"`
	Requires []string `yaml:"requires" json:"requires"`
	Hint     string   `yaml:"hint" json:"hint"`
	// Instruction 执行包模板（external 步骤）：{{.ip}} 占位渲染目标资源 IP
	Instruction string `yaml:"instruction" json:"instruction"`
}

// ParamField 参数表单字段（params.yaml 与 onboard_config.json 内联声明共用）
type ParamField struct {
	Name        string   `yaml:"name" json:"name"`
	Label       string   `yaml:"label" json:"label"`
	Type        string   `yaml:"type" json:"type"` // string|int|secret|enum|duration|textarea|kv
	Required    bool     `yaml:"required" json:"required"`
	Default     string   `yaml:"-" json:"default,omitempty"`
	DefaultAny  any      `yaml:"default" json:"-"`
	Options     []string `yaml:"options" json:"options,omitempty"`
	Placeholder string   `yaml:"placeholder" json:"placeholder,omitempty"`
	Help        string   `yaml:"help" json:"help,omitempty"`
	Probe       string   `yaml:"probe" json:"probe,omitempty"`
}

// normDefault 把 YAML 里的 int/bool/string 默认值统一成字符串
func (f *ParamField) normDefault() {
	if f.Default != "" || f.DefaultAny == nil {
		return
	}
	switch v := f.DefaultAny.(type) {
	case string:
		f.Default = v
	case int:
		f.Default = itoa(v)
	case int64:
		f.Default = itoa64(v)
	case bool:
		if v {
			f.Default = "true"
		} else {
			f.Default = "false"
		}
	default:
		f.Default = ""
	}
}

// ParamsFile 插件包内的 params.yaml（L3 层）
type ParamsFile struct {
	Plugin string       `yaml:"plugin"`
	Title  string       `yaml:"title"`
	Fields []ParamField `yaml:"fields"`
}

// Ability 可选采集能力：由 onboard_config.json + 插件包 params.yaml 合成，前端零写死
type Ability struct {
	ID          string       `json:"id"`     // probe 名（如 mysql_probe），也是 targets.plugin 的取值
	Name        string       `json:"name"`   // 展示名
	Desc        string       `json:"desc"`   //
	Scope       string       `json:"scope"`  // agent（装 Agent 的机器上采）/ remote（远程拉取）
	Locked      bool         `json:"locked"` // 地基能力，强制开启不可关闭
	DefaultPort string       `json:"default_port,omitempty"`
	ParamsPath  string       `json:"params_path,omitempty"` // 相对 data/integrations/，便于溯源
	HasParams   bool         `json:"has_params"`
	Params      []ParamField `json:"params,omitempty"`
}

// ---------- 模板加载 ----------

const (
	defaultFlowTemplateDir = "data/flow_templates"
	// integrationsDir 插件包根目录（params.yaml / checks.yaml / flow.yaml 都在各包内）
	integrationsDir = "data/integrations"
)

// onboardCfg 产品语义配置（启动时加载，见 main.go）；由接入流程与渲染器共用
var onboardCfg *OnboardConfig

var (
	tplMu    sync.RWMutex
	tplCache map[string]*FlowTemplate
	tplOrder []string
)

// loadFlowTemplates 从目录加载全部模式模板（启动时调用；目录缺失只告警不阻断）
func loadFlowTemplates(dir string) ([]*FlowTemplate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []*FlowTemplate{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		t := &FlowTemplate{}
		if err := yaml.Unmarshal(data, t); err != nil {
			// 单个模板坏了不影响其它模板，但必须留下痕迹
			println("flow template parse error: " + e.Name() + " -> " + err.Error())
			continue
		}
		if t.ID == "" {
			t.ID = strings.TrimSuffix(e.Name(), ".yaml")
		}
		if t.Name == "" {
			t.Name = t.ID
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	tplMu.Lock()
	tplCache = map[string]*FlowTemplate{}
	tplOrder = nil
	for _, t := range out {
		tplCache[t.ID] = t
		tplOrder = append(tplOrder, t.ID)
	}
	tplMu.Unlock()
	return out, nil
}

// flowTemplateList 返回全部模板（稳定顺序）
func flowTemplateList() []*FlowTemplate {
	tplMu.RLock()
	defer tplMu.RUnlock()
	out := make([]*FlowTemplate, 0, len(tplOrder))
	for _, id := range tplOrder {
		if t := tplCache[id]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// flowTemplateListForWizard 向导可见的模式模板：排除 internal（如 offboard 卸载）。
// getFlowTemplate 仍能按 id 取到 internal 模板——创建入口在资源操作按钮，不在向导
func flowTemplateListForWizard() []*FlowTemplate {
	all := flowTemplateList()
	out := make([]*FlowTemplate, 0, len(all))
	for _, t := range all {
		if t.Internal {
			continue
		}
		out = append(out, t)
	}
	return out
}

// getFlowTemplate 按 id 取模板
func getFlowTemplate(id string) *FlowTemplate {
	tplMu.RLock()
	defer tplMu.RUnlock()
	return tplCache[id]
}

// templateInstructionFor 取某模式下某步骤的当前执行包模板。
// 下发物实时取自模板（而非创建时固化的步骤快照）：模板改命令即时生效，
// 存量流水线同样受益——快照字段仅作为模板缺失时的回退
func templateInstructionFor(mode, stepID string) string {
	t := getFlowTemplate(mode)
	if t == nil {
		return ""
	}
	for _, s := range t.Steps {
		if s.ID == stepID {
			return s.Instruction
		}
	}
	return ""
}

// snapshots 把模板步骤转成可持久化的步骤快照
func (t *FlowTemplate) snapshots() []storepkg.FlowStepSnapshot {
	out := make([]storepkg.FlowStepSnapshot, 0, len(t.Steps))
	for _, s := range t.Steps {
		sc := s.Scope
		if sc == "" {
			sc = "platform"
		}
		out = append(out, storepkg.FlowStepSnapshot{
			ID: s.ID, Atom: s.Atom, Title: s.Title, Scope: sc,
			When: s.When, Gate: s.Gate, Requires: s.Requires, Hint: s.Hint,
			Instruction: s.Instruction,
		})
	}
	return out
}

// ---------- when 求值器 ----------

// evalWhen 求值步骤的 when 表达式。
// 白名单设计：只认识下面这些变量与算子，**不求值任意表达式**（安全 + 可预测）。
//
//	变量：agent_exists / target_agent_exists / abilities_requires_params / preflight_passed
//	      mode（与字符串比较：mode=='edge'）
//	算子：! && || == !=
func evalWhen(expr string, env map[string]bool, mode string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true
	}
	// 顶层 || 拆分
	for _, alt := range splitTop(expr, "||") {
		allTrue := true
		for _, term := range splitTop(alt, "&&") {
			if !evalTerm(term, env, mode) {
				allTrue = false
				break
			}
		}
		if allTrue {
			return true
		}
	}
	return false
}

// skipReason 把 when 跳过翻译成具体原因（2026-09-21 用户评审）。
// 引擎的 when 是白名单小语言，跳过原因也应该一样：枚举已知条件给确定文案，
// 未识别的表达式退回「条件不成立」——不猜、不写放之四海皆准的空话
func skipReason(when string, env map[string]bool) string {
	switch when {
	case "!agent_exists":
		if env["agent_exists"] {
			return "目标机已有 SAgent——本步跳过，复用现有安装"
		}
		return "目标机尚未安装 SAgent，但本步被条件拦截（异常，请人工核查）"
	case "abilities_requires_params":
		return "所选能力没有需要填写的采集参数——本步跳过"
	}
	if when == "" {
		return "前置步骤未就绪——本步不适用"
	}
	return "前置条件不成立（when: " + when + "）——本步不适用"
}

// splitTop 按分隔符拆分（本引擎不支持括号，故只需顺序拆分）
func splitTop(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func evalTerm(t string, env map[string]bool, mode string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	// 比较式
	for _, op := range []string{"==", "!="} {
		if i := strings.Index(t, op); i > 0 {
			left := strings.TrimSpace(t[:i])
			right := strings.Trim(strings.TrimSpace(t[i+len(op):]), "'\"")
			lv := varValue(left, env, mode)
			if op == "==" {
				return lv == right
			}
			return lv != right
		}
	}
	// 取反
	neg := false
	for strings.HasPrefix(t, "!") {
		neg = !neg
		t = strings.TrimSpace(strings.TrimPrefix(t, "!"))
	}
	v := env[t]
	if neg {
		return !v
	}
	return v
}

// varValue 变量取值（用于比较式）：未知变量一律空串
func varValue(name string, env map[string]bool, mode string) string {
	if name == "mode" {
		return mode
	}
	v, ok := env[name]
	if !ok {
		return ""
	}
	if v {
		return "true"
	}
	return "false"
}

// ---------- 能力解析 ----------

// loadAbilities 合成能力清单：
//   - onboard_config.json 的 onboard_plugins（远程采集，含 scope/params 路径）
//   - onboard_config.json 的 agent_abilities（Agent 侧内置能力，可含内联参数声明）
//
// 参数声明优先读插件包内的 params.yaml（L3 层，随插件包走），读不到时用内联声明。
// baseDir 为 data/integrations 相对路径。
func loadAbilities(cfg *OnboardConfig, baseDir string) []*Ability {
	out := []*Ability{}
	if cfg == nil {
		return out
	}
	for _, p := range cfg.OnboardPlugins {
		scope := p.Scope
		if scope == "" {
			scope = "remote"
		}
		a := &Ability{
			ID: p.Probe, Name: p.Name, Scope: scope,
			DefaultPort: p.Port,
		}
		if p.Probe == "" {
			a.ID = p.Name
		}
		if p.Params != "" {
			a.ParamsPath = p.Params
			if pf, err := loadParamsFile(filepath.Join(baseDir, p.Params)); err == nil && pf != nil {
				a.Params = pf.Fields
				a.HasParams = len(pf.Fields) > 0
				if pf.Title != "" {
					a.Name = strings.TrimSuffix(pf.Title, " 采集参数")
				}
			}
		}
		out = append(out, a)
	}
	for _, ab := range cfg.AgentAbilities {
		a := &Ability{
			ID: ab.ID, Name: ab.Name, Desc: ab.Desc,
			Scope: ab.Scope, Locked: ab.Locked,
		}
		if a.Scope == "" {
			a.Scope = "agent"
		}
		a.Params = ab.ParamsInline
		a.HasParams = len(ab.ParamsInline) > 0
		for i := range a.Params {
			a.Params[i].normDefault()
		}
		out = append(out, a)
	}
	return out
}

// loadParamsFile 读插件包内的 params.yaml
func loadParamsFile(path string) (*ParamsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pf := &ParamsFile{}
	if err := yaml.Unmarshal(data, pf); err != nil {
		return nil, err
	}
	for i := range pf.Fields {
		pf.Fields[i].normDefault()
	}
	return pf, nil
}

// abilityByID 按 id 取能力
func abilityByID(list []*Ability, id string) *Ability {
	for _, a := range list {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// abilitiesRequireParams 选中的能力中是否有声明了参数表单的（驱动 collect_params 步骤的 when）
func abilitiesRequireParams(list []*Ability, selected []string) bool {
	for _, id := range selected {
		if a := abilityByID(list, id); a != nil && a.HasParams {
			return true
		}
	}
	return false
}

func itoa(v int) string     { return strconv.Itoa(v) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
