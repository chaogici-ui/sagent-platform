# SAgent 采集平台架构设计与高可用运维方案

> 本文件是《PLAN-控制台前端升级-原型风格迁移-设计.md》的架构配套文档，覆盖三层诉求：
> ① 现有代码废弃/冗余审计标记（后续优化项）；② 按 L0/L1 两层架构核对现有代码映射与差距；③ L0/L1 高可用、可运维、可观测、可监控方案。
>
> 设计红线继承自 `CONSTRAINTS.md`（PostgreSQL 持久化、配置轨道 vs Agent 版本轨道分离、演示数据显式开关、指标命名业界标准、双轨分离）。

---

## 一、两层架构定义（用户给定的目标架构）

```
┌───────────────────────── L0 层（集中管理服务 + 状态中心）─────────────────────────┐
│  L0 VMStorage (VictoriaMetrics)  ◄────── 指标汇聚（独立于管控）                │
│  L0 Console（配置管理服务）· 状态管理在 L0 数据库（L1 组件无状态）              │
│    · 管理多租户采集平台  · 给 L1 下指令(安装/升级/启停/配置)  · 观测 L1 状态     │
│    · 版本包管理：上传/导入 → 同步分发到各 L1 Package Cache                   │
└───────────────▲───────────────────────────────▲────────────────────────────────┘
  控制指令/期望    │  心跳 / 状态回执      数据通路不与管控组件耦合(下行箭头为数据)
┌───────────────┴───────────────────────────────┴────────────────────────────────┐
│ L1 采集机（每资源池 3-5 台跳板机 · 组件全部无状态，状态落在 L0 DB）                │
│                                                                                 │
│  ┌── Controller ──┐   接收 L0 指令 → 分流到 Ansible / Gateway                    │
│  │   └─ Package Cache  版本包本地镜像(就近取包，避免跨池重复传包)                 │
│  │   └─ Ansible 执行端  首装 SAgent(SSH 推二进制→上报心跳)                       │
│  │   └─ Gateway 采集中继  运行时插件/脚本安装(Agent通道)；心跳汇聚(Agent断连兜底) │
│  └── 自身也装 SAgent，同时被监控/被 L0 控制                                      │
│                                                                                 │
│  数据面：Exporter① ─┐                                                           │
│          Exporter② ─┼─► L1 VMAgent ──► L0 VMStorage  (独立链路)                │
│           ...      ─┘                                                           │
└──────────────────────────────┬──────────────────────────────────────────────────┘
                               │  (控制: 首装走Ansible · 运行时走Gateway)
┌──────────────────────────────┴──────────────────────────────────────────────────┐
│ 被管对象（数万台 主机/中间件）        SAgent + 自定义采集脚本(目标机唯一组件)      │
└─────────────────────────────────────────────────────────────────────────────────┘
```

**控制通路（两条分工）**：
- 首装 SAgent：`L0 Console → L1 Controller → L1 Ansible → SAgent → 上报心跳`
- 安装/升级插件、自定义脚本（Agent 已在线）：`L0 Console → L1 Controller → L1 Gateway → SAgent → 装插件/脚本`
- 版本包分发：`L0 上传管理 → 同步给各 L1 Package Cache → 安装时就近取包`

**数据通路（独立于管控）**：`n×Exporter → L1 VMAgent 收敛 → L0 VMStorage`
※ 数据链路**不走 Controller/Gateway 管控组件**：管控故障不影响指标采集，指标量大不压控制面**。**

**心跳通路（控制面汇聚，对称于数据面）**：
- **一律经 L1 采集机汇聚，杜绝万台 SAgent 直连 L0**：`SAgent ──只与本资源池采集机──► L1 Gateway ──► L1 Controller ──► L0 Console`
- 网络安全域仅两跳：万台 Agent **只需打通到本资源池采集机（每池 3-5 台）**，只有采集机才打通到 L0；避免"每台 Agent 都要开 L0 网络策略"在万级规模下不可行。
- 下行反向：L0 → L1 Controller → Gateway，楼主端只认采集机，指令下行与心跳上行同走汇聚链路（Agent 通道正反向）
- **跨网域进程清单（只有两个角色）**：控制面出口 = `L1 Controller`（唯一双挂 l0-net + l1-net，承接 Gateway 汇聚的 SAgent 通道与 L1 内部组件的任务拉取，白名单见 `l0-console/gateway.go`）；数据面出口 = `VMAgent`（l1-net 经 vmauth 抓取、l0-net remoteWrite 至 VMStorage）。`L1 Gateway`（面向半可信 SAgent 的中继，**不持有 L0 可达**）、`L1 Package Cache`、`L1 Ansible Runner`、`SAgent`、`vmauth` 一律只挂 l1-net。

---

## 二、现有代码与目标架构的映射核对

### 2.1 组件 → 代码/容器 映射表

| 目标架构组件 | 现有实现（代码/容器） | 是否就位 |
|-------------|---------------------|---------|
| L0 VMStorage | `deploy/docker/docker-compose.yml` 的 `victoriametrics` (8428)，容器 `l0-victoriametrics` | ✅ |
| L0 Console（配置管理服务） | `l0-console/`（Go），容器 `l0-console` (8080)，67 API | ✅ |
| L0 版本包管理 | `l0-console/version_upload.go` (`/api/versions/upload·download·match`)，版本清单（versions.yaml） | ✅ |
| L0 可视化 | `grafana` (3000)，容器 `l0-grafana` | ✅（非核心，可观测） |
| L1 Controller（收指令+分流） | 部分：`sagent/internal/control/client.go` 已实现心跳拉取任务；**Ansible/Gateway 分流中枢待建** | ⚠️ 一半 |
| L1 Package Cache（版本包镜像） | L0 侧有版本管理；**SAgent/L1 侧包缓存目录待建**（无 packages/ 缓存） | ❌ 待建 |
| L1 Ansible 执行端（首装 SAgent） | `l0-console/onboard_ansible.go` + playbooks（L0 驱动）；**L1 侧独立执行端待定** | ⚠️ 重定位 |
| L1 Gateway（插件/脚本安装 + 心跳汇聚） | 无独立组件；插件安装走 `sagent/internal/control/client.go` 任务通道；心跳汇聚为新增。**【已定：Gateway=控制通道双向上行中继，包缓存剥离给独立 PackageCache】** | ❌ 待建 |
| L1 SAgent（边缘控制端） | `sagent/`（Go），容器 `l1-sagent-1/2` + `l1-sagent-proxy` | ✅ |
| L1 VMAgent（数据收敛） | `vmagent` 容器 `l1-vmagent`，`remoteWrite.url=l0 victoriametrics`，本地缓冲 2GB | ✅ |
| L1 Exporter 插件 | `sagent/internal/plugin/`（host_metrics、mysql_probe、custom_scripts、port_checker、prometheus_scrape、log_metrics） | ✅ |
| L1 VMHost（宿主 DEMO） | `vm-host` 容器 `vm-host-1`（sshd+python，ansible 接入靶机） | ✅ |
| L0→L1 控制指令通道 | `sagent/internal/control/client.go`（心跳拉取任务）+ `l0-console/agent_api.go` 的 `executeAction` | ⚠️ 见 2.2 |
| 多租户能力 | `l0-console/store` 无 tenant 表维度；前端 `tenants` 页存在但 render 简单 | ⚠️ 薄弱 |
| L1 无状态（状态落 L0 DB） | 心跳/回执存 L0；**L1 本地状态未收敛为纯无状态** | ⚠️ 待收敛 |
| Exporter 故障漂移/HA | 无 | ❌ 缺失（见第三章） |

### 2.2 控制指令通道的两条并存路径（架构冲突点）

现状存在**两条下指令通道并存**：

| 通道 | 实现 | 特点 | 与目标架构契合度 |
|------|------|------|-----------------|
| A. 心跳拉取任务（control 通道） | `sagent/internal/control/client.go`：SAgent 主动心跳 → L0 下发 `tasks` → SAgent 取回执行 | SAgent 主动拉取 + 回执 | ✅ **契合**（Agent 主动拉取，SAgent 断连自主兜底） |
| B. 单机 docker 直连 | `l0-console/docker_util.go` 的 `executeAction`：L0 直接 `docker exec` 容器启停 | 强制依赖 docker.sock + 同机，无法跨网、无法管全网 | ❌ **与目标冲突**（无法覆盖"全网 L1"，也无法用隧道/控制面下发） |

**结论**：目标架构明确要求 L0 通过控制面给全网 L1 下指令。通道 B 仅对"L0 与容器同机"的演示形态有效，不满足"全网统一管控 + 故障漂移"。**建议逐步退役通道 B**，统一走通道 A（SAgent 心跳拉取 control 指令），docker 探测（`docker_util.go` 探测 l1-* 状态）保留仅用于演示 SEED 开关内。

### 2.3 架构差距清单

| # | 差距 | 影响 | 关联现有代码 |
|---|------|------|-------------|
| G1 | 多租户维度缺失 | 目标明确"管理多租户采集平台"，现无 tenant 隔离。**【已定：本期做】** 全链路加 tenant_id（DB 表 + API + 前端过滤隔离） | `store` 无 tenant 表、`static/js/panel.js#renderTenants` 简单 |
| G2 | 控制通道双轨并存 | 通道 B 无法覆盖全网、与 HA 冲突 | `docker_util.go` vs `control/client.go` |
| G3 | Exporter 故障漂移无支撑 | exporter 单点、vmagent 单点，无自动切换 | `sagent/internal/plugin/subprocess` 无漂移/归属迁移 |
| G4 | VMStorage/VMAgent 无 HA | 中心与汇总层单实例 | compose 均 `restart: unless-stopped` 单副本 |
| G5 | 心跳/状态可观测是查询式，非主动告警 | 无主动推送/告警通道 | `collect_stats`、`recon` 均为 pull 接口 |

---

## 三、L0/L1 高可用（HA）、可运维、可观测、可监控方案

### 3.0 L1 无状态化与采集机接管（本次架构核心前提）

**目标**：每个资源池部署 N 套 L1 采集自管理组件（Controller/Gateway/Package Cache/Ansible），**组件全部无状态，状态统一落在 L0 数据库**；一台采集机故障时，**同资源池其它采集机上的同类组件自动接管**。

**无状态化的判断标准（逐组件）**：

| 组件 | 状态在哪 | 是否天然无状态 |
|------|---------|--------------|
| Controller | 任务表/状态在 L0 DB | ✅（心跳拉取天然无本地态） |
| Gateway | 汇聚仅转发/暂存，状态 L0；仅瞬态 buffer | ⚠️ 需保证不落本地数据库，仅瞬态 buffer |
| Package Cache | 仅文件镜像（可重建，从 L0 重新同步） | ✅（可当只读缓存，失效可回源） |
| Ansible | 无状态（每次全量 playbook） | ✅ |
| SAgent（采集机自身） | 配置/版本上报 L0 | ⚠️ 本地仅留运行态，幂等可重建 |

**接管机制（故障漂移的三层落地）**：

```
采集机 N1 故障
   ├─ Controller：心跳断连 → L0 感知 → 指令改发同池另一台 (N2.Controller)
   ├─ Gateway：汇聚链路断 → L0/lb 感知 → 汇聚 endpoint 切到 N2.Gateway
   └─ Package Cache：取包失败 → Agent 回源 L0 或同池 N3.Cache 索取
```

- **判定**：L0 侧通过 SAgent 心跳超时判定某采集机失联（复用 `onboard_timeout.go` 超时机制 + 重试策略）。
- **接管是"重新调度"而非"状态恢复"**：因为无状态，接手方只要从 L0 重新拉期望配置/任务即可，无需迁移本地状态——这正是无状态化的价值。
- **工程红线**：接管触发必须幂等（一个目标至多被一个接管方认领，`claimed_by`+租约），避免 N1 故障瞬间多台同时接手导致重复执行。
- **规模映射**：12 租户 × N 资源池 → 每个资源池的采集机清单与租户映射在 L0 DB 维护；接管只改"目标→采集机归属"映射。

**Package Cache 一致性约束（风险 A 的强制红线）**——防止"教装老包"导致批量升级分裂：
- Package Cache **只能承载 L0 校验过的只读镜像**，禁止 L1 本地直接生成的产物。
- 每个包带 **L0 下发的校验和/签名清单**；SAgent 安装前**强制校验**，校验失败一律**拒绝安装并回源 L0**，不允许降级用旧包。
- 缓存同步幂等：L0 增量 diff 下发，重复同步不重复任务；缓存失效可完全从 L0 重建（只读，可废弃重下）。
- 版本一致性由 L0 版本清单（`versions.yaml`）唯一裁决，L1 Cache 不充当版本决策源。

### 3.1 高可用（HA）方案

#### (1) 控制/通信面 HA（用户重点强调：通信模块 + exporter 远程采集组件支持故障漂移自动切换）

**A. L0 Console 高可用（无状态多副本 + PostgreSQL）**
- L0 Console 无状态（会话/任务/状态全部落数据库）：多副本 `l0-console-1/2`，前端经 `nginx/` 负载均衡。
- **存储决策（已定）：现在就迁 PostgreSQL**，不保留 SQLite。原因：
  - SQLite 经 NFS/共享卷并发写有 WAL 锁风险，在多副本 + "状态落 L0 DB"（接管前提）下不可接受；
  - HA 与 L1 接管都依赖"状态在数据库强一致读"，PG 是唯一干净满足的选项；
  - 符合原型 settings 里"PostgreSQL 16+"方向，一次到位避免后续大规模重构。
- 迁移范围：`catalog.db`（原 SQLite）→ PostgreSQL，表结构/SQL 方言适配。**实施结果（2026-09-25 收口）：SQLite 载体已彻底移除**——驱动（modernc.org/sqlite）、方言翻译层、`CATALOG_DSN` 缺省回退分支一并删除，`store/` 层只保留 PG 单载体（`pgx/v5`，SQL 以 `?` 书写、执行前 `rebind` 转 `$N`），单测载体同源改 PG（`testpg.Provision` 每用例独立 schema）。
- 指令下发幂等：control 通道天然幂等（心跳拉任务），多副本不重复下发（任务表加 `claimed_by` + 租约）。

**B. L0→L1 通信模块 HA（主动拉 HTTP + 重试/重连）**
- 通信走 **SAgent 主动 HTTPS 心跳拉取**（L1 反向连 L0），天然规避"L0 无法直连内网 L1 / NAT / 隧道"问题。
- 多 L0 端点：SAgent 配置 `l0_ha_endpoints[]`，主端点连不上自动切换备端点（RoundRobin/健康探测），断线重连带指数退避 + 心跳缓存补报。
- 状态机约束（沿用工程经验）：stop/restart/install 前置状态校验 + 幂等，避免 HA 下重复执行。

**C. Exporter 远程采集组件故障漂移自动切换 + 端口收敛（重点）**
- **端口收口方案（已定：vmauth 单端口）**：达成"curl 不暴露采集数据"的目标效果，选中 vmauth 单端口而非 unix socket。理由：
  - vmauth 对外**只暴露 1 个受鉴权端口**，按 path 路由到各 Exporter；未带 auth_key 的 curl/扫描一律 401，返回不了任何 metric——**数据层不暴露达标**。
  - **必须覆盖远程 PaaS 采集**（mysql/redis/kafka 跨主机，场景六）：unix socket 只能保护目标机本机 exporter，对远程采集器无效；vmauth 单端口统一收口本地+远程全部 exporter，是唯一能覆盖全采集类型的选择。
  - vmagent 走标准 HTTP/TCP 经 vmauth 抓取，兼容稳定，且保留端口/日志便于可观测排障。
  - 威胁模型匹配："curl 不返回数据"=数据保护；unix socket 是最强"端口不可见"形态，仅作本地 host exporter 的可选叠加，非主方案。
- 实现载体：L1 部署 vmauth（单端口 + auth_key + path 白名单 + gzip），vmagent scrape 目标指向 vmauth 而非各 Exporter 直连（对应场景四步骤⑥⑦⑧）。
- 核心思路：**Exporter 归属 L1 SAgent 动态管理，故障时在 L1 内/跨 L1 漂移**。
  - L1 内多副本：一个采集目标可由多个 exporter 实例（如 mysql A/B）双写，SAgent 探活不健康时切到健康副本（`port_checker`/`scrape_plugin` 的健康探测复用）。
  - 跨 L1 漂移：L0 维护"采集目标 - exporter 归属"映射，目标上报失败且超时 → L0 决策指令把归属迁移到健康 L1（走 control 通道下发新配置），SAgent 收到后起对应插件。幂等 + 终态保护，避免重复拉起。
- 实现载体：复用现有插件体系，新增"目标探活 + 归属迁移"逻辑（在 `scrape_plugin.go`/`subprocess` 增加健康探测与切换钩子）。

**D. VMAgent / VMStorage HA**
- vmstorage：VictoriaMetrics 支持 `-clusterMode` 集群（vminsert/vmselect/vmstorage）或多副本，或 Runbook 用 VM 副本 `-replicationFactor=2`。
- vmagent：`-remoteWrite.url` 配置多个 vminsert 端点 + 本地缓冲盘（已配 `maxDiskUsagePerURL`），vmagent 自身多副本各拉一份目标，远端去重由 VM 侧处理。
- **数据通路兜底**：exporter→vmagent 失败时 SAgent 本地暂存，恢复后补写（已由 vmagent buffering 承接一部分）。

#### (2) 高可用落地优先级（按真实采集平台规模设定，不做轻量降级）
> 注：早期版本曾按"内部轻量工具"定位收敛 HA 强度，已按用户纠正撤销——本平台是通用采集平台，L1 侧架构按真实规模做，不预设功能降级。

| 优先级 | 组件 | 方案 | 理由 |
|--------|------|------|------|
| P0 | L0 Console 控制面 | 多副本 + PG 化 + 租约幂等 | 控制中心失联＝全网失管，最关键 |
| P0 | L0↔L1 通信 | SAgent 主动拉 + 多端点切换 + 重连 | 通信面是用户点名重点 |
| P1 | Exporter 故障漂移 | 归属迁移 + 多副本探活 | 用户点名重点 |
| P2 | VMStorage/VMAgent | 副本 + 多端点 fallback | 指标丢失可容忍，优先级低于控制面 |

> 工程红线提醒：任何 HA 改动必须遵守"操作前校验状态 → 幂等 → 终态保护 → 恢复成功清终态"四件套，先前踩过的坑（stop 状态被心跳覆盖、install→start 重复）在 HA 多副本下会被放大，必须慎做。

### 3.2 可运维（Operability）方案

- **安装/升级全网**：已是核心能力（`onboard_ansible.go`、`version_upload/match.go`、playbooks），补齐"批量 + 灰度 + 回滚"的可视化（已在控制台升级方案覆盖）。
- **统一日志**（符合用户日志红线：标准库、异步、归档压缩、保留最近 10 个、单压缩 ≤10MB、杜绝垃圾日志）：L0/L1 均落 `logs/`，异步写。
- **配置热生效**：走已有 config 轨道（`config_version+1` 心跳热生效），Agent 版本分离（`main.go` 插件注册表）。
- **健康自检**：SAgent 提供 `/health`（已有），L0 采集健康页提供 `/api/health` 汇总（规划新增）。
- **后台任务治理**：超时清扫（`onboard_timeout.go`）、隧道守护（`onboard_tunnel.go`）已有，补全失败注入与自愈。

### 3.3 可观测（Observability）方案

- **指标自观测**：`sagent/internal/pipeline/self_metrics.go` 已有自指标；补齐 L0/console/vmagent 自身指标推送进 VM。
- **链路证据**：沿用控制台升级里的"证据链"（heartbeat→config→source→transport→central_ingest），数据来自 `recon` + `collect-stats` + `plugin-status`。
- **关键指标清单**（进 VM，供 Grafana）：
  - L1：心跳时延、task 执行时延、exporter 起停次数、上报字节、失败率
  - 通信：endpoint 切换次数、重连次数、积压量
  - L0：指令下发到回执时延、任务队列深度、失败/超时
  - 链路：exporter→vmagent→vmstorage 各hop 延迟与丢点
- **仪表盘**：复用现有 Grafana provisioning，新增"SAgent 平台自身健康"Dashboard。

### 3.4 可监控（Monitoring）方案

- **主动告警通道**：目前是 pull 式（`collect_stats`/`recon` 接口）。补齐：
  - VM 告警规则（VictoriaMetrics vmalert 或 Grafana alert）：心跳断连、task 失败、exporter 不可用、上报丢失、存储水位。
  - 通知落点：L0 控制台内告警中心（新增视图）+ 可选 webhook/飞书/邮件（走 env 配置，不写死）。
- **探活机制**：L0 周期性探测 L1（SAgent 心跳超时判定），配合 recon 的 `stale/zombie` 对账。
- **阈值定义**：在 `CONSTRAINTS.md` 红线 5 基础上，新增告警阈值登记表（避免散落）。

---

## 四、实施路线（与前端升级方案并行/衔接）

| 阶段 | 范围 | 关联前端升级 |
|------|------|-------------|
| HA-1 | L0↔L1 通信多端点 + 重连（P0），退役 docker 直连通道 | 无（后端） |
| HA-2 | L0 Console 多副本 + PG 化 + 任务租约（P0） | 无 |
| HA-3 | Exporter 归属迁移与故障漂移（P1） | 前端"采集健康"证据链展示此状态 |
| OBS-1 | 自观测指标 + 平台健康 Dashboard（可观测/监控） | 前端"工作台"可展示 |
| ALERT-1 | vmalert/Grafana 告警规则 + 告警中心视图 | 前端新增"告警中心"或并入"采集健康" |

> 每个阶段独立可验收、可回滚；后端改动全部走环境变量登记（`main.go` 顶部 `cfg*` 块），遵守红线 2/3。

---

## 五、决策记录（已定案，取代原"待确认项"）

| # | 事项 | 结论 | 状态 |
|---|------|------|:---:|
| D1 | SQLite → PostgreSQL | **现在就迁 PG**（不保留 SQLite 双载体）；**2026-09-25 已收口：SQLite 载体彻底移除，PG 为唯一载体** | ✅ 已落地 |
| D2 | docker 直连通道（`docker_util.go`） | 统一退役，走 control 心跳拉取；演示 SEED 开关内保留 | ✅ 已定 |
| D3 | 多租户 G1 | **本期做**：全链路 tenant_id（DB+API+前端隔离） | ✅ 已定 |
| D4 | Exporter 端口方案 | **vmauth 单端口**（单端口+auth_key+path白名单+gzip），unix socket 仅作本地可选叠加 | ✅ 已定 |
| D5 | L1 组件形态 | 4 进程独立 + 1 tar.gz 打包；Gateway 剥离包缓存给独立 PackageCache | ✅ 已定（对齐合分方案）|
| D6 | 告警通知落点 | 默认 L0 控制台内部告警中心；外发 webhook/飞书按 env 配置启用 | ⬜ 默认，可再调 |

---

## 六、废弃/冗余代码清单（补充点 1 交付，仅标记不删）

### 明确废弃（建议清理）
- `sagent/cmd_fsprobe_main.go.bak`（临时调试脚本）
- `sagent/bin/SAgent`、`sagent/bin/SAgent-linux`（构建产物，Dockerfile 现编）
- `sagent/run/bus.sock`、`control.sock`（运行时产物）
- `l0-console/static/js/fleet.js#renderOverview()`（旧总览，被 overview.js 取代）
- `l0-console/static/js/panel.js#renderVersions()`（被 version.js 覆盖）
- `l0-console/static/js/panel.js#renderAuth()`（无路由无调用）
- `l0-console/onboard_ansible_stream.go`（零引用）
- `sagent/internal/plugin/builtin/port_checker.go:136-150`、`scrape_plugin.go:130-143`（未引用旧配置类型 + `var _ = fmt.Sprintf` 占位）

### 冗余（建议合并）
- `l0-console/static/js/api.js`（12 行 1 函数）
- `l0-console/static/js/fleet.js`（75 行 1 废弃函数）
- 根目录多份 `PLAN-SAgent*.md` 规划/实施并存，建议仅留最新

### 架构路径待定（非死代码）
- `l0-console/docker_util.go`（单机 docker 直连通道，与目标架构冲突，见 2.2）

> 以上仅标记，不执行删除；清理时机由你按阶段确认。