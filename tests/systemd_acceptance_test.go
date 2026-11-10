package tests

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/service"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 真实 systemd 用户服务的启停验收，只在 GitHub Actions 的 Linux runner 上跑（见 .github/workflows/ci.yml）。
// 用户实例认 passwd 里的家目录，临时 HOME 对它没用，所以这里用 runner 的真实用户，unit 名带随机串隔离，结束清理。
// 假 claude，不消耗模型额度。本机绝不跑：没有 GITHUB_ACTIONS 就退出。
const acceptanceClaude = `#!/bin/sh
case "$1" in
  --version) echo '2.1.296 (Claude Code)'; exit 0 ;;
  --help) echo '--print --output-format --session-id --effort'; exit 0 ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}'; exit 0 ;;
esac
if [ "$1" = "-p" ] && [ "$2" = "/usage" ]; then
  echo '{"type":"result","is_error":false,"result":"You are currently using your subscription"}'; exit 0
fi
echo $$ > "$HOME/ci-claude.pid"
sleep 12
sed -i -e 's/^- \[ \] 验收任务.*/- [x] 验收任务 —— 合并提交 1a2b3c4/' docs/BACKLOG.md
echo "$(date +%s)" > "$HOME/ci-claude.done"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"已合并，提交 1a2b3c4。","session_id":"s-ci"}'
`

type sysAcc struct {
	t     *testing.T
	home  string
	label string
	bin   string // ~/.local/bin/agentq 之外的候选，安装引擎从它复制
}

func (s *sysAcc) agentq(extraEnv []string, args ...string) (string, error) {
	s.t.Helper()
	ctxArgs := append([]string{}, args...)
	cmd := exec.Command(s.bin, ctxArgs...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return b.String(), err
	case <-time.After(5 * time.Minute):
		cmd.Process.Kill()
		return b.String(), fmt.Errorf("agentq %v 超时", args)
	}
}

func (s *sysAcc) mustAgentq(args ...string) string {
	s.t.Helper()
	out, err := s.agentq(nil, args...)
	s.t.Logf("agentq %v:\n%s", args, out)
	if err != nil {
		s.t.Fatalf("agentq %v: %v\n%s", args, err, out)
	}
	return out
}

func (s *sysAcc) systemctl(args ...string) (string, error) {
	b, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(b)), err
}

func (s *sysAcc) prop(unit, name string) string {
	s.t.Helper()
	out, err := s.systemctl("show", "--property="+name, "--value", unit)
	if err != nil {
		s.t.Fatalf("show %s %s: %v %s", unit, name, err, out)
	}
	return out
}

func (s *sysAcc) mustBeEnabled(units ...string) {
	s.t.Helper()
	for _, u := range units {
		if got, err := s.systemctl("is-enabled", u); err != nil || got != "enabled" {
			s.t.Fatalf("%s 应已 enable：%q %v", u, got, err)
		}
		if got, err := s.systemctl("is-active", u); err != nil || got != "active" {
			s.t.Fatalf("%s 应在运行：%q %v", u, got, err)
		}
	}
}

func (s *sysAcc) mustBeGone(units ...string) {
	s.t.Helper()
	for _, u := range units {
		if got, err := s.systemctl("is-enabled", u); err == nil {
			s.t.Fatalf("%s 应已卸下：%q", u, got)
		}
		if got := s.prop(u, "ActiveState"); got == "active" {
			s.t.Fatalf("%s 应已停止", u)
		}
		if _, err := os.Stat(filepath.Join(s.home, ".config", "systemd", "user", u)); !os.IsNotExist(err) {
			s.t.Fatalf("%s 文件应已删除：%v", u, err)
		}
	}
}

func (s *sysAcc) waitFor(what string, d time.Duration, ok func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			s.t.Fatalf("等 %s 超时（%s）", what, d)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (s *sysAcc) writeConfig(paused bool, root string) {
	s.t.Helper()
	b, _ := json.Marshal(map[string]any{
		"launchd_label": s.label, "projects_root": root, "claude_path": filepath.Join(s.home, ".local", "bin", "claude"),
		"paused": paused, "patrol_enabled": false, "notify_enabled": false, "night_enabled": false, "review_enabled": false,
		"permission_mode": "default",
	})
	if err := os.WriteFile(filepath.Join(s.home, ".agentq", "config.json"), append(b, '\n'), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sysAcc) app() *App {
	cfg := config.Default()
	cfg.LaunchdLabel = s.label
	return wireApp(&App{Env: &env.Env{Home: s.home, Cfg: cfg, Out: os.Stderr, Now: time.Now}})
}

func (s *sysAcc) stateFor(label string) (service.ServiceState, bool) {
	s.t.Helper()
	states, err := s.app().Service.Svc().List()
	if err != nil {
		s.t.Fatalf("list：%v", err)
	}
	for _, st := range states {
		if st.Label == label {
			return st, true
		}
	}
	return service.ServiceState{}, false
}

func TestSystemdAcceptance(t *testing.T) {
	if os.Getenv("AGENTQ_SYSTEMD_ACCEPTANCE") != "1" {
		t.Skip("只在 CI 的 Linux runner 上显式开启")
	}
	if runtime.GOOS != "linux" || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("这个用例动真实的用户服务和家目录，只能在一次性的 CI runner 上跑")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".agentq", ".local/share/agentq", ".local/bin/agentq", ".local/bin/claude"} {
		if _, err := os.Lstat(filepath.Join(home, p)); err == nil {
			t.Fatalf("%s 已存在，不覆盖", p)
		}
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	s := &sysAcc{t: t, home: home, label: fmt.Sprintf("io.agentqci.r%x", rnd), bin: agentqBin}
	night, review, watchdog, listen := s.label+".night", s.label+".review", s.label+".watchdog", s.label+".listen"
	timers := []string{s.label + ".timer", night + ".timer", review + ".timer"}
	root := filepath.Join(home, "agentq-ci-projects")

	t.Cleanup(func() {
		out, _ := s.systemctl("list-units", "--all", "--no-legend", "--plain", s.label+"*")
		t.Logf("清理前的 unit：\n%s", out)
		entries, _ := os.ReadDir(filepath.Join(home, ".config", "systemd", "user"))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), s.label) {
				s.systemctl("disable", "--now", e.Name())
				s.systemctl("stop", e.Name())
				os.Remove(filepath.Join(home, ".config", "systemd", "user", e.Name()))
			}
		}
		s.systemctl("daemon-reload")
		if pid := readPID(filepath.Join(home, "ci-claude.pid")); pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		for _, p := range []string{".agentq", ".local/share/agentq", ".local/bin/agentq", ".local/bin/claude", "agentq-ci-projects", "ci-claude.pid", "ci-claude.done"} {
			os.RemoveAll(filepath.Join(home, p))
		}
		if entries, _ := os.ReadDir(filepath.Join(home, ".config", "systemd", "user")); len(entries) != 0 {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), s.label) {
					t.Errorf("清理后还有 unit：%s", e.Name())
				}
			}
		}
	})

	if out, err := s.systemctl("show-environment"); err != nil {
		t.Fatalf("runner 的 systemd 用户实例不可用：%v %s", err, out)
	}
	testkit.WriteInstall(t, filepath.Join(home, ".local", "bin", "claude"), acceptanceClaude, 0o755)
	if err := os.MkdirAll(filepath.Join(home, ".agentq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s.writeConfig(true, root)

	// 1. 安装引擎：setup --services --watchdog
	s.mustAgentq("setup", "--services", "--watchdog", "--yes")
	s.mustBeEnabled(timers...)
	s.mustBeEnabled(watchdog + ".timer")
	if got := s.prop(night+".timer", "TimersCalendar"); !strings.Contains(got, "01:00:00") {
		t.Errorf("夜班 timer 的时刻：%s", got)
	}
	if got := s.prop(s.label+".service", "KillMode"); got != "process" {
		t.Errorf("KillMode：%s", got)
	}
	// 连着启动不能撞 start-limit-hit：默认 10 秒 5 次的限制在这里曾让真实验收偶发失败
	for n := 0; n < 8; n++ {
		if out, err := s.systemctl("start", s.label+".service"); err != nil {
			t.Fatalf("第 %d 次连续启动 tick：%v %s", n+1, err, out)
		}
	}
	for _, u := range append(append([]string{}, timers...), s.label+".service", night+".service", review+".service") {
		verify := exec.Command("systemd-analyze", "--user", "verify", filepath.Join(home, ".config", "systemd", "user", u))
		if out, err := verify.CombinedOutput(); err != nil || strings.Contains(string(out), s.label) {
			t.Errorf("systemd-analyze verify %s：%v\n%s", u, err, out)
		}
	}
	// OnActiveSec=0：装上就跑一次 tick
	s.waitFor("tick 装上后先跑一次", 60*time.Second, func() bool {
		return s.prop(s.label+".service", "ExecMainExitTimestampMonotonic") != "0"
	})
	if got := s.prop(s.label+".service", "Result"); got != "success" {
		t.Errorf("首次 tick：Result=%s", got)
	}
	if got := s.prop(s.label+".timer", "NextElapseUSecRealtime"); got == "" {
		t.Errorf("tick timer 没有下一次触发时间")
	}
	doctor := s.mustAgentq("doctor")
	if !strings.Contains(doctor, "macOS") || strings.Contains(doctor, "Linger is off") {
		t.Errorf("doctor：\n%s", doctor)
	}

	// 2. 巡逻读到真实状态，失败能看见退出码（配置写坏，tick 非 0 退出；保护脚本第 1 次失败不回滚）
	if st, ok := s.stateFor(s.label); !ok || st.Code != "0" {
		t.Errorf("tick 状态：%+v %v", st, ok)
	}
	if _, ok := s.stateFor(listen); ok {
		t.Errorf("setup 不该装 listen")
	}
	if err := os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := s.systemctl("start", s.label+".service"); err == nil {
		t.Errorf("配置坏了 tick 应失败：%s", out)
	}
	if st, ok := s.stateFor(s.label); !ok || st.Code == "0" {
		t.Errorf("失败的退出码应读出来：%+v %v", st, ok)
	}
	if tail := s.app().Service.Svc().LogTail(s.label); !strings.Contains(tail, "launchd.log") {
		t.Errorf("日志末尾：%q", tail)
	}
	s.writeConfig(true, root)
	if out, err := s.systemctl("start", s.label+".service"); err != nil {
		t.Errorf("配置修好后 tick：%v %s", err, out)
	}
	if st, ok := s.stateFor(s.label); !ok || st.Code != "0" {
		t.Errorf("恢复后：%+v %v", st, ok)
	}

	// 3. 关键：tick 这个 oneshot 退出后，它拉起的任务不能被 systemd 杀掉，结束后还要能接上收尾
	demo := filepath.Join(root, "demo")
	testkit.WriteInstall(t, filepath.Join(demo, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n- [ ] 验收任务 —— 真实 systemd 下跑完\n\n## 进行中\n\n## 已完成\n", 0o644)
	gitInit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", demo}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v %s", args, err, out)
		}
	}
	gitInit("init", "-q", "-b", "main")
	gitInit("add", ".")
	gitInit("-c", "user.name=ci", "-c", "user.email=ci", "commit", "-q", "-m", "init")
	s.writeConfig(false, root)
	if out, err := s.systemctl("start", s.label+".service"); err != nil {
		t.Fatalf("tick：%v %s", err, out)
	}
	pidPath := filepath.Join(home, "ci-claude.pid")
	s.waitFor("假 claude 启动", 30*time.Second, func() bool { return readPID(pidPath) > 0 })
	s.waitFor("tick 退出", 30*time.Second, func() bool {
		st := s.prop(s.label+".service", "ActiveState")
		return st == "inactive" || st == "failed"
	})
	time.Sleep(3 * time.Second) // systemd 真要杀，KillMode 的处理在主进程退出后马上发生
	pid := readPID(pidPath)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("tick 退出后任务被杀了（pid %d）：%v", pid, err)
	}
	if _, err := os.Stat(filepath.Join(home, "ci-claude.done")); err == nil {
		t.Fatalf("任务太快结束，没测到存活")
	}
	s.waitFor("任务跑完并被收尾", 90*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(home, ".agentq", "state.json"))
		if err != nil {
			return false
		}
		var st State
		if json.Unmarshal(b, &st) != nil || len(st.Jobs) == 0 {
			return false
		}
		for _, j := range st.Jobs {
			if j.Status == state.StatusRunning {
				return false
			}
		}
		return true
	})
	if b, err := os.ReadFile(filepath.Join(home, ".agentq", "state.json")); err == nil {
		var st State
		json.Unmarshal(b, &st)
		for _, j := range st.Jobs {
			t.Logf("任务 %s：%s %s，开机时间 %d", j.ID, j.Status, j.Summary, j.BootTime)
			if j.BootTime <= 0 {
				t.Errorf("任务没记开机时间：%+v", j)
			}
		}
	}
	s.writeConfig(true, root)

	// 4. 升级保留服务，回滚完整代
	s.mustAgentq("setup", "--yes")
	s.mustBeEnabled(timers...)
	s.mustAgentq("rollback", "--yes")
	s.mustBeEnabled(timers...)
	s.mustBeEnabled(watchdog + ".timer")

	// 5. 卸载、恢复、再卸载
	s.mustAgentq("remove", "--yes")
	s.mustBeGone(append(append([]string{}, timers...), watchdog+".timer", s.label+".service")...)
	if states, err := s.app().Service.Svc().List(); err != nil || len(states) != 0 {
		t.Errorf("卸载后状态应为空：%+v %v", states, err)
	}
	s.mustAgentq("restore", "--yes")
	s.mustBeEnabled(timers...)
	s.mustAgentq("doctor")
	s.mustAgentq("remove", "--yes")
	s.mustBeGone(timers...)

	// 6. systemctl --user 不可用：明确报错，不写东西（模拟 WSL 没开 systemd、容器）
	testkit.WriteInstall(t, filepath.Join(home, ".local", "bin", "agentq"), mustRead(t, agentqBin), 0o755)
	out, err := s.agentq([]string{"XDG_RUNTIME_DIR=/nonexistent", "DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent"}, "install")
	if err == nil || !strings.Contains(out, "systemctl --user is not usable") || !strings.Contains(out, "WSL") {
		t.Errorf("不可用时要明说：%v\n%s", err, out)
	}
	if _, e := os.Stat(filepath.Join(home, ".config", "systemd", "user", s.label+".timer")); !os.IsNotExist(e) {
		t.Errorf("不可用时不该写 unit")
	}

	// 7. 旧入口 agentq install：含常驻的 listen，被杀后 Restart=always 拉起来
	s.mustAgentq("install")
	s.mustBeEnabled(timers...)
	s.mustBeEnabled(listen + ".service")
	var first string
	s.waitFor("listen 起来", 30*time.Second, func() bool {
		first = s.prop(listen+".service", "MainPID")
		return first != "" && first != "0"
	})
	if st, ok := s.stateFor(listen); !ok || st.Pid != first {
		t.Errorf("list 读到的 listen：%+v，MainPID %s", st, first)
	}
	firstPID, _ := strconv.Atoi(first)
	if err := syscall.Kill(firstPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	s.waitFor("listen 被杀后重启", 40*time.Second, func() bool {
		got := s.prop(listen+".service", "MainPID")
		return got != "" && got != "0" && got != first
	})
	s.mustAgentq("uninstall")
	s.mustBeGone(append(append([]string{}, timers...), listen+".service")...)
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
