package gate

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// decideKey：门停住时那张决定卡片在推送队列里的 Key。
func decideKey(jobID string) string { return "gate:" + jobID }

// supersede：卡在门上等用户定的任务，同一条目后来由别的任务做完合并了，或条目已经勾成完成、写着这个任务开始之后的合并提交，就不用再定了：
// 门记录标成已被取代并写明被谁取代，status 不再列，没发出去的决定卡片撤掉；已经发出去的卡片由推送改成结论（notify.GateSettled）。
func (a *Service) supersede(st *state.State) {
	texts := map[string]string{} // 一次 tick 每个项目只读一次 BACKLOG
	for _, j := range st.Jobs {
		if !state.GateStuckNow(j) || j.Status == state.StatusRunning {
			continue
		}
		by, merge := mergedLater(st, j)
		if merge == "" {
			merge = a.finishedAfter(j, texts)
		}
		if merge != "" {
			a.markSuperseded(j, by, merge)
		}
	}
}

// mergedLater：同项目、同一条目（指纹相同）在 j 之后开始的任务里合并了的那个。没有返回空。
func mergedLater(st *state.State, j *state.Job) (id, merge string) {
	if j.Fingerprint == "" {
		return "", ""
	}
	for _, n := range st.Jobs {
		if n == j || n.Project != j.Project || n.Fingerprint != j.Fingerprint || !n.StartedAt.After(j.StartedAt) {
			continue
		}
		if g := n.Gate; g != nil && g.State == state.GateMerged && g.Merge != "" {
			return n.ID, g.Merge
		}
	}
	return "", ""
}

// finishedAfter：默认分支的 BACKLOG 里 j 这一条已经完成，写的合并提交在 j 开始之后，返回这个提交（完整号）。
// 读不出、认不出都返回空：门照旧等用户定，下次 tick 再看。
func (a *Service) finishedAfter(j *state.Job, texts map[string]string) string {
	if j.Title == "" {
		return ""
	}
	text, ok := texts[j.ProjectDir]
	if !ok {
		text = a.defaultBacklog(j)
		texts[j.ProjectDir] = text
	}
	c := backlog.FinishedCommit(text, j.Title)
	if c == "" {
		return ""
	}
	out, err := a.Gitx.Git(j.ProjectDir, "log", "-1", "--format=%H %ct", c+"^{commit}")
	if err != nil {
		return ""
	}
	full, ts, _ := strings.Cut(out, " ")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || !time.Unix(sec, 0).After(j.StartedAt) { // 以前同名条目的旧号
		return ""
	}
	return full
}

// defaultBacklog：任务记下的默认分支上提交了的 BACKLOG。读不出是空。
func (a *Service) defaultBacklog(j *state.Job) string {
	def, err := a.jobBranch(j)
	if err != nil {
		return ""
	}
	rel, err := backlog.BacklogRepoPath(backlog.Project{Name: j.Project, Dir: j.ProjectDir})
	if err != nil {
		return ""
	}
	out, err := a.Gitx.Git(j.ProjectDir, "show", "refs/heads/"+def+":"+rel)
	if err != nil {
		return ""
	}
	return out
}

func (a *Service) markSuperseded(j *state.Job, by, merge string) {
	g := j.Gate
	g.State, g.SupersededBy, g.SupersededMerge = state.GateSuperseded, by, merge
	j.StuckPending = "" // 卡住标记还没写上的不用写了
	what := g.SupersededText()
	j.Summary = "门没过，已被" + what + "取代，不用再定：" + textfmt.OneLine(g.Report, 150)
	msg := fmt.Sprintf("门：%s「%s」任务 %s 已被%s取代，不用再定", j.Project, textfmt.TruncRunes(j.Title, 40), j.ID, what)
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	a.Notify.DropPending(decideKey(j.ID))
	a.Ledger.WriteMeta(j)
}
