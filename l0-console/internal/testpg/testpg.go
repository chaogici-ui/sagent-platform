// Package testpg 提供单测用的 PostgreSQL 目录库载体（架构 D1：目录库唯一载体 PG）。
//
// 隔离口径与 SQLite 时代的 t.TempDir() 等价：每个用例一个独立 schema，
// 同名表互不可见 —— 用例乱序、并发、重复跑都不会互相污染。
//
// 仅做「开辟/回收 schema」这一件事，不 import store：store 包自身的内部测试文件
// 也要用它，若这里反向依赖 store 会构成 import cycle。
package testpg

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Provision 为单个用例开辟独立 schema，返回带 search_path 的 DSN，
// 并注册 t.Cleanup 做 DROP SCHEMA ... CASCADE 回收。
// CATALOG_TEST_DSN 未设置时跳过用例（本机没有 PG 时不至于把整个包判死）。
func Provision(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CATALOG_TEST_DSN")
	if dsn == "" {
		t.Skip("CATALOG_TEST_DSN 未设置：目录库已全面 PG，单测需 PG 载体" +
			"（示例 postgres://sagent:sagent@127.0.0.1:5432/sagent?sslmode=disable）")
	}

	schema := fmt.Sprintf("t_%s_%d", strconv.FormatInt(time.Now().UnixNano(), 36), rand.Intn(100000))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg admin: %v", err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})

	return withSearchPath(dsn, schema)
}

// withSearchPath 把 schema 挂到 DSN 的 search_path 上。
// pgx 把非标准连接键透传为 PG 启动参数，search_path 因此对池内每条连接都生效。
func withSearchPath(dsn, schema string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}
