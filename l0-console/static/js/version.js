// 「版本与兼容性」页：版本清单 + 兼容矩阵编辑 + 环境匹配预览。
// 零硬编码约定：字段、状态枚举、匹配结论全部来自后端（/api/versions、/api/versions/match），
// 本页只做渲染与提交；所有动态值经 vEsc（含引号转义）后再进 HTML。
(function () {
  var _versions = [];
  var _statusOptions = [];

  function vEsc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function vAttr(s) { return vEsc(s).replace(/\n/g, '&#10;'); }

  // 状态词表在前端只做显示翻译，值仍是后端契约（draft/stable/…）——中文只为少一次心算
  function statusLabel(s) {
    switch (s) {
      case 'draft': return '草稿（未发布）';
      case 'stable': return '已发布';
      case 'deprecated': return '已弃用';
      case 'disabled': return '已停用';
      default: return s || '—';
    }
  }

  function compsLabel(list) {
    var out = (list || []).map(function (c) {
      return c.static ? (c.name + '（静态）') : (c.name + ' ≥ glibc ' + (c.min_glibc || '?'));
    });
    return out.length ? out.join('；') : '—';
  }

  function renderVersions() {
    fetch(API + '/versions').then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { toast(d.error, 'err'); return; }
      _versions = d.versions || [];
      _statusOptions = d.status_options || [];
      var html = uploadCard() + '<div class="card"><div class="card-hd">🧬 版本与兼容性</div><div class="card-bd">';
      html += '<div style="margin-bottom:8px;color:var(--muted);font-size:12px">兼容判据：'
        + '架构 + 最低内核 + 包内组件要求。装机/升级时平台按目标机实测环境自动选定版本；'
        + '无兼容版本会拦住并说明差哪一项。</div>';
      html += '<table style="font-size:12px"><thead><tr><th>tag</th><th>版本</th><th>架构</th>'
        + '<th>状态</th><th>内核下限</th><th>组件要求</th><th>来源</th><th>更新时间</th><th></th></tr></thead><tbody>';
      if (!_versions.length) {
        html += '<tr><td colspan="9" style="text-align:center;color:var(--muted)">'
          + '清单为空：请把 SAgent 二进制放进 data/binaries/<tag>/ 并在 data/versions.yaml 登记</td></tr>';
      }
      _versions.forEach(function (v) {
        html += '<tr><td>' + vEsc(v.tag) + '</td><td>' + vEsc(v.version) + '</td><td>' + vEsc(v.arch) + '</td>'
          + '<td>' + vEsc(statusLabel(v.status)) + '</td><td>' + vEsc(v.min_kernel || '—') + '</td>'
          + '<td>' + vEsc(compsLabel(v.components)) + '</td><td>' + vEsc(v.source) + '</td>'
          + '<td>' + vEsc(v.updated_at || '—') + '</td>'
          + '<td style="white-space:nowrap">'
          + '<button class="btn btn-o btn-sm" onclick="editVersion(\'' + vAttr(v.tag) + '\')">编辑矩阵</button> '
          + '<button class="btn btn-o btn-sm" onclick="downloadVersion(\'' + vAttr(v.tag) + '\')">下载</button> '
          + actionBtn(v)
          + '</td></tr>';
      });
      html += '</tbody></table></div></div>';
      html += matchPreviewCard();
      document.getElementById('main-content').innerHTML = html;
    }).catch(function (e) { toast('版本清单加载失败：' + (e && e.message || e), 'err'); });
  }

  function matchPreviewCard() {
    return '<div class="card" style="margin-top:12px"><div class="card-hd">🎯 匹配预览</div><div class="card-bd">'
      + '<div style="color:var(--muted);font-size:12px;margin-bottom:8px">'
      + '选一台资源，用它探路实测的环境跑一次自动选版：会选哪个、为什么、其它候选为何落选</div>'
      + '<div style="display:flex;gap:8px;align-items:center">'
      + '<input id="vm-res" placeholder="资源 ID（如 ip-10-1-207-156）" '
      + 'style="padding:6px 8px;border:1px solid var(--border);border-radius:4px;width:260px">'
      + '<button class="btn btn-p btn-sm" onclick="previewVersionMatch()">预览</button></div>'
      + '<div id="vm-out" style="margin-top:10px;font-size:12px"></div></div></div>';
  }

  function previewVersionMatch() {
    var el = document.getElementById('vm-res');
    var id = el ? el.value.trim() : '';
    if (!id) { toast('请输入资源 ID', 'err'); return; }
    fetch(API + '/versions/match', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ resource_id: id })
    }).then(function (r) { return r.json(); }).then(function (d) {
      var host = document.getElementById('vm-out');
      if (!host) return;
      if (d.error) { host.innerHTML = '<span style="color:var(--err)">' + vEsc(d.error) + '</span>'; return; }
      var env = d.env || {};
      var html = '<div>实测环境：arch=' + vEsc(env.arch || '-')
        + '，内核=' + vEsc(env.kernel || '未采到')
        + '，glibc=' + vEsc(env.glibc || '未采到') + '</div>';
      if (d.compatible) {
        html += '<div style="color:var(--ok);margin:4px 0">✅ 会选：' + vEsc((d.chosen || {}).tag)
          + ' —— ' + vEsc(d.reason || '') + '</div>';
      } else {
        html += '<div style="color:var(--err);margin:4px 0">⛔ 无兼容版本（装机/升级会被拦住，不会硬装）</div>';
      }
      html += '<table style="margin-top:6px"><thead><tr><th>候选</th><th>结论</th><th>原因</th></tr></thead><tbody>';
      (d.candidates || []).forEach(function (c) {
        html += '<tr><td>' + vEsc(c.tag) + '</td><td>' + (c.compatible ? '兼容' : '不兼容')
          + (c.unverified ? '（未核实）' : '') + '</td><td>' + vEsc(c.why || '—') + '</td></tr>';
      });
      html += '</tbody></table>';
      host.innerHTML = html;
    }).catch(function (e) { toast('预览失败：' + (e && e.message || e), 'err'); });
  }

  function editVersion(tag) {
    var v = (_versions || []).filter(function (x) { return x.tag === tag; })[0];
    if (!v) return;
    var statusOpts = _statusOptions.length ? _statusOptions : ['stable', 'deprecated', 'disabled'];
    renderOverlay('🧬 编辑兼容矩阵 · ' + tag, function () {
      var h = '<div style="font-size:13px;display:grid;gap:10px">';
      h += '<div><b>最低内核</b>（留空 = 无约束）<br><input id="ve-kernel" value="' + vAttr(v.min_kernel || '')
        + '" placeholder="例: 3.10" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div>';
      h += '<div><b>状态</b><br><select id="ve-status" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
        + statusOpts.map(function (s) {
          return '<option value="' + vAttr(s) + '"' + (v.status === s ? ' selected' : '') + '>' + vEsc(statusLabel(s)) + '</option>';
        }).join('') + '</select></div>';
      h += '<div><b>包内组件要求</b>（每行一条：组件名,最低glibc；静态组件写 组件名,static）<br>'
        + '<textarea id="ve-comps" rows="3" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
        + vEsc((v.components || []).map(function (c) {
          return c.static ? (c.name + ',static') : (c.name + ',' + (c.min_glibc || ''));
        }).join('\n')) + '</textarea></div>';
      h += '<div><b>说明</b><br><input id="ve-notes" value="' + vAttr(v.notes || '')
        + '" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div>';
      h += '</div><div style="margin-top:14px;display:flex;gap:8px">'
        + '<button class="btn btn-p" onclick="saveVersion(\'' + vAttr(tag) + '\')">💾 保存</button>'
        + '<button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
      return h;
    });
  }

  function saveVersion(tag) {
    var ta = document.getElementById('ve-comps');
    var comps = ((ta && ta.value) || '').split('\n').map(function (line) {
      var p = line.split(',').map(function (s) { return s.trim(); });
      if (!p[0]) return null;
      return p[1] === 'static' ? { name: p[0], static: true } : { name: p[0], min_glibc: p[1] || '' };
    }).filter(Boolean);
    fetch(API + '/versions', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        tag: tag,
        min_kernel: (document.getElementById('ve-kernel') || {}).value || '',
        status: (document.getElementById('ve-status') || {}).value || '',
        notes: (document.getElementById('ve-notes') || {}).value || '',
        components: comps
      })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { toast(d.error, 'err'); return; }
      closeOverlay(); toast('兼容矩阵已保存（立即生效）', 'ok'); renderVersions();
    }).catch(function (e) { toast('保存失败：' + (e && e.message || e), 'err'); });
  }

  // 状态迁移按钮：draft→stable（发布）→deprecated（弃用）→disabled（停用）；disabled 可重新发布。
  // draft 不参与自动选版，所以"发布"是一道真实的人工闸门
  function statusAction(status) {
    switch (status) {
      case 'draft': return { label: '发布', to: 'stable' };
      case 'stable': return { label: '弃用', to: 'deprecated' };
      case 'deprecated': return { label: '停用', to: 'disabled' };
      default: return { label: '重新发布', to: 'stable' };
    }
  }

  function actionBtn(v) {
    var a = statusAction(v.status);
    return '<button class="btn btn-p btn-sm" onclick="setVersionStatus(\'' + vAttr(v.tag) + '\',\'' + vAttr(a.to)
      + '\')">' + vEsc(a.label) + '</button>';
  }

  function uploadCard() {
    return '<div class="card" style="margin-bottom:12px"><div class="card-hd">⬆️ 上传版本包</div><div class="card-bd">'
      + '<div style="color:var(--muted);font-size:12px;margin-bottom:8px">'
      + '平台会核对 ELF 架构（tag 声明的架构必须与包一致）并计算 sha256；'
      + '上传后状态为「草稿（未发布）」，点「发布」后才参与自动选版</div>'
      + '<div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">'
      + '<input id="vu-tag" placeholder="tag（如 linux-arm64-0.4.2）" '
      + 'style="padding:6px 8px;border:1px solid var(--border);border-radius:4px;width:240px">'
      + '<input id="vu-file" type="file" style="font-size:12px">'
      + '<button class="btn btn-p btn-sm" onclick="uploadVersion()">上传</button></div>'
      + '</div></div>';
  }

  function uploadVersion() {
    var tag = ((document.getElementById('vu-tag') || {}).value || '').trim();
    var files = (document.getElementById('vu-file') || {}).files || [];
    if (!tag || !files[0]) { toast('请填写 tag 并选择文件', 'err'); return; }
    var fd = new FormData();
    fd.append('tag', tag);
    fd.append('file', files[0]);
    fetch(API + '/versions/upload', { method: 'POST', body: fd })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d.error) { toast(d.error, 'err'); return; }
        toast('已上传 ' + d.tag + '（草稿，需发布才参与选版）', 'ok');
        renderVersions();
      })
      .catch(function (e) { toast('上传失败：' + (e && e.message || e), 'err'); });
  }

  function downloadVersion(tag) {
    window.location.href = API + '/versions/download?tag=' + encodeURIComponent(tag);
  }

  function setVersionStatus(tag, to) {
    if (!window.confirm('确认把 ' + tag + ' 置为「' + statusLabel(to) + '」？')) return;
    fetch(API + '/versions', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tag: tag, status: to })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { toast(d.error, 'err'); return; }
      toast(tag + ' → ' + statusLabel(to), 'ok');
      renderVersions();
    }).catch(function (e) { toast('操作失败：' + (e && e.message || e), 'err'); });
  }

  window.renderVersions = renderVersions;
  window.editVersion = editVersion;
  window.saveVersion = saveVersion;
  window.previewVersionMatch = previewVersionMatch;
  window.uploadVersion = uploadVersion;
  window.downloadVersion = downloadVersion;
  window.setVersionStatus = setVersionStatus;
})();
