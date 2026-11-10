# Install the complete Claude work system

[简体中文](install.zh-CN.md) · [Agent maintenance](../AGENTS.md) · [llms.txt](../llms.txt)

## One entry point

One path for new and existing users:

```sh
curl -fsSL https://github.com/dodo258/agentq/releases/latest/download/install.sh | sh
```

Install, then just say it: a new install ends up running (services loaded, not paused) without asking or waiting for input; installing is the consent. At the end it says plainly that background tasks can read and write files on this machine, run commands and use your own model quota, and gives the pause and remove commands. Then open Claude Code or Codex and say what you want done. To install without running yet, add `--paused`.

`install.sh` refuses to run as root before writing anything. It checks the platform and Git, gets the program, and invokes the same `agentq setup` engine used by updates. It installs the executable, complete skill directories, support scripts, five templates, locked dependencies and license notices. It does not copy the author's `.claude`, require Node or sudo, or request a Telegram token. Upgrade by running it again; configuration and task data stay.


The program comes from, in order: a local build inside this checkout; the GitHub Release, verified against SHA256SUMS (a mismatch exits); a source build when no Release is reachable and Go is available. `--version` or `AGENTQ_VERSION` selects a version, and `main` builds from source. `AGENTQ_RELEASE_BASE` replaces the download address, taking priority over the repository in the script. `--proxy <url>` (`AGENTQ_PROXY`) routes downloads through a proxy. `--mirror <prefix>` (`AGENTQ_MIRROR`) fetches the release archive from a mirror; the checksum file and source clones still use the original address, so a file the mirror changed is refused and a mirror answering 404 cannot push the script onto an unverified path, and the locked third-party dependencies are checked against their locked hashes. `--uninstall` hands over to `agentq remove --yes` and keeps your data; if the program was deleted by hand it says what may be left instead of claiming a clean removal.

A missing or logged-out Claude CLI does not block the install. The install runs right away with or without a terminal, in CI too; only `--paused` (`AGENTQ_PAUSED=1`) installs without running and records no consent. The old `--yes` is still accepted and does nothing. A configuration that existed before the install and says paused (old binary-only users) stays paused. With a terminal, a missing CLI is installed with the official `curl -fsSL https://claude.ai/install.sh | bash`; on failure it stops, shows the original error and offers `brew install --cask claude-code` and `npm install -g @anthropic-ai/claude-code`. A logged-out CLI runs `claude auth login`, and the background stays off until login succeeds. Without a terminal (CI, an agent installing for you) it neither installs the CLI nor logs in: it prints the commands and exits non-zero. Consent and permissions are already recorded, so once the CLI works, saying what you want in a chat is enough: the dispatch skill sees the background is off and runs `agentq start`. When `~/.local/bin` is not on PATH it adds one export line marked `# agentq managed` to your shell config, backing the file up as `*.pre-agentq` first (with a timestamp if a backup exists). `agentq remove --yes` deletes only that line and restores the file byte for byte when nothing else changed; restoring an uninstall does not put the line back. fish and other shells are not edited; you get the line to add yourself. Consent is recorded in `~/.agentq/consent.json` (text version, time, method; `install` for this flow); a new text version does not ask again and is only mentioned by `agentq doctor`. On the first run, general engineering rules go to `~/.claude/rules/agentq/`, unless `~/.claude/CLAUDE.md` or `~/.claude/rules/` already holds your own content, in which case the install message says so and nothing is added; upgrades never add them, see `agentq env`. The dispatch skill goes to Claude Code's `~/.claude/skills`, and also to `~/.agents/skills` when Codex is in use here (`~/.codex` or `$CODEX_HOME` exists); if you install Codex later, `agentq update` adds it and `agentq doctor` reminds you. Upgrades (`agentq update` or rerunning the command) never ask and keep services and the paused state: a paused install stays paused.

Maintainers can also use `sh scripts/bootstrap.sh --yes`, which also runs right away on a new install (`--paused` installs only); options below such as `--plan` and `--extras` work with both entry points (for example `sh install.sh --extras`).

After a `--paused` install, `agentq start` explains the risk of full permissions and asks you first; or say "turn on the background" in Claude Code or Codex.

One sentence for an Agent: **Read this repository's llms.txt and install agentq with one command.** It runs once installed; ask for `--paused` if you don't want it running yet.

## Prerequisites and detection

- macOS 13+, arm64 or amd64. Real acceptance currently covers arm64; Intel execution has not been verified on an Intel Mac.
- Linux amd64 or arm64; background services need `systemd --user` (see "Linux and WSL" below). Verification scope is GitHub Actions ubuntu-latest.
- Git. Go (see `go.mod`) only for source builds; it is not needed to run the installed scheduler. Python 3 is optional: the scheduler and required skills do not use it, only optional Waza helper scripts do, and a missing interpreter produces one warning. Missing prerequisites produce actionable guidance, not a silent system install.
- An independently callable Claude Code CLI, needed to start rather than to install. The engine checks its path, version, print/JSON/session/effort capabilities, and authentication in the CLI worker environment. Use `--claude /absolute/path/to/claude` to select an executable, including paths with spaces. Command names already in configuration (such as `claude`) resolve through PATH. If `--claude` differs from the preserved `claude_path`, installation stops; the operator must explicitly update their own configuration first. The installer does not test one engine and leave workers using another.
- CLI-only works. Desktop-only, an uploaded skill ZIP, or an executable hidden inside an app's private bundle does not qualify. If the CLI is absent, an install at a terminal runs the official `curl -fsSL https://claude.ai/install.sh | bash` and `claude auth login`; without a terminal they are only printed for the operator to run.
- With `--services`, a second probe uses the HOME/PATH declared in the service file (plist on macOS, unit on Linux) without inheriting shell tokens or proxy variables. A shell-only identity cannot authorize a service installation. The operator must explicitly configure durable CLI authentication/network settings; secrets are not written into service files, and the installer does not log in.
- Authentication detection is not proof of a successful model request. Existing, supported gateway/model configuration belongs to the operator; agentq does not change endpoints or billing channels. Parent API variables are not silently inherited by workers; see the [environment allowlist](reference/scheduling.md#环境变量白名单).
- Claude settings are checked for JSON and common field structure, then left byte-for-byte unchanged. Use the official `claude doctor` for comprehensive host diagnostics. The current projection uses `$HOME/.claude`; a custom `CLAUDE_CONFIG_DIR` is not automatically migrated.

If a prerequisite is missing, install it from the documented official source and repeat the same entry point. You do not assemble individual skills, scripts or services manually.

## Installation scopes

| Scope | Add to the entry point | Includes | Does not imply |
|---|---|---|---|
| Complete system / base capability | nothing | agentq, eight owned skills, four required Waza skills, support, templates, licenses and lock | Active scheduling, Telegram, copied permissions or identity |
| Extended capability | `--extras` | Base plus Waza read/write/learn/health | Unverified third-party extensions |
| Skills only | `--scope skills` | Complete skill closure, support, templates and licenses | Background executable or services; CLI login is not required |
| Host plugins | Use the upstream host's plugin flow; provenance is in the lock | Operator-selected host extensions | An agentq background installation or ownership of plugin caches |
| Desktop skill ZIP | Desktop's upload scope | Desktop skill capability only | A CLI engine or scheduler; this stage does not publish a ZIP |
| Scheduler services | `--services` | tick every 5 minutes, night at 01:00, review at 06:30 | Resuming scheduling or installing Telegram listen |
| Telegram listener | Add `--listen` (requires `--services`); `agentq start` adds it when Telegram credentials exist and `notify_enabled=true` | The long-running listen service, kept across upgrades | Credentials or turning notifications on |
| Private code backup | `--backup` | A per-user service every 86400 seconds | Remote writes without explicit targets, or a global Git hook replacement |
| Local watchdog | `--watchdog` | An independent shell check every 1800 seconds, local report | Automatic repairs, notifications, permission changes or resumed scheduling |

Owned skill sources: agent-task, dispatch, nightly-review, new-project, project-status, no-ai-look (with references), prior-art and feedback-triage. Templates: AGENTS, BACKLOG, PROGRESS, DECISIONS and LESSONS. Support: with-lock, ask-rule-scan and watchdog.

Required Waza skills: check, think, hunt and ui. `dependencies/claude.lock.json` records source URL, full commit, MIT license and per-file Git blob hashes. Only declared files are extracted and verified. Upstream installers are not executed, and vendor/cache/identity trees are not copied. `--extras` selects the optional Waza modules. Other frontend-design/iOS/Swift/Obsidian/shaders/document-plugin capabilities remain explicitly declared; unverified sources or licenses are not auto-installed, and existing user modules are not removed.

## Plans, ownership and customization

```sh
sh scripts/bootstrap.sh --plan
```

A plan prepares and self-checks a candidate before printing changes, including each path, before/after entry types and the transaction backup location. Selecting backup also checks the gh executable path and version; remote identity and target authorization are verified when a backup actually runs. **Preparation fetches locked public dependencies and runs agentq's own temporary-HOME checks**; it does not activate entries, run upstream installers or change current settings/services. `--yes` confirms the selected installation changes, not account operations, releases, payments or external messages.

Vendor files live under `~/.local/share/agentq/generations/gen-*/`. The executable and skills are managed links. A receipt records ownership and generation digests; activation keeps restricted-permission transaction backups.

User rules, project instructions, independent skills, third-party hooks/status lines and machine configuration stay outside vendor generations. Do not edit a vendor generation to customize behavior. Installation/update checks before replacing:

- An identical external skill directory stays externally owned and survives uninstall. Its dependency digest is recorded so doctor can detect later removal or edits without adopting or deleting the external directory. A different same-name skill such as `check` stays yours: agentq uses it as is, does not adopt or overwrite it, and says so during install. `agent-task`, `dispatch`, `new-project` and `nightly-review` cannot be substituted, because the task contract depends on them: a different copy blocks activation and is left untouched, so rename it and retry.
- Local edits to managed entries/generations, unknown collisions, loaded services outside the receipt, invalid JSON or common settings-shape errors stop installation. The operator's content remains intact; there is no force-overwrite-all option.
- Existing configuration, unknown fields and scheduler switches remain byte-for-byte unchanged. A fresh installation first writes `paused=true`, disables night/review/patrol/notify, and selects `permission_mode=default`; `start --welcome` then switches it to running: unpaused, full permissions, notify on (none of this with `--paused`). It does not inherit the author's bypass permissions. Legacy configurations retain their existing missing-field bypass semantics rather than having their policy silently changed.
- Installation shares a lock with tick and refuses replacement while jobs are running or state is unknown. Candidate `status`/`tick` self-checks use a fresh temporary HOME. Executables are not overwritten in place or deleted while in use.
- A failed activation restores prior entries and services. An interrupted activation keeps a pending journal and requires restore. Recovery refuses to overwrite user changes made after interruption. Active, previous and removed receipts share complete validation; missing fields cannot be interpreted as instructions to delete entries. Doctor reports an absent managed receipt as not installed. Runtime jobs, secrets, identity and existing user configuration are not rolled back with code.

For a conflict, compare and back up the reported paths, then decide explicitly how to move customization into the user/project layer. Atomic file replacement alone is not customization preservation.

## Use it without Telegram

Open Claude Code or Codex in any folder and state a development request; no trigger word. Standard skill descriptions let the host discover dispatch, which decides whether to do it right there (a question, a trivial edit such as a typo, or "just change it" / "I'm watching") or write it to BACKLOG for the background (a feature, a bug fix or a UI change, however small). It brings unregistered folders in by itself (`git init` with an initial commit and a `.gitignore` that keeps out secrets and dependencies when the folder is not a repository; registration in `extra_projects` outside projects_root), and asks once for a name and location in places that don't look like a project, such as the home folder or Desktop. It first runs the read-only `agentq start --check`: if the services aren't running it runs `agentq start`; a pause you set stays. `agentq run/status/logs` are management commands it runs for you, not a natural-language chat interface.

An explicit `agentq run <project> <keyword>` can start a task even while automatic scheduling is paused; authentication, quota and concurrency limits still apply. Default permissions may reject unattended writes until the operator authorizes an appropriate policy. Installation does not bypass that decision.

Results remain local in `~/.agentq/jobs/<ID>/`, `agentq logs <ID>` and `~/.claude/nightly/<date>-agentq.md` (`~/.agentq/nightly/` when the default backend is not Claude). Telegram credentials and listen are optional, operator-enabled messaging features, not engine credentials. The installer does not request a bot token, install listen by default or send external messages. Once credentials are in place and `notify_enabled` is `true`, `agentq start` installs listen too. The installed launchd services run without an open Desktop/terminal window.

## Update, rollback, remove and restore

- **Update:** repeat the entry point, or run `scripts/update.sh` for a managed installation. It builds a candidate, preserves optional selections/services, and repeats conflict checks without rewriting user configuration. For an unmanaged legacy installation, update.sh retains its binary-only compatibility flow; that is not a complete installation.
- **Rollback:** `agentq rollback --yes` restores the previous executable, skills, support, templates and service entries, not runtime state or configuration. The rollback target is consumed to avoid oscillating between broken versions.
- **Remove:** `agentq remove --yes` (alias `agentq uninstall --yes`) unloads owned services and removes only still-matching owned entries. Configuration, jobs, secrets, generations and backups remain. Local edits block removal rather than being discarded.
- **Restore a removal:** `sh scripts/bootstrap.sh restore --yes`, or the retained generation executable printed by remove. Skills-only installations can also restore via bootstrap.
- **Restore an interruption:** the same restore entry first reverses a pending transaction. Conflicts created after interruption are preserved and reported.
- **Runtime fallback:** after two failing scheduled ticks, the guard tries a whole-generation rollback through the previous executable. Active jobs, unknown state or local modifications block automatic replacement. Without a previous generation there is nothing to roll back to. The independent watchdog writes a local report even if agentq cannot execute.
- Old generations and user data are never automatically purged. Removing the installation is not deleting private data.

The optional backup service requires explicitly authorized local `~/.agentq/backup.json` targets, for example `{"targets":[{"project":"/absolute/project","remote":"backup","url":"https://example.invalid/owner/repo.git"}]}`. Substitute an actual authorized URL; the example is not a working target. Each run checks that the remote push URL is unchanged and gh verifies PRIVATE visibility before pushing branches/tags. Missing targets do nothing; unknown/public/changed targets fail closed. Reports are local `~/.agentq/*-report.txt` files.

## Linux and WSL

The command is the same: `sh scripts/bootstrap.sh --yes`, and `setup`, `doctor`, `rollback`, `remove`, `restore` share one engine. Without services, systemd is not needed: containers can still install skills and run `agentq tick` or `agentq run` by hand.

**Services (`--services`, `--watchdog`, `--backup`, legacy `agentq install`) use `systemd --user` and need systemd 240 or newer:**

| Service | On Linux | macOS equivalent |
| --- | --- | --- |
| tick | `<label>.timer` (every 5 minutes, once on install) + oneshot `<label>.service` | StartInterval 300 + RunAtLoad |
| night, review | calendar `.timer` + oneshot `.service`; no `Persistent`, tick catches up after a shutdown | StartCalendarInterval |
| Telegram listener | long-running `.service`, `Restart=always`, `RestartSec=10` | KeepAlive, ThrottleInterval 10 |
| backup, watchdog | interval `.timer` + oneshot `.service` | StartInterval |

Unit files live in `~/.config/systemd/user/` and are tracked by the receipt; a file of the same name not owned by agentq is never overwritten. The default label is `io.agentq.agentq`. The setting is still called `launchd_label` and the log is still `~/.agentq/launchd.log` on Linux; only the names are inherited. Every service sets `KillMode=process`: tasks are background processes started by tick, and the default `control-group` would kill them when tick exits. The journal will show "Found left-over process" notices as a result; they do not affect operation.

- **Linger:** without it, user services and running tasks stop when you log out and resume when you log in. agentq does not enable it for you; `agentq doctor` and setup report it. To keep things running, run `loginctl enable-linger` yourself.
- **`systemctl --user` unavailable** (WSL without systemd, containers, ssh or su without a user session): `setup --services`, `install`, `uninstall` and `doctor` fail with an explicit reason. Nothing is written and nothing is silently skipped.
- **`XDG_CONFIG_HOME`:** only `~/.config/systemd/user` is used. If you moved your systemd user config elsewhere, `enable` fails and the install rolls back.
- **Local notifications:** shown when `notify-send` and a desktop session exist. Without `notify-send` they are only written to `~/.agentq/launchd.log`. If it exists but fails (no desktop session, for example) the message is not counted as delivered: it stays queued and is retried hourly. Configure Telegram to reach your phone.
- **Media:** screen recordings over 50 MB need `ffmpeg` to compress; without it you get a message with the file path.
- **Mac-only capabilities:** iOS builds, Xcode, simulators, code signing and TestFlight upload require macOS. agentq on Linux can schedule tasks, but such tasks must run on a Mac.

**WSL is unverified.** It has not been run in a real WSL environment and is not claimed as supported. In principle WSL2 with systemd takes the same Linux path: set `[boot]` and `systemd=true` in `/etc/wsl.conf`, run `wsl --shutdown`, reopen, and confirm `systemctl --user status` works. Known limits: WSL may shut down when no window is open, stopping timers and tasks; its clock can drift from Windows (a boot time differing by over 60 seconds, with the task already gone, is treated as a reboot interruption); native Windows is not supported.

## Evidence and upstream ownership

Normal tests use temporary HOME and fake CLI/launchctl: `go test ./...`.

Opt-in formal-entry/network acceptance: `AGENTQ_BOOTSTRAP_ACCEPTANCE=1 go test -count=1 -v -run '^TestBootstrapAcceptance$' ./...`.

Opt-in real-model acceptance: `AGENTQ_REAL_E2E=1 go test -count=1 -v -run '^TestRealClaudeInstallationE2E$' ./...`. A connection-based identity is projected from the caller's existing connection environment only with the additional explicit `AGENTQ_REAL_E2E_CONNECTION=1`; no real HOME is read, and the temporary settings projection is removed afterwards. This consumes authorized model usage and is not a default CI test.

Capabilities and what has been verified are in the [support matrix](reference/support-matrix.en.md). Intel execution, WSL, real opencode runs, public remote downloads and formal releases are not claimed as verified.

Official references, checked 2026-10-13: [Claude setup/stable channel](https://code.claude.com/docs/en/setup), [CLI/auth reference](https://code.claude.com/docs/en/cli-reference), [gh repo view](https://cli.github.com/manual/gh_repo_view). Native Claude updates preserve custom launchers, while ordinary launchers remain managed by the official updater. npm supports automatic updates; Homebrew defaults to manual package-manager updates. These policies do not promise preservation of every user customization. agentq neither overwrites them nor takes over custom CLI launchers.
