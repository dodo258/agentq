# Telegram

Telegram 是可选的消息入口。不配置它，agentq 也能调度任务、看状态和日志、生成本地汇总和早报。

Telegram Bot API 只负责收发消息，不负责模型推理。`TELEGRAM_BOT_TOKEN` 和 `TELEGRAM_CHAT_ID` 不是模型 API Key，也不能替代 Claude CLI 登录。

配了之后：

| 用途 | 本机 CLI | Telegram |
| --- | --- | --- |
| 自然语言交代需求、提问 | 在 `claude` 交互会话里说 | 给机器人发消息 |
| 管理任务 | `agentq run`、`status`、`logs` 等 | 发「状态」，发「停」加任务 id，或直接说 |
| 接收消息 | 本机通知 | 早报、异常、要你决定的事、任务结果 |
| 查看产物 | 读本地文件 | 发送截图、录屏、文件，有大小限制 |

两个入口用同一批项目文件和 BACKLOG，但聊天记录不互通。

## 配机器人

1. 在 Telegram 里找 @BotFather，发 `/newbot` 建一个机器人，记下 token。
2. 给机器人发一句话，在浏览器打开 `https://api.telegram.org/bot<token>/getUpdates`，找到 `"chat":{"id":…}` 里的数字。
3. 写 `~/.agentq/secrets/telegram.env`：

   ```sh
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<数字>
   ```

   ```sh
   chmod 600 ~/.agentq/secrets/telegram.env
   ```

4. 打开推送。凭据写好后运行一次，它把 `notify_enabled` 改成 `true`，别的配置不动：

   ```sh
   agentq notify setup
   ```

5. 发测试消息，再运行 `agentq start` 把常驻监听一起装上：

   ```sh
   agentq notify test
   agentq start
   ```

手机上收到就通了。还没写凭据时 `agentq notify setup` 会打印同样的步骤；如果你有 `~/.agentq/secrets/telegram-setup.sh`，它会执行这个脚本。

权限不是 600 时 agentq 不用这个文件，`agentq status` 的「推送」一行报原因。agentq 只给这个 chat id 发消息，也只认它发来的消息。

## 让消息随到随回

有两种收消息的方式：

- **不装常驻进程**：tick 每 5 分钟短轮询一次，要有 tick 服务在跑。回复最多延迟 5 分钟。
- **常驻监听**：`agentq listen` 用长轮询，消息一到就收。

配好凭据后运行 `agentq start`，它会把常驻监听和调度服务一起装上，之后升级也保留。没配 Telegram 时不装。也可以用 `agentq setup --services --listen --yes` 显式安装。监听退出后 tick 接回兜底。

配了 Telegram 又没有名为 `<label>.listen` 的服务时，巡逻会把它记成一个问题（只写 `patrol.json` 和汇总，不推送），提示你运行 `agentq start`。

`agentq status` 的「监听」一行显示它在不在跑。

## 派活和问事

直接说，不用记命令：

```text
my-app：加个按月对比
demo 现在进度怎么样
```

agentq 先回「收到」，再交给 `claude -p` 理解意图，答完挂在你那条消息下面。可以带截图，比如「这里怎么歪了」配一张图就能派活。

这几句由代码直接答，不花额度，几秒到（只认整句，回复别的消息时不算）：

| 发 | 效果 |
| --- | --- |
| `状态` | 要你定的事、调度、额度、在做、待做、最近结束 |
| `暂停`、`恢复` | 改 `paused` |
| `额度` | 各窗口用量和重置时间 |
| `在跑什么` | 任务 id、项目、标题、跑了多久 |
| `停 <任务id>` | 和 `agentq stop` 一样 |

## 决定和续跑

- **要你定**：回复那条消息说选哪个，agentq 写回条目并派出。条目写了固定格式的选项时，点按钮就行，不起模型。格式见[决策卡片](../reference/features.md#决策卡片)。
- **任务提醒**：回复它就接上那个任务。任务中断了就写模式续跑；已经合并了就只读回答你的问题。规则见[回复一条消息接上那件事](../reference/features.md#回复一条消息接上那件事)。
- **建议**：回 `做 1 3` 把编号的建议移进待做，回 `不做 2` 写进不做清单。
- **发版**：回复「准备好了」那条消息，整句写 `发`，才提交审核。

## 为什么没收到推送

- 同类 30 分钟内只发一条，后面的攒着合并。
- 23:00 到 08:00 只攒着。
- 每天最多 10 条，超出的跟着下一条发。
- 任务合并了才推「完成」，进程退出、还在过门时不推。失败、中断、卡住推「要你知道」；会自动续跑、接手、重试的，到了结果再说。
- `notify_enabled` 关着。一键脚本里同意启动时会打开；先只安装、后来用 `agentq start` 启动的要自己打开，它只改 `permission_mode` 和 `paused`。

`agentq notify status` 列出攒着的消息和最近的错误。完整规则见[推送](../reference/features.md#推送)。

## 限制

- 设了 webhook 的机器人收不到消息（长轮询报 409）。删掉 webhook 再用。
- 机器人只能下载 20 MB 以内的文件。语音和贴纸不处理。
- chat id 配成群时，按钮和「发」没人点得动，因为要求 `from.id` 等于配置的 chat id。用私聊。
- 决策卡片按钮回调只用假服务测过，真实 Telegram 上还没验证。
