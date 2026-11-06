package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/gitx"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Reap 收尾进程已经不在的 running 任务，返回收尾个数。启动后机器重启过的任务不清进程组，记中断（见 finishRebooted）。
func (a *Service) Reap(st *state.State) int {
	a.AdoptStarting(st)
	n := 0
	for _, j := range st.Running() {
		if j.Status != state.StatusRunning {
			continue // 收尾 Router 时它的 stop 动作已经停了这个任务
		}
		if !a.adoptPID(j) && !a.bootChanged(j) { // 重启过的话外壳也没了，走下面按重启收尾
			if a.startingUp(j) {
				continue // 启动方被杀了，外壳可能还在起 claude：等它写 claude.pid
			}
			// 外壳写 claude.pid 之前也可能死了：确认 claude 不在了才能算失败（见 joblost.go）
			pid, err := a.FindLostClaude(j)
			switch {
			case err != nil:
				a.Logf("任务 %s 没报回 pid，查不了 %s 还在不在：%v；当它在，下次 tick 再查", j.ID, a.BackendFor(j.Backend).Name(), err)
				continue
			case pid == 0:
				a.Finish(st, j, state.StatusFailed, "启动中断：外壳没报回 "+a.BackendFor(j.Backend).Name()+" 的 pid，已确认这个任务没有进程在跑")
				n++
				continue
			}
			a.Logf("任务 %s 没报回 pid，%s 还在跑（pid %d），接回来继续看着", j.ID, a.BackendFor(j.Backend).Name(), pid)
			j.PID = pid
			a.Ledger.WriteMeta(j)
		}
		live := a.JobLive(j)
		if live == proc.ProcUnsure { // 认不出：当它还在跑，占着名额不重复起，到了超时由 EnforceTimeouts 放开，不杀
			if !a.bootChanged(j) {
				a.Logf("任务 %s：pid %d 还在，但认不出是不是它（环境读不到，外壳也不在了），当还在跑；超时后不杀只收尾", j.ID, j.PID)
				continue
			}
			live = proc.ProcGone // 重启过，这个 pid 不可能是它
		}
		alive := live == proc.ProcOurs
		if a.bootChanged(j) {
			if alive {
				// 进程还在就不可能重启过，是校时让 kern.boottime 跟着变了：记下新的开机时间，照常看着
				a.Logf("任务 %s 的开机时间记录（%d）和现在（%d）不一致，但进程还在，按校时处理", j.ID, j.BootTime, a.CurBoot())
				j.BootTime = a.CurBoot()
				a.Ledger.WriteMeta(j) // 状态文件坏了要按 meta.json 重建，别让重建后又被当成重启过
				continue
			}
			a.FinishRebooted(st, j)
			n++
			continue
		}
		if alive {
			continue
		}
		a.FinishFromResult(st, j)
		a.CleanupGroup(j)
		n++
	}
	return n
}

// bootChanged：任务记了启动时的开机时间，且和现在的不一致（机器在任务启动后重启过）。
func (a *Service) bootChanged(j *state.Job) bool {
	cur := a.CurBoot()
	if j.BootTime == 0 || cur == 0 {
		return false
	}
	d := cur - j.BootTime
	return d > proc.BootSlack || d < -proc.BootSlack
}

// FinishRebooted 收尾启动后机器重启过的任务：记下的 pid 和进程组号可能已经属于别的程序，不清进程组。
// claude 在重启前已经正常退出、写了结果的，照常按结果收尾；否则记中断（不计连环失败），
// BACKLOG 里这一条仍标进行中、有会话 id、本身不是续跑的，用同一会话续跑一次。
func (a *Service) FinishRebooted(st *state.State, j *state.Job) {
	if res := a.Ledger.ReadResult(j); res.HaveTerminal && (res.Text != "" || res.SessionID != "") {
		a.FinishFromResult(st, j)
		return
	}
	j.Rebooted = true
	why := "机器重启过（开机时间和启动任务时不一致），" + a.BackendFor(j.Backend).Name() + " 已不在；没清它的进程组（组号可能已被别的程序占用）"
	if j.Kind != "" { // 夜班、复盘、new 没有 BACKLOG 条目，不续跑
		a.Finish(st, j, state.StatusInterrupted, why)
		return
	}
	items, err := a.jobItems(j)
	if err != nil {
		why += "；核验 BACKLOG 失败：" + err.Error()
	}
	if it, ok := InProgressItem(items, j.Fingerprint); ok {
		why += "；BACKLOG 里这一条仍标进行中" + BranchNote(j.ProjectDir, it.Text)
		switch {
		case j.ResumeOf != "":
			why += "；这次已经是续跑，不再续跑，等你处理"
		case j.SessionID == "":
			why += a.noSessionNote(j)
		default:
			j.ResumePending = true
			why += "；将自动用同一会话续跑一次"
		}
	} else if err == nil {
		why += "；条目已不在进行中，不续跑，等你核对"
	}
	a.Finish(st, j, state.StatusInterrupted, why)
}

// CleanupGroup：claude 已退出但它拉起的子进程还在同一进程组里时，发 SIGTERM 清掉。
// 平时任务外壳在 claude 退出时已经清过；走到这里多半是外壳没了，离 claude 退出可能已经很久，
// 组号可能已被别的程序占用，所以组里要有带这个任务标记的进程才杀。
func (a *Service) CleanupGroup(j *state.Job) {
	if j.PID <= 0 || !proc.GroupAlive(j.PID) {
		return
	}
	ok, err := proc.GroupMarked(j.PID, j.ID)
	switch {
	case err != nil:
		a.Logf("任务 %s 的进程组 %d 还有进程，但核对不了是不是它留下的，不清理：%v", j.ID, j.PID, err)
	case !ok:
		a.Logf("任务 %s 的进程组 %d 还有进程，但都没有这个任务的标记 %s=%s（可能组号已被别的程序占用，或只剩系统自带命令），不清理", j.ID, j.PID, proc.JobEnv, j.ID)
	default:
		how := "SIGTERM"
		if proc.KillGroup(j.PID, proc.LeftoverGrace) { // 这是最后一次有人管它们：不理 SIGTERM 的要清掉
			how = "SIGTERM 后 SIGKILL"
		}
		a.Logf("任务 %s 的进程组 %d 还有残留进程，已发 %s", j.ID, j.PID, how)
	}
}

// FinishFromResult 读 result.json 定结果：is_error 为假 → done，但核验出没做完 → interrupted；为真或读不出 → failed。
// 可能撞了上限（见 mayBeLimit）且结果文本里有用量上限字样 → 设冷却（能解析出重置时间就冷却到那时），这次不算「试过」；
// 撞上限时 BACKLOG 里这一条仍标进行中（做到一半撞上的）→ 记中断，冷却过后用同一会话续跑。
func (a *Service) FinishFromResult(st *state.State, j *state.Job) {
	res := a.BackendFor(j.Backend).Parse(a.Ledger.JobRef(j), backend.Exit{At: a.ExitedAt(j)})
	status, summary, limit := state.StatusFailed, "", false
	parsed := res.HaveTerminal
	be := a.BackendFor(j.Backend)
	if res.Stop == backend.StopAuth { // 不计连环失败，这次不算试过：重新登录之前起多少个都一样
		j.LimitHit = true
		until := a.PauseAuth(be.Name())
		msg := "后端 " + be.Name() + " 认证失败，暂停到 " + textfmt.WhenCN(until) + "；" + textfmt.OneLine(res.Text, 200)
		a.Logf("任务 %s：%s", j.ID, msg)
		a.Notify.Notify(notify.AuthPaused(be.Name(), j.ID, a.Tilde(a.AuthPausePath(be.Name())), until))
		a.Finish(st, j, state.StatusFailed, msg)
		return
	}
	if parsed {
		if res.SessionID != "" && be.Caps().Resume {
			j.SessionID = res.SessionID
		}
		summary = summaryParagraph(res.Text)
		if j.Kind == state.KindSense { // 结果文字可能带外部原文，不进摘要（摘要进 meta 和汇总）
			summary = ""
		}
		if j.Kind == state.KindPatrol || state.IsChatKind(j.Kind) || j.Kind == state.KindInbox { // 分析结论、对话回答、任务箱结论整段都要，不按合并情况挑段落
			summary = textfmt.OneLine(res.Text, 600)
		}
		limit = a.jobHitLimit(j, res)
		// 回答类任务撞自己的回合、预算上限是这次没答完，不是系统出问题，不算连环失败
		if (state.IsChatKind(j.Kind) || j.Kind == state.KindSuggest || j.Kind == state.KindSense || j.Kind == state.KindRelease || j.Kind == state.KindDiscuss) && res.Stop.SelfLimit() {
			j.LimitHit = true
		}
		if j.Kind == state.KindRelease && res.Stop == backend.StopMaxBudget { // 不走通用的撞预算收尾：只留 finishReleasePrep 那一条提醒
			summary = "撞到预算上限（" + backend.BudgetLabel(a.Cfg.ReleaseBudgetUSD) + "，失控兜底），项目里的改动都留着；" + summary
		}
		if !res.Failed {
			status = state.StatusDone
		}
	} else {
		tail := fsx.TailLines(a.Ledger.JobOutput(j.ID, "stderr.log"), 5)
		summary = "没有可读的结果（result.json 为空或格式不对）"
		if t := strings.TrimSpace(tail); t != "" {
			summary += "；stderr 末尾：" + textfmt.OneLine(t, 200)
		}
		limit = !limitFromStructuredOnly(j.Kind) && res.LimitText
		if j.Kind == state.KindSense { // stderr 里可能带外部原文，不进摘要（摘要进 meta 和汇总）
			summary = "没有可读的结果（详情在 " + a.Tilde(a.Ledger.SenseRunDir(j.ID)) + "）"
		}
		if !limit && res.SessionID != "" && be.Caps().Resume && j.Kind == "" { // 事件流开头就有会话 id 的后端：中途死了也接得上。只在结束时写结果的后端走不到这里
			j.SessionID = res.SessionID
			status, summary = a.interruptedNoResult(j, status, summary)
		}
	}
	if limit {
		j.LimitHit = true
		until, how := a.Now().Add(a.Cfg.LimitCooldown.Duration), ""
		// 后端以 CLI 退出（打出提示）的时刻为参照解析，收尾晚了、跨过午夜也不会推到下一天/下一周。文字不可信的任务连重置时间也不从文字里读
		if t := res.ResetAt; !limitFromStructuredOnly(j.Kind) && t.After(a.Now()) {
			until, how = t, "（按提示的重置时间）"
		}
		st.ExtendCooldown(j.Backend, a.Cfg.Backend, until)
		cooling := st.CooldownOf(j.Backend, a.Cfg.Backend)
		summary = "撞到用量上限，暂停启动到 " + textfmt.WhenCN(cooling) + how + "；" + summary
		a.Logf("任务 %s 撞到用量上限，冷却到 %s%s", j.ID, cooling.Format("2006-01-02 15:04"), how)
		var items []backlog.Item
		var err error
		if j.Kind == "" { // 夜班、复盘没有 BACKLOG 条目，不续跑
			items, err = a.jobItems(j)
		}
		if err != nil {
			summary += "；核验 BACKLOG 失败：" + err.Error()
		}
		if it, ok := InProgressItem(items, j.Fingerprint); ok {
			status = state.StatusInterrupted
			summary += "；BACKLOG 里这一条仍标进行中" + BranchNote(j.ProjectDir, it.Text)
			switch {
			case j.SessionID == "":
				summary += a.noSessionNote(j)
			case ResumeDepth(st, j) >= maxLimitResumes:
				summary += fmt.Sprintf("；已经连续续跑 %d 次，不再续跑，等你处理", maxLimitResumes)
			default:
				j.ResumePending = true
				summary += "；冷却过后用同一会话续跑"
			}
		}
	}
	if parsed && res.Stop == backend.StopMaxBudget && j.Kind != state.KindInbox && j.Kind != state.KindSuggest && j.Kind != state.KindSense && j.Kind != state.KindRelease && j.Kind != state.KindDiscuss && !state.IsChatKind(j.Kind) { // 任务箱、闲时找活、主动感知、发版准备自己定（inboxResult、suggestResult、senseResult、finishReleasePrep），回答类任务回「没答完」
		status, summary = a.budgetResult(st, j, summary)
	}
	if summary == "" && status == state.StatusFailed {
		summary = a.BackendFor(j.Backend).Name() + " 报错结束"
	}
	if parsed && status == state.StatusFailed && !j.LimitHit {
		if note := a.noteTransient(st, j, res); note != "" {
			summary = note + "；" + summary
		}
	}
	if _, routine := config.Routines[j.Kind]; status == state.StatusDone && !j.LimitHit && routine {
		if p := a.Hooks.MissingReport(j); p != "" {
			status = state.StatusInterrupted
			summary = "正常退出但没写出报告 " + p + "，可能中途退出了（不续跑，等你看 agentq logs " + j.ID + "）；结果最后：" + summary
		}
	}
	if status == state.StatusDone && j.Kind == state.KindNew {
		status, summary = a.Hooks.CheckNewResult(j, res, summary)
	}
	gated := false
	if status == state.StatusDone && !j.LimitHit && j.Kind == "" {
		gated = a.enqueueReady(st, j)
	}
	if gated {
		if j.Gate.State == state.GateQueued {
			summary = "分支 " + j.Gate.Branch + " 可合并，排队过门；" + summary
		} else {
			summary = j.Summary
		}
	}
	if status == state.StatusDone && !j.LimitHit && j.Kind == "" && !gated {
		if bad, resumable, why := a.checkInterrupted(j, res); bad {
			status = state.StatusInterrupted
			switch {
			case j.ResumeOf != "":
				why = "续跑后仍中断，等你处理：" + why
			case j.Takeover:
				why = "接手后仍未完成：" + why
			case !resumable:
				why += "；条目已不在进行中，不续跑，等你核对"
			case j.SessionID == "" || j.SessionInRoot():
				why += a.noSessionNote(j)
			default:
				j.ResumePending = true
				why += "；将自动用同一会话续跑一次"
			}
			if summary != "" {
				why += "；结果最后：" + summary
			}
			summary = why
		}
	}
	if status == state.StatusDone && j.Kind == "" && !gated {
		summary = a.withDoneCommit(j, summary)
		if !j.LimitHit { // 撞上限的没开工，不算「报告做完」
			summary = a.noteStillListed(st, j, summary)
			summary = a.noteBypass(j, summary)
		}
	}
	if j.Kind == state.KindInbox {
		status, summary = a.Hooks.InboxResult(j, res, status, summary)
	}
	if j.Kind == state.KindSuggest && parsed {
		status, summary = a.Hooks.SuggestResult(j, res, status, summary)
	}
	if j.Kind == state.KindSense && parsed {
		status, summary = a.Hooks.SenseResult(j, res, status, summary)
	}
	a.Finish(st, j, status, summary)
}

// AdoptFirstItem：new 任务把第一条做到了分支可合并。从这里起这个任务就是新项目里那一条的普通任务：过门、退回、卡住、接手都走同一条路。
// 会话是在 projects_root 建的，换了工作目录接不回去（Job.SessionInRoot），退回时由接手任务从分支现状接着做。分支、条目、ready.json 对不上返回错误，任务不变。
func (a *Service) AdoptFirstItem(j *state.Job, name, dir, branch string) error {
	p := backlog.Project{Name: name, Dir: dir}
	items, err := p.Items()
	if err != nil {
		return fmt.Errorf("读不了 %s 的 BACKLOG：%w", name, err)
	}
	var it backlog.Item
	found := false
	for _, x := range items {
		if x.InProgress && !x.Stuck && backlog.MarkerBranch(x.Text) == branch {
			it, found = x, true
			break
		}
	}
	if !found {
		return fmt.Errorf("%s 的「待做」里没有标着分支 %s 的进行中条目", name, branch)
	}
	if _, _, ok, err := a.Gate.ReadReady(dir, branch); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s 的分支 %s 上没有 %s", name, branch, gate.ReadyFile)
	}
	def, err := a.Gitx.DefaultBranch(dir) // 过门、退回、接手都按它，主目录切走了等切回来
	if err != nil {
		return err
	}
	j.Kind, j.Project, j.ProjectDir, j.Item, j.Fingerprint, j.DefaultBranch = "", name, dir, it.Text, it.Fingerprint, def
	j.Title = it.Title
	if j.Title == "" {
		j.Title = it.Text
	}
	j.GateContract = true
	return nil
}

// interruptedNoResult：进程死了没有终态，条目还标着进行中就是做到一半中断了，用同一会话续跑一次。
func (a *Service) interruptedNoResult(j *state.Job, status, summary string) (string, string) {
	items, err := backlog.Project{Dir: j.ProjectDir}.Items()
	if err != nil {
		return status, summary + "；核验 BACKLOG 失败：" + err.Error()
	}
	it, ok := InProgressItem(items, j.Fingerprint)
	if !ok {
		return status, summary
	}
	summary += "；BACKLOG 里这一条仍标进行中" + BranchNote(j.ProjectDir, it.Text)
	switch {
	case j.ResumeOf != "":
		summary += "；这次已经是续跑，不再续跑，等你处理"
	case j.Takeover:
		summary += "；接手后仍未完成"
	default:
		j.ResumePending = true
		summary += "；将自动用同一会话续跑一次"
	}
	return state.StatusInterrupted, summary
}

// enqueueReady：条目标着进行中、任务分支上有合格的 ready.json，就排进门队列。ready.json 不合格的只记下，这一条按没做完走（续跑时任务再写）。
func (a *Service) enqueueReady(st *state.State, j *state.Job) bool {
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	items, err := a.jobItems(j)
	if err != nil {
		a.Logf("任务 %s：读不了 BACKLOG，不知道分支，当没写 ready.json：%v", j.ID, err)
		return false
	}
	it, ok := stuckTarget(j, p, items)
	if !ok || !it.InProgress || it.Stuck {
		return false
	}
	r, head, found, err := a.Gate.ReadReady(j.ProjectDir, backlog.MarkerBranch(it.Text))
	if err != nil {
		a.Logf("任务 %s：%v", j.ID, err)
		return false
	}
	if !found {
		return false
	}
	j.Fingerprint = it.Fingerprint   // 任务改过正文时指纹变了：退回时要认得这一条
	if a.Gate.OnJobBranch(j, head) { // 它自己合并了：过不了门，也不能当引擎合的
		a.Gate.Bypassed(st, j, r, head)
		return true
	}
	a.Gate.Enqueue(j, r, head)
	return true
}

// noteBypass：新合同的任务没写 ready.json，「已完成」里却写了合并提交：它自己合并了，没过门。合并不撤销，告诉用户。
func (a *Service) noteBypass(j *state.Job, summary string) string {
	if !a.selfMerged(j) {
		return summary
	}
	msg := fmt.Sprintf("门：任务 %s（%s「%s」）没写 ready.json，自己合并了，没过门", j.ID, j.Project, textfmt.TruncRunes(j.Title, 40))
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	a.Notify.Notify(notify.Notice{Cat: "alert", Key: "gate-bypass:" + j.ID, JobID: j.ID,
		Text:  j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」没过门就合并了",
		Lines: []string{"合并不撤销，只提醒；详情 agentq logs " + j.ID}})
	return summary
}

// selfMerged：新合同的任务没过门，「已完成」里却写了合并提交。升级前启动的任务按老合同自己合并，不算绕过。
func (a *Service) selfMerged(j *state.Job) bool {
	return j.GateContract && a.doneCommit(j) != ""
}

// noteStillListed：任务报告做完了，这一条却还在「待做」、也没标进行中或卡住。条目还能被挑到，retry_after 一到又起一个，
// 如果每次都这样（任务忘了移到「已完成」、或者根本没做），会一直重做一直花钱。第一次只记下，下一次还这样就标卡住，通知用户。
func (a *Service) noteStillListed(st *state.State, j *state.Job, summary string) string {
	if a.Backlog.Excluded(j.Project) { // tick 不会再挑它，也不写它的 BACKLOG
		return summary
	}
	items, err := a.jobItems(j)
	if err != nil {
		return summary
	}
	listed := false
	for _, it := range items {
		listed = listed || (it.Fingerprint == j.Fingerprint && it.Eligible())
	}
	if !listed {
		return summary
	}
	var prev *state.Job
	for _, o := range st.Jobs {
		if o != j && o.Kind == "" && o.EndedAt != nil && state.SameItem(o, j.Project, j.Fingerprint) && !o.LimitHit && (prev == nil || o.EndedAt.After(*prev.EndedAt)) {
			prev = o
		}
	}
	j.StillListed = true
	if prev == nil || !prev.StillListed {
		a.Logf("任务 %s 报告做完了，但条目还在「待做」，%s 后会再做一次，再这样就标卡住", j.ID, textfmt.FormatDuration(a.Cfg.RetryAfter.Duration))
		return summary
	}
	j.StuckPending = "连续两次报告做完，条目还在待做，不再自动重做，任务 " + j.ID
	a.Notify.Notify(notify.Notice{Cat: "alert", Key: "still-listed:" + j.Fingerprint, JobID: j.ID,
		Text:  j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」连续两次报告做完，条目还在「待做」，标了卡住",
		Lines: []string{"看 agentq logs " + j.ID + "；确实做完了就移到「已完成」，没做完就去掉卡住标记"}})
	return "连续两次报告做完，条目还在「待做」，标卡住，等你看；" + summary
}

// withDoneCommit：BACKLOG「已完成」里同标题的条目写了「合并提交 <号>」时，把它放在摘要开头
// （status 只显示摘要前 80 个字，前 80 个字里已经有这个号就不重复）。
// 「待做」里还有同标题的条目（比如标了卡住没合并）就不取，免得带出以前同名条目的旧号。
func (a *Service) withDoneCommit(j *state.Job, summary string) string {
	c := a.doneCommit(j)
	if c == "" || strings.Contains(textfmt.OneLine(summary, 80), c) {
		return summary
	}
	return "合并提交 " + c + "；" + summary
}

// doneCommit：「已完成」里同标题条目写的合并提交。「待做」里还有同标题的条目（比如标了卡住没合并）就不取，免得带出以前同名条目的旧号。
func (a *Service) doneCommit(j *state.Job) string {
	text, err := a.jobBacklog(j)
	if err != nil {
		return ""
	}
	for _, it := range backlog.ParseBacklog(text) {
		if it.Title == j.Title {
			return ""
		}
	}
	return backlog.DoneMergeCommit(text, j.Title)
}

// noSessionNote：不能续跑时写进摘要的话。后端本来就不能续跑的，这一条由接手任务从分支现状接着做（见 internal/orphan/orphan.go）。
func (a *Service) noSessionNote(j *state.Job) string {
	if j.SessionInRoot() {
		return "；新项目第一条的会话接不回去，由接手任务从分支现状接着做"
	}
	if be := a.BackendFor(j.Backend); !be.Caps().Resume {
		return "；后端 " + be.Name() + " 不能续跑，由接手任务从分支现状接着做"
	}
	return "；没有会话 id，不能续跑，等你处理"
}

// maxLimitResumes：同一条做到一半撞上限后最多接连续跑几次（每次之间都有冷却），防止一直报上限时无限续跑。
const maxLimitResumes = 3

// ResumeDepth：j 是第几次续跑（顺着 ResumeOf 往回数，原任务为 0）。
func ResumeDepth(st *state.State, j *state.Job) int {
	n := 0
	for j.ResumeOf != "" && n <= maxLimitResumes {
		prev, err := st.Find(j.ResumeOf)
		if err != nil {
			break
		}
		j = prev
		n++
	}
	return n
}

// mergeRe：结果里说到合并情况的字样（prompt 要求最后一段写「合并了没有」）。
var mergeRe = regexp.MustCompile(`(?i)合并|合入|没合|未合|merge`)

// checkInterrupted 核验 is_error 为假、没写 ready.json 的结束是不是真的做完了，返回是否中断、能否续跑、写进摘要的原因。
// BACKLOG「待做」里同一条仍标进行中 → 中断，可以续跑（agent 认领后没走到写 ready.json 那一步）。
// 条目不在进行中但结果里没写合并情况 → 也按中断报，不续跑（不知道做到哪了，交给用户核对）。
func (a *Service) checkInterrupted(j *state.Job, res backend.Result) (bool, bool, string) {
	var why []string
	inProgress := false
	items, err := a.jobItems(j)
	if err != nil {
		why = append(why, "核验 BACKLOG 失败："+err.Error())
	}
	if it, ok := InProgressItem(items, j.Fingerprint); ok {
		inProgress = true
		why = append(why, "BACKLOG 里这一条仍标进行中"+BranchNote(j.ProjectDir, it.Text))
	}
	if !inProgress {
		if mergeRe.MatchString(res.Text) {
			return false, false, ""
		}
		why = append(why, "结果里没写合并情况")
	}
	if inProgress {
		why = append(why, "分支上没有合格的 "+gate.ReadyFile)
	}
	if res.Background {
		why = append(why, "最后一个回合由后台通知触发")
	}
	return true, inProgress, strings.Join(why, "；")
}

// BranchNote 从进行中标记里取分支名，用 git 看它有没有合进默认分支，返回「（分支 …）」说明；取不到就返回空。
func BranchNote(dir, text string) string {
	m := backlog.BranchRe.FindStringSubmatch(text)
	if m == nil || strings.HasPrefix(m[1], "-") {
		return ""
	}
	b := m[1]
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // tick 拿着锁
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		proc.BoundCmd(cmd)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+b); err != nil {
		return "（分支 " + b + " 不存在）"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	def, err := gitx.DefaultBranch(ctx, os.Getenv("HOME"), dir)
	if err != nil {
		return "（分支 " + b + "）"
	}
	n, err := git("rev-list", "--count", "refs/heads/"+def+"..refs/heads/"+b)
	switch {
	case err != nil:
		return "（分支 " + b + "）"
	case n == "0":
		return "（分支 " + b + " 没有 " + def + " 之外的提交）"
	}
	return "（分支 " + b + " 比 " + def + " 多 " + n + " 个提交，没合并）"
}

// Finish 标记任务结束，更新 meta.json，追加当天汇总。
func (a *Service) Finish(st *state.State, j *state.Job, status, summary string) {
	now := a.Now()
	if be := a.BackendFor(j.Backend); j.SessionID == "" && !be.Caps().PresetSession && be.Caps().Resume { // 被杀的任务没走 FinishFromResult：输出里有会话 id 就还能接上
		j.SessionID = a.Ledger.ReadResult(j).SessionID
	}
	if j.Kind == state.KindNew && status != state.StatusDone {
		summary += a.Hooks.NewDirsNote(st, j, status)
	}
	j.Status = status
	j.EndedAt = &now
	j.Summary = summary
	if j.Handoff {
		a.noteHandoffClaim(j)
	}
	a.Ledger.ReadJobUsage(st, j)
	a.Ledger.WriteMeta(j)
	a.Ledger.AppendNightly(j)
	a.Ledger.UpdateCostFooter(st, now)
	a.Logf("任务 %s 结束：%s %s", j.ID, state.StatusLabel(status), textfmt.OneLine(summary, 120))
	a.noteOutcome(st, j)
	stuck := j.Kind == "" && j.StuckPending != ""
	if stuck {
		a.writeStuckMark(j)
	} else {
		a.noticeEnded(j)
	}
	if j.BudgetHit && j.Kind != state.KindNew && j.Kind != state.KindInbox {
		a.noticeBudget(j, stuck)
	}
	switch {
	case j.Kind == state.KindRouter:
		a.Hooks.FinishRouter(st, j)
	case j.Kind == state.KindInbox:
		a.Hooks.FinishInbox(st, j)
	case state.IsChatKind(j.Kind):
		if !a.Hooks.RequeueThread(j) {
			a.Hooks.ReplyChat(j)
		}
	case j.Kind == "" && j.FromThread:
		a.Hooks.RequeueThread(j)
	case j.Kind == state.KindSuggest:
		a.Hooks.NoticeSuggest(j)
	case j.Kind == state.KindRelease:
		a.Hooks.FinishReleasePrep(j)
	case j.Kind == state.KindNew:
		a.Hooks.NoticeNew(j)
	case j.Kind == state.KindDiscuss:
		a.Hooks.FinishDiscuss(st, j)
	}
	if j.NewReport != nil { // 同时停掉过另一个 new 时，这个做成的项目可能被它算成多出的目录
		a.Hooks.ReleaseHeld(st, filepath.Base(j.NewReport.Path))
	}
}

// noticeEnded 告诉用户一条待办的结果。只说普通任务：别的 Kind 有自己的通知，或者不算「任务做完」。
// 不说的：过门的（合并、卡片都由门说）；会接着做的（续跑、接手、重试，到了结果再说）；用户自己停的；撞上限、撞预算、自动停、绕过门、标了卡住的（都有专门的通知）。
func (a *Service) noticeEnded(j *state.Job) {
	if j.Kind != "" || j.Gate != nil || j.ResumePending || j.LimitHit || j.BudgetHit || j.Transient || j.AutoStop != "" || j.Status == state.StatusKilled {
		return
	}
	if j.Status == state.StatusInterrupted && (j.Takeover || !a.BackendFor(j.Backend).Caps().Resume) {
		return // 这一条还标着进行中：接手任务接着做，接手后还不行由孤儿扫描标卡住并通知
	}
	name := j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」"
	n := notify.Notice{Cat: "alert", Key: notify.TaskEndKey(j.Project, j.Fingerprint), JobID: j.ID, Text: name + state.StatusLabel(j.Status),
		Lines: []string{textfmt.OneLine(j.Summary, 200), "详情 agentq logs " + j.ID}}
	if j.Status == state.StatusDone {
		p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
		items, err := a.jobItems(j)
		if err != nil {
			a.Logf("任务 %s：读不了 BACKLOG，不知道这一条的结果，没推：%v", j.ID, err)
			return
		}
		it, listed := stuckTarget(j, p, items)
		switch c := a.doneCommit(j); {
		case listed && it.Stuck:
			n.Text = name + "卡住了"
			n.Lines[0] = textfmt.OneLine(it.StuckMark(), 200)
		case a.selfMerged(j): // noteBypass 说过了
			return
		case c != "": // 升级前启动的任务按老合同自己合并
			n.Cat, n.Text = "done", name+"做完了，已合并"
			n.Lines = []string{"合并提交 " + c}
		default:
			n.Text = name + "跑完了，没有要合并的分支"
		}
	}
	a.Notify.Notify(n)
}

// summaryParagraph 依次取结果文本里最后一个写着合并提交号的段落、最后一个说到合并情况的段落
// （prompt 要求最后一段写，但 agent 不一定放在最后），都没有就取最后一段。压成一行。
func summaryParagraph(s string) string {
	paras := strings.Split(strings.TrimSpace(s), "\n\n")
	for _, re := range []*regexp.Regexp{state.CommitRe, mergeRe} {
		for i := len(paras) - 1; i >= 0; i-- {
			if p := strings.TrimSpace(paras[i]); re.MatchString(p) {
				return textfmt.OneLine(p, 300)
			}
		}
	}
	for i := len(paras) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(paras[i]); p != "" {
			return textfmt.OneLine(p, 300)
		}
	}
	return ""
}
