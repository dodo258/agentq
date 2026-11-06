package proc

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSystemctlEnvFallback(t *testing.T) {
	exists := func(string) bool { return true }
	missing := func(string) bool { return false }
	uid := strconv.Itoa(os.Getuid())
	if env := SystemctlEnv([]string{"A=1"}, exists); !slices.Contains(env, "XDG_RUNTIME_DIR=/run/user/"+uid) {
		t.Errorf("/run/user/<uid> 在时要补上：%v", env)
	}
	if env := SystemctlEnv([]string{"A=1"}, missing); slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "XDG_RUNTIME_DIR=") }) {
		t.Errorf("目录不在时不编：%v", env)
	}
	if env := SystemctlEnv([]string{"XDG_RUNTIME_DIR=/x"}, exists); len(env) != 1 || env[0] != "XDG_RUNTIME_DIR=/x" {
		t.Errorf("已有的不覆盖：%v", env)
	}
}

func TestProcStatBtime(t *testing.T) {
	got, err := ParseProcStatBtime("cpu  1 2 3\nintr 5\nctxt 6\nbtime 1791363855\nprocesses 7\n")
	if err != nil || got != 1791363855 {
		t.Errorf("%v %v", got, err)
	}
	for _, bad := range []string{"cpu 1\n", "btime abc\n", "btime\n", "btime 0\n"} {
		if _, err := ParseProcStatBtime(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// ps 看中文命令行：项目名不是 ASCII 时，后台（没有 LANG）也要认得出任务还活着。
func TestProcAliveNonASCIIMarker(t *testing.T) {
	marker := NameMarker("中文项目")
	cmd := exec.Command("/bin/sh", "-c", "sleep 20; :", marker+"标题")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !ProcAlive(cmd.Process.Pid, marker) {
		if time.Now().After(deadline) {
			t.Fatalf("procAlive 认不出中文标记")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestParseBootTime(t *testing.T) {
	got, err := parseBootTime("{ sec = 1791363855, usec = 257843 } Wed Oct  7 17:04:15 2026\n")
	if err != nil || got != 1791363855 {
		t.Errorf("得到 %d %v", got, err)
	}
	if _, err := parseBootTime("garbage"); err == nil {
		t.Error("看不懂的输出应报错")
	}
	if b, err := BootTime(); err != nil || b <= 0 {
		t.Errorf("本机开机时间读不出：%d %v", b, err)
	}
}

func TestChildEnvWhitelist(t *testing.T) {
	AddChildEnvKeep("CLAUDE_CONFIG_DIR") // 后端注册时声明的变量；这个包的测试没有后端
	t.Cleanup(func() { RemoveChildEnvKeep("CLAUDE_CONFIG_DIR") })
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "x")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	t.Setenv("SOME_SECRET", "s")
	t.Setenv("AGENTQ_KEEP_ME", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	env := "\n" + strings.Join(ChildEnv(), "\n")
	for _, bad := range []string{"CLAUDECODE=", "CLAUDE_CODE_SESSION_ID=", "ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "SOME_SECRET="} {
		if strings.Contains(env, "\n"+bad) {
			t.Errorf("应去掉 %s", bad)
		}
	}
	for _, want := range []string{"AGENTQ_KEEP_ME=1", "CLAUDE_CONFIG_DIR=/cfg", "LC_CTYPE=UTF-8", "HTTPS_PROXY=http://127.0.0.1:7890", "HOME=", "PATH="} {
		if !strings.Contains(env, "\n"+want) {
			t.Errorf("应保留 %s", want)
		}
	}
}

func TestProcAlive(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if !ProcAlive(pid, "sleep 5") {
		t.Error("命令行含标记的活进程应判为活着")
	}
	if ProcAlive(pid, NameMarker("别的项目")) {
		t.Error("命令行不含标记（pid 被复用）应判为不在")
	}
	if ProcAlive(0, "x") || ProcAlive(-1, "x") {
		t.Error("非法 pid 应判为不在")
	}
	cmd.Process.Kill()
	// 不 Wait：子进程成为僵尸，kill(pid,0) 仍成功，但应判为不在
	deadline := time.Now().Add(3 * time.Second)
	for ProcAlive(pid, "sleep 5") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if ProcAlive(pid, "sleep 5") {
		t.Error("僵尸进程应判为不在")
	}
	cmd.Wait()
}

// 启动 CLI 只带公共部分和这个后端自己声明的：别家的登录变量不传。agentq 自己的子进程带全集。
func TestBackendEnvOnlyOwnKeep(t *testing.T) {
	AddChildEnvKeep("A_TOKEN", "B_TOKEN")
	t.Cleanup(func() { RemoveChildEnvKeep("A_TOKEN", "B_TOKEN") })
	t.Setenv("A_TOKEN", "a")
	t.Setenv("B_TOKEN", "b")
	t.Setenv("SOME_SECRET", "s")
	env := "\n" + strings.Join(BackendEnv([]string{"A_TOKEN"}), "\n")
	if !strings.Contains(env, "\nA_TOKEN=a") || !strings.Contains(env, "\nPATH=") {
		t.Errorf("应保留自己的和公共的：%s", env)
	}
	if strings.Contains(env, "\nB_TOKEN=") || strings.Contains(env, "\nSOME_SECRET=") {
		t.Errorf("不该带别家的：%s", env)
	}
	all := "\n" + strings.Join(ChildEnv(), "\n")
	if !strings.Contains(all, "\nA_TOKEN=a") || !strings.Contains(all, "\nB_TOKEN=b") {
		t.Errorf("全集应带所有后端的：%s", all)
	}
}
