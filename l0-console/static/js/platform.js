// platform.js — P2 新建两页（IA 整合设计 5 组 12 页收尾）：
//   ① 数据治理（data-gov）：野指标 + 高基数治理切片。判决卡留在采集健康页（规则 4：数据同源，下钻代替复制），
//      本页只放治理动作面。高基数数据源 = /api/vm/cardinality（VM TSDB 状态接口，真实数据）。
//   ② 平台健康（platform-health）：/api/health 组件化自检（L0 控制面/目录存储/Agent 心跳/VM 数据链）
//      + L1 采集机心跳切片（agents type=proxy 实测）+ 对账引擎心跳（/api/recon generated_at）。
//      池化派发属 L1HA-2 待立项，本页不展示假池数据。
// 所有数据源均为只读端点；两页均无写操作。

(function () {

  // ---------- ① 数据治理 ----------
  window.renderDataGov = function () {
    var html = '';
    // 页头：主问题（IA 规范）
    html += '<div class="card"><div class="card-bd" style="padding-bottom:10px"><span style="font-size:16px;font-weight:700">🧹 数据治理</span>'
      + '<span style="font-size:12px;color:var(--muted);margin-left:10px">数据干不干净？</span></div></div>';
    html += '<div class="card"><div class="card-bd" id="dg-verdict" style="font-size:14px;font-weight:600">⏳ 拉取对账口径...</div></div>';
    // 口径覆盖统计（D8：应报口径 = 目录注册集 ∪ Agent 生效配置展开集）
    html += '<div class="slo-grid" style="margin-top:16px" id="dg-stats"></div>';
    // 野指标清单（治理动作面）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">🌿 野指标治理 <span style="font-weight:400;font-size:11px;color:var(--muted)">VM 在报但应报口径未覆盖 · 判定以采集健康页对账引擎为准</span></div>';
    html += '<div class="card-bd" id="dg-wild" style="text-align:center;color:var(--muted);padding:16px">加载中...</div></div>';
    // 高基数治理（VM TSDB 真实数据）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">📊 高基数治理 <span style="font-weight:400;font-size:11px;color:var(--muted)">series 按指标名 TopN · 数据源 VM TSDB 状态接口（实测）</span></div>';
    html += '<div class="card-bd" id="dg-card" style="text-align:center;color:var(--muted);padding:16px">加载中...</div></div>';
    html += '<div class="card" style="margin-top:16px"><div class="card-bd" style="font-size:11px;color:var(--muted)">治理口径（D8）：目录该有而没有的 → 到 <a href="javascript:void(0)" onclick="goPage(\'capabilities\')" style="color:var(--primary)">插件与策略</a> 登记指标；确认不该采的 → 检查对应 Agent 生效配置；高基数治理建议确认后走 <a href="javascript:void(0)" onclick="goPage(\'changes\')" style="color:var(--primary)">变更中心</a> 执行，本页不直接改采集配置。野指标判定每轮对账刷新，本页与采集健康页同源（/api/recon），不复制数字。</div></div>';
    document.getElementById("main-content").innerHTML = html;

    fetch(API + '/recon').then(function (r) { return r.json(); }).catch(function () { return null; }).then(function (d) {
      var v = document.getElementById('dg-verdict');
      if (!d || !d.summary) { if (v) v.textContent = '⚠ 对账引擎暂不可用，治理切片无法计算'; return; }
      var s = d.summary;
      var wildTotal = s.wild || 0;
      if (v) {
        if (s.vm_reachable === false) {
          v.textContent = '⚠ VM 不可达，本轮野指标不可判（空清单不等于无野指标）';
          v.style.color = 'var(--error)';
        } else if (wildTotal > 0) {
          v.innerHTML = '发现 <span style="color:var(--warning,#b45309)">' + wildTotal + '</span> 个野指标待治理（登记口径或收敛采集）';
        } else {
          v.innerHTML = '✅ 无野指标：VM 在报指标全部落在应报口径内';
        }
      }
      var st = document.getElementById('dg-stats');
      if (st) {
        st.innerHTML =
          '<div class="slo-card"><div class="slo-val" style="color:' + (wildTotal > 0 ? 'var(--warning,#b45309)' : 'var(--success)') + '">' + wildTotal + '</div><div class="slo-label">野指标总数</div></div>'
          + '<div class="slo-card"><div class="slo-val" style="color:var(--primary)">' + (s.known_metrics || 0) + '</div><div class="slo-label">应报口径指标数</div></div>'
          + '<div class="slo-card"><div class="slo-val" style="color:var(--primary)">' + (s.config_expanded || 0) + '</div><div class="slo-label">参与口径展开的 Agent</div></div>';
      }
      var wb = document.getElementById('dg-wild');
      if (wb) {
        if (s.vm_reachable === false) { wb.textContent = '⚠ VM 不可达，无法判定野指标'; }
        else if (!d.wild || d.wild.length === 0) { wb.innerHTML = '✅ 无野指标'; }
        else {
          wb.style.textAlign = 'left';
          wb.innerHTML = d.wild.map(function (n) {
            return '<code style="display:inline-block;margin:2px 4px 2px 0;padding:2px 6px;background:var(--bg2,#f5f5f5);border-radius:4px;font-size:11px">' + escHtml(n) + '</code>';
          }).join('')
            + (wildTotal > d.wild.length ? '<div style="margin-top:8px;color:var(--muted);font-size:11px">…共 ' + wildTotal + ' 条（本页展示前 ' + d.wild.length + '）</div>' : '');
        }
      }
    });

    // 高基数：VM TSDB 实测 TopN（不可达时显示不可判，不显示空清单冒充"无高基数"）
    fetch(API + '/vm/cardinality').then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); }).catch(function () { return null; }).then(function (rs) {
      var cb = document.getElementById('dg-card');
      if (!cb) return;
      if (!rs || !rs.ok) {
        cb.textContent = '⚠ VM TSDB 状态不可用' + (rs && rs.j && rs.j.error ? '：' + rs.j.error : '');
        return;
      }
      var top = rs.j.top_metrics || [];
      var total = rs.j.total_series || 0;
      if (!top.length) { cb.innerHTML = '— VM 尚无 series 数据'; return; }
      cb.style.textAlign = 'left';
      var maxV = top[0].value || 1;
      cb.innerHTML = '<div style="font-size:11px;color:var(--muted);margin-bottom:6px">VM 总 series：' + total + ' · 以下为 series 数最多的指标（治理建议：确认 label 设计，必要时收敛基数后走变更中心）</div>'
        + top.map(function (m, i) {
          var w = Math.max(4, Math.round(m.value / maxV * 100));
          var hot = i === 0 && m.value > 1000;
          return '<div style="display:flex;align-items:center;gap:10px;font-size:11.5px;padding:2px 0">'
            + '<code style="min-width:200px;overflow:hidden;text-overflow:ellipsis">' + escHtml(m.name) + '</code>'
            + '<div style="flex:1;height:7px;border-radius:4px;background:var(--border,#e3eaee);overflow:hidden"><div style="width:' + w + '%;height:100%;background:' + (hot ? 'var(--warning,#b45309)' : 'var(--primary)') + '"></div></div>'
            + '<span style="white-space:nowrap;color:var(--muted)">' + m.value + ' series' + (hot ? ' ⚠' : '') + '</span></div>';
        }).join('');
    });
  };

  // ---------- ② 平台健康 ----------
  var _phTimer = null;
  window.renderPlatformHealthPage = function () {
    var html = '';
    // 页头：主问题（IA 规范）
    html += '<div class="card"><div class="card-bd" style="padding-bottom:10px"><span style="font-size:16px;font-weight:700">🩺 平台健康</span>'
      + '<span style="font-size:12px;color:var(--muted);margin-left:10px">平台自己活得好不好？· 本页管平台自身组件，采集数据链在采集健康页（两页主问题不同，不得混）</span></div></div>';
    html += '<div class="card"><div class="card-bd" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">'
      + '<div id="ph-verdict" style="font-size:14px;font-weight:600">⏳ 平台自检中...</div>'
      + '<span id="ph-meta" style="font-size:11px;color:var(--muted);margin-left:auto"></span>'
      + '<button class="btn btn-o btn-sm" onclick="renderPlatformHealthPage()">↻ 重新自检</button></div></div>';
    // 组件分层明细（逐项可下钻语义：状态 + 人话说明）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">🧩 L0 组件健康 <span style="font-weight:400;font-size:11px;color:var(--muted)">L0 控制面 / 目录存储 / Agent 心跳 / VM 数据链 · 任一子项探活失败不阻断整体观测</span></div>';
    html += '<div class="card-bd" id="ph-comps" style="text-align:center;color:var(--muted);padding:16px">自检中...</div></div>';
    // L1 采集机切片 + 对账引擎心跳（真实数据：agents type=proxy + recon generated_at）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">🛰 L1 采集机 <span style="font-weight:400;font-size:11px;color:var(--muted)">采集机（proxy）心跳实测 · 池化派发属 L1HA-2 待立项，本页不展示池数据</span></div>';
    html += '<div class="card-bd" id="ph-l1" style="text-align:center;color:var(--muted);padding:16px">查询中...</div></div>';
    html += '<div class="card" style="margin-top:16px"><div class="card-bd" style="font-size:11px;color:var(--muted)">口径说明：心跳新鲜阈值 120s（与对账/自监控一致）；status 由组件多数派汇总——任一 fail → down，有 degraded 或断采/野指标/僵尸 → degraded。对账引擎全自动周期运行，无人值守；异常时向工作台待办投递。本页纯只读，不下发任何指令，不提供修复操作。</div></div>';
    document.getElementById("main-content").innerHTML = html;
    fetchHealth();
    // 页面存续期间 30s 轻刷新（离开页面即停，不叠加定时器）
    if (_phTimer) clearInterval(_phTimer);
    _phTimer = setInterval(function () {
      if (currentPage !== 'platform-health') { clearInterval(_phTimer); _phTimer = null; return; }
      fetchHealth();
    }, 30000);
  };

  function fetchHealth() {
    fetch(API + '/health').then(function (r) { return r.json(); }).catch(function () { return null; }).then(function (d) {
      var v = document.getElementById('ph-verdict');
      var meta = document.getElementById('ph-meta');
      if (!d || !d.components) {
        if (v) { v.textContent = '⚠ 平台自检端点不可用'; v.style.color = 'var(--error)'; }
        var cb = document.getElementById('ph-comps');
        if (cb) cb.textContent = '自检端点不可用';
        return;
      }
      var stMap = {
        ok: { t: '✅ 平台整体健康', c: 'var(--success)', b: 'b-h', l: '正常' },
        degraded: { t: '🟡 平台降级运行', c: 'var(--warning,#b45309)', b: 'b-o', l: '降级' },
        down: { t: '🔴 平台故障', c: 'var(--error)', b: 'b-s', l: '故障' }
      };
      var m = stMap[d.status] || stMap.ok;
      if (v) { v.textContent = m.t; v.style.color = m.c; }
      if (meta) {
        var g = d.generated_at ? new Date(d.generated_at).toLocaleString('zh-CN', { hour12: false }) : '';
        meta.textContent = '自检时间 ' + g;
      }
      var cb2 = document.getElementById('ph-comps');
      if (cb2) {
        cb2.style.textAlign = 'left';
        cb2.innerHTML = '<table style="text-align:left"><thead><tr><th>组件</th><th>状态</th><th>说明</th></tr></thead><tbody>'
          + d.components.map(function (c) {
            var cm = stMap[c.status] || stMap.ok;
            return '<tr><td>' + escHtml(c.name || c.key) + '</td><td><span class="badge ' + cm.b + '">' + cm.l + '</span></td><td style="font-size:11px;color:var(--muted)">' + escHtml(c.detail || '') + '</td></tr>';
          }).join('') + '</tbody></table>';
      }
    });
    // L1 采集机切片 + 对账引擎心跳
    Promise.all([
      fetch(API + '/agents').then(function (r) { return r.json(); }).catch(function () { return []; }),
      fetch(API + '/recon').then(function (r) { return r.json(); }).catch(function () { return null; })
    ]).then(function (rs) {
      var lb = document.getElementById('ph-l1');
      if (!lb) return;
      var ag = Array.isArray(rs[0]) ? rs[0] : (rs[0] || {}).agents || [];
      var proxies = ag.filter(function (a) { return a.type === 'proxy'; });
      var now = Math.floor(Date.now() / 1000);
      var rows = proxies.map(function (p) {
        var online = p.last_seen && (now - p.last_seen) <= 120;
        return '<span class="badge ' + (online ? 'b-h' : 'b-o') + '">' + escHtml(p.id || p.name) + (online ? ' ✅ 心跳正常' : ' ⏳ 心跳过期') + '</span>';
      }).join(' ');
      var recon = rs[1];
      var rcTxt = recon && recon.generated_at
        ? (() => {
            // generated_at 为 'YYYY-MM-DD HH:MM:SS' 本地时区格式
            var t = recon.generated_at.replace(' ', 'T');
            var age = Math.round((Date.now() - new Date(t).getTime()) / 1000);
            return '对账引擎心跳 ' + (isFinite(age) ? age + 's 前' : escHtml(recon.generated_at)) + '（周期自动运行）';
          })()
        : '对账引擎不可用 ⚠';
      if (!proxies.length) {
        lb.innerHTML = '暂无采集机（proxy）—— <span style="color:var(--muted)">' + rcTxt + '</span>';
      } else {
        lb.style.textAlign = 'left';
        lb.innerHTML = rows + '<div style="margin-top:8px;font-size:11px;color:var(--muted)">' + rcTxt + '</div>';
      }
    });
  }

})();
