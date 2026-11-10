# Telegram

Telegram 是可选的消息入口。不配置它，agentq 也能调度任务、查看状态和日志、生成本地汇总和早报。

Telegram Bot API 只负责收发消息，不负责模型推理。`TELEGRAM_BOT_TOKEN` 和 `TELEGRAM_CHAT_ID` 不是模型 API Key，也不能替代 Claude CLI 登录。

配置后，两个入口的对应关系：

| 用途 | 本机 CLI | Telegram |
| --- | --- | --- |
| 用自然语言说明需求、提问 | 在 `claude` 交互会话里说 | 给机器人发消息 |
| 管理任务 | `agentq run`、`status`、`logs` 等 | 发「状态」，发「停」加任务 id，或直接说明 |
| 接收消息 | 本机通知 | 早报、异常、要你决定的事、任务结果 |
| 查看产物 | 读本地文件 | 发送截图、录屏、文件，有大小限制 |

两个入口用同一批项目文件和 BACKLOG，聊天记录不互通。

## 配置机器人

1. 在 Telegram 里找 @BotFather，发 `/newbot` 创建机器人，记下 token。
2. 给机器人发一条消息，在浏览器打开 `https://api.telegram.org/bot<token>/getUpdates`，找到 `"chat":{"id":…}` 里的数字。
3. 写入 `~/.agentq/secrets/telegram.env`：

   ```sh
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<数字>
   ```

   ```sh
   chmod 600 ~/.agentq/secrets/telegram.env
   ```

4. 打开推送。凭据写好后运行下面的命令，它把 `notify_enabled` 改成 `true`，其他配置不变：

   ```sh
   agentq notify setup
   ```

5. 发送测试消息，再运行 `agentq start` 安装常驻监听：

   ```sh
   agentq notify test
   agentq start
   ```

手机上收到测试消息就说明配置成功。没有写凭据时，`agentq notify setup` 打印同样的步骤。存在 `~/.agentq/secrets/telegram-setup.sh` 时，它执行这个脚本。

文件权限不是 600 时，agentq 不使用这个文件，`agentq status` 的「推送」一行显示原因。agentq 只给这个 chat id 发消息，也只接受它发来的消息。

## 常驻监听

收消息有两种方式：

- **不装常驻进程**：tick 每 5 分钟短轮询一次，要有 tick 服务在运行。回复最多延迟 5 分钟。
- **常驻监听**：`agentq listen` 用长轮询，消息到达后立刻处理。

配好凭据后运行 `agentq start`，常驻监听和调度服务一起安装，升级时保留。没配 Telegram 时不安装。也可以用 `agentq setup --services --listen --yes` 明确安装。监听退出后，tick 继续短轮询。

配了 Telegram 但没有名为 `<label>.listen` 的服务时，巡逻把它记成一个问题（只写 `patrol.json` 和汇总，不推送），提示你运行 `agentq start`。

`agentq status` 的「监听」一行显示它是否在运行。

## 添加任务和提问

直接发消息，不用记命令：

```text
my-app：加个按月对比
demo 现在进度怎么样
```

agentq 先回复「收到」，再交给 `claude -p` 判断意图，回答挂在你那条消息下面。消息可以带截图，比如「这里怎么歪了」配一张图也能添加任务。

下面这些整句由代码直接回答，不消耗额度，几秒内返回（回复别的消息时不算）：

| 发送 | 效果 |
| --- | --- |
| `状态` | 要你定的事、调度、额度、在做、待做、最近结束 |
| `暂停`、`恢复` | 修改 `paused` |
| `额度` | 各窗口用量和重置时间 |
| `在跑什么` | 任务 id、项目、标题、运行时长 |
| `停 <任务id>` | 和 `agentq stop` 相同 |

## 决定和续跑

- **要你定**：回复那条消息说明选哪个，agentq 写回条目并启动。条目写了固定格式的选项时，点按钮即可，不启动模型。格式见[决策卡片](../reference/features.md#决策卡片)。
- **任务提醒**：回复它就接上那个任务。任务中断时以写模式续跑，已经合并时只读回答问题。规则见[回复一条消息接上那件事](../reference/features.md#回复一条消息接上那件事)。
- **建议**：回复 `做 1 3` 把这些编号的建议移进待做，回复 `不做 2` 写进不做清单。

## 推送规则

- 同类消息 30 分钟内只发一条，后面的攒着合并。
- 23:00 到 08:00 的消息攒到早上发。
- 每天最多 10 条，超出的跟着下一条发送。
- 任务合并后才推送「完成」，进程退出或正在运行质量门时不推送。失败、中断、卡住推送「要你知道」。会自动续跑、接手、重试的任务，有结果后才推送。
- `notify_enabled` 关闭时不推送。一键脚本里同意启动时会打开。先只安装、后来用 `agentq start` 启动的要自己打开，因为它只改 `permission_mode` 和 `paused`。

`agentq notify status` 列出攒着的消息和最近的错误。完整规则见[推送](../reference/features.md#推送)。

## 限制

- 设了 webhook 的机器人收不到消息（长轮询报 409）。删除 webhook 后再用。
- 机器人只能下载 20 MB 以内的文件。语音和贴纸不处理。
- chat id 配成群时，按钮和「发」都不生效，因为要求 `from.id` 等于配置的 chat id。要用私聊。
- 完成推送和决策卡片的按钮回调在真实 Telegram 上走通，回复链和完整对话往返只用假服务测过。
