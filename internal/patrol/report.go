package patrol

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"agentq/internal/backend"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// notifyPatrol：新发现、没自动处理的问题推给用户（合成一条「巡逻发现」）。「等用户决定」由 tick 的 watchDecisions 推，
// 额度由读额度时推，基础设施类由看门狗推，这里不重复。
func (a *Service) notifyPatrol(r *PatrolReport) {
	var list []notify.Notice
	for _, is := range r.New {
		if is.Check == "decide" || is.Check == "usage" || is.infra() || is.handled {
			continue
		}
		n := notify.Notice{Cat: "patrol", Key: "patrol:" + is.Fingerprint, Text: is.Title, JobID: is.jobID}
		if len(is.Evidence) > 0 {
			n.Lines = append(n.Lines, textfmt.OneLine(is.Evidence[0], 120))
		}
		if is.Action != "" {
			n.Lines = append(n.Lines, textfmt.OneLine(is.Action, 120))
		}
		list = append(list, n)
	}
	a.Notify.Notify(list...)
}

// patrolAnalyze：有新异常、patrol_enabled 开着、今天还没满 patrolAnalyzeMax 次、能接活（没暂停、不在冷却/熔断、额度没到 90%）、
// 上一次分析也没在跑时，启动一次 claude -p 只读分析新异常的原因。结果由那个任务结束时写进汇总。
func (a *Service) patrolAnalyze(st *state.State, ps *PatrolState, r *PatrolReport) {
	var issues []PatrolIssue
	for _, is := range r.New {
		if is.Anomaly {
			issues = append(issues, is)
		}
	}
	if len(issues) == 0 {
		return
	}
	now := a.Now()
	day := now.Format("2006-01-02")
	if ps.AnalysisDay != day {
		ps.AnalysisDay, ps.AnalysisCount = day, 0
	}
	no := func(why string) { r.Analysis = "没启动 " + a.HeadlessWord(a.Cfg.Backend) + " 分析：" + why }
	lvl, _, _ := backend.UsageLevel(st.Usage, now, a.Cfg.UsagePolicy())
	switch {
	case !a.Cfg.PatrolEnabled:
		no("patrol_enabled=false")
		return
	case ps.AnalysisCount >= patrolAnalyzeMax:
		no(fmt.Sprintf("今天已经分析过 %d 次（每天最多 %d 次）", ps.AnalysisCount, patrolAnalyzeMax))
		return
	case a.Cfg.Paused:
		no("已暂停（paused=true）")
		return
	case now.Before(st.CooldownUntil):
		no("用量冷却中，到 " + textfmt.WhenCN(st.CooldownUntil))
		return
	case now.Before(st.BreakerUntil):
		no("连环失败熔断中，到 " + textfmt.WhenCN(st.BreakerUntil))
		return
	case lvl >= backend.UsageStop:
		no("订阅额度到了后台上限")
		return
	}
	for _, j := range st.Running() {
		if j.Kind == state.KindPatrol {
			no("上一次分析还在跑（任务 " + j.ID + "）")
			return
		}
	}
	launch := a.AnalyzePatrol
	if launch == nil {
		launch = a.startPatrolJob
	}
	j, err := launch(st, patrolPrompt(issues))
	if err != nil {
		no(err.Error())
		return
	}
	ps.AnalysisCount++
	r.AnalysisJobID = j.ID
	r.Analysis = fmt.Sprintf("已启动一次 %s 只读分析 %d 个新异常的原因（任务 %s，今天第 %d/%d 次），结果在它结束时写进当天汇总",
		a.HeadlessWord(a.Cfg.Backend), len(issues), j.ID, ps.AnalysisCount, patrolAnalyzeMax)
}

// startPatrolJob 经任务外壳启动分析任务（和夜班、复盘一样记在 state.json，不占并发名额）。
func (a *Service) startPatrolJob(st *state.State, prompt string) (*state.Job, error) {
	if ok, why := a.Runner.CheckReady(backend.MustPatrol); !ok { // 分析要只读：后端做不到就不起，巡逻照常报
		return nil, errors.New(why)
	}
	dir := a.ProjectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = a.Home
	}
	j := &state.Job{Kind: state.KindPatrol, Project: PatrolJobName, ProjectDir: dir, Item: "agentq patrol", Title: "agentq patrol 分析"}
	return a.Runner.Start(st, j, prompt, "")
}

const PatrolPromptHead = "你由 agentq 巡逻在无人值守模式下用 claude -p 启动，只做只读分析：不改任何文件、不提交、不推送、不启动或停止任何服务和任务" +
	"（回合一结束进程就退出，不用后台命令和「等通知」）。下面是这次巡逻新发现的异常和证据。逐条查原因（可以读日志、配置、" +
	"git 历史，跑只读命令），每条写一句原因和建议怎么修（能自动派活修的写派什么活，要用户做的写用户做什么）；查不出就写查不出和下一步该看哪里。" +
	"整个回答不超过 250 字，不用标题，不复述证据。\n\n"

func patrolPrompt(issues []PatrolIssue) string {
	var b strings.Builder
	b.WriteString(PatrolPromptHead)
	for i, is := range issues {
		fmt.Fprintf(&b, "%d. %s\n", i+1, is.Title)
		for _, e := range is.Evidence {
			fmt.Fprintf(&b, "   证据：%s\n", e)
		}
	}
	return b.String()
}

func (is PatrolIssue) nightlyText() string {
	var ev []string
	for _, e := range is.Evidence {
		ev = append(ev, textfmt.OneLine(e, 200))
	}
	s := is.Title + "；证据：" + strings.Join(ev, "；")
	if is.Action != "" {
		s += "；处理：" + is.Action
	}
	return s
}

func (r *PatrolReport) logLine() string {
	if len(r.New)+len(r.Old) == 0 {
		s := "巡逻：没有发现问题"
		if len(r.Gone) > 0 {
			s += fmt.Sprintf("（已恢复或已处理 %d 个）", len(r.Gone))
		}
		return s + r.tail()
	}
	var t []string
	for _, is := range r.New {
		t = append(t, textfmt.TruncRunes(is.Title, 40))
	}
	s := fmt.Sprintf("巡逻：现有 %d 个问题（新 %d 个，已恢复或已处理 %d 个）", len(r.New)+len(r.Old), len(r.New), len(r.Gone))
	if len(t) > 0 {
		s += "；新的：" + strings.Join(t, "；")
	}
	return s + r.tail()
}

func (r *PatrolReport) tail() string {
	s := ""
	if r.Analysis != "" {
		s += "；" + r.Analysis
	}
	if len(r.Notes) > 0 {
		s += "；检查没做成：" + strings.Join(r.Notes, "；")
	}
	return s
}

// print 是 agentq patrol 的输出：新问题、之前报过还在的、已恢复的，各带证据和处理。
func (r *PatrolReport) print(w io.Writer, patrolPath, nightly string) {
	fmt.Fprintf(w, "巡逻 %s：现有 %d 个问题（新 %d 个，之前报过 %d 个），已恢复或已处理 %d 个\n",
		r.At.Format("2006-01-02 15:04"), len(r.New)+len(r.Old), len(r.New), len(r.Old), len(r.Gone))
	section := func(head string, list []PatrolIssue, since bool) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", head)
		for _, is := range list {
			extra := ""
			if since {
				extra = "（" + textfmt.WhenCN(is.FirstSeen) + " 首次发现）"
			}
			fmt.Fprintf(w, "  - %s%s\n", is.Title, extra)
			for _, e := range is.Evidence {
				fmt.Fprintf(w, "      证据：%s\n", e)
			}
			if is.Action != "" {
				fmt.Fprintf(w, "      处理：%s\n", is.Action)
			}
		}
	}
	section("新问题（已写进当天汇总；疑似卡住、额度由 tick 写过，不重复写）：", r.New, false)
	section("之前报过、还没消失（不重复写汇总）：", r.Old, true)
	if len(r.Gone) > 0 {
		fmt.Fprintln(w, "\n已恢复或已处理：")
		for _, is := range r.Gone {
			fmt.Fprintf(w, "  - %s：%s（%s 首次发现）\n", is.gone, is.Title, textfmt.WhenCN(is.FirstSeen))
		}
	}
	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "\n这些检查没做成（那一类上次的问题照旧记着）：")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	if r.Analysis != "" {
		fmt.Fprintf(w, "\n分析：%s\n", r.Analysis)
	}
	fmt.Fprintf(w, "\n记录：%s；当天汇总：%s\n", patrolPath, nightly)
}
