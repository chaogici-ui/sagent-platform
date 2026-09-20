#!/usr/bin/env bash
# kpi_diff.sh — SAgent host_metrics 与 node_exporter 同机口径校验（CP2 验收关卡）
# 用法（在 Linux 主机上）：
#   1. 同机运行 node_exporter（默认 :9100）与 SAgent（默认 :19090）
#   2. ./kpi_diff.sh 9100 19090 > diff_report.txt
# 输出：三类差异 —— 仅 node_exporter 有 / 仅 SAgent 有 / 同名不同值
# 说明：同名不同值需人工判断（counter 类累计值、per-label 实例数天然不同，
#       重点核对 netdev 设备过滤、filesystem 挂载过滤两处）。
set -euo pipefail

NE_PORT=${1:-9100}
SA_PORT=${2:-19090}
TMP=$(mktemp -d)

curl -s "http://localhost:${NE_PORT}/metrics" > "$TMP/node.txt"
curl -s "http://localhost:${SA_PORT}/metrics" > "$TMP/sagent.txt"

# 抽取指标名（去 HELP/TYPE 行，series 名按 { 前与 label 排序归并）
extract_names() {
  grep -v '^#' "$1" | awk '{print $1}' | sed 's/{.*//' | grep -v '^$' | sort -u
}

extract_names "$TMP/node.txt"    > "$TMP/node_names.txt"
extract_names "$TMP/sagent.txt"  > "$TMP/sagent_names.txt"

# SAgent host_metrics 相关前缀（KPI 标准表三段命名）
grep -E '^(node_|host_node_|host_)' "$TMP/sagent_names.txt" > "$TMP/sagent_host_names.txt" || true

echo "== 仅 node_exporter 有（SAgent 缺失，需评估）=="
comm -23 "$TMP/node_names.txt" "$TMP/sagent_host_names.txt" || true

echo
echo "== 仅 SAgent 有（超出 node_exporter 声明集，需对照 KPI 表确认）=="
comm -13 "$TMP/node_names.txt" "$TMP/sagent_host_names.txt" || true

echo
echo "== 值抽样对比（同名 gauge 型指标，差异 >1% 列出）=="
# 取少量纯 gauge 单序列指标对比
for m in node_load1 node_time_seconds node_boot_time_seconds node_filefd_allocated \
         node_filefd_maximum node_entropy_available_bits node_sockstat_sockets_used \
         node_nf_conntrack_entries node_arp_entries; do
  nv=$(grep -m1 "^${m} " "$TMP/node.txt" | awk '{print $2}' || true)
  sv=$(grep -m1 "^${m} " "$TMP/sagent.txt" | awk '{print $2}' || true)
  if [ -n "$nv" ] && [ -n "$sv" ]; then
    awk -v m="$m" -v a="$nv" -v b="$sv" 'BEGIN{
      d=(a==b)?0:(a-b)/a; if(d<0)d=-d;
      if(d>0.01) printf "  %-36s node=%s sagent=%s 差异%.2f%%\n", m, a, b, d*100;
    }'
  fi
done
echo "== 校验完成，剩余差异按 PLAN §3 说明人工核对 =="
