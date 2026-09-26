# SPEC：Agent 托管目标应用与预热接管 边界定义与实施计划（立项准备稿）

> 依据：《SPEC-远程采集集中部署与漂移提速-边界与实施计划.md》§八「R6 受阻说明」——
> R6 预热接管**没有可依附的执行链**，因为 Agent 端不支持平台下发的 `targets`。
> 本稿把 R6 的硬前置（下称 **R0：Agent 托管目标应用**）与 R6 本体合并为一个立项，
> 只固化边界、状态机与分阶段计划。**未改任何代码 / 未起任何容器 / 未动生产状态。**

---

## 一、为什么必须单独立项

R6 预热接管的诉求是「候选端**预置配置但不启动**，确认故障后**单指令启动**」，
把切换段从「改配置 + 下发 + 启动 + 验证」压缩到「启动」。

但现状是：**平台下发的采集目标根本到不了数据面**。

| 证据类型 | 事实 |
|---------|------|
| 代码 | `sagent/config_apply.go:190-192`：`targets` 非空直接 `return errors.New("managed target application is not supported yet; existing collectors were not changed")` |
| 代码 | `applyPlatformConfig` 只解析并应用 `host_metrics` 一个段，没有第二个段落的运行时 |
| 运行态 | 采集机的远端采集目标实际来自其 **SAgent.yaml**（如 `sagent-proxy.yaml` 的 `mysql_probe.targets`），与平台配置文档无关 |
| 运行态 | 迁移只改平台侧归属记录 + 期望配置文档，数据面「在新采集机上真正起 exporter」从未发生 |

**结论**：现有 HA-3 迁移是「平台侧改账」；要在其上做预热接管，必须先让 Agent 能**按平台声明启停远端采集插件**。
该改动改变 Agent「配置即生效」的既有语义、影响**所有** Agent，属架构级改动，故单独立项定边界。

---

## 二、现状事实（代码依据，非虚构）

| 能力 | 现状 | 出处 |
|------|------|------|
| 配置应用入口 | `applyPlatformConfig` 仅支持 `host_metrics`；`targets` 非空即报错 | `sagent/config_apply.go:175-204` |
| 配置应用语义 | 版本单调递增；同版本必须 checksum 一致；`apply` 返回**回滚闭包**；received/applied 双落盘；开机 `Restore` 上一版 | `sagent/internal/control/apply.go:34-104` |
| 插件运行时接口 | `PluginName` / `Running` / `Start` / `Stop` / `Signal` / `Apply`；**目前仅 `hostRuntime` 实现 `Apply`**（热更 + 回滚） | `sagent/config_apply.go:41-109`、`sagent/main.go:183-184` |
| 远端采集插件 | 已存在为 builtin：`mysql_probe` / `scrape_plugin` / `port_checker` / `exec_scripts` | `sagent/internal/plugin/builtin/` |
| 远端目标来源 | SAgent.yaml 静态配置，开机 `initPlugins` 一次性起 | `sagent/main.go:180-191` |
| L0 漂移执行 | **先新后旧**：改归属 + 新端 `syncAgentConfig`，再旧端 `syncAgentConfig` 停 | `l0-console/relocate.go:197-206`、`l0-console/agent_api.go:702` |
| 期望/生效配置回报 | `cfg_desired` vs `cfg_effective`（Agent 回报 `applied_version`） | `l0-console/agent_api.go:187-213` |
| 能力上报 | `SelfDesc{Type, Plugins, HeartbeatInterval}`——**无能力位** | `sagent/internal/control/client.go:21-27` |

**关键推论**：`ConfigApplier` 已经提供了「版本 + 校验 + 回滚 + 落盘 + 开机恢复」的完整骨架，
R0 应当**复用**这套语义，而不是另造一套下发通道。

---

## 三、范围边界

### 本稿包含
- **R0-a** Agent 侧「托管目标应用」：按平台声明启停远端采集插件（`prometheus_scrape` / `mysql_probe` 等）
- **R0-b** `standby` 预置态：配置与版本包就位、连通性已验证，**但采集插件不启动**
- **R0-c** 能力协商：L0 按 Agent 能力决定是否下发 `targets`（防止把老 Agent 打挂）
- **R6** 预热接管：候选端预置 → 故障确认后单指令启动 → 完成判定

### 本稿不包含（明确排除）
- 多副本双活探活（**单归属不变**，预置的是配置副本、不是进程副本）
- 新增探测链（SPEC D 档，触红线，另起立项）
- 目标内主备端点切换（B16 已落地，`Replicas` + `failbackThreshold`）
- L1 四进程拆分（既有独立立项）
- 数据面直连管控组件（红线不变）

---

## 四、核心机制与边界定义

### 4.1 期望态表达：条目级 `desired_state`

平台下发的 `targets` 每个条目带一个**期望态**，而非一个全局开关：

```
desired_state: active | standby | absent
```

- 用**条目级**而非全局开关的原因：同一台采集机上会**同时**存在
  「自己是属主的 active 目标」与「为别人热备的 standby 目标」，全局开关无法表达。
- Agent 侧维护 `observed_state` 并回报 L0，形成 `desired` vs `observed` 双口径（与既有 `cfg_desired`/`cfg_effective` 同构）。

### 4.2 状态机（本立项的核心）

**合法状态**

| 状态 | 含义 | 资源占用 |
|------|------|---------|
| `absent` | 平台未声明该目标，Agent 无任何本地痕迹 | 无 |
| `standby` | **预置态**：配置已落盘、版本包已校验、连通性已验证；采集插件**未启动** | 磁盘（配置+包），**无进程、无 scrape 带宽** |
| `active` | 采集插件运行中，指标已入链 | 进程 + scrape 带宽 + 承载数 |
| `failed` | 启动尝试失败（包校验不过 / 连通性失败 / 进程起不来） | 无（保留失败原因供诊断） |

**迁移表（前置状态 → 后置状态）**

| 操作 | 前置状态 | 后置状态 | 触发者 | 幂等语义 |
|------|---------|---------|--------|---------|
| 预置 | `absent` | `standby` | L0（配置下发） | 已 `standby` → **SKIP 返回成功**，不重复落盘 |
| 接管 | `standby` | `active` | L0（**单指令 start**） | 已 `active` → **SKIP 返回成功** |
| 回收 | `standby` | `absent` | L0（配置撤销） | 已 `absent` → SKIP |
| 释放 | `active` | `absent` / `standby` | L0（属主回收） | 已非 `active` → SKIP |
| 启动失败 | `standby` | `failed` | Agent | 重试须**显式**指令，不自动重试 |
| 恢复 | `failed` | `standby` | L0（重新预置） | — |

**终态保护（双方向，必须同时满足）**
- 方向一：显式 `stop` 打的终态**不被心跳/配置拉取覆盖**。
- 方向二：属主回收后，`standby` 必须**能被再次接管**（清终态），否则预置态永久卡死。

### 4.3 单归属如何不被破坏（最关键边界）

- **`active` 全局唯一**由 **L0** 保证：`targets.agent_id` 是唯一属主。
- 同一 target 可在**多个候选端同时 `standby`**——那是**配置副本**，不是进程副本，不构成双写。
- **Agent 不得自主 `standby → active`**：无自主升级、无自愈抢主。
  唯一升级路径是 L0 的单指令。这条是防脑裂/防双写的**根本边界**。
- 预置集合 = 池内候选（同租户 + 同池 + 健康 + 非属主），规模随池大小（3-5 台）。

### 4.4 承载数与产能口径

- `load`（承载数）**只算 `active`，不算 `standby`**——`standby` 不起 exporter、不占 scrape 带宽。
- 但 `standby` 占**预置槽位**，需定上限（建议 ≤ 池内候选数，即全池热备）。
- 若不区分口径，「预置越多、负载越高」会误导自动挑机，把候选端越挑越偏。

### 4.5 异步完成判定（接管到底成没成）

- **`start` 指令返回 success ≠ 已接管**（继承既有工程教训：异步操作的结果时机）。
- 完成判定采用**双证**：① Agent 回报 `observed_state=active` 且 `applied_version` 前进；② **首帧指标入链**（与既有 `verify_probe` 同口径）。
- 超时未达成 → **回落「现配现起」路径**（改配置+下发+启动+验证），**不降级到不可用**。

### 4.6 能力协商（兼容性硬边界）

- **老 Agent 收到含 `targets` 的配置会直接 apply 失败**（`config_apply.go:191`）。
- 若 L0 无脑下发：配置失败 → 触发回滚 + 告警风暴，**影响所有 Agent**。
- 因此 **L0 必须按 Agent 能力 gate**：仅对「支持托管目标」的 Agent 下发 `targets`；
  老 Agent **不收到** `targets`，其远端目标继续走 SAgent.yaml 静态配置（即**现配现起**）。
- 协商方式待定（见 §七）：`SelfDesc` 增 `capabilities`（推荐）或按 `version` 比较。

### 4.7 失败兜底与回滚

- **预置失败**（包校验不过 / 连通性失败）→ 该候选标记 **not-ready**，**不可作为接管目标**；
  否则会出现「切过去才发现起不来」的最坏情况。
- Agent 侧 `Apply` **必须返回回滚闭包**（沿用 `ConfigApplier` 语义），失败自动恢复上一版。
- 破坏性动作（替换属主）前**备份关键路径**，失败自动恢复。

### 4.8 进程组边界

- 一个 `active` target = 一个 exporter 子进程（或 scrape 插件实例）。
- **停止必须终止其派生的全部子进程/插件进程**（PID 文件 + `/proc/PID/exe` 路径匹配 + `pgrep` 三重兜底），否则残留僵尸进程。
- `standby` 不产生进程 → 无残留风险；`active → 回收` 必须清干净。

---

## 五、红线（继承 + 新增）

继承既有 7 条（幂等改映射 / 终态保护双方向 / 防双写防递归 / 不跨租户 / 数据面不直连 / 不新增探测链 / 池内不足不硬迁），
本立项**新增 3 条**：

8. **`active` 唯一性由 L0 保证，Agent 不得自主升 `active`**——唯一升级路径是 L0 单指令。
9. **配置语义变更必须能力协商**——不得把不支持 `targets` 的老 Agent 打挂。
10. **`standby` 不等于 `active` 的产能**——承载数只算 `active`，预置不得虚增负载口径。

---

## 六、分阶段实施计划（每步可独立交付 / 回滚）

| 阶段 | 内容 | 验证门禁 | 涉及文件（预计） |
|------|------|---------|------------------|
| **R0-1** 托管目标运行时 | Agent 侧 `targets` 运行时：解析 → 校验 → 启停远端插件 → 回滚闭包 | 单测：apply / rollback / 幂等 / 进程组清理 | `sagent/config_apply.go`、`internal/plugin/builtin` |
| **R0-2** `standby` 预置态 | 条目级 `desired_state`；`standby` 落盘 + 校验但**不起进程**；`observed_state` 回报 | 单测：`standby` 不产生进程、回报口径正确 | `sagent/config_apply.go`、`internal/control` |
| **R0-3** 能力协商 | L0 侧 gate（能力位/版本）；老 Agent 不收到 `targets` | 单测：老 Agent 配置不被打挂、回落静态配置 | `l0-console/agent_api.go`、`sagent/internal/control/client.go` |
| **R0-4** L0 预置下发 | relocate 侧对池内候选下发 `standby` 预置；预置集合上限 | 单测：预置集合 = 池内非属主健康成员 | `l0-console/relocate.go` |
| **R6-1** 单指令接管 | 故障确认后单指令 `start`；双证完成判定；超时回落 | **端到端：切换段耗时实测** | `l0-console/relocate.go`、`agent_api.go`、SAgent 侧 |
| **R6-2** 预置回收 | 预置失效（目标配置变更/版本过期）、上限裁剪、回收留痕 | 单测 + 运行态观测 | `l0-console/relocate.go` |
| **R7** 前端证据链 | 展示 `standby`/`active`、预置就绪、检测耗时 | 浏览器验证 | `static/js/panel.js` |
| **R8** 回归 | `go vet` / `go build` / 全量单测 + compose 回归 + `?v=` bump | 全绿 | — |

> 后端新增配置项一律走 `main.go` 顶部 `cfg*` 环境变量登记（红线 2/3）。
> 每阶段独立可回滚；每阶段落地后更新交付清单并登记运行态证据。

### 量化目标
- 检测段：已达成（采集机 10s 短心跳 + 阈值 30s + 扫描 10s ⇒ 最坏 ~40s）。
- 切换段：现为「改配置 + 下发 + 启动 + 验证」；预热接管后压缩到 **单指令启动 + 首帧入链，目标 ≤ 3s**。
- 端到端：在已达成 ~45s 基础上，切换段不再叠加，保持 SPEC「≤45s」并留出余量。

---

## 七、待你确认的决策项

| # | 决策点 | 本稿建议默认 | 备选 |
|---|--------|-------------|------|
| 1 | 期望态表达 | **条目级 `desired_state`**（可表达同机 active+standby 并存） | 独立字段 / 复用配置版本号 |
| 2 | 预置范围 | **池内全部候选**（全池热备，池仅 3-5 台，成本可控） | 只预置 top-1（省资源，但次选接管仍需现配） |
| 3 | 属主恢复后预置是否保留 | **保留为热备**（下次切换仍秒级） | 回收（省槽位，但每次切换都要重新预置） |
| 4 | 完成判定口径 | **双证**：`observed_state=active` + **首帧指标入链** | 仅 Agent 回报（快但可能"起了没数据"） |
| 5 | 能力协商方式 | **`SelfDesc` 增 `capabilities`**（语义明确、不依赖版本号硬编码） | 按 `version` 比较 |
| 6 | 老 Agent 回落路径 | **现配现起**（改配置+下发+启动+验证，功能不变、只是慢） | 拒绝迁移并告警 |

---

## 八、本次边界声明

- 本文件为**立项准备稿**，仅固化边界、状态机、红线与分阶段计划，**未改任何代码 / 未起任何容器 / 未动生产状态**。
- R0-1 ~ R8 每阶段**都需你按「立项 → 建议默认组合或改选 → 放行」**后才落地，
  严格对齐「架构改造前明确立项并确定边界」红线。
- 若你不推进此立项，本文件作为待评审输入留存；
  现有 17 服务、远程采集首期（R1–R5 已落地）与前端六模块功能**不受任何影响**。