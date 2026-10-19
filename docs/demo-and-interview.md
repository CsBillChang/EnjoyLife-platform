# 演示与阶段末面试复盘

## 先讲全局链路

```text
客户端
  ├─ GET /shops/:id → Ristretto L1 → Redis L2 → MySQL
  ├─ GET /shops/:id/activities、GET /activities/:id → 活动 L1/L2 → MySQL
  └─ POST /purchase-requests
       → 登录/活动+用户限流
       → MySQL 持久化 RECEIVED
       → Redis Lua 预占（仅资格，不是订单）
       → MySQL ADMITTED + Outbox
       → RabbitMQ → 有界 Worker
       → MySQL 条件扣库存 + 唯一订单 + DEDUCT + SUCCEEDED
       → 模拟支付 → 核销，或关单/退款 → 唯一 RELEASE
```

MySQL 是交易事实；Redis 可重建，RabbitMQ 可重复。HTTP 202 表示请求已受理，客户端必须按 request_id 查询终态。

## 本地演示

数据库结构统一由 `cmd/migrate` 管理；Compose 只启动 MySQL，不再挂载 SQL 初始化目录。空库首次执行 001–005 并写入迁移账本，再次执行只跳过已记录的迁移。

保留旧数据时，先检查 `schema_migrations`。如果业务表已经存在但账本缺失或为空（例如旧版 Docker 初始化创建的库），迁移命令会在写入前拒绝执行。应先备份并逐项核对实际表、列、索引与 001–005，再按实际已完成的迁移建立明确基线；不能仅凭列已存在就盲目补账，也不要清卷或重复执行 ALTER。已有完整账本的库继续按原方式增量迁移。当前本地库是否属于该场景需实际检查，不能据此自动补账。

本工具从空库启动时，会先在账本写入保留名 `__bootstrap__:` 的初始化检查点，包含 001 的源码哈希；这不是迁移完成记录。当前 001 只含幂等 `CREATE TABLE IF NOT EXISTS`，因此中断后可在源码未变时重跑并保留已有行，完成时原子写入正式记录并删除检查点。源码变化、不再满足幂等前提或检查点与正式记录冲突时仍停止并要求人工核对。旧库没有这个检查点，不能据此自动接管。存在未完成检查点时应使用同一版迁移工具恢复，完成后再考虑回退；后续非幂等 ALTER 的中断不属于自动续跑范围。

同一 MySQL 实例、同一数据库的迁移，从旧库检查到最后记账均在一个固定连接持有数据库专属命名锁，避免其他迁移进程根据过时的空库判断重新插入检查点。命名锁按数据库名隔离，DDL 隐式提交不会释放它，参见 [MySQL GET_LOCK 说明](https://dev.mysql.com/doc/refman/8.4/en/locking-functions.html)。获取锁最多等待 30 秒并支持 context 取消；完成或失败后显式释放，无法确认锁状态时丢弃物理连接，避免锁随连接进入池。互斥依赖迁移进程使用这版工具，升级前应先停止旧迁移进程。已存在的冲突账本仍拒绝自动处理，需备份后人工核对；本修复不自动删除检查点或补写旧库历史。

恢复可靠性专项可运行 `scripts/verify-recovery-reliability.sh`：脚本新建带 tmpfs 数据目录的 MySQL/Redis 临时实例，验证迁移、坏任务隔离、库存任务租约和 Redis 回补后中断重放，退出时只移除自己创建的临时容器。需要 Docker、Go、Python 3；不依赖现有业务库的数据，也不清项目数据卷。直接运行迁移集成测试时，需另设指向测试实例、可创建临时 schema 的 `LOCAL_LIFE_MIGRATION_TEST_DSN`；缺少此变量会跳过该组测试。

迁移故障断言的三组预期失败对照可单独复现（需要 Docker、Go 和 Python 3）：

```bash
# 在仓库根目录运行；证据目录必须尚不存在，每次换一个新名称。
python3 scripts/verify-migration-fault-controls.py \
  --output-dir .git/migration-fault-controls-run-1
```

脚本自行启动 MySQL 8.4 tmpfs 临时容器并设置测试连接，不使用已有业务库。先要求正式代码的七项故障测试全部通过，再用临时 Go `-overlay` 分别去掉物理连接丢弃、绕过获取锁响应注入、跳过重跑工作，要求恰好 5/2/7 项在指定断言处失败；三个子进程预期退出 1，全部匹配时脚本整体退出 0。编译失败、跳过用例、缺失断言或额外失败均不能算成功；源码锚点变化时停止，需先复核对照。源码不落盘修改，临时覆盖层和本次容器在退出时移除，既有容器和数据卷保留。证据目录保存完整 Go JSON 事件、stderr、源码哈希、运行版本和结果摘要；省略 `--output-dir` 可只查看终端摘要。该脚本专门验证七类故障的断言能力，不替代上面的完整恢复专项。

切换包含库存回补租约修复的 Worker 版本时，先停止旧 Worker，再启动新版本，避免旧二进制绕过新增所有权条件。已有 RUNNING 任务可在租约过期后重新领取；本次修复不需要表结构迁移。回退代码也应先停止并等待在途处理结束，不改任务事实、订单或库存流水。

```bash
docker compose -p huixiang-life --profile async up -d
go run ./cmd/migrate
export LOCAL_DEMO_PASSWORD='<choose-a-local-only-password>'
go run ./cmd/seed

# 保留数据/旧代际使用围栏重建；仅首次新活动用 admission-init
go run ./cmd/admission-rebuild --activity-id 4

# 两个终端
go run ./cmd/api
go run ./cmd/worker

# 回归
scripts/verify-stage-a.sh
scripts/verify-lua-purchase.sh
scripts/verify-redis-fail-closed.sh

# 可选：终端会话结束前清除演示密码变量
unset LOCAL_DEMO_PASSWORD

# 可选：重跑 pprof 证据（约数分钟；默认会启用有界诊断采样）
PPROF_RUN_ID=<new-run-id> scripts/run-shop-profiling.sh
```

登录后可用以下路由演示活动展示与记录查询（令牌只在本机 shell 变量中保存）：

```bash
curl http://127.0.0.1:18080/shops/$SHOP_ID/activities
curl http://127.0.0.1:18080/activities/$ACTIVITY_ID
curl -H "Authorization: Bearer $CUSTOMER_TOKEN" 'http://127.0.0.1:18080/me/payment-records?limit=20'
curl -H "Authorization: Bearer $MERCHANT_TOKEN" "http://127.0.0.1:18080/merchant/redemption-records?shop_id=$SHOP_ID&limit=20"
# 商家仅可修改其活动标题；价格、时段、库存和券规则会被拒绝。
curl -X PATCH -H "Authorization: Bearer $MERCHANT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"演示标题"}' http://127.0.0.1:18080/merchant/activities/$ACTIVITY_ID
```

演示结束时对 API 和 Worker 分别按 `Ctrl-C`，等待优雅退出。如需停止依赖但保留数据：

```bash
docker compose -p huixiang-life --profile async stop
# 下次仍用 up -d 启动，不执行 down -v
```

重建演示先停止正常 API/Worker 写入，再执行：

```bash
go run ./cmd/admission-rebuild --activity-id 4
# 若输出或进程状态不确定，使用第一行打印的 task_id 重试：
go run ./cmd/admission-rebuild --task-id <task-id>
```

首次命令先持久化 FROZEN 任务并打印 ID，再续做；MySQL 最后才 OPEN。快照超过 10,000 个已成单用户时明确失败并保持停写，不截断资格。

## 必讲失败场景

- 最后一份库存：MySQL `available_stock > 0` 条件扣减和唯一键是最终防线，Redis 库存只是入口提示。
- 提交后 Ack 前崩溃：消息会重投；消费者用 request_id、订单唯一键和终态重查返回相同结果。
- Redis 预占后崩溃：请求已先写 MySQL，恢复扫描按 reservation_state 推进或补偿。
- 支付与关单、退款与核销：锁同一订单后按状态条件转移，只有一方成功；RELEASE 有唯一约束。
- Redis 重建：MySQL FROZEN→BUILDING→VERIFIED→OPEN，先发布完整 Redis 新代际，最后开放 MySQL；旧释放只能改旧 key。旧 generation 中无订单的 RECEIVED/ADMITTED 请求安全失败，全部历史成单用户仍保留限购。
- Lua 时间边界：schema 2 元数据要求 `start <= accepted_at < end`，脚本用 Redis TIME 判断 `now < processing_deadline`；六个 key 的元数据、类型或 sentinel 任一异常即 fail-closed，不能由 Go 侧时间检查绕过。
- 活动缓存失效：标题更新与 recovery task 同一 MySQL 事务提交，dispatcher 用 lease、退避和 poison 隔离删 L1/L2；没有 Pub/Sub，其他 API 实例的 L1 最迟由固定 TTL 收敛。
- 关单/退款库存回补：MySQL 事务先写唯一 RELEASE 和持久恢复任务；Worker 再用 activity generation + order ID 的 Lua 回补 Redis 库存提示，保留用户 `S:` 标记，关单/退款不恢复购买资格。
- 支付响应丢失：数据库只存券码 hash；用户可调用 `POST /orders/:id/voucher/reissue`。服务在订单/券行锁下替换 hash，旧码立即失效。

## 面试追问

1. 为什么 202 不能算下单成功？因为它只证明请求与准入流程已持久化，Worker 仍可能遇到售罄、过期或代际变化；订单事务提交才是成交事实。
2. 为什么有 Redis 还需要 MySQL 条件扣减？缓存会丢失、过期或被旧代际写入；只有 MySQL 事务、唯一约束和流水能定义最终库存与订单。
3. Outbox 解决什么、没解决什么？它消除“数据库提交但消息未记录”的双写窗口；发布仍可能重复，所以需要 confirm、租约恢复和幂等消费。
4. 为什么重建不能直接把旧预占逐个 INCR？迟到补偿会跨代际污染新库存；应停写、从 MySQL 权威订单/库存构造完整新代际，再用 generation 隔离旧操作。当前安全策略不迁移无订单旧请求，而是将其失败，避免把未知预占当事实。
5. 为什么活动商家只能改标题？已受理请求固定了价格、accepted_at、processing_deadline 和 generation；更改交易条件若不同步 MySQL 与 Redis schema 2 会破坏原子准入语义，因此接口显式拒绝这些字段。
6. 支付/核销记录怎样防越权？支付查询以认证用户过滤，商户核销记录必须先验证 shop membership；游标仅承载排序边界，不能决定归属。
7. 性能数字怎样保证没偷换口径？相同机器、数据、并发、预热和 60 秒轮次，客户端计时到 body 读完并 close，报告全部轮次及 transport/read/close/incomplete。异步分开 `accepted_at→orders.created_at` 和首次观察终态上界，两者都不写成精确 commit 时间，更不用 202 RTT 代替。已有数字只覆盖店铺详情旧版本路径，不能外推活动缓存。
8. pprof 找到了什么，为什么没有做锁优化？新轮次确认采样 PID 和 30 秒窗口完整落在负载内；mutex 中 `singleflight.Do` 累计链路只占 3.60%，没有显著项目锁热点，所以未为了简历强行改锁。allocs 则定位到详情响应动态 map，换 typed struct 后 handler 从 25 降到 20 allocs/op，应用归一化分配也下降；但关闭诊断后的 3×60 秒对照没有稳定 P95/吞吐提升，因此只讲分配收益。

## 亲手修改练习

将 `WORKER_COUNT` 从 4 改为 1，在全新活动编号上重跑 `scripts/run-async-benchmark.sh`，比较 P95 与队列等待，并解释为什么订单唯一性和库存不变量不应随 Worker 数变化。用户尚未完成此练习；这不影响工程验收，但影响“已掌握”的判断。
