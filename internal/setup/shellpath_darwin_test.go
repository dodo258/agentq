package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// macOS 的 bash 登录只读 .bash_profile、.bash_login、.profile 里第一个存在的：改第一个存在的，都没有才新建。
func TestBashRCPicksFirstExisting(t *testing.T) {
	for _, c := range []struct {
		existing []string
		want     string
	}{
		{nil, ".bash_profile"},
		{[]string{".profile"}, ".profile"},
		{[]string{".bash_login"}, ".bash_login"},
		{[]string{".bash_login", ".profile"}, ".bash_login"},
		{[]string{".bash_profile", ".bash_login", ".profile"}, ".bash_profile"},
	} {
		home := t.TempDir()
		for _, name := range c.existing {
			writeRC(t, filepath.Join(home, name), "# mine\n", 0o644)
		}
		if got, err := rcFile(home, "/bin/bash", ""); err != nil || got != filepath.Join(home, c.want) {
			t.Fatalf("%v：%q %v", c.existing, got, err)
		}
	}
}

// 真 bash：只有 .profile 的用户，加完那一行 .profile 还生效，PATH 也进去了；卸载后逐字节恢复。
func TestBashProfileStaysEffective(t *testing.T) {
	bash, err := exec.LookPath("/bin/bash")
	if err != nil {
		t.Skip("没有 /bin/bash")
	}
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	original := "RV_MARK=from_profile\nexport RV_MARK\n"
	writeRC(t, profile, original, 0o644)
	login := func() string {
		cmd := exec.Command(bash, "-lc", `printf '%s\n%s\n' "$RV_MARK" "$PATH"`)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := login(); !strings.HasPrefix(got, "from_profile\n") {
		t.Fatalf("改之前就不对：%q", got)
	}
	change, err := AddPathLine(home, "/bin/bash", "", pathTestTime)
	if err != nil || !change.Changed || change.RC != profile {
		t.Fatalf("%+v %v", change, err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bash_profile")); !os.IsNotExist(err) {
		t.Fatal("不该新建 .bash_profile")
	}
	got := login()
	if !strings.HasPrefix(got, "from_profile\n") {
		t.Fatalf(".profile 失效了：%q", got)
	}
	if !strings.Contains(got, filepath.Join(home, ".local", "bin")) {
		t.Fatalf("PATH 没进去：%q", got)
	}
	if _, err := RemovePathLine(home); err != nil {
		t.Fatal(err)
	}
	if readRC(t, profile) != original {
		t.Fatal("没回到原样")
	}
}
