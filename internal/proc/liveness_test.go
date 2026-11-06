package proc

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// 测试程序自己当「任务的 CLI」：不是系统自带的程序，环境读得到（Node、Codex 原生程序也是这样，实测记录在 proc_darwin.go）。
func TestMain(m *testing.M) {
	if os.Getenv("AGENTQ_PROC_TEST_SLEEP") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startHelper(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), append(env, "AGENTQ_PROC_TEST_SLEEP=1")...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	waitEnvVisible(t, cmd.Process.Pid)
	return cmd
}

func waitEnvVisible(t *testing.T, pid int) {
	t.Helper()
	for range 50 {
		if _, readable := procEnvHas(pid, "X=1"); readable {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestJobLivenessByEnv(t *testing.T) {
	cmd := startHelper(t, JobEnv+"=job-a")
	pid := cmd.Process.Pid
	if got := JobLiveness(pid, "job-a", "_job /不存在 "); got != ProcOurs {
		t.Errorf("环境里有这个任务的标记就是它：%v", got)
	}
	// 前缀相同的另一个任务不算
	if got := JobLiveness(pid, "job", "_job /不存在 "); got != ProcGone {
		t.Errorf("环境读得到、标记不对就是 pid 被复用了：%v", got)
	}
	if got := JobLiveness(pid, "job-b", "_job /不存在 "); got != ProcGone {
		t.Errorf("%v", got)
	}
}

func TestJobLivenessGone(t *testing.T) {
	if got := JobLiveness(0, "j", "x"); got != ProcGone {
		t.Error(got)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := JobLiveness(cmd.Process.Pid, "j", "x"); got != ProcGone {
		t.Errorf("进程没了：%v", got)
	}
}

func TestJobLivenessZombie(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := PsOutput("-o", "stat=", "-p", strconv.Itoa(cmd.Process.Pid))
		if len(out) > 0 && out[0] == 'Z' {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := JobLiveness(cmd.Process.Pid, "j", "x"); got != ProcGone {
		t.Errorf("僵尸不算活着：%v", got)
	}
}

// 环境读不到（macOS 上系统自带的程序）：外壳还在等它就是它，外壳不在就下不了结论，不能杀，也不能当结束了。
func TestJobLivenessEnvHidden(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Linux 上 /proc 总读得到自己用户的进程环境")
	}
	sleep := exec.Command("/bin/sleep", "30")
	sleep.Env = append(os.Environ(), JobEnv+"=job-a")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
	}()
	pid := sleep.Process.Pid
	time.Sleep(100 * time.Millisecond)
	if found, readable := procEnvHas(pid, JobEnv+"=job-a"); found || readable {
		t.Fatalf("/bin/sleep 的环境应该读不到：%v %v", found, readable)
	}
	if got := JobLiveness(pid, "job-a", "_job /tmp/没有这个外壳-"+strconv.Itoa(pid)+" "); got != ProcUnsure {
		t.Errorf("认不出：%v", got)
	}
	// 外壳的命令行还在：它在等这个 pid，pid 不可能被复用
	marker := exec.Command(os.Args[0], "_job", "/tmp/外壳"+strconv.Itoa(pid), "x")
	marker.Env = append(os.Environ(), "AGENTQ_PROC_TEST_SLEEP=1")
	if err := marker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = marker.Process.Kill()
		_ = marker.Wait()
	}()
	deadline := time.Now().Add(3 * time.Second)
	got := ProcUnsure
	for time.Now().Before(deadline) && got != ProcOurs {
		got = JobLiveness(pid, "job-a", "_job /tmp/外壳"+strconv.Itoa(pid)+" ")
		time.Sleep(50 * time.Millisecond)
	}
	if got != ProcOurs {
		t.Errorf("外壳在：%v", got)
	}
}
