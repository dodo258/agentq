// Package app 组装全部模块（每个进程一份实例、钩子接线），放跨模块的命令：status、run、stop、logs、notify、backlog archive。
package app

import (
	"io"

	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/discuss"
	"agentq/internal/env"
	"agentq/internal/evolve"
	"agentq/internal/gate"
	"agentq/internal/gitx"
	"agentq/internal/ledger"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/onboard"
	"agentq/internal/orphan"
	"agentq/internal/patrol"
	"agentq/internal/quality"
	"agentq/internal/routine"
	"agentq/internal/runner"
	"agentq/internal/sched"
	"agentq/internal/service"
	"agentq/internal/stats"
	"agentq/internal/suggest"
	"agentq/internal/telegram"
)

type App struct {
	*env.Env
	Gitx     *gitx.Service
	Backlog  *backlog.Service
	Telegram *telegram.Service
	Quality  *quality.Service
	Ledger   *ledger.Service
	Service  *service.Service
	Notify   *notify.Service
	Gate     *gate.Service
	Runner   *runner.Service
	Orphan   *orphan.Service
	Discuss  *discuss.Service
	Newproj  *newproj.Service
	Suggest  *suggest.Service
	Routine  *routine.Service
	Patrol   *patrol.Service
	Chat     *chat.Service
	Stats    *stats.Service
	Sched    *sched.Service
	Onboard  *onboard.Service
	Evolve   *evolve.Service
}

// GateProcToken 是过门后台进程的内部命令名。
const GateProcToken = gate.ProcToken

// RunGate 是过门后台进程的入口，要配置和后端，所以在这里建。
func (a *App) RunGate(args []string, stderr io.Writer) int { return a.Gate.RunShell(args, stderr) }

// New 建这个进程的 App：读配置、接好所有模块。
func New(home string, out io.Writer, assets Assets) (*App, error) {
	e, err := env.New(home, out)
	if err != nil {
		return nil, err
	}
	a := Wire(&App{Env: e}, assets)
	a.Service.Launchctl = service.TestLaunchctl()
	return a, nil
}
