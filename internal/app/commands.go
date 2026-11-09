package app

import (
	"fmt"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// CmdRun 立刻为项目启动一个任务：不看 paused、retry_after 和连环失败熔断（用户明确要做），仍遵守上限、用量冷却和登录检查；
// 订阅额度 ≥usageRefuseAt 时拒绝，≥usageStopAt 只提示。
func (a *App) CmdRun(project, keyword string) error {
	if setup.NeedsConsent(a.Cfg) {
		fmt.Fprintln(a.Out, setup.PermissionDefaultHint)
	}
	if a.Backlog.Excluded(project) {
		fmt.Fprintf(a.Out, "%s 在 exclude_projects 里，自动流程不碰它；这次是你明确指定的，照跑。中断了不会自动续跑。\n", project)
	}
	lock, err := a.Runner.LockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出，当作未知，不启动：%w", err)
	}
	a.Runner.Reap(st)
	a.Runner.EnforceTimeouts(st)
	if err := state.Save(a.StatePath(), st); err != nil {
		return err
	}
	r, err := a.Runner.RunProject(st, project, keyword)
	if err != nil {
		return err
	}
	a.Runner.ReportStarted(st, r, "run")
	return nil
}

// jobProcNote：status 里说明运行中任务的进程情况。pid 还不知道不等于进程不在了（见 internal/runner/joblost.go）。
func (a *App) jobProcNote(j *state.Job) string {
	if j.PID <= 0 {
		return "（还不知道 pid，下次 tick 再查）"
	}
	switch a.Runner.JobLive(j) {
	case proc.ProcGone:
		return "（进程已结束，等下次 tick 收尾）"
	case proc.ProcUnsure:
		return "（pid 还在但认不出是不是它，当还在跑，超时后只收尾不杀）"
	}
	return ""
}

// CmdProjects 列出所有项目，技能和提示词按名字找项目都走这里，不用各自解析配置。扫不了 projects_root 时列出看得到的，再报错。
func (a *App) CmdProjects() error {
	projects, err := a.Backlog.AllProjects()
	for _, p := range projects {
		if a.Backlog.Excluded(p.Name) {
			fmt.Fprintf(a.Out, "%s\t%s\texclude_projects\n", p.Name, p.Dir)
		} else {
			fmt.Fprintf(a.Out, "%s\t%s\n", p.Name, p.Dir)
		}
	}
	if err != nil {
		return fmt.Errorf("projects_root %s 扫不了，上面可能不全：%w", a.ProjectsRoot(), err)
	}
	return nil
}

func (a *App) CmdStatus() error {
	now := a.Now()
	w := a.Out
	fmt.Fprintf(w, "agentq 状态（%s）\n", now.Format("2006-01-02 15:04"))
	st, stErr := state.Load(a.StatePath())
	// 额度：缓存还新就用 state 里的；过期了现读一次（只显示，不写回 state，status 不拿锁）
	var u *backend.UsageInfo
	if st != nil {
		u = st.Usage
	}
	if u.Stale(now) {
		u = a.Runner.ReadUsage(now)
	}
	ulvl, uuntil, uwhy := backend.UsageLevel(u, now, a.Cfg.UsagePolicy())
	fmt.Fprintf(w, "调度：%s\n", a.Runner.SchedNote(st, stErr, now, ulvl, uuntil, uwhy))
	if pid, since, held := fsx.LockHolder(a.LockPath()); held {
		fmt.Fprintf(w, "tick.lock：pid %d 拿着 tick.lock，已 %s（这期间别的 tick 只跳过，不收尾也不接活；一直这样就是卡住了，run、stop、升级也拿不到锁）\n", pid, time.Since(since).Round(time.Second))
	}
	fmt.Fprintf(w, "额度：%s（%s 读取）\n", backend.UsageDescribe(u, now, a.Cfg.UsagePolicy()), textfmt.WhenCN(u.CheckedAt))
	if st != nil { // 别的后端只看缓存：status 不替它们读
		for _, line := range st.OtherUsageLines(a.Cfg, now) {
			fmt.Fprintf(w, "额度（%s\n", strings.Replace(line, "：", "）：", 1))
		}
	}
	// 开着的功能各占一行；没开的收成最后一行，新用户不用看配置键名。
	var off []string
	for _, kind := range []string{config.KindNight, config.KindReview} {
		if r := config.Routines[kind]; r.Enabled(a.Cfg) {
			fmt.Fprintf(w, "定点任务：%s\n", a.Service.RoutineStatus(kind))
		} else {
			off = append(off, r.Name)
		}
	}
	// 没开自动巡逻但手动跑过，结果还有用，照常显示。
	if ps, err := a.Patrol.LoadPatrol(); !a.Cfg.PatrolEnabled && err == nil && ps.LastRun.IsZero() {
		off = append(off, "巡逻")
	} else {
		fmt.Fprintf(w, "巡逻：%s\n", a.Patrol.PatrolStatus())
	}
	if s := a.Inbox.InboxStatus(st); s != "空" {
		fmt.Fprintf(w, "任务箱：%s\n", s)
	}
	if a.Cfg.SuggestEnabled { // 关着时不多一行，输出和原来一样
		fmt.Fprintf(w, "闲时找活：%s\n", a.Suggest.SuggestStatus(st, now))
	}
	if a.Cfg.SenseEnabled { // 关着时不多一行
		fmt.Fprintf(w, "主动感知：%s\n", a.Sense.SenseStatus())
	}
	if a.Cfg.ReleaseAssistEnabled { // 关着时不多一行
		fmt.Fprintf(w, "发版助手：%s\n", a.Release.ReleaseStatus())
	}
	if a.Cfg.NotifyEnabled {
		fmt.Fprintf(w, "推送：%s\n", a.Notify.NotifyStatus())
	} else {
		off = append(off, "推送")
	}
	if l := a.Chat.ListenStatus(); l != "" {
		fmt.Fprintf(w, "监听：%s\n", l)
	}
	if len(off) > 0 {
		fmt.Fprintf(w, "没开：%s（docs/guide/configuration.md 说明怎么开）\n", strings.Join(off, "、"))
	}

	if st != nil {
		// 夜班等的就是「运行中」变成 0，所以它和复盘不算在里面，另列一段
		for _, j := range st.Running() {
			if state.TakesSlot(j.Kind) {
				continue
			}
			alive := a.jobProcNote(j)
			fmt.Fprintf(w, "定点任务在跑：%s，任务 %s，已跑 %s（超时 %s），pid %d%s\n", j.Project, j.ID,
				now.Sub(j.StartedAt).Round(time.Minute), textfmt.FormatDuration(a.Runner.JobTimeout(j)), j.PID, alive)
		}
		for _, l := range a.Discuss.StatusLines() {
			fmt.Fprintln(w, l)
		}
		for _, l := range a.Gate.StatusLines(st) {
			fmt.Fprintln(w, l)
		}
		for _, l := range a.Runner.FirstHoldLines(st) {
			fmt.Fprintln(w, l)
		}
		for _, l := range a.Runner.ResumeBlockLines(st) {
			fmt.Fprintln(w, l)
		}
		running := st.Tasks()
		fmt.Fprintf(w, "\n运行中（%d/%d）：\n", len(running), a.Cfg.MaxParallel)
		if len(running) == 0 {
			fmt.Fprintln(w, "  无")
		}
		for _, j := range running {
			alive := a.jobProcNote(j)
			if idle, ok := a.Runner.IdleFor(j); alive == "" && ok && idle >= a.Cfg.StallAfter.Duration {
				alive = "，疑似卡死（会话记录 " + textfmt.FormatIdle(idle) + " 没增长，不会自动杀；要停就 agentq stop " + j.ID + "）"
			}
			alive += runner.ProgressLabel(j)
			resume := ""
			if j.ResumeOf != "" {
				resume = "续跑中（接中断的 " + j.ResumeOf + "），"
			}
			fmt.Fprintf(w, "  %s  %s「%s」%s已运行 %s，pid %d%s\n", j.ID, j.Project, textfmt.TruncRunes(j.Title, 30),
				resume, now.Sub(j.StartedAt).Round(time.Minute), j.PID, alive)
		}
		if l := a.Newproj.HeldStatus(st); l != "" {
			fmt.Fprintf(w, "\n%s\n", l)
		}
	}

	fmt.Fprintf(w, "\n各项目「待做」（%s）：\n", a.ProjectsRoot())
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		fmt.Fprintf(w, "  扫描失败：%v\n", err)
	}
	empty := 0
	for _, p := range projects {
		items, err := p.Items()
		if err != nil {
			fmt.Fprintf(w, "  %s：BACKLOG 读不出：%v\n", p.Name, err)
			continue
		}
		if len(items) == 0 {
			empty++
			continue
		}
		inProg, stuck := 0, 0
		for _, it := range items {
			if it.InProgress {
				inProg++
			} else if it.Stuck {
				stuck++
			}
		}
		fmt.Fprintf(w, "  %s：%d 条（进行中 %d，卡住 %d）\n", p.Name, len(items), inProg, stuck)
		for _, it := range items {
			if n := it.DepNote(); n != "" {
				fmt.Fprintf(w, "    %s\n", n)
			}
		}
		if err := a.Runner.ProjectBlock(st, p.Name, p.Dir); err != nil {
			fmt.Fprintf(w, "    %v\n", err)
		}
	}
	if empty > 0 {
		fmt.Fprintf(w, "  其余 %d 个项目「待做」为空或没有 BACKLOG\n", empty)
	}
	if len(a.Cfg.ExcludeProjects) > 0 {
		fmt.Fprintf(w, "  被排除（exclude_projects，自动流程不碰）：%s\n", strings.Join(a.Cfg.ExcludeProjects, "、"))
	}
	if gone := a.Backlog.MissingExtras(); len(gone) > 0 {
		fmt.Fprintf(w, "  extra_projects 里这些用不了（不在了、不是 Git 主目录，或和 projects_root 下的项目重名），没在看：%s\n", strings.Join(gone, "、"))
	}
	if st != nil {
		for _, s := range a.Orphan.StoppedOrphans(st) {
			fmt.Fprintf(w, "  已停止：%s\n", s)
		}
	}

	if st != nil {
		var ended []*state.Job
		for _, j := range st.Jobs {
			if j.Status != state.StatusRunning && j.EndedAt != nil {
				ended = append(ended, j)
			}
		}
		state.SortJobsByEnd(ended)
		if len(ended) > 10 {
			ended = ended[:10]
		}
		fmt.Fprintln(w, "\n最近结束：")
		if len(ended) == 0 {
			fmt.Fprintln(w, "  无")
		}
		for _, j := range ended {
			cost := ""
			if j.Usage != nil {
				cost = "（" + backend.JobUsageShort(*j.Usage) + "）"
			} else if !a.BackendFor(j.Backend).Caps().Usage {
				cost = "（用量未知）"
			}
			fmt.Fprintf(w, "  %s  %s %s「%s」%s%s：%s%s\n", textfmt.WhenCN(*j.EndedAt), j.ID, j.Project,
				textfmt.TruncRunes(j.Title, 30), state.JobLabel(j), resumeNote(j), textfmt.OneLine(j.Summary, 80), cost)
		}
	}
	return nil
}

// resumeNote：中断任务后面标上续跑情况。
func resumeNote(j *state.Job) string {
	switch {
	case j.ResumedBy != "":
		return "（已续跑：" + j.ResumedBy + "）"
	case j.ResumePending:
		return "（等待续跑）"
	case j.ResumeOf != "":
		return "（续跑自 " + j.ResumeOf + "）"
	}
	return ""
}

func (a *App) CmdLogs(id string) error {
	var j *state.Job
	if st, err := state.Load(a.StatePath()); err == nil {
		j, err = st.Find(id)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("状态文件读不出：%w；任务文件直接看 %s", err, a.JobDir(id))
	}
	dir := a.JobDir(j.ID)
	w := a.Out
	fmt.Fprintf(w, "任务 %s：%s「%s」\n", j.ID, j.Project, j.Title)
	fmt.Fprintf(w, "状态：%s%s；启动 %s", state.JobLabel(j), resumeNote(j), j.StartedAt.Format("2006-01-02 15:04"))
	if j.EndedAt != nil {
		fmt.Fprintf(w, "；结束 %s", j.EndedAt.Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "；pid %d\n", j.PID)
	if j.SessionID != "" {
		if h, ok := a.BackendFor(j.Backend).(backend.ResumeHint); ok {
			fmt.Fprintf(w, "会话：%s（在 %s 里 %s 可接着看）\n", j.SessionID, j.ProjectDir, h.ResumeCommand(j.SessionID))
		} else {
			fmt.Fprintf(w, "会话：%s（后端 %s）\n", j.SessionID, a.BackendFor(j.Backend).Name())
		}
	}
	fmt.Fprintf(w, "目录：%s\n", dir)
	if j.Output != "" {
		fmt.Fprintf(w, "产出：%s\n", j.Output)
	}
	if j.Status == state.StatusRunning {
		fmt.Fprintln(w, "\n还在运行，结果要等它结束。")
	} else {
		fmt.Fprintf(w, "\n摘要：%s\n", j.Summary)
		if res := a.Ledger.ReadResult(j); res.HaveTerminal && strings.TrimSpace(res.Text) != "" {
			fmt.Fprintf(w, "\n结果全文：\n%s\n", strings.TrimSpace(res.Text))
		}
	}
	tail := fsx.TailLines(a.Ledger.JobOutput(j.ID, "stderr.log"), 40)
	if strings.TrimSpace(tail) == "" {
		fmt.Fprintln(w, "\nstderr：空")
	} else {
		fmt.Fprintf(w, "\nstderr 末尾：\n%s\n", tail)
	}
	return nil
}

func (a *App) CmdStop(id string) error {
	lock, err := a.Runner.LockForCommand()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出：%w", err)
	}
	msg, err := a.Runner.StopByID(st, id)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, msg)
	return nil
}
