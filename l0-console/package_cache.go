package main

// package_cache.go —— L1 Package Cache（架构 D5：Package Cache 子项）。
//
// 红线（PLAN 风险 A 强制约束）：
//   · 缓存只能承载 L0 校验过的只读镜像，禁止 L1 本地直接生成的产物
//   · 每个包带 L0 下发的 sha256 签名（<tag>/SAgent.sha256 + 顶层 sha256.list 清单）
//   · 安装（ansibleInstallJob）就近从缓存取包前强制校验；校验失败一律回源 L0 重建，
//     重建后仍不一致则拒绝安装——不允许降级用旧包
//   · 缓存同步幂等：L0 增量 diff 下发，重复同步不产生重复任务；缓存可从 L0 完全重建（只读可废弃重下）
//
// 形态说明：L1 进程拆分（D5 进程外壳）尚未落地，本模块先作为与「迁入 L1 Package Cache
// 进程后」完全相同的独立逻辑单元内嵌于平台进程，install 就近取缓存、失效回源 L0。
// cache root 可用 L1_PACKAGE_CACHE 覆盖（默认 data/packages），为 L1 进程化预留挂载点。

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	storepkg "github.com/sagent/l0-console/store"
)

// defaultPackageCacheRoot 缓存根（data 卷内；L1 进程化后可指向 L1 采集机本地挂载）。
var defaultPackageCacheRoot = "data/packages"

// pkgCacheMu 串行化「同步写缓存」与「安装读取校验」，避免边同步边取包读到半包。
var pkgCacheMu sync.Mutex

// pkgManifestName 顶层签名清单文件名：每行 `<tag>\t<sha256>`（L0 分发下去的一致性依据）。
const pkgManifestName = "sha256.list"

// packageCacheRoot 返回缓存根（环境变量可覆盖）。
func packageCacheRoot() string {
	if v := os.Getenv("L1_PACKAGE_CACHE"); v != "" {
		return v
	}
	return defaultPackageCacheRoot
}

func pkgCacheDir(tag string) string { return filepath.Join(packageCacheRoot(), tag) }
func pkgCacheBin(tag string) string { return filepath.Join(pkgCacheDir(tag), "SAgent") }
func pkgCacheSig(tag string) string { return filepath.Join(pkgCacheDir(tag), "SAgent.sha256") }
func pkgManifestPath() string       { return filepath.Join(packageCacheRoot(), pkgManifestName) }

// PackageCacheEntry 单版本缓存状态（/api/package-cache/status 观测，运维可读）。
type PackageCacheEntry struct {
	Tag       string `json:"tag"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
	Cached    bool   `json:"cached"`     // 缓存文件是否存在
	Verified  bool   `json:"verified"`   // 缓存文件 sha256 与 L0 权威一致（就近取包的准入条件）
	SigFile   bool   `json:"sig_file"`   // 签名文件是否存在
	SigMatch  bool   `json:"sig_match"`  // 签名文件内容 == 权威 sha
	ActualSHA string `json:"actual_sha"` // 缓存文件实际 sha（前 12 位）
	OriginSHA string `json:"origin_sha"` // L0 权威 sha（前 12 位）
}

func sha256File(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// l0EndorsedSHA 返回 L0 对该版本的权威 sha256（签名的来源，即 L0 校验口径）。
// 规则：清单登记的 sha256 优先；为 ” 时以 L0 版本库文件实测为权威（与 install 现状一致）。
// 若清单登记值存在但与 L0 文件实测不一致，视为 L0 权威源本身被篡改/未同步——返回清单值，
// 使缓存与安装对此强制失败（拒绝），与 install 既有 binary_sha256_mismatch 语义吻合。
func l0EndorsedSHA(tag string) (string, error) {
	p, err := resolveVersionBinary(tag)
	if err != nil {
		return "", err // L0 无源
	}
	fsha, err := sha256File(p)
	if err != nil {
		return "", err
	}
	if v := versionByTag(tag); v != nil {
		cat := strings.TrimSpace(v.SHA256)
		if cat != "" {
			return cat, nil // 以清单为准；若清单≠文件由 resolveInstallBinary 层拒绝
		}
	}
	return fsha, nil
}

// readManifest 读缓存顶层签名清单（tag→sha256）；损坏行跳过。
func readManifest() (map[string]string, error) {
	m := map[string]string{}
	f, err := os.Open(pkgManifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[0] != "" && len(parts[1]) == 64 {
			m[parts[0]] = parts[1]
		}
	}
	return m, sc.Err()
}

// writeManifest 回写签名清单（按 tag 排序保证确定性）。
func writeManifest(m map[string]string) error {
	if err := os.MkdirAll(packageCacheRoot(), 0o755); err != nil {
		return err
	}
	tags := make([]string, 0, len(m))
	for t := range m {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	var b strings.Builder
	for _, t := range tags {
		fmt.Fprintf(&b, "%s\t%s\n", t, m[t])
	}
	return os.WriteFile(pkgManifestPath(), []byte(b.String()), 0o644)
}

// cacheBinSHA 读缓存二进制 sha；文件缺失返回 ok=false。
func cacheBinSHA(tag string) (string, bool) {
	p := pkgCacheBin(tag)
	s, err := sha256File(p)
	if err != nil {
		return "", false
	}
	return s, true
}

// describePackageCache 遍历版本清单，产出每个版本当前缓存状态（不改写任何文件）。
func describePackageCache() []PackageCacheEntry {
	versionMu.RLock()
	cats := make([]SAVersion, len(versionCatalog))
	copy(cats, versionCatalog)
	versionMu.RUnlock()
	out := []PackageCacheEntry{}
	for i := range cats {
		cat := cats[i]
		e := PackageCacheEntry{Tag: cat.Tag, Version: cat.Version, Arch: cat.OS + "/" + cat.Arch}
		if endorsed, err := l0EndorsedSHA(cat.Tag); err == nil {
			e.OriginSHA = shortSHA(endorsed)
			if actual, ok := cacheBinSHA(cat.Tag); ok {
				e.Cached = true
				e.ActualSHA = shortSHA(actual)
				e.Verified = actual == endorsed
			} else if _, ok := pkgSigContent(cat.Tag); ok {
				e.Cached = false
			}
		}
		if content, ok := pkgSigContent(cat.Tag); ok {
			e.SigFile = true
			endorsed, _ := l0EndorsedSHA(cat.Tag)
			e.SigMatch = strings.TrimSpace(content) == endorsed
		}
		out = append(out, e)
	}
	return out
}

// pkgSigContent 读签名文件内容（存在为准）。
func pkgSigContent(tag string) (string, bool) {
	b, err := os.ReadFile(pkgCacheSig(tag))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func shortSHA(s string) string {
	if len(s) >= 12 {
		return s[:12]
	}
	return s
}

// syncPackageCacheFromL0 把 L0 版本库（data/binaries）中校验通过的镜像同步进缓存。
// 幂等：已存在且签名一致 → 跳过（不产生重复任务）；L0 源缺包/内部不一致 → 拒绝缓存该镜像
// （红线：缓存只承载 L0 校验过的镜像）。返回 (synced, rejected) 及各版本状态。
func syncPackageCacheFromL0() (int, int, []PackageCacheEntry, error) {
	pkgCacheMu.Lock()
	defer pkgCacheMu.Unlock()

	manifest, err := readManifest()
	if err != nil {
		return 0, 0, nil, err
	}
	synced, rejected := 0, 0

	versionMu.RLock()
	cats := make([]SAVersion, len(versionCatalog))
	copy(cats, versionCatalog)
	versionMu.RUnlock()

	// 先扫一遍 L0 权威源，仅把「L0 内部一致（清单==文件 或 清单为空）」的镜像纳入可缓存集合
	cacheable := map[string]string{} // tag -> endorsed sha
	for i := range cats {
		tag := cats[i].Tag
		p, err := resolveVersionBinary(tag)
		if err != nil {
			continue // L0 无源，跳过
		}
		fsha, err := sha256File(p)
		if err != nil {
			continue
		}
		cat := strings.TrimSpace(cats[i].SHA256)
		if cat != "" && cat != fsha {
			rejected++ // L0 权威源内部不一致，禁止把该镜像写进缓存
			continue
		}
		cacheable[tag] = cat
		if cat == "" {
			cacheable[tag] = fsha
		}
	}

	for tag, endorsed := range cacheable {
		dir := pkgCacheDir(tag)
		bin := pkgCacheBin(tag)
		sig := pkgCacheSig(tag)

		// 幂等：缓存文件已存在且 sha == 权威「且」签名文件 == 权威 → 跳过
		if actual, ok := cacheBinSHA(tag); ok && actual == endorsed {
			if content, ok2 := pkgSigContent(tag); ok2 && strings.TrimSpace(content) == endorsed {
				manifest[tag] = endorsed
				continue
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return synced, rejected, describeByManifest(manifest), err
		}
		// 先落临时文件再 rename，避免读到半包
		l0bin, _ := resolveVersionBinary(tag)
		tmp := bin + ".part"
		in, err := os.ReadFile(l0bin)
		if err != nil {
			return synced, rejected, describeByManifest(manifest), err
		}
		if err := os.WriteFile(tmp, in, 0o755); err != nil {
			return synced, rejected, describeByManifest(manifest), err
		}
		if err := os.Rename(tmp, bin); err != nil {
			_ = os.Remove(tmp)
			return synced, rejected, describeByManifest(manifest), err
		}
		if err := os.WriteFile(sig, []byte(endorsed+"\n"), 0o644); err != nil {
			return synced, rejected, describeByManifest(manifest), err
		}
		manifest[tag] = endorsed
		synced++
	}

	if err := writeManifest(manifest); err != nil {
		return synced, rejected, nil, err
	}
	return synced, rejected, describePackageCache(), nil
}

// describeByManifest 兜底：以内存清单拼装精简状态（写缓存中途出错时用）。
func describeByManifest(m map[string]string) []PackageCacheEntry {
	out := []PackageCacheEntry{}
	for t, s := range m {
		actual, ok := cacheBinSHA(t)
		e := PackageCacheEntry{Tag: t, OriginSHA: shortSHA(s), Cached: ok,
			Verified: ok && actual == s, SigFile: true, SigMatch: true}
		if ok {
			e.ActualSHA = shortSHA(actual)
		}
		out = append(out, e)
	}
	return out
}

// resolveInstallBinary 就近取包（供 ansibleInstallJob 取代直接读 L0 版本库）：
//  1. 缓存命中且 sha 与 L0 权威一致 → 返回缓存路径（就近取包）；
//  2. 缓存缺失/失效 → 回源 L0 重建缓存，重建后再校验一次；
//  3. 仍不一致（或 L0 无源）→ 返回错误，拒绝安装，不降级用旧包。
func resolveInstallBinary(tag string) (string, error) {
	if err := validateVersionTag(tag); err != nil {
		return "", err
	}
	bin := pkgCacheBin(tag)
	endorsed, err := l0EndorsedSHA(tag)
	if err != nil {
		return "", err // L0 无源 → 安装本就无从取包
	}
	if actual, ok := cacheBinSHA(tag); ok && actual == endorsed {
		return bin, nil // ① 就近命中
	}
	// ② 回源 L0 重建
	if _, _, _, err := syncPackageCacheFromL0(); err != nil {
		return "", fmt.Errorf("同步 L1 包缓存失败：%v", err)
	}
	if actual, ok := cacheBinSHA(tag); ok && actual == endorsed {
		return bin, nil
	}
	// ③ 拒绝（红线：不降级用旧包）
	return "", fmt.Errorf("L1 包缓存经回源 L0 重建后 sha256 仍与权威不一致，拒绝安装（不允许降级用旧包）；tag=%s", tag)
}

// resolveInstallBinaryAny 安装侧就近取包的统一入口：
// 配置了 L1_PACKAGE_CACHE_URL（指向独立 L1 Package Cache 进程，架构 D5 IN1）→ 走 HTTP resolve；
// 否则进程内 resolveInstallBinary（基线语义，红线一致：失效回源、仍不一致拒绝，不降级）。
func resolveInstallBinaryAny(tag string) (string, error) {
	if base := strings.TrimSpace(os.Getenv("L1_PACKAGE_CACHE_URL")); base != "" {
		return remoteResolveInstallBinary(base, tag)
	}
	return resolveInstallBinary(tag)
}

// handlePackageCacheStatus 只读状态：各版本 缓存/签名/校验 一致与否（运维观测就近取包准入）。
func handlePackageCacheStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ok":       true,
		"root":     packageCacheRoot(),
		"manifest": filepath.Join(packageCacheRoot(), pkgManifestName),
		"entries":  describePackageCache(),
	})
}

// handlePackageCacheResolve 就近取包解析（独立 Package Cache 进程的对外接口）：
// 复用 resolveInstallBinary 的『命中缓存→回源 L0 重建→仍不一致拒绝（不降级）』红线语义。
// GET /v1/package-cache/resolve?tag=<tag>
func handlePackageCacheResolve(w http.ResponseWriter, r *http.Request) {
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	if tag == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "tag 必填"})
		return
	}
	bin, err := resolveInstallBinary(tag)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "tag": tag, "error": err.Error(), "bin": ""})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "tag": tag, "bin": bin, "error": ""})
}

// remoteResolveInstallBinary 安装侧经独立 L1 Package Cache 进程就近取包（L1_PACKAGE_CACHE_URL 非空时启用）。
// 语义与进程内 resolveInstallBinary 一致：命中即用；失败把错误上抛给安装作业，由既有 fail 分支拒绝（不降级）。
func remoteResolveInstallBinary(base, tag string) (string, error) {
	u := strings.TrimRight(base, "/") + "/v1/package-cache/resolve?tag=" + url.QueryEscape(tag)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("L1 Package Cache 就近取包请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Bin   string `json:"bin"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("L1 Package Cache 就近取包响应解析失败: %v", err)
	}
	if !out.OK || strings.TrimSpace(out.Bin) == "" {
		msg := strings.TrimSpace(out.Error)
		if msg == "" {
			msg = "未知错误"
		}
		return "", fmt.Errorf("L1 Package Cache 就近取包拒绝: %s", msg)
	}
	return out.Bin, nil
}

// handlePackageCacheSync 触发一次 L0→L1 缓存分发（幂等：一致则跳过）。成功落审计。
func handlePackageCacheSync(w http.ResponseWriter, r *http.Request, catDB *storepkg.DB) {
	synced, rejected, entries, err := syncPackageCacheFromL0()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	addAudit("同步 L1 包缓存", "L1 Package Cache", "版本分发",
		fmt.Sprintf("synced=%d rejected=%d", synced, rejected))
	writeJSON(w, map[string]any{"ok": true, "synced": synced, "rejected": rejected, "entries": entries})
}

// registerPackageCacheRoutes 版本包缓存 API。
func registerPackageCacheRoutes(mux *http.ServeMux, catDB *storepkg.DB) {
	mux.HandleFunc("/api/package-cache/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		handlePackageCacheStatus(w, r)
	})
	mux.HandleFunc("/v1/package-cache/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		// 与 L0 侧 /api/package-cache/status 同源，但独立进程经此对外就近取包
		handlePackageCacheResolve(w, r)
	})
}

// cfgPackageCacheListen 独立 Package Cache 进程监听地址（PACKAGE_CACHE_LISTEN，默认 :8450）。
var cfgPackageCacheListen = envOr("PACKAGE_CACHE_LISTEN", ":8450")

// runPackageCacheDaemon 独立 Package Cache 进程入口：不建 DB、不起 console，
// 只加载版本清单（文件口径，L1 不裁决版本）+ 起 package-cache HTTP 服务并阻塞。
// 供 `l0-console -package-cache` 模式与 compose `l1-package-cache` 服务使用（架构 D5 IN1）。
func runPackageCacheDaemon() {
	fmt.Println("L1 Package Cache daemon starting on " + cfgPackageCacheListen + " (root=" + packageCacheRoot() + ")")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/package-cache/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		handlePackageCacheStatus(w, r)
	})
	mux.HandleFunc("/api/package-cache/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		synced, rejected, entries, err := syncPackageCacheFromL0()
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "synced": synced, "rejected": rejected, "entries": entries})
	})
	mux.HandleFunc("/v1/package-cache/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		handlePackageCacheResolve(w, r)
	})
	log.Fatal(http.ListenAndServe(cfgPackageCacheListen, mux))
}
