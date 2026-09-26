// platform.js — P2 新建两页（IA 整合设计 5 组 12 页收尾）：
//   ① 数据治理（data-gov）：野指标治理切片。判决卡留在采集健康页（规则 4：数据同源，下钻代替复制），
//      本页只放治理动作面：野指标清单 + 应报口径统计。
//   ② 平台健康（platform-health）：/api/health 组件化自检视图（L0 控制面/目录存储/Agent 心跳/VM 数据链）。
// 数据源均为既有只读端点，无新后端。高基数/丢弃率切片依赖 VM TopN 查询，后端未暴露，不放假卡。

(function () {

  // ---------- ① 数据治理 ----------
  window.renderDataGov = function () {
    var html = '';
    html += '<div class="card"><div class="card-bd" id="dg-verdict" style="font-size:14px;font-weight:600">⏳ 拉取对账口径...</div></div>';
    // 口径覆盖统计（D8：应报口径 = 目录注册集 ∪ Agent 生效配置展开集）
    html += '<div class="slo-grid" style="margin-top:16px" id="dg-stats"></div>';
    // 野指标清单（治理动作面）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">🌿 野指标治理 <span style="font-weight:400;font-size:11px;color:var(--muted)">VM 在报但应报口径未覆盖 · 判定以采集健康页对账引擎为准</span></div>';
    html += '<div class="card-bd" id="dg-wild" style="text-align:center;color:var(--muted);padding:16px">加载中...</div></div>';
    html += '<div class="card" style="margin-top:16px"><div class="card-bd" style="font-size:11px;color:var(--muted)">治理口径（D8）：目录该有而没有的 → 到 <a href="javascript:void(0)" onclick="goPage(\'capabilities\')" style="color:var(--primary)">插件与策略</a> 登记指标；确认不该采的 → 检查对应 Agent 生效配置。野指标判定每轮对账刷新，本页与采集健康页同源（/api/recon），不复制数字。</div></div>';
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
      if (!wb) return;
      if (s.vm_reachable === false) { wb.textContent = '⚠ VM 不可达，无法判定野指标'; return; }
      if (!d.wild || d.wild.length === 0) { wb.innerHTML = '✅ 无野指标'; return; }
      wb.style.textAlign = 'left';
      wb.innerHTML = d.wild.map(function (n) {
        return '<code style="display:inline-block;margin:2px 4px 2px 0;padding:2px 6px;background:var(--bg2,#f5f5f5);border-radius:4px;font-size:11px">' + escHtml(n) + '</code>';
      }).join('')
        + (wildTotal > d.wild.length ? '<div style="margin-top:8px;color:var(--muted);font-size:11px">…共 ' + wildTotal + ' 条（本页展示前 ' + d.wild.length + '）</div>' : '');
    });
  };

  // ---------- ② 平台健康 ----------
  var _phTimer = null;
  window.renderPlatformHealth = function () {
    var html = '';
    html += '<div class="card"><div class="card-bd" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">'
      + '<div id="ph-verdict" style="font-size:14px;font-weight:600">⏳ 平台自检中...</div>'
      + '<span id="ph-meta" style="font-size:11px;color:var(--muted);margin-left:auto"></span>'
      + '<button class="btn btn-o btn-sm" onclick="renderPlatformHealth()">↻ 重新自检</button></div></div>';
    // 组件分层明细（逐项可下钻语义：状态 + 人话说明）
    html += '<div class="card" style="margin-top:16px"><div class="card-hd">🧩 组件健康 <span style="font-weight:400;font-size:11px;color:var(--muted)">L0 控制面 / 目录存储 / Agent 心跳 / VM 数据链 · 任一子项探活失败不阻断整体观测</span></div>';
    html += '<div class="card-bd" id="ph-comps" style="text-align:center;color:var(--muted);padding:16px">自检中...</div></div>';
    html += '<div class="card" style="margin-top:16px"><div class="card-bd" style="font-size:11px;color:var(--muted)">口径说明：心跳新鲜阈值 120s（与对账/自监控一致）；status 由组件多数派汇总——任一 fail → down，有 degraded 或断采/野指标/僵尸 → degraded。本页纯只读，不下发任何指令。</div></div>';
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
  }

})();
