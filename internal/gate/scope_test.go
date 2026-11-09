package gate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// failOn 登记一道 error 档的假门：没过，点名 files（可以是空的）。
func failOn(t *testing.T, name string, files ...string) {
	t.Helper()
	register(t, fakeGate{name: name, applies: true, run: func(c gate.Candidate) gate.Result {
		return gate.Result{Outcome: gate.Fail, Summary: name + " 没过", Files: files}
	}}, gate.LevelError)
}

// nextRound 像 runner 接着做退回的任务那样起下一轮：带上退回时的范围和上一轮的分支尖，排队过门。
func (f *fix) nextRound(prev *state.Job) *state.Job {
	f.t.Helper()
	g := prev.Gate
	if g.State != state.GateReturned {
		f.t.Fatalf("上一轮没退回：%+v", g)
	}
	j := &state.Job{ID: prev.ID + "r", Project: prev.Project, ProjectDir: prev.ProjectDir, DefaultBranch: prev.DefaultBranch, Title: prev.Title, Item: prev.Item,
		Fingerprint: prev.Fingerprint, Status: state.StatusDone, SessionID: "s1", StartedAt: f.a.Now(), GateRound: g.Round + 1, GateScope: g.Scope, GateScopeFrom: g.Head}
	os.MkdirAll(f.a.JobDir(j.ID), 0o755)
	f.st.Jobs = append(f.st.Jobs, j)
	got, head, found, err := f.a.ReadReady(f.dir, g.Branch)
	if err != nil || !found {
		f.t.Fatalf("ReadReady：%v %v", found, err)
	}
	f.a.Enqueue(j, got, head)
	return j
}

// 退回提示带范围行：没过的门点名的文件，加同目录已有的测试文件；别的改动不算进去。
func TestReturnNoteHasScope(t *testing.T) {
	f := newFix(t, fixBacklog)
	failOn(t, "zz", "pkg/a.go")
	j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "pkg/a_test.go": "t", "pkg/b.go": "b", "other/c.go": "c", "pkg/sub/d_test.go": "d"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "fail" {
		t.Fatalf("%+v", res)
	}
	if want := []string{"pkg/a.go", "pkg/a_test.go"}; !slices.Equal(res.Scope, want) {
		t.Fatalf("范围 %v，要 %v", res.Scope, want)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateReturned || !slices.Equal(j.Gate.Scope, res.Scope) {
		t.Fatalf("%+v", j.Gate)
	}
	note := f.a.ReturnNote(j)
	if !strings.Contains(note, "范围：只改这些文件：pkg/a.go、pkg/a_test.go") || !strings.Contains(note, "要改范围外的文件就不要写 ready.json，按 agent-task 写卡住标记说明为什么。") {
		t.Errorf("退回提示没有范围行：\n%s", note)
	}
}

// 门都没点名文件：范围是这一轮改的文件，加同目录已有的测试文件（子目录不展开）。
func TestReturnScopeFallsBackToChangedFiles(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"pkg/old_test.go": "旧测试", "pkg/tests/e_test.py": "不展开"})
	failOn(t, "zz")
	j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "README.md": "r"}, ready("agent/x"))
	res := f.run(j)
	if want := []string{"README.md", "pkg/a.go", "pkg/old_test.go"}; !slices.Equal(res.Scope, want) {
		t.Fatalf("范围 %v，要 %v", res.Scope, want)
	}
}

// 下一轮：只改范围内的（含同目录新加的测试）照常过；改了范围外的 scope 门判不过并列出文件；合 main 带来的不算；首轮不跑 scope 门。
func TestScopeGateNextRound(t *testing.T) {
	setup := func(t *testing.T) (*fix, *state.Job, string) {
		f := newFix(t, fixBacklog)
		calls := 0
		register(t, fakeGate{name: "zz", applies: true, calls: &calls, run: func(c gate.Candidate) gate.Result {
			if calls == 1 { // 只有第一轮没过
				return gate.Result{Outcome: gate.Fail, Summary: "zz 没过", Files: []string{"pkg/a.go"}}
			}
			return gate.Result{Outcome: gate.Pass, Summary: "zz 过了"}
		}}, gate.LevelError)
		j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "pkg/b.go": "b"}, ready("agent/x"))
		res := f.run(j)
		if _, ran := resultOf(res, "scope"); ran || res.Outcome != "fail" {
			t.Fatalf("首轮不跑 scope 门：%+v", res)
		}
		if !slices.Contains(res.Notes, "scope 门不适用这次改动") {
			t.Errorf("首轮 scope 门不适用：%v", res.Notes)
		}
		f.collect(j, res)
		return f, j, filepath.Join(f.dir, ".claude", "worktrees", "x")
	}
	t.Run("只改范围内的照常过", func(t *testing.T) {
		f, j, wt := setup(t)
		f.commit(wt, map[string]string{"pkg/a.go": "a2", "pkg/a_test.go": "新测试"}, ready("agent/x"))
		j2 := f.nextRound(j)
		res := f.run(j2)
		r, ran := resultOf(res, "scope")
		if !ran || r.Outcome != gate.Pass || res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("范围内加记录文件照常过", func(t *testing.T) {
		f, j, wt := setup(t)
		f.commit(wt, map[string]string{"pkg/a.go": "a2", "CHANGELOG.md": "变更", "DECISIONS.md": "取舍"}, ready("agent/x"))
		j2 := f.nextRound(j)
		res := f.run(j2)
		r, ran := resultOf(res, "scope")
		if !ran || r.Outcome != gate.Pass || res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("改了范围外的判不过", func(t *testing.T) {
		f, j, wt := setup(t)
		f.commit(wt, map[string]string{"pkg/a.go": "a2", "pkg/b.go": "b2", "other/c.go": "c"}, ready("agent/x"))
		j2 := f.nextRound(j)
		before := f.main()
		res := f.run(j2)
		r, ran := resultOf(res, "scope")
		if !ran || r.Outcome != gate.Fail || r.Level != gate.LevelError || res.Outcome != "fail" || f.main() != before {
			t.Fatalf("%+v", res)
		}
		if want := []string{"other/c.go", "pkg/b.go"}; !slices.Equal(r.Files, want) {
			t.Errorf("Files %v，要 %v", r.Files, want)
		}
		ev := strings.Join(r.Evidence, "\n")
		if !strings.Contains(ev, "范围外：pkg/b.go") || !strings.Contains(ev, "范围外：other/c.go") || strings.Contains(ev, "范围外：pkg/a.go") {
			t.Errorf("证据：\n%s", ev)
		}
		if _, ran := resultOf(res, "zz"); ran {
			t.Error("scope 门挡下了，后面的门不跑")
		}
	})
	t.Run("合main带来的不算", func(t *testing.T) {
		f, j, wt := setup(t)
		f.commitMain(map[string]string{"main/only.go": "main 上的"})
		testkit.GitT(t, f.home, wt, "merge", "-q", "--no-edit", "main")
		f.commit(wt, map[string]string{"pkg/a.go": "a2"}, ready("agent/x"))
		j2 := f.nextRound(j)
		res := f.run(j2)
		r, ran := resultOf(res, "scope")
		if !ran || r.Outcome != gate.Pass || res.Outcome != "merged" {
			t.Fatalf("合 main 带来的文件不算越界：%+v", res)
		}
	})
	t.Run("项目可以把scope门调成warn", func(t *testing.T) {
		f, j, wt := setup(t)
		f.commitMain(map[string]string{gate.GatesFile: `{"levels":{"review":"off","scope":"warn"}}`})
		f.commit(wt, map[string]string{"other/c.go": "c"}, ready("agent/x"))
		j2 := f.nextRound(j)
		res := f.run(j2)
		if r, _ := resultOf(res, "scope"); r.Outcome != gate.Fail || r.Level != gate.LevelWarn || res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
}

// 到上限转用户：原因写成计划的问题，卡住标记、报告、通知都带这句。
func TestRoundLimitSaysPlanProblem(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.a.Cfg.NotifyEnabled = true
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.Gate.Round = gate.MaxRounds
	f.collect(j, gate.RunResult{Outcome: "fail", Reason: "测试没过", Results: []gate.Result{{Gate: "test", Outcome: gate.Fail, Summary: "退出码 1"}}})
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	want := fmt.Sprintf("退回 %d 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收", gate.MaxRounds)
	if !strings.Contains(j.Gate.Report, want) || !strings.Contains(j.Gate.Report, "测试没过") {
		t.Errorf("报告：%s", j.Gate.Report)
	}
	if !strings.Contains(j.StuckPending, "多半是计划(条目拆法)的问题") {
		t.Errorf("卡住标记：%s", j.StuckPending)
	}
	f.k.Runner.RetryStuckMarks(f.st)
	if b := f.backlog(); !strings.Contains(b, "多半是计划(条目拆法)的问题") {
		t.Errorf("BACKLOG：\n%s", b)
	}
	card := stuckCard(t, f)
	if card == nil || card.GateJob != j.ID {
		t.Fatalf("%+v", card)
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range s.Pending {
		if n.Key == "gate:"+j.ID && strings.Contains(strings.Join(n.Lines, "\n"), want) {
			found = true
		}
	}
	if !found {
		t.Errorf("通知没带计划的问题：%+v", s.Pending)
	}
}

// 补丁门点名它挡下的模块里改了的文件；测试门从日志里认出文件。
func TestGateFilesFromEvidence(t *testing.T) {
	t.Run("补丁门", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		patched(f)
		f.levels(`{"levels":{"review":"off","patch":"error"}}`)
		j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "package core\n\nvar A = 4\n", "other/b.go": "package other\n"}, fixReady("agent/x"))
		res := f.run(j)
		r, _ := resultOf(res, "patch")
		if r.Outcome != gate.Fail || !slices.Equal(r.Files, []string{"core/a.go"}) {
			t.Fatalf("%+v", r)
		}
		if !slices.Contains(res.Scope, "core/a.go") || slices.Contains(res.Scope, "other/b.go") {
			t.Errorf("范围：%v", res.Scope)
		}
	})
	t.Run("测试门", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.verifyOnMain("#!/bin/sh\necho '--- FAIL: TestA'\necho '    a_test.go:12: 不对'\necho 'File \"'\"$PWD\"'/py/x.py\", line 3'\necho 'nothere.go:1: 不是仓库里的'\necho 'a.go:3: 只有一个'\nexit 1\n")
		j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "pkg/a_test.go": "t", "py/x.py": "x", "other/a_test.go": "o"}, ready("agent/x"))
		res := f.run(j)
		r := testResult(t, res)
		if r.Outcome != gate.Fail {
			t.Fatalf("%+v", r)
		}
		// a_test.go 有两个同名的：都是这次改的，认不出是哪个，不硬凑；a.go 只有一个，认得出
		if want := []string{"py/x.py", "pkg/a.go"}; !slices.Equal(r.Files, want) {
			t.Errorf("Files %v，要 %v", r.Files, want)
		}
	})
}

// 测试门的日志只点名测试文件：范围带上同目录这一轮改过的代码，下一轮修代码能过 scope 门。
func TestScopeTestOnlyFailureIncludesCode(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\nif grep -q fixed pkg/a.go; then exit 0; fi\necho '--- FAIL: TestA'\necho '    a_test.go:12: got 1 want 2'\nexit 1\n")
	j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "pkg/a_test.go": "t", "other/c.go": "c"}, ready("agent/x"))
	res := f.run(j)
	if r := testResult(t, res); !slices.Equal(r.Files, []string{"pkg/a_test.go"}) {
		t.Fatalf("测试门只认出测试文件：%+v", r)
	}
	if want := []string{"pkg/a.go", "pkg/a_test.go"}; !slices.Equal(res.Scope, want) {
		t.Fatalf("范围 %v，要 %v", res.Scope, want)
	}
	f.collect(j, res)
	f.commit(filepath.Join(f.dir, ".claude", "worktrees", "x"), map[string]string{"pkg/a.go": "fixed"}, ready("agent/x"))
	j2 := f.nextRound(j)
	res2 := f.run(j2)
	if r, ran := resultOf(res2, "scope"); !ran || r.Outcome != gate.Pass || res2.Outcome != "merged" {
		t.Fatalf("修被测的代码不算越界：%+v", res2)
	}
}

// 测试在根目录 tests/、代码在别处、这一轮还改了根目录的 CHANGELOG.md：记录文件不算对上了被测代码，兜底用这一轮改的全部文件。
func TestScopeTestDirFallbackIgnoresRecordFiles(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.verifyOnMain("#!/bin/sh\nif grep -q fixed internal/x/a.go; then exit 0; fi\necho '    x_test.go:3: got 1 want 2'\nexit 1\n")
	j := f.claim("做事", "agent/x", map[string]string{"tests/x_test.go": "t", "internal/x/a.go": "a", "CHANGELOG.md": "变更"}, ready("agent/x"))
	res := f.run(j)
	if r := testResult(t, res); !slices.Equal(r.Files, []string{"tests/x_test.go"}) {
		t.Fatalf("测试门只认出测试文件：%+v", r)
	}
	if !slices.Contains(res.Scope, "internal/x/a.go") {
		t.Fatalf("范围要带上这一轮改的代码：%v", res.Scope)
	}
	f.collect(j, res)
	f.commit(filepath.Join(f.dir, ".claude", "worktrees", "x"), map[string]string{"internal/x/a.go": "fixed"}, ready("agent/x"))
	j2 := f.nextRound(j)
	res2 := f.run(j2)
	if r, ran := resultOf(res2, "scope"); !ran || r.Outcome != gate.Pass || res2.Outcome != "merged" {
		t.Fatalf("修被测的代码不算越界：%+v", res2)
	}
}

// 只收挡住合并的门点名的文件：warn 档没过的不收窄范围。
func TestScopeIgnoresWarnGateFiles(t *testing.T) {
	f := newFix(t, fixBacklog)
	register(t, fakeGate{name: "ww", applies: true, run: func(c gate.Candidate) gate.Result {
		return gate.Result{Outcome: gate.Fail, Summary: "warn 没过", Files: []string{"pkg/b.go"}}
	}}, gate.LevelWarn)
	failOn(t, "zz")
	j := f.claim("做事", "agent/x", map[string]string{"pkg/a.go": "a", "pkg/b.go": "b"}, ready("agent/x"))
	res := f.run(j)
	if want := []string{"pkg/a.go", "pkg/b.go"}; res.Outcome != "fail" || !slices.Equal(res.Scope, want) {
		t.Fatalf("范围 %v，要 %v（%s）", res.Scope, want, res.Outcome)
	}
}

// 范围的折叠和判定。
func TestScopeFoldAndMatch(t *testing.T) {
	var many []string
	for i := range 40 {
		many = append(many, fmt.Sprintf("d%d/f.go", i%5), fmt.Sprintf("d%d/g%d.go", i%5, i))
	}
	got := gate.FoldScope(many)
	if want := []string{"d0/", "d1/", "d2/", "d3/", "d4/"}; !slices.Equal(got, want) {
		t.Errorf("按目录折叠：%v", got)
	}
	var wide []string
	for i := range 40 {
		wide = append(wide, fmt.Sprintf("top%d/x/f.go", i))
	}
	if got := gate.FoldScope(wide); got != nil {
		t.Errorf("折叠了还多就不限定：%v", got)
	}
	scope := []string{"pkg/a.go", "web/"}
	for f, want := range map[string]bool{
		"pkg/a.go": true, "pkg/a_test.go": true, "pkg/new_test.go": true, "pkg/b.go": false, "pkg/sub/a_test.go": false,
		"web/x/y.ts": true, gate.ReadyFile: true, "other.go": false, "CHANGELOG.md": true, "docs/BACKLOG.md": true, "sub/LESSONS.md": true, "docs/other.md": false,
	} {
		if gate.InScope(f, scope) != want {
			t.Errorf("%s 在不在范围：要 %v", f, want)
		}
	}
}
