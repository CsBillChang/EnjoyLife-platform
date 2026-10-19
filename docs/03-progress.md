# 03｜当前进度与验证记录

更新日期：2026-10-19。项目：惠享生活 · Go 本地生活服务平台。

最新维护结果（2026-10-19）：迁移与恢复可靠性修复已完成；本轮对 `091d540` 运行隔离最终回归，全仓普通测试、race、vet、现有恢复专项与三组迁移故障对照均通过，实际测试用例无跳过，详见文末记录。保留旧库保护策略、业务分包和 Worker 位置，未清现有数据卷。代码仅本地提交，未推送、未创建 Release；[Issue #1](https://github.com/CsBillChang/EnjoyLife-platform/issues/1) 保持开启。2026-09 的交付与性能记录仍为历史证据，以下增量不重新声明性能收益。

## 当前状态

- 本轮完成：L00–L14 工程范围，包括店铺/权限、两级缓存及持久失效、同步交易、模拟支付/核销/关单/退款、Outbox+RabbitMQ 异步成单、Lua 预占、限流、有界 Worker 和 generation 重建。
- **当前任务：工程、pprof 补充分析与简历原句对齐已验收，进入演示/面试复盘。** 增量源码与真实依赖验收见 `artifacts/verification/20260922-resume-alignment/`；性能和 pprof 数据分别见 [量化报告](benchmark-report.md)与 [pprof 报告](profiling-report.md)。保留数据中的旧 Redis 代际需按活动做 fenced rebuild，用户动手与面试复盘仍待进行。
- 工程状态：5 个 SQL 迁移可从空库重放；`huixiang-life` 独立 MySQL/Redis/RabbitMQ 容器健康；稳定启动、故障、负载与证据脚本已交付。
- 验证状态：全仓 `go test ./...`、`go test -race ./...`、`go vet ./...` 通过；带 `LOCAL_LIFE_INTEGRATION=1` 的全部 internal 真实依赖 race 回归通过。完整响应口径店铺查询三轮中位 P95 33.427→5.179 ms；500 用户“受理到订单行创建” P95 4.096 s，首次观察终态上界 P95 5.147 s。pprof 未发现显著业务锁热点；typed response 的 handler 微基准为 20 allocs/op（基线 25），正式 HTTP 对照未显示稳定 P95/吞吐收益。
- 学习状态：用户有 Python/C 和基础 SQL 经验，本项目内的手动运行、解释与修改均待完成。
- 范围：店铺查询、优惠券抢购、订单支付模拟、到店核销与未核销退款；采用模块化单体与后台 Worker。
- 状态约束：每用户每活动最多成功创建一单；关闭或退款也不重购，未成单失败可以新请求重试。
- 实施入口：[项目说明](../README.md)、[协作约定](../agent.md)、[目标契约](01-design-and-contracts.md)、[任务卡](02-work-items.md)。

## 任务状态

| 卡片 | 工程状态 | 用户动手状态 |
| --- | --- | --- |
| L00 Go 店铺 CLI | 已验证 | 待学习 |
| L01 Gin 店铺查询 | 已验证 | 待学习 |
| L02 MySQL 与 GORM | 已验证 | 待学习 |
| L03 用户登录与商家权限 | 已验证 | 待学习 |
| L04 Redis 店铺缓存 | 已验证 | 待学习 |
| L05 Ristretto、singleflight 与缓存一致性 | 已验证 | 待学习 |
| L06 优惠券活动与同步下单 | 已验证 | 待学习 |
| L07 模拟支付与券核销 | 已验证 | 待学习 |
| L08 超时关闭与退款回补 | 已验证 | 待学习 |
| L09 购买请求、Outbox 与 RabbitMQ 异步成单 | 已验证 | 待学习 |
| L10 Lua 资格预占与恢复 | 已验证 | 待学习 |
| L11 活动、用户限流与有界 Worker | 已验证 | 待学习 |
| L12 故障恢复与库存核对 | 已验证 | 待学习 |
| L13 pprof 与负载优化 | 已验证 | 待学习 |
| L14 评测与交付 | 已验证 | 待学习 |

工程状态使用“未开始 / 进行中 / 已实现未验证 / 已验证 / 受阻”；用户动手状态单独记录，不因代码通过而标记掌握。

## 每次推进后追加

```text
日期 / 卡片 / 本次子步骤：
目标与新增目录职责：
实际完成与输入 → 执行 → 输出链路：
实际命令、退出结果和观察到的现象：
成功场景 / 失败场景证据：
未运行或未覆盖项：
用户已手动运行 / 已解释 / 已修改的内容：
待用户完成的一处练习：
本次面试问题、对应代码/实验、用户复述与追问表现：
待补基础、最小例子与下次复习安排：
当前卡剩余内容与下一步：
```

模拟支付不代表真实渠道接入；性能目标与实测值分开记录。L10 必须当卡验证预占后崩溃与恢复，L12 不能作为推迟关键失败检查的理由。

## 2026-09-13｜本轮准备材料检查

- 完成六份独立项目文档、L00–L14 任务卡、根入口和相关 Go 课件同步；简历第二项目与 Redis/RabbitMQ 技能已替换。
- 工作区文档检查通过：29 份 Markdown、96 个本地链接及锚点；代码围栏完整，活动入口无旧项目路径。
- 简历源码完成大括号、环境、资源引用与保留内容检查；18 文件源码包通过完整性及实际来源文件比对。按用户要求未编译。
- ResearchLoop 和 Go 教学示例共 13 个文件的哈希未变；旧项目资料归档后已移出当前开发入口。
- 独立审阅后统一了缓存引入顺序、accepted_at 与 processing_deadline 规则。以上均为文档验证，不是库存、支付、恢复或性能测试结果。
- 当前仍为 L00 未开始；未安装业务依赖、未新增业务代码，用户运行、解释和修改练习均待开始。

## 2026-09-13｜补充面试学习安排

在协作规则中加入每卡技术解释、代码复盘、2–3 个递进问题、独立修改证据及阶段模拟面试；GPM、GC、索引、事务等基础按阶段补充短专题。本次仅更新教学文档，相关复述、练习和模拟面试尚未进行，L00–L14 业务卡状态不变。

本次检查：四份修改文档的本地链接、代码围栏及学习/业务状态区分检查通过；未运行代码测试。

## 2026-09-14：补充全局与文件结构导览

用户说明习惯先看整个项目框架，再进入局部开发。本轮新增 [全局与目录地图](04-project-map.md)，并将“先讲完整架构和目录职责、再定位当前任务”写入协作规则。地图中的目标路径是示例，按实际任务逐步创建；本轮只整理文档，没有新增业务代码、安装依赖或运行业务测试。原有工程进度保留，用户全局复述与动手状态待实际反馈。

本轮文档检查：31 份 Markdown、122 个本地链接与标题锚点检查通过；代码围栏完整。12 个已有代码与配置文件的 SHA-256 保持不变，目标业务目录未提前创建。本轮未重跑业务测试或外部 API。

## 2026-09-22：切换交付优先并定义量化口径

用户希望 Codex 连续完成项目与量化验证，以便尽快投递。本轮核对目录与历史记录，新增 [交付与量化计划](07-delivery-and-metrics.md)，更新协作入口。后续按里程碑连续实现/测试/修复，不因学习练习未完成而停顿；历史“再次授权才进入下一卡”是旧节奏，不再作为默认停止条件。

本轮只改协作和计划文档，未启动业务开发、Docker、模型评测或压测，未更改模型设置。历史验证结果没有重跑，工程状态和学习掌握均未上调，所有新增量化数值仍待真实测量。

本轮静态检查：12 份修改/新增文档的 88 个本地链接及代码围栏通过，24 个现有业务代码/配置文件的 SHA-256 未变。未运行项目测试或量化实验。

## 2026-09-22：里程碑 A 与同步交易核心交付

- 建立独立 Go 1.26 模块；锁定 Gin 1.10.0、GORM 1.31.2、GORM MySQL 驱动 1.6.0、go-redis 9.22.0、Ristretto 2.4.2、x/sync 0.23.0 与 RabbitMQ 客户端 1.15.0。依赖只写入本项目 `go.mod/go.sum`。
- 建立 `huixiang-life` Compose：MySQL `127.0.0.1:13306`、Redis `127.0.0.1:16379`、RabbitMQ `127.0.0.1:15672` / 管理台 `15673`，使用独立网络和具名卷；未触碰旧 `support-agent-platform` 资源。
- L00 CLI 成功查询 ID 1；不存在 ID 返回非零退出。L01--L03 的 HTTP、GORM/MySQL、登录会话及商家归属已接通。Stage A 脚本实际得到 `200/400/401/403/404`，消费者更新店铺为 403，商家跨店更新为 404；MySQL 容器重启后已提交店铺更新仍存在。
- L04--L05 已实现 Ristretto → Redis → MySQL、负缓存、soft/hard TTL、有界刷新及 singleflight。单元竞态测试通过；真实 Redis 冷读后 10 次请求观察 `BackendLoad=1、L1Hit=9`，API 重启后观察到 L2 命中。正式固定负载对照尚未执行；持久缓存失效任务仍待补。
- L06 真实 MySQL 测试：8 用户争抢库存 1，1 次成单、7 次售罄；同键重放返回同一订单，同用户换键被拒；扣库存后制造外键失败，事务回滚后库存 1、订单 0、DEDUCT 0。
- L07--L08 真实 MySQL 测试：支付/关单竞争 6 轮观察支付胜 2、关单胜 4；退款/核销竞争 6 轮观察退款胜 5、核销胜 1。重复支付、重复退款结果和迟到支付补偿均保持每订单 `RELEASE <= 1`，每轮核对库存范围、库存恒等式与订单唯一性。支付与退款均为本地模拟器，不是真实渠道。
- 修正时区边界：Go 与 MySQL 会话统一 UTC，避免 `DATETIME` 被本机 Asia/Shanghai 解析后产生 8 小时活动/支付截止偏移。
- 实际检查：`go test ./...`、`go test -race ./...`、`go vet ./...` 均通过；MySQL 8.4.11、Redis 7.4 容器健康。以上不是正式性能结果。

## 2026-09-22：L09 开始与本轮交接

- 已应用 `purchase_requests`、`outbox_events`、`recovery_tasks` 迁移；已实现带租约的 Outbox 领取/失败退避、RabbitMQ durable 拓扑、持久消息和 Publisher Confirm 基础设施，单元/竞态/vet 通过。
- RabbitMQ 4.1 镜像已拉取并使用项目独立资源启动；本轮结束前仍需完成异步请求服务、API/Worker 接线与真实重复消息/提交后 Ack 前中断验证，未将 L09 标成完成。
- 未完成：L05 持久缓存失效任务；L09 真实发布消费闭环；L10 Lua/generation 停写重建；L11 限流和有界 Worker；L12 组合恢复；L13 正式基线/pprof/复测；L14 演示、报告和简历证据。当前没有可写入简历的性能提升数字。
- 用户动手、复述与面试复盘仍全部待阶段末进行，不能由上述工程测试推定已掌握。

## 2026-09-22：L09 真实闭环与可恢复投递

- HTTP 持久化受理→Outbox→RabbitMQ→Worker→MySQL 订单事务端到端通过：202 后查询到 SUCCEEDED，同幂等键返回同一 request/order，订单 1、DEDUCT 1、Outbox SENT。单样本 accepted_at 到 order.created_at 为 38,661 微秒，仅作正确性观察，不是 P95 或性能结论。
- 8 个并发处理者重复处理同一持久请求，最终仅 1 单和 1 条扣减流水；过期请求与代际变更请求均落为可查询 FAILED，库存不变。
- Outbox 使用独立 owner token 围栏租约，旧 Worker 不能覆盖新租约；重试最多 20 次后 DEAD，耗尽事件不再毒化后续批次。真实 RabbitMQ 解绑后 mandatory Return 未被 Ack 误判成功，重绑后按 MessageId 读回确认消息。
- 实际停止 RabbitMQ 时 API 仍将请求持久化并返回 202；Worker 观察到 250ms、500ms、1s、2s 退避，broker 恢复后请求 SUCCEEDED、Outbox SENT。原始摘要见 `artifacts/verification/20260922T093000Z-l09/result.json`。
- 精确重投实验中，第一消费者在 MySQL 订单提交已可查后直接关闭 channel，未 Ack/Nack/Reject；第二独立连接收到 `Redelivered=true` 的相同 MessageId/消息体，重放后仍是同 request/order，最终订单 1、DEDUCT 1、RELEASE 0、库存 0，Ack 后队列为空。
- 未完成：L05 持久缓存失效；L10--L14。尚无 P95、吞吐、错误率或 pprof 结论。

## 2026-09-22：L05、L10–L14 交付收口

- L05：店铺更新与 `SHOP_CACHE_INVALIDATE` 恢复任务在同一 MySQL 事务写入；dispatcher 使用 generation、lease token、有限重试与指数退避，旧 worker 不能覆盖新更新。真实 MySQL+Redis 集成测通过。当前语义是当前 API 实例 L1 + 共享 Redis L2 失效，不声称多实例 L1 广播。
- L10–L11：请求先持久化，Lua 再按 activity+generation 原子预占，MySQL 最后写 ADMITTED+Outbox；崩溃后按 `reservation_state` 恢复。Redis TIME 令牌桶同时限制活动和用户，Redis 故障时 fail-closed，未写 MySQL 购买请求。真实脚本观察到 429 与 Redis 中断时 503。
- L12：新增持久重建任务与 MySQL GET_LOCK，顺序为 FROZEN→NORMALIZED→INSTALLED→VERIFIED→Redis OPEN→MySQL OPEN。五个中断点加正常完成共 6 场真实实验可续做；已有 CLOSED/REFUNDED 订单仍保留限购，旧代际释放不能修改新库存。演示活动 4 实际重建为 task 19 / generation 2，按 task ID 重放无副作用。
- L13：10,000 店铺、20,000 请求/轮、100 并发、80/20 热点。MySQL-only 与两级缓存热态各 3 轮，P95 中位 14.091→2.048 ms（降低约 85.5%），吞吐中位 17,757→94,192 req/s（约 5.30 倍），120,000 对比请求零错误。回源审计轮 MySQL prepared 执行量减少 90.5%。
- L13 异步：有效轮为活动 202，500 独立用户/500 请求/100 并发；`accepted_at → orders.created_at` P50/P95/P99 为 3.005/3.927/4.204 s，500 SUCCEEDED、0 FAILED、0 UNFINISHED。两个采集器缺陷轮保留为无效证据，不纳入结论。
- L14：新增 `benchmark-report.md`、`resume-evidence.md`、`demo-and-interview.md`、固定负载和异步采集工具。共享简历源码未修改，未安装/编译 LaTeX。
- 最终回归：全仓 test/race/vet 通过；`LOCAL_LIFE_INTEGRATION=1 go test -count=1 -race ./internal/...` 对 MySQL、Redis、RabbitMQ 的真实回归全部通过；001–005 已在临时空库重放后删除该临时库。
- 未完成/边界：用户尚未完成亲手修改与模拟面试；未做多机/云端压测、多 API 实例 L1 广播、真实支付渠道或生产部署。pprof 只有 40 ms CPU 样本，不足以证明请求 CPU 热点；已如实保留。

## 2026-09-22：最终交付验收

- 使用 gpt-6-astra high 子代理审查交易/恢复边界，gpt-5.6-terra medium 子代理审查量化采集器；主代理完成接线、集成与最终验收。
- 修复支付副作用前参数校验、锁后时间边界、券码丢失后重新签发、Worker 自动关单/退款恢复与 MySQL RELEASE→Redis 库存提示幂等回补。重建新增库存流水平衡校验；Redis 部分 key 丢失时 fail-closed。
- 活动 204 真实验证：订单被 Worker 自动关闭，MySQL 只有一条 RELEASE，MySQL/Redis 库存均回到1，原用户已购 `S:` 标记保留。保留数据的约1500个旧代际回补任务因缺 sentinel 安全待恢复；公平调度后新任务仍在有界批次中完成。
- 查询采集器改为完整读取/关闭 body 后停时并分类记录失败；发现并修复 fixed-duration 结果通道死锁。无效 v2 保留，v3 以 10 秒预热 + 3×60 秒/模式重测。
- 异步采集拆分为订单行创建时间与首次观察终态上界，不再将 `orders.created_at` 命名为 commit 耗时。活动 203 的 500 请求全部成功，poll error=0，队列 ready/unacked=0/0。
- pprof 仍只有 40 ms CPU 有效样本，未做无证据的 CPU 调优或收益宣称。共享简历源码未修改，未安装/编译 LaTeX。
- 独立启停复验：迁移重放成功，API `/health` 正常，API/Worker 均能响应 SIGTERM；Stage A 权限脚本的 200/400/401/403/404 断言全部通过。Compose `stop` 后再 `up -d` 依然保留订单 2262 和 Redis 库存值，未删除任何卷。

### 本轮收口记录

- **完成项：** 核心交易/权限/重投幂等/库存恢复审查与修复，全仓及真实依赖回归，v3 查询重测，活动 203 异步重测，独立迁移/演示/停止复验，文档与证据同步。
- **未完成项：** 保留数据里的旧 Redis 代际尚未逐活动做 fenced rebuild；用户亲手修改、独立复述与模拟面试尚未验收。
- **阻塞：** 新建或已重建活动的演示与代码交付无阻塞；旧代际回补任务会安全 fail-closed，这是保留数据升级的运维工作，不能通过补 sentinel 或清数据绕过。
- **下一步：** 停止新功能扩展，按 `demo-and-interview.md` 做一次用户主导演示与面试复盘；只有运维上需继续使用旧活动时，才逐个执行 fenced rebuild。

## 2026-09-22：确认以简历为准的补充验收

用户最新决定：保留简历中的 pprof 锁竞争与内存开销分析目标，继续取得对应证据。现有业务闭环验收保留；补充分析仍未完成，不再仅安排面试复盘，也不能以 40 ms CPU 样本宣布该项达成。 执行范围、顺序和完成条件见 [08-profiling-evidence.md](08-profiling-evidence.md)。

本轮只核对代码、环境工具及简历原文并更新验收文档；未修改业务代码、未启动压测、未调用付费模型。新指标、功能与用户掌握状态均未标为完成。文档静态检查结果另行记录。

本轮文档静态检查：13 份修改/新增 Markdown 的 98 个本地链接及代码围栏通过；未运行业务测试。

## 2026-09-22：pprof 锁竞争与内存开销补充验收

- 按既定分工使用 gpt-6-astra high 审查锁竞争解释和优化门槛，gpt-5.6-terra medium 审查采样脚本；主代理完成诊断配置、实际采集、优化、回归和文档集成。
- API 新增默认关闭的 `PPROF_MUTEX_FRACTION`/`PPROF_BLOCK_RATE`；采集脚本保存 PID/PPID、HTTP 与 pprof 监听端口、二进制哈希、脱敏配置和 UTC 时间线。CPU、mutex、block、allocs、heap 每类均启动新进程，100 并发、10 秒预热、45 秒负载，30 秒 profile 完整落在负载窗口内。
- 新 CPU profile 在 30.12 秒墙钟内得到 68.78 CPU 秒样本，解决旧 40 ms 样本无效问题。mutex 累计等待 1,689.16 ms；`singleflight.Do` 链路只占 3.60%，未发现值得修改的业务锁热点。block 主要为多 goroutine 的空闲/保护性等待，不作为请求延迟。
- allocs 定位到详情响应的动态 `gin.H`/JSON map 编码。只将成功详情响应改为 typed struct 后，handler 五轮微基准从 25 降至 20 allocs/op，B/op 约降低 6.2%；应用 heap 轮按完整响应归一化的分配字节与 malloc 分别约降低 14.30% 和 17.26%。
- 关闭 mutex/block 诊断后，优化前后固定二进制交错完成各 3×60 秒。六轮共 13,545,409 个完整 HTTP 200、各类错误为 0；P95 中位 4.902→4.894 ms，吞吐中位 40,415.54→39,826.50 req/s，方向不稳定，因此只声明分配下降，不声明 P95/吞吐提升或内存泄漏修复。
- 回归通过：`go test ./...`、`go test -race ./...`、`go vet ./...`、`LOCAL_LIFE_INTEGRATION=1 go test -race -count=1 ./internal/...`。共享简历源码未修改，也未安装或编译 LaTeX。

### 本轮收口记录

- **完成项：** 采样对象/开关/负载重叠核对，CPU/mutex/block/allocs/heap 原始采集，项目栈解释，typed response 优化，同条件微基准/heap/正式 HTTP 对照，报告与简历证据同步。
- **未完成项：** 保留数据里的旧 Redis 代际仍需按实际使用活动执行 fenced rebuild；用户亲手演示、独立复述与模拟面试尚未验收。
- **阻塞：** 工程与本机证据无阻塞。结果不支持“锁优化”“吞吐提升”“P95 提升”或“修复内存泄漏”，这些不是待包装的收益。
- **下一步：** 不再扩展功能或无限调参；按 [演示与面试材料](demo-and-interview.md)完成用户主导演示和面试复盘。

## 2026-09-22：简历原句对齐补充验收

- 使用 gpt-5.6-terra medium 子代理完成活动展示缓存与记录查询的边界实现，gpt-6-astra high 子代理审查并补齐 Lua 时段、generation 与恢复一致性；主代理完成路由接线、集成和验收。
- 新增活动列表/详情查询的 Ristretto→Redis→MySQL 展示缓存：详情负缓存、singleflight、单店单调 generation；商家仅能修改本人活动标题。标题更新与 `COUPON_CACHE_INVALIDATE` recovery task 同一 MySQL 事务提交，dispatcher 采用 lease、退避、generation 检查与 poison 隔离。固定 TTL 为 30 秒、负缓存 10 秒；没有 TTL 抖动、Pub/Sub 或多实例 L1 广播。
- Lua 升级为 schema 2：以活动元数据校验 `start <= accepted_at < end`，以 Redis TIME 校验当前时间小于 processing_deadline，并对元数据、库存、用户、request owner、release 和订单回补六 key/sentinel 的类型/完整性 fail-closed；MySQL 仍复核代际和交易事实。
- fenced rebuild 不再尝试迁移未知旧预占：旧 generation 中无订单的 RECEIVED/ADMITTED 失败为 `ACTIVITY_GENERATION_CHANGED`；重建恢复所有历史成单用户限购。保留旧 generation recovery task 因 schema 不完整被 worker 报 incomplete 并 defer，未清数据或伪造 sentinel。
- 新增 `GET /me/payment-records` 与 `GET /merchant/redemption-records?shop_id=`，均为稳定 keyset 分页并验证归属、越权、游标和不存在记录。
- 本轮实际验证：离线 test/race/vet、`LOCAL_LIFE_INTEGRATION=1 go test -race -count=1 ./internal/...`、迁移、API/Worker 独立启动均通过。性能实验未重跑：已有数值仅覆盖店铺详情旧版本路径；异步指标仍命名为 `accepted_at → orders.created_at`，不称 transaction commit。

### 本轮收口记录

- **完成项：** 活动展示/失效、Lua 原子时段与 sentinel、旧代安全恢复、支付与核销记录查询、简历逐句证据矩阵、演示与验收文档。
- **未完成项：** 旧 Redis 代际仍需按实际使用活动执行 fenced rebuild；用户亲手演示、独立复述与模拟面试未验收。
- **阻塞：** 本机工程与证据无阻塞。旧 recovery task 的 defer 是安全升级策略，不可通过清数据绕过。
- **下一步：** 停止扩展业务，按 `demo-and-interview.md` 做一次用户主导演示；只有继续使用旧活动时才逐个 fenced rebuild。


## 2026-09-23：简历事实修订与最终范围冻结（文档工作）

根任务核对当前代码、已保存的验收与实测证据，修订共享简历中不受证据支持的效果和范围表述。后续以 [09-final-closeout.md](09-final-closeout.md) 为最终收束约定；旧简历哈希与旧矩阵保留作历史，不据此追加功能。本轮未改业务代码、未重跑测试、未发起付费调用，个人面试掌握未上调。


## 2026-09-23：开发基准纠偏（文档工作）

开发已结项，不再主动启动完善或新的演示验收任务；用户自行运行与面试，只有实际故障或新需求才重开。 已同步09与协作入口。用户选择原稿技术细节作为基准，收益后据实调整。未改业务代码、依赖、模型配置或共享简历，未运行新测试/付费调用，未在根任务创建Goal。

## 2026-10-19｜恢复可靠性维护：基线与迁移

- 回退基线：初始提交 `eccaf9ff006c77d451fbbf7badfa073b22f14118`，标签 `baseline/architecture-fixes-20261019`；`.git/architecture-baseline-20261019.bundle` 已通过 `git bundle verify`。基线包含 283 个未被忽略的原有文件；被忽略的原始实验样本保持原位，不清理或强制纳入 Git。修复分支为 `codex/recovery-reliability-fixes`。
- 目录职责不变：移除 Compose 的 migrations 初始化挂载，统一由 `cmd/migrate → platform/migrate` 执行 DDL 和写账本；没有修改 001–005 历史迁移。
- 增加迁移前检查：目录不存在时不写账本；业务表已存在而迁移账本缺失或为空时，在任何 DDL 前报错，提示核对 schema 后建立明确基线，不自动补账。已存在正常账本继续增量迁移。
- 新增真实 MySQL 测试覆盖空库执行及重放、保留已有行、缺失/空账本、旧 Docker 直接执行 SQL 的无账本状态、无效目录。修复前缺失账本与无效目录测试确实失败，修复后通过。
- 验证环境：独立临时 MySQL 8.4.11 容器，`/var/lib/mysql` 使用 tmpfs，非现有业务库；`LOCAL_LIFE_INTEGRATION=1 LOCAL_LIFE_MIGRATION_TEST_DSN=<临时实例管理连接> go test -race -count=1 -v ./internal/platform/migrate` 通过，迁移 CLI 在临时业务库连续执行两次成功。`docker compose config --quiet` 与 `git diff --check` 通过。
- 只读核对：现有 `huixiang_life.schema_migrations` 已记录 001–005，不属于无账本旧库；没有对它执行 DDL 或补账。
- 用户运行、解释、修改状态仍待验证；本轮工具验证不表示用户已掌握。下一步按授权处理坏任务隔离与库存回补租约。

## 2026-10-19｜恢复可靠性维护：坏任务隔离

- 店铺缓存失效与库存回补领取到不兼容 JSON、缺失字段或错误 generation 时，同事务持久记录 `FAILED`、清理租约并保留原因，提交后再返回错误；下一轮可以继续领取健康任务。活动失效已有同等行为，本轮保留并回归。
- 店铺领取同时修正最终尝试的活跃租约被提前退休的问题：只有 PENDING 或租约已过期的 RUNNING 才能因预算耗尽转 FAILED。
- 修复前，新增真实 MySQL 测试在 `{}`、`null`、数组、字段类型不符、代际不符等五类输入上均观察到坏任务仍为 PENDING；活跃最终租约也确实被错误变为 FAILED。修复后同组测试、健康任务继续完成、活动失效的 poison/lease 既有回归全部通过 race 检查。
- 命令：`LOCAL_LIFE_INTEGRATION=1 MYSQL_PORT=<临时端口> MYSQL_DATABASE=huixiang_architecture_test MYSQL_PASSWORD=<临时密码> go test -race -count=1 -v ./internal/shop ./internal/coupon ./cmd/worker -run 'TestMySQL(ShopInvalidationPoison|ShopInvalidationPreserves|StockRestorePoison)|TestCouponInvalidation'`。
- 验证使用临时 MySQL 实例中的连接私有临时表；没有更改现有业务恢复任务，也未拆分 Worker 或增加依赖。下一步处理库存回补租约所有权。

## 2026-10-19｜恢复可靠性维护：库存回补租约与完成验证

- 每次领取库存回补任务生成新的随机 owner token，保存在现有 JSON payload 中；领取在获取行锁后用数据库当前时间生成 10 秒租约。无新增依赖、表结构或目录。
- 成功与重试均校验任务 ID、kind、generation、attempt、owner token、RUNNING 状态和数据库当前时间下有效的租约；影响行数不是 1 时明确返回租约丢失。重试退避从操作结束后的时间计算。
- 修复前，真实 MySQL 测试分别复现旧执行者在重新领取后覆盖成功/重试状态，以及租约过期但未被重新领取时仍可成功/重试。修复后四种操作全部被拒绝；正常重试及完成、逐字段身份不符也已覆盖。
- 真实 MySQL+Redis 实验：首次回补 Redis 后模拟进程在记录完成前中断，租约过期后重新领取并重放，任务第二次尝试完成；Redis 库存仍只增加一次，历史已成单用户的新购买请求仍被拒绝。
- 独立专项脚本 `scripts/verify-recovery-reliability.sh` 已实际运行并通过：校验 Compose 不再挂载 SQL，启动 tmpfs MySQL/Redis，执行迁移集成测试、CLI 两次迁移、shop/coupon/worker 三包全部 race 测试，退出时只移除本轮临时容器。完整输出保存在 `.git/verification-recovery-20261019/integration.log`。
- 全仓 `LOCAL_LIFE_INTEGRATION=0 go test -race -count=1 ./...`、`go vet ./...`、脚本 `bash -n` 与 `git diff --check` 通过。真实依赖验证限于受影响包及专项场景，未重跑全套真实依赖验收或性能实验。
- 发布/回退：本轮没有替换正在运行的业务 API/Worker。后续切换 Worker 版本先停止旧 Worker；旧 RUNNING 可在过期后由新进程接管，不能混跑仍缺少 fencing 条件的旧二进制。代码回退同样先停止并等待在途处理结束，不删除订单、库存流水或持久任务。
- 回退点：迁移修复提交 `0185601`；隔离修复提交 `821b9bd`；最终租约提交由标签 `checkpoint/recovery-reliability-20261019` 指向。保存后续工作后，如需逆序撤回，可依次执行 `git revert checkpoint/recovery-reliability-20261019`、`git revert 821b9bd`、`git revert 0185601`。这些命令只回退代码；原始基线仍由 `baseline/architecture-fixes-20261019` 与本地 bundle 保留。
- 可复现实例：仓库根目录运行 `scripts/verify-recovery-reliability.sh`，成功后自动清理它新建的临时实例。失败场景见 `TestMySQLStockRestoreLeaseFencesStaleOwners` 与 `TestMySQLRedisStockRestoreCrashReplayPreservesPurchaseLimit`。
- 用户运行/复述/修改仍待完成。可选亲手练习：在专项测试的隔离实例里，让旧任务租约过期，解释为何 `Succeed` 与 `Retry` 都必须返回租约丢失，以及为何 Redis 重放仍不能二次加库存；不要对现有业务表直接制造故障。
- 当前授权范围已完成；无需继续拆 Worker、调整业务分包或新增功能。

## 2026-10-19｜P2 复审修复 1：首次迁移中断续跑

- 修复前，用测试专用 MySQL connector 在真实执行后注入中断，分别停在只创建 users 后，以及 001 全部 DDL 完成但正式记账前；两种状态重跑均被旧库保护误拦。日志：`.git/verification-p2-20261019/migration-before.log`。
- 最小修复保留现有账本结构：只在空库或匹配的工具检查点状态下，于 001 DDL 前写入保留检查点；只允许当前纯幂等 CREATE 形式的 001 自动续跑，并验证源码 SHA-256。正式记账与删除检查点在同一 metadata 事务中提交，没有提前把未完成迁移记为成功。
- 验证通过：两处中断恢复并保留已有 users 行；恢复后账本恰有 001–005 五条正式记录；源码哈希变化、非幂等编辑仍拒绝；无账本/空账本旧库、旧 Docker 初始化库继续拒绝自动接管。
- 隔离 MySQL 8.4.11 tmpfs 实例执行 `LOCAL_LIFE_INTEGRATION=1 LOCAL_LIFE_MIGRATION_TEST_DSN=<临时管理连接> go test -race -count=1 -v ./internal/platform/migrate` 通过。日志：`.git/verification-p2-20261019/migration-after.log`。
- 不修改 001–005 历史 SQL、业务分包或现有数据卷。工具只有在 001 纯幂等且源码匹配时续跑；非幂等 ALTER 中断仍需人工核对。存在未完成检查点时先完成恢复，再回退工具版本。
- 用户运行、解释、修改状态仍待验证；后续按授权继续处理租约时钟与等锁过期。

## 2026-10-19｜P2 复审修复 2：统一库存任务时钟

- 真实 MySQL 复现：向领取/重试传入应用时间偏移 +1 小时或 -1 小时。修复前出现活跃租约提前接管、已过期任务无法接管，以及退避时间偏移一小时；四个子场景均失败。日志：`.git/verification-p2-20261019/clock-before.log`。
- 最小修复只调整库存回补链路 SQL：首次入队 available_at、就绪判断、租约过期筛选、租约创建、成功校验、重试校验和重试退避均采用数据库时钟。原有扫描/测试调用参数保留，但不再参与持久任务的时间判定。
- 首次修复后回归发现旧夹具用应用时间构造立即就绪任务，受宿主机与容器时差影响；改为数据库时间构造就绪任务，并用同一时钟验证退避，避免测试依赖两端时钟一致。首次失败日志保留在 `.git/verification-p2-20261019/clock-after.log`。
- 修正夹具后，+1/-1 小时偏差测试、原有过期/接管/身份校验、健康重试、坏任务隔离与恢复编排的 race 回归通过。命令：`LOCAL_LIFE_INTEGRATION=1 MYSQL_PORT=<临时端口> MYSQL_DATABASE=huixiang_p2_test MYSQL_PASSWORD=<临时密码> go test -race -count=1 -v ./cmd/worker -run 'TestMySQLStockRestore|TestRecover'`。通过日志：`.git/verification-p2-20261019/clock-after-fixture-fix.log`。
- 首次入队的补充复现：仅把测试连接的 MySQL 会话时间调慢一小时，原 SQL 生成的任务比数据库时间晚约一小时，验证确实失败（`enqueue-clock-before.log`）；改为 CURRENT_TIMESTAMP(6)，与领取时钟一致。隔离实例的完整 order race 回归结果见 `enqueue-clock-after.log`；验证脚本增加 order 包，覆盖关单、退款、原子回补意图与购买规则。
- 不更改 Redis、业务分包或 Worker 位置；下一项单独复现 UPDATE 等锁跨租约到期点的场景。

## 2026-10-19｜P2 复审修复 3：等锁后重新校验租约

- 修复前，在专用临时库中由一个事务锁住任务行，另一个连接执行成功/重试；通过 performance_schema 确认真正进入行锁等待且租约仍有效，再等数据库确认过期后释放锁。两个场景都错误返回成功，复现日志：`.git/verification-p2-20261019/lock-before.log`。
- 最小修复只调整现有 Worker 库存任务仓储：成功/重试共用短事务，先锁任务行，下一条 UPDATE 再用新的数据库时间校验。保留 kind、generation、attempts、RUNNING、token、未过期条件及 RowsAffected 校验，Redis 操作仍在事务外；缺失任务或已失去所有权返回原有租约错误。
- 针对性 race 验证通过：成功/重试等待期间租约到期均拒绝写入；等待期间 owner 被其他事务替换也不覆盖新状态；拒绝旧 owner 后，新 owner 仍可接管并正常完成。原有坏任务隔离、正常退避、身份字段及恢复编排回归通过。日志：`.git/verification-p2-20261019/lock-after.log`。
- 组合复验通过：`scripts/verify-recovery-reliability.sh` 在新建 MySQL 8.4 / Redis 7.4 tmpfs 实例中运行 migrate、shop、coupon、order、worker 的真实依赖 race 测试，包括旧库拒绝、两处迁移中断、会话时钟偏移、坏任务后的健康任务、租约并发、Redis 回补后崩溃重放与不恢复购买资格。日志：`.git/verification-p2-20261019/integration.log`。
- 全仓离线 `LOCAL_LIFE_INTEGRATION=0 go test -race -count=1 ./...`、`go vet ./...`、脚本语法和 diff 空白检查通过；日志分别为同目录 `offline-race.log`、`vet.log`。这些是本轮故障回归，不是新的 HTTP/RabbitMQ 全链路或性能验收；新增行锁事务的开销未量化。
- 回退基线仍为 `checkpoint/recovery-reliability-20261019`（1f5f575）；本轮三个修复按迁移、时钟、锁等待分别提交，可从最后一项开始逐项 revert。完成点为 `checkpoint/p2-reliability-20261019`，可审查该标签与基线之间的 diff。回退代码不删除数据；若新库仍有未完成的 bootstrap 检查点，先用当前迁移工具完成恢复，再回退迁移工具。
- 只创建和移除本轮拥有的临时实例/临时 schema；已有数据卷和容器保持原样。001–005 历史 SQL、业务分包、Worker 位置不变，无新增依赖或 Issue。本轮工程修复完成，未继续扩展重构范围。
- 用户手动运行、解释与修改状态仍待验证；可自行运行上述脚本，并结合 `TestMySQLStockRestoreRechecksLeaseAfterRowLock` 解释为什么“SQL 开始时未过期”不足以证明“取得行锁后仍拥有租约”。

## 2026-10-19｜并发迁移 P2：同库全流程互斥

- 先将只读复审的复现加入正式测试 `TestMigrationConcurrentBootstrapDoesNotReinsertCheckpointIntegration`。在独立 MySQL tmpfs 实例中暂停 A 的检查点写入，让 B 完成迁移后再恢复 A；修复前两个进程都返回成功，但账本为 6 条、其中 1 条检查点，测试按预期失败。日志：`.git/verification-migration-lock-20261019/before.log`。
- 最小修复限定在迁移包：预留一个 `sql.Conn`，以数据库名派生的 64 字符命名锁覆盖旧库检查、全部 DDL、检查点及最后记账。获取锁最多 30 秒，每次服务端等待 1 秒以兼容应用 3 秒读超时；同库串行，不同库独立。释放使用独立清理 context；无法确认锁状态时丢弃物理连接，不把可能持锁的会话放回连接池。
- 正式回归通过：并发首次迁移后仅 5 条正式记录、无检查点且可重跑；锁持续覆盖 005 最后记账；同实例不同库可独立迁移；等待方与持锁方取消后可重新执行且无持续锁泄漏。无账本/空账本旧库、旧 Docker 初始化库、中断续跑、源码变更拒绝及失败释放锁测试继续通过。完整迁移包日志：`after-final.log`。
- 首次组合测试的立即锁计数断言失败（`integration-initial.log`）：客户端关闭后，服务端可能仍在完成 GET_LOCK 等待。断言改为最多 3 秒有界观察服务端清理并验证后续迁移；若锁持续存在仍失败。取消场景连续 10 次 race 验证通过，日志：`cancellation-recheck.log`。此项没有放宽旧库或账本校验。
- 最终 `scripts/verify-recovery-reliability.sh` 通过，覆盖新建 MySQL/Redis 中的迁移、坏任务隔离、库存回补时钟与租约、崩溃重放等现有专项；全仓 `LOCAL_LIFE_INTEGRATION=0 go test -race -count=1 ./...`、`go vet ./...`、脚本语法与 diff 检查通过。日志在同目录 `integration.log`、`offline-race.log`、`vet.log`。这不是新的 HTTP/RabbitMQ 全链路或性能验收。
- 问题记录：[EnjoyLife-platform Issue #1](https://github.com/CsBillChang/EnjoyLife-platform/issues/1)，保持开启，待后续推送代码后关联提交关闭。本轮只有该 Issue 写入 GitHub；修复独立本地提交，不推送代码或创建 Release。
- 未改 001–005 SQL、迁移账本结构、业务分包、Worker 或依赖；只移除本轮临时实例，现有数据卷和五个原有容器保留。无账本旧库和已有冲突账本仍拒绝自动接管，不自动补账或删除冲突检查点。命名锁只约束使用新版工具的迁移进程，升级前需停止旧进程。
- 回退基线为 `checkpoint/p2-reliability-20261019`（65c8e82）；如需回退可 revert 本次独立提交，仅回退代码。回退前停止迁移进程，并先完成未结束的 bootstrap 恢复；不删除业务数据或修改账本以强行通过。
- 本轮工程修复完成。用户手动运行、解释与修改状态仍待验证；可在隔离测试中对照两个暂停点，解释为何锁必须覆盖旧库检查到最后记账，且不能仅靠连接归还或事务提交来释放。

## 2026-10-19｜迁移异常路径：临时注入固化为正式回归

- 对 `03eb4c2` 的只读复审未发现新的可操作缺陷或旧库保护回归。本轮按授权新增 `internal/platform/migrate/failure_integration_test.go`，将临时验证收敛为正式测试 `TestMigrationFailureReleasesLocksAndRecoversPoolIntegration`；未修改迁移实现或生产业务代码。
- 覆盖七类故障：真实 GET_LOCK 已授予后响应错误/NULL；未真正执行 RELEASE_LOCK 时响应错误/0/NULL；普通 DDL 失败；初始化元数据事务插入正式 001 后、清除检查点前取消。错误与取消必须可识别，原有同库互斥、旧库拒绝、源码变化拒绝和中断恢复测试继续通过。
- 获取结果注入先验证 MySQL 确实已授予锁，再隐藏响应；仅这两项使用参数插值以防驱动 ErrSkip/Prepare 回退绕过注入，并断言实际授予标记。包装器保留原驱动的事务、会话重置和连接验证接口，避免夹具通过额外丢弃连接掩盖回收问题。
- 正式断言通过：结果不确定时物理连接关闭、池中 OpenConnections/InUse 均为 0；普通 DDL 失败后健康连接可复用；事务取消不提交正式 001 且保留检查点。每项均确认无持续命名锁残留，随后使用同一连接池重跑，账本恰为 5 条正式记录、无检查点，池占用归零且有可复用空闲连接。
- 隔离 MySQL tmpfs 实例运行 `LOCAL_LIFE_INTEGRATION=1 LOCAL_LIFE_MIGRATION_TEST_DSN=<临时管理连接> go test -race -count=1 -v ./internal/platform/migrate -run '^TestMigrationFailureReleasesLocksAndRecoversPoolIntegration$'` 通过。日志：`.git/verification-migration-faults-20261019/targeted.log`。
- 组合验证 `scripts/verify-recovery-reliability.sh` 通过，包含七项新回归以及现有迁移、坏任务隔离、时钟、租约和 Redis 崩溃重放专项；全仓 `LOCAL_LIFE_INTEGRATION=0 go test -race -count=1 ./...`、`go vet ./...`、脚本语法与 diff 检查通过。日志在同目录 `integration.log`、`offline-race.log`、`vet.log`。脚本自动发现新增测试，无需修改验证脚本。
- 已在 [Issue #1 补充复审评论](https://github.com/CsBillChang/EnjoyLife-platform/issues/1#issuecomment-6258656485)，保持 Issue 开启以待后续代码推送后关联。此次提交仅含正式测试与验证记录；代码未推送，未创建 Release。
- 证据边界：这些是实际 MySQL 上的驱动响应与取消注入，不等于真实网络分区、完整进程崩溃或生产故障验收；命名锁仍要求参与进程使用新版迁移工具。未增加依赖、改历史 SQL、调整业务分包或 Worker，也未清现有数据卷；本轮临时实例已移除，原有五个容器保留。
- 回退基线为 `03eb4c2`；revert 本次独立测试提交只撤回测试和记录，不撤回已实现的迁移互斥。用户手动运行、解释与修改状态仍待验证。本轮授权范围已完成。

## 2026-10-19｜七类迁移故障复审：固化三组预期失败对照

- `811905d` 复审结论：未发现新的可操作缺陷。七类注入均有触发、连接池状态、锁清理和安全重跑断言；三组对照能证实关键断言会拒绝相应退化。此次只新增 `scripts/verify-migration-fault-controls.py` 并更新验证说明，未修改 Go 实现、正式测试、历史 SQL 或依赖。
- 脚本用 Python 标准库管理独立 MySQL 8.4 tmpfs 容器、随机本机端口和随机临时口令。每个正式子用例仍独立创建并清理临时 schema；对照改动只通过临时 Go `-overlay` 编译，不写回仓库。只移除本次创建的容器，保留所有原有容器和数据卷。
- 实际运行：`python3 scripts/verify-migration-fault-controls.py --output-dir .git/verification-migration-controls-20261019/run-1`，脚本退出 0。四轮均运行 `go test -json -race -count=1 -timeout=30s`，仅选择 `TestMigrationFailureReleasesLocksAndRecoversPoolIntegration`；Go 1.26.4（darwin/arm64）、MySQL 8.4.11。没有跳过用例。

| 轮次 | 通过 / 预期失败 | 实际失败位置与含义 |
| --- | --- | --- |
| 正式代码 | 7 / 0；Go 退出 0 | 七类故障均被触发并正确恢复 |
| 去掉三处物理连接丢弃 | 2 / 5；Go 退出 1 | 获取锁响应错误/NULL、释放锁响应错误/0/NULL 均报 `ambiguous locked session returned to pool`；DDL 失败与事务取消仍通过 |
| 获取锁注入关闭参数插值 | 5 / 2；Go 退出 1 | 获取锁响应错误/NULL 均报 `fault was not exercised`，能识别 ErrSkip/Prepare 回退绕过注入 |
| 同一池第二次迁移直接返回成功 | 0 / 7；Go 退出 1 | 获取锁两项、DDL、事务取消报 `history after retry`；释放锁三项报 `pool not reusable after successful retry`，不会仅凭返回 nil 接受重跑 |

- 结果判定要求完整七项子用例、父测试、包结果和退出码全部符合矩阵，并逐项匹配预期断言。编译失败、跳过、非 JSON、缺失或额外失败、race/panic、schema 清理错误均不能被当作成功；源码锚点不匹配时拒绝运行。只给 Go 子进程清空 `GOFLAGS` 并设置 `GOWORK=off`，避免继承的跳过或覆盖选项改变验证矩阵，不修改用户全局配置。
- 证据：上述目录中的 `summary.json`、四组 `*.jsonl`/`*.stderr.log` 保留完整事件、运行版本、镜像 ID 和两个原始 Go 文件 SHA-256；脚本结束验证这两个文件字节未变。另用本次真实事件流手动构造九种异常输入（跳过、额外失败、漏用例、缺指定断言、编译错误、非 JSON、race、退出码不符、schema 清理失败），解析器均拒绝；结果为 `validator-guards.json`。这九项是本轮脚本判定器检查，不是额外数据库故障注入。
- Python 语法与 `git diff --check` 通过；已有证据目录被拒绝（退出 2），逐文件哈希确认未覆盖既有证据。临时 MySQL 与覆盖层已移除，原有五个运行中容器及一个原有停止容器保留。此次未重跑其余迁移、库存回补、HTTP/RabbitMQ 全链路或性能验收；三组对照只证明指定断言能发现对应退化，不代表覆盖所有网络故障或并发交错。旧库保护逻辑和现有业务分包保持原样，未拆 Worker。
- 已在 [Issue #1 补充本轮复审结论与隔离对照结果](https://github.com/CsBillChang/EnjoyLife-platform/issues/1#issuecomment-6258813100)，并读回确认评论。Issue 保持开启，待以后推送代码时再关联提交。
- 回退基线为 `811905d`；revert 本轮独立提交只移除脚本和相应文档记录，不改变业务行为或数据库。本轮不推送代码、不创建 Release、不扩大重构范围。用户手动运行、解释与修改状态仍待验证。

## 2026-10-19｜最终隔离回归：全仓测试、race、vet 与恢复专项

- 被测提交：`091d5401d74bf63b7c25620a614095461d1d20b9`，开始时工作区干净。实际运行窗口为北京时间 2026-10-19 11:45:02–11:47:06；Go 1.26.4（darwin/arm64），临时 MySQL 8.4.11、Redis 7.4.11、RabbitMQ 4.1.8。
- 所有集成写入都使用本轮新建的 tmpfs 容器、随机本机端口和临时口令；先用 `go run ./cmd/migrate` 初始化测试库。全仓测试开启 `LOCAL_LIFE_INTEGRATION=1`，显式设置测试 MySQL/Redis/RabbitMQ 连接及迁移测试管理 DSN；不使用现有项目依赖，也不启动正常 API/Worker。只在子进程清空 `GOFLAGS`、设置 `GOWORK=off`，未修改全局配置。
- 包间用 `-p=1` 顺序运行，避免共享数据库夹具之间交错；用例内部仍执行真实并发。所有普通/race 测试均 `-count=1`，未复用测试结果缓存。以下命令均在仓库根目录、上述临时连接环境下执行。

| 检查 | 实际命令 | 结果 |
| --- | --- | --- |
| 全仓测试 | `go test -json -count=1 -timeout=120s -p=1 ./...` | 退出 0；14 个有测试包通过，181 个顶层测试、247 个子用例通过，实际用例跳过 0 |
| 全仓 race | `go test -json -race -count=1 -timeout=120s -p=1 ./...` | 退出 0；同样 14 包、181 个顶层测试、247 个子用例通过，实际用例跳过 0，无 race 报告 |
| 全仓 vet | `go vet ./...` | 退出 0，无诊断 |
| 现有恢复专项 | `bash scripts/verify-recovery-reliability.sh` | 退出 0；migrate/shop/coupon/order/worker 五包 race 测试通过，共 112 个顶层测试、197 个子用例，跳过 0；迁移 CLI 连续两次成功 |
| 迁移故障预期失败对照 | `python3 scripts/verify-migration-fault-controls.py --output-dir .git/verification-final-20261019/run-1/migration-fault-controls` | 脚本退出 0；正式代码 7 项通过；三组覆盖层按指定断言分别失败 5/2/7 项，没有将额外失败或跳过算作成功 |

- 恢复专项自行创建另外的 MySQL/Redis 临时实例；其 order 测试继承本轮临时 RabbitMQ 地址，实际验证提交后 Ack 前断连与 broker 重投，没有连接现有业务 RabbitMQ。全仓集成还执行 Redis Lua 原子校验、限流、活动缓存、Outbox、代际重建中断恢复与历史限购等现有测试。
- 无测试文件的 8 包：`cmd/admission-init`、`cmd/admission-rebuild`、`cmd/api`、`cmd/benchmark-seed`、`cmd/migrate`、`cmd/seed`、`cmd/shop-cli`、`internal/platform/database`。Go JSON 中这些包的包级 skip 表示无测试文件；没有具名测试用例被跳过。`cmd/migrate` 另有上述真实 CLI 执行，但这不代表所有入口错误路径都有直接测试。
- 原始证据目录：`.git/verification-final-20261019/run-1/`。包含全仓普通/race 的完整 JSON 事件、各阶段 stdout/stderr、版本和镜像 ID、前后源码 SHA-256 清单、容器与具名卷快照、恢复专项审计，以及故障对照子目录。测试期间全部已跟踪文件哈希未变；记录本段前工作区仍干净。全部六个本轮临时容器已移除，既有容器与具名卷保留。
- 收尾工具误报及复核：七个执行阶段均退出 0；临时总控初次退出 1，因为 Docker inspect 两次返回同一旧容器的 Mounts 数组顺序不同，按有序列表比较被误判为资源变化。逐项内容完全一致；对挂载列表规范排序后，保存的前后快照及再次读取的实时快照一致，容器 ID/状态/启动时间/重启数/挂载和具名卷清单均未变化。保留原始 `summary.json` 与 `runner.log`，不覆盖初次失败；`audit-summary.json` 记录原因及复核通过结论，复核程序退出 0。此项没有触发业务修复或重复运行测试。
- 未覆盖：未启动独立 API/Worker 进程做 HTTP→MQ→Worker 整链路验收；未重跑压测、benchmark 或 pprof；未做真实进程强杀、网络分区、多主机或真实支付渠道验收。未操作现有数据卷，也未对其中旧活动执行 fenced rebuild；隔离夹具通过不能替代保留数据的运维检查。测试覆盖限于当前平台、现有用例和已构造的故障/并发交错。
- 本记录按后续授权独立本地提交；已在 [Issue #1 补充最终回归结果、快照误报与未覆盖项](https://github.com/CsBillChang/EnjoyLife-platform/issues/1#issuecomment-6259890082)，并读回确认评论，Issue 保持开启。原始日志、失败摘要、复核结论和快照继续保留在上述本地证据目录；`record-preservation-manifest.json` 保存关键证据的 SHA-256，提交前后校验内容未变，原始证据未上传 GitHub。
- 本次仅核对并提交验证记录，没有重新运行测试；不改业务代码、正式测试、依赖或目录结构，不扩大重构范围、不清理现有数据卷、不推送代码或创建 Release。用户手动运行、解释与修改状态不变。
