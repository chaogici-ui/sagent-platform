package main

// testdb_test.go —— main 包单测的统一目录库载体（架构 D1：目录库唯一载体 PG）。
//
// 目录库已全面切 PG，单测载体同源：不再有 SQLite 分支，也不会「本地跑一套、生产跑另一套」。
// 隔离口径等价于 SQLite 时代的 t.TempDir()：每用例一个独立 schema，乱序/并发/重复跑都不互相污染。

import (
	"testing"

	"github.com/sagent/l0-console/internal/testpg"
	storepkg "github.com/sagent/l0-console/store"
)

// openTestCatalog 开一个隔离 schema 的 PG 目录库，并按 main 启动引导同款路径建齐全部业务表。
// CATALOG_TEST_DSN 未设置时用例跳过（本机没 PG 时不至于把整个包判死）。
func openTestCatalog(t *testing.T) *storepkg.DB {
	t.Helper()
	db, err := storepkg.OpenPostgres(testpg.Provision(t))
	if err != nil {
		t.Fatalf("open test catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, init := range []func() error{db.InitFleet, db.InitOnboard, db.InitL1Tasks, db.InitRelocations} {
		if err := init(); err != nil {
			t.Fatalf("init test catalog: %v", err)
		}
	}
	return db
}
