            function openAddWizard() {
        var modal = document.getElementById("add-agent-modal");
        if (!modal) return;
        modal.classList.add("open");
        renderInstallView();
      }
      function renderInstallView() {
        onboardCfg(function(cfg) {
          var templates = cfg.agent_types.map(function(t){
            return {id: t.id, name: t.name, desc: t.desc, plugins: t.plugins.join(' + ') || '自定义组合'};
          });
          var body = document.getElementById('install-body');
          if (!body) return;
        var h = '<div style="font-size:15px;font-weight:600;margin-bottom:16px">🤖 安装部署 Agent</div>';
        // Section 1: Profile
        h += '<div style="margin-bottom:16px"><b style="font-size:12px">① 选择部署模板（定义 Agent 角色）</b>';
        h += '<div style="display:flex;gap:8px;margin-top:8px">';
        templates.forEach(function(t,i){
          h += '<div class="tmpl-card" onclick="selectInstallTemplate('+i+')" id="tmpl-'+i+'">';
          h += '<div class="tmpl-name">'+t.name+'</div><div class="tmpl-desc">'+t.desc+'</div>';
          h += '<div style="font-size:11px;color:var(--muted);margin-top:6px">'+t.plugins+'</div></div>';
        });
        h += '</div><input type="hidden" id="install-tmpl" value="0"></div>';
        // Section 2: IDC + tags
        h += '<div style="margin-bottom:16px">';
        h += '<b style="font-size:12px">② 部署范围</b>';
        h += '<div style="display:flex;gap:8px;margin-top:8px;flex-wrap:wrap">';
        h += '<input id="install-idc" placeholder="IDC 标识" value="idc-a" style="width:160px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px">';
        h += '<input id="install-tags" placeholder="标签（逗号分隔）" value="prod" style="width:240px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px">';
        h += '</div></div>';
        // Section 3: Generated command
        h += '<div style="margin-bottom:12px"><b style="font-size:12px">③ 安装命令</b>';
        h += '<div style="margin-top:6px;background:#1e1e1e;border-radius:6px;padding:12px;position:relative">';
        h += '<pre id="install-cmd" style="margin:0;font-size:12px;color:#d4d4d4;white-space:pre-wrap">选择一个模板生成安装命令</pre>';
        h += '<button class="btn btn-o btn-sm" style="position:absolute;top:8px;right:8px" onclick="genInstallCmd()">🔄 生成</button>';
        h += '<button class="btn btn-p btn-sm" style="position:absolute;top:8px;right:80px" onclick="copyInstallCmd()">📋 复制</button>';
        h += '</div></div>';
        // Section 4: Recently registered
        h += '<div><b style="font-size:12px">④ 最近自动注册的 Agent</b>';
        h += '<div id="install-new-agents" style="margin-top:6px;font-size:12px;color:var(--muted)">查看 Agent 列表...</div></div>';
        // Close
        h += '<div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--border);text-align:right"><button class="btn btn-o" onclick="document.getElementById(\'add-agent-modal\').classList.remove(\'open\')">关闭</button></div>';
        body.innerHTML = h;
        // Find recently registered
        var recently = agents.filter(function(a){return a.version==='unknown'||!a.version});
        var list = document.getElementById('install-new-agents');
        if (recently.length > 0 && list) {
          list.innerHTML = recently.slice(0,5).map(function(a){return '<div style="padding:4px 0">🔹 '+a.name+' <span style="color:var(--muted)">'+a.ip+'</span></div>'}).join('') + '<div style="margin-top:4px"><a href="#" onclick="closeModal(\'add-agent-modal\');goPage(\'agent-list\')" style="color:var(--primary)">查看全部 →</a></div>';
        }
        });
      }
      function selectInstallTemplate(idx) {
        document.querySelectorAll('.tmpl-card').forEach(function(c,i){c.classList.toggle('active',i===idx)});
        document.getElementById('install-tmpl').value = idx;
        genInstallCmd();
      }
      function genInstallCmd() {
        var idx = parseInt(document.getElementById('install-tmpl').value) || 0;
        var idc = document.getElementById('install-idc').value || 'default';
        var tags = document.getElementById('install-tags').value || 'default';
        var origin = location.origin;
        // Agent 类型捆绑由后端下发（onboard/config），前端不写死插件清单
        var tpl = (_onboardCfg && _onboardCfg.agent_types || [])[idx] || {};
        var plugins = JSON.stringify(tpl.plugins || []);
        var body = '{"id":"","type":"'+(tpl.id||'')+'","version":"dev","ip":"","plugins":'+plugins+',"labels":{"idc":"'+idc+'","env":"'+tags+'"}}';
        var cmd = '# ① 注册：在目标主机执行（Agent 自动出现在本平台清单）\n';
        cmd += 'curl -sS -X POST ' + origin + '/api/agent/register \\\n';
        cmd += '  -H "Content-Type: application/json" -d \'' + body + '\'\n\n';
        cmd += '# ② 心跳：Agent 侧每 30s 上报（存活状态 / 版本 / 生效配置 / 采集成败）\n';
        cmd += 'curl -sS -X POST ' + origin + '/api/agent/heartbeat \\\n';
        cmd += '  -H "Content-Type: application/json" \\\n';
        cmd += '  -d \'{"id":"<注册返回的agent_id>","version":"dev","config_version":0,"stats":{"success":0,"fail":0}}\'\n';
        document.getElementById('install-cmd').textContent = cmd;
        window._lastCmd = cmd;
      }
      function copyInstallCmd() {
        if (!window._lastCmd) { genInstallCmd(); }
        navigator.clipboard.writeText(window._lastCmd).then(function(){alert('已复制安装命令')}).catch(function(){alert('复制失败，请手动选中复制')});
      }
      function closeModal(id) { document.getElementById(id).classList.remove('open'); }

function renderTasks() {
        var html = '<div class="card"><div class="card-hd">📋 任务历史 <span style="font-weight:400;font-size:11px;color:var(--muted)">批量操作执行记录</span></div><div class="card-bd">';
        html += '<div style="margin-bottom:8px;font-size:12px"><span style="color:var(--muted)" id="tasks-count">加载中...</span></div>';
        html += '<table style="font-size:12px"><thead><tr><th>时间</th><th>操作类型</th><th>目标</th><th>范围</th><th>结果</th></tr></thead><tbody id="tasks-tbody">';
        html += '</tbody></table></div></div>';
        document.getElementById("main-content").innerHTML = html;
        fetch(API+"/tasks").then(function(r){return r.json()}).then(function(tasks){
          var tbody = document.getElementById('tasks-tbody');
          if (!tbody) return;
          if (tasks.length===0) {
            tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;color:var(--muted)">暂无任务记录</td></tr>';
          } else {
            tbody.innerHTML = tasks.reverse().map(function(t){
              var badge = t.result.indexOf('失败')>=0 ? 'b-o' : 'b-h';
              return '<tr><td style="white-space:nowrap">'+t.time+'</td><td><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+t.action+'</span></td><td>'+t.target+'</td><td>'+t.scope+'</td><td><span class="badge '+badge+'">'+t.result+'</span></td></tr>';
            }).join('');
          }
          var cnt = document.getElementById('tasks-count');
          if (cnt) cnt.textContent = '共 '+tasks.length+' 条操作记录';
        }).catch(function(){});
      }

      // 采集健康 = 对账工作台（2026-09-22 定稿）：一屏回答"有没有问题、问题在哪、接下来点什么"。
      // 顶部一句人话结论；三张问题卡锚到三个清单，清单行带处理动作。
      // 存活率/采集成功率百分比卡与 IDC 分布删掉（总览页已有 Agent 状态，本页只留对账语义）；
      // 批量操作条移至资源页（batchOp 留在 panel.js，fleet 页共用）。
      function renderSLO() {
        var anomaly = agents.filter(function(a){return a.status!=='healthy'&&a.status!=='running'})
          .filter(function (a) { return window.tenantVisible(a); }); // 多租户(D3)界面隔离
        var html = '';
        // ① 结论条（对账结果回来后填一句人话）+ 证据链概要（共享 buildEvidenceChainHTML，与工作台同源）
        html += '<div class="card"><div class="card-bd" style="display:flex;align-items:baseline;gap:16px;flex-wrap:wrap">';
        html += '<div id="recon-verdict" style="font-size:14px;font-weight:600">⏳ 对账引擎计算中...</div>';
        html += '<div id="recon-meta" style="font-size:11px;color:var(--muted);margin-left:auto"></div>';
        html += '<div id="recon-evid" style="flex:1 1 100%;margin-top:10px"></div>';
        html += '</div></div>';
        // ② 三张问题卡（点击定位到清单）
        html += '<div class="slo-grid" style="margin-top:16px">';
        html += '<div class="slo-card" style="cursor:pointer" onclick="document.getElementById(\'recon-stale\').scrollIntoView({behavior:\'smooth\'})"><div class="slo-val" id="slo-stale">⏳</div><div class="slo-label">数据断采 &gt; 10min</div></div>';
        html += '<div class="slo-card" style="cursor:pointer" onclick="document.getElementById(\'recon-wild\').scrollIntoView({behavior:\'smooth\'})"><div class="slo-val" id="slo-wild">⏳</div><div class="slo-label">野指标（VM 有 / 应报口径无）</div></div>';
        html += '<div class="slo-card" style="cursor:pointer" onclick="document.getElementById(\'recon-anomaly\').scrollIntoView({behavior:\'smooth\'})"><div class="slo-val" style="color:'+(anomaly.length>0?'var(--error)':'var(--success)')+'">'+anomaly.length+'</div><div class="slo-label">异常 Agent</div></div>';
        html += '</div>';
        // ③-1 断采清单
        html += '<div class="card" style="margin-top:16px" id="recon-stale"><div class="card-hd">📡 断采目标 <span style="font-weight:400;font-size:11px;color:var(--muted)">目录声明应采 × VM 实际在报（近 10min 无数据）</span></div>';
        html += '<div class="card-bd" id="recon-stale-body" style="text-align:center;color:var(--muted);padding:16px">对账引擎计算中...</div></div>';
        // 对账：野指标
        html += '<div class="card" style="margin-top:16px" id="recon-wild"><div class="card-hd">🌿 野指标 <span style="font-weight:400;font-size:11px;color:var(--muted)">VM 在报但应报口径未覆盖（目录注册 ∪ Agent 生效配置展开，前 50 条）</span></div>';
        html += '<div class="card-bd" id="recon-wild-body" style="text-align:center;color:var(--muted);padding:16px">对账引擎计算中...</div></div>';
        // ③-3 异常 Agent
        html += '<div class="card" style="margin-top:16px" id="recon-anomaly"><div class="card-hd">⚠️ 异常 Agent（'+anomaly.length+'）</div>';
        if(anomaly.length===0){
          html += '<div class="card-bd" style="text-align:center;color:var(--muted);padding:20px">✅ 全部正常</div>';
        }else{
          html += '<div class="card-bd" style="padding:0"><table><thead><tr><th>ID</th><th>类型</th><th>状态</th><th>IDC</th><th>操作</th></tr></thead><tbody>';
          anomaly.forEach(function(a){html += '<tr><td style="color:var(--primary);cursor:pointer" onclick="openDetailById(\''+a.id+'\')">'+a.id+'</td><td>'+(a.type==='proxy'?'Proxy':'Edge')+'</td><td><span class="badge '+(a.status==='stopped'?'b-s':'b-o')+'">'+(a.status==='stopped'?'已停':'离线')+'</span></td><td>'+((a.labels||{}).idc||'-')+'</td><td><button class="btn btn-s btn-sm" onclick="quickStart(\''+a.id+'\')">启动</button></td></tr>'});
          html += '</tbody></table>';
        }
        html += '</div>';
        // ③-4 HA-3 归属迁移记录（证据链：跨 L1 归属故障上收后，此处留痕供审计/排障下钻）
        html += '<div class="card" style="margin-top:16px" id="recon-relocate"><div class="card-hd">↹ HA-3 归属迁移记录 <span style="font-weight:400;font-size:11px;color:var(--muted)">跨 L1 归属故障上收证据：Exporter 归属从失效 SAgent 迁至同租户同地域健康端（冷却期内不重复迁移）</span></div>';
        html += '<div class="card-bd" id="recon-relocate-body" style="text-align:center;color:var(--muted);padding:16px">查询中...</div></div>';
        document.getElementById("main-content").innerHTML = html;
        // HA-3 归属迁移记录（证据链下钻，独立轻量请求不阻塞对账主链）
        fetch(API+'/relocations').then(function(r){return r.json()}).catch(function(){return {relocations:[]}}).then(function(rd){
          var rb = document.getElementById('recon-relocate-body');
          if (!rb) return;
          var rels = rd.relocations || [];
          if (!rels.length) { rb.innerHTML = '✅ 暂无归属迁移记录（跨 L1 归属稳定，未发生故障上收）'; rb.style.color='var(--muted)'; return; }
          rb.style.textAlign='left';
          rb.innerHTML = '<table style="text-align:left"><thead><tr><th>时间</th><th>目标</th><th>租户</th><th>地域</th><th>原属主</th><th>→ 新属主</th><th>结果</th><th>原因</th></tr></thead><tbody>'
            + rels.map(function(x){
                var st = x.Status==='moved' ? '<span class="badge" style="background:#e0f2f1;color:#0b6e66">已迁移</span>' : (x.Status==='skipped' ? '<span class="badge" style="background:#fff3cd;color:#8a6d3b">冷却跳过</span>' : '<span class="badge" style="background:#fde8e8;color:#b91c1c">拒绝</span>');
                var ts = x.CreatedAt ? new Date(x.CreatedAt*1000).toLocaleString('zh-CN',{hour12:false}) : '-';
                return '<tr><td style="white-space:nowrap">'+ts+'</td><td>'+escHtml(x.TargetName||x.TargetID)+'</td><td>'+escHtml(x.TenantID||'default')+'</td><td>'+(x.Region?escHtml(x.Region):'-')+'</td><td>'+(x.FromAgent?escHtml(x.FromAgent):'-')+'</td><td>'+(x.ToAgent?escHtml(x.ToAgent):'-')+'</td><td>'+st+'</td><td style="font-size:11px;color:var(--muted)">'+escHtml(x.Reason||x.Note||'')+'</td></tr>';
              }).join('')
            + '</tbody></table>'
            + '<div style="margin-top:8px;font-size:11px;color:var(--muted)">迁移记录经 config 轨接管（先新后旧防双写）+ 冷却终态保护，供审计与排障留痕。</div>';
        });
        // 对账结果 + 目标↔资源反查（断采行要给"打开资源"动作）
        Promise.all([
          fetch(API+'/recon').then(function(r){return r.json()}).catch(function(){return null}),
          fetch(API+'/targets').then(function(r){return r.json()}).catch(function(){return []}),
          fetch(API+'/resources').then(function(r){return r.json()}).catch(function(){return {}}),
          fetch(API+'/agents').then(function(r){return r.json()}).catch(function(){return []}),
          fetch(API+'/collect-stats').then(function(r){return r.json()}).catch(function(){return {}})
        ]).then(function(rs){
          var d = rs[0], tgts = rs[1]||[], resList = (rs[2]||{}).resources||[];
          // 证据链数据（与工作台 resFetchAll 同源口径）：agents / collect-stats 归一化进 dE
          var ag = rs[3]||[], cstat = rs[4]||{};
          var dE = {
            agents: Array.isArray(ag) ? ag : (ag.agents||[]),
            recon: d,
            stats: {}
          };
          (cstat.resources||[]).forEach(function(s){ if(s.resource_id) dE.stats[s.resource_id]=s; if(s.instance) dE.stats[s.instance]=s; });
          var evWrap = document.getElementById('recon-evid');
          if (evWrap && typeof globalThis.buildEvidenceChainHTML === 'function') {
            // 就地归因：点击证据链数据侧缺口(源采集/链路/入库)时，直接滚动定位到本页对应问题卡并高亮。
            // kind: stale(断采) / wild(野指标) / transport / anomaly
            globalThis.__sloDrill = function (kind) {
              var map = { stale: 'recon-stale', wild: 'recon-wild', transport: 'recon-verdict', anomaly: 'recon-anomaly' };
              var id = map[kind] || map.stale;
              var el = document.getElementById(id);
              if (!el) return;
              el.scrollIntoView({ behavior: 'smooth', block: 'start' });
              var old = el.style.boxShadow, oldB = el.style.borderColor;
              el.style.boxShadow = '0 0 0 3px rgba(217,119,6,.45)';
              el.style.borderColor = 'var(--warning,#d97706)';
              setTimeout(function () {
                el.style.boxShadow = old; el.style.borderColor = oldB;
              }, 2200);
            };
            evWrap.innerHTML = globalThis.buildEvidenceChainHTML(dE);
          }
          var tgRes = {};
          tgts.forEach(function(t){ tgRes[t.id] = t.resource_id; });
          var resNames = {};
          resList.forEach(function(x){ resNames[x.id] = x.name || x.ip || x.id; });
          var meta = document.getElementById('recon-meta');
          var sb0 = document.getElementById('recon-stale-body');
          var wb0 = document.getElementById('recon-wild-body');
          var v = document.getElementById('recon-verdict');
          if (!d || !d.summary) {
            if (v) v.textContent = '⚠ 对账引擎暂不可用，无法判断"数据是否在报"';
            ['slo-stale','slo-wild'].forEach(function(id){ var el=document.getElementById(id); if(el){ el.textContent='?'; el.style.color='var(--muted)'; } });
            if (sb0) sb0.textContent = '对账引擎暂不可用';
            if (wb0) wb0.textContent = '对账引擎暂不可用';
            return;
          }
          var st = d.summary.stale||0, wild = d.summary.wild||0;
          var vmOk = !!d.summary.vm_reachable;
          // VM 不可达时断采/野指标不可判（全量误报"断采"、空清单误读"无野指标"），显示 —
          var el1 = document.getElementById('slo-stale');
          if (el1) { el1.textContent = vmOk ? st : '—'; el1.style.color = (vmOk&&st>0)?'var(--error)':'var(--success)'; }
          var el4 = document.getElementById('slo-wild');
          if (el4) { el4.textContent = vmOk ? wild : '—'; el4.style.color = (vmOk&&wild>0)?'var(--warning,#b45309)':'var(--success)'; }
          if (meta) meta.textContent = '对账时间 '+(d.generated_at||'')+' · 检查目标 '+(d.summary.checked||0)+'/'+(d.summary.targets||0)+' · VM '+(vmOk?'可达':'不可达');
          // ① 一句人话结论
          if (v) {
            if (!vmOk) {
              v.textContent = '⚠ VM 不可达，本次无法判断"数据是否在报"，请先检查 VictoriaMetrics';
              v.style.color = 'var(--error)';
            } else if (st>0 || wild>0 || anomaly.length>0) {
              var parts = [];
              if (st>0) parts.push('<span style="color:var(--error)">断采 '+st+'</span>');
              if (wild>0) parts.push('<span style="color:var(--warning,#b45309)">野指标 '+wild+'</span>');
              if (anomaly.length>0) parts.push('<span style="color:var(--error)">异常 Agent '+anomaly.length+'</span>');
              v.innerHTML = '发现 '+parts.length+' 类问题待处理：'+parts.join('、')+'（点击下方卡片定位）';
              v.style.color = '';
            } else if ((d.summary.checked||0) > 0) {
              v.innerHTML = '✅ 平台健康：已核对 '+(d.summary.checked||0)+' 个目标全部在报，VM 无野指标，Agent 无异常';
              v.style.color = '';
            } else {
              v.innerHTML = '✅ 未发现问题，但本次没有可核对的目标（声明 '+(d.summary.targets||0)+' 个，缺插件口径或未注册）——可到 <a href="javascript:void(0)" onclick="goPage(\'targets\')" style="color:var(--primary)">采集目标</a> 检查目标配置';
              v.style.color = '';
            }
          }
          // 断采清单（带"打开资源"动作）
          var sb = document.getElementById('recon-stale-body');
          if (sb) {
            if (!vmOk) { sb.innerHTML = '⚠ VM 不可达，无法判定断采'; sb.style.color='var(--muted)'; }
            else if (!d.stale || d.stale.length===0) { sb.innerHTML = '✅ 全部在报'; sb.style.color='var(--muted)'; }
            else {
              var h = '<table style="text-align:left"><thead><tr><th>目标</th><th>地址</th><th>插件</th><th>分派 Agent</th><th>探测指标</th><th>操作</th></tr></thead><tbody>';
              d.stale.forEach(function(t){
                var rid = tgRes[t.id], act;
                if (!rid) act = '<span style="color:var(--muted)">—</span>';
                else if (resNames[rid]) act = '<a href="javascript:void(0)" onclick="openResourceDrawer(\''+rid+'\')" style="color:var(--primary)">打开资源</a>';
                else act = '<span style="color:var(--muted)">资源已删除</span>';
                h += '<tr><td>'+escHtml(t.name)+'</td><td><code>'+escHtml(t.address)+'</code></td><td><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+escHtml(t.plugin)+'</span></td><td>'+(t.agent_id?'<a href="javascript:void(0)" onclick="openDetailById(\''+t.agent_id+'\')" style="color:var(--primary)">'+escHtml(t.agent_id)+'</a>':'-')+'</td><td style="font-size:10px;color:var(--muted)">'+escHtml((t.probe_keys||[]).slice(0,2).join(', '))+'</td><td>'+act+'</td></tr>';
              });
              h += '</tbody></table>';
              sb.innerHTML = h; sb.style.color='';
            }
          }
          // 野指标清单 + 处理建议
          var wb = document.getElementById('recon-wild-body');
          if (wb) {
            if (!vmOk) { wb.innerHTML = '⚠ VM 不可达，无法判定野指标'; }
            else if (!d.wild || d.wild.length===0) { wb.innerHTML = '✅ 无野指标'; }
            else {
              wb.style.textAlign='left';
              wb.innerHTML = d.wild.map(function(n){return '<code style="display:inline-block;margin:2px 4px 2px 0;padding:2px 6px;background:var(--bg2,#f5f5f5);border-radius:4px;font-size:11px">'+escHtml(n)+'</code>'}).join('')
                + (d.summary.wild>50?'<div style="margin-top:8px;color:var(--muted);font-size:11px">…共 '+d.summary.wild+' 条</div>':'')
                + '<div style="margin-top:10px;font-size:12px;color:var(--muted)">处理建议：目录该有而没有的 → <a href="javascript:void(0)" onclick="goPage(\'capabilities\')" style="color:var(--primary)">去能力与指标登记</a>；确认不该采的 → 检查对应 Agent 生效配置。</div>';
            }
          }
        });
      }

function renderMetricsBrowse() {
        // 默认 VMUI：免登录、免建面板，打开就能查；Grafana 只留入口（登录后落到面板列表）。
        // 之前默认 iframe Grafana dashboard/new?editPanel=1 是个建面板的半成品页面，不是"看数据"
        siteCfg(function(cfg) {
          var vmuiUrl = cfg.vm_url + '/vmui/';
          var html = '<div class="card" style="height:calc(100vh - 130px);display:flex;flex-direction:column">';
          html += '<div class="card-hd">📈 指标浏览 <span class="tabs" style="margin-left:12px"><span class="active" onclick="switchMetricsTab(\'vmui\')">VMUI</span><span onclick="switchMetricsTab(\'grafana\')">Grafana</span></span></div>';
          html += '<div class="card-bd" style="flex:1;padding:0;overflow:hidden">';
          html += '<iframe id="metrics-frame" src="'+vmuiUrl+'" style="width:100%;height:100%;border:0"></iframe>';
          html += '</div></div>';
          document.getElementById("main-content").innerHTML = html;
        });
      }
      function switchMetricsTab(t) {
        document.querySelectorAll(".card-hd .tabs span").forEach(function(s){s.classList.toggle("active",s.textContent.toLowerCase().indexOf(t)>=0)});
        var frame = document.getElementById("metrics-frame");
        if (frame) siteCfg(function(cfg) {
          frame.src = t==='grafana' ? cfg.grafana_url + '/' : cfg.vm_url + '/vmui/';
        });
      }

      // ===================================================================
      //  PAGE: Agent List
      // ===================================================================
      function renderAgentList() {
        // 多租户(D3)界面隔离：当前租户非"全部"时，仅展示该租户的 Agent
        var scope = agents.filter(function (a) { return window.tenantVisible(a); });
        var html = '<div class="card"><div class="card-hd">📋 Agent 清单（'+scope.length+' / 共 '+agents.length+'）</div>';
        html += '<div class="card-bd">';
        html += '<div class="filter-bar"><input placeholder="搜索 ID / IP / 名称" oninput="renderAgentListFiltered(this.value)" style="flex:1;max-width:300px"><select onchange="renderAgentListFilteredByType(this.value)"><option value="all">全部类型</option><option value="edge">📡 边缘</option><option value="proxy">🔗 Proxy</option></select><select onchange="renderAgentListFilteredByStatus(this.value)"><option value="all">全部状态</option><option value="healthy">健康</option><option value="offline">离线</option><option value="stopped">已停</option></select></div>';
        html += '<table><thead><tr><th>Agent ID</th><th>名称</th><th>类型</th><th>状态</th><th>版本</th><th>IDC</th><th>插件</th><th>操作</th></tr></thead><tbody id="agent-list-tbody">';
        scope.forEach(function(a) {
          html += '<tr style="cursor:pointer" onclick="openDetailById(\''+a.id+'\')"><td style="color:var(--primary)">'+a.id+'</td><td>'+a.name+'</td><td>'+(a.type==='proxy'?'Proxy':'Edge')+'</td><td><span class="badge '+(a.status==='healthy'?'b-h':'b-s')+'">'+a.status+'</span></td><td>'+a.version+'</td><td>'+((a.labels||{}).idc||'-')+'</td><td>'+(a.plugins||[]).map(function(p){return '<span class="badge" style="background:#f0f0f0;color:#666;margin-right:2px">'+p+'</span>'}).join('')+'</td><td><button class="btn btn-o btn-sm" onclick="event.stopPropagation();quickStart(\''+a.id+'\')">启动</button></td></tr>';
        });
        html += '</tbody></table></div>';
        document.getElementById("main-content").innerHTML = html;
      }
      function renderAgentListFiltered(q) {
        var tbody = document.getElementById("agent-list-tbody"); if (!tbody) return;
        var rows = tbody.querySelectorAll("tr");
        rows.forEach(function(r){r.style.display=r.textContent.toLowerCase().indexOf(q.toLowerCase())>=0?'':'none'});
      }
      function renderAgentListFilteredByType(t){renderAgentListFilteredByCol(3,t)}
      function renderAgentListFilteredByStatus(s){renderAgentListFilteredByCol(4,s)}
      function renderAgentListFilteredByCol(col,val){
        var tbody = document.getElementById("agent-list-tbody"); if (!tbody) return;
        if (val==='all') { tbody.querySelectorAll("tr").forEach(function(r){r.style.display=''}); return; }
        tbody.querySelectorAll("tr").forEach(function(r){r.style.display=r.cells[col].textContent.indexOf(val)>=0?'':'none'});
      }

      // ===================================================================
      //  PAGE: Audit (Operation Log)
      // ===================================================================
            function renderAudit() {
        var html = '<div class="card"><div class="card-hd">📜 审计日志 <span style="font-weight:400;font-size:11px;color:var(--muted)">全平台操作记录</span></div><div class="card-bd">';
        html += '<div style="display:flex;gap:8px;margin-bottom:12px;flex-wrap:wrap;align-items:center">';
        html += '<input placeholder="搜索操作人/目标/结果..." oninput="auditFilter()" style="width:200px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px" id="audit-search">';
        html += '<select onchange="auditFilter()" id="audit-action" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="all">全部操作</option></select>';
        html += '<select onchange="auditFilter()" id="audit-result" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="all">全部结果</option><option value="成功">成功</option><option value="失败">失败</option></select>';
        html += '<span style="font-size:11px;color:var(--muted)" id="audit-count"></span>';
        html += '</div>';
        html += '<table style="font-size:12px;table-layout:fixed;width:100%"><thead><tr><th style="width:170px">时间</th><th style="width:90px">操作人</th><th style="width:150px">操作类型</th><th style="width:101px">目标</th><th style="width:100px">范围</th><th style="width:421px">结果</th></tr></thead><tbody id="audit-tbody">';
        html += '</tbody></table><div id="audit-pager" style="display:flex;align-items:center;justify-content:center;gap:10px;margin-top:12px;font-size:12px"></div></div></div>';
        document.getElementById("main-content").innerHTML = html;
        window._auditData = [];
        window._auditPage = 1;
        fetch(API+"/audit?limit=1000").then(function(r){return r.json()}).then(function(entries){
          window._auditData = entries || [];
          auditRender();
        }).catch(function(){ document.getElementById("audit-tbody").innerHTML = '<tr><td colspan="6" style="color:var(--error)">加载失败</td></tr>'; });
      }
      function auditFilter() { window._auditPage = 1; auditRender(); }
      function auditGoPage(d) { window._auditPage = Math.max(1, (window._auditPage||1) + d); auditRender(); }
      // 审计条目字段兼容（API 返回小写 key，旧数据可能为大写）
      function auditF(e) {
        return {
          time: e.time||e.Time||'', operator: e.operator||e.Operator||'',
          action: e.action||e.Action||'', target: e.target||e.Target||'',
          scope: e.scope||e.Scope||'', result: e.result||e.Result||''
        };
      }
      function auditRender() {
        var entries = window._auditData || [];
        var q = ((document.getElementById('audit-search')||{}).value||'').toLowerCase();
        var act = (document.getElementById('audit-action')||{}).value||'all';
        var res = (document.getElementById('audit-result')||{}).value||'all';
        // 操作类型下拉按真实数据动态生成
        var actSel = document.getElementById('audit-action');
        if (actSel && actSel.options.length <= 1) {
          [...new Set(entries.map(function(e){return auditF(e).action}).filter(Boolean))].sort().forEach(function(a){
            var o = document.createElement('option'); o.value = a; o.textContent = a; actSel.appendChild(o);
          });
          actSel.value = act;
        }
        var filtered = entries.map(auditF).filter(function(e){
          if (act!=='all' && e.action!==act) return false;
          if (res==='成功' && e.result.indexOf('失败')>=0) return false;
          if (res==='失败' && e.result.indexOf('失败')<0) return false;
          if (q && e.operator.toLowerCase().indexOf(q)<0 && e.target.toLowerCase().indexOf(q)<0 && e.result.toLowerCase().indexOf(q)<0 && e.scope.toLowerCase().indexOf(q)<0) return false;
          return true;
        }).reverse();
        // 前端分页（每页 20 条）：过滤后的全量在内存里，翻页不回服务端
        var ps = 20;
        var totalPages = Math.max(1, Math.ceil(filtered.length / ps));
        if ((window._auditPage||1) > totalPages) window._auditPage = totalPages;
        var page = window._auditPage || 1;
        var pageItems = filtered.slice((page-1)*ps, page*ps);
        var tbody = document.getElementById('audit-tbody');
        if (tbody) {
          if (filtered.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" style="text-align:center;color:var(--muted)">无匹配记录</td></tr>';
          } else {
          tbody.innerHTML = pageItems.map(function(e,i){
            var isFail = e.result.indexOf('失败')>=0;
            var badge = isFail ? 'b-o' : 'b-h';
            var badgeColor = isFail ? 'background:#fef2f2;color:#b91c1c' : 'background:#f0fdf4;color:#15803d';
            // 结果拆两段：状态徽章 + 详情文本（默认两行截断，点击展开全量）
            // 注意：全局 CSS 有 td{white-space:nowrap}，详情/目标列必须显式 white-space:normal 才能折行
            var detail = escHtml(e.result);
            var no = 'aud-' + Math.abs(hashStr(JSON.stringify(e) + i));
            return '<tr><td style="white-space:nowrap;overflow:hidden;text-overflow:ellipsis">'+escHtml(e.time)+'</td>'
              +'<td style="overflow:hidden;text-overflow:ellipsis" title="'+escHtml(e.operator)+'">'+escHtml(e.operator)+'</td>'
              +'<td style="overflow:hidden"><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+escHtml(e.action)+'</span></td>'
              +'<td style="white-space:normal;word-break:break-all" title="'+escHtml(e.target)+'">'+escHtml(e.target)+'</td>'
              +'<td style="white-space:normal;word-break:break-all">'+escHtml(e.scope)+'</td>'
              +'<td><div style="display:flex;align-items:flex-start;gap:6px;min-width:0">'
              +'<span class="badge '+badge+'" style="'+badgeColor+';flex-shrink:0">'+(isFail?'失败':'成功')+'</span>'
              +'<span id="'+no+'" title="'+detail+'" onclick="audToggle(\''+no+'\')" style="flex:1;min-width:0;cursor:pointer;white-space:normal;word-break:break-all;max-height:34px;overflow:hidden;color:var(--text)">'+detail+'</span>'
              +'</div></td></tr>';
          }).join('');
        }
        }
        var cnt = document.getElementById('audit-count');
        if (cnt) cnt.textContent = filtered.length === 0 ? '共 0 条'
          : '第 ' + ((page-1)*ps+1) + '-' + Math.min(page*ps, filtered.length) + ' 条 / 共 ' + filtered.length + ' 条';
        var pager = document.getElementById('audit-pager');
        if (pager) {
          pager.innerHTML = filtered.length === 0 ? '' :
            '<button class="btn btn-o btn-sm" '+(page<=1?'disabled':'')+' onclick="auditGoPage(-1)">上一页</button>'
            + '<span style="color:var(--muted)">第 '+page+' / '+totalPages+' 页 · 每页 '+ps+' 条</span>'
            + '<button class="btn btn-o btn-sm" '+(page>=totalPages?'disabled':'')+' onclick="auditGoPage(1)">下一页</button>';
        }
      }function verCmp(a, b) {
        // 语义化版本比较：v0.10.0 > v0.9.0（分段数值比较，修字典序 sort 的 v0.10 < v0.9 缺陷）；unknown 恒为最小
        if (a === b) return 0;
        if (a === 'unknown') return -1;
        if (b === 'unknown') return 1;
        var pa = String(a).replace(/^v/, '').split('.').map(Number);
        var pb = String(b).replace(/^v/, '').split('.').map(Number);
        for (var i = 0; i < Math.max(pa.length, pb.length); i++) {
          var x = pa[i] || 0, y = pb[i] || 0;
          if (x !== y) return x - y;
        }
        return 0;
      }
      function renderVersions() {
        var total = agents.length||1;
        // Version distribution
        var verMap = {};
        agents.forEach(function(a){var v=a.version||'unknown';verMap[v]=(verMap[v]||0)+1});
        // OS distribution (simulated - from labels)
        var osMap = {};
        agents.forEach(function(a){var os=(a.labels||{}).os||'linux-amd64';osMap[os]=(osMap[os]||0)+1});
        var archs = {'linux-amd64':0,'linux-arm64':0,'anolis-amd64':0,'anolis-arm64':0,'openeuler-amd64':0};
        Object.keys(osMap).forEach(function(k){if(archs.hasOwnProperty(k))archs[k]=osMap[k];else archs[k]=osMap[k]});

        var html = '';
        // Summary cards
        html += '<div style="display:grid;grid-template-columns:repeat(4,1fr);gap:10px;margin-bottom:16px">';
        html += '<div style="padding:14px;background:var(--card);border-radius:8px;text-align:center;box-shadow:var(--shadow)"><div style="font-size:24px;font-weight:700">'+total+'</div><div style="font-size:11px;color:var(--muted)">Agent 总数</div></div>';
        html += '<div style="padding:14px;background:var(--card);border-radius:8px;text-align:center;box-shadow:var(--shadow)"><div style="font-size:24px;font-weight:700">'+Object.keys(verMap).length+'</div><div style="font-size:11px;color:var(--muted)">运行版本数</div></div>';
        html += '<div style="padding:14px;background:var(--card);border-radius:8px;text-align:center;box-shadow:var(--shadow)"><div style="font-size:24px;font-weight:700">'+Object.keys(osMap).length+'</div><div style="font-size:11px;color:var(--muted)">操作系统类型</div></div>';
        var latest = Object.keys(verMap).filter(function(v){return v!=='unknown'}).sort(verCmp).pop()||'unknown';
        var onLatest = verMap[latest]||0;
        html += '<div style="padding:14px;background:var(--card);border-radius:8px;text-align:center;box-shadow:var(--shadow)"><div style="font-size:24px;font-weight:700;color:'+(onLatest===total?'var(--success)':'var(--warn)')+'">'+Math.round(onLatest/total*100)+'%</div><div style="font-size:11px;color:var(--muted)">已升级到最新 '+latest+'</div></div>';
        html += '</div>';

        // Two columns: version + OS
        html += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:16px">';
        // Version column
        html += '<div class="card"><div class="card-hd">📦 版本分布</div><div class="card-bd">';
        var verColors = {'v0.3.0':'#10b981','v0.3.1':'#0f766e','v0.4.0':'#f59e0b',unknown:'#d1d5db'};
        html += '<table style="font-size:12px;width:100%"><thead><tr><th>版本</th><th style="text-align:right">数量</th><th>占比</th><th>状态</th></tr></thead><tbody>';
        Object.keys(verMap).sort(verCmp).reverse().forEach(function(v){
          var pct = Math.round(verMap[v]/total*100);
          var status = v===latest?'<span class="badge b-h">最新</span>':(pct<10?'<span class="badge" style="background:#fef3c7;color:#f59e0b">老旧</span>':'<span class="badge b-r">稳定</span>');
          html += '<tr><td><span style="display:inline-block;width:10px;height:10px;border-radius:50%;background:'+(verColors[v]||'#d1d5db')+';margin-right:6px"></span><b>'+v+'</b></td><td style="text-align:right">'+verMap[v]+'</td><td><div style="display:flex;align-items:center;gap:8px"><div style="flex:1;height:6px;background:var(--bg);border-radius:3px;overflow:hidden"><div style="width:'+pct+'%;height:100%;background:'+(verColors[v]||'#0f766e')+';border-radius:3px"></div></div>'+pct+'%</div></td><td>'+status+'</td></tr>';
        });
        html += '</tbody></table></div>';
        // OS column
        html += '<div class="card"><div class="card-hd">💻 操作系统 / 架构</div><div class="card-bd">';
        html += '<table style="font-size:12px;width:100%"><thead><tr><th>平台</th><th style="text-align:right">数量</th><th>占比</th></tr></thead><tbody>';
        var osNames = {'linux-amd64':'🐧 Linux x86_64','linux-arm64':'🐧 Linux ARM64','anolis-amd64':'🦎 龙蜥 Anolis x86_64','anolis-arm64':'🦎 龙蜥 Anolis ARM64','openeuler-amd64':'🐉 欧拉 openEuler x86_64'};
        Object.keys(archs).forEach(function(os){
          var cnt = archs[os]||0;
          if (cnt===0) return;
          var pct = Math.round(cnt/total*100);
          html += '<tr><td>'+(osNames[os]||os)+'</td><td style="text-align:right">'+cnt+'</td><td><div style="display:flex;align-items:center;gap:8px"><div style="flex:1;height:6px;background:var(--bg);border-radius:3px;overflow:hidden"><div style="width:'+pct+'%;height:100%;background:#6366f1;border-radius:3px"></div></div>'+pct+'%</div></td></tr>';
        });
        html += '</tbody></table></div>';
        html += '</div>'; // end two columns

        // Upgrade plan
        var idcs = {};
        agents.forEach(function(a){var d=(a.labels||{}).idc||'unknown';idcs[d]=(idcs[d]||0)+1});
        html += '<div class="card" style="margin-top:16px"><div class="card-hd">🚀 灰度升级计划 <span style="font-weight:400;font-size:11px;color:var(--muted)">目标: '+latest+' → v0.4.0</span></div><div class="card-bd" style="padding:0">';
        html += '<table style="font-size:12px;width:100%"><thead><tr><th>批次</th><th>IDC</th><th>Agent 数</th><th>当前版本</th><th>目标版本</th><th>进度</th><th>操作</th></tr></thead><tbody>';
        var batch=1;
        Object.keys(idcs).sort().forEach(function(d){
          html += '<tr><td>Batch '+batch+'</td><td>'+d+'</td><td>'+idcs[d]+'</td><td>'+latest+'</td><td>v0.4.0</td><td><span class="badge" style="background:#f0f0f0;color:#999">待执行</span></td><td><button class="btn btn-p btn-sm">执行</button></td></tr>';
          batch++;
        });
        html += '</tbody></table></div>';

        document.getElementById("main-content").innerHTML = html;
      }
      
// ============ 插件能力目录（目录库驱动，全数据渲染） ============
var _catalog = {
  currentId: null, detail: null, metrics: [], filters: {types:[],units:[]},
  expanded: {}, dashList: [], dash: null, dashRange: '1h', dashVar: {}, dashPanels: []
};

function renderPluginsMart(hostId) {
  var host = hostId || 'main-content';
  var html = '<div class="card"><div class="card-hd">🔌 采集插件 <span style="font-weight:400;font-size:11px;color:var(--muted)">采集能力目录 · 配置化驱动</span> <button class="btn btn-o btn-sm" style="float:right" onclick="reimportCatalog()">⟳ 重新导入</button></div><div class="card-bd" id="plugin-main"><div style="text-align:center;color:var(--muted);padding:20px">⏳ 加载插件目录...</div></div></div>';
  document.getElementById(host).innerHTML = html;
  fetch(API+'/catalog/plugins').then(function(r){return r.json()}).then(function(list){
    window._catalogList = list || [];
    renderPluginGrid();
  }).catch(function(){ window._catalogList = []; renderPluginGrid(); });
}
function reimportCatalog() {
  toast('正在重新导入夜莺集成包...', 'ok');
  fetch(API+'/catalog/reimport',{method:'POST'}).then(function(r){return r.json()}).then(function(res){
    if (res.error) { toast('导入失败: '+res.error, 'err'); return; }
    // 读取移植变更报告：上游插件包升级时提示指标新增/移除
    fetch(API+'/catalog/sync-report').then(function(r){return r.json()}).then(function(rep){
      var added=0, removed=0, detail=[];
      (rep.diffs||[]).forEach(function(d){
        added += (d.added||[]).length; removed += (d.removed||[]).length;
        if ((d.added||[]).length || (d.removed||[]).length) detail.push(d.plugin+' +'+(d.added||[]).length+'/-'+(d.removed||[]).length);
      });
      toast('已导入 '+res.imported+' 个插件'
        + (added||removed ? '，指标变更：新增 '+added+' / 移除 '+removed+'（'+detail.join('、')+'）' : '，指标无变更'), 'ok');
      renderPluginsMart();
    }).catch(function(){ toast('已导入 '+res.imported+' 个插件', 'ok'); renderPluginsMart(); });
  }).catch(function(){ toast('导入失败', 'err'); });
}
function renderPluginGrid() {
  var P = window._catalogList || [];
  var el = document.getElementById('plugin-main');
  if (!el) return;
  var cats = [];
  P.forEach(function(p){ if (cats.indexOf(p.category)<0) cats.push(p.category); });
  var h = '<div style="display:flex;gap:8px;margin-bottom:16px;flex-wrap:wrap">';
  h += '<input placeholder="搜索插件..." oninput="pluginFilter()" style="width:200px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px" id="plugin-search">';
  h += '<select onchange="pluginFilter()" id="plugin-cat" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="all">全部分类</option>';
  cats.forEach(function(c){ h += '<option value="'+c+'">'+c+'</option>'; });
  h += '</select><span style="font-size:11px;color:var(--muted)">共 '+P.length+' 个插件</span></div>';
  h += '<div id="plugin-grid" style="display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:10px">';
  P.forEach(function(p){
    var icon = p.icon && p.icon.indexOf('/')>=0 ? '<img src="'+p.icon+'" style="width:32px;height:32px;border-radius:6px">' : '🧩';
    h += '<div class="plugin-card plugin-item" data-cat="'+p.category+'" style="cursor:pointer" onclick="showPluginDetail('+p.id+')">'
      + '<div class="plugin-icon">'+icon+'</div>'
      // 版本徽标只在后端给了真版本时渲染——集成包不带版本，空值就别占位（2026-09-22 界面评审）
      + '<div class="plugin-info"><div class="plugin-name">'+p.display_name+(p.version?' <span style="font-size:10px;color:var(--muted)">'+p.version+'</span>':'')+'</div>'
      + '<div class="plugin-desc">📏 '+p.metric_count+' 指标 · 📊 '+p.dash_count+' 仪表盘</div>'
      + '<div style="font-size:10px;color:var(--muted);margin-top:4px">'+p.category+'</div></div>'
      + '<div><span class="badge '+(p.source==='custom'?'b-s':'b-h')+'">'+(p.source==='custom'?'自定义':'内置')+'</span></div></div>';
  });
  h += '</div>';
  el.innerHTML = h;
}
function pluginFilter() {
  var q = ((document.getElementById('plugin-search')||{}).value||'').toLowerCase();
  var cat = (document.getElementById('plugin-cat')||{}).value||'all';
  document.querySelectorAll('.plugin-item').forEach(function(p){
    var match = (!q || p.textContent.toLowerCase().indexOf(q)>=0) && (cat==='all' || p.getAttribute('data-cat')===cat);
    p.style.display = match ? '' : 'none';
  });
}
function showPluginDetail(id) {
  _catalog.currentId = id;
  _catalog.detail = null; _catalog.metrics = []; _catalog.dashList = []; _catalog.dash = null;
  _catalog.expanded = {};
  switchPluginTab(id, 'doc');
}
function switchPluginTab(id, tab) {
  if (id !== _catalog.currentId) { _catalog.currentId = id; _catalog.detail = null; }
  ensureDetail(function(){ renderPluginShell(tab); });
}
function ensureDetail(cb) {
  var id = _catalog.currentId;
  if (_catalog.detail && _catalog.detail.id === id) { cb(); return; }
  fetch(API+'/catalog/plugins/'+id).then(function(r){return r.json()}).then(function(d){
    _catalog.detail = d; cb();
  }).catch(function(){ _catalog.detail = {id:id, name:'?', display_name:'未知插件', collect_doc_md:''}; cb(); });
}
// ---- 单位口径人话化：夜莺单位码 → 展示标签 + 数值格式化 ----
var _UNIT_LABELS = {
  '': '-', 'none': '个数', 'bytesIEC': '字节', 'bytes': '字节', 'bytesSI': '字节',
  'bitsIEC': '比特', 'bits': '比特', 'bitsSecIEC': 'bit/s', 'bitsSecSI': 'bit/s',
  'bytesSecIEC': 'B/s', 'bytesSecSI': 'B/s', 'sishort': '数值', 'percent': '%',
  'milliseconds': '毫秒', 'seconds': '秒', 'datetimeSeconds': '时间'
};
function unitLabel(u) {
  if (!u) return '-';
  return _UNIT_LABELS[u] !== undefined ? _UNIT_LABELS[u] : u;
}
// 按单位格式化数值（图表 Y 轴 / stat 大字 / 悬浮）
function fmtMetricVal(v, u) {
  var num = Number(v);
  if (v === null || v === undefined || isNaN(num)) return '-';
  u = u || '';
  if (u === 'percent') return (Math.round(num * 100) / 100) + '%';
  if (u === 'milliseconds') return (Math.round(num * 10) / 10) + 'ms';
  if (u === 'seconds') return (Math.round(num * 100) / 100) + 's';
  if (/^bits/i.test(u)) {
    var bunits = ['bit', 'Kbit', 'Mbit', 'Gbit', 'Tbit'], bi = 0, bval = Math.abs(num);
    var bsuffix = /sec/i.test(u) ? '/s' : '';
    var bsign = num < 0 ? '-' : '';
    while (bval >= 1000 && bi < bunits.length - 1) { bval /= 1000; bi++; }
    return bsign + (Math.round(bval * 10) / 10) + bunits[bi] + bsuffix;
  }
  if (/bytes/i.test(u)) {
    var units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'], i = 0, val = Math.abs(num);
    var suffix = /sec/i.test(u) ? '/s' : '';
    var sign = num < 0 ? '-' : '';
    while (val >= 1024 && i < units.length - 1) { val /= 1024; i++; }
    return sign + (Math.round(val * 10) / 10) + units[i] + suffix;
  }
  if (Math.abs(num) >= 1000000) return (num / 1000000).toFixed(1) + 'M';
  if (Math.abs(num) >= 1000) return (num / 1000).toFixed(1) + 'k';
  return String(Math.round(num * 100) / 100);
}
// ---- 指标活跃度：近 24h VM 有无该指标数据 ----
function ensureVmNames(rerender) {
  if (window._vmNameSet) return;
  if (!window._vmNameCbs) window._vmNameCbs = [];
  window._vmNameCbs.push(rerender);
  if (window._vmNameLoading) return;
  window._vmNameLoading = true;
  fetch(API + '/vm/names').then(function(r){return r.json()}).then(function(res){
    var set = {};
    ((res || {}).data || []).forEach(function(n){ set[n] = 1; });
    window._vmNameSet = set;
  }).catch(function(){ window._vmNameSet = {}; }).then(function(){
    var cbs = window._vmNameCbs || []; window._vmNameCbs = [];
    window._vmNameLoading = false;
    cbs.forEach(function(f){ try { f(); } catch(e) {} });
  });
}
function vmHasData(rawName) { return !!(window._vmNameSet && window._vmNameSet[rawName]); }
function actDot(rawName) {
  var ok = vmHasData(rawName);
  return '<span title="' + (ok ? '近 24h 有数据上报' : '近 24h 无数据上报') + '" style="color:' + (ok ? 'var(--success,#2ea043)' : 'var(--muted)') + ';font-size:9px;margin-right:4px;vertical-align:1px">●</span>';
}
// 内置指标的 name 是中文展示名，英文 key 存在 expression（纯指标名）里；其余来源 name 即 key
function metricKey(m) {
  var e = (m.expression || '').trim();
  if (e && /^[a-zA-Z_:][a-zA-Z0-9_:]*$/.test(e)) return e;
  return m.name || '';
}
function copyKey(txt) {
  var done = function(){ toast('已复制: ' + txt, 'ok'); };
  var fallback = function(){
    var ta = document.createElement('textarea');
    ta.value = txt; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); done(); } catch (e) { toast('复制失败', 'err'); }
    document.body.removeChild(ta);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(txt).then(done, fallback);
  } else { fallback(); }
}
function renderPluginShell(tab) {
  var d = _catalog.detail || {};
  var headIcon = d.icon && d.icon.indexOf('/')>=0
    ? '<img src="'+d.icon+'" style="width:22px;height:22px;border-radius:4px;vertical-align:-5px;margin-right:6px">'
    : '🧩 ';
  var html = '<div class="card"><div class="card-hd">'
    + headIcon+(d.display_name||d.name||'')
    + ' <span style="font-weight:400;font-size:11px;color:var(--muted)">'+(d.category||'')+' · '+(d.type||'exporter')+'</span>'
    + '<button class="btn btn-o btn-sm" style="float:right" onclick="renderPluginsMart()">← 返回</button>'
    + '<button class="btn btn-o btn-sm" style="float:right;margin-right:6px" onclick="goPage(\'targets\'); setTimeout(function(){openOnboardWizard(\''+(d.name||'')+'\')},300)" title="到采集目标菜单，通过接入向导接入该插件">⚡ 去接入</button>'
    + '<button class="btn btn-o btn-sm" style="float:right;margin-right:6px" onclick="exportPlugin('+d.id+')" title="导出插件包（说明+指标+仪表盘）">⬇ 导出</button></div>';
  html += '<div class="detail-tabs" style="border-bottom:1px solid var(--border);padding:0 20px">'
    + '<span class="'+(tab==='doc'?'active':'')+'" onclick="switchPluginTab('+d.id+',\'doc\')">📖 采集说明</span>'
    + '<span class="'+(tab==='metrics'?'active':'')+'" onclick="switchPluginTab('+d.id+',\'metrics\')">📏 指标说明</span>'
    + '<span class="'+(tab==='dashboard'?'active':'')+'" onclick="switchPluginTab('+d.id+',\'dashboard\')">📊 仪表盘</span>'
    + '</div><div class="card-bd" id="plugin-tab-content"></div></div>';
  document.getElementById("main-content").innerHTML = html;
  if (tab==='doc') renderCollectDoc();
  else if (tab==='metrics') renderMetricsExplorer();
  else if (tab==='dashboard') renderDashList();
}
function exportPlugin(id) {
  window.open(API + '/catalog/plugins/' + id + '/export', '_blank');
  toast('插件包导出中，浏览器将下载 JSON 文件', 'ok');
}
// ---- 采集说明（纯展示） ----
function renderCollectDoc() {
  var el = document.getElementById('plugin-tab-content'); if (!el) return;
  var d = _catalog.detail || {};
  el.innerHTML = '<div style="max-width:820px;margin:0 auto;padding:8px 4px">'
    + '<div style="text-align:right;margin-bottom:8px"><button class="btn btn-o btn-sm" onclick="openDocEditor()">✏️ 编辑说明</button></div>'
    + '<div id="doc-body" style="font-size:13px;line-height:1.7">'+mdRender(d.collect_doc_md)+'</div>'
    + '</div>';
}
function openDocEditor() {
  var d = _catalog.detail || {};
  renderOverlay('✏️ 编辑采集说明', function(){
    return '<textarea id="doc-editor" style="width:100%;min-height:340px;padding:10px;border:1px solid var(--border);border-radius:6px;font-family:ui-monospace,monospace;font-size:12px">'+escHtml(d.collect_doc_md||'')+'</textarea>'
      + '<div style="margin-top:12px;display:flex;gap:8px"><button class="btn btn-p" onclick="savePluginDoc()">💾 保存</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
  });
}
function savePluginDoc() {
  var doc = document.getElementById('doc-editor').value;
  fetch(API+'/catalog/plugins/'+_catalog.currentId+'/doc', {method:'PUT', headers:{'Content-Type':'application/json'}, body: JSON.stringify({doc:doc})})
    .then(function(r){return r.json()}).then(function(res){
      if (res.error) { toast('保存失败: '+res.error,'err'); return; }
      _catalog.detail.collect_doc_md = doc;
      closeOverlay(); toast('采集说明已保存','ok'); renderCollectDoc();
    });
}
// ---- 轻量 Markdown 渲染（零依赖） ----
function escHtml(s) {
  return (s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}
// 审计结果详情展开/收起：两行截断(34px) ↔ 全量
function audToggle(id) {
  var el = document.getElementById(id); if (!el) return;
  var open = el.style.maxHeight === 'none';
  el.style.maxHeight = open ? '34px' : 'none';
  el.title = open ? el.textContent : '点击收起';
}
// 简易字符串哈希（生成审计详情元素的稳定 id）
function hashStr(s) {
  var h = 0;
  for (var i = 0; i < s.length; i++) { h = (h * 31 + s.charCodeAt(i)) | 0; }
  return h;
}
function inlineMD(s) {
  s = escHtml(s);
  s = s.replace(/`([^`]+)`/g, '<code style="background:rgba(128,128,128,0.15);padding:1px 5px;border-radius:3px;font-family:ui-monospace,monospace;font-size:12px">$1</code>');
  s = s.replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
  return s;
}
function mdRender(md) {
  if (!md) return '<div style="color:var(--muted);font-size:12px">暂无采集说明，点击右上角「编辑说明」填写</div>';
  var lines = md.split('\n'), html = '', inCode = false, inList = false;
  for (var i=0;i<lines.length;i++) {
    var line = lines[i];
    if (line.trim().indexOf('```')===0) {
      if (inCode) { html += '</pre>'; inCode = false; }
      else {
        if (inList) { html += '</ul>'; inList = false; }
        html += '<pre style="background:rgba(128,128,128,0.12);padding:10px 12px;border-radius:6px;overflow:auto;font-size:12px;line-height:1.5"><code>';
        inCode = true;
      }
      continue;
    }
    if (inCode) { html += escHtml(line)+'\n'; continue; }
    var t = line.trim();
    if (t.indexOf('- ')===0 || t.indexOf('* ')===0) {
      if (!inList) { html += '<ul style="margin:6px 0;padding-left:22px">'; inList = true; }
      html += '<li>'+inlineMD(t.substring(2))+'</li>';
      continue;
    }
    if (inList) { html += '</ul>'; inList = false; }
    if (t.indexOf('### ')===0) html += '<h4 style="margin:14px 0 6px;font-size:13px">'+inlineMD(t.substring(4))+'</h4>';
    else if (t.indexOf('## ')===0) html += '<h3 style="margin:16px 0 8px;font-size:14px;border-bottom:1px solid var(--border);padding-bottom:4px">'+inlineMD(t.substring(3))+'</h3>';
    else if (t.indexOf('# ')===0) html += '<h2 style="margin:8px 0 10px;font-size:16px">'+inlineMD(t.substring(2))+'</h2>';
    else if (t==='') html += '<div style="height:8px"></div>';
    else html += '<div style="margin:4px 0">'+inlineMD(t)+'</div>';
  }
  if (inCode) html += '</code></pre>';
  if (inList) html += '</ul>';
  return html;
}
      // ---- 指标说明（过滤检索 / 列表 / 明细 / 新增扩展 / 删除） ----
      function renderMetricsExplorer() {
        var id = _catalog.currentId;
        var el = document.getElementById('plugin-tab-content'); if (!el) return;
        el.innerHTML = '<div style="color:var(--muted);padding:12px;font-size:12px">⏳ 加载指标清单...</div>';
        fetch(API+'/catalog/plugins/'+id+'/metrics?view='+(_catalog.view||'curated')).then(function(r){return r.json()}).then(function(list){
          _catalog.metrics = list || [];
          return fetch(API+'/catalog/plugins/'+id+'/metrics/filters').then(function(r){return r.json()});
        }).then(function(f){
          _catalog.filters = f || {types:[],units:[]};
          _catalog.pmPage = 1;
          pmTableShellRender();
        }).catch(function(){ pmTableShellRender(); });
      }
      function pmTableShellRender() {
        var el = document.getElementById('plugin-tab-content'); if (!el) return;
        var keepQ = document.getElementById('pm-search') ? document.getElementById('pm-search').value : '';
        var keepT = document.getElementById('pm-type') ? document.getElementById('pm-type').value : '';
        var keepU = document.getElementById('pm-unit') ? document.getElementById('pm-unit').value : '';
        var total = _catalog.metrics.length;
        var nCustom=0, nDisc=0;
        _catalog.metrics.forEach(function(m){
          if (m.source==='custom') nCustom++;
          else if (m.source==='vm_discovered') nDisc++;
        });
        var nBuiltin = total - nCustom - nDisc;
        var view = _catalog.view||'curated';
        var h = '<div style="display:flex;gap:8px;margin-bottom:10px;flex-wrap:wrap;align-items:center">';
        h += '<span style="display:inline-flex;border:1px solid var(--border);border-radius:4px;overflow:hidden">'
          + '<button class="btn btn-o btn-sm" style="border:0;border-radius:0;'+(view==='curated'?'background:var(--primary,#2f81f7);color:#fff':'')+'" onclick="pmSetView(\'curated\')">精选</button>'
          + '<button class="btn btn-o btn-sm" style="border:0;border-radius:0;'+(view==='all'?'background:var(--primary,#2f81f7);color:#fff':'')+'" onclick="pmSetView(\'all\')">全部</button></span>';
        h += '<input placeholder="搜索 Key / 名称 / PromQL..." oninput="metricsFilterChange()" value="'+escHtml(keepQ)+'" style="width:220px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px" id="pm-search">';
        h += '<select onchange="metricsFilterChange()" id="pm-type" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部类型</option>';
        (_catalog.filters.types||[]).forEach(function(t){ h += '<option value="'+t+'"'+(t===keepT?' selected':'')+'>'+t+'</option>'; });
        h += '</select><select onchange="metricsFilterChange()" id="pm-unit" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部单位</option>';
        (_catalog.filters.units||[]).forEach(function(u){ h += '<option value="'+escHtml(u)+'"'+(u===keepU?' selected':'')+'>'+escHtml(unitLabel(u))+'</option>'; });
        h += '</select><button class="btn btn-p btn-sm" onclick="openMetricModal()">＋ 新增指标</button>';
        h += '<span style="margin-left:auto;font-size:11px;color:var(--muted)">共 '+total+' 条（内置 '+nBuiltin+' · 自定义 '+nCustom+(view==='all'?' · 自动发现 '+nDisc:'')+'）</span></div>';
        h += '<div style="overflow:auto"><table style="font-size:12px;width:100%;border-collapse:collapse"><thead><tr style="color:var(--muted);text-align:left">'
          + '<th style="padding:6px 8px;border-bottom:1px solid var(--border)">指标名（Key）</th>'
          + '<th style="padding:6px 8px;border-bottom:1px solid var(--border)">类型</th>'
          + '<th style="padding:6px 8px;border-bottom:1px solid var(--border)">单位</th>'
          + '<th style="padding:6px 8px;border-bottom:1px solid var(--border)">说明</th>'
          + '<th style="padding:6px 8px;border-bottom:1px solid var(--border);text-align:right">操作</th></tr></thead><tbody id="pm-tbody"></tbody></table></div>';
        h += '<div id="pm-pager" style="display:flex;align-items:center;justify-content:center;gap:8px;margin-top:10px;font-size:12px"></div>';
        el.innerHTML = h;
        pmTbodyRender();
      }
      function metricsFilterChange() { _catalog.pmPage = 1; pmTbodyRender(); }
      function pmSetView(v) { _catalog.view = v; _catalog.pmPage = 1; renderMetricsExplorer(); }
      function pmFilteredMetrics() {
        var q = (document.getElementById('pm-search')||{}).value||'';
        var t = (document.getElementById('pm-type')||{}).value||'';
        var u = (document.getElementById('pm-unit')||{}).value||'';
        q = q.toLowerCase();
        return (_catalog.metrics||[]).filter(function(m){
          var okQ = !q || (m.name||'').toLowerCase().indexOf(q)>=0 || (m.note||'').toLowerCase().indexOf(q)>=0 || (m.expression||'').toLowerCase().indexOf(q)>=0;
          var okT = !t || m.metric_type===t;
          var okU = !u || m.unit===u;
          return okQ && okT && okU;
        });
      }
      function pmTbodyRender() {
        var tbody = document.getElementById('pm-tbody'); if (!tbody) return;
        var filtered = pmFilteredMetrics();
        // 分组折叠：指标带 grp 字段（如 host_metrics 的 cpu/memory/disk...）时按组渲染可折叠段
        var hasGrp = filtered.some(function(m){return m.grp});
        var ps = 15, page = _catalog.pmPage||1;
        if (hasGrp) ps = 100000; // 分组模式下不分页，靠折叠控制长度
        var totalPages = Math.max(1, Math.ceil(filtered.length/ps));
        if (page > totalPages) page = totalPages;
        var items = filtered.slice((page-1)*ps, page*ps);
        _catalog.grpCollapsed = _catalog.grpCollapsed || {};
        var h = '';
        if (hasGrp) {
          // 组顺序按过滤结果首次出现序，保持稳定
          var order = [], groups = {};
          items.forEach(function(m){
            var g = m.grp || '';
            if (!groups[g]) { groups[g] = []; order.push(g); }
            groups[g].push(m);
          });
          var grpTotal = {};
          filtered.forEach(function(m){ grpTotal[m.grp||''] = (grpTotal[m.grp||'']||0)+1; });
          var _GRP_LABELS = {cpu:'CPU', memory:'内存', disk:'磁盘 IO', filesystem:'文件系统', network:'网络', netstack:'网络协议栈', loadproc:'负载与进程', kernel:'内核与 VM', system:'系统信息', custom:'平台自定义', '':'其他'};
          order.forEach(function(g){
            var collapsed = _catalog.grpCollapsed[g];
            var label = _GRP_LABELS[g] || g || '未分组';
            h += '<tr style="cursor:pointer;background:rgba(128,128,128,0.06)" onclick="pmToggleGrp(\''+escHtml(g)+'\')">'
              + '<td colspan="5" style="padding:6px 8px;border-bottom:1px solid var(--border);font-size:12px;font-weight:600">'
              + '<span style="display:inline-block;width:12px;color:var(--muted)">'+(collapsed?'▸':'▾')+'</span>'
              + escHtml(label) + ' <span style="font-weight:400;font-size:10px;color:var(--muted)">'+(g?'('+g+') · ':'')+''
              + (collapsed ? grpTotal[g]+' 项（已折叠）' : groups[g].length+' 项显示')
              + '</span></td></tr>';
            if (!collapsed) h += pmRowsHTML(groups[g]);
          });
        } else {
          h += pmRowsHTML(items);
        }
        if (filtered.length===0) h = '<tr><td colspan="5" style="padding:24px;text-align:center;color:var(--muted)">无匹配指标</td></tr>';
        tbody.innerHTML = h;
        ensureVmNames(function(){ if (document.getElementById('pm-tbody')) pmTbodyRender(); });
        var pager = document.getElementById('pm-pager');
        if (pager) {
          if (hasGrp) { pager.innerHTML = '<span style="color:var(--muted)">共 '+filtered.length+' 条 · '+Object.keys(_catalog.grpCollapsed).filter(function(k){return _catalog.grpCollapsed[k]}).length+' 组已折叠</span>'; return; }
          var ph = '<span style="color:var(--muted)">第 '+page+'/'+totalPages+' 页 · '+filtered.length+' 条</span>';
          if (totalPages>1) {
            ph += '<button class="btn btn-o btn-sm" onclick="_catalog.pmPage='+Math.max(1,page-1)+';pmTbodyRender()"'+(page===1?' disabled':'')+'>‹</button>';
            ph += '<button class="btn btn-o btn-sm" onclick="_catalog.pmPage='+Math.min(totalPages,page+1)+';pmTbodyRender()"'+(page===totalPages?' disabled':'')+'>›</button>';
          }
          pager.innerHTML = ph;
        }
      }
      // 单条指标行渲染（分组/分页共用）
      function pmRowsHTML(rows) {
        var h = '';
        rows.forEach(function(m){
          var isCustom = m.source==='custom';
          var isDisc = m.source==='vm_discovered';
          var badge = isCustom ? ' <span style="background:rgba(46,160,67,0.2);color:#3fb950;font-size:10px;padding:1px 6px;border-radius:8px;margin-left:4px">自定义</span>'
            : (isDisc ? ' <span style="background:rgba(139,148,158,0.2);color:var(--muted);font-size:10px;padding:1px 6px;border-radius:8px;margin-left:4px">自动发现</span>' : '');
          var ops = isCustom ? '<button class="btn btn-o btn-sm" onclick="event.stopPropagation();editMetric('+m.id+')">编辑</button> <button class="btn btn-o btn-sm" onclick="event.stopPropagation();deleteCustomMetric('+m.id+')">删除</button>'
            : (isDisc ? '<button class="btn btn-o btn-sm" onclick="event.stopPropagation();deleteCustomMetric('+m.id+')">删除</button>' : '<span style="color:var(--muted);font-size:11px">内置</span>');
          var mk = metricKey(m);
          var nameCell = '<code style="font-size:12px;color:var(--primary)">'+escHtml(mk)+'</code>'+badge
            + ' <span title="复制英文 Key" style="cursor:pointer;color:var(--muted);font-size:10px" onclick="event.stopPropagation();copyKey(\''+mk+'\')">⧉</span>'
            + (mk!==m.name ? '<div style="font-size:10px;color:var(--muted)">'+escHtml(m.name)+'</div>' : '');
          h += '<tr style="cursor:pointer" onclick="toggleMetricDetail('+m.id+')">'
            + '<td style="padding:7px 8px;border-bottom:1px solid var(--border)">'+actDot(m.expression||m.name)+nameCell+'</td>'
            + '<td style="padding:7px 8px;border-bottom:1px solid var(--border)">'+escHtml(m.metric_type||'gauge')+'</td>'
            + '<td style="padding:7px 8px;border-bottom:1px solid var(--border)" title="'+escHtml(m.unit||'')+'">'+escHtml(unitLabel(m.unit))+'</td>'
            + '<td style="padding:7px 8px;border-bottom:1px solid var(--border)">'+escHtml(m.note||'')+'</td>'
            + '<td style="padding:7px 8px;border-bottom:1px solid var(--border);text-align:right;white-space:nowrap">'+ops+'</td></tr>';
          if (_catalog.expanded[m.id]) {
            h += '<tr><td colspan="5" style="padding:0;border-bottom:1px solid var(--border)"><div style="background:rgba(128,128,128,0.08);padding:10px 14px">'
              + '<div style="font-size:11px;color:var(--muted);margin-bottom:4px">PromQL 表达式</div>'
              + '<code style="font-size:12px;word-break:break-all">'+escHtml(m.expression||'-')+'</code>'
              + '</div></td></tr>';
          }
        });
        return h;
      }
      function pmToggleGrp(g) { _catalog.grpCollapsed[g] = !_catalog.grpCollapsed[g]; pmTbodyRender(); }
      function toggleMetricDetail(mid) { _catalog.expanded[mid] = !_catalog.expanded[mid]; pmTbodyRender(); }
      function openMetricModal(editId) {
        var m = editId ? (_catalog.metrics||[]).find(function(x){return x.id===editId}) : null;
        renderOverlay(m?'✏️ 编辑指标':'＋ 新增 / 扩展指标', function(){
          var h = '<div style="display:grid;gap:10px;font-size:13px">';
          h += '<div><b>指标名 *</b><br><input id="cm-name" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: mysql_custom_slowquery_ratio" value="'+(m?escHtml(m.name):'')+'"></div>';
          h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
            + '<div><b>类型</b><br><select id="cm-type" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"><option'+(m&&m.metric_type==='gauge'?' selected':'')+'>gauge</option><option'+(m&&m.metric_type==='counter'?' selected':'')+'>counter</option></select></div>'
            + '<div><b>单位</b><br><input id="cm-unit" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: % / s / short" value="'+(m?escHtml(m.unit||''):'')+'"></div></div>';
          h += '<div><b>说明</b><br><input id="cm-note" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="指标业务含义" value="'+(m?escHtml(m.note||''):'')+'"></div>';
          h += '<div><b>PromQL 表达式 *</b><br><textarea id="cm-expr" rows="3" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px;font-family:ui-monospace,monospace;font-size:12px" placeholder="例: rate(mysql_global_status_slow_queries[5m])">'+(m?escHtml(m.expression||''):'')+'</textarea></div>';
          h += '</div><div style="margin-top:14px;display:flex;gap:8px"><button class="btn btn-p" onclick="saveCustomMetric('+(editId||0)+')">💾 保存</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
          return h;
        });
      }
      function editMetric(mid) { openMetricModal(mid); }
      function saveCustomMetric(editId) {
        var body = {
          name: document.getElementById('cm-name').value.trim(),
          unit: document.getElementById('cm-unit').value.trim(),
          note: document.getElementById('cm-note').value.trim(),
          expression: document.getElementById('cm-expr').value.trim(),
          metric_type: document.getElementById('cm-type').value
        };
        if (!body.name || !body.expression) { alert('指标名和 PromQL 表达式为必填项'); return; }
        if (editId) {
          var m = (_catalog.metrics||[]).find(function(x){return x.id===editId});
          if (m) { Object.assign(m, body); }
          toast('已更新（内存）','ok'); closeOverlay(); pmTbodyRender();
          return;
        }
        fetch(API+'/catalog/plugins/'+_catalog.currentId+'/metrics', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)})
          .then(function(r){return r.json()}).then(function(res){
            if (res.error) { toast('保存失败: '+res.error,'err'); return; }
            closeOverlay(); toast('指标已入库','ok');
            fetch(API+'/catalog/plugins/'+_catalog.currentId+'/metrics').then(function(r){return r.json()}).then(function(list){ _catalog.metrics=list||[]; metricsFilterChange(); });
          });
      }
      function deleteCustomMetric(mid) {
        if (!confirm('确认删除该自定义指标？')) return;
        fetch(API+'/catalog/metrics/'+mid, {method:'DELETE'}).then(function(r){return r.json()}).then(function(res){
          if (res.error) { toast('删除失败: '+res.error,'err'); return; }
          toast('已删除','ok');
          _catalog.metrics = (_catalog.metrics||[]).filter(function(x){return x.id!==mid});
          metricsFilterChange();
        });
      }
      // ---- 仪表盘（列表 → 视图：时间范围 + 实例切换 + SVG 时序） ----
      function renderDashList() {
        var id = _catalog.currentId;
        var el = document.getElementById('plugin-tab-content'); if (!el) return;
        el.innerHTML = '<div style="color:var(--muted);padding:12px;font-size:12px">⏳ 加载仪表盘...</div>';
        fetch(API+'/catalog/plugins/'+id+'/dashboards').then(function(r){return r.json()}).then(function(list){
          _catalog.dashList = list || [];
          var h = '<div style="font-size:13px;margin-bottom:12px;color:var(--muted)">点击进入大盘视图，顶部可切换时间范围与具体实例</div>';
          h += '<div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:10px">';
          _catalog.dashList.forEach(function(d){
            h += '<div style="border:1px solid var(--border);border-radius:8px;padding:14px;cursor:pointer" onclick="openDashView('+d.id+')">'
              + '<div style="font-size:13px;font-weight:500">📊 '+escHtml(d.name)+'</div>'
              + '<div style="font-size:11px;color:var(--muted);margin-top:6px">创建于 '+(d.created_at||'').substring(0,10)+'</div></div>';
          });
          if (_catalog.dashList.length===0) h += '<div style="color:var(--muted);font-size:12px">该插件暂无仪表盘模板</div>';
          h += '</div>';
          el.innerHTML = h;
        }).catch(function(){ el.innerHTML = '<div style="color:var(--muted);font-size:12px">加载失败</div>'; });
      }
      function openDashView(did) {
        fetch(API+'/catalog/dashboards/'+did).then(function(r){return r.json()}).then(function(d){
          _catalog.dash = d;
          _catalog.dashRange = _catalog.dashRange || '1h';
          _catalog.dashVar = {};
          renderDashView();
        });
      }
      var _rangeSec = {'1h':3600,'6h':21600,'24h':86400,'7d':604800};
      function renderDashView() {
        var el = document.getElementById('plugin-tab-content'); if (!el) return;
        var d = _catalog.dash || {}; var cfg = {};
        try { cfg = JSON.parse(d.configs_json||'{}'); } catch(e) {}
        if (cfg.configs) cfg = cfg.configs; // 夜莺顶层结构 {name, configs:{panels,var}}
        var panels = cfg.panels || [];
        _catalog.dashPanels = panels;
        var vars = cfg.var || [];
        var instanceVar = (vars||[]).find(function(v){return v.type==='query' && v.name==='instance'});
        var h = '<div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:12px;padding:8px 10px;border:1px solid var(--border);border-radius:8px">';
        h += '<button class="btn btn-o btn-sm" onclick="renderDashList()">← 面板列表</button>';
        h += '<b style="font-size:13px">'+escHtml(d.name||'仪表盘')+'</b>';
        h += '<span style="margin-left:12px;font-size:11px;color:var(--muted)">时间</span>';
        ['1h','6h','24h','7d'].forEach(function(r){
          h += '<button class="btn btn-sm '+(r===_catalog.dashRange?'btn-p':'btn-o')+'" onclick="dashSetRange(\''+r+'\')">'+r+'</button>';
        });
        h += '<span style="margin-left:12px;font-size:11px;color:var(--muted)">实例</span>';
        h += '<select id="dash-instance" onchange="dashSetInstance(this.value)" style="padding:5px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px;max-width:200px"><option value=".*">全部实例</option></select>';
        h += '<button class="btn btn-o btn-sm" style="margin-left:auto" onclick="renderDashView()">⟳ 刷新</button>';
        h += '</div>';
        h += '<div id="dash-grid" style="display:grid;grid-template-columns:repeat(24,1fr);gap:8px"></div>';
        el.innerHTML = h;
        // 实例下拉填充
        if (instanceVar && instanceVar.definition) {
          var mm = instanceVar.definition.match(/label_values\((\w+),\s*(\w+)\)/);
          if (mm) {
            fetch(API+'/vm/label-values?metric='+encodeURIComponent(mm[1])+'&label='+encodeURIComponent(mm[2]))
              .then(function(r){return r.json()}).then(function(res){
                var sel = document.getElementById('dash-instance'); if (!sel) return;
                ((res.data||{}).result||[]).forEach(function(v){
                  var opt = document.createElement('option'); opt.value = v; opt.textContent = v; sel.appendChild(opt);
                });
              });
          }
        }
        // 布局渲染（夜莺 24 列 grid）
        var grid = document.getElementById('dash-grid');
        var ghtml = '';
        panels.forEach(function(p){
          var L = p.layout || {};
          var w = Math.max(4, Math.min(24, L.w||8));
          if (p.type==='row') {
            ghtml += '<div style="grid-column:1/-1;font-size:13px;font-weight:500;padding:10px 0 4px;border-bottom:1px solid var(--border)">'+escHtml(p.name||'')+'</div>';
          } else {
            ghtml += '<div style="grid-column:span '+w+';border:1px solid var(--border);border-radius:8px;display:flex;flex-direction:column;min-height:'+Math.max(110,(L.h||3)*44)+'px">'
              + '<div style="padding:7px 10px;font-size:11px;font-weight:500;border-bottom:1px solid var(--border)">'+escHtml(p.name||'')+'</div>'
              + '<div id="dp-'+p.layout.i+'" style="flex:1;padding:6px;display:flex;align-items:center;justify-content:center;font-size:11px;color:var(--muted)">⏳ 查询中</div>'
              + '</div>';
          }
        });
        grid.innerHTML = ghtml;
        // 异步查询各面板
        panels.forEach(function(p){
          if (p.type==='row') return;
          (function(panel){ setTimeout(function(){ queryPanel(panel); }, 50); })(p);
        });
      }
      function dashSetRange(r) { _catalog.dashRange = r; renderDashView(); }
      function dashSetInstance(v) { _catalog.dashVar.instance = v; renderDashView(); }
      function resolveExpr(expr) {
        var inst = _catalog.dashVar.instance || '.*';
        expr = expr.replace(/\$\{?instance\}?\??/g, inst);
        expr = expr.replace(/\$\{?(ident|address)\}?\??/g, inst);
        expr = expr.replace(/\$\{?prom\}?\??/g, '1');
        expr = expr.replace(/\$\{?(\w+)\}?\??/g, '.*');
        return expr;
      }
      var _seriesColors = ['#378ADD','#F09595','#EF9F27','#5DCAA5','#CECBF6','#B4B2A9','#E24B4A','#1D9E75'];
      function queryPanel(panel) {
        var el = document.getElementById('dp-'+(panel.layout||{}).i); if (!el) return;
        var target = (panel.targets||[])[0];
        var expr = target ? (target.expr||'') : '';
        if (!expr) { el.textContent = '无查询'; return; }
        var sec = _rangeSec[_catalog.dashRange] || 3600;
        var end = Math.floor(Date.now()/1000);
        var step = Math.max(15, Math.round(sec/120));
        var start = end - sec;
        fetch(API+'/vm/query_range?query='+encodeURIComponent(resolveExpr(expr))+'&start='+start+'&end='+end+'&step='+step)
          .then(function(r){return r.json()}).then(function(res){
            var series = ((res||{}).data||{}).result || [];
            if (series.length===0) { el.innerHTML = '<span style="color:var(--muted)">暂无数据</span>'; return; }
            if (panel.type==='stat') {
              var v = series[0].values && series[0].values.length ? series[0].values[series[0].values.length-1][1] : '-';
              var num = parseFloat(v);
              var disp = isNaN(num) ? escHtml(v) : fmtMetricVal(num, panel.unit||'');
              el.innerHTML = '<div style="text-align:center"><div style="font-size:26px;font-weight:500;color:var(--primary)">'+disp+'</div></div>';
              return;
            }
            el.innerHTML = seriesSVG(series, el.clientWidth||300, el.clientHeight||140, panel.unit||'')
              + '<div style="display:flex;gap:8px;flex-wrap:wrap;padding:2px 6px 4px;font-size:10px;color:var(--muted)">' + legendHTML(series) + '</div>';
          }).catch(function(){ el.textContent = '查询失败'; });
      }
      function legendHTML(series) {
        return series.slice(0,6).map(function(s,i){
          var label = (s.metric && (s.metric.legend || s.metric.instance || s.metric.__name__)) || ('series '+(i+1));
          return '<span><span style="display:inline-block;width:8px;height:8px;border-radius:2px;background:'+_seriesColors[i%_seriesColors.length]+';margin-right:3px"></span>'+escHtml(String(label).substring(0,30))+'</span>';
        }).join('');
      }
      function seriesSVG(series, w, h, unit) {
        var padL = 44, padB = 14, padT = 6, padR = 6;
        var iw = Math.max(60, w - padL - padR), ih = Math.max(40, h - padT - padB);
        var pts = [];
        var min = Infinity, max = -Infinity;
        series.forEach(function(s){ (s.values||[]).forEach(function(p){ var v = parseFloat(p[1]); if (!isNaN(v)) { if (v<min) min=v; if (v>max) max=v; } }); });
        if (!isFinite(min)) { min = 0; max = 1; }
        if (min === max) { max = min + 1; }
        var paths = '', t0 = 0, t1 = 1;
        var first = series[0].values||[];
        if (first.length>1) { t0 = first[0][0]; t1 = first[first.length-1][0]; }
        var span = (t1 - t0) || 1;
        series.forEach(function(s, si){
          var d = '';
          (s.values||[]).forEach(function(p){
            var x = padL + ((p[0]-t0)/span)*iw;
            var v = parseFloat(p[1]);
            if (isNaN(v)) return;
            var y = padT + ih - ((v-min)/(max-min))*ih;
            d += (d?' L':'M') + x.toFixed(1) + ' ' + y.toFixed(1);
          });
          if (d) paths += '<path d="'+d+'" fill="none" stroke="'+_seriesColors[si%_seriesColors.length]+'" stroke-width="1.5"/>';
        });
        var gridY = '';
        for (var g=0; g<=2; g++) {
          var gy = padT + ih*g/2;
          gridY += '<line x1="'+padL+'" y1="'+gy+'" x2="'+(padL+iw)+'" y2="'+gy+'" stroke="rgba(128,128,128,0.2)" stroke-width="0.5"/>';
          var gv = max - (max-min)*g/2;
          gridY += '<text x="'+(padL-4)+'" y="'+gy+'" font-size="9" fill="#888780" text-anchor="end" dominant-baseline="central">'+fmtMetricVal(gv, unit)+'</text>';
        }
        return '<svg viewBox="0 0 '+w+' '+h+'" width="100%" height="100%" preserveAspectRatio="none" style="display:block">'+gridY+paths+'</svg>';
      }
      function renderTargets() {
        var html = '<div class="card"><div class="card-hd">🎯 采集目标 <span style="font-weight:400;font-size:11px;color:var(--muted)">远程采集的目标实例清单（目录库持久化）</span>'
          + '<button class="btn btn-o btn-sm" style="float:right" onclick="targetEdit()">➕ 新增目标</button>'
          + '<button class="btn btn-p btn-sm" style="float:right;margin-right:6px" onclick="openOnboardWizard()">🚀 接入向导</button></div><div class="card-bd">';
        html += '<div style="display:flex;gap:8px;margin-bottom:12px;flex-wrap:wrap">';
        html += '<input placeholder="搜索目标..." oninput="targetFilter()" style="width:200px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px" id="target-search">';
        html += '<select onchange="targetFilter()" id="target-type" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="all">全部类型</option><option value="mysql">MySQL</option><option value="redis">Redis</option><option value="kafka">Kafka</option><option value="elasticsearch">Elasticsearch</option><option value="clickhouse">ClickHouse</option><option value="http">HTTP</option></select>';
        html += '</div>';
        html += '<table style="font-size:12px"><thead><tr><th>目标名称</th><th>类型</th><th>地址</th><th>所属资源</th><th>分派 Agent</th><th>插件</th><th>连通性</th><th>操作</th></tr></thead><tbody id="target-tbody">';
        html += '</tbody></table></div></div>';
        document.getElementById("main-content").innerHTML = html;
        Promise.all([
          fetch(API+'/targets').then(function(r){return r.json()}).catch(function(){ return []; }),
          fetch(API+'/resources').then(function(r){return r.json()}).catch(function(){ return {}; })
        ]).then(function(out){
          window._targets = out[0] || [];
          var map = {};
          ((out[1]||{}).resources||[]).forEach(function(x){ map[x.id] = x.name || x.ip || x.id; });
          window._resMap = map;
          targetRender();
        });
      }
      function targetRender() {
        var q = ((document.getElementById('target-search')||{}).value||'').toLowerCase();
        var t = (document.getElementById('target-type')||{}).value||'all';
        var filtered = (window._targets||[]).filter(function(tg){
          return (t==='all'||tg.type===t) && (!q || tg.name.toLowerCase().indexOf(q)>=0||tg.address.indexOf(q)>=0);
        });
        var tbody = document.getElementById('target-tbody');
        if (!tbody) return;
        if (filtered.length===0) { tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--muted);padding:20px">无匹配目标，点击右上角「新增目标」开始接入</td></tr>'; return; }
        tbody.innerHTML = filtered.map(function(tg){
          var res = tg.last_test_result||'';
          var resCell = !tg.resource_id ? '<span style="color:var(--muted)">—</span>'
            : ((window._resMap||{})[tg.resource_id]
              ? '<a href="javascript:void(0)" onclick="openResourceDrawer(\''+escHtml(tg.resource_id)+'\')" style="color:var(--primary)" title="打开资源详情">'+escHtml((window._resMap||{})[tg.resource_id])+'</a>'
              : '<span style="color:var(--muted)" title="台账里已没有这个资源（资源被删除后目标残留）">'+escHtml(tg.resource_id)+'（资源已删除）</span>');
          var st = res ? (res.indexOf('失败')===0
            ? '<span class="badge" style="background:#fee2e2;color:var(--error)" title="'+escHtml(tg.last_test_at||'')+'">'+escHtml(res)+'</span>'
            : '<span class="badge b-h" title="'+escHtml(tg.last_test_at||'')+'">'+escHtml(res)+'</span>')
            : '<span class="badge b-o">未测试</span>';
          var traceBtn = tg.flow_id ? ' <button class="btn btn-o btn-sm" style="font-size:10px;padding:2px 6px" onclick="openOnboardFlow('+tg.flow_id+')" title="回到产生这个目标的接入流水线，看它是怎么走过来的">🧭 接入过程</button>' : '';
          return '<tr><td>'+escHtml(tg.name)+'</td><td><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+escHtml(tg.type)+'</span></td><td><code>'+escHtml(tg.address)+'</code></td><td>'+resCell+'</td><td>'+(tg.agent_id?'<a href="javascript:void(0)" onclick="openDetailById(\''+tg.agent_id+'\')" style="color:var(--primary)">'+escHtml(tg.agent_id)+'</a>':'<span style="color:var(--muted)">未分派</span>')+'</td><td>'+(tg.plugin?'<span class="badge" style="background:#f0f0f0;color:#666">'+escHtml(tg.plugin)+'</span>':'-')+'</td><td>'+st+'</td><td style="white-space:nowrap"><button class="btn btn-o btn-sm" style="font-size:10px;padding:2px 6px" onclick="targetTest('+tg.id+')">测试</button> <button class="btn btn-o btn-sm" style="font-size:10px;padding:2px 6px" onclick="targetEdit('+tg.id+')">编辑</button> <button class="btn btn-o btn-sm" style="font-size:10px;padding:2px 6px;color:var(--error)" onclick="targetDelete('+tg.id+',\''+escHtml(tg.name)+'\')">删除</button>'+traceBtn+'</td></tr>';
        }).join('');
      }
      function targetFilter() { targetRender(); }
      function targetTest(id) {
        toast('正在测试连通性...', 'ok');
        fetch(API+'/targets/'+id+'/test',{method:'POST'}).then(function(r){return r.json()}).then(function(d){
          toast(d.result||d.error||'完成', d.ok?'ok':'err');
          return fetch(API+'/targets');
        }).then(function(r){return r.json()}).then(function(list){
          window._targets = list||[]; targetRender();
        }).catch(function(){});
      }
      function targetDelete(id, name) {
        if (!confirm('确定删除采集目标「'+name+'」？分派 Agent 的配置将同步变更。')) return;
        fetch(API+'/targets?id='+id,{method:'DELETE'}).then(function(r){return r.json()}).then(function(d){
          if (d.error) { toast('删除失败: '+d.error,'err'); return; }
          toast('已删除 '+name,'ok');
          renderTargets();
        }).catch(function(){ toast('删除失败','err'); });
      }
      function targetEdit(id) {
        var tg = id ? (window._targets||[]).find(function(x){return x.id===id}) : null;
        var agentOpts = '<option value="">未分派</option>';
        agents.forEach(function(a){ agentOpts += '<option value="'+a.id+'"'+(tg&&tg.agent_id===a.id?' selected':'')+'>'+escHtml(a.id)+' ('+(a.type==='proxy'?'Proxy':'Edge')+(a.source==='heartbeat'?' · 心跳':' · 本机')+')</option>'; });
        renderOverlay(tg?'✏️ 编辑采集目标':'➕ 新增采集目标', function(){
          var h = '<div style="display:grid;gap:10px;font-size:13px">';
          h += '<div><b>目标名称 *</b><br><input id="tg-name" value="'+(tg?escHtml(tg.name):'')+'" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: order-prod-db-01"></div>';
          h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
            + '<div><b>类型</b><br><select id="tg-type" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
            + ['mysql','redis','kafka','elasticsearch','clickhouse','http'].map(function(t){return '<option'+(tg&&tg.type===t?' selected':'')+'>'+t+'</option>'}).join('')
            + '</select></div>'
            + '<div><b>地址 *</b><br><input id="tg-addr" value="'+(tg?escHtml(tg.address):'')+'" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="host:port"></div></div>';
          h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
            + '<div><b>分派 Agent</b><br><select id="tg-agent" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'+agentOpts+'</select></div>'
            + '<div><b>采集插件</b><br><input id="tg-plugin" value="'+(tg?escHtml(tg.plugin||''):'')+'" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: mysql_probe"></div></div>';
          h += '<div><b>备注</b><br><input id="tg-note" value="'+(tg?escHtml(tg.note||''):'')+'" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '</div><div style="margin-top:16px;display:flex;gap:8px"><button class="btn btn-p" onclick="targetSave('+(tg?tg.id:0)+')">💾 保存</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
          return h;
        });
      }
      function targetSave(id) {
        var body = {
          id: id,
          name: document.getElementById('tg-name').value.trim(),
          type: document.getElementById('tg-type').value,
          address: document.getElementById('tg-addr').value.trim(),
          agent_id: document.getElementById('tg-agent').value,
          plugin: document.getElementById('tg-plugin').value.trim(),
          note: document.getElementById('tg-note').value.trim()
        };
        if (!body.name || !body.address) { toast('名称和地址必填','err'); return; }
        fetch(API+'/targets',{method:id?'PUT':'POST',headers:{"Content-Type":"application/json"},body:JSON.stringify(body)}).then(function(r){return r.json()}).then(function(d){
          if (d.error) { toast('保存失败: '+d.error,'err'); return; }
          toast(id?'已保存，分派 Agent 配置版本已更新':'已新增，分派 Agent 配置版本已更新','ok');
          closeOverlay();
          renderTargets();
        }).catch(function(){ toast('保存失败','err'); });
      }
      // ---- M1-⑤ 接入向导：选插件 → 填目标 → 分派 Agent → 连通性测试 → 配置下发回执 ----
      // prePlugin 预选插件；preAgentID 预选分派 Agent（从资源抽屉进来时必须带，否则建出来的目标
      // 落在"未分派"，本机清单里看不到）；backFn 关闭后回哪页（默认回采集目标页）
      function openOnboardWizard(prePlugin, preAgentID, backFn) {
        onboardCfg(function(cfg) {
        var plugins = cfg.onboard_plugins || [];
        var agentOpts = agents.map(function(a){
          return '<option value="'+a.id+'"'+(a.id===preAgentID?' selected':'')+(a.source==='heartbeat'?' data-hb="1"':'')+'>'+escHtml(a.id)+'（'+(a.type==='proxy'?'Proxy':'Edge')+(a.source==='heartbeat'?' · 心跳':' · 本机')+' · '+a.status+'）</option>';
        }).join('');
        renderOverlay('🚀 接入向导 <span style="font-weight:400;font-size:11px;color:var(--muted)">选插件 → 填目标 → 分派 → 能力配置 → 测试 → 配置下发</span>', function(){
          var h = '<div style="display:grid;gap:12px;font-size:13px;max-width:560px">';
          h += '<div><b>① 选择采集插件 *</b><br><select id="ob-plugin" onchange="obFillPort()" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">';
          plugins.forEach(function(p){ h += '<option value="'+p.probe+'" data-port="'+p.port+'"'+(prePlugin&&p.name===prePlugin?' selected':'')+'>'+p.name+'（'+p.probe+'）</option>'; });
          h += '</select><div style="font-size:11px;color:var(--muted);margin-top:3px">插件能力与指标说明见「采集插件」菜单</div></div>';
          h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
            + '<div><b>② 目标名称 *</b><br><input id="ob-name" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: order-prod-db-01"></div>'
            + '<div><b>目标地址 *</b><br><input id="ob-addr" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="host:port"></div></div>';
          h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
            + '<div><b>③ 分派 Agent *</b><br><select id="ob-agent" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'+agentOpts+'</select></div>'
            + '<div><b>备注</b><br><input id="ob-note" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"></div></div>';
          // ④ 主机指标能力配置（配置轨道，随目标一起下发给分派 Agent）
          h += '<div style="border:1px solid var(--border);border-radius:8px;padding:10px 12px">'
            + '<div style="font-weight:600">④ 主机指标能力 <span style="font-weight:400;font-size:10px;color:var(--muted)">仅对分派 Agent 的 host_metrics 生效（配置轨道，版本 +1）</span></div>'
            + '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;margin-top:8px">'
            + '<div><b style="font-size:12px">采集频率</b><br><select id="ob-hm-interval" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px">'
            + ['15s','30s','60s','5m'].map(function(iv){return '<option value="'+iv+'"'+(iv==='30s'?' selected':'')+'>'+iv+'</option>'}).join('')
            + '</select></div>'
            + '<div><b style="font-size:12px">保持 Agent 现有分组/例外</b><br><label style="font-size:12px;color:var(--muted);display:inline-flex;align-items:center;gap:4px;padding-top:6px"><input type="checkbox" id="ob-hm-keep" checked> 不修改分组配置</label></div>'
            + '</div>'
            + '<div id="ob-hm-groups" style="margin-top:8px;display:none"><b style="font-size:12px">分组勾选（未勾 = 关闭该组）</b><div id="ob-hm-grp-list" style="display:grid;grid-template-columns:repeat(2,1fr);gap:4px;margin-top:4px;font-size:12px;color:var(--muted)">⏳ 加载分组...</div></div>'
            + '</div>';
          h += '<div style="display:flex;gap:8px"><button class="btn btn-p" onclick="onboardRun()">🚀 创建并测试连通性</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
          h += '<div id="ob-receipt"></div>';
          h += '</div>';
          return h;
        }, true, backFn || renderTargets);
        obLoadGroupList();
        });
      }
      // 接入向导：动态加载 host_metrics 分组清单（治理目录推导，不写死）
      function obLoadGroupList() {
        fetch(API+'/metrics/gov?plugin_name=host_metrics&only_phase1=1').then(function(r){return r.json()}).then(function(list){
          var counts = {};
          (list||[]).forEach(function(m){ if (m.grp) counts[m.grp] = (counts[m.grp]||0)+1; });
          var labels = {cpu:'CPU',memory:'内存',disk:'磁盘 IO',filesystem:'文件系统',network:'网络',netstack:'网络协议栈',loadproc:'负载与进程',kernel:'内核与 VM',system:'系统信息',custom:'平台自定义'};
          var el = document.getElementById('ob-hm-grp-list');
          if (!el) return;
          var h = '';
          Object.keys(counts).sort().forEach(function(g){
            h += '<label style="display:inline-flex;align-items:center;gap:4px;cursor:pointer;color:var(--text,#333)">'
              + '<input type="checkbox" class="ob-hm-grp-cb" data-grp="'+escHtml(g)+'" checked> '
              + escHtml(labels[g]||g) + ' <span style="color:var(--muted);font-size:10px">'+counts[g]+'</span></label>';
          });
          el.innerHTML = h || '治理目录暂无 host_metrics 分组数据';
          el.style.color = '';
          // 「保持现有配置」勾选切换分组区显隐
          var keep = document.getElementById('ob-hm-keep');
          var box = document.getElementById('ob-hm-groups');
          if (keep && box) keep.onchange = function(){ box.style.display = keep.checked ? 'none' : 'block'; };
        }).catch(function(){});
      }
      function obFillPort() {
        var sel = document.getElementById('ob-plugin');
        var port = sel.options[sel.selectedIndex].dataset.port;
        var addr = document.getElementById('ob-addr');
        if (port && addr && addr.value.indexOf(':')<0) addr.placeholder = 'host:'+port;
      }
      function onboardRun() {
        var probe = document.getElementById('ob-plugin').value;
        var body = {
          name: document.getElementById('ob-name').value.trim(),
          type: probe.replace(/_probe|_exporter$/,'').replace('http_response','http'),
          address: document.getElementById('ob-addr').value.trim(),
          agent_id: document.getElementById('ob-agent').value,
          plugin: probe,
          note: document.getElementById('ob-note').value.trim()
        };
        if (!body.name || !body.address) { toast('名称和地址必填','err'); return; }
        var rc = document.getElementById('ob-receipt');
        rc.innerHTML = '<div style="margin-top:8px;color:var(--muted)">⏳ 创建目标...</div>';
        fetch(API+'/targets',{method:'POST',headers:{"Content-Type":"application/json"},body:JSON.stringify(body)}).then(function(r){return r.json()}).then(function(d){
          if (d.error) { rc.innerHTML = '<div style="margin-top:8px;color:var(--error)">❌ 创建失败：'+escHtml(d.error)+'</div>'; return; }
          var tid = d.id || d.target_id;
          // ④ 主机指标能力：未勾「保持现有配置」时，把频率+分组勾选写入分派 Agent 的 host_metrics 段（配置轨道）
          var keep = document.getElementById('ob-hm-keep');
          var cfgPromise;
          if (keep && !keep.checked && body.agent_id) {
            var groups = {};
            document.querySelectorAll('.ob-hm-grp-cb').forEach(function(cb){ groups[cb.dataset.grp] = cb.checked; });
            var hmBody = {host_metrics: {enabled: true, interval: document.getElementById('ob-hm-interval').value, groups: groups, exclude_metrics: []}};
            cfgPromise = fetch(API+'/agents/'+encodeURIComponent(body.agent_id)+'/config', {method:'PUT',headers:{"Content-Type":"application/json"},body:JSON.stringify(hmBody)})
              .then(function(r){return r.json()});
          } else {
            cfgPromise = Promise.resolve(null);
          }
          return cfgPromise.then(function(cfgRes){
            var cfgLine = cfgRes
              ? (cfgRes.error ? '<div style="margin-top:4px;color:var(--error)">⚠️ 指标配置下发失败：'+escHtml(cfgRes.error)+'</div>'
                             : '<div style="margin-top:4px">📈 主机指标配置已下发（频率 '+escHtml(document.getElementById('ob-hm-interval').value)+'，cfg-'+cfgRes.version+'）</div>')
              : '';
            rc.innerHTML = '<div style="margin-top:8px;font-size:12px"><div>✅ 目标已创建（id='+tid+'），分派 Agent 配置版本 +1</div>'+cfgLine
              + '<div style="margin-top:4px;color:var(--muted)">⏳ 正在测试连通性...</div></div>';
            return fetch(API+'/targets/'+tid+'/test',{method:'POST'}).then(function(r){return r.json()}).then(function(t){
              var ok = t.ok;
              var line2 = ok ? '✅ 连通性测试通过：'+escHtml(t.result||'') : '⚠️ 连通性测试失败：'+escHtml(t.result||t.error||'');
              rc.innerHTML = '<div style="margin-top:8px;font-size:12px"><div>✅ 目标已创建（id='+tid+'）</div>'+cfgLine
                + '<div style="margin-top:4px">'+line2+'</div>'
                + '<div style="margin-top:4px">📦 配置已写入分派 Agent 期望配置，Agent 下次心跳（≤30s）自动拉取生效</div>'
                + (ok ? '<div style="margin-top:6px;color:var(--muted)">💡 采集开始后，可在插件仪表盘（约 2-5 分钟）看到该目标曲线；若持续无数据，SLO 页断采清单会出现该目标</div>' : '<div style="margin-top:6px;color:var(--muted)">💡 网络不通不影响配置下发；修复网络后采集自动开始</div>')
                + '<div style="margin-top:10px;display:flex;gap:8px"><button class="btn btn-o btn-sm" onclick="closeOverlay()">完成</button></div></div>';
            });
          });
        }).catch(function(e){
          rc.innerHTML = '<div style="margin-top:8px;color:var(--error)">❌ 请求失败：'+escHtml(String(e))+'</div>';
        });
      }
      // ---- 指标中心：多渠道治理视图（D9 五渠道 × 层级树 × 渠道/分组/归属过滤） ----
      var _CHANNEL_LABELS = {builtin:'内置采集', exporter:'Exporter', script:'自定义脚本', sql:'SQL/JDBC', log:'日志转指标', '':'未归类'};
      var _OWNERSHIP_LABELS = {preset:'预置（可停用）', custom:'自定义', '':'-'};
      function renderMetricsCatalog(hostId) {
        var host = hostId || 'main-content';
        window._gov = window._gov || {query:'',level:'',major:'',channel:'',grp:'',ownership:'',phase1:false,page:1,ps:15};
        var g = window._gov;
        var html = '<div class="card"><div class="card-hd">📏 指标中心 <span style="font-weight:400;font-size:11px;color:var(--muted)">多渠道统一治理：内置 / Exporter / 自定义脚本 / SQL / 日志转指标</span>';
        html += '<span style="float:right;display:flex;gap:6px">';
        html += '<button class="btn btn-o btn-sm" onclick="openAddMetricModal()">➕ 新增</button>';
        html += '<button class="btn btn-o btn-sm" onclick="openVmModal()">🔍 从 VM 导入</button>';
        html += '</span></div><div class="card-bd" id="metrics-main">';
        html += '<div style="text-align:center;color:var(--muted);padding:20px">⏳ 加载中...</div></div></div>';
        document.getElementById(host).innerHTML = html;
        // facets（层级树/渠道/分组计数）+ 数据并行拉取
        fetch(API+'/metrics/gov/facets').then(function(r){return r.json()}).then(function(f){
          window._govFacets = f || {};
          metricsGovFetch();
        }).catch(function(){ window._govFacets = {}; metricsGovFetch(); });
      }
      // gov 过滤参数 → 查询串；数据 normalize 到 MetricDef 形状（plugin/desc/type 别名），保留治理字段
      function metricsGovFetch() {
        var g = window._gov;
        var qs = [];
        if (g.query) qs.push('query='+encodeURIComponent(g.query));
        if (g.level) qs.push('level='+encodeURIComponent(g.level));
        if (g.major) qs.push('major='+encodeURIComponent(g.major));
        if (g.channel) qs.push('channel='+encodeURIComponent(g.channel));
        if (g.grp) qs.push('grp='+encodeURIComponent(g.grp));
        if (g.ownership) qs.push('ownership='+encodeURIComponent(g.ownership));
        if (g.phase1) qs.push('only_phase1=1');
        fetch(API+'/metrics/gov'+(qs.length?'?'+qs.join('&'):'')).then(function(r){return r.json()}).then(function(data){
          window._METRICS = (data||[]).map(function(m){
            return Object.assign({}, m, {
              plugin: m.plugin_name || m.plugin || '',
              desc: m.note || '',
              type: m.metric_type || 'unknown'
            });
          });
          metricsGovRender();
        }).catch(function(){ window._METRICS = []; metricsGovRender(); });
      }
      function metricsGovRender() {
        var g = window._gov;
        var f = window._govFacets || {};
        var METRICS = window._METRICS || [];
        var el = document.getElementById('metrics-main');
        if (!el) return;
        var html = '';
        // Batch action bar
        html += '<div id="metrics-batch-bar" style="display:none;padding:8px 12px;background:#eef2ff;border-radius:6px;margin-bottom:10px;font-size:12px">';
        html += '<span id="metrics-batch-count">0 个选中</span> ';
        html += '<select id="batch-status" style="margin-left:8px;padding:3px 6px;border:1px solid var(--border);border-radius:4px"><option value="">— 修改状态 —</option><option value="active">active</option><option value="deprecated">deprecated</option></select> ';
        html += '<button class="btn btn-o btn-sm" onclick="batchUpdateStatus()">应用</button> ';
        html += '<button class="btn btn-o btn-sm" style="color:var(--error)" onclick="batchDeleteMetrics()">批量删除</button>';
        html += '</div>';
        // 过滤条：搜索 + 层级树（level→major 分组下拉）+ 渠道 + 分组 + 归属 + 仅本期
        html += '<div style="display:flex;gap:8px;margin-bottom:12px;flex-wrap:wrap;align-items:center">';
        html += '<input placeholder="搜索 Key / 名称 / 说明 / 口径..." oninput="govDebounce()" value="'+escHtml(g.query)+'" style="flex:1;max-width:220px;padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px" id="gov-search">';
        html += '<select onchange="govSet(\'level\',this.value);govCascadeMajor()" id="gov-level" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部层级</option>';
        Object.keys(f.levels||{}).sort().forEach(function(lv){
          var cnt = Object.values(f.levels[lv]).reduce(function(a,b){return a+b},0);
          html += '<option value="'+escHtml(lv)+'"'+(g.level===lv?' selected':'')+'>'+escHtml(lv||'未归类')+' ('+cnt+')</option>';
        });
        html += '</select>';
        html += '<select onchange="govSet(\'major\',this.value)" id="gov-major" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部大类</option>';
        if (g.level) {
          Object.keys((f.levels||{})[g.level]||{}).sort().forEach(function(mj){
            html += '<option value="'+escHtml(mj)+'"'+(g.major===mj?' selected':'')+'>'+escHtml(mj||'未归类')+' ('+(f.levels[g.level][mj]||0)+')</option>';
          });
        } else {
          var agg = {};
          Object.values(f.levels||{}).forEach(function(mmap){Object.keys(mmap).forEach(function(mj){agg[mj]=(agg[mj]||0)+mmap[mj]})});
          Object.keys(agg).sort().forEach(function(mj){ html += '<option value="'+escHtml(mj)+'"'+(g.major===mj?' selected':'')+'>'+escHtml(mj||'未归类')+' ('+agg[mj]+')</option>'; });
        }
        html += '</select>';
        html += '<select onchange="govSet(\'channel\',this.value)" id="gov-channel" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部渠道</option>';
        Object.keys(f.channel||{}).sort().forEach(function(c){
          html += '<option value="'+escHtml(c)+'"'+(g.channel===c?' selected':'')+'>'+(_CHANNEL_LABELS[c]||c)+' ('+f.channel[c]+')</option>';
        });
        html += '</select>';
        html += '<select onchange="govSet(\'grp\',this.value)" id="gov-grp" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部分组</option>';
        Object.keys(f.grp||{}).sort().forEach(function(gr){
          html += '<option value="'+escHtml(gr)+'"'+(g.grp===gr?' selected':'')+'>'+escHtml(gr)+' ('+f.grp[gr]+')</option>';
        });
        html += '</select>';
        html += '<select onchange="govSet(\'ownership\',this.value)" id="gov-ownership" style="padding:6px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="">全部归属</option><option value="preset"'+(g.ownership==='preset'?' selected':'')+'>预置</option><option value="custom"'+(g.ownership==='custom'?' selected':'')+'>自定义</option></select>';
        html += '<label style="font-size:12px;color:var(--muted);display:inline-flex;align-items:center;gap:4px"><input type="checkbox"'+(g.phase1?' checked':'')+' onchange="govSet(\'phase1\',this.checked)"> 仅本期</label>';
        html += '<span style="font-size:11px;color:var(--muted)">共 '+METRICS.length+' 条</span></div>';
        // Table
        html += '<table style="font-size:12px"><thead><tr><th style="width:24px"><input type="checkbox" onchange="metricsSelectAll(this.checked)" id="metrics-select-all"></th><th>插件 / 溯源</th><th>渠道</th><th>分组</th><th>指标名（Key）</th><th>说明 / 口径</th><th>类型</th><th>单位</th><th>状态</th><th>归属</th><th>操作</th></tr></thead><tbody id="metrics-tbody"></tbody></table>';
        html += '<div id="metrics-pager" style="display:flex;align-items:center;justify-content:center;gap:8px;margin-top:12px;font-size:12px"></div>';
        el.innerHTML = html;
        govTbodyRender();
      }
      var _govDebounceTimer = null;
      function govDebounce() {
        clearTimeout(_govDebounceTimer);
        _govDebounceTimer = setTimeout(function(){
          var el = document.getElementById('gov-search');
          window._gov.query = el ? el.value.trim() : '';
          window._gov.page = 1;
          metricsGovFetch();
        }, 300);
      }
      function govSet(k, v) { window._gov[k] = v; window._gov.page = 1; metricsGovFetch(); }
      function govCascadeMajor() { window._gov.major = ''; }
      function govTbodyRender() {
        var g = window._gov;
        var METRICS = window._METRICS || [];
        var page = g.page||1, ps = g.ps||15;
        var totalPages = Math.max(1, Math.ceil(METRICS.length/ps));
        if (page > totalPages) page = totalPages;
        var pageItems = METRICS.slice((page-1)*ps, page*ps);
        var tbody = document.getElementById("metrics-tbody");
        if (tbody) {
          if (METRICS.length===0) {
            tbody.innerHTML = '<tr><td colspan="11" style="text-align:center;color:var(--muted);padding:20px">无匹配指标（可清空过滤条件重试）</td></tr>';
          } else {
            tbody.innerHTML = pageItems.map(function(m){
              var statusBadge = m.status==='deprecated'?'<span class="badge" style="background:#fef3c7;color:#f59e0b">deprecated</span>':(m.status?'<span class="badge b-h">'+m.status+'</span>':'<span class="badge b-o">-</span>');
              var mk = metricKey(m);
              var nameCell = '<code style="font-size:12px">'+escHtml(mk)+'</code>'
                + ' <span title="复制英文 Key" style="cursor:pointer;color:var(--muted);font-size:10px" onclick="event.stopPropagation();copyKey(\''+mk+'\')">⧉</span>'
                + (mk!==m.name ? '<div style="font-size:10px;color:var(--muted)">'+escHtml(m.name)+'</div>' : '');
              var phaseBadge = m.phase===2 ? ' <span title="二期规划" style="font-size:9px;color:var(--muted)">二期</span>' : '';
              return '<tr class="metrics-row" data-name="'+escHtml(m.name)+'">'
                + '<td><input type="checkbox" class="metrics-cb" value="'+escHtml(m.name)+'" onchange="metricsBatchToggle()"></td>'
                + '<td style="cursor:pointer;max-width:150px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" onclick="showMetricDetail(\''+escHtml(m.name)+'\')"><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+escHtml(m.plugin||'-')+'</span>'+(m.source_ref?'<div style="font-size:10px;color:var(--muted)" title="溯源 '+escHtml(m.source_ref)+'">'+escHtml(m.source_ref)+'</div>':'')+'</td>'
                + '<td style="cursor:pointer" onclick="govSet(\'channel\',\''+escHtml(m.channel)+'\');null">'+(_CHANNEL_LABELS[m.channel]||m.channel||'-')+'</td>'
                + '<td style="cursor:pointer;color:var(--muted)" onclick="showMetricDetail(\''+escHtml(m.name)+'\')">'+escHtml(m.grp||'-')+'</td>'
                + '<td style="cursor:pointer" onclick="showMetricDetail(\''+escHtml(m.name)+'\')">'+actDot(m.expression||m.name)+nameCell+phaseBadge+'</td>'
                + '<td style="cursor:pointer;max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted)" title="'+escHtml(m.desc||'')+'" onclick="showMetricDetail(\''+escHtml(m.name)+'\')">'+escHtml(m.desc||'-')+'</td>'
                + '<td style="cursor:pointer" onclick="showMetricDetail(\''+escHtml(m.name)+'\')"><span style="color:var(--muted)">'+escHtml(m.type)+'</span></td>'
                + '<td style="cursor:pointer" title="'+escHtml(m.unit||'')+'" onclick="showMetricDetail(\''+escHtml(m.name)+'\')">'+unitLabel(m.unit)+'</td>'
                + '<td>'+statusBadge+'</td>'
                + '<td style="cursor:pointer" onclick="govSet(\'ownership\',\''+escHtml(m.ownership)+'\')">'+(_OWNERSHIP_LABELS[m.ownership]||m.ownership||'-')+'</td>'
                + '<td><button class="btn btn-o btn-sm" style="font-size:10px;padding:2px 6px" onclick="event.stopPropagation();showMetricDetail(\''+escHtml(m.name)+'\')">✏️</button></td></tr>';
            }).join('');
          }
        }
        var pager = document.getElementById("metrics-pager");
        if (pager) {
          var ph = '<span style="color:var(--muted)">共 '+METRICS.length+' 条，第 '+page+'/'+totalPages+' 页</span>';
          if (totalPages > 1) {
            ph += '<button class="btn btn-o btn-sm" onclick="window._gov.page=1;govTbodyRender()" '+(page===1?'disabled':'')+'>«</button>';
            ph += '<button class="btn btn-o btn-sm" onclick="window._gov.page=Math.max(1,page-1);govTbodyRender()" '+(page===1?'disabled':'')+'>‹</button>';
            for (var i=Math.max(1,page-2); i<=Math.min(totalPages,page+2); i++) {
              ph += '<button class="btn '+(i===page?'btn-p':'btn-o')+' btn-sm" onclick="window._gov.page='+i+';govTbodyRender()">'+i+'</button>';
            }
            ph += '<button class="btn btn-o btn-sm" onclick="window._gov.page=Math.min(totalPages,page+1);govTbodyRender()" '+(page===totalPages?'disabled':'')+'>›</button>';
            ph += '<button class="btn btn-o btn-sm" onclick="window._gov.page='+totalPages+';govTbodyRender()" '+(page===totalPages?'disabled':'')+'>»</button>';
          }
          pager.innerHTML = ph;
        }
        ensureVmNames(function(){ if (document.getElementById('metrics-tbody')) govTbodyRender(); });
      }
      // ---- Batch ops ----
      function metricsSelectAll(checked) {
        document.querySelectorAll('.metrics-cb').forEach(function(cb){cb.checked=checked});
        metricsBatchToggle();
      }
      function metricsBatchToggle() {
        var checked = document.querySelectorAll('.metrics-cb:checked');
        var bar = document.getElementById('metrics-batch-bar');
        if (bar) {
          bar.style.display = checked.length > 0 ? 'block' : 'none';
          document.getElementById('metrics-batch-count').textContent = checked.length + ' 个选中';
        }
      }
      function metricsGetSelectedNames() {
        var names = [];
        document.querySelectorAll('.metrics-cb:checked').forEach(function(cb){names.push(cb.value)});
        return names;
      }
      function batchUpdateStatus() {
        var names = metricsGetSelectedNames();
        if (names.length===0) return;
        var status = document.getElementById('batch-status').value;
        if (!status) return;
        var updates = names.map(function(n){return {name:n,status:status}});
        fetch(API+'/metrics', {method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(updates)}).then(function(r){return r.json()}).then(function(resp){
          toast('已更新 '+resp.updated+' 个指标','ok');
          renderMetricsCatalog();
        }).catch(function(e){alert('失败: '+e.message)});
      }
      function batchDeleteMetrics() {
        var names = metricsGetSelectedNames();
        if (names.length===0 || !confirm('确定删除 '+names.length+' 个指标？')) return;
        fetch(API+'/metrics', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({metrics:[],delete:names})}).then(function(r){return r.json()}).then(function(){
          renderMetricsCatalog();
        }).catch(function(e){alert('失败: '+e.message)});
      }
      // ---- Metric detail (modal) ----
      function showMetricDetail(name) {
        var m = (window._METRICS||[]).find(function(x){return x.name===name});
        if (!m) return;
        renderOverlay('指标明细', function(){
          var h = '<div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;font-size:13px">';
          var mkd = metricKey(m);
          h += '<div><b>英文 Key</b><br><code style="font-size:13px">'+escHtml(mkd)+'</code> <button class="btn btn-o btn-sm" style="font-size:10px;padding:1px 6px" onclick="copyKey(\''+mkd+'\')">⧉ 复制</button>'
            + (mkd!==m.name ? '<div style="font-size:11px;color:var(--muted)">展示名：'+escHtml(m.name)+'</div>' : '')+'</div>';
          h += '<div><b>来源</b><br><span style="color:var(--muted)">'+(m.source||'manual')+'</span></div>';
          h += '<div><b>状态</b><br><select id="md-status" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"><option value="active"'+(m.status==='active'?' selected':'')+'>active</option><option value="deprecated"'+(m.status==='deprecated'?' selected':'')+'>deprecated</option></select></div>';
          h += '<div><b>更新时间</b><br><span style="color:var(--muted)">'+(m.updated_at||'-')+'</span></div>';
          h += '<div><b>插件</b><br><input value="'+m.plugin+'" id="md-plugin" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div><b>类别</b><br><input value="'+m.cat+'" id="md-cat" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div><b>类型</b><br><select id="md-type" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"><option value="gauge"'+(m.type==='gauge'?' selected':'')+'>gauge</option><option value="counter"'+(m.type==='counter'?' selected':'')+'>counter</option><option value="histogram"'+(m.type==='histogram'?' selected':'')+'>histogram</option><option value="summary"'+(m.type==='summary'?' selected':'')+'>summary</option></select></div>';
          h += '<div><b>单位</b><br><input value="'+(m.unit||'')+'" id="md-unit" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div style="grid-column:1/-1"><b>说明</b><br><input value="'+(m.desc||'')+'" id="md-desc" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div style="grid-column:1/-1"><b>标签</b><br><input value="'+(m.labels||'')+'" id="md-labels" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px" placeholder="逗号分隔"></div>';
          var hasData = vmHasData(m.expression||m.name);
          h += '<div style="grid-column:1/-1"><b>近 24h 上报状态</b><br>'+(hasData
            ? '<span style="color:var(--success,#2ea043)">● 有数据上报</span>'
            : '<span style="color:var(--muted)">● 无数据上报（目录注册了但采集侧没报）</span>')+'</div>';
          h += '<div style="grid-column:1/-1"><b>仪表盘引用</b><br><span id="md-refs" style="color:var(--muted)">检查中...</span></div>';
          h += '</div><div style="margin-top:16px;display:flex;gap:8px">';
          h += '<button class="btn btn-p" onclick="saveMetricEdit(\''+m.name+'\')">💾 保存</button>';
          h += '<button class="btn btn-o" onclick="closeOverlay()">取消</button>';
          h += '<button class="btn btn-o" style="color:var(--error);margin-left:auto" onclick="deleteMetric(\''+m.name+'\')">🗑 删除</button>';
          h += '</div>';
          return h;
        });
        fetch(API+'/metrics/refs?name='+encodeURIComponent(name)).then(function(r){return r.json()}).then(function(res){
          var el = document.getElementById('md-refs'); if (!el) return;
          if (res.error) { el.textContent = '查询失败'; return; }
          if (!res.count) { el.innerHTML = '<span style="color:var(--muted)">无仪表盘引用，可安全下线/删除</span>'; return; }
          el.innerHTML = '<span style="color:var(--warn,#d29922)">⚠ 被 '+res.count+' 个仪表盘引用</span>：'
            + res.items.map(function(x){ return escHtml(x.plugin+' / '+x.dashboard); }).join('、');
        }).catch(function(){ var el = document.getElementById('md-refs'); if (el) el.textContent = '查询失败'; });
      }
      function deleteMetric(name) {
        if (!confirm('确定删除指标 '+name+' ？')) return;
        fetch(API+'/metrics?name='+name, {method:'DELETE'}).then(function(r){return r.json()}).then(function(){
          renderMetricsCatalog();
        }).catch(function(e){alert('失败: '+e.message)});
      }
      function saveMetricEdit(name) {
        var m = {
          name:name, plugin:document.getElementById('md-plugin').value,
          cat:document.getElementById('md-cat').value, type:document.getElementById('md-type').value,
          unit:document.getElementById('md-unit').value, desc:document.getElementById('md-desc').value,
          labels:document.getElementById('md-labels').value, status:document.getElementById('md-status').value
        };
        fetch(API+'/metrics', {method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify([m])}).then(function(r){return r.json()}).then(function(){
          closeOverlay(); toast('指标已更新','ok');
          renderMetricsCatalog();
        }).catch(function(e){alert('失败: '+e.message)});
      }
      // ---- Add metric modal ----
      function openAddMetricModal() {
        renderOverlay('➕ 新增指标', function(){
          var h = '<div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;font-size:13px">';
          h += '<div style="grid-column:1/-1"><b>指标名</b><br><input id="am-name" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px" placeholder="例: host_cpu_percent"></div>';
          h += '<div><b>插件</b><br><input id="am-plugin" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px" value="manual"></div>';
          h += '<div><b>类别</b><br><input id="am-cat" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div><b>类型</b><br><select id="am-type" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"><option>gauge</option><option>counter</option><option>histogram</option><option>summary</option></select></div>';
          h += '<div><b>单位</b><br><input id="am-unit" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div style="grid-column:1/-1"><b>说明</b><br><input id="am-desc" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px"></div>';
          h += '<div style="grid-column:1/-1"><b>标签</b><br><input id="am-labels" style="width:100%;padding:4px 6px;border:1px solid var(--border);border-radius:4px" placeholder="逗号分隔"></div>';
          h += '</div><div style="margin-top:16px"><button class="btn btn-p" onclick="saveNewMetric()">💾 创建</button><button class="btn btn-o" onclick="closeOverlay()" style="margin-left:8px">取消</button></div>';
          return h;
        });
      }
      function saveNewMetric() {
        var m = {
          name:document.getElementById('am-name').value, plugin:document.getElementById('am-plugin').value,
          cat:document.getElementById('am-cat').value, type:document.getElementById('am-type').value,
          unit:document.getElementById('am-unit').value, desc:document.getElementById('am-desc').value,
          labels:document.getElementById('am-labels').value, source:'manual', status:'active'
        };
        if (!m.name) { alert('请输入指标名'); return; }
        fetch(API+'/metrics', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({metrics:[m],delete:[]})}).then(function(r){return r.json()}).then(function(){
          closeOverlay(); toast('指标已创建','ok');
          renderMetricsCatalog();
        }).catch(function(e){alert('失败: '+e.message)});
      }
      // ---- VM Import modal ----
      function openVmModal() {
        renderOverlay('🔍 从 VictoriaMetrics 导入指标', function(){
          return '<div id="vm-modal-body" style="min-height:200px">⏳ 正在查询 VictoriaMetrics...</div>';
        }, true);
        fetchVmMetrics();
      }
      function fetchVmMetrics() {
        var body = document.getElementById('vm-modal-body');
        if (!body) return;
        var prefixes = ["host_", "mysql_", "custom_", "redis_", "kafka_", "elasticsearch_", "pg_"];
        Promise.all(prefixes.map(function(p){
          return fetch(API+"/vm/metrics?prefix="+p).then(function(r){return r.json()});
        })).then(function(results){
          var all = [];
          results.forEach(function(r){all=all.concat(r.data||[])});
          all.sort();
          var catalogNames = (window._METRICS||[]).map(function(m){return m.name});
          window._vmAll = all;
          window._vmCatalogNames = catalogNames;
          var inCat = all.filter(function(n){return catalogNames.indexOf(n)>=0});
          var notInCat = all.filter(function(n){return catalogNames.indexOf(n)<0});
          window._vmContext = {inCat: inCat, notInCat: notInCat};
          var h = '<div style="padding:8px 0;font-size:12px;display:flex;gap:16px;align-items:center;flex-wrap:wrap">';
          h += '共 <b>'+all.length+'</b> 个，<span style="color:var(--success)">'+inCat.length+'</span> 已在目录，<span style="color:var(--warn)">'+notInCat.length+'</span> 可导入';
          h += '<input placeholder="搜索..." oninput="vmModalFilter()" style="padding:4px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px;width:200px" id="vm-modal-search">';
          h += '<select onchange="vmModalFilter()" id="vm-modal-prefix" style="padding:4px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px"><option value="all">全部</option><option value="host_">host_</option><option value="mysql_">mysql_</option><option value="custom_">custom_</option><option value="redis_">redis_</option><option value="kafka_">kafka_</option><option value="elasticsearch_">elasticsearch_</option><option value="pg_">pg_</option></select>';
          h += '<span style="flex:1"></span>';
          h += '<button class="btn btn-o btn-sm" onclick="vmModalSelectAll()">全选</button><button class="btn btn-o btn-sm" onclick="vmModalDeselectAll()">取消</button>';
          h += '<button class="btn btn-p btn-sm" onclick="vmModalImport()">📥 导入选中</button>';
          h += '</div>';
          h += '<div style="overflow:auto;max-height:55vh"><table style="font-size:12px;width:100%"><thead style="position:sticky;top:0;background:var(--card);z-index:1"><tr><th style="width:24px">☐</th><th>指标名</th><th style="width:80px">状态</th></tr></thead><tbody id="vm-modal-tbody">';
          notInCat.forEach(function(n){
            h += '<tr class="vm-modal-row"><td><input type="checkbox" class="vm-modal-cb" value="'+n.replace(/"/g,'&quot;')+'"></td><td><code>'+n+'</code></td><td><span class="badge b-o">未纳入</span></td></tr>';
          });
          inCat.forEach(function(n){
            h += '<tr class="vm-modal-row"><td><input type="checkbox" class="vm-modal-cb" value="'+n.replace(/"/g,'&quot;')+'"></td><td><code>'+n+'</code></td><td><span class="badge b-h">已纳入</span></td></tr>';
          });
          h += '</tbody></table></div>';
          body.innerHTML = h;
          document.getElementById('vm-modal-tbody').parentElement.style.tableLayout = 'fixed';
        }).catch(function(e){ body.innerHTML = '<div style="color:var(--error)">查询失败: '+e.message+'</div>'; });
      }
      function vmModalFilter() {
        var q = ((document.getElementById('vm-modal-search')||{}).value||'').toLowerCase();
        var pf = (document.getElementById('vm-modal-prefix')||{}).value||'all';
        document.querySelectorAll('.vm-modal-row').forEach(function(r){
          var txt = r.textContent.toLowerCase();
          r.style.display = (!q || txt.indexOf(q)>=0) && (pf==='all' || txt.indexOf(pf)===0) ? '' : 'none';
        });
      }
      function vmModalSelectAll() {
        document.querySelectorAll('.vm-modal-cb').forEach(function(cb){cb.checked=true});
      }
      function vmModalDeselectAll() {
        document.querySelectorAll('.vm-modal-cb').forEach(function(cb){cb.checked=false});
      }
      function vmModalImport() {
        var checked = document.querySelectorAll('.vm-modal-cb:checked');
        if (checked.length===0) { alert('请选择要导入的指标'); return; }
        var prefixToPlugin = {
          'host_':'host_metrics','mysql_':'mysql_probe','custom_':'custom_scripts',
          'redis_':'redis_exporter','kafka_':'kafka_exporter','elasticsearch_':'elasticsearch_exporter','pg_':'postgres_exporter'
        };
        var METRICS = window._METRICS || [];
        var toAdd = [];
        var skipped = [];
        checked.forEach(function(cb){
          var name = cb.value.replace(/&quot;/g,'"');
          if (!METRICS.find(function(m){return m.name===name})) {
            var plugin = 'manual';
            for (var p in prefixToPlugin) {
              if (name.indexOf(p) === 0) { plugin = prefixToPlugin[p]; break; }
            }
            toAdd.push({plugin:plugin,cat:'自动发现',name:name,type:'unknown',unit:'',desc:'从 VM 反向发现',labels:'',source:'vm_discovered',status:'active'});
          } else {
            skipped.push(name);
          }
        });
        if (toAdd.length===0) { alert('选中指标均已在目录中，无需重复导入'); return; }
        var msg = '将导入 '+toAdd.length+' 个指标';
        if (skipped.length) msg += '\n另有 '+skipped.length+' 个已在目录，将跳过：'+skipped.slice(0,5).join('、')+(skipped.length>5?' 等':'');
        if (!confirm(msg)) return;
        fetch(API+'/metrics', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({metrics:toAdd,delete:[]})}).then(function(r){return r.json()}).then(function(){
          toast('成功导入 '+toAdd.length+' 个指标','ok');
          closeOverlay();
          renderMetricsCatalog();
        }).catch(function(e){alert('导入失败: '+e.message)});
      }
      // ---- Overlay system (replaces modal) ----
      function renderOverlay(title, contentFn, wide, backFn) {
        if (backFn) window._prevView = backFn; // 未传 backFn 时由 closeOverlay 兜底回 currentPage
        var h = '<div class="card"><div class="card-hd">'+title+' <button class="btn btn-o btn-sm" style="float:right" onclick="closeOverlay()">← 返回</button></div><div class="card-bd" style="'+(wide?'max-width:80vw;margin:0 auto':'')+'" id="overlay-body"></div></div>';
        document.getElementById("main-content").innerHTML = h;
        var body = document.getElementById('overlay-body');
        if (body) body.innerHTML = contentFn();
      }
      function closeOverlay() {
        if (window._prevView) { window._prevView(); return; }
        // 无记忆时回当前导航页（currentPage 由 goPage 维护），而非写死的插件页
        if (typeof currentPage === 'string' && typeof goPage === 'function') { goPage(currentPage); return; }
        renderPluginsMart();
      }
      // ===================================================================
      //  INTERACTIVE: Detail panel, batch ops, navigation
      // ===================================================================
      function openDetailById(id) {
        var a = agents.find(function(x) { return x.id === id; });
        if (a) openDetail(a);
      }
      function openDetail(agent) {
        currentDetailAgent = agent;
        document.getElementById("detail-title").textContent = agent.name || agent.id;
        document.getElementById("detail-panel").classList.add("open");
        switchDetailTab("plugins");
      }
      function closeDetail() {
        document.getElementById("detail-panel").classList.remove("open");
        currentDetailAgent = null;
      }
      function switchDetailTab(tab) {
        currentDetailTab = tab;
        document.querySelectorAll(".detail-tabs span").forEach(function(s) {
          s.classList.toggle("active", s.dataset.dtab === tab);
        });
        var a = currentDetailAgent;
        if (!a) return;
        var html = "";
        if (tab === "plugins") {
          html = '<div style="padding:8px 0">';
          (a.plugins||[]).forEach(function(p) {
            var m = ((_onboardCfg && _onboardCfg.manageable_plugins) || ["log_metrics","mysql_probe","custom_scripts"]).indexOf(p) >= 0;
            html += '<div class="wiz-plugin-row"><div><b>'+p+'</b></div>'+
              (m ? '<div class="plugin-actions" id="pa-'+p+'">加载中...</div>' : '<span class="badge" style="background:#f0f0f0;color:#999">内置</span>')+
              '</div>';
          });
          html += '</div>';
          html += '<div style="margin-top:12px;padding-top:8px;border-top:1px solid var(--border)">'+
            (a.status==="stopped" ? '<button class="btn btn-s btn-sm" onclick="quickStart(\''+a.id+'\')">▶ 启动 Agent</button>' : '<button class="btn btn-d btn-sm" onclick="batchOpSingle(\''+a.id+'\',\'stop\')">⏹ 停止 Agent</button>')+
            '</div>';
          document.getElementById("detail-content").innerHTML = html;
          setTimeout(function() { loadPluginStatus(a.id); }, 100);
        } else if (tab === "config") {
          html = '<div style="font-size:13px;color:var(--muted);text-align:center;padding:30px">加载中...</div>';
          document.getElementById("detail-content").innerHTML = html;
          fetch(API+"/agents/"+a.id+"/config").then(function(r){return r.json()}).then(function(d) {
            // 配置版本对照（期望 vs 生效）
            var eff = d.cfg_effective || '';
            var want = d.version || d.cfg_desired || 0;
            var synced = a.source !== 'heartbeat' || (eff !== '' && eff === ('cfg-'+want));
            var verLine = '<div style="display:flex;gap:12px;align-items:center;margin-bottom:10px;font-size:12px;flex-wrap:wrap">'
              + '<span>配置版本: <b>cfg-'+want+'</b></span>'
              + '<span>Agent 生效: <b>'+(a.source==='heartbeat'?(eff||'未上报'):'随容器生效')+'</b></span>'
              + (synced
                ? '<span class="badge b-h">✅ 已同步</span>'
                : '<span class="badge" style="background:#fef3c7;color:#f59e0b">⚠️ 待下发（Agent 下次心跳将拉取）</span>')
              + '<span style="margin-left:auto;font-size:11px;color:var(--muted)">指标配置调整走配置轨道（版本 +1 热生效），不涉及 Agent 代码升级</span>'
              + '</div>';
            // 结构化视图：targets 段 + host_metrics 段
            var parsed = d.parsed || {};
            var targets = parsed.targets || [];
            var hm = parsed.host_metrics || {};
            var grpLabels = {cpu:'CPU',memory:'内存',disk:'磁盘 IO',filesystem:'文件系统',network:'网络',netstack:'网络协议栈',loadproc:'负载与进程',kernel:'内核与 VM',system:'系统信息',custom:'平台自定义'};
            var groupChips = '';
            var grpKeys = Object.keys(hm.groups||{});
            // groups 未声明 = 开启；展示已声明的关闭项 + 说明
            var offGroups = grpKeys.filter(function(k){return hm.groups[k]===false});
            if (offGroups.length===0) {
              groupChips = '<span class="badge b-h">全部分组开启</span>';
            } else {
              groupChips = offGroups.map(function(k){return '<span class="badge" style="background:#fee2e2;color:var(--error)" title="已关闭">'+escHtml(grpLabels[k]||k)+' ✕</span>'}).join(' ')
                + ' <span style="font-size:11px;color:var(--muted)">其余分组开启</span>';
            }
            var excl = hm.exclude_metrics || [];
            var h = verLine;
            h += '<div style="display:grid;gap:10px;font-size:12px">';
            // targets 段
            h += '<div style="border:1px solid var(--border);border-radius:8px;overflow:hidden"><div style="padding:8px 12px;background:rgba(128,128,128,0.06);font-weight:600">🎯 采集目标（'+targets.length+'）<span style="font-weight:400;font-size:10px;color:var(--muted);margin-left:8px">采集目标页变更自动同步</span></div>';
            if (targets.length===0) { h += '<div style="padding:8px 12px;color:var(--muted)">暂无分派目标</div>'; }
            else {
              h += '<table style="font-size:12px;width:100%"><thead><tr style="color:var(--muted);text-align:left"><th style="padding:4px 12px">插件</th><th style="padding:4px 12px">目标</th><th style="padding:4px 12px">地址</th></tr></thead><tbody>';
              targets.forEach(function(t){ h += '<tr><td style="padding:4px 12px"><span class="badge" style="background:#e0f2f1;color:#0b6e66">'+escHtml(t.plugin||'-')+'</span></td><td style="padding:4px 12px">'+escHtml(t.target||'-')+'</td><td style="padding:4px 12px"><code>'+escHtml(t.address||'-')+'</code></td></tr>'; });
              h += '</tbody></table>';
            }
            h += '</div>';
            // host_metrics 段
            h += '<div style="border:1px solid var(--border);border-radius:8px;overflow:hidden"><div style="padding:8px 12px;background:rgba(128,128,128,0.06);font-weight:600;display:flex;align-items:center">📈 主机指标（host_metrics）'
              + '<span style="margin-left:8px;font-weight:400;font-size:10px;color:var(--muted)">配置轨道 · 界面增删指标 = 配置版本 +1</span>'
              + '<button class="btn btn-p btn-sm" style="margin-left:auto" onclick="openAgentCfgEditor(\''+a.id+'\')">✏️ 编辑指标配置</button></div>'
              + '<div style="padding:10px 12px;display:grid;gap:8px">'
              + '<div>采集状态: <b>'+(hm.enabled===false?'<span style="color:var(--error)">已停用</span>':'<span style="color:var(--success)">启用</span>')+'</b>'
              + ' &nbsp;·&nbsp; 采集频率: <b>'+escHtml(hm.interval||'30s')+'</b>'
              + ' &nbsp;·&nbsp; 指标范围: <span class="badge" style="background:#f0f0f0;color:#666">'+(hm.exclude_metrics||[]).length+' 项例外排除</span></div>'
              + '<div>分组开关: '+groupChips+'</div>'
              + (excl.length ? '<div>例外清单: '+excl.map(function(x){return '<code style="font-size:11px;background:rgba(128,128,128,0.12);padding:1px 5px;border-radius:3px;margin-right:4px">'+escHtml(x)+'</code>'}).join('')+'</div>' : '')
              + '</div></div>';
            h += '</div>';
            // 原始 JSON 折叠
            var t = d.content || '';
            try { t = JSON.stringify(JSON.parse(t), null, 2); } catch(e) {}
            h += '<details style="margin-top:12px"><summary style="font-size:12px;color:var(--muted);cursor:pointer">查看原始配置 JSON</summary>'
              + '<pre style="font-size:11px;background:#1e1e1e;color:#d4d4d4;padding:12px;border-radius:6px;overflow:auto;max-height:300px;line-height:1.5;margin-top:8px">'+escHtml(t)+'</pre></details>';
            document.getElementById("detail-content").innerHTML = h;
          }).catch(function(){ document.getElementById("detail-content").innerHTML = '<div style="color:var(--error);padding:20px">加载失败</div>'; });
        } else if (tab === "metrics") {
          html = '<div style="font-size:13px;color:var(--muted);text-align:center;padding:30px">加载指标...</div>';
          document.getElementById("detail-content").innerHTML = html;
          fetch(API+"/agents/"+a.id+"/metrics").then(function(r){return r.text()}).then(function(t) {
            function getVal(name) { var m=t.match(new RegExp('^'+name+'\\{[^}]*\\}\\s+([0-9.e+\\-]+)','m')); return m?parseFloat(m[1]):null; }
            var cpu=getVal('host_cpu_percent')||getVal('cpu_usage_percent');
            var mem=getVal('host_memory_used_percent')||getVal('mem_usage_percent');
            var disk=getVal('host_disk_used_percent')||getVal('disk_usage_percent');
            var all=t.split('\n').filter(function(l){return l.trim()&&!l.startsWith('#')});
            var cards=[['CPU 使用率',cpu,'%'],['内存使用率',mem,'%'],['磁盘使用率',disk,'%'],['总指标数',all.length,'条']];
            var gg=function(v){return v!=null?v.toFixed(1):'--'};
            var cc=function(label){return label.indexOf('CPU')>=0||label.indexOf('内存')>=0||label.indexOf('磁盘')>=0?'color:'+(parseFloat(gg(cards.filter(function(c){return c[0]===label})[0][1]))>80?'var(--error)':'var(--success)')+';':'color:var(--primary);';};
            var out='<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;margin-bottom:12px">';
            cards.forEach(function(c){out+='<div style="padding:20px;background:var(--bg);border-radius:8px;text-align:center"><div style="font-size:32px;font-weight:700;'+cc(c[0])+'">'+gg(c[1])+' <span style="font-size:14px;font-weight:400">'+c[2]+'</span></div><div style="font-size:12px;color:var(--muted);margin-top:6px">'+c[0]+'</div></div>';});
            out+='</div><div style="font-size:11px;color:var(--muted)">共 '+all.length+' 条指标 · <a href="'+(_siteCfg?_siteCfg.grafana_url:location.protocol+'//'+location.hostname+':3000')+'" target="_blank" style="color:var(--primary)">📊 Grafana →</a></div>';
            document.getElementById("detail-content").innerHTML=out;
          }).catch(function(){document.getElementById("detail-content").innerHTML='<div style="text-align:center;padding:40px;color:var(--muted)"><div style="font-size:36px;margin-bottom:8px">📊</div>无法获取指标<br><a href="'+(_siteCfg?_siteCfg.grafana_url:location.protocol+'//'+location.hostname+':3000')+'" target="_blank" style="color:var(--primary);font-size:12px">Grafana →</a></div>';});
        } else if (tab === "log") {
          html = '<div style="font-size:13px;color:var(--muted);text-align:center;padding:30px">加载中...</div>';
          document.getElementById("detail-content").innerHTML = html;
          fetch(API+"/agents/"+a.id+"/logs").then(function(r){return r.text()}).then(function(t) {
            document.getElementById("detail-content").innerHTML = '<div style="background:#1a1a2e;color:#c0caf5;font-family:monospace;font-size:12px;padding:14px;border-radius:6px;max-height:500px;overflow:auto;white-space:pre-wrap;line-height:1.4">'+t.replace(/</g,'&lt;').replace(/>/g,'&gt;')+'</div>';
          }).catch(function(){ document.getElementById("detail-content").innerHTML = '<div style="color:var(--error);padding:20px">无法获取日志</div>'; });
        } else if (tab === "trouble") {
          // M1-⑧ 排障视图：心跳 / 成功率 / 配置同步 / 分派目标 / 最近操作
          var age = a.last_seen ? Math.round(Date.now()/1000) - a.last_seen : -1;
          var ageTxt = age < 0 ? '从未心跳' : (age < 60 ? age+'s 前' : (age < 3600 ? Math.round(age/60)+' 分钟前' : Math.round(age/3600)+' 小时前'));
          var st = a.stats || {};
          var sOk = parseInt(st.success)||0, sFail = parseInt(st.fail)||0;
          var rate = (sOk+sFail)>0 ? (Math.round(sOk/(sOk+sFail)*1000)/10)+'%（成功 '+sOk+' / 失败 '+sFail+'）' : '暂无采集统计数据';
          var cfgLine = '';
          if (a.source === 'heartbeat') {
            var synced = a.cfg_effective !== '' && a.cfg_effective === ('cfg-'+(a.cfg_desired||0));
            cfgLine = '<tr><td>配置同步</td><td>期望 cfg-'+(a.cfg_desired||0)+' / 生效 '+(a.cfg_effective||'未上报')+'</td><td>'+(synced?'<span class="badge b-h">✅ 已同步</span>':'<span class="badge" style="background:#fef3c7;color:#f59e0b">⚠️ 待下发</span>')+'</td></tr>';
          }
          document.getElementById("detail-content").innerHTML = '<div style="font-size:13px"><div style="color:var(--muted);padding:6px 0">⏳ 加载分派目标与最近操作...</div></div>';
          Promise.all([
            fetch(API+'/targets').then(function(r){return r.json()}),
            fetch(API+'/recon').then(function(r){return r.json()}).catch(function(){return null}),
            fetch(API+'/audit').then(function(r){return r.json()}).catch(function(){return []})
          ]).then(function(rs){
            var tgts = rs[0]||[], recon = rs[1], audits = rs[2]||[];
            var mine = tgts.filter(function(t){return t.agent_id===a.id});
            var staleIds = {};
            if (recon && recon.stale) recon.stale.forEach(function(t){staleIds[t.id]=true});
            var recent = audits.filter(function(e){return (e.target||'').indexOf(a.id)>=0}).slice(-5).reverse();
            var h = '<table style="font-size:12px"><thead><tr><th style="width:90px">检查项</th><th>现状</th><th style="width:130px">判定</th></tr></thead><tbody>';
            if (a.source === 'heartbeat') {
              h += '<tr><td>心跳</td><td>最近心跳：'+ageTxt+'（间隔约定 30s，90s 判离线）</td><td>'+(age>=0&&age<=90?'<span class="badge b-h">正常</span>':(age<0?'<span class="badge b-o">未知</span>':'<span class="badge" style="background:#fee2e2;color:var(--error)">超时</span>'))+'</td></tr>';
            } else {
              h += '<tr><td>容器状态</td><td>本机演示 Agent，状态由 Docker 实时探测（不经心跳）</td><td>'+(a.status==='healthy'?'<span class="badge b-h">运行中</span>':'<span class="badge" style="background:#fee2e2;color:var(--error)">'+escHtml(a.status)+'</span>')+'</td></tr>';
            }
            h += '<tr><td>采集成功率</td><td>'+rate+'</td><td>'+(sFail===0&&sOk>0?'<span class="badge b-h">正常</span>':(sFail>0?'<span class="badge" style="background:#fef3c7;color:#f59e0b">有失败</span>':'<span class="badge b-o">无数据</span>'))+'</td></tr>';
            h += cfgLine;
            h += '<tr><td>分派目标</td><td>'+(mine.length?mine.map(function(t){return escHtml(t.name)+'（'+escHtml(t.plugin||t.type)+'）'}).join('、'):'未分派任何采集目标')+'</td><td>'+(mine.length?'<span class="badge b-h">'+mine.length+' 个</span>':'<span class="badge b-o">空</span>')+'</td></tr>';
            var mineStale = mine.filter(function(t){return staleIds[t.id]});
            h += '<tr><td>断采检测</td><td>'+(mineStale.length?mineStale.map(function(t){return escHtml(t.name)}).join('、')+' 近 10min 无数据上报':(mine.length?'分派目标全部在报':'无可检查目标'))+'</td><td>'+(mineStale.length?'<span class="badge" style="background:#fee2e2;color:var(--error)">断采 '+mineStale.length+'</span>':'<span class="badge b-h">正常</span>')+'</td></tr>';
            // 插件级采集条数：心跳 stats 中除 success/fail 外的键值对（plugin → 最近批次条数）
            var pluginCounts = Object.keys(st).filter(function(k){return k!=='success'&&k!=='fail'});
            var pcCell;
            if (pluginCounts.length===0) {
              pcCell = '<span style="color:var(--muted)">心跳未携带插件级条数（旧版本 Agent）</span>';
            } else {
              pcCell = pluginCounts.map(function(k){return '<span class="badge" style="background:#f0f0f0;color:#666;margin-right:4px">'+escHtml(k)+': '+escHtml(String(st[k]))+'</span>'}).join('');
            }
            h += '<tr><td>插件采集条数</td><td>'+pcCell+'</td><td>'+(pluginCounts.length?'<span class="badge b-o">参考</span>':'<span class="badge b-o">无数据</span>')+'</td></tr>';
            h += '</tbody></table>';
            h += '<div style="margin-top:12px;font-size:12px"><b>最近操作（审计）</b>';
            if (recent.length===0) { h += '<div style="color:var(--muted);padding:6px 0">暂无与该 Agent 相关的操作记录</div>'; }
            else {
              h += '<table style="font-size:11px;margin-top:6px"><tbody>';
              recent.forEach(function(e){ h += '<tr><td style="padding:3px 6px;color:var(--muted)">'+escHtml(e.time||'')+'</td><td style="padding:3px 6px">'+escHtml(e.action||'')+'</td><td style="padding:3px 6px">'+escHtml(e.detail||e.target||'')+'</td></tr>'; });
              h += '</tbody></table>';
            }
            h += '</div>';
            h += '<div style="margin-top:10px;font-size:11px;color:var(--muted)">排障提示：断采时先看「配置同步」是否待下发 → 再用目标页「测试」验证网络连通性 → 最后查看日志 tab 定位采集进程错误。</div>';
            document.getElementById("detail-content").innerHTML = '<div style="font-size:13px">'+h+'</div>';
          }).catch(function(){
            document.getElementById("detail-content").innerHTML = '<div style="font-size:13px"><table><tbody><tr><td>心跳</td><td>'+ageTxt+'</td></tr><tr><td>采集成功率</td><td>'+rate+'</td></tr></tbody></table><div style="color:var(--error);margin-top:10px">分派目标/审计加载失败</div></div>';
          });
        }
      }
      // ---- host_metrics 指标配置编辑（配置轨道：分组开关 + 例外清单 + 频率，版本 +1 热生效） ----
      var _GRP_LABELS_CFG = {cpu:'CPU',memory:'内存',disk:'磁盘 IO',filesystem:'文件系统',network:'网络',netstack:'网络协议栈',loadproc:'负载与进程',kernel:'内核与 VM',system:'系统信息',custom:'平台自定义'};
      function openAgentCfgEditor(agentId) {
        Promise.all([
          fetch(API+'/agents/'+agentId+'/config').then(function(r){return r.json()}),
          fetch(API+'/metrics/gov?plugin_name=host_metrics&only_phase1=1').then(function(r){return r.json()})
        ]).then(function(rs){
          var d = rs[0] || {};
          var govList = rs[1] || [];
          // 分组清单从治理目录动态推导（组=指标 grp 值 distinct），新分组注册后自动出现
          var grpCounts = {};
          govList.forEach(function(m){ if (m.grp) grpCounts[m.grp] = (grpCounts[m.grp]||0)+1; });
          var parsed = d.parsed || {};
          var hm = parsed.host_metrics || {};
          var curGroups = hm.groups || {};
          var excl = (hm.exclude_metrics || []).join('\n');
          var ver = d.version || 0;
          renderOverlay('✏️ 编辑主机指标配置 <span style="font-weight:400;font-size:11px;color:var(--muted)">'+escHtml(agentId)+' · 当前 cfg-'+ver+' → 保存后版本 +1，心跳热生效</span>', function(){
            var h = '<div style="display:grid;gap:12px;font-size:13px;max-width:620px">';
            h += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">'
              + '<div><b>采集状态</b><br><select id="hm-enabled" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">'
              + '<option value="on"'+(hm.enabled!==false?' selected':'')+'>启用</option><option value="off"'+(hm.enabled===false?' selected':'')+'>停用</option></select></div>'
              + '<div><b>采集频率</b><br><select id="hm-interval" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px">';
            ['15s','30s','60s','5m'].forEach(function(iv){
              h += '<option value="'+iv+'"'+((hm.interval||'30s')===iv?' selected':'')+'>'+iv+'</option>';
            });
            var isCustomIv = ['15s','30s','60s','5m'].indexOf(hm.interval||'30s')<0;
            h += '<option value="__custom"'+(isCustomIv?' selected':'')+'>自定义…</option></select>';
            h += '<input id="hm-interval-custom" value="'+(isCustomIv?escHtml(hm.interval||''):'')+'" placeholder="例: 45s / 2m" style="width:100%;padding:5px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px;margin-top:4px;display:'+(isCustomIv?'block':'none')+'"></div></div>';
            h += '<div><b>指标分组开关</b> <span style="font-weight:400;font-size:11px;color:var(--muted)">未勾选 = 关闭该组采集；组内指标清单见「指标中心」分组过滤</span><br>';
            h += '<div style="display:grid;grid-template-columns:repeat(2,1fr);gap:6px;margin-top:6px">';
            Object.keys(grpCounts).sort().forEach(function(g){
              var on = curGroups[g] !== false; // 未声明默认开启
              var label = _GRP_LABELS_CFG[g] || g;
              h += '<label style="display:inline-flex;align-items:center;gap:6px;padding:6px 10px;border:1px solid var(--border);border-radius:6px;font-size:12px;cursor:pointer">'
                + '<input type="checkbox" class="hm-grp-cb" data-grp="'+escHtml(g)+'"'+(on?' checked':'')+'>'
                + escHtml(label) + ' <span style="color:var(--muted);font-size:10px">'+g+' · '+grpCounts[g]+' 项</span></label>';
            });
            h += '</div></div>';
            h += '<div><b>例外清单（exclude_metrics）</b> <span style="font-weight:400;font-size:11px;color:var(--muted)">每行一个指标 Key，从输出中排除单条指标</span><br>'
              + '<textarea id="hm-exclude" rows="4" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px;font-family:ui-monospace,monospace;font-size:12px" placeholder="例:&#10;node_cpu_core_throttle_cycles_total">'+escHtml(excl)+'</textarea></div>';
            h += '<div style="display:flex;gap:8px"><button class="btn btn-p" onclick="saveAgentCfg(\''+agentId+'\')">💾 保存（版本 +1 下发生效）</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
            h += '</div>';
            return h;
          }, true, function(){ if (currentDetailAgent && currentDetailAgent.id===agentId) { openDetailById(agentId); switchDetailTab('config'); } });
          // 自定义频率输入框显隐
          var sel = document.getElementById('hm-interval');
          if (sel) sel.onchange = function(){ document.getElementById('hm-interval-custom').style.display = sel.value==='__custom'?'block':'none'; };
        }).catch(function(e){ toast('加载配置失败: '+e, 'err'); });
      }
      function saveAgentCfg(agentId) {
        var enabled = document.getElementById('hm-enabled').value !== 'off';
        var interval = document.getElementById('hm-interval').value;
        if (interval === '__custom') {
          interval = document.getElementById('hm-interval-custom').value.trim() || '30s';
        }
        var groups = {};
        document.querySelectorAll('.hm-grp-cb').forEach(function(cb){
          groups[cb.dataset.grp] = cb.checked; // 显式声明每个组（含 true），便于对账展开与界面回显
        });
        var exclude = document.getElementById('hm-exclude').value.split('\n').map(function(s){return s.trim()}).filter(Boolean);
        var body = {host_metrics: {enabled: enabled, interval: interval, groups: groups, exclude_metrics: exclude}};
        fetch(API+'/agents/'+agentId+'/config', {method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
          .then(function(r){return r.json()}).then(function(res){
            if (res.error) { toast('保存失败: '+res.error, 'err'); return; }
            closeOverlay();
            toast('✅ 指标配置已保存（cfg-'+res.version+'），Agent 下次心跳（≤30s）拉取生效', 'ok');
            refreshAgentsSoon(agentId);
          }).catch(function(e){ toast('保存失败: '+e, 'err'); });
      }
      function refreshAgentsSoon(agentId) {
        fetch(API+'/agents').then(function(r){return r.json()}).then(function(list){ agents = list||[]; });
        setTimeout(function(){ if (currentDetailAgent && currentDetailAgent.id===agentId) switchDetailTab('config'); }, 800);
      }
      function loadPluginStatus(agentId) {
        var agent = agents.find(function(a){return a.id===agentId});
        if (agent && (agent.status==="stopped"||agent.status==="offline"||agent.status==="created")) {
          var manageable = ["log_metrics","mysql_probe","custom_scripts"];
          (agent.plugins||[]).forEach(function(p){
            if (manageable.indexOf(p)<0) return;
            var el=document.getElementById("pa-"+p);
            if (el) el.innerHTML='<span class="badge b-o">Agent 已停止</span>';
          });
          return;
        }
        fetch(API+"/agents/"+agentId+"/plugin-status").then(function(r){return r.json()}).then(function(d) {
          var plugins = d.plugins||{};
          // 如果 API 返回 error（agent 停了），全部显示启动按钮
          if (d.error || Object.keys(plugins).length === 0) {
            var manageable = ["log_metrics","mysql_probe","custom_scripts"];
            (agent.plugins||[]).forEach(function(p){
              if (manageable.indexOf(p)<0) return;
              var el=document.getElementById("pa-"+p);
              if (el) el.innerHTML='<button class="btn btn-s btn-sm" onclick="pluginOp(\''+agentId+'\',\''+p+'\',\'start\')">▶ 启动</button>';
            });
            return;
          }
          Object.keys(plugins).forEach(function(p) {
            var el = document.getElementById("pa-"+p);
            if (!el) return;
            var running = plugins[p]==="running";
            if (running) {
              el.innerHTML = '<button class="btn btn-o btn-sm" onclick="pluginOp(\''+agentId+'\',\''+p+'\',\'reload\')">🔄 热加载</button> <button class="btn btn-d btn-sm" onclick="pluginOp(\''+agentId+'\',\''+p+'\',\'stop\')">⏹ 停止</button>';
            } else {
              el.innerHTML = '<button class="btn btn-s btn-sm" onclick="pluginOp(\''+agentId+'\',\''+p+'\',\'start\')">▶ 启动</button>';
            }
          });
        }).catch(function(){});
      }
      function pluginOp(agentId, plugin, action) {
        fetch(API+"/plugin-action", {method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({agent_id:agentId,plugin:plugin,action:action})}).then(function(r){return r.json()}).then(function(d) {
          toast((d.success?"✅ ":"❌ ")+action+" "+plugin+": "+(d.output||d.error||""), d.success?"ok":"err");
          setTimeout(function(){ loadPluginStatus(agentId); }, 2000);
        });
      }
      function showCurrentAgents(list, title) {
        var html = '<div class="card"><div class="card-hd">'+title+' ('+list.length+')</div><div class="card-bd" style="padding:0"><table><thead><tr><th>ID</th><th>类型</th><th>状态</th><th>IDC</th><th>版本</th></tr></thead><tbody>';
        list.forEach(function(a){html += '<tr style="cursor:pointer" onclick="openDetailById(\''+a.id+'\')"><td style="color:var(--primary)">'+a.id+'</td><td>'+(a.type==='proxy'?'Proxy':'Edge')+'</td><td>'+a.status+'</td><td>'+((a.labels||{}).idc||'-')+'</td><td>'+a.version+'</td></tr>'});
        html += '</tbody></table></div>';
        document.getElementById("main-content").innerHTML = html;
      }
      function filterGroup(key, value) {
        var filtered = agents.filter(function(a) {
          if (key==='type') return a.type===value;
          return (a.labels||{})[key]===value;
        });
        showCurrentAgents(filtered, key+'='+value);
      }
      function closeBatchModal() { document.getElementById("batch-modal").classList.remove("open"); }
      function confirmBatch() {
        document.getElementById("batch-modal").classList.remove("open");
        batchOp();
      }
      function batchOp() {
        var scopeType = document.getElementById("batch-scope-type").value;
        var scopeVal = document.getElementById("batch-scope-val").value;
        var action = document.getElementById("batch-action").value;
        var target = agents.filter(function(a) {
          if (scopeType==='type') return a.type===scopeVal;
          return (a.labels||{})[scopeType]===scopeVal;
        });
        if (target.length===0) { toast("没有匹配的 Agent","err"); return; }
        showLog("批量"+action+": "+target.length+" Agent", "ok");
        var done=0, ok=0;
        target.forEach(function(a) {
          fetch(API+"/action",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({agent_id:a.id,action:action})}).then(function(r){return r.json()}).then(function(d){
            done++; if(d.success) ok++;
            if(done===target.length){ toast("完成: "+ok+"/"+target.length+" 成功", ok===target.length?"ok":"err"); refresh(); if (typeof window.resRefreshAfterBatch==='function') window.resRefreshAfterBatch(); }
          });
        });
      }
      function batchOpSingle(id, action) {
        fetch(API+"/action",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({agent_id:id,action:action})}).then(function(r){return r.json()}).then(function(d) {
          toast((d.success?"✅ ":"❌ ")+action+" "+id, d.success?"ok":"err");
          setTimeout(refresh, 3000);
        });
      }

      // ===================================================================
      //  PAGE: Auth
      // ===================================================================
      function renderAuth() {
        var html = '<div class="card"><div class="card-hd">🔑 认证与权限</div><div class="card-bd">';
        html += '<div style="font-size:13px;margin-bottom:16px">RBAC 角色管理 | API Key 管理</div>';
        html += '<table><thead><tr><th>角色</th><th>权限</th><th>成员数</th><th></th></tr></thead><tbody>';
        html += '<tr><td>管理员</td><td>全部权限</td><td>2</td><td><button class="btn btn-o btn-sm">编辑</button></td></tr>';
        html += '<tr><td>运维</td><td>查看 + 操作 Agent</td><td>5</td><td><button class="btn btn-o btn-sm">编辑</button></td></tr>';
        html += '<tr><td>只读</td><td>仅查看</td><td>10</td><td><button class="btn btn-o btn-sm">编辑</button></td></tr>';
        html += '</tbody></table></div>';
        document.getElementById("main-content").innerHTML = html;
      }
      function openAddPluginModal() {
        renderOverlay('➕ 新增采集插件', function() {
          var h = '<div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;font-size:13px">';
          h += '<div style="grid-column:1/-1"><b>插件名称</b><br><input id="np-name" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: nginx_exporter"></div>';
          h += '<div style="grid-column:1/-1"><b>显示名</b><br><input id="np-label" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: Nginx Exporter"></div>';
          h += '<div><b>分类</b><br><select id="np-cat" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"><option>数据库</option><option>中间件</option><option>系统</option><option>自定义脚本</option><option>云服务</option></select></div>';
          h += '<div><b>类型</b><br><select id="np-type" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px"><option>exporter</option><option>script</option><option>builtin</option></select></div>';
          h += '<div style="grid-column:1/-1"><b>说明</b><br><textarea id="np-desc" rows="2" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="插件功能描述"></textarea></div>';
          h += '<div style="grid-column:1/-1"><b>指标前缀（用于匹配指标目录）</b><br><input id="np-key" style="width:100%;padding:6px 8px;border:1px solid var(--border);border-radius:4px" placeholder="例: nginx_"></div>';
          h += '</div><div style="margin-top:16px;display:flex;gap:8px"><button class="btn btn-p" onclick="saveNewPlugin()">💾 创建</button><button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';
          return h;
        });
      }
      function saveNewPlugin() {
        var name = document.getElementById('np-name').value;
        if (!name) { alert('请输入插件标识'); return; }
        var p = {
          id: name, icon: '🧩', name: document.getElementById('np-label').value || name,
          cat: document.getElementById('np-cat').value, metricKey: name,
          type: document.getElementById('np-type').value, enabled: true,
          desc: document.getElementById('np-desc').value || '用户自定义插件',
          params: [], modes: [{name:'默认', config: name+':\n  enabled: true', note:'默认配置'}],
          links: {}, iconFile: '', scriptFile: ''
        };
        // Upload icon and script
        var uploads = [];
        var iconFile = document.getElementById('np-icon').files[0];
        if (iconFile) { uploads.push(uploadPluginFile(name, iconFile, 'icon')); }
        var scriptFile = document.getElementById('np-script').files[0];
        if (scriptFile) { uploads.push(uploadPluginFile(name, scriptFile, 'script')); p.scriptFile = scriptFile.name; }
        Promise.all(uploads).then(function(results){
          results.forEach(function(r){ if (r && r.filename && uploads.indexOf(r)===0) p.iconFile = r.filename; });
          // Save plugin definition
          var list = (window._PD||[]).concat([p]);
          return fetch(API+'/plugins', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(list)});
        }).then(function(){
          window._PD = (window._PD||[]).concat([p]);
          closeOverlay();
          renderPluginsMart();
        }).catch(function(e){ if (e) alert('保存失败: '+(e.message||e)); else { window._PD = (window._PD||[]).concat([p]); closeOverlay(); renderPluginsMart(); } });
      }
      function uploadPluginFile(pluginId, file, fileType) {
        var formData = new FormData();
        formData.append('plugin_id', pluginId);
        formData.append('type', fileType);
        formData.append('file', file);
        return fetch(API+'/plugin/upload', {method:'POST', body:formData}).then(function(r){return r.json()});
      }
      function renderTenants() {
        var html = '<div class="card"><div class="card-hd">🏢 租户管理</div><div class="card-bd"><p style="color:var(--muted);font-size:13px">多租户功能开发中...<br><br>支持按租户划分 Agent、配置告警隔离、指标权限控制。</p></div></div>';
        document.getElementById("main-content").innerHTML = html;
      }

      // ===================================================================
      //  PAGE: 系统设置（平台参数 / 租户与权限）
      //  原「租户管理」菜单收进本页，避免占位菜单独立成项
      // ===================================================================
      function renderSettings() {
        siteCfg(function(cfg) {
          var html = '<div class="card"><div class="card-hd">⚙️ 平台参数</div><div class="card-bd">';
          html += '<table><thead><tr><th style="width:220px">参数</th><th>值</th><th style="width:280px">来源</th></tr></thead><tbody>';
          html += '<tr><td>指标查询地址（浏览器可达）</td><td><code>' + (cfg.vm_url || '-') + '</code></td><td>环境变量 <code>VM_PUBLIC_URL</code>，未配置时按访问地址推导</td></tr>';
          html += '<tr><td>Grafana 地址</td><td><code>' + (cfg.grafana_url || '-') + '</code></td><td>环境变量 <code>GRAFANA_PUBLIC_URL</code></td></tr>';
          html += '</tbody></table>';
          html += '<p style="color:var(--muted);font-size:12px;margin-top:10px">服务端监听地址、Agent 容器前缀、CORS 白名单等由容器环境变量注入（见 <code>deploy/docker/.env.example</code>），不在界面暴露以免误改生产配置。</p>';
          html += '</div></div>';
          html += '<div class="card" style="margin-top:14px"><div class="card-hd">🏢 租户与权限 <button class="btn btn-o btn-sm" style="float:right" onclick="tenantNewPrompt()">＋ 新建租户</button></div><div class="card-bd"><div id="tenant-root" style="min-height:20px;color:var(--muted)">加载租户…</div></div></div>';
          html += '</div></div>';
          // 平台自监控（架构文档 3.3 · 前端效果设计第 6 章）：消费 /api/selfmon，展示平台自身运行态，纯只读
          html += '<div class="card" style="margin-top:14px"><div class="card-hd">🩺 平台自监控 <button class="btn btn-o btn-sm" style="float:right" onclick="selfMonRefresh()">↻ 刷新</button></div><div class="card-bd"><div id="selfmon-root" style="min-height:40px;color:var(--muted)">加载运行态…</div></div></div>';
          // 告警中心（架构 D6/G5）：主动告警，默认纯内部；外发 webhook 按 env 启用
          html += '<div class="card" style="margin-top:14px"><div class="card-hd">🚨 告警中心 <button class="btn btn-o btn-sm" style="float:right" onclick="alertCenterRefresh()">↻ 刷新</button></div><div class="card-bd"><div id="alert-root" style="min-height:30px;color:var(--muted)">加载告警…</div></div></div>';
          document.getElementById("main-content").innerHTML = html;
          if (typeof selfMonRender === 'function') selfMonRender();
          if (typeof alertCenterRender === 'function') alertCenterRender();
          if (typeof tenantRender === 'function') tenantRender();
        });
      }

      // 平台自监控渲染：消费 /api/selfmon，展示 L0 自身运行态（只读，数据缺失降级提示，不造假）
      function selfMonFetch() {
        if (window.selfMonXhr) window.selfMonXhr.abort();
        var xhr = window.selfMonXhr = new XMLHttpRequest();
        xhr.open("GET", "/api/selfmon", true);
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          var root = document.getElementById("selfmon-root");
          if (!root) return;
          if (xhr.status !== 200) { root.innerHTML = '<span style="color:var(--error)">自监控接口不可用（HTTP ' + xhr.status + '）</span>'; return; }
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { root.innerHTML = '<span style="color:var(--error)">自监控数据解析失败</span>'; return; }
          root.innerHTML = selfMonHTML(d);
        };
        xhr.send();
      }
      function selfMonRefresh() { selfMonFetch(); }
      function selfMonRender() { selfMonFetch(); }
      function selfMonHTML(d) {
        var up = d.uptime_sec || 0;
        var upStr = up < 60 ? up + 's' : (up < 3600 ? Math.floor(up/60) + 'm' + (up%60) + 's' : Math.floor(up/3600) + 'h' + Math.floor((up%3600)/60) + 'm');
        var a = d.agents || {};
        var cat = d.catalog || {};
        var cfg = d.config || {};
        function chip(label, val, ok) {
          var c = ok === false ? 'var(--error)' : (ok === true ? 'var(--success)' : 'var(--text)');
          return '<div style="flex:1;min-width:140px;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:8px"><div style="font-size:11px;color:var(--muted)">' + label + '</div><div style="font-size:15px;font-weight:600;color:' + c + '">' + val + '</div></div>';
        }
        var h = '<div style="font-size:12px;color:var(--muted);margin-bottom:8px">平台自启动于 ' + (d.started_at ? d.started_at.replace('T',' ').replace('Z','') : '-') + ' · ' + (d.go_version || '-') + '</div>';
        h += '<div style="display:flex;flex-wrap:wrap;gap:8px;margin-bottom:12px">'
          + chip('运行时长', upStr, d.goroutines > 0)
          + chip('Agent 注册', (a.total||0) + ' · 在线 ' + (a.online||0), true)
          + chip('edge/proxy', (a.edge||0) + '/' + (a.proxy||0), null)
          + chip('goroutine', d.goroutines||0, null)
          + chip('catalog 库', (cat.reachable ? '可达' : '不可达'), cat.reachable)
          + chip('插件/指标', (cat.plugins||0) + '/' + (cat.metrics||0), cat.reachable)
          + chip('租户数', (cat.tenants||1), cat.reachable)
          + '</div>';
        h += '<table><thead><tr><th style="width:200px">运行配置</th><th>值</th></tr></thead><tbody>';
        var rows = [['监听地址', cfg.listen_addr], ['Agent HTTP 端口', cfg.agent_http_port], ['Agent 容器前缀', cfg.agent_container_pre], ['SAgent 版本', cfg.sagent_version], ['VM 基地址', cfg.vm_url], ['接入停滞阈值', cfg.onboard_stall_sec + 's'], ['隧道远端端口', cfg.tunnel_remote_port], ['演示种子', cfg.seed_demo_agents === 'true' ? '开启' : '关闭']];
        rows.forEach(function(rd){ h += '<tr><td>' + rd[0] + '</td><td><code>' + (rd[1]||'-') + '</code></td></tr>'; });
        h += '</tbody></table>';
        return h;
      }

      // ===== 告警中心（架构 D6/G5）：消费 /api/alerts，主动告警 + 默认内部、webhook 按 env =====
      function alertCenterFetch() {
        var root = document.getElementById("alert-root");
        if (!root) return;
        var xhr = new XMLHttpRequest();
        xhr.open("GET", "/api/alerts", true);
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          if (xhr.status !== 200) { root.innerHTML = '<span style="color:var(--error)">告警接口不可用（HTTP ' + xhr.status + '）</span>'; return; }
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { root.innerHTML = '<span style="color:var(--error)">告警数据解析失败</span>'; return; }
          root.innerHTML = alertCenterHTML(d);
        };
        xhr.send();
      }
      function alertCenterRefresh() { alertCenterFetch(); }
      function alertCenterRender() { alertCenterFetch(); }
      function alertCenterHTML(d) {
        var list = (d && d.alerts) || [];
        var firing = d.firing || 0;
        var badgeColor = firing > 0 ? 'var(--error)' : 'var(--success)';
        var h = '<div style="font-size:12px;color:var(--muted);margin-bottom:8px">当前告警 ' + firing + ' 条'
          + (d.webhook_gated ? ' · 外发未启用（ALERT_WEBHOOK_URL 为空，纯内部）' : ' · 外发 webhook 已启用')
          + '</div>';
        if (!list.length) { h += '<span style="color:var(--muted)">暂无告警事件</span>'; return h; }
        h += '<table><thead><tr><th style="width:120px">时间</th><th style="width:200px">规则</th><th style="width:70px">级别</th><th style="width:80px">状态</th><th>标题</th></tr></thead><tbody>';
        list.forEach(function(a) {
          var sev = a.severity === 'critical' ? '<span style="color:var(--error);font-weight:600">CRIT</span>'
            : '<span style="color:var(--warning,#f59e0b);font-weight:600">WARN</span>';
          var st = a.state === 'firing' ? '<span style="color:var(--error)">● firing</span>' : '<span style="color:var(--success)">○ resolved</span>';
          h += '<tr><td style="white-space:nowrap">' + a.time + '</td><td><code>' + a.source + '</code></td><td>' + sev + '</td><td>' + st + '</td>'
            + '<td><div>' + a.title + '</div><div style="font-size:12px;color:var(--muted)">' + (a.detail||'') + '</div></td></tr>';
        });
        h += '</tbody></table>';
        return h;
      }

      // ===== 多租户（架构 D3 骨架）：租户列表渲染 + 新建 =====
      function tenantRender() {
        var root = document.getElementById("tenant-root");
        if (!root) return;
        root.innerHTML = '<span style="color:var(--muted)">加载租户…</span>';
        var xhr = new XMLHttpRequest();
        xhr.open("GET", "/api/tenants", true);
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          if (xhr.status !== 200) { root.innerHTML = '<span style="color:var(--error)">租户接口不可用</span>'; return; }
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { root.innerHTML = '<span style="color:var(--error)">租户数据解析失败</span>'; return; }
          var list = (d && d.tenants) || [];
          if (!list.length) { root.innerHTML = '<span style="color:var(--muted)">暂无租户</span>'; return; }
          var h = '<table><thead><tr><th style="width:60px">#</th><th style="width:180px">标识</th><th>名称</th><th>说明</th><th style="width:230px">API 凭证（D3）</th></tr></thead><tbody>';
          list.forEach(function(t) {
            h += '<tr><td>' + t.id + '</td><td><code>' + t.code + '</code></td><td>' + t.name + '</td>'
              + '<td style="color:var(--muted)">' + (t.note||'') + '</td>'
              + '<td style="white-space:nowrap"><span id="ttok-' + t.code + '" style="font-size:12px;color:var(--muted)">查状态…</span> '
              + '<button class="btn btn-o btn-sm" onclick="tenantTokenIssue(\'' + t.code + '\')">签发/轮换</button> '
              + '<button class="btn btn-o btn-sm" onclick="tenantTokenRevoke(\'' + t.code + '\')">吊销</button></td></tr>';
          });
          h += '</tbody></table>';
          root.innerHTML = h;
          // 逐租户查凭证状态，异步就地回填
          list.forEach(function(t) { tenantTokenStatus(t.code); });
        };
        xhr.send();
      }
      function tenantNewPrompt() {
        var code = prompt("租户标识（唯一，小写字母/数字/-）：", "");
        if (!code) return;
        var name = prompt("租户名称：", "");
        if (!name) return;
        var note = prompt("说明（可选）：", "");
        var xhr = new XMLHttpRequest();
        xhr.open("POST", "/api/tenants", true);
        xhr.setRequestHeader("Content-Type", "application/json");
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          try {
            var d = JSON.parse(xhr.responseText);
            if (xhr.status === 200 && d.ok) { toast('已新建租户 ' + code, 'ok'); } else { toast((d && d.error) || '新建失败', 'err'); }
          } catch (e) { toast('新建失败', 'err'); }
          if (typeof tenantRender === 'function') tenantRender();
        };
        xhr.send(JSON.stringify({ code: code, name: name, note: note || '' }));
      }

      // ===== 租户 API Token（架构 D3 凭证鉴权）：状态 / 签发 / 吊销 =====
      function tenantTokenStatus(code) {
        var span = document.getElementById('ttok-' + code);
        if (!span) return;
        var xhr = new XMLHttpRequest();
        xhr.open("GET", "/api/tenants/" + encodeURIComponent(code) + "/token", true);
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          if (!span) return;
          if (xhr.status !== 200) { span.textContent = '状态未知(' + xhr.status + ')'; span.style.color='var(--error)'; return; }
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { span.textContent = '状态未知'; return; }
          if (d.has_token) { span.textContent = '已签发'; span.style.color='var(--success)'; }
          else { span.textContent = '未签发'; span.style.color='var(--muted)'; }
        };
        xhr.send();
      }
      function tenantTokenIssue(code) {
        if (!window.confirm('为租户【' + code + '】签发/轮换 API Token？\n\n新 Token 明文仅本次展示一次，替换旧 Token 后旧凭证立即失效。')) return;
        var xhr = new XMLHttpRequest();
        xhr.open("POST", "/api/tenants/" + encodeURIComponent(code) + "/token", true);
        xhr.setRequestHeader("Content-Type", "application/json");
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { toast('签发失败', 'err'); return; }
          if (xhr.status === 200 && d.ok) {
            toast('Token 已签发（明文仅本次可见）', 'ok');
            var msg = '请立即保存以下 API Token（仅本次返回，库内只存哈希）：\n\n' + d.api_token + '\n\n调用示例：\ncurl -H "X-Tenant-Token: ' + d.api_token + '" /api/agents';
            window.alert(msg);
          } else { toast((d && d.error) || '签发失败', 'err'); }
          tenantTokenStatus(code);
        };
        xhr.send('{}');
      }
      function tenantTokenRevoke(code) {
        if (!window.confirm('吊销租户【' + code + '】的 API Token？\n吊销后该凭证立即失效，无法恢复。')) return;
        var xhr = new XMLHttpRequest();
        xhr.open("DELETE", "/api/tenants/" + encodeURIComponent(code) + "/token", true);
        xhr.onreadystatechange = function() {
          if (xhr.readyState !== 4) return;
          var d;
          try { d = JSON.parse(xhr.responseText); } catch (e) { toast('吊销失败', 'err'); return; }
          if (xhr.status === 200 && d.ok) { toast('已吊销 ' + code + ' 的 Token', 'ok'); }
          else { toast((d && d.error) || '吊销失败', 'err'); }
          tenantTokenStatus(code);
        };
        xhr.send('{}');
      }
