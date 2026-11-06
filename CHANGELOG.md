# 更新记录

只写用得到的变化。

## v0.1.0

### 评测

- `agentq eval`：评测集（`evals/`）和基线。`run` 默认只跑不调模型的程序题，`--fake` 用假 CLI 走完全部题，`--real` 才调模型；`compare` 任一题或任一领域变差就报。留出题放 `~/.agentq/evals/heldout/`。

### 后端

- Codex CLI 后端（`{"backend":"codex"}` 或 `project_backends`）：提示词走标准输入，会话从事件流取，能续跑；审查用 `workspace-write` 沙箱；技能在 `~/.agents/skills`，规则是 `AGENTS.md` 里带标记的一段。能力表和没有的东西见 `docs/backends.md`。
- `backends.<名字>`：每个后端自己的路径、模型、强度、权限。模型和权限不再串给别的后端。

### 质量门

- 任务只做到分支可合并，写 `.agentq/ready.json`；引擎在合并态上跑测试门和审查门，过了才合并，并写 BACKLOG、PROGRESS。合并后核验删除，`verify_after_merge` 不再起作用，`verify_timeout` 改作门的超时。新增 `agentq gate report|pass`，项目用 `.agentq/gates.json` 调档位。独立审查由引擎起进程，后端不再要求子代理（`doctor` 的能力表改成「引擎审查」）。

- 三道新门，默认只记录，`.agentq/gates.json` 可以调成挡合并。补丁门：同一模块 30 天内第三次修复记下来，调成 `error` 就退回，合同改成重写。界面门：改到界面文件才跑，脚本截图，查不是空白，再交给审查进程，只有渲染坏、溢出、看不清、占位内容能阻塞。成品门：`agentq new` 的第一版和标了 `（成品）` 的条目才跑，烟测过了才有交付单。
- `agentq health [项目]`：每个模块最近 30 天的改动、修复次数、行数，两周返工率。
- `agentq new` 的第一条不再自己合并，和别的条目一样过门。结尾第四行从「合并提交」换成「任务分支」。第一版没合并时这个项目后面的条目不开工，`agentq status` 写明卡在哪和解法。
- 界面门的审查走任务的后端：Claude 读文件看截图，Codex 用 `--image` 附上；后端看不到图片算判不了，不放行。
- `agentq gate abandon <任务>`：放弃卡在门上的任务或新项目的第一版，分支留着。
- 任务动 `docs/design/approved/`（用户确认过的界面基线）过不了测试门。

- 大方向商量：标题带 `（大方向）` 的条目和 `agentq new` 开工前问一次，最多 3 个问题，每个带默认答案。Telegram 卡片等 `discuss_wait`（默认 2 小时）没回按默认，别的条目不等；没配 Telegram 直接按默认，不调模型。`agentq discuss` 看和答。方向写进条目，审查门看得到。
- `agentq stats` 按门统计通过、失败、判不了、warn 档会挡下多少、退回和放行。早报多一节：放行、放弃、warn 没过的、关着的门。
- 门调模型撞上游 5xx：退避 15 分钟、1 小时重跑，3 次都不行才判不了。之前重试一次就卡住。
- `agentq gate rerun` 和卡片的「再审一次」：不改代码重新跑门，不算退回轮数，同一提交最多 2 次。
- `gate/reviews/` 随 `retention_days` 清，在用的和卡住的不清。门上卡住的任务记录不再 30 天过期。

### 上手

- 默认分支自动认：`origin/HEAD` 指的分支，没有就看本地 `main`、`master`，不再要求叫 `main`。认不出的项目不起任务，`agentq status` 和日志写明原因。
- 做完告诉你：任务合并后推一条，带合并提交和验收方法；失败、中断、卡住也说。一键脚本里第一次同意启动时打开本机通知，配了 Telegram 发 Telegram；已有配置里写的 `notify_enabled=false` 不动。
- 一条命令安装和升级：`install.sh` 检查 Git 和 Claude CLI，下载程序并校验 SHA256，配置和任务数据保留。发第一个 Release 后可以用 curl 直接运行，在此之前 clone 后 `sh install.sh`。
- 新安装的最后一步讲清楚任务能做什么，按回车同意并启动，输入 n 只安装。没有终端或在 CI 里只安装不启动；`--yes` 是明确同意。同意记在 `~/.agentq/consent.json`。
- 安装脚本加 `--proxy`、`--mirror`（校验文件不走镜像）、`--uninstall`、`--version`，拒绝 root，终端里转圈，遵守 `NO_COLOR`。
- 通用工程规则随安装带：第一次同意时放到 `~/.claude/rules/agentq/`，前台会话和后台任务都读得到。已有全局规则的不放；`agentq env status|on|off` 管理；改过的文件升级和卸载都不动。
- 第一次 `agentq add` 自动接入项目，不用先 `init`；项目不用放到 `~/projects` 下，在外面就登记到 `extra_projects`。没有 `~/projects` 时 tick 不再报错。
- 接手老项目时按 `go.mod`、`package.json`、`Cargo.toml`、`pyproject.toml`、`Package.swift` 生成 `.agentq/verify`，判断不了就不写。核验脚本退出 125 表示这次跑不了，不算失败。
- 新配置的模型和强度留空，跟随你在 CLI 里选的；已有配置照旧。可选 `model_policy=auto` 按任务类型选，重写、审查类不降级。
- 只有桌面端或什么都没有也能走完：同意后运行官方命令装 Claude CLI、登录，没登录不开后台。官方脚本装不上就停下，贴原文，给 Homebrew 和 npm。
- `~/.local/bin` 不在 PATH 时，同意后往 zsh、bash 的配置文件加一行带标记的 export，改前备份，卸载时只删这一行。
- `agentq update` 升级：取发布地址上的 `install.sh`，校验后运行。
- `agentq init` 把项目接入，`agentq add` 加一条待做，`agentq start` 一步打开后台。
- `agentq start` 讲清完全权限的风险再问你。不同意就什么都不改。
- `agentq version` 查版本。
- 没有 Python 也能装，只有可选第三方技能的辅助脚本要用。

### 配置和后台

- 后端接口：`backend` 选 CLI 后端（默认 `claude`），写错不回退，任务不启动；`project_backends` 按项目换。目前只有 Claude 一个实现，`agentq doctor` 打出能力表，接新后端见 `docs/backends.md`。
- `exclude_projects`：列出的项目 tick、闲时找活、主动感知都不碰，`agentq run` 明确指定时仍可跑。
- 磁盘清理：`launchd.log` 超过 10MB 只留尾部；任务记录超过 `retention_days`（默认 90 天，`0` 不删）的 tick 每天清一次。你的产出 `outbox/` 不动。
- Telegram：`agentq notify setup` 配好凭据后自动打开推送；`agentq start` 把常驻监听一起装上，也可以 `setup --services --listen`。
- 配置文件不存在时跑普通命令，新写出的配置和安装器一致：暂停、权限 `default`、后台开关全关。已有配置一个字节都不动。
- 权限是 `default` 时，`agentq run` 和 `agentq doctor` 会提醒任务写不了文件。
- `agentq status` 说出谁拿着 `tick.lock`、拿了多久：tick 卡住时不再只写「调度：正常」。
- `state.json` 和任务的 `meta.json` 同时坏时，tick 重建状态会杀掉这个任务还活着的进程并写进当天汇总，不再让它没人管，也不会同一条起两个。
