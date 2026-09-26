package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// newVersionMux 测试用：只挂版本路由的最小 mux
func newVersionMux(t *testing.T, db *storepkg.DB) *http.ServeMux {
	t.Helper()
	m := http.NewServeMux()
	registerVersionRoutes(m, db)
	return m
}

// 只改状态（发布/弃用/停用）不得清掉兼容矩阵与说明：局部更新语义
func TestVersionStatusOnlyUpdatePreservesMatrix(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	cj := `[{"name":"mysqld_exporter","min_glibc":"2.17"}]`
	if err := db.UpsertVersion(storepkg.VersionRow{Tag: "v-p", Version: "0.7.0", OS: "linux", Arch: "amd64",
		Status: "draft", MinKernel: "3.10", ComponentsJSON: cj, Notes: "人工写的说明"}, "ui"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := newVersionMux(t, db)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("PUT", "/api/versions", strings.NewReader(`{"tag":"v-p","status":"stable"}`)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("状态更新应成功：%d %s", rr.Code, rr.Body.String())
	}
	rows, _ := db.ListVersions()
	r := rows[0]
	if r.Status != "stable" {
		t.Fatalf("状态应已更新：%+v", r)
	}
	if r.MinKernel != "3.10" || r.ComponentsJSON != cj || r.Notes != "人工写的说明" {
		t.Fatalf("只改状态不得清掉其它字段：%+v", r)
	}
}

// /api/versions 列出矩阵；PUT 编辑落 ui 来源；/api/versions/match 用实测环境预览匹配
func TestVersionAPIListEditMatch(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	if err := db.UpsertVersion(storepkg.VersionRow{Tag: "v-a", Version: "0.4.0", OS: "linux", Arch: "amd64",
		Status: "stable", MinKernel: "3.10"}, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := newVersionMux(t, db)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/versions", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "v-a") || !strings.Contains(rr.Body.String(), "status_options") {
		t.Fatalf("GET 应列出条目与状态枚举：%d %s", rr.Code, rr.Body.String())
	}

	body := `{"tag":"v-a","min_kernel":"4.19","status":"stable","notes":"收紧内核要求",
		"components":[{"name":"mysqld_exporter","min_glibc":"2.17"}]}`
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("PUT", "/api/versions", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("PUT 应成功：%d %s", rr.Code, rr.Body.String())
	}
	rows, _ := db.ListVersions()
	if rows[0].MinKernel != "4.19" || rows[0].Source != "ui" || !strings.Contains(rows[0].ComponentsJSON, "2.17") {
		t.Fatalf("编辑未落库：%+v", rows[0])
	}

	// 编辑后内存缓存立即生效（不必重启）
	saved := versionCatalog
	defer func() { versionCatalog = saved }()
	versionCatalog = rowsToSAVersions(rows)
	if len(versionCatalog) != 1 || versionCatalog[0].MinKernel != "4.19" {
		t.Fatalf("缓存未跟随编辑：%+v", versionCatalog)
	}

	// 匹配预览：目标机实测内核 3.10 < 4.19 → 必须不兼容并说明原因
	if err := db.UpsertResource(&storepkg.Resource{ID: "r-m", Name: "r-m", ResourceType: "host",
		Role: "business", OS: "linux", Arch: "amd64", Kernel: "3.10.0"}); err != nil {
		t.Fatalf("res: %v", err)
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/versions/match", strings.NewReader(`{"resource_id":"r-m"}`)))
	out := rr.Body.String()
	if !strings.Contains(out, `"compatible":false`) || !strings.Contains(out, "内核") {
		t.Fatalf("预览须给出不兼容结论与原因：%s", out)
	}

	// 未知 tag 的编辑必须被拒（防手滑）
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("PUT", "/api/versions", strings.NewReader(`{"tag":"nope"}`)))
	if !strings.Contains(rr.Body.String(), "版本清单中无") {
		t.Fatalf("未知 tag 应被拒：%s", rr.Body.String())
	}
}
