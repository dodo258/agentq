package evolve

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/textfmt"
)

// 返工：用户验收过一条合并了的条目、又提了意见，说明那次交付没达到原条目要的。这是 agentq 自己的问题（selfKind），
// 按领域记信号（同一领域两次写进待做），也记进按领域的账（能力缺口）。
//
// 意见由 dispatch 写成「验收 <原条目标题> 的意见」，分两种，只有第一种是返工：
//   - 交付没做到原条目要的（做错、漏了、坏了、达不到原验收）：照这个写法；
//   - 原条目照要求做到了，用户改了主意或加新要求（换个键位、换种说法、再加一样）：标题末尾加「（改需求）」。
//
// 为什么这样分：以前凡是验收意见都算返工。11-23 huitai 用户嫌 ⌃] 难按、改成 ⌃Q，而 ⌃] 正是原条目点名要的，
// 交付没错，这条却同时记成前端返工、后端返工，各和一条不相干的返工凑成两条「验收后返工反复出现」写进 agentq 的待做。
// 11-16 那次（顺手改的记录文件被当成文档）也是同一个毛病：把「有后续意见」当成「交付不行」，再按顺带的属性归类。
// 是不是改主意只有听到用户原话的派活会话分得清，所以在源头标、这里只认标记，不猜意见的文字。
//
// 归领域见 domain.go。同一条意见（reworkKey）只算一次，认领、合并后移到「已完成」也还是同一条。

// changeMark：dispatch 写在「验收 … 的意见」后面，说这条是改需求、不是返工。只认写在标题里的（「——」之前）：
// 合并后引擎会在行尾接着写合并提交，写在行尾的标记会被挤到中间。
const changeMark = "（改需求）"

// reworkItem 是 BACKLOG 里一条算返工的验收意见。
type reworkItem struct {
	project string
	key     string // reworkKey，记进 ReworkSeen
	title   string // 原条目标题
	text    string
	domains []string
	basis   string // 按什么归的领域
}

// evidence：信号里的证据，指回那条意见。
func (r reworkItem) evidence() string {
	return "项目 " + r.project + "「验收 " + textfmt.TruncRunes(r.title, 30) + " 的意见」"
}

// reworkRe：dispatch 写的「验收 <原条目标题> 的意见」，只认标题开头的固定写法，正文顺带提到不算。
var (
	reworkRe  = regexp.MustCompile(`^验收\s+(.+?)\s*的意见`)
	mentionRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

const reworkMax = 8 // 一条意见最多看几个提交

// reworkFeedback：这一条（「- [ ] 」后面的原文）是算返工的验收意见就返回原条目标题；标了（改需求）的不算。
func reworkFeedback(body string) (string, bool) {
	title := backlog.ItemTitle(body)
	m := reworkRe.FindStringSubmatch(title)
	if m == nil || strings.Contains(title, changeMark) {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(m[1]), "「」“”\"`"), true
}

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
			orig, ok := reworkFeedback(body)
			if !ok {
				return
			}
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

// absorbRework：一条返工意见，每个领域记一次（E7 按领域看返工）。
// 摘要先说是哪一类，再说最近一次是哪个项目的哪条意见；意见原文放在引号里：它是别的项目的需求，写进 agentq 待做时不能读成要 agentq 做的事。
func (s *Service) absorbRework(st *SignalStore, r reworkItem, now time.Time) {
	ev := r.evidence()
	quote := textfmt.OneLine(strings.TrimLeft(strings.TrimPrefix(r.text, backlog.ItemTitle(r.text)), " —"), 100)
	for _, d := range r.domains {
		summary := d + "返工（" + r.basis + "），最近一次：" + ev + "，意见原文「" + quote + "」"
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
