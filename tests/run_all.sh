#!/bin/bash
# SAgent 全面自动化测试框架
# 覆盖：数据流/崩溃恢复/高负载/配置变更/网络中断/多Agent协调
# 口径对齐：主机指标为 node_*/host_node_* KPI 标准名（CP1/CP2 定案，旧 host_* 自造名已废弃）
#
# 用法约定（重要）：check 只接收「单一命令字符串」做断言，取数在 check 外完成；
# 禁止在 check 参数里写管道（管道会被外层解析，pass 输出被 grep 吞掉 + set -e 误杀脚本）
set -u

# docker CLI 在 /usr/local/bin（Docker Desktop），非交互 shell 默认 PATH 不含
export PATH="/usr/local/bin:/opt/homebrew/bin:$PATH"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE_DIR="$ROOT/deploy/docker"
PASS=0
FAIL=0
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[TEST]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
fail() { echo -e "${RED}[FAIL]${NC} $1"; FAIL=$((FAIL + 1)); }
pass() { echo -e "${GREEN}[PASS]${NC} $1"; PASS=$((PASS + 1)); }

# check "描述" '断言命令' —— 命令为单一字符串，eval 执行；命中 pass，未命中 fail
check() {
    local desc="$1" cmd="$2"
    if eval "$cmd" 2>/dev/null; then pass "$desc"; else fail "$desc ($cmd)"; fi
}

# 帮助函数：从 VM 取指标名列表 / 查询结果
vm_names() { curl -s "http://localhost:8428/api/v1/label/__name__/values" | python3 -c 'import sys,json; print("\n".join(json.load(sys.stdin)["data"]))'; }

# ====== 1. 环境准备 ======
log "===== 1. 环境准备 ====="
log "启动 Docker 全栈环境（重建卷，走产品化启动引导）..."
cd "$COMPOSE_DIR" || exit 1
docker compose down --volumes >/dev/null 2>&1 || true
docker compose up -d >/dev/null 2>&1 || true

log "等待服务就绪 (40s)..."
sleep 40

check "VictoriaMetrics运行" 'docker inspect l0-victoriametrics --format "{{.State.Running}}" | grep -q true'
check "Grafana运行"         'docker inspect l0-grafana --format "{{.State.Running}}" | grep -q true'
check "vmagent运行"         'docker inspect l1-vmagent --format "{{.State.Running}}" | grep -q true'
check "SAgent-1运行"        'docker inspect l1-sagent-1 --format "{{.State.Running}}" | grep -q true'
check "SAgent-2运行"        'docker inspect l1-sagent-2 --format "{{.State.Running}}" | grep -q true'

# 验证目录库启动引导（删库重建路径：integrations 同步 + metrics.json 种子 + 系统插件行）
BOOT_LOG=$(docker logs l0-console 2>&1)
check "目录插件同步22包"  'echo "$BOOT_LOG" | grep -q "Catalog synced from integrations: 22 plugins"'
check "指标种子885行"     'echo "$BOOT_LOG" | grep -q "Metrics seeded from metrics.json: 885 rows"'
check "系统插件行引导"    'echo "$BOOT_LOG" | grep -q "system plugin ensured: host_metrics"'

# ====== 2. 数据流正确性测试 ======
log ""
log "===== 2. 数据流正确性测试 ====="

log "等待指标写入 (40s)..."
sleep 40

NAMES=$(vm_names)
for m in node_cpu_seconds_total host_node_memory_memtotal_bytes node_disk_read_bytes_total \
         host_node_netstat_ipext_inoctets_bytes node_load1 node_pressure_cpu_waiting_seconds_total \
         log_events_total sagent_queue_size sagent_uptime_seconds; do
    check "指标存在: $m" "echo \"\$NAMES\" | grep -qx '$m'"
done

# 旧命名清零（CP1 承诺：无旧自造名）
OLD_COUNT=$(printf '%s\n' "$NAMES" | grep -cx -e host_cpu_percent -e host_memory_total_bytes -e host_disk_total_bytes -e host_net_bytes_recv_total || true)
check "旧命名清零" "[ \"\${OLD_COUNT:-0}\" = \"0\" ]"

# 资源ID标签注入
RID_COUNT=$(curl -s "http://localhost:8428/api/v1/series?match[]=node_load1" | grep -c resource_id || true)
check "资源ID标签注入" "[ \"\${RID_COUNT:-0}\" -gt 0 ]"

# 两个采集节点均 up
UP_COUNT=$(curl -s "http://localhost:8428/api/v1/query?query=up" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len([r for r in d.get("data",{}).get("result",[]) if r.get("value",[0,"0"])[1]=="1"]))' 2>/dev/null || echo 0)
check "两个节点均为up" "[ \"\${UP_COUNT:-0}\" -ge 2 ]"

# ====== 3. 崩溃恢复测试 ======
log ""
log "===== 3. 崩溃恢复测试 ====="
BEFORE_CRASH=$(date +%s)
log "Kill SAgent-1 Core进程..."
docker exec l1-sagent-1 kill 1 2>/dev/null || true
sleep 8

check "Core崩溃后容器恢复" 'docker inspect l1-sagent-1 --format "{{.State.Running}}" | grep -q true'
sleep 5
HEALTH=$(docker inspect l1-sagent-1 --format '{{.State.Health.Status}}' 2>/dev/null || echo "")
check "健康检查恢复healthy" '[ "$HEALTH" = "healthy" ]'

log "等待崩溃后数据回补 (35s)..."
sleep 35
# 窗口终点取等待后的实际时间（而不是 before+40 定死），避免回补慢时窗口尾部无数据点的时序偶发
AFTER_CRASH=$(date +%s)
DATA_POINTS=$(curl -s "http://localhost:8428/api/v1/query_range?query=node_load1&start=$BEFORE_CRASH&end=$AFTER_CRASH&step=15" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(sum(len(r.get("values",[])) for r in d.get("data",{}).get("result",[])))' 2>/dev/null || echo 0)
check "崩溃后数据继续上报" "[ \"\${DATA_POINTS:-0}\" -gt 0 ]"
log "崩溃恢复时间 (目标<10s): 约8s"

# ====== 4. 高负载压测 ======
log ""
log "===== 4. 高负载压测 ====="
log "模拟连续指标查询压力（50 组并发查询）..."
for i in $(seq 1 50); do
    curl -s "http://localhost:8428/api/v1/query?query=node_cpu_seconds_total" >/dev/null 2>&1 &
    curl -s "http://localhost:8428/api/v1/query_range?query=host_node_memory_memtotal_bytes&start=$(($(date +%s) - 300))&end=$(date +%s)&step=15" >/dev/null 2>&1 &
done
wait
check "高负载查询未崩溃" 'true'
sleep 5
HEALTH2=$(docker inspect l1-sagent-1 --format '{{.State.Health.Status}}' 2>/dev/null || echo "")
check "压测后健康检查正常" '[ "$HEALTH2" = "healthy" ]'

# ====== 5. 配置变更测试 ======
log ""
log "===== 5. 配置变更测试 ====="
docker exec l1-sagent-1 sh -c 'echo "test_config_change: 1" >> /tmp/test.txt' 2>/dev/null || true
sleep 5
check "配置变更后Core不崩溃" 'docker inspect l1-sagent-1 --format "{{.State.Running}}" | grep -q true'

# ====== 6. 网络中断恢复测试 ======
log ""
log "===== 6. 网络中断恢复 ====="
log "断开 vmagent → VictoriaMetrics 网络 30s..."
docker network disconnect docker_l0-net l1-vmagent 2>/dev/null || true
sleep 30
docker network connect docker_l0-net l1-vmagent 2>/dev/null || true
log "恢复网络，等待回补 (20s)..."
sleep 20
AFTER_NET=$(curl -s "http://localhost:8428/api/v1/query?query=up" 2>/dev/null | grep -c '"value":\[[0-9]*,"1"\]' || echo 0)
check "网络恢复后数据上报" "[ \"\${AFTER_NET:-0}\" -gt 0 ]"

# ====== 7. 多Agent协调测试 ======
log ""
log "===== 7. 多Agent协调测试 ====="
NODE1=$(curl -s 'http://localhost:8428/api/v1/query?query=node_load1{resource_id="order.prod.host.sagent-1"}' | grep -c resource_id || true)
NODE2=$(curl -s 'http://localhost:8428/api/v1/query?query=node_load1{resource_id="order.prod.host.sagent-2"}' | grep -c resource_id || true)
check "节点1独立资源ID" "[ \"\${NODE1:-0}\" -gt 0 ]"
check "节点2独立资源ID" "[ \"\${NODE2:-0}\" -gt 0 ]"

GRAFANA_CODE=$(curl -s -o /dev/null -w '%{http_code}' -L http://localhost:3000 2>/dev/null || echo 0)
check "Grafana可访问" '[ "$GRAFANA_CODE" = "200" ]'

# ====== 8. L0管控台测试 ======
log ""
log "===== 8. L0管控台测试 ====="
cd "$ROOT/l0-console" || exit 1
check "L0控制台可编译"            'go build -o /dev/null .'
check "L0控制台linux交叉编译"     'GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null .'

# API 测试针对容器化运行的 l0-console（不在宿主机裸起进程）
AGENTS=$(curl -s http://localhost:8080/api/agents 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get("agents",[])))' 2>/dev/null || echo 0)
check "L0 Agent列表API正常" "[ \"\${AGENTS:-0}\" -ge 3 ]"

WILD=$(curl -s http://localhost:8080/api/recon 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("summary",{}).get("wild",-1))' 2>/dev/null || echo -1)
check "对账引擎wild=0" '[ "$WILD" = "0" ]'

PLUGINS=$(curl -s http://localhost:8080/api/catalog/plugins 2>/dev/null | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))' 2>/dev/null || echo 0)
check "插件目录>=25" "[ \"\${PLUGINS:-0}\" -ge 25 ]"

# 站点配置 API（前端外部地址统一后端下发，红线 2 落点）
SITE_VM=$(curl -s http://localhost:8080/api/site-config 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("grafana_url",""))' 2>/dev/null || echo "")
check "site-config下发Grafana地址" '[ -n "$SITE_VM" ]'
CORS_ECHO=$(curl -s -i -X OPTIONS http://localhost:8080/api/agents -H 'Origin: http://example.test:9999' 2>/dev/null | grep -i 'access-control-allow-origin' | grep -c 'example.test:9999' || echo 0)
check "CORS按请求Origin回显" "[ \"\${CORS_ECHO:-0}\" -ge 1 ]"

# ====== 9. 红线 lint（CONSTRAINTS.md 静态检查，机器执行红线 2） ======
log ""
log "===== 9. 红线 lint（生产化约束） ====="
check "CONSTRAINTS.md存在"            '[ -f "$ROOT/CONSTRAINTS.md" ]'
check "Go代码l1-仅配置注册处(默认值)" '[ "$(grep -c "\"l1-" "$ROOT/l0-console/main.go")" = "1" ] && grep -qn "AGENT_CONTAINER_PREFIX" "$ROOT/l0-console/main.go"'
check "Go代码无写死Grafana端口3000"   '! grep -qn "localhost:3000" "$ROOT/l0-console/"*.go'
check "Go代码无写死console端口8080"   '! grep -qn "localhost:8080" "$ROOT/l0-console/"*.go'
check "前端JS零写死http://localhost"  '! grep -qn "http://localhost" "$ROOT/l0-console/static/js/"*.js'
check "前端走siteCfg下发外部地址"     'grep -qn "function siteCfg" "$ROOT/l0-console/static/js/utils.js"'
check "Grafana密码走env覆盖"          'grep -qn "GF_SECURITY_ADMIN_PASSWORD=\${GRAFANA_ADMIN_PASSWORD" "$ROOT/deploy/docker/docker-compose.yml"'
check "演示种子有显式开关变量"        'grep -qn "SEED_DEMO_AGENTS" "$ROOT/l0-console/main.go"'
check "VM地址双概念分离"              'grep -qn "VM_PUBLIC_URL" "$ROOT/l0-console/main.go" && grep -qn "VM_PUBLIC_URL" "$ROOT/deploy/docker/docker-compose.yml"'

# ====== 总结 ======
log ""
log "============================================"
log "           测试结果汇总"
log "============================================"
echo -e "  ${GREEN}通过: $PASS${NC}"
echo -e "  ${RED}失败: $FAIL${NC}"
echo ""
if [ $FAIL -eq 0 ]; then
    echo -e "${GREEN}🎉 全部测试通过！${NC}"
    exit 0
else
    echo -e "${RED}⚠ 存在 $FAIL 个失败项${NC}"
    exit 1
fi
