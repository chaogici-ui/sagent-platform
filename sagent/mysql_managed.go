package main

// mysql_managed.go —— R0-1 Agent 托管目标运行时（mysql_probe）。
//
// 语义（SPEC-Agent托管目标与预热接管 §4，本期范围 R0-1，不含 standby/R6）：
//   - 平台 targets 段是采集任务的唯一期望源：applyPlatformConfig 解析 mysql_probe 条目
//     转成目标集，经 Apply() reconcile 到数据面（起 exporter 进程 / 增删 scrape 目标）；
//   - 单归属边界不变：active 唯一性由 L0 保证（targets.agent_id），Agent 只忠实执行，
//     不自主判定属主、不自主起停非平台声明的目标；
//   - 静态 SAgent.yaml 只作引导（装机自愈兜底）：平台配置一旦到达（managed=true），
//     目标集以平台为准；平台配置到达前维持静态采集，行为与老版本完全一致；
//   - 失败自愈 + 回滚闭包沿用 ConfigApplier 语义：apply 失败内部恢复上一版，
//     返回的闭包供配置落盘失败时外部回滚。
//
// 二进制获取（插件超市 → package-cache，用户裁决本期落地）：
//   ① 静态配置 exporter_bin 存在 → 直接用（老装机包向后兼容）；
//   ② 本地 package-cache（data/packages/plugins/<name>/<version>/）命中且 sha256 一致 → 用；
//   ③ 缓存未命中 → 从 L0 插件超市（/api/plugin-packages/...）下载 → sha256 校验 → 落缓存。
//   校验不过一律拒绝拉起（红线：不降级用未校验的二进制）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sagent/core/internal/config"
	"github.com/sagent/core/internal/constants"
	"github.com/sagent/core/internal/logger"
	"github.com/sagent/core/internal/pipeline"
	"github.com/sagent/core/internal/plugin/builtin"
)

// 插件包名映射：插件 → L0 插件超市中的包名
var pluginPackageNames = map[string]string{
	constants.PluginNameMySQLProbe: "mysql-exporter",
	constants.PluginNameRedisProbe: "redis-exporter",
}

type mysqlProbeRuntime struct {
	mu        sync.Mutex
	probe     *builtin.MySQLProbe
	staticCfg config.MySQLProbeConfig
	metricsCh chan builtin.MetricBatch
	log       *logger.Logger
	current   []config.MySQLTarget // 当前生效目标集（managed 后 = 平台口径，含 standby 预置）
	managed   bool                 // 平台是否已接管（首个平台配置到达后恒真）
	l0Base    string               // L0 插件超市基址（cfg.L0Console URL/Endpoints 主端点）
	dataDir   string               // SAgent data 目录（package-cache 根的父目录）
	workDir   string               // SAgent 工作目录（静态相对路径的基准）
	pipeline  *pipeline.Pipeline   // 快照源清理（目标移除时 RemoveSource，防旧属主残留序列双写）
	standby   map[string]*standbyState // R0-2：standby 预置态台账（key=resource_id）
}

// standbyState 一个 standby 预置目标的就绪状态（R0-2 observed_state 回报口径）。
// 语义：配置与版本包就位、连通性已验证，但采集任务不启动——预置的是配置副本，不是进程副本。
type standbyState struct {
	DSN       string `json:"dsn"`
	Reachable bool   `json:"reachable"`   // TCP 连通性最近一次验证结果
	CheckedAt int64  `json:"checked_at"`  // 最近验证时间（unix 秒）
	Note      string `json:"note"`        // 验证结论（空 = 连通正常）
}

func newMySQLProbeRuntime(cfg *config.Config, metricsCh chan builtin.MetricBatch, pl *pipeline.Pipeline, log *logger.Logger) *mysqlProbeRuntime {
	l0 := cfg.L0Console.Endpoints
	if l0 == "" {
		l0 = cfg.L0Console.URL
	}
	// 多端点取主端点（下载走主，与控制通道 HA 倾向一致）
	if i := strings.IndexAny(l0, ",; "); i > 0 {
		l0 = l0[:i]
	}
	dataDir := cfg.L0Console.DataDir
	if dataDir == "" {
		dataDir = constants.DefaultDataDir
	}
	return &mysqlProbeRuntime{
		staticCfg: cfg.Plugins.MySQLProbe,
		metricsCh: metricsCh,
		log:       log,
		l0Base:    strings.TrimRight(l0, "/"),
		dataDir:   dataDir,
		workDir:   ".",
		pipeline:  pl,
	}
}

func (r *mysqlProbeRuntime) PluginName() string { return constants.PluginNameMySQLProbe }

func (r *mysqlProbeRuntime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probe != nil && r.probe.Running()
}

// StartStatic 静态引导（yaml 配置驱动）：平台配置到达前的行为与老版本一致
func (r *mysqlProbeRuntime) StartStatic() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.staticCfg.Enabled || len(r.staticCfg.Targets) == 0 {
		return nil
	}
	r.current = cloneTargetsB(r.staticCfg.Targets)
	return r.startProbeLocked(r.current)
}

// Start 实现 PluginHandle（ctl start mysql_probe 恢复场景）：按当前目标集拉起（仅 active，standby 预置不启动）
func (r *mysqlProbeRuntime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := activeTargets(r.current)
	if len(active) == 0 {
		return errors.New("mysql_probe: no managed targets to start")
	}
	return r.startProbeLocked(active)
}

// Stop 实现 PluginHandle（ctl stop mysql_probe / 进程组清理）
func (r *mysqlProbeRuntime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopProbeLocked()
}

// Signal 实现 PluginHandle（reload = 按当前目标集重启采集）
func (r *mysqlProbeRuntime) Signal(sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopProbeLocked(); err != nil {
		return err
	}
	active := activeTargets(r.current)
	if len(active) == 0 {
		return nil
	}
	return r.startProbeLocked(active)
}

// Apply 平台托管目标 reconcile（applyPlatformConfig 调用）。
// R0-2 语义：目标集按 desired_state 分区——active 进 scrape 运行时，standby 只做
// 校验 + 版本包就位 + 连通性验证（不启动任务）。返回回滚闭包（恢复到本调用前的目标集）；
// 失败时内部已自愈回上一版。
func (r *mysqlProbeRuntime) Apply(ctx context.Context, next []config.MySQLTarget) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := cloneTargetsB(r.current)
	prevManaged := r.managed
	prevStandby := r.standby
	r.managed = true
	r.current = cloneTargetsB(next)
	r.standby = map[string]*standbyState{}
	for i := range next {
		if next[i].IsStandby() {
			r.standby[next[i].ResourceID] = &standbyState{DSN: next[i].DSN}
		}
	}

	// standby 预置就绪动作（不启动任务）：
	//   ① 版本包就位：ensureBinary 一次（active 与 standby 共用同一 exporter 包）；
	//   ② 连通性验证：对每个 standby 目标做 TCP dial（失败不阻断 apply——预置副本
	//      的意义就是提前把坑暴露出来，reachable=false 会如实体现在 observed_state）。
	if len(next) > 0 {
		if _, err := r.ensureBinary(); err != nil {
			r.rollbackLocked(prev, prevManaged, prevStandby)
			return nil, fmt.Errorf("mysql_probe exporter 二进制不可用: %w", err)
		}
	}
	for i := range next {
		if !next[i].IsStandby() {
			continue
		}
		st := r.standby[next[i].ResourceID]
		if hp := mysqlDSNHostPort(next[i].DSN); hp == "" {
			st.Note = "DSN 无法解析出 host:port，跳过连通性验证"
		} else {
			conn, derr := net.DialTimeout("tcp", hp, 3*time.Second)
			if derr != nil {
				st.Reachable = false
				st.Note = "连通性验证失败: " + derr.Error()
			} else {
				conn.Close()
				st.Reachable = true
			}
		}
		st.CheckedAt = time.Now().Unix()
	}

	active := activeTargets(next)
	if err := r.reconcileLocked(active); err != nil {
		// 失败自愈：恢复上一版目标集（ConfigApplier 语义：apply 失败不留半态）
		r.rollbackLocked(prev, prevManaged, prevStandby)
		return nil, err
	}
	r.log.Info("mysql_probe 托管目标已生效（active %d 个 / standby 预置 %d 个）", len(active), len(r.standby))
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.current = prev
		r.managed = prevManaged
		r.standby = prevStandby
		return r.reconcileLocked(activeTargets(prev))
	}, nil
}

// rollbackLocked 失败自愈：恢复上一版目标集（含 standby 台账）
func (r *mysqlProbeRuntime) rollbackLocked(prev []config.MySQLTarget, prevManaged bool, prevStandby map[string]*standbyState) {
	r.current = prev
	r.managed = prevManaged
	r.standby = prevStandby
	if rbErr := r.reconcileLocked(activeTargets(prev)); rbErr != nil {
		r.log.Error("mysql_probe 回滚上一版目标集失败: %v", rbErr)
	}
}

// reconcileLocked 目标集落地（入参为 active 子集，standby 预置不进此路径）：
// 空集 → 全停；非空 → 起进程（幂等）/ 热更目标
func (r *mysqlProbeRuntime) reconcileLocked(next []config.MySQLTarget) error {
	if r.probe == nil {
		if len(next) == 0 {
			return nil // 本来就没跑，空集 no-op
		}
		return r.startProbeLocked(next)
	}
	// probe 已存在：ApplyTargets 内部做幂等 diff（含任务清零回收进程）
	_, err := r.probe.ApplyTargets(next)
	return err
}

// startProbeLocked 解析二进制（package-cache get）→ 创建并启动 probe
func (r *mysqlProbeRuntime) startProbeLocked(targets []config.MySQLTarget) error {
	if len(targets) == 0 {
		return nil
	}
	bin, err := r.ensureBinary()
	if err != nil {
		return fmt.Errorf("mysql_probe exporter 二进制不可用: %w", err)
	}
	probe := builtin.NewMySQLProbe(config.MySQLProbeConfig{Enabled: true, Targets: cloneTargetsB(targets)})
	if r.pipeline != nil {
		probe.SetSourceRemover(r.pipeline.RemoveSource)
	}
	if err := probe.StartWith(bin, r.metricsCh); err != nil {
		return fmt.Errorf("start mysql_probe: %w", err)
	}
	r.probe = probe
	r.log.Info("mysql_probe started（bin=%s, %d targets）", bin, len(targets))
	return nil
}

func (r *mysqlProbeRuntime) stopProbeLocked() error {
	if r.probe == nil {
		return nil
	}
	err := r.probe.Stop()
	r.probe = nil
	return err
}

// ---------- 插件包获取：静态路径 → 本地 package-cache → L0 插件超市 ----------

type pluginManifest struct {
	OK       bool   `json:"ok"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Error    string `json:"error"`
}

// ensureBinary 解析可用二进制：静态路径 → 本地缓存（校验）→ L0 插件超市下载（校验后落缓存）
func (r *mysqlProbeRuntime) ensureBinary() (string, error) {
	// ① 静态配置的装机路径（向后兼容：老装机包已捆绑二进制）
	for _, p := range []string{r.staticCfg.ExporterBin, constants.DefaultMysqlExportBin} {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.workDir, p)
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	// ②③ 插件超市路径（mysql_probe → mysql-exporter 包）
	pkgName, ok := pluginPackageNames[constants.PluginNameMySQLProbe]
	if !ok || r.l0Base == "" {
		return "", errors.New("静态路径无二进制且未配置 L0 插件超市地址")
	}
	mani, err := r.fetchManifest(pkgName)
	if err != nil {
		return "", fmt.Errorf("获取插件包清单失败: %w", err)
	}
	cachePath := filepath.Join(r.dataDir, "packages", "plugins", pkgName, mani.Version, mani.Filename)
	// ② 本地 package-cache 命中且 sha256 一致 → 直接用（就近取包）
	if sha, err := sha256FileB(cachePath); err == nil && sha == mani.SHA256 {
		return cachePath, nil
	}
	// ③ 从 L0 插件超市下载 → sha256 校验 → 落缓存（校验不过一律拒绝，不降级用未校验包）
	if err := r.downloadPackage(pkgName, mani, cachePath); err != nil {
		return "", err
	}
	r.log.Info("插件包 %s v%s 已入本地 package-cache（sha256 校验通过）", pkgName, mani.Version)
	return cachePath, nil
}

// fetchManifest / downloadPackage：mysql 运行时委托到共用自由函数（redis 运行时同源复用）
func (r *mysqlProbeRuntime) fetchManifest(pkgName string) (*pluginManifest, error) {
	return fetchPluginManifest(r.l0Base, pkgName)
}

func (r *mysqlProbeRuntime) downloadPackage(pkgName string, mani *pluginManifest, cachePath string) error {
	return downloadPluginPackage(r.l0Base, pkgName, mani, cachePath)
}

// fetchPluginManifest 从 L0 插件超市取包清单（sha256 为下载校验的唯一凭据）
func fetchPluginManifest(l0Base, pkgName string) (*pluginManifest, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(l0Base + "/api/plugin-packages/" + urlPathEscape(pkgName) + "/manifest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var mani pluginManifest
	if err := json.NewDecoder(resp.Body).Decode(&mani); err != nil {
		return nil, err
	}
	if !mani.OK || mani.Version == "" || mani.Filename == "" || len(mani.SHA256) != 64 {
		return nil, fmt.Errorf("插件包清单无效: %s", mani.Error)
	}
	return &mani, nil
}

// downloadPluginPackage 从 L0 插件超市下载 → sha256 校验 → 落缓存（校验不过一律拒绝，不降级）
func downloadPluginPackage(l0Base, pkgName string, mani *pluginManifest, cachePath string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(l0Base + "/api/plugin-packages/" + urlPathEscape(pkgName) + "/download?version=" + urlPathEscape(mani.Version))
	if err != nil {
		return fmt.Errorf("下载插件包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载插件包失败: http %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	tmp := cachePath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("下载插件包中断: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != mani.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("插件包 sha256 校验失败（want %s… got %s…），拒绝使用", mani.SHA256[:12], got[:12])
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------- 小工具 ----------

func urlPathEscape(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "/", "_") }

func sha256FileB(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func cloneTargetsB(targets []config.MySQLTarget) []config.MySQLTarget {
	out := make([]config.MySQLTarget, len(targets))
	copy(out, targets)
	return out
}

// activeTargets 过滤出 desired_state=active 的目标（R0-2：standby 预置不进 scrape 运行时）
func activeTargets(targets []config.MySQLTarget) []config.MySQLTarget {
	out := make([]config.MySQLTarget, 0, len(targets))
	for _, t := range targets {
		if !t.IsStandby() {
			out = append(out, t)
		}
	}
	return out
}

// mysqlDSNHostPort 从 DSN 提取 host:port（user:pass@tcp(host:port)/... 形态）；解析不出返回空串
func mysqlDSNHostPort(dsn string) string {
	i := strings.Index(dsn, "tcp(")
	if i < 0 {
		return ""
	}
	rest := dsn[i+len("tcp("):]
	j := strings.Index(rest, ")")
	if j < 0 {
		return ""
	}
	hp := strings.TrimSpace(rest[:j])
	if _, _, err := net.SplitHostPort(hp); err != nil {
		return ""
	}
	return hp
}

// ObservedTargets R0-2 observed_state 回报口径（心跳 stats.observed_targets）：
// 按 resource_id 给出每个托管目标的实际状态。active 目标看 probe 运行态；
// standby 预置看就绪台账（版本包就位 + 最近连通性验证）。
// 供 L0 聚合到界面"进程状态"列——看进程不再需要进主机 ps。
func (r *mysqlProbeRuntime) ObservedTargets() map[string]map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]map[string]any{}
	procRunning := r.probe != nil && r.probe.Running()
	for _, t := range r.current {
		entry := map[string]any{"state": "active"}
		if t.IsStandby() {
			entry["state"] = "standby"
			if st := r.standby[t.ResourceID]; st != nil {
				entry["reachable"] = st.Reachable
				entry["checked_at"] = st.CheckedAt
				if st.Note != "" {
					entry["note"] = st.Note
				}
			}
		} else {
			entry["proc"] = procRunning
		}
		out[t.ResourceID] = entry
	}
	return out
}
