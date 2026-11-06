package gate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

const fixBacklog = "# 待办\n\n## 待做\n\n- [ ] 做事 —— 要什么；验收：看\n- [ ] 下一条 —— 后面的活\n\n## 等用户决定\n\n## 已完成\n\n- [x] 旧事 —— 合并提交 abc1234\n"

type fix struct {
	t    *testing.T
	def  string // 项目的默认分支
	a    *gate.Service
	k    *stack.Stack
	home string
	dir  string
	p    backlog.Project
	out  *bytes.Buffer
	st   *state.State
}

func newFix(t *testing.T, body string) *fix {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
	os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
	var out bytes.Buffer
	e, err := env.New(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	e.Cfg.ProjectsRoot = root
	// 默认后端是假的：用例忘了指定后端时不能起真的 CLI。起不来就是判不了。
	e.Cfg.Backend = testkit.RegisterFake(t, testkit.FakeOpts{Caps: testkit.AllCaps(), Path: "/usr/bin/false"})
	k := stack.New(e)
	dir := filepath.Join(root, "demo")
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(body), 0o644)
	os.WriteFile(filepath.Join(dir, "PROGRESS.md"), []byte("# 进度\n\n## 当前任务\n无\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, gate.VerifyScript), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(dir, gate.GatesFile), []byte(`{"levels":{"review":"off"}}`), 0o644) // 审查门的用例自己打开
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	return &fix{t: t, def: "main", a: k.Gate, k: k, home: home, dir: dir, p: backlog.Project{Name: "demo", Dir: dir}, out: &out, st: &state.State{}}
}

func (f *fix) git(args ...string) string {
	f.t.Helper()
	return testkit.GitT(f.t, f.home, f.dir, args...)
}

func (f *fix) main() string { return f.git("rev-parse", "refs/heads/main") }

func (f *fix) backlog() string { return testkit.ReadFile(f.t, f.p.BacklogPath()) }

// claim 像 agent-task 那样认领第一条，从 main 开分支和 worktree，在分支上提交 files 和 ready.json。
func (f *fix) claim(title, branch string, files map[string]string, r gate.Ready) *state.Job {
	f.t.Helper()
	items, err := f.p.Items()
	if err != nil {
		f.t.Fatal(err)
	}
	var it backlog.Item
	for _, x := range items {
		if x.Title == title {
			it = x
		}
	}
	if it.Title == "" {
		f.t.Fatalf("没有条目 %s", title)
	}
	err = f.a.Backlog.CommitBacklog(f.p, "待办：认领", func(old string) (string, error) {
		text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, func(l string) string { return l + "（进行中：" + branch + " 2026-11-05 10:00）" })
		return text, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	wt := filepath.Join(f.dir, ".claude", "worktrees", filepath.Base(branch))
	f.git("worktree", "add", "-q", wt, "-b", branch, f.def)
	f.commit(wt, files, r)
	j := &state.Job{ID: "1105-1000" + fmt.Sprint(10+len(f.st.Jobs)) + "-demo", Project: "demo", ProjectDir: f.dir, DefaultBranch: f.def, Title: title, Item: it.Text,
		Fingerprint: it.Fingerprint, Status: state.StatusDone, SessionID: "s1", StartedAt: f.a.Now()}
	now := f.a.Now()
	j.EndedAt = &now
	os.MkdirAll(f.a.JobDir(j.ID), 0o755)
	f.st.Jobs = append(f.st.Jobs, j)
	if r.Type == "" { // 用例接着在分支上提交，最后自己写 ready.json
		f.a.Enqueue(j, ready(branch), "")
		return j
	}
	got, head, found, err := f.a.ReadReady(f.dir, branch)
	if err != nil || !found {
		f.t.Fatalf("ReadReady：%v %v", found, err)
	}
	f.a.Enqueue(j, got, head)
	return j
}

func (f *fix) commit(wt string, files map[string]string, r gate.Ready) {
	f.t.Helper()
	for name, text := range files {
		p := filepath.Join(wt, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	if r.Type != "" {
		os.MkdirAll(filepath.Join(wt, ".agentq"), 0o755)
		b, _ := json.Marshal(r)
		os.WriteFile(filepath.Join(wt, gate.ReadyFile), b, 0o644)
	}
	testkit.GitT(f.t, f.home, wt, "add", "-A")
	testkit.GitT(f.t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "做事")
}

func ready(branch string) gate.Ready {
	return gate.Ready{Branch: branch, Type: "feature", Acceptance: "看 a.txt", Progress: "做好了"}
}

// run 在进程内走一次门进程。
func (f *fix) run(j *state.Job) gate.RunResult {
	f.t.Helper()
	dir := f.a.Dir(j.ID)
	os.MkdirAll(dir, 0o700)
	return f.a.Process(context.Background(), dir, gate.PlanOf(f.a, j), time.Minute)
}

// collect 把这次结果交给 tick 那边：和门进程写完结果、tick 收结果一样。
func (f *fix) collect(j *state.Job, res gate.RunResult) {
	f.t.Helper()
	if err := gate.WriteResult(f.a.Dir(j.ID), res); err != nil {
		f.t.Fatal(err)
	}
	j.Gate.State = state.GateRunning
	f.a.Collect(f.st)
}

// fakeGate 是登记给测试的门。
type fakeGate struct {
	name    string
	outcome gate.Outcome
	applies bool
	calls   *int
	run     func(c gate.Candidate) gate.Result
}

func (g fakeGate) Name() string                    { return g.name }
func (g fakeGate) AppliesTo(c gate.Candidate) bool { return g.applies }
func (g fakeGate) Run(ctx context.Context, c gate.Candidate) gate.Result {
	if g.calls != nil {
		*g.calls++
	}
	if g.run != nil {
		return g.run(c)
	}
	return gate.Result{Outcome: g.outcome, Summary: g.name + " 说 " + string(g.outcome)}
}

func register(t *testing.T, g fakeGate, def gate.Level) {
	t.Helper()
	t.Cleanup(gate.Register(g.name, def, func(gate.Deps) gate.Gate { return g }))
}

func resultOf(res gate.RunResult, name string) (gate.Result, bool) {
	for _, r := range res.Results {
		if r.Gate == name {
			return r, true
		}
	}
	return gate.Result{}, false
}

// 三档 × 三态：error 档只有 pass 放行，warn 档都放行只记录，off 档不跑。
func TestLevelsAndOutcomes(t *testing.T) {
	cases := []struct {
		lvl     gate.Level
		outcome gate.Outcome
		want    string
		ran     bool
	}{
		{gate.LevelError, gate.Pass, "merged", true},
		{gate.LevelError, gate.Fail, "fail", true},
		{gate.LevelError, gate.Unknown, "unknown", true},
		{gate.LevelWarn, gate.Pass, "merged", true},
		{gate.LevelWarn, gate.Fail, "merged", true},
		{gate.LevelWarn, gate.Unknown, "merged", true},
		{gate.LevelOff, gate.Fail, "merged", false},
		{gate.LevelOff, gate.Unknown, "merged", false},
	}
	for _, c := range cases {
		t.Run(string(c.lvl)+"-"+string(c.outcome), func(t *testing.T) {
			f := newFix(t, fixBacklog)
			calls := 0
			register(t, fakeGate{name: "假门", outcome: c.outcome, applies: true, calls: &calls}, c.lvl)
			before := f.main()
			j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
			res := f.run(j)
			if res.Outcome != c.want {
				t.Fatalf("%+v，要 %s", res, c.want)
			}
			if (calls > 0) != c.ran {
				t.Errorf("跑了 %d 次，要 %v", calls, c.ran)
			}
			merged := f.main() != before && strings.Contains(f.backlog(), "- [x] 做事 —— 要什么")
			if merged != (c.want == "merged") {
				t.Errorf("合并了没有不对：%v\n%s", merged, f.backlog())
			}
			if r, ok := resultOf(res, "假门"); c.ran != ok || (ok && r.Level != c.lvl) {
				t.Errorf("结果要记档位和门名：%+v", res.Results)
			}
		})
	}
}

// 不适用的门不跑；error 档挡住后面的门不再跑（审查要花额度）；warn 档不挡。
func TestGateOrderAndApplies(t *testing.T) {
	f := newFix(t, fixBacklog)
	first, last, skipped := 0, 0, 0
	register(t, fakeGate{name: "不适用", outcome: gate.Fail, applies: false, calls: &skipped}, gate.LevelError)
	register(t, fakeGate{name: "第一道", outcome: gate.Fail, applies: true, calls: &first}, gate.LevelError)
	register(t, fakeGate{name: "第二道", outcome: gate.Pass, applies: true, calls: &last}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "fail" || skipped != 0 || first != 1 || last != 0 {
		t.Fatalf("%+v %d %d %d", res, skipped, first, last)
	}
}

// 候选的内容：合了最新 main 和任务改动，ready.json 不在里面，改动文件、tree、patch-id 都给门。
func TestCandidateContents(t *testing.T) {
	f := newFix(t, fixBacklog)
	var got gate.Candidate
	readyInTree := false
	register(t, fakeGate{name: "看候选", applies: true, run: func(c gate.Candidate) gate.Result {
		got = c
		_, err := os.Stat(filepath.Join(c.Dir, gate.ReadyFile))
		readyInTree = err == nil
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if got.Branch != "agent/x" || got.Type != "feature" || got.Ready.Acceptance != "看 a.txt" || got.Tree == "" || got.PatchID == "" || got.JobID != j.ID {
		t.Errorf("%+v", got)
	}
	if len(got.Files) != 1 || got.Files[0] != "new.txt" {
		t.Errorf("改动文件：%v", got.Files)
	}
	if readyInTree {
		t.Error("ready.json 不该在候选里")
	}
	if _, err := os.Stat(filepath.Join(f.dir, gate.ReadyFile)); err == nil {
		t.Error("ready.json 进了 main")
	}
	if sub := f.git("log", "-1", "--format=%P", "refs/heads/main~1"); sub == "" {
		t.Error("main 上要有合并提交")
	}
}

// main 中途动过：重建候选，门重跑（审查门靠 patch-id 沿用，见 G3），用新 main 合并。同一 patch-id、不同 tree。
func TestMainMovedRebuilds(t *testing.T) {
	f := newFix(t, fixBacklog)
	var seen []gate.Candidate
	register(t, fakeGate{name: "看候选", applies: true, run: func(c gate.Candidate) gate.Result {
		seen = append(seen, c)
		if len(seen) == 1 { // 门跑到一半，别人往 main 提交
			os.WriteFile(filepath.Join(f.dir, "other.txt"), []byte("别人的"), 0o644)
			f.git("add", "other.txt")
			f.git("commit", "-q", "-m", "别人的提交")
		}
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" || len(seen) != 2 {
		t.Fatalf("%+v，跑了 %d 次", res, len(seen))
	}
	if seen[0].Tree == seen[1].Tree || seen[0].Base == seen[1].Base {
		t.Errorf("第二次要用新 main：%s %s", seen[0].Tree, seen[1].Tree)
	}
	if seen[0].PatchID != seen[1].PatchID || seen[0].PatchID == "" {
		t.Errorf("任务改动没变，patch-id 要一样：%q %q", seen[0].PatchID, seen[1].PatchID)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "new.txt")); err != nil {
		t.Error("任务改动没进主目录")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "other.txt")); err != nil {
		t.Error("别人的提交丢了")
	}
}

// main 一直动：不死循环，排回队尾。
func TestMainKeepsMoving(t *testing.T) {
	f := newFix(t, fixBacklog)
	n := 0
	register(t, fakeGate{name: "捣乱", applies: true, run: func(c gate.Candidate) gate.Result {
		n++
		os.WriteFile(filepath.Join(f.dir, fmt.Sprint("动", n)), []byte("x"), 0o644)
		f.git("add", "-A", "--", fmt.Sprint("动", n))
		f.git("commit", "-q", "-m", fmt.Sprint("动 ", n))
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "wait" || n != 3 {
		t.Fatalf("%+v，跑了 %d 次", res, n)
	}
}

// 和最新 main 冲突算失败，报告写清文件，退回任务。
func TestConflictFails(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"same.txt": "分支的"}, ready("agent/x"))
	os.WriteFile(filepath.Join(f.dir, "same.txt"), []byte("main 的"), 0o644)
	f.git("add", "same.txt")
	f.git("commit", "-q", "-m", "main 也改")
	before := f.main()
	res := f.run(j)
	if res.Outcome != "fail" || !strings.Contains(res.Reason, "same.txt") {
		t.Fatalf("%+v", res)
	}
	if f.main() != before {
		t.Error("冲突不能进 main")
	}
	if n := strings.Count(f.git("worktree", "list", "--porcelain"), "worktree "); n != 2 {
		t.Errorf("门的 worktree 要清掉（主目录 + 任务的）：%d", n)
	}
}

// 主目录有和这次改动重叠的未提交改动：不覆盖，等下次 tick；没重叠的改动不碰，照常合并。
func TestMainDirOverlap(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"same.txt": "分支的"}, ready("agent/x"))
	os.WriteFile(filepath.Join(f.dir, "same.txt"), []byte("用户没提交的"), 0o644)
	os.WriteFile(filepath.Join(f.dir, "unrelated.txt"), []byte("无关的"), 0o644)
	before := f.main()
	res := f.run(j)
	if res.Outcome != "wait" || f.main() != before {
		t.Fatalf("%+v", res)
	}
	if got := testkit.ReadFile(t, filepath.Join(f.dir, "same.txt")); got != "用户没提交的" {
		t.Errorf("用户的改动被盖了：%s", got)
	}
	// 用户清掉重叠的：下次合并，无关的改动还在
	os.Remove(filepath.Join(f.dir, "same.txt"))
	res = f.run(j)
	if res.Outcome != "merged" || testkit.ReadFile(t, filepath.Join(f.dir, "same.txt")) != "分支的" {
		t.Fatalf("%+v", res)
	}
	if got := testkit.ReadFile(t, filepath.Join(f.dir, "unrelated.txt")); got != "无关的" {
		t.Errorf("无关的改动不能动：%s", got)
	}
}

// 门都过了但主目录挡着时，下次不再跑门：结论按 tree 留着。tree 变了才重跑。
func TestPassedReusedOnSameTree(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "贵的门", outcome: gate.Pass, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"same.txt": "分支的"}, ready("agent/x"))
	os.WriteFile(filepath.Join(f.dir, "same.txt"), []byte("用户没提交的"), 0o644)
	if res := f.run(j); res.Outcome != "wait" || calls != 1 {
		t.Fatalf("%+v %d", res, calls)
	}
	if res := f.run(j); res.Outcome != "wait" || calls != 1 {
		t.Fatalf("同一个 tree 不该再跑：%+v %d", res, calls)
	}
	os.Remove(filepath.Join(f.dir, "same.txt"))
	f.git("commit", "-q", "--allow-empty", "-m", "main 动了，但 tree 没变")
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("%+v %d", res, calls)
	}
}

// 主目录不在 main：不切它的分支，等着。
func TestMainDirOnOtherBranch(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	f.git("checkout", "-q", "-b", "用户的分支")
	before := f.main()
	res := f.run(j)
	if res.Outcome != "wait" || f.main() != before || !strings.Contains(res.Reason, "不在 main") {
		t.Fatalf("%+v", res)
	}
	if got := f.git("symbolic-ref", "--short", "HEAD"); got != "用户的分支" {
		t.Errorf("不能切分支：%s", got)
	}
}

// 门进程合并后、写结果前被杀：重跑不再合并，只补收尾。
func TestAlreadyMergedOnlyWraps(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "门", outcome: gate.Pass, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	head := j.Gate.Head
	// 模拟：合并了，收尾没写（用户在 BACKLOG 上有没提交的改动）
	orig := f.backlog()
	os.WriteFile(f.p.BacklogPath(), []byte(orig+"用户在写\n"), 0o644)
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr == "" {
		t.Fatalf("%+v", res)
	}
	merge := f.main()
	os.WriteFile(f.p.BacklogPath(), []byte(orig), 0o644)
	calls = 0
	res = f.run(j)
	if res.Outcome != "merged" || res.Merge != merge || calls != 0 {
		t.Fatalf("重跑不该再跑门：%+v %d", res, calls)
	}
	if got := f.git("rev-list", "--count", "--merges", head+"..refs/heads/main"); got != "1" {
		t.Errorf("只能合并一次：%s", got)
	}
	if b := f.backlog(); !strings.Contains(b, "- [x] 做事 —— 要什么；验收：看；合并提交 "+merge[:12]+"；验收：看 a.txt") || strings.Contains(b, "（进行中") {
		t.Errorf("%s", b)
	}
	// 再来一次也不重复写，PROGRESS 也不留下没提交的改动
	before := f.main()
	defer func() {
		if got := f.git("status", "--porcelain", "--", "PROGRESS.md", "docs/BACKLOG.md"); got != "" {
			t.Errorf("%s", got)
		}
	}()
	if res := f.run(j); res.Outcome != "merged" || f.main() != before || strings.Count(f.backlog(), "- [x] 做事") != 1 {
		t.Fatalf("%+v\n%s", res, f.backlog())
	}
}

// 引擎写收尾：BACKLOG、PROGRESS 在一个提交里，分支和任务 worktree 删掉，下一条还在待做。
func TestWrapUp(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	b := f.backlog()
	short := res.Merge[:12]
	if !strings.Contains(b, "- [x] 做事 —— 要什么；验收：看；合并提交 "+short+"；验收：看 a.txt\n") || strings.Contains(b, "- [ ] 做事") || !strings.Contains(b, "- [ ] 下一条") {
		t.Errorf("%s", b)
	}
	if i, k := strings.Index(b, "- [x] 做事"), strings.Index(b, "- [x] 旧事"); i < 0 || i > k {
		t.Errorf("新的在前面：%s", b)
	}
	p := testkit.ReadFile(t, filepath.Join(f.dir, "PROGRESS.md"))
	if !strings.Contains(p, "## 最近合并") || !strings.Contains(p, "做好了（"+short+"）") || !strings.Contains(p, "## 当前任务\n无") {
		t.Errorf("%s", p)
	}
	if got := f.git("status", "--porcelain", "--", "docs/BACKLOG.md", "PROGRESS.md"); got != "" {
		t.Errorf("都要提交：%s", got)
	}
	if got := f.git("log", "-1", "--name-only", "--format=", "refs/heads/main"); !strings.Contains(got, "docs/BACKLOG.md") || !strings.Contains(got, "PROGRESS.md") {
		t.Errorf("一个提交：%s", got)
	}
	if got := f.git("branch", "--list", "agent/x"); got != "" {
		t.Errorf("分支要删：%s", got)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("任务 worktree 要删：%v", err)
	}
}

// 任务 worktree 里有没提交的东西：不强删，worktree 和分支都留着，合并照常算。
func TestWrapUpKeepsDirtyWorktree(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("没提交"), 0o644)
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	if got := testkit.ReadFile(t, filepath.Join(wt, "scratch.txt")); got != "没提交" {
		t.Errorf("%s", got)
	}
	if got := f.git("branch", "--list", "agent/x"); !strings.Contains(got, "agent/x") {
		t.Error("分支要留着")
	}
}

// PROGRESS 有别人没提交的改动、没有这个文件：不写它，也不挡合并。
func TestProgressNotWritable(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	os.WriteFile(filepath.Join(f.dir, "PROGRESS.md"), []byte("别人在写\n"), 0o644)
	if res := f.run(j); res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	if got := testkit.ReadFile(t, filepath.Join(f.dir, "PROGRESS.md")); got != "别人在写\n" {
		t.Errorf("%s", got)
	}
	g := newFix(t, fixBacklog)
	g.git("rm", "-q", "PROGRESS.md")
	g.git("commit", "-q", "-m", "不要 PROGRESS")
	j2 := g.claim("做事", "agent/x", map[string]string{"new.txt": "新"}, ready("agent/x"))
	if res := g.run(j2); res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "PROGRESS.md")); err == nil {
		t.Error("没有就不建")
	}
}

func TestNoteProgressKeepsLast(t *testing.T) {
	text := "# 进度\n\n## 当前任务\n无\n"
	for i := 0; i < 12; i++ {
		text = gate.NoteProgress(text, fmt.Sprintf("2026-11-05 第 %d 条（c%d）", i, i), fmt.Sprintf("c%d", i))
	}
	if n := strings.Count(text, "\n- 2026"); n != 8 {
		t.Errorf("只留 8 条：%d\n%s", n, text)
	}
	if !strings.Contains(text, "第 11 条") || strings.Contains(text, "第 3 条") || !strings.Contains(text, "## 当前任务\n无") {
		t.Errorf("%s", text)
	}
	if again := gate.NoteProgress(text, "2026-11-05 第 11 条（c11）", "c11"); again != text {
		t.Errorf("同一提交不重复写")
	}
}

// .agentq/gates.json 读 main 上的：任务在分支上把门关掉不算。写坏了按默认档位，不放宽。
func TestGatesFile(t *testing.T) {
	gatesOn := func(t *testing.T, f *fix, g fakeGate) {
		register(t, g, gate.LevelError)
	}
	t.Run("任务改门配置无效", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		calls := 0
		gatesOn(t, f, fakeGate{name: "门", outcome: gate.Fail, applies: true, calls: &calls})
		j := f.claim("做事", "agent/x", map[string]string{gate.GatesFile: `{"levels":{"门":"off"}}`}, ready("agent/x"))
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		testkit.GitT(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", gate.NoteMark+"想关门") // 过了测试门才看得到关没关成
		if res := f.run(j); res.Outcome != "fail" || calls != 1 {
			t.Fatalf("%+v %d", res, calls)
		}
	})
	t.Run("main 上的管用", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		calls := 0
		gatesOn(t, f, fakeGate{name: "门", outcome: gate.Fail, applies: true, calls: &calls})
		os.MkdirAll(filepath.Join(f.dir, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(`{"levels":{"门":"off","review":"off"}}`), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "关门")
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
		if res := f.run(j); res.Outcome != "merged" || calls != 0 {
			t.Fatalf("%+v %d", res, calls)
		}
	})
	t.Run("warn 只记录", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		gatesOn(t, f, fakeGate{name: "门", outcome: gate.Fail, applies: true})
		os.MkdirAll(filepath.Join(f.dir, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(`{"levels":{"门":"warn","别的门":"off","review":"off"}}`), 0o644)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "warn")
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
		res := f.run(j)
		r, _ := resultOf(res, "门")
		if res.Outcome != "merged" || r.Outcome != gate.Fail || !strings.Contains(strings.Join(res.Notes, "；"), "不认识的门 别的门") {
			t.Fatalf("%+v", res)
		}
	})
	for name, raw := range map[string]string{"不是 JSON": "{坏了", "档位写错": `{"levels":{"门":"关"}}`} {
		t.Run("写坏了："+name, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			gatesOn(t, f, fakeGate{name: "门", outcome: gate.Fail, applies: true})
			os.MkdirAll(filepath.Join(f.dir, ".agentq"), 0o755)
			os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(raw), 0o644)
			f.git("add", "-A")
			f.git("commit", "-q", "-m", "坏的")
			j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
			res := f.run(j)
			if res.Outcome == "merged" { // 默认档位下审查门开着，这里没配后端：判不了，也不放行
				t.Fatalf("写坏了不能放宽：%+v", res)
			}
		})
	}
}

// ready.json 的校验：没写、写坏、字段不全、分支对不上。
func TestReadReady(t *testing.T) {
	f := newFix(t, fixBacklog)
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	f.git("worktree", "add", "-q", wt, "-b", "agent/x", "main")
	if _, _, found, err := f.a.ReadReady(f.dir, "agent/x"); found || err != nil {
		t.Errorf("没写：%v %v", found, err)
	}
	if _, _, found, err := f.a.ReadReady(f.dir, "没有这个分支"); found || err != nil {
		t.Errorf("分支不在：%v %v", found, err)
	}
	if _, _, found, err := f.a.ReadReady(f.dir, "-x"); found || err != nil {
		t.Errorf("分支名不能被当选项：%v %v", found, err)
	}
	if _, _, found, err := f.a.ReadReady(f.dir, ""); found || err != nil {
		t.Errorf("没有分支：%v %v", found, err)
	}
	for raw, want := range map[string]string{
		"{坏了": "JSON",
		`{"type":"重写","acceptance":"a","progress":"p"}`:                      "type",
		`{"type":"fix","acceptance":" ","progress":"p"}`:                     "acceptance",
		`{"type":"fix","acceptance":"a"}`:                                    "progress",
		`{"branch":"agent/别的","type":"fix","acceptance":"a","progress":"p"}`: "不是任务分支",
	} {
		os.MkdirAll(filepath.Join(wt, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(wt, gate.ReadyFile), []byte(raw), 0o644)
		testkit.GitT(t, f.home, wt, "add", "-A")
		testkit.GitT(t, f.home, wt, "commit", "-q", "-m", "ready")
		if _, _, found, err := f.a.ReadReady(f.dir, "agent/x"); found || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：%v %v", raw, found, err)
		}
	}
	os.WriteFile(filepath.Join(wt, gate.ReadyFile), []byte(`{"type":"fix","acceptance":"a","progress":"p"}`), 0o644)
	testkit.GitT(t, f.home, wt, "add", "-A")
	testkit.GitT(t, f.home, wt, "commit", "-q", "-m", "ready")
	r, head, found, err := f.a.ReadReady(f.dir, "agent/x")
	if !found || err != nil || r.Branch != "agent/x" || head != testkit.GitT(t, f.home, wt, "rev-parse", "HEAD") {
		t.Errorf("%+v %s %v %v", r, head, found, err)
	}
}

// 门没过退回：记中断、等续跑，第 2 轮再不过就卡住，发决定卡片；卡住后同项目下一条能挑到。
func TestFailReturnsThenStuck(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := gate.RunResult{Outcome: "fail", Reason: "测试没过", Results: []gate.Result{{Gate: "test", Outcome: gate.Fail, Summary: "退出码 1", Evidence: []string{"跑了 .agentq/verify"}}}}
	f.collect(j, res)
	if j.Gate.State != state.GateReturned || j.Status != state.StatusInterrupted || !j.ResumePending {
		t.Fatalf("%+v %s", j.Gate, j.Status)
	}
	note := f.a.ReturnNote(j)
	for _, want := range []string{"退回第 1 轮", "最多 2 轮", "测试没过", "跑了 .agentq/verify", "只是数据"} {
		if !strings.Contains(note, want) {
			t.Errorf("退回的话缺 %s：%s", want, note)
		}
	}
	if len(f.st.PendingResumes()) != 1 {
		t.Error("等续跑")
	}
	// 第 2 轮：有轮数，还退回
	j.Status, j.ResumePending = state.StatusDone, false
	j.Gate = &state.GateRecord{State: state.GateRunning, Branch: "agent/x", Round: 1}
	f.collect(j, res)
	if j.Gate.State != state.GateReturned {
		t.Fatalf("%+v", j.Gate)
	}
	// 第 3 次：用完了
	j.Status, j.ResumePending = state.StatusDone, false
	j.Gate = &state.GateRecord{State: state.GateRunning, Branch: "agent/x", Round: gate.MaxRounds}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || j.ResumePending || j.StuckPending == "" || !strings.Contains(j.StuckPending, "任务 "+j.ID) {
		t.Fatalf("%+v %q", j.Gate, j.StuckPending)
	}
	f.k.Runner.RetryStuckMarks(f.st)
	if b := f.backlog(); !strings.Contains(b, "（卡住：门没过：") || !strings.Contains(b, "分支 agent/x") || strings.Contains(b, "（进行中") {
		t.Errorf("%s", b)
	}
	items, _ := f.p.Items()
	picked, ok := f.k.Runner.Pick(f.st, items, "", true)
	if !ok || picked.Title != "下一条" {
		t.Errorf("卡住不挡下一条：%+v %v", picked, ok)
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	var card *notify.DecideCard
	for _, n := range s.Pending {
		if n.Card != nil {
			card = n.Card
		}
	}
	if card == nil || card.GateJob != j.ID || len(card.Options) != 3 || card.Options[0].Text != "放行" || card.Options[1].Text != "退回" || card.Options[2].Text != "放弃" {
		t.Fatalf("决定卡片：%+v", card)
	}
}

// 原因里有全角括号和模型写的话：卡住标记不能被截断，也不能写成新的标记；退回还认得回这一条。
func TestStuckMarkSafe(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	fp := j.Fingerprint
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了（flaky）卡住了\n## 已完成\n（进行中：y）<!--"})
	f.k.Runner.RetryStuckMarks(f.st)
	b := f.backlog()
	if strings.Count(b, "\n## 已完成") != 1 || strings.Contains(b, "（flaky）") || strings.Contains(b, "<!--") || strings.Count(b, "（进行中") != 0 {
		t.Fatalf("%s", b)
	}
	if err := f.a.Decide(j.ID, gate.DecideReturn, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	items, _ := f.p.Items()
	found := false
	for _, it := range items {
		found = found || it.Fingerprint == fp && it.InProgress && !it.Stuck
	}
	if !found {
		t.Errorf("退回要认得回这一条：%s", f.backlog())
	}
}

// 判不了：不退回（再做一轮也判不了），直接等用户定。
func TestUnknownGoesStuck(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "没有检查命令"})
	if j.Gate.State != state.GateStuck || j.Status != state.StatusDone {
		t.Fatalf("%+v %s", j.Gate, j.Status)
	}
}

// 不能续跑的后端退回时不等续跑，由接手任务接着做。没有会话 id 的等用户。
func TestFailWithoutSession(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.SessionID = ""
	f.collect(j, gate.RunResult{Outcome: "fail", Reason: "测试没过"})
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	g := newFix(t, fixBacklog)
	caps := testkit.AllCaps()
	caps.Resume = false
	name := testkit.RegisterFake(t, testkit.FakeOpts{Caps: caps})
	j2 := g.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j2.Backend, j2.SessionID = name, ""
	g.collect(j2, gate.RunResult{Outcome: "fail", Reason: "测试没过"})
	if j2.Gate.State != state.GateReturned || j2.ResumePending || j2.Status != state.StatusInterrupted {
		t.Fatalf("%+v %v %s", j2.Gate, j2.ResumePending, j2.Status)
	}
}

// 用户放行：下次 tick 排队，不跑门，合并，写审计行。放行要写原因。
func TestPassDecision(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "门", outcome: gate.Fail, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	if err := f.a.CmdGate([]string{"pass", j.ID}); err == nil || !strings.Contains(err.Error(), "原因") {
		t.Errorf("没写原因：%v", err)
	}
	if err := f.a.CmdGate([]string{"pass", j.ID, "--reason", "  "}); err == nil {
		t.Error("空原因不行")
	}
	if err := f.a.CmdGate([]string{"pass", j.ID, "--reason", "我看过了"}); err != nil {
		t.Fatal(err)
	}
	if j.Gate.State != state.GateStuck {
		t.Error("命令只记决定，tick 才落")
	}
	f.a.Collect(f.st)
	if j.Gate.State != state.GateQueued || j.Gate.Override != "我看过了" || j.StuckPending != "" {
		t.Fatalf("%+v", j.Gate)
	}
	res := f.run(j)
	if res.Outcome != "merged" || calls != 0 || res.Results[0].Gate != "override" {
		t.Fatalf("%+v %d", res, calls)
	}
	if b := f.backlog(); !strings.Contains(b, "- [x] 做事 —— 要什么") || strings.Contains(b, "- [ ] 做事") {
		t.Errorf("卡住标记的条目也要移走：%s", b)
	}
	audit := testkit.ReadFile(t, filepath.Join(f.a.DataDir(), "gate", "audit.log"))
	if !strings.Contains(audit, "放行") || !strings.Contains(audit, "我看过了") || !strings.Contains(audit, j.ID) {
		t.Errorf("%s", audit)
	}
	// 不是卡着的不能放行
	if err := f.a.CmdGate([]string{"pass", "没有这个任务", "--reason", "x"}); err == nil {
		t.Error("没有的任务")
	}
}

// 退回：卡住标记换回进行中，等续跑；放弃：标记留着，不再管，之后还能放行。
func TestReturnAndAbandonDecision(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	if !strings.Contains(f.backlog(), "（卡住：门没过") {
		t.Fatal(f.backlog())
	}
	if err := f.a.Decide(j.ID, gate.DecideAbandon, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.State != state.GateAbandoned || !strings.Contains(f.backlog(), "（卡住：门没过") {
		t.Fatalf("%+v", j.Gate)
	}
	if err := f.a.Decide(j.ID, gate.DecideReturn, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	b := f.backlog()
	if j.Gate.State != state.GateReturned || !j.ResumePending || j.Status != state.StatusInterrupted || strings.Contains(b, "（卡住") || !strings.Contains(b, "（进行中：agent/x") {
		t.Fatalf("%+v\n%s", j.Gate, b)
	}
	items, _ := f.p.Items()
	found := false
	for _, it := range items {
		found = found || it.Fingerprint == j.Fingerprint && it.InProgress
	}
	if !found {
		t.Errorf("换回去之后指纹要对得上，续跑才接得上：%+v", items)
	}
	// 已经不卡着的决定不生效
	if err := f.a.Decide(j.ID, gate.DecidePass, "x"); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.Override != "" {
		t.Errorf("%+v", j.Gate)
	}
}

// 点了「退回」时主目录不在 main：决定留着，切回来下次 tick 做成。用户已经看到「已定」了，不能丢。
func TestReturnDecisionSurvivesBlockedMain(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	f.git("checkout", "-q", "-b", "用户的分支")
	if err := f.a.Decide(j.ID, gate.DecideReturn, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	f.a.Collect(f.st)
	if j.Gate.State != state.GateStuck || j.Gate.DecideErr == "" {
		t.Fatalf("%+v", j.Gate)
	}
	if n := strings.Count(f.out.String(), "退回还没做成"); n != 1 {
		t.Errorf("同样的错只记一次：%d", n)
	}
	f.git("checkout", "-q", "main")
	f.a.Collect(f.st)
	if j.Gate.State != state.GateReturned || !j.ResumePending || j.Gate.DecideErr != "" {
		t.Fatalf("%+v", j.Gate)
	}
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), "decision.json")); err == nil {
		t.Error("做成了要删")
	}
}

// 排队：同项目一个、计入 max_parallel、按排队时间、冷却时不起、NotBefore 没到不起。
func TestStartQueuedRules(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.a.SelfPath = "/usr/bin/true"
	mk := func(id, project, st string, at time.Time) *state.Job {
		j := &state.Job{ID: id, Project: project, ProjectDir: filepath.Join("/p", project), Status: state.StatusDone,
			Gate: &state.GateRecord{State: st, Branch: "b", QueuedAt: at}}
		os.MkdirAll(f.a.JobDir(id), 0o755)
		f.st.Jobs = append(f.st.Jobs, j)
		return j
	}
	now := f.a.Now()
	f.a.Cfg.MaxParallel = 2
	a2 := mk("a2", "a", state.GateQueued, now.Add(time.Second))
	a1 := mk("a1", "a", state.GateQueued, now)
	b1 := mk("b1", "b", state.GateQueued, now)
	c1 := mk("c1", "c", state.GateQueued, now)
	if got := f.a.StartQueued(f.st); got != 2 {
		t.Fatalf("上限 2：%d", got)
	}
	if a1.Gate.State != state.GateRunning || a2.Gate.State != state.GateQueued || b1.Gate.State != state.GateRunning || c1.Gate.State != state.GateQueued {
		t.Errorf("%s %s %s %s", a1.Gate.State, a2.Gate.State, b1.Gate.State, c1.Gate.State)
	}
	if ok, why := state.Capacity(f.st, f.a.Cfg, ""); ok || !strings.Contains(why, "在过门") {
		t.Errorf("门进程占名额：%v %s", ok, why)
	}
	a1.Gate.State, b1.Gate.State = state.GateMerged, state.GateMerged
	for _, j := range []*state.Job{a2, c1} { // 冷却按任务的后端看：这里的 cooldown_until 是默认后端的
		j.Backend = f.a.Cfg.Backend
	}
	f.st.CooldownUntil = now.Add(time.Hour)
	if got := f.a.StartQueued(f.st); got != 0 {
		t.Errorf("冷却时不起：%d", got)
	}
	f.st.CooldownUntil = time.Time{}
	a2.Gate.NotBefore = now.Add(time.Minute)
	c1.Gate.NotBefore = now.Add(time.Minute)
	if got := f.a.StartQueued(f.st); got != 0 {
		t.Errorf("没到时候不起：%d", got)
	}
	c1.Gate.NotBefore = now.Add(1000 * time.Hour) // 时钟跳过：不能一直等
	if got := f.a.StartQueued(f.st); got != 1 || c1.Gate.State != state.GateRunning {
		t.Errorf("%d %s", got, c1.Gate.State)
	}
}

// 门进程没留下结果：排回去重跑；连续多次算判不了。还拿着锁且没过期限的不动。
func TestCrashRequeueThenStuck(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	os.MkdirAll(f.a.Dir(j.ID), 0o700)
	for i := 1; i <= 2; i++ {
		j.Gate.State = state.GateRunning
		f.a.Collect(f.st)
		if j.Gate.State != state.GateQueued || j.Gate.Crashes != i {
			t.Fatalf("第 %d 次：%+v", i, j.Gate)
		}
	}
	j.Gate.State = state.GateRunning
	f.a.Collect(f.st)
	if j.Gate.State != state.GateStuck || !strings.Contains(j.Gate.Report, "连续 3 次") {
		t.Fatalf("%+v", j.Gate)
	}
}

func TestResultsResetCrashes(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	os.MkdirAll(f.a.Dir(j.ID), 0o700)
	j.Gate.Crashes = 2
	res := f.run(j)
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || j.Gate.Crashes != 0 || j.Gate.Merge == "" || !strings.HasPrefix(j.Summary, "合并提交 ") {
		t.Fatalf("%+v %q", j.Gate, j.Summary)
	}
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), "last.json")); err != nil {
		t.Error("结果要留给 report")
	}
}

// 主目录挡着：排回队尾，只通知一次。
func TestWaitNotifiesOnce(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	before := j.Gate.QueuedAt
	time.Sleep(2 * time.Millisecond)
	f.collect(j, gate.RunResult{Outcome: "wait", Reason: "主目录挡着合并"})
	f.collect(j, gate.RunResult{Outcome: "wait", Reason: "主目录挡着合并"})
	if j.Gate.State != state.GateQueued || !j.Gate.QueuedAt.After(before) || !j.Gate.WaitNoted || !j.Gate.NotBefore.After(f.a.Now()) {
		t.Fatalf("%+v", j.Gate)
	}
	if got := f.a.StartQueued(f.st); got != 0 {
		t.Errorf("主目录挡着时不能每次 tick 都占名额：%d", got)
	}
	s, _ := f.k.Notify.LoadNotify()
	n := 0
	for _, x := range s.Pending {
		if strings.HasPrefix(x.Key, "gate-wait:") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("只通知一次：%d", n)
	}
}

// 撞用量上限：不算门的结论，不消耗轮数，进全局冷却，到时候重跑。
func TestDeferOnLimit(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.Backend = f.a.Cfg.Backend // 冷却按任务的后端记：这里的 cooldown_until 是默认后端的
	reset := f.a.Now().Add(2 * time.Hour)
	f.collect(j, gate.RunResult{Outcome: "defer", Reason: "审查撞了上限", Results: []gate.Result{{Gate: "review", Outcome: gate.Unknown, Defer: "撞了上限", Limit: true, ResetAt: reset}}})
	if j.Gate.State != state.GateQueued || !j.Gate.NotBefore.IsZero() || !f.st.CooldownUntil.Equal(reset) || j.Status != state.StatusDone {
		t.Fatalf("%+v %v", j.Gate, f.st.CooldownUntil)
	}
}

// 收尾没写上：下次 tick 补，补上之前这一条还归门管（不算孤儿）。
func TestRetryWrap(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	orig := f.backlog()
	os.WriteFile(f.p.BacklogPath(), []byte(orig+"用户在写\n"), 0o644)
	res := f.run(j)
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr == "" || !state.GateActive(j) {
		t.Fatalf("%+v", j.Gate)
	}
	f.a.Collect(f.st)
	if j.Gate.WrapErr == "" {
		t.Fatal("还写不了")
	}
	os.WriteFile(f.p.BacklogPath(), []byte(orig), 0o644)
	f.a.Collect(f.st)
	if j.Gate.WrapErr != "" || state.GateActive(j) || !strings.Contains(f.backlog(), "- [x] 做事 —— 要什么") {
		t.Fatalf("%+v\n%s", j.Gate, f.backlog())
	}
	if got := f.git("branch", "--list", "agent/x"); got != "" {
		t.Errorf("补上之后分支也要删：%s", got)
	}
}

// 第三方新增一道门：登记、项目调档位、进报告，只走这些接口。
func TestThirdPartyGate(t *testing.T) {
	f := newFix(t, fixBacklog)
	var deps gate.Deps
	t.Cleanup(gate.Register("lint", gate.LevelWarn, func(d gate.Deps) gate.Gate {
		deps = d
		return fakeGate{name: "lint", outcome: gate.Fail, applies: true}
	}))
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	r, ok := resultOf(res, "lint")
	if res.Outcome != "merged" || !ok || r.Level != gate.LevelWarn {
		t.Fatalf("%+v", res)
	}
	if deps.Git == nil || deps.Script == nil || deps.Env == nil {
		t.Errorf("门能用的东西：%+v", deps)
	}
}

// 重名是接线写错了。
func TestRegisterTwicePanics(t *testing.T) {
	undo := gate.Register("一道门", gate.LevelWarn, func(gate.Deps) gate.Gate { return nil })
	defer undo()
	defer func() {
		if recover() == nil {
			t.Error("重名要 panic")
		}
	}()
	gate.Register("一道门", gate.LevelWarn, func(gate.Deps) gate.Gate { return nil })
}

// 脚本隔离运行：通过、失败、超时、被停、跑不起来；环境走白名单。
func TestRunScript(t *testing.T) {
	cases := []struct {
		name, script string
		timeout      time.Duration
		cancel       bool
		status       gate.ScriptStatus
	}{
		{"通过", "#!/bin/sh\nexit 0\n", time.Minute, false, gate.ScriptPassed},
		{"失败", "#!/bin/sh\necho 坏了\nexit 3\n", time.Minute, false, gate.ScriptFailed},
		{"超时", "#!/bin/sh\nsleep 60\n", 300 * time.Millisecond, false, gate.ScriptTimeout},
		{"被停", "#!/bin/sh\nsleep 60\n", time.Minute, true, gate.ScriptUnknown},
		{"跑不起来", "#!/没有这个解释器\n", time.Minute, false, gate.ScriptUnknown},
		{"脚本说跑不了", "#!/bin/sh\nexit " + strconv.Itoa(gate.ScriptCannotRun) + "\n", time.Minute, false, gate.ScriptUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "s")
			os.WriteFile(script, []byte(c.script), 0o755)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			res := gate.RunScript(ctx, gate.ScriptOpts{Script: script, Dir: dir, Log: filepath.Join(dir, "log"), Timeout: c.timeout, WorkDir: dir, Mark: "gate-x"})
			if res.Status != c.status {
				t.Fatalf("%+v，要 %s", res, c.status)
			}
			if c.status == gate.ScriptFailed && res.ExitCode != 3 {
				t.Errorf("退出码：%+v", res)
			}
		})
	}
	t.Run("环境白名单和输出上限", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "不该传进去")
		dir := t.TempDir()
		script := filepath.Join(dir, "s")
		os.WriteFile(script, []byte("#!/bin/sh\nenv\nwhile :; do echo 写满写满写满写满写满写满写满写满写满写满写满写满写满; done\n"), 0o755)
		res := gate.RunScript(context.Background(), gate.ScriptOpts{Script: script, Dir: dir, Log: filepath.Join(dir, "log"), Timeout: 2 * time.Second, WorkDir: dir, Mark: "gate-x"})
		if res.Status != gate.ScriptTimeout {
			t.Fatalf("%+v", res)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		if strings.Contains(string(b), "ANTHROPIC_API_KEY") || !strings.Contains(string(b), "AGENTQ_JOB=gate-x") {
			t.Errorf("环境不对：%.200s", b)
		}
		if int64(len(b)) > gate.LogMax+4096 {
			t.Errorf("输出 %d 字节", len(b))
		}
	})
	t.Run("不是可执行文件", func(t *testing.T) {
		dir := t.TempDir()
		res := gate.RunScript(context.Background(), gate.ScriptOpts{Script: filepath.Join(dir, "没有"), Dir: dir, Log: filepath.Join(dir, "log"), Timeout: time.Second, WorkDir: dir})
		if res.Status != gate.ScriptUnknown {
			t.Errorf("%+v", res)
		}
	})
}

// 升级：老任务还标着核验在跑，改记未知、清 worktree；没写进去的修回归不写了；老进程还活着就等它。老记录其余字段不动。
func TestMigrateVerify(t *testing.T) {
	f := newFix(t, fixBacklog)
	dir := filepath.Join(f.a.DataDir(), "verify", "old")
	os.MkdirAll(dir, 0o700)
	wt := gate.Worktree(dir)
	f.git("worktree", "add", "--detach", "--quiet", wt, "HEAD")
	running := &state.Job{ID: "old", Project: "demo", ProjectDir: f.dir, Verify: &state.MergeVerify{Commit: "abc", Script: "running", PID: 999999, RegressionPending: "- [ ] 修回归：abc 做事"}}
	failed := &state.Job{ID: "old2", Project: "demo", ProjectDir: f.dir, Verify: &state.MergeVerify{Commit: "def", Script: "failed", Failed: true, Regression: "修回归：def"}}
	os.MkdirAll(f.a.JobDir("old"), 0o755)
	os.MkdirAll(f.a.JobDir("old2"), 0o755)
	f.st.Jobs = []*state.Job{running, failed}
	before := f.backlog()
	f.a.MigrateVerify(f.st)
	if running.Verify.Script != "unknown" || running.Verify.RegressionPending != "" || running.Verify.Commit != "abc" {
		t.Errorf("%+v", running.Verify)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree 要清：%v", err)
	}
	if !state.VerifyFailed(failed) || state.JobLabel(failed) != "合并后核验失败" {
		t.Errorf("老记录的显示不变：%+v", failed.Verify)
	}
	if f.backlog() != before {
		t.Errorf("不写修回归：%s", f.backlog())
	}
}

// status 里看得到在过门和卡着的，没有就没有这一段。
func TestStatusLines(t *testing.T) {
	f := newFix(t, fixBacklog)
	if got := f.a.StatusLines(f.st); len(got) != 0 {
		t.Errorf("%v", got)
	}
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	for st, want := range map[string]string{state.GateQueued: "等过门", state.GateRunning: "过门中", state.GateStuck: "agentq gate report " + j.ID} {
		j.Gate.State = st
		if got := f.a.StatusLines(f.st); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s：%v", st, got)
		}
	}
	j.Gate.State = state.GateMerged
	if got := f.a.StatusLines(f.st); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestReportCommand(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "没有检查命令", Results: []gate.Result{{Gate: "test", Level: gate.LevelError, Outcome: gate.Unknown, Summary: "没有检查命令", Evidence: []string{"证据一行"}}}})
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", j.ID}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"门没过，等你定", "agent/x", "没有检查命令", "证据一行", "test（error 档）"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("缺 %s：%s", want, f.out)
		}
	}
	if err := f.a.CmdGate([]string{"别的", j.ID}); err == nil {
		t.Error("不认识的子命令")
	}
	if err := f.a.CmdGate(nil); err == nil {
		t.Error("没参数")
	}
}

// 分支已经在 main 上，不是引擎合的：判绕过，门不跑；用户放行就只补收尾。
func TestBranchOnMainWithoutIntentIsBypass(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "门", outcome: gate.Pass, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("merge", "-q", "--no-edit", "agent/x")
	res := f.run(j)
	if res.Outcome != "bypass" || calls != 0 || res.Merge != "" {
		t.Fatalf("%+v %d", res, calls)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || j.Gate.Merge != "" || strings.Contains(f.out.String(), "过门合并") {
		t.Fatalf("%+v\n%s", j.Gate, f.out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, gate.ReadyFile)); err == nil {
		t.Error("残留的 ready.json 要删")
	}
	f.k.Runner.RetryStuckMarks(f.st)
	j.Gate.Override = "我看过了"
	res = f.run(j)
	if res.Outcome != "merged" || calls != 0 || !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatalf("%+v\n%s", res, f.backlog())
	}
}

// main 上残留一份 ready.json：不算任务改了门，候选顺手删掉，后面的任务照常过。
func TestStrayReadyOnMainDoesNotBlame(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	os.WriteFile(filepath.Join(f.dir, gate.ReadyFile), []byte(`{"branch":"agent/old"}`), 0o644)
	f.git("add", "-f", gate.ReadyFile)
	f.git("commit", "-q", "-m", "别人留下的")
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.dir, gate.ReadyFile)); err == nil {
		t.Error("合并要带掉残留的 ready.json")
	}
}

// 分支只提交了 ready.json：条目不能关，退回；轮数用完等用户定，不死循环。用户放行才关。
func TestEmptyChangeDoesNotClose(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := 0
	register(t, fakeGate{name: "门", outcome: gate.Pass, applies: true, calls: &calls}, gate.LevelError)
	j := f.claim("做事", "agent/x", nil, ready("agent/x"))
	before := f.main()
	res := f.run(j)
	if res.Outcome != "fail" || calls != 0 || f.main() != before || strings.Contains(f.backlog(), "- [x] 做事") || !strings.Contains(res.Reason, "没有改动") || !strings.Contains(res.Reason, "不用改") {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateReturned || !j.ResumePending {
		t.Fatalf("先退回：%+v", j.Gate)
	}
	j.Status, j.ResumePending = state.StatusDone, false
	j.Gate = &state.GateRecord{State: state.GateRunning, Branch: "agent/x", Round: gate.MaxRounds, Ready: ready("agent/x")}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("轮数用完等用户定：%+v", j.Gate)
	}
	j.Gate.Override = "确实不用改"
	res = f.run(j)
	if res.Outcome != "merged" || !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatalf("放行才关：%+v", res)
	}
}

// 用户放行后遇到合并冲突：不退回（条目标着卡住，退回没人接得上），再等用户定，能再放行。
func TestOverrideConflictGoesStuck(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"same.txt": "分支的"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	os.WriteFile(filepath.Join(f.dir, "same.txt"), []byte("main 的"), 0o644)
	f.git("add", "same.txt")
	f.git("commit", "-q", "-m", "main 也改")
	if err := f.a.Decide(j.ID, gate.DecidePass, "我看过了"); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	res := f.run(j)
	if res.Outcome != "fail" {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || j.Status != state.StatusDone || j.ResumePending || j.Gate.Override != "" {
		t.Fatalf("%+v %s %v", j.Gate, j.Status, j.ResumePending)
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range s.Pending {
		found = found || n.Card != nil && n.Card.GateJob == j.ID && strings.Contains(strings.Join(n.Lines, " "), "same.txt")
	}
	if !found {
		t.Errorf("要通知：%+v", s.Pending)
	}
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	if err := f.a.CmdGate([]string{"pass", j.ID, "--reason", "再放行"}); err != nil {
		t.Errorf("还能再放行：%v", err)
	}
}

// 放行只合用户看到的那个提交：卡住之后分支上多出来的不带，审计写提交号，摘要不再说门没过。
func TestPassPinsHead(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	seen := j.Gate.Head
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了", Head: seen})
	f.k.Runner.RetryStuckMarks(f.st)
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	f.commit(wt, map[string]string{"later.txt": "后来的"}, gate.Ready{})
	if err := f.a.Decide(j.ID, gate.DecidePass, "我看过了"); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	res := f.run(j)
	f.collect(j, res)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v %+v", res, j.Gate)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "later.txt")); err == nil {
		t.Error("放行之后多出来的提交不能带进去")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a.txt")); err != nil {
		t.Error("看到的那个提交要进去")
	}
	if got := f.git("branch", "--list", "agent/x"); got == "" {
		t.Error("分支上还有没合的提交，要留着")
	}
	audit := testkit.ReadFile(t, filepath.Join(f.a.DataDir(), "gate", "audit.log"))
	if !strings.Contains(audit, seen) {
		t.Errorf("审计要写提交号：%s", audit)
	}
	if strings.Contains(j.Summary, "门没过") || !strings.Contains(j.Summary, "用户放行") {
		t.Errorf("%s", j.Summary)
	}
}

// 收尾提交时被杀（pre-commit 跑到一半）：文件改了没提交，不能当写过了，要提交上，不留脏文件挡后面。
func TestWrapFinishesAfterKilledCommit(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	// 第一次：BACKLOG 有用户的改动，合并了但收尾没写上
	orig := f.backlog()
	os.WriteFile(f.p.BacklogPath(), []byte(orig+"用户在写\n"), 0o644)
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr == "" {
		t.Fatalf("%+v", res)
	}
	os.WriteFile(f.p.BacklogPath(), []byte(orig), 0o644)
	short := res.Merge[:12]
	// 模拟：写好了两个文件，提交时被杀
	marked := backlog.MarkMerged(orig, j.Fingerprint, j.Title, "agent/x", backlog.MergedLine{Commit: short, Acceptance: backlog.CleanTodoText("看 a.txt", 300)})
	os.WriteFile(f.p.BacklogPath(), []byte(marked), 0o644)
	progress := filepath.Join(f.dir, "PROGRESS.md")
	os.WriteFile(progress, []byte(gate.NoteProgress(testkit.ReadFile(t, progress), "2026-11-06 做好了（"+short+"）", short)), 0o644)
	if err := f.a.WrapUp(gate.PlanOf(f.a, j), res.Merge); err != nil {
		t.Fatal(err)
	}
	if got := f.git("status", "--porcelain", "--", "docs/BACKLOG.md", "PROGRESS.md"); got != "" {
		t.Errorf("要提交上：%s", got)
	}
	if got := f.git("show", "HEAD:docs/BACKLOG.md"); !strings.Contains(got, "合并提交 "+short) {
		t.Errorf("%s", got)
	}
	if got := f.git("show", "HEAD:PROGRESS.md"); strings.Count(got, "（"+short+"）") != 1 {
		t.Errorf("%s", got)
	}
	// 用户自己改的不当成我们写的
	head := f.main()
	os.WriteFile(f.p.BacklogPath(), []byte(f.backlog()+"用户又在写\n"), 0o644)
	if err := f.a.WrapUp(gate.PlanOf(f.a, j), res.Merge); err != nil || f.main() != head || !strings.Contains(f.backlog(), "用户又在写") {
		t.Errorf("写过了什么都不动：%v", err)
	}
}

// 排队时用户把任务分支删了：他扔掉的提交不能进 main，判不了，等他定。
func TestDeletedBranchNotMerged(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	before := f.main()
	f.git("worktree", "remove", "--force", filepath.Join(f.dir, ".claude", "worktrees", "x"))
	f.git("branch", "-D", "agent/x")
	res := f.run(j)
	if res.Outcome != "unknown" || f.main() != before {
		t.Fatalf("%+v main 动了：%v", res, f.main() != before)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a.txt")); err == nil {
		t.Error("删掉的分支的文件不能进主目录")
	}
}

// 排队时别人合了分支又删了：提交在 main 上，还是绕过，不是查不了。
func TestDeletedBranchOnMainIsBypass(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("merge", "-q", "--no-edit", "agent/x")
	f.git("worktree", "remove", "--force", filepath.Join(f.dir, ".claude", "worktrees", "x"))
	f.git("branch", "-D", "agent/x")
	if res := f.run(j); res.Outcome != "bypass" {
		t.Fatalf("%+v", res)
	}
}

// 合并命令报错但 main 已经前进（钩子跑超时）：是引擎合的，不能判成绕过。
// 写收尾的提交时用户正好切走：记录落到别的分支，默认分支上条目还标着进行中。要报出来，切回来后补写。
func TestWrapUpBranchSwitchedDuringCommit(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("branch", "side")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	flag := filepath.Join(bin, "once")
	script := "#!/bin/sh\nd=$PWD\n[ \"$1\" = -C ] && d=$2\n" +
		"case \" $* \" in *\" commit \"*) case \"$d\" in */demo) if [ ! -e " + flag + " ]; then : > " + flag + "; " + real + " -C \"$d\" checkout -q side; fi;; esac;; esac\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr == "" {
		t.Fatalf("没写上要报出来：%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr == "" {
		t.Fatalf("%+v", j.Gate)
	}
	f.git("checkout", "-q", "main")
	f.a.Collect(f.st)
	if j.Gate.WrapErr != "" || !strings.Contains(f.git("show", "refs/heads/main:docs/BACKLOG.md"), "- [x] 做事") {
		t.Fatalf("切回来要补写：%+v\n%s", j.Gate, f.git("show", "refs/heads/main:docs/BACKLOG.md"))
	}
}

func TestMergeErrorAfterMainMoved(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\n" + real + " \"$@\"\ncode=$?\ncase \" $* \" in *\" merge --ff-only \"*) exit 1;; esac\nexit $code\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatalf("%+v\n%s", j.Gate, f.backlog())
	}
}

// 门撞上认证失败：和任务一样暂停这个后端、通知用户，同一个 Key 只提醒一次。
func TestAuthFailureNotifiesAndPauses(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "defer", Reason: "认证失败", Results: []gate.Result{{Gate: "review", Outcome: gate.Unknown, Defer: "认证失败", Auth: true}}})
	if j.Gate.State != state.GateQueued || j.Gate.NotBefore.IsZero() {
		t.Fatalf("%+v", j.Gate)
	}
	if _, ok := f.a.AuthPausedUntil(f.a.BackendFor(j.Backend).Name()); !ok {
		t.Error("要记下暂停")
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 1 || !strings.Contains(s.Pending[0].Text, "认证失败") {
		t.Fatalf("要通知：%+v", s.Pending)
	}
	if st := f.st.CooldownUntil; !st.IsZero() {
		t.Errorf("认证失败不走全局冷却：%v", st)
	}
}
