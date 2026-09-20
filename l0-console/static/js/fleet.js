function renderOverview() {
        var html = '';

        // --- 1. IDC 分布（横向条） ---
        var idcMap = {};
        agents.forEach(function(a) {
          var idc = (a.labels||{}).idc || a.idc || 'unknown';
          if (!idcMap[idc]) idcMap[idc] = {total:0, healthy:0, stopped:0, offline:0};
          idcMap[idc].total++;
          if (a.status==='healthy') idcMap[idc].healthy++;
          else if (a.status==='stopped') idcMap[idc].stopped++;
          else idcMap[idc].offline++;
        });
        var maxTotal = Math.max.apply(null, Object.values(idcMap).map(function(x){return x.total}));
        html += '<div class="card"><div class="card-hd">🏢 按 IDC 分布</div><div class="card-bd">';
        Object.keys(idcMap).sort().forEach(function(idc) {
          var m = idcMap[idc];
          var pct = maxTotal > 0 ? Math.round(m.total/maxTotal*100) : 0;
          var hasIssue = m.stopped + m.offline > 0;
          html += '<div class="idc-bar" onclick="filterGroup(\'idc\',\''+idc+'\')" style="cursor:pointer">'+
            '<div class="idc-bar-label">'+idc+'</div>'+
            '<div class="idc-bar-track"><div class="idc-bar-fill'+(hasIssue?' idc-bar-warn':'')+'" style="width:'+pct+'%"></div></div>'+
            '<div class="idc-bar-nums"><b>'+m.total.toLocaleString()+'</b> '+
            '<span style="color:var(--success)">'+m.healthy+' 健康</span>'+
            (hasIssue ? ' <span style="color:var(--error)">'+(m.stopped+m.offline)+' 异常</span>' : '')+
            '</div></div>';
        });
        html += '</div></div>';

        // --- 2. 类型汇总（紧凑两格） ---
        var typeMap = {edge:{total:0,healthy:0},proxy:{total:0,healthy:0}};
        agents.forEach(function(a) {
          var t = a.type||'edge';
          if (typeMap[t]) { typeMap[t].total++; if (a.status==='healthy') typeMap[t].healthy++; }
        });
        html += '<div class="card"><div class="card-hd">📊 类型汇总</div><div class="card-bd" style="display:flex;gap:16px">';
        [{key:'edge',icon:'📡',name:'边缘采集'},{key:'proxy',icon:'🔗',name:'Collector Proxy'}].forEach(function(t){
          var m = typeMap[t.key];
          html += '<div style="flex:1;padding:12px 16px;background:var(--bg);border-radius:8px;cursor:pointer" onclick="filterGroup(\'type\',\''+t.key+'\')">'+
            '<div style="font-size:13px;color:var(--muted)">'+t.icon+' '+t.name+'</div>'+
            '<div style="font-size:28px;font-weight:700">'+m.total.toLocaleString()+'</div>'+
            '<div style="font-size:11px;color:var(--muted)">'+m.healthy+' 健康 ('+Math.round(m.healthy/Math.max(m.total,1)*100)+'%)</div></div>';
        });
        html += '</div></div>';

        // --- 3. 异常 Agent 列表 ---
        var anomaly = agents.filter(function(a){return a.status!=='healthy'&&a.status!=='running'});
        html += '<div class="card"><div class="card-hd">⚠️ 异常 Agent（'+anomaly.length+'）</div>';
        if (anomaly.length === 0) {
          html += '<div class="card-bd" style="text-align:center;color:var(--muted);padding:30px">✅ 所有 Agent 运行正常</div>';
        } else {
          html += '<div class="card-bd" style="padding:0"><table><thead><tr><th>Agent ID</th><th>类型</th><th>状态</th><th>IDC</th><th>版本</th><th>操作</th></tr></thead><tbody>';
          anomaly.forEach(function(a){
            var s = a.status==='stopped'?'b-s':'b-o';
            var sn = a.status==='stopped'?'已停止':'离线';
            html += '<tr><td><a href="javascript:void(0)" onclick="openDetailById(\''+a.id+'\')" style="color:var(--primary)">'+a.id+'</a></td>'+
              '<td>'+(a.type==='proxy'?'Proxy':'Edge')+'</td>'+
              '<td><span class="badge '+s+'">'+sn+'</span></td>'+
              '<td>'+((a.labels||{}).idc||'-')+'</td><td>'+a.version+'</td>'+
              '<td><button class="btn btn-s btn-sm" onclick="quickStart(\''+a.id+'\')">启动</button></td></tr>';
          });
          html += '</tbody></table></div>';
        }
        html += '</div>';

        // --- 4. 批量操作 ---
        html += '<div class="batch-bar"><strong style="font-size:13px">批量操作</strong>'+
          '<select id="batch-scope-type"><option value="idc">IDC</option><option value="type">类型</option></select>'+
          '<select id="batch-scope-val"></select>'+
          '<select id="batch-action"><option value="restart">重启 Agent</option><option value="stop">停止 Agent</option><option value="start">启动 Agent</option></select>'+
          '<button class="btn btn-p" onclick="batchOp()">执行</button></div>';

        document.getElementById("main-content").innerHTML = html;
        updateBatchScope();
        document.getElementById("batch-scope-type").onchange = updateBatchScope;
      }