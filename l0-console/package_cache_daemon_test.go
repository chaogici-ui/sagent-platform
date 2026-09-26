package main

// package_cache_daemon_test.go —— D5 IN1 独立 Package Cache 进程的 resolve 接口单测。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// primeCacheForDaemon 起一个带假包的缓存（复刻 mkFakePackage 的隔离语义）。
func primeCacheForDaemon(t *testing.T) {
	t.Helper()
	setCacheRoot(t, t.TempDir())
	mkFakePackage(t, "linux-test-daemon-1.0.0", "daemon-pkg-bytes")
	if _, _, _, err := syncPackageCacheFromL0(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
}

// TestHandlePackageCacheResolveOK 独立进程 resolve 接口：缓存命中返回缓存路径。
func TestHandlePackageCacheResolveOK(t *testing.T) {
	primeCacheForDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/package-cache/resolve?tag=linux-test-daemon-1.0.0", nil)
	rec := httptest.NewRecorder()
	handlePackageCacheResolve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var out struct {
		OK  bool   `json:"ok"`
		Bin string `json:"bin"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.Bin == "" {
		t.Fatalf("resolve should hit cache and return bin, got %s", rec.Body.String())
	}
	if want := pkgCacheBin("linux-test-daemon-1.0.0"); filepath.Clean(out.Bin) != filepath.Clean(want) {
		t.Fatalf("want %s, got %s", want, out.Bin)
	}
}

// TestHandlePackageCacheResolveMissingTag resolve 缺 tag → 报错。
func TestHandlePackageCacheResolveMissingTag(t *testing.T) {
	primeCacheForDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/package-cache/resolve", nil)
	rec := httptest.NewRecorder()
	handlePackageCacheResolve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler always 200 with JSON ok flag, got %d", rec.Code)
	}
	var out struct{ OK bool `json:"ok"` }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK {
		t.Fatalf("missing tag must not resolve ok")
	}
}

// TestRemoteResolveInstallBinaryWire 安装侧走远程 resolve 的语义：命中返回、缺 L0 源拒错。
func TestRemoteResolveInstallBinaryWire(t *testing.T) {
	primeCacheForDaemon(t)
	srv := httptest.NewServer(http.HandlerFunc(handlePackageCacheResolve))
	defer srv.Close()

	// 命中：已入缓存的 tag 能解析出缓存路径
	bin, err := remoteResolveInstallBinary(srv.URL, "linux-test-daemon-1.0.0")
	if err != nil {
		t.Fatalf("remote resolve hit should succeed: %v", err)
	}
	if want := pkgCacheBin("linux-test-daemon-1.0.0"); filepath.Clean(bin) != filepath.Clean(want) {
		t.Fatalf("remote resolve want %s, got %s", want, bin)
	}

	// 拒绝：L0 无源（未登记的 tag）→ resolve 返回 ok=false，客户端上抛错误
	if _, err := remoteResolveInstallBinary(srv.URL, "no-such-tag-xx"); err == nil {
		t.Fatalf("remote resolve for absent tag should error")
	}
}

// TestResolveInstallBinaryAnyToggle L1_PACKAGE_CACHE_URL 打开时走远程；关闭走进程内。
func TestResolveInstallBinaryAnyToggle(t *testing.T) {
	primeCacheForDaemon(t)
	srv := httptest.NewServer(http.HandlerFunc(handlePackageCacheResolve))
	defer srv.Close()

	// 未配置 → 进程内（命中缓存）
	os.Unsetenv("L1_PACKAGE_CACHE_URL")
	bin, err := resolveInstallBinaryAny("linux-test-daemon-1.0.0")
	if err != nil {
		t.Fatalf("in-process resolve: %v", err)
	}
	if want := pkgCacheBin("linux-test-daemon-1.0.0"); filepath.Clean(bin) != filepath.Clean(want) {
		t.Fatalf("in-process want %s got %s", want, bin)
	}

	// 配置 → 远程 resolve（同一命中结果，路径一致）
	t.Setenv("L1_PACKAGE_CACHE_URL", srv.URL)
	bin, err = resolveInstallBinaryAny("linux-test-daemon-1.0.0")
	if err != nil {
		t.Fatalf("remote resolve via Any: %v", err)
	}
	if want := pkgCacheBin("linux-test-daemon-1.0.0"); filepath.Clean(bin) != filepath.Clean(want) {
		t.Fatalf("remote want %s got %s", want, bin)
	}
}