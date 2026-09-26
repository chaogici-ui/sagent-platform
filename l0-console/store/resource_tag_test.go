package store

import "testing"

func openOnboardTestDB(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.InitOnboard(); err != nil {
		t.Fatalf("InitOnboard: %v", err)
	}
	return db
}

// 装的是哪个 tag 必须落在台账上：Agent 自报版本是二进制里的常量（换 tag 不重建就不变），
// 不能当"这台机装的哪一版"用。装完写 tag、卸载清空，才是唯一可信口径
func TestSetResourceInstalledTag(t *testing.T) {
	db := openOnboardTestDB(t)
	if err := db.UpsertResource(&Resource{ID: "r1", IP: "10.0.0.1"}); err != nil {
		t.Fatalf("UpsertResource: %v", err)
	}
	if err := db.SetResourceInstalledTag("r1", "linux-arm64-0.4.1-dev"); err != nil {
		t.Fatalf("SetResourceInstalledTag: %v", err)
	}
	list, err := db.ListResources("", "")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListResources: %v %v", list, err)
	}
	if list[0].InstalledTag != "linux-arm64-0.4.1-dev" {
		t.Fatalf("安装 tag 应写入台账，实际 %q", list[0].InstalledTag)
	}

	// 普通 upsert（重新登记资源清单）不得把 tag 抹掉
	if err := db.UpsertResource(&Resource{ID: "r1", IP: "10.0.0.1", Name: "再次登记"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	list, _ = db.ListResources("", "")
	if list[0].InstalledTag != "linux-arm64-0.4.1-dev" {
		t.Fatalf("重新 upsert 资源清单不应清空安装 tag，实际 %q", list[0].InstalledTag)
	}

	// 卸载清空
	if err := db.SetResourceInstalledTag("r1", ""); err != nil {
		t.Fatalf("clear tag: %v", err)
	}
	list, _ = db.ListResources("", "")
	if list[0].InstalledTag != "" {
		t.Fatalf("卸载后 tag 应清空，实际 %q", list[0].InstalledTag)
	}
}

// installed_tag 列是后加的：更早装成的机器，事件流里有 tag 证据（install_agent ok 的
// detail.tag），必须能反推出来——否则资源页对着一台真的装过的机器显示"未记录 tag"
func TestInstalledTagFromEvents(t *testing.T) {
	db := openOnboardTestDB(t)
	flowID, err := db.CreateFlow(&Flow{ResourceID: "r1", ResourceIP: "10.0.0.1", Mode: "edge", Status: "done"})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	// 事件里 tag 的真实落点：step 元信息在外层，安装器自己的 detail 嵌在 detail.detail（2026-09-22 实测）
	if _, err := db.AppendEvent(&FlowEvent{FlowID: flowID, Step: "install_agent", Title: "安装 SAgent",
		Status: "ok", Scope: "external", Summary: "装好了",
		Detail: `{"source":"ansible_install","detail":{"tag":"linux-arm64-0.4.1-dev","sha256":"abc"}}`}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	got, err := db.InstalledTagFromEvents("r1")
	if err != nil {
		t.Fatalf("InstalledTagFromEvents: %v", err)
	}
	if got != "linux-arm64-0.4.1-dev" {
		t.Fatalf("应从事件 detail.detail.tag 反推安装版本，实际 %q", got)
	}
	// 没有成功安装事件 → 空（不能瞎猜）
	if got, _ := db.InstalledTagFromEvents("never-installed"); got != "" {
		t.Fatalf("无证据应返回空，实际 %q", got)
	}
}
