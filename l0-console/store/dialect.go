package store

import (
	"database/sql"
	"strconv"
	"strings"
)

// pgDB 包装 *sql.DB：store 各文件的 SQL 一律用 ? 占位符书写，执行前经 rebind
// 翻译为 PG 的 $N，业务层调用点保持 s.db.Exec/Query/QueryRow 不变。
//
// 目录库已全面 PG（架构 D1），DDL/DML 均为 PG 原生写法（SERIAL / TIMESTAMP /
// COALESCE / ON CONFLICT），此处不再做任何方言改写。
type pgDB struct {
	db *sql.DB
}

func (d *pgDB) Exec(q string, args ...any) (sql.Result, error) {
	return d.db.Exec(rebind(q), args...)
}

func (d *pgDB) Query(q string, args ...any) (*sql.Rows, error) {
	return d.db.Query(rebind(q), args...)
}

func (d *pgDB) QueryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRow(rebind(q), args...)
}

// Begin 事务内 SQL 同样需要占位符翻译，故返回包装类型 pgTx。
func (d *pgDB) Begin() (*pgTx, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil
}

func (d *pgDB) Close() error { return d.db.Close() }

// pgTx 包装 *sql.Tx，方法签名与 *sql.Tx 一致，供事务内 SQL 做占位符翻译。
type pgTx struct {
	tx *sql.Tx
}

func (t *pgTx) Exec(q string, args ...any) (sql.Result, error) {
	return t.tx.Exec(rebind(q), args...)
}

func (t *pgTx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(rebind(q), args...)
}

func (t *pgTx) QueryRow(q string, args ...any) *sql.Row {
	return t.tx.QueryRow(rebind(q), args...)
}

func (t *pgTx) Commit() error   { return t.tx.Commit() }
func (t *pgTx) Rollback() error { return t.tx.Rollback() }

// rebind ? 占位符 → $N（PG 用 $1,$2...）。
// 跳过单引号字符串内部，避免误替换 SQL 字面量里的 ?。
func rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			// 简单处理转义引号（'' 续引号），逐字符翻转无引号状态
			if inStr && i+1 < len(q) && q[i+1] == '\'' {
				b.WriteByte(c)
				b.WriteByte(q[i+1])
				i++
				continue
			}
			inStr = !inStr
			b.WriteByte(c)
			continue
		}
		if c == '?' && !inStr {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}