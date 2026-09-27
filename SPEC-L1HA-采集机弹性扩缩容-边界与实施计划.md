# SPEC-L1HA：L1 采集机弹性扩缩容 边界定义与实施计划（定稿 · 2026-09-26）

> 依据：2026-09-26 需求讨论与三轮评审收敛结论；B18/B20（网络域与控制通道收敛）、D4（单端口收口）、B8（SAgent 多端点 failover）、HA-3（归属迁移与池告警）、VM-HA（数据面双副本）既有事实。
> **本稿已定稿并获授权（进 L1HA-1）。** 分期实施仍逐阶段验收，实施中不改边界。

---

## 一、目标与定位

L1 采集机是 L0 与 SAgent 之间的**中间通信枢纽**：解决网络可达、承接部署实施控制下发与采集数据上报的中转。本 SPEC 让枢纽具备弹性：

1. **L1 采集机横向扩容/缩容，L0 零配置、零动作**（至多次日看到规模变化，无需任何操作）。
2. **组件级扩缩容对 SAgent 最低感知乃至零感知**。
3. **组件按角色分组部署，非 1:1 铺满**：如 2 台跑数据面（vmauth/vmagent）、3 台跑控制面（controller/gateway/package-cache/ansible-runner），每台自选角色组合。
4. **跨资源池网络走 VIP 网络策略，不引入 DNS**（生产现实：L0 与 L1 异资源池，网络策略按目的 VIP+端口一次性申请）。
5. L1 规模随 SAgent 接入数量弹性调配。

**口径基线（评审修正后）**：
- 控制面：SAgent→gateway **单向出站**（心跳/任务拉取）。
- 数据面：vmagent→vmauth→SAgent **定向入站抓取**（仅 :19090 对 vmauth 网段开放）。"单向出站"仅对控制通道成立。
- L0 对 L1 **零配置**；L1 对 L0 **单向配置**（地址+凭据）。

---

## 二、现状事实（代码依据，非虚构）

| 环节 | 现状 | 出处 | 对弹性的含义 |
|------|------|------|------|
| 任务派发 | `PullL1Tasks` 按 `controller_id = ?` 预绑定 | `store/l1task.go:172` | controller 挂→名下任务滞留，**池化要解决的核心** |
| 拉取一次性 | pending→dispatched 事务内原子翻转 | 同上 | N 实例竞争拉取天然不重复 |
| 重试语义 | attempt + `HasInflightL1Task(flow,step,attempt)`，对账桥只认当前尝试 | `store/l1task.go` | 死亡收割后重投的地基已有 |
| 执行预算 | payload 携带 `budget_sec` | `onboard_l1_dispatch.go` | 收割超时基准现成 |
| SAgent 多端点 | `SAGENT_L0_CONSOLE_ENDPOINTS` 逗号列表，**请求驱动切换**：失败试下一端点、成功记 curIdx、主恢复下一请求自动回切 | `config.go:22`、`control/client.go:415`、`client_test.go:174` | gateway 多实例的客户端兜底**零开发** |
| D4 抓取链 | vmagent →(Bearer)→ vmauth:8427（路径白名单中继+注入 Bearer）→ SAgent:19090 | `configs/vmagent/scrape.yml` 注释、`configs/vmauth/auth.yaml` | 数据面"抓取最后一公里"的清单载体在**两处** |
| SAgent 清单现状 | vmagent scrape.yml `static_configs`（逻辑名）+ vmauth url_map（白名单）均为静态 | 同上 | L1HA-2 要动态化的范围 |
| 注册→清单先例 | `configs/scrape-sd/*.json` 由 onboard_flow 自动生成（file_sd 格式，每 resource 一个文件） | `onboard_ansible.go:1647`、`onboard_l1_ack.go:143` | "注册事件→渲染清单"机制平台已有先例。**注意：先例是 L0 单写者场景（L0 进程独写挂载目录），多实例互斥渲染是 L1HA-2 新增约束，非先例自然延伸——工期评估不得引用"先例已有"压缩 L1HA-2** |
| 池告警 | 采集机池健康数 <2 告警不迁移 | HA-3 | 容量信号可延伸 |
| 数据面副本 | vmagent 双副本、VM 主+HA | VM-HA | 数据面弹性部分已收口 |
| 认证 | SAgent token（D3）、vmauth 双层 Bearer、gateway 3+2 路径白名单 | D3/D4/G2 | 安全底座已在；TLS 未落地（见四.8） |

---

## 三、目标架构

### 3.1 角色矩阵（同镜像 + env 声明）

> 注（2026-09-27）：容器 `l1-collector` 已更名 `l1-hub`（守护脚本同步改名 `l1-hub-supervise.sh`），本文历史记录中的旧名已统一替换为新名；更名时网络别名与 SAgent URL 零改动。

同一 `l1-hub` 镜像（容器整合产物，四进程 daemon flag），每台机器用角色矩阵 env 声明跑哪些组件：

| 组 | 组件 | 扩缩语义 |
|----|------|---------|
| 控制面组 | controller（池化）/ gateway（多实例）/ package-cache / ansible-runner（池化） | 起机器→配角色→入池即生效；缩容走礼仪（四.9） |
| 数据面组 | vmauth（多实例）/ vmagent（双副本起） | vmagent 多副本抓同一 vmauth、推双 VM |
| 混部 | 数据面与控制面可同机（如 2 台全装=小规模最小形态） | 角色矩阵任意组合 |

### 3.2 VIP 规划（①②一次性申请；③为规模化触发项）

| VIP | 用途 | 后端 | 申请时机 |
|-----|------|------|---------|
| ① gateway :8460 | SAgent 唯一配置入口（恒一条地址） | 控制面组全部 gateway 实例 | L1HA-1 前一次性申请 |
| ② VM 写入入口 | vmagent remote_write | L0 VM 主/HA | L1HA-1 前一次性申请 |
| ③ L0 控制入口 :8080 | controller/runner/cache 拉任务拉包 | L0 console（**现单地址即可**） | 规模化触发项：L0 console 自身多实例部署时才 VIP 化，届时另行申请，不在本期清单 |

扩缩容只变 VIP 后端成员，**不新增 VIP 申请、不改防火墙策略**。演示环境用 compose 网络别名/轻量 LB 模拟，生产由网络组按触发时机申请。

### 3.3 连接方向（全图口径）

- SAgent→VIP①→gateway→L0（控制面，出站）
- vmagent→vmauth→SAgent:19090（数据面抓取，L1 内+定向入站）
- vmagent→VM、controller/runner/cache→L0（数据与控制出站）
- **不存在任何 L0→L1 主动连接**（收割只读 PG，不连死者）
- NAT/不可达目标不改协议，走 collector_proxy 远程采集代理在可达区收口
- **B8 端点列表元素形态**：`VIP:port`（IP 形态），生产按网络组交付形态填写；列表元素是 VIP 地址而非域名解析，与"不引入 DNS"口径一致

---

## 四、关键设计决策（已定案）

1. **指标链路维持拉模式**。三理由：D4 单端口红线（vmauth:8427+Bearer+路径白名单）为拉链路设计；改推则每个 SAgent 持写入凭据，凭据泄露=可注入假指标，攻击面扩大；NAT 场景走 collector_proxy 代理是拓扑问题不是协议问题。
2. **池化派发 + 卡死收割**（核心改造，动 L0 派发层）：任务投递 `controller_id=''` 入池，任意存活 controller/runner 竞争拉取；每轮 pull 即心跳（`last_pull_at`），不加主动探活端点；dispatched 超 `budget_sec + 宽限` 无 ack → 判持有者死亡 → attempt+1 → 回池重投，旧回执被对账桥按 attempt 丢弃。**agent-action 任务不池化**（`controller_id=agentID` 绑死目标，通道 A 语义不变）。顺序性：流程引擎逐步投递，同一 flow 在飞任务 ≤1，无并发抢跑。
3. **清单动态化范围=两处**：vmagent 抓取目标（改 file_sd）+ vmauth url_map。渲染职责归 controller（已持注册视图，不新增第八进程），复用 onboard_flow→scrape-sd 先例（先例为 L0 单写者，见二）。**渲染互斥与幂等（L1HA-2 即交付，不等池化）**：渲染由周期对账驱动（非事件驱动），输出只依赖注册表当前状态——幂等、重入安全；多 controller 并发用 PG advisory lock 互斥（复用 PG 唯一载体既有依赖，零新组件），抢到锁者渲染，渲染前后各比对一次清单版本，无变化即跳过写入。**对账周期与生效上界**：默认 30s（env 可调）；新注册 SAgent 纳入抓取与白名单的延迟上界 = 对账周期 + 一次 vmagent 抓取周期（`scrape_interval`，L1HA-2 实施时钉死具体值），L1HA-2 验收按此口径实测登记，不承诺即时生效。**锁持有语义**：advisory lock 为 **session 级**（渲染含文件写与 rename，不可能全程在事务内）；进程崩溃时连接断开由 PG 自动释放；优雅退出时主动 unlock 再退（见决策 9）。
4. **原子更新语义："先放后抓、先停后收" + 失败自愈**。新增：先渲染 url_map 放行（无人请求，无副作用）→ 再更新 scrape.yml 开始抓；移除：先摘 scrape → 再收 url_map（反序会出现短暂 401 告警）。**跨文件写的事务化**：渲染先落临时文件，url_map 与 scrape.yml 两处全部写成功后逐一 atomic rename（组件经 `-configCheckInterval` 热加载），任一失败则临时文件丢弃、不 rename、现网状态不变；每次渲染周期先做**一致性自愈**——拉注册表与现网清单 diff，发现上一次半途失败留下的漂移即优先修复。回滚=按相反顺序回退。**热加载能力已核实**（官方文档，L1HA-2 实施前提）：vmagent 与 vmauth 均原生支持 `-configCheckInterval` 热加载（vmauth 另有 SIGHUP 与 `/-/reload`），重载失败沿用最后一次成功配置（`vmauth_config_last_reload_successful` 指标可观测）——无重启窗口；compose 中 vmauth 现未挂该 flag，L1HA-2 实施时补上。
5. **B8 切换时间 SLA（可测形式，数值实测回填）**：控制通道为请求驱动切换（`client.go:415`），端点故障→切换生效 ≤ **2×max(客户端请求超时, 请求周期)**；主端点恢复→下一请求自动回切。请求超时与请求周期均为 SAgent 侧可配置项，SLA 承诺值以**默认配置**为基准，L1HA-4 杀实例实测后回填具体秒数。抓取侧走 HA-3 同款健康周期探测 + `failbackThreshold` 防抖回切，同一形式登记。
6. **VIP/健康检查归网络层**：云 LB 或 keepalived 双活、健康检查与后端摘除由 LB 层承担，**平台代码零开发**；VIP 整体故障由 B8 多端点客户端兜底（二线防御，SLA 见决策 5）。若部署形态选 haproxy 且后端成员需要动态渲染，**另立子项单独立项**——不默认包含，不用"可复用"模糊带过。
7. **网络策略静态化**：采集机从**预留 IP 段**分配，防火墙按网段放行；段内扩缩容零网络变更，段外扩容才需申请新策略。
8. **TLS P1 触发条件**（命中任一即实施，否则演示内网维持 Bearer）：① 链路跨资源池边界或经公网/专线；② 生产准入安全评审要求传输加密；③ 客户合规要求（等保）。
9. **缩容礼仪（任务空窗=秒级，不等收割器）**：摘 VIP 后端 → 停止拉取（不再接新任务）→ **主动回池在飞任务**——本机 controller/runner 将自己名下 dispatched 任务立即置回池（attempt+1），幸存者下一拉取周期竞争接手，不等待 `budget_sec + 宽限` 的超时收割 → 进程退出。收割器仅兜底**急断**（机器直接宕机）场景，礼仪路径不依赖它。缩容不丢任务、空窗可控。**回池实现边界**：① 回池范围=**未开始执行**的 dispatched（已 ack 进入执行的在本机等其跑完上报，不回池——否则幸存者接手后同一任务跑两遍）；② **agent-action 任务不回池**（`controller_id=agentID` 绑死语义，本机在飞则等完成，超时由收割器按 attempt 兜底）；③ 回池操作为**单事务批量** UPDATE（`controller_id=''` + attempt+1），失败整体回滚重试，不逐条提交。**持渲染锁时的退出语义**：若退出信号到达时正持 advisory lock 渲染，先完成本次渲染 + rename 再退（秒级）；渲染未开始则丢弃临时文件直接退、不回滚现网——漂移由下一周期一致性自愈修复（决策 4）。

---

## 五、分期实施与验收口径

| 阶段 | 内容 | 验收口径 |
|------|------|---------|
| L1HA-1 | 角色矩阵 + 分组编排 + VIP 模拟（**LB 容器 + 健康检查**，非 compose 别名——别名不会摘死实例）。载体=容器整合（`l1-hub` 四进程合一 + 角色 env） | 杀 1 个 gateway 实例：① LB 层——健康检查摘除后端，新连接不再进死实例；② SAgent 层——心跳不断、配置零改动（B8 兜底验证）。**交付容量基线**：每台 controller 支撑 SAgent 数的经验值（实测给出）+ 扩容触发线（如 CPU>60% 持续 5min / 池健康数，写入告警中心） |
| L1HA-2 | 清单动态化（scrape file_sd + url_map 渲染 reload，归 controller）+ PackageCache 消费侧 URL 列表化。**本阶段即含渲染互斥（advisory lock）、临时文件事务化、一致性自愈——以多实例安全为交付标准，不留到 L1HA-3 返工**。实施时给 vmauth 补 `-configCheckInterval`（两组件原生支持热加载已核实，见决策 4） | 新 SAgent 注册后自动纳入抓取与白名单，零手工改配置，**纳入延迟 ≤ 对账周期 + 一次抓取周期（默认 30s 基准，实测值登记）**；"先放后抓"顺序实证（无 401 窗口）；**并发双 controller 渲染无写冲突**——验收方法：起两个 controller 实例（不同 controller_id）、同时触发注册事件，断言清单文件最终一致且渲染日志只有一个执行者 |
| L1HA-3 | 池化派发 + 卡死收割 + 缩容礼仪（核心，动 L0 派发层） | 杀任务持有者：attempt+1 被幸存者重跑、旧回执被对账桥丢弃、flow 照常 done；缩容 5→3 无任务丢失且**在飞任务空窗=秒级**（主动回池生效，不等收割超时） |
| L1HA-4 | 终验：3→5 扩容、5→3 缩容、混杀（控制面+数据面各杀） | **断言：L0 侧变更 diff=0、SAgent 配置 diff=0**；B8 切换时间实测值登记对照 SLA |

每阶段完成后按惯例登记《交付清单-采集平台代码对齐-阶段.md》。

---

## 六、边界外（明确不做）

- **不改指标推拉模式**（决策 1）；**不建 L1 内部服务发现**（全组件上游收敛 L0，无互调需求）；**平台代码不做 VIP 管理与健康检查**（网络层职责）；**不引入 DNS**。
- agent-action 通道不池化（通道 A 语义不变）。
- B5（vm-host 搬 l1-net）仍以 B4b 完成为前置，顺序红线不变，本 SPEC 不改变其排期。

---

## 七、依赖与顺序

1. **前置**：容器整合（l1-hub 四进程合一 + 角色矩阵 env）先收口单机形态——L1HA-1 与其是同一件事的两面（整合定单机，L1HA 定集群）。
2. **渲染互斥/事务化/自愈在 L1HA-2 交付即按多实例安全实现**（决策 3/4），L1HA-3 池化落地时渲染层零返工——渲染只依赖注册表状态与 advisory lock，不依赖 controller 实例数。
3. L1HA-3 动 L0 派发层，独立分支 + `tests/run_all.sh` 全量回归（PG 载体零 SKIP 口径）。
4. 文档波及：SPEC-D5 网络域红线表述随容器整合改写；交付清单中"l1-gateway 内解析不到 l0-console"等历史实证加合并后标注。
