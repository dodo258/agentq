package app

import (
	"io/fs"

	_ "agentq/internal/backend/claude" // 注册后端：接新后端加一行 import
	_ "agentq/internal/backend/codex"
	_ "agentq/internal/backend/opencode"
	"agentq/internal/chat"
	"agentq/internal/discuss"
	"agentq/internal/runner"
)

// Assets 是编译进二进制的东西：go:embed 不能写 ..，所以放根目录，由 main 传进来。
type Assets struct {
	RoutineText map[string]string // 夜班、复盘的说明文件，键是任务种类
	Install     fs.FS             // skills、templates、support、依赖锁定文件
	Evals       fs.FS             // 评测集，根下有 evals/
}

// Wire 建每个模块的唯一实例并接好依赖。
// 最后四行的顺序是固定的：方法值在接线时就绑定接收者，排在建实例之前绑到的是 nil，调用才崩，所以 Hooks 最后。
func Wire(a *App, assets Assets) *App {
	a.Gitx = orNew(a.Gitx)
	a.Gitx.Env = a.Env
	a.Backlog = orNew(a.Backlog)
	a.Backlog.Env = a.Env
	a.Backlog.Gitx = a.Gitx
	a.Telegram = orNew(a.Telegram)
	a.Telegram.Env = a.Env
	a.Quality = orNew(a.Quality)
	a.Quality.Env = a.Env
	a.Quality.Backlog = a.Backlog
	a.Quality.Gitx = a.Gitx
	a.Ledger = orNew(a.Ledger)
	a.Ledger.Env = a.Env
	a.Ledger.Quality = a.Quality
	a.Service = orNew(a.Service)
	a.Service.Env = a.Env
	a.Notify = orNew(a.Notify)
	a.Notify.Env = a.Env
	a.Notify.Backlog = a.Backlog
	a.Notify.Telegram = a.Telegram
	a.Gate = orNew(a.Gate)
	a.Gate.Env = a.Env
	a.Gate.Backlog = a.Backlog
	a.Gate.Gitx = a.Gitx
	a.Gate.Ledger = a.Ledger
	a.Gate.Notify = a.Notify
	a.Runner = orNew(a.Runner)
	a.Runner.Env = a.Env
	a.Runner.Backlog = a.Backlog
	a.Runner.Gitx = a.Gitx
	a.Runner.Ledger = a.Ledger
	a.Runner.Notify = a.Notify
	a.Runner.Gate = a.Gate
	a.Orphan = orNew(a.Orphan)
	a.Orphan.Env = a.Env
	a.Orphan.Backlog = a.Backlog
	a.Orphan.Ledger = a.Ledger
	a.Orphan.Notify = a.Notify
	a.Orphan.Runner = a.Runner
	a.Discuss = orNew(a.Discuss)
	a.Discuss.Env = a.Env
	a.Discuss.Backlog = a.Backlog
	a.Discuss.Gitx = a.Gitx
	a.Discuss.Ledger = a.Ledger
	a.Discuss.Notify = a.Notify
	a.Discuss.Runner = a.Runner
	a.Discuss.Telegram = a.Telegram
	a.Newproj = orNew(a.Newproj)
	a.Newproj.Env = a.Env
	a.Newproj.Backlog = a.Backlog
	a.Newproj.Discuss = a.Discuss
	a.Newproj.Ledger = a.Ledger
	a.Newproj.Notify = a.Notify
	a.Newproj.Runner = a.Runner
	a.Suggest = orNew(a.Suggest)
	a.Suggest.Env = a.Env
	a.Suggest.Backlog = a.Backlog
	a.Suggest.Gitx = a.Gitx
	a.Suggest.Notify = a.Notify
	a.Routine = orNew(a.Routine)
	a.Routine.Env = a.Env
	a.Routine.Backlog = a.Backlog
	a.Routine.Gitx = a.Gitx
	a.Routine.Ledger = a.Ledger
	a.Routine.Notify = a.Notify
	a.Routine.Runner = a.Runner
	a.Routine.Service = a.Service
	a.Patrol = orNew(a.Patrol)
	a.Patrol.Env = a.Env
	a.Patrol.Backlog = a.Backlog
	a.Patrol.Gitx = a.Gitx
	a.Patrol.Ledger = a.Ledger
	a.Patrol.Notify = a.Notify
	a.Patrol.Runner = a.Runner
	a.Patrol.Service = a.Service
	a.Patrol.Telegram = a.Telegram
	a.Chat = orNew(a.Chat)
	a.Chat.Env = a.Env
	a.Chat.Backlog = a.Backlog
	a.Chat.Discuss = a.Discuss
	a.Chat.Gate = a.Gate
	a.Chat.Ledger = a.Ledger
	a.Chat.Notify = a.Notify
	a.Chat.Orphan = a.Orphan
	a.Chat.Runner = a.Runner
	a.Chat.Service = a.Service
	a.Chat.Suggest = a.Suggest
	a.Chat.Telegram = a.Telegram
	a.Stats = orNew(a.Stats)
	a.Stats.Env = a.Env
	a.Stats.Ledger = a.Ledger
	a.Evolve = orNew(a.Evolve)
	a.Evolve.Env = a.Env
	a.Evolve.Backlog = a.Backlog
	a.Evolve.Gate = a.Gate
	a.Evolve.Ledger = a.Ledger
	a.Evolve.Notify = a.Notify
	a.Evolve.Suggest = a.Suggest
	a.Evolve.Evals = assets.Evals
	a.Sched = orNew(a.Sched)
	a.Sched.Env = a.Env
	a.Sched.Backlog = a.Backlog
	a.Sched.Chat = a.Chat
	a.Sched.Discuss = a.Discuss
	a.Sched.Evolve = a.Evolve
	a.Sched.Gate = a.Gate
	a.Sched.Ledger = a.Ledger
	a.Sched.Newproj = a.Newproj
	a.Sched.Notify = a.Notify
	a.Sched.Orphan = a.Orphan
	a.Sched.Patrol = a.Patrol
	a.Sched.Routine = a.Routine
	a.Sched.Runner = a.Runner
	a.Sched.Telegram = a.Telegram
	a.Onboard = orNew(a.Onboard)
	a.Onboard.Env = a.Env
	a.Onboard.Backlog = a.Backlog
	a.Onboard.Gitx = a.Gitx
	a.Onboard.Sched = a.Sched
	a.Onboard.Service = a.Service
	a.Onboard.Telegram = a.Telegram
	a.Onboard.Assets = assets.Install
	a.Notify.MorningGates = a.Gate.MorningLines
	a.Routine.RoutineText = assets.RoutineText
	a.Notify.MorningIssues = a.Patrol.MorningIssues
	a.Runner.Hooks = a.wiredHooks()
	return a
}

// orNew：字面量里已经带了（测试换掉钩子）就用它，没有就新建。
func orNew[T any](p *T) *T {
	if p == nil {
		return new(T)
	}
	return p
}

// wiredHooks 把任务生命周期的钩子接到各功能。
func (a *App) wiredHooks() runner.Hooks {
	return runner.Hooks{
		ChatFilesDir:    a.Chat.ChatFilesDir,
		FollowupRequest: chat.FollowupRequest,
		DiscussRequest:  discuss.Request,
		ConstraintsNote: a.Evolve.ConstraintsNote,

		MissingReport:  a.Routine.MissingReport,
		CheckNewResult: a.Newproj.CheckNewResult,

		NewDirs:       a.Newproj.NewDirs,
		NewDirsNote:   a.Newproj.NewDirsNote,
		ReleaseHeld:   a.Newproj.ReleaseHeld,
		RequeueThread: a.Chat.RequeueThread,
		ReplyChat:     a.Chat.ReplyChat,
		NoticeNew:     a.Newproj.NoticeNew,
		FinishDiscuss: a.Discuss.FinishJob,
	}
}
