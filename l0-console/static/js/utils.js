      // ==== 站点外部地址（生产化红线：前端禁止写死环境地址，统一从 /api/site-config 获取） ====
      var _siteCfg = null;
      function siteCfg(cb) {
        if (_siteCfg) { cb(_siteCfg); return; }
        var fallback = function() {
          _siteCfg = {
            vm_url: location.protocol + '//' + location.hostname + ':8428',
            grafana_url: location.protocol + '//' + location.hostname + ':3000'
          };
          cb(_siteCfg);
        };
        fetch(API + '/site-config').then(function(r){ return r.json(); }).then(function(d) {
          _siteCfg = {
            vm_url: d.vm_url || location.protocol + '//' + location.hostname + ':8428',
            grafana_url: d.grafana_url || location.protocol + '//' + location.hostname + ':3000'
          };
          cb(_siteCfg);
        }).catch(fallback);
      }

      // ==== 接入配置（Agent 类型捆绑 / 接入向导插件清单，产品语义由后端下发，前端零写死） ====
      var _onboardCfg = null;
      function onboardCfg(cb) {
        if (_onboardCfg) { cb(_onboardCfg); return; }
        fetch(API + '/onboard/config').then(function(r){ return r.json(); }).then(function(d) {
          _onboardCfg = d;
          cb(_onboardCfg);
        }).catch(function(){
          _onboardCfg = { agent_types: [], manageable_plugins: [], onboard_plugins: [] };
          cb(_onboardCfg);
        });
      }

      async function refresh() {
        try {
          agents = await (await fetch(API + "/agents")).json();
        } catch (e) {
          return;
        }
        renderStats();
        if (currentPage === "overview") renderOverview();
      }
      // ==== Page routing ====
      function goPage(page) {
        currentPage = page;
        document.querySelectorAll("nav a").forEach(function(a){a.classList.toggle("active",a.dataset.page===page)});
        switch (page) {
          case "plugins-mart": renderPluginsMart(); break;
          case "metrics-catalog": renderMetricsCatalog(); break;
          case "slo": renderSLO(); break;
          case "agent-list": renderAgentList(); break;
          case "targets": renderTargets(); break;
          case "versions": renderVersions(); break;
          case "add": openAddWizard(); break;
          case "metrics-browse": renderMetricsBrowse(); break;
          case "audit": renderAudit(); break;
          case "tasks": renderTasks(); break;
          case "tenants": renderTenants(); break;
          default: renderSLO();
        }
      }

      // ==== Global search ====
      function doSearch() {
        var q = document.getElementById("search-input").value.trim();
        if (!q) return;
        var found = agents.filter(function (a) {
          return (
            a.id.indexOf(q) >= 0 ||
            a.name.indexOf(q) >= 0 ||
            (a.labels || {}).tenant === q ||
            (a.labels || {}).idc === q
          );
        });
        if (found.length === 1) {
          openDetail(found[0]);
          return;
        }
        if (found.length > 0) {
          showCurrentAgents(found, "搜索结果: " + q);
        } else {
          toast("未找到匹配的 Agent", "err");
        }
      }

      // ==== Render Stats ====
      function renderStats() {
        var total = agents.length;
        var healthy = agents.filter(function (a) {
          return a.status === "healthy";
        }).length;
        var running = agents.filter(function (a) {
          return a.status === "running" || a.status === "healthy";
        }).length;
        var stopped = agents.filter(function (a) {
          return a.status === "stopped";
        }).length;
        var offline = agents.filter(function (a) {
          return a.status === "offline" || a.status === "created";
        }).length;
        document.getElementById("stat-cards").innerHTML =
          '<div class="sc"><div class="n" style="color:var(--text)">' +
          total +
          '</div><div class="l">Agent 总数</div></div>' +
          '<div class="sc"><div class="n" style="color:var(--success)">' +
          healthy +
          '</div><div class="l">健康</div></div>' +
          '<div class="sc"><div class="n" style="color:var(--primary)">' +
          running +
          '</div><div class="l">运行中</div></div>' +
          '<div class="sc"><div class="n" style="color:var(--error)">' +
          (stopped + offline) +
          '</div><div class="l">异常/离线</div></div>';
      }

      function updateBatchScope() {
        var t = document.getElementById("batch-scope-type").value;
        var vals = {};
        agents.forEach(function(a) {
          var v = t === "type" ? a.type : ((a.labels||{})[t] || "unknown");
          vals[v] = (vals[v]||0) + 1;
        });
        var sel = document.getElementById("batch-scope-val");
        sel.innerHTML = Object.keys(vals).sort().map(function(v){return '<option>'+v+' ('+vals[v]+')</option>'}).join("");
      }

      // ===================================================================
      //  PAGE: Fleet Overview
      // ===================================================================
      function toast(msg, type) {
        var t = document.getElementById("toast");
        if (!t) return;
        t.textContent = msg;
        t.className = "toast toast-" + (type === "err" ? "err" : "ok") + " show";
        setTimeout(function () {
          t.classList.remove("show");
        }, 2500);
      }

      function showLog(msg, type) {
        var cls = type === "err" ? "log-err" : type === "ok" ? "log-ok" : "";
        var el = document.getElementById("log-text");
        if (el) el.innerHTML = '<span class="log-msg '+cls+'">['+new Date().toLocaleTimeString()+'] '+msg+'</span>';
      }
