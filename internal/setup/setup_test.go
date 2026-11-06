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
