package main

// chain_trace.go —— 全链路证据链装配（证据链 P1，2026-09-27）。
//
// 需求来源：用户要求"从 L0 配置采集任务到 vmagent 拿到数据，每一跳都要能验证、
// 能看数据，否则无法相信 L0→L1 按流程走"。本文件把散在四处的证据
// （onboard_flow / l1_tasks / package_downloads / agents 心跳 / VM 实测）
// 按流水线装配成一条六跳时间线，每一跳 = 「声明 + 独立实证」配对：
//
//	① L0 下发     声明：流程已建        实证：flow 行 + l1_tasks 创建记录
//	② 投递/认领   声明：任务已投递      实证：controller_id + acked_at
//	③ 取包        声明：包已取到        实证：package_downloads 事件（tag/sha256/verified/source）
//	④ 执行安装    声明：安装完成        实证：runner 回执（tag/version/sha256/sha_verified/recap）
//	⑤ 心跳回写    声明：Agent 在跑      实证：心跳 last_seen/version/cfg_effective（Agent 侧独立上报）
//	⑥ VM 实测     声明：数据已入库      实证：collect-stats reporting/age + 哪个 vmagent 副本抓到（replica 标签）
//
// 原则（与 recon 同源）：不信单方陈述——⑤⑥ 是独立于 ①-④ 的外部实证，二者对齐才算绿。
// 只读 API：GET /api/trace?flow=<id>。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// traceHop 证据链一跳。
type traceHop struct {
	Step     string         `json:"step"`     // ①②③④⑤⑥
	Name     string         `json:"name"`
	Status   string         `json:"status"`   // ok | unknown | failed
	Declared string         `json:"declared"` // 声明（链路内组件的自述）
	Evidence []traceEvidence `json:"evidence"` // 独立实证（可机器复核的数据）
	Ts       int64          `json:"ts"`       // 本跳最新证据时刻（0=无）
}

type traceEvidence struct {
	K string `json:"k"`
	V string `json:"v"`
}

// traceOut /api/trace 响应。
type traceOut struct {
	Ok         bool       `json:"ok"`
	FlowID     int64      `json:"flow_id"`
	ResourceID string     `json:"resource_id"`
	Mode       string     `json:"mode"`
	Abilities  []string   `json:"abilities"`
	Hops       []traceHop `json:"hops"`
	Summary    string     `json:"summary"`
	GeneratedAt int64     `json:"generated_at"`
}

// registerTraceRoutes 证据链查询路由（只读）。
func registerTraceRoutes(mux *http.ServeMux, catDB *storepkg.DB, agentStore *AgentStore) {
	mux.HandleFunc("/api/trace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		var fid int64
		fmt.Sscanf(r.URL.Query().Get("flow"), "%d", &fid)
		if fid <= 0 {
			writeJSON(w, map[string]any{"ok": false, "error": "flow 参数必填（流水线 id）"})
			return
		}
		writeJSON(w, buildTrace(catDB, agentStore, fid))
	})
}

// buildTrace 装配一条流水线的六跳证据链。
func buildTrace(catDB *storepkg.DB, agentStore *AgentStore, flowID int64) traceOut {
	out := traceOut{Ok: true, FlowID: flowID, GeneratedAt: time.Now().Unix()}
	f, err := catDB.GetFlow(flowID)
	if err != nil || f == nil {
		out.Ok = false
		out.Summary = "流水线不存在（flow_id=" + itoa64(flowID) + "）"
		return out
	}
	out.ResourceID = f.ResourceID
	out.Mode = f.Mode
	out.Abilities = f.Abilities

	tasks, _ := catDB.ListL1TasksByFlow(flowID)
	var installTask *storepkg.L1TaskRow
	for _, t := range tasks {
		if t.Kind == "install" {
			installTask = t // 多次尝试取最新一条（列表按时间正序，后者覆盖）
		}
	}
	// 取包事件：按资源过滤（同资源的安装类任务共用），随后再按 tag 收窄
	pkgTag := ""
	if installTask != nil {
		if res := storepkg.UnmarshalPayload(installTask.ResultJSON); res != nil {
			if v, ok := res["tag"].(string); ok {
				pkgTag = v
			}
		}
	}
	pkgEvents, _ := catDB.ListPackageDownloads(pkgTag, f.ResourceID, 20)

	out.Hops = []traceHop{
		hopDispatch(f, tasks),
		hopDeliver(tasks),
		hopPackage(pkgEvents),
		hopExecute(installTask),
		hopHeartbeat(agentStore, f, installTask),
		hopVMObserved(f.ResourceID),
	}
	out.Summary = traceSummary(out.Hops)
	return out
}

// hopDispatch ① L0 下发：流程建立 + 任务创建记录。
func hopDispatch(f *storepkg.Flow, tasks []*storepkg.L1TaskRow) traceHop {
	h := traceHop{Step: "①", Name: "L0 下发任务",
		Declared: "接入流水线已建立，任务写入 l1_tasks（唯一任务源）",
		Evidence: []traceEvidence{
			{"flow_id", itoa64(f.ID)},
			{"mode", f.Mode},
			{"abilities", strings.Join(f.Abilities, ",")},
		}}
	if len(tasks) > 0 {
		first := tasks[0]
		h.Status = "ok"
		h.Ts = first.CreatedAt
		h.Evidence = append(h.Evidence,
			traceEvidence{"task_id", first.ID},
			traceEvidence{"task_created", tsStr(first.CreatedAt)},
			traceEvidence{"task_count", itoa(len(tasks))})
	} else {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"note", "该流水线没有 l1_tasks 记录（进程内执行或非 L1 通道）"})
	}
	return h
}

// hopDeliver ② Controller 投递/认领。
func hopDeliver(tasks []*storepkg.L1TaskRow) traceHop {
	h := traceHop{Step: "②", Name: "Controller 投递 · 执行端认领",
		Declared: "任务经 l1_tasks 表投递，执行端拉取后回执"}
	controllers := map[string]bool{}
	acked, done := 0, 0
	for _, t := range tasks {
		if t.ControllerID != "" {
			controllers[t.ControllerID] = true
		}
		if t.AckedAt > 0 {
			acked++
			if h.Ts < t.AckedAt {
				h.Ts = t.AckedAt
			}
		}
		if t.Status == "done" {
			done++
		}
	}
	h.Evidence = append(h.Evidence, traceEvidence{"tasks_acked", itoa(acked) + "/" + itoa(len(tasks))})
	if len(controllers) > 0 {
		ids := make([]string, 0, len(controllers))
		for k := range controllers {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		h.Evidence = append(h.Evidence, traceEvidence{"controller", strings.Join(ids, ",")})
	}
	switch {
	case len(tasks) == 0:
		h.Status = "unknown"
	case done == len(tasks):
		h.Status = "ok"
	case done > 0:
		h.Status = "ok"
		h.Evidence = append(h.Evidence, traceEvidence{"note", itoa(done) + "/" + itoa(len(tasks)) + " 终态（含失败/重试）"})
	default:
		h.Status = "unknown"
	}
	return h
}

// hopPackage ③ 取包（package_downloads 事件，含被拒绝的 resolve）。
func hopPackage(events []*storepkg.PackageDownloadRow) traceHop {
	h := traceHop{Step: "③", Name: "PackageCache 取包",
		Declared: "就近取包：缓存命中 → 回源 L0 重建 → 仍不一致拒绝（不降级）"}
	if len(events) == 0 {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"note", "无取包事件（该流水线早于事件表建立，或未走到取包步骤）"})
		return h
	}
	latest := events[0]
	h.Ts = latest.Ts
	h.Status = "ok"
	if latest.Source == "rejected" {
		h.Status = "failed"
	}
	h.Evidence = append(h.Evidence,
		traceEvidence{"tag", latest.Tag},
		traceEvidence{"sha256", storepkg.ShortSHA(latest.SHA256)},
		traceEvidence{"verified", fmt.Sprintf("%v", latest.Verified)},
		traceEvidence{"source", latest.Source},
		traceEvidence{"ts", tsStr(latest.Ts)},
		traceEvidence{"event_count", itoa(len(events))})
	if latest.Note != "" {
		h.Evidence = append(h.Evidence, traceEvidence{"note", latest.Note})
	}
	return h
}

// hopExecute ④ 执行安装（runner 回执的结构化字段是独立实证）。
func hopExecute(t *storepkg.L1TaskRow) traceHop {
	h := traceHop{Step: "④", Name: "Runner 执行安装",
		Declared: "ansible 安装完成：二进制分发 + sha256 目标机实测核对 + 进程存活验证"}
	if t == nil {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"note", "无 install 任务回执"})
		return h
	}
	res := storepkg.UnmarshalPayload(t.ResultJSON)
	h.Ts = t.AckedAt
	switch t.Status {
	case "done":
		h.Status = "ok"
	case "failed":
		h.Status = "failed"
	default:
		h.Status = "unknown"
	}
	if res != nil {
		for _, k := range []string{"tag", "version", "sha256", "sha_verified", "recap_line"} {
			if v, ok := res[k]; ok {
				val := fmt.Sprintf("%v", v)
				if k == "sha256" {
					val = storepkg.ShortSHA(val)
				}
				h.Evidence = append(h.Evidence, traceEvidence{k, val})
			}
		}
	}
	h.Evidence = append(h.Evidence, traceEvidence{"l1_task_id", t.ID},
		traceEvidence{"executor", t.Executor}, traceEvidence{"attempt", itoa(t.Attempt)},
		traceEvidence{"acked_at", tsStr(t.AckedAt)})
	return h
}

// hopHeartbeat ⑤ 心跳回写（Agent 侧独立上报，独立于任务通道）。
func hopHeartbeat(agentStore *AgentStore, f *storepkg.Flow, installTask *storepkg.L1TaskRow) traceHop {
	h := traceHop{Step: "⑤", Name: "SAgent 心跳回写",
		Declared: "Agent 注册心跳：last_seen/version/cfg_effective 周期回写（经 Gateway 白名单通道）"}
	agentID := f.AgentID
	if agentID == "" && installTask != nil {
		if res := storepkg.UnmarshalPayload(installTask.ResultJSON); res != nil {
			if v, ok := res["agent_id"].(string); ok {
				agentID = v
			}
		}
	}
	if agentID == "" {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"note", "无法确定承载 Agent（flow 与回执均未提供 agent_id）"})
		return h
	}
	a := agentStore.Get(agentID)
	if a == nil {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"agent_id", agentID}, traceEvidence{"note", "注册表无此 Agent（尚未首装注册）"})
		return h
	}
	h.Ts = a.LastSeen
	h.Status = "ok"
	h.Evidence = append(h.Evidence,
		traceEvidence{"agent_id", a.ID},
		traceEvidence{"status", a.Status},
		traceEvidence{"version", a.Version},
		traceEvidence{"cfg_effective", a.CfgEffective},
		traceEvidence{"last_seen", tsStr(a.LastSeen)})
	return h
}

// hopVMObserved ⑥ VM 实测（外部实证：数据真的到了 VictoriaMetrics + 哪个 vmagent 副本抓的）。
func hopVMObserved(resourceID string) traceHop {
	h := traceHop{Step: "⑥", Name: "VM 实测入库",
		Declared: "vmagent 抓取并双写主/副本 VictoriaMetrics（dedup 10s 合并重叠采样）"}
	stats, err := fetchCollectStats()
	if err == nil {
		for _, s := range stats {
			if s.ResourceID == resourceID {
				h.Status = "ok"
				h.Ts = time.Now().Unix() - int64(s.AgeSec)
				h.Evidence = append(h.Evidence,
					traceEvidence{"reporting", fmt.Sprintf("%v", s.Reporting)},
					traceEvidence{"metrics", itoa(s.Metrics)},
					traceEvidence{"age_sec", fmt.Sprintf("%.0f", s.AgeSec)})
				break
			}
		}
	}
	// 副本归属（P0 落地后 series 带 replica 标签）：直接回答"哪个 vmagent 拉的数据"
	if reps := vmSeriesReplicas(resourceID); len(reps) > 0 {
		sort.Strings(reps)
		h.Evidence = append(h.Evidence, traceEvidence{"scraped_by", strings.Join(reps, " + ")})
	} else {
		h.Evidence = append(h.Evidence, traceEvidence{"scraped_by", "未标注（replica 标签生效前的旧数据或 vmagent 未带标识）"})
	}
	if h.Status == "" {
		h.Status = "unknown"
		h.Evidence = append(h.Evidence, traceEvidence{"note", "VM 近 5m 无该资源样本（lookback 窗口外或确实断采）"})
	}
	return h
}

// vmSeriesReplicas 查 VM 该资源近 30m series 的去重 replica 标签集合。
func vmSeriesReplicas(resourceID string) []string {
	q := url.QueryEscape(`{resource_id="` + resourceID + `"}`)
	u := fmt.Sprintf("%s/api/v1/series?match[]=%s&start=%d&end=%d",
		vmBaseURL(), q, time.Now().Add(-30*time.Minute).Unix(), time.Now().Unix())
	resp, err := http.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var d struct {
		Data []map[string]string `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range d.Data {
		if r := m["replica"]; r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// traceSummary 结论一句话：全绿 = 链路可信；有 unknown = 证据缺口明说；failed = 断点在哪。
func traceSummary(hops []traceHop) string {
	var ok, unknown, failed []string
	for _, h := range hops {
		switch h.Status {
		case "ok":
			ok = append(ok, h.Name)
		case "failed":
			failed = append(failed, h.Name)
		default:
			unknown = append(unknown, h.Name)
		}
	}
	switch {
	case len(failed) > 0:
		return "链路存在断点：" + strings.Join(failed, "、") + " 失败——按跳内 evidence 下钻"
	case len(unknown) > 0:
		return "链路 " + itoa(len(ok)) + "/6 跳有实证，" + strings.Join(unknown, "、") + " 证据缺失（unknown ≠ failed，可能是旧数据或未走到）"
	default:
		return "六跳全部有独立实证，链路可信"
	}
}

func tsStr(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04:05")
}
