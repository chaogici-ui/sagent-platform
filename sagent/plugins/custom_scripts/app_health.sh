#!/bin/sh
# 示例：应用健康检查脚本
echo "# HELP sagent_app_health 应用健康状态 (1=正常 0=异常)"
echo "# TYPE sagent_app_health gauge"
echo "sagent_app_health 1"

echo "# HELP sagent_app_uptime_seconds 应用运行时长(秒)"
echo "# TYPE sagent_app_uptime_seconds counter"
echo "sagent_app_uptime_seconds 3600"