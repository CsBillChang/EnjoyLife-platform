# pprof 锁竞争与内存开销报告

日期：2026-09-22。对象为本项目 API 进程的两级缓存店铺详情热读链路。所有结果均为 Apple M2 / 8 GiB 宿主机上的本地实验，不代表生产容量。

## 1. 采样对象与有效性

诊断开关 `PPROF_MUTEX_FRACTION` 和 `PPROF_BLOCK_RATE` 已接入 API，正常启动默认均为 0；正式采样轮显式使用 mutex fraction 1 和 block rate 1,000,000 ns。pprof 只绑定 `127.0.0.1:18081`。

基线运行 `20260922-shop-profiling-baseline-v1` 对 CPU、mutex、block、allocs 和 heap 分别启动新 API 进程；每轮 10 秒预热、45 秒负载、100 并发，profile 为负载中间的 30 秒差分窗口。脚本保存了：

- API PID/PPID、启动时间、脱敏启动配置、二进制 SHA-256 与 `go version -m`。
- PID 同时拥有 HTTP 与 pprof 监听端口的 `lsof` 证据。
- 预热、负载与 profile 起止 UTC 时间线；每个 profile 窗口均完整落在对应负载窗口内。
- 每轮完整响应摘要和逐请求 JSONL。五轮合计数百万请求均为 HTTP 200，transport/read/close/incomplete 均为 0。

旧 40 ms CPU profile 仍保留，但不参与结论。新 CPU profile 在 30.12 秒墙钟窗口内得到 68.78 CPU 秒样本，已证明进程和负载重叠。

## 2. CPU 与锁竞争

CPU 样本中 `syscall.rawsyscalln` 占 71.65%，读写 socket 是主要累计路径；项目 `shop.Handler.getByID` 累计占 1.44%。这是网络密集本地热读的观测，不是生产 CPU 上限。

mutex 的 30.01 秒差分 profile 累计 1,689.16 ms 等待，相当于墙钟的 5.63%：

| 路径 | 累计等待 | 占 mutex 样本 | 判断 |
| --- | ---: | ---: | --- |
| runtime/sync 通用 unlock | 约 1,596.6 ms | 94.52% | 主要是 runtime、`sync.Pool` 与 net/http 通用路径 |
| `singleflight.Group.Do` 累计链路 | 60.87 ms | 3.60% | 包含回源函数执行；其 `g.mu.Unlock` 定位行只有约 10.6 ms |
| `CachedRepository.FindByID` | 17.94 ms | 1.06% | 热读链路不构成显著锁瓶颈 |

mutex profile 把时间归因到持锁者释放位置，不是 MySQL 行锁指标。当前没有证据支持删除 singleflight、更换 Ristretto 或调整数据库连接池。

block profile 的 490.63 秒是多 goroutine 累加等待，可以超过 30 秒墙钟：`runtime.selectgo` 占 86.58%，主要来自 MySQL watcher、net/http、定时失效扫描和 pprof 自身；`sync.Cond.Wait` 占 12.86%，主要是 HTTP 连接收尾。Ristretto 两个后台 worker 各等待约 30 秒，是空闲/保护性等待，不是请求卡死。

**锁竞争结论：** 完成了有效采样和项目栈定位，但未发现值得修改的业务锁热点，因此没有实施“锁优化”，也不声称锁等待收益。

## 3. 内存分配定位

基线 allocs 差分窗口累计 3,269.41 MiB / 32,613,074 个对象。主要来自 net/http 请求解析、Header clone 和 Gin/JSON 输出。可由项目直接控制的最明确热点是：

- `shop.Handler.getByID` 在 `c.JSON(http.StatusOK, gin.H{"data": result})` 一行直接分配 420.09 MiB。
- `encoding/json.mapEncoder.encode` 为单键动态 map 的反射、键提取与排序直接分配 52.50 MiB。
- `shopCacheKey` 累计只占 0.87% alloc_space，本轮不优化。

同一进程在完整 45 秒 heap 轮前后使用相同 `gc=1` 条件：1,952,996 个完整响应期间增加 5,731,352,968 字节累计分配和 57,297,176 次 malloc，约为 2,934.65 B/response 和 29.34 mallocs/response。强制 GC 后 HeapAlloc 只增加 328,552 字节，HeapObjects 增加 906，RSS 从 40,816 KiB 降至 38,080 KiB。这些结果不支持“内存泄漏”结论。

## 4. 证据驱动的优化

只将店铺详情成功响应的动态 `gin.H` 换为字段固定的 `shopDetailResponse` 结构体；仍由 `c.JSON` 编码，状态码、Content-Type、`data` 外层和 `Shop` 字段契约不变。

五轮 handler 微基准均使用完整 Gin 路由和 `httptest` 响应：

| 指标 | 优化前 | 优化后 | 变化 |
| --- | ---: | ---: | ---: |
| allocs/op | 25 | 20 | -5（-20.0%） |
| B/op | 6,708–6,709 | 6,292 | -416–417（约 -6.2%） |
| ns/op 中位 | 2,089 | 1,859 | -11.0%（仅微基准） |

优化后同参数 allocs profile 中已找不到 `encoding/json.mapEncoder.encode`。完整 heap 轮按完成响应归一化后：

- 累计分配从 2,934.65 降至 2,515.02 B/response，约 -14.30%。
- malloc 从 29.34 降至 24.27/response，约 -17.26%。
- 强制 GC 后 HeapAlloc 只增加 193,488 字节，HeapObjects 增加 828；仍不声称修复内存泄漏或减少 RSS。

进程累计分配包含 HTTP、GC、pprof 和后台扫描，所以上述应用级比率是本地该负载下的近似值；微基准和 mapEncoder 栈消失是更直接的因果证据。

## 5. 关闭诊断开关后的正式对照

优化前后两个固定二进制显式使用 mutex/block=0，在 10 秒预热、100 并发、3×60 秒负载下交错运行。六轮共 13,545,409 个完整 HTTP 200，所有 transport/read/close/incomplete 均为 0。

| 版本 | P95 三轮（ms） | P95 中位 | 吞吐三轮（req/s） | 吞吐中位 |
| --- | --- | ---: | --- | ---: |
| 动态 map 基线 | 8.946 / 4.902 / 4.017 | 4.902 ms | 24,847.08 / 40,415.54 / 46,361.10 | 40,415.54 |
| typed response | 7.161 / 4.894 / 4.676 | 4.894 ms | 30,548.38 / 39,826.50 / 40,766.64 | 39,826.50 |

P95 中位变化 -0.16%，吞吐中位变化 -1.46%，且单轮方向不一致。因此只接受“减少分配”结论，不声称提升 HTTP P95 或吞吐。

## 6. 产物与复现

- 基线 profile：`artifacts/measurements/20260922-shop-profiling-baseline-v1/`
- 优化后 allocs/heap：`artifacts/measurements/20260922-shop-profiling-typed-v1/`
- 微基准：`artifacts/measurements/20260922-shop-detail-response-optimization/`
- 关闭诊断的正式对照：`artifacts/measurements/20260922-shop-detail-response-comparison-v1/`

采集命令：

```bash
PPROF_RUN_ID=<new-run-id> scripts/run-shop-profiling.sh

# 只重测受影响的分配和存活堆
PPROF_RUN_ID=<new-run-id> PPROF_PROFILE_KINDS='allocs heap' \
  scripts/run-shop-profiling.sh
```

当前 Go 发行版未提供可直接调用的 `go tool pprof`，本轮使用同一 GOROOT 源码离线构建分析器：

```bash
cd /private/tmp
GO111MODULE=off GOPROXY=off go build -o huixiang-pprof-tool "$(go env GOROOT)/src/cmd/pprof"
```

报告中每个 top/list 结果、原始 protobuf profile、请求摘要、时间线、源码清单和校验和均在对应目录保留。
