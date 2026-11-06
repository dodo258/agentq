package tests

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 外壳在报回 pid 之前死掉、claude 还活着。真外壳从起 claude 到报 pid 只隔几微秒，靠杀进程撞不稳定，
// 所以把外壳换成 TestHelperLostShell：和真外壳一样起 claude（startClaude），然后什么都不报就退出。

// 只作为假外壳运行：参数和真外壳一样，_job <任务目录> <项目目录> <claude> [参数…]。
func TestHelperLostShell(t *testing.T) {
	if os.Getenv("AGENTQ_TEST_LOST_SHELL") != "1" {
		t.Skip("只作为 agentq _job 的替身运行")
	}
	args := flag.Args()
	if len(args) < 4 || args[0] != "_job" {
		os.Exit(9)
	}
	jobDir, projectDir, claude := args[1], args[2], args[3]
	syscall.CloseOnExec(3) // 和真外壳一样：别让 claude 继承报 pid 的管道
	if _, err := runner.StartCLI(claude, projectDir, filepath.Base(jobDir), args[4:], nil, "", filepath.Join(jobDir, "result.json"), filepath.Join(jobDir, "stderr.log")); err != nil {
		os.Exit(8)
	}
	os.Exit(0) // 报 pid 的管道随退出关掉，启动方读到 EOF
}

// 假 claude：记下自己的 pid，等放行文件出现才写结果退出。
const lostShellClaude = `#!/bin/sh
case "$1" in
  auth) echo '{"loggedIn": true}'; exit 0 ;;
  -p) [ "$2" = /usage ] && { echo usage; exit 0; } ;;
esac
echo "$$" >> "$AGENTQ_FAKE_LOG/claude-pids"
while [ ! -f "$AGENTQ_FAKE_LOG/release" ]; do /bin/sleep 0.1; done
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"做完了。","session_id":"s-lost"}'
`

// lostShellEnv 准备两个项目各一条待做、并发上限 1，外壳换成假的。
func lostShellEnv(t *testing.T) (*App, string) {
	t.Helper()
	a, _ := newTestApp(t)
	log := t.TempDir()
	t.Setenv("AGENTQ_FAKE_LOG", log)
	fakeClaudePath := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaudePath, []byte(lostShellClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	testkit.MkProject(t, root, "alpha", "- [ ] 甲 —— 做事\n")
	testkit.MkProject(t, root, "beta", "- [ ] 乙 —— 做事\n")
	a.Cfg.ClaudePath, a.Cfg.ProjectsRoot = fakeClaudePath, root
	a.Cfg.MaxParallel, a.Cfg.PerProject = 1, 1
	a.Cfg.PatrolEnabled, a.Cfg.NotifyEnabled, a.Cfg.VerifyAfterMerge = false, false, false
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), "lost-shell")
	script := "#!/bin/sh\nAGENTQ_TEST_HELPER=1 AGENTQ_TEST_LOST_SHELL=1 exec " + fsx.ShQuote(os.Args[0]) + " '-test.run=^TestHelperLostShell$' -- \"$@\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := runner.JobExecutable
	runner.JobExecutable = func() (string, error) { return shell, nil }
	t.Cleanup(func() { runner.JobExecutable = old })
	t.Cleanup(func() {
		for _, pid := range lostShellPids(t, log) {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	return a, log
}

func lostShellPids(t *testing.T, log string) []int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(log, "claude-pids"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// liveClaudes：记下来的 claude 里现在还活着的。
func liveClaudes(t *testing.T, log string) []int {
	t.Helper()
	var live []int
	for _, pid := range lostShellPids(t, log) {
		if syscall.Kill(pid, 0) == nil {
			if out, err := proc.PsOutput("-o", "stat=", "-p", strconv.Itoa(pid)); err == nil && !strings.HasPrefix(strings.TrimSpace(out), "Z") {
				live = append(live, pid)
			}
		}
	}
	return live
}

func waitForClaudes(t *testing.T, log string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for len(lostShellPids(t, log)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("等了 20 秒，只起了 %d 个 claude，要 %d 个", len(lostShellPids(t, log)), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 外壳报 pid 前死掉：不能把任务记成启动失败又放着 claude 不管，下一条接上就超过 max_parallel 了。
func TestShellLostDoesNotExceedMaxParallel(t *testing.T) {
	a, log := lostShellEnv(t)
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	waitForClaudes(t, log, 1)
	time.Sleep(300 * time.Millisecond) // 真出问题时第二个 claude 要点时间才写日志
	if live := liveClaudes(t, log); len(live) != 1 {
		t.Fatalf("max_parallel=1，活着的 claude 应该只有 1 个：%v\n%s", live, a.Out)
	}
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if live := liveClaudes(t, log); len(live) != 1 {
		t.Fatalf("再 tick 一次也只能有 1 个：%v\n%s", live, a.Out)
	}
	st, err := state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 1 || st.Jobs[0].Status != state.StatusRunning {
		t.Fatalf("这个任务应记成运行中：%s\n%s", fmtJobs(st), a.Out)
	}
	pid := lostShellPids(t, log)[0]
	if st.Jobs[0].PID != pid {
		t.Fatalf("应接回 claude 的 pid %d：%s", pid, fmtJobs(st))
	}

	// 放行：按结果收尾，下一条才接上
	testkit.WriteInstall(t, filepath.Join(log, "release"), "", 0o644)
	deadline := time.Now().Add(10 * time.Second)
	for len(liveClaudes(t, log)) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := os.Remove(filepath.Join(log, "release")); err != nil {
		t.Fatal(err)
	}
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	waitForClaudes(t, log, 2)
	st, err = state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 2 || st.Jobs[0].Status == state.StatusRunning || st.Jobs[0].Status == state.StatusFailed || st.Jobs[1].Status != state.StatusRunning {
		t.Fatalf("第一个收尾、第二个接上：%s\n%s", fmtJobs(st), a.Out)
	}
}

func fmtJobs(st *State) string {
	var b strings.Builder
	for _, j := range st.Jobs {
		fmt.Fprintf(&b, "%s %s pid=%d %s\n", j.ID, j.Status, j.PID, j.Summary)
	}
	return b.String()
}
