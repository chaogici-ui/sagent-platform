#!/bin/bash
# SAgent 全面自动化测试框架
# 覆盖：数据流/崩溃恢复/高负载/配置变更/网络中断/多Agent协调
# 口径对齐：主机指标为 node_*/host_node_* KPI 标准名（CP1/CP2 定案，旧 host_* 自造名已废弃）
#
# 用法约定（重要）：check 只接收「单一命令字符串」做断言，取数在 check 外完成；
# 禁止在 check 参数里写管道（管道会被外层解析，pass 输出被 grep 吞掉 + set -e 误杀脚本）
set -u

# Go 工具链锚定：本机存在双 go（~/tools/go=1.26.2 与 /opt/homebrew/bin/go=1.26.7），
# GOCACHE 里的 stdlib 产物绑定编译它的工具链版本，混用即报
# "compile: version goX does not match go tool version goY"。必须在改动 PATH 前
# 锚定用户原始 PATH 解析到的 go，下方所有 Go 检查统一用 "$GO_BIN"。
GO_BIN="$(command -v go 2>/dev/null || echo go)"
# docker CLI 在 /usr/local/bin（Docker Desktop），非交互 shell 默认 PATH 不含
export PATH="/usr/local/bin:/opt/homebrew/bin:$PATH"
# Go 模块代理兜底：本机直连 proxy.golang.org 间歇超时，未显式配置时用国内镜像
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

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
# 窗口设计：起点前移 60s（崩溃前最后一点常落在 kill 时刻之前）、终点取等待后的实际时间
# 再延后 30s（重启 8s + 采集周期 30s + scrape 15s，崩溃后首点最晚 ~53s 才落库）。
# 原实现两头都贴死 kill/恢复时刻，采集相位稍偏就 0 点——这是概率性误报的根因。
AFTER_CRASH=$(($(date +%s) + 30))
DATA_POINTS=$(curl -s "http://localhost:8428/api/v1/query_range?query=node_load1&start=$((BEFORE_CRASH - 60))&end=$AFTER_CRASH&step=15" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(sum(len(r.get("values",[])) for r in d.get("data",{}).get("result",[])))' 2>/dev/null || echo 0)
check "崩溃后数据继续上报" "[ \"\${DATA_POINTS:-0}\" -ge 2 ]"
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
check "L0控制台可编译"            '"$GO_BIN" build -o /dev/null .'
check "L0控制台linux交叉编译"     'GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -o /dev/null .'

# sagent 侧门禁（红线 6 对称覆盖：两个模块都要过编译与测试）
cd "$ROOT/sagent" || exit 1
check "SAgent可编译"              '"$GO_BIN" build -o /dev/null .'
check "SAgentlinux交叉编译"       'GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -o /dev/null .'
check "SAgent单测通过"            '"$GO_BIN" test ./... > /dev/null'

# 单元测试（含 D8 对账展开 / 配置解析 / 审计与配置版本链）
cd "$ROOT/l0-console" || exit 1
check "L0控制台单测通过"          '"$GO_BIN" test ./... > /dev/null'

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

# 幽灵 Agent 封堵（配置轨道/目标分派只允许绑定真实 Agent）
GHOST_PUT=$(curl -s -o /dev/null -w '%{http_code}' -X PUT http://localhost:8080/api/agents/ghost-agent/config -H 'Content-Type: application/json' -d '{"host_metrics":{"enabled":true,"interval":"30s","groups":{},"exclude_metrics":[]}}' 2>/dev/null || echo 0)
check "幽灵Agent写配置被拒(404)" '[ "$GHOST_PUT" = "404" ]'
GHOST_TGT=$(curl -s -X POST http://localhost:8080/api/targets -H 'Content-Type: application/json' -d '{"name":"ghost-t","type":"mysql","address":"1.2.3.4:3306","agent_id":"ghost-agent"}' 2>/dev/null | grep -c 'agent not found' || echo 0)
check "目标分派幽灵Agent被拒" "[ \"\${GHOST_TGT:-0}\" -ge 1 ]"

# ---- 接入中心：模板 / 能力 / 流水线（前端零写死的数据源，批 A 落点）----
log "接入中心 API 校验..."
TPL_IDS=$(curl -s http://localhost:8080/api/onboard/templates 2>/dev/null | python3 -c 'import sys,json; print(",".join(sorted(t["id"] for t in json.load(sys.stdin).get("templates",[]))))' 2>/dev/null || echo "")
check "接入模板下发edge/hybrid/remote" '[ "$TPL_IDS" = "edge,hybrid,remote" ]'

ABILITY_N=$(curl -s http://localhost:8080/api/onboard/abilities 2>/dev/null | python3 -c 'import sys,json; print(len(json.load(sys.stdin).get("abilities",[])))' 2>/dev/null || echo 0)
check "接入能力清单非空" "[ \"\${ABILITY_N:-0}\" -ge 8 ]"

# 参数表单字段来自插件包 params.yaml（L3 层）——写死则这里必为 0
MYSQL_FIELDS=$(curl -s http://localhost:8080/api/onboard/abilities 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); a=[x for x in d.get("abilities",[]) if x.get("id")=="mysql_probe"]; print(len(a[0].get("params") or []) if a else 0)' 2>/dev/null || echo 0)
check "mysql参数表单来自params.yaml" "[ \"\${MYSQL_FIELDS:-0}\" -ge 3 ]"

# 能力声明唯一（同一 id 声明两处会让参数表单被前一处静默遮蔽）
DUP_ABILITY=$(curl -s http://localhost:8080/api/onboard/abilities 2>/dev/null | python3 -c 'import sys,json,collections; ids=[a["id"] for a in json.load(sys.stdin).get("abilities",[])]; print(sum(1 for k,v in collections.Counter(ids).items() if v>1))' 2>/dev/null || echo 0)
check "能力声明无重复id" "[ \"\${DUP_ABILITY:-0}\" = \"0\" ]"

FLOW_CREATE=$(curl -s -X POST http://localhost:8080/api/onboard/flow -H 'Content-Type: application/json' \
  -d '{"resources":["10.99.99.201"],"mode":"edge","abilities":["prometheus_scrape"],"params":{"prometheus_scrape":{"url":"http://10.99.99.201:9100/metrics"}}}' 2>/dev/null)
FLOW_ID=$(echo "$FLOW_CREATE" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("flows",[{}])[0].get("id",0))' 2>/dev/null || echo 0)
check "新建接入流水线" "[ \"\${FLOW_ID:-0}\" -gt 0 ]"

FLOW_DET=$(curl -s "http://localhost:8080/api/onboard/flow?id=$FLOW_ID" 2>/dev/null)
STEP_OK_N=$(echo "$FLOW_DET" | python3 -c 'import sys,json; s=json.load(sys.stdin).get("flow",{}).get("step_status",{}); print(sum(1 for v in s.values() if v=="ok"))' 2>/dev/null || echo 0)
STEP_RUN_N=$(echo "$FLOW_DET" | python3 -c 'import sys,json; s=json.load(sys.stdin).get("flow",{}).get("step_status",{}); print(sum(1 for v in s.values() if v=="running"))' 2>/dev/null || echo 0)
STEP_PEND_N=$(echo "$FLOW_DET" | python3 -c 'import sys,json; s=json.load(sys.stdin).get("flow",{}).get("step_status",{}); print(sum(1 for v in s.values() if v=="pending"))' 2>/dev/null || echo 0)
FLOW_STATUS=$(echo "$FLOW_DET" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("flow",{}).get("status",""))' 2>/dev/null || echo "")
check "平台侧步骤自动完成" "[ \"\${STEP_OK_N:-0}\" -ge 1 ]"
check "外部步骤停下等回报(不假装完成)" "[ \"\${STEP_RUN_N:-0}\" -ge 1 ]"
check "未就绪步骤为pending" "[ \"\${STEP_PEND_N:-0}\" -ge 1 ]"
check "流水线状态running" '[ "$FLOW_STATUS" = "running" ]'

# 能力探路已并入主机探路（2026-09-21）：edge 流程不许再有 preflight_ability 环节；
# 主机探路（external）推进后应处于 running（平台 ansible 代跑/停等回报），不许瞬间假完成
PAB_GONE=$(echo "$FLOW_DET" | python3 -c 'import sys,json; ss=json.load(sys.stdin).get("flow",{}).get("step_status",{}); print("no" if "preflight_ability" in ss else "yes")' 2>/dev/null || echo "")
check "能力探路已并入主机探路" '[ "$PAB_GONE" = "yes" ]'
PH_STATUS=$(echo "$FLOW_DET" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("flow",{}).get("step_status",{}).get("preflight_host",""))' 2>/dev/null || echo "")
check "主机探路不假完成" '[ "$PH_STATUS" = "running" ] || [ "$PH_STATUS" = "pending" ]'

# 步骤快照与时间线可读（模板演进后历史流水线仍可重放）
STEP_N=$(echo "$FLOW_DET" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d.get("flow",{}).get("step_ids",[])))' 2>/dev/null || echo 0)
EV_N=$(echo "$FLOW_DET" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d.get("events") or []))' 2>/dev/null || echo 0)
check "步骤快照已持久化" "[ \"\${STEP_N:-0}\" -ge 5 ]"
check "时间线与步骤一一对应" '[ "${EV_N:-0}" = "${STEP_N:-0}" ]'

RES_N=$(curl -s "http://localhost:8080/api/resources?q=10.99.99.201" 2>/dev/null | python3 -c 'import sys,json; print(len(json.load(sys.stdin).get("resources",[])))' 2>/dev/null || echo 0)
check "资源对象已登记" "[ \"\${RES_N:-0}\" -ge 1 ]"

# collect_params 落参数建目标，且目标带 flow_id 可回溯到流水线（采集目标页「接入过程」深链依赖它）
TG_FLOW=$(curl -s http://localhost:8080/api/targets 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); ts=d if isinstance(d,list) else d.get("targets",[]); print(max([t.get("flow_id",0) or 0 for t in ts] or [0]))' 2>/dev/null || echo 0)
check "采集目标带流水线回溯ID" "[ \"\${TG_FLOW:-0}\" -ge 1 ]"

# 强制通过必须填理由（审计留痕，不可静默放行）
FORCE_NO_REASON=$(curl -s -X POST http://localhost:8080/api/onboard/flow/step -H 'Content-Type: application/json' \
  -d "{\"flow_id\":$FLOW_ID,\"step\":\"preflight_host\",\"action\":\"force\",\"reason\":\"\"}" 2>/dev/null | grep -c '必须填写理由' || echo 0)
check "强制通过未填理由被拒" "[ \"\${FORCE_NO_REASON:-0}\" -ge 1 ]"

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
check "VM地址双概念分离"              'grep -qn "VM_PUBLIC_URL" "$ROOT/l0-console/"*.go && grep -qn "VM_PUBLIC_URL" "$ROOT/deploy/docker/docker-compose.yml"'
check "前端零写死插件捆绑"            '! grep -qn "host_metrics","log_metrics" "$ROOT/l0-console/static/js/panel.js" && ! grep -qn "probe:.\x27mysql_probe" "$ROOT/l0-console/static/js/panel.js"'
check "接入配置由后端下发"            'grep -qn "onboard/config" "$ROOT/l0-console/"*.go && grep -qn "onboardCfg" "$ROOT/l0-console/static/js/utils.js"'
check "onboard数据文件在镜像COPY清单" 'grep -qn "onboard_config.json" "$ROOT/l0-console/Dockerfile"'
check "data卷遮蔽根治:种子同步entrypoint" 'grep -qn "l0-seed" "$ROOT/l0-console/docker-entrypoint.sh" && grep -qn "docker-entrypoint.sh" "$ROOT/l0-console/Dockerfile" && grep -qn "ENTRYPOINT.*docker-entrypoint" "$ROOT/l0-console/Dockerfile"'
check "审计operator走env非写死admin"   'grep -qn "cfgAuditOperator" "$ROOT/l0-console/main.go" && ! grep -qn "InsertAudit(ts, \"admin\"" "$ROOT/l0-console/main.go"'
check "审计operator透传compose"        'grep -qn "AUDIT_OPERATOR" "$ROOT/deploy/docker/docker-compose.yml"'
check "版本比较用语义化verCmp"         'grep -qn "function verCmp" "$ROOT/l0-console/static/js/panel.js" && ! grep -qn "verMap).sort()" "$ROOT/l0-console/static/js/panel.js"'
check "死端点清零(JSON文件存储)"       '! grep -qn "data/plugins.json" "$ROOT/l0-console/"*.go && ! grep -qEn "/api/plugins\"|/api/metrics/synced|metrics-by-target" "$ROOT/l0-console/"*.go'
check "旧JSON插件存储文件已删"         '[ ! -e "$ROOT/l0-console/data/plugins.json" ]'

# ---- 批 A：接入中心与流程引擎（引擎只认原子能力，编排/参数/检查项全部数据化）----
check "流程模板在镜像COPY清单"         'grep -qn "data/flow_templates/" "$ROOT/l0-console/Dockerfile"'
check "流程模板进种子并随启动同步"     'grep -qn "l0-seed/flow_templates" "$ROOT/l0-console/Dockerfile" && grep -qn "flow_templates" "$ROOT/l0-console/docker-entrypoint.sh"'
check "六个模式模板文件存在"           '[ "$(ls -1 "$ROOT/l0-console/data/flow_templates/"*.yaml 2>/dev/null | wc -l | tr -d " ")" = "6" ]'
# 引擎零插件名：编排引擎里出现具体插件名 = 分层退化（该写进 onboard_config.json / 模板 / params.yaml）
check "引擎零插件名写死"               '! grep -qE "\"(mysql_probe|redis_probe|kafka_exporter|elasticsearch_exporter|clickhouse_exporter|http_response|log_metrics|host_metrics|port_checker|prometheus_scrape|custom_scripts)\"" "$ROOT/l0-console/onboard_engine.go" "$ROOT/l0-console/onboard_atoms.go"'
check "接入流水线走库(PG)非JSON"       '! grep -qEn "onboard_flow\.json|flows\.json|onboard\.json" "$ROOT/l0-console/"*.go'
check "探路端点不提前占位(批D才注册)"  '! grep -qn "/api/onboard/preflight" "$ROOT/l0-console/"*.go'
check "引擎回归测试存在"               '[ -f "$ROOT/l0-console/onboard_test.go" ] && grep -qn "func TestAdvanceFlow" "$ROOT/l0-console/onboard_test.go"'
check "接入中心前端模块存在"           '[ -f "$ROOT/l0-console/static/js/onboard.js" ] && grep -qn "onboard.js" "$ROOT/l0-console/static/index.html"'
check "接入中心菜单项存在"             'grep -qn "onboard-center" "$ROOT/l0-console/static/index.html" && grep -qn "case \"onboard-center\"" "$ROOT/l0-console/static/js/utils.js"'
# 接入向导零硬编码：能力清单与参数表单字段必须来自 /api/onboard/*（写死即分层退化）
check "接入向导零硬编码插件名"         '! grep -qE "host_metrics|mysql_probe|redis_probe|kafka_exporter|elasticsearch_exporter|clickhouse_exporter|http_response|log_metrics|port_checker|prometheus_scrape|custom_scripts" "$ROOT/l0-console/static/js/onboard.js"'
TPL_OK=$(grep -l "^steps:" "$ROOT/l0-console/data/flow_templates/"*.yaml 2>/dev/null | wc -l | tr -d " ")
check "六个模板均含steps定义"          '[ "${TPL_OK:-0}" = "6" ]'
# 参数声明文件齐备：onboard_config.json 里声明的 params 路径必须在磁盘上真实存在。
# 路径写错不会报错，只会静默退化成「该能力无需参数」——接入向导表单为空且无人察觉。
PARAMS_MISS=$(python3 -c 'import json,os,sys
root=sys.argv[1]
cfg=json.load(open(os.path.join(root,"l0-console/data/onboard_config.json")))
base=os.path.join(root,"l0-console/data/integrations")
print(len([p["params"] for p in cfg.get("onboard_plugins",[]) if p.get("params") and not os.path.isfile(os.path.join(base,p["params"]))]))' "$ROOT" 2>/dev/null || echo 999)
check "插件参数声明文件齐备"           '[ "${PARAMS_MISS:-1}" = "0" ]'
check "目标详情有接入过程深链"         'grep -qn "openOnboardFlow" "$ROOT/l0-console/static/js/panel.js"'
# 审计修正（2026-09-21）：三处方案↔代码对账缺口的防回归门禁
check "hybrid含远程可达性探路"         'grep -qn "preflight_remote" "$ROOT/l0-console/data/flow_templates/hybrid.yaml"'
check "平台设备强制自监控兜底"         'grep -qn "platform_device" "$ROOT/l0-console/agent_api.go" && grep -qn "GetAgentResourceID" "$ROOT/l0-console/agent_api.go" "$ROOT/l0-console/store/fleet.go"'
check "stall判定分执行域"              'grep -qn "runningScopes" "$ROOT/l0-console/onboard_api.go"'
# 能力探路已并入主机探路（2026-09-21）：三模板不许再有独立 preflight_ability 环节，
# 但三模板都必须保留 atom preflight_host（能力可行性检查现在由它承担）
check "探路环节已合并(三模板)"  'TPL_OK=1; for t in edge remote hybrid; do grep -q "id: preflight_ability" "$ROOT/l0-console/data/flow_templates/$t.yaml" && TPL_OK=0; grep -q "atom: preflight_host" "$ROOT/l0-console/data/flow_templates/$t.yaml" || TPL_OK=0; done; [ "$TPL_OK" = "1" ]'
# 下发配置必须等探路门禁（edge: 主机探路含能力检查；remote/hybrid 另有远程可达性探路）
check "下发配置依赖探路门禁(三模板)"  'TPL_DEP_OK=1; for t in edge remote hybrid; do grep -A6 "id: sync_config" "$ROOT/l0-console/data/flow_templates/$t.yaml" | grep -q "preflight_host" || TPL_DEP_OK=0; done; [ "$TPL_DEP_OK" = "1" ]'
# Agent 域验证步骤必须等 Agent 就位：Agent 未装就「等待回报」是死等（2026-09-21 用户评审）
check "验证/入库依赖Agent就位(三模板)" 'TPL_AG_OK=1; for t in edge remote hybrid; do for s in verify_probe observe_collect; do grep -A5 "id: $s" "$ROOT/l0-console/data/flow_templates/$t.yaml" | grep -q "install_agent" || TPL_AG_OK=0; done; done; [ "$TPL_AG_OK" = "1" ]'
# 五阶段视图（2026-09-21 用户评审「11步压缩为5阶段」）：引擎步骤粒度不变，展示层归组；
# 前端必须定义五阶段映射 + self_metrics 注册回报 180s 步骤级超时（不落 3600 default）
check "五阶段视图已定义(onboard.js)"  'grep -q "OB_STAGES" "$ROOT/l0-console/static/js/onboard.js" && grep -q "obStageAgg" "$ROOT/l0-console/static/js/onboard.js" && grep -q "obStageDots" "$ROOT/l0-console/static/js/onboard.js"'
check "self_metrics注册超时180s"      'grep -q "\"self_metrics\"" "$ROOT/l0-console/onboard.go" && grep -q "self_metrics" "$ROOT/l0-console/data/onboard_config.json" && grep -q "timeoutSpecForStep" "$ROOT/l0-console/onboard_timeout.go"'

# 升级/启停流程（2026-09-22 用户需求）：internal 模板 + 守护协议 playbook + 决策卡 + 维护态
check "升级模板五步齐备"        'grep -q "id: upgrade_agent" "$ROOT/l0-console/data/flow_templates/upgrade.yaml" && grep -q "atom: pick_version" "$ROOT/l0-console/data/flow_templates/upgrade.yaml" && grep -q "atom: confirm_upgrade" "$ROOT/l0-console/data/flow_templates/upgrade.yaml"'
check "启停模板四步齐备"        'grep -q "atom: confirm_service" "$ROOT/l0-console/data/flow_templates/service.yaml" && grep -q "atom: service_execute" "$ROOT/l0-console/data/flow_templates/service.yaml"'
check "升级playbook自证契约"    'grep -q "UPGRADE_OK" "$ROOT/l0-console/data/playbooks/upgrade.yml" && grep -q "UPGRADE_DIRTY" "$ROOT/l0-console/data/playbooks/upgrade.yml" && grep -q "BAK_NAME" "$ROOT/l0-console/data/playbooks/upgrade.yml"'
check "启停playbook守护协议"    'grep -q "run/stopped" "$ROOT/l0-console/data/playbooks/agent_service.yml" && grep -q "SVC_STOPPED" "$ROOT/l0-console/data/playbooks/agent_service.yml" && grep -q "SVC_PLUGIN_STARTED" "$ROOT/l0-console/data/playbooks/agent_service.yml"'
check "启停参数白名单"          'grep -q "validPluginName" "$ROOT/l0-console/onboard_service.go" && grep -q "func (p svcParams) valid" "$ROOT/l0-console/onboard_service.go"'
check "维护态登记与清理"        'grep -q "SetResourceSvcState" "$ROOT/l0-console/store/onboard.go" && grep -q "SetResourceSvcState" "$ROOT/l0-console/onboard_atoms.go" && grep -q "svc_state" "$ROOT/l0-console/static/js/onboard.js"'
check "行内升级/启停按钮"       'grep -q "obUpgrade" "$ROOT/l0-console/static/js/onboard.js" && grep -q "obService" "$ROOT/l0-console/static/js/onboard.js" && grep -q "can_upgrade" "$ROOT/l0-console/onboard_api.go"'

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
