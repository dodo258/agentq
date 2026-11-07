# 配置

配置文件是 `~/.agentq/config.json`。改完不用重启，下次 tick 生效。每个键的类型和默认值见[配置参考](../reference/config.md)。

JSON 写错时命令报错退出。服务里的 tick 连续两次失败会触发回滚保护，所以改完运行一次 `agentq status` 确认。

## 权限

新安装的权限是 `default`，任务写不了文件。`agentq start` 在你同意后改成 `bypassPermissions`。手动修改前先读 [SECURITY](../../SECURITY.md)：

```json
{ "permission_mode": "bypassPermissions" }
```

## 项目目录

```json
{ "projects_root": "~/work" }
```

agentq 只扫描这个目录下一层、`.git` 是目录的 Git 主目录，跳过以 `.` 开头的目录。个别项目在外面时不用改它：在那个仓库里运行 `agentq add`，项目会自动登记到 `extra_projects`，见[外面的项目](../reference/config.md#外面的项目)。

## 并发

```json
{ "max_parallel": 3, "per_project": 1 }
```

`per_project` 大于 1 时，同一项目会并行多个任务。每个任务有自己的 worktree，但合并时可能冲突。

## 模型和强度

新配置的 `model`、`effort` 是空的，任务跟随你在 CLI 里选的设置，不传参数。要给后台任务单独指定时：

```json
{ "model": "sonnet", "effort": "medium", "job_budget_usd": 30 }
```

`job_budget_usd` 是单次运行的预算上限，撞到后保留已有改动，并接着做一次。夜班、复盘、巡逻分析各有自己的预算，见[配置参考](../reference/config.md#预算)。

低风险条目可以用单独的模型：配置 `low_risk_model`，并在条目标题里写 `（低风险）`。

```json
{ "low_risk_model": "sonnet" }
```

要让 agentq 按任务类型选模型，写 `"model_policy": "auto"`，重写和审查类不降级，见[按任务类型选模型](../reference/config.md#按任务类型选模型)。默认不开，因为降级模型省额度，但可能做坏。

用量用 `agentq stats` 查看，见[用量统计](../reference/features.md#每个任务的用量和-agentq-stats)。

## 失败重试和超时

```json
{ "retry_after": "6h", "timeout": "2h" }
```

`retry_after` 不要配得太短。任务结束时会立刻运行 tick，遇上 claude 一启动就失败时，任务会以秒级反复启动。连环失败熔断（`breaker_*`）会拦住这种情况，见[连环失败熔断](../reference/scheduling.md#连环失败熔断)。

## 排除项目

`projects_root` 下的项目只要有待做就会被处理。公开仓库或不想交给 agentq 的项目，写进排除名单：

```json
{ "exclude_projects": ["my-public-repo"] }
```

tick、闲时找活、主动感知都不处理它，`agentq status` 会列出来。`agentq run my-public-repo` 明确指定时仍可运行。详情见[配置参考](../reference/config.md#排除项目)。

## 夜班和复盘

需要服务（`agentq start` 或 `--services` 安装）。开关在每次定点运行时读取，改了不用重装。

```json
{ "night_enabled": true, "review_enabled": true }
```

`agentq status` 的「定点任务」一行应显示「夜班 01:00 开」「复盘 06:30 开」。第二天早上查看 `~/.claude/nightly/<日期>-night.md` 和 `<日期>.md`。

Claude 桌面端另有同类定时任务时，先停用它，否则会运行两次。详情见[夜班和复盘](../reference/features.md#夜班和复盘)。

## 巡逻和推送

```json
{ "patrol_enabled": true, "patrol_analyze": false, "notify_enabled": true }
```

巡逻本身不消耗额度。打开 `patrol_analyze` 后，有新异常时会启动模型分析，每天最多 3 次。推送的通道配置见 [Telegram](telegram.md)。

## 可选功能

这些功能默认关闭，关闭时不发请求、不启动进程。

| 功能 | 开关 | 打开前 |
| --- | --- | --- |
| [闲时找活](../reference/features.md#闲时找活) | `suggest_enabled` | 无。可以先运行 `agentq suggest` 试一次 |
| [主动感知](../reference/features.md#主动感知) | `sense_enabled` | 配置 `~/.appstoreconnect/config.env`（用只读 key），运行 `agentq sense --dry-run` 核对条数 |
| [发版助手](../reference/features.md#发版助手) | `release_assist_enabled` | 配好 App Store Connect 和 Telegram，运行 `agentq release check <项目>` |
| [Router](../reference/features.md#router) | `router_enabled` | 配好 Telegram，读 [SECURITY](../../SECURITY.md#router) |

## 测试门的检查命令

第一次 `agentq init` 或 `agentq add` 时，程序按 `go.mod`、`package.json`、`Cargo.toml`、`pyproject.toml`、`Package.swift` 生成一份 `.agentq/verify`。判断不了时，任务要在实现时补上，也可以自己在项目里提交一个可执行文件：

```sh
mkdir -p .agentq
printf '#!/bin/sh\nset -e\ngo test ./...\n' > .agentq/verify
chmod +x .agentq/verify
git add .agentq/verify && git commit -m '加门的检查命令'
```

合并前，agentq 在一次性 worktree 的合并结果上运行默认分支上的这个脚本，超时 `verify_timeout`（默认 30 分钟）。未通过时退回任务。退出码 125 表示这次跑不了（工具没有、依赖装不上），不算提交有问题，不退回，等你查看。

档位在 `.agentq/gates.json` 调整，见[质量门和引擎合并](../reference/features.md#质量门和引擎合并)。

## 界面门和成品门的脚本

示例在 `~/.local/share/agentq/templates/gates/`：网站用 `ui-playwright.sh`，iOS 用 `ui-simctl.sh`，烟测用 `smoke.sh`。复制到项目后，修改开头几行：

```sh
cp ~/.local/share/agentq/templates/gates/ui-playwright.sh .agentq/ui
cp ~/.local/share/agentq/templates/gates/smoke.sh .agentq/smoke
chmod +x .agentq/ui .agentq/smoke
git add .agentq && git commit -m '加界面门和成品门的脚本'
```

界面门只在改到界面文件时运行，截图写进 `$AGENTQ_ARTIFACTS`。成品门只在 `agentq new` 的第一版和标了 `（成品）` 的条目上运行。两道门默认只记录，要挡合并就在 `.agentq/gates.json` 调成 `error`。用户确认过的界面基线提交到 `docs/design/approved/`。见[界面门](../reference/features.md#界面门)、[成品门](../reference/features.md#成品门)。

## 大方向商量

设计、界面、技术选型这类条目，在标题结尾写 `（大方向）`：

```markdown
- [ ] 做个主页（大方向） —— 要什么；验收：打开看
```

条目开工前，agentq 问一次（Telegram 卡片，最多 3 个问题，每个带默认答案），`discuss_wait`（默认 2 小时）没有回答就按默认开工，其他条目不等待。没配 Telegram 时直接按默认，不调用模型。

把 `discuss_enabled` 设成 `false` 可以关闭询问。方向已经定好时，直接在行尾写 `（方向：极简风，Go 后端）`。

## 不做清单

在项目的 `docs/NOT-DOING.md` 里记录不做的事。闲时找活和主动感知永久避开同标题的条目，任务开工前也会查看：

```markdown
## 不做桌面版
- 原话：只做网页版
- 日期：2026-11-04
```
