package tests

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/gitx"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 验收：projects_root 外、master 分支的仓库能接任务、过门、合并、收尾，条目关掉。假 claude 只照提示词说的分支开分支：提示词没写清就开不出来。
func TestIntegrationMasterExtraProject(t *testing.T) {
	t.Parallel()
	outer, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(outer, "far-master")
	e := newEnv(t, map[string]any{"extra_projects": []string{dir}})
	title := "外面的活 READYMERGE"
	inRoot := gateRepo(t, e, "far-master", "- [ ] "+title+" —— BRANCH=agent/t-m FILE=far.txt")
	testkit.GitT(t, e.home, inRoot, "branch", "-q", "-m", "main", "master")
	if err := os.Rename(inRoot, dir); err != nil {
		t.Fatal(err)
	}
	before := testkit.GitT(t, e.home, dir, "rev-parse", "master")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "默认分支是 master", "引擎过门后合进 master")
	j = e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/master"); err != nil {
		t.Errorf("合并提交不在 master 上：%v", err)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", before, j.Gate.Merge); err != nil {
		t.Errorf("候选要从 master 建：%v", err)
	}
	if err := testkit.GitTErr(e.home, dir, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil {
		t.Error("不该多出 main 分支")
	}
	if _, err := os.Stat(filepath.Join(dir, "far.txt")); err != nil {
		t.Error("任务的改动没进主目录")
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
	if out := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-m"); out != "" {
		t.Errorf("分支要删：%s", out)
	}
	if out := e.mustRun("status"); strings.Contains(out, "不接任务") {
		t.Errorf("status 不该报：\n%s", out)
	}
}

// origin/HEAD 指向 develop：从 develop 开分支，合进 develop，main 不动。
func TestIntegrationOriginHeadDevelop(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "发展分支的活 READYMERGE"
	dir := gateRepo(t, e, "dev-app", "- [ ] "+title+" —— BRANCH=agent/t-d FILE=dev.txt")
	head := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "develop")
	testkit.GitT(t, e.home, dir, "update-ref", "refs/remotes/origin/develop", head)
	testkit.GitT(t, e.home, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	e.mustRun("tick")
	j := e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/develop"); err != nil {
		t.Errorf("合并提交不在 develop 上：%v", err)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "main"); got != head {
		t.Errorf("main 不该动：%s", got)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
}

// 升级不回归：HEAD 在 main，本地有 develop，origin/HEAD 指 develop。和升级前一样过门合进 main，提示词不变。
func TestIntegrationHeadOnMainIgnoresOriginHead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "老项目的活 READYMERGE"
	dir := gateRepo(t, e, "old-app", "- [ ] "+title+" —— BRANCH=agent/t-o FILE=o.txt")
	head := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	testkit.GitT(t, e.home, dir, "branch", "develop")
	testkit.GitT(t, e.home, dir, "update-ref", "refs/remotes/origin/develop", head)
	testkit.GitT(t, e.home, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	if want := runner.BuildPrompt(j.ProjectDir, j.Item) + "\n"; prompt != want {
		t.Fatalf("提示词不该变：\n%s", prompt)
	}
	j = e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("要合进 main：%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/main"); err != nil {
		t.Errorf("合并提交不在 main 上：%v", err)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "develop"); got != head {
		t.Errorf("develop 不该动：%s", got)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
}

// main、master 并存、没有 origin/HEAD、HEAD 在 master：认领、分支、合并都在 master，不从旧 main 开分支。
func TestIntegrationHeadOnMasterWithOldMain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "两个主线的活 READYMERGE"
	dir := gateRepo(t, e, "two-app", "- [ ] 占位")
	old := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "master")
	b := strings.Replace(e.backlogOf(dir), "- [ ] 占位", "- [ ] "+title+" —— BRANCH=agent/t-m FILE=m.txt", 1)
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), b, 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "master 往前走了")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "默认分支是 master")
	j = e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("要合进 master：%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/master"); err != nil {
		t.Errorf("合并提交不在 master 上：%v", err)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "main"); got != old {
		t.Errorf("旧 main 不该动：%s", got)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
}

// 主目录在 feature：不起任务，假 claude 一次都不调；status 和 run 说清楚，日志同样的原因只记一次；切回去接着做。
func TestIntegrationHeadOnFeatureNoJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "切走时的活 READYMERGE"
	dir := gateRepo(t, e, "away-app", "- [ ] "+title+" —— BRANCH=agent/t-f FILE=f.txt")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "feature")
	why := "主目录在 feature 分支，默认分支是 main"
	first := e.mustRun("tick")
	testkit.AssertContains(t, first, "away-app 不接任务", why, "switch main", "启动 0")
	if second := e.mustRun("tick"); strings.Contains(second, why) {
		t.Fatalf("同样的原因只记一次：\n%s", second)
	}
	st := e.state()
	if len(st.Jobs) != 0 || st.BreakerUntil.After(time.Now()) || st.FailStreak != 0 {
		t.Fatalf("不起任务，不算失败：%+v", st)
	}
	if calls := e.claudeCalls(); len(calls) != 0 {
		t.Fatalf("不该调假 claude：%v", calls)
	}
	testkit.AssertContains(t, e.mustRun("status"), "away-app：1 条", "不接任务", why, "switch main")
	out, code := e.run("run", "away-app")
	if code == 0 || !strings.Contains(out, why) {
		t.Fatalf("手动 run 也不起，说清原因：%d\n%s", code, out)
	}
	if b := e.backlogOf(dir); strings.Contains(b, "进行中") {
		t.Errorf("不该认领：\n%s", b)
	}
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	if status := e.mustRun("status"); strings.Contains(status, "不接任务") {
		t.Errorf("切回去就不说了：\n%s", status)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动任务") {
		t.Fatalf("切回去要接着做：\n%s", out)
	}
	if e.state().BranchBlocked != nil && len(e.state().BranchBlocked) != 0 {
		t.Errorf("好了要清掉：%v", e.state().BranchBlocked)
	}
}

// 游离 HEAD 一样不起。
func TestIntegrationDetachedHeadNoJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "det-app", "- [ ] 游离时的活 READYMERGE —— BRANCH=agent/t-g FILE=g.txt")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "--detach")
	testkit.AssertContains(t, e.mustRun("tick"), "det-app 不接任务", "不在任何分支上", "switch main")
	if st := e.state(); len(st.Jobs) != 0 {
		t.Fatalf("%+v", st.Jobs)
	}
}

// HEAD 在 develop、origin/HEAD 也指 develop：仍走 develop。
func TestIntegrationDevelopHeadAndOrigin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "dev2", "- [ ] 活 READYMERGE —— BRANCH=agent/t-h FILE=h.txt")
	head := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "main", "develop")
	testkit.GitT(t, e.home, dir, "update-ref", "refs/remotes/origin/develop", head)
	testkit.GitT(t, e.home, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	e.mustRun("tick")
	j := e.jobByTitle("活 READYMERGE")
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "默认分支是 develop")
}

// 主目录切走时要续跑的不起，记录留着，切回去接着续跑。
func TestIntegrationResumeWaitsForHead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "resume2", "- [ ] 中断的活 —— INTERRUPT SLEEP=1")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断的活").PID, 10*time.Second)
	if out := testkit.GitT(t, e.home, dir, "status", "--porcelain"); out != "" {
		testkit.GitT(t, e.home, dir, "add", "-A")
		testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "认领")
	}
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "feature")
	calls := len(e.claudeCalls())
	testkit.AssertContains(t, e.mustRun("tick"), "resume2 不接任务", "主目录在 feature 分支")
	st := e.state()
	if len(st.Jobs) != 1 || !st.Jobs[0].ResumePending {
		t.Fatalf("续跑记录要留着：%+v", st.Jobs[0])
	}
	if got := len(e.claudeCalls()); got != calls {
		t.Fatalf("不该调假 claude：%d -> %d", calls, got)
	}
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	if out := e.mustRun("tick"); !strings.Contains(out, "续跑任务") {
		t.Fatalf("切回去要续跑：\n%s", out)
	}
}

// 叫 main 的项目提示词一字不变。
func TestDefaultBranchMainPromptUnchanged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "主线的活 READYMERGE"
	gateRepo(t, e, "plain", "- [ ] "+title+" —— BRANCH=agent/t-p FILE=p.txt")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	if want := runner.BuildPrompt(j.ProjectDir, j.Item) + "\n"; prompt != want {
		t.Fatalf("提示词变了：\n%s\n想要：\n%s", prompt, want)
	}
	if runner.BranchContract("main") != "" {
		t.Fatal("叫 main 时不多话")
	}
}

// 认不出默认分支的项目：不起任务，status 和日志写明原因，别的项目照常接。认出来之后接上。
func TestIntegrationNoDefaultBranchNoJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 2})
	bad := gateRepo(t, e, "a-trunk", "- [ ] 认不出的活 READYMERGE —— BRANCH=agent/t-x FILE=x.txt")
	testkit.GitT(t, e.home, bad, "branch", "-q", "-m", "main", "trunk")
	gateRepo(t, e, "b-good", "- [ ] 好的活 —— 不写 ready.json，下面的 tick 不会起门进程") // 门进程没人等：用例结束后还在写临时 HOME
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "a-trunk 不接任务", "认不出默认分支", "启动 1")
	st := e.state()
	if len(st.Jobs) != 1 || st.Jobs[0].Project != "b-good" {
		t.Fatalf("只该起好的项目：%+v", st.Jobs)
	}
	if st.BreakerUntil.After(time.Now()) {
		t.Error("认不出默认分支不算连环失败")
	}
	status := e.mustRun("status")
	testkit.AssertContains(t, status, "a-trunk：1 条", "不接任务", "认不出默认分支")
	if b := e.backlogOf(bad); strings.Contains(b, "进行中") {
		t.Errorf("不该认领：\n%s", b)
	}
	if out, code := e.run("run", "a-trunk"); code == 0 || !strings.Contains(out, "认不出默认分支") {
		t.Errorf("手动 run 也不起，说清原因：%d\n%s", code, out)
	}
	if runs := e.claudeCallsOf("认不出的活"); runs != 0 {
		t.Errorf("不该起进程：%d", runs)
	}
	testkit.GitT(t, e.home, bad, "branch", "-q", "-m", "trunk", "master")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动任务") {
		t.Fatalf("认得出之后要接上：\n%s", out)
	}
	if j := e.findJob("认不出的活 READYMERGE"); j == nil {
		t.Fatal("应该起了")
	}
}

// 续跑时认不出默认分支：不起，等着，认得出了接着续跑，中断的记录不丢。
func TestIntegrationResumeWaitsForDefaultBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "resume-app", "- [ ] 中断的活 —— INTERRUPT SLEEP=1")
	if got := "main"; testkit.GitT(t, e.home, dir, "symbolic-ref", "--short", "HEAD") != got {
		t.Fatal("夹具不对")
	}
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断的活").PID, 10*time.Second)
	if out := testkit.GitT(t, e.home, dir, "status", "--porcelain"); out != "" {
		testkit.GitT(t, e.home, dir, "add", "-A")
		testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "认领")
	}
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "main", "trunk")
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "resume-app 不接任务", "这条任务基于 main 分支，现在查不到了")
	st := e.state()
	if len(st.Jobs) != 1 || !st.Jobs[0].ResumePending {
		t.Fatalf("要留着等续跑：%+v", st.Jobs[0])
	}
	testkit.AssertContains(t, e.mustRun("status"), "等续跑：resume-app", "基于 main 分支，现在查不到了")
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "trunk", "main")
	if out := e.mustRun("tick"); !strings.Contains(out, "续跑任务") {
		t.Fatalf("分支回来了要续跑：\n%s", out)
	}
	if res := e.state().Jobs[1]; res.DefaultBranch != "main" {
		t.Errorf("续跑沿用记下的：%q", res.DefaultBranch)
	}
}

// developRepo：main 上什么活都没有；develop 比 main 多一个没发布的提交，活在 develop 的 BACKLOG 里；origin/HEAD 指 develop，主目录在 develop。
func branchRepo(t *testing.T, e *testEnv, name, branch, line string) (dir, mainSHA string) {
	t.Helper()
	dir = gateRepo(t, e, name)
	mainSHA = testkit.GitT(t, e.home, dir, "rev-parse", "main")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", branch)
	b := strings.Replace(e.backlogOf(dir), "## 待做\n", "## 待做\n\n"+line+"\n", 1)
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), b, 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, "unpublished.txt"), "没发布的\n", 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", branch+" 上没发布的提交")
	return dir, mainSHA
}

func setOriginHead(t *testing.T, e *testEnv, dir, branch string) {
	t.Helper()
	head := testkit.GitT(t, e.home, dir, "rev-parse", branch)
	testkit.GitT(t, e.home, dir, "update-ref", "refs/remotes/origin/"+branch, head)
	testkit.GitT(t, e.home, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

// gateRound：tick 收尾或重试，等门进程写结果，再 tick 收结果。at 每次往后拨 5 分钟，过了重试的等待。
func (e *testEnv) gateRound(at *time.Time, title string) *Job {
	e.t.Helper()
	*at = at.Add(5 * time.Minute)
	e.runAt(*at, "tick")
	j := e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateRunning {
		e.t.Fatalf("应该在过门：%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	waitFor(e.t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	*at = at.Add(time.Minute)
	e.runAt(*at, "tick")
	return e.jobByTitle(title)
}

func todayNoon() time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}

// 复现一：origin/HEAD 和主目录都在 develop，有旧 main。任务跑完切到 feature，门等着；用户切到 main 看一眼也不能合进 main；切回 develop 才合，main 一个提交都不多。
func TestIntegrationQueuedStaysOnDevelop(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "发展分支的活 READYMERGE"
	dir, mainSHA := branchRepo(t, e, "dev-app", "develop", "- [ ] "+title+" —— BRANCH=agent/t-d FILE=d.txt")
	setOriginHead(t, e, dir, "develop")
	at := todayNoon()
	e.runAt(at, "tick")
	j := e.jobByTitle(title)
	if j.DefaultBranch != "develop" {
		t.Fatalf("起任务时要记下：%q", j.DefaultBranch)
	}
	waitExit(t, j.PID, 20*time.Second)
	developSHA := testkit.GitT(t, e.home, dir, "rev-parse", "develop")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "feature")

	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateQueued {
		t.Fatalf("主目录在 feature，要等着：%+v %s", j.Gate, j.Summary)
	}
	testkit.AssertContains(t, macLines(t, mac), "主目录不在 develop", "git switch develop")
	status := e.runAt(at, "status")
	testkit.AssertContains(t, status, "这条基于 develop", "git switch develop")
	if strings.Contains(status, "switch main") || strings.Contains(macLines(t, mac), "switch main") {
		t.Fatalf("不能让用户切到 main：\n%s\n%s", status, macLines(t, mac))
	}

	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateQueued || j.Gate.Merge != "" {
		t.Fatalf("主目录在 main，不能合：%+v %s", j.Gate, j.Summary)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "main"); got != mainSHA {
		t.Fatalf("main 动了：\n%s", testkit.GitT(t, e.home, dir, "log", "--oneline", "main"))
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "develop"); got != developSHA {
		t.Fatal("develop 动了")
	}
	if got := macLines(t, mac); strings.Contains(got, "做完了") {
		t.Fatalf("没合并不能说做完：\n%s", got)
	}

	testkit.GitT(t, e.home, dir, "checkout", "-q", "develop")
	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/develop"); err != nil {
		t.Errorf("合并提交不在 develop 上：%v", err)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "main"); got != mainSHA {
		t.Errorf("main 一个提交都不能多：\n%s", testkit.GitT(t, e.home, dir, "log", "--oneline", "main"))
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
	testkit.AssertContains(t, macLines(t, mac), "做完了，已合并")
}

// 复现二：main、master 并存，没有 origin/HEAD，任务基于 master。跑完切到 feature，现认的是 main，但 status 和通知要写 master；照错的切到 main 也不合。
func TestIntegrationQueuedStaysOnMaster(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "两个主线的活 READYMERGE"
	dir, mainSHA := branchRepo(t, e, "two-app", "master", "- [ ] "+title+" —— BRANCH=agent/t-m FILE=m.txt")
	at := todayNoon()
	e.runAt(at, "tick")
	j := e.jobByTitle(title)
	if j.DefaultBranch != "master" {
		t.Fatalf("起任务时要记下：%q", j.DefaultBranch)
	}
	waitExit(t, j.PID, 20*time.Second)
	masterSHA := testkit.GitT(t, e.home, dir, "rev-parse", "master")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "-b", "feature")

	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateQueued {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	status := e.runAt(at, "status")
	testkit.AssertContains(t, status, "这条基于 master", "git switch master", "有任务在等 master", "switch master")
	testkit.AssertContains(t, macLines(t, mac), "主目录不在 master", "git switch master")
	if strings.Contains(status, "switch main") || strings.Contains(macLines(t, mac), "switch main") {
		t.Fatalf("提示不能写 main：\n%s\n%s", status, macLines(t, mac))
	}

	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateQueued || j.Gate.Merge != "" {
		t.Fatalf("主目录在 main，不能合：%+v %s", j.Gate, j.Summary)
	}
	if testkit.GitT(t, e.home, dir, "rev-parse", "main") != mainSHA || testkit.GitT(t, e.home, dir, "rev-parse", "master") != masterSHA {
		t.Fatal("两条分支都不该动")
	}

	testkit.GitT(t, e.home, dir, "checkout", "-q", "master")
	j = e.gateRound(&at, title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/master"); err != nil {
		t.Errorf("合并提交不在 master 上：%v", err)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", "main"); got != mainSHA {
		t.Errorf("main 一个提交都不能多：\n%s", testkit.GitT(t, e.home, dir, "log", "--oneline", "main"))
	}
}

// 同一个项目两条任务基于不同分支：各走各的。master 那条等主目录切回来时，main 上的新任务照常接、合进 main。
func TestIntegrationTwoBasesDontCross(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	onMaster := "基于 master 的活 READYMERGE"
	onMain := "基于 main 的活 READYMERGE"
	dir, _ := branchRepo(t, e, "both", "master", "- [ ] "+onMaster+" —— BRANCH=agent/t-1 FILE=one.txt")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	b := strings.Replace(e.backlogOf(dir), "## 待做\n", "## 待做\n\n- [ ] "+onMain+" —— BRANCH=agent/t-2 FILE=two.txt\n", 1)
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), b, 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "main 上加一条")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "master")

	at := todayNoon()
	e.runAt(at, "tick")
	first := e.jobByTitle(onMaster)
	if first.DefaultBranch != "master" {
		t.Fatalf("%q", first.DefaultBranch)
	}
	waitExit(t, first.PID, 20*time.Second)
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")

	tick := func() {
		at = at.Add(5 * time.Minute)
		e.runAt(at, "tick")
		time.Sleep(150 * time.Millisecond)
	}
	var second *Job
	waitFor(t, 60*time.Second, "main 上的任务合并", func() bool {
		tick()
		second = e.findJob(onMain)
		if second != nil && second.Status == state.StatusRunning {
			waitExit(t, second.PID, 20*time.Second)
		}
		return second != nil && second.Gate != nil && second.Gate.State == state.GateMerged
	})
	if second.DefaultBranch != "main" {
		t.Errorf("新任务记新结果：%q", second.DefaultBranch)
	}
	if j := e.jobByTitle(onMaster); j.Gate == nil || j.Gate.State == state.GateMerged {
		t.Fatalf("主目录在 main，master 那条不能合：%+v", j.Gate)
	}
	testkit.GitT(t, e.home, dir, "checkout", "-q", "master")
	waitFor(t, 60*time.Second, "master 上的任务合并", func() bool {
		tick()
		j := e.jobByTitle(onMaster)
		return j.Gate != nil && j.Gate.State == state.GateMerged
	})
	inBranch := func(branch, file string) bool {
		return testkit.GitTErr(e.home, dir, "cat-file", "-e", branch+":"+file) == nil
	}
	if !inBranch("main", "two.txt") || inBranch("main", "one.txt") || !inBranch("master", "one.txt") || inBranch("master", "two.txt") {
		t.Errorf("串了：main 有 two=%v one=%v，master 有 one=%v two=%v", inBranch("main", "two.txt"), inBranch("main", "one.txt"), inBranch("master", "one.txt"), inBranch("master", "two.txt"))
	}
}

// 老任务（state.json 里没有 default_branch）排着队：照常现认，照常合并。
func TestIntegrationOldJobWithoutBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "老任务 READYMERGE"
	dir := gateRepo(t, e, "old-job", "- [ ] "+title+" —— BRANCH=agent/t-o FILE=o.txt")
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "main", "master")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.editState(func(st *State) {
		for _, x := range st.Jobs {
			x.DefaultBranch = ""
		}
	})
	b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "state.json"))
	if err != nil || strings.Contains(string(b), "default_branch") {
		t.Fatalf("夹具不对：%v", err)
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateRunning {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if err := testkit.GitTErr(e.home, dir, "merge-base", "--is-ancestor", j.Gate.Merge, "refs/heads/master"); err != nil {
		t.Errorf("%v", err)
	}
}

// 续跑沿用记下的分支：主目录切到 main（项目层面能接新任务），这一条还是等 develop。
func TestIntegrationResumeKeepsRecordedBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir, _ := branchRepo(t, e, "resume3", "develop", "- [ ] 中断的活 —— INTERRUPT SLEEP=1")
	setOriginHead(t, e, dir, "develop")
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断的活").PID, 10*time.Second)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "认领")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main")
	testkit.AssertContains(t, e.mustRun("tick"), "resume3 不接任务", "任务基于 develop", "switch develop")
	st := e.state()
	if len(st.Jobs) != 1 || !st.Jobs[0].ResumePending {
		t.Fatalf("续跑记录要留着，不能因为 main 的 BACKLOG 里没有这一条就放掉：%+v", st.Jobs[0])
	}
	testkit.AssertContains(t, e.mustRun("status"), "等续跑：resume3", "git -C", "switch develop")
	testkit.GitT(t, e.home, dir, "checkout", "-q", "develop")
	testkit.GitT(t, e.home, dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD") // 现认现在认的是 main：沿用记下的才续跑得上
	if out := e.mustRun("tick"); !strings.Contains(out, "续跑任务") {
		t.Fatalf("切回去要续跑：\n%s", out)
	}
	res := e.state().Jobs[1]
	if res.DefaultBranch != "develop" {
		t.Errorf("续跑沿用记下的：%q", res.DefaultBranch)
	}
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", res.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "默认分支是 develop")
}

// 任务结束时主目录切走了：照样认得出这一条、排上门（读记下那条分支上的 BACKLOG）。
func TestIntegrationFinishWhileHeadAway(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	title := "结束时切走了 READYMERGE"
	dir, _ := branchRepo(t, e, "away2", "master", "- [ ] "+title+" —— BRANCH=agent/t-w FILE=w.txt")
	at := todayNoon()
	e.runAt(at, "tick")
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	testkit.GitT(t, e.home, dir, "checkout", "-q", "main") // main 的 BACKLOG 里没有这一条
	e.runAt(at.Add(time.Minute), "tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.Branch != "agent/t-w" {
		t.Fatalf("要排上门：%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool { // 门进程没人等：用例结束后还在写临时 HOME
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
}

// 接手沿用上一个任务记下的分支：现认认不出（主线叫 develop，没有 origin/HEAD）也接得上。
func TestIntegrationTakeoverKeepsRecordedBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— NOMERGE"+oldMarker("agent/x")+"\n")
	gitBranch(t, e, dir, "agent/x")
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "main", "develop")
	ended := time.Now()
	e.editState(func(st *State) {
		st.Jobs = append(st.Jobs, &Job{ID: "0101-000000-demo", Project: "demo", ProjectDir: dir, Title: "甲", DefaultBranch: "develop",
			Status: state.StatusFailed, StartedAt: ended.Add(-time.Hour), EndedAt: &ended})
	})
	if out := e.tickAt(time.Now().Add(72*time.Hour), 0); !strings.Contains(out, "接手任务") {
		t.Fatalf("应启动接手任务：\n%s", out)
	}
	j := e.jobByTitle("甲")
	var takeover *Job
	for _, x := range e.state().Jobs {
		if x.Takeover {
			takeover = x
		}
	}
	if takeover == nil || takeover.DefaultBranch != "develop" {
		t.Fatalf("%+v（%s）", takeover, j.ID)
	}
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", takeover.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "默认分支是 develop")
}

func (e *testEnv) claudeCallsOf(text string) int {
	e.t.Helper()
	n := 0
	for _, c := range e.claudeCalls() {
		if strings.Contains(c, text) {
			n++
		}
	}
	return n
}

// branchApp 是一个进程内的 App，加一个 projects_root 下的真仓库。
func branchApp(t *testing.T, branch string) (*App, bl.Project) {
	t.Helper()
	home := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(home, ".agentq", "config.json"), "{}\n", 0o600)
	testkit.WriteInstall(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n", 0o644)
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg.ProjectsRoot = filepath.Join(home, "projects")
	dir := testkit.MkProject(t, a.ProjectsRoot(), "demo", "- [ ] 甲 —— 要什么\n")
	os.RemoveAll(filepath.Join(dir, ".git"))
	testkit.GitT(t, home, dir, "init", "-q", "-b", branch)
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	return a, bl.Project{Name: "demo", Dir: dir}
}

func addTodoLine(old string) (string, error) {
	text, _ := bl.InsertTodo(old, "- [ ] 新的一条")
	return text, nil
}

func TestCommitBacklogFollowsDefaultBranch(t *testing.T) {
	t.Parallel()
	t.Run("master 上能提交", func(t *testing.T) {
		a, p := branchApp(t, "master")
		if err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine); err != nil {
			t.Fatal(err)
		}
		if got := testkit.GitT(t, a.Home, p.Dir, "log", "-1", "--format=%s", "master"); got != "待办：加" {
			t.Fatal(got)
		}
	})
	t.Run("主目录不在默认分支", func(t *testing.T) {
		a, p := branchApp(t, "master")
		testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
		err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine)
		if err == nil || !strings.Contains(err.Error(), "主目录不在 master（feature）") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("认不出就不写", func(t *testing.T) {
		a, p := branchApp(t, "trunk")
		before := testkit.ReadFile(t, p.BacklogPath())
		if err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine); !errors.Is(err, gitx.ErrNoDefaultBranch) {
			t.Fatalf("%v", err)
		}
		if testkit.ReadFile(t, p.BacklogPath()) != before {
			t.Fatal("不该改文件")
		}
	})
	t.Run("HEAD 在 main 时 origin/HEAD 指别的也用 main", func(t *testing.T) {
		a, p := branchApp(t, "main")
		head := testkit.GitT(t, a.Home, p.Dir, "rev-parse", "HEAD")
		testkit.GitT(t, a.Home, p.Dir, "branch", "develop")
		testkit.GitT(t, a.Home, p.Dir, "update-ref", "refs/remotes/origin/develop", head)
		testkit.GitT(t, a.Home, p.Dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
		if err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine); err != nil {
			t.Fatal(err)
		}
		if got := testkit.GitT(t, a.Home, p.Dir, "log", "-1", "--format=%s", "main"); got != "待办：加" {
			t.Fatal(got)
		}
		testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
		err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine)
		if err == nil || !strings.Contains(err.Error(), "主目录不在 develop（feature）") {
			t.Fatalf("在 feature 上时 origin/HEAD 说了算：%v", err)
		}
		testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "develop")
		if err := a.Backlog.CommitBacklog(p, "待办：加", addTodoLine); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOrphanFactsDefaultBranch(t *testing.T) {
	t.Parallel()
	a, p := branchApp(t, "master")
	wt := filepath.Join(t.TempDir(), "wt")
	testkit.GitT(t, a.Home, p.Dir, "worktree", "add", "-q", wt, "-b", "agent/x", "master")
	testkit.GitT(t, a.Home, wt, "commit", "-q", "--allow-empty", "-m", "活")
	f, err := a.Backlog.OrphanFacts(p, bl.Item{Text: "甲（进行中：agent/x 10-10）"}, time.Time{})
	if err != nil || f.Default != "master" || f.Branch != "agent/x" || f.Ahead != 1 {
		t.Fatalf("%+v %v", f, err)
	}
	f, err = a.Backlog.OrphanFacts(p, bl.Item{Text: "甲（进行中：master 10-10）"}, time.Time{})
	if err != nil || f.Named != "" || f.Branch != "" {
		t.Fatalf("默认分支不是任务分支：%+v %v", f, err)
	}
	// 默认分支叫别的时，标记写它也不算任务分支
	a2, p2 := branchApp(t, "develop")
	head := testkit.GitT(t, a2.Home, p2.Dir, "rev-parse", "HEAD")
	testkit.GitT(t, a2.Home, p2.Dir, "update-ref", "refs/remotes/origin/develop", head)
	testkit.GitT(t, a2.Home, p2.Dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	f, err = a2.Backlog.OrphanFacts(p2, bl.Item{Text: "甲（进行中：develop 10-10）"}, time.Time{})
	if err != nil || f.Named != "" || f.Branch != "" {
		t.Fatalf("默认分支不是任务分支：%+v %v", f, err)
	}
	a3, p3 := branchApp(t, "trunk")
	if _, err := a3.Backlog.OrphanFacts(p3, bl.Item{Text: "甲（进行中：agent/x 10-10）"}, time.Time{}); !errors.Is(err, gitx.ErrNoDefaultBranch) {
		t.Fatalf("认不出要报错，调用方当未知：%v", err)
	}
}

func TestSuggestUnwritableDefaultBranch(t *testing.T) {
	t.Parallel()
	a, p := branchApp(t, "master")
	if why := a.Suggest.SuggestUnwritable(p); why != "" {
		t.Fatalf("master 上能写：%s", why)
	}
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
	if why := a.Suggest.SuggestUnwritable(p); why != "主目录不在 master（feature）" {
		t.Fatal(why)
	}
	a2, p2 := branchApp(t, "trunk")
	if why := a2.Suggest.SuggestUnwritable(p2); !strings.Contains(why, "认不出默认分支") {
		t.Fatal(why)
	}
}

func TestCommitNotDoingOnMaster(t *testing.T) {
	t.Parallel()
	a, p := branchApp(t, "master")
	if err := a.Suggest.CommitNotDoing(p, "待办：不做", &bl.NotDoingEntry{Title: "乙"}, func(old string) (string, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}
	if got := testkit.GitT(t, a.Home, p.Dir, "log", "-1", "--format=%s", "master"); got != "待办：不做" {
		t.Fatal(got)
	}
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
	err := a.Suggest.CommitNotDoing(p, "待办：不做", &bl.NotDoingEntry{Title: "丙"}, func(old string) (string, error) { return old, nil })
	if err == nil || !strings.Contains(err.Error(), "主目录不在 master（feature）") {
		t.Fatalf("%v", err)
	}
}

// 接入：master 不用改分支，没提交的 master 也行；在别的分支上给切回去的命令。
func TestOnboardDefaultBranch(t *testing.T) {
	a, _, _, out := onboardApp(t)
	t.Run("master 仓库能 init", func(t *testing.T) {
		dir := onboardRepo(t, a, "init-master", true)
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "master")
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		if got := onboardGit(t, a.Home, dir, "log", "-1", "--format=%D", "HEAD"); !strings.Contains(got, "master") {
			t.Fatal(got)
		}
		if !exists(filepath.Join(dir, "docs", "BACKLOG.md")) {
			t.Fatal("要生成 BACKLOG")
		}
	})
	t.Run("没提交的 master 仓库能 init", func(t *testing.T) {
		dir := filepath.Join(a.ProjectsRoot(), "unborn-master")
		os.MkdirAll(dir, 0o755)
		onboardGit(t, a.Home, dir, "init", "-q", "-b", "master")
		onboardGit(t, a.Home, dir, "config", "user.name", "t")
		onboardGit(t, a.Home, dir, "config", "user.email", "t")
		if err := a.Onboard.CmdInit([]string{dir}); err != nil {
			t.Fatal(err)
		}
		if got := onboardGit(t, a.Home, dir, "symbolic-ref", "--short", "HEAD"); got != "master" {
			t.Fatal(got)
		}
	})
	t.Run("在别的分支上", func(t *testing.T) {
		dir := onboardRepo(t, a, "on-feature-master", true)
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "master")
		onboardGit(t, a.Home, dir, "checkout", "-q", "-b", "feature")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "默认分支是 master", "switch master")
	})
	t.Run("有 origin 时多给一条路", func(t *testing.T) {
		dir := onboardRepo(t, a, "with-origin", true)
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "trunk")
		onboardGit(t, a.Home, dir, "remote", "add", "origin", "https://example.invalid/x.git")
		err := a.Onboard.CmdInit([]string{dir})
		if err == nil {
			t.Fatal("应拒绝")
		}
		testkit.AssertContains(t, err.Error(), "认不出默认分支", "branch -m", "remote set-head origin -a")
	})
	t.Run("外面的 master 仓库能 add", func(t *testing.T) {
		out.Reset()
		dir := outsideRepo(t, a, "far-master")
		onboardGit(t, a.Home, dir, "branch", "-m", "main", "master")
		t.Chdir(dir)
		if err := a.Onboard.CmdAdd([]string{"第一条"}); err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, out.String(), "已登记到 extra_projects", "已接入 far-master", "已加到 far-master")
		if got := extraInFile(t, a); !slices.Contains(got, dir) {
			t.Fatalf("%v", got)
		}
		if got := onboardGit(t, a.Home, dir, "symbolic-ref", "--short", "HEAD"); got != "master" {
			t.Fatal(got)
		}
	})
}
