---
paths:
  - "**/*.go"
---
# Go
- 有网络、数据库、子进程调用的函数，第一个参数收 `ctx context.Context` 并一路往下传；`context.Background()` 只在入口和测试里创建。
- 错误用 `fmt.Errorf("做什么: %w", err)` 带上下文返回，不用 `_` 丢掉 error。
- 打开文件、连接、锁、`resp.Body` 后紧跟 `defer` 释放。HTTP 请求用设了 `Timeout` 的 Client 或带超时的 ctx。
- 每个 goroutine 都有退出路径；共享可变状态用 mutex 保护，或只归一个 goroutine 所有。
- 交付前：`gofmt -l .` 无输出、`go vet ./...`、`go test -race ./...`。
