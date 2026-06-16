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
req POST "/api/v1/organizations/$ORG_ID/tiers" "$AD_TOK" "{\"name\":\"标准档\",\"model_set\":[\"gpt-5-mini\",\"claude-sonnet-4-5-20250929\"],\"newapi_group\":\"default\"}"
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

hdr "10) 零侵入核对:new-api 侧真建了该用户"
NU="$(curl -s "http://localhost:13000/api/user/search?keyword=o${ORG_ID}m${MEMBER_ID}" \
  -H "Authorization: Bearer $(curl -s -c /tmp/j -X POST http://localhost:13000/api/user/login -H 'Content-Type: application/json' -d '{"username":"root","password":"RootPass123"}' >/dev/null; curl -s -b /tmp/j -H 'New-Api-User: 1' http://localhost:13000/api/user/token | grep -oE '"data":"[^"]+"' | sed -E 's/.*:"([^"]+)"/\1/')" \
  -H 'New-Api-User: 1' 2>/dev/null | grep -oE "\"username\":\"o${ORG_ID}m${MEMBER_ID}\"" | head -1)"
[ -n "$NU" ] && ok "new-api 侧存在用户 $NU(平台经官方 API 真建,非 mock)" || bad "未在 new-api 找到该用户"

printf '\n==== 联调结果: \033[32m%d 通过\033[0m / \033[31m%d 失败\033[0m ====\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
