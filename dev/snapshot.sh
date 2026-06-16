#!/usr/bin/env bash
# 把当前常驻 new-api 的库快照成"黄金基准库" dev/seed/base.sql。
# base.sql 只含 MOCK 数据(占位假 key),可安全入库。之后 reset.sh 从它一键还原。
#
# 用法(在仓库根目录):bash dev/snapshot.sh
set -euo pipefail
cd "$(dirname "$0")/.."   # 切到仓库根

COMPOSE="docker compose -f dev/docker-compose.dev.yml"
mkdir -p dev/seed
echo "[snapshot] 导出 newapi 库 → dev/seed/base.sql"
$COMPOSE exec -T mysql sh -c 'exec mysqldump -uroot -pdevroot --single-transaction --no-tablespaces --databases newapi' \
  > dev/seed/base.sql
echo "[snapshot] 完成,大小: $(wc -c < dev/seed/base.sql) 字节"
echo "[snapshot] 提醒:确认 base.sql 内无真实密钥再入库(应只有 sk-mock-* 占位串)。"
