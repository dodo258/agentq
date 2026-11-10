# 支持矩阵

[English](support-matrix.en.md)

每一项验证到了哪一步，如实写。「假 CLI」指用假的 claude 跑，没消耗模型额度。「真机」指 macOS、Claude Code 2.1.316、订阅登录、真实模型。

后端有 Claude CLI、Codex CLI 和 opencode（实验，真实的没跑过）。接别的 CLI 的接口见 [docs/backends.md](../backends.md)；`agentq doctor` 打出当前后端和 `project_backends` 用到的后端的能力表。

| 平台 | 状态 | 验证到哪一步 |
| --- | --- | --- |
| macOS arm64（launchd） | 主要平台 | 真实 launchd、真实 Claude CLI 跑过任务 |
| macOS amd64 | 能构建 | 交叉编译，没有在 Intel Mac 上跑过 |
| Linux amd64（`systemd --user`） | CI 验证 | GitHub Actions 的 ubuntu-latest：全部测试，加真实用户服务的安装、启停、任务存活、升级、回滚、卸载、恢复。用假 claude，没跑过真实模型任务，没有长期运行 |
| Linux arm64 | 能构建 | 交叉编译 |
| WSL | 未验证 | 不声明支持 |
| Windows 原生 | 不支持 | |

| 能力 | 新安装默认 | 验证到哪一步 |
| --- | --- | --- |
| 一键安装 `install.sh`、回车启动、`init`、`add` | 手动运行一次 | 真机：官方 CLI 的安装和升级（老用户环境），配置逐字节不变，已有同名技能保留。GitHub Release：v0.1.0 已发布，macOS 和 Linux 的工作流都过了，匿名下载 `install.sh` 在临时 HOME 里一键安装成功（没有终端时只安装不启动）。其余路径用假 CLI、假服务管理器、本地假 Release 走通，CI 上跑过 Linux 的终端和 shell 配置用例。新用户环境（没有 CLI、没有登录）下脚本代装和登录不在这次真实验证的范围内 |
| Claude 桌面端派活 | 手动 | 真机：桌面端会话用 `dispatch` 写条目并启动，走到合并 |
| 后端选择 `backend`、`project_backends`、`backends`，`install.sh --backend`，能力降级 | `claude` | 假后端，覆盖每条降级路径；`install.sh --backend` 用假 CLI 和本地假 Release 走通 |
| 混用后端时撞限冷却和额度按后端分（`backend_limits`） | 只用一种后端时不写这个键 | 假 CLI 和暴力测试。真实两个后端同时跑、真实撞限没跑过 |
| Codex 后端 | 手动选 | codex-cli 0.166.0、macOS arm64、ChatGPT 登录、临时项目：真实跑通启动、分支提交、`ready.json`、测试门、审查门、引擎合并，SIGTERM 后同一会话续跑，走到合并。撞限只用按官方文字拼的样本测过，令牌失效没有真实样本（只测过没有凭据的 401），大方向商量和巡逻分析在 Codex 上不启动。Codex 桌面端派活没有真实验证。Linux 上只跑过假 CLI。后台服务（launchd）下的 ChatGPT 令牌刷新没验证 |
| opencode 后端（实验） | 手动选 | **真实的 opencode 没跑过**：本机没装，按 v1.19.5 的官方文档和源码写，假 CLI 和暴力测试（真二进制）走通启动、分支提交、`ready.json`、测试门、审查门、引擎合并、撞限续跑。事件样本是手写的。审查结论靠提示词要 JSON。只读、隔离 MCP 没验证，要它们的任务（追问、巡逻分析、大方向商量）不启动。登录查不了，第一次认证失败才暂停。官方记过丢最后 `step_finish` 的问题：丢了就当没有终态，不算成功。各模型来源的 API Key 环境变量不传，用 `opencode auth login` |
| 调度、并发、中断续跑、用量和额度、熔断 | 暂停，`agentq start` 打开 | macOS arm64 真实运行。真机：`agentq start` 装 launchd 服务，定时器和任务结束后的 tick 自己走到合并。真实撞限、令牌失效没跑过，这些故障路径用假 CLI 和暴力测试覆盖 |
| 排除项目 `exclude_projects` | 空 | 假 CLI |
| 外面的项目 `extra_projects`、第一次 `add` 自动接入 | 空，`add` 时登记 | 假 CLI，走到 tick 接任务 |
| 通用工程规则 `agentq env` | 第一次同意时装，已有全局规则不装 | 假 CLI，看文件有没有装对。真实 CLI 读到规则是桌面端手动确认的结论，这里没有自动验收 |
| 生成 `.agentq/verify` | 第一次 `init`、`add` 时写 | 假工具真跑生成的脚本，五种项目各一个夹具。真实的 npm、cargo、uv、poetry、swift 没跑过 |
| 模型跟随、按活分配模型 | 新配置留空 | 假 CLI 看 argv。真实 CLI 不带这两个参数时用哪个模型没有自动验收 |
| 磁盘清理（`launchd.log`、超过 90 天的任务记录） | 开，90 天 | 假数据，包括时钟跳变和软链接 |
| `agentq new` | 手动可用 | 真机：一句话建新项目，过测试门、成品门、审查门，合并，生成交付单。另有假 CLI 和暴力测试 |
| 质量门和引擎合并 | 开，测试门和审查门都是 `error`；补丁门、界面门、成品门是 `warn` | 真机：普通任务到分支，测试门、审查门（真实 Claude）通过，引擎合并，BACKLOG 关闭。测试门把修复还原后再跑，确认新测试失败。界面门用桌面和手机两张真实截图，按 `no-ai-look` 审查通过，截图脚本是任务自己新增的。矛盾的需求任务不硬做，转交用户决定，不合并。另有假 CLI 和暴力测试（真二进制）。Codex 上的审查见 Codex 那一行，opencode 上没跑过。界面门的 Playwright、`simctl` 脚本示例只查过语法，没有在真实项目上跑过；阈值没有用真实项目校准。门和裸 CLI 在真实项目上的对比没有做过 |
| 大方向商量 | 开 | 真机加真实 Telegram：选了方向后写进条目并开工 |
| 夜班、复盘 | 关 | 假 CLI 和暴力测试 |
| 巡逻 | 关 | 假 CLI 和暴力测试 |
| 本机通知、任务结果通知 | 一键脚本里同意启动时开；`agentq start` 不开 | Telegram 的完成推送真实走通。其余用假 CLI 和暴力测试，真实桌面弹窗没验证 |
| 默认分支自动识别 | 开 | 假 CLI 和暴力测试（真 git 仓库），Linux CI |
| Telegram 收发 | 关，可选 | 基本收发真实用过。回复链、完整对话往返只用假服务测过 |
| 决策卡片按钮 | 随 Telegram | 真实 Telegram 上按钮回调走通 |

暴力测试（杀进程、写坏状态文件、并发、磁盘写满、时钟跳变）在 macOS 上跑过，Linux 可以手动触发但还没跑过，依赖全部是假的，方法见 [CONTRIBUTING](../../CONTRIBUTING.md#暴力测试)。

还没有真实验证的：Codex 桌面端派活；真实撞限、令牌失效；Linux 上的真实任务和暴力测试（Linux 只有 CI 的测试和真实 systemd 验收）；opencode 的任何真实运行；门和裸 CLI 在真实项目上的对比。
