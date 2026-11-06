// Package gitx 跑 git 命令：带超时、干净的环境。
package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentq/internal/env"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// Service 是跑 git 用的模块实例，只用来取 Home。
type Service struct {
	*env.Env
}

// IsGitMain：.git 必须是目录（worktree 和子模块的 .git 是文件）。
func IsGitMain(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

// defaultBranchWait：认一次默认分支最多等多久。tick 拿着锁，git 卡住不能一直等。
const defaultBranchWait = 30 * time.Second

// ErrNoDefaultBranch：查得了，但这个仓库认不出默认分支。不猜。
var ErrNoDefaultBranch = errors.New("认不出默认分支（主目录不在 main、master 上，origin/HEAD、本地 main、master 也都没有）")

// defaultBranchNameRe：能当默认分支的名字。这个名字会进给模型的提示词和它要跑的命令，远程能决定 origin/HEAD，带 shell 元字符的不认。
var defaultBranchNameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._/-]*$`)

// DefaultBranch 认项目的默认分支：主目录 HEAD 正在 main 或 master 上（包括还没有提交的）就用它，否则 origin/HEAD 指的（本地要有这个分支），再看本地 main、master。
// HEAD 排在 origin/HEAD 前面：BACKLOG 从工作目录读，认领也提交到这里，用它才前后一致，老用户升级后行为也不变。
// 都不是返回 ErrNoDefaultBranch，查不了（不是仓库、git 超时）返回带原因的错误。dir 是项目主目录，不往上层目录找仓库。
func DefaultBranch(ctx context.Context, home, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultBranchWait)
	defer cancel()
	// git 约定：--quiet 下引用不存在退出码是 1，别的退出码（不是仓库）才是查不了
	git := func(args ...string) (string, bool, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--git-dir=.git"}, args...)...)
		cmd.Env = GitEnv(home)
		proc.BoundCmd(cmd)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return strings.TrimSpace(string(out)), true, nil
		case ctx.Err() == nil && errors.As(err, &ee) && ee.ExitCode() == 1:
			return "", false, nil
		}
		return "", false, fmt.Errorf("查 %s 的默认分支：git %s：%w %s", dir, args[0], err, textfmt.OneLine(stderr.String(), 200))
	}
	hasLocal := func(name string) (bool, error) {
		_, ok, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+name+"^{commit}")
		return ok, err
	}
	head, ok, err := git("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	if name, onBranch := strings.CutPrefix(head, "refs/heads/"); ok && onBranch && (name == "main" || name == "master") {
		return name, nil
	}
	target, ok, err := git("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	if name, found := strings.CutPrefix(target, "refs/remotes/origin/"); ok && found && defaultBranchNameRe.MatchString(name) {
		local, err := hasLocal(name)
		if err != nil {
			return "", err
		}
		if local {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		local, err := hasLocal(name)
		if err != nil {
			return "", err
		}
		if local {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s %w", dir, ErrNoDefaultBranch)
}

// DefaultBranch 是 DefaultBranch 用这个实例的 Home。
func (a *Service) DefaultBranch(dir string) (string, error) {
	return DefaultBranch(context.Background(), a.Home, dir)
}

// OnBranch：主目录 HEAD 正在 branch 上（包括还没有提交的）。
func (a *Service) OnBranch(dir, branch string) bool {
	head, err := a.Git(dir, "symbolic-ref", "--quiet", "HEAD")
	return err == nil && head == "refs/heads/"+branch
}

// HasBranch：本地有 branch 这个分支。
func (a *Service) HasBranch(dir, branch string) bool {
	_, err := a.Git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// WorktreeOf 从 git worktree list --porcelain 的输出里找 ref 所在的 worktree；目录已经不在的不算。
func WorktreeOf(list, ref string) string {
	cur := ""
	for _, line := range strings.Split(list, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			cur = p
			continue
		}
		if line == "branch "+ref {
			if fi, err := os.Stat(cur); err == nil && fi.IsDir() {
				return cur
			}
			return ""
		}
	}
	return ""
}

// orphanSkipDirs：不算活动的目录：git 自己的和依赖、构建产物。
var orphanSkipDirs = map[string]bool{".git": true, "node_modules": true, ".build": true, "DerivedData": true}

// LatestMod 返回目录树里最新的修改时间，找到晚于 since 的就不再往下找。读不了的文件跳过。
func LatestMod(root string, since time.Time) time.Time {
	var last time.Time
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && orphanSkipDirs[d.Name()] {
			return fs.SkipDir
		}
		if d.Name() == ".git" {
			return nil // worktree 的 .git 是文件
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(last) {
			last = fi.ModTime()
		}
		if last.After(since) {
			return fs.SkipAll
		}
		return nil
	})
	return last
}

// Git 在 dir 执行 git，HOME 用 a.Home（读用户的 git 配置和钩子；测试里是临时目录）。
func (a *Service) Git(dir string, args ...string) (string, error) {
	// tick 拿着锁：提交时的钩子（如查仓库可见性要联网）卡住也不能一直等
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = GitEnv(a.Home)
	proc.BoundCmd(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func GitEnv(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || name == "XDG_CONFIG_HOME" || strings.HasPrefix(name, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home)
}

// GitInfo 是一个提交的时间和说明；OK=false 表示在仓库里找不到（别的仓库的号、改写历史前的旧号、写错的号）。
type GitInfo struct {
	Full    string
	Time    time.Time
	Subject string
	Body    string
	OK      bool
}

// qualityGitWait：查一个提交最多等多久（复盘前会跑，不能让 git 挂住把它卡死）。
const qualityGitWait = 30 * time.Second

type GitRepo struct {
	Dir     string
	Commits map[string]GitInfo
}

func (g *GitRepo) Commit(h string) GitInfo {
	if c, ok := g.Commits[h]; ok {
		return c
	}
	var c GitInfo
	ctx, cancel := context.WithTimeout(context.Background(), qualityGitWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", g.Dir, "show", "-s", "--format=%H%x00%ct%x00%s%x00%B", h+"^{commit}")
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if f := strings.SplitN(string(out), "\x00", 4); err == nil && len(f) == 4 {
		if sec, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			c = GitInfo{Full: f[0], Time: time.Unix(sec, 0), Subject: f[2], Body: f[3], OK: true}
		}
	}
	g.Commits[h] = c
	return c
}

// Own 返回合并提交 m 自己分支上的提交：从 m 沿第一父提交往回走，到第一个「待办：」开头的提交（认领、记账这类只在 main 上的提交）为止。
// 本地合并时 m 是分支末端，往回是分支上的提交，直到认领提交；GitHub 压缩合并时 m 的父提交就是 main 上的，只剩 m 自己。
func (g *GitRepo) Own(m string) map[string]bool {
	set := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), qualityGitWait)
	defer cancel()
	// 引擎合并的 m 是合并提交：第二父那边的才是这一条自己分支上的
	if out, err := g.gitOut(ctx, "rev-list", "-n", "200", m+"^1.."+m); err == nil {
		for _, h := range strings.Fields(out) {
			set[h] = true
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", g.Dir, "log", "--first-parent", "-n", "50", "--format=%H%x00%s", m)
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return set
	}
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		h, subj, _ := strings.Cut(line, "\x00")
		if i > 0 && strings.HasPrefix(subj, "待办：") {
			break
		}
		set[h] = true
	}
	return set
}

func (g *GitRepo) gitOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Dir}, args...)...)
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	return string(out), err
}

// RoutineGit 查项目自己的 .git，不往上找（.git 坏了时不会查到上一层的仓库）。
func (a *Service) RoutineGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--git-dir=.git"}, args...)...)
	cmd.Env = GitEnv(a.Home)
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

var GithubRemoteRe = regexp.MustCompile(`(?:^|[@/])github\.com[:/]([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9][A-Za-z0-9_.-]*?)(?:\.git)?/?$`)

var SafeCommitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
