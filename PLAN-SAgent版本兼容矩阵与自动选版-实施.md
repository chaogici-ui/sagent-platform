# SAgent 版本兼容矩阵与自动选版 —— 实施计划（P1）

> **给执行者：** 本计划配合 `PLAN-SAgent版本兼容矩阵与自动选版.md`（设计）使用，按任务逐条执行，每步以「门禁绿」为完成标志。
> 本仓库当前有大量在建未提交改动，**本计划不包含 git 提交步骤**（提交由用户决定）；每个任务以 `go vet` + `go test` 通过收口。
> 命令约定：`GO=/Users/gici/tools/go/bin/go`；控制面工作目录 `code/l0-console`，数据面 `code/sagent`。

**Goal:** 让装机/升级的版本选择从"人工停等"变为"平台按目标机实测环境自动选定"，并把兼容性写成可维护、可视化的数据。

**Architecture:** 版本元数据落目录库（PostgreSQL，`sa_versions` 表，`versions.yaml` 降为种子按 tag 合并）；兼容性判据 = `arch 相等 && kernel ≥ 下限 && 目标 glibc ≥ 组件要求`；匹配逻辑做成纯函数 `selectVersion`，由 `pick_version` 原子调用；无候选命中落 fail 并给出"差哪一项"的诊断，人工通过已有的「指定版本（需理由）」例外通道覆盖。
**Tech Stack:** Go 1.25（pgx/v5，无 CGO）、原生 JS 前端、ansible 探路采集。

---

## 文件结构（谁负责什么）

| 文件 | 职责 | 动作 |
|---|---|---|
| `l0-console/store/versions.go` | `sa_versions` 表 DDL + 版本行 CRUD + 种子合并语义 | 新建 |
| `l0-console/store/versions_test.go` | 上者单测 | 新建 |
| `l0-console/store/store.go` | init 末尾串联 `initVersions()` | 改 1 行 |
| `l0-console/version_match.go` | `TargetEnv` / `VersionComponent` / `verAtLeast` / `versionCompatible` / `selectVersion` / `parseGlibcVersion` / `targetEnvOf` | 新建 |
| `l0-console/version_match_test.go` | 匹配算法与解析器单测 | 新建 |
| `l0-console/onboard_versions.go` | `SAVersion` 增字段；`loadVersionCatalog` 改为「解析 yaml → 合并入目录库 → 从库读回内存缓存」 | 改 |
| `l0-console/onboard_atoms.go` | `atomPickVersion` 由"停等人工"改为"自动匹配" | 改 ~45 行 |
| `l0-console/onboard_ansible.go` | 探路 env_check 增 glibc 探测；probe_json 增 `glibc` | 改 ~6 行 |
| `l0-console/onboard_api.go` | 步骤 force 通道支持 `version_tag`（人工指定版本，留痕） | 改 ~15 行 |
| `l0-console/version_api.go` | `/api/versions`（GET/PUT）、`/api/versions/match` | 新建 |
| `l0-console/version_api_test.go` | 上者单测 | 新建 |
| `l0-console/static/js/version.js` | 「版本与兼容性」页：列表 + 编辑弹层 + 匹配预览 | 新建 |
| `l0-console/static/index.html` | 菜单入口 | 改 1 行 |
| `l0-console/main.go` | `loadVersionCatalog` 调用点 + `registerVersionRoutes` + 修正设计文档 §7 描述 | 改 3 行 |
| `l0-console/onboard_test.go` | 旧"停等人工"语义测试改写为"自动选定"语义 | 改 ~40 行 |

**已核对的关键事实（省得重复查）：**
- `pick_version` 模板步骤已是 `scope: platform`（`flow_templates/{edge,remote,hybrid,upgrade}.yaml`），停等是**原子内部**写出的 → **模板不需要改**。
- `pickedVersionTag()`（`onboard_ansible.go:2186`）读「`pick_version` 步骤最近一条 **stOK** 事件 detail 里的 `tag`」→ 自动选定只要产出同形状事件，安装作业零改动。
- 迁移是函数式（`migrateMetricsV2/V3`），`init()` 末尾 `return s.migrateMetricsV3()`。
- 已有 `normArch`（aarch64→arm64）、`semverLess`（点分数字比较）可复用。

---

## Task 1：版本表与种子合并语义（store 层）

**Files:**
- Create: `l0-console/store/versions.go`
- Create: `l0-console/store/versions_test.go`
- Modify: `l0-console/store/store.go`（init 末尾）

- [ ] **Step 1: 写失败测试** `store/versions_test.go`

```go
package store

import "testing"

func TestVersionSeedDoesNotOverwriteUIEdit(t *testing.T) {
	db := openTestDB(t) // 本包既有夹具（store_test.go:9）
	if err := db.UpsertVersion(VersionRow{Tag: "linux-amd64-0.4.1", Version: "0.4.1", Arch: "amd64",
		MinKernel: "3.10", Source: "ui"}, "ui"); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	// 镜像种子同 tag 再来一次：人工改动不许被覆盖
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

func TestVersionSeedUpdatesSeedOwnedRow(t *testing.T) {
	db := testDB(t)
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

func TestVersionComponentsRoundtrip(t *testing.T) {
	db := testDB(t)
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
```

- [ ] **Step 2: 跑测试确认失败**

`cd code/l0-console && $GO test ./store/ -run TestVersion -v`
预期：编译失败 `undefined: VersionRow / UpsertVersionSeed`。

- [ ] **Step 3: 实现** `store/versions.go`

```go
package store

import (
	"database/sql"
)

// VersionRow SAgent 版本登记行（目录库 PostgreSQL 权威；data/versions.yaml 只作种子）。
// 兼容性判据：arch + min_kernel + 包内组件（components_json）要求
type VersionRow struct {
	Tag            string `json:"tag"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SHA256         string `json:"sha256"`
	Status         string `json:"status"` // stable / deprecated / disabled
	Released       string `json:"released"`
	Notes          string `json:"notes"`
	MinKernel      string `json:"min_kernel"`
	ComponentsJSON string `json:"components_json"`
	Source         string `json:"source"` // seed / ui
	UpdatedAt      string `json:"updated_at"`
	UpdatedBy      string `json:"updated_by"`
}

func (s *DB) initVersions() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sa_versions (
		tag             TEXT PRIMARY KEY,
		version         TEXT NOT NULL DEFAULT '',
		os              TEXT NOT NULL DEFAULT '',
		arch            TEXT NOT NULL DEFAULT '',
		sha256          TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL DEFAULT 'stable',
		released        TEXT NOT NULL DEFAULT '',
		notes           TEXT NOT NULL DEFAULT '',
		min_kernel      TEXT NOT NULL DEFAULT '',
		components_json TEXT NOT NULL DEFAULT '[]',
		source          TEXT NOT NULL DEFAULT 'seed',
		updated_at      TEXT NOT NULL DEFAULT '',
		updated_by      TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// ListVersions 全量版本行（版本号降序，便于直接给界面用）
func (s *DB) ListVersions() ([]VersionRow, error) {
	rows, err := s.db.Query(`SELECT tag, version, os, arch, sha256, status, released, notes,
		min_kernel, IFNULL(components_json,'[]'), IFNULL(source,'seed'),
		IFNULL(updated_at,''), IFNULL(updated_by,'')
		FROM sa_versions ORDER BY version DESC, tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionRow{}
	for rows.Next() {
		var r VersionRow
		if err := rows.Scan(&r.Tag, &r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Status, &r.Released,
			&r.Notes, &r.MinKernel, &r.ComponentsJSON, &r.Source, &r.UpdatedAt, &r.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertVersion 写入/更新版本行；source 标识来源（ui = 人工编辑，seed = 清单导入）
func (s *DB) UpsertVersion(r VersionRow, source string) error {
	if r.ComponentsJSON == "" {
		r.ComponentsJSON = "[]"
	}
	if r.Status == "" {
		r.Status = "stable"
	}
	_, err := s.db.Exec(`INSERT INTO sa_versions
		(tag, version, os, arch, sha256, status, released, notes, min_kernel, components_json,
		 source, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?, datetime('now','localtime'), ?)
		ON CONFLICT(tag) DO UPDATE SET
			version=excluded.version, os=excluded.os, arch=excluded.arch, sha256=excluded.sha256,
			status=excluded.status, released=excluded.released, notes=excluded.notes,
			min_kernel=excluded.min_kernel, components_json=excluded.components_json,
			source=excluded.source, updated_at=datetime('now','localtime'), updated_by=excluded.updated_by`,
		r.Tag, r.Version, r.OS, r.Arch, r.SHA256, r.Status, r.Released, r.Notes,
		r.MinKernel, r.ComponentsJSON, source, r.UpdatedBy)
	return err
}

// UpsertVersionSeed 镜像种子导入：不存在则插入；已存在且 source=seed 才更新——
// 界面改过（source=ui）的行不被镜像覆盖，否则每次重建镜像都会吞掉人工维护的兼容矩阵
func (s *DB) UpsertVersionSeed(r VersionRow) error {
	var src string
	err := s.db.QueryRow(`SELECT IFNULL(source,'seed') FROM sa_versions WHERE tag=?`, r.Tag).Scan(&src)
	if err == sql.ErrNoRows {
		return s.UpsertVersion(r, "seed")
	}
	if err != nil {
		return err
	}
	if src == "ui" {
		return nil
	}
	return s.UpsertVersion(r, "seed")
}
```

`store.go` 的 init 末尾改为：

```go
	if err := s.migrateMetricsV3(); err != nil {
		return err
	}
	return s.initVersions()
```

- [ ] **Step 4: 跑测试确认通过**

`$GO test ./store/ -run TestVersion -v` → 三个测试 PASS。
风险点：本包既有夹具是 `openTestDB(t)`（`store_test.go:9`），照它建库即可。

- [ ] **Step 5: 门禁** `$GO vet ./... && $GO test ./...`

---

## Task 2：兼容性判据与匹配算法（纯函数）

**Files:**
- Create: `l0-console/version_match.go`
- Create: `l0-console/version_match_test.go`

- [ ] **Step 1: 写失败测试** `version_match_test.go`

```go
package main

import (
	"strings"
	"testing"
)

func mkVer(tag, ver, arch, minKernel, status string, comps ...VersionComponent) SAVersion {
	return SAVersion{Tag: tag, Version: ver, Arch: arch, OS: "linux", Status: status,
		MinKernel: minKernel, Components: comps}
}

var mysqlExp = VersionComponent{Name: "mysqld_exporter", MinGlibc: "2.17"}
var vectorStatic = VersionComponent{Name: "vector", Static: true}

func TestVersionCompatibleReasons(t *testing.T) {
	env := TargetEnv{Arch: "amd64", Kernel: "3.10.0", Glibc: "2.17"}
	// 架构不符
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "arm64", "", "stable"), env); ok || !strings.Contains(why, "架构") {
		t.Errorf("架构不符应落选并说明：ok=%v why=%q", ok, why)
	}
	// 内核不足
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "4.19", "stable"), env); ok || !strings.Contains(why, "内核") {
		t.Errorf("内核不足应落选并说明：ok=%v why=%q", ok, why)
	}
	// glibc 不足
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", mysqlExp),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "2.17"}); ok || !strings.Contains(why, "glibc") {
		t.Errorf("glibc 不足应落选并说明：ok=%v why=%q", ok, why)
	}
	// 目标为 musl：glibc 依赖组件直接不兼容；静态组件不参与
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", mysqlExp),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "musl"}); ok || !strings.Contains(why, "musl") {
		t.Errorf("musl 目标应落选：ok=%v why=%q", ok, why)
	}
	if ok, _, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", vectorStatic),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "musl"}); !ok {
		t.Errorf("静态组件不应产生约束")
	}
	// 判据字段缺失 → 不阻断但标记未核实
	ok, _, un := versionCompatible(mkVer("a", "1.0", "amd64", "3.10", "stable", mysqlExp),
		TargetEnv{Arch: "amd64"})
	if !ok || !un {
		t.Errorf("缺内核/glibc 时应判兼容但标记未核实：ok=%v unverified=%v", ok, un)
	}
}

func TestSelectVersionPicksHighestStable(t *testing.T) {
	env := TargetEnv{Arch: "amd64", Kernel: "5.4.237", Glibc: "2.28"}
	cands := []SAVersion{
		mkVer("v0.4.0", "0.4.0", "amd64", "3.10", "stable"),
		mkVer("v0.9.0", "0.9.0", "amd64", "3.10", "stable"),
		mkVer("v0.10.0", "0.10.0", "amd64", "3.10", "stable"),
		mkVer("v0.11.0", "0.11.0", "amd64", "3.10", "disabled"),
	}
	got, reason, ds := selectVersion(cands, env)
	if got == nil || got.Tag != "v0.10.0" {
		t.Fatalf("应选最高 stable（语义版本比较），实际 %+v", got)
	}
	if !strings.Contains(reason, "内核 5.4.237≥3.10") || len(ds) == 0 {
		t.Fatalf("reason 要能自解释：%q", reason)
	}
}

func TestSelectVersionDeprecatedOnlyWarns(t *testing.T) {
	env := TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "2.17"}
	got, _, ds := selectVersion([]SAVersion{mkVer("v0.3.2", "0.3.2", "amd64", "", "deprecated")}, env)
	if got == nil || got.Tag != "v0.3.2" {
		t.Fatalf("无 stable 时可用 deprecated")
	}
	if !hasDiagCode(diagJSON(ds), "version_deprecated_only") {
		t.Errorf("用 deprecated 必须告警：%s", diagJSON(ds))
	}
}

func TestSelectVersionNoCompatibleAggregatesWhy(t *testing.T) {
	env := TargetEnv{Arch: "amd64", Kernel: "3.10.0", Glibc: "2.17"}
	cands := []SAVersion{
		mkVer("v1", "1.0", "amd64", "4.19", "stable"),
		mkVer("v2", "0.9", "amd64", "", "stable", mysqlExp),
	}
	got, _, ds := selectVersion(cands, TargetEnv{Arch: "amd64", Kernel: "3.10.0", Glibc: "2.17"})
	if got != nil {
		t.Fatalf("无兼容候选不得硬选：%+v", got)
	}
	j := diagJSON(ds)
	if !hasDiagCode(j, "version_no_compatible") || !strings.Contains(j, "内核") || !strings.Contains(j, "glibc") {
		t.Fatalf("诊断须聚合每个候选的落选原因：%s", j)
	}
	_ = env
}

func TestParseGlibcVersion(t *testing.T) {
	cases := map[string]string{
		"PY:3.6.8\nGLIBC:2.17\nLDD:ldd (GNU libc) 2.17\n":            "2.17",
		"GLIBC:\nLDD:musl libc (x86_64)\nVersion 1.2.4\n":             "musl",
		"GLIBC:\nLDD:ldd (Ubuntu GLIBC 2.35-0ubuntu3) 2.35\n":         "2.35",
		"PY:3.6.8\n":                                                 "",
		"GLIBC:\nLDD:BusyBox v1.36.1 (2023-01-01) multi-call binary\n": "musl",
	}
	for in, want := range cases {
		if got := parseGlibcVersion(in); got != want {
			t.Errorf("parseGlibcVersion(%q) = %q，期望 %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败** `$GO test . -run 'TestVersion|TestSelectVersion|TestParseGlibc' -v` → 编译失败 `undefined: versionCompatible`.

- [ ] **Step 3: 实现**

先给 `onboard_versions.go` 的 `SAVersion` 增两字段（匹配算法的输入契约，必须在本任务内落地）：

```go
type SAVersion struct {
	Tag      string `yaml:"tag" json:"tag"`
	Version  string `yaml:"version" json:"version"`
	OS       string `yaml:"os" json:"os"`
	Arch     string `yaml:"arch" json:"arch"`
	SHA256   string `yaml:"sha256" json:"sha256"`
	Status   string `yaml:"status" json:"status"`
	Released string `yaml:"released" json:"released"`
	Notes    string `yaml:"notes" json:"notes"`
	// 兼容性判据（自动选版用；空 = 该项无约束）
	MinKernel  string             `yaml:"min_kernel" json:"min_kernel"`
	Components []VersionComponent `yaml:"components" json:"components"`
}
```

再新建 `version_match.go`：

```go
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// TargetEnv 目标机实测环境判据（探路 facts 抽取；缺失留空 = 未核实）
type TargetEnv struct {
	Arch   string `json:"arch"`
	Kernel string `json:"kernel"`
	Glibc  string `json:"glibc"` // "2.17" / "musl" / ""（未采到）
}

// VersionComponent 包内第三方组件及其环境要求（Static=true 表示静态链接，无 glibc 约束）
type VersionComponent struct {
	Name     string `json:"name" yaml:"name"`
	MinGlibc string `json:"min_glibc" yaml:"min_glibc"`
	Static   bool   `json:"static" yaml:"static"`
}

// verAtLeast actual ≥ min（点分数字比较，复用 semverLess）；min 为空视为无约束
func verAtLeast(actual, min string) bool {
	if strings.TrimSpace(min) == "" {
		return true
	}
	if strings.TrimSpace(actual) == "" {
		return false
	}
	return !semverLess(actual, min)
}

// versionCompatible 单候选兼容判定。why 为落选原因；unverified 表示判据缺数据（未核实，不阻断）
func versionCompatible(v SAVersion, env TargetEnv) (ok bool, why string, unverified bool) {
	if normArch(v.Arch) != normArch(env.Arch) {
		return false, fmt.Sprintf("架构不符（候选 %s / 目标 %s）", normArch(v.Arch), normArch(env.Arch)), false
	}
	if v.MinKernel != "" {
		if strings.TrimSpace(env.Kernel) == "" {
			unverified = true
		} else if !verAtLeast(env.Kernel, v.MinKernel) {
			return false, fmt.Sprintf("要求内核 ≥%s（目标 %s）", v.MinKernel, env.Kernel), false
		}
	}
	for _, c := range v.Components {
		if c.Static || strings.TrimSpace(c.MinGlibc) == "" {
			continue
		}
		switch {
		case strings.TrimSpace(env.Glibc) == "":
			unverified = true
		case env.Glibc == "musl":
			return false, fmt.Sprintf("组件 %s 依赖 glibc（目标为 musl）", c.Name), false
		case !verAtLeast(env.Glibc, c.MinGlibc):
			return false, fmt.Sprintf("组件 %s 要求 glibc ≥%s（目标 %s）", c.Name, c.MinGlibc, env.Glibc), false
		}
	}
	return true, "", unverified
}

// selectVersion 按实测环境自动选定版本（纯函数）。
// stable 优先、版本号降序；无 stable 命中时才用 deprecated；全落空返回聚合诊断
func selectVersion(cands []SAVersion, env TargetEnv) (*SAVersion, string, []diag) {
	if len(cands) == 0 {
		return nil, "", []diag{fatalDiag("version_catalog_empty",
			"版本清单为空：没有可参与匹配的版本",
			"先在「版本与兼容性」页登记版本，或补齐 data/versions.yaml 后重建镜像")}
	}
	pickable := []SAVersion{}
	reasons := []string{}
	anyUnverified := false
	considered := 0
	for _, v := range cands {
		if strings.EqualFold(strings.TrimSpace(v.Status), "disabled") {
			continue
		}
		considered++
		ok, why, un := versionCompatible(v, env)
		if un {
			anyUnverified = true
		}
		if ok {
			pickable = append(pickable, v)
			continue
		}
		reasons = append(reasons, v.Tag+"："+why)
	}
	if len(pickable) > 0 {
		sort.SliceStable(pickable, func(i, j int) bool {
			si, sj := pickable[i].Status == "stable", pickable[j].Status == "stable"
			if si != sj {
				return si
			}
			return semverLess(pickable[j].Version, pickable[i].Version)
		})
		chosen := pickable[0]
		reason := fmt.Sprintf("已按实测环境自动选定 %s（arch=%s", chosen.Tag, normArch(env.Arch))
		if chosen.MinKernel != "" && env.Kernel != "" {
			reason += fmt.Sprintf("，内核 %s≥%s", env.Kernel, chosen.MinKernel)
		}
		if env.Glibc != "" {
			reason += "，glibc " + env.Glibc
		}
		reason += "）"
		ds := []diag{infoDiag("version_auto_selected", reason,
			"如需改选其它版本，可在本步用「指定版本（需理由）」覆盖（留痕审计）")}
		if chosen.Status != "stable" {
			ds = append(ds, warnDiag("version_deprecated_only",
				"兼容候选里已无 stable 版本，仅剩 "+chosen.Tag+"（deprecated）可用",
				"尽快登记新版 SAgent 并安排升级"))
		}
		if anyUnverified {
			ds = append(ds, warnDiag("version_env_unverified",
				"部分环境判据未采到（内核/glibc），本次未用该判据核对——不代表已核实兼容",
				"在目标机确认 uname -r 与 getconf GNU_LIBC_VERSION 后重跑探路"))
		}
		return &pickable[0], reason, ds
	}
	msg := fmt.Sprintf("没有与目标机环境兼容的 SAgent 版本（参与匹配 %d 个）", considered)
	if len(reasons) == 0 {
		msg = "版本清单里没有可用条目（全部为 disabled）"
	}
	return nil, "", []diag{fatalDiag("version_no_compatible", msg+"："+strings.Join(reasons, "；"),
		"① 在「版本与兼容性」页为相关版本补齐 min_kernel / 组件 glibc 要求；② 或登记适配该环境的新版本；③ 确认目标机实测值后点「↻ 重试该步」")}
}

var reLddVersion = regexp.MustCompile(`(\d+\.\d+)\s*$`)

// parseGlibcVersion 从探路输出解析目标机 libc：glibc 版本号 / "musl" / ""（判不出）
func parseGlibcVersion(out string) string {
	lines := strings.Split(out, "\n")
	for _, ln := range lines {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "GLIBC:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	for _, ln := range lines {
		v, ok := strings.CutPrefix(strings.TrimSpace(ln), "LDD:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		low := strings.ToLower(v)
		if strings.Contains(low, "musl") || strings.Contains(low, "busybox") {
			return "musl"
		}
		if m := reLddVersion.FindStringSubmatch(v); m != nil {
			return m[1]
		}
	}
	return ""
}

// targetEnvOf 从资源台账（探路回填）构造目标机环境判据
func targetEnvOf(res *storepkg.Resource) TargetEnv {
	env := TargetEnv{Arch: normArch(res.Arch), Kernel: strings.TrimSpace(res.Kernel)}
	var probe struct {
		Glibc string `json:"glibc"`
	}
	if strings.TrimSpace(res.ProbeJSON) != "" {
		_ = json.Unmarshal([]byte(res.ProbeJSON), &probe)
	}
	env.Glibc = strings.TrimSpace(probe.Glibc)
	return env
}
```

（`json` 与 `storepkg` 按需加入 import；`infoDiag/warnDiag/fatalDiag/hasDiagCode/diagJSON` 均为本包既有函数。）

- [ ] **Step 4: 跑测试确认通过** `$GO test . -run 'TestVersion|TestSelectVersion|TestParseGlibc' -v` → 全 PASS。

- [ ] **Step 5: 门禁** `$GO vet ./... && $GO test ./...`

---

## Task 3：SAVersion 增字段 + 探路采集 glibc

**Files:**
- Modify: `l0-console/onboard_versions.go`（SAVersion 结构）
- Modify: `l0-console/onboard_ansible.go`（env_check 命令 + probe_json）
- Test: `l0-console/onboard_test.go`（解析器测试已在 Task 2 覆盖；此处补 probe 回填断言）

- [ ] **Step 1: 写失败测试**（追加到 `onboard_test.go`）

```go
// 探路回填必须带上 glibc：自动选版的兼容判据依赖它（缺了就只能判"未核实"）
func TestProbeBackfillsGlibc(t *testing.T) {
	out := "PY:3.6.8\nEPOCH:1700000000\nCONN:200 TOOL:curl\nGLIBC:2.17\nLDD:ldd (GNU libc) 2.17\n"
	if got := parseGlibcVersion(out); got != "2.17" {
		t.Fatalf("探路输出应解析出 glibc 2.17，实际 %q", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败** `$GO test . -run TestProbeBackfillsGlibc -v` → 若 Task 2 已完成则此测试直接通过（属回归保护，保留即可）。

- [ ] **Step 3: 实现**（`SAVersion` 增字段已在 Task 2 落地，本任务只做采集侧）

`onboard_ansible.go` 的 env_check 命令末尾追加 glibc 探测（`envCmd` 内，`'` 单引号块内不能出现 `'`）：

```go
		`; echo GLIBC:$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{print $2}'); echo LDD:$(ldd --version 2>&1 | head -1)`
```

即 envCmd 的 shells 串在 `echo CONN:...` 之后追加上述片段；随后在探路作业里：

```go
	glibcVer := parseGlibcVersion(re.Output)
	envEvidence["glibc"] = glibcVer
```

并在 `probe` map（回填台账）中增加：

```go
		"glibc": glibcVer,
```

- [ ] **Step 4: 跑测试确认通过** `$GO test . -v -run 'TestProbe|TestParseEnvCheck'` → PASS；随后 `$GO test ./...` 确认既有探路断言不回归。

- [ ] **Step 5: 门禁** `$GO vet ./...`

---

## Task 4：`pick_version` 原子改为自动匹配（含手工 override 通道）

**Files:**
- Modify: `l0-console/onboard_atoms.go`（`atomPickVersion`，562-607 行）
- Modify: `l0-console/onboard_api.go`（步骤 force 支持 `version_tag`；`/api/onboard/flow/version` 保留为兼容通道）
- Test: `l0-console/onboard_test.go`（改写原"停等人工"语义测试 + 新增无匹配用例）

- [ ] **Step 1: 改写失败测试**

把 `onboard_test.go:919-965` 那段（断言 `pick_version` 落 blocked + `waiting_for=human_pick`）改为断言自动选定：

```go
	// 执行器回报探路成功 → 同一次推进里 pick_version 自动定版（不再停等人工）
	...
	if latest["pick_version"] == nil || latest["pick_version"].Status != stOK {
		t.Fatalf("pick_version 应自动选定并落 ok，实际 %+v", latest["pick_version"])
	}
	if tag := pickedVersionTag(db, id); tag == "" {
		t.Fatalf("自动选定必须产出 detail.tag（安装作业据此取版本）")
	}
```

并新增：

```go
// 无兼容候选时必须 fail 并聚合"差哪一项"，绝不硬选最新
func TestPickVersionFailsWhenNoCompatible(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	saved := versionCatalog
	defer func() { versionCatalog = saved }()
	versionCatalog = []SAVersion{{Tag: "v9", Version: "9.0", OS: "linux", Arch: "amd64",
		Status: "stable", MinKernel: "99.0"}}
	id := makeFlow(t, db, "res-noc", []storepkg.FlowStepSnapshot{
		{ID: "pick_version", Atom: "pick_version", Title: "选版本", Scope: "platform"},
	})
	if err := db.UpdateResourceProbe("res-noc", `{}`, "linux", "amd64", "3.10.0"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	advanceFlow(db, nil, id)
	ev, _ := db.LatestEvent(id, "pick_version")
	if ev == nil || ev.Status != stFail {
		t.Fatalf("应判 fail，实际 %+v", ev)
	}
	if !hasDiagCode(ev.Detail, "version_no_compatible") {
		t.Fatalf("必须带"差哪一项"诊断：%s", ev.Detail)
	}
}
```

- [ ] **Step 2: 跑测试确认失败** `$GO test . -run 'TestPickVersion|TestAdvanceFlow' -v` → 期望：原测试红（现在是 blocked）、新测试红（现在是 blocked 不 fail）。

- [ ] **Step 3: 实现**

`onboard_atoms.go` 用下面实现替换 `atomPickVersion`（保留 upgrade 模式的 sha 反查逻辑与"无实测不停等"的前半段，把后半段"停等人工"换成自动匹配）：

```go
// pick_version 按主机探路实测环境自动匹配版本（2026-09-22 起不再停等人工：
// 有兼容候选即选定并落 ok；无候选才 fail；人工可用「指定版本（需理由）」覆盖）
func (r *flowRun) atomPickVersion(catDB *storepkg.DB) (string, string, string) {
	res := r.resource
	if res == nil {
		return stFail, "资源对象缺失，无法定版本", "{}"
	}
	osName, arch := strings.ToLower(res.OS), strings.ToLower(res.Arch)
	verSrc := "resource_ledger"
	if r.flow.Mode == "upgrade" {
		if pre := svcLatestPreflightUpgrade(catDB, r.flow.ID); pre != nil {
			if v := versionBySHA(pre.SHA); v != nil {
				osName, arch = strings.ToLower(v.OS), strings.ToLower(v.Arch)
				verSrc = "target_binary_sha"
			}
		}
	}
	if osName == "" || arch == "" {
		// 不猜版本：探路实测落台账后再推进（人工回报探路结果后重跑本步骤）
		return stBlocked, "等待主机探路实测 OS/架构", detailJSON(map[string]any{
			"waiting_for": "probe",
			"note":        "版本必须基于实测环境匹配，不使用 latest 兜底",
		})
	}
	candidates := compatibleVersions(osName, arch)
	env := targetEnvOf(res)
	env.Arch = normArch(arch)
	chosen, reason, ds := selectVersion(candidates, env)
	if chosen == nil {
		return stFail, "没有与目标机环境兼容的 SAgent 版本", detailJSON(map[string]any{
			"os": osName, "arch": arch, "version_source": verSrc, "env": env,
			"candidates": len(candidates), "diagnosis": diagJSON(ds),
		})
	}
	return stOK, reason, detailJSON(map[string]any{
		"tag": chosen.Tag, "version": chosen.Version, "source": "auto_match",
		"version_source": verSrc, "env": env, "reason": reason,
		"diagnosis": diagJSON(ds),
	})
}
```

`onboard_api.go` 的步骤 force 通道增加"指定版本"（这是设计里的例外通道，不新增端点）：在 `case "force":` 分支内、写事件之前插入：

```go
			// 指定版本例外通道：仅 pick_version 可用，tag 必须在版本清单里（防手滑写错）
			if req.Step == "pick_version" && strings.TrimSpace(req.VersionTag) != "" {
				tag := strings.TrimSpace(req.VersionTag)
				if ver := versionByTag(tag); ver == nil {
					writeJSON(w, map[string]interface{}{"error": "版本清单中无 " + tag})
					return
				}
				_ = appendStepEvent(catDB, req.FlowID, step, stOK,
					"人工指定版本："+tag+"（理由："+req.Reason+"）",
					detailJSON(map[string]any{"tag": tag, "source": "human_force",
						"reason": req.Reason, "operator": cfgAuditOperator}),
					durationSinceLast(catDB, req.FlowID, req.Step, stRunning))
				addAudit("人工指定接入版本", req.Step, "接入中心", tag+"，理由："+req.Reason)
				if err := advanceFlow(catDB, agentStore, req.FlowID); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, map[string]interface{}{"ok": true, "step": req.Step, "action": "force", "tag": tag})
				return
			}
```

并在该 handler 的请求结构体加字段：`VersionTag string \`json:"version_tag"\``。

- [ ] **Step 4: 跑测试确认通过** `$GO test . -v -run 'TestPickVersion|TestAdvanceFlow|TestWaiting'` → PASS；`$GO test ./...` 全绿。

- [ ] **Step 5: 门禁 + 手工验证**：`$GO vet ./...`；启动控制台后对一台资源跑接入流水线，确认 `pick_version` 事件为 ok 且摘要带"已按实测环境自动选定 …"。

---

## Task 5：版本清单改走种子合并（yaml → 目录库 → 内存缓存）

**Files:**
- Modify: `l0-console/onboard_versions.go`（`loadVersionCatalog`）
- Modify: `l0-console/main.go`（调用点传 db）
- Test: `l0-console/onboard_test.go`

- [ ] **Step 1: 写失败测试**

```go
// 版本清单改为"yaml 种子 → 目录库权威"：界面改过的行不被种子覆盖，未登记的行由种子补入
func TestVersionCatalogSeedsIntoCatalog(t *testing.T) {
	db := testOnboardDB(t)
	seed := t.TempDir() + "/versions.yaml"
	body := "versions:\n  - tag: v-seed-1\n    version: \"0.4.0\"\n    os: linux\n    arch: amd64\n    status: stable\n    min_kernel: \"3.10\"\n"
	if err := os.WriteFile(seed, []byte(body), 0o644); err != nil {
		t.Fatalf("写种子失败：%v", err)
	}
	loadVersionCatalog(seed, db)
	if len(versionCatalog) != 1 || versionCatalog[0].Tag != "v-seed-1" || versionCatalog[0].MinKernel != "3.10" {
		t.Fatalf("种子应导入并读回内存缓存，实际 %+v", versionCatalog)
	}
	// 人工改过 → 种子不再覆盖
	if err := db.UpsertVersion(storepkg.VersionRow{Tag: "v-seed-1", Version: "0.4.0", Arch: "amd64",
		OS: "linux", Status: "stable", MinKernel: "4.19"}, "ui"); err != nil {
		t.Fatalf("upsert ui: %v", err)
	}
	loadVersionCatalog(seed, db)
	if versionCatalog[0].MinKernel != "4.19" {
		t.Fatalf("人工改动应保留，实际 %+v", versionCatalog[0])
	}
}
```

- [ ] **Step 2: 跑测试确认失败** → 编译失败（签名不匹配 `loadVersionCatalog(string)`）。

- [ ] **Step 3: 实现**

```go
// loadVersionCatalog 加载版本清单：data/versions.yaml 只作种子，
// 按 tag 合并进目录库（人工改动 source=ui 不被覆盖），再从库读回内存缓存——
// 界面改完矩阵无需重启即生效（写入方刷新缓存），重建镜像也不会吞掉人工维护
func loadVersionCatalog(path string, db *storepkg.DB) {
	imported := 0
	if db != nil {
		if data, err := os.ReadFile(path); err == nil {
			var c struct {
				Versions []SAVersion `yaml:"versions"`
			}
			if err := yaml.Unmarshal(data, &c); err == nil {
				for _, v := range c.Versions {
					cj, _ := json.Marshal(v.Components)
					row := storepkg.VersionRow{
						Tag: v.Tag, Version: v.Version, OS: v.OS, Arch: v.Arch, SHA256: v.SHA256,
						Status: v.Status, Released: v.Released, Notes: v.Notes,
						MinKernel: v.MinKernel, ComponentsJSON: string(cj),
					}
					if err := db.UpsertVersionSeed(row); err != nil {
						println("version seed upsert error:", err.Error())
					} else {
						imported++
					}
				}
			} else {
				println("versions catalog parse error:", err.Error())
			}
		} else {
			println("versions catalog not loaded:", err.Error())
		}
		if rows, err := db.ListVersions(); err == nil {
			versionMu.Lock()
			versionCatalog = rowsToSAVersions(rows)
			versionMu.Unlock()
			println("versions catalog loaded from db:", len(versionCatalog), "entries (seeded", imported, ")")
			return
		}
	}
	// DB 不可用：退回纯文件模式（本地开发/离线兜底）
	if data, err := os.ReadFile(path); err == nil {
		var c struct {
			Versions []SAVersion `yaml:"versions"`
		}
		if err := yaml.Unmarshal(data, &c); err == nil {
			versionMu.Lock()
			versionCatalog = c.Versions
			versionMu.Unlock()
			println("versions catalog loaded (file only):", len(c.Versions))
			return
		}
	}
	println("versions catalog empty: no db and no usable seed file")
}

// rowsToSAVersions 存储行 → 领域对象（components_json 解回结构体）
func rowsToSAVersions(rows []storepkg.VersionRow) []SAVersion {
	out := make([]SAVersion, 0, len(rows))
	for _, r := range rows {
		v := SAVersion{Tag: r.Tag, Version: r.Version, OS: r.OS, Arch: r.Arch, SHA256: r.SHA256,
			Status: r.Status, Released: r.Released, Notes: r.Notes, MinKernel: r.MinKernel}
		if strings.TrimSpace(r.ComponentsJSON) != "" {
			_ = json.Unmarshal([]byte(r.ComponentsJSON), &v.Components)
		}
		out = append(out, v)
	}
	return out
}
```

`main.go` 调用点改为 `loadVersionCatalog("data/versions.yaml", catDB)`（注意：`catDB` 在 `main()` 里 Open 之后才存在，把这次调用移到 `catDB` 就绪之后）。

- [ ] **Step 4: 跑测试确认通过** `$GO test . -run 'TestVersionCatalog|TestCompatibleVersions' -v` → PASS（含既有的 `TestCompatibleVersionsSemverOrder`）。

- [ ] **Step 5: 门禁** `$GO vet ./... && $GO test ./...`

---

## Task 6：API 与「版本与兼容性」界面

**Files:**
- Create: `l0-console/version_api.go`、`l0-console/version_api_test.go`
- Create: `l0-console/static/js/version.js`
- Modify: `l0-console/main.go`（注册路由）、`l0-console/static/index.html`（菜单入口）

- [ ] **Step 1: 写失败测试** `version_api_test.go`

```go
package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// /api/versions 列出矩阵；PUT 编辑落 ui 来源；/api/versions/match 用实测环境预览匹配
func TestVersionAPIListEditMatch(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	if err := db.UpsertVersion(storepkg.VersionRow{Tag: "v-a", Version: "0.4.0", OS: "linux", Arch: "amd64",
		Status: "stable", MinKernel: "3.10"}, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := newVersionMux(t, db)

	// GET
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/versions", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "v-a") {
		t.Fatalf("GET 应列出条目：%d %s", rr.Code, rr.Body.String())
	}

	// PUT：编辑矩阵（min_kernel + 组件要求）
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

	// POST /api/versions/match：给一台实测内核 3.10 的资源 → 必须不兼容
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
}
```

- [ ] **Step 2: 跑测试确认失败** → 编译失败 `undefined: newVersionMux`。

- [ ] **Step 3: 实现** `version_api.go`

```go
package main

import (
	"encoding/json"
	"net/http"
	"strings"

	storepkg "github.com/sagent/l0-console/store"
)

// registerVersionRoutes 版本与兼容矩阵 API（前端零硬编码：字段与状态枚举都由这里下发）
func registerVersionRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	mux.HandleFunc("/api/versions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rows, err := catDB.ListVersions()
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				comps := []VersionComponent{}
				if strings.TrimSpace(row.ComponentsJSON) != "" {
					_ = json.Unmarshal([]byte(row.ComponentsJSON), &comps)
				}
				out = append(out, map[string]any{
					"tag": row.Tag, "version": row.Version, "os": row.OS, "arch": row.Arch,
					"sha256": row.SHA256, "status": row.Status, "released": row.Released,
					"notes": row.Notes, "min_kernel": row.MinKernel, "components": comps,
					"source": row.Source, "updated_at": row.UpdatedAt, "updated_by": row.UpdatedBy,
				})
			}
			writeJSON(w, map[string]interface{}{
				"versions": out,
				"status_options": []string{"stable", "deprecated", "disabled"},
			})
		case http.MethodPut:
			var req struct {
				Tag        string             `json:"tag"`
				MinKernel  string             `json:"min_kernel"`
				Status     string             `json:"status"`
				Notes      string             `json:"notes"`
				Components []VersionComponent `json:"components"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Tag) == "" {
				writeJSON(w, map[string]interface{}{"error": "tag 必填"})
				return
			}
			rows, _ := catDB.ListVersions()
			var cur *storepkg.VersionRow
			for i := range rows {
				if rows[i].Tag == req.Tag {
					cur = &rows[i]
				}
			}
			if cur == nil {
				writeJSON(w, map[string]interface{}{"error": "版本清单中无 " + req.Tag + "（新增版本请先放进 data/binaries 并登记种子）"})
				return
			}
			cur.MinKernel = strings.TrimSpace(req.MinKernel)
			if s := strings.TrimSpace(req.Status); s != "" {
				cur.Status = s
			}
			cur.Notes = req.Notes
			cj, _ := json.Marshal(req.Components)
			cur.ComponentsJSON = string(cj)
			cur.UpdatedBy = cfgAuditOperator
			if err := catDB.UpsertVersion(*cur, "ui"); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			// 刷新内存缓存，编辑立即生效（不必重启）
			if rows2, err := catDB.ListVersions(); err == nil {
				versionMu.Lock()
				versionCatalog = rowsToSAVersions(rows2)
				versionMu.Unlock()
			}
			addAudit("编辑版本兼容矩阵", req.Tag, "版本管理",
				"内核≥"+cur.MinKernel+"，状态="+cur.Status)
			writeJSON(w, map[string]interface{}{"ok": true, "tag": req.Tag})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// 匹配预览：给一台资源（用其实测 facts）跑一次自动选版，回答"会选哪个、其它为什么不选"
	mux.HandleFunc("/api/versions/match", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ResourceID string `json:"resource_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		res, _ := catDB.GetResource(strings.TrimSpace(req.ResourceID))
		if res == nil {
			writeJSON(w, map[string]interface{}{"error": "资源不存在：" + req.ResourceID})
			return
		}
		env := targetEnvOf(res)
		cands := compatibleVersions(strings.ToLower(res.OS), strings.ToLower(res.Arch))
		chosen, reason, ds := selectVersion(cands, env)
		considered := make([]map[string]any, 0, len(cands))
		for _, v := range cands {
			ok, why, un := versionCompatible(v, env)
			considered = append(considered, map[string]any{
				"tag": v.Tag, "version": v.Version, "status": v.Status,
				"compatible": ok, "why": why, "unverified": un,
			})
		}
		out := map[string]any{
			"resource_id": res.ID, "env": env, "candidates": considered,
			"compatible": chosen != nil, "diagnosis": diagJSON(ds),
		}
		if chosen != nil {
			out["chosen"] = map[string]any{"tag": chosen.Tag, "version": chosen.Version, "status": chosen.Status}
			out["reason"] = reason
		}
		writeJSON(w, out)
	})
}

// newVersionMux 测试用：只挂版本路由的最小 mux
func newVersionMux(t *testing.T, db *storepkg.DB) *http.ServeMux {
	t.Helper()
	m := http.NewServeMux()
	registerVersionRoutes(m, db)
	return m
}
```

`main.go` 在其它 `register*Routes` 旁加：`registerVersionRoutes(mux, catDB)`。

- [ ] **Step 4: 跑测试确认通过** `$GO test . -run TestVersionAPI -v` → PASS。

- [ ] **Step 5: 前端** `static/js/version.js`（结构；字段全部取自 `/api/versions` 下发）

```js
// 「版本与兼容性」页：列表 + 编辑矩阵 + 匹配预览
// 约定：所有字段来自后端（/api/versions、/api/versions/match），前端不写死枚举
(function () {
  var _versions = [];

  function renderVersions() {
    fetch(API + '/versions').then(function (r) { return r.json(); }).then(function (d) {
      _versions = d.versions || [];
      var html = '<div class="card"><div class="card-hd">🧬 版本与兼容性</div><div class="card-bd">';
      html += '<div style="margin-bottom:8px;color:var(--muted);font-size:12px">'
        + '兼容判据：架构 + 最低内核 + 包内组件要求；装机/升级时平台按目标机实测环境自动选定版本</div>';
      html += '<table style="font-size:12px"><thead><tr><th>tag</th><th>版本</th><th>架构</th><th>状态</th>'
        + '<th>内核下限</th><th>组件要求</th><th>来源</th><th>更新时间</th><th></th></tr></thead><tbody>';
      (_versions || []).forEach(function (v) {
        var comps = (v.components || []).map(function (c) {
          return c.static ? (c.name + '（静态）') : (c.name + ' ≥ glibc ' + c.min_glibc);
        }).join('；') || '—';
        html += '<tr><td>' + escHtml(v.tag) + '</td><td>' + escHtml(v.version) + '</td><td>' + escHtml(v.arch) + '</td>'
          + '<td>' + escHtml(v.status) + '</td><td>' + escHtml(v.min_kernel || '—') + '</td>'
          + '<td>' + escHtml(comps) + '</td><td>' + escHtml(v.source) + '</td><td>' + escHtml(v.updated_at) + '</td>'
          + '<td><button class="btn btn-o btn-sm" onclick="editVersion(\'' + obEscape(v.tag) + '\')">编辑矩阵</button></td></tr>';
      });
      html += '</tbody></table></div></div>';
      html += matchPreviewCard();
      document.getElementById('main-content').innerHTML = html;
    });
  }

  function matchPreviewCard() {
    return '<div class="card" style="margin-top:12px"><div class="card-hd">🎯 匹配预览</div><div class="card-bd">'
      + '<div style="display:flex;gap:8px;align-items:center">'
      + '<input id="vm-res" placeholder="资源 ID（如 ip-10-1-207-156）" style="padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
      + '<button class="btn btn-p btn-sm" onclick="previewVersionMatch()">预览</button></div>'
      + '<div id="vm-out" style="margin-top:10px;font-size:12px"></div></div></div>';
  }

  function previewVersionMatch() {
    var id = document.getElementById('vm-res').value.trim();
    if (!id) { toast('请输入资源 ID', 'err'); return; }
    fetch(API + '/versions/match', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ resource_id: id }) }).then(function (r) { return r.json(); }).then(function (d) {
      var host = document.getElementById('vm-out');
      if (!host) return;
      var html = '<div>实测环境：arch=' + escHtml((d.env || {}).arch || '-')
        + '，内核=' + escHtml((d.env || {}).kernel || '未采到')
        + '，glibc=' + escHtml((d.env || {}).glibc || '未采到') + '</div>';
      html += d.compatible
        ? '<div style="color:var(--ok)">✅ 会选：' + escHtml((d.chosen || {}).tag) + ' —— ' + escHtml(d.reason || '') + '</div>'
        : '<div style="color:var(--err)">⛔ 无兼容版本</div>';
      html += '<table style="margin-top:6px"><thead><tr><th>候选</th><th>结论</th><th>原因</th></tr></thead><tbody>';
      (d.candidates || []).forEach(function (c) {
        html += '<tr><td>' + escHtml(c.tag) + '</td><td>' + (c.compatible ? '兼容' : '不兼容')
          + (c.unverified ? '（未核实）' : '') + '</td><td>' + escHtml(c.why || '—') + '</td></tr>';
      });
      html += '</tbody></table>';
      host.innerHTML = html;
    });
  }

  function editVersion(tag) {
    var v = (_versions || []).filter(function (x) { return x.tag === tag; })[0];
    if (!v) return;
    renderOverlay('🧬 编辑兼容矩阵 · ' + tag, function () {
      var h = '<div style="font-size:13px;display:grid;gap:10px">';
      h += '<div><b>最低内核</b>（留空 = 无约束）<br><input id="ve-kernel" value="' + obEscape(v.min_kernel || '')
        + '" placeholder="例: 3.10" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div>';
      h += '<div><b>状态</b><br><select id="ve-status" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
        + ['stable', 'deprecated', 'disabled'].map(function (s) {
          return '<option value="' + s + '"' + (v.status === s ? ' selected' : '') + '>' + s + '</option>';
        }).join('') + '</select></div>';
      h += '<div><b>包内组件要求</b>（每行一条：组件名,最低glibc；静态组件填 static）<br>'
        + '<textarea id="ve-comps" rows="3" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
        + obEscape((v.components || []).map(function (c) {
          return c.static ? (c.name + ',static') : (c.name + ',' + c.min_glibc);
        }).join('\n')) + '</textarea></div>';
      h += '<div><b>说明</b><br><input id="ve-notes" value="' + obEscape(v.notes || '')
        + '" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div>';
      h += '</div><div style="margin-top:14px;display:flex;gap:8px">'
        + '<button class="btn btn-p" onclick="saveVersion(\'' + obEscape(tag) + '\')">💾 保存</button>'
        + '<button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
      return h;
    });
  }

  function saveVersion(tag) {
    var comps = (document.getElementById('ve-comps').value || '').split('\n').map(function (line) {
      var p = line.split(',').map(function (s) { return s.trim(); });
      if (!p[0]) return null;
      return p[1] === 'static' ? { name: p[0], static: true } : { name: p[0], min_glibc: p[1] || '' };
    }).filter(Boolean);
    fetch(API + '/versions', { method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tag: tag, min_kernel: document.getElementById('ve-kernel').value.trim(),
        status: document.getElementById('ve-status').value, notes: document.getElementById('ve-notes').value,
        components: comps }) }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { toast(d.error, 'err'); return; }
      closeOverlay(); toast('兼容矩阵已保存', 'ok'); renderVersions();
    });
  }

  window.renderVersions = renderVersions;
  window.editVersion = editVersion;
  window.saveVersion = saveVersion;
  window.previewVersionMatch = previewVersionMatch;
})();
```

`static/index.html`：在 `panel.js` 之后加 `<script src="/js/version.js"></script>`，并在侧边菜单加一项：

```html
<a class="nav-item" onclick="goPage('versions');renderVersions()">🧬 版本与兼容性</a>
```

（菜单项 class 名与相邻项保持一致；`goPage('versions')` 的兜底分支若未定义，`renderVersions()` 会直接渲染到 `#main-content`，与现有 `renderTenants` 的做法一致。）

- [ ] **Step 6: 门禁 + 浏览器验证**：`$GO vet ./... && $GO test ./...`；启动界面后用 chrome-devtools MCP 打开控制台 → 版本与兼容性页 → 断言：列表出现、编辑矩阵保存后刷新可见、匹配预览对 `ip-10-1-207-156` 给出结论与原因（该资源 facts 已实测，内核与 arch 均有值）。

---

## 交付前的整体验收

- [ ] `$GO vet ./...` 两模块 exit 0
- [ ] `$GO test ./...` 两模块全绿（含本次新增：store 3 个、匹配算法 5 个、API 1 个、原子语义 2 个、种子合并 1 个）
- [ ] `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64` 交叉编译通过
- [ ] 起一条 edge 流水线跑真机（vm-host-1）：`pick_version` 事件为 ok、"已按实测环境自动选定"、安装成功、SD 登记
- [ ] 人为把某版本 `min_kernel` 抬到 `99` → 观察 fail 与"差哪一项"诊断；再用「指定版本」通道覆盖成功
- [ ] 同步修订设计文档 `PLAN-SAgent版本兼容矩阵与自动选版.md` §7：模板无需改动（停等由原子内部产生，步骤已是 `scope: platform`）

## 已识别的风险与规避

- **`compatibleVersions` 仍按 os/arch 预过滤**：arm64 目标机上的 amd64 版本不会进候选（对）；但 `os` 为空的资源（探路未跑）会走 `stBlocked` 分支，不会误选。
- **`targetEnvOf` 依赖 `probe_json`**：老资源行没有 glibc → 判据未核实并告警（设计如此），不阻断。
- **`onboard_test.go` 旧语义测试**：Task 4 必须同步改写，否则它会把"自动选版"判成回归。
- **前端 `obEscape` 只为属性转义**：本页所有动态值均经 `escHtml/obEscape`，不得再拼裸字符串（沿用 `panel.js:601` 的既有约定）。
