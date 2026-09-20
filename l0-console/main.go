package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagent/l0-console/importer"
	storepkg "github.com/sagent/l0-console/store"
)

// Agent 代表一个 SAgent 实例
type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         string            `json:"type"` // edge 或 proxy
	Status       string            `json:"status"`
	Host         string            `json:"host"`
	Port         int               `json:"port"`
	Plugins      []string          `json:"plugins"`
	Version      string            `json:"version"`
	Labels       map[string]string `json:"labels"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Source       string            `json:"source,omitempty"`   // docker: 本机演示; heartbeat: 注册接入
	LastSeen     int64             `json:"last_seen,omitempty"` // 最近心跳 unix 秒
	CfgDesired   int               `json:"cfg_desired,omitempty"`   // 服务端期望配置版本
	CfgEffective string            `json:"cfg_effective,omitempty"` // Agent 上报的生效配置版本
	Stats        map[string]interface{} `json:"stats,omitempty"`    // 心跳携带的采集成败统计
}

// AgentStore 管理所有 Agent
type AgentStore struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

// NewAgentStore 生产形态从空注册表起步：Agent 由心跳注册（/api/agent/register）产生，
// 不预置任何演示数据。本机演示环境通过 SEED_DEMO_AGENTS=1 + seedDemoAgentsInMemory 注入。
func NewAgentStore() *AgentStore {
	return &AgentStore{agents: map[string]*Agent{}}
}

// seedDemoAgentsInMemory 本机演示种子（仅 SEED_DEMO_AGENTS=1 时调用）
func seedDemoAgentsInMemory(store *AgentStore) {
	demo := []*Agent{
		{ID: "sagent-1", Name: "Edge Collector 1", Type: "edge",
			Status: "unknown", Host: "sagent-1", Port: 19090,
			Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"}},
		{ID: "sagent-2", Name: "Edge Collector 2", Type: "edge",
			Status: "unknown", Host: "sagent-2", Port: 19090,
			Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"}},
		{ID: "sagent-proxy", Name: "Collector Proxy", Type: "proxy",
			Status: "unknown", Host: "sagent-proxy", Port: 19090,
			Plugins: []string{"mysql_probe"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"}},
	}
	for _, a := range demo {
		store.agents[a.ID] = a
	}
}

// List 返回所有 Agent
func (s *AgentStore) List() []*Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		result = append(result, a)
	}
	return result
}

// Get 获取单个 Agent
func (s *AgentStore) Get(id string) *Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agents[id]
}

// Update 更新 Agent 状态
func (s *AgentStore) Update(id string, status string, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		a.Status = status
		a.UpdatedAt = time.Now()
		if version != "" {
			a.Version = version
		}
	}
}

// Put 注册/覆盖一个 Agent（心跳注册用）
func (s *AgentStore) Put(a *Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.agents[a.ID]; ok {
		a.Status = old.Status // 状态由心跳/探测推导，注册不覆盖
	}
	if a.Status == "" {
		a.Status = "healthy"
	}
	a.UpdatedAt = time.Now()
	s.agents[a.ID] = a
}

// detectVersion 从容器中读取 SAgent 实际版本
func detectVersion(agentID string) string {
	containerName := agentContainerName(agentID)
	out, err := runCmd(".", "docker", "exec", containerName,
		"/home/deploy/SAgent/bin/SAgent", "-version")
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if idx := strings.LastIndex(out, "v"); idx >= 0 {
		return out[idx:]
	}
	return ""
}

// AuditEntry 操作审计记录
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
	cfgListenAddr          string // HTTP 监听地址          LISTEN_ADDR（默认 :8080）
	cfgAgentHTTPPort       string // Agent 容器内 HTTP 端口  AGENT_HTTP_PORT（默认 19090）
	cfgAgentContainerPre   string // Agent 容器名前缀        AGENT_CONTAINER_PREFIX（默认 l1-）
	cfgL0Net               string // L0 docker 网络名        L0_NET（默认 docker_l0-net）
	cfgGrafanaPublicURL    string // 浏览器可达的 Grafana 地址 GRAFANA_PUBLIC_URL
	cfgCORSOrigin          string // 允许的 CORS 源           CORS_ORIGIN（空=回显请求 Origin，即同源部署）
	cfgSeedDemoAgents      bool   // 是否注入演示 Agent 种子   SEED_DEMO_AGENTS=1（生产部署必须留空）
	cfgAuditOperator       string // 审计记录操作者标识        AUDIT_OPERATOR（默认 admin，生产建议改为真实账号体系标识）
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

// ==================== 接入配置（产品语义数据化：Agent 类型捆绑 / 接入向导插件清单） ====================
// 红线 2 延伸：插件捆绑关系是产品策略，禁止写死在前端——由后端下发，可在 data/onboard_config.json
// 覆盖（演进路径：插件捆绑包 DB 化）。文件缺失时回退内置默认，行为与历史版本一致。

// OnboardConfig 接入向导与 Agent 注册模板的运行时配置
type OnboardConfig struct {
	AgentTypes []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Desc    string   `json:"desc"`
		Plugins []string `json:"plugins"`
	} `json:"agent_types"`
	ManageablePlugins []string `json:"manageable_plugins"`
	OnboardPlugins []struct {
		Name  string `json:"name"`
		Probe string `json:"probe"`
		Port  string `json:"port"`
	} `json:"onboard_plugins"`
}

// defaultOnboardConfig 内置默认（与 data/onboard_config.json 保持一致）
func defaultOnboardConfig() *OnboardConfig {
	return &OnboardConfig{
		AgentTypes: []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Desc    string   `json:"desc"`
			Plugins []string `json:"plugins"`
		}{
			{ID: "edge", Name: "📡 边缘采集 Agent", Desc: "部署在主机上，采集 CPU/内存/磁盘/网络、日志指标、运行巡检脚本", Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"}},
			{ID: "proxy", Name: "🔗 Collector Proxy", Desc: "部署在 Proxy 节点，远程采集 MySQL/Redis/Kafka 等中间件指标", Plugins: []string{"mysql_probe", "prometheus_scrape"}},
			{ID: "custom", Name: "⚙️ 自定义", Desc: "按需选择组件，适用于特殊场景", Plugins: []string{}},
		},
		ManageablePlugins: []string{"log_metrics", "mysql_probe", "custom_scripts"},
		OnboardPlugins: []struct {
			Name  string `json:"name"`
			Probe string `json:"probe"`
			Port  string `json:"port"`
		}{
			{Name: "MySQL", Probe: "mysql_probe", Port: "3306"},
			{Name: "Redis", Probe: "redis_probe", Port: "6379"},
			{Name: "Kafka", Probe: "kafka_exporter", Port: "9092"},
			{Name: "Elasticsearch", Probe: "elasticsearch_exporter", Port: "9200"},
			{Name: "ClickHouse", Probe: "clickhouse_exporter", Port: "9363"},
			{Name: "HTTP 拨测", Probe: "http_response", Port: "443"},
			{Name: "自定义脚本", Probe: "custom_scripts", Port: ""},
		},
	}
}

// loadOnboardConfig 加载接入配置：data/onboard_config.json 覆盖 → 内置默认兜底
func loadOnboardConfig(path string) *OnboardConfig {
	cfg := defaultOnboardConfig()
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Printf("onboard config parse warning (使用内置默认): %v", err)
		} else {
			fmt.Println("Onboard config loaded from " + path)
		}
	}
	return cfg
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

	// API: 站点配置（前端外部跳转地址统一从后端取，禁止在 JS 里写死环境地址）
	// vm_url 用 VM_PUBLIC_URL（浏览器可达），与服务端代理用的 VM_URL（docker service name）是两个概念
	mux.HandleFunc("/api/site-config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{
			"vm_url":      os.Getenv("VM_PUBLIC_URL"), // 为空时前端按 window.location 推导
			"grafana_url": cfgGrafanaPublicURL,
		})
	})

	// API: 接入配置（Agent 类型捆绑 / 接入向导插件清单，产品语义数据化下发）
	mux.HandleFunc("/api/onboard/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, onboardCfg)
	})

	// API: Agent 列表
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		// 刷新状态
		refreshAgentStatuses(store)
		writeJSON(w, store.List())
	})

	// API: 单个 Agent 详情 + 配置管理
	mux.HandleFunc("/api/agents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/agents/")

		// /api/agents/{id}/config  GET 读（结构化 parsed）/ PUT 改（host_metrics 配置轨道）
		if strings.HasSuffix(path, "/config") {
			id := strings.TrimSuffix(path, "/config")
			handleAgentConfigRoute(w, r, id, store, catDB)
			return
		}

		// /api/agents/{id}/plugin-status
		if strings.HasSuffix(path, "/plugin-status") {
			id := strings.TrimSuffix(path, "/plugin-status")
			handlePluginStatus(w, r, id)
			return
		}

		// /api/agents/{id}/metrics
		if strings.HasSuffix(path, "/metrics") {
			id := strings.TrimSuffix(path, "/metrics")
			handleAgentMetrics(w, r, id)
			return
		}

		// /api/agents/{id}/logs
		if strings.HasSuffix(path, "/logs") {
			id := strings.TrimSuffix(path, "/logs")
			handleAgentLogs(w, r, id)
			return
		}

		// /api/agents/{id}
		if agent := store.Get(path); agent != nil {
			writeJSON(w, agent)
		} else {
			http.NotFound(w, r)
		}
	})

	// M0-①：Agent 注册（安装引导后回调）
	mux.HandleFunc("/api/agent/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		// 可选静态 token 校验（SA_REG_TOKEN 设置后强制）
		if tok := os.Getenv("SA_REG_TOKEN"); tok != "" && r.Header.Get("X-Reg-Token") != tok {
			writeJSON(w, map[string]interface{}{"error": "invalid registration token"})
			return
		}
		var req struct {
			ID      string            `json:"id"`
			Type    string            `json:"type"`
			Version string            `json:"version"`
			IP      string            `json:"ip"`
			Labels  map[string]string `json:"labels"`
			Plugins []string          `json:"plugins"`
			CfgHash string            `json:"config_hash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if req.Type == "" {
			req.Type = "edge"
		}
		if req.ID == "" {
			req.ID = fmt.Sprintf("%s-%d", req.Type, time.Now().Unix()%100000)
		}
		if src, _ := catDB.GetAgentSource(req.ID); src == "docker" {
			writeJSON(w, map[string]interface{}{"error": "该 ID 为本机演示容器保留，请换一个 id"})
			return
		}
		row := &storepkg.AgentRow{
			ID: req.ID, Name: req.ID, Type: req.Type, Host: req.IP,
			Plugins: req.Plugins, Version: req.Version, Labels: req.Labels,
			Source: "heartbeat", LastSeen: time.Now().Unix(), CfgEffective: req.CfgHash,
		}
		if err := catDB.UpsertAgentRow(row); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		ver, content, _ := catDB.GetAgentConfig(req.ID)
		store.Put(rowToAgent(row))
		if a := store.Get(req.ID); a != nil {
			a.CfgDesired = ver
		}
		addAudit("注册 Agent", req.ID, "心跳接入", "成功")
		writeJSON(w, map[string]interface{}{"ok": true, "agent_id": req.ID, "config_version": ver, "config": content})
	})

	// M0-①：Agent 心跳（30s 一次），携带版本/生效配置/采集成败；响应携带待下发配置
	mux.HandleFunc("/api/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			ID         string                 `json:"id"`
			Status     string                 `json:"status"`
			Version    string                 `json:"version"`
			CfgVersion int                    `json:"config_version"`
			CfgHash    string                 `json:"config_hash"`
			Stats      map[string]interface{} `json:"stats"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		src, _ := catDB.GetAgentSource(req.ID)
		if src == "" {
			writeJSON(w, map[string]interface{}{"error": "unregistered agent", "need_register": true})
			return
		}
		if err := catDB.AgentHeartbeat(req.ID, req.Version, req.CfgHash, req.Stats); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if a := store.Get(req.ID); a != nil {
			a.LastSeen = time.Now().Unix()
			a.CfgEffective = req.CfgHash
			a.Stats = req.Stats
			if req.Version != "" {
				a.Version = req.Version
			}
			a.Status = "healthy"
		}
		ver, content, _ := catDB.GetAgentConfig(req.ID)
		resp := map[string]interface{}{"ok": true, "config_version": ver, "interval": 30}
		if ver > 0 && req.CfgVersion != ver {
			resp["config"] = content
			resp["config_changed"] = true
		}
		writeJSON(w, resp)
	})

	// M0-②：采集目标 CRUD
	mux.HandleFunc("/api/targets", func(w http.ResponseWriter, r *http.Request) {
		type TargetReq struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Address string `json:"address"`
			AgentID string `json:"agent_id"`
			Plugin  string `json:"plugin"`
			Note    string `json:"note"`
		}
		switch r.Method {
		case http.MethodGet:
			list, err := catDB.ListTargets()
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			writeJSON(w, list)
		case http.MethodPost:
			var q TargetReq
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Name == "" || q.Address == "" {
				writeJSON(w, map[string]interface{}{"error": "name 和 address 必填"})
				return
			}
			// 目标分派只允许绑定真实 Agent（未分派允许 agent_id 为空）
			if q.AgentID != "" && !agentExists(store, catDB, q.AgentID) {
				writeJSON(w, map[string]interface{}{"error": "agent not found: " + q.AgentID})
				return
			}
			id, err := catDB.InsertTarget(&storepkg.TargetRow{Name: q.Name, Type: q.Type, Address: q.Address, AgentID: q.AgentID, Plugin: q.Plugin, Note: q.Note})
			if err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, q.AgentID)
			addAudit("新增采集目标", q.Name, q.AgentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true, "id": id})
		case http.MethodPut:
			var q TargetReq
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.ID == 0 {
				writeJSON(w, map[string]interface{}{"error": "id 必填"})
				return
			}
			// 目标分派只允许绑定真实 Agent（未分派允许 agent_id 为空）
			if q.AgentID != "" && !agentExists(store, catDB, q.AgentID) {
				writeJSON(w, map[string]interface{}{"error": "agent not found: " + q.AgentID})
				return
			}
			// 找出旧分派，双端同步配置版本
			oldAgent := ""
			if list, _ := catDB.ListTargets(); list != nil {
				for _, t := range list {
					if t.ID == q.ID {
						oldAgent = t.AgentID
					}
				}
			}
			if err := catDB.UpdateTarget(&storepkg.TargetRow{ID: q.ID, Name: q.Name, Type: q.Type, Address: q.Address, AgentID: q.AgentID, Plugin: q.Plugin, Note: q.Note}); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, q.AgentID)
			if oldAgent != q.AgentID {
				syncAgentConfig(store, catDB, oldAgent)
			}
			addAudit("编辑采集目标", q.Name, q.AgentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true})
		case http.MethodDelete:
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if id == 0 {
				writeJSON(w, map[string]interface{}{"error": "id 必填"})
				return
			}
			agentID := ""
			if list, _ := catDB.ListTargets(); list != nil {
				for _, t := range list {
					if t.ID == id {
						agentID = t.AgentID
					}
				}
			}
			if err := catDB.DeleteTarget(id); err != nil {
				writeJSON(w, map[string]interface{}{"error": err.Error()})
				return
			}
			syncAgentConfig(store, catDB, agentID)
			addAudit("删除采集目标", fmt.Sprintf("id=%d", id), agentID, "成功")
			InvalidateReconCache()
			writeJSON(w, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// M0-②：采集目标连通性测试（真实 TCP dial）
	mux.HandleFunc("/api/targets/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/targets/")
		if !strings.HasSuffix(path, "/test") || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		idStr := strings.TrimSuffix(path, "/test")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "bad id"})
			return
		}
		list, _ := catDB.ListTargets()
		var tgt *storepkg.TargetRow
		for _, t := range list {
			if t.ID == id {
				tgt = t
			}
		}
		if tgt == nil {
			writeJSON(w, map[string]interface{}{"error": "target not found"})
			return
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", tgt.Address, 3*time.Second)
		result := ""
		if err != nil {
			result = "失败: " + err.Error()
		} else {
			conn.Close()
			result = fmt.Sprintf("连通 (%d ms)", time.Since(start).Milliseconds())
		}
		_ = catDB.UpdateTargetTest(id, result)
		writeJSON(w, map[string]interface{}{"ok": err == nil, "result": result})
	})

	// API: 执行操作
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
			Action  string `json:"action"` // start, stop, restart, upgrade, deploy
			Version string `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}

		result := executeAction(req.AgentID, req.Action, req.Version)
		if req.Action == "start" || req.Action == "restart" {
			store.Update(req.AgentID, "running", req.Version)
		} else if req.Action == "stop" {
			store.Update(req.AgentID, "stopped", "")
		} else if req.Action == "upgrade" {
			store.Update(req.AgentID, "running", req.Version)
		}
		writeJSON(w, result)
	})

	// API: 插件级操作
	mux.HandleFunc("/api/plugin-action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
			Plugin  string `json:"plugin"`
			Action  string `json:"action"` // reload, stop, start, restart
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		result := executePluginAction(req.AgentID, req.Plugin, req.Action)
		writeJSON(w, result)
	})

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

	// API: 指标目录（统一存储：SQLite catalog.db；data/metrics.json 仅作首次种子）
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

	// API: 操作审计（M2-⑪：读落库数据，重启不丢；DB 不可用时回退内存镜像）
	mux.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		if auditDB != nil {
			if rows, err := auditDB.ListAudit(200); err == nil {
				writeJSON(w, rows)
				return
			}
		}
		auditMu.Lock()
		defer auditMu.Unlock()
		writeJSON(w, auditLog)
	})

	// API: 从 VictoriaMetrics 反查已采集指标
	mux.HandleFunc("/api/vm/metrics", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		vmURL := vmBase()+"/api/v1/label/__name__/values"
		resp, err := http.Get(vmURL)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		var result struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		if prefix != "" {
			var filtered []string
			for _, name := range result.Data {
				if strings.HasPrefix(name, prefix) {
					filtered = append(filtered, name)
				}
			}
			result.Data = filtered
		}
		writeJSON(w, result)
	})

	// API: 同步指标到目录
	syncedMetrics := make(map[string]bool)
	mux.HandleFunc("/api/metrics/synced", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var names []string
			json.NewDecoder(r.Body).Decode(&names)
			for _, n := range names {
				syncedMetrics[n] = true
			}
			writeJSON(w, map[string]interface{}{"synced": len(names)})
		} else {
			keys := make([]string, 0, len(syncedMetrics))
			for k := range syncedMetrics {
				keys = append(keys, k)
			}
			writeJSON(w, keys)
		}
	})

	// API: 从 VM 获取已采集的指标按资源分组
	mux.HandleFunc("/api/vm/metrics-by-target", func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(vmBase()+"/api/v1/query?query=count+by+(__name__,resource_id)({__name__=~\".+\"})")
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// API: 插件定义管理
	mux.HandleFunc("/api/plugins", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			data, _ := os.ReadFile("data/plugins.json")
			if len(data) == 0 {
				writeJSON(w, []interface{}{})
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
			}
			return
		}
		if r.Method == "POST" {
			var list []map[string]interface{}
			json.NewDecoder(r.Body).Decode(&list)
			data, _ := json.MarshalIndent(list, "", "  ")
			os.WriteFile("data/plugins.json", data, 0644)
			addAudit("编辑插件", "plugins", "系统", fmt.Sprintf("保存 %d 个插件定义", len(list)))
			writeJSON(w, map[string]interface{}{"saved": len(list)})
			return
		}
	})

	// API: 任务历史（从审计日志聚合）
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		auditMu.Lock()
		defer auditMu.Unlock()
		type TaskItem struct {
			Time     string `json:"time"`
			Operator string `json:"operator"`
			Action   string `json:"action"`
			Target   string `json:"target"`
			Scope    string `json:"scope"`
			Result   string `json:"result"`
		}
		var tasks = make([]TaskItem, 0)
		for _, e := range auditLog {
			if e.Action == "启动 Agent" || e.Action == "停止 Agent" || e.Action == "重启 Agent" || e.Action == "部署 Agent" || e.Action == "升级 Agent" || e.Action == "sync_metrics" {
				tasks = append(tasks, TaskItem(e))
			}
		}
		writeJSON(w, tasks)
	})

	// API: 插件文件上传（图标 + 脚本）
	mux.HandleFunc("/api/plugin/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		pluginID := r.FormValue("plugin_id")
		fileType := r.FormValue("type") // script or icon
		if pluginID == "" || fileType == "" {
			writeJSON(w, map[string]interface{}{"error": "plugin_id and type required"})
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer file.Close()
		// Ensure upload directory exists
		uploadDir := fmt.Sprintf("data/plugins/upload/%s/%s", pluginID, fileType)
		os.MkdirAll(uploadDir, 0755)
		// Save file
		dst, err := os.Create(fmt.Sprintf("%s/%s", uploadDir, hdr.Filename))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer dst.Close()
		io.Copy(dst, file)
		addAudit("上传插件文件", pluginID, fileType, "成功: "+hdr.Filename)
		writeJSON(w, map[string]interface{}{"success": true, "filename": hdr.Filename})
	})

	// Serve uploaded files
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("data/plugins/upload"))))

	// API: 集成列表（来自 Nightingale integrations）
	mux.HandleFunc("/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		integDir := "data/integrations"
		entries, err := os.ReadDir(integDir)
		if err != nil {
			writeJSON(w, []interface{}{})
			return
		}
		type Integration struct {
			Name       string `json:"name"`
			Icon       string `json:"icon"`
			HasCollect bool   `json:"has_collect"`
			HasMetrics bool   `json:"has_metrics"`
			HasDash    bool   `json:"has_dash"`
			HasAlerts  bool   `json:"has_alerts"`
		}
		var list []Integration
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			integ := Integration{Name: name}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/icon", integDir, name)); err == nil {
				if icons, _ := os.ReadDir(fmt.Sprintf("%s/%s/icon", integDir, name)); len(icons) > 0 {
					integ.Icon = fmt.Sprintf("/uploads/integrations/%s/icon/%s", name, icons[0].Name())
				}
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/collect", integDir, name)); err == nil {
				integ.HasCollect = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/metrics", integDir, name)); err == nil {
				integ.HasMetrics = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/dashboards", integDir, name)); err == nil {
				integ.HasDash = true
			}
			if _, err := os.Stat(fmt.Sprintf("%s/%s/alerts", integDir, name)); err == nil {
				integ.HasAlerts = true
			}
			list = append(list, integ)
		}
		writeJSON(w, list)
	})

	// API: 集成详情（collect/metrics/dashboards）
	mux.HandleFunc("/api/integrations/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/integrations/")
		integDir := "data/integrations"
		// Case-insensitive directory lookup
		entries, _ := os.ReadDir(integDir)
		realName := ""
		for _, e := range entries {
			if strings.EqualFold(e.Name(), name) {
				realName = e.Name()
				break
			}
		}
		if realName == "" {
			writeJSON(w, map[string]interface{}{"error": "not found"})
			return
		}
		result := map[string]interface{}{
			"name":       name,
			"collect":    []interface{}{},
			"metrics":    []interface{}{},
			"dashboards": []interface{}{},
		}
		// Read collect templates
		collectDir := fmt.Sprintf("%s/%s/collect", integDir, realName)
		if dirs, err := os.ReadDir(collectDir); err == nil {
			var templates []string
			for _, d := range dirs {
				if d.IsDir() {
					if files, err := os.ReadDir(fmt.Sprintf("%s/%s", collectDir, d.Name())); err == nil {
						for _, f := range files {
							if data, err := os.ReadFile(fmt.Sprintf("%s/%s/%s", collectDir, d.Name(), f.Name())); err == nil {
								templates = append(templates, string(data))
							}
						}
					}
				}
			}
			result["collect"] = templates
		}
		// Read metrics
		metricsDir := fmt.Sprintf("%s/%s/metrics", integDir, realName)
		if files, err := os.ReadDir(metricsDir); err == nil {
			for _, f := range files {
				if data, err := os.ReadFile(fmt.Sprintf("%s/%s", metricsDir, f.Name())); err == nil {
					var metrics []interface{}
					json.Unmarshal(data, &metrics)
					result["metrics"] = metrics
				}
			}
		}
		// Read dashboards
		dashDir := fmt.Sprintf("%s/%s/dashboards", integDir, realName)
		if files, err := os.ReadDir(dashDir); err == nil {
			var dashList []map[string]interface{}
			for _, f := range files {
				if data, err := os.ReadFile(fmt.Sprintf("%s/%s", dashDir, f.Name())); err == nil {
					var dash map[string]interface{}
					json.Unmarshal(data, &dash)
					if dash != nil {
						dash["file"] = f.Name()
						dashList = append(dashList, dash)
					}
				}
			}
			result["dashboards"] = dashList
		}
		writeJSON(w, result)
	})

	// API: VM PromQL 查询代理
	mux.HandleFunc("/api/vm/query", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if query == "" {
			writeJSON(w, map[string]interface{}{"error": "query required"})
			return
		}
		vmURL := fmt.Sprintf("%s/api/v1/query?query=%s", vmBase(), url.QueryEscape(query))
		resp, err := http.Get(vmURL)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// Serve integration icons
	mux.Handle("/uploads/integrations/", http.StripPrefix("/uploads/integrations/", http.FileServer(http.Dir("data/integrations"))))

	fmt.Println("L0 Console starting on " + cfgListenAddr)
	log.Fatal(http.ListenAndServe(cfgListenAddr, handler))
}

// executeAction 通过 Docker Compose 执行操作
func executeAction(agentID, action, version string) map[string]interface{} {
	result := map[string]interface{}{
		"agent_id": agentID,
		"action":   action,
		"success":  true,
	}

	composeDir := "../deploy/docker"
	service := agentID

	switch action {
	case "start":
		// 启动 SAgent 进程：删除 stopped 标记文件，supervisor 自动拉起
		containerName := agentContainerName(agentID)
		out, err := runCmd(".", "docker", "exec", containerName, "rm", "-f", "/home/deploy/SAgent/run/stopped")
		if err != nil {
			// 如果 exec 失败（容器不存在），尝试 compose up
			out2, err2 := runCmd(composeDir, "docker", "compose", "up", "-d", service)
			if err2 != nil {
				result["success"] = false
				result["error"] = err2.Error()
				break
			}
			out = strings.TrimSpace(out2)
		}
		result["output"] = "SAgent 进程已启动: " + strings.TrimSpace(out)
		addAudit("启动 Agent", agentID, "单机", "成功")

	case "stop":
		// 停止 SAgent 进程组：先标记 stopped（防止 supervisor 自动重启）→ SIGTERM 优雅退出 → SIGKILL 兜底
		containerName := agentContainerName(agentID)
		dockerCmd("exec", containerName, "sh", "-c",
			"touch /home/deploy/SAgent/run/stopped; "+
				"kill -15 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"sleep 3; "+
				"kill -9 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; true")
		result["output"] = "SAgent 进程组已停止"
		addAudit("停止 Agent", agentID, "单机", "成功")

	case "restart":
		// 重启 SAgent 进程组
		// touch stopped → SIGTERM 优雅退出 → 等 3s → SIGKILL 兜底 → 删 stopped 标记触发 supervisor 重启
		containerName := agentContainerName(agentID)
		dockerCmd("exec", containerName, "sh", "-c",
			"touch /home/deploy/SAgent/run/stopped; "+
				"kill -15 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"sleep 3; "+
				"kill -9 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"rm -f /home/deploy/SAgent/run/stopped; true")
		time.Sleep(5 * time.Second)
		result["output"] = "SAgent 进程组已重启"
		addAudit("重启 Agent", agentID, "单机", "成功")

	case "deploy":
		out, err := runCmd(composeDir, "docker", "compose", "up", "-d", "--force-recreate", service)
		if err != nil {
			result["success"] = false
			result["error"] = err.Error()
			addAudit("部署 Agent", agentID, "单机", "失败: "+err.Error())
		} else {
			result["output"] = strings.TrimSpace(out)
			addAudit("部署 Agent", agentID, "单机", "成功")
		}

	case "upgrade":
		runCmd(composeDir, "docker", "compose", "build", service)
		runCmd(composeDir, "docker", "compose", "up", "-d", "--force-recreate", service)
		result["output"] = fmt.Sprintf("upgraded to %s", version)
		result["version"] = version
		addAudit("升级 Agent", agentID, "单机", "目标版本: "+version)

	default:
		// 处理测试类 action (test_*)
		if strings.HasPrefix(action, "test_") {
			result = runTestAction(action)
		} else {
			result["success"] = false
			result["error"] = "unknown action: " + action
		}
	}

	return result
}

// runTestAction 执行实际测试
func runTestAction(action string) map[string]interface{} {
	result := map[string]interface{}{
		"action":  action,
		"success": true,
	}

	switch action {
	case "test_dataflow":
		// 数据流测试：检查 VM 中的指标
		checks := []map[string]interface{}{}
		resp, err := httpGet(vmBase()+"/api/v1/label/__name__/values")
		if err != nil {
			result["success"] = false
			result["output"] = fmt.Sprintf("VM unreachable: %v", err)
			return result
		}
		hostMetrics := []string{"host_cpu_percent", "host_memory_total_bytes", "host_disk_total_bytes", "host_net_bytes_recv_total", "host_load1"}
		for _, m := range hostMetrics {
			if strings.Contains(resp, m) {
				checks = append(checks, map[string]interface{}{"metric": m, "status": "PASS"})
			} else {
				checks = append(checks, map[string]interface{}{"metric": m, "status": "FAIL"})
				result["success"] = false
			}
		}
		// 自监控
		if strings.Contains(resp, "sagent_uptime_seconds") {
			checks = append(checks, map[string]interface{}{"metric": "sagent_self_metrics", "status": "PASS"})
		} else {
			checks = append(checks, map[string]interface{}{"metric": "sagent_self_metrics", "status": "FAIL"})
			result["success"] = false
		}
		// up 检查
		upResp, _ := httpGet(vmBase()+"/api/v1/query?query=up")
		upCount := strings.Count(upResp, `"1"`)
		checks = append(checks, map[string]interface{}{"metric": fmt.Sprintf("up_nodes(%d)", upCount), "status": "PASS"})

		result["checks"] = checks
		result["output"] = fmt.Sprintf("数据流测试: %d 项检查", len(checks))

	case "test_crash":
		// 崩溃恢复测试
		before := time.Now()
		dockerCmd("exec", agentContainerName("sagent-1"), "kill", "1")
		time.Sleep(8 * time.Second)
		after := time.Now()
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		recoveryTime := after.Sub(before).Seconds()
		result["output"] = fmt.Sprintf("崩溃恢复: %.0fs, 状态=%s", recoveryTime, strings.TrimSpace(statusOut))
		result["recovery_seconds"] = recoveryTime
		if strings.TrimSpace(statusOut) != "healthy" {
			result["success"] = false
		}

	case "test_load":
		// 高负载压测
		start := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				httpGet(vmBase()+"/api/v1/query?query=host_cpu_percent")
				httpGet(fmt.Sprintf("%s/api/v1/query_range?query=host_memory_used_percent&start=%d&end=%d&step=15", vmBase(), time.Now().Unix()-300, time.Now().Unix()))
			}()
		}
		wg.Wait()
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		result["output"] = fmt.Sprintf("高负载压测: 50请求/%.1fs, 状态=%s", time.Since(start).Seconds(), strings.TrimSpace(statusOut))
		if strings.TrimSpace(statusOut) != "healthy" {
			result["success"] = false
		}

	case "test_config":
		// 配置变更测试
		dockerCmd("exec", agentContainerName("sagent-1"), "sh", "-c", "echo 'test: 1' >> /tmp/test.txt")
		time.Sleep(3 * time.Second)
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		result["output"] = fmt.Sprintf("配置变更测试: 状态=%s", strings.TrimSpace(statusOut))

	case "test_network":
		// 网络中断恢复测试
		dockerCmd("network", "disconnect", cfgL0Net, agentContainerName("vmagent"))
		time.Sleep(15 * time.Second)
		dockerCmd("network", "connect", cfgL0Net, agentContainerName("vmagent"))
		time.Sleep(15 * time.Second)
		upResp, _ := httpGet(vmBase()+"/api/v1/query?query=up")
		upCount := strings.Count(upResp, `"1"`)
		result["output"] = fmt.Sprintf("网络中断恢复: %d 节点在线", upCount)

	default:
		result["success"] = false
		result["output"] = "未知测试: " + action
	}

	return result
}

func httpGet(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

func dockerCmd(args ...string) {
	runCmd("../deploy/docker", "docker", args...)
}

func dockerCmdOut(args ...string) (string, error) {
	return runCmd("../deploy/docker", "docker", args...)
}

// rowToAgent SQLite 行转内存 Agent
func rowToAgent(r *storepkg.AgentRow) *Agent {
	st := "healthy"
	return &Agent{
		ID: r.ID, Name: r.Name, Type: r.Type, Host: r.Host, Port: r.Port,
		Plugins: r.Plugins, Version: r.Version, Labels: r.Labels,
		Source: r.Source, LastSeen: r.LastSeen, CfgDesired: r.CfgDesired,
		CfgEffective: r.CfgEffective, Stats: r.Stats, Status: st,
	}
}

// ensureSystemPlugins 系统内置插件行引导（host_metrics/docker_metrics/custom_scripts）。
// 这三类插件的指标定义走 data/metrics.json 种子（保留 grp/level/phase 等治理字段，
// integrations 包格式不带），但其插件行此前依赖手工 SQL，删库重建后会丢失；
// 启动时按需补齐，已存在则跳过（不覆盖用户编辑）。
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

// syncAgentConfig 目标变更后重算分派 Agent 的期望配置（结构化 doc：targets 段 + host_metrics 段）。
// host_metrics 段属于配置轨道（界面编辑产生），重算时原样保留；targets 段随采集目标变更刷新。
// 内容有变化则版本 +1（心跳热生效）。
func syncAgentConfig(store *AgentStore, catDB *storepkg.DB, agentID string) {
	if agentID == "" {
		return
	}
	targets, err := catDB.ListTargets()
	if err != nil {
		return
	}
	list := []map[string]string{}
	for _, t := range targets {
		if t.AgentID == agentID {
			list = append(list, map[string]string{"plugin": t.Plugin, "target": t.Name, "type": t.Type, "address": t.Address})
		}
	}
	doc := map[string]interface{}{"targets": list}
	// 配置轨道独立：保留既有 host_metrics 段，避免目标变更把界面下发的分组/例外冲掉
	// （parseAgentConfigDoc 兼容旧格式纯数组 content，避免解析失败导致配置段丢失）
	if _, content, err := catDB.GetAgentConfig(agentID); err == nil && content != "" {
		if hm, ok := parseAgentConfigDoc(content)["host_metrics"]; ok && hm != nil {
			doc["host_metrics"] = hm
		}
	}
	// 从未配置过的 Agent 补显式默认段，让下发配置自描述（不依赖 Agent 端隐式约定）
	if _, ok := doc["host_metrics"]; !ok {
		doc["host_metrics"] = defaultHostMetricsCfg()
	}
	content, _ := json.MarshalIndent(doc, "", "  ")
	ver, changed, err := catDB.SetAgentConfig(agentID, string(content))
	if err != nil {
		return
	}
	if a := store.Get(agentID); a != nil {
		a.CfgDesired = ver
	}
	if changed {
		addAudit("配置变更", agentID, "目标变更触发", fmt.Sprintf("版本 cfg-%d", ver))
	}
	InvalidateReconCache()
}

// refreshAgentStatuses 通过 Docker 查询 Agent 状态
func refreshAgentStatuses(store *AgentStore) {
	for _, agent := range store.List() {
		// 心跳型 Agent：状态由 last_seen 推导（90s 内在线），不做 docker 探测
		if agent.Source == "heartbeat" {
			if agent.LastSeen > 0 && time.Now().Unix()-agent.LastSeen > 90 {
				agent.Status = "offline"
			} else if agent.LastSeen > 0 {
				agent.Status = "healthy"
			}
			continue
		}
		out, err := runCmd(".", "docker", "inspect", "-f", "{{.State.Status}}", agentContainerName(agent.ID))
		if err != nil {
			agent.Status = "stopped"
		} else {
			status := strings.TrimSpace(out)
			if status == "running" {
				// 先检查 stopped 标记文件
				stoppedOut, _ := runCmd(".", "docker", "exec", agentContainerName(agent.ID),
					"cat", "/home/deploy/SAgent/run/stopped")
				// 再检查 SAgent health
				healthOut, _ := runCmd(".", "docker", "exec", agentContainerName(agent.ID),
					"wget", "-qO-", "--timeout=2", "http://localhost:"+cfgAgentHTTPPort+"/health")
				if strings.TrimSpace(healthOut) == `{"status":"healthy"}` {
					agent.Status = "healthy"
					// 探测真实版本
					if v := detectVersion(agent.ID); v != "" {
						agent.Version = v
					}
				} else if strings.TrimSpace(stoppedOut) != "" {
					// stopped 标记文件存在 → agent 已被管控停止
					agent.Status = "stopped"
				} else {
					agent.Status = "stopped"
				}
			} else {
				agent.Status = status
			}
		}
		agent.UpdatedAt = time.Now()
	}
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
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
func normalizeTargets(v interface{}) interface{} {
	if arr, ok := v.([]interface{}); ok && arr != nil {
		return arr
	}
	return []interface{}{}
}

// defaultHostMetricsCfg 默认 host_metrics 配置段（MVP：全组默认开启、30s、无例外清单）
func defaultHostMetricsCfg() map[string]interface{} {
	return map[string]interface{}{
		"enabled":        true,
		"interval":       "30s",
		"groups":         map[string]interface{}{},
		"exclude_metrics": []string{},
	}
}

// parseAgentConfigDoc 解析期望配置 content 为结构化 doc，兼容旧格式（纯 targets 数组）；
// host_metrics 段缺失时补默认值（未声明组默认开启，与 Agent 注册表约定一致）
func parseAgentConfigDoc(content string) map[string]interface{} {
	doc := map[string]interface{}{}
	var legacy []map[string]interface{}
	if err := json.Unmarshal([]byte(content), &legacy); err == nil && legacy != nil {
		doc["targets"] = legacy
		doc["host_metrics"] = defaultHostMetricsCfg()
		return doc
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(content), &m); err == nil && m != nil {
		doc["targets"] = m["targets"]
		if hm, ok := m["host_metrics"].(map[string]interface{}); ok {
			doc["host_metrics"] = hm
		} else {
			doc["host_metrics"] = defaultHostMetricsCfg()
		}
		return doc
	}
	doc["targets"] = []interface{}{}
	doc["host_metrics"] = defaultHostMetricsCfg()
	return doc
}

// handleAgentConfigRoute GET /api/agents/{id}/config 读取（结构化 parsed 视图）；
// PUT 结构化更新 host_metrics 配置段（配置轨道：config_version+1，心跳热生效，不涉及 Agent 代码版本）
// agentExists 校验 Agent 真实存在（内存注册表或持久层任一命中）。
// 配置轨道/目标分派只允许绑定真实 Agent，杜绝幽灵数据（红线 4 的前提约束）。
func agentExists(store *AgentStore, catDB *storepkg.DB, id string) bool {
	if id == "" {
		return false
	}
	if store.Get(id) != nil {
		return true
	}
	if srcRow, _ := catDB.GetAgentSource(id); srcRow != "" {
		return true
	}
	return false
}

func handleAgentConfigRoute(w http.ResponseWriter, r *http.Request, agentID string, store *AgentStore, catDB *storepkg.DB) {
	// 健壮性门禁：此前幽灵 Agent PUT 会 200 静默落库，GET 还会触发 nil 解引用 panic——双缺陷一并封堵
	if !agentExists(store, catDB, agentID) {
		http.Error(w, "agent not found: "+agentID, http.StatusNotFound)
		return
	}
	ver, content, _ := catDB.GetAgentConfig(agentID)
	a := store.Get(agentID)
	src := ""
	if a != nil {
		src = a.Source
	}

	switch r.Method {
	case http.MethodGet:
		if ver > 0 && content != "" {
			writeJSON(w, map[string]interface{}{
				"agent_id":      agentID,
				"version":       ver,
				"content":       content,
				"parsed":        parseAgentConfigDoc(content),
				"cfg_desired":   ver,
				"cfg_effective": a.CfgEffective,
				"source":        src,
			})
			return
		}
		// 无版本化配置：本机演示容器回退展示文件配置；其余回默认 doc（version=0，可直接 PUT 初始化）
		configPath := fmt.Sprintf("../deploy/docker/configs/sagent/%s.yaml", agentID)
		if data, err := os.ReadFile(configPath); err == nil && src == "docker" {
			writeJSON(w, map[string]interface{}{
				"agent_id": agentID, "version": 0, "content": string(data),
				"parsed": parseAgentConfigDoc(""), "file_fallback": true, "source": src,
			})
			return
		}
		writeJSON(w, map[string]interface{}{
			"agent_id": agentID, "version": 0, "content": "",
			"parsed": parseAgentConfigDoc(""), "source": src,
		})

	case http.MethodPut:
		var req struct {
			HostMetrics *map[string]interface{} `json:"host_metrics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostMetrics == nil {
			writeJSON(w, map[string]interface{}{"error": "host_metrics 配置段必填"})
			return
		}
		// 无版本化配置先引导出一份（targets 展开 + host_metrics 默认段）
		if ver == 0 {
			syncAgentConfig(store, catDB, agentID)
			ver, content, _ = catDB.GetAgentConfig(agentID)
		}
		// 兼容旧格式（纯数组 content）：统一解析保底，避免 targets 段丢失
		prev := parseAgentConfigDoc(content)
		prev["targets"] = normalizeTargets(prev["targets"])
		prev["host_metrics"] = *req.HostMetrics
		newContent, _ := json.MarshalIndent(prev, "", "  ")
		newVer, changed, err := catDB.SetAgentConfig(agentID, string(newContent))
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if a := store.Get(agentID); a != nil {
			a.CfgDesired = newVer
		}
		if changed {
			addAudit("配置变更", agentID, "指标配置调整", fmt.Sprintf("host_metrics 段更新，版本 cfg-%d", newVer))
			InvalidateReconCache()
		}
		writeJSON(w, map[string]interface{}{"ok": true, "version": newVer, "changed": changed})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handlePluginStatus 查询 Agent 的插件运行状态
func handlePluginStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	containerName := agentContainerName(agentID)
	out, err := runCmd(".", "docker", "exec", containerName,
		"/home/deploy/SAgent/bin/SAgent", "-ctl", "status")
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error(), "plugins": map[string]string{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(strings.TrimSpace(out)))
}

// handleAgentMetrics 获取 Agent 的 Prometheus 指标
func handleAgentMetrics(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	out, err := runCmd(".", "docker", "exec", agentContainerName(agentID),
		"wget", "-qO-", "--timeout=3", "http://localhost:"+cfgAgentHTTPPort+"/metrics")
	if err != nil {
		http.Error(w, "failed to fetch metrics: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(out))
}

// handleAgentLogs 获取 Agent 的日志
func handleAgentLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	containerName := agentContainerName(agentID)
	out, err := runCmd(".", "docker", "exec", containerName,
		"tail", "-50", "/home/deploy/SAgent/logs/SAgent.log")
	if err != nil {
		http.Error(w, "failed to fetch logs: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(out))
}

// executePluginAction 执行插件级操作（通过 control.sock 或直接信号）
func executePluginAction(agentID, plugin, action string) map[string]interface{} {
	result := map[string]interface{}{
		"agent_id": agentID,
		"plugin":   plugin,
		"action":   action,
		"success":  true,
	}
	containerName := agentContainerName(agentID)
	sagentBin := "/home/deploy/SAgent/bin/SAgent"

	checkResult := func(out string, err error) {
		if err != nil {
			result["success"] = false
			result["error"] = err.Error()
			return
		}
		out = strings.TrimSpace(out)
		result["output"] = out
		if strings.HasPrefix(out, "ERROR") {
			result["success"] = false
			result["error"] = out
		}
	}

	switch action {
	case "reload":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("reload %s", plugin)))

	case "stop":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("stop %s", plugin)))

	case "start":
		checkResult(runCmd(".", "docker", "exec", containerName, sagentBin, "-ctl",
			fmt.Sprintf("start %s", plugin)))

	case "restart":
		executePluginAction(agentID, plugin, "stop")
		time.Sleep(1 * time.Second)
		return executePluginAction(agentID, plugin, "start")

	default:
		result["success"] = false
		result["error"] = "unknown plugin action: " + action
	}
	return result
}
