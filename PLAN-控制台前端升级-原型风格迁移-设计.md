# SAgent 控制台前端升级方案 —— 以「平台化设计原型」为风格与逻辑蓝本

> 目标：把 `http://localhost:8080`（SAgent MVP 控制台）的视觉、信息架构与交互逻辑，对齐 `http://127.0.0.1:59365`（SAgent 平台化设计原型，纯静态演示稿）的品质，同时保留 8080 已有的真实后端能力（67 个 API、真实采集链路）。
>
> 一句话定位：**原型提供"壳与思想"，8080 提供"肉与数据"，以原型为唯一视觉与逻辑目标，把真实数据灌进新壳。**

---

## 一、现状对比与设计原则

### 1.1 两套的客观差异（已核实）

| 维度 | 59365 原型 | 8080 MVP | 结论 |
|------|-----------|----------|------|
| 形态 | 纯静态（零网络请求，数据硬编码 fixtures） | Go 后端 67 API + 真实采集 | 8080 有真实能力，缺精致表达 |
| 导航 | 6 单级模块（工作台/资源接入/采集策略/采集健康/变更中心/平台设置） | 5 分组 9 项（总览/接入/定义/观测/系统） | 原型的**业务叙事**更完整 |
| 视觉 | 深青侧栏 + teal 主色，统一令牌体系 | 蓝色 #2563eb，扁平朴素 | 原型明显更精致专业 |
| 交互 | 场景切换/证据链/上下文助手/变更生命周期 | 表格/抽屉/modal/toast | 原型交互叙事性强 |
| 数据 | 演示 fixtures | 真实目录库（PostgreSQL）+ VM 实况 | **改造必须以 8080 真实数据为准** |

### 1.2 设计原则（红线）

1. **壳换思想，肉换数据**：视觉与交互逻辑以原型为蓝本，但所有数据来自 8080 真实 API，**绝不引入 fixtures 假数据**。
2. **保持轻量**：维持现有"纯静态 + fetch"架构，不引入 React/Vite 等重依赖（项目定位内部工具，符合既有技术选型）。
3. **后端能力优先、不重造轮子**：尽量复用现有 67 个 API；个别差距（如"变更中心"治理闭环）先以现有 `onboard/audit/tasks` 数据聚合，新增后端仅在必要处最小化。
4. **1:1 继承原型视觉令牌**：深青侧栏、teal 主色、圆角、状态色语义（healthy/unknown/maintenance 等）全部迁移。
5. **可回滚、可增量**：因为是纯前端改造，保留原 HTML 文件中 `?v=` 缓存版本机制，升级可随时回退版本号。
6. **符合既有工程红线**：遵守 `CONSTRAINTS.md`，前端不写死地址、外部地址走 `/api/site-config`。

---

## 二、视觉设计体系迁移（1:1 继承原型）

### 2.1 设计令牌（Design Tokens）

将原型 `styles.css` 的令牌迁移进 `style.css` 的 `:root`，统一全局：

| 令牌 | 原型值 | 用途替换 |
|------|--------|---------|
| `--canvas` | `#f4f6f8` | 原 `--bg`(#f5f6fa) |
| `--surface` | `#fff` | 原 `--card` |
| `--ink` | `#172c36` | 原 `--text`(#1a1a2e) |
| `--muted` | `#5c6b76` | 原 `--muted`(#8e8e9a) |
| `--line` | `#e1e7eb` | 原 `--border`(#e8e8f0) |
| `--teal` / `--teal-soft` | `#087d75` / `#e9f5f2` | **主色**，替换原 `--primary`(#2563eb) |
| `--amber` / `--amber-soft` | `#946011` / `#fff5df` | 警示/未知态 |
| `--red` / `--red-soft` | `#ae3c38` / `#fff0ee` | 失败/错误态 |
| `--blue` | `#345e95` | 辅助信息 |
| `--sidebar` | `#142a34` | 深青侧栏底色（新增） |
| `--radius` | `12px` | 卡片圆角（原 10px） |

**关键升级点**：当前蓝色扁平配色 → 墨青 + teal 的"观测/健康"专业氛围。这是最主要的视觉差异。

### 2.2 状态色语义（迁移原型统一状态语言）

原型有一套完整的状态词表（`app.js` 的 `names`），迁移到前端作为统一 badge/pill 语义：

- 健康：`healthy`(teal) / 降级：`degraded`(amber) / 失败：`failed`(red) / 未知：`unknown`(灰色楔形) / 维护：`maintenance` / 禁用：`disabled`
- 配置管线：`pending → received → validated → applied → verified`（applied≠verified，验收必须中央独立判定）
- 变更：`draft/previewed/approved/running/verifying/succeeded/partial/canceled`

> 这些与现有后端 `agent.status`（healthy/running/stopped/offline）、`onboard.flow.status`、`recon` 的语义一一映射，不冲突。

### 2.3 导航升级（信息架构重组）

把 8080 现有 5 组 9 项导航，重组为原型的业务叙事，同时保留深层页面可访问（走 `goPage` 深链兜底，历史链接不失效）：

| 原型导航 | 对应 8080 页面（复用现有 render + API） | 备注 |
|---------|--------------------------------------|------|
| 工作台 | `overview` | 升级为原型"工作台"总览（健康覆盖/待处理问题/区域链路/最近变更） |
| 资源与接入 | `resources` + `onboard-center` | 资源表格升级 + 接入向导走原生三步（选择对象→能力→预演） |
| 采集策略 | `capabilities`（能力目录）→ 新增"策略"视图 | 用插件 interval/capability 数据呈现策略卡片（后端无独立策略实体，先聚合） |
| 采集健康 | `slo` | 升级为原型"待处理发现 + 证据链"视图（用 `recon` + `collect-stats` 数据） |
| 变更中心 | `audit` + `tasks` + `onboard 流水线` | 聚合三类真实记录为"变更"时间线；后端无"变更"实体先用聚合视图 |
| 平台设置 | `settings` | 升级为基线规格展示 |

**仍可从原深链访问、不进主菜单**（沿用现做法）：`agent-list`、`metrics-browse`/`plugins-mart`/`metrics-catalog`（分析大屏与能力目录入口保留在"采集策略"详情内）、`targets`、`tenants`、`versions`。

---

## 三、交互逻辑升级（核心：从"表格工具"到"治理叙事"）

这是本次升级的**价值主体**，每一块都映射到真实 API。

### 3.1 工作台（Overview）——迁移原型"证据驱动"表达
- **改造 `renderHealthOverview()`**：
  - 顶部 4 张指标卡（健康检查覆盖/有效健康目标/待处理问题/待跟进变更）→ 数据源：`/api/resources` + `/api/onboard/flows` + `/api/recon`（`summary.checked/targets/stale/wild`）+ `/api/collect-stats`（`reporting/age_sec`）。
  - 增加"上下文助手"卡片（事实/推断/缺证）→ 用 `recon.summary` + `collect-stats.vm_reachable` 真实计算三段文案（如 `vm_reachable=false` 时推断"中央查询不可达"）。**不调用模型**（与原型一致），纯规则模板。
  - 区域链路 → 用 `/api/vm/label-values`(region) + `collect-stats` 聚合，而非 fixtures。

### 3.2 采集健康（SLO）——迁移"证据链"模型
- **改造 `renderSLO()`**：在现有断采/野指标/异常 Agent 基础上，新增**逐目标证据链**：
  - 一条证据链 = `对象连接(agent heartbeat)` → `配置版本(applied)` → `源采集(collect-stats.reporting)` → `传输(recon)` → `中央验收(recon.summary.vm_reachable + checked)`。
  - 数据源全部来自 `recon` + `collect-stats` + `agents/{id}/plugin-status`，真实生成，非演示。
  - 沿用原型语义："unknown 不是失败也不是健康"→ 对 `reporting=false` 且无堆栈证据的目标标记 `unknown` 而非直接 red。

### 3.3 资源与接入（Resources）——接入向导三步化 + 能力声明
- **改造 `renderResources()`**：
  - 资源行状态用原型 `pill`（healthy/unknown/disabled/维护）。
  - "接入采集能力"按钮 → 复用现有 `openAddWizard`，但按原型**三步向导**（选择对象→选择能力 host_metrics/log_to_metrics→预演影响与 diff）。能力清单来自 `/api/onboard/abilities`。
  - 预演展示"影响目标/请求频次估计/diff"，数据来自真实资源与能力参数，不写死。
- **改动最小**：资源表格结构（资源/地址/区域/能力/健康证据/最近回报）基本对齐现有字段。

### 3.4 采集策略（新增视图，聚合实现）
- **新增页面 `policy` 视图**：展示能力/策略清单 + 影响目标 + 采集间隔。
- 数据源：`/api/onboard/templates`、`/api/onboard/abilities`、`/api/catalog/plugins`（能力目录）+ `/api/resources`（影响目标）。
- 后端**不新增策略实体**：策略 = 能力 + 模板的 `interval/params` 聚合。变更预演记录的落库走现有 `/api/onboard/flow`（复用接入/升级流水线）。

### 3.5 变更中心（新增聚合视图）
- **新增页面 `changes` 视图**：聚合三类真实记录为统一"变更"时间线：
  1. 接入/升级/卸载流水线（`/api/onboard/flows`，取其 status/step/时间/目标）
  2. 审计操作（`/api/audit`，写入即变更）
  3. 任务历史（`/api/tasks`）
- 每单展示：编码、类型（接入/策略/修复）、目标数、区域、状态（复用原型状态词）、步骤证据（真实 step 记录）。
- **边界**：原生型"人工批准"在 MVP 中由流水线人工步骤承担，不做伪审批入口（符合原型"不伪造"精神）。

### 3.6 平台设置（Settings）——基线规格展示
- **改造 `renderSettings()`**：按原型四组规格（平台与存储/能力与执行边界/健康与配置语义/智能与工作空间）展示**真实**配置，数据来自 `/api/site-config` + 环境变量登记表 + 现有 `/api/integrations`。只读，保留原型"无伪保存按钮"。

---

## 四、实施计划（分阶段，每阶段可独立验收）

> 纯前端改造，后端 API 全部现有可用（除个别人工确认点）。每个阶段结束保持可运行、可回滚。

### 阶段 0：落地视觉令牌与导航骨架（打底）
- 替换 `style.css` 令牌为原型体系；重构 `index.html` 导航为 6 模块叙事。
- 新增统一 `pill/badge`、空状态、加载骨架组件。
- **验收**：整体观感达到原型风格，6 个导航能进入现有页面，无数据回归。

### 阶段 1：工作台 + 采集健康（价值最高，优先）
- 重写 `overview.js`、`panel.js#renderSLO()`，按原型证据驱动叙事 + 上下文助手。
- **验收**：真实采集数据在原型风格下呈现，助手三段文案与 `recon/collect-stats` 实况一致。

### 阶段 2：资源与接入 + 采集策略
- 重写 `resources.js` 状态语义与三步接入向导；新增策略视图。
- **验收**：接入手动流程三步化跑通真实 ansible 流水线。

### 阶段 3：变更中心 + 平台设置
- 新增变更聚合视图（audit/tasks/flows）；升级设置页。
- **验收**：三类真实记录聚合为时间线，深链历史不失效。

### 阶段 4：收尾与回归
- 全页面走查、内联样式收敛、toast/空态/加载态统一、缓存版本号 bump。
- **验收**：对照原型逐屏 1:1 视觉对齐，数据全部真实；`go test` 通过、`?v=` 升级生效。

---

## 五、风险与待确认项

| 项 | 风险/决策 | 建议 |
|----|----------|------|
| 「变更中心」无独立后端实体 | 聚合视图在数据量大时性能 | 先聚合，后端不动；若审计/任务量大再议索引 |
| 「采集策略」无策略实体 | 策略=能力+模板的聚合，非真实独立策略 | 符合 MVP 现状，避免过度设计 |
| 侧栏深青 vs 现有蓝色系 | 视觉差异大，需用户确认调性 | 按原型 1:1，用户可微调 teal 深浅 |
| `overview` 15s 全局刷新 | 原型无自动刷新；auto 刷新会打断详情 | 保留 15s，但 resources 页仍不自动重绘（现状） |
| 上下文助手文案 | 规则模板，非模型 | 与原型一致，仅当 `vm_reachable=false` 等触发推断 |

---

## 六、交付物

- 改造后的 `l0-console/static/`（index.html / style.css / 各 js），沿用 `?v=` 版本机制。
- 统一设计令牌与组件说明（作为 style.css 内注释维护，不另开文档）。
- 每个阶段的验收记录。

> 最终只保留本一份设计文档；阶段完成后的中间过程不另产生设计文件。