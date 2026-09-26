package main

// package_cache_test.go —— L1 Package Cache（架构 D5）红线行为单测。
// 覆盖 PLAN 风险 A 强约束：幂等同步、就近命中、缓存篡改自愈回源、L0 内部不一致拒绝（不降级）。

import (
	"os"
	"path/filepath"
	"testing"
)

// mkFakePackage 建一个假 L0 版本源：临时二进制 + 登记进全局版本目录/清单，测试结束恢复。
// 返回 (l0BinPath, tag, restoredFunc)。
func mkFakePackage(t *testing.T, tag, content string) string {
	t.Helper()
	// 临时 L0 源目录，优先于真实 data/binaries
	l0src := t.TempDir()
	tagDir := filepath.Join(l0src, tag)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tagDir, "SAgent")
	if err := os.WriteFile(bin, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	origDirs := append([]string{}, versionBinDirs...)
	// 只保留本次假包作 L0 源：隔离测试之间的全局 versionBinDirs/versionCatalog 共享，
	// 否则幂等断言的 synced 绝对计数会被其它测试/真实版本库污染
	versionBinDirs = []string{l0src}

	// 登记一条清单：sha256 以实际文件计算为准（权威=清单值）
	versionMu.Lock()
	fake := SAVersion{Tag: tag, Version: "9.9.9", OS: "linux", Arch: "test"}
	origCatalog := append([]SAVersion{}, versionCatalog...)
	versionCatalog = []SAVersion{fake}
	versionMu.Unlock()

	t.Cleanup(func() {
		versionBinDirs = origDirs
		versionMu.Lock()
		versionCatalog = origCatalog
		versionMu.Unlock()
	})
	return filepath.Join(l0src, tag, "SAgent")
}

func setCacheRoot(t *testing.T, root string) {
	t.Helper()
	t.Setenv("L1_PACKAGE_CACHE", root)
}

// TestPackageCacheSyncIdempotent 幂等：同一 L0 源重复同步不产生重复任务。
func TestPackageCacheSyncIdempotent(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "packages")
	setCacheRoot(t, cache)
	mkFakePackage(t, "linux-test-9.9.9", "abc-pkg-bytes")

	synced1, rejected1, _, err := syncPackageCacheFromL0()
	if err != nil {
		t.Fatalf("sync #1: %v", err)
	}
	if rejected1 != 0 || synced1 != 1 {
		t.Fatalf("sync #1 want synced=1 rejected=0, got synced=%d rejected=%d", synced1, rejected1)
	}
	// 缓存文件 + 签名文件 + 清单均已落
	if fi, err := os.Stat(pkgCacheBin("linux-test-9.9.9")); err != nil || fi.Size() == 0 {
		t.Fatalf("cache bin missing: %v", err)
	}
	if fi, err := os.Stat(pkgCacheSig("linux-test-9.9.9")); err != nil || fi.Size() == 0 {
		t.Fatalf("cache sig file missing: %v", err)
	}

	// 再跑一次必须零新增
	synced2, _, _, err := syncPackageCacheFromL0()
	if err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	if synced2 != 0 {
		t.Fatalf("idempotence broken: sync #2 synced=%d, want 0", synced2)
	}
}

// TestResolveInstallBinaryNearHit 就近命中：缓存完整且校验通过时直接返回缓存路径，不做多余同步。
func TestResolveInstallBinaryNearHit(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "packages")
	setCacheRoot(t, cache)
	mkFakePackage(t, "linux-test-9.9.9", "near-hit-bytes")

	if _, _, _, err := syncPackageCacheFromL0(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	got, err := resolveInstallBinary("linux-test-9.9.9")
	if err != nil {
		t.Fatalf("resolveInstallBinary near-hit: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(pkgCacheBin("linux-test-9.9.9")) {
		t.Fatalf("want cache path %s, got %s", pkgCacheBin("linux-test-9.9.9"), got)
	}
}

// TestResolveInstallBinarySelfHeal 缓存被篡改：权威 L0 仍好 → 回源自愈，不放行坏包。
func TestResolveInstallBinarySelfHeal(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "packages")
	setCacheRoot(t, cache)
	mkFakePackage(t, "linux-test-9.9.9", "heal-bytes")

	if _, _, _, err := syncPackageCacheFromL0(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	// 篡改缓存二进制（模拟缓存被污染成旧包/坏包）
	if err := os.WriteFile(pkgCacheBin("linux-test-9.9.9"), []byte("tampered-old-pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := describePackageCache()
	found := false
	for i := range e {
		if e[i].Tag == "linux-test-9.9.9" {
			found = true
			if e[i].Verified {
				t.Fatalf("tampered cache should report verified=false")
			}
		}
	}
	if !found {
		t.Fatalf("entry not found in status")
	}
	// 就近取包应触发回源重建并放行正确的 L0 镜像
	got, err := resolveInstallBinary("linux-test-9.9.9")
	if err != nil {
		t.Fatalf("self-heal should succeed via 回源, got: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(pkgCacheBin("linux-test-9.9.9")) {
		t.Fatalf("want cache path after heal")
	}
	b, _ := os.ReadFile(got)
	if string(b) != "heal-bytes" {
		t.Fatalf("cache not restored to L0 权威 payload: %q", string(b))
	}
}

// TestResolveInstallBinaryRefuseOnL0Inconsistency L0 权威源内部不一致（清单 sha≠实际文件）：
// 同步拒绝缓存该镜像，就近取包返回错误拒绝安装（不降级用旧包）。
func TestResolveInstallBinaryRefuseOnL0Inconsistency(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "packages")
	setCacheRoot(t, cache)
	l0bin := mkFakePackage(t, "linux-test-9.9.9", "refuse-bytes")

	// 篡改 L0 清单：把权威 sha 改成与文件一致之外的值，模拟「登记版本被篡改/未同步」
	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	versionMu.Lock()
	for i := range versionCatalog {
		if versionCatalog[i].Tag == "linux-test-9.9.9" {
			versionCatalog[i].SHA256 = wrong
		}
	}
	versionMu.Unlock()
	_ = l0bin

	synced, rejected, _, err := syncPackageCacheFromL0()
	if err != nil {
		t.Fatalf("sync with bad catalog: %v", err)
	}
	if rejected < 1 {
		t.Fatalf("want rejected>=1 (L0 内部不一致镜像禁止入缓存), got rejected=%d", rejected)
	}
	if _, err := resolveInstallBinary("linux-test-9.9.9"); err == nil {
		t.Fatalf("L0 清单不一致时应拒绝安装（不降级），但 resolveInstallBinary 未报错")
	}
	_ = synced
}