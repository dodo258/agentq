package evolve

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 自我进化 E7：按领域看问题，发现能力缺口；能力长出来以后看它有没有帮上忙，没用的写条目撤掉。都只读已经落盘的数据，不调模型。
//   - 领域：一张表（domainTable）对上中文名（信号里用）和英文名（评测题、能力清单里用）。
//   - 按领域记：每条任务（按条目算一次）、返工意见、门 error 档没过（门点名的文件 → 退回范围 → 条目）、超时、卡在门上。
//   - 缺口：某领域近 30 天的问题（返工、门没过、超时卡住）够 gapMin 次，问题率不低于 gapRate，且至少是其他领域的 gapTimes 倍，
//     写一条「能力缺口：<领域>」，走自我派活（selfgrow.go、proposal.go），同一领域只写一次；这一条合并后这个领域的记录清零。
//   - 能力：能力门（gate/capability.go）每次记下用上了哪些能力、检查报没报问题。检查报了问题、同一条之后修好了，
//     或用上这项能力的任务过门合并了，算帮上忙；报了问题却照样合并或被放弃，算放过（误报嫌疑）。
//   - 撤掉：一项能力 capIdleDays 天没帮上忙（从加进来那天算），或检查报出 capFlagMin 次以上、一半以上被放过，
//     写一条「撤掉能力：<名字>」，不自动删文件。
const (
	gapMin   = 3
	gapRate  = 0.25
	gapTimes = 2.0

	capIdleDays = 60
	capFlagMin  = 4

	KindGap    = "capability-gap"
	KindRetire = "capability-retire"

	gapTitlePrefix    = "能力缺口："
	retireTitlePrefix = "撤掉能力："

	evTask     = "task"
	evRework   = "rework"
	evGateFail = "gate-fail"
	evTimeout  = "timeout"
	evStuck    = "stuck"
)

// domainTable：中文名是信号和条目里用的，英文名是评测题 case.json 和能力清单的 domain。前四个由文件路径归（pathDomain）。
var domainTable = []struct{ en, cn string }{
	{"frontend", DomainFrontend},
	{"backend", DomainBackend},
	{"design", DomainDesign},
	{"docs", DomainDocs},
	{"performance", "性能"},
	{"security", "安全"},
	{"data", "数据"},
	{"product", "产品"},
	{"process", "流程"},
	{"install", "安装"},
	{"engine", "引擎"},
	{"routine", "例行"},
}

// DomainEN 是中文领域名对应的英文名，不认识的原样返回。
func DomainEN(cn string) string {
	for _, d := range domainTable {
		if d.cn == cn {
			return d.en
		}
	}
	return cn
}

// DomainCN 是英文领域名对应的中文名，不认识的原样返回。
func DomainCN(en string) string {
	for _, d := range domainTable {
		if d.en == en {
			return d.cn
		}
	}
	return en
}

// DomainLabel：「前端（frontend）」，中文英文都认。
func DomainLabel(name string) string {
	cn, en := DomainCN(name), DomainEN(name)
	if cn == en {
		return name
	}
	return cn + "（" + en + "）"
}

// DomainEvent 是按领域记的一件事。Ref 是条目（项目|指纹）或返工意见的键，同一领域同一种同一 Ref 只记一次。
type DomainEvent struct {
	At       time.Time `json:"at"`
	Domain   string    `json:"domain"`
	Kind     string    `json:"kind"`
	Ref      string    `json:"ref"`
	Evidence string    `json:"evidence,omitempty"`
}

// CapUse 是一项能力用得怎么样。
type CapUse struct {
	LastHelp time.Time `json:"last_help,omitzero"`
	Help     string    `json:"help,omitempty"` // 最近一次帮上忙的证据
	Flags    int       `json:"flags,omitempty"`
	Ignored  int       `json:"ignored,omitempty"`
	Open     []capOpen `json:"open,omitempty"`
}

// capOpen 是用上了这项能力、还没合并或放弃的条目。Flagged：检查报的问题还没修。
type capOpen struct {
	Project     string    `json:"project"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Title       string    `json:"title,omitempty"`
	Job         string    `json:"job"`
	Flagged     bool      `json:"flagged,omitempty"`
	At          time.Time `json:"at"`
}

// capList 是运行中这一代登记的能力。测试换掉它。
var capList = gate.Capabilities

func jobRef(j *state.Job) string {
	if j.Fingerprint != "" {
		return j.Project + "|" + j.Fingerprint
	}
	return j.Project + "|" + j.ID
}

// filesDomains：这些文件和目录归到的领域，排好序，归不了的不算。
func filesDomains(paths []string) []string {
	var out []string
	for _, f := range paths {
		if d := pathDomain(f); d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// wordDomains：路径归不了时看文字。
func wordDomains(text string) []string {
	var out []string
	for d, re := range domainWords {
		if re.MatchString(text) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func (st *SignalStore) addDomain(at time.Time, domains []string, kind, ref, ev string) {
	for _, d := range domains {
		if slices.ContainsFunc(st.Domains, func(e DomainEvent) bool { return e.Domain == d && e.Kind == kind && e.Ref == ref }) {
			continue
		}
		st.Domains = append(st.Domains, DomainEvent{At: at, Domain: d, Kind: kind, Ref: ref, Evidence: ev})
	}
}

// jobDomains：任务归哪些领域。记过就用记下的；没记过按条目写的范围和合并提交改的文件，都没有再看条目文字。
func (s *Service) jobDomains(ctx context.Context, st *SignalStore, j *state.Job) []string {
	ref := jobRef(j)
	var known []string
	for _, e := range st.Domains {
		if e.Kind == evTask && e.Ref == ref && !slices.Contains(known, e.Domain) {
			known = append(known, e.Domain)
		}
	}
	if len(known) > 0 {
		sort.Strings(known)
		return known
	}
	paths := backlog.ItemScope(j.Item)
	if j.Gate != nil && j.Gate.Merge != "" && j.ProjectDir != "" {
		paths = append(paths, changedFiles(ctx, j.ProjectDir, j.Gate.Merge)...)
	}
	if ds := filesDomains(paths); len(ds) > 0 {
		return ds
	}
	return wordDomains(j.Title + " " + j.Item)
}

// absorbGateDomains：一份门结果。error 档没过的按门点名的文件归领域（没点名用退回范围，再没有用条目的领域）；能力门的结果记到能力上。
func (s *Service) absorbGateDomains(ctx context.Context, st *SignalStore, g gateObs) {
	j := g.job
	for _, r := range g.res.Results {
		if r.Defer != "" || r.Cached {
			continue
		}
		if len(r.Caps) > 0 {
			st.capRuns(j, r.Caps, g.at)
		}
		if g.res.Outcome != "fail" || r.Outcome != gate.Fail || r.Level != gate.LevelError {
			continue
		}
		files := r.Files
		if len(files) == 0 {
			files = g.res.Scope
		}
		ds := filesDomains(files)
		if len(ds) == 0 {
			ds = s.jobDomains(ctx, st, j)
		}
		st.addDomain(g.at, ds, evGateFail, jobRef(j), "任务 "+j.ID+"（"+j.Project+"）"+gateWord(r.Gate)+"没过："+textfmt.OneLine(r.Summary, 80))
	}
}

// absorbJobDomains：一份任务记录。合并或放弃了就结一次能力的账；合并的是能力缺口条目就清零那个领域；
// 结束了的待做按条目记一次任务，超时、卡在门上的记成问题。
func (s *Service) absorbJobDomains(ctx context.Context, st *SignalStore, j *state.Job, now time.Time) {
	if j.Kind != "" {
		return
	}
	at := now
	if j.EndedAt != nil {
		at = *j.EndedAt
	}
	if g := j.Gate; g != nil {
		switch g.State {
		case state.GateMerged:
			st.settleCaps(j, true, at)
			st.fixGap(j, now)
		case state.GateAbandoned:
			st.settleCaps(j, false, at)
		}
	}
	if j.EndedAt == nil || strings.HasPrefix(j.Title, gapTitlePrefix) || strings.HasPrefix(j.Title, retireTitlePrefix) {
		return // 长能力、撤能力是 agentq 自己的活，不算进它要补的那个领域
	}
	ds := s.jobDomains(ctx, st, j)
	if len(ds) == 0 {
		return
	}
	ref, ev := jobRef(j), jobEvidence(j)
	st.addDomain(at, ds, evTask, ref, ev)
	if j.Status == state.StatusTimeout {
		st.addDomain(at, ds, evTimeout, ref, ev+"超时")
	}
	if j.Gate != nil && j.Gate.State == state.GateStuck {
		st.addDomain(at, ds, evStuck, ref, ev+"卡在门上："+textfmt.OneLine(j.Gate.Report, 80))
	}
}

// DomainStat 是一个领域近 30 天的账。
type DomainStat struct {
	Domain   string
	Tasks    int
	Rework   int
	GateFail int
	Stuck    int // 超时、卡在门上的条目
	Bad      int // 返工次数加出过问题的条目数
	Units    int // 任务加返工
	Rate     float64
	Others   float64 // 其他领域合起来的问题率
	Gap      bool
	Evidence []string
}

// domainStats 按领域算账、判断缺口。「未分类」不算。
func domainStats(events []DomainEvent, now time.Time) []DomainStat {
	by := map[string]*DomainStat{}
	badRefs := map[string]map[string]bool{}
	for _, e := range events {
		if now.Sub(e.At) > signalKeepDays*24*time.Hour {
			continue
		}
		d := by[e.Domain]
		if d == nil {
			d = &DomainStat{Domain: e.Domain}
			by[e.Domain] = d
			badRefs[e.Domain] = map[string]bool{}
		}
		switch e.Kind {
		case evTask:
			d.Tasks++
			continue
		case evRework:
			d.Rework++
		case evGateFail:
			d.GateFail++
		case evTimeout, evStuck:
			d.Stuck++
		}
		if e.Kind != evRework {
			if badRefs[e.Domain][e.Ref] {
				continue
			}
			badRefs[e.Domain][e.Ref] = true
		}
		d.Bad++
		d.Evidence = append(d.Evidence, e.Evidence)
	}
	var out []DomainStat
	allBad, allUnits := 0, 0
	for _, d := range by {
		d.Units = max(d.Tasks+d.Rework, d.Bad)
		if d.Units > 0 {
			d.Rate = float64(d.Bad) / float64(d.Units)
		}
		allBad += d.Bad
		allUnits += d.Units
	}
	for _, d := range by {
		if ou := allUnits - d.Units; ou > 0 {
			d.Others = float64(allBad-d.Bad) / float64(ou)
		}
		d.Gap = d.Bad >= gapMin && d.Rate >= gapRate && d.Rate >= gapTimes*d.Others
		out = append(out, *d)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Rate != out[b].Rate {
			return out[a].Rate > out[b].Rate
		}
		return out[a].Domain < out[b].Domain
	})
	return out
}

func gapKey(domain string) string  { return KindGap + "|" + domain }
func retireKey(name string) string { return KindRetire + "|" + name }

func (d DomainStat) summary() string {
	return fmt.Sprintf("%s近 30 天问题 %d 次（返工 %d、门没过 %d、超时或卡在门上 %d），占这个领域 %d 次任务和返工的 %.0f%%，其他领域 %.0f%%",
		DomainLabel(d.Domain), d.Bad, d.Rework, d.GateFail, d.Stuck, d.Units, d.Rate*100, d.Others*100)
}

// detectGaps：够得上缺口的领域记成信号（一次就写，propose 写条目）；已经记着的刷新证据。
func (st *SignalStore) detectGaps(now time.Time) {
	for _, d := range domainStats(st.Domains, now) {
		if !d.Gap {
			continue
		}
		key := gapKey(d.Domain)
		ev := d.Evidence
		if len(ev) > evidenceMax {
			ev = ev[len(ev)-evidenceMax:]
		}
		i := slices.IndexFunc(st.Signals, func(sig Signal) bool { return sig.Key == key })
		if i < 0 {
			st.Signals = append(st.Signals, Signal{Kind: KindGap, Key: key, First: now, Proposed: slices.Contains(st.ProposedKey, key)})
			i = len(st.Signals) - 1
		}
		sig := &st.Signals[i]
		sig.Summary, sig.Evidence, sig.Seen, sig.Last = d.summary(), slices.Clone(ev), d.Bad, now
	}
}

// fixGap：合并的是「能力缺口：<领域>」条目，这个领域到这一条结束为止的记录清零，信号和提过案的记号去掉：再出现是新的缺口。
// 自己长大写的条目 fixedSelfItem 已经去掉信号、记过修好，这里再做一遍也一样。
func (st *SignalStore) fixGap(j *state.Job, now time.Time) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(j.Title), gapTitlePrefix)
	if !ok {
		return
	}
	domain := ""
	for _, d := range domainTable {
		if strings.HasPrefix(rest, d.cn) {
			domain = d.cn
			break
		}
	}
	if domain == "" {
		return
	}
	key, merge := gapKey(domain), short(j.Gate.Merge)
	if merge == "" {
		merge = "任务 " + j.ID
	}
	cut := now // 只清到这一条结束为止：合并后收尾再写 meta.json、再读一次，也不会清掉之后新记的
	if j.EndedAt != nil {
		cut = *j.EndedAt
	}
	st.Domains = slices.DeleteFunc(st.Domains, func(e DomainEvent) bool { return e.Domain == domain && !e.At.After(cut) })
	if slices.Contains(st.Fixes[key], merge) {
		return // 记过这次修好（自己长大的条目、或者合并后又读了一次）：之后新出的缺口信号不能被这次合并去掉
	}
	st.Signals = slices.DeleteFunc(st.Signals, func(sig Signal) bool { return sig.Key == key })
	st.ProposedKey = slices.DeleteFunc(st.ProposedKey, func(k string) bool { return k == key })
	if st.Fixes == nil {
		st.Fixes = map[string][]string{}
	}
	st.Fixes[key] = append(st.Fixes[key], merge)
}

// ---- 能力用得怎么样 ----

func (st *SignalStore) capUse(name string) *CapUse {
	if st.Caps == nil {
		st.Caps = map[string]*CapUse{}
	}
	u := st.Caps[name]
	if u == nil {
		u = &CapUse{}
		st.Caps[name] = u
	}
	return u
}

func (o capOpen) same(j *state.Job) bool {
	return o.Project == j.Project && (o.Fingerprint != "" && o.Fingerprint == j.Fingerprint || o.Title != "" && o.Title == j.Title)
}

func (u *CapUse) helped(at time.Time, ev string) {
	if at.After(u.LastHelp) {
		u.LastHelp, u.Help = at, ev
	}
}

// capRuns：能力门一次的结果。检查报了问题记一次；同一条之前报过、这次过了，算帮上忙（问题被修了）。
func (st *SignalStore) capRuns(j *state.Job, runs []gate.CapRun, at time.Time) {
	for _, r := range runs {
		u := st.capUse(r.Name)
		i := slices.IndexFunc(u.Open, func(o capOpen) bool { return o.same(j) })
		if i < 0 {
			u.Open = append(u.Open, capOpen{Project: j.Project, Fingerprint: j.Fingerprint, Title: j.Title, Job: j.ID, At: at})
			i = len(u.Open) - 1
		}
		o := &u.Open[i]
		o.Job = j.ID
		switch r.Outcome {
		case gate.Fail:
			if !o.Flagged {
				o.Flagged = true
				u.Flags++
			}
		case gate.Pass:
			if o.Flagged {
				o.Flagged = false
				u.helped(at, "任务 "+j.ID+"（"+j.Project+"）：检查报的问题之后修好了")
			}
		}
	}
}

// settleCaps：这一条合并了或被放弃了。检查报的问题还没修就过去了算放过；用上这项能力的任务过门合并了算帮上忙。
func (st *SignalStore) settleCaps(j *state.Job, merged bool, at time.Time) {
	for _, u := range st.Caps {
		u.Open = slices.DeleteFunc(u.Open, func(o capOpen) bool {
			if !o.same(j) {
				return false
			}
			switch {
			case o.Flagged:
				u.Ignored++
			case merged:
				u.helped(at, "任务 "+j.ID+"（"+j.Project+"）用上这项能力、过门合并")
			}
			return true
		})
	}
}

// retireWhy：该撤掉的原因，不该撤是空的。
func retireWhy(c gate.Capability, u *CapUse, now time.Time) string {
	since := c.AddedDate()
	if u != nil && u.LastHelp.After(since) {
		since = u.LastHelp
	}
	if days := int(now.Sub(since).Hours() / 24); days > capIdleDays {
		if u != nil && !u.LastHelp.IsZero() {
			return fmt.Sprintf("连续 %d 天没帮上忙（最近一次 %s：%s）", days, u.LastHelp.Format("01-02"), u.Help)
		}
		return fmt.Sprintf("加进来 %d 天（%s）一次都没帮上忙", days, c.Added)
	}
	if u != nil && u.Flags >= capFlagMin && u.Ignored*2 >= u.Flags {
		return fmt.Sprintf("检查报出问题 %d 次，%d 次被放过（照样合并或放弃），误报多", u.Flags, u.Ignored)
	}
	return ""
}

// retireDue：有该撤掉、还没写过条目的能力。scan 用它判断有没有新数据：撤掉看的是时间，没有新文件也要写。
func (st *SignalStore) retireDue(now time.Time) bool {
	caps, err := capList()
	if err != nil {
		return false
	}
	for _, c := range caps {
		key := retireKey(c.Name)
		if retireWhy(c, st.Caps[c.Name], now) != "" && !slices.Contains(st.ProposedKey, key) && !slices.ContainsFunc(st.Signals, func(sig Signal) bool { return sig.Key == key }) {
			return true
		}
	}
	return false
}

// detectRetire：该撤掉的能力记成信号（一次就写）。清单里已经没有的能力不再记账。
func (st *SignalStore) detectRetire(now time.Time) {
	caps, err := capList()
	if err != nil {
		return
	}
	for name := range st.Caps {
		if !slices.ContainsFunc(caps, func(c gate.Capability) bool { return c.Name == name }) {
			delete(st.Caps, name)
		}
	}
	for _, c := range caps {
		why := retireWhy(c, st.Caps[c.Name], now)
		if why == "" {
			continue
		}
		key := retireKey(c.Name)
		i := slices.IndexFunc(st.Signals, func(sig Signal) bool { return sig.Key == key })
		if i < 0 {
			st.Signals = append(st.Signals, Signal{Kind: KindRetire, Key: key, First: now, Seen: 1, Proposed: slices.Contains(st.ProposedKey, key)})
			i = len(st.Signals) - 1
		}
		sig := &st.Signals[i]
		sig.Summary = "能力 " + c.Name + "（" + DomainLabel(c.Domain) + "）" + why
		sig.Evidence = []string{"能力清单 " + gate.CapabilitiesFile + " 的 " + c.Name}
		sig.Last = now
	}
}

func (st *SignalStore) pruneCaps(now time.Time) {
	old := func(t time.Time) bool { return now.Sub(t) > signalKeepDays*24*time.Hour }
	st.Domains = slices.DeleteFunc(st.Domains, func(e DomainEvent) bool { return old(e.At) })
	for _, u := range st.Caps {
		u.Open = slices.DeleteFunc(u.Open, func(o capOpen) bool { return old(o.At) })
	}
}

// capProposal：缺口和撤掉的条目。标题以固定写法开头（合并后靠标题认出是哪个领域的缺口），正文写清怎么做；
// 修好过又出现的缺口标次数，不然和「已完成」里的旧条目同名，写不进去。
func capProposal(sig *Signal, fixes []string) (proposal, bool) {
	ev := sig.Evidence
	if len(ev) > proposalEvidMax {
		ev = ev[len(ev)-proposalEvidMax:]
	}
	var title, body string
	switch sig.Kind {
	case KindGap:
		cn := strings.TrimPrefix(sig.Key, KindGap+"|")
		title = gapTitlePrefix + cn
		if len(fixes) > 0 {
			title += fmt.Sprintf("（补过 %d 次又出现）", len(fixes))
		}
		body = fmt.Sprintf("%s；证据：%s；照 docs/reference/commands.md「能力缺口和撤掉能力」长出成套能力："+
			"1）先按 prior-art 找现成的做法和工具；2）先在 evals/ 新加 domain 写 %s 的考题，在当前这一代上判没过；"+
			"3）知识进 skills/ 或 rules/，能写成退出码的检查放进 internal/gate/capabilities/（能力门默认 warn），when 写清按项目怎么判断要不要用；"+
			"4）在 %s 登记这项能力（名字、领域、知识文件、检查、考题、启用条件、日期）；"+
			"5）ready.json 的 type 写 capability：评测门要这个领域加上新题后涨分、别的领域不掉。合并后这个领域的记录清零",
			sig.Summary, strings.Join(ev, "、"), DomainEN(cn), gate.CapabilitiesFile)
	case KindRetire:
		title = retireTitlePrefix + strings.TrimPrefix(sig.Key, KindRetire+"|")
		body = sig.Summary + "；核实后删掉它的知识文件、检查和 " + gate.CapabilitiesFile + " 里这一项，考题留着（只加不改）；不自动删文件，不该撤就回「不做」"
	default:
		return proposal{}, false
	}
	return proposal{sig: sig, title: title, line: title + " —— " + backlog.SuggestField(body, proposalLineMax+400)}, true
}

// printDomains 是 agentq evolve signals 里按领域的账和能力清单。
func (s *Service) printDomains(st *SignalStore) {
	now := s.Now()
	stats := domainStats(st.Domains, now)
	if len(stats) == 0 {
		fmt.Fprintln(s.Out, "按领域：近 30 天还没有归得了领域的任务")
	} else {
		fmt.Fprintf(s.Out, "按领域（近 30 天；返工、门没过、超时卡住算问题；问题 %d 次以上、问题率 %.0f%% 以上且是其他领域 %.0f 倍算缺口）：\n", gapMin, gapRate*100, gapTimes)
		for _, d := range stats {
			mark := ""
			if d.Gap {
				mark = "  缺口"
				if slices.Contains(st.ProposedKey, gapKey(d.Domain)) {
					mark += "，已写「" + gapTitlePrefix + d.Domain + "」"
				}
			}
			fmt.Fprintf(s.Out, "  %-16s 任务 %d  返工 %d  门没过 %d  超时卡住 %d  问题率 %.0f%%%s\n", DomainLabel(d.Domain), d.Tasks, d.Rework, d.GateFail, d.Stuck, d.Rate*100, mark)
		}
	}
	caps, err := capList()
	switch {
	case err != nil:
		fmt.Fprintf(s.Out, "能力清单读不了：%v\n", err)
		return
	case len(caps) == 0:
		fmt.Fprintln(s.Out, "能力：还没有登记的能力（"+gate.CapabilitiesFile+"）")
		return
	}
	fmt.Fprintf(s.Out, "能力 %d 项：\n", len(caps))
	for _, c := range caps {
		u := st.Caps[c.Name]
		help := "还没帮上过忙"
		flags := ""
		if u != nil {
			if !u.LastHelp.IsZero() {
				help = "最近帮上忙 " + u.LastHelp.Format("01-02") + "：" + textfmt.OneLine(u.Help, 60)
			}
			if u.Flags > 0 {
				flags = fmt.Sprintf("；检查报出 %d 次，放过 %d 次", u.Flags, u.Ignored)
			}
		}
		fmt.Fprintf(s.Out, "  %s（%s，%s 加）%s%s\n", c.Name, DomainLabel(c.Domain), c.Added, help, flags)
	}
}
