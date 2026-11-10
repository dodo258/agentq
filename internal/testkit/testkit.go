// Package testkit 放测试共用的夹具，只给测试 import。
package testkit

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"agentq/internal/backend"
	"agentq/internal/backend/claude"
	"agentq/internal/gitx"
)

// RepoRoot 是仓库根目录：从工作目录往上找 go.mod。
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 往上找不到 go.mod", dir)
		}
		dir = parent
	}
}

// 根目录 assets.go 的 go:embed 写法。tests/ 不能 import main，照这两张表从磁盘读同样的文件；根目录的 TestEmbeddedAssetsMatchRepo 查两边一样。
var (
	InstallAssetPatterns = []string{"skills", "templates", "support", "rules", "dependencies/claude.lock.json"}
	EvalAssetPatterns    = []string{"all:evals"}
)

// EmbedView 按 go:embed 的规则从 root 下读出 patterns 指的文件：目录跳过 . 和 _ 开头的（写 all: 的不跳）、跳过版本库目录和带 go.mod 的子目录，只要普通文件。
func EmbedView(root string, patterns ...string) (fstest.MapFS, error) {
	view := fstest.MapFS{}
	for _, pattern := range patterns {
		rel, all := strings.CutPrefix(pattern, "all:")
		base := filepath.Join(root, filepath.FromSlash(rel))
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if p != base {
				if name == ".git" || name == ".hg" || name == ".bzr" || name == ".svn" {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if !all && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if d.IsDir() {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			key, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			view[filepath.ToSlash(key)] = &fstest.MapFile{Data: data, Mode: 0o444}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("读 %s：%w", pattern, err)
		}
	}
	return view, nil
}

func GoldenCompare(t *testing.T, name string, got []byte) {
	t.Helper()
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tests", "testdata", "golden", name)
	if os.Getenv("AGENTQ_GOLDEN_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("黄金文件 %s：%v（新加的先用 AGENTQ_GOLDEN_UPDATE=1 生成）", path, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		gl, wl := "<没有这一行>", "<没有这一行>"
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Fatalf("%s 第 %d 行不一样\n想要：%s\n实际：%s", path, i+1, wl, gl)
		}
	}
}

func GitT(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitx.GitEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// MkGitDir 造一个还没有提交的 main 仓库的 .git：git 认它是仓库，认得出默认分支，比 git init 快（根包几百个用例都要）。
func MkGitDir(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, ".git", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// GitTErr 跑 git，只要结果：断言「是不是祖先」「有没有这个分支」用。
func GitTErr(home, dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitx.GitEnv(home)
	return cmd.Run()
}

func MkProject(t *testing.T, root, name, todo string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	MkGitDir(t, dir)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	backlog := "# 待办\n\n## 待做\n\n<!-- - [ ] 模板 -->\n" + todo + "\n## 已完成\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func RealPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// RealRepo 建一个真 git 项目，返回目录和 main 上的提交号。
func RealRepo(t *testing.T, home, dir string) (string, string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	GitT(t, home, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	GitT(t, home, dir, "add", "-A")
	GitT(t, home, dir, "commit", "-q", "-m", "init")
	return dir, GitT(t, home, dir, "rev-parse", "HEAD")
}

func ReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func AssertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("没有 %q：\n%s", w, got)
		}
	}
}

func WriteInstall(t *testing.T, p, text string, mode os.FileMode) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(text), mode); e != nil {
		t.Fatal(e)
	}
}

// AssertBytes 查文件内容一字不差。
func AssertBytes(t *testing.T, p, want string) {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil || string(b) != want {
		t.Fatalf("%s: %v got %q want %q", p, e, b, want)
	}
}

// ReadBytes 读文件，读不出就失败。
func ReadBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ClaudeArgv 是这个启动意图在 Claude 后端上的参数。import testkit 也把 Claude 后端注册好了。
func ClaudeArgv(t *testing.T, req backend.Request) []string {
	t.Helper()
	inv, _, err := claude.New(backend.Options{}).Build(req)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Args
}
