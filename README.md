# agentq

[English](README.en.md)

说一句需求，agentq 带几个 Claude Code 会话把它从方案做到测试通过、合进本地主分支，再告诉你做了什么、怎么看。只支持 macOS。

## 例子

下面两段是 2026-11-26 在一次性试验项目上真实跑出来的，时间和文字照抄运行记录，没有改。消息是程序发出的原文（运行记录里「消息」那一行），配了 Telegram 的话手机上收到的就是它。

**小改动，1 分钟。** 03:02:07 开工，03:03:11 合进主分支：

```
$ agentq do "加一个 --version 选项，打印 wc2 0.1.0"
1126-030207 开工：wordle-cli「加一个 --version 选项，打印 wc2 0.1.0」
```

```
🟢 wordle-cli 做好了：wc2 加 --version 选项
做了什么：给 wc2 加了 `--version` 选项，并补上测试、`.agentq/verify` 检查脚本和 README 用法说明。
用户怎么看：在项目目录运行 `go run . --version`，会看到 `wc2 0.1.0`。
用了 1m0s，合并提交 39854fe
```

**从零做一个工具，53 分钟。** 需求：「用 Go 做一个命令行工具 mdsite：把一个文件夹的 Markdown 变成一个好看的静态网站，带全文搜索、深浅两种配色，还能本地预览、改了自动刷新。只用标准库」。

| 时间 | 发生了什么 |
|---|---|
| 03:02 | 开工，产品经理看项目、查同类、写方案 |
| 03:06 | 方案出来，分 6 块；第一块 base 开工。有一次会话没起来，自动重试 |
| 03:14 | base 合好，markdown、search、theme、site 四块同时开工 |
| 03:36 | 四块都合好，最后一块 app 开工 |
| 03:52 | 合完，测试一次过，开始审计 |
| 03:55 | 审计 0 个大 bug，合进主分支 |

```
📋 mdsite 开工：mdsite：一个文件夹变成好看的网站
用 Go 标准库做一个单文件命令行工具：指给它一个 Markdown 文件夹，不写配置就出一个能双击打开、中文搜得到、深浅两色的静态网站，mdsite serve 边写边自动刷新。分 6 块：先打公共骨架，再由 Markdown 解析、网站生成、搜索、主题四块同时做，最后一块做预览服务和命令行并接到一起。做好的标准：一条检查命令全过，拿示例文档生成的站点不起服务器也能看能搜，改一个字一秒左右浏览器就变。
```

```
🟢 mdsite 做好了：mdsite：一个文件夹变成好看的网站
用 Go 标准库做一个单文件命令行工具：指给它一个 Markdown 文件夹，不写配置就出一个能双击打开、中文搜得到、深浅两色的静态网站，mdsite serve 边写边自动刷新。分 6 块：先打公共骨架，再由 Markdown 解析、网站生成、搜索、主题四块同时做，最后一块做预览服务和命令行并接到一起。做好的标准：一条检查命令全过，拿示例文档生成的站点不起服务器也能看能搜，改一个字一秒左右浏览器就变。
用了 53m0s，合并提交 59275c2
```

这两次跑的时候还没接 Codex，mdsite 的审计是 Claude 做的。Codex 审计是之后接上的。

## 为什么不是调度器

agentq 的旧版（v0.1–v0.3）是一个调度器：排队、分名额、过五道门、自己巡逻、自己改自己。重做之前最近 7 天的数据：

- 产品代码从 1,700 行长到 48,110 行、28 个模块。
- 合并的 183 条，从开工到最后一次进门平均 76 分钟，干活 38 分钟，其余在过门、退回、等续跑。
- 过门 293 次，退回 106 次。抽看 45 条退回理由，多数是几个少见条件凑齐才出的边角。
- 324 次任务里 286 次由调度器自己挑起来。
- 13% 的额度花在 agentq 改自己。

这一版从头重写，只把一件事从头交接到尾：没有队列、名额、多道门、评测，也不自己开工改自己。产品代码上限 5,000 行，测试卡着。理由和取舍在 [docs/DESIGN.md](docs/DESIGN.md)。

## 一条直线

```
一句话 ─→ 产品经理 ─→ 按模块同时做 ─→ 合到一起跑测试 ─→ 审计一次 ─→ 修一次 ─→ 合进主分支 ─→ 告诉你
小改动 ─→ 产品经理 ─→ 一个会话做完 ─→ 跑测试 ─→ 合进主分支 ─→ 告诉你
```

1. **产品经理**：看项目；大活再上网看同类和抱怨，写方案，划模块、定接口和验收。小改动（一个会话一两个小时能做完的）不调研。不问人。大活的方案提交在项目的 `docs/specs/`。
2. **做**：每个模块一个会话、一个 git worktree，没有依赖的同时开工，不限个数。
3. **合**：做完就合进这条需求自己的分支，冲突让做这个模块的会话自己解。
4. **测试**：程序跑项目的 `.agentq/verify`，只看退出码。红了让 Opus 修，最多 3 次；还红请 Fable 出手一次。
5. **审计**：整个需求只审一次，只找大 bug。小改动不审。
6. **修**：逐条核对，成立的一次修掉，再跑测试，不再审。教训写进项目的 `LESSONS.md`。
7. **交付**：合进本地主分支，发一条消息。程序不推远程。

停下来找你的情况：测试或审计的 bug 修不好；冲突解不了；会话没登录、跑不起来或什么都没做出来；产品经理认为要你出面（花钱、发布、删数据、要账号）。主分支检出的目录里有没提交的改动挡着时，它发一条消息，等你提交或挪开，然后自己合。

每一步都记在交接单上：`agentq log <编号>`。你看过成品觉得要改，再说一句，就是一条新的需求。

## 谁干什么

| 活 | 模型 | 强度 |
|---|---|---|
| 产品经理、难的模块、修测试、修审计的 bug、解合主分支的冲突 | Opus | high |
| 普通模块 | Sonnet | high |
| Opus 修 3 次还红，出手一次 | Fable | xhigh |
| 唯一一次审计 | Codex `gpt-6.2-astra`；没装、没登录、没额度换 Opus | xhigh |
| 收尾没提交的改动 | 写那一块的模型 | medium |
| 听 Telegram 消息、认项目 | Sonnet | medium |
| 打勾、早报、合并、跑测试 | 程序，不用模型 | |

模块难不难由产品经理标。模型都能换，见[环境变量](#数据放哪环境变量)。

## 前提和安装

前提：

- macOS 13 或更新（Go 1.27 编的程序的要求；实际只在 macOS 27、Apple 芯片上跑过）。
- git，配好 `user.name` 和 `user.email`。
- 装好并登录过的 [Claude Code](https://code.claude.com/docs/en/setup)。作者用的是 2.1.332，更早的没试过。
- Codex 可选，只用来审计。作者用的是 codex-cli 0.172.0。

一条命令，装到 `~/.local/bin/agentq`，下载后核对 SHA-256，对不上什么都不装：

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
agentq install
```

脚本只放程序，不用 sudo，不改 shell 配置；`~/.local/bin` 不在 PATH 时它会打印要加的那一行。指定版本：`curl … | AGENTQ_VERSION=v1.2.3 sh`。机器上的或下载到的是旧版就什么都不改，见[从旧版怎么换](#从旧版怎么换)。

装完 `agentq --help` 的第一行应该是「agentq：说一句需求，做到合进主分支」。这一版的第一个 Release 发布之前，上面的地址给的还是旧版 v0.3.0。

`agentq install` 做三件事，逐条告诉你装了什么：

- 两个 launchd 服务：每 5 分钟接着做被打断的；早上 8 点写早报。配了 Telegram 再加一个收消息的。
- [派活技能](#派活技能)。
- 把当时的 `PATH` 和 `AGENTQ_*` 环境变量写进服务。

不跑 `agentq install` 也能用 `agentq do`，只是断了要自己跑 `agentq resume`。

从源码装（要 Go 1.27.4 或更新）：

```sh
git clone https://github.com/dodo258/agentq && cd agentq
mkdir -p ~/.local/bin
go build -o ~/.local/bin/agentq .
agentq install
```

升级：再跑一次安装命令，再跑一次 `agentq install`。没有自动升级。

## 全部命令

| 命令 | 做什么 |
|---|---|
| `agentq do "<需求>"` | 在当前项目开工。`-C <目录>` 指定项目，写在需求前面；需求写 `-` 从标准输入读 |
| `agentq todo` | 看当前项目的待办（也认 `-C <目录>`）。后面写关键字就做那一条，写 `all` 就能做的全做、做完接着做 |
| `agentq ls` | 看进度。`-a` 连做完的一起看 |
| `agentq log <编号>` | 看一条的交接单 |
| `agentq stop <编号>` | 停掉，分支和 worktree 留着 |
| `agentq retry <编号>` | 卡住、停了的从那一步接着做 |
| `agentq resume` | 把被打断的接着做（装了服务每 5 分钟自动跑一次） |
| `agentq self` | 把早报里「agentq 自己」那几条交给 agentq 修；合进它的源码后编新程序换掉自己。源码默认 `~/projects/agentq`，别处用 `AGENTQ_SOURCE` |
| `agentq brief` | 发早报：昨天做好的、卡住的、用了多少 token，还有 agentq 自己没跑顺的 |
| `agentq install` | 装上：派活技能、自动接着做、早上 8 点早报，配了 Telegram 就收消息 |
| `agentq uninstall` | 卸掉 `install` 装的 |
| `agentq version` | 版本号，自己编的是 `dev` |

项目要是一个 git 仓库，至少提交过一次。主分支先看 `origin/HEAD` 指的，没有就 `main`、`master`，再没有是当前分支。

## 待办怎么写

写在项目的 `docs/BACKLOG.md`，提交到主分支才算数：

```markdown
## 待做

- [ ] 统计页 —— 按月看合计，能切换年份
- [ ] 导出 CSV —— 设置页加导出按钮（依赖：统计页）
- [ ] 登录 —— 接 OAuth（卡住：等账号）

## 已完成
```

```
$ agentq todo
统计页  
导出 CSV  等「统计页」
登录  卡住
```

- 只读「## 待做」这一节里顶格的 `- [ ] `，一条一行。「——」前面是标题，整行是需求。
- `（依赖：…）` 写别的待办标题里的几个字，多个用逗号或顿号隔开。对得上的那条还在「待做」里就等它。
- 写着 `（卡住` 的，`all` 跳过；点名就照做。
- `agentq todo <关键字>` 做标题含这几个字、现在能开工的第一条。正在做的、等别的做完的、上次卡住的不会再开（卡住的用 `agentq retry`）。
- `agentq todo all` 把能做的同时开工，每做完一条打勾、挪到「## 已完成」，再开工被它挡着的。做过、停过的同名待办不会自己再来一遍。
- 你不说，它不会自己从待办里挑活。大项目分期时，产品经理会把后面几期写进待办。

## 项目的检查：.agentq/verify

一个可执行的脚本，一条命令跑这个项目的全部检查（测试、静态检查、构建），退出码 0 算过。例如：

```sh
#!/bin/sh
set -e
npm ci
npm run lint
npm test
```

- 没有这个脚本算没过；产品经理会在第一块里要求补上。
- 在 worktree 里跑，单独给一个 TMPDIR，最长 30 分钟。
- 环境变量是白名单（`HOME`、`PATH`、语言、代理、`GOTOOLCHAIN`、`GOROOT`、`CODEX_HOME`、`AGENTQ_*` 等）。别的要在脚本里自己设。
- 它就是质量的底：脚本测得少，程序就只能知道那么多。

## 配 Telegram

可选。不配就用 `agentq ls`，早报写在 `~/.agentq2/brief.md`。

1. 在 Telegram 找 @BotFather，发 `/newbot`，记下 token。
2. 给你的 bot 发一条消息，浏览器打开 `https://api.telegram.org/bot<token>/getUpdates`，找到 `"chat":{"id":…}` 里的数字。
3. 写 `~/.agentq2/telegram.env`：

   ```
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<数字>
   ```

4. `chmod 600 ~/.agentq2/telegram.env`
5. 再跑一次 `agentq install`，装上收消息的服务。`agentq brief` 会马上发一条，用来试通不通。

只认这个 chat id 本人的私聊，群里的不理。第 2 步那条消息也可能被收到并回答。收到的消息由一个会话处理：是要做的事就认出项目、开工，问的就答。项目在 `~/projects` 下找（没有会建一个空的），换地方用 `AGENTQ_PROJECTS`。

会发的消息：📋 大活开工、🟢 做好了、🔴 卡住了、🟠 等合并、☀️ 早报、对你消息的回答。没发出去的会补发；Telegram 不收的（比如 chat id 填错）报出原因、留着，改好了补发。

`~/.agentq2/telegram.env` 没有、数据目录是默认的、旧版的 `~/.agentq/secrets/telegram.env` 存在时，用旧的。

## 数据放哪、环境变量

都在 `~/.agentq2`：

| 位置 | 内容 |
|---|---|
| `orders/<编号>/order.json` | 这条需求的状态 |
| `orders/<编号>/HANDOFF.md`、`run.log` | 交接单、运行记录 |
| `orders/<编号>/` 里其余文件 | 会话输出、测试和审计的日志 |
| `orders/<编号>/trees/` | 干活的 worktree，合进主分支后清掉 |
| `brief.md`、`resume.log`、`listen.log`、`brief.log` | 早报、服务日志 |
| `inbox/`、`outbox/`、`chat-*` | 收到没处理完的、没发出去的、对话会话的输出 |
| `telegram.env` | Telegram 密钥 |
| `skill-dispatch.md` | 装过的派活技能原文，卸载时用来比对 |
| `todo.lock`、`locks/` | 文件锁 |

项目里会有：`agentq/<编号>/…` 分支（合完清掉）、`.git/agentq.lock`、提交进去的 `docs/specs/`、`LESSONS.md`、待办打勾。记录不会自动清理。

没有配置文件，只有环境变量：

| 变量 | 默认 | 作用 |
|---|---|---|
| `AGENTQ_HOME` | `~/.agentq2` | 数据目录 |
| `AGENTQ_PROJECTS` | `~/projects` | Telegram 消息里提到的项目在哪找，派活技能新建项目放哪 |
| `AGENTQ_TELEGRAM` | 见上一节 | Telegram 密钥文件；指到不存在的文件就不收发 |
| `AGENTQ_TELEGRAM_API` | `https://api.telegram.org` | Telegram 接口地址 |
| `AGENTQ_CLAUDE` | 自己找 | claude 程序。依次找 PATH、`~/.npm-global/bin`、`~/.local/bin`、`~/.claude/local` |
| `AGENTQ_CODEX` | 自己找 | codex 程序，找法同上 |
| `AGENTQ_LEAD` | `opus` | 产品经理、难的模块、修 |
| `AGENTQ_CODE` | `sonnet` | 普通模块、对话 |
| `AGENTQ_LAST` | `fable` | 最后出手一次的 |
| `AGENTQ_AUDIT` | `gpt-6.2-astra` | Codex 审计用的模型 |
| `AGENTQ_SOURCE` | `~/projects/agentq` | agentq 自己的源码，`agentq self` 在这里修，修完从这里编新程序 |

服务用的是 `agentq install` 那一刻的 `AGENTQ_*` 和 `PATH`，`agentq do` 用的是当时终端里的。要改就写进 shell 配置，再跑一次 `agentq install`，两边才一样。

安装脚本另认两个：`AGENTQ_VERSION`（装哪个版本）、`AGENTQ_RELEASE_BASE`（换下载地址，测试用）。

## 断了怎么接着

- 每条需求一个独立进程，关终端不影响。每一步做完都存盘。
- 进程被杀、机器重启：装了服务 5 分钟内自动接着做，没装就跑 `agentq resume`。`agentq ls` 里写着「断了，等接着做」的就是。
- 撞额度上限：会话自己睡到重置再续，`agentq ls` 里看得到等到几点。
- 会话出错：退避重试，4 次没跑成才算卡住。一次会话最长 4 小时，Codex 审计最长 3 小时。
- 卡住了（🔴）：`agentq log <编号>` 看原因，处理完 `agentq retry <编号>`。你可以直接在 `~/.agentq2/orders/<编号>/trees/all` 里改，接着做时会把你的改动提交进去。
- 不想做了：`agentq stop <编号>`，分支和 worktree 留着，`retry` 能接上。

## 怎么卸

```sh
agentq ls                  # 还有在做的先停：agentq stop <编号>
agentq uninstall           # 卸服务；派活技能没改过就删，改过的留着
```

`uninstall` 留下程序和数据，正在做的会做完。要删干净：

```sh
rm ~/.local/bin/agentq
rm -r ~/.agentq2           # 记录、没合的 worktree、Telegram 密钥一起没了
```

没合完的需求在项目里还留着登记和分支，到项目目录里清：`git worktree prune`，再用 `git branch --list 'agentq/*'` 看、`git branch -D` 删。

## 从旧版怎么换

旧版指公开发布过的 v0.1–v0.3。两版是完全不同的程序，共用 `agentq` 这个命令名，要先卸旧的。**这条路没有在真的旧版安装上实测过**，下面的旧版命令照旧版文档写。

1. `agentq pause`，不再开新活；等在做的做完，或 `agentq stop <任务id>` 停掉。
2. `agentq remove --yes`。卸掉旧版的程序、服务和技能，配置、任务、密钥留在 `~/.agentq`。
3. 跑[安装命令](#前提和安装)，再跑 `agentq install`。

- 顺序不能反：安装脚本认出旧版会拒绝，什么都不改。两个版本同时收同一个 bot 会互相抢消息。
- 在旧版里跑 `agentq update`，取到的就是新版的安装脚本，会看到同样的拒绝说明；旧版的自动更新不会换成这一版（Release 里没有它认的 `VERSION` 文件）。
- 数据分开放：新版用 `~/.agentq2`，不读旧版的配置和任务记录。只有 Telegram 密钥例外，新的没写就用旧的。
- 项目里的 `docs/BACKLOG.md` 照用，只认「## 待做」。待办上写着做到一半的分支的，产品经理会先看那条分支。
- 旧版装过、你又改过的技能可能留在 `~/.claude/skills`。`dispatch` 还在的话，`agentq install` 会说没动它，见下一节。
- 要退回旧版：`agentq uninstall`、删掉 `~/.local/bin/agentq`，再照旧版卸载时打印的 restore 命令恢复。也没实测过。

## 派活技能

`agentq install` 把 `dispatch` 技能装到 `~/.claude/skills/dispatch/SKILL.md`。之后在 Claude Code 里说要做一件开发的事，它会用 `agentq do` 派出去；问进度、看待办、停、接着做也可以直接说。

你已经有同名的、或者改过我们装的，就不覆盖，`agentq install` 会写明路径。想用自带的：把那个目录挪到 `~/.claude/skills` 外面，再跑一次 `agentq install`。想两个都要：把[自带的内容](skills/dispatch/SKILL.md)合进你自己的。

仓库里另有两个技能不会自动装：`new-project`（从一句话建新项目）、`project-status`（看全部项目的进度）。要就自己拷，`-n` 不覆盖已有的：

```sh
mkdir -p ~/.claude/skills
cp -Rn skills/new-project skills/project-status ~/.claude/skills/
```

## 安全

- **会话用 `--permission-mode bypassPermissions` 跑**：等于以你的用户权限执行任何命令，不再确认。只在你信得过的仓库上用。
- 「不付款、不发布、不推远程、不删数据」是写给模型的规矩，不是沙箱。程序保证的只有：每块在自己的 worktree 里做，主分支只收 `.agentq/verify` 过了的成品，只快进、不覆盖你没提交的改动。
- 会话会上网、会读仓库里的文件，里面夹带的指令模型有可能照做。
- Codex 审计在 `workspace-write` 沙箱里跑，能联网，只能写审计那份 worktree 和临时目录。
- Telegram 只认你那个 chat id 的私聊，但能用你 Telegram 账号的人就能让它在你的机器上干活；谁拿到 bot token 谁就能冒充它给你发消息、读走你发的消息。密钥文件权限 600。
- 传给会话的环境变量是白名单。认 Claude Code 的登录和 `CLAUDE_CODE_OAUTH_TOKEN`；`ANTHROPIC_API_KEY`、Bedrock、Vertex、`CLAUDE_CONFIG_DIR` 不传。launchd 起的不带 `CLAUDE_CODE_OAUTH_TOKEN`，也不带代理变量。
- agentq 自己只连 Telegram，没有遥测。需求和代码会发给 Claude 和 Codex。

## 已知限制

- 只支持 macOS，没有 Linux、Windows。
- 命令行输出、提示词、待办的小节名（「## 待做」）都是中文。用别的语言写需求没有试过。
- 同时开工的会话不限个数，额度用得快；撞上限会自己等。
- 只在作者一台 Apple 芯片的机器上装过，没有在别人的机器上验证过。
- Intel 包是交叉编译的，没在 Intel 机器上跑过。
- Release 工作流没触发过；`install.sh` 只对本地的假 Release 测过，没从 GitHub 上真装过。
- 从旧版 `agentq remove --yes` 再装这条路没实测。
- 没做 Apple 公证，完整性靠 `SHA256SUMS`。
- `opus`、`sonnet`、`fable` 是 Claude Code 的别名，指到哪个版本由它定。你的账号没有 `gpt-6.2-astra` 就换 `AGENTQ_AUDIT`，不换也会自动改用 Claude 审。
- `go install` 装不了（module 名是 `agentq`），从源码装要先克隆。

以上「没验证」都是 2026-11-26 的情况。

## 许可

[MIT](LICENSE)
