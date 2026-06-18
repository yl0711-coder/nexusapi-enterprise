#!/usr/bin/env bash
# 复位平台库 nexus 到基线:DROP + CREATE 空库,让平台启动时重新迁移建表 + bootstrap 运营方。
# 清掉多轮联调累积的测试组织/成员/折扣镜像等(T-ENV)。不动 new-api 侧(那个用 dev/reset.sh)。
#
# 用法(仓库根):
#   bash dev/reset-platform.sh        # 只复位平台库 nexus,然后需重跑 run-server.sh
#   bash dev/reset-platform.sh --all  # 平台库 + new-api 侧一起复位(= reset.sh + 本脚本)
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f dev/docker-compose.dev.yml"

if [ "${1:-}" = "--all" ]; then
  echo "[reset-platform] 先复位 new-api 侧"
  bash dev/reset.sh
fi

echo "[reset-platform] 停平台 server 容器(若在跑)"
docker rm -f nexus-ent-dev >/dev/null 2>&1 || true

echo "[reset-platform] DROP + CREATE 平台库 nexus(清空全部测试数据)"
$COMPOSE exec -T mysql mysql -uroot -pdevroot \
  -e "DROP DATABASE IF EXISTS nexus; CREATE DATABASE nexus CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

echo "[reset-platform] 完成。平台库已清空,重跑 bash dev/run-server.sh 会重新迁移建表 + bootstrap ops@nexus.local"
