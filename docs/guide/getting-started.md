# 入门

本文说明如何安装 agentq、添加第一个任务和查看结果。

## 前提条件

- macOS 13 以上，或带 `systemd --user`（240 以上）的 Linux。支持范围见[支持矩阵](../reference/support-matrix.md)。
- Git。项目主目录要在默认分支上，否则 agentq 不启动该项目的任务。默认分支的识别顺序见[调度规则](../reference/scheduling.md#默认分支)。
- Claude Code CLI，要能用 `--print`、`--output-format`、`--session-id`、`--effort`，启动时会检查。没有时，脚本在你同意后运行官方命令 `curl -fsSL https://claude.ai/install.sh | bash` 并运行 `claude auth login`。没有登录时不开启后台。

桌面端不能跑后台任务，只装了桌面端也需要 CLI，安装过程相同。

官方脚本安装失败时，脚本停止并显示原始报错。可以改用官方的另外两种方式：

```sh
brew install --cask claude-code
npm install -g @anthropic-ai/claude-code
```

## 安装

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

脚本下载预编译程序并校验，不需要 Go。已安装时重跑就是升级，配置和数据保留。


安装结束时，脚本说明任务的权限（独立进程能读写当前用户的所有文件，任务内容发给你使用的模型服务），然后询问：

```text
按回车同意并启动，输入 n 只安装不启动：
```

只有空行算同意，同意记录在 `~/.agentq/consent.json`。回答前先读 [SECURITY](../../SECURITY.md)。同意后，脚本做以下操作：

- 安装缺少的 CLI 并登录。
- `~/.local/bin` 不在 PATH 时，往 zsh 或 bash 的配置文件加一行带标记的 export，改前备份成 `*.pre-agentq`，卸载时只删这一行。其他 shell 不改，打印需要手动添加的那一行。写入哪个文件见[命令参考](../reference/commands.md#agentq-start---welcome---assume-yes)。
- 第一次同意时，把通用工程规则放到 `~/.claude/rules/agentq/`，前台会话和后台任务都会读取。`~/.claude/CLAUDE.md` 或 `~/.claude/rules/` 里已有内容时不放。`agentq env on` 添加，`agentq env off` 移除。
- 把 `permission_mode` 设成 `bypassPermissions`。任务无人值守时要改文件、运行命令，新安装的 `default` 写不了文件。
- 打开推送。任务合并、失败、中断、卡住时通知你，没配 Telegram 时用本机通知。
- 安装服务，取消暂停，立刻检查一次待做。没有待做时不调用模型，不消耗额度。

没有终端（管道、容器）或设了 `CI`、`NONINTERACTIVE` 时，脚本只安装不启动，退出码 0：不装 CLI，不改 shell 配置，打印需要运行的官方命令。之后运行 `agentq start` 启动。

在脚本里要一次完成时加 `--yes`（`... | sh -s -- --yes`，或 `AGENTQ_ASSUME_YES=1`），这算明确同意。没有终端时它同样不安装 CLI 和登录，缺少时以非 0 退出。

当前终端找不到 `agentq` 时，用 `~/.local/bin/agentq`，或新开一个终端。

## 添加第一个任务

在项目目录打开 Claude Code，说明要做什么。`dispatch` 技能把它写成带验收标准的条目。

不打开 Claude Code 时，用 `agentq add`：

```sh
cd ~/code/my-app
agentq add "给导出流程加失败重试并补测试"
```

`agentq add` 往「待做」加一条并提交。不在项目目录时加 `--project my-app`。条目写清要什么、怎样算完成，任务结果更容易符合预期。

第一次 `add` 时项目自动接入：缺少的 `docs/BACKLOG.md` 等文件会生成，已有文件不改。项目要是 Git 仓库，主目录在默认分支上。项目不要求放在 `projects_root`（默认 `~/projects`）下，放在外面时自动登记到 `extra_projects`，并打印一行说明。

## 查看结果

```sh
agentq status                 # 运行中的任务、各项目待做、最近结束的任务
agentq logs <任务id>          # 结果全文和 stderr 末尾，id 写前缀就行
```

每天的记录在 `~/.claude/nightly/<日期>-agentq.md`。任务在项目的独立 worktree 里工作，默认位置是 `.claude/worktrees/`。任务把分支做到可合并，agentq 通过质量门后合并到本地默认分支，不执行 `git push`。

工作流程见 [README](../../README.md#工作流程)，日常操作见[日常使用](daily-ops.md)。

## 安装内容

完整安装包括：

- 程序和支持脚本。
- 8 个自有技能：`agent-task`、`dispatch`、`nightly-review`、`new-project`、`project-status`、`no-ai-look`、`prior-art`、`feedback-triage`。
- 4 个锁定版本的第三方 Waza 技能：`check`、`think`、`hunt`、`ui`。
- 5 个项目模板。
- 通用工程规则（`core.md` 加 6 个语言文件）。
- 许可证。

不需要 Node，也不需要复制别人的配置。Telegram 可选，安装时不要机器人 token。

## 只手动运行

要逐条手动运行、不装服务时，只改权限。读过 [SECURITY](../../SECURITY.md) 后，编辑 `~/.agentq/config.json`，把 `permission_mode` 改成 `bypassPermissions`，然后运行：

```sh
agentq run my-app 导出流程
```

`agentq run` 不看暂停。任务结束后的收尾由任务外壳触发的 tick 完成，不依赖定时服务。

## 其他安装方式

一键脚本自己的参数：

| 参数 | 作用 |
| --- | --- |
| `--yes` | 明确同意启动，不再询问（`AGENTQ_ASSUME_YES=1` 同义） |
| `--proxy <地址>` | 下载走代理，支持 `http://`、`https://`、`socks5://`、`socks5h://`（`AGENTQ_PROXY` 同义） |
| `--mirror <前缀>` | GitHub 下载镜像（`AGENTQ_MIRROR` 同义）。校验文件和源码仍从原地址取，镜像改过的文件会被拒绝 |
| `--version <版本>` | 要安装的版本，默认最新。`main` 走源码，要有 Go（`AGENTQ_VERSION` 同义） |
| `--uninstall` | 转给 `agentq remove --yes`，数据保留 |

也可以从源码安装。入口调用同一个安装引擎，需要 Go（版本见 `go.mod`）。Python 3 可选，只有可选第三方技能的辅助脚本要用，缺少时只给一行警告。

```sh
sh scripts/bootstrap.sh --yes
```

下面的选项 `scripts/bootstrap.sh` 和 `install.sh` 都接受：

| 选项 | 作用 |
| --- | --- |
| `--plan` | 检测、构建候选并打印计划，不激活。计划列出具体变化路径和事务备份位置。它会联网获取锁定的公开依赖，并在临时 HOME 里自检 |
| `--scope skills` | 只装技能、模板和支持文件，不装后台程序和服务，不要求 Claude 登录 |
| `--extras` | 加可选的第三方技能 read、write、learn、health |
| `--backend <名字>` | 新安装用哪个后端，默认 `claude`。已有配置不改，不一致时报错 |
| `--claude <绝对路径>` | 指定 CLI。已有配置的 `claude_path` 和它不同时拒绝 |
| `--services` | 安装 tick、夜班、复盘三个服务，不恢复调度 |
| `--listen` | 再安装常驻的 Telegram 监听，要和 `--services` 一起用。`agentq start` 在 Telegram 凭据配好且 `notify_enabled` 打开时自动带上 |
| `--backup` | 安装可选的私有 Git 备份服务，需要 `gh`，还要在 `~/.agentq/backup.json` 明确列出目标 |
| `--watchdog` | 安装可选的本地看门狗，只写本地报告 |

只安装不启动时的默认状态：暂停，夜班、复盘、巡逻、推送关闭，权限 `default`。已有配置逐字节保留。同名技能用你自己的版本，只有任务合同依赖的四个（`agent-task`、`dispatch`、`new-project`、`nightly-review`）内容不同时才停下。本地修改或非法配置先报告并停止，不覆盖。

先只安装、后来用 `agentq start` 启动时，推送不会自动打开，要自己打开。夜班、巡逻等开关也要自己打开，见[配置](configuration.md)。

由 agent 代装时，可以这样说：读本仓 `llms.txt`，一条命令装好 agentq，只装不启动，把 `agentq start` 交给我确认。

完整的检测项、所有权规则和恢复流程见[安装指南](../install.zh-CN.md)。旧版 binary-only 安装见 [legacy-install](../legacy-install.md)。

## Linux

命令和 macOS 相同。不选服务时不需要 systemd，容器里也能安装和手动运行。

- 服务用 `systemd --user`。`systemctl --user` 不可用时（WSL 没开 systemd、容器、ssh 登录没有用户会话），`--services` 明确报错，不写文件。
- 没开 linger 时，退出登录后用户服务和运行中的任务都会停止。要持续运行，自己执行 `loginctl enable-linger`，agentq 不替你开。
- iOS 构建、Xcode、模拟器、代码签名、TestFlight 上传都需要 macOS，这类任务要在 Mac 上运行。

## 语言

命令输出和推送消息目前是中文。BACKLOG 的节标题要写中文（`## 待做`、`## 已完成` 等），语法见[调度规则](../reference/scheduling.md#backlog-语法)。条目内容可以用任何语言。

## 升级、回滚、卸载

```sh
agentq update                                                                     # 升级：取发布地址上的 install.sh，校验后运行
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh   # 或者重跑安装命令
agentq doctor                                                                     # 只读检查
agentq rollback --yes                                                             # 回到上一完整代，数据和配置不回滚
agentq remove --yes                                                               # 卸载受管入口和服务，保留数据和配置
agentq restore --yes  # 撤回中断的安装；卸载之后用卸载时打印的路径运行
```

升级时不询问，已选的服务和暂停状态不变。源码构建的程序里没有发布地址，`agentq update` 只说明怎么升级。

有活跃任务、未知状态或用户修改时，安装程序拒绝替换，不覆盖运行中的程序。
