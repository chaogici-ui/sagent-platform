package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// versionBinDirs 版本包目录：UI 上传目录优先，回落镜像种子目录。
// 两者物理隔离——镜像种子每次启动被 entrypoint 覆盖（"镜像赢"），
// UI 上传的包落在独立目录才不会被抹掉（包级变量便于测试替换）
var versionBinDirs = []string{"data/binaries-ui", "data/binaries"}

// reVersionTag 标签即落盘目录名，白名单字符（防路径穿越）
var reVersionTag = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// validateVersionTag 校验版本标签：白名单字符 + 三段式 <os>-<arch>-<version>
func validateVersionTag(tag string) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("tag 不能为空")
	}
	if !reVersionTag.MatchString(tag) {
		return fmt.Errorf("tag 只允许小写字母/数字/._-（且不以符号开头）：%s", tag)
	}
	parts := strings.SplitN(tag, "-", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("tag 需形如 <os>-<arch>-<version>（例：linux-amd64-0.4.1）：%s", tag)
	}
	return nil
}

// tagArch / tagVersion 取 tag 的架构段与版本段
func tagArch(tag string) string {
	if parts := strings.SplitN(tag, "-", 3); len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func tagVersion(tag string) string {
	if parts := strings.SplitN(tag, "-", 3); len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

// elfArchOf 读 ELF 头识别架构（只认 64 位；x86-64=0x3E / AArch64=0xB7）。
// 上传时必须核对：否则"arm64 的 tag 里装 x86 二进制"要到目标机上执行才炸
func elfArchOf(head []byte) (string, error) {
	if len(head) < 20 || head[0] != 0x7f || head[1] != 'E' || head[2] != 'L' || head[3] != 'F' {
		return "", fmt.Errorf("不是 ELF 文件")
	}
	if head[4] != 2 {
		return "", fmt.Errorf("只接受 64 位 ELF（EI_CLASS=%d）", head[4])
	}
	machine := uint16(head[18]) | uint16(head[19])<<8
	switch machine {
	case 0x3E:
		return "amd64", nil
	case 0xB7:
		return "arm64", nil
	default:
		return "", fmt.Errorf("不支持的机器架构（e_machine=0x%X）", machine)
	}
}

// resolveVersionBinary 定位版本包：UI 上传目录优先，回落镜像种子
func resolveVersionBinary(tag string) (string, error) {
	if err := validateVersionTag(tag); err != nil {
		return "", err
	}
	for _, dir := range versionBinDirs {
		p := filepath.Join(dir, tag, "SAgent")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("平台版本库无该二进制 %s（已查 %s）", tag, strings.Join(versionBinDirs, ", "))
}

// maxVersionUploadBytes 上传上限（SAgent 当前约 11MB，留足余量）
const maxVersionUploadBytes = 64 << 20

// refreshVersionCache 刷新内存版本缓存（编辑/上传后立即生效，不必重启）
func refreshVersionCache(catDB *storepkg.DB) {
	if rows, err := catDB.ListVersions(); err == nil {
		versionMu.Lock()
		versionCatalog = rowsToSAVersions(rows)
		versionMu.Unlock()
	}
}

// handleVersionUpload 上传版本包：ELF 架构核对 → sha256 → 落 UI 目录 → 登记为 draft。
// draft 不参与自动选版，需在界面「发布」（PUT /api/versions 置 stable）后才生效
func handleVersionUpload(w http.ResponseWriter, r *http.Request, catDB *storepkg.DB) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVersionUploadBytes)
	if err := r.ParseMultipartForm(maxVersionUploadBytes); err != nil {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("解析上传失败（限 %dMB）：%v", maxVersionUploadBytes>>20, err)})
		return
	}
	tag := strings.TrimSpace(r.FormValue("tag"))
	if err := validateVersionTag(tag); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, map[string]any{"error": "缺少 file 字段：" + err.Error()})
		return
	}
	defer file.Close()

	head := make([]byte, 20)
	if _, err := io.ReadFull(file, head); err != nil {
		writeJSON(w, map[string]any{"error": "读取文件头失败：" + err.Error()})
		return
	}
	arch, err := elfArchOf(head)
	if err != nil {
		writeJSON(w, map[string]any{"error": "版本包校验失败：" + err.Error()})
		return
	}
	if want := tagArch(tag); want != arch {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("包架构与 tag 不一致：tag 声明 %s，实际 %s", want, arch)})
		return
	}

	dir := filepath.Join(versionBinDirs[0], tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, map[string]any{"error": "创建目录失败：" + err.Error()})
		return
	}
	dst := filepath.Join(dir, "SAgent")
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		writeJSON(w, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	// 校验时已从流里读走 20 字节文件头，落盘必须先回写，否则存下来的是残缺文件
	if _, err := mw.Write(head); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		writeJSON(w, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	if _, err := io.Copy(mw, file); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		writeJSON(w, map[string]any{"error": "写入失败：" + err.Error()})
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, map[string]any{"error": "落盘失败：" + err.Error()})
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, map[string]any{"error": "落盘失败：" + err.Error()})
		return
	}

	sum := hex.EncodeToString(h.Sum(nil))
	parts := strings.SplitN(tag, "-", 3)
	row := storepkg.VersionRow{
		Tag: tag, Version: tagVersion(tag), OS: parts[0], Arch: arch,
		SHA256: sum, Status: "draft", Released: time.Now().Format("2006-01-02"),
		Notes: "界面上传（" + hdr.Filename + "，待发布）", ComponentsJSON: "[]",
		UpdatedBy: cfgAuditOperator,
	}
	if err := catDB.UpsertVersion(row, "ui"); err != nil {
		writeJSON(w, map[string]any{"error": "登记版本失败：" + err.Error()})
		return
	}
	refreshVersionCache(catDB)
	addAudit("上传版本包", tag, "版本管理", "sha256="+sum[:12]+"，状态=draft（需发布才参与选版）")
	writeJSON(w, map[string]any{"ok": true, "tag": tag, "sha256": sum, "status": "draft", "path": dst})
}

// handleVersionDownload 下载版本包（受控读文件流：tag 必须已在清单里登记）
func handleVersionDownload(w http.ResponseWriter, r *http.Request, catDB *storepkg.DB) {
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	if err := validateVersionTag(tag); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	rows, err := catDB.ListVersions()
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	registered := false
	for _, row := range rows {
		if row.Tag == tag {
			registered = true
		}
	}
	if !registered {
		writeJSON(w, map[string]any{"error": "版本清单中无 " + tag})
		return
	}
	p, err := resolveVersionBinary(tag)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="SAgent-`+tag+`"`)
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	}
	_, _ = io.Copy(w, f)
}
