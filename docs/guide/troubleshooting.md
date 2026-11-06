# 排查

按症状找。先跑这三条，多数问题的原因已经写在输出里：

```sh
agentq status
agentq patrol
tail -50 ~/.agentq/launchd.log
```

## 任务不启动

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 调度：已暂停 | `paused=true`，新安装默认如此 | `agentq start` 或 `agentq resume` |
| 调度：用量冷却中 | 撞了用量上限 | 等到写明的时间。`run` 也会拒绝 |
| 调度：订阅额度快用完 | 计入的窗口 ≥90% | 到重置时间自动恢复。`agentq run` 还能用，≥98% 也拒绝 |
| 调度：连环失败熔断中 | 连续 3 个任务秒退 | 看当天汇总的「熔断：…」。修好后 `agentq run` 试跑一条，跑完就解除 |
| 没有报错，就是不接活 | 没有定时服务 | `agentq start`。macOS 用 `launchctl print gui/$(id -u)/io.agentq.agentq` 确认，Linux 用 `systemctl --user status io.agentq.agentq.timer` |
| 某个项目一直不接活 | 它在 `exclude_projects` 里 | `agentq status` 会列出被排除的项目。要接就从配置里去掉，只跑一次用 `agentq run` |
| 日志写未登录 | `claude auth status` 显示 `loggedIn=false` | `claude auth login`。服务只带 `HOME` 和 `PATH`，只靠当前 shell 环境变量的身份在服务里用不了，要配持久的认证 |
| `agentq run` 说不是 git 主目录 | 目录不在 `projects_root` 下一层也没登记，或 `.git` 不是目录（worktree、子模块） | 在项目主目录运行一次 `agentq add`，会自动登记 |

## 条目一直不被挑

逐项看：

- 节标题是不是 `## 待做`，条目是不是以 `- [ ]` 开头，有没有被 HTML 注释包住。
- 有没有 `（进行中…）` 或卡住标记。`status` 的项目行会数这两种。
- 上次结束不到 `retry_after`（默认 12 小时）。`agentq run` 不受这条限制。
- 这条刚被去掉孤儿标记，也要等 `retry_after`。
- 项目是 `agentq new` 正在建的，或被手动停掉的新项目。`status` 会列一行「停掉的新项目，不自动做」，`agentq run <项目>` 解除。
- 主目录被占满名额：同项目默认最多 1 个任务。

## 任务跑了但没写文件

`permission_mode` 是 `default`。这个模式下无人值守的任务写不了文件，任务箱的摘要也会提示这一点。先读 [SECURITY](../../SECURITY.md)，然后 `agentq start`，或手动把 `permission_mode` 改成 `bypassPermissions`。

## 任务反复重做同一条

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 报告做完，条目还在待做 | 任务没把条目移到「已完成」 | 连续两次后 agentq 标卡住。确实做完了就手动移到「已完成」，并写上 `合并提交 <号>` |
| 反复失败 | 每隔 `retry_after` 重试 | 看 `agentq logs`，修好原因或把条目移走 |
| 上游一直 5xx | 瞬时错误的短退避，一天最多约 6 次，几乎不花钱 | 等上游恢复 |

## 任务状态异常

| 症状 | 含义 | 处理 |
| --- | --- | --- |
| 疑似卡死 | 会话记录 45 分钟没增长，可能在跑长测试 | 不会自动杀，`timeout`（默认 4 小时）兜底。确认卡住就 `agentq stop <id>` |
| 中断 | 进程退出但条目还标进行中，或做到一半撞上限 | 自动续跑一次，见[中断与自动续跑](../reference/scheduling.md#中断与自动续跑) |
| 续跑后仍中断 | 不再自动续跑 | 看分支现状，手动 `claude -r` 接着做，或去掉标记 |
| 子代理失控，自动停 | 子代理日志 ≥25 个或再派 ≥8 个 | 回复通知接回，或去掉卡住标记。不想让它自动停：`progress_autostop=false` |
| 撞预算两次，卡住 | 单次超过 `job_budget_usd`，续跑又超 | 同上，或调高预算 |
| 门没过，条目标了卡住 | 退回两轮还不过，或判不了（比如没有 `.agentq/verify`） | `agentq gate report <任务>` 看报告。卡片点放行、退回、放弃、再审一次，或 `agentq gate pass <任务> --reason "..."`、`agentq gate rerun <任务>`、`agentq gate abandon <任务>`，或去掉卡住标记从分支接着做 |
| 条目标了 `（大方向）` 没开工 | 在等你回答：Telegram 卡片或 `agentq discuss`。到 `discuss_wait` 没回就按默认开工，别的条目不等 | 回答；要马上开工就 `agentq discuss answer <编号> 随便` |
| 新项目后面的条目不开工 | 第一版还没合并，后面的活要在它上面做 | `agentq status` 写了卡在哪和三种解法：去掉第一条的卡住标记从分支重做（第二条继续等）；`agentq gate abandon <任务>` 放弃，分支留着，后面的开工；把第一条从 `docs/BACKLOG.md` 删掉并提交，后面的开工 |
| 界面门判不了，或没有交付单 | 项目还没有 `.agentq/ui`、`.agentq/smoke`，默认只记录，不挡合并 | 见[要加界面门和成品门的脚本](configuration.md#要加界面门和成品门的脚本) |
| 退回时合同改成重写 | 补丁门调成了 `error`，同一模块 30 天内第三次修复 | `agentq health <项目>` 看是哪个模块。任务重写后 `type` 写 `rewrite` |
| 门过了还没合并 | 主目录不在任务基于的分支（任务起的时候记下的，`agentq gate report <任务id>` 里写着），或者有和这次改动重叠的未提交改动 | 不在就按提示 `git switch`，切到别的主线也不会合；有重叠改动就提交或还原。下次 tick 自动合并 |
| 有待做但不接任务 | 认不出默认分支，或者主目录不在默认分支上。不花额度，`agentq status` 的「不接任务」一行写明原因 | 按那一行的命令切回去，下次 tick 接着做；认不出就改分支名或 `git remote set-head origin -a` |
| 任务绕过门的提醒 | 任务没写 `ready.json`，自己把条目移到了「已完成」 | 合并不撤销，只提醒。自己看一眼这个提交 |

## 打开调度后突然起了很多任务

第一次 tick 会处理所有超过 24 小时的旧进行中标记，有未合并内容的会各起一个接手任务。不想要的先 `agentq pause`，手动处理这些标记，再恢复。

## 状态文件和配置

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 状态文件损坏 | 内容坏了 | 下次 tick 自动备份成 `state.json.bad-<时间>`，按 `meta.json` 重建，那次不启动新任务。不用管 |
| 状态文件读不出 | 没权限、I/O 错误、变成了目录 | 不收尾不启动，只记日志。修好文件就恢复 |
| 命令报配置格式不对 | `config.json` 写坏或校验不过 | 修好它，错误信息写明了哪个键。agentq 不覆盖 |
| 自动回滚了 | tick 连续两次非 0，常见原因是配置写错 | 看 `launchd.log`，修好原因后重跑安装 |
| 所有 tick 都跳过 | `tick.lock` 被拿着。正常的 tick 外部命令都有超时，多半是持锁进程被 SIGSTOP 或卡死 | `lsof ~/.agentq/tick.lock` 找到进程处理掉 |
| 冷却或熔断卡到很久以后 | 时钟跳过 | tick 会自动改回现在算，并记日志 |
| 目录里有 `.xxx.tmp-*` | 写文件时被 SIGKILL | 可以删，不影响功能 |

## 额度

| 症状 | 含义 | 处理 |
| --- | --- | --- |
| 额度：未知，照常接活 | `claude -p /usage` 读不出 | 不影响运行。真撞上限时冷却接住。闲时找活读不出就不生成 |
| `agentq status` 比平时慢 | 额度缓存过期，status 现读一次，最多 60 秒 | 正常 |

## 服务

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| `Bootstrap failed: 5: Input/output error` | 刚卸下旧服务，launchd 还没清完 | 再执行一次 |
| `service environment unavailable` | 服务环境（只有 `HOME`、`PATH`）里 CLI 用不了 | 配持久的 CLI 认证，或确认 `claude_path` 是绝对路径 |
| `systemctl --user` 不可用 | WSL 没开 systemd、容器、ssh 进来没有用户会话 | 报错不写文件。修好用户会话，或只用手动命令 |
| 退出登录后任务停了（Linux） | 没开 linger | `loginctl enable-linger` |
| `enable` 失败并回滚（Linux） | 改了 `XDG_CONFIG_HOME`，agentq 只认 `~/.config/systemd/user` | 用默认位置 |
| journal 里有 `Found left-over process` | `KillMode=process` 的副作用 | 不影响运行 |
| 开机后夜班没跑 | 定点时错过了，tick 过 15 分钟补跑，夜班只在 08:00 前补 | 手动 `agentq night` |

## 安装和升级

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 终端找不到 `agentq` | 当前终端读的还是旧 PATH，或 shell 不是 zsh、bash | 新开一个终端，或用全路径 `~/.local/bin/agentq`。同意启动时会往 zsh、bash 的配置文件加一行，卸载时删掉 |
| 官方 CLI 装不上 | 网络不通或平台不支持，原文在输出里 | 换网络或代理重试，或 `brew install --cask claude-code`、`npm install -g @anthropic-ai/claude-code`，再 `claude auth login`、`agentq start` |
| 拒绝替换，有任务在跑 | 活跃任务阻止升级 | 等它结束，或 `agentq stop` |
| 报 conflict | `agent-task`、`dispatch`、`new-project`、`nightly-review` 有同名但内容不同的技能，或有 agentq 不认识的同名文件 | 原件不动。把你的技能改个名再装；其他同名技能（比如 `check`）会直接用你的版本，不会报这个 |
| `--claude` 和配置不同 | 已有配置的 `claude_path` 和它不一样 | 先手动改配置 |
| `custom CLAUDE_CONFIG_DIR` | 自定义配置目录不会自动迁移 | 按安装指南处理 |
| `no managed installation` | `agentq doctor` 只认受管安装 | 跑安装命令 |
| 中断的安装 | pending 事务 | `sh scripts/bootstrap.sh restore --yes` |

## BACKLOG 写不进去

agentq 改 BACKLOG 前要求主目录在默认分支，且 BACKLOG 没有未提交的改动，否则拒绝，不会替你提交。

- 超过 1 小时巡逻会报「的 BACKLOG 有没提交的改动」。提交或还原后恢复。
- 拿不到 `<项目>-main` 锁：锁目录没有 pid 文件超过 1 分钟会自动清掉。
- BACKLOG 是指向仓库外的软链接：报错，不改。

## Telegram

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 收不到推送 | `notify_enabled` 关着（新安装同意启动前，或者你改过），或 `telegram.env` 权限不是 600 | `agentq status` 的「推送」行写明原因 |
| 回消息要等几分钟 | 没有常驻监听，tick 每 5 分钟收一次 | 见[让消息随到随回](telegram.md#让消息随到随回) |
| 长轮询报 409 | 机器人设了 webhook，或两个地方在收 | 删 webhook。`listen` 每分钟重试 |
| 回「按钮过期了」 | 超过 14 天，或进程重启前发出的旧按钮 | 回文字 |
| 回「条目改过了，请回文字」 | 发卡片后条目被改 | 回文字 |
| 回「没定成」 | 主目录一时提交不了 | 处理后再点 |
| 「发」没提交 | 只认回复那条提示消息、整句就是「发」，7 天内，构建没变 | 按回复里写的原因处理 |
| 巡逻报监听服务没在列表里 | 受管安装不装监听服务 | 自己保持 `agentq listen` 运行，或忽略 |
| 基础设施问题没推送 | 这类只写 `patrol.json` 和汇总 | 看 `agentq patrol`。随仓库的看门狗也只写本地报告 |

推送规则（合并窗口、夜间、每天上限）见[为什么没收到推送](telegram.md#为什么没收到推送)。

## 主动感知和发版助手

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| 没配 App Store Connect | 没有 `~/.appstoreconnect/config.env` | 感知只看 GitHub，发版助手不工作 |
| 第一次跑没有结果 | 第一次只记基线，历史不算新内容 | 正常 |
| 项目对应不上 App | bundle id 精确对应失败 | 看日志。id 要在 `project.pbxproj` 或 `project.yml` 里，三层以内 |
| 读取失败 | 接口 401、403、429、5xx | 只记日志，下次再读。这条路径只用假服务测过，遇到实际差异请提 Issue |
