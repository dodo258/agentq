package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/runner"
	"agentq/internal/setup"
	"agentq/internal/testkit"
)

func TestAssignedTaskKeepsExecutorRole(t *testing.T) {
	for _, prompt := range []string{runner.BuildPrompt("/fixture", "不是当前 dispatch 会话直接实现"), runner.BuildResumePrompt("/fixture", "只派活不等待")} {
		if !strings.Contains(prompt, "执行者") || !strings.Contains(prompt, "亲自") || !strings.Contains(prompt, "dispatch 或 agentq run") {
			t.Fatal("assigned task can be mistaken for dispatch entry", prompt)
		}
	}
	for _, skill := range []string{"agent-task", "dispatch"} {
		b, e := installAssets.ReadFile("skills/" + skill + "/SKILL.md")
		if e != nil || !strings.Contains(string(b), "同一任务") || !strings.Contains(string(b), "执行合同") {
			t.Fatal("missing assigned-task role boundary", skill, e)
		}
	}
}

func TestRecoveryRejectsIncompleteReceipts(t *testing.T) {
	for _, command := range []string{"rollback", "restore"} {
		t.Run(command, func(t *testing.T) {
			i, o := installFixture(t)
			mustInstall(t, i, o)
			r := mustReceipt(t, i)
			file := "previous.json"
			if command == "rollback" {
				mustInstall(t, i, o)
				r = mustReceipt(t, i)
			} else {
				if e := i.Remove(true); e != nil {
					t.Fatal(e)
				}
				file = "removed.json"
			}
			before := map[string]setup.FileImage{}
			for p := range r.Owned {
				before[p], _ = setup.Image(p)
			}
			before[i.ReceiptPath()], _ = setup.Image(i.ReceiptPath())
			testkit.WriteInstall(t, filepath.Join(i.Share(), file), `{"version":1}`, 0600)
			var e error
			if command == "rollback" {
				e = i.Rollback(true)
			} else {
				e = i.Restore(true)
			}
			if e == nil {
				t.Fatal("incomplete recovery receipt activated")
			}
			for p, want := range before {
				got, e := setup.Image(p)
				if e != nil || !setup.SameImage(got, want) {
					t.Fatal("invalid recovery changed active entries", p, e)
				}
			}
		})
	}
}

func TestRecoveryRejectsPartialGenerationEntries(t *testing.T) {
	i, o := installFixture(t)
	mustInstall(t, i, o)
	old := mustReceipt(t, i)
	before, _ := os.ReadFile(i.ReceiptPath())
	for _, path := range []string{filepath.Join(i.Home, ".local", "bin", "agentq"), filepath.Join(i.Home, ".claude", "skills", "dispatch")} {
		bad := mustReceipt(t, i)
		delete(bad.Owned, path)
		if e := setup.SaveInstallJSON(filepath.Join(i.Share(), "previous.json"), bad); e != nil {
			t.Fatal(e)
		}
		if e := i.Rollback(true); e == nil {
			t.Fatal("partial generation receipt accepted", path)
		}
		testkit.AssertBytes(t, i.ReceiptPath(), string(before))
		if e := i.CheckOwned(old); e != nil {
			t.Fatal(e)
		}
	}
}

func TestProbeUsesTargetEnvironmentPATH(t *testing.T) {
	i, o := installFixture(t)
	bad := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(bad, "claude"), "#!/bin/sh\nexit 9\n", 0755)
	t.Setenv("PATH", bad+":"+os.Getenv("PATH"))
	env := []string{"HOME=" + i.Home, "PATH=" + filepath.Dir(o.Claude) + ":/usr/bin:/bin"}
	r, e := setup.ProbeEngineEnv(i.Home, "claude", env)
	if e != nil || r.Path != o.Claude {
		t.Fatalf("probe resolved shell PATH instead of target PATH: %+v %v", r, e)
	}
}

func TestServiceProbeRejectsShellOnlyIdentity(t *testing.T) {
	i, o := installFixture(t)
	cli := strings.Replace(probeClaude, `auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';;`, `auth) if [ "$CLAUDE_CODE_OAUTH_TOKEN" = fixture ]; then echo '{"loggedIn":true}'; else echo '{"loggedIn":false}'; fi;;`, 1)
	testkit.WriteInstall(t, o.Claude, cli, 0755)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture")
	i.Launchctl = func(...string) ([]byte, error) {
		t.Fatal("services reached launchctl before durable authentication check")
		return nil, nil
	}
	o.Services = true
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "service environment") {
		t.Fatalf("shell-only identity accepted for background services: %v", e)
	}
	if mustReceipt(t, i).Version != 0 {
		t.Fatal("service install activated without durable identity")
	}
}

func TestDoctorRequiresInstallationReceipt(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("PATH", filepath.Dir(o.Claude)+":"+os.Getenv("PATH"))
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "no managed installation") {
		t.Fatalf("absent installation reported healthy: %v", e)
	}
}

func TestDoctorChecksExternalRequiredSkills(t *testing.T) {
	for _, change := range []string{"missing", "modified"} {
		t.Run(change, func(t *testing.T) {
			i, o := installFixture(t)
			b, _ := installAssets.ReadFile("skills/dispatch/SKILL.md")
			p := filepath.Join(i.Home, ".claude", "skills", "dispatch", "SKILL.md")
			testkit.WriteInstall(t, p, string(b), 0644)
			mustInstall(t, i, o)
			if e := i.Doctor(); e != nil {
				t.Fatal(e)
			}
			if _, ok := mustReceipt(t, i).Owned[filepath.Dir(p)]; ok {
				t.Fatal("external skill adopted")
			}
			if change == "missing" {
				os.RemoveAll(filepath.Dir(p))
			} else {
				testkit.WriteInstall(t, p, "user edit", 0644)
			}
			if e := i.Doctor(); e == nil {
				t.Fatal("broken external required skill reported healthy")
			}
			// 依赖诊断不等于所有权，用户修改外部技能不妨碍只卸载受管入口。
			if e := i.Remove(true); e != nil {
				t.Fatal(e)
			}
			if change == "modified" {
				testkit.AssertBytes(t, p, "user edit")
			}
		})
	}
}

func TestSkillsRemovalPrintsWorkingRestoreEntry(t *testing.T) {
	i, o := installFixture(t)
	o.Scope = "skills"
	mustInstall(t, i, o)
	var out bytes.Buffer
	i.Out = &out
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "sh scripts/bootstrap.sh restore --yes") {
		t.Fatalf("skills removal has no source restore entry: %s", out.String())
	}
	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
}
