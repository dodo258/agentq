package tests

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/service"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

func TestBootstrapAcceptance(t *testing.T) {
	if os.Getenv("AGENTQ_BOOTSTRAP_ACCEPTANCE") != "1" {
		t.Skip("opt-in network acceptance for the formal bootstrap entry")
	}
	i, o := installFixture(t)
	source, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	env := setup.InstallEnv(i.Home)
	run := func(args ...string) []byte {
		t.Helper()
		c := exec.Command("/bin/sh", append([]string{filepath.Join(source, "scripts", "bootstrap.sh")}, args...)...)
		c.Env = env
		c.Dir = source
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("bootstrap: %v\n%s", e, b)
		}
		return b
	}
	// 对正式入口本身验收，而非仅测安装引擎。
	run("--yes", "--claude", o.Claude, "--extras")
	r := mustReceipt(t, i)
	if !r.Extras {
		t.Fatal("extras not selected")
	}
	for _, name := range []string{"check", "think", "hunt", "ui", "read", "write", "learn", "health"} {
		if _, e = os.Stat(filepath.Join(i.Home, ".claude", "skills", name, "SKILL.md")); e != nil {
			t.Fatal(e)
		}
	}
	run("--yes")
	updated := mustReceipt(t, i)
	if !updated.Extras {
		t.Fatal("update lost extras")
	}
	run("rollback", "--yes")
	if mustReceipt(t, i).Generation != r.Generation {
		t.Fatal("entry rollback mismatch")
	}
	run("remove", "--yes")
	if _, e = os.Stat(i.ReceiptPath()); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("entry uninstall failed")
	}
	run("restore", "--yes")
	if e = i.CheckOwned(mustReceipt(t, i)); e != nil {
		t.Fatal(e)
	}
	t.Log("formal bootstrap full/extras/repeat/rollback/remove/restore passed; no Telegram or launchd")
}
func TestSettingsShapeAndAncestorConflict(t *testing.T) {
	for _, payload := range []string{`{"env":{"TOKEN":123}}`, `{"hooks":"bad"}`, `{"permissions":{"allow":"all"}}`, `{"statusLine":true}`, `null`} {
		t.Run(payload, func(t *testing.T) {
			i, o := installFixture(t)
			p := filepath.Join(i.Home, ".claude", "settings.json")
			testkit.WriteInstall(t, p, payload, 0600)
			if e := i.Setup(o); e == nil {
				t.Fatal("invalid settings accepted")
			}
			testkit.AssertBytes(t, p, payload)
		})
	}
	i, o := installFixture(t)
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(i.Home, ".claude"), 0700)
	os.Symlink(outside, filepath.Join(i.Home, ".claude", "skills"))
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "ancestor") {
		t.Fatalf("ancestor conflict: %v", e)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote through external ancestor")
	}
}
func TestInstallAndTickMutuallyExclusive(t *testing.T) {
	i, o := installFixture(t)
	os.MkdirAll(filepath.Join(i.Home, ".agentq"), 0700)
	lock, e := fsx.AcquireLock(filepath.Join(i.Home, ".agentq", "tick.lock"), 0)
	if e != nil || lock == nil {
		t.Fatal(e)
	}
	i.TickWait = 50 * time.Millisecond
	e = i.Setup(o)
	if e == nil {
		t.Fatal("setup replaced installation while tick held lock")
	}
	if !strings.Contains(e.Error(), "tick") || strings.Contains(e.Error(), "another installation") {
		t.Errorf("要说清是 tick 在跑：%v", e)
	}
	lock.Close()
	mustInstall(t, i, o)
}

// 升级撞上正在跑的 tick：等它跑完接着装，不报错。
func TestInstallWaitsForTick(t *testing.T) {
	i, o := installFixture(t)
	os.MkdirAll(filepath.Join(i.Home, ".agentq"), 0700)
	lock, e := fsx.AcquireLock(filepath.Join(i.Home, ".agentq", "tick.lock"), 0)
	if e != nil || lock == nil {
		t.Fatal(e)
	}
	time.AfterFunc(500*time.Millisecond, func() { lock.Close() })
	mustInstall(t, i, o)
}
func TestLegacyServiceCommandsProtectManagedInstall(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	cfg, _, _ := setup.ReadInstallConfig(i.Home)
	a := wireApp(&App{Env: &env.Env{Home: i.Home, Cfg: cfg, Out: io.Discard}, Service: &service.Service{Launchctl: func(...string) ([]byte, error) { t.Fatal("legacy command reached launchctl"); return nil, nil }}})
	if e := a.Service.CmdInstall(); e == nil {
		t.Fatal("legacy install took over managed guard")
	}
	if e := a.Service.CmdUninstall(); e == nil {
		t.Fatal("legacy uninstall removed managed entries")
	}
}
func TestWatchdogDetectsBrokenBinary(t *testing.T) {
	home := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(home, ".local", "bin", "agentq"), "#!/bin/sh\nexit 9\n", 0755)
	c := exec.Command("/bin/sh", "support/scripts/watchdog.sh")
	c.Env = setup.InstallEnv(home)
	if e := c.Run(); e == nil {
		t.Fatal("broken binary not reported")
	}
	b, e := os.ReadFile(filepath.Join(home, ".agentq", "watchdog-report.txt"))
	if e != nil || !strings.Contains(string(b), "ERROR") {
		t.Fatalf("watchdog %v %s", e, b)
	}
}
func TestBundledAssetClosureAndReviewBoundary(t *testing.T) {
	files, e := setup.BundledFiles(installAssets)
	if e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"agent-task", "dispatch", "nightly-review", "new-project", "project-status", "no-ai-look", "prior-art", "feedback-triage"} {
		b, ok := files["skills/"+n+"/SKILL.md"]
		if !ok || !strings.Contains(string(b.Data), "description:") {
			t.Fatal("missing standard description", n)
		}
	}
	for _, n := range []string{"AGENTS", "BACKLOG", "DECISIONS", "LESSONS", "PROGRESS"} {
		if _, ok := files["templates/"+n+".md"]; !ok {
			t.Fatal(n)
		}
	}
	var lock map[string]any
	b, _ := installAssets.ReadFile("dependencies/claude.lock.json")
	if json.Unmarshal(b, &lock) != nil {
		t.Fatal("bad lock")
	}
	text := string(files["skills/agent-task/SKILL.md"].Data)
	for _, want := range []string{"medium", "实现与自验", "不再调用 Agent/Workflow", "第二轮只复验", "check"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing review boundary", want)
		}
	}
	if strings.Contains(nightPrompt, "运行 `agentq resume`，再运行") {
		t.Fatal("night enables global scheduling")
	}
}
