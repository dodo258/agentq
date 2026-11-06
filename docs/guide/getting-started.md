# 入门

## 要先有什么

- macOS 13 以上，或带 `systemd --user`（240 以上）的 Linux。支持范围见[支持矩阵](../reference/support-matrix.md)。
- Git。默认分支自动认（主目录在 `main` 或 `master` 上就用它，否则看 `origin/HEAD`，再看本地 `main`、`master`），主目录要在它上面，不在就不接任务。
- Claude Code CLI。没有也行：在终端里按回车同意后，脚本运行官方命令 `curl -fsSL https://claude.ai/install.sh | bash` 帮你装上，再运行 `claude auth login` 登录。没登录不开后台。
- CLI 要能用 `--print`、`--output-format`、`--session-id`、`--effort`，启动时会检查。

只有 Claude 桌面端也能这样走：桌面端不能跑后台任务，后台要独立的 CLI，脚本照样帮你装。

官方脚本装不上时停下，贴出原文，可以换官方的另外两种方式：

```sh
brew install --cask claude-code
npm install -g @anthropic-ai/claude-code
```

## 一条命令上手

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

下载预编译程序并校验，不需要 Go。重跑就是升级，配置和数据保留。


装好后脚本讲清楚任务能做什么（独立进程能读写这个用户的所有文件，任务内容发给你所用的模型服务），问一句：

```text
按回车同意并启动，输入 n 只安装不启动：
```

只有空行算同意，同意记在 `~/.agentq/consent.json`。之后脚本补上缺的 CLI 和登录，`~/.local/bin` 不在 PATH 时往 shell 配置文件（zsh 的 `~/.zshrc`，bash 在 Linux 上是 `~/.bashrc`，在 macOS 上是 `~/.bash_profile`、`~/.bash_login`、`~/.profile` 里第一个存在的，都没有才新建 `~/.bash_profile`）加一行带标记的 export，改前备份成 `*.pre-agentq`，卸载时只删这一行。别的 shell 不改，给你手动的那一行。第一次同意时还会把一份通用工程规则放到 `~/.claude/rules/agentq/`，前台会话和后台任务都读得到；你已有全局规则（`~/.claude/CLAUDE.md` 或 `~/.claude/rules/` 里有内容）就不放，要加运行 `agentq env on`，不想要运行 `agentq env off`。然后把 `permission_mode` 设成 `bypassPermissions`（任务无人值守要改文件、跑命令，新安装的 `default` 写不了文件），装服务，取消暂停，立刻看一次待做。先读 [SECURITY](../../SECURITY.md)。没有待做时什么都不做，不消耗额度。

然后在项目目录打开 Claude Code，说一句要做什么就行，由 `dispatch` 技能写成带验收标准的条目。

没有终端（管道、容器）或设了 `CI`、`NONINTERACTIVE` 时只装不启动，退出码 0：不装 CLI、不改 shell 配置，打印要运行的官方命令，之后自己运行 `agentq start`。脚本里要一步到位就加 `--yes`（`... | sh -s -- --yes`，或 `AGENTQ_ASSUME_YES=1`），这算明确同意；没有终端时它同样不替你装 CLI 和登录，缺了就退出非 0。

当前终端找不到 `agentq` 时用 `~/.local/bin/agentq`，或重开一个终端。

## 手动派活

不想开会话时：

```sh
cd ~/code/my-app
agentq add "给导出流程加失败重试并补测试"
```

`add` 往「待做」加一条并提交。不在项目目录时加 `--project my-app`。需求写清要什么、怎样算做完，效果会好很多。

第一次 `add` 会把项目接入：缺的 `docs/BACKLOG.md` 等文件会生成，已有的不动。项目要是 Git 仓库，主目录在默认分支上。不用放到 `projects_root`（默认 `~/projects`）下，在外面就自动登记到 `extra_projects`，打印一行说明。

## 装了什么

完整安装包括程序、8 个自有技能（`agent-task`、`dispatch`、`nightly-review`、`new-project`、`project-status`、`no-ai-look`、`prior-art`、`feedback-triage`）、4 个锁定版本的第三方 Waza 技能（`check`、`think`、`hunt`、`ui`）、支持脚本、5 个项目模板、通用工程规则（`core.md` 加 6 个语言文件）和许可证。不要求复制别人的配置，不需要 Node。Telegram 可选，安装时不要机器人 token。

## 看结果

```sh
agentq status                 # 在跑什么、各项目待做、最近结束
agentq logs <任务id>          # 结果全文和 stderr 末尾，id 写前缀就行
```

每天的记录在 `~/.claude/nightly/<日期>-agentq.md`。任务在项目的独立 worktree 里工作，默认位置是 `.claude/worktrees/`，做到分支可合并，引擎过门后合并进本地默认分支，不会 `git push`。

一条从待做到合并的流程见 [README](../../README.md#它怎么工作)，日常操作见[日常使用](daily-ops.md)。

## 不想开后台

想一条一条手动跑，只改权限，不装服务。编辑 `~/.agentq/config.json` 把 `permission_mode` 改成 `bypassPermissions`（读过 [SECURITY](../../SECURITY.md) 再改），然后：

```sh
agentq run my-app 导出流程
```

`agentq run` 不看暂停。任务结束后的收尾由任务外壳触发的 tick 完成，不依赖定时服务。

## 其他安装方式

一键脚本自己的参数：

| 参数 | 作用 |
| --- | --- |
| `--yes` | 明确同意启动，不再问（`AGENTQ_ASSUME_YES=1` 同义） |
| `--proxy <地址>` | 下载走代理，`http://`、`https://`、`socks5://`、`socks5h://`（`AGENTQ_PROXY` 同义） |
| `--mirror <前缀>` | GitHub 下载镜像（`AGENTQ_MIRROR` 同义）。校验文件和源码仍从原地址取，镜像改过的文件会被拒绝 |
| `--version <版本>` | 要装的版本，默认最新；`main` 走源码，要有 Go（`AGENTQ_VERSION` 同义） |
| `--uninstall` | 转给 `agentq remove --yes`，数据保留 |

一键安装之外，还有从源码走的入口。它们调用同一个安装引擎，需要 Go（版本见 `go.mod`）。下面的选项 `install.sh` 也认。Python 3 可选，只有可选第三方技能的辅助脚本要用，缺了只给一行警告。

```sh
sh scripts/bootstrap.sh --yes
```

| 选项 | 作用 |
| --- | --- |
| `--plan` | 先检测、构建候选并打印计划，不激活。计划列出具体变化路径和事务备份位置。它会联网获取锁定的公开依赖，并在临时 HOME 里自检 |
| `--scope skills` | 只装技能、模板和支持文件，不装后台程序和服务，不要求 Claude 登录 |
| `--extras` | 加可选的第三方技能 read、write、learn、health |
| `--claude <绝对路径>` | 指定 CLI。已有配置的 `claude_path` 和它不同时拒绝 |
| `--services` | 装 tick、夜班、复盘三个服务，不恢复调度 |
| `--listen` | 再装常驻的 Telegram 监听，要和 `--services` 一起用。`agentq start` 在凭据配好时会自动带上 |
| `--backup` | 装可选的私有 Git 备份服务，需要 `gh`，还要在 `~/.agentq/backup.json` 明确列目标 |
| `--watchdog` | 装可选的本地看门狗，只写本地报告 |

只装不启动时的默认状态：暂停，夜班、复盘、巡逻、推送关闭，权限 `default`。已有配置逐字节保留。同名技能用你自己的版本，只有任务合同靠的四个（`agent-task`、`dispatch`、`new-project`、`nightly-review`）内容不同才停下；本地修改或非法配置先报告并停止，不覆盖。

一键脚本里同意启动时会打开推送：任务合并后通知你，失败、中断、卡住也说，没配 Telegram 就弹本机通知。先只安装、后来用 `agentq start` 启动的，推送要自己打开。夜班、巡逻等开关也要自己打开，见[配置](configuration.md)。

Agent 代装时可以用这句话：读本仓 `llms.txt`，一条命令装好 agentq，只装不启动，把 `agentq start` 交给我确认。

完整的检测项、所有权规则和恢复流程见[安装指南](../install.zh-CN.md)。旧版 binary-only 安装见 [legacy-install](../legacy-install.md)。

## Linux

命令相同。不选服务时不需要 systemd，容器里也能装和手动跑。

- 服务用 `systemd --user`。`systemctl --user` 不可用时（WSL 没开 systemd、容器、ssh 进来没有用户会话）`--services` 明确报错，不写文件。
- 没开 linger 时，你退出登录后用户服务和正在跑的任务都会停。要一直跑就自己执行 `loginctl enable-linger`，agentq 不替你开。
- iOS 构建、Xcode、模拟器、代码签名、TestFlight 上传都要 macOS，这类任务要放在 Mac 上跑。

## 语言

命令输出和推送消息目前是中文。BACKLOG 的节标题要写中文（`## 待做`、`## 已完成` 等），语法见[调度规则](../reference/scheduling.md#backlog-语法)。条目内容可以用任何语言。

## 升级、回滚、卸载

```sh
agentq update                                                                     # 升级：取发布地址上的 install.sh，校验后运行
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh   # 或者重跑同一条
agentq doctor                                                                     # 只读检查
agentq rollback --yes                                                             # 回到上一完整代，数据和配置不回滚
agentq remove --yes                                                               # 卸载受管入口和服务，保留数据和配置
sh scripts/bootstrap.sh restore --yes                                             # 恢复卸载或中断的事务
```

升级不问，已选的服务和暂停状态不变。源码构建的程序里没有发布地址，`agentq update` 会说明怎么升级，不猜。

活跃任务、未知状态和用户修改会阻止替换，不会覆盖运行中的程序。
