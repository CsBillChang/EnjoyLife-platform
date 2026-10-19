# 交付清单与简历证据映射

> **2026-09-23解释规则：** 执行和结束条件以 [09当前约定](09-final-closeout.md) 为准；下文按发生时间理解，不能重做已完成能力或以报告式新稿降低技术目标。开发已结项，不再主动启动完善或新的演示验收任务；用户自行运行与面试，只有实际故障或新需求才重开。

**最新验收基准：** 业务闭环与 pprof 补充分析均已完成本机交付验收。旧 40 ms CPU 样本保留但排除；有效采样、锁竞争结论、分配优化及关闭诊断后的同条件对照见 [pprof 报告](profiling-report.md)。


更新日期：2026-09-22。本文将目标、实现、验证和原始证据绑定起来；量化结论见 [benchmark-report.md](benchmark-report.md)。

## 当前事实

- L00–L14 工程范围及简历对齐补充已实现：Go/Gin/GORM/MySQL、认证权限、店铺和活动展示两级缓存与持久失效、同步交易、模拟支付/退款/核销、Outbox+RabbitMQ、Lua 准入、限流与代际重建、支付/核销记录查询。
- 最终审查后补齐 Worker 自动关单、本地模拟退款恢复、MySQL RELEASE 到 Redis 库存提示的幂等回补，以及本人授权的券码重新签发。
- 全仓 test/race/vet 和带 race 的 MySQL/Redis/RabbitMQ 真实依赖回归已通过；无效压测轮次也被保留并在报告中排除。
- MySQL 是交易事实来源；Redis 准入和 RabbitMQ 消息均不等于订单已创建。
- 保留数据中的旧 Redis 代际缺少新去重 sentinel，对应回补任务会 fail-closed；需对这些历史活动执行 fenced rebuild，不直接补键或清数据。

## 简历目标与可复现证据

| 简历可写目标（完成后） | 需要实现 | 必须验证 | 原始证据位置 | 当前状态 |
| --- | --- | --- | --- | --- |
| 构建 Go 本地生活优惠券闭环，支持店铺查询、购券、模拟支付、商家核销和未核销退款 | L00--L08 的 API、SQL 迁移、订单/支付/券状态机和本地支付模拟器 | 干净环境下完成成功链路；无效输入、越权、重复回调、支付/关单与退款/核销竞争均有自动化测试 | `internal/order/*integration_test.go`、`scripts/verify-stage-a.sh`、`docs/demo-and-interview.md` | 已实现并本机验证 |
| 以 MySQL 事务、条件扣减、唯一约束和库存流水防止重复成单与超卖 | L06 的活动、订单、`inventory_movements`、事务和唯一键 | 最后一份库存并发竞争、同用户重复购买、事务中断；核对订单唯一性、库存范围及流水恒等式 | `internal/order/mysql_service_integration_test.go`、`fulfillment_integration_test.go` | 已实现；有限本机并发实验通过 |
| 使用 Redis 缓存与进程内缓存降低热点店铺回源，并验证陈旧与失效边界 | 店铺 Cache-Aside；活动 `internal/coupon/` 另有列表/详情两级缓存、详情负缓存、singleflight、单店 generation 与持久失效任务 | 店铺完整响应口径的 60 秒三轮对照；活动空值、热点回源、更新失效、失败退避/租约/poison 隔离有自动化回归 | `internal/shop/*test.go`、`internal/coupon/*test.go`、`artifacts/measurements/20260922-shop-full-body-v3-*`、`benchmark-report.md` | 店铺指标已验证；活动缓存本轮只做正确性回归，未声称性能收益 |
| 通过 Outbox、RabbitMQ 和幂等消费者实现可恢复的异步成单 | L09 的购买请求、Outbox、发布者、Worker、结果查询和幂等事务 | 发布确认丢失、提交后 Ack 前中断、重复消息和处理超期；每个请求终态可查询 | `artifacts/verification/20260922T093000Z-l09/result.json`、`internal/platform/mq/reliability_integration_test.go`、`internal/order/async_service_integration_test.go`、`internal/order/async_redelivery_integration_test.go` | 已实现并在本地 MySQL/RabbitMQ 验证列出的故障；尚非生产环境规模 |
| 使用 Redis Lua 资格预占与 generation 恢复机制处理高并发准入 | schema 2 活动元数据、6 key/sentinel fail-closed、Redis TIME deadline、MySQL 最终校验、补偿、停写重建 | 开售/结束边界、重复请求、延迟处理、损坏 key fail-closed、旧 generation 回补和所有历史成单用户限购恢复 | `internal/admission/`、`internal/recovery/rebuild_integration_test.go`、`cmd/admission-rebuild/` | 已实现并真实依赖回归；旧 generation 无订单 RECEIVED/ADMITTED 安全失败，不做 reservation 迁移 |
| 基于可重复负载建立基线并用 pprof 判断是否继续优化 | L13 的基线、采样、同条件复测和不变量复核 | 固定数据、版本、并发和轮次时长下多轮基线/复测；同时报告失败、超时、未完成和无效轮 | `artifacts/measurements/20260922-*`、`benchmark-report.md`、`profiling-report.md`、`cmd/shop-loadgen/` | 已实现并验证；锁侧无显著项目热点，内存侧完成 typed response 优化，只声明已验证的分配下降 |

## 阶段交付门槛

| 阶段 | 交付门槛 | 允许写入的结论 |
| --- | --- | --- |
| A（L00--L03） | 模块、HTTP、迁移、店铺 CRUD/查询、认证与商家归属检查均通过记录的测试 | 只可说明已实现并验证的基础查询和权限能力 |
| B（L04--L08） | 缓存和同步交易闭环，且所有列出的竞争/失败用例有证据 | 可陈述本地模拟支付和同步交易规则；不可称真实渠道或线上效果 |
| C（L09--L12） | 异步链路及每种中断/重放/恢复用例均通过库存与订单核对 | 可陈述本地故障实验的具体条件和观察结果 |
| D（L13--L14） | 原始测量、环境快照、报告、可复现启动和演示全部齐全 | 仅可按报告中的口径、条件和数值陈述性能结论 |

## 每次交付必备物

1. 源码、迁移、测试与启动配置同步提交到工作区；不以截图替代可执行输入。
2. `go test ./...`、`go test -race ./...`、`go vet ./...` 的实际退出码和输出（按当前已引入模块执行）。
3. 故障用例的注入位置、预期不变量、实际 SQL 核对和原始日志。
4. 每个性能运行独立保存到 `artifacts/measurements/<run-id>/`；`run-id` 使用 UTC 时间戳加简短场景名。
5. 失败、超时和未完成请求不得从性能或正确性结论中省略。

## 2026-09-22 最终验收结果

- [x] 核心交易、商家权限、MQ 重投/幂等、库存恢复与 generation 旧写入回归通过。
- [x] 支付 operation key 在模拟器副作用前完整校验；Purchase/Redeem 在获锁后重取时钟。
- [x] 券码响应丢失后可由本人重新签发；旧码失效，每单仍只有一张券。
- [x] Worker 有界扫描到期订单、PENDING/UNKNOWN 本地模拟退款与 `ADMISSION_STOCK_RESTORE` 任务。活动 204 实测自动关单后 MySQL `available_stock=1`、唯一 RELEASE=1、Redis stock=1，原用户 `S:` 购买标记保留。
- [x] 量化采集器将 body read/close 纳入延迟并记录 transport/read/close 失败；异步行创建时间与首次可见上界分开。
- [x] 修复 fixed-duration 结果通道死锁并保留无效 v2；v3 在相同数据、预热、并发和 60 秒轮次下完成基线/优化各3轮。
- [x] `go test ./...`、`go test -race ./...`、`go vet ./...` 及 `LOCAL_LIFE_INTEGRATION=1 go test -race -count=1 ./internal/...` 通过。
- [x] 迁移可重复执行；API/Worker 可独立启动并优雅停止；Stage A 权限演示通过。Compose 停止/重启后订单与 Redis 库存数据仍保留。
- [x] 最终环境、指标、SQL 不变量、源码清单及关键结果校验和保存在 `artifacts/measurements/20260922-final-acceptance/`。
- [x] pprof 采样对象、PID/监听端口、二进制哈希和负载时间重叠均有证据；CPU/mutex/block/allocs/heap 使用独立进程采集，诊断开关默认关闭。
- [x] 30 秒 mutex/block 样本完成解释：无值得修改的业务锁热点；未把 goroutine 累加等待当作请求延迟，也未把 mutex 当作 MySQL 行锁。
- [x] 动态 JSON map 分配经 typed response 优化；微基准与应用归一化分配同向下降，JSON 契约及全量回归通过。
- [x] 关闭诊断后完成基线/优化各 3×60 秒交错对照；未观察到稳定 P95 或吞吐改善，报告明确不写这两项收益。
- [x] 活动列表/详情接入两级缓存、详情负缓存、singleflight、单店 generation；商家 PATCH 仅可改所属活动标题，交易条件不可修改。
- [x] 支付记录与商户核销记录接口以 keyset 分页返回，并覆盖归属、越权、游标和不存在资源。
- [x] Lua schema 2 覆盖 accepted_at 窗口、Redis TIME deadline、sentinel/type 损坏 fail-closed 与旧代际恢复；MySQL 仍为最终成单校验。
- [x] 本轮离线 test/race/vet、`LOCAL_LIFE_INTEGRATION=1 go test -race -count=1 ./internal/...`、迁移/API/Worker 启动通过；保留旧 generation 的 recovery task 因 schema 不完整被 defer，未冒险执行。
- [x] 本轮环境、命令、定向故障场景、限制与源码 SHA-256 已保存到 `artifacts/verification/20260922-resume-alignment/`。
- [ ] 保留数据中旧 Redis 代际需按活动执行 fenced rebuild；这是升级运维项，不通过清数据或伪造 sentinel 跳过。
- [ ] 用户亲手修改、独立复述与模拟面试仍待完成；不影响工程验收，但不能标记为已掌握。
