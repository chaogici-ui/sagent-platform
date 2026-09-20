const API = "/api";
var agents = [];
var currentPage = "slo";
var currentDetailAgent = null;
var currentDetailTab = "plugins";
var expandedGroups = {};

refresh();
setInterval(refresh, 15000);
goPage("slo");