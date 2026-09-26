#!/bin/sh
# l0-console 启动引导：种子同步 → 业务进程
# 语义：数据源文件（integrations/metrics.json/onboard_config.json）随镜像版本走（镜像赢），
#       运行态产物（plugins 上传目录 / sync_report）留在卷里（卷赢）；目录库在 pg-data 卷（PostgreSQL）。
# 根治「data 卷遮蔽镜像内新文件」问题（已三现）：新数据文件随镜像升级自动落卷，
# 不再需要 docker cp+chown 或重建卷。见 CONSTRAINTS.md 红线 1。
set -e
SEED=/opt/l0-seed
WORK=/home/deploy/l0-console/data
mkdir -p "$WORK"
# 种子文件：镜像覆盖（json 种子 + integrations 插件包，均随镜像版本演进）
cp -f "$SEED"/*.json "$WORK"/ 2>/dev/null || true
if [ -d "$SEED/integrations" ]; then
    rm -rf "$WORK/integrations"
    cp -r "$SEED/integrations" "$WORK/integrations"
fi
# 流程模板同理：随镜像版本演进（改流程 = 改模板文件，不是改代码）
if [ -d "$SEED/flow_templates" ]; then
    rm -rf "$WORK/flow_templates"
    cp -r "$SEED/flow_templates" "$WORK/flow_templates"
fi
# SAgent 版本清单同理：随镜像版本演进（登记新版本 = 改 versions.yaml，不是改代码）
if [ -f "$SEED/versions.yaml" ]; then
    cp -f "$SEED/versions.yaml" "$WORK/versions.yaml"
fi
# 平台本地版本库（安装作业的分发源）同理：二进制随镜像版本走（镜像赢）
if [ -d "$SEED/binaries" ]; then
    rm -rf "$WORK/binaries"
    cp -r "$SEED/binaries" "$WORK/binaries"
fi
# 安装 playbook 同理：随镜像版本演进（改安装动作 = 改 data/playbooks/*.yml，不是改代码）
if [ -d "$SEED/playbooks" ]; then
    rm -rf "$WORK/playbooks"
    cp -r "$SEED/playbooks" "$WORK/playbooks"
fi
# 采集目标登记目录（vmagent file_sd 共享目录，安装作业写入）：compose bind mount
# 挂到这里（l0-console 与 vmagent 双挂），宿主机目录属主天然可写，无需 chown
# 运行态产物不在 seed 清单内，卷中已有的一律不动
# forward 额外参数：`docker ... entrypoint -- -package-cache` 可跑独立 L1 Package Cache 进程（架构 D5 IN1）
exec ./l0-console "$@"
