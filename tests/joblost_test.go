package tests

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
)

func lostJob(a *App, id string) *Job {
	os.MkdirAll(a.JobDir(id), 0o755)
	return &Job{ID: id, Project: "alpha", Status: state.StatusRunning, StartedAt: a.Now().Add(-time.Hour), SessionID: "sess-" + id}
}

func fastLookup(t *testing.T) {
	t.Helper()
	tries, gap := runner.JobLookupTries, runner.JobLookupGap
	runner.JobLookupTries, runner.JobLookupGap = 2, time.Millisecond
	t.Cleanup(func() { runner.JobLookupTries, runner.JobLookupGap = tries, gap })
}

// 确认没有进程：才记启动失败。
func TestFindLostClaudeNothingThere(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	if pid, err := a.Runner.FindLostClaude(lostJob(a, "1104-000001-alpha")); err != nil || pid != 0 {
		t.Fatalf("%d %v", pid, err)
	}
}

// 环境标记认得出（真 claude 的情况）；别的任务的标记不算；只有环境标记、没有 --name 的不是 claude，是要清掉的子进程。
func TestFindLostClaudeByEnv(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	child := lostJob(a, "1104-000012-alpha")
	group := startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + child.ID}, os.Args[0], "-test.run=^TestHelperSleep$")
	if got, err := a.Runner.FindLostClaude(child); err != nil || got != 0 {
		t.Fatalf("没有 --name 的不算 claude：%d %v", got, err)
	}
	if !proc.WaitGroupGone(group, 8*time.Second) {
		t.Fatal("只剩子进程时应清掉")
	}
	j := lostJob(a, "1104-000002-alpha")
	pid := startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + j.ID}, os.Args[0], "-test.run=^TestHelperSleep$", "--", proc.NameMarker(j.Project)+"甲")
	if got, err := a.Runner.FindLostClaude(j); err != nil || got != pid {
		t.Fatalf("应认出 %d：%d %v", pid, got, err)
	}
	other := lostJob(a, "1104-000003-alpha")
	if got, err := a.Runner.FindLostClaude(other); err != nil || got != 0 {
		t.Fatalf("别的任务的进程不算：%d %v", got, err)
	}
}

// 命令行认得出（sh 包装脚本，macOS 上看不到环境）；只有会话 id 或只有 --name 都不算。
func TestFindLostClaudeByCommandLine(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	j := lostJob(a, "1104-000004-alpha")
	script := "sleep 30; :"
	both := startGroup(t, nil, "/bin/sh", "-c", script, "--session-id", j.SessionID, "--name", proc.NameMarker(j.Project)+"甲")
	if got, err := a.Runner.FindLostClaude(j); err != nil || got != both {
		t.Fatalf("应认出 %d：%d %v", both, got, err)
	}
	k := lostJob(a, "1104-000005-alpha")
	startGroup(t, nil, "/bin/sh", "-c", script, "--session-id", k.SessionID)                    // 用户手动 claude -r 同一个会话
	startGroup(t, nil, "/bin/sh", "-c", script, "--name", proc.NameMarker(k.Project)+"同项目别的任务") // 同项目别的任务
	if got, err := a.Runner.FindLostClaude(k); err != nil || got != 0 {
		t.Fatalf("证据不够不算：%d %v", got, err)
	}
}

// 查不了进程：不能下结论，start 留成运行中，reap 也不收尾；恢复后接着查。
func TestLostClaudeUnknownIsNotFailure(t *testing.T) {
	a, log := lostShellEnv(t)
	real := runner.ListProcs
	runner.ListProcs = func() ([]runner.JobProc, error) { return nil, errors.New("ps 坏了") }
	t.Cleanup(func() { runner.ListProcs = real })
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	waitForClaudes(t, log, 1)
	st, err := state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 1 || st.Jobs[0].Status != state.StatusRunning || st.Jobs[0].PID != 0 {
		t.Fatalf("查不了就当它还在：%s", fmtJobs(st))
	}
	// 过了报 pid 的时限，还是查不了：不收尾、不接新的
	later := time.Now().Add(runner.ShellReportWait + time.Minute)
	a.Now = func() time.Time { return later }
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	if live := liveClaudes(t, log); len(live) != 1 {
		t.Fatalf("不该接新的：%v", live)
	}
	if st, _ = state.Load(a.StatePath()); st.Jobs[0].Status != state.StatusRunning {
		t.Fatalf("不该收尾：%s", fmtJobs(st))
	}
	// 恢复：认回 pid
	runner.ListProcs = real
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	if st, _ = state.Load(a.StatePath()); st.Jobs[0].PID != lostShellPids(t, log)[0] {
		t.Fatalf("应认回 pid：%s", fmtJobs(st))
	}
}

// 启动方被杀、没有 claude.pid：reap 过了时限也要找，claude 在就接回来，不在才记中断。
func TestReapAdoptsLostClaude(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	j := lostJob(a, "1104-000006-alpha")
	pid := startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + j.ID}, os.Args[0], "-test.run=^TestHelperSleep$", "--", proc.NameMarker(j.Project)+"甲")
	st := &State{Jobs: []*Job{j}}
	if n := a.Runner.Reap(st); n != 0 {
		t.Fatalf("还活着不该收尾：%d %s", n, fmtJobs(st))
	}
	if j.PID != pid || j.Status != state.StatusRunning {
		t.Fatalf("应接回 %d：%s", pid, fmtJobs(st))
	}

	gone := lostJob(a, "1104-000007-alpha")
	st = &State{Jobs: []*Job{gone}}
	if n := a.Runner.Reap(st); n != 1 || gone.Status != state.StatusFailed || !strings.Contains(gone.Summary, "已确认") {
		t.Fatalf("没有进程才记中断：%d %s", n, fmtJobs(st))
	}
}

// lostRunningJob：外壳报 pid 前死掉、当时 ps 也查不了：任务记运行中、PID 0，claude 活着。
func lostRunningJob(t *testing.T) (*App, string, *State, func(error)) {
	t.Helper()
	a, log := lostShellEnv(t)
	real := runner.ListProcs
	var broken error
	runner.ListProcs = func() ([]runner.JobProc, error) {
		if broken != nil {
			return nil, broken
		}
		return real()
	}
	t.Cleanup(func() { runner.ListProcs = real })
	broken = errors.New("ps 坏了")
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	broken = nil
	waitForClaudes(t, log, 1)
	st, err := state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 1 || st.Jobs[0].PID != 0 || st.Jobs[0].Status != state.StatusRunning {
		t.Fatalf("测试前提：运行中、PID 0：%s", fmtJobs(st))
	}
	return a, log, st, func(e error) { broken = e }
}

// stop 不知道 pid 的任务：按任务找到 claude，真的杀掉再收尾，名额放开后下一条才接上，不超并发。
func TestStopFindsLostClaude(t *testing.T) {
	for name, later := range map[string]time.Duration{"报 pid 时限之内": 0, "过了时限": runner.ShellReportWait + time.Minute} {
		t.Run(name, func(t *testing.T) {
			a, log, st, _ := lostRunningJob(t)
			now := time.Now().Add(later)
			a.Now = func() time.Time { return now }
			pid := lostShellPids(t, log)[0]
			msg, err := a.Runner.StopByID(st, st.Jobs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(msg, "已经结束") || !strings.Contains(msg, "已停止") {
				t.Fatalf("进程还在，不能说已经结束：%s", msg)
			}
			if st.Jobs[0].Status != state.StatusKilled || st.Jobs[0].PID != pid {
				t.Fatalf("应记成已停止并记下 pid：%s", fmtJobs(st))
			}
			if live := liveClaudes(t, log); len(live) != 0 {
				t.Fatalf("claude 应被杀掉：%v", live)
			}
			if err := a.Sched.Tick(""); err != nil {
				t.Fatal(err)
			}
			waitForClaudes(t, log, 2)
			if live := liveClaudes(t, log); len(live) != 1 {
				t.Fatalf("max_parallel=1：%v", live)
			}
		})
	}
}

// ps 还是坏的：没停成就如实说，任务照旧算在跑、占着名额，下一条不接；恢复后再停才停得掉。
func TestStopCannotLookUpSaysSo(t *testing.T) {
	a, log, st, breakPS := lostRunningJob(t)
	id := st.Jobs[0].ID
	breakPS(errors.New("ps 坏了"))
	msg, err := a.Runner.StopByID(st, id)
	if err == nil {
		t.Fatalf("没停成不能算成功：%s", msg)
	}
	if !strings.Contains(err.Error(), "没停成") || !strings.Contains(err.Error(), "ps 坏了") {
		t.Fatalf("要说没停成和原因：%v", err)
	}
	if st.Jobs[0].Status != state.StatusRunning {
		t.Fatalf("不能放掉名额：%s", fmtJobs(st))
	}
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if live := liveClaudes(t, log); len(live) != 1 {
		t.Fatalf("没停成时不能接下一条：%v", live)
	}
	breakPS(nil)
	st, err = state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Runner.StopByID(st, id); err != nil {
		t.Fatal(err)
	}
	if live := liveClaudes(t, log); len(live) != 0 {
		t.Fatalf("恢复后应停掉：%v", live)
	}
}

// 确认没有进程：直接记已停止，用户不用等。过了时限前后都一样。
func TestStopNoProcessAtAll(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	j := lostJob(a, "1104-000011-alpha")
	j.StartedAt = a.Now() // 还在报 pid 的时限之内
	st := &State{Jobs: []*Job{j}}
	msg, err := a.Runner.StopByID(st, j.ID)
	if err != nil || j.Status != state.StatusKilled || !strings.Contains(msg, "已停止") {
		t.Fatalf("%q %v %s", msg, err, fmtJobs(st))
	}
}

// status 不能把不知道 pid 的任务说成进程已结束。
func TestStatusUnknownPIDNotReportedDead(t *testing.T) {
	a, _, _, _ := lostRunningJob(t)
	out := a.Out.(*bytes.Buffer)
	out.Reset()
	if err := a.CmdStatus(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "进程已结束") || !strings.Contains(out.String(), "还不知道 pid") {
		t.Fatalf("%s", out)
	}
}

// 用户停掉的任务：不管还有没有进程，都不接回。
func TestLostClaudeStoppedStaysStopped(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	j := lostJob(a, "1104-000008-alpha")
	j.Status = state.StatusKilled
	now := a.Now()
	j.EndedAt = &now
	startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + j.ID}, os.Args[0], "-test.run=^TestHelperSleep$")
	st := &State{Jobs: []*Job{j}}
	a.Runner.Reap(st)
	if j.Status != state.StatusKilled || j.PID != 0 {
		t.Fatalf("停掉的不该接回：%s", fmtJobs(st))
	}
}

// 过了时限的外壳还活着：杀掉，它不能之后再起一个 claude。
func TestFindLostClaudeKillsLateShell(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	j := lostJob(a, "1104-000009-alpha")
	shell := startGroup(t, nil, "/bin/sh", "-c", "sleep 30; :", "agentq", "_job", a.JobDir(j.ID), "/project")
	if got, err := a.Runner.FindLostClaude(j); err != nil || got != 0 {
		t.Fatalf("外壳不是 claude：%d %v", got, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, err := proc.PsOutput("-o", "stat=", "-p", strconv.Itoa(shell))
		if err != nil || strings.HasPrefix(strings.TrimSpace(out), "Z") {
			return // 不在了，或者只等回收
		}
		if time.Now().After(deadline) {
			t.Fatalf("外壳应被杀：%s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 只剩子进程、claude 没了：清掉，记失败后没人再管它们。
func TestFindLostClaudeCleansStragglers(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	j := lostJob(a, "1104-000010-alpha")
	child := "/bin/sh -c 'sleep 30; :' --session-id " + j.SessionID + " '" + proc.NameMarker(j.Project) + "甲' & exit 0"
	group := startGroup(t, nil, "/bin/sh", "-c", child) // 组长马上退出，留下不是组长的子进程
	if !proc.GroupAlive(group) {
		t.Fatal("测试前提：子进程还在组里")
	}
	if got, err := a.Runner.FindLostClaude(j); err != nil || got != 0 {
		t.Fatalf("子进程不是 claude：%d %v", got, err)
	}
	if !proc.WaitGroupGone(group, 8*time.Second) {
		t.Fatal("子进程应被清掉")
	}
}
