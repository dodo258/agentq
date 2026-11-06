# 配置

配置文件是 `~/.agentq/config.json`。改完不用重启，下次 tick 生效。每个键的类型和默认值见[配置参考](../reference/config.md)。

写错 JSON 时命令会报错退出。服务里的 tick 连续两次失败会触发回滚保护，所以改完跑一次 `agentq status` 确认。

## 要让任务能写文件

新安装的权限是 `default`，任务写不了文件。`agentq start` 会在你同意后改好。也可以手动改，先读 [SECURITY](../../SECURITY.md)：

```json
{ "permission_mode": "bypassPermissions" }
```

## 要换项目目录

```json
{ "projects_root": "~/work" }
```

agentq 只扫这个目录下一层、`.git` 是目录的 Git 主目录，跳过以 `.` 开头的目录。个别项目在外面不用改它：在那个仓库里运行 `agentq add`，会自动登记到 `extra_projects`，见[外面的项目](../reference/config.md#外面的项目)。

## 要同时跑更多任务

```json
{ "max_parallel": 3, "per_project": 1 }
```

`per_project` 大于 1 时，同一项目会并行多个任务，每个任务在自己的 worktree 里，但合并时可能冲突。

## 要选模型和强度

新配置的 `model`、`effort` 是空的，任务跟随你在 CLI 里选的，不传参数。想给后台任务单独定：

```json
{ "model": "sonnet", "effort": "medium", "job_budget_usd": 30 }
```

`job_budget_usd` 是单次运行的兜底，撞到后保留成果并接着做一次。夜班、复盘、巡逻分析各有自己的预算，见[配置参考](../reference/config.md#预算)。

便宜的活用便宜的模型：配 `low_risk_model`，在条目标题里写 `（低风险）`。

```json
{ "low_risk_model": "sonnet" }
```

想让 agentq 按任务类型选，写 `"model_policy": "auto"`，重写、审查类不降级，见[按任务类型选模型](../reference/config.md#按任务类型选模型)。默认不开：降级省额度，但可能做坏。

用 `agentq stats` 看实际花费，见[用量统计](../reference/features.md#每个任务的用量和-agentq-stats)。

## 要调整失败重试

```json
{ "retry_after": "6h", "timeout": "2h" }
```

`retry_after` 不要配得太短：任务结束会立刻 tick，遇上 claude 一启动就失败时会以秒级反复拉起。连环失败熔断（`breaker_*`）会挡住这种情况，见[连环失败熔断](../reference/scheduling.md#连环失败熔断)。

## 有项目不想交出去

`projects_root` 下的项目只要有待做就会被接。公开仓库或不想交出去的，写进排除名单：

```json
{ "exclude_projects": ["my-public-repo"] }
```

tick、闲时找活、主动感知都不碰它，`agentq status` 会列出来。`agentq run my-public-repo` 明确指定时仍可跑。详情见[配置参考](../reference/config.md#排除项目)。

## 要开夜班和复盘

需要服务（`agentq start` 或 `--services` 装好）。开关每次叫醒时现读，不用重装。

```json
{ "night_enabled": true, "review_enabled": true }
```

`agentq status` 的「定点任务」一行应显示「夜班 01:00 开」「复盘 06:30 开」。第二天早上看 `~/.claude/nightly/<日期>-night.md` 和 `<日期>.md`。

如果 Claude 桌面端另有同类定时任务，先停用它，否则会跑两次。详情见[夜班和复盘](../reference/features.md#夜班和复盘)。

## 要开巡逻和推送

```json
{ "patrol_enabled": true, "patrol_analyze": false, "notify_enabled": true }
```

巡逻本身不花额度。`patrol_analyze` 打开后，有新异常时会起模型分析，每天最多 3 次。推送的通道配置见 [Telegram](telegram.md)。

## 要开可选功能

都默认关，关着时不发请求、不起进程。

| 功能 | 开关 | 打开前 |
| --- | --- | --- |
| [闲时找活](../reference/features.md#闲时找活) | `suggest_enabled` | 无。可以先 `agentq suggest` 试一次 |
| [主动感知](../reference/features.md#主动感知) | `sense_enabled` | 配 `~/.appstoreconnect/config.env`（用只读 key），跑 `agentq sense --dry-run` 看条数对不对 |
| [发版助手](../reference/features.md#发版助手) | `release_assist_enabled` | 配好 App Store Connect 和 Telegram，跑 `agentq release check <项目>` |
| [Router](../reference/features.md#router) | `router_enabled` | 配好 Telegram，读 [SECURITY](../../SECURITY.md#router) |

## 要加门的检查命令

第一次 `agentq init` 或 `agentq add` 会按 `go.mod`、`package.json`、`Cargo.toml`、`pyproject.toml`、`Package.swift` 生成一份，判断不了才要自己写。在项目里提交一个可执行文件 `.agentq/verify`：

```sh
mkdir -p .agentq
printf '#!/bin/sh\nset -e\ngo test ./...\n' > .agentq/verify
chmod +x .agentq/verify
git add .agentq/verify && git commit -m '加门的检查命令'
```

合并前，agentq 在一次性 worktree 的合并态上跑默认分支上的这个脚本，超时 `verify_timeout`（默认 30 分钟）。没过就退回任务；退出 125 表示这次跑不了（工具没有、依赖装不上），不算提交有问题，不退回，等你看。要调门的档位写 `.agentq/gates.json`。见[质量门和引擎合并](../reference/features.md#质量门和引擎合并)。

## 要加界面门和成品门的脚本

示例在 `~/.local/share/agentq/templates/gates/`：网站用 `ui-playwright.sh`，iOS 用 `ui-simctl.sh`，烟测用 `smoke.sh`。复制过来，改开头几行：

```sh
cp ~/.local/share/agentq/templates/gates/ui-playwright.sh .agentq/ui
cp ~/.local/share/agentq/templates/gates/smoke.sh .agentq/smoke
chmod +x .agentq/ui .agentq/smoke
git add .agentq && git commit -m '加界面门和成品门的脚本'
```

界面门改到界面文件才跑，截图写进 `$AGENTQ_ARTIFACTS`。成品门只有 `agentq new` 的第一版和标了 `（成品）` 的条目才跑。两道门默认只记录，要挡合并就在 `.agentq/gates.json` 调成 `error`。用户确认过的界面基线提交到 `docs/design/approved/`。见[界面门](../reference/features.md#界面门)、[成品门](../reference/features.md#成品门)。

## 要开工前问一下大方向

设计、界面、前后端选型这类事，在条目标题结尾写 `（大方向）`：

```markdown
- [ ] 做个主页（大方向） —— 要什么；验收：打开看
```

真要开工时 agentq 问你一次（Telegram 卡片，最多 3 个问题，每个带默认答案），`discuss_wait`（默认 2 小时）没回就按默认。别的条目不等。没配 Telegram 就直接按默认，不调模型。不想被问：`discuss_enabled` 设成 `false`。已经想好了，直接在行尾写 `（方向：纸墨风，Go 后端）`。

## 要记下不做的事

在项目 `docs/NOT-DOING.md` 里写，闲时找活和主动感知会永久避开同标题，任务开工前也会看：

```markdown
## 不做桌面版
- 原话：只做网页版
- 日期：2026-11-04
```
