/* NexusAPI 企业管理平台 · 生产前端 SPA(接 /api/v1 真后端)。
   设计沿用原型 app.css;数据全来自真实 API(非 mock)。按 /me 的角色拼装菜单与视图。 */
"use strict";
const S = { token: localStorage.getItem("nx_token") || "", me: null, role: "", view: "", org: null, orgId: 0, mvp: false, win: 168 };
// 改动⑦:看板时间窗(小时)。7/30/90 天选择器用,默认 168=近 7 天。
function winLabel(h) { return ({ 168: "近 7 天", 720: "近 30 天", 2160: "近 90 天" })[h] || ("近 " + Math.round(h / 24) + " 天"); }
function winSelect() {
  return `<select class="fsel" onchange="S.win=+this.value;renderView()">`
    + [168, 720, 2160].map(h => `<option value="${h}"${S.win === h ? " selected" : ""}>${winLabel(h)}</option>`).join("")
    + `</select>`;
}

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
const money = q => "$" + (q / 500000).toLocaleString(undefined, { maximumFractionDigits: 2 }); // quota→美元(锚定 500000=$1)
// GZ-05 修复A:补单引号与反引号转义。' → &#39; 放进单引号 JS 串内即变普通文本,无法闭合 onclick 参数;
// ` → &#96; 顺手堵掉模板串反引号面。对「文本内容」与「双引号属性」渲染无副作用。
const esc = s => String(s == null ? "" : s).replace(/[&<>"'`]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;", "`": "&#96;" }[c]));
const roleCN = r => ({ operator: "运营方", org_admin: "组织管理员", team_leader: "团队负责人", member: "成员" }[r] || r);

/* ---------- 登录 ---------- */
async function doLogin() {
  const email = document.getElementById("lg_email").value.trim();
  const pw = document.getElementById("lg_pw").value;
  const err = document.getElementById("lg_err"); err.textContent = "";
  document.getElementById("lg_btn").textContent = "登录中…";
  try {
    const d = await api("POST", "/auth/login", { email, password: pw });
    S.token = d.token; localStorage.setItem("nx_token", S.token);
    document.getElementById("login").classList.add("hide");
    await boot();
  } catch (e) { err.textContent = e.message; }
  document.getElementById("lg_btn").textContent = "登录";
}
function logout() { S.token = ""; localStorage.removeItem("nx_token"); location.reload(); }

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

const NAV = {
  operator: [{ grp: "运营" }, { v: "orgs", ic: "▦", t: "客户组织" }],
  org_admin: [{ grp: "管理" }, { v: "dash", ic: "◧", t: "概览" }, { v: "members", ic: "☷", t: "成员" }, { v: "teams", ic: "▣", t: "团队" }, { v: "tiers", ic: "◆", t: "层级" }, { v: "approvals", ic: "✓", t: "审批" }, { v: "billing", ic: "¥", t: "余额与计费" }, { v: "mynotif", ic: "✉", t: "通知" }],
  team_leader: [{ grp: "团队" }, { v: "members", ic: "☷", t: "团队成员" }, { v: "approvals", ic: "✓", t: "审批" }],
  member: [{ grp: "我的" }, { v: "myusage", ic: "▦", t: "我的用量" }, { v: "mykey", ic: "⚿", t: "我的 API Key" }, { v: "myreq", ic: "✚", t: "申请增额" }, { v: "mynotif", ic: "✉", t: "通知" }],
};

function renderShell() {
  const av = (S.me.display_name || S.me.login_email || "U").slice(0, 1).toUpperCase();
  document.getElementById("app").innerHTML = `
  <div class="app">
    <div class="top">
      <div class="logo"><span class="mk">N</span> 企业管理台</div>
      <span class="demolbl">${esc(roleCN(S.role))}</span>
      <div class="spacer"></div>
      <div class="me"><span class="av">${esc(av)}</span><span>${esc(S.me.display_name || S.me.login_email)}</span>
        <span class="lk" onclick="go('settings')" style="margin-left:10px">设置</span>
        <span class="lk" onclick="logout()" style="margin-left:10px">退出</span></div>
    </div>
    <div class="side" id="side"></div>
    <div class="main" id="main"></div>
  </div>`;
}
function renderSide() {
  let h = "";
  // 改动⑥-2:MVP 下藏掉钱/控菜单(审批/余额计费/申请增额);其端点已被后端 mvpGate 404。
  const mvpHide = ["approvals", "billing", "myreq"];
  (NAV[S.role] || []).filter(n => !(S.mvp && mvpHide.includes(n.v))).forEach(n => {
    if (n.grp) { h += `<div class="grp">${esc(n.grp)}</div>`; return; }
    h += `<div class="nav ${S.view === n.v ? "on" : ""}" id="nav-${n.v}" onclick="go('${n.v}')"><span class="ic">${n.ic}</span>${esc(n.t)}</div>`;
  });
  document.getElementById("side").innerHTML = h;
  updateBadges();
}
async function updateBadges() {
  try {
    if (S.role === "org_admin" || S.role === "team_leader") {
      const d = await api("GET", "/organizations/" + S.orgId + "/approvals?state=pending&page=1&page_size=1", null);
      setBadge("approvals", (d.pagination || {}).total || 0);
    }
    if (S.role === "member" || S.role === "org_admin") {
      const n = await api("GET", "/notifications?page=1&page_size=1", null);
      setBadge("mynotif", n.unread || 0);
    }
  } catch (e) {}
}
function setBadge(v, n) {
  const el = document.getElementById("nav-" + v);
  if (el && n > 0) el.insertAdjacentHTML("beforeend", `<span class="badge-dot">${n}</span>`);
}
function go(v) { S.view = v; renderSide(); renderView(); }

async function renderView() {
  const m = document.getElementById("main");
  m.innerHTML = `<div class="empty">加载中…</div>`;
  const fn = VIEWS[S.view];
  if (!fn) { m.innerHTML = `<div class="empty">待建</div>`; return; }
  try { m.innerHTML = await fn(); if (VIEWS_AFTER[S.view]) VIEWS_AFTER[S.view](); }
  catch (e) { m.innerHTML = `<div class="empty" style="color:var(--bad)">加载失败:${esc(e.message)}</div>`; }
}

/* ---------- helpers ---------- */
function head(h1, sub) { return `<div class="h1">${esc(h1)}</div><div class="sub">${esc(sub || "")}</div>`; }
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

/* ===================== 运营方 ===================== */
const VIEWS = {};

/* ---- 个人设置(全角色:账号信息 + 改密) ---- */
VIEWS.settings = async () => {
  const m = S.me;
  return head("个人设置", "账号信息与登录密码")
    + `<div class="panel"><div class="ph">账号信息</div><div class="pb"><table class="kvtable">
        <tr><td class="k">显示名</td><td>${esc(m.display_name || "-")}</td></tr>
        <tr><td class="k">登录邮箱</td><td>${esc(m.login_email)}</td></tr>
        <tr><td class="k">角色</td><td>${esc(roleCN(m.role))}</td></tr></table></div></div>
    <div class="panel"><div class="ph">修改密码</div><div class="pb">
        <div class="fld" style="max-width:360px"><label>当前密码</label><input id="cp_old" type="password" placeholder="当前密码"></div>
        <div class="fld" style="max-width:360px"><label>新密码</label><input id="cp_new" type="password" placeholder="8–64 位"></div>
        <div class="fld" style="max-width:360px"><label>确认新密码</label><input id="cp_new2" type="password" placeholder="再输一次"></div>
        <button class="btn pri" onclick="doChangePassword()">保存新密码</button>
      </div></div>`;
};
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
  const d = await api("GET", "/organizations?page=1&page_size=50" + (inclArch ? "&include_archived=true" : ""), null);
  // 过滤掉平台运营方伪组织(slug=_operator),只列真实客户(R2-轻微)。
  const list = (d.list || []).filter(o => o.slug !== "_operator");
  const rows = list.map(o => `<tr data-q="${esc((o.name + " " + o.slug).toLowerCase())}">
    <td><span class="lk" style="display:inline-block;max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:middle" title="${esc(o.name)}" onclick="enterOrg(${o.id},'${esc(o.name)}')">${esc(o.name)}</span><div class="mini">${esc(o.slug)}</div></td>
    <td>${pill(o.status, o.status === "active" ? "ok" : o.status === "low" ? "warn" : "bad")}${o.archived ? ' <span class="tag">已归档</span>' : ''}</td>
    <td>${esc(o.timezone)}</td><td>${esc(o.billing_mode)}</td>
    <td class="right"><span class="btn sm" onclick="enterOrg(${o.id},'${esc(o.name)}')">进入</span> ${o.archived
      ? `<span class="btn sm" onclick="doArchiveOrg(${o.id},'${esc(o.name)}',false)">取消归档</span>`
      : `<span class="btn sm danger" onclick="doArchiveOrg(${o.id},'${esc(o.name)}',true)">归档</span>`}</td></tr>`).join("");
  return head("客户组织", "运营方:管理所有客户组织、入账、计费灰度、支持会话")
    + `<div class="toolbar"><div class="search"><input id="orgSearch" placeholder="搜索组织名或 slug…" oninput="filterRows('orgSearch','orgTbody')"></div>
       <label class="mini" style="margin:0 10px;cursor:pointer"><input type="checkbox" ${inclArch ? "checked" : ""} onchange="S.orgShowArchived=this.checked;renderView()"> 显示已归档</label>
       <button class="btn pri" onclick="openCreateOrg()">+ 新建客户组织</button></div>
    <div class="panel"><table><thead><tr><th>组织</th><th>状态</th><th>时区</th><th>计费</th><th></th></tr></thead>
    <tbody id="orgTbody">${rows || '<tr><td colspan=5 class="empty">暂无组织</td></tr>'}</tbody></table></div>`;
};
async function doArchiveOrg(id, name, archive) {
  if (!confirm((archive ? "归档" : "取消归档") + "组织「" + name + "」?" + (archive ? "归档后从默认列表隐藏,数据保留、可恢复。" : ""))) return;
  try { await api("POST", "/organizations/" + id + (archive ? "/archive" : "/unarchive"), null); toast(archive ? "已归档" : "已取消归档"); renderView(); }
  catch (e) { toast(e.message); }
}
// filterRows 通用前端筛选:按 data-q 包含关键词显隐行(无需重新拉数据)。
function filterRows(inputId, tbodyId) {
  const q = (document.getElementById(inputId).value || "").trim().toLowerCase();
  document.querySelectorAll("#" + tbodyId + " tr[data-q]").forEach(tr => {
    tr.style.display = (!q || tr.getAttribute("data-q").indexOf(q) >= 0) ? "" : "none";
  });
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
    const u = await api("GET", "/members/" + mid + "/usage?since_hours=" + S.win, null);
    const rows = (u.by_model || []).map(b => `<tr><td>${esc(b.key)}</td><td class="right">${money(b.consumed_quota)}</td><td class="right mini">${b.count}</td></tr>`).join("") || `<tr><td class="empty" colspan="3">该窗口暂无用量</td></tr>`;
    modal("员工用量明细 · " + name + "(" + winLabel(S.win) + ")",
      `<table class="kvtable"><tr><td class="k">模型</td><td class="right k">费用</td><td class="right k">次数</td></tr>${rows}
       <tr><td class="k">合计</td><td class="right"><b>${money(u.total_quota)}</b></td><td></td></tr></table>`,
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
  } catch (e) { toast(e.message); }
}
function openCreateOrg() {
  modal("新建客户组织", `<div class="fld"><label>组织名称</label><input id="co_n" placeholder="Acme 科技"></div>
    <div class="fld"><label>唯一标识 slug</label><input id="co_s" placeholder="acme"></div>
    <div class="fld"><label>管理员邮箱</label><input id="co_e" placeholder="admin@acme.com"></div>
    <div class="fld"><label>new-api 用户分组</label><input id="co_g" placeholder="org_acme"></div>
    <div class="note">new-api 用户分组须先在 new-api 配好(挂模型分组 group_special_usable_group),否则建组织会被拒。一个分组只绑一个组织。管理员初始密码本次回显一次。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateOrg()">创建</button>`);
}
async function doCreateOrg() {
  try {
    const d = await api("POST", "/organizations", { name: val("co_n"), slug: val("co_s"), admin_email: val("co_e"), newapi_user_group: val("co_g") });
    modal("已创建", `<div class="note">组织已创建。请把管理员初始凭证交付客户(仅显示一次):</div>
      <table class="kvtable"><tr><td class="k">管理员邮箱</td><td>${esc(d.admin_email)}</td></tr>
      <tr><td class="k">初始密码</td><td><b>${esc(d.admin_initial_password)}</b></td></tr></table>`,
      `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
async function enterOrg(id, name) {
  S.orgId = id; S.org = { id, name };
  const [org, bal, bs, ref] = await Promise.all([
    api("GET", "/organizations/" + id, null),
    api("GET", "/organizations/" + id + "/balance", null),
    api("GET", "/organizations/" + id + "/billing-settings", null),
    api("GET", "/organizations/" + id + "/budget-ref", null), // #4:运营方也看 已用$/预付$(垫钱敞口)
  ]);
  const mem = await api("GET", "/organizations/" + id + "/members?page=1&page_size=20", null);
  const main = document.getElementById("main");
  const rows = (mem.list || []).map(m => `<tr><td>${esc(m.display_name || m.login_email)}</td>
    <td>${pill(m.status, m.status === "active" ? "ok" : "mut")}</td><td class="mini">${esc(m.key_masked || "-")}</td></tr>`).join("");
  main.innerHTML = head(esc(name), S.mvp ? "运营方支持视角 · 成员 / 组织状态 / 支持会话" : "运营方支持视角 · 余额 / 计费灰度 / 成员 / 支持会话")
    + `<div class="crumb"><span class="lk" onclick="go('orgs')">客户组织</span><span class="sep">/</span><b>${esc(name)}</b></div>
    <div class="cards">
      ${S.mvp ? "" : kpi("当前余额", money(bal.balance_quota), "累计充值 " + money(bal.total_recharged_quota))}
      ${S.mvp ? "" : kpi("累计消耗", money(bal.total_consumed_quota), "")}
      ${kpi("组织状态", pill(org.status, org.status === "active" ? "ok" : "warn"), "")}
      ${S.mvp ? "" : kpi("计费灰度", (bs.billing_enabled ? "扣费开" : "扣费关") + " · " + (bs.hard_stop_enabled ? "硬停开" : "硬停关"), "默认全关")}
    </div>
    ${S.mvp ? `<div class="panel"><div class="ph">额度参考(仅供参考,本期不停服)</div><div class="pb"><div class="cards">${kpi("累计已用", money(ref.consumed_quota), "读 new-api 日志结算")}${kpi("预付总额", money(ref.recharged_quota), "")}${kpi("剩余(参考)", money((ref.recharged_quota || 0) - (ref.consumed_quota || 0)), "用超不停服,仅提示")}</div></div></div>` : ""}
    <div class="toolbar">
      ${S.mvp ? "" : `<button class="btn pri" onclick="openTopup(${id})">充值入账</button>
      <button class="btn" onclick="toggleBilling(${id},${!bs.billing_enabled})">${bs.billing_enabled ? "关闭扣费" : "开启扣费(灰度)"}</button>
      <button class="btn" onclick="openDiscount(${id})">配置折扣</button>`}
      <button class="btn" onclick="openSupport(${id})">支持会话</button>
    </div>
    <div class="panel"><div class="ph">成员</div><div class="pb"><table><tbody>${rows || '<tr><td class="empty">暂无成员</td></tr>'}</tbody></table></div></div>`;
}
function openTopup(id) {
  modal("充值入账(运营方)", `<div class="fld"><label>入账金额(美元)</label><input id="tp_a" type="number" placeholder="100"></div>
    <div class="fld"><label>转账唯一号(幂等键)</label><input id="tp_t" placeholder="银行流水号"></div>
    <div class="note">动钱操作:仅运营方可入账,转账号唯一防重复入账,全程留痕。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doTopup(${id})">确认入账</button>`);
}
async function doTopup(id) {
  try {
    const usd = parseFloat(val("tp_a")) || 0;
    await api("POST", "/organizations/" + id + "/recharges", { amount_quota: Math.round(usd * 500000), transfer_no: val("tp_t") });
    closeM(); toast("已入账"); enterOrg(id, S.org.name);
  } catch (e) { toast(e.message); }
}
async function toggleBilling(id, on) {
  try { await api("PATCH", "/organizations/" + id + "/billing-settings", { billing_enabled: on }); toast(on ? "已开启扣费(灰度)" : "已关闭扣费"); enterOrg(id, S.org.name); }
  catch (e) { toast(e.message); }
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
  modal("运营方支持会话", `<div class="fld"><label>支持类型</label><select id="sp_s"><option value="readonly">只读支持(看不能改)</option><option value="assist">协助态(可受控写,动钱/读key红线挡)</option></select></div>
    <div class="note">进入支持会话后,你将以组织管理员身份在该组织操作,受后端闸约束,每步双身份留痕。</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doSupport(${id})">进入</button>`);
}
async function doSupport(id) {
  try {
    const scope = val("sp_s");
    const d = await api("POST", "/organizations/" + id + "/support-sessions", { scope, grant_type: scope === "assist" ? "authorized" : "", ttl_seconds: 7200, reason: "运营支持" });
    S.token = d.token; localStorage.setItem("nx_token", S.token); closeM(); toast("已进入" + (scope === "readonly" ? "只读" : "协助") + "支持态"); await boot();
  } catch (e) { toast(e.message); }
}

/* ===================== 组织管理员 / 团队负责人 ===================== */
VIEWS.dash = async () => {
  const id = S.orgId;
  const win = S.win, wl = winLabel(win);
  // 改动⑦:MVP 看板纯用量化 —— 钱相关(余额/累计消耗/低位阈值)只在非 MVP 拉取与展示。
  // 改动⑦:钱(余额/累计消耗/低位)只非 MVP 展示;MVP 改用 #4 额度参考条(只读,不停服)。
  const reqs = [api("GET", "/organizations/" + id + "/usage?since_hours=" + win, null)];
  reqs.push(api("GET", "/organizations/" + id + (S.mvp ? "/budget-ref" : "/balance"), null));
  const [usage, extra] = await Promise.all(reqs);
  // #6:模型/员工都全量渲染 + 各自搜索框 + 滚动容器(成员超 8 人也找得到),不再 slice 截断。
  const mods = usage.by_model || [];
  const maxv = Math.max(1, ...mods.map(b => b.consumed_quota));
  const bars = mods.map(b => `<div class="bar" data-q="${esc((b.key || "").toLowerCase())}"><span class="nm">${esc(b.key)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / maxv * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`).join("") || `<div class="empty">${wl}暂无用量</div>`;
  // 改动④ + #5:员工排行(降序);行可点 → 下钻该员工按模型明细(后端已有 /members/{id}/usage)。
  const mem = usage.by_member || [];
  const maxm = Math.max(1, ...mem.map(b => b.consumed_quota));
  const mbars = mem.map(b => {
    const nm = b.label || ("用户#" + b.key);
    const attrs = b.member_id ? `class="bar lk" onclick="openMemberUsage(${b.member_id},'${esc(nm)}')" title="查看该员工按模型明细"` : `class="bar"`;
    return `<div ${attrs} data-q="${esc(nm.toLowerCase())}"><span class="nm">${esc(nm)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / maxm * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`;
  }).join("") || `<div class="empty">${wl}暂无用量</div>`;
  const moneyCards = S.mvp ? "" : `
      ${kpi("当前余额", money(extra.balance_quota), "累计充值 " + money(extra.total_recharged_quota))}
      ${kpi("累计消耗", money(extra.total_consumed_quota), "")}
      ${kpi("低位阈值", money(extra.low_watermark_quota), "")}`;
  // #4 额度参考条(MVP):已用$/预付$/剩余$,仅供参考、本期不停服。
  const refBar = S.mvp ? `<div class="panel"><div class="ph">额度参考(仅供参考,本期不停服)</div><div class="pb"><div class="cards">
      ${kpi("累计已用", money(extra.consumed_quota), "读 new-api 日志结算")}
      ${kpi("预付总额", money(extra.recharged_quota), "")}
      ${kpi("剩余(参考)", money((extra.recharged_quota || 0) - (extra.consumed_quota || 0)), "用超不停服,仅提示")}
    </div></div></div>` : "";
  const srch = (iid, cid, ph) => `<input id="${iid}" placeholder="${ph}" oninput="filterEls('${iid}','${cid}')" style="float:right;width:150px;padding:2px 8px;font-size:12px">`;
  return head("概览", S.mvp ? "公司用量总览(只读)" : "公司余额 + 用量")
    + `<div class="toolbar"><div class="search"></div><button class="btn" onclick="downloadUsageCsv()">导出CSV(员工名·美元)</button><span class="mini">时间窗</span>${winSelect()}</div>
    <div class="cards">
      ${kpi(wl + "消耗", money(usage.total_quota), "读 new-api 日志结算")}${moneyCards}
    </div>
    ${refBar}
    <div class="panel"><div class="ph">员工用量排行(${wl})· 点员工看明细${srch("memSearch", "memBars", "搜员工…")}</div><div class="pb" id="memBars" style="max-height:340px;overflow:auto">${mbars}</div></div>
    <div class="panel"><div class="ph">按模型用量(${wl})${srch("modSearch", "modBars", "搜模型…")}</div><div class="pb" id="modBars" style="max-height:340px;overflow:auto">${bars}</div></div>`;
};
VIEWS.members = async () => {
  const id = S.orgId;
  const d = await api("GET", "/organizations/" + id + "/members?page=1&page_size=50", null);
  // 管理类账号(运营方/组织管理员)不进"成员"列表(M7);只列 API 使用成员。
  const rows = (d.list || []).filter(m => m.role !== "org_admin" && m.role !== "operator").map(m => `<tr>
    <td>${esc(m.display_name || m.login_email)}<div class="mini">${esc(m.login_email)}</div></td>
    <td>${pill(m.status, m.status === "active" ? "ok" : "mut")}</td>
    <td class="mini">${esc(m.key_masked || "-")}</td>
    <td class="right">
      ${S.mvp ? "" : `<span class="btn sm" onclick="openAdjust(${m.id})">调额</span>`}
      <span class="btn sm" onclick="toggleMember(${m.id},${m.status !== "active"})">${m.status === "active" ? "停用" : "恢复"}</span>
    </td></tr>`).join("");
  return head(S.role === "team_leader" ? "团队成员" : "成员", S.mvp ? "开通成员建账号+交付登录凭证(Key 由员工自助创建)" : "开通成员即代发 API key;调额走临时 grant、到期自动回退")
    + `<div class="toolbar"><div class="search"></div>${S.mvp ? "" : `<button class="btn" onclick="openBulk()">批量导入</button>`}<button class="btn pri" onclick="openAddMember()">+ 开通成员</button></div>
    <div class="panel"><table><thead><tr><th>成员</th><th>状态</th><th>Key(脱敏)</th><th></th></tr></thead>
    <tbody>${rows || '<tr><td colspan=4 class="empty">暂无成员</td></tr>'}</tbody></table></div>`;
};
async function openAddMember() {
  let tiers = []; try { tiers = (await api("GET", "/organizations/" + S.orgId + "/tiers", null)) || []; } catch (e) {}
  const opts = tiers.map(t => `<option value="${t.id}">${esc(t.name)}${t.is_default ? "(默认)" : ""}</option>`).join("");
  modal("开通成员", `<div class="fld"><label>姓名</label><input id="am_n" placeholder="钱晨"></div>
    <div class="fld"><label>登录名(真实邮箱/用户名,可选)</label><input id="am_e" placeholder="留空则自动生成"></div>
    <div class="fld"><label>层级</label><select id="am_t">${opts || '<option value="">(先建层级)</option>'}</select></div>
    <div class="note">登录名留空将自动生成。${S.mvp ? "系统将建 new-api 用户并下发层级初始额度,本期不代发 Key(员工登录后在「我的 Key」自助创建);登录邮箱与初始密码开通后回显一次。" : "系统将建 new-api 用户、代发 API key、下发层级初始额度。明文 key 仅创建后回显一次。"}</div>`,
    `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doAddMember()">开通成员</button>`);
}
async function doAddMember() {
  try {
    const body = { name: val("am_n") };
    const em = val("am_e").trim(); if (em) body.email = em; // T11:自定义登录名,留空后端 fallback
    const t = val("am_t"); if (t) body.tier_id = parseInt(t);
    const d = await api("POST", "/organizations/" + S.orgId + "/members", body);
    const keyRow = d.api_key
      ? `<tr><td class="k">API Key</td><td><b>${esc(d.api_key)}</b> <span class="lk" onclick="navigator.clipboard&&navigator.clipboard.writeText('${esc(d.api_key)}');toast('已复制')">复制</span></td></tr>`
      : `<tr><td class="k">API Key</td><td class="mini">本期不代发,员工登录后在「我的 Key」自助创建</td></tr>`;
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
    <div class="fld"><label>统一层级</label><select id="bk_tier">${opts}</select></div>
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
async function toggleMember(mid, enable) {
  try { await api("POST", "/members/" + mid + "/status", { enabled: enable }); toast(enable ? "已恢复" : "已停用"); renderView(); }
  catch (e) { toast(e.message); }
}
VIEWS.teams = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/teams", null);
  const rows = (d || []).map(t => `<tr><td>${esc(t.name)}</td><td>${pill(t.status, "mut")}</td></tr>`).join("");
  return head("团队", "组织管理员维护团队")
    + (S.role === "org_admin" ? `<div class="toolbar"><button class="btn pri" onclick="openCreateTeam()">+ 新建团队</button></div>` : "")
    + `<div class="panel"><table><tbody>${rows || '<tr><td class="empty">暂无团队</td></tr>'}</tbody></table></div>`;
};
function openCreateTeam() { modal("新建团队", `<div class="fld"><label>团队名称</label><input id="tm_n" placeholder="研发一组"></div>`, `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doCreateTeam()">创建</button>`); }
async function doCreateTeam() { try { await api("POST", "/organizations/" + S.orgId + "/teams", { name: val("tm_n") }); closeM(); toast("已创建"); renderView(); } catch (e) { toast(e.message); } }
VIEWS.tiers = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/tiers", null);
  S.tiersCache = d || []; // 供编辑弹窗回填
  try { S.billingGroups = (await api("GET", "/pricing/groups", null)) || []; } catch (e) { S.billingGroups = []; } // T17-6 计费分组
  // 改动⑦补丁(灰度前阻断·产品总监逮出):MVP 下层级页藏掉钱字段(月额度/基础倍率/折后价),
  // 只留层级名 + 模型分组 + 模型集(setup 部分,管理员要靠它把成员映射到模型分组);别藏整菜单否则没法建层级。
  const rows = (d || []).map(t => `<tr><td>${esc(t.name)}${t.is_default ? ' <span class="tag">默认</span>' : ""}</td>
    <td>${t.newapi_group ? esc(t.newapi_group) : '<span class="mini">默认</span>'}</td>
    ${S.mvp ? "" : `<td>${t.monthly_limit_quota != null ? money(t.monthly_limit_quota) + " / 月" : '<span class="mini">不限</span>'}</td>`}
    <td>${(t.model_set || []).map(m => `<span class="mcap">${esc(m)}</span>`).join("") || '<span class="mini">继承</span>'}</td>
    <td class="right"><span class="btn sm" onclick="openEditTier(${t.id})">编辑</span> <span class="btn sm danger" onclick="doDeleteTier(${t.id},'${esc(t.name)}')">删除</span></td></tr>`).join("");
  const tierColspan = S.mvp ? 4 : 5;
  return head("层级", S.mvp ? "可复用档位 = 模型分组 + 模型集(决定成员可用哪些模型)" : "可复用档位 = 计费分组 + 模型集 + 月额度 + 单模型日上限")
    + `<div class="toolbar"><button class="btn pri" onclick="openCreateTier()">+ 新建层级</button></div>
    <div class="panel"><table><thead><tr><th>层级</th><th>${S.mvp ? "模型分组" : "计费分组"}</th>${S.mvp ? "" : "<th>月额度</th>"}<th>模型集</th><th></th></tr></thead><tbody>${rows || `<tr><td colspan=${tierColspan} class="empty">暂无层级</td></tr>`}</tbody></table></div>`;
};
// groupSelectHTML 计费分组下拉(T17-6):来源实时拉的 /pricing/groups,选项带基础倍率;空=回落默认。
function groupSelectHTML(selId, selected) {
  const opts = [`<option value="">默认(回落组织默认${S.mvp ? "分组" : "/标准价"})</option>`].concat(
    (S.billingGroups || []).map(g => `<option value="${esc(g.group)}" ${g.group === selected ? "selected" : ""}>${esc(g.group)}${S.mvp ? "" : `(基础倍率 ${g.ratio})`}</option>`));
  return `<div class="fld"><label>${S.mvp ? "模型分组(决定成员可用哪些模型)" : "计费分组(决定可用模型+收费倍率)"}</label>
    <select id="${selId}" onchange="tierGroupHint('${selId}')">${opts.join("")}</select>
    <div class="mini" id="${selId}_hint" style="margin-top:4px"></div></div>`;
}
function tierGroupHint(selId) {
  const g = document.getElementById(selId).value;
  const hint = document.getElementById(selId + "_hint");
  if (!g) { hint.innerHTML = "回落组织默认分组,模型集需在该分组可用范围内"; return; }
  const bg = (S.billingGroups || []).find(x => x.group === g);
  if (!bg) { hint.innerHTML = ""; return; }
  const ms = (bg.models || []);
  const modelsTxt = `可用模型(${ms.length}):${ms.slice(0, 12).map(esc).join("、")}${ms.length > 12 ? " …" : ""}`;
  // MVP:只展示可用模型,藏基础倍率/折后价(钱结构不给客户管理员看)。
  hint.innerHTML = S.mvp ? `${modelsTxt}<br>模型集须 ⊆ 该可用模型(否则保存被拦)`
    : `基础倍率 <b>${bg.ratio}</b> · ${modelsTxt}<br>模型集须 ⊆ 该可用模型(否则保存被拦);折后价 = 基础倍率 × 客户折扣%`;
}
function openEditTier(tid) {
  const t = (S.tiersCache || []).find(x => x.id === tid); if (!t) return;
  const ms = (t.model_set || []).join(",");
  const mq = t.monthly_limit_quota != null ? (t.monthly_limit_quota / 500000) : "";
  modal("编辑层级", `<div class="fld"><label>层级名称</label><input id="te_n" value="${esc(t.name)}"></div>
    ${groupSelectHTML("te_g", t.newapi_group || "")}
    ${S.mvp ? "" : `<div class="fld"><label>月额度(美元,成员当期上限基线)</label><input id="te_q" type="number" value="${mq}"></div>`}
    <div class="fld"><label>模型集(逗号分隔,留空=继承)</label><input id="te_m" value="${esc(ms)}"></div>
    <div class="note">${S.mvp ? "改后该层级成员的可用模型集随之更新。" : "改后引用该层级的成员当期上限按新档重算下发;改计费分组=改计价档(动钱相邻)。"}</div>`,
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
  if (!confirm("确认删除层级「" + name + "」?被成员引用或为默认档将无法删除。")) return;
  try { await api("DELETE", "/tiers/" + tid, null); toast("已删除"); renderView(); }
  catch (e) { toast(e.message); }
}
function openCreateTier() {
  modal("新建层级", `<div class="fld"><label>层级名称</label><input id="ti_n" placeholder="标准档"></div>
    ${groupSelectHTML("ti_g", "")}
    ${S.mvp ? "" : `<div class="fld"><label>月额度(美元,成员当期上限基线)</label><input id="ti_q" type="number" placeholder="50"></div>`}
    <div class="fld"><label>模型集(逗号分隔,留空=继承)</label><input id="ti_m" placeholder="gpt-4o,claude-sonnet-4-5-20250929"></div>
    <div class="note">${S.mvp ? "模型分组决定成员可用哪些模型;模型集须在该分组可用范围内(否则保存被拦)。" : "计费分组决定可用模型+倍率;模型集须在该分组可用范围内(否则保存被拦)。"}</div>`,
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
VIEWS.approvals = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/approvals?page=1&page_size=50", null);
  const rows = (d.list || []).map(a => {
    const can = a.state === "pending" || a.state === "l1_approved";
    return `<tr><td>#${a.id} ${esc(a.request_type)}${a.is_level2 ? ' <span class="tag">二审</span>' : ""}<div class="mini">${esc(a.applicant_name || ("成员 #" + a.applicant_id))} (#${a.applicant_id}) · ${esc((a.created_at || "").slice(0, 16).replace("T", " "))} · ${esc(a.model || "")} ${money(a.amount_quota)} / ${esc(a.duration)}</div></td>
    <td>${pill(a.state, a.state === "approved" || a.state === "auto_approved" ? "ok" : a.state === "rejected" ? "bad" : "warn")}</td>
    <td class="right">${can ? `<span class="btn sm pri" onclick="decide(${a.id},true)">批准</span> <span class="btn sm danger" onclick="decide(${a.id},false)">驳回</span>` : ""}</td></tr>`;
  }).join("");
  return head("审批", "三档:自动通过 / 一审(团队负责人)/ 二审(组织管理员)")
    + `<div class="panel"><table><thead><tr><th>申请</th><th>状态</th><th></th></tr></thead><tbody>${rows || '<tr><td colspan=3 class="empty">暂无申请</td></tr>'}</tbody></table></div>`;
};
async function decide(id, ok) { try { await api("POST", "/approvals/" + id + "/decide", { approved: ok, comment: "" }); toast(ok ? "已批准" : "已驳回"); renderView(); } catch (e) { toast(e.message); } }
VIEWS.billing = async () => {
  const id = S.orgId;
  const [bal, recs, reqs] = await Promise.all([
    api("GET", "/organizations/" + id + "/balance", null),
    api("GET", "/organizations/" + id + "/recharges?page=1&page_size=10", null),
    api("GET", "/organizations/" + id + "/recharge-requests?page=1&page_size=10", null),
  ]);
  let pricing = null; try { pricing = await api("GET", "/organizations/" + id + "/pricing", null); } catch (e) {}
  const rrows = (recs.list || []).map(r => `<tr><td>${esc(r.recharged_at.slice(0, 10))}</td><td>${money(r.amount_quota)}</td><td class="mini">${esc(r.operator_name || r.operator)}</td></tr>`).join("");
  const qrows = (reqs.list || []).map(r => `<tr><td>${esc(r.request_type)}</td><td>${money(r.amount_quota)}</td><td>${pill(r.status, r.status === "pending" ? "warn" : "ok")}</td></tr>`).join("");
  return head("余额与计费", "预付余额 = 累计充值 − 累计消耗;充值由运营方入账,你可发起申请")
    + `<div class="cards">${kpi("当前余额", money(bal.balance_quota), "")}${kpi("累计充值", money(bal.total_recharged_quota), "")}${kpi("累计消耗", money(bal.total_consumed_quota), "")}${kpi("累计退款", money(bal.total_refunded_quota || 0), "冲正/退款累计")}</div>
    ${pricingReadonly(pricing)}
    <div class="toolbar"><button class="btn pri" onclick="openReqTopup()">申请充值</button></div>
    <div class="row2"><div class="panel"><div class="ph">入账记录</div><div class="pb"><table><tbody>${rrows || '<tr><td class="empty">暂无</td></tr>'}</tbody></table></div></div>
    <div class="panel"><div class="ph">我的申请</div><div class="pb"><table><tbody>${qrows || '<tr><td class="empty">暂无</td></tr>'}</tbody></table></div></div></div>`;
};
// pricingReadonly 客户侧只读折扣回显(T7):无写控件,数据取 GET /pricing 镜像。折扣率显示为"X 折"。
function pricingReadonly(p) {
  const entries = (p && p.entries) || {};
  const gs = Object.keys(entries);
  let body;
  if (!p || p.mode === "none" || gs.length === 0) {
    body = `<tr><td class="empty">当前无折扣,按标准价计费</td></tr>`;
  } else {
    body = gs.map(g => {
      const e = entries[g] || {};
      const zhe = e.pct ? (Math.round(e.pct * 100) / 10) : null; // 0.8 → 8 折
      return `<tr><td>${esc(g)}</td><td>${zhe != null ? zhe + " 折" : "—"}</td><td class="mini">特殊倍率 ${e.abs != null ? e.abs : "—"}(基础 ${e.base != null ? e.base : "—"})</td></tr>`;
    }).join("");
  }
  return `<div class="panel"><div class="ph">当前折扣(只读 · 由运营方配置)</div><div class="pb"><table><thead><tr><th>令牌分组</th><th>折扣</th><th>计价</th></tr></thead><tbody>${body}</tbody></table></div></div>`;
}
function openReqTopup() { modal("申请充值", `<div class="fld"><label>申请金额(美元)</label><input id="rq_a" type="number" placeholder="100"></div><div class="fld"><label>说明</label><input id="rq_n" placeholder="需补预付"></div><div class="note">仅发起申请通知运营方,不改余额。</div>`, `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doReqTopup()">提交</button>`); }
async function doReqTopup() { try { await api("POST", "/organizations/" + S.orgId + "/recharge-requests", { type: "topup", amount_quota: Math.round((parseFloat(val("rq_a")) || 0) * 500000), note: val("rq_n") }); closeM(); toast("已提交申请"); renderView(); } catch (e) { toast(e.message); } }

/* ===================== 成员 ===================== */
VIEWS.myusage = async () => {
  const win = S.win, wl = winLabel(win);
  const u = await api("GET", "/members/" + S.me.id + "/usage?since_hours=" + win, null);
  const top = (u.by_model || []).slice(0, 8);
  const max = Math.max(1, ...top.map(b => b.consumed_quota));
  const bars = top.map(b => `<div class="bar"><span class="nm">${esc(b.key)}</span><span class="track"><span class="fill" style="width:${Math.max(4, Math.round(b.consumed_quota / max * 100))}%"></span></span><span class="vv">${money(b.consumed_quota)}</span></div>`).join("") || `<div class="empty">${wl}暂无用量</div>`;
  // 改动⑦:去掉「调用次数」KPI(单 key 自助态恒=1,无信息量);时间窗 7/30/90 天可选。
  return head("我的用量", wl + "消耗")
    + `<div class="toolbar"><div class="search"></div><span class="mini">时间窗</span>${winSelect()}</div>
    <div class="cards">${kpi(wl + "消耗", money(u.total_quota), "读 new-api 结算")}${kpi("涉及模型", String(top.length), wl)}${kpi("当前状态", '<span class="pill ok">正常</span>', "")}</div>
    <div class="panel"><div class="ph">按模型</div><div class="pb">${bars}</div></div>`;
};
VIEWS.mykey = async () => {
  const m = await api("GET", "/members/" + S.me.id, null);
  return head("我的 API Key", "明文 key 只在创建/轮换时显示一次;之后只见脱敏串,可轮换不可读出(红线)")
    + `<div class="panel"><div class="pb">
      <div class="keybox"><span>${esc(m.key_masked || "(尚未生成,点下方新建)")}</span></div>
      <div style="margin-top:14px"><button class="btn pri" onclick="openNewKey()">新建 / 重建 Key(选模型分组)</button>
        ${m.key_masked ? '<button class="btn" onclick="rotateKey()">轮换(沿用分组)</button>' : ""}</div>
      <div class="note">改动③:一把 key 即可调所选模型分组里的所有模型;建新 key 会替换上一枚(旧 key 失效),明文仅显示一次。</div>
      <div class="fld" style="margin-top:16px"><label>IP 白名单(单 IP 或 CIDR,逗号分隔;留空=不限)</label>
        <input id="ipwl" value="${esc(m.key_masked ? "" : "")}" placeholder="203.0.113.5, 10.0.0.0/8"></div>
      <button class="btn" onclick="saveIP()">保存 IP 白名单</button>
      <div class="note">由 new-api 网关数据面拦截,与平台可用性解耦。</div></div></div>`;
}
async function saveIP() {
  try { await api("POST", "/members/" + S.me.id + "/key:ip-whitelist", { allow_ips: val("ipwl") }); toast("已保存 IP 白名单"); }
  catch (e) { toast(e.message); }
};
async function rotateKey() {
  try { const d = await api("POST", "/members/" + S.me.id + "/key:rotate", null);
    modal("新 API Key", `<div class="note">已轮换,旧 key 失效。新明文仅此一次:</div><div class="keybox"><span>${esc(d.api_key)}</span><span class="lk" onclick="navigator.clipboard&&navigator.clipboard.writeText('${esc(d.api_key)}');toast('已复制')">复制</span></div>`, `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
// 改动③:员工自助建 key——先拉本企业可用模型分组,选一个生成 key(明文仅一次)。
async function openNewKey() {
  let groups = [];
  try { const d = await api("GET", "/members/" + S.me.id + "/usable-groups", null); groups = d.groups || []; }
  catch (e) { toast(e.message); return; }
  const opts = ['<option value="default">default(基础)</option>']
    .concat(groups.map(g => `<option value="${esc(g)}">${esc(g)}</option>`)).join("");
  modal("新建 API Key", `<div class="note">从本企业可用的模型分组里选一个;一把 key 即可调该分组里所有模型。建新 key 会替换你上一枚 key(旧 key 失效)。</div>
    <div class="fld" style="margin-top:12px"><label>模型分组</label><select id="nk_grp">${opts}</select></div>`,
    `<button class="btn pri" onclick="doCreateKey()">生成 Key</button>`);
}
async function doCreateKey() {
  try {
    const d = await api("POST", "/members/" + S.me.id + "/tokens", { group: val("nk_grp") });
    modal("新建成功 · 明文 Key(仅显示一次)", `<div class="note">请立即复制保存,关闭后只能看到脱敏串。</div>
      <div class="keybox"><span>${esc(d.api_key)}</span><span class="lk" onclick="navigator.clipboard&&navigator.clipboard.writeText('${esc(d.api_key)}');toast('已复制')">复制</span></div>`,
      `<button class="btn pri" onclick="closeM();renderView()">完成</button>`);
  } catch (e) { toast(e.message); }
}
VIEWS.myreq = async () => {
  const d = await api("GET", "/organizations/" + S.orgId + "/approvals?page=1&page_size=20", null);
  const rows = (d.list || []).map(a => `<tr><td>${esc(a.model || a.request_type)} ${money(a.amount_quota)}/${esc(a.duration)}</td><td>${pill(a.state, a.state === "approved" || a.state === "auto_approved" ? "ok" : a.state === "rejected" ? "bad" : "warn")}</td></tr>`).join("");
  return head("申请增额", "小额今日自动通过;大额或开新模型走审批")
    + `<div class="toolbar"><button class="btn pri" onclick="openSubmitReq()">+ 提交申请</button></div>
    <div class="panel"><div class="ph">我的申请</div><div class="pb"><table><tbody>${rows || '<tr><td class="empty">暂无</td></tr>'}</tbody></table></div></div>`;
};
function openSubmitReq() { modal("提交申请", `<div class="fld"><label>模型(开新模型→二审,留空=纯增额)</label><input id="sr_m" placeholder="claude-opus-4-6"></div><div class="fld"><label>申请额度(美元)</label><input id="sr_a" type="number" placeholder="20"></div><div class="fld"><label>时长</label><select id="sr_d"><option value="today">今日</option><option value="3d">3 天</option><option value="week">本周</option></select></div><div class="fld"><label>理由</label><input id="sr_r" placeholder="赶项目"></div>`, `<button class="btn" onclick="closeM()">取消</button><button class="btn pri" onclick="doSubmitReq()">提交</button>`); }
async function doSubmitReq() { try { await api("POST", "/approvals", { model: val("sr_m"), amount_quota: Math.round((parseFloat(val("sr_a")) || 0) * 500000), duration: val("sr_d"), reason: val("sr_r") }); closeM(); toast("已提交"); renderView(); } catch (e) { toast(e.message); } }
VIEWS.mynotif = async () => {
  const d = await api("GET", "/notifications?page=1&page_size=30", null);
  const rows = (d.list || []).map(n => `<tr><td>${n.is_read ? "" : '<span class="dot" style="background:var(--brand)"></span> '}${esc(n.title)}<div class="mini">${esc(n.body || "")} · ${esc(n.created_at.slice(0, 16).replace("T", " "))}</div></td></tr>`).join("");
  return head("通知", "站内通知 · 仅本人相关 · 未读 " + (d.unread || 0))
    + `<div class="toolbar"><button class="btn" onclick="markAll()">全部已读</button></div>
    <div class="panel"><table><tbody>${rows || '<tr><td class="empty">暂无通知</td></tr>'}</tbody></table></div>`;
};
async function markAll() { try { await api("POST", "/notifications/all/read", null); toast("已全部标记已读"); renderView(); } catch (e) { toast(e.message); } }

/* ---------- 进入 ---------- */
if (S.token) { boot().catch(() => { document.getElementById("login").classList.remove("hide"); }); }
else { document.getElementById("login").classList.remove("hide"); }
