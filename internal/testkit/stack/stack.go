// Package stack 给功能模块的测试建真实的 runner 和它下层的模块实例，接法和 app.Wire 一样。只给测试 import。
// Hooks 不接：要走到上层的用例自己设，或者留根。
package stack

import (
	"agentq/internal/asc"
	_ "agentq/internal/backend/claude"
	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/gate"
	"agentq/internal/gitx"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/quality"
	"agentq/internal/runner"
	"agentq/internal/service"
	"agentq/internal/telegram"
)

// Stack 是一套接好的模块实例，字段名和 app.App 的一样。
type Stack struct {
	Gitx     *gitx.Service
	Backlog  *backlog.Service
	Telegram *telegram.Service
	ASC      *asc.Service
	Quality  *quality.Service
	Ledger   *ledger.Service
	Service  *service.Service
	Notify   *notify.Service
	Gate     *gate.Service
	Runner   *runner.Service
}

// New 建一套模块实例，都共用 e。
func New(e *env.Env) *Stack {
	g := &gitx.Service{Env: e}
	b := &backlog.Service{Env: e, Gitx: g}
	tg := &telegram.Service{Env: e}
	ac := &asc.Service{Env: e}
	q := &quality.Service{Env: e, Backlog: b, Gitx: g}
	l := &ledger.Service{Env: e, Quality: q}
	sv := &service.Service{Env: e}
	n := &notify.Service{Env: e, Backlog: b, Telegram: tg}
	v := &gate.Service{Env: e, Backlog: b, Gitx: g, Ledger: l, Notify: n}
	r := &runner.Service{Env: e, Backlog: b, Gitx: g, Ledger: l, Notify: n, Gate: v}
	return &Stack{Gitx: g, Backlog: b, Telegram: tg, ASC: ac, Quality: q, Ledger: l, Service: sv, Notify: n, Gate: v, Runner: r}
}
