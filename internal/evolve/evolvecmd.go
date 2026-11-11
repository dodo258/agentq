package evolve

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentq/internal/state"
	"agentq/internal/textfmt"
)

const evolveUsage = `agentq evolve：自我进化的信号、学到的约束、自己升级的记录
  agentq evolve signals              看收到的信号、等确认的门拦截、按领域的账和能力缺口、能力清单、提案和自己长大写进待做的条目
  agentq evolve constraints <项目>    看这个项目学到的约束（新任务合同末尾带的就是这些）
  agentq evolve upgrades             看每次演练、升级、留下、回滚的记录和现在的观察期（自己长大和自动更新）
  agentq evolve apply                现在就照规则做一次升级或回滚（tick 空闲时自动起它；有任务在跑什么都不做）
`

// CmdEvolve 是命令入口。apply 之外只读，不收集、不写文件、不调模型。
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
	case args[0] == "upgrades" && len(args) == 1:
		return s.cmdUpgrades()
	case args[0] == "apply" && len(args) == 1:
		return s.Apply()
	}
	fmt.Fprint(s.Out, evolveUsage)
	return fmt.Errorf("不认识的写法：agentq evolve %s", strings.Join(args, " "))
}

func (s *Service) cmdSignals() error {
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
	s.printDomains(st)
	for _, it := range st.SelfItems {
		where := "等起任务"
		switch {
		case it.Job != "":
			where = "任务 " + it.Job
		case it.Note != "":
			where = "没自动起：" + it.Note
		}
		fmt.Fprintf(s.Out, "写进 %s 的待做（%s）：%s\n", it.Project, where, textfmt.OneLine(it.Title, 80))
	}
	switch {
	case s.SelfGrowOn():
		fmt.Fprintf(s.Out, "自己长大开着：agentq 自己的问题写进 %s 的「待做」，自动接着做\n", s.Cfg.SelfProject)
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

func (s *Service) cmdUpgrades() error {
	evs, err := s.LoadUpgrades()
	if err != nil {
		return err
	}
	switch {
	case s.Cfg.SelfProject == "" && s.Cfg.AutoUpdate:
		fmt.Fprintln(s.Out, "自动更新："+s.AutoUpdateLine())
	case !s.Cfg.SelfGrowEnabled:
		fmt.Fprintln(s.Out, "自己升级关着（self_grow_enabled=false）")
	case s.Cfg.SelfProject == "":
		fmt.Fprintln(s.Out, "自己升级没起作用：没配 self_project（agentq 源码所在的项目名）")
	case !devVersionRe.MatchString(s.Version):
		fmt.Fprintf(s.Out, "自己升级没起作用：这一代是 %s，只认源码构建的 dev-<提交>（发行版用 agentq update）\n", s.Version)
	default:
		fmt.Fprintf(s.Out, "自己升级开着：self_project=%s，现在是 %s\n", s.Cfg.SelfProject, s.Version)
	}
	if up := openUpgrade(evs, s.Version); up != nil {
		if st, err := state.Load(s.StatePath()); err == nil {
			before, after := windows(st.Jobs, up.At)
			fmt.Fprintf(s.Out, "观察期：升级后 %d/%d 个任务（%s）；升级前 %s\n", after.N, observeTasks, after, before)
		}
	}
	if len(evs) == 0 {
		fmt.Fprintln(s.Out, "还没有演练、升级或回滚过")
		return nil
	}
	for _, ev := range evs {
		fmt.Fprintf(s.Out, "%s  %s\n", ev.At.Format("2006-01-02 15:04"), eventLine(ev))
	}
	fmt.Fprintf(s.Out, "（%s）\n", s.Tilde(s.upgradesPath()))
	return nil
}
