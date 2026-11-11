package evolve

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 自己长大（self_grow_enabled，默认开；配了 self_project 才起作用）：
//   - agentq 自己的问题够次数（漏洞一次就够）直接写进 self_project 的「待做」，不进「建议」等人回「做 N」；
//     项目在 exclude_projects 里也写（self_project 是用户点名的），任务由 sched 当成亲手派的起（SelfItemsToStart、MarkSelfItem）。
//   - 这一条合并了，记一次修好（Fixes），同一类信号清零：再出现是回归，重新写一条；修过两次还出现，写成重写这一块。
//   - 升级和回滚见 upgrade.go。关掉开关和以前一样：写「建议」，不升级。
const (
	selfItemKeepDays = 30
	rewriteAfter     = 2 // 同一类修过这么多次还出现：重写，不再打补丁
)

// SelfItem 是写进 self_project「待做」的一条，等 sched 起任务、等它合并。
type SelfItem struct {
	Key     string    `json:"key"`
	Project string    `json:"project"`
	Title   string    `json:"title"`
	At      time.Time `json:"at"`
	// Job：起了的任务；Note：起不来的原因，之后不再自动起（用户可以 agentq run）。
	Job  string `json:"job,omitempty"`
	Note string `json:"note,omitempty"`
}

// SelfGrowOn：开关开着、配了 self_project。
func (s *Service) SelfGrowOn() bool { return s.Cfg.SelfGrowEnabled && s.Cfg.SelfProject != "" }

// selfProjectAny 找 self_project，被排除的也算：它是用户点名的 agentq 源码。没配或找不到返回 false。
func (s *Service) selfProjectAny() (backlog.Project, bool) {
	if s.Cfg.SelfProject == "" {
		return backlog.Project{}, false
	}
	projects, _ := s.Backlog.AllProjects() // 扫描不全时只要找得到它就行
	for _, p := range projects {
		if p.Name == s.Cfg.SelfProject {
			return p, true
		}
	}
	return backlog.Project{}, false
}

// todoProposal 是写进「待做」的那一行：修过的再出现标次数（标题不同，「已完成」里的旧条目不挡它），修过两次的写成重写。
func todoProposal(sig *Signal, fixes []string) proposal {
	if p, ok := capProposal(sig, fixes); ok {
		return p
	}
	what := kindWords[sig.Kind] + "反复出现"
	if oneShot(sig.Kind) {
		what = kindWords[sig.Kind]
	}
	head := "agentq 自身："
	switch n := len(fixes); {
	case n >= rewriteAfter && !oneShot(sig.Kind):
		head = fmt.Sprintf("agentq 自身（重写，修过 %d 次又出现）：", n)
	case n > 0:
		head = fmt.Sprintf("agentq 自身（修过 %d 次又出现）：", n)
	}
	title := backlog.SuggestField(head+what+"："+textfmt.OneLine(sig.Summary, 30), proposalTitleMax+12)
	ev := sig.Evidence
	if len(ev) > proposalEvidMax {
		ev = ev[len(ev)-proposalEvidMax:]
	}
	body := fmt.Sprintf("同类信号 %d 次，%s 到 %s：%s；证据：%s；自己长大写进待做，自动接着做",
		sig.Seen, sig.First.Format("01-02"), sig.Last.Format("01-02"), textfmt.OneLine(sig.Summary, 200), strings.Join(ev, "、"))
	switch {
	case sig.Kind == KindVuln:
		body += "：按 govulncheck 的报告把 Go 版本或依赖升到修好的版本，跑完整检查"
	case sig.Kind == KindRollback:
		body += "：对照升级前后的任务记录找出这次升级带进来的改动里哪处让任务变差，在根因处修，加一道能复现它的评测题（evals/ 新目录，只加不改）；这一条合并之前，带着那次改动的版本不会再自动升级"
	case len(fixes) >= rewriteAfter:
		body += fmt.Sprintf("：这一类已经修过 %d 次（合并提交 %s）还出现，按模块化规则重写这一块：先写清为什么老出错，用测试固定要保留的行为再重写，ready.json 的 type 写 rewrite；交付时加一道能复现它的评测题（evals/ 新目录，只加不改）",
			len(fixes), strings.Join(fixes, "、"))
	default:
		body += "：在根因处修，交付时加一道能复现它的评测题（evals/ 新目录，只加不改）"
	}
	return proposal{sig: sig, title: title, line: title + " —— " + backlog.SuggestField(body, proposalLineMax+200)}
}

// proposeToTodo 把自身信号写进 self_project 的「待做」并提交。写成了（或在不做清单里、已经有同标题的条目）返回这些，写不进返回空切片（下次再试）。
// 调用方拿着 evolve.lock。
func (s *Service) proposeToTodo(p backlog.Project, sigs []*Signal, st *SignalStore, now time.Time) []proposal {
	var props []proposal
	for _, sig := range sigs {
		props = append(props, todoProposal(sig, st.Fixes[sig.Key]))
	}
	var done, settled []proposal
	err := s.Backlog.CommitBacklogNamed(p, "待办：自己长大，agentq 自己的问题进待做", func(old string) (string, error) {
		done, settled = nil, nil
		have := backlog.BacklogTitleKeys(old)
		rejected := s.Suggest.NotDoingKeys(p)
		text := old
		var lines []string
		for _, pr := range props {
			k := backlog.TitleKey(pr.title)
			switch {
			case rejected[k]:
				s.Logf("自我进化：「%s」在不做清单里，不写", pr.title)
				settled = append(settled, pr)
				continue
			case have[k]:
				settled = append(settled, pr)
				continue
			}
			have[k] = true
			line := "- [ ] " + pr.line
			next, ok := backlog.InsertTodo(text, line)
			if !ok {
				return "", fmt.Errorf("BACKLOG 里没有「## 待做」一节，或它的末尾卡在没关的注释里")
			}
			text = next
			lines = append(lines, line)
			done = append(done, pr)
		}
		if len(lines) == 0 {
			return old, nil
		}
		if err := backlog.VerifyTodoPresent(text, lines); err != nil {
			return "", err
		}
		return text, nil
	})
	if err != nil {
		s.Logf("自我进化：%d 条没写进 %s 的待做（%v），下次再写", len(props), p.Name, err)
		return []proposal{}
	}
	if len(done) == 0 {
		return settled
	}
	n := notify.Notice{Cat: "done", Key: "evolve-todo:" + now.Format("20060102-1504"), Text: fmt.Sprintf("自己长大：agentq 自己的 %d 个问题写进 %s 的待做，接着自动做", len(done), p.Name)}
	for _, pr := range done {
		st.SelfItems = append(st.SelfItems, SelfItem{Key: pr.sig.Key, Project: p.Name, Title: pr.title, At: now})
		s.Ledger.AppendNightlyLine(now, "自己长大：写进 "+p.Name+" 的待做："+pr.title)
		n.Lines = append(n.Lines, textfmt.TruncRunes(pr.title, 60))
	}
	s.Notify.Notify(n)
	return append(done, settled...)
}

// fixedSelfItem：这次合并的任务做的是自己长大的条目，就记一次修好、清掉这一类的信号（再出现是回归，重新算次数）。
func (st *SignalStore) fixedSelfItem(j *state.Job, merge string) {
	i := slices.IndexFunc(st.SelfItems, func(it SelfItem) bool { return it.Project == j.Project && it.Title == j.Title })
	if i < 0 {
		return
	}
	key := st.SelfItems[i].Key
	st.SelfItems = slices.Delete(st.SelfItems, i, i+1)
	if st.Fixes == nil {
		st.Fixes = map[string][]string{}
	}
	if merge == "" {
		merge = "任务 " + j.ID
	}
	st.Fixes[key] = append(st.Fixes[key], short(merge))
	st.Signals = slices.DeleteFunc(st.Signals, func(sig Signal) bool { return sig.Key == key })
	st.ProposedKey = slices.DeleteFunc(st.ProposedKey, func(k string) bool { return k == key })
}

// pruneSelfItems：30 天还没合并的不再跟（条目在待做里照旧，用户可以 agentq run）。
func (st *SignalStore) pruneSelfItems(now time.Time) {
	st.SelfItems = slices.DeleteFunc(st.SelfItems, func(it SelfItem) bool { return now.Sub(it.At) > selfItemKeepDays*24*time.Hour })
}

// SelfItemsToStart 是还没起过任务的自己长大条目（sched 起任务前调用，只读）。开关关着、读不了是空的。
func (s *Service) SelfItemsToStart(st *state.State) []SelfItem {
	if !s.SelfGrowOn() {
		return nil
	}
	ss, err := s.LoadSignals()
	if err != nil {
		return nil
	}
	var out []SelfItem
	for _, it := range ss.SelfItems {
		if it.Job != "" || it.Note != "" {
			continue
		}
		if slices.ContainsFunc(st.Jobs, func(j *state.Job) bool { return j.Project == it.Project && j.Title == it.Title }) {
			continue // 已经有任务做过（用户亲手 run 了）
		}
		out = append(out, it)
	}
	return out
}

// MarkSelfItem 记下这一条起了的任务（job），或起不来的原因（note，之后不再自动起）。
func (s *Service) MarkSelfItem(it SelfItem, job, note string) {
	lock, err := fsx.AcquireLock(s.evolveLockPath(), evolveLockWait)
	if err != nil || lock == nil {
		s.Logf("自我进化：拿不到 %s，「%s」的任务号没记下：%v", s.evolveLockPath(), it.Title, err)
		return
	}
	defer lock.Close()
	st, err := s.LoadSignals()
	if err != nil {
		s.Logf("自我进化：%v", err)
		return
	}
	i := slices.IndexFunc(st.SelfItems, func(x SelfItem) bool { return x.Project == it.Project && x.Title == it.Title })
	if i < 0 {
		return
	}
	st.SelfItems[i].Job, st.SelfItems[i].Note = job, note
	if err := s.saveSignals(st); err != nil {
		s.Logf("自我进化：%v", err)
	}
}

// RollbackKey 是回滚过的这个提交的信号 Key：修它的那一条合并后记在 Fixes 里，带着这个提交的版本才能再升（upgrade.go）。
func RollbackKey(commit string) string { return KindRollback + "|" + commit }

// reportRegression：升级到 commit 后任务变差、已回滚，记一个信号，马上写进待做（和漏洞一样一次就写）。
func (s *Service) reportRegression(commit, version, why string, now time.Time) {
	if err := os.MkdirAll(s.evolveDir(), 0o700); err != nil {
		s.Logf("自己长大：%v", err)
		return
	}
	lock, err := fsx.AcquireLock(s.evolveLockPath(), evolveLockWait)
	if err != nil || lock == nil {
		s.Logf("自己长大：拿不到 %s，回滚的待做没写：%v", s.evolveLockPath(), err)
		return
	}
	defer lock.Close()
	st, err := s.LoadSignals()
	if err != nil {
		s.Logf("自己长大：回滚的待做没写：%v", err)
		return
	}
	st.add(now, KindRollback, "", RollbackKey(commit), "升级到 "+version+"（提交 "+short(commit)+"）后任务变差，已回滚："+why, "升级记录 "+now.Format("01-02 15:04"))
	s.propose(st, now)
	if err := s.saveSignals(st); err != nil {
		s.Logf("自己长大：%v", err)
	}
}
