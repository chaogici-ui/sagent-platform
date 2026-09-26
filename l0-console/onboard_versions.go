package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"

	storepkg "github.com/sagent/l0-console/store"
	"gopkg.in/yaml.v3"
)

// SAVersion SAgent 版本清单条目（data/versions.yaml 为种子，目录库为权威源）
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

var (
	versionMu      sync.RWMutex
	versionCatalog []SAVersion
)

// loadVersionCatalog 加载版本清单：data/versions.yaml 只作种子——
// 按 tag 合并进目录库（界面改过的行 source=ui 不被覆盖），再从库读回内存缓存。
// DB 不可用时回落纯文件模式（本地开发/离线兜底），与旧实现行为一致
func loadVersionCatalog(path string, db *storepkg.DB) {
	var parsed []SAVersion
	if data, err := os.ReadFile(path); err != nil {
		println("versions catalog not loaded:", err.Error())
	} else {
		var c struct {
			Versions []SAVersion `yaml:"versions"`
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			println("versions catalog parse error:", err.Error())
		} else {
			// 按 tag 去重：清单手工维护，重复条目会让候选列表同一版本出现两次（2026-09-22 实测踩到）。
			// 重复且内容冲突时后写的覆盖先写的（文件末尾为准），与人工编辑直觉一致
			seen := map[string]int{}
			deduped := c.Versions[:0]
			for _, v := range c.Versions {
				if j, dup := seen[v.Tag]; dup {
					deduped[j] = v
					continue
				}
				seen[v.Tag] = len(deduped)
				deduped = append(deduped, v)
			}
			parsed = deduped
		}
	}
	if db == nil {
		versionMu.Lock()
		versionCatalog = parsed
		versionMu.Unlock()
		println("versions catalog loaded (file only):", len(parsed))
		return
	}
	for _, v := range parsed {
		cj, _ := json.Marshal(v.Components)
		row := storepkg.VersionRow{
			Tag: v.Tag, Version: v.Version, OS: v.OS, Arch: v.Arch, SHA256: v.SHA256,
			Status: v.Status, Released: v.Released, Notes: v.Notes,
			MinKernel: v.MinKernel, ComponentsJSON: string(cj),
		}
		if err := db.UpsertVersionSeed(row); err != nil {
			println("version seed upsert error:", err.Error())
		}
	}
	rows, err := db.ListVersions()
	if err != nil {
		versionMu.Lock()
		versionCatalog = parsed
		versionMu.Unlock()
		println("versions catalog loaded (file only, db read failed):", len(parsed))
		return
	}
	versionMu.Lock()
	versionCatalog = rowsToSAVersions(rows)
	versionMu.Unlock()
	println("versions catalog loaded from db:", len(versionCatalog), "entries (seeded", len(parsed), ")")
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

// normOS / normArch 探测原始输出 → 清单口径。
// uname 给的是 x86_64/aarch64/Darwin 这类原始值，人工表单也可能随意大小写——
// 匹配前先归一化，避免"实测 aarch64 却匹配不上 arm64"这种表里两张皮
func normOS(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linux":
		return "linux"
	case "windows", "mingw64":
		return "windows"
	case "darwin":
		return "darwin"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func normArch(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv7":
		return "armv7"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// versionByTag 按 tag 精确取版本条目（安装作业核对 sha256 用），未登记返回 nil
func versionByTag(tag string) *SAVersion {
	versionMu.RLock()
	defer versionMu.RUnlock()
	for i := range versionCatalog {
		if versionCatalog[i].Tag == tag {
			v := versionCatalog[i]
			return &v
		}
	}
	return nil
}

// compatibleVersions 按实测 OS/架构过滤可选版本：stable 在前、deprecated 殿后、
// 同档内版本号新的在前——清单顺序即界面默认选中顺序
func compatibleVersions(osName, arch string) []SAVersion {
	osName, arch = normOS(osName), normArch(arch)
	versionMu.RLock()
	defer versionMu.RUnlock()
	out := []SAVersion{}
	for _, v := range versionCatalog {
		if normOS(v.OS) == osName && normArch(v.Arch) == arch {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].Status == "stable", out[j].Status == "stable"
		if si != sj {
			return si
		}
		// 版本新在前必须按语义版本比：字符串比较会得出 "0.9.0" > "0.10.0"，
		// 界面默认选中的"最新版"就会选错
		return semverLess(out[j].Version, out[i].Version)
	})
	return out
}
