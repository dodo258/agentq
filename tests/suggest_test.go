package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
)

// ---- 纯函数 ----

// 闲时找活失败不该计入连环失败，更不该熔断：它是可有可无的后台活。
func TestSuggestNeverTripsBreaker(t *testing.T) {
	cfg := config.Default()
	st := &State{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		j := &Job{Kind: state.KindSuggest, Status: state.StatusFailed}
		if tripped, _ := runner.RecordOutcome(st, cfg, j, time.Second, now); tripped {
			t.Fatal("闲时找活秒退不该熔断")
		}
	}
	if st.FailStreak != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("不该计数：%+v", st)
	}
	// 对照：同样的秒退，待做任务照旧计数
	for i := 0; i < cfg.BreakerStreak; i++ {
		runner.RecordOutcome(st, cfg, &Job{Status: state.StatusFailed}, time.Second, now)
	}
	if st.BreakerUntil.IsZero() {
		t.Error("待做任务连续秒退应熔断（对照）")
	}
}

// ---- 带真 git 仓库的单元测试 ----

func sgApp(t *testing.T) (*App, string) {
	t.Helper()
	a, root := newRootApp(t)
	os.WriteFile(filepath.Join(a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	return a, root
}

func sgRepo(t *testing.T, a *App, root, name, backlog string) bl.Project {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "add", "-A")
	testkit.GitT(t, a.Home, dir, "commit", "-q", "-m", "init")
	return bl.Project{Name: name, Dir: dir}
}

const sgBacklog = "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 已完成\n\n- [x] 做过的事 —— 合并提交 abc1234\n"

func sgItem(project, title, kind string) suggest.SuggestItem {
	return suggest.SuggestItem{Project: project, Title: title, Why: "要做的事", Benefit: "好处", Risk: "风险", Kind: kind}
}

func commits(t *testing.T, a *App, p bl.Project) int {
	t.Helper()
	var n int
	fmt.Sscan(testkit.GitT(t, a.Home, p.Dir, "rev-list", "--count", "HEAD"), &n)
	return n
}

func backlogOf(t *testing.T, p bl.Project) string {
	t.Helper()
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// ---- 集成：真二进制、假 claude、假 Telegram ----

const sgUsageOK = "You are currently using your subscription to power your Claude Code usage\n\n" +
	"Current session: 10% used\nCurrent week (all models): 20% used\nCurrent week (Fable): 100% used\n"

func sgEnv(t *testing.T, cfg map[string]any) (*testEnv, *fakeTG) {
	t.Helper()
	full := map[string]any{"suggest_enabled": true, "paused": false}
	for k, v := range cfg {
		full[k] = v
	}
	e, tg := threadEnv(t, full)
	e.atNoon()
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	e.setUsage(sgUsageOK)
	return e, tg
}

func (e *testEnv) sgProject(name, backlog string) bl.Project {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	testkit.GitT(e.t, e.home, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644)
	testkit.GitT(e.t, e.home, dir, "add", "-A")
	testkit.GitT(e.t, e.home, dir, "commit", "-q", "-m", "init")
	return bl.Project{Name: name, Dir: dir}
}

func (e *testEnv) suggestOut(items ...suggest.SuggestItem) {
	e.t.Helper()
	e.suggestRaw(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "session_id": "s-sg",
		"structured_output": map[string]any{"items": append([]suggest.SuggestItem{}, items...)}})
}

func (e *testEnv) suggestRaw(v any) {
	e.t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(e.home, "fake-suggest.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) suggestJobs() []*Job {
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Kind == state.KindSuggest {
			out = append(out, j)
		}
	}
	return out
}

func (e *testEnv) claudeRuns() int {
	files, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	return len(files)
}

func (e *testEnv) store() *suggest.SuggestStore {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "suggestions.json"))
	if err != nil {
		return &suggest.SuggestStore{}
	}
	var s suggest.SuggestStore
	if err := json.Unmarshal(b, &s); err != nil {
		e.t.Fatal(err)
	}
	return &s
}

// runSuggest：tick 启动找活任务（必须启动），再 tick 到它收尾。
func (e *testEnv) runSuggest() *Job {
	e.t.Helper()
	before := len(e.suggestJobs())
	e.mustRun("tick")
	if len(e.suggestJobs()) != before+1 {
		e.t.Fatalf("tick 应启动一个闲时找活任务：%d → %d", before, len(e.suggestJobs()))
	}
	return e.endSuggest()
}

func (e *testEnv) endSuggest() *Job {
	e.t.Helper()
	return e.tickUntilEnded(func() *Job {
		js := e.suggestJobs()
		return js[len(js)-1]
	})
}

func (e *testEnv) tomorrow() {
	e.t.Helper()
	y, m, d := time.Now().AddDate(0, 0, 1).Date()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, d, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
}

// 关着（默认）：无论什么情况都不起任何进程、不读额度、不建文件，status 也不多一行。
func TestSuggestDisabledStartsNothing(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, map[string]any{"suggest_enabled": false})
	e.sgProject("alpha", sgBacklog)
	e.suggestOut(sgItem("alpha", "不该有", "test"))
	for i := 0; i < 3; i++ {
		e.mustRun("tick")
	}
	if e.claudeRuns() != 0 {
		t.Errorf("关着不起进程：%d", e.claudeRuns())
	}
	for _, f := range []string{"fake-usage-calls", ".agentq/suggestions.json", ".agentq/suggestions.lock"} {
		if _, err := os.Stat(filepath.Join(e.home, f)); err == nil {
			t.Errorf("关着不该产生 %s", f)
		}
	}
	if len(e.suggestJobs()) != 0 {
		t.Error("关着不该有找活任务")
	}
	if out := e.mustRun("status"); strings.Contains(out, "闲时找活") {
		t.Errorf("关着 status 不多一行：\n%s", out)
	}
}

// 触发条件逐个不满足：一个 claude 进程都不起（读额度的 /usage 是本地命令，不算模型进程，记在 fake-usage-calls）。
func TestSuggestGates(t *testing.T) {
	t.Parallel()
	type gate struct {
		name  string
		cfg   map[string]any
		setup func(e *testEnv)
		usage string // 非空覆盖假 /usage 输出
		reads bool   // 应该读过额度
		tasks int    // 正常待做任务启动的 claude 个数（不是找活）
	}
	week := func(pct string) string {
		return "Current session: 10% used\nCurrent week (all models): " + pct + "% used\n"
	}
	gates := []gate{
		{name: "有待做", tasks: 1, reads: true, setup: func(e *testEnv) {
			e.sgProject("busy", "# 待办\n\n## 待做\n\n- [ ] 有活 —— SLEEP=0\n\n## 已完成\n")
		}},
		{name: "待做里只有卡住的也不算闲", setup: func(e *testEnv) {
			e.sgProject("busy", "# 待办\n\n## 待做\n\n- [ ] 有活 —— x（卡住：没过）\n\n## 已完成\n")
		}},
		{name: "暂停", cfg: map[string]any{"paused": true}},
		{name: "会话额度 60%", usage: "Current session: 60% used\nCurrent week (all models): 10% used\n", reads: true},
		{name: "本周额度 60%", usage: week("60"), reads: true},
		{name: "额度读不出", usage: "You are currently using your subscription", reads: true},
		{name: "项目 BACKLOG 读不出", setup: func(e *testEnv) {
			p := e.sgProject("odd", sgBacklog)
			os.Remove(p.BacklogPath())
			os.MkdirAll(p.BacklogPath(), 0o755)
		}},
	}
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			e, _ := sgEnv(t, g.cfg)
			e.sgProject("alpha", sgBacklog)
			if g.setup != nil {
				g.setup(e)
			}
			if g.usage != "" {
				e.setUsage(g.usage)
			}
			e.suggestOut(sgItem("alpha", "不该有", "test"))
			e.mustRun("tick")
			e.mustRun("tick")
			if e.claudeRuns() != g.tasks || len(e.suggestJobs()) != 0 {
				t.Errorf("不该起找活的进程：runs=%d（待做任务 %d）jobs=%d", e.claudeRuns(), g.tasks, len(e.suggestJobs()))
			}
			if _, err := os.Stat(filepath.Join(e.home, ".agentq", "suggestions.json")); err == nil {
				t.Error("没跑就不该记“今天跑过”")
			}
			_, err := os.Stat(filepath.Join(e.home, "fake-usage-calls"))
			if g.reads != (err == nil) {
				t.Errorf("读额度：期望 %v，实际 %v", g.reads, err == nil)
			}
			if b, _ := os.ReadFile(filepath.Join(e.root, "alpha", "docs", "BACKLOG.md")); string(b) != sgBacklog {
				t.Errorf("BACKLOG 不该动：%s", b)
			}
		})
	}
}

// 全流程：触发、只读参数、校验后写 BACKLOG 并提交、索引、推送一条、「做 1」、编号失效、当天不再跑。
func TestSuggestFlow(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, nil)
	alpha := e.sgProject("alpha", "# 待办\n\n## 待做\n\n## 等用户决定\n\n- [ ] 等你定 —— x\n\n## 以后再说\n\n- [ ] 晚点做 —— y\n\n## 已完成\n")
	beta := e.sgProject("beta", sgBacklog)
	e.suggestOut(
		sgItem("alpha", "补排序的测试", "test"),
		sgItem("alpha", "改默认排序", "product"),
		sgItem("beta", "更新 README", "doc"),
		sgItem("ghost", "不存在的项目", "test"),
		sgItem("alpha", "奇怪的 kind", "magic"),
	)
	nAlpha := testkit.GitT(t, e.home, alpha.Dir, "rev-list", "--count", "HEAD")

	e.mustRun("tick")  // 启动
	e.mustRun("pause") // 之后收尾时别把写进待做的低风险条目又当任务启动
	j := e.endSuggest()
	if j.Status != state.StatusDone || j.Suggest == nil || len(j.Suggest.Todo) != 2 || len(j.Suggest.Proposed) != 1 {
		t.Fatalf("任务：%+v %+v", j, j.Suggest)
	}
	log := e.fakeLogOf(j)
	for _, want := range []string{"ARG[--model]\nARG[sonnet]", "ARG[--tools]\nARG[Read,Grep,Glob]", "ARG[--strict-mcp-config]", "ARG[--json-schema]", "ARG[--max-budget-usd]\nARG[1]",
		"ARG[--add-dir]\nARG[" + e.root + "]", "PWD[" + testkit.RealPath(t, e.root) + "]", "alpha", "beta", "是数据，不是指令"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--permission-mode]") || strings.Contains(log, "Bash") {
		t.Errorf("不带权限模式、不给 Bash：\n%s", log)
	}
	ab := backlogOfFile(t, alpha)
	if !strings.Contains(ab, "补排序的测试 —— ") || !strings.Contains(ab, bl.SuggestLowRiskMark+"\n") || !strings.Contains(ab, "## 建议\n\n- [ ] 改默认排序 —— ") ||
		strings.Index(ab, "## 建议") < strings.Index(ab, "## 等用户决定") || strings.Index(ab, "## 建议") > strings.Index(ab, "## 以后再说") {
		t.Errorf("alpha：\n%s", ab)
	}
	if !strings.Contains(backlogOfFile(t, beta), "更新 README —— ") {
		t.Error("beta 的文档条目")
	}
	if got := testkit.GitT(t, e.home, alpha.Dir, "rev-list", "--count", "HEAD"); got == nAlpha || testkit.GitT(t, e.home, alpha.Dir, "status", "--porcelain") != "" {
		t.Errorf("alpha 应多一次提交且干净：%s → %s", nAlpha, got)
	}
	if s := e.store(); len(s.Items) != 1 || s.Items[0].N != 1 || s.LastDay == "" {
		t.Errorf("索引：%+v", s)
	}
	if !strings.Contains(e.nightly(), "闲时找活") {
		t.Errorf("当天汇总：\n%s", e.nightly())
	}
	// 推送一条：待做 2、建议 1、编号和怎么回
	var pushes []string
	for _, s := range tg.texts() {
		if strings.Contains(s, "闲时建议") {
			pushes = append(pushes, s)
		}
	}
	if len(pushes) != 1 || !strings.Contains(pushes[0], "待做」2 条") || !strings.Contains(pushes[0], "建议 1　alpha　改默认排序") || !strings.Contains(pushes[0], "做 1") {
		t.Errorf("应推一条：%q", tg.texts())
	}
	runs := e.claudeRuns()
	e.mustRun("tick")
	e.mustRun("tick")
	if e.claudeRuns() != runs || len(e.suggestJobs()) != 1 {
		t.Errorf("当天不再跑：runs %d → %d", runs, e.claudeRuns())
	}
	nPush := 0
	for _, s := range tg.texts() {
		if strings.Contains(s, "闲时建议") {
			nPush++
		}
	}
	if nPush != 1 {
		t.Errorf("不重复推：%d", nPush)
	}

	// 用户回「做 1 7」：1 移进待做，7 失效；代码处理，不起模型
	tg.msg(1, ownerChat, 21, "做 1 7", "")
	e.mustRun("tick")
	reply := tg.sentLast().Get("text")
	if !strings.Contains(reply, "已移进「待做」　1 条") || !strings.Contains(reply, "1　alpha　改默认排序") || !strings.Contains(reply, "7（没有这个编号") {
		t.Errorf("回复：%q", reply)
	}
	ab = backlogOfFile(t, alpha)
	if strings.Contains(ab, "## 建议\n\n- [ ] 改默认排序") || !strings.Contains(ab, "- [ ] 改默认排序 —— ") || strings.Contains(ab, "（建议 1）") {
		t.Errorf("移进待做：\n%s", ab)
	}
	if e.claudeRuns() != runs || len(chatJobs(e)) != 0 {
		t.Errorf("「做 1 7」不起模型：runs %d → %d", runs, e.claudeRuns())
	}
	if len(e.store().Items) != 0 {
		t.Errorf("索引：%+v", e.store().Items)
	}
}

func backlogOfFile(t *testing.T, p bl.Project) string { return backlogOf(t, p) }

// 无变化不生成（含自己的提交）；项目有变化或「以后再说」变了才再生成。
func TestSuggestOnlyWhenProjectsChanged(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	e.sgProject("alpha", sgBacklog+"\n## 以后再说\n\n- [ ] 晚点做\n")
	beta := e.sgProject("beta", sgBacklog)
	e.suggestOut(sgItem("alpha", "改默认排序", "product"), sgItem("beta", "改默认配色", "product"))
	j := e.runSuggest()
	if j.Status != state.StatusDone || len(j.Suggest.Proposed) != 2 {
		t.Fatalf("第一天：%+v", j.Suggest)
	}
	e.tomorrow()
	runs := e.claudeRuns()
	e.mustRun("tick")
	e.mustRun("tick")
	if e.claudeRuns() != runs || len(e.suggestJobs()) != 1 {
		t.Fatalf("第二天没有变化（自己的提交不算）不该再生成：runs %d → %d", runs, e.claudeRuns())
	}
	// beta 有新提交：再生成
	testkit.GitT(t, e.home, beta.Dir, "commit", "-q", "--allow-empty", "-m", "新提交")
	e.suggestOut()
	j2 := e.runSuggest()
	if j2.Status != state.StatusDone || len(e.suggestJobs()) != 2 {
		t.Fatalf("beta 变了应再生成：%+v", j2)
	}
	// 第三天：只改 alpha 的「以后再说」（工作区改动，HEAD 没变）也算变化
	y, m, d := time.Now().AddDate(0, 0, 2).Date()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, d, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
	runs = e.claudeRuns()
	e.mustRun("tick")
	if e.claudeRuns() != runs {
		t.Fatal("第三天没有变化不该生成")
	}
	p := filepath.Join(e.root, "alpha", "docs", "BACKLOG.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "晚点做", "晚点做，现在值得做了", 1)), 0o644)
	e.runSuggest()
	if len(e.suggestJobs()) != 3 {
		t.Errorf("以后再说变了应再生成：%d", len(e.suggestJobs()))
	}
}

// 每天最多一次：当天项目又有了新提交也不再跑，第二天才跑。
func TestSuggestOncePerDay(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	alpha := e.sgProject("alpha", sgBacklog)
	e.suggestOut(sgItem("alpha", "改默认排序", "product"))
	e.runSuggest()
	testkit.GitT(t, e.home, alpha.Dir, "commit", "-q", "--allow-empty", "-m", "当天的新提交")
	runs := e.claudeRuns()
	e.mustRun("tick")
	e.mustRun("tick")
	if e.claudeRuns() != runs || len(e.suggestJobs()) != 1 {
		t.Fatalf("当天不再跑：runs %d → %d", runs, e.claudeRuns())
	}
	e.tomorrow()
	e.suggestOut()
	e.runSuggest()
	if len(e.suggestJobs()) != 2 {
		t.Errorf("第二天项目有变化应再跑：%d", len(e.suggestJobs()))
	}
}

// 「建议」节里的条目绝不会被 tick 挑中：关着开关、待做为空、建议里有一堆条目，tick 一个任务都不起。
func TestSuggestionSectionNeverPicked(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, map[string]any{"suggest_enabled": false})
	e.sgProject("alpha", "# 待办\n\n## 待做\n\n## 建议\n\n- [ ] 改默认排序 —— 要做（建议 1）\n- [ ] 另一条 —— 要做（建议 2）\n\n## 已完成\n")
	for i := 0; i < 2; i++ {
		out := e.mustRun("tick")
		if !strings.Contains(out, "启动 0") {
			t.Errorf("建议里的条目不该被启动：\n%s", out)
		}
	}
	if e.claudeRuns() != 0 || len(e.state().Jobs) != 0 {
		t.Errorf("runs=%d jobs=%d", e.claudeRuns(), len(e.state().Jobs))
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "没有可以做的条目") {
		t.Errorf("agentq run 也不挑建议里的条目：%d %s", code, out)
	}
}

// 输出缺失、畸形、撞预算：零写入，任务记失败/撞上限但不熔断，当天不重试。
func TestSuggestBadOutputWritesNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]any{
		"没有结构化输出": map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": `{"items":[]}`, "session_id": "s"},
		"畸形":      map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "session_id": "s", "structured_output": map[string]any{"items": "不是数组"}},
		"撞预算":     map[string]any{"type": "result", "subtype": "error_max_budget_usd", "is_error": true, "result": "", "session_id": "s"},
		"报错":      map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "出错了", "session_id": "s"},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, tg := sgEnv(t, nil)
			p := e.sgProject("alpha", sgBacklog)
			e.suggestRaw(out)
			n0 := testkit.GitT(t, e.home, p.Dir, "rev-list", "--count", "HEAD")
			j := e.runSuggest()
			if j.Status == state.StatusDone || j.Suggest != nil {
				t.Errorf("应记失败：%+v", j)
			}
			if b, _ := os.ReadFile(p.BacklogPath()); string(b) != sgBacklog || testkit.GitT(t, e.home, p.Dir, "rev-list", "--count", "HEAD") != n0 {
				t.Errorf("零写入：%s", b)
			}
			if len(e.store().Items) != 0 {
				t.Error("索引不该有条目")
			}
			st := e.state()
			if st.FailStreak != 0 || !st.BreakerUntil.IsZero() {
				t.Errorf("不该熔断：%+v", st)
			}
			for _, s := range tg.texts() {
				if strings.Contains(s, "闲时建议") {
					t.Errorf("自动跑没成不推送建议：%q", s)
				}
			}
			runs := e.claudeRuns()
			e.mustRun("tick")
			if e.claudeRuns() != runs {
				t.Error("当天不重试")
			}
			// 失败的那次不算看过：快照没更新，项目没变化第二天也能再试
			if snap := e.store().Snapshot; len(snap) != 0 {
				t.Errorf("失败不更新快照：%+v", snap)
			}
			e.tomorrow()
			e.suggestOut(sgItem("alpha", "第二天的新活", "product"))
			j2 := e.runSuggest()
			if j2.Status != state.StatusDone || len(j2.Suggest.Proposed) != 1 {
				t.Errorf("第二天应能再试：%+v", j2.Suggest)
			}
		})
	}
}

// 主目录脏的项目跳过，别的项目照写。
func TestSuggestSkipsDirtyProject(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	dirty := e.sgProject("alpha", sgBacklog)
	e.sgProject("beta", sgBacklog)
	os.WriteFile(dirty.BacklogPath(), []byte(sgBacklog+"\n用户正在改\n"), 0o644)
	// 脏了 HEAD 没变，但这是第一次生成，照样触发
	e.suggestOut(sgItem("alpha", "甲", "test"), sgItem("beta", "乙", "test"))
	j := e.runSuggest()
	if j.Status != state.StatusDone || len(j.Suggest.Todo) != 1 || len(j.Suggest.Failed) != 1 {
		t.Fatalf("%+v", j.Suggest)
	}
	if b, _ := os.ReadFile(dirty.BacklogPath()); string(b) != sgBacklog+"\n用户正在改\n" {
		t.Errorf("脏的不能动：%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "beta", "docs", "BACKLOG.md")); !strings.Contains(string(b), "乙 —— ") {
		t.Errorf("beta：%s", b)
	}
}

// 手动：不看开关、暂停、待做是否为空和额度 <60%，占用当天；指定项目只看这一个；摘要推送，没找到也推一句。
func TestSuggestManual(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, map[string]any{"suggest_enabled": false, "paused": true})
	e.setUsage("Current session: 80% used\nCurrent week (all models): 70% used\n")
	alpha := e.sgProject("alpha", "# 待办\n\n## 待做\n\n- [ ] 有活 —— SLEEP=0\n\n## 已完成\n")
	beta := e.sgProject("beta", sgBacklog)
	if out, code := e.run("suggest", "ghost"); code == 0 || !strings.Contains(out, "没有 git 项目") {
		t.Errorf("没有这个项目：%d %s", code, out)
	}
	if out, code := e.run("suggest", "a", "b"); code != 2 {
		t.Errorf("参数个数：%d %s", code, out)
	}
	e.suggestOut(sgItem("beta", "手动一条", "product"), sgItem("alpha", "越界的一条", "product"))
	out := e.mustRun("suggest", "beta")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "1 个项目") {
		t.Errorf("输出：%s", out)
	}
	j := e.endSuggest()
	log := e.fakeLogOf(j)
	if !j.Manual || !strings.Contains(log, "PWD["+testkit.RealPath(t, beta.Dir)+"]") || !strings.Contains(log, "ARG[--add-dir]\nARG["+beta.Dir+"]") {
		t.Errorf("指定项目时工作目录和可读范围收窄到它：%+v\n%s", j, log)
	}
	if j.Status != state.StatusDone || len(j.Suggest.Proposed) != 1 || len(j.Suggest.Skipped) != 1 {
		t.Errorf("越界的项目不写：%+v", j.Suggest)
	}
	if b, _ := os.ReadFile(alpha.BacklogPath()); strings.Contains(string(b), "越界") {
		t.Error("没指定的项目不能写")
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "闲时找活") {
		t.Errorf("应推送：%q", tg.texts())
	}
	// 占用当天：打开开关、取消暂停、额度正常、项目有变化，当天自动那次也不跑
	cfgPath := filepath.Join(e.home, ".agentq", "config.json")
	var cfg map[string]any
	json.Unmarshal(must(os.ReadFile(cfgPath)), &cfg)
	cfg["suggest_enabled"], cfg["paused"] = true, false
	os.WriteFile(cfgPath, must(json.Marshal(cfg)), 0o644)
	e.setUsage(sgUsageOK)
	os.WriteFile(alpha.BacklogPath(), []byte(sgBacklog), 0o644)
	testkit.GitT(t, e.home, alpha.Dir, "commit", "-qam", "清空待做")
	e.mustRun("tick")
	if len(e.suggestJobs()) != 1 {
		t.Error("手动跑过之后，当天自动那次不再跑")
	}
	// 手动再跑一次：不看当天次数；没找到也推一句。假时钟往后拨过合并窗口：每个进程都从中午算起，
	// 不拨的话这条能不能发出去取决于这个进程的时钟有没有赶上上一条的发送时间。
	y, m, d := time.Now().Date()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, d, 12, 0, 0, 0, time.Local).Add(notify.NotifyMergeWindow+time.Minute).Format(time.RFC3339))
	e.suggestOut()
	e.mustRun("suggest")
	e.endSuggest()
	// 同类 30 分钟合并：这一句可能已发出，也可能还在队列里（每个进程的假时钟都从 12:00 起算，先后取决于进程跑多快），两边都算记下了
	queued, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "notify.json"))
	if got := strings.Join(tg.texts(), "\n") + string(queued); !strings.Contains(got, "这次没有值得做的新活") {
		t.Errorf("手动没找到也推一句：%q", got)
	}
	// 在跑时拒绝再启动
	e.suggestOut()
	e.extra = append(e.extra, "AGENTQ_FAKE_SLEEP=4")
	e.mustRun("suggest")
	if out, code := e.run("suggest"); code == 0 || !strings.Contains(out, "已经在跑") {
		t.Errorf("在跑时再启动：%d %s", code, out)
	}
	e.endSuggest()
}

// 额度 ≥98% 手动也拒绝（同 agentq run）；没有建议时「做 1」交给对话任务，不拦截。
func TestSuggestManualRefusedAtHighUsage(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	e.sgProject("alpha", sgBacklog)
	e.setUsage("Current session: 99% used\nCurrent week (all models): 10% used\n")
	if out, code := e.run("suggest"); code == 0 || !strings.Contains(out, "额度") {
		t.Errorf("%d %s", code, out)
	}
	if e.claudeRuns() != 0 {
		t.Error("拒绝时不起进程")
	}
}

func TestDoWithoutSuggestionsGoesToChat(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, map[string]any{"suggest_enabled": false})
	e.sgProject("alpha", sgBacklog)
	tg.msg(1, ownerChat, 11, "做 1", "")
	e.mustRun("tick")
	waitFor(t, 20*time.Second, "对话任务启动", func() bool { e.mustRun("tick"); return len(chatJobs(e)) == 1 })
}

// ---- 独立审查后补的用例 ----

// 写入全部失败：如实说收到几条、哪些项目为什么没写进去；写不进的项目不算处理过，快照不更新，第二天还会再生成。
func TestSuggestWriteFailureIsReported(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, nil)
	p := e.sgProject("alpha", sgBacklog)
	os.WriteFile(p.BacklogPath(), []byte(sgBacklog+"\n用户正在编辑\n"), 0o644) // 没提交的改动：commitBacklog 拒绝
	e.suggestOut(sgItem("alpha", "新活", "product"), sgItem("alpha", "另一条", "test"))
	j := e.runSuggest()
	if j.Suggest == nil || j.Suggest.Written() != 0 || len(j.Suggest.Failed) != 1 || !strings.Contains(j.Summary, "收到 2 条候选") || !strings.Contains(j.Summary, "没写进去") {
		t.Fatalf("摘要要如实：%q %+v", j.Summary, j.Suggest)
	}
	var push string
	waitFor(t, 10*time.Second, "推送", func() bool {
		e.mustRun("tick")
		for _, s := range tg.texts() {
			if strings.Contains(s, "闲时找活") {
				push = s
				return true
			}
		}
		return false
	})
	if !strings.Contains(push, "收到 2 条候选，没写进去") || !strings.Contains(push, "alpha") || strings.Contains(push, "没有值得做") {
		t.Errorf("推送要如实：%q", push)
	}
	if snap := e.store().Snapshot; len(snap) != 0 {
		t.Errorf("写不进的项目不更新快照：%+v", snap)
	}
	if e.store().LastDay == "" {
		t.Error("每天一次的限制保留")
	}
	// 用户提交了编辑；项目没有变化，第二天因为快照没更新还会再生成，这次写得进去
	testkit.GitT(t, e.home, p.Dir, "commit", "-qam", "用户的编辑")
	e.tomorrow()
	e.suggestOut(sgItem("alpha", "新活", "product"))
	j2 := e.runSuggest()
	if j2.Suggest == nil || len(j2.Suggest.Proposed) != 1 {
		t.Errorf("第二天再试：%+v", j2.Suggest)
	}
}

// 手动跑也一样：不能说“没有新活”。
func TestSuggestManualWriteFailureIsReported(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, map[string]any{"suggest_enabled": false})
	p := e.sgProject("alpha", sgBacklog)
	os.WriteFile(p.BacklogPath(), []byte(sgBacklog+"\n用户正在编辑\n"), 0o644)
	e.suggestOut(sgItem("alpha", "新活", "product"))
	e.mustRun("suggest")
	e.endSuggest()
	var push string
	waitFor(t, 10*time.Second, "推送", func() bool {
		e.mustRun("tick")
		for _, s := range tg.texts() {
			if strings.Contains(s, "闲时找活") {
				push = s
				return true
			}
		}
		return false
	})
	if !strings.Contains(push, "收到 1 条候选，没写进去") || strings.Contains(push, "没有值得做") {
		t.Errorf("手动也要如实：%q", push)
	}
	if len(e.store().Snapshot) != 0 {
		t.Errorf("快照不更新：%+v", e.store().Snapshot)
	}
}

// 没有一个有效编号就放行给模型；部分有效时有效的做、失效的说明。
func TestDoSuggestStaleNumbersPassThrough(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, map[string]any{"suggest_enabled": false})
	alpha := e.sgProject("alpha", sgBacklog)
	// 索引里只有一条过期的（BACKLOG 里早没了）
	stale := suggest.SuggestStore{Next: 2, Items: []suggest.SuggestEntry{{N: 1, Project: "alpha", Fingerprint: "x", Title: "没了", CreatedAt: time.Now()}}}
	os.WriteFile(filepath.Join(e.home, ".agentq", "suggestions.json"), must(json.Marshal(stale)), 0o600)
	tg.msg(1, ownerChat, 11, "做 1", "")
	e.mustRun("tick")
	waitFor(t, 20*time.Second, "对话任务启动", func() bool { e.mustRun("tick"); return len(chatJobs(e)) == 1 })
	if b, _ := os.ReadFile(alpha.BacklogPath()); string(b) != sgBacklog {
		t.Errorf("不该动 BACKLOG：%s", b)
	}
	if len(e.store().Items) != 0 {
		t.Errorf("过期的编号被清掉：%+v", e.store().Items)
	}
}

// 索引损坏：tick 反复跑只备份一次。
func TestSuggestCorruptIndexBackedUpOnce(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	e.sgProject("alpha", "# 待办\n\n## 待做\n\n- [ ] 甲 —— x（卡住：没过）\n\n## 已完成\n")
	path := filepath.Join(e.home, ".agentq", "suggestions.json")
	os.WriteFile(path, []byte("{bad"), 0o600)
	for i := 0; i < 3; i++ {
		e.mustRun("tick")
		time.Sleep(1100 * time.Millisecond)
	}
	if m, _ := filepath.Glob(path + ".bad-*"); len(m) != 1 {
		t.Errorf("只备份一次：%v", m)
	}
	if n := strings.Count(e.launchdLog(), "格式不对"); n > 1 {
		t.Errorf("日志只记一次：%d", n)
	}
}

// ---- 第二轮复验后补的用例 ----

// 集成：用户删掉建议，第二天项目有变化，模型又提同一个标题：不写；提示词里写明已经提过。
func TestSuggestDeletedNotProposedAgain(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	p := e.sgProject("alpha", sgBacklog)
	e.suggestOut(sgItem("alpha", "改默认排序", "product"))
	e.runSuggest()
	b, _ := os.ReadFile(p.BacklogPath())
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, "改默认排序") {
			keep = append(keep, l)
		}
	}
	os.WriteFile(p.BacklogPath(), []byte(strings.Join(keep, "\n")), 0o644)
	testkit.GitT(t, e.home, p.Dir, "commit", "-qam", "用户删掉不要的建议")
	e.tomorrow()
	e.suggestOut(sgItem("alpha", "改默认排序", "product"))
	j := e.runSuggest()
	if j.Suggest == nil || len(j.Suggest.Proposed) != 0 || len(j.Suggest.Skipped) != 1 {
		t.Fatalf("删掉的不能回来：%+v", j.Suggest)
	}
	if log := e.fakeLogOf(j); !strings.Contains(log, "已经提过") || !strings.Contains(log, "改默认排序") {
		t.Errorf("提示词要写明：\n%s", log)
	}
}

// 永远写不进的项目（没有 BACKLOG、主目录不在 main、没有待做节）：不看它、不算变化、不推，日志只记一次。
func TestSuggestSkipsStructurallyUnwritableProject(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"没有 BACKLOG", "不在 main", "没有待做节"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			e, tg := sgEnv(t, nil)
			switch mode {
			case "没有 BACKLOG":
				dir := filepath.Join(e.root, "alpha")
				os.MkdirAll(dir, 0o755)
				testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
				testkit.GitT(t, e.home, dir, "commit", "-q", "--allow-empty", "-m", "i")
			case "不在 main":
				p := e.sgProject("alpha", sgBacklog)
				testkit.GitT(t, e.home, p.Dir, "checkout", "-q", "-b", "feature")
			case "没有待做节":
				e.sgProject("alpha", "# x\n\n## 已完成\n")
			}
			e.suggestOut(sgItem("alpha", "补测试", "test"))
			var logs string
			for d := 0; d < 3; d++ {
				y, m, dd := time.Now().AddDate(0, 0, d).Date()
				e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, dd, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
				logs += e.mustRun("tick") + e.mustRun("tick")
			}
			if len(e.suggestJobs()) != 0 || e.claudeRuns() != 0 {
				t.Errorf("不该起进程：jobs=%d runs=%d", len(e.suggestJobs()), e.claudeRuns())
			}
			if n := strings.Count(logs, "写不进 BACKLOG"); n != 1 {
				t.Errorf("日志只记一次：%d\n%s", n, logs)
			}
			for _, s := range tg.texts() {
				if strings.Contains(s, "闲时") {
					t.Errorf("不该推送：%q", s)
				}
			}
			if out, code := e.run("suggest", "alpha"); code == 0 || !strings.Contains(out, "没有能写 BACKLOG 的项目") {
				t.Errorf("手动指定它：%d %s", code, out)
			}
		})
	}
}

// 有的项目写不进、有的能写：写不进的不给模型看、不算变化、不算进闲不闲；恢复后重新看。
func TestSuggestExcludesOnlyUnwritable(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, nil)
	alpha := e.sgProject("alpha", sgBacklog)
	testkit.GitT(t, e.home, alpha.Dir, "checkout", "-q", "-b", "feature")
	e.sgProject("beta", sgBacklog)
	e.suggestOut(sgItem("beta", "乙", "product"))
	j := e.runSuggest()
	if len(j.SuggestProjects) != 1 || j.SuggestProjects[0] != "beta" {
		t.Fatalf("只给模型看能写的：%v", j.SuggestProjects)
	}
	if log := e.fakeLogOf(j); strings.Contains(log, "- alpha") {
		t.Errorf("提示词里不该有 alpha：\n%s", log)
	}
	if _, ok := e.store().Snapshot["alpha"]; ok {
		t.Error("排除的项目不记快照")
	}
	// alpha 在 feature 上的新提交不算变化：第二天不跑
	testkit.GitT(t, e.home, alpha.Dir, "commit", "-q", "--allow-empty", "-m", "又一个提交")
	e.tomorrow()
	e.mustRun("tick")
	if len(e.suggestJobs()) != 1 {
		t.Error("排除的项目有提交不算变化")
	}
	// 切回 main：恢复，重新看它（快照里没有它，算有变化）
	testkit.GitT(t, e.home, alpha.Dir, "checkout", "-q", "main")
	out := e.mustRun("tick")
	if !strings.Contains(out, "又能写了") {
		t.Errorf("恢复要记一行：%s", out)
	}
	e.suggestOut()
	j2 := e.endSuggest()
	if len(j2.SuggestProjects) != 2 {
		t.Errorf("恢复后两个都看：%v", j2.SuggestProjects)
	}
}

// 临时写不进（BACKLOG 有未提交改动）：下次照样重试，但同一项目同样的原因只推一次；写进去之后原因记录清掉。
func TestSuggestTemporaryFailurePushedOnce(t *testing.T) {
	t.Parallel()
	e, tg := sgEnv(t, nil)
	p := e.sgProject("alpha", sgBacklog)
	os.WriteFile(p.BacklogPath(), []byte(sgBacklog+"\n用户正在编辑\n"), 0o644)
	pushes := func() int {
		n := 0
		for _, s := range tg.texts() {
			if strings.Contains(s, "闲时找活") {
				n++
			}
		}
		return n
	}
	settle := func() {
		for i := 0; i < 3; i++ {
			e.mustRun("tick")
		}
	}
	e.suggestOut(sgItem("alpha", "新活", "product"))
	e.runSuggest()
	settle()
	if pushes() != 1 {
		t.Fatalf("第一天推一次：%q", tg.texts())
	}
	for d := 0; d < 2; d++ { // 之后几天：仍然重试（会花钱，这是要求的），但不再推
		y, m, dd := time.Now().AddDate(0, 0, d+1).Date()
		e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, dd, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
		j := e.runSuggest()
		settle()
		if j.Suggest == nil || len(j.Suggest.Failed) != 1 || len(j.Suggest.NewFailed) != 0 {
			t.Errorf("第 %d 天：仍然失败但不是新原因：%+v", d+2, j.Suggest)
		}
	}
	if pushes() != 1 {
		t.Errorf("同一项目同样的原因当天之后不重复推：%q", tg.texts())
	}
	if len(e.store().FailNotes) != 1 {
		t.Errorf("记下推过的原因：%+v", e.store().FailNotes)
	}
	// 用户提交了编辑：写得进去，推一条，原因记录清掉
	testkit.GitT(t, e.home, p.Dir, "commit", "-qam", "用户的编辑")
	y, m, dd := time.Now().AddDate(0, 0, 4).Date()
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Date(y, m, dd, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
	e.runSuggest()
	settle()
	if pushes() != 2 || len(e.store().FailNotes) != 0 {
		t.Errorf("写进去了要推，原因记录清掉：pushes=%d notes=%+v", pushes(), e.store().FailNotes)
	}
}

// 混用：默认后端撞限中，别的后端让 tick 还在跑，闲时找活不能在撞限的后端上起进程。
func TestSuggestRespectsDefaultCooldownWhenMixed(t *testing.T) {
	t.Parallel()
	e, _ := sgEnv(t, map[string]any{"project_backends": map[string]any{"cx": "codex"}, "backends": map[string]any{"codex": map[string]any{"path": codexFake(t), "permission": "danger-full-access"}}})
	testkit.MkProject(t, e.root, "aa", sgBacklog)
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	st := e.state()
	st.CooldownUntil = time.Now().Add(72 * time.Hour) // 夹具的时钟定在当天中午，和真实时间可能差半天
	if err := state.Save(statePath, st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if n := len(e.suggestJobs()); n != 0 {
		t.Errorf("撞限中不找活：%d", n)
	}
}
