# 本地常驻联调环境(dev/)

给**联调**用的常驻 new-api + MySQL,跟 `test/` 下的一次性自动化测试栈是两码事:

- `test/docker-compose.*.yml`:自动化测试用,**一次性**(tmpfs,跑完 `down -v` 清掉),CI 也用它。
- `dev/`(本目录):**常驻**,数据落命名卷,可 `stop`/`start` 不丢;用于人工联调、手动戳接口、对着真 new-api 调平台。

> 全是 **MOCK 数据**:渠道 key 都是占位假串(`sk-mock-*`),**无任何真实密钥 / 客户数据**。
> 不从线上主站搬数据(线上库含上游渠道密钥 + 客户 key + PII,不入开发机,红线)。

## 一次性初始化(新机器 / 第一次)

```bash
# 1) 起栈(空库)。new-api 13000,MySQL 13306。
docker compose -f dev/docker-compose.dev.yml up -d

# 2) 灌 mock 配置(走 new-api 官方 API,零侵入):
#    - QuotaPerUnit=500000(1元=1美元=500000quota,09 §16)
#    - GroupRatio 顺带建出 default/vip/enterprise 三个分组(平台给成员绑分组用)
#    - ModelRatio 用 rc.4 自带的几百模型默认(claude/gpt/deepseek/glm/gemini/qwen…,比手写更贴线上)
#    - 2 个 mock 渠道(占位假 key)
bash dev/seed.sh

# 3) 快照成黄金基准库(committed,新机器可直接 reset 还原,免再 seed):
bash dev/snapshot.sh   # → dev/seed/base.sql
```

如果仓库里已带 `dev/seed/base.sql`,新机器可跳过 seed,直接 `up -d` 后 `bash dev/reset.sh` 一步到位。

## 日常联调

```bash
docker compose -f dev/docker-compose.dev.yml start   # 用时起来(数据还在)
# ... 联调 ...
docker compose -f dev/docker-compose.dev.yml stop    # 不用时停掉(数据还在,不占资源)
```

## 污染后一键还原

联调会建用户 / 改额度 / 造数据,弄脏了从基准库还原(**站点容器保留**,只重启 new-api 重连干净库):

```bash
bash dev/reset.sh
```

## 端口 / 账号

- new-api:http://localhost:13000 ,root / `RootPass123`(本地 mock,非线上)
- MySQL:`127.0.0.1:13306` ,root / `devroot`(本地 mock);库:`newapi`(new-api 用)
- 平台 server 联调要连 MySQL 时,先建平台库:
  `docker compose -f dev/docker-compose.dev.yml exec mysql mysql -uroot -pdevroot -e "CREATE DATABASE IF NOT EXISTS nexus CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"`
  然后平台 DSN 指 `root:devroot@tcp(127.0.0.1:13306)/nexus?parseTime=true&loc=UTC`。

## 联调平台 server(把后端接到这套 dev 栈上)

```bash
bash dev/run-server.sh   # 建平台库 nexus + 自动取 new-api 管理员 token + 跑 server(容器,端口 18080)
bash dev/smoke.sh        # 里程碑1 接口走查:登录→建组织/团队/层级→开通成员(真机代发key)→列表脱敏→轮换→RBAC(403/404/401)
```

- 联调入口:http://localhost:18080 ;运营方账号 `ops@nexus.local` / `OpsPass123`(dev 引导账号)。
- server 容器名 `nexus-ent-dev`,停:`docker rm -f nexus-ent-dev`;看日志:`docker logs nexus-ent-dev`。
- dev 主密钥/会话密钥是脚本里写死的固定值,**仅 dev**(稳定才能让重启后旧密文仍可解);生产经环境变量注入真密钥,绝不入库。
- 手动戳接口示例:
  `TOK=$(curl -s -XPOST localhost:18080/api/v1/auth/login -d '{"email":"ops@nexus.local","password":"OpsPass123"}' | grep -oE '"token":"[^"]+"' | cut -d'"' -f4)`
  然后 `curl -s localhost:18080/api/v1/organizations -H "Authorization: Bearer $TOK"`。

## 改 mock 数据

直接改 `dev/seed.sh`(加模型倍率 / 改分组 / 加渠道)→ `bash dev/reset.sh` 回空基线前先 `down -v` 重来,或在现有库上重跑 `seed.sh`(option 覆盖、渠道按名去重)→ 满意后 `bash dev/snapshot.sh` 重新快照。
