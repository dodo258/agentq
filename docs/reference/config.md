# 配置参考

配置文件是 `~/.agentq/config.json`。每次命令启动时读一次，开关类的改动下次 tick 或叫醒时生效，不用重启。

## 三种默认值

同一个键可能有三种值，先分清你是哪一种：

| 情况 | 行为 |
| --- | --- |
| 配置里缺这个键 | 用下表「缺键默认」，已有配置升级后新增的键也走这条 |
| 安装器新建配置（`agentq setup`、`scripts/bootstrap.sh`） | 在缺键默认之上显式写入 `paused=true`、`night_enabled=false`、`review_enabled=false`、`patrol_enabled=false`、`patrol_analyze=false`、`notify_enabled=false`、`permission_mode=default`，`model`、`effort`、`router_model`、`suggest_model`、`sense_model`、`light_review_model` 写成空字符串（跟随你在 CLI 里选的），`claude_path` 写检测到的绝对路径 |
| 配置文件不存在时跑了普通命令（如 `agentq status`） | 和安装器新建的一样：`paused=true`、`permission_mode=default`，模型和强度留空，夜班、复盘、巡逻、推送都关。`claude_path` 是命令名 `claude` |

已有配置文件逐字节保留，安装和升级不改。JSON 写错或校验不过时命令报错退出，不覆盖文件。`agentq pause`、`resume` 只替换 `paused` 的值，不认识的键也留着。

时长写成字符串，如 `"4h"`、`"30m"`。美元金额写数字。

## 基础

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `projects_root` | 字符串 | `~/projects` | 扫这个目录下一层，不能为空。默认的 `~/projects` 不存在算没有项目；自己配的目录不存在每次 tick 都报，可能是盘没挂上，`extra_projects` 里的项目照跑，`agentq status` 显示扫描失败 |
| `extra_projects` | 字符串数组 | 空 | `projects_root` 外的项目，写绝对路径。`agentq add` 在外面的仓库里运行时自动登记。见下面「外面的项目」 |
| `claude_path` | 字符串 | `claude` | Claude CLI 路径，不能为空 |
| `model` | 字符串 | `opus`，新配置是空 | 传给 `--model`。空字符串不传，跟随你在 CLI 里选的。也决定哪个只限模型的周窗口计入额度；空的时候不知道实际用哪个，这种窗口不计入，撞了靠撞限冷却兜住 |
| `effort` | 字符串 | `high`，新配置是空 | 传给 `--effort`。空字符串不传 |
| `model_policy` | 字符串 | `fixed` | `fixed` 照 `model` 和 `low_risk_model`。`auto` 按任务类型选，见下面「按任务类型选模型」 |
| `permission_mode` | 字符串 | `bypassPermissions` | 传给 `--permission-mode`。可选 `default`、`manual`、`acceptEdits`、`auto`、`dontAsk`、`plan`、`bypassPermissions`。空字符串表示不传。含义和风险见 [SECURITY](../../SECURITY.md#权限模式) |
| `launchd_label` | 字符串 | `io.agentq.agentq` | 服务名前缀，macOS 和 Linux 都用这个键。要是点分隔的字母、数字、下划线或连字符 |
| `paused` | 布尔 | `false` | 为真时 tick 只收尾不启动。`agentq run` 不受影响 |
| `exclude_projects` | 字符串数组 | 空 | 这些项目自动流程都不碰，写目录名。见下面「排除项目」 |
| `retention_days` | 整数 | `90` | 任务记录这些运行数据留多少天，tick 每天清一次。`0` 表示不删。见[运行数据](files.md) |
| `discuss_enabled` | 布尔 | `true` | 标了 `（大方向）` 的条目和 `agentq new` 开工前问一次。关掉就都按默认直接开工。见[大方向商量](features.md#大方向商量) |
| `discuss_wait` | 时长 | `2h` | 问了之后等多久，没回就按默认开工。夜里（23:00–08:00）卡片发不出去，从能发的时候算。要大于 0 |
| `backend` | 字符串 | 不写就是 `claude` | 用哪个 CLI 后端。写了不认识的名字不回退：任务不启动，`agentq status`、`agentq doctor` 说明。能力表用 `agentq doctor` 看，接新后端见[这里](../backends.md) |
| `project_backends` | 对象 | 空 | 按项目换后端，键是项目目录名。放用户配置，不放项目仓库。续跑用任务原来的后端，不跟随之后的配置 |
| `backends` | 对象 | 空 | 每个后端自己的配置，键是后端名，值可以有 `path`、`model`、`effort`、`permission`。顶层的 `model`、`effort`、`permission_mode` 只归默认后端，别的后端只看这里；空的不传，跟随 CLI。`permission` 和 `permission_mode` 的合法值由后端声明。默认后端不是 Claude，或项目用了默认以外的后端时，权限空着或是 `default` 任务不起，`status` 和 `doctor` 会说要写什么 |

### 按任务类型选模型

`model_policy` 默认 `fixed`，因为降级模型省额度却可能做坏。写成 `auto` 后，只有普通待做任务会换模型，夜班、复盘、对话、`new` 都用 `model`：

- 标题提到重写、重构、迁移、审查、审计（`rewrite`、`refactor`、`migrat`、`review`、`audit`）的不降级，带 `（低风险）` 也不降。
- 带 `（低风险）` 标记，或标题以 `文档：`、`docs:` 开头的，用 `low_risk_model`；没配就用后端自己说的轻模型（Claude 是 `sonnet`）。
- 其余用 `model`。

### 外面的项目

项目不用移动。在 `projects_root` 外的 Git 仓库里运行 `agentq add`，它把仓库主目录写进 `extra_projects`，打印一行说明，之后和 `projects_root` 下的项目一样接活。

- agentq 按目录名认项目。和已有项目重名的不登记，报错让你改名；手写进配置的重名项，`projects_root` 下的赢，`agentq status` 说明哪些没在看。
- `exclude_projects` 优先：写了目录名就不进自动流程，明确点名的 `agentq run` 照样能跑。
- 目录不在了或不再是 Git 主目录时跳过，不报错，`agentq status` 说明。
- 夜班、复盘的工作目录还是 `projects_root`，提示词也按它写，不看外面的项目。闲时找活的自动那次和不带名字的 `agentq suggest` 只看 `projects_root` 下，要看外面的就点名 `agentq suggest <名字>`。

### 排除项目

`projects_root` 下可能有公开仓库或不想交出去的项目，只要有待做就会被接。列进 `exclude_projects` 后：

- tick 不接它的待做、不续跑、不处理它的进行中标记；巡逻、推送不看它的 BACKLOG 和决定项，不写接手待做；闲时找活、主动感知不写它，提示词不带它；Router 拒绝动它，也不发它的文件。
- `agentq run` 跑的任务是你明确要的：它的失败、卡住照常报告和推送，收尾时读这一条的状态判断合没合。过门、合并和合并后的收尾（BACKLOG 和 PROGRESS）照常做，这是你点名要的；门没过时不退回、不写卡住标记，等你处理。巡逻不接手，也不为它起模型分析。
- 夜班、复盘、Telegram 对话、Router 和它派的小事是模型按提示词做的，提示词写明跳过，这部分是**软约束**：模型能读写这个用户的所有文件，判断出错时碰了也没有东西拦得住。不想被碰到的项目，放到 `projects_root` 外面，或用单独的用户跑 agentq。见 [SECURITY](../../SECURITY.md)。
- `agentq run <它>` 明确指定时仍可跑，提示一句，中断了不自动续跑。`agentq add --project <它>`、`agentq init` 照做，提示 tick 不会接。
- `agentq status` 列出被排除的项目，不算进待做。还不存在的项目名不算错。

## 调度

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `max_parallel` | 整数 | `2` | 全局同时运行的任务数，至少 1 |
| `per_project` | 整数 | `2` | 同一项目同时运行的任务数，至少 1。第二条起要条目写了互不重叠的 `（范围：…）` 才并行，没写范围的一次一条，见[挑活](scheduling.md#挑活) |
| `timeout` | 时长 | `4h` | 待做任务超时，到时杀掉整个进程组，大于 0 |
| `retry_after` | 时长 | `12h` | 同一条目再试的间隔，大于 0 |
| `limit_cooldown` | 时长 | `1h` | 撞用量上限又解析不出重置时间时的冷却时长，大于 0 |
| `tick_after_job` | 布尔 | `true` | 任务结束时马上 tick 一次接下一条。为假时只靠定时 tick |
| `stall_after` | 时长 | `45m` | 运行中的任务会话记录这么久没增长，标疑似卡死（只提醒，不杀），大于 0 |
| `progress_autostop` | 布尔 | `true` | 子代理失控时自动停。关掉只报不停 |
| `low_risk_model` | 字符串 | 空 | 非空时，带 `（低风险）` 标记的待做任务用这个模型 |

## 熔断

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `breaker_streak` | 整数 | `3` | 连续这么多个任务秒退就熔断，至少 1 |
| `breaker_quick` | 时长 | `3m` | 运行不到这么久就结束且没做完，算秒退，大于 0 |
| `breaker_cooldown` | 时长 | `30m` | 第一次熔断暂停启动的时长，大于 0 |
| `breaker_max` | 时长 | `4h` | 熔断后再秒退时长翻倍的上限，不能小于 `breaker_cooldown` |

## 额度和轻量流程

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `usage_stop_session_pct` | 数字 | `70` | 会话窗口（5 小时）用到这个百分比，tick 不领新活，等窗口重置；留出的给你自己的会话。1 到 97 |
| `usage_stop_week_pct` | 数字 | `90` | 周窗口用到这个百分比，tick 不领新活。1 到 97。`agentq run` 不受这两个管，到 98% 才拒绝 |
| `light_review_model` | 字符串 | `sonnet`，新配置是空 | 改动小、局部、好撤回时（不算测试和记录文件最多 3 个文件、60 行，不是 rewrite，没改指令文件和门脚本），审查和界面审查用这个模型，强度照旧 `medium`；空就照 `model`。写别名（如 `sonnet`），新模型出来自动跟上，不写带版本号的模型 ID。不能用 haiku，不能以 `-` 开头 |

## 预算

单位美元，传给 `--max-budget-usd`，按单次运行算。

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `job_budget_usd` | 数字 | `150` | 待做、`agentq new`、任务箱。0 表示不传，不能是负数 |
| `night_budget_usd` | 数字 | `10` | 夜班。0 表示不传，不能是负数 |
| `review_budget_usd` | 数字 | `15` | 复盘。0 表示不传，不能是负数 |
| `patrol_budget_usd` | 数字 | `2` | 巡逻分析。0 表示不传，不能是负数 |
| `suggest_budget_usd` | 数字 | `1` | 闲时找活。大于 0，不超过 1000 |
| `sense_budget_usd` | 数字 | `1` | 主动感知判断。大于 0，不超过 1000 |
| `release_budget_usd` | 数字 | `30` | 发版准备。大于 0，不超过 1000 |

## 定点任务和巡逻

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `night_enabled` | 布尔 | `false` | 每天 01:00 叫醒时是否执行夜班 |
| `review_enabled` | 布尔 | `false` | 每天 06:30 叫醒时是否执行复盘 |
| `night_timeout` | 时长 | `6h` | 夜班超时，大于 0 |
| `review_timeout` | 时长 | `4h` | 复盘超时，大于 0 |
| `patrol_enabled` | 布尔 | `true` | tick 是否每小时巡逻一次。`agentq patrol` 不看它 |
| `autopush_log` | 字符串 | 空 | 外部自动推送脚本的日志路径，可写 `~`。配了巡逻才查推送失败 |
| `patrol_analyze` | 布尔 | `true` | 巡逻有新异常时是否启动一次 `claude -p` 分析原因，每天最多 3 次 |

## 推送和对话

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `notify_enabled` | 布尔 | `true` | 是否推送和收 Telegram 消息。关掉同时停止两者。没配 Telegram 时只弹本机通知。安装器新建的配置先写 `false`，在 `agentq start --welcome` 第一次同意时打开；升级前就有的配置里写的 `false` 不动。先只安装、后来用 `agentq start` 启动的不会打开，要收通知就把它改成 `true` |
| `router_enabled` | 布尔 | `false` | 新消息改由只读的 Router 处理 |
| `router_model` | 字符串 | `sonnet`，新配置是空 | Router 用的模型。空字符串不传，走后端默认。不能以 `-` 开头 |

## 可选功能

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `suggest_enabled` | 布尔 | `false` | 闲时找活。关着时不起任何进程 |
| `suggest_model` | 字符串 | `sonnet`，新配置是空 | 闲时找活用的模型。空字符串不传，走后端默认。不能以 `-` 开头 |
| `sense_enabled` | 布尔 | `false` | 主动感知。关着时不发请求、不起进程、不建文件 |
| `sense_model` | 字符串 | `sonnet`，新配置是空 | 判断外部反馈用的模型。空字符串不传，走后端默认。不能以 `-` 开头 |
| `evolve_signals_enabled` | 布尔 | `true` | tick 每小时从门结果、任务记录、巡逻收自我进化的信号，只读本机文件，不调模型。关着时不收、不写文件，见[自我进化](commands.md#自我进化) |
| `learned_constraints_enabled` | 布尔 | `true` | 确认过的门拦截记成项目约束，接在新任务合同末尾。关着时不学，合同一字不变 |
| `self_project` | 字符串 | 空 | agentq 源码所在的项目目录名。配了，agentq 自己反复出的问题写成那个项目「## 建议」里的提案；空的只写 `~/.agentq/evolve/proposals.md` |
| `release_assist_enabled` | 布尔 | `false` | 发版助手。关着时不发请求、不起进程、不建文件，回复「发」也不提交 |
| `verify_after_merge` | 布尔 | `true` | 老配置键，不再起作用。合并后核验并进了质量门，留着是为了已有配置不变 |
| `verify_timeout` | 时长 | `30m` | 门里每个脚本、每次审查的超时，大于 0。超时算判不了 |

## 不在配置文件里的设置

| 位置 | 作用 |
| --- | --- |
| `~/.agentq/secrets/telegram.env` | Telegram 机器人凭据，权限必须是 600，见 [Telegram 指南](../guide/telegram.md) |
| `~/.appstoreconnect/config.env` | App Store Connect 的 `ASC_KEY_ID`、`ASC_ISSUER_ID`、`ASC_KEY_PATH`，主动感知和发版助手用 |
| `~/.agentq/backup.json` | 可选代码备份的目标，格式见[安装指南](../install.zh-CN.md) |
| `.agentq/verify`（项目内） | 测试门跑的检查命令，见[质量门和引擎合并](features.md#测试门) |
| `.agentq/gates.json`（项目内） | 调每道门的档位和算界面文件的后缀，见[质量门和引擎合并](features.md#质量门和引擎合并) |
| `.agentq/ui`、`.agentq/smoke`（项目内） | [界面门](features.md#界面门)的截图命令、[成品门](features.md#成品门)的烟测 |
| `AGENTQ_PROJECTS_ROOT`、`AGENTQ_SHARE_DIR` | 技能读的环境变量，分别指项目根目录和安装资产目录，默认 `$HOME/projects`、`$HOME/.local/share/agentq` |
