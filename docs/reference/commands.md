# 命令参考

`agentq help` 打印简表。退出码：0 成功，1 执行出错，2 用法不对（没有参数、不认识的命令、参数个数不对）。

agentq 不是聊天命令。自然语言需求在项目目录的 `claude` 会话里说（`dispatch` 技能负责写 BACKLOG），或发给 Telegram 机器人。

任务 id 形如 `1104-120000-demo`，凡是要任务 id 的地方都可以写前缀，至少 4 个字符且不重名。

## 上手

### 一键安装

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

下载预编译程序并校验 SHA256，校验不过直接退出，不需要 Go，不需要 clone。重跑就是升级，配置和数据保留。带参数：`... | sh -s -- --extras`。


| 环境变量 | 作用 |
| --- | --- |
| `AGENTQ_ASSUME_YES` | `1` 等于 `--yes` |
| `AGENTQ_PROXY` | 等于 `--proxy` |
| `AGENTQ_MIRROR` | 等于 `--mirror` |
| `CI`、`NONINTERACTIVE` | 设了就只装不启动，有 `--yes` 除外 |
| `NO_COLOR` | 不上色 |
| `AGENTQ_VERSION` | 要装的版本，默认 `latest`，等于 `--version` |
| `AGENTQ_RELEASE_BASE` | 下载地址，优先于脚本里的仓库名，测试和镜像用。`latest` 取 `<地址>/<文件>`，指定版本取 `<地址>/<版本>/<文件>` |

| 参数 | 作用 |
| --- | --- |
| `--yes` | 明确同意启动，不再问 |
| `--proxy <地址>` | 下载走代理：`http://`、`https://`、`socks5://`、`socks5h://` |
| `--mirror <前缀>` | GitHub 下载镜像。校验文件和源码仍从原地址取，镜像改过的文件会被拒绝 |
| `--version <版本>` | 要装的版本；`main` 走源码，要有 Go |
| `--uninstall` | 转给 `agentq remove --yes`，数据保留。上次卸载没能改回 shell 配置文件时，重跑接着处理那一行 |

这几个参数脚本自己用，不传给 `setup`，其余参数原样传。

取程序的顺序：在本仓 checkout 里运行时本地构建；下载 Release；下载不到且有 Go 时拉源码构建。缺 Git 时给说明后退出。缺 Claude CLI 或没登录不挡安装，交给 `agentq start --welcome`。不用 sudo，root 运行时在写任何文件之前拒绝。终端里才转圈和上色。其他安装方式见[入门指南](../guide/getting-started.md#其他安装方式)。

新安装的最后一步是 `agentq start --welcome`，升级不问。

### `agentq update [--version 版本] [--proxy 地址] [--mirror 前缀]`

取发布地址上的 `install.sh`，对过 `SHA256SUMS` 再运行，之后和重跑一键脚本一样。发行构建把仓库名编进程序；`AGENTQ_RELEASE_BASE` 优先。源码构建的程序里没有地址，只说明怎么升级，退出 1，不猜。

### `agentq version`

打印版本，`--version` 同义。不读写配置。

### `agentq init [目录]`

```sh
cd ~/projects/my-app && agentq init
```

把目录（默认当前目录）接入 agentq。要求：Git 主目录，当前在默认分支（主目录在 `main` 或 `master` 上就用它，否则看 `origin/HEAD`，再看本地 `main`、`master`；都没有就报错，不猜），位于 `projects_root` 下一层或已登记在 `extra_projects`。不满足时报哪条和要执行的命令，不替你 `git init`、改分支或移动目录。项目不在那里时给的是 `mkdir -p <projects_root> && mv <项目> <projects_root>/<名字>`，也可以不动目录、直接 `agentq add`；同名目录已存在时不给移动命令；目录本身就是 `projects_root` 时说明怎么恢复。

生成缺的 `docs/BACKLOG.md`、`AGENTS.md`、`PROGRESS.md`、`DECISIONS.md`、`LESSONS.md` 并只提交这些，已有文件（包括 `CLAUDE.md`）和别的改动不动。提交失败时把这次生成的撤掉。可重复运行。

还没有 `.agentq/verify` 时，按项目根目录的文件判断检查命令写一份（可执行，一起提交，不调模型）：

| 看到 | 检查命令 |
| --- | --- |
| `go.mod` | `go test ./...` |
| `package.json` 的 `scripts.test`（`npm init` 的占位不算） | 按锁文件或 `packageManager` 用 npm、pnpm、yarn、bun，先装依赖 |
| `Cargo.toml` | `cargo test`，有 `[workspace]` 时加 `--workspace` |
| `pyproject.toml` 提到 `pytest` | 有 `uv.lock` 用 uv，有 `poetry.lock` 用 poetry，有 `[project]` 在临时虚拟环境里装好再跑；都没有算判断不了 |
| `Package.swift` 和 `Tests/` | `swift test`；只声明 iOS 的算判断不了 |

判断不了就不写，打印一句。`.agentq/` 被 `.gitignore` 忽略时也不写，测试门只认提交进去的脚本。工具没有、依赖下不来、Go 模块下一个包都没有时脚本退出 125，核验记未知，不写修回归；pytest 收集不到测试（退出码 5）算通过。见[合并后核验](features.md#合并后核验)。

### `agentq add "<需求>" [--project 名字]`

往当前目录所属项目（或 `--project`）的「待做」末尾加一条，只提交 BACKLOG。需求以 `-` 开头时写在 `--` 后面。空的、超过 800 字、带控制字符的拒绝；「待做」里已有同样一条时不重复加。

不用先 `init`：项目没有 `docs/BACKLOG.md` 时先接入（和 `init` 生成同样的文件）。当前目录所在的仓库不在 `projects_root` 下一层时自动登记到 [`extra_projects`](config.md#外面的项目)，打印一行说明，worktree 里运行时认到主目录。不是 Git 仓库的不接管，给 `git init` 命令；家目录这种大仓库、和已有项目重名的拒绝；主目录要在默认分支上。

### `agentq start --welcome [--assume-yes]`

一键脚本把终端交过来之后的那一步，也可以手动运行。讲清楚任务能做什么，问一句，只有空行算同意：n、其他输入、EOF、Ctrl-C 都不算，管道里的回车也不算。`--assume-yes` 或 `AGENTQ_ASSUME_YES=1` 是明确同意，不问。没有明确同意时，设了 `CI`、`NONINTERACTIVE` 或标准输入不是终端就只说明，退出 0。

同意后：

1. 没有 CLI 时运行官方命令 `curl -fsSL https://claude.ai/install.sh | bash`，失败就停，贴原文，给 `brew install --cask claude-code` 和 `npm install -g @anthropic-ai/claude-code`，什么都不改。标准输入不是终端时不装，打印命令，退出 1。
2. 没登录时运行 `claude auth login`（打开浏览器）。没登录上就不开后台，退出 1。标准输入不是终端时不运行，打印命令。
3. `~/.local/bin` 不在 PATH 时往 shell 配置文件加一行带 `# agentq managed` 标记的 export，改前备份成 `*.pre-agentq`。zsh、bash 之外不改，打印手动的那一行。
4. 写 `~/.agentq/consent.json`；第一次同意时把通用工程规则装到 `~/.claude/rules/agentq/`（已有用户规则不装，同意文字说明，见 [`agentq env`](#agentq-env-statusonoff)）；把 `permission_mode` 设成 `bypassPermissions`，其余和 `agentq start` 相同。没有待做时只打印一句怎么开始，不调用模型。

没有同意时不做这些，只打印后续命令。

### `agentq start [--accept-bypass]`

一步打开后台（只改 `permission_mode` 和 `paused`，不动 `notify_enabled`，要收通知自己打开）：跑和 `doctor` 相同的检查；权限显式写成非 `bypassPermissions` 时讲清风险再问；装调度服务（Telegram 凭据配好且 `notify_enabled=true` 时连监听一起装）；取消暂停；立刻跑一次 tick，列出在做和排着的条目。前一步不过就不做后一步，服务装不上时 `paused` 不变。可重复运行。

没有 CLI 或没登录时什么都不改，给官方命令，退出 1。新安装的 `permission_mode` 是 `default`，这个模式下任务写不了文件，所以这一步必须有。配置里没有这个键的（老配置，默认就是完全权限）不问。`--accept-bypass` 表示已经读过风险说明，不再询问，用于脚本。标准输入不是终端又没带它时不改，退出非 0。你回答否时不改、不开后台，退出 1。开始前先读 [SECURITY](../../SECURITY.md)。这一步是第一次同意时，通用工程规则的规矩和 `--welcome` 一样。

### `agentq env status|on|off`

看、装、关通用工程规则。规则放在后端读用户级规则的地方（Claude 是 `~/.claude/rules/agentq/`；Codex 没有规则目录，是 `$CODEX_HOME/AGENTS.md` 末尾一段带标记的块，只放 `core.md`，改前备份到 `~/.local/share/agentq/env-layer-backups/`，关掉只删这一段），前台会话和后台任务都读得到，不写你的 `CLAUDE.md` 和 `settings.json`。`core.md` 常驻，不超过 4KB；语言规则带 `paths`，碰到对应文件才加载。第一条就写明项目规则和你自己的规则优先，但这只是提示：官方写明用户级和项目级规则互不覆盖，冲突时模型可能任选一个，所以里面只有通用工程习惯。

- 什么时候自动装：只有第一次同意启动时，并且 `~/.claude/CLAUDE.md` 和 `~/.claude/rules/` 里都没有你自己的内容。升级、已经同意过的不自动加。
- `on`：明确要加，已有你自己的规则也装，你的文件不动。同名文件内容不一样时保留并报告。
- `off`：只删和 agentq 写入时内容一样的文件，改过的留着并报告。之后升级不再加回来。
- `status`：开没开、每个文件和写入时是否一样、你自己的全局规则在哪。

开着时升级会更新没改过的文件，改过的不动，也不挡升级和 `doctor`。你删掉的不写回；整个目录都删了就当你不要了，转成关着。要加回来运行 `on`。`agentq remove --yes` 和 `off` 一样只删没改过的，`agentq restore --yes` 恢复卸载时装回来。后端没有规则目录时 `on` 报错退出 1。

## 派活和查看

### `agentq run <项目名> [关键字]`

认不出默认分支、主目录不在默认分支时拒绝，说明原因和要执行的命令，见[默认分支](scheduling.md#默认分支)。

立刻为该项目启动一个任务。给了关键字就挑「待做」里第一条包含它的。不看暂停、`retry_after` 和连环失败熔断，仍遵守并发上限、用量冷却和登录检查。订阅额度 ≥98% 时拒绝，≥90% 只提示。标着进行中、卡住或正在跑的条目不算可做。

### `agentq discuss`、`agentq discuss answer <编号> "<回答>"`

看在商量大方向的条目：问题、默认答案、到什么时候按默认。`answer` 回答，回「随便」就是用默认，下次 tick 写进条目开工。Telegram 卡片的按钮和回复是同一件事。见[大方向商量](features.md#大方向商量)。

### `agentq new [--direction "方向"] "<一句话>"`

从零做出新项目第一版，不问问题，结果写当天汇总并推送。限制和格式见[一句话做新项目](features.md#一句话做新项目)。

### `agentq status`

依次显示：调度状态（暂停、用量冷却、额度快用完或连环失败熔断）；订阅额度各窗口的用量和重置时间；定点任务开关；巡逻；任务箱；已打开的闲时找活、主动感知、发版助手；推送；监听；在跑的定点任务和过门；运行中的任务（续跑的标「续跑中」，会话记录久没增长的标「疑似卡死」）；各项目「待做」条数；手动停掉还留着标记的条目；最近 10 个结束的任务（中断的标「中断」和续跑情况）。不拿锁。额度缓存过期时现读一次，只显示不写回。

### `agentq projects`

列出所有项目，每行目录名和路径，用制表符分开，`projects_root` 下的和 `extra_projects` 登记的都在。在 `exclude_projects` 里的多一列 `exclude_projects`。`dispatch`、`project-status` 技能和 Telegram 对话按名字找项目都用它。扫不了 `projects_root` 时列出看得到的，再报错退出 1。

### `agentq gate report <任务id>`、`agentq gate pass <任务id> --reason "原因"`、`agentq gate rerun <任务id>`、`agentq gate abandon <任务id>`

`report` 看过门的状态、分支、轮数、每道门的结论和证据，过了成品门的还有交付单。`pass` 只放行这一次卡在门上的任务：下次 tick 不跑门，只合 `report` 里你看到的那个提交（之后分支又动了不算），写审计行和当天汇总，原因必写。放行后遇到冲突合不了，还是等你定，能再放行。`abandon` 放弃卡在门上的任务，或新项目第一版的任务（接手的、没过门的、new 任务本身都行），下次 tick 生效：分支留着，写审计行；新项目的第一条标卡住，后面的条目开工。和 Telegram 卡片的「放弃」是同一个操作。任务还在跑、在过门时拒绝。见[质量门和引擎合并](features.md#质量门和引擎合并)。

### `agentq health [项目名]`

最近 30 天每个模块的改动次数、修复次数、行数（模块是文件所在目录，根目录的文件各算一个；文档、测试、锁文件不算），标出再修就要重写的模块，再加两周返工率。不写项目名就看全部。只读，只跑 git，不调模型。见[补丁门](features.md#补丁门)。

### `agentq logs <任务id>`

任务的状态、会话 id（可以 `claude -r` 接着看）、目录、结果全文、stderr 末尾 40 行。

### `agentq stop <任务id>`

杀掉该任务的进程组，先 SIGTERM，10 秒后仍在就 SIGKILL，标为已停止。进程已经自己结束的，按它的结果正常收尾。还不知道 pid 时按任务找进程，找到就杀；确认没有进程就直接标已停止；查不了（`ps` 失败）时报错说没停成，任务照旧算在跑、占着名额。下一条待做会立刻接上，想停住先 `agentq pause`。

### `agentq pause`、`agentq resume`

暂停或恢复自动接活。暂停时 tick 只收尾不启动，`run` 不受影响。只改 `config.json` 里 `paused` 的值。

### `agentq inbox`、`agentq inbox add "<内容>"`

通用任务箱：列出待做和各条状态；放一条不属于任何项目的活。内容压成一行、中和标记，超过 4000 字拒绝，一模一样的不重复放。见[任务箱](features.md#任务箱)。

## 评测

给 agentq 自己打分。任务在临时 HOME 和临时项目里跑，不碰 `~/.agentq` 的任务数据、`~/projects`。tick 永远不会自己跑。

### `agentq eval list`

列公开题：题号、领域、程序题还是模型题、一句话。留出集（`~/.agentq/evals/heldout/`）只显示数量。

### `agentq eval run [题号…] [--fake|--real] [--bin 路径]`

先说要跑几题、用哪个后端、调不调模型，再跑。每题和按领域打印分数，结果写 `~/.agentq/evals/runs/<时间>/results.json`。有题没通过退出码 1。题号写题号、短名或全名都行。

| 参数 | 作用 |
| --- | --- |
| 不带 | 只跑不调模型的程序题 |
| `--fake` | 假 CLI 走真实引擎的全流程（认领、过门、合并、判分），分数只说明流程通，不代表模型好坏 |
| `--real` | 调真实模型。周额度 ≥85% 拒绝，读不了额度就照跑并提示。任务用临时 HOME，CLI 经一个指回钥匙串的软链接用本机的登录，不复制凭据；目前只有 macOS 上的 Claude |
| `--bin 路径` | 评判这个 agentq 二进制，不写就评判自己。题目和判分逻辑都用运行中这一份 |

判分不问模型，看这些：默认分支上记录的合并提交是不是引擎写的第一父合并，默认分支上有没有没过门的改动，条目状态和文件（都读默认分支的干净克隆，不读工作区），题目原件的 `.agentq/verify` 和 `check.sh` 在这份克隆上的退出码（任务跑完才放到磁盘上），门的结果和日志（`gate/<任务id>/`）对得上才算门跑过，退回轮数、要人插手几次、token、时长。一题的分是过了的检查占多少。到时限没走完算没过；没跑成、撞用量上限算没结论。

任务有本机权限，判分防的是偷懒和误操作，不是安全边界：存心伪造一个带引擎字样的合并提交、造齐门的日志，判分认不出来。真实任务的观察期是第二道关。

没通过的题现场留在系统临时目录（`aq-*`，结果里有路径），下次 `run` 清掉超过 7 天的：只认这个前缀加标记文件 `.agentq-eval`，删的时候不跟软链接。`kill -9` 留下的也这样清。

`--fake` 和程序题里，假 CLI 放在 PATH 最前面，自检也调不到真实的 CLI。只有 `--real` 的模型题才用真实 CLI。

### `agentq eval baseline [run]`

把一次结果并进基线（`~/.agentq/evals/baseline.json`，默认最近一次）。同一题同一种跑法换成新的，别的留着；没结论的不进。

### `agentq eval compare [run]`

和基线比，只比同一题同一种跑法。任一题分数掉、有检查项从过变没过、退回变多、要人插手变多、这次没结论，或者任一领域平均变差，都算变差，退出码 1，不看总平均。token 和耗时波动大，只提醒。

## 统计

### `agentq stats [天数]`

最近 N 天（默认 7，1 到 3650）按任务种类、项目、模型统计次数、成败、token 和折算费用。只读。见[每个任务的用量](features.md#每个任务的用量和-agentq-stats)。

### `agentq quality`

最近 7 天的一次满意率、返工率，按项目列出每条合并的条目算成了什么、依据是什么。只读 BACKLOG 和 git。见[一次满意率和返工率](features.md#一次满意率和返工率)。

## 维护项目

### `agentq backlog archive`

```sh
agentq backlog archive <项目名|--all> [--dry-run]
```

把 `docs/BACKLOG.md`「已完成」的旧条目按原顺序移到 `docs/BACKLOG-archive.md` 顶部。留下：最前面 20 条、合并提交在 14 天内的、查不到合并时间的验收意见（`quality` 的 7 天窗口不受影响）。BACKLOG 或归档文件有未提交改动、项目有任务在跑、条目标着 24 小时内的进行中时拒绝。不自动提交。`--dry-run` 只打印会移多少、瘦到多大。

## 定点和主动功能

### `agentq night`、`agentq review`

立刻执行一次夜班或复盘。手动执行不看 `night_enabled`、`review_enabled`，也不过无事跳过。服务定点叫醒时带 `--scheduled`，开关为假就只记一行日志，确认没事也不起 claude。见[夜班和复盘](features.md#夜班和复盘)。

### `agentq patrol`

立刻巡逻一次，列出现有的每个问题和证据、处理说明。不看 `patrol_enabled` 和上次巡逻时间。见[巡逻](features.md#巡逻)。

### `agentq suggest [项目名]`

手动跑一次闲时找活，只读地看各项目（给了项目名只看这一个），低风险的写进「待做」，会改产品的写进「## 建议」，推送一条。不看 `suggest_enabled`、暂停、待做是否为空、额度低于 60% 和当天次数，但占用当天的记录。仍遵守用量冷却和登录检查，订阅额度 ≥98% 时拒绝。见[闲时找活](features.md#闲时找活)。

### `agentq sense [--dry-run]`

手动跑一次主动感知：读 App Store 评价、TestFlight 反馈和 GitHub issue，有新内容才起模型判断，真问题写进「## 建议」并推送一条。`--dry-run` 只读，列每个来源读到多少条，不写任何文件，不调模型。不看 `sense_enabled`。额度 ≥98% 时读了但不判断。见[主动感知](features.md#主动感知)。

### `agentq release check [项目名]`

只读显示发版条件。没有能提交审核的命令，提交只能回复那条提示消息说「发」。没配 `~/.appstoreconnect/config.env` 时报错。见[发版助手](features.md#发版助手)。

## 推送

### `agentq notify setup`

配 Telegram。有 `~/.agentq/secrets/telegram-setup.sh` 就执行它；没有且还没配凭据时打印手动配置的步骤。凭据能用时把 `notify_enabled` 改成 `true`（只改这一个键），提示运行 `agentq start` 装上常驻监听。凭据不能用（如权限不是 600）时不改，说明原因。

### `agentq notify test`

立刻发一条测试消息（配了 Telegram 发 Telegram，没配弹本机通知），不占每天的条数。

### `agentq notify send`

```sh
agentq notify send [--security] [--key=K] [--file=路径]… <类别> <一句话> [补充…]
```

记一条消息并按规则推。类别：`decide`、`alert`、`patrol`、`release`、`done`。夜班和 agent 用。`--security` 不等合并窗口，不管夜间和上限。`--key` 相同的只留最新说法。`--file` 附截图、效果图（按图片发）、录屏（按视频发，超过 50MB 先压缩），跟在消息后面发，调用的那一刻检查和压缩。

### `agentq notify file <路径> [说明]`

马上把一个文件发出去（Telegram），对话任务里回图片、录屏用，不进队列，不占条数。

### `agentq notify status`

用哪个通道、今天推了几条、攒着哪些消息、排队等回答的消息数和最近的错误。

### `agentq listen`

常驻收 Telegram 消息，随到随回。见[在 Telegram 里说话](features.md#在-telegram-里说话)。

## 安装和升级

### `agentq setup`

```sh
agentq setup [--yes] [--plan] [--scope full|skills] [--claude 路径] [--services] [--listen] [--backup] [--watchdog] [--extras]
```

| 选项 | 作用 |
| --- | --- |
| `--yes` | 确认计划中的变化 |
| `--plan` | 准备候选并打印计划，不激活 |
| `--scope` | `full`（默认）或 `skills`（只装技能，不装后台程序和服务，不要求 Claude 登录） |
| `--claude`、`--backend-path` | 指定 CLI 路径。已有配置里这个后端的路径和它不同时拒绝 |
| `--backend` | 新安装用哪个后端（默认 `claude`），写进新建的配置，技能投影到这个后端读的目录。已有配置不改，不一致就报错 |
| `--services` | 装 tick、夜班、复盘三个服务，不自动恢复调度，不装 Telegram 监听 |
| `--listen` | 再装常驻的 Telegram 监听，要和 `--services` 一起用，升级时保留 |
| `--backup` | 装可选的私有 Git 备份服务，需要 `gh` |
| `--watchdog` | 装可选的本地看门狗 |
| `--extras` | 加可选的第三方技能 read、write、learn、health |

`scripts/bootstrap.sh` 是同一个引擎的源码入口，先构建临时候选再调用它，参数相同。详情见[安装指南](../install.zh-CN.md)。

### `agentq doctor`

只读检查配置、安装完整性和 CLI。没有受管安装时报错退出。

### `agentq rollback --yes`

回到上一完整代，配置和任务数据不回滚。回滚目标用一次就没了。

### `agentq remove --yes`

卸载受管入口和服务，保留数据、配置和恢复材料。用户后来修改过的受管文件先停下。`agentq uninstall --yes` 同义。

### `agentq restore --yes`

恢复中断事务或最近一次卸载。也可以用 `sh scripts/bootstrap.sh restore --yes`。

### `agentq maintenance backup`、`agentq maintenance watchdog`

可选服务调用的入口。`backup` 只向 `~/.agentq/backup.json` 明确列出的目标推送，每次确认远程地址没变且 `gh` 识别为私有仓库，否则拒绝。`watchdog` 跑一次 `doctor`。结果写 `~/.agentq/<名字>-report.txt`。

### `agentq install`、`agentq uninstall`

旧版 binary-only 安装的入口，见 [legacy-install](../legacy-install.md)。有安装回执时拒绝执行，提示改用 `setup --services` 或 `remove`。带参数时分别转给 `setup`、`remove`。

## 内部命令

平时不用手动执行。

| 命令 | 作用 |
| --- | --- |
| `agentq tick` | 一次调度，由服务和任务外壳调用，手动执行也可以。上一个 tick 没结束时跳过 |
| `agentq tick --after <任务id>` | 任务外壳在任务结束后调用，拿不到锁最多等 60 秒 |
| `agentq night --scheduled`、`agentq review --scheduled` | 服务定点叫醒 |
| `agentq _job`、`agentq _gate` | 任务外壳和过门后台进程，不列在 `help` 里 |
