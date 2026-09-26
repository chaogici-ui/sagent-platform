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
        // 总览是"现在怎么样"的仪表盘，跟着心跳一起刷；资源页不自动重绘（会打断筛选/抽屉）。
        // 叠加视图（流水线详情/Agent 详情等 renderOverlay 打开的）优先：刷新把详情页
        // 顶掉，等于每隔 15 秒把人踢出正在看的页面（2026-09-22 实测踩到）
        var overlayOpen = typeof window._prevView === "function";
        if (currentPage === "overview" && !overlayOpen) renderHealthOverview();
        refreshNavBadges();
      }
      // 导航琥珀徽标（前端效果设计第 1 章信息架构）：实时反映"该动手的事"（断采目标/待处理流水线）。
      // 消费既有 /api/recon(断采数) 与 /api/onboard/flows(待处理数)，未避免打扰正在看的内容，
      // 仅在徽标计数变化时才重写 DOM（同值跳过）。失败静默保留上次值，不阻断页面。
      var _navBadgeCache = {};
      async function refreshNavBadges() {
        try {
          var recon = await (await fetch(API + "/recon")).json();
          var flows = await (await fetch(API + "/onboard/flows")).json();
          var stale = (recon && recon.summary && recon.summary.stale) || 0;
          var pending = (flows && flows.flows ? flows.flows : []).filter(function (f) {
            return f.status === 'running' || f.status === 'blocked' || f.status === 'stalled' || f.status === 'failed';
          }).length;
          var map = { slo: stale, "onboard-center": pending };
          Object.keys(map).forEach(function (page) {
            var n = map[page];
            var a = document.querySelector('nav a[data-page="' + page + '"]');
            if (!a) return;
            // 旧徽标移除，再按需重建（保证每次反映最新，且避免重复叠加）
            var b = a.querySelector('.nav-badge');
            if (b) b.remove();
            if (n > 0) {
              var span = document.createElement('span');
              span.className = 'nav-badge';
              span.textContent = n;
              a.appendChild(span);
            }
          });
        } catch (e) { /* 静默降级 */ }
      }
      // ==== Page routing ====
      function goPage(page) {
        currentPage = page;
        window._prevView = null; // 正常导航时清空 overlay 返回记忆，防粘性回错页面
        document.querySelectorAll("nav a").forEach(function(a){a.classList.toggle("active",a.dataset.page===page)});
        switch (page) {
          // —— 四项菜单（2026-09-22 用户评审后的信息架构）——
          case "overview": renderHealthOverview(); break;
          case "resources": renderResources(); break;      // 资源与接入（resources.js，主战场）
          case "capabilities": renderCapabilities(); break; // 能力与指标（插件 + 指标合页）
          // ---- 以下为旧实现物页面：不再占菜单位，保留深链兜底（历史链接/文档不失效）----
          case "plugins-mart": renderPluginsMart(); break;
          case "metrics-catalog": renderMetricsCatalog(); break;
          case "onboard-center": renderOnboardCenter(); break;
          case "changes": renderChanges(); break;     // 变更中心（聚合流水线+审计为时间线）
          case "add": openAddWizard(); break;
          case "agent-list": renderAgentList(); break;
          case "targets": renderTargets(); break;
          case "metrics-browse": renderMetricsBrowse(); break;
          case "slo": renderSLO(); break;
          case "audit": renderAudit(); break;
          case "tasks": renderTasks(); break;      // 读落库审计（/api/tasks → ListAudit），重启不丢
          case "settings": renderSettings(); break;
          case "versions": renderVersions(); break; // 版本与兼容（version.js）
          case "tenants": renderTenants(); break;
          default: renderHealthOverview();
        }
        renderStats(); // 统计条随页面显隐（见 statsRelevantPage）
      }

      // ==== 多租户(D3) 界面隔离：租户下拉 + 全局可见性过滤 ====
      // 当前选中租户，默认落在 default（不再提供"全部租户"视图，始终处于某一租户内）
      window._currentTenant = "default";
      // 某对象（Agent/资源等）对当前租户是否可见：后端字段统一为 tenant_id
      window.tenantVisible = function (o) {
        if (!window._currentTenant) return true;
        return (o && (o.tenant_id || "default")) === window._currentTenant;
      };
      // 切换租户：更新全局状态并按当前页就地重绘（保持用户在的页面，避免踢回工作台）
      // 内部用 code 过滤，提示文案用展示名（如 code=tenant-a 显示为「西藏」）
      window.changeTenant = function (code) {
        window._currentTenant = code || "default";
        var nm = (window._tenantCodes && window._tenantCodes[window._currentTenant]) || window._currentTenant;
        goPage(currentPage);
        toast("已切换到租户「" + nm + "」");
      };
      // 拉取租户列表填充下拉（用于站/工作台顶部租户切换，tenant_id 为空的对象视为 default）
      window.loadTenantOptions = function () {
        fetch(API + "/tenants").then(function (r) { return r.json(); }).then(function (d) {
          var sel = document.getElementById("tenant-switch");
          // 缓存租户 code→name：资源页"归属租户"下拉共用同一来源（D3 收尾：资源级赋值）
          var codes = {}, arr = [];
          (d && d.tenants || []).forEach(function (t) { if (t.code) { codes[t.code] = t.name || t.code; arr.push(t.code); } });
          if (!codes["default"]) codes["default"] = "默认租户";
          if (arr.indexOf("default") < 0) arr.unshift("default");
          window._tenantCodes = codes; window._tenantCodesArr = arr;
          // D3 资源级分配：资源页行内归属租户下拉依赖此缓存，就绪后补齐选项
          if (typeof window.resFillTenantCells === 'function') window.resFillTenantCells();
          if (!sel || !d || !d.tenants) return;
          var opts = '';
          Object.keys(codes).forEach(function (code) {
            opts += '<option value="' + code + '">' + (codes[code] || code) + '</option>';
          });
          var prev = window._currentTenant || "default";
          sel.innerHTML = opts;
          sel.value = prev;
        }).catch(function () { window._tenantCodes = window._tenantCodes || { default: "默认租户" }; });
      };

      // ==== Global search ====
      function doSearch() {
        var q = document.getElementById("search-input").value.trim();
        if (!q) return;
        var found = agents.filter(function (a) {
          return (
            a.id.indexOf(q) >= 0 ||
            a.name.indexOf(q) >= 0 ||
            (a.labels || {}).tenant === q ||
            (a.tenant_id || "default") === q ||
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
      // 顶部统计条讲的是"Agent 资源盘点"，只在没有自己计数器的清单页出现：
      // 总览有 SLO 卡、资源页有筛选计数，再挂一条全局统计就是同一件事说两遍
      // （2026-09-22 界面评审：与当前页无关/重复的统计只分散注意力）
      function statsRelevantPage(page) {
        return page === "agent-list" || page === "targets";
      }

      function renderStats() {
        var box = document.getElementById("stat-cards");
        if (!box) return;
        if (!statsRelevantPage(currentPage)) {
          box.innerHTML = "";
          return;
        }
        var scope = agents.filter(function (a) { return window.tenantVisible(a); }); // D3：顶部统计随租户隔离
        var total = scope.length;
        var healthy = scope.filter(function (a) {
          return a.status === "healthy";
        }).length;
        var running = scope.filter(function (a) {
          return a.status === "running" || a.status === "healthy";
        }).length;
        var stopped = scope.filter(function (a) {
          return a.status === "stopped";
        }).length;
        var offline = scope.filter(function (a) {
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
        // value 放原始标签值、文本才带计数——batchOp 按 value 等值匹配，带计数永远匹配不上
        sel.innerHTML = Object.keys(vals).sort().map(function(v){return '<option value="'+escHtml(v)+'">'+escHtml(v)+' ('+vals[v]+')</option>'}).join("");
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
