#!/bin/sh
# 示例：订单队列监控脚本
# 输出 Prometheus 格式，Core 自动解析并注入标签

# 模拟队列检查（实际环境替换为真实 redis-cli 调用）
QUEUE_SIZE=42
PENDING_SIZE=7

echo "# HELP sagent_queue_size 当前队列长度"
echo "# TYPE sagent_queue_size gauge"
echo "sagent_queue_size ${QUEUE_SIZE}"

echo "# HELP sagent_pending_size 待处理队列长度"
echo "# TYPE sagent_pending_size gauge"
echo "sagent_pending_size ${PENDING_SIZE}"

echo "# HELP sagent_script_ok 脚本执行状态"
echo "# TYPE sagent_script_ok gauge"
echo "sagent_script_ok 1"