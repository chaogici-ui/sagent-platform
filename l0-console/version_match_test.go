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

// detailOf 把诊断切片包成事件 detail JSON（与生产同一形状，供 hasDiagCode 断言）
func detailOf(ds []diag) string {
	return detailJSON(map[string]any{"diagnosis": diagJSON(ds)})
}

// 兼容判据逐项：架构 / 内核 / glibc / musl / 静态组件不设限 / 缺数据不阻断但标记未核实
func TestVersionCompatibleReasons(t *testing.T) {
	env := TargetEnv{Arch: "amd64", Kernel: "3.10.0", Glibc: "2.17"}
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "arm64", "", "stable"), env); ok || !strings.Contains(why, "架构") {
		t.Errorf("架构不符应落选并说明：ok=%v why=%q", ok, why)
	}
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "4.19", "stable"), env); ok || !strings.Contains(why, "内核") {
		t.Errorf("内核不足应落选并说明：ok=%v why=%q", ok, why)
	}
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", mysqlExp),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "2.12"}); ok || !strings.Contains(why, "glibc") {
		t.Errorf("glibc 不足应落选并说明：ok=%v why=%q", ok, why)
	}
	if ok, why, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", mysqlExp),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "musl"}); ok || !strings.Contains(why, "musl") {
		t.Errorf("musl 目标应落选：ok=%v why=%q", ok, why)
	}
	if ok, _, _ := versionCompatible(mkVer("a", "1.0", "amd64", "", "stable", vectorStatic),
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "musl"}); !ok {
		t.Errorf("静态组件不应产生约束")
	}
	ok, _, un := versionCompatible(mkVer("a", "1.0", "amd64", "3.10", "stable", mysqlExp),
		TargetEnv{Arch: "amd64"})
	if !ok || !un {
		t.Errorf("缺内核/glibc 时应判兼容但标记未核实：ok=%v unverified=%v", ok, un)
	}
}

// 多候选取最高 stable，且 reason 自解释（能直接给运维看）
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

// 无 stable 命中时可用 deprecated，但必须告警
func TestSelectVersionDeprecatedOnlyWarns(t *testing.T) {
	got, _, ds := selectVersion([]SAVersion{mkVer("v0.3.2", "0.3.2", "amd64", "", "deprecated")},
		TargetEnv{Arch: "amd64", Kernel: "5.4", Glibc: "2.17"})
	if got == nil || got.Tag != "v0.3.2" {
		t.Fatalf("无 stable 时可用 deprecated")
	}
	if !hasDiagCode(detailOf(ds), "version_deprecated_only") {
		t.Errorf("用 deprecated 必须告警：%s", detailOf(ds))
	}
}

// 全落空：不得硬选，诊断必须聚合"每个候选差在哪"
func TestSelectVersionNoCompatibleAggregatesWhy(t *testing.T) {
	cands := []SAVersion{
		mkVer("v1", "1.0", "amd64", "4.19", "stable"),
		mkVer("v2", "0.9", "amd64", "", "stable", mysqlExp),
	}
	got, _, ds := selectVersion(cands, TargetEnv{Arch: "amd64", Kernel: "3.10.0", Glibc: "2.12"})
	if got != nil {
		t.Fatalf("无兼容候选不得硬选：%+v", got)
	}
	j := detailOf(ds)
	if !hasDiagCode(j, "version_no_compatible") || !strings.Contains(j, "内核") || !strings.Contains(j, "glibc") {
		t.Fatalf("诊断须聚合每个候选的落选原因：%s", j)
	}
}

// 空的候选清单（清单为空 / 全 disabled）也要有可执行建议
func TestSelectVersionEmptyCatalog(t *testing.T) {
	if got, _, ds := selectVersion(nil, TargetEnv{Arch: "amd64"}); got != nil || !hasDiagCode(detailOf(ds), "version_catalog_empty") {
		t.Fatalf("空清单应落 version_catalog_empty：%s", detailOf(ds))
	}
	got, _, _ := selectVersion([]SAVersion{mkVer("v1", "1.0", "amd64", "", "disabled")}, TargetEnv{Arch: "amd64"})
	if got != nil {
		t.Fatalf("全 disabled 不得选出任何版本")
	}
}

// glibc 探测片段要能安全嵌进 sh -c '…' 单引号块（片段内不得出现单引号，否则截断外层），
// 且两路探测（getconf / ldd）都要在——缺一路会把"能判出来的环境"误判成未核实
func TestGlibcProbeShellQuotingAndCoverage(t *testing.T) {
	sh := glibcProbeShell()
	if strings.Contains(sh, "'") {
		t.Fatalf("片段含单引号，会截断外层 sh -c '…' 块：%q", sh)
	}
	for _, want := range []string{"GLIBC:", "getconf GNU_LIBC_VERSION", "LDD:", "ldd --version"} {
		if !strings.Contains(sh, want) {
			t.Errorf("缺少 %s：%q", want, sh)
		}
	}
	if !strings.HasPrefix(sh, ";") {
		t.Errorf("片段要能以分号接在既有探测链之后：%q", sh)
	}
}

// glibc 解析：getconf 优先、ldd 兜底、musl/busybox 识别、取不到给空串
func TestParseGlibcVersion(t *testing.T) {
	cases := map[string]string{
		"PY:3.6.8\nGLIBC:2.17\nLDD:ldd (GNU libc) 2.17\n":             "2.17",
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
