package evolve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 用户和真实使用里冒出来的问题（会话或用户发现、门和任务记录里看不出来的），都只读已经落盘的数据，不调模型：
//   - 用户亲手停的任务（user-stop）；其中亲手派、启动 3 分钟内就被停的单算一类「刚启动就被停」（quick-stop），一次就写：
//     多半是派活的人（常是 Claude、Codex 会话）被说明或输出带偏了，派完才发现不对。证据带上谁停的（命令行、Telegram）和停时的额度、
//     后台上限。停了以后把那一条撤出「待做」（移走、删掉）或接着做完的是改计划，两类都不算；停了以后很快亲手重派是改需求，
//     只是 user-stop 不算，quick-stop 照算（派错条目、改了说法再派，正是要查的说明问题）。怎么判见下面「按任务记录判的信号」一节的 jobCounts。
//   - 门卡片上用户点了退回或放弃：门的审计日志 gate/audit.log 的「退回」「放弃」行（卡片和 gate 命令写的都是这一行）。
//   - 返工：项目 BACKLOG 里出现「验收 <原条目标题> 的意见」（dispatch 的固定写法），按条目范围、原条目范围和提到的合并提交改了的文件归到领域。
//   - 巡逻报过、当天又自己消失的问题（疑似误报）：信号文件记着上次还在的巡逻问题，这次不在了、首次发现是今天，就算。
//   - 同一条目被 retry_after 挡住超过 6 小时：还在「待做」、能挑，最近一次任务结束满 6 小时、还没到 retry_after。
//
// 这几类都是 agentq 自己的问题（selfKind），同一 Key 两次以上照 E2 的规则写提案。
const (
	KindUserStop   = "user-stop"
	KindUserDecide = "user-decide"
	KindRework     = "rework"
	KindPatrolGone = "patrol-gone"
	KindRetryWait  = "retry-wait"
	KindQuickStop  = "quick-stop"

	quickStopWithin = 3 * time.Minute
	redoWithin      = 30 * time.Minute
	retryWaitLong   = 6 * time.Hour
	auditTail       = 500
	reworkSeenMax   = 500
	gitShowWait     = 10 * time.Second
)

// hasEvidence：这一类信号里已经有这份证据。
func (st *SignalStore) hasEvidence(kind, ev string) bool {
	return slices.ContainsFunc(st.Signals, func(sig Signal) bool { return sig.Kind == kind && slices.Contains(sig.Evidence, ev) })
}

func jobEvidence(j *state.Job) string {
	ev := "任务 " + j.ID + "（" + j.Project + "）"
	if j.Title != "" {
		ev += "「" + textfmt.TruncRunes(j.Title, 30) + "」"
	}
	return ev
}

// ---- 按任务记录判的信号：亲手停、刚启动就停、放行 ----
//
// 这三类说的都是用户对一个任务做了什么，算不算 agentq 的问题，要看这个任务（有时还有它之后的任务）的记录。
// 以前每类在第一次读到任务记录时各判一次，判法散在几处；规则后来改了（刚启动就停单算、改了尺子等人放行不算、停了重派不算），
// 按旧规则记下的证据还留着凑次数提案，只有放行一类会重判。现在只有 jobCounts 一处规则：收的时候用它，之后每次收信号
// 用它重判记过的证据（recheckJobSignals），规则再改也不用另写重判。
var jobKinds = []string{KindUserStop, KindQuickStop, KindGateOverride}

// jobCounts：这份任务记录按现在的规则算不算 kind 这一类。
//   - quick-stop：亲手派、启动 3 分钟内（含）被用户停了，停的那一条也还在「待做」里（stopItemGone）：派完马上停又把条目撤走，
//     是用户改了主意（11-20 那次是派完同一秒用户说不做了），不是说明带偏。停了马上重派的照算，见文件头。
//   - user-stop：用户停了（agentq 按信号自动停的记中断，不算），不是刚启动就停的，停了以后 30 分钟内同一个项目也没有亲手再派
//     （改了条目重派、换一条替它是改需求，不是 agentq 的问题），停的那一条也还在「待做」里（stopItemGone）。
//     停了还不到 30 分钟又没重派的看不出来，见 jobUndecided。
//   - gate-override：合并了、是用户放行的，不是门都过了只因为改了尺子等人看（heldOnPurpose）。
func (s *Service) jobCounts(kind string, j *state.Job) bool {
	switch kind {
	case KindQuickStop:
		return j.Status == state.StatusKilled && quickStop(j) && !stopItemGone(j)
	case KindUserStop:
		return j.Status == state.StatusKilled && !quickStop(j) && s.stopRedo(j) == nil && !stopItemGone(j)
	case KindGateOverride:
		g := j.Gate
		return g != nil && g.State == state.GateMerged && (g.Passes > 0 || g.Override != "") && !s.heldOnPurpose(j)
	}
	return false
}

// jobUndecided：用户停的（刚启动就停的不算），停了还不到 30 分钟、也还没重派，看不出是不是改需求：这次先不收这份任务记录。
func (s *Service) jobUndecided(j *state.Job, now time.Time) bool {
	return s.jobCounts(KindUserStop, j) && j.EndedAt != nil && now.Sub(*j.EndedAt) < redoWithin
}

// recheckJobSignals 按 jobCounts 重判记过的按任务记录判的信号：现在不算这一类的证据去掉，一份不剩的信号删掉。
// 放行信号还按 overrideRegrouped 去掉按放行理由凑的证据。
// 任务记录已经清掉、读不了的证据照留：没法重判。只去不加：按旧规则记在别处的证据不挪过来，免得把早处理过的事再提一次。
func (s *Service) recheckJobSignals(st *SignalStore) {
	for i := range st.Signals {
		sig := &st.Signals[i]
		if !slices.Contains(jobKinds, sig.Kind) {
			continue
		}
		n := len(sig.Evidence)
		sig.Evidence = slices.DeleteFunc(sig.Evidence, func(ev string) bool {
			id, ok := evidenceJob(ev)
			if !ok {
				return false
			}
			j := s.readMeta(filepath.Join(s.JobDir(id), "meta.json"))
			return j != nil && (!s.jobCounts(sig.Kind, j) || sig.Kind == KindGateOverride && s.overrideRegrouped(sig.Key, j))
		})
		sig.Seen -= n - len(sig.Evidence)
	}
	st.Signals = slices.DeleteFunc(st.Signals, func(sig Signal) bool {
		return slices.Contains(jobKinds, sig.Kind) && (len(sig.Evidence) == 0 || sig.Seen <= 0)
	})
}

// overrideRegrouped：这条放行证据当时按放行理由归的类，现在认得出门停在哪、归到别处了（overrideStop）。只去不挪，理由同上。
func (s *Service) overrideRegrouped(key string, j *state.Job) bool {
	name, why := s.overrideStop(j)
	return key == SignalKey(KindGateOverride, "", j.Gate.Override) && key != SignalKey(KindGateOverride, name, why)
}

// absorbStop：用户亲手停的任务。按任务种类归（待做、新项目……），停的原因五花八门，不进 Key。
func (s *Service) absorbStop(st *SignalStore, j *state.Job, now time.Time) {
	switch {
	case s.jobCounts(KindQuickStop, j):
		absorbQuickStop(st, j, now)
	case s.jobCounts(KindUserStop, j):
		kind := j.Kind
		if kind == "" {
			kind = "待做"
		}
		st.add(now, KindUserStop, j.Project, SignalKey(KindUserStop, kind, ""), "用户停了"+kind+"任务「"+textfmt.TruncRunes(j.Title, 30)+"」："+textfmt.OneLine(j.Summary, 160), jobEvidence(j))
	}
}

// quickStop：亲手派的任务，启动后 3 分钟内（含）被用户停了。
func quickStop(j *state.Job) bool {
	return j.Manual && j.EndedAt != nil && !j.StartedAt.IsZero() && j.EndedAt.Sub(j.StartedAt) <= quickStopWithin
}

// stopRedo：用户停了这个任务之后 30 分钟内（含），同一个项目亲手派的任务；没有是 nil。后来自动接的不算：那是空出位子 tick 挑的下一条。
func (s *Service) stopRedo(j *state.Job) *state.Job {
	if j.EndedAt == nil {
		return nil
	}
	metas, _ := filepath.Glob(filepath.Join(s.DataDir(), "jobs", "*", "meta.json")) // 模式是固定的，不会报错
	for _, p := range metas {
		n := s.readMeta(p)
		if n != nil && n.ID != j.ID && n.Project == j.Project && n.Manual &&
			n.StartedAt.After(*j.EndedAt) && n.StartedAt.Sub(*j.EndedAt) <= redoWithin {
			return n
		}
	}
	return nil
}

// stopItemGone：用户停的待做任务，那一条在项目 BACKLOG 里待过（git 历史里有这个标题），现在已经不在「待做」里了。
// 停了以后孤儿标记让用户二选一：不做就移走（移到「以后再说」、删掉，是撤回需求），要做就去掉标记接着做（做完合并移到
// 「已完成」，停只是让路）；两种都是用户对计划的决定，不是这个任务做歪了。不是待做任务、没记项目目录、读不了 BACKLOG、
// 历史里找不到这一条（看不出它去哪了）的，当还在。
func stopItemGone(j *state.Job) bool {
	if j.Kind != "" || j.ProjectDir == "" || j.Title == "" {
		return false
	}
	p := backlog.Project{Dir: j.ProjectDir}
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil || slices.ContainsFunc(backlog.ParseBacklog(string(b)), func(it backlog.Item) bool { return backlog.SameItemText(it, j.Fingerprint, j.Title) }) {
		return false
	}
	rel, _ := filepath.Rel(p.Dir, p.BacklogPath()) // 同一个目录下拼出来的，不会报错
	out, ok := gitFields(context.Background(), p.Dir, "log", "-1", "--format=%h", "-S"+j.Title, "--", rel)
	return ok && len(out) > 0
}

func absorbQuickStop(st *SignalStore, j *state.Job, now time.Time) {
	via, usage := j.StopVia, textfmt.OneLine(j.StopUsage, 160)
	if via == "" {
		via = "来源没记"
	}
	if usage == "" {
		usage = "额度没记"
	}
	after := textfmt.FormatIdle(j.EndedAt.Sub(j.StartedAt))
	what := "启动 " + after + "后被停（" + via + "），停时" + usage
	st.add(now, KindQuickStop, j.Project, SignalKey(KindQuickStop, "", ""), "亲手派的「"+textfmt.TruncRunes(j.Title, 30)+"」"+what, jobEvidence(j)+what)
}

// ---- 门卡片上的决定 ----

// gateDecision 是审计日志里用户退回或放弃的一行。
type gateDecision struct {
	at      time.Time
	action  string // 退回、放弃
	job     string
	project string
	title   string
}

// auditRe：gate.audit 写的「退回 <任务> <项目>「<标题>」分支 …」「放弃 <任务> <项目>「<标题>」分支 … 留着」。只认行首的固定写法。
var auditRe = regexp.MustCompile(`^(退回|放弃) (\S+) (\S+?)「(.*)」分支 `)

func (d gateDecision) evidence() string {
	return "任务 " + d.job + "（" + d.project + "）" + d.action + " " + d.at.Format("01-02 15:04")
}

// scanDecisions：审计日志里上次之后（同一秒的再看一遍，靠证据去重）、30 天内的退回和放弃。
func (s *Service) scanDecisions(st *SignalStore, now time.Time, in *scanned) {
	in.auditAfter = st.AuditAfter
	for _, line := range strings.Split(fsx.TailLines(filepath.Join(s.DataDir(), "gate", "audit.log"), auditTail), "\n") {
		at, text, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil || t.Before(st.AuditAfter) || now.Sub(t) > signalKeepDays*24*time.Hour {
			continue
		}
		m := auditRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		d := gateDecision{at: t, action: m[1], job: m[2], project: m[3], title: m[4]}
		if t.After(in.auditAfter) {
			in.auditAfter = t
		}
		if st.hasEvidence(KindUserDecide, d.evidence()) {
			continue
		}
		in.decisions = append(in.decisions, d)
		in.touched = true
	}
}

// absorbDecision：一次退回或放弃。按动作归；卡住的原因从任务记录里取（门写的最后结论）。
func (s *Service) absorbDecision(st *SignalStore, d gateDecision, now time.Time) {
	why := ""
	if j := s.readMeta(filepath.Join(s.JobDir(d.job), "meta.json")); j != nil && j.Gate != nil {
		why = j.Gate.Report
	}
	summary := "用户在门卡片上" + d.action + "了「" + textfmt.TruncRunes(d.title, 30) + "」"
	if why != "" {
		summary += "，门的结论：" + textfmt.OneLine(why, 160)
	}
	st.add(now, KindUserDecide, d.project, SignalKey(KindUserDecide, d.action, ""), summary, d.evidence())
}

// ---- 返工 ----

// reworkItem 是 BACKLOG 里一条「验收 <标题> 的意见」。
type reworkItem struct {
	project string
	key     string // reworkKey，记进 ReworkSeen
	title   string // 原条目标题
	text    string
	domains []string
	basis   string // 按什么归的领域
}

// reworkRe：dispatch 写的「验收 <原条目标题> 的意见」，只认标题开头的固定写法，正文顺带提到不算。
var (
	reworkRe  = regexp.MustCompile(`^验收\s+(.+?)\s*的意见`)
	mentionRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

const reworkMax = 8 // 一条意见最多看几个提交

// scanRework：各项目 BACKLOG（含已完成）里没见过的返工意见。被排除的项目也看：亲手派的活照样会返工。
func (s *Service) scanRework(ctx context.Context, st *SignalStore, in *scanned) {
	projects, _ := s.Backlog.AllProjects() // 扫描不全时看得到几个算几个
	for _, p := range projects {
		b, err := os.ReadFile(p.BacklogPath())
		if err != nil {
			continue
		}
		text := string(b)
		backlog.EachBacklogLine(text, func(_, line string) {
			body, ok := checkboxBody(line)
			if !ok {
				return
			}
			m := reworkRe.FindStringSubmatch(backlog.ItemTitle(body))
			if m == nil {
				return
			}
			orig := strings.Trim(strings.TrimSpace(m[1]), "「」“”\"`")
			key := reworkKey(p.Name, body)
			if slices.Contains(st.ReworkSeen, key) || slices.ContainsFunc(in.rework, func(r reworkItem) bool { return r.key == key }) {
				return
			}
			r := reworkItem{project: p.Name, key: key, title: orig, text: body}
			r.domains, r.basis = s.reworkDomains(ctx, p, text, orig, body)
			in.rework = append(in.rework, r)
			in.touched = true
		})
	}
}

// reworkKey：一条意见的键。去掉进行中、卡住标记和合并后补的「合并提交 …」，认领、合并移到「已完成」都还是同一条；
// 同一条目第二轮的意见写的内容不一样，是另一条。
func reworkKey(project, body string) string {
	b := backlog.InProgressRe.ReplaceAllString(body, "")
	for _, cut := range []string{"合并提交 ", "（卡住"} {
		if i := strings.Index(b, cut); i >= 0 {
			b = b[:i]
		}
	}
	return project + "|" + textfmt.TruncRunes(backlog.TitleKey(b), 80)
}

// checkboxBody：「- [ ] 」「- [x] 」后面的原文。
func checkboxBody(line string) (string, bool) {
	for _, pre := range []string{"- [ ]", "- [x]", "- [X]"} {
		if strings.HasPrefix(line, pre) {
			return strings.TrimSpace(line[len(pre):]), true
		}
	}
	return "", false
}

// reworkDomains：按路径归领域——意见和原条目写的（范围：…），意见里提到的提交、原条目「已完成」里记的合并提交改了的文件。
// 一个路径都归不了时才看意见的文字。都没有是「未分类」。
func (s *Service) reworkDomains(ctx context.Context, p backlog.Project, backlogText, orig, body string) ([]string, string) {
	paths := backlog.ItemScope(body)
	backlog.EachBacklogLine(backlogText, func(_, line string) {
		if t, ok := checkboxBody(line); ok && backlog.ItemTitle(t) == orig {
			paths = append(paths, backlog.ItemScope(t)...)
		}
	})
	commits := mentionRe.FindAllString(body, reworkMax)
	if c := backlog.DoneMergeCommit(backlogText, orig); c != "" && !slices.Contains(commits, c) {
		commits = append(commits, c)
	}
	for _, c := range commits {
		paths = append(paths, changedFiles(ctx, p.Dir, c)...)
	}
	if out := filesDomains(paths); len(out) > 0 {
		return out, "按范围和改动文件"
	}
	if out := wordDomains(body); len(out) > 0 {
		return out, "按意见的文字"
	}
	return []string{domainUnknown}, "范围、改动文件、文字都看不出"
}

// changedFiles：这个提交（合并提交按第一父）改了的文件。不是提交、git 出错都当没有。
func changedFiles(ctx context.Context, dir, commit string) []string {
	out, _ := gitFields(ctx, dir, "diff", "--name-only", commit+"^", commit)
	return out
}

// gitFields：在 dir 里跑 git，输出按空白切开；出错、超时 ok=false。
func gitFields(ctx context.Context, dir string, args ...string) (out []string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, gitShowWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, proc.ChildEnv()
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	b, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return strings.Fields(string(b)), true
}

// absorbRework：一条返工意见，每个领域记一次（E7 按领域看返工）。
func (s *Service) absorbRework(st *SignalStore, r reworkItem, now time.Time) {
	ev := "项目 " + r.project + "「验收 " + textfmt.TruncRunes(r.title, 30) + " 的意见」"
	for _, d := range r.domains {
		summary := d + "返工（" + r.basis + "）：「" + textfmt.TruncRunes(r.title, 30) + "」" + textfmt.OneLine(strings.TrimPrefix(r.text, backlog.ItemTitle(r.text)), 120)
		st.add(now, KindRework, r.project, SignalKey(KindRework, d, ""), summary, ev)
		if d != domainUnknown {
			st.addDomain(now, []string{d}, evRework, r.key, ev)
		}
	}
	if !slices.Contains(st.ReworkSeen, r.key) {
		st.ReworkSeen = append(st.ReworkSeen, r.key)
	}
	if len(st.ReworkSeen) > reworkSeenMax {
		st.ReworkSeen = st.ReworkSeen[len(st.ReworkSeen)-reworkSeenMax:]
	}
}

// ---- 巡逻：报了又自己消失 ----

// scanPatrolGone：issues 是这次读到的巡逻问题。和上次记着还在的比：不在了、首次发现和现在是同一天的，算疑似误报。
// 记着的集合变了（新出现、消失）才算有新数据。
func (s *Service) scanPatrolGone(st *SignalStore, issues []patrolIssue, now time.Time, in *scanned) {
	var open []patrolIssue
	for _, is := range issues {
		if selfPatrol(is) {
			open = append(open, is)
		}
	}
	in.patrolOpen = open
	fps := func(list []patrolIssue) []string {
		out := make([]string, 0, len(list))
		for _, is := range list {
			out = append(out, is.Fingerprint)
		}
		sort.Strings(out)
		return out
	}
	if !slices.Equal(fps(open), fps(st.PatrolOpen)) {
		in.touched = true
	}
	y, m, d := now.Date()
	for _, old := range st.PatrolOpen {
		if slices.ContainsFunc(open, func(is patrolIssue) bool { return is.Fingerprint == old.Fingerprint }) {
			continue
		}
		fy, fm, fd := old.FirstSeen.In(now.Location()).Date()
		if fy == y && fm == m && fd == d {
			in.patrolGone = append(in.patrolGone, old)
		}
	}
}

func (s *Service) absorbPatrolGone(st *SignalStore, is patrolIssue, now time.Time) {
	st.add(now, KindPatrolGone, "", SignalKey(KindPatrolGone, is.Check, is.Title),
		"巡逻报了又自己消失（疑似误报）："+textfmt.OneLine(is.Title, 200),
		"巡逻 "+is.Fingerprint+" "+is.FirstSeen.In(now.Location()).Format("01-02 15:04")+" 报，"+now.Format("15:04")+" 前消失")
}

// ---- retry_after 挡太久 ----

// retryWait 是一条被 retry_after 挡住满 6 小时的条目。
type retryWait struct {
	project string
	title   string
	last    *state.Job
	at      time.Time // 到这个时间才会再自动接
}

func (w retryWait) evidence() string {
	return "项目 " + w.project + "「" + textfmt.TruncRunes(w.title, 30) + "」任务 " + w.last.ID + " 之后"
}

// scanRetryWait：自动流程会挑的项目里，还在「待做」能挑、没在跑，最近一次任务结束满 6 小时却还在 retry_after 里的条目。
// 暂停时 tick 不自动接，挡住它的不是 retry_after，不看。读不了状态文件就不看。
func (s *Service) scanRetryWait(st *SignalStore, now time.Time, in *scanned) {
	if s.Cfg.Paused {
		return
	}
	sst, err := state.Load(s.StatePath())
	if err != nil {
		return
	}
	projects, _ := s.Backlog.ActiveProjects()
	for _, p := range projects {
		items, err := p.Items()
		if err != nil {
			continue
		}
		for _, it := range items {
			if !it.Eligible() || sst.RunningFor(it.Project, it.Fingerprint) != nil {
				continue
			}
			at, last := state.RetryAt(sst, it.Project, it.Fingerprint, s.Cfg.RetryAfter.Duration)
			if last == nil || last.EndedAt == nil || !now.Before(at) || now.Sub(*last.EndedAt) < retryWaitLong {
				continue
			}
			w := retryWait{project: p.Name, title: it.Title, last: last, at: at}
			if st.hasEvidence(KindRetryWait, w.evidence()) {
				continue
			}
			in.retry = append(in.retry, w)
			in.touched = true
		}
	}
}

// absorbRetryWait：按上次任务怎么结束的归（用户停的、失败的……）。
func (s *Service) absorbRetryWait(st *SignalStore, w retryWait, now time.Time) {
	label := state.StatusLabel(w.last.Status)
	summary := "「" + textfmt.TruncRunes(w.title, 30) + "」上次" + label + "（" + textfmt.WhenCN(w.last.EndedAt.In(now.Location())) +
		"），retry_after 挡到 " + textfmt.WhenCN(w.at.In(now.Location())) + "，已经等了 " + textfmt.FormatAge(now.Sub(*w.last.EndedAt))
	st.add(now, KindRetryWait, w.project, SignalKey(KindRetryWait, w.last.Status, ""), summary, w.evidence())
}
