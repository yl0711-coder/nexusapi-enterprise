#!/usr/bin/env node

const H = process.env.NEXUS_H || "http://127.0.0.1:18080/api/v1";
const orgID = Number(process.env.DEMO_ORG_ID || "25");
const accounts = [
  ["operator", "ops@nexus.local", "OpsPass123"],
  ["org_admin", process.env.DEMO_ADMIN_EMAIL, process.env.DEMO_ADMIN_PASSWORD],
  ["member", process.env.DEMO_MEMBER_EMAIL, process.env.DEMO_MEMBER_PASSWORD],
].filter((x) => x[1] && x[2]);

async function api(method, path, token, body) {
  const r = await fetch(H + path, {
    method,
    headers: { "Content-Type": "application/json", ...(token ? { Authorization: "Bearer " + token } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const txt = await r.text();
  let j = {};
  try {
    j = JSON.parse(txt);
  } catch {}
  if (!r.ok) throw new Error(`${method} ${path} ${r.status} ${txt}`);
  return j.data ?? j;
}

for (const [role, email, password] of accounts) {
  const d = await api("POST", "/auth/login", null, { email, password });
  const me = await api("GET", "/me", d.token);
  console.log(`${role}: login ok -> role=${me.role} org=${me.org_id} member=${me.id}`);
  if (role === "operator") {
    const orgs = await api("GET", "/organizations?page=1&page_size=50", d.token);
    const org = await api("GET", `/organizations/${orgID}`, d.token);
    const usage = await api("GET", `/organizations/${orgID}/usage?since_hours=720`, d.token);
    const logs = await api("GET", `/organizations/${orgID}/newapi-logs?page=1&page_size=5`, d.token);
    console.log("  organizations:", (orgs.list || []).slice(0, 5).map((o) => `${o.id}:${o.name}`).join(" | "));
    console.log(`  target_org=${org.name} usage_quota=${usage.total_quota} mirrored_logs=${logs.items?.length || 0}`);
  }
  if (role === "org_admin") {
    const usage = await api("GET", `/organizations/${orgID}/usage?since_hours=720`, d.token);
    const members = await api("GET", `/organizations/${orgID}/members?page=1&page_size=20`, d.token);
    const notifications = await api("GET", "/notifications?page=1&page_size=10", d.token);
    console.log(`  usage_quota=${usage.total_quota} members=${members.list?.length || 0} notifications=${notifications.list?.length || 0}`);
  }
  if (role === "member") {
    const usage = await api("GET", `/members/${me.id}/usage?since_hours=720`, d.token);
    const detail = await api("GET", `/members/${me.id}/usage/detail?since_hours=720&page_size=5`, d.token);
    const notifications = await api("GET", "/notifications?page=1&page_size=10", d.token);
    console.log(`  usage_quota=${usage.total_quota} detail=${detail.records?.length || 0} notifications=${notifications.list?.length || 0}`);
  }
}
