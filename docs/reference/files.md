# 文件参考

## 平台差异

| 用途 | macOS | Linux |
| --- | --- | --- |
| 服务管理 | launchd，`launchctl` | `systemd --user`，`systemctl --user` |
| 服务文件 | `~/Library/LaunchAgents/<label>*.plist` | `~/.config/systemd/user/<label>*.{service,timer}` |
| 看服务状态 | `launchctl print gui/$(id -u)/<label>` | `systemctl --user status <label>.timer` |
| 日志文件 | `~/.agentq/launchd.log` | 同名文件 |
| 配置键 | `launchd_label` | 同名键 |
| 开机时间 | `kern.boottime` | `/proc/stat` 的 `btime` |

文档里写的 launchd 在 Linux 上对应 `systemd --user`。`<label>` 默认是 `io.agentq.agentq`。

## `~/.agentq/`

状态文件内容坏了时多数会备份成 `<文件名>.bad-<时间>` 再重置或重建，详情在各行。读不了（权限、I/O 错误）一律当未知，不覆盖。

| 路径 | 内容 | 坏了怎么办 |
| --- | --- | --- |
| `config.json`、`config.json.lock` | 配置和改 `paused` 时的锁，见[配置参考](config.md) | 命令报错，不覆盖，等你修 |
| `consent.json` | 同意启动的记录：文案版本、时间、方式（`enter`、`assume-yes`、`prompt`、`accept-bypass`）。已有就不覆盖 | 只影响 `doctor` 的说明，不影响任务 |
| `state.json` | 任务列表、用量冷却和熔断的截止时间、连续秒退计数、最近一次额度读数、条目等待（`item_hold`）、夜班和复盘上次看到的现状（`routine_marks`） | 备份，按 `jobs/*/meta.json` 重建，见[状态文件损坏](scheduling.md#状态文件损坏) |
| `tick.lock` | 防止两个 tick 重叠（定时 tick、任务结束后的 tick、`run`、`stop`、`backlog archive` 共用） | 没有持有者超时 |
| `jobs/<id>/` | `prompt.txt`、`result.json`（claude 的 JSON 输出）、`stderr.log`、`meta.json`（任务记录，结束后带用量）、`claude.pid`（外壳写的 pid）、`files/`（对话任务收到的附件） | 读不出的 meta 跳过。结束的任务记录保留 30 天 |
| `starting/<id>` | 正在启动的任务记录，tick 被杀后据此接回 | |
| `launchd.log` | 每次 tick 的日志（包括保护脚本记的失败和回滚）、定点叫醒的日志、任务外壳和任务结束后那次 tick 的日志。超过 10MB 时只留尾部 2MB | |
| `retention-day` | 今天清理过了，见下面「磁盘清理」 | 当今天没清过 |
| `tick-guard.sh`、`guard-fails` | 服务调用的保护脚本和它记的连续失败次数，见[保护脚本](#保护脚本) | |
| `inbox/` | 从 Telegram 收到、还没交给 claude 的附件 | |
| `suggestions.json`、`suggestions.lock` | 「## 建议」的编号索引，见[建议](features.md#建议) | 备份，当空的 |
| `discuss/<编号>.json`、`discuss/<编号>.answer` | 一次大方向商量：问题、默认答案、到点时间、结果；用户的回答（下次 tick 收走）。保留 90 天，还在商量的不清 | 坏了就不再问，按默认开工 |
| `gate/<任务id>/` | 过门的 `plan.json`、`result.json`（下次 tick 收走后改名 `last.json`）、`decision.json`（卡片和 `agentq gate pass` 的决定）、门的输出 `test*.log`（权限 600，每个最多 2MB）、`smoke.log`、`ui.log`、评测门的 `eval-build.log`、`eval.log` 和结论 `eval/verdict.json`、截图 `ui-artifacts/`、交付单 `delivery.md`、`review-<次>/`、`ui-review-<次>/`、进程组号 `pgid`、活着的锁 `lock`，跑的时候还有一次性 worktree `wt/` | |
| `gate/reviews/<项目>/<tree>.json`、`gate/reviews/<项目>/ui/<tree>.json` | 审查门、界面门模型部分的结论，绑定 tree hash。保留 90 天，任务还在记录里（在跑、在过门、卡住的）不清 | 删了就重审 |
| `gate/audit.log` | 人工放行、再审、退回、放弃的记录。进早报 | |
| `evals/runs/<时间>/results.json`、`evals/baseline.json` | `agentq eval` 的结果和基线，只有明确运行才写 | |
| 系统临时目录下的 `aq-*` | 没通过的题留下的现场（有标记文件 `.agentq-eval`），下次 `agentq eval run` 清掉超过 7 天的 | |
| `evals/heldout/<题号>-<短名>/` | 留出题，和仓库 `evals/` 同样的格式，自己放，`list` 只显示数量 | |
| `verify/<任务id>/` | 升级前合并后核验留下的，只等过期清掉 | |
| `patrol.json` | 巡逻的现有问题（指纹、证据、处理、首次和最后发现时间）、上次巡逻时间、当天分析次数 | 备份一次，从空的开始，报过的问题会再报一次 |
| `notify.json`、`notify.lock` | 推送队列、各类上次发送时间、今天推了几条、哪天推过早报、见过的待决定条目、Telegram 的 offset、排队等回答的消息、没发出去待补发的回答、回复链 `threads`、决策卡片 `cards`、最近的推送错误（不含 token） | 备份一次，从空的开始 |
| `notify/` | 超过 12 行的推送全文（`<时间>-<类别>.txt`）、压缩后的录屏 `media/` | |
| `replies/<任务id>.md` | 超过 600 字的回答全文 | |
| `listen.lock`、`listen.pid` | 常驻监听拿着的锁和它的 pid。tick 据此知道它在不在跑 | |
| `secrets/telegram.env` | Telegram 凭据（权限必须是 600，agentq 只读不写） | 权限不对就不用，`agentq status` 报原因 |
| `secrets/telegram-setup.sh` | 可选，`agentq notify setup` 发现它就执行 | |
| `backup.json` | 可选代码备份的目标 | |
| `backup-report.txt`、`watchdog-report.txt`、`backup.log`、`watchdog.log` | 可选备份和看门狗的本地报告和日志 | |
| `private/` | 维护者本机的隐私规则，不分发，见[清洁源](../privacy.md) | |

### 磁盘清理

tick 每天最多跑一次清理，不调模型，删不掉只记日志，不影响 tick。

- `launchd.log` 超过 10MB 只留尾部 2MB。在原文件上截断，常驻服务还开着它。
- `jobs/<任务id>`、`gate/<任务id>`、`verify/<任务id>`、`replies/<任务id>.md` 超过 90 天的删掉。
- 不删：`state.json` 还引用的（运行中、等续跑、`HeldProjects`、`StuckPending` 和最近 30 天结束的任务）、门的 worktree 还在的、名字不是 agentq 格式的。
- 全程不跟软链接：这些目录本身是软链接就不进去，条目是软链接就不动。
- 时钟跳变不误删：天数从「现在」和记录里最新时间中较早的那个算起，比现在还晚的记录不算过期。所以长期没有新任务时不清理，有新任务后才清。如果时钟跳到未来之后还留着并生成了新记录，这条保护就失效了。

## 项目内

| 路径 | 内容 |
| --- | --- |
| `docs/BACKLOG.md` | 待办，语法见[调度规则](scheduling.md#backlog-语法) |
| `docs/BACKLOG-archive.md` | `agentq backlog archive` 移来的旧条目，新的在前 |
| `docs/NOT-DOING.md` | 不做清单 |
| `.agentq/verify` | 测试门跑的检查命令，任务合同要求有 |
| `.agentq/gates.json` | 可选，调每道门的档位和算界面文件的后缀，读默认分支上的 |
| `.agentq/risk.json` | 可选，追加难撤回、影响面广的路径规则，读默认分支上的，见[按改错了多难撤回放行](features.md#质量门和引擎合并) |
| `.agentq/ui`、`.agentq/smoke` | 界面门的截图命令、成品门的烟测，示例在安装资产的 `templates/gates/` |
| `docs/design/approved/` | 用户确认过的界面基线，只认默认分支上的，引擎和任务都不更新，任务动它测试门不过 |
| `.agentq/ready.json` | 只在任务分支上：任务的交付单，不进任务基于的分支 |
| `.claude/worktrees/` | `agent-task` 默认的任务 worktree 位置 |
| `AGENTS.md`、`PROGRESS.md`、`DECISIONS.md`、`LESSONS.md` | 模板生成的项目说明和交接文件 |

## `~/.claude/`

| 路径 | 内容 |
| --- | --- |
| `nightly/<日期>-agentq.md` | 当天汇总，每个结束的任务一行，加上续跑启动、额度变化、熔断、疑似卡死、状态文件损坏、回滚、巡逻发现和恢复、定点任务结束或没能启动等记录，末尾是自动重写的「当天花费」和质量小结 |
| `nightly/<日期>.md` | 复盘写的早报 |
| `nightly/<日期>-night.md` | 夜班报告 |
| `nightly/<日期>-backup.md` | 外部备份脚本写的报告（可选，巡逻会读） |
| `state/report-pushed-<日期>` | 早报已推到 Telegram 的标记 |
| `state/locks/` | `with-lock.sh` 和 `<项目>-main` 锁。默认后端不是 Claude 时在 `~/.agentq/locks/`，`~/.agentq/nightly/`、`~/.agentq/state/` 同理，不往 `~/.claude` 写 |
| `skills/<名字>/` | 安装器链接的技能入口 |
| `rules/agentq/*.md` | 通用工程规则，普通文件，可以改，改过的升级和卸载都不动。见 [`agentq env`](commands.md#agentq-env-statusonoff) |
| `projects/*/<会话id>.jsonl` | Claude Code 的会话记录，agentq 只读，用来判断卡死和审查证据 |

自定义 `CLAUDE_CONFIG_DIR` 时安装器拒绝激活默认投影，不会自动迁移。

## 安装资产

| 路径 | 内容 |
| --- | --- |
| `~/.local/bin/agentq` | 受管安装时是指向当前代的链接 |
| `~/.local/share/agentq/generations/gen-*/` | 每一代的完整内容（程序、技能、模板、支持脚本、许可证） |
| `~/.local/share/agentq/current`、`templates`、`support`、`licenses`、`dependencies` | 指向当前代的链接 |
| `~/.local/share/agentq/receipt.json` | 安装回执，记录所有权和摘要 |
| `~/.local/share/agentq/shell-path.json` | 改过哪个 shell 配置文件、备份在哪，卸载时用 |
| `~/.local/share/agentq/env-layer.json` | 通用工程规则的状态和每个文件写入时的哈希，不随 `rollback` 回滚 |
| `~/.local/bin/agentq.prev`、`agentq.bad` | 旧版 binary-only 安装才有：上一个版本和自动回滚换下来的版本，见 [legacy-install](../legacy-install.md) |

## 服务

| 服务 | 由谁装 | 行为 |
| --- | --- | --- |
| `<label>` | `--services` | 每 5 分钟 tick，装上时先跑一次 |
| `<label>.night` | `--services` | 每天 01:00 `agentq night --scheduled` |
| `<label>.review` | `--services` | 每天 06:30 `agentq review --scheduled` |
| `<label>.backup` | `--backup` | 每 86400 秒 `agentq maintenance backup` |
| `<label>.watchdog` | `--watchdog` | 每 1800 秒运行 `support/scripts/watchdog.sh` |
| `<label>.listen` | `--listen`，或 `agentq start` 在 Telegram 配好时 | 常驻 `agentq listen`，见 [Telegram 指南](../guide/telegram.md#常驻监听) |

Linux 上所有 service 都写 `KillMode=process`，任务是 tick 拉起的后台进程，默认的 `control-group` 会在 tick 退出时把它们一起杀掉。journal 里的 "Found left-over process" 提示不影响运行。定时任务写 `StartLimitIntervalSec=0`。

### 保护脚本

服务调用 `~/.agentq/tick-guard.sh`，由它执行 `agentq tick`。tick 连续 2 次非 0 退出时，受管安装尝试通过上一代程序执行整代回滚；活跃任务、用户修改或未知状态会阻止替换，没有上一代时不能回滚。正常的「上一个 tick 还没结束，本次跳过」、暂停、冷却、没登录都返回 0，不计数。所以 `config.json` 写错也会触发这条路径（旧版本读同一个配置照样失败）。看到回滚记录先看 `launchd.log` 里的原因。任务结束后外壳直接执行的 `agentq tick --after` 不经保护脚本，它失败了由下一次定时 tick 收尾。
