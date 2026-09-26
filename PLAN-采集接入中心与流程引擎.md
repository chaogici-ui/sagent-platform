# PLAN — 采集接入中心与流程引擎

> 面向 aicoding 的设计与实施手册。每一项都给出了**精确文件路径、表名、函数签名、验收命令**，可逐条执行、逐条验证。
> 文档中的行号/路由/表结构均以当前仓库实测为准（基线提交 `de170af` 之后的 `b0cc643` 分层拆分版）。

---

## 0. aicoding 执行须知（先读这一节）

### 0.1 仓库基线

| 模块 | 路径 | 语言 | 说明 |
|---|---|---|---|
| 控制面 | `l0-console/` | Go 1.25 + PostgreSQL | 平台前端 + API |
| 数据面 | `sagent/` | Go 1.26 单二进制 | 采集 Agent |
| 部署 | `deploy/docker/` | docker compose | 运行态全部在容器内 |
| 回归 | `tests/run_all.sh` | Bash | 61 项门禁 |

### 0.2 每次改动的强制门禁（一条都不能跳）

```bash
# ① 静态 + 双平台 + 单测（GOPROXY 走国内镜像；go 二进制必须锚定，见 0.4）
cd /Users/gici/代码/SAgent/code/l0-console
export GOPROXY=https://goproxy.cn,direct
go vet ./... && \
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./ && \
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./ && \
go test ./...

# ② 重建部署
cd /Users/gici/代码/SAgent/code/deploy/docker
PATH=/usr/local/bin:/opt/homebrew/bin:$PATH DOCKER_BUILDKIT=0 docker compose build l0-console
PATH=/usr/local/bin:/opt/homebrew/bin:$PATH docker compose up -d l0-console

# ③ 全量回归（约 3.5 分钟）
cd /Users/gici/代码/SAgent/code && bash tests/run_all.sh
```

### 0.3 硬性约束（违反即回滚）

1. **只改本批次列出的文件**。不得顺手优化相邻文件。
2. **持久化一律目录库（PostgreSQL）**。禁止把运行态业务数据写进 JSON 文件、禁止内存 map 当存储、禁止手工 SQL 编辑运行库。
3. **环境值一律 env**，集中登记在 `main.go` 的 `cfg*` 变量块。默认值只允许用于本地开发。
4. **演示数据必须显式开关**（`SEED_DEMO_AGENTS=1`），生产部署留空。
5. **前端不写死产品语义**：插件清单、模板、参数表单、检查项一律来自后端数据。
6. **新增数据文件必须同时加进 `Dockerfile` 与 `/opt/l0-seed/`**（否则会被 data 卷遮蔽，见 `docker-entrypoint.sh`）。
7. **依赖安装**用 `~/.workbuddy/bin/workbuddy-install`，禁止裸 `npm install`。

### 0.4 三个已踩过的坑（务必避开）

| 坑 | 现象 | 规避 |
|---|---|---|
| 双 go 工具链 | `compile: version "go1.26.2" does not match go tool version "go1.26.7"` | 脚本内 `GO_BIN="$(command -v go)"` 在改 PATH **之前**锚定 |
| data 卷遮蔽 | 新数据文件进不了已存在的卷 | 种子文件双份 COPY 到 `/opt/l0-seed/` + entrypoint 同步 |
| Edit 工具静默丢编辑 | 同文件连续快速编辑有一笔不落盘 | 改完 grep 校验；大文件用 python 脚本切片 |

### 0.5 每个批次的完成条件检查清单（强制）

- [ ] 代码改动**仅限本批列出的文件**
- [ ] `go vet` + 双平台交叉编译 + `go test ./...` 全过
- [ ] 镜像重建部署，容器内端点实测通过
- [ ] `tests/run_all.sh` **全绿**（无 FAIL）
- [ ] 新增 lint 断言已加入 `run_all.sh` 第 9 节
- [ ] 演示数据已恢复
- [ ] 本次踩坑教训已写入 `.workbuddy/memory/YYYY-MM-DD.md`

---

## 1. 背景：要解决的四个真问题

| # | 问题 | 证据 |
|---|---|---|
| P1 | **接入过程不可见**。点完"接入"就黑盒，卡在哪、报什么错、上报了什么数据全看不见 | `/api/targets/{id}/test` 实测 `lookup l1-mysql-1 on 127.0.0.11:53: no such host`；Agent 详情显示「期望 cfg-1 / 生效 None」 |
| P2 | **接入流程写死在代码里**。步骤、表单、文案硬编码，加一个插件要改 JS | `panel.js:14-43` 全是 `h += '① ...'` 字符串拼接 |
| P3 | **插件生命周期缺环**。只能服务端扫目录，没有新增/导入 | `openAddPluginModal()`（panel.js:1913）零调用死代码；`catalog_api.go` 无创建路由 |
| P4 | **数据面自动接线缺失**。Agent 侧不会调平台，只能人工 curl 模拟 | `sagent` 全仓无 l0-console HTTP 调用；`constants.DefaultL0ConsolePort = 8080` 定义了零引用 |

**目标**：把「接入」从一次性动作升级为**可观测、可配置、可复用**的能力装配过程。

---

## 2. 核心设计原则

### 2.1 引擎与配置的边界（最重要的一条）

**写死在 Go 里的是原子能力（稳定不变），数据化的是流程编排与参数（随插件变）。**

```
┌──────────────────────────────────────────────────────┐
│ L3 · 插件声明    data/integrations/<plugin>/          │
│   params.yaml   参数表单（字段/类型/校验/探路绑定）      │
│   preflight/    checks.yaml + playbook.yml + README   │
│   flow.yaml     可选：仅当流程与模式模板不同才声明       │
├──────────────────────────────────────────────────────┤
│ L2 · 模式模板    data/flow_templates/<mode>.yaml       │
│   edge.yaml / remote.yaml / hybrid.yaml               │
│   定义原子步骤的编排顺序 + 条件（when）                 │
├──────────────────────────────────────────────────────┤
│ L1 · 原子能力    Go 内置（~12 个，所有插件共用）         │
│   pick_object / preflight_host / install_agent / ...   │
└──────────────────────────────────────────────────────┘
```

**验收标准（用它检验有没有做死）**：接入一个新的开源 exporter = `data/integrations/` 加 1 个目录 + 5 个文件，**不改一行 Go、不改一行 JS**。

### 2.2 为什么模板按「模式」而不按「插件」

MySQL 与 Redis 的接入流程**完全一致**（选对象→探路→装 Agent→填参数→下发→验证），差异 100% 在参数与检查项。

- 按插件出模板 = 25 份几乎相同的 YAML，第 26 个插件改 26 处 ❌
- 按模式出模板 = 3 份，插件只提供声明文件 ✅

### 2.3 探路分两段（P1 / P2）

| 段 | 执行者 | 时机 | 产出 | 决定什么 |
|---|---|---|---|---|
| **P1 主机探路** | 外部执行器（ansible/ssh，目标机零依赖） | **装 Agent 之前** | 实测 OS / 架构 / 内核 / 磁盘 / 端口 / 是否已有 Agent | **装哪个 SAgent 镜像 tag**、能不能装 |
| **P2 能力探路** | Agent 本机内置检查器 | 装完 Agent、下发配置之前 | exporter 端口占用 / 依赖 / 账号权限 / NTP 偏差 | 这个插件能不能采、阻断还是告警 |

**为什么必须分两段**：装 Agent 之前目标机上没有我方任何进程，P1 只能由外部执行器跑——正因如此，它天然是 **OS/架构的唯一可靠来源**。资源台账里带 OS/架构时只做**预填 + 一致性校验**（实测 ≠ 登记 → 提示资源信息可能过期），**实测值权威**。

### 2.4 ansible 是「表达层」而非「唯一执行层」

| 方案 | 判断 |
|---|---|
| A 平台侧 ansible（容器 SSH 到目标） | ❌ 平台要存目标机凭据（安全债）+ 容器↔目标网不通（P1 已实证） |
| B Agent 强绑 ansible | ❌ sagent 是 Go 单二进制，强绑 Python 毁掉零依赖部署 |
| **C 双轨（采纳）** | ✅ 声明层 YAML/ansible，执行层可插拔 |

C 的落地：`preflight/checks.yaml`（平台读，一份声明同时驱动界面渲染 + 门禁判定 + playbook 生成）、`preflight/playbook.yml`（运维可脱离平台手工跑，**断网环境可用**）、`preflight/README.md`（每项失败怎么修）。

两条执行路径**并存**：自动（平台下发 → Agent 内置检查器 → 结构化回报）与手工（生成 zip 探路包 → 运维 `ansible-playbook` → JSON 回传平台）。

### 2.5 探路与拨测合并为同一个执行器

两者本质同构——**下发任务 → 执行 → 结构化回报**。批 C 做成一个执行器承载两类任务，顺带修掉「拨测方向错位」：

```json
{ "task_id": "t-01", "type": "preflight | probe", "target": "mysql-on-demo", "spec": {...} }
```

### 2.6 混合场景的定序规则

| 序 | 理由 |
|---|---|
| 1. Agent 必须先装 | 不装它，后续所有平台侧动作无处落地 |
| 2. 自监控紧跟 Agent、先于任何插件 | 它是"这台机器活着"的唯一证据，也是后续步骤的可观测性前提 |
| 3. 边缘能力与远程能力无依赖 | 都是"往已有 Agent 上加能力"，界面允许一次勾选多个，后端展开为并行分支 |
| 4. 二次进入跳过地基段 | 这就是"接入过"分支 |

`resources.role` 分两类，**硬规则**：

| role | 是什么 | host_metrics |
|---|---|---|
| `business` | 业务对象（被采的 MySQL/Redis 主机） | 可选（对应"可以不选只装 SAgent"） |
| `platform_device` | **承载平台能力的机器**，含所有代理机 | **强制注入、不可关闭** |

代理机一旦被选用，平台自动登记为 `platform_device` 并强制带自监控——它不属于任何业务系统，是采集平台自己的设备，平台必须能看见它。

---

## 3. 概念模型与数据模型

### 3.1 概念关系

```
resources (资源对象)
   │  1
   │
   │  n
onboard_flow (一次接入实例) ──1:n──> onboard_event (每步状态/耗时/原始数据)
   │
   ├── mode:   edge | remote | hybrid
   ├── abilities: [host_metrics, mysql_probe, log_metrics, custom_scripts, ...]
   ├── agent_id: 承载能力的 Agent
   └── proxy_agent_id: 远程采集的代理机

agents (Agent 台账)  ←── 由 register/heartbeat 维护
targets (采集目标)   ←── 由 onboard_flow 完成后自动创建
```

### 3.2 新增表 DDL（写入 `l0-console/store/onboard.go`，新文件）

```sql
-- 资源对象：接入流程的第①步落点
CREATE TABLE IF NOT EXISTS resources (
  id            TEXT PRIMARY KEY,           -- 资源 ID（手工填 / CMDB / Agent 自注册）
  name          TEXT DEFAULT '',
  ip            TEXT DEFAULT '',
  resource_type TEXT DEFAULT 'host',        -- host | mysql | redis | kafka | ...
  role          TEXT DEFAULT 'business',    -- business | platform_device
  os            TEXT DEFAULT '',            -- 探路回填（实测值权威）
  arch          TEXT DEFAULT '',
  kernel        TEXT DEFAULT '',
  source        TEXT DEFAULT 'manual',      -- manual | cmdb | import | agent
  labels_json   TEXT DEFAULT '{}',
  probe_json    TEXT DEFAULT '{}',          -- 最近一次 P1 探路原始结果
  probed_at     TEXT DEFAULT '',
  created_at    TEXT DEFAULT (datetime('now','localtime')),
  updated_at    TEXT DEFAULT (datetime('now','localtime'))
);

-- 接入实例：一个资源 × 一种模式 × 一组能力 = 一条流水线
CREATE TABLE IF NOT EXISTS onboard_flow (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  resource_id    TEXT NOT NULL,
  resource_ip    TEXT DEFAULT '',
  mode           TEXT DEFAULT 'edge',       -- edge | remote | hybrid
  template_id    TEXT DEFAULT '',           -- 模式模板 id
  abilities_json TEXT DEFAULT '[]',         -- 勾选的能力（插件名数组）
  agent_id       TEXT DEFAULT '',           -- 承载能力的 Agent（代理机）
  current_step   TEXT DEFAULT '',           -- 当前原子步骤 id
  status         TEXT DEFAULT 'running',    -- running|done|failed|blocked|stalled|canceled
  started_at     TEXT DEFAULT (datetime('now','localtime')),
  finished_at    TEXT DEFAULT '',
  updated_at     TEXT DEFAULT (datetime('now','localtime'))
);

-- 事件：每步的状态/耗时/原始数据（接入中心的数据地基）
CREATE TABLE IF NOT EXISTS onboard_event (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  flow_id     INTEGER NOT NULL,
  step        TEXT NOT NULL,                -- 原子步骤 id
  status      TEXT NOT NULL,                -- pending|running|ok|fail|skipped|blocked
  scope       TEXT DEFAULT '',              -- platform | agent | external
  started_at  TEXT DEFAULT '',
  finished_at TEXT DEFAULT '',
  duration_ms INTEGER DEFAULT 0,
  detail      TEXT DEFAULT '',              -- JSON：报错原文/实测值/期望值/上报样本/配置快照
  created_at  TEXT DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_onboard_event_flow ON onboard_event(flow_id, id);
CREATE INDEX IF NOT EXISTS idx_onboard_flow_status ON onboard_flow(status, updated_at);
```

### 3.3 扩列（`store/fleet.go` 的 `InitFleet` 内追加，用 `ALTER TABLE ... ADD COLUMN` 且忽略已存在错误）

```sql
-- targets 扩列：现在只有 name/type/address/agent_id/plugin/note，无参数
ALTER TABLE targets ADD COLUMN params_json  TEXT DEFAULT '{}';  -- 端口/口令/频率/采集项
ALTER TABLE targets ADD COLUMN resource_id  TEXT DEFAULT '';    -- 关联资源
ALTER TABLE targets ADD COLUMN flow_id      INTEGER DEFAULT 0;  -- 由哪条接入流水线创建

-- agents 扩列：只补「关联资源」一列
ALTER TABLE agents ADD COLUMN resource_id TEXT DEFAULT '';      -- 关联资源对象
```

> **实现期修正（先核对再动手）**：原计划给 agents 加 `applied_version` 与 `role` 两列，
> 核对后取消——`cfg_effective`（注释即「Agent 上报的生效配置 hash/版本」）已承载**生效配置版本**，
> 心跳只需让它真正被 Agent 上报即可；`agents.type`（`edge | proxy`）已承载 **Agent 形态**。
> 新增同义列会制造双份真相。

> 迁移写法必须幂等：加列统一走 `tableColumns` 探测（PG 读 `information_schema.columns`），已存在则跳过；不依赖特定方言的 `duplicate column` 错误串匹配。

### 3.4 store 层方法签名（新增，`store/onboard.go`）

```go
// 资源
func (s *DB) UpsertResource(r *Resource) error
func (s *DB) ListResources(q, role string) ([]*Resource, error)
func (s *DB) GetResource(id string) (*Resource, error)
func (s *DB) UpdateResourceProbe(id, probeJSON, os, arch, kernel string) error

// 流水线
func (s *DB) CreateFlow(f *Flow) (int64, error)
func (s *DB) ListFlows(status string, limit int) ([]*Flow, error)
func (s *DB) GetFlow(id int64) (*Flow, error)
func (s *DB) UpdateFlowStep(id int64, step, status string) error
func (s *DB) UpdateFlowAgent(id int64, agentID string) error

// 事件
func (s *DB) AppendEvent(e *FlowEvent) (int64, error)
func (s *DB) ListEvents(flowID int64) ([]*FlowEvent, error)
func (s *DB) LatestEvent(flowID int64, step string) (*FlowEvent, error)
```

---

## 4. 流程模板引擎

### 4.1 原子能力清单（L1，Go 内置，写死在 `l0-console/onboard_atoms.go`）

| atom | 作用 | scope | 阻断语义 |
|---|---|---|---|
| `pick_object` | 选/建资源对象，拿 IP | platform | — |
| `pick_agent` | 确定承载能力的 Agent（代理机；边缘模式即本机复用） | platform | — |
| `preflight_host` | P1 主机探路（OS/架构/磁盘/端口 + 能力可行性检查一次探完） | external | blocker 不过 → **阻断** |
| `pick_version` | 按探路结果选 SAgent 镜像 tag | platform | — |
| `install_agent` | 生成安装命令 / 下发安装任务 | external | 失败 → 阻断 |
| `pick_ability` | 勾选采集能力（插件多选） | platform | — |
| `collect_params` | 按 `params.yaml` 渲染参数表单 | platform | 必填未填 → 阻断 |
| `preflight_ability` | P2 远程可达性探路（仅 remote/hybrid 的 `preflight_remote` 使用；边缘能力检查已并入 `preflight_host`） | external | blocker 不过 → 阻断 |
| `sync_config` | 渲染配置文档并下发 | platform | — |
| `verify_probe` | 真连一次、取回一条指标 | agent | 失败 → 告警不阻断 |
| `observe_collect` | 采集观测（成功率/样本） | agent | — |
| `register_platform_device` | 登记/更新平台设备角色 | platform | — |

> 只有这 12 个写死在 Go。新增插件不需要新原子能力。

### 4.2 模式模板格式（`l0-console/data/flow_templates/edge.yaml`）

```yaml
id: edge
name: 边缘采集（Agent 装在被采主机）
description: 平台在自己的主机上安装 SAgent，采本机指标
steps:
  - {id: pick_object,    atom: pick_object,    title: 选择资源对象, scope: platform}
  - {id: pick_ability,   atom: pick_ability,   title: 选择采集能力, scope: platform, requires: pick_object}
  - {id: collect_params, atom: collect_params, title: 采集参数,     scope: platform, requires: pick_ability, when: "abilities_requires_params"}
  - {id: preflight_host, atom: preflight_host, title: 主机探路,     scope: external, requires: collect_params, gate: blocker}
  - {id: pick_version,   atom: pick_version,   title: 选择 SAgent 版本, scope: platform, requires: preflight_host}
  - {id: install_agent,  atom: install_agent,  title: 安装 SAgent,  scope: external, requires: pick_version, when: "!agent_exists"}
  - {id: self_metrics,   atom: observe_collect, title: 自监控上报,  scope: agent, requires: install_agent}
  - {id: sync_config,    atom: sync_config,    title: 下发配置,     scope: platform, requires: preflight_host}
  - {id: verify_probe,   atom: verify_probe,   title: 连通验证,     scope: agent, requires: [sync_config, install_agent]}
  - {id: observe_collect, atom: observe_collect, title: 采集入库,   scope: agent, requires: [sync_config, install_agent]}
```

> 定序原则（2026-09-21 起）：**先声明（采什么 + 参数）→ 再探路（主机事实 + 能力可行性一次探完）→ 再选版本装机**。能力可行性检查并入主机探路：装之前就地拦住「装完才发现采不到」，省掉一次「装 + 卸」的代价；需要凭据的账号类检查交由 Agent 在配置下发后验证（探路结论里标注 `delegated`）。

**五阶段视图（2026-09-21 用户评审「步骤过多」的落地方案）**：引擎步骤保持上表粒度不变——每个停等/门禁环节独立留痕、独立可重试，合并引擎步骤会破坏人工决策的停等粒度；改为**展示层归组**，接入中心时间线按 5 个阶段插分组头（聚合组内状态与完成数），列表页进度点 1 阶段 1 点：

| 阶段 | 引擎步骤 | 执行者 | 通过条件 |
|---|---|---|---|
| ① 接入预检 | pick_object → pick_ability → collect_params | 平台 | 资源存在 · 能力匹配 · 参数齐备（接入点随资源登记） |
| ② 主机探路 | preflight_host | 外部执行器 | SSH 通 · OS/架构支持 · 端口/磁盘/内存达标 · **接入点回连可达（硬门禁）** · 能力可行性检查 |
| ③ 安装 Agent | pick_version → install_agent | 外部执行器 | 版本人工选定 · 包校验 · 进程拉起并发出注册请求 |
| ④ Agent 注册与能力上报 | self_metrics | Agent | 平台收到心跳与能力清单（**180s 无进展判失败**，步骤级超时覆盖） |
| ⑤ 配置下发与采集验证 | sync_config → verify_probe → observe_collect | 平台 + Agent | 期望配置注册后自动生效 · 探针通 · 样本入库 |

与外部评审稿的两处差异（有意为之）：**接入点校验放②不放①**——它必须 SSH 上目标机实测，资源选择阶段没有执行通道，①只做台账登记；**能力探路不再单列**——已并入②（见下文修正记录）。超时配套：`timeoutSpecForStep` 支持步骤 ID 覆盖 atom（`self_metrics` 180s idle，与共用同一 atom 的 `observe_collect` 600s 观察周期解耦）。

**`remote.yaml`** 与 edge 的差异：`install_agent` 装的是**代理机**（目标机零侵入），并多 `register_platform_device`；`agent_id` 语义是代理机而非被采主机。

**`hybrid.yaml`**：`install_agent` 后并行两条分支（边缘能力 / 远程能力），界面一次勾选，后端展开。

### 4.3 条件表达式支持（`when` 求值器）

只支持这些变量与算子，**不求值任意表达式**（安全 + 可预测）：

| 变量 | 含义 |
|---|---|
| `agent_exists` | 该资源已有 Agent |
| `mode` | `edge` / `remote` / `hybrid` |
| `abilities_requires_params` | 勾选的能力中存在声明了 `params.yaml` 的 |
| `preflight_passed` | P1 探路 blocker 全过 |

算子：`!` `&&` `||` `==` `!=`，布尔与字符串字面量。

### 4.4 插件参数声明（`l0-console/data/integrations/<plugin>/params.yaml`）

```yaml
plugin: mysql_probe
fields:
  - {name: address,  label: 目标地址, type: string, required: true,  probe: tcp}
  - {name: port,     label: 端口,     type: int,    default: 3306,  probe: tcp}
  - {name: mode,     label: 部署形态, type: enum,   options: [single, cluster], default: single}
  - {name: username, label: 采集账号, type: string, required: true,  probe: mysql_auth}
  - {name: password, label: 口令,     type: secret, required: true}
  - {name: interval, label: 采集频率, type: duration, default: 30s}
```

`probe:` 字段把参数与探路检查项绑定——填了 `address`/`port`，才谈得上探 TCP。

### 4.5 配置渲染器（缺失的关键件）

平台手上的 `agent_config.content` 是简版意图 doc `{targets:[...], host_metrics:{...}}`。

**职责收口（2026-09-21 审计定稿）**：「意图 doc → SAgent.yaml 各插件段」的翻译**独占在 sagent 侧**（`sagent/internal/control/apply.go`，批 C）——SAgent 配置 schema 升级只改 agent 一处，平台不必跟版。`l0-console/onboard_render.go` 的渲染器定位为**展示/验证视图**：产物只进接入中心 sync_config 的事件 detail，让运维看到「这台 Agent 最终会跑成什么样」，不是下发通道。映射关系：

| 目标 plugin | SAgent 配置段 | 字段映射 |
|---|---|---|
| `mysql_probe` | `plugins.mysql_probe.targets[]` | `params.dsn` / `resource_id` |
| `prometheus_scrape` | `plugins.prometheus_scrape.targets[]` | `name`/`url`/`timeout`/`labels` |
| `port_checker` | `plugins.port_checker.targets[]` | `name`/`type`/`address`/`timeout`/`labels` |
| `custom_scripts` | `plugins.custom_scripts.scripts[]` | `name`/`command`/`interval`/`timeout`/`labels` |
| `log_metrics` | `plugins.log_metrics` | `enabled`/`conf_dir` + 生成 Vector 配置 |
| `host_metrics` | `plugins.host_metrics` | `enabled`/`interval`/`groups`/`exclude_metrics` |

---

## 5. 接入中心（页面 + API）

### 5.1 页面设计（`l0-console/static/js/onboard.js`，新文件）

三层结构：

1. **顶部统计条**：接入中 3 / 已完成 12 / 失败 1 / 卡住 2
2. **流水线列表**：每行 = 一个资源 × 模式，8 个圆点即进度
   - 圆点四色：绿=完成 / 橙=进行中 / 红=失败 / 灰空心=未开始
   - 卡住判定（平台侧，不依赖 Agent）：`status=running` 且 `now - updated_at > 停滞阈值`（默认 300s，env `ONBOARD_STALL_SEC`）；**分执行域**——任一 running 分支是 external（人工 ansible 探路/安装，合法耗时以小时计）就不判 stalled，只显示已等待时长；注意并行分支下不能只看 current_step 的域（它是最后一个 running 步）
3. **单条时间线**（点开）：纵向步骤列表，每步可展开看**原始数据**
   - 配置 JSON 快照 / 报错全文 / 拨测结果 / 该 target 在 VM 里的指标样本

### 5.2 API 清单（`l0-console/onboard_api.go`，新文件）

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/onboard/templates` | 模板清单（含渲染后的步骤序列） |
| GET | `/api/onboard/flows?status=` | 流水线列表 + 顶部统计 |
| GET | `/api/onboard/flow?id=` | 单条流水线 + 全部事件 |
| POST | `/api/onboard/flow` | 创建并启动一条接入流水线 |
| POST | `/api/onboard/flow/step` | 推进/重试某一步（`action: retry\|skip\|force`） |
| POST | `/api/onboard/flow/cancel` | 取消 |
| GET | `/api/resources?q=&role=` | 资源查询 |
| POST | `/api/resources` | 新建资源（手填 IP / 资源 ID，支持批量） |
| GET | `/api/onboard/abilities` | 能力清单（含各能力 params 字段声明，前端向导表单数据源） |
| GET | `/api/onboard/preflight/spec?plugin=` | 取该插件的 `checks.yaml`（**批 D**） |
| GET | `/api/onboard/preflight/package?flow_id=` | 下载探路包（zip：playbook + checks + README）（**批 D**） |
| POST | `/api/onboard/preflight/report` | 手工探路 JSON 回传入口（**批 D**） |

### 5.3 Agent 回报协议（扩展心跳，**不新增端点**）

**请求**（在现有 `POST /api/agent/heartbeat` 上扩字段，向后兼容）：

```json
{
  "id": "host-demo-01",
  "version": "v0.4.0",
  "config_version": 3,
  "applied_version": 3,
  "apply_ms": 42,
  "stats": {"success": 18, "fail": 2},
  "targets": [
    { "target": "mysql-on-demo",
      "probe":   {"ok": false, "error": "dial tcp: i/o timeout", "ms": 3002},
      "collect": {"ok": true, "last_ts": 1789955904, "success": 18, "fail": 2},
      "samples": {"mysql_up": 1, "threads_connected": 12} }
  ]
}
```

**响应**（心跳返回中追加 `tasks`，拉取式下发）：

```json
{
  "config_version": 4,
  "interval": 30,
  "ok": true,
  "tasks": [
    { "task_id": "t-01", "type": "preflight", "target": "mysql-on-demo",
      "spec": {"checks": ["tcp_3306", "mysql_auth", "ntp_skew", "disk_free"]} },
    { "task_id": "t-02", "type": "probe", "target": "mysql-on-demo",
      "spec": {"address": "10.20.30.40:3306", "timeout_s": 3} }
  ]
}
```

Agent 执行后用 `POST /api/onboard/task/result` 回报（或随下次心跳带 `task_results[]`）。

---

## 6. 菜单信息架构定稿

`l0-console/static/index.html` 当前 11 项 → 目标 **11 项 / 4 组**（移除 `versions`、`tenants` 2 项；新增 `onboard-center`、`settings` 2 项）：

| 组 | 菜单 | `data-page` | 功能说明 | 变化 |
|---|---|---|---|---|
| **采集能力**<br>定义「采什么」 | 采集插件 | `plugins-mart` | 采集能力库。目录列表；**新增插件**（元数据+采集方式+默认端口）；**导入插件包**；详情五 tab：说明/参数/指标/仪表盘/脚本；「同步上游集成包」 | ➕补新增、导入；✏️改名 |
| | 指标中心 | `metrics-catalog` | 指标治理。五渠道视图、指标 CRUD、VM 导入发现、引用检查 | 保持 |
| **部署与接入**<br>装上去、接上目标 | **接入中心** | `onboard-center` | 🆕 流水线总览 + 单条时间线（本章第 5 节） | 🆕新增 |
| | 安装部署 | `install` | 平台第一入口。选模板 → 生成完整安装命令 → 目标机执行；最近自动注册 | ⬆️从弹窗提级为页面 |
| | Agent 清单 | `agent-list` | Agent 运维主场。列表；**批量操作：升级/重启/停止**；详情五 tab | 🔀原「批量升级」并入 |
| | 采集目标 | `targets` | 目标 CRUD（含**插件参数**）、分派 Agent、连通性拨测 | ➕补参数 |
| **可观测**<br>看数据、看健康 | 分析大屏 | `metrics-browse` | VM 查询可视化 | 保持 |
| | 采集健康 | `slo` | 断采清单、覆盖率、野指标、僵尸 Agent、stale 目标 | ✏️原「SLO 仪表盘」改名 |
| **系统**<br>留痕与配置 | 审计日志 | `audit` | 谁在什么时候改了什么 | 保持 |
| | 任务历史 | `tasks` | 批量/单点操作的执行记录 | 保持 |
| | 系统设置 | `settings` | 租户 / RBAC / 平台参数 | 🔀原「租户管理」收进来（假数据，先隐藏） |

**接入向导从菜单降为动作**：保留采集目标页、插件详情「去接入」、安装部署页 3 个深链入口，但状态机统一（都走 `onboard_flow`），不再各走各的。

> `data-page` 值一旦被前端 `goPage()` 与 `panel.js` 的 `switch` 引用，改名需同步改两处；本轮**只改 `slo` 的显示文案，不改 key**（`slo` → 「采集健康」），`versions` 菜单移除、`tenants` 收进 `settings`。

---

## 7. 实施批次

依赖顺序：**0 → A → B → C → D**。每批都可独立交付、独立验收。

---

### 批 0 — 菜单重排 + 接入中心占位

**改动文件**

| 文件 | 改动 |
|---|---|
| `l0-console/static/index.html` | 菜单分 4 组；新增 `onboard-center`、`install`、`settings`；移除 `versions`、`tenants` |
| `l0-console/static/js/panel.js` | `switch` 增加 `onboard-center` / `install` / `settings` 分支；`renderVersions` 保留函数但不再挂菜单；新增 `renderSettings()` |
| `l0-console/static/js/onboard.js` | 🆕 新建，先只放 `renderOnboardCenter()` 空态 |
| `l0-console/static/js/utils.js` | 无需改（`goPage` 已支持任意 page） |
| `tests/run_all.sh` | 第 9 节加 3 条 lint：菜单项数、菜单分组、`onboard-center` 存在 |

**验收**
```bash
curl -s http://localhost:8080/ | grep -c 'data-page='      # 期望 11
curl -s http://localhost:8080/ | grep -q 'onboard-center'  # 存在
```

---

### 批 A — 数据地基 + 流程模板引擎 + 接入中心

**这是最大的一批，也是整件事的地基。**

#### A1 数据层

| 文件 | 改动 |
|---|---|
| `l0-console/store/onboard.go` | 🆕 三张表 DDL + 资源/流水线/事件三组方法（见 §3.4） |
| `l0-console/store/fleet.go` | `InitFleet` 追加幂等 `ALTER TABLE` 扩列（targets 3 列、agents 3 列） |
| `l0-console/main.go` | 启动引导调用 `catDB.InitOnboard()` |

#### A2 模板引擎

| 文件 | 改动 |
|---|---|
| `l0-console/onboard_engine.go` | 🆕 模板加载（`data/flow_templates/*.yaml`）+ `when` 求值器 + 步骤展开 |
| `l0-console/onboard_atoms.go` | 🆕 12 个原子能力的注册表与执行骨架（平台侧 7 个先实现，agent/external 5 个先置"等待"） |
| `l0-console/onboard_render.go` | 🆕 配置渲染器（§4.5 映射表） |
| `l0-console/data/flow_templates/edge.yaml` | 🆕 |
| `l0-console/data/flow_templates/remote.yaml` | 🆕 |
| `l0-console/data/flow_templates/hybrid.yaml` | 🆕 |
| `l0-console/data/integrations/mysql/params.yaml` | 🆕（先做 mysql 一个样板，其余插件按需补） |
| `l0-console/Dockerfile` | `COPY flow_templates` + 双份 COPY 到 `/opt/l0-seed/` |
| `l0-console/docker-entrypoint.sh` | 种子同步清单加 `flow_templates` |

#### A3 API 层

| 文件 | 改动 |
|---|---|
| `l0-console/onboard_api.go` | 🆕 §5.2 的 11 个路由 |
| `l0-console/main.go` | `main()` 里 `registerOnboardRoutes(mux, store, catDB)`（与 §9.2「新路由一律挂新文件」一致） |
| `l0-console/agent_api.go` | 心跳响应追加 `tasks`（批 A 先返回空数组占位）；心跳请求解析 `applied_version`（经 `cfg_effective` 落库，不新增同义列）/`apply_ms`/`targets[]`；`targets[]` 的事件化落库在批 C 随 tasks 通道接线 |

#### A4 前端

| 文件 | 改动 |
|---|---|
| `l0-console/static/js/onboard.js` | 🆕 统计条 + 流水线列表 + 单条时间线 + 步骤展开原始数据 |
| `l0-console/static/js/panel.js` | `renderTargets` 的目标详情增加「查看接入过程」深链；`openOnboardWizard` 改为读模板渲染（替换 `panel.js:14-43` 硬编码） |
| `l0-console/static/js/utils.js` | `goPage()` 补新页面 case（不单独做 `onboardApi()` 加载器，onboard.js 模块自带 fetch，避免多一层全局态） |

#### A5 门禁

`tests/run_all.sh` 新增：
- lint：`data/flow_templates/` 三模板存在且含 `steps:`
- lint：`params.yaml` 存在
- lint：`onboard_engine.go` 中无插件名硬编码（`grep -q 'mysql\|redis\|kafka' onboard_engine.go` 必须无命中）
- 行为：`GET /api/onboard/templates` 返回 3 个模板
- 行为：`POST /api/resources` + `POST /api/onboard/flow` 后 `GET /api/onboard/flow?id=` 步骤快照完整（步骤数 = 模板步骤数，事件时间线非空）
- 门禁：P1 未过时 `sync_config` 事件状态为 `blocked`

**批 A 的诚实说明**：做完后第 ④ 步（配置已拉取）往右全是橙色「等待中（已等待 Xs）」。这不是界面没做完，而是**如实反映 Agent 侧还没接线**——恰恰是"卡哪了"。这也给了批 B 一个可验收标准：**做完 B，④⑤ 必须变绿**。

---

### 批 B — 安装命令生成器

| 文件 | 改动 |
|---|---|
| `l0-console/onboard_install.go` | 🆕 按探路结果（OS/架构）+ 平台地址生成真安装命令 |
| `l0-console/static/js/panel.js` | `genInstallCmd`（panel.js:57）替换为调后端 |
| `l0-console/static/js/onboard.js` | 时间线 `install_agent` 步骤展示安装命令 + 一键复制 |
| `l0-console/data/onboard_config.json` | 增加 `sa_versions[]`（镜像 tag 清单，按 os/arch 分） |

**生成物形态**（docker 部署铁律：Agent 也走容器）

```bash
docker run -d --name sagent \
  --restart unless-stopped \
  -v /var/lib/sagent:/root/SAgent \
  -e SAGENT_RESOURCE_ID=<资源ID> \
  -e SAGENT_ENV=prod -e SAGENT_IDC=idc-b \
  -e SAGENT_L0_SERVER=http://<平台地址>:8080 \
  <registry>/sagent:<os>-<arch>-v0.4.0
```

**验收**：探路结果 `{os: linux, arch: amd64}` → 命令中含 `linux-amd64-v0.4.0`；复制粘贴到目标机（容器）能起来并自动注册。

---

### 批 C — sagent 接线 + 下发式执行器

**sagent 侧（新增文件，不动已有插件实现）**

| 文件 | 改动 |
|---|---|
| `sagent/internal/control/client.go` | 🆕 控制面客户端：注册 / 心跳 / 拉配置 / 回报 |
| `sagent/internal/control/apply.go` | 🆕 配置应用：平台配置文档 → `SAgent.yaml` 各插件段 → 热重载 |
| `sagent/internal/control/executor.go` | 🆕 任务执行器（`preflight` / `probe` 两类） |
| `sagent/internal/control/preflight.go` | 🆕 内置检查器：tcp / 进程占用 / 磁盘 / NTP 偏差 / 账号权限 |
| `sagent/main.go` | 启动 `control` 协程；`heartbeat()`（现仅打日志）改为调控制面 |
| `sagent/internal/constants/constants.go` | 启用 `DefaultL0ConsolePort`；新增 `EnvL0Server`、`DefaultHeartbeat` 已存在 |

**l0-console 侧**

| 文件 | 改动 |
|---|---|
| `l0-console/agent_api.go` | 心跳 handler 落地 `applied_version`、`targets[].collect/samples`；下发 `tasks` |
| `l0-console/onboard_api.go` | 新增 `POST /api/onboard/task/result` |
| `l0-console/agent_api.go` | `/api/targets/{id}/test` 改为**下发式**（平台不再本地 dial），保留一个 `?local=1` 快速自检 |

**关键约束**：`sagent` 必须保持**零外部依赖单二进制**——不得引入 ansible/Python。

**验收（可自动化的端到端）**
```bash
# 起一个容器化 sagent（本机 Docker），指向平台
# 期望：注册 → 心跳 → 拉配置 → 应用 → 回报 applied_version
curl -s http://localhost:8080/api/agents | jq '.[]|select(.name=="sagent-it-01")|{cfg_desired,applied_version}'
# 期望 cfg_desired == applied_version（实测的「生效 None」消失）
```
接入中心第 ④⑤⑥⑦ 步转绿。

---

### 批 D — P1 主机探路落地

| 文件 | 改动 |
|---|---|
| `l0-console/onboard_preflight.go` | 🆕 checks.yaml 解析 + 门禁判定 + playbook 生成 + zip 打包 |
| `l0-console/onboard_api.go` | 补齐 `/preflight/spec`、`/preflight/package`、`/preflight/report` |
| `l0-console/data/integrations/*/preflight/checks.yaml` | 🆕 按内置 25 插件各预置一份（标准项模板批量生成） |
| `l0-console/data/integrations/*/preflight/playbook.yml` | 🆕 按 checks.yaml 同源生成 |
| `l0-console/data/integrations/*/preflight/README.md` | 🆕 每项失败怎么修 |
| `l0-console/static/js/onboard.js` | 时间线 `preflight_host` 步骤展示检查项明细表（实测值/期望值/结论/修复建议）+ 下载探路包 + JSON 回传 |

**checks.yaml 格式**（四类检查项，与用户点名的网络/环境/端口/配置对齐）

```yaml
plugin: mysql_probe
checks:
  - {id: dns_resolve,   category: network,  title: 目标地址可解析,   level: blocker, cmd: "getent hosts {{address}}"}
  - {id: tcp_3306,      category: network,  title: 目标端口可达,     level: blocker, cmd: "nc -z -w3 {{address}} {{port}}"}
  - {id: os_support,    category: env,      title: OS/架构受支持,    level: blocker, cmd: "uname -sm"}
  - {id: disk_free,     category: env,      title: 磁盘余量 ≥ 1G,    level: blocker, cmd: "df -P / | awk 'NR==2{print $4}'"}
  - {id: ntp_skew,      category: env,      title: NTP 偏差 < 5s,    level: warn,    cmd: "chronyc tracking"}
  - {id: port_idle,     category: port,     title: 采集端口未被占用, level: blocker, cmd: "ss -lntp | grep :{{port}}"}
  - {id: existing_exp,  category: config,   title: 无同类 exporter,  level: warn,    cmd: "pgrep -f mysqld_exporter"}
  - {id: mysql_auth,    category: config,   title: 采集账号可登录,   level: blocker, cmd: "mysql -h{{address}} -P{{port}} -u{{username}} -p{{password}} -e 'SELECT 1'"}
```

**门禁语义**
- `blocker` 未过 → 硬阻断：不生成配置、不下发；第 ③ 步标红，显示实测值/期望值/修复建议 + 「重新探路」
- `warn` → 黄灯提示，允许继续
- 强制继续需填理由 + 审计留痕（`audit_log` 记 `action=强制通过探路`）

**验收**：只有 IP、无 OS/架构的资源能走完整条链（P1 探出 `linux/amd64` → 决定镜像 tag → 装 → P2 → 采集）。

---

## 8. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| 批 A 改动面大（新增 3 表 + 7 文件） | 回归可能挂 | 分批提交；批 A 内部再分 A1/A2/A3/A4 四个 commit |
| `agent_config.content` 扩展破坏既有 Agent | 配置解析失败 | `parseAgentConfigDoc` 已兼容旧格式（纯数组）；渲染器只**追加**不覆盖未知段 |
| 心跳协议扩展破坏旧 Agent | 旧 Agent 心跳 400 | 新字段全部可选；缺失时走原路径 |
| 前端 `data-page` 改名破坏深链 | 菜单点击无效 | 本轮只增不改 key（`slo`/`tenants` 的 key 保留） |
| 探路需要 SSH 凭据 | 安全债 | 凭据只用于外部执行器，**不落平台库**；平台只存探路结果 |

**回滚**：每批一个 commit，`git revert <hash>` 即可。数据库新增表/列均为**增量**，不影响旧代码运行。

---

## 9. 附录 · 现状代码地图（对齐基线）

### 9.1 控制面文件与行数

| 文件 | 行数 | 职责 |
|---|---|---|
| `main.go` | 245 | cfg 块 + main() 装配 + 启动引导 |
| `agent_registry.go` | 136 | Agent 结构体/内存注册表/版本探测 |
| `agent_api.go` | 667 | Agent 生命周期/目标 CRUD/配置轨道/插件操作 |
| `metric_api.go` | 119 | 指标中心 API + MetricDef 契约 |
| `misc_api.go` | 265 | site-config/审计/任务/集成包/VM 代理 |
| `onboard.go` | 71 | 接入配置（产品语义数据化） |
| `docker_util.go` | 222 | Docker/动作执行外壳工具 |
| `catalog_api.go` | 376 | 插件目录 API |
| `recon.go` | 319 | 采集健康对账 |
| `store/store.go` | 1034 | plugins/metrics/dashboards/audit_log |
| `store/fleet.go` | 236 | agents/targets/agent_config |
| `importer/importer.go` | 437 | 插件包导入 |

### 9.2 现有路由（31 个 `HandleFunc` + 3 个 `Handle`）

| 文件 | 路由 |
|---|---|
| `agent_api.go` (8) | `/api/agents`、`/api/agents/`（含 `/{id}` `/config` `/plugin-status` `/metrics` `/logs` 子路由）、`/api/agent/register`、`/api/agent/heartbeat`、`/api/targets`、`/api/targets/`、`/api/action`、`/api/plugin-action` |
| `metric_api.go` (1) | `/api/metrics` |
| `misc_api.go` (9+2) | 9 个 `HandleFunc`：`/api/site-config`、`/api/onboard/config`、`/api/audit`、`/api/vm/metrics`、`/api/tasks`、`/api/plugin/upload`、`/api/integrations`、`/api/integrations/`、`/api/vm/query`；2 个 `Handle`：`/uploads/`（→ `data/plugins/upload`）、`/uploads/integrations/` |
| `catalog_api.go` (12) | `/api/catalog/plugins`、`/api/catalog/plugins/`、`/api/catalog/metrics/`、`/api/catalog/dashboards/`、`/api/catalog/reimport`、`/api/catalog/sync-report`、`/api/metrics/refs`、`/api/metrics/gov`、`/api/metrics/gov/facets`、`/api/vm/names`、`/api/vm/query_range`、`/api/vm/label-values` |
| `recon.go` (1) | `/api/recon` |
| `main.go` (1 Handle) | `/`（SPA 入口 + 静态资源） |

> `/uploads/` 服务 `data/plugins/upload`——**该目录是运行态上传资产目录，不是残留**（删除后运行时会 `MkdirAll` 重建，`.gitignore` 已排除其内容）。

> 新增路由**一律挂在新的 `registerOnboardRoutes(mux, ...)` 里**，不要再往已有文件堆。

### 9.3 现有表（8 张，含 1 张 legacy）

`plugins` · `metrics` · `metrics_v2`(legacy) · `dashboards` · `audit_log`（以上 `store/store.go`）
`agents` · `targets` · `agent_config`（以上 `store/fleet.go`，由 `InitFleet()` 创建）

### 9.4 前端文件

| 文件 | 行数 |
|---|---|
| `panel.js` | 1964 |
| `utils.js` | 145 |
| `fleet.js` | 75 |
| `api.js` | 12 |
| `app.js` | 9 |

### 9.5 Agent 侧配置结构（`sagent/internal/config/config.go`）

```
Config
├── Server{Listen}
├── Resource{ID, Type, BusinessSystem, Env, IDC, Cluster}
└── Plugins
    ├── HostMetrics{Enabled, Interval, Groups, ExcludeMetrics}
    ├── LogMetrics{Enabled, BinPath, ConfDir}
    ├── MySQLProbe{Enabled, ExporterBin, Targets[]{DSN, ResourceID}}
    ├── CustomScripts{Enabled, Scripts[]{Name, Command, Interval, Timeout, ResourceID, Labels}}
    ├── PortChecker{Enabled, Interval, Targets[]{Name, Type, Address, Timeout, Labels}}
    └── PrometheusScrape{Enabled, Interval, Targets[]{Name, URL, Timeout, Labels}}
```

`constants.Version = "0.4.0"`（编译期常量，交付形态为 Docker 镜像 → "选版本" = 选镜像 tag）
`constants.DefaultL0ConsolePort = 8080`（**已定义、零引用** → 批 C 接线点）

---

## 10. 已决事项（实施前确认记录）

1. **资源对象来源**：✅ 先建本地表 + 手填/批量输入，`source=cmdb` 仅留适配位（`resources.source` 列已备）。
2. **开工范围**：✅ 批 0 + 批 A 一起实施（已完成，88/88 回归全绿）。
3. **强制继续**：✅ 允许「填理由 + 审计留痕后强制继续」（`/api/onboard/flow/step` 的 `force` 动作，审计记 `接入中心`）。

### 实施期修正记录（对账后已落码）

- **hybrid 模板补 `preflight_remote`**：远程能力在装 Agent 前必须有「从代理机侧探目标可达性」预检，与 remote 模式对称（此前 hybrid 只有装完 Agent 后的 P2，不对称）。
- **platform_device 强制自监控落到配置层**：`syncAgentConfig` 对 `role=platform_device` 的 Agent 强制 `host_metrics.enabled=true`——界面配置轨道里被关掉的也兜底拉起，不再依赖 Agent 端默认值。
- **stall 判定分执行域**：external 步骤不判卡住（见 §5.1）。
- **`when` 求值时序**：依赖判定先行，`when` 只在依赖就绪后求值；「装完 Agent 之后才生效」类时序用 `requires` 表达，不用 `when`（`when: agent_exists` 模式已从模板移除）。
- **「能力探路」环节并入「主机探路」**（2026-09-21，用户决策：减少一个环节）：三模板删除独立 `preflight_ability` 步骤，能力可行性检查改由 `preflight_host`（external，ansible 平台代跑）在装机之前承担；`pick_ability/collect_params` 前移到探路之前（remote/hybrid 对齐 edge 已有原则）。原「agent 域能力探路等 install_agent」的死等问题随之消失——检查不再依赖 Agent 存在。remote/hybrid 的 `preflight_remote` 步骤保留 atom `preflight_ability`（探的是远程目标可达性，不是能力声明核对），并补 `collect_params` 依赖（探什么目标由参数声明）。
- **接入点可达性升为硬门禁**（2026-09-21，用户评审）：`preflight_host` 从目标机实测 `agent_console_url` 可达性，curl 缺失回落 wget（alpine 精简镜像常态），双工具都探不通 → 步骤就地 fail（装之前拦截，不许「装完卡死在等回报」）；无探测工具 → `access_point_unverified` 告警（未核实 ≠ 不可达，不误伤精简系统）。带病接入走「强制通过」留痕。
- **stall 分域防并行覆盖**：stalled 判定收集全部 running 分支的执行域，任一是 external 即不判卡住（单看 current_step 会在并行分支下取错域）。
