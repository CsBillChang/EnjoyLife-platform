# 简历证据（仅本项目）

> **2026-09-23解释规则：** 执行和结束条件以 [09当前约定](09-final-closeout.md) 为准；下文按发生时间理解，不能重做已完成能力或以报告式新稿降低技术目标。开发已结项，不再主动启动完善或新的演示验收任务；用户自行运行与面试，只有实际故障或新需求才重开。


日期：2026-09-22。本文件只对照共享简历中的本项目原句，不修改共享简历、不编译 LaTeX。实现、自动化回归与本机测量是不同层次的证据。

## 简历原句对齐矩阵

| 共享简历原句标题／关键断言 | 实现位置 | 测试／实测证据 | 对齐与限制 |
| --- | --- | --- | --- |
| 项目描述：店铺查询、优惠券秒杀、下单支付和到店核销 | `internal/order/`、`internal/payment/`、`migrations/002_transaction_core.sql` | 订单/履约集成测试；支付/关单、退款/核销竞争回归 | 已对齐；支付仅本地模拟器，不是实际渠道 |
| 多级缓存与热点回源治理：店铺详情与优惠券展示使用 Ristretto + Redis | `internal/shop/cache.go`、`internal/coupon/cache.go` | `internal/shop/*test.go`、`internal/coupon/*test.go` | 主体对齐：活动列表/详情、详情负缓存、singleflight、单店 generation 已实现。随机过期和逻辑过期是既有店铺路径能力；新 coupon 路径是固定 30s/10s TTL，不宣称同样能力 |
| 缓存与商品数据一致性：MySQL 更新后失效本地/Redis，交易事实不进缓存 | `internal/coupon/repository.go`、`internal/coupon/invalidation.go` | 更新、失效失败重试、lease、generation 和 poison 隔离测试 | 已对齐：业务更新与 recovery task 同事务；本进程 dispatcher 失效 L1/L2。没有 Pub/Sub 或跨实例 L1 强一致，coupon TTL 30 秒/负缓存 10 秒且无抖动 |
| 秒杀资格与库存防超卖：Lua 原子校验时段、剩余资格和限购，MySQL 最终成交 | `internal/admission/`、`internal/order/admission_coordinator.go` | 开售/结束、重复、延迟处理、损坏 sentinel/type、真实 Redis 集成测试 | 已对齐：schema 2 的 `start <= accepted_at < end`、Redis `TIME < processing_deadline`、6 key/sentinel fail-closed；Redis 预占不是成单 |
| 异步下单与消息可靠性：Outbox + RabbitMQ 可恢复异步成单 | `internal/platform/mq/`、`internal/order/async_service.go`、`cmd/worker/` | 重投、broker 中断、提交后 Ack 前中断、真实依赖回归 | 已对齐；消息可重复，MySQL 请求/订单唯一约束和幂等事务收敛结果 |
| 支付关单与消费券履约：保留支付和核销记录供查询 | `internal/order/record_query.go`、`internal/order/record_http.go` | 分页、本人/商户归属、越权、游标和不存在记录单元及 MySQL 集成测试 | 已对齐：`GET /me/payment-records` 与 `GET /merchant/redemption-records?shop_id=` 用稳定 keyset 分页；不暴露支付渠道敏感字段或券码明文 |
| 热点限流与并发性能优化：固定负载、pprof 锁竞争与内存开销 | `internal/shop/cache.go`、`cmd/shop-loadgen/`、`internal/shop/handler.go` | [benchmark-report.md](benchmark-report.md) 和 [profiling-report.md](profiling-report.md) | 仅对绑定源码的店铺详情路径成立：P95 33.427→5.179 ms，吞吐 8,456.83→34,107.53 req/s；无显著业务锁热点，typed response 25→20 allocs/op。不外推 coupon 性能，不写锁/P95/吞吐优化收益 |

## 可写陈述（投递时选 2–3 条）

1. 使用 Go、Gin、GORM 与 MySQL 实现店铺查询、优惠券购券、本地模拟支付、商家核销、关单及退款闭环；以条件扣减、`(user_id, activity_id)` 唯一约束、库存流水和短事务状态机处理并发。
2. 将购买请求、Outbox、RabbitMQ Publisher Confirm、手动 Ack、租约围栏和幂等消费者组合为可恢复异步成单链路；Redis Lua schema 2 原子校验受理窗口、处理截止、资格与限购，MySQL 最终决定成交。
3. 在固定 60 秒、100 并发、10 秒预热、10,000 店铺、80% 热点的本机完整响应测试中，店铺详情两级缓存相对 MySQL-only 三轮中位 P95 为 33.427→5.179 ms，吞吐为 8,456.83→34,107.53 req/s；此数字不外推到本轮新增活动缓存。

异步实验的 `accepted_at → orders.created_at` P50/P95/P99 为 3.258310/4.096050/4.442061 s；它是订单行创建时间差，**不是事务 commit 延迟**，HTTP 202 更不是成单时间。

## 本轮验收与保留限制

- 本轮离线 `go test ./...`、`go test -race ./...`、`go vet ./...` 通过；`LOCAL_LIFE_INTEGRATION=1 go test -race -count=1 ./internal/...`、迁移、API/Worker 启动通过。环境、命令、定向场景和源码 SHA-256 见 `artifacts/verification/20260922-resume-alignment/`。
- 保留旧 generation 的 recovery task 会因 schema/sentinel 不完整而报告 incomplete 并 defer；必须按活动 fenced rebuild。重建会失败旧 generation 中无订单的 `RECEIVED/ADMITTED`，但保留所有历史成单用户限购。
- 活动缓存是展示层：陈旧价格/库存提示不能决定交易；交易条件禁止通过 merchant PATCH 修改。
- 不写生产 QPS、线上用户量、零故障保证、真实支付接入、全局缓存强一致、锁优化收益或内存泄漏修复。工程证据不等于用户已能独立复述、修改并通过面试。
