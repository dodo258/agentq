package evolve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/fsx"
)

// 复审只写进 proposals.md 的提案：以前没配 self_project（或找不到它）时提案只追加到 proposals.md，Key 记进 proposed_keys 后不再提。
// 配上之后第一次收信号（提案能写进项目了），把文件里还没复审过的每一行对回它的信号重新判一次：
//   - 近 14 天还出现过：照现在的去处写一次（自己长大开着进待做，关着进「建议」），行尾标「已转进 …」；
//   - 对不上信号（30 天没出现、已经删了）或 14 天没再出现：行尾标「已过时」，不再提。
//
// 复审过的标在文件行尾，不另记状态：老的 proposals.md 照样能认，标过的不再看。写不进项目的行不标，下次再试。
const (
	reviewDays     = 14
	markStale      = "（已过时"
	markMoved      = "（已转进"
	fileLinePrefix = "- "
)

// fileLine 是 proposals.md 里没复审过的一行提案。
type fileLine struct {
	idx  int    // 在文件里的行号（从 0 起）
	text string // 时间之后的「标题 —— 说明」
}

// unreviewedFileLines 读 proposals.md 里没复审过的提案行。没配 self_project、没有文件、读不了都是空。
func (s *Service) unreviewedFileLines() []fileLine {
	if s.Cfg.SelfProject == "" {
		return nil
	}
	b, err := os.ReadFile(s.ProposalsPath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Logf("自我进化：读 %s 失败，这次不复审旧提案：%v", s.Tilde(s.ProposalsPath()), err)
		}
		return nil
	}
	var out []fileLine
	for i, l := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(l, fileLinePrefix)
		if !ok || strings.Contains(l, markStale) || strings.Contains(l, markMoved) {
			continue
		}
		// 「- 2006-01-02 15:04　标题 —— 说明」：时间和正文之间是全角空格
		if _, text, ok := strings.Cut(rest, "　"); ok && text != "" {
			out = append(out, fileLine{idx: i, text: text})
		}
	}
	return out
}

// fileReview 是一次复审的结论：again 是要重新写的信号，lineSig 是每行对上的信号（对不上或过时的是 nil）。
type fileReview struct {
	lines   []fileLine
	lineSig []*Signal
	again   []*Signal
}

// reviewFileLines 把每行对回信号：近 14 天还出现过的进 again。
func (s *Service) reviewFileLines(st *SignalStore, lines []fileLine, now time.Time) fileReview {
	r := fileReview{lines: lines, lineSig: make([]*Signal, len(lines))}
	for i, l := range lines {
		for j := range st.Signals {
			sig := &st.Signals[j]
			if !selfKind(sig.Kind) || !proposedIn(l.text, sig) {
				continue
			}
			if now.Sub(sig.Last) <= reviewDays*24*time.Hour {
				r.lineSig[i] = sig
				if !slices.Contains(r.again, sig) {
					r.again = append(r.again, sig)
				}
			}
			break
		}
	}
	return r
}

// proposedIn：这行提案是不是这个信号写的。能力缺口、撤掉能力的标题由 Key 定，对标题；
// 其余对「agentq 自身：<种类>反复出现」开头，再对证据（提案带着当时最后几条证据，同一种类不同信号的证据不会一样）。
func proposedIn(text string, sig *Signal) bool {
	if p, ok := capProposal(sig, nil); ok {
		return strings.HasPrefix(text, p.title+" ") || strings.HasPrefix(text, p.title+"（")
	}
	if !strings.HasPrefix(text, "agentq 自身："+kindWords[sig.Kind]+"反复出现") {
		return false
	}
	return slices.ContainsFunc(sig.Evidence, func(ev string) bool {
		return strings.Contains(text, ev) || strings.Contains(text, backlog.SuggestField(ev, proposalLineMax))
	})
}

// markFileLines 在复审过的行尾标结论：对上的信号写成了标「已转进 where」，过时的标「已过时」，写不进的不标（下次再试）。
func (s *Service) markFileLines(r fileReview, done []proposal, where string, now time.Time) {
	if len(r.lines) == 0 {
		return
	}
	b, err := os.ReadFile(s.ProposalsPath())
	if err != nil {
		s.Logf("自我进化：读 %s 失败，旧提案这次没标：%v", s.Tilde(s.ProposalsPath()), err)
		return
	}
	text := strings.Split(string(b), "\n")
	day := now.Format("01-02")
	moved, stale := 0, 0
	for i, l := range r.lines {
		if l.idx >= len(text) || !strings.HasSuffix(text[l.idx], l.text) { // 拿着 evolve.lock，文件只会被追加；对不上就不动
			continue
		}
		sig := r.lineSig[i]
		switch {
		case sig == nil:
			text[l.idx] += fmt.Sprintf("%s %s：近 %d 天没再出现）", markStale, day, reviewDays)
			stale++
		case slices.ContainsFunc(done, func(p proposal) bool { return p.sig == sig }):
			text[l.idx] += markMoved + " " + where + " " + day + "）"
			moved++
		}
	}
	if moved+stale == 0 {
		return
	}
	if err := fsx.WriteFileAtomic(s.ProposalsPath(), []byte(strings.Join(text, "\n"))); err != nil {
		s.Logf("自我进化：旧提案没标上，下次再标：%v", err)
		return
	}
	s.Ledger.AppendNightlyLine(now, fmt.Sprintf("自我进化：复审 proposals.md 里的旧提案：%d 条转进 %s，%d 条已过时", moved, where, stale))
}
