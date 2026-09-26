const API = "/api";
var agents = [];
var currentPage = "overview";
var currentDetailAgent = null;
var currentDetailTab = "plugins";
var expandedGroups = {};

refresh();
setInterval(refresh, 15000);
window.loadTenantOptions(); // 多租户(D3)：顶部租户下拉填充（直接可用，failed 静默降级）
goPage("overview");

// 导航版本号与平台真实运行版本对齐（架构 3.3 可观测：界面不显示编造/缓存号）。
// 从 /api/selfmon 读取 sagent_version，失败时保留 index.html 里的静态降级值，不阻断页面。
(function syncNavVersion() {
  var xhr = new XMLHttpRequest();
  xhr.open("GET", "/api/selfmon", true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) return;
    if (xhr.status !== 200) return;
    try {
      var el = document.getElementById("nav-ver");
      if (!el) return;
      var d = JSON.parse(xhr.responseText);
      var v = d && d.config && (d.config.sagent_version || "");
      if (v) el.textContent = v;
    } catch (e) { /* 解析失败保留静态值 */ }
  };
  xhr.send();
})();
refreshNavBadges(); // 导航计数徽标立即刷新（随后由 refresh() 每 15s 承接）
