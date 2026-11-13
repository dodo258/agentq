<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/logo-dark.svg">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/logo-light.svg" width="96" alt="agentq">
</picture>

# agentq

agentq 是 Claude Code 的后台任务调度器：它把各项目 `docs/BACKLOG.md` 里的待做条目交给独立的 Claude Code 进程实现，测试和审查通过后合并到本地默认分支。

[![License](https://img.shields.io/badge/license-MIT-16181D)](LICENSE) ![Go](https://img.shields.io/badge/Go-16181D?logo=go&logoColor=white) ![macOS](https://img.shields.io/badge/macOS-16181D?logo=apple&logoColor=white) ![Linux](https://img.shields.io/badge/Linux-16181D?logo=linux&logoColor=white) ![Claude Code](https://img.shields.io/badge/Claude_Code-16181D?logo=claude&logoColor=white)
[![Release](https://img.shields.io/github/v/release/dodo258/agentq?label=release&color=16181D)](https://github.com/dodo258/agentq/releases/latest) [![Stars](https://img.shields.io/github/stars/dodo258/agentq?style=flat&color=16181D)](https://github.com/dodo258/agentq/stargazers)

**[安装](#安装)** &nbsp;·&nbsp; **[入门指南](docs/guide/getting-started.md)** &nbsp;·&nbsp; **[命令](docs/reference/commands.md)** &nbsp;·&nbsp; **[安全](SECURITY.md)** &nbsp;·&nbsp; **简体中文** · [English](README.en.md)

<br>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/hero-dark.png">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/hero-light.png" width="900" alt="一键安装，装完就在运行；agentq add 添加一条任务；agentq status 显示调度、额度和运行中的任务；通过质量门后合并">
</picture>

</div>

<br>

## 安装

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

脚本下载预编译程序并校验 SHA256，不需要 sudo 和 Go，只依赖 Git。已安装时重跑同一条命令就是升级，配置和任务数据保留。


装好就在运行，不再问你：安装本身就是同意。安装结束时程序如实告诉你三件事和暂停、卸载命令：

- 后台任务能读写当前系统用户的所有文件、运行命令。
- 任务内容发给你使用的模型服务，消耗你自己的额度。
- 没有待做时不消耗额度。

想先不跑，安装时加 `--paused`（`curl ... | sh -s -- --paused`）。已有安装重跑这条命令是升级，保留你原来的开关，暂停着的不会被打开。

如果没有 Claude Code CLI 或没有登录，有终端时脚本运行官方安装命令和 `claude auth login`；没有终端时只给命令，弄好后在聊天里说需求，后台会自己开起来。桌面端不能跑后台任务，只装了桌面端也需要 CLI。

安装内容包括程序、任务流程和审查用的技能、项目模板和一份通用工程规则，已有配置不修改。网络不通时加 `--proxy <地址>` 或 `--mirror <前缀>`，用镜像时校验文件和源码仍从原地址取。卸载用 `agentq remove --yes`，数据保留。其他安装方式和参数见[入门指南](docs/guide/getting-started.md#其他安装方式)。

由 agent 代装时，把这句话发给它：

> 读 `https://github.com/dodo258/agentq/blob/main/llms.txt`，帮我装 agentq

## 用法

装好，开口说。打开 Claude Code 或 Codex（CLI、桌面端都行），在项目目录里直接说要做什么：

```text
导出失败时重试 3 次，断网时要能看到重试日志
```

不用说「派活」，也不用敲 agentq 命令。`dispatch` 技能自己判断：加功能、修 bug、改界面，哪怕很小，都写成带验收标准的条目，提交到 `docs/BACKLOG.md` 交给后台，做完通知你；只是问问、改错字改文案这类零碎改动、你说「直接改」「我看着」的，当场做。

- 目录不在 `~/projects` 下、不是 Git 仓库、没有 BACKLOG，都不用先处理：技能自己 `git init`（现有文件做初始提交，`.gitignore` 挡住密钥和依赖）、补文件、登记到 `extra_projects`。在家目录、桌面这类不像项目的地方说需求，它会问一句项目叫什么、放哪，带默认答案。
- 后台没在跑（被卸、重启后没加载）时技能自己拉起来；你自己暂停的不会替你打开。
- 进度、验收、停也在聊天里说：「做得怎样」「做完的怎么看」「先停下」。

也可以在终端操作：

```sh
agentq add "..."                  # 往当前项目的「待做」加一条，项目第一次添加时自动接入
agentq run my-app                 # 立刻启动该项目的一条待做
agentq new "..."                  # 按一句话新建项目并做出第一版
agentq discuss                    # 查看开工前等你回答的大方向问题

agentq status                     # 查看额度、运行中的任务、各项目待做和最近结束的任务
agentq logs 1105-211629-shop-api  # 查看一个任务的结果全文
agentq stop 1105-211629-shop-api  # 停止一个任务，已有改动保留
agentq pause                      # 暂停启动新任务，agentq resume 恢复
agentq doctor                     # 只读检查安装、CLI 和配置
agentq update                     # 升级到最新版
```

日常操作见[日常使用](docs/guide/daily-ops.md)，所有命令见[命令参考](docs/reference/commands.md)。

## 工作流程

agentq 负责选择条目、启动和监控进程、运行质量门、合并和记录，不写业务代码。实现和自测由 Claude Code 按随附的 `agent-task` 技能完成，审查由另一个 Claude Code 进程完成。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/flow-zh-dark.svg">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/flow-zh-light.svg" width="900" alt="你说一句，写进 docs/BACKLOG.md；agentq tick 检查后启动独立的 claude -p 进程，在独立 worktree 里实现、自测，写 ready.json；agentq 运行质量门（测试、审查），通过后由引擎合并并收尾；未通过时退回任务接着改，仍未通过就等你决定；中断或撞上限时用同一会话续跑；结果写进当天汇总和早报">
</picture>

调度器每 5 分钟运行一次，任务结束时也立刻运行一次，所以排队的条目几秒内开始。默认全局同时运行 2 个任务；同一项目里写了 `（范围：…）`、范围不重叠又不互相依赖的条目也能同时跑 2 个，没写范围的一次一条。任务不依赖终端或客户端，机器要保持开机。

调度器不执行 `git push`，合并只发生在本地默认分支，推送远程要另外授权。完整规则见[调度规则](docs/reference/scheduling.md)。

## 质量门

任务只能把分支做到可合并，并写一份 `.agentq/ready.json`。是否合并由 agentq 按下面五道质量门判定，门都在分支和最新默认分支合并后的代码上运行。

| 质量门 | 什么时候运行 | 检查内容 | 默认档位 |
| :-- | :-- | :-- | :-- |
| **测试门** | 每次 | 运行项目的 `.agentq/verify` | 挡合并 |
| **审查门** | 只改 markdown 文档时跳过 | 独立进程审查改动，阻塞项要有文件、行号、证据和复现步骤 | 挡合并 |
| **补丁门** | 改动类型是修复时 | 同一模块 30 天内第三次修复。调成 `error` 后退回任务，要求重写该模块 | 只记录 |
| **界面门** | 改到界面文件时 | 项目脚本截图，agentq 检查不是空白，审查进程只对渲染坏、溢出、看不清和占位内容阻塞 | 只记录 |
| **成品门** | `agentq new` 的第一版和标了 `（成品）` 的条目 | 运行项目的烟测 `.agentq/smoke`，通过后生成交付单 | 只记录 |

项目可以在 `.agentq/gates.json` 调整档位。第一次 `agentq init` 或 `agentq add` 时，程序按项目文件生成 `.agentq/verify`；判断不了时，任务要在实现时补上。

质量门未通过时，原因退回任务，同一会话接着修改，最多 2 轮。仍未通过或无法判定（比如脚本说这次跑不了）时，条目标记为卡住，等你决定：

```sh
agentq gate report <任务id>                # 查看原因
agentq gate pass <任务id> --reason "..."   # 放行这一次
agentq gate rerun <任务id>                 # 不改代码重新运行质量门
agentq gate abandon <任务id>               # 放弃，分支保留
```

任务绕过质量门自己合并的不算通过，agentq 会标出来并通知你。质量门针对的是模型偷懒和误操作，不是安全边界：任务在你的机器上有完整权限。

设计、界面和技术选型这类方向问题，在开工前问一次：最多 3 个问题，每个带默认答案。在 Claude Code 会话里派活时当场问。条目标了 `（大方向）` 或使用 `agentq new` 时，后台通过 Telegram 卡片问，2 小时没有回答就按默认开工，其他条目不等待；没配 Telegram 时直接按默认。

`agentq health` 显示每个模块最近的改动次数、修复次数和两周返工率，`agentq stats` 显示每道门的拦截和放行次数，以及按提交记录计算的一次满意率和返工率。细节见[质量门和引擎合并](docs/reference/features.md#质量门和引擎合并)。

## 故障处理

| 情况 | 处理 |
| :-- | :-- |
| **进程中途退出、机器重启** | 核对 BACKLOG 后用同一会话续跑，已合并的不重做 |
| **撞上用量上限** | 冷却到重置时间，再用同一会话续跑 |
| **额度快用完** | 5 小时窗口到 70%、周额度到 90% 时不启动新任务，留出的给你自己用；`agentq run` 到 98% 时拒绝 |
| **任务启动后立刻失败** | 连续 3 个任务在 3 分钟内退出时熔断，暂停启动 30 分钟，再次失败时时长翻倍，最长 4 小时 |
| **任务自己卡住** | 先用新会话从上次的分支再接一次，要求先查资料、换思路；再卡住才通知你。授权、付费、账号、产品方向这类要你定的直接交给你 |
| **任务失控** | 单次运行有预算上限，子代理失控时自动停止，超时默认 4 小时。已有改动保留 |
| **状态文件损坏** | 读不了时不收尾也不启动。内容损坏时备份坏文件，按任务记录重建，那一轮不启动新任务 |
| **运行数据增长** | 任务记录保留 90 天，日志定期截断 |

这些路径有假 CLI 和暴力测试覆盖，真实撞限还没有跑过。暴力测试会杀进程、写坏状态文件、制造并发、写满磁盘、跳变时钟，依赖全部是假的，目前只在 macOS 上跑过。方法见 [CONTRIBUTING](CONTRIBUTING.md#暴力测试)，规则见[调度规则](docs/reference/scheduling.md)。

## Telegram

<table>
<tr>
<td width="55%">

Telegram 是可选的。配好后可以给机器人发需求，也可以带截图。

- 回复「要你定」的消息或点按钮，决定写回条目，任务继续。
- 回复任务提醒，接上那个任务。
- `状态`、`额度`、`暂停`、`恢复` 由代码直接回答，不消耗额度。

每天最多推送 10 条，23:00 到 08:00 的消息攒到早上发，同类消息 30 分钟内合并。任务合并后推送合并提交，失败、中断、卡住也推送。没配 Telegram 时用本机通知。

完成推送和决定卡片按钮在真实 Telegram 上走通，回复链和完整对话往返只用假服务测过。配置方法见 [Telegram 指南](docs/guide/telegram.md)。

</td>
<td width="45%">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/telegram-dark.png">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/telegram-light.png" alt="Telegram 对话：机器人问整体想要什么感觉，给 A、B、C 三个方向；选了 B；做完后推送已合并和合并提交">
</picture>
</td>
</tr>
</table>

## 配置

配置文件是 `~/.agentq/config.json`，改完下次 tick 生效，不用重启。常见设置见[配置指南](docs/guide/configuration.md)，每个键的说明见[配置参考](docs/reference/config.md)。

新安装时下面这些功能关闭，打开对应开关才运行：

| 功能 | 开关 | 作用 |
| :-- | :-- | :-- |
| **夜班** | `night_enabled` | 每天 01:00 推进待做、分诊 issue 并写报告 |
| **复盘** | `review_enabled` | 每天 06:30 复盘近期提交和失败任务，写早报 |
| **巡逻** | `patrol_enabled` | 每小时检查服务、备份、卡住的任务、额度和磁盘，有新异常时起一次只读分析 |
| **自动更新** | `auto_update` | 一键安装的发行版每天最多看一次新版本，没有任务在跑时装上；装上后变差就回滚到上一代，这个版本不再自动装。见[命令参考](docs/reference/commands.md#自动更新) |

## 支持范围

| 项目 | 状态 |
| :-- | :-- |
| **Claude Code CLI** | 默认后端。2.1.316 在 macOS 上真实走到合并，包括审查门、界面门、`agentq new`、launchd 后台和桌面端派活 |
| **Codex CLI** | 手动选择。codex-cli 0.166.0 在 macOS 上真实走到合并和 SIGTERM 续跑。大方向商量和巡逻分析在它上面不启动，没有美元上限 |
| **pi** | 手动选择。pi 1.5.1 在 macOS 上真实走到合并和 SIGTERM 续跑，模型跟随 pi 自己的设置。pi 没有权限确认和沙箱，工具以你的身份直接运行；大方向商量、追问和巡逻分析在它上面不启动，没有美元上限（tick 按用量停） |
| **macOS** | arm64 是主要平台。amd64 能交叉编译，没有在 Intel Mac 上跑过 |
| **Linux** | 需要 `systemd --user`。amd64 在 CI 上跑测试和真实 systemd 验收，没跑过真实任务。arm64 能交叉编译 |
| **WSL、Windows** | WSL 未验证，Windows 不支持 |

还没有真实验证的：真实撞限和令牌失效、Linux 上的真实任务和暴力测试。门和裸 CLI 在真实项目上的对比也没有做过。每项详情见[支持矩阵](docs/reference/support-matrix.md)。

后端现在支持 Claude Code、Codex 和 pi，其他 CLI 以后再加。安装时用 `--backend codex` 或 `--backend pi` 选（`... | sh -s -- --backend pi`），不写就是 Claude Code。接新工具的接口见 [docs/backends.md](docs/backends.md)。预编译程序只依赖 Git，源码构建只用 Go 标准库。

Claude 桌面端的 Code 标签页和 CLI 读同一份规则和技能。后台任务不显示在客户端的会话列表里，结果用 `agentq status` 或推送查看。

## 权限和限制

无人值守写代码需要 `bypassPermissions`。任务以你的系统用户运行，有你的全部文件权限，能读本地密钥（包括 App Store Connect 私钥和 SSH key），能运行任意命令。提示词和 deny 规则不是安全边界。agentq 能防模型偷懒和误操作，防不了存心破坏。

在高敏感的机器上，用单独的系统用户或容器运行，不放生产凭据。详见 [SECURITY](SECURITY.md)。

其他限制：

- 机器要开着。睡眠和重启后任务继续，关机期间不运行。
- 任务消耗你自己的模型额度。
- 命令输出、推送和 BACKLOG 的节标题目前是中文，条目内容不限语言。

## 隐私

agentq 没有遥测，不收集数据。任务内容只发给你使用的模型服务。除安装和升级外，agentq 自身只在对应功能打开时访问 Telegram 和 GitHub。

## 常见问题

- **什么任务不适合用 agentq？** 需要边做边看、频繁对话的任务，直接用 Claude Code。
- **没有待做时会消耗额度吗？** 不会。没有待做时不启动模型。夜班和巡逻分析要打开开关才运行。
- **项目已有规则时怎么办？** 项目的 `CLAUDE.md`、`AGENTS.md` 照常生效。agentq 装完第一次运行时放一份通用工程规则到 `~/.claude/rules/agentq/`，已有全局规则时不放。`agentq env on` 添加，`agentq env off` 移除。这份规则第一条写明项目规则优先，但这只是提示，所以里面只放通用工程习惯。
- **用哪个模型？** 新安装跟随你在 CLI 里选的模型和强度，不传参数。已有配置里写了的照用。
- **会把代码推到远程吗？** 调度器不推送，任务按 `agent-task` 技能也只走本地合并，推送要另外授权。这条对任务是技能约定，不是技术限制：任务能用你已登录的 `git`。可选的 `--backup` 服务只推 `~/.agentq/backup.json` 明确列出的私有目标。
- **怎么立刻停下？** `agentq pause` 不再启动新任务，`agentq stop <任务id>` 停止运行中的任务。
- **怎么卸载？** `agentq remove --yes` 卸载程序和服务，保留数据和恢复材料。恢复方法见[安装指南](docs/install.zh-CN.md)。
- **升级会丢数据吗？** 升级走事务，配置和任务数据保留。`agentq rollback --yes` 回到上一版。

## 文档

| 文档 | 内容 |
| :-- | :-- |
| **[入门指南](docs/guide/getting-started.md)** | 安装、第一个任务、其他安装方式 |
| **[配置指南](docs/guide/configuration.md)** | 项目目录、并发、模型、夜班和可选功能 |
| **[Telegram 指南](docs/guide/telegram.md)** | 配置机器人、添加任务、回复决定 |
| **[日常使用](docs/guide/daily-ops.md)** | 查看状态、停止、恢复、决定、早报 |
| **[故障排查](docs/guide/troubleshooting.md)** | 按症状查原因和处理方法 |
| **[命令参考](docs/reference/commands.md)** | 每个命令 |
| **[配置参考](docs/reference/config.md)** | 每个键的类型、默认值和作用 |
| **[调度规则](docs/reference/scheduling.md)** | 选择条目、并发、额度、熔断、续跑、状态损坏 |
| **[功能参考](docs/reference/features.md)** | 新项目、质量门、建议、夜班、巡逻、推送 |
| **[文件参考](docs/reference/files.md)** | `~/.agentq/` 下每个文件 |
| **[支持矩阵](docs/reference/support-matrix.md)** | 每项能力的验证情况 |
| **[安装指南](docs/install.zh-CN.md)** | 候选自检、冲突、事务备份和回滚 |

还有：[隐私门禁](docs/privacy.md)、[旧版安装](docs/legacy-install.md)、[给 Agent 的入口](llms.txt)。

## 参与

欢迎提 Issue、PR 和使用反馈，先看 [CONTRIBUTING](CONTRIBUTING.md)。漏洞按 [SECURITY](SECURITY.md) 报告。

## 许可证

[MIT](LICENSE)。第三方技能的来源、版本和许可证记录在 [`dependencies/claude.lock.json`](dependencies/claude.lock.json)。

agentq 是独立项目，和 Anthropic 没有隶属关系。
