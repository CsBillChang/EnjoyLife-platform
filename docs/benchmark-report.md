# 本地量化评测报告

日期：2026-09-22。所有数字来自本机 `darwin/arm64`、Apple M2 8 核、8 GiB 内存及本项目 Docker Compose。本报告不代表云环境、生产容量或线上用户数据。

## 查询：v3 有效对比

v3 使用修复后的采集器：每轮固定 60 秒、100 并发、10 秒预热、10,000 店铺、80% 请求落在 100 个热点店铺。客户端延迟覆盖完整响应体读取与 `Close`；三轮均为 HTTP 200，transport/read/close/incomplete 均为 0。

| 模式 | P95（ms，3 轮） | P95 中位数 | 吞吐（req/s，3 轮） | 吞吐中位数 | 完整样本数（3 轮） |
| --- | --- | ---: | --- | ---: | --- |
| MySQL-only | 15.221 / 33.427 / 41.800 | 33.427 | 16,836.47 / 8,456.83 / 6,323.14 | 8,456.83 | 1,012,417 / 508,801 / 380,041 |
| 两级缓存（Ristretto + Redis） | 4.741 / 5.179 / 9.669 | 5.179 | 41,142.41 / 34,107.53 / 23,683.33 | 34,107.53 | 2,493,324 / 2,102,447 / 1,428,494 |

按三轮中位数，两级缓存的 P95 从 33.427 ms 降至 5.179 ms，降幅 **84.51%**；吞吐从 8,456.83 增至 34,107.53 req/s，为 **4.03 倍**。这只是同一宿主机、同一负载下的对照，不是生产 SLA。

两级缓存测量窗口的应用计数为：`BackendLoad` 从预热后 2,073 增到 13,524，即测量期内 11,451 次后台回源尝试；`L1Hit` 增加 6,024,265；`RefreshError` 24，`RefreshDrop` 20,372。它们说明缓存重建与有界刷新确有发生，不能被解释为“所有请求只回源一次”或全局强一致。

### 保留但不可比较的旧轮次

- 所有旧查询结果均使用旧采集口径：耗时未覆盖完整 body read/close，不能与 v3 比较；原始产物保留。
- v2 fixed-duration 的 mysql warmup/r1/r2 每轮仅约 200 样本，原因是结果通道死锁；均标记无效并保留，不能引用为性能证据。
- 先前 GORM 慢查询日志干扰的计数轮，以及冷态/故障轮，仍可作为各自目的的诊断证据，不纳入 v3 性能中位数。

## 异步成单：活动 203

活动 203 的库存为 500；500 个独立用户以 100 并发提交。结果为 500 × HTTP 202、500 × `SUCCEEDED`，poll errors 为 0。202 仅表示持久化受理，**不**作为成单延迟。

| 指标 | P50 | P95 | P99 | 口径 |
| --- | ---: | ---: | ---: | --- |
| `accepted_at → orders.created_at` | 3.258310 s | 4.096050 s | 4.442061 s | MySQL 行创建时间；不是事务 commit 时间 |
| `accepted_at → first terminal observed` | 3.813779 s | 5.147043 s | 5.235335 s | 轮询首次观察到终态的上界，包含采样延迟 |

订单、库存、Outbox 与队列不变量仍应以对应 SQL/故障实验为准；该表仅报告本次有效的 500 个成功样本。FAILED 的精确终态时间当前没有不可变字段，不能拿可被后续修复更新的 `updated_at` 伪装成失败延迟。

## pprof

旧 40 ms CPU 样本仍保留为无效/不足证据。新实验对 API 的两级缓存店铺详情热读分别采集 CPU、mutex、block、allocs 和 heap；每类使用独立进程、10 秒预热、45 秒负载、100 并发，30 秒 profile 窗口完整落在负载内。

- CPU：30.12 秒墙钟内得到 68.78 CPU 秒样本，已解决“采样窗口和负载是否重叠”的证据问题。
- mutex：累计等待 1,689.16 ms（墙钟的 5.63%）；`singleflight.Do` 累计链路 60.87 ms，占 mutex 样本 3.60%。没有发现值得修改的项目级锁热点。
- block：490.63 秒为多 goroutine 累加等待，86.58% 在 `runtime.selectgo`，主要是 MySQL watcher、net/http、Ristretto 和定时扫描的空闲/保护性等待；不解读为请求瓶颈。
- allocs 定位到店铺详情使用动态 `gin.H` 的响应包装和 JSON map 编码分配。换成 typed struct 后，handler 微基准从 25 降到 20 allocs/op（-20.0%），B/op 从 6,708–6,709 降到 6,292（约 -6.2%）。
- 同条件 heap 轮按完整响应归一化后，进程累计分配约从 2,934.65 降至 2,515.02 B/response（-14.30%），malloc 从 29.34 降至 24.27/response（-17.26%）。强制 GC 前后的存活堆/RSS 不支持内存泄漏结论。

关闭 mutex/block 诊断后，优化前后固定二进制交错运行 3×60 秒：P95 中位 4.902→4.894 ms（-0.16%），吞吐中位 40,415.54→39,826.50 req/s（-1.46%），六轮 13,545,409 个 HTTP 200 且无 transport/read/close/incomplete 错误。轮次方向不一致，因此不声称 P95 或吞吐提升。详细栈、解读与局限见 [profiling-report.md](profiling-report.md)。

## 复现与限制

- 当前工作区没有可解析的 Git `HEAD`，因此本轮不使用虚构提交号。`artifacts/measurements/20260922-final-acceptance/source-manifest.sha256` 以 114 个源码、配置、迁移、脚本和文档文件的 SHA-256 标识本轮源码版本，`checksums.sha256` 再绑定环境、指标、SQL 不变量与关键轮次摘要。
- 查询复现：`BENCHMARK_RUN_PREFIX=<new-id> BENCHMARK_DURATION_SECONDS=60 BENCHMARK_WARMUP_SECONDS=10 BENCHMARK_ROUNDS=3 BENCHMARK_CONCURRENCY=100 scripts/run-shop-benchmark.sh`。压测客户端与服务端共享宿主机，三轮波动应完整报告。
- 异步复现：使用全新活动号执行 `BENCHMARK_ACTIVITY_ID=<fresh-id> BENCHMARK_OUTPUT_DIR=artifacts/measurements/<run-id> scripts/run-async-benchmark.sh`。
- 异步实验要求主代理先准备用户、活动、Redis generation/准入状态，以及 API、MySQL、RabbitMQ 和 worker；本项目支付为本地模拟器。
- Redis 历史代际、丢失数据或代际不明时必须按停写、generation 切换与 MySQL 核对重建，不能直接以旧缓存继续写入。
- 本机容器未设置生产级资源隔离；不得外推为真实商户、真实支付、生产用户规模或零故障保证。

原始结果保存在 `artifacts/measurements/` 和 `artifacts/verification/` 对应 run 目录；未删除无效轮次。详细口径见 [evaluation-plan.md](evaluation-plan.md)。

## 2026-09-22 简历对齐增量说明

本轮新增活动列表/详情缓存、Lua schema 2 时间元数据和支付/核销记录查询，并完成离线 test/race/vet 及真实 MySQL/Redis/RabbitMQ race 回归。这些修改没有更改原店铺详情 `shop.Handler`/`shop.CachedRepository` 性能路径，因此保留原有正式压测证据，不为得到更好数字重跑。原数字只适用于当时 source manifest 绑定的店铺详情路径，不外推为新 coupon 缓存的性能收益。

本轮增量环境、命令、定向故障验收和源码 SHA-256 保存在 `artifacts/verification/20260922-resume-alignment/`。异步指标名称仍为 `accepted_at → orders.created_at`和 `accepted_at → first terminal observed`，不更名成事务 commit 延迟。
