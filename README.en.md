<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/logo-dark.svg">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/logo-light.svg" width="96" alt="agentq">
</picture>

# agentq

### Say it once. It gets merged.

**Bug fixes, missing tests, the first version of a new project. Queue them up.**<br>
Separate Claude Code processes do the work, and agentq merges it only after tests and a review pass. Interrupted work resumes, usage limits are waited out, and every result is on record.

[![License](https://img.shields.io/badge/license-MIT-16181D)](LICENSE) ![Go](https://img.shields.io/badge/Go-16181D?logo=go&logoColor=white) ![macOS](https://img.shields.io/badge/macOS-16181D?logo=apple&logoColor=white) ![Linux](https://img.shields.io/badge/Linux-16181D?logo=linux&logoColor=white) ![Claude Code](https://img.shields.io/badge/Claude_Code-16181D?logo=claude&logoColor=white)
[![Release](https://img.shields.io/github/v/release/dodo258/agentq?label=release&color=16181D)](https://github.com/dodo258/agentq/releases/latest) [![Stars](https://img.shields.io/github/stars/dodo258/agentq?style=flat&color=16181D)](https://github.com/dodo258/agentq/stargazers)

**[Quick start](#quick-start)** &nbsp;·&nbsp; **[Install guide](docs/install.en.md)** &nbsp;·&nbsp; **[Support matrix](docs/reference/support-matrix.en.md)** &nbsp;·&nbsp; **[Security](SECURITY.md)** &nbsp;·&nbsp; [简体中文](README.md) · **English**

<br>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/hero-dark.png">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/hero-light.png" width="900" alt="Install with one command and press Enter to start; agentq add queues a task; agentq status shows the scheduler, quota and running tasks; the task passes the gates and merges. Output is in Chinese for now.">
</picture>

<sub>Command output is in Chinese for now. Items can be written in any language.</sub>

</div>

<br>

## Is agentq for you?

Claude Code gets a lot done, but it waits for you. When one task ends you start the next. When it stops midway you have to notice. When it hits a limit you have to remember to come back. agentq takes that over.

- **One after another**: each project keeps a `docs/BACKLOG.md`, and the next item starts seconds after the last task ends. No terminal or client has to stay open.
- **Interruptions resume**: a process exit, a reboot or a usage limit continues in the same session instead of starting over.
- **"Done" gets checked**: a task saying it is done does not count. It only gets its branch ready to merge. agentq runs the project's check script on the merged code (generated from the project's files on first takeover; if it can't tell, the task adds one), then has a separate process review it, and only then merges to the default branch. A failure goes back to the task; if it still fails, it waits for you.
- **Everything in one install**: the skills for the task workflow, review and wrap-up come with it, plus a set of general engineering rules. No plugins to find, no rules to write. If you already have global rules, this set isn't added (to add it: `agentq env on`), and your own configuration stays as it was.

It's for you if you have more than one Git project and want to hand work off and look at the results later. If you want to watch and steer as it goes, open Claude Code directly. You don't need agentq for that.

## Quick start

**1 · Install.**

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

<sub>Downloads a prebuilt binary and checks its SHA256. No sudo, no Go. New and existing users run the same command: running it again upgrades and keeps your configuration and task data. If the Claude Code CLI is missing, it runs the official installer and sign-in after you agree, including when only the desktop app is installed. Behind a firewall, use `--proxy <url>` or `--mirror <prefix>`: the checksum file and source never come from the mirror, so a changed file is refused. Remove it with `curl … | sh -s -- --uninstall` or `agentq remove --yes`; both keep your data.</sub>


Or tell your agent:

> Read `https://github.com/dodo258/agentq/blob/main/llms.txt` and install agentq for me

**2 · Press Enter.** After installing it spells out three things: tasks can read and write every file your user can; task content goes to the model service you use, on your own quota; with nothing to do, it uses no quota. Enter agrees and starts it, `n` only installs. With no terminal (CI, or an agent installing for you) it installs without starting, and never agrees on your behalf.

**3 · Say what you want done.** Open Claude Code in your project, the CLI or the desktop app:

```text
派活：Retry an export 3 times when it fails, and show the retries in a log when offline
```

`派活` ("assign this") triggers the dispatch skill. It writes the request as an item with acceptance criteria and hands it to the background, and you carry on with something else.

<sub>A project must be a Git repository. Its default branch is detected: `main` or `master` if the working directory is on one of them, otherwise `origin/HEAD`, then a local `main` or `master`. It doesn't have to sit under `~/projects`.</sub>

Or stay in the terminal:

```sh
agentq add "..."                  # queue one line for the background; the first one brings the project in
agentq run my-app                 # start now rather than at the next round
agentq new "..."                  # build a new project's first version from one line
agentq discuss                    # see the big-direction questions waiting on you
agentq inbox add "..."            # work outside any project: research, writing, sorting files

agentq status                     # quota, what's running, each project's queue, what just ended
agentq logs 1105-211629-shop-api  # one task's full result
agentq stop 1105-211629-shop-api  # stop a task; its work stays
agentq pause                      # take no new work; agentq resume undoes it
agentq doctor                     # read-only check of install, CLI and config
agentq update                     # upgrade to the latest release
```

## What it works with

- **Tools**: Claude Code, with background tasks on the official CLI and your own account. The Codex CLI also works as a backend: one real run on macOS with a ChatGPT login went all the way to a merge and resumed after an interruption. Tasks that need tool limits (Router, suggestions, sensing, release prep) do not start on it, and it has no dollar cap. opencode is experimental: the real CLI has never been run, only a fake one. Pick one at install time with `--backend codex` or `--backend opencode` (`... | sh -s -- --backend codex`); the [interface for new tools](docs/backends.md) is in place.
- **Clients**: The desktop app's Code tab and the CLI read the same rules and skills, so one line in either assigns work. Assigned tasks run in the background and don't appear in the client's session list; see results with `agentq status` or notifications.
- **Systems**: macOS (launchd), Linux with `systemd --user`. Windows is not supported; WSL is unverified.
- **Dependencies**: Git. The prebuilt binary needs nothing else; building from source uses only the Go standard library.

How far each item has been verified: [support matrix](docs/reference/support-matrix.en.md).

## How it fits together

agentq writes no product code. Picking work, starting it, watching it, running the gates, merging, wrapping up and keeping records are its job. Building and self-testing are done by Claude Code, following the bundled `agent-task` skill; review is done by a separate Claude Code process.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/flow-en-dark.svg">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/flow-en-light.svg" width="900" alt="You say it, and it goes into docs/BACKLOG.md. agentq tick checks conditions and starts a separate claude -p process in its own worktree, which builds, self-tests and writes ready.json. agentq runs the gates (tests, review); when they pass, the engine merges and wraps up. A failed gate sends the work back to the task, and if it still fails it waits for you. Interrupted or limited work resumes in the same session. Results go to the daily digest and morning report.">
</picture>

agentq never runs `git push`; pushing to a remote needs separate authorization. Full rules: [scheduling](docs/reference/scheduling.md) (in Chinese).

## What it does

### Unattended doesn't mean unwatched

The fear with unattended work is something going wrong at 3am with nobody knowing, or one error burning through the whole backlog. Each of these has a defined response and tests behind it.

| What happens | What agentq does |
| :-- | :-- |
| **A process exits midway** | Checks the BACKLOG and resumes the same session; what is already merged isn't redone |
| **A usage limit is hit** | Cools down until the reset time, then continues |
| **Quota is nearly gone** | Takes no new work at 90%; manual starts are refused at 98% too |
| **Everything fails at startup** | Three tasks exiting within 3 minutes trip a breaker, starting at a 30-minute cooldown, so the backlog isn't burned through |
| **A task runs away** | A per-run budget cap, an automatic stop on subagent runaway and a 4-hour default timeout. The work is kept |
| **The state file breaks** | Unreadable: nothing is wrapped up or started. Corrupt: the bad file is backed up, state is rebuilt from task records, and no new task starts that round |
| **Disk** | Task records are kept 90 days by default and logs are trimmed |

<sub>The stress test kills processes, corrupts state files, forces concurrency, fills the disk and jumps the clock, all with fake dependencies. The latest macOS run was 3 seeds at 40 rounds each with no failures. On Linux, all unit tests and a real `systemd --user` flow have run; the stress test has not. Method: [CONTRIBUTING](CONTRIBUTING.md#暴力测试).</sub>

### "Done" has to add up

A task saying it finished is only its word. All it can do is get its branch ready to merge and write `.agentq/ready.json`. Whether it merges is agentq's call:

- **Test gate**: merge the task branch with the latest default branch and run the project's `.agentq/verify` in a throwaway worktree.
- **Review gate**: a separate process reviews the change. A blocking finding has to come with evidence.
- **Patch gate**: reads the git history and records a third fix to the same module within 30 days. Set it to `error` and the task is sent back with its contract changed to rewriting that module.
- **UI gate**: runs only when UI files change. A project script takes screenshots, the engine checks they aren't blank, then a review process looks at them. Only evidence-backed problems can block: broken rendering, overflow, unreadable text, placeholder content. Whether it looks good is only an opinion.
- **Product gate**: runs only on the first version from `agentq new` and items marked `（成品）`. The handoff sheet, saying what was done and how to open it, comes only after the project's smoke test passes.
- When they all pass, the engine merges to the default branch, then writes the Done entry and the handoff note.

The test and review gates block merging by default; the other three only record. A project can change this in `.agentq/gates.json`. `agentq health` shows recent changes, fix counts and the two-week rework rate for each module; `agentq stats` shows how much each gate stopped and how often you let something through.

Big-direction questions (design, UI, front-end and back-end choices) get asked once before work starts: at most 3 questions, each with a default answer, no detail questions. When you assign work in a session it asks right there. In the background it sends a Telegram card, waits 2 hours, then goes with the defaults. Other items don't wait.

If a gate fails, the reason goes back to the task and the same session keeps working. If it still fails, or the gate can't tell (say the script reports it can't run this time), the item is marked stuck and waits for you: `agentq gate report` shows why, `agentq gate pass` lets it through, `agentq gate rerun` runs the gates again. A task that merges by itself around the gates is flagged, not counted as passed. The gates guard against shortcuts and mistakes. They are not a security boundary: tasks have full access to your machine. `agentq quality` works out first-time satisfaction and rework rates from commit history, with the evidence for each item.

### Assign and decide from your phone

<table>
<tr>
<td width="55%">

With Telegram set up (optional), message the bot directly, screenshots included.

- Reply to a "decide" message, or tap a button, and the item is updated and work continues
- Reply to a task alert to pick that task back up
- `状态`, `额度`, `暂停` and `恢复` (status, quota, pause, resume) are answered by code and use no quota
- One line can start a new project; when it's done you get the path and merge commit

At most 10 notifications a day, held from 23:00 to 08:00, and one per kind every 30 minutes. When a task is merged you get a notice with the merge commit; failures, interruptions and stuck items are reported too. Without Telegram it's a local desktop notification. [Setup](docs/guide/telegram.md) (in Chinese).

</td>
<td width="45%">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/telegram-dark.png">
  <img src="https://raw.githubusercontent.com/dodo258/agentq/main/docs/brand/readme/telegram-light.png" alt="A Telegram conversation: the bot asks what feel the page should have and offers directions A, B and C; B is picked; when done it reports the merge and the merge commit. Messages are in Chinese for now.">
</picture>
</td>
</tr>
</table>

### Also

- **Inbox** (always available): research, writing and file tidying that belong to no project. Output goes to `~/.agentq/outbox/`.

The rest are off until you turn them on ([configuration](docs/guide/configuration.md), in Chinese):

- **Night shift and review**: move the backlog along at set times, with a report in the morning.
- **Idle suggestions**: with nothing to do, read the projects, queue low-risk work, and write product changes up as suggestions for your yes.
- **Feedback sensing**: read App Store reviews, TestFlight feedback and GitHub issues, separate real from noise, and write real problems up as suggestions.
- **Release assistant**: watch release conditions on App Store Connect, tell you when they're met, and submit only when you reply `发`.
- **Patrol**: check services, backups, stuck tasks, quota and disk every hour.

## The cost

Unattended coding needs `bypassPermissions`. A task runs as your OS user with all your file permissions, can read local secrets (including App Store Connect private keys and SSH keys), and can run any command. Prompts and deny rules are not a security boundary. agentq guards against a model cutting corners or making mistakes, not against deliberate damage.

So on a sensitive machine, run it under a separate OS user or in a container, give feedback sensing a read-only App Store Connect key, and keep production credentials off it. All of this is stated before you press Enter. Details: [SECURITY](SECURITY.md).

Also worth knowing:

- The machine has to be on. Work continues after sleep or a reboot, but nothing happens while it's off.
- Tasks spend your own model quota, faster the more you queue.
- Command output, notifications and BACKLOG section headings are in Chinese for now (`## 待做` means "to do"). Item text can be in any language. Most guides are in Chinese.

## Privacy

agentq has no telemetry and collects nothing. Your task content goes only to the model service you use. agentq itself connects out only for features you turned on: Telegram, App Store Connect, GitHub.

## FAQ

- **Does it use quota with nothing to do?** No. With an empty queue no model starts. The night shift and patrol analysis only run if you turn them on.
- **My project has its own rules.** Its `CLAUDE.md` and `AGENTS.md` apply as usual, and tasks follow them. The first line of agentq's general rules says project rules come first, but that is only a hint, so they hold general engineering habits and nothing project-specific. Don't want them: `agentq env off`.
- **Which model does it use?** New installs follow the model and effort you picked in the CLI and pass no flags. A model written in an existing config is used as is.
- **Does it push to my remote?** No. Tasks merge to the local default branch. Pushing needs separate authorization.
- **How do I stop it right now?** `agentq pause` takes no new work, and `agentq stop <job id>` stops a running task.
- **How do I remove it?** `agentq remove --yes` takes off the program and services and keeps your data and recovery material. To undo that, see the [install guide](docs/install.en.md).
- **Can an upgrade lose anything?** Upgrades are transactional and keep your configuration and task data. `agentq rollback --yes` returns to the previous version.

## Why

Most people who install a CLI or client don't go on to configure much. agentq aims to be the one install you need before getting to work. Newcomers skip learning plugins, rules and docs first. Veterans queue the work and go do something else, with their existing setup untouched.

## Documentation

| Document | What's in it |
| :-- | :-- |
| **[Install guide](docs/install.en.md)** | Candidate self-check, conflicts, transactional backup and rollback |
| **[Support matrix](docs/reference/support-matrix.en.md)** | How far each capability has been verified |
| **[Getting started](docs/guide/getting-started.md)** | Install, first task, other routes (in Chinese) |
| **[Configuration](docs/guide/configuration.md)** | Changing directories, controlling cost, the night shift (in Chinese) |
| **[Telegram](docs/guide/telegram.md)** | Setting up the bot, assigning work, deciding (in Chinese) |
| **[Daily use](docs/guide/daily-ops.md)** | Status, stop, resume, decisions, morning report (in Chinese) |
| **[Troubleshooting](docs/guide/troubleshooting.md)** | Look up by symptom (in Chinese) |
| **[Commands](docs/reference/commands.md)** | Every command (in Chinese) |
| **[Config keys](docs/reference/config.md)** | Type, default and effect of each key (in Chinese) |
| **[Scheduling](docs/reference/scheduling.md)** | Picking work, concurrency, quota, breaker, resume, state corruption (in Chinese) |
| **[Features](docs/reference/features.md)** | Inbox, quality gates, sensing, release assistant, night shift, patrol, notifications (in Chinese) |
| **[Files](docs/reference/files.md)** | Every file under `~/.agentq/` (in Chinese) |

Also: [privacy gate](docs/privacy.md), [legacy install](docs/legacy-install.md), [entry point for agents](llms.txt).

## Contributing

Issues, PRs and real-world feedback are welcome; start with [CONTRIBUTING](CONTRIBUTING.md). Report vulnerabilities as described in [SECURITY](SECURITY.md).

## License

[MIT](LICENSE). Sources, versions and licenses of third-party skills are recorded in [`dependencies/claude.lock.json`](dependencies/claude.lock.json).

agentq is an independent project and is not affiliated with Anthropic.
