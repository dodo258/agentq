package runner_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// 任务分支推到 backup 后又被 amend：本地不是推上去那个提交的后代，查得出来；只是往后加提交、没推过、没有 backup 都不算。
func TestBackupForkAfterAmend(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
	os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
	e, err := env.New(home, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	e.Cfg.ProjectsRoot = t.TempDir()
	k := stack.New(e)
	dir := filepath.Join(e.Cfg.ProjectsRoot, "demo")
	branch := "agent/1121-x"
	item := "加导出 —— 要什么"
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n- [ ] "+item+"（进行中："+branch+" 2026-11-21 05:00）\n\n## 已完成\n"), 0o644)
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	job := &state.Job{ID: "1121-052300-demo", Project: "demo", ProjectDir: dir, Item: item, Title: "加导出", Fingerprint: backlog.Fingerprint("- [ ] " + item)}
	commit := func(name, msg string) {
		os.WriteFile(filepath.Join(dir, name), []byte(msg), 0o644)
		testkit.GitT(t, home, dir, "add", "-A")
		testkit.GitT(t, home, dir, "commit", "-q", "-m", msg)
	}
	testkit.GitT(t, home, dir, "switch", "-q", "-c", branch)
	commit("a.txt", "第一版")
	if got := k.Runner.BackupFork(dir, branch); got != "" {
		t.Fatalf("没有 backup 远程也报了：%s", got)
	}

	bare := filepath.Join(t.TempDir(), "backup.git")
	testkit.GitT(t, home, dir, "init", "-q", "--bare", bare)
	testkit.GitT(t, home, dir, "remote", "add", "backup", bare)
	if got := k.Runner.BackupFork(dir, branch); got != "" {
		t.Fatalf("还没推过也报了：%s", got)
	}
	testkit.GitT(t, home, dir, "push", "-q", "backup", "--all")
	if got := k.Runner.BackupFork(dir, branch); got != "" {
		t.Fatalf("刚推完、和备份一样也报了：%s", got)
	}
	commit("b.txt", "再加一个提交")
	if got := k.Runner.BackupFork(dir, branch); got != "" {
		t.Fatalf("只往后加提交（备份的后代）也报了：%s", got)
	}
	testkit.GitT(t, home, dir, "push", "-q", "backup", "--all")

	testkit.GitT(t, home, dir, "commit", "-q", "--amend", "-m", "改了说明")
	got := k.Runner.BackupFork(dir, branch)
	testkit.AssertContains(t, got, branch, "不是它的后代", "push --force-with-lease backup "+branch)
	if err := testkit.GitTErr(home, dir, "push", "-q", "backup", "--all"); err == nil {
		t.Fatal("夹具不对：amend 后 push --all 应该被拒")
	}
	if fork := k.Runner.TaskBranchFork(job); !strings.Contains(fork, branch) {
		t.Fatalf("按 BACKLOG 进行中标记找分支没查出分叉：%q", fork)
	}
	chat := *job
	chat.Kind = state.KindChat
	if fork := k.Runner.TaskBranchFork(&chat); fork != "" {
		t.Fatalf("对话任务没有任务分支，不该查：%s", fork)
	}
	if got := k.Runner.BackupFork(dir, "agent/没有这个分支"); got != "" {
		t.Fatalf("本地没有的分支也报了：%s", got)
	}
}
