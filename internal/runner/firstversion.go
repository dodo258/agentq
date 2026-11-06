package runner

import (
	"fmt"

	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 新项目的第一版没进默认分支时，后面的条目不开工：它们要在第一版上面做。
// 看的是条目现状，不只是任务记录：第一条不在「待做」了（合并了、用户删了）就放开；用户放弃了就放开；
// 标记去掉了，第一条能被挑到，后面的继续等。出路都在 agentq status 里写明。

// FirstVersionPending：这个项目的第一版可能还挡着，不用读 BACKLOG 就知道的那部分：有记录，没合并，没放弃。
func FirstVersionPending(st *state.State, project string) bool {
	j := st.FirstVersionJob(project)
	return j != nil && (j.Gate == nil || (j.Gate.State != state.GateMerged && j.Gate.State != state.GateAbandoned))
}

// NoteFirstItem 记下 new 任务没做好的第一条：标着进行中的优先，没有就是第一条没卡住的，再没有就是第一条。「待做」是空的不用挡，返回假。
func (a *Service) NoteFirstItem(j *state.Job, name string, r state.NewReport) bool {
	items, err := backlog.Project{Name: name, Dir: r.Path}.Items()
	if err != nil {
		a.Logf("任务 %s：读不了 %s 的 BACKLOG，不知道第一条是哪条：%v", j.ID, name, err)
		return false
	}
	var it *backlog.Item
	for i := range items {
		switch x := &items[i]; {
		case x.InProgress && !x.Stuck:
			if it == nil || !it.InProgress {
				it = x
			}
		case it == nil && x.Eligible():
			it = x
		}
	}
	if it == nil && len(items) > 0 {
		it = &items[0]
	}
	if it == nil {
		return false
	}
	title := it.Title
	if title == "" {
		title = it.Text
	}
	j.FirstItem = &state.FirstItem{Project: name, Dir: r.Path, Fingerprint: it.Fingerprint, Title: title, Branch: backlog.MarkerBranch(it.Text), View: r.View, Assumptions: r.Assumptions}
	return true
}

// FirstHold 是挡着后面条目的第一版。
type FirstHold struct {
	Job  *state.Job
	Item backlog.Item
}

// FirstVersionHold：这个项目的第一版还挡着后面的条目。items 是项目现在的「待做」。
func FirstVersionHold(st *state.State, p backlog.Project, items []backlog.Item) (FirstHold, bool) {
	if !FirstVersionPending(st, p.Name) {
		return FirstHold{}, false
	}
	j := st.FirstVersionJob(p.Name)
	_, _, fp, title, _ := j.FirstTarget()
	for _, it := range items {
		if backlog.SameItemText(it, fp, title) {
			return FirstHold{Job: j, Item: it}, true
		}
	}
	return FirstHold{}, false // 第一条不在了
}

// firstReport：这一条是新项目的第一条时，接着做的任务带的报告：还是第一版，要过成品门、出交付单。
func firstReport(st *state.State, p backlog.Project, it backlog.Item) *state.NewReport {
	h, ok := FirstVersionHold(st, p, []backlog.Item{it})
	if !ok {
		return nil
	}
	if f := h.Job.FirstItem; f != nil {
		return &state.NewReport{Path: f.Dir, View: f.View, Assumptions: f.Assumptions}
	}
	return h.Job.NewReport.Next()
}

// reason 是挡着的真实原因，给日志和 status。
func (h FirstHold) reason(st *state.State, p backlog.Project) string {
	_, _, _, name, _ := h.Job.FirstTarget()
	title := "第一条「" + textfmt.TruncRunes(name, 30) + "」"
	switch {
	case h.Item.Stuck:
		return title + "卡住了"
	case st.RunningFor(p.Name, h.Item.Fingerprint) != nil:
		return title + "有任务在做"
	case state.GateActive(h.Job):
		return title + "在过门"
	case h.Item.InProgress:
		return title + "标着进行中，没人在做（24 小时没动静会自动接手）"
	}
	return title + "等着重做"
}

// NoteFirstHold 在日志里说一次为什么后面的条目不开工。同样的原因不重复。
func (a *Service) NoteFirstHold(st *state.State, p backlog.Project, h FirstHold) {
	why := h.reason(st, p)
	if h.Job.FirstNote == why {
		return
	}
	h.Job.FirstNote = why
	a.Logf("%s 第一版还没合并，%s，后面的条目不开工（agentq status 看怎么办）", p.Name, why)
	a.Ledger.WriteMeta(h.Job)
}

// FirstHoldLines：status 里那一段：哪个项目、卡在哪、三种解法。只读，不改状态。
func (a *Service) FirstHoldLines(st *state.State) []string {
	seen := map[string]bool{}
	var out []string
	for _, j := range st.Jobs {
		name, dir, _, _, ok := j.FirstTarget()
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		if !FirstVersionPending(st, name) {
			continue
		}
		// 主目录切走了：挡着的是这个，「不接任务」那一行写了怎么切回去。工作目录里是别的分支的 BACKLOG，第一条的现状看不准
		p := backlog.Project{Name: name, Dir: dir}
		if err := a.ProjectBlock(st, name, dir); err != nil {
			if items, ierr := p.Items(); ierr == nil && len(items) == 0 { // 那条分支上「待做」是空的，项目列表不会列它，这里说
				out = append(out, name+"："+err.Error())
			}
			continue
		}
		items, err := p.Items()
		if err != nil {
			continue
		}
		h, held := FirstVersionHold(st, p, items)
		if !held {
			continue
		}
		out = append(out, fmt.Sprintf("新项目 %s 第一版还没合并：%s，后面的条目先不开工。", name, h.reason(st, p)))
		if st.RunningFor(name, h.Item.Fingerprint) != nil || state.GateActive(h.Job) {
			continue
		}
		path := a.Tilde(p.BacklogPath())
		branch := backlog.MarkerBranch(h.Item.Text)
		if branch == "" {
			branch = h.Job.StuckBranch
		}
		if branch == "" && h.Job.FirstItem != nil {
			branch = h.Job.FirstItem.Branch
		}
		redo := "  重做：去掉 " + path + " 这一条的卡住或进行中标记并提交，下次 tick 接着做这一条"
		if branch != "" {
			redo += "（从分支 " + branch + "）"
		}
		out = append(out, redo,
			"  放弃：agentq gate abandon "+h.Job.ID+"，分支和条目留着，标卡住，后面的条目开工",
			"  删条目：把这一条从 "+path+" 删掉并提交，后面的条目开工")
	}
	return out
}
