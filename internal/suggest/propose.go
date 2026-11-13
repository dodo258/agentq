package suggest

import (
	"fmt"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/textfmt"
)

// Proposal 是要写进「## 建议」的一条：Title 判重用，Line 是「标题 —— 说明」（不带「- [ ]」和编号，调用方已清洗）。
type Proposal struct {
	Title string
	Line  string
}

// Propose 把 props 写进项目 BACKLOG 的「## 建议」并提交，编号进索引。自我进化的提案和产品经理调研都走这里。
// 不写的：不做清单里的、BACKLOG 里已有同标题的、30 天内提过的。返回写进去的（「编号　项目　标题」）和处理完的下标（写了的和判重跳过的）。
// 写不进（BACKLOG 结构不对、主目录脏、拿不到锁、索引读不了）返回错误，调用方下次再试。
func (a *Service) Propose(p backlog.Project, commitMsg string, props []Proposal, now time.Time) (written []string, settled []int, err error) {
	if why := a.SuggestUnwritable(p); why != "" {
		return nil, nil, fmt.Errorf("%s 的 BACKLOG 现在写不进：%s", p.Name, why)
	}
	store, err := a.LiveSuggestions(true)
	if err != nil {
		return nil, nil, fmt.Errorf("读不了建议索引：%w", err)
	}
	known := map[string]bool{}
	for _, e := range store.Proposed(p.Name) {
		known[backlog.TitleKey(e.Title)] = true
	}
	first, err := a.ReserveSuggestNumbers(len(props))
	if err != nil {
		return nil, nil, fmt.Errorf("建议编号没存下：%w", err)
	}
	var entries []SuggestEntry
	err = a.Backlog.CommitBacklog(p, commitMsg, func(old string) (string, error) {
		written, settled, entries = nil, nil, nil
		have := backlog.BacklogTitleKeys(old)
		rejected := a.NotDoingKeys(p)
		var lines []string
		next := first
		for i, pr := range props {
			k := backlog.TitleKey(pr.Title)
			switch {
			case rejected[k]:
				a.Logf("建议：%s 的「%s」在不做清单里，不提", p.Name, pr.Title)
				settled = append(settled, i)
				continue
			case have[k] || known[k]:
				settled = append(settled, i)
				continue
			}
			have[k] = true
			line := fmt.Sprintf("- [ ] %s（建议 %d）", pr.Line, next)
			lines = append(lines, line)
			entries = append(entries, SuggestEntry{N: next, Project: p.Name, Fingerprint: backlog.Fingerprint(line), Title: pr.Title, CreatedAt: now})
			written = append(written, fmt.Sprintf("%d　%s　%s", next, p.Name, pr.Title))
			settled = append(settled, i)
			next++
		}
		if len(lines) == 0 {
			return old, nil
		}
		text := InsertSuggestions(old, lines)
		if err := VerifySuggestionPresent(text, lines); err != nil {
			return "", err
		}
		return text, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(entries) > 0 {
		if err := a.WithSuggest(func(st *SuggestStore) error {
			st.Items = append(st.Items, entries...)
			st.Prune(now)
			return nil
		}); err != nil {
			a.Logf("建议：写进了 %s 的 BACKLOG，但编号没存下（回「做 N」用不了，手动移也行）：%v", p.Name, err)
		}
	}
	return written, settled, nil
}

// OpenSuggestionKeys：BACKLOG「## 建议」里还没回的条目的标题（backlog.TitleKey，注释里的不算）。
func OpenSuggestionKeys(text string) map[string]bool {
	keys := map[string]bool{}
	backlog.EachBacklogLine(text, func(section, line string) {
		if strings.HasPrefix(section, suggestSection) && strings.HasPrefix(line, "- [ ] ") {
			keys[backlog.TitleKey(backlog.ItemTitle(strings.TrimPrefix(line, "- [ ] ")))] = true
		}
	})
	return keys
}

// SuggestNotice 是写进「建议」后推给用户的那几行：每条一行，最后一行教怎么回。
func SuggestNotice(written []string) []string {
	var lines []string
	for _, w := range written {
		lines = append(lines, "建议 "+textfmt.TruncRunes(w, 56))
	}
	return append(lines, "回复「做 "+strings.Join(SuggestNumbers(written, 2), " ")+"」移进待做，「不做 N」以后不再提")
}
