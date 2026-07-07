#!/usr/bin/env node

import { mkdirSync, writeFileSync } from "node:fs";

const APP = process.env.NEXUS_APP || "http://127.0.0.1:18080";
const API = `${APP}/api/v1`;
const CDP = process.env.CDP || "http://127.0.0.1:9224";
const outDir = process.env.OUT_DIR || "/private/tmp/nexus-enterprise-ui-audit";
const orgID = Number(process.env.DEMO_ORG_ID || "25");
const orgName = process.env.DEMO_ORG_NAME || "演示公司";

const roles = [
  {
    name: "01-operator-orgs",
    email: "ops@nexus.local",
    password: "OpsPass123",
    steps: [
      ["orgs", null],
      ["org-detail-overview", `enterOrg(${orgID}, ${JSON.stringify(orgName)})`],
      ["org-detail-members", `switchOrgTab("members")`],
      ["org-detail-tokens", `switchOrgTab("tokens")`],
      ["org-detail-logs", `switchOrgTab("logs")`],
      ["org-detail-risk", `switchOrgTab("risk")`],
    ],
  },
  {
    name: "02-org-admin",
    email: process.env.DEMO_ADMIN_EMAIL,
    password: process.env.DEMO_ADMIN_PASSWORD,
    steps: [
      ["dash", null],
      ["members", `go("members")`],
      ["teams", `go("teams")`],
      ["tiers", `go("tiers")`],
      ["billing", `go("billing")`],
      ["notifications", `go("mynotif")`],
    ],
  },
  {
    name: "03-member",
    email: process.env.DEMO_MEMBER_EMAIL,
    password: process.env.DEMO_MEMBER_PASSWORD,
    steps: [
      ["myusage", null],
      ["mykey", `go("mykey")`],
      ["notifications", `go("mynotif")`],
    ],
  },
].filter((r) => r.email && r.password);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function login(email, password) {
  const r = await fetch(`${API}/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  const j = await r.json();
  if (!r.ok || j.code !== 0) throw new Error(`login failed: ${email} ${r.status} ${JSON.stringify(j)}`);
  return j.data.token;
}

async function newTarget() {
  const r = await fetch(`${CDP}/json/new?${encodeURIComponent(APP)}`, { method: "PUT" });
  if (!r.ok) throw new Error(`create cdp target failed ${r.status} ${await r.text()}`);
  return r.json();
}

class Client {
  constructor(url) {
    this.id = 0;
    this.pending = new Map();
    this.ws = new WebSocket(url);
  }
  async open() {
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve, { once: true });
      this.ws.addEventListener("error", reject, { once: true });
    });
    this.ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
      }
    });
  }
  call(method, params = {}) {
    const id = ++this.id;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
  }
}

async function captureRole(role) {
  const token = await login(role.email, role.password);
  const target = await newTarget();
  const c = new Client(target.webSocketDebuggerUrl);
  await c.open();
  await c.call("Page.enable");
  await c.call("Runtime.enable");
  await c.call("Emulation.setDeviceMetricsOverride", {
    width: 1440,
    height: 1100,
    deviceScaleFactor: 1,
    mobile: false,
  });
  await c.call("Page.navigate", { url: APP });
  await sleep(800);
  await c.call("Runtime.evaluate", {
    expression: `localStorage.setItem("nx_token", ${JSON.stringify(token)}); location.reload();`,
  });
  await sleep(1800);
  for (const [name, expression] of role.steps) {
    if (expression) {
      await c.call("Runtime.evaluate", { expression: `(async()=>{ ${expression}; })()`, awaitPromise: true });
      await sleep(1600);
    }
    const shot = await c.call("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
    const file = `${outDir}/${role.name}-${name}.png`;
    writeFileSync(file, Buffer.from(shot.data, "base64"));
    console.log(file);
  }
  c.ws.close();
}

mkdirSync(outDir, { recursive: true });
for (const role of roles) {
  await captureRole(role);
}
