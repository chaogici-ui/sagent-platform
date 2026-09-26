package main

import (
	"encoding/json"
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
// stable 优先、版本号降序；无 stable 命中时才用 deprecated；全落空返回聚合诊断。
// 判据缺数据时不用该判据核对，但把"未核实"明确说出来——不假装通过
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
		// draft（界面上传未发布）与 disabled 都不参与自动选版：
		// 上传即生效会把"发布"这道人工闸门绕过去
		if s := strings.ToLower(strings.TrimSpace(v.Status)); s == "disabled" || s == "draft" {
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
		ds := []diag{infoDiag("version_auto_selected",
			reason+"；如需改选其它版本，可用「指定版本（需理由）」覆盖（留痕审计）")}
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
		msg = "版本清单里没有可参与匹配的条目（全部为 draft/disabled，未发布）"
	}
	return nil, "", []diag{fatalDiag("version_no_compatible", msg+"："+strings.Join(reasons, "；"),
		"① 在「版本与兼容性」页为相关版本补齐 min_kernel / 组件 glibc 要求；② 或登记适配该环境的新版本；③ 确认目标机实测值后点「↻ 重试该步」")}
}

var reLddVersion = regexp.MustCompile(`(\d+\.\d+)\s*$`)

// glibcProbeShell glibc/musl 探测片段：getconf 优先、ldd 兜底。
// 本片段会被拼进 sh -c '…' 单引号块，因此**不得含单引号**——
// awk '{print $2}' 这类写法会截断外层引号，故用 sed 配双引号剥离 "glibc " 前缀
func glibcProbeShell() string {
	return `; echo GLIBC:$(getconf GNU_LIBC_VERSION 2>/dev/null | sed -n "s/^glibc //p"); echo LDD:$(ldd --version 2>&1 | head -1)`
}

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
	if res == nil {
		return TargetEnv{}
	}
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
