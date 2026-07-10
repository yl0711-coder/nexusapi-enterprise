#!/usr/bin/env bash
# 架构B 演示/测试数据造数(dev 栈专用)。
#
# 造出"真一些、丰富一些"的可点测数据,全程走**平台真实 API**(建组织/开成员/划账/建令牌
# 都是产品代码路径,造数即联调),消费按 new-api **原生双扣**模拟(扣成员额度 + 记 logs)。
#
# 全 MOCK:渠道假 key、客户/成员都是编的,无任何真实密钥/PII(红线:不搬线上数据)。
#
# 前置:dev 栈已起 + 平台 server 在跑(先 bash dev/reset-platform.sh && bash dev/run-server.sh)。
# 用法(仓库根):bash dev/seed-archb-demo.sh
#
# 造出:
#   组织1 星尘科技(公司客户)  金库$500  固定档+订阅档  研发组/运营组  4成员(含离职退额)
#   组织2 云帆代理(代理商)     金库$1000 入门/专业两档  4下游子账户(含停用+追加划账)
#   组织3 初创工作室(小客户)   金库$30   基础档          2成员(触发金库低预警)
set -uo pipefail
cd "$(dirname "$0")/.."

PLAT="http://localhost:18080/api/v1"
NEWAPI="http://localhost:13000"
MYSQL=(docker compose -f dev/docker-compose.dev.yml exec -T mysql mysql -uroot -pdevroot)
Q=500000  # QuotaPerUnit:1 美元 = 500000 raw
usd(){ echo $(( $1 * Q )); }  # 美元 -> raw

say(){ printf '\n\033[1;36m[seed] %s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m[seed][warn] %s\033[0m\n' "$*"; }

# JSON 取值:getj 'data.token' < resp
getj(){ python3 -c '
import sys,json
try: d=json.load(sys.stdin)
except Exception: print(""); sys.exit(0)
for k in sys.argv[1].split("."):
    if d is None: print(""); sys.exit(0)
    d = d[int(k)] if isinstance(d,list) else d.get(k)
print("" if d is None else d)' "$1"; }

# ---- 平台 API ----
TOKEN=""
plogin(){ curl -s -XPOST "$PLAT/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"$2\"}" | getj data.token; }
platp(){ curl -s -XPOST "$PLAT$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$2"; }
platget(){ curl -s "$PLAT$1" -H "Authorization: Bearer $TOKEN"; }

# ---- new-api 管理(root 会话 cookie,绝不调 GET /api/user/token 以免旋转 server 的 admin token)----
NJAR="$(mktemp)"; trap 'rm -f "$NJAR"' EXIT
curl -s -c "$NJAR" -XPOST "$NEWAPI/api/user/login" -H 'Content-Type: application/json' \
  -d '{"username":"root","password":"RootPass123"}' >/dev/null
na_admin(){ curl -s -b "$NJAR" -H 'New-Api-User: 1' -H 'Content-Type: application/json' "$@"; }
# 金库充值 / 消费扣额:走 /api/user/manage(cache 安全,与平台划账同一上游路径)
na_quota(){ na_admin -XPOST "$NEWAPI/api/user/manage" -d "{\"id\":$1,\"action\":\"add_quota\",\"mode\":\"$2\",\"value\":$3}" >/dev/null; }
fund(){ na_quota "$1" add "$2"; }

# 金库是惰性开通的(首次开成员才建;member.go:92)。造数要先充值再开成员,故用一个引子成员
# 触发建金库(它因金库=0 首笔划账失败被隔离),随后充值,再删掉引子。orgId tierId 金库美元 -> echo 金库uid
ensure_treasury_funded(){
  platp "/organizations/$1/members" "{\"name\":\"__prime__\",\"email\":\"__prime_$1@seed.local\",\"tier_id\":$2,\"team_id\":null}" >/dev/null 2>&1
  local uid; uid="$(treasury_uid "$1")"
  [ -n "$uid" ] || { warn "org $1 金库 provision 失败"; echo ""; return; }
  fund "$uid" "$(usd $3)"
  # 删引子(隔离的孤儿成员 + 其占位行;FK 子行先清)
  local pmid; pmid="$("${MYSQL[@]}" -N nexus -e "SELECT id FROM member WHERE org_id=$1 AND login_email LIKE '__prime%'" 2>/dev/null | tr -d '[:space:]')"
  [ -n "$pmid" ] && "${MYSQL[@]}" nexus -e "DELETE FROM member_key_token WHERE member_id=$pmid; DELETE FROM member_key_slot WHERE member_id=$pmid; DELETE FROM member_grant WHERE member_id=$pmid; DELETE FROM member WHERE id=$pmid;" 2>/dev/null
  echo "$uid"
}

# 平台库读金库 new-api user id(org 视图不暴露金库,隔离;造数从库读;轮询等金库回填)
treasury_uid(){
  local v; for _ in $(seq 1 20); do
    v="$("${MYSQL[@]}" -N nexus -e "SELECT newapi_user_id FROM organization WHERE id=$1" 2>/dev/null | tr -d '[:space:]')"
    [ -n "$v" ] && [ "$v" != "NULL" ] && { echo "$v"; return; }
    sleep 0.3
  done; echo ""
}

# ---- 前置:配 new-api 可用分组映射(default/vip/enterprise 互通,免建组/建令牌 422)----
say "配 new-api 可用模型分组映射(group_special_usable_group)"
# 三个模型/计价分组(成员令牌用)互通 + 三个组织专属金库分组(org 唯一绑定,金库不发令牌但建组织要校验其可用映射非空)
USABLE='{"default":{"default":"默认","vip":"VIP","enterprise":"企业"},"vip":{"default":"默认","vip":"VIP","enterprise":"企业"},"enterprise":{"default":"默认","vip":"VIP","enterprise":"企业"},"org-stardust":{"default":"默认"},"org-yunfan":{"default":"默认"},"org-startup":{"default":"默认"}}'
na_admin -XPUT "$NEWAPI/api/option/" \
  -d "{\"key\":\"group_ratio_setting.group_special_usable_group\",\"value\":$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$USABLE")}" >/dev/null

# ---- 运营方登录 ----
say "运营方登录"
TOKEN="$(plogin ops@nexus.local OpsPass123)"
[ -n "$TOKEN" ] || { echo "运营方登录失败,server 起了吗?"; exit 1; }
OPS_TOKEN="$TOKEN"

# ============ 通用造数函数 ============
# create_org 名 slug 管理员邮箱 管理员密码 分组 -> echo orgId(以运营方身份)
create_org(){
  TOKEN="$OPS_TOKEN"
  local r; r="$(platp /organizations "{\"name\":\"$1\",\"slug\":\"$2\",\"admin_email\":\"$3\",\"admin_password\":\"$4\",\"newapi_user_group\":\"$5\"}")"
  local id; id="$(echo "$r" | getj data.org.id)"
  [ -n "$id" ] || { warn "建组织 $1 失败: $r"; echo ""; return; }
  echo "$id"
}
# org_admin 登录 -> 设置全局 TOKEN
admin_login(){ TOKEN="$(plogin "$1" "$2")"; [ -n "$TOKEN" ] || warn "org_admin $1 登录失败"; }
# create_tier orgId 名 型(fixed/subscription) 额度美元 分组 周期(可空) 模型CSV -> echo tierId
create_tier(){
  local models; models="$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1].split(",")))' "$7")"
  local period="null"; [ -n "$6" ] && period="\"$6\""
  local body="{\"name\":\"$2\",\"quota_type\":\"$3\",\"amount_raw\":$(usd $4),\"visibility\":\"all\",\"newapi_group\":\"$5\",\"reset_period\":$period,\"model_set\":$models}"
  local r; r="$(platp "/organizations/$1/tiers" "$body")"
  local id; id="$(echo "$r" | getj data.id)"; [ -z "$id" ] && id="$(echo "$r" | getj id)"
  [ -n "$id" ] || warn "建档位 $2 失败: $r"
  echo "$id"
}
# create_team orgId 名 -> echo teamId
create_team(){ local r; r="$(platp "/organizations/$1/teams" "{\"name\":\"$2\"}")"; local id; id="$(echo "$r" | getj data.id)"; [ -z "$id" ] && id="$(echo "$r" | getj id)"; echo "$id"; }
# open_member orgId tierId teamId(可空) 名 邮箱 -> echo "memberId newapiUid pass"
open_member(){
  local team="null"; [ -n "$3" ] && team="$3"
  local r; r="$(platp "/organizations/$1/members" "{\"name\":\"$4\",\"email\":\"$5\",\"tier_id\":$2,\"team_id\":$team}")"
  local mid uid pass; mid="$(echo "$r" | getj data.member_id)"; uid="$(echo "$r" | getj data.newapi_user_id)"; pass="$(echo "$r" | getj data.initial_password)"
  [ -n "$mid" ] || { warn "开成员 $4 失败: $r"; echo ""; return; }
  echo "$mid $uid $pass"
}
# member_token 邮箱 密码 分组 令牌名 [额度美元] -> echo tokenId
member_token(){
  local mt; mt="$(plogin "$1" "$2")"
  [ -n "$mt" ] || { warn "成员 $1 登录失败"; echo ""; return; }
  local q="null"; [ -n "${5:-}" ] && q="$(usd $5)"
  local r; r="$(curl -s -XPOST "$PLAT/me/tokens" -H "Authorization: Bearer $mt" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$4\",\"group\":\"$3\",\"quota_raw\":$q}")"
  local id; id="$(echo "$r" | getj data.id)"; [ -z "$id" ] && id="$(echo "$r" | getj id)"
  [ -n "$id" ] || { warn "成员 $1 建令牌失败: $r"; echo ""; return; }
  echo "$id"
}
# 45号-21:插历史日志前把结算水位回拨到造数窗口起点(-8天),否则 forward 结算只扫水位后、
# seed 的历史消费永远结不进 usage_ledger → 演示数据"已用/消耗图"恒 0 不自洽(dev 演示专用,生产无此操作)。
"${MYSQL[@]}" nexus -e "UPDATE settlement_cursor SET last_settled_ts=$(( $(date +%s) - 8*86400 )), last_settled_log_id=0 WHERE org_id=0;" 2>/dev/null || true

# consume 成员newapiUid tokenId 令牌名 用户名 分组 模型 用量美元(小数x100->用cents) 几天前
# 扣成员额度(cache安全) + 记 new-api logs + 累加 used_quota(报表/用量口径一致)
consume(){
  local uid=$1 tid=$2 tname=$3 uname=$4 grp=$5 model=$6 cents=$7 daysago=$8
  local raw=$(( cents * Q / 100 ))
  local ts=$(( $(date +%s) - daysago*86400 - RANDOM%80000 ))
  local pt=$(( 200 + RANDOM%3000 )) ct=$(( 100 + RANDOM%1500 ))
  na_quota "$uid" subtract "$raw"
  "${MYSQL[@]}" newapi -e "
    INSERT INTO logs (user_id,created_at,type,content,username,token_name,model_name,quota,prompt_tokens,completion_tokens,use_time,is_stream,channel_id,channel_name,token_id,\`group\`,ip,request_id,other)
      VALUES ($uid,$ts,2,'','$uname','$tname','$model',$raw,$pt,$ct,$(( 1+RANDOM%8 )),$(( RANDOM%2 )),1,'mock-channel',$tid,'$grp','','',' ');
    UPDATE users SET used_quota=used_quota+$raw, request_count=request_count+1 WHERE id=$uid;
    UPDATE tokens SET remain_quota=GREATEST(remain_quota-$raw,0), used_quota=used_quota+$raw WHERE id=$tid;" 2>/dev/null
}
# 停用成员 / 离职成员 / 追加划账(都走平台真 API)
member_disable(){ platp "/members/$1/status" '{"enabled":false}' >/dev/null; }
member_offboard(){ platp "/members/$1/offboard" '{}' >/dev/null; }
member_grant(){ platp "/members/$1/quota:grant" "{\"amount_raw\":$(usd $2),\"reason\":\"topup\"}" >/dev/null; }

##############################################################################
say "组织1:星尘科技(公司客户)"
O1=$(create_org "星尘科技" "stardust" "admin@stardust.com" "Admin@1234" "org-stardust")
admin_login "admin@stardust.com" "Admin@1234"
T1A=$(create_tier "$O1" "标准员工" fixed 50 default "" "gpt-4o-mini,claude-haiku-4-5-20251001,deepseek-chat")
T1B=$(create_tier "$O1" "研发套餐" subscription 150 vip weekly "claude-sonnet-4-5-20250929,gpt-5-mini,glm-4-plus,deepseek-chat")
TEAM_RD=$(create_team "$O1" "研发组"); TEAM_OPS=$(create_team "$O1" "运营组")
T=$(ensure_treasury_funded "$O1" "$T1A" 500); say "  金库 user=$T 充值 \$500"

read -r M_ZW U_ZW P_ZW <<<"$(open_member "$O1" "$T1B" "$TEAM_RD" "张伟" "zhangwei@stardust.com")"
read -r M_LN U_LN P_LN <<<"$(open_member "$O1" "$T1A" "$TEAM_OPS" "李娜" "lina@stardust.com")"
read -r M_WF U_WF P_WF <<<"$(open_member "$O1" "$T1A" "$TEAM_OPS" "王芳" "wangfang@stardust.com")"
read -r M_CJ U_CJ P_CJ <<<"$(open_member "$O1" "$T1A" "" "陈杰" "chenjie@stardust.com")"

TK_ZW1=$(member_token "zhangwei@stardust.com" "$P_ZW" vip "Claude-Code" 80)
TK_ZW2=$(member_token "zhangwei@stardust.com" "$P_ZW" vip "本地调试" 40)
TK_LN=$(member_token "lina@stardust.com" "$P_LN" default "日常运营")
TK_WF=$(member_token "wangfang@stardust.com" "$P_WF" default "新号待用")

say "  模拟消费(张伟重度 / 李娜轻度 / 王芳空 / 陈杰离职退额)"
consume "$U_ZW" "$TK_ZW1" "Claude-Code" "张伟" vip "claude-sonnet-4-5-20250929" 1850 1
consume "$U_ZW" "$TK_ZW1" "Claude-Code" "张伟" vip "claude-sonnet-4-5-20250929" 2230 3
consume "$U_ZW" "$TK_ZW1" "Claude-Code" "张伟" vip "gpt-5-mini" 940 5
consume "$U_ZW" "$TK_ZW2" "本地调试" "张伟" vip "deepseek-chat" 120 2
consume "$U_ZW" "$TK_ZW2" "本地调试" "张伟" vip "glm-4-plus" 65 4
consume "$U_LN" "$TK_LN" "日常运营" "李娜" default "gpt-4o-mini" 210 1
consume "$U_LN" "$TK_LN" "日常运营" "李娜" default "deepseek-chat" 95 6
member_offboard "$M_CJ"; say "  陈杰已离职(停令牌 + 未用额度退回金库)"

##############################################################################
say "组织2:云帆代理(代理商,成员=下游子账户硬限)"
O2=$(create_org "云帆代理" "yunfan" "admin@yunfan.com" "Admin@1234" "org-yunfan")
admin_login "admin@yunfan.com" "Admin@1234"
T2A=$(create_tier "$O2" "入门套餐" fixed 20 default "" "gpt-4o-mini,deepseek-chat,claude-haiku-4-5-20251001")
T2B=$(create_tier "$O2" "专业套餐" fixed 100 enterprise "" "claude-sonnet-4-5-20250929,gpt-5-mini,glm-4-plus,deepseek-chat")
T=$(ensure_treasury_funded "$O2" "$T2A" 1000); say "  金库 user=$T 充值 \$1000"

read -r M_A U_A P_A <<<"$(open_member "$O2" "$T2A" "" "下游-甲(设计工作室)" "clientA@yunfan.com")"
read -r M_B U_B P_B <<<"$(open_member "$O2" "$T2B" "" "下游-乙(SaaS团队)" "clientB@yunfan.com")"
read -r M_C U_C P_C <<<"$(open_member "$O2" "$T2A" "" "下游-丙(个人开发者)" "clientC@yunfan.com")"
read -r M_D U_D P_D <<<"$(open_member "$O2" "$T2B" "" "下游-丁(电商客户)" "clientD@yunfan.com")"

TK_A=$(member_token "clientA@yunfan.com" "$P_A" default "生产key")
TK_B1=$(member_token "clientB@yunfan.com" "$P_B" enterprise "主站")
TK_B2=$(member_token "clientB@yunfan.com" "$P_B" enterprise "测试环境")
TK_D=$(member_token "clientD@yunfan.com" "$P_D" enterprise "商品描述生成")

say "  模拟消费(甲快用完触发额度告警 / 乙部分 / 丁追加划账)"
consume "$U_A" "$TK_A" "生产key" "下游甲" default "gpt-4o-mini" 880 1
consume "$U_A" "$TK_A" "生产key" "下游甲" default "deepseek-chat" 760 2
consume "$U_A" "$TK_A" "生产key" "下游甲" default "claude-haiku-4-5-20251001" 300 4   # 甲已用$19.4/20 逼近上限
consume "$U_B" "$TK_B1" "主站" "下游乙" enterprise "claude-sonnet-4-5-20250929" 2600 1
consume "$U_B" "$TK_B1" "主站" "下游乙" enterprise "gpt-5-mini" 1400 3
consume "$U_B" "$TK_B2" "测试环境" "下游乙" enterprise "glm-4-plus" 180 2
consume "$U_D" "$TK_D" "商品描述生成" "下游丁" enterprise "gpt-5-mini" 3100 2
member_grant "$M_D" 50; say "  下游丁额度追加划账 \$50(金库→成员)"
member_disable "$M_C"; say "  下游丙已停用(欠费风控演示)"

##############################################################################
say "组织3:初创工作室(小客户,金库低预警场景)"
O3=$(create_org "初创工作室" "startup" "admin@startup.com" "Admin@1234" "org-startup")
admin_login "admin@startup.com" "Admin@1234"
T3=$(create_tier "$O3" "基础" fixed 10 default "" "gpt-4o-mini,deepseek-chat")
T=$(ensure_treasury_funded "$O3" "$T3" 30); say "  金库 user=$T 充值 \$30(故意少,开2成员后见底)"
read -r M_XM U_XM P_XM <<<"$(open_member "$O3" "$T3" "" "小明" "xiaoming@startup.com")"
read -r M_XH U_XH P_XH <<<"$(open_member "$O3" "$T3" "" "小红" "xiaohong@startup.com")"
TK_XM=$(member_token "xiaoming@startup.com" "$P_XM" default "试用")
consume "$U_XM" "$TK_XM" "试用" "小明" default "deepseek-chat" 220 1

say "设金库低预警阈值 \$12(令组织3 金库\$10 触发预警)"
"${MYSQL[@]}" nexus -e "INSERT INTO platform_setting (k,v,updated_by) VALUES ('treasury_low_watermark_raw','$(usd 12)','seed') ON DUPLICATE KEY UPDATE v='$(usd 12)';" 2>/dev/null

##############################################################################
say "造数完成。守恒自检(每组织:金库+Σ成员在册额度+Σ已消费 应 = Σ注资+Σ划账入)"
TOKEN="$OPS_TOKEN"
for OID in "$O1" "$O2" "$O3"; do
  bal="$(platget "/organizations/$OID/balance")"
  echo "  org $OID balance: $(echo "$bal" | python3 -c 'import sys,json;d=json.load(sys.stdin).get("data",{});print({k:d.get(k) for k in ("treasury_raw","members_total_raw","total_raw") if k in d} or d)' 2>/dev/null)"
done
echo
say "登录信息(全 dev mock 密码):"
cat <<EOF
  运营方后台: http://localhost:18080   ops@nexus.local / OpsPass123
  组织管理员: admin@stardust.com / admin@yunfan.com / admin@startup.com   密码均 Admin@1234
  成员示例:   zhangwei@stardust.com / clientA@yunfan.com ...  初始密码见上面脚本输出(每次随机)
  查数据:     平台库 nexus / new-api 库 newapi @ 127.0.0.1:13306 (root/devroot)
EOF
