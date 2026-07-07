/* NexusAPI 企业管理平台 · 生产前端 SPA(接 /api/v1 真后端)。
   设计沿用原型 app.css;数据全来自真实 API(非 mock)。按 /me 的角色拼装菜单与视图。 */
"use strict";
const S = { token: localStorage.getItem("nx_token") || "", me: null, role: "", view: "", org: null, orgId: 0, mvp: false, win: 168, gran: "day" };
// 品牌/支持集中常量(运营定稿前用占位;company/doc 含"待定"时前端优雅降级不露占位)。改这一处全局生效。
const BRAND = { product: "企业管理台", company: "(公司名待定)", support: "support@example.com", doc: "(文档地址待定)" };
const brandCompany = () => BRAND.company.includes("待定") ? "" : BRAND.company;
const brandDoc = () => BRAND.doc.includes("待定") ? "" : BRAND.doc;
// 改动⑦:看板时间窗(小时)。7/30/90 天 + 全部历史(24:门B 回填历史可能远早于默认窗);默认 168=近 7 天。
const WIN_ALL = 263520; // C4:=后端 maxUsageWindowHours(24*366*30≈30年),口径一致;覆盖全部历史且 since>1970
function winLabel(h) { return ({ 168: "近 7 天", 720: "近 30 天", 2160: "近 90 天", [WIN_ALL]: "全部历史" })[h] || ("近 " + Math.round(h / 24) + " 天"); }
// A5:余额接口失败显示"暂不可用"占位,不把失败当真实 $0.00(否则误导客户误判欠费/停服)。
// 取数处 catch 返回 {__err:true},渲染走此函数区分"真 0"与"取数失败"。
function balMoney(b) { return (b && b.__err) ? "暂不可用" : money((b && b.available_quota) || 0); }
function winSelect() {
  return `<select class="fsel" onchange="S.win=+this.value;renderView()">`
    + [168, 720, 2160, WIN_ALL].map(h => `<option value="${h}"${S.win === h ? " selected" : ""}>${winLabel(h)}</option>`).join("")
    + `</select>`;
}
// M2:用量趋势粒度(天/周/月,UTC+8 自然边界,后端 /usage/timeseries 同口径)。
function granLabel(g) { return ({ day: "按天", week: "按周", month: "按月" })[g] || g; }
function granSelect() {
  // C3(28):改粒度只需重拉 timeseries,局部替换趋势面板——不再 renderView() 全量重渲染(丢滚动位置/搜索框输入,整页闪)。
  return `<select class="fsel" onchange="reloadTrend(this.value)">`
    + ["day", "week", "month"].map(g => `<option value="${g}"${S.gran === g ? " selected" : ""}>${granLabel(g)}</option>`).join("")
    + `</select>`;
}
// reloadTrend 局部重绘用量趋势(C3/28):按当前视图选 timeseries 数据源,只替换 #trendBody;找不到容器回退全渲染。
async function reloadTrend(g) {
  S.gran = g;
  const box = document.getElementById("trendBody");
  if (!box) { renderView(); return; }
  const path = S.view === "myusage"
    ? "/members/" + S.me.id + "/usage/timeseries"
    : "/organizations/" + S.orgId + "/usage/timeseries";
  try {
    const ts = await api("GET", path + "?since_hours=" + S.win + "&granularity=" + S.gran, null);
    box.innerHTML = lineChart((ts && ts.series) || []);
  } catch (e) { toast(e.message); }
}
// M2-2 下钻明细:时间短格式 + 逐条调用表(读 usage_detail 的 records)。
function fmtTs(s) { try { return new Date(s).toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }); } catch (e) { return esc(s); } }
function detailTable(records) {
  records = records || [];
  if (records.length === 0) return `<div class="empty">该时段暂无逐条记录</div>`;
  const rows = records.map(r => {
    const q = Number(r.consumed_quota ?? r.quota ?? 0) || 0;
    return `<tr><td class="mini">${fmtTs(r.log_ts)}</td><td>${esc(r.model_name)}</td><td class="right mini">${Number(r.prompt_tokens) || 0}/${Number(r.completion_tokens) || 0}</td><td class="right">${money(q)}</td></tr>`;
  }).join("");
  return `<table class="kvtable"><tr><td class="k">时间</td><td class="k">模型</td><td class="right k">tokens(入/出)</td><td class="right k">费用</td></tr>${rows}</table>`;
}
// lineChart 手绘内联 SVG 折线图(不引图表库,沿用原生 JS 风格)。series=[{period,consumed_quota}]。
// 金额按 money() 同口径(500000=$1);hover 圆点 <title> 显示「日期: 金额」。单点居中,空数据降级提示。
function lineChart(series) {
  series = series || [];
  if (series.length === 0) return `<div class="empty">该时段暂无趋势数据</div>`;
  const W = 760, H = 220, padL = 54, padR = 10, padT = 20, padB = 28;
  const innerW = W - padL - padR, innerH = H - padT - padB, n = series.length;
  const maxv = Math.max(1, ...series.map(p => p.consumed_quota || 0));
  const x = i => padL + (n === 1 ? innerW / 2 : innerW * i / (n - 1));
  const y = v => padT + innerH - ((v || 0) / maxv) * innerH;
  const base = padT + innerH;
  const pts = series.map((p, i) => `${x(i).toFixed(1)},${y(p.consumed_quota).toFixed(1)}`).join(" ");
  const area = `${x(0).toFixed(1)},${base} ${pts} ${x(n - 1).toFixed(1)},${base}`;
  const yTicks = [maxv, Math.round(maxv / 2), 0].map(v => {
    const yy = y(v);
    return `<line x1="${padL}" y1="${yy.toFixed(1)}" x2="${W - padR}" y2="${yy.toFixed(1)}" stroke="#eef1f7"/>`
      + `<text x="${padL - 8}" y="${(yy + 3).toFixed(1)}" font-size="10" fill="#8a94a6" text-anchor="end">${money(v)}</text>`;
  }).join("");
  // 外圈 r=8 透明圆做命中热区(挂 data-label/data-val,自定义浮层用);内圈 r=3.5 是可见点。去掉原生 <title>,改自定义浮层(§1)。
  const dots = series.map((p, i) => `<g><circle class="lc-dot" data-label="${esc(p.period)}" data-val="${esc(money(p.consumed_quota))}" cx="${x(i).toFixed(1)}" cy="${y(p.consumed_quota).toFixed(1)}" r="8" fill="transparent"></circle><circle cx="${x(i).toFixed(1)}" cy="${y(p.consumed_quota).toFixed(1)}" r="3.5" class="lc-pt"></circle></g>`).join("");
  const step = Math.max(1, Math.ceil(n / 6));
  const idxs = [];
  for (let i = 0; i < n; i += step) idxs.push(i);
  if (idxs[idxs.length - 1] !== n - 1) idxs.push(n - 1); // 末期标签必显示
  const xl = idxs.map(i => {
    const xi = x(i); // 贴边标签换锚点防裁切(最左 start/最右 end/中间 middle)
    const anc = xi < W * 0.12 ? "start" : xi > W * 0.88 ? "end" : "middle";
    return `<text x="${xi.toFixed(1)}" y="${H - 6}" font-size="10" fill="#999" text-anchor="${anc}">${esc(series[i].period)}</text>`;
  }).join("");
  // 包一层 .lc-wrap(相对定位)承载自定义浮层 .lc-tip(pointer-events:none,防挡 hover)。
  return `<div class="lc-wrap"><svg viewBox="0 0 ${W} ${H}" style="width:100%;height:220px;display:block">`
    + yTicks
    + `<line x1="${padL}" y1="${base}" x2="${W - padR}" y2="${base}" stroke="#eee"/>`
    + `<polygon points="${area}" class="lc-area"/>`
    + `<polyline points="${pts}" class="lc-line"/>`
    + dots
    + `<text x="${padL}" y="${padT - 8}" font-size="10" fill="#8a94a6">消耗金额</text>`
    + xl + `</svg><div class="lc-tip" style="display:none"></div></div>`;
}
// 趋势图自定义浮层:document 级委托一次绑定(全局生效,不依赖每次渲染后重绑)。
// 从命中 .lc-dot 读 dataset,textContent 填充(不拼 HTML/onclick),按点坐标定位到正上方;移动端 touchstart 也触发。
function showLineTip(dot) {
  const wrap = dot.closest && dot.closest(".lc-wrap"); if (!wrap) return;
  const tip = wrap.querySelector(".lc-tip"); if (!tip) return;
  tip.textContent = "";
  const l = document.createElement("div"); l.className = "lc-tip-l"; l.textContent = dot.getAttribute("data-label") || "";
  const v = document.createElement("div"); v.className = "lc-tip-v"; v.textContent = dot.getAttribute("data-val") || "";
  tip.appendChild(l); tip.appendChild(v);
  tip.style.display = "block";
  const wr = wrap.getBoundingClientRect(), dr = dot.getBoundingClientRect();
  const cx = dr.left + dr.width / 2 - wr.left, cy = dr.top - wr.top;
  const tw = tip.offsetWidth, th = tip.offsetHeight;
  let xx = cx - tw / 2; xx = Math.max(2, Math.min(xx, wrap.clientWidth - tw - 2));
  let yy = cy - th - 10; if (yy < 2) yy = cy + 16;
  tip.style.left = xx + "px"; tip.style.top = yy + "px"; tip.style.opacity = "1";
}
function hideLineTip(dot) {
  const wrap = dot.closest && dot.closest(".lc-wrap"); if (!wrap) return;
  const tip = wrap.querySelector(".lc-tip"); if (tip) { tip.style.opacity = "0"; tip.style.display = "none"; }
}
function hideAllLineTips() { document.querySelectorAll(".lc-tip").forEach(t => { t.style.opacity = "0"; t.style.display = "none"; }); }
document.addEventListener("mouseover", e => { const d = e.target.closest && e.target.closest(".lc-dot"); if (d) showLineTip(d); });
document.addEventListener("mouseout", e => { const d = e.target.closest && e.target.closest(".lc-dot"); if (d) hideLineTip(d); });
document.addEventListener("touchstart", e => { const d = e.target.closest && e.target.closest(".lc-dot"); if (d) showLineTip(d); else hideAllLineTips(); }, { passive: true });

/* ---------- API ---------- */
async function api(method, path, body) {
  const h = { "Content-Type": "application/json" };
  if (S.token) h.Authorization = "Bearer " + S.token;
  const r = await fetch("/api/v1" + path, { method, headers: h, body: body ? JSON.stringify(body) : undefined });
  let j = {}; try { j = await r.json(); } catch (e) {}
  if (r.status === 401) { logout(); throw new Error(j.message || "登录已过期"); }
  if (j.code !== 0 && j.code !== undefined) throw new Error(j.message || ("请求失败 " + r.status));
  return j.data;
}
const money = q => "$" + (q / 500000).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }); // quota→美元(锚定 500000=$1),统一两位
// esc:HTML 文本/双引号属性转义(用于可见文本、title=、value= 等 HTML 上下文)。
// 安全审计 2026-06-30 修正:esc 仅适用 HTML 上下文,绝不可用于内联事件处理器(onclick="…")里的 JS 字符串参数——
// HTML 解析器会先把 &#39; 解码回 ',字符串被闭合可注入任意 JS(经典嵌套上下文坑;原"&#39; 能堵 onclick"判断为误)。
// onclick 内的字符串参数一律用 jsstr。
const esc = s => String(s == null ? "" : s).replace(/[&<>"'`]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;", "`": "&#96;" }[c]));
// jsstr:编码成「HTML 属性 + JS 字符串」双重上下文都安全的形式——非字母数字下划线一律转 \xHH / \uHHHH。
// 产物只含 反斜杠/x/u/十六进制,无 ' " < > & → HTML 解码后原样保留,JS 里是字面字符、不闭合字符串。用于 onclick 等内联处理器参数。
const jsstr = s => String(s == null ? "" : s).replace(/[^a-zA-Z0-9_]/g, c => { const n = c.charCodeAt(0); return n < 256 ? "\\x" + n.toString(16).padStart(2, "0") : "\\u" + n.toString(16).padStart(4, "0"); });
const roleCN = r => ({ operator: "运营方", org_admin: "组织管理员", team_leader: "团队负责人", member: "成员" }[r] || r);
const memberStatusCN = s => ({ active: "启用", disabled: "禁用", offboarded: "离职" }[s] || s || "-");
// D2(28):计费模式中文化,不再对客户/运营直出英文枚举。
const billingModeCN = s => ({ wallet: "余额钱包", prepaid: "预充值", postpaid: "后付费", subscription: "订阅" }[s] || s || "-");

/* ---------- 登录 ---------- */
async function doLogin() {
  const email = document.getElementById("lg_email").value.trim();
  const pw = document.getElementById("lg_pw").value;
  const err = document.getElementById("lg_err"); err.textContent = "";
  const btn = document.getElementById("lg_btn");
  if (btn.disabled) return; // F1(28):提交期间禁用,防快速双击重复提交
  btn.disabled = true; btn.textContent = "登录中…";
  try {
    const d = await api("POST", "/auth/login", { email, password: pw });
    S.token = d.token; localStorage.setItem("nx_token", S.token);
    document.getElementById("login").classList.add("hide");
    await boot();
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false; btn.textContent = "登录";
}
function logout() { S.token = ""; localStorage.removeItem("nx_token"); location.reload(); }
// F2(28):登录框产品名/浏览器标题走 BRAND 单一来源(app.js 在 body 底加载,此时 DOM 已就绪)。
document.title = BRAND.product;
{ const el = document.getElementById("lg_prod"); if (el) el.textContent = BRAND.product; }

/* ---------- 启动 ---------- */
async function boot() {
  S.me = await api("GET", "/me", null);
  document.getElementById("login").classList.add("hide"); // 自动登录路径也要隐藏登录浮层
  S.role = S.me.role; S.orgId = S.me.org_id;
  S.mvp = !!S.me.mvp_mode; // 改动⑥-2:MVP 灰度,前端藏掉钱/控入口(真正拦截以后端 mvpGate 为准)
  const def = { operator: "orgs", org_admin: "dash", team_leader: "members", member: "myusage" }[S.role];
  S.view = def;
  renderShell(); renderSide(); renderView();
}

// v1 观测管理版(20-§2.1 裁定A):额度申请/审批/tier 限额属 v2 配额功能,前端隐藏(approvals/myreq 不在菜单);
// billing 改纯"余额"(充值在 new-api,平台无充值入口,19-F6)。
const NAV = {
  operator: [{ grp: "运营" }, { v: "orgs", ic: "▦", t: "客户组织" }, { v: "employees", ic: "☷", t: "员工管理" }, { v: "oplogs", ic: "▤", t: "使用日志" }],
  org_admin: [{ grp: "管理" }, { v: "dash", ic: "◧", t: "概览" }, { v: "members", ic: "☷", t: "成员" }, { v: "teams", ic: "▣", t: "团队" }, { v: "tiers", ic: "◆", t: "可用模型档位" }, { v: "logs", ic: "▤", t: "使用日志" }, { v: "billing", ic: "¥", t: "余额" }, { v: "mynotif", ic: "✉", t: "通知" }],
  team_leader: [{ grp: "团队" }, { v: "members", ic: "☷", t: "团队成员" }],
  member: [{ grp: "我的" }, { v: "myusage", ic: "▦", t: "我的用量" }, { v: "mykey", ic: "⚿", t: "我的 API Key" }, { v: "logs", ic: "▤", t: "使用日志" }, { v: "mynotif", ic: "✉", t: "通知" }],
};

function renderShell() {
  const av = (S.me.display_name || S.me.login_email || "U").slice(0, 1).toUpperCase();
  document.getElementById("app").innerHTML = `
  <div class="app">
    <div class="top">
      <button class="hamb" onclick="toggleSide()" title="菜单">☰</button>
      <div class="logo"><span class="mk">N</span> ${esc(BRAND.product)}</div>
      <span class="demolbl">${esc(roleCN(S.role))}</span>
      <div class="spacer"></div>
      <div class="me"><span class="av">${esc(av)}</span><span>${esc(S.me.display_name || S.me.login_email)}</span>
        <span class="lk" onclick="openHelp()" style="margin-left:10px">帮助</span>
        <span class="lk" onclick="go('settings')" style="margin-left:10px">设置</span>
        <span class="lk" onclick="logout()" style="margin-left:10px">退出</span></div>
    </div>
    <div class="side" id="side"></div>
    <div class="main" id="main"></div>
    <div class="footer">© 2026 ${brandCompany() ? esc(brandCompany()) + " · " : ""}${esc(BRAND.product)} · 数据仅用于用量统计</div>
  </div>`;
}
function openHelp() {
  const docLine = brandDoc() ? `<div class="fld"><label>产品文档</label><a href="${esc(brandDoc())}" target="_blank" rel="noopener">${esc(brandDoc())}</a></div>` : "";
  modal("帮助与支持", `${docLine}<div class="fld"><label>联系客服</label><a href="mailto:${esc(BRAND.support)}">${esc(BRAND.support)}</a></div>
    <div class="note">如遇账号登录、用量统计、成员开通等问题,可邮件联系客服。</div>`,
    `<button class="btn pri" onclick="closeM()">知道了</button>`);
}
// NAV_IC 导航图标(28-建议批):内联 SVG 取代 Unicode 几何字符(跨平台渲染发虚),复用 copyBtn 的 stroke 体系。
const NAV_IC = (() => {
  const w = p => `<svg viewBox="0 0 24 24" aria-hidden="true" style="width:15px;height:15px;fill:none;stroke:currentColor;stroke-width:2;stroke-linecap:round;stroke-linejoin:round">${p}</svg>`;
  return {
    orgs: w('<rect x="3" y="7" width="18" height="14" rx="2"></rect><path d="M8 7V5a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path>'),
    employees: w('<circle cx="9" cy="8" r="3.2"></circle><path d="M3.5 20c.6-3.2 2.8-5 5.5-5s4.9 1.8 5.5 5"></path><path d="M16 8.5a2.8 2.8 0 1 0 0-5.6"></path><path d="M17 15c2.2.4 3.6 2 4 5"></path>'),
    members: w('<circle cx="9" cy="8" r="3.2"></circle><path d="M3.5 20c.6-3.2 2.8-5 5.5-5s4.9 1.8 5.5 5"></path><path d="M16 8.5a2.8 2.8 0 1 0 0-5.6"></path><path d="M17 15c2.2.4 3.6 2 4 5"></path>'),
    oplogs: w('<path d="M4 6h16M4 12h16M4 18h10"></path>'),
    logs: w('<path d="M4 6h16M4 12h16M4 18h10"></path>'),
    dash: w('<rect x="3" y="3" width="8" height="8" rx="1.5"></rect><rect x="13" y="3" width="8" height="8" rx="1.5"></rect><rect x="3" y="13" width="8" height="8" rx="1.5"></rect><rect x="13" y="13" width="8" height="8" rx="1.5"></rect>'),
    teams: w('<rect x="3" y="8" width="18" height="12" rx="2"></rect><path d="M3 8l3-4h12l3 4"></path>'),
    tiers: w('<path d="M12 3l9 9-9 9-9-9z"></path>'),
    billing: w('<circle cx="12" cy="12" r="9"></circle><path d="M9 9l3 4 3-4M12 13v5M9.5 15h5"></path>'),
    mynotif: w('<rect x="3" y="5" width="18" height="14" rx="2"></rect><path d="M3 7l9 6 9-6"></path>'),
    myusage: w('<path d="M4 20V10M10 20V4M16 20v-8M21 20H3"></path>'),
    mykey: w('<circle cx="8" cy="14" r="4"></circle><path d="M11 11l9-9M16 6l3 3"></path>'),
  };
})();
function renderSide() {
  let h = "";
  // v1:审批/申请增额已直接从 NAV 移除(裁定A);余额页对全角色可见(读求和,19-F3)。
  (NAV[S.role] || []).forEach(n => {
    if (n.grp) { h += `<div class="grp">${esc(n.grp)}</div>`; return; }
    h += `<div class="nav ${S.view === n.v ? "on" : ""}" id="nav-${n.v}" onclick="go('${n.v}')"><span class="ic">${NAV_IC[n.v] || n.ic || ""}</span>${esc(n.t)}</div>`;
  });
  document.getElementById("side").innerHTML = h;
  updateBadges();
}
async function updateBadges() {
  try {
    if (S.role === "member" || S.role === "org_admin") {
      const n = await api("GET", "/notifications?page=1&page_size=1", null);
      setBadge("mynotif", n.unread || 0);
    }
  } catch (e) {}
}
function setBadge(v, n) {
  const el = document.getElementById("nav-" + v);
  if (el && n > 0) el.insertAdjacentHTML("beforeend", `<span class="badge">${n}</span>`);
}
function go(v) { S.view = v; renderSide(); renderView(); const sd = document.getElementById("side"); if (sd) sd.classList.remove("open"); }
function toggleSide() { const sd = document.getElementById("side"); if (sd) sd.classList.toggle("open"); }

async function renderView() {
  const m = document.getElementById("main");
  m.innerHTML = `<div class="empty">加载中…</div>`;
  const fn = VIEWS[S.view];
  if (!fn) { m.innerHTML = `<div class="empty">待建</div>`; return; }
  try { m.innerHTML = await fn(); if (VIEWS_AFTER[S.view]) VIEWS_AFTER[S.view](); }
  catch (e) { m.innerHTML = `<div class="empty" style="color:var(--bad)">加载失败:${esc(e.message)}</div>`; }
}

/* ---------- helpers ---------- */
function head(h1, sub) {
  // A4:支持态横幅——运营方进入客户组织支持会话后,顶部常显身份提示 + 退出入口(还原运营方身份)。
  const sb = S.opToken ? `<div class="safebar" style="background:#fff3cd;color:#7a5b00">支持态:你正以支持身份操作该组织,每步双身份留痕。<span class="lk" onclick="exitSupport()" style="margin-left:8px"><b>退出支持会话</b></span></div>` : "";
  return sb + `<div class="h1">${esc(h1)}</div><div class="sub">${esc(sub || "")}</div>`;
}
// A4:退出支持会话,还原运营方身份(支持 token 仅内存,localStorage 始终是运营方 token,刷新亦回运营方)。
function exitSupport() { if (!S.opToken) return; S.token = S.opToken; S.opToken = null; S.supportOrgId = 0; toast("已退出支持会话"); boot(); }
function kpi(k, v, d) { return `<div class="card kpi"><div class="k">${esc(k)}</div><div class="v">${v}</div><div class="d">${esc(d || "")}</div></div>`; }
function pill(t, c) { return `<span class="pill ${c || "mut"}">${esc(t)}</span>`; }
function toast(t) { const e = document.getElementById("toast"); e.textContent = t; e.classList.add("on"); setTimeout(() => e.classList.remove("on"), 2200); }
function modal(title, body, footer) {
  document.getElementById("modal").innerHTML = `<div class="mh">${esc(title)}<span class="x" onclick="closeM()">×</span></div><div class="mb">${body}</div><div class="mf">${footer || ""}</div>`;
  document.getElementById("mask").classList.add("on");
}
function closeM() { document.getElementById("mask").classList.remove("on"); }
const val = id => (document.getElementById(id) || {}).value || "";
const VIEWS_AFTER = {};
function dangerConfirm(title, body, confirmText, action) {
  modal(title, `<div class="danger-confirm">${body}</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn danger danger-solid" onclick="${action}">${esc(confirmText || "确认")}</button>`);
}
function copyText(text, label) {
  const s = String(text || "");
  if (!s || s === "-") { toast("没有可复制内容"); return; }
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(s).then(() => toast("已复制" + (label ? label : ""))).catch(() => toast("复制失败"));
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = s; document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); toast("已复制" + (label ? label : "")); } catch (e) { toast("复制失败"); }
  document.body.removeChild(ta);
}
function copyBtn(text, label) {
  return `<button class="icon-btn copy-btn" title="复制${esc(label || "")}" aria-label="复制${esc(label || "")}" onclick="copyText('${jsstr(text)}','${jsstr(label || "内容")}')"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="10" height="10" rx="2"></rect><path d="M5 15V7a2 2 0 0 1 2-2h8"></path></svg></button>`;
}
// revealMemberKey:点"复制完整"→ 调揭示端点即时取明文(平台不存明文)→ 弹框展示 + 复制(27-§3.2)。
// 走弹框而非直接写剪贴板:clipboard.writeText 需在用户手势同步栈,await 后调用 Safari 会失效;弹框内复制按钮点击是新手势,稳。
// 运营方无此入口;后端 RevealKey 再兜底(operator 403、越权 403、离职/禁用 403)。
async function revealMemberKey(memberID, name) {
  try {
    const d = await api("POST", "/members/" + memberID + "/key:reveal", null);
    modal("完整 API Key" + (name ? (" · " + name) : ""),
      `<div class="note">明文 Key,复制后请妥善保管;平台不留存明文,如泄露请轮换。</div>
       <div class="keybox"><span>${esc(d.api_key)}</span>${copyBtn(d.api_key, "API Key")}</div>`,
      `<button class="btn pri" onclick="closeM()">完成</button>`);
  } catch (e) { toast(e.message); }
}
// revealKeyBtn:脱敏串旁的"复制完整"入口(员工本人 / 组织管理员本组织成员)。运营方视图不放此按钮。
function revealKeyBtn(memberID, name) {
  return `<button class="btn micro reveal-key" onclick="revealMemberKey(${memberID},'${jsstr(name || "")}')">复制完整</button>`;
}
// 空状态引导:图标 + 标题 + 说明 +(可选)行动按钮。表格内用时外层包 <td colspan>。
function emptyState(icon, title, desc, btnText, onclick) {
  const btn = btnText ? `<button class="btn pri" style="margin-top:14px" onclick="${onclick}">${esc(btnText)}</button>` : "";
  return `<div class="emptyx"><div class="emptyx-ic">${icon}</div>
    <div class="emptyx-t">${esc(title)}</div>
    <div class="emptyx-d">${esc(desc || "")}</div>${btn}</div>`;
}
function emptyx(title, desc) { return emptyState("·", title, desc, "", ""); }

/* ===================== 运营方 ===================== */
const VIEWS = {};

/* ---- 个人设置(全角色:账号信息 + 改密) ---- */
VIEWS.settings = async () => {
  const m = S.me;
  return head("个人设置", "账号信息与登录密码")
    + `<div class="panel"><div class="ph">账号信息</div><div class="pb"><table class="kvtable">
        <tr><td class="k">显示名</td><td><input id="st_dn" value="${esc(m.display_name || "")}" placeholder="你的显示名" style="width:200px;padding:5px 9px;vertical-align:middle"> <button class="btn sm pri" onclick="doSaveDisplayName()">保存</button></td></tr>
        <tr><td class="k">登录邮箱</td><td>${esc(m.login_email)}</td></tr>
        <tr><td class="k">角色</td><td>${esc(roleCN(m.role))}</td></tr></table></div></div>
    <div class="panel"><div class="ph">修改密码</div><div class="pb">
        <div class="fld" style="max-width:360px"><label>当前密码</label><input id="cp_old" type="password" placeholder="当前密码"></div>
        <div class="fld" style="max-width:360px"><label>新密码</label><input id="cp_new" type="password" placeholder="8–64 位"></div>
        <div class="fld" style="max-width:360px"><label>确认新密码</label><input id="cp_new2" type="password" placeholder="再输一次"></div>
        <button class="btn pri" onclick="doChangePassword()">保存新密码</button>
      </div></div>`;
};
async function doSaveDisplayName() {
  try {
    const d = await api("PATCH", "/me", { display_name: val("st_dn") });
    S.me.display_name = d.display_name; // 刷新顶栏头像/名
    toast("显示名已更新");
    renderShell(); renderSide(); renderView();
  } catch (e) { toast(e.message); }
}
async function doChangePassword() {
  const oldp = val("cp_old"), np = val("cp_new"), np2 = val("cp_new2");
  if (!oldp || !np) { toast("请填写当前密码与新密码"); return; }
  if (np !== np2) { toast("两次新密码不一致"); return; }
  if (np.length < 8 || np.length > 64) { toast("新密码须 8–64 位"); return; }
  try {
    await api("POST", "/me/password", { old_password: oldp, new_password: np });
    toast("密码已修改,请牢记新密码");
    ["cp_old", "cp_new", "cp_new2"].forEach(id => { const e = document.getElementById(id); if (e) e.value = ""; });
  } catch (e) { toast(e.message); }
}

VIEWS.orgs = async () => {
  const inclArch = !!S.orgShowArchived; // T12:是否显示已归档
  // B1(28):接后端分页 + q 服务端搜索——原来 page_size=50 无分页、搜索只筛前端已加载的 50 条,是假搜索。
  const page = S.orgsPage || 1, size = 50;
  const q = (S.orgsQ || "").trim();
  const d = await api("GET", "/organizations?page=" + page + "&page_size=" + size
    + (q ? "&q=" + encodeURIComponent(q) : "") + (inclArch ? "&include_archived=true" : ""), null);
  // 过滤掉平台运营方伪组织(slug=_operator),只列真实客户(R2-轻微)。
  const list = (d.list || []).filter(o => o.slug !== "_operator");
  const total = (d.pagination || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(total / size));
  const rows = list.map(o => `<tr>
    <td><span class="lk" style="display:inline-block;max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:middle" title="${esc(o.name)}" onclick="enterOrg(${o.id},'${jsstr(o.name)}')">${esc(o.name)}</span><div class="mini">${esc(o.slug)}</div></td>
    <td>${pill(o.status, o.status === "active" ? "ok" : o.status === "low" ? "warn" : "bad")}${o.archived ? ' <span class="tag">已归档</span>' : ''}</td>
    <td>${esc(o.timezone)}</td><td>${esc(billingModeCN(o.billing_mode))}</td>
    <td class="right"><span class="btn sm" onclick="enterOrg(${o.id},'${jsstr(o.name)}')">进入</span> ${o.archived
      ? `<span class="btn sm" onclick="doArchiveOrg(${o.id},'${jsstr(o.name)}',false)">取消归档</span>`
      : `<span class="btn sm" onclick="doArchiveOrg(${o.id},'${jsstr(o.name)}',true)">归档</span>`}</td></tr>`).join("");
  return head("客户组织", "运营方:管理所有客户组织、入账、计费灰度、支持会话")
    + `<div class="toolbar"><div class="search"><input id="orgSearch" value="${esc(q)}" placeholder="搜索组织名或 slug…" onkeydown="if(event.key==='Enter')doOrgSearch()"></div>
       <button class="btn sm" onclick="doOrgSearch()">搜索</button>
       <label class="mini" style="margin:0 10px;cursor:pointer"><input type="checkbox" ${inclArch ? "checked" : ""} onchange="S.orgShowArchived=this.checked;S.orgsPage=1;renderView()"> 显示已归档</label>
       <button class="btn pri" onclick="openCreateOrg()">+ 新建客户组织</button></div>
    <div class="panel"><div class="table-scroll"><table><thead><tr><th>组织</th><th>状态</th><th>时区</th><th>计费</th><th></th></tr></thead>
    <tbody id="orgTbody">${rows || '<tr><td colspan=5>' + emptyState("▦", q ? "没有匹配的组织" : "还没有客户组织", q ? "换个关键词试试" : "新建第一个客户组织开始管理", q ? "" : "+ 新建客户组织", "openCreateOrg()") + '</td></tr>'}</tbody></table></div>
    <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 个 · 第 ${page}/${maxPage} 页</span>
      <button onclick="goOrgsPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
      <button onclick="goOrgsPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div></div>`;
};
function doOrgSearch() { S.orgsQ = val("orgSearch"); S.orgsPage = 1; renderView(); }
function goOrgsPage(p) { S.orgsPage = Math.max(1, p); renderView(); }
async function doArchiveOrg(id, name, archive) {
  if (!archive) { // 取消归档是恢复性操作,轻确认即可
    try { await api("POST", "/organizations/" + id + "/unarchive", null); toast("已取消归档"); renderView(); } catch (e) { toast(e.message); }
    return;
  }
  dangerConfirm("确认归档组织?", `<p>确认归档组织「${esc(name)}」?</p><p>归档后从默认列表隐藏,数据保留、可随时恢复。</p>`,
    "确认归档", `doArchiveOrgConfirmed(${id})`);
}
async function doArchiveOrgConfirmed(id) {
  try { await api("POST", "/organizations/" + id + "/archive", null); closeM(); toast("已归档"); renderView(); } catch (e) { toast(e.message); }
}
// filterEls:通用按 data-q 筛选容器直接子元素(#6 排行/搜索看全用,bar 不是 table 行,filterRows 用不上)。
function filterEls(inputId, containerId) {
  const q = (document.getElementById(inputId).value || "").trim().toLowerCase();
  document.querySelectorAll("#" + containerId + " [data-q]").forEach(el => {
    el.style.display = (!q || el.getAttribute("data-q").indexOf(q) >= 0) ? "" : "none";
  });
}
// openMemberUsage:#5 单员工下钻——拉该员工当前时间窗的按模型明细(后端 /members/{id}/usage 已具备)。
async function openMemberUsage(mid, name) {
  try {
    const [u, d] = await Promise.all([
      api("GET", "/members/" + mid + "/usage?since_hours=" + S.win, null),
      api("GET", "/members/" + mid + "/usage/detail?since_hours=" + S.win + "&page_size=50", null).catch(() => ({ records: [], total: 0 })),
    ]);
    const rows = (u.by_model || []).map(b => `<tr><td>${esc(b.key)}</td><td class="right">${money(b.consumed_quota)}</td><td class="right mini">${Number(b.count) || 0}</td></tr>`).join("") || `<tr><td class="empty" colspan="3">该窗口暂无用量</td></tr>`;
    const dRecords = (d && d.records) || [], dTotal = (d && d.total) || 0;
    modal("员工用量明细 · " + name + "(" + winLabel(S.win) + ")",
      `<table class="kvtable"><tr><td class="k">模型</td><td class="right k">费用</td><td class="right k">次数</td></tr>${rows}
       <tr><td class="k">合计</td><td class="right"><b>${money(u.total_quota)}</b></td><td></td></tr></table>
       <div class="ph" style="margin-top:14px">最近调用(共 ${dTotal} 条,显示最近 ${dRecords.length})</div>${detailTable(dRecords)}`,
      `<button class="btn pri" onclick="closeM()">关闭</button>`);
  } catch (e) { toast(e.message); }
}
// downloadUsageCsv:#3 导出用量对账 CSV(员工名+美元)。导出端点需 Bearer,故 fetch+blob 下载(<a href> 带不上鉴权头)。
async function downloadUsageCsv() {
  try {
    const r = await fetch("/api/v1/organizations/" + S.orgId + "/usage/export?since_hours=" + S.win, { headers: S.token ? { Authorization: "Bearer " + S.token } : {} });
    if (!r.ok) { toast("导出失败 " + r.status); return; }
    const url = URL.createObjectURL(await r.blob());
    const a = document.createElement("a");
    a.href = url; a.download = "usage_org" + S.orgId + "_" + winLabel(S.win) + ".csv";
    document.body.appendChild(a); a.click(); a.remove(); URL.revokeObjectURL(url);
    toast("已导出 CSV"); // 28-建议批:成功反馈,不再无声
  } catch (e) { toast(e.message); }
}
// v1 组织入网两个门(19-F1):门A 新建(平台建 new-api 用户)/ 门B 关联现有(录入企业 user + access token,
// 导入其令牌为成员)。进来后同一种组织。
function openCreateOrg() {
  modal("组织入网", `<div class="fld"><label>接入方式</label><select id="co_mode" onchange="document.getElementById('co_assoc').style.display=this.value==='associate'?'':'none'">
      <option value="create">新建(平台自动创建 new-api 用户)</option>
      <option value="associate">关联现有 new-api 用户(企业已在用)</option></select></div>
    <div class="fld"><label>组织名称</label><input id="co_n" placeholder="Acme 科技"></div>
    <div class="fld"><label>唯一标识 slug</label><input id="co_s" placeholder="acme"></div>
    <div class="fld"><label>管理员邮箱</label><input id="co_e" placeholder="admin@acme.com"></div>
    <div class="fld"><label>new-api 用户分组</label><input id="co_g" placeholder="org_acme"></div>
    <div id="co_assoc" style="display:none">
      <div class="fld"><label>企业 new-api 用户 ID</label><input id="co_uid" type="number" placeholder="123"></div>
      <div class="fld"><label>该用户的 access token</label><input id="co_tok" placeholder="企业在 new-api 个人设置里生成后粘入"></div>
      <div class="fld"><label>导入成员显示名</label><select id="co_np"><option value="inherit">继承令牌名(可改)</option><option value="random">随机串(可改)</option></select></div>
      <div class="note">关联要求:token 必须属<b>普通用户</b>(拒管理员);建议企业为组织专门建一个普通用户再生成 token。
        关联后其名下全部令牌导入为成员,员工继续用旧 key;分组须与该用户在 new-api 的分组一致。</div>
    </div>
    <div class="note">new-api 用户分组须先在 new-api 配好(挂模型分组 group_special_usable_group),否则建组织会被拒。一个分组只绑一个组织。管理员初始密码本次回显一次。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateOrg()">创建</button>`);
}
async function doCreateOrg() {
  try {
    const body = { name: val("co_n"), slug: val("co_s"), admin_email: val("co_e"), newapi_user_group: val("co_g") };
    if (val("co_mode") === "associate") {
      body.associate = { newapi_user_id: parseInt(val("co_uid"), 10) || 0, access_token: val("co_tok"), name_policy: val("co_np") };
    }
    const d = await api("POST", "/organizations", body);
    const impLine = body.associate ? `<tr><td class="k">导入成员</td><td>${d.imported_members || 0} 个${d.import_failed ? `(失败 ${d.import_failed} 个,可在组织页"重新导入"补齐)` : ""}</td></tr>` : "";
    modal("已创建", `<div class="note">组织已创建。请把管理员初始凭证交付客户(仅显示一次):</div>
      <table class="kvtable"><tr><td class="k">管理员邮箱</td><td>${esc(d.admin_email)}</td></tr>
      <tr><td class="k">初始密码</td><td><b>${esc(d.admin_initial_password)}</b></td></tr>${impLine}</table>`,
      `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
async function enterOrg(id, name) {
  const prevOrgId = S.orgId;
  S.orgId = id; S.org = { id, name };
  if (prevOrgId !== id || !S.orgTab) S.orgTab = "overview";
  if (prevOrgId !== id) S.orgMemPage = 1; // A2(28):成员 Tab 分页,换组织重置页码
  // C2(28):先渲染载入骨架再拉数——原来 4 个 await 后才首次改 DOM,点"进入"像卡死、会重复点。
  {
    const pre = document.getElementById("main");
    if (pre) pre.innerHTML = head("客户组织详情", "运营方支持视角 · 客户概览 / 成员排障 / 使用日志 / 风险控制")
      + `<div class="crumb"><span class="lk" onclick="go('orgs')">客户组织</span><span class="sep">/</span><b>${esc(name)}</b></div>
         <div class="panel"><div class="pb"><div class="empty">正在载入「${esc(name)}」…</div></div></div>`;
  }
  // v1(20-§9):充值在 new-api 完成,平台无充值/续充入口(escrow 休眠);运营方看实时余额 + 硬停(风控)+ 门B 重新导入。
  const [org, eb, bf] = await Promise.all([
    api("GET", "/organizations/" + id, null),
    api("GET", "/organizations/" + id + "/escrow-balance", null).catch(() => ({ __err: true })),
    api("GET", "/organizations/" + id + "/backfill", null).catch(() => ({ status: "none" })),
  ]);
  const mem = await api("GET", "/organizations/" + id + "/members?page=1&page_size=20", null);
  const main = document.getElementById("main");
  const hardStopped = org.status === "hard_stopped";
  S.orgDetail = { org, eb, bf, mem };
  main.innerHTML = head("客户组织详情", "运营方支持视角 · 客户概览 / 成员排障 / 使用日志 / 风险控制") // C10:head() 内部已 esc,勿双重转义
    + `<div class="crumb"><span class="lk" onclick="go('orgs')">客户组织</span><span class="sep">/</span><b>${esc(name)}</b></div>
    ${hardStopped ? `<div class="note" style="color:#c00">该组织处于运维硬停中:全部 key 已 403,平台管理操作暂不可用,解除后恢复。</div>` : ""}
    <div class="org-hero">
      <div>
        <div class="org-title">${esc(name)}</div>
        <div class="org-meta">组织 ID ${id} · ${esc(billingModeCN(org.billing_mode || "wallet"))} · ${esc(org.timezone || "Asia/Shanghai")}</div>
      </div>
      <div class="org-status">${pill(org.status, org.status === "active" ? "ok" : hardStopped ? "bad" : "warn")}</div>
    </div>
    <div class="cards">
      ${kpi("实时余额", balMoney(eb), "实时读 new-api 余额")}
      ${kpi("组织状态", pill(org.status, org.status === "active" ? "ok" : hardStopped ? "bad" : "warn"), hardStopped ? "组织级硬停中" : "正常服务中")}
      ${kpi("成员数", (mem.pagination || {}).total || (mem.list || []).length || 0, "API 使用成员")}
    </div>
    ${bf && bf.status && bf.status !== "none" ? `<div class="panel"><div class="ph">历史用量回填</div><div class="pb">${backfillLine(bf)}</div></div>` : ""}
    <div class="tabs org-tabs">
      ${orgTabButton("overview", "概览")}
      ${orgTabButton("members", "成员")}
      ${orgTabButton("tokens", "API Key 归属")}
      ${orgTabButton("logs", "使用日志")}
      ${orgTabButton("support", "支持会话")}
      ${orgTabButton("risk", "风险控制")}
    </div>
    <div id="orgTabBody">${renderOrgOverviewPanel(org, eb, bf, mem)}</div>`;
  if (S.orgTab !== "overview") renderOrgCurrentTab();
}
function orgTabButton(tab, label) {
  return `<button data-org-tab="${tab}" class="${S.orgTab === tab ? "on" : ""}" onclick="switchOrgTab('${tab}')">${label}</button>`;
}
function switchOrgTab(tab) {
  S.orgTab = tab;
  document.querySelectorAll("[data-org-tab]").forEach(b => b.classList.toggle("on", b.dataset.orgTab === tab));
  renderOrgCurrentTab();
}
async function renderOrgCurrentTab() {
  const box = document.getElementById("orgTabBody");
  if (!box || !S.orgId) return;
  box.innerHTML = `<div class="panel"><div class="pb">${emptyx("加载中", "正在读取数据...")}</div></div>`;
  try {
    if (S.orgTab === "overview") {
      if (!S.orgDetail) {
        const [org, eb, bf, mem] = await Promise.all([
          api("GET", "/organizations/" + S.orgId, null),
          api("GET", "/organizations/" + S.orgId + "/escrow-balance", null).catch(() => ({ __err: true })),
          api("GET", "/organizations/" + S.orgId + "/backfill", null).catch(() => ({ status: "none" })),
          api("GET", "/organizations/" + S.orgId + "/members?page=1&page_size=20", null),
        ]);
        S.orgDetail = { org, eb, bf, mem };
      }
      box.innerHTML = renderOrgOverviewPanel(S.orgDetail.org, S.orgDetail.eb, S.orgDetail.bf, S.orgDetail.mem);
    } else if (S.orgTab === "members") {
      const mem = await api("GET", "/organizations/" + S.orgId + "/members?page=" + (S.orgMemPage || 1) + "&page_size=50", null);
      box.innerHTML = renderOrgMembersPanel(mem);
    } else if (S.orgTab === "tokens") {
      await renderTokenMappingsPanel(S.orgId);
    } else if (S.orgTab === "logs") {
      if (!S.logMirror || S.logMirror.orgId !== S.orgId) {
        S.logMirror = newLogMirrorState(S.orgId);
      }
      await renderNewapiLogs();
    } else if (S.orgTab === "support") {
      box.innerHTML = renderSupportPanel(S.orgId);
    } else if (S.orgTab === "risk") {
      const org = await api("GET", "/organizations/" + S.orgId, null);
      S.orgDetail = Object.assign({}, S.orgDetail || {}, { org });
      box.innerHTML = renderRiskPanel(org);
    }
  } catch (e) {
    box.innerHTML = `<div class="panel"><div class="pb">${emptyx("加载失败", e.message)}</div></div>`;
  }
}
function renderOrgOverviewPanel(org, eb, bf, mem) {
  const id = S.orgId;
  const hardStopped = org.status === "hard_stopped";
  const memberTotal = (mem.pagination || {}).total || (mem.list || []).length || 0;
  const reimport = org.newapi_created_by_platform === false
    ? `<button class="btn" onclick="doReimport(${id})">重新导入令牌</button>`
    : `<span class="mini">平台自动创建组织,无需令牌导入</span>`;
  const backfill = bf && bf.status && bf.status !== "none"
    ? `<button class="btn" onclick="doRequeueBackfill(${id})">重新回填历史用量</button>`
    : `<span class="mini">暂无历史回填任务</span>`;
  return `<div class="row2">
    <div class="panel">
      <div class="ph">组织概览</div>
      <div class="pb">
        <table class="kvtable">
          <tr><td class="k">实时余额</td><td><b>${balMoney(eb)}</b></td></tr>
          <tr><td class="k">组织状态</td><td>${pill(org.status, org.status === "active" ? "ok" : hardStopped ? "bad" : "warn")}</td></tr>
          <tr><td class="k">成员数</td><td>${memberTotal}</td></tr>
          <tr><td class="k">计费方式</td><td>${esc(billingModeCN(org.billing_mode || "-"))}</td></tr>
          <tr><td class="k">时区</td><td>${esc(org.timezone || "-")}</td></tr>
        </table>
      </div>
    </div>
    <div class="panel">
      <div class="ph">运营操作</div>
      <div class="pb action-list">
        <div class="action-row"><div><b>API Key 归属</b><div class="mini">排查 token_id 属于哪位成员</div></div><button class="btn sm" onclick="switchOrgTab('tokens')">查看</button></div>
        <div class="action-row"><div><b>使用日志</b><div class="mini">按 Request ID、成员、类型排查调用</div></div><button class="btn sm" onclick="switchOrgTab('logs')">查看</button></div>
        <div class="action-row"><div><b>支持会话</b><div class="mini">以受控身份进入客户组织协助排障</div></div><button class="btn sm" onclick="switchOrgTab('support')">进入</button></div>
      </div>
    </div>
    <div class="panel">
      <div class="ph">维护任务</div>
      <div class="pb action-list">
        <div class="action-row"><div><b>令牌导入</b><div class="mini">仅关联现有 new-api 用户的组织需要</div></div>${reimport}</div>
        <div class="action-row"><div><b>历史用量回填</b><div class="mini">用于补齐迁移前后的历史记录</div></div>${backfill}</div>
      </div>
    </div>
    <div class="panel ${hardStopped ? "risk-card" : ""}">
      <div class="ph">风险状态</div>
      <div class="pb">
        <div class="status-copy">${hardStopped ? "该组织已被硬停,所有 API Key 已返回 403。解除前不要做普通排障结论。" : "当前未硬停。遇到欠费纠纷、滥用或安全事件时,进入风险控制执行组织级硬停。"}</div>
        <button class="btn ${hardStopped ? "pri" : "danger"}" style="margin-top:12px" onclick="switchOrgTab('risk')">${hardStopped ? "处理硬停" : "进入风险控制"}</button>
      </div>
    </div>
  </div>`;
}
function renderOrgMembersPanel(mem) {
  const rows = (mem.list || []).map(m => `<tr><td>${esc(m.display_name || m.login_email)}</td>
    <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td><td class="mini">${esc(m.key_masked || "-")}</td></tr>`).join("");
  // A2(28-§阻断):成员 Tab 分页——>50 成员时显示总数 + 上/下页,消除"page_size=20 静默截断致运营看不到人"。
  const total = (mem.pagination || {}).total || (mem.list || []).length || 0;
  const page = S.orgMemPage || 1, pages = Math.max(1, Math.ceil(total / 50));
  const pager = total > 50
    ? `<div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 人 · 第 ${page}/${pages} 页</span>
        <button onclick="goOrgMemPage(-1)" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goOrgMemPage(1)" ${page >= pages ? "disabled" : ""}>下一页</button></div>`
    : `<div class="mini" style="text-align:right;margin-top:8px">共 ${total} 人</div>`;
  return `<div class="panel"><div class="ph">成员</div><div class="pb"><div class="table-scroll"><table><thead><tr><th>成员</th><th>状态</th><th>Key(脱敏)</th></tr></thead><tbody>${rows || `<tr><td colspan="3">${emptyState("☷", "该组织还没有成员", "开通员工后会显示在这里", "", "")}</td></tr>`}</tbody></table></div>${pager}</div></div>`;
}
function goOrgMemPage(delta) {
  S.orgMemPage = Math.max(1, (S.orgMemPage || 1) + delta);
  renderOrgCurrentTab();
}
function renderRiskPanel(org) {
  const hardStopped = org.status === "hard_stopped";
  return `<div class="panel danger-zone">
    <div class="ph">组织级风险控制</div>
    <div class="pb">
      <div class="danger-title">${hardStopped ? "当前组织已硬停" : "组织硬停"}</div>
      <div class="danger-desc">
        ${hardStopped
          ? "解除硬停后,该组织的 API Key 将恢复可用。解除前请确认欠费、滥用或安全问题已经处理完成。"
          : "硬停会禁用该组织对应的 new-api 用户,使该组织下全部 API Key 近实时返回 403。此操作用于欠费纠纷、疑似滥用、严重安全风险等场景,不是普通员工禁用。"}
      </div>
      <div class="risk-facts">
        <div><b>影响范围</b><span>整个组织,不是单个员工</span></div>
        <div><b>调用影响</b><span>全部 API Key 近实时 403</span></div>
        <div><b>恢复方式</b><span>可由运营方解除硬停</span></div>
      </div>
      ${hardStopped
        ? `<button class="btn pri" onclick="doHardStop(${S.orgId},false)">解除硬停</button>`
        : `<button class="btn danger danger-solid" onclick="confirmHardStop(${S.orgId})">确认进入硬停流程</button>`}
    </div>
  </div>`;
}
// 历史回填状态行(24-§9):回填中 / 已同步·起点 / 失败·重跑。
function backfillLine(bf) {
  if (bf.status === "done") {
    const s = bf.earliest_seen_ts ? new Date(bf.earliest_seen_ts * 1000).toLocaleDateString() : "-";
    return `${pill("已同步", "ok")} 历史用量已全部回填,起点 ${s}(逐条 ${bf.rows_ingested || 0} 条,可在成员用量下钻查全历史)。`;
  }
  if (bf.status === "failed") {
    return `${pill("回填失败", "warn")} ${esc(bf.last_error || "")} —— 点"重新回填"重试(幂等,不会重复计)。`;
  }
  return `${pill("回填中", "mut")} 正在同步该企业历史用量…(已灌 ${bf.rows_ingested || 0} 条,通常秒级完成,可刷新查看)。`;
}
// 运营方重新回填(幂等):后台从边界重跑,detail 幂等不双算。
async function doRequeueBackfill(id) {
  try {
    await api("POST", "/organizations/" + id + "/backfill/requeue", null);
    toast("已触发重新回填(幂等,后台执行)"); enterOrg(id, S.org.name);
  } catch (e) { toast(e.message); }
}
// v1 硬停(20-§4,风控):禁用该组织的 new-api 用户,近实时 403 全部令牌(有钱也停:欠费纠纷/风控);可解除。
function confirmHardStop(id) {
  dangerConfirm("确认硬停整个组织?", `<p>硬停作用于<b>整个客户组织</b>,不是单个员工。</p>
    <p>执行后,该组织全部 API Key 将近实时返回 403,平台管理操作也会被屏蔽。此操作用于欠费纠纷、风控等紧急止损场景,可由运营方解除。</p>`,
    "确认硬停组织", `doHardStop(${id},true)`);
}
async function doHardStop(id, stop) {
  try {
    await api("POST", "/organizations/" + id + (stop ? "/hard-stop" : "/hard-stop-release"), null);
    closeM(); toast(stop ? "已硬停(全部 key 已 403)" : "已解除硬停"); enterOrg(id, S.org.name);
  } catch (e) { toast(e.message); }
}
// 门B 重新导入(幂等):补齐导入失败/企业后来在 new-api 新建的令牌。
async function doReimport(id) {
  try {
    const d = await api("POST", "/organizations/" + id + "/import-tokens", null);
    toast(`重新导入完成:新导入 ${d.imported || 0} 个,已存在 ${d.skipped || 0} 个,失败 ${d.failed || 0} 个`); enterOrg(id, S.org.name);
  } catch (e) { toast(e.message); }
}
async function openTokenMappings(id) {
  S.orgTab = "tokens"; S.orgId = id;
  await renderTokenMappingsPanel(id);
}
async function renderTokenMappingsPanel(id) {
  const box = document.getElementById("orgTabBody");
  try {
    // B4(28):分页,不再一次拉全量(历史令牌多的大客户首屏慢)。
    const page = S.tokenMapPage || 1, size = 50;
    const d = await api("GET", "/organizations/" + id + "/token-mappings?page=" + page + "&page_size=" + size, null);
    const p = d.page || {};
    const total = p.total || (d.items || []).length;
    const maxPage = Math.max(1, Math.ceil(total / size));
    const rows = (d.items || []).map(x => {
      const key = x.key_masked || "-";
      const tokenName = x.token_name || "-";
      const tokenId = x.newapi_token_id || 0;
      return `<tr>
      <td><span class="cell-clip" title="${esc(x.display_name || x.login_email || "-")}">${esc(x.display_name || x.login_email || "-")}</span>${x.member_deleted ? ' <span class="mini">(已离职)</span>' : ""}</td>
      <td class="mini">${esc(memberStatusCN(x.member_status))}</td>
      <td><span class="cell-clip" title="${esc(tokenName)}">${esc(tokenName)}</span>${copyBtn(tokenName, "令牌名")}</td>
      <td class="mini key-copy"><span class="cell-clip" title="${esc(key)}">${esc(key)}</span></td>
      <td class="right" title="new-api 侧令牌的内部编号,排障时对照 new-api 后台用">${tokenId} ${copyBtn(String(tokenId), "token_id")}</td>
      <td title="换过 Key 会产生多条记录;“当前”=正在生效的那把">${x.is_current ? pill("当前", "ok") : pill("历史", "mut")}</td>
      <td class="mini">${esc(x.token_status || "-")}</td>
    </tr>`;
    }).join("");
    if (!box) return;
    box.innerHTML = `<div class="panel"><div class="ph">员工 - new-api 令牌映射</div><div class="pb">
      <div class="table-scroll"><table class="kvtable token-map-table"><colgroup><col class="tm-member"><col class="tm-status"><col class="tm-token"><col class="tm-key"><col class="tm-tokenid"><col class="tm-version"><col class="tm-state"></colgroup><tr><td class="k">成员</td><td class="k">成员状态</td><td class="k">令牌名</td><td class="k">Key</td><td class="right k" title="new-api 侧令牌的内部编号">token_id</td><td class="k" title="换过 Key 会产生多条;当前=正在生效">版本</td><td class="k">令牌状态</td></tr>${rows || `<tr><td colspan="7">${emptyState("◧", "暂无令牌映射", "开通员工后这里会出现员工与令牌的对应关系", "", "")}</td></tr>`}</table></div>
      <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 条 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goTokenMapPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goTokenMapPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
    </div></div>`;
  } catch (e) {
    // C1(28):失败渲染进 box,不再只 toast 留面板永久停在"加载中…"。
    if (box) box.innerHTML = `<div class="panel"><div class="pb">${emptyState("!", "加载失败", e.message || "请稍后重试", "重试", "renderTokenMappingsPanel(" + id + ")")}</div></div>`;
    toast(e.message);
  }
}
function goTokenMapPage(p) { S.tokenMapPage = Math.max(1, p); renderTokenMappingsPanel(S.orgId); }
async function openNewapiLogs(id) {
  S.orgTab = "logs"; S.orgId = id;
  S.logMirror = newLogMirrorState(id);
  await renderNewapiLogs();
}
function newLogMirrorState(orgId) {
  return { orgId, global: false, page: 1, size: 50, start: "", end: "", type: "", member: "", token: "", model: "", channel: "", group: "", request: "", org: "" };
}
function newGlobalLogState() {
  const st = newLogMirrorState(0);
  st.global = true;
  return st;
}
function dateToStartTS(v) {
  if (!v) return "";
  const n = Math.floor(new Date(v + "T00:00:00").getTime() / 1000);
  return Number.isFinite(n) ? String(n) : "";
}
function dateToEndTS(v) {
  if (!v) return "";
  const n = Math.floor(new Date(v + "T23:59:59").getTime() / 1000);
  return Number.isFinite(n) ? String(n) : "";
}
/* ---------- 使用日志:展开详情 + 计费过程(照抄 new-api,仅只读展示) ---------- */
// parseOther:安全解析镜像 other JSON;坏串/空串一律回退空对象(后端已保证低权 other 剥 admin_info/stream_status)。
function parseOther(s) { if (!s) return {}; try { const o = JSON.parse(s); return (o && typeof o === "object") ? o : {}; } catch (e) { return {}; } }
// fmtUsd:$ 金额,最多 6 位小数、去尾零(new-api 用 toFixed(6),此处去零更易读)。单价/算式结果已是美元,勿再除 500000。
function fmtUsd(n) { let s = Number(n || 0).toFixed(6).replace(/0+$/, "").replace(/\.$/, ""); return "$" + (s === "" ? "0" : s); }
// fmtDur:首字延迟 other.frt(毫秒)显示。
function fmtDur(ms) { ms = Number(ms) || 0; return ms >= 1000 ? (ms / 1000).toFixed(2) + "s" : Math.round(ms) + "ms"; }
// pickGroupRatio:专属倍率(user_group_ratio)优先,否则分组倍率(group_ratio),对齐 new-api getEffectiveRatio。
function pickGroupRatio(o) {
  const ugr = Number(o.user_group_ratio);
  if (Number.isFinite(ugr) && ugr > 0) return { ratio: ugr, label: "专属倍率" };
  const gr = Number(o.group_ratio);
  return { ratio: (Number.isFinite(gr) && gr > 0) ? gr : 1, label: "分组倍率" };
}
// billingProcess:复刻 new-api renderModelPrice 主力单模型算式(截图那种:输入/输出/缓存单价 + 算式 * 倍率 = 金额);
// 音频/阶梯/任务/Claude 长尾降级为直接展示已算好的花费(不重写算式,§6)。纯只读展示,不参与企业扣费。
function billingProcess(x, o, pt, ct) {
  const gr = pickGroupRatio(o);
  const note = `<div class="lx-note">new-api 计费过程，仅供参考，以实际扣费为准</div>`;
  const wrap = lines => `<div class="bill">${lines}${note}</div>`;
  if (o.billing_mode === "tiered_expr") {
    let expr = ""; try { expr = o.expr_b64 ? decodeURIComponent(escape(atob(o.expr_b64))) : ""; } catch (e) { expr = ""; }
    return wrap((expr ? `<div class="bill-line">${esc(expr)}</div>` : "") + `<div class="bill-line">阶梯计价，最终花费 ${money(x.quota || 0)}</div>`);
  }
  if (o.is_task || o.task_id != null) return wrap(`<div class="bill-line">任务计价，最终花费 ${money(x.quota || 0)}</div>`);
  if (o.ws || o.audio) return wrap(`<div class="bill-line">音频计价，最终花费 ${money(x.quota || 0)}</div>`);
  if (o.claude) return wrap(`<div class="bill-line">Claude 计价，最终花费 ${money(x.quota || 0)}</div>`);
  const modelPrice = (o.model_price == null) ? -1 : Number(o.model_price);
  if (modelPrice !== -1) {
    return wrap(`<div class="bill-line">按次：${fmtUsd(modelPrice)}</div>`
      + `<div class="bill-line">按次 ${fmtUsd(modelPrice)} * ${gr.label} ${gr.ratio} = ${fmtUsd(modelPrice * gr.ratio)}</div>`);
  }
  const mr = Number(o.model_ratio) || 0;
  const cr = (o.completion_ratio == null) ? 0 : Number(o.completion_ratio);
  const cacheRatio = (o.cache_ratio == null) ? 1 : Number(o.cache_ratio);
  const cacheTokens = Number(o.cache_tokens) || 0;
  const inP = mr * 2.0, outP = mr * 2.0 * cr, cacheP = inP * cacheRatio;
  const effIn = pt - cacheTokens + cacheTokens * cacheRatio;
  const price = (effIn / 1e6) * inP * gr.ratio + (ct / 1e6) * outP * gr.ratio;
  const lines = [
    `<div class="bill-line">输入价格：${fmtUsd(inP)} / 1M tokens</div>`,
    `<div class="bill-line">输出价格：${fmtUsd(outP)} / 1M tokens</div>`,
  ];
  if (cacheTokens > 0) lines.push(`<div class="bill-line">缓存读取价格：${fmtUsd(cacheP)} / 1M tokens</div>`);
  const inExpr = cacheTokens > 0
    ? `(输入 ${(pt - cacheTokens).toLocaleString()} tokens / 1M * ${fmtUsd(inP)} + 缓存 ${cacheTokens.toLocaleString()} tokens / 1M * ${fmtUsd(cacheP)}`
    : `(输入 ${pt.toLocaleString()} tokens / 1M * ${fmtUsd(inP)}`;
  const outExpr = `输出 ${ct.toLocaleString()} tokens / 1M * ${fmtUsd(outP)}) * ${gr.label} ${gr.ratio}`;
  lines.push(`<div class="bill-line">${inExpr} + ${outExpr} = ${fmtUsd(price)}</div>`);
  return wrap(lines.join(""));
}
// logExpand:一条日志的展开面板(Descriptions);字段全来自 other,渠道信息仅超管(后端已保证低权拿不到 channel/admin_info)。
function logExpand(x, o, pt, ct, isOperator) {
  const rows = [];
  const addT = (k, v) => { if (v == null || String(v) === "") return; rows.push(`<div class="lx-row"><div class="lx-k">${esc(k)}</div><div class="lx-v">${esc(v)}</div></div>`); };
  const addH = (k, v) => { if (!v) return; rows.push(`<div class="lx-row"><div class="lx-k">${esc(k)}</div><div class="lx-v">${v}</div></div>`); };
  const chId = Number(x.channel_id) || 0;
  if (isOperator && (chId > 0 || o.admin_info)) addT("渠道信息", `#${chId}${x.channel_name ? (" - " + x.channel_name) : ""}`);
  if (x.request_id) addT("Request ID", x.request_id);
  if (Number(o.cache_tokens) > 0) addT("缓存 Tokens", o.cache_tokens);
  if (Number(o.cache_creation_tokens) > 0) addT("缓存创建 Tokens", o.cache_creation_tokens);
  if (o.is_model_mapped && o.upstream_model_name) { addT("请求并计费模型", x.model_name); addT("实际模型", o.upstream_model_name); }
  if (Number(x.log_type) === 2) addH("计费过程", billingProcess(x, o, pt, ct));
  if (o.reasoning_effort) addT("Reasoning Effort", o.reasoning_effort);
  if (o.request_path) addT("请求路径", o.request_path);
  if (o.stream_status) addT("流状态", o.stream_status); // 后端只对超管保留;低权已剥
  if (x.content) addT("其他详情", x.content);
  return rows.length ? `<div class="log-desc">${rows.join("")}</div>` : `<div class="lx-empty">无更多详情</div>`;
}
function toggleLogRow(i) {
  const r = document.getElementById("lx_" + i), t = document.getElementById("tri_" + i);
  if (!r) return;
  const open = r.style.display === "none";
  r.style.display = open ? "" : "none";
  if (t) t.textContent = open ? "▾" : "▸";
}

async function renderNewapiLogs() {
  const st = S.logMirror || newLogMirrorState(S.orgId);
  const memberSelf = S.role === "member";
  const global = !!st.global;
  const orgScoped = !global && !memberSelf;
  const q = new URLSearchParams({ page: String(st.page || 1), page_size: String(st.size || 50) });
  const startTS = dateToStartTS(st.start), endTS = dateToEndTS(st.end);
  if (startTS) q.set("start_timestamp", startTS);
  if (endTS) q.set("end_timestamp", endTS);
  if (st.type !== "" && st.type != null) q.set("type", st.type);
  if (global && st.org) q.set("org_id", st.org.trim());
  if (!memberSelf && st.member) q.set("member_id", st.member);
  if (!memberSelf && st.token) q.set("token_name", st.token.trim());
  if (st.model) q.set("model_name", st.model.trim());
  if (global && st.channel) q.set("channel", st.channel.trim());
  if (!memberSelf && st.group) q.set("group", st.group.trim());
  if (st.request) q.set("request_id", st.request.trim());
  try {
    const path = global ? "/newapi-logs?" : ("/organizations/" + st.orgId + "/newapi-logs?");
    const d = await api("GET", path + q.toString(), null);
    const p = d.page || {};
    const total = p.total || 0, page = p.page || st.page || 1, size = p.page_size || st.size || 50;
    const maxPage = Math.max(1, Math.ceil(total / size));
    const isOperator = S.role === "operator";
    const colspan = memberSelf ? 9 : orgScoped ? 12 : 14;
    const rows = (d.items || []).map((x, i) => {
      const o = parseOther(x.other);
      const isErr = Number(x.log_type) === 5;
      const rid = x.request_id || "-";
      const token = x.token_name || "-";
      const channelID = Number(x.channel_id) || 0;
      const promptTokens = Number(x.prompt_tokens) || 0;
      const completionTokens = Number(x.completion_tokens) || 0;
      const seconds = Number(x.use_time) || 0;
      const frt = Number(o.frt) || 0;
      const content = x.content || "";
      const detailText = [rid !== "-" ? `Request ID ${rid}` : "", content].filter(Boolean).join(" · ") || "查看详情";
      const streamTag = x.is_stream ? `<span class="stream-badge">流</span>` : `<span class="stream-badge muted">非流</span>`;
      const memberName = x.member_name || (x.member_id ? `成员#${x.member_id}` : "-");
      const memberSub = x.member_email || (x.member_id ? `ID ${x.member_id}` : "");
      const base = {
        time: `<td class="mini">${fmtTs(x.log_ts)}</td>`,
        org: `<td><span class="cell-clip org-cell" title="${esc(x.org_name || ("组织#" + x.org_id))}">${esc(x.org_name || ("组织#" + x.org_id))}</span><span class="cell-sub">ID ${x.org_id}</span></td>`,
        type: `<td>${pill(logTypeName(x.log_type), isErr ? "bad" : Number(x.log_type) === 2 ? "ok" : "mut")}</td>`,
        member: `<td><span class="cell-clip member-cell" title="${esc(memberName)}">${esc(memberName)}</span>${memberSub ? `<span class="cell-sub" title="${esc(memberSub)}">${esc(memberSub)}</span>` : ""}</td>`,
        token: `<td class="mini"><span class="log-token" title="${esc(token)}">${esc(token)}</span>${copyBtn(token, "令牌名")}</td>`,
        model: `<td class="mini"><span class="model-badge" title="${esc(x.model_name || "-")}">${esc(x.model_name || "-")}</span>${x.model_name ? copyBtn(x.model_name, "模型名") : ""}</td>`,
        channel: `<td class="mini"><span class="channel-badge">${channelID || "-"}</span>${x.channel_name ? `<span class="cell-sub" title="${esc(x.channel_name)}">${esc(x.channel_name)}</span>` : ""}</td>`,
        group: `<td class="mini"><span class="group-badge" title="${esc(x.group_name || "-")}">${esc(x.group_name || "-")}</span></td>`,
        prompt: `<td class="right mini">${promptTokens.toLocaleString()}</td>`,
        completion: `<td class="right mini">${completionTokens.toLocaleString()}</td>`,
        cost: `<td class="right">${money(x.quota || 0)}</td>`,
        seconds: `<td class="right mini">${seconds ? `<span class="time-badge">${seconds}s</span>` : "-"}${frt > 0 ? `<span class="cell-sub">首字 ${fmtDur(frt)}</span>` : ""} ${streamTag}</td>`,
        ip: `<td class="mini"><span class="cell-clip" title="${esc(x.ip || "-")}">${esc(x.ip || "-")}</span>${x.ip ? copyBtn(x.ip, "IP") : ""}</td>`,
        detail: `<td class="mini"><span class="log-toggle" onclick="toggleLogRow(${i})"><span class="tri" id="tri_${i}">▸</span><span class="cell-clip logmsg" title="${esc(detailText)}">${esc(detailText)}</span></span>${rid !== "-" ? copyBtn(rid, "Request ID") : ""}</td>`,
      };
      let cells;
      if (memberSelf) cells = `${base.time}${base.type}${base.model}${base.seconds}${base.prompt}${base.completion}${base.cost}${base.ip}${base.detail}`;
      else if (orgScoped) cells = `${base.time}${base.member}${base.token}${base.group}${base.type}${base.model}${base.seconds}${base.prompt}${base.completion}${base.cost}${base.ip}${base.detail}`;
      else cells = `${base.time}${base.channel}${base.org}${base.member}${base.token}${base.group}${base.type}${base.model}${base.seconds}${base.prompt}${base.completion}${base.cost}${base.ip}${base.detail}`;
      const xrow = `<tr class="log-xrow" id="lx_${i}" style="display:none"><td colspan="${colspan}">${logExpand(x, o, promptTokens, completionTokens, isOperator)}</td></tr>`;
      return `<tr class="log-mainrow">${cells}</tr>${xrow}`;
    }).join("");
    const typeOptions = (memberSelf ? [2, 5] : [0, 1, 2, 3, 4, 5, 6])
      .map(v => `<option value="${v}"${String(st.type) === String(v) ? " selected" : ""}>${esc(logTypeName(v))}</option>`).join("");
    const tableCls = memberSelf ? "member-log-table" : orgScoped ? "org-log-table" : "global-log-table";
    const colgroup = memberSelf
      ? `<colgroup><col class="c-time"><col class="c-type"><col class="c-model"><col class="c-timeuse"><col class="c-prompt"><col class="c-completion"><col class="c-quota"><col class="c-ip"><col class="c-detail"></colgroup>`
      : orgScoped
        ? `<colgroup><col class="c-time"><col class="c-member"><col class="c-token"><col class="c-group"><col class="c-type"><col class="c-model"><col class="c-timeuse"><col class="c-prompt"><col class="c-completion"><col class="c-quota"><col class="c-ip"><col class="c-detail"></colgroup>`
        : `<colgroup><col class="c-time"><col class="c-channel"><col class="c-org"><col class="c-member"><col class="c-token"><col class="c-group"><col class="c-type"><col class="c-model"><col class="c-timeuse"><col class="c-prompt"><col class="c-completion"><col class="c-quota"><col class="c-ip"><col class="c-detail"></colgroup>`;
    const header = memberSelf
      ? `<tr><td class="k">时间</td><td class="k">类型</td><td class="k">模型</td><td class="right k">耗时</td><td class="right k">输入</td><td class="right k">输出</td><td class="right k">费用</td><td class="k">IP</td><td class="k">详情</td></tr>`
      : orgScoped
        ? `<tr><td class="k">时间</td><td class="k">成员</td><td class="k">令牌</td><td class="k">模型分组</td><td class="k">类型</td><td class="k">模型</td><td class="right k">耗时</td><td class="right k">输入</td><td class="right k">输出</td><td class="right k">费用</td><td class="k">IP</td><td class="k">详情</td></tr>`
        : `<tr><td class="k">时间</td><td class="k">渠道</td><td class="k">客户组织</td><td class="k">成员</td><td class="k">令牌</td><td class="k">分组</td><td class="k">类型</td><td class="k">模型</td><td class="right k">耗时</td><td class="right k">输入</td><td class="right k">输出</td><td class="right k">费用</td><td class="k">IP</td><td class="k">详情</td></tr>`;
    const emptyColspan = memberSelf ? 9 : orgScoped ? 12 : 14;
    const body = `<div class="panel"><div class="ph">${memberSelf ? "使用日志" : "使用日志"}</div><div class="pb">
       <div class="toolbar log-filter" style="margin-bottom:10px;gap:8px;flex-wrap:wrap">
        <input id="nl_start" class="fsel" type="date" value="${esc(st.start || "")}" title="开始日期">
        <input id="nl_end" class="fsel" type="date" value="${esc(st.end || "")}" title="结束日期">
        ${global ? `<input id="nl_org" class="fsel" value="${esc(st.org || "")}" placeholder="组织 ID">` : ""}
        <input id="nl_model" class="fsel" value="${esc(st.model || "")}" placeholder="模型名">
        ${memberSelf ? "" : `<input id="nl_group" class="fsel" value="${esc(st.group || "")}" placeholder="模型分组">`}
        <select class="fsel" id="nl_type"><option value="">全部类型</option>${typeOptions}</select>
        ${memberSelf ? "" : `<input id="nl_token" class="fsel" value="${esc(st.token || "")}" placeholder="令牌名">
        <input id="nl_member" class="fsel" value="${esc(st.member || "")}" placeholder="member_id">`}
        ${global ? `<input id="nl_channel" class="fsel" value="${esc(st.channel || "")}" placeholder="渠道 ID">` : ""}
        <input id="nl_req" class="fsel req-filter" value="${esc(st.request || "")}" placeholder="Request ID">
        <button class="btn sm" onclick="applyNewapiLogFilter()">筛选</button>
        <button class="btn sm" onclick="resetNewapiLogFilter()">重置</button>
       </div>
       <div class="table-scroll"><table class="kvtable log-table ${tableCls}">${colgroup}${header}${rows || `<tr><td colspan="${emptyColspan}" class="empty">暂无日志</td></tr>`}</table></div>
       <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 条 · 第 ${page}/${maxPage} 页</span>
        <button onclick="gotoNewapiLogPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="gotoNewapiLogPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
      </div></div>`;
    const box = document.getElementById("orgTabBody");
    if (box) box.innerHTML = body;
    else modal("使用日志", body, `<button class="btn pri" onclick="closeM()">关闭</button>`);
  } catch (e) { toast(e.message); }
}
function applyNewapiLogFilter() {
  if (!S.logMirror) return;
  S.logMirror.start = val("nl_start");
  S.logMirror.end = val("nl_end");
  S.logMirror.type = val("nl_type");
  S.logMirror.org = S.logMirror.global ? val("nl_org").trim() : "";
  S.logMirror.member = S.role === "member" ? "" : val("nl_member").trim();
  S.logMirror.token = S.role === "member" ? "" : val("nl_token").trim();
  S.logMirror.model = val("nl_model").trim();
  S.logMirror.channel = S.logMirror.global ? val("nl_channel").trim() : "";
  S.logMirror.group = S.role === "member" ? "" : val("nl_group").trim();
  S.logMirror.request = val("nl_req").trim();
  S.logMirror.page = 1;
  renderNewapiLogs();
}
function resetNewapiLogFilter() {
  if (!S.logMirror) return;
  S.logMirror = S.logMirror.global ? newGlobalLogState() : newLogMirrorState(S.logMirror.orgId || S.orgId);
  renderNewapiLogs();
}
function gotoNewapiLogPage(page) {
  if (!S.logMirror || page < 1) return;
  S.logMirror.page = page;
  renderNewapiLogs();
}
VIEWS.logs = async () => {
  if (!S.logMirror || S.logMirror.orgId !== S.orgId) {
    S.logMirror = newLogMirrorState(S.orgId);
  }
  const self = S.role === "member";
  return head(self ? "使用日志" : "使用日志", self ? "查看自己的 API Key 调用记录,按模型和 Request ID 排障" : "查看本组织成员调用记录,按成员、令牌、模型和 Request ID 排障")
    + `<div id="orgTabBody"><div class="empty">加载中…</div></div>`;
};
VIEWS_AFTER.logs = () => { renderNewapiLogs(); };
VIEWS.oplogs = async () => {
  if (!S.logMirror || !S.logMirror.global) S.logMirror = newGlobalLogState();
  return head("使用日志", "运营方全局日志视图,字段和筛选口径对齐 new-api 使用日志,用于跨客户排障")
    + `<div id="orgTabBody"><div class="empty">加载中…</div></div>`;
};
VIEWS_AFTER.oplogs = () => { renderNewapiLogs(); };
function logTypeName(t) {
  return ({ 0: "未知", 1: "充值", 2: "消费", 3: "管理", 4: "系统", 5: "错误", 6: "退款" })[Number(t)] || ("类型 " + (t || 0));
}
async function openDiscount(id) {
  let cur = {}; try { cur = await api("GET", "/organizations/" + id + "/pricing", null); } catch (e) {}
  const up = cur.upstream_special_ratio || {};
  const curTxt = Object.keys(up).length ? Object.entries(up).map(([g, r]) => `${g}: ${r}`).join(" · ") : "无";
  modal("配置客户折扣", `<div class="fld"><label>模式</label><select id="dc_m">
      <option value="total">整体折扣(所有分组)</option><option value="per_group">按分组折扣</option><option value="none">取消折扣</option></select></div>
    <div class="fld"><label>折扣率(0.9 = 9 折)</label><input id="dc_p" type="number" step="0.05" placeholder="0.9"></div>
    <div class="fld"><label>令牌分组(逗号分隔,整体留空=default)</label><input id="dc_g" placeholder="default"></div>
    <div class="note">单向写 new-api 分组特殊倍率(绝对值=基础倍率×折扣率);客户侧只读。当前 new-api 实际值:${esc(curTxt)}</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doDiscount(${id})">下发</button>`);
}
async function doDiscount(id) {
  try {
    const body = { mode: val("dc_m") };
    const p = parseFloat(val("dc_p")); if (p > 0) body.discount_pct = p;
    const gs = val("dc_g").split(",").map(s => s.trim()).filter(Boolean); if (gs.length) body.token_groups = gs;
    await api("PUT", "/organizations/" + id + "/pricing", body); closeM(); toast("折扣已下发(写入 new-api)");
  } catch (e) { toast(e.message); }
}
function openSupport(id) {
  S.orgTab = "support"; S.orgId = id;
  const box = document.getElementById("orgTabBody");
  if (box) box.innerHTML = renderSupportPanel(id);
}
function renderSupportPanel(id) {
  return `<div class="panel"><div class="ph">运营方支持会话</div><div class="pb">
    <div class="fld"><label>支持类型</label><select id="sp_s"><option value="readonly">只读支持(看不能改)</option><option value="assist">协助模式(可代客户改配置,涉资金/密钥操作会被系统拦截)</option></select></div>
    <div class="note">进入支持会话后,你将以组织管理员身份在该组织操作,受后端闸约束,每步双身份留痕。</div>
    <div class="toolbar" style="margin-top:12px"><button class="btn pri" onclick="doSupport(${id})">进入支持态</button></div>
  </div></div>`;
}
async function doSupport(id) {
  try {
    const scope = val("sp_s");
    const d = await api("POST", "/organizations/" + id + "/support-sessions", { scope, grant_type: scope === "assist" ? "authorized" : "", ttl_seconds: 7200, reason: "运营支持" });
    // A4:记住运营方 token(仅内存,退出/刷新还原);支持 token 只切内存、**不写 localStorage**,不覆盖运营方身份。
    S.opToken = S.token; S.token = d.token; S.supportOrgId = id;
    closeM(); toast("已进入" + (scope === "readonly" ? "只读" : "协助") + "支持态"); await boot();
  } catch (e) { toast(e.message); }
}

/* ===================== 组织管理员 / 团队负责人 ===================== */
VIEWS.dash = async () => {
  const id = S.orgId;
  // 门B 回填历史常远早于默认窗(近7天)→ 首次进入该组织若有更早的已回填历史,自动切"全部历史",
  // 免客户第一眼以为没数据(24 验收·产品建议)。仅首次自动一次;之后用户手动改窗不再被覆盖。
  if (S._dashWideFor !== id) {
    S._dashWideFor = id;
    const bf = await api("GET", "/organizations/" + id + "/backfill", null).catch(() => ({ status: "none" }));
    if (bf.status === "done" && bf.earliest_seen_ts && (Date.now() / 1000 - bf.earliest_seen_ts) > S.win * 3600) {
      S.win = WIN_ALL;
    }
  }
  const win = S.win, wl = winLabel(win);
  const reqs = [api("GET", "/organizations/" + id + "/usage?since_hours=" + win, null)];
  // v1 M5(20-§3):客户余额=读求和(实时读 new-api 池子,available_quota);订阅组织显示"订阅计费"。
  reqs.push(api("GET", "/organizations/" + id + "/balance", null).catch(() => ({ __err: true })));
  // M2:用量趋势(折线图);失败不拖垮看板,降级空序列。
  reqs.push(api("GET", "/organizations/" + id + "/usage/timeseries?since_hours=" + win + "&granularity=" + S.gran, null).catch(() => ({ series: [] })));
  const [usage, bal, ts] = await Promise.all(reqs);
  const series = (ts && ts.series) || [];
  // #6:模型/员工都全量渲染 + 各自搜索框 + 滚动容器(成员超 8 人也找得到),不再 slice 截断。
  const mods = usage.by_model || [];
  const maxv = Math.max(1, ...mods.map(b => b.consumed_quota));
  const bars = mods.map(b => `<div class="bar" data-q="${esc((b.key || "").toLowerCase())}"><span class="nm">${esc(b.key)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / maxv * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`).join("") || `<div class="empty">${wl}暂无用量</div>`;
  // 改动④ + #5:员工排行(降序);行可点 → 下钻该员工按模型明细(后端已有 /members/{id}/usage)。
  const mem = usage.by_member || [];
  const maxm = Math.max(1, ...mem.map(b => b.consumed_quota));
  const mbars = mem.map(b => {
    const nm = b.label || ("用户#" + b.key);
    const attrs = b.member_id ? `class="bar lk" onclick="openMemberUsage(${b.member_id},'${jsstr(nm)}')" title="查看该员工按模型明细"` : `class="bar"`;
    return `<div ${attrs} data-q="${esc(nm.toLowerCase())}"><span class="nm">${esc(nm)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / maxm * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`;
  }).join("") || `<div class="empty">${wl}暂无用量</div>`;
  // F3:按团队用量排行(含"未分组"桶);行可点 → 下钻该团队成员/模型明细。
  const tms = usage.by_team || [];
  const maxt = Math.max(1, ...tms.map(b => b.consumed_quota));
  const tbars = tms.map(b => {
    const nm = b.label || ("团队#" + b.key);
    const tid = Number(b.key); // C8:数字守卫——"未分组"桶(key=0/非数字)不可点,避免 onclick 语法错(对齐成员条)
    const attrs = tid > 0 ? `class="bar lk" onclick="openTeamUsage(${tid},'${jsstr(nm)}')" title="查看该团队成员/模型明细"` : `class="bar"`;
    return `<div ${attrs} data-q="${esc(nm.toLowerCase())}"><span class="nm">${esc(nm)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / maxt * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`;
  }).join("") || `<div class="empty">${wl}暂无用量(或未建团队)</div>`;
  // v1 M5:可用余额=读求和(实时);订阅计费组织不显示数字(池子不反映其消费,显示会误导)。
  const balCards = bal.billing_kind === "subscription"
    ? kpi("可用余额", "订阅计费", "该组织按订阅计费,无钱包余额")
    : kpi("可用余额", balMoney(bal), "实时读取(充值请联系运营方)");
  const srch = (iid, cid, ph) => `<input id="${iid}" placeholder="${ph}" oninput="filterEls('${iid}','${cid}')" style="float:right;width:150px;padding:2px 8px;font-size:12px">`;
  // 数据安全承诺条(仅客户 org_admin);文案站得住:MVP 下成员 key 员工自助建、平台不经手,观测不扣款。
  const safebar = S.role === "org_admin" ? `<div class="safebar">本平台仅统计您的用量,不接触您的 API 密钥;观测期不扣款,数据仅用于用量统计。</div>` : "";
  // 新组织上手清单:v1 无全期消耗字段(company_balance 降级),改用「当前窗口零消耗且无任何用量条目」近似判断。
  const showOnboard = S.role === "org_admin" && (usage.total_quota || 0) === 0 && mem.length === 0;
  const onboard = showOnboard ? `<div class="panel onboard"><div class="ph">快速上手</div><div class="pb">
    <div class="ob-row"><span class="ob-n">1</span><div><b>开通员工</b><div class="mini">在「成员」开通员工账号,交付登录凭证</div></div><button class="btn sm pri" onclick="go('members')">去开通</button></div>
    <div class="ob-row"><span class="ob-n">2</span><div><b>(可选)建团队</b><div class="mini">想按团队看用量就先建团队,再把员工归入</div></div><button class="btn sm" onclick="go('teams')">建团队</button></div>
    <div class="ob-row"><span class="ob-n">3</span><div><b>查看用量</b><div class="mini">员工开始调用后,这里会显示按团队 / 员工 / 模型的用量</div></div></div>
  </div></div>` : "";
  return head("概览", "公司可用余额 + 用量")
    + safebar
    + `<div class="toolbar"><div class="spacer"></div><button class="btn" onclick="downloadUsageCsv()">导出CSV(员工名·美元)</button><span class="mini">时间窗</span>${winSelect()}</div>
    <div class="cards">
      ${kpi(wl + "消耗", money(usage.total_quota), "按实际调用量统计")}${balCards}
    </div>
    ${onboard}
    <div class="panel"><div class="ph">用量趋势(${wl})<span class="mini" style="font-weight:400;float:right">粒度 ${granSelect()}</span></div><div class="pb" id="trendBody">${lineChart(series)}</div></div>
    <div class="panel"><div class="ph">按团队用量(${wl})· 点团队下钻 <span class="mini" style="font-weight:400">团队用量按成员当前归属聚合${srch("teamSearch", "teamBars", "搜团队…")}</span></div><div class="pb" id="teamBars" style="max-height:300px;overflow:auto">${tbars}</div></div>
    <div class="panel"><div class="ph">员工用量排行(${wl})· 点员工看明细${srch("memSearch", "memBars", "搜员工…")}</div><div class="pb" id="memBars" style="max-height:340px;overflow:auto">${mbars}</div></div>
    <div class="panel"><div class="ph">按模型用量(${wl})${srch("modSearch", "modBars", "搜模型…")}</div><div class="pb" id="modBars" style="max-height:340px;overflow:auto">${bars}</div></div>`;
};
VIEWS.members = async () => {
  const id = S.orgId;
  const showOff = !!S.membersShowOffboarded;
  const mf = S.memberFilter || { q: "", team: "", status: "" };
  const mq = new URLSearchParams({ page: String(S.membersPage || 1), page_size: "50" });
  if (mf.q) mq.set("q", mf.q);
  if (mf.team) mq.set("team_id", mf.team);
  if (mf.status) mq.set("status", mf.status);
  const reqs = [
    api("GET", "/organizations/" + id + "/members?" + mq.toString(), null),
    api("GET", "/organizations/" + id + "/teams", null).catch(() => []),
  ];
  if (showOff) reqs.push(api("GET", "/organizations/" + id + "/members/offboarded?page=1&page_size=50", null).catch(() => ({ list: [] })));
  const res = await Promise.all(reqs);
  const d = res[0], teams = res[1], off = showOff ? (res[2] || { list: [] }) : { list: [] };
  S.teamsCache = teams || []; // 供调团队弹窗
  const tmap = {}; (teams || []).forEach(t => tmap[t.id] = t.name);
  // 管理类账号(运营方/组织管理员)不进"成员"列表(M7);只列 API 使用成员。
  const rows = (d.list || []).filter(m => m.role !== "org_admin" && m.role !== "operator").map(m => `<tr>
    <td>${esc(m.display_name || m.login_email)}<div class="mini">${esc(m.login_email)}</div></td>
    <td>${m.team_id ? esc(tmap[m.team_id] || ("团队#" + m.team_id)) : '<span class="mini">未分组</span>'}</td>
    <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td>
    <td class="mini"><span class="cell-clip" title="${esc(m.key_masked || "-")}">${esc(m.key_masked || "-")}</span>${revealKeyBtn(m.id, m.display_name || m.login_email)}</td>
    <td class="right table-actions">
      ${S.mvp ? "" : `<span class="btn sm" onclick="openAdjust(${m.id})">调额</span>`}
      <span class="btn sm" onclick="openMemberLogs(${m.id},'${jsstr(m.display_name || m.login_email)}')">日志</span>
      <span class="btn sm" onclick="toggleMember(${m.id},${m.status !== "active"})">${m.status === "active" ? "禁用" : "启用"}</span>
      <span class="btn sm" onclick="openMemberMore(${m.id},'${jsstr(m.display_name || m.login_email)}','${jsstr(m.display_name || "")}',${m.team_id || 0})">更多</span>
    </td></tr>`).join("");
  const offRows = showOff ? (off.list || []).map(m => `<tr>
    <td>${esc(m.display_name || m.login_email)}<div class="mini">${esc(m.login_email)}</div></td>
    <td><span class="tag">已离职</span></td>
    <td class="mini">key 已失效</td>
    <td class="right"><span class="btn sm" onclick="doRestoreMember(${m.id},'${jsstr(m.display_name || m.login_email)}')">恢复入职</span></td>
  </tr>`).join("") : "";
  // A6:成员分页(page_size=50)——>50 成员时显示总数 + 上/下页,确保全量可达(客户端搜索只筛已加载行,不够)。
  const memTotal = (d.pagination || {}).total || (d.list || []).length;
  const memPage = S.membersPage || 1, memPages = Math.max(1, Math.ceil(memTotal / 50));
  const memPager = memTotal > 50
    ? `<div class="toolbar mini">共 ${memTotal} 名成员 · 第 ${memPage}/${memPages} 页
        <button class="btn sm" ${memPage <= 1 ? "disabled" : ""} onclick="goMembersPage(-1)">上一页</button>
        <button class="btn sm" ${memPage >= memPages ? "disabled" : ""} onclick="goMembersPage(1)">下一页</button></div>`
    : "";
  const teamOpts = (teams || []).filter(t => t.status !== "archived").map(t => `<option value="${t.id}"${String(mf.team) === String(t.id) ? " selected" : ""}>${esc(t.name)}</option>`).join("");
  return head(S.role === "team_leader" ? "团队成员" : "成员", "员工即企业 API Key 使用主体;禁用=暂停当前 Key,启用即通;离职=Key 失效并转离职列表")
    + `<div class="toolbar log-filter" style="gap:8px;flex-wrap:wrap">
      <input id="mf_q" class="fsel" value="${esc(mf.q || "")}" placeholder="搜索姓名 / 登录名 / Key">
      <select id="mf_team" class="fsel"><option value="">全部团队</option>${teamOpts}</select>
      <select id="mf_status" class="fsel"><option value="">全部状态</option><option value="active"${mf.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${mf.status === "disabled" ? " selected" : ""}>禁用</option></select>
      <button class="btn sm" onclick="applyMemberFilter()">筛选</button>
      <button class="btn sm" onclick="resetMemberFilter()">重置</button>
      <div class="spacer"></div>
      <label class="mini" style="margin-left:12px;cursor:pointer"><input type="checkbox" ${showOff ? "checked" : ""} onclick="S.membersShowOffboarded=this.checked;renderView()"> 显示离职</label>
      ${S.mvp ? "" : `<button class="btn" onclick="openBulk()">批量导入</button>`}<button class="btn pri" onclick="openAddMember()">+ 开通成员</button></div>
    <div class="panel"><div class="table-scroll"><table class="kvtable member-table"><colgroup><col class="m-name"><col class="m-team"><col class="m-status"><col class="m-key"><col class="m-actions"></colgroup><thead><tr><th>成员</th><th>团队</th><th>状态</th><th>Key(脱敏)</th><th></th></tr></thead>
    <tbody>${rows || '<tr><td colspan=5>' + emptyState("☷", "还没有成员", "开通第一位员工,系统会生成登录凭证交付给他", "+ 开通成员", "openAddMember()") + '</td></tr>'}</tbody></table></div>${memPager}</div>`
    + (showOff ? `<div class="panel"><div class="ph">离职成员(软删·可恢复)</div><div class="pb"><table><tbody>${offRows || '<tr><td class="empty">暂无离职成员</td></tr>'}</tbody></table></div></div>` : "");
};
function applyMemberFilter() {
  S.memberFilter = { q: val("mf_q").trim(), team: val("mf_team"), status: val("mf_status") };
  S.membersPage = 1;
  renderView();
}
function resetMemberFilter() {
  S.memberFilter = { q: "", team: "", status: "" };
  S.membersPage = 1;
  renderView();
}
function goMembersPage(delta) { S.membersPage = Math.max(1, (S.membersPage || 1) + delta); renderView(); }
VIEWS.employees = async () => {
  const f = S.employeeFilter || { q: "", org: "", status: "", team: "" };
  const page = S.employeePage || 1;
  const q = new URLSearchParams({ page: String(page), page_size: "50" });
  if (f.q) q.set("q", f.q);
  if (f.org) q.set("org_id", f.org);
  if (f.status) q.set("status", f.status);
  if (f.team) q.set("team_id", f.team);
  const d = await api("GET", "/members?" + q.toString(), null);
  const list = d.list || [];
  const total = (d.pagination || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(total / 50));
  // B3(28):管理类角色过滤已下沉后端 SQL,total 与可见行一致,前端不再二次过滤(否则计数/翻页错位)。
  const rows = list.map(m => {
    const key = m.key_masked || "-";
    const limit = m.monthly_limit_quota == null ? "未设置" : money(m.monthly_limit_quota);
    const remain = m.remaining_quota == null ? "未设置" : money(m.remaining_quota);
    return `<tr>
      <td><span class="cell-clip org-cell" title="${esc(m.org_name)}">${esc(m.org_name)}</span><span class="cell-sub">ID ${m.org_id}</span></td>
      <td><span class="cell-clip" title="${esc(m.display_name || m.login_email)}">${esc(m.display_name || m.login_email)}</span><span class="cell-sub">${esc(m.login_email)}</span></td>
      <td><span class="cell-clip" title="${esc(m.team_name || "未分组")}">${esc(m.team_name || "未分组")}</span></td>
      <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td>
      <td class="mini"><span class="cell-clip" title="${esc(key)}">${esc(key)}</span></td>
      <td class="mini">${m.newapi_token_id || "-"}</td>
      <td><span class="cell-clip" title="${esc(m.newapi_group || "-")}">${esc(m.newapi_group || "-")}</span></td>
      <td class="right">${money(m.consumed_quota || 0)}</td>
      <td class="right mini">${limit}</td>
      <td class="right mini">${remain}</td>
      <td class="right table-actions">
        <span class="btn sm" onclick="S.logMirror=newGlobalLogState();S.logMirror.member='${m.id}';go('oplogs')">日志</span>
        <span class="btn sm" onclick="enterOrg(${m.org_id},'${jsstr(m.org_name)}')">进组织</span>
      </td>
    </tr>`;
  }).join("");
  return head("员工管理", "运营方全局员工和 API Key 视图,用于跨客户排障和维护")
    + `<div class="toolbar log-filter" style="gap:8px;flex-wrap:wrap">
      <input id="ef_q" class="fsel" value="${esc(f.q || "")}" placeholder="姓名 / 登录名 / Key / 公司">
      <input id="ef_org" class="fsel" value="${esc(f.org || "")}" placeholder="组织 ID">
      <input id="ef_team" class="fsel" value="${esc(f.team || "")}" placeholder="团队 ID">
      <select id="ef_status" class="fsel"><option value="">全部状态</option><option value="active"${f.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${f.status === "disabled" ? " selected" : ""}>禁用</option><option value="provisioning"${f.status === "provisioning" ? " selected" : ""}>开通中</option></select>
      <button class="btn sm" onclick="applyEmployeeFilter()">筛选</button>
      <button class="btn sm" onclick="resetEmployeeFilter()">重置</button>
    </div>
    <div class="panel"><div class="ph">全局员工列表</div><div class="pb">
      <div class="table-scroll"><table class="kvtable employee-table"><colgroup><col class="e-org"><col class="e-member"><col class="e-team"><col class="e-status"><col class="e-key"><col class="e-tokenid"><col class="e-group"><col class="e-used"><col class="e-limit"><col class="e-remain"><col class="e-actions"></colgroup><tr><td class="k">客户组织</td><td class="k">员工</td><td class="k">部门/团队</td><td class="k">状态</td><td class="k">Key(脱敏)</td><td class="k" title="new-api 侧令牌的内部编号,排障时对照 new-api 后台用">token_id</td><td class="k">模型分组</td><td class="right k">已用额度</td><td class="right k">月额度</td><td class="right k">剩余额度</td><td></td></tr>${rows || `<tr><td colspan="11">${emptyState("☷", "还没有员工", "各客户组织开通员工后会汇总在这里", "", "")}</td></tr>`}</table></div>
      <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 名 · 第 ${page}/${maxPage} 页</span>
        <button onclick="gotoEmployeePage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="gotoEmployeePage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
    </div></div>`;
};
function applyEmployeeFilter() {
  S.employeeFilter = { q: val("ef_q").trim(), org: val("ef_org").trim(), team: val("ef_team").trim(), status: val("ef_status") };
  S.employeePage = 1;
  renderView();
}
function resetEmployeeFilter() {
  S.employeeFilter = { q: "", org: "", team: "", status: "" };
  S.employeePage = 1;
  renderView();
}
function gotoEmployeePage(page) {
  if (page < 1) return;
  S.employeePage = page;
  renderView();
}
function openMemberLogs(mid, name) {
  S.logMirror = newLogMirrorState(S.orgId);
  S.logMirror.member = String(mid);
  S.logMirror.page = 1;
  if (S.role === "operator" && document.getElementById("orgTabBody")) {
    S.orgTab = "logs";
    switchOrgTab("logs");
    return;
  }
  toast("已筛选成员日志:" + name);
  go("logs");
}
function openMemberMore(mid, name, curName, curTid) {
  modal("成员更多操作 · " + name, `<div class="action-list compact-actions">
    <div class="action-row"><div><b>修改显示名</b><div class="mini">用于报表、成员列表和排障识别。</div></div><button class="btn sm" onclick="openRename(${mid},'${jsstr(curName)}')">改名</button></div>
    <div class="action-row"><div><b>调整团队</b><div class="mini">影响团队维度用量统计口径。</div></div><button class="btn sm" onclick="openChangeTeam(${mid},${curTid || 0})">调团队</button></div>
    <div class="action-row danger-line"><div><b>离职</b><div class="mini">删除 API Key 并转入离职列表。临时停用请用“禁用”。</div></div><button class="btn sm danger" onclick="doOffboard(${mid},'${jsstr(name)}')">离职</button></div>
  </div>`, `<button class="btn pri" onclick="closeM()">关闭</button>`);
}
function openChangeTeam(mid, curTid) {
  const topts = (S.teamsCache || []).filter(t => t.status !== "archived").map(t => `<option value="${t.id}" ${t.id === curTid ? "selected" : ""}>${esc(t.name)}</option>`).join("");
  if (!topts) { toast("请先在「团队」页建团队"); return; }
  modal("调整团队", `<div class="fld"><label>团队</label><select id="ct_team">${topts}</select></div>
    <div class="note">改团队后该成员用量在团队看板按新团队归属(口径:按成员当前归属聚合)。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doChangeTeam(${mid})">保存</button>`);
}
async function doChangeTeam(mid) {
  try { await api("PATCH", "/members/" + mid, { team_id: parseInt(val("ct_team")) }); closeM(); toast("已调整团队"); renderView(); }
  catch (e) { toast(e.message); }
}
async function openAddMember() {
  let tiers = [], teams = [];
  try { tiers = (await api("GET", "/organizations/" + S.orgId + "/tiers", null)) || []; } catch (e) {}
  try { teams = (await api("GET", "/organizations/" + S.orgId + "/teams", null)) || []; } catch (e) {}
  const opts = tiers.map(t => `<option value="${t.id}">${esc(t.name)}${t.is_default ? "(默认)" : ""}</option>`).join("");
  const topts = (teams || []).filter(t => t.status !== "archived").map(t => `<option value="${t.id}">${esc(t.name)}</option>`).join("");
  modal("开通成员", `<div class="fld"><label>姓名</label><input id="am_n" placeholder="钱晨"></div>
    <div class="fld"><label>登录名(真实邮箱/用户名,可选)</label><input id="am_e" placeholder="留空则自动生成"></div>
    <div class="fld"><label>团队(可选)</label><select id="am_team"><option value="">未分组</option>${topts}</select></div>
    <div class="fld"><label>可用模型档位</label><select id="am_t">${opts || `<option value="">(先建档位)</option>`}</select></div>
    <div class="note">登录名留空将自动生成。系统将为该员工建账号、代发 API Key、下发可用模型档位;登录邮箱与初始密码开通后回显一次。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doAddMember()">开通成员</button>`);
}
async function doAddMember() {
  try {
    const body = { name: val("am_n") };
    const em = val("am_e").trim(); if (em) body.email = em; // T11:自定义登录名,留空后端 fallback
    const t = val("am_t"); if (t) body.tier_id = parseInt(t);
    const tm = val("am_team"); if (tm) body.team_id = parseInt(tm); // F2-1:开通时选团队(留空=未分组)
    const d = await api("POST", "/organizations/" + S.orgId + "/members", body);
    const keyRow = d.api_key
      ? `<tr><td class="k">API Key</td><td><b>${esc(d.api_key)}</b> <span class="lk" onclick="navigator.clipboard&&navigator.clipboard.writeText('${jsstr(d.api_key)}');toast('已复制')">复制</span></td></tr>`
      : `<tr><td class="k">API Key</td><td class="mini">已代发,员工登录后在“我的 API Key”查看并复制</td></tr>`; // A1:自助建 key 已下线,key 全程平台代发
    modal("已开通 · 交付登录凭证", `<div class="note">以下凭证仅此一次显示,请交付员工本人,首次登录后请改密:</div>
      <table class="kvtable">
        <tr><td class="k">登录邮箱</td><td>${esc(d.login_email)}</td></tr>
        <tr><td class="k">初始密码</td><td><b>${esc(d.initial_password)}</b></td></tr>
        ${keyRow}
      </table>`,
      `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
async function openBulk() {
  let tiers = []; try { tiers = (await api("GET", "/organizations/" + S.orgId + "/tiers", null)) || []; } catch (e) {}
  const opts = tiers.map(t => `<option value="${t.id}">${esc(t.name)}${t.is_default ? "(默认)" : ""}</option>`).join("");
  modal("批量导入成员", `<div class="fld"><label>姓名(每行一个)</label><textarea id="bk_t" rows="6" placeholder="张三&#10;李四&#10;王五"></textarea></div>
    <div class="fld"><label>统一档位</label><select id="bk_tier">${opts}</select></div>
    <div class="note">逐个限速代发 key,逐行返回结果;同批重名跳过;部分失败不回滚已成功行。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doBulk()">导入</button>`);
}
async function doBulk() {
  try {
    const names = val("bk_t").split("\n").map(s => s.trim()).filter(Boolean);
    const t = val("bk_tier"); const body = { names }; if (t) body.tier_id = parseInt(t);
    const d = await api("POST", "/organizations/" + S.orgId + "/members:bulk", body);
    const rows = (d.results || []).map(r => `<tr><td>${esc(r.name)}</td><td>${r.ok ? pill("成功", "ok") : pill("失败", "bad") + " " + esc(r.error || "")}</td></tr>`).join("");
    modal("批量导入结果", `<div class="note">成功 ${d.success} · 失败 ${d.failed}</div><table>${rows}</table>`, `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
function openAdjust(mid) {
  modal("临时调额", `<div class="fld"><label>调整量(美元,可负)</label><input id="aj_a" type="number" placeholder="10"></div>
    <div class="fld"><label>生效时长</label><select id="aj_d"><option value="today">今日</option><option value="3d">3 天</option><option value="week">本周</option></select></div>
    <div class="fld"><label>原因</label><input id="aj_r" placeholder="赶项目"></div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doAdjust(${mid})">确认下发</button>`);
}
async function doAdjust(mid) {
  try {
    const usd = parseFloat(val("aj_a")) || 0;
    await api("POST", "/members/" + mid + "/quota:adjust", { delta_quota: Math.round(usd * 500000), duration: val("aj_d"), reason: val("aj_r") });
    closeM(); toast("已下发(到期自动回退)"); renderView();
  } catch (e) { toast(e.message); }
}
function openRename(mid, cur) {
  modal("修改成员显示名", `<div class="fld"><label>显示名</label><input id="rn_n" value="${esc(cur)}" placeholder="张三"></div>
    <div class="note">导入/随机名可改成真实姓名,报表与列表随之更新(19-F2)。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doRename(${mid})">保存</button>`);
}
async function doRename(mid) {
  try { await api("PATCH", "/members/" + mid, { display_name: val("rn_n") }); closeM(); toast("已改名"); renderView(); }
  catch (e) { toast(e.message); }
}
async function toggleMember(mid, enable) {
  // 模型2 R5后:禁用=把 token 置禁用状态(**key 保留、启用即通、近实时生效**),不是删。删除走「离职」。
  if (!enable) {
    return dangerConfirm("确认禁用员工?", `<p>禁用后该员工当前 API Key 会暂停调用,但 Key 本身保留。</p>
      <p>后续重新启用后,仍使用同一枚 Key 恢复调用。若员工已经离职,请使用“离职”。</p>`,
      "确认禁用", `doToggleMember(${mid},false)`);
  }
  return doToggleMember(mid, true);
}
async function doToggleMember(mid, enable) {
  try { await api("POST", "/members/" + mid + "/status", { enabled: enable }); closeM(); toast(enable ? "已启用(同 key,立即通)" : "已禁用(key 保留,启用即通)"); renderView(); }
  catch (e) { toast(e.message); }
}
// 离职(危险):删 token + 软删转离职列表(与禁用区分清楚)。
async function doOffboard(mid, name) {
  return dangerConfirm("确认员工离职?", `<p>确认让「${esc(name)}」离职?</p>
    <p>离职会删除该员工 API Key,调用立即失效,并转入离职列表。资料和历史记录保留,可恢复入职,但恢复后需要重建新 Key,旧 Key 不可恢复。</p>
    <p>若只是临时停用,请使用“禁用”。</p>`, "确认离职", `doOffboardConfirmed(${mid})`);
}
async function doOffboardConfirmed(mid) {
  try { await api("POST", "/members/" + mid + "/offboard", null); closeM(); toast("已离职(key 失效,转离职列表)"); renderView(); }
  catch (e) { toast(e.message); }
}
// 恢复入职:清软删置 active(员工自助重建 key,新 key)。
async function doRestoreMember(mid, name) {
  // A1(28):恢复时平台自动重建新 Key(不再依赖员工自助);统一走品牌 dangerConfirm。
  dangerConfirm("恢复入职?", `<p>恢复「${esc(name)}」入职?</p><p>恢复后系统自动为其生成新 API Key(旧 Key 不可恢复),员工登录即可查看并复制。</p>`,
    "确认恢复", `doRestoreMemberConfirmed(${mid})`);
}
async function doRestoreMemberConfirmed(mid) {
  try { await api("POST", "/members/" + mid + "/restore", null); closeM(); toast("已恢复入职(新 Key 已生成)"); renderView(); }
  catch (e) { toast(e.message); }
}
VIEWS.teams = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/teams", null);
  const showArch = !!S.teamShowArchived;
  const list = (d || []).filter(t => showArch || t.status !== "archived");
  const isA = S.role === "org_admin";
  const rows = list.map(t => {
    const arch = t.status === "archived";
    const acts = isA ? `<td class="right table-actions">
      <span class="btn sm" onclick="openTeamUsage(${t.id},'${jsstr(t.name)}')">用量</span>
      <span class="btn sm" onclick="openTeamMore(${t.id},'${jsstr(t.name)}',${arch})">更多</span>
    </td>` : "<td></td>";
    return `<tr><td>${esc(t.name)}${arch ? ' <span class="tag">已归档</span>' : ""}</td><td>${t.member_count} 人</td>${acts}</tr>`;
  }).join("");
  return head("团队", "团队用于管理与用量统计 · 员工归入团队后可按团队查看用量")
    + (isA ? `<div class="toolbar"><button class="btn pri" onclick="openCreateTeam()">+ 新建团队</button>
       <label class="mini" style="margin-left:12px;cursor:pointer"><input type="checkbox" ${showArch ? "checked" : ""} onclick="S.teamShowArchived=this.checked;renderView()"> 显示已归档</label></div>` : "")
    + `<div class="panel"><div class="table-scroll"><table><thead><tr><th>团队</th><th>成员数</th><th></th></tr></thead><tbody>${rows || '<tr><td colspan=3>' + emptyState("▣", "还没有团队", "建团队后可按团队维度统计用量", isA ? "+ 新建团队" : "", "openCreateTeam()") + '</td></tr>'}</tbody></table></div></div>`;
};
function openCreateTeam() { modal("新建团队", `<div class="fld"><label>团队名称</label><input id="tm_n" placeholder="研发一组"></div>`, `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateTeam()">创建</button>`); }
async function doCreateTeam() { try { await api("POST", "/organizations/" + S.orgId + "/teams", { name: val("tm_n") }); closeM(); toast("已创建"); renderView(); } catch (e) { toast(e.message); } }
function openTeamMore(tid, name, archived) {
  modal("团队更多操作 · " + name, `<div class="action-list compact-actions">
    <div class="action-row"><div><b>修改团队名称</b><div class="mini">只影响展示名称,不改变成员和历史用量。</div></div><button class="btn sm" onclick="openRenameTeam(${tid},'${jsstr(name)}')">改名</button></div>
    <div class="action-row ${archived ? "" : "danger-line"}"><div><b>${archived ? "恢复团队" : "归档团队"}</b><div class="mini">${archived ? "恢复后可继续分配成员。" : "归档前请确认团队下没有在用成员。"}</div></div>
      <button class="btn sm ${archived ? "" : "danger"}" onclick="${archived ? `doUnarchiveTeam(${tid})` : `doArchiveTeam(${tid},'${jsstr(name)}')`}">${archived ? "恢复" : "归档"}</button></div>
  </div>`, `<button class="btn pri" onclick="closeM()">关闭</button>`);
}
function openRenameTeam(tid, name) { modal("团队改名", `<div class="fld"><label>团队名称</label><input id="tm_rn" value="${esc(name)}"></div>`, `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doRenameTeam(${tid})">保存</button>`); }
async function doRenameTeam(tid) { try { await api("PATCH", "/organizations/" + S.orgId + "/teams/" + tid, { name: val("tm_rn") }); closeM(); toast("已改名"); renderView(); } catch (e) { toast(e.message); } }
async function doArchiveTeam(tid, name) {
  return dangerConfirm("确认归档团队?", `<p>确认归档团队「${esc(name)}」?</p><p>归档前请确认团队下没有在用成员。归档不会删除历史用量。</p>`,
    "确认归档", `doArchiveTeamConfirmed(${tid})`);
}
async function doArchiveTeamConfirmed(tid) { try { await api("POST", "/organizations/" + S.orgId + "/teams/" + tid + "/archive", null); closeM(); toast("已归档"); renderView(); } catch (e) { toast(e.message); } }
async function doUnarchiveTeam(tid) { try { await api("POST", "/organizations/" + S.orgId + "/teams/" + tid + "/unarchive", null); closeM(); toast("已恢复"); renderView(); } catch (e) { toast(e.message); } }
async function openTeamUsage(tid, name) {
  try {
    const u = await api("GET", "/organizations/" + S.orgId + "/teams/" + tid + "/usage?since_hours=" + S.win, null);
    const mrows = (u.by_member || []).map(b => `<tr><td>${esc(b.label || ("用户#" + b.key))}</td><td class="right">${money(b.consumed_quota)}</td></tr>`).join("") || '<tr><td class="empty" colspan=2>该窗口暂无用量</td></tr>';
    const modrows = (u.by_model || []).map(b => `<tr><td>${esc(b.key)}</td><td class="right">${money(b.consumed_quota)}</td></tr>`).join("") || '<tr><td class="empty" colspan=2>该窗口暂无用量</td></tr>';
    modal("团队用量 · " + name + "(" + winLabel(S.win) + ")",
      `<div class="note">合计 ${money(u.total_quota)} · 团队用量按成员当前归属聚合</div>
       <table class="kvtable"><tr><td class="k">成员</td><td class="right k">费用</td></tr>${mrows}</table>
       <table class="kvtable" style="margin-top:10px"><tr><td class="k">模型</td><td class="right k">费用</td></tr>${modrows}</table>`,
      `<button class="btn pri" onclick="closeM()">关闭</button>`);
  } catch (e) { toast(e.message); }
}
VIEWS.tiers = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/tiers", null);
  S.tiersCache = d || []; // 供编辑弹窗回填
  try { S.billingGroups = (await api("GET", "/pricing/groups", null)) || []; } catch (e) { S.billingGroups = []; } // T17-6 计费分组
  // 改动⑦补丁(灰度前阻断·产品总监逮出):MVP 下层级页藏掉钱字段(月额度/基础倍率/折后价),
  // 只留层级名 + 模型分组 + 模型集(setup 部分,管理员要靠它把成员映射到模型分组);别藏整菜单否则没法建层级。
  const rows = (d || []).map(t => `<tr><td>${esc(t.name)}${t.is_default ? ' <span class="tag">默认</span>' : ""}</td>
    <td>${t.newapi_group ? esc(t.newapi_group) : '<span class="mini">默认</span>'}</td>
    ${S.mvp ? "" : `<td>${t.monthly_limit_quota != null ? money(t.monthly_limit_quota) + " / 月" : '<span class="mini">不限</span>'}</td>`}
    <td>${(t.model_set || []).map(m => `<span class="mcap">${esc(m)}</span>`).join("") || '<span class="mini">继承组织默认</span>'}</td>
    <td class="right"><span class="btn sm" onclick="openEditTier(${t.id})">编辑</span> <span class="btn sm danger" onclick="doDeleteTier(${t.id},'${jsstr(t.name)}')">删除</span></td></tr>`).join("");
  const tierColspan = S.mvp ? 4 : 5;
  return head("可用模型档位", S.mvp ? "可复用档位 = 一组可用模型 + 模型清单(决定成员能调用哪些模型)" : "可复用档位 = 计费分组 + 模型清单 + 月额度")
    + `<div class="tier-intro">
        <div class="tier-intro-t">档位是什么</div>
        <div class="tier-intro-b">档位 = 一张“员工套餐模板卡”,预先设好【计费分组 + 可用模型清单${S.mvp ? "" : " + 月额度"}】。建档时<b>不会产生任何实际变更</b>,只是存一张模板;真正生效是在<b>开通员工、发 Key 那一刻</b>,把这张卡的内容下发到该员工的 Key 上。计费分组是从平台已有分组里<b>选</b>(企业侧不新建分组)。</div>
      </div>
      <div class="toolbar"><button class="btn pri" onclick="openCreateTier()">+ 新建档位</button></div>
    <div class="panel"><div class="table-scroll"><table><thead><tr><th>档位</th><th>模型分组</th>${S.mvp ? "" : "<th>月额度</th>"}<th>模型清单</th><th></th></tr></thead><tbody>${rows || `<tr><td colspan=${tierColspan}>${emptyState("◆", "还没有可用模型档位", "先建一个档位,决定成员能调用哪些模型", "+ 新建档位", "openCreateTier()")}</td></tr>`}</tbody></table></div></div>`;
};
// groupSelectHTML 计费分组下拉(T17-6):来源实时拉的 /pricing/groups,选项带基础倍率;空=回落默认。
function groupSelectHTML(selId, selected) {
  const opts = [`<option value="">默认(回落组织默认${S.mvp ? "分组" : "/标准价"})</option>`].concat(
    (S.billingGroups || []).map(g => `<option value="${esc(g.group)}" ${g.group === selected ? "selected" : ""}>${esc(g.group)}${S.mvp ? "" : `(基础倍率 ${g.ratio})`}</option>`));
  return `<div class="fld"><label>模型分组(决定成员可用哪些模型${S.mvp ? "" : "与收费倍率"})</label>
    <select id="${selId}" onchange="tierGroupHint('${selId}')">${opts.join("")}</select>
    <div class="mini" id="${selId}_hint" style="margin-top:4px"></div></div>`;
}
function tierGroupHint(selId) {
  const g = document.getElementById(selId).value;
  const hint = document.getElementById(selId + "_hint");
  if (!g) { hint.innerHTML = S.mvp ? "回落组织默认分组,所选模型须在该分组可用范围内" : "回落组织默认分组,所选模型须在该分组可用范围内"; return; }
  const bg = (S.billingGroups || []).find(x => x.group === g);
  if (!bg) { hint.innerHTML = ""; return; }
  const ms = (bg.models || []);
  const modelsTxt = `可用模型(${ms.length}):${ms.slice(0, 12).map(esc).join("、")}${ms.length > 12 ? " …" : ""}`;
  // MVP:只展示可用模型,藏基础倍率/折后价(钱结构不给客户管理员看)。
  hint.innerHTML = S.mvp ? `${modelsTxt}<br>所选模型须在该范围内(否则保存被拦)`
    : `基础倍率 <b>${bg.ratio}</b> · ${modelsTxt}<br>模型清单须 ⊆ 该可用模型(否则保存被拦);折后价 = 基础倍率 × 客户折扣%`;
}
function openEditTier(tid) {
  const t = (S.tiersCache || []).find(x => x.id === tid); if (!t) return;
  const ms = (t.model_set || []).join(",");
  const mq = t.monthly_limit_quota != null ? (t.monthly_limit_quota / 500000) : "";
  modal("编辑档位", `<div class="fld"><label>档位名称</label><input id="te_n" value="${esc(t.name)}"></div>
    ${groupSelectHTML("te_g", t.newapi_group || "")}
    ${S.mvp ? "" : `<div class="fld"><label>月额度(美元,成员当期上限基线)</label><input id="te_q" type="number" value="${mq}"></div>`}
    <div class="fld"><label>模型清单(逗号分隔,留空=继承组织默认分组的可用模型)<span class="tip" title="决定该档位成员能调用哪些大模型,例如 GPT / Claude 系列" onclick="toast(this.title)">?</span></label><input id="te_m" value="${esc(ms)}"></div>
    <div class="note">${S.mvp ? "改后该档位成员的可用模型清单随之更新。" : "改后引用该档位的成员当期上限按新档重算下发;改计费分组=改计价档(动钱相邻)。"}</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doEditTier(${tid})">保存</button>`);
  tierGroupHint("te_g");
}
async function doEditTier(tid) {
  try {
    const ms = val("te_m").split(",").map(s => s.trim()).filter(Boolean);
    const body = { name: val("te_n"), model_set: ms, newapi_group: val("te_g") || null };
    const q = parseFloat(val("te_q")); if (q > 0) body.monthly_limit = Math.round(q * 500000); // 留空=不改
    await api("PUT", "/tiers/" + tid, body); closeM(); toast("已保存"); renderView();
  } catch (e) { toast(e.message); }
}
async function doDeleteTier(tid, name) {
  // 28-建议批:删除是最重操作,统一走品牌 dangerConfirm(原来反而用最弱的原生 confirm)。
  dangerConfirm("确认删除档位?", `<p>确认删除档位「${esc(name)}」?</p><p>被成员引用或为默认档将无法删除;删除不影响已开通成员的现有 Key。</p>`,
    "确认删除", `doDeleteTierConfirmed(${tid})`);
}
async function doDeleteTierConfirmed(tid) {
  try { await api("DELETE", "/tiers/" + tid, null); closeM(); toast("已删除"); renderView(); }
  catch (e) { toast(e.message); }
}
function openCreateTier() {
  modal("新建档位", `<div class="note">建档只是<b>存一张模板卡</b>,此刻不产生实际变更;开通员工时才把它下发到员工 Key 生效。</div>
    <div class="fld"><label>档位名称</label><input id="ti_n" placeholder="标准档"></div>
    ${groupSelectHTML("ti_g", "")}
    ${S.mvp ? "" : `<div class="fld"><label>月额度(美元,成员当期上限基线)</label><input id="ti_q" type="number" placeholder="50"></div>`}
    <div class="fld"><label>模型清单(逗号分隔,留空=继承组织默认分组的可用模型)<span class="tip" title="决定该档位成员能调用哪些大模型,例如 GPT / Claude 系列" onclick="toast(this.title)">?</span></label><input id="ti_m" placeholder="gpt-4o,claude-sonnet-4-5-20250929"></div>
    <div class="note">${S.mvp ? "模型分组决定成员可用哪些模型;所选模型须在该分组可用范围内(否则保存被拦)。" : "模型分组决定可用模型与计价;所选模型须在该分组可用范围内(否则保存被拦)。"}</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateTier()">创建</button>`);
  tierGroupHint("ti_g");
}
async function doCreateTier() {
  try {
    const ms = val("ti_m").split(",").map(s => s.trim()).filter(Boolean);
    const body = { name: val("ti_n"), model_set: ms };
    const g = val("ti_g"); if (g) body.newapi_group = g;
    const q = parseFloat(val("ti_q")); if (q > 0) body.monthly_limit = Math.round(q * 500000);
    await api("POST", "/organizations/" + S.orgId + "/tiers", body); closeM(); toast("已创建"); renderView();
  } catch (e) { toast(e.message); }
}
// v1 裁定A(20-§2.1):额度审批(VIEWS.approvals/decide)与申请增额(VIEWS.myreq)前端隐藏——v2 配额功能,从 git 历史恢复。
VIEWS.billing = async () => {
  const id = S.orgId;
  // v1(19-F3/F6):余额=读求和实时读 new-api;充值在 new-api 完成,平台无充值入口/不记充值流水;
  // 藏价(19-F7):不展示折扣/倍率/计费设置。订阅计费组织显示"订阅计费(无钱包余额)"。
  const [bal, usage] = await Promise.all([
    api("GET", "/organizations/" + id + "/balance", null).catch(() => ({ __err: true })),
    api("GET", "/organizations/" + id + "/usage?since_hours=" + S.win, null).catch(() => ({ total_quota: 0, by_model: [] })),
  ]);
  const isSub = bal.billing_kind === "subscription";
  const balCards = isSub
    ? kpi("计费方式", "订阅计费", "该组织按订阅计费,无钱包余额;用量报表照常")
    : kpi("可用余额", balMoney(bal), "实时读取,消费即时反映");
  return head("余额", isSub ? "订阅计费组织:无钱包余额,用量见概览" : "可用余额实时读取;充值请联系运营方")
    + `<div class="cards">${balCards}${kpi(winLabel(S.win) + "消耗", money(usage.total_quota || 0), "来自当前组织调用记录")}${kpi("涉及模型", String((usage.by_model || []).length), winLabel(S.win))}</div>
    <div class="panel"><div class="ph">说明</div><div class="pb"><div class="note">
      余额为实时值,员工调用后即时下降。${isSub ? "" : "需要充值请联系运营方。"}充值到账后此处自动反映,无需操作。
    </div><div class="toolbar" style="margin-top:12px"><button class="btn" onclick="go('dash')">查看用量概览</button></div></div></div>`;
};

/* ===================== 成员 ===================== */
// A3(28-§阻断):员工"当前状态"读真实状态,不再硬编码"正常"。
// 组织状态(硬停/停服/低余额)优先于本人状态;组织正常时看本人(禁用/离职)。org 硬停时员工调用真会 403,必须诚实告知。
function myStatus() {
  const os = (S.me && S.me.org_status) || "active";
  const ms = (S.me && S.me.status) || "active";
  if (os === "hard_stopped") return { txt: "服务暂停", cls: "bad", note: "组织被运维硬停,调用暂不可用" };
  if (os === "stopped") return { txt: "服务暂停", cls: "bad", note: "组织服务已停,请联系管理员" };
  if (os === "low") return { txt: "余额偏低", cls: "warn", note: "组织余额偏低,请及时充值" };
  if (ms === "offboarded") return { txt: "已离职", cls: "bad", note: "账号已离职" };
  if (ms === "disabled") return { txt: "已禁用", cls: "warn", note: "账号被临时禁用,Key 暂停调用" };
  return { txt: "正常", cls: "ok", note: "" };
}
function myStatusPill() { const s = myStatus(); return `<span class="pill ${s.cls}">${esc(s.txt)}</span>`; }
function myStatusNote() { return myStatus().note; }
VIEWS.myusage = async () => {
  const win = S.win, wl = winLabel(win);
  const [u, ts, d] = await Promise.all([
    api("GET", "/members/" + S.me.id + "/usage?since_hours=" + win, null),
    api("GET", "/members/" + S.me.id + "/usage/timeseries?since_hours=" + win + "&granularity=" + S.gran, null).catch(() => ({ series: [] })),
    api("GET", "/members/" + S.me.id + "/usage/detail?since_hours=" + win + "&page_size=50", null).catch(() => ({ records: [], total: 0 })),
  ]);
  const series = (ts && ts.series) || [];
  const dRecords = (d && d.records) || [], dTotal = (d && d.total) || 0;
  const top = (u.by_model || []).slice(0, 8);
  const max = Math.max(1, ...top.map(b => b.consumed_quota));
  const bars = top.map(b => `<div class="bar"><span class="nm">${esc(b.key)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / max * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`).join("") || `<div class="empty">${wl}暂无用量</div>`;
  // 改动⑦:去掉「调用次数」KPI(单 key 自助态恒=1,无信息量);时间窗 7/30/90 天可选。
  return head("我的用量", wl + "消耗")
    + `<div class="toolbar"><div class="spacer"></div><span class="mini">时间窗</span>${winSelect()}</div>
    <div class="cards">${kpi(wl + "消耗", money(u.total_quota), "按实际调用量统计")}${kpi("涉及模型", String(top.length), wl)}${kpi("当前状态", myStatusPill(), myStatusNote())}</div>
    <div class="panel"><div class="ph">用量趋势(${wl})<span class="mini" style="font-weight:400;float:right">粒度 ${granSelect()}</span></div><div class="pb" id="trendBody">${lineChart(series)}</div></div>
    <div class="panel"><div class="ph">按模型</div><div class="pb">${bars}</div></div>
    <div class="panel"><div class="ph">最近调用(共 ${dTotal} 条,显示最近 ${dRecords.length})</div><div class="pb" style="max-height:360px;overflow:auto">${detailTable(dRecords)}</div></div>`;
};
VIEWS.mykey = async () => {
  const m = await api("GET", "/members/" + S.me.id, null);
  const key = m.key_masked || "-";
  // F3(28):员工自助闭环——补接入地址 + 调用示例 + 本人可用范围,拿到 key 就知道怎么用、能调什么。
  const gw = (S.me && S.me.gateway_base_url || "").replace(/\/+$/, "");
  const gwBlock = gw ? `<div class="panel"><div class="ph">接入地址</div><div class="pb">
      <table class="kvtable">
        <tr><td class="k">Base URL</td><td><b>${esc(gw)}</b> ${copyBtn(gw, "接入地址")}</td></tr>
        <tr><td class="k">OpenAI 兼容端点</td><td>${esc(gw)}/v1/chat/completions</td></tr>
      </table>
      <div class="note" style="margin-top:10px">调用示例(把 <b>&lt;你的Key&gt;</b> 换成上方"复制完整"取得的 Key):</div>
      <pre class="codebox">curl ${esc(gw)}/v1/chat/completions \\
  -H "Authorization: Bearer &lt;你的Key&gt;" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${esc((m.model_set || [])[0] || "gpt-4o")}","messages":[{"role":"user","content":"你好"}]}'</pre>
    </div></div>` : "";
  const models = (m.model_set || []).map(x => `<span class="mcap">${esc(x)}</span>`).join("")
    || '<span class="mini">按组织默认分组的可用范围</span>';
  const rangeBlock = `<div class="panel"><div class="ph">我的可用范围</div><div class="pb"><table class="kvtable">
      ${m.tier_name ? `<tr><td class="k">档位</td><td>${esc(m.tier_name)}</td></tr>` : ""}
      <tr><td class="k">可用模型</td><td>${models}</td></tr>
    </table></div></div>`;
  return head("我的 API Key", "用于在 Claude Code、Codex、OpenAI SDK 等工具中访问本企业已开放的模型")
    + `<div class="panel"><div class="pb">
      <div class="keybox"><span>${esc(key)}</span>${revealKeyBtn(S.me.id, "我的 API Key")}</div>
      <div class="note">页面只展示脱敏串;点"复制完整"可取本人明文 Key 用于配置工具。Key 由平台在开通时生成,生成后不可修改;如需更换 Key 请联系管理员。</div>
      <div class="toolbar" style="margin-top:14px"><button class="btn" onclick="go('logs')">查看使用日志</button></div>
    </div></div>` + gwBlock + rangeBlock;
}
// A1(28-§阻断):员工 key 纯只读——自助建/轮换 key、改 IP 白名单已整体下线(前端函数删除,后端对 member 一律 403)。
// key 生命周期全程平台/管理员驱动:开通建、离职删、恢复重建;员工侧只读查看 + 复制完整(revealKeyBtn)。
VIEWS.mynotif = async () => {
  // B2(28):分页,>30 条历史通知不再永久不可达;未读单条可点已读(后端 /notifications/{id}/read 本就支持)。
  const page = S.notifPage || 1, size = 30;
  const d = await api("GET", "/notifications?page=" + page + "&page_size=" + size, null);
  const total = (d.pagination || {}).total || (d.list || []).length;
  const maxPage = Math.max(1, Math.ceil(total / size));
  const rows = (d.list || []).map(n => `<div class="notif-item ${n.is_read ? "" : "unread"}" ${n.is_read ? "" : `onclick="markOneRead(${n.id})" title="点击标记已读" style="cursor:pointer"`}>
    <div class="notif-title">${n.is_read ? "" : '<span class="dot"></span>'}${esc(n.title)}</div>
    <div class="notif-body">${esc(n.body || "")}</div>
    <div class="notif-time">${esc((n.created_at || "").slice(0, 16).replace("T", " ") || "-")}</div>
  </div>`).join("");
  const pager = total > size
    ? `<div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 条 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goNotifPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goNotifPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>` : "";
  return head("通知", "站内通知 · 仅本人相关 · 未读 " + (d.unread || 0))
    + `<div class="toolbar"><button class="btn" onclick="markAll()">全部已读</button></div>
    <div class="panel"><div class="pb">${rows ? `<div class="notif-list">${rows}</div>` : emptyState("✉", "暂无通知", "账号、用量相关的消息会显示在这里", "", "")}${pager}</div></div>`;
};
function goNotifPage(p) { S.notifPage = Math.max(1, p); renderView(); }
async function markOneRead(id) { try { await api("POST", "/notifications/" + id + "/read", null); renderView(); } catch (e) { toast(e.message); } }
async function markAll() { try { await api("POST", "/notifications/all/read", null); toast("已全部标记已读"); renderView(); } catch (e) { toast(e.message); } }

/* ---------- 进入 ---------- */
if (S.token) { boot().catch(() => { document.getElementById("login").classList.remove("hide"); }); }
else { document.getElementById("login").classList.remove("hide"); }
