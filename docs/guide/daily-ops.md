# 日常使用

## 要派活

在项目目录打开 `claude`，用自然语言说，`dispatch` 技能写成条目并启动：

```text
派活：给导出流程加失败重试并补测试
```

说「夜里做」或「只加到待办」时只写不启动。也可以直接加：

```sh
agentq add "给导出流程加失败重试并补测试" --project my-app
```

或者手写进 `docs/BACKLOG.md` 的「待做」并提交，语法见[调度规则](../reference/scheduling.md#backlog-语法)：

```markdown
- [ ] 导出重试 —— 失败时重试 3 次；验收：断网时能看到重试日志，测试通过
```

## 要立刻跑一条

```sh
agentq run my-app 导出重试
```

不看暂停和 `retry_after`，仍遵守并发上限和用量冷却。

## 要从零做个新项目

```sh
agentq new "一个记录读书笔记的网页工具"
```

不问问题，补的假设写进方案，第一条做到分支可合并，由引擎过门后合并，过了烟测有交付单。结果写当天汇总并推送。

## 要看哪个模块老出问题

```sh
agentq health 项目名
```

最近 30 天每个模块的改动和修复次数，再修就要重写的会标出来，还有两周返工率。

## 要放一条不属于项目的活

```sh
agentq inbox add "调研三款日程应用的同步方式 —— 写一份对比"
agentq inbox
```

项目待做之后、有名额时接，产出在 `~/.agentq/outbox/`。

## 要看状态

```sh
agentq status
```

先看前几行：

- **调度**：`正常`，或暂停、冷却、额度、熔断的原因和恢复时间。
- **额度**：各窗口用量。
- **运行中**：任务 id、项目、标题、已运行多久。「续跑中」「疑似卡死」直接标在后面。
- **各项目「待做」**：条数，进行中和卡住各多少。
- **最近结束**：状态、摘要、用量。

## 要看一个任务的结果

```sh
agentq logs 1104-12          # id 写前缀，至少 4 个字符
```

要接着看那个会话：在项目目录 `claude -r <会话id>`，`logs` 里有命令。

文件在 `~/.agentq/jobs/<任务id>/`。

## 要看今天发生了什么

```sh
cat ~/.claude/nightly/$(date +%F)-agentq.md
```

每个结束的任务一行，末尾是当天花费和质量小结。

看花费和质量：

```sh
agentq stats 30
agentq quality
```

## 要停

```sh
agentq stop 1104-12          # 停一个任务
agentq pause                 # 不再接新活，正在跑的不动
```

`agentq stop` 之后下一条待做会立刻接上，要全部停住先 `pause`。

被 `stop` 的条目标记还在时，agentq 不接手也不重挑。不要了就把条目删掉或移走；要接着做就去掉 `（进行中…）` 标记，或回复那个任务的 Telegram 提醒。

## 要恢复

```sh
agentq resume
```

## 任务中断了

大多数情况不用管：

| 情况 | agentq 怎么做 |
| --- | --- |
| 退出但条目还标进行中 | 用同一会话续跑一次 |
| 撞用量上限 | 冷却到重置时间，再续跑 |
| 机器重启 | 用同一会话续跑一次 |
| 撞预算 | 同一会话接着做一次，再撞才标卡住 |
| 上游 5xx、422 | 15 分钟、1 小时各重试一次 |
| 标记留下超过 24 小时没人动 | 有未合并内容就接手一次，没有就去掉标记 |

## 条目标了卡住

看标记写的原因和 `agentq logs`。分支和 worktree 都留着。

- agentq 写的标记（子代理失控、撞两次预算）：回复那条通知，写模式接回原会话；或者去掉整个标记，下次挑到时起新会话，从分支现状接着做，不等 `retry_after`。
- `agent-task` 写的标记（`（卡住：<分支> <卡在哪>）`）：解决问题后去掉标记，按 `retry_after` 再挑。
- 接手后仍未完成：同上。

详见[失控兜底](../reference/scheduling.md#失控兜底)。

## 要你决定的事

任务遇到必须由你决定的事，会放进 BACKLOG 的「等用户决定」。

- 配了 Telegram：回复「要你定」那条消息，或点按钮。
- 没配：在 `claude` 会话里说选哪个，`dispatch` 技能写回条目并移到「待做」顶部。或者手动改。

看哪些在等：`agentq patrol`，或 `agentq status` 所在项目的 BACKLOG。

条目要等别的事做完才轮到你定，写 `（等条件：<条件>）`，之前不提醒。

## 早报

打开 `review_enabled` 后，复盘每天 06:30 写 `~/.claude/nightly/<日期>.md`。配了推送就在 08:00 后推一条摘要，要你决定的和出问题的放最前。

没有新东西时复盘不起模型，写一份一句话的占位报告，早报照常推。

## 验收

看完成品有意见，写成新条目。写法固定，才计入返工统计：

```markdown
- [ ] 验收 导出重试 的意见 —— 重试间隔太短；修 abc1234 引入的问题
```

## BACKLOG 太长

```sh
agentq backlog archive my-app --dry-run
agentq backlog archive my-app
```

把「已完成」的旧条目移到 `docs/BACKLOG-archive.md`，不自动提交。

## 升级

运行 `agentq update`，或重跑安装命令。配置和数据保留，有任务在跑时拒绝替换，等它结束再来。

```sh
agentq update
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

装坏了 `agentq rollback --yes`。
