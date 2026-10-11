# SPEC-B4b：探路执行端下沉 L1 —— 设计与实施计划

> 依据：用户 2026-10-07 决策——「探路 = 安装前的环境检查 + 目标可达性验证，这一步应该在 L1 侧执行」。
> 父 SPEC：`SPEC-D5-IN5-安装执行端搬迁边界与分阶段计划.md` §四 **B4b**（余下 5 个 atom 中的 `preflight_host`）。
> 前置：**B2 / B3 / B4a 已落地**（投递链路 + 回执对账桥 + 支持集开关），本次只在其上**加一个 kind**，不另造通道。
> 本文只做**设计与边界（立项）**，不含实现。确认后再动手。

---

## 零、立项结论（先看这一句）

把 `preflight_host`（探路）从「**L0 进程内 ansible**」搬到「**L1 Ansible Runner 执行**」，
复用 B3/B4a 已经跑通的「投递自足载荷 → Runner 执行 → 结构化回执 → L0 对账桥收口」通道。

**只加 kind（`probe` / `probe_remote`）+ 收口分支**，不新增通道、不新增表、不动状态机。

附带要定义清楚一件事：`preflight_remote`（`preflight_ability`）**目前根本没有平台代执行通道**
（不在 `ansibleKindOfAtom` 映射里），只会落「下发执行包 + 人工回报」并**傻等 3600s** —— 它必须一并定落点。

### 零.一 网络拓扑原则（2026-10-07 用户锁定，最高优先，先于一切设计）

> 本节是**架构红线**，任何与本原则冲突的实现/文案/选项一律作废，不再逐条讨论。

| # | 原则 | 推论（对本次的影响） |
|---|---|---|
| P1 | **采集机（L1）是 L0 与 L1 之间的沟通桥梁与控制枢纽** | 所有面向被管主机的动作都以采集机为中转，L0 不越过采集机直接碰主机 |
| P2 | **所有采集/被管对象 target 的网络必须与 L1 侧采集机双向直通** | 下行：ansible 安装/部署；上行：采集数据。**探路必须从采集机侧发起，探测 target** |
| P3 | **L1 与 L0 的网络可以直通**（L0↔L1 直连允许） | 投递链路 L0→L1 Controller→Runner 合法；**但 L0↔target 不通** |
| P4 | 采集机自身事实（OS/架构/磁盘/端口/是否已有 SAgent）**在采集机侧就地获取** | 不额外 SSH 到第三台机器；`pick_version`/`install` 的 `when` 依据采集机本地事实 |
| P5 | **探路必须与安装走同一条路径**：同一执行端（L1 Runner）、同一落点主机、同一凭据、同一机制（ansible/SSH） | 严禁"另起一条探测链"（L0 直连、裸 TCP/`nc` 旁路、单测一条链）。**TCP 探通 ≠ 实际 ansible 部署能成**——两套判据并存会互相打架，且掩盖真实故障 |

**一句话**：`preflight_host` 的**执行端 = 采集机（L1 Runner）**，**探测对象 = target（被采对象）**；
采集机自身事实**就地取**。这与 §三 D1 的结论一致——**D1 由本原则直接裁定，不再作为待拍板项**。

### 零.二 远程 vs 边缘：探路口径必须分开（2026-10-07 用户补充锁定）

> 用户原话：「remote 安装和边缘安装应该逻辑有些区别，remote 只要探测目标对象的 ip、端口、
> 进程是否存在、口令密码是否正确、目标对象能否连接成功 大概就行了吧？」
> 以及：「L0 侧应该维护或管理一张 ssh 所需的主机用户和口令、端口表吧？」

**remote 与 edge 是两条不同的探路语义，不能共用一套**：

| 模式 | 探路要回答的问题 | 执行端 | 落点与机制 | 是否需要 SSH |
|---|---|---|---|---|
| **remote**（承载机=采集机，被采对象零侵入） | 被采对象**本身**：IP/端口可达、服务在（进程在）、**口令正确**、能连上（1:1 采集路径） | L1 Runner（采集机侧） | 从 **Runner 本机（=采集机）** 直连被采对象，按插件类型做**真实协议探测**（Redis：`AUTH`+`PING` 期待 `+PONG`；其余类型走 TCP 兜底） | **否**——不 SSH 到采集机，也不 SSH 到被采对象 |
| **edge / hybrid**（承载机=资源自身） | 目标主机**事实**（OS/架构/磁盘/端口/是否已装 SAgent）+ 能力可行性 | L1 Runner | SSH 到资源主机（ansible ping/setup/df/env/ability），**1:1 安装路径** | 是——落点=资源自身 |

**关键推论**：
- remote 模式**不装 SAgent 到被采对象**（零侵入），也没有"探路 SSH 到采集机"这一步——采集机已存在
  （`pick_proxy` 只挑已注册心跳的采集机 → `install_agent` 的 `when: !agent_exists` 恒为真跳过）。
  故 remote 的探路 = **对目标对象的一次真实连接核验**，机制与「采集」同源（P5 的 1:1 落在采集链路上）。
- **「裸 TCP 探通 ≠ 能采到」**：口令错、库不可用都能让 TCP 通而采集失败，故 remote 探路必须做
  **协议级 + 认证级**核验，不能只 `wait_for` 一个端口。
- 采集机凭据口径：L0 侧统一维护**SSH 主机/用户/口令/端口**台账（现状落在 `resources` 表的
  `ip/ssh_user/ssh_password/ssh_port` 列）。edge 探路/安装按资源行取；**remote 探路不取 SSH 凭据**
  （被采对象的口令来自 `collect_params` 参数，随载荷下发，明文不进探路日志与证据）。

### 零.三 SSH 接入凭据台账（2026-10-07 用户补充锁定）

> 用户原话：「L0 侧应该维护或管理一张 ssh 所需的主机用户和口令、端口表吧？」

**结论：这张表 L0 已经有了——就是 `resources` 行的 `ip / ssh_port / ssh_user / ssh_password`**
（`store/onboard.go:167-170`，按主机标识落行，带 `tenant_id` 隔离）。**不需要新建表。**

但它有两个必须补的口径（现状错在"取错行"，不在"缺表"）：

| # | 现状事实 | 必须改成 |
|---|---|---|
| **G1** | remote 模式 `tryAnsibleExecutor` 用 `sshCredOf(r.resource)` 取 SSH 凭据（`onboard_ansible.go:232`）——`r.resource` 是 `pick_object` 选的**被采对象** | SSH 落点一律按 **`flow.AgentID`（采集机）** 取：`agents.resource_id` → 采集机资源行 → `ssh_*`。remote 模式下被采对象**不是** SSH 落点 |
| **G2** | 采集机无资源行 / 无凭据时，会静默落到"被采对象凭据"这条错路 | **显式失败 + 可执行诊断**：「采集机 <id> 未登记 SSH 接入凭据，无法 ansible 探路/安装；请为该采集机登记 主机/端口/用户/口令」。**绝不静默回落**到被采对象凭据 |

**为什么复用 `resources` 而不新建 `host_credentials` 表**：
- `resources.ssh_*` 本就是"主机级接入凭据"语义（按主机标识落行）；采集机也已被强制登记为平台设备资源行（`role=platform_device`）。
- 新建一张按 host 的表 = edge 模式出现**两个凭据来源**，违反"同一实体只有一个账本"（工程经验 §4）。
- 若将来确有"同一主机多套凭据 / 轮换"需求，再单独立项迁移；**本期不引入**。

**凭据登记入口（随 G2）**：`pick_proxy` 选定采集机后，若该采集机资源行无 `ssh_*`，步骤 detail 下发
`need_ssh_cred: true` + 采集机 host；前端决策区给"登记采集机 SSH 凭据"入口（复用资源表单的 ssh 三字段），
登记后重试探路。这是 G2 的"修复路径从猜变成选"，与 `autoResolveAccessPoint` 的兜底自诊断同一风格。

**与 P4 的关系（拓扑澄清）**：生产 4 进程模型下 Runner 与采集机**同机**，采集机自身事实"就地取"（无 SSH）；
但**演示环境** `l1-hub`（Runner）≠ `sagent-proxy`（采集机）是两台容器，故演示期采集机事实与安装仍需
**SSH 到采集机**——这正是 G1 必须成立的原因（取错行时演示直接断链）。

### 已锁定的决策（2026-10-07 用户确认）

| 决策 | 结论 |
|---|---|
| **执行端** | 探路在 **L1 采集机侧**执行（L1 Ansible Runner）——解决 B1 |
| **探测对象** | **目标 target 主机**（被采对象），**从采集机侧发起** |
| **采集机自身事实** | 在采集机侧**就地获取**（不额外 SSH 到第三台机器），用于 `pick_version` / `install` 的 `when` |
| **`preflight_remote`** | **保留为独立步骤**，新增 `probe_remote` kind 走同一通道（见 §三 D2） |
| **动手范围** | **调彻底**：B4b-1 / B4b-2 / B4b-3 全做（含 `autoResolveAccessPoint` 下沉） |

---

## 一、现状事实（带证据）

### 1.1 探路在哪执行

| 环节 | 位置 | 事实 |
|---|---|---|
| 步骤定义 | `data/flow_templates/remote.yaml:40-47` | `preflight_host`，`scope: external`，`gate: blocker`，标题「采集机探路」 |
| 走不走平台代执行 | `onboard_ansible.go:203-226` `ansibleKindOfAtom` | `preflight_host → "probe"`；**`preflight_ability` 不在映射里 → 返回空 → 不代执行** |
| SSH 目标 | `onboard_ansible.go:232` | `cred := sshCredOf(r.resource)` —— 取的是 **flow 的资源对象**凭据 |
| 执行落点 | `onboard_ansible.go:286-290` | `go ansibleProbeJob(...)` —— **在 L0 容器进程内**起 goroutine 跑 ansible |
| 是否有 L1 分支 | `onboard_ansible.go:249-251` | 只有 `l1ExecExecutor()==runner && l1ExecSupportedKind(kind)` 才投 L1；`probe` 不在支持集 |

**结论**：探路 100% 在 L0 进程内执行（`ansibleProbeJob`，`onboard_ansible.go:1066`）。

### 1.2 探路内部做了什么（`ansibleProbeJob` 的 6 个阶段）

| # | 阶段 | 干什么 | 目标机 | 产物 |
|---|---|---|---|---|
| ① | `ping` | `ansible -m ping` 连通性 | SSH 到目标机 | recap |
| ② | `setup` | gather facts（OS/架构/内核/内存/磁盘/网卡/hostname…） | 同上 | `facts` |
| ③ | `df` | `df -BG` 兜底磁盘（setup 无 `/` 时） | 同上 | `diskFromDF` |
| ④ | `env_check` | Python / **平台接入点可达性** / 时间同步 / glibc | 同上 | `envEvidence`；不可达时触发 `autoResolveAccessPoint`（`onboard_ansible.go:944`） |
| ⑤ | `ability_check` | 按所选能力 `params.yaml` 的 `probe` 绑定，从目标机侧核对**被采对象**可达性（`execAbilityChecks`，`:803`） | 同上 | `probes[]` + `blockerFail` |
| ⑥ | 回写台账 + 回报 | `UpdateResourceProbe`（`:1340`）写 OS/arch/kernel/probe_json；`autoReportStep`（`:1388`） | 无 | 步骤证据 `extra` |

门禁语义：`blockerFail` 非空 → 本步 **fail**（`:1276-1284`），**就在装机之前拦住**。

### 1.3 已有通道（B3/B4a 已落地，本次直接复用）

```
投递端（L0）
  onboard_l1_dispatch.go:68   l1ExecSupportedKind()      支持集白名单（现含 install + 卸载侧四步）
  onboard_l1_dispatch.go:277  buildStepPayload()         按 kind 构造自足载荷
  onboard_l1_dispatch.go:321  deliverStepToRunner()      取号/重入闸/幂等投递/落 running(exec_channel=l1)
  onboard_l1_dispatch.go:401  refreshL1TaskLiveness()    长任务续活跃（防误判卡死重试）
执行端（L1）
  l1_ansible_runner.go:120    runnerExecuteTask()        kind 分派
  l1_ansible_runner.go:140    runnerExecuteInstall()     版本包路径
  l1_ansible_runner.go:343    runnerExecuteOffboard()    渲染→syntax 门禁→流式执行→结构化回执
对账端（L0）
  onboard_l1_ack.go:34        reconcileL1TaskAcks()      清扫器每轮扫描（20s），重启不丢
  onboard_l1_ack.go:52        driveStepFromL1Task()      done→autoReportStep / failed→failStep
  onboard_l1_ack.go:118       卸载侧分支 → l1OffboardReconcile（重建 DTO 后走**共享收口**）
```

表结构已就绪：`store/l1task.go:35-42` 已有 `FlowID / StepID / Attempt / ResultJSON / ReconciledAt`。
**本次无需任何 DDL。**

### 1.4 断点（为什么不修走不通）

- L0 只挂 `l0-net`，被管主机在 `l1-net`，**两侧无路由**；生产网络策略更明确「万级 SAgent 不得直连 L0」。
- 实测证据（报告 §B1）：`UNREACHABLE / connect to host 10.20.0.15 port 22: Operation timed out`。
- 后果：`preflight_host` 必失败 → `pick_version` 等不到 OS/架构 → `blocked`（flow 7 至今 blocked）；
  `preflight_remote` 更是**连代执行通道都没有**，会先傻等 3600s。
- 演示环境恰好同网段，把问题盖住了——**Redis 这次能采到，靠的是配置被正确落地，不是流程跑通**。

---

## 二、边界

### 2.1 动

| # | 内容 | 落点 |
|---|---|---|
| 1 | 支持集加 `probe` | `l1ExecSupportedKind`（`onboard_l1_dispatch.go:68`） |
| 2 | 新增探路载荷构造 `buildProbePayload` | `onboard_l1_dispatch.go`（与 `buildInstallPayload` 同构） |
| 3 | 新增 `runnerExecuteProbe`（多阶段：ping/setup/df/env/ability） | `l1_ansible_runner.go` |
| 4 | 新增 L0 侧收口 `l1ProbeReconcile` → 共享 `probeFinish` | `onboard_l1_ack.go` + 从 `ansibleProbeJob` 抽出收口段 |
| 5 | `preflight_remote` 落点（见 D2） | 待拍板 |
| 6 | 探路 SSH 目标主机口径（见 D1） | 待拍板 |

### 2.2 不动（红线）

| # | 内容 | 理由 |
|---|---|---|
| 1 | **流水线状态机 / 步骤事件 / `advanceFlow` / 超时重试清扫器** | 状态统一落 L0 DB；L1 无状态（父 SPEC §2.2） |
| 2 | **`blockerFail` 门禁裁定** | 判据只能有一套；执行端只回原始事实，成败由 L0 裁定 |
| 3 | **`UpdateResourceProbe` 台账回写** | 台账是 L0 账本职责（"谁持账本谁落笔"） |
| 4 | **就近取包红线**（缓存命中→回源→仍不一致才拒绝） | 与探路无关，原样继承 |
| 5 | **语法门禁**（碰目标机前 `--syntax-check`） | 原样继承 |
| 6 | 平台「执行包 + 人工回报」回落路径 | 凭据缺失 / 无 ansible 时的兜底，本次不动 |
| 7 | `register_platform_device` / `sync_config` 等 `platform` scope 步骤 | 不 SSH 目标机，不搬 |

### 2.3 边界口径（一句话）

> **L1 Runner = 纯执行端**：拿自足载荷 → 渲染/组命令 → 在采集机侧跑 → 回结构化事实（facts/tasks/recap/evidence）。
> **L0 = 状态机 + 账本 + 唯一判据**：投递任务、消费回执、跑 `blockerFail` 门禁、回写台账、决定重试与判死。

---

## 三、关键设计决策（D1 已裁定；D2/D3 待拍板）

### D1（**已由 §零.一 原则裁定，无需拍板**）探路的执行端与探测对象

**原则裁定**：
- **执行端 = 采集机（L1 Runner）**：探路在采集机侧跑（Runner 与采集机同域），L0 不直接 SSH target（P1/P3）。
- **探测对象 = target（被采对象）**：从采集机侧发起，核对「采集机 → target」的可达性/账号/端口（P2，下行安装/上行采集的双向直通前置校验）。
- **采集机自身事实 = 就地获取**：OS/架构/磁盘/端口/是否已有 SAgent，在采集机本地 `gather_facts`，不 SSH 到第三台（P4）。

**事实校正（现状代码为什么错）**：
- 模板语义：`remote.yaml:42` 标题「**采集机探路**」，`:46` hint「① **采集机**事实…② 能力可行性：**从采集机侧**核对被采对象可达性」——**模板本来就是对的**。
- 代码实现：`onboard_ansible.go:232` `cred := sshCredOf(r.resource)` 取的是 `pick_object` 选的**被采对象**；且 `flowRun.resource` 全程不重绑 → 探路/安装 SSH 的是**被采对象**，与模板语义相反。
- 演示数据佐证：flow 3-7 均 `resource_id=ip-10-20-0-15`（被采对象）、`agent_id=order.prod.mysql.proxy-02`（采集机 @ 10.20.0.3），两者**不同机**。

**结论（唯一口径）**：探路/安装的 SSH 目标主机 = **`flow.AgentID` 对应的采集机**；target 仅作为**被探测对象**出现在能力可行性核对里（`execAbilityChecks`），不作为 SSH 落点。

> 需要补一小项：**采集机凭据获取口径**——在 `pick_proxy` 环节登记/继承采集机 SSH 凭据，落到采集机对应的资源行（当前 `resources` 表无采集机行/无凭据）。此项并入 B4b-1 的载荷构造（`buildProbePayload` 用采集机凭据，而非 `r.resource` 凭据）。

### D2 `preflight_remote`（远程可达性探路）落点

**事实**：不在 `ansibleKindOfAtom`（`onboard_ansible.go:203-226`）→ 无代执行通道 → 走人工回报，傻等 3600s。
父 SPEC §2.2 把它列为「走 Agent 侧，不经 ansible，不搬」；但模板要求它**在装机之前**完成（`remote.yaml:52-54`）。

| 选项 | 说明 | 代价 |
|---|---|---|
| **A（按 §零.一 P2/P5 + §零.二 裁定）纳入 L1 执行端**（kind=`probe_remote`） | 在 **L1 Runner（=采集机侧）本机** 直连被采对象，做**协议级 + 认证级**核验（Redis：`AUTH`+`PING` 期待 `+PONG`；其余类型 TCP 兜底）——与「采集」同源（**不是** L0 直连、也**不是**裸 `nc`/`wait_for` 只探端口） | 需新增一个轻量 kind；语义与 §零.二 remote 口径完全一致 |
| B 维持 Agent 侧 | 装完 SAgent 后由 Agent 上报可达性 | 与"装机前拦住"矛盾；装完才发现采不到 = 多付一次装+卸；且违反 P2「前置校验」 |
| C 并入 `probe` 阶段 | 不新增 kind，作为 `probe` 的一个阶段（target 由 `collect_params` 声明） | 最省事；但 `preflight_remote` 步骤会变成空步骤，需删步骤或改指向 |

> **裁定**：**A**（与 P2 一致，独立 kind 语义最干净）。C 仅作为"若求最小切口"的备选——但会牺牲步骤与原则的一一对应，不建议。

### D3 `env_check` 里的 `autoResolveAccessPoint` 怎么办

**事实**：`onboard_ansible.go:944` `autoResolveAccessPoint` 会读资源台账、再跑 ansible 试候选接入点、必要时建反向隧道（L0 策略逻辑 + 再跑 SSH）。

| 选项 | 说明 |
|---|---|
| **A（建议）作为 `probe` 任务内的一个阶段搬走** | Runner 回"候选实测结果"，**自动选定与隧道决策仍回 L0**（第二跳任务或 L0 侧判定后追加指令） |
| B 本次不搬 | 保留在 L0 → 但它也 SSH 目标机，**与红线冲突**（主机搬 `l1-net` 后同样断链），只能算临时 |

> **建议 A**，但**可后置**：先让 `probe` 主体跑通（B4b-1），`autoResolveAccessPoint` 单独一个子项（B4b-3）。

### D4 结构化回执

沿用 `result JSON`（`store/l1task.go:40`）：Runner 回 `facts / ability.checks / evidence / phases / diagnosis`；
L0 侧 `l1ProbeReconcile` 重建后走**共享 `probeFinish`**，保证「切了开关结论与证据形状不变」。

### D5 超时预算

沿用 D5 口径：L0 用 `timeoutSpecForStep(onboardCfg, step.ID, step.Atom)` + `execBudget(...)` 算 `budget_sec` 随载荷下发；
Runner 按 `budget_sec` 设 ctx，**不自设时限**（探路现有 `budgetPing/budgetDisk` 等分阶段预算需一并随载荷或在 Runner 侧按阶段比例切）。

### D6 支持集与开关

沿用总开关 `L1_EXEC_EXECUTOR` + 支持集 `l1ExecSupportedKind`：**新增 atom 只动支持集，不动开关语义**（父 SPEC §实施进展）。

### D7 阶段顺序（红线）

**B4b 必须先于 B5**：`sagent-host` 一旦搬 `l1-net`，L0 侧探路当场断链（父 SPEC §四 B4b 注）。
验证期沿用现有「Runner 临时双挂 `l0-net`」偏离（父 SPEC「验证期网络边界修正」），B5 收口时删除。

---

## 四、分阶段计划（各自可独立交付）

| 阶段 | 内容 | 验证门禁 |
|---|---|---|
| **B4b-1** | **`preflight_host` 走 Runner**：支持集加 `probe`；`buildProbePayload`；`runnerExecuteProbe`（ping/setup/df/env/ability 五阶段回结构化事实）；L0 侧 `l1ProbeReconcile` + 抽出的共享 `probeFinish`（门禁裁定 + 台账回写 + 回报） | 端到端：remote 流水线在 `runner` 模式下探路 **ok**，`facts` 正确回写台账、`blockerFail` 仍能拦住；切回 `inprocess` 结论与证据**逐字一致**；单测覆盖 done/failed/timeout/重投/旧快照；`go vet` + 全量回归绿 |
| **B4b-2** | **`preflight_remote` 落点**（按 D2 拍板：A 新增 `probe_remote` / C 并入 `probe`） | 远程可达性在**装机前**给出 blocker 结论；不再傻等 3600s |
| **B4b-3** | **`autoResolveAccessPoint` 下沉**（按 D3） | 接入点自动选定/隧道行为与进程内一致 |
| **（收口）B5** | 主机搬 `l1-net` + 删灰度开关与 L0 进程内 ansible 分支 + 删 Runner 临时双挂 | 全链路回归 + 容器实证 L0 侧无目标机可达 |

> B4b-1 是**最小可交付切口**：它单独就能把 flow 7 的 `blocked` 解开（探路能跑通 → `pick_version` 拿到 OS/架构）。
> B4b-2 / B4b-3 可独立排期，不阻塞 B4b-1 的交付与验证。

---

## 五、每阶段共同门禁

- `go build` + `go vet` 零告警；全量单测绿
- `docker compose config --quiet` 通过
- 改 Go 代码：**交叉编译二进制** → `docker compose build`（`--no-cache`）→ `up -d --force-recreate`
- 运行态验证：`exec_channel=l1` 出现在步骤事件；`l1_tasks` 有 `probe` 任务且 `reconciled_at` 置位
- 交付清单登记子项 + 运行态证据段

---

## 六、风险与红线

| 风险 | 后果 | 对策 |
|---|---|---|
| 探路目标主机选错（D1） | 选版依据是错机器的 OS/架构 | **D1 先拍板**；A 方案需补采集机凭据口径 |
| `preflight_remote` 继续无通道 | 傻等 3600s，用户以为卡死 | B4b-2 收口（A/C） |
| `autoResolveAccessPoint` 留 L0 | 主机搬 `l1-net` 后断链 | D3 选 A；B5 前必须清 |
| facts 无结构化回传 | 探路结论丢失，选版/可行性无据 | D4：走 `result` JSON，非 `note` 字符串 |
| 双超时口径 | 执行端自设时限与 L0 预算打架 | D5：预算单点由 L0 下发 |
| 灰度开关长期留存 | 两套执行路径并存，回归面翻倍 | B5 收口即删（不留半成品） |

**红线（继承父 SPEC）**：**L0 不持有任何被管主机可达**——所有面向主机的执行一律经 L1 Runner。

---

## 七、本次不做

- 不动 `register_platform_device` / `sync_config` / `self_metrics` / `verify_probe`（`platform`/`agent` scope）
- 不动就近取包、语法门禁、人工回报回落路径
- 不改 `l1_tasks` 表结构（列已齐）
- 不引入新的执行端进程（复用 `l1-ansible-runner`）

---

## 八、待你确认的点

> **状态（2026-10-11）：已全部确认并实施完成**，D1/D2/G1/G2 按本 SPEC 裁定落地，切口=B4b-1/2/3 一体交付；实施进展与运行态证据见「九」。

---

## 九、实施进展与运行态证据（2026-10-11 收口）

### 9.1 交付内容

| 子项 | 内容 | 状态 |
|---|---|---|
| **B4b-1** | `preflight_host` 走 Runner：支持集加 `probe`；`buildProbePayload`；`runnerExecuteProbe`；L0 侧 `l1ProbeReconcile`（共享 `probeFinish`：门禁裁定+台账回写+回报） | ✅ 落地 |
| **B4b-2** | `preflight_remote` 落点=`probe_remote` kind：`buildProbeRemotePayload`；`runnerExecuteProbeRemote`（从采集机侧对被采对象 AUTH+PING 协议级核验）；`l1ProbeRemoteReconcile`（四档事实 `probed_by=remote_probe/probe_channel=l1_runner/remote_target/tcp_ok/protocol_ok/auth_ok/reply` 与主机档案 merge 共存回写台账） | ✅ 落地 |
| **B4b-3** | 探路执行端含接入点自动择优（`auto_resolve`），随 `runnerExecuteProbe` 下沉 | ✅ 落地 |
| **G1/G2** | `flowSSHCred`：SSH 落点一律按 `flow.AgentID`（采集机）取 `agents.resource_id → resources.ssh_*`；无资源行/凭据显式 fail + 诊断 | ✅ 落地 |
| **demo 前置** | sagent 镜像加 `openssh-server+python3+su-exec`（口令 deploy/deploy123 与台账对齐）；`run.sh` root 起容器先拉 sshd 再 `su-exec deploy` 降权跑 SAgent；compose 三 proxy `user: root` + `SSHD_ENABLED=1` | ✅ 落地 |

### 9.2 observe_collect 收口修复（端到端最后断点，本阶段）

flow 23 探路双 ok 后卡 `observe_collect`，根因五点、修复五处：

| # | 根因 | 修复 |
|---|---|---|
| 1 | Agent `countOwnSeries` 拉 `/metrics` 无 Bearer → D4 鉴权 401 → 恒 0 → `metrics_confirmed` 永不上报 | `client.go` 增 `SetMetricsAuthKey`（main 装配 `cfg.Server.MetricsAuthKey`），清点序列携带 Bearer（与 vmagent 同凭据） |
| 2 | Agent 继续上报 B4b 前的 `preflight_ability`（步骤已改名不存在）→ "step not found" 无限重试噪音 | `client.go` 删该上报块（死代码）；L0 `stepByKind` 删死键并注明 B4b 起不接受 |
| 3 | L0 `vmSeriesCount` 只查 `{resource_id="X"}`；remote 序列经采集机 pool relabel 后原值落在 `remote_resource_id` → 即使上报也会被拒 | 改双口径 `{resource_id="X"} or {remote_resource_id="X"}` |
| 4 | L0 `metrics_confirmed` 分支无条件 `registerScrapeTarget(被采对象IP)` → remote 模式制造死抓取目标（P3：L0 侧不通被采对象） | `f.Mode=="remote"` 跳过 file_sd 登记（其指标本就经采集机 pool 目标入链） |
| 5 | redis 持续采集 `redis_up=0`（NOAUTH）：①`sameRedisTargets` 判等不含 Password——last-good（无口令）→ 新 cfg（有口令）被 diff 判「无变化」跳过重启；②redis_exporter **多目标模式不解析 target URL 内嵌凭据**（实测 NOAUTH，官方 README：`/scrape` 全实例共用 `--redis.password`） | 判等 key 加 Password；exporter 口令改经 `--redis.password` 启动参数传入（`redisTargetURL` 去口令）；`buildExporterArgs` 与运行进程 args 比对、变更连进程重建；目标集口令不统一 **fail-closed 报错**（绝不用 A 口令静默连 B 实例）；补 3 个单测锁行为 |

### 9.3 运行态证据（flow 24，runner 模式端到端全绿）

- **流程全绿**：`POST /api/onboard/flow`（resource=ip-10-20-0-16，mode=remote，agent_id=order.prod.mysql.proxy-01，params 含口令）→ `preflight_host ok`（`exec_channel=l1`，ansible 实测 Alpine 3.19.9/arm64/Python 3.11.14/mem/disk，端口实测 [22,19090,19104,44405]，接入点 auto_resolve 自动择优）→ `pick_version ok`（linux-arm64-0.4.2-dev，musl）→ `install_agent skipped`（复用现有，预期）→ `register_platform_device ok` → `preflight_remote ok`（AUTH+PING +PONG）→ `sync_config ok`（cfg-14）→ `verify_probe ok`（applied_version=14）→ `self_metrics ok` → **`observe_collect ok`：`{series_count:502, vm_series_count:328, vm_verified:true}`** → **flow status=done**。
- **数据面实证**：VM 直查 `redis_up{remote_resource_id="ip-10-20-0-16", resource_id="sagent-proxy"} = 1`（remote 序列经采集机 pool 目标入链的 relabel 口径）；redis 业务面 300+ 序列入链。
- **无死目标实证**：`vmagent_targets` 无 `10.20.0.16` 行、scrape-sd 目录无 `ip-10-20-0-16.json`——remote 跳过 file_sd 生效，P3 红线未破坏。
- **台账实证**：被采对象行 probe_json 为远程四档 + 主机档案 merge 共存（probed_by=remote_probe / probe_channel=l1_runner / auth_ok=true / reply=+PONG）；承载机行（ip-10-20-0-3）仅凭据载体无探路档案（符合 G1 设计）。
- **l1_tasks**：`flow-24-preflight_host-a1|probe|done|ansible-runner|reconciled=t`、`flow-24-preflight_remote-a1|probe_remote|done|ansible-runner|reconciled=t`。
- **质量门禁**：`go vet` 两模块零告警；`go test ./...` 全量绿（含新增 `TestSameRedisTargetsPasswordChange`/`TestRedisUniformPassword`/`TestRedisTargetURLNoPassword`）；交叉编译 linux/arm64 → `docker compose build` → `up -d --force-recreate` 生效。

### 9.4 过程事故与对照（留档）

- **flow 22 作废**：l0-console 容器为对照实验期以 `inprocess` 环境所建，`.env` 恢复 `runner` 后未重建容器（容器 env 与 .env 可漂移）→ 探路 fail「当前执行器为平台进程内」。教训：**改 .env 后必须 `up -d --force-recreate` 使容器 env 同步**。
- **flow 23 判死**：observe_collect 900s 超时（本阶段修复前），根因即 9.2 五点；修复后重发 flow 24 全绿。
- **IP 重排**：force-recreate 导致采集机容器 IP 重排（10.20.0.7→10.20.0.5→10.20.0.3），资源台账与 `agents.resource_id` 绑定同步改绑。教训：**演示环境 force-recreate 后必查 l1-net IP 与台账一致性**。

## 十、B4b-4 升级/启停四步搬 L1 + B5 收口（2026-10-11）

### 10.1 交付内容

| 子项 | 内容 | 状态 |
|---|---|---|
| **B4b-4 投递** | `l1ExecSupportedKind` 支持集加 `preflight_upgrade/upgrade_agent/preflight_service/service_execute`；新增 `buildSvcPayload`——kind/playbook/title 取自 `svcSpecForKind`，升级分支把人工选定版本/清单 sha/备份名（预检反查 `svcLatestPreflightUpgrade`，查不到时间戳兜底）算好随载荷，启停分支 `svcParams.valid()` 白名单后下发动作/目标 | ✅ 落地 |
| **B4b-4 执行** | `runnerExecuteSvc`（`parseRunnerSvcReq` 门禁 → 载荷自足校验 → 就近取包 `resolveInstallBinaryAny` + sha 断言不降级 → `loadSvcPlaybook` 渲染 → 语法门禁 → 流式执行 → 结构化回执）；`upgrade_agent` 回执带 tag/sha256/sha_verified/bak_name | ✅ 落地 |
| **B4b-4 对账** | `driveStepFromL1Task` 在卸载侧分支**之前**插入 svc 分支（承载机口径 `flowSSHCred` + `l1SvcReconcile`）；`l1SvcReconcile` 走共享裁定 `sp.Verdict`（UPGRADE_OK/SVC_* 自证行 + `svcCheckLine` 兜底）+ `PostOK` 维护态登记 + `RecordPackageDownload`（runner-resolve 留痕） | ✅ 落地 |
| **B5 收口** | 删灰度开关 `L1_EXEC_EXECUTOR`（代码 `l1ExecExecutor()` + .env/.env.example/compose 注入）与全部进程内 ansible 作业（`ansibleProbeJob/ansibleInstallJob/ansibleUninstallJob/ansibleScanJob/ansibleUninstallPluginsJob/ansibleCleanupAutostartJob/ansibleSvcJob` 及独用辅助 `ansibleJobs/ansibleStaleLimit/runAnsible/autoResolveAccessPoint`）；`tryAnsibleExecutor` 恒走 `deliverStepToRunner`；sagent-1/2 已只挂 l1-net | ✅ 落地 |
| **共享收口保留** | `probeFinish/uninstallFinish/offboardFinish`、`svcSpecForKind` 全套、`loadSvcPlaybook`（磁盘优先 embed 兜底）、流式基座 `runPlaybookStream/runSyntaxCheck/writeInventory` 均保留（L1 执行端与 L0 对账桥共用，判据只有一份） | ✅ 落地 |
| **投递继承修复** | `deliverStepToRunner` 重写 running 事件时继承既有 running 事件的 `diagnosis`（`step_timeout_retry` warn）与 `attempts` 历史——B5 前 inprocess 靠 `ansibleStaleLimit` 不重写事件，恒 runner 后若丢弃会抹掉「平台自己救过一次」与超时留痕 | ✅ 落地 |

### 10.2 运行态证据（B5 收口版本，恒 L1 Runner）

- **升级 flow-31 全绿**：`POST /api/onboard/flow/upgrade`（resource=ip-10-20-0-3）→ `preflight_upgrade ok`（`exec_channel=l1`，UPGRADE_PRE 自证 sha=bd79824b…guard=yes）→ `pick_version ok`（linux-arm64-0.4.2-dev）→ `confirm_upgrade` 人工放行 → `upgrade_agent ok`（`UPGRADE_OK pid=9741 ver_sha=a33644be…（=0.4.2-dev 清单 sha）bak=SAgent.bak-linux-arm64-0.4.2-dev`，**备份落目标机 `~/SAgent/bin/SAgent.bak-linux-arm64-0.4.2-dev`**，进程存活 pid=9741）。`l1_tasks`：`flow-31-preflight_upgrade-a1`/`flow-31-upgrade_agent-a1` 均 `done|reconciled=t`；`package_downloads` 记 `runner:flow-31-upgrade_agent-a1|runner-resolve|verified=t`。
- **启停 flow-29/30 全绿**：stop → `service_execute ok`（`SVC_STOPPED`）+ 目标机进程归零 + `resources.svc_state=stopped`；start → `SVC_STARTED` + 进程恢复 + `svc_state=running`。两轮 `l1_tasks` 均 `done|reconciled=t`。
- **探路 flow-28 全绿**：`preflight_host ok`（`exec_channel=l1`，L1 Runner 实测 OS/arch/mem/disk），`flow-28-preflight_host-a1|probe|done|reconciled=t`；install_agent skipped（复用现有 SAgent），后续 Agent 回报步骤正常下发。
- **质量门禁**：`go vet` 两模块零告警；`go test ./...` 全量绿（PG 载体真跑，无 SKIP 假绿）——含新增 `TestSvcReconcile*`×6、`TestBuildSvcPayload*`×4、`TestParseRunnerSvcReq/TestRunnerSvcGateRejectsIncompletePayload/TestRunnerExecuteTaskSvcDispatch`，及 B5 语义适配（fixture 补 `seedSSHCred/seedHumanPick` 让 external 步骤正常投递到 running；既有 `TestSyncAgentConfigPreservesTargetIdentityAndParameters` 补能力位声明修复 R0-3 fail-closed 存量债）。交叉编译 linux/arm64 → `docker compose build` → `up -d --force-recreate l0-console l1-hub` 生效（l1-hub 保持 l1-net 10.20.0.14 与台账一致）。

### 10.3 过程要点与留档

- **恒 runner 的测试语义变化**：B5 前单测默认 `inprocess`，external 步骤凭据缺失→回落人工（保持 running）；B5 后恒 runner，凭据缺失→当场 `failStep(no_ssh_cred)`。受影响用例 fixture 统一补凭据（`seedSSHCred`）+ install/upgrade 补人工选版事件（`seedHumanPick`），测试意图（步骤到 running 等回报/回执对账）不变。
- **升级同版本提示**：目标机已是 0.4.2-dev 时重升同版本，`confirm_upgrade` 落 blocked 文案「目标版本与当前版本相同，重装同版本，待人工确认」——升级不可逆，人工闸门照常生效，放行后幂等重装成功。
- **交付物目录**：`SPEC-D5-IN5`（父计划）与「交付清单-采集平台代码对齐-阶段」的 IN5 行证据段同步登记（见交付清单 §D5-IN5）。