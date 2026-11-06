package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// startGroup 在新进程组里启动命令，返回组号；结束时杀掉整组。另起 goroutine Wait，被杀后不留僵尸。
func startGroup(t *testing.T, env []string, name string, args ...string) int {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	pid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond) // 等 sh 拉起子进程
	return pid
}

// 测试用的长时间进程：测试二进制本身不是系统自带命令，ps -E 看得到它的环境变量。
func TestHelperSleep(t *testing.T) {
	if os.Getenv("AGENTQ_TEST_HELPER") != "1" {
		t.Skip("只作为 TestCleanupGroupNeedsMarker 的子进程运行")
	}
	time.Sleep(30 * time.Second)
}

// 测试二进制当长时间进程用时不能先编译 agentq：go 不在 PATH、工作目录不对、源码正改到一半时，它会提前退出，
// 依赖这个进程的用例（TestCleanupGroupNeedsMarker、TestReapAfterReboot）就跟着失败。
func TestHelperProcessSkipsBuild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Dir = t.TempDir() // 这里没有 go.mod
	cmd.Env = []string{"AGENTQ_TEST_HELPER=1", "PATH=/nonexistent"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cmd.Process.Kill() })
	select {
	case <-exited:
		t.Fatal("助手进程不该因为编译不了而退出")
	case <-time.After(time.Second):
	}
}

func newTestApp(t *testing.T) (*App, *bytes.Buffer) {
	var out bytes.Buffer
	return wireApp(&App{Env: &env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: time.Now, Out: &out}}), &out
}

// tick 收尾时离 claude 退出可能已经很久，组号可能被别的程序占了：组里没有这个任务的标记就不杀。
func TestCleanupGroupNeedsMarker(t *testing.T) {
	a, out := newTestApp(t)
	j := &Job{ID: "1010-000000-alpha", Project: "alpha"}

	j.PID = startGroup(t, nil, "/bin/sleep", "30")
	a.Runner.CleanupGroup(j)
	time.Sleep(300 * time.Millisecond)
	if !proc.GroupAlive(j.PID) {
		t.Error("命令行和环境里都没有标记的进程组不应被杀")
	}
	if !strings.Contains(out.String(), "没有这个任务的标记") || strings.Contains(out.String(), "已发 SIGTERM") {
		t.Errorf("日志：\n%s", out)
	}

	// 命令行里只有项目级的 --name 标记（比如组号被同项目另一个任务的 claude 占了）也不算
	out.Reset()
	j.PID = startGroup(t, nil, "/bin/sh", "-c", "sleep 30; :", "agentq alpha 别的任务")
	a.Runner.CleanupGroup(j)
	time.Sleep(300 * time.Millisecond)
	if !proc.GroupAlive(j.PID) || strings.Contains(out.String(), "已发 SIGTERM") {
		t.Errorf("只有项目级标记的进程组不应被杀：\n%s", out)
	}

	// 环境变量里有 AGENTQ_JOB=<任务 id>（claude 拉起的子进程都会继承）
	out.Reset()
	j.PID = startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + j.ID}, os.Args[0], "-test.run=^TestHelperSleep$")
	a.Runner.CleanupGroup(j)
	if !proc.WaitGroupGone(j.PID, 3*time.Second) || !strings.Contains(out.String(), "已发 SIGTERM") {
		t.Errorf("环境里带任务标记的进程组应被清掉：\n%s", out)
	}

	// 别的任务的标记不算
	out.Reset()
	j.PID = startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + j.ID + "-2"}, os.Args[0], "-test.run=^TestHelperSleep$")
	a.Runner.CleanupGroup(j)
	time.Sleep(300 * time.Millisecond)
	if !proc.GroupAlive(j.PID) || strings.Contains(out.String(), "已发 SIGTERM") {
		t.Errorf("别的任务的标记不应触发清理：\n%s", out)
	}
}

// 任务启动后机器重启过（开机时间不一致）：记下的 pid/组号可能已属于别的程序，不清进程组，任务记中断。
func TestReapAfterReboot(t *testing.T) {
	a, out := newTestApp(t)
	root := t.TempDir()
	dir := testkit.MkProject(t, root, "alpha", "- [ ] 甲 —— 做事（进行中：agent/t-boot 10-10 03:00）\n- [ ] 乙 —— 做事\n")
	items, err := backlog.Project{Dir: dir}.Items()
	if err != nil {
		t.Fatal(err)
	}
	cur := a.CurBoot()
	if cur == 0 {
		t.Fatal("测试前提：读得出本机开机时间")
	}
	mk := func(id string, it backlog.Item, sid string) *Job {
		j := &Job{ID: id, Project: "alpha", ProjectDir: dir, Item: it.Text, Title: it.Title, Fingerprint: it.Fingerprint,
			StartedAt: time.Now().Add(-time.Minute), Status: state.StatusRunning, SessionID: sid, BootTime: cur - 3600}
		os.MkdirAll(a.JobDir(id), 0o755)
		os.WriteFile(filepath.Join(a.JobDir(id), "result.json"), nil, 0o644)
		// 进程组里的进程带着这个任务的标记（同一次开机时 cleanupGroup 会杀它），命令行里没有 --name 标记（claude 本身已不在）：
		// 能保住它的只有「开机时间不一致就不清进程组」
		j.PID = startGroup(t, []string{"AGENTQ_TEST_HELPER=1", proc.JobEnv + "=" + id}, os.Args[0], "-test.run=^TestHelperSleep$")
		return j
	}
	ja := mk("1010-000001-alpha", items[0], "sid-boot")
	jb := mk("1010-000002-alpha", items[1], "")
	st := &State{Jobs: []*Job{ja, jb}}
	if n := a.Runner.Reap(st); n != 2 {
		t.Fatalf("应收尾 2 个，得到 %d：\n%s", n, out)
	}
	time.Sleep(300 * time.Millisecond)
	for _, j := range []*Job{ja, jb} {
		if !proc.GroupAlive(j.PID) {
			t.Errorf("重启过的任务不应清进程组 %d", j.PID)
		}
		if j.Status != state.StatusInterrupted || !j.Rebooted || !strings.Contains(j.Summary, "机器重启过") {
			t.Errorf("应记中断：%+v", j)
		}
	}
	if strings.Contains(out.String(), "SIGTERM") {
		t.Errorf("不应发任何信号：\n%s", out)
	}
	if !ja.ResumePending || !strings.Contains(ja.Summary, "将自动用同一会话续跑一次") {
		t.Errorf("条目仍标进行中、有会话 id 的应续跑：%+v", ja)
	}
	if jb.ResumePending || !strings.Contains(jb.Summary, "条目已不在进行中") {
		t.Errorf("条目没标进行中的不续跑：%+v", jb)
	}
	if st.FailStreak != 0 {
		t.Errorf("重启中断不算秒退：fail_streak=%d", st.FailStreak)
	}
	if n, _ := os.ReadFile(a.NightlyPath(time.Now())); strings.Count(string(n), "机器重启过") != 2 {
		t.Errorf("当天汇总应有 2 行：\n%s", n)
	}

	// 重启前 claude 已经正常退出写了结果的，照常按结果收尾（也不清进程组）
	out.Reset()
	jc := mk("1010-000003-alpha", items[1], "")
	os.WriteFile(filepath.Join(a.JobDir(jc.ID), "result.json"),
		[]byte(`{"is_error":false,"result":"已合并，提交 abc1234。","session_id":"s-c"}`), 0o644)
	st = &State{Jobs: []*Job{jc}}
	a.Runner.Reap(st)
	time.Sleep(300 * time.Millisecond)
	if jc.Status != state.StatusDone || jc.Rebooted || !proc.GroupAlive(jc.PID) {
		t.Errorf("有结果的应按结果收尾、不清进程组：%+v\n%s", jc, out)
	}

	// 开机时间变了但进程还在（校时）：不收尾，更新记录的开机时间
	out.Reset()
	jd := &Job{ID: "1010-000004-alpha", Project: "alpha", ProjectDir: dir, Status: state.StatusRunning, BootTime: cur - 3600, StartedAt: time.Now()}
	jd.PID = startGroup(t, nil, "/bin/sh", "-c", "sleep 30; :", "agentq alpha 丁")
	st = &State{Jobs: []*Job{jd}}
	if n := a.Runner.Reap(st); n != 0 || jd.Status != state.StatusRunning || jd.BootTime != cur {
		t.Errorf("进程还在时不应收尾，应更新开机时间：n=%d %+v\n%s", n, jd, out)
	}
}

// 会话记录超过 stall_after 没增长：tick 记日志、汇总写一行（只一次），status 标疑似卡死，进程不杀。
// 子代理记录一直在长的任务不算。
func TestIntegrationStallWatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"stall_after": "2s"})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 不出声 —— SLEEP=30\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 一直在写 —— TRANSCRIPT SLEEP=30\n")
	e.mustRun("tick")
	ja, jb := e.jobByTitle("不出声"), e.jobByTitle("一直在写")
	defer func() {
		e.run("stop", ja.ID)
		e.run("stop", jb.ID)
	}()
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, j := range []*Job{ja, jb} {
		if !uuid.MatchString(j.SessionID) || j.BootTime == 0 {
			t.Errorf("启动时应预先指定会话 id、记下开机时间：%+v", j)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if b, _ := os.ReadFile(filepath.Join(e.fakeLg, strconv.Itoa(ja.PID)+".txt")); !strings.Contains(string(b), "ARG[--session-id]\nARG["+ja.SessionID+"]") {
		t.Errorf("claude 应收到 --session-id：\n%s", b)
	}
	if out := e.mustRun("status"); strings.Contains(out, "疑似卡死") {
		t.Errorf("刚启动不应标疑似卡死：\n%s", out)
	}

	time.Sleep(2500 * time.Millisecond)
	out := e.mustRun("tick")
	if !strings.Contains(out, "疑似卡死：任务 "+ja.ID) || strings.Contains(out, "疑似卡死：任务 "+jb.ID) {
		t.Errorf("只有不出声的应报疑似卡死：\n%s", out)
	}
	for _, j := range []*Job{ja, jb} {
		if syscall.Kill(j.PID, 0) != nil {
			t.Errorf("疑似卡死不应杀进程：%s", j.Title)
		}
		if got := e.jobByTitle(j.Title); got.Status != state.StatusRunning {
			t.Errorf("应仍在运行：%+v", got)
		}
	}
	status := e.mustRun("status")
	for _, line := range strings.Split(status, "\n") {
		switch {
		case strings.Contains(line, ja.ID) && !strings.Contains(line, "疑似卡死（会话记录"):
			t.Errorf("status 应给不出声的标疑似卡死：%s", line)
		case strings.Contains(line, jb.ID) && strings.Contains(line, "疑似卡死"):
			t.Errorf("子代理记录在长的不应标：%s", line)
		}
	}
	e.mustRun("tick")
	if n := e.nightly(); strings.Count(n, "疑似卡死") != 1 || !strings.Contains(n, "不自动杀") {
		t.Errorf("当天汇总应只报一次：\n%s", n)
	}
}
