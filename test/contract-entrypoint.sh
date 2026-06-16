#!/bin/sh
# 容器内:等 new-api(rc.4)就绪后,跑真机契约测试(自动 setup root)。
set -e

BASE="${NEWAPI_CONTRACT_BASE_URL:-http://newapi:3000}"
echo "等待 new-api 就绪: $BASE ..."
ok=0
i=0
while [ "$i" -lt 60 ]; do
  if wget -qO- "$BASE/api/status" >/dev/null 2>&1; then
    ok=1
    break
  fi
  i=$((i + 1))
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  echo "new-api 未在预期时间内就绪" >&2
  exit 1
fi
echo "new-api 就绪,开始真机契约测试。"

exec go test ./adapter/newapi/ -run Contract -v
