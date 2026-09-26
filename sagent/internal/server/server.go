package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/resource"
)

// Server HTTP 服务，暴露 :19090
type Server struct {
	cfg           *config.Config
	pipeline      *pipeline.Pipeline
	selfMetrics   *pipeline.SelfMetrics
	pluginManager *PluginManager
	log           Logger
	controlMeters []func() string // OBS-1：控制通道通信可观测指标 provider（client.MetricsText）
}

// Logger 简单日志接口（避免循环依赖）
type Logger interface {
	Info(format string, args ...interface{})
	Warn(format string, args ...interface{})
}

// PluginHandle 插件句柄，暴露给 control.sock 管理
type PluginHandle interface {
	PluginName() string
	Stop() error
	Start() error
	Signal(sig os.Signal) error
	Running() bool
}

// authOK 校验 /metrics 鉴权：优先 Bearer Header，兼容 ?auth_key= 查询参数。
// 常量时间比较防时序侧信道（密钥不比对本体传入）
func authOK(r *http.Request, want string) bool {
	ah := r.Header.Get("Authorization")
	if strings.HasPrefix(ah, "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(ah, "Bearer ")), []byte(want)) == 1 {
		return true
	}
	if qk := r.URL.Query().Get("auth_key"); qk != "" && subtle.ConstantTimeCompare([]byte(qk), []byte(want)) == 1 {
		return true
	}
	return false
}

// handleRootRouter 根级兜底路由：识别 vmauth(v1.93 保留前缀转发) 过来的 /sagent-{n}/metrics 请求，
// 转 handleMetrics 复用同一 Bearer 鉴权逻辑。分割方式避免在 Server Mux 混排通配符与字面量（会 panic）。
func (s *Server) handleRootRouter(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 2 && strings.HasPrefix(segs[0], "sagent-") && segs[1] == "metrics" {
		s.handleMetrics(w, r)
		return
	}
	http.NotFound(w, r)
}

// handleMetrics /metrics 处理器：未配置 auth_key 时开放（兼容存量/演示）；配置后强制鉴权
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Server.MetricsAuthKey != "" && !authOK(r, s.cfg.Server.MetricsAuthKey) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sagent-metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, s.pipeline.PrometheusText())
	fmt.Fprint(w, s.selfMetrics.Collect())
	for _, p := range s.controlMeters {
		fmt.Fprint(w, p())
	}
}

// PluginManager 管理所有插件
func NewPluginManager() *PluginManager {
	return &PluginManager{plugins: make(map[string]PluginHandle)}
}

type PluginManager struct {
	plugins map[string]PluginHandle
}

func (pm *PluginManager) Register(p PluginHandle) {
	pm.plugins[p.PluginName()] = p
}

func (pm *PluginManager) Get(name string) PluginHandle {
	return pm.plugins[name]
}

func (pm *PluginManager) All() map[string]PluginHandle {
	return pm.plugins
}

// New 创建 HTTP 服务
func New(cfg *config.Config, pl *pipeline.Pipeline, pm *PluginManager, log Logger) *Server {
	return &Server{
		cfg:           cfg,
		pipeline:      pl,
		pluginManager: pm,
		log:           log,
		selfMetrics:   pipeline.NewSelfMetrics(),
	}
}

// AddControlMeter 注册控制通道通信可观测指标 provider（OBS-1），/metrics 聚合输出。
func (s *Server) AddControlMeter(provider func() string) {
	if provider != nil {
		s.controlMeters = append(s.controlMeters, provider)
	}
}

// Start 启动 HTTP 服务
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// /metrics — Prometheus 格式指标（含自监控）。
	// D4 安全收敛：配置了 metrics_auth_key 后，未携带正确鉴权的访问一律 401 且不返回任何 metric——
	// 达成"curl 目标机:19090/metrics 拿不到采集数据"的目标。vmagent 通过 Authorization: Bearer 抓取
	mux.HandleFunc("/metrics", s.handleMetrics)

	// D4 端口收口（vmauth 单端口收敛）：vmauth 在保留原 path 前缀的前提下把 /sagent-{n}/metrics
	// 转发到该 SAgent:19090，因此为满足 vmauth(v1.93 不支持剥前缀) 的转发语义，需同时暴露
	// /sagent-*/metrics 的前缀路由，供 vmauth 按 path 白名单路由到对应 SAgent。
	// 注意：Go ServeMux 不允许通配符与字面量混排在同一 path 段（"/sagent-{name}/metrics" 会 panic），
	// 故用根级兜底路由 handleRootRouter 统一识别命中 /sagent-*/metrics 的请求，转 handleMetrics。
	// 根级路由只对未被更具体路由（/metrics、/health、/plugins 等）匹配的路径生效，不影响其他端点。
	mux.HandleFunc("/", s.handleRootRouter)

	// /health — 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"healthy"}`)
	})

	// /plugins — 全部插件状态
	mux.HandleFunc("/plugins", s.handlePlugins)

	// /otlp/v1/metrics — OTLP 接收端点
	mux.HandleFunc("/otlp/v1/metrics", s.handleOTLP)

	// control.sock — Unix Socket 管理通道
	go s.startControlSocket()

	fmt.Printf("SAgent Core starting on %s\n", s.cfg.Server.Listen)
	fmt.Printf("Resource ID: %s\n", s.cfg.Resource.ID)
	return http.ListenAndServe(s.cfg.Server.Listen, mux)
}

// handlePlugins 返回所有已启用插件列表
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type pluginInfo struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Type   string `json:"type"`
	}
	var plugins []pluginInfo
	add := func(name, typ string) {
		plugins = append(plugins, pluginInfo{Name: name, Status: "running", Type: typ})
	}
	if s.cfg.Plugins.HostMetrics.Enabled {
		add("host_metrics", "builtin")
	}
	if s.cfg.Plugins.LogMetrics.Enabled {
		add("log_metrics", "subprocess")
	}
	if s.cfg.Plugins.MySQLProbe.Enabled {
		add("mysql_probe", "subprocess")
	}
	if s.cfg.Plugins.CustomScripts.Enabled {
		add("custom_scripts", "exec")
	}
	if s.cfg.Plugins.PortChecker.Enabled {
		add("port_checker", "builtin")
	}
	if s.cfg.Plugins.PrometheusScrape.Enabled {
		add("prometheus_scrape", "scrape")
	}

	resp, _ := json.Marshal(map[string]interface{}{"plugins": plugins})
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// handleOTLP 接收 OTLP metrics（简化实现，只解析 JSON body）
func (s *Server) handleOTLP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"read body"}`)
		return
	}
	r.Body.Close()

	// 记录收到的 OTLP 数据大小
	s.pipeline.IngestOTLP(len(body))

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"partialSuccess":{"rejectedDataPoints":0}}`)
}

// startControlSocket 启动 control.sock（热加载/管理通道）
func (s *Server) startControlSocket() {
	socketPath := constants.DefaultControlSock
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control.sock: listen failed: %v\n", err)
		return
	}
	defer listener.Close()
	os.Chmod(socketPath, 0600)

	fmt.Printf("Control socket listening on %s\n", socketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go s.handleControl(conn)
	}
}

// handleControl 处理 control.sock 命令
func (s *Server) handleControl(conn net.Conn) {
	defer conn.Close()

	var buf [4096]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		return
	}

	cmd := strings.TrimSpace(string(buf[:n]))
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "reload":
		if len(parts) > 1 {
			p := s.pluginManager.Get(parts[1])
			if p != nil {
				s.log.Info("[control] reload plugin %s", parts[1])
				if err := p.Signal(os.Signal(syscall.SIGHUP)); err != nil {
					s.log.Warn("[control] reload %s: signal failed: %v", parts[1], err)
					fmt.Fprintf(conn, "ERROR reload %s: %v\n", parts[1], err)
				} else {
					s.log.Info("[control] reload %s: SIGHUP sent OK", parts[1])
					fmt.Fprintf(conn, "OK reload %s: SIGHUP delivered\n", parts[1])
				}
			} else {
				s.log.Warn("[control] reload: plugin %s not found", parts[1])
				fmt.Fprintf(conn, "ERROR plugin %s not found\n", parts[1])
			}
		} else {
			s.log.Info("[control] global reload requested")
			fmt.Fprintf(conn, "OK global reload\n")
		}

	case "status":
		plugins := make(map[string]string)
		for name, p := range s.pluginManager.All() {
			if p.Running() {
				plugins[name] = "running"
			} else {
				plugins[name] = "stopped"
			}
		}
		status := map[string]interface{}{
			"uptime_seconds": time.Since(s.selfMetrics.Uptime()).Seconds(),
			"plugins_active": s.activePluginCount(),
			"plugins":        plugins,
		}
		data, _ := json.Marshal(status)
		fmt.Fprintf(conn, "%s\n", data)

	case "stop":
		if len(parts) > 1 {
			p := s.pluginManager.Get(parts[1])
			if p != nil {
				s.log.Info("[control] stop plugin %s", parts[1])
				fmt.Fprintf(conn, "OK plugin %s stopped\n", parts[1])
				go p.Stop()
			} else {
				s.log.Warn("[control] stop: plugin %s not found", parts[1])
				fmt.Fprintf(conn, "ERROR plugin %s not found\n", parts[1])
			}
		} else {
			s.log.Info("[control] stop-all requested")
			for name, p := range s.pluginManager.All() {
				s.log.Info("[control]   stopping %s", name)
				go p.Stop()
				fmt.Fprintf(conn, "OK plugin %s stopped\n", name)
			}
		}

	case "start":
		if len(parts) > 1 {
			p := s.pluginManager.Get(parts[1])
			if p != nil {
				s.log.Info("[control] start plugin %s", parts[1])
				if err := p.Start(); err != nil {
					s.log.Warn("[control] start %s failed: %v", parts[1], err)
					fmt.Fprintf(conn, "ERROR start %s: %v\n", parts[1], err)
				} else {
					fmt.Fprintf(conn, "OK plugin %s started\n", parts[1])
				}
			} else {
				s.log.Warn("[control] start: plugin %s not found", parts[1])
				fmt.Fprintf(conn, "ERROR plugin %s not found\n", parts[1])
			}
		} else {
			fmt.Fprintf(conn, "USAGE: start <plugin_name>\n")
		}

	default:
		s.log.Warn("[control] unknown command: %s", parts[0])
		fmt.Fprintf(conn, "UNKNOWN command: %s\n", parts[0])
	}
}

func (s *Server) activePluginCount() int {
	count := 0
	if s.cfg.Plugins.HostMetrics.Enabled {
		count++
	}
	if s.cfg.Plugins.LogMetrics.Enabled {
		count++
	}
	if s.cfg.Plugins.MySQLProbe.Enabled {
		count++
	}
	if s.cfg.Plugins.CustomScripts.Enabled {
		count++
	}
	if s.cfg.Plugins.PortChecker.Enabled {
		count++
	}
	if s.cfg.Plugins.PrometheusScrape.Enabled {
		count++
	}
	return count
}

// BuildResourceLabels 从配置构建资源标签
func BuildResourceLabels(cfg *config.ResourceConfig) resource.Labels {
	return resource.Labels{
		ResourceID:     cfg.ID,
		ResourceType:   cfg.Type,
		BusinessSystem: cfg.BusinessSystem,
		Env:            cfg.Env,
		IDC:            cfg.IDC,
		Cluster:        cfg.Cluster,
	}
}
