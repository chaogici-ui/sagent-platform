# SPEC-D5-IN5：安装执行端搬迁（L0 进程内 ansible → L1 Runner）边界与分阶段计划

> 依据：`SPEC-D5-L1四进程拆分边界与实施计划.md`（IN1–IN4 已交付）；用户决策「sagent-host 搬进 l1-net 后，SAgent 的探路/安装切 l1-ansible-runner」。
> 前置：A 段（控制面出口收敛到 L1 Controller）已落地并运行态验证通过。
> 本文只做**边界定义与分阶段计划**（立项），不含实现。

## 零、立项结论（先看这一句）

搬迁的对象**不是 install 一个作业，而是整个 ansible 执行面**——共 **11 个步骤原子**共用同一个执行入口，全部要 SSH 到被管主机：

```
onboard_atoms.go:222  →  tryAnsibleExecutor(...)   （scope != "platform" 时唯一接管入口）
onboard_ansible.go:202 →  tryAnsibleExecutor 定义
onboard_ansible.go:207 →  atom → kind 映射（11 个）
```

| atom | kind | 执行函数 | 是否 SSH 目标机 |
|------|------|----------|-----------------|
| `preflight_host` | probe | `ansibleProbeJob` | 是（探路 + facts） |
| `install_agent` | install | `ansibleInstallJob` | 是 |
| `uninstall_agent` | uninstall | `ansibleUninstallJob` | 是 |
| `scan_collectors` | scan_collectors | `ansibleScanJob` | 是 |
| `uninstall_plugins` | uninstall_plugins | `ansibleUninstallPluginsJob` | 是 |
| `cleanup_autostart` | cleanup_autostart | `ansibleCleanupAutostartJob` | 是 |
| `preflight_upgrade` | preflight_upgrade | `ansibleSvcJob` | 是 |
| `upgrade_agent` | upgrade_agent | `ansibleSvcJob` | 是 |
| `preflight_service` | preflight_service | `ansibleSvcJob` | 是 |
| `service_execute` | service_execute | `ansibleSvcJob` | 是 |
| （`preflight_ability`） | — | 走 Agent 侧 | 否（不经 ansible） |

**因此**：只要 `sagent-host` 离开 `l0-net`，**这 10 个 SSH 作业同时失去可达性**——不存在"只搬 install 就行"的切口。分阶段计划必须**先搬执行面、最后搬主机**（见 §四）。

## 一、现状事实（带证据）

### 1.1 L0 进程内执行链路（现状）

```
flow 引擎（onboard_atoms.go:222）
  └─ tryAnsibleExecutor（onboard_ansible.go:202）
       ├─ 前置：cred.valid() && ansibleAvailable()   ← 否则回落「执行包 + 人工回报」
       ├─ 防并发/孤儿兜底：ansibleJobs map + takeForcedRestart + ansibleStaleLimit
       ├─ 落 running 事件（appendStepEvent）
       └─ go { 按 kind 分发到 11 个 job 函数 }（onboard_ansible.go:264-288）
```

`ansibleInstallJob`（`onboard_ansible.go:1639`）对 **L0 进程的依赖面**：

| 依赖 | 位置 | 说明 |
|------|------|------|
| 读流水线/资源 | `catDB.GetFlow` / `catDB.GetResource`（1679/1684） | 取 agentID、`AgentConsoleURL`、采集机池标签 |
| 读人工选版 | `pickedVersionTag(catDB, flowID)`（1663） | 版本只装人选的那一版 |
| 就近取包 | `resolveInstallBinaryAny(tag)`（1692） | 缓存命中 → 回源 L0 → 仍不一致拒绝 |
| 写步骤进度 | `newStepRunState`/`taskHook`/`flush`（1641-1644） | 逐 TASK 实时回传 |
| 写步骤终态 | `autoReportStep`（1876）→ `advanceFlow` | 步骤 ok + 推进流水线 |
| 写资源台账 | `catDB.SetResourceInstalledTag`（1883） | 记录装的是哪个 tag |
| 写 file_sd | `registerScrapeTarget`（1846） | 共享卷，Runner 侧也可写 |
| 写审计 | `addAudit`（1886） | |
| 失败收口 | `failStep`（494）+ `jobStillCurrent`（711） | 旧尝试不覆盖新尝试 |
| 超时/重试 | `handleStepTimeout`（onboard_timeout.go:574） | 额度内自动重试，用尽判死 |

### 1.2 L1 Runner 现状（IN4 已交付，但未接入流水线）

- `runnerExecuteInstall`（`l1_ansible_runner.go:109`）**只认 install / ansible-\***，非安装 kind 一律回执 failed。
- 载荷 `runnerInstallReq`（`:93`）**已自足**：tag / agent_id / console_url / ssh 凭据 / dest_home / collector / pool。
- 上游 = L1 Controller（`:87`）；拉取/回执走 `/api/l1/task/{pull,ack}`。
- 超时**硬编码 10 分钟**（`:182`），与 L0 的 `execBudget/budgetPlaybook` 两套口径。

### 1.3 传输层与回执（关键缺口）

- `l1_tasks` 表（`store/l1task.go:16`）**只有 id / controller_id / kind / payload / status / executor / note / created_at / acked_at**：
  - **没有 flow_id / step_id** → 回执无法定位到步骤；
  - **没有结构化 result** → facts / tasks / recap / phases 只能塞进 `note` 字符串。
- ack 端点（`l1_task.go:115`）**只落库终态**，**没有任何代码回驱步骤状态**（全仓 grep 无消费者）。
- `controllerDispatch`（`l1_controller.go:120`）对 install/ansible-\* 返回 `executor_pending`，**并未真正转发给 Runner**；实际路由靠"投递时就把 controller_id 写成 Runner 身份"（IN4 端到端验证即如此）。

## 二、搬迁边界

### 2.1 动

| # | 内容 |
|---|------|
| 1 | `tryAnsibleExecutor` 的**执行动作**：由"进程内起 goroutine"改为"投递 L1 任务 + 等回执" |
| 2 | 11 个 job 函数的**执行核心**（渲染 playbook/config → syntax-check → 流式执行 → 结构化结果）搬到 Runner |
| 3 | `l1_tasks` 表新增 `flow_id` / `step_id` / `attempt` / `result` 列 |
| 4 | L0 侧新增**回执对账桥**：把任务终态翻译成步骤终态 + L0 侧 DB 写 |
| 5 | `sagent-host` 服务：改名（已完成目录改名）+ 搬 `l1-net`（`10.20.0.20`） |

### 2.2 不动（红线）

| # | 内容 | 理由 |
|---|------|------|
| 1 | **流水线状态机、步骤事件、`advanceFlow`、重试/超时清扫器** 全部留在 L0 | 状态统一落 L0 DB（D5 既定前提）；L1 无状态 |
| 2 | **就近取包红线**（缓存命中 → 回源 → 不一致拒绝，不降级） | 原样继承，Runner 侧已是同一 `resolveInstallBinaryAny` |
| 3 | **语法门禁**（碰目标机前 `--syntax-check`） | 原样继承 |
| 4 | 平台"执行包 + 人工回报"回落路径 | 凭据缺失 / 无 ansible 时的兜底，本次不动 |
| 5 | L0↔L1 网络域红线（只有 Controller + VMAgent 双挂） | A 段已收敛，本次不改 |
| 6 | `preflight_ability`（Agent 侧远程可达性） | 不经 ansible，不搬 |

### 2.3 边界口径（一句话）

> **L1 Runner = 纯执行端**：只做「拿自足载荷 → 渲染 → 跑 → 回结构化结果」，**不碰 L0 DB、不判状态机、不决定重试**。
> **L0 = 状态机 + 账本**：投递任务、消费回执、写步骤终态与资源台账、决定重试与判死。

## 三、关键设计决策（待拍板）

| # | 决策点 | 建议 | 备选 | 影响 |
|---|--------|------|------|------|
| D1 | 任务↔步骤绑定 | `l1_tasks` 加 `flow_id`/`step_id`/`attempt` 三列（可查询、可对账、重启不丢） | 把绑定编码进任务 ID | 决定回执桥能否可靠对账 |
| D2 | 结构化结果通道 | `l1_tasks` 加 `result TEXT`（JSON：facts/tasks/recap/phases/sha/evidence） | 全塞进 `note` 字符串 | 决定探路 facts 回写与界面逐任务回传是否断链 |
| D3 | 执行语义粒度 | **薄执行端**：Runner 只做渲染+执行+回结构化结果；渲染所需 tag/sha 随载荷，Runner 侧 resolve 出本地路径后再渲染 | 厚执行端：Runner 内嵌各 job 全逻辑（需大改 2000+ 行） | 决定改造量与耦合度 |
| D4 | 凭据处置 | 载荷携带明文 SSH 口令（与 `resources` 表同密级，`store/onboard.go:28-30`）；**回执后置空 payload**，仅留 `result`/`note` | 引入独立密管 / 只传句柄 | 安全边界与审计留痕 |
| D5 | 超时口径 | 统一由 L0 下发 `budget_sec`（源自 `execBudget`），Runner 按此设 ctx；Runner 不再硬编码 10min | 保留双口径 | 决定"超时判死"是否一致 |
| D6 | 幂等与重试 | 任务 ID = `flow-<id>-<stepID>-a<attempt>`；重试换 attempt → 新 ID；同 ID 重复投递 SKIP（`QueueL1Task` 已幂等） | 复用同 ID | 决定重试是否会重复执行 |
| D7 | Runner 并发 | 当前 ticker 串行执行（`l1_ansible_runner.go:76-80`）；多流水线会排队 | 小工作池（如 4 并发） | 影响接入吞吐 |
| D8 | 灰度开关 | `L1_INSTALL_EXECUTOR=inprocess|runner`，默认 `inprocess` 保回滚；B5 收口时**删除开关与 L0 进程内分支** | 无开关硬切 | 决定回滚能力与"不留半成品" |

## 四、分阶段计划（各自可独立交付）

> 顺序不可颠倒：**先搬执行面，最后搬主机**。主机搬走后 L0 即失去目标机可达，任何未搬迁的作业都会当场断链。

| 阶段 | 内容 | 验证门禁 |
|------|------|----------|
| **B2** | L0 侧任务绑定 + 回执对账桥（**不动执行端**）：`l1_tasks` 加 4 列；回执桥接清扫器 tick，`done`→`autoReportStep`、`failed`→`failStep`、无回执超预算→`handleStepTimeout` 同一套口径 | 单测：done/failed/timeout/幂等重投/旧快照无绑定列不炸；`go vet` + 全量回归绿 |
| **B3** | **install 走 Runner**：载荷构造（D3/D4/D5/D6）+ 灰度开关（D8）；主机**留 l0-net**，Runner **验证期临时双挂 l0-net** 验证（见下方「验证期网络边界修正」），确保可回滚 | 端到端：一条完整接入流水线（探路→选版→安装→观测）在 `runner` 模式下全绿；切回 `inprocess` 仍全绿 |
| **B4** | **其余 9 个 atom 迁移**（probe/uninstall/scan_collectors/uninstall_plugins/cleanup_autostart/preflight_upgrade/upgrade_agent/preflight_service/service_execute）：按 job 抽"渲染+执行"核心为可复用执行单元，Runner 按 kind 分派 | 逐 atom 端到端：探路 facts 回写台账、卸载三口径自证、升级回滚、启停核对均与进程内语义一致 |
| **B5** | **`sagent-host` 搬 `l1-net`（`10.20.0.20`）** + 收口：删灰度开关与 L0 进程内 ansible 分支；compose 修 `build.context: ./vm-host` → `./sagent-host`（目录已改名，当前 compose 指向不存在的路径）；**删 Runner 验证期临时双挂 `l0-net`** | 全链路回归（探路/安装/卸载/升级/启停/漂移）+ 容器实证 L0 侧已无目标机可达路径；17 服务无回归 |

### 实施进展（2026-09-25）

- **B2 / B3 已落地**，运行态证据见交付清单 D5-IN5 B2 / B3 段。
- **B4 拆为 B4a / B4b 两批**（9 个 atom 的回执语义差异较大——卸载侧是「三口径自证」、探路是「facts 回写台账」、升级是「回滚」、启停是「核对」——一次搬完无法逐批对照等价性）：
  - **B4a（已落地）**：卸载侧四步 `uninstall` / `scan_collectors` / `uninstall_plugins` / `cleanup_autostart`。Runner 侧新增 `runnerExecuteOffboard`（渲染 → syntax 门禁 → 流式执行 → DTO 回执），L0 侧新增 `onboard_l1_result.go`（`ansibleRunResult` 可序列化 DTO 双向转换，`l1VerdictTasks` 保留逐任务 `Lines`/`Stdout`/`Msg` 使自证标记不丢）与 `l1OffboardReconcile`（按 kind 分派到 `offboardFinish`/`uninstallFinish`，**与进程内共用同一收口函数**）。端到端 runner 全绿（flow-35：四 atom `exec_channel=l1`）+ inprocess 等价性对照全绿（flow-37：自证 note 与 flow-35 逐字一致、零 L1 任务），证据见交付清单 D5-IN5 B4a 段。
  - **B4b（待放行）**：余下 5 个 atom `preflight_host`（探路）/ `preflight_upgrade` / `upgrade_agent` / `preflight_service` / `service_execute`。其中 **`preflight_host` 必须先于 B5**——探路仍在 L0 进程内时，主机一旦搬 `l1-net`，L0 侧探路当场断链（同下方「验证期网络边界修正」的成因）。
- **D8 开关改名与语义拆分**：`L1_INSTALL_EXECUTOR` → **`L1_EXEC_EXECUTOR`**（总开关），并把「敢不敢走 Runner」与「哪些 atom 能走」拆成两件事——支持集由 `l1ExecSupportedKind()` 决定（当前含 `install` + 卸载侧四步）；新增 atom 只动支持集、不动开关语义。旧名降为**仅总开关未设时的兜底**（两处开关同时生效会互相打架）。B5 收口时仍按原计划删除开关与 L0 进程内分支。

### 验证期网络边界修正（B3 实测，2026-09-25）

原计划「主机暂留 l0-net 双挂」**不成立**，B3 实测后修正为「主机留 l0-net + Runner 临时双挂 l0-net」。原因：

- 本阶段 install 已搬 Runner（走 `l1-net`），而 `preflight_host` 探路仍在 L0 进程内（走 `l0-net`）——两者要够到的是**同一台主机**。
- 台账 `resources` 只有**单一 IP 字段**（`ssh_host` 随载荷下发，见 D3 载荷构造），无法表达「L0 走 10.10.0.20、Runner 走 10.20.0.20」。
- Docker 对两个 bridge 网络做隔离（实测 Runner→`10.10.0.20` 不可达）；主机若搬 `l1-net`，L0 进程内探路**当场断链**（探路属 B4 才搬，不能提前）。

故 B3 验证期：主机**留在 l0-net**（IP/台账零改动，L0 进程内探路不受影响），Runner **临时额外挂 l0-net**，共享 L0 对同一 IP 的可达性。这是最小可逆改动：

- compose `l1-ansible-runner` 加 `- l0-net`，已就地标注 `⚠ B3 验证期临时偏离（B5 收口即删）`；
- **对红线的临时偏离**：§五「L0↔L1 网络域红线（只有 Controller + VMAgent 双挂）」在验证期被 Runner 短暂打破——仅限演示 compose、仅限 B3/B4 验证，B5 主机搬 `l1-net` 后即删；
- 实测：Runner 双挂后 `nc -z 10.10.0.20 22` 通、`nc -z l1-controller 8461` 通；runner 模式端到端全绿、切回 inprocess 仍全绿。

### 每阶段共同门禁

- `go build` + `go vet` 零告警；全量单测绿
- `docker compose config --quiet` 通过
- 关键变更走 `build`（改 Go 代码必须 `--no-cache`）+ `up -d --force-recreate`
- 交付清单登记子项与运行态证据段

## 五、风险与红线

| 风险 | 后果 | 对策 |
|------|------|------|
| 主机先搬、执行面后搬 | 探路/安装/卸载/升级/启停**全线断链** | §四 顺序红线：主机最后搬 |
| 回执桥缺失 | 步骤永久卡 `running`，靠清扫器判死误报 | B2 先行，桥与清扫器同口径 |
| facts 无结构化回传 | 探路结论丢失，选版/可行性判定无据 | D2 加 `result` 列 |
| 凭据长期驻留 `l1_tasks` | 口令在库中无限期可读 | D4 回执后置空 payload |
| 双超时口径 | Runner 10min 先杀 vs L0 预算判死，结论互相矛盾 | D5 统一由 L0 下发 budget |
| Runner 串行执行 | 多流水线排队，接入变慢 | D7 工作池（B4 视实测决定） |
| 灰度开关长期留存 | 两套执行路径并存，回归面翻倍 | D8 B5 收口即删 |

**新增红线（本阶段确立）**：**L0 不持有任何被管主机可达**——所有面向主机的执行一律经 L1 Runner；L0 与主机之间不得存在容器直连路径。