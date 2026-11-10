package tests

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

func rulesDir(home string) string { return filepath.Join(home, ".claude", "rules", "agentq") }

func assertRuleFiles(t *testing.T, home string, want bool) {
	t.Helper()
	got := exists(filepath.Join(rulesDir(home), "core.md"))
	if got != want {
		t.Fatalf("core.md 装了=%v，应该=%v", got, want)
	}
}

// agentq env：空 HOME 装上；不写用户的 CLAUDE.md、settings.json；改过的文件 off 时留着并报告。
func TestEnvCommandOnOff(t *testing.T) {
	a, i, _, out := onboardApp(t)
	if err := a.Onboard.CmdEnv([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "没装")
	assertRuleFiles(t, i.Home, false)
	out.Reset()

	if err := a.Onboard.CmdEnv([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "core.md", "已装到")
	err := fs.WalkDir(installAssets, "rules", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := installAssets.ReadFile(p)
		if err != nil {
			return err
		}
		testkit.AssertBytes(t, filepath.Join(rulesDir(i.Home), filepath.Base(p)), string(want))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"CLAUDE.md", "settings.json"} {
		if exists(filepath.Join(i.Home, ".claude", p)) {
			t.Fatalf("不该写用户的 %s", p)
		}
	}
	// agentq 的规则不算用户规则，也不影响安装引擎的冲突判断
	modified := filepath.Join(rulesDir(i.Home), "go.md")
	testkit.WriteInstall(t, modified, "我改过\n", 0o644)
	if err := i.Doctor(); err != nil {
		t.Fatalf("改了规则不该让 doctor 报冲突：%v", err)
	}
	out.Reset()
	if err := a.Onboard.CmdEnv([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "开着", "go.md：内容和 agentq 写的不一样", "core.md：和 agentq 写的一样")
	if strings.Contains(out.String(), "你自己的全局规则") {
		t.Fatalf("agentq 的文件不算用户规则：\n%s", out)
	}
	out.Reset()

	if err := a.Onboard.CmdEnv([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "已删", "core.md", "go.md 你改过，留着没动")
	assertRuleFiles(t, i.Home, false)
	testkit.AssertBytes(t, modified, "我改过\n")

	if err := a.Onboard.CmdEnv([]string{"nope"}); err == nil {
		t.Fatal("应报用法")
	}
	noArgs := exec.Command(agentqBin, "env")
	noArgs.Env = append(os.Environ(), "HOME="+t.TempDir())
	_ = noArgs.Run()
	if code := noArgs.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("没写子命令应该报用法：%d", code)
	}
}

// 卸载：只删没改过的，改过的留着；恢复卸载把规则装回来，改过的不盖。没开过的卸载、恢复都不加。升级也不加。
func TestEnvLayerFollowsInstallLifecycle(t *testing.T) {
	a, i, _, _ := onboardApp(t)
	var notes bytes.Buffer
	i.Out, i.Notes = &notes, &notes
	if err := a.Onboard.CmdEnv([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	modified := filepath.Join(rulesDir(i.Home), "go.md")
	testkit.WriteInstall(t, modified, "我改过\n", 0o644)
	if err := i.Setup(setup.SetupOptions{Scope: "full", Yes: true, Services: true}); err != nil {
		t.Fatalf("改了规则不该挡住升级：%v", err)
	}
	testkit.AssertContains(t, notes.String(), "go.md 你改过，这次升级没动它")
	testkit.AssertBytes(t, modified, "我改过\n")
	notes.Reset()

	if err := i.Remove(true); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, notes.String(), "通用工程规则已删", "go.md 你改过，留着")
	assertRuleFiles(t, i.Home, false)
	testkit.AssertBytes(t, modified, "我改过\n")

	if err := i.Restore(true); err != nil {
		t.Fatal(err)
	}
	assertRuleFiles(t, i.Home, true)
	testkit.AssertBytes(t, modified, "我改过\n")

	// 关掉之后：升级、卸载、恢复都不加回来
	if err := a.Onboard.CmdEnv([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if err := i.Setup(setup.SetupOptions{Scope: "full", Yes: true, Services: true}); err != nil {
		t.Fatal(err)
	}
	assertRuleFiles(t, i.Home, false)
	if err := i.Remove(true); err != nil {
		t.Fatal(err)
	}
	if err := i.Restore(true); err != nil {
		t.Fatal(err)
	}
	assertRuleFiles(t, i.Home, false)
}

// 审查复现的两种：删一个文件、删整个目录，再升级都不写回。
func TestSetupDoesNotRestoreDeletedRules(t *testing.T) {
	a, i, _, _ := onboardApp(t)
	var notes bytes.Buffer
	i.Out, i.Notes = &notes, &notes
	upgrade := func() {
		t.Helper()
		if err := i.Setup(setup.SetupOptions{Scope: "full", Yes: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Onboard.CmdEnv([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rulesDir(i.Home), "go.md")); err != nil {
		t.Fatal(err)
	}
	upgrade()
	testkit.AssertContains(t, notes.String(), "go.md 你删了，升级不写回")
	upgrade()
	if exists(filepath.Join(rulesDir(i.Home), "go.md")) {
		t.Fatal("go.md 被写回了")
	}
	assertRuleFiles(t, i.Home, true)
	notes.Reset()
	if err := os.RemoveAll(rulesDir(i.Home)); err != nil {
		t.Fatal(err)
	}
	upgrade()
	testkit.AssertContains(t, notes.String(), "当作不要了")
	upgrade()
	if exists(rulesDir(i.Home)) {
		t.Fatal("目录被写回了")
	}
	if err := a.Onboard.CmdEnv([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	assertRuleFiles(t, i.Home, true)
	if !exists(filepath.Join(rulesDir(i.Home), "go.md")) {
		t.Fatal("env on 要补全")
	}
}

// agentq start：第一次同意才装；已有用户规则不装，说一句；已经同意过的（升级上来的）不加。
func TestStartFirstConsentEnablesRules(t *testing.T) {
	start := func(t *testing.T, a *App, i *setup.Installer) {
		t.Helper()
		if err := a.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("第一次同意", func(t *testing.T) {
		a, i, _, out := onboardApp(t)
		start(t, a, i)
		assertRuleFiles(t, i.Home, true)
		testkit.AssertContains(t, out.String(), "所有项目的会话都会读到", "agentq env off")
		// 关掉以后再 start：不加回来
		if err := a.Onboard.CmdEnv([]string{"off"}); err != nil {
			t.Fatal(err)
		}
		start(t, a, i)
		assertRuleFiles(t, i.Home, false)
	})
	t.Run("已有用户规则", func(t *testing.T) {
		a, i, _, out := onboardApp(t)
		mine := filepath.Join(i.Home, ".claude", "CLAUDE.md")
		testkit.WriteInstall(t, mine, "我的规则\n", 0o644)
		start(t, a, i)
		assertRuleFiles(t, i.Home, false)
		testkit.AssertContains(t, out.String(), "检测到你已有全局规则，保留你的，不加 agentq 的。要加：agentq env on")
		testkit.AssertBytes(t, mine, "我的规则\n")
		// 明确要加：加上，用户的不动
		if err := a.Onboard.CmdEnv([]string{"on"}); err != nil {
			t.Fatal(err)
		}
		assertRuleFiles(t, i.Home, true)
		testkit.AssertBytes(t, mine, "我的规则\n")
	})
	t.Run("升级上来的", func(t *testing.T) {
		a, i, _, out := onboardApp(t)
		if err := setup.WriteConsent(i.Home, setup.Consent{TextVersion: setup.ConsentTextVersion, Time: time.Now().UTC().Format(time.RFC3339), Method: "enter"}); err != nil {
			t.Fatal(err)
		}
		start(t, a, i)
		assertRuleFiles(t, i.Home, false)
		if strings.Contains(out.String(), "通用工程规则") {
			t.Fatalf("升级不该提规则：\n%s", out)
		}
	})
}

// 真走一遍安装脚本：装完即运行，规则装上并说一句。
func TestWelcomeInstallsRules(t *testing.T) {
	t.Parallel() // 真装一遍要几秒
	k := newWelcomeKit(t)
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "通用工程规则放到了 ~/.claude/rules/agentq/")
	assertRuleFiles(t, k.home, true)
	if exists(filepath.Join(k.home, ".claude", "CLAUDE.md")) {
		t.Fatal("不该写用户的 CLAUDE.md")
	}
	// 卸载走真命令：删掉规则
	if b, code := k.agentq(k.home, "remove", "--yes"); code != 0 {
		t.Fatalf("%d\n%s", code, b)
	}
	assertRuleFiles(t, k.home, false)
}

// 已有全局规则：装完的告知里说明，不装。
func TestWelcomeKeepsExistingRules(t *testing.T) {
	t.Parallel() // 真装一遍要几秒
	k := newWelcomeKit(t)
	mine := filepath.Join(k.home, ".claude", "rules", "mine.md")
	testkit.WriteInstall(t, mine, "我的规则\n", 0o644)
	out, code := k.runTTY(nil)
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "检测到你已有全局规则，保留你的，不加 agentq 的。要加：agentq env on")
	assertRuleFiles(t, k.home, false)
	testkit.AssertBytes(t, mine, "我的规则\n")
	if !exists(setup.ConsentPath(k.home)) {
		t.Fatal(os.ErrNotExist)
	}
}
