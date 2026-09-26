# SPEC：远程采集集中部署与漂移提速 边界定义与实施计划（立项准备稿）

> 依据：用户 2026-09-24 需求——「在采集跳板机上安装 paas 远程采集插件，连接被管对象进行端口远程采集」，
> 并补充两点：① 选目标安装主机（采集机）与配置采谁（被监控对象）应分两步，需考虑界面交互；
> ② 采集机下线/异常时，采集任务如何漂移/高可用切换到其它采集机，且**周期越短越好**。
> 关联既有立项：《SPEC-HA3-跨L1归属迁移-边界与实施计划.md》（本稿在其上收窄候选池并提速）。
> **本稿只定边界与计划，不动代码。** 仅在你逐阶段授权后按第五节执行。

---

## 一、目标与定位

把「远程采集」从"装在一台机器上、坏了就断"升级为**池化承载 + 故障自动漂移**：

1. **界面两步分离**：① 选采集机（池）与 ② 配被监控对象（ip/端口/账号/口令）在交互上明确分开，不再挤在同一个 `resource_id` 里。
2. **采集机池化**：一个采集机池（同地域）承载若干被监控对象；采集机是少数派（每池 3-5 台），普通 SAgent 是万台级。
3. **故障自动漂移**：池内某台采集机失效，其名下采集目标自动改归池内健康成员，**端到端目标 ≤45s**。
4. **单归属**：同一对象同一时刻只归一台采集机（不做多副本双活）；漂移是"换归属"，不是"加副本"。

**不在本稿范围**：目标内主备端点切换（已落地的 B16，`scrape_plugin.go`/`port_checker.go` 的 `Replicas` + `failbackThreshold`）；多副本双活探活。

---

## 二、现状事实（代码依据，非虚构）

| 能力 | 现状 | 出处 |
|------|------|------|
| 远程采集流程模板 | **已存在**：`pick_object → pick_proxy → pick_ability → collect_params → preflight_host → preflight_remote → pick_version → install_agent → register_platform_device → self_metrics → sync_config → verify_probe → observe_collect` | `data/flow_templates/remote.yaml` |
| 远端采集插件 | **已声明** `Scope: "remote"`：MySQL / Redis / Kafka / Elasticsearch / ClickHouse / HTTP 拨测 | `onboard.go` `OnboardPlugins` |
| 被监控对象参数 | **已声明**：`address` / `port` / `mode` / `username` / `password` / `interval` / `extra_labels`，字段由插件包 `params.yaml` 驱动 | `data/integrations/MySQL/params.yaml` 等 |
| 归属映射 | `targets.agent_id`=承载采集机；`targets.address`+`params_json`=被监控对象；`targets.region`=地域 | `store/fleet.go` `TargetRow` |
| 目标创建 | `atomCollectParams` 建 target：`Address` 取参数 address，`AgentID`=r.agentID，`ResourceID`=flow.ResourceID | `onboard_atoms.go:643` |
| 归属改写 | `UpdateTargetOwner(id, toAgent)` 仅改 `agent_id`，不动其余字段（幂等） | `store/fleet.go:338` |
| 地域登记 | `SetTargetRegion(id, region)` 已实现；`targets.region` 增量列已 ALTER | `store/fleet.go:357`、`store/fleet.go:111` |
| agent 侧可承载池语义 | `agents` 表已有 `type`（edge/proxy/custom）与 `labels_json`；`agentRegion` 读 `Labels["region"]` / `Labels["availability_zone"]` | `store/fleet.go:59/64`、`relocate.go:46` |
| **漂移决策与执行** | **已实现**：`planRelocation` / `runRelocation` / `autoRelocate` / `startRelocateAuto` + `relocations` 表 + 3 个 API | `relocate.go`、`store/relocation.go` |
| 漂移判据（现状默认） | 失效阈值 `RELOCATE_FAIL_SEC=120`；冷却 `RELOCATE_COOLDOWN_SEC=300`；自动调度 `RELOCATE_AUTO`（**默认关**）；扫描间隔 `RELOCATE_AUTO_INTERVAL_SEC=30` | `relocate.go:21-26` |
| 漂移边界 | 仅远端采集可迁；`host_metrics`/`docker_metrics`/`sagent`/`sagent_health`/`port_checker` 等本地能力不可迁 | `relocate.go:30-44` |
| 漂移顺序 | **先新后旧**：先改 `agent_id` + 新端 `syncAgentConfig`，再旧端 `syncAgentConfig` 停 | `relocate.go:197-206` |
| 漂移留痕 | `relocations` 表 + `addAudit`；前端证据链卡片可下钻「从 → 到」 | `store/relocation.go`、`static/js/panel.js:140` |
| 心跳间隔 | **硬编码常量 30s**（决定检测延迟物理下限） | `sagent/internal/constants/constants.go:44` |

### 缺口（本稿要解决的）

| # | 缺口 | 事实依据 |
|---|------|---------|
| **G1** | **`pick_proxy` 是空壳**：`atomPickAgent` 直接取 `r.agentID`，为空则回落 `flow.ResourceID`——没有候选池、没有选择界面 | `onboard_atoms.go:542-559` |
| **G2** | **前端自认未实现**：remote/hybrid 模式的承载机显示"待「确认承载主机」环节确定" | `static/js/onboard.js:1535` |
| **G3** | **候选池过宽**：漂移候选是"同租户 + 同 region 的**任意**健康 heartbeat agent"，未限定为采集机池成员 | `relocate.go:69-103` |
| **G4** | **检测慢**：心跳 30s × 4（阈值 120s）+ 扫描 30s ⇒ 最坏 150s 才发现采集机失效 | `constants.go:44`、`relocate.go:22-25` |
| **G5** | **切换慢**：确认故障后才走"改映射 → 下发 config → 起插件 → 验证"全链路 | `relocate.go:197-206` |
| **G6** | **池内不足无保护**：整个池故障时会尝试池内互迁（无健康承接方） | `relocate.go:190-196` |

---

## 三、核心机制

### 3.1 采集机池建模（零新增表）

复用既有字段，不新建表：

- **采集机身份**：`agents.type == 'proxy'`（Collector Proxy）或 label `role=collector`
- **池归属**：`agents.labels_json.pool`（新约定），未登记时回落 `region`
- **被监控对象的地域/池**：`targets.region`（已存在）

候选集判据（收窄 G3）：**同租户 + 同池 + 健康 heartbeat + 是采集机 + 非当前属主**。

### 3.2 两步界面交互（补 G1/G2）

`pick_proxy` 从"空壳回显"改为**真选择**，且**选池不选机**：

- 候选来源：采集机池成员（同租户 + 同池 + 健康），展示每台**当前承载数**（负载可见）
- 默认动作：平台在池内自动挑一台（承载数最少 / 稳定排序取最小 id），界面给"推荐一台 + 可改选"
- 被监控对象（`collect_params`）保持插件 `params.yaml` 驱动，支持一次配多条（一个采集机采多个实例，每条落一个 target）
- 界面分区：① 选采集机（池） ② 配被监控对象（地址/端口/账号/口令），互不混淆

数据模型不动：`targets.agent_id`=采集机，`targets.address`+`params_json`=被监控对象。

### 3.3 漂移收窄与自动触发

- **候选收窄**（G3）：`pickCandidate` 增加"必须是采集机池成员"判据
- **自动触发**：`RELOCATE_AUTO` 默认打开（带冷却终态保护）
- **池内不足保护**（G6）：池内健康成员 < 2 时**不迁移**，直接告警，避免故障池内互迁产生无意义变更

### 3.4 检测提速（补 G4/G5）——「周期越短越好」的两段拆解

延迟拆成两段，分别提速：

**② 切换段：预热接管（压到秒级）**

候选采集机**预先加载**目标配置但**不启动**采集插件（config 已就位、依赖已下、连通性已验证）。
确认故障后只发一条"启动"指令 ⇒ 接管从"改配置+下发+启动+验证"压缩到"启动"。

- 与"单归属"不冲突：预置的是**配置与包**，不是采集进程
- `Package Cache` 已在 L1 本地缓存版本包，天然支持

**① 检测段：采集机专用短心跳（压到 ~40s）**

关键杠杆——**采集机是少数派**：每池 3-5 台，普通 SAgent 万台级。
只给采集机单独用短心跳，L0/网关负载增量可控，万台普通 SAgent 不受影响。

| 档 | 检测最坏 | 端到端 | 代价 | 结论 |
|---|---|---|---|---|
| 现状 | 150s | ~2.5 min | — | 基线 |
| A. 只调参（阈值 120→90s、扫描 30→10s） | ~100s | ~1.7 min | 零改动（仅 env） | 备选 |
| **B. 采集机专用短心跳**（10s / 阈值 30s / 扫描 10s） | **~40s** | **~45s** | 心跳间隔按 agent 类型可配 | **采用** |
| C. B + 数据面断流交叉验证（只读查 VictoriaMetrics `up`） | ~30s | ~35s | 仅只读信号 | 可选加强 |
| D. 同池 peer 主动探活（5s × 3 次判定） | ~15s | ~20s | **新增探测链** | **触红线，需单独立项** |

**红线提示**：D 档要新增探测链，与既有红线「告警引擎仅扫描只读信号，不新增探测链」冲突。
**本稿不含 D 档**；如确需 15s 级，另起立项，不得夹带。

**物理下限**：指标断流最快也要一个 scrape interval 才看得出来；若 scrape interval=30s，"确认采集真断了"不可能快于 30s。

---

## 四、已定决策（用户 2026-09-24 拍板）

| # | 决策点 | 选定 |
|---|--------|------|
| 1 | 「选采集机」形态 | **选池 + 平台自动挑机** |
| 2 | 漂移候选池口径 | **收窄为「采集机池成员」** |
| 3 | 漂移触发方式 | **自动触发（带冷却）** |
| 4 | 是否多副本双活 | **不需要，单归属 + 故障漂移（本期）** |
| 5 | 提速档位 | **B 档（采集机专用短心跳）+ 预热接管**（本稿建议默认，待确认） |
| 6 | 池内健康成员 < 2 | **不迁移，直接告警**（本稿建议默认，待确认） |

---

## 五、分阶段实施计划（每步可独立交付/回滚）

| 阶段 | 内容 | 验证门禁 | 涉及文件（预计） |
|------|------|---------|------------------|
| R1 池建模 | 采集机身份与池归属约定（`type=proxy` / `labels.pool`）；池成员查询函数 | 单测：池成员筛选（同租户+同池+健康+采集机） | `relocate.go`、`store/fleet.go` |
| R2 选机环节 | `pick_proxy` 改为真选择：池候选 + 承载数 + 自动挑机；前端承载机选择卡 | 单测 + 浏览器验证（remote 向导能选池、能改选） | `onboard_atoms.go`、`static/js/onboard.js` |
| R3 候选收窄 | `pickCandidate` 增加"采集机池成员"判据；池内不足保护（G6） | 单测：非采集机不入候选；池内 <2 不迁 | `relocate.go` |
| R4 自动触发 | `RELOCATE_AUTO` 默认打开；冷却/每轮单目标/池内不足三重保护回归 | 单测 + 端到端漂移 | `relocate.go`、`main.go` |
| R5 检测提速 | 采集机心跳间隔可配（默认仍 30s，采集机可设 10s）；阈值/扫描间隔按档位调 | 单测 + 运行态观测实际检测耗时 | `sagent/internal/constants`、`sagent/internal/control`、`relocate.go` |
| R6 预热接管 | 候选端预置 config（不启动）；确认故障后单指令启动 | 端到端：切换耗时实测 ≤ 目标值 | `relocate.go`、`agent_api.go`、SAgent 侧 |
| R7 前端证据链 | 采集健康证据链展示池归属、迁移记录、检测耗时 | 浏览器验证 | `static/js/panel.js` |
| R8 回归 | `go vet`/`go build`/全量单测 + compose 回归 + `?v=` bump | 全绿 | — |

> 每阶段独立可回滚；后端改动走 `main.go` 顶部 `cfg*` 环境变量登记（红线 2/3）。

---

## 六、红线与状态机（继承工程经验）

1. **迁移＝幂等改映射**：重复迁移同目标=无操作；已处于目标归属不重复执行。
2. **终态保护双方向**：显式迁移打的终态不被心跳覆盖；但"迁移下一目标""恢复后回迁"必须能清终态，防永久卡死。
3. **防双写／防递归**：先新后旧；冷却窗口内绝不二次迁移同目标；每轮只迁一个目标。
4. **不跨租户**：候选集强约束 `tenant_id`，杜绝"旁租户接管"越权。
5. **数据面不直连管控组件**：所有迁移指令走既有 control 通道，不经数据链。
6. **不新增探测链**：检测提速只复用既有只读信号；peer 主动探活（D 档）需单独立项。
7. **池内不足不硬迁**：池内健康成员 < 2 时不迁移，转告警。

---

## 七、本次边界声明

- 本文件为**立项准备稿**，仅固化边界、决策与分阶段计划，**未改任何代码 / 未起任何容器 / 未动生产状态**。
- R1–R8 中每阶段**都需你按「立项→建议默认组合或改选→放行」**后才落地，严格对齐「架构改造前明确立项并确定边界」红线。
- 若你不推进此立项，本文件作为待评审输入留存，不影响现有 17 服务与前端六模块任何功能。

---

## 八、实施进展（2026-09-24）

| 阶段 | 状态 | 落地证据 |
|------|------|---------|
| R1 池建模 | ✅ 已落地 | `relocate.go` `isCollectorAgent`/`agentPool`/`isCollectorType`；身份认别名 `{proxy, collector, collector_proxy}` + `labels.role=collector`（演示环境既有取值 `collector_proxy` 已实测认定） |
| R2 选机环节 | ✅ 已落地 | `collectors.go`（池聚合/自动挑机/改选 API）、`onboard_atoms.go` 分流、`onboard.js` 承载采集机决策卡、`remote.yaml` 两步语义 |
| R3 候选收窄 | ✅ 已落地 | `candidateList` 限池成员；`poolInsufficient` 池内 <2 不迁移 |
| R4 自动触发 | ✅ 已落地 | `RELOCATE_AUTO` 默认 `1`、扫描 `10s`；单测覆盖「失效属主自动漂移」与「池内不足不迁」 |
| R5 检测提速 | ✅ 已落地 | 采集机 10s 短心跳（`constants.CollectorHeartbeatInterval`）+ 安装渲染 `type=proxy / heartbeat_interval:10s`；失效阈值按身份分档（采集机 30s / 其余 120s）。**运行态实测**：`order.prod.mysql.proxy-01` last_seen 每 10s 前进、`order.prod.host.sagent-1` 每 30s 前进 |
| R6 预热接管 | ⛔ **受阻（发现前置缺口）** | 见下 |

### R6 受阻说明（前置缺口，需先立项）

**事实（代码 + 运行态双重证据）**：SAgent 端**不支持平台下发的 `targets`**——`sagent/config_apply.go:191` 对非空 `targets` 直接返回
`managed target application is not supported yet`；采集机的远端采集目标实际来自其 **SAgent.yaml**
（如 `sagent-proxy.yaml` 的 `mysql_probe.targets`），与平台配置文档无关。

**运行态佐证**：`order.prod.mysql.proxy-01` 的平台配置 `cfg_desired=2` 但 `cfg_effective=cfg-1`——
含 `targets` 的期望配置从未被 Agent 应用。

**结论**：现有 HA-3 迁移只改了平台侧归属记录与期望配置文档，**数据面接管（在新采集机上真正起 exporter）尚未打通**；
"预热接管"（候选端预置配置、故障后单指令启动）没有可依附的执行链。

**建议前置项（R0）**：先实现「Agent 侧托管目标应用」——按平台配置文档启停远端采集插件（`prometheus_scrape`/`mysql_probe` 等），
并引入 `standby` 标记（预置配置但不启动），再在其上做 R6 预热接管。
此项改变 Agent「配置即生效」的语义、影响所有 Agent，属架构级改动，**需你确认边界后单独立项**。

> **已立项（2026-09-24）**：R0 + R6 合并为独立立项稿
> [`SPEC-Agent托管目标与预热接管-边界与实施计划.md`](./SPEC-Agent托管目标与预热接管-边界与实施计划.md)
> ——已固化 `absent/standby/active/failed` 状态机、单归属边界、能力协商与分阶段计划；**待你逐阶段放行**，未改代码。

---

## 附：待你确认的两项默认值

1. **提速档位**：B 档（采集机专用短心跳 10s + 预热接管），端到端 ~45s。
   若需更快（~30s），加 C 档数据面断流交叉验证；若需 15s 级，D 档需单独立项。
2. **池内健康成员 < 2**：不迁移、直接告警。