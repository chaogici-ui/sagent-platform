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
      // 导航琥珀徽标（IA 设计稿 2.1 ① + 样式要点 2）：实时反映"该动手的事"。
      // 规则 4：待办徽章数一处计算两处消费——工作台待办卡与「采集健康」badge 共用 computeTodos，
      // 口径只有这一处实现（同一份 recon + flows 进出，结果确定），禁止两边各算一遍。
      // 旧实现的 "onboard-center" 键在导航里没有对应 data-page（该页只是深链兜底），徽标永远
      // 渲染不出来——已按设计稿收敛为「采集健康」一个 badge，不另开第二个入口。
      // 渲染策略：先移除旧徽标再按需重建——每次反映最新计数且不会叠加；失败静默保留上次值，
      // 不阻断页面（徽标是辅助信息，不该让整页挂掉）。
      // 待办三类（设计稿 ① 工作台 界面效果）：未复核草案 / 待批准变更 / 受阻任务。
      function computeTodos(recon, flows) {
        var sum = (recon && recon.summary) || {};
        // 未复核草案：草案由断采目标模板逐条生成，一条断采 = 一份待复核草案
        var drafts = (recon && recon.stale && recon.stale.length) ? recon.stale.length : (sum.stale || 0);
        var list = (flows && flows.flows) ? flows.flows : (flows || []);
        var approvals = 0, blocked = 0;
        list.forEach(function (f) {
          if (!f) return;
          if (f.wait_kind === 'human') approvals++;  // 停等人工决策 = 待批准
          else if (f.status === 'stalled' || f.status === 'blocked' || f.status === 'failed') blocked++;
        });
        return { drafts: drafts, approvals: approvals, blocked: blocked,
                 total: drafts + approvals + blocked };
      }
      window.computeTodos = computeTodos;

      async function refreshNavBadges() {
        try {
          var recon = await (await fetch(API + "/recon")).json();
          var flows = await (await fetch(API + "/onboard/flows")).json();
          var map = { slo: computeTodos(recon, flows).total };
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
      // ==== 四态骨架（IA 设计稿 P3 通用条款）====
      // 全站 loading / error / readonly / empty 的唯一实现。四条纪律：
      //   ① 0/0 一律判「不适用」，禁止显示 100%（分母为 0 时百分比没有意义）；
      //   ② 空态必须带引导动作，或至少说清"下一步去哪"，不许只丢一句"暂无数据"；
      //   ③ 只读页必须显式声明只读，并指明写入口在哪个页面（规则 3）；
      //   ④ 取数失败必须给错误行 + 重试，绝不静默留白。
      // 各页只准调用这四个函数产出状态，禁止再各写一套内联灰字（历史 30+ 处各写各的，
      // 同一句"暂无数据"在四个页面是四种灰，也没有一处给了下一步）。
      function stSk(rows) {                     // loading：骨架屏，不白屏
        var n = rows || 2, h = '<div class="st st-sk" data-st="loading">';
        for (var i = 0; i < n; i++) h += '<i></i>';
        return h + '</div>';
      }
      function stEmpty(o) {                     // empty：说清"没有"并给下一步
        o = o || {};
        var h = '<div class="st st-empty" data-st="empty">';
        if (o.icon) h += '<span class="st-ic">' + o.icon + '</span>';
        h += '<div class="st-t">' + (o.title || '暂无数据') + '</div>';
        if (o.hint) h += '<div>' + o.hint + '</div>';
        if (o.action && o.action.label) {
          h += '<div class="st-actions"><button class="btn btn-p btn-sm" onclick="' + o.action.onclick + '">'
            + o.action.label + '</button></div>';
        }
        return h + '</div>';
      }
      function stErr(o) {                       // error：错误行 + 重试，不静默留白
        o = o || {};
        var h = '<div class="st st-err" data-st="error"><div class="st-t">⚠ 加载失败</div>'
          + '<div>' + (o.msg || '取数失败') + '</div>';
        if (o.retry) h += '<div class="st-actions"><button class="btn btn-o btn-sm" onclick="' + o.retry + '">重试</button></div>';
        return h + '</div>';
      }
      function stRo(text) {                     // readonly：只读声明条
        return '<span class="st-ro" title="本页不产生变更单，写操作在别处">🔒 ' + (text || '只读') + '</span>';
      }
      window.stSk = stSk; window.stEmpty = stEmpty; window.stErr = stErr; window.stRo = stRo;

      // ==== Page routing ====
      // P3 四态兜底挂在路由层：goPage 是"打开任意一页"的唯一入口，一处改动覆盖全部 12 页，
      // 不用每页各写一遍。loading = 先落骨架屏（不白屏）；error = 渲染器同步抛错、或 12s 后
      // 骨架还在（取数失败被 catch 静默吞掉的典型症状）→ 兜成"错误行 + 重试"。
      // readonly / empty 由各页渲染器调 stRo / stEmpty 产出。
      var _pageSeq = 0, _pageWatch = null;
      // 只开叠加层、不写主内容区的深链页不套骨架与看门狗，否则会误报"加载失败"
      var _noPageGuard = { add: 1 };
      function goPage(page) {
        currentPage = page;
        window._prevView = null; // 正常导航时清空 overlay 返回记忆，防粘性回错页面
        document.querySelectorAll("nav a").forEach(function(a){a.classList.toggle("active",a.dataset.page===page)});
        var _seq = ++_pageSeq, _guard = !_noPageGuard[page];
        clearTimeout(_pageWatch);
        if (_guard) {
          var mc0 = document.getElementById("main-content");
          if (mc0) mc0.innerHTML = stSk(3);
        }
        try {
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
          case "data-gov": renderDataGov(); break;          // 数据治理（platform.js，野指标治理切片）
          case "platform-health": renderPlatformHealthPage(); break; // 平台健康（platform.js，/api/health 组件化自检）
          case "slo": renderSLO(); break;
          case "audit": renderAudit(); break;
          case "tasks": renderTasks(); break;      // 读落库审计（/api/tasks → ListAudit），重启不丢
          case "settings": renderSettings(); break;
          case "versions": renderVersions(); break; // 版本与兼容（version.js）
          case "tenants": renderTenants(); break;
          default: renderHealthOverview();
          }
        } catch (e) {
          // 渲染器同步抛错：给错误行 + 重试，别让人对着白屏猜
          var mce = document.getElementById("main-content");
          if (mce) mce.innerHTML = stErr({ msg: (e && e.message) || String(e), retry: "goPage('" + page + "')" });
        }
        renderStats(); // 统计条随页面显隐（见 statsRelevantPage）
        // 看门狗：12s 后主内容区还是骨架 → 该页渲染器一个字节都没写出来，兜成错误态。
        // _seq 比对防止"先开的慢页"误伤"后开的页"。
        if (_guard) {
          _pageWatch = setTimeout(function () {
            if (_seq !== _pageSeq) return;
            var mc = document.getElementById("main-content");
            if (mc && mc.querySelector('.st-sk')) {
              mc.innerHTML = stErr({ msg: '超时：' + page + ' 的数据没能取回来（后端未响应或接口报错）', retry: "goPage('" + page + "')" });
            }
          }, 12000);
        }
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
          // 四值语义（P1）：stopped=明确维护态，offline/created=状态未知（未证实失败）——
          // 二者都不是"异常"，合并标红会把"可能只是网络断了"当故障报
          '<div class="sc"><div class="n" style="color:var(--warning,#b45309)">' +
          (stopped + offline) +
          '</div><div class="l">停用/未知</div></div>';
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
        // 三态提示（IA 设计稿 3.2 unknown≠failed）：ok / err / warn。
        // "不适用""待确认"这类既非成功也非失败的结果走 warn 琥珀色——
        // 拿红色当"未证实"就是在报假故障。
        t.className = "toast toast-" + (type === "err" ? "err" : type === "warn" ? "warn" : "ok") + " show";
        setTimeout(function () {
          t.classList.remove("show");
        }, 2500);
      }

      function showLog(msg, type) {
        var cls = type === "err" ? "log-err" : type === "ok" ? "log-ok" : "";
        var el = document.getElementById("log-text");
        if (el) el.innerHTML = '<span class="log-msg '+cls+'">['+new Date().toLocaleTimeString()+'] '+msg+'</span>';
      }
