package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/logger"
)

type SelfDesc struct {
	Type    string   // 资源类型（配置 resource.type；空则回落 edge）
	Plugins []string // 已启用插件名（配置 plugins 段中 enabled=true 的）
	// HeartbeatInterval 控制通道心跳间隔；0 = 按类型取默认
	// （proxy/collector → 10s 短心跳，其余 → 30s）。由 main 从配置/资源类型解析后注入
	HeartbeatInterval time.Duration
}

type Client struct {
	endpoints  []string // L0 Console 基址列表（HA 多端点，逗号分隔单个传入拆分；索引即优先级，0=主）
	curIdx     int      // 当前生效端点索引；失败自动 failover 到下一个健康端点
	agentID    string   // 即 resource.id
	version    string
	ip         string
	labels     map[string]string
	metricsURL string // 自身 /metrics 基址，用于清点本机序列（采集入库证据）
	desc       SelfDesc

	sent        map[string]bool
	appliedVer  int
	receivedVer int
	configError string
	applyConfig func(context.Context, int, string) error
	httpClient  *http.Client
	log         *logger.Logger

	meter *Meter // OBS-1：控制通道通信可观测指标（/metrics 暴露，vmagent 抓取入 VM）
}

// NewClient 构造回传客户端；l0URLs 为空时由调用方决定不启动。
// HA：l0URLs 支持多端点（逗号/空格分隔），第一个为主端点，其余为备用；
// post() 失败时按索引 failover 切换健康端点（断线重带指数回退由调用方心跳频率承接）。
func NewClient(l0URLs, agentID, version string, labels map[string]string, metricsURL string, desc SelfDesc, log *logger.Logger) *Client {
	if desc.Plugins == nil {
		desc.Plugins = []string{}
	}
	endpoints := splitURLs(l0URLs)
	ip := ""
	if len(endpoints) > 0 {
		ip = outboundIP(endpoints[0])
	}
	return &Client{
		endpoints:  endpoints,
		curIdx:     0,
		agentID:    agentID,
		version:    version,
		ip:         ip,
		labels:     labels,
		metricsURL: metricsURL,
		desc:       desc,
		log:        log,
		sent:       map[string]bool{},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		meter:      &Meter{},
	}
}

// MetricsText 控制通道通信可观测指标（OBS-1）；由 server /metrics 聚合输出。
func (c *Client) MetricsText() string {
	if c.meter == nil {
		return ""
	}
	return c.meter.PrometheusText()
}

// ReportBytes 累计上报字节（心跳/上报载荷），供外部只读。
func (c *Client) ReportBytes() uint64 {
	if c.meter == nil {
		return 0
	}
	return atomic.LoadUint64(&c.meter.reportBytes)
}

// splitURLs 拆分隔符分隔的 L0 端点列表并剔除空项；空输入返回空切片
func splitURLs(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// endpointsDesc 当前端点列表描述（日志用）
func (c *Client) endpointsDesc() string {
	s := strings.Join(c.endpoints, "; ")
	if len(s) > 300 {
		s = c.endpoints[0] + "; …(" + strconv.Itoa(len(c.endpoints)-1) + " 备用)"
	}
	return s
}

// selfType 上报的资源类型：配置缺省时回落 edge（平台侧注册接口的默认口径）
func (c *Client) selfType() string {
	if t := strings.TrimSpace(c.desc.Type); t != "" {
		return t
	}
	return string(constants.AgentRoleEdge)
}

// heartbeatEvery 控制通道心跳间隔：显式配置优先；未配时按资源类型取默认——
// 采集机（proxy/collector/collector_proxy）用 10s 短心跳换取更快的故障检测，其余维持 30s。
func (c *Client) heartbeatEvery() time.Duration {
	if d := c.desc.HeartbeatInterval; d > 0 {
		return d
	}
	switch c.selfType() {
	case string(constants.AgentRoleProxy), "collector", "collector_proxy":
		return constants.CollectorHeartbeatInterval
	}
	return constants.DefaultHeartbeatInterval
}

// Run 阻塞运行：注册（带重试）→ 心跳循环。ctx 取消即退出。
func (c *Client) Run(ctx context.Context) {
	if err := c.register(ctx); err != nil {
		c.log.Error("[l0] 注册失败，回传通道停用：%v", err)
		return
	}
	c.log.Info("[l0] 注册成功 agent_id=%s（l0=%s）", c.agentID, c.endpointsDesc())

	ticker := time.NewTicker(c.heartbeatEvery())
	defer ticker.Stop()
	// 注册成功后立即心跳一次拉齐配置，并尝试首轮证据回报
	if c.beat() {
		c.flushReports(context.Background())
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.beat() {
				c.flushReports(ctx)
			}
		}
	}
}

// flushReports 每个心跳后尝试推送接入证据（批 C 精简版）。
// 未被平台接受的 kind 下个心跳重试——拒绝原因通常是依赖步骤尚未 running
// 或 cfg_effective 尚未回写，重试即可收敛。
func (c *Client) flushReports(ctx context.Context) {
	if !c.sent["self_metrics"] {
		if c.tryReport(ctx, "self_metrics", map[string]any{
			"process": "up", "health": "healthy", "note": "Agent 本机自检",
		}) {
			c.sent["self_metrics"] = true
		}
	}
	if !c.sent["preflight_ability"] {
		if c.tryReport(ctx, "preflight_ability", map[string]any{
			"port_listen": true, "config_readable": true, "deps_ok": true,
			"note": "Agent 本机基线检查（端口/配置可读/依赖）",
		}) {
			c.sent["preflight_ability"] = true
		}
	}
	if c.appliedVer > 0 && !c.sent["config_applied"] {
		if c.tryReport(ctx, "config_applied", map[string]any{"applied_version": c.appliedVer}) {
			c.sent["config_applied"] = true
		}
	}
	if !c.sent["metrics_confirmed"] {
		if n := c.countOwnSeries(); n > 0 {
			if c.tryReport(ctx, "metrics_confirmed", map[string]any{
				"series_count": n, "endpoint": c.metricsURL,
			}) {
				c.sent["metrics_confirmed"] = true
			}
		}
	}
}

// tryReport 推送一条接入证据；平台返回 ok=true 才算成功（不接受≠失败，下轮重试）
func (c *Client) tryReport(ctx context.Context, kind string, data map[string]any) bool {
	body, _ := json.Marshal(map[string]any{"agent_id": c.agentID, "kind": kind, "data": data})
	// OBS-1：上报载荷字节统计
	if m := c.meter; m != nil {
		m.addReportBytes(len(body))
	}
	var resp struct {
		Ok    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, "/api/onboard/flow/agent-report", body, &resp); err != nil {
		return false
	}
	if resp.Ok {
		c.log.Info("[l0] 自动回报被接受：%s", kind)
	} else {
		c.log.Info("[l0] 自动回报暂不被接受（%s）：%s", kind, resp.Error)
	}
	return resp.Ok
}

// countOwnSeries 清点自身 /metrics 中带本机 resource_id 标签的序列数（采集在吐的证据）
func (c *Client) countOwnSeries() int {
	if c.metricsURL == "" {
		return 0
	}
	resp, err := c.httpClient.Get(c.metricsURL + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	tag := `resource_id="` + c.agentID + `"`
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, tag) {
			n++
		}
	}
	return n
}

// register 注册；失败按 5s/10s/20s/…（上限 60s）重试，直到 ctx 取消
func (c *Client) register(ctx context.Context) error {
	backoff := 5 * time.Second
	for {
		body, _ := json.Marshal(map[string]any{
			"id":      c.agentID,
			"type":    c.selfType(),
			"version": c.version,
			"ip":      c.ip,
			"labels":  c.labels,
			"plugins": c.desc.Plugins,
		})
		var resp struct {
			Ok         bool   `json:"ok"`
			AgentID    string `json:"agent_id"`
			CfgVersion int    `json:"config_version"`
			Config     string `json:"config"`
			Error      string `json:"error"`
		}
		regErr := c.post(ctx, "/api/agent/register", body, &resp)
		registered := regErr == nil && resp.Ok
		if m := c.meter; m != nil {
			m.addReportBytes(len(body))
			m.addRegister(registered)
		}
		if registered {
			if resp.Config != "" {
				c.receiveConfig(ctx, resp.CfgVersion, resp.Config)
			}
			return nil
		} else if regErr != nil {
			c.log.Warn("[l0] 注册请求失败：%v", regErr)
		} else {
			c.log.Warn("[l0] 注册被拒：%s", resp.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) beat() bool {
	status := "healthy"
	if c.configError != "" {
		status = "degraded"
	}
	body, _ := json.Marshal(map[string]any{
		"id":              c.agentID,
		"status":          status,
		"version":         c.version,
		"config_version":  c.appliedVer,
		"applied_version": c.appliedVer,
		"stats": map[string]any{
			"reporter": "l0-control", "received_version": c.receivedVer,
			"config_error": c.configError,
		},
	})
	var resp struct {
		Ok            bool            `json:"ok"`
		CfgVersion    int             `json:"config_version"`
		Config        string          `json:"config"`
		ConfigChanged bool            `json:"config_changed"`
		Tasks         []map[string]any `json:"tasks"`
		Error         string          `json:"error"`
	}
	beats := time.Now()
	beatErr := c.post(context.Background(), "/api/agent/heartbeat", body, &resp)
	if m := c.meter; m != nil {
		m.addReportBytes(len(body))
		m.addBeat(beatErr == nil && resp.Ok, time.Since(beats))
	}
	if beatErr != nil {
		c.log.Warn("[l0] 心跳失败：%v", beatErr)
		return false
	}
	if !resp.Ok {
		c.log.Warn("[l0] 心跳被拒：%s", resp.Error)
		return false
	}
	if resp.ConfigChanged && resp.Config != "" {
		c.receiveConfig(context.Background(), resp.CfgVersion, resp.Config)
	}
	// G2 通道退役：心跳拉取的下发任务（agent-action 等）本机执行并回执（通道 A）。
	for _, t := range resp.Tasks {
		c.handleTask(context.Background(), t)
	}
	return true
}

// handleTask 消费一条心跳下拉任务：agent-action 本机执行并回执 /api/l1/task/ack；
// 未知任务类回执 failed 分流 controller，避免任务滞留 pending。
func (c *Client) handleTask(ctx context.Context, raw map[string]any) {
	id, _ := raw["id"].(string)
	if id == "" {
		return
	}
	kind, _ := raw["kind"].(string)
	payload, _ := raw["payload"].(map[string]any)
	began := time.Now()
	if kind != "agent-action" {
		c.ackTask(ctx, id, "controller", "failed", "unsupported kind: "+kind)
		if m := c.meter; m != nil {
			m.addTask(false, time.Since(began))
		}
		return
	}
	action, _ := payload["action"].(string)
	if action == "" {
		c.ackTask(ctx, id, "sagent", "failed", "empty action")
		if m := c.meter; m != nil {
			m.addTask(false, time.Since(began))
		}
		return
	}
	status, note := localAction(action)
	c.ackTask(ctx, id, "sagent", status, note)
	if m := c.meter; m != nil {
		m.addTask(status == "done", time.Since(began))
	}
}

// ackTask 回执一条任务终态到 L0（幂等：L0 侧按 id 落库）。
func (c *Client) ackTask(ctx context.Context, id, executor, status, note string) {
	body, _ := json.Marshal(map[string]any{
		"id": id, "controller_id": c.agentID,
		"status": status, "executor": executor, "note": note,
	})
	var resp struct {
		Ok   bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, "/api/l1/task/ack", body, &resp); err != nil {
		c.log.Warn("[l0] 任务回执失败 %s: %v", id, err)
		return
	}
	c.log.Info("[l0] 任务回执 %s: %s/%s (%s)", id, status, executor, note)
}

func (c *Client) SetConfigApplier(apply func(context.Context, int, string) error) {
	c.applyConfig = apply
}

func (c *Client) receiveConfig(ctx context.Context, version int, content string) {
	c.receivedVer = version
	var err error
	switch {
	case version < 1 || !json.Valid([]byte(content)):
		err = fmt.Errorf("invalid configuration envelope")
	case c.applyConfig == nil:
		err = fmt.Errorf("runtime configuration application is unavailable")
	default:
		err = c.applyConfig(ctx, version, content)
	}
	if err != nil {
		c.configError = err.Error()
		c.log.Warn("[l0] 配置 cfg-%d 未应用：%v", version, err)
		return
	}
	c.configError = ""
	if c.appliedVer != version {
		delete(c.sent, "config_applied")
	}
	c.appliedVer = version
}

// post 通用 POST + JSON 解码。多端点 HA：总是优先主端点（索引 0），主不通按索引依次 fallback；
// 成功命中的端点记录为 curIdx。因此主端点恢复后下一个请求自然回切到主（HA 倾向主端点）。
// 应用层驳回（HTTP 200 但对端返回 ok=false）不视为端点故障——那是对端语义，切换无意义。
func (c *Client) post(ctx context.Context, path string, body []byte, out any) error {
	if len(c.endpoints) == 0 {
		return fmt.Errorf("no l0 endpoints configured")
	}
	var lastErr error
	for idx := 0; idx < len(c.endpoints); idx++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints[idx]+path, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, derr := c.httpClient.Do(req)
		if derr != nil {
			lastErr = derr
			// 当前点滴台账但非主端点才告警一次；主端点故障静默等多端点都有机会试，避免误报
			if idx != 0 && idx == c.curIdx {
				c.log.Warn("[l0] 端点 <-%s> 断连：%v，尝试下一个端点", c.endpoints[idx], derr)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("http %d @ %s", resp.StatusCode, c.endpoints[idx])
			continue
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}
		// 成功：记录命中端点；从备用回切主端点时提示（HA 应倾向主）
		if c.curIdx != 0 && idx == 0 {
			c.log.Info("[l0] 主端点恢复,已回切 <l0=%s>", c.endpoints[0])
		}
		c.curIdx = idx
		if m := c.meter; m != nil {
			m.setEndpoint(c.endpoints[idx])
		}
		return nil
	}
	return lastErr
}

// outboundIP 取本机回连平台的出口 IPv4（注册展示用；失败留空，不阻塞注册）。
// 做法：向平台地址做一次 UDP dial 选路（不发包）——拿到的就是真正回连平台所用的本机地址；
// 平台与本机同机/环回时该值无意义，回落枚举非环回网卡。不再写死任何网关地址
func outboundIP(l0URL string) string {
	if hp := hostPortFor(l0URL); hp != "" {
		if conn, err := net.Dial("udp", hp); err == nil {
			addr, ok := conn.LocalAddr().(*net.UDPAddr)
			_ = conn.Close()
			if ok && addr != nil {
				if ip4 := addr.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsUnspecified() {
					return ip4.String()
				}
			}
		}
	}
	return firstNonLoopbackIPv4()
}

// hostPortFor 从 L0 基址解析 host:port（无端口按 scheme 补 80/443）；解析不出返回空串
func hostPortFor(l0URL string) string {
	u, err := url.Parse(strings.TrimSpace(l0URL))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return ""
	}
	if port := u.Port(); port != "" {
		return net.JoinHostPort(u.Hostname(), port)
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// firstNonLoopbackIPv4 枚举本机非环回 IPv4（无 routable 出口时的兜底）
func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP == nil {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsUnspecified() {
			return ip4.String()
		}
	}
	return ""
}
