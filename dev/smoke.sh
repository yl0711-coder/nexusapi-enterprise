#!/usr/bin/env bash
# 里程碑1 接口联调走查:对常驻 dev 栈(平台 server + 真实 new-api + MySQL)逐个端点打一遍,
# 含 US-01 真机代发 key、列表脱敏、轮换、RBAC(403/404/401)。需先 bash dev/run-server.sh。
set -uo pipefail

H="http://localhost:18080"
SESSION_KEY="dev-only-session-signing-key-32bytes!!"   # 与 run-server.sh 一致(仅 dev)
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
hdr()  { printf '\n=== %s ===\n' "$*"; }

# req METHOD PATH TOKEN BODY  -> 设全局 CODE / BODY
req() {
  local m="$1" p="$2" t="${3:-}" b="${4:-}"; local args=(-s -o /tmp/smk_b -w '%{http_code}' -X "$m" "$H$p")
  [ -n "$t" ] && args+=(-H "Authorization: Bearer $t")
  [ -n "$b" ] && args+=(-H 'Content-Type: application/json' -d "$b")
  CODE="$(curl "${args[@]}")"; BODY="$(cat /tmp/smk_b)"
}
field() { printf '%s' "$BODY" | grep -oE "\"$1\":(\"[^\"]*\"|[0-9]+)" | head -1 | sed -E "s/\"$1\"://; s/\"//g"; }

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
mint_token() { # mid oid role tid
  local now exp payload p sig
  now="$(date +%s)"; exp=$((now+3600))
  payload="{\"mid\":$1,\"oid\":$2,\"role\":\"$3\",\"tid\":$4,\"iat\":$now,\"exp\":$exp}"
  p="$(printf '%s' "$payload" | b64url)"
  sig="$(printf '%s' "$p" | openssl dgst -sha256 -hmac "$SESSION_KEY" -binary | b64url)"
  printf '%s.%s' "$p" "$sig"
}

SLUG="demo-$(date +%s)"

hdr "1) 运营方登录"
req POST /api/v1/auth/login "" "{\"email\":\"ops@nexus.local\",\"password\":\"OpsPass123\"}"
OP_TOK="$(field token)"
[ "$CODE" = 200 ] && [ -n "$OP_TOK" ] && ok "登录 200,拿到会话 token" || bad "登录失败 CODE=$CODE BODY=$BODY"

hdr "2) 运营方建客户组织(连带建组织管理员)"
req POST /api/v1/organizations "$OP_TOK" "{\"name\":\"演示公司\",\"slug\":\"$SLUG\",\"admin_email\":\"admin@$SLUG.com\"}"
ORG_ID="$(field id)"; ADMIN_PW="$(field admin_initial_password)"
[ "$CODE" = 201 ] && [ -n "$ORG_ID" ] && [ -n "$ADMIN_PW" ] && ok "建组织 201,org_id=$ORG_ID,管理员初始密码已回显一次" || bad "建组织失败 CODE=$CODE BODY=$BODY"

hdr "3) 组织管理员登录"
req POST /api/v1/auth/login "" "{\"email\":\"admin@$SLUG.com\",\"password\":\"$ADMIN_PW\"}"
AD_TOK="$(field token)"
[ "$CODE" = 200 ] && [ -n "$AD_TOK" ] && ok "管理员登录 200" || bad "管理员登录失败 CODE=$CODE BODY=$BODY"

hdr "4) 管理员建团队 + 层级 + 设默认层级"
req POST "/api/v1/organizations/$ORG_ID/teams" "$AD_TOK" "{\"name\":\"研发一组\"}"
TEAM_ID="$(field id)"; [ "$CODE" = 201 ] && ok "建团队 201 team_id=$TEAM_ID" || bad "建团队 CODE=$CODE BODY=$BODY"
req POST "/api/v1/organizations/$ORG_ID/tiers" "$AD_TOK" "{\"name\":\"标准档\",\"model_set\":[\"gpt-5-mini\",\"claude-sonnet-4-5-20250929\"],\"newapi_group\":\"default\",\"monthly_limit\":25000000}"
TIER_ID="$(field id)"; [ "$CODE" = 201 ] && ok "建层级 201 tier_id=$TIER_ID" || bad "建层级 CODE=$CODE BODY=$BODY"
req POST "/api/v1/tiers/$TIER_ID/default" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "设默认层级 200" || bad "设默认 CODE=$CODE BODY=$BODY"

hdr "5) US-01 开通成员(真机代发 key,打 dev new-api)"
req POST "/api/v1/organizations/$ORG_ID/members" "$AD_TOK" "{\"name\":\"钱晨\",\"team_id\":$TEAM_ID,\"tier_id\":$TIER_ID}"
MEMBER_ID="$(field member_id)"; APIKEY="$(field api_key)"; MASKED="$(field key_masked)"; NUID="$(field newapi_user_id)"
if [ "$CODE" = 201 ] && [ -n "$APIKEY" ] && [ -n "$NUID" ]; then
  ok "开通成员 201 member_id=$MEMBER_ID newapi_user_id=$NUID"
  printf '       明文 key(仅此一次): %s\n       脱敏: %s\n' "$APIKEY" "$MASKED"
else bad "开通成员 CODE=$CODE BODY=$BODY"; fi

hdr "6) 成员列表(必须脱敏:明文 key 绝不出现)"
req GET "/api/v1/organizations/$ORG_ID/members?page=1&page_size=20" "$AD_TOK" ""
if [ "$CODE" = 200 ] && ! printf '%s' "$BODY" | grep -q "$APIKEY"; then
  ok "列表 200 且无明文 key(只见脱敏串)"
else bad "列表泄露明文 key 或失败 CODE=$CODE"; fi

hdr "7) 成员详情"
req GET "/api/v1/members/$MEMBER_ID" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "详情 200 status=$(field status) bootstrap_state=$(field bootstrap_state)" || bad "详情 CODE=$CODE BODY=$BODY"

hdr "8) 成员本人轮换 key(自签成员会话)"
M_TOK="$(mint_token "$MEMBER_ID" "$ORG_ID" member "$TEAM_ID")"
req POST "/api/v1/members/$MEMBER_ID/key:rotate" "$M_TOK" ""
NEWKEY="$(field api_key)"
if [ "$CODE" = 200 ] && [ -n "$NEWKEY" ] && [ "$NEWKEY" != "$APIKEY" ]; then
  ok "轮换 200,得到不同的新 key:$NEWKEY"
else bad "轮换 CODE=$CODE BODY=$BODY"; fi

hdr "9) RBAC 越权判定"
req POST "/api/v1/organizations/$ORG_ID/members" "$OP_TOK" "{\"name\":\"x\",\"tier_id\":$TIER_ID}"
[ "$CODE" = 403 ] && ok "运营方直接开通成员 → 403(E05,需走支持会话)" || bad "应 403 得 $CODE"
req POST "/api/v1/organizations/$ORG_ID/members" "$M_TOK" "{\"name\":\"x\",\"tier_id\":$TIER_ID}"
[ "$CODE" = 403 ] && ok "成员开通成员 → 403" || bad "应 403 得 $CODE"
req GET "/api/v1/organizations/999999/members" "$AD_TOK" ""
[ "$CODE" = 404 ] && ok "跨 org 访问 → 404(不暴露存在性)" || bad "应 404 得 $CODE"
req GET /api/v1/me "" ""
[ "$CODE" = 401 ] && ok "无 token 访问 → 401" || bad "应 401 得 $CODE"

hdr "10) 里程碑2 额度执行:调额(US-03)"
req POST "/api/v1/members/$MEMBER_ID/quota:adjust" "$AD_TOK" "{\"delta_quota\":5000000,\"duration\":\"today\",\"reason\":\"赶项目\"}"
GRANT_ID="$(field grant_id)"; NEWCAP="$(field new_cap_quota)"
[ "$CODE" = 200 ] && [ -n "$GRANT_ID" ] && ok "调额 200,new_cap_quota=$NEWCAP grant_id=$GRANT_ID" || bad "调额 CODE=$CODE BODY=$BODY"

hdr "11) 列临时权限 + 撤销(回退)"
req GET "/api/v1/members/$MEMBER_ID/grants" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "列 grants 200 total=$(field total)" || bad "列 grants CODE=$CODE"
req DELETE "/api/v1/grants/$GRANT_ID" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "撤销 grant 200(override 回退基线)" || bad "撤销 CODE=$CODE BODY=$BODY"

hdr "12) 临时账号有效期 account_ttl(US-04a)"
EXP="$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"
req POST "/api/v1/members/$MEMBER_ID/grants" "$AD_TOK" "{\"type\":\"account_ttl\",\"expire_at\":\"$EXP\",\"reason\":\"实习生\"}"
[ "$CODE" = 201 ] && ok "设 account_ttl 201(到期由 worker 反向停号)" || bad "account_ttl CODE=$CODE BODY=$BODY"

hdr "13) 停用 / 恢复成员(US-05)"
req POST "/api/v1/members/$MEMBER_ID/status" "$AD_TOK" "{\"enabled\":false}"
[ "$CODE" = 200 ] && ok "停用 200 status=$(field status)" || bad "停用 CODE=$CODE BODY=$BODY"
req POST "/api/v1/members/$MEMBER_ID/status" "$AD_TOK" "{\"enabled\":true}"
[ "$CODE" = 200 ] && ok "恢复 200 status=$(field status)" || bad "恢复 CODE=$CODE BODY=$BODY"

hdr "14) RBAC:成员自己调额 → 403(只有管理员/团队负责人可调)"
req POST "/api/v1/members/$MEMBER_ID/quota:adjust" "$M_TOK" "{\"delta_quota\":1,\"duration\":\"today\"}"
[ "$CODE" = 403 ] && ok "成员调额 → 403" || bad "应 403 得 $CODE"

hdr "15) 里程碑3a 计费:充值入账(US-08,运营方)"
TR="TR-$(date +%s)-$RANDOM"
req POST "/api/v1/organizations/$ORG_ID/recharges" "$OP_TOK" "{\"amount_quota\":10000000,\"transfer_no\":\"$TR\",\"note\":\"Q2预付\"}"
BAL_AFTER="$(field balance_quota_after)"
[ "$CODE" = 201 ] && [ "$BAL_AFTER" = 10000000 ] && ok "入账 201,balance_after=$BAL_AFTER" || bad "入账 CODE=$CODE BODY=$BODY"
req GET "/api/v1/organizations/$ORG_ID/balance" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "余额查询 200 balance_quota=$(field balance_quota)" || bad "余额查询 CODE=$CODE"
req POST "/api/v1/organizations/$ORG_ID/recharges" "$OP_TOK" "{\"amount_quota\":10000000,\"transfer_no\":\"$TR\"}"
[ "$CODE" = 409 ] && ok "入账幂等:重复 transfer_no → 409" || bad "应 409 得 $CODE"
req POST "/api/v1/organizations/$ORG_ID/recharges" "$AD_TOK" "{\"amount_quota\":1,\"transfer_no\":\"X\"}"
[ "$CODE" = 403 ] && ok "动钱红线:组织管理员入账 → 403" || bad "应 403 得 $CODE"

hdr "16) 申请充值(US-09,组织管理员,不改余额)"
req POST "/api/v1/organizations/$ORG_ID/recharge-requests" "$AD_TOK" "{\"type\":\"topup\",\"amount_quota\":5000000,\"note\":\"补预付\"}"
[ "$CODE" = 201 ] && ok "申请充值 201 status=$(field status)" || bad "申请充值 CODE=$CODE BODY=$BODY"
req POST "/api/v1/organizations/$ORG_ID/recharge-requests" "$M_TOK" "{\"type\":\"topup\",\"amount_quota\":1}"
[ "$CODE" = 403 ] && ok "成员申请充值 → 403(计费子集仅组织管理员)" || bad "应 403 得 $CODE"

hdr "17) 里程碑3b 计费开关(逐组织灰度,仅运营方)"
req PATCH "/api/v1/organizations/$ORG_ID/billing-settings" "$OP_TOK" "{\"billing_enabled\":true,\"hard_stop_enabled\":false,\"low_watermark_quota\":1000000}"
[ "$CODE" = 200 ] && ok "运营方开计费 200 billing_enabled=$(field billing_enabled)" || bad "开计费 CODE=$CODE BODY=$BODY"
req GET "/api/v1/organizations/$ORG_ID/billing-settings" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "查计费开关 200(组织管理员可读)" || bad "查开关 CODE=$CODE"
req PATCH "/api/v1/organizations/$ORG_ID/billing-settings" "$AD_TOK" "{\"billing_enabled\":false}"
[ "$CODE" = 403 ] && ok "组织管理员改计费开关 → 403(仅运营方)" || bad "应 403 得 $CODE"
# 关回去,避免 dev 误扣(本地无真实用量,但保持干净)
req PATCH "/api/v1/organizations/$ORG_ID/billing-settings" "$OP_TOK" "{\"billing_enabled\":false}" >/dev/null

hdr "18) 里程碑3c 计价/折扣联动(总折扣,仅运营方)"
req PUT "/api/v1/organizations/$ORG_ID/pricing" "$OP_TOK" "{\"mode\":\"total\",\"newapi_group\":\"org$ORG_ID\",\"group_ratio\":0.8}"
[ "$CODE" = 200 ] && ok "配总折扣 200 回显 upstream=$(field group_ratio_upstream)" || bad "配折扣 CODE=$CODE BODY=$BODY"
req GET "/api/v1/organizations/$ORG_ID/pricing" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "组织管理员只读折扣 200" || bad "读折扣 CODE=$CODE"
req PUT "/api/v1/organizations/$ORG_ID/pricing" "$AD_TOK" "{\"mode\":\"total\",\"group_ratio\":0.5}"
[ "$CODE" = 403 ] && ok "组织管理员配折扣 → 403(客户只读,仅运营方可配)" || bad "应 403 得 $CODE"

hdr "19) 里程碑4 申请-审批 + 通知(成员自助)"
req POST "/api/v1/approvals" "$M_TOK" "{\"amount_quota\":10000000,\"duration\":\"today\",\"reason\":\"赶工\"}"
[ "$CODE" = 201 ] && [ "$(field state)" = auto_approved ] && ok "小额申请自动通过 201(即时下发)" || bad "自动通过 CODE=$CODE BODY=$BODY"
req GET "/api/v1/notifications" "$M_TOK" ""
[ "$CODE" = 200 ] && ok "成员站内通知 200 unread=$(field unread)" || bad "通知 CODE=$CODE"
req POST "/api/v1/approvals" "$M_TOK" "{\"amount_quota\":200000000000,\"duration\":\"today\",\"reason\":\"大项目\"}"
BIG_ID="$(field id)"
[ "$CODE" = 201 ] && [ "$(field state)" = pending ] && ok "大额申请 → pending(待审,二审档)" || bad "大额 CODE=$CODE BODY=$BODY"
req POST "/api/v1/approvals/$BIG_ID/decide" "$M_TOK" "{\"approved\":true}"
[ "$CODE" = 403 ] && ok "成员裁决自己的申请 → 403" || bad "应 403 得 $CODE"
req POST "/api/v1/approvals/$BIG_ID/decide" "$AD_TOK" "{\"approved\":true}"
[ "$CODE" = 200 ] && ok "组织管理员一审 200 state=$(field state)" || bad "一审 CODE=$CODE BODY=$BODY"
req POST "/api/v1/approvals/$BIG_ID/decide" "$AD_TOK" "{\"approved\":true}"
[ "$CODE" = 200 ] && ok "组织管理员二审 200 state=$(field state)(终批+下发)" || bad "二审 CODE=$CODE BODY=$BODY"

hdr "20) 里程碑5 用量看板 + 运营方三层支持"
req GET "/api/v1/organizations/$ORG_ID/usage?since_hours=24" "$AD_TOK" ""
[ "$CODE" = 200 ] && ok "组织用量看板 200" || bad "用量 CODE=$CODE"
req POST "/api/v1/organizations/$ORG_ID/support-sessions" "$OP_TOK" "{\"scope\":\"readonly\",\"ttl_seconds\":600,\"reason\":\"排障\"}"
SUP_TOK="$(field token)"; SUP_ID="$(field session_id)"
[ "$CODE" = 201 ] && [ -n "$SUP_TOK" ] && ok "开只读支持会话 201" || bad "开会话 CODE=$CODE BODY=$BODY"
req GET "/api/v1/organizations/$ORG_ID/members" "$SUP_TOK" ""
[ "$CODE" = 200 ] && ok "只读支持态能读成员列表 200" || bad "只读读 CODE=$CODE"
req POST "/api/v1/organizations/$ORG_ID/members" "$SUP_TOK" "{\"name\":\"x\"}"
[ "$CODE" = 403 ] && ok "只读支持态写 → 403(后端闸)" || bad "应 403 得 $CODE"
req POST "/api/v1/support-sessions/$SUP_ID/close" "$OP_TOK" ""
[ "$CODE" = 200 ] && ok "结束支持会话 200" || bad "结束 CODE=$CODE"

# 放最后:此步会调 GET /api/user/token 旋转 root token,放末尾避免作废 server 持有的管理员 token。
hdr "21) 零侵入核对:new-api 侧真建了该用户"
NU="$(curl -s "http://localhost:13000/api/user/search?keyword=o${ORG_ID}m${MEMBER_ID}" \
  -H "Authorization: Bearer $(curl -s -c /tmp/j -X POST http://localhost:13000/api/user/login -H 'Content-Type: application/json' -d '{"username":"root","password":"RootPass123"}' >/dev/null; curl -s -b /tmp/j -H 'New-Api-User: 1' http://localhost:13000/api/user/token | grep -oE '"data":"[^"]+"' | sed -E 's/.*:"([^"]+)"/\1/')" \
  -H 'New-Api-User: 1' 2>/dev/null | grep -oE "\"username\":\"o${ORG_ID}m${MEMBER_ID}\"" | head -1)"
[ -n "$NU" ] && ok "new-api 侧存在用户 $NU(平台经官方 API 真建,非 mock)" || bad "未在 new-api 找到该用户"

printf '\n==== 联调结果: \033[32m%d 通过\033[0m / \033[31m%d 失败\033[0m ====\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
