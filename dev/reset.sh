#!/usr/bin/env bash
# 从黄金基准库 base.sql 一键还原常驻 new-api 的数据(联调污染后恢复干净)。
# 站点容器保留(只重启 new-api 让它重连新库),不删容器、不动命名卷里的其他库。
#
# 用法(在仓库根目录):bash dev/reset.sh
set -euo pipefail
cd "$(dirname "$0")/.."   # 切到仓库根

COMPOSE="docker compose -f dev/docker-compose.dev.yml"
BASE_SQL="dev/seed/base.sql"
[ -f "$BASE_SQL" ] || { echo "缺少 $BASE_SQL,先 bash dev/seed.sh && bash dev/snapshot.sh" >&2; exit 1; }

echo "[reset] 停 new-api(容器保留)"
$COMPOSE stop newapi >/dev/null

echo "[reset] 重建 newapi 库"
$COMPOSE exec -T mysql mysql -uroot -pdevroot \
  -e "DROP DATABASE IF EXISTS newapi; CREATE DATABASE newapi CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

echo "[reset] 载入基准库 base.sql"
$COMPOSE exec -T mysql mysql -uroot -pdevroot < "$BASE_SQL"

echo "[reset] 重启 new-api"
$COMPOSE start newapi >/dev/null
echo "[reset] 完成,已还原到基准库状态。"
echo ""
echo "[reset] ⚠ 本脚本只重置 new-api 库。**平台库(nexus)未动**——若之前建过组织/成员,平台库仍有旧 member_key_token"
echo "        引用旧 new-api token id;new-api 重置后 token id 从头重算 → 撞 uk_key_token_newapi → 开成员 500。"
echo "        联调完整重置请**接着跑**:  bash dev/reset-platform.sh"
