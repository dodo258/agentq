package gate_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/gitx"
	"agentq/internal/testkit"
)

// 仓库里登记的每项能力：知识文件、检查脚本、考题都在，考题的领域和能力一样。登记了却缺一样，能力门和评测门都会判错。
func TestCapabilityRegistryFiles(t *testing.T) {
	caps, err := gate.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	for _, cp := range caps {
		for _, k := range cp.Knowledge {
			if !strings.HasPrefix(k, "skills/") && !strings.HasPrefix(k, "rules/") {
				t.Errorf("%s 的知识 %s 要在 skills/ 或 rules/ 下", cp.Name, k)
			}
			if _, err := os.Stat(filepath.Join(root, k)); err != nil {
				t.Errorf("%s 的知识文件：%v", cp.Name, err)
			}
		}
		if cp.Check != "" {
			fi, err := os.Stat(filepath.Join("capabilities", cp.Check))
			if err != nil || fi.Mode()&0o111 == 0 {
				t.Errorf("%s 的检查 %s 要在 capabilities/ 下且可执行：%v", cp.Name, cp.Check, err)
			}
		}
		for _, c := range cp.Cases {
			raw, err := os.ReadFile(filepath.Join(root, "evals", c, "case.json"))
			var cs struct {
				Domain string `json:"domain"`
			}
			if err == nil {
				err = json.Unmarshal(raw, &cs)
			}
			if err != nil || cs.Domain != cp.Domain {
				t.Errorf("%s 的考题 %s：领域 %q，要 %q（%v）", cp.Name, c, cs.Domain, cp.Domain, err)
			}
		}
	}
}

// backend-delete-guard 的检查：新加删数据的代码、同目录没改测试才报；清临时文件、写给用户看的命令、注释不报。
func TestDeleteGuardScript(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("capabilities", "delete-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		files map[string]string
		exit  int
		out   string
	}{
		{"没有删除", map[string]string{"a/a.go": "package a\n"}, 0, "没有新加删数据的代码"},
		{"删目录没测试", map[string]string{"prune/p.go": "package prune\n\nfunc P(d string) { os.RemoveAll(d) }\n"}, 1, "prune/p.go:3：func P(d string) { os.RemoveAll(d) }"},
		{"同目录改了测试", map[string]string{"prune/p.go": "package prune\nfunc P(d string) { os.RemoveAll(d) }\n", "prune/p_test.go": "package prune\n"}, 0, "同目录都改了测试"},
		{"别的目录的测试不算", map[string]string{"prune/p.go": "package prune\nfunc P(d string) { os.RemoveAll(d) }\n", "other/o_test.go": "package other\n"}, 1, "prune/p.go:2"},
		{"删分支参数数组", map[string]string{"g/g.go": "package g\nvar a = []string{\"branch\", \"-D\", name}\n"}, 1, "g/g.go:2"},
		{"删工作树", map[string]string{"g/g.go": "package g\nvar a = []string{\"worktree\", \"remove\", p}\n"}, 1, "g/g.go:2"},
		{"脚本里 rm -rf", map[string]string{"scripts/clean.sh": "#!/bin/sh\nrm -rf \"$out\"\n"}, 1, "scripts/clean.sh:2"},
		{"Rust 上层 tests 目录", map[string]string{"src/fs/prune.rs": "fn p() { remove_dir_all(d); }\n", "tests/prune.rs": "#[test]\nfn t() {}\n"}, 0, "同目录都改了测试"},
		{"Rust 同文件测试", map[string]string{"src/prune.rs": "fn p() { remove_dir_all(d); }\n#[cfg(test)]\nmod t {}\n"}, 0, "同目录都改了测试"},
		{"defer 清临时文件", map[string]string{"a/a.go": "package a\nfunc f() {\n\tdefer os.RemoveAll(dir)\n}\n"}, 0, "没有新加删数据的代码"},
		{"临时目录", map[string]string{"a/a.go": "package a\nfunc f() { os.RemoveAll(tmpDir) }\n"}, 0, "没有新加删数据的代码"},
		{"trap 清理", map[string]string{"s.sh": "trap 'rm -rf \"$stage\"' EXIT\n"}, 0, "没有新加删数据的代码"},
		{"注释", map[string]string{"a/a.go": "package a\n// 以前用 os.RemoveAll(d)\n"}, 0, "没有新加删数据的代码"},
		{"写给用户看的命令", map[string]string{"ui/Strings.swift": "let hint = \"恢复：git branch -D x 之后 rm -rf y\"\n"}, 0, "没有新加删数据的代码"},
		{"删单个文件", map[string]string{"a/a.go": "package a\nfunc f() { os.Remove(out) }\n"}, 0, "没有新加删数据的代码"},
		{"测试文件里删", map[string]string{"a/a_test.go": "package a\nfunc f() { os.RemoveAll(d) }\n"}, 0, "没有新加删数据的代码"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
			writeFiles(t, dir, map[string]string{"README.md": "x\n"})
			testkit.GitT(t, home, dir, "add", "-A")
			testkit.GitT(t, home, dir, "commit", "-q", "-m", "基点")
			base := strings.TrimSpace(testkit.GitT(t, home, dir, "rev-parse", "HEAD"))
			writeFiles(t, dir, c.files)
			testkit.GitT(t, home, dir, "add", "-A")
			testkit.GitT(t, home, dir, "commit", "-q", "-m", "改动")
			code, out := runGuard(t, script, home, dir, base)
			if code != c.exit || !strings.Contains(out, c.out) {
				t.Fatalf("退出码 %d（要 %d），输出：\n%s\n要含：%s", code, c.exit, out, c.out)
			}
		})
	}

	t.Run("基点不是提交判不了", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
		if code, out := runGuard(t, script, home, dir, "0000000000000000000000000000000000000000"); code != 125 {
			t.Fatalf("退出码 %d，要 125：%s", code, out)
		}
		if code, _ := runGuard(t, script, home, dir, ""); code != 125 {
			t.Fatalf("没有 AGENTQ_BASE 退出码 %d，要 125", code)
		}
	})
}

func runGuard(t *testing.T, script, home, dir, base string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", script)
	cmd.Dir = dir
	cmd.Env = append(gitx.GitEnv(home), "AGENTQ_BASE="+base)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("跑检查：%v", err)
	return -1, ""
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
