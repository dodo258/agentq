# 旧版 binary-only 安装兼容流程

仅适用于尚无完整安装 receipt 的既有安装；不安装技能/模板/依赖，不能作为新用户完整安装入口。新安装见 [中文指南](install.zh-CN.md)。受管安装中 legacy install/uninstall 会拒绝操作，update.sh 复用完整安装引擎。以下保留旧版操作说明与当时的验证边界。

## 安装

```bash
cd ~/projects/agentq
scripts/update.sh # 构建、自检、装到 ~/.local/bin/agentq（见下文「安装新版本」）
agentq pause      # 可选：先暂停，确认无误再 resume
agentq install
```

`install` 写入保护脚本 `~/.agentq/tick-guard.sh` 和 `~/Library/LaunchAgents/io.agentq.agentq.plist`，再 `launchctl bootstrap`。launchd 调用的是 `/bin/sh ~/.agentq/tick-guard.sh`，由它执行 `agentq tick`（见下文「自动回滚」）。装上时立刻执行一次 tick，之后每 300 秒一次。同时装上夜班、复盘两个定点任务（`io.agentq.agentq.night`、`io.agentq.agentq.review`，见[夜班和复盘](reference/features.md#夜班和复盘)），开关默认关，装上不会改变什么；还有常驻的 Telegram 监听 `io.agentq.agentq.listen`（见[在 Telegram 里说话](reference/features.md#在-telegram-里说话)，没配 Telegram 时它只歇着）。可以重复执行，已装着时会先卸下再装。

确认装好：

```bash
launchctl print gui/$(id -u)/io.agentq.agentq | head   # 能看到这个服务
launchctl print gui/$(id -u)/io.agentq.agentq.night | head    # 夜班定点任务（每天 01:00）
launchctl print gui/$(id -u)/io.agentq.agentq.review | head   # 复盘定点任务（每天 06:30）
tail -f ~/.agentq/launchd.log                            # 每 5 分钟至少一行「tick：…」
```

重复执行 `agentq install` 偶尔会报 `Bootstrap failed: 5: Input/output error`（刚卸下旧服务，launchd 还没清完），再执行一次就好。

卸载：`agentq uninstall`。它会 bootout 并删掉四个 plist。`~/.agentq` 里的配置、状态和任务记录都保留。正在跑的任务不受影响：任务外壳和 claude 都在自己的进程组里，launchd bootout 时不会一起清掉（2026-10-10 在 Darwin 27 上实测：一次性测试服务拉起的、在自己进程组里的子进程，bootout 和重新 bootstrap 后都还在；真实 `agentq install` 重装时两个运行中的任务也都没被杀）。`agentq install` 重装同理。卸载后还在跑的任务结束时仍会 tick 一次、可能接下一条待做；要彻底不接新活，再执行 `agentq pause`。

前提：终端版 Claude 已登录（`claude auth status` 显示 `"loggedIn": true`）。没登录时 tick 只记一行日志，不启动任务。

## 安装新版本

agentq 会被 agent 改代码、重新安装，装坏了就没人修。所以改了 agentq、合并进 main 后，一律在项目主目录执行：

```bash
scripts/update.sh
```

它做三步，任何一步失败都不动现在装着的版本：

1. `go build` 到临时目录（编译不过就停）；
2. 用临时 HOME 跑一次 `agentq status` 和一次空跑 `agentq tick`（没有项目、不启动任务，不读写真实的 `~/.agentq`、`~/projects`、`~/.claude`），任何一个非 0 退出就停；
3. 通过才替换：旧的 `~/.local/bin/agentq` 留成 `~/.local/bin/agentq.prev`，新的原子改名过去，清零保护脚本的连续失败计数。下次 tick 自动用新版本。现在装着的版本在 launchd 下已经失败过（`~/.agentq/guard-fails` 非空）且已有 `agentq.prev` 时，保留原来的 `agentq.prev` 不覆盖，免得回滚换回一个坏的。

不要再直接 `go build -o ~/.local/bin/agentq .`：那样没有自检，也不留 `agentq.prev`，坏了不能自动回滚。保护脚本或 plist 的内容改了（`internal/service`），装完新版本再执行一次 `agentq install`。

## 自动回滚

自检只能挡住一启动就坏的版本。装上后真实 tick 才出错的，由 launchd 调用的保护脚本 `~/.agentq/tick-guard.sh`（`agentq install` 生成，不要手改）兜住：

- 每次执行 `agentq tick`，返回 0 就清零计数；非 0 就把连续失败次数记进 `~/.agentq/guard-fails`，日志写一行「保护脚本：agentq tick 退出码 N（连续第 n 次非 0）」。
- 连续 2 次非 0：把 `agentq.prev` 改名换回 `~/.local/bin/agentq`（原子替换），出错的版本留成 `agentq.bad` 供排查，当天汇总 `~/.claude/nightly/<日期>-agentq.md` 写一行「已自动回滚：…」，带上换回的那个版本的构建时间（可能已经很旧）。
- 换回后 `agentq.prev` 就没了，换回的版本也失败时不会来回换，只在第 2 次失败时写一行「…没有可换回的 agentq.prev，没有回滚…」；没有 `agentq.prev`（从没用 `scripts/update.sh` 装过）时也一样。
- tick 非 0 退出的情况：二进制坏了或起不来、配置文件写错、状态文件读不了或写不进。正常的「上一个 tick 还没结束，本次跳过」、暂停、冷却、没登录都返回 0，不计数。所以把 `config.json` 改错了也会触发一次回滚（旧版本读同一个配置照样失败，之后只报警）：看到「已自动回滚」先看 `launchd.log` 里的原因，修好后用 `scripts/update.sh` 重新装。
- 任务结束后外壳直接执行的 `agentq tick --after` 不经保护脚本；它失败了由下一次定时 tick 收尾。
