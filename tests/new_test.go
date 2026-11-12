package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/gate"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// newApp 带 projects_root 的 App，HOME 也指到临时目录（核对提交时要跑 git）。
func newRootApp(t *testing.T) (*App, string) {
	t.Helper()
	a, _ := newTestApp(t)
	t.Setenv("HOME", a.Home)
	root := t.TempDir()
	a.Cfg.ProjectsRoot = root
	return a, root
}

// new 占名额：计入全局上限，不算 per_project；其他 Kind 照旧不占。
func TestNewOccupiesCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.MaxParallel, cfg.PerProject = 2, 1
	st := &State{Jobs: []*Job{{ID: "n", Kind: state.KindNew, Project: newproj.NewJobName, Status: state.StatusRunning}}}
	if got := st.Tasks(); len(got) != 1 {
		t.Errorf("new 应算任务：%+v", got)
	}
	if ok, why := state.Capacity(st, cfg, "alpha"); !ok {
		t.Errorf("还有一个名额：%s", why)
	}
	if ok, why := state.Capacity(st, cfg, ""); !ok {
		t.Errorf("per_project 不算 new：%s", why)
	}
	st.Jobs = append(st.Jobs, &Job{ID: "m", Kind: state.KindNew, Project: newproj.NewJobName, Status: state.StatusRunning})
	if ok, _ := state.Capacity(st, cfg, "alpha"); ok {
		t.Errorf("两个 new 占满 max_parallel=2")
	}
	for _, k := range []string{config.KindNight, config.KindReview, state.KindPatrol, state.KindChat, state.KindFollowup, state.KindDiscuss} {
		if state.TakesSlot(k) {
			t.Errorf("%s 不占名额", k)
		}
	}
	if state.IsChatKind(state.KindNew) {
		t.Errorf("new 的结果不走对话回发")
	}
	if _, ok := config.Routines[state.KindNew]; ok {
		t.Errorf("new 不是定点任务")
	}
}

// 熔断：new 只计秒退，跑完不解除熔断，用户停的不算。
func TestNewBreaker(t *testing.T) {
	cfg := config.Default()
	now := time.Now()
	quick, long := time.Second, cfg.BreakerQuick.Duration+time.Second
	st := &State{FailStreak: 1}
	runner.RecordOutcome(st, cfg, &Job{Kind: state.KindNew, Status: state.StatusInterrupted}, quick, now)
	if st.FailStreak != 2 {
		t.Errorf("秒退要计数：%+v", st)
	}
	runner.RecordOutcome(st, cfg, &Job{Kind: state.KindNew, Status: state.StatusInterrupted}, long, now)
	runner.RecordOutcome(st, cfg, &Job{Kind: state.KindNew, Status: state.StatusDone}, quick, now)
	runner.RecordOutcome(st, cfg, &Job{Kind: state.KindNew, Status: state.StatusKilled}, quick, now)
	if st.FailStreak != 2 {
		t.Errorf("跑了一阵的、跑完的、停的都不改计数：%+v", st)
	}
	until := now.Add(time.Hour)
	st = &State{BreakerUntil: until}
	runner.RecordOutcome(st, cfg, &Job{Kind: state.KindNew, Status: state.StatusDone}, long, now)
	if !st.BreakerUntil.Equal(until) {
		t.Errorf("new 跑完不解除熔断：%+v", st)
	}
}

// 回复 new 任务的进展提醒：它在跑时算对任务的意见，不能卡在收件箱里等它跑完。
func TestNewThreadWhileRunning(t *testing.T) {
	a, _ := newRootApp(t)
	st := &State{Jobs: []*Job{{ID: "n1", Kind: state.KindNew, Project: newproj.NewJobName, Status: state.StatusRunning, SessionID: "s"}}}
	s := &notify.State{Threads: map[int64]notify.Thread{9: {Kind: notify.ThreadJob, JobID: "n1", SessionID: "s", Project: newproj.NewJobName}}}
	m := notify.InMsg{ID: 1, Text: "换个思路", ReplyToID: 9}
	route, _, _ := a.Chat.RouteReply(s, st, &m)
	if route != chat.RoutePlain || !strings.Contains(m.Note, "n1") {
		t.Errorf("route=%d note=%q", route, m.Note)
	}
}

// 巡逻：结束的 new 不再报（结束时已经直接推了），在跑的照样看卡住。
func TestNewPatrol(t *testing.T) {
	a, _ := newRootApp(t)
	now := time.Now()
	old := now.Add(-time.Hour)
	st := &State{Jobs: []*Job{
		{ID: "n1", Kind: state.KindNew, Project: newproj.NewJobName, Title: "中断的", Status: state.StatusInterrupted, StartedAt: old, EndedAt: &now},
		{ID: "n2", Kind: state.KindNew, Project: newproj.NewJobName, Title: "失败的", Status: state.StatusFailed, StartedAt: old, EndedAt: &now},
	}}
	if got := a.Patrol.CheckJobs(st, now); len(got) != 0 {
		t.Errorf("不重复报：%+v", got)
	}
	st.Jobs = append(st.Jobs, &Job{ID: "n3", Kind: state.KindNew, Project: newproj.NewJobName, Title: "卡住的", Status: state.StatusRunning, StartedAt: now.Add(-24 * time.Hour), SessionID: "s3"})
	if got := a.Patrol.CheckJobs(st, now); len(got) != 1 || got[0].Check != "stall" {
		t.Errorf("在跑的要看卡住：%+v", got)
	}
}

// 状态摘要把 new 算成任务。
func TestNewInDigest(t *testing.T) {
	a, _ := newRootApp(t)
	now := time.Now()
	st := &State{Jobs: []*Job{
		{ID: "r", Kind: state.KindNew, Project: newproj.NewJobName, Title: "在跑的工具", Status: state.StatusRunning, StartedAt: now},
		{ID: "d", Kind: state.KindNew, Project: newproj.NewJobName, Title: "跑完的工具", Status: state.StatusDone, StartedAt: now, EndedAt: &now},
	}}
	got := a.Chat.StatusDigest(st)
	for _, want := range []string{"在做　1/", "在跑的工具", "跑完的工具"} {
		if !strings.Contains(got, want) {
			t.Errorf("状态摘要缺 %q：\n%s", want, got)
		}
	}
}

func TestIntegrationNewFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true}) // 暂停也能启动
	testkit.MkProject(t, e.root, "old", "")
	idea := "NEWOK SLEEP=2 做一个把剪贴板历史存起来的 Mac 小工具"
	out := e.mustRun("new", idea)
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "new 不受影响") {
		t.Errorf("输出：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 1 {
		t.Fatalf("应有一个任务：%+v", st.Jobs)
	}
	j := st.Jobs[0]
	if j.Kind != state.KindNew || j.ProjectDir != e.root || j.Item != idea || j.Fingerprint != "" || j.Status != state.StatusRunning || j.SessionID == "" {
		t.Errorf("任务：%+v", j)
	}
	if len(j.ProjectsBefore) != 1 || j.ProjectsBefore[0] != "old" {
		t.Errorf("快照：%+v", j.ProjectsBefore)
	}
	status := e.mustRun("status")
	if !strings.Contains(status, "运行中（1/2）") || !strings.Contains(status, j.ID) || !strings.Contains(status, newproj.NewJobName+"「") {
		t.Errorf("status 要看到在跑的 new 任务：\n%s", status)
	}
	if strings.Contains(status, "定点任务在跑："+newproj.NewJobName) {
		t.Errorf("不算定点任务：\n%s", status)
	}
	waitExit(t, j.PID, 15*time.Second)
	log := e.fakeLogOf(j)
	for _, want := range []string{"PWD[" + testkit.RealPath(t, e.root) + "]", "ARG[--permission-mode]\nARG[bypassPermissions]", "ARG[--output-format]\nARG[json]",
		"ARG[--disallowed-tools]\nARG[Monitor]", "ARG[--session-id]\nARG[" + j.SessionID + "]", "ARG[--name]\nARG[agentq " + newproj.NewJobName + " "} {
		if !strings.Contains(log, want) {
			t.Errorf("假 claude 参数缺 %q：\n%s", want, log)
		}
	}
	prompt, err := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), newproj.NewIdeaBegin+"\n"+idea+"\n"+newproj.NewIdeaEnd) {
		t.Errorf("prompt：%v\n%s", err, prompt)
	}

	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1") {
		t.Fatalf("tick：\n%s", out)
	}
	j = e.state().Jobs[0]
	dir := filepath.Join(testkit.RealPath(t, e.root), "clip-tool")
	if j.Status != state.StatusDone || j.ResumePending || j.NewReport == nil || j.NewReport.Path != dir || j.NewReport.View != "cd clip-tool 后 make run" || j.NewReport.Branch != "agent/first" {
		t.Fatalf("应跑完并记下报告：%+v %+v", j, j.NewReport)
	}
	// 第一条和别的条目走同一条路：这个任务改认新项目里的那一条，暂停时也过门，合并的是引擎
	if j.DefaultBranch != "main" {
		t.Errorf("改认时记下新项目的默认分支：%q", j.DefaultBranch)
	}
	if j.Kind != "" || j.Project != "clip-tool" || j.ProjectDir != dir || j.Title != "第一条活" || j.Gate == nil || j.Gate.State != state.GateRunning || j.Gate.Branch != "agent/first" {
		t.Fatalf("应排进门队列：%+v %+v", j, j.Gate)
	}
	if out := testkit.GitT(t, e.home, dir, "log", "-1", "--format=%s", "main"); out != "init" {
		t.Errorf("任务不合并：%s", out)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.state().Jobs[0]
	if j.Gate.State != state.GateMerged || j.NewReport.Commit == "" || !strings.HasPrefix(j.Gate.Merge, j.NewReport.Commit) {
		t.Fatalf("应过门合并：%+v %+v", j.Gate, j.NewReport)
	}
	if !strings.Contains(testkit.GitT(t, e.home, dir, "log", "-1", "--format=%B", j.Gate.Merge), "Agentq-Type: feature") {
		t.Errorf("第一条的合并提交和别的条目一样")
	}
	if bl := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")); !strings.Contains(bl, "- [x] 第一条活") {
		t.Errorf("引擎写收尾：\n%s", bl)
	}
	report := e.mustRun("gate", "report", j.ID)
	for _, want := range []string{"# 交付单：clip-tool「第一条活」", "做出第一版", "cd clip-tool 后 make run", "只给自己用"} {
		if !strings.Contains(report, want) {
			t.Errorf("过了成品门要有交付单，缺 %q：\n%s", want, report)
		}
	}
	sha := j.NewReport.Commit
	night := e.nightly()
	for _, want := range []string{"等过门", dir, "只给自己用", "cd clip-tool 后 make run", sha} {
		if !strings.Contains(night, want) {
			t.Errorf("当天汇总缺 %q：\n%s", want, night)
		}
	}
	if strings.Contains(night, "agent/fake") {
		t.Errorf("正文顺带提到的分支不算：\n%s", night)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "跑完") || !strings.Contains(out, sha) {
		t.Errorf("logs：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "跑完：合并提交 "+sha) { // 只显示摘要前 80 个字，临时目录的路径会被截断
		t.Errorf("status 最近结束要看到合并提交：\n%s", out)
	}

	// 暂停期间不接；恢复后新项目剩下的待做由 tick 接着做
	e.mustRun("tick")
	if n := len(e.state().Jobs); n != 1 {
		t.Errorf("暂停时不接：%d", n)
	}
	e.mustRun("resume")
	e.mustRun("tick")
	next := e.jobByTitle("第二条活")
	if next.Project != "clip-tool" || next.Kind != "" {
		t.Errorf("接着做：%+v", next)
	}
	waitExit(t, next.PID, 10*time.Second)
}

// 中途退出（停在等通知）、路径不对：按中断报，不算跑完，不续跑，半截目录不删。
func TestIntegrationNewInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWWAIT 做个工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 10*time.Second)
	e.mustRun("tick")
	j = e.state().Jobs[0]
	if j.Status != state.StatusInterrupted || j.ResumePending || j.NewReport != nil {
		t.Fatalf("应中断、不续跑：%+v", j)
	}
	for _, want := range []string{"继续等通知", "后台通知", "half-done"} {
		if !strings.Contains(j.Summary, want) {
			t.Errorf("摘要缺 %q：%s", want, j.Summary)
		}
	}
	if !strings.Contains(e.nightly(), "中断") || strings.Contains(e.nightly(), "跑完") {
		t.Errorf("汇总：\n%s", e.nightly())
	}
	if _, err := os.Stat(filepath.Join(e.root, "half-done")); err != nil {
		t.Errorf("半截目录不能删：%v", err)
	}
	e.mustRun("resume")
	e.mustRun("tick")
	if n := len(e.state().Jobs); n != 1 {
		t.Errorf("不续跑、不重试：%d 个任务", n)
	}
	e.mustRun("pause")

	e.mustRun("new", "NEWOUTSIDE 再做一个")
	k := e.jobByTitle("NEWOUTSIDE 再做一个")
	waitExit(t, k.PID, 10*time.Second)
	e.mustRun("tick")
	k = e.jobByTitle("NEWOUTSIDE 再做一个")
	if k.Status != state.StatusInterrupted || k.NewReport != nil || !strings.Contains(k.Summary, "/etc") {
		t.Errorf("路径不在 projects_root 下：%+v", k)
	}
}

// 报错、读不出结果、撞上限、超时、用户停：都不算跑完，都不删东西。
func TestIntegrationNewBadEnds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true, "timeout": "2s", "max_parallel": 9})
	finish := func(idea string) *Job {
		t.Helper()
		e.mustRun("new", idea)
		j := e.jobByTitle(idea)
		waitExit(t, j.PID, 15*time.Second)
		e.mustRun("tick")
		return e.jobByTitle(idea)
	}
	if j := finish("FAIL 甲"); j.Status != state.StatusFailed || !strings.Contains(j.Summary, "出错了") {
		t.Errorf("报错：%+v", j)
	}
	if j := finish("EMPTY 乙"); j.Status != state.StatusFailed || !strings.Contains(j.Summary, "没有可读的结果") {
		t.Errorf("没有结果：%+v", j)
	}
	// is_error 为假、结果是撞上限的提示：不能算跑完
	if j := finish("HITOK 丙"); j.Status != state.StatusInterrupted || !j.LimitHit {
		t.Errorf("撞上限：%+v", j)
	}
	st := e.state()
	if !time.Now().Before(st.CooldownOf("")) {
		t.Errorf("撞上限要冷却：%+v", st.CooldownOf(""))
	}
	delete(st.BackendLimits, "claude")
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}

	e.mustRun("new", "SLEEP=30 丁")
	j := e.jobByTitle("SLEEP=30 丁")
	time.Sleep(2500 * time.Millisecond)
	e.mustRun("tick")
	if got := e.jobByTitle("SLEEP=30 丁"); got.Status != state.StatusTimeout {
		t.Errorf("超时：%+v", got)
	}
	waitExit(t, j.PID, 15*time.Second)

	e.mustRun("new", "SLEEP=30 戊")
	j = e.jobByTitle("SLEEP=30 戊")
	if err := os.MkdirAll(filepath.Join(e.root, "half", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out := e.mustRun("stop", j.ID); !strings.Contains(out, "half") {
		t.Errorf("停的时候要说多了哪个目录：\n%s", out)
	}
	if got := e.jobByTitle("SLEEP=30 戊"); got.Status != state.StatusKilled {
		t.Errorf("stop：%+v", got)
	}
	if _, err := os.Stat(filepath.Join(e.root, "half")); err != nil {
		t.Errorf("不能删：%v", err)
	}
}

// 机器重启过：记中断，不续跑。
func TestNewRebooted(t *testing.T) {
	a, _ := newRootApp(t)
	j := &Job{ID: "1104-120000-新项目", Kind: state.KindNew, Project: newproj.NewJobName, Title: "做个工具", Status: state.StatusRunning, StartedAt: time.Now(), SessionID: "s"}
	if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{j}}
	a.Runner.FinishRebooted(st, j)
	if j.Status != state.StatusInterrupted || !j.Rebooted || j.ResumePending {
		t.Errorf("%+v", j)
	}
}

func TestIntegrationNewRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 2})
	noJobs := func(what string) {
		t.Helper()
		if n := len(e.state().Jobs); n != 0 {
			t.Errorf("%s：不该启动，有 %d 个任务", what, n)
		}
		logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
		if len(logs) != 0 {
			t.Errorf("%s：不该起 claude：%v", what, logs)
		}
	}
	for _, args := range [][]string{{"new"}, {"new", "两个", "参数"}} {
		if out, code := e.run(args...); code != 2 || !strings.Contains(out, "参数个数不对") {
			t.Errorf("%v：%d\n%s", args, code, out)
		}
	}
	for _, idea := range []string{"", "   \n ", strings.Repeat("字", newproj.NewIdeaMax+1), "--help"} {
		if out, code := e.run("new", idea); code == 0 || strings.Contains(out, "已启动") {
			t.Errorf("%q：%d\n%s", idea, code, out)
		}
	}
	noJobs("输入不对")

	// 额度 ≥98% 拒绝，并说明；到后台上限只提示（下面的用例）
	e.setUsage(usageLine("99"))
	if out, code := e.run("new", "做个工具"); code == 0 || !strings.Contains(out, "订阅额度快用完") {
		t.Errorf("额度：%d\n%s", code, out)
	}
	noJobs("额度到上限")
	os.Remove(filepath.Join(e.home, "fake-usage.json"))

	// 没登录
	saved := e.extra
	e.extra = append(append([]string(nil), saved...), "AGENTQ_FAKE_LOGGED_OUT=1")
	if out, code := e.run("new", "做个工具"); code == 0 || !strings.Contains(out, "未登录") {
		t.Errorf("没登录：%d\n%s", code, out)
	}
	e.extra = saved
	noJobs("没登录")

	// 用量冷却
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	if err := state.Save(statePath, &State{BackendLimits: claudeLimit(time.Now().Add(time.Hour), nil)}); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("new", "做个工具"); code == 0 || !strings.Contains(out, "用量冷却") {
		t.Errorf("冷却：%d\n%s", code, out)
	}
	noJobs("冷却")

	// 状态文件坏了当未知，不启动
	if err := os.WriteFile(statePath, []byte("{坏的"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("new", "做个工具"); code == 0 || !strings.Contains(out, "状态文件读不出") {
		t.Errorf("坏状态：%d\n%s", code, out)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	// projects_root 不存在
	badRoot := newEnv(t, map[string]any{"projects_root": filepath.Join(e.home, "没有")})
	if out, code := badRoot.run("new", "做个工具"); code == 0 || !strings.Contains(out, "projects_root") {
		t.Errorf("projects_root：%d\n%s", code, out)
	}

	noJobs("上面所有拒绝")
}

// 名额：new 占着时 tick 少接一个、run 被拒；满了 new 也被拒；同一句话不重复起；到后台上限只提示。
func TestIntegrationNewCapacity(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 2})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲 —— SLEEP=20\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙 —— SLEEP=20\n")
	e.setUsage(usageLine("91"))
	out := e.mustRun("new", "SLEEP=20 做个工具")
	if !strings.Contains(out, "订阅额度到了后台上限") {
		t.Errorf("到后台上限只提示：\n%s", out)
	}
	os.Remove(filepath.Join(e.home, "fake-usage.json"))
	st := e.state()
	st.SetUsageOf("", nil)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("new", "SLEEP=20  做个工具"); code == 0 || !strings.Contains(out, "已经在跑") {
		t.Errorf("同一句话（空白不同）不重复起：%d\n%s", code, out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "运行中 2/2") || !strings.Contains(out, "启动 1") {
		t.Errorf("new 占一个名额，tick 只接一个：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "运行中（2/2）") {
		t.Errorf("status：\n%s", out)
	}
	if out, code := e.run("new", "另一个工具"); code == 0 || !strings.Contains(out, "已满") {
		t.Errorf("满了：%d\n%s", code, out)
	}
	if out, code := e.run("run", "beta"); code == 0 || !strings.Contains(out, "已满") {
		t.Errorf("run 也算进去：%d\n%s", code, out)
	}
	for _, j := range e.state().Running() {
		e.mustRun("stop", j.ID)
	}
}

// new 在跑时，启动后才出现的项目 tick 不挑（第一条可能还没认领），已有的照常；它结束后才挑。
func TestIntegrationNewScheduleSkipsFresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	testkit.MkProject(t, e.root, "old", "- [ ] 旧活\n")
	e.mustRun("new", "SLEEP=4 做个工具")
	j := e.jobByTitle("SLEEP=4 做个工具")
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活\n")
	out := e.mustRun("tick")
	if !strings.Contains(out, "fresh 是新项目任务启动后才出现的") {
		t.Errorf("tick：\n%s", out)
	}
	if e.findJob("新活") != nil || e.findJob("旧活") == nil {
		t.Fatalf("只接旧活：%+v", e.state().Jobs)
	}
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	if e.findJob("新活") == nil {
		t.Errorf("new 结束后要接：%+v", e.state().Jobs)
	}
}

// 推送端到端：做完推一条，正文转义过；回复它在 projects_root 里只读续跑（会话是在那里开的）。
func TestIntegrationNewNotify(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"discuss_enabled": false})
	now := time.Now()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).Format(time.RFC3339))
	e.mustRun("new", "NEWOK 做个工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 10*time.Second)
	e.mustRun("tick")
	e.waitGate(e.state().Jobs[0]) // 第一条过门合并后才说
	var hit []string
	for _, x := range tg.texts() {
		if strings.Contains(x, "clip-tool") {
			hit = append(hit, x)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("应正好推一条：%q", tg.texts())
	}
	if !strings.Contains(hit[0], "新项目第一版可以交付了") || !strings.Contains(hit[0], "&lt;b&gt;不做同步") || !strings.Contains(hit[0], "make run") {
		t.Errorf("正文：%s", hit[0])
	}
	var msgID int64
	for id, th := range e.readNotify().Threads {
		if th.JobID == j.ID {
			msgID = id
		}
	}
	if msgID == 0 {
		t.Fatalf("推送要登记回复链：%+v", e.readNotify().Threads)
	}
	e.seedSession("s-new")
	tg.replyMsg(1, 11, "FUPREPLY 怎么跑", msgID)
	fup := e.tickUntilEnded(func() *Job {
		for _, x := range e.state().Jobs {
			if x.Kind == state.KindFollowup {
				return x
			}
		}
		return nil
	})
	if log := e.fakeLogOf(fup); !strings.Contains(log, "ARG[-r]\nARG[s-new]") || !strings.Contains(log, "PWD["+testkit.RealPath(t, e.root)+"]") {
		t.Errorf("续跑：\n%s", log)
	}
	if got := tg.sentLast().Get("text"); !strings.Contains(got, "已经合并了") {
		t.Errorf("回答：%q", got)
	}
}

func heldNames(e *testEnv, id string) []string {
	e.t.Helper()
	st := e.state()
	j, err := st.Find(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j.HeldProjects
}

// CLI 停：输出写目录和恢复办法；tick 不挑，别的项目照常；status 一行；agentq run 解除后 tick 接着做，文件里不留字段。
func TestIntegrationNewStopHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	testkit.MkProject(t, e.root, "old", "- [ ] 旧活\n")
	e.mustRun("new", "SLEEP=30 做个工具")
	j := e.jobByTitle("SLEEP=30 做个工具")
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活一\n- [ ] 新活二\n")
	out := e.mustRun("stop", j.ID)
	for _, want := range []string{"fresh", "agentq run", "挪走"} {
		if !strings.Contains(out, want) {
			t.Errorf("stop 输出缺 %q：\n%s", want, out)
		}
	}
	if got := heldNames(e, j.ID); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("保留：%v", got)
	}
	if got := e.jobByTitle("SLEEP=30 做个工具"); got.Status != state.StatusKilled || !strings.Contains(got.Summary, "不会自动做") {
		t.Errorf("摘要：%+v", got)
	}
	for range 2 {
		out = e.mustRun("tick")
		if !strings.Contains(out, "fresh 是停掉的新项目") {
			t.Errorf("tick：\n%s", out)
		}
		if e.findJob("新活一") != nil {
			t.Fatalf("停掉的项目不该被挑：%+v", e.state().Jobs)
		}
	}
	old := e.findJob("旧活")
	if old == nil {
		t.Fatalf("启动前就有的项目照常：%+v", e.state().Jobs)
	}
	waitExit(t, old.PID, 10*time.Second)
	status := e.mustRun("status")
	if !strings.Contains(status, "停掉的新项目，不自动做：fresh") || !strings.Contains(status, "agentq run <项目>") {
		t.Errorf("status：\n%s", status)
	}

	e.mustRun("run", "fresh", "新活一")
	one := e.jobByTitle("新活一")
	if got := heldNames(e, j.ID); len(got) != 0 {
		t.Errorf("run 启动后解除：%v", got)
	}
	for _, f := range []string{filepath.Join(e.home, ".agentq", "state.json"), filepath.Join(jobHome(j, e), "meta.json")} {
		if b, err := os.ReadFile(f); err != nil || strings.Contains(string(b), "held_projects") {
			t.Errorf("%s 不该留字段：%v\n%s", f, err, b)
		}
	}
	if status := e.mustRun("status"); strings.Contains(status, "停掉的新项目") {
		t.Errorf("解除后 status 不再列：\n%s", status)
	}
	waitExit(t, one.PID, 10*time.Second)
	e.mustRun("tick")
	if two := e.findJob("新活二"); two == nil {
		t.Errorf("解除后 tick 接着做：%+v", e.state().Jobs)
	} else {
		waitExit(t, two.PID, 10*time.Second)
	}
}

// 乙先启动、甲后启动，甲先做成，再停乙：甲的项目不是乙留下的，不保留、不列，它剩下的待做 tick 接着做；乙自己留下的照样保留。
func TestIntegrationNewDoneThenStopOther(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 3})
	e.mustRun("new", "SLEEP=30 乙")
	b := e.jobByTitle("SLEEP=30 乙")
	e.mustRun("new", "NEWOK 甲")
	a := e.jobByTitle("NEWOK 甲")
	waitExit(t, a.PID, 15*time.Second)
	e.mustRun("tick")
	if got := e.jobByTitle("第一条活"); got.Status != state.StatusDone || got.NewReport == nil {
		t.Fatalf("甲应做成：%+v", got)
	}
	if e.findJob("第二条活") != nil {
		t.Fatalf("乙还在跑时不挑：%+v", e.state().Jobs)
	}
	testkit.MkProject(t, e.root, "half", "- [ ] 半截的活\n")
	out := e.mustRun("stop", b.ID)
	if !strings.Contains(out, "half") || strings.Contains(out, "clip-tool") {
		t.Errorf("stop 只列乙留下的：\n%s", out)
	}
	if got := heldNames(e, b.ID); len(got) != 1 || got[0] != "half" {
		t.Errorf("保留：%v", got)
	}
	if got := e.jobByTitle("SLEEP=30 乙"); strings.Contains(got.Summary, "clip-tool") {
		t.Errorf("摘要只列乙留下的：%s", got.Summary)
	}
	e.waitGate(e.jobByTitle("第一条活"))
	next := e.findJob("第二条活")
	if next == nil {
		t.Fatalf("甲的项目要接着做：%+v", e.state().Jobs)
	}
	waitExit(t, next.PID, 10*time.Second)
	if e.findJob("半截的活") != nil {
		t.Errorf("乙留下的不挑：%+v", e.state().Jobs)
	}
}

// agentq run 没启动成就不解除：没这个条目、名额满了。
func TestIntegrationNewRunKeepsHoldOnRefuse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 1, "paused": true})
	e.mustRun("new", "SLEEP=30 做个工具")
	j := e.jobByTitle("SLEEP=30 做个工具")
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活\n")
	testkit.MkProject(t, e.root, "busy", "- [ ] 占名额 SLEEP=30\n")
	e.mustRun("stop", j.ID)
	if out, code := e.run("run", "fresh", "没有这个词"); code == 0 || !strings.Contains(out, "没有包含") {
		t.Errorf("没这个条目：%d\n%s", code, out)
	}
	if got := heldNames(e, j.ID); len(got) != 2 {
		t.Errorf("没启动不解除：%v", got)
	}
	e.mustRun("run", "busy")
	if got := heldNames(e, j.ID); len(got) != 1 || got[0] != "fresh" {
		t.Errorf("只解除启动的那个：%v", got)
	}
	if out, code := e.run("run", "fresh"); code == 0 || !strings.Contains(out, "已满") {
		t.Errorf("名额满了：%d\n%s", code, out)
	}
	if got := heldNames(e, j.ID); len(got) != 1 {
		t.Errorf("名额满了不解除：%v", got)
	}
	e.mustRun("stop", e.jobByTitle("占名额 SLEEP=30").ID)
	// 用户把目录挪走了：tick 清掉记录，status 不再列
	if err := os.Rename(filepath.Join(e.root, "fresh"), filepath.Join(e.home, "moved")); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if got := heldNames(e, j.ID); len(got) != 0 {
		t.Errorf("挪走后清记录：%v", got)
	}
}

// 老状态文件没有 held_projects：照常读，被停掉的老 new 记录不挡任何项目。
func TestIntegrationNewHoldOldState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活\n")
	old := `{"jobs":[{"id":"1101-120000-新项目","project":"新项目","project_dir":"` + e.root + `","item":"老的","title":"老的","pid":0,` +
		`"started_at":"2026-11-01T12:00:00+08:00","status":"killed","ended_at":"2026-11-01T12:10:00+08:00","kind":"new"}],` +
		`"cooldown_until":"0001-01-01T00:00:00Z","breaker_until":"0001-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "state.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := e.mustRun("status"); strings.Contains(status, "停掉的新项目") {
		t.Errorf("status：\n%s", status)
	}
	e.mustRun("tick")
	if n := e.findJob("新活"); n == nil {
		t.Errorf("照常挑：%+v", e.state().Jobs)
	} else {
		waitExit(t, n.PID, 10*time.Second)
	}
}

// 状态文件损坏重建：保留不丢；解除过的不回来。
func TestIntegrationNewHoldRecover(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	corrupt := func() {
		t.Helper()
		if err := os.WriteFile(statePath, []byte("{坏的"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := e.mustRun("tick"); !strings.Contains(out, "只收尾不启动") {
			t.Errorf("应走重建：\n%s", out)
		}
	}
	e.mustRun("new", "SLEEP=30 做个工具")
	j := e.jobByTitle("SLEEP=30 做个工具")
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活一\n- [ ] 新活二\n")
	e.mustRun("stop", j.ID)

	corrupt()
	if got := heldNames(e, j.ID); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("重建后保留丢了：%v", got)
	}
	e.mustRun("tick")
	if e.findJob("新活一") != nil {
		t.Fatalf("重建后也不该挑：%+v", e.state().Jobs)
	}
	if status := e.mustRun("status"); !strings.Contains(status, "停掉的新项目，不自动做：fresh") {
		t.Errorf("status：\n%s", status)
	}

	e.mustRun("run", "fresh", "新活一")
	one := e.jobByTitle("新活一")
	waitExit(t, one.PID, 10*time.Second)
	corrupt()
	if got := heldNames(e, j.ID); len(got) != 0 {
		t.Fatalf("解除过的不该回来：%v", got)
	}
	e.mustRun("tick")
	if two := e.findJob("新活二"); two == nil {
		t.Errorf("解除后接着做：%+v", e.state().Jobs)
	} else {
		waitExit(t, two.PID, 10*time.Second)
	}
}

// 失败结束不算用户停的：不保留，待做之后接着做（行为不变）。
func TestIntegrationNewFailedNotHeld(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.mustRun("new", "FAIL SLEEP=2 做个工具")
	j := e.jobByTitle("FAIL SLEEP=2 做个工具")
	testkit.MkProject(t, e.root, "fresh", "- [ ] 新活\n")
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	got := e.jobByTitle("FAIL SLEEP=2 做个工具")
	if got.Status != state.StatusFailed || len(got.HeldProjects) != 0 || strings.Contains(got.Summary, "不会自动做") || !strings.Contains(got.Summary, "tick 会接着做") {
		t.Errorf("失败：%+v", got)
	}
	if n := e.findJob("新活"); n == nil {
		t.Errorf("待做接着做：%+v", e.state().Jobs)
	} else {
		waitExit(t, n.PID, 10*time.Second)
	}
}

func TestNewHeldUnit(t *testing.T) {
	a, root := newRootApp(t)
	mk := func(id string, held ...string) *Job {
		t.Helper()
		if err := os.MkdirAll(a.JobDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		return &Job{ID: id, Kind: state.KindNew, Project: newproj.NewJobName, Title: "x", Status: state.StatusKilled, StartedAt: time.Now(), HeldProjects: held}
	}
	for _, d := range []string{"b", "a"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// status：去重、排序，挪走的不列
	st := &State{Jobs: []*Job{mk("j1", "b", "a", "gone"), mk("j2", "a")}}
	if got := a.Newproj.HeldStatus(st); !strings.Contains(got, "不自动做：a、b（") || strings.Contains(got, "gone") {
		t.Errorf("status：%q", got)
	}
	if got := a.Newproj.HeldStatus(&State{}); got != "" {
		t.Errorf("没有就不列：%q", got)
	}

	// projects_root 不在：不能当成用户挪走了
	a.Cfg.ProjectsRoot = filepath.Join(root, "没有")
	a.Newproj.DropGoneHeld(st)
	if len(st.Jobs[0].HeldProjects) != 3 {
		t.Errorf("root 不在时不判断：%v", st.Jobs[0].HeldProjects)
	}
	a.Cfg.ProjectsRoot = root
	// 挪走的清掉，同步 meta.json
	a.Newproj.DropGoneHeld(st)
	if got := st.Jobs[0].HeldProjects; len(got) != 2 || st.HeldProject("gone") || !st.HeldProject("a") {
		t.Errorf("挪走的要清：%v", got)
	}
	if b, err := os.ReadFile(filepath.Join(a.JobDir("j1"), "meta.json")); err != nil || strings.Contains(string(b), "gone") {
		t.Errorf("meta.json 要同步：%v\n%s", err, b)
	}

	// 结束 30 天的记录：还有保留的不清
	ended := time.Now().Add(-state.KeepEndedJobs - time.Hour)
	keep, drop := mk("j3", "a"), mk("j4")
	keep.EndedAt, drop.EndedAt = &ended, &ended
	pr := &State{Jobs: []*Job{keep, drop}}
	pr.Prune(time.Now())
	if len(pr.Jobs) != 1 || pr.Jobs[0].ID != "j3" {
		t.Errorf("prune：%+v", pr.Jobs)
	}

	// 另一个 new 做成了：它的项目不再算保留，别的不动
	other := mk("j5", "a", "b")
	done := mk("j6")
	done.Status, done.NewReport = state.StatusRunning, &state.NewReport{Path: filepath.Join(root, "a"), Assumptions: "无", View: "x", Commit: "abc1234"}
	both := &State{Jobs: []*Job{other, done}}
	a.Runner.Finish(both, done, state.StatusDone, "摘要")
	if got := other.HeldProjects; len(got) != 1 || got[0] != "b" {
		t.Errorf("做成的解除：%v", got)
	}
}

// waitGate 等门进程写好结果，再 tick 一次收结果。
func (e *testEnv) waitGate(j *Job) {
	e.t.Helper()
	waitFor(e.t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
}

// 第一条门没过：退回，由接手任务从分支接着做（会话在 projects_root，接不回去），不用等一天；第一版没进默认分支时后面的条目不挑。修好再过门，出交付单。
func TestIntegrationNewFirstReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.mustRun("new", "NEWBADVERIFY 做工具")
	first := e.state().Jobs[0]
	id := first.ID
	waitExit(t, first.PID, 15*time.Second)
	e.mustRun("tick")
	byID := func(id string) *Job {
		for _, j := range e.state().Jobs {
			if j.ID == id {
				return j
			}
		}
		t.Fatalf("没有任务 %s", id)
		return nil
	}
	first = byID(id)
	if first.Gate == nil || first.Gate.State != state.GateRunning {
		t.Fatalf("%+v %+v", first, first.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(first))
		return err == nil
	})
	e.mustRun("tick")
	first = byID(id)
	if first.Gate.State != state.GateReturned || first.Status != state.StatusInterrupted || first.ResumePending {
		t.Fatalf("要退回：%+v %+v", first, first.Gate)
	}
	if e.findJob("第二条活") != nil {
		t.Fatalf("第一版没合并时不挑后面的：%+v", e.state().Jobs)
	}
	var next *Job
	for _, j := range e.state().Jobs {
		if j.ID != id {
			next = j
		}
	}
	if next == nil || !next.Takeover || next.NewReport == nil || next.GateRound != 1 {
		t.Fatalf("要马上接手，还是第一版：%+v", next)
	}
	prompt := testkit.ReadFile(t, filepath.Join(jobHome(next, e), "prompt.txt"))
	if !strings.Contains(prompt, "门的报告") || !strings.Contains(prompt, ".agentq/verify") {
		t.Errorf("提示词要带门的报告：\n%s", prompt)
	}
	waitExit(t, next.PID, 15*time.Second)
	e.mustRun("tick")
	next = byID(next.ID)
	if next.Gate == nil || next.Gate.State != state.GateRunning {
		t.Fatalf("%+v %+v", next, next.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(next))
		return err == nil
	})
	e.mustRun("tick")
	next = byID(next.ID)
	if next.Gate.State != state.GateMerged || next.NewReport.Commit == "" {
		t.Fatalf("%+v %+v", next.Gate, next.NewReport)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "gate", next.ID, gate.DeliveryFile)); err != nil {
		t.Errorf("要有交付单：%v", err)
	}
	e.mustRun("pause")
}

// 烟测没过（默认 warn）：合并了，不交付，告诉用户。
func TestIntegrationNewSmokeFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWBADSMOKE 做工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	j = e.state().Jobs[0]
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.state().Jobs[0]
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "gate", j.ID, gate.DeliveryFile)); err == nil {
		t.Error("烟测没过不交付")
	}
	if !strings.Contains(e.nightly(), "smoke 门（warn）fail") {
		t.Errorf("要记下来：\n%s", e.nightly())
	}
}

// 第一条也绕不过门：写了 ready.json 又自己合并的不记成过门合并，等用户定。
func TestIntegrationNewSelfMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWSELFMERGE 做工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	j = e.state().Jobs[0]
	if j.Gate == nil || j.Gate.State != state.GateStuck {
		t.Fatalf("%+v %+v", j, j.Gate)
	}
	if !strings.Contains(j.Summary, "没过门") {
		t.Errorf("%s", j.Summary)
	}
}

// 分支上没有 ready.json、结尾写了第一条没做到可合并：还是 new 任务，按中断报，不过门。
func TestIntegrationNewNotReady(t *testing.T) {
	t.Parallel()
	for _, idea := range []string{"NEWNOREADY 做工具", "NEWNOTREADY 做工具"} {
		e := newEnv(t, map[string]any{"paused": true})
		e.mustRun("new", idea)
		j := e.state().Jobs[0]
		waitExit(t, j.PID, 15*time.Second)
		e.mustRun("tick")
		j = e.state().Jobs[0]
		if j.Kind != state.KindNew || j.Status != state.StatusInterrupted || j.ResumePending || j.Gate != nil || j.NewReport != nil {
			t.Fatalf("%s：%+v %+v", idea, j, j.Gate)
		}
		if !strings.Contains(j.Summary, "没有确认做完") {
			t.Errorf("%s：%s", idea, j.Summary)
		}
		if j.FirstItem == nil || j.FirstItem.Project != "clip-tool" || j.FirstItem.Title != "第一条活" {
			t.Errorf("%s：要记下第一条：%+v", idea, j.FirstItem)
		}
	}
}

// stuckFirst 走到复现的现场：第一条过门失败退回，接手任务退出时分支上还是没过门的那个提交，下一次 tick 标卡住。
func stuckFirst(t *testing.T, cfg map[string]any) (e *testEnv, firstID string, takeover *Job, dir string) {
	t.Helper()
	e = newEnv(t, cfg)
	e.mustRun("new", "NEWFIRSTSTUCK 做工具")
	j := e.state().Jobs[0]
	firstID = j.ID
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	e.waitGate(jobOf(t, e, firstID))
	for _, x := range e.state().Jobs {
		if x.ID != firstID {
			takeover = x
		}
	}
	if takeover == nil || !takeover.Takeover || takeover.NewReport == nil {
		t.Fatalf("要有接手任务：%+v", e.state().Jobs)
	}
	waitExit(t, takeover.PID, 15*time.Second)
	dir = filepath.Join(testkit.RealPath(t, e.root), "clip-tool")
	waitFor(t, 30*time.Second, "第一条标卡住", func() bool {
		e.mustRun("tick")
		return strings.Contains(testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")), "第一条死活（卡住")
	})
	if e.findJob("第二条活") != nil {
		t.Fatalf("第一版没合并不开工后面的：%+v", e.state().Jobs)
	}
	return e, firstID, jobOf(t, e, takeover.ID), dir
}

func jobOf(t *testing.T, e *testEnv, id string) *Job {
	t.Helper()
	for _, j := range e.state().Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("没有任务 %s", id)
	return nil
}

// editBacklogLine 改 BACKLOG 并提交，像用户手改。
func editBacklogLine(t *testing.T, e *testEnv, dir string, fn func(line string) (string, bool)) {
	t.Helper()
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	var out []string
	for _, l := range strings.Split(testkit.ReadFile(t, p), "\n") {
		if nl, keep := fn(l); keep {
			out = append(out, nl)
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, dir, "add", "docs/BACKLOG.md")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "用户改待办")
}

// 接手失败后把第一条删掉：下一条开工。
func TestIntegrationFirstStuckDeleteItem(t *testing.T) {
	t.Parallel()
	e, _, _, dir := stuckFirst(t, nil)
	editBacklogLine(t, e, dir, func(l string) (string, bool) { return l, !strings.Contains(l, "第一条死活") })
	e.mustRun("tick")
	next := e.findJob("第二条活")
	if next == nil {
		t.Fatalf("第一条不在了要放开：%+v", e.state().Jobs)
	}
	waitExit(t, next.PID, 10*time.Second)
}

// 去掉卡住标记：第一条被接走（从分支接着做，还是第一版），第二条继续等。
func TestIntegrationFirstStuckClearMark(t *testing.T) {
	t.Parallel()
	e, firstID, takeover, dir := stuckFirst(t, nil)
	editBacklogLine(t, e, dir, func(l string) (string, bool) {
		if i := strings.Index(l, "（卡住"); i >= 0 && strings.Contains(l, "第一条死活") {
			return l[:i], true
		}
		return l, true
	})
	e.mustRun("tick")
	if e.findJob("第二条活") != nil {
		t.Fatalf("第二条继续等第一版：%+v", e.state().Jobs)
	}
	var next *Job
	for _, j := range e.state().Jobs {
		if j.ID != firstID && j.ID != takeover.ID {
			next = j
		}
	}
	if next == nil || next.Title != "第一条死活" || next.NewReport == nil || !next.Handoff {
		t.Fatalf("第一条要被接走，还是第一版：%+v", next)
	}
	if prompt := testkit.ReadFile(t, filepath.Join(jobHome(next, e), "prompt.txt")); !strings.Contains(prompt, "agent/first") {
		t.Errorf("从分支接着做：\n%s", prompt)
	}
	waitExit(t, next.PID, 10*time.Second)
}

// gate abandon 处理没有门记录的接手任务：第一条标卡住，下一条开工；只记一次审计。第一个任务（门记录是退回）也能放弃。
func TestIntegrationFirstStuckAbandon(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"接手的", "第一个"} {
		e, firstID, takeover, dir := stuckFirst(t, nil)
		id := takeover.ID
		if which == "第一个" {
			id = firstID
		}
		if takeover.Gate != nil {
			t.Fatalf("接手任务没有门记录才是这个用例：%+v", takeover.Gate)
		}
		out, code := e.run("gate", "pass", id, "--reason", "x")
		if code == 0 || !strings.Contains(out, "agentq gate abandon "+id) {
			t.Errorf("放行不了时要说出路：%d %s", code, out)
		}
		if out := e.mustRun("gate", "abandon", id); !strings.Contains(out, "记下了") {
			t.Errorf("%s", out)
		}
		e.mustRun("tick")
		next := e.findJob("第二条活")
		if next == nil {
			t.Fatalf("%s：放弃后下一条开工：%+v", which, e.state().Jobs)
		}
		waitExit(t, next.PID, 10*time.Second)
		if bl := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")); !strings.Contains(bl, "第一条死活（卡住") {
			t.Errorf("条目和标记留着：\n%s", bl)
		}
		if got := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/first"); got == "" {
			t.Error("分支留着")
		}
		if out := e.mustRun("status"); strings.Contains(out, "第一版还没合并") {
			t.Errorf("放弃后 status 不再挡：\n%s", out)
		}
		// 接手任务放弃后再放行：没人看过要合哪个提交，不能把分支尖合进去。第一个任务有用户看过的提交，放行照常
		if which == "接手的" {
			before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
			if out, code := e.run("gate", "pass", id, "--reason", "还是要"); code == 0 || !strings.Contains(out, "没有可合的提交") {
				t.Errorf("放弃后放行要拒绝：%d %s", code, out)
			}
			e.mustRun("tick")
			if after := testkit.GitT(t, e.home, dir, "rev-parse", "main"); after != before {
				t.Errorf("main 不能因为放行动：%s -> %s", before, after)
			}
		}
		audit := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "gate", "audit.log"))
		if strings.Count(audit, "放弃 "+id) != 1 {
			t.Errorf("审计：%s", audit)
		}
	}
}

// status 明说：哪个项目、第一条卡住了、三种解法。tick 日志说真实原因，同样的只说一次，不再说「等它结束」。
func TestIntegrationFirstStuckStatusAndLog(t *testing.T) {
	t.Parallel()
	e, _, takeover, _ := stuckFirst(t, nil)
	status := e.mustRun("status")
	for _, want := range []string{"新项目 clip-tool 第一版还没合并", "第一条「第一条死活」卡住了", "重做：", "docs/BACKLOG.md", "分支 agent/first", "放弃：agentq gate abandon " + takeover.ID, "删条目："} {
		if !strings.Contains(status, want) {
			t.Errorf("status 缺 %q：\n%s", want, status)
		}
	}
	a, b := e.mustRun("tick"), e.mustRun("tick")
	for _, out := range []string{a, b} {
		if strings.Contains(out, "等它结束再挑") || strings.Contains(out, "第二条活") {
			t.Errorf("任务早结束了：\n%s", out)
		}
		if strings.Contains(out, "第一版还没合并") {
			t.Errorf("同样的原因只说一次，前面已经说过：\n%s", out)
		}
	}
}

// 结尾写第一条没做到可合并：同样挡住，status 说清楚。去掉进行中标记，第一条从分支接着做，合并后第二条才开工，还有交付单。
func TestIntegrationNewNotReadyHolds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWNOTREADY 做工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	status := e.mustRun("status")
	for _, want := range []string{"新项目 clip-tool 第一版还没合并", "第一条「第一条活」标着进行中，没人在做", "从分支 agent/first", "放弃：agentq gate abandon " + j.ID, "删条目："} {
		if !strings.Contains(status, want) {
			t.Errorf("status 缺 %q：\n%s", want, status)
		}
	}
	e.mustRun("resume")
	e.mustRun("tick")
	if e.findJob("第二条活") != nil || len(e.state().Jobs) != 1 {
		t.Fatalf("第一版不在 main 上，第二条不开工：%+v", e.state().Jobs)
	}
	dir := filepath.Join(testkit.RealPath(t, e.root), "clip-tool")
	editBacklogLine(t, e, dir, func(l string) (string, bool) {
		if i := strings.Index(l, "（进行中"); i >= 0 {
			return l[:i], true
		}
		return l, true
	})
	e.mustRun("tick")
	if e.findJob("第二条活") != nil {
		t.Fatalf("第二条继续等：%+v", e.state().Jobs)
	}
	var next *Job
	for _, x := range e.state().Jobs {
		if x.ID != j.ID {
			next = x
		}
	}
	if next == nil || next.Title != "第一条活" || next.NewReport == nil || !next.Handoff {
		t.Fatalf("第一条被接走，还是第一版：%+v", next)
	}
	if prompt := testkit.ReadFile(t, filepath.Join(jobHome(next, e), "prompt.txt")); !strings.Contains(prompt, "agent/first") {
		t.Errorf("从分支接着做：\n%s", prompt)
	}
	waitExit(t, next.PID, 15*time.Second)
}

// 没改认的 new 任务也能放弃。
func TestIntegrationNewNotReadyAbandon(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWNOTREADY 做工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	e.mustRun("gate", "abandon", j.ID)
	e.mustRun("tick")
	dir := filepath.Join(testkit.RealPath(t, e.root), "clip-tool")
	if bl := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")); !strings.Contains(bl, "第一条活（卡住：用户放弃") {
		t.Errorf("\n%s", bl)
	}
	e.mustRun("resume")
	e.mustRun("tick")
	next := e.findJob("第二条活")
	if next == nil {
		t.Fatalf("放弃后下一条开工：%+v", e.state().Jobs)
	}
	waitExit(t, next.PID, 10*time.Second)
}

// 第一版挡着、主目录又切走了：tick 日志和 status 只说切分支这个真原因；工作目录里是别的分支的 BACKLOG，不能当第一条「不在了」放开后面的条目。切回来又是第一版的原因。
func TestIntegrationFirstHoldAndBranchBlocked(t *testing.T) {
	t.Parallel()
	e, _, _, dir := stuckFirst(t, nil)
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "别的")
	os.Remove(filepath.Join(dir, "docs", "BACKLOG.md"))
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "这条分支上没有 BACKLOG")
	out := e.mustRun("tick")
	if !strings.Contains(out, "不接任务") || strings.Contains(out, "第一版还没合并") || !strings.Contains(out, "switch main") {
		t.Errorf("tick 只说切分支：\n%s", out)
	}
	if e.findJob("第二条活") != nil {
		t.Fatalf("切走了不开工：%+v", e.state().Jobs)
	}
	status := e.mustRun("status")
	if !strings.Contains(status, "不接任务") || strings.Contains(status, "第一版还没合并") {
		t.Errorf("status 只说切分支：\n%s", status)
	}
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	e.mustRun("tick")
	if e.findJob("第二条活") != nil {
		t.Fatalf("切回来第一版还挡着：%+v", e.state().Jobs)
	}
	if status := e.mustRun("status"); !strings.Contains(status, "第一版还没合并") || strings.Contains(status, "不接任务") {
		t.Errorf("切回来说第一版：\n%s", status)
	}
}

// 新项目默认分支是 master：第一条记 master，合进 master，后面的条目接得上。
func TestIntegrationNewOnMaster(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": true})
	e.mustRun("new", "NEWMASTER 做工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 15*time.Second)
	e.mustRun("tick")
	j = e.state().Jobs[0]
	if j.DefaultBranch != "master" || j.Gate == nil {
		t.Fatalf("%+v %+v", j, j.Gate)
	}
	e.waitGate(j)
	j = e.state().Jobs[0]
	dir := filepath.Join(testkit.RealPath(t, e.root), "clip-tool")
	if j.Gate.State != state.GateMerged || testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/master") != nil {
		t.Fatalf("%+v", j.Gate)
	}
	if testkit.GitTErr(e.home, dir, "rev-parse", "--verify", "--quiet", "refs/heads/main") == nil {
		t.Error("不该多出 main")
	}
}

// new 任务还在跑时，它的第一条不算孤儿：标记时间再老、分支还没提交，也不能被去掉。不然结尾的分支对不上，第一条改认不了。
func TestOrphanScanSkipsRunningNew(t *testing.T) {
	st := &State{Jobs: []*Job{{Kind: state.KindNew, Status: state.StatusRunning, ProjectsBefore: []string{"old"}}}}
	if !st.NewProjectBusy("fresh") || st.NewProjectBusy("old") {
		t.Errorf("启动后才出现的算它的")
	}
	e := newEnv(t, map[string]any{"max_parallel": 3})
	e.mustRun("new", "SLEEP=8 做个工具")
	j := e.state().Jobs[0]
	// 真 git 项目，标记很老，分支还没有提交：没有 new 任务的话这一条会被去掉标记
	dir := gateRepo(t, e, "fresh", "- [ ] 第一条（进行中：agent/first 2026-01-01 10:00）")
	out := e.mustRun("tick")
	bl := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md"))
	if !strings.Contains(bl, "（进行中：agent/first") || strings.Contains(out, "孤儿标记") {
		t.Errorf("不能去掉：\n%s\n%s", out, bl)
	}
	waitExit(t, j.PID, 20*time.Second)
}

// 通知不重复：warn 档烟测没过，合并时只推一条 alert，没有 done，也没有 new 自己那一条。
func TestIntegrationNewSmokeFailNotifiesOnce(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"discuss_enabled": false})
	now := time.Now()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).Format(time.RFC3339))
	e.mustRun("new", "NEWBADSMOKE 做个工具")
	j := e.state().Jobs[0]
	waitExit(t, j.PID, 10*time.Second)
	e.mustRun("tick")
	e.waitGate(e.state().Jobs[0])
	var hit []string
	for _, x := range tg.texts() {
		if strings.Contains(x, "clip-tool") || strings.Contains(x, "新项目") {
			hit = append(hit, x)
		}
	}
	if len(hit) != 1 || !strings.Contains(hit[0], "烟测没过，没有交付单") || strings.Contains(hit[0], "🟢") {
		t.Fatalf("应正好推一条 alert：%q", hit)
	}
	e.mustRun("pause")
}
