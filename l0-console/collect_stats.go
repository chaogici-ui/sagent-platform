package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// 采集实况（资源页「采集状态 / 最近数据」两列的数据源）。
//
// 为什么要有这个接口：台账说"已接入"、VM 说"没有数据"，是两件独立的事，必须能同时看见。
// 判据取平台自己写进 vmagent file_sd 的 resource_id 标签——不认 IP 字符串、不认指标名，
// 换目标机/换端口/换指标都不影响（零硬编码）。instance 一并返回：演示容器等
// 没走接入流水线的目标只有 instance 可用，前端按 instance 兜底匹配。
const (
	// 指标条数：该资源名下近 5m 有过样本的 series 数
	vmQResourceMetrics = `count by (resource_id, instance) (count_over_time({resource_id!=""}[5m]))`
	// 新鲜度：该资源最新样本距今多少秒（lookback 5m，超时无样本的行自然消失）
	vmQResourceFreshness = `time() - max by (resource_id, instance) (timestamp({resource_id!=""}))`
)

type resourceStat struct {
	ResourceID string  `json:"resource_id"`
	Instance   string  `json:"instance"`
	Metrics    int     `json:"metrics"`
	AgeSec     float64 `json:"age_sec"` // -1 = 无样本（VM 里没有该目标的任何数据）
	Reporting  bool    `json:"reporting"`
}

// vmRow VM 向量里的一行（只要平台关心的两个标签 + 数值）
type vmRow struct {
	ResourceID string
	Instance   string
	Value      float64
}

// parseVMRows 解析 VM instant query 响应。跳过无身份标签或数值非法的行——
// 一条脏样本不该让整页资源状态变"未知"；但非 JSON（网关错误页）必须报错，
// 因为"VM 挂了"与"确实没有数据"是两种结论
func parseVMRows(body []byte) ([]vmRow, error) {
	var v struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []interface{}     `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("VM 响应非 JSON：%w", err)
	}
	out := make([]vmRow, 0, len(v.Data.Result))
	for _, r := range v.Data.Result {
		id, inst := r.Metric["resource_id"], r.Metric["instance"]
		if id == "" && inst == "" {
			continue
		}
		if len(r.Value) < 2 {
			continue
		}
		raw, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		out = append(out, vmRow{ResourceID: id, Instance: inst, Value: f})
	}
	return out, nil
}

// statKey 对齐键：resource_id 优先（平台写入的权威标识），instance 兜底
func statKey(r vmRow) string {
	if r.ResourceID != "" {
		return r.ResourceID
	}
	return r.Instance
}

// mergeResourceStats 把两路查询并成资源行：resource_id 优先、instance 兜底。
// 只有一路有数据的行也保留（年龄未知记 -1），前端据此显示"断采"而不是拿 0 当真值
func mergeResourceStats(metrics, age []vmRow) []resourceStat {
	index := map[string]*resourceStat{}
	order := []string{}
	get := func(r vmRow) *resourceStat {
		k := statKey(r)
		if st, ok := index[k]; ok {
			if st.Instance == "" {
				st.Instance = r.Instance
			}
			if st.ResourceID == "" {
				st.ResourceID = r.ResourceID
			}
			return st
		}
		st := &resourceStat{ResourceID: r.ResourceID, Instance: r.Instance, AgeSec: -1}
		if st.ResourceID == "" {
			st.ResourceID = k // 只有 instance 时，用 instance 充当行标识（演示容器）
		}
		index[k] = st
		order = append(order, k)
		return st
	}
	for _, r := range metrics {
		st := get(r)
		st.Metrics = int(r.Value)
		st.Reporting = r.Value > 0
	}
	for _, r := range age {
		get(r).AgeSec = r.Value
	}
	sort.Strings(order)
	out := make([]resourceStat, 0, len(order))
	for _, k := range order {
		out = append(out, *index[k])
	}
	return out
}

// vmQuery 执行一次 instant query，返回响应体
func vmQuery(query string) ([]byte, error) {
	u := fmt.Sprintf("%s/api/v1/query?query=%s", vmBaseURL(), url.QueryEscape(query))
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// fetchCollectStats 两路查询；任一失败即整体失败（宁可显示"取不到"也不显示错的 0）
func fetchCollectStats() ([]resourceStat, error) {
	mBody, err := vmQuery(vmQResourceMetrics)
	if err != nil {
		return nil, err
	}
	metrics, err := parseVMRows(mBody)
	if err != nil {
		return nil, err
	}
	aBody, err := vmQuery(vmQResourceFreshness)
	if err != nil {
		return nil, err
	}
	age, err := parseVMRows(aBody)
	if err != nil {
		return nil, err
	}
	return mergeResourceStats(metrics, age), nil
}

var (
	collectStatsMu    sync.Mutex
	collectStatsCache []resourceStat
	collectStatsAt    time.Time
)

// handleCollectStats GET /api/collect-stats —— 采集实况（15s 缓存，与抓取间隔同量级）
func handleCollectStats(w http.ResponseWriter, r *http.Request) {
	collectStatsMu.Lock()
	if collectStatsCache != nil && time.Since(collectStatsAt) < 15*time.Second {
		out, at := collectStatsCache, collectStatsAt
		collectStatsMu.Unlock()
		writeJSON(w, map[string]interface{}{
			"resources": out, "vm_reachable": true,
			"generated_at": at.Format("2006-01-02 15:04:05"),
		})
		return
	}
	collectStatsMu.Unlock()

	rows, err := fetchCollectStats()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "采集实况查询失败（VM 不可达或响应异常）：" + err.Error()})
		return
	}
	collectStatsMu.Lock()
	collectStatsCache = rows
	collectStatsAt = time.Now()
	at := collectStatsAt
	collectStatsMu.Unlock()
	writeJSON(w, map[string]interface{}{
		"resources": rows, "vm_reachable": true,
		"generated_at": at.Format("2006-01-02 15:04:05"),
	})
}

func registerCollectStatsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/collect-stats", handleCollectStats)
}
