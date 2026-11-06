package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/notify"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

const probeClaude = `#!/bin/sh
case "$1" in
 --version) echo '2.1.296 (Claude Code)';;
 --help) echo '--print --output-format --session-id --effort';;
 auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';;
 *) echo '{"is_error":false,"result":"ready"}';;
esac
`

func installFixture(t *testing.T) (*setup.Installer, setup.SetupOptions) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "ignored-config"))
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	cli := filepath.Join(home, "engine with spaces", "claude")
	testkit.WriteInstall(t, cli, probeClaude, 0755)
	i := &setup.Installer{Assets: installAssets, Home: home, Out: io.Discard, Check: func(string) error { return nil }, Fetch: func(bool) (map[string]setup.InstallFile, error) {
		m := map[string]setup.InstallFile{"licenses/Waza-LICENSE": {Data: []byte("test license"), Mode: 0644}}
		for _, n := range []string{"check", "think", "hunt", "ui"} {
			m["skills/"+n+"/SKILL.md"] = setup.InstallFile{Data: []byte("---\nname: " + n + "\ndescription: fixture\n---\n"), Mode: 0644}
		}
		return m, nil
	}}
	return i, setup.SetupOptions{Scope: "full", Claude: cli, Yes: true}
}
func mustReceipt(t *testing.T, i *setup.Installer) setup.InstallReceipt {
	t.Helper()
	r, e := i.Receipt()
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func mustInstall(t *testing.T, i *setup.Installer, o setup.SetupOptions) {
	t.Helper()
	if e := i.Setup(o); e != nil {
		t.Fatal(e)
	}
}
func TestSetupNoTelegramLifecycle(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	first := mustReceipt(t, i)
	cfg, exists, e := setup.ReadInstallConfig(i.Home)
	if e != nil || !exists || !cfg.Paused || cfg.NotifyEnabled || cfg.NightEnabled || cfg.ReviewEnabled || cfg.PatrolEnabled || cfg.PermissionMode != "default" {
		t.Fatalf("unsafe new config: %+v %v", cfg, e)
	}
	if _, e = os.Stat(filepath.Join(i.Home, ".agentq", "secrets", "telegram.env")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("Telegram requested")
	}
	if len(first.Services) != 0 {
		t.Fatal("default installed services")
	}
	for _, p := range []string{"templates/BACKLOG.md", "support/scripts/with-lock.sh", "skills/no-ai-look/references/research-2026-10-11.md", "skills/check/SKILL.md", "licenses/Waza-LICENSE"} {
		if _, e = os.Stat(filepath.Join(first.Generation, p)); e != nil {
			t.Fatal(e)
		}
	}
	// 用户定制和状态不属于 generation，不回滚/卸载。
	configPath := filepath.Join(i.Home, ".agentq", "config.json")
	b, _ := os.ReadFile(configPath)
	var raw map[string]any
	json.Unmarshal(b, &raw)
	raw["paused"] = false
	raw["notify_enabled"] = false
	raw["user_extension"] = "keep"
	setup.SaveInstallJSON(configPath, raw)
	custom, _ := os.ReadFile(configPath)
	settings := `{"permissions":{"defaultMode":"default"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"user-hook"}]}]},"statusLine":{"type":"command","command":"user-status"}}`
	testkit.WriteInstall(t, filepath.Join(i.Home, ".claude", "settings.json"), settings, 0600)
	testkit.WriteInstall(t, filepath.Join(i.Home, ".agentq", "jobs", "old", "artifact"), "keep data", 0600)
	mustInstall(t, i, o)
	second := mustReceipt(t, i)
	if second.Generation == first.Generation {
		t.Fatal("update did not stage a generation")
	}
	testkit.AssertBytes(t, configPath, string(custom))
	testkit.AssertBytes(t, filepath.Join(i.Home, ".claude", "settings.json"), settings)
	if e = i.Rollback(true); e != nil {
		t.Fatal(e)
	}
	if mustReceipt(t, i).Generation != first.Generation {
		t.Fatal("rollback did not restore entire generation")
	}
	testkit.AssertBytes(t, configPath, string(custom))
	if e = i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Lstat(filepath.Join(i.Home, ".local", "bin", "agentq")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("owned binary remains")
	}
	testkit.AssertBytes(t, filepath.Join(i.Home, ".agentq", "jobs", "old", "artifact"), "keep data")
	testkit.AssertBytes(t, configPath, string(custom))
	testkit.AssertBytes(t, filepath.Join(i.Home, ".claude", "settings.json"), settings)
	if e = i.Restore(true); e != nil {
		t.Fatal(e)
	}
	if mustReceipt(t, i).Generation != first.Generation {
		t.Fatal("restore mismatch")
	}
}
func TestSetupConflictsPreserveContent(t *testing.T) {
	for _, kind := range []string{"existing-skill", "vendor-edit", "link-edit", "invalid-settings", "invalid-config", "running", "null-job"} {
		t.Run(kind, func(t *testing.T) {
			i, o := installFixture(t)
			var target string
			switch kind {
			case "existing-skill":
				target = filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
				testkit.WriteInstall(t, target, "custom skill", 0644)
			case "invalid-settings":
				target = filepath.Join(i.Home, ".claude", "settings.json")
				testkit.WriteInstall(t, target, "{ invalid", 0600)
			case "invalid-config":
				target = filepath.Join(i.Home, ".agentq", "config.json")
				testkit.WriteInstall(t, target, "{ invalid", 0600)
			case "running":
				target = filepath.Join(i.Home, ".agentq", "state.json")
				testkit.WriteInstall(t, target, `{"jobs":[{"status":"running"}]}`, 0600)
			case "null-job":
				target = filepath.Join(i.Home, ".agentq", "state.json")
				testkit.WriteInstall(t, target, `{"jobs":[null]}`, 0600)
			default:
				mustInstall(t, i, o)
				r := mustReceipt(t, i)
				if kind == "vendor-edit" {
					target = filepath.Join(r.Generation, "skills", "dispatch", "SKILL.md")
					testkit.WriteInstall(t, target, "local vendor edit", 0644)
				} else {
					target = filepath.Join(i.Home, ".claude", "skills", "dispatch")
					os.Remove(target)
					os.Symlink(filepath.Join(i.Home, "user-skill"), target)
				}
			}
			before, e := setup.Image(target)
			if e != nil {
				t.Fatal(e)
			}
			receiptBefore, _ := os.ReadFile(i.ReceiptPath())
			if e = i.Setup(o); e == nil {
				t.Fatal("expected conflict")
			}
			after, e := setup.Image(target)
			if e != nil || !setup.SameImage(before, after) {
				t.Fatal("conflicting content changed")
			}
			receiptAfter, _ := os.ReadFile(i.ReceiptPath())
			if !bytes.Equal(receiptBefore, receiptAfter) {
				t.Fatal("active install changed")
			}
		})
	}
}
func TestSetupFailureRollbackAndInterruptedRecovery(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	old := mustReceipt(t, i)
	before, _ := os.ReadFile(i.ReceiptPath())
	i.AfterWrite = func(n int) error {
		if n == 3 {
			return errors.New("injected write failure")
		}
		return nil
	}
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "rolled back") {
		t.Fatalf("want rolled back failure: %v", e)
	}
	after, _ := os.ReadFile(i.ReceiptPath())
	if !bytes.Equal(before, after) {
		t.Fatal("failed update changed receipt")
	}
	if e := i.CheckOwned(old); e != nil {
		t.Fatal(e)
	}
	i.AfterWrite = nil
	i.Check = func(string) error { return errors.New("candidate does not start") }
	if e := i.Setup(o); e == nil {
		t.Fatal("bad candidate activated")
	}
	after, _ = os.ReadFile(i.ReceiptPath())
	if !bytes.Equal(before, after) {
		t.Fatal("bad candidate changed receipt")
	}
	// 模拟崩溃停在一次已完成 rename 后，下一次安装必须拒绝，restore 能恢复。
	p := filepath.Join(i.Home, ".local", "bin", "agentq")
	v, _ := setup.Image(p)
	replacement := setup.FileImage{Kind: "link", Link: "/missing-candidate"}
	j := setup.InstallJournal{Changes: []setup.InstallChange{{Path: p, Before: v, After: replacement}}, Applied: 1}
	setup.SaveInstallJSON(i.PendingPath(), j)
	setup.PutImage(p, replacement)
	if e := i.NoPending(); e == nil {
		t.Fatal("ignored interrupted installation")
	}
	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
	now, _ := setup.Image(p)
	if !setup.SameImage(now, v) {
		t.Fatal("interrupted restore failed")
	}
}
func TestSetupIdenticalExternalSkillNotOwned(t *testing.T) {
	i, o := installFixture(t)
	b, _ := installAssets.ReadFile("skills/dispatch/SKILL.md")
	p := filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
	testkit.WriteInstall(t, p, string(b), 0644)
	mustInstall(t, i, o)
	r := mustReceipt(t, i)
	if _, ok := r.Owned[filepath.Dir(p)]; ok {
		t.Fatal("adopted external skill")
	}
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	testkit.AssertBytes(t, p, string(b))
}

// 用户自己的同名技能（常见的是别的版本的 check）：不挡安装、不替换、卸载不删；随他更新，doctor 不报。
func TestSetupKeepsUsersDifferentSkill(t *testing.T) {
	i, o := installFixture(t)
	dir := filepath.Join(i.Home, ".claude", "skills", "no-ai-look")
	p := filepath.Join(dir, "SKILL.md")
	testkit.WriteInstall(t, p, "我自己的版本", 0644)
	var out bytes.Buffer
	i.Out = &out
	mustInstall(t, i, o)
	testkit.AssertBytes(t, p, "我自己的版本")
	if !strings.Contains(out.String(), "Keep your own skill") {
		t.Errorf("要说一声：%s", out.String())
	}
	r := mustReceipt(t, i)
	if _, ok := r.Owned[dir]; ok || !slices.Contains(r.KeptSkills, dir) {
		t.Fatalf("不接管：%+v", r.KeptSkills)
	}
	testkit.WriteInstall(t, p, "他更新了", 0644)
	if e := i.Doctor(); e != nil {
		t.Errorf("用户更新自己的技能不算坏：%v", e)
	}
	mustInstall(t, i, o) // 升级也不动
	testkit.AssertBytes(t, p, "他更新了")
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	testkit.AssertBytes(t, p, "他更新了")
}

// 用户把自己的技能放在别处、这里是个链接：一样保留。
func TestSetupKeepsUsersSkillLink(t *testing.T) {
	i, o := installFixture(t)
	target := filepath.Join(t.TempDir(), "mine")
	testkit.WriteInstall(t, filepath.Join(target, "SKILL.md"), "我的", 0644)
	link := filepath.Join(i.Home, ".claude", "skills", "prior-art")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	mustInstall(t, i, o)
	if got, e := os.Readlink(link); e != nil || got != target {
		t.Fatalf("链接要留着：%q %v", got, e)
	}
}

// 悬空链接保留不了：安装时就说清楚，不能装完才让 doctor 一直过不去。
func TestSetupBrokenSkillLink(t *testing.T) {
	i, o := installFixture(t)
	link := filepath.Join(i.Home, ".claude", "skills", "prior-art")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if e := os.Symlink(filepath.Join(t.TempDir(), "没了"), link); e != nil {
		t.Fatal(e)
	}
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "broken link") {
		t.Fatalf("%v", e)
	}
	os.Remove(link)
	mustInstall(t, i, o)
	if e := i.Doctor(); e != nil {
		t.Fatal(e)
	}
}

// 技能目录读不了：报真实原因，不能说成同名技能冲突（用户根本没有这个技能，没得改名）。
func TestSetupSkillsDirErrorNotBlamedOnSkill(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	i, o := installFixture(t)
	dir := filepath.Join(i.Home, ".claude", "skills")
	if e := os.MkdirAll(dir, 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(dir, 0); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	e := i.Setup(o)
	if e == nil || strings.Contains(e.Error(), "Rename it") || !strings.Contains(e.Error(), "permission denied") {
		t.Fatalf("%v", e)
	}
}

// 用户的技能删了：doctor 说清楚，再装一遍就用我们带的。
func TestSetupKeptSkillGone(t *testing.T) {
	i, o := installFixture(t)
	dir := filepath.Join(i.Home, ".claude", "skills", "no-ai-look")
	testkit.WriteInstall(t, filepath.Join(dir, "SKILL.md"), "我的", 0644)
	mustInstall(t, i, o)
	os.RemoveAll(dir)
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "run setup again") {
		t.Fatalf("%v", e)
	}
	mustInstall(t, i, o)
	if _, ok := mustReceipt(t, i).Owned[dir]; !ok {
		t.Error("再装一遍要用我们带的")
	}
}

// 任务合同靠的技能内容不一样：顶替不了，停下、原件不动，并说怎么办。
func TestSetupContractSkillConflict(t *testing.T) {
	i, o := installFixture(t)
	p := filepath.Join(i.Home, ".claude", "skills", "agent-task", "SKILL.md")
	testkit.WriteInstall(t, p, "同名但不是一回事", 0644)
	e := i.Setup(o)
	if e == nil || !strings.Contains(e.Error(), "Rename it") {
		t.Fatalf("%v", e)
	}
	testkit.AssertBytes(t, p, "同名但不是一回事")
}

func TestSetupOptionalServicesRollback(t *testing.T) {
	i, o := installFixture(t)
	loaded := map[string]bool{}
	fail := false
	i.Launchctl = func(args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			label := filepath.Base(args[1])
			if !loaded[label] {
				return nil, errors.New("not loaded")
			}
		case "bootout":
			delete(loaded, filepath.Base(args[1]))
		case "bootstrap":
			if fail {
				fail = false
				return nil, errors.New("injected bootstrap failure")
			}
			loaded[strings.TrimSuffix(filepath.Base(args[2]), ".plist")] = true
		}
		return nil, nil
	}
	o.Services = true
	o.Backup = true
	o.Watchdog = true
	mustInstall(t, i, o)
	r := mustReceipt(t, i)
	if len(loaded) != 5 || len(r.Services) != 5 {
		t.Fatalf("services: %v", loaded)
	}
	o.Services = false
	o.Backup = false
	o.Watchdog = false // 升级保留所选服务。
	mustInstall(t, i, o)
	if len(loaded) != 5 {
		t.Fatal("upgrade dropped service")
	}
	current := mustReceipt(t, i)
	fail = true
	if e := i.Setup(o); e == nil {
		t.Fatal("expected bootstrap failure")
	}
	if len(loaded) != 5 || mustReceipt(t, i).Generation != current.Generation {
		t.Fatal("failed bootstrap lost old services")
	}
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if len(loaded) != 0 {
		t.Fatal("service remains")
	}
	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
	if len(loaded) != 5 {
		t.Fatal("restore lost service")
	}
}
func TestProbeIndependentCLIAndAuthentication(t *testing.T) {
	i, o := installFixture(t)
	r, e := setup.ProbeEngine(i.Home, o.Claude)
	if e != nil || !r.Headless || !r.Authenticated || r.Path != o.Claude {
		t.Fatalf("probe %+v %v", r, e)
	}
	private := filepath.Join(i.Home, "Desktop.app", "Contents", "MacOS", "claude")
	testkit.WriteInstall(t, private, probeClaude, 0755)
	if _, e = setup.ProbeEngine(i.Home, private); e == nil {
		t.Fatal("accepted private app CLI")
	}
	testkit.WriteInstall(t, o.Claude, strings.Replace(probeClaude, `"loggedIn":true`, `"loggedIn":false`, 1), 0755)
	if _, e = setup.ProbeEngine(i.Home, o.Claude); e == nil || !strings.Contains(e.Error(), "authentication") {
		t.Fatal("logged out accepted")
	}
}
func TestSetupSkillsOnlyAndPlan(t *testing.T) {
	i, o := installFixture(t)
	o.Scope = "skills"
	o.Plan = true
	mustInstall(t, i, o)
	if mustReceipt(t, i).Version != 0 {
		t.Fatal("plan activated")
	}
	o.Plan = false
	mustInstall(t, i, o)
	if _, e := os.Stat(filepath.Join(i.Home, ".local", "bin", "agentq")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("skills scope installed background binary")
	}
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
}
func TestGenericBacklogDecisionSection(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "sample")
	os.MkdirAll(filepath.Join(p, ".git"), 0700)
	testkit.WriteInstall(t, filepath.Join(p, "docs", "BACKLOG.md"), "## 等用户决定\n- [ ] 明确授权 —— 推荐保留\n", 0644)
	ds, errs := notify.ScanDecisions(root)
	if len(errs) != 0 || len(ds) != 1 {
		t.Fatalf("%v %v", ds, errs)
	}
}
