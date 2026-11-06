package gate_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// masterFix：默认分支叫 master 的项目。
func masterFix(t *testing.T) *fix {
	t.Helper()
	f := newFix(t, fixBacklog)
	f.git("branch", "-q", "-m", "main", "master")
	f.def = "master"
	return f
}

func (f *fix) isAncestor(commit, branch string) bool {
	f.t.Helper()
	return testkit.GitTErr(f.home, f.dir, "merge-base", "--is-ancestor", commit, "refs/heads/"+branch) == nil
}

func TestGateMergesIntoMaster(t *testing.T) {
	f := masterFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	before := f.git("rev-parse", "master")
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr != "" || res.Base != before {
		t.Fatalf("%+v", res)
	}
	if !f.isAncestor(res.Merge, "master") {
		t.Error("合并提交不在 master 上")
	}
	if testkit.GitTErr(f.home, f.dir, "rev-parse", "--verify", "--quiet", "refs/heads/main") == nil {
		t.Error("不该多出 main 分支")
	}
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatalf("%+v\n%s", j.Gate, f.backlog())
	}
	if out := f.git("branch", "--list", "agent/x"); out != "" {
		t.Errorf("分支要删：%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, gate.ReadyFile)); err == nil {
		t.Error("ready.json 不进 master")
	}
}

// 门说的话用项目实际的分支名，退回给任务的提示才对得上。
func TestGateConflictNamesMaster(t *testing.T) {
	f := masterFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "任务的\n"}, ready("agent/x"))
	testkit.WriteInstall(t, filepath.Join(f.dir, "a.txt"), "用户的\n", 0o644)
	f.git("add", "a.txt")
	f.git("commit", "-q", "-m", "用户也改了")
	res := f.run(j)
	if res.Outcome != "fail" || !strings.Contains(res.Reason, "和最新 master 有冲突") || !strings.Contains(res.Reason, "从最新 master 合入") {
		t.Fatalf("%+v", res)
	}
}

// 主目录不在默认分支：等着，说的是 master。
func TestGateWaitsWhenNotOnMaster(t *testing.T) {
	f := masterFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("checkout", "-q", "-b", "用户的分支")
	res := f.run(j)
	if res.Outcome != "wait" || !strings.Contains(res.Reason, "主目录不在 master") {
		t.Fatalf("%+v", res)
	}
}

func TestGateBypassOnMaster(t *testing.T) {
	f := masterFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("merge", "-q", "--no-edit", "agent/x")
	if !f.a.OnJobBranch(j, j.Gate.Head) {
		t.Fatal("分支尖已经在默认分支上")
	}
	res := f.run(j)
	if res.Outcome != "bypass" || !strings.Contains(res.Reason, "已经在 master 上") {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.dir, gate.ReadyFile)); err == nil {
		t.Error("残留的 ready.json 要从 master 上删掉")
	}
	if got := f.git("log", "-1", "--format=%s", "master"); !strings.Contains(got, "去掉 master 上残留的") {
		t.Errorf("%s", got)
	}
}

// 老任务（没记分支）过门时认不出默认分支（分支被改了名）：判不了，等用户定，分支留着，什么都不合。
func TestGateNoDefaultBranchIsUnknown(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.DefaultBranch = ""
	f.git("branch", "-q", "-m", "main", "trunk")
	before := f.git("rev-parse", "trunk")
	res := f.run(j)
	if res.Outcome != "unknown" || !strings.Contains(res.Reason, "认不出默认分支") {
		t.Fatalf("%+v", res)
	}
	if f.git("rev-parse", "trunk") != before || f.git("branch", "--list", "agent/x") == "" {
		t.Error("什么都不该动")
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	if f.a.OnJobBranch(j, j.Gate.Head) {
		t.Error("认不出算不在默认分支上")
	}
}

// 老任务过门途中 origin/HEAD 变了：这次过门还是合进开始时认的分支，收尾也写在那边，不会合一半换目标。主目录在 develop 才看 origin/HEAD。
func TestGateOldJobBranchFixedDuringRun(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.git("branch", "-q", "-m", "main", "develop")
	f.def = "develop"
	head := f.git("rev-parse", "develop")
	f.git("branch", "release")
	f.git("update-ref", "refs/remotes/origin/develop", head)
	f.git("update-ref", "refs/remotes/origin/release", head)
	f.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	script := "#!/bin/sh\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\ngit -C '" + f.dir + "' symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/release\n"
	testkit.WriteInstall(t, filepath.Join(f.dir, gate.VerifyScript), script, 0o755)
	f.git("add", gate.VerifyScript)
	f.git("commit", "-q", "-m", "门脚本")
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.DefaultBranch = ""
	releaseBefore := f.git("rev-parse", "release")
	res := f.run(j)
	if got := f.git("symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/release" {
		t.Fatalf("门脚本没跑到：%s %+v", got, res)
	}
	if res.Outcome != "merged" || res.WrapErr != "" {
		t.Fatalf("%+v", res)
	}
	if !f.isAncestor(res.Merge, "develop") || f.git("rev-parse", "release") != releaseBefore {
		t.Error("要合进开始时认的 develop，release 不动")
	}
	if !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Errorf("收尾要写上：\n%s", f.backlog())
	}
}

func (f *fix) waitNotice(j *state.Job) notify.Notice {
	f.t.Helper()
	s, err := f.a.Notify.LoadNotify()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, n := range s.Pending {
		if n.Key == "gate-wait:"+j.ID {
			return n
		}
	}
	f.t.Fatalf("没有挡着合并的通知：%+v", s.Pending)
	return notify.Notice{}
}

// 排队的任务主目录挡着：不在默认分支说切分支，有重叠改动才说处理改动，别的原因不让用户动手。
func TestGateWaitNoticeSaysWhatToDo(t *testing.T) {
	t.Run("不在默认分支", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.a.Cfg.NotifyEnabled = true
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
		f.git("checkout", "-q", "-b", "feature")
		res := f.run(j)
		if res.Outcome != "wait" || res.WaitBranch != "main" || res.WaitChanges {
			t.Fatalf("%+v", res)
		}
		f.collect(j, res)
		n := f.waitNotice(j)
		lines := n.Text + strings.Join(n.Lines, "\n")
		testkit.AssertContains(t, lines, "主目录不在 main", "git switch main")
		if strings.Contains(lines, "改动") {
			t.Fatalf("没有改动要处理：\n%s", lines)
		}
	})
	t.Run("不在默认分支时主目录也有无关改动", func(t *testing.T) {
		f := masterFix(t)
		f.a.Cfg.NotifyEnabled = true
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
		f.git("checkout", "-q", "-b", "feature")
		testkit.WriteInstall(t, filepath.Join(f.dir, "无关.txt"), "x", 0o644)
		f.collect(j, f.run(j))
		testkit.AssertContains(t, strings.Join(f.waitNotice(j).Lines, "\n"), "git switch master")
	})
	t.Run("有重叠的改动", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.a.Cfg.NotifyEnabled = true
		j := f.claim("做事", "agent/x", map[string]string{"中文 名.txt": "分支的"}, ready("agent/x"))
		testkit.WriteInstall(t, filepath.Join(f.dir, "中文 名.txt"), "用户没提交的", 0o644)
		res := f.run(j)
		if res.Outcome != "wait" || res.WaitBranch != "" || !res.WaitChanges {
			t.Fatalf("%+v", res)
		}
		f.collect(j, res)
		n := f.waitNotice(j)
		testkit.AssertContains(t, n.Text+strings.Join(n.Lines, "\n"), "改动挡着合并", "重叠的未提交改动", "提交或还原")
		if strings.Contains(strings.Join(n.Lines, "\n"), "git switch") {
			t.Fatalf("%+v", n)
		}
	})
	t.Run("别的原因：index 被锁", func(t *testing.T) {
		f := newFix(t, fixBacklog)
		f.a.Cfg.NotifyEnabled = true
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
		testkit.WriteInstall(t, filepath.Join(f.dir, ".git", "index.lock"), "", 0o644)
		res := f.run(j)
		if res.Outcome != "wait" || res.WaitBranch != "" || res.WaitChanges {
			t.Fatalf("%+v", res)
		}
		f.collect(j, res)
		n := f.waitNotice(j)
		lines := n.Text + strings.Join(n.Lines, "\n")
		testkit.AssertContains(t, lines, "这次没合并上", "下次 tick 自动再试")
		if strings.Contains(lines, "提交或还原") || strings.Contains(lines, "git switch") {
			t.Fatalf("没有要用户做的：\n%s", lines)
		}
	})
}

// 排队时默认分支被改名：卡住标记提交进 BACKLOG，不带本机路径；日志和 status 留完整路径。
func TestGateStuckMarkHasNoLocalPath(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.DefaultBranch = ""
	f.git("branch", "-q", "-m", "main", "trunk")
	res := f.run(j)
	if !strings.Contains(res.Reason, f.dir) {
		t.Fatalf("门的报告留完整路径：%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	f.git("branch", "-q", "-m", "trunk", "master")
	f.k.Runner.RetryStuckMarks(f.st)
	b := f.backlog()
	testkit.AssertContains(t, b, "（卡住：门没过：", "<本机路径>")
	if strings.Contains(b, f.dir) || strings.Contains(f.git("log", "-p", "-1", "--format=", "--", "docs/BACKLOG.md"), f.dir) {
		t.Fatalf("本机路径进了 BACKLOG：\n%s", b)
	}
}

// bothFix：main、master 并存、没有 origin/HEAD，主目录在 master，任务基于 master。main 比 master 旧。
func bothFix(t *testing.T) *fix {
	t.Helper()
	f := newFix(t, fixBacklog)
	f.git("checkout", "-q", "-b", "master")
	f.def = "master"
	testkit.WriteInstall(t, filepath.Join(f.dir, "master-only.txt"), "x", 0o644)
	f.git("add", "master-only.txt")
	f.git("commit", "-q", "-m", "master 往前走")
	return f
}

// 主目录切到错的分支：等着，不合；提示写记下的分支；切回去合进它，另一条分支一个提交都不多。
func TestGateUsesRecordedBranch(t *testing.T) {
	f := bothFix(t)
	f.a.Cfg.NotifyEnabled = true
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	plan := gate.PlanOf(f.a, j)
	if plan.DefaultBranch != "master" {
		t.Fatalf("%+v", plan)
	}
	mainBefore, masterBefore := f.git("rev-parse", "main"), f.git("rev-parse", "master")
	for _, cur := range []string{"feature", "main"} {
		if cur == "feature" {
			f.git("checkout", "-q", "-b", "feature")
		} else {
			f.git("checkout", "-q", "main")
		}
		res := f.run(j)
		if res.Outcome != "wait" || res.WaitBranch != "master" || !strings.Contains(res.Reason, "主目录不在 master（"+cur+"）") {
			t.Fatalf("在 %s：%+v", cur, res)
		}
		if f.git("rev-parse", "main") != mainBefore || f.git("rev-parse", "master") != masterBefore {
			t.Fatalf("在 %s 时不该合", cur)
		}
	}
	f.git("checkout", "-q", "master")
	res := f.run(j)
	if res.Outcome != "merged" || res.WrapErr != "" || !f.isAncestor(res.Merge, "master") {
		t.Fatalf("%+v", res)
	}
	if f.git("rev-parse", "main") != mainBefore {
		t.Error("main 一个提交都不能多")
	}
	if f.isAncestor(res.Merge, "main") {
		t.Error("合并提交不能进 main")
	}
}

// 记下的分支没了：判不了，发决定卡片，不换分支合。
func TestGateRecordedBranchGone(t *testing.T) {
	f := bothFix(t)
	f.a.Cfg.NotifyEnabled = true
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("checkout", "-q", "main")
	f.git("branch", "-q", "-m", "master", "renamed")
	before := f.git("rev-parse", "main")
	res := f.run(j)
	if res.Outcome != "unknown" || !strings.Contains(res.Reason, "这条任务基于 master 分支，查不到了") || res.Merge != "" {
		t.Fatalf("%+v", res)
	}
	if f.git("rev-parse", "main") != before {
		t.Error("不能换分支合")
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	s, err := f.a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 1 || s.Pending[0].Cat != "decide" || s.Pending[0].Card == nil {
		t.Fatalf("要发决定卡片：%+v", s.Pending)
	}
}

// 用户放行也只合到记下的分支；主目录在别的分支时等着。
func TestGateOverrideStaysOnRecordedBranch(t *testing.T) {
	f := bothFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.Gate.Override = "我看过了"
	f.git("checkout", "-q", "main")
	mainBefore := f.git("rev-parse", "main")
	res := f.run(j)
	if res.Outcome != "wait" || res.WaitBranch != "master" || f.git("rev-parse", "main") != mainBefore {
		t.Fatalf("%+v", res)
	}
	f.git("checkout", "-q", "master")
	res = f.run(j)
	if res.Outcome != "merged" || !f.isAncestor(res.Merge, "master") || f.git("rev-parse", "main") != mainBefore {
		t.Fatalf("%+v", res)
	}
}

// 合并后收尾没写上，主目录又切走了：不写到别的分支，也不当写过了；切回来补上。
func TestGateWrapRetryWaitsForBranch(t *testing.T) {
	f := bothFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	f.git("reset", "-q", "--hard", "HEAD~1") // 把收尾那个提交退掉，模拟收尾没写上
	if strings.Contains(f.backlog(), "- [x] 做事") {
		t.Fatal("夹具不对")
	}
	f.git("checkout", "-q", "main")
	f.git("rm", "-q", "docs/BACKLOG.md") // main 上没有 BACKLOG：不看分支会当没东西要写
	f.git("commit", "-q", "-m", "main 上没有 BACKLOG")
	mainBefore := f.git("rev-parse", "main")
	err := f.a.WrapUp(gate.PlanOf(f.a, j), res.Merge)
	if err == nil || !strings.Contains(err.Error(), "主目录不在 master") || f.git("rev-parse", "main") != mainBefore {
		t.Fatalf("%v", err)
	}
	f.git("checkout", "-q", "master")
	if err := f.a.WrapUp(gate.PlanOf(f.a, j), res.Merge); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.backlog(), "- [x] 做事") {
		t.Errorf("要补上：\n%s", f.backlog())
	}
}

// 点了「退回」时主目录在别的分支：决定留着，不写到别的分支的 BACKLOG；切回来做成。
func TestGateReturnUsesRecordedBranch(t *testing.T) {
	f := bothFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	if !strings.Contains(f.backlog(), "（卡住：门没过") {
		t.Fatalf("夹具不对：\n%s", f.backlog())
	}
	f.git("checkout", "-q", "main")
	mainBefore := f.git("rev-parse", "main")
	if err := f.a.Decide(j.ID, gate.DecideReturn, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.State != state.GateStuck || !strings.Contains(j.Gate.DecideErr, "主目录不在 master") || f.git("rev-parse", "main") != mainBefore {
		t.Fatalf("%+v", j.Gate)
	}
	f.git("checkout", "-q", "master")
	f.a.Collect(f.st)
	if j.Gate.State != state.GateReturned || !strings.Contains(f.backlog(), "（进行中：agent/x") {
		t.Fatalf("%+v\n%s", j.Gate, f.backlog())
	}
}

// 主目录切走时卡住标记不写：main 的 BACKLOG 里这一条的说法不算数，也不能当写过了。切回来写在 master 上。
func TestGateStuckMarkWaitsForBranch(t *testing.T) {
	f := bothFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("checkout", "-q", "main")
	mainBefore := f.git("rev-parse", "main")
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了"})
	f.k.Runner.RetryStuckMarks(f.st)
	if j.StuckPending == "" || f.git("rev-parse", "main") != mainBefore {
		t.Fatalf("要等切回来：%q", j.StuckPending)
	}
	f.git("checkout", "-q", "master")
	f.k.Runner.RetryStuckMarks(f.st)
	if j.StuckPending != "" || !strings.Contains(f.backlog(), "（卡住：门没过") {
		t.Fatalf("%q\n%s", j.StuckPending, f.backlog())
	}
}

// status 里等过门的写记下的分支。
func TestStatusLineNamesRecordedBranch(t *testing.T) {
	f := bothFix(t)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.git("checkout", "-q", "-b", "feature")
	lines := strings.Join(f.a.StatusLines(f.st), "\n")
	testkit.AssertContains(t, lines, "等过门：", "这条基于 master", "git switch master")
	if strings.Contains(lines, "git switch main") {
		t.Fatalf("%s", lines)
	}
	f.git("checkout", "-q", "master")
	if lines := strings.Join(f.a.StatusLines(f.st), "\n"); strings.Contains(lines, "git switch") {
		t.Fatalf("在上面就不说了：%s（%s）", lines, j.ID)
	}
}

// 老任务的 plan.json 没有默认分支字段：门进程现认。
func TestGateOldPlanWithoutBranch(t *testing.T) {
	f := masterFix(t)
	f.a.Cfg.TickAfterJob = false
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.DefaultBranch = ""
	b, err := json.Marshal(gate.PlanOf(f.a, j))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("default_branch")) {
		t.Fatalf("没认过就不落盘：%s", b)
	}
	dir := f.a.Dir(j.ID)
	testkit.WriteInstall(t, filepath.Join(dir, "plan.json"), string(b), 0o600)
	var stderr bytes.Buffer
	if code := f.a.RunShell([]string{j.ID}, &stderr); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	var res gate.RunResult
	if err := json.Unmarshal(testkit.ReadBytes(t, filepath.Join(dir, "result.json")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "merged" || !f.isAncestor(res.Merge, "master") {
		t.Fatalf("%+v", res)
	}
}

func (f *fix) endNotices(j *state.Job) []notify.Notice {
	f.t.Helper()
	s, err := f.a.Notify.LoadNotify()
	if err != nil {
		f.t.Fatal(err)
	}
	var out []notify.Notice
	for _, n := range s.Pending {
		if n.Key == notify.TaskEndKey(j.Project, j.Fingerprint) {
			out = append(out, n)
		}
	}
	return out
}

// 合并了才说做完，带合并提交和验收；队列里同一条上次失败的说法被盖掉，不重复说。
func TestMergedNoticesDone(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.a.Cfg.NotifyEnabled = true
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.a.Notify.Notify(notify.Notice{Cat: "alert", Key: notify.TaskEndKey(j.Project, j.Fingerprint), Text: "上次失败"})
	res := f.run(j)
	if got := f.endNotices(j); len(got) != 1 || got[0].Cat != "alert" {
		t.Fatalf("门进程不发通知，tick 收结果才发：%+v", got)
	}
	f.collect(j, res)
	got := f.endNotices(j)
	if len(got) != 1 || got[0].Cat != "done" || got[0].JobID != j.ID || !strings.Contains(got[0].Text, "demo「做事」做完了，已合并") {
		t.Fatalf("%+v", got)
	}
	short := res.Merge[:12]
	if lines := strings.Join(got[0].Lines, "\n"); !strings.Contains(lines, "合并提交 "+short) || !strings.Contains(lines, "验收：看 a.txt") {
		t.Fatalf("%s", lines)
	}
}

// 门没过：退回不算结束，不说；卡住只有决定卡片。
func TestGateFailureNoEndNotice(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.a.Cfg.NotifyEnabled = true
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "任务的\n"}, ready("agent/x"))
	j.SessionID = "s1"
	testkit.WriteInstall(t, filepath.Join(f.dir, "a.txt"), "用户的\n", 0o644)
	f.git("add", "a.txt")
	f.git("commit", "-q", "-m", "用户也改了")
	f.collect(j, f.run(j))
	if j.Gate.State != state.GateReturned {
		t.Fatalf("%+v", j.Gate)
	}
	if got := f.endNotices(j); len(got) != 0 {
		t.Fatalf("退回不说：%+v", got)
	}
	j.Gate.Round = gate.MaxRounds
	j.Gate.State = state.GateQueued
	f.collect(j, f.run(j))
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	if got := f.endNotices(j); len(got) != 0 {
		t.Fatalf("卡住有决定卡片：%+v", got)
	}
	s, err := f.a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 1 || s.Pending[0].Cat != "decide" {
		t.Fatalf("%+v", s.Pending)
	}
}
