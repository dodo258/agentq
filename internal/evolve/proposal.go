package evolve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/suggest"
	"agentq/internal/textfmt"
)

// 自我派活：agentq 自己的信号同一 Key 出现 proposeAt 次以上（漏洞一次）、没提过，写一条有证据的条目。
// 自己长大开着、找得到 self_project：直接进它的「待做」，接着自动做（selfgrow.go）。
// 关着时只提案：写进 self_project BACKLOG 的「## 建议」，编号、「做 N」「不做 N」用 suggest 的索引和不做清单；在不做清单里的不再提。
// 没配（或找不到这个项目）：追加到 ~/.agentq/evolve/proposals.md，当天汇总记一行。
const (
	proposalTitleMax = 60
	proposalLineMax  = 400
	proposalEvidMax  = 5
)

type proposal struct {
	sig   *Signal
	title string
	line  string // 「标题 —— 说明」，不带「- [ ]」和编号
}

func makeProposal(sig *Signal) proposal {
	title := backlog.SuggestField("agentq 自身："+kindWords[sig.Kind]+"反复出现："+textfmt.OneLine(sig.Summary, 30), proposalTitleMax)
	ev := sig.Evidence
	if len(ev) > proposalEvidMax {
		ev = ev[len(ev)-proposalEvidMax:]
	}
	body := fmt.Sprintf("同类信号 %d 次，%s 到 %s：%s；证据：%s；自我进化 L0 提案，只提案不改代码，做不做你定",
		sig.Seen, sig.First.Format("01-02"), sig.Last.Format("01-02"), textfmt.OneLine(sig.Summary, 200), strings.Join(ev, "、"))
	return proposal{sig: sig, title: title, line: title + " —— " + backlog.SuggestField(body, proposalLineMax)}
}

// propose 把够次数、没提过的自身信号写成提案。写成了（或在不做清单里、已经提过）才标已提案，写不进的下次再试。
func (s *Service) propose(st *SignalStore, now time.Time) {
	var sigs []*Signal
	for i := range st.Signals {
		sig := &st.Signals[i]
		if !selfKind(sig.Kind) || sig.Proposed || sig.Seen < proposeAt && !oneShot(sig.Kind) {
			continue
		}
		sigs = append(sigs, sig)
	}
	if len(sigs) == 0 {
		return
	}
	if s.SelfGrowOn() {
		if p, ok := s.selfProjectAny(); ok {
			for _, pr := range s.proposeToTodo(p, sigs, st, now) {
				pr.sig.Proposed = true
				st.ProposedKey = append(st.ProposedKey, pr.sig.Key)
			}
			return
		}
		s.Logf("自我进化：self_project %s 不在项目里，提案照旧写", s.Cfg.SelfProject)
	}
	var props []proposal
	for _, sig := range sigs {
		if p, ok := capProposal(sig, st.Fixes[sig.Key]); ok {
			props = append(props, p)
			continue
		}
		props = append(props, makeProposal(sig))
	}
	done := s.proposeToProject(props, now)
	if done == nil {
		done = s.proposeToFile(props, now)
	}
	for _, p := range done {
		p.sig.Proposed = true
		st.ProposedKey = append(st.ProposedKey, p.sig.Key)
	}
}

// selfProject 找 self_project。没配、找不到、在排除名单里返回 false。
func (s *Service) selfProject() (backlog.Project, bool) {
	if s.Cfg.SelfProject == "" {
		return backlog.Project{}, false
	}
	projects, _ := s.Backlog.ActiveProjects() // 扫描不全时只要找得到它就行
	for _, p := range projects {
		if p.Name == s.Cfg.SelfProject {
			return p, true
		}
	}
	s.Logf("自我进化：self_project %s 不在项目里（或被排除），提案写进 %s", s.Cfg.SelfProject, s.Tilde(s.ProposalsPath()))
	return backlog.Project{}, false
}

// proposeToProject 写进 self_project 的「## 建议」。没配或找不到返回 nil（改写文件）；写不进返回空切片（下次再试）。
func (s *Service) proposeToProject(props []proposal, now time.Time) []proposal {
	p, ok := s.selfProject()
	if !ok {
		return nil
	}
	in := make([]suggest.Proposal, len(props))
	for i, pr := range props {
		in[i] = suggest.Proposal{Title: pr.title, Line: pr.line}
	}
	written, settled, err := s.Suggest.Propose(p, "待办：自我进化的提案", in, now)
	if err != nil {
		s.Logf("自我进化：%d 条提案没写进 %s（%v），下次再写", len(props), p.Name, err)
		return []proposal{}
	}
	if len(written) > 0 {
		for _, w := range written {
			s.Ledger.AppendNightlyLine(now, "自我进化：提案写进「建议」："+w)
		}
		s.Notify.Notify(notify.Notice{Cat: "suggest", Key: "evolve-propose:" + now.Format("20060102-1504"),
			Text: fmt.Sprintf("自我进化：agentq 自己反复出的 %d 个问题写进 %s 的「建议」", len(written), p.Name), Lines: suggest.SuggestNotice(written)})
	}
	out := []proposal{}
	for _, i := range settled {
		out = append(out, props[i])
	}
	return out
}

const proposalsHead = "# agentq 自我进化提案\n\n只提案，不改代码。配置 `self_project` 后提案写进那个项目 BACKLOG 的「## 建议」。\n\n"

// proposeToFile 追加到 proposals.md，当天汇总记一行。写不了返回空切片（下次再试）。
func (s *Service) proposeToFile(props []proposal, now time.Time) []proposal {
	var b strings.Builder
	if _, err := os.Stat(s.ProposalsPath()); errors.Is(err, fs.ErrNotExist) {
		b.WriteString(proposalsHead)
	}
	for _, p := range props {
		fmt.Fprintf(&b, "- %s　%s\n", now.Format("2006-01-02 15:04"), p.line)
	}
	f, err := os.OpenFile(s.ProposalsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.Logf("自我进化：提案没写上，下次再写：%v", err)
		return []proposal{}
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		s.Logf("自我进化：提案没写上，下次再写：%v", err)
		return []proposal{}
	}
	for _, p := range props {
		s.Ledger.AppendNightlyLine(now, "自我进化：提案「"+p.title+"」，见 "+s.Tilde(s.ProposalsPath()))
	}
	return props
}
