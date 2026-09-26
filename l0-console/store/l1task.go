package store

// l1task.go —— L1 任务源持久化（架构 D5 IN3 收口：任务源 PG 持久化）。
//
// 将原进程内内存队列（l1_task 传输层缓存）落到 catalog 库持久化任务表：
// 任务投递/拉取/回执均以数据库为唯一真相源，重启不丢、多 Controller 可各自拉取，
// 满足「任务源 PG 持久化任务表 + 切执行通道」随 G2 退役一次立项收口的正式落地形态。
// 写法沿用本项目惯例：SQL 以 ? 占位符书写，执行前统一 rebind 为 PG 的 $N。
//
// IN5 B2 增量：任务与流水线步骤绑定（flow_id/step_id/attempt）+ 结构化执行结果（result）
// + 对账消费标记（reconciled_at）。执行端搬到 L1 后，步骤终态不再由 L0 进程内作业直接落库，
// 而是由「L1 回执 → L0 对账桥」驱动；绑定与消费标记是这条链路可对账、可幂等的前提。

import (
	"encoding/json"
	"strings"
	"time"
)

// l1TaskCols 全量列序：所有 SELECT 共用一份，避免新增列时漏改某条查询导致 scan 错位。
const l1TaskCols = `id, controller_id, kind, payload, status, executor, note, ` +
	`flow_id, step_id, attempt, result, reconciled_at, created_at, acked_at`

// L1TaskRow l1_tasks 表行（持久化载荷；Payload/Result 以 JSON 文本存储）。
type L1TaskRow struct {
	ID           string
	ControllerID string
	Kind         string
	PayloadJSON  string
	Status       string // pending|dispatched|done|failed|executor_pending
	Executor     string
	Note         string
	// 与流水线步骤的绑定（IN5 B2）：未绑定的任务（如 controller-ping）三项为零值，
	// 不参与步骤对账。
	FlowID  int64
	StepID  string
	Attempt int // 步骤的第几次尝试；超时重试会换 attempt，旧尝试回执据此丢弃
	// ResultJSON L1 执行端回传的结构化结果（facts/tasks/recap/diagnosis 等）；
	// 只塞 note 字符串会让探路 facts 等结构化结论断链。
	ResultJSON string
	// ReconciledAt 对账消费时刻（0=未消费）：置位后不再重复驱动步骤，保证对账幂等。
	ReconciledAt int64
	CreatedAt    int64
	AckedAt      int64
}

// InitL1Tasks 建 l1_tasks 表（迁移式，IF NOT EXISTS；随 console 启动引导调用）。
func (s *DB) InitL1Tasks() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS l1_tasks (
			id TEXT PRIMARY KEY,
			controller_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			payload TEXT DEFAULT '',
			status TEXT DEFAULT 'pending',
			executor TEXT DEFAULT '',
			note TEXT DEFAULT '',
			flow_id INTEGER DEFAULT 0,
			step_id TEXT DEFAULT '',
			attempt INTEGER DEFAULT 1,
			result TEXT DEFAULT '',
			reconciled_at INTEGER DEFAULT 0,
			created_at INTEGER DEFAULT 0,
			acked_at INTEGER DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_l1_tasks_controller ON l1_tasks(controller_id, status)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	// 存量表增量列（幂等：tableColumns 探测后再 ALTER，重跑不会因列已存在而失败）。
	for _, alt := range []string{
		`ALTER TABLE l1_tasks ADD COLUMN flow_id INTEGER DEFAULT 0`,
		`ALTER TABLE l1_tasks ADD COLUMN step_id TEXT DEFAULT ''`,
		`ALTER TABLE l1_tasks ADD COLUMN attempt INTEGER DEFAULT 1`,
		`ALTER TABLE l1_tasks ADD COLUMN result TEXT DEFAULT ''`,
		`ALTER TABLE l1_tasks ADD COLUMN reconciled_at INTEGER DEFAULT 0`,
	} {
		cols, err := s.tableColumns("l1_tasks")
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
	// 对账扫描索引必须建在增量列补齐之后：存量表上 reconciled_at 是 ALTER 加出来的，
	// 放在建表语句里会让老库启动时直接 "no such column: reconciled_at"。
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_l1_tasks_reconcile ON l1_tasks(reconciled_at, status)`); err != nil {
		return err
	}
	return nil
}

// rowToL1Task scan 一行 l1_tasks 到结构体（列序与 l1TaskCols 一致）。
func rowToL1Task(row interface{ Scan(...any) error }) (*L1TaskRow, error) {
	t := &L1TaskRow{}
	if err := row.Scan(&t.ID, &t.ControllerID, &t.Kind, &t.PayloadJSON,
		&t.Status, &t.Executor, &t.Note, &t.FlowID, &t.StepID, &t.Attempt,
		&t.ResultJSON, &t.ReconciledAt, &t.CreatedAt, &t.AckedAt); err != nil {
		return nil, err
	}
	return t, nil
}

// QueueL1Task 幂等投递：ID 已存在 → 返回既有记录与 created=false（重复投递 SKIP，不退队列）。
// ID 为空时由时间戳生成（非幂等场景兜底）。
func (s *DB) QueueL1Task(t L1TaskRow) (*L1TaskRow, bool, error) {
	if t.ID == "" {
		t.ID = "l1task-" + itoa64(time.Now().UnixNano())
	}
	if t.Status == "" {
		t.Status = "pending"
	}
	if t.Attempt <= 0 {
		t.Attempt = 1
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	// 幂等：已存在直接返回既有行，不覆盖（保留原 status/note 防回执被重投覆盖）。
	if exist, err := s.GetL1Task(t.ID); err == nil && exist != nil {
		return exist, false, nil
	}
	if _, err := s.db.Exec(
		`INSERT INTO l1_tasks (id, controller_id, kind, payload, status, executor, note,
			flow_id, step_id, attempt, result, reconciled_at, created_at, acked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0)`,
		t.ID, t.ControllerID, t.Kind, t.PayloadJSON, t.Status, t.Executor, t.Note,
		t.FlowID, t.StepID, t.Attempt, t.ResultJSON, t.CreatedAt,
	); err != nil {
		return nil, false, err
	}
	created, _ := s.GetL1Task(t.ID)
	return created, true, nil
}

// GetL1Task 按 id 取单条（存在返回行，不存在返回 (nil,nil)）。
func (s *DB) GetL1Task(id string) (*L1TaskRow, error) {
	row := s.db.QueryRow(`SELECT `+l1TaskCols+` FROM l1_tasks WHERE id = ?`, id)
	t, err := rowToL1Task(row)
	if err != nil {
		// database/sql ErrNoRows → 视为不存在
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// PullL1Tasks 事务内将指定 Controller 名下全部 pending 任务置 dispatched 并取回：
// 一次性、防重复拉取/重复执行；并发下（多实例）以数据库行级原子性保证不重。
func (s *DB) PullL1Tasks(controllerID string) ([]*L1TaskRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 取符合条件 → 逐行置 dispatched（防御写入：本轮仅认 pending，状态翻转前再校验一次）
	rows, err := tx.Query(
		`SELECT `+l1TaskCols+` FROM l1_tasks WHERE controller_id = ? AND status = 'pending'`, controllerID)
	if err != nil {
		return nil, err
	}
	out := []*L1TaskRow{}
	for rows.Next() {
		t, err := rowToL1Task(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		if _, err := tx.Exec(`UPDATE l1_tasks SET status='dispatched' WHERE id = ? AND status = 'pending'`, t.ID); err != nil {
			return nil, err
		}
		t.Status = "dispatched" // 返回行反映事务后真相
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// PullAgentActions 事务内将指定 SAgent（controller_id=agentID 且 kind='agent-action'）名下
// pending 的采集动作任务置 dispatched 并取回。这是 G2 通道退役后「心跳拉取任务」（通道 A）
// 的交付端点：L0 心跳处理在此把该 SAgent 的待执行动作注入响应 tasks 字段。
// 一次性、防重复拉取/重复执行；与 PullL1Tasks 同套事务级原子语义。
func (s *DB) PullAgentActions(agentID string) ([]*L1TaskRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT `+l1TaskCols+` FROM l1_tasks WHERE controller_id = ? AND kind = 'agent-action' AND status = 'pending'`, agentID)
	if err != nil {
		return nil, err
	}
	out := []*L1TaskRow{}
	for rows.Next() {
		t, err := rowToL1Task(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		if _, err := tx.Exec(`UPDATE l1_tasks SET status='dispatched' WHERE id = ? AND status = 'pending'`, t.ID); err != nil {
			return nil, err
		}
		t.Status = "dispatched"
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// AckL1Task 回执任务终态与分流执行器（含结构化结果）。只认 exist 的已派发任务；返回是否命中。
// 回执同时置空 payload（SPEC D4）：载荷携带明文 SSH 口令，执行完毕后没有继续驻留库里的理由——
// 留在库里等于口令无限期可读。结论只保留 result/note。
func (s *DB) AckL1Task(id, executor, status, note, resultJSON string) (bool, error) {
	if status == "" {
		status = "done"
	}
	res, err := s.db.Exec(
		`UPDATE l1_tasks SET executor = ?, status = ?, note = ?, result = ?, payload = '', acked_at = ?
		 WHERE id = ?`, executor, status, note, resultJSON, time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListReconcilableL1Tasks 取「已绑定流水线步骤、已到终态、尚未对账」的任务（回执对账桥的输入）。
// 只认 done/failed：executor_pending 表示下游执行器尚未收口，不驱动步骤终态。
func (s *DB) ListReconcilableL1Tasks(limit int) ([]*L1TaskRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT `+l1TaskCols+` FROM l1_tasks
		 WHERE flow_id > 0 AND reconciled_at = 0 AND status IN ('done','failed')
		 ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*L1TaskRow{}
	for rows.Next() {
		t, err := rowToL1Task(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListInflightL1Tasks 取「已绑定步骤、仍在下游手上、尚无终态」的任务（pending 待拉取 / dispatched 已拉取执行中）。
// 用途：L0 侧活跃刷新——执行端搬到 L1 后，步骤的"还在干活"信号不再来自 L0 进程内心跳，
// 而来自"这条任务仍被 L1 持有且未回执"。清扫器据此刷新步骤活跃时间，避免把长安装误判为无响应
// （硬上限仍由 RunSec 兜底：执行端真死了，任务会一直停在这里，RunSec 到点照样判死）。
func (s *DB) ListInflightL1Tasks(limit int) ([]*L1TaskRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT `+l1TaskCols+` FROM l1_tasks
		 WHERE flow_id > 0 AND reconciled_at = 0 AND status IN ('pending','dispatched')
		 ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*L1TaskRow{}
	for rows.Next() {
		t, err := rowToL1Task(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasInflightL1Task 判断某步骤的某次尝试是否已有在飞任务（pending/dispatched 未回执）。
// 用途：活跃刷新与投递端判重——"这条任务仍被 L1 持有"是执行端搬到 L1 后唯一的在干活信号。
// 只认同 attempt：超时重试换 attempt 后是另一次执行。
func (s *DB) HasInflightL1Task(flowID int64, stepID string, attempt int) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM l1_tasks
		 WHERE flow_id = ? AND step_id = ? AND attempt = ? AND reconciled_at = 0
		   AND status IN ('pending','dispatched')`, flowID, stepID, attempt).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// L1TaskAttemptTaken 判断某步骤的某次尝试号是否已被「不会再回执」的任务占用。
// 占用 = 该 attempt 已有任务，且它既不在飞（pending/dispatched 且未对账）。
// 用途：投递端取号——D6 的任务 ID 编码 attempt，而人工「重试该步」只把步骤重置回
// pending、不递增 attempt；此时若沿用同 attempt，ID 会撞上已被消费的旧任务
// （QueueL1Task 幂等 SKIP），表现为"投递成功却永远等不到回执"，步骤卡死到超时。
func (s *DB) L1TaskAttemptTaken(flowID int64, stepID string, attempt int) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM l1_tasks
		 WHERE flow_id = ? AND step_id = ? AND attempt = ?
		   AND NOT (reconciled_at = 0 AND status IN ('pending','dispatched'))`,
		flowID, stepID, attempt).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkL1TaskReconciled 标记任务已被 L0 侧对账消费（幂等：重复调用只置一次时间）。
func (s *DB) MarkL1TaskReconciled(id string) error {
	_, err := s.db.Exec(
		`UPDATE l1_tasks SET reconciled_at = ? WHERE id = ? AND reconciled_at = 0`,
		time.Now().Unix(), id)
	return err
}

// L1TaskMetric L1 任务队列可观测汇总（PLAN 3.3 L0 侧：队列深度/失败/下发回执时延）。
type L1TaskMetric struct {
	Pending   int64 // 待下发（pending + dispatched 未回执）→ 队列深度
	OK        int64 // done
	Failed    int64 // failed（执行失败/超时/被拒）
	AvgAckMs  int64 // 有回执任务的 创建→回执 平均时延（ms）；无回执为 0
	AckedCount int64
}

// L1TaskMetrics 聚合 l1_tasks 队列与回执时延，供自监控/告警只读观测。
func (s *DB) L1TaskMetrics() (*L1TaskMetric, error) {
	m := &L1TaskMetric{}
	if s == nil || s.db == nil {
		return m, nil
	}
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN status NOT IN ('done','failed','canceled') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='done' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0),
		COALESCE(COUNT(CASE WHEN acked_at>0 THEN 1 END),0),
		COALESCE(ROUND(AVG(CASE WHEN acked_at>0 THEN (acked_at-created_at)*1000 ELSE NULL END),0),0)
		FROM l1_tasks`).
		Scan(&m.Pending, &m.OK, &m.Failed, &m.AckedCount, &m.AvgAckMs); err != nil {
		return nil, err
	}
	return m, nil
}

// ListL1Tasks 按 Controller（可选空=全部）返回任务清单（倒序），供观测/测试。
func (s *DB) ListL1Tasks(controllerID string, limit int) ([]*L1TaskRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + l1TaskCols + ` FROM l1_tasks`
	args := []any{}
	if controllerID != "" {
		q += ` WHERE controller_id = ?`
		args = append(args, controllerID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*L1TaskRow{}
	for rows.Next() {
		t, err := rowToL1Task(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// itoa64 极简正整数转十进制（store 包内复用，避免与 main.itoa 冲突）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// MarshalPayload 便捷：map 载荷 → JSON 文本（写 DB）；nil→"{}"。
func MarshalPayload(p map[string]any) string {
	if len(p) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(p)
	if b == nil {
		return "{}"
	}
	return string(b)
}

// UnmarshalPayload JSON 文本 → map（读 DB）；空/坏数据 → 空 map 不报错。
func UnmarshalPayload(s string) map[string]any {
	m := map[string]any{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}