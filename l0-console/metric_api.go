package main

import (
	"encoding/json"
	"net/http"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// MetricDef 指标中心条目（/api/metrics 前端契约）
type MetricDef struct {
	Plugin     string `json:"plugin"`
	Cat        string `json:"cat"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Unit       string `json:"unit"`
	Desc       string `json:"desc"`
	Labels     string `json:"labels"`
	Status     string `json:"status,omitempty"`
	Source     string `json:"source,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	Expression string `json:"expression,omitempty"` // 原始指标名（VM 活跃度匹配用）
}

// registerMetricRoutes 注册指标中心 API（统一存储：catalog 库 / PG）
func registerMetricRoutes(mux *http.ServeMux) {
	// API: 指标目录（统一存储：catalog 库 / PG；data/metrics.json 仅作首次种子）
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Format("2006-01-02 15:04")

		if r.Method == http.MethodDelete {
			name := r.URL.Query().Get("name")
			found, err := catalogDB.DeleteMetricByName(name)
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{"deleted": found})
			return
		}

		if r.Method == http.MethodPut {
			// 批量更新：按指标名修改非空字段
			var updates []MetricDef
			json.NewDecoder(r.Body).Decode(&updates)
			ms := make([]*storepkg.Metric, 0, len(updates))
			for i := range updates {
				u := &updates[i]
				ms = append(ms, &storepkg.Metric{
					Name: u.Name, Cat: u.Cat, Unit: u.Unit, Note: u.Desc, Labels: u.Labels,
					Status: u.Status, MetricType: u.Type, PluginName: u.Plugin, UpdatedAt: now,
				})
			}
			updated, err := catalogDB.UpdateMetricsFields(ms)
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{"updated": updated})
			return
		}

		if r.Method == http.MethodPost {
			var body struct {
				Metrics []MetricDef `json:"metrics"`
				Delete  []string    `json:"delete"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, name := range body.Delete {
				catalogDB.DeleteMetricByName(name)
			}
			for i := range body.Metrics {
				d := &body.Metrics[i]
				if d.Source == "" {
					d.Source = "manual"
				}
				if d.Status == "" {
					d.Status = "active"
				}
				m := &storepkg.Metric{
					Name: d.Name, Unit: d.Unit, Note: d.Desc, MetricType: d.Type,
					Source: d.Source, Cat: d.Cat, Labels: d.Labels, Status: d.Status,
					PluginName: d.Plugin, UpdatedAt: now,
				}
				if pid := catalogDB.ResolvePluginID(d.Plugin); pid > 0 {
					m.PluginID = pid
				}
				if err := catalogDB.UpsertMetricByName(m); err != nil {
					writeJSON(w, map[string]interface{}{"error": err.Error()})
					return
				}
			}
			total, _ := catalogDB.CountMetrics()
			writeJSON(w, map[string]interface{}{"added": len(body.Metrics), "deleted": len(body.Delete), "total": total})
			return
		}

		// GET：指标中心全量视图（plugin 字段回落规则见 store.ListAllMetrics）
		q := r.URL.Query()
		list, err := catalogDB.ListAllMetrics(q.Get("query"), q.Get("plugin"))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if list == nil {
			list = []*storepkg.Metric{}
		}
		out := make([]MetricDef, 0, len(list))
		for _, m := range list {
			out = append(out, MetricDef{
				Plugin: m.Plugin, Cat: m.Cat, Name: m.Name, Type: m.MetricType,
				Unit: m.Unit, Desc: m.Note, Labels: m.Labels, Status: m.Status,
				Source: m.Source, UpdatedAt: m.UpdatedAt, Expression: m.Expression,
			})
		}
		writeJSON(w, out)
	})
}
