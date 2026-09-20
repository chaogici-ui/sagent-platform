#!/bin/sh
# l0-console 启动引导：种子同步 → 业务进程
# 语义：数据源文件（integrations/metrics.json/onboard_config.json）随镜像版本走（镜像赢），
#       运行态产物（catalog.db 三件套 / plugins 上传目录 / sync_report）留在卷里（卷赢）。
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
# 运行态产物不在 seed 清单内，卷中已有的一律不动
exec ./l0-console
