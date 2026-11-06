package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo 建一个有一个提交的仓库，分支名是 branch。
func initRepo(t *testing.T, home, branch string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, home, dir, "init", "-q", "-b", branch)
	gitT(t, home, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func gitT(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = GitEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setOriginHead 造出 clone 留下的样子：refs/remotes/origin/<branch> 和指向它的 origin/HEAD。
func setOriginHead(t *testing.T, home, dir, branch string) {
	t.Helper()
	head := gitT(t, home, dir, "rev-parse", "HEAD")
	gitT(t, home, dir, "update-ref", "refs/remotes/origin/"+branch, head)
	gitT(t, home, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

func TestDefaultBranch(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	want := func(t *testing.T, dir, name string) {
		t.Helper()
		got, err := DefaultBranch(ctx, home, dir)
		if err != nil || got != name {
			t.Fatalf("got %q, %v，想要 %q", got, err, name)
		}
	}
	none := func(t *testing.T, dir string) {
		t.Helper()
		got, err := DefaultBranch(ctx, home, dir)
		if !errors.Is(err, ErrNoDefaultBranch) || got != "" {
			t.Fatalf("got %q, %v，想要 ErrNoDefaultBranch", got, err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("错误里要有目录：%v", err)
		}
	}

	t.Run("只有 main", func(t *testing.T) { want(t, initRepo(t, home, "main"), "main") })
	t.Run("只有 master", func(t *testing.T) { want(t, initRepo(t, home, "master"), "master") })
	t.Run("HEAD 在 main，origin/HEAD 指别的：用 main", func(t *testing.T) {
		dir := initRepo(t, home, "main")
		gitT(t, home, dir, "branch", "develop")
		setOriginHead(t, home, dir, "develop")
		want(t, dir, "main")
	})
	t.Run("HEAD 在 master，main 也有：用 master", func(t *testing.T) {
		dir := initRepo(t, home, "master")
		gitT(t, home, dir, "branch", "main")
		want(t, dir, "master")
	})
	t.Run("HEAD 在 develop，origin/HEAD 也指它", func(t *testing.T) {
		dir := initRepo(t, home, "develop")
		setOriginHead(t, home, dir, "develop")
		want(t, dir, "develop")
	})
	t.Run("HEAD 在 feature：origin/HEAD 优先于本地 main", func(t *testing.T) {
		dir := initRepo(t, home, "main")
		gitT(t, home, dir, "branch", "develop")
		setOriginHead(t, home, dir, "develop")
		gitT(t, home, dir, "checkout", "-q", "-b", "feature")
		want(t, dir, "develop")
	})
	t.Run("HEAD 在 feature，没有 origin/HEAD，main 和 master 都有：先 main", func(t *testing.T) {
		dir := initRepo(t, home, "master")
		gitT(t, home, dir, "branch", "main")
		gitT(t, home, dir, "checkout", "-q", "-b", "feature")
		want(t, dir, "main")
	})
	t.Run("带斜杠的默认分支", func(t *testing.T) {
		dir := initRepo(t, home, "release/1.x")
		setOriginHead(t, home, dir, "release/1.x")
		want(t, dir, "release/1.x")
	})
	t.Run("origin/HEAD 指的分支本地没有，退回本地 main", func(t *testing.T) {
		dir := initRepo(t, home, "main")
		setOriginHead(t, home, dir, "develop")
		gitT(t, home, dir, "checkout", "-q", "-b", "feature")
		want(t, dir, "main")
	})
	t.Run("origin/HEAD 指的分支本地没有，也没有 main、master", func(t *testing.T) {
		dir := initRepo(t, home, "trunk")
		setOriginHead(t, home, dir, "develop")
		none(t, dir)
	})
	t.Run("名字带 shell 元字符不认", func(t *testing.T) {
		dir := initRepo(t, home, "main")
		gitT(t, home, dir, "branch", "x$(id)")
		setOriginHead(t, home, dir, "x$(id)")
		gitT(t, home, dir, "checkout", "-q", "-b", "feature")
		want(t, dir, "main")
	})
	t.Run("既没有 main 也没有 master", func(t *testing.T) { none(t, initRepo(t, home, "trunk")) })
	t.Run("当前在别的分支不影响判断", func(t *testing.T) {
		dir := initRepo(t, home, "master")
		gitT(t, home, dir, "checkout", "-q", "-b", "feature")
		want(t, dir, "master")
	})
	t.Run("游离 HEAD 不影响判断", func(t *testing.T) {
		dir := initRepo(t, home, "master")
		gitT(t, home, dir, "checkout", "-q", "--detach")
		want(t, dir, "master")
	})
	t.Run("同名的 tag 不影响判断", func(t *testing.T) {
		dir := initRepo(t, home, "master")
		gitT(t, home, dir, "tag", "master")
		want(t, dir, "master")
	})
	t.Run("游离 HEAD，没有 main、master", func(t *testing.T) {
		dir := initRepo(t, home, "trunk")
		gitT(t, home, dir, "checkout", "-q", "--detach")
		none(t, dir)
	})
	for _, branch := range []string{"main", "master"} {
		t.Run("没提交的 "+branch, func(t *testing.T) {
			dir := t.TempDir()
			gitT(t, home, dir, "init", "-q", "-b", branch)
			want(t, dir, branch)
		})
	}
	t.Run("没提交的别的分支", func(t *testing.T) {
		dir := t.TempDir()
		gitT(t, home, dir, "init", "-q", "-b", "trunk")
		none(t, dir)
	})
	t.Run("不是仓库：查不了，不是认不出", func(t *testing.T) {
		dir := t.TempDir()
		_, err := DefaultBranch(ctx, home, dir)
		if err == nil || errors.Is(err, ErrNoDefaultBranch) || !strings.Contains(err.Error(), dir) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("上层目录是仓库也不往上找", func(t *testing.T) {
		parent := initRepo(t, home, "main")
		sub := filepath.Join(parent, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := DefaultBranch(ctx, home, sub); err == nil {
			t.Fatal("应该查不了")
		}
	})
	t.Run("环境里的 GIT_DIR 不影响", func(t *testing.T) {
		other := initRepo(t, home, "trunk")
		dir := initRepo(t, home, "master")
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		want(t, dir, "master")
	})
	t.Run("调用方取消了算查不了", func(t *testing.T) {
		dir := initRepo(t, home, "main")
		c, cancel := context.WithCancel(ctx)
		cancel()
		_, err := DefaultBranch(c, home, dir)
		if err == nil || errors.Is(err, ErrNoDefaultBranch) {
			t.Fatalf("%v", err)
		}
	})
}

// git 卡住时不能一直等：超时报错，不当成认不出。
func TestDefaultBranchTimeout(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := DefaultBranch(ctx, t.TempDir(), t.TempDir())
	if err == nil || errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("等了 %s", d)
	}
}
