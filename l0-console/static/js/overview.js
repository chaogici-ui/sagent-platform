// 采集总览（图 1 的「📊 总览」）：回答"现在怎么样"——采集中 / 异常 / 未接入 / 待处理，
// 加上最近事件流。下钻入口：资源页（按状态过滤）与 Grafana。
// 数据与资源页同源（台账×心跳×流水线×对账×VM 实测），口径只有一处。
function renderHealthOverview() {
  var mc = document.getElementById('main-content');
  mc.innerHTML = '<div class="card"><div class="card-bd" style="text-align:center;color:var(--muted);padding:24px">⏳ 汇总采集实况…</div></div>';
  resFetchAll(function (d) {
    var rows = resBuildRows(d);
    var nOk = 0, nBad = 0, nOff = 0;
    rows.forEach(function (r) {
      if (r.state.key === 'collecting') nOk++;
      else if (r.state.key === 'off') nOff++;
      else nBad++;
    });
    var flows = d.flows || [];
    var pending = flows.filter(function (f) {
      return f.status === 'running' || f.status === 'blocked' || f.status === 'stalled' || f.status === 'failed';
    });
    var summary = (d.recon && d.recon.summary) || {};
    var h = '';

    h += '<div class="card"><div class="card-hd">◧ 工作台'
      + '<span style="font-weight:400;font-size:11px;color:var(--muted)">　现在最要紧的事是什么？'
      + (d.recon && d.recon.generated_at ? ' · 对账时间 ' + ovEsc(d.recon.generated_at) : '') + '</span>'
      + '<button class="btn btn-o btn-sm" style="float:right" onclick="renderHealthOverview()">↻ 刷新</button></div><div class="card-bd">';

    // —— 结论卡（IA 设计稿 ① 工作台 样式要点 1）：页面最顶部最醒目的元素 ——
    // --teal-soft 底 + 4px 左边框，按 健康/异常/不可用 三色切换（--success / --warn / --error）。
    // 覆盖率与采集健康页**同口径**（分母 = summary.checked 可核对目标数）；checked=0 显示「不适用」，
    // 绝不画一根 100% 的假健康条——数据同源，下钻代替复制（规则 4）。
    var ovChk = summary.checked || 0, ovStale = summary.stale || 0, ovWild = summary.wild || 0;
    var ovVmOk = summary.vm_reachable !== false;
    // 待办三类与左侧导航「采集健康」badge 同源：computeTodos 一处计算、两处消费（规则 4 / 样式要点 2）
    var ovTodos = (typeof window.computeTodos === 'function')
      ? window.computeTodos(d.recon, d)
      : { drafts: ovStale, approvals: 0, blocked: 0, total: ovStale };
    var ovVerdict, ovColor, ovSoft;
    if (!ovVmOk || d.err) {
      ovVerdict = '✕ 采集实况不可用：VM 不可达，无法判定谁在报、谁断了';
      ovColor = 'var(--error)'; ovSoft = 'var(--red-soft)';
    } else if (ovTodos.total > 0) {
      ovVerdict = '⚠ 有 ' + ovTodos.total + ' 项在等你：' + ovTodos.drafts + ' 份修复草案待复核 · '
        + ovTodos.approvals + ' 笔变更待批准 · ' + ovTodos.blocked + ' 个任务受阻';
      ovColor = 'var(--warn)'; ovSoft = 'var(--amber-soft)';
    } else {
      ovVerdict = '✅ 平台健康：已核对 ' + ovChk + ' 个目标全部在报，VM 无野指标，Agent 无异常';
      ovColor = 'var(--success)'; ovSoft = 'var(--teal-soft)';
    }
    var ovCover = ovChk > 0
      ? ('覆盖率 ' + Math.round((ovChk - ovStale) / ovChk * 100) + '%（可核验 ' + (ovChk - ovStale) + '/' + ovChk + '）')
      : ('覆盖率 —（' + (ovVmOk ? '无可核对目标，不适用' : 'VM 不可达，不可判') + '）');
    h += '<div style="background:' + ovSoft + ';border-left:4px solid ' + ovColor + ';border-radius:0 8px 8px 0;padding:12px 14px;margin-bottom:12px">'
      + '<div style="font-size:13.5px;font-weight:600;color:' + ovColor + '">' + ovEsc(ovVerdict) + '</div>'
      + '<div style="font-size:11.5px;color:var(--muted);margin-top:5px;display:flex;gap:14px;flex-wrap:wrap">'
      + '<span>对账引擎' + (d.recon && d.recon.generated_at ? ' · ' + ovEsc(d.recon.generated_at) : ' · 时间未回传') + '</span>'
      + '<span>' + ovEsc(ovCover) + '</span></div></div>';

    // 证据链概要（共享实现见 buildEvidenceChainHTML，与采集健康页同源，口径唯一）
    h += buildEvidenceChainHTML(d);

    // 平台健康条（消费 /api/health，架构 3.3 可运维·可观测）：L0 控制面/目录/Agent心跳/VM链/对账 的整体健康，
    // 与证据链（数据面"是否在报"）互补——这一条追"平台自身好不好"，逐组件可悬停看细节。
    h += '<div id="platform-health" style="margin-top:10px;display:flex;align-items:center;gap:10px;flex-wrap:wrap;font-size:12px"></div>';

    h += '<div class="slo-grid">'
      + ovCard(nOk, '采集中', '--success', 'resources', 'collecting')
      + ovCard(nBad, '异常', nBad ? '--error' : '--success', 'resources', 'problem')
      + ovCard(nOff, '未接入', '--muted', 'resources', 'off')
      + ovCard(pending.length, '待处理流水线', pending.length ? '--warning,--warn' : '--success', 'onboard-center', '')
      + '</div>';
    if (d.err) h += '<div style="margin-top:8px;font-size:12px;color:var(--warn)">⚠ ' + ovEsc(d.err) + '</div>';
    h += '<div style="margin-top:8px;font-size:11px;color:var(--muted)">'
      + '台账声明应采目标 ' + (summary.targets || 0) + ' 个 · 断采 ' + (summary.stale || 0) + ' 个 · 野指标 ' + (summary.wild || 0) + ' 条'
      + (summary.vm_reachable === false ? ' · <span style="color:var(--error)">VM 不可达，采集实况取不到</span>' : '')
      + '</div>';
    // 现场发布结论（前端效果设计·第2节"上下文助手"：用真实数据做自动化巡检，给"当前最要紧的事"，不调模型不造假）
    h += '<div class="card" style="margin-top:12px"><div class="card-hd">🪄 现场解读 <span style="font-weight:400;font-size:11px;color:var(--muted)">由本页台账×心跳×对账×入库实测自动推导，追的是"现在最要紧的事"</span></div><div class="card-bd">';
    h += '<div id="ctx-assist" style="min-height:20px"></div></div></div>';
    setTimeout(function() { renderCtxAssist(d); }, 0); // 数据已在 d 内，下一帧同步渲染（无异步依赖）
    h += '';

    // 待办（IA 设计稿 ① 工作台 界面效果）：分三类 chip —— 未复核草案 / 待批准变更 / 受阻任务。
    // 计数与左侧导航「采集健康」badge 同源（computeTodos 一处计算、两处消费——规则 4）。
    // 本页不处理：chip 只跳对应页，明细按钮进的是变更过程视图（看，不是改）。
    var ovChip = function (n, label, cls) {
      return '<span class="badge ' + cls + '" style="cursor:pointer;font-size:12px;padding:4px 11px"'
        + ' onclick="goPage(\'changes\')" title="跳变更中心处理">' + n + ' ' + label + '</span>';
    };
    h += '<div class="card" style="margin-top:12px"><div class="card-hd">⚠️ 待办（' + ovTodos.total + '）'
      + '<span style="font-weight:400;font-size:11px;color:var(--muted)">未复核草案 / 待批准变更 / 受阻任务——点击跳对应页处理，本页不处理</span></div><div class="card-bd">';
    if (!ovTodos.total) {
      h += '<div style="text-align:center;color:var(--muted);padding:16px">✅ 没有等待你的事</div>';
    } else {
      // 颜色口径：待复核/待批准＝amber（等你决策），受阻＝red（等你救火）
      h += '<div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:4px">'
        + ovChip(ovTodos.drafts, '份修复草案待复核', 'b-s')
        + ovChip(ovTodos.approvals, '笔变更待批准', 'b-s')
        + ovChip(ovTodos.blocked, '个任务受阻', 'b-o')
        + '</div>';
      if (pending.length) {
        h += '<div style="margin-top:12px;font-size:11px;color:var(--muted)">明细（只读：点进去看过程，动作在变更中心发生）</div>';
        h += '<table style="margin-top:6px"><thead><tr><th style="width:180px">资源</th><th style="width:150px">操作</th><th>当前环节</th><th style="width:110px">状态</th><th style="width:150px"></th></tr></thead><tbody>';
        pending.forEach(function (f) {
          h += '<tr><td><b>' + ovEsc(f.resource_id) + '</b><div style="font-size:11px;color:var(--muted)">' + ovEsc(f.resource_ip || '') + '</div></td>'
            + '<td style="font-size:12px">' + ovEsc(f.mode_name || f.mode || '') + '</td>'
            + '<td style="font-size:12px">' + ovEsc(f.current_title || f.current_step || '—')
            + (f.warn_count ? ' <span class="ob-warn">⚠ ' + f.warn_count + ' 项待复核</span>' : '') + '</td>'
            + '<td>' + (typeof obBadge === 'function' ? obBadge(f.status, f.wait_kind) : ovEsc(f.status)) + '</td>'
            + '<td><button class="btn btn-o btn-sm" onclick="openOnboardFlow(' + f.id + ')">看变更过程</button></td></tr>';
        });
        h += '</tbody></table>';
      }
    }
    h += '</div></div>';

    // 最近事件：流水线时间线（成功与失败都在，回答"刚发生了什么"）
    h += '<div class="card" style="margin-top:12px"><div class="card-hd">🕒 最近事件 <button class="btn btn-o btn-sm" style="float:right" onclick="goPage(\'onboard-center\')">全部流水线 →</button></div><div class="card-bd">';
    var recent = flows.slice(0, 8);
    if (!recent.length) {
      h += '<div style="text-align:center;color:var(--muted);padding:16px">还没有流水线记录</div>';
    } else {
      recent.forEach(function (f) {
        var cls = f.status === 'done' ? 'res-ev ok' : (f.status === 'failed' ? 'res-ev fail' : (f.status === 'running' ? 'res-ev run' : 'res-ev wait'));
        var icon = f.status === 'done' ? '✓' : (f.status === 'failed' ? '✕' : (f.status === 'running' ? '◐' : '⏸'));
        h += '<div class="' + cls + '" style="cursor:pointer" onclick="openOnboardFlow(' + f.id + ')"><span>' + icon + '</span><span class="t">'
          + '<b style="font-weight:500">' + ovEsc(f.resource_id) + '</b> <span class="m">' + ovEsc(f.mode_name || f.mode || '') + ' · ' + ovEsc(f.current_title || '') + '</span>'
          + '<div class="m">' + ovEsc(f.updated_at || f.started_at || '') + '</div></span></div>';
      });
    }
    h += '</div></div>';

    h += '<div style="margin-top:12px;display:flex;gap:8px;flex-wrap:wrap">'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'resources\')">打开接入中心</button>'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'slo\')">对账详情（断采 / 野指标 / 异常 Agent）</button>'
      + '<button class="btn btn-p btn-sm" onclick="goPage(\'metrics-browse\')">📈 指标大屏</button>'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'targets\')">采集目标</button>'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'audit\')">审计日志</button>'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'tasks\')">任务历史</button></div>';
    mc.innerHTML = h;
    renderPlatformHealth();
  });
}

// 平台健康条渲染：消费 /api/health，聚合 L0 控制面/目录/Agent心跳/VM链/对账 的整体健康。
// 失败降级为"未能获取"一行，不打扰页面其余内容（可观测性自身不因探测失败失灵）。
function renderPlatformHealth() {
  var el = document.getElementById('platform-health');
  if (!el) return;
  fetch(API + '/health').then(function (r) { return r.json() }).catch(function () { return null }).then(function (hp) {
    if (!hp || !hp.components) { el.innerHTML = '<span style="color:var(--muted)">🩺 平台健康：未能获取</span>'; return; }
    var st = hp.status, stName = st === 'ok' ? '平台健康' : (st === 'degraded' ? '部分异常' : '故障');
    var stColor = st === 'ok' ? 'var(--success)' : (st === 'degraded' ? 'var(--warning,#d97706)' : 'var(--error)');
    var dots = (hp.components || []).map(function (c) {
      var ck = c.status === 'ok' ? 'var(--success)' : (c.status === 'degraded' ? 'var(--warning,#d97706)' : 'var(--error)');
      var gly = c.status === 'ok' ? '●' : (c.status === 'degraded' ? '◐' : '●');
      return '<span title="' + ovEsc(c.name) + '：' + ovEsc(c.detail || '') + '" style="color:' + ck + '">' + gly + ' ' + ovEsc(c.name) + '</span>';
    }).join('');
    var sum = hp.summary || {};
    var onlineTxt = (sum.agents && (sum.online === undefined)) ? '' : (' Agent ' + (sum.online || 0) + '/' + (sum.agents || 0) + ' 在线');
    el.innerHTML = '<span style="font-weight:600;color:' + stColor + '">🩺 ' + stName + '</span>'
      + '<span style="color:var(--muted)">' + onlineTxt + '</span>' + dots
      + (hp.generated_at ? '<span style="font-size:11px;color:var(--muted)">· ' + ovEsc(hp.generated_at) + '</span>' : '');
  });
}

// 证据链概要条（IA 设计稿·采集健康页语义核心：六环证据链，每环独立状态）。
// 共享实现：工作台(overview)与采集健康(slo)同源复用，口径唯一（裁决 3）。
// 数据全部来自真实后端，缺失即标「未知」，绝不硬编码填绿（unknown ≠ failed）。
//   E1 对象连接  ← Agent 心跳在线占比（last_seen 距当前 ≤ HEARTBEAT_TTL）
//   E2 配置版本  ← Agent 生效配置与期望配置一致（cfg_effective == cfg_desired）
//   E3 源采集    ← 台账应采目标中非断采占比（recon.summary）
//   E4 区域抓取  ← collect-stats 有真实采集采样（抓取端在出数）
//   E5 区域传输  ← VM 可达（recon vm_reachable，链路 upstream 通不通）
//   E6 中央入库  ← 抓取有采样 且 传输通（数据确已落库）
// 逐环归因：缺口环可点击下钻——Agent 侧→资源页"问题"筛选；数据侧→采集健康对账。
globalThis.buildEvidenceChainHTML = function (d) {
  var esc = (typeof ovEsc === 'function') ? ovEsc : function (s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  };
  var agentsArr = (d.agents || []).filter(function (a) { return window.tenantVisible(a); }); // D3：证据链按当前租户隔离
  var now = Math.floor(Date.now() / 1000);
  var HEARTBEAT_TTL = 120; // 心跳新鲜度阈值(秒)：last_seen 距今超过该值视为 Agent 离线
  var RECON_TTL = 240;     // 对账证据有效期(秒)：超过视为对账停摆，证据降级为未知
  var STATS_TTL = 300;     // 采样证据有效期(秒)：与采集健康"近 5 分钟有样本"口径一致
  // 证据时间解析：支持 epoch 秒与 'YYYY-MM-DD HH:MM:SS'（本地时区）两种来源
  function toSec(v) {
    if (v == null || v === '') return 0;
    if (typeof v === 'number') return v;
    var n = parseInt(v, 10);
    if (String(v).indexOf('-') < 0 && isFinite(n) && n > 1e9) return n;
    var t = new Date(String(v).replace(' ', 'T')).getTime();
    return isFinite(t) ? Math.floor(t / 1000) : 0;
  }
  function hm(sec) {
    if (!sec) return '';
    try { return new Date(sec * 1000).toLocaleTimeString('zh-CN', { hour12: false }); } catch (e) { return ''; }
  }
  function expired(until) { return until > 0 && now > until; }
  var alive = agentsArr.filter(function (a) {
    var ls = a && a.last_seen;
    return ls && (now - ls) <= HEARTBEAT_TTL;
  }).length;
  var e1Ok = agentsArr.length ? (alive / agentsArr.length >= 0.6) : null;
  var sm = (d.recon && d.recon.summary) || {};
  var targetsN = sm.targets || 0, staleN = sm.stale || 0;
  var e3Ok = targetsN ? ((targetsN - staleN) / targetsN >= 0.6) : null;
  var vmOk = sm.vm_reachable === true;
  var statsN = Object.keys(d.stats || {}).length;
  var e4Ok = statsN > 0 ? true : null; // 无采样证据时标未知，不硬编码
  // 配置版本环：生效配置与期望配置一致（revisions 对齐）；字段缺失时降级为未知
  var cfgOk = 0, cfgBad = 0, cfgKnown = 0;
  agentsArr.forEach(function (a) {
    if (a.cfg_desired !== undefined && a.cfg_effective !== undefined && a.cfg_desired !== null && a.cfg_effective !== null) {
      cfgKnown++;
      if (String(a.cfg_desired) === String(a.cfg_effective)) cfgOk++; else cfgBad++;
    }
  });
  var e2Ok = cfgKnown ? (cfgBad === 0) : null;
  var e2Detail = cfgKnown
    ? (cfgBad ? cfgOk + '/' + cfgKnown + ' 配置对齐' : cfgKnown + ' 个配置全部对齐')
    : '配置版本证据待接入';
  var e5Ok = statsN > 0 && vmOk;
  // ---- 证据时效（IA 设计稿：每环显示 状态 · 证据时间 · 有效至；过期证据自动降级为「未知」）----
  var atHeartbeat = 0;
  agentsArr.forEach(function (a) { var ls = toSec(a && a.last_seen); if (ls > atHeartbeat) atHeartbeat = ls; });
  var atRecon = toSec(d.recon && (d.recon.generated_at || d.recon.generated_at_ts));
  var minAge = null;
  Object.keys(d.stats || {}).forEach(function (k) {
    var s = d.stats[k];
    if (!s || s.age_sec == null || s.age_sec < 0) return;
    if (minAge === null || s.age_sec < minAge) minAge = s.age_sec;
  });
  var atStats = minAge === null ? 0 : (now - minAge);
  function untilOf(at, ttl) { return at ? at + ttl : 0; }
  // 证据过期 → 明确状态降级为「未知」（不是失败，也不是健康）
  function degrade(ok, until) { return (ok === true && expired(until)) ? null : ok; }
  var u1 = untilOf(atHeartbeat, HEARTBEAT_TTL), u3 = untilOf(atRecon, RECON_TTL),
      u4 = untilOf(atStats, STATS_TTL), u5 = untilOf(atRecon, RECON_TTL);
  var d1 = degrade(e1Ok, u1), d3 = degrade(e3Ok, u3), d4 = degrade(e4Ok, u4),
      d5 = degrade(vmOk, u5), d6 = degrade(e5Ok, Math.min(u4 || Infinity, u5 || Infinity));
  var chain = [
    { k: 'heartbeat', label: '① 对象连接', ok: d1, detail: agentsArr.length ? alive + ' / ' + agentsArr.length + ' 在线(≤' + HEARTBEAT_TTL + 's)' : '无 Agent', at: atHeartbeat, until: u1 },
    { k: 'config', label: '② 配置版本', ok: e2Ok, detail: e2Detail, at: 0, until: 0 },
    { k: 'source', label: '③ 源采集', ok: d3, detail: '应采 ' + targetsN + ' · 断采 ' + staleN, at: atRecon, until: u3 },
    { k: 'scrape', label: '④ 区域抓取', ok: d4, detail: d4 ? statsN + ' 个目标有采集采样' : (statsN ? '采样证据已过期' : '采样证据待接入'), at: atStats, until: u4 },
    { k: 'transport', label: '⑤ 区域传输', ok: d5, detail: d5 ? 'VM 可达' : (vmOk ? 'VM 可达但证据过期' : 'VM 无响应'), at: atRecon, until: u5 },
    { k: 'central', label: '⑥ 中央入库', ok: d6, detail: d6 ? '稳定采集中' : (e5Ok ? '入库证据已过期' : '待确认采样'), at: Math.max(atStats, 0), until: Math.min(u4 || 0, u5 || 0) }
  ];
  var h = '<div class="ev-chain">';
  chain.forEach(function (c) {
    var st = c.ok === true ? 'ev-ok' : (c.ok === false ? 'ev-bad' : 'ev-unknown');
    var ic = c.ok === true ? '✓' : (c.ok === false ? '✕' : '·');
    // 只有有缺口的节点可点（全绿节点点击无意义，也不误导）。
    // 归因目标：Agent 侧(heartbeat/config)→资源页"问题"筛选；
    // 数据侧(source/scrape/transport/central)→若在采集健康页由 __sloDrill 就地定位问题卡，否则跳 slo 页。
    var jt = null;
    if (c.ok === false) {
      if (c.k === 'heartbeat') {
        jt = "resSetFilter('problem');goPage('resources')";
      } else if (c.k === 'config') {
        // A5（2026-09-27）：配置漂移下钻走专用「配置漂移」筛选（cfg_desired≠cfg_effective），
        // 不再混入「异常」——归因错位曾导致环②报 0/4 漂移、下钻却只出 1 台采集问题的机器。
        jt = "resSetFilter('cfgdrift');goPage('resources')";
      } else {
        // 数据侧缺口：优先就地钻取（本页存在对应问题卡时），否则回退跳采集健康页
        var drillFn = 'window.__sloDrill';
        var kindArg = c.k === 'source' ? "'stale'" : (c.k === 'transport' ? "'transport'" : (c.k === 'scrape' ? "'stale'" : "'wild'"));
        jt = "if(" + drillFn + ")" + drillFn + "(" + kindArg + "); else goPage('slo')";
      }
    }
    var click = jt ? ' style="cursor:pointer" onclick="' + jt + '"' : '';
    // 证据时效小字：过期显式标注「已过期 → 未知」，不静默降级
    var tsTxt;
    if (!c.at) tsTxt = '证据待接入';
    else {
      tsTxt = '证据 ' + hm(c.at) + (c.until ? ' · 有效至 ' + hm(c.until) : '');
      if (c.until && expired(c.until)) tsTxt += ' · 已过期 → 未知';
    }
    h += '<div class="ev-node ' + st + '"' + click + ' title="' + esc(c.detail) + ' · ' + esc(tsTxt) + (jt ? ' · 点击逐环归因' : '') + '">'
      + '<span class="ev-ic">' + ic + '</span><span class="ev-lb">' + esc(c.label) + '</span><span class="ev-dt">' + esc(c.detail) + '</span>'
      + '<span class="ev-ts">' + esc(tsTxt) + '</span></div>';
  });
  var gateOk = d1 !== false && e2Ok !== false && d3 !== false && d4 !== false && d5 !== false && d6 !== false;
  h += '<div class="ev-node ev-gate" title="已应用 ≠ 已入库，六环全绿方为正常">' + (gateOk ? '采集正常 ✓' : '有缺口，逐环归因 →') + '</div></div>';
  if (!gateOk) {
    h += '<div style="margin-top:-6px;margin-bottom:14px;font-size:11.5px;color:var(--muted)">'
      + '🔼 红色/缺口环可点击，快速下钻归因：Agent 侧问题→资源页"问题"，数据侧问题→采集健康对账。'
      + ' <span style="color:var(--warn)">"已应用 ≠ 已入库"</span>，请逐环核验；灰点环=证据缺失或已过期（降级为未知，不是失败）。</div>';
  }
  return h;
};

// 现场解读（前端效果设计·第2节"上下文助手"）：不调模型、不造假，纯规则模板 + 真实数据推导结论。
// 输出"当前最要紧的事"优先级清单：每个结论项带真实来源与可点击下钻动作。
function renderCtxAssist(d) {
  var el = document.getElementById('ctx-assist');
  if (!el) return;
  var now = Math.floor(Date.now() / 1000), TTL = 120;
  var arr = (d.agents || []).filter(function (a) { return window.tenantVisible(a); }); // D3：现场解读按当前租户隔离
  var alive = arr.filter(function (a) { var ls = a && a.last_seen; return ls && (now - ls) <= TTL; }).length;
  var sm = (d.recon && d.recon.summary) || {};
  var targetsN = sm.targets || 0, staleN = sm.stale || 0, wildN = sm.wild || 0;
  var vmOk = sm.vm_reachable === true;
  var statsN = Object.keys(d.stats || {}).length;
  var statInfo = statsN > 0; // 有真实入库采样
  var pendingN = (d.flows || []).filter(function (f) { return f.status === 'running' || f.status === 'blocked' || f.status === 'stalled' || f.status === 'failed'; }).length;

  var items = []; // {sev:0..3, icon, text, action(可选跳转)}
  // 最高优先：链路基础不达标
  if (vmOk === false) items.push({ sev: 3, icon: '🔴', text: 'VictoriaMetrics 不可达，链路已断——所有"断采/野指标/入库"判断都不可信，先恢复存储', act: "goPage('metrics-browse')" });
  // Agent 侧
  if (arr.length && alive === 0) items.push({ sev: 3, icon: '🔴', text: '所有 Agent 心跳全失联（' + alive + '/' + arr.length + '），疑似网络或 Agent 大面积故障', act: "goPage('overview')" });
  else if (arr.length && alive < arr.length * 0.6) items.push({ sev: 2, icon: '🟠', text: 'Agent 心跳偏少（' + alive + '/' + arr.length + ' 在线），部分主机可能离线', act: "resSetFilter('problem');goPage('resources')" });
  // 配置/对账
  if (staleN > 0) items.push({ sev: 2, icon: '🟠', text: '有 ' + staleN + ' 个目标断采超过 10 分钟，数据"应采未入库"', act: 'if(window.__sloDrill)__sloDrill("stale");else goPage("slo")' });
  if (wildN > 0) items.push({ sev: 1, icon: '🟡', text: '发现 ' + wildN + ' 条野指标（VM 有 / 应报口径无），需核对来源', act: 'if(window.__sloDrill)__sloDrill("wild");else goPage("slo")' });
  // 待处理流水线
  if (pendingN > 0) items.push({ sev: 1, icon: '🟡', text: '有 ' + pendingN + ' 条流水线待处理（停等/卡住/失败），需要决策', act: "goPage('onboard-center')" });
  // 正常结论
  if (!items.length) items.push({ sev: 0, icon: '✅', text: '现场无异常待办：心跳、配置、源采集、链路、入库五段均健康，' + (targetsN || 0) + ' 个目标按口径在报' });

  // 状态卡：无缺口→绿，轻微→琥珀，严重→红
  var worst = Math.max.apply(null, items.map(function (i) { return i.sev; }));
  var st = worst === 0 ? 'ev-ok' : (worst >= 3 ? 'ev-bad' : 'ev-unknown');
  var html = '<div class="ev-node ' + st + '" style="max-width:100%;margin-bottom:6px">'
    + '<span class="ev-lb" style="color:inherit">系统巡检 · ' + (worst === 0 ? '正常' : (worst >= 3 ? '需立即处理' : '有事项待办')) + '</span></div>';
  items.forEach(function (i) {
    var click = i.act ? ' style="cursor:pointer;color:var(--primary)" onclick="' + i.act + '"' : '';
    html += '<div style="display:flex;align-items:baseline;gap:8px;padding:5px 0;border-bottom:1px solid var(--border);font-size:12.5px">'
      + '<span>' + i.icon + '</span><span style="flex:1">' + ovEsc(i.text) + '</span>'
      + (i.act ? '<span' + click + ' style="white-space:nowrap;font-size:11.5px;color:var(--primary);cursor:pointer">去处理 →</span>' : '') + '</div>';
  });
  el.innerHTML = html;
}

function ovEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function ovCard(n, label, color, page, filter) {
  var click = page ? ' style="cursor:pointer" onclick="ovDrill(\'' + page + '\',\'' + filter + '\')"' : '';
  return '<div class="slo-card"' + click + '><div class="slo-val" style="color:var(' + color + ')">' + n + '</div>'
    + '<div class="slo-label">' + label + '</div></div>';
}

function ovDrill(page, filter) {
  if (filter && typeof resSetFilter === 'function') resSetFilter(filter);
  goPage(page);
}
