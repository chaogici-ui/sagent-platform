package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	storepkg "github.com/sagent/l0-console/store"
)

// fakeELF 构造最小 ELF 头（仅 magic/class/machine 三处被解析）
func fakeELF(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	b[18] = byte(machine)
	b[19] = byte(machine >> 8)
	return b
}

// 版本标签即落盘目录名：必须白名单校验（防路径穿越），且必须含可核对的架构段
func TestVersionTagValidation(t *testing.T) {
	ok := []string{"linux-amd64-0.4.1", "linux-arm64-0.4.1-dev", "linux-amd64-0.3.2"}
	for _, tag := range ok {
		if err := validateVersionTag(tag); err != nil {
			t.Errorf("%q 应通过校验：%v", tag, err)
		}
	}
	bad := map[string]string{
		"":                       "空",
		"../evil":                "路径穿越",
		"linux/amd64/1.0":        "含斜杠",
		"linux-amd64-0.4.1/../":  "含斜杠",
		"LINUX-AMD64-1.0":        "大写",
		"0.4.1":                  "无架构段",
		"linux-amd64":            "无版本段",
		"linux-amd64-":           "版本段为空",
		"linux-amd64-0.4.1;rm":   "含特殊字符",
		"linux-amd64-0.4.1\ncat": "含换行",
	}
	for tag, why := range bad {
		if err := validateVersionTag(tag); err == nil {
			t.Errorf("%q（%s）应被拒绝", tag, why)
		}
	}
}

// ELF 架构识别：amd64=0x3E / arm64=0xB7；非 ELF 或 32 位一律拒
func TestELFArchDetection(t *testing.T) {
	if a, err := elfArchOf(fakeELF(0x3E)); err != nil || a != "amd64" {
		t.Errorf("x86-64 ELF 应识别为 amd64：%q %v", a, err)
	}
	if a, err := elfArchOf(fakeELF(0xB7)); err != nil || a != "arm64" {
		t.Errorf("AArch64 ELF 应识别为 arm64：%q %v", a, err)
	}
	if _, err := elfArchOf([]byte("not an elf at all, just text")); err == nil {
		t.Errorf("非 ELF 应被拒")
	}
	bin32 := fakeELF(0x3E)
	bin32[4] = 1 // EI_CLASS=1 → 32 位
	if _, err := elfArchOf(bin32); err == nil {
		t.Errorf("32 位 ELF 应被拒（本平台只分发 64 位）")
	}
}

// 包查找：UI 上传目录优先，回落镜像种子目录；两处都没有则报错
func TestResolveVersionBinaryPrefersUIBinaries(t *testing.T) {
	saved := versionBinDirs
	defer func() { versionBinDirs = saved }()
	uiDir, seedDir := t.TempDir(), t.TempDir()
	versionBinDirs = []string{uiDir, seedDir}

	if _, err := resolveVersionBinary("linux-amd64-9.9.9"); err == nil {
		t.Fatalf("两处都没有应报错")
	}
	mk := func(dir, tag, body string) {
		if err := os.MkdirAll(filepath.Join(dir, tag), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, tag, "SAgent"), []byte(body), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk(seedDir, "linux-amd64-1.0.0", "seed-binary")
	if got, err := resolveVersionBinary("linux-amd64-1.0.0"); err != nil || got == "" {
		t.Fatalf("应回落到种子目录：%q %v", got, err)
	}
	mk(uiDir, "linux-amd64-1.0.0", "ui-binary")
	got, err := resolveVersionBinary("linux-amd64-1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b, _ := os.ReadFile(got); string(b) != "ui-binary" {
		t.Fatalf("UI 上传目录应优先于镜像种子：解析到 %s", got)
	}
}

// 上传→落 draft→下载→发布：完整回路（含 ELF 架构与 tag 不一致的拒绝）
func TestVersionUploadPublishDownload(t *testing.T) {
	testOnboardCfg(t)
	db := testOnboardDB(t)
	saved := versionBinDirs
	defer func() { versionBinDirs = saved }()
	versionBinDirs = []string{t.TempDir(), t.TempDir()}
	mux := newVersionMux(t, db)

	upload := func(tag string, body []byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("tag", tag)
		fw, _ := w.CreateFormFile("file", "SAgent")
		_, _ = fw.Write(body)
		_ = w.Close()
		req := httptest.NewRequest("POST", "/api/versions/upload", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	// 架构与 tag 不一致 → 拒（本仓约定：参数类错误走 200 + {"error":…}）
	if rr := upload("linux-arm64-0.5.0", fakeELF(0x3E)); !strings.Contains(rr.Body.String(), "不一致") {
		t.Fatalf("amd64 二进制配 arm64 tag 应被拒：%s", rr.Body.String())
	}
	// 正常上传 → 落 draft
	rr := upload("linux-arm64-0.5.0", fakeELF(0xB7))
	if rr.Code != 200 {
		t.Fatalf("上传应成功：%d %s", rr.Code, rr.Body.String())
	}
	rows, _ := db.ListVersions()
	var row *storepkg.VersionRow
	for i := range rows {
		if rows[i].Tag == "linux-arm64-0.5.0" {
			row = &rows[i]
		}
	}
	if row == nil || row.Status != "draft" || row.Source != "ui" || row.SHA256 == "" {
		t.Fatalf("上传后应落 draft + ui + sha256：%+v", row)
	}
	// draft 不参与自动选版
	if _, _, ds := selectVersion([]SAVersion{{Tag: row.Tag, Version: row.Version, Arch: "arm64", Status: "draft"}},
		TargetEnv{Arch: "arm64", Kernel: "5.4", Glibc: "2.17"}); !hasDiagCode(detailOf(ds), "version_no_compatible") {
		t.Fatalf("draft 版本不得被自动选中：%s", detailOf(ds))
	}
	// 匹配预览也要如实标注 draft 不可选（否则出现"兼容=True 却没被选中"的自我矛盾）
	if err := db.UpsertResource(&storepkg.Resource{ID: "r-draft", Name: "r-draft", ResourceType: "host",
		Role: "business", OS: "linux", Arch: "arm64", Kernel: "5.4.0"}); err != nil {
		t.Fatalf("res: %v", err)
	}
	mrr := httptest.NewRecorder()
	mux.ServeHTTP(mrr, httptest.NewRequest("POST", "/api/versions/match", strings.NewReader(`{"resource_id":"r-draft"}`)))
	if !strings.Contains(mrr.Body.String(), "未发布（draft）") {
		t.Fatalf("预览须把 draft 标为不可选并说明：%s", mrr.Body.String())
	}
	// 下载：拿到刚上传的字节
	dreq := httptest.NewRequest("GET", "/api/versions/download?tag=linux-arm64-0.5.0", nil)
	drr := httptest.NewRecorder()
	mux.ServeHTTP(drr, dreq)
	if drr.Code != 200 || !bytes.Equal(drr.Body.Bytes(), fakeELF(0xB7)) {
		t.Fatalf("下载应回原字节：%d %d bytes，body=%s", drr.Code, drr.Body.Len(), drr.Body.String())
	}
	if cd := drr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("应带 attachment 头：%q", cd)
	}
	// 发布：draft → stable，之后参与选版
	if rr := putVersionStatus(t, mux, "linux-arm64-0.5.0", "stable"); rr.Code != 200 {
		t.Fatalf("发布应成功：%s", rr.Body.String())
	}
	if _, reason, _ := selectVersion([]SAVersion{{Tag: "linux-arm64-0.5.0", Version: "0.5.0", Arch: "arm64", Status: "stable"}},
		TargetEnv{Arch: "arm64", Kernel: "5.4", Glibc: "2.17"}); !strings.Contains(reason, "linux-arm64-0.5.0") {
		t.Fatalf("发布后应可被自动选中：%q", reason)
	}
	// 未知/非法 tag 下载 → 拒绝且不得吐出任何文件内容（防任意路径读取）
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest("GET", "/api/versions/download?tag=../../etc/passwd", nil))
	if bytes.Contains(rr2.Body.Bytes(), []byte{0x7f, 'E', 'L', 'F'}) || !strings.Contains(rr2.Body.String(), "只允许小写字母") {
		t.Fatalf("非法 tag 下载必须被拒并给出可读原因：%s", rr2.Body.String())
	}
}

// putVersionStatus 走既有 PUT /api/versions 改状态（发布/弃用/停用共用同一端点）
func putVersionStatus(t *testing.T, mux *http.ServeMux, tag, status string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"tag": tag, "status": status})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("PUT", "/api/versions", bytes.NewReader(body)))
	return rr
}
