# Support matrix

[简体中文](support-matrix.md)

How far each item has been verified, stated plainly. "Fake CLI" means it ran against a fake `claude`, with no model usage.

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
| One-line install `install.sh`, Enter to start, `init`, `add` | Run once by hand | Full path with a fake CLI, fake service manager and a local fake Release; terminal and shell-config cases have run on Linux in CI. No real GitHub Release yet; the real official CLI install and sign-in, and assigning work from the desktop app, have not been run |
| Backend choice `backend`, `project_backends`, `backends`, `install.sh --backend`, capability fallbacks | `claude` | Fake backend, every fallback path covered; `install.sh --backend` run with a fake CLI and a local fake Release |
| Usage-limit cooldown and quota per backend when mixed (`backend_limits`) | Key not written with a single backend | Fake CLI and stress tests. Two real backends side by side and a real usage limit have not been run |
| Codex backend | Choose it yourself | codex-cli 0.166.0, macOS arm64, ChatGPT login: a real run went from start, branch commit and `ready.json` through the test gate and review gate to the engine merge, and a SIGTERM resumed on the same session. Usage limits were only tested with samples built from the official error strings, an expired token has no real sample (only the 401 with no credentials), and Router, suggestions, sensing and release prep do not start on Codex. Linux has only run the fake CLI. Token refresh under a background service (launchd) is not verified |
| Scheduling, concurrency, resume, usage and quota, breaker | Paused, `agentq start` turns it on | Real runs on macOS arm64. Failure paths also covered by fake CLI and stress tests |
| Excluded projects `exclude_projects` | Empty | Fake CLI |
| Outside projects `extra_projects`, first `add` onboards | Empty, registered by `add` | Fake CLI, through to a tick taking the task |
| General engineering rules `agentq env` | Installed on first consent, skipped if you have global rules | Fake CLI, checks the files. That the real CLI reads them was confirmed by hand in the desktop app, there is no automated acceptance |
| Generated `.agentq/verify` | Written on the first `init` or `add` | The generated scripts run with fake tools, one fixture per project type. Real npm, cargo, uv, poetry and swift have not been run |
| Model following, `model_policy` | Empty on new configs, `fixed` | Fake CLI, checks argv. Which model the real CLI picks without these flags has no automated acceptance |
| Disk cleanup (`launchd.log`, task records older than `retention_days`) | On, 90 days | Fake data, including clock jumps and symlinks |
| `agentq new`, inbox | Available manually | Fake CLI and stress tests |
| Quality gates and engine merge | On, test and review gates at `error`; patch, UI and product gates at `warn` | Fake CLI and stress tests (real binary). Review against a real model has only run on Codex (see that row), not on Claude or opencode. The Playwright and `simctl` sample scripts for the UI gate were only syntax-checked, not run on a real project; thresholds have not been calibrated on real projects |
| Night shift, daily review | Off | Fake CLI and stress tests |
| Patrol | Off | Fake CLI and stress tests |
| Local notifications, task result notices | On when you consent in the one-line installer; `agentq start` leaves them off | Fake CLI and stress tests; no real desktop popup verified |
| Default branch detection | On | Fake CLI and stress tests (real git repositories), Linux CI |
| Telegram messaging | Off, optional | Basic send and receive used for real. Reply threads and full conversation round trips tested only against a fake service |
| Decision card buttons | With Telegram | Callbacks tested only against a fake service |
| Router | Off | One round with the real CLI on temporary data; no real Telegram round trip |
| Idle-time suggestions | Off | Fake CLI and stress tests |
| Feedback sensing | Off | App Store Connect API and `gh` search tested only against fake services |
| Release assistant | Off | App Store Connect API tested only against a fake service; nothing has really been submitted |

Stress tests (killing processes, corrupting state files, concurrency, full disk, clock jumps) have run on macOS, with every dependency faked. A manual Linux run is available in CI but has not been run yet. See [CONTRIBUTING](../../CONTRIBUTING.md#暴力测试).
