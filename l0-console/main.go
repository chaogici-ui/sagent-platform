package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sagent/l0-console/importer"
	storepkg "github.com/sagent/l0-console/store"
)

// AuditEntry 操作审计记录
type AuditEntry struct {
	Time     string `json:"time"`
	Operator string `json:"operator"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Scope    string `json:"scope"`
	Result   string `json:"result"`
}

var auditLog = make([]AuditEntry, 0)
var auditMu sync.Mutex

// auditDB 审计落库用的 catalog DB 句柄（main 里 Open 后赋值；为 nil 时退化为纯内存）
var auditDB *storepkg.DB

func addAudit(action, target, scope, result string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	auditMu.Lock()
	auditLog = append(auditLog, AuditEntry{
		Time:     ts,
		Operator: "admin",
		Action:   action,
		Target:   target,
		Scope:    scope,
		Result:   result,
	})
	if len(auditLog) > 1000 {
		auditLog = auditLog[len(auditLog)-1000:]
	}
	auditMu.Unlock()
	// M2-⑪ 审计落库：best effort，失败只打日志不阻断业务
	if auditDB != nil {
		if err := auditDB.InsertAudit(ts, cfgAuditOperator, action, target, scope, result); err != nil {
			log.Printf("audit persist: %v", err)
		}
	}
}

// vmBase 返回 VictoriaMetrics 基地址（容器部署用 VM_URL 覆盖）
func vmBase() string {
	if v := os.Getenv("VM_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8428"
}

// ==================== 运行时配置（生产化红线：业务代码禁止写死环境地址/演示数据） ====================
// 所有环境相关值一律走环境变量，默认值仅服务于本地开发。新配置项必须登记在此。
var (
	cfgListenAddr        string // HTTP 监听地址          LISTEN_ADDR（默认 :8080）
	cfgAgentHTTPPort     string // Agent 容器内 HTTP 端口  AGENT_HTTP_PORT（默认 19090）
	cfgAgentContainerPre string // Agent 容器名前缀        AGENT_CONTAINER_PREFIX（默认 l1-）
	cfgL0Net             string // L0 docker 网络名        L0_NET（默认 docker_l0-net）
	cfgGrafanaPublicURL  string // 浏览器可达的 Grafana 地址 GRAFANA_PUBLIC_URL
	cfgCORSOrigin        string // 允许的 CORS 源           CORS_ORIGIN（空=回显请求 Origin，即同源部署）
	cfgSeedDemoAgents    bool   // 是否注入演示 Agent 种子   SEED_DEMO_AGENTS=1（生产部署必须留空）
	cfgAuditOperator     string // 审计记录操作者标识        AUDIT_OPERATOR（默认 admin，生产建议改为真实账号体系标识）
)

func init() {
	cfgListenAddr = envOr("LISTEN_ADDR", ":8080")
	cfgAgentHTTPPort = envOr("AGENT_HTTP_PORT", "19090")
	cfgAgentContainerPre = envOr("AGENT_CONTAINER_PREFIX", "l1-")
	cfgL0Net = envOr("L0_NET", "docker_l0-net")
	cfgGrafanaPublicURL = envOr("GRAFANA_PUBLIC_URL", "")
	cfgCORSOrigin = os.Getenv("CORS_ORIGIN")
	cfgSeedDemoAgents = os.Getenv("SEED_DEMO_AGENTS") == "1"
	cfgAuditOperator = envOr("AUDIT_OPERATOR", "admin")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// agentContainerName Agent 的本机演示容器名（前缀可配，生产集群形态下不依赖 docker 探测）
func agentContainerName(agentID string) string {
	return cfgAgentContainerPre + agentID
}

func main() {
	onboardCfg := loadOnboardConfig("data/onboard_config.json")
	store := NewAgentStore()
	if cfgSeedDemoAgents {
		seedDemoAgentsInMemory(store)
		fmt.Println("Demo agents seeded (SEED_DEMO_AGENTS=1)")
	}

	// 插件能力目录库（SQLite）：打开 + 夜莺集成包增量同步
	catDB, err := storepkg.Open("data/catalog.db")
	if err != nil {
		log.Fatalf("open catalog db: %v", err)
	}
	defer catDB.Close()
	auditDB = catDB // M2-⑪ 审计落库句柄
	if n, err := importer.Sync(catDB, "data/integrations"); err != nil {
		log.Printf("integrations sync warning: %v", err)
	} else {
		fmt.Printf("Catalog synced from integrations: %d plugins\n", n)
	}
	if n, err := importer.SyncMetricsFile(catDB, "data/metrics.json"); err != nil {
		log.Printf("metrics seed sync warning: %v", err)
	} else if n > 0 {
		fmt.Printf("Metrics seeded from metrics.json: %d rows\n", n)
	}
	// 系统内置插件行引导：host_metrics/docker_metrics/custom_scripts 的指标走
	// metrics.json 种子（携带治理字段），但插件行不在 integrations 包内，须在此补齐；
	// 仅在缺失时创建，不覆盖既有行（用户可能编辑过元数据）
	ensureSystemPlugins(catDB)
	// M0：fleet 三表（agents/targets/agent_config）+ 演示 Agent 种子 + 内存 hydrate
	if err := catDB.InitFleet(); err != nil {
		log.Fatalf("init fleet tables: %v", err)
	}
	seedBuiltinAgents(store, catDB) // 内部仅处理内存已有的 Agent，空注册表时为 no-op
	hydrateAgents(store, catDB)
	// 为尚无版本化配置的 Agent 引导出结构化期望配置（targets 展开 + host_metrics 默认段）
	for _, a := range store.List() {
		if ver, _, _ := catDB.GetAgentConfig(a.ID); ver == 0 {
			syncAgentConfig(store, catDB, a.ID)
		}
	}

	mux := http.NewServeMux()

	// 插件能力目录 API（SQLite 配置库驱动）
	registerCatalogRoutes(mux, catDB)

	// CORS middleware
	handler := corsMiddleware(mux)

	// Agent 生命周期 / 指标中心 / 杂项 API 分域注册
	registerAgentRoutes(mux, store, catDB)
	registerMetricRoutes(mux)
	registerMiscRoutes(mux, onboardCfg)

	// 静态文件（强制正确的 Content-Type）
	fs := http.FileServer(http.Dir("static"))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 根据扩展名设置 MIME
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else if strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		fs.ServeHTTP(w, r)
	}))

	fmt.Println("L0 Console starting on " + cfgListenAddr)
	log.Fatal(http.ListenAndServe(cfgListenAddr, handler))
}

func ensureSystemPlugins(catDB *storepkg.DB) {
	sys := []storepkg.Plugin{
		{Name: "host_metrics", DisplayName: "主机指标", Category: "主机", Type: "builtin", Version: "v1.0.0", Source: "builtin"},
		{Name: "docker_metrics", DisplayName: "Docker 指标", Category: "Docker", Type: "builtin", Version: "v1.0.0", Source: "builtin"},
		{Name: "custom_scripts", DisplayName: "自定义脚本", Category: "脚本", Type: "script", Version: "v1.0.0", Source: "builtin"},
	}
	for _, p := range sys {
		if _, err := catDB.GetPluginByName(p.Name); err == nil {
			continue
		}
		pv := p
		if _, err := catDB.UpsertPlugin(&pv); err != nil {
			log.Printf("ensure system plugin %s: %v", p.Name, err)
		} else {
			fmt.Printf("[bootstrap] system plugin ensured: %s\n", p.Name)
		}
	}
}

// seedBuiltinAgents 首次启动把 3 个演示 Agent 写入 SQLite（source=docker）
func seedBuiltinAgents(store *AgentStore, catDB *storepkg.DB) {
	for _, a := range store.List() {
		src, _ := catDB.GetAgentSource(a.ID)
		if src != "" {
			continue
		}
		_ = catDB.UpsertAgentRow(&storepkg.AgentRow{
			ID: a.ID, Name: a.Name, Type: a.Type, Host: a.Host, Port: a.Port,
			Plugins: a.Plugins, Version: a.Version, Labels: a.Labels,
			Source: "docker", LastSeen: time.Now().Unix(),
		})
	}
}

// hydrateAgents 启动时把 SQLite 里的 Agent 全部装回内存
func hydrateAgents(store *AgentStore, catDB *storepkg.DB) {
	rows, err := catDB.ListAgentRows()
	if err != nil {
		log.Printf("hydrate agents: %v", err)
		return
	}
	for _, r := range rows {
		store.Put(rowToAgent(r))
	}
	fmt.Printf("Agents hydrated: %d (from sqlite)\n", len(rows))
}
func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS_ORIGIN 显式配置优先；未配置时回显请求 Origin（默认同源部署形态，
		// 前端与 API 同源时浏览器不发预检，此回显仅为兼容前端独立部署的调试场景）
		origin := cfgCORSOrigin
		if origin == "" {
			origin = r.Header.Get("Origin")
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeTargets 保证 targets 段为数组（legacy 数组内容 / 解析失败时兜底空数组）
