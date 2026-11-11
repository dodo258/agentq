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
| `AGENTQ_PAUSED` | `1` 等于 `--paused` |
| `AGENTQ_PROXY` | 等于 `--proxy` |
| `AGENTQ_MIRROR` | 等于 `--mirror` |
| `CI`、`NONINTERACTIVE` | 设了就不打开终端：CLI 没装或没登录时只打印命令 |
| `NO_COLOR` | 不上色 |
| `AGENTQ_VERSION` | 要装的版本，默认 `latest`，等于 `--version` |
| `AGENTQ_RELEASE_BASE` | 下载地址，优先于脚本里的仓库名，测试和镜像用。`latest` 取 `<地址>/<文件>`，指定版本取 `<地址>/<版本>/<文件>` |

| 参数 | 作用 |
| --- | --- |
| `--paused` | 只安装，后台先不跑。默认装完即运行（安装本身就是同意） |
| `--yes` | 旧参数，留着不报错，没有作用 |
| `--proxy <地址>` | 下载走代理：`http://`、`https://`、`socks5://`、`socks5h://` |
| `--mirror <前缀>` | GitHub 下载镜像。校验文件和源码仍从原地址取，镜像改过的文件会被拒绝 |
| `--version <版本>` | 要装的版本；`main` 走源码，要有 Go |
| `--uninstall` | 转给 `agentq remove --yes`，数据保留。上次卸载没能改回 shell 配置文件时，重跑接着处理那一行 |

这几个参数脚本自己用，不传给 `setup`，其余参数原样传。

取程序的顺序：在本仓 checkout 里运行时本地构建；下载 Release；下载不到且有 Go 时拉源码构建。缺 Git 时给说明后退出。缺 Claude CLI 或没登录不挡安装，交给 `agentq start --welcome`。不用 sudo，root 运行时在写任何文件之前拒绝。终端里才转圈和上色。其他安装方式见[入门指南](../guide/getting-started.md#其他安装方式)。

新安装的最后一步是 `agentq start --welcome`，升级不问。

### `agentq update [--version 版本] [--proxy 地址] [--mirror 前缀] [--check] [--saved-net]`

取发布地址上的 `install.sh`，对过 `SHA256SUMS` 再运行，之后和重跑一键脚本一样。发行构建把仓库名编进程序；`AGENTQ_RELEASE_BASE` 优先。源码构建的程序里没有地址，只说明怎么升级，退出 1，不猜。下载有超时，安装脚本本身不设超时（安装事务中途被杀会留下服务全停的半截安装）。

- `--check`：只看发布上的最新版本：取 `VERSION`，对过 `SHA256SUMS` 再打印，不装；拿不到或对不上退出 1。
- `--saved-net`：没给 `--proxy`、`--mirror`（也没有 `AGENTQ_PROXY`、`AGENTQ_MIRROR`）时，沿用一键安装时用过的，记在 `~/.agentq/update-net.json`（一键脚本每次装完更新它，都没用就删掉）。[自动更新](#自动更新)用它。

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

判断不了就不写，打印一句。`.agentq/` 被 `.gitignore` 忽略时也不写，测试门只认提交进去的脚本。工具没有、依赖下不来、Go 模块下一个包都没有时脚本退出 125，测试门判不了，不退回任务；pytest 收集不到测试（退出码 5）算通过。见[测试门](features.md#测试门)。

### `agentq add "<需求>" [--project 名字]`

往当前目录所属项目（或 `--project`）的「待做」末尾加一条，只提交 BACKLOG。需求以 `-` 开头时写在 `--` 后面。空的、超过 800 字、带控制字符的拒绝；「待做」里已有同样一条时不重复加。

不用先 `init`：项目没有 `docs/BACKLOG.md` 时先接入（和 `init` 生成同样的文件）。当前目录所在的仓库不在 `projects_root` 下一层时自动登记到 [`extra_projects`](config.md#外面的项目)，打印一行说明，worktree 里运行时认到主目录。当前目录不在任何 Git 仓库里时就地 `git init -b main`，没有 `.gitignore` 就写一份挡住 `.env`、密钥和依赖目录的，再把现有文件做初始提交（没配 git 身份等失败时撤掉这次建的 `.git` 和 `.gitignore`）；家目录、它的上级、`projects_root` 本身不建，有 `.git` 但 git 认不出的不碰。家目录这种大仓库、和已有项目重名的拒绝；主目录要在默认分支上。

### `agentq start --welcome [--paused]`

一键脚本装完之后的那一步，也可以手动运行。装完即运行：安装本身就是同意，不问也不等输入，只把后台任务能做什么（读写本机文件、运行命令、用你自己的模型额度）和暂停、卸载命令说一遍。`--paused` 只说明怎么开，什么都不改。装之前就有的配置写着暂停时保留暂停，不打开。旧的 `--assume-yes` 还认，没有作用。

接着：

1. 写 `~/.agentq/consent.json`（方式 `install`），把 `permission_mode` 设成 `bypassPermissions`；第一次运行时把通用工程规则装到 `~/.claude/rules/agentq/`（已有用户规则不装，说明一句，见 [`agentq env`](#agentq-env-statusonoff)）；新安装打开推送。
2. `~/.local/bin` 不在 PATH 时往 shell 配置文件加一行带 `# agentq managed` 标记的 export，改前备份成 `*.pre-agentq`。zsh 写 `~/.zshrc`；bash 在 Linux 上写 `~/.bashrc`，在 macOS 上写 `~/.bash_profile`、`~/.bash_login`、`~/.profile` 里第一个存在的，都没有才新建 `~/.bash_profile`。zsh、bash 之外或不知道 shell 时不改，打印手动的那一行。
3. 没有 CLI 时运行官方命令 `curl -fsSL https://claude.ai/install.sh | bash`，失败就停，贴原文，给 `brew install --cask claude-code` 和 `npm install -g @anthropic-ai/claude-code`。没登录时运行 `claude auth login`（打开浏览器）。标准输入不是终端时这两步都不做，打印命令，退出 1；第 1 步已经记下，CLI 弄好后派活技能查到 `not-running` 会自己 `agentq start`。
4. 其余和 `agentq start` 相同。没有待做时只打印一句怎么开始，不调用模型。

### `agentq start --check`

只读，派活技能交任务前先跑。第一行是状态，第二行 `projects_root: <路径>`，后面是给人看的说明：

| 状态 | 意思 |
| --- | --- |
| `ready` | 后台在跑 |
| `not-running` | 同意过、不是你暂停的，服务没在跑（被卸、重启后没加载、装的时候 CLI 没弄好）：`agentq start` 直接拉起 |
| `paused` | 你自己 `agentq pause` 的：不替你打开 |
| `off` | `--paused` 装的，或装之前就暂停着的老配置：你说开才开 |
| `need-cli` | 官方 CLI 没装或没登录 |
| `skills-only` | 只装了技能 |

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

立刻为该项目启动一个任务。给了关键字就挑「待做」里第一条包含它的。不看暂停、`retry_after` 和连环失败熔断，仍遵守并发上限、用量冷却和登录检查。订阅额度 ≥98% 时拒绝，≥90% 只提示。标着进行中、卡住或正在跑的条目不算可做；但这个项目里有中断了、还没人续跑的条目（门退回、暂停时没续跑、`exclude_projects` 的项目里自动起的任务不自动续跑）且包含关键字时，先用同一会话接着做它。`run` 启动的任务暂停时、项目在 `exclude_projects` 里时也会被 tick 续跑。

### `agentq discuss`、`agentq discuss answer <编号> "<回答>"`

看在商量大方向的条目：问题、默认答案、到什么时候按默认。`answer` 回答，回「随便」就是用默认，下次 tick 写进条目开工。Telegram 卡片的按钮和回复是同一件事。见[大方向商量](features.md#大方向商量)。

### `agentq new [--direction "方向"] "<一句话>"`

从零做出新项目第一版，不问问题，结果写当天汇总并推送。限制和格式见[一句话做新项目](features.md#一句话做新项目)。

### `agentq status`

依次显示：调度状态（暂停、用量冷却、额度快用完或连环失败熔断）；订阅额度各窗口的用量和重置时间；定点任务开关；巡逻；推送；监听；在跑的定点任务和过门；运行中的任务（续跑的标「续跑中」，会话记录久没增长的标「疑似卡死」）；各项目「待做」条数；手动停掉还留着标记的条目；最近 10 个结束的任务（中断的标「中断」和续跑情况）。不拿锁。额度缓存过期时现读一次，只显示不写回。

### `agentq projects`

列出所有项目，每行目录名和路径，用制表符分开，`projects_root` 下的和 `extra_projects` 登记的都在。在 `exclude_projects` 里的多一列 `exclude_projects`。`dispatch`、`project-status` 技能和 Telegram 对话按名字找项目都用它。扫不了 `projects_root` 时列出看得到的，再报错退出 1。

### `agentq gate report <任务id>`、`agentq gate pass <任务id> --reason "原因"`、`agentq gate return <任务id> --reason "意见"`、`agentq gate rerun <任务id>`、`agentq gate abandon <任务id>`

`report` 看过门的状态、分支、轮数、每道门的结论和证据，过了成品门的还有交付单。`pass` 只放行这一次卡在门上的任务：下次 tick 不跑门，只合 `report` 里你看到的那个提交（之后分支又动了不算），写审计行和当天汇总，原因必写。放行后遇到冲突合不了，还是等你定，能再放行。`return` 带意见退回卡在门上的任务，意见必写：下次 tick 用同一会话接着改，意见原文接在门的报告后面进提示词，写进审计行；改完交付再过一遍门。`rerun` 重新跑一遍门，不沿用旧结论，不算退回轮数：看过结论后在分支上补了提交（带着交付单），再审就看分支最新的提交，放行也只合它；同一提交最多再审 2 次。`abandon` 放弃卡在门上的任务，或新项目第一版的任务（接手的、没过门的、new 任务本身都行），下次 tick 生效：分支留着，写审计行；新项目的第一条标卡住，后面的条目开工。和 Telegram 卡片的「放弃」是同一个操作。任务还在跑、在过门时拒绝。见[质量门和引擎合并](features.md#质量门和引擎合并)。

### `agentq health [项目名]`

最近 30 天每个模块的改动次数、修复次数、行数（模块是文件所在目录，根目录的文件各算一个；文档、测试、锁文件不算），标出再修就要重写的模块，再加两周返工率。不写项目名就看全部。只读，只跑 git，不调模型。见[补丁门](features.md#补丁门)。

### `agentq logs <任务id>`

任务的状态、会话 id（可以 `claude -r` 接着看）、目录、结果全文、stderr 末尾 40 行。

### `agentq stop <任务id>`

杀掉该任务的进程组，先 SIGTERM，10 秒后仍在就 SIGKILL，标为已停止。进程已经自己结束的，按它的结果正常收尾。还不知道 pid 时按任务找进程，找到就杀；确认没有进程就直接标已停止；查不了（`ps` 失败）时报错说没停成，任务照旧算在跑、占着名额。下一条待做会立刻接上，想停住先 `agentq pause`。停掉的这一条要等 `retry_after`（默认 12 小时）才会再自动接：stop 的回复和 `agentq status` 都写明到几点，以及现在就做用的 `agentq run` 命令。

### `agentq pause`、`agentq resume`

暂停或恢复自动接活。暂停时 tick 只收尾不启动，`run` 不受影响：你用 `run` 或 Telegram「做」启动的条目被门退回、中断了，tick 照样用同一会话接着做完。只改 `config.json` 里 `paused` 的值。

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

### `agentq eval judge --bin 路径 [--out 文件] [题号…]`

评测门用的那一步，也能手动跑：拿这一代编译进去的题和判分评判 `--bin` 那个 agentq，跑法是 `--fake` 加程序题，不调模型；再和基线比，比法同 `compare`。`--out` 写结论（JSON：跑了几题、没过的题、有没有能比的基线、变差、变好、提醒）。结果不进 `runs/`。变差退出码 1；没有能比的基线只记录，退出码 0。

### `agentq eval compare [run]`

和基线比，只比同一题同一种跑法。任一题分数掉、有检查项从过变没过、退回变多、要人插手变多、这次没结论，或者任一领域平均变差，都算变差，退出码 1，不看总平均。token 和耗时波动大，只提醒。

## 自我进化

tick 每小时从本机已经落盘的数据里收一次信号：门的结果（`~/.agentq/gate/<任务>/last.json`）、任务记录（`jobs/<任务>/meta.json`）、巡逻（`patrol.json`），门结果和任务记录按文件记，只读新出现或改过的；巡逻只看上次之后首次发现的问题。不调模型。这些都没有就什么都不写；任务记录在收尾、续跑时常被改写，有任务在动时一般每小时写一次。同一类问题（种类、门名、去掉路径、哈希、数字后的原因前 60 字）归成一条，同一个任务只算一次。存 `~/.agentq/evolve/signals.json`。

- **学到的约束：** 门 `error` 档没过、退回了任务，之后这一条合并了或你放弃了，算确认过的拦截，并进项目约束 `~/.agentq/evolve/constraints/<项目>.json`。`agentq gate pass` 放行的、再审过了的不算。每个项目最多 10 条，30 天没再出现的删掉。新任务的合同末尾带上这些（有出处），派活写条目前也看。
- **自我派活：** agentq 自己的问题（门判不了、你放行了门拦下的任务、退回用完还没过、门进程没留下结果、任务超时、巡逻发现）同一类出现 2 次以上（巡逻报的漏洞 1 次就算），写一条带证据的条目，同一类只写一次。配了 `self_project`、`self_grow_enabled` 开着（默认）时直接写进它的「待做」并提交，接着自动做，见下面「自己长大」；关着时写进「## 建议」，回「做 N」移进待做、「不做 N」以后不再提。没配 `self_project` 就写 `~/.agentq/evolve/proposals.md`，当天汇总记一行。

### 自己长大

配了 `self_project`（agentq 源码所在的项目）、`self_grow_enabled` 开着（默认）时，使用中遇到的问题从发现到用上修好的版本不用人插手。关掉 `self_grow_enabled` 和以前一样：只写「建议」，不升级。

1. **发现到动手：** 自身问题写进 `self_project` 的「待做」，tick 先于别的新条目起任务，记成自己长大派的（不是你亲手派的）：`self_project` 在 `exclude_projects` 里也做，门退回、中断了接着做；暂停时不起、不续跑，冷却、熔断、额度到线时也不起。这一条合并了记一次修好，这一类信号清零；再出现是回归，标「修过 N 次又出现」重新写；修过 2 次还出现，写成「重写」：条目要求按模块化规则重写这一块，`ready.json` 的 `type` 写 `rewrite`。不做清单里的不写。
2. **漏洞：** 巡逻每周一次用 `govulncheck -mode=binary` 查正在运行的 agentq（编进去的 Go 版本和依赖）。没装就跳过，`agentq status` 的巡逻那一行说明怎么装（`go install golang.org/x/vuln/cmd/govulncheck@latest`）；跑不成（没网）记原因，上次的结果照旧。只算真用到了有漏洞函数的，按模块报成巡逻问题，第一次报出就写进待做。
3. **修过就记住：** agentq 仓库里 `type` 是 `fix`、改了 Go 代码的交付，必须在 `evals/` 新加一道能复现它的题（新目录，只加不改），评测门才放行，见[评测门](features.md#评测门)。经验性的教训照样由「学到的约束」带进下一条任务的合同。
4. **修好就用上：** 引擎合并、评测门过了的合并提交，是现在这一代的后代、不带着回滚过的改动的，就要升级。有任务在跑、在过门（排队或在跑）时不接新活，等它们做完；挡满 2 小时还没空闲就照常接活，等下次自然空闲。空闲时 tick 起一个脱离自己的 `agentq evolve apply`，它先等前一个 tick 放开锁，再动安装引擎（安装那一步不设超时、不会中途杀掉）：在临时克隆里按这一代同样的构建标记构建，用新程序 `setup --yes`（和 `scripts/bootstrap.sh` 同一个安装引擎），新的一代装在新目录，旧的一代原样留着，任务、状态、配置不动。第一次升级前先在真实服务上演练一次整代回滚（用这一代自己再装一代、再 `rollback` 回来，演练前的回滚点 `previous.json` 演练完放回去），没过就不自动升级。每次安装后 `~/.local/share/agentq/generations/` 只留现在、上一代和最近 3 代。只认源码构建的这一代（版本 `dev-<提交>`）和安装引擎装的；发行版用 `agentq update`，或打开[自动更新](#自动更新)。
5. **观察期：** 升级后开始的 3 个任务结束，和升级前最近 5 个比：失败或超时至少 2 个且比例比升级前高，或者平均过门退回多 1 轮以上，算变差，空闲时 `rollback` 回上一代、推通知说原因，再往待做写一条去找、修让任务变差的改动。带着这个提交的版本（它和之后的合并）都不再升，直到修它的那一条合并进来；你自己装上带着它的版本也算放行。3 天凑不够 3 个就按已有的判。没变差记「留下」，之后照常看有没有新的可升。升级或回滚没做成记一行、推一条，6 小时后再试。

每次演练、升级、留下、回滚、没做成记一行 `~/.agentq/evolve/upgrades.jsonl`，当天汇总也记；`agentq evolve apply` 的输出在 `~/.agentq/evolve/upgrade.log`。

### 自动更新

发行版（一键安装的、版本不是 `dev` 开头）在 `~/.agentq/config.json` 写 `"auto_update": true` 打开，默认关。关着时 tick 不发任何网络请求，和没有这个功能一样。

- **看：** 开着时 tick 每天最多看一次发布地址，跑 `agentq update --check --saved-net`：取发布里的 `VERSION`，对过 `SHA256SUMS`（`scripts/build-release.sh` 产出的这两个文件）。按版本号比（`v1.2.3`、`v1.2.3-rc.1` 这样的），比现在新才装；认不出的版本号不比、不装。看不成只记原因，明天再看。
- **装：** 和自己长大第 4 步一样等空闲：有任务在跑、在过门（排队或在跑）时不接新活，等它们做完，挡满 2 小时就照常接活、等下次空闲；暂停时不看也不装。空闲时 tick 起 `agentq evolve apply`，它先等 tick 放开锁，再跑 `agentq update --saved-net --version <新版本>`：下载、对 `SHA256SUMS` 校验、运行安装脚本都是 `agentq update` 那一条路，安装那一步不设超时；新的一代装在新目录，任务、状态、配置不动。安装时用过的 `--proxy`、`--mirror` 照样用。
- **没装成：** 下载失败、校验不过不装，还是现在这一代；记一行、推一条，同一个版本只推一次，6 小时后再试。
- **观察期：** 和自己长大第 5 步一样：装上后开始的 3 个任务结束，和之前最近 5 个比，变差就 `rollback` 回上一代、推通知说原因，这个版本以后不再自动装，有更新的版本再说。没变差记「留下」。
- **不管的机器：** 源码构建的（`dev`、`dev-<提交>`，用 `git pull` 再 `sh install.sh`）、配了 `self_project` 的（由自己长大升级）、不是安装引擎装的（没有 `~/.local/share/agentq/receipt.json`）。
- **不做的事：** 不往外发任何使用数据，只下载 `VERSION`、`SHA256SUMS`、`install.sh` 和程序包；不改你的配置、任务和状态；不降级；不装没对上校验的东西；不替你打开（升级、重装都保留 `auto_update` 原来的值）。

`agentq status` 的「自动更新」一行说开没开、上次看的时间和结果、现在是不是最新；关着时收进「没开」。每次装上、留下、回滚、没装成和自己长大一样记进 `~/.agentq/evolve/upgrades.jsonl`（带 `release` 字段），`agentq evolve upgrades` 看；上次看的结果在 `~/.agentq/evolve/autoupdate.json`。

### 能力缺口和撤掉能力

- **按领域记账：** 同一次收信号里，每条结束了的待做按条目记一次领域（条目写的范围、合并提交改的文件，都没有再看条目文字）；返工意见、门 `error` 档没过（门点名的文件，没点名用退回范围，再没有用条目的领域）、超时、卡在门上记成这个领域的问题。领域一张表对上中文名和英文名：前端 frontend、后端 backend、设计 design、文档 docs、性能 performance、安全 security、数据 data，另有评测题用的产品 product、流程 process、安装 install、引擎 engine、例行 routine。
- **能力缺口：** 某领域近 30 天问题 3 次以上、问题率（问题 ÷ 任务和返工）25% 以上，且至少是其他领域合起来的 2 倍，写一条「能力缺口：<领域>」，带证据（任务号、门的结论、返工意见），走上面的自我派活（`self_project` 加自己长大进待做，否则进「建议」或 `proposals.md`），同一领域只写一次。条目要求照方案第 3a 节做：先按 `prior-art` 找现成做法，先写这个领域的考题、在当前这一代上判没过，再做成套能力（知识、`warn` 起步的检查、启用条件），登记进能力清单，`ready.json` 的 `type` 写 `capability`，过[评测门](features.md#评测门)。这一条合并后这个领域的记录清零；再出现是新的缺口，标「补过 N 次又出现」。
- **能力有没有帮上忙：** [能力门](features.md#能力门)每次记下用上了哪项能力、检查报没报问题。检查报了问题、同一条后来修好了，或用上这项能力的任务过门合并了，算帮上忙，记下最近一次的证据；报了问题照样合并或被放弃，算放过。
- **撤掉能力：** 一项能力连续 60 天没帮上忙（从加进来那天算），或检查报出 4 次以上、一半以上被放过（误报多），写一条「撤掉能力：<名字>」，同一项只写一次。不自动删文件：核实后删它的知识、检查和清单里这一项，考题留着。只是时间到了、没有别的新数据也会写。

### `agentq evolve signals`

看收到的信号（种类、次数、项目、最近一次、证据）、等确认的门拦截有几次、哪些已经提案，按领域的账（任务、返工、门没过、超时卡住、问题率，够得上的标「缺口」），和能力清单里每项能力最近一次帮上忙的证据、检查报出和放过的次数。只读。

### `agentq evolve constraints <项目名>`

看这个项目学到的约束，和新任务合同末尾带的是同一份。只读。

### `agentq evolve upgrades`

看自己升级开没开、为什么没起作用、现在的观察期（升级后几个任务、和升级前比），和每次演练、升级、留下、回滚的记录。只读。

### `agentq evolve apply`

现在就照上面的规则做一次升级（第一次先演练）或回滚。没有要做的、有任务在跑、已经有一个在做时什么都不做。tick 空闲时自动起它，一般不用手动跑。

## 统计

### `agentq stats [天数]`

最近 N 天（默认 7，1 到 3650）按任务种类、项目、模型统计次数、成败和 token。末尾是最近 7 天的一次满意率、返工率，按项目列出每条合并的条目算成了什么、依据是什么。只读。见[每个任务的用量](features.md#每个任务的用量和-agentq-stats)、[一次满意率和返工率](features.md#一次满意率和返工率)。

## 维护项目

### `agentq backlog archive`

```sh
agentq backlog archive <项目名|--all> [--dry-run]
```

把 `docs/BACKLOG.md`「已完成」的旧条目按原顺序移到 `docs/BACKLOG-archive.md` 顶部。留下：最前面 20 条、合并提交在 14 天内的、查不到合并时间的验收意见（满意率的 7 天窗口不受影响）。BACKLOG 或归档文件有未提交改动、项目有任务在跑、条目标着 24 小时内的进行中时拒绝。不自动提交。`--dry-run` 只打印会移多少、瘦到多大。

## 定点任务和巡逻

### `agentq night`、`agentq review`

立刻执行一次夜班或复盘。手动执行不看 `night_enabled`、`review_enabled`，也不过无事跳过。服务定点叫醒时带 `--scheduled`，开关为假就只记一行日志，确认没事也不起 claude。见[夜班和复盘](features.md#夜班和复盘)。

### `agentq patrol`

立刻巡逻一次，列出现有的每个问题和证据、处理说明。不看 `patrol_enabled` 和上次巡逻时间；有新异常时的只读分析要 `patrol_enabled` 开着。见[巡逻](features.md#巡逻)。

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
