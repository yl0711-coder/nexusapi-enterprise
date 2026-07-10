/* NexusAPI 企业管理平台 · 生产前端 SPA(接 /api/v1 真后端)。
   设计沿用原型 app.css;数据全来自真实 API(非 mock)。按 /me 的角色拼装菜单与视图。 */
"use strict";
const S = { token: localStorage.getItem("nx_token") || "", me: null, role: "", view: "", org: null, orgId: 0, win: 168, gran: "day", qpu: 500000, psettings: null };
// 品牌(42号样式-3:运营配置,后台可改不经代码发版)。boot 时从公开端点 GET /branding 拉取;
// 未配置的字段一律优雅降级:产品名回退中性默认,公司/客服/文档为空则隐藏对应入口——绝不露占位邮箱。
const BRAND = { product: "企业管理台", company: "", support: "", doc: "" };
const brandCompany = () => BRAND.company || "";
const brandDoc = () => BRAND.doc || "";
async function loadBranding() {
  try {
    const b = await api("GET", "/branding", null);
    if (b) {
      if (b.product_name) BRAND.product = b.product_name;
      BRAND.company = b.company_name || "";
      BRAND.support = b.support_email || "";
      BRAND.doc = b.doc_url || "";
      document.title = BRAND.product;
      const el = document.getElementById("lg_prod"); if (el) el.textContent = BRAND.product;
    }
  } catch (e) {}
}
// 改动⑦:看板时间窗(小时)。7/30/90 天 + 全部历史(24:门B 回填历史可能远早于默认窗);默认 168=近 7 天。
const WIN_ALL = 263520; // C4:=后端 maxUsageWindowHours(24*366*30≈30年),口径一致;覆盖全部历史且 since>1970
function winLabel(h) { return ({ 168: "近 7 天", 720: "近 30 天", 2160: "近 90 天", [WIN_ALL]: "全部历史" })[h] || ("近 " + Math.round(h / 24) + " 天"); }
// A5:余额接口失败显示"暂不可用"占位,不把失败当真实 $0.00(否则误导客户误判欠费/停服)。
// 取数处 catch 返回 {__err:true},渲染走此函数区分"真 0"与"取数失败"。
// 架构B:余额对象为 /orgs/:id/balance 形态(treasury_raw);兼容旧 available_quota 形态。
function balMoney(b) {
  if (!b || b.__err) return "暂不可用";
  const t = rawOf(b, "treasury_raw", "treasury_quota_raw", "available_quota");
  return t != null ? money(t) : "暂不可用";
}
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
  // 39号 P2-7:非 2xx 且无信封 code(5xx 空 body / 网关错误页)不再静默返回 undefined——
  // 如实抛错让页面显示"加载失败",而非误读成"没数据"。
  if (!r.ok && j.code === undefined) throw new Error("请求失败 " + r.status);
  return j.data;
}
// 架构B 单位口径(31-ADR §3):内部/API 全 raw quota;换算只在 UI 边界一次——显示 ÷quota_per_unit、输入 ×quota_per_unit。
// quota_per_unit 不硬编码:boot 时从 GET /platform-settings 拉(S.qpu),500000 仅为拉取失败时的兜底。
const money = q => "$" + ((Number(q) || 0) / (S.qpu || 500000)).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
const usd2raw = usd => Math.round((Number(usd) || 0) * (S.qpu || 500000)); // 输入边界:美元 → raw
// rawOf:契约金额字段一律 *_raw(int64);字段名逐一容错取第一个非空值,取不到返回 null(显示 "-",不冒充 $0)。
const rawOf = (o, ...ks) => { for (const k of ks) { if (o && o[k] != null && o[k] !== "") return Number(o[k]); } return null; };
const normList = d => Array.isArray(d) ? d : ((d && (d.list || d.items)) || []);
function setErr(id, msg) { const el = document.getElementById(id); if (el) el.textContent = msg || ""; }
async function loadQpu() {
  // 33 §3.5:FE 用 /platform-settings 的 quota_per_unit 换算美元;契约把该端点列在 operator 下,
  // 非 operator 角色若被 403 则回退 /me 附带值,再兜底 500000(缺口已记交付说明,待组长裁定)。
  try {
    const ps = await api("GET", "/platform-settings", null);
    if (ps) { S.psettings = ps; const v = Number(ps.quota_per_unit); if (v > 0) { S.qpu = v; return; } }
  } catch (e) {}
  const v = Number(S.me && S.me.quota_per_unit);
  if (v > 0) S.qpu = v;
}
// esc:HTML 文本/双引号属性转义(用于可见文本、title=、value= 等 HTML 上下文)。
// 安全审计 2026-06-30 修正:esc 仅适用 HTML 上下文,绝不可用于内联事件处理器(onclick="…")里的 JS 字符串参数——
// HTML 解析器会先把 &#39; 解码回 ',字符串被闭合可注入任意 JS(经典嵌套上下文坑;原"&#39; 能堵 onclick"判断为误)。
// onclick 内的字符串参数一律用 jsstr。
const esc = s => String(s == null ? "" : s).replace(/[&<>"'`]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;", "`": "&#96;" }[c]));
// jsstr:编码成「HTML 属性 + JS 字符串」双重上下文都安全的形式——非字母数字下划线一律转 \xHH / \uHHHH。
// 产物只含 反斜杠/x/u/十六进制,无 ' " < > & → HTML 解码后原样保留,JS 里是字面字符、不闭合字符串。用于 onclick 等内联处理器参数。
const jsstr = s => String(s == null ? "" : s).replace(/[^a-zA-Z0-9_]/g, c => { const n = c.charCodeAt(0); return n < 256 ? "\\x" + n.toString(16).padStart(2, "0") : "\\u" + n.toString(16).padStart(4, "0"); });
const roleCN = r => ({ operator: "运营方", org_admin: "组织管理员", team_leader: "团队负责人", member: "成员" }[r] || r);
const memberStatusCN = s => ({ active: "启用", disabled: "停用", offboarded: "离职", provisioning: "开通中", quarantined: "开通异常", provision_failed: "开通失败" }[s] || s || "-");
const orgStatusCN = s => ({ active: "正常", low: "余额偏低", stopped: "欠费停用", hard_stopped: "已硬停" }[s] || s || "-"); // 45号 P3-9:组织状态中文化
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
loadBranding(); // 未登录态(登录页)也拉一次公开品牌(免鉴权端点;42号样式-3)

/* ---------- 启动 ---------- */
async function boot() {
  S.me = await api("GET", "/me", null);
  document.getElementById("login").classList.add("hide"); // 自动登录路径也要隐藏登录浮层
  S.role = S.me.role; S.orgId = S.me.org_id;
  // 架构B:mvpGate 机制退役(33 §12 增补),/me 的 mvp_mode 阶段2 移除;前端按"全功能"口径,不再读该字段。
  await loadQpu(); // 架构B:金额换算基准(quota_per_unit),进 UI 前拿到
  await loadBranding(); // 42号样式-3:品牌运营配置(产品名/公司/客服/文档)
  const def = { operator: "orgs", org_admin: "dash", team_leader: "members", member: "myusage" }[S.role];
  S.view = def;
  renderShell(); renderSide(); renderView();
}

// 架构B(29-PRD v4 / 33 §3.5):org_admin=成员/档位/余额与账本/模型广场;member=自管令牌/额度与账本/模型广场;
// operator 加平台账本 + 平台设置(money_freeze 等)。mykey(A 版单 key 页)退役,由「我的令牌」取代。
const NAV = {
  operator: [{ grp: "运营" }, { v: "orgs", ic: "▦", t: "客户组织" }, { v: "employees", ic: "☷", t: "员工管理" }, { v: "oplogs", ic: "▤", t: "使用日志" }, { v: "pledger", ic: "▤", t: "平台账本" }, { v: "psettings", ic: "⚙", t: "平台设置" }],
  org_admin: [{ grp: "管理" }, { v: "dash", ic: "◧", t: "概览" }, { v: "members", ic: "☷", t: "成员" }, { v: "teams", ic: "▣", t: "团队" }, { v: "tiers", ic: "◆", t: "额度档位" }, { v: "marketplace", ic: "▦", t: "模型广场" }, { v: "billing", ic: "¥", t: "余额与账本" }, { v: "logs", ic: "▤", t: "使用日志" }, { v: "mynotif", ic: "✉", t: "通知" }],
  team_leader: [{ grp: "团队" }, { v: "members", ic: "☷", t: "团队成员" }],
  member: [{ grp: "我的" }, { v: "myusage", ic: "▦", t: "我的用量" }, { v: "mytokens", ic: "⚿", t: "我的令牌" }, { v: "mybalance", ic: "¥", t: "额度与账本" }, { v: "marketplace", ic: "▦", t: "模型广场" }, { v: "logs", ic: "▤", t: "使用日志" }, { v: "mynotif", ic: "✉", t: "通知" }],
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
  const supLine = BRAND.support ? `<div class="fld"><label>联系客服</label><a href="mailto:${esc(BRAND.support)}">${esc(BRAND.support)}</a></div>` : "";
  modal("帮助与支持", `${docLine}${supLine}
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
    mytokens: w('<circle cx="8" cy="14" r="4"></circle><path d="M11 11l9-9M16 6l3 3"></path>'),
    mybalance: w('<circle cx="12" cy="12" r="9"></circle><path d="M9 9l3 4 3-4M12 13v5M9.5 15h5"></path>'),
    marketplace: w('<path d="M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4z"></path><circle cx="16.5" cy="16.5" r="3.5"></circle>'),
    pledger: w('<path d="M5 3h14v18l-3-2-2 2-2-2-2 2-2-2-3 2z"></path><path d="M9 8h6M9 12h6"></path>'),
    psettings: w('<circle cx="12" cy="12" r="3.2"></circle><path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5 5l2.1 2.1M16.9 16.9 19 19M19 5l-2.1 2.1M7.1 16.9 5 19"></path>'),
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
// revealTokenKey:成员本人对自己令牌"复制完整"→ POST /me/tokens/:id/key:reveal 即时取明文(平台不存明文)。
// 走弹框而非直接写剪贴板:clipboard.writeText 需在用户手势同步栈,await 后调用 Safari 会失效;弹框内复制按钮点击是新手势,稳。
// 仅成员本人可揭示(assertSelf);运营方无此入口;后端再兜底(挡 SupportSession、越权 403)。
// 架构B:A 版成员级 /members/:id/key:reveal 已退役(org_admin 复制成员 key 的端点缺口见交付说明)。
async function revealTokenKey(tokenID, name) {
  try {
    const d = await api("POST", "/me/tokens/" + tokenID + "/key:reveal", null);
    const k = (d && (d.key || d.api_key)) || "";
    modal("完整令牌 Key" + (name ? (" · " + name) : ""),
      `<div class="note">明文 Key,复制后请妥善保管;平台不留存明文。Key 不可修改,如需更换请删除该令牌后重建。</div>
       <div class="keybox"><span>${esc(k)}</span>${copyBtn(k, "API Key")}</div>`,
      `<button class="btn pri" onclick="closeM()">完成</button>`);
  } catch (e) { toast(e.message); }
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
    <td>${pill(orgStatusCN(o.status), o.status === "active" ? "ok" : o.status === "low" ? "warn" : "bad")}${o.archived ? ' <span class="tag">已归档</span>' : ''}</td>
    <td>${esc(o.timezone)}</td><td>${esc(billingModeCN(o.billing_mode))}</td>
    <td class="right"><span class="btn sm" onclick="openOrgTreasury(${o.id},'${jsstr(o.name)}')">金库</span> <span class="btn sm" onclick="enterOrg(${o.id},'${jsstr(o.name)}')">进入</span> ${o.archived
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
// 架构B 组织入网 = 只有门A 新建(平台建金库 new-api 用户)。门B"关联现有用户"已整体退役
// (总监裁定 2026-07-07/ADR §11):后端关联字段已删、导入端点已摘,前端此弹窗
// 曾残留门B 选项致提交带 associate 字段被严格 JSON 解析拒(生产报"请求体格式非法"),本次清净。
function openCreateOrg() {
  modal("组织入网", `<div class="fld"><label>组织名称</label><input id="co_n" placeholder="Acme 科技"></div>
    <div class="fld"><label>唯一标识 slug</label><input id="co_s" placeholder="acme"></div>
    <div class="fld"><label>管理员邮箱</label><input id="co_e" placeholder="admin@acme.com"></div>
    <div class="fld"><label>new-api 用户分组</label><input id="co_g" placeholder="org_acme"></div>
    <div class="note">平台将自动创建该组织的金库 new-api 用户(门A);组织的钱由运营方向金库代充,再由管理员划拨给成员。</div>
    <div class="note">new-api 用户分组须先在 new-api 配好(挂模型分组 group_special_usable_group),否则建组织会被拒。一个分组只绑一个组织。管理员初始密码本次回显一次。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateOrg()">创建</button>`);
}
async function doCreateOrg() {
  try {
    const body = { name: val("co_n"), slug: val("co_s"), admin_email: val("co_e"), newapi_user_group: val("co_g") };
    const d = await api("POST", "/organizations", body);
    modal("已创建", `<div class="note">组织已创建。请把管理员初始凭证交付客户(仅显示一次):</div>
      <table class="kvtable"><tr><td class="k">管理员邮箱</td><td>${esc(d.admin_email)}</td></tr>
      <tr><td class="k">初始密码</td><td><b>${esc(d.admin_initial_password)}</b></td></tr></table>`,
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
  // 架构B:金库余额走 /orgs/:id/balance(读求和,escrow 已退役);充值仍在 new-api 侧对金库 user 代充。
  const [org, eb] = await Promise.all([
    api("GET", "/organizations/" + id, null),
    api("GET", "/orgs/" + id + "/balance", null).catch(() => ({ __err: true })),
  ]);
  const mem = await api("GET", "/organizations/" + id + "/members?page=1&page_size=20&exclude_admin=1", null);
  const main = document.getElementById("main");
  const hardStopped = org.status === "hard_stopped";
  S.orgDetail = { org, eb, mem };
  main.innerHTML = head("客户组织详情", "运营方支持视角 · 客户概览 / 成员排障 / 使用日志 / 风险控制") // C10:head() 内部已 esc,勿双重转义
    + `<div class="crumb"><span class="lk" onclick="go('orgs')">客户组织</span><span class="sep">/</span><b>${esc(name)}</b></div>
    ${hardStopped ? `<div class="note" style="color:#c00">该组织处于运维硬停中:全部 key 已 403,平台管理操作暂不可用,解除后恢复。</div>` : ""}
    <div class="org-hero">
      <div>
        <div class="org-title">${esc(name)}</div>
        <div class="org-meta">组织 ID ${id} · ${esc(billingModeCN(org.billing_mode || "wallet"))} · ${esc(org.timezone || "Asia/Shanghai")}</div>
      </div>
      <div class="org-status">${pill(orgStatusCN(org.status), org.status === "active" ? "ok" : hardStopped ? "bad" : "warn")}</div>
    </div>
    <div class="cards">
      ${kpi("金库余额", balMoney(eb), "组织金库实时余额")}
      ${kpi("组织状态", pill(orgStatusCN(org.status), org.status === "active" ? "ok" : hardStopped ? "bad" : "warn"), hardStopped ? "组织级硬停中" : "正常服务中")}
      ${kpi("成员数", (mem.pagination || {}).total || (mem.list || []).length || 0, "API 使用成员")}
    </div>
    <div class="tabs org-tabs">
      ${orgTabButton("overview", "概览")}
      ${orgTabButton("members", "成员")}
      ${orgTabButton("tokens", "API Key 归属")}
      ${orgTabButton("logs", "使用日志")}
      ${orgTabButton("support", "支持会话")}
      ${orgTabButton("risk", "风险控制")}
    </div>
    <div id="orgTabBody">${renderOrgOverviewPanel(org, eb, mem)}</div>`;
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
        const [org, eb, mem] = await Promise.all([
          api("GET", "/organizations/" + S.orgId, null),
          api("GET", "/orgs/" + S.orgId + "/balance", null).catch(() => ({ __err: true })),
          api("GET", "/organizations/" + S.orgId + "/members?page=1&page_size=20&exclude_admin=1", null) // 45号 P3-13:成员数KPI/概览表=API使用成员口径,
        ]);
        S.orgDetail = { org, eb, mem };
      }
      box.innerHTML = renderOrgOverviewPanel(S.orgDetail.org, S.orgDetail.eb, S.orgDetail.mem);
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
function renderOrgOverviewPanel(org, eb, mem) {
  const id = S.orgId;
  const hardStopped = org.status === "hard_stopped";
  const memberTotal = (mem.pagination || {}).total || (mem.list || []).length || 0;
  return `<div class="row2">
    <div class="panel">
      <div class="ph">组织概览</div>
      <div class="pb">
        <table class="kvtable">
          <tr><td class="k">金库余额</td><td><b>${balMoney(eb)}</b></td></tr>
          <tr><td class="k">组织状态</td><td>${pill(orgStatusCN(org.status), org.status === "active" ? "ok" : hardStopped ? "bad" : "warn")}</td></tr>
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
  // 45号 P2-D 连带:Key(脱敏)列删除——m.key_masked 是 1:1 时代死字段(1:N 后恒空),Key 归属看「API Key 归属」tab。
  const rows = (mem.list || []).map(m => `<tr><td>${esc(m.display_name || m.login_email)}</td>
    <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td></tr>`).join("");
  // A2(28-§阻断):成员 Tab 分页——>50 成员时显示总数 + 上/下页,消除"page_size=20 静默截断致运营看不到人"。
  const total = (mem.pagination || {}).total || (mem.list || []).length || 0;
  const page = S.orgMemPage || 1, pages = Math.max(1, Math.ceil(total / 50));
  const pager = total > 50
    ? `<div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 人 · 第 ${page}/${pages} 页</span>
        <button onclick="goOrgMemPage(-1)" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goOrgMemPage(1)" ${page >= pages ? "disabled" : ""}>下一页</button></div>`
    : `<div class="mini" style="text-align:right;margin-top:8px">共 ${total} 人</div>`;
  return `<div class="panel"><div class="ph">成员</div><div class="pb"><div class="table-scroll"><table><thead><tr><th>成员</th><th>状态</th></tr></thead><tbody>${rows || `<tr><td colspan="2">${emptyState("☷", "该组织还没有成员", "开通员工后会显示在这里", "", "")}</td></tr>`}</tbody></table></div>${pager}</div></div>`;
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
// 门B 令牌导入 / 历史回填 UI 已随机器退役删除(总监裁定;后端端点已摘,v2 收编按 doc24 重建)。
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
        // 45号 P3-7:空值(N/A)不渲染彩色胶囊、不给复制钮——"–"当色块+复制空串是噪声(四处日志表同源此处)。
        token: `<td class="mini">${token && token !== "-" ? `<span class="log-token" title="${esc(token)}">${esc(token)}</span>${copyBtn(token, "令牌名")}` : '<span class="mini">-</span>'}</td>`,
        model: `<td class="mini">${x.model_name ? `<span class="model-badge" title="${esc(x.model_name)}">${esc(x.model_name)}</span>${copyBtn(x.model_name, "模型名")}` : '<span class="mini">-</span>'}</td>`,
        channel: `<td class="mini">${channelID ? `<span class="channel-badge">${channelID}</span>` : '<span class="mini">-</span>'}${x.channel_name ? `<span class="cell-sub" title="${esc(x.channel_name)}">${esc(x.channel_name)}</span>` : ""}</td>`,
        group: `<td class="mini">${x.group_name ? `<span class="group-badge" title="${esc(x.group_name)}">${esc(x.group_name)}</span>` : '<span class="mini">-</span>'}</td>`,
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
        <input id="nl_member" class="fsel" value="${esc(st.member || "")}" placeholder="成员 ID">`}
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
  // (门B 历史回填的"自动切全部历史"逻辑已随机器退役删除;架构B 组织都是新建,默认近 7 天窗即可。)
  const win = S.win, wl = winLabel(win);
  const reqs = [api("GET", "/organizations/" + id + "/usage?since_hours=" + win, null)];
  // 架构B(31-ADR §2):组织余额 = 金库 + Σ成员,读求和走 /orgs/:id/balance;概览 KPI 展示金库余额。
  reqs.push(api("GET", "/orgs/" + id + "/balance", null).catch(() => ({ __err: true })));
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
  // 架构B:金库余额 KPI + 金库低预警条(低于阈值后端置 low 标记;文案按 29-PRD §4.9)。
  const balCards = kpi("金库余额", balMoney(bal), "组织金库实时余额(充值请联系运营方)");
  const lowBar = (!bal.__err && (bal.low === true || bal.treasury_low === true))
    ? `<div class="note" style="background:var(--warnbg);border-color:#fde68a;color:#92400e;margin:0 0 14px"><b>组织余额不足,请联系运营方充值。</b>金库偏低时,开通成员 / 划拨额度 / 订阅补满可能失败。</div>` : "";
  const srch = (iid, cid, ph) => `<input id="${iid}" placeholder="${ph}" oninput="filterEls('${iid}','${cid}')" style="float:right;width:150px;padding:2px 8px;font-size:12px">`;
  // 数据安全承诺条(仅客户 org_admin);文案站得住:MVP 下成员 key 员工自助建、平台不经手,观测不扣款。
  const safebar = S.role === "org_admin" ? `<div class="safebar">额度与消费以 new-api 实际扣费为准;平台负责金库划拨与用量统计,不经手您的请求内容与令牌明文。</div>` : "";
  // 新组织上手清单:v1 无全期消耗字段(company_balance 降级),改用「当前窗口零消耗且无任何用量条目」近似判断。
  const showOnboard = S.role === "org_admin" && (usage.total_quota || 0) === 0 && mem.length === 0;
  const onboard = showOnboard ? `<div class="panel onboard"><div class="ph">快速上手</div><div class="pb">
    <div class="ob-row"><span class="ob-n">1</span><div><b>开通员工</b><div class="mini">在「成员」开通员工账号,交付登录凭证</div></div><button class="btn sm pri" onclick="go('members')">去开通</button></div>
    <div class="ob-row"><span class="ob-n">2</span><div><b>(可选)建团队</b><div class="mini">想按团队看用量就先建团队,再把员工归入</div></div><button class="btn sm" onclick="go('teams')">建团队</button></div>
    <div class="ob-row"><span class="ob-n">3</span><div><b>查看用量</b><div class="mini">员工开始调用后,这里会显示按团队 / 员工 / 模型的用量</div></div></div>
  </div></div>` : "";
  return head("概览", "组织金库余额 + 用量")
    + safebar + lowBar
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
// 架构B 成员页(29-PRD §4.3 / 33 §3.5):成员=独立 new-api user,持硬限额;列表含 额度/已用/剩余(美元显示)。
// 端点:GET /orgs/:id/members(分页);操作:quota:grant 划拨 / status 停用启用 / offboard 离职退额 / restore 恢复重新分配。
VIEWS.members = async () => {
  const id = S.orgId;
  const mf = S.memberFilter || { q: "", team: "", status: "" };
  const mq = new URLSearchParams({ page: String(S.membersPage || 1), page_size: "50" });
  if (mf.q) mq.set("q", mf.q);
  if (mf.team) mq.set("team_id", mf.team);
  if (mf.status) mq.set("status", mf.status);
  const [d, teams] = await Promise.all([
    api("GET", "/orgs/" + id + "/members?" + mq.toString(), null),
    api("GET", "/organizations/" + id + "/teams", null).catch(() => []), // 团队端点续用(33 §5)
  ]);
  S.teamsCache = teams || []; // 供调团队弹窗
  const tmap = {}; (teams || []).forEach(t => tmap[t.id] = t.name);
  // 管理类账号(运营方/组织管理员)不进"成员"列表(M7);只列 API 使用成员。
  const rows = (d.list || []).filter(m => m.role !== "org_admin" && m.role !== "operator").map(m => {
    const nm = m.display_name || m.login_email || ("成员#" + m.id);
    const remain = rawOf(m, "remaining_raw"); // 40号 P2-1 统一契约名(单一名,契约测锁死)
    const used = rawOf(m, "used_raw");        // 已用 = max(0, granted−remaining) 实时派生(P1-1 单一口径)
    const granted = rawOf(m, "granted_raw");
    const off = m.status === "offboarded";
    const zero = m.status === "active" && granted != null && granted > 0 && remain != null && remain <= 0; // 42号应修-1:开通中/失败不误标"额度已用完"
    const pfail = m.status === "provision_failed";
    const acts = pfail
      ? `<span class="btn sm pri" onclick="doRetryProvision(${m.id},'${jsstr(nm)}')">重试开通</span>`
      : off
      ? `<span class="btn sm pri" onclick="openRestore(${m.id},'${jsstr(nm)}')">恢复入职</span>`
      : `<span class="btn sm" onclick="openGrant(${m.id},'${jsstr(nm)}')">划拨额度</span>
         <span class="btn sm" onclick="openMemberLogs(${m.id},'${jsstr(nm)}')">日志</span>
         <span class="btn sm" onclick="toggleMember(${m.id},${m.status !== "active"})">${m.status === "active" ? "停用" : "启用"}</span>
         <span class="btn sm" onclick="openMemberMore(${m.id},'${jsstr(nm)}','${jsstr(m.display_name || "")}',${m.team_id || 0})">更多</span>`;
    return `<tr>
      <td><span class="cell-clip" title="${esc(nm)}">${esc(nm)}</span><div class="mini">${esc(m.login_email || "")}</div></td>
      <td>${m.team_id ? esc(tmap[m.team_id] || ("团队#" + m.team_id)) : '<span class="mini">未分组</span>'}</td>
      <td>${m.tier_name ? esc(m.tier_name) : '<span class="mini">-</span>'}</td>
      <td class="right">${granted != null ? money(granted) : "-"}</td>
      <td class="right">${used != null ? money(used) : "-"}</td>
      <td class="right">${remain != null ? `<b>${money(remain)}</b>` : "-"}${zero ? '<div class="mini" style="color:var(--bad)">额度已用完,请联系管理员</div>' : ""}${pfail ? '<div class="mini" style="color:var(--bad)">开通失败:金库充值后可重试</div>' : ""}</td>
      <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : off ? "bad" : "mut")}</td>
      <td class="right table-actions">${acts}</td></tr>`;
  }).join("");
  // A6:成员分页(page_size=50)——>50 成员时显示总数 + 上/下页,确保全量可达(不静默截断)。
  const memTotal = (d.pagination || {}).total || (d.list || []).length;
  const memPage = S.membersPage || 1, memPages = Math.max(1, Math.ceil(memTotal / 50));
  const memPager = memTotal > 50
    ? `<div class="toolbar mini">共 ${memTotal} 名成员 · 第 ${memPage}/${memPages} 页
        <button class="btn sm" ${memPage <= 1 ? "disabled" : ""} onclick="goMembersPage(-1)">上一页</button>
        <button class="btn sm" ${memPage >= memPages ? "disabled" : ""} onclick="goMembersPage(1)">下一页</button></div>`
    : "";
  const teamOpts = (teams || []).filter(t => t.status !== "archived").map(t => `<option value="${t.id}"${String(mf.team) === String(t.id) ? " selected" : ""}>${esc(t.name)}</option>`).join("");
  return head(S.role === "team_leader" ? "团队成员" : "成员", "成员 = 独立服务账号,持硬限额(额度由组织金库划拨,自管令牌);停用=令牌暂停、额度保留;离职=停用并退额回金库")
    + `<div class="toolbar log-filter" style="gap:8px;flex-wrap:wrap">
      <input id="mf_q" class="fsel" value="${esc(mf.q || "")}" placeholder="搜索姓名 / 登录名">
      <select id="mf_team" class="fsel"><option value="">全部团队</option>${teamOpts}</select>
      <select id="mf_status" class="fsel"><option value="">全部状态</option><option value="active"${mf.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${mf.status === "disabled" ? " selected" : ""}>停用</option><option value="offboarded"${mf.status === "offboarded" ? " selected" : ""}>离职</option></select>
      <button class="btn sm" onclick="applyMemberFilter()">筛选</button>
      <button class="btn sm" onclick="resetMemberFilter()">重置</button>
      <div class="spacer"></div>
      <button class="btn pri" onclick="openAddMember()">+ 开通成员</button></div>
    <div class="panel"><div class="table-scroll"><table class="kvtable member-table"><colgroup><col class="m-name"><col class="m-team"><col class="m-tier"><col class="m-granted"><col class="m-used"><col class="m-remain"><col class="m-status"><col class="m-actions"></colgroup><thead><tr><th>成员</th><th>团队</th><th>档位</th><th class="right">累计划入</th><th class="right">已用</th><th class="right">剩余额度</th><th>状态</th><th></th></tr></thead>
    <tbody>${rows || '<tr><td colspan=8>' + emptyState("☷", "还没有成员", "开通第一位成员:选一个额度档位,从金库划拨初始额度", "+ 开通成员", "openAddMember()") + '</td></tr>'}</tbody></table></div>${memPager}</div>`;
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
  // 45号 P2-D 方案a:裁成**身份定位视图**(组织/员工/团队/状态/分组+入口)——Key/token_id 读 1:1 旧列
  // 架构B 下恒空、月额度读 0032 废字段、剩余没查、"已用"口径与成员页分裂,全部裁掉;
  // Key 排障走组织详情"API Key 归属"页(数据正确),额度管理走各组织成员页。跨组织按 Key 搜留 v-future。
  const rows = list.map(m => {
    return `<tr>
      <td><span class="cell-clip org-cell" title="${esc(m.org_name)}">${esc(m.org_name)}</span><span class="cell-sub">ID ${m.org_id}</span></td>
      <td><span class="cell-clip" title="${esc(m.display_name || m.login_email)}">${esc(m.display_name || m.login_email)}</span><span class="cell-sub">${esc(m.login_email)}</span></td>
      <td><span class="cell-clip" title="${esc(m.team_name || "未分组")}">${esc(m.team_name || "未分组")}</span></td>
      <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td>
      <td><span class="cell-clip" title="${esc(m.newapi_group || "-")}">${esc(m.newapi_group || "-")}</span></td>
      <td class="right table-actions">
        <span class="btn sm" onclick="S.logMirror=newGlobalLogState();S.logMirror.member='${m.id}';go('oplogs')">日志</span>
        <span class="btn sm" onclick="enterOrg(${m.org_id},'${jsstr(m.org_name)}')">进组织</span>
      </td>
    </tr>`;
  }).join("");
  return head("员工管理", "运营方全局员工目录:跨客户定位人与组织;Key 排障请进组织详情「API Key 归属」,额度管理在各组织成员页")
    + `<div class="toolbar log-filter" style="gap:8px;flex-wrap:wrap">
      <input id="ef_q" class="fsel" value="${esc(f.q || "")}" placeholder="姓名 / 登录名 / 公司">
      <input id="ef_org" class="fsel" value="${esc(f.org || "")}" placeholder="组织 ID">
      <input id="ef_team" class="fsel" value="${esc(f.team || "")}" placeholder="团队 ID">
      <select id="ef_status" class="fsel"><option value="">全部状态</option><option value="active"${f.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${f.status === "disabled" ? " selected" : ""}>禁用</option><option value="provisioning"${f.status === "provisioning" ? " selected" : ""}>开通中</option></select>
      <button class="btn sm" onclick="applyEmployeeFilter()">筛选</button>
      <button class="btn sm" onclick="resetEmployeeFilter()">重置</button>
    </div>
    <div class="panel"><div class="ph">全局员工列表</div><div class="pb">
      <div class="table-scroll"><table class="kvtable employee-table"><colgroup><col class="e-org"><col class="e-member"><col class="e-team"><col class="e-status"><col class="e-group"><col class="e-actions"></colgroup><tr><td class="k">客户组织</td><td class="k">员工</td><td class="k">部门/团队</td><td class="k">状态</td><td class="k">模型分组</td><td></td></tr>${rows || `<tr><td colspan="6">${emptyState("☷", "还没有员工", "各客户组织开通员工后会汇总在这里", "", "")}</td></tr>`}</table></div>
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
    <div class="action-row"><div><b>成员详情</b><div class="mini">额度 / 已用 / 剩余 / 令牌数 / 档位。</div></div><button class="btn sm" onclick="openMemberDetail(${mid},'${jsstr(name)}')">查看</button></div>
    <div class="action-row"><div><b>修改显示名</b><div class="mini">用于报表、成员列表和排障识别。</div></div><button class="btn sm" onclick="openRename(${mid},'${jsstr(curName)}')">改名</button></div>
    <div class="action-row"><div><b>调整团队</b><div class="mini">影响团队维度用量统计与“授权到团队”的档位范围。</div></div><button class="btn sm" onclick="openChangeTeam(${mid},${curTid || 0})">调团队</button></div>
    <div class="action-row danger-line"><div><b>离职</b><div class="mini">停用其全部令牌,未用额度退回组织金库。临时停用请用“停用”。</div></div><button class="btn sm danger" onclick="doOffboard(${mid},'${jsstr(name)}')">离职</button></div>
  </div>`, `<button class="btn pri" onclick="closeM()">关闭</button>`);
}
// 成员详情(33 §3.5 GET /members/:id:额度/剩余/令牌数/档位)。
async function openMemberDetail(mid, name) {
  try {
    const m = await api("GET", "/members/" + mid, null);
    const remain = rawOf(m, "remaining_raw"); // 40号 P2-1 统一契约名(单一名,契约测锁死)
    const used = rawOf(m, "used_raw");        // 已用 = max(0, granted−remaining) 实时派生(P1-1 单一口径)
    const granted = rawOf(m, "granted_raw");
    modal("成员详情 · " + name, `<table class="kvtable">
      <tr><td class="k">状态</td><td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td></tr>
      <tr><td class="k">档位</td><td>${esc(m.tier_name || "-")}</td></tr>
      <tr><td class="k">累计划入</td><td>${granted != null ? money(granted) : "-"}</td></tr>
      <tr><td class="k">已用</td><td>${used != null ? money(used) : "-"}</td></tr>
      <tr><td class="k">剩余额度</td><td><b>${remain != null ? money(remain) : "-"}</b></td></tr>
      <tr><td class="k">令牌数</td><td>${m.token_count != null ? m.token_count : "-"}</td></tr>
    </table><div class="note">成员令牌由成员本人在「我的令牌」自管;组织管理员不代建/代改/代删(29-PRD §3 铁律)。</div>`,
      `<button class="btn pri" onclick="closeM()">关闭</button>`);
  } catch (e) { toast(e.message); }
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
// 建成员(33 §3.5 POST /orgs/:id/members):tier_id 必填=初始额度;金库不足整单失败(409 如实展示,不半成功)。
async function openAddMember() {
  let tiers = [], teams = [], tiersErr = false;
  try { tiers = normList(await api("GET", "/orgs/" + S.orgId + "/tiers", null)); } catch (e) { tiersErr = true; toast("档位加载失败:" + e.message); }
  try { teams = (await api("GET", "/organizations/" + S.orgId + "/teams", null)) || []; } catch (e) {}
  const opts = tiers.map(t => `<option value="${t.id}">${esc(t.name)} · ${esc(tierQuotaLabel(t))}${t.is_default ? "(默认)" : ""}</option>`).join("");
  const topts = (teams || []).filter(t => t.status !== "archived").map(t => `<option value="${t.id}">${esc(t.name)}</option>`).join("");
  modal("开通成员", `<div class="fld"><label>姓名</label><input id="am_n" placeholder="钱晨"></div>
    <div class="fld"><label>登录邮箱(可选)</label><input id="am_e" placeholder="留空则自动生成"></div>
    <div class="fld"><label>团队(可选)</label><select id="am_team"><option value="">未分组</option>${topts}</select></div>
    <div class="fld"><label>初始额度档位(必选)</label><select id="am_t">${opts || `<option value="">${tiersErr ? "(档位加载失败,请关闭本窗重试)" : "(请先到「额度档位」建档位)"}</option>`}</select></div>
    <div class="note">开通即从组织金库为其划拨初始额度(所选档位额度);<b>金库余额不足会开通失败</b>,请先联系运营方充值。成员登录后在「我的令牌」自助创建 API 令牌。登录邮箱与初始密码开通后回显一次。</div>
    <div class="errline" id="am_err"></div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doAddMember()">开通成员</button>`);
}
async function doAddMember() {
  const name = val("am_n").trim();
  const tid = parseInt(val("am_t"), 10) || 0;
  if (!name) { setErr("am_err", "请填写姓名"); return; }
  if (!tid) { setErr("am_err", "必须选择初始额度档位(决定初始划拨额度)"); return; }
  try {
    const body = { name, tier_id: tid };
    const em = val("am_e").trim(); if (em) body.email = em; // 留空后端自动生成
    const tm = val("am_team"); if (tm) body.team_id = parseInt(tm, 10);
    const d = await api("POST", "/orgs/" + S.orgId + "/members", body);
    modal("已开通 · 交付登录凭证", `<div class="note">以下凭证仅此一次显示,请交付成员本人,首次登录后请改密:</div>
      <table class="kvtable">
        <tr><td class="k">登录邮箱</td><td>${esc(d.login_email)}</td></tr>
        <tr><td class="k">初始密码</td><td><b>${esc(d.initial_password)}</b></td></tr>
        <tr><td class="k">API 令牌</td><td class="mini">成员登录后在「我的令牌」自助创建(分组限被授权范围)</td></tr>
      </table>`,
      `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { setErr("am_err", e.message); toast(e.message); } // 金库不足等 409 原文展示
}
// 追加划拨(33 §3.5 POST /members/:id/quota:grant):美元输入 → ×quota_per_unit 转 raw;走 Transfer 守恒记账。
async function openGrant(mid, name) {
  const bal = await api("GET", "/orgs/" + S.orgId + "/balance", null).catch(() => null);
  const tre = bal ? rawOf(bal, "treasury_raw", "treasury_quota_raw") : null;
  modal("划拨额度 · " + name, `
    ${tre != null ? `<div class="note" style="margin-top:0">组织金库当前余额:<b>${money(tre)}</b></div>` : ""}
    <div class="fld"><label>划拨金额(美元)</label><input id="gr_a" type="number" min="0" step="0.01" placeholder="10"></div>
    <div class="fld"><label>原因(可选)</label><input id="gr_r" placeholder="赶项目临时追加"></div>
    <div class="note">从组织金库划拨到该成员额度(金库减、成员加,每笔入分配账本);金库余额不足会失败。</div>
    <div class="errline" id="gr_err"></div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doGrant(${mid})">确认划拨</button>`);
}
async function doGrant(mid) {
  const usd = parseFloat(val("gr_a"));
  if (!(usd > 0)) { setErr("gr_err", "金额必须为正数"); return; }
  try {
    await api("POST", "/members/" + mid + "/quota:grant", { amount_raw: usd2raw(usd), reason: val("gr_r").trim() || "manual_grant" });
    closeM(); toast("已划拨 " + money(usd2raw(usd))); renderView();
  } catch (e) { setErr("gr_err", e.message); toast(e.message); }
}
function openRename(mid, cur) {
  modal("修改成员显示名", `<div class="fld"><label>显示名</label><input id="rn_n" value="${esc(cur)}" placeholder="张三"></div>
    <div class="note">随机名可改成真实姓名,报表与列表随之更新。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doRename(${mid})">保存</button>`);
}
async function doRename(mid) {
  try { await api("PATCH", "/members/" + mid, { display_name: val("rn_n") }); closeM(); toast("已改名"); renderView(); }
  catch (e) { toast(e.message); }
}
async function toggleMember(mid, enable) {
  // 架构B:停用 = disable 成员 user(全部令牌近实时暂停,~60s TTL),额度保留;非 override(31-ADR §4.5)。
  if (!enable) {
    return dangerConfirm("确认停用成员?", `<p>停用后该成员的<b>全部令牌</b>会暂停调用(约 1 分钟内生效),额度保留不动。</p>
      <p>重新启用后即恢复调用。若成员已离职,请使用“离职”(会退回未用额度)。</p>`,
      "确认停用", `doToggleMember(${mid},false)`);
  }
  return doToggleMember(mid, true);
}
async function doToggleMember(mid, enable) {
  try { await api("POST", "/members/" + mid + "/status", { enabled: enable }); closeM(); toast(enable ? "已启用(令牌恢复调用)" : "已停用(全部令牌暂停,额度保留)"); renderView(); }
  catch (e) { toast(e.message); }
}
// 离职(涉钱,危险):disable-first → 静默 → 未用额度退回金库(33 §3.2 OffboardMember)。
async function doOffboard(mid, name) {
  return dangerConfirm("确认成员离职?", `<p>确认让「${esc(name)}」离职?</p>
    <p>离职会:<b>停用其全部令牌</b>(调用近实时失效)→ 将其<b>未用完的额度退回组织金库</b>(每笔入分配账本)。</p>
    <p>资料与历史记录保留;之后可“恢复入职”,但需重新选择档位、从金库重新划拨额度(不自动恢复原额度)。</p>
    <p>若只是临时停用,请使用“停用”。</p>`, "确认离职", `doOffboardConfirmed(${mid})`);
}
async function doOffboardConfirmed(mid) {
  try { await api("POST", "/members/" + mid + "/offboard", null); closeM(); toast("已离职(令牌已停用,未用额度退回金库)"); renderView(); }
  catch (e) { toast(e.message); }
}
// 重试开通(42号 P1:POST /members/:id/provision:retry)——金库补钱后自助救活开通失败的成员。
async function doRetryProvision(mid, name) {
  if (!confirm("重试为「" + name + "」开通?将按其档位从金库重新划拨初始额度(金库不足会失败)。")) return;
  try { await api("POST", "/members/" + mid + "/provision:retry", null); toast("重试开通成功"); renderView(); }
  catch (e) { toast("重试开通失败:" + e.message); }
}
// 恢复入职(33 §3.5 POST /members/:id/restore {tier_id}):跟新建一样重新分配额度(离职已退额,不自动恢复)。
async function openRestore(mid, name) {
  let tiers = [], tiersErr = false;
  try { tiers = normList(await api("GET", "/orgs/" + S.orgId + "/tiers", null)); } catch (e) { tiersErr = true; toast("档位加载失败:" + e.message); }
  const opts = tiers.map(t => `<option value="${t.id}">${esc(t.name)} · ${esc(tierQuotaLabel(t))}</option>`).join("");
  modal("恢复入职 · " + name, `<div class="fld"><label>额度档位(必选)</label><select id="rs_t">${opts || `<option value="">${tiersErr ? "(档位加载失败,请关闭本窗重试)" : "(请先到「额度档位」建档位)"}</option>`}</select></div>
    <div class="note">离职时额度已退回金库;恢复 = 跟新建一样,按所选档位从金库<b>重新划拨</b>额度。金库余额不足会失败。</div>
    <div class="errline" id="rs_err"></div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doRestore(${mid})">确认恢复</button>`);
}
async function doRestore(mid) {
  const tid = parseInt(val("rs_t"), 10) || 0;
  if (!tid) { setErr("rs_err", "请选择额度档位"); return; }
  try { await api("POST", "/members/" + mid + "/restore", { tier_id: tid }); closeM(); toast("已恢复入职(额度已按档位重新划拨)"); renderView(); }
  catch (e) { setErr("rs_err", e.message); toast(e.message); }
}
// 运营方:组织列表"金库"入口(GET /orgs/:id/balance 读求和实时值;operator 只看,不代充)。
async function openOrgTreasury(id, name) {
  try {
    const b = await api("GET", "/orgs/" + id + "/balance", null);
    const tre = rawOf(b, "treasury_raw", "treasury_quota_raw");
    const msum = rawOf(b, "members_total_raw", "members_sum_raw", "members_quota_raw");
    const totalRaw = rawOf(b, "total_raw");
    const total = totalRaw != null ? totalRaw : ((tre != null && msum != null) ? tre + msum : null);
    modal("组织金库 · " + name, `<table class="kvtable">
      <tr><td class="k">金库余额</td><td><b>${tre != null ? money(tre) : "-"}</b></td></tr>
      <tr><td class="k">成员额度合计</td><td>${msum != null ? money(msum) : "-"}</td></tr>
      <tr><td class="k">组织总余额</td><td>${total != null ? money(total) : "-"}</td></tr></table>
      <div class="note">读求和实时读 new-api(走 DB 不读缓存)。代充请在 new-api 侧对金库 user <b>增量代充</b>,单次不超 $4294(int32 上限,33 §8 运维注记)。</div>`,
      `<button class="btn" onclick="closeM();S.pledgerOrg='${jsstr(String(id))}';S.pledgerPage=1;go('pledger')">查看该组织流水</button><button class="btn pri" onclick="closeM()">关闭</button>`);
  } catch (e) { toast(e.message); }
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
/* ---------- 架构B 档位(31-ADR §5 / 33 §3.5):额度型 + 额度 + 周期 + 分组 + 可用模型;授权到成员/团队 ---------- */
const periodCN = p => ({ daily: "每日", weekly: "每周", monthly: "每月" })[p] || (p || "-");
function tierQuotaLabel(t) {
  const amt = money(rawOf(t, "amount_raw") || 0);
  return t.quota_type === "subscription" ? ("订阅 " + amt + " / " + periodCN(t.reset_period)) : ("固定 " + amt);
}
function grantLabel(g, members, teams) {
  const tt = g.target_type || g.type || "";
  if (tt === "all") return "全员";
  if (tt === "team") { const t = (teams || S.teamsCache || []).find(x => x.id === Number(g.target_id)); return "团队:" + (g.target_name || (t && t.name) || ("#" + g.target_id)); }
  return "成员:" + (g.target_name || ("#" + g.target_id));
}
function grantChips(t) {
  const gs = t.grants || [];
  if (!gs.length) return '<span class="mini">未授权</span>';
  return gs.slice(0, 3).map(g => `<span class="tag">${esc(grantLabel(g))}</span>`).join("") + (gs.length > 3 ? `<span class="mini"> 等 ${gs.length} 条</span>` : "");
}
VIEWS.tiers = async () => {
  const d = normList(await api("GET", "/orgs/" + S.orgId + "/tiers", null));
  S.tiersCache = d; // 供编辑/授权弹窗回填
  // 分组下拉数据源 = 组织模型广场(GET /orgs/:id/marketplace,分组+倍率;33 §12 增补②)。
  S.orgGroupsErr = false;
  try { S.orgGroups = marketGroups(await api("GET", "/orgs/" + S.orgId + "/marketplace", null)); } catch (e) { S.orgGroups = []; S.orgGroupsErr = true; toast("分组加载失败:" + e.message); }
  const rows = d.map(t => {
    const sub = t.quota_type === "subscription";
    return `<tr><td>${esc(t.name)}${t.is_default ? ' <span class="tag">默认</span>' : ""}</td>
      <td>${sub ? pill("订阅 · " + periodCN(t.reset_period), "ok") : pill("固定/单次", "mut")}</td>
      <td class="right"><b>${money(rawOf(t, "amount_raw") || 0)}</b>${sub ? `<div class="mini">${periodCN(t.reset_period)}补满到该值</div>` : ""}</td>
      <td>${t.newapi_group ? `<span class="group-badge" title="${esc(t.newapi_group)}">${esc(t.newapi_group)}</span>` : '<span class="mini">-</span>'}</td>
      <td>${(t.model_limits || t.model_set || []).map(m => `<span class="mcap">${esc(m)}</span>`).join("") || '<span class="mini">分组内全部模型</span>'}</td>
      <td>${grantChips(t)}</td>
      <td class="right table-actions">
        <span class="btn sm" onclick="openTierGrants(${t.id},'${jsstr(t.name)}')">授权</span>
        <span class="btn sm" onclick="openEditTier(${t.id})">编辑</span>
        <span class="btn sm danger" onclick="doDeleteTier(${t.id},'${jsstr(t.name)}')">删除</span></td></tr>`;
  }).join("");
  return head("额度档位", "档位 = 额度型 + 额度 + 周期 + 分组 + 可用模型;开通 / 恢复成员时按档位从金库划拨额度")
    + `<div class="tier-intro">
        <div class="tier-intro-t">档位是什么</div>
        <div class="tier-intro-b">档位是组织给成员配额度的<b>套餐</b>:【固定/单次】一次划一笔,用完组织再给;【订阅/周期】每日/每周/每月把成员额度<b>补满到目标值</b>(没用完不累积,组织只付实际消耗)。额度必设、必须正数、<b>没有“无上限”</b>——它就是成员刷不掉的硬限额。分组 + 可用模型决定成员建令牌时能选的范围;档位需先<b>授权</b>到成员或团队,成员建令牌的分组只能从被授权档位里选。</div>
      </div>
      <div class="toolbar"><button class="btn pri" onclick="openCreateTier()">+ 新建档位</button></div>
    <div class="panel"><div class="table-scroll"><table class="tier-table"><colgroup><col class="t-name"><col class="t-type"><col class="t-amount"><col class="t-group"><col class="t-models"><col class="t-grants"><col class="t-actions"></colgroup><thead><tr><th>档位</th><th>额度型</th><th class="right">额度</th><th>分组</th><th>可用模型</th><th>已授权</th><th></th></tr></thead><tbody>${rows || `<tr><td colspan=7>${emptyState("◆", "还没有额度档位", "先建档位(额度型+额度+分组+模型),开通成员时必选一个作为初始额度", "+ 新建档位", "openCreateTier()")}</td></tr>`}</tbody></table></div></div>`;
};
// 档位表单(建/编辑共用):额度型二选一(无无上限)、额度美元必填正数、周期仅订阅、分组=组织广场分组、可用模型可选。
function tierFormHTML(p, t) {
  t = t || {};
  const sub = t.quota_type === "subscription";
  const amtRaw = rawOf(t, "amount_raw");
  const gopts = [`<option value="">${S.orgGroupsErr ? "(分组加载失败,请刷新页面重试)" : "(请选择分组)"}</option>`].concat(
    (S.orgGroups || []).map(g => `<option value="${esc(g.group)}"${g.group === (t.newapi_group || "") ? " selected" : ""}>${esc(g.group)}${g.ratio != null ? `(倍率 ${esc(String(g.ratio))})` : ""}</option>`)).join("");
  return `<div class="fld"><label>档位名称</label><input id="${p}_n" value="${esc(t.name || "")}" placeholder="标准档"></div>
    <div class="fld"><label>额度型(二选一,无“无上限”)</label><select id="${p}_qt" onchange="document.getElementById('${p}_rpwrap').style.display=this.value==='subscription'?'':'none'">
      <option value="fixed"${!sub ? " selected" : ""}>固定/单次(用完组织再给)</option>
      <option value="subscription"${sub ? " selected" : ""}>订阅/周期(到期补满到目标,不累积)</option></select></div>
    <div class="fld"><label>额度(美元,必填、正数;即成员硬限额)</label><input id="${p}_a" type="number" min="0" step="0.01" value="${amtRaw != null ? (amtRaw / (S.qpu || 500000)) : ""}" placeholder="150"></div>
    <div id="${p}_rpwrap" style="display:${sub ? "" : "none"}"><div class="fld"><label>重置周期(自然边界,按组织时区)</label><select id="${p}_rp">
      ${["daily", "weekly", "monthly"].map(x => `<option value="${x}"${(t.reset_period || "monthly") === x ? " selected" : ""}>${periodCN(x)}</option>`).join("")}</select></div></div>
    <div class="fld"><label>分组(决定计价档与可用模型范围)</label><select id="${p}_g">${gopts}</select></div>
    <div class="fld"><label>可用模型(逗号分隔,留空=分组内全部模型)</label><input id="${p}_m" value="${esc((t.model_set || []).join(","))}" placeholder="gpt-4o,claude-sonnet-4-5"></div>
    <div class="errline" id="${p}_err"></div>`;
}
function readTierForm(p) {
  if (!val(p + "_n").trim()) return { err: "请填写档位名称" };
  const usd = parseFloat(val(p + "_a"));
  if (!(usd > 0)) return { err: "额度必须为正数(0 = 没有额度不能用,不是无限,不能填 0)" };
  const g = val(p + "_g");
  if (!g) return { err: "请选择分组" };
  const qt = val(p + "_qt");
  const body = { // 45号 P1-1:字段名=后端契约 newapi_group/model_set(曾提交 group/model_limits 必 400)
    name: val(p + "_n").trim(), quota_type: qt, amount_raw: usd2raw(usd), newapi_group: g,
    model_set: val(p + "_m").split(",").map(s => s.trim()).filter(Boolean),
  };
  if (qt === "subscription") body.reset_period = val(p + "_rp");
  return { body };
}
function openCreateTier() {
  modal("新建档位", tierFormHTML("ti") + `<div class="note">建档只是存套餐模板,此刻不动钱;开通/恢复成员选中它时才从金库划拨。成员额度受平台成员帽约束(默认 $1000,超帽会被拒)。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateTier()">创建</button>`);
}
async function doCreateTier() {
  const r = readTierForm("ti"); if (r.err) { setErr("ti_err", r.err); return; }
  try { await api("POST", "/orgs/" + S.orgId + "/tiers", r.body); closeM(); toast("已创建"); renderView(); }
  catch (e) { setErr("ti_err", e.message); }
}
function openEditTier(tid) {
  const t = (S.tiersCache || []).find(x => x.id === tid); if (!t) return;
  modal("编辑档位", tierFormHTML("te", t) + `<div class="note">订阅档改额度/周期,自下个周期起按新目标补满;固定档改额度不追溯已划拨成员(需要再给用「划拨额度」)。改分组会影响该档位成员建令牌时可选的分组。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doEditTier(${tid})">保存</button>`);
}
async function doEditTier(tid) {
  const r = readTierForm("te"); if (r.err) { setErr("te_err", r.err); return; }
  try { await api("PUT", "/tiers/" + tid, r.body); closeM(); toast("已保存"); renderView(); }
  catch (e) { setErr("te_err", e.message); }
}
async function doDeleteTier(tid, name) {
  dangerConfirm("确认删除档位?", `<p>确认删除档位「${esc(name)}」?</p><p>被成员引用或为默认档将无法删除;删除不回收已划拨给成员的额度。</p>`,
    "确认删除", `doDeleteTierConfirmed(${tid})`);
}
async function doDeleteTierConfirmed(tid) {
  try { await api("DELETE", "/tiers/" + tid, null); closeM(); toast("已删除"); renderView(); }
  catch (e) { toast(e.message); }
}
// 档位授权(33 §3.5 POST/DELETE /tiers/:id/grants,target=member/team):全员 / 指定成员 / 指定团队。
// 注:现有授权清单随 tier.grants 带出、"全员"target_type=all —— 两处契约未明写,已记交付说明待组长裁定。
async function openTierGrants(tid, name) {
  const t = (S.tiersCache || []).find(x => x.id === tid) || {};
  let members = [], teams = S.teamsCache || [];
  try { members = (await api("GET", "/orgs/" + S.orgId + "/members?page=1&page_size=200", null)).list || []; } catch (e) {}
  if (!teams.length) { try { teams = (await api("GET", "/organizations/" + S.orgId + "/teams", null)) || []; } catch (e) {} }
  const grants = t.grants || [];
  const cur = grants.length
    ? grants.map(g => `<span class="tag">${esc(grantLabel(g, members, teams))} <span class="lk" onclick="doDeleteGrant(${tid},${Number(g.grant_id) || 0})" title="取消该授权">×</span></span>`).join(" ")
    : '<span class="mini">尚未授权;未被授权的成员不能被配到此档位、也选不到它的分组</span>';
  const mopts = members.filter(m => m.status !== "offboarded" && m.role !== "org_admin" && m.role !== "operator").map(m => `<option value="${m.id}">${esc(m.display_name || m.login_email)}</option>`).join("");
  const topts = (teams || []).filter(x => x.status !== "archived").map(x => `<option value="${x.id}">${esc(x.name)}</option>`).join("");
  modal("档位授权 · " + name, `
    <div class="fld"><label>已授权</label><div>${cur}</div></div>
    <div class="fld"><label>授权对象</label><select id="tg_type" onchange="document.getElementById('tg_mwrap').style.display=this.value==='member'?'':'none';document.getElementById('tg_twrap').style.display=this.value==='team'?'':'none'">
      <option value="member">指定成员</option><option value="team">指定团队</option><option value="all">全员</option></select></div>
    <div class="fld" id="tg_mwrap"><label>成员</label><select id="tg_m">${mopts || '<option value="">(暂无成员)</option>'}</select></div>
    <div class="fld" id="tg_twrap" style="display:none"><label>团队</label><select id="tg_t">${topts || '<option value="">(暂无团队)</option>'}</select></div>
    <div class="note">成员建令牌时,分组<b>只能</b>从被授权档位覆盖的分组里选(31-ADR §5 护栏);成员可选的档位 = 被授权的并集。</div>
    <div class="errline" id="tg_err"></div>`,
    `<button class="btn" onclick="closeM()">关闭</button><button class="btn pri" onclick="doAddGrant(${tid})">添加授权</button>`);
}
async function doAddGrant(tid) {
  const type = val("tg_type");
  const body = { target_type: type };
  if (type === "member") body.target_id = parseInt(val("tg_m"), 10) || 0;
  if (type === "team") body.target_id = parseInt(val("tg_t"), 10) || 0;
  if (type !== "all" && !body.target_id) { setErr("tg_err", "请选择授权对象"); return; }
  try { await api("POST", "/tiers/" + tid + "/grants", body); closeM(); toast("已授权"); renderView(); }
  catch (e) { setErr("tg_err", e.message); }
}
async function doDeleteGrant(tid, gid) { // 45号 P1-2:后端契约=grant_id(曾读 g.id 恒 0 落兜底 body 必 400)
  if (!(gid > 0)) { toast("授权记录缺 grant_id,请刷新后重试"); return; }
  try { await api("DELETE", "/tiers/" + tid + "/grants", { grant_id: gid }); closeM(); toast("已取消授权"); renderView(); }
  catch (e) { toast(e.message); }
}
/* ---------- 架构B 余额与账本(31-ADR §15):金库 + 各成员额度 + 划拨流水;方向绿=金库→成员入账、橙=退额 ---------- */
function ledgerReasonCN(r) {
  return ({
    initial: "初始额度", initial_grant: "初始额度", grant: "追加划拨", manual_grant: "追加划拨",
    topup: "订阅补满", subscription_topup: "订阅补满", offboard_refund: "离职退额", refund: "退额",
    restore: "恢复分配", restore_grant: "恢复分配", reconcile: "对账补齐",
  })[String(r || "").toLowerCase()] || (r || "-");
}
// 方向:后端权威枚举 credit(金库→成员 入账)/ debit(成员→金库 退额),45号 P2-3 修正
// (曾不认 debit → 离职退额画成绿色入账);枚举已补进 33号契约。空串才走 reason 兜底。
function ledgerDir(x) {
  const d = String(x.direction || "").toLowerCase();
  if (d === "credit") return "in";
  if (d === "debit") return "out";
  if (d) return (d === "refund" || d === "out" || d === "member_to_org" || d === "to_treasury") ? "out" : "in";
  const r = String(x.reason || "").toLowerCase();
  if (r.indexOf("refund") >= 0 || r.indexOf("offboard") >= 0 || r.indexOf("退") >= 0) return "out";
  if (Number(rawOf(x, "amount_raw")) < 0) return "out";
  return "in";
}
function ledgerStatusPill(s) {
  s = String(s || "applied");
  return s === "applied" ? pill("已入账", "ok") : s === "pending" ? pill("处理中", "warn") : pill("失败", "bad");
}
// ledgerRows:三视角共用流水行。view=org(组织流水)/me(成员到账)/platform(全平台)。
function ledgerRows(list, view) {
  const cols = view === "platform" ? 7 : view === "me" ? 5 : 6;
  if (!list || !list.length) return `<tr><td colspan="${cols}" class="empty">暂无划拨流水</td></tr>`;
  return list.map(x => {
    const dir = ledgerDir(x);
    const amt = Math.abs(Number(rawOf(x, "amount_raw")) || 0);
    const who = x.member_name || x.member_display_name || (x.member_id ? "成员#" + x.member_id : "-");
    const dirHtml = view === "me"
      ? (dir === "in" ? `<span class="ldg-in">到账(金库 → 我)</span>` : `<span class="ldg-out">退回金库</span>`)
      : (dir === "in" ? `<span class="ldg-in">金库 → 成员</span>` : `<span class="ldg-out">成员 → 金库</span>`);
    const amtHtml = dir === "in" ? `<span class="ldg-in">+${money(amt)}</span>` : `<span class="ldg-out">-${money(amt)}</span>`;
    const cells = [`<td class="mini">${fmtTs(x.created_at)}</td>`];
    if (view === "platform") cells.push(`<td><span class="cell-clip" title="${esc(x.org_name || ("组织#" + (x.org_id || "-")))}">${esc(x.org_name || ("组织#" + (x.org_id || "-")))}</span></td>`);
    if (view !== "me") cells.push(`<td><span class="cell-clip" title="${esc(who)}">${esc(who)}</span></td>`);
    cells.push(`<td>${dirHtml}</td>`, `<td class="right">${amtHtml}</td>`, `<td class="mini">${esc(ledgerReasonCN(x.reason))}</td>`, `<td>${ledgerStatusPill(x.status)}</td>`);
    return `<tr>${cells.join("")}</tr>`;
  }).join("");
}
function goLedgerPage(p) { S.ledgerPage = Math.max(1, p); renderView(); }
VIEWS.billing = async () => {
  const id = S.orgId;
  const page = S.ledgerPage || 1;
  const [bal, led] = await Promise.all([
    api("GET", "/orgs/" + id + "/balance", null).catch(() => ({ __err: true })),
    api("GET", "/orgs/" + id + "/ledger?page=" + page + "&page_size=50", null).catch(() => ({ list: [], pagination: {} })),
  ]);
  const tre = bal.__err ? null : rawOf(bal, "treasury_raw", "treasury_quota_raw", "available_quota");
  const msum = bal.__err ? null : rawOf(bal, "members_total_raw", "members_sum_raw", "members_quota_raw");
  const totalRaw = bal.__err ? null : rawOf(bal, "total_raw");
  const total = totalRaw != null ? totalRaw : ((tre != null && msum != null) ? tre + msum : null);
  const low = !bal.__err && (bal.low === true || bal.treasury_low === true);
  const lowBar = low ? `<div class="note" style="background:var(--warnbg);border-color:#fde68a;color:#92400e;margin:0 0 14px"><b>组织余额不足,请联系运营方充值。</b>金库偏低时,开通成员 / 划拨额度 / 订阅补满可能失败。</div>` : "";
  // 各成员额度:balance 若带 members 明细直接用,否则回退成员列表端点(同一 *_raw 口径)。
  let mlist = (!bal.__err && (bal.members || bal.member_quotas)) || null;
  if (!mlist) { try { mlist = (await api("GET", "/orgs/" + id + "/members?page=1&page_size=50", null)).list || []; } catch (e) { mlist = []; } }
  const mrows = (mlist || []).filter(m => m.role !== "org_admin" && m.role !== "operator").map(m => {
    const remain = rawOf(m, "remaining_raw"); // 40号 P2-1/P2-2:统一契约名
    const used = rawOf(m, "used_raw");
    const nm = m.display_name || m.name || m.login_email || ("成员#" + (m.member_id || m.id));
    return `<tr><td><span class="cell-clip" title="${esc(nm)}">${esc(nm)}</span></td>
      <td>${pill(memberStatusCN(m.status), m.status === "active" ? "ok" : "mut")}</td>
      <td class="right">${used != null ? money(used) : "-"}</td>
      <td class="right"><b>${remain != null ? money(remain) : "-"}</b></td></tr>`;
  }).join("");
  const list = led.list || led.items || [];
  const ltotal = (led.pagination || led.page || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(ltotal / 50));
  return head("余额与账本", "组织金库 + 各成员额度(读求和实时值)· 每笔金库↔成员划拨都有流水")
    + lowBar
    + `<div class="cards">
      ${kpi("金库余额", tre != null ? money(tre) : "暂不可用", "组织的钱池子;充值请联系运营方")}
      ${kpi("成员额度合计", msum != null ? money(msum) : "-", "已划到各成员、尚未消耗的额度")}
      ${kpi("组织总余额", total != null ? money(total) : "-", "金库 + 成员额度合计")}
    </div>
    <div class="panel"><div class="ph">各成员额度</div><div class="pb"><div class="table-scroll"><table class="kvtable"><tr><td class="k">成员</td><td class="k">状态</td><td class="right k">已用</td><td class="right k">剩余额度</td></tr>${mrows || `<tr><td colspan="4" class="empty">暂无成员</td></tr>`}</table></div><div class="mini" style="margin-top:8px">此处最多显示 50 人;完整成员额度管理与翻页见「成员」页。</div></div></div>
    <div class="panel"><div class="ph">划拨流水(分配账本)</div><div class="pb">
      <div class="table-scroll"><table class="kvtable"><tr><td class="k">时间</td><td class="k">成员</td><td class="k">方向</td><td class="right k">金额</td><td class="k">原因</td><td class="k">状态</td></tr>${ledgerRows(list, "org")}</table></div>
      <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${ltotal} 笔 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goLedgerPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goLedgerPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
    </div></div>`;
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
/* ===================== 架构B 成员自助:我的令牌 / 额度与账本 / 模型广场 ===================== */
// marketGroups:规整模型广场返回(容错数组或 {groups}/{list};模型项容错字符串或对象)。倍率照 new-api 普通用户可见口径展示。
function marketGroups(d) {
  if (!d) return [];
  const arr = Array.isArray(d) ? d : (d.groups || d.list || []);
  return arr.map(g => ({
    group: g.group || g.name || "",
    ratio: g.ratio != null ? g.ratio : g.group_ratio,
    desc: g.desc || g.description || "",
    models: (g.models || []).map(m => (typeof m === "string" ? m : (m.model_name || m.name || m.model || ""))).filter(Boolean),
  })).filter(g => g.group);
}
// 我的令牌(29-PRD §4.4,交互对齐 new-api 令牌管理):成员像 new-api 用户一样自管多令牌。
// 端点:GET/POST /me/tokens、PATCH/DELETE /me/tokens/:id、POST /me/tokens/:id/key:reveal;分组下拉=GET /me/marketplace。
VIEWS.mytokens = async () => {
  const page = S.tokPage || 1;
  S.myGroupsErr = false;
  const [d, mk] = await Promise.all([
    api("GET", "/me/tokens?page=" + page + "&page_size=50", null),
    api("GET", "/me/marketplace", null).catch((e) => { S.myGroupsErr = true; toast("分组加载失败:" + (e && e.message || "")); return null; }),
  ]);
  S.myGroups = marketGroups(mk);
  const list = d.list || d.items || (Array.isArray(d) ? d : []);
  S.tokCache = list;
  const total = (d.pagination || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(total / 50));
  // 令牌数上限(默认 2,超管可配):优先取 /me/tokens 返回值,回退 /me 附带值;取不到不前端拦(后端 count+insert 事务兜底)。
  const limit = Number(d.token_limit != null ? d.token_limit : (d.limit != null ? d.limit : (S.me && S.me.member_token_limit))) || 0;
  const atCap = limit > 0 && total >= limit;
  const rows = list.map(t => {
    const nm = t.name || "-";
    const quota = rawOf(t, "quota_raw");   // 令牌维度契约名(MyTokenView,40号 P2-1 同批清多名兜底)
    const remain = rawOf(t, "remain_raw");
    const usedP = rawOf(t, "period_used_raw");
    const on = !(t.status === "disabled" || t.status === 2 || t.enabled === false);
    return `<tr>
      <td><span class="cell-clip" title="${esc(nm)}">${esc(nm)}</span></td>
      <td class="mini key-copy"><span class="cell-clip" title="${esc(t.key_masked || "-")}">${esc(t.key_masked || "-")}</span><button class="btn micro reveal-key" onclick="revealTokenKey(${t.id},'${jsstr(nm)}')">复制完整</button></td>
      <td><span class="group-badge" title="${esc(t.group || "-")}">${esc(t.group || "-")}</span></td>
      <td class="right">${quota != null ? money(quota) : '<span class="mini">随成员额度</span>'}</td>
      <td class="right">${remain != null ? money(remain) : "-"}</td>
      <td class="mini"><span class="cell-clip" title="${esc(t.allow_ips || "")}">${esc(t.allow_ips || "不限")}</span></td>
      <td>${on ? pill("启用", "ok") : pill("停用", "mut")}</td>
      <td class="right">${usedP != null ? money(usedP) : "-"}</td>
      <td class="right table-actions">
        <span class="btn sm" onclick="openEditToken(${t.id})">编辑</span>
        <span class="btn sm danger" onclick="doDeleteToken(${t.id},'${jsstr(nm)}')">删除</span></td></tr>`;
  }).join("");
  const capNote = atCap
    ? `<span class="mini" style="color:var(--warn)">已达令牌数上限(${total}/${limit});删除旧令牌后才能新建,需更高上限请联系管理员</span>`
    : (limit > 0 ? `<span class="mini">令牌数 ${total}/${limit}</span>` : "");
  const pager = total > 50
    ? `<div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 个 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goTokPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goTokPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>` : "";
  return head("我的令牌", "自管多个 API 令牌(交互对齐 new-api);分组只能选被授权范围;Key 不可改,换 Key = 删了重建")
    + `<div class="toolbar">${capNote}<div class="spacer"></div><button class="btn pri" ${atCap ? "disabled" : ""} onclick="openCreateToken()">+ 新建令牌</button></div>
    <div class="panel"><div class="table-scroll"><table class="kvtable token-self-table"><colgroup><col class="ts-name"><col class="ts-key"><col class="ts-group"><col class="ts-quota"><col class="ts-remain"><col class="ts-ip"><col class="ts-status"><col class="ts-used"><col class="ts-actions"></colgroup><tr><td class="k">令牌名</td><td class="k">Key</td><td class="k">分组</td><td class="right k">令牌额度</td><td class="right k">剩余</td><td class="k">IP 白名单</td><td class="k">状态</td><td class="right k">本期用量</td><td class="right k"></td></tr>${rows || `<tr><td colspan="9">${emptyState("⚿", "还没有令牌", "创建你的第一个 API 令牌;分组只能从被授权的档位分组里选", atCap ? "" : "+ 新建令牌", "openCreateToken()")}</td></tr>`}</table></div>${pager}</div>`;
};
function goTokPage(p) { S.tokPage = Math.max(1, p); renderView(); }
// 令牌表单(建/编辑共用):分组下拉只列被授权分组(GET /me/marketplace);编辑不含名称与 Key(PATCH 契约仅 分组/额度/IP)。
function tokenFormHTML(p, t) {
  t = t || {};
  const q = rawOf(t, "quota_raw"); // 令牌维度契约名
  const gopts = (S.myGroups || []).map(g => `<option value="${esc(g.group)}"${g.group === (t.group || "") ? " selected" : ""}>${esc(g.group)}${g.ratio != null ? `(倍率 ${esc(String(g.ratio))})` : ""}</option>`).join("");
  return `${t.id ? "" : `<div class="fld"><label>令牌名</label><input id="${p}_n" placeholder="my-dev-key"></div>`}
    <div class="fld"><label>分组(仅列你被授权的分组)</label><select id="${p}_g">${gopts || `<option value="">${S.myGroupsErr ? "(分组加载失败,请刷新重试)" : "(暂无被授权分组,请联系管理员配置档位授权)"}</option>`}</select></div>
    <div class="fld"><label>令牌额度(美元,可选;留空=不单独限额,仅受你的成员总额度约束)</label><input id="${p}_q" type="number" min="0" step="0.01" value="${q != null ? (q / (S.qpu || 500000)) : ""}"></div>
    <div class="fld"><label>IP 白名单(可选,逗号分隔;留空=不限)</label><input id="${p}_ip" value="${esc(t.allow_ips || "")}" placeholder="1.2.3.4,10.0.0.0/8"></div>
    <div class="errline" id="${p}_err"></div>`;
}
function openCreateToken() {
  modal("新建令牌", tokenFormHTML("tk") + `<div class="note">创建后 Key 仅可查看/复制、<b>不可修改</b>;换 Key = 删除后重建。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateToken()">创建</button>`);
}
async function doCreateToken() {
  const name = val("tk_n").trim();
  if (!name) { setErr("tk_err", "请填写令牌名"); return; }
  const g = val("tk_g");
  if (!g) { setErr("tk_err", "请选择分组(需管理员先把档位授权给你)"); return; }
  const body = { name, group: g };
  const qs = val("tk_q").trim();
  if (qs !== "") {
    const q = parseFloat(qs);
    if (!(q > 0)) { setErr("tk_err", "令牌额度须为正数,或留空表示不单独限额"); return; }
    body.quota_raw = usd2raw(q);
  }
  const ip = val("tk_ip").trim(); if (ip) body.allow_ips = ip;
  try {
    const d = await api("POST", "/me/tokens", body);
    closeM();
    const k = d && (d.key || d.api_key);
    if (k) {
      modal("令牌已创建", `<div class="note">请复制保存;之后也可在列表点“复制完整”再次获取。</div><div class="keybox"><span>${esc(k)}</span>${copyBtn(k, "API Key")}</div>`,
        `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
    } else { toast("已创建"); renderView(); }
  } catch (e) { setErr("tk_err", e.message); } // 上限满/分组越权等错误原文展示
}
function openEditToken(tid) {
  const t = (S.tokCache || []).find(x => x.id === tid); if (!t) return;
  modal("编辑令牌 · " + (t.name || ""), tokenFormHTML("tke", t) + `<div class="note">Key 不可修改;如需更换 Key,请删除该令牌后重建(名称亦不在可改范围)。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doEditToken(${tid})">保存</button>`);
}
async function doEditToken(tid) {
  const g = val("tke_g");
  if (!g) { setErr("tke_err", "请选择分组"); return; }
  const body = { group: g, allow_ips: val("tke_ip").trim() };
  const qs = val("tke_q").trim();
  if (qs !== "") {
    const q = parseFloat(qs);
    if (!(q > 0)) { setErr("tke_err", "令牌额度须为正数,或留空表示不单独限额"); return; }
    body.quota_raw = usd2raw(q);
  }
  try { await api("PATCH", "/me/tokens/" + tid, body); closeM(); toast("已保存"); renderView(); }
  catch (e) { setErr("tke_err", e.message); }
}
function doDeleteToken(tid, name) {
  dangerConfirm("确认删除令牌?", `<p>确认删除令牌「${esc(name)}」?</p><p>删除后该 Key <b>立即失效且不可恢复</b>,正在使用它的工具会调用失败。如只是暂时不用,可保留不动。</p>`,
    "确认删除", `doDeleteTokenConfirmed(${tid})`);
}
async function doDeleteTokenConfirmed(tid) {
  try { await api("DELETE", "/me/tokens/" + tid, null); closeM(); toast("已删除"); renderView(); }
  catch (e) { toast(e.message); }
}
// 额度与账本(成员):GET /me/balance(额度/已用/剩余)+ GET /me/ledger(给自己的到账记录,31-ADR §15)。
VIEWS.mybalance = async () => {
  const page = S.ledgerPage || 1;
  const [bal, led] = await Promise.all([
    api("GET", "/me/balance", null).catch(() => ({ __err: true })),
    api("GET", "/me/ledger?page=" + page + "&page_size=50", null).catch(() => ({ list: [], pagination: {} })),
  ]);
  const remain = bal.__err ? null : rawOf(bal, "remaining_raw"); // 40号 P2-1 统一契约名
  const used = bal.__err ? null : rawOf(bal, "used_raw");
  const grantedRaw = bal.__err ? null : rawOf(bal, "granted_raw");
  const granted = grantedRaw != null ? grantedRaw : ((remain != null && used != null) ? remain + used : null);
  const usedUp = remain != null && remain <= 0;
  const bar = usedUp ? `<div class="note" style="background:var(--badbg);border-color:#fecaca;color:#7f1d1d;margin:0 0 14px"><b>额度已用完,请联系管理员。</b>额度用完后你的所有令牌调用都会被拒绝;管理员追加划拨后即恢复。</div>` : "";
  const list = led.list || led.items || [];
  const ltotal = (led.pagination || led.page || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(ltotal / 50));
  return head("额度与账本", "你的额度 / 已用 / 剩余(实时),以及组织给你的每笔到账记录")
    + bar
    + `<div class="cards">
      ${kpi("累计划入", granted != null ? money(granted) : "-", "组织金库累计划拨给你的额度")}
      ${kpi("已用", used != null ? money(used) : "-", "按实际调用扣减")}
      ${kpi("剩余额度", remain != null ? money(remain) : "暂不可用", usedUp ? "额度已用完,请联系管理员" : "任一令牌消费都从这里扣")}
    </div>
    <div class="panel"><div class="ph">到账记录</div><div class="pb">
      <div class="table-scroll"><table class="kvtable"><tr><td class="k">时间</td><td class="k">方向</td><td class="right k">金额</td><td class="k">原因</td><td class="k">状态</td></tr>${ledgerRows(list, "me")}</table></div>
      <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${ltotal} 笔 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goLedgerPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goLedgerPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
    </div></div>`;
};
// 模型广场(29-PRD §4.6):org=组织可用范围(/orgs/:id/marketplace);member=被授权范围(/me/marketplace)。
// 倍率正常展示——照 new-api 普通用户可见口径(31-ADR §9 镜像原则)。
VIEWS.marketplace = async () => {
  const isMember = S.role === "member";
  const path = isMember ? "/me/marketplace" : "/orgs/" + S.orgId + "/marketplace";
  const groups = marketGroups(await api("GET", path, null));
  const panels = groups.map(g => `<div class="panel"><div class="ph">${esc(g.group)}<span class="mini" style="font-weight:400">计价倍率 ${g.ratio != null ? esc(String(g.ratio)) : "-"}</span></div>
    <div class="pb">${g.desc ? `<div class="mini" style="margin-bottom:8px">${esc(g.desc)}</div>` : ""}${g.models.map(m => `<span class="mcap">${esc(m)}</span>`).join("") || '<span class="mini">该分组暂无模型</span>'}</div></div>`).join("");
  return head("模型广场", isMember ? "你被授权的分组、模型与计价倍率(建令牌时只能选这些分组)" : "本组织可用的分组、模型与计价倍率(档位的分组从这里选)")
    + (panels || `<div class="panel"><div class="pb">${emptyState("▦", "暂无可用分组", isMember ? "还没有档位授权给你,请联系管理员" : "请联系运营方为组织配置可用分组", "", "")}</div></div>`);
};

/* ===================== 运营方:平台账本 / 平台设置(架构B) ===================== */
VIEWS.pledger = async () => {
  const page = S.pledgerPage || 1;
  const org = (S.pledgerOrg || "").trim();
  const q = new URLSearchParams({ page: String(page), page_size: "50" });
  if (org) q.set("org_id", org);
  const d = await api("GET", "/ledger?" + q.toString(), null);
  const list = d.list || d.items || [];
  const total = (d.pagination || d.page || {}).total || list.length;
  const maxPage = Math.max(1, Math.ceil(total / 50));
  return head("平台账本", "全平台划拨流水(金库 ↔ 成员);守恒审计与排障入口,保留不少于 1 年")
    + `<div class="toolbar log-filter"><input id="pl_org" class="fsel" value="${esc(org)}" placeholder="组织 ID" onkeydown="if(event.key==='Enter')applyPledgerFilter()">
      <button class="btn sm" onclick="applyPledgerFilter()">筛选</button>
      <button class="btn sm" onclick="S.pledgerOrg='';S.pledgerPage=1;renderView()">重置</button></div>
    <div class="panel"><div class="pb">
      <div class="table-scroll"><table class="kvtable"><tr><td class="k">时间</td><td class="k">组织</td><td class="k">成员</td><td class="k">方向</td><td class="right k">金额</td><td class="k">原因</td><td class="k">状态</td></tr>${ledgerRows(list, "platform")}</table></div>
      <div class="pager" style="justify-content:flex-end;margin-top:10px"><span class="mini">共 ${total} 笔 · 第 ${page}/${maxPage} 页</span>
        <button onclick="goPledgerPage(${page - 1})" ${page <= 1 ? "disabled" : ""}>上一页</button>
        <button onclick="goPledgerPage(${page + 1})" ${page >= maxPage ? "disabled" : ""}>下一页</button></div>
    </div></div>`;
};
function applyPledgerFilter() { S.pledgerOrg = val("pl_org").trim(); S.pledgerPage = 1; renderView(); }
function goPledgerPage(p) { S.pledgerPage = Math.max(1, p); renderView(); }
// 平台设置(33 §2-0033 / §3.5 GET/PUT /platform-settings):money_freeze 急停、令牌上限、成员帽、预警阈值;quota_per_unit 只读。
VIEWS.psettings = async () => {
  const ps = (await api("GET", "/platform-settings", null)) || {};
  S.psettings = ps;
  const qpu = Number(ps.quota_per_unit) || 0;
  if (qpu > 0) S.qpu = qpu;
  const freeze = ps.money_freeze === true || String(ps.money_freeze) === "true";
  const cap = rawOf(ps, "member_quota_cap_raw");
  const low = rawOf(ps, "treasury_low_watermark_raw");
  const freezePanel = `<div class="panel ${freeze ? "danger-zone" : ""}"><div class="ph">钱动作急停(money_freeze)</div><div class="pb">
    <div class="status-copy">当前状态:${freeze ? pill("已冻结", "bad") : pill("正常", "ok")}。开启后<b>全平台所有划拨动作立即拒绝</b>——建成员首笔划拨、追加划拨、订阅补满、离职退额、对账补齐写入,全部冻结(对账的读/检测/告警照常)。仅用于灰度资金事故一把止血,平时必须保持关闭。</div>
    ${freeze
      ? `<button class="btn pri" style="margin-top:12px" onclick="confirmMoneyFreeze(false)">解除急停(恢复划拨)</button>`
      : `<button class="btn danger danger-solid" style="margin-top:12px" onclick="confirmMoneyFreeze(true)">开启急停(冻结所有钱动作)</button>`}
  </div></div>`;
  return head("平台设置", "平台级配置(KV);涉钱与额度上限,保存即刻全局生效")
    + freezePanel
    + `<div class="panel"><div class="ph">额度与预警</div><div class="pb">
      <table class="kvtable" style="margin-bottom:12px">
        <tr><td class="k">quota_per_unit(只读)</td><td><b>${qpu || "-"}</b><div class="mini">须与所连 new-api 实例一致(启动自检不一致拒启动,33 §3.6);当前换算 $1 = ${qpu || "-"} quota</div></td></tr>
      </table>
      <div class="fld" style="max-width:400px"><label>成员令牌数上限(member_token_limit,默认 2)</label><input id="pf_tl" type="number" min="1" step="1" value="${Number(ps.member_token_limit) || ""}"></div>
      <div class="fld" style="max-width:400px"><label>成员额度帽(美元;member_quota_cap_raw,默认 $1000)</label><input id="pf_cap" type="number" min="0" step="1" value="${cap != null ? (cap / (S.qpu || 500000)) : ""}"><div class="mini" style="margin-top:4px">单成员持有额度上限;raw 不得超 int32(约 $4294,31-ADR §3 护栏)</div></div>
      <div class="fld" style="max-width:400px"><label>金库低预警阈值(美元;treasury_low_watermark_raw)</label><input id="pf_low" type="number" min="0" step="1" value="${low != null ? (low / (S.qpu || 500000)) : ""}"><div class="mini" style="margin-top:4px">金库低于该值 → 预警组织管理员 + 运营方</div></div>
      <button class="btn pri" onclick="doSavePlatformSettings()">保存</button>
      <div class="errline" id="pf_err"></div>
    </div></div>
    <div class="panel"><div class="ph">品牌信息(全站与登录页展示;改完即时生效)</div><div class="pb">
      <div class="fld" style="max-width:400px"><label>产品名称</label><input id="pf_bn" value="${esc(ps.brand_product_name || "")}" placeholder="未配置时显示"企业管理台""></div>
      <div class="fld" style="max-width:400px"><label>公司名称</label><input id="pf_bc" value="${esc(ps.brand_company_name || "")}" placeholder="留空则页脚不显示公司名"></div>
      <div class="fld" style="max-width:400px"><label>客服邮箱</label><input id="pf_be" value="${esc(ps.brand_support_email || "")}" placeholder="留空则帮助弹层不显示客服入口"></div>
      <div class="fld" style="max-width:400px"><label>文档地址</label><input id="pf_bd" value="${esc(ps.brand_doc_url || "")}" placeholder="留空则不显示文档链接"></div>
      <button class="btn pri" onclick="doSaveBranding()">保存品牌信息</button>
      <div class="errline" id="pf_berr"></div>
    </div></div>`;
};
async function doSaveBranding() {
  setErr("pf_berr", "");
  const body = {
    brand_product_name: val("pf_bn").trim(), brand_company_name: val("pf_bc").trim(),
    brand_support_email: val("pf_be").trim(), brand_doc_url: val("pf_bd").trim(),
  };
  if (body.brand_support_email && !/^[^@\s]+@[^@\s]+$/.test(body.brand_support_email)) { setErr("pf_berr", "客服邮箱格式不正确"); return; }
  try { await api("PUT", "/platform-settings", body); await loadBranding(); toast("品牌信息已保存,全站即时生效"); renderShell(); renderSide(); renderView(); }
  catch (e) { setErr("pf_berr", e.message); }
}
function confirmMoneyFreeze(on) {
  if (on) {
    dangerConfirm("确认开启钱动作急停?", `<p style="color:var(--bad)"><b>这是全平台级红色开关。</b></p>
      <p>开启后所有组织的划拨立即全部拒绝:开通成员、追加划拨、订阅补满、离职退额、对账补齐写入,全部冻结。</p>
      <p>仅用于灰度期资金事故止血;处理完成后请尽快解除。</p>`, "确认冻结所有钱动作", "doMoneyFreeze(true)");
  } else {
    dangerConfirm("解除钱动作急停?", `<p>解除后划拨 / 订阅补满 / 退额恢复执行。</p>
      <p>请确认事故已处理、对账(reconcile)漂移已收敛后再解除。</p>`, "确认解除", "doMoneyFreeze(false)");
  }
}
async function doMoneyFreeze(on) {
  try { await api("PUT", "/platform-settings", { money_freeze: on }); closeM(); toast(on ? "已开启急停:所有钱动作已冻结" : "已解除急停,划拨恢复"); renderView(); }
  catch (e) { toast(e.message); }
}
async function doSavePlatformSettings() {
  setErr("pf_err", "");
  const body = {};
  const tls = val("pf_tl").trim();
  if (tls !== "") { const tl = parseInt(tls, 10); if (!(tl > 0)) { setErr("pf_err", "令牌数上限须为正整数"); return; } body.member_token_limit = tl; }
  const capUsd = val("pf_cap").trim();
  if (capUsd !== "") {
    const c = parseFloat(capUsd);
    if (!(c > 0)) { setErr("pf_err", "成员额度帽须为正数"); return; }
    const raw = usd2raw(c);
    if (raw > 2147483647) { setErr("pf_err", "额度帽 raw 超 int32 上限(约 $" + Math.floor(2147483647 / (S.qpu || 500000)) + "),请调低"); return; }
    body.member_quota_cap_raw = raw;
  }
  const lowUsd = val("pf_low").trim();
  if (lowUsd !== "") { const l = parseFloat(lowUsd); if (!(l >= 0)) { setErr("pf_err", "预警阈值须为非负数"); return; } body.treasury_low_watermark_raw = usd2raw(l); }
  try { await api("PUT", "/platform-settings", body); toast("已保存"); renderView(); }
  catch (e) { setErr("pf_err", e.message); }
}
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
