package main

import (
	"context"
	"flag"
	"fmt"
	stdlog "log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sagent/core/internal/bus"
	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/control"
	"github.com/sagent/core/internal/logger"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
	"github.com/sagent/core/internal/plugin/subprocess"
	"github.com/sagent/core/internal/server"
)

// App SAgent 应用实例，组合所有核心组件
type App struct {
	cfg         *config.Config
	log         *logger.Logger
	pipeline    *pipeline.Pipeline
	busServer   *bus.Server
	server      *server.Server
	pluginMgr   *server.PluginManager
	subPlugins  []*subprocess.Plugin
	metricsCh   chan builtin.MetricBatch
	hostRuntime *hostRuntime
	ctl         *control.Client // OBS-1：控制通道，其通信指标经 server./metrics 暴露
}

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	ctlCmd := flag.String("ctl", "", "send command to control.sock (e.g. 'reload log_metrics')")
	configPath := flag.String("config", constants.DefaultConfigPath, "path to SAgent.yaml")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("SAgent v%s\n", constants.Version)
		return
	}

	// -ctl 模式：向 control.sock 发送命令并打印响应
	if *ctlCmd != "" {
		conn, err := net.Dial("unix", constants.DefaultControlSock)
		if err != nil {
			fmt.Fprintf(os.Stderr, "control.sock connect failed: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s\n", *ctlCmd)
		var buf [4096]byte
		n, _ := conn.Read(buf[:])
		os.Stdout.Write(buf[:n])
		return
	}

	// 初始化日志
	log, err := logger.New(constants.DefaultLogDir, constants.DefaultLogFile, logger.INFO)
	if err != nil {
		panic("failed to init logger: " + err.Error())
	}
	defer log.Close()

	// 重定向标准库 log 到 logger 的 writer，确保插件 log.Printf 也写入文件
	stdlog.SetOutput(log.StdLogger().Writer())

	log.Info("SAgent Core v%s starting...", constants.Version)

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("Failed to load config: %v", err)
		os.Exit(1)
	}
	log.Info("Resource ID: %s", cfg.Resource.ID)

	// 构建 App
	app := &App{
		cfg:       cfg,
		log:       log,
		metricsCh: make(chan builtin.MetricBatch, 100),
	}

	// 初始化各组件
	app.pluginMgr = server.NewPluginManager()
	app.initPipeline()
	app.initPlugins()
	app.initBus()

	// 定期心跳
	go app.heartbeat()

	// L0 接入中心回传通道（注册/心跳/配置拉取）；未配置 l0_console.url 时不启用
	if app.cfg.L0Console.URL != "" {
		labels := map[string]string{
			constants.TagBusinessSystem: app.cfg.Resource.BusinessSystem,
			constants.TagEnv:            app.cfg.Resource.Env,
			constants.TagIDC:            app.cfg.Resource.IDC,
			constants.TagCluster:        app.cfg.Resource.Cluster,
		}
		// 池归属只在登记时带上（空值不写，避免平台侧把空串当成"已登记池"）
		if p := app.cfg.Resource.Pool; p != "" {
			labels[constants.TagPool] = p
		}
		if r := app.cfg.Resource.Region; r != "" {
			labels[constants.TagRegion] = r
		}
		// 本机自描述与自身 /metrics 地址都从配置来，不再写死：
		// 注册上报的插件清单 = 实际启用的插件；metricsURL 处理 0.0.0.0/[::] 前缀
		desc := control.SelfDesc{
			Type:              app.cfg.Resource.Type,
			Plugins:           app.cfg.EnabledPluginNames(),
			HeartbeatInterval: app.cfg.L0Console.HeartbeatInterval, // 0 = 按资源类型取默认（采集机 10s / 其余 30s）
		}
		metricsURL := loopbackURL(app.cfg.Server.Listen)
		applier := control.NewConfigApplier(app.cfg.L0Console.DataDir, app.applyPlatformConfig)
		if err := applier.Restore(context.Background()); err != nil {
			log.Warn("Failed to restore last-good platform configuration: %v", err)
		}
		// L0 地址合并：endpoints 非空用 endpoints（已含主端点顺序），否则回落单点 URL（兼容存量配置）
		l0URLs := app.cfg.L0Console.Endpoints
		if l0URLs == "" {
			l0URLs = app.cfg.L0Console.URL
		}
		ctl := control.NewClient(l0URLs, app.cfg.Resource.ID,
			constants.Version, labels, metricsURL, desc, log)
		ctl.SetConfigApplier(applier.Apply)
		app.ctl = ctl
		go ctl.Run(context.Background())
	}

	// 优雅退出
	app.handleSignals()

	// 等待首次指标就绪后启动 HTTP
	app.waitForMetrics()
	app.initServer()
	app.run()
}

// loopbackURL 把 listen 地址转成本机可达的 URL：0.0.0.0 / [::] / 空前缀一律换成 127.0.0.1。
// 直接拼 "http://127.0.0.1"+listen 在 listen=0.0.0.0:19090 时会生成非法 URL（metrics_confirmed 永不回报）
func loopbackURL(listen string) string {
	addr := strings.TrimSpace(listen)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = strings.TrimPrefix(addr, ":")
		host = ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// initPipeline 初始化流水线
func (app *App) initPipeline() {
	resLabels := server.BuildResourceLabels(&app.cfg.Resource)
	app.pipeline = pipeline.New(resLabels)
	go app.consumeMetricBatches()
}

func (app *App) consumeMetricBatches() {
	for batch := range app.metricsCh {
		app.pipeline.IngestBatch(batch)
	}
}

// initPlugins 初始化所有插件
func (app *App) initPlugins() {
	cfg := app.cfg

	app.hostRuntime = newHostRuntime(app.pipeline, cfg.Plugins.HostMetrics)
	app.pluginMgr.Register(app.hostRuntime)
	if cfg.Plugins.HostMetrics.Enabled {
		if err := app.hostRuntime.Start(); err != nil {
			app.log.Warn("Failed to start %s: %v", constants.PluginNameHostMetrics, err)
		} else {
			app.log.Info("Plugin %s started", constants.PluginNameHostMetrics)
		}
	}

	// ---- subprocess: log_metrics (Vector 日志转指标) ----
	if cfg.Plugins.LogMetrics.Enabled {
		bin := cfg.Plugins.LogMetrics.BinPath
		if bin == "" {
			bin = constants.DefaultVectorBin
		}
		confDir := cfg.Plugins.LogMetrics.ConfDir
		if confDir == "" {
			confDir = constants.DefaultVectorConfDir
		}
		vp := subprocess.New(constants.PluginNameLogMetrics, bin, []string{"--config-dir", confDir, "--watch-config"})
		vp.WorkDir = "."
		if err := vp.Start(); err != nil {
			app.log.Warn("Failed to start %s: %v", constants.PluginNameLogMetrics, err)
		} else {
			app.subPlugins = append(app.subPlugins, vp)
			app.pluginMgr.Register(vp)
			app.log.Info("Plugin %s started", constants.PluginNameLogMetrics)
		}
	}

	// ---- subprocess: mysql_probe ----
	if cfg.Plugins.MySQLProbe.Enabled {
		bin := cfg.Plugins.MySQLProbe.ExporterBin
		if bin == "" {
			bin = constants.DefaultMysqlExportBin
		}
		probe := builtin.NewMySQLProbe(cfg.Plugins.MySQLProbe)
		if err := probe.StartWith(bin, app.metricsCh); err != nil {
			app.log.Warn("Failed to start %s: %v", constants.PluginNameMySQLProbe, err)
		} else {
			app.pluginMgr.Register(probe)
			app.log.Info("Plugin %s started (%d targets)", constants.PluginNameMySQLProbe, len(cfg.Plugins.MySQLProbe.Targets))
		}
	}

	// ---- exec: custom_scripts ----
	if cfg.Plugins.CustomScripts.Enabled {
		e := builtin.NewExecScripts(cfg.Plugins.CustomScripts)
		e.SetMetricsCh(app.metricsCh)
		if err := e.Start(); err != nil {
			app.log.Warn("Failed to start %s: %v", constants.PluginNameCustomScripts, err)
		} else {
			app.pluginMgr.Register(e)
			app.log.Info("Plugin %s started (%d scripts)", constants.PluginNameCustomScripts, len(cfg.Plugins.CustomScripts.Scripts))
		}
	}

	// ---- builtin: port_checker ----
	if cfg.Plugins.PortChecker.Enabled {
		targets := make([]builtin.PortTarget, len(cfg.Plugins.PortChecker.Targets))
		for i, t := range cfg.Plugins.PortChecker.Targets {
			targets[i] = builtin.PortTarget{Name: t.Name, Type: t.Type, Address: t.Address, Replicas: t.Replicas, Timeout: t.Timeout, Labels: t.Labels}
		}
		interval := cfg.Plugins.PortChecker.Interval
		if interval == 0 {
			interval = constants.DefaultPortCheckInterval
		}
		pc := builtin.NewPortChecker(targets, interval)
		pc.Start(app.metricsCh)
		app.log.Info("Plugin %s started (%d targets)", constants.PluginNamePortChecker, len(targets))
	}

	// ---- scrape: prometheus_scrape ----
	if cfg.Plugins.PrometheusScrape.Enabled {
		targets := make([]builtin.ScrapeTarget, len(cfg.Plugins.PrometheusScrape.Targets))
		for i, t := range cfg.Plugins.PrometheusScrape.Targets {
			targets[i] = builtin.ScrapeTarget{Name: t.Name, URL: t.URL, Replicas: t.Replicas, Timeout: t.Timeout, Labels: t.Labels}
		}
		interval := cfg.Plugins.PrometheusScrape.Interval
		if interval == 0 {
			interval = constants.DefaultScrapeInterval
		}
		sp := builtin.NewScrapePlugin(targets, interval)
		sp.Start(app.metricsCh)
		app.log.Info("Plugin %s started (%d targets)", constants.PluginNameScrape, len(targets))
	}
}

// initBus 初始化 Unix Socket 总线
func (app *App) initBus() {
	app.busServer = bus.NewServer(constants.DefaultBusSock)
	if err := app.busServer.Start(); err != nil {
		app.log.Error("Failed to start bus: %v", err)
		os.Exit(1)
	}
	app.busServer.SetHandler(func(metrics []bus.Metric) {
		builtinMetrics := make([]builtin.Metric, len(metrics))
		for i, m := range metrics {
			builtinMetrics[i] = builtin.Metric{
				Name:   m.Name,
				Value:  m.Value,
				Help:   m.Help,
				Type:   constants.MetricType(m.Type),
				Labels: m.Labels,
			}
		}
		app.pipeline.Ingest(builtinMetrics)
	})
}

// initServer 初始化 HTTP 服务
func (app *App) initServer() {
	app.server = server.New(app.cfg, app.pipeline, app.pluginMgr, app.log)
	// OBS-1：控制通道通信可观测指标接入 /metrics
	if app.ctl != nil {
		app.server.AddControlMeter(app.ctl.MetricsText)
	}
}

// waitForMetrics 等待首次指标采集完成
func (app *App) waitForMetrics() {
	app.log.Info("Waiting for initial metrics collection...")
	deadline := time.After(constants.DefaultStartupWaitTimeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			app.log.Warn("Initial metrics timeout, starting server anyway")
			return
		case <-ticker.C:
			if app.pipeline.MetricCount() > 0 {
				app.log.Info("Initial metrics collected (%d entries)", app.pipeline.MetricCount())
				return
			}
		}
	}
}

// heartbeat 定期心跳日志
func (app *App) heartbeat() {
	ticker := time.NewTicker(constants.DefaultHeartbeatInterval)
	defer ticker.Stop()
	for range ticker.C {
		app.log.Info("[heartbeat] metrics=%d goroutines=%d", app.pipeline.MetricCount(), runtime.NumGoroutine())
	}
}

// handleSignals 优雅退出
func (app *App) handleSignals() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		app.log.Info("[lifecycle] received signal %s, shutting down...", sig)

		// 停止所有可管理插件
		for name, p := range app.pluginMgr.All() {
			app.log.Info("[lifecycle] stopping plugin %s", name)
			p.Stop()
		}

		// 停止子进程插件（Vector 等）
		for _, sp := range app.subPlugins {
			app.log.Info("[lifecycle] stopping subprocess %s", sp.Name)
			sp.Stop()
		}

		app.busServer.Stop()
		app.log.Info("[lifecycle] shutdown complete")
		app.log.Close()
		os.Exit(0)
	}()
}

// run 启动 HTTP 服务（阻塞）
func (app *App) run() {
	if err := app.server.Start(); err != nil {
		app.log.Error("Server error: %v", err)
		os.Exit(1)
	}
}
