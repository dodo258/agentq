package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agentq/internal/testkit"
)

// 真的 .agentq/release 在 UTF-8 语言环境里跑到「同步公开仓」那一行不报错，同步脚本失败就带着它的退出码停下，不往后走。
// macOS 的 sh（bash 3.2）会把「$RELEASE_SYNC）」里全角括号的首字节当成变量名的一部分。同步脚本、git、gh 都是假的。
func TestReleaseScriptStopsWhenSyncFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	called := filepath.Join(dir, "called")
	for _, name := range []string{"git", "gh"} {
		write("bin/"+name, "#!/bin/sh\necho \""+name+" $*\" >>'"+called+"'\nexit 9\n")
	}
	sync := write("sync.sh", "#!/bin/sh\nexit 7\n")
	conf := write("release.env", "RELEASE_SYNC="+sync+"\n")
	cmd := exec.Command("/bin/sh", filepath.Join(".agentq", "release"), "v9.9.9")
	cmd.Env = []string{"HOME=" + dir, "PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"LC_ALL=en_US.UTF-8", "AGENTQ_RELEASE_REPO=o/demo", "AGENTQ_RELEASE_CONF=" + conf}
	out, err := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); err == nil || code != 7 {
		t.Errorf("退出码要是同步脚本的 7：%d %v\n%s", code, err, out)
	}
	if !strings.Contains(string(out), "同步公开仓（"+sync+"）") || strings.Contains(string(out), "unbound variable") {
		t.Errorf("跑到同步那一行不报错：\n%s", out)
	}
	if b, err := os.ReadFile(called); err == nil {
		t.Errorf("同步失败就停，不该往后走：\n%s", b)
	}
}

// set -u 的脚本带 EXIT trap 时，中途失败（这里用没设的变量）退出码非 0。bash 3.2 里 set -u 报错时 trap 看到的 $? 已经是 0，
// 清理完直接退就成了 0，.agentq/release 因此把没发成当成发成过。用每个脚本自己的那行 trap 跑。
// support/ 的两个不查：trap 之后只展开自己赋过值的变量。
func TestShellScriptsKeepFailureExit(t *testing.T) {
	t.Parallel()
	setU := regexp.MustCompile(`(?m)^\s*set -[a-z]*u`)
	trapRe := regexp.MustCompile(`(?m)^\s*(trap '.*' EXIT\b.*)$`)
	varRe := regexp.MustCompile(`\$([a-z_]+)`)
	checked := 0
	for _, p := range shellScripts(t) {
		text := testkit.ReadFile(t, p)
		if strings.HasPrefix(p, "support/") || !setU.MatchString(text) {
			continue
		}
		for _, m := range trapRe.FindAllStringSubmatch(text, -1) {
			checked++
			if !strings.Contains(text, "unfinished=0") {
				t.Errorf("%s：跑到最后要 unfinished=0", p)
			}
			prog := "set -eu\nspin_stop() { :; }\nxcrun() { :; }\n"
			for _, v := range varRe.FindAllStringSubmatch(m[1], -1) {
				if v[1] != "code" && v[1] != "unfinished" {
					prog += v[1] + "=" + filepath.Join(t.TempDir(), "gone") + "\n"
				}
			}
			prog += "unfinished=1\n" + m[1] + "\n: \"$agentq_test_unset\"\nunfinished=0\n"
			cmd := exec.Command("/bin/sh", "-c", prog)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("%s：中途失败退出码是 0（trap 没保住）：%s\n%s", p, m[1], out)
			}
		}
	}
	if checked < 8 {
		t.Errorf("只查到 %d 处 EXIT trap", checked)
	}
}
