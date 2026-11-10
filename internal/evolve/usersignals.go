package evolve

import (
	"context"
	"os"
	"os/exec"
	"path"
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
//   - 用户亲手停的任务：任务记录 status=killed（agentq stop 写的；agentq 按信号自动停的记中断，不算）。
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

	retryWaitLong = 6 * time.Hour
	auditTail     = 500
	reworkSeenMax = 500
	gitShowWait   = 10 * time.Second
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

// absorbStop：用户亲手停的任务。按任务种类归（待做、新项目……），停的原因五花八门，不进 Key。
func (s *Service) absorbStop(st *SignalStore, j *state.Job, now time.Time) {
	if j.Status != state.StatusKilled {
		return
	}
	kind := j.Kind
	if kind == "" {
		kind = "待做"
	}
	st.add(now, KindUserStop, j.Project, SignalKey(KindUserStop, kind, ""), "用户停了"+kind+"任务「"+textfmt.TruncRunes(j.Title, 30)+"」："+textfmt.OneLine(j.Summary, 160), jobEvidence(j))
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
	set := map[string]bool{}
	for _, f := range paths {
		if d := pathDomain(f); d != "" {
			set[d] = true
		}
	}
	basis := "按范围和改动文件"
	if len(set) == 0 {
		for d, re := range domainWords {
			if re.MatchString(body) {
				set[d] = true
			}
		}
		basis = "按意见的文字"
	}
	if len(set) == 0 {
		return []string{"未分类"}, "范围、改动文件、文字都看不出"
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, basis
}

// changedFiles：这个提交（合并提交按第一父）改了的文件。不是提交、git 出错都当没有。
func changedFiles(ctx context.Context, dir, commit string) []string {
	ctx, cancel := context.WithTimeout(ctx, gitShowWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", commit+"^", commit)
	cmd.Dir, cmd.Env = dir, proc.ChildEnv()
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// 领域：前端、后端、设计、文档。路径归不了的不算。
const (
	DomainFrontend = "前端"
	DomainBackend  = "后端"
	DomainDesign   = "设计"
	DomainDocs     = "文档"
)

var (
	frontendDirs = []string{"web", "frontend", "ui", "components", "pages", "views", "public", "styles", "site"}
	backendDirs  = []string{"server", "backend", "api", "internal", "cmd", "pkg", "db", "migrations"}
	domainWords  = map[string]*regexp.Regexp{
		DomainDesign:   regexp.MustCompile(`设计|配色|视觉|排版|字体|图标|好看|难看|丑`),
		DomainFrontend: regexp.MustCompile(`界面|页面|前端|按钮|布局|样式|动画|交互|(?i:\bui\b)`),
		DomainBackend:  regexp.MustCompile(`后端|接口|数据库|服务端|(?i:\bapi\b)`),
	}
)

// pathDomain：一个文件或目录（范围写的前缀）归哪个领域，归不了是空。
func pathDomain(p string) string {
	p = strings.ToLower(strings.TrimSuffix(filepath.ToSlash(p), "/"))
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	hasDir := func(names []string) bool {
		return slices.ContainsFunc(parts, func(seg string) bool { return slices.Contains(names, seg) })
	}
	base := parts[len(parts)-1]
	switch path.Ext(base) {
	case ".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".fig", ".sketch", ".icns":
		return DomainDesign
	case ".tsx", ".jsx", ".vue", ".svelte", ".astro", ".css", ".scss", ".sass", ".less", ".html":
		return DomainFrontend
	case ".go", ".rs", ".py", ".java", ".rb", ".php", ".sql", ".proto":
		return DomainBackend
	case ".swift", ".kt":
		if strings.Contains(base, "view") || hasDir([]string{"views", "ui", "screens"}) {
			return DomainFrontend
		}
		return ""
	case ".ts", ".js", ".mjs":
		if hasDir(backendDirs) {
			return DomainBackend
		}
		return DomainFrontend
	case ".md":
		if hasDir([]string{"design"}) || strings.HasPrefix(base, "design") {
			return DomainDesign
		}
		return DomainDocs
	}
	switch { // 范围写的目录
	case hasDir([]string{"design"}):
		return DomainDesign
	case hasDir(frontendDirs):
		return DomainFrontend
	case hasDir(backendDirs):
		return DomainBackend
	case hasDir([]string{"docs"}):
		return DomainDocs
	}
	return ""
}

// absorbRework：一条返工意见，每个领域记一次（E7 按领域看返工）。
func (s *Service) absorbRework(st *SignalStore, r reworkItem, now time.Time) {
	ev := "项目 " + r.project + "「验收 " + textfmt.TruncRunes(r.title, 30) + " 的意见」"
	for _, d := range r.domains {
		summary := d + "返工（" + r.basis + "）：「" + textfmt.TruncRunes(r.title, 30) + "」" + textfmt.OneLine(strings.TrimPrefix(r.text, backlog.ItemTitle(r.text)), 120)
		st.add(now, KindRework, r.project, SignalKey(KindRework, d, ""), summary, ev)
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
		if selfPatrol(is.Check) {
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
