package main

import (
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// Agent 代表一个 SAgent 实例
type Agent struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Type         string                 `json:"type"` // edge 或 proxy
	Status       string                 `json:"status"`
	Host         string                 `json:"host"`
	Port         int                    `json:"port"`
	Plugins      []string               `json:"plugins"`
	Version      string                 `json:"version"`
	Labels       map[string]string      `json:"labels"`
	UpdatedAt    time.Time              `json:"updated_at"`
	Source       string                 `json:"source,omitempty"`        // docker: 本机演示; heartbeat: 注册接入
	LastSeen     int64                  `json:"last_seen,omitempty"`     // 最近心跳 unix 秒
	CfgDesired   int                    `json:"cfg_desired,omitempty"`   // 服务端期望配置版本
	CfgEffective string                 `json:"cfg_effective,omitempty"` // Agent 上报的生效配置版本
	Stats        map[string]interface{} `json:"stats,omitempty"`         // 心跳携带的采集成败统计
	TenantID     string                 `json:"tenant_id,omitempty"`     // 多租户(D3)：归属租户，default=默认租户
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
			Status: "unknown", Host: "sagent-1", Port: agentHTTPPortNum(),
			Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"},
			TenantID: "default"},
		{ID: "sagent-2", Name: "Edge Collector 2", Type: "edge",
			Status: "unknown", Host: "sagent-2", Port: agentHTTPPortNum(),
			Plugins: []string{"host_metrics", "log_metrics", "custom_scripts"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"},
			TenantID: "default"},
		{ID: "sagent-proxy", Name: "Collector Proxy", Type: "proxy",
			Status: "unknown", Host: "sagent-proxy", Port: agentHTTPPortNum(),
			Plugins: []string{"mysql_probe"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-a", "env": "prod"},
			TenantID: "default"},
		// 多租户(D3)演示：两台边缘采集器归属 tenant-a，用于界面租户切换隔离演示
		{ID: "sagent-3", Name: "租户A · Edge Collector", Type: "edge",
			Status: "unknown", Host: "sagent-3", Port: agentHTTPPortNum(),
			Plugins: []string{"host_metrics", "custom_scripts"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-b", "env": "prod"},
			TenantID: "tenant-a"},
		{ID: "sagent-4", Name: "租户A · Log Collector", Type: "edge",
			Status: "unknown", Host: "sagent-4", Port: agentHTTPPortNum(),
			Plugins: []string{"log_metrics"},
			Version: "v0.4.0", Labels: map[string]string{"idc": "idc-b", "env": "prod"},
			TenantID: "tenant-a"},
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

// Remove 注销一个 Agent（重装/换机重新接入前清身份用）。
// 只清内存台账，DB 行由 DeleteAgent 负责
func (s *AgentStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, id)
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

func rowToAgent(r *storepkg.AgentRow) *Agent {
	st := "healthy"
	// 空租户视为 default：注册链路不带租户信息，默认归属默认租户（D3 兼容存量）
	tid := r.TenantID
	if tid == "" {
		tid = "default"
	}
	return &Agent{
		ID: r.ID, Name: r.Name, Type: r.Type, Host: r.Host, Port: r.Port,
		Plugins: r.Plugins, Version: r.Version, Labels: r.Labels,
		Source: r.Source, LastSeen: r.LastSeen, CfgDesired: r.CfgDesired,
		CfgEffective: r.CfgEffective, Stats: r.Stats, Status: st, TenantID: tid,
	}
}

// ensureSystemPlugins 系统内置插件行引导（host_metrics/docker_metrics/custom_scripts）。
// 这三类插件的指标定义走 data/metrics.json 种子（保留 grp/level/phase 等治理字段，
// integrations 包格式不带），但其插件行此前依赖手工 SQL，删库重建后会丢失；
// 启动时按需补齐，已存在则跳过（不覆盖用户编辑）。
