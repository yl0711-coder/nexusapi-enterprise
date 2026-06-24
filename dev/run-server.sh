#!/usr/bin/env bash
# 把平台 server 接到常驻 dev 栈(new-api + MySQL)上跑,供联调。
#   - 自动建平台库 nexus
#   - 自动从 dev new-api 取管理员 access_token 注入 server
#   - 容器跑(镜像方式,不碰本机 Go),端口 18080
# dev 专用固定密钥(非生产!生产经环境变量注入真密钥,绝不入库)。
#
# 用法(仓库根):bash dev/run-server.sh
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f dev/docker-compose.dev.yml"
NET="dev_default"                 # dev compose 默认网络
NEWAPI_HOST="http://localhost:13000"
ROOT_PASS="${ROOT_PASS:-RootPass123}"
IMAGE="nexusapi-enterprise:dev"
# dev 固定主密钥(base64 of 32 字节)/会话密钥——稳定即可让重启后旧密文仍可解。仅 dev。
MASTER_KEY="$(printf '0123456789abcdef0123456789abcdef' | base64)"
SESSION_KEY="dev-only-session-signing-key-32bytes!!"

say() { printf '[run-server] %s\n' "$*"; }

# 0) 镜像:**默认每次重建**(容器内编译),保证联调跑的是当前源码。
#    踩过的坑(五总监真机逮到):旧逻辑"镜像存在就跳过 build",测新代码会复用旧镜像 → 改动根本没进去
#    (表现:mvp_mode=null、端点 422/403 而非预期行为)。Docker 层缓存使无源码改动时秒级、有改动时自动重编,
#    所以默认每次 build 是安全且廉价的。SKIP_BUILD=1 才显式复用旧镜像(确知在干嘛时)。
if [ "${SKIP_BUILD:-0}" = "1" ] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
  say "SKIP_BUILD=1:复用现有镜像 $IMAGE(未重建——确认这就是你要测的版本!)"
else
  say "构建镜像 $IMAGE(容器内编译,确保跑当前源码;无改动靠层缓存秒级)"
  docker build -t "$IMAGE" --build-arg VERSION=dev . >/dev/null
fi

# 1) 建平台库 nexus(幂等)。
say "确保平台库 nexus 存在"
$COMPOSE exec -T mysql mysql -uroot -pdevroot \
  -e "CREATE DATABASE IF NOT EXISTS nexus CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

# 2) 取 dev new-api 管理员 access_token(先等 new-api 就绪,T8:reset 后立即 run-server 偶发 connection reset)。
say "等待 new-api 就绪($NEWAPI_HOST/api/status)"
for i in $(seq 1 30); do
  curl -fsS "$NEWAPI_HOST/api/status" >/dev/null 2>&1 && break
  [ "$i" = 30 ] && { echo "new-api 30s 内未就绪,先 docker compose -f dev/docker-compose.dev.yml up -d" >&2; exit 1; }
  sleep 1
done
JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT
curl -fsS -c "$JAR" -X POST "$NEWAPI_HOST/api/user/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"root\",\"password\":\"$ROOT_PASS\"}" >/dev/null
ADMIN_TOKEN="$(curl -fsS -b "$JAR" -H "New-Api-User: 1" "$NEWAPI_HOST/api/user/token" \
  | grep -oE '"data":"[^"]+"' | head -1 | sed -E 's/"data":"([^"]+)"/\1/')"
[ -n "$ADMIN_TOKEN" ] || { echo "未取到 new-api 管理员 token,先 bash dev/seed.sh" >&2; exit 1; }
say "已取 new-api 管理员 token"

# 3) 跑 server(接 dev 网络,容器内用服务名连 mysql/newapi)。
docker rm -f nexus-ent-dev >/dev/null 2>&1 || true
docker run -d --name nexus-ent-dev --network "$NET" -p 18080:8080 \
  --memory=350m --cpus=1 --restart no \
  `# 本机 dev 不设自动重启(省资源,挂了不 crash loop、不开机自启);用时手动 run-server.sh 起` \
  -e LISTEN_ADDR=":8080" \
  -e NEXUS_WORKER_INTERVAL_SEC="${WORKER_INTERVAL:-60}" \
  `# 可选:WORKER_INTERVAL=5 bash dev/run-server.sh 缩短结算/配额周期,方便联调真账验;默认 60` \
  -e NEXUS_MVP_MODE="${MVP_MODE:-false}" \
  `# 可选:MVP_MODE=true bash dev/run-server.sh 复现灰度MVP封锁(观测模式+路由白名单);默认 false 全功能` \
  -e NEXUS_DB_DSN="root:devroot@tcp(mysql:3306)/nexus?parseTime=true&loc=UTC&charset=utf8mb4" \
  -e NEXUS_MASTER_KEY="$MASTER_KEY" \
  -e NEXUS_SESSION_KEY="$SESSION_KEY" \
  -e NEXUS_ALLOW_DEV_KEYS="true" \
  `# dev 专用:放行上面的 dev 默认密钥(生产绝不设此开关,启动会拒 dev 默认值)` \
  -e NEWAPI_BASE_URL="http://newapi:3000" \
  -e NEWAPI_ADMIN_TOKEN="$ADMIN_TOKEN" \
  -e NEWAPI_ADMIN_USER_ID="1" \
  -e NEXUS_BOOTSTRAP_OPERATOR_EMAIL="ops@nexus.local" \
  -e NEXUS_BOOTSTRAP_OPERATOR_PASSWORD="OpsPass123" \
  "$IMAGE" >/dev/null
say "server 启动中(容器 nexus-ent-dev,端口 18080)"

for i in $(seq 1 30); do
  if curl -fsS http://localhost:18080/readyz >/dev/null 2>&1; then
    say "就绪:$(curl -s http://localhost:18080/readyz)"
    say "联调入口 http://localhost:18080 ;运营方账号 ops@nexus.local / OpsPass123"
    exit 0
  fi
  sleep 1
done
say "server 未就绪,看日志:docker logs nexus-ent-dev"
exit 1
