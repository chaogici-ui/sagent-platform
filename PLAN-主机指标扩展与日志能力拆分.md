# 主机指标扩展与日志能力拆分 — 实施计划

> 状态：待确认（2026-09-20 讨论）
> 范围：SAgent host_metrics 扩展 / log_collector 改名 / 指标目录导入 / 平台界面四处改造

---

## 1. 背景与目标

- 当前 host_metrics 仅采 ~20 项自造名指标（host_cpu_percent 等），目标对齐企业 KPI 标准表（sagent-host-kpi.txt），主机段 ~120 项。
- log_collector 当前实际行为是「日志转指标」，与未来真正的「日志采集」撞名，现在拆分。
- 平台界面需要承载多插件、多资源层级的指标规模，并支持基础资源指标的界面化增删配置。

## 2. 已拍板决策

| # | 决策 | 内容 |
|---|------|------|
| D1 | 指标命名对齐 node_exporter | host_metrics 输出 node_* 命名，KPI 表计算公式直接可用；旧 host_* 自造名废弃（breaking change，仪表盘表达式同步换） |
| D2 | Docker 独立插件 | docker_metrics（37 项，cAdvisor 口径），二期，本期不做 |
| D3 | host_* 前缀收编 3 项 | host_fd_used / host_nfs_mount_status / host_chronyd_status 进 host_metrics；host_ha_switch_status 留 custom_scripts（无通用口径，待定） |
| D4 | 采集频率配置化 | 默认 30s，支持 15s/30s/60s/5m/自定义（下限 5s）；yaml < Agent 配置 < 平台下发 三层生效 |
| D5 | 日志能力拆分 | 按产出物分类：产指标 = log_metrics（当前实现，改名落地）；产日志 = log_collector（规划中）。同一 Vector 引擎，未来可同时跑两条 pipeline |
| D6 | 配置与版本分离 | 界面增删指标 = config_version+1（心跳下发热生效）；新增采集能力 = SAgent 版本升级。指标定义带 min_agent_version，下发前校验 |
| D7 | 配置粒度 | 分组开关（按资源分类）为主 + exclude_metrics 例外清单；逐指标勾选作为微调入口 |
| D8 | 对账基准细化 | 对账「应采」= Agent 生效配置声明的指标集（分组开关+例外展开后），不是目录全集，避免误报断采 |
| D9 | 指标目录双维度治理 | 按「来源渠道 × 归属」管理：来源渠道 = builtin 内置 / exporter 社区 / script 自定义脚本 / sql JDBC采集 / log 日志转指标；归属 = 预置（只读可停用）vs 自定义（可增删改）。每条指标带溯源（产出物：插件/脚本/SQL任务）。对账「野指标」检测覆盖全部渠道。sql 渠道本期只预留字段与目录展示位，采集实现放二期 |

## 3. 采集口径承诺与校验

**口径原则**：与 node_exporter 同源（/proc、/sys、statfs），命名/标签/单位对齐。

| 指标组 | 数据源 | 备注 |
|--------|--------|------|
| CPU 秒总量/频率 | /proc/stat, /sys cpufreq | jiffies÷100 转秒，按 mode/core 标签 |
| 内存/swap 全系 | /proc/meminfo | 直读原文，不用 gopsutil 二次命名 |
| 磁盘 IO | /proc/diskstats | per-device 标签 |
| 文件系统 | statfs + /proc/mounts | device/mountpoint/fstype 标签 |
| 网络设备/协议栈 | /proc/net/dev, netstat, snmp, sockstat | 设备过滤规则对齐 node_exporter 默认 |
| 负载/进程/fork/ctxt/intr/vmstat/pressure | /proc/loadavg, /proc/stat, /proc/vmstat, /proc/pressure/* | |
| filefd/entropy/conntrack | /proc/sys/fs/file-nr 等 | 文件不存在自动跳过 |
| uname/time/timex | uname、adjtimex syscall | |
| hwmon 温度 | /sys/class/hwmon | 有才采，虚拟机无则跳过 |

**不做**：node_systemd_unit_state / node_systemd_system_running（D-Bus，二期）；host_ha_switch_status（口径待定）。

**验收关卡**：同一台机器同时跑 node_exporter 与 SAgent，脚本逐指标 diff 值与标签，差异清单清零才算对齐。重点核对：netdev 设备过滤、filesystem 挂载过滤。

## 4. 工作分解

| # | 工作项 | 主要改动 | 验收标准 |
|---|--------|----------|----------|
| ① | host_metrics 扩展（~119 项） | sagent/internal/plugin/builtin/ 拆分重写：host_cpu.go / host_mem.go / host_disk.go / host_fs.go / host_net.go / host_kernel.go / host_custom.go；分组开关 + exclude_metrics + interval 配置 | /metrics 可见全部启用指标，含 3 项 host_* |
| ② | 口径校验 | 临时对比脚本（node_exporter vs SAgent /metrics 逐项 diff） | 差异清单清零 |
| ③ | log_metrics 改名 | sagent: config.go yaml key、constants、main.go、server.go；deploy 配置 yaml；l0-console 目录条目一拆二（log_metrics 已实现 / log_collector 规划中）；vector pipeline 不动 | 全链路 grep 无 log_collector 残留（规划条目除外） |
| ④ | KPI 表 → 目录导入 | 转换脚本：txt → metrics.json（中文名/代码/单位/说明/计算公式）；MetricDef 增加字段：层级/大类/分类 + min_agent_version + **来源渠道（builtin/exporter/script/sql/log）+ 归属（预置/自定义）+ 溯源（产出物标识）**；docker 37 条标注 docker_metrics 二期 | 指标中心可见层级树数据，主机 120 条入库，全量条目带渠道与溯源字段 |
| ⑤ | 界面四处 | panel.js + index.html + store/main.go（见 §5） | 见 §6 Checkpoints |
| ⑥ | 仪表盘表达式迁移 | 现有主机演示面板 host_* → node_* 表达式 | 面板数据真实 |

执行顺序：① → ② → ③ → ④ → ⑤ → ⑥（④的目录数据与①同步产出）。

## 5. 界面设计（指标中心 + 三处）

1. **指标中心 · 双维度治理**（核心升级，兼容多渠道指标）：
   - 左侧过滤器两段式：**资源层级树**（层级→大类→分类，来自 KPI 表，覆盖预置目录）+ **来源渠道**（内置采集 / 社区exporter / 自定义脚本 / JDBC SQL / 日志转指标，五通道可多选）；
   - **归属标记**：预置 📦（只读、可停用）vs 自定义 ✏️（可增删改）；自定义指标目录页直接新建/编辑（名称/中文名/单位/说明/公式/标签）；
   - **溯源标注**：每条指标显示产出物（哪个 exporter / 哪个脚本 / 哪个 SQL 任务 / 哪个内置插件），可跳转；
   - 存量 915 条 MySQL 社区指标自动归入「exporter 渠道 · 预置」；业务标签（business_system/env）支持按标签筛选自定义与业务指标；
   - 跨渠道重名检测复用 #6 机制；「野指标」检测覆盖全部渠道。
2. **插件详情 · 指标分组**：host_metrics 100+ 指标按资源分类折叠分组，组内展开 + 英文 Key 复制。
3. **接入向导 · 能力勾选**：步骤1 装 SAgent + host_metrics 默认勾选 + 频率下拉；步骤2 可选 log_metrics / custom_scripts；明示后续可随时增量开启。
4. **Agent 详情 · 生效配置 tab**：config_version、interval、分组开关状态、exclude 清单、待下发变更提示；排障 tab 增加每轮心跳 per-plugin 采集条数；对账新增规则「host_metrics 启用但持续 0 条 → 半死不活」。

基础指标配置（分组开关 + 例外 + 频率）在 Agent 详情内编辑，保存即 config_version+1，心跳带回热生效。

## 6. 验收 Checkpoints

- CP1：host_metrics 输出 ~119 项，命名全 node_*/host_*，无旧自造名。
- CP2：与 node_exporter 同机 diff 差异清零。
- CP3：界面改频率/关分组 → config_version+1 → Agent 下一轮心跳带回 → 热生效，无需重装。
- CP4：对账引擎不误报被界面关闭的指标。
- CP5：log_collector 改名后全链路无残留；目录页两条目文案正确。
- CP6：KPI 主机 120 条进指标中心，层级树可筛、可搜、公式可见。

## 7. 本期不做

- docker_metrics（37 项，二期）
- systemd 指标 2 项（D-Bus）
- host_ha_switch_status（口径待定，先走 custom_scripts）
- 真正的日志采集 log_collector（规划中，上 ES/Loki 时立项）
- sql 渠道（JDBC 采集）的采集实现（本期只预留渠道字段 + 目录展示位 + 导入定义管理，采集通道二期）
- 新增社区 exporter 预置包（redis/kafka 等，沿用 sagent-plugin-bundle 导入机制按需扩展）
- M2 任务系统 / 灰度升级通道（SAgent 版本升级落地依赖它，本期只做 min_agent_version 标注）
