package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // 目录库唯一驱动：PostgreSQL（架构 D1）
)

// Plugin 插件定义（配置化，来源：夜莺移植 / 用户新建）
type Plugin struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	Category     string    `json:"category"`
	Type         string    `json:"type"`
	Icon         string    `json:"icon"`
	Version      string    `json:"version"`
	CollectDocMD string    `json:"collect_doc_md"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
	MetricCount  int       `json:"metric_count"`
	DashCount    int       `json:"dash_count"`
}

// Metric 指标定义（统一存储：精选/自定义/VM 自动发现；expression 即 PromQL）
type Metric struct {
	ID         int64     `json:"id"`
	PluginID   int64     `json:"plugin_id"` // 可为 0（尚未映射到目录插件）
	Name       string    `json:"name"`
	Unit       string    `json:"unit"`
	Note       string    `json:"note"`
	Expression string    `json:"expression"`
	MetricType string    `json:"metric_type"`
	Source     string    `json:"source"` // builtin / custom / vm_discovered / manual / kpi_seed
	Cat        string    `json:"cat"`
	Labels     string    `json:"labels"`
	Status     string    `json:"status"`
	PluginName string    `json:"plugin_name"` // 指标中心侧的归属标识（如 mysql_probe），映射到目录插件后为空
	UpdatedAt  string    `json:"updated_at"`
	CreatedAt  time.Time `json:"created_at"`

	// ---- 多渠道治理字段（D9）：KPI 种子填充，用户不可编辑（预置语义） ----
	Level           string `json:"level"`             // 资源层级：基础资源层/中间层…
	Major           string `json:"major"`             // 资源大类：主机设备类/容器类/数据库…
	Category        string `json:"category"`          // 资源名称：主机/Docker/MySQL…
	Grp             string `json:"grp"`               // 采集分组 key（配置开关用，与 Agent 注册表一致）
	Channel         string `json:"channel"`           // 来源渠道：builtin/exporter/script/sql/log
	Ownership       string `json:"ownership"`         // 归属：preset（只读可停用）/ custom（可增删改）
	SourceRef       string `json:"source_ref"`        // 溯源：产出插件/脚本/任务标识
	MinAgentVersion string `json:"min_agent_version"` // 需要的最低 Agent 版本
	Phase           int    `json:"phase"`             // 1=本期已实现 2=二期规划
	Freq            string `json:"freq,omitempty"`    // 建议采集频率（KPI 表原文）
	// Plugin 为只读展示字段：LEFT JOIN plugins 得到的目录插件名
	Plugin string `json:"plugin,omitempty"`
}

// Dashboard 仪表盘（configs_json 存夜莺原始结构）
type Dashboard struct {
	ID          int64     `json:"id"`
	PluginID    int64     `json:"plugin_id"`
	Name        string    `json:"name"`
	ConfigsJSON string    `json:"configs_json"`
	CreatedAt   time.Time `json:"created_at"`
}

// DB 存储封装（目录库唯一载体：PostgreSQL）
type DB struct {
	db *pgDB
}

// OpenPostgres 打开 PostgreSQL 目录库并初始化表结构。
// dsn 例如 postgres://user:pass@host:5432/db?sslmode=disable
func OpenPostgres(dsn string) (*DB, error) {
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	s := &DB{db: &pgDB{db: raw}}
	if err := s.init(); err != nil {
		raw.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭连接
func (s *DB) Close() error { return s.db.Close() }

// insertID 执行一条 INSERT 并返回自增 id。
// PG 不支持 LastInsertId，统一追加 RETURNING id 经 QueryRow 取回。
func (s *DB) insertID(query string, args ...any) (int64, error) {
	var id int64
	if err := s.db.QueryRow(query+" RETURNING id", args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// tableColumns 返回指定表的列名集合（幂等迁移用）。
// 按 current_schema() 限定：生产为 public，单测为每用例独立 schema，
// 不限定会把同名表在其它 schema 的列一并捞回来（重复列名污染迁移判断）。
func (s *DB) tableColumns(table string) (map[string]bool, error) {
	cols := map[string]bool{}
	rows, err := s.db.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name=$1`, table)
	if err != nil {
		return cols, err
	}
	defer rows.Close()
	var name string
	for rows.Next() {
		if err := rows.Scan(&name); err != nil {
			return cols, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func (s *DB) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS plugins (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			display_name TEXT DEFAULT '',
			category TEXT DEFAULT '',
			type TEXT DEFAULT 'exporter',
			icon TEXT DEFAULT '',
			version TEXT DEFAULT '',
			collect_doc_md TEXT DEFAULT '',
			source TEXT DEFAULT 'builtin',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS metrics (
			id SERIAL PRIMARY KEY,
			plugin_id INTEGER NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			unit TEXT DEFAULT '',
			note TEXT DEFAULT '',
			expression TEXT DEFAULT '',
			metric_type TEXT DEFAULT 'gauge',
			source TEXT DEFAULT 'builtin',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS dashboards (
			id SERIAL PRIMARY KEY,
			plugin_id INTEGER NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
			name TEXT DEFAULT '',
			configs_json TEXT DEFAULT '{}',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_plugin ON metrics(plugin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dashboards_plugin ON dashboards(plugin_id)`,
		// M2-⑪ 审计落库：操作审计持久化（重启不丢）
		`CREATE TABLE IF NOT EXISTS audit_log (
			id SERIAL PRIMARY KEY,
			time TEXT DEFAULT '',
			operator TEXT DEFAULT '',
			action TEXT DEFAULT '',
			target TEXT DEFAULT '',
			scope TEXT DEFAULT '',
			result TEXT DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(id DESC)`,
		// 多租户（架构 D3）：租户维度骨架。存量 Agent/资源默认归 default 租户，不破坏既有单租户行为。
		`CREATE TABLE IF NOT EXISTS tenants (
			id SERIAL PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			note TEXT DEFAULT '',
			api_token_hash TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO tenants (code, name, note) VALUES ('default', '默认租户', '存量资源归属的默认租户') ON CONFLICT DO NOTHING`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("init: %w", err)
		}
	}
	if err := s.migrateTenantToken(); err != nil {
		return fmt.Errorf("init migrate tenant token: %w", err)
	}
	if err := s.migrateMetricsV2(); err != nil {
		return err
	}
	if err := s.migrateMetricsV3(); err != nil {
		return err
	}
	return s.initVersions()
}

// migrateTenantToken 追加式迁移：为存量 tenants 表补 auth 凭证列（架构 D3 凭证鉴权）。
// 新库由 CREATE TABLE 直接含列，无需再 ALTER；旧库无损加列，存量 token_hash 为空=未签发。
func (s *DB) migrateTenantToken() error {
	cols, err := s.tableColumns("tenants")
	if err != nil {
		return err
	}
	if cols["api_token_hash"] {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE tenants ADD COLUMN api_token_hash TEXT DEFAULT ''`); err != nil {
		return err
	}
	return nil
}

// migrateMetricsV3 多渠道治理列（D9）：level/major/category/grp/channel/ownership/
// source_ref/min_agent_version/phase/freq —— 追加式迁移，旧库数据保留
func (s *DB) migrateMetricsV3() error {
	cols, err := s.tableColumns("metrics")
	if err != nil {
		return err
	}
	add := map[string]string{
		"level":             `ALTER TABLE metrics ADD COLUMN level TEXT DEFAULT ''`,
		"major":             `ALTER TABLE metrics ADD COLUMN major TEXT DEFAULT ''`,
		"category":          `ALTER TABLE metrics ADD COLUMN category TEXT DEFAULT ''`,
		"grp":               `ALTER TABLE metrics ADD COLUMN grp TEXT DEFAULT ''`,
		"channel":           `ALTER TABLE metrics ADD COLUMN channel TEXT DEFAULT ''`,
		"ownership":         `ALTER TABLE metrics ADD COLUMN ownership TEXT DEFAULT ''`,
		"source_ref":        `ALTER TABLE metrics ADD COLUMN source_ref TEXT DEFAULT ''`,
		"min_agent_version": `ALTER TABLE metrics ADD COLUMN min_agent_version TEXT DEFAULT ''`,
		"phase":             `ALTER TABLE metrics ADD COLUMN phase INTEGER DEFAULT 1`,
		"freq":              `ALTER TABLE metrics ADD COLUMN freq TEXT DEFAULT ''`,
	}
	for name, ddl := range add {
		if cols[name] {
			continue
		}
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("migrate metrics v3 add %s: %w", name, err)
		}
	}
	// 存量指标渠道归类回填（D9）：社区 exporter 派生 → exporter/预置；自定义脚本 → script/自定义
	backfills := []string{
		`UPDATE metrics SET channel='exporter', ownership='preset', source_ref=plugin_name
		 WHERE channel='' AND (plugin_name LIKE '%_exporter' OR plugin_name LIKE '%_probe' OR plugin_name IN ('MySQL','Redis','Kafka'))`,
		`UPDATE metrics SET channel='script', ownership='custom', source_ref='custom_scripts'
		 WHERE channel='' AND plugin_name='custom_scripts'`,
		`UPDATE metrics SET channel='builtin', ownership='preset', source_ref='host_metrics'
		 WHERE channel='' AND plugin_name='host_metrics'`,
	}
	for _, q := range backfills {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate metrics v3 backfill: %w", err)
		}
	}
	return nil
}

// migrateMetricsV2 统一指标存储：metrics 表放开 plugin_id 可空约束并增加
// cat/labels/status/plugin_name/updated_at 列（旧库一次性重建迁移，数据保留）
func (s *DB) migrateMetricsV2() error {
	cols, err := s.tableColumns("metrics")
	if err != nil {
		return err
	}
	if cols["cat"] && cols["plugin_name"] {
		return nil // 已是 V2
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE metrics_v2 (
			id SERIAL PRIMARY KEY,
			plugin_id INTEGER REFERENCES plugins(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			unit TEXT DEFAULT '',
			note TEXT DEFAULT '',
			expression TEXT DEFAULT '',
			metric_type TEXT DEFAULT 'gauge',
			source TEXT DEFAULT 'builtin',
			cat TEXT DEFAULT '',
			labels TEXT DEFAULT '',
			status TEXT DEFAULT '',
			plugin_name TEXT DEFAULT '',
			updated_at TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO metrics_v2(id, plugin_id, name, unit, note, expression, metric_type, source, created_at)
			SELECT id, COALESCE(plugin_id,0) AS plugin_id, name, unit, note, expression, metric_type, source, created_at FROM metrics`,
		`DROP TABLE metrics`,
		`ALTER TABLE metrics_v2 RENAME TO metrics`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_plugin ON metrics(plugin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_name ON metrics(name)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate metrics v2: %w", err)
		}
	}
	return tx.Commit()
}

// UpsertPlugin 移植器用：按 name 冲突时只更新元数据，保留用户可能改过的 collect_doc_md
func (s *DB) UpsertPlugin(p *Plugin) (int64, error) {
	q := `INSERT INTO plugins(name, display_name, category, type, icon, version, collect_doc_md, source)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET display_name=excluded.display_name, category=excluded.category,
		type=excluded.type, version=excluded.version,
		icon=CASE WHEN excluded.icon!='' THEN excluded.icon ELSE plugins.icon END`
	// INSERT..ON CONFLICT DO UPDATE RETURNING id：插入与更新两条路径都返回该行 id，无需回查
	return s.insertID(q, p.Name, p.DisplayName, p.Category, p.Type, p.Icon, p.Version, p.CollectDocMD, p.Source)
}

// UpdatePluginIcon 移植器用：为图标为空的插件回填静态服务路径
func (s *DB) UpdatePluginIcon(id int64, icon string) error {
	_, err := s.db.Exec(`UPDATE plugins SET icon=? WHERE id=?`, icon, id)
	return err
}

// UpdatePluginDoc 用户编辑采集说明
func (s *DB) UpdatePluginDoc(id int64, doc string) error {
	_, err := s.db.Exec(`UPDATE plugins SET collect_doc_md=? WHERE id=?`, doc, id)
	return err
}

// ListPlugins 插件列表（带指标/仪表盘计数）
func (s *DB) ListPlugins() ([]*Plugin, error) {
	rows, err := s.db.Query(`SELECT p.id, p.name, p.display_name, p.category, p.type, p.icon, p.version,
		p.collect_doc_md, p.source, p.created_at,
		(SELECT COUNT(*) FROM metrics m WHERE m.plugin_id=p.id),
		(SELECT COUNT(*) FROM dashboards d WHERE d.plugin_id=p.id)
		FROM plugins p ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Plugin
	for rows.Next() {
		p := &Plugin{}
		if err := rows.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Category, &p.Type, &p.Icon, &p.Version,
			&p.CollectDocMD, &p.Source, &p.CreatedAt, &p.MetricCount, &p.DashCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPluginByName 按目录名取插件
func (s *DB) GetPluginByName(name string) (*Plugin, error) {
	p := &Plugin{}
	err := s.db.QueryRow(`SELECT id, name, display_name, category, type, icon, version,
		collect_doc_md, source, created_at FROM plugins WHERE name=?`, name).
		Scan(&p.ID, &p.Name, &p.DisplayName, &p.Category, &p.Type, &p.Icon, &p.Version,
			&p.CollectDocMD, &p.Source, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetPlugin 单个插件
func (s *DB) GetPlugin(id int64) (*Plugin, error) {
	p := &Plugin{}
	err := s.db.QueryRow(`SELECT id, name, display_name, category, type, icon, version,
		collect_doc_md, source, created_at FROM plugins WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.DisplayName, &p.Category, &p.Type, &p.Icon, &p.Version,
			&p.CollectDocMD, &p.Source, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// InsertMetric 新增指标（builtin 移植与 custom 共用）
func (s *DB) InsertMetric(m *Metric) (int64, error) {
	return s.insertID(`INSERT INTO metrics(plugin_id, name, unit, note, expression, metric_type, source)
		VALUES(?,?,?,?,?,?,?)`, m.PluginID, m.Name, m.Unit, m.Note, m.Expression, m.MetricType, m.Source)
}

// DeleteMetric 删除指标（前端仅允许删 custom）
func (s *DB) DeleteMetric(id int64) error {
	_, err := s.db.Exec(`DELETE FROM metrics WHERE id=?`, id)
	return err
}

// ListMetrics 插件指标列表。view=curated 仅精选（builtin/custom），view=all 含 VM 自动发现全量。
// query 模糊匹配 name/note/expression
func (s *DB) ListMetrics(pluginID int64, query, typ, unit, view string) ([]*Metric, error) {
	q := `SELECT id, COALESCE(plugin_id,0) AS plugin_id, name, unit, note, expression, metric_type,
		source, cat, labels, status, plugin_name, updated_at, created_at
		FROM metrics WHERE plugin_id=?`
	args := []interface{}{pluginID}
	if view != "all" {
		q += ` AND source IN ('builtin','custom')`
	}
	if query != "" {
		q += ` AND (name LIKE ? OR note LIKE ? OR expression LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	if typ != "" {
		q += ` AND metric_type=?`
		args = append(args, typ)
	}
	if unit != "" {
		q += ` AND unit=?`
		args = append(args, unit)
	}
	q += ` ORDER BY source, name LIMIT 3000`
	return s.queryMetrics(q, args...)
}

// ListAllMetrics 指标中心全量视图（跨插件），plugin 字段为 JOIN 出的目录插件名，缺省回落 plugin_name
func (s *DB) ListAllMetrics(query, plugin string) ([]*Metric, error) {
	q := `SELECT m.id, COALESCE(m.plugin_id,0) AS plugin_id, m.name, m.unit, m.note, m.expression, m.metric_type,
		m.source, m.cat, m.labels, m.status, m.plugin_name, m.updated_at, m.created_at,
		COALESCE(p.name, m.plugin_name) AS plugin_label
		FROM metrics m LEFT JOIN plugins p ON m.plugin_id=p.id WHERE 1=1`
	args := []interface{}{}
	if query != "" {
		q += ` AND (m.name LIKE ? OR m.note LIKE ? OR m.expression LIKE ? OR m.cat LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like, like)
	}
	if plugin != "" {
		q += ` AND COALESCE(p.name, m.plugin_name)=?`
		args = append(args, plugin)
	}
	q += ` ORDER BY COALESCE(p.name, m.plugin_name), m.name LIMIT 5000`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Metric
	for rows.Next() {
		m := &Metric{}
		if err := rows.Scan(&m.ID, &m.PluginID, &m.Name, &m.Unit, &m.Note, &m.Expression,
			&m.MetricType, &m.Source, &m.Cat, &m.Labels, &m.Status, &m.PluginName, &m.UpdatedAt,
			&m.CreatedAt, &m.Plugin); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ResolvePluginID 按指标中心的归属标识解析目录插件 id（mysql_probe→MySQL、redis_exporter→Redis…）
// 解析不到返回 0
func (s *DB) ResolvePluginID(pluginName string) int64 {
	n := strings.ToLower(strings.TrimSpace(pluginName))
	for _, suf := range []string{"_probe", "_exporter"} {
		n = strings.TrimSuffix(n, suf)
	}
	if n == "" {
		return 0
	}
	var id int64
	s.db.QueryRow(`SELECT id FROM plugins WHERE lower(name)=?`, n).Scan(&id)
	return id
}

// UpsertMetricByName 按指标名全局 upsert（兼容旧 /api/metrics 语义：同名覆盖）
func (s *DB) UpsertMetricByName(m *Metric) error {
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.Status == "" {
		m.Status = "active"
	}
	if m.MetricType == "" {
		m.MetricType = "unknown"
	}
	if m.PluginName == "" && m.PluginID == 0 {
		m.PluginName = "manual"
	}
	var pid interface{}
	if m.PluginID > 0 {
		pid = m.PluginID
	}
	// 手动 upsert（name 无唯一约束，跨插件可能重名）
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM metrics WHERE name=? AND COALESCE(plugin_id,0)=?`,
		m.Name, m.PluginID).Scan(&existing)
	if err == sql.ErrNoRows {
		// 同名但不同插件也视为已占用：全局同名时覆盖旧行，保持旧 /api/metrics 语义
		err = s.db.QueryRow(`SELECT id FROM metrics WHERE name=?`, m.Name).Scan(&existing)
	}
	if err == nil {
		_, err = s.db.Exec(`UPDATE metrics SET plugin_id=?, unit=?, note=?, expression=?, metric_type=?,
			source=?, cat=?, labels=?, status=?, plugin_name=?, updated_at=? WHERE id=?`,
			pid, m.Unit, m.Note, m.Expression, m.MetricType,
			m.Source, m.Cat, m.Labels, m.Status, m.PluginName, m.UpdatedAt, existing)
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO metrics(plugin_id, name, unit, note, expression, metric_type,
		source, cat, labels, status, plugin_name, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		pid, m.Name, m.Unit, m.Note, m.Expression, m.MetricType,
		m.Source, m.Cat, m.Labels, m.Status, m.PluginName, m.UpdatedAt)
	return err
}

// InsertMetricIfAbsent 仅当指标名不存在时插入（metrics.json 种子导入用，不覆盖用户编辑）
func (s *DB) InsertMetricIfAbsent(m *Metric) error {
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.Status == "" {
		m.Status = "active"
	}
	if m.MetricType == "" {
		m.MetricType = "unknown"
	}
	var pid interface{}
	if m.PluginID > 0 {
		pid = m.PluginID
	}
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM metrics WHERE name=?`, m.Name).Scan(&existing)
	if err == nil {
		return nil // 已存在，跳过
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO metrics(plugin_id, name, unit, note, expression, metric_type,
		source, cat, labels, status, plugin_name, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		pid, m.Name, m.Unit, m.Note, m.Expression, m.MetricType,
		m.Source, m.Cat, m.Labels, m.Status, m.PluginName, m.UpdatedAt)
	return err
}

// CountMetrics 指标总数（指标中心响应 total 字段用）
func (s *DB) CountMetrics() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&n)
	return n, err
}

// SeedMetric 种子导入：不存在则插入；已存在但归属为空（早期种子）则仅回填 plugin 归属，
// 不覆盖 note/unit 等用户可能编辑过的字段
func (s *DB) SeedMetric(m *Metric) error {
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.Status == "" {
		m.Status = "active"
	}
	if m.MetricType == "" {
		m.MetricType = "unknown"
	}
	if m.PluginName == "" && m.PluginID == 0 {
		m.PluginName = "manual"
	}
	var pid interface{}
	if m.PluginID > 0 {
		pid = m.PluginID
	}
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM metrics WHERE name=?`, m.Name).Scan(&existing)
	if err == nil {
		// 回填早期种子的空归属 + 多渠道治理字段（种子所有，非用户编辑面）
		_, err = s.db.Exec(`UPDATE metrics SET
			plugin_id=CASE WHEN plugin_name='' AND source IN ('vm_discovered','manual') THEN ? ELSE plugin_id END,
			plugin_name=CASE WHEN plugin_name='' AND source IN ('vm_discovered','manual') THEN ? ELSE plugin_name END,
			channel=CASE WHEN channel='' THEN ? ELSE channel END,
			ownership=CASE WHEN ownership='' THEN ? ELSE ownership END,
			source_ref=CASE WHEN source_ref='' THEN ? ELSE source_ref END,
			min_agent_version=CASE WHEN min_agent_version='' THEN ? ELSE min_agent_version END,
			grp=CASE WHEN grp='' THEN ? ELSE grp END,
			level=CASE WHEN level='' THEN ? ELSE level END,
			major=CASE WHEN major='' THEN ? ELSE major END,
			category=CASE WHEN category='' THEN ? ELSE category END,
			phase=CASE WHEN phase=0 THEN ? ELSE phase END
			WHERE id=?`,
			pid, m.PluginName, m.Channel, m.Ownership, m.SourceRef, m.MinAgentVersion,
			m.Grp, m.Level, m.Major, m.Category, m.Phase, existing)
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO metrics(plugin_id, name, unit, note, expression, metric_type,
		source, cat, labels, status, plugin_name, updated_at,
		level, major, category, grp, channel, ownership, source_ref, min_agent_version, phase, freq)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pid, m.Name, m.Unit, m.Note, m.Expression, m.MetricType,
		m.Source, m.Cat, m.Labels, m.Status, m.PluginName, m.UpdatedAt,
		m.Level, m.Major, m.Category, m.Grp, m.Channel, m.Ownership, m.SourceRef, m.MinAgentVersion, m.Phase, m.Freq)
	return err
}

// BackfillMetricNote 说明回填：库中该指标说明为空时写入种子说明（不覆盖用户已编辑内容）
func (s *DB) BackfillMetricNote(name, note string) error {
	if name == "" || note == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE metrics SET note=? WHERE name=? AND (note='' OR note IS NULL)`, note, name)
	return err
}

// M2-⑪ 审计落库
// InsertAudit 写入一条操作审计；超量裁剪保留最近 AuditKeep 条
func (s *DB) InsertAudit(time, operator, action, target, scope, result string) error {
	if _, err := s.db.Exec(`INSERT INTO audit_log(time, operator, action, target, scope, result)
		VALUES(?,?,?,?,?,?)`, time, operator, action, target, scope, result); err != nil {
		return err
	}
	// 裁剪：仅保留最近 AuditKeep 条（低频操作，简单 DELETE 即可）
	_, err := s.db.Exec(`DELETE FROM audit_log WHERE id <= (SELECT MAX(id) FROM audit_log) - ?`, AuditKeep)
	return err
}

// AuditKeep 审计落库保留条数（导出：/api/audit 的 limit 参数以此为读取上限）
const AuditKeep = 5000

// AuditRow 落库审计行（与 main.go 的 AuditEntry 字段一一对应）
type AuditRow struct {
	Time     string `json:"time"`
	Operator string `json:"operator"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Scope    string `json:"scope"`
	Result   string `json:"result"`
}

// ListAudit 最近 limit 条审计（时间正序返回，与内存镜像语义一致）
func (s *DB) ListAudit(limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT time, operator, action, target, scope, result FROM audit_log
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditRow, 0)
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.Time, &r.Operator, &r.Action, &r.Target, &r.Scope, &r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	// 反转为时间正序（旧行在前），与原内存切片遍历顺序一致
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ReplacePluginMetrics 用声明集清理某插件旧指标（命名口径切换等 breaking 迁移用）
// 返回删除条数；keep 为新声明集（name 列表）
func (s *DB) ReplacePluginMetrics(pluginName string, keep []string) (int64, error) {
	if pluginName == "" || len(keep) == 0 {
		return 0, nil
	}
	ph := strings.Repeat("?,", len(keep))
	ph = ph[:len(ph)-1]
	args := make([]interface{}, 0, len(keep)+1)
	args = append(args, pluginName)
	for _, k := range keep {
		args = append(args, k)
	}
	res, err := s.db.Exec(`DELETE FROM metrics WHERE plugin_name=? AND name NOT IN (`+ph+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListPluginMetricExprs 返回插件的指标英文 key 集合（expression 优先，回退 name），供对账引擎做 VM 探测
func (s *DB) ListPluginMetricExprs(pluginID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT COALESCE(expression,''), name FROM metrics WHERE plugin_id=?`, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var expr, name string
		if err := rows.Scan(&expr, &name); err != nil {
			continue
		}
		if k := strings.TrimSpace(expr); k != "" {
			out = append(out, k)
		} else if strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// NameGrp 指标名 + 采集分组（生效配置展开用）
type NameGrp struct {
	Name string
	Grp  string
}

// ListMetricNameGrpByPluginName 按归属标识取指标名与分组 key（如 host_metrics），
// 供 D8 对账引擎把 Agent 生效配置（分组开关 + 例外清单）展开为应报指标集
func (s *DB) ListMetricNameGrpByPluginName(pluginName string) ([]NameGrp, error) {
	rows, err := s.db.Query(`SELECT name, COALESCE(grp,'') FROM metrics WHERE plugin_name=? AND COALESCE(phase,1)=1`, pluginName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameGrp
	for rows.Next() {
		var ng NameGrp
		if err := rows.Scan(&ng.Name, &ng.Grp); err != nil {
			continue
		}
		out = append(out, ng)
	}
	return out, rows.Err()
}

// ListPluginMetricNames 返回插件下全部指标名（移植器变更对比用）
func (s *DB) ListPluginMetricNames(pluginID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM metrics WHERE plugin_id=?`, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// DashRef 指标被仪表盘引用的条目
type DashRef struct {
	DashboardID int64  `json:"dashboard_id"`
	Dashboard   string `json:"dashboard"`
	Plugin      string `json:"plugin"`
}

// FindDashboardRefs 查询指标名被哪些仪表盘引用（configs_json 精确子串匹配，不走 LIKE 通配）
func (s *DB) FindDashboardRefs(metricName string) ([]DashRef, error) {
	rows, err := s.db.Query(`SELECT d.id, d.name, COALESCE(p.display_name, p.name, '')
		FROM dashboards d LEFT JOIN plugins p ON p.id = d.plugin_id
		WHERE INSTR(d.configs_json, ?) > 0`, metricName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []DashRef
	for rows.Next() {
		var r DashRef
		if err := rows.Scan(&r.DashboardID, &r.Dashboard, &r.Plugin); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// DeleteMetricByName 按指标名删除（指标中心 DELETE 语义）
func (s *DB) DeleteMetricByName(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM metrics WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateMetricsFields 批量按名更新非空字段（指标中心 PUT / 批量改状态语义）
func (s *DB) UpdateMetricsFields(updates []*Metric) (int, error) {
	updated := 0
	for _, u := range updates {
		if u.Name == "" {
			continue
		}
		sets, args := []string{}, []interface{}{}
		if u.Cat != "" {
			sets, args = append(sets, "cat=?"), append(args, u.Cat)
		}
		if u.MetricType != "" {
			sets, args = append(sets, "metric_type=?"), append(args, u.MetricType)
		}
		if u.Unit != "" {
			sets, args = append(sets, "unit=?"), append(args, u.Unit)
		}
		if u.Note != "" {
			sets, args = append(sets, "note=?"), append(args, u.Note)
		}
		if u.Labels != "" {
			sets, args = append(sets, "labels=?"), append(args, u.Labels)
		}
		if u.Status != "" {
			sets, args = append(sets, "status=?"), append(args, u.Status)
		}
		if u.PluginName != "" {
			if pid := s.ResolvePluginID(u.PluginName); pid > 0 {
				sets, args = append(sets, "plugin_id=?"), append(args, pid)
			}
			sets, args = append(sets, "plugin_name=?"), append(args, u.PluginName)
		}
		if len(sets) == 0 {
			continue
		}
		sets, args = append(sets, "updated_at=?"), append(args, u.UpdatedAt)
		args = append(args, u.Name)
		res, err := s.db.Exec(`UPDATE metrics SET `+strings.Join(sets, ", ")+` WHERE name=?`, args...)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, nil
}

func (s *DB) queryMetrics(q string, args ...interface{}) ([]*Metric, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Metric
	for rows.Next() {
		m := &Metric{}
		if err := rows.Scan(&m.ID, &m.PluginID, &m.Name, &m.Unit, &m.Note, &m.Expression,
			&m.MetricType, &m.Source, &m.Cat, &m.Labels, &m.Status, &m.PluginName, &m.UpdatedAt,
			&m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMetricsGovernance 指标中心治理视图（D9 五渠道）：层级/大类/渠道/分组/归属过滤
// query 匹配 名称/中文说明/口径；level/major/channel/grp 传空表示不过滤；onlyPhase1=true 排除二期条目；
// pluginName 按归属标识过滤（如 host_metrics，接入向导取分组清单用）
func (s *DB) ListMetricsGovernance(query, level, major, channel, grp, ownership string, onlyPhase1 bool, pluginName string) ([]*Metric, error) {
	q := `SELECT id, COALESCE(plugin_id,0) AS plugin_id, name, unit, note, expression, metric_type,
		source, cat, labels, status, plugin_name, updated_at, created_at,
		COALESCE(level,''), COALESCE(major,''), COALESCE(category,''), COALESCE(grp,''),
		COALESCE(channel,''), COALESCE(ownership,''), COALESCE(source_ref,''),
		COALESCE(min_agent_version,''), COALESCE(phase,1), COALESCE(freq,'')
		FROM metrics WHERE 1=1`
	args := []interface{}{}
	if query != "" {
		q += ` AND (name LIKE ? OR note LIKE ? OR expression LIKE ? OR cat LIKE ? OR plugin_name LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like, like, like)
	}
	if pluginName != "" {
		q += ` AND plugin_name=?`
		args = append(args, pluginName)
	}
	if level != "" {
		q += ` AND level=?`
		args = append(args, level)
	}
	if major != "" {
		q += ` AND major=?`
		args = append(args, major)
	}
	if channel != "" {
		q += ` AND channel=?`
		args = append(args, channel)
	}
	if grp != "" {
		q += ` AND grp=?`
		args = append(args, grp)
	}
	if ownership != "" {
		q += ` AND ownership=?`
		args = append(args, ownership)
	}
	if onlyPhase1 {
		q += ` AND COALESCE(phase,1)=1`
	}
	q += ` ORDER BY level, major, cat, name LIMIT 5000`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Metric
	for rows.Next() {
		m := &Metric{}
		if err := rows.Scan(&m.ID, &m.PluginID, &m.Name, &m.Unit, &m.Note, &m.Expression,
			&m.MetricType, &m.Source, &m.Cat, &m.Labels, &m.Status, &m.PluginName, &m.UpdatedAt,
			&m.CreatedAt, &m.Level, &m.Major, &m.Category, &m.Grp, &m.Channel, &m.Ownership,
			&m.SourceRef, &m.MinAgentVersion, &m.Phase, &m.Freq); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MetricGovFacets 治理视图 facets：层级树/渠道计数（树渲染用）
type GovFacets struct {
	Levels  map[string]map[string]int `json:"levels"`  // level -> major -> count
	Channel map[string]int            `json:"channel"` // channel -> count
	Grp     map[string]int            `json:"grp"`     // grp -> count
	Total   int                       `json:"total"`
}

func (s *DB) MetricGovFacets() (*GovFacets, error) {
	f := &GovFacets{Levels: map[string]map[string]int{}, Channel: map[string]int{}, Grp: map[string]int{}}
	rows, err := s.db.Query(`SELECT COALESCE(level,''), COALESCE(major,''), COALESCE(channel,''), COALESCE(grp,''), COUNT(*)
		FROM metrics GROUP BY level, major, channel, grp`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lv, mj, ch, gr string
		var n int
		if err := rows.Scan(&lv, &mj, &ch, &gr, &n); err != nil {
			continue
		}
		if f.Levels[lv] == nil {
			f.Levels[lv] = map[string]int{}
		}
		f.Levels[lv][mj] += n
		f.Channel[ch] += n
		if gr != "" {
			f.Grp[gr] += n
		}
		f.Total += n
	}
	return f, rows.Err()
}

// MetricTypes 插件下指标类型去重（前端过滤下拉用）
func (s *DB) MetricTypes(pluginID int64) ([]string, error) {
	return s.distinct(`SELECT DISTINCT metric_type FROM metrics WHERE plugin_id=? AND metric_type<>'' ORDER BY 1`, pluginID)
}

// MetricUnits 插件下指标单位去重
func (s *DB) MetricUnits(pluginID int64) ([]string, error) {
	return s.distinct(`SELECT DISTINCT unit FROM metrics WHERE plugin_id=? AND unit<>'' ORDER BY 1`, pluginID)
}

func (s *DB) distinct(q string, pluginID int64) ([]string, error) {
	rows, err := s.db.Query(q, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// InsertDashboard 插入仪表盘
func (s *DB) InsertDashboard(d *Dashboard) (int64, error) {
	return s.insertID(`INSERT INTO dashboards(plugin_id, name, configs_json) VALUES(?,?,?)`,
		d.PluginID, d.Name, d.ConfigsJSON)
}

// ListDashboards 仪表盘列表（不含大 JSON，避免列表页过重）
func (s *DB) ListDashboards(pluginID int64) ([]*Dashboard, error) {
	rows, err := s.db.Query(`SELECT id, plugin_id, name, created_at FROM dashboards
		WHERE plugin_id=? ORDER BY name`, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Dashboard
	for rows.Next() {
		d := &Dashboard{}
		if err := rows.Scan(&d.ID, &d.PluginID, &d.Name, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDashboard 仪表盘详情（含 configs_json）
func (s *DB) GetDashboard(id int64) (*Dashboard, error) {
	d := &Dashboard{}
	err := s.db.QueryRow(`SELECT id, plugin_id, name, configs_json, created_at FROM dashboards WHERE id=?`, id).
		Scan(&d.ID, &d.PluginID, &d.Name, &d.ConfigsJSON, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ReplacePluginDashboards 移植器重导时清空该插件仪表盘后重插
func (s *DB) ReplacePluginDashboards(pluginID int64, dashboards []*Dashboard) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM dashboards WHERE plugin_id=?`, pluginID); err != nil {
		return err
	}
	for _, d := range dashboards {
		if _, err := tx.Exec(`INSERT INTO dashboards(plugin_id, name, configs_json) VALUES(?,?,?)`,
			pluginID, d.Name, d.ConfigsJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplacePluginBuiltinMetrics 移植器重导时清空该插件 builtin 指标后重插（保留 custom）
func (s *DB) ReplacePluginBuiltinMetrics(pluginID int64, metrics []*Metric) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM metrics WHERE plugin_id=? AND source='builtin'`, pluginID); err != nil {
		return err
	}
	for _, m := range metrics {
		if _, err := tx.Exec(`INSERT INTO metrics(plugin_id, name, unit, note, expression, metric_type, source,
			plugin_name, channel, ownership, source_ref)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, pluginID, m.Name, m.Unit, m.Note, m.Expression, m.MetricType, m.Source,
			m.PluginName, m.Channel, m.Ownership, m.SourceRef); err != nil {
			return err
		}
	}
	return tx.Commit()
}
