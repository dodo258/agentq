# agentq

[中文](README.md)

Say what you want in one sentence. agentq runs a few Claude Code sessions that take it from a plan to passing tests to a merge into your local main branch, then tells you what was done and how to look at it. macOS only.

**The CLI output, the prompts, the Telegram messages and the backlog section names are in Chinese.** This page translates them where needed. Writing requests in another language has not been tried.

## Examples

Both runs are real. They happened on 2026-11-26 in throwaway projects. Times and text are copied from the run logs unchanged. The messages are what the program emitted (the 「消息」 lines of the log); with Telegram set up, that is what arrives on your phone.

**A small change, 1 minute.** Started 03:02:07, merged into main 03:03:11. The request: "add a --version option that prints wc2 0.1.0".

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

(Done. Added `--version` to wc2, plus tests, an `.agentq/verify` script and README usage. To see it, run `go run . --version`. Took 1m0s, merge commit 39854fe.)

**A tool from scratch, 53 minutes.** The request, translated: "Write a command-line tool mdsite in Go that turns a folder of Markdown into a good-looking static site, with full-text search, light and dark themes, local preview and auto-reload. Standard library only."

| Time | What happened |
|---|---|
| 03:02 | Started. The product manager reads the project, looks at similar tools, writes a plan |
| 03:06 | Plan ready, 6 modules; the first one, base, starts. One session failed to launch and was retried automatically |
| 03:14 | base merged; markdown, search, theme and site start at the same time |
| 03:36 | All four merged; the last one, app, starts |
| 03:52 | Everything merged, tests pass first time, audit starts |
| 03:55 | Audit finds 0 major bugs, merged into main |

```
📋 mdsite 开工：mdsite：一个文件夹变成好看的网站
用 Go 标准库做一个单文件命令行工具：指给它一个 Markdown 文件夹，不写配置就出一个能双击打开、中文搜得到、深浅两色的静态网站，mdsite serve 边写边自动刷新。分 6 块：先打公共骨架，再由 Markdown 解析、网站生成、搜索、主题四块同时做，最后一块做预览服务和命令行并接到一起。做好的标准：一条检查命令全过，拿示例文档生成的站点不起服务器也能看能搜，改一个字一秒左右浏览器就变。
```

```
🟢 mdsite 做好了：mdsite：一个文件夹变成好看的网站
用 Go 标准库做一个单文件命令行工具：指给它一个 Markdown 文件夹，不写配置就出一个能双击打开、中文搜得到、深浅两色的静态网站，mdsite serve 边写边自动刷新。分 6 块：先打公共骨架，再由 Markdown 解析、网站生成、搜索、主题四块同时做，最后一块做预览服务和命令行并接到一起。做好的标准：一条检查命令全过，拿示例文档生成的站点不起服务器也能看能搜，改一个字一秒左右浏览器就变。
用了 53m0s，合并提交 59275c2
```

(Started / done: a single-binary tool on the Go standard library; point it at a Markdown folder and it produces a searchable two-theme static site without configuration, and `mdsite serve` reloads as you write. 6 modules: a shared skeleton, then Markdown parsing, site generation, search and themes in parallel, then the preview server and CLI that tie them together. Accepted when one check command passes, the site built from the sample docs works and searches without a server, and an edit shows up in the browser in about a second. Took 53m0s, merge commit 59275c2.)

Codex was not connected yet when these two ran; the mdsite audit was done by Claude. The Codex audit was added later.

## Why it is not a scheduler

The old agentq (v0.1–v0.3) was a scheduler: queues, slots, five gates, patrols, and work on itself. Data from its last 7 days before the rewrite:

- Product code had grown from 1,700 lines to 48,110 lines in 28 modules.
- 183 merged items took 76 minutes on average from start to the last gate. 38 of those were work; the rest was gates, rejections and waiting to be resumed.
- 293 trips through a gate, 106 sent back. Of 45 rejection reasons sampled, most were edge cases needing several rare conditions at once.
- 286 of 324 tasks were started by the scheduler itself.
- 13% of the usage quota went into agentq changing itself.

This version is written from scratch and does one thing: carry one request from start to finish. No queue, no slots, no chain of gates, no evaluation, no self-evolution. Product code is capped at 2,600 lines, enforced by a test. The reasoning is in [docs/DESIGN.md](docs/DESIGN.md) (Chinese).

## One straight line

```
request ─→ product manager ─→ modules in parallel ─→ merge, run tests ─→ audit once ─→ fix once ─→ merge into main ─→ tell you
small   ─→ product manager ─→ one session ─→ run tests ─→ merge into main ─→ tell you
```

1. **Product manager**: reads the project. For larger work it also looks at similar tools and their complaints on the web, writes a plan, splits it into modules, fixes interfaces and acceptance. Small changes (one session, an hour or two) skip the research. It does not ask you questions. Plans for larger work are committed to `docs/specs/` in the project.
2. **Build**: one session and one git worktree per module. Modules without dependencies start together, with no limit on how many.
3. **Merge**: each finished module goes into the request's own branch. The session that wrote it resolves its conflicts.
4. **Test**: the program runs the project's `.agentq/verify` and looks only at the exit code. If it fails, Opus fixes it, at most 3 times; then Fable gets one try.
5. **Audit**: once per request, major bugs only. Small changes are not audited.
6. **Fix**: each finding is checked, the real ones are fixed in one go, tests run again, no second audit. Lessons go into the project's `LESSONS.md`.
7. **Deliver**: merged into your local main branch, one message sent. The program does not push.

It stops for you when: failing tests or audit bugs cannot be fixed; a conflict cannot be resolved; sessions are not logged in, will not run or produce nothing; or the product manager decides you are needed (spending money, publishing, deleting data, accounts). If uncommitted changes in the directory where main is checked out are in the way, it sends one message, waits until you commit or move them, then merges by itself.

Every step is on the handoff sheet: `agentq log <id>`. If you look at the result and want something changed, say so; that is a new request.

## Who does what

| Work | Model | Effort |
|---|---|---|
| Product manager, hard modules, fixing tests and audit bugs, conflicts with main | Opus | high |
| Ordinary modules | Sonnet | high |
| One try after Opus has failed 3 times | Fable | xhigh |
| The single audit | Codex `gpt-6.2-astra`; Opus if Codex is missing, logged out or out of quota | xhigh |
| Cleaning up uncommitted changes | the model that wrote that part | medium |
| Reading Telegram messages, finding the project | Sonnet | medium |
| Ticking off, morning brief, merging, running tests | the program, no model | |

The product manager decides which modules are hard. All models can be changed, see [environment variables](#data-and-environment-variables).

## Requirements and installation

Requirements:

- macOS 13 or later (what Go 1.27 binaries need; it has only been run on macOS 27 on Apple silicon).
- git with `user.name` and `user.email` set.
- [Claude Code](https://code.claude.com/docs/en/setup), installed and logged in. The author uses 2.1.332; nothing older was tried.
- Codex is optional and only used for the audit. The author uses codex-cli 0.172.0.

One command, installs to `~/.local/bin/agentq`. The download is checked against its SHA-256; on a mismatch nothing is installed:

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
agentq install
```

The script only places the program. No sudo, no changes to your shell config; if `~/.local/bin` is not on your PATH it prints the line to add. To pick a version: `curl … | AGENTQ_VERSION=v1.2.3 sh`. If the machine has the old version, or the download turns out to be one, it changes nothing, see [Moving from the old version](#moving-from-the-old-version).

Afterwards the first line of `agentq --help` should be 「agentq：说一句需求，做到合进主分支」. Until the first release of this version is published, the address above still serves the old v0.3.0.

`agentq install` does three things and lists each of them:

- Two launchd services: resume interrupted work every 5 minutes, write the morning brief at 8:00. A third one receives messages if Telegram is set up.
- The [dispatch skill](#the-dispatch-skill).
- The current `PATH` and `AGENTQ_*` variables are written into the services.

`agentq do` works without `agentq install`; you then have to run `agentq resume` yourself after an interruption.

From source (Go 1.27.4 or later):

```sh
git clone https://github.com/dodo258/agentq && cd agentq
mkdir -p ~/.local/bin
go build -o ~/.local/bin/agentq .
agentq install
```

To upgrade, run the install command again, then `agentq install` again. There is no automatic upgrade.

## All commands

| Command | What it does |
|---|---|
| `agentq do "<request>"` | Start work in the current project. `-C <dir>` picks another project and goes before the request; `-` reads the request from standard input |
| `agentq todo` | Show the project's backlog (also takes `-C <dir>`). Add a keyword to do that item, or `all` to do everything that can start and keep going |
| `agentq ls` | Show progress. `-a` includes finished ones |
| `agentq log <id>` | Show the handoff sheet |
| `agentq stop <id>` | Stop it; branches and worktrees stay |
| `agentq retry <id>` | Continue a stuck or stopped one from that step |
| `agentq resume` | Continue interrupted ones (runs every 5 minutes once the services are installed) |
| `agentq brief` | Send the morning brief: done yesterday, stuck, tokens used |
| `agentq install` | Install the dispatch skill, auto-resume, the 8:00 brief, and message receiving if Telegram is set up |
| `agentq uninstall` | Remove what `install` installed |
| `agentq version` | Version; `dev` if you built it yourself |

A project must be a git repository with at least one commit. The main branch is what `origin/HEAD` points to, otherwise `main`, `master`, otherwise the current branch.

## The backlog

It lives in `docs/BACKLOG.md` and counts only once committed to main. The section names and markers have to be in Chinese:

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

(Three items: a statistics page; CSV export, which depends on the statistics page; login, stuck waiting for an account.)

- Only unindented `- [ ] ` lines under `## 待做` (to do) are read, one item per line. The part before `——` is the title; the whole line is the request.
- `（依赖：…）` (depends on) takes a few characters of another item's title, several separated by commas. It waits as long as a matching item is still under `## 待做`.
- Items containing `（卡住` (stuck) are skipped by `all`; naming one does it anyway.
- `agentq todo <keyword>` does the first item whose title contains the keyword and that can start now. Items in progress, waiting or stuck last time are not started again (use `agentq retry` for stuck ones).
- `agentq todo all` starts everything that can start. Each finished item is ticked and moved to `## 已完成` (done), then whatever it blocked starts. Items of the same title already done or stopped are not redone by themselves.
- It never picks work from the backlog unless you say so. For projects done in phases, the product manager adds the later phases to the backlog.

## The project check: .agentq/verify

An executable script that runs all of the project's checks (tests, static checks, build) in one command. Exit code 0 means pass. For example:

```sh
#!/bin/sh
set -e
npm ci
npm run lint
npm test
```

- No script counts as a failure; the product manager will have the first module add one.
- It runs in a worktree with its own TMPDIR, for at most 30 minutes.
- The environment is a whitelist (`HOME`, `PATH`, locale, proxies, `GOTOOLCHAIN`, `GOROOT`, `CODEX_HOME`, `AGENTQ_*` and the like). Set anything else inside the script.
- It is the floor for quality: the program knows only what this script tests.

## Telegram

Optional. Without it, use `agentq ls`; the morning brief is in `~/.agentq2/brief.md`.

1. In Telegram, find @BotFather, send `/newbot`, note the token.
2. Send your bot a message, open `https://api.telegram.org/bot<token>/getUpdates` in a browser and find the number in `"chat":{"id":…}`.
3. Write `~/.agentq2/telegram.env`:

   ```
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_CHAT_ID=<number>
   ```

4. `chmod 600 ~/.agentq2/telegram.env`
5. Run `agentq install` again to install the receiving service. `agentq brief` sends a message right away, which is a quick test.

Only private messages from that chat id are accepted; groups are ignored. The message from step 2 may be received and answered too. Each message is handled by a session: if it is work, it finds the project and starts; if it is a question, it answers. Projects are looked up in `~/projects` (created empty if missing); change that with `AGENTQ_PROJECTS`.

Messages you get: 📋 larger work started, 🟢 done, 🔴 stuck, 🟠 waiting to merge, ☀️ morning brief, and answers to you. Unsent messages are sent later. Messages Telegram refuses (a wrong chat id, for example) are kept, the reason is reported, and they go out once it is fixed.

If `~/.agentq2/telegram.env` does not exist, the data directory is the default one and the old version's `~/.agentq/secrets/telegram.env` exists, that file is used.

## Data and environment variables

Everything is in `~/.agentq2`:

| Location | Contents |
|---|---|
| `orders/<id>/order.json` | state of the request |
| `orders/<id>/HANDOFF.md`, `run.log` | handoff sheet, run log |
| other files in `orders/<id>/` | session output, test and audit logs |
| `orders/<id>/trees/` | worktrees, removed after the merge into main |
| `brief.md`, `resume.log`, `listen.log`, `brief.log` | morning brief, service logs |
| `inbox/`, `outbox/`, `chat-*` | received but unhandled, not yet sent, chat session output |
| `telegram.env` | Telegram credentials |
| `skill-dispatch.md` | the skill text as installed, compared on uninstall |
| `todo.lock`, `locks/` | file locks |

Inside a project you will find `agentq/<id>/…` branches (removed once merged), `.git/agentq.lock`, and committed `docs/specs/`, `LESSONS.md` and backlog ticks. Records are not cleaned up automatically.

There is no config file, only environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `AGENTQ_HOME` | `~/.agentq2` | data directory |
| `AGENTQ_PROJECTS` | `~/projects` | where projects named in Telegram messages are looked up and where the dispatch skill creates new ones |
| `AGENTQ_TELEGRAM` | see above | Telegram credentials file; point it at a missing file to neither send nor receive |
| `AGENTQ_TELEGRAM_API` | `https://api.telegram.org` | Telegram API address |
| `AGENTQ_CLAUDE` | searched for | the claude program. Looks in PATH, `~/.npm-global/bin`, `~/.local/bin`, `~/.claude/local` |
| `AGENTQ_CODEX` | searched for | the codex program, same search |
| `AGENTQ_LEAD` | `opus` | product manager, hard modules, fixes |
| `AGENTQ_CODE` | `sonnet` | ordinary modules, chat |
| `AGENTQ_LAST` | `fable` | the one last try |
| `AGENTQ_AUDIT` | `gpt-6.2-astra` | model for the Codex audit |

The services use `AGENTQ_*` and `PATH` as they were when you ran `agentq install`; `agentq do` uses those of its terminal. To change one, put it in your shell config and run `agentq install` again so both agree.

The install script also reads `AGENTQ_VERSION` (which version) and `AGENTQ_RELEASE_BASE` (another download address, for testing).

## After an interruption

- Each request is its own process; closing the terminal does not matter. Every step is saved when done.
- Process killed or machine restarted: with the services installed it continues within 5 minutes, otherwise run `agentq resume`. `agentq ls` marks these 「断了，等接着做」 (interrupted, waiting to continue).
- Usage limit hit: the session sleeps until the reset and continues; `agentq ls` shows until when.
- Session errors: retried with back-off; stuck after 4 failed runs. A session may run 4 hours, the Codex audit 3 hours.
- Stuck (🔴): see why with `agentq log <id>`, deal with it, then `agentq retry <id>`. You may edit directly in `~/.agentq2/orders/<id>/trees/all`; your changes are committed when it continues.
- Not wanted any more: `agentq stop <id>`. Branches and worktrees stay, `retry` picks it up again.

## Uninstalling

```sh
agentq ls                  # stop what is still running: agentq stop <id>
agentq uninstall           # removes the services; the skill too, unless you changed it
```

`uninstall` leaves the program and the data, and running requests finish. To remove everything:

```sh
rm ~/.local/bin/agentq
rm -r ~/.agentq2           # records, unmerged worktrees and Telegram credentials go with it
```

Unmerged requests leave a worktree entry and branches in their project. Clean up there with `git worktree prune`, list with `git branch --list 'agentq/*'`, delete with `git branch -D`.

## Moving from the old version

The old version means the published v0.1–v0.3. The two are entirely different programs sharing the command name `agentq`, so the old one has to go first. **This path has not been tested on a real old installation**; the old commands below are taken from its documentation.

1. `agentq pause`, so nothing new starts. Wait for running tasks, or `agentq stop <task id>`.
2. `agentq remove --yes`. Removes the old program, services and skills; configuration, tasks and credentials stay in `~/.agentq`.
3. Run the [install command](#requirements-and-installation), then `agentq install`.

- The order matters: the install script refuses and changes nothing when it finds the old version. Two versions receiving from the same bot take messages from each other.
- `agentq update` in the old version fetches the new install script and shows the same refusal. The old automatic update will not switch to this version (the release has no `VERSION` file, which it looks for).
- Data is separate: the new version uses `~/.agentq2` and does not read old configuration or task records. The one exception is the Telegram credentials, used if there are no new ones.
- `docs/BACKLOG.md` in your projects keeps working; only `## 待做` is read. If an item names a half-finished branch, the product manager looks at that branch first.
- Skills of the old version that you modified may remain in `~/.claude/skills`. If `dispatch` is still there, `agentq install` will say it was left alone; see the next section.
- Going back: `agentq uninstall`, delete `~/.local/bin/agentq`, then use the restore command the old version printed when it was removed. Not tested either.

## The dispatch skill

`agentq install` puts the `dispatch` skill at `~/.claude/skills/dispatch/SKILL.md`. After that, tell Claude Code about a piece of development work and it hands it off with `agentq do`. You can also ask it for progress, the backlog, to stop or to continue. The skill text is in Chinese.

If you already have one by that name, or changed ours, it is not overwritten and `agentq install` prints the path. To use ours, move that directory out of `~/.claude/skills` and run `agentq install` again. To keep both, merge [ours](skills/dispatch/SKILL.md) into yours.

Two more skills in the repository are not installed automatically: `new-project` (create a project from one sentence) and `project-status` (progress across projects). Copy them if you want; `-n` does not overwrite:

```sh
mkdir -p ~/.claude/skills
cp -Rn skills/new-project skills/project-status ~/.claude/skills/
```

## Security

- **Sessions run with `--permission-mode bypassPermissions`**: they can execute any command with your user's rights without asking. Use it only on repositories you trust.
- "Do not pay, publish, push or delete data" is an instruction to the model, not a sandbox. What the program guarantees is: each module is built in its own worktree, main only receives work that passed `.agentq/verify`, and it only fast-forwards and never overwrites your uncommitted changes.
- Sessions browse the web and read files in the repository; a model may follow instructions planted there.
- The Codex audit runs in its `workspace-write` sandbox with network access and can write only to the audit worktree and temporary directories.
- Telegram accepts only private messages from your chat id, but anyone who can use your Telegram account can have it work on your machine. Anyone with the bot token can message you as the bot and read what you send it. Keep the credentials file at mode 600.
- The environment passed to sessions is a whitelist. Claude Code's own login and `CLAUDE_CODE_OAUTH_TOKEN` work; `ANTHROPIC_API_KEY`, Bedrock, Vertex and `CLAUDE_CONFIG_DIR` are not passed. Work started by launchd does not get `CLAUDE_CODE_OAUTH_TOKEN` or proxy variables either.
- agentq itself connects only to Telegram and has no telemetry. Requests and code are sent to Claude and Codex.

## Known limitations

- macOS only; no Linux, no Windows.
- CLI output, prompts and the backlog section names (`## 待做`) are in Chinese. Requests in other languages have not been tried.
- No limit on sessions running at once, so quota goes fast. On hitting the limit it waits by itself.
- Only ever installed on one machine, the author's, with Apple silicon. Not verified on anyone else's.
- The Intel package is cross-compiled and has not been run on an Intel machine.
- The release workflow has never been triggered. `install.sh` has only been tested against a local fake release, never a real one from GitHub.
- Going from the old version via `agentq remove --yes` to this one has not been tested.
- Not notarized by Apple; integrity rests on `SHA256SUMS`.
- `opus`, `sonnet` and `fable` are Claude Code aliases, and it decides which version they mean. If your account has no `gpt-6.2-astra`, set `AGENTQ_AUDIT`; otherwise the audit falls back to Claude by itself.
- `go install` does not work (the module is named `agentq`); clone to build from source.

Everything described as not verified is as of 2026-11-26.

## License

[MIT](LICENSE)
