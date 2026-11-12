package setup

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/fsx"
	"agentq/internal/testkit"
)

func TestBackupRequiresExplicitVerifiedPrivateTarget(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	log := filepath.Join(home, "calls")
	// fake git/gh，不读取凭据、不向远端写。
	testkit.WriteInstall(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = remote ]; then echo https://example.invalid/private-repo; else echo push >> "+fsx.ShQuote(log)+"; fi\n", 0755)
	testkit.WriteInstall(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho '{\"isPrivate\":false}'\n", 0755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if e := backupProjects(home, io.Discard); e != nil {
		t.Fatal(e)
	}
	SaveInstallJSON(filepath.Join(home, ".agentq", "backup.json"), map[string]any{"targets": []backupTarget{{home, "backup", "https://example.invalid/private-repo"}}})
	if e := backupProjects(home, io.Discard); e == nil {
		t.Fatal("public target accepted")
	}
	if _, e := os.Stat(log); !os.IsNotExist(e) {
		t.Fatal("pushed to public target")
	}
	testkit.WriteInstall(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho '{\"isPrivate\":true}'\n", 0755)
	if e := backupProjects(home, io.Discard); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "push") != 2 {
		t.Fatal("private target not backed up")
	}
}

// 换安装前要等的：任务在跑、门在跑（任务记成 done 了，_gate 跑完还会 tick 接新活）。排队的门、已合并只差收尾的不挡；
// 状态文件读不出或不是状态文件（{}、空任务记录）当未知，和调度器同一套判断。
func TestInstallerIdleWaitsForRunningGate(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agentq", "state.json")
	if e := InstallerIdle(home); e != nil {
		t.Fatalf("没有状态文件算空闲：%v", e)
	}
	for body, busy := range map[string]bool{
		`{"jobs":[{"id":"a","status":"done","gate":{"state":"running","branch":"agent/a"}}]}`:               true,
		`{"jobs":[{"id":"a","status":"running"}]}`:                                                          true,
		`{"jobs":[{"id":"a","status":"done","gate":{"state":"queued","branch":"agent/a"}}]}`:                false,
		`{"jobs":[{"id":"a","status":"done","gate":{"state":"merged","branch":"agent/a","wrap_err":"x"}}]}`: false,
		`{"jobs":[]}`:     false,
		`{}`:              true,
		`null`:            true,
		`{"jobs":[null]}`: true,
		`{"jobs":`:        true,
	} {
		testkit.WriteInstall(t, path, body, 0o600)
		if e := InstallerIdle(home); (e != nil) != busy {
			t.Errorf("%s：要 busy=%v，得到 %v", body, busy, e)
		}
	}
}
