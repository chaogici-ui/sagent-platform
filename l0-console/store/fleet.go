package store

import (
	"database/sql"
	"encoding/json"
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
	LastTestAt     string `json:"last_test_at"`
	LastTestResult string `json:"last_test_result"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
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
			created_at TEXT DEFAULT (datetime('now','localtime')),
			updated_at TEXT DEFAULT (datetime('now','localtime'))
		)`,
		`CREATE TABLE IF NOT EXISTS targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			type TEXT DEFAULT 'mysql',
			address TEXT DEFAULT '',
			agent_id TEXT DEFAULT '',
			plugin TEXT DEFAULT '',
			note TEXT DEFAULT '',
			last_test_at TEXT DEFAULT '',
			last_test_result TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now','localtime')),
			updated_at TEXT DEFAULT (datetime('now','localtime'))
		)`,
		`CREATE TABLE IF NOT EXISTS agent_config (
			agent_id TEXT PRIMARY KEY,
			version INTEGER DEFAULT 1,
			content TEXT DEFAULT '',
			updated_at TEXT DEFAULT (datetime('now','localtime'))
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
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
	_, err := s.db.Exec(`INSERT INTO agents(id,name,type,host,port,plugins_json,version,labels_json,source,last_seen,stats_json,cfg_desired,cfg_effective)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, host=excluded.host, port=excluded.port,
		plugins_json=excluded.plugins_json, version=excluded.version, labels_json=excluded.labels_json,
		last_seen=excluded.last_seen, stats_json=excluded.stats_json, updated_at=datetime('now','localtime')`,
		a.ID, a.Name, a.Type, a.Host, a.Port, string(pl), a.Version, string(lb), a.Source, a.LastSeen, string(st), a.CfgDesired, a.CfgEffective)
	return err
}

// ListAgentRows 加载全部 Agent（启动时 hydrate 到内存）
func (s *DB) ListAgentRows() ([]*AgentRow, error) {
	rows, err := s.db.Query(`SELECT id,name,type,host,port,plugins_json,version,labels_json,source,last_seen,stats_json,cfg_desired,cfg_effective,created_at,updated_at FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentRow{}
	for rows.Next() {
		r := &AgentRow{}
		var pl, lb, st string
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Host, &r.Port, &pl, &r.Version, &lb, &r.Source, &r.LastSeen, &st, &r.CfgDesired, &r.CfgEffective, &r.CreatedAt, &r.UpdatedAt); err != nil {
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

// AgentHeartbeat 更新心跳：last_seen / 版本 / 生效配置 / 采集成败
func (s *DB) AgentHeartbeat(id, version string, cfgEffective string, stats map[string]interface{}) error {
	st, _ := json.Marshal(stats)
	res, err := s.db.Exec(`UPDATE agents SET last_seen=?, version=CASE WHEN ?!='' THEN ? ELSE version END,
		cfg_effective=?, stats_json=?, updated_at=datetime('now','localtime') WHERE id=?`,
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
	var ver int
	var cur string
	err := s.db.QueryRow(`SELECT version, content FROM agent_config WHERE agent_id=?`, agentID).Scan(&ver, &cur)
	if err == sql.ErrNoRows {
		if _, err := s.db.Exec(`INSERT INTO agent_config(agent_id, version, content) VALUES(?,1,?)`, agentID, content); err != nil {
			return 0, false, err
		}
		return 1, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if cur == content {
		return ver, false, nil
	}
	nv := ver + 1
	if _, err := s.db.Exec(`UPDATE agent_config SET version=?, content=?, updated_at=datetime('now','localtime') WHERE agent_id=?`, nv, content, agentID); err != nil {
		return 0, false, err
	}
	return nv, true, nil
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
	rows, err := s.db.Query(`SELECT id,name,type,address,IFNULL(agent_id,''),IFNULL(plugin,''),IFNULL(note,''),IFNULL(last_test_at,''),IFNULL(last_test_result,''),created_at,updated_at FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TargetRow{}
	for rows.Next() {
		t := &TargetRow{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Address, &t.AgentID, &t.Plugin, &t.Note, &t.LastTestAt, &t.LastTestResult, &t.CreatedAt, &t.UpdatedAt); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// InsertTarget 新增采集目标
func (s *DB) InsertTarget(t *TargetRow) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO targets(name,type,address,agent_id,plugin,note) VALUES(?,?,?,?,?,?)`,
		t.Name, t.Type, t.Address, t.AgentID, t.Plugin, t.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateTarget 编辑采集目标
func (s *DB) UpdateTarget(t *TargetRow) error {
	_, err := s.db.Exec(`UPDATE targets SET name=?, type=?, address=?, agent_id=?, plugin=?, note=?, updated_at=datetime('now','localtime') WHERE id=?`,
		t.Name, t.Type, t.Address, t.AgentID, t.Plugin, t.Note, t.ID)
	return err
}

// DeleteTarget 删除采集目标
func (s *DB) DeleteTarget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM targets WHERE id=?`, id)
	return err
}

// UpdateTargetTest 记录连通性测试结果
func (s *DB) UpdateTargetTest(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE targets SET last_test_at=datetime('now','localtime'), last_test_result=? WHERE id=?`, result, id)
	return err
}
