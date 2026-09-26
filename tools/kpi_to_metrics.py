#!/usr/bin/env python3
# kpi_to_metrics.py — 把《sagent-host-kpi.txt》标准表转换为 l0-console 指标目录种子（metrics.json 增量）
# 规则（对应 PLAN 决策 D1/D2/D9）：
#   - 基础资源层 120 项 → plugin=host_metrics（ha_switch 归 custom_scripts，systemd 2 项标二期）
#   - 中间层/容器 37 项 → plugin=docker_metrics，phase=2（二期实现，本期只进目录）
#   - expression 存纯指标名（图表查询用）；计算公式进 note（展示口径）
#   - cat 取采集分组中文名（与 Agent 分组注册表对齐）；level/major/category 为 KPI 表层级三列
import json, sys, io, os

# 源 KPI 表与目标 metrics.json 都从参数/环境变量取，默认落在仓库内相对路径——
# 不再写死任何用户目录（KPI_SRC 也可用第一个命令行参数给出）
SRC = os.environ.get("KPI_SRC") or (sys.argv[1] if len(sys.argv) > 1 else "")
if not SRC:
    sys.exit("用法：kpi_to_metrics.py <KPI 标准表路径>（或用环境变量 KPI_SRC 指定）")
DST = os.environ.get("KPI_DST") or os.path.normpath(
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "l0-console", "data", "metrics.json"))

# 分组 key + 中文名（与 sagent/internal/plugin/builtin/host_metrics.go 注册表一致）
def group_of(name):
    if name.startswith("node_cpu_"): return ("cpu", "CPU")
    if name.startswith("host_node_memory_"): return ("memory", "内存")
    if name.startswith("node_disk_"): return ("disk", "磁盘IO")
    if name.startswith("node_filesystem_"): return ("filesystem", "文件系统")
    if name.startswith("node_network_"): return ("network", "网络设备")
    if name.startswith("host_node_netstat_") or name.startswith("host_node_sockstat_") or name.startswith("node_sockstat_"):
        return ("netstack", "网络协议栈")
    if name in ("node_load1","node_procs_running","node_procs_blocked","node_processes","node_processes_state",
                "node_processes_threads","node_forks_total","node_context_switches_total","node_intr_total"):
        return ("loadproc", "负载与进程")
    if name.startswith(("node_vmstat_","node_entropy_","node_filefd_","node_nf_conntrack_","node_pressure_","node_arp_entries")):
        return ("kernel", "内核统计")
    if name.startswith(("node_uname_","node_time_","node_boot_time_","node_timex_","node_temperature_","node_hwmon_",
                        "node_scrape_collector_","host_up","host_scrape_duration_seconds","node_systemd_")):
        return ("system", "系统与时间")
    if name.startswith("host_"):
        return ("custom", "主机扩展")
    return ("other", "其他")

def metric_type_of(t):
    t = t or ""
    if "计数" in t: return "counter"
    if "直方图" in t or "摘要" in t: return "histogram"
    if "信息" in t: return "info"
    return "gauge"  # 仪表/状态/无单位/标签一律 gauge（info 型 value=1）

def parse_tsv(path):
    rows, row, in_quote = [], [], False
    for line in io.open(path, encoding="utf-8"):
        line = line.rstrip("\n")
        if in_quote:
            row[-1] += "\n" + line
            if row[-1].count('"') % 2 == 0: in_quote = False
            continue
        cells = next_all(row, line)
        row = cells
    return rows

def next_all(row, line):
    # 按制表符切分，处理 "..." 内含换行/制表符
    cells, cur, i, q = [], [], 0, False
    while i < len(line):
        c = line[i]
        if c == '"':
            q = not q
        elif c == '\t' and not q:
            cells.append("".join(cur)); cur = []
            i += 1; continue
        cur.append(c); i += 1
    cells.append("".join(cur))
    return cells

def clean(s):
    if s is None: return ""
    s = s.strip().strip('"').replace('""','"')
    # 公式里的双引号被原文转成两个，还原为一个
    s = s.replace('""', '"')
    return s.strip()

def main():
    import csv
    data_rows = []
    with open(SRC, encoding="utf-8", newline="") as fp:
        reader = csv.reader(fp, delimiter="\t", quotechar='"')
        rows = [r for r in reader if any(c.strip() for c in r)]
    header = [h.strip() for h in rows[0]]
    idx = {h: i for i, h in enumerate(header)}
    entries = []
    for f in rows[1:]:
        if len(f) < len(header):
            f += [""] * (len(header) - len(f))
        get = lambda k: clean(f[idx[k]])
        level, major, category = get("层级"), get("资源大类"), get("资源名称")
        name, unit = get("指标代码"), get("计量单位")
        # 注意：原表“指标说明”列内容实际是类型描述（如“计数器 (Counter)”），真实口径在“指标计算公式”列
        type_col = get("指标说明")
        formula = get("指标计算公式")
        impl = get("建议是否部署监控")
        freq = get("建议采集频率")
        deployer = get("采集器/Exporter")
        constraint = get("部署约束说明")
        if not name:
            continue
        gkey, glabel = group_of(name)
        if name == "host_ha_switch_status":
            entries.append(dict(plugin="custom_scripts", name=name, type="gauge", unit=unit,
                desc=(formula or type_col) + "（口径待定，走自定义脚本采集）", labels="", cat="主机扩展",
                status="active", source="kpi_seed", level=level, major=major, category=category,
                grp="custom", channel="script", ownership="custom", source_ref="custom_scripts",
                min_agent_version="", phase=1, freq=freq))
            continue
        if name.startswith("docker_"):
            entries.append(dict(plugin="docker_metrics", name=name, type=metric_type_of(type_col), unit=unit,
                desc=formula or type_col, labels="", cat="容器", status="active", source="kpi_seed",
                level=level, major=major, category=category, grp="", channel="builtin",
                ownership="preset", source_ref="docker_metrics", min_agent_version="",
                phase=2, freq=freq, constraint=constraint))
            continue
        desc_full = formula if formula else type_col
        entries.append(dict(plugin="host_metrics", name=name, type=metric_type_of(type_col), unit=unit,
            desc=desc_full, labels="", cat=glabel, status="active", source="kpi_seed",
            level=level, major=major, category=category, grp=gkey, channel="builtin",
            ownership="preset", source_ref="host_metrics", min_agent_version="v0.4.0",
            phase=2 if name.startswith("node_systemd_") else 1, freq=freq))
    # 合并进 metrics.json：移除旧 host_metrics 18 项自造名，追加新条目
    data = json.load(io.open(DST, encoding="utf-8"))
    kept = [m for m in data if m.get("plugin") != "host_metrics"]
    removed = len(data) - len(kept)
    # host_ha_switch_status 若已存在不重复
    have = {m["name"] for m in kept}
    added = 0
    for e in entries:
        if e["name"] in have:
            continue
        e["updated_at"] = "2026-09-20 15:50"
        kept.append(e)
        added += 1
    json.dump(kept, io.open(DST, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print(f"removed_old_host_metrics={removed} added={added} total={len(kept)}")
    from collections import Counter
    print(Counter(m["plugin"] for m in kept))
    print(Counter(m.get("grp","") for m in kept if m["plugin"]=="host_metrics"))

if __name__ == "__main__":
    main()
