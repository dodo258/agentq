# Support matrix

[简体中文](support-matrix.md)

How far each item has been verified, stated plainly. "Fake CLI" means it ran against a fake `claude`, with no model usage. "Real machine" means macOS, Claude Code 2.1.316, a subscription login and a real model.

Backends: the Claude CLI, the Codex CLI and opencode (experimental, the real CLI has never been run). The interface for other CLIs is in [docs/backends.md](../backends.md); `agentq doctor` prints the capabilities of the current backend and of those in `project_backends`.

| Platform | Status | Verified to what point |
| --- | --- | --- |
| macOS arm64 (launchd) | Primary | Real launchd and the real Claude CLI have run tasks |
| macOS amd64 | Builds | Cross-compiled, never run on an Intel Mac |
| Linux amd64 (`systemd --user`) | CI verified | GitHub Actions ubuntu-latest: all tests, plus real user-service install, start and stop, task survival, upgrade, rollback, removal and restore. Uses a fake claude; no real model task and no long-term run |
| Linux arm64 | Builds | Cross-compiled |
| WSL | Unverified | Not claimed as supported |
| Windows native | Not supported | |

| Capability | Default on new install | Verified to what point |
| --- | --- | --- |
| One-line install `install.sh`, Enter to start, `init`, `add` | Run once by hand | Real machine: install and upgrade with the official CLI (existing-user environment), with the configuration byte-for-byte unchanged and existing same-named skills kept. GitHub Release: v0.1.0 is published, the macOS and Linux workflows passed, and an anonymously downloaded `install.sh` installed successfully in a temporary HOME (with no terminal it installs without starting). Other paths ran with a fake CLI, fake service manager and a local fake Release; terminal and shell-config cases have run on Linux in CI. The script installing the CLI and signing in for a new user (no CLI, no login) is outside this real verification |
| Assigning work from the Claude desktop app | Manual | Real machine: a desktop session used `dispatch` to write an item and start it, through to the merge |
| Backend choice `backend`, `project_backends`, `backends`, `install.sh --backend`, capability fallbacks | `claude` | Fake backend, every fallback path covered; `install.sh --backend` run with a fake CLI and a local fake Release |
| Usage-limit cooldown and quota per backend when mixed (`backend_limits`) | Key not written with a single backend | Fake CLI and stress tests. Two real backends side by side and a real usage limit have not been run |
| Codex backend | Choose it yourself | codex-cli 0.166.0, macOS arm64, ChatGPT login, temporary project: a real run went from start, branch commit and `ready.json` through the test gate and review gate to the engine merge, and a SIGTERM resumed on the same session through to the merge. Usage limits were only tested with samples built from the official error strings, an expired token has no real sample (only the 401 with no credentials), and Router, suggestions, sensing and release prep do not start on Codex. Assigning work from the Codex desktop app is not verified. Linux has only run the fake CLI. Token refresh under a background service (launchd) is not verified |
| opencode backend (experimental) | Choose it yourself | **The real opencode has never been run**: it isn't installed here, and the backend follows the v1.19.5 docs and source. A fake CLI and stress tests (real binary) cover start, branch commit, `ready.json`, test and review gates, engine merge and resume after a limit. The event samples are hand-written. Review verdicts depend on asking for JSON in the prompt. Read-only mode, deny rules and isolated MCP are unverified, so tasks that need them (Router, suggestions, sensing, release prep, follow-ups, patrol analysis, direction questions) do not start. Login can't be checked, so it only pauses after the first auth failure. The project has recorded losing the final `step_finish`; a lost one counts as no final state, not as success. Model-provider API key variables are not passed; use `opencode auth login` |
| Scheduling, concurrency, resume, usage and quota, breaker | Paused, `agentq start` turns it on | Real runs on macOS arm64. Real machine: `agentq start` installed the launchd services, and the timer and the tick after each task carried work through to the merge. A real usage limit and an expired token have not been run; those failure paths are covered by fake CLI and stress tests |
| Excluded projects `exclude_projects` | Empty | Fake CLI |
| Outside projects `extra_projects`, first `add` onboards | Empty, registered by `add` | Fake CLI, through to a tick taking the task |
| General engineering rules `agentq env` | Installed on first consent, skipped if you have global rules | Fake CLI, checks the files. That the real CLI reads them was confirmed by hand in the desktop app, there is no automated acceptance |
| Generated `.agentq/verify` | Written on the first `init` or `add` | The generated scripts run with fake tools, one fixture per project type. Real npm, cargo, uv, poetry and swift have not been run |
| Model following, `model_policy` | Empty on new configs, `fixed` | Fake CLI, checks argv. Which model the real CLI picks without these flags has no automated acceptance |
| Disk cleanup (`launchd.log`, task records older than `retention_days`) | On, 90 days | Fake data, including clock jumps and symlinks |
| `agentq new` | Available manually | Real machine: one line created a new project, which passed the test, product and review gates, merged and produced a handoff sheet. Also covered by fake CLI and stress tests |
| Inbox | Available manually | Fake CLI and stress tests |
| Quality gates and engine merge | On, test and review gates at `error`; patch, UI and product gates at `warn` | Real machine: an ordinary task reached a branch, passed the test gate and the review gate (real Claude), and the engine merged it and closed the BACKLOG item. The test gate reverted a fix, ran again and confirmed the new test failed. The UI gate reviewed two real screenshots (desktop and phone) against `no-ai-look` and passed; the screenshot script was added by the task itself. Given contradictory requirements, the task did not force a result, handed the decision to the user and did not merge. Also covered by fake CLI and stress tests (real binary). For review on Codex see that row; it has not run on opencode. The Playwright and `simctl` sample scripts for the UI gate were only syntax-checked, not run on a real project; thresholds have not been calibrated on real projects. The gates have not been compared with a bare CLI on real projects |
| Direction questions | On | Real machine and real Telegram: the chosen direction was written into the item and work started |
| Night shift, daily review | Off | Fake CLI and stress tests |
| Patrol | Off | Fake CLI and stress tests |
| Local notifications, task result notices | On when you consent in the one-line installer; `agentq start` leaves them off | The completion notice works on real Telegram. The rest use fake CLI and stress tests; no real desktop popup verified |
| Default branch detection | On | Fake CLI and stress tests (real git repositories), Linux CI |
| Telegram messaging | Off, optional | Basic send and receive used for real. Reply threads and full conversation round trips tested only against a fake service |
| Decision card buttons | With Telegram | Button callbacks work on real Telegram |
| Router | Off | One round with the real CLI on temporary data; no real Telegram round trip |
| Idle-time suggestions | Off | Fake CLI and stress tests |
| Feedback sensing | Off | App Store Connect API and `gh` search tested only against fake services |
| Release assistant | Off | App Store Connect API tested only against a fake service; nothing has really been submitted |

Stress tests (killing processes, corrupting state files, concurrency, full disk, clock jumps) have run on macOS, with every dependency faked. A manual Linux run is available in CI but has not been run yet. See [CONTRIBUTING](../../CONTRIBUTING.md#暴力测试).

Not yet verified for real: assigning work from the Codex desktop app; a real usage limit and an expired token; real tasks and stress tests on Linux (Linux has only the CI tests and the real systemd acceptance); any real opencode run; comparing the gates with a bare CLI on real projects.
