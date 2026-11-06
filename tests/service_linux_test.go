package tests

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/env"
	"agentq/internal/proc"
	"agentq/internal/service"
	"agentq/internal/testkit"
)

func TestLinuxDefaultManagerIsSystemd(t *testing.T) {
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir()}})
	if _, ok := a.Service.Svc().(service.SystemdManager); !ok {
		t.Fatalf("Linux 默认应是 systemd：%T", a.Service.Svc())
	}
	if !strings.Contains(a.Service.GuardContent(), `prev_time=$(date -r "$bin.prev" '+%Y-%m-%d %H:%M' 2>/dev/null)`) {
		t.Errorf("保护脚本应保持原样：\n%s", a.Service.GuardContent())
	}
	// 假 launchctl 只用来跑 launchd 的用例，选到它不等于会调真实 launchctl
	a.Service.Launchctl = func(...string) ([]byte, error) { return nil, nil }
	if _, ok := a.Service.Svc().(service.LaunchdManager); !ok {
		t.Fatalf("带假 launchctl 时走 launchd：%T", a.Service.Svc())
	}
}

func TestLinuxRefusesRealSystemctlWithFakeHome(t *testing.T) {
	a, _, _ := newSystemdApp(t)
	a.Service.Systemctl, a.Service.Launchctl = nil, nil
	if err := a.Service.CheckRealLaunchd(); err == nil || !strings.Contains(err.Error(), "不调用真实 systemctl") {
		t.Fatalf("%v", err)
	}
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	if err := a.Service.CmdInstall(); err == nil || !strings.Contains(err.Error(), "不调用真实 systemctl") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(a.Service.GuardPath()); !os.IsNotExist(err) {
		t.Errorf("被拒绝时不该写文件")
	}
}

func TestLinuxBootTime(t *testing.T) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		t.Fatal(err)
	}
	want, err := proc.ParseProcStatBtime(string(b))
	if err != nil {
		t.Fatal(err)
	}
	got, err := proc.BootTime()
	if err != nil || got != want {
		t.Errorf("%d %v，想要 %d", got, err, want)
	}
	if time.Unix(got, 0).After(time.Now()) {
		t.Errorf("开机时间在未来：%d", got)
	}
}

// 真实 /proc：组里的进程带着任务标记才算，系统程序也读得到环境。
func TestLinuxGroupMarked(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "20")
	cmd.Env = []string{"AGENTQ_JOB=job-linux"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	pgid := cmd.Process.Pid
	deadline := time.Now().Add(3 * time.Second)
	for {
		ok, err := proc.GroupMarked(pgid, "job-linux")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("进程组 %s 认不出标记", strconv.Itoa(pgid))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ok, err := proc.GroupMarked(pgid, "job-othe"); ok || err != nil {
		t.Errorf("别的任务不该算：%v %v", ok, err)
	}
}

// 保护脚本在 Linux 上取 agentq.prev 的时间不能用 BSD stat。
func TestLinuxGuardRecordsPrevTime(t *testing.T) {
	a, run := guardEnv(t, true)
	setCode(t, a.Home, "1")
	run()
	_, out := run()
	if !strings.Contains(out, time.Now().Format("2006-01-02")) || !strings.Contains(out, "已自动回滚") {
		t.Errorf("回滚记录里应有 prev 的时间：\n%s", out)
	}
}
