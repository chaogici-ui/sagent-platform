# SPEC-D5：L1 四进程拆分 边界定义与实施计划

> 依据：《PLAN-架构设计与高可用运维方案.md》决策记录 D5「4 进程独立 + 1 tar.gz 打包；Gateway 剥离包缓存给独立 PackageCache」。
> 采纳的既定前提：组件**全部无状态**，状态统一落在 L0 数据库；Package Cache 只能承载 L0 校验过的只读镜像。

## 一、四进程职责与接口边界

| 进程 | 职责 | 监听/入口 | L0 交互 | 状态归属 | 现有关联代码 |
|------|------|-----------|---------|---------|-------------|
| **L1 Controller** | 收 L0 下行指令并分流到 Ansible/Gateway；**控制面唯一出口**（承接 Gateway 汇聚的 SAgent 通道与 L1 内部组件的任务拉取） | 反连 L0 拉取任务 + 中继入口 `:8461`（`controllerRelayHandler`） | 拉任务 + 回执 + 中继转发 | L0 DB（任务表） | `sagent/internal/control/client.go`（心跳拉任务）；`l0-console/l1_controller.go` + `gateway.go` 白名单 |
| **L1 Gateway** | 控制面双向上行中继（心跳汇聚 + 指令路由）＋运行时插件/脚本安装（Agent 通道） | HTTP 中继端口 `:8460` | **不直连 L0**：汇聚心跳转发上游 L1 Controller（`L1_CONTROLLER_URL`） | 仅瞬态 buffer 不落库 | `sagent/internal/control/client.go` 插件安装任务通道 |
| **L1 Package Cache** | 版本包只读镜像 + 签名校验 + 就近分发 | HTTP `:8450`（`PACKAGE_CACHE_LISTEN`） | 回源 L0 同步包 | 本地缓存文件（sha 校验），非决策源 | `l0-console/package_cache.go`（D5-L1PC 已落地） |
| **L1 Ansible Runner** | 首装 SAgent（playbook 执行端） | 反连 L1 Controller 接安装任务 `:8462` | **不直连 L0**：任务拉取/回执经上游 L1 Controller | L0 DB（流水线） | `l0-console/l1_ansible_runner.go` + playbooks |

**红线（继承 D5-L1PC）**：安装就近取包 → 缓存失效回源 L0 → 仍不一致拒绝安装，不降级用旧包。

**红线（网络域）**：L0↔L1 互通的进程只有两个角色——控制面出口 `L1 Controller`（唯一双挂 l0-net + l1-net）、数据面出口 `VMAgent`（l1-net 抓 vmauth、l0-net 写 VMStorage）。Gateway / PackageCache / AnsibleRunner / SAgent / vmauth 一律只挂 l1-net。Gateway 白名单只收 SAgent 通道（`/api/agent/*` + 证据回报 + 任务回执），`/api/l1/task/pull` 属 L1 内部通道，只经 Controller。

## 二、增量拆分与验证门禁（各自独立交付）

| 子项 | 内容 | 验证门禁 |
|------|------|----------|
| **IN1 Package Cache 独立进程**（✅ 已交付） | `l0-console -package-cache` daemon 模式：仅起 package-cache API（status/sync/resolve），不加 DB/console；l0-console 安装路径可用 `L1_PACKAGE_CACHE_URL` 走 HTTP resolve（缺省仍进程内 `resolveInstallBinary`，红线语义一致） | 单测 + 容器运行态：daemon status/sync/resolve + 篡改自愈 + 就近取包通路（均已验：见交付清单 D5-IN1 段） |
| IN2 Gateway 汇聚中继（✅ 已交付） | 心跳/指令汇聚通道进程；SAgent 控制通道改指 L1 Gateway（网络域收敛末环，随 G2 退役收口）。**上游已收敛为 L1 Controller**（`L1_CONTROLLER_URL`），Gateway 只挂 l1-net、不持有 L0 可达 | 单测 `TestGatewayRejectsL1InternalTaskPull` + 容器运行态：`l1-gateway` 内解析不到 `l0-console`、可解析 `l1-controller` |
| IN3 Controller 分流中枢（✅ 已交付） | 任务拉取 + 分流到 Ansible/Gateway（`-controller` 独立进程 + L0 侧 L1 任务源三接口）。**控制面唯一出口**：双挂 l0-net + l1-net，挂中继入口承接 Gateway 汇聚的 SAgent 通道与 Runner 的任务拉取 | 单测 `TestControllerRelayIsControlPlaneEgress` + 容器运行态：SAgent 心跳经 gateway→controller→L0 落库、Runner 拉任务经 controller 得 200 |
| IN4 Ansible Runner 独立 + tar.gz 打包（✅ 已交付） | playbook 执行端（`-ansible-runner` 独立进程）+ 四进程 tar.gz 部署脚本（`scripts/build-l1-bundle.sh`） | 依赖 IN1-3 + 采集机部署目标 |
| **IN5 安装执行端搬迁**（📋 已立项，见 `SPEC-D5-IN5-安装执行端搬迁边界与分阶段计划.md`） | 把 `tryAnsibleExecutor` 接管的 **11 个 ansible 步骤原子**从「L0 进程内执行」搬到 L1 Runner：L0 投递任务 + 回执对账桥驱动步骤终态；Runner 退化为纯执行端（不碰 L0 DB/状态机）。**最后**再把目标机 `sagent-host` 搬 l1-net | 分 4 阶段（B2 绑定与回执桥 → B3 install → B4 其余 9 atom → B5 主机搬迁与收口），每阶段独立门禁 |

## 三、本阶段落地项（IN1）设计

- **daemon 模式**：`l0-console` 增加 `-package-cache` 标志（或 `PACKAGE_CACHE_DAEMON=1`）+ `PACKAGE_CACHE_LISTEN`（默认 `:8450`）。该模式只 `loadVersionCatalog(..., nil)`（文件口径，L1 不裁决版本）+ 注册 package-cache HTTP 路由，`http.ListenAndServe` 阻塞。缓存根沿用 `L1_PACKAGE_CACHE`（默认 `data/packages`）。
- **新增 resolve 接口**：`GET /v1/package-cache/resolve?tag=<tag>` → `{ok, bin, error}`，复用 `resolveInstallBinary`。
- **l0-console 可选用远程 resolve**：`L1_PACKAGE_CACHE_URL` 非空时，安装侧走 `remoteResolveInstallBinary`（HTTP resolve）；为空走进程内 `resolveInstallBinary`（基线语义不变）。
- **compose**：新增 `l1-package-cache` 服务（复用 `docker-l0-console` 镜像，`-package-cache`），暴露 8450。