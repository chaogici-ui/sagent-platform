package server

import (
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

// Start 启动 HTTP 服务
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// /metrics — Prometheus 格式指标（含自监控）
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, s.pipeline.PrometheusText())
		fmt.Fprint(w, s.selfMetrics.Collect())
	})

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
