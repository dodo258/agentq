# 安装完整 Claude 工作系统

[English](install.en.md) · [维护指南](../AGENTS.md) · [给 Agent 的入口](../llms.txt)

## 一条安装命令

新老用户同一条：

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

装好，开口说：新安装装完直接是运行状态（服务加载、不暂停），不问也不等输入，安装本身就是同意；结束时如实说一句后台任务能读写本机文件、运行命令、用你自己的模型额度，并给出暂停、卸载命令。之后打开 Claude Code 或 Codex，在项目里直接说要做什么。想先不跑：加 `--paused`。


`install.sh` 检查平台和 Git，取到程序后调用同一个 `agentq setup` 安装引擎，安装程序、完整技能目录、支持脚本、五模板及固定第三方依赖；不要求复制作者的 `.claude`，不需要 Node、sudo，以 root 运行时在写任何文件之前拒绝。官方 CLI 没装或没登录不挡安装，交给 `agentq start --welcome`。升级重跑同一条或运行 `agentq update`，不问，配置、任务数据、已选服务和暂停状态都不变：暂停着的不会被打开。

装完即运行，有没有终端、在不在 CI 里都一样；只有 `--paused`（`AGENTQ_PAUSED=1`）只装不跑，不记同意。旧的 `--yes` 还认，不再有作用。装之前就有的配置写着暂停（老版本只装二进制的用户）时保留暂停，不替你打开。有终端时：没有 CLI 就运行官方命令 `curl -fsSL https://claude.ai/install.sh | bash`，失败就停，贴原文，给 `brew install --cask claude-code` 和 `npm install -g @anthropic-ai/claude-code`；没登录就运行 `claude auth login`，没登录不开后台。没有终端（CI、由 agent 代装）时不装 CLI、不登录，只打印命令，退出非 0；同意和权限已经记下，CLI 弄好后在聊天里说需求，派活技能查到后台没开会自己 `agentq start`。`~/.local/bin` 不在 PATH 时往 shell 配置文件加一行带 `# agentq managed` 标记的 export，改前备份成 `*.pre-agentq`（已有备份就加时间戳），`agentq remove --yes` 只删这一行，没别的改动时逐字节恢复，恢复卸载不把这一行加回来。fish 等别的 shell 不改，打印手动的那一行。同意记在 `~/.agentq/consent.json`（文案版本、时间、方式；装完即运行的方式是 `install`），文案改版不重问，只在 `agentq doctor` 里说明。第一次运行时把通用工程规则放到 `~/.claude/rules/agentq/`，`~/.claude/CLAUDE.md` 或 `~/.claude/rules/` 里已有你自己的内容就不放，装完的说明里多一行；升级不自动加，见 `agentq env`。派活技能装到 Claude Code 的 `~/.claude/skills`，本机用着 Codex（有 `~/.codex` 或 `$CODEX_HOME`）时也装到 `~/.agents/skills`；后来才装 Codex 的，`agentq update` 补上，`agentq doctor` 会提醒。

取程序的顺序：本仓 checkout 里本地构建；下载 Release 并校验 SHA256（校验不过直接退出）；下载不到且有 Go 时拉源码构建。`--version` 或 `AGENTQ_VERSION` 选版本，`main` 直接走源码；`AGENTQ_RELEASE_BASE` 换下载地址，它优先于脚本里的仓库名。`--proxy <地址>`（`AGENTQ_PROXY`）让下载走代理。`--mirror <前缀>`（`AGENTQ_MIRROR`）让 GitHub 上的程序包从镜像取，校验文件和源码仍从原地址取，镜像改过的文件会被拒绝；镜像回 404 时退到源码构建也是克隆原地址；锁定的第三方依赖也走镜像，同样按锁定哈希核对。`--uninstall` 转给 `agentq remove --yes`，数据保留；程序已被手动删掉时如实说明还留着什么，不假装卸干净。

维护者从源码安装也可以用 `sh scripts/bootstrap.sh --yes`，新安装同样装完即运行（`--paused` 只装），下文的 `--plan`、`--extras` 等参数两个入口通用（如 `sh install.sh --extras`）。

用 `--paused` 装的，之后运行 `agentq start`，它会讲清完全权限的风险并由你确认；或者在 Claude Code、Codex 里说「开后台」。

给 Agent 的一句话：**请阅读本仓 `llms.txt`，一条命令装好 agentq。** 装完就在运行；不想马上跑就让它加 `--paused`。

## 安装前检测

- macOS 13+，arm64 或 amd64。当前验收主机为 macOS arm64；amd64 构建可交叉检查，但尚未在 Intel Mac 实机验收。
- Linux amd64 或 arm64，后台服务需要 `systemd --user`（见下文「Linux 与 WSL」）。验证范围是 GitHub Actions 的 ubuntu-latest。
- Git。Go（版本要求见 `go.mod`）仅用于源码构建，运行调度器不需要。Python 3 可选：调度器和必需技能不用它，只有可选 Waza 的辅助脚本要用，缺了只给一行警告。安装器检测缺项后给出处，不静默安装系统软件。
- 可独立调用的 Claude Code CLI（启动时要有，安装不要求）：检测真实路径、`--version`、print/JSON/session/effort 能力和 CLI 任务环境中的 `auth status`。可用 `--claude /absolute/path/to/claude` 明确选择，路径可含空格。CLI-only 可用，不需要 Desktop。已有配置中的命令名（如 `claude`）按 PATH 解析；已有 `claude_path` 与 `--claude` 不同时拒绝安装，先由安装者明确修改自己的配置，不会只探测新引擎却继续运行旧引擎。
- 未找到 CLI 时，有终端就运行官方安装命令；没有终端时只打印 `curl -fsSL https://claude.ai/install.sh | bash` 与 `claude auth login`，由安装者执行。安装器不索要 token，不读取 Desktop 私有 bundle 路径。检测到 Desktop、上传技能 ZIP 或打开桌面会话均不算后台引擎可用。
- 选择 `--services` 时，再按服务文件（macOS 的 plist、Linux 的 unit）声明的 HOME/PATH 做服务环境探测，不继承安装 shell 的 token 或代理变量。只依赖临时 shell 身份时拒绝服务安装；安装者需明确配置持久的 CLI 认证/网络设置。安装器不把秘密写进服务文件、不代登录。
- 认证检测只证明 CLI 在该环境识别身份，不证明远端模型请求必然成功；真实模型验收单独执行。用户自己的受支持网关/模型映射属于本机配置，安装器不迁移或切换收费入口。父进程 API 环境变量不自动传入任务，详见 [环境变量白名单](reference/scheduling.md#环境变量白名单)。
- `settings.json`/全局 local 设置只校验 JSON 和常用结构，绝不重写；更全面的宿主诊断可用官方 `claude doctor`。当前安装投影使用默认 `$HOME/.claude`；自定义 `CLAUDE_CONFIG_DIR` 不会被自动迁移。

只有缺项时才按提示补齐前置工具，再重复同一条安装命令；不用手工拼装技能、Hook、脚本或服务。

## 范围和清单

| 范围 | 入口 | 安装内容 | 不包含 |
|---|---|---|---|
| 完整系统（基础能力） | `sh scripts/bootstrap.sh --yes` | agentq、8 个自有技能完整目录、4 个必需 Waza 技能、templates/support/license/lock | 自动接活、Telegram、作者规则/权限/身份、第三方缓存 |
| 完整扩展能力 | 同一入口加 `--extras` | 基础内容 + Waza read/write/learn/health | 未核实许可或来源的扩展 |
| 仅技能 | 同一入口加 `--scope skills` | 完整技能闭包、支持文件、模板与许可证 | 后台二进制、服务；不要求 Claude 登录 |
| Claude 插件 | 上游宿主插件安装方式，见 lock | 用户自行选择的宿主能力 | 不是 agentq 后台安装；不接管插件 cache |
| Desktop 技能 ZIP | 桌面宿主的技能上传范围 | 只代表桌面技能能力 | 不构建/安装后台、不证明 CLI 可用；目前不发布 ZIP |
| 可选调度服务 | 完整入口加 `--services` | tick 每 5 分钟、night 01:00、review 06:30，使用当前配置 | 不自动 resume；不装 Telegram listen |
| Telegram 消息监听 | 再加 `--listen`（要同时有 `--services`）；`agentq start` 在 Telegram 凭据配好且 `notify_enabled=true` 时自动带上 | 常驻的 listen 服务，升级时保留 | 不配凭据、不打开推送 |
| 可选代码备份 | 完整入口加 `--backup` | 每 86400 秒运行一次的用户级备份服务 | 无显式备份目标就不写远端；不改全局 Git Hook |
| 可选看门狗 | 完整入口加 `--watchdog` | 每 1800 秒运行一次的独立 shell 看门狗、本地报告 | 不恢复调度、不发送消息、不改权限 |

自有技能：agent-task、dispatch、nightly-review、new-project、project-status、no-ai-look（含 references）、prior-art、feedback-triage。五模板：AGENTS、BACKLOG、PROGRESS、DECISIONS、LESSONS。支持：with-lock、ask-rule-scan、watchdog。

必需 Waza：check、think、hunt、ui。来源、完整 commit、许可证和逐文件 Git blob 哈希见 `dependencies/claude.lock.json`。安装器只取声明文件，验证哈希，保留 MIT 许可证，不执行上游安装脚本、不复制 vendor/cache。`--extras` 安装声明中可选 Waza 目录；其他 frontend-design/iOS/Swift/Obsidian/shaders/文档插件仍有能力与来源声明，未核实的明确不自动安装，不因本阶段整理删掉用户已有模块。

## 计划、所有权与定制

```sh
sh scripts/bootstrap.sh --plan
```

先检测并构建可自检的候选，再打印计划，不激活入口。计划列出具体变化路径、前后类型及事务备份位置；选用 backup 时额外检测 gh 的路径与版本，远程身份和目标授权在实际备份时核实。**准备计划会联网获取锁定的公开依赖并执行自己的临时 HOME 自检**，不执行第三方安装脚本，不改当前技能/服务/配置。`--yes` 确认该范围的安装文件变化；服务旗标是额外显式选择，不授予账号/发布/付款等权限。

- 厂商资产位于 `~/.local/share/agentq/generations/gen-*/`。受管二进制与技能入口是指向该代的链接；receipt 记录所有权和整代摘要。
- 用户定制保留在项目规则、用户规则、独立技能/Hook/状态栏和本机配置，不直接编辑厂商 generation。它们不随代码回滚。
- 已有同名技能：完整内容及权限位相同则保留为外部所有，卸载不删除。内容不同时，`check` 这类技能用你自己的版本，不替换、不接管，安装时说一声；任务合同靠的 `agent-task`、`dispatch`、`new-project`、`nightly-review` 顶替不了，报告冲突、停止激活，原件不变，改个名再装。相同的外部技能在回执中记录依赖摘要，doctor 会报告随后丢失或修改，但不会因此接管或删除它们。不替用户删重、选旧版或覆盖手改内容。
- 已受管技能链接或 generation 有本地修改、未知文件/服务同名、非法 JSON/常用配置结构错误时停止，保留冲突证据。不把原子替换说成“自动保留定制”。
- 既有配置逐字节保留，包括未知字段、第三方 Hook/状态栏和开关。新配置先写 `paused=true`，night/review/patrol/notify 全关、`permission_mode=default`，`start --welcome` 接着把它改成运行状态：取消暂停、权限放开、打开 notify（`--paused` 时都不改）；不继承作者 bypass 权限。旧版缺省 bypass 的配置兼容语义保留，不静默改现有策略。
- 安装/更新与 tick 共用锁；有运行任务或未知状态时拒绝替换。启动前做临时 HOME status/tick 自检。正在执行的文件不被就地覆盖或删掉。
- 激活前保存受限权限事务备份，逐路径写入；失败恢复旧文件/服务，崩溃则保留 pending 并拒绝继续，使用 restore 恢复。活动、上一代和卸载恢复回执共用完整校验；不完整回执拒绝恢复，不能把缺失字段当成删除指令。没有受管回执时 doctor 明确报未安装。数据、身份、秘密不进入安装事务备份。

遇到冲突时先按计划路径备份并比较，由用户在自己的规则层调整或明确决定迁移；安装器没有“强制覆盖所有定制”开关。

## 使用：不配置 Telegram

在任何目录打开 Claude Code 或 Codex，直接说开发需求，不用说「派活」；宿主根据标准 skill description 发现 dispatch，由它判断当场做（只是问问、改错字改文案这类零碎改动、你说「直接改/我看着」）还是写进 BACKLOG 交后台（加功能、修 bug、改界面，哪怕很小）。没接入的目录它自己接入（不是 Git 仓库就 `git init` 并做初始提交，`.gitignore` 挡住密钥和依赖；不在 projects_root 下就登记 `extra_projects`）；家目录、桌面这类不像项目的地方先问一句项目名和位置。它先跑只读的 `agentq start --check`：服务没在跑就 `agentq start` 拉起，你自己暂停的不打开。`agentq run/status/logs` 是它替你跑的管理命令，不是聊天窗口。

`agentq run <项目名> <关键词>` 是明确启动，即使自动调度暂停也会尝试运行，仍遵守认证/额度/并发限制。未授权无人值守工具时，默认权限可能拒绝写入；安装不会替用户改权限，用户应按自己的项目授权选择允许的权限策略。

本地结果：`~/.agentq/jobs/<ID>/`、`agentq logs <ID>`、`~/.claude/nightly/<日期>-agentq.md`（默认后端不是 Claude 时在 `~/.agentq/nightly/`）。不用 Telegram 也完整生成。Telegram 配置和 `listen` 是用户之后主动启用的通道；安装入口不索要机器人 token、不自动装监听、不发送外部消息。配好凭据并把 `notify_enabled` 改成 `true` 后运行 `agentq start`，会一起装上监听。完整系统退出桌面/终端窗口后仍由已安装的 launchd 服务运行。

## 升级、回滚、卸载与恢复

- 升级：重新运行同一安装入口，或受管安装中运行 `scripts/update.sh`。会构建候选、保留原来的可选模块/服务选择并重新比较；不覆盖用户配置。未受管旧版 `update.sh` 保留原 binary-only 兼容流程，不算完整安装。
- 整代回滚：`agentq rollback --yes`，恢复上一代 binary/skills/support/templates/service 入口；配置和任务状态不回滚。回滚目标被消耗，避免在两版间反复摆动。
- 卸载：`agentq remove --yes`（`agentq uninstall --yes` 同义）。删掉 shell 配置文件里带标记的那一行，只移除回执拥有且仍一致的入口及服务，保留配置、任务、秘密、generations 和备份。用户后来修改受管文件时先停，不丢内容。
- 恢复卸载：`sh scripts/bootstrap.sh restore --yes`；或卸载时打印的保留代 `bin/agentq restore --yes`。不需要重新拼装技能。skills-only 同样可由 bootstrap 恢复。
- 中断恢复：`sh scripts/bootstrap.sh restore --yes` 优先撤回 pending 事务；发现用户在中断后修改过冲突路径时停止并保留备份，不强盖。
- 受管 tick 连续两次非 0，保护脚本尝试通过上代二进制执行整代回滚；活跃任务、用户修改或未知状态会阻止自动替换。仅服务加载失败不会丢掉旧定义。无上代时不能凭空回滚；watchdog 留本地报告。
- 不自动清理旧代、秘密或用户数据。卸载不是抹除数据；保留代的磁盘清理由用户另行确认。

可选备份目标必须由用户在本机 `~/.agentq/backup.json` 明确授权：`{"targets":[{"project":"/absolute/project","remote":"backup","url":"https://example.invalid/owner/repo.git"}]}`。每次检查 remote push URL 未变且 gh 识别为 PRIVATE，才推分支和 tags；没配目标不推，未知/公开/URL 改变均拒绝。实际 URL 由用户填写，不使用样例地址。看门狗和备份报告在 `~/.agentq/*-report.txt`，默认只在本地。

## Linux 与 WSL

安装命令相同：`sh scripts/bootstrap.sh --yes`，`setup`、`doctor`、`rollback`、`remove`、`restore` 都走同一个引擎。不选服务时不需要 systemd，容器里也能装技能和手动跑 `agentq tick`、`agentq run`。

**服务（`--services`、`--watchdog`、`--backup`、旧入口 `agentq install`）用 `systemd --user`，要求 systemd 240 或更新：**

| 服务 | Linux 上的形态 | 对应 macOS |
| --- | --- | --- |
| tick | `<label>.timer`（每 5 分钟，装上时先跑一次）+ oneshot `<label>.service` | StartInterval 300 + RunAtLoad |
| 夜班、复盘 | `.timer` 定点 + oneshot `.service`；不写 `Persistent`，关机错过由 tick 补跑 | StartCalendarInterval |
| Telegram 监听 | 常驻 `.service`，`Restart=always`、`RestartSec=10` | KeepAlive、ThrottleInterval 10 |
| 备份、看门狗 | `.timer` 按间隔 + oneshot `.service` | StartInterval |

unit 文件在 `~/.config/systemd/user/`，由收据管理；同名的非 agentq 文件不会被覆盖。默认 label 是 `io.agentq.agentq`，配置项名字仍叫 `launchd_label`，日志文件名仍叫 `~/.agentq/launchd.log`（Linux 上一样用，只是沿用名字）。所有 service 都写 `KillMode=process`：任务是 tick 拉起的后台进程，默认的 `control-group` 会在 tick 退出时把它们一起杀掉。代价是 journal 里会有 “Found left-over process” 这类提示，不影响运行。

- **linger：** 没开时，你退出登录后用户服务和正在跑的任务都会停，再登录才恢复。agentq 不替你开，`agentq doctor` 和安装结果会提示；要一直跑就自己执行 `loginctl enable-linger`。
- **`systemctl --user` 不可用**（WSL 没开 systemd、容器、ssh 或 su 进来没有用户会话）：`setup --services`、`install`、`uninstall`、`doctor` 都明确报错并说明原因，不写文件、不静默跳过。
- **`XDG_CONFIG_HOME`：** 只认 `~/.config/systemd/user`。如果你把 systemd 用户配置目录改到别处，`enable` 会失败并回滚。
- **本机通知：** 有 `notify-send` 和桌面会话就弹。没有 `notify-send` 时只写日志（`~/.agentq/launchd.log`）。有但发不出（比如没有桌面会话）时不算送达，消息留在队列，每小时再试一次。要推到手机配 Telegram。
- **媒体：** 录屏超过 50 MB 要 `ffmpeg` 才能压缩，没有就如实发一句带路径的话。
- **要 Mac 的能力：** iOS 构建、Xcode、模拟器、代码签名、TestFlight 上传都要 macOS。Linux 上的 agentq 能调度任务，但这类任务必须放在 Mac 上跑。

**WSL 未经验证。** 没有在真实 WSL 环境跑过，不声明支持。理论上 WSL2 启用 systemd 后走的是同一条 Linux 路径：在 `/etc/wsl.conf` 写 `[boot]` 和 `systemd=true`，`wsl --shutdown` 后重开，确认 `systemctl --user status` 能用。已知限制：WSL 在没有打开的窗口时可能被关掉，定时器和任务会跟着停；时钟可能和 Windows 不同步（开机时间相差超过 60 秒且任务已退出时，任务按重启中断处理）；Windows 原生不支持。

## 验证边界与官方资料

常规离线测试：`go test ./...`，全部临时 HOME/假 CLI/假 launchctl/假 systemctl。真实 systemd 用户服务验收 `TestSystemdAcceptance` 只在 CI runner 上跑（动真实用户服务和家目录，没有 `GITHUB_ACTIONS` 就拒绝）。显式网络入口验收：`AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`。真实模型闭环：`AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...`；连接型身份只有另显式 `AGENTQ_REAL_E2E_CONNECTION=1` 时才把调用方现有连接环境最小投影到临时 HOME，用后删除，不读取真实 HOME。会消耗已授权模型额度，不默认在 CI 跑。

能力和验证范围见[支持矩阵](reference/support-matrix.md)。后端支持 Claude Code、Codex 和 pi（`--backend codex`、`--backend pi`，不写是 Claude Code），其他 CLI 以后再加。没有宣称 Intel 实机、WSL、公开远程下载或正式 Release 已通过。

官方文档（核对于 2026-10-13）：[CLI 安装与 stable 通道](https://code.claude.com/docs/en/setup)、[CLI 参数与 auth status](https://code.claude.com/docs/en/cli-reference)、[GitHub CLI repo view](https://cli.github.com/manual/gh_repo_view)。原生 Claude 自动更新保留自定义 launcher，但普通 launcher 仍由官方管理；npm 支持自动更新，Homebrew 默认通过包管理器手动更新。这些策略不等于保留所有用户定制；agentq 不覆盖它们，不接管用户自定义 CLI launcher。
