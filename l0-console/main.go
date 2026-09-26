package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
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
		Operator: cfgAuditOperator,
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
	cfgSAVersion         string // SAgent 镜像版本          SA_VERSION（默认 0.4.0；镜像 tag 由实测 OS/架构 + 本值拼出）
	cfgOnboardStallSec   int    // 接入流水线停滞判定阈值（秒）  ONBOARD_STALL_SEC（默认 300）
	cfgAgentHomeRoot     string // 目标机 Agent 安装根目录   AGENT_HOME_ROOT（默认 /home；实际安装到 <root>/<ssh_user>/SAgent）
	cfgAgentConfDir      string // 本机演示 Agent 配置文件目录 AGENT_CONF_DIR（默认 ../deploy/docker/configs/sagent）
	cfgTunnelRemotePort  int    // 反向隧道目标机侧端口      TUNNEL_REMOTE_PORT（默认 18080）
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
	cfgSAVersion = envOr("SA_VERSION", "0.4.0")
	cfgOnboardStallSec = envIntOr("ONBOARD_STALL_SEC", 300)
	cfgAgentHomeRoot = envOr("AGENT_HOME_ROOT", "/home")
	cfgAgentConfDir = envOr("AGENT_CONF_DIR", "../deploy/docker/configs/sagent")
	cfgTunnelRemotePort = envIntOr("TUNNEL_REMOTE_PORT", 18080)
}

// envIntOr 读整数环境变量，缺失或非法时用默认值
func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openCatalogDB 打开目录库（架构 D1：唯一载体 PostgreSQL）。
// CATALOG_DSN 必填 —— 目录库已全面切 PG，不保留 SQLite 回退：
// 缺配置就快速失败，避免「悄悄落回本地文件库」这类最难排查的部署事故。
func openCatalogDB() (*storepkg.DB, error) {
	dsn := os.Getenv("CATALOG_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("CATALOG_DSN 未配置：目录库唯一载体为 PostgreSQL（示例 postgres://user:pass@host:5432/db?sslmode=disable）")
	}
	fmt.Println("Catalog DB: PostgreSQL")
	return storepkg.OpenPostgres(dsn)
}

// agentContainerName Agent 的本机演示容器名（前缀可配，生产集群形态下不依赖 docker 探测）
func agentContainerName(agentID string) string {
	return cfgAgentContainerPre + agentID
}

// agentHTTPPortNum Agent HTTP 端口的整数形式（演示种子/端口探测等需要 int 的场景）
func agentHTTPPortNum() int {
	if n, err := strconv.Atoi(cfgAgentHTTPPort); err == nil && n > 0 {
		return n
	}
	return 19090
}

// platformPort 平台自身 HTTP 端口：从 LISTEN_ADDR 解析（反向隧道远端要转发到这里）
func platformPort() int {
	addr := cfgListenAddr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if n, err := strconv.Atoi(addr[i+1:]); err == nil && n > 0 {
			return n
		}
	}
	return 8080
}

// destHomeFor 目标机 Agent 安装根目录：<AGENT_HOME_ROOT>/<ssh_user>。
// root 等自定义家目录场景改 AGENT_HOME_ROOT 即可，不再写死 /home
func destHomeFor(cred *sshCred) string {
	root := strings.TrimRight(cfgAgentHomeRoot, "/")
	if root == "" {
		root = "/home"
	}
	user := ""
	if cred != nil {
		user = cred.User
	}
	return root + "/" + user
}

func main() {
	// 独立 L1 Package Cache 进程（架构 D5 IN1）：`l0-console -package-cache`。
	// 不建 DB、不起 console，仅加载版本清单(文件口径) + 起 package-cache HTTP 服务阻塞。
	pkgCacheDaemon := flag.Bool("package-cache", false, "run as standalone L1 Package Cache daemon")
	// 独立 L1 Gateway 控制通道中继进程（架构 D5 IN2）：`l0-console -gateway`。
	// 不建 DB、不起 console，仅起控制通道汇聚中继（注册/心跳/接入证据转发 L0）阻塞。
	gatewayDaemon := flag.Bool("gateway", false, "run as standalone L1 Gateway relay daemon")
	// 独立 L1 Controller 任务拉取/分流/回执进程（架构 D5 IN3）：`l0-console -controller`。
	// 不建 DB、不起 console，仅反向连接 L0 拉任务 → 分流决策 → 回执 循环阻塞。
	controllerDaemon := flag.Bool("controller", false, "run as standalone L1 Controller task-pull/ack daemon")
	// 独立 L1 Ansible Runner 执行进程（架构 D5 IN4）：`l0-console -ansible-runner`。
	// 不建 DB、不起 console，仅反向连接 L0 拉安装任务 → 本地执行 playbook → 回执终态。
	ansibleRunnerDaemon := flag.Bool("ansible-runner", false, "run as standalone L1 Ansible Runner install executor")
	flag.Parse()
	if *pkgCacheDaemon {
		loadVersionCatalog("data/versions.yaml", nil)
		runPackageCacheDaemon()
		return
	}
	if *gatewayDaemon {
		runGatewayDaemon()
		return
	}
	if *controllerDaemon {
		runControllerDaemon()
		return
	}
	if *ansibleRunnerDaemon {
		runAnsibleRunnerDaemon()
		return
	}
	// 滚动日志基础设施：data/logs/app.log，10MB 滚动 + gzip 归档 + 保留最近 10 份 + 异步，
	// 业务 log.Printf 调用点零改动（见 logging.go 头部说明）。输出不变更可见，仅落盘与归档。
	logCleanup := initLogging()
	defer logCleanup()
	onboardCfg = loadOnboardConfig("data/onboard_config.json")
	store := NewAgentStore()
	if cfgSeedDemoAgents {
		seedDemoAgentsInMemory(store)
		fmt.Println("Demo agents seeded (SEED_DEMO_AGENTS=1)")
	}

	// 插件能力目录库（架构 D1：唯一载体 PostgreSQL）：
	// CATALOG_DSN 必填（postgres://user:pass@host:5432/db），缺失即启动失败。
	catDB, err := openCatalogDB()
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
	// 接入中心三表（resources/onboard_flow/onboard_event）：数据地基，先于任何接入动作建好
	if err := catDB.InitOnboard(); err != nil {
		log.Fatalf("init onboard tables: %v", err)
	}
	// L1 任务源表（l1_tasks）：任务投递/拉取/回执持久化真相源
	if err := catDB.InitL1Tasks(); err != nil {
		log.Fatalf("init l1 tasks table: %v", err)
	}
	// HA-3 跨 L1 归属迁移记录表（relocations）：迁移证据 + 冷却判定来源
	if err := catDB.InitRelocations(); err != nil {
		log.Fatalf("init relocations table: %v", err)
	}
	// 流程模板（模式模板，L2 层）：编排数据化，引擎只认原子能力
	if tpls, err := loadFlowTemplates(defaultFlowTemplateDir); err != nil {
		log.Printf("flow templates load warning (%s): %v", defaultFlowTemplateDir, err)
	} else {
		fmt.Printf("Flow templates loaded: %d (%s)\n", len(tpls), defaultFlowTemplateDir)
	}
	// SAgent 版本清单：data/versions.yaml 为种子，catalog 库为权威源（界面可编辑兼容矩阵）
	loadVersionCatalog("data/versions.yaml", catDB)
	seedBuiltinAgents(store, catDB) // 内部仅处理内存已有的 Agent，空注册表时为 no-op
	hydrateAgents(store, catDB)
	// 为尚无版本化配置的 Agent 引导出结构化期望配置（targets 展开 + host_metrics 默认段）
	for _, a := range store.List() {
		if ver, _, _ := catDB.GetAgentConfig(a.ID); ver == 0 {
			syncAgentConfig(store, catDB, a.ID)
		}
	}

	mux := http.NewServeMux()

	// 插件能力目录 API（catalog 库驱动）
	registerCatalogRoutes(mux, catDB)

	// CORS middleware
	handler := corsMiddleware(mux)

	// Agent 生命周期 / 指标中心 / 杂项 API 分域注册
	registerAgentRoutes(mux, store, catDB)
	registerMetricRoutes(mux)
	registerMiscRoutes(mux, onboardCfg)
	registerOnboardRoutes(mux, store, catDB)
	registerVersionRoutes(mux, catDB)
	registerPackageCacheRoutes(mux, catDB)
	registerCollectStatsRoutes(mux)          // 资源页采集实况（VM 侧，按 resource_id 标签）
	registerSelfMonRoutes(mux, store, catDB) // 平台自监控（可运维·可观测）
	registerMetricsRoute(mux, store, catDB)  // 平台自身 Prometheus /metrics（供 L0 侧 VM 抓取）
	registerTenantRoutes(mux, catDB)         // 多租户管控（架构 D3 骨架：租户清单 + 新建）
	registerTenantTokenRoutes(mux, catDB)    // 租户 API Token 签发/吊销/状态（架构 D3 凭证鉴权）
	registerHealthRoute(mux, store, catDB)   // 平台健康自检汇总（架构 3.3 可运维·相与规划）
	registerAlertRoutes(mux, store, catDB)   // 告警中心（架构 D6/G5：查询式→主动告警，外发按 env）
	registerL1TaskRoutes(mux, catDB)         // L1 任务源（架构 D5 IN3：Controller 拉取/回执，持久化 pg 任务表）
	registerRelocateRoutes(mux, store, catDB) // HA-3 跨 L1 归属迁移（评审/执行/历史证据）
	registerCollectorRoutes(mux, store, catDB) // R2 采集机选机（池候选 + 承载数 + 自动挑机 + 人工改选）
	startRelocateAuto(store, catDB)           // HA-3 自动调度（RELOCATE_AUTO=1 启用，默认关）

	// 接入流水线超时清扫：running 步骤到点自动重试（有限次）或判死。
	// 由平台侧定时器驱动，不依赖"有人开着页面" —— 这是「不能无底线等待」的保证
	startTimeoutSweeper(catDB, store)
	startTunnelKeeper(catDB)

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
		{Name: "host_metrics", DisplayName: "主机指标", Category: "主机", Type: "builtin", Source: "builtin"},
		{Name: "docker_metrics", DisplayName: "Docker 指标", Category: "Docker", Type: "builtin", Source: "builtin"},
		{Name: "custom_scripts", DisplayName: "自定义脚本", Category: "脚本", Type: "script", Source: "builtin"},
	}
	// 平台自有的三个插件每次启动都对齐定义：upsert 只覆盖元数据（说明文档等用户编辑保留），
	// 这样历史遗留的编造版本号（旧移植器统一写死 v1.0.0）也能被清掉
	for _, p := range sys {
		pv := p
		if _, err := catDB.UpsertPlugin(&pv); err != nil {
			log.Printf("ensure system plugin %s: %v", p.Name, err)
		} else {
			fmt.Printf("[bootstrap] system plugin ensured: %s\n", p.Name)
		}
	}
}

// seedBuiltinAgents 首次启动把 3 个演示 Agent 写入目录库（source=docker）
func seedBuiltinAgents(store *AgentStore, catDB *storepkg.DB) {
	for _, a := range store.List() {
		src, _ := catDB.GetAgentSource(a.ID)
		if src != "" {
			continue
		}
		_ = catDB.UpsertAgentRow(&storepkg.AgentRow{
			ID: a.ID, Name: a.Name, Type: a.Type, Host: a.Host, Port: a.Port,
			Plugins: a.Plugins, Version: a.Version, Labels: a.Labels,
			Source: "docker", LastSeen: time.Now().Unix(), TenantID: a.TenantID,
		})
	}
}

// hydrateAgents 启动时把目录库里的 Agent 全部装回内存
func hydrateAgents(store *AgentStore, catDB *storepkg.DB) {
	rows, err := catDB.ListAgentRows()
	if err != nil {
		log.Printf("hydrate agents: %v", err)
		return
	}
	for _, r := range rows {
		store.Put(rowToAgent(r))
	}
	fmt.Printf("Agents hydrated: %d (from catalog)\n", len(rows))
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
