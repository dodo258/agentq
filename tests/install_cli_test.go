package tests

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/fsx"
	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

// 官方命令，和 code.claude.com/docs/en/setup 一致。
const (
	officialInstallCommand = "curl -fsSL https://claude.ai/install.sh | bash"
	officialBrewCommand    = "brew install --cask claude-code"
	officialNpmCommand     = "npm install -g @anthropic-ai/claude-code"
	officialLoginCommand   = "claude auth login"
)

// 假 claude：登录状态是 $HOME 下的一个文件（服务环境只带 HOME 和 PATH），auth login 建它。按官方文档，没登录时 auth status 退出 1。
// 其余和集成测试的假 CLI 一样。
var loginFakeClaude = strings.Replace(installFakeClaude, "#!/bin/sh\n", `#!/bin/sh
if [ "$1 $2" = "auth login" ]; then
  echo login >> "$HOME/fake-login-calls"
  [ -f "$HOME/fake-login-fails" ] && { echo 'login cancelled' >&2; exit 1; }
  : > "$HOME/fake-logged-in"
  echo 'Login successful'
  exit 0
fi
if [ "$1 $2" = "auth status" ]; then
  if [ -f "$HOME/fake-logged-in" ]; then echo '{"loggedIn": true, "authMethod": "claude.ai"}'; exit 0; fi
  echo '{"loggedIn": false, "authMethod": "none"}'
  exit 1
fi
`, 1)

// useFakeOfficialInstaller：假 curl 收到 claude.ai 的地址时给一段脚本，它把假 CLI 装到 ~/.local/bin/claude，和官方安装位置一样。
// failure 非空时：curl 按这段话失败。broken 为 true：脚本退出非 0。noBinary 为 true：脚本退出 0 但什么都没装。
func (k *installKit) useFakeOfficialInstaller(mode string) {
	k.t.Helper()
	dist := filepath.Join(k.log, "claude-dist")
	testkit.WriteInstall(k.t, dist, loginFakeClaude, 0o755)
	script := "mkdir -p \"$HOME/.local/bin\" && cp " + fsx.ShQuote(dist) + " \"$HOME/.local/bin/claude\" && echo 'Claude Code successfully installed!'\n"
	switch mode {
	case "broken":
		script = "echo 'Unsupported platform' >&2; exit 1\n"
	case "no-binary":
		script = "echo 'pretending to install'\n"
	}
	testkit.WriteInstall(k.t, filepath.Join(k.log, "official.sh"), script, 0o644)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		k.t.Skip("这台机器没有 curl")
	}
	failure := ""
	if mode == "curl-403" {
		failure = "echo 'curl: (22) The requested URL returned error: 403' >&2; exit 22"
	}
	k.writeBin("curl", `#!/bin/sh
echo "$*" >> "$FAKE_LOG_DIR/curl-calls"
case "$*" in
  *https://claude.ai/install.sh*)
    `+failure+`
    cat "$FAKE_LOG_DIR/official.sh"
    exit 0 ;;
esac
exec `+fsx.ShQuote(realCurl)+` "$@"
`)
}

// newNoCLIKit：PATH 里没有 claude 的临时 HOME，shell 是 zsh。
func newNoCLIKit(t *testing.T, mode string) *installKit {
	t.Helper()
	k := newInstallKit(t, "claude")
	k.useFakeOfficialInstaller(mode)
	k.serveRelease("v1.0.0")
	k.env = append(k.env, "SHELL=/bin/zsh")
	return k
}

func (k *installKit) officialCalls() int {
	return strings.Count(k.logFile("curl-calls"), "https://claude.ai/install.sh")
}

func (k *installKit) loginCalls() int {
	b, _ := os.ReadFile(filepath.Join(k.home, "fake-login-calls"))
	return strings.Count(string(b), "login")
}

// 验收：只有 Claude.app 的临时 HOME，只按回车：假 curl 收到官方地址，装好 CLI，登录，后台开了，没待做就不调用模型。
// 桌面端私有目录里的程序一次都不碰。卸载后 rc 文件和原来逐字节相同。
func TestOnlyDesktopAppWalkthrough(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	app := filepath.Join(k.home, "Applications", "Claude.app", "Contents", "MacOS", "claude")
	touched := filepath.Join(k.home, "desktop-touched")
	testkit.WriteInstall(t, app, "#!/bin/sh\n: > "+fsx.ShQuote(touched)+"\necho '9.9.9 (Claude Code)'\n", 0o755)
	rc := filepath.Join(k.home, ".zshrc")
	original := "# 我的配置\nalias ll='ls -l'\n"
	testkit.WriteInstall(t, rc, original, 0o640)

	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "检测到 Claude 桌面端", "还没有官方 Claude CLI", officialInstallCommand, "打开浏览器", "~/.zshrc", "Claude Code successfully installed!", "Login successful", onboard.IdleHint)
	if k.officialCalls() != 1 {
		t.Fatalf("官方地址没收到或收到多次：%s", k.logFile("curl-calls"))
	}
	if k.loginCalls() != 1 {
		t.Fatalf("登录次数不对：%d", k.loginCalls())
	}
	if exists(touched) {
		t.Fatal("不该调用桌面端里的程序")
	}
	k.assertStarted("install")
	k.assertNoClaudeWork()
	cli := filepath.Join(k.home, ".local", "bin", "claude")
	if got := k.installedConfig().ClaudePath; got != cli {
		t.Fatalf("claude_path 应记下真实路径：%q", got)
	}
	got := string(testkit.ReadBytes(t, rc))
	if !strings.HasPrefix(got, original) || !strings.Contains(got, "agentq managed") || strings.Count(got, "\n") != strings.Count(original, "\n")+1 {
		t.Fatalf("rc 应该只多一行：\n%s", got)
	}
	testkit.AssertBytes(t, rc+".pre-agentq", original)

	out = k.mustInstall("--uninstall")
	testkit.AssertContains(t, out, "改回了原样")
	testkit.AssertBytes(t, rc, original)
	if st, err := os.Stat(rc); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("权限变了：%v %v", st, err)
	}
	if exists(rc + ".pre-agentq") {
		t.Fatal("一模一样的备份不用留")
	}
}

// 什么都没有：一样走完。没有 rc 文件时建一个，卸载时删掉。
func TestNothingInstalledWalkthrough(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertStarted("install")
	rc := filepath.Join(k.home, ".zshrc")
	if !exists(rc) {
		t.Fatal("应建 rc")
	}
	k.mustInstall("--uninstall")
	if exists(rc) || exists(rc+".pre-agentq") {
		t.Fatal("rc 是我们建的，卸载应删掉，也不留备份")
	}
}

// CLI 在、没登录：同意后运行 claude auth login，不下载官方脚本。
func TestLoggedOutCLILogsIn(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	k.writeBin("claude", loginFakeClaude)
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "打开浏览器", "Login successful")
	if strings.Contains(out, "还要安装官方") || k.officialCalls() != 0 {
		t.Fatalf("CLI 在就不用装：\n%s", out)
	}
	k.assertStarted("install")
}

// 登录失败：后台不开，说明原因。安装就是同意，同意和权限已经记下；弄好登录后派活技能查到 not-running，agentq start 直接拉起，不用再同意。
func TestLoginFailureKeepsBackgroundOff(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	k.writeBin("claude", loginFakeClaude)
	testkit.WriteInstall(t, filepath.Join(k.home, "fake-login-fails"), "", 0o644)
	out, code := k.runTTY(nil)
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "login cancelled", "还没登录，后台没开", "claude auth login", "agentq 已经装好", "后台会自己开起来")
	k.assertConsentedNotStarted()
	if err := os.Remove(filepath.Join(k.home, "fake-login-fails")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteInstall(t, filepath.Join(k.home, "fake-logged-in"), "", 0o644)
	if out, code := k.agentq(k.home, "start", "--check"); code != 0 || !strings.HasPrefix(out, "not-running\n") {
		t.Fatalf("%d\n%s", code, out)
	}
	if out, code := k.agentq(k.home, "start"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertStarted("install")
}

// 官方安装失败：停，贴原文，给 Homebrew 和 npm；后台没开，配置里的 CLI 路径不变。
func TestOfficialInstallFailure(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for mode, want := range map[string]string{
		"curl-403":  "returned error: 403",
		"broken":    "Unsupported platform",
		"no-binary": "还是找不到 claude",
	} {
		t.Run(mode, func(t *testing.T) {
			k := newNoCLIKit(t, mode)
			out, code := k.runTTY(nil)
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, want, officialBrewCommand, officialNpmCommand, officialLoginCommand, "agentq 已经装好")
			k.assertConsentedNotStarted()
			if k.loginCalls() != 0 {
				t.Fatal("没装上不该登录")
			}
			cfg := string(testkit.ReadBytes(t, k.configPath()))
			if !strings.Contains(cfg, `"claude_path": "claude"`) {
				t.Fatalf("CLI 路径不该变：%s", cfg)
			}
		})
	}
}

// --paused：只装。不装 CLI、不登录、不动 rc、不记同意，给后续命令；升级不打开。之后自己装好 CLI 再 agentq start 能开。
func TestDeclineLeavesCLIAlone(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	out, code := k.runTTY(nil, "--paused")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "--paused 只安装", officialInstallCommand, officialLoginCommand, "agentq start")
	k.assertInstalledNotStarted()
	if k.officialCalls() != 0 || k.loginCalls() != 0 || exists(filepath.Join(k.home, ".zshrc")) {
		t.Fatalf("不该动：\n%s", k.logFile("curl-calls"))
	}
	if out, code := k.agentq(k.home, "start", "--check"); code != 0 || !strings.HasPrefix(out, "need-cli\n") {
		t.Fatalf("%d\n%s", code, out)
	}
	k.serveRelease("v2.0.0")
	out = k.mustInstall()
	testkit.AssertContains(t, out, "已升级", "升级不替你打开", "agentq start")
	k.assertInstalledNotStarted()
	dist := filepath.Join(k.log, "claude-dist")
	testkit.WriteInstall(t, filepath.Join(k.home, ".local", "bin", "claude"), string(testkit.ReadBytes(t, dist)), 0o755)
	testkit.WriteInstall(t, filepath.Join(k.home, "fake-logged-in"), "", 0o644)
	if out, code := k.agentq(k.home, "start", "--check"); code != 0 || !strings.HasPrefix(out, "off\n") {
		t.Fatalf("先不跑的不替他开：%d\n%s", code, out)
	}
	if out, code := k.agentq(k.home, "start", "--accept-bypass"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	k.assertStarted("accept-bypass")
	if got := k.installedConfig().ClaudePath; got != filepath.Join(k.home, ".local", "bin", "claude") {
		t.Fatalf("claude_path 应记下真实路径：%q", got)
	}
}

// 没有终端、CI：不装 CLI、不登录（登录离不开终端，也不悄悄联网装程序），给官方命令，退出非 0；同意已记下，弄好后派活技能自己拉起后台。
func TestNoTerminalPrintsOfficialCommand(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for name, env := range map[string][]string{"无 tty": nil, "CI": {"CI=1"}} {
		t.Run(name, func(t *testing.T) {
			k := newNoCLIKit(t, "")
			k.env = append(k.env, env...)
			out, code := k.run()
			if code == 0 {
				t.Fatalf("应退出非 0：\n%s", out)
			}
			testkit.AssertContains(t, out, officialInstallCommand, officialLoginCommand, "这里没有终端", "agentq 已经装好", "后台会自己开起来")
			if strings.Contains(strings.ToLower(out), "token") {
				t.Fatalf("不该提 token：\n%s", out)
			}
			if k.officialCalls() != 0 || k.loginCalls() != 0 {
				t.Fatalf("官方 %d 登录 %d\n%s", k.officialCalls(), k.loginCalls(), out)
			}
			k.assertConsentedNotStarted()
		})
	}
}

// 有终端：装 CLI、登录、开后台，一个回车都不用按。旧的 --yes 照样认。
func TestAssumeYesWithoutCLI(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for name, args := range map[string][]string{"不带参数": nil, "--yes": {"--yes"}} {
		t.Run(name, func(t *testing.T) {
			k := newNoCLIKit(t, "")
			out, code := k.runTTY(nil, args...)
			if code != 0 {
				t.Fatalf("%d\n%s", code, out)
			}
			if k.officialCalls() != 1 || k.loginCalls() != 1 {
				t.Fatalf("官方 %d 登录 %d\n%s", k.officialCalls(), k.loginCalls(), out)
			}
			k.assertStarted("install")
		})
	}
}

// 直接 agentq start：没有 CLI 或没登录时，什么都没改，给官方命令。
func TestPlainStartExplainsMissingCLI(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newNoCLIKit(t, "")
	k.mustInstall("--paused")
	before := string(testkit.ReadBytes(t, k.configPath()))
	out, code := k.agentq(k.home, "start", "--accept-bypass")
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "什么都没改", officialInstallCommand, officialLoginCommand)
	testkit.AssertBytes(t, k.configPath(), before)
}

// PATH 已有 ~/.local/bin：不动 rc，同意文字也不提。
func TestPathAlreadySetLeavesRCAlone(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.env[1] = "PATH=" + k.bin + ":" + filepath.Join(k.home, ".local", "bin") + ":" + k.tools
	k.env = append(k.env, "SHELL=/bin/zsh")
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	if strings.Contains(out, ".zshrc") || exists(filepath.Join(k.home, ".zshrc")) {
		t.Fatalf("不该动 rc：\n%s", out)
	}
	k.assertStarted("install")
}

// 别的 shell：不改配置文件，给手动的那一行。
func TestOtherShellGetsManualLine(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.env = append(k.env, "SHELL=/usr/bin/fish")
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "fish", `export PATH="$HOME/.local/bin:$PATH"`)
	k.assertStarted("install")
	entries, err := os.ReadDir(k.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "rc") && strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("不该建 %s", e.Name())
		}
	}
}

// rc 里只是提到 .local/bin：照样加那一行，卸载后逐字节恢复。
func TestRCMentionIsNotEnough(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.env = append(k.env, "SHELL=/bin/zsh")
	rc := filepath.Join(k.home, ".zshrc")
	original := "alias foo=~/.local/bin/foo\n"
	testkit.WriteInstall(t, rc, original, 0o644)
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "往 ~/.zshrc 加带标记", "加进了 ~/.zshrc")
	if got := string(testkit.ReadBytes(t, rc)); !strings.Contains(got, "agentq managed") {
		t.Fatalf("应加那一行：\n%s", got)
	}
	k.mustInstall("--uninstall")
	testkit.AssertBytes(t, rc, original)
}

// rc 里已经赋值过 PATH，只是这个终端还没读到：不改，但要说一声。
func TestRCAlreadyAssignsGetsHint(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.env = append(k.env, "SHELL=/bin/zsh")
	rc := filepath.Join(k.home, ".zshrc")
	original := "export PATH=\"$HOME/.local/bin:$PATH\"\n"
	testkit.WriteInstall(t, rc, original, 0o644)
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "~/.zshrc 里已经把 ~/.local/bin 放进 PATH", "~/.local/bin/agentq")
	if strings.Contains(out, "往 ~/.zshrc 加带标记") {
		t.Fatalf("不会改就不该说要改：\n%s", out)
	}
	testkit.AssertBytes(t, rc, original)
}

// 卸载时改不了 rc：如实说程序已经卸了、rc 没改回去。修好后重跑接着清那一行，不说「没有装」。
func TestUninstallRCFailureIsHonest(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	k := newWelcomeKit(t)
	k.env = append(k.env, "SHELL=/bin/zsh")
	rc := filepath.Join(k.home, ".zshrc")
	original := "# 我的配置\n"
	testkit.WriteInstall(t, rc, original, 0o644)
	k.mustInstall("--yes")
	if !strings.Contains(string(testkit.ReadBytes(t, rc)), "agentq managed") {
		t.Fatal("应该加了那一行")
	}
	if err := os.Chmod(rc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(rc, 0o644) })

	out, code := k.run("--uninstall")
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "程序和后台服务已经卸载", "没改回去")
	if strings.Contains(out, "什么都没有删") {
		t.Fatalf("程序已经卸了，不能这么说：\n%s", out)
	}
	k.assertNotInstalled()

	// 还没修好时重跑：还是如实说，不说「没有装」。
	out, code = k.run("--uninstall")
	if code == 0 || strings.Contains(out, "没有装") {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "agentq managed", "没有清掉")

	if err := os.Chmod(rc, 0o644); err != nil {
		t.Fatal(err)
	}
	out = k.mustInstall("--uninstall")
	testkit.AssertContains(t, out, "改回了原样")
	if strings.Contains(out, "没有装") {
		t.Fatalf("不该说没有装：\n%s", out)
	}
	testkit.AssertBytes(t, rc, original)
	// 这回才真的什么都没有了。
	testkit.AssertContains(t, k.mustInstall("--uninstall"), "没有装")
}

// 保留的那一代也被删了：如实说还留着那一行，怎么手动处理。
func TestUninstallRCFailureWithoutGenerations(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	k := newWelcomeKit(t)
	k.env = append(k.env, "SHELL=/bin/zsh")
	rc := filepath.Join(k.home, ".zshrc")
	testkit.WriteInstall(t, rc, "# 我的配置\n", 0o644)
	k.mustInstall("--yes")
	if err := os.Chmod(rc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(rc, 0o644) })
	if out, code := k.run("--uninstall"); code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	if err := os.RemoveAll(filepath.Join(k.home, ".local", "share", "agentq", "generations")); err != nil {
		t.Fatal(err)
	}
	out, code := k.run("--uninstall")
	if code == 0 || strings.Contains(out, "没有装") {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "agentq managed", "pre-agentq")
}

// 技能里的 agentq 命令写兜底路径：桌面会话不一定读到新的 PATH。
func TestSkillsCarryAgentqFallbackPath(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	for _, name := range []string{"dispatch", "project-status"} {
		testkit.AssertContains(t, testkit.ReadFile(t, filepath.Join("skills", name, "SKILL.md")), "$HOME/.local/bin/agentq")
	}
}

// consent.json 只记三样：文案版本、时间、方式。
func TestConsentFileShape(t *testing.T) {
	t.Parallel() // 每个用例都真装一遍要几秒，并行才不把根包的时限吃光
	k := newWelcomeKit(t)
	k.mustInstall("--yes")
	var raw map[string]any
	if err := json.Unmarshal(testkit.ReadBytes(t, setup.ConsentPath(k.home)), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("字段不对：%v", raw)
	}
}

// 不是这个脚本装的旧版：没有回执，程序还在，remove 失败。什么都没动，不能说「已经卸载」。
func TestUninstallLegacyBinaryIsHonest(t *testing.T) {
	t.Parallel()
	k := newWelcomeKit(t)
	bin := filepath.Join(k.home, ".local", "bin", "agentq")
	testkit.WriteInstall(t, bin, "#!/bin/sh\necho unknown command remove >&2\nexit 2\n", 0o755)

	out, code := k.run("--uninstall")
	if code == 0 {
		t.Fatalf("应退出非 0：\n%s", out)
	}
	testkit.AssertContains(t, out, "什么都没有删", "agentq.legacy")
	if strings.Contains(out, "已经卸载") {
		t.Fatalf("程序还在，不能说已经卸载：\n%s", out)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("程序不该被动：%v", err)
	}
}
