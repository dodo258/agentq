package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pathTestTime = time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)

func writeRC(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func readRC(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 加一行再卸载，各种原始内容都逐字节回到原样。
func TestPathLineRoundTrip(t *testing.T) {
	for name, original := range map[string]string{
		"普通":       "alias a=b\n",
		"结尾没换行":    "alias a=b",
		"空文件":      "",
		"中文和空行":    "\n# 我的配置\n\n\n",
		"有 CRLF":   "alias a=b\r\nalias c=d\r\n",
		"已有注释掉的路径": "# export PATH=\"$HOME/.local/bin:$PATH\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			rc := filepath.Join(home, ".zshrc")
			writeRC(t, rc, original, 0o640)
			change, err := AddPathLine(home, "/bin/zsh", "", pathTestTime)
			if err != nil || !change.Changed || change.RC != rc || change.Backup != rc+".pre-agentq" {
				t.Fatalf("%+v %v", change, err)
			}
			got := readRC(t, rc)
			if !strings.Contains(got, pathLine+"\n") || !strings.HasPrefix(got, original) {
				t.Fatalf("内容不对：%q", got)
			}
			if readRC(t, change.Backup) != original {
				t.Fatal("备份不对")
			}
			notes, err := RemovePathLine(home)
			if err != nil || len(notes) != 1 {
				t.Fatalf("%v %v", notes, err)
			}
			if got := readRC(t, rc); got != original {
				t.Fatalf("没回到原样：%q，想要 %q", got, original)
			}
			if st, err := os.Stat(rc); err != nil || st.Mode().Perm() != 0o640 {
				t.Fatalf("权限变了：%v %v", st, err)
			}
			if _, err := os.Stat(change.Backup); !os.IsNotExist(err) {
				t.Fatal("一样的备份不用留")
			}
			if _, err := os.Stat(pathManifest(home)); !os.IsNotExist(err) {
				t.Fatal("记录应删掉")
			}
		})
	}
}

// 文件是我们建的：卸载后不留。
func TestPathLineCreatedFile(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	change, err := AddPathLine(home, "zsh", "", pathTestTime)
	if err != nil || !change.Changed || change.Backup != "" {
		t.Fatalf("%+v %v", change, err)
	}
	if _, err := RemovePathLine(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Fatal("应删掉")
	}
}

// 用户之后改过：只删我们那一行，别的和备份都留着。
func TestPathLineKeepsLaterEdits(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	writeRC(t, rc, "first\n", 0o644)
	change, err := AddPathLine(home, "zsh", "", pathTestTime)
	if err != nil {
		t.Fatal(err)
	}
	writeRC(t, rc, "first\n"+pathLine+"\nlater\n", 0o644)
	notes, err := RemovePathLine(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := readRC(t, rc); got != "first\nlater\n" {
		t.Fatalf("%q", got)
	}
	if readRC(t, change.Backup) != "first\n" || !strings.Contains(notes[0], change.Backup) {
		t.Fatalf("备份要留并说明：%v", notes)
	}
}

// 那一行被用户改了：不动，如实说。
func TestPathLineEditedByUserIsLeft(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	writeRC(t, rc, "first\n", 0o644)
	if _, err := AddPathLine(home, "zsh", "", pathTestTime); err != nil {
		t.Fatal(err)
	}
	edited := "first\nx" + pathLine + "\n"
	writeRC(t, rc, edited, 0o644)
	notes, err := RemovePathLine(home)
	if err != nil {
		t.Fatal(err)
	}
	if readRC(t, rc) != edited || !strings.Contains(notes[0], "没找到") {
		t.Fatalf("%q %v", readRC(t, rc), notes)
	}
}

// 已有备份：加时间戳，原来的不覆盖。
func TestPathLineExistingBackup(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	writeRC(t, rc, "now\n", 0o644)
	writeRC(t, rc+".pre-agentq", "older\n", 0o644)
	change, err := AddPathLine(home, "zsh", "", pathTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if want := rc + ".pre-agentq-20261105T010203"; change.Backup != want {
		t.Fatalf("%q", change.Backup)
	}
	if readRC(t, rc+".pre-agentq") != "older\n" || readRC(t, change.Backup) != "now\n" {
		t.Fatal("备份不对")
	}
}

// 已经配好、已经加过：不再加。
func TestPathLineNotDuplicated(t *testing.T) {
	for _, existing := range []string{"export PATH=\"$HOME/.local/bin:$PATH\"\n", "path+=(~/.local/bin)\n"} {
		home := t.TempDir()
		rc := filepath.Join(home, ".zshrc")
		writeRC(t, rc, existing, 0o644)
		change, err := AddPathLine(home, "zsh", "", pathTestTime)
		if err != nil || change.Changed || change.Reason != "" {
			t.Fatalf("%+v %v", change, err)
		}
		if readRC(t, rc) != existing {
			t.Fatal("不该动")
		}
		if RCForPath(home, "zsh", "") != "" {
			t.Fatal("同意文字不该提")
		}
	}
	home := t.TempDir()
	if _, err := AddPathLine(home, "zsh", "", pathTestTime); err != nil {
		t.Fatal(err)
	}
	change, err := AddPathLine(home, "zsh", "", pathTestTime)
	if err != nil || change.Changed {
		t.Fatalf("重复加：%+v %v", change, err)
	}
	if n := strings.Count(readRC(t, filepath.Join(home, ".zshrc")), pathLine); n != 1 {
		t.Fatalf("加了 %d 行", n)
	}
}

// 只有真正给 PATH 赋值才算配好了；只是提到 .local/bin 的不算。
func TestAssignsLocalBin(t *testing.T) {
	for line, want := range map[string]bool{
		`export PATH="$HOME/.local/bin:$PATH"`:                     true,
		`  export PATH=~/.local/bin:$PATH`:                         true,
		`PATH="$HOME/.local/bin:$PATH"`:                            true,
		`path+=(~/.local/bin)`:                                     true,
		`path=(~/.local/bin $path)`:                                true,
		`typeset -x PATH=~/.local/bin:$PATH`:                       true,
		`[ -d ~/.local/bin ] && PATH=~/.local/bin:$PATH`:           true,
		`if [ -d ~/.local/bin ]; then PATH=~/.local/bin:$PATH; fi`: true,
		`case :$PATH: in *:$HOME/.local/bin:*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac`: true,
		pathLine:                                      true,
		`alias foo=~/.local/bin/foo`:                  false,
		`source ~/.local/bin/env`:                     false,
		`. "$HOME/.local/bin/env"`:                    false,
		`# export PATH="$HOME/.local/bin:$PATH"`:      false,
		`echo PATH=~/.local/bin`:                      false,
		`alias p='PATH=~/.local/bin:$PATH foo'`:       false,
		`export PATH="/opt/bin:$PATH" # ~/.local/bin`: false,
		`export MYPATH=~/.local/bin`:                  false,
	} {
		if got := assignsLocalBin(line + "\n"); got != want {
			t.Errorf("%q：%v，想要 %v", line, got, want)
		}
	}
}

// 配置文件是链接：改它指向的文件，链接还在，卸载也一样。
func TestPathLineFollowsSymlink(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "dotfiles", "zshrc")
	writeRC(t, real, "mine\n", 0o644)
	rc := filepath.Join(home, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Fatal(err)
	}
	if _, err := AddPathLine(home, "zsh", "", pathTestTime); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(rc); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("链接没了：%v %v", st, err)
	}
	if !strings.Contains(readRC(t, real), pathLine) {
		t.Fatal("没改到目标文件")
	}
	if _, err := RemovePathLine(home); err != nil {
		t.Fatal(err)
	}
	if readRC(t, real) != "mine\n" {
		t.Fatal("没回到原样")
	}
}

// 不碰的情况：别的 shell、不知道 shell、坏链接、目录、ZDOTDIR 在 HOME 外。
func TestPathLineRefuses(t *testing.T) {
	home := t.TempDir()
	broken := t.TempDir()
	if err := os.Symlink(filepath.Join(broken, "nope"), filepath.Join(broken, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	dirHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirHome, ".zshrc"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for name, c := range map[string]struct{ home, shell, zdot string }{
		"fish":    {home, "/usr/bin/fish", ""},
		"不知道":     {home, "", ""},
		"坏链接":     {broken, "zsh", ""},
		"目录":      {dirHome, "zsh", ""},
		"HOME 之外": {home, "zsh", outside},
	} {
		t.Run(name, func(t *testing.T) {
			change, err := AddPathLine(c.home, c.shell, c.zdot, pathTestTime)
			if err != nil || change.Changed || change.Reason == "" {
				t.Fatalf("%+v %v", change, err)
			}
		})
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("HOME 外不该写：%v", entries)
	}
}

// ZDOTDIR 在 HOME 下时用它；bash 按平台选文件。
func TestRCFile(t *testing.T) {
	home := t.TempDir()
	if got, err := rcFile(home, "/bin/zsh", filepath.Join(home, ".config", "zsh")); err != nil || got != filepath.Join(home, ".config", "zsh", ".zshrc") {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := rcFile(home, "/bin/bash", ""); err != nil || got != filepath.Join(home, bashRCNames[0]) {
		t.Fatalf("%q %v", got, err)
	}
}

// 没有记录：卸载什么都不做。
func TestRemovePathLineNoManifest(t *testing.T) {
	notes, err := RemovePathLine(t.TempDir())
	if err != nil || len(notes) != 0 {
		t.Fatalf("%v %v", notes, err)
	}
}
