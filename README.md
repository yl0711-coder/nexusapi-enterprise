# NexusAPI 企业管理平台（私有）

在现有 LLM 网关 new-api(rc.4)之上的一层**独立企业管理平台**(Go 后端 + SPA 前端),对 new-api **零数据侵入**(只走官方 HTTP 管理 API + 只读官方日志,不改其源码、不直连其 MySQL/Redis)。

> 商业核心,**私有仓库,绝不开源**。镜像走私有 GHCR,生产只 pull + up。

## 施工图(唯一来源)

代码实现以文档为准,本仓库不重复施工图内容:

- 总入口:`文档/NexusAPI/35-企业管理平台/02-完整方案-内部/11-研发开工导读.md`
- 功能 AC + 端点级 RBAC 矩阵:`08-功能规格与验收标准.md`
- 建库 DDL + 计量口径:`09-数据字典与计量口径.md`
- 平台 REST 契约 + **代发 key adapter 规格** + 密钥加密 + 工程规约:`10-平台API与工程规约.md`
- new-api 依赖清单 + rc.4 实证:`05-newapi依赖清单与升级兼容预案.md`
- 决策唯一来源:`01-过程与调研/决策记录与评审纪要.md`

## 四条铁律

1. 对 new-api **零数据侵入**(只官方 HTTP API + 只读日志)。
2. 平台不可用**绝不能影响** new-api 网关(旁路控制面,出事 `docker stop` 摘平台保网关)。
3. **不在生产机 build 镜像**(本机/CI 编好 → push 私有 GHCR → 生产只 pull + up)。
4. 涉钱(余额/quota/授信/折扣/退款)**先讲风险与回滚再动手**,不默认 fail-closed,全程留痕。

## 目录结构（对齐 `10 §4.3`）

```
handler/   HTTP 入口:解析/校验、组信封、定 HTTP 状态码。不含业务逻辑。
service/   业务逻辑 + RBAC + 事务编排 + 幂等。唯一可编排 repo/adapter 的层。
repo/      平台自有库读写(MySQL),乐观锁。不碰 new-api。
adapter/   外部系统封装。adapter/newapi = 对 new-api 官方 API 的唯一收口(10 §2)。
worker/    leader 单写者:定时重置 / grant 到期 / 重加密滚动 / 异步补偿。
pkg/       crypto / log / idemp / lock 等横切。
```

依赖方向单向:`handler → service → {repo, adapter}`。禁止 handler 直调 adapter/repo。

## 当前进度

- **里程碑 0 · 打通代发 key adapter**(进行中):`adapter/newapi` 全链路 + 失败矩阵 + 补偿 + 并发互斥 + 自愈 + 契约测试。这是全项目最大不确定性,先消除。

## 测试

```bash
# 单元/契约(mock-rc.4):无需任何外部依赖,随时跑绿
go test ./...

# 真机契约(对照真实 rc.4,验证 adapter 与上游契约一致):
#   1) docker run -d -p 13000:3000 calciumion/new-api:v1.0.0-rc.4   # 见 05 §5.6
#   2) 首跑 POST /api/setup 建 root,拿 admin access_token + user_id
#   3) 带环境变量跑:
NEWAPI_CONTRACT_BASE_URL=http://localhost:13000 \
NEWAPI_CONTRACT_ADMIN_TOKEN=<admin_access_token> \
NEWAPI_CONTRACT_ADMIN_USER_ID=1 \
  go test ./adapter/newapi/ -run Contract -v
# 未设环境变量时该用例自动 skip(无 Docker 也能全绿)。
```
