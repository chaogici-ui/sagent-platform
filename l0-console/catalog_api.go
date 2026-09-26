package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sagent/l0-console/importer"
	"github.com/sagent/l0-console/store"
)

var catalogDB *store.DB

// registerCatalogRoutes 注册插件能力目录（目录库 PG）相关 API
func registerCatalogRoutes(mux *http.ServeMux, db *store.DB) {
	catalogDB = db

	mux.HandleFunc("/api/catalog/plugins", handleCatalogPlugins)
	mux.HandleFunc("/api/catalog/plugins/", handleCatalogPluginSub)
	mux.HandleFunc("/api/catalog/metrics/", handleCatalogMetricDelete)
	mux.HandleFunc("/api/catalog/dashboards/", handleCatalogDashboardGet)
	mux.HandleFunc("/api/catalog/reimport", handleCatalogReimport)
	mux.HandleFunc("/api/catalog/sync-report", handleCatalogSyncReport)

	// 指标治理：仪表盘引用查询（废弃/删除前先看影响面）
	mux.HandleFunc("/api/metrics/refs", handleMetricRefs)

	// 指标治理：多渠道视图（D9 五渠道 × 层级树）+ facets
	mux.HandleFunc("/api/metrics/gov", handleMetricsGov)
	mux.HandleFunc("/api/metrics/gov/facets", handleMetricsGovFacets)

	// VM 指标名清单（近 24h 有数据上报的指标名集合，用于目录活跃度标注）
	mux.HandleFunc("/api/vm/names", handleVMNames)

	// VM range 查询代理（仪表盘时序图数据）
	mux.HandleFunc("/api/vm/query_range", handleVMQueryRange)

	// VM label values 代理（仪表盘实例下拉）
	mux.HandleFunc("/api/vm/label-values", handleVMLabelValues)

	// M1-⑥ 三方对账引擎
	registerReconRoute(mux)
}

// handleMetricsGov GET /api/metrics/gov 多渠道指标治理视图（D9）
// 过滤：query/level/major/channel/grp/ownership/only_phase1
func handleMetricsGov(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, map[string]interface{}{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	list, err := catalogDB.ListMetricsGovernance(
		q.Get("query"), q.Get("level"), q.Get("major"), q.Get("channel"),
		q.Get("grp"), q.Get("ownership"), q.Get("only_phase1") == "1", q.Get("plugin_name"))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.Metric{}
	}
	writeJSON(w, list)
}

// handleMetricsGovFacets GET /api/metrics/gov/facets 层级树/渠道/分组计数
func handleMetricsGovFacets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, map[string]interface{}{"error": "method not allowed"})
		return
	}
	f, err := catalogDB.MetricGovFacets()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, f)
}

// handleVMNames 代理 VM /api/v1/label/__name__/values（默认近 24h 窗口）
func handleVMNames(w http.ResponseWriter, r *http.Request) {
	end := time.Now().Unix()
	u := fmt.Sprintf("%s/api/v1/label/__name__/values?start=%d&end=%d", vmBaseURL(), end-86400, end)
	resp, err := http.Get(u)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// handleMetricRefs GET /api/metrics/refs?name=xxx 查询指标被哪些仪表盘引用
func handleMetricRefs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, map[string]interface{}{"error": "name required"})
		return
	}
	refs, err := catalogDB.FindDashboardRefs(name)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if refs == nil {
		refs = []store.DashRef{}
	}
	writeJSON(w, map[string]interface{}{"count": len(refs), "items": refs})
}

// handleCatalogSyncReport GET 最近一次移植的变更报告
func handleCatalogSyncReport(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile("data/sync_report.json")
	if err != nil {
		writeJSON(w, map[string]interface{}{"time": "", "plugins": 0, "diffs": []interface{}{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func vmBaseURL() string {
	// 统一走 main.go 的 vmBase()（VM_URL 环境变量驱动），避免多处默认值漂移
	return vmBase()
}

// handleVMLabelValues 代理 /api/v1/label/{label}/values?match[]={metric}
// /api/vm/label-values?metric=mysql_up&label=instance
func handleVMLabelValues(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	label := r.URL.Query().Get("label")
	if metric == "" || label == "" {
		writeJSON(w, map[string]interface{}{"error": "metric and label required"})
		return
	}
	vmURL := vmBase()
	match := fmt.Sprintf(`{__name__=%q}`, metric)
	u := fmt.Sprintf("%s/api/v1/label/%s/values?match[]=%s", vmURL, url.PathEscape(label), url.QueryEscape(match))
	resp, err := http.Get(u)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func handleCatalogPlugins(w http.ResponseWriter, r *http.Request) {
	list, err := catalogDB.ListPlugins()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*store.Plugin{}
	}
	writeJSON(w, list)
}

// handleCatalogPluginSub 处理 /api/catalog/plugins/{id}[/metrics|/doc|/dashboards]
func handleCatalogPluginSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/catalog/plugins/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, map[string]interface{}{"error": "plugin id required"})
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "invalid plugin id"})
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		p, err := catalogDB.GetPlugin(id)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "plugin not found"})
			return
		}
		writeJSON(w, p)
	case sub == "doc" && r.Method == http.MethodPut:
		var body struct {
			Doc string `json:"doc"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, map[string]interface{}{"error": "bad json"})
			return
		}
		if err := catalogDB.UpdatePluginDoc(id, body.Doc); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case sub == "metrics" && len(parts) == 2 && r.Method == http.MethodGet:
		q := r.URL.Query()
		list, err := catalogDB.ListMetrics(id, q.Get("query"), q.Get("type"), q.Get("unit"), q.Get("view"))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if list == nil {
			list = []*store.Metric{}
		}
		writeJSON(w, list)
	case sub == "metrics" && r.Method == http.MethodPost:
		var m store.Metric
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, map[string]interface{}{"error": "bad json"})
			return
		}
		if m.Name == "" || m.Expression == "" {
			writeJSON(w, map[string]interface{}{"error": "name 和 expression(PromQL) 必填"})
			return
		}
		m.PluginID = id
		m.Source = "custom"
		if m.MetricType == "" {
			m.MetricType = "gauge"
		}
		mid, err := catalogDB.InsertMetric(&m)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"id": mid})
	case sub == "metrics" && len(parts) >= 3 && parts[2] == "filters" && r.Method == http.MethodGet:
		types, _ := catalogDB.MetricTypes(id)
		units, _ := catalogDB.MetricUnits(id)
		writeJSON(w, map[string]interface{}{"types": types, "units": units})
	case sub == "dashboards" && r.Method == http.MethodGet:
		list, err := catalogDB.ListDashboards(id)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if list == nil {
			list = []*store.Dashboard{}
		}
		writeJSON(w, list)
	case sub == "export" && r.Method == http.MethodGet:
		p, err := catalogDB.GetPlugin(id)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "plugin not found"})
			return
		}
		ms, err := catalogDB.ListMetrics(id, "", "", "", "all")
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if ms == nil {
			ms = []*store.Metric{}
		}
		ds, err := catalogDB.ListDashboards(id)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		dashes := make([]map[string]interface{}, 0, len(ds))
		for _, d := range ds {
			var raw interface{}
			_ = json.Unmarshal([]byte(d.ConfigsJSON), &raw)
			dashes = append(dashes, map[string]interface{}{"name": d.Name, "configs": raw})
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s-plugin.json\"", p.Name))
		writeJSON(w, map[string]interface{}{
			"format":        "sagent-plugin-bundle/v1",
			"plugin":        p,
			"collect_doc_md": p.CollectDocMD,
			"metrics":       ms,
			"dashboards":    dashes,
		})
	default:
		writeJSON(w, map[string]interface{}{"error": "not found"})
	}
}

// handleCatalogMetricDelete DELETE /api/catalog/metrics/{id}（仅 custom 可删）
func handleCatalogMetricDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, map[string]interface{}{"error": "method not allowed"})
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/catalog/metrics/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "invalid metric id"})
		return
	}
	if err := catalogDB.DeleteMetric(id); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleCatalogDashboardGet GET /api/catalog/dashboards/{id}
func handleCatalogDashboardGet(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/catalog/dashboards/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "invalid dashboard id"})
		return
	}
	d, err := catalogDB.GetDashboard(id)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "dashboard not found"})
		return
	}
	writeJSON(w, d)
}

// handleCatalogReimport POST /api/catalog/reimport 手动触发重新导入
func handleCatalogReimport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]interface{}{"error": "method not allowed"})
		return
	}
	n, err := importerSync()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"imported": n})
}

// importerSync 供启动与手动重导共用
func importerSync() (int, error) {
	if catalogDB == nil {
		return 0, fmt.Errorf("catalog db not ready")
	}
	return importer.Sync(catalogDB, "data/integrations")
}

// handleVMQueryRange 代理 VictoriaMetrics range 查询
// /api/vm/query_range?query=...&start=...&end=...&step=...
func handleVMQueryRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("query")
	if query == "" {
		writeJSON(w, map[string]interface{}{"error": "query required"})
		return
	}
	u := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%s&end=%s&step=%s",
		vmBase(), url.QueryEscape(query), url.QueryEscape(q.Get("start")),
		url.QueryEscape(q.Get("end")), url.QueryEscape(q.Get("step")))
	resp, err := http.Get(u)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
	log.Printf("query_range: %s", query)
}
