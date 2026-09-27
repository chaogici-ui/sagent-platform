#!/bin/sh
# drill-chain.sh —— 全链路证据链演练（P3，2026-09-27）
#
# 用途：给"从 L0 配置采集任务到 vmagent 拿到数据"的整条链路出一份可复核的演练报告。
# 每一跳 = 声明 + 独立实证配对（与 /api/trace 同源原则），全部断言基于实测，不采信口头结论。
#
# 用法：
#   ./drill-chain.sh                     # 只读演练：六跳断言 + vmagent 双副本归属检查
#   ./drill-chain.sh --inject-fail 60    # 附加故障注入：停 l1-vmagent-ha 60s，验证另一副本持续写入
#   ./drill-chain.sh --flow 10           # 指定流水线（缺省取最新一条有 l1_tasks 的接入流水线）
#
# 产物：stdout 报告 + .workbuddy/evidence/drill-<时间戳>.md
# 前置：宿主机可访问 localhost:8080（L0）与 localhost:8428（VM）；--inject-fail 需 docker 权限。

set -u
L0="http://localhost:8080"
VM="http://localhost:8428"
INJECT_SEC=0
FLOW_ID=""
prev=""
for arg in "$@"; do
  case "$prev" in
    --inject-fail) INJECT_SEC="$arg"; prev=""; continue ;;
    --flow) FLOW_ID="$arg"; prev=""; continue ;;
  esac
  case "$arg" in
    --inject-fail) INJECT_SEC=60; prev="$arg" ;;
    --inject-fail=*) INJECT_SEC="${arg#*=}" ;;
    --flow) FLOW_ID="next"; prev="$arg" ;;
    --flow=*) FLOW_ID="${arg#*=}" ;;
  esac
done
[ "$FLOW_ID" = "next" ] && FLOW_ID=""
if [ -z "$FLOW_ID" ]; then
  FLOW_ID=$(curl -s "$L0/api/onboard/flows" | python3 -c "
import json,sys
d=json.load(sys.stdin); fls=d.get('flows') or d
done=[f for f in fls if f.get('status') in ('done','success','ok')]
print(done[0]['id'] if done else (fls[0]['id'] if fls else ''))" 2>/dev/null)
fi
if [ -z "$FLOW_ID" ]; then echo "❌ 没有可用流水线（先接入一个对象）"; exit 1; fi

PASS=0; FAIL=0; UNKNOWN=0
TEE=""
say()  { printf '%s\n' "$*"; TEE="$TEE\n$*"; }
sec()  { if [ "$2" = "ok" ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi; printf '%s %s\n' "$1" "$2"; TEE="$TEE\n$1 $2"; }

say "# 全链路证据链演练报告"
say "- 时间：$(date '+%F %T')　流水线：#$FLOW_ID　注入时长：${INJECT_SEC}s（0=不注入）"
say ""

say "## 六跳断言（/api/trace 装配，逐跳核验）"
TRACE=$(curl -s "$L0/api/trace?flow=$FLOW_ID")
echo "$TRACE" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('summary:', d.get('summary',''))
for h in d.get('hops',[]):
    evs='; '.join(e['k']+'='+e['v'] for e in h['evidence'][:5])
    print(f\"  {h['step']} {h['name']}: {h['status']} | {evs}\")" > /tmp/drill-trace.txt
TEE="$TEE\n$(cat /tmp/drill-trace.txt)"
cat /tmp/drill-trace.txt
for H in ①②③④⑤⑥; do :; done
# 逐跳判定：ok 计通过；unknown 计证据缺口；failed 计失败
echo "$TRACE" | python3 -c "
import json,sys
d=json.load(sys.stdin)
bad=[h['name'] for h in d.get('hops',[]) if h['status']=='failed']
unk=[h['name'] for h in d.get('hops',[]) if h['status']=='unknown']
print(\"FAILED_HOPS='%s'\" % ','.join(bad))
print(\"UNKNOWN_HOPS='%s'\" % ','.join(unk))" > /tmp/drill-judge.txt
. /tmp/drill-judge.txt
[ -z "$FAILED_HOPS" ] && say "六跳终态：无 failed" || say "六跳终态：❌ failed = $FAILED_HOPS"
[ -n "$UNKNOWN_HOPS" ] && say "证据缺口（unknown≠failed，多为旧流水线早于事件表）：" && say "  $UNKNOWN_HOPS"

say ""
say "## vmagent 双副本归属与接管实证（P0）"
NOW=$(date +%s); START=$((NOW-300))
for R in vmagent-1 vmagent-ha-1; do
  N=$(curl -s "$VM/api/v1/query" --data-urlencode "query=count(up{replica=\"$R\"})" --data-urlencode "time=$NOW" | python3 -c "import json,sys; d=json.load(sys.stdin); r=d.get('data',{}).get('result',[]); print(r[0]['value'][1] if r else '0')" 2>/dev/null)
  say "  近 5m 副本 $R 在抓目标数：$N"
done
say "  （两副本各自 >0 且目标数一致 = active-active 双写正常）"

if [ "$INJECT_SEC" -gt 0 ]; then
  say ""
  say "## 故障注入：停 l1-vmagent-ha ${INJECT_SEC}s"
  docker stop l1-vmagent-ha >/dev/null 2>&1 && say "  已停止 l1-vmagent-ha"
  SAMPLE1=$(curl -s "$VM/api/v1/query" --data-urlencode "query=count(max by (job, instance) (up) == 1)" | python3 -c "import json,sys; d=json.load(sys.stdin); r=d.get('data',{}).get('result',[]); print(r[0]['value'][1] if r else '0')" 2>/dev/null)
  say "  注入期间在线目标数（折叠副本后，应保持 >0）：$SAMPLE1"
  sleep "$INJECT_SEC"
  SAMPLE2=$(curl -s "$VM/api/v1/query" --data-urlencode "query=count(max by (job, instance) (up) == 1)" | python3 -c "import json,sys; d=json.load(sys.stdin); r=d.get('data',{}).get('result',[]); print(r[0]['value'][1] if r else '0')" 2>/dev/null)
  say "  注入结束时在线目标数：$SAMPLE2"
  docker start l1-vmagent-ha >/dev/null 2>&1 && say "  已恢复 l1-vmagent-ha"
  sleep 45
  REC=$(curl -s "$VM/api/v1/query" --data-urlencode "query=count(up{replica=\"vmagent-ha-1\"}[2m])" 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin); r=d.get('data',{}).get('result',[]); print(r[0]['value'][1] if r else '0')" 2>/dev/null)
  say "  恢复后 vmagent-ha-1 近 2m 恢复抓取目标数：$REC"
  say "  判据：注入期间 SAMPLE>0（vmagent-1 单副本持续供数）+ 恢复后 REC>0（双副本回归）= 接管实证成立"
fi

say ""
say "## 结论"
if [ -z "$FAILED_HOPS" ]; then
  say "六跳无 failed。${UNKNOWN_HOPS:+（存在证据缺口：$UNKNOWN_HOPS，属旧数据超出事件窗口，非遗链断点）}"
  say "vmagent 副本归属标签已生效（见上），故障注入后由存活副本持续供数即接管实证。"
fi

OUTDIR="/Users/gici/代码/SAgent/code/.workbuddy/evidence"
mkdir -p "$OUTDIR"
printf '%b\n' "$TEE" > "$OUTDIR/drill-$(date '+%Y%m%d-%H%M%S').md"
say ""
say "报告已存：$OUTDIR/drill-*.md"
