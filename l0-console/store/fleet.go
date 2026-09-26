package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// ---------- M0：Agent 注册/心跳、采集目标、配置版本化 ----------

// AgentRow 持久化的 Agent 实例
type AgentRow struct {
	ID           string
	Name         string
	Type         string // edge | proxy
	Host         string
	Port         int
	Plugins      []string
	Version      string
	Labels       map[string]string
	Source       string // docker: 本机演示容器; heartbeat: 注册接入
	LastSeen     int64  // unix 秒
	Stats        map[string]interface{}
	CfgDesired   int    // 服务端期望配置版本
	CfgEffective string // Agent 上报的生效配置 hash/版本
	ResourceID   string // 关联资源对象
	TenantID     string // 多租户(D3)：归属租户，default=默认租户
	CreatedAt    string
	UpdatedAt    string
}

// TargetRow 采集目标实体
type TargetRow struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Address        string `json:"address"`
	AgentID        string `json:"agent_id"`
	Plugin         string `json:"plugin"`
	Note           string `json:"note"`
	ParamsJSON     string `json:"params_json"` // 插件参数（端口/口令/频率/采集项），JSON
	ResourceID     string `json:"resource_id"` // 关联资源对象
	FlowID         int64  `json:"flow_id"`     // 由哪条接入流水线创建（0=手工建）
	LastTestAt     string `json:"last_test_at"`
	LastTestResult string `json:"last_test_result"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	TenantID      string `json:"tenant_id"` // 归属租户（架构 D3 隔离），空按 default
	Region        string `json:"region"`    // 候选地域（HA-3 跨 L1 迁移候选集限定）
}

// InitFleet 创建 M0 三张表（agents / targets / agent_config）
func (s *DB) InitFleet() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			name TEXT DEFAULT '',
			type TEXT DEFAULT 'edge',
			host TEXT DEFAULT '',
			port INTEGER DEFAULT 0,
			plugins_json TEXT DEFAULT '[]',
			version TEXT DEFAULT '',
			labels_json TEXT DEFAULT '{}',
			source TEXT DEFAULT 'heartbeat',
			last_seen INTEGER DEFAULT 0,
			stats_json TEXT DEFAULT '{}',
			cfg_desired INTEGER DEFAULT 0,
			cfg_effective TEXT DEFAULT '',
			created_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS')),
			updated_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
		`CREATE TABLE IF NOT EXISTS targets (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			type TEXT DEFAULT 'mysql',
			address TEXT DEFAULT '',
			agent_id TEXT DEFAULT '',
			plugin TEXT DEFAULT '',
			note TEXT DEFAULT '',
			last_test_at TEXT DEFAULT '',
			last_test_result TEXT DEFAULT '',
			created_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS')),
			updated_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
		`CREATE TABLE IF NOT EXISTS agent_config (
			agent_id TEXT PRIMARY KEY,
			version INTEGER DEFAULT 1,
			content TEXT DEFAULT '',
			updated_at TEXT DEFAULT (to_char(now(), 'YYYY-MM-DD HH24:MI:SS'))
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	// 增量列（幂等：tableColumns 探测后再 ALTER，重跑不会因列已存在而失败）。
	// targets：插件参数 / 关联资源 / 来源流水线；agents：关联资源。
	// 说明：不加 applied_version（cfg_effective 已承载生效版本）、不加 role（type 已承载 edge|proxy）。
	alters := []struct {
		table, ddl string
	}{
		{"targets", `ALTER TABLE targets ADD COLUMN params_json TEXT DEFAULT '{}'`},
		{"targets", `ALTER TABLE targets ADD COLUMN resource_id TEXT DEFAULT ''`},
		{"targets", `ALTER TABLE targets ADD COLUMN flow_id INTEGER DEFAULT 0`},
		{"agents", `ALTER TABLE agents ADD COLUMN resource_id TEXT DEFAULT ''`},
		{"agents", `ALTER TABLE agents ADD COLUMN tenant_id TEXT DEFAULT 'default'`},
		{"targets", `ALTER TABLE targets ADD COLUMN tenant_id TEXT DEFAULT 'default'`},
	{"targets", `ALTER TABLE targets ADD COLUMN region TEXT DEFAULT ''`},
	}
	for _, a := range alters {
		cols, err := s.tableColumns(a.table)
		if err != nil {
			return err
		}
		name := a.ddl[strings.Index(a.ddl, "ADD COLUMN ")+len("ADD COLUMN "):]
		if i := strings.IndexAny(name, " "); i >= 0 {
			name = name[:i]
		}
		if cols[name] {
			continue
		}
		if _, err := s.db.Exec(a.ddl); err != nil {
			return err
		}
	}
	return nil
}

// UpsertAgentRow 注册/更新 Agent
func (s *DB) UpsertAgentRow(a *AgentRow) error {
	pl, _ := json.Marshal(a.Plugins)
	lb, _ := json.Marshal(a.Labels)
	st, _ := json.Marshal(a.Stats)
	// tenant_id：Agent 心跳上报不带租户信息，仅首次注册落 default；
	// 已分配的租户通过 CASE 保留，避免心跳覆盖管理端分配的租户归属（终态/权限保护）。
	_, err := s.db.Exec(`INSERT INTO agents(id,name,type,host,port,plugins_json,version,labels_json,source,last_seen,stats_json,cfg_desired,cfg_effective,resource_id,tenant_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,COALESCE(?, 'default'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, host=excluded.host, port=excluded.port,
		plugins_json=excluded.plugins_json, version=excluded.version, labels_json=excluded.labels_json,
		last_seen=excluded.last_seen, stats_json=excluded.stats_json,
		resource_id=CASE WHEN excluded.resource_id<>'' THEN excluded.resource_id ELSE agents.resource_id END,
		tenant_id=CASE WHEN excluded.tenant_id NOT IN ('','default') THEN excluded.tenant_id ELSE agents.tenant_id END,
		updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS')`,
		a.ID, a.Name, a.Type, a.Host, a.Port, string(pl), a.Version, string(lb), a.Source, a.LastSeen, string(st), a.CfgDesired, a.CfgEffective, a.ResourceID, a.TenantID)
	return err
}

// ListAgentRows 加载全部 Agent（启动时 hydrate 到内存）
func (s *DB) ListAgentRows() ([]*AgentRow, error) {
	rows, err := s.db.Query(`SELECT id,name,type,host,port,plugins_json,version,labels_json,source,last_seen,stats_json,cfg_desired,cfg_effective,COALESCE(resource_id,''),COALESCE(tenant_id,'default'),created_at,updated_at FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentRow{}
	for rows.Next() {
		r := &AgentRow{}
		var pl, lb, st string
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Host, &r.Port, &pl, &r.Version, &lb, &r.Source, &r.LastSeen, &st, &r.CfgDesired, &r.CfgEffective, &r.ResourceID, &r.TenantID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}
		json.Unmarshal([]byte(pl), &r.Plugins)
		json.Unmarshal([]byte(lb), &r.Labels)
		json.Unmarshal([]byte(st), &r.Stats)
		out = append(out, r)
	}
	return out, nil
}

// GetAgentSource 查 Agent 来源（注册时防覆盖本机演示容器）
func (s *DB) GetAgentSource(id string) (string, error) {
	var src string
	err := s.db.QueryRow(`SELECT source FROM agents WHERE id=?`, id).Scan(&src)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return src, err
}

// DeleteAgent 注销 Agent 的 DB 行（重装/换机重新接入前清身份用），
// 返回是否真的删了行
func (s *DB) DeleteAgent(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agents WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteAgentConfig 删除 Agent 的期望配置轨道。
// 卸载（offboard）需把 Agent 的存在痕迹清干净：只删 agents 行会留下一条
// 悬挂的期望配置，重装时 sync_config 会因为"内容没变"而不再版本 +1，
// 界面看到的是"下发成功但版本没动"，容易误判成配置没生效
func (s *DB) DeleteAgentConfig(agentID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agent_config WHERE agent_id=?`, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetAgentResourceID 查 Agent 关联的资源对象 ID（未关联返回空串）
func (s *DB) GetAgentResourceID(id string) (string, error) {
	var rid string
	err := s.db.QueryRow(`SELECT resource_id FROM agents WHERE id=?`, id).Scan(&rid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return rid, err
}

// AgentHeartbeat 更新心跳：last_seen / 版本 / 生效配置 / 采集成败
func (s *DB) AgentHeartbeat(id, version string, cfgEffective string, stats map[string]interface{}) error {
	st, _ := json.Marshal(stats)
	res, err := s.db.Exec(`UPDATE agents SET last_seen=?, version=CASE WHEN ?!='' THEN ? ELSE version END,
		cfg_effective=?, stats_json=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`,
		time.Now().Unix(), version, version, cfgEffective, string(st), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAgentConfig 写入期望配置，内容有变化时版本 +1；返回 (新版本, 是否变更)
func (s *DB) SetAgentConfig(agentID, content string) (int, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var version int
	err = tx.QueryRow(`INSERT INTO agent_config(agent_id, version, content) VALUES(?,1,?)
		ON CONFLICT(agent_id) DO UPDATE SET version=agent_config.version+1,
		content=excluded.content, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS')
		WHERE agent_config.content<>excluded.content
		RETURNING version`, agentID, content).Scan(&version)
	changed := err == nil
	if err == sql.ErrNoRows {
		err = tx.QueryRow(`SELECT version FROM agent_config WHERE agent_id=?`, agentID).Scan(&version)
	}
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return version, changed, nil
}

// GetAgentConfig 读取期望配置
func (s *DB) GetAgentConfig(agentID string) (int, string, error) {
	var ver int
	var content string
	err := s.db.QueryRow(`SELECT version, content FROM agent_config WHERE agent_id=?`, agentID).Scan(&ver, &content)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return ver, content, err
}

// ---------- Targets CRUD ----------

// ListTargets 全部采集目标
func (s *DB) ListTargets() ([]*TargetRow, error) {
	rows, err := s.db.Query(`SELECT id,name,type,address,COALESCE(agent_id,''),COALESCE(plugin,''),COALESCE(note,''),COALESCE(params_json,'{}'),COALESCE(resource_id,''),COALESCE(flow_id,0),COALESCE(last_test_at,''),COALESCE(last_test_result,''),created_at,updated_at,COALESCE(tenant_id,'default'),COALESCE(region,'') FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TargetRow{}
	for rows.Next() {
		t := &TargetRow{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Address, &t.AgentID, &t.Plugin, &t.Note, &t.ParamsJSON, &t.ResourceID, &t.FlowID, &t.LastTestAt, &t.LastTestResult, &t.CreatedAt, &t.UpdatedAt, &t.TenantID, &t.Region); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// InsertTarget 新增采集目标
func (s *DB) InsertTarget(t *TargetRow) (int64, error) {
	if t.ParamsJSON == "" {
		t.ParamsJSON = "{}"
	}
	return s.insertID(`INSERT INTO targets(name,type,address,agent_id,plugin,note,params_json,resource_id,flow_id,tenant_id,region) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Type, t.Address, t.AgentID, t.Plugin, t.Note, t.ParamsJSON, t.ResourceID, t.FlowID, t.TenantID, t.Region)
}

// UpdateTarget 编辑采集目标
func (s *DB) UpdateTarget(t *TargetRow) error {
	if t.ParamsJSON == "" {
		t.ParamsJSON = "{}"
	}
	_, err := s.db.Exec(`UPDATE targets SET name=?, type=?, address=?, agent_id=?, plugin=?, note=?, params_json=?, tenant_id=?, region=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`,
		t.Name, t.Type, t.Address, t.AgentID, t.Plugin, t.Note, t.ParamsJSON, t.TenantID, t.Region, t.ID)
	return err
}

// DeleteTarget 删除采集目标
func (s *DB) DeleteTarget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM targets WHERE id=?`, id)
	return err
}

// UpdateTargetTest 记录连通性测试结果
func (s *DB) UpdateTargetTest(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE targets SET last_test_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS'), last_test_result=? WHERE id=?`, result, id)
	return err
}

// GetTarget 按 id 取采集目标
func (s *DB) GetTarget(id int64) (*TargetRow, error) {
	var t TargetRow
	err := s.db.QueryRow(`SELECT id,name,type,address,COALESCE(agent_id,''),COALESCE(plugin,''),COALESCE(note,''),COALESCE(params_json,'{}'),COALESCE(resource_id,''),COALESCE(flow_id,0),COALESCE(last_test_at,''),COALESCE(last_test_result,''),created_at,updated_at,COALESCE(tenant_id,'default'),COALESCE(region,'')
		FROM targets WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.Type, &t.Address, &t.AgentID, &t.Plugin, &t.Note, &t.ParamsJSON,
			&t.ResourceID, &t.FlowID, &t.LastTestAt, &t.LastTestResult, &t.CreatedAt, &t.UpdatedAt, &t.TenantID, &t.Region)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpdateTargetOwner 只迁移采集目标的归属 agent（HA-3 跨 L1 归属迁移）。
// 仅改 agent_id 与更新时间，不动 name/type/params 等其余字段，避免覆盖手工配置。
// 返回受影响行数；目标不存在返回 0。
func (s *DB) UpdateTargetOwner(id int64, toAgent string) (int64, error) {
	if toAgent == "" {
		// 空归属：清空 agent_id（解除分派，不写入 '')
		res, err := s.db.Exec(`UPDATE targets SET agent_id='', updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, id)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	res, err := s.db.Exec(`UPDATE targets SET agent_id=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, toAgent, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetTargetRegion 登记/更新目标的候选地域（HA-3 候选集限定）。
func (s *DB) SetTargetRegion(id int64, region string) error {
	_, err := s.db.Exec(`UPDATE targets SET region=?, updated_at=to_char(now(), 'YYYY-MM-DD HH24:MI:SS') WHERE id=?`, region, id)
	return err
}

// FindTargetByName 按名称查目标（接入流程幂等用：同名则复用不重复建）
func (s *DB) FindTargetByName(name string) (*TargetRow, error) {
	var t TargetRow
	err := s.db.QueryRow(`SELECT id FROM targets WHERE name=?`, name).Scan(&t.ID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetTarget(t.ID)
}
