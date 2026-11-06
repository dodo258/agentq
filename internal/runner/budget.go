package runner

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

const budgetResumeNote = "\n\n这次不是等通知退出的：上一个进程撞到预算上限（失控兜底，不是你做错了）。不重做做过的，不铺开新的范围，尽快收口。"

// JobBudget：各种任务的预算。任务箱在 inboxArgs 里传（参数顺序不变）；对话类任务不设预算，Router、追问有自己的小额度。
func (a *Service) JobBudget(kind string) float64 {
	switch kind {
	case "", state.KindNew:
		return a.Cfg.JobBudgetUSD
	case config.KindNight:
		return a.Cfg.NightBudgetUSD
	case config.KindReview:
		return a.Cfg.ReviewBudgetUSD
	case state.KindPatrol:
		return a.Cfg.PatrolBudgetUSD
	}
	return 0
}

// budgetHitBefore：这条续跑链上之前已经撞过预算（顺着 ResumeOf 往回看）。
func budgetHitBefore(st *state.State, j *state.Job) bool {
	for n := 0; j.ResumeOf != "" && n <= 20; n++ {
		prev, err := st.Find(j.ResumeOf)
		if err != nil {
			return false
		}
		if prev.BudgetHit {
			return true
		}
		j = prev
	}
	return false
}

// budgetResult 定撞预算的任务的结果：按中断处理，不算失败、不计连环失败。待做和 new 用同一会话自动接着做一次，再撞才放下；
// 夜班、复盘、巡逻分析不接着做，下次到点照跑（上次没跑完不会被当成没事跳过）。
func (a *Service) budgetResult(st *state.State, j *state.Job, summary string) (string, string) {
	j.BudgetHit = true
	note := "撞到预算上限（" + backend.BudgetLabel(a.JobBudget(j.Kind)) + "，失控兜底）"
	switch j.Kind {
	case "":
		note += "，分支和会话都留着" + a.budgetTaskNext(st, j)
	case state.KindNew:
		switch {
		case budgetHitBefore(st, j):
			note += "，成果都留着；接着做之后又撞，不再自动做，等你处理"
		case j.SessionID == "":
			note += "，成果都留着；没有会话 id，接不上，等你处理"
		default:
			j.ResumePending = true
			note += "，成果都留着；将自动用同一会话接着做一次"
		}
	default:
		note += "，不算失败；不接着做，下次到点照跑"
	}
	if summary != "" {
		note += "；结果最后：" + summary
	}
	return state.StatusInterrupted, note
}

// budgetTaskNext 定待做任务撞预算之后怎么办，返回写进摘要的话。条目不在进行中（还没认领，或用户动过）时不续跑，等 retry_after。
func (a *Service) budgetTaskNext(st *state.State, j *state.Job) string {
	if budgetHitBefore(st, j) {
		j.StuckPending = fmt.Sprintf("续跑后又撞预算上限 %s，任务 %s", backend.BudgetLabel(a.Cfg.JobBudgetUSD), j.ID)
		return "；续跑后又撞，标卡住，等你处理"
	}
	items, err := backlog.Project{Dir: j.ProjectDir}.Items()
	if err != nil {
		return "；核验 BACKLOG 失败：" + err.Error()
	}
	if _, ok := InProgressItem(items, j.Fingerprint); !ok {
		return "；条目已不在进行中，不续跑，等你核对"
	}
	if j.SessionID == "" {
		return a.noSessionNote(j)
	}
	j.ResumePending = true
	return "；将自动用同一会话续跑一次"
}

// noticeBudget：撞预算推一条（new 由 noticeNew 推，任务箱由 finishInbox 推）。
func (a *Service) noticeBudget(j *state.Job, stuck bool) {
	label := backend.BudgetLabel(a.JobBudget(j.Kind))
	name := j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」"
	n := notify.Notice{Cat: "alert", Key: "budget:" + j.ID, JobID: j.ID}
	logs := "详情 agentq logs " + j.ID
	switch {
	case j.ResumePending:
		n.Text = name + " 撞到预算上限，会接着做一次"
		n.Lines = []string{label + " 是失控兜底，分支和会话都留着；有名额时用同一会话续跑", logs}
	case stuck:
		n.Text = name + " 续跑后又撞预算上限，放下了"
		n.Lines = append([]string{label + " 是失控兜底，成果都留着"}, append(a.continueLines(j), logs)...)
	default:
		n.Text = name + " 撞到预算上限"
		n.Lines = []string{textfmt.OneLine(j.Summary, 200), logs}
	}
	a.Notify.Notify(n)
}

// CheckLiveBudget：没有美元硬上限、但运行中读得到用量的后端，tick 替它看预算，超了发 SIGTERM，滞后最多一个 tick。
// 收尾和 CLI 自己撞预算一样（续跑一次，再撞放下）。运行中读不到用量的后端只有超时兜底。
func (a *Service) CheckLiveBudget(st *state.State) {
	for _, j := range st.Running() {
		if j.Status != state.StatusRunning || j.PID <= 0 {
			continue
		}
		be := a.BackendFor(j.Backend)
		live, ok := be.(backend.UsageStream)
		if !ok || be.Caps().HardBudget {
			continue
		}
		limit := a.JobBudget(j.Kind)
		if j.Kind == state.KindInbox {
			limit = a.Cfg.JobBudgetUSD
		}
		if limit <= 0 {
			continue
		}
		u := live.LiveUsage(a.Ledger.JobRef(j))
		if u == nil || u.CostUnknown || u.CostUSD < limit || a.JobLive(j) != proc.ProcOurs {
			continue
		}
		how := "SIGTERM"
		if proc.KillGroup(j.PID, proc.KillGrace) {
			how = "SIGTERM 后 SIGKILL"
		}
		a.Logf("任务 %s 花到 $%.2f，超过预算 %s，已发 %s（后端 %s 没有硬上限）", j.ID, u.CostUSD, backend.BudgetLabel(limit), how, be.Name())
		if j.Kind == state.KindInbox {
			status, summary := a.Hooks.InboxResult(j, backend.Result{HaveTerminal: true, Stop: backend.StopMaxBudget}, state.StatusFailed, "")
			a.Finish(st, j, status, summary)
			continue
		}
		status, summary := a.budgetResult(st, j, "")
		a.Finish(st, j, status, summary)
	}
}

// autoStopRunaway：「子代理失控」命中时停掉任务（调用方拿着 tick.lock，进程在跑）。保留分支和 worktree，写卡住标记，通知用户。
// 不是用户 stop：状态记中断、不用 statusKilled（孤儿逻辑、status、任务箱和 new 的收尾都把 statusKilled 当成用户的决定）；
// 不计连环失败，巡逻不再报。new 没有 BACKLOG 条目可标，多出的目录和用户停掉时一样不自动做；任务箱标卡住（finishInbox）。
func (a *Service) autoStopRunaway(st *state.State, j *state.Job, h backend.ProgressHit) {
	hard := proc.KillGroup(j.PID, proc.KillGrace)
	how := "SIGTERM"
	if hard {
		how = "SIGTERM 后 SIGKILL"
	}
	j.AutoStop = h.Signal
	why := backend.ProgressNames[h.Signal] + "，" + h.Evidence
	switch j.Kind {
	case "":
		if !a.Backlog.Excluded(j.Project) {
			j.StuckPending = why + "，任务 " + j.ID
		}
	case state.KindNew:
		j.HeldProjects = a.Hooks.NewDirs(st, j)
	}
	a.Finish(st, j, state.StatusInterrupted, why+"，已自动停止（"+how+"）；分支和 worktree 留着")
	if j.Kind != "" { // 任务箱、new 在 finish 里推过
		return
	}
	a.Notify.Notify(notify.Notice{Cat: "alert", Key: "progress:" + j.ID + ":" + h.Signal, JobID: j.ID,
		Text:  j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」" + backend.ProgressNames[h.Signal] + "，自动停了",
		Lines: append([]string{h.Evidence}, append(a.continueLines(j), "详情 agentq logs "+j.ID)...)})
}

// continueLines：放下的任务在通知里写清成果在哪、两种接法。
func (a *Service) continueLines(j *state.Job) []string {
	where := "没记下分支（标记里没写）"
	if j.StuckBranch != "" {
		where = "分支 " + j.StuckBranch + " 留着"
		if j.StuckWorktree != "" {
			where = "分支 " + j.StuckBranch + "、worktree " + a.Tilde(j.StuckWorktree) + " 留着"
		}
	}
	lines := []string{where, "接着做：回复这条消息，接回原会话和分支；或者去掉卡住标记，agent 从分支现状接着做"}
	if j.StuckPending != "" {
		lines = append(lines, "卡住标记还没写上，下次 tick 再写")
	}
	return lines
}

// writeStuckMark 把 j.StuckPending 写成条目上的卡住标记并提交，写上才清空。提交不了（主目录不在默认分支、BACKLOG 有改动）只记日志，下次 tick 再来。
// 条目已经不在「待做」、已经标了卡住时不用写。分支名从进行中标记里取。
func (a *Service) writeStuckMark(j *state.Job) {
	if a.Backlog.Excluded(j.Project) { // 不写就不用每次 tick 再试；tick 本来也不会接这个项目的活
		a.Logf("任务 %s：%s 在 exclude_projects 里，不写卡住标记", j.ID, j.Project)
		j.StuckPending = ""
		a.Ledger.WriteMeta(j)
		return
	}
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	// 主目录切走了：工作目录里是别的分支的 BACKLOG，读不到这一条会当已经不用写。等切回来。记下的分支没了就写在现在的默认分支上：这只是个标记，不合代码。
	onRecorded := j.DefaultBranch != "" && a.Gitx.HasBranch(j.ProjectDir, j.DefaultBranch)
	if onRecorded && !a.Gitx.OnBranch(j.ProjectDir, j.DefaultBranch) {
		a.Logf("任务 %s：主目录不在 %s，卡住标记下次 tick 再写", j.ID, j.DefaultBranch)
		return
	}
	items, err := p.Items()
	if err != nil {
		a.Logf("任务 %s：读不了 BACKLOG，卡住标记下次 tick 再写：%v", j.ID, err)
		return
	}
	done := func() {
		j.StuckPending = ""
		a.Ledger.WriteMeta(j)
	}
	it, found := stuckTarget(j, p, items)
	if !found || it.Stuck {
		done()
		return
	}
	a.noteStuckBranch(j, p, it)
	mark := "（卡住：" + textfmt.RedactPaths(j.StuckPending) // 原因里可能有 git 报错、项目路径，这行要提交进仓库
	if j.StuckBranch != "" {
		mark += "，分支 " + j.StuckBranch
	}
	mark += "）"
	title := textfmt.TruncRunes(it.Title, 40)
	changed := false
	edit := func(old string) (string, error) {
		text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, func(line string) string { return backlog.DropMarker(line) + mark })
		changed = text != old
		return text, nil
	}
	msg := "待办：「" + title + "」标卡住（agentq 自动放下）"
	if onRecorded {
		err = a.Backlog.CommitBacklogNamedOn(p, j.DefaultBranch, msg, edit)
	} else {
		err = a.Backlog.CommitBacklog(p, msg, edit)
	}
	if err != nil {
		a.Logf("任务 %s：卡住标记没写上，下次 tick 再写：%v", j.ID, err)
		return
	}
	if !changed {
		return // 读完之后条目刚被改过，下次 tick 再看
	}
	j.StuckMarked = true
	done()
	note := fmt.Sprintf("%s「%s」标了卡住：%s", p.Name, title, textfmt.OneLine(mark, 200))
	a.Logf("%s", note)
	a.Ledger.AppendNightlyLine(a.Now(), note)
}

// stuckTarget 找任务做的那一条：先比指纹，同标题的别的条目（别人认领的、已经卡住的）不能被当成它；
// 指纹对不上（agent 改过正文）才按标题找。
func stuckTarget(j *state.Job, p backlog.Project, items []backlog.Item) (backlog.Item, bool) {
	for _, x := range items {
		if state.SameItem(j, p.Name, x.Fingerprint) {
			return x, true
		}
	}
	for _, x := range items {
		if JobOnItem(j, p, x) {
			return x, true
		}
	}
	return backlog.Item{}, false
}

// noteStuckBranch 记下这一条的分支和 worktree（标记去掉后就只有这里有了）。查不了 git 时只记标记里写的分支名。
// 标记里没写分支时不动（接手任务还没认领就又被放下，来源分支记在任务上）。
func (a *Service) noteStuckBranch(j *state.Job, p backlog.Project, it backlog.Item) {
	b := backlog.MarkerBranch(it.Text)
	if b == "" {
		return
	}
	j.StuckBranch = b
	if f, err := a.Backlog.OrphanFacts(p, it, time.Time{}); err == nil {
		j.StuckWorktree = f.Worktree
	}
}

// AutoStuckRe：agentq 自己写的卡住标记，在行尾，写着是哪个任务（见 writeStuckMark）。用户写的、正文提到的都不算（LESSONS：只认固定格式）。
var AutoStuckRe = regexp.MustCompile(`（卡住：(?:子代理失控，|续跑后又撞预算上限 )[^）]*任务 ([^，）]+)[^）]*）\s*$`)

// autoStuckJob 从条目里取写下卡住标记的任务 id，没有返回空。
func autoStuckJob(text string) string {
	if m := AutoStuckRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// StuckResumable：用户回复了放下的任务：条目标着 agentq 写的卡住标记、任务记录对得上（自动停或撞预算，同一个会话）时返回那个任务，可以写模式接回。
func (a *Service) StuckResumable(st *state.State, th notify.Thread, items []backlog.Item) *state.Job {
	for _, it := range items {
		id := autoStuckJob(it.Text)
		if id == "" {
			continue
		}
		j := a.Notify.LookupJob(st, id)
		if j == nil || j.Kind != "" || (j.AutoStop == "" && !j.BudgetHit) || j.SessionID != th.SessionID || j.Fingerprint != th.Fingerprint {
			continue
		}
		return j
	}
	return nil
}

// ReclaimStuck：接回之前把 agentq 写的卡住标记换回进行中并提交。提交不了就不接回，免得任务跑着、条目还标着卡住。
// 标记已经不在了（用户动过）不用换。
func (a *Service) ReclaimStuck(j *state.Job) error {
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	items, err := p.Items()
	if err != nil {
		return err
	}
	for _, it := range items {
		if autoStuckJob(it.Text) != j.ID {
			continue
		}
		claim := "（进行中：" + a.Now().Format("2006-01-02 15:04") + "）"
		if j.StuckBranch != "" {
			claim = "（进行中：" + j.StuckBranch + " " + a.Now().Format("2006-01-02 15:04") + "）"
		}
		changed := false
		err := a.Backlog.CommitBacklogNamed(p, "待办：「"+textfmt.TruncRunes(it.Title, 40)+"」接回（用户回复）", func(old string) (string, error) {
			text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, func(line string) string {
				return strings.TrimRight(AutoStuckRe.ReplaceAllString(line, ""), " \t") + claim
			})
			changed = text != old
			return text, nil
		})
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("条目刚被改过，再回复一次")
		}
		break
	}
	j.StuckMarked = false
	a.Ledger.WriteMeta(j)
	return nil
}

// ContinueNote：接回放下的任务时写进提示词：为什么放下、成果在哪。
func (a *Service) ContinueNote(j *state.Job) string {
	var b strings.Builder
	switch {
	case j.AutoStop != "":
		b.WriteString("\n\n这次不是等通知退出的：上一个进程因为" + backend.ProgressNames[j.AutoStop] + "被 agentq 停了。不要再大量派子代理、也不要让子代理再派子代理，主线自己做。")
	case j.BudgetHit:
		b.WriteString(budgetResumeNote)
	}
	if j.StuckBranch != "" {
		b.WriteString("分支 " + j.StuckBranch)
		if j.StuckWorktree != "" {
			b.WriteString("，worktree " + j.StuckWorktree)
		}
		b.WriteString("，从现状接着做，不重做。")
	}
	return b.String()
}

// handoffNote：用户去掉卡住标记后，新任务用接手的提示词，多这一句。
const handoffNote = "用户去掉了 agentq 写的卡住标记，要接着做。条目现在没有进行中标记：认领时写上面这个分支和当前时间，不另开分支。"

// handoffFacts：条目上一个任务被 agentq 放下过、用户去掉了卡住标记（条目又能挑到了），或者上一个接手任务还没认领就结束了时，
// 返回记下的分支现状。分支已经不在（删了、合了）或查不了时返回 false，当普通条目。
func (a *Service) handoffFacts(st *state.State, p backlog.Project, it backlog.Item) (backlog.OrphanFacts, bool) {
	branch := ""
	if last := LastTaskJob(st, p, it); last != nil && (last.StuckMarked || last.Handoff) {
		branch = last.StuckBranch
	} else if h, ok := FirstVersionHold(st, p, []backlog.Item{it}); ok && h.Job.FirstItem != nil { // new 任务没做好的第一条：用户去掉了进行中标记
		branch = h.Job.FirstItem.Branch
	}
	if branch == "" {
		return backlog.OrphanFacts{}, false
	}
	f, err := a.Backlog.OrphanFacts(p, backlog.Item{Text: "（进行中：" + branch + "）"}, time.Time{})
	if err != nil {
		a.Logf("这一条记下的分支 %s 查不了，当普通条目做：%v", branch, err)
		return backlog.OrphanFacts{}, false
	}
	return f, f.Branch != ""
}

// noteHandoffClaim：接手任务结束时看它认领过没有：条目标了进行中、卡住，或已不在待做，就是认领过了，之后的任务不再继承来源分支。
// BACKLOG 读不了时不知道，留着：继承只是提示词多写了分支，丢了分支才是丢成果。
func (a *Service) noteHandoffClaim(j *state.Job) {
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	items, err := p.Items()
	if err != nil {
		return
	}
	for _, it := range items {
		if JobOnItem(j, p, it) {
			if it.Eligible() {
				return
			}
			break
		}
	}
	j.Handoff = false
}

// RetryStuckMarks：tick 补写之前没写上的卡住标记。暂停时也写：只是记账，不启动东西。
func (a *Service) RetryStuckMarks(st *state.State) {
	for _, j := range st.Jobs {
		if j.Status != state.StatusRunning && j.Kind == "" && j.StuckPending != "" {
			a.writeStuckMark(j)
		}
	}
}
