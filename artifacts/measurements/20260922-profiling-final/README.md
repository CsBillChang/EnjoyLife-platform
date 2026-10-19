# pprof 补充验收证据索引

本目录记录 2026-09-22 pprof 补充验收的收口结果，不覆盖较早的 `20260922-final-acceptance/` 快照。

- `results.json`：采样口径、锁判断、分配优化、关闭诊断后的 HTTP 对照和回归结果。
- `source-manifest.sha256`：本轮最终源码、脚本和文档的内容清单。
- `checksums.sha256`：本目录文件及四个直接测量摘要的校验和。

原始 profile 与逐轮结果保留在：

- `../20260922-shop-profiling-baseline-v1/`
- `../20260922-shop-profiling-typed-v1/`
- `../20260922-shop-detail-response-optimization/`
- `../20260922-shop-detail-response-comparison-v1/`

解释与复现命令见 `../../../docs/profiling-report.md`。原始逐请求 JSONL 和运行日志按 `.gitignore` 保留在本机，不将大样本强制纳入版本库。
