# agentq 2

说一句需求，做到合进主分支。设计和为什么这么小看 `docs/DESIGN.md`，改之前先读。

- Go 标准库，单二进制。模块：`order` 状态、`claude` 会话、`codex` 审计、`repo` git、`todo` 待办、`tell` 消息、`flow` 那条直线，只有 `flow` 和 `main` 能用别的模块。
- 检查：`.agentq/verify`（gofmt、vet、`go test -race ./...`，几秒钟）。端到端用例在 `flow_test.go`，真二进制加假 Claude，不读写真实的 `~`。
- 产品代码不超过 2,600 行（`TestSmall`）。不加队列、名额、门、评测、自我进化。选型和模块化的规范在 `internal/flow/standards.md`，编译进程序，产品经理照它定方案。要加东西先删东西。
- `scripts/release.sh` 打发布包，`install.sh` 装 Release 里的程序，测试在 `scripts/release_test.go`，用本地假 Release；发版步骤在 `docs/RELEASE.md`。
- 两份 README 章节对等；命令表和 `agentq --help`、环境变量和代码、安装命令和 `install.sh` 要对得上，`public_test.go`（`TestPublic`）卡着。没验证过的写没验证。
- 仓库要能公开：除 `docs/specs/`（开发记录，`.gitattributes` 挡在公开快照外）不写个人信息、本机路径、作者的其他项目。
- 真 Claude 只在临时项目里试，`AGENTQ_HOME` 指到临时目录，`AGENTQ_TELEGRAM=/nonexistent` 不发消息。
