#!/bin/sh
# l1-collector 四进程守护入口（容器整合，SPEC-L1HA / L1HA-1）
# 语义：真实部署形态是「一台采集机一个 tar.gz 包、四个 systemd 单元」——四个控制面进程
# （controller / gateway / package-cache / ansible-runner）同机同网络、同版本轨道、
# 同一部署单元。本脚本就是演示容器里 systemd 的替身：拉起角色矩阵声明的全部进程、
# 崩溃自动重启（≤5s 检测周期）、SIGTERM/SIGINT 时全部优雅退出。
# 设计边界（与真实 systemd 对齐的部分，刻意不做的部分）：
# - 子进程直接继承容器 stdout/stderr（docker logs 可见，无中间管道，避免信号丢失/僵尸）
# - 不做依赖编排（各进程自身重试上游，controller 断 L0 会按 LOOP_MS 循环重拉）
# - 进程级重启即全部语义；不做单进程 CPU/内存限制（编排层职责）
# 角色矩阵：L1_COLLECTOR_ROLES=controller,gateway,package-cache,ansible-runner（缺省全装）。
# L1HA 弹性扩缩容载体——每台采集机用 env 声明自己跑哪些角色。
set -u

ROLE_DEFAULT="controller,gateway,package-cache,ansible-runner"
ROLES="${L1_COLLECTOR_ROLES:-$ROLE_DEFAULT}"

P_CTL=""; P_GW=""; P_PC=""; P_AR=""

flag_for_role() {
    case "$1" in
        controller)     printf -- "-controller" ;;
        gateway)        printf -- "-gateway" ;;
        package-cache)  printf -- "-package-cache" ;;
        ansible-runner) printf -- "-ansible-runner" ;;
        *)              printf "" ;;
    esac
}

pid_of_role() {
    case "$1" in
        controller)     printf %s "$P_CTL" ;;
        gateway)        printf %s "$P_GW" ;;
        package-cache)  printf %s "$P_PC" ;;
        ansible-runner) printf %s "$P_AR" ;;
    esac
}

start_role() {
    role="$1"
    flag=$(flag_for_role "$role")
    if [ -z "$flag" ]; then
        echo "[l1-collector] 未知角色: ${role}，跳过"
        return
    fi
    case "$role" in
        controller)     ./l0-console "$flag" & P_CTL=$! ;;
        gateway)        ./l0-console "$flag" & P_GW=$! ;;
        package-cache)  ./l0-console "$flag" & P_PC=$! ;;
        ansible-runner) ./l0-console "$flag" & P_AR=$! ;;
    esac
    echo "[l1-collector] 角色 ${role} 已启动 (pid=$!, flag=${flag})"
}

stop_all() {
    echo "[l1-collector] 收到退出信号，停止全部角色进程"
    for p in $P_CTL $P_GW $P_PC $P_AR; do
        [ -n "$p" ] && kill "$p" 2>/dev/null
    done
    for p in $P_CTL $P_GW $P_PC $P_AR; do
        [ -n "$p" ] && wait "$p" 2>/dev/null
    done
    echo "[l1-collector] 全部角色进程已退出"
    exit 0
}
trap stop_all TERM INT

echo "[l1-collector] 角色矩阵: ${ROLES}"
ROLES_LIST=$(printf %s "$ROLES" | tr ',' ' ')
for r in $ROLES_LIST; do
    start_role "$r"
done

# 看护循环：每 5s 检查一轮，死掉的进程原样重启（systemd Restart=on-failure 的替身）
while :; do
    sleep 5
    for r in $ROLES_LIST; do
        p=$(pid_of_role "$r")
        if [ -z "$p" ] || ! kill -0 "$p" 2>/dev/null; then
            echo "[l1-collector] 角色 ${r} 未在运行 (pid=${p:-无})，重启"
            start_role "$r"
        fi
    done
done
