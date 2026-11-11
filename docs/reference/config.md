# 配置参考

配置文件是 `~/.agentq/config.json`。每次命令启动时读一次，开关类的改动下次 tick 或叫醒时生效，不用重启。

## 三种默认值

同一个键可能有三种值，先分清你是哪一种：

| 情况 | 行为 |
| --- | --- |
| 配置里缺这个键 | 用下表「缺键默认」，已有配置升级后新增的键也走这条 |
| 安装器新建配置（`agentq setup`、`scripts/bootstrap.sh`） | 在缺键默认之上显式写入 `paused=true`、`night_enabled=false`、`review_enabled=false`、`patrol_enabled=false`、`notify_enabled=false`、`permission_mode=default`，`model`、`effort` 写成空字符串（跟随你在 CLI 里选的），`claude_path` 写检测到的绝对路径 |
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
| `permission_mode` | 字符串 | `bypassPermissions` | 传给 `--permission-mode`。可选 `default`、`manual`、`acceptEdits`、`auto`、`dontAsk`、`plan`、`bypassPermissions`。空字符串表示不传。含义和风险见 [SECURITY](../../SECURITY.md#权限模式) |
| `launchd_label` | 字符串 | `io.agentq.agentq` | 服务名前缀，macOS 和 Linux 都用这个键。要是点分隔的字母、数字、下划线或连字符 |
| `paused` | 布尔 | `false` | 为真时 tick 只收尾不启动。`agentq run` 不受影响 |
| `exclude_projects` | 字符串数组 | 空 | 这些项目自动流程都不碰，写目录名。见下面「排除项目」 |
| `retention_days` | 整数 | `90` | 任务记录这些运行数据留多少天，tick 每天清一次。`0` 表示不删。见[运行数据](files.md) |
| `discuss_enabled` | 布尔 | `true` | 标了 `（大方向）` 的条目和 `agentq new` 开工前问一次。关掉就都按默认直接开工。见[大方向商量](features.md#大方向商量) |
| `backend` | 字符串 | 不写就是 `claude` | 用哪个 CLI 后端。写了不认识的名字不回退：任务不启动，`agentq status`、`agentq doctor` 说明。能力表用 `agentq doctor` 看，接新后端见[这里](../backends.md) |
| `project_backends` | 对象 | 空 | 按项目换后端，键是项目目录名。放用户配置，不放项目仓库。续跑用任务原来的后端，不跟随之后的配置 |
| `backends` | 对象 | 空 | 每个后端自己的配置，键是后端名，值可以有 `path`、`model`、`effort`、`permission`。顶层的 `model`、`effort`、`permission_mode` 只归默认后端，别的后端只看这里；空的不传，跟随 CLI。`permission` 和 `permission_mode` 的合法值由后端声明。默认后端不是 Claude，或项目用了默认以外的后端时，权限空着或是 `default` 任务不起，`status` 和 `doctor` 会说要写什么 |

### 按活分配模型

每类调用按活的性质用固定的模型和强度，依据是官方对各档模型的定位：Sonnet 做范围清楚的日常编码、知识工作和看图；Opus 做长时间自主编码和大重构。强度 `medium` 给范围清楚的活，`high` 给要验证、边界多的活，`xhigh` 给超过半小时的长任务。审查最低 Sonnet + `medium`，不用 Haiku。

下表只在 `model` 非空、后端是 Claude 时生效。`model` 为空（跟随 CLI，安装器新建的就是）或用别家后端时，这些活都照 `model`、`effort`。

| 活 | 模型和强度 |
| --- | --- |
| 开工前出大方向问题 | `sonnet`、`medium` |
| 带 `（低风险）` 的待做任务 | `sonnet`、`medium` |
| 标题提到重写、重构、迁移，或标了 `（难撤回）` 的待做任务 | 模型照 `model`，强度 `xhigh` |
| 复盘（写教训和早报） | `sonnet`、`high` |
| 巡逻分析异常原因 | `sonnet`、`medium` |
| Telegram 回复链里的只读答问；要动手改代码的对话照 `model` | `sonnet`、`medium` |
| 小改动的审查和界面审查（不算测试和记录文件最多 3 个文件、60 行，不是 rewrite，没改指令文件和门脚本）。完整流程的审查用 `model`：局部改动 `medium`，影响面广、难撤回 `high` | `sonnet`、`medium` |

标题提到重写、重构、迁移、审查、审计（`rewrite`、`refactor`、`migrat`、`review`、`audit`）的不换成轻模型，带 `（低风险）` 也不换。夜班、对话、`agentq new` 和其余待做任务用 `model`、`effort`。

`model` 写别名（`opus`、`sonnet`），Claude Code 会让别名指向最新版，新模型出来自动跟上；写带版本号的完整 ID（如 `claude-opus-5-5`）就钉死了，`agentq doctor` 会提醒。

### 外面的项目

项目不用移动。在 `projects_root` 外的 Git 仓库里运行 `agentq add`，它把仓库主目录写进 `extra_projects`，打印一行说明，之后和 `projects_root` 下的项目一样接活。

- agentq 按目录名认项目。和已有项目重名的不登记，报错让你改名；手写进配置的重名项，`projects_root` 下的赢，`agentq status` 说明哪些没在看。
- `exclude_projects` 优先：写了目录名就不进自动流程，明确点名的 `agentq run` 照样能跑。
- 目录不在了或不再是 Git 主目录时跳过，不报错，`agentq status` 说明。
- 夜班、复盘的工作目录还是 `projects_root`，提示词也按它写，不看外面的项目。

### 排除项目

`projects_root` 下可能有公开仓库或不想交出去的项目，只要有待做就会被接。列进 `exclude_projects` 后：

- tick 不接它的待做、不续跑、不处理它的进行中标记；巡逻、推送不看它的 BACKLOG 和决定项，不写接手待做；自我进化不往它的「建议」写提案。例外是 `self_project`：自己长大照样往它的「待做」写 agentq 自己的问题并起任务，见[自己长大](commands.md#自己长大)。
- `agentq run` 跑的任务是你明确要的：它的失败、卡住照常报告和推送，收尾时读这一条的状态判断合没合。过门、合并和合并后的收尾（BACKLOG 和 PROGRESS）照常做，这是你点名要的；门没过时不退回、不写卡住标记，等你处理。巡逻不接手，也不为它起模型分析。
- 夜班、复盘、Telegram 对话是模型按提示词做的，提示词写明跳过，这部分是**软约束**：模型能读写这个用户的所有文件，判断出错时碰了也没有东西拦得住。不想被碰到的项目，放到 `projects_root` 外面，或用单独的用户跑 agentq。见 [SECURITY](../../SECURITY.md)。
- `agentq run <它>` 明确指定时仍可跑，提示一句。这样亲手启动的条目被门退回或中断了，tick 会用同一会话接着做；排除前自动起的任务中断了不自动续跑。`agentq add --project <它>`、`agentq init` 照做，提示 tick 不会接。
- `agentq status` 列出被排除的项目，不算进待做。还不存在的项目名不算错。

## 调度

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `max_parallel` | 整数 | `2` | 全局同时运行的任务数，至少 1 |
| `per_project` | 整数 | `2` | 同一项目同时运行的任务数，至少 1。第二条起要条目写了互不重叠的 `（范围：…）` 才并行，没写范围的一次一条，见[挑活](scheduling.md#挑活) |
| `timeout` | 时长 | `4h` | 待做任务超时，到时杀掉整个进程组，大于 0 |
| `retry_after` | 时长 | `12h` | 同一条目再试的间隔，大于 0 |
| `limit_cooldown` | 时长 | `1h` | 撞用量上限又解析不出重置时间时的冷却时长，大于 0 |

## 额度

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `usage_stop_session_pct` | 数字 | `70` | 会话窗口（5 小时）用到这个百分比，tick 不领新活，等窗口重置；留出的给你自己的会话。1 到 97 |
| `usage_stop_week_pct` | 数字 | `90` | 周窗口用到这个百分比，tick 不领新活。1 到 97。`agentq run` 不受这两个管，到 98% 才拒绝 |

## 预算

单次运行有美元上限，传给 `--max-budget-usd`，只防失控，不在配置里：待做和 `agentq new` 150，夜班 10，复盘 15，巡逻分析 2。

## 定点任务和巡逻

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `night_enabled` | 布尔 | `false` | 每天 01:00 叫醒时是否执行夜班 |
| `review_enabled` | 布尔 | `false` | 每天 06:30 叫醒时是否执行复盘 |
| `patrol_enabled` | 布尔 | `true` | tick 是否每小时巡逻一次，有新异常时起一次只读分析（每天最多 3 次）。`agentq patrol` 巡逻不看它，分析看它 |
| `autopush_log` | 字符串 | 空 | 外部自动推送脚本的日志路径，可写 `~`。配了巡逻才查推送失败 |

## 推送和对话

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `notify_enabled` | 布尔 | `true` | 是否推送和收 Telegram 消息。关掉同时停止两者。没配 Telegram 时只弹本机通知。安装器新建的配置先写 `false`，在 `agentq start --welcome` 第一次同意时打开；升级前就有的配置里写的 `false` 不动。先只安装、后来用 `agentq start` 启动的不会打开，要收通知就把它改成 `true` |

## 自我进化

| 键 | 类型 | 缺键默认 | 作用 |
| --- | --- | --- | --- |
| `self_project` | 字符串 | 空 | agentq 源码所在的项目目录名。配了，agentq 自己反复出的问题写进那个项目（`self_grow_enabled` 开着时进「待做」接着做，关着时进「## 建议」等你回「做 N」）；空的只写 `~/.agentq/evolve/proposals.md`。见[自我进化](commands.md#自我进化) |
| `self_grow_enabled` | 布尔 | `true` | 自己长大：配了 `self_project` 时，自身问题直接进待做接着做，合并过了评测门的修复空闲时用安装引擎升级自己，之后几个任务变差就回滚。关掉和以前一样：只提案、不升级。没配 `self_project` 时开着也不起作用。见[自己长大](commands.md#自己长大) |
| `auto_update` | 布尔 | `false` | 自动更新：发行版每天最多看一次发布地址，有更新的版本就在空闲时照 `agentq update` 装上（沿用安装时的代理和镜像），之后几个任务变差就回滚、这个版本不再自动装。关着时不联网。源码构建、配了 `self_project` 的机器不管。见[自动更新](commands.md#自动更新) |

## 不在配置文件里的设置

| 位置 | 作用 |
| --- | --- |
| `~/.agentq/secrets/telegram.env` | Telegram 机器人凭据，权限必须是 600，见 [Telegram 指南](../guide/telegram.md) |
| `~/.agentq/backup.json` | 可选代码备份的目标，格式见[安装指南](../install.zh-CN.md) |
| `.agentq/verify`（项目内） | 测试门跑的检查命令，见[质量门和引擎合并](features.md#测试门) |
| `.agentq/gates.json`（项目内） | 调每道门的档位和算界面文件的后缀，见[质量门和引擎合并](features.md#质量门和引擎合并) |
| `.agentq/ui`、`.agentq/smoke`（项目内） | [界面门](features.md#界面门)的截图命令、[成品门](features.md#成品门)的烟测 |
| `AGENTQ_PROJECTS_ROOT`、`AGENTQ_SHARE_DIR` | 技能读的环境变量，分别指项目根目录和安装资产目录，默认 `$HOME/projects`、`$HOME/.local/share/agentq` |
