package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

func ar(action string) storepkg.AuditRow {
	return storepkg.AuditRow{
		Time: "2026-09-22 12:00:00", Operator: "admin",
		Action: action, Target: "order.prod.host.sagent-1", Scope: "接入中心", Result: "成功",
	}
}

// 任务历史 = 人工/运维操作记录：自动化回报类动作必须被排除，其余动作保留
func TestTaskRowsFromAudits(t *testing.T) {
	rows := []storepkg.AuditRow{
		ar("自动回报接入步骤"),
		ar("注册 Agent"),
		ar("ansible 探路完成"),
		ar("探路结果回填"),
		ar("执行器回报接入环节"),
		ar("ansible 扫描采集物（只读）"),
		ar("步骤超时判死"),
		ar("步骤超时（未完成）"),
		ar("步骤超时自动重试"),
		ar("发起卸载"),
		ar("发起升级"),
		ar("升级 Agent"),
		ar("配置变更"),
		ar("sync_metrics"),
		ar("新建采集目标"),
	}
	got := taskRowsFromAudits(rows)
	want := []string{"发起卸载", "发起升级", "升级 Agent", "配置变更", "sync_metrics", "新建采集目标"}
	if len(got) != len(want) {
		t.Fatalf("任务条数 = %d, 要 %d；实际动作: %v", len(got), len(want), actionsOf(got))
	}
	for i, w := range want {
		if got[i].Action != w {
			t.Fatalf("第 %d 条动作 = %q, 要 %q", i, got[i].Action, w)
		}
	}
	// 字段透传：落库行的六元组必须原样进入任务条目
	if got[0].Target != "order.prod.host.sagent-1" || got[0].Time != "2026-09-22 12:00:00" || got[0].Result != "成功" {
		t.Fatalf("字段透传不一致: %+v", got[0])
	}
}

func actionsOf(rows []TaskItem) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

// /api/audit 支持 limit 参数（默认 200，上限 AuditKeep）：审计页分页取数用
func TestAuditLimitParam(t *testing.T) {
	db := openTestCatalog(t)
	old := auditDB
	auditDB = db
	defer func() { auditDB = old }()
	for i := 0; i < 60; i++ {
		if err := db.InsertAudit(fmt.Sprintf("2026-01-01 00:%02d:00", i), "admin", "测试动作", "t", "s", "成功"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mux := http.NewServeMux()
	registerMiscRoutes(mux, nil)
	get := func(url string) int {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", url, nil))
		var rows []storepkg.AuditRow
		_ = json.Unmarshal(rr.Body.Bytes(), &rows)
		return len(rows)
	}
	if got := get("/api/audit?limit=50"); got != 50 {
		t.Fatalf("limit=50 返回 %d 条, 要 50", got)
	}
	if got := get("/api/audit"); got != 60 {
		t.Fatalf("默认返回 %d 条, 要 60（全部）", got)
	}
}
