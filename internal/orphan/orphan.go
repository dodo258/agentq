// Package orphan 处理无人认领的「进行中」条目：去标记、标卡住、起接手任务。
package orphan

import (
	"fmt"
	"os"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是孤儿标记处理的模块实例。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Ledger  *ledger.Service
	Notify  *notify.Service
	Runner  *runner.Service
}

// orphanIdle：标记时间、分支最后提交、worktree 文件修改时间、上一个任务结束时间都早于这么久，才当成没人在做。
// 用户有时开着交互会话按 agent-task 认领，这个窗口内绝对不动。
const orphanIdle = 24 * time.Hour

type orphanAction int

const (
	orphanLeave     orphanAction = iota // 不动
	orphanClear                         // 标记没写分支：去掉标记，下次当普通条目
	orphanClearWait                     // 写了分支但没内容（分支没有默认分支之外的提交或已不在）：去掉标记，通知，等 retry_after 再挑
	orphanTakeover                      // 分支有未合并内容或 worktree 有未提交改动：起接手任务
)

// OrphanJob 是一条要接手的孤儿条目。
type OrphanJob struct {
	Project backlog.Project
	Item    backlog.Item
	Facts   backlog.OrphanFacts
}

// decideOrphan：active 是已知的最后活动时间。未来的时间（钟不准）当活跃。未提交的改动优先于「已合并」，免得丢东西。
func decideOrphan(f backlog.OrphanFacts, active, now time.Time) orphanAction {
	switch {
	case now.Sub(active) < orphanIdle:
		return orphanLeave
	case f.Dirty || f.Ahead > 0:
		return orphanTakeover
	case f.Named != "": // 分不清活已合并（分支可能也删了）还是没动手，不猜，都等 retry_after
		return orphanClearWait
	}
	return orphanClear
}

// orphanCovered：已经有任务管着这一条：运行中，或等着续跑。
func orphanCovered(st *state.State, p backlog.Project, it backlog.Item) bool {
	for _, j := range st.Running() {
		if runner.JobOnItem(j, p, it) {
			return true
		}
	}
	for _, j := range st.PendingResumes() {
		if runner.JobOnItem(j, p, it) {
			return true
		}
	}
	for _, j := range st.Jobs { // 自动停的、撞两次预算的：卡住标记写不上时下次 tick 再写，不接手。在过门的归门管
		if (j.StuckPending != "" || state.GateActive(j)) && runner.JobOnItem(j, p, it) {
			return true
		}
	}
	return false
}

// patrolTakeoverListed：巡逻已经在「待做」写了接手这一条的待做（见 takeOverTodo），不重复接手；那条卡住了也是已经报过用户。
func patrolTakeoverListed(items []backlog.Item, it backlog.Item) bool {
	prefix := backlog.TakeOverPrefix + textfmt.TruncRunes(it.Title, 30) + "」"
	for _, o := range items {
		if strings.HasPrefix(o.Title, prefix) {
			return true
		}
	}
	return false
}

// orphanCandidate：这一条是孤儿标记：标在行尾、没卡住、没有任务管着、巡逻也没写接手待做。
func orphanCandidate(st *state.State, p backlog.Project, items []backlog.Item, it backlog.Item) bool {
	return it.InProgress && !it.Stuck && backlog.MarkerAtEnd.MatchString(it.Text) && !orphanCovered(st, p, it) && !patrolTakeoverListed(items, it)
}

// stoppedOrphan 是用户停掉任务后标记还留着的条目。
type stoppedOrphan struct {
	Project, Title, JobID string
}

func (s stoppedOrphan) String() string {
	return fmt.Sprintf("%s「%s」（任务 %s 被停止，标记还在，不会自动接手；要接着做就去掉进行中标记，不做就移走）", s.Project, textfmt.TruncRunes(s.Title, 30), s.JobID)
}

// StoppedOrphans 列出 agentq status 和「状态」要提示的条目。没有被停止的任务时不读 BACKLOG。
func (a *Service) StoppedOrphans(st *state.State) []stoppedOrphan {
	any := false
	for _, j := range st.Jobs {
		any = any || (j.Kind == "" && j.Status == state.StatusKilled)
	}
	if !any {
		return nil
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return nil
	}
	var out []stoppedOrphan
	for _, p := range projects {
		items, err := p.Items()
		if err != nil {
			continue
		}
		for _, it := range items {
			if !orphanCandidate(st, p, items, it) {
				continue
			}
			if last := runner.LastTaskJob(st, p, it); last != nil && last.Status == state.StatusKilled {
				out = append(out, stoppedOrphan{Project: p.Name, Title: it.Title, JobID: last.ID})
			}
		}
	}
	return out
}

// noteStopped：用户停掉的任务留下的标记不自动处理，只记一次日志（StopNoted 存在任务上，重启不重报）。
func (a *Service) noteStopped(p backlog.Project, it backlog.Item, last *state.Job) {
	if last.StopNoted {
		return
	}
	last.StopNoted = true
	a.Ledger.WriteMeta(last)
	msg := "孤儿标记：" + stoppedOrphan{Project: p.Name, Title: it.Title, JobID: last.ID}.String()
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
}

// takenOver：这个指纹接手过（进程起来过才算，启动失败的不算）。记录就是 state.json 里 Takeover 的任务。
func takenOver(st *state.State, project, fp string) bool {
	for _, j := range st.Jobs {
		if j.Takeover && state.SameItem(j, project, fp) && j.PID > 0 {
			return true
		}
	}
	return false
}

// ScanOrphans 处理各项目的孤儿标记（去掉标记、标卡住都在这里做），返回要接手的条目，由 schedule 按名额和额度启动。
// 不调模型。项目目录、BACKLOG 读不了时 schedule 会报，这里跳过。
func (a *Service) ScanOrphans(st *state.State) []OrphanJob {
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return nil
	}
	var out []OrphanJob
	stopped, unread := map[*state.Job]bool{}, map[string]bool{}
	for _, p := range projects {
		if st.HeldProject(p.Name) { // 用户停掉的 new 留下的目录，接手和去标记都算自动做
			continue
		}
		if st.NewProjectBusy(p.Name) { // 正在建的新项目：第一条的标记归那个 new 任务管，分支可能还没提交
			continue
		}
		items, err := p.Items()
		if err != nil {
			unread[p.Dir] = true
			continue
		}
		for _, it := range items {
			if !orphanCandidate(st, p, items, it) {
				continue
			}
			if last := runner.LastTaskJob(st, p, it); last != nil && last.Status == state.StatusKilled {
				stopped[last] = true
				a.noteStopped(p, it, last)
				continue
			}
			if o, ok := a.handleOrphan(st, p, it); ok {
				out = append(out, o)
			}
		}
	}
	// 用户删了标记、或这一条又有了新任务：解除，这个任务记录照常过期
	for _, j := range st.Jobs {
		if j.StopNoted && !stopped[j] && !unread[j.ProjectDir] {
			j.StopNoted = false
			a.Ledger.WriteMeta(j)
		}
	}
	return out
}

// markerActivity 返回标记写下的时间；标记没写时间就用 BACKLOG 的修改时间（不早于写标记的时候），读不到就当刚改过。
func markerActivity(p backlog.Project, it backlog.Item, now time.Time) time.Time {
	if t, ok := backlog.MarkerTime(it.Text, now); ok {
		return t
	}
	if fi, err := os.Stat(p.BacklogPath()); err == nil {
		return fi.ModTime()
	}
	return now
}

// handleOrphan 处理一条孤儿标记，要接手时返回 true。
func (a *Service) handleOrphan(st *state.State, p backlog.Project, it backlog.Item) (OrphanJob, bool) {
	now := a.Now()
	unknown := func(err error) (OrphanJob, bool) {
		a.Logf("孤儿标记：查不了 %s「%s」的现状，不动：%v", p.Name, textfmt.TruncRunes(it.Title, 40), err)
		return OrphanJob{}, false
	}
	last := runner.LastTaskJob(st, p, it)
	recorded := runner.TakeoverRecorded(st, p, it)
	active := markerActivity(p, it, now)
	if takenOver(st, p.Name, it.Fingerprint) && last != nil && last.EndedAt != nil {
		// 接手任务结束后标记又写过，多半是用户重新认领（标记只精确到分钟，同一分钟也算）
		if !active.Before(last.EndedAt.Truncate(time.Minute)) && now.Sub(active) < orphanIdle {
			return OrphanJob{}, false
		}
		// 上游瞬时错误没开工，不算用掉那一次：退避过后再接手
		if last.Transient && state.RetryBlocked(st, p.Name, it.Fingerprint, now, a.Cfg.RetryAfter.Duration) {
			return OrphanJob{}, false
		}
		f, err := a.Backlog.OrphanFactsOn(p, it, *last.EndedAt, recorded)
		if err != nil {
			return unknown(err)
		}
		if f.Activity.After(*last.EndedAt) && now.Sub(f.Activity) < orphanIdle {
			return OrphanJob{}, false // 接手任务结束后又有人在动，多半是用户
		}
		if last.Transient {
			return OrphanJob{Project: p, Item: it, Facts: f}, true
		}
		a.markOrphanStuck(p, it, f, last)
		return OrphanJob{}, false
	}
	if last != nil && last.EndedAt != nil && last.EndedAt.After(active) {
		active = *last.EndedAt
	}
	// 上一个任务中断了又续跑不了（后端不能续跑，或者是新项目的第一版，会话在 projects_root）：接手就是它的续跑，不用等没人在做的窗口
	noResume := last != nil && last.EndedAt != nil && last.Status == state.StatusInterrupted && (!a.BackendFor(last.Backend).Caps().Resume || last.SessionInRoot())
	if now.Sub(active) < orphanIdle && !noResume {
		return OrphanJob{}, false
	}
	f, err := a.Backlog.OrphanFactsOn(p, it, now.Add(-orphanIdle), recorded)
	if err != nil {
		return unknown(err)
	}
	if f.Activity.After(active) {
		active = f.Activity
	}
	if noResume {
		active = time.Time{}
	}
	switch decideOrphan(f, active, now) {
	case orphanClear:
		a.clearOrphan(st, p, it, f, false)
	case orphanClearWait:
		a.clearOrphan(st, p, it, f, true)
	case orphanTakeover:
		return OrphanJob{Project: p, Item: it, Facts: f}, true
	}
	return OrphanJob{}, false
}

// clearOrphan 去掉进行中标记并提交。提交不了（主目录不在默认分支、BACKLOG 有改动）只记日志，下次 tick 再来。
// wait 为真时多半是活已合并、只是 BACKLOG 没更新：等 retry_after 再挑，让用户来得及处理。先存好等待记录再改 BACKLOG，
// 中途退出最多是标记还在，不会去掉标记后马上重做。
func (a *Service) clearOrphan(st *state.State, p backlog.Project, it backlog.Item, f backlog.OrphanFacts, wait bool) {
	title := textfmt.TruncRunes(it.Title, 40)
	if wait {
		if st.ItemHold == nil {
			st.ItemHold = map[string]time.Time{}
		}
		st.ItemHold[state.HoldKey(p.Name, it.Fingerprint)] = a.Now().Add(a.Cfg.RetryAfter.Duration)
		if err := state.Save(a.StatePath(), st); err != nil {
			delete(st.ItemHold, state.HoldKey(p.Name, it.Fingerprint))
			a.Logf("孤儿标记：%s「%s」没人在做，但存不了等待记录，没去掉标记：%v", p.Name, title, err)
			return
		}
	}
	changed := false
	err := a.Backlog.CommitBacklog(p, "待办：去掉「"+title+"」的进行中标记（没人在做）", func(old string) (string, error) {
		text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, backlog.DropMarker)
		changed = text != old
		return text, nil
	})
	if err != nil || !changed {
		if wait {
			delete(st.ItemHold, state.HoldKey(p.Name, it.Fingerprint))
		}
		if err != nil {
			a.Logf("孤儿标记：%s「%s」没人在做，但没去掉标记：%v", p.Name, title, err)
		}
		return
	}
	msg := fmt.Sprintf("孤儿标记：%s「%s」没人在做（标记没写分支），已去掉进行中标记，下次当普通条目", p.Name, title)
	if wait {
		after := textfmt.FormatDuration(a.Cfg.RetryAfter.Duration)
		why := "分支 " + f.Named + " 已不在"
		if f.Branch != "" {
			why = "分支 " + f.Branch + " 没有 " + f.Default + " 之外的提交"
		}
		msg = fmt.Sprintf("孤儿标记：%s「%s」的%s，已去掉进行中标记，没勾完成，%s 后重新做", p.Name, title, why, after)
		a.Notify.Notify(notify.Notice{Cat: "patrol", Key: "orphan:" + it.Fingerprint, Text: p.Name + "「" + textfmt.TruncRunes(it.Title, 30) + "」去掉了进行中标记，" + after + " 后重新做",
			Lines: []string{why + "（可能已合并，也可能没动手）；没勾完成。已经做完就移到「已完成」，否则 " + after + " 后回到待做重新做"}})
	}
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
}

// markOrphanStuck 接手过还没做完：换成卡住标记并通知。分支和 worktree 留着。
func (a *Service) markOrphanStuck(p backlog.Project, it backlog.Item, f backlog.OrphanFacts, last *state.Job) {
	title := textfmt.TruncRunes(it.Title, 40)
	mark := "（卡住：接手后仍未完成）"
	if f.Branch != "" {
		mark = "（卡住：接手后仍未完成，分支 " + f.Branch + "）"
	}
	changed := false
	err := a.Backlog.CommitBacklog(p, "待办：「"+title+"」标卡住（接手后仍未完成）", func(old string) (string, error) {
		text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, func(line string) string { return backlog.DropMarker(line) + mark })
		changed = text != old
		return text, nil
	})
	if err != nil {
		a.Logf("孤儿标记：%s「%s」接手后仍未完成，但没标上卡住：%v", p.Name, title, err)
		return
	}
	if !changed {
		return
	}
	last.StuckMarked = true // 用户去掉标记就是表态了：不等 retry_after，从分支接着做（handoffFacts）
	if f.Branch != "" {
		last.StuckBranch = f.Branch
	}
	a.Ledger.WriteMeta(last)
	msg := fmt.Sprintf("孤儿标记：%s「%s」接手后仍未完成，已标卡住，等你处理；看 agentq logs %s", p.Name, title, last.ID)
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	line := "看 agentq logs " + last.ID
	if f.Branch != "" {
		line = "分支 " + f.Branch + " 留着；" + line
	}
	a.Notify.Notify(notify.Notice{Cat: "alert", Key: "orphan:" + it.Fingerprint, JobID: last.ID,
		Text: p.Name + "「" + textfmt.TruncRunes(it.Title, 30) + "」接手后仍未完成，标了卡住", Lines: []string{line}})
}

// LaunchTakeover 为孤儿条目起接手任务：新会话，prompt 写明分支、worktree、从现状接着做。
func (a *Service) LaunchTakeover(st *state.State, o OrphanJob) (*state.Job, error) {
	def, err := a.Runner.JobBranch(o.Project.Dir, runner.TakeoverRecorded(st, o.Project, o.Item))
	if err != nil {
		return nil, err
	}
	title := o.Item.Title
	if title == "" {
		title = o.Item.Text
	}
	j := &state.Job{Project: o.Project.Name, ProjectDir: o.Project.Dir, Item: o.Item.Text, Title: title, Fingerprint: o.Item.Fingerprint, Takeover: true, DefaultBranch: def,
		Summary: "接手没做完的条目（分支 " + o.Facts.Branch + "）"}
	note := a.Runner.GateTakeoverNote(st, j, o.Project, o.Item)
	j, err = a.Runner.Start(st, j, runner.BuildTakeoverPrompt(o.Project.Dir, o.Item.Text, o.Facts)+runner.BranchContract(def)+note, "")
	if err == nil {
		a.Ledger.AppendNightly(j)
	}
	return j, err
}
