# SPEC-VM-HA：中心/汇总层（VMStorage·VMAgent）高可用 边界定义与实施计划（立项准备稿）

> 依据：《PLAN-架构设计与高可用运维方案.md》决策记录 **P1/P2 VMStorate/VMAgent HA**（第 167-168、179 行）。本文件为立项边界稿，供评审后分阶段实施。
> **本稿只定边界与计划，不动代码、不改 compose、不起容器。** 仅在你逐阶段授权后按第四/五节执行。

---

## 一、目标与定位

消除 G4 缺口（PLAN:95「VMStorage/VMAgent 无 HA，中心与汇总层单实例」），让数据面两条链具备故障冗余：

| 层 | 组件 | 目标 |
|----|------|------|
| 汇总层（L1） | `vmagent` | 多副本 + `remoteWrite` 多端点 fallback + 本地缓冲兜底 |
| 存储层（L0） | `victoriametrics` | 多副本/集群，`-replicationFactor=2`，远端去重由 VM 侧处理 |

**优先级别**：P2——**指标丢失可容忍，优先级低于控制面**（PLAN:179）。不因此抬高运维复杂度、不与控制面 HA 抢序。

**红线（继承）**：数据链 `exporter→vmagent→vmstorage` **不得经过管控组件**；万级 SAgent 数据必经 L1 `vmagent` 汇聚再与 L0 通信，SAgent 不直连 L0；vmauth 单端口 + auth_key + path 白名单（D4）仍是 SAgent `/metrics` 的唯一出口。

---

## 二、现状事实（compose 依据，非虚构）

`deploy/docker/docker-compose.yml` 实测：

| 服务 | 现状 | 备注 |
|------|------|------|
| `victoriametrics`（L0 中心） | 单节点 `l0-victoriametrics` v1.93，`:8428`，**非 clusterMode，无副本** | 众多 L0 服务 `depends_on` |
| `vmagent`（L1 汇聚） | 单副本 `l1-vmagent` v1.93，`remoteWrite.url=http://victoriametrics:8428/api/v1/write`（**单端点**），缓冲 `--tmpDataPath=/tmp/vmagent-buffer` + `--maxDiskUsagePerURL=2GB` | 缓冲已具备（数据通路兜底的一半） |
| `vmauth`（L1 安全前端） | 单前端 `l1-vmauth` v1.93，单端口 + 白名单 | D4，仅本机可达 |
| 卷 | `vmagent-buffer`（共享） | 多副本需评估缓冲卷共享/拆分 |

**关键推论**：缓冲（2GB tmpDataPath）已存在 =「exporter→vmagent 失败本地暂存、恢复补写」已承接；真正缺口是**存储无副本** + **vmagent 单副本/单端点**。因此 VM-HA = **拓扑多副本化 + 多端点 fallback**，不改采集语义。

---

## 三、核心机制（两层 HA 闭环）

- **汇总层**：`vmagent` 增副本（同 L1 内 scale 或跨 L1 各自副本），`remoteWrite.url` 配**多个 vminsert/vmstorage 端点**（逗号并列）；单副本把目标各拉一份，远端**去重由 VM 侧 `-dedup.minScrapeInterval`/`replicationFactor` 处理**；单端点失败按序 fallback；缓冲 2GB 兜底断连补写。
- **存储层**（两选一，见四.1）：
  - A) **可复制单点**：VictoriaMetrics 多实例副本 + `-replicationFactor=2`（VM 统一读，就近副本可读）；
  - B) **集群模式**：`-clusterMode`（vminsert/vmselect/vmstorage 三组件），`vmstorage` 多副本 + `replicationFactor=2`，`vmselect` 查询聚合。
- **数据通路兜底**：任一层短时故障，靠 vmagent 缓冲 + 恢复后补写，指标不静默丢。

---

## 四、需你定界的决策点（本 SPEC 不代决）

| # | 决策点 | 选项与建议 | 影响 |
|---|--------|-----------|------|
| 1 | **存储层形态** | A) 单节点复制副本（`-replicationFactor=2`，改动最小，镜像同为 v1.93）★建议；B) `-clusterMode` 三组件集群（更弹，但引入 vminsert/vmselect 编排面，运维更重） | 决定中心层拓扑复杂度；P2 建议取轻的 A |
| 2 | **vmagent 副本数/来源** | 同 L1 内 2 副本（简单 failover，VM 侧去重）vs 跨 L1 各自 1 副本（天然多机冗余，但目标需在各自域内都有） | 决定去重与目标可见性语义 |
| 3 | **remoteWrite 多端点粒度** | vmagent `remoteWrite.url` 并列多个 L0 存储端点（主备 fallback）★建议；是否另配 `remoteWrite.shardByURL` | 决定存储故障时的写面冗余 |
| 4 | **缓冲卷策略** | A) 保留共享 `vmagent-buffer` 单卷（多副本竞争写，需 VM 侧去重）；B) 每副本独立缓冲卷（隔离更干净但卷数翻倍/排障成本高） | 决定多副本是否 write-conflict |
| 5 | **验证门禁强度** | 端到端：杀任一副本/停单存储节点 → 指标在 VM 侧仍可查、无静默丢（容忍窗口内），再恢复 | 决定 HA 是否被实证而非仅拓扑声明 |
| 6 | **是否引入 vmalert/告警水位** | PLAN:205 提及 VM 告警（心跳断连/task 失败/exporter 不可用/上报丢失/存储水位）；本轮是否同步加，还是只做数据面 HA | 决定是否把「存储水位/丢失」纳入现有内部告警引擎 |

> 建议默认组合：**1A + 2 同 L1 双副本 + 3 多端点 fallback + 4A 共享缓冲 + 5 杀进程实证 + 6 存储水位纳入现有告警**。逐项待你确认。

---

## 五、分阶段实施计划（每步可独立交付/回滚）

| 阶段 | 内容 | 验证门禁 | 涉及（预计） |
|------|------|---------|--------------|
| M1 存储多副本 | `victoriametrics` 副本节点 + `-replicationFactor=2`（A 案）或 clusterMode（B 案） | `docker compose up` 起多存储副本，VM `/api/v1/query` 两副本可查一致 | docker-compose.yml、configs |
| M2 vmagent 多端点 | `vmagent` 副本化 + `remoteWrite.url` 并列存储多端点 + 缓冲策略 | 单副本 down → 指标仍入 VM | docker-compose.yml、vmagent 启动参数 |
| M3 去重与一致性 | VM 侧 `-dedup.minScrapeInterval` / replicationFactor 去重实证（无重复时间序列） | 查同一序列，无重复采样点 | compose/vm 参数 |
| M4 数据通路兜底验证 | 断连写缓冲→恢复补写补齐，无静默丢 | SAgent 上报 → 断连 → 恢复 → VM 补齐前后一致 | 端到端指标对比 |
| M5 告警水位（若 6 选） | 存储水位/上报丢失纳入内部告警引擎（只读信号，不加探测链） | 告警单测 + 前端告警中心可见 | 告警引擎、规则表 |
| M6 回归 | `docker compose` 全服务 17 起、健康检查、数据链验证（`/api/v1/targets` 良好、指标入库可查） | 全绿，`?v=` 如需 bump | compose、前端 |

> 每阶段独立可回滚；后端/告警改动符合既有红线，仅读信号、agent-action 沿用外部通道。

---

## 六、红线

1. **数据链不经管控组件**（红线）：所有 VM 拓扑改动只发生在数据面；L0 管控只是编排，不劫持 exporter→vmagent→vmstorage。
2. **万级汇聚**：SAgent 不直连 L0；即便存储多副本，写面仍经 L1 `vmagent` 汇聚。
3. **P2 优先级**：不做威胁控制面优先级的重编排；复杂度以「指标丢失可容忍」为上限。
4. **不造伪 HA**：每阶段以「杀进程实证不静默丢」为门禁，绝不止于拓扑声明。

---

## 七、本次边界声明

- 本文件为**立项准备稿**：仅固化边界、决策点与分阶段计划；**未改 compose / 未起容器 / 未动运行态**。
- M1–M6 每阶段都需你按「立项→确认默认组合或改选→放行」才落地，对齐「破坏性/数据隔离/部署拓扑改动需立项授权」红线。
- 若你暂不立项本项，文件作为待评审输入留存，不影响现有 17 服务、数据链与前端六模块。

---

## 附：决策点默认组合（一次性可全选）

1A（单节点复制副本 replicationFactor=2）· 2（同 L1 双副本）· 3（多端点 fallback）· 4A（共享缓冲+VM侧去重）· 5（杀进程实证）· 6（存储水位入告警）
若你回复「按默认组合立项 VM-HA」，我即从 M1 落地并每阶段门禁验证。