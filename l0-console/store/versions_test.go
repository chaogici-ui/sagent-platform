package store

import "testing"

// 种子导入不得覆盖界面改过的行（否则每次重建镜像都会吞掉人工维护的兼容矩阵）
func TestVersionSeedDoesNotOverwriteUIEdit(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertVersion(VersionRow{Tag: "linux-amd64-0.4.1", Version: "0.4.1", Arch: "amd64",
		MinKernel: "3.10"}, "ui"); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	if err := db.UpsertVersionSeed(VersionRow{Tag: "linux-amd64-0.4.1", Version: "0.4.1", Arch: "amd64",
		MinKernel: "4.19"}); err != nil {
		t.Fatalf("UpsertVersionSeed: %v", err)
	}
	got, err := db.ListVersions()
	if err != nil || len(got) != 1 {
		t.Fatalf("ListVersions: %v %v", got, err)
	}
	if got[0].MinKernel != "3.10" || got[0].Source != "ui" {
		t.Fatalf("人工改动应保留，实际 %+v", got[0])
	}
}

// 种子自己拥有的行（source=seed）应被新种子更新
func TestVersionSeedUpdatesSeedOwnedRow(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertVersionSeed(VersionRow{Tag: "t1", Version: "0.4.0", Arch: "amd64", MinKernel: "2.6"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.UpsertVersionSeed(VersionRow{Tag: "t1", Version: "0.4.0", Arch: "amd64", MinKernel: "3.10"}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	got, _ := db.ListVersions()
	if got[0].MinKernel != "3.10" || got[0].Source != "seed" {
		t.Fatalf("种子行应被新种子更新，实际 %+v", got[0])
	}
}

// 组件声明（components_json）往返不失真，且默认值与时间戳落库
func TestVersionComponentsRoundtrip(t *testing.T) {
	db := openTestDB(t)
	cj := `[{"name":"mysqld_exporter","min_glibc":"2.17"}]`
	if err := db.UpsertVersion(VersionRow{Tag: "t2", Version: "0.4.1", Arch: "amd64", ComponentsJSON: cj}, "ui"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ := db.ListVersions()
	if got[0].ComponentsJSON != cj {
		t.Fatalf("组件声明往返失真：%q", got[0].ComponentsJSON)
	}
	if got[0].Status != "stable" || got[0].UpdatedAt == "" {
		t.Fatalf("默认值与时间戳未落：%+v", got[0])
	}
}
