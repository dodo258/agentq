package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/pm"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 产品经理调研的用例：假 claude 认「产品经理调研进程」，默认给两条带出处的建议（见 fakeClaude）。

func pmJobs(e *testEnv) []*Job {
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Kind == state.KindPM {
			out = append(out, j)
		}
	}
	return out
}

// pmRan：起了正好 n 个调研任务；最后一个等它结束、再 tick 一次收尾（建议写进 BACKLOG）。
func pmRan(t *testing.T, e *testEnv, n int) *Job {
	t.Helper()
	jobs := pmJobs(e)
	if len(jobs) != n {
		t.Fatalf("要起 %d 个产品经理调研：%d 个\n%s", n, len(jobs), e.launchdLog())
	}
	if n == 0 {
		return nil
	}
	j := jobs[n-1]
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	return j
}

// suggestNums：「## 建议」里每条的编号。
func suggestNums(b string) []string {
	return regexp.MustCompile(`（建议 (\d+)）`).FindAllString(b, -1)
}

// 第一版过门合并后起一次：只读加联网、不接 MCP、在项目目录里跑；建议带出处进「## 建议」，推一条；再 tick 不重复；回「做 N」移进待做。
func TestPMAfterFirstVersion(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"discuss_enabled": false, "pm_enabled": true, "paused": false})
	e.atNoon()
	e.mustRun("new", "NEWOK 做个工具")
	first := e.state().Jobs[0]
	waitExit(t, first.PID, 15*time.Second)
	e.mustRun("tick")
	if n := len(pmJobs(e)); n != 0 {
		t.Fatalf("第一版还在过门，不调研：%d", n)
	}
	e.waitGate(e.state().Jobs[0])
	j := pmRan(t, e, 1)
	dir := filepath.Join(e.root, "clip-tool")
	if j.Project != "clip-tool" || j.ProjectDir != dir {
		t.Errorf("项目：%s %s", j.Project, j.ProjectDir)
	}
	log := e.fakeLogOf(j)
	for _, want := range []string{"ARG[--tools]\nARG[Read,Grep,Glob,WebSearch,WebFetch]", "ARG[--allowedTools]\nARG[WebSearch,WebFetch]", "ARG[--strict-mcp-config]", "ARG[--json-schema]", "PWD[" + testkit.RealPath(t, dir) + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("参数缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--permission-mode]") || strings.Contains(log, "Bash") || strings.Contains(log, "Write") {
		t.Errorf("只读：不放写文件和跑命令，不传权限模式：\n%s", log)
	}
	if p := promptOf(t, e, j); !strings.Contains(p, "第一版刚合并") || !strings.Contains(p, "GitHub issues") || !strings.Contains(p, "source") {
		t.Errorf("提示词：%s", p)
	}
	b := e.backlogOf(dir)
	sec := b[strings.Index(b, "## 建议"):]
	for _, want := range []string{"- [ ] 加导出 CSV —— 同类工具都有，用户在 issue 里反复要；出处：https://github.com/example/clipper/issues/12；产品经理调研 ", "- [ ] 第一次打开放示例数据 —— README 写的定位是给新手用，空白页留不住人；出处：README 写的定位：给新手用的小工具"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("建议要带出处和为什么：\n%s", b)
		}
	}
	if todo := b[:strings.Index(b, "## 建议")]; strings.Contains(todo, "加导出 CSV") {
		t.Errorf("不直接写进待做：\n%s", b)
	}
	nums := suggestNums(b)
	if len(nums) != 2 {
		t.Fatalf("%v\n%s", nums, b)
	}
	if !strings.Contains(testkit.GitT(t, e.home, dir, "log", "--format=%s", "main"), "产品经理调研的下一步建议") {
		t.Errorf("要提交")
	}
	sent := strings.Join(tg.texts(), "\n")
	if !strings.Contains(sent, "clip-tool：产品经理调研了同类，写了 2 条下一步进「建议」") || !strings.Contains(sent, "加导出 CSV") {
		t.Errorf("有新建议推一条：\n%s", sent)
	}
	if !strings.Contains(e.nightly(), "产品经理：下一步写进「建议」") {
		t.Errorf("当天汇总：\n%s", e.nightly())
	}
	for range 2 {
		e.mustRun("tick")
	}
	if n := len(pmJobs(e)); n != 1 {
		t.Errorf("第一版只调研一次：%d", n)
	}
	// 回「做 N」：移进待做
	n := strings.TrimSuffix(strings.TrimPrefix(nums[0], "（建议 "), "）")
	tg.msg(1, ownerChat, 11, "做 "+n, "")
	e.mustRun("tick")
	b = e.backlogOf(dir)
	todo := b[:strings.Index(b, "## 建议")]
	if !strings.Contains(todo, "- [ ] 加导出 CSV —— 同类工具都有") || !strings.Contains(todo, "出处：https://github.com/example/clipper/issues/12") {
		t.Fatalf("回「做 %s」要移进待做：\n%s\n%s", n, b, strings.Join(tg.texts(), "\n"))
	}
}

// pmMerge：在 demo 的待做里加一条 READYMERGE 并做到过门合并。
func pmMerge(t *testing.T, e *testEnv, dir, title, branch, file string) {
	t.Helper()
	b := e.backlogOf(dir)
	b = strings.Replace(b, "## 待做\n\n", "## 待做\n\n- [ ] "+title+" READYMERGE —— BRANCH="+branch+" FILE="+file+"\n", 1)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(b), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-qam", "加一条")
	e.mustRun("tick")
	if j := e.tickUntilGate(title + " READYMERGE"); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
}

// 待做清空、「建议」里没有它提过没回的才起（别人提的建议不挡）；14 天内不重复；它提的还没回不起；回完了、满 14 天、又做完一版再起。
func TestPMWhenTodoEmpty(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"pm_enabled": true})
	e.atNoon()
	dir := gateRepo(t, e, "demo", "- [ ] 第一条 READYMERGE —— BRANCH=agent/t-a FILE=a.txt", "- [ ] 第二条")
	b := e.backlogOf(dir)
	b = strings.Replace(b, "## 已完成", "## 建议\n\n- [ ] 别处提的 —— 闲时找活写的（建议 90）\n\n## 已完成", 1)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(b), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-qam", "建议")
	e.mustRun("tick")
	e.tickUntilGate("第一条 READYMERGE")
	if n := len(pmJobs(e)); n != 0 {
		t.Fatalf("待做还有「第二条」，不调研：%d", n)
	}
	// 第二条做完（默认假 claude 只说做完，不改 BACKLOG），用户手动勾掉
	waitExit(t, e.jobByTitle("第二条").PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	e.mustRun("resume")
	b = strings.Replace(e.backlogOf(dir), "- [ ] 第二条\n", "", 1)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(b), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-qam", "勾掉")
	e.mustRun("tick")
	j := pmRan(t, e, 1)
	if p := promptOf(t, e, j); !strings.Contains(p, "待做都做完了") {
		t.Errorf("%s", p)
	}
	if got := suggestNums(e.backlogOf(dir)); len(got) != 3 {
		t.Fatalf("别处的 1 条加它的 2 条：%v", got)
	}
	// 14 天内又做完一版：不起
	pmMerge(t, e, dir, "第三条", "agent/t-c", "c.txt")
	pmRan(t, e, 1)
	// 满 14 天：它提的两条还没回，不起
	e.later(pm.Every + time.Hour)
	e.mustRun("tick")
	pmRan(t, e, 1)
	// 用户回了（这里直接删掉那两行）：起
	var kept []string
	for _, l := range strings.Split(e.backlogOf(dir), "\n") {
		if !strings.Contains(l, "产品经理调研") {
			kept = append(kept, l)
		}
	}
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Join(kept, "\n")), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-qam", "回了")
	e.mustRun("tick")
	pmRan(t, e, 2)
	// 同一天里再没新合并：不起
	e.mustRun("tick")
	pmRan(t, e, 2)
}

// 额度到后台上限不起（日志说原因），额度回落再起。
func TestPMUsageLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"pm_enabled": false})
	e.atNoon()
	dir := gateRepo(t, e, "demo", "- [ ] 第一条 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	e.tickUntilGate("第一条 READYMERGE")
	e.mergeConfig(map[string]any{"pm_enabled": true})
	e.setUsage(usageLine("75"))
	e.expireUsage()
	out := e.mustRun("tick")
	pmRan(t, e, 0)
	if !strings.Contains(out+e.launchdLog(), "产品经理：demo 这次不调研") {
		t.Fatalf("日志要说原因：\n%s\n%s", out, e.launchdLog())
	}
	e.setUsage(usageLine("20"))
	e.expireUsage()
	e.mustRun("tick")
	pmRan(t, e, 1)
	if !strings.Contains(e.backlogOf(dir), "加导出 CSV") {
		t.Errorf("%s", e.backlogOf(dir))
	}
}

// exclude_projects 里的（亲手 run 的照样合并）、self_project 都不起；pm_enabled 关着也不起。
func TestPMSkippedProjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"pm_enabled": true, "exclude_projects": []string{"ex"}, "self_project": "me", "max_parallel": 3})
	e.atNoon()
	ex := gateRepo(t, e, "ex", "- [ ] 排除的 READYMERGE —— BRANCH=agent/t-x FILE=x.txt")
	gateRepo(t, e, "me", "- [ ] 自己的 READYMERGE —— BRANCH=agent/t-m FILE=m.txt")
	e.mustRun("run", "ex", "排除的")
	e.mustRun("tick")
	for _, title := range []string{"排除的 READYMERGE", "自己的 READYMERGE"} {
		waitExit(t, e.jobByTitle(title).PID, 20*time.Second)
	}
	e.mustRun("tick")
	for _, title := range []string{"排除的 READYMERGE", "自己的 READYMERGE"} {
		j := e.jobByTitle(title)
		waitFor(t, 30*time.Second, "门的结果", func() bool {
			_, err := os.Stat(e.gateResult(j))
			return err == nil
		})
	}
	e.mustRun("tick")
	for _, title := range []string{"排除的 READYMERGE", "自己的 READYMERGE"} {
		if j := e.jobByTitle(title); j.Gate == nil || j.Gate.State != state.GateMerged {
			t.Fatalf("%s 要合并：%+v", title, j.Gate)
		}
	}
	e.mustRun("tick")
	pmRan(t, e, 0)
	if strings.Contains(e.backlogOf(ex), "## 建议") {
		t.Errorf("%s", e.backlogOf(ex))
	}
	// 关掉开关：不在排除里的普通项目也不起
	e.mergeConfig(map[string]any{"pm_enabled": false, "exclude_projects": []string{}, "self_project": ""})
	e.mustRun("tick")
	pmRan(t, e, 0)
}

// 模型没给出处的不写；一条合格的都没有就不写、不推，当天汇总留一句，14 天后再看。
func TestPMDropsUnsourced(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"pm_enabled": true})
	e.atNoon()
	os.WriteFile(filepath.Join(e.home, "fake-pm.json"), []byte(`{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s-pm","structured_output":{"suggestions":[{"title":"提升体验","why":"更好用","source":""}]}}`+"\n"), 0o644)
	dir := gateRepo(t, e, "demo", "- [ ] 第一条 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	e.tickUntilGate("第一条 READYMERGE")
	pmRan(t, e, 1)
	if strings.Contains(e.backlogOf(dir), "提升体验") {
		t.Errorf("没出处的不写：\n%s", e.backlogOf(dir))
	}
	if !strings.Contains(e.nightly(), "没有值得提的下一步") {
		t.Errorf("%s", e.nightly())
	}
	e.mustRun("tick")
	pmRan(t, e, 1)
}

// 后端做不到只读联网：产品经理调研不起，doctor 的能力表说明。
func TestPMNeedsReadOnlyWeb(t *testing.T) {
	t.Parallel()
	names := backend.Names()
	if !slices.Contains(names, "codex") || !slices.Contains(names, "pi") {
		t.Fatalf("后端没注册全：%v", names)
	}
	for _, name := range names {
		be := backend.Get(name, backend.Options{})
		lacks := be.Caps().Lacks(backend.MustPM)
		if name == "claude" && len(lacks) != 0 {
			t.Errorf("claude 做得到：%v", lacks)
		}
		if name != "claude" && be.Caps().WebTools && len(lacks) != 0 {
			t.Errorf("%s 声明了 WebTools：%v", name, lacks)
		}
		if !be.Caps().WebTools && !strings.Contains(strings.Join(lacks, "；"), "联网") {
			t.Errorf("%s 做不到要说原因：%v", name, lacks)
		}
	}
}
