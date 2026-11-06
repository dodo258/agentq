package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

const welcomePrompt = "输入 n 只安装不启动"

// ttyStep：输出里出现 wait 后输入 send；wait 为空就马上输入。
type ttyStep struct{ wait, send string }

func enterAt(answer string) []ttyStep { return []ttyStep{{welcomePrompt, answer}} }

// runTTY 用 script 造一个伪终端跑 sh install.sh：它是控制终端，脚本能打开 /dev/tty。不依赖跑测试的那个终端。
func (k *installKit) runTTY(steps []ttyStep, args ...string) (string, int) {
	k.t.Helper()
	return k.ttyCommand(steps, "/bin/sh", append([]string{k.script}, args...)...)
}

// ttyCommand 在伪终端里跑一条命令。
func (k *installKit) ttyCommand(steps []ttyStep, name string, args ...string) (string, int) {
	k.t.Helper()
	bin, err := exec.LookPath("script")
	if err != nil {
		k.t.Skip("这台机器没有 script")
	}
	var argv []string
	if runtime.GOOS == "darwin" {
		argv = append([]string{"-q", "/dev/null", name}, args...)
	} else {
		// util-linux 的 script 用 $SHELL 跑 -c：这台机器不一定有测试写的那个 shell，换成 sh，再把测试要的值传给里面的脚本。
		line := fsx.ShQuote(name)
		for _, a := range args {
			line += " " + fsx.ShQuote(a)
		}
		for _, kv := range k.env {
			if v, ok := strings.CutPrefix(kv, "SHELL="); ok {
				line = "SHELL=" + fsx.ShQuote(v) + " " + line
			}
		}
		argv = []string{"-qefc", line, "/dev/null"}
	}
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(append([]string(nil), k.env...), "TERM=xterm")
	if runtime.GOOS != "darwin" {
		cmd.Env = append(cmd.Env, "SHELL=/bin/sh")
	}
	cmd.Dir = k.home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		k.t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		k.t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		k.t.Fatal(err)
	}
	var mu sync.Mutex
	var out bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	text := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}
	done := read
	deadline := time.Now().Add(2 * time.Minute)
	seen := 0
	for _, s := range steps {
		for s.wait != "" && !strings.Contains(text()[seen:], s.wait) {
			if time.Now().After(deadline) {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				k.t.Fatalf("等不到 %q：\n%s", s.wait, text())
			}
			select {
			case <-done:
				k.t.Fatalf("进程结束了也没等到 %q：\n%s", s.wait, text())
			case <-time.After(20 * time.Millisecond):
			}
		}
		seen = len(text())
		if _, err := io.WriteString(stdin, s.send); err != nil {
			k.t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		k.t.Fatalf("没有结束：\n%s", text())
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			k.t.Fatal(err)
		}
	}
	return strings.ReplaceAll(text(), "\r\n", "\n"), code
}

func newWelcomeKit(t *testing.T) *installKit {
	t.Helper()
	k := newInstallKit(t)
	k.useFakeLaunchctl()
	k.serveRelease("v1.0.0")
	return k
}

func (k *installKit) installedConfig() config.Config {
	k.t.Helper()
	cfg, _, err := setup.ReadInstallConfig(k.home)
	if err != nil {
		k.t.Fatal(err)
	}
	return cfg
}

// assertStarted：同意记了，权限放开了，假 launchctl 里服务已加载，暂停取消了。
func (k *installKit) assertStarted(method string) {
	k.t.Helper()
	var c setup.Consent
	if err := json.Unmarshal(testkit.ReadBytes(k.t, setup.ConsentPath(k.home)), &c); err != nil {
		k.t.Fatal(err)
	}
	if c.Method != method || c.TextVersion != setup.ConsentTextVersion || c.Time == "" {
		k.t.Fatalf("同意记录不对：%+v", c)
	}
	cfg := k.installedConfig()
	if cfg.Paused || cfg.PermissionMode != "bypassPermissions" {
		k.t.Fatalf("配置不对：%+v", cfg)
	}
	if !exists(filepath.Join(k.home, "fake-launchd", config.LaunchdLabel)) {
		k.t.Fatal("调度服务没加载")
	}
	if len(k.receipt().Services) != 3 {
		k.t.Fatalf("服务不对：%v", k.receipt().Services)
	}
}

// assertInstalledNotStarted：程序在，同意没记，配置还是新装的默认，假 launchctl 没加载任何服务。
func (k *installKit) assertInstalledNotStarted() {
	k.t.Helper()
	if !exists(k.receiptPath()) {
		k.t.Fatal("应该装好了")
	}
	if exists(setup.ConsentPath(k.home)) {
		k.t.Fatal("不该记同意")
	}
	cfg := k.installedConfig()
	if !cfg.Paused || cfg.PermissionMode != "default" {
		k.t.Fatalf("配置不该变：%+v", cfg)
	}
	if strings.Contains(k.launchctlCalls(), "bootstrap") || len(k.receipt().Services) != 0 {
		k.t.Fatalf("不该装服务：%s %v", k.launchctlCalls(), k.receipt().Services)
	}
}

func (k *installKit) launchctlCalls() string {
	b, _ := os.ReadFile(filepath.Join(k.home, "fake-launchctl-calls"))
	return string(b)
}

// assertNoClaudeWork：假 claude 只被问过身份和版本，没有任何任务或额度调用。
func (k *installKit) assertNoClaudeWork() {
	k.t.Helper()
	if logs, _ := filepath.Glob(filepath.Join(k.log, "*.txt")); len(logs) != 0 {
		k.t.Fatalf("没有待做却调用了 claude：%v", logs)
	}
	b, _ := os.ReadFile(filepath.Join(k.home, "fake-claude-calls"))
	for _, line := range strings.Fields(string(b)) {
		if line != "auth" {
			k.t.Fatalf("假 claude 被调用了 %q：\n%s", line, b)
		}
	}
}

// 终端里按回车：讲清楚再启动；没有待做就只说那句话，不调用模型。
func TestWelcomeEnterStarts(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out, code := k.runTTY(enterAt("\n"))
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "能读写这个用户的所有文件（包括本地密钥）", "任务内容会发给你所用的模型服务", "没有待做时什么都不做", "agentq pause", "agentq remove --yes", onboard.IdleHint)
	for _, old := range []string{"agentq init", `agentq add "`, "接下来三步"} {
		if strings.Contains(out, old) {
			t.Fatalf("不该再讲 %s：\n%s", old, out)
		}
	}
	for _, noise := range []string{"已恢复", "tick：", "扫描项目目录"} {
		if strings.Contains(out, noise) {
			t.Fatalf("第一屏不该有调度日志 %q：\n%s", noise, out)
		}
	}
	k.assertStarted("enter")
	k.assertNoClaudeWork()
}

// n、其他输入、只有空格、EOF：都不算同意，只装不启动，退出 0。
func TestWelcomeOnlyEmptyLineConsents(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	// 经过脚本的一次：真的从 install.sh 走到回答。
	k := newWelcomeKit(t)
	out, code := k.runTTY(enterAt("n\n"))
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "只安装，没有启动", "agentq start")
	k.assertInstalledNotStarted()
	k.assertNoClaudeWork()
	// 其余回答在同一个 HOME 上直接问 start --welcome：不同意什么都不变，用不着重装。
	for name, answer := range map[string]string{"n": "n\n", "y": "y\n", "空格": " \n", "EOF": "\x04"} {
		t.Run(name, func(t *testing.T) {
			out, code := k.ttyCommand(enterAt(answer), k.binPath(), "start", "--welcome")
			if code != 0 {
				t.Fatalf("%d\n%s", code, out)
			}
			testkit.AssertContains(t, out, "只安装，没有启动", "agentq start")
			k.assertInstalledNotStarted()
			k.assertNoClaudeWork()
		})
	}
}

// Ctrl-C：不算同意。程序已经装好，退出非 0。
func TestWelcomeCtrlC(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out, code := k.runTTY(enterAt("\x03"))
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	k.assertInstalledNotStarted()
}

// 下载时多按的回车不算回答：问之前攒在终端里的输入要清掉。
func TestWelcomeTypeAheadIsNotConsent(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out, code := k.runTTY([]ttyStep{{"", "\n\n\n"}, {welcomePrompt, "n\n"}})
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertInstalledNotStarted()
}

// 打不开 /dev/tty：只装不启动，退出 0，打印后续命令。
func TestWelcomeNoTerminal(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out := k.mustInstall()
	testkit.AssertContains(t, out, "这里没有终端", "agentq start")
	if strings.Contains(out, welcomePrompt) {
		t.Fatalf("没有终端不该问：\n%s", out)
	}
	k.assertInstalledNotStarted()
	k.assertNoClaudeWork()
}

// curl | sh：标准输入是脚本，也不能被当成回答。
func TestWelcomePipedScriptIsNotConsent(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out, code := k.piped()
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertInstalledNotStarted()
}

// CI、NONINTERACTIVE：有终端也不问、不启动，退出 0。
func TestWelcomeAutomation(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for _, name := range []string{"CI", "NONINTERACTIVE"} {
		t.Run(name, func(t *testing.T) {
			k := newWelcomeKit(t)
			k.env = append(k.env, name+"=1")
			out, code := k.runTTY(nil)
			if code != 0 {
				t.Fatalf("%d\n%s", code, out)
			}
			testkit.AssertContains(t, out, "自动化环境", "agentq start")
			if strings.Contains(out, welcomePrompt) {
				t.Fatalf("不该问：\n%s", out)
			}
			k.assertInstalledNotStarted()
		})
	}
}

// --yes、AGENTQ_ASSUME_YES=1 是明确同意：没有终端、在 CI 里都启动。
func TestWelcomeAssumeYes(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for name, c := range map[string]struct {
		args []string
		env  []string
	}{
		"--yes":             {args: []string{"--yes"}},
		"AGENTQ_ASSUME_YES": {env: []string{"AGENTQ_ASSUME_YES=1"}},
		"CI 加 --yes":        {args: []string{"--yes"}, env: []string{"CI=1"}},
	} {
		t.Run(name, func(t *testing.T) {
			k := newWelcomeKit(t)
			k.env = append(k.env, c.env...)
			out := k.mustInstall(c.args...)
			if strings.Contains(out, welcomePrompt) {
				t.Fatalf("不该问：\n%s", out)
			}
			testkit.AssertContains(t, out, onboard.IdleHint)
			k.assertStarted("assume-yes")
			k.assertNoClaudeWork()
		})
	}
	t.Run("AGENTQ_ASSUME_YES 别的值不算", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.env = append(k.env, "AGENTQ_ASSUME_YES=true")
		k.mustInstall()
		k.assertInstalledNotStarted()
	})
	t.Run("有终端也不问", func(t *testing.T) {
		k := newWelcomeKit(t)
		out, code := k.runTTY(nil, "--yes")
		if code != 0 || strings.Contains(out, welcomePrompt) {
			t.Fatalf("%d\n%s", code, out)
		}
		k.assertStarted("assume-yes")
	})
}

// 新安装同意后打开推送，只改 notify_enabled 这一个键；不同意不开。没配 Telegram 也收得到，走本机通知。
func TestWelcomeOpensNotify(t *testing.T) {
	t.Parallel()
	notifyOn := func(t *testing.T, k *installKit) bool {
		t.Helper()
		var c struct {
			Notify bool `json:"notify_enabled"`
		}
		if err := json.Unmarshal(testkit.ReadBytes(t, k.configPath()), &c); err != nil {
			t.Fatal(err)
		}
		return c.Notify
	}
	t.Run("回车", func(t *testing.T) {
		k := newWelcomeKit(t)
		out, code := k.runTTY(enterAt("\n"))
		if code != 0 || !notifyOn(t, k) {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "任务做完会通知你", "notify_enabled")
		st, code := k.agentq(k.home, "status")
		if code != 0 {
			t.Fatalf("%d\n%s", code, st)
		}
		testkit.AssertContains(t, st, "推送：本机通知")
	})
	t.Run("--yes", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall("--yes")
		if !notifyOn(t, k) {
			t.Fatal("同意了要打开")
		}
	})
	t.Run("只安装不开", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.runTTY(enterAt("n\n"))
		if notifyOn(t, k) {
			t.Fatal("没同意不开")
		}
	})
	t.Run("先不同意，后来 start --welcome 同意", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.runTTY(enterAt("n\n"))
		if out, code := k.agentq(k.home, "start", "--welcome", "--assume-yes"); code != 0 || !notifyOn(t, k) {
			t.Fatalf("%d\n%s", code, out)
		}
	})
	t.Run("安装前就有配置写着 false：是用户的选择", func(t *testing.T) {
		k := newWelcomeKit(t)
		testkit.WriteInstall(t, k.configPath(), `{"notify_enabled": false, "paused": true, "permission_mode": "default"}`+"\n", 0o600)
		out, code := k.runTTY(enterAt("\n"))
		if code != 0 || notifyOn(t, k) {
			t.Fatalf("%d\n%s", code, out)
		}
		if strings.Contains(out, "任务做完会通知你") {
			t.Fatalf("没打开就不说：\n%s", out)
		}
	})
	t.Run("同意之后用户改回 false：升级和再次启动不动", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall("--yes")
		cfg := strings.Replace(string(testkit.ReadBytes(t, k.configPath())), `"notify_enabled": true`, `"notify_enabled": false`, 1)
		testkit.WriteInstall(t, k.configPath(), cfg, 0o600)
		k.serveRelease("v2.0.0")
		if out, code := k.runTTY(nil); code != 0 || notifyOn(t, k) {
			t.Fatalf("%d\n%s", code, out)
		}
		if out, code := k.agentq(k.home, "start", "--welcome", "--assume-yes"); code != 0 || notifyOn(t, k) {
			t.Fatalf("%d\n%s", code, out)
		}
	})
}

// 升级不问：已启动的还开着；当初没同意的还是没启动，提醒一句。
func TestWelcomeUpgradeDoesNotAsk(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	t.Run("已启动", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall("--yes")
		consent := string(testkit.ReadBytes(t, setup.ConsentPath(k.home)))
		cfg := string(testkit.ReadBytes(t, k.configPath()))
		k.serveRelease("v2.0.0")
		out, code := k.runTTY(nil)
		if code != 0 || strings.Contains(out, welcomePrompt) {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "已升级", "v2.0.0")
		testkit.AssertBytes(t, setup.ConsentPath(k.home), consent)
		testkit.AssertBytes(t, k.configPath(), cfg)
		k.assertStarted("assume-yes")
	})
	t.Run("没启动的保持没启动", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.runTTY(enterAt("n\n"))
		k.serveRelease("v2.0.0")
		out, code := k.runTTY(nil)
		if code != 0 || strings.Contains(out, welcomePrompt) {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "已升级", "agentq start")
		k.assertInstalledNotStarted()
	})
}

// 有待做：回车后列出接了什么，不说「说一句话」那句。
func TestWelcomeWithPendingWork(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	e := &testEnv{t: t, home: k.home, root: filepath.Join(k.home, "projects"), fake: filepath.Join(k.bin, "claude"), fakeLg: k.log}
	t.Cleanup(e.waitShells)
	project := filepath.Join(k.home, "projects", "my-app")
	testkit.WriteInstall(t, filepath.Join(project, "docs", "BACKLOG.md"), "# BACKLOG\n\n## 待做\n\n- [ ] 修登录 SLEEP=1\n\n## 已完成\n", 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "t"}, {"config", "user.email", "t"}, {"add", "."}, {"commit", "-q", "-m", "x"}} {
		if out, code := k.exec(nil, project, filepath.Join(k.tools, "git"), args...); code != 0 {
			t.Fatalf("git %v：%s", args, out)
		}
	}
	out, code := k.runTTY(enterAt("\n"))
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "my-app", "修登录", "agentq status")
	if strings.Contains(out, onboard.IdleHint) {
		t.Fatalf("有待做不该说这句：\n%s", out)
	}
	e.waitShells()
	waitNoRunningJobs(t, k)
}

// 直接运行 start --welcome：管道里的回车不算同意；参数组合不对就拒绝。
func TestWelcomeCommandEdges(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.mustInstall()
	out, code := k.exec(strings.NewReader("\n"), k.home, k.binPath(), "start", "--welcome")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertInstalledNotStarted()
	for _, args := range [][]string{{"start", "--assume-yes"}, {"start", "--welcome", "--accept-bypass"}} {
		if out, code := k.agentq(k.home, args...); code == 0 {
			t.Fatalf("%v 应拒绝：\n%s", args, out)
		}
	}
	k.assertInstalledNotStarted()
	// 老入口的同意也记下来：之后 doctor、升级提示都靠它。
	if out, code := k.agentq(k.home, "start", "--accept-bypass"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertStarted("accept-bypass")
}

// 同意文字改版不重问，只在 doctor 里说明。
func TestDoctorMentionsConsentVersion(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.mustInstall("--yes")
	if out, code := k.agentq(k.home, "doctor"); code != 0 || strings.Contains(out, "同意文字") {
		t.Fatalf("版本一致不该说：%d\n%s", code, out)
	}
	testkit.WriteInstall(t, setup.ConsentPath(k.home), `{"text_version":0,"time":"t","method":"enter"}`, 0o600)
	out, code := k.agentq(k.home, "doctor")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "同意文字更新过")
}

// root：写任何文件之前拒绝，也不联网。
func TestInstallShRefusesRoot(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	k.writeBin("id", "#!/bin/sh\necho 0\n")
	tmp := strings.TrimPrefix(k.env[2], "TMPDIR=")
	out, code := k.run("--yes")
	if code == 0 {
		t.Fatalf("应拒绝：\n%s", out)
	}
	testkit.AssertContains(t, out, "root")
	for _, dir := range []string{k.home, tmp} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("%s 不该有东西：%v %v", dir, entries, err)
		}
	}
	if len(k.requests()) != 0 {
		t.Fatal("不该下载")
	}
}

// 进度：捕获时只有 ✓ 行，没有动画和颜色；终端里上色，NO_COLOR 不上色。
func TestInstallShProgress(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	out := k.mustInstall()
	testkit.AssertContains(t, out, "✓ 校验通过", "✓ 下载 "+k.platformFile())
	for _, bad := range []string{"\r", "\033[", "⠋"} {
		if strings.Contains(out, bad) {
			t.Fatalf("捕获的输出不该有 %q：\n%q", bad, out)
		}
	}
	k = newWelcomeKit(t)
	out, _ = k.runTTY(enterAt("n\n"))
	testkit.AssertContains(t, out, "\033[32m✓\033[0m 校验通过", "安装程序和技能")

	k = newWelcomeKit(t)
	k.env = append(k.env, "NO_COLOR=1")
	out, _ = k.runTTY(enterAt("\n"))
	testkit.AssertContains(t, out, "✓ 校验通过", "✓ 后台已启动")
	if strings.Contains(out, "\033[32m") {
		t.Fatalf("NO_COLOR 不该上色：\n%q", out)
	}
}

// mirrorServer 记下请求，按路径给文件。
func mirrorServer(t *testing.T, files map[string][]byte) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var hits []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hits...)
	}
}

// --mirror：程序从镜像取，校验文件从原地址取；镜像改过的文件被拒绝。
func TestInstallShMirror(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	const github = "https://github.com/some-owner/agentq/releases/latest/download/"
	setupKit := func(t *testing.T) (*installKit, []byte) {
		k := newInstallKit(t)
		k.fillRepo("some-owner/agentq")
		k.useFakeCurl()
		k.unsetBase()
		data := tarGz(t, "agentq", installBinary(t, "v1.0.0"))
		k.setFile("/some-owner/agentq/releases/latest/download/"+k.platformFile(), data)
		k.setFile("/some-owner/agentq/releases/latest/download/SHA256SUMS", []byte(sha256Hex(data)+"  "+k.platformFile()+"\n"))
		return k, data
	}
	t.Run("真文件装上", func(t *testing.T) {
		k, data := setupKit(t)
		mirror, _ := mirrorServer(t, map[string][]byte{"/" + github + k.platformFile(): data})
		out := k.mustInstall("--mirror", mirror.URL)
		testkit.AssertContains(t, out, "校验通过", "经镜像")
		calls := k.logFile("curl-calls")
		testkit.AssertContains(t, calls, mirror.URL+"/"+github+k.platformFile(), github+"SHA256SUMS")
		if strings.Contains(calls, mirror.URL+"/"+github+"SHA256SUMS") {
			t.Fatalf("校验文件不该走镜像：%s", calls)
		}
		if !exists(k.receiptPath()) {
			t.Fatal("没装上")
		}
	})
	for name, how := range map[string]func(k *installKit, url string) []string{
		"参数":   func(k *installKit, url string) []string { return []string{"--mirror=" + url} },
		"环境变量": func(k *installKit, url string) []string { k.env = append(k.env, "AGENTQ_MIRROR="+url); return nil },
	} {
		t.Run("镜像篡改被拒："+name, func(t *testing.T) {
			k, _ := setupKit(t)
			evil := tarGz(t, "agentq", []byte("#!/bin/sh\necho evil\n"))
			mirror, hits := mirrorServer(t, map[string][]byte{"/" + github + k.platformFile(): evil, "/" + github + "SHA256SUMS": []byte(sha256Hex(evil) + "  " + k.platformFile() + "\n")})
			out, code := k.run(how(k, mirror.URL)...)
			if code == 0 {
				t.Fatalf("应拒绝：\n%s", out)
			}
			testkit.AssertContains(t, out, "校验不过")
			k.assertNotInstalled()
			for _, h := range hits() {
				if strings.HasSuffix(h, "SHA256SUMS") {
					t.Fatalf("校验文件不该从镜像取：%v", hits())
				}
			}
		})
	}
	// 恶意镜像回 404 就能把脚本赶到源码路径：克隆下来直接构建运行，没有校验。所以源码路径一律不走镜像。
	sourceCases := map[string][]string{"回退到源码": nil, "--version main": {"--version", "main"}}
	for name, extra := range sourceCases {
		t.Run("源码不走镜像："+name, func(t *testing.T) {
			k := newInstallKit(t)
			k.fillRepo("some-owner/agentq")
			k.useFakeCurl()
			k.useFakeGoAndGit("v1.0.0")
			k.unsetBase()
			mirror, hits := mirrorServer(t, nil)
			out := k.mustInstall(append([]string{"--mirror", mirror.URL}, extra...)...)
			testkit.AssertContains(t, out, "从源码构建")
			calls := k.logFile("clone-calls")
			testkit.AssertContains(t, calls, "https://github.com/some-owner/agentq.git")
			if strings.Contains(calls, mirror.URL) {
				t.Fatalf("克隆不该走镜像：%s", calls)
			}
			for _, h := range hits() {
				if strings.Contains(h, ".git") {
					t.Fatalf("镜像不该收到克隆请求：%v", hits())
				}
			}
		})
	}
	t.Run("地址不对", func(t *testing.T) {
		k, _ := setupKit(t)
		out, code := k.run("--mirror", "ftp://x/")
		if code == 0 || !strings.Contains(out, "--mirror") {
			t.Fatalf("%d\n%s", code, out)
		}
		if k.logFile("curl-calls") != "" {
			t.Fatal("不该下载")
		}
	})
}

// --proxy：下载真的走代理（k.server 同时当代理用：代理请求的路径就是文件路径）。
func TestInstallShProxy(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for name, how := range map[string]func(k *installKit) []string{
		"参数":   func(k *installKit) []string { return []string{"--proxy", k.server.URL} },
		"环境变量": func(k *installKit) []string { k.env = append(k.env, "AGENTQ_PROXY="+k.server.URL); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			k := newInstallKit(t)
			k.serveRelease("v1.0.0")
			k.unsetBase()
			k.env = append(k.env, "AGENTQ_RELEASE_BASE=http://agentq-release.test")
			testkit.AssertContains(t, k.mustInstall(how(k)...), "校验通过")
			if got := k.requests(); len(got) < 2 {
				t.Fatalf("没走代理：%v", got)
			}
		})
	}
	k := newInstallKit(t)
	out, code := k.run("--proxy", "127.0.0.1:7890")
	if code == 0 || !strings.Contains(out, "--proxy") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// --version：和 AGENTQ_VERSION 一样取那个版本的目录，不传给 setup；main 走源码；缺值和非法值拒绝。
func TestInstallShVersionFlag(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	data := tarGz(t, "agentq", installBinary(t, "v1.2.3"))
	for _, args := range [][]string{{"--version", "v1.2.3"}, {"--version=v1.2.3"}} {
		k := newInstallKit(t)
		k.setFile("/v1.2.3/"+k.platformFile(), data)
		k.setFile("/v1.2.3/SHA256SUMS", []byte(sha256Hex(data)+"  "+k.platformFile()+"\n"))
		testkit.AssertContains(t, k.mustInstall(args...), "agentq v1.2.3")
	}
	t.Run("main 走源码", func(t *testing.T) {
		k := newInstallKit(t)
		k.fillRepo("some-owner/agentq")
		k.useFakeCurl()
		k.useFakeGoAndGit("v1.0.0")
		k.unsetBase()
		testkit.AssertContains(t, k.mustInstall("--version", "main"), "从源码构建")
		testkit.AssertContains(t, k.logFile("clone-calls"), "--branch main", "https://github.com/some-owner/agentq.git")
		if k.logFile("curl-calls") != "" {
			t.Fatalf("main 不该下载 Release：%s", k.logFile("curl-calls"))
		}
	})
	t.Run("main 没有 Go", func(t *testing.T) {
		k := newInstallKit(t)
		out, code := k.run("--version", "main")
		if code == 0 || !strings.Contains(out, "没有 Go") {
			t.Fatalf("%d\n%s", code, out)
		}
		if len(k.requests()) != 0 {
			t.Fatal("不该下载")
		}
	})
	for _, args := range [][]string{{"--version"}, {"--version", "../x"}, {"--proxy"}, {"--mirror"}} {
		k := newInstallKit(t)
		if out, code := k.run(args...); code == 0 {
			t.Fatalf("%v 应拒绝：\n%s", args, out)
		}
		k.assertNotInstalled()
	}
}

// --uninstall：转给 agentq remove --yes，数据保留；没装、手动删过程序都如实说。
func TestInstallShUninstall(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	t.Run("正常卸载", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall("--yes")
		job := filepath.Join(k.home, ".agentq", "jobs", "old", "artifact")
		testkit.WriteInstall(t, job, "keep", 0o600)
		cfg := string(testkit.ReadBytes(t, k.configPath()))
		before := len(k.requests())
		out := k.mustInstall("--uninstall")
		testkit.AssertContains(t, out, "卸载了", "配置和任务数据还在", "restore --yes")
		if len(k.requests()) != before {
			t.Fatal("卸载不该下载")
		}
		k.assertNotInstalled()
		if exists(filepath.Join(k.home, "fake-launchd", config.LaunchdLabel)) {
			t.Fatal("服务应卸掉")
		}
		testkit.AssertBytes(t, job, "keep")
		testkit.AssertBytes(t, k.configPath(), cfg)
	})
	t.Run("没装过", func(t *testing.T) {
		k := newInstallKit(t)
		testkit.AssertContains(t, k.mustInstall("--uninstall"), "没有装 agentq")
		if entries, _ := os.ReadDir(k.home); len(entries) != 0 {
			t.Fatalf("不该写文件：%v", entries)
		}
	})
	t.Run("程序被手动删了", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		if err := os.Remove(k.binPath()); err != nil {
			t.Fatal(err)
		}
		out, code := k.run("--uninstall")
		if code == 0 {
			t.Fatalf("不能假装卸干净：\n%s", out)
		}
		testkit.AssertContains(t, out, "手动删过", "launchctl list", "没有卸载")
	})
	t.Run("卸载失败不说成功", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		testkit.WriteInstall(t, filepath.Join(k.home, ".agentq", "state.json"), `{"jobs":[{"status":"running"}]}`, 0o600)
		out, code := k.run("--uninstall")
		if code == 0 || strings.Contains(out, "卸载了") {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "jobs still running", "没有完成")
		if !exists(k.receiptPath()) {
			t.Fatal("不该卸掉")
		}
	})
}

// serveUpdate 放新版本：程序、脚本、校验文件。
func (k *installKit) serveUpdate(version string, script []byte) {
	k.t.Helper()
	data := tarGz(k.t, "agentq", installBinary(k.t, version))
	k.setFile("/"+k.platformFile(), data)
	k.setFile("/install.sh", script)
	k.setFile("/SHA256SUMS", []byte(sha256Hex(data)+"  "+k.platformFile()+"\n"+sha256Hex(script)+"  install.sh\n"))
}

// agentq update：对过校验才运行脚本，升级走同一条路；脚本被改了就不运行；没有地址时只说明。
func TestAgentqUpdate(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	t.Run("升级", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		k.serveUpdate("v2.0.0", testkit.ReadBytes(t, k.script))
		out, code := k.agentq(k.home, "update")
		if code != 0 {
			t.Fatalf("%d\n%s", code, out)
		}
		testkit.AssertContains(t, out, "已升级", "v2.0.0")
		if got, _ := k.agentq(k.home, "version"); !strings.Contains(got, "v2.0.0") {
			t.Fatalf("没升上去：%s", got)
		}
	})
	t.Run("脚本校验不过", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		k.serveUpdate("v2.0.0", testkit.ReadBytes(t, k.script))
		marker := filepath.Join(k.home, "evil-ran")
		k.setFile("/install.sh", []byte("#!/bin/sh\ntouch "+fsx.ShQuote(marker)+"\n"))
		out, code := k.agentq(k.home, "update")
		if code == 0 {
			t.Fatalf("应拒绝：\n%s", out)
		}
		testkit.AssertContains(t, out, "校验不过")
		if exists(marker) {
			t.Fatal("不该运行被改过的脚本")
		}
		if got, _ := k.agentq(k.home, "version"); !strings.Contains(got, "v1.0.0") {
			t.Fatalf("不该升级：%s", got)
		}
	})
	t.Run("源码构建只说明", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		k.unsetBase()
		before := len(k.requests())
		out, code := k.agentq(k.home, "update")
		if code == 0 {
			t.Fatalf("没升级不该退出 0：\n%s", out)
		}
		testkit.AssertContains(t, out, "不猜", "git pull", "sh install.sh")
		if len(k.requests()) != before {
			t.Fatal("不该联网")
		}
	})
	t.Run("参数不对", func(t *testing.T) {
		k := newWelcomeKit(t)
		k.mustInstall()
		for _, args := range [][]string{{"update", "--version", "../x"}, {"update", "多余"}} {
			if out, code := k.agentq(k.home, args...); code == 0 {
				t.Fatalf("%v 应拒绝：\n%s", args, out)
			}
		}
	})
}

// HOME 没设：退出非 0，什么都没做。macOS 的 sh 下 ${HOME:?} 加 EXIT trap 退出码是 0，脚本里也不留这种写法。
func TestInstallShRefusesWithoutHome(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newInstallKit(t)
	k.serveRelease("v1.0.0")
	var kept []string
	for _, kv := range k.env {
		if !strings.HasPrefix(kv, "HOME=") {
			kept = append(kept, kv)
		}
	}
	k.env = kept
	out, code := k.run()
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "没有设置 HOME")
	if len(k.requests()) != 0 {
		t.Fatal("不该下载")
	}
	re := regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:?\?`)
	for n, line := range strings.Split(testkit.ReadFile(t, "install.sh"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && re.MatchString(line) {
			t.Errorf("install.sh:%d 不要用 ${变量:?}：%s", n+1, line)
		}
	}
}

// macOS 的 sh（bash 3.2）在 UTF-8 环境下把变量后面紧跟的中文标点当成变量名的一部分，set -u 时直接退出。脚本里这种位置都要写 ${变量}。
func TestShellScriptsBraceVarsBeforeMultibyte(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	re := regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*[^\x00-\x7f]`)
	scripts, err := filepath.Glob(filepath.Join("scripts", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("%v %v", scripts, err)
	}
	for _, p := range append(scripts, "install.sh") {
		for n, line := range strings.Split(testkit.ReadFile(t, p), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := re.FindString(line); m != "" {
				t.Errorf("%s:%d：%s 要写成 ${变量}", p, n+1, m)
			}
		}
	}
}
