package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 任务和门各用自己的临时目录，收尾就清（真二进制、假 claude）。
// 项目名每个用例一个，别的文件也不用：任务号是「日期时分秒-项目名」，并行的用例同一秒起同名项目会撞号，清理时把对方的进程当成还在用这个目录。

// tmpCaller 给 e 一个放了东西的 TMPDIR，末尾带斜杠（和 macOS 默认的一样）。用例结束时里面只能剩原来的东西，一个不少。
func tmpCaller(t *testing.T, e *testEnv) string {
	t.Helper()
	caller := t.TempDir()
	keep := filepath.Join(caller, "agq别人的", "文件") // 名字像 agentq 建的，没登记过就不是
	os.MkdirAll(filepath.Dir(keep), 0o755)
	os.WriteFile(keep, []byte("别动"), 0o644)
	os.Mkdir(filepath.Join(caller, "huitai-fuzz-1"), 0o755)
	e.extra = append(e.extra, "TMPDIR="+caller+"/")
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", caller).Run() }) // 没清掉的用例失败，也要让 TempDir 删得掉
	return caller
}

func tmpOwned(t *testing.T, caller string) []string {
	t.Helper()
	ents, err := os.ReadDir(caller)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	for _, want := range []string{"agq别人的", "huitai-fuzz-1"} {
		if !slices.Contains(names, want) {
			t.Errorf("调用方的 %s 没了：%v", want, names)
		}
	}
	if b, err := os.ReadFile(filepath.Join(caller, "agq别人的", "文件")); err != nil || string(b) != "别动" {
		t.Errorf("调用方原有的文件被动了：%v %q", err, b)
	}
	return slices.DeleteFunc(names, func(n string) bool { return n == "agq别人的" || n == "huitai-fuzz-1" })
}

// tmpClean：收尾之后目录、登记都没了，调用方原有的一个不少。
func (e *testEnv) tmpClean(caller string) {
	e.t.Helper()
	if left := tmpOwned(e.t, caller); len(left) > 0 {
		e.t.Errorf("收尾后还留着 %v\n%s", left, e.launchdLog())
	}
	if recs, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "tmp", "*.json*")); len(recs) > 0 {
		e.t.Errorf("登记还在：%v", recs)
	}
}

var envTmpRe = regexp.MustCompile(`ENV\[TMPDIR=([^\]]*)\]`)

// cliTmp 是这个 pid 的假 claude 拿到的 TMPDIR（去掉末尾斜杠）。
func (e *testEnv) cliTmp(pid int) string {
	e.t.Helper()
	var dir string
	waitFor(e.t, 10*time.Second, "假 claude 记下环境", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", pid)))
		if m := envTmpRe.FindSubmatch(b); m != nil {
			dir = string(m[1])
		}
		return dir != ""
	})
	if !strings.HasSuffix(dir, "/") {
		e.t.Errorf("调用方的 TMPDIR 带斜杠，子进程的也要带：%s", dir)
	}
	return strings.TrimSuffix(dir, "/")
}

// 正常结束：两个任务各用各的，任务自己不清的文件、没写权限的目录收尾时一起清。
func TestIntegrationTmpdirDone(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp1-alpha", "- [ ] 甲 —— TMPWRITE SLEEP=2\n")
	testkit.MkProject(t, e.root, "tmp1-beta", "- [ ] 乙 —— TMPWRITE SLEEP=2\n")
	e.mustRun("tick")
	a, b := e.jobByTitle("甲"), e.jobByTitle("乙")
	ta, tb := e.cliTmp(a.PID), e.cliTmp(b.PID)
	if ta == tb || filepath.Dir(ta) != caller || filepath.Dir(tb) != caller {
		t.Fatalf("各用各的，都不是调用方的 %s：%s %s", caller, ta, tb)
	}
	e.mustRun("tick") // 在跑的不能动
	if !exists(ta) || !exists(tb) {
		t.Fatal("任务还在跑，目录不能清")
	}
	waitExit(t, a.PID, 15*time.Second)
	waitExit(t, b.PID, 15*time.Second)
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 任务结束后的自动 tick（外壳变的）当场就清。
func TestIntegrationTmpdirAfterJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"tick_after_job": true})
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp2-alpha", "- [ ] 甲 —— TMPWRITE SLEEP=1\n")
	e.mustRun("tick")
	dir := e.cliTmp(e.jobByTitle("甲").PID)
	waitFor(t, 30*time.Second, "自动 tick 清掉", func() bool { return !exists(dir) })
	waitFor(t, 10*time.Second, "记日志", func() bool { return strings.Contains(e.launchdLog(), "用完了，清了 "+dir) })
	e.tmpClean(caller)
}

// 超时和被停：进程杀掉之后清。
func TestIntegrationTmpdirTimeoutAndStop(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "2s"})
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp3-alpha", "- [ ] 超时 —— TMPWRITE SLEEP=60\n")
	testkit.MkProject(t, e.root, "tmp3-beta", "- [ ] 被停 —— TMPWRITE SLEEP=60\n")
	e.mustRun("tick")
	late, stop := e.jobByTitle("超时"), e.jobByTitle("被停")
	tl, ts := e.cliTmp(late.PID), e.cliTmp(stop.PID)
	e.mustRun("stop", stop.ID)
	e.mustRun("tick") // 停掉的这次 tick 清；另一个可能还没到时限
	if exists(ts) {
		t.Errorf("被停的要清\n%s", e.launchdLog())
	}
	time.Sleep(2500 * time.Millisecond)
	e.mustRun("tick")
	if got := e.jobByTitle("超时").Status; got != state.StatusTimeout {
		t.Fatalf("%s", got)
	}
	if got := e.jobByTitle("被停").Status; got != state.StatusKilled {
		t.Fatalf("%s", got)
	}
	if exists(tl) {
		t.Error("超时的要清")
	}
	e.tmpClean(caller)
}

// 中断：等续跑的不清，续跑接着用同一个（会话记得自己写在里面的文件），做完才清。
func TestIntegrationTmpdirResumeKeeps(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp4-alpha", "- [ ] 做到一半撞上限 —— LIMITMID TMPWRITE\n")
	e.mustRun("tick")
	orig := e.jobByTitle("做到一半撞上限")
	dir := e.cliTmp(orig.PID)
	waitExit(t, orig.PID, 10*time.Second)
	for range 2 { // 冷却中，等着续跑
		e.mustRun("tick")
	}
	st := e.state()
	if j, err := st.Find(orig.ID); err != nil || !j.ResumePending {
		t.Fatalf("应该在等续跑：%+v %v", j, err)
	}
	if !exists(filepath.Join(dir, "d", "ro", "f")) {
		t.Fatalf("可续跑的不能清\n%s", e.launchdLog())
	}
	st.BackendLimits = nil // 冷却过了
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	var res *Job
	for _, x := range e.state().Jobs {
		if x.ResumeOf == orig.ID {
			res = x
		}
	}
	if res == nil {
		t.Fatalf("应该续跑了：%s", e.launchdLog())
	}
	if got := e.cliTmp(res.PID); got != dir {
		t.Errorf("续跑接着用 %s，拿到的是 %s", dir, got)
	}
	waitExit(t, res.PID, 10*time.Second)
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 中断：续跑完还中断、等用户的，条目还在进行中就能用同一会话接着做，不清；条目了结了才清。中断了也接不回去的当场清。
func TestIntegrationTmpdirInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	alpha := testkit.MkProject(t, e.root, "tmp5-alpha", "- [ ] 再中断 —— INTERRUPT AGAIN TMPWRITE\n")
	testkit.MkProject(t, e.root, "tmp5-beta", "- [ ] 没写合并 —— NOMERGE TMPWRITE\n")
	e.mustRun("tick")
	first, none := e.jobByTitle("再中断"), e.jobByTitle("没写合并")
	dir, gone := e.cliTmp(first.PID), e.cliTmp(none.PID)
	waitExit(t, first.PID, 10*time.Second)
	waitExit(t, none.PID, 10*time.Second)
	e.mustRun("tick")
	if got := e.jobByTitle("没写合并").Status; got != state.StatusInterrupted || exists(gone) {
		t.Errorf("中断了也接不回去的要清：%s %v", got, exists(gone))
	}
	var res *Job
	for _, x := range e.state().Jobs {
		if x.ResumeOf == first.ID {
			res = x
		}
	}
	if res == nil {
		t.Fatalf("应该续跑了：%s", e.launchdLog())
	}
	waitExit(t, res.PID, 10*time.Second)
	e.mustRun("tick")
	e.mustRun("tick")
	if !exists(dir) {
		t.Fatalf("还能接着做的不能清\n%s", e.launchdLog())
	}
	b := filepath.Join(alpha, "docs", "BACKLOG.md")
	text := testkit.ReadFile(t, b)
	os.WriteFile(b, []byte(regexp.MustCompile(`(?m)^- \[ \] 再中断.*$`).ReplaceAllString(text, "- [x] 再中断")), 0o644)
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 等用户定：任务标了卡住、交给用户的不清；用户了结这一条之后清。
func TestIntegrationTmpdirStuckKeeps(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	dir := gateRepo(t, e, "tmp6-demo", "- [ ] 难活 —— STUCKRETAKE STUCKUSER TMPWRITE")
	e.mustRun("tick")
	j := e.jobByTitle("难活")
	tmp := e.cliTmp(j.PID)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	e.mustRun("tick")
	if !exists(tmp) {
		t.Fatalf("等用户定的不能清\n%s", e.launchdLog())
	}
	b := filepath.Join(dir, "docs", "BACKLOG.md")
	text := testkit.ReadFile(t, b)
	os.WriteFile(b, []byte(regexp.MustCompile(`(?m)^- \[ \] 难活.*$`).ReplaceAllString(text, "")), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "不做了")
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 引擎起任务时被杀：下次 tick 认领，任务在跑时不清，结束后补清。
func TestIntegrationTmpdirEngineKilled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp7-alpha", "- [ ] 启动窗口 —— KILLTICK TMPWRITE SLEEP=4\n")
	e.run("tick") // 被假 claude 杀掉
	e.mustRun("tick")
	j := e.jobByTitle("启动窗口")
	dir := e.cliTmp(j.PID)
	if filepath.Dir(dir) != caller || !exists(dir) {
		t.Fatalf("在跑的不能清：%s\n%s", dir, e.launchdLog())
	}
	// 外壳也没了：没有人在任务结束时收尾，只剩下次 tick
	shell := 0
	b := testkit.ReadFile(t, filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j.PID)))
	if m := regexp.MustCompile(`PPID\[(\d+)\]`).FindStringSubmatch(b); m != nil {
		fmt.Sscan(m[1], &shell)
	}
	if shell <= 1 {
		t.Fatalf("外壳 pid：%d", shell)
	}
	syscall.Kill(shell, syscall.SIGKILL)
	waitExit(t, j.PID, 15*time.Second)
	if !exists(dir) {
		t.Fatal("还没 tick，谁清的")
	}
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 任务里执行 agentq run：新任务的目录不建在这个任务的里面，这个任务收尾时不会把它的删掉。
func TestIntegrationTmpdirRunInsideJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	testkit.MkProject(t, e.root, "tmp8-alpha", "- [ ] 外面 —— SLEEP=4\n")
	testkit.MkProject(t, e.root, "tmp8-beta", "- [ ] 里面 —— SLEEP=8 TMPWRITE\n")
	e.mustRun("run", "tmp8-alpha")
	outer := e.jobByTitle("外面")
	dir := e.cliTmp(outer.PID)
	saved := e.extra
	e.extra = append(slices.Clone(saved), "TMPDIR="+dir+"/")
	e.mustRun("run", "tmp8-beta")
	e.extra = saved
	inner := e.jobByTitle("里面")
	if got := e.cliTmp(inner.PID); filepath.Dir(got) != caller {
		t.Fatalf("应该建在 %s 里：%s", caller, got)
	}
	waitExit(t, outer.PID, 15*time.Second)
	e.mustRun("tick")
	if exists(dir) {
		t.Errorf("外面的要清\n%s", e.launchdLog())
	}
	if got := e.cliTmp(inner.PID); !exists(filepath.Join(got, "d", "ro", "f")) {
		t.Error("里面的还在跑，不能动")
	}
	waitExit(t, inner.PID, 20*time.Second)
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 门：测试脚本、审查各用自己的，和任务的不是一个，跑完当场清（不等 tick）。卡在门上等用户定时任务的留着，用户放弃了才清。
func TestIntegrationTmpdirGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	verifyRepo(t, e, "tmp9-demo", `#!/bin/sh
echo "$TMPDIR" >> "$HOME/门的TMPDIR"
mkdir -p "${TMPDIR}v/ro"; echo x > "${TMPDIR}v/ro/f"; chmod 555 "${TMPDIR}v/ro"
`, "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt TMPWRITE")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	task := e.cliTmp(j.PID)
	waitExit(t, j.PID, 20*time.Second)
	before, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	e.mustRun("tick")
	waitFor(t, 60*time.Second, "门的结果", func() bool { return exists(e.gateResult(e.jobByTitle(title))) })
	if left := tmpOwned(t, caller); !slices.Equal(left, []string{filepath.Base(task)}) {
		t.Errorf("门跑完只剩任务自己的 %s（还没有结论）：%v", task, left)
	}
	dirs := strings.Fields(testkit.ReadFile(t, filepath.Join(e.home, "门的TMPDIR")))
	after, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	for _, f := range after {
		if !slices.Contains(before, f) { // 审查进程
			if m := envTmpRe.FindStringSubmatch(testkit.ReadFile(t, f)); m != nil {
				dirs = append(dirs, m[1])
			}
		}
	}
	if len(dirs) < 2 {
		t.Fatalf("测试脚本和审查都要有：%v", dirs)
	}
	seen := map[string]bool{task + "/": true}
	for _, d := range dirs {
		if seen[d] || filepath.Dir(strings.TrimSuffix(d, "/")) != caller {
			t.Errorf("每次运行各用各的：%v", dirs)
		}
		seen[d] = true
	}
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	e.tmpClean(caller)
}

func TestIntegrationTmpdirGateStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	verifyRepo(t, e, "tmp10-demo", "#!/bin/sh\nexit 125\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt TMPWRITE")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	task := e.cliTmp(e.jobByTitle(title).PID)
	j := e.tickUntilGate(title)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	e.mustRun("tick")
	if left := tmpOwned(t, caller); !slices.Equal(left, []string{filepath.Base(task)}) {
		t.Fatalf("等用户定，任务的留着，门的清了：%v\n%s", left, e.launchdLog())
	}
	e.mustRun("gate", "abandon", j.ID)
	e.mustRun("tick")
	// 放弃后条目还标着卡住，仍是等用户：去掉这一条才算了结
	b := filepath.Join(e.root, "tmp10-demo", "docs", "BACKLOG.md")
	text := testkit.ReadFile(t, b)
	os.WriteFile(b, []byte(regexp.MustCompile(`(?m)^- \[ \] 修东西.*$`).ReplaceAllString(text, "")), 0o644)
	testkit.GitT(t, e.home, filepath.Join(e.root, "tmp10-demo"), "commit", "-q", "-am", "不做了")
	e.mustRun("tick")
	e.tmpClean(caller)
}

// 门脚本超时、门进程被 SIGTERM：门自己清。
func TestIntegrationTmpdirGateTimeoutAndTerm(t *testing.T) {
	t.Parallel()
	script := "#!/bin/sh\ntouch \"${TMPDIR}f\"\ntouch \"$HOME/脚本开始了\"\nsleep 300\n"
	start := func(t *testing.T, cfg map[string]any) (*testEnv, string, *Job) {
		e := newEnv(t, cfg)
		caller := tmpCaller(t, e)
		verifyRepo(t, e, "tmp11-demo", script, "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
		e.mustRun("tick")
		waitExit(t, e.jobByTitle("修东西 READYMERGE").PID, 20*time.Second)
		e.mustRun("tick")
		waitFor(t, 30*time.Second, "脚本开始", func() bool { return exists(filepath.Join(e.home, "脚本开始了")) })
		return e, caller, e.jobByTitle("修东西 READYMERGE")
	}
	taskOnly := func(t *testing.T, e *testEnv, caller string, j *Job) {
		t.Helper()
		if left := tmpOwned(t, caller); !slices.Equal(left, []string{filepath.Base(e.cliTmp(j.PID))}) {
			t.Errorf("门的要清，任务的还在排队过门、留着：%v", left)
		}
	}
	t.Run("超时", func(t *testing.T) {
		e, caller, j := start(t, map[string]any{"verify_timeout": "3s"})
		waitFor(t, 60*time.Second, "门的结果", func() bool { return exists(e.gateResult(j)) })
		taskOnly(t, e, caller, j)
	})
	t.Run("被 SIGTERM", func(t *testing.T) {
		e, caller, j := start(t, nil)
		if err := syscall.Kill(j.Gate.PID, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		waitExit(t, j.Gate.PID, 30*time.Second)
		taskOnly(t, e, caller, j)
	})
}

// 门进程被 SIGKILL：脚本还在跑时不清，下次 tick 终止脚本之后补清。
func TestIntegrationTmpdirGateKilled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	caller := tmpCaller(t, e)
	verifyRepo(t, e, "tmp12-demo", "#!/bin/sh\n[ -f \"$HOME/放行\" ] && exit 0\necho \"$TMPDIR\" > \"$HOME/门的TMPDIR.part\"\nmv \"$HOME/门的TMPDIR.part\" \"$HOME/门的TMPDIR\"\nsleep 300\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	waitExit(t, e.jobByTitle(title).PID, 20*time.Second)
	e.mustRun("tick")
	j := e.jobByTitle(title)
	mark := filepath.Join(e.home, "门的TMPDIR")
	waitFor(t, 30*time.Second, "脚本开始", func() bool { return exists(mark) })
	dir := strings.TrimSpace(testkit.ReadFile(t, mark))
	if err := syscall.Kill(j.Gate.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "门进程退出", func() bool { return !proc.ProcAlive(j.Gate.PID, gate.ProcToken+" "+j.ID) })
	if !exists(dir) {
		t.Fatal("没人清")
	}
	os.WriteFile(filepath.Join(e.home, "放行"), nil, 0o644)
	e.mustRun("tick") // 终止脚本，重跑一次门
	waitFor(t, 60*time.Second, "门的结果", func() bool { return exists(e.gateResult(j)) })
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	e.tmpClean(caller)
}
