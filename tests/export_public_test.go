package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/testkit"
)

type exportFixture struct {
	repo, home, literal string
}

// exportRepo：临时 Git 仓库，带真的导出脚本、prepare-public、扫描器；HOME 下是临时禁用词表，词是现生成的。
func exportRepo(t *testing.T, public map[string]string) exportFixture {
	t.Helper()
	base := t.TempDir()
	f := exportFixture{repo: filepath.Join(base, "repo"), home: filepath.Join(base, "home"), literal: "sentinel-" + privacyID(t)}
	for _, name := range []string{"export-public.sh", "prepare-public.sh", "privacy_scan.py"} {
		b, err := os.ReadFile(filepath.Join("scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteInstall(t, filepath.Join(f.repo, "scripts", name), string(b), 0o755)
	}
	testkit.WriteInstall(t, filepath.Join(f.repo, "PROGRESS.md"), "私有 "+f.literal+"\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(f.repo, "notes", "x.md"), "私有 "+f.literal+"\n", 0o644)
	manifest := map[string]any{"version": 1, "private_paths": []string{"PROGRESS.md", "notes/"}, "approved_files": map[string]string{}}
	for name, body := range public {
		body = strings.ReplaceAll(body, "SENTINEL", f.literal)
		testkit.WriteInstall(t, filepath.Join(f.repo, name), body, 0o644)
		if strings.HasSuffix(name, ".png") {
			sum := sha256.Sum256([]byte(body))
			manifest["approved_files"].(map[string]string)[name] = hex.EncodeToString(sum[:])
		}
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteInstall(t, filepath.Join(f.repo, "config", "clean-source.json"), string(b)+"\n", 0o644)
	testkit.GitT(t, f.home, base, "init", "-q", f.repo)
	testkit.GitT(t, f.home, f.repo, "add", "-A")
	testkit.GitT(t, f.home, f.repo, "-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--no-verify", "-m", "x")
	f.writePatterns(t)
	return f
}

func (f exportFixture) writePatterns(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "patterns": []map[string]any{{"type": "private-word", "literal": f.literal}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.home, ".agentq", "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privacy-patterns.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f exportFixture) run(t *testing.T, out string) (string, int) {
	t.Helper()
	return f.runRepo(t, out, "acme/agentq")
}

func (f exportFixture) runRepo(t *testing.T, out, repo string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(f.repo, "scripts", "export-public.sh"), out, repo)
	cmd.Env = privacyEnv(f.home)
	b, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	return string(b), cmd.ProcessState.ExitCode()
}

func TestExportPublic(t *testing.T) {
	clean := map[string]string{"README.md": "下载 <owner>/agentq\n"}

	t.Run("干净的导出：去掉私有路径，填仓库名，不带 git，工作区改动不算", func(t *testing.T) {
		f := exportRepo(t, clean)
		testkit.WriteInstall(t, filepath.Join(f.repo, "README.md"), "没提交的 "+f.literal+"\n", 0o644)
		out := filepath.Join(t.TempDir(), "out")
		if s, code := f.run(t, out); code != 0 {
			t.Fatalf("退出 %d：%s", code, s)
		}
		if got := testkit.ReadFile(t, filepath.Join(out, "README.md")); got != "下载 acme/agentq\n" {
			t.Errorf("README：%q", got)
		}
		for _, gone := range []string{"PROGRESS.md", "notes", ".git"} {
			if exists(filepath.Join(out, gone)) {
				t.Errorf("%s 不该导出", gone)
			}
		}
	})

	t.Run("禁用词表缺失就报错，不跳过", func(t *testing.T) {
		f := exportRepo(t, clean)
		if err := os.Remove(filepath.Join(f.home, ".agentq", "private", "privacy-patterns.json")); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "out")
		if s, code := f.run(t, out); code == 0 || !strings.Contains(s, "禁用词表") {
			t.Fatalf("退出 %d：%s", code, s)
		}
		if exists(out) {
			t.Error("没开始导出，不该建输出目录")
		}
	})

	t.Run("公开文件命中就失败，报告不写命中的内容", func(t *testing.T) {
		f := exportRepo(t, map[string]string{"README.md": "这里有 SENTINEL\n"})
		s, code := f.run(t, filepath.Join(t.TempDir(), "out"))
		if code == 0 {
			t.Fatalf("应该失败：%s", s)
		}
		if strings.Contains(s, f.literal) {
			t.Errorf("报告不该写命中内容：%s", s)
		}
	})

	t.Run("扫描器按 hash 放行的图片也逐字节查", func(t *testing.T) {
		f := exportRepo(t, map[string]string{"README.md": "x\n", "art/a.png": "\x89PNG\r\n\x1a\nSENTINEL"})
		s, code := f.run(t, filepath.Join(t.TempDir(), "out"))
		if code == 0 || !strings.Contains(s, "art/a.png: private-word") || strings.Contains(s, f.literal) {
			t.Fatalf("退出 %d：%s", code, s)
		}
	})

	t.Run("账号名在词表里：完整的目标仓库名放过，别处出现照样失败", func(t *testing.T) {
		f := exportRepo(t, clean)
		out := filepath.Join(t.TempDir(), "out")
		if s, code := f.runRepo(t, out, f.literal+"/agentq"); code != 0 {
			t.Fatalf("退出 %d：%s", code, s)
		}
		if got := testkit.ReadFile(t, filepath.Join(out, "README.md")); got != "下载 "+f.literal+"/agentq\n" {
			t.Errorf("README：%q", got)
		}
		g := exportRepo(t, map[string]string{"README.md": "下载 <owner>/agentq，作者 SENTINEL\n"})
		s, code := g.runRepo(t, filepath.Join(t.TempDir(), "out"), g.literal+"/agentq")
		if code == 0 || strings.Contains(strings.ReplaceAll(s, g.literal+"/agentq", ""), g.literal) {
			t.Fatalf("退出 %d：%s", code, s)
		}
	})

	t.Run("文件名命中也失败", func(t *testing.T) {
		f := exportRepo(t, clean)
		testkit.WriteInstall(t, filepath.Join(f.repo, "docs", f.literal+".md"), "x\n", 0o644)
		testkit.GitT(t, f.home, f.repo, "add", "-A")
		testkit.GitT(t, f.home, f.repo, "-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--no-verify", "-m", "y")
		s, code := f.run(t, filepath.Join(t.TempDir(), "out"))
		if code == 0 {
			t.Fatalf("应该失败：%s", s)
		}
	})

	t.Run("输出目录不空、在仓库里、参数不对都拒绝", func(t *testing.T) {
		f := exportRepo(t, clean)
		full := t.TempDir()
		testkit.WriteInstall(t, filepath.Join(full, "x"), "x", 0o644)
		if s, code := f.run(t, full); code != 2 || !strings.Contains(s, "不是空的") {
			t.Errorf("退出 %d：%s", code, s)
		}
		if s, code := f.run(t, filepath.Join(f.repo, "out")); code != 2 || !strings.Contains(s, "仓库里") {
			t.Errorf("退出 %d：%s", code, s)
		}
		cmd := exec.Command("/bin/sh", filepath.Join(f.repo, "scripts", "export-public.sh"), "only-one")
		cmd.Env = privacyEnv(f.home)
		if err := cmd.Run(); cmd.ProcessState.ExitCode() != 2 {
			t.Errorf("参数不对应退出 2：%v", err)
		}
	})
}
