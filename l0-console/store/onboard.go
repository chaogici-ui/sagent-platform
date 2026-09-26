package store

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// ===================================================================
//  接入中心（Onboard Center）数据层
//  三张表：resources（资源对象）/ onboard_flow（接入实例）/ onboard_event（每步事件）
//  设计依据见 PLAN-采集接入中心与流程引擎.md §3
// ===================================================================

// Resource 资源对象：接入流程的第①步落点。
// 台账里的 OS/Arch 只作预填；主机探路（P1）实测后回填，实测值权威。
type Resource struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	ResourceType string `json:"resource_type"` // host/mysql/redis/kafka…
	Role         string `json:"role"`          // business（业务对象）/ platform_device（采集平台设备）
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Kernel       string `json:"kernel"`
	// SSH 凭据（模拟资源台账下发：现阶段人工随资源录入，接 CMDB 后由资源 get）。
	// 平台 ansible 执行器（探路/安装）用；列表接口回显时密码打码，不回传明文
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SSHPassword string `json:"ssh_password"`
	// Agent 控制通道接入点（覆盖值）：目标机回连平台的地址。按资源网络分区下发——
	// docker 网内目标机用默认 l0-console:8080，跨网段真实主机用可达的接入点（如隧道/内网 LB）
	AgentConsoleURL string `json:"agent_console_url"`
	// TargetKind 目标类型（2026-09-22 用户要求：只填 IP + 类型，接入点平台全自动）：
	// docker = 本机/同 Docker 网络容器目标（平台自动探测候选回连地址）；
	// host   = 公司内网真实主机/虚拟机（直连不通时平台自动建反向隧道并保活）。
	// 空 = 未登记，探路按 auto（候选 → 隧道）顺序自动处理
	TargetKind string `json:"target_kind"`
	Source     string `json:"source"` // manual / cmdb / import / agent（CMDB 适配位）
	// SvcState 启停维护态（2026-09-22 用户拍板）："" = 正常；"stopped" = Agent 已停（维护，
	// 心跳消失是预期表现，不算失联）；"running" = 启停流程确认在跑。卸载完成后清空
	SvcState string `json:"svc_state"`
	// InstalledTag 平台装上去的版本包 tag（安装成功写入，卸载清空）。
	// 为什么不用 Agent 自报版本：那是二进制里的编译常量，换 tag 不重建就不变，
	// 拿它判断"该不该升级"会得出错误结论（2026-09-22 两处口径问题）
	InstalledTag string `json:"installed_tag"`
	LabelsJSON   string `json:"labels_json"`
	ProbeJSON    string `json:"probe_json"` // 最近一次 P1 探路原始结果
	ProbedAt     string `json:"probed_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	// TenantID 资源归属租户（架构 D3 隔离）：空按 default 处理。接 CMDB 时由 CMDB 打租户标签，
	// 资源级隔离以本字段为准（Agent 级隔离仍看 Agent.tenant_id，两者在触达层复合生效）
	TenantID string `json:"tenant_id"`
}

// FlowStepSnapshot 步骤快照：创建流水线时把模板里的步骤定义整段拷下来。
// 模板文件后续演进（改顺序/加步骤）不影响历史流水线的可读性与可重放性。
type FlowStepSnapshot struct {
	ID       string   `json:"id"`
	Atom     string   `json:"atom"`
	Title    string   `json:"title"`
	Scope    string   `json:"scope"` // platform（平台侧自完成）/ agent / external
	When     string   `json:"when"`  // 步骤生效条件（由引擎的白名单求值器解析）
	Gate     string   `json:"gate"`  // blocker 时未通过则阻断下游
	Requires []string `json:"requires"`
	Hint     string   `json:"hint"`
	// Instruction 执行包模板（external 步骤下发物）：平台按资源 IP 渲染成可复制命令。
	// 模板来自 flow_templates/<mode>.yaml——下发物配置化，改命令不改代码
	Instruction string `json:"instruction,omitempty"`
}

// Flow 一次接入实例：一个资源 × 一种模式 × 一组能力 = 一条流水线。
type Flow struct {
	ID          int64              `json:"id"`
	ResourceID  string             `json:"resource_id"`
	ResourceIP  string             `json:"resource_ip"`
	Mode        string             `json:"mode"`        // edge / remote / hybrid
	TemplateID  string             `json:"template_id"` // 模式模板 id
	Abilities   []string           `json:"abilities"`   // 勾选的采集能力（插件名）
	AgentID     string             `json:"agent_id"`    // 承载能力的 Agent
	ParamsJSON  string             `json:"-"`           // 用户填写的插件参数：{ability_id: {field: value}}
	Steps       []FlowStepSnapshot `json:"steps"`       // 步骤快照（有序）
	CurrentStep string             `json:"current_step"`
	Status      string             `json:"status"` // running/done/failed/blocked/stalled/canceled
	StartedAt   string             `json:"started_at"`
	FinishedAt  string             `json:"finished_at"`
	UpdatedAt   string             `json:"updated_at"`
}

// FlowEvent 接入过程的单步事件。detail 承载原始数据（报错原文/实测值/样本/配置快照）。
type FlowEvent struct {
	ID         int64  `json:"id"`
	FlowID     int64  `json:"flow_id"`
	Step       string `json:"step"`
	Title      string `json:"title"`
	Status     string `json:"status"` // pending/running/ok/fail/blocked/skipped
	Scope      string `json:"scope"`  // platform/agent/external
	Summary    string `json:"summary"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}

// InitOnboard 创建接入中心三张表（幂等）
func (s *DB) InitOnboard() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS resources (
			id TEXT PRIMARY KEY,
			name TEXT DEFAULT '',
			ip TEXT DEFAULT '',
			resource_type TEXT DEFAULT 'host',
			role TEXT DEFAULT 'business',
			os TEXT DEFAULT '',
			arch TEXT DEFAULT '',
			kernel TEXT DEFAULT '',
			source TEXT DEFAULT 'manual',
			labels_json TEXT DEFAULT '{}',
			probe_json TEXT DEFAULT '{}',
			probed_at TEXT DEFAULT '',
			created_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS')),
			updated_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
		`CREATE TABLE IF NOT EXISTS onboard_flow (
			id SERIAL PRIMARY KEY,
			resource_id TEXT NOT NULL,
			resource_ip TEXT DEFAULT '',
			mode TEXT DEFAULT 'edge',
			template_id TEXT DEFAULT '',
			abilities_json TEXT DEFAULT '[]',
			steps_json TEXT DEFAULT '[]',
			params_json TEXT DEFAULT '{}',
			agent_id TEXT DEFAULT '',
			current_step TEXT DEFAULT '',
			status TEXT DEFAULT 'running',
			started_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS')),
			finished_at TEXT DEFAULT '',
			updated_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
		`CREATE TABLE IF NOT EXISTS onboard_event (
			id SERIAL PRIMARY KEY,
			flow_id INTEGER NOT NULL,
			step TEXT NOT NULL,
			title TEXT DEFAULT '',
			status TEXT NOT NULL,
			scope TEXT DEFAULT '',
			summary TEXT DEFAULT '',
			started_at TEXT DEFAULT '',
			finished_at TEXT DEFAULT '',
			duration_ms INTEGER DEFAULT 0,
			detail TEXT DEFAULT '',
			created_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_onboard_event_flow ON onboard_event(flow_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_onboard_flow_status ON onboard_flow(status, updated_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	// 增量列（幂等，跨驱动统一用 tableColumns 探测后再 ALTER）：
	// resources 多租户(D3, tenant_id) + SSH 凭据 + Agent 接入点 + 维护态（svc_state/installed_tag）。
	for _, alt := range []string{
		`ALTER TABLE resources ADD COLUMN tenant_id TEXT DEFAULT 'default'`,
		`ALTER TABLE resources ADD COLUMN ssh_port INTEGER DEFAULT 22`,
		`ALTER TABLE resources ADD COLUMN ssh_user TEXT DEFAULT ''`,
		`ALTER TABLE resources ADD COLUMN ssh_password TEXT DEFAULT ''`,
		`ALTER TABLE resources ADD COLUMN agent_console_url TEXT DEFAULT ''`,
		`ALTER TABLE resources ADD COLUMN target_kind TEXT DEFAULT ''`,
		`ALTER TABLE resources ADD COLUMN svc_state TEXT DEFAULT ''`,
		`ALTER TABLE resources ADD COLUMN installed_tag TEXT DEFAULT ''`,
	} {
		cols, err := s.tableColumns("resources")
		if err != nil {
			return err
		}
		name := alt[strings.Index(alt, "ADD COLUMN ")+len("ADD COLUMN "):]
		if i := strings.IndexAny(name, " "); i >= 0 {
			name = name[:i]
		}
		if cols[name] {
			continue
		}
		if _, err := s.db.Exec(alt); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------- 资源 ----------------------

// UpsertResource 新建或更新资源对象（按 id 覆盖；ssh_* 为空时保留旧值——
// 凭据是资源台账属性，重复 upsert 资源清单不应把已录入的凭据抹掉）
func (s *DB) UpsertResource(r *Resource) error {
	if r.LabelsJSON == "" {
		r.LabelsJSON = "{}"
	}
	_, err := s.db.Exec(`INSERT INTO resources(id,name,ip,resource_type,role,os,arch,kernel,source,labels_json,ssh_port,ssh_user,ssh_password,agent_console_url,target_kind)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		-- 标识字段空值不覆盖：部分更新（如只补 agent_console_url）不应把已登记的 ip/name 抹成空
		name=COALESCE(NULLIF(excluded.name,''), resources.name, ''),
		ip=COALESCE(NULLIF(excluded.ip,''), resources.ip, ''),
		resource_type=COALESCE(NULLIF(excluded.resource_type,''), resources.resource_type, ''),
		role=COALESCE(NULLIF(excluded.role,''), resources.role, ''),
		source=COALESCE(NULLIF(excluded.source,''), resources.source, ''),
		labels_json=COALESCE(NULLIF(excluded.labels_json,''), resources.labels_json, '{}'),
		os=COALESCE(NULLIF(excluded.os,''), resources.os, ''),
		arch=COALESCE(NULLIF(excluded.arch,''), resources.arch, ''),
		kernel=COALESCE(NULLIF(excluded.kernel,''), resources.kernel, ''),
		ssh_port=COALESCE(NULLIF(excluded.ssh_port,0), resources.ssh_port, 22),
		ssh_user=COALESCE(NULLIF(excluded.ssh_user,''), resources.ssh_user, ''),
		ssh_password=COALESCE(NULLIF(excluded.ssh_password,''), resources.ssh_password, ''),
		agent_console_url=COALESCE(NULLIF(excluded.agent_console_url,''), resources.agent_console_url, ''),
		target_kind=COALESCE(NULLIF(excluded.target_kind,''), resources.target_kind, ''),
		updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS')`,
		r.ID, r.Name, r.IP, r.ResourceType, r.Role, r.OS, r.Arch, r.Kernel, r.Source, r.LabelsJSON,
		r.SSHPort, r.SSHUser, r.SSHPassword, r.AgentConsoleURL, r.TargetKind)
	return err
}

// DeleteResource 删除资源对象（资源管理用）。删除前由 API 层做活跃流水线守卫，
// 这里只负责删行并如实回报是否真的删到（RowsAffected=0 说明 id 不存在）
func (s *DB) DeleteResource(id string) (bool, error) {
	r, err := s.db.Exec(`DELETE FROM resources WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// SetResourceInstalledTag 记录/清空该资源当前装着的版本包 tag（tag 为空 = 已卸载）。
// 单独一个写入口：UpsertResource 的"空值不覆盖"语义保护的是台账字段，
// 而 tag 必须能被显式清空，两者混在一起会互相打架
func (s *DB) SetResourceInstalledTag(id, tag string) error {
	_, err := s.db.Exec(`UPDATE resources SET installed_tag=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, tag, id)
	return err
}

// InstalledTagFromEvents 从事件流反推该资源当前装的是哪个 tag：取最近一条
// install_agent 成功事件里的 detail.tag。installed_tag 列是后加的，更早装成的机器
// 只有事件里有证据——宁可读证据，也不要让资源页对着真装过的机器显示"未记录 tag"。
// 卸载流水线不改写历史事件，所以这里要给"装过又被卸了"留判断余地：调用方只在
// 台账为空且资源当前有 Agent 时才用它
func (s *DB) InstalledTagFromEvents(resourceID string) (string, error) {
	rows, err := s.db.Query(`SELECT e.detail FROM onboard_event e
		JOIN onboard_flow f ON f.id = e.flow_id
		WHERE f.resource_id=? AND e.step='install_agent' AND e.status='ok'
		ORDER BY e.id DESC LIMIT 5`, resourceID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return "", err
		}
		// tag 的真实落点有两处：旧事件把安装器 detail 平铺在外层（tag），
		// 新事件把它嵌在 detail.detail（step 元信息在外层）——两种都要认
		var d struct {
			Tag    string `json:"tag"`
			Detail struct {
				Tag string `json:"tag"`
			} `json:"detail"`
		}
		if json.Unmarshal([]byte(detail), &d) == nil {
			if d.Tag != "" {
				return d.Tag, nil
			}
			if d.Detail.Tag != "" {
				return d.Detail.Tag, nil
			}
		}
	}
	return "", rows.Err()
}

// CountActiveFlowsByResource 该资源名下未终态（running/blocked/stalled）的流水线条数。
// 终态（done/failed/canceled）只是历史留痕，不阻止删除
func (s *DB) CountActiveFlowsByResource(resourceID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM onboard_flow
		WHERE resource_id=? AND status IN ('running','blocked','stalled')`, resourceID).Scan(&n)
	return n, err
}

// ListResources 按关键字与角色查询资源
func (s *DB) ListResources(query, role string) ([]*Resource, error) {
	q := `SELECT id,name,ip,resource_type,role,os,arch,kernel,source,labels_json,probe_json,probed_at,created_at,updated_at,COALESCE(ssh_port,22),COALESCE(ssh_user,''),COALESCE(ssh_password,''),COALESCE(agent_console_url,''),COALESCE(target_kind,''),COALESCE(svc_state,''),COALESCE(installed_tag,''),COALESCE(tenant_id,'default') FROM resources WHERE 1=1`
	args := []interface{}{}
	if query != "" {
		q += ` AND (id LIKE ? OR ip LIKE ? OR name LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	if role != "" {
		q += ` AND role=?`
		args = append(args, role)
	}
	q += ` ORDER BY updated_at DESC LIMIT 500`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Resource{}
	for rows.Next() {
		r := &Resource{}
		if err := rows.Scan(&r.ID, &r.Name, &r.IP, &r.ResourceType, &r.Role, &r.OS, &r.Arch,
			&r.Kernel, &r.Source, &r.LabelsJSON, &r.ProbeJSON, &r.ProbedAt, &r.CreatedAt, &r.UpdatedAt,
			&r.SSHPort, &r.SSHUser, &r.SSHPassword, &r.AgentConsoleURL, &r.TargetKind, &r.SvcState,
			&r.InstalledTag, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetResource 按 id 取资源
func (s *DB) GetResource(id string) (*Resource, error) {
	r := &Resource{}
	err := s.db.QueryRow(`SELECT id,name,ip,resource_type,role,os,arch,kernel,source,labels_json,probe_json,probed_at,created_at,updated_at,COALESCE(ssh_port,22),COALESCE(ssh_user,''),COALESCE(ssh_password,''),COALESCE(agent_console_url,''),COALESCE(target_kind,''),COALESCE(svc_state,''),COALESCE(installed_tag,''),COALESCE(tenant_id,'default')
		FROM resources WHERE id=?`, id).Scan(&r.ID, &r.Name, &r.IP, &r.ResourceType, &r.Role,
		&r.OS, &r.Arch, &r.Kernel, &r.Source, &r.LabelsJSON, &r.ProbeJSON, &r.ProbedAt, &r.CreatedAt, &r.UpdatedAt,
		&r.SSHPort, &r.SSHUser, &r.SSHPassword, &r.AgentConsoleURL, &r.TargetKind, &r.SvcState, &r.InstalledTag, &r.TenantID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// UpdateResourceProbe 主机探路（P1）结果回填：实测 OS/架构/内核 + 原始探路结果
func (s *DB) UpdateResourceProbe(id, probeJSON, osName, arch, kernel string) error {
	_, err := s.db.Exec(`UPDATE resources SET probe_json=?, os=?, arch=?, kernel=?,
		probed_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS'), updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`,
		probeJSON, osName, arch, kernel, id)
	return err
}

// UpdateResourceRole 调整资源角色（代理机被选用时登记为 platform_device）
func (s *DB) UpdateResourceRole(id, role string) error {
	_, err := s.db.Exec(`UPDATE resources SET role=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, role, id)
	return err
}

// SetResourceSvcState 登记启停维护态：stop 成功 → "stopped"（心跳消失不算失联）；
// start/restart 成功 → "running"；卸载完成 → ""（恢复常规语义）。
// 目标机操作与状态登记分两步落，登记失败只告警不回滚（目标机事实不因平台标记失败而改变）
func (s *DB) SetResourceSvcState(id, state string) error {
	_, err := s.db.Exec(`UPDATE resources SET svc_state=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, state, id)
	return err
}

// UpdateResourceTenant 调整资源归属租户（架构 D3 隔离）：行级租户赋值。
// 租户合法性由 API 层校验（必须存在于 tenants 表），本方法只做落库并如实回报路径深度。
func (s *DB) UpdateResourceTenant(id, tenant string) error {
	_, err := s.db.Exec(`UPDATE resources SET tenant_id=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, tenant, id)
	return err
}

// ---------------------- 流水线 ----------------------

// CreateFlow 新建一条接入流水线
func (s *DB) CreateFlow(f *Flow) (int64, error) {
	ab, _ := json.Marshal(f.Abilities)
	if f.Abilities == nil {
		ab = []byte("[]")
	}
	st, _ := json.Marshal(f.Steps)
	if f.Steps == nil {
		st = []byte("[]")
	}
	if f.ParamsJSON == "" {
		f.ParamsJSON = "{}"
	}
	return s.insertID(`INSERT INTO onboard_flow(resource_id,resource_ip,mode,template_id,abilities_json,steps_json,params_json,agent_id,current_step,status)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		f.ResourceID, f.ResourceIP, f.Mode, f.TemplateID, string(ab), string(st), f.ParamsJSON, f.AgentID, f.CurrentStep, f.Status)
}

// ListFlows 流水线列表（status 为空表示全部）
func (s *DB) ListFlows(status string, limit int) ([]*Flow, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id,resource_id,resource_ip,mode,template_id,abilities_json,steps_json,COALESCE(params_json,'{}'),agent_id,current_step,status,started_at,finished_at,updated_at FROM onboard_flow`
	args := []interface{}{}
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Flow{}
	for rows.Next() {
		f := &Flow{}
		var ab, st string
		if err := rows.Scan(&f.ID, &f.ResourceID, &f.ResourceIP, &f.Mode, &f.TemplateID, &ab, &st, &f.ParamsJSON,
			&f.AgentID, &f.CurrentStep, &f.Status, &f.StartedAt, &f.FinishedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Abilities = decodeStrList(ab)
		f.Steps = decodeSteps(st)
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFlow 取单条流水线
func (s *DB) GetFlow(id int64) (*Flow, error) {
	f := &Flow{}
	var ab, st string
	err := s.db.QueryRow(`SELECT id,resource_id,resource_ip,mode,template_id,abilities_json,steps_json,COALESCE(params_json,'{}'),agent_id,current_step,status,started_at,finished_at,updated_at
		FROM onboard_flow WHERE id=?`, id).Scan(&f.ID, &f.ResourceID, &f.ResourceIP, &f.Mode, &f.TemplateID,
		&ab, &st, &f.ParamsJSON, &f.AgentID, &f.CurrentStep, &f.Status, &f.StartedAt, &f.FinishedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.Abilities = decodeStrList(ab)
	f.Steps = decodeSteps(st)
	return f, nil
}

// decodeSteps 解析步骤快照；兼容早期仅存 id 字符串的形态
func decodeSteps(s string) []FlowStepSnapshot {
	out := []FlowStepSnapshot{}
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err == nil && len(out) > 0 {
		return out
	}
	for _, id := range decodeStrList(s) {
		out = append(out, FlowStepSnapshot{ID: id, Title: id, Scope: "platform"})
	}
	return out
}

// UpdateFlowStep 推进流水线当前步骤与状态
func (s *DB) UpdateFlowStep(id int64, step, status string) error {
	done := ""
	if status == "done" || status == "failed" || status == "canceled" {
		done = "finished_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS'),"
	}
	_, err := s.db.Exec(`UPDATE onboard_flow SET current_step=?, status=?, `+done+` updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`,
		step, status, id)
	return err
}

// UpdateFlowAgent 绑定承载能力的 Agent
func (s *DB) UpdateFlowAgent(id int64, agentID string) error {
	_, err := s.db.Exec(`UPDATE onboard_flow SET agent_id=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, agentID, id)
	return err
}

// UpdateFlowAbilities 回写勾选的能力清单（pick_ability 补齐地基能力后落库）
func (s *DB) UpdateFlowAbilities(id int64, abilities []string) error {
	if abilities == nil {
		abilities = []string{}
	}
	ab, err := json.Marshal(abilities)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE onboard_flow SET abilities_json=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`,
		string(ab), id)
	return err
}

// ListRunningFlows 供停滞判定使用（只看未结束的）
func (s *DB) ListRunningFlows() ([]*Flow, error) {
	return s.ListFlows("running", 500)
}

// ---------------------- 事件 ----------------------

// AppendEvent 追加一条步骤事件
func (s *DB) AppendEvent(e *FlowEvent) (int64, error) {
	if e.Status == "" {
		e.Status = "pending"
	}
	return s.insertID(`INSERT INTO onboard_event(flow_id,step,title,status,scope,summary,started_at,finished_at,duration_ms,detail)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.FlowID, e.Step, e.Title, e.Status, e.Scope, e.Summary, e.StartedAt, e.FinishedAt, e.DurationMs, e.Detail)
}

// UpdateEventDetail 就地改写某条事件的 detail（不改状态/时间戳）。
// 用途：执行端搬到 L1 后，步骤的"还在干活"信号由 L1 侧任务持有状态表达，
// L0 侧只刷新 running 事件里的 progress.last_activity_at——事件本身仍是同一条，
// 不会在时间线上多出一串"心跳"噪声。
func (s *DB) UpdateEventDetail(id int64, detail string) error {
	_, err := s.db.Exec(`UPDATE onboard_event SET detail=? WHERE id=?`, detail, id)
	return err
}

// ListEvents 取一条流水线的全部事件（时间正序）
func (s *DB) ListEvents(flowID int64) ([]*FlowEvent, error) {
	rows, err := s.db.Query(`SELECT id,flow_id,step,title,status,scope,summary,started_at,finished_at,duration_ms,detail,created_at
		FROM onboard_event WHERE flow_id=? ORDER BY id ASC`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FlowEvent{}
	for rows.Next() {
		e := &FlowEvent{}
		if err := rows.Scan(&e.ID, &e.FlowID, &e.Step, &e.Title, &e.Status, &e.Scope, &e.Summary,
			&e.StartedAt, &e.FinishedAt, &e.DurationMs, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestEvent 取某步最近一条事件（推进时判断当前状态）
func (s *DB) LatestEvent(flowID int64, step string) (*FlowEvent, error) {
	e := &FlowEvent{}
	err := s.db.QueryRow(`SELECT id,flow_id,step,title,status,scope,summary,started_at,finished_at,duration_ms,detail,created_at
		FROM onboard_event WHERE flow_id=? AND step=? ORDER BY id DESC LIMIT 1`, flowID, step).
		Scan(&e.ID, &e.FlowID, &e.Step, &e.Title, &e.Status, &e.Scope, &e.Summary,
			&e.StartedAt, &e.FinishedAt, &e.DurationMs, &e.Detail, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// CountFlowsByStatus 顶部统计条用的状态计数
func (s *DB) CountFlowsByStatus() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM onboard_flow GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Params 解析用户填写的插件参数：{ability_id: {field: value}}
func (f *Flow) Params() map[string]map[string]string {
	out := map[string]map[string]string{}
	s := strings.TrimSpace(f.ParamsJSON)
	if s == "" || s == "null" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		return map[string]map[string]string{}
	}
	return out
}

// decodeStrList 解析 JSON 字符串数组，容错空值与脏数据
func decodeStrList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}
