#!/usr/bin/env node
// 本地 UI 走查数据种子。
// 只面向 dev 栈: nexus-ent-dev + dev-newapi-1 + dev-mysql-1。
// 目标:生成一个数据相对完整的演示组织,方便分别以运营方、组织管理员、员工身份检查前端。

import { spawnSync } from "node:child_process";

const H = process.env.NEXUS_H || "http://127.0.0.1:18080";
const NEWAPI_H = process.env.NEWAPI_H || "http://127.0.0.1:13000";
const now = new Date();
const stamp = now.toISOString().replace(/[-:T.Z]/g, "").slice(0, 14);
const slug = `ui-demo-${stamp}`;
const orgGroup = `grp-${slug}`;
const tokenGroups = ["vip", "enterprise", "default"];

function die(msg) {
  console.error(`\n[seed-ui-demo] ${msg}`);
  process.exit(1);
}

function shell(cmd, args, input = "") {
  const r = spawnSync(cmd, args, { input, encoding: "utf8" });
  if (r.status !== 0) {
    die(`${cmd} ${args.join(" ")} 失败\nSTDOUT:\n${r.stdout}\nSTDERR:\n${r.stderr}`);
  }
  return r.stdout;
}

function sqlValue(v) {
  if (v === null || v === undefined) return "NULL";
  if (typeof v === "number" || typeof v === "bigint") return String(v);
  if (typeof v === "boolean") return v ? "1" : "0";
  return `'${String(v).replaceAll("\\", "\\\\").replaceAll("'", "''")}'`;
}

function mysql(db, sql) {
  return shell("docker", ["exec", "-i", "dev-mysql-1", "mysql", "--default-character-set=utf8mb4", "-uroot", "-pdevroot", "-N", "-B", db], sql);
}

async function api(method, path, token, body) {
  const headers = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(`${H}/api/v1${path}`, {
    method,
    headers,
    body: body === undefined || body === null ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { raw: text };
  }
  if (!res.ok) {
    die(`${method} ${path} -> ${res.status}\n${text}`);
  }
  return data.data ?? data;
}

async function newapi(method, path, token, body) {
  const headers = { Authorization: `Bearer ${token}`, "New-Api-User": "1", "Content-Type": "application/json" };
  const res = await fetch(`${NEWAPI_H}${path}`, {
    method,
    headers,
    body: body === undefined || body === null ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { raw: text };
  }
  if (!res.ok || data.success === false) {
    die(`new-api ${method} ${path} 失败 -> ${res.status}\n${text}`);
  }
  return data.data ?? data;
}

function newapiAdminTokenFromServer() {
  const env = shell("docker", ["inspect", "nexus-ent-dev", "--format", "{{range .Config.Env}}{{println .}}{{end}}"]);
  const line = env.split("\n").find((x) => x.startsWith("NEWAPI_ADMIN_TOKEN="));
  if (!line) die("未从 nexus-ent-dev 读取到 NEWAPI_ADMIN_TOKEN。请先执行 bash dev/run-server.sh");
  return line.slice("NEWAPI_ADMIN_TOKEN=".length);
}

async function ensureUsableGroups(userGroup, groups) {
  const tok = newapiAdminTokenFromServer();
  const rows = await newapi("GET", "/api/option/", tok);
  const arr = Array.isArray(rows) ? rows : [];
  const row = arr.find((x) => x.key === "group_ratio_setting.group_special_usable_group");
  let value = {};
  if (row?.value) {
    try {
      value = JSON.parse(row.value);
    } catch {
      value = {};
    }
  }
  value[userGroup] ||= {};
  for (const g of groups) value[userGroup][g] = `UI 演示可用分组 ${g}`;
  await newapi("PUT", "/api/option/", tok, {
    key: "group_ratio_setting.group_special_usable_group",
    value: JSON.stringify(value),
  });
}

function firstCol(sql) {
  return mysql("nexus", sql).trim().split(/\s+/)[0] || "";
}

function queryMembers(orgID) {
  const out = mysql(
    "nexus",
    `SELECT m.id, COALESCE(m.display_name,''), m.login_email, COALESCE(m.team_id,0), COALESCE(m.tier_id,0),
            COALESCE(m.newapi_token_id,0), COALESCE(s.id,0), COALESCE(t.token_name,''), COALESCE(m.key_masked,'')
       FROM member m
       LEFT JOIN member_key_slot s ON s.member_id=m.id AND s.org_id=m.org_id AND s.is_primary=1
       LEFT JOIN member_key_token t ON t.key_id=s.id AND t.is_current=1
      WHERE m.org_id=${sqlValue(orgID)} AND m.deleted_at IS NULL
      ORDER BY m.id;`
  ).trim();
  if (!out) return [];
  return out.split("\n").map((line) => {
    const [id, name, email, teamID, tierID, tokenID, keyID, tokenName, keyMasked] = line.split("\t");
    return {
      id: Number(id),
      name,
      email,
      teamID: Number(teamID),
      tierID: Number(tierID),
      tokenID: Number(tokenID),
      keyID: Number(keyID),
      tokenName,
      keyMasked,
    };
  });
}

function seedShowcaseData({ orgID, orgNewapiUserID, teams, members }) {
  const normalMembers = members.filter((m) => !m.email.startsWith("admin@"));
  const admin = members.find((m) => m.email.startsWith("admin@")) || members[0];
  const baseLog = Date.now() * 1000;
  const models = [
    ["claude-sonnet-4-5-20250929", 95000, 6200],
    ["claude-opus-4-6", 138000, 9100],
    ["gpt-5-mini", 72000, 4800],
    ["gpt-4o-mini", 36000, 2600],
    ["deepseek-chat", 18000, 1400],
    ["glm-4-plus", 22000, 1700],
  ];
  const usageLedgerRows = [];
  const usageDetailRows = [];
  const mirrorRows = [];
  const bucket0 = new Date(Date.now());
  bucket0.setMinutes(0, 0, 0);

  let idx = 0;
  for (let d = 0; d < 14; d++) {
    for (let mi = 0; mi < models.length; mi++) {
      const member = normalMembers[(d + mi) % normalMembers.length];
      const [model, basePrompt, baseCompletion] = models[mi];
      const ts = new Date(bucket0.getTime() - (d * 24 + mi * 2) * 3600 * 1000);
      const prompt = basePrompt + d * 173 + mi * 211;
      const completion = baseCompletion + d * 71 + mi * 89;
      const quota = Math.round((prompt * 0.006 + completion * 0.028) * (mi >= 4 ? 0.35 : 1) * 650);
      const logID = baseLog + idx;
      const requestID = `demo-${stamp}-${String(idx).padStart(3, "0")}`;
      usageLedgerRows.push([
        orgID,
        member.id,
        orgNewapiUserID,
        member.keyID,
        member.teamID || null,
        model,
        ts,
        prompt,
        completion,
        quota,
        ts,
        logID,
      ]);
      usageDetailRows.push([
        orgID,
        member.id,
        orgNewapiUserID,
        member.keyID,
        member.teamID || null,
        model,
        logID,
        prompt,
        completion,
        quota,
        ts,
      ]);
      mirrorRows.push([
        orgID,
        member.id,
        member.keyID,
        orgNewapiUserID,
        member.tokenID,
        member.tokenName,
        2,
        model,
        12 + (mi % 4),
        ["OpenAI-企业池", "Claude-编程池", "Gemini-备用池", "DeepSeek-经济池"][mi % 4],
        ["vip", "enterprise", "internal_test"][mi % 3],
        requestID,
        quota,
        prompt,
        completion,
        2 + ((d + mi) % 19),
        1,
        `调用成功: ${model} / ${member.name}`,
        ["203.0.113.12", "198.51.100.8", "192.0.2.45"][mi % 3],
        JSON.stringify({ latency_ms: 1200 + mi * 300, source: "ui-demo" }),
        logID,
        ts,
      ]);
      idx++;
    }
  }

  for (let i = 0; i < 6; i++) {
    const member = normalMembers[i % normalMembers.length];
    const ts = new Date(bucket0.getTime() - (i + 1) * 37 * 60 * 1000);
    const logID = baseLog + idx + i;
    mirrorRows.push([
      orgID,
      member.id,
      member.keyID,
      orgNewapiUserID,
      member.tokenID,
      member.tokenName,
      5,
      ["claude-opus-4-6", "gpt-5-mini"][i % 2],
      20 + (i % 3),
      ["Kiro", "wf", "taokenAI"][i % 3],
      ["vip", "internal_test"][i % 2],
      `demo-error-${stamp}-${i}`,
      0,
      0,
      0,
      0,
      1,
      i % 2 === 0 ? "status_code=503, upstream overloaded" : "status_code=524, upstream timeout",
      "203.0.113.77",
      JSON.stringify({ source: "ui-demo", severity: i % 2 === 0 ? "warn" : "error" }),
      logID,
      ts,
    ]);
  }

  const total = usageLedgerRows.reduce((s, r) => s + r[9], 0);
  const recharged = 80000000;
  const balance = recharged - total;
  const sql = [];

  sql.push(`INSERT INTO company_balance (org_id,total_recharged,total_consumed,balance,low_watermark,version)
VALUES (${orgID},${recharged},${total},${balance},5000000,1)
ON DUPLICATE KEY UPDATE total_recharged=VALUES(total_recharged),total_consumed=VALUES(total_consumed),balance=VALUES(balance),low_watermark=VALUES(low_watermark),version=version+1;`);

  sql.push(`INSERT INTO usage_ledger
(org_id,member_id,newapi_user_id,key_id,team_id,model_name,time_bucket,prompt_tokens,completion_tokens,consumed_quota,log_max_ts,log_max_id)
VALUES ${usageLedgerRows
    .map((r) => `(${r.map((v) => (v instanceof Date ? sqlValue(v.toISOString().slice(0, 19).replace("T", " ")) : sqlValue(v))).join(",")})`)
    .join(",\n")}
ON DUPLICATE KEY UPDATE prompt_tokens=VALUES(prompt_tokens),completion_tokens=VALUES(completion_tokens),consumed_quota=VALUES(consumed_quota),log_max_ts=VALUES(log_max_ts),log_max_id=VALUES(log_max_id);`);

  sql.push(`INSERT IGNORE INTO usage_detail
(org_id,member_id,newapi_user_id,key_id,team_id,model_name,newapi_log_id,prompt_tokens,completion_tokens,consumed_quota,log_ts)
VALUES ${usageDetailRows
    .map((r) => `(${r.map((v) => (v instanceof Date ? sqlValue(v.toISOString().slice(0, 19).replace("T", " ")) : sqlValue(v))).join(",")})`)
    .join(",\n")};`);

  sql.push(`INSERT IGNORE INTO org_newapi_log
(org_id,member_id,key_id,newapi_user_id,newapi_token_id,token_name,log_type,model_name,channel_id,channel_name,group_name,request_id,quota,prompt_tokens,completion_tokens,use_time,is_stream,content,ip,other,newapi_log_id,log_ts)
VALUES ${mirrorRows
    .map((r) => `(${r.map((v) => (v instanceof Date ? sqlValue(v.toISOString().slice(0, 19).replace("T", " ")) : sqlValue(v))).join(",")})`)
    .join(",\n")};`);

  const notes = [
    [admin.id, "balance_low", "余额提醒", "当前演示组织余额低于预设水位时,这里会展示提醒。"],
    [admin.id, "quota_reset", "周期额度已重置", "研发中心与客服支持团队额度已按演示周期重置。"],
    [normalMembers[0].id, "grant_expire", "临时额度即将到期", "你的临时额度将在 24 小时内到期,如需继续使用请联系管理员。"],
    [normalMembers[0].id, "approval_result", "模型权限已开通", "高级编程档已允许使用 Claude / GPT 相关模型。"],
    [normalMembers[1].id, "quota_reset", "本周额度已刷新", "你的本周额度已刷新,可以继续使用企业 API Key。"],
  ].filter(([memberID]) => !!memberID);
  sql.push(`INSERT INTO notification (org_id,member_id,type,title,body,is_read,created_at)
VALUES ${notes
    .map((n, i) => `(${orgID},${sqlValue(n[0])},${sqlValue(n[1])},${sqlValue(n[2])},${sqlValue(n[3])},${i === 1 ? 1 : 0},DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ${i + 1} HOUR))`)
    .join(",\n")};`);

  const teamIDs = Object.values(teams);
  if (normalMembers[2]?.id && teamIDs[2]) {
    sql.push(`UPDATE member SET status='disabled' WHERE id=${normalMembers[2].id} AND org_id=${orgID};`);
  }
  if (normalMembers[3]?.id) {
    sql.push(`UPDATE member SET role='team_leader' WHERE id=${normalMembers[3].id} AND org_id=${orgID};`);
    if (normalMembers[3].teamID) {
      sql.push(`UPDATE team SET leader_member_id=${normalMembers[3].id} WHERE id=${normalMembers[3].teamID} AND org_id=${orgID};`);
    }
  }

  mysql("nexus", sql.join("\n"));
}

async function main() {
  console.log("[seed-ui-demo] 检查本地 dev 服务...");
  const ready = await fetch(`${H}/readyz`).then((r) => r.text()).catch(() => "");
  if (!ready.includes("ready") && !ready.includes("ok")) die(`企业后台未就绪: ${H}/readyz。请先执行 bash dev/run-server.sh`);
  await ensureUsableGroups(orgGroup, tokenGroups);

  console.log("[seed-ui-demo] 创建演示组织与账号...");
  const op = await api("POST", "/auth/login", null, { email: "ops@nexus.local", password: "OpsPass123" });
  const opToken = op.token;
  const org = await api("POST", "/organizations", opToken, {
    name: `演示公司-${stamp}`,
    slug,
    admin_email: `admin@${slug}.com`,
    newapi_user_group: orgGroup,
  });
  const orgID = org.org?.id || org.id;
  if (!orgID) die(`创建组织响应缺少 org.id: ${JSON.stringify(org)}`);
  const adminEmail = `admin@${slug}.com`;
  const adminPassword = org.admin_initial_password;
  const admin = await api("POST", "/auth/login", null, { email: adminEmail, password: adminPassword });
  const adminToken = admin.token;

  const teams = {};
  for (const name of ["研发中心", "市场运营", "客服支持"]) {
    const t = await api("POST", `/organizations/${orgID}/teams`, adminToken, { name });
    teams[name] = t.id;
  }
  const tiers = {};
  const tierSpecs = [
    { name: "标准模型档", newapi_group: "vip", monthly_limit: 30000000, model_set: ["gpt-5-mini", "gpt-4o-mini", "deepseek-chat"] },
    { name: "高级编程档", newapi_group: "enterprise", monthly_limit: 90000000, model_set: ["claude-sonnet-4-5-20250929", "claude-opus-4-6", "gpt-5-mini"] },
    { name: "内部测试档", newapi_group: "enterprise", monthly_limit: 150000000, model_set: ["claude-haiku-4-5-20251001", "claude-opus-4-6", "qwen-plus"] },
  ];
  for (const spec of tierSpecs) {
    const t = await api("POST", `/organizations/${orgID}/tiers`, adminToken, spec);
    tiers[spec.name] = t.id;
  }
  await api("POST", `/tiers/${tiers["高级编程档"]}/default`, adminToken, null);

  const memberSpecs = [
    ["钱晨", "研发中心", "高级编程档"],
    ["林晚", "市场运营", "标准模型档"],
    ["周然", "客服支持", "标准模型档"],
    ["何雨", "研发中心", "内部测试档"],
    ["赵明", "客服支持", "高级编程档"],
  ];
  const createdMembers = [];
  for (const [name, teamName, tierName] of memberSpecs) {
    const m = await api("POST", `/organizations/${orgID}/members`, adminToken, {
      name,
      team_id: teams[teamName],
      tier_id: tiers[tierName],
    });
    createdMembers.push({ name, email: m.login_email, password: m.initial_password, id: m.member_id });
  }

  const orgNewapiUserID = Number(firstCol(`SELECT COALESCE(newapi_user_id,0) FROM organization WHERE id=${orgID};`));
  const members = queryMembers(orgID);
  if (!orgNewapiUserID || members.length < 3) die("组织或成员创建后未能查询到完整 new-api 归属数据");

  console.log("[seed-ui-demo] 写入报表、日志、通知等展示数据...");
  seedShowcaseData({ orgID, orgNewapiUserID, teams, members });

  const employee = createdMembers[0];
  const disabled = createdMembers[2];
  console.log("\n========== 企业后台 UI 演示数据已生成 ==========");
  console.log(`入口: ${H}`);
  console.log(`组织: 演示公司-${stamp} (org_id=${orgID}, slug=${slug})`);
  console.log("");
  console.log("超管/运营方:");
  console.log("  ops@nexus.local / OpsPass123");
  console.log("");
  console.log("组织管理员:");
  console.log(`  ${adminEmail} / ${adminPassword}`);
  console.log("");
  console.log("员工账号:");
  console.log(`  ${employee.email} / ${employee.password}  (${employee.name}, active)`);
  console.log(`  ${disabled.email} / ${disabled.password}  (${disabled.name}, 已禁用,用于成员列表状态展示;不要用它做员工登录)`);
  console.log("");
  console.log("建议走查:");
  console.log("  1. 超管登录 -> 客户组织 -> 打开该演示公司 -> 概览/成员/API Key 归属/请求日志/支持会话/风险控制");
  console.log("  2. 组织管理员登录 -> 概览/成员/团队/可用模型档位/余额/通知");
  console.log("  3. 员工登录 -> 我的用量/我的 API Key/通知");
  console.log("==============================================\n");
}

main().catch((e) => die(e?.stack || e?.message || String(e)));
