# PLAN — SAgent 版本兼容矩阵与自动选版

> 状态：设计待评审（2026-09-22）
> 决策来源：用户拍板三条 —— ① 兼容性按「内核 + 架构 + 包内组件要求」建模；② 选版策略「最高 stable 自动选定 + 无匹配时阻塞 + 人工例外通道」；③ 版本库管理「矩阵界面可编辑 + 二进制仍走文件」
> 关联约束：CONSTRAINTS.md 红线 1（业务数据落目录库 PostgreSQL）、红线 2（零硬编码，新配置项登记 `cfg*`）、红线 4（版本轨/配置轨分离）

## 1. 背景与问题

现状（已核对代码）：

- 版本目录 = `data/versions.yaml`（tag/version/os/arch/sha256/status/released/notes），二进制在 `data/binaries/<tag>/SAgent`，安装时目标机 `sha256sum` 断言与清单一致；两者随镜像种子同步进卷（entrypoint，镜像赢）。
- `pick_version` 是**人工停等门禁**：平台按实测 `os/arch` 过滤出候选推给人工选；没有人工确认记录，安装作业直接拒绝执行（`pickedVersionTag` + 「平台不使用 latest 兜底」）。
- 界面上没有版本管理页：登记版本 = 改 yaml + 放二进制 + 重建镜像。
- 「哪个版本支持哪种环境」这条知识目前只隐含在 `os/arch` 两个字段里，无表达力。

因此三处缺口：**决策权在人**（每台机都要点一次）、**兼容性无模型**（无法表达"CentOS 7 上 mysqld_exporter 跑不起来"）、**版本库不可视**（运维不知道库里有什么、谁在用哪个）。

## 2. 关键事实（决定了判据该按什么建）

SAgent 本体是 `CGO_ENABLED=0` 的**静态 Go 二进制**：只受 **CPU 架构**与**内核能力**约束，**不挑发行版版本**。
真正挑环境的是**包内第三方组件**：Vector（musl 静态，无额外依赖）、mysqld_exporter（glibc 动态链接，最低 glibc 2.17；Alpine 无 glibc 跑不起来）。

结论：判据应为 `arch 相等 && kernel ≥ 下限 && 目标 glibc ≥ 组件要求（若含 glibc 依赖组件）`，而不是"发行版版本区间"。发行版信息只作展示与诊断文案。

## 3. 方案取舍（为什么这么切）

| 方案 | 说明 | 取舍 |
|---|---|---|
| A（选定）匹配逻辑做成平台原子 `pick_version`（原 id 复用，语义升级） | 决策产物落事件 detail，界面可见可审；升级流程直接复用同一原子；零流水线迁移（步骤 id 不变） | 需要在探路阶段补采 glibc 一项 |
| B 匹配塞进安装作业内部 | 少一个原子 | 决策不可见、升级流程要重写一遍；诊断难复用到界面 |
| C Agent 自举（Agent 上报环境、自己拉匹配版本） | 生产级终态（无需平台推送） | 需要 Agent 侧下载/校验通道，本期不做（记为 P2） |

## 4. 数据模型（目录库 PostgreSQL 权威，yaml 降为种子）

目录库迁移 V4，新增表 `sa_versions`：

| 列 | 说明 |
|---|---|
| tag (PK) | `linux-amd64-0.4.1` |
| version / arch / os | 版本号与平台 |
| sha256 | 与 `data/binaries/<tag>/SAgent` 一致，安装作业在目标机断言 |
| status | stable / deprecated / disabled（disabled 直接不参与匹配） |
| min_kernel | 如 `3.10`（两段式比较） |
| components_json | `[{"name":"mysqld_exporter","min_glibc":"2.17"},{"name":"vector","static":true}]` |
| released / notes | 展示用 |
| source | `seed`（来自 versions.yaml）/ `ui`（界面编辑过） |
| updated_at / updated_by | 审计 |

**种子合并语义**：启动时读 `data/versions.yaml`，按 tag 合并——不存在的插入；已存在且 `source=seed` 的按文件更新；`source=ui` 的**不被覆盖**（人工改动优先）。二进制仍只走文件/镜像（本设计不引入上传通道）。

## 5. 环境采集补充

探路阶段（`preflight_host`）facts 增加一项：

- `glibc`：`getconf GNU_LIBC_VERSION` → 回落 `ldd --version` → 无 glibc（Alpine/musl）记 `musl` → 全部取不到记空。
- 复用已有 `ansible_kernel`、`ansible_machine`（arch）。
- 落 `probe_json`（台账）+ 步骤证据，供匹配与诊断引用。

**缺数据口径**（与现有"缺字段不静默填 0"一致）：glibc 未采到时不阻断，但**该条判据降级为"未核实"并在事件 detail 显式标注**，不假装通过。

## 6. 匹配算法（纯函数，可单测）

```
selectVersion(cands []SAVersion, env TargetEnv) (chosen *SAVersion, reason string, diags []diag)
```

- 过滤：`status=disabled` 排除；`arch` 必须相等；`kernel ≥ min_kernel`；候选含 `min_glibc>0` 的组件时，目标须为 glibc 且 `glibc ≥ min_glibc`（目标为 musl → 直接不兼容）。
- 排序：命中者按语义版本降序（复用 `semverLess`）；stable 优先，仅在无 stable 命中时才考虑 deprecated（并在诊断里标注"仅剩 deprecated 可用"）。
- 全落空：返回**逐候选的落选原因聚合**，例：`3 个候选全部不兼容：2 个要求内核 ≥4.19（目标 3.10），1 个要求 glibc ≥2.28（目标 2.17）`。
- 产出的 `reason` 直接写进步骤事件，界面无需二次解释。

## 7. 流水线改造

- `pick_version` 步骤：**步骤 id 与模板都不改**——"停等人工"是原子内部写出来的（`onboard_atoms.go` 的 `atomPickVersion` 返回 `waiting_for=human_pick`），模板里该步本来就是 `scope: platform`。改造只发生在原子内部：由"停等人工"改为"自动匹配"（实现核实记录，2026-09-22）。
  - 成功事件 detail：`{tag, version, source: auto_match, version_source, env{arch,kernel,glibc}, reason, diagnosis}`，界面直显"已按实测环境自动选定 v0.4.1（arch=amd64，内核 5.4≥3.10，glibc 2.28≥2.17）"。
  - 失败：fail + fatal 诊断（逐候选落选原因聚合）；人工走已有通道——「↻ 重试该步」（补采/改矩阵后重跑）或「强制通过」并在同一端点带 `version_tag` 指定版本（复用 `POST /api/onboard/flow/step` 的 `force`，**不新增端点**）。
- `upgrade` 模板同款替换为同一原子。
- 安装作业：`pickedVersionTag()` 的取值来源从"人工事件"改为"自动选定事件，或人工强制指定事件"，仍拒绝 latest 兜底。

## 8. 界面（复用现有页面注册方式，前端零硬编码）

新增「版本与兼容性」页：

1. **列表**：tag / 版本 / arch / 状态 / 内核下限 / 组件要求 / sha256 前 12 位 / 来源（种子/界面）/ 更新时间；行内操作：编辑矩阵、弃用、禁用。
2. **编辑弹层**：min_kernel、组件表（名称 + min_glibc + 静态标记）、notes、status；保存写目录库（配置轨语义，不触发 Agent 动作）。
3. **环境匹配预览**（把黑盒决策变透明的关键）：选一台资源 → 用其实测 facts 跑一次匹配，显示"会选哪个、为什么、其它候选为何落选"。
4. **接入向导 / 资源详情**：原"选版本"决策卡位置改为展示自动结论 + 「指定版本（需理由）」例外入口。

字段与状态枚举全部由 `/api/versions` 下发，前端不写死。

## 9. 红线与规范

- 元数据落目录库（红线 1）；二进制仍走文件（不新增存储通道）。
- 新增配置项登记进 `main.go` cfg 块（红线 2），如 `VERSIONS_SEED_PATH`（默认 `data/versions.yaml`）。
- 版本轨（二进制版本）与配置轨（cfg-N）语义不变（红线 4）。
- 不涉及指标命名（红线 5）。

## 10. 迁移与兼容

- V4 迁移：建表 + 首次导入 versions.yaml；无破坏性变更。
- 步骤 id 不变 → 存量流水线不断裂；正在停等的实例在升级后由新的平台原子接管（首次推进即自动选定）。
- 旧的人工选版事件（`human_pick`）仍被 `pickedVersionTag()` 识别，历史流水线证据可读。

## 11. 验证计划

- **单测**：匹配算法（arch 不符 / 内核不足 / glibc 不足 / musl 目标 / glibc 未采到降级为未核实 / 多 stable 排序 / deprecated 兜底 / 全落空诊断文案）；种子合并（ui 来源不被覆盖）；`version_tag` 强制通道校验。
- **集成**：对 `vm-host-1`（facts 已实测）跑一条 edge 流水线，断言"自动选版事件 + 安装成功 + SD 登记"；再把某版本 `min_kernel` 人为抬到 `99` → 断言 fail 与"差哪一项"诊断。
- **门禁**：`go vet` / linux-amd64 与 darwin-arm64 交叉编译 / 两模块全量测试；部署后真机验收一次。

## 12. 分期

- **P1（本期）**：V4 表 + seeds 合并、glibc 采集、匹配算法 + 单测、`pick_version` 平台原子化（含 force 指定 tag）、界面列表与匹配预览、编辑矩阵。
- **P2（后续）**：环境级基线版本 pin、机群版本分布视图、二进制上传通道（如确需）、Agent 自举（方案 C）。
