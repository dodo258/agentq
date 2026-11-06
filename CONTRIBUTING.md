# 参与开发

欢迎提 Issue、PR 和真实使用反馈。提 Issue 请写系统版本、架构、复现步骤、期望结果和实际结果。日志请先删掉用户名、路径、token、聊天内容和项目私有信息。

## 项目结构

Go 标准库，单二进制，不加第三方 Go 依赖。

| 位置 | 内容 |
| --- | --- |
| `main.go`、`assets.go` | 入口和编译进二进制的资产 |
| `internal/<模块>/` | 调度器，模块表和依赖方向见 `AGENTS.md`，新模块先改 `tests/arch_test.go` 的表。平台差异只在 `*_darwin.go`、`*_linux.go`（服务管理在 `internal/service/`，systemd 那部分不带标签） |
| `tests/` | 跑真二进制和跨模块的行为用例、门禁，黄金文件在 `tests/testdata/`。模块自己的用例在模块目录，根目录只留 `assets_test.go`（查编译进二进制的资产和 `tests/` 读到的一样） |
| `skills/` | 自有技能唯一源，已安装的是生成物，不要反向改 |
| `templates/` | 五个项目模板 |
| `support/scripts/` | 随安装分发的支持脚本 |
| `prompts/` | 夜班和复盘入口，编译进二进制 |
| `dependencies/claude.lock.json` | 第三方来源、commit、许可证和路径声明 |
| `config/clean-source.json` | 清洁源边界 |

加一道质量门：实现 `gate.Gate`，在 `internal/gate/gate.go` 的 `init` 里 `Register`（名字、默认档位、构造函数），按登记顺序跑，流程不用动。新门先 `warn`；能写成命令和退出码的才有阻塞权，模型判断要带证据。用例照 `internal/gate/testgate_test.go`，在进程内走 `Service.Process`。任务合同（`skills/agent-task`）只写「任务要交什么」，门的规则在引擎里。

## 开发命令

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
```

Go 版本见 `go.mod`。Go 1.26.3 在 darwin/arm64 上的 `-race` 运行时有偶发的 fork 段错误（[golang/go#79804](https://github.com/golang/go/issues/79804)），修复在 1.26.5。遇到时换新工具链：

```sh
GOTOOLCHAIN=go1.27.3 go test -race ./...
```

Linux 路径在 macOS 上也能编译检查：`GOOS=linux go vet ./...`。

## 测试规则

默认测试只用临时 HOME、临时 `projects_root`、假 claude、假 launchctl、假 systemctl、假 Telegram，不读写真实的 `~/.agentq`、`~/projects`、`~/.claude`，也不碰真实服务。新加的测试要守这一条。

手动跑二进制时也设临时 HOME：

```sh
go build -o /tmp/agentq . && HOME=$(mktemp -d) /tmp/agentq status
```

注意 `agentq status` 会调用真实的 `claude -p /usage`（本地命令，不花 token）。

修 bug 尽量写一个撤掉修复就会失败的测试。不要为了通过测试去硬编码、放宽超时或删测试。agentq 自己调度的任务里，测试门会查这些。

以下测试默认跳过，要显式打开：

| 测试 | 命令 | 说明 |
| --- | --- | --- |
| 网络入口 | `AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...` | 联网取锁定依赖 |
| 真实 CLI | `AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...` | 消耗模型额度。连接型身份另设 `AGENTQ_REAL_E2E_CONNECTION=1` |
| 真实 systemd | `TestSystemdAcceptance` | 只在 CI 的 Linux runner 上跑，动真实用户服务，没有 `GITHUB_ACTIONS` 就拒绝。见 `.github/workflows/ci.yml` |

## 暴力测试

`AGENTQ_STRESS=1` 打开，默认 `go test` 跳过。它真的起子进程：编译好的 agentq（带 `-race`）和假 claude，全部用临时 HOME，每轮随机破坏（杀进程、写坏状态文件、并发抢锁、Telegram 故障、磁盘写满、时钟跳变等），再断言不变量：没有 panic、`fatal error`、`DATA RACE`；条目不丢不重复；同条目一个活进程；并发不超；结束后没有进程残留；状态和配置能解析；私钥和 bot token 不出现在文件和消息里。

```sh
AGENTQ_STRESS=1 AGENTQ_STRESS_SEED=201 AGENTQ_STRESS_ROUNDS=40 \
  GOTOOLCHAIN=go1.27.3 go test -race -count=1 -run '^TestStress$' -timeout 3h -v ./tests
```

| 变量 | 作用 |
| --- | --- |
| `AGENTQ_STRESS_SEED` | 种子。同一种子同一轮的破坏序列相同，进程调度的时序不同，时序类问题不保证一次复现 |
| `AGENTQ_STRESS_ROUNDS` | 轮数 |
| `AGENTQ_STRESS_ONLY=<场景>` | 只跑一个场景，如 `kill`、`killstart`、`modes`、`concurrent`、`corrupt`、`backlog`、`messages`、`env`、`diskfull`、`timelapse`、`release`、`features`、`cards`、`gate`、`gatequeue`、`notdoing`、`stats`、`configrace`、`stoplock`、`corruptboth`、`longrun` |
| `AGENTQ_STRESS_KIND=<类型>` | 指定 `env` 场景的类型 |
| `AGENTQ_STRESS_KEEP=1` | 留现场（失败时总是留） |
| `AGENTQ_STRESS_NORACE=1` | 编译不带 `-race` 的 agentq |

每个找到的问题都要在 `tests/stress_regress_test.go` 加回归用例，平时的 `go test` 就跑。暴力测试平时在 macOS 跑。GitHub Actions 手动运行 `ci` 时勾选 `stress` 会在 Linux 跑一次（1 个种子，20 轮），磁盘写满那个场景要 `hdiutil`，在 Linux 上跳过。

用户能感知的变化写进 `CHANGELOG.md`「未发布」一节。

## 提交要求

- 一个提交只做一件事。提交前看 `git status` 和 `git diff --cached`，只提交本任务的文件。
- 提交说明正文写清跑了哪些验证、结果如何、哪些没跑。
- 不用 `--no-verify` 跳过钩子。
- 改了用户可见的行为，同一个提交更新 `docs/reference/`。
- 传给 Claude 的环境变量只走 `internal/proc` 的 `ChildEnvKeep` 白名单，新加变量要说明必要性。
- 状态文件读不了当未知：不收尾、不启动，只记日志。

## 隐私门禁

仓库不放个人信息。`config/clean-source.json` 定义边界，`scripts/privacy_scan.py` 扫描，`go test` 内含门禁。提交被拦下时改内容，不要改门禁。例子里的项目名用 `demo`、`my-app` 这样的虚构名字。

```sh
python3 -I scripts/privacy_scan.py --manifest config/clean-source.json
```

个人规则放在仓库外的 `~/.agentq/private/privacy-patterns.json`（权限 600），缺失或损坏时扫描失败关闭。格式和运行方式见 [docs/privacy.md](docs/privacy.md)。启用钩子：`git config core.hooksPath scripts`，已有钩子时先比较并串联。

## 写文档

- 文档面向陌生读者：用「你」，不写开发过程和内部事故。
- 新数字和行为以代码为准，写进 `docs/reference/`，操作步骤写进 `docs/guide/`。
- 中文文档不用破折号，BACKLOG 语法示例放进代码块。

## 维护合同

代理可以在任务 worktree 改源码、验证、构建候选。升级用同一安装引擎，不就地覆盖运行中的程序，不丢状态、身份和用户配置，不借维护绕过权限、认证、发布或远程授权。活跃任务、未知状态、冲突时停下并保留材料。完整约定见 [AGENTS.md](AGENTS.md)。
