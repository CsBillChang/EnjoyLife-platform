# 2026-09-22 简历原文对齐验收

本目录记录本轮活动/优惠券展示、Lua 时间窗口、支付/核销记录查询的增量验收。历史性能与 pprof 原始证据保留在 `artifacts/measurements/20260922-*`；本轮没有重跑正式压测，也不声称 coupon 查询的性能收益。

## 环境

- macOS 27.0, arm64
- Go 1.26.4 darwin/arm64
- Docker client/server 29.7.2, Compose 5.5.1
- Compose project: `huixiang-life`
- MySQL `127.0.0.1:13306`, Redis `127.0.0.1:16379`, RabbitMQ AMQP `127.0.0.1:15672`
- 三个服务在验收时均为 healthy；没有停止、清理或重建其他项目容器。

## 命令与结果

```bash
GOCACHE=/private/tmp/huixiang-go-cache go test ./...
GOCACHE=/private/tmp/huixiang-go-cache go test -race ./...
GOCACHE=/private/tmp/huixiang-go-cache go vet ./...

LOCAL_LIFE_INTEGRATION=1 GOCACHE=/private/tmp/huixiang-go-cache \
  go test -race -count=1 ./internal/...
```

四条命令全部退出 0。最终源码的真实依赖回归通过包：`admission 1.753s`、`coupon 1.407s`、`order 3.679s`、`mq 1.542s`、`recovery 2.364s`，其余 `internal` 包也全部通过。

定向 `-v -race` 验收另覆盖：

- coupon 热列表更新后刷新、generation 乱序不回退/损坏值修复、Gin 路由组合、MySQL 更新+失效任务同事务、失败重试、坏 payload 隔离与租约围栏；均 PASS。最终 coupon 真实 MySQL+Redis race 包回归为 1.430s。
- Lua 的 before-start/at-start/before-end/at-end/expired/after-natural-end/大 generation，以及 11 种 metadata/sentinel/键类型损坏；均 PASS。
- 重建在 FROZEN、NORMALIZED、INSTALLED、VERIFIED、REDIS_OPEN、DONE 六个中断点续作；均 PASS，快照为 owners=3、stock=2。
- 准入并发重放/预占后崩溃/售罄/关闭/generation/超时/Redis 短故障/DB 短故障/提交响应丢失；9 个场景均 PASS。
- RabbitMQ 成单提交后 Ack 前重投：`redelivered=true`、`orders=1`、`DEDUCT=1`、`RELEASE=0`。
- 支付/关单 6 轮只有一方胜出（本次实测 payment=2, close=4）；退款/核销 6 轮只有一方胜出（本次 refund=6, redemption=0）。胜者分布不是稳定性目标，不用作性能数据。
- 用户支付记录与商户核销记录的所有权/店铺成员关系集成用例 PASS。

```bash
GOCACHE=/private/tmp/huixiang-go-cache go run ./cmd/migrate
GOCACHE=/private/tmp/huixiang-go-cache HTTP_ACCESS_LOG=0 go run ./cmd/api
curl --fail --silent --show-error http://127.0.0.1:18080/health
curl --fail --silent --show-error 'http://127.0.0.1:18080/shops/1/activities?page=1&page_size=2'
curl --fail --silent --show-error http://127.0.0.1:18080/activities/1
curl --fail --silent --show-error http://127.0.0.1:18080/debug/cache-metrics
GOCACHE=/private/tmp/huixiang-go-cache go run ./cmd/worker
```

迁移输出 `migrations applied`；API 路由完整注册，健康检查返回 `{"status":"ok"}`，活动列表/详情返回 200 与 `available_stock_hint` 提示，缓存指标可见；API 与 Worker 均可独立启动并用 SIGINT 停止。

Worker 启动时对保留数据中的旧 Redis generation 任务报 `admission generation state is incomplete` 并 deferred。这是 fail-closed 的升级运维项，需按活动执行 fenced rebuild；本轮未清空数据、未伪造 sentinel。

## 口径

- `available_stock_hint` 只用于展示；实际成单仍由 MySQL 事务重读活动条件、条件扣库存与唯一约束决定。
- coupon 缓存为固定 30s TTL、空值 10s，无 TTL 抖动或 Pub/Sub；多实例 L1 不做广播，受 TTL 约束。Ristretto `MaxCost` 中本实现每条 cost=1，它是条目权重预算，不是精确字节上限。
- 历史异步指标是 `accepted_at → orders.created_at`（订单行创建时间）和首次观察终态上界，都不是精确事务提交完成时间。
- 此工作区没有可用的已跟踪 Git 基线；用 `source-manifest.sha256` 绑定本轮验收源码，不伪造 commit id。
