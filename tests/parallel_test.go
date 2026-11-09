package tests

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 同一项目里写了不重叠范围、没有依赖的两条：一次 tick 一起启动（默认 per_project 2），各自分支，都过门合并进 main。
func TestParallelScopedItemsBothMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo",
		"- [ ] 修甲 READYMERGE —— BRANCH=agent/p-a FILE=a.txt（范围：a.txt）",
		"- [ ] 修乙 READYMERGE —— BRANCH=agent/p-b FILE=b.txt SLEEP=1（范围：b.txt）") // 乙晚一秒认领，两个假 claude 不抢主目录的 git 锁
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 2") {
		t.Fatalf("两条应在同一次 tick 启动：\n%s", out)
	}
	a, b := e.jobByTitle("修甲 READYMERGE"), e.jobByTitle("修乙 READYMERGE")
	if a.Status != state.StatusRunning || b.Status != state.StatusRunning || a.PID == b.PID {
		t.Fatalf("应同时在跑：%+v %+v", a, b)
	}
	waitExit(t, a.PID, 20*time.Second)
	waitExit(t, b.PID, 20*time.Second)
	deadline := time.Now().Add(90 * time.Second)
	for {
		e.mustRun("tick")
		a, b = e.jobByTitle("修甲 READYMERGE"), e.jobByTitle("修乙 READYMERGE")
		if a.Gate != nil && b.Gate != nil && a.Gate.State == state.GateMerged && b.Gate.State == state.GateMerged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("90 秒内没都合并：%+v %+v\n%s", a.Gate, b.Gate, e.backlogOf(dir))
		}
		time.Sleep(300 * time.Millisecond)
	}
	if a.Gate.Branch != "agent/p-a" || b.Gate.Branch != "agent/p-b" {
		t.Errorf("各自分支：%s %s", a.Gate.Branch, b.Gate.Branch)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s 没进 main", f)
		}
	}
	bl := e.backlogOf(dir)
	if !strings.Contains(bl, "- [x] 修甲 READYMERGE") || !strings.Contains(bl, "- [x] 修乙 READYMERGE") || strings.Contains(bl, "（进行中") {
		t.Errorf("两条都应移到已完成：\n%s", bl)
	}
}

// 有依赖的按顺序；范围重叠、没写范围的不并行；依赖找不到 status 说一句；正文顺带写的「依赖：」「范围：」不当标记。
func TestParallelRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 8})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=20（范围：a/）\n- [ ] 甲二 —— SLEEP=20（范围：a/b/）\n- [ ] 甲三 —— SLEEP=20\n")
	beta := testkit.MkProject(t, e.root, "beta", "- [ ] 乙二 —— SLEEP=20（依赖：乙一）（范围：y/）\n- [ ] 乙一 —— SLEEP=20（范围：x/）\n")
	testkit.MkProject(t, e.root, "gamma", "- [ ] 丙一 —— SLEEP=20（依赖：不存在的活）（范围：c/）\n- [ ] 丙二 —— SLEEP=20 正文提到依赖：不存在的活（范围：d/）\n")
	testkit.MkProject(t, e.root, "delta", "- [ ] 丁一 —— SLEEP=20 范围：d/\n- [ ] 丁二 —— SLEEP=20 范围：e/\n")
	defer func() {
		for _, j := range e.state().Running() {
			e.run("stop", j.ID)
		}
	}()
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 4") {
		t.Fatalf("每个项目只该起一条：\n%s", out)
	}
	var titles []string
	for _, j := range e.state().Running() {
		titles = append(titles, j.Title)
	}
	slices.Sort(titles)
	if want := []string{"丁一", "丙二", "乙一", "甲一"}; !slices.Equal(titles, want) {
		t.Fatalf("在跑的：%q，想要 %q", titles, want)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Errorf("还有名额也不该再起：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "「丙一」依赖『不存在的活』找不到") || strings.Contains(out, "「乙二」依赖") {
		t.Errorf("status 只说找不到的依赖：\n%s", out)
	}
	if out, code := e.run("run", "alpha"); code == 0 || !strings.Contains(out, "（范围：…）且不重叠") {
		t.Errorf("run 也守范围，并说明为什么：\n%s", out)
	}

	// 乙一停了名额空出来，乙二照样等它做完
	e.mustRun("stop", e.jobByTitle("乙一").ID)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("乙一没做完，乙二不该起：\n%s", out)
	}
	// 乙一做完移到「已完成」：乙二起
	os.WriteFile(filepath.Join(beta, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n- [ ] 乙二 —— SLEEP=20（依赖：乙一）（范围：y/）\n\n## 已完成\n\n- [x] 乙一 —— 合并提交 abc1234\n"), 0o644)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("依赖做完了，乙二应启动：\n%s", out)
	}
	if j := e.jobByTitle("乙二"); j.Status != state.StatusRunning {
		t.Errorf("乙二：%+v", j)
	}
}

// 续跑也守范围：中断的条目没写范围，同项目已有任务在跑时不续跑，等它结束。
func TestParallelResumeWaitsForUnscoped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT\n- [ ] 甲二 —— SLEEP=20\n")
	defer func() {
		for _, j := range e.state().Running() {
			e.run("stop", j.ID)
		}
	}()
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("先起中断一：\n%s", out)
	}
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	e.mustRun("pause")
	e.mustRun("tick") // 收尾成中断、等续跑；暂停着不续跑
	e.mustRun("run", "alpha", "甲二")
	e.mustRun("resume")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("甲二在跑、两条都没写范围，不该续跑：\n%s", out)
	}
	if j := e.jobByTitle("中断一"); j.Status != state.StatusInterrupted || !j.ResumePending {
		t.Errorf("中断一应还等着续跑：%+v", j)
	}
}

// 没写范围的中断任务等续跑时，同项目写了范围的新条目不插队：这次 tick 不起新条目，在跑的结束后先续跑它。
func TestParallelResumeNotOvertaken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT\n- [ ] 甲一 —— SLEEP=20（范围：a/）\n- [ ] 甲三 —— SLEEP=20（范围：c/）\n")
	defer func() {
		for _, j := range e.state().Running() {
			e.run("stop", j.ID)
		}
	}()
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("先起中断一：\n%s", out)
	}
	x := e.jobByTitle("中断一")
	waitExit(t, x.PID, 10*time.Second)
	e.mustRun("pause")
	e.mustRun("tick") // 收尾成中断、等续跑
	e.mustRun("run", "alpha", "甲一")
	e.mustRun("resume")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
		t.Fatalf("中断一等续跑，甲三不该插队：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "等续跑：alpha「中断一」") || !strings.Contains(out, "这个项目先不起新条目") {
		t.Errorf("status 要说为什么不起：\n%s", out)
	}
	e.mustRun("stop", e.jobByTitle("甲一").ID)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("甲一停了应先续跑中断一：\n%s", out)
	}
	var resumedX, started3 bool
	for _, j := range e.state().Running() {
		resumedX = resumedX || j.ResumeOf == x.ID
		started3 = started3 || j.Title == "甲三"
	}
	if !resumedX || started3 {
		t.Errorf("应续跑中断一、不起甲三：%+v", e.state().Running())
	}
}
