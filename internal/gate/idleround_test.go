package gate_test

import (
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 照任务 1124-021619-agentq → 1124-051336-agentq：第 1 轮测试门没过是默认分支自己的毛病（整套测试撞时限），
// main 修好后任务只合了 main、一个文件没改就过了测试门，接着审查门查出问题。白占的那轮要还回来：退回接着改，不标卡住、不说是计划的问题。

const (
	verifyBad  = "#!/bin/sh\necho 撞时限\nexit 1\n"
	verifyGood = "#!/bin/sh\necho 好了\n"
)

// nextHow 是第 2 轮怎么接上第 1 轮的。
type nextHow int

const (
	nextResume     nextHow = iota // 同一会话接着做
	nextAfterBreak                // 中间中断过一次再接着做
	nextTakeover                  // 接手：新项目第一版、不能续跑的后端
)

// idleSetup：第 1 轮被测试门退回，main 上的门脚本换成 after，分支上按 files 提交一次，再起第 2 轮，这次登记一道没过的 zz 门。
func idleSetup(t *testing.T, after string, files map[string]string, how nextHow) (*fix, *state.Job, gate.RunResult) {
	t.Helper()
	f := newFix(t, fixBacklog)
	f.verifyOnMain(verifyBad)
	j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a"}, ready("agent/x"))
	j.Gate.Round, j.GateRound = 1, 1
	first := f.run(j)
	if first.Outcome != "fail" {
		t.Fatalf("第 1 轮：%+v", first)
	}
	f.collect(j, first)
	if j.Gate.State != state.GateReturned || len(j.Gate.Scope) == 0 {
		t.Fatalf("第 1 轮要退回、带范围：%+v", j.Gate)
	}
	f.verifyOnMain(after)
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	f.commit(wt, files, ready("agent/x"))
	failOn(t, "zz")
	prev := j
	if how == nextAfterBreak { // 第 2 轮做到一半中断过一次：中间那个任务没过门
		m := &state.Job{ID: "1105-100050-demo", Project: "demo", ProjectDir: f.dir, Status: state.StatusInterrupted, ResumeOf: j.ID, GateRound: 2, GateScope: j.Gate.Scope, GateScopeFrom: j.Gate.Head}
		f.st.Jobs = append(f.st.Jobs, m)
		prev = m
	}
	j2 := f.nextRound(j)
	j2.ResumeOf = prev.ID
	if how == nextTakeover { // 和 orphan.LaunchTakeover 建的一样：没有 ResumeOf
		j2.ResumeOf, j2.Takeover = "", true
	}
	if j2.Gate.Round != gate.MaxRounds {
		t.Fatalf("第 2 轮：%+v", j2.Gate)
	}
	res := f.run(j2)
	return f, j2, res
}

func TestIdleRoundGivenBack(t *testing.T) {
	for name, how := range map[string]nextHow{"接着做": nextResume, "中间中断过": nextAfterBreak, "接手": nextTakeover} {
		t.Run(name, func(t *testing.T) {
			f, j, res := idleSetup(t, verifyGood, nil, how)
			scope, _ := resultOf(res, "scope")
			if res.Outcome != "fail" || !scope.NoChange {
				t.Fatalf("第 2 轮只合了 main，scope 门要记没改：%+v", res)
			}
			f.collect(j, res)
			g := j.Gate
			if g.State != state.GateReturned || g.Round != gate.MaxRounds-1 || j.StuckPending != "" || !j.ResumePending {
				t.Fatalf("白占的一轮要还回来：%+v %+v", g, j)
			}
			if !strings.Contains(j.Summary, "没改代码测试就过了") || strings.Contains(f.backlog(), "（卡住") {
				t.Errorf("摘要：%s", j.Summary)
			}
		})
	}
}

// 合了 main 的也一样：自动合出来的内容和分支尖对得上，就是没改。
func TestIdleRoundMergedMain(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain(verifyBad)
	j := f.round1("feature", map[string]string{"pkg/a.go": "a"}, ".agentq/verify 没过")
	f.commitMain(map[string]string{"pkg/b.go": "别的任务合进来的"})
	f.verifyOnMain(verifyGood)
	testkit.GitT(t, f.home, f.wt(), "merge", "-q", "--no-edit", "main")
	f.commit(f.wt(), nil, gate.Ready{Branch: "agent/x", Type: "feature", Acceptance: "看 a.txt", Progress: "合了最新 main 重交"})
	failOn(t, "zz")
	j2 := f.nextRound(j)
	j2.ResumeOf = j.ID
	f.collect(j2, f.run(j2))
	if g := j2.Gate; g.State != state.GateReturned || g.Round != gate.MaxRounds-1 {
		t.Fatalf("白占的一轮要还回来：%+v", g)
	}
}

// 合 main 时只在只追加的记录文件上撞了冲突、两边各自新加的都留下：和引擎替任务合的一样，也是没改。谁在前都行。
func TestIdleRoundMergedAppendConflict(t *testing.T) {
	for name, tc := range map[string]struct {
		resolved string
		want     string
	}{
		"main 在前": {"- main 修门\n- 任务\n", state.GateReturned},
		"任务在前":    {"- 任务\n- main 修门\n", state.GateReturned},
		"借合并加了内容": {"- main 修门\n- 任务\n- 顺手加的\n", state.GateStuck},
		"空行合得不一样": {"\n- main 修门\n\n- 任务\n", state.GateReturned},
		"丢了一边":    {"- 任务\n", state.GateStuck},
	} {
		t.Run(name, func(t *testing.T) {
			f, j := appendConflict(t)
			f.commit(f.wt(), map[string]string{"CHANGELOG.md": logHead + "- 旧\n" + tc.resolved}, gate.Ready{Branch: "agent/x", Type: "feature", Acceptance: "看 a.txt", Progress: "合了最新 main 重交"})
			f.wantState(j, tc.want)
		})
	}
	t.Run("同时改了代码", func(t *testing.T) {
		f, j := appendConflict(t)
		f.commit(f.wt(), map[string]string{"CHANGELOG.md": logHead + "- 旧\n- main 修门\n- 任务\n", "pkg/a.go": "a2"}, gate.Ready{})
		f.wantState(j, state.GateStuck)
	})
	t.Run("代码文件也有冲突", func(t *testing.T) { // 留下任务那一份，内容看着没变，也是任务自己解的
		f := newFix(t, fixBacklog)
		f.commitMain(map[string]string{"pkg/a.go": "旧"})
		f.verifyOnMain(verifyBad)
		j := f.round1("feature", map[string]string{"pkg/a.go": "任务"}, ".agentq/verify 没过")
		f.git("rm", "-q", "pkg/a.go")
		f.git("commit", "-q", "-m", "main 删了")
		f.verifyOnMain(verifyGood)
		f.mergeMain("pkg/a.go")
		f.commit(f.wt(), map[string]string{"pkg/a.go": "任务"}, gate.Ready{})
		f.wantState(j, state.GateStuck)
	})
	t.Run("改的是原有的行", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.commitMain(map[string]string{"CHANGELOG.md": logHead + "- 旧\n"})
		f.verifyOnMain(verifyBad)
		j := f.round1("feature", map[string]string{"CHANGELOG.md": logHead + "- 任务改的旧\n", "pkg/a.go": "a"}, ".agentq/verify 没过")
		f.commitMain(map[string]string{"CHANGELOG.md": logHead + "- main 改的旧\n"})
		f.verifyOnMain(verifyGood)
		f.mergeMain("CHANGELOG.md")
		f.commit(f.wt(), map[string]string{"CHANGELOG.md": logHead + "- main 改的旧\n- 任务改的旧\n"}, gate.Ready{})
		f.wantState(j, state.GateStuck)
	})
}

const logHead = "# c\n\n## 未发布\n\n"

// appendConflict：任务在第 1 轮被测试门退回，main 修好了门、也在 CHANGELOG.md 同一处加了一条；任务合 main 撞上冲突，还没解。
func appendConflict(t *testing.T) (*fix, *state.Job) {
	t.Helper()
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"CHANGELOG.md": logHead + "- 旧\n"})
	f.verifyOnMain(verifyBad)
	j := f.round1("feature", map[string]string{"pkg/a.go": "a", "CHANGELOG.md": logHead + "- 旧\n- 任务\n"}, ".agentq/verify 没过")
	f.commitMain(map[string]string{"CHANGELOG.md": logHead + "- 旧\n- main 修门\n"})
	f.verifyOnMain(verifyGood)
	f.mergeMain("CHANGELOG.md")
	return f, j
}

// mergeMain 在任务分支上合 main，停在 file 有冲突。
func (f *fix) mergeMain(file string) {
	f.t.Helper()
	testkit.GitTErr(f.home, f.wt(), "merge", "-q", "--no-commit", "--no-ff", "main") // 有冲突退出 1，下面查的就是它
	if got := testkit.GitT(f.t, f.home, f.wt(), "diff", "--name-only", "--diff-filter=U"); strings.TrimSpace(got) != file {
		f.t.Fatalf("要撞上 %s 的冲突：%q", file, got)
	}
}

// 照旧占轮数的：这一轮改了代码或记录文件；在合并提交里改了；测试门这次还没过；上一轮不是脚本没过。
func TestIdleRoundStillCounts(t *testing.T) {
	t.Run("改了代码", func(t *testing.T) {
		f, j, res := idleSetup(t, verifyGood, map[string]string{"pkg/a.go": "a2"}, nextResume)
		f.collect(j, res)
		if g := j.Gate; g.State != state.GateStuck || !gate.IsPlanReport(g.Report) {
			t.Fatalf("%+v", g)
		}
	})
	t.Run("测试还没过", func(t *testing.T) {
		f, j, res := idleSetup(t, verifyBad, nil, nextResume)
		f.collect(j, res)
		if j.Gate.State != state.GateStuck {
			t.Fatalf("%+v", j.Gate)
		}
	})
	t.Run("审查退回的", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(verifyGood)
		failOn(t, "zz")
		j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a"}, ready("agent/x"))
		j.Gate.Round, j.GateRound = 1, 1
		f.collect(j, f.run(j))
		f.commit(filepath.Join(f.dir, ".claude", "worktrees", "x"), nil, ready("agent/x"))
		j2 := f.nextRound(j)
		j2.ResumeOf = j.ID
		f.collect(j2, f.run(j2))
		if j2.Gate.State != state.GateStuck {
			t.Fatalf("%+v", j2.Gate)
		}
	})
	t.Run("别的分支被退回的不算", func(t *testing.T) {
		f, j, res := idleSetup(t, verifyGood, nil, nextTakeover)
		for _, o := range f.st.Jobs {
			if o.ID != j.ID {
				o.Gate.Branch = "agent/other"
			}
		}
		f.collect(j, res)
		if j.Gate.State != state.GateStuck {
			t.Fatalf("%+v", j.Gate)
		}
	})
	t.Run("改了记录文件", func(t *testing.T) { // 门脚本可能就查它
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\n! grep -n secret CHANGELOG.md\n")
		j := f.round1("feature", map[string]string{"pkg/a.go": "a", "CHANGELOG.md": "secret\n"}, ".agentq/verify 没过")
		f.commit(f.wt(), map[string]string{"CHANGELOG.md": "ok\n"}, ready("agent/x"))
		f.wantStuck(j)
	})
	t.Run("合并提交里改的", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\n! grep -q bad pkg/a.go\n")
		j := f.round1("feature", map[string]string{"pkg/a.go": "bad"}, ".agentq/verify 没过")
		f.verifyOnMain("#!/bin/sh\n# 动一下\n! grep -q bad pkg/a.go\n")
		testkit.GitT(t, f.home, f.wt(), "merge", "-q", "--no-commit", "--no-ff", "main")
		f.commit(f.wt(), map[string]string{"pkg/a.go": "good"}, gate.Ready{})
		f.wantStuck(j)
	})
	t.Run("补了门说明", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(verifyGood)
		j := f.round1("feature", map[string]string{"pkg/a.go": "a", "pkg/a_test.go": "t." + "Skip()\n"}, "没写原因")
		testkit.GitT(t, f.home, f.wt(), "commit", "-q", "--allow-empty", "-m", "补说明\n\n"+gate.NoteMark+"等上游修")
		f.wantStuck(j)
	})
	t.Run("换 type 躲开撤修复检查", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain(verifyGood)
		j := f.round1("fix", map[string]string{"pkg/a.go": "a", "pkg/a_test.go": "func TestA(t) {}\n"}, "撤掉修复测试还是过")
		f.commit(f.wt(), nil, ready("agent/x"))
		f.wantStuck(j)
	})
}

// wt 是 agent/x 的 worktree。
func (f *fix) wt() string { return filepath.Join(f.dir, ".claude", "worktrees", "x") }

// round1：任务在第 1 轮被测试门退回，摘要里有 why。
func (f *fix) round1(typ string, files map[string]string, why string) *state.Job {
	f.t.Helper()
	r := ready("agent/x")
	r.Type = typ
	j := f.claim("做事", "agent/x", files, r)
	j.Gate.Round, j.GateRound = 1, 1
	res := f.run(j)
	if got := testResult(f.t, res); got.Outcome != gate.Fail || !strings.Contains(got.Summary, why) {
		f.t.Fatalf("第 1 轮：%+v", res)
	}
	f.collect(j, res)
	return j
}

// wantStuck：照旧占轮数，停下等用户。
func (f *fix) wantStuck(prev *state.Job) {
	f.t.Helper()
	f.wantState(prev, state.GateStuck)
}

// wantState：接着 prev 做的第 2 轮，测试门过了、另一道门没过，门要停在 want。
func (f *fix) wantState(prev *state.Job, want string) {
	f.t.Helper()
	failOn(f.t, "zz")
	j := f.nextRound(prev)
	j.ResumeOf = prev.ID
	res := f.run(j)
	if got := testResult(f.t, res); got.Outcome != gate.Pass {
		f.t.Fatalf("第 2 轮测试门要过：%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != want {
		f.t.Fatalf("门要停在 %s：%+v %s", want, j.Gate, j.Summary)
	}
}
