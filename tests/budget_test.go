package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backend/claude"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/patrol"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
)

func TestBudgetArgs(t *testing.T) {
	for in, want := range map[float64]string{0: "", -1: "", 150: "--max-budget-usd 150", 7.5: "--max-budget-usd 7.5"} {
		if got := strings.Join(claude.BudgetArgs(in), " "); got != want {
			t.Errorf("%v：%q", in, got)
		}
	}
	a := wireApp(&App{Env: &env.Env{Cfg: config.Default()}})
	for kind, want := range map[string]float64{"": 150, state.KindNew: 150, config.KindNight: 10, config.KindReview: 15, state.KindPatrol: 2, state.KindChat: 0, state.KindQuick: 0, state.KindFollowup: 0, state.KindRouter: 0} {
		if got := a.Runner.JobBudget(kind); got != want {
			t.Errorf("%q：%v", kind, got)
		}
	}
}

// 各种任务实际带上的参数：待做、夜班、复盘、任务箱、巡逻分析有预算；对话不带；0 不传。
func TestIntegrationBudgetArgs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"job_budget_usd": 33.5, "review_budget_usd": 4})
	testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— x\n")
	e.mustRun("tick")
	e.mustRun("night")
	e.mustRun("review")
	log := func(j *Job) string { waitExit(t, j.PID, 10*time.Second); return e.fakeLogOf(j) }
	if got := log(e.jobByTitle("甲")); !strings.Contains(got, "ARG[--max-budget-usd]\nARG[33.5]") {
		t.Errorf("待做：\n%s", got)
	}
	if got := log(e.routineJob(config.KindNight)); !strings.Contains(got, "ARG[--max-budget-usd]\nARG[10]") {
		t.Errorf("夜班：\n%s", got)
	}
	if got := log(e.routineJob(config.KindReview)); !strings.Contains(got, "ARG[--max-budget-usd]\nARG[4]") {
		t.Errorf("复盘：\n%s", got)
	}

	off := newEnv(t, map[string]any{"job_budget_usd": 0, "night_budget_usd": 0})
	testkit.MkProject(t, off.root, "demo", "- [ ] 甲 —— x\n")
	off.mustRun("tick")
	off.mustRun("night")
	if got := log2(t, off, off.jobByTitle("甲")); strings.Contains(got, "--max-budget-usd") {
		t.Errorf("0 不传：\n%s", got)
	}
	if got := log2(t, off, off.routineJob(config.KindNight)); strings.Contains(got, "--max-budget-usd") {
		t.Errorf("0 不传：\n%s", got)
	}
}

// 巡逻分析撞预算：不算失败、不计连环失败，写进汇总。回答类任务不走这条路：还是回「没答完」（见 TestRouterNoStructuredOutput）。
func TestPatrolBudgetHit(t *testing.T) {
	a, _ := newTestApp(t)
	a.Cfg.NotifyEnabled = false
	now := time.Now()
	j := &Job{ID: "1104-120000-巡逻分析", Kind: state.KindPatrol, Project: patrol.PatrolJobName, Title: "agentq patrol 分析", Status: state.StatusRunning, StartedAt: now}
	if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.JobDir(j.ID), "result.json"), []byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{j}, FailStreak: 2}
	a.Runner.FinishFromResult(st, j)
	if j.Status != state.StatusInterrupted || !j.BudgetHit || j.ResumePending || !strings.Contains(j.Summary, "撞到预算上限（$2，失控兜底），不算失败") {
		t.Errorf("%+v", j)
	}
	if st.FailStreak != 2 || !st.BreakerUntil.IsZero() {
		t.Errorf("不计连环失败，也不清零：%+v", st)
	}
	if b, err := os.ReadFile(a.NightlyPath(now)); err != nil || !strings.Contains(string(b), "撞到预算上限") {
		t.Errorf("汇总：%v %s", err, b)
	}
}

func log2(t *testing.T, e *testEnv, j *Job) string {
	t.Helper()
	waitExit(t, j.PID, 10*time.Second)
	return e.fakeLogOf(j)
}

// 撞预算：记中断、保留分支和会话、不算失败；同一次 tick 用同一会话续跑一次（有一整份预算），提示词说明情况；通知用户；做完进已完成。
func TestIntegrationBudgetResume(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false})
	e.atNoon()
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— TASKOVERBUDGET\n")
	e.mustRun("tick")
	first := e.jobByTitle("甲")
	waitExit(t, first.PID, 10*time.Second)
	out := e.mustRun("tick")
	if !strings.Contains(out, "续跑任务") {
		t.Fatalf("应续跑：\n%s", out)
	}
	jobs := e.state().Jobs
	if len(jobs) != 2 {
		t.Fatalf("%+v", jobs)
	}
	first, second := jobs[0], jobs[1]
	if first.Status != state.StatusInterrupted || !first.BudgetHit || first.ResumedBy != second.ID || first.LimitHit || first.Transient ||
		!strings.Contains(first.Summary, "撞到预算上限（$150，失控兜底），分支和会话都留着；将自动用同一会话续跑一次") {
		t.Fatalf("%+v", first)
	}
	log := log2(t, e, second)
	for _, want := range []string{"ARG[-r]\nARG[s-over]", "ARG[--max-budget-usd]\nARG[150]", "撞到预算上限（失控兜底，不是你做错了）", "尽快收口", "TASKOVERBUDGET"} {
		if !strings.Contains(log, want) {
			t.Errorf("续跑缺 %q：\n%s", want, log)
		}
	}
	if st := e.state(); st.FailStreak != 0 {
		t.Errorf("不计连环失败：%d", st.FailStreak)
	}
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "撞到预算上限，会接着做一次") || !strings.Contains(got, "分支和会话都留着") {
		t.Errorf("要通知：%s", got)
	}
	e.mustRun("tick")
	if j := e.state().Jobs[1]; j.Status != state.StatusDone || j.ResumeOf != first.ID {
		t.Errorf("%+v", j)
	}
	if b := e.backlogOf(dir); strings.Contains(b, "卡住") || !strings.Contains(b, "- [x] 甲") {
		t.Errorf("\n%s", b)
	}
	if out := e.mustRun("logs", first.ID); !strings.Contains(out, "已续跑") {
		t.Errorf("%s", out)
	}
}

// 续跑再撞才写卡住：去掉进行中标记、分支留着、提交了、通知；之后不再自动做。
func TestIntegrationBudgetTwiceStuck(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false})
	e.atNoon()
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— TASKOVERTWICE\n")
	e.mustRun("tick")
	first := e.jobByTitle("甲")
	waitExit(t, first.PID, 10*time.Second)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "认领") // agent-task 认领时会提交
	e.mustRun("tick")
	second := e.state().Jobs[1]
	waitExit(t, second.PID, 10*time.Second)
	y, m, d := time.Now().Date()
	noon := time.Date(y, m, d, 12, 0, 0, 0, time.Local)
	out := e.tickAt(noon, notify.NotifyMergeWindow+time.Minute)
	if !strings.Contains(out, "启动 0") {
		t.Fatalf("不该再启动：\n%s", out)
	}
	second = e.state().Jobs[1]
	if second.Status != state.StatusInterrupted || second.ResumePending || !second.BudgetHit || second.StuckPending != "" {
		t.Fatalf("%+v", second)
	}
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（卡住：续跑后又撞预算上限 $150，任务 "+second.ID+"，分支 agent/t-over）") || strings.Contains(b, "（进行中") {
		t.Fatalf("\n%s", b)
	}
	if st := testkit.GitT(t, e.home, dir, "status", "--porcelain"); st != "" {
		t.Errorf("BACKLOG 应已提交：%s", st)
	}
	got := strings.Join(tg.texts(), "\n")
	for _, want := range []string{"续跑后又撞预算上限，放下了", "分支 agent/t-over 留着", "回复这条消息，接回原会话和分支", "去掉卡住标记"} {
		if !strings.Contains(got, want) {
			t.Errorf("通知缺 %q：%s", want, got)
		}
	}
	if out := e.tickAt(noon, 72*time.Hour); !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 2 {
		t.Errorf("不接手、不重做：\n%s", out)
	}
}

// 条目还没认领就撞预算（不在进行中）：不续跑，等 retry_after，不算失败。
func TestIntegrationBudgetNotClaimed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"breaker_streak": 1})
	e.extra = append(e.extra, "AGENTQ_FAKE_BUDGET=1")
	orphanRepo(t, e, "demo", "- [ ] 甲 —— x\n")
	base := time.Now()
	e.tickAt(base, 0)
	j := e.jobByTitle("甲")
	waitExit(t, j.PID, 10*time.Second)
	if out := e.tickAt(base, time.Minute); !strings.Contains(out, "启动 0") {
		t.Fatalf("\n%s", out)
	}
	j = e.jobByTitle("甲")
	if j.Status != state.StatusInterrupted || j.ResumePending || !strings.Contains(j.Summary, "条目已不在进行中，不续跑") {
		t.Errorf("%+v", j)
	}
	st := e.state()
	if !st.BreakerUntil.IsZero() || st.FailStreak != 0 {
		t.Errorf("秒退撞预算也不计连环失败：%+v", st)
	}
	if out := e.tickAt(base, 13*time.Hour); !strings.Contains(out, "启动 1") {
		t.Errorf("过了 retry_after 再做：\n%s", out)
	}
	jobs := e.state().Jobs
	waitExit(t, jobs[len(jobs)-1].PID, 10*time.Second)
}

// 夜班、复盘撞预算：不算失败、推一条、巡逻不再起分析；下次定点照跑（上次没跑完不会被跳过）。
func TestIntegrationRoutineBudget(t *testing.T) {
	t.Parallel()
	e, _, day1 := idleEnv(t, config.KindNight)
	sec := filepath.Join(e.home, ".agentq", "secrets")
	os.MkdirAll(sec, 0o700)
	tg := newFakeTG(t)
	os.WriteFile(filepath.Join(sec, "telegram.env"), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)), 0o600)
	cfg, err := config.Load(filepath.Join(e.home, ".agentq", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.NotifyEnabled = true
	if err := config.Save(filepath.Join(e.home, ".agentq", "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	e.extra = append(e.extra, "AGENTQ_TELEGRAM_API="+tg.srv.URL)
	saved := e.extra
	e.extra = append(append([]string(nil), saved...), "AGENTQ_FAKE_BUDGET=1")
	noon := day1.Add(11 * time.Hour)
	e.runAt(noon, "night", "--scheduled")
	j := e.routineJob(config.KindNight)
	waitExit(t, j.PID, 10*time.Second)
	e.extra = saved
	e.runAt(noon.Add(time.Minute), "tick")
	j = e.routineJob(config.KindNight)
	if j.Status != state.StatusInterrupted || !j.BudgetHit || j.ResumePending || !strings.Contains(j.Summary, "撞到预算上限（$10，失控兜底），不算失败；不接着做，下次到点照跑") {
		t.Fatalf("%+v", j)
	}
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "撞到预算上限") {
		t.Errorf("要报告：%s", got)
	}
	a := wireApp(&App{Env: &env.Env{Home: e.home, Cfg: cfg, Now: func() time.Time { return noon }, Out: os.Stderr}})
	if issues := a.Patrol.CheckJobs(e.state(), noon); len(issues) != 0 {
		t.Errorf("已经报过，巡逻不再报：%+v", issues)
	}
	e.assertRuns(config.KindNight, day1.AddDate(0, 0, 1), "上次夜班没跑完")
}

// new 撞预算：接着做一次，认的目录范围不变，做完正常推送；接着做之前 tick 不抢它的项目。再撞不再做。
func TestIntegrationNewBudget(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false, "max_parallel": 1, "discuss_enabled": false})
	e.atNoon()
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n"), 0o644)
	e.mustRun("new", "做个剪贴板工具 NEWOVER")
	var first *Job
	for _, j := range e.state().Jobs {
		first = j
	}
	waitExit(t, first.PID, 10*time.Second)
	e.mustRun("tick")
	jobs := e.state().Jobs
	if len(jobs) != 2 || !jobs[0].BudgetHit || jobs[0].ResumedBy != jobs[1].ID || jobs[1].Kind != state.KindNew || jobs[1].ResumeOf != jobs[0].ID || !slicesEq(jobs[1].ProjectsBefore, jobs[0].ProjectsBefore) {
		t.Fatalf("%+v", jobs)
	}
	log := log2(t, e, jobs[1])
	for _, want := range []string{"ARG[-r]\nARG[s-over]", "撞到预算上限（失控兜底，不是你做错了）", newproj.NewIdeaBegin + "\n做个剪贴板工具 NEWOVER\n" + newproj.NewIdeaEnd, "ARG[--max-budget-usd]\nARG[150]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "新项目撞到预算上限，会接着做一次") {
		t.Errorf("%s", got)
	}
	e.mustRun("tick")
	if j := e.state().Jobs[1]; j.Status != state.StatusDone || j.NewReport == nil {
		t.Errorf("%+v", j)
	}
	e.waitGate(e.state().Jobs[1])

	// 再撞：不再做
	e2 := newEnv(t, map[string]any{"max_parallel": 1})
	e2.extra = append(e2.extra, "AGENTQ_FAKE_BUDGET=1")
	e2.mustRun("new", "另一个工具")
	first2 := e2.state().Jobs[0]
	waitExit(t, first2.PID, 10*time.Second)
	e2.mustRun("tick")
	second2 := e2.state().Jobs[1]
	waitExit(t, second2.PID, 10*time.Second)
	e2.mustRun("tick")
	if j := e2.state().Jobs; len(j) != 2 || j[1].ResumePending || !strings.Contains(j[1].Summary, "接着做之后又撞") {
		t.Errorf("%+v", j)
	}
}

func slicesEq(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

// 等着接着做的 new：名额满时 tick 也不抢它建的项目。
func TestNewProjectBusyWhilePending(t *testing.T) {
	st := &State{Jobs: []*Job{{Kind: state.KindNew, Status: state.StatusInterrupted, ResumePending: true, ProjectsBefore: []string{"old"}}}}
	if !newproj.NewProjectBusy(st, "fresh") || newproj.NewProjectBusy(st, "old") {
		t.Error("等着接着做时也算在建")
	}
	st.Jobs[0].ResumePending = false
	if newproj.NewProjectBusy(st, "fresh") {
		t.Error("结束了就不算")
	}
}

// runawayEnv：一个真 git 项目、跑着的任务（已认领、BACKLOG 提交了）、25 个子代理日志。到 tickAt(noon, 31m) 时命中「子代理失控」。
func runawayEnv(t *testing.T, cfg map[string]any, todo string) (*testEnv, *fakeTG, string, string, *Job, time.Time) {
	t.Helper()
	full := map[string]any{"paused": false, "timeout": "1000h"}
	for k, v := range cfg {
		full[k] = v
	}
	e, tg := threadEnv(t, full)
	e.atNoon()
	dir := orphanRepo(t, e, "demo", todo)
	e.mustRun("tick")
	j := e.jobByTitle("甲")
	waitFor(t, 10*time.Second, "认领写下标记", func() bool { return strings.Contains(e.backlogOf(dir), "（进行中") })
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "认领")
	wt := gitBranch(t, e, dir, "agent/t-cf")
	writeRunawayLogs(t, e, j)
	y, m, d := time.Now().Date()
	return e, tg, dir, wt, j, time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}

func writeRunawayLogs(t *testing.T, e *testEnv, j *Job) {
	t.Helper()
	at := j.StartedAt.Add(time.Minute)
	base := filepath.Join(e.home, ".claude", "projects", "-fake", j.SessionID)
	writeSession(t, base+".jsonl", []backend.ToolEvent{{At: at, Name: "Read"}})
	for i := range backend.RunawayLogs {
		writeSession(t, filepath.Join(base, "subagents", fmt.Sprintf("agent-%d.jsonl", i)), []backend.ToolEvent{{At: at, Name: "Read"}})
	}
}

// 子代理失控：停掉进程组、记中断（不是用户 stop）、写卡住标记并提交、分支和 worktree 留着、通知、不计连环失败。
// 之后不接手、不重做；去掉标记后照常（不会像用户停的那样一直不动）。
func TestIntegrationRunawayAutoStop(t *testing.T) {
	t.Parallel()
	e, tg, dir, wt, j, noon := runawayEnv(t, map[string]any{"breaker_streak": 1}, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	out := e.tickAt(noon, 31*time.Minute)
	got := e.jobByTitle("甲")
	if got.Status != state.StatusInterrupted || got.AutoStop != backend.SigRunaway || got.ResumePending || got.StuckPending != "" || !strings.Contains(got.Summary, "子代理失控，子代理 25 个，其中 0 个再派子代理，已自动停止") {
		t.Fatalf("%+v\n%s", got, out)
	}
	if got.Status == state.StatusKilled {
		t.Fatal("不能算用户 stop")
	}
	waitFor(t, 5*time.Second, "进程组退出", func() bool { return syscall.Kill(-j.PID, 0) != nil })
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（卡住：子代理失控，子代理 25 个，其中 0 个再派子代理，任务 "+j.ID+"，分支 agent/t-cf）") || strings.Contains(b, "（进行中") {
		t.Fatalf("\n%s", b)
	}
	if st := testkit.GitT(t, e.home, dir, "status", "--porcelain"); st != "" {
		t.Errorf("BACKLOG 应已提交：%s", st)
	}
	if _, err := os.Stat(filepath.Join(wt, "work.txt")); err != nil {
		t.Error("worktree 要留着")
	}
	if b := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-cf"); b == "" {
		t.Error("分支要留着")
	}
	st := e.state()
	if !st.BreakerUntil.IsZero() || st.FailStreak != 0 {
		t.Errorf("不计连环失败：%+v", st)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "子代理失控，自动停了") {
		t.Errorf("要通知：%q", tg.texts())
	}
	for _, want := range []string{"分支 agent/t-cf、worktree " + testkit.RealPath(t, wt) + " 留着", "回复这条消息，接回原会话和分支", "去掉卡住标记，agent 从分支现状接着做"} {
		if !strings.Contains(strings.Join(tg.texts(), "\n"), want) {
			t.Errorf("通知缺 %q：%q", want, tg.texts())
		}
	}
	if n := readNightly(e, noon); !strings.Contains(n, "子代理失控") {
		t.Errorf("汇总：%s", n)
	}
	// 不接手、不重做、不列成「用户停的」
	if out := e.tickAt(noon, 72*time.Hour); !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 1 {
		t.Errorf("\n%s", out)
	}
	if out := e.mustRun("status"); strings.Contains(out, "已停止：") {
		t.Errorf("不是用户停的：\n%s", out)
	}
	a := wireApp(&App{Env: &env.Env{Home: e.home, Cfg: config.Default(), Now: time.Now, Out: os.Stderr}})
	if issues := a.Patrol.CheckJobs(e.state(), e.state().Jobs[0].EndedAt.Add(time.Minute)); len(issues) != 0 {
		t.Errorf("巡逻不再报、不起分析：%+v", issues)
	}
	// 用户去掉卡住标记：过了 retry_after 照常做
	text := strings.Replace(e.backlogOf(dir), "（卡住：子代理失控，子代理 25 个，其中 0 个再派子代理，任务 "+j.ID+"，分支 agent/t-cf）", "", 1)
	text = strings.Replace(text, "CLAIMFIRST SLEEP=120", "x", 1)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "去掉卡住")
	if out := e.tickAt(noon, 96*time.Hour); !strings.Contains(out, "启动 1") {
		t.Errorf("\n%s", out)
	}
	jobs := e.state().Jobs
	waitExit(t, jobs[len(jobs)-1].PID, 10*time.Second)
}

// 开关关掉：和原来一样只报不停，文字也不变。
func TestIntegrationRunawayAutostopOff(t *testing.T) {
	t.Parallel()
	e, tg, dir, _, j, noon := runawayEnv(t, map[string]any{"progress_autostop": false}, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	out := e.tickAt(noon, 31*time.Minute)
	got := e.jobByTitle("甲")
	if got.Status != state.StatusRunning || got.AutoStop != "" || !runner.Noted(got, backend.SigRunaway) {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(out, "不自动杀，运行满 1000h 才终止，要停就 agentq stop "+j.ID) {
		t.Errorf("文字不变：\n%s", out)
	}
	if syscall.Kill(j.PID, 0) != nil {
		t.Error("不该杀")
	}
	if strings.Contains(e.backlogOf(dir), "卡住") {
		t.Error("不该标卡住")
	}
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "子代理失控") || !strings.Contains(got, "要停就 agentq stop") {
		t.Errorf("照报：%s", got)
	}
	e.mustRun("stop", j.ID)
}

// 开关开着：其他信号（无改动）仍只报不停。
func TestIntegrationOtherSignalsOnlyReport(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false, "timeout": "1000h"})
	e.atNoon()
	testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— SLEEP=120\n")
	e.mustRun("tick")
	j := e.jobByTitle("甲")
	at := j.StartedAt.Add(time.Minute)
	main := filepath.Join(e.home, ".claude", "projects", "-fake", j.SessionID+".jsonl")
	writeSession(t, main, []backend.ToolEvent{{At: at, Name: "Read"}})
	y, m, d := time.Now().Date()
	out := e.tickAt(time.Date(y, m, d, 12, 0, 0, 0, time.Local), 91*time.Minute)
	got := e.jobByTitle("甲")
	if got.Status != state.StatusRunning || !runner.Noted(got, backend.SigNoChange) || !strings.Contains(out, "不自动杀") {
		t.Errorf("%+v\n%s", got, out)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "无改动") {
		t.Errorf("%q", tg.texts())
	}
	e.mustRun("stop", j.ID)
}

// 主目录提交不了（BACKLOG 有改动）：任务照样停，卡住标记下次 tick 再写；这期间不接手、不重挑；改动提交后写上。
func TestIntegrationRunawayStuckRetry(t *testing.T) {
	t.Parallel()
	e, tg, dir, _, j, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\n用户正在改\n")
	f.Close()
	out := e.tickAt(noon, 31*time.Minute)
	got := e.jobByTitle("甲")
	if got.Status != state.StatusInterrupted || got.StuckPending == "" || !strings.Contains(out, "卡住标记没写上") {
		t.Fatalf("%+v\n%s", got, out)
	}
	if syscall.Kill(j.PID, 0) == nil {
		waitExit(t, j.PID, 5*time.Second)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "卡住标记还没写上") {
		t.Errorf("%q", tg.texts())
	}
	// 72 小时后还是写不上：不接手
	if out := e.tickAt(noon, 72*time.Hour); !strings.Contains(out, "启动 0") || strings.Contains(out, "接手任务") {
		t.Errorf("\n%s", out)
	}
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "用户改完了")
	out = e.tickAt(noon, 73*time.Hour)
	if got := e.jobByTitle("甲"); got.StuckPending != "" || !strings.Contains(e.backlogOf(dir), "（卡住：子代理失控") {
		t.Errorf("%+v\n%s\n%s", got, out, e.backlogOf(dir))
	}
	if len(e.state().Jobs) != 1 {
		t.Errorf("%+v", e.state().Jobs)
	}
}

// 任务箱任务失控：inbox.md 标卡住，推「放下了」。不写项目 BACKLOG。
func TestIntegrationRunawayInbox(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false, "timeout": "1000h"})
	e.atNoon()
	e.writeInbox("## 待做\n- [ ] 甲 —— SLEEP=120\n")
	e.mustRun("tick")
	j := inboxJobs(e)[0]
	writeRunawayLogs(t, e, j)
	y, m, d := time.Now().Date()
	e.tickAt(time.Date(y, m, d, 12, 0, 0, 0, time.Local), 31*time.Minute)
	got := inboxJobs(e)[0]
	if got.Status != state.StatusInterrupted || got.AutoStop != backend.SigRunaway {
		t.Fatalf("%+v", got)
	}
	inbox := e.readInbox()
	if !strings.Contains(inbox, "（卡住：任务 "+j.ID+" 子代理失控，自动停了") || strings.Contains(inbox, "（进行中") {
		t.Errorf("\n%s", inbox)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "放下了") {
		t.Errorf("%q", tg.texts())
	}
}

// new 任务失控：多出的目录和用户停掉时一样不自动做，推一条；不算用户停的（用户停的不推）。
func TestIntegrationRunawayNew(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false, "timeout": "1000h", "discuss_enabled": false})
	e.atNoon()
	e.mustRun("new", "做个工具 SLEEP=120")
	j := e.state().Jobs[0]
	orphanRepo(t, e, "half", "- [ ] 第二条 —— x\n")
	writeRunawayLogs(t, e, j)
	y, m, d := time.Now().Date()
	out := e.tickAt(time.Date(y, m, d, 12, 0, 0, 0, time.Local), 31*time.Minute)
	got := e.state().Jobs[0]
	if got.Status != state.StatusInterrupted || got.AutoStop != backend.SigRunaway || !slicesEq(got.HeldProjects, []string{"half"}) || !strings.Contains(got.Summary, "不会自动做") {
		t.Fatalf("%+v\n%s", got, out)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "新项目子代理失控，自动停了") {
		t.Errorf("%q", tg.texts())
	}
	if len(e.state().Jobs) != 1 {
		t.Errorf("不该挑它的待做：%+v", e.state().Jobs)
	}
}

// 夜班、对话这些不看进展，失控信号碰不到它们。
func TestRunawayOnlyTasks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_timeout": "1000h"})
	e.extra = append(e.extra, "AGENTQ_FAKE_SLEEP=120")
	e.mustRun("night")
	j := e.routineJob(config.KindNight)
	writeRunawayLogs(t, e, j)
	out := e.tickAt(time.Now(), 31*time.Minute)
	if got := e.routineJob(config.KindNight); got.Status != state.StatusRunning || got.AutoStop != "" {
		t.Errorf("%+v\n%s", got, out)
	}
	e.mustRun("stop", j.ID)
}

func modelOf(t *testing.T, e *testEnv, j *Job) string {
	t.Helper()
	log := log2(t, e, j)
	_, rest, ok := strings.Cut(log, "ARG[--model]\nARG[")
	if !ok {
		t.Fatalf("没有 --model：\n%s", log)
	}
	m, _, _ := strings.Cut(rest, "]")
	return m
}

// 配了 low_risk_model：带固定标记的用它；正文提到的不算；别的不变。没配：标记不起作用，参数和原来一样。
func TestIntegrationLowRiskModel(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"low_risk_model": "haiku", "max_parallel": 3})
	testkit.MkProject(t, e.root, "a", "- [ ] 补测试（低风险） —— x\n")
	testkit.MkProject(t, e.root, "b", "- [ ] 改解析 —— 支持（低风险）标记\n")
	e.mustRun("tick")
	e.mustRun("night")
	if got := modelOf(t, e, e.jobByTitle("补测试（低风险）")); got != "haiku" {
		t.Errorf("带标记：%s", got)
	}
	if got := modelOf(t, e, e.jobByTitle("改解析")); got != "opus" {
		t.Errorf("正文提到不算：%s", got)
	}
	if got := modelOf(t, e, e.routineJob(config.KindNight)); got != "opus" {
		t.Errorf("夜班不看标记：%s", got)
	}

	def := newEnv(t, nil)
	testkit.MkProject(t, def.root, "a", "- [ ] 补测试（低风险） —— x\n")
	def.mustRun("tick")
	if got := modelOf(t, def, def.jobByTitle("补测试（低风险）")); got != "sonnet" {
		t.Errorf("没写这个键用默认的 sonnet：%s", got)
	}

	off := newEnv(t, map[string]any{"low_risk_model": ""})
	testkit.MkProject(t, off.root, "a", "- [ ] 补测试（低风险） —— x\n")
	off.mustRun("tick")
	j := off.jobByTitle("补测试（低风险）")
	if got := modelOf(t, off, j); got != "opus" {
		t.Errorf("写成空照 model：%s", got)
	}
}

// 续跑和接手沿用同样判断。
func TestIntegrationLowRiskResumeAndTakeover(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"low_risk_model": "haiku"})
	orphanRepo(t, e, "demo", "- [ ] （低风险）补测试 —— TASKOVERBUDGET\n")
	e.mustRun("tick")
	first := e.state().Jobs[0]
	if got := modelOf(t, e, first); got != "haiku" {
		t.Errorf("首次：%s", got)
	}
	e.mustRun("tick")
	second := e.state().Jobs[1]
	if second.ResumeOf != first.ID {
		t.Fatalf("%+v", second)
	}
	if got := modelOf(t, e, second); got != "haiku" {
		t.Errorf("续跑：%s", got)
	}

	// 接手：条目上有进行中标记，标题去掉标记后判断
	t2 := newEnv(t, map[string]any{"low_risk_model": "haiku"})
	d2 := orphanRepo(t, t2, "demo", "- [ ] （低风险）补测试 —— ORPHANDONE"+oldMarker("agent/x")+"\n")
	gitBranch(t, t2, d2, "agent/x")
	t2.tickAt(time.Now().Add(72*time.Hour), 0)
	j := t2.state().Jobs[0]
	if !j.Takeover {
		t.Fatalf("%+v", j)
	}
	if got := modelOf(t, t2, j); got != "haiku" {
		t.Errorf("接手：%s", got)
	}
	// 接手没标记的：不用便宜模型
	t3 := newEnv(t, map[string]any{"low_risk_model": "haiku"})
	d3 := orphanRepo(t, t3, "demo", "- [ ] 补测试 —— ORPHANDONE 提到（低风险）这个词"+oldMarker("agent/x")+"\n")
	gitBranch(t, t3, d3, "agent/x")
	t3.tickAt(time.Now().Add(72*time.Hour), 0)
	if got := modelOf(t, t3, t3.state().Jobs[0]); got != "opus" {
		t.Errorf("接手没标记：%s", got)
	}
}

// noticeThreadOf 找任务那条提醒的消息 id。
func noticeThreadOf(t *testing.T, e *testEnv, jobID string) int64 {
	t.Helper()
	for id, th := range e.readNotify().Threads {
		if th.JobID == jobID {
			return id
		}
	}
	t.Fatalf("任务 %s 的提醒没登记回复链：%+v", jobID, e.readNotify().Threads)
	return 0
}

func committedBacklog(t *testing.T, e *testEnv, dir string) string {
	t.Helper()
	return testkit.GitT(t, e.home, dir, "show", "HEAD:docs/BACKLOG.md")
}

// 自动停后用户回复提醒：写模式接回原会话和原分支，卡住标记换回进行中；失控信号重新计数：旧日志不算，一接回不会又被停，新的失控照样停。
func TestIntegrationStuckReplyResumes(t *testing.T) {
	t.Parallel()
	e, tg, dir, wt, j, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	e.tickAt(noon, 31*time.Minute)
	id := noticeThreadOf(t, e, j.ID)
	tg.replyMsg(1, 11, "接着做，先把测试跑过", id)
	e.tickAt(noon, 32*time.Minute)
	jobs := e.state().Jobs
	if len(jobs) != 2 {
		t.Fatalf("应接回：%+v", jobs)
	}
	next := jobs[1]
	if next.Status != state.StatusRunning || next.ResumeOf != j.ID || next.SessionID != j.SessionID || next.AutoStop != "" || next.Kind != "" {
		t.Fatalf("%+v", next)
	}
	if b := committedBacklog(t, e, dir); !strings.Contains(b, "（进行中：agent/t-cf ") || strings.Contains(b, "（卡住") {
		t.Errorf("卡住标记要换回进行中并提交：\n%s", b)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "已接回任务 "+j.ID) {
		t.Errorf("%q", tg.texts())
	}
	waitFor(t, 10*time.Second, "假 claude 启动", func() bool {
		_, err := os.Stat(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", next.PID)))
		return err == nil
	})
	log := e.fakeLogOf(next)
	for _, want := range []string{"ARG[-r]\nARG[" + j.SessionID + "]", "ARG[--max-budget-usd]\nARG[150]", "因为子代理失控被 agentq 停了", "分支 agent/t-cf", "worktree " + testkit.RealPath(t, wt), "从现状接着做", "接着做，先把测试跑过"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	// 假 claude 像认领一样又写了一遍标记，提交掉，后面的 tick 才写得上卡住
	waitFor(t, 10*time.Second, "假 claude 写标记", func() bool { return strings.Count(e.backlogOf(dir), "（进行中") == 2 })
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "认领")
	// 接回 31 分钟后：旧的 25 个子代理日志还在，但都早于这次启动，不算
	e.tickAt(noon, 63*time.Minute)
	if got := e.state().Jobs[1]; got.Status != state.StatusRunning || runner.Noted(got, backend.SigRunaway) {
		t.Fatalf("旧日志不算，不该又被停：%+v", got)
	}
	// 这次自己又失控：照样停，再写卡住
	writeRunawayLogs(t, e, next)
	e.tickAt(noon, 64*time.Minute)
	got := e.state().Jobs[1]
	if got.Status != state.StatusInterrupted || got.AutoStop != backend.SigRunaway {
		t.Fatalf("新的失控要停：%+v", got)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：子代理失控，子代理 25 个，其中 0 个再派子代理，任务 "+next.ID+"，分支 agent/t-cf）") || strings.Contains(b, "（进行中") {
		t.Errorf("\n%s", b)
	}
}

// 接回受名额、额度限制；BACKLOG 提交不了时不接回。这些时候卡住标记不动。
func TestIntegrationStuckReplyGates(t *testing.T) {
	t.Parallel()
	e, tg, dir, _, j, noon := runawayEnv(t, map[string]any{"max_parallel": 1}, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	e.tickAt(noon, 31*time.Minute)
	id := noticeThreadOf(t, e, j.ID)
	stuck := func() {
		t.Helper()
		if b := committedBacklog(t, e, dir); !strings.Contains(b, "（卡住：子代理失控") || strings.Contains(b, "（进行中") {
			t.Fatalf("卡住标记不该动：\n%s", b)
		}
		if n := len(e.state().Jobs); n != 1 {
			t.Fatalf("不该起任务：%+v", e.state().Jobs)
		}
	}
	// BACKLOG 有没提交的改动
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(append([]byte(nil), orig...), []byte("\n用户正在改\n")...), 0o644)
	tg.replyMsg(1, 11, "接着做", id)
	e.tickAt(noon, 32*time.Minute)
	stuck()
	if got := tg.sentLast().Get("text"); !strings.Contains(got, "卡住标记换不回进行中") {
		t.Errorf("%s", got)
	}
	os.WriteFile(p, orig, 0o644)
	// 用量冷却
	e.editState(func(st *State) { st.CooldownUntil = noon.Add(time.Hour) })
	tg.replyMsg(2, 12, "接着做", id)
	e.tickAt(noon, 33*time.Minute)
	stuck()
	if got := tg.sentLast().Get("text"); !strings.Contains(got, "用量冷却中") {
		t.Errorf("%s", got)
	}
	e.editState(func(st *State) { st.CooldownUntil = time.Time{} })
	// 没名额：回排队了，有名额再接
	e.editState(func(st *State) {
		st.Jobs = append(st.Jobs, &Job{ID: "占名额", Project: "other", Status: state.StatusRunning, PID: os.Getpid(), StartedAt: noon})
	})
	sleeper := startGroup(t, nil, "/bin/bash", "-c", "exec -a 'agentq other x' /bin/sleep 60")
	e.editState(func(st *State) { st.Jobs[1].PID = sleeper })
	tg.replyMsg(3, 13, "接着做", id)
	e.tickAt(noon, 34*time.Minute)
	if b := committedBacklog(t, e, dir); !strings.Contains(b, "（卡住：子代理失控") {
		t.Fatalf("\n%s", b)
	}
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "排队了") {
		t.Errorf("%s", got)
	}
	if s := e.readNotify(); len(s.Inbox) != 1 {
		t.Errorf("消息要留着：%+v", s.Inbox)
	}
	// 名额空出来了：接回
	syscall.Kill(-sleeper, syscall.SIGKILL)
	waitFor(t, 5*time.Second, "占名额的进程退出", func() bool { return !proc.ProcAlive(sleeper, "agentq other") })
	e.tickAt(noon, 35*time.Minute)
	jobs := e.newJobs(j.ID, "占名额")
	if len(jobs) != 1 || jobs[0].ResumeOf != j.ID {
		t.Fatalf("%+v", e.state().Jobs)
	}
	waitFor(t, 10*time.Second, "假 claude 启动", func() bool {
		_, err := os.Stat(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", jobs[0].PID)))
		return err == nil
	})
	e.mustRun("stop", jobs[0].ID)
}

// 撞两次预算放下后回复：同样接回，又有一整份预算；再撞再放下。
func TestIntegrationBudgetStuckReplyResumes(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false})
	e.atNoon()
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— TASKOVERTWICE\n")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "认领")
	e.mustRun("tick")
	second := e.state().Jobs[1]
	waitExit(t, second.PID, 10*time.Second)
	y, m, d := time.Now().Date()
	noon := time.Date(y, m, d, 12, 0, 0, 0, time.Local)
	e.tickAt(noon, notify.NotifyMergeWindow+time.Minute)
	second = e.state().Jobs[1]
	if !second.StuckMarked {
		t.Fatalf("%+v", second)
	}
	e.seedSession(second.SessionID)
	tg.replyMsg(1, 11, "预算够了，接着做", noticeThreadOf(t, e, second.ID))
	e.tickAt(noon, notify.NotifyMergeWindow+2*time.Minute)
	jobs := e.state().Jobs
	if len(jobs) != 3 || jobs[2].ResumeOf != second.ID || jobs[2].SessionID != second.SessionID {
		t.Fatalf("%+v", jobs)
	}
	log := log2(t, e, jobs[2])
	for _, want := range []string{"ARG[-r]\nARG[" + second.SessionID + "]", "ARG[--max-budget-usd]\nARG[150]", "撞到预算上限（失控兜底，不是你做错了）", "预算够了，接着做"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	e.tickAt(noon, notify.NotifyMergeWindow+3*time.Minute)
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：续跑后又撞预算上限 $150，任务 "+jobs[2].ID) {
		t.Errorf("再撞再放下：\n%s", b)
	}
}

// 只认 agentq 自己写的固定标记：用户写的卡住、正文提到的，回复照旧走只读追问。
func TestIntegrationStuckReplyOwnerMark(t *testing.T) {
	t.Parallel()
	e, tg, dir, _, j, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	e.tickAt(noon, 31*time.Minute)
	id := noticeThreadOf(t, e, j.ID)
	var lines []string
	for _, l := range strings.Split(e.backlogOf(dir), "\n") {
		lines = append(lines, runner.AutoStuckRe.ReplaceAllString(l, "（卡住：我自己放下的）"))
	}
	text := strings.Join(lines, "\n")
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "用户写的卡住")
	tg.replyMsg(1, 11, "FUPREPLY 怎么回事", id)
	e.tickAt(noon, 32*time.Minute)
	jobs := e.state().Jobs
	if len(jobs) != 2 || jobs[1].Kind != state.KindFollowup {
		t.Fatalf("应走只读追问：%+v", jobs)
	}
	if strings.Contains(committedBacklog(t, e, dir), "（进行中") {
		t.Error("不该动标记")
	}
	log2(t, e, jobs[1])
}

// stuckResumable：标记写的任务对得上才接回。
func TestStuckResumable(t *testing.T) {
	a, _ := newTestApp(t)
	items := []backlog.Item{{Text: "甲（卡住：子代理失控，x，任务 t1）"}}
	th := notify.Thread{Kind: notify.ThreadJob, JobID: "t1", SessionID: "s", Fingerprint: "fp"}
	job := func() *Job { return &Job{ID: "t1", AutoStop: backend.SigRunaway, SessionID: "s", Fingerprint: "fp"} }
	if got := a.Runner.StuckResumable(&State{Jobs: []*Job{job()}}, th, items); got == nil {
		t.Error("自动停的要能接回")
	}
	b := job()
	b.AutoStop, b.BudgetHit = "", true
	if got := a.Runner.StuckResumable(&State{Jobs: []*Job{b}}, th, items); got == nil {
		t.Error("撞预算的要能接回")
	}
	for name, mut := range map[string]func(j *Job){
		"不是 agentq 放下的": func(j *Job) { j.AutoStop = "" },
		"别的会话":          func(j *Job) { j.SessionID = "x" },
		"别的条目":          func(j *Job) { j.Fingerprint = "x" },
		"不是待做":          func(j *Job) { j.Kind = state.KindInbox },
		"任务记录不在":        func(j *Job) { j.ID = "t2" },
	} {
		j := job()
		mut(j)
		if got := a.Runner.StuckResumable(&State{Jobs: []*Job{j}}, th, items); got != nil {
			t.Errorf("%s：不该接回", name)
		}
	}
}

// 用户去掉整个卡住标记：不等 retry_after，下次 tick 或 agentq run 从记下的分支接着做，提示词写明分支、worktree、从现状接着做。
func TestIntegrationHandoffAfterMarkRemoved(t *testing.T) {
	t.Parallel()
	removeMark := func(t *testing.T, e *testEnv, dir string) {
		t.Helper()
		var lines []string
		for _, l := range strings.Split(e.backlogOf(dir), "\n") {
			lines = append(lines, runner.AutoStuckRe.ReplaceAllString(l, ""))
		}
		os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Join(lines, "\n")), 0o644)
		testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "用户去掉卡住")
	}
	check := func(t *testing.T, e *testEnv, wt string, j *Job) {
		t.Helper()
		if j.ResumeOf != "" || j.Takeover {
			t.Errorf("新会话，普通任务：%+v", j)
		}
		prompt, _ := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
		for _, want := range []string{"分支 agent/t-cf", "worktree " + testkit.RealPath(t, wt), "从现状接着做", "不重做", "用户去掉了 agentq 写的卡住标记", "不另开分支"} {
			if !strings.Contains(string(prompt), want) {
				t.Errorf("提示词缺 %q：\n%s", want, prompt)
			}
		}
	}
	t.Run("tick 不等 retry_after", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=60\n")
		e.tickAt(noon, 31*time.Minute)
		removeMark(t, e, dir)
		if out := e.tickAt(noon, 32*time.Minute); !strings.Contains(out, "启动 1") {
			t.Fatalf("用户已经表态，不等 12 小时：\n%s", out)
		}
		next := e.state().Jobs[1]
		check(t, e, wt, next)
		e.mustRun("stop", next.ID)
	})
	t.Run("agentq run", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=60\n")
		e.tickAt(noon, 31*time.Minute)
		removeMark(t, e, dir)
		e.runAt(noon.Add(32*time.Minute), "run", "demo")
		next := e.state().Jobs[1]
		check(t, e, wt, next)
		e.mustRun("stop", next.ID)
	})
	t.Run("分支已经不在就当普通条目", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=60\n")
		e.tickAt(noon, 31*time.Minute)
		removeMark(t, e, dir)
		testkit.GitT(t, e.home, dir, "worktree", "remove", "--force", wt)
		testkit.GitT(t, e.home, dir, "branch", "-q", "-D", "agent/t-cf")
		if out := e.tickAt(noon, 32*time.Minute); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		next := e.state().Jobs[1]
		prompt, _ := os.ReadFile(filepath.Join(jobHome(next, e), "prompt.txt"))
		if string(prompt) != runner.BuildPrompt(dir, next.Item)+"\n" {
			t.Errorf("普通提示词：\n%s", prompt)
		}
		e.mustRun("stop", next.ID)
	})
	t.Run("改了正文也认得（按标题）", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=60\n")
		e.tickAt(noon, 31*time.Minute)
		removeMark(t, e, dir)
		p := filepath.Join(dir, "docs", "BACKLOG.md")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), "—— CLAIMFIRST", "—— 用户补一句 CLAIMFIRST", 1)), 0o644)
		testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "改正文")
		if out := e.tickAt(noon, 32*time.Minute); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		next := e.state().Jobs[1]
		check(t, e, wt, next)
		e.mustRun("stop", next.ID)
	})
}

// handoffPrompt 断言任务的提示词接着记下的分支。
func handoffPrompt(t *testing.T, e *testEnv, j *Job, wt string) {
	t.Helper()
	prompt, _ := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	for _, want := range []string{"分支 agent/t-cf", "worktree " + testkit.RealPath(t, wt), "从现状接着做", "用户去掉了 agentq 写的卡住标记"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("任务 %s 的提示词缺 %q：\n%s", j.ID, want, prompt)
		}
	}
}

// setTodo 把条目换成 text 并提交（去掉卡住标记时顺便换成假 claude 认的关键字）。
func setTodo(t *testing.T, e *testEnv, dir, text string) {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(e.backlogOf(dir), "\n") {
		if strings.HasPrefix(l, "- [ ] 甲") {
			l = "- [ ] 甲 —— " + text
		}
		lines = append(lines, l)
	}
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Join(lines, "\n")), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "改条目")
}

// 接手任务写进行中标记之前就失败：分支不能丢，下一个任务还接它。瞬时错误（529，15 分钟后重试）和普通失败（等 retry_after）都一样。
func TestIntegrationHandoffUnclaimedKeepsBranch(t *testing.T) {
	t.Parallel()
	t.Run("上游 529", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
		e.tickAt(noon, 31*time.Minute)
		setTodo(t, e, dir, "APIERR529")
		e.tickAt(noon, 32*time.Minute)
		first := e.state().Jobs[1]
		handoffPrompt(t, e, first, wt)
		waitExit(t, first.PID, 10*time.Second)
		e.tickAt(noon, 33*time.Minute)
		first = e.state().Jobs[1]
		if first.Status != state.StatusFailed || !first.Transient || !first.Handoff || first.StuckBranch != "agent/t-cf" {
			t.Fatalf("没认领，来源分支要留着：%+v", first)
		}
		// 退避 15 分钟后重试：还是接分支，也还是瞬时错误
		if out := e.tickAt(noon, 49*time.Minute); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		second := e.state().Jobs[2]
		handoffPrompt(t, e, second, wt)
		waitExit(t, second.PID, 10*time.Second)
		e.tickAt(noon, 50*time.Minute)
		// 上游好了（条目换成正常的）：第三次还是接分支，普通失败的 12 小时不用等，上一次是瞬时错误
		setTodo(t, e, dir, "x SLEEP=0")
		if out := e.tickAt(noon, 51*time.Minute); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		third := e.state().Jobs[3]
		handoffPrompt(t, e, third, wt)
		waitExit(t, third.PID, 10*time.Second)
	})
	t.Run("普通失败", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
		e.tickAt(noon, 31*time.Minute)
		setTodo(t, e, dir, "FAIL")
		e.tickAt(noon, 32*time.Minute)
		first := e.state().Jobs[1]
		handoffPrompt(t, e, first, wt)
		waitExit(t, first.PID, 10*time.Second)
		e.tickAt(noon, 33*time.Minute)
		first = e.state().Jobs[1]
		if first.Status != state.StatusFailed || first.Transient || !first.Handoff {
			t.Fatalf("%+v", first)
		}
		// 普通失败照旧等 retry_after，不因为接手就马上再试
		if out := e.tickAt(noon, 10*time.Hour); !strings.Contains(out, "启动 0") {
			t.Fatalf("要等 12 小时：\n%s", out)
		}
		if out := e.tickAt(noon, 13*time.Hour); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		next := e.state().Jobs[2]
		handoffPrompt(t, e, next, wt)
		waitExit(t, next.PID, 10*time.Second)
	})
	t.Run("结果读不懂、秒退", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
		e.tickAt(noon, 31*time.Minute)
		setTodo(t, e, dir, "EMPTY")
		e.tickAt(noon, 32*time.Minute)
		first := e.state().Jobs[1]
		waitExit(t, first.PID, 10*time.Second)
		e.tickAt(noon, 33*time.Minute)
		if out := e.tickAt(noon, 13*time.Hour); !strings.Contains(out, "启动 1") {
			t.Fatalf("\n%s", out)
		}
		next := e.state().Jobs[2]
		handoffPrompt(t, e, next, wt)
		waitExit(t, next.PID, 10*time.Second)
	})
	t.Run("用户停掉没认领的接手任务", func(t *testing.T) {
		t.Parallel()
		e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
		e.tickAt(noon, 31*time.Minute)
		setTodo(t, e, dir, "SLEEP=60")
		e.tickAt(noon, 32*time.Minute)
		first := e.state().Jobs[1]
		e.runAt(noon.Add(33*time.Minute), "stop", first.ID)
		e.runAt(noon.Add(34*time.Minute), "run", "demo")
		next := e.state().Jobs[2]
		handoffPrompt(t, e, next, wt)
		e.mustRun("stop", next.ID)
	})
}

// 认领之后就不再继承：之后由它自己的进行中标记管（孤儿逻辑、续跑）；用户连进行中标记也去掉时，是普通条目。
func TestIntegrationHandoffClaimedStopsInheriting(t *testing.T) {
	t.Parallel()
	e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	e.tickAt(noon, 31*time.Minute)
	setTodo(t, e, dir, "CLAIMFIRST FAIL")
	e.tickAt(noon, 32*time.Minute)
	first := e.state().Jobs[1]
	handoffPrompt(t, e, first, wt)
	waitExit(t, first.PID, 10*time.Second)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "认领")
	e.tickAt(noon, 33*time.Minute)
	first = e.state().Jobs[1]
	if first.Status != state.StatusFailed || first.Handoff {
		t.Fatalf("认领过了不再继承：%+v", first)
	}
	if !strings.Contains(e.backlogOf(dir), "（进行中：agent/t-cf") {
		t.Fatalf("\n%s", e.backlogOf(dir))
	}
	setTodo(t, e, dir, "x")
	if out := e.tickAt(noon, 13*time.Hour); !strings.Contains(out, "启动 1") {
		t.Fatalf("\n%s", out)
	}
	next := e.state().Jobs[2]
	prompt, _ := os.ReadFile(filepath.Join(jobHome(next, e), "prompt.txt"))
	if string(prompt) != runner.BuildPrompt(dir, next.Item)+"\n" {
		t.Errorf("普通提示词：\n%s", prompt)
	}
	waitExit(t, next.PID, 10*time.Second)
}

// 接手任务没认领就又被自动停：来源分支不能被空的标记盖掉，卡住标记和通知都还写它，再去掉标记还接得上。
func TestIntegrationHandoffRunawayKeepsBranch(t *testing.T) {
	t.Parallel()
	e, _, dir, wt, _, noon := runawayEnv(t, nil, "- [ ] 甲 —— CLAIMFIRST SLEEP=120\n")
	e.tickAt(noon, 31*time.Minute)
	setTodo(t, e, dir, "SLEEP=120")
	e.tickAt(noon, 32*time.Minute)
	first := e.state().Jobs[1]
	writeRunawayLogs(t, e, first)
	e.tickAt(noon, 63*time.Minute)
	first = e.state().Jobs[1]
	if first.AutoStop != backend.SigRunaway || first.StuckBranch != "agent/t-cf" || !first.StuckMarked {
		t.Fatalf("%+v", first)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "任务 "+first.ID+"，分支 agent/t-cf）") {
		t.Errorf("\n%s", b)
	}
	setTodo(t, e, dir, "SLEEP=60")
	if out := e.tickAt(noon, 64*time.Minute); !strings.Contains(out, "启动 1") {
		t.Fatalf("\n%s", out)
	}
	next := e.state().Jobs[2]
	handoffPrompt(t, e, next, wt)
	e.mustRun("stop", next.ID)
}

// 闲时找活撞预算：走它自己的收尾（一条都不写，不接着做），不算「放下的任务」，也不推撞预算的提醒。
func TestSuggestBudgetKeepsOwnHandling(t *testing.T) {
	a, _ := newTestApp(t)
	a.Cfg.NotifyEnabled = false
	now := time.Now()
	j := &Job{ID: "1104-120000-闲时找活", Kind: state.KindSuggest, Project: suggest.SuggestJobName, Title: "闲时找活", Status: state.StatusRunning, StartedAt: now, PID: 1}
	if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.JobDir(j.ID), "result.json"), []byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","session_id":"s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{j}}
	a.Runner.FinishFromResult(st, j)
	if j.BudgetHit || j.ResumePending || j.Status != state.StatusFailed || !strings.Contains(j.Summary, "撞到回合或预算上限，没有写任何建议") {
		t.Errorf("%+v", j)
	}
}
