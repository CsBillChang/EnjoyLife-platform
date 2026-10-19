# 量化评测计划

更新日期：2026-09-22。本文固定评测口径与执行记录格式。查询 v3 已按固定 60 秒场景执行；具体参数、所有轮次与无效数据见 [benchmark-report.md](benchmark-report.md)。旧固定请求数/旧采集口径结果保留，但不得与 v3 比较。

## 评测前置条件

- L12 的正确性与恢复验证已完成后才执行 L13 比较；优化后又重跑了全部 internal 真实依赖 race 回归。
- 同一轮只运行本项目的负载；不与其他项目压测并行。
- 运行前记录 Git 提交；若工作区仍无 Git，则记录源码 SHA-256 清单。配置、数据库种子和负载脚本版本必须一并保存。

## 固定指标口径

| 指标 | 定义 | 必须同时报告 |
| --- | --- | --- |
| 店铺查询延迟 P50/P95/P99 | 从客户端发出 `GET /shops/{id}` 到完整读取并关闭响应体的耗时，分别统计全部完成请求和完整 HTTP 200 请求 | load mode、请求数、吞吐、transport/read/close/incomplete、缓存命中、Redis/MySQL 回源次数 |
| 购买入口延迟 P50/P95/P99 | 从发送购买请求到收到 HTTP 响应的耗时；L09 后 202 仅是受理，不是成单 | HTTP 状态分布、受理数、拒绝数、超时数 |
| 异步成单终态耗时 P50/P95/P99 | 成功样本从 `accepted_at` 到 `orders.created_at`；另报首次轮询观察终态的上界 | 队列等待、处理耗时、成功/失败原因、超期与未完成数；不得把 202 或行创建时间称为 commit 时间 |
| 正确性 | 每用户每活动成功订单数不超过 1；库存范围合法；库存流水恒等式成立；每订单最多一次 RELEASE | 每类故障注入次数、观察错误数、SQL 核对原文 |
| 资源 | 容器 CPU、内存、Go goroutine、数据库连接、RabbitMQ 队列深度 | 容器限制、Worker 数、Prefetch、连接池、采样间隔 |

分位数按一轮内预定义样本的 nearest-rank 计算；不能删除慢失败、超时或未完成请求后声称整体延迟改善。FAILED 尚无不可变终态时间字段，单独计数和保存审计证据，不伪造精确失败延迟。

## 数据集与负载（计划值）

| 场景 | 固定种子数据 | 负载计划 | 轮次 |
| --- | --- | --- | --- |
| 店铺冷/热查询 | 10,000 个店铺；1,000 个可查询活动；热点集合为 100 个店铺 | 每轮 60 秒、100 并发；80% 请求命中热点集合，20% 均匀访问其余店铺 | 预热前冷态 1 轮，预热后热态 3 轮 |
| 同步库存竞争 | 单活动库存 100；200 个认证用户 | 200 个并发购买请求；另加同用户重复键和不同键请求 | 每个故障/正常场景 3 轮 |
| 异步成单 | 单活动库存 1,000；2,000 个用户 | 2,000 次购买受理；并发 100；记录从受理到终态 | 正常 3 轮，重复消息与中断场景各 1 轮 |
| 缓存故障 | 与店铺查询数据相同 | Redis 正常、缓存清空、单键过期、Redis 不可用分别执行 | 每场景 1 轮正确性验证；正常场景纳入性能轮次 |

上述数据和并发为计划基线。改变任一数据规模、热点分布、时长、并发或服务配置，必须创建新的场景标识，不能与基线直接比较。

## 基线与优化比较

1. 查询基线：只经 MySQL 的店铺查询实现；比较 Redis Cache-Aside；再比较 Ristretto + Redis + singleflight。三组使用相同种子、负载、容器限制和客户端。
2. 异步基线：在正确性已验证的默认 Worker/Prefetch 配置上采集数据。仅在 pprof 或队列指标证明存在热点后，修改一个候选项（例如连接池、Worker 数或缓存路径）并复测。
3. 每次优化后完整重跑对应正确性 SQL 核对与最相关的竞争/恢复测试。性能变快但不变量失败，结果判为无效。

## 环境记录模板

每个 `artifacts/measurements/<run-id>/environment.json` 至少包含以下字段，严禁记录密码、token 或完整环境变量值：

```json
{
  "run_id": "UTC-timestamp-scenario",
  "started_at_utc": "",
  "source_revision_or_sha256_manifest": "",
  "host_os": "",
  "host_architecture": "",
  "cpu_model_and_cores": "",
  "memory_bytes": 0,
  "go_version": "",
  "docker_version": "",
  "compose_version": "",
  "image_digests": {},
  "service_config_nonsecret": {},
  "container_cpu_memory_limits": {},
  "mysql_version": "",
  "redis_version": "",
  "rabbitmq_version": "",
  "worker_count": 0,
  "prefetch": 0,
  "database_pool": {},
  "load_generator_version": "",
  "seed_data_version": ""
}
```

## 原始产物与报告

```text
artifacts/
  measurements/<run-id>/
    environment.json
    load-config.json
    raw-results.jsonl
    summary.json
    service-metrics.json
    pprof/                 # 仅 L13 且实际采集时存在
    sql-invariants.sql
    sql-invariants-result.txt
    checksums.sha256
  verification/<run-id>/
    scenario.md
    command-output.txt
    fault-injection.log
    database-before.sql
    database-after.sql
```

`docs/benchmark-report.md` 仅可引用上述原始文件，需写明样本范围、失败/超时/未完成数量、所有改动和局限。

## 终止条件

- 每个正常性能场景达到计划的 3 轮；任一轮出现正确性失败、服务崩溃、无法解释的环境变化或超过 5% 未完成请求时，停止比较并标记该轮无效。
- 故障验证在覆盖列出的故障点并完成 SQL 不变量核对后停止；不得靠无限重试制造“成功”。
- 优化只允许在一个已证实瓶颈上进行一次可解释改动；若优化后未改善或波动超过可解释范围，保留负结果并结束该候选项。
- 达到 L14 所需证据后停止负载；不以持续压测替代交付。
