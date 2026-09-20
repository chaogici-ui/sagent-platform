package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sagent/l0-console/store"
)

// n9eMetric 夜莺 metrics/*.json 里的单条指标结构（只取需要的字段）
type n9eMetric struct {
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	Note       string `json:"note"`
	Expression string `json:"expression"`
	MetricType string `json:"metric_type"`
}

// categoryMap 目录名 → 插件分类（未命中归「其他」）
var categoryMap = map[string]string{
	"MySQL": "数据库", "PostgreSQL": "数据库", "MongoDB": "数据库", "Redis": "数据库",
	"Elasticsearch": "数据库", "ClickHouse": "数据库", "Doris": "数据库", "Oracle": "数据库",
	"SQLServer": "数据库", "OceanBase": "数据库", "TiDB": "数据库", "Polardb": "数据库",
	"Kafka": "中间件", "RabbitMQ": "中间件", "RocketMQ": "中间件", "EMQX": "中间件",
	"Nginx": "中间件", "Tomcat": "中间件", "HAProxy": "中间件", "Consul": "中间件",
	"Zookeeper": "中间件", "Etcd": "中间件", "Nacos": "中间件", "Ceph": "中间件",
	"Zabbix": "中间件", "Canal": "中间件", "Zhiyuan": "中间件",
	"Docker": "系统", "Linux": "系统", "Windows": "系统", "Switch": "系统",
	"Kubernetes": "系统", "Prometheus": "系统", "VictoriaMetrics": "系统",
	"Ziyan": "系统", "TimeSeriesLib": "系统",
	"Jenkins": "DevOps", "Gitlab": "DevOps", "TFS": "DevOps",
	"AliYun": "云服务", "TencentCloud": "云服务", "AWS_CloudWatch": "云服务", "Azure": "云服务",
	"AutoMQ": "云服务", "Akamai": "云服务", "CDN": "云服务", "AppDynamics": "云服务",
	"HTTP_Response": "可用性探测", "Tuning": "其他", "Machine": "系统",
	// 内置采集能力插件包（非外部 exporter 接入）
	"log_metrics": "日志", "sagent_self": "系统", "vmagent_scrape": "系统",
}

// typeMap 目录名 → 插件类型（未命中默认 exporter）。内置采集能力包不是外部
// exporter 进程，标记为 builtin，避免面板/接入向导按「填地址」语义处理。
var typeMap = map[string]string{
	"log_metrics": "builtin", "sagent_self": "builtin", "vmagent_scrape": "builtin",
}

// channelMap 内置采集能力包指标的渠道归属（治理视图渠道维度用；
// 未命中的插件包指标不标渠道，保持外部 exporter 语义）
var channelMap = map[string]string{
	"log_metrics": "log", "sagent_self": "builtin", "vmagent_scrape": "builtin",
}

var identRe = regexp.MustCompile(`\$?(ident|address)\b`)

// cleanDashVariables 把 categraf 标签变量（ident/address）统一映射为标准 exporter 的 instance
func cleanDashVariables(v interface{}) {
	switch tv := v.(type) {
	case map[string]interface{}:
		if name, ok := tv["name"].(string); ok && (name == "ident" || name == "address") {
			tv["name"] = "instance"
		}
		if def, ok := tv["definition"].(string); ok {
			tv["definition"] = cleanExprText(def)
		}
		for _, k := range []string{"datasource", "options"} {
			if sub, ok := tv[k]; ok {
				cleanDashVariables(sub)
			}
		}
	case []interface{}:
		for _, item := range tv {
			cleanDashVariables(item)
		}
	}
}

// cleanExprText 替换表达式/文本里的 ident/address 标签为 instance
func cleanExprText(s string) string {
	return identRe.ReplaceAllString(s, "instance")
}

// cleanConfigs 深度清洗 dashboard configs：所有字符串里的 $ident/$address → $instance
func cleanConfigs(v interface{}) interface{} {
	switch tv := v.(type) {
	case map[string]interface{}:
		// 变量定义走专门处理
		if vars, ok := tv["var"]; ok {
			cleanDashVariables(vars)
		}
		for k, item := range tv {
			if k == "var" {
				continue
			}
			tv[k] = cleanConfigs(item)
		}
		return tv
	case []interface{}:
		for i, item := range tv {
			tv[i] = cleanConfigs(item)
		}
		return tv
	case string:
		return cleanExprText(tv)
	default:
		return v
	}
}

// exporterDocTemplate 生成标准 exporter 视角的采集说明（单机版/社区版），不带任何 categraf toml
func exporterDocTemplate(name, displayName string) string {
	lower := strings.ToLower(name)
	return fmt.Sprintf(`# %s 采集说明

本插件采集能力基于标准 / 自定义开发的 **Prometheus Exporter**，平台通过 Prometheus 拉取协议抓取其暴露的 /metrics 指标端点。

## 接入前提

- 目标主机上已部署并运行对应的 exporter（如「%s」），且可被采集端访问
- 已知 exporter 监听地址与端口

## 单机版接入

1. 在目标机器上启动 exporter，默认监听 :9104（端口以实际为准）：

   ` + "```bash" + `
   ./%s_exporter --web.listen-address=:9104 [其他启动参数]
   ` + "```" + `

2. 验证指标端点可访问：

   ` + "```bash" + `
   curl -s http://<目标机IP>:9104/metrics | head
   ` + "```" + `

3. 在本平台「采集目标」菜单，新建采集目标并绑定本插件，填入 exporter 地址（IP:端口）。

## 社区版（集群 / 多实例）接入

1. 每个 %s 实例各部署一个 exporter 进程，使用不同端口，分别暴露各实例指标。
2. 统一在「采集目标」菜单批量登记多个目标，标签中用 instance 区分各实例。
3. 进入本插件「仪表盘」，顶部实例下拉可切换查看任意实例的大盘。

## 指标核对

接入完成后，可在「指标说明」tab 检索指标名，确认数据已上报；常见指标前缀见下方指标清单。
`, displayName, lower, lower, displayName)
}

// SyncReport 移植变更报告：本次同步相对库中已有内置指标集的新增/移除
type SyncReport struct {
	Time    string       `json:"time"`
	Plugins int          `json:"plugins"`
	Diffs   []PluginDiff `json:"diffs"`
}

// PluginDiff 单个插件的指标差异
type PluginDiff struct {
	Plugin  string   `json:"plugin"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// Sync 扫描 integrations 目录增量入库。只插入/更新 builtin 数据，不覆盖用户编辑。
// 同时把指标变更报告写入 data/sync_report.json（上游升级重导时可对比变化）。
func Sync(s *store.DB, integDir string) (int, error) {
	report := &SyncReport{Time: time.Now().Format("2006-01-02 15:04:05")}
	n, err := syncIntegrations(s, integDir, report)
	if err != nil {
		return n, err
	}
	report.Plugins = n
	if b, jerr := json.Marshal(report); jerr == nil {
		_ = os.WriteFile(filepath.Join(filepath.Dir(integDir), "sync_report.json"), b, 0644)
	}
	return n, nil
}

func syncIntegrations(s *store.DB, integDir string, report *SyncReport) (int, error) {
	entries, err := os.ReadDir(integDir)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", integDir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		base := filepath.Join(integDir, name)
		// 至少要有 metrics 或 dashboards 之一才认为是个有效插件包
		metricsDir := filepath.Join(base, "metrics")
		dashDir := filepath.Join(base, "dashboards")
		hasMetrics := dirHasFiles(metricsDir, ".json")
		hasDashes := dirHasFiles(dashDir, ".json")
		if !hasMetrics && !hasDashes {
			continue
		}

		display := name
		if name == "HTTP_Response" {
			display = "HTTP 探测"
		}
		cat := categoryMap[name]
		if cat == "" {
			cat = "其他"
		}
		ptype := typeMap[name]
		if ptype == "" {
			ptype = "exporter"
		}
		p := &store.Plugin{
			Name: name, DisplayName: display, Category: cat, Type: ptype,
			Version: "v1.0.0", Source: "builtin",
		}
		// 插件不存在时才写采集说明模板；已存在的保留用户编辑
		if _, err := s.UpsertPlugin(p); err != nil {
			return n, fmt.Errorf("upsert plugin %s: %w", name, err)
		}
		pl, err := s.GetPluginByName(name)
		if err != nil {
			return n, err
		}
		pid := pl.ID
		// 图标：插件包内 icon/ 目录下的图片映射到静态服务路径；已设置的（含用户上传）不覆盖
		if pl.Icon == "" {
			if icon := pluginIconPath(base); icon != "" {
				if err := s.UpdatePluginIcon(pid, icon); err != nil {
					return n, fmt.Errorf("update icon %s: %w", name, err)
				}
			}
		}
		if pl.CollectDocMD == "" {
			s.UpdatePluginDoc(pid, exporterDocTemplate(name, display))
		}

		// 指标：清空 builtin 后重插（保留 custom）；同时计算与库中已有指标集的差异
		var ms []*store.Metric
		if hasMetrics {
			files, _ := os.ReadDir(metricsDir)
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(metricsDir, f.Name()))
				if err != nil {
					continue
				}
				var list []n9eMetric
				if err := json.Unmarshal(data, &list); err != nil {
					continue
				}
			for _, m := range list {
				mm := &store.Metric{
					PluginID: pid, PluginName: name, Name: m.Name, Unit: m.Unit, Note: m.Note,
					Expression: m.Expression, MetricType: m.MetricType, Source: "builtin",
				}
				// 内置采集能力包标注渠道/归属/溯源，治理视图渠道维度可筛选
				if ch := channelMap[name]; ch != "" {
					mm.Channel, mm.Ownership, mm.SourceRef = ch, "preset", name
				}
				ms = append(ms, mm)
			}
			}
		}
		// 变更对比：库中该插件现有指标名 vs 本次包内指标名
		oldNames := map[string]bool{}
		if olds, err := s.ListPluginMetricNames(pid); err == nil {
			for _, on := range olds {
				oldNames[on] = true
			}
		}
		newNames := map[string]bool{}
		for _, m := range ms {
			newNames[m.Name] = true
		}
		diff := PluginDiff{Plugin: name}
		for _, m := range ms {
			if !oldNames[m.Name] {
				diff.Added = append(diff.Added, m.Name)
			}
		}
		for on := range oldNames {
			if !newNames[on] {
				diff.Removed = append(diff.Removed, on)
			}
		}
		if len(diff.Added) > 0 || len(diff.Removed) > 0 {
			report.Diffs = append(report.Diffs, diff)
		}
		if err := s.ReplacePluginBuiltinMetrics(pid, ms); err != nil {
			return n, fmt.Errorf("replace metrics %s: %w", name, err)
		}

		// 仪表盘：清空后重插（清洗变量）
		var ds []*store.Dashboard
		if hasDashes {
			files, _ := os.ReadDir(dashDir)
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dashDir, f.Name()))
				if err != nil {
					continue
				}
				var raw map[string]interface{}
				if err := json.Unmarshal(data, &raw); err != nil {
					continue
				}
				// 夜莺部分 dashboard 的 configs 是双重 JSON 编码（字符串内嵌 JSON），先解开再清洗
				if cs, ok := raw["configs"].(string); ok {
					var inner map[string]interface{}
					if json.Unmarshal([]byte(cs), &inner) == nil {
						raw["configs"] = inner
					}
				}
				delete(raw, "ident")
				cleaned := cleanConfigs(raw)
				b, err := json.Marshal(cleaned)
				if err != nil {
					continue
				}
				dname, _ := raw["name"].(string)
				if dname == "" {
					dname = strings.TrimSuffix(f.Name(), ".json")
				}
				// 名称同步清洗：本平台采集层为标准 exporter，不带 categraf 字样
				dname = strings.ReplaceAll(dname, " by categraf", "")
				dname = strings.ReplaceAll(dname, "by categraf, ", "")
				dname = strings.ReplaceAll(dname, "Categraf", "Exporter")
				dname = strings.ReplaceAll(dname, "categraf", "exporter")
				ds = append(ds, &store.Dashboard{PluginID: pid, Name: dname, ConfigsJSON: string(b)})
			}
		}
		if err := s.ReplacePluginDashboards(pid, ds); err != nil {
			return n, fmt.Errorf("replace dashboards %s: %w", name, err)
		}
		n++
	}
	return n, nil
}

// pluginIconPath 返回插件包内图标的静态服务路径（经 /uploads/integrations/ 提供），无图标返回空串
func pluginIconPath(base string) string {
	files, err := os.ReadDir(filepath.Join(base, "icon"))
	if err != nil {
		return ""
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(f.Name())) {
		case ".png", ".svg", ".jpg", ".jpeg", ".webp", ".gif":
			return "/uploads/integrations/" + filepath.Base(base) + "/icon/" + f.Name()
		}
	}
	return ""
}

func dirHasFiles(dir, suffix string) bool {
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), suffix) {
			return true
		}
	}
	return false
}

// SyncMetricsFile 把旧指标中心数据（data/metrics.json）一次性导入 SQLite 统一存储。
// 仅插入缺失的指标（同名跳过），用户在指标中心的编辑不回写该文件、不被覆盖。
// 返回新插入条数。
func SyncMetricsFile(s *store.DB, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 无种子文件视为正常（全新环境）
		}
		return 0, err
	}
	// seedMetricsFile 种子文件结构：store.Metric 之外的 desc 字段是中文解释口径
	type seedMetric struct {
		store.Metric
		Desc string `json:"desc"`
	}
	var list []seedMetric
	if err := json.Unmarshal(data, &list); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	// 声明集清理：种子文件携带插件完整声明集时，清掉库中同名插件下不在声明集里的旧指标
	// （host_metrics 命名口径切换：host_* 自造名 → node_*/host_node_* KPI 标准名）
	replaceSet := map[string]bool{"host_metrics": true}
	declared := map[string][]string{}
	n := 0
	for i := range list {
		if list[i].Metric.Plugin != "" {
			declared[list[i].Metric.Plugin] = append(declared[list[i].Metric.Plugin], list[i].Metric.Name)
		}
	}
	for pluginName, names := range declared {
		if replaceSet[pluginName] {
			if removed, err := s.ReplacePluginMetrics(pluginName, names); err != nil {
				return n, fmt.Errorf("replace metrics %s: %w", pluginName, err)
			} else if removed > 0 {
				fmt.Printf("[importer] %s: removed %d obsolete metrics (declared set migration)\n", pluginName, removed)
			}
		}
	}
	for i := range list {
		m := &list[i].Metric
		if m.Name == "" {
			continue
		}
		// metrics.json 的归属字段叫 plugin（如 mysql_probe），映射到统一存储的 plugin_name；
		// desc（中文说明口径）落到统一存储的 note 列
		m.PluginName = m.Plugin
		m.Plugin = ""
		m.Note = list[i].Desc
		m.PluginID = s.ResolvePluginID(m.PluginName)
		if err := s.SeedMetric(m); err != nil {
			return n, fmt.Errorf("seed metric %s: %w", m.Name, err)
		}
		// 早期已入库但说明为空的指标，补齐中文说明（不覆盖用户编辑）
		if err := s.BackfillMetricNote(m.Name, list[i].Desc); err != nil {
			return n, fmt.Errorf("backfill note %s: %w", m.Name, err)
		}
		n++
	}
	return n, nil
}
