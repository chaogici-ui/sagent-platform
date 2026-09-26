#!/usr/bin/env bash
# 跨网段接入点隧道（Agent 回连通道）
#
# 场景：目标机与平台跨网段（例：目标机在公司内网 10.x，平台跑在家庭网 192.168.x），
# 目标机无法直连平台 → Agent 装好后注册不回来，接入流程会卡在「主机自监控生效」等回报。
# 本脚本在 l0-console 容器内建立 SSH 反向隧道，把目标机本机端口映射回平台：
#
#   目标机 127.0.0.1:<REMOTE_PORT>  --(ssh -R)-->  平台容器 localhost:8080
#
# 配套：把该资源的 Agent 接入点（资源台账 agent_console_url）设为 http://127.0.0.1:<REMOTE_PORT>
#
# 用法：
#   ./start-crossnet-tunnel.sh <target_ip> <ssh_user> <ssh_password> [ssh_port] [remote_port]
# 示例：
#   ./start-crossnet-tunnel.sh 10.1.207.156 ibomc '密码' 22022 18080
#
# 注意：隧道进程随 l0-console 容器生命周期，容器重建后需要重跑本脚本。
set -euo pipefail

TARGET_IP="${1:?用法: $0 <target_ip> <ssh_user> <ssh_password> [ssh_port] [remote_port]}"
SSH_USER="${2:?缺少 ssh_user}"
SSH_PASS="${3:?缺少 ssh_password}"
SSH_PORT="${4:-22}"
REMOTE_PORT="${5:-18080}"

CONTAINER="${L0_CONTAINER:-l0-console}"
# docker 可执行文件：优先 PATH，其次常见安装位置（宿主机 GUI 启动的 shell 常没有 PATH）
DOCKER_BIN="${DOCKER_BIN:-$(command -v docker 2>/dev/null || true)}"
if [ -z "$DOCKER_BIN" ]; then
  for cand in /usr/local/bin/docker /opt/homebrew/bin/docker "$HOME/.docker/bin/docker"; do
    [ -x "$cand" ] && DOCKER_BIN="$cand" && break
  done
fi
if [ -z "$DOCKER_BIN" ]; then
  echo "找不到 docker 可执行文件（可用 DOCKER_BIN 指定）" >&2
  exit 1
fi

if ! "$DOCKER_BIN" inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "找不到容器 $CONTAINER（可用 L0_CONTAINER 覆盖）" >&2
  exit 1
fi

# 幂等：已有到同一目标机的隧道就先停掉，避免端口占用 Stacking
"$DOCKER_BIN" exec -u 0 "$CONTAINER" sh -c \
  "pkill -f 'ssh -N -R ${REMOTE_PORT}:localhost:8080' 2>/dev/null || true" || true
sleep 1

"$DOCKER_BIN" exec -d "$CONTAINER" sh -c \
  "sshpass -p '$SSH_PASS' ssh -N -R ${REMOTE_PORT}:localhost:8080 \
     -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
     -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
     -o ExitOnForwardFailure=yes -p ${SSH_PORT} ${SSH_USER}@${TARGET_IP}"

sleep 3

# 验证：从目标机侧 curl 平台 API（经隧道），200 即通
CODE=$("$DOCKER_BIN" exec -w /tmp "$CONTAINER" sh -c \
  "printf '[t]\nt ansible_host=${TARGET_IP} ansible_port=${SSH_PORT} ansible_user=${SSH_USER} ansible_ssh_pass=${SSH_PASS} ansible_ssh_common_args=\"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8\"\n' > /tmp/_tuninv && \
   ansible t -i /tmp/_tuninv -m shell -a 'curl -s -o /dev/null -w %{http_code} --connect-timeout 5 http://127.0.0.1:${REMOTE_PORT}/api/onboard/templates' 2>/dev/null | tail -1; rm -f /tmp/_tuninv")

if [ "$CODE" = "200" ]; then
  echo "✅ 隧道已建立：${TARGET_IP} 127.0.0.1:${REMOTE_PORT} → 平台:8080（目标机侧验证 HTTP 200）"
  echo "   请把资源 ${TARGET_IP} 的 Agent 接入点设为 http://127.0.0.1:${REMOTE_PORT}"
else
  echo "⚠️ 隧道已尝试建立，但目标机侧回连验证失败（HTTP ${CODE:-无响应}）" >&2
  echo "   排查：目标机到平台方向的网络策略 / sshd 是否允许远程转发（AllowTcpForwarding）" >&2
  exit 2
fi
