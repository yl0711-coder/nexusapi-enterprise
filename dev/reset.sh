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
