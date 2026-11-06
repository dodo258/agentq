package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 能力降级：调研 §4.4 每一行一个用例，跑真的 runner，后端是假的。名字里的行号就是那张表的行。

type fakeRig struct {
	t    *testing.T
	e    *testEnv
	a    *App
	out  *bytes.Buffer
	name string
	st   *State
}

// fakeApp 建一个默认后端是假后端的 App。caps 从全有开始，mut 摘掉要测的那一项。
func fakeApp(t *testing.T, o testkit.FakeOpts, mut func(*backend.Caps)) *fakeRig {
	t.Helper()
	if o.Caps == (backend.Caps{}) {
		o.Caps = testkit.AllCaps()
	}
	if mut != nil {
		mut(&o.Caps)
	}
	if o.Auth.State == backend.AuthUnknown && o.Caps.AuthProbe {
		o.Auth.State = backend.AuthIn
	}
	name := testkit.RegisterFake(t, o)
	e := newEnv(t, map[string]any{"backend": name})
	var out bytes.Buffer
	a, err := newApp(e.home, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv("AGENTQ_FAKE_LOG", e.fakeLg)
	return &fakeRig{t: t, e: e, a: a, out: &out, name: name, st: &State{}}
}

// job 建一个正在跑的待做任务（已经写好任务目录），后端是假后端。
func (r *fakeRig) job(kind string, events ...testkit.FakeEvent) *Job {
	r.t.Helper()
	dir := testkit.MkProject(r.t, r.e.root, "demo"+fmt.Sprint(len(r.st.Jobs)), "- [ ] 修登录（进行中：agent/x 2026-03-15 09:00）\n")
	id := fmt.Sprintf("0315-10000%d-demo", len(r.st.Jobs))
	if err := os.MkdirAll(r.a.JobDir(id), 0o755); err != nil {
		r.t.Fatal(err)
	}
	items, err := backlog.Project{Dir: dir}.Items()
	if err != nil || len(items) == 0 {
		r.t.Fatalf("%v %v", items, err)
	}
	j := &Job{ID: id, Kind: kind, Backend: r.name, Project: filepath.Base(dir), ProjectDir: dir, Item: items[0].Text, Title: items[0].Title,
		Fingerprint: items[0].Fingerprint, Status: state.StatusRunning, StartedAt: time.Now().Add(-time.Hour)}
	r.st.Jobs = append(r.st.Jobs, j)
	testkit.WriteEvents(r.t, r.a.JobDir(id), events...)
	return j
}

func okResult(text string) testkit.FakeEvent {
	return testkit.FakeEvent{Type: "result", OK: true, Text: text, Turns: 5}
}

// startFake 真的起进程：真外壳 + 假 CLI 脚本。
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakecli")
	// 记下父进程（任务外壳）：testEnv.waitShells 靠它等外壳走完
	testkit.WriteInstall(t, p, "#!/bin/sh\n[ -n \"$AGENTQ_FAKE_LOG\" ] && echo \"PPID[$PPID]\" > \"$AGENTQ_FAKE_LOG/$$.txt\"\n"+body+"\n", 0o755)
	return p
}

func useRealShell(t *testing.T) {
	t.Helper()
	old := runner.JobExecutable
	runner.JobExecutable = func() (string, error) { return agentqBin, nil }
	t.Cleanup(func() { runner.JobExecutable = old })
}

// §4.4 美元预算：运行中读得到用量的，tick 超了发 SIGTERM，按撞预算收尾；读不到的只有超时，日志和 doctor 明说。
func TestDegradeBudgetLiveUsage(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"session","id":"s1"}'; echo '{"type":"usage","input":1,"cost":200}'; sleep 300; :`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli, LiveUsage: true}, func(c *backend.Caps) { c.HardBudget = false })
	useRealShell(t)
	t.Setenv("PATH", os.Getenv("PATH"))
	dir := testkit.MkProject(t, r.e.root, "demo", "- [ ] 修登录（进行中：agent/x 2026-03-15 09:00）\n")
	items, _ := backlog.Project{Dir: dir}.Items()
	j, err := r.a.Runner.Start(r.st, &Job{Project: "demo", ProjectDir: dir, Title: items[0].Title, Item: items[0].Text, Fingerprint: items[0].Fingerprint}, "做", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Backend != r.name {
		t.Fatalf("任务没记后端：%q", j.Backend)
	}
	if strings.Contains(strings.Join(j.Args, " "), "--budget") {
		t.Errorf("没有硬上限不该传预算：%v", j.Args)
	}
	if !strings.Contains(r.out.String(), "不是硬上限") {
		t.Errorf("日志要说预算不是硬上限：%s", r.out.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for j.Status == state.StatusRunning && time.Now().Before(deadline) {
		r.a.Runner.CheckLiveBudget(r.st)
		time.Sleep(50 * time.Millisecond)
	}
	if j.Status != state.StatusInterrupted || !j.BudgetHit || !j.ResumePending {
		t.Fatalf("应按撞预算收尾：%s budget=%v resume=%v %s", j.Status, j.BudgetHit, j.ResumePending, j.Summary)
	}
	if !strings.Contains(j.Summary, "撞到预算上限") {
		t.Error(j.Summary)
	}
	if j.SessionID != "s1" && j.SessionID == "" {
		t.Errorf("会话 id：%q", j.SessionID)
	}
}

func TestDegradeBudgetNoLiveUsage(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{Path: "/bin/true"}, func(c *backend.Caps) { c.HardBudget = false })
	be := r.a.BackendFor(r.name)
	_, degraded, err := be.Build(backend.Request{Prompt: "p", Label: "l", BudgetUSD: 10})
	if err != nil || len(degraded) != 1 || !strings.Contains(degraded[0].Note, "job_budget_usd 不生效") {
		t.Errorf("%v %v", degraded, err)
	}
	j := r.job("", testkit.FakeEvent{Type: "usage", Cost: 999})
	r.a.Runner.CheckLiveBudget(r.st)
	if j.Status != state.StatusRunning {
		t.Error("读不到运行中用量的后端不该被 tick 停")
	}
	// agentq start 要明说
	r.out.Reset()
	r.a.Onboard.StartReport()
	if !strings.Contains(r.out.String(), "job_budget_usd 不生效") {
		t.Errorf("agentq start 要提醒：%s", r.out.String())
	}
}

// §4.4 回合上限：靠回合收住的短任务用短超时。
func TestDegradeMaxTurnsShortTimeout(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.MaxTurns = false })
	for _, k := range []string{state.KindRouter, state.KindSuggest, state.KindSense} {
		if got := r.a.Runner.JobTimeout(&Job{Kind: k, Backend: r.name}); got != 10*time.Minute {
			t.Errorf("%s：%v", k, got)
		}
	}
	if got := r.a.Runner.JobTimeout(&Job{Backend: r.name}); got != r.a.Cfg.Timeout.Duration {
		t.Errorf("待做不受影响：%v", got)
	}
	r2 := fakeApp(t, testkit.FakeOpts{}, nil)
	if got := r2.a.Runner.JobTimeout(&Job{Kind: state.KindRouter, Backend: r2.name}); got == 10*time.Minute {
		t.Errorf("有回合上限不该用短超时：%v", got)
	}
}

// §4.4 结构化输出：追问靠提示词要 JSON 并校验，失败当没结果；外部输入的任务不用这个补法，直接不启动。
func TestDegradeSchemaFallback(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"这是回答 {\"reply\":\"你好\",\"todo\":\"\"}"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, func(c *backend.Caps) { c.Schema = false })
	useRealShell(t)
	r.st.Jobs = append(r.st.Jobs, &Job{ID: "orig", SessionID: "s1", Backend: r.name, Status: state.StatusDone})
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindFollowup, Project: "demo", ProjectDir: dir, Title: "追问"}, "问", "s1")
	if err != nil {
		t.Fatal(err)
	}
	r.e.waitShells()
	prompt := string(testkit.ReadBytes(t, filepath.Join(r.a.JobDir(j.ID), "prompt.txt")))
	if !strings.Contains(prompt, "只输出一个 JSON") || !strings.Contains(prompt, `"reply"`) {
		t.Errorf("提示词没要求 JSON：%s", prompt)
	}
	if strings.Contains(strings.Join(j.Args, " "), "--schema") {
		t.Errorf("CLI 不能强制就不该传：%v", j.Args)
	}
	res := r.a.Ledger.ReadResult(j)
	if !strings.Contains(string(res.Structured), "你好") {
		t.Errorf("没取到最后一个 JSON：%+v", res)
	}
	// 校验不过：当这次没结果，不把原文当回答
	testkit.WriteEvents(t, r.a.JobDir(j.ID), okResult(`答 {"wrong":1}`))
	if res := r.a.Ledger.ReadResult(j); len(res.Structured) != 0 || res.Text != "" {
		t.Errorf("校验不过应当没结果：%+v", res)
	}
	// 外部输入的任务不用这个补法
	for _, kind := range []string{state.KindSense, state.KindSuggest, state.KindRelease, state.KindRouter} {
		_, err := r.a.Runner.Start(r.st, &Job{Kind: kind, Project: "demo", ProjectDir: dir, Title: "x"}, "p", "")
		if !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("%s 应该拒绝：%v", kind, err)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	schema := chat.FollowupSchema
	cases := map[string]bool{
		`{"reply":"x"}`: true,
		"前面的话\n```json\n{\"reply\":\"x\"}\n```": true,
		`{"reply":"旧"} 中间 {"reply":"新"}`:        true,
		`{"reply":1}`:                           false,
		`{"todo":"x"}`:                          false,
		`{"reply":"x","extra":1}`:               false,
		`没有 JSON`:                               false,
		`{"reply":"x"} 后面还有话`:                   false,
	}
	for in, want := range cases {
		if _, ok := backend.ExtractJSON(in, schema); ok != want {
			t.Errorf("%q：%v，要 %v", in, ok, want)
		}
	}
	if raw, _ := backend.ExtractJSON(`{"reply":"旧"} 中间 {"reply":"新"}`, schema); !strings.Contains(string(raw), "新") {
		t.Errorf("应取最后一个：%s", raw)
	}
}

// §4.4 工具限制、拒绝规则、隔离 MCP：声明 Must 的任务不启动，如实说原因；Router 走对话任务，巡逻分析不起，巡逻照常。
func TestDegradeMustRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		mut   func(*backend.Caps)
		kinds []string
		want  string
	}{
		"只读":     {func(c *backend.Caps) { c.ToolProfiles = false }, []string{state.KindSense, state.KindSuggest, state.KindRouter}, "不能强制只读"},
		"拒绝规则":   {func(c *backend.Caps) { c.DenyRules = false }, []string{state.KindSuggest, state.KindRouter, state.KindRelease}, "不能强制拒绝规则"},
		"隔离 MCP": {func(c *backend.Caps) { c.NoExternalMCP = false }, []string{state.KindSense, state.KindSuggest, state.KindRouter}, "不能隔离"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fakeApp(t, testkit.FakeOpts{Path: "/bin/true"}, tc.mut)
			dir := testkit.MkProject(t, r.e.root, "demo", "")
			before, _ := filepath.Glob(filepath.Join(r.e.home, ".agentq", "jobs", "*"))
			for _, k := range tc.kinds {
				j, err := r.a.Runner.Start(r.st, &Job{Kind: k, Project: "demo", ProjectDir: dir, Title: "x"}, "p", "")
				if j != nil || !errors.Is(err, backend.ErrUnsupported) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s：%v %v", k, j, err)
				}
			}
			after, _ := filepath.Glob(filepath.Join(r.e.home, ".agentq", "jobs", "*"))
			if len(after) != len(before) {
				t.Errorf("拒绝时不该留任务目录：%v", after)
			}
			if len(r.st.Jobs) != 0 {
				t.Errorf("拒绝时不该有任务记录：%v", r.st.Jobs)
			}
		})
	}
}

func TestDegradeFeaturesOffWithoutProfile(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{Path: "/bin/true"}, func(c *backend.Caps) { c.ToolProfiles = false })
	if ok, why := r.a.Runner.CheckReady(backend.MustSuggest); ok || !strings.Contains(why, "不能强制只读") {
		t.Errorf("%v %q", ok, why)
	}
	if why := r.a.Runner.Lacks(backend.MustRouter); why == "" {
		t.Error("Router 应该被关")
	}
	if ok, why := r.a.Runner.CheckReady(backend.MustPatrol); ok || why == "" {
		t.Error("巡逻分析应该被关")
	}
	if why := r.a.Runner.Lacks(backend.MustFollowup); why == "" {
		t.Error("追问应该回退")
	}
	if ok, _ := r.a.Runner.CheckLogin(); !ok {
		t.Error("普通任务不受影响")
	}
}

// 合并前审查是硬前提：引擎起不了审查进程的后端，待做和 new 如实拒绝，不假装有；不合并的任务照常。
func TestDegradeNoReviewRefusesMerge(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"结论"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, func(c *backend.Caps) { c.ReviewProfile = false })
	useRealShell(t)
	dir := testkit.MkProject(t, r.e.root, "demo", "- [ ] 修登录\n")
	for _, k := range []string{"", state.KindNew} {
		j, err := r.a.Runner.Start(r.st, &Job{Kind: k, Project: "demo", ProjectDir: dir, Title: "修登录", Item: "修登录"}, "做", "")
		if j != nil || !errors.Is(err, backend.ErrUnsupported) || !strings.Contains(err.Error(), "独立审查") {
			t.Errorf("Kind=%q：%v %v", k, j, err)
		}
	}
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, "问", "")
	if err != nil {
		t.Fatalf("对话不合并，不该拒绝：%v", err)
	}
	r.e.waitShells()
	r.a.Runner.Reap(r.st)
	if j.Status != state.StatusDone {
		t.Errorf("%s %s", j.Status, j.Summary)
	}
}

// 不能强制结构化输出的后端，引擎也起不了审查进程：待做和 new 不接活。
func TestDegradeNoSchemaRefusesMerge(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"结论"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, func(c *backend.Caps) { c.Schema = false })
	dir := testkit.MkProject(t, r.e.root, "demo", "- [ ] 修登录\n")
	j, err := r.a.Runner.Start(r.st, &Job{Project: "demo", ProjectDir: dir, Title: "修登录", Item: "修登录"}, "做", "")
	if j != nil || !errors.Is(err, backend.ErrUnsupported) || !strings.Contains(err.Error(), "独立审查") {
		t.Errorf("%v %v", j, err)
	}
}

// §4.4 会话预设 id：从输出取；取到之前被杀就没有 id，走接手。
func TestDegradeNoPresetSession(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"session","id":"from-stream"}'; echo '{"type":"result","ok":true,"text":"合并了，提交 abc1234"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, func(c *backend.Caps) { c.PresetSession = false })
	useRealShell(t)
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, "问", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.SessionID != "" || strings.Contains(strings.Join(j.Args, " "), "--session") {
		t.Errorf("不该预设会话：%q %v", j.SessionID, j.Args)
	}
	r.e.waitShells()
	r.a.Runner.Reap(r.st)
	if j.SessionID != "from-stream" {
		t.Errorf("应从输出取会话 id：%q", j.SessionID)
	}
	// 被杀：输出里已经有 id 的还能接；没有的没有
	killed := r.job("", testkit.FakeEvent{Type: "session", ID: "s2"})
	r.a.Runner.Finish(r.st, killed, state.StatusInterrupted, "被杀")
	if killed.SessionID != "s2" {
		t.Errorf("被杀的也要取到：%q", killed.SessionID)
	}
	early := r.job("")
	r.a.Runner.Finish(r.st, early, state.StatusInterrupted, "被杀")
	if early.SessionID != "" {
		t.Errorf("还没取到就被杀：%q", early.SessionID)
	}
}

// §4.4 没有续跑（含会话 id 到结束才有）：不标续跑，交给接手；接手不用等 24 小时；回复链回退。
func TestDegradeNoResume(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{Path: "/bin/true"}, func(c *backend.Caps) { c.Resume, c.PresetSession = false, false })
	j := r.job("", testkit.FakeEvent{Type: "session", ID: "s1"}, okResult("做了一半"))
	r.a.Runner.FinishFromResult(r.st, j)
	if j.Status != state.StatusInterrupted || j.ResumePending || j.SessionID != "" {
		t.Fatalf("%s resume=%v session=%q %s", j.Status, j.ResumePending, j.SessionID, j.Summary)
	}
	if !strings.Contains(j.Summary, "不能续跑") || !strings.Contains(j.Summary, "接手") {
		t.Error(j.Summary)
	}
	// 接手：分支有内容就马上接，不等 orphanIdle
	testkit.GitT(t, r.e.home, j.ProjectDir, "init", "-q", "-b", "main")
	testkit.GitT(t, r.e.home, j.ProjectDir, "add", "-A")
	testkit.GitT(t, r.e.home, j.ProjectDir, "commit", "-q", "-m", "init")
	testkit.GitT(t, r.e.home, j.ProjectDir, "checkout", "-q", "-b", "agent/x")
	testkit.WriteInstall(t, filepath.Join(j.ProjectDir, "new.txt"), "x", 0o644)
	testkit.GitT(t, r.e.home, j.ProjectDir, "add", "-A")
	testkit.GitT(t, r.e.home, j.ProjectDir, "commit", "-q", "-m", "wip")
	testkit.GitT(t, r.e.home, j.ProjectDir, "checkout", "-q", "main")
	found := r.a.Orphan.ScanOrphans(r.st)
	if len(found) != 1 || found[0].Facts.Branch != "agent/x" {
		t.Fatalf("应该马上接手：%+v；日志 %s", found, r.out.String())
	}
	// 启动时带 resume 也如实拒绝
	if _, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: j.ProjectDir, Title: "x"}, "p", "s1"); !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("不能续跑的后端不该接受 resume：%v", err)
	}
	if r.a.BackendFor(r.name).CanResume("s1") {
		t.Error("CanResume 应该是假")
	}
}

// §4.4 用量数字：没有就写未知，合计注明，瞬时错误退回只看回合数。只有 token 的写费用未知。
func TestDegradeNoUsage(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.Usage, c.Cost = false, false })
	j := r.job(state.KindChat, okResult("答"))
	r.a.Runner.FinishFromResult(r.st, j)
	if j.Usage != nil {
		t.Fatalf("%+v", j.Usage)
	}
	if !strings.Contains(r.e.nightly(), "用量未知") {
		t.Errorf("当天汇总要写未知：%s", r.e.nightly())
	}
	if err := state.Save(r.a.StatePath(), r.st); err != nil {
		t.Fatal(err)
	}
	r.out.Reset()
	if err := r.a.CmdStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "用量未知") {
		t.Errorf("status 要写未知：%s", r.out.String())
	}
	if f := r.a.Ledger.CostFooter(r.st, *j.EndedAt); !strings.Contains(f, "1 个任务没有用量") {
		t.Errorf("合计要注明：%s", f)
	}

	cheap := r.job("", testkit.FakeEvent{Type: "result", Status: 503, Turns: 1, Text: "上游挂了"})
	r.a.Runner.FinishFromResult(r.st, cheap)
	if !cheap.Transient {
		t.Errorf("没有用量、回合很少算瞬时错误：%s", cheap.Summary)
	}
	worked := r.job("", testkit.FakeEvent{Type: "result", Status: 503, Turns: 50, Text: "做到一半挂了"})
	r.a.Runner.FinishFromResult(r.st, worked)
	if worked.Transient {
		t.Errorf("回合多了不算瞬时错误：%s", worked.Summary)
	}

	tok := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.Cost = false })
	k := tok.job(state.KindChat, testkit.FakeEvent{Type: "usage", Input: 1000, Cost: 0}, okResult("答"))
	tok.a.Runner.FinishFromResult(tok.st, k)
	if k.Usage == nil || !k.Usage.CostUnknown {
		t.Fatalf("%+v", k.Usage)
	}
	if f := tok.a.Ledger.CostFooter(tok.st, *k.EndedAt); !strings.Contains(f, "费用未知") || !strings.Contains(f, "只有 token 没有金额") {
		t.Errorf("%s", f)
	}
	if s := backend.JobUsageShort(*k.Usage); strings.Contains(s, "$") {
		t.Errorf("没有金额不能写 $：%s", s)
	}
	work := tok.job("", testkit.FakeEvent{Type: "usage", Input: 1000}, testkit.FakeEvent{Type: "result", Status: 503, Turns: 50})
	tok.a.Runner.FinishFromResult(tok.st, work)
	if work.Transient {
		t.Errorf("没有金额也退回看回合数：%s", work.Summary)
	}
}

// §4.4 额度查询：当未知，照常跑。
func TestDegradeNoQuota(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, nil)
	u := r.a.Runner.Usage(r.st)
	lvl, _, _ := backend.UsageLevel(u, time.Now(), r.a.Cfg.Model)
	if lvl != backend.UsageUnknown || u.Err == "" {
		t.Errorf("%d %+v", lvl, u)
	}
	if !strings.Contains(backend.UsageDescribe(u, time.Now(), r.a.Cfg.Model), "照常接活") {
		t.Error(backend.UsageDescribe(u, time.Now(), r.a.Cfg.Model))
	}
	if _, err := r.a.Runner.ManualGate(r.st); err != nil {
		t.Errorf("额度未知不该挡：%v", err)
	}
}

// §4.4 撞限信号：只认错误通道，不扫模型正文；解析不出重置时间按 limit_cooldown。
func TestDegradeLimitSignal(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, nil)
	body := r.job("", okResult("修好了 usage limit 的提示，已合并"))
	r.a.Runner.FinishFromResult(r.st, body)
	if body.LimitHit || !r.st.CooldownUntil.IsZero() {
		t.Errorf("模型正文不算撞限：%+v", body.Summary)
	}
	hit := r.job("")
	testkit.WriteInstall(t, filepath.Join(r.a.JobDir(hit.ID), "stderr.log"), "You've hit your usage limit\n", 0o644)
	r.a.Runner.FinishFromResult(r.st, hit)
	if !hit.LimitHit {
		t.Fatalf("错误通道要认：%s", hit.Summary)
	}
	want := time.Now().Add(r.a.Cfg.LimitCooldown.Duration)
	if d := want.Sub(r.st.CooldownUntil); d < -time.Minute || d > time.Minute {
		t.Errorf("应按 limit_cooldown 冷却：%v", r.st.CooldownUntil)
	}

	none := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.LimitSignal = false })
	j := none.job("")
	testkit.WriteInstall(t, filepath.Join(none.a.JobDir(j.ID), "stderr.log"), "You've hit your usage limit\n", 0o644)
	none.a.Runner.FinishFromResult(none.st, j)
	if j.LimitHit {
		t.Error("没有信号的后端认不出，落到普通失败")
	}
	if j.Status != state.StatusFailed {
		t.Error(j.Status)
	}
}

// §4.4 认证探测：查不了照常启动；第一次因认证失败不计连环失败，通知，暂停该后端。
func TestDegradeAuthUnknown(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.AuthProbe = false })
	r.a.Cfg.NotifyEnabled = true
	if ok, why := r.a.Runner.CheckLogin(); !ok {
		t.Fatalf("查不了应该照常：%s", why)
	}
	streak := r.st.FailStreak
	j := r.job("", testkit.FakeEvent{Type: "result", Auth: true, Text: "not logged in"})
	r.a.Runner.FinishFromResult(r.st, j)
	if j.Status != state.StatusFailed || !j.LimitHit {
		t.Fatalf("%s %v", j.Status, j.LimitHit)
	}
	if r.st.FailStreak != streak {
		t.Errorf("认证失败不计连环失败：%d", r.st.FailStreak)
	}
	if ok, why := r.a.Runner.CheckLogin(); ok || !strings.Contains(why, "认证失败") {
		t.Errorf("应该暂停：%v %q", ok, why)
	}
	ns, err := r.a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	if len(ns.Pending) != 1 || !strings.Contains(ns.Pending[0].Text, "认证失败") {
		t.Errorf("要通知：%+v", ns.Pending)
	}
	// 重新登录（删记录）就恢复
	if err := os.RemoveAll(filepath.Join(r.e.home, ".agentq", "backend-auth")); err != nil {
		t.Fatal(err)
	}
	if ok, why := r.a.Runner.CheckLogin(); !ok {
		t.Errorf("删记录后应恢复：%s", why)
	}

	out := fakeApp(t, testkit.FakeOpts{Auth: backend.Auth{State: backend.AuthOut, Detail: "假后端没登录"}}, nil)
	if ok, why := out.a.Runner.CheckLogin(); ok || why != "假后端没登录" {
		t.Errorf("%v %q", ok, why)
	}
}

// §4.4 会话记录路径、事件没有时间戳：读 agentq 自己保存的事件流，时间取首次被 tick 看到的时刻。
func TestDegradeOwnEventStream(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, nil)
	j := r.job("", testkit.FakeEvent{Type: "session", ID: "s1"})
	idle, ok := r.a.Runner.IdleFor(j)
	if !ok || idle > time.Minute {
		t.Errorf("刚写过的事件流：%v %v", idle, ok)
	}

	nt := fakeApp(t, testkit.FakeOpts{NoTimestamps: true, ToolLog: true}, nil)
	k := nt.job("", testkit.FakeEvent{Type: "tool", Name: "Bash", Cmd: "ls"})
	if _, ok := nt.a.Runner.IdleFor(k); !ok { // 第一次 tick 看到
		t.Fatal("应该判断得了")
	}
	dir := nt.a.JobDir(k.ID)
	first := backend.LastSeen(dir, "result.json")
	if first.IsZero() {
		t.Fatal("应该记下首次看到的时刻")
	}
	log := nt.a.BackendFor(nt.name).(backend.ToolLog)
	ev, _, err := log.ToolEvents(nt.a.Ledger.JobRef(k), false)
	if err != nil || len(ev) != 1 || !ev[0].At.Equal(first) {
		t.Fatalf("%v %v", ev, err)
	}
	// 没变长不再记；变长了后面的事件是新的时刻
	time.Sleep(5 * time.Millisecond)
	nt.a.Runner.IdleFor(k)
	if !backend.LastSeen(dir, "result.json").Equal(first) {
		t.Error("没变长不该更新")
	}
	testkit.WriteEvents(t, dir, testkit.FakeEvent{Type: "tool", Name: "Bash", Cmd: "ls"}, testkit.FakeEvent{Type: "tool", Name: "Edit"})
	nt.a.Runner.IdleFor(k)
	ev, _, err = log.ToolEvents(nt.a.Ledger.JobRef(k), false)
	if err != nil || len(ev) != 2 || !ev[0].At.Equal(first) || !ev[1].At.After(first) {
		t.Fatalf("%v %v", ev, err)
	}
}

// §4.4 看不到工具调用：看进展记未知不报，不杀；疑似卡死还在。审查证据也是未知。
func TestDegradeNoToolLog(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, nil)
	j := r.job("", testkit.FakeEvent{Type: "session", ID: "s1"})
	j.PID = os.Getpid()
	r.a.Runner.CheckProgress(r.st)
	if !runner.Noted(j, "unknown") {
		t.Errorf("应记未知：%v", j.ProgressNoted)
	}
	if !strings.Contains(r.out.String(), "当作未知") || j.Status != state.StatusRunning {
		t.Errorf("%s %s", j.Status, r.out.String())
	}
	if _, ok := r.a.Runner.IdleFor(j); !ok {
		t.Error("疑似卡死还要能判断")
	}
}

// §4.4 --disallowed-tools Monitor：只有 Claude 要；提示词里的这句由能力决定。
func TestDegradeMonitorWords(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"答"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, nil)
	useRealShell(t)
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, chat.ChatPromptHead, "")
	if err != nil {
		t.Fatal(err)
	}
	r.e.waitShells()
	prompt := string(testkit.ReadBytes(t, filepath.Join(r.a.JobDir(j.ID), "prompt.txt")))
	if strings.Contains(prompt, "Monitor") || !strings.Contains(prompt, "不用后台命令") {
		t.Errorf("没有 Monitor 的后端不提它：%s", prompt)
	}
	if strings.Contains(strings.Join(j.Args[1:], " "), "Monitor") {
		t.Errorf("%v", j.Args)
	}
}

// 配置写错：不回退，不接活，说清楚。
func TestUnknownBackendRefuses(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "nosuch"})
	testkit.MkProject(t, e.root, "demo", "- [ ] 修登录\n")
	out, code := e.run("tick")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if !strings.Contains(out, "未知后端") {
		t.Errorf("tick 要说明：%s", out)
	}
	if st := e.state(); st != nil && len(st.Jobs) != 0 {
		t.Errorf("不该接活：%+v", st.Jobs)
	}
	if got := e.claudeRuns(); got != 0 {
		t.Errorf("不能回退到 claude：%d 次", got)
	}
	if out, _ := e.run("status"); !strings.Contains(out, "未知后端") {
		t.Errorf("status 要说明：%s", out)
	}
}

// 按项目换后端；续跑跟着原后端，不跟随之后的配置。
func TestProjectBackendAndResumeSticks(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"答"}'`)
	name := testkit.RegisterFake(t, testkit.FakeOpts{Path: cli, Caps: testkit.AllCaps(), Auth: backend.Auth{State: backend.AuthIn}})
	e := newEnv(t, map[string]any{"project_backends": map[string]string{"demo": name}})
	var out bytes.Buffer
	a, err := newApp(e.home, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv("AGENTQ_FAKE_LOG", e.fakeLg)
	useRealShell(t)
	dir := testkit.MkProject(t, e.root, "demo", "- [ ] 修登录\n")
	st := &State{}
	j, err := a.Runner.Start(st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, "问", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitShells()
	if j.Backend != name {
		t.Fatalf("按项目换后端：%q", j.Backend)
	}
	if j2, err := a.Runner.Start(st, &Job{Kind: state.KindChat, Project: "别的", ProjectDir: dir, Title: "对话"}, "问", ""); err != nil || j2.Backend != "" {
		t.Errorf("别的项目用默认后端：%+v %v", j2, err)
	}
	e.waitShells()
	// 配置改了，续跑还用原后端
	a.Cfg.ProjectBackends = nil
	j.SessionID = "s-orig"
	r, err := a.Runner.ResumeJob(st, j, "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitShells()
	if r.Backend != name || !strings.Contains(strings.Join(r.Args, " "), "--resume s-orig") {
		t.Errorf("续跑应跟着原后端：%q %v", r.Backend, r.Args)
	}
}

// 别家的登录变量不传给 CLI；外壳最后变成的 tick 还带着全集。
func TestBackendEnvKeepReachesCLI(t *testing.T) {
	log := filepath.Join(t.TempDir(), "env.txt")
	cli := fakeCLI(t, `env > `+log+`; echo '{"type":"result","ok":true,"text":"答"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli, EnvKeep: []string{"FAKE_LOGIN"}}, nil)
	useRealShell(t)
	t.Setenv("FAKE_LOGIN", "mine")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "别家的")
	t.Setenv("ANTHROPIC_API_KEY", "不放行")
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	if _, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, "问", ""); err != nil {
		t.Fatal(err)
	}
	r.e.waitShells()
	got := "\n" + string(testkit.ReadBytes(t, log))
	if !strings.Contains(got, "\nFAKE_LOGIN=mine") {
		t.Errorf("自己声明的要传：%s", got)
	}
	for _, bad := range []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=", "AGENTQ_SHELL_ENV_KEEP="} {
		if strings.Contains(got, "\n"+bad) {
			t.Errorf("不该传 %s：%s", bad, got)
		}
	}
}

// 假后端的完整一趟：起进程、等结束、按结果收尾、记用量、统计认模型。
func TestFakeBackendEndToEnd(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"session","id":"s9"}'; echo '{"type":"usage","input":10,"output":5,"cost":0.5}'; echo '{"type":"result","ok":true,"text":"答完了"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, nil)
	r.a.Cfg.Backends = map[string]config.BackendConfig{r.name: {Model: "m1"}}
	useRealShell(t)
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindChat, Project: "demo", ProjectDir: dir, Title: "对话"}, "问", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.PID <= 0 || !strings.HasPrefix(strings.Join(j.Args, " "), cli+" --label agentq demo") {
		t.Fatalf("%d %v", j.PID, j.Args)
	}
	r.e.waitShells()
	if n := r.a.Runner.Reap(r.st); n != 1 {
		t.Fatalf("收尾 %d 个", n)
	}
	if j.Status != state.StatusDone || j.SessionID != "s9" || j.Usage == nil || j.Usage.CostUSD != 0.5 {
		t.Errorf("%s %q %+v", j.Status, j.SessionID, j.Usage)
	}
	if r.a.BackendFor(r.name).RecordedModel(j.Args) != "m1" { // 只传给这个后端配的；Claude 的默认 opus 不串，见 TestCodexProjectBackendGetsNoClaudeDefaults
		t.Errorf("%v", j.Args)
	}
	if s, code := r.e.run("logs", j.ID); code != 0 {
		t.Log(s)
	}
}

// doctor：当前后端、版本、能力表、降级说明；CLI 没装或没登录如实说。Claude 没配 backend 时全有。
func TestDescribeBackend(t *testing.T) {
	all := testkit.RegisterFake(t, testkit.FakeOpts{Path: "/x/fake", Caps: testkit.AllCaps(), Auth: backend.Auth{State: backend.AuthIn}, ToolLog: true})
	s := backend.Describe(backend.Get(all, backend.Options{}), backend.Probe{Path: "/x/fake", Version: "1.0", Authenticated: true, AuthMethod: "key"}, nil)
	for _, want := range []string{"后端：" + all + " 1.0（/x/fake）", "认证：已登录（key）", "续跑：有", "美元上限：有", "额度查询：无", "关掉的任务：无"} {
		if !strings.Contains(s, want) {
			t.Errorf("缺 %q：\n%s", want, s)
		}
	}
	c := testkit.AllCaps()
	c.HardBudget, c.ReviewProfile, c.ToolProfiles, c.Resume, c.AuthProbe = false, false, false, false, false
	weak := testkit.RegisterFake(t, testkit.FakeOpts{Path: "/x/weak", Caps: c})
	s = backend.Describe(backend.Get(weak, backend.Options{}), backend.Probe{Path: "/x/weak", Version: "2.0"}, nil)
	for _, want := range []string{"美元上限：无，job_budget_usd 不生效", "续跑：无", "引擎审查：无", "认证：查不了", "待做和 new", "Router", "主动感知", "巡逻分析"} {
		if !strings.Contains(s, want) {
			t.Errorf("缺 %q：\n%s", want, s)
		}
	}
	if len(backend.Limits(backend.Get(weak, backend.Options{}))) == 0 {
		t.Error("Limits 应该列出缺的能力")
	}
	s = backend.Describe(backend.Get(weak, backend.Options{}), backend.Probe{}, errors.New("没装"))
	if !strings.Contains(s, "CLI 没找到") || !strings.Contains(s, "没装") {
		t.Error(s)
	}
	s = backend.Describe(backend.Get(weak, backend.Options{}), backend.Probe{Path: "/x/weak"}, errors.New("没登录"))
	if !strings.Contains(s, "没登录或查不到") {
		t.Error(s)
	}
	s = backend.Describe(backend.Get("nosuch", backend.Options{}), backend.Probe{}, errors.New("未知后端"))
	if !strings.Contains(s, "未知") {
		t.Error(s)
	}
	claudeRow := backend.Describe(backend.Get("", backend.Options{}), backend.Probe{Path: "/c", Version: "2", Authenticated: true}, nil)
	if !strings.Contains(claudeRow, "关掉的任务：无") || strings.Contains(claudeRow, "：无，") {
		t.Errorf("Claude 应该全有：%s", claudeRow)
	}
}

// doctor 真跑一趟：未知后端、Claude 默认。
func TestDoctorReportsBackend(t *testing.T) {
	i, o := installFixture(t)
	if e := i.Setup(o); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	i.Out = &out
	if e := i.Doctor(); e != nil {
		t.Fatalf("%v\n%s", e, out.String())
	}
	if !strings.Contains(out.String(), "后端：claude") || !strings.Contains(out.String(), "能力：") || !strings.Contains(out.String(), "关掉的任务：无") {
		t.Errorf("%s", out.String())
	}
	setBackend := func(name string) {
		t.Helper()
		cfg, _, err := setup.ReadInstallConfig(i.Home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Backend = name
		if err := config.Save(filepath.Join(i.Home, ".agentq", "config.json"), cfg); err != nil {
			t.Fatal(err)
		}
		out.Reset()
	}
	setBackend("nosuch")
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "未知后端") || !strings.Contains(out.String(), "未知") {
		t.Errorf("未知后端要报错：%v\n%s", e, out.String())
	}
	setBackend(testkit.RegisterFake(t, testkit.FakeOpts{Caps: testkit.AllCaps()}))
	if e := i.Doctor(); e == nil || !strings.Contains(out.String(), "CLI 没找到") {
		t.Errorf("没装要如实说：%v\n%s", e, out.String())
	}
	setBackend(testkit.RegisterFake(t, testkit.FakeOpts{Caps: testkit.AllCaps(), Path: "/x/fake", Auth: backend.Auth{State: backend.AuthOut, Detail: "假后端没登录"}}))
	if e := i.Doctor(); e == nil || !strings.Contains(out.String(), "没登录或查不到") || !strings.Contains(out.String(), "假后端没登录") {
		t.Errorf("没登录要如实说：%v\n%s", e, out.String())
	}

	// project_backends 的每个值都要查：默认后端好好的，项目的后端写错了 doctor 也要报
	setProjects := func(m map[string]string) {
		t.Helper()
		cfg, _, err := setup.ReadInstallConfig(i.Home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Backend, cfg.ProjectBackends = "", m
		if err := config.Save(filepath.Join(i.Home, ".agentq", "config.json"), cfg); err != nil {
			t.Fatal(err)
		}
		out.Reset()
	}
	setProjects(map[string]string{"demo": "nosuch"})
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "nosuch") || !strings.Contains(out.String(), "项目 demo") {
		t.Errorf("项目的后端写错要报：%v\n%s", e, out.String())
	}
	noCLI := testkit.RegisterFake(t, testkit.FakeOpts{Caps: testkit.AllCaps()})
	setProjects(map[string]string{"demo": noCLI})
	if e := i.Doctor(); e == nil || !strings.Contains(out.String(), noCLI+"，CLI 没找到") {
		t.Errorf("项目的后端没装要报：%v\n%s", e, out.String())
	}
	c := testkit.AllCaps()
	c.ReviewProfile = false
	weak := testkit.RegisterFake(t, testkit.FakeOpts{Caps: c, Path: "/x/weak", Auth: backend.Auth{State: backend.AuthIn}})
	setProjects(map[string]string{"demo": weak})
	if e := i.Doctor(); e != nil || !strings.Contains(out.String(), "项目 demo 的后端 "+weak) || !strings.Contains(out.String(), "待做和 new 不接活") {
		t.Errorf("项目的后端关掉了待做要说明：%v\n%s", e, out.String())
	}
}

// 注册表：加一个后端只要一行注册；未知的名字有实例但什么都做不了。
func TestBackendRegistry(t *testing.T) {
	name := testkit.RegisterFake(t, testkit.FakeOpts{Caps: testkit.AllCaps()})
	if !backend.Known(name) || backend.Get(name, backend.Options{}).Name() != name {
		t.Error(name)
	}
	if !backend.Known("") || backend.Get("", backend.Options{}).Name() != backend.Default {
		t.Error("空名字算默认后端")
	}
	m := backend.Get("nosuch", backend.Options{})
	if backend.Known("nosuch") || m.Name() != "nosuch" || m.CanResume("x") || m.Parse(backend.JobRef{}, backend.Exit{}).HaveTerminal {
		t.Errorf("%+v", m)
	}
	if _, _, err := m.Build(backend.Request{}); err == nil || !strings.Contains(err.Error(), "未知后端") {
		t.Error(err)
	}
}

// Router 打开、后端做不到它的硬要求：消息走对话任务，不起 Router。
func TestDegradeRouterFallsBackToChat(t *testing.T) {
	cli := fakeCLI(t, `echo '{"type":"result","ok":true,"text":"答"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli}, func(c *backend.Caps) { c.DenyRules = false })
	r.a.Cfg.RouterEnabled = true
	useRealShell(t)
	s := &notify.State{Inbox: []notify.InMsg{{ID: 1, Text: "帮我看看"}}}
	r.a.Chat.ServeInbox(s, r.st, nil)
	r.e.waitShells()
	if len(r.st.Jobs) != 1 || r.st.Jobs[0].Kind != state.KindChat {
		t.Fatalf("应该起对话任务：%+v；日志 %s", r.st.Jobs, r.out.String())
	}
}

// 追问：后端做不到只读就回退成普通对话任务；能做到才只读续跑。
func TestDegradeFollowupFallsBack(t *testing.T) {
	route := func(mut func(*backend.Caps)) int {
		r := fakeApp(t, testkit.FakeOpts{}, mut)
		r.st.Jobs = append(r.st.Jobs, &Job{ID: "orig", Backend: r.name, SessionID: "s1", Status: state.StatusDone})
		s := &notify.State{Threads: map[int64]notify.Thread{7: {Kind: notify.ThreadJob, JobID: "orig", SessionID: "s1", Project: "demo"}}}
		m := &notify.InMsg{ID: 8, ReplyToID: 7, Text: "为什么"}
		got, _, _ := r.a.Chat.RouteReply(s, r.st, m)
		return got
	}
	if got := route(nil); got != chat.RouteFollowup {
		t.Errorf("能只读应该追问：%d", got)
	}
	if got := route(func(c *backend.Caps) { c.ToolProfiles = false }); got != chat.RoutePlain {
		t.Errorf("不能只读应该回退：%d", got)
	}
	if got := route(func(c *backend.Caps) { c.Resume = false }); got != chat.RoutePlain {
		t.Errorf("不能续跑应该回退：%d", got)
	}
}

// 手动跑闲时找活也如实拒绝，不占今天。
func TestDegradeSuggestRefused(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{Path: "/bin/true"}, func(c *backend.Caps) { c.ToolProfiles = false })
	dir := testkit.MkProject(t, r.e.root, "alpha", "")
	testkit.GitT(t, r.e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, r.e.home, dir, "add", "-A")
	testkit.GitT(t, r.e.home, dir, "commit", "-q", "-m", "init")
	err := r.a.Suggest.CmdSuggest("")
	if err == nil || !strings.Contains(err.Error(), "不能强制只读") {
		t.Errorf("%v；日志 %s", err, r.out.String())
	}
	if s, _ := r.a.Suggest.LoadSuggest(); s != nil && s.LastDay != "" {
		t.Errorf("没跑不该占今天：%+v", s)
	}
}

// project_backends 写错，这个项目永远不启动：status 不能写「正常」。
func TestStatusReportsProjectBackend(t *testing.T) {
	e := newEnv(t, map[string]any{"project_backends": map[string]string{"demo": "nosuch"}, "paused": false})
	testkit.MkProject(t, e.root, "demo", "- [ ] 修登录\n")
	if out, code := e.run("tick"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if st := e.state(); st != nil && len(st.Jobs) != 0 {
		t.Errorf("不该接活：%+v", st.Jobs)
	}
	out, code := e.run("status")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if strings.Contains(out, "调度：正常") || !strings.Contains(out, "项目 demo 的后端 nosuch 未知") {
		t.Errorf("status 要说明被拒的项目：%s", out)
	}
	// 暂停时也要说，不能被盖住
	e.mustRun("pause")
	out, _ = e.run("status")
	if !strings.Contains(out, "已暂停") || !strings.Contains(out, "项目 demo 的后端 nosuch 未知") {
		t.Errorf("暂停也要说：%s", out)
	}
}

// 默认后端关掉了待做和 new：status 说清楚，不写「正常」。
func TestStatusReportsDefaultBackendOff(t *testing.T) {
	r := fakeApp(t, testkit.FakeOpts{}, func(c *backend.Caps) { c.ReviewProfile = false })
	if err := r.a.CmdStatus(); err != nil {
		t.Fatal(err)
	}
	if out := r.out.String(); strings.Contains(out, "调度：正常") || !strings.Contains(out, "待做和 new 不接活") || !strings.Contains(out, "默认后端 "+r.name) {
		t.Errorf("%s", out)
	}
	ok := fakeApp(t, testkit.FakeOpts{}, nil)
	if err := ok.a.CmdStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ok.out.String(), "调度：正常") {
		t.Errorf("什么都不缺还是正常：%s", ok.out.String())
	}
}

// 新后端的 Build 忘了查硬要求：感知这类外部内容任务也不能静默跑在没有限制的后端上。
func TestStartChecksMustEvenIfBuildForgets(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ran")
	cli := fakeCLI(t, `touch `+log+`; echo '{"type":"result","ok":true,"text":"答"}'`)
	r := fakeApp(t, testkit.FakeOpts{Path: cli, SkipCheckMust: true}, func(c *backend.Caps) { c.ToolProfiles = false })
	useRealShell(t)
	dir := testkit.MkProject(t, r.e.root, "demo", "")
	if _, _, err := r.a.BackendFor(r.name).Build(backend.Request{Profile: backend.ProfileNoTools, Must: backend.MustSense}); err != nil {
		t.Fatalf("这个假后端的 Build 不该自己拒绝：%v", err)
	}
	j, err := r.a.Runner.Start(r.st, &Job{Kind: state.KindSense, Project: "demo", ProjectDir: dir, Title: "主动感知"}, "外部原文", "")
	r.e.waitShells()
	if j != nil || !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("Start 应该拒绝：%+v %v", j, err)
	}
	if _, serr := os.Stat(log); serr == nil {
		t.Error("CLI 不该被启动")
	}
	if len(r.st.Jobs) != 0 {
		t.Errorf("不该留任务记录：%+v", r.st.Jobs)
	}
}
