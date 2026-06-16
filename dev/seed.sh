#!/usr/bin/env bash
# 给常驻 new-api 灌一套贴近线上结构的 MOCK 配置(零侵入:全走 new-api 官方管理 API)。
#
# 灌的内容:计价锚定 QuotaPerUnit + 模型倍率 ModelRatio + 分组倍率 GroupRatio
# (default/vip/enterprise)+ 2 个 MOCK 渠道(占位假 key,绝无真实密钥)+ 一组代表性模型。
# 幂等:重复跑会覆盖 option;渠道按名去重不重复建。
#
# 用法:bash dev/seed.sh   (默认打 http://localhost:13000)
set -euo pipefail

BASE="${NEWAPI_URL:-http://localhost:13000}"
ROOT_PASS="${ROOT_PASS:-RootPass123}"
COOKIE_JAR="$(mktemp)"
trap 'rm -f "$COOKIE_JAR"' EXIT

say() { printf '[seed] %s\n' "$*"; }

# 等 new-api 起来。
say "等待 new-api 就绪: $BASE"
for i in $(seq 1 60); do
  if curl -fsS "$BASE/api/status" >/dev/null 2>&1; then break; fi
  sleep 2
  [ "$i" = 60 ] && { echo "new-api 未就绪" >&2; exit 1; }
done

# 1) 初始化 root(已初始化则忽略报错)。
curl -fsS -X POST "$BASE/api/setup" -H 'Content-Type: application/json' \
  -d "{\"username\":\"root\",\"password\":\"$ROOT_PASS\",\"confirmPassword\":\"$ROOT_PASS\"}" >/dev/null 2>&1 || true

# 2) root 登录拿 cookie + uid。
LOGIN_RESP="$(curl -fsS -c "$COOKIE_JAR" -X POST "$BASE/api/user/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"root\",\"password\":\"$ROOT_PASS\"}")"
UID_VAL="$(printf '%s' "$LOGIN_RESP" | grep -oE '"id":[0-9]+' | head -1 | grep -oE '[0-9]+' || true)"
UID_VAL="${UID_VAL:-1}"
say "root 登录 ok, uid=$UID_VAL"

# 3) 取管理员 access_token(cookie + New-Api-User 双头,rc.4 实证)。
TOK_RESP="$(curl -fsS -b "$COOKIE_JAR" -H "New-Api-User: $UID_VAL" "$BASE/api/user/token")"
ADMIN_TOKEN="$(printf '%s' "$TOK_RESP" | grep -oE '"data":"[^"]+"' | head -1 | sed -E 's/"data":"([^"]+)"/\1/')"
[ -n "$ADMIN_TOKEN" ] || { echo "未取到管理员 access_token: $TOK_RESP" >&2; exit 1; }
say "已取管理员 access_token"

# 管理员 API 调用封装(Bearer + New-Api-User)。
admin() { # method path json
  curl -fsS -X "$1" "$BASE$2" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "New-Api-User: $UID_VAL" \
    -H 'Content-Type: application/json' -d "$3"
}

# option 更新:rc.4 端点要带尾斜杠 /api/option/(实证);返回 200+success 信封。
set_option() { # key value(value 须是已转义的 JSON 串字面量)
  local resp; resp="$(admin PUT /api/option/ "{\"key\":\"$1\",\"value\":$2}" 2>/dev/null || true)"
  if printf '%s' "$resp" | grep -q '"success":true'; then
    say "option $1 已设置"
  else
    say "option $1 设置失败(忽略): $(printf '%s' "$resp" | head -c 80)"
  fi
}

# 4) 计价锚定 + 分组倍率(贴线上口径:1 元=1 美元=500000 quota,见 09 §16)。
#    分组倍率会顺带把 default/vip/enterprise 三个分组建出来(平台给成员绑分组用)。
#    ModelRatio 不覆盖:rc.4 自带几百个模型的完整默认倍率(claude/gpt/deepseek/glm/gemini/qwen…),
#    比手写更贴线上;要改个别模型再单独 set。
set_option QuotaPerUnit '"500000"'
set_option GroupRatio '"{\"default\":1,\"vip\":0.9,\"enterprise\":0.85}"'

# 5) MOCK 渠道(占位假 key,绝无真实密钥;只为结构存在,不用于真实调用)。
#    rc.4 渠道创建须外层包 {"mode":"single","channel":{...}}(否则 nil panic,实证)。
EXISTING="$(admin GET '/api/channel/?p=0&page_size=100' '' 2>/dev/null || echo '')"
create_channel() { # name type key base_url models
  if printf '%s' "$EXISTING" | grep -q "\"name\":\"$1\""; then
    say "渠道 $1 已存在,跳过"; return
  fi
  local ch body resp
  ch="{\"name\":\"$1\",\"type\":$2,\"key\":\"$3\",\"base_url\":\"$4\",\"models\":\"$5\",\"groups\":[\"default\",\"vip\",\"enterprise\"],\"group\":\"default,vip,enterprise\",\"model_mapping\":\"\",\"setting\":\"\",\"status_code_mapping\":\"\",\"auto_ban\":1,\"weight\":0,\"priority\":0,\"tag\":\"\"}"
  body="{\"mode\":\"single\",\"channel\":$ch}"
  resp="$(admin POST /api/channel/ "$body" 2>/dev/null || true)"
  if printf '%s' "$resp" | grep -q '"success":true'; then
    say "渠道 $1 已建"
  else
    say "渠道 $1 建失败(忽略): $(printf '%s' "$resp" | head -c 80)"
  fi
}
create_channel "Mock-OpenAI兼容" 1 "sk-mock-openai-DONOTUSE-0001" "https://mock-upstream.local" \
  "gpt-5-mini,gpt-4o-mini,deepseek-chat,glm-4-plus,qwen-plus"
create_channel "Mock-Anthropic" 14 "sk-mock-anthropic-DONOTUSE-0001" "" \
  "claude-opus-4-6,claude-sonnet-4-5-20250929,claude-haiku-4-5-20251001"

# 6) 校验:分组 + 渠道数,确认 mock 已生效。
say "---- 校验 ----"
say "分组: $(admin GET /api/group/ '' 2>/dev/null | grep -oE '"data":\[[^]]*\]' | head -c 120)"
CH="$(admin GET '/api/channel/?p=0&page_size=100' '' 2>/dev/null || echo '')"
say "渠道 total: $(printf '%s' "$CH" | grep -oE '"total":[0-9]+' | head -1)"
say "QuotaPerUnit: $(admin GET /api/option/ '' 2>/dev/null | grep -oE '"key":"QuotaPerUnit","value":"[0-9]+"' | head -1)"
say "完成。这是 MOCK 数据,渠道 key 均为占位假串(sk-mock-*)。"
