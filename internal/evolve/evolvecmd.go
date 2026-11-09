package evolve

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentq/internal/textfmt"
)

const evolveUsage = `agentq evolve：自我进化的信号和学到的约束（只读）
  agentq evolve signals              看收到的信号、等确认的门拦截和提案
  agentq evolve constraints <项目>    看这个项目学到的约束（新任务合同末尾带的就是这些）
`

// CmdEvolve 是命令入口。只读，不收集、不写文件、不调模型。
func (s *Service) CmdEvolve(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(s.Out, evolveUsage)
		return errors.New("要写子命令")
	}
	switch {
	case args[0] == "signals" && len(args) == 1:
		return s.cmdSignals()
	case args[0] == "constraints" && len(args) == 2:
		return s.cmdConstraints(args[1])
	}
	fmt.Fprint(s.Out, evolveUsage)
	return fmt.Errorf("不认识的写法：agentq evolve %s", strings.Join(args, " "))
}

func (s *Service) cmdSignals() error {
	if !s.Cfg.EvolveSignalsEnabled {
		fmt.Fprintln(s.Out, "evolve_signals_enabled 关着：tick 不收信号，下面是关之前收到的")
	}
	st, err := s.LoadSignals()
	if err != nil {
		return err
	}
	if st.Checked.IsZero() {
		fmt.Fprintln(s.Out, "还没收到过信号（tick 每小时看一次门结果、任务记录和巡逻）")
		return nil
	}
	fmt.Fprintf(s.Out, "信号 %d 类，等确认的门拦截 %d 次；最近一次收到新数据 %s（%s）\n", len(st.Signals), len(st.Pending), st.Checked.Format("2006-01-02 15:04"), s.Tilde(s.SignalsPath()))
	sigs := append([]Signal{}, st.Signals...)
	sort.SliceStable(sigs, func(a, b int) bool { return sigs[a].Last.After(sigs[b].Last) })
	for _, sig := range sigs {
		who := sig.Project
		if selfKind(sig.Kind) {
			who = "agentq"
		}
		mark := ""
		if sig.Proposed {
			mark = "  已提案"
		}
		fmt.Fprintf(s.Out, "%-13s %2d 次  %-12s %s%s\n", sig.Kind, sig.Seen, who, textfmt.OneLine(sig.Summary, 100), mark)
		fmt.Fprintf(s.Out, "    最近 %s；证据：%s\n", sig.Last.Format("01-02 15:04"), textfmt.OneLine(strings.Join(sig.Evidence, "、"), 200))
	}
	switch {
	case s.Cfg.SelfProject != "":
		fmt.Fprintf(s.Out, "提案写进 %s 的「## 建议」，回「做 N」移进待做\n", s.Cfg.SelfProject)
	default:
		fmt.Fprintf(s.Out, "提案写在 %s（配 self_project 就写进那个项目的「## 建议」）\n", s.Tilde(s.ProposalsPath()))
	}
	return nil
}

func (s *Service) cmdConstraints(project string) error {
	items, err := s.LoadConstraints(project)
	if err != nil {
		return err
	}
	if !s.Cfg.LearnedConstraintsEnabled {
		fmt.Fprintln(s.Out, "learned_constraints_enabled 关着：新任务合同不带这些，也不再学新的")
	}
	if len(items) == 0 {
		fmt.Fprintf(s.Out, "%s 还没有学到的约束\n", project)
		return nil
	}
	fmt.Fprintln(s.Out, constraintsHead)
	for _, l := range constraintLines(items) {
		fmt.Fprintln(s.Out, l)
	}
	return nil
}
