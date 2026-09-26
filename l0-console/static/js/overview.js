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

    h += '<div class="card"><div class="card-hd">🩺 采集总览'
      + '<span style="font-weight:400;font-size:11px;color:var(--muted)">　' + (d.recon && d.recon.generated_at ? '对账时间 ' + ovEsc(d.recon.generated_at) : '') + '</span>'
      + '<button class="btn btn-o btn-sm" style="float:right" onclick="renderHealthOverview()">↻ 刷新</button></div><div class="card-bd">';

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

    // 待处理：必须有人动手的事（停等的决策、卡住的、失败的）
    h += '<div class="card" style="margin-top:12px"><div class="card-hd">⚠️ 待处理 <span style="font-weight:400;font-size:11px;color:var(--muted)">停等人工决策 / 卡住 / 失败的流水线——这些是"我该做什么"</span></div><div class="card-bd">';
    if (!pending.length) {
      h += '<div style="text-align:center;color:var(--muted);padding:16px">✅ 没有待处理的流水线</div>';
    } else {
      h += '<table><thead><tr><th style="width:180px">资源</th><th style="width:150px">操作</th><th>当前环节</th><th style="width:110px">状态</th><th style="width:150px"></th></tr></thead><tbody>';
      pending.forEach(function (f) {
        h += '<tr><td><b>' + ovEsc(f.resource_id) + '</b><div style="font-size:11px;color:var(--muted)">' + ovEsc(f.resource_ip || '') + '</div></td>'
          + '<td style="font-size:12px">' + ovEsc(f.mode_name || f.mode || '') + '</td>'
          + '<td style="font-size:12px">' + ovEsc(f.current_title || f.current_step || '—')
          + (f.warn_count ? ' <span class="ob-warn">⚠ ' + f.warn_count + ' 项待复核</span>' : '') + '</td>'
          + '<td>' + (typeof obBadge === 'function' ? obBadge(f.status, f.wait_kind) : ovEsc(f.status)) + '</td>'
          + '<td><button class="btn btn-o btn-sm" onclick="openOnboardFlow(' + f.id + ')">去处理</button></td></tr>';
      });
      h += '</tbody></table>';
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
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'resources\')">打开资源页</button>'
      + '<button class="btn btn-o btn-sm" onclick="goPage(\'slo\')">对账详情（断采 / 野指标 / 异常 Agent）</button>'
      + '<button class="btn btn-p btn-sm" onclick="goPage(\'metrics-browse\')">📈 分析大屏</button>'
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

// 证据链概要条（前端效果设计·第4节：已应用 ≠ 已入库，五段全绿才叫「采集正常」）。
// 共享实现：工作台(overview)与采集健康(slo)同源复用，口径唯一。
// 数据全部来自真实后端（resFetchAll 的 d：agents/recon/collect-stats），缺失即标「未知」，绝不硬编码填绿。
//   E1 heartbeat ← Agent 心跳在线占比（d.agents 的 last_seen 距当前 ≤ HEARTBEAT_TTL 算在线）
//   E2 config    ← 台账目标中在线 Agent 覆盖占比（Agent 已装上就位才算配置实际生效）
//   E3 source    ← 台账应采目标中非断采占比（recon.summary.targets - stale）
//   E4 transport ← VM 可达（recon.summary.vm_reachable），指标链路 upstream 通不通
//   E5 central   ← collect-stats 有真实入库采样 且 transport 通过（数据确已落库）
// 逐段归因：缺口节点可点击下钻——Agent 侧(heartbeat/config)→资源页"问题"筛选；数据侧(source/transport/central)→采集健康对账。
globalThis.buildEvidenceChainHTML = function (d) {
  var esc = (typeof ovEsc === 'function') ? ovEsc : function (s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  };
  var agentsArr = (d.agents || []).filter(function (a) { return window.tenantVisible(a); }); // D3：证据链按当前租户隔离
  var now = Math.floor(Date.now() / 1000);
  var HEARTBEAT_TTL = 120; // 心跳新鲜度阈值(秒)：last_seen 距今超过该值视为 Agent 离线
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
  var e5Ok = (statsN > 0) && vmOk;
  var e2Ok = targetsN ? (alive >= targetsN * 0.6) : null;

  var chain = [
    { k: 'heartbeat', label: 'Agent 心跳', ok: e1Ok, detail: agentsArr.length ? alive + ' / ' + agentsArr.length + ' 在线(≤' + HEARTBEAT_TTL + 's)' : '无 Agent' },
    { k: 'config', label: '配置生效', ok: e2Ok, detail: '台账目标 ' + targetsN + ' · 在线 ' + alive },
    { k: 'source', label: '源采集', ok: e3Ok, detail: '应采 ' + targetsN + ' · 断采 ' + staleN },
    { k: 'transport', label: '链路贯通', ok: vmOk, detail: vmOk ? 'VM 可达' : 'VM 无响应' },
    { k: 'central', label: '中央入库', ok: e5Ok, detail: e5Ok ? '稳定采集中' : '待确认采样' }
  ];
  var h = '<div class="ev-chain">';
  chain.forEach(function (c) {
    var st = c.ok === true ? 'ev-ok' : (c.ok === false ? 'ev-bad' : 'ev-unknown');
    var ic = c.ok === true ? '✓' : (c.ok === false ? '✕' : '·');
    // 只有有缺口的节点可点（全绿节点点击无意义，也不误导）。
    // 归因目标：Agent 侧(heartbeat/config)→资源页"问题"筛选；
    // 数据侧(source/transport/central)→若在采集健康页由 __sloDrill 就地定位问题卡，否则跳 slo 页。
    var jt = null;
    if (c.ok === false) {
      if (c.k === 'heartbeat' || c.k === 'config') {
        jt = "resSetFilter('problem');goPage('resources')";
      } else {
        // 数据侧缺口：优先就地钻取（本页存在对应问题卡时），否则回退跳采集健康页
        var drillFn = 'window.__sloDrill';
        var kindArg = c.k === 'source' ? "'stale'" : (c.k === 'transport' ? "'transport'" : "'wild'");
        jt = "if(" + drillFn + ")" + drillFn + "(" + kindArg + "); else goPage('slo')";
      }
    }
    var click = jt ? ' style="cursor:pointer" onclick="' + jt + '"' : '';
    h += '<div class="ev-node ' + st + '"' + click + ' title="' + esc(c.detail) + (jt ? ' · 点击逐段归因' : '') + '">'
      + '<span class="ev-ic">' + ic + '</span><span class="ev-lb">' + esc(c.label) + '</span><span class="ev-dt">' + esc(c.detail) + '</span></div>';
  });
  var gateOk = e1Ok !== false && e2Ok !== false && e3Ok !== false && vmOk && e5Ok !== false;
  h += '<div class="ev-node ev-gate" title="已应用 ≠ 已入库，五段全绿方为正常">' + (gateOk ? '采集正常 ✓' : '有缺口，逐段归因 →') + '</div></div>';
  if (!gateOk) {
    h += '<div style="margin-top:-6px;margin-bottom:14px;font-size:11.5px;color:var(--muted)">'
      + '🔼 红色/缺口节点可点击，快速下钻归因：Agent 侧问题→资源页"问题"，数据侧问题→采集健康对账。'
      + ' <span style="color:var(--warn)">"已应用 ≠ 已入库"</span>，请逐段核验。</div>';
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
