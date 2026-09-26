// 资源页（主战场）+ 机器详情右侧抽屉。
//
// 设计口径（2026-09-22 用户评审，图 2/图 3）：
//   一台机器一行 —— 身份 · 实测档案 · Agent 版本 · 采集状态 · 最近数据 · 动作，
//   一屏回答"哪些机器在采、哪些有问题、我该做什么"。升级/停用/卸载就在行上，
//   不再跳三个页面；详情是右侧抽屉，不换页。
//
// 数据全部来自平台自己的接口（台账/心跳/流水线/对账/VM 实测），前端不做业务推断：
//   /api/resources     台账（含实测档案 probe_json、安装 tag）
//   /api/agents        心跳（Agent 活着吗、跑着哪些插件）
//   /api/onboard/flows 流水线（最近一次接入/升级/卸载的结果）
//   /api/recon         对账（断采目标）
//   /api/collect-stats VM 实测（在报多少条指标、最近一条样本多久之前）
//   /api/versions      版本清单（是否可升级）
(function () {
  var _resData = { resources: [], agents: [], flows: [], recon: null, stats: {}, versions: [], err: '' };
  var _resFilter = 'all';
  var _resQuery = '';
  var _drawerKey = null;

  function rEsc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  // ---------- 状态判定（与后端语义一一对应，前端只做映射，不猜） ----------
  // collecting 采集中 / lagging 数据延迟 / nodata 无数据 / stopped 已停 / off 未接入
  function resState(agent, rec, stat) {
    if (!agent) return { key: 'off', label: '未接入', cls: 'res-st-off' };
    if (agent.status === 'stopped' || rec.svc_state === 'stopped') return { key: 'stopped', label: '已停用', cls: 'res-st-stop' };
    if (!stat) return { key: 'nodata', label: '无数据', cls: 'res-st-bad' };
    if (!stat.reporting) return { key: 'nodata', label: '无数据', cls: 'res-st-bad' };
    if (stat.age_sec >= 0 && stat.age_sec > 60) return { key: 'lagging', label: '数据延迟', cls: 'res-st-warn' };
    if (agent.status === 'offline') return { key: 'lagging', label: '心跳中断', cls: 'res-st-warn' };
    return { key: 'collecting', label: '采集中', cls: 'res-st-ok' };
  }

  function resProblem(st) { return st.key === 'lagging' || st.key === 'nodata' || st.key === 'stopped'; }

  // ---------- 行装配：三份数据按 id 对齐；台账没有的 Agent（演示容器）单独成行 ----------
  function resBuildRows(d) {
    var agentByID = {}, flowsByRes = {}, staleByRes = {};
    (d.agents || []).forEach(function (a) { agentByID[a.id] = a; });
    // 一台机器的**每一次**过程都留着：按资源归组成数组（id 倒序），不再只留最新一条。
    // 只留最新一条的后果是真实踩到的缺陷——「已接入成功」的机器后来跑过卸载/启停，
    // 最新一条变成卸载，接入过程就从资源页彻底进不去了（2026-09-24 用户实测：
    // "为什么已接入成功，看不到接入当时的整体过程数据"）
    (d.flows || []).forEach(function (f) {
      var arr = flowsByRes[f.resource_id] || (flowsByRes[f.resource_id] = []);
      arr.push(f);
    });
    for (var rk in flowsByRes) {
      flowsByRes[rk].sort(function (x, y) { return (y.id || 0) - (x.id || 0); });
    }
    ((d.recon && d.recon.stale) || []).forEach(function (s) { if (s.agent_id) staleByRes[s.agent_id] = s; });

    var rows = [], seen = {};
    (d.resources || []).forEach(function (r) {
      var st = d.stats[r.id] || d.stats[r.ip + ':19090'];
      var agent = agentByID[r.id] || agentByID[r.ip] || null;
      seen[r.id] = true;
      if (agent) seen[agent.id] = true;
      // 多租户(D3)：资源归属其 Agent 的租户（或在资源自身 tenant 无归属时回退 Agent），
      // 当前租户非"全部"时仅展示可见资源——租户隔离作用于资源页这个主战场
      var scopeKey = agent ? agent : r;
      if (typeof window.tenantVisible === 'function' && !window.tenantVisible(scopeKey)) return;
      rows.push(resMakeRow(r, agent, flowsByRes[r.id] || [], staleByRes[r.id] || staleByRes[r.ip], st));
    });
    // Agent 有、台账没有：本机演示容器等直接接入的目标。不能因为台账里没有就从资源页消失
    (d.agents || []).forEach(function (a) {
      if (seen[a.id]) return;
      if (typeof window.tenantVisible === 'function' && !window.tenantVisible(a)) return; // D3：Agent 租户隔离
      var st = d.stats[a.id] || d.stats[a.host + ':' + a.port];
      rows.push(resMakeRow({ id: a.id, ip: a.host, name: a.name, arch: '', os: '', kernel: '', labels_json: '{}' },
        a, flowsByRes[a.id] || [], staleByRes[a.id], st));
    });
    return rows;
  }

  function resMakeRow(r, agent, flows, stale, stat) {
    flows = flows || [];
    var labels = {};
    try { labels = JSON.parse(r.labels_json || '{}') || {}; } catch (e) { labels = {}; }
    if (agent && agent.labels) { for (var k in agent.labels) { if (!labels[k]) labels[k] = agent.labels[k]; } }
    var facts = {};
    try { facts = JSON.parse(r.probe_json || '{}') || {}; } catch (e) { facts = {}; }
    var st = resState(agent, r, stat);
    return {
      key: r.id, res: r, agent: agent, flow: flows[0] || null, flows: flows, stale: stale, stat: stat,
      labels: labels, facts: facts, state: st,
      up: resUpgradeTarget(r.installed_tag, r.arch || facts.arch, _resData.versions),
      alt: resAltTarget(r.installed_tag, r.arch || facts.arch, _resData.versions),
      search: [r.id, r.ip, r.name, labels.business_system, labels.env, labels.idc, agent ? agent.id : '']
        .filter(Boolean).join(' ').toLowerCase()
    };
  }

  // ---------- 版本比较（版本清单的 version 字段，如 0.4.1-dev） ----------
  function verParts(v) {
    return String(v == null ? '' : v).replace(/^v/, '').split(/[-+]/)[0].split('.')
      .map(function (x) { return parseInt(x, 10) || 0; });
  }
  function verCmp(a, b) {
    var x = verParts(a), y = verParts(b);
    for (var i = 0; i < 3; i++) {
      var d = (x[i] || 0) - (y[i] || 0);
      if (d) return d > 0 ? 1 : -1;
    }
    return 0;
  }
  function hasPre(v) { return /[-+]/.test(String(v == null ? '' : v)); }

  // tagVersion：tag 形如 linux-arm64-0.4.1-dev → 0.4.1-dev
  function tagVersion(tag, arch) {
    var t = String(tag == null ? '' : tag);
    if (!t) return '';
    if (arch && t.indexOf('linux-' + arch + '-') === 0) return t.slice(('linux-' + arch + '-').length);
    var m = t.match(/^[a-z0-9]+-[a-z0-9]+-(.+)$/);
    return m ? m[1] : t;
  }

  // resUpgradeTarget 该机可升级到的目标版本（只认已发布 stable，与自动选版口径一致）；
  // 无 tag 或无更高版本 → null（不显示"可升级"，绝不用 Agent 自报版本推断）
  function resUpgradeTarget(installedTag, arch, versions) {
    var cur = tagVersion(installedTag, arch);
    if (!cur) return null;
    var best = null;
    (versions || []).forEach(function (v) {
      if (!v || v.status !== 'stable') return;
      if (arch && v.arch && v.arch !== arch) return;
      var c = verCmp(v.version, cur);
      var better = c > 0 || (c === 0 && hasPre(cur) && !hasPre(v.version));
      if (!better) return;
      if (!best || verCmp(v.version, best.version) > 0) best = v;
    });
    return best;
  }

  // resAltTarget 该机可变更到的"其它版本"（stable，含降级）——「升级」入口的兜底。
  // 为什么需要它：升级流程本身**允许降级**（upgrade.yaml 用户拍板 2026-09-22），但入口
  // 只认"有更高版本"才显示「升级」；于是当该架构 stable 最高就是已装版本时，界面一个
  // 入口都不给，降级这条路从界面走不通（实测 amd64 机器装在 0.4.1-dev、该架构 stable
  // 最高即它，可降到 0.4.0 却无从发起）。这里给出「同架构、非当前 tag 的最高 stable」，
  // 只作兜底——有更高版本时仍走「升级」入口，语义更准
  function resAltTarget(installedTag, arch, versions) {
    var cur = tagVersion(installedTag, arch);
    if (!cur) return null;
    var best = null;
    (versions || []).forEach(function (v) {
      if (!v || v.status !== 'stable') return;
      if (arch && v.arch && v.arch !== arch) return;
      if (v.tag === installedTag) return;                 // 当前 tag 不算"变更"
      if (!best || verCmp(v.version, best.version) > 0) best = v;
    });
    return best;
  }

  // resVerDelta 目标版本相对当前版本的方向：+1 升 / 0 平 / -1 降（界面据此给不同措辞）
  function resVerDelta(installedTag, arch, tgt) {
    var cur = tagVersion(installedTag, arch);
    if (!cur || !tgt) return 0;
    var c = verCmp(tgt.version, cur);
    if (c > 0) return 1;
    if (c < 0) return -1;
    return hasPre(cur) && !hasPre(tgt.version) ? 1 : 0;
  }

  // ---------- 取数 ----------
  function resFetchAll(cb) {
    var urls = ['/resources', '/agents', '/onboard/flows', '/recon', '/collect-stats', '/versions', '/targets'];
    Promise.all(urls.map(function (u) {
      return fetch(API + u).then(function (r) { return r.json(); }).catch(function () { return null; });
    })).then(function (out) {
      var res = out[0] || {}, agents = out[1] || [], flows = out[2] || {}, recon = out[3],
        stats = out[4] || {}, vers = out[5] || {}, targets = out[6] || [];
      var d = {
        resources: res.resources || [],
        agents: Array.isArray(agents) ? agents : (agents.agents || []),
        flows: flows.flows || [],
        recon: recon || null,
        stats: {},
        versions: vers.versions || [],
        targets: Array.isArray(targets) ? targets : [],
        err: res.error || stats.error || ''
      };
      (stats.resources || []).forEach(function (s) {
        if (s.resource_id) d.stats[s.resource_id] = s;
        if (s.instance) d.stats[s.instance] = s;
      });
      cb(d);
    });
  }

  // ---------- 页面 ----------
  function renderResources() {
    var mc = document.getElementById('main-content');
    mc.innerHTML = '<div class="card"><div class="card-bd" style="text-align:center;color:var(--muted);padding:24px">⏳ 正在汇总资源、Agent、流水线与采集实况…</div></div>';
    resFetchAll(function (d) {
      _resData = d;
      resRenderPage();
    });
  }

  function resRenderPage() {
    var rows = resBuildRows(_resData);
    var nAll = rows.length, nOk = 0, nBad = 0, nOff = 0;
    rows.forEach(function (r) {
      if (r.state.key === 'collecting') nOk++;
      else if (r.state.key === 'off') nOff++;
      else nBad++;
    });

    var h = '<div class="card"><div class="card-hd">🖥 资源与接入'
      + '<span style="font-weight:400;font-size:11px;color:var(--muted)">　一台机器一行 · 采集实况来自 VM 实测</span>';
    if (_resData.err) h += '<span class="ob-warn" style="margin-left:8px;font-weight:400">⚠ ' + rEsc(_resData.err) + '</span>';
    h += '<button class="btn btn-p btn-sm" style="float:right" onclick="openOnboardNew()">+ 接入新资源</button></div><div class="card-bd">';

    h += '<div class="res-filters" style="margin-bottom:10px">'
      + resFilterBtn('all', '全部', nAll)
      + resFilterBtn('collecting', '采集中', nOk)
      + resFilterBtn('problem', '异常', nBad)
      + resFilterBtn('off', '未接入', nOff)
      + '<input id="res-search" placeholder="搜 IP / 资源名 / 业务系统" value="' + rEsc(_resQuery) + '" '
      + 'oninput="resSearch(this.value)" style="margin-left:auto;padding:5px 10px;border:0.5px solid var(--border);border-radius:6px;font-size:12px;width:240px">'
      + '</div>';

    h += '<table><thead><tr><th style="width:210px">资源</th><th style="width:150px">环境</th>'
      + '<th style="width:150px">Agent</th><th style="width:150px">采集状态</th><th style="width:100px">最近数据</th>'
      + '<th style="width:150px">归属租户</th><th style="width:230px"></th></tr></thead><tbody id="res-tbody">' + resRowsHTML(rows) + '</tbody></table>';

    h += '<div class="batch-bar" style="margin-top:12px"><strong style="font-size:13px">批量操作</strong>'
      + '<span style="font-size:11px;color:var(--muted)">按 Agent 的 IDC / 类型批量执行（来自心跳清单，不含未接入机器）</span>'
      + '<select id="batch-scope-type"><option value="idc">IDC</option><option value="type">类型</option></select>'
      + '<select id="batch-scope-val"></select>'
      + '<select id="batch-action"><option value="restart">重启</option><option value="stop">停止</option><option value="start">启动</option></select>'
      + '<button class="btn btn-p" onclick="batchOp()">执行</button></div>';

    h += '<div style="margin-top:8px;font-size:11px;color:var(--muted)">'
      + '「采集中」= VM 近 5 分钟确有该资源样本；「无数据」= 台账有、VM 没有——接入成功≠数据在流，两件事分开看。'
      + (_resData.recon && _resData.recon.generated_at ? '　对账时间 ' + rEsc(_resData.recon.generated_at) : '') + '</div>';
    h += '</div></div>';
    document.getElementById('main-content').innerHTML = h;
    if (typeof updateBatchScope === 'function') {
      updateBatchScope();
      document.getElementById('batch-scope-type').onchange = updateBatchScope;
    }
  }

  // batchOp（panel.js，fleet 页共用）批量执行完后回调：重拉资源页数据，让 Agent 状态立刻可见
  window.resRefreshAfterBatch = function () {
    resFetchAll(function (d) { _resData = d; resRenderPage(); });
  };

  function resFilterBtn(k, label, n) {
    return '<span class="res-f' + (_resFilter === k ? ' on' : '') + '" onclick="resSetFilter(\'' + k + '\')">'
      + rEsc(label) + ' <b>' + n + '</b></span>';
  }

  function resRowsHTML(rows) {
    var out = '', shown = 0;
    rows.forEach(function (r) {
      if (!resMatch(r)) return;
      shown++;
      out += resRowHTML(r);
    });
    if (!shown) return '<tr><td colspan="7" style="text-align:center;color:var(--muted);padding:20px">没有匹配的资源</td></tr>';
    return out;
  }

  function resMatch(r) {
    if (_resQuery && r.search.indexOf(_resQuery.toLowerCase()) < 0) return false;
    if (_resFilter === 'collecting') return r.state.key === 'collecting';
    if (_resFilter === 'problem') return resProblem(r.state);
    if (_resFilter === 'off') return r.state.key === 'off';
    return true;
  }

  function resRowHTML(r) {
    var facts = r.facts || {};
    var envLine = [facts.distribution || r.res.os, facts.arch || r.res.arch, facts.kernel || r.res.kernel]
      .filter(Boolean).map(rEsc).join(' · ');
    var envChips = [r.labels.env, r.labels.idc, r.labels.business_system].filter(Boolean)
      .map(function (t) { return '<span class="res-chip">' + rEsc(t) + '</span>'; }).join('');
    var h = '<tr>';
    h += '<td><b>' + rEsc(r.res.ip || r.res.id) + '</b>'
      + (r.res.name && r.res.name !== r.res.ip ? '<div style="font-size:11px;color:var(--muted)">' + rEsc(r.res.name) + '</div>' : '')
      + (envLine ? '<div style="font-size:11px;color:var(--muted)">' + envLine + '</div>' : '') + '</td>';

    h += '<td style="font-size:12px">' + (envChips || '<span style="color:var(--muted)">—</span>')
      + (r.res.target_kind ? '<div style="font-size:11px;color:var(--muted)">' + rEsc(r.res.target_kind) + ' · SSH ' + rEsc(r.res.ssh_port || 22) + '</div>' : '')
      + '</td>';

    // Agent 列：装的是哪个 tag（台账记录）才是"版本"；Agent 自报版本不作数
    var agCell;
    if (!r.agent) {
      agCell = '<span style="color:var(--muted)">—</span>';
    } else {
      var tag = r.res.installed_tag || '';
      // 版本口径：只有平台装的 tag 才算"这台机装的哪一版"；Agent 自报的是编译常量，
      // 换包不重建就不变，拿它当版本会误导升级判断（2026-09-22 两处口径）
      if (tag) agCell = '<b>' + rEsc(tag) + '</b>';
      else agCell = '<b style="color:var(--muted)" title="平台没有该机的安装记录（演示容器或直接接入的目标）">未登记安装版本</b>';
      if (r.up) agCell += ' <span class="res-chip" style="border-color:#fcd34d;background:#fffbeb;color:#92400e" title="版本清单里有更新的已发布版本：' + rEsc(r.up.tag) + '">可升级 ' + rEsc(r.up.version) + '</span>';
      agCell += '<div style="font-size:11px;color:var(--muted)">' + rEsc(r.agent.type || '') + ' · ' + rEsc(r.agent.status || '')
        + (r.agent.version ? ' · 自报 ' + rEsc(r.agent.version) : '') + '</div>';
    }
    h += '<td>' + agCell + '</td>';

    var stHtml = '<span class="' + r.state.cls + '">' + rEsc(r.state.label) + '</span>';
    if (r.stat && r.stat.reporting) stHtml += ' <span style="font-size:12px">' + r.stat.metrics + ' 项指标</span>';
    if (r.stale) stHtml += ' <span class="res-chip" style="border-color:#fca5a5;color:#991b1b">对账判断采</span>';
    h += '<td>' + stHtml + '</td>';

    h += '<td style="font-size:12px">' + (r.stat && r.stat.age_sec >= 0 ? resAge(r.stat.age_sec) : '<span style="color:var(--muted)">—</span>') + '</td>';

    // 归属租户（架构 D3 收尾）：资源级分配。有效租户 = 资源自身 tenant_id → 回退其 Agent 租户 → default。
    // 下拉 onchange 即发 PATCH 落库，改完就地刷新——租户隔离直接作用于本页主战场
    h += '<td style="font-size:12px;white-space:nowrap">' + resTenantCell(r) + '</td>';

    var acts = '<button class="btn btn-o btn-sm" onclick="openResourceDrawer(\'' + rEsc(r.key) + '\')">详情</button>';
    if (!r.agent) {
      acts += ' <button class="btn btn-p btn-sm" onclick="openOnboardNew()">接入</button>';
    } else {
      if (r.up) acts += ' <button class="btn btn-p btn-sm" onclick="resUpgrade(\'' + rEsc(r.key) + '\')">升级</button>';
      else if (r.alt) acts += ' <button class="btn btn-o btn-sm" title="该架构没有更高版本，但可变更到其它已发布版本（含降级）" onclick="resUpgrade(\'' + rEsc(r.key) + '\')">变更版本</button>';
      if (r.state.key === 'stopped') acts += ' <button class="btn btn-o btn-sm" onclick="resService(\'' + rEsc(r.key) + '\',\'start\')">启动</button>';
      else acts += ' <button class="btn btn-o btn-sm" onclick="resService(\'' + rEsc(r.key) + '\',\'stop\')">停用</button>';
      acts += ' <button class="btn btn-d btn-sm" onclick="resOffboard(\'' + rEsc(r.key) + '\')">卸载</button>';
    }
    h += '<td style="white-space:nowrap">' + acts + '</td>';
    return h + '</tr>';
  }

  function resAge(sec) {
    var s = Number(sec) || 0;
    if (s < 90) return Math.round(s) + 's 前';
    if (s < 5400) return Math.round(s / 60) + 'min 前';
    return Math.round(s / 3600) + 'h 前';
  }

  window.resSetFilter = function (k) { _resFilter = k; resRenderPage(); };
  window.resSearch = function (q) { _resQuery = q || ''; var tb = document.getElementById('res-tbody'); if (tb) tb.innerHTML = resRowsHTML(resBuildRows(_resData)); };

  // 归属租户下拉（D3 资源级分配）。有效租户 = 资源 tenant_id → Agent tenant_id → default。
  // 无缓存租户清单时先渲染当前值，异步拉取后由 resFillTenantCells 就地补齐选项。
  function resEffectiveTenant(r) {
    return (r.res && r.res.tenant_id) || (r.agent && r.agent.tenant_id) || 'default';
  }
  function resTenantCell(r) {
    var cur = resEffectiveTenant(r);
    var c = (window._tenantCodes || { default: '默认租户' });
    var opts = '';
    Object.keys(c).forEach(function (code) { opts += '<option value="' + rEsc(code) + '"' + (code === cur ? ' selected' : '') + '>' + rEsc(c[code]) + '</option>'; });
    return '<select onchange="resSetTenant(' + rEsc(r.key) + ', this.value)" title="调整该资源归属租户（Agent 侧租户不受影响）" style="padding:3px 6px;border:0.5px solid var(--border);border-radius:5px;font-size:12px;max-width:130px">' + opts + '</select>';
  }
  window.resSetTenant = function (id, code) {
    fetch(API + '/resources', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: id, tenant_id: code })
    }).then(function (r) { return r.json(); }).then(function (o) {
      if (o && o.error) { toast('调整失败：' + o.error, 'err'); resRenderPage(); return; }
      toast('资源 ' + id + ' 已归属租户「' + ((window._tenantCodes || {})[code] || code) + '」', 'ok');
      _resData = _resData;
      // 资源已按新租户归属，触达层过滤随之生效；若当前切走的租户不再含它则从清单消失
      resRenderPage();
    });
  };
  // 租户清单异步就绪后补齐所有行下拉选项（loadTenantOptions 首次拉完即触发）
  window.resFillTenantCells = function () {
    var tb = document.getElementById('res-tbody');
    if (tb) tb.innerHTML = resRowsHTML(resBuildRows(_resData));
  };

  // ---------- 行上动作：复用既有流水线端点（resource_id 即可拉起，不依赖来源 flow） ----------
  function resLaunch(path, body, okMsg) {
    fetch(API + path, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
    }).then(function (r) { return r.json(); }).then(function (o) {
      if (o && o.error) { toast('发起失败：' + o.error, 'err'); return; }
      toast(okMsg + '（flow-' + o.flow_id + '）', 'ok');
      closeResourceDrawer();
      setTimeout(function () { openOnboardFlow(o.flow_id); }, 400);
    }).catch(function (e) { toast('发起失败：' + (e && e.message || e), 'err'); });
  }

  window.resUpgrade = function (key) {
    var r = resFind(key);
    var label = r ? (r.res.ip || r.res.id) : key;
    if (!r) { toast('资源不存在（可能已被删除）', 'err'); return; }
    // 目标版本：优先"有更高版本"的升级入口，其次兜底的"变更版本"（含降级）
    var tgt = r.up || r.alt;
    if (!tgt) { toast('版本清单里没有可变更的已发布版本', 'err'); return; }
    var dir = resVerDelta(r.res.installed_tag, r.res.arch || r.facts.arch, tgt);
    var verb = dir < 0 ? '降级' : '升级';
    var lines = [
      verb + ' SAgent · ' + label,
      '',
      '当前版本：' + (r.res.installed_tag || '—') + '  →  目标版本：' + tgt.tag + '（' + tgt.version + '）'
    ];
    if (dir < 0) lines.push('', '⚠ 这是一次【降级】：目标版本低于当前版本，属"换回旧版"操作，请确认确有必要');
    lines.push('',
      '流程：预检当前版本与守护形态 → 人工选定目标版本（本页给的是建议，选版环节可改）→ 人工确认影响 → ansible 执行（停插件/停进程/备份/换包/sha256 断言/启动/自证核对）',
      '不受影响：配置、数据、插件、平台登记；旧二进制备份（SAgent.bak-<旧版本>）留在目标机可手工回滚',
      '', '确认发起？');
    if (!confirm(lines.join('\n'))) return;
    resLaunch('/onboard/flow/upgrade', { resource_id: r.res.id }, '已发起' + verb + '流水线');
  };

  // 启停＝一次点击直达：不弹确认框、不走向导界面，点了就下发，回执里把作用范围说清。
  // 不再问"确认吗"（2026-09-24 用户评审："停用，需要我确认？确认什么东西？？？"）——
  // 动作与目标在拉起时已选定，审计 + 过程记录逐次留痕，停→启随时可回，没有需要人点头的东西。
  // 作用范围是整机采集：启就是全部、停也是全部（SAgent 进程 + 该机全部采集插件）
  window.resService = function (key, action) {
    var r = resFind(key);
    if (!r) { toast('资源不存在（可能已被删除）', 'err'); return; }
    var label = r.res.ip || r.res.id;
    var verb = action === 'stop' ? '停用' : '启动';
    fetch(API + '/onboard/flow/service', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ resource_id: r.res.id, action: action, target: 'process' })
    }).then(function (resp) { return resp.json(); }).then(function (o) {
      if (o && o.error) { toast('发起失败：' + o.error, 'err'); return; }
      toast(verb + '整机采集已下发 · ' + label + '（SAgent 进程 + 该机全部采集插件，flow-' + o.flow_id + '）· 结果见「过程记录」', 'ok');
      // ansible 在目标机执行需要时间，晚一步回读；不打开向导，抽屉就地刷新状态
      setTimeout(function () {
        resFetchAll(function (d) {
          _resData = d;
          resRenderPage();
          if (_drawerKey && resFind(_drawerKey)) openResourceDrawer(_drawerKey);
        });
      }, 4000);
    }).catch(function (e) { toast('发起失败：' + (e && e.message || e), 'err'); });
  };

  // resNewTarget 给这台机器扩采集：走接入向导（插件从平台能力目录选、带参数与连通测试、下发后回执），
  // 并把本机 Agent 预选好。走向导而不是那个精简表单，是因为"扩展采集"要选能力、填参数、验连通，
  // 精简表单只能手打插件名、既无参数也不测通不通（2026-09-24 用户确认要保留此入口后修正）
  window.resNewTarget = function (agentID) {
    closeResourceDrawer();
    if (typeof openOnboardWizard === 'function') {
      // 关掉向导回「资源与接入」本页——向导默认回采集目标页，从抽屉进来的话落点不对
      openOnboardWizard(null, agentID || null, function () { goPage('resources'); });
    }
  };

  window.resOffboard = function (key) {
    var r = resFind(key);
    var label = r ? (r.res.ip || r.res.id) : key;
    if (!r) { toast('资源不存在（可能已被删除）', 'err'); return; }
    // 台账缺失、仅有 Agent 登记（演示/直连 Agent，无独立目标机）→ 无法执行主机侧卸载，
    // 退化为注销平台登记（内存 + DB 行），不做 ansible；避免 offboard 因查无资源记录而 "资源对象不存在"
    var agentOnly = !(_resData.resources || []).some(function (x) { return x.id === r.res.id; }) && r.agent != null;
    if (agentOnly) {
      if (!confirm('卸载 Agent · ' + label + '\n\n该行仅有平台登记、无资源台账（演示/直连 Agent，无独立目标机），无法执行主机侧卸载。\n将注销该 Agent 的台账登记（内存 + 数据库）。\n\n确认注销？')) return;
      fetch(API + '/agents/' + encodeURIComponent(r.res.id), { method: 'DELETE' })
        .then(function (resp) { return resp.json(); })
        .then(function (o) {
          if (o && o.error) { toast('注销失败：' + o.error, 'err'); return; }
          toast('已注销 Agent「' + r.res.id + '」', 'ok');
          closeResourceDrawer();
          setTimeout(function () { resRenderPage(); }, 200);
        }).catch(function (e) { toast('注销失败：' + (e && e.message || e), 'err'); });
      return;
    }
    if (!confirm('卸载 SAgent（不可逆：会删除目标机上的 SAgent 安装目录）· ' + label + '\n\n目标机侧：停进程 → 删安装目录 → 自证核对\n平台侧：注销 Agent 台账与期望配置 → 撤抓取登记 → 删采集目标\n保留：资源对象与 SSH 凭据（可原样重新接入）\n\n确认发起？')) return;
    resLaunch('/onboard/flow/offboard', { resource_id: r.res.id }, '已发起卸载流水线');
  };

  function resFind(key) {
    var rows = resBuildRows(_resData);
    for (var i = 0; i < rows.length; i++) { if (rows[i].key === key) return rows[i]; }
    return null;
  }

  // ---------- 机器详情抽屉（图 3） ----------
  window.openResourceDrawer = function (key) {
    var r = resFind(key);
    if (!r) {
      // 从别的页面跳进来（采集目标页「所属资源」等）时资源数据可能还没拉过，先补一次
      resFetchAll(function (d) {
        _resData = d;
        if (!resFind(key)) { toast('资源不存在（可能已被删除）', 'err'); return; }
        openResourceDrawer(key);
      });
      return;
    }
    _drawerKey = key;
    var m = document.getElementById('res-drawer-mask');
    var old = document.getElementById('res-drawer');
    if (m) m.parentNode.removeChild(m);
    if (old) old.parentNode.removeChild(old);
    // 背景灰化遮罩：盖住底层页面使其与白色面板对比明显 + 点遮罩关闭抽屉
    m = document.createElement('div');
    m.id = 'res-drawer-mask';
    m.className = 'res-drawer-mask';
    m.onclick = function () { closeResourceDrawer(); };
    var d = document.createElement('div');
    d.id = 'res-drawer';
    d.className = 'res-drawer';
    d.innerHTML = resDrawerHTML(r);
    document.body.appendChild(m);
    document.body.appendChild(d);
    requestAnimationFrame(function () {
      d.classList.add('open');
      m.classList.add('open');
    });
  };

  window.closeResourceDrawer = function () {
    var d = document.getElementById('res-drawer');
    var m = document.getElementById('res-drawer-mask');
    if (d) d.classList.remove('open');
    if (m) m.classList.remove('open');
    setTimeout(function () {
      if (d && d.parentNode) d.parentNode.removeChild(d);
      if (m && m.parentNode) m.parentNode.removeChild(m);
    }, 200);
  };

  // resProcessList 过程记录列表：该机全部流水线（时间倒序），默认只铺最近 3 条、
  // 其余折叠——抽屉不被十几行撑爆，但历史一条不丢，每行都能进对应向导。
  // 这是「已接入成功却看不到接入过程」的正解：接入过程不再被后来的卸载/启停顶掉
  var RES_PROC_SHOW = 3;
  function resProcessList(r) {
    var flows = r.flows || [];
    var h = '<div class="res-sec"><div class="res-sec-hd">过程记录'
      + (flows.length ? ' <span style="font-weight:400;color:var(--muted)">共 ' + flows.length + ' 次</span>' : '')
      + '</div>';
    if (!flows.length) {
      return h + '<div style="font-size:12px;color:var(--muted)">这台机还没有过程记录——接入流水线跑起来后会在这里逐次留痕</div></div>';
    }
    h += '<div class="res-proc">';
    for (var i = 0; i < flows.length; i++) {
      var f = flows[i], k = obFlowKindOf(f), extra = i >= RES_PROC_SHOW;
      h += '<div class="res-proc-row"' + (extra ? ' data-proc-extra="1" style="display:none"' : '')
        + ' onclick="openOnboardFlow(' + f.id + ')">';
      h += '<span class="k">' + k.icon + ' ' + obEscape(k.label || '流水线') + '</span>';
      h += obBadge(f.status, f.wait_kind);
      h += '<span class="tm">' + obEscape(f.started_at || '') + '</span>';
      h += '<span class="st">' + (f.step_ids || []).length + ' 步</span>';
      h += '<button class="btn btn-o btn-sm" style="margin-left:auto" onclick="event.stopPropagation();openOnboardFlow(' + f.id + ')">查看过程</button>';
      h += '</div>';
    }
    h += '</div>';
    if (flows.length > RES_PROC_SHOW) {
      h += '<div style="margin-top:6px"><button class="btn btn-o btn-sm" data-total="' + flows.length
        + '" onclick="resProcessToggle(this)">展开全部 ' + flows.length + ' 条</button></div>';
    }
    return h + '</div>';
  }

  // resProcessToggle 过程记录展开/收起。抽屉每次打开都是新 DOM（data-open 未设=收起），
  // 所以不需要额外的复位逻辑，也不会跨次串台
  window.resProcessToggle = function (btn) {
    var rows = document.querySelectorAll('#res-drawer [data-proc-extra]');
    var open = btn.getAttribute('data-open') === '1';
    for (var i = 0; i < rows.length; i++) rows[i].style.display = open ? 'none' : 'flex';
    btn.setAttribute('data-open', open ? '0' : '1');
    btn.textContent = open ? ('展开全部 ' + btn.getAttribute('data-total') + ' 条')
      : ('收起（只看最近 ' + RES_PROC_SHOW + ' 条）');
  };

  function resDrawerHTML(r) {
    var facts = r.facts || {};
    var h = '<div class="res-drawer-hd"><b style="font-size:14px">' + rEsc(r.res.ip || r.res.id) + '</b>'
      + '<span style="color:var(--muted);font-size:12px">' + rEsc(r.res.name || '') + '</span>'
      + '<span class="' + r.state.cls + '">' + rEsc(r.state.label) + '</span>'
      + '<span style="margin-left:auto;cursor:pointer" onclick="closeResourceDrawer()">✕</span></div><div class="res-drawer-bd">';

    // ① 实测档案（自动采的，不用人工填）
    h += '<div class="res-sec"><div class="res-sec-hd">实测档案 <span style="font-weight:400;color:var(--muted)">最近探路 ' + rEsc(r.res.probed_at || '未探路') + '</span></div>';
    var chips = [];
    if (facts.distribution) chips.push(facts.distribution);
    if (facts.arch || r.res.arch) chips.push(facts.arch || r.res.arch);
    if (facts.kernel || r.res.kernel) chips.push('内核 ' + (facts.kernel || r.res.kernel));
    if (facts.glibc) chips.push('glibc: ' + facts.glibc);
    if (facts.mem_mb) chips.push('内存 ' + (facts.mem_mb / 1024).toFixed(0) + 'G');
    if (facts.disk_free_gb) chips.push('磁盘余 ' + Math.round(facts.disk_free_gb) + 'G');
    if (facts.cpu_vcpus) chips.push(facts.cpu_vcpus + ' vCPU');
    if (facts.port_source) chips.push('SSH ' + rEsc(r.res.ssh_port || 22) + '（' + facts.port_source + '）');
    h += chips.length ? chips.map(function (c) { return '<span class="res-chip">' + rEsc(c) + '</span>'; }).join('')
      : '<span style="color:var(--muted);font-size:12px">尚无实测档案——接入流水线的探路环节会采到这些</span>';
    h += '</div>';

    // ② 版本（平台装的 tag 才是口径）
    if (r.agent) {
      h += '<div class="res-sec"><div class="res-sec-hd">版本</div><div style="font-size:12px">'
        + (r.res.installed_tag ? '平台安装：<b>' + rEsc(r.res.installed_tag) + '</b>' : '<span style="color:var(--muted)">平台无安装记录（演示容器或直接接入的目标）</span>')
        + (r.agent.version ? ' · Agent 自报 <code style="font-size:11px">' + rEsc(r.agent.version) + '</code>' : '')
        + (r.up ? ' · <span style="color:#92400e">可升级到 ' + rEsc(r.up.tag) + '</span>' : '')
        + '</div></div>';
    }

    // ③ 这台机器上的采集目标（台账关联优先，Agent 分派兜底）——定义"采什么"
    h += '<div class="res-sec"><div class="res-sec-hd">这台机器上的采集目标 <span style="font-weight:400;color:var(--muted)">定义"采什么"</span></div>';
    var tgs = (_resData.targets || []).filter(function (tg) {
      return tg.resource_id === r.res.id || (r.agent && tg.agent_id === r.agent.id);
    });
    if (tgs.length) {
      h += '<table style="font-size:12px;width:100%">' + tgs.map(function (tg) {
        var st = tg.last_test_result
          ? (tg.last_test_result.indexOf('失败') === 0
            ? '<span class="badge" style="background:#fee2e2;color:var(--error)">' + rEsc(tg.last_test_result) + '</span>'
            : '<span class="badge b-h">' + rEsc(tg.last_test_result) + '</span>')
          : '<span class="badge b-o">未测试</span>';
        return '<tr><td><b style="font-weight:500">' + rEsc(tg.name) + '</b></td>'
          + '<td><span class="badge" style="background:#e0f2f1;color:var(--primary)">' + rEsc(tg.type) + '</span></td>'
          + '<td><code style="font-size:11px">' + rEsc(tg.address) + '</code></td>'
          + '<td>' + (tg.plugin ? rEsc(tg.plugin) : '<span style="color:var(--muted)">—</span>') + '</td>'
          + '<td>' + st + '</td></tr>';
      }).join('') + '</table>';
    } else {
      h += '<div style="font-size:12px;color:var(--muted)">这台机器还没有采集目标——采集目标定义"采什么"（MySQL / Redis / HTTP…），登记后才真的开始采</div>';
    }
    // 空态给的是「给这台机器扩采集」的入口——走接入向导（插件从能力目录选、带参数与连通测试），
    // 且**预选本机 Agent**：原来这里调 targetEdit() 不带上下文，建出来的目标落在"未分派"，
    // 本机清单里根本看不到它，等于白填一遍表单（2026-09-24 用户追问后修正）
    h += '<div style="margin-top:6px;display:flex;gap:6px;flex-wrap:wrap">'
      + (tgs.length ? '<button class="btn btn-o btn-sm" onclick="goPage(\'targets\')">采集目标页管理</button>'
                    : (r.agent ? '<button class="btn btn-p btn-sm" onclick="resNewTarget(\'' + rEsc(r.agent.id) + '\')">➕ 新增采集目标</button>' : ''))
      + (r.agent ? '<button class="btn btn-o btn-sm" onclick="resOpenAgentDetail(\'' + rEsc(r.agent.id) + '\')">改采集配置 / 频率</button>' : '')
      + '</div></div>';

    // ④ 最近数据（VM 实测）
    h += '<div class="res-sec"><div class="res-sec-hd">最近数据</div>';
    if (r.stat && r.stat.reporting) {
      h += '<div style="font-size:12px">在报 <b>' + r.stat.metrics + '</b> 项指标 · 最近一条样本 <b>' + resAge(r.stat.age_sec) + '</b>'
        + (r.stat.instance ? ' · <code style="font-size:11px">' + rEsc(r.stat.instance) + '</code>' : '') + '</div>';
    } else {
      h += '<div style="font-size:12px;color:#991b1b">VM 里没有该资源的样本——' + (r.agent ? '接入完成但数据没到，点下方「过程记录」最近一次过程，看卡在哪一步' : '这台机还没接入') + '</div>';
    }
    h += '<div style="margin-top:6px"><button class="btn btn-o btn-sm" onclick="resOpenVMData()">看数据（VMUI）→</button></div>';
    h += '</div>';

    // ⑤ 动作：只留「真会改状态」的动作按钮，一个动作一个按钮、点了就办。
    // 不再放「查看接入/卸载/启停过程」这类跳转向导的按钮——过程回溯统一收口到下方「过程记录」，
    // 那里每次流水线都在、按类别标好人话，比在动作区各挂一个"最近一次"入口更不容易看漏（2026-09-24 用户评审）
    h += '<div class="res-sec"><div class="res-sec-hd">动作</div><div style="display:flex;gap:6px;flex-wrap:wrap">';
    if (r.agent) {
      if (r.up) h += '<button class="btn btn-p btn-sm" onclick="resUpgrade(\'' + rEsc(r.key) + '\')">升级 Agent（→ ' + rEsc(r.up.version) + '）</button>';
      if (r.state.key === 'stopped') h += '<button class="btn btn-o btn-sm" onclick="resService(\'' + rEsc(r.key) + '\',\'start\')">启动</button>';
      else h += '<button class="btn btn-o btn-sm" onclick="resService(\'' + rEsc(r.key) + '\',\'stop\')">停用</button>';
      h += '<button class="btn btn-d btn-sm" onclick="resOffboard(\'' + rEsc(r.key) + '\')">卸载</button>';
    } else {
      h += '<button class="btn btn-p btn-sm" onclick="openOnboardNew()">接入</button>';
    }
    h += '</div></div>';

    // ⑥ 过程记录：这台机器的每一次流水线都在（时间倒序），每行直接进对应向导
    h += resProcessList(r);
    return h + '</div>';
  }

  window.resOpenAgentDetail = function (agentID) {
    closeResourceDrawer();
    if (typeof openDetailById === 'function') { openDetailById(agentID); if (typeof switchDetailTab === 'function') switchDetailTab('config'); }
  };

  // 看数据：VMUI 深链（免登录）。查询口径与 /api/collect-stats 一致：
  // series 带 resource_id 标签用 resource_id 查；演示容器只有 instance 标签就按 instance 查
  window.resOpenVMData = function () {
    var r = resFind(_drawerKey);
    var expr = '{resource_id="' + (r && r.res ? r.res.id : '') + '"}';
    if (r && r.stat && r.stat.instance && !r.stat.resource_id) expr = '{instance="' + r.stat.instance + '"}';
    siteCfg(function (cfg) { window.open(cfg.vm_url + '/vmui/?g0.expr=' + encodeURIComponent(expr), '_blank'); });
  };

  // ---------- 采集策略（前端效果设计 3.4：聚合实现，后端无策略实体） ----------
  // 策略 = 采集能力 + 模板参数的 interval/params 聚合，影响目标取 /api/targets 真实引用。
  // 数据源：/api/onboard/templates（能力+默认参数）/ api/targets（已落地目标）/ api/resources（租户上下文）。
  function renderPolicy(hostId) {
    var el = hostId ? document.getElementById(hostId) : document.getElementById('main-content');
    if (!el) return;
    el.innerHTML = '<div style="text-align:center;color:var(--muted);padding:20px">⏳ 汇总采集策略…</div>';
    Promise.all([
      fetch(API + '/onboard/templates').then(function (r) { return r.json(); }).catch(function () { return {}; }),
      fetch(API + '/targets').then(function (r) { return r.json(); }).catch(function () { return []; }),
      fetch(API + '/resources').then(function (r) { return r.json(); }).catch(function () { return {}; })
    ]).then(function (out) {
      var abilities = (out[0] && out[0].abilities) || [];
      var targets = Array.isArray(out[1]) ? out[1] : ((out[1] && out[1].targets) || []);
      var tenant = window._currentTenant || 'default';
      // 影响目标：当前租户可见且 type===能力 id 的已落地目标（去重资源）
      var impact = {};
      targets.forEach(function (t) {
        var vis = window.tenantVisible ? ((t.tenant_id || 'default') === tenant) : true;
        if (!vis) return;
        if (!impact[t.type]) impact[t.type] = { targets: 0, res: {} };
        impact[t.type].targets++;
        if (t.resource_id) impact[t.type].res[t.resource_id] = 1;
      });
      // 能力默认间隔：params 里 name==='interval' 的 default；端口型能力标 "按端口"；无则随接入参数
      function defInterval(a) {
        var p = a.params || [];
        for (var i = 0; i < p.length; i++) {
          if (p[i].name === 'interval' && p[i].default) return String(p[i].default);
        }
        if (a.default_port) return '按端口 ' + a.default_port;
        return '随接入参数';
      }
      var agentAb = [], remoteAb = [];
      abilities.forEach(function (a) {
        var im = impact[a.id] || { targets: 0, res: {} };
        var obj = { a: a, nTargets: im.targets, nRes: Object.keys(im.res).length, interval: defInterval(a) };
        if (a.scope === 'remote') remoteAb.push(obj); else agentAb.push(obj);
      });
      var totalT = 0;
      abilities.forEach(function (a) { totalT += (impact[a.id] || { targets: 0 }).targets; });

      var h = '<div class="pol-note">策略 = 采集能力 + 默认采集间隔的聚合视图，无独立策略实体。影响目标取自 <code>/api/targets</code> 真实引用，随归属租户过滤。'
        + '　当前租户「' + rEsc((window._tenantCodes || {})[tenant] || tenant) + '」：<b>' + totalT + '</b> 个已落地目标</div>';

      h += polGroup('agent', '平台能力（Agent 侧采集）', '随 SAgent 一起部署的本地采集能力，多为地基能力', agentAb);
      h += polGroup('remote', '远端接入能力（Proxy 侧采集）', '部署在 Proxy 节点远程采集中间件，按目标端口探测', remoteAb);
      h += '<div style="margin-top:8px;font-size:11px;color:var(--muted)">🔌 修改采集参数 / 频率：资源行「详情」→ 改采集配置；新增能力走「资源与接入 → + 接入新资源」三步向导。</div>';

      el.innerHTML = '<div style="display:flex;flex-wrap:wrap;gap:10px">' + h + '</div>';
    }).catch(function () {
      el.innerHTML = '<div style="color:#991b1b;font-size:12px;padding:12px">采集策略加载失败</div>';
    });
  }

  function polGroup(scope, title, sub, list) {
    var h = '<div class="pol-group"><div class="pol-gt">' + rEsc(title) + ' <span class="pol-gs">' + rEsc(sub) + '</span></div>';
    if (!list.length) { h += '<div style="color:var(--muted);font-size:12px;padding:8px 0">无能力</div>'; }
    list.forEach(function (o) {
      var a = o.a;
      var imCls = o.nTargets ? 'pol-badge pol-impact' : 'pol-badge';
      var scBadge = a.locked
        ? '<span class="pol-badge lock">🔒 地基能力</span>'
        : '<span class="pol-badge scope-' + ((a.scope === 'remote') ? 'remote' : 'agent') + '">' + rEsc(a.scope === 'remote' ? '远端' : 'Agent') + '</span>';
      h += '<div class="pol-card"><div class="pol-head">'
        + '<span class="pol-name">' + rEsc(a.name || a.id) + '</span>'
        + '<span class="pol-id">' + rEsc(a.id) + '</span>'
        + scBadge
        + '<span class="' + imCls + '">📌 影响 ' + o.nRes + ' 资源 / ' + o.nTargets + ' 目标</span>'
        + '<span class="pol-intv">⏱ <b>' + rEsc(o.interval) + '</b></span>'
        + '</div>'
        + (a.desc ? '<div class="pol-desc">' + rEsc(a.desc) + '</div>' : '')
        + '</div>';
    });
    return h + '</div>';
  }

  // 能力与策略合页（采集策略 / 采集插件 / 指标目录），低频治理字段留在各自页内
  window.renderCapabilities = function () {
    var h = '<div class="card"><div class="card-hd">🔌 采集能力与策略'
      + '<span class="tabs" style="margin-left:12px"><span class="active" data-tab="policy" onclick="capTab(\'policy\')">采集策略</span>'
      + '<span data-tab="plugins" onclick="capTab(\'plugins\')">采集插件</span>'
      + '<span data-tab="metrics" onclick="capTab(\'metrics\')">指标目录</span></span></div>'
      + '<div class="card-bd" id="cap-body"></div></div>';
    document.getElementById('main-content').innerHTML = h;
    capTab('policy');
  };

  window.capTab = function (t) {
    document.querySelectorAll('#main-content .card-hd .tabs span').forEach(function (s) {
      s.classList.toggle('active', s.dataset.tab === t);
    });
    // 各子视图渲染到同一容器（renderXxx(hostId)），不互相覆盖
    if (t === 'metrics') renderMetricsCatalog('cap-body');
    else if (t === 'policy') renderPolicy('cap-body');
    else renderPluginsMart('cap-body');
  };

  window.renderResources = renderResources;
  window.openResourceDrawer = window.openResourceDrawer;
  // 总览页与资源页共用同一份取数与装配（口径只有一处）
  window.resFetchAll = resFetchAll;
  window.resBuildRows = resBuildRows;
})();
