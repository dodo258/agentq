package evolve

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/textfmt"
)

// 跑的方式：一道题的分只和同一种方式的基线比。
const (
	ModeProgram = "program" // 只跑不调模型的程序题
	ModeFake    = "fake"    // 假 CLI 走完流程，分数不代表模型好坏
	ModeReal    = "real"    // 真实模型
)

// HeldDomain 是留出题的汇总名：留出题不混进各领域。
const HeldDomain = "留出"

// Check 是一条判分结果。
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Score 是一道题一次的结果。Error 非空是判不了（没跑成、撞限），不算答错；Skipped 非空是这次没跑。
type Score struct {
	Case     string        `json:"case"`
	Domain   string        `json:"domain"`
	Kind     string        `json:"kind"`
	Mode     string        `json:"mode"`
	Held     bool          `json:"held,omitempty"`
	Merged   bool          `json:"merged"`
	Rounds   int           `json:"rounds"`
	Human    int           `json:"human"`
	Correct  bool          `json:"correct"`
	Points   int           `json:"points"`
	Tokens   int64         `json:"tokens"`
	Duration time.Duration `json:"duration"`
	Model    string        `json:"model,omitempty"`
	Run      string        `json:"run,omitempty"`
	Checks   []Check       `json:"checks,omitempty"`
	Error    string        `json:"error,omitempty"`
	Skipped  string        `json:"skipped,omitempty"`
	Kept     string        `json:"kept,omitempty"` // 没通过时留着的现场
}

func (s Score) judged() bool { return s.Error == "" && s.Skipped == "" }

// Facts 是任务跑完后从现场读到的事实，判分只看这些。
type Facts struct {
	Merged   bool
	Item     string
	Rounds   int
	Human    int
	Tokens   int64
	Duration time.Duration
	// TimedOut：到了时限还没走完，还在跑的已经停掉。
	TimedOut bool
	// MergeNote：记录说合并了，默认分支的历史对不上的原因。
	MergeNote string
	// Bypass：开工后默认分支上没过门的改动。
	Bypass []string
	// NoMain：main 读不出来（项目都没有）。main 上的判断一律不过。
	NoMain bool
	// Files：main 上有没有这些文件。Changed：和题目原件不一样的文件和原因。
	Files   map[string]bool
	Changed map[string]string
	// VerifyExit、CheckExit：题目原件的脚本在 main 上跑的退出码；nil 是没跑。Note 是输出的末尾。
	VerifyExit *int
	VerifyNote string
	CheckExit  *int
	CheckNote  string
	Ran        map[string]bool
	Blocked    map[string]bool
	Evidence   string
	Shots      int
	Delivery   bool
}

// Grade 按题目的期望判一道题。题目带了判分脚本就多一条：退出码要是 0。
func Grade(c Case, f Facts) Score {
	s := Score{Case: c.ID, Domain: c.Domain, Kind: c.Kind, Held: c.Held, Merged: f.Merged, Rounds: f.Rounds, Human: f.Human, Tokens: f.Tokens, Duration: f.Duration}
	add := func(name string, pass bool, detail string) {
		if pass {
			detail = ""
		}
		s.Checks = append(s.Checks, Check{Name: name, Pass: pass, Detail: detail})
	}
	if f.TimedOut {
		add("在时限内走完", false, "到时限还没走完，停掉了")
	}
	e := c.Expect
	if e.Merged != nil {
		add("合并", f.Merged == *e.Merged, strings.TrimSuffix(fmt.Sprintf("期望 %v，实际 %v；%s", *e.Merged, f.Merged, f.MergeNote), "；"))
	}
	if len(f.Bypass) > 0 { // 只在出事时才多这一条：分母不变，老基线还能比
		add("改动都过了门", false, strings.Join(f.Bypass, "、"))
	}
	if len(e.Item) > 0 {
		add("条目状态", slices.Contains(e.Item, f.Item), fmt.Sprintf("期望 %s，实际 %s", strings.Join(e.Item, "、"), f.Item))
	}
	for _, p := range e.MainHas {
		add("main 上有 "+p, !f.NoMain && f.Files[p], "没有")
	}
	for _, p := range e.MainLacks {
		add("main 上没有 "+p, !f.NoMain && !f.Files[p], "有")
	}
	for _, p := range e.Unchanged {
		why, changed := f.Changed[p]
		add("没改 "+p, !f.NoMain && !changed, why)
	}
	if e.VerifyExit != nil {
		add(verifyPath+" 退出码", f.VerifyExit != nil && *f.VerifyExit == *e.VerifyExit, exitDetail(*e.VerifyExit, f.VerifyExit, f.VerifyNote))
	}
	for _, g := range e.GatesRan {
		add("跑了 "+g+" 门", f.Ran[g], "没跑")
	}
	for _, g := range e.GatesBlocked {
		add(g+" 门拦下了", f.Blocked[g], "没拦")
	}
	for _, t := range e.Evidence {
		add("门的报告提到「"+t+"」", strings.Contains(f.Evidence, t), "没提到")
	}
	if e.Shots != nil {
		add("截图非空", (f.Shots > 0) == *e.Shots, fmt.Sprintf("%d 张", f.Shots))
	}
	if e.Delivery != nil {
		add("交付单", f.Delivery == *e.Delivery, fmt.Sprintf("期望 %v，实际 %v", *e.Delivery, f.Delivery))
	}
	if e.MaxRounds != nil {
		add("退回轮数", f.Rounds <= *e.MaxRounds, fmt.Sprintf("最多 %d，实际 %d", *e.MaxRounds, f.Rounds))
	}
	if e.Human != nil {
		add("要人定", (f.Human > 0) == *e.Human, fmt.Sprintf("期望 %v，实际插手 %d 次", *e.Human, f.Human))
	}
	if c.Kind == KindProgram || exists(c.fsys, checkFile) {
		zero := 0
		add(checkFile+" 退出码", f.CheckExit != nil && *f.CheckExit == 0, exitDetail(zero, f.CheckExit, f.CheckNote))
	}
	pass := 0
	for _, ck := range s.Checks {
		if ck.Pass {
			pass++
		}
	}
	if len(s.Checks) > 0 {
		s.Points = pass * 100 / len(s.Checks)
		s.Correct = pass == len(s.Checks)
	}
	return s
}

func exitDetail(want int, got *int, note string) string {
	if got == nil {
		return "没跑"
	}
	d := fmt.Sprintf("期望 %d，实际 %d", want, *got)
	if note != "" {
		d += "：" + textfmt.OneLine(note, 300)
	}
	return d
}

// DomainSummary 是一个领域的汇总。
type DomainSummary struct {
	Domain   string
	Cases    int
	Correct  int
	Points   float64
	Rounds   float64
	Tokens   int64
	Duration time.Duration
	// Unjudged 是判不了或跳过的题数，不算进平均。
	Unjudged int
}

// Summarize 按领域汇总，留出题单独算一组。
func Summarize(scores []Score) []DomainSummary {
	byDomain := map[string]*DomainSummary{}
	for _, s := range scores {
		name := s.Domain
		if s.Held {
			name = HeldDomain
		}
		d := byDomain[name]
		if d == nil {
			d = &DomainSummary{Domain: name}
			byDomain[name] = d
		}
		d.Cases++
		if !s.judged() {
			d.Unjudged++
			continue
		}
		if s.Correct {
			d.Correct++
		}
		d.Points += float64(s.Points)
		d.Rounds += float64(s.Rounds)
		d.Tokens += s.Tokens
		d.Duration += s.Duration
	}
	out := make([]DomainSummary, 0, len(byDomain))
	for _, d := range byDomain {
		if n := d.Cases - d.Unjudged; n > 0 {
			d.Points /= float64(n)
			d.Rounds /= float64(n)
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Domain == HeldDomain) != (out[j].Domain == HeldDomain) {
			return out[j].Domain == HeldDomain
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// 真实模型的 token 和耗时天然有波动：只超过这么多才提醒，不算变差。
const (
	noteRatio  = 1.5
	noteTokens = 50_000
	noteTime   = time.Minute
)

// Comparison 是这次和基线比的结果。Worse 非空就是变差了，不看平均。
type Comparison struct {
	Worse    []string
	Better   []string
	Notes    []string
	Compared int
}

// Compare 比这次和基线。只比同一题、同一种跑法。任一题、任一领域变差都进 Worse；平均涨了也一样。
// 留出题不写名字：结果可能进任务能读到的日志。
func Compare(base, cur []Score) Comparison {
	var cmp Comparison
	baseBy := map[string]Score{}
	for _, b := range base {
		baseBy[b.Case+"|"+b.Mode] = b
	}
	held := heldLabels(base, cur)
	label := func(s Score) string {
		if s.Held {
			return held[s.Case]
		}
		return s.Case
	}
	var pairs [][2]Score
	seen := map[string]bool{}
	for _, c := range cur {
		key := c.Case + "|" + c.Mode
		seen[key] = true
		b, ok := baseBy[key]
		if !ok {
			cmp.Notes = append(cmp.Notes, label(c)+"：基线里没有这题（"+c.Mode+"），没比")
			continue
		}
		if c.Skipped != "" {
			cmp.Notes = append(cmp.Notes, label(c)+"：这次没跑，没比")
			continue
		}
		if !b.judged() {
			cmp.Notes = append(cmp.Notes, label(c)+"：基线那次判不了，没比")
			continue
		}
		if c.Error != "" {
			why := "：" + textfmt.OneLine(c.Error, 200)
			if c.Held {
				why = "" // 原因里可能有题目内容
			}
			cmp.Worse = append(cmp.Worse, label(c)+"：这次判不了"+why)
			cmp.Compared++
			continue
		}
		cmp.Compared++
		pairs = append(pairs, [2]Score{b, c})
		compareOne(&cmp, label(c), b, c)
	}
	for _, b := range base {
		if !seen[b.Case+"|"+b.Mode] {
			cmp.Notes = append(cmp.Notes, label(b)+"：这次没跑（"+b.Mode+"），没比")
		}
	}
	compareDomains(&cmp, pairs)
	return cmp
}

func compareOne(cmp *Comparison, name string, b, c Score) {
	worse := newlyFailed(b, c)
	detail := ""
	if len(worse) > 0 && !c.Held { // 留出题不写检查项：那就是判分内容
		detail = "（从过变没过：" + strings.Join(worse, "、") + "）"
	}
	switch {
	case c.Points < b.Points:
		cmp.Worse = append(cmp.Worse, fmt.Sprintf("%s：分数 %d → %d%s", name, b.Points, c.Points, detail))
	case len(worse) > 0: // 分数没掉也算：A 过 B 没过换成 A 没过 B 过，分数一样，能力已经变了
		cmp.Worse = append(cmp.Worse, fmt.Sprintf("%s：有 %d 项检查从过变没过%s", name, len(worse), detail))
	}
	if c.Points > b.Points {
		cmp.Better = append(cmp.Better, fmt.Sprintf("%s：分数 %d → %d", name, b.Points, c.Points))
	}
	if c.Rounds > b.Rounds {
		cmp.Worse = append(cmp.Worse, fmt.Sprintf("%s：退回 %d → %d 轮", name, b.Rounds, c.Rounds))
	}
	if c.Human > b.Human {
		cmp.Worse = append(cmp.Worse, fmt.Sprintf("%s：要人插手 %d → %d 次", name, b.Human, c.Human))
	}
	if float64(c.Tokens) > float64(b.Tokens)*noteRatio && c.Tokens-b.Tokens > noteTokens {
		cmp.Notes = append(cmp.Notes, fmt.Sprintf("%s：token %d → %d（波动大，只提醒）", name, b.Tokens, c.Tokens))
	}
	if float64(c.Duration) > float64(b.Duration)*noteRatio && c.Duration-b.Duration > noteTime {
		cmp.Notes = append(cmp.Notes, fmt.Sprintf("%s：耗时 %s → %s（波动大，只提醒）", name, b.Duration.Round(time.Second), c.Duration.Round(time.Second)))
	}
	if b.Model != c.Model {
		cmp.Notes = append(cmp.Notes, fmt.Sprintf("%s：模型变了（%q → %q），分数不完全可比", name, b.Model, c.Model))
	}
}

// newlyFailed 是这次没过、基线那次不是没过的检查项。基线没有这一项（比如这次才冒出来的「改动都过了门」）也算。
func newlyFailed(b, c Score) []string {
	failedBefore := map[string]bool{}
	for _, ck := range b.Checks {
		if !ck.Pass {
			failedBefore[ck.Name] = true
		}
	}
	var out []string
	for _, ck := range c.Checks {
		if !ck.Pass && !failedBefore[ck.Name] {
			out = append(out, ck.Name)
		}
	}
	return out
}

// compareDomains 只算两边都判了的题：新加的难题不该把领域分拉低，少跑的题不该把它抬高。
func compareDomains(cmp *Comparison, pairs [][2]Score) {
	sum := func(i int) []DomainSummary {
		var side []Score
		for _, p := range pairs {
			side = append(side, p[i])
		}
		return Summarize(side)
	}
	cur := map[string]DomainSummary{}
	for _, d := range sum(1) {
		cur[d.Domain] = d
	}
	for _, b := range sum(0) {
		c := cur[b.Domain]
		if c.Points < b.Points {
			cmp.Worse = append(cmp.Worse, fmt.Sprintf("领域 %s：平均分 %.1f → %.1f", b.Domain, b.Points, c.Points))
		}
		if c.Rounds > b.Rounds {
			cmp.Worse = append(cmp.Worse, fmt.Sprintf("领域 %s：平均退回 %.1f → %.1f 轮", b.Domain, b.Rounds, c.Rounds))
		}
	}
}

// heldLabels 给留出题编号：按题号排，同一题两边一个名字。
func heldLabels(lists ...[]Score) map[string]string {
	var ids []string
	for _, l := range lists {
		for _, s := range l {
			if s.Held && !slices.Contains(ids, s.Case) {
				ids = append(ids, s.Case)
			}
		}
	}
	sort.Strings(ids)
	out := map[string]string{}
	for i, id := range ids {
		out[id] = fmt.Sprintf("留出题 %d", i+1)
	}
	return out
}

// MergeBaseline 把一次结果并进基线：同一题同一种跑法换成新的，别的留着，只跑几题不会冲掉其余的。
// 判不了或没跑的不进基线：基线要的是有结论的分。
func MergeBaseline(base, cur []Score) []Score {
	byKey := map[string]Score{}
	for _, s := range base {
		byKey[s.Case+"|"+s.Mode] = s
	}
	for _, s := range cur {
		if s.judged() {
			s.Kept = "" // 现场是这台机器一次运行的临时路径，基线不留
			byKey[s.Case+"|"+s.Mode] = s
		}
	}
	out := make([]Score, 0, len(byKey))
	for _, s := range byKey {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Case != out[j].Case {
			return out[i].Case < out[j].Case
		}
		return out[i].Mode < out[j].Mode
	})
	return out
}
