#!/bin/sh
# SAgent 进程管理器 — 作为容器 PID 1，管理 SAgent 子进程
# 崩溃自动重启，支持外部启停控制（通过 run/stopped 文件）

# root 启动且启用 sshd（采集机承载机 demo）：先拉起 sshd（deploy 口令登录，与安装同
# 一条 SSH 路径，SPEC-B4b P5），再经 su-exec 降权回 deploy 重入本脚本跑主循环。
if [ "$(id -u)" = "0" ] && [ "${SSHD_ENABLED:-0}" = "1" ] && command -v sshd >/dev/null 2>&1; then
    mkdir -p /run/sshd
    /usr/sbin/sshd
    echo "[supervisor] sshd started on :22 (user deploy)"
    exec su-exec deploy "$0" "$@"
fi

SAAGENT_BIN="./bin/SAgent"
SAAGENT_CONF="conf/SAgent.yaml"
PID_FILE="run/SAgent.pid"
STOP_FILE="run/stopped"

cleanup() {
    if [ -f "$PID_FILE" ]; then
        kill -9 -$(cat "$PID_FILE") 2>/dev/null
    fi
    exit 0
}
trap cleanup TERM INT

while true; do
    rm -f "$STOP_FILE"
    echo "[supervisor] Starting SAgent..."
    setsid $SAAGENT_BIN --config "$SAAGENT_CONF" &
    SAGENT_PID=$!
    echo $SAGENT_PID >"$PID_FILE"
    echo "[supervisor] SAgent started, PID=$SAGENT_PID"

    wait $SAGENT_PID
    EXIT_CODE=$?

    if [ -f "$STOP_FILE" ]; then
        echo "[supervisor] SAgent stopped by request (exit=$EXIT_CODE), paused"
        while [ -f "$STOP_FILE" ]; do
            sleep 2
            # 检查 stopped 文件是否被删除（来自 L0 的启动命令）
        done
        echo "[supervisor] Restart signal received, resuming..."
    else
        echo "[supervisor] SAgent crashed (exit=$EXIT_CODE), restarting in 3s..."
        sleep 3
    fi
done
