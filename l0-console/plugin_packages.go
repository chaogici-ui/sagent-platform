package main

// plugin_packages.go —— L0 插件超市（插件包仓，2026-09-27）。
//
// 与 L1 Package Cache（package_cache.go，承载 SAgent 主程序镜像）同红线语义，
// 承载对象扩展到采集机侧 exporter 插件二进制（R0-1 配套）：
//   · 目录约定：data/plugin-packages/<包名>/<版本>/<文件名>（版本目录取字典序最新为 latest）
//   · /api/plugin-packages/{name}/manifest  → {ok, name, version, filename, sha256, size}
//   · /api/plugin-packages/{name}/download  → 二进制流（?version= 可指定，缺省 latest）
// 校验口径：manifest 的 sha256 为 L0 权威签名（文件实测生成）；SAgent 侧下载后强制校验，
// 不一致拒绝拉起（不降级用未校验二进制）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pluginPackageRoot 插件包仓根目录（data 卷内）。
func pluginPackageRoot() string {
	if v := os.Getenv("PLUGIN_PACKAGE_ROOT"); v != "" {
		return v
	}
	return "data/plugin-packages"
}

// pluginPackageVersions 列出包名下全部版本（字典序升序，最后一个为 latest）。
func pluginPackageVersions(name string) []string {
	root := filepath.Join(pluginPackageRoot(), sanitizePkgName(name))
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// pluginPackageBinary 定位某版本下的二进制文件（目录内首个普通文件；约定单文件包）。
func pluginPackageBinary(name, version string) (string, error) {
	if sanitizePkgName(name) != name || sanitizePkgName(version) != version {
		return "", fmt.Errorf("非法包名/版本")
	}
	dir := filepath.Join(pluginPackageRoot(), name, version)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("包不存在: %s/%s", name, version)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".sha256") || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		return filepath.Join(dir, e.Name()), nil
	}
	return "", fmt.Errorf("包目录为空: %s/%s", name, version)
}

// sanitizePkgName 包名/版本目录穿越防护：只允许字母数字 . _ -
func sanitizePkgName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// handlePluginPackageManifest GET /api/plugin-packages/{name}/manifest
func handlePluginPackageManifest(w http.ResponseWriter, r *http.Request, name string) {
	versions := pluginPackageVersions(name)
	if len(versions) == 0 {
		writeJSON(w, map[string]any{"ok": false, "name": name, "error": "插件包不存在或无版本"})
		return
	}
	version := versions[len(versions)-1] // latest
	if v := strings.TrimSpace(r.URL.Query().Get("version")); v != "" {
		found := false
		for _, vv := range versions {
			if vv == v {
				version, found = v, true
				break
			}
		}
		if !found {
			writeJSON(w, map[string]any{"ok": false, "name": name, "error": "指定版本不存在: " + v})
			return
		}
	}
	bin, err := pluginPackageBinary(name, version)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "name": name, "error": err.Error()})
		return
	}
	st, err := os.Stat(bin)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "name": name, "error": err.Error()})
		return
	}
	sum, err := fileSHA256(bin)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "name": name, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "name": name, "version": version,
		"filename": filepath.Base(bin), "sha256": sum, "size": st.Size(),
	})
}

// handlePluginPackageDownload GET /api/plugin-packages/{name}/download?version=
func handlePluginPackageDownload(w http.ResponseWriter, r *http.Request, name string) {
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if version == "" {
		versions := pluginPackageVersions(name)
		if len(versions) == 0 {
			http.Error(w, `{"ok":false,"error":"插件包不存在"}`, http.StatusNotFound)
			return
		}
		version = versions[len(versions)-1]
	}
	bin, err := pluginPackageBinary(name, version)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	f, err := os.Open(bin)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"读取包文件失败"}`, http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(bin)))
	_, _ = io.Copy(w, f)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// registerPluginPackageRoutes 插件超市 API（供 SAgent package-cache get）。
func registerPluginPackageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/plugin-packages/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/plugin-packages/")
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "包名必填"})
			return
		}
		name := sanitizePkgName(parts[0])
		if name == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "包名非法"})
			return
		}
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		switch action {
		case "manifest":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", 405)
				return
			}
			handlePluginPackageManifest(w, r, name)
		case "download":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", 405)
				return
			}
			handlePluginPackageDownload(w, r, name)
		default:
			writeJSON(w, map[string]any{"ok": false, "error": "未知动作（支持 manifest/download）"})
		}
	})
}
