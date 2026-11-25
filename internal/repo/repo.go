// Package repo 管 git：worktree、合并、跑测试。
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentq/internal/lock"
)

// Verify 是项目的检查脚本，退出码 0 算过。
const Verify = ".agentq/verify"

// ErrConflict：合并有冲突，要人或会话解决。冲突留在 worktree 里。
var ErrConflict = errors.New("合并有冲突")

// Git 在 dir 跑 git。
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("git %s：%w %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()+" "+s))
	}
	return s, nil
}

// OK：这条 git 命令成了。
func OK(dir string, args ...string) bool {
	_, err := Git(dir, args...)
	return err == nil
}

// Top 是仓库主目录：在 worktree 里执行也认主目录。
func Top(dir string) (string, error) {
	common, err := Git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s 不是 git 仓库：%w", dir, err)
	}
	return filepath.Dir(common), nil
}

// MainBranch 是默认分支：远程说的那个，没有就 main、master，再没有是现在这个。
func MainBranch(dir string) (string, error) {
	remote, _ := Git(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	head, _ := Git(dir, "symbolic-ref", "--short", "HEAD")
	for _, b := range []string{strings.TrimPrefix(remote, "origin/"), "main", "master", head} {
		if OK(dir, "rev-parse", "--verify", "-q", "refs/heads/"+b) {
			return b, nil
		}
	}
	return "", errors.New("找不到主分支：仓库要先提交一次，没有 main、master 的要检出一个分支")
}

// Worktree 从 from 开分支 branch 的 worktree；已经有就用现成的。
func Worktree(top, path, branch, from string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	held, err := alone(top)
	if err != nil {
		return err
	}
	defer held.Close()
	// -f：目录没了、登记还在的重新用起来。不 prune：那会清掉别人暂时看不到的 worktree
	if OK(top, "rev-parse", "--verify", "-q", "refs/heads/"+branch) {
		_, err := Git(top, "worktree", "add", "-f", "-q", path, branch)
		return err
	}
	_, err = Git(top, "worktree", "add", "-f", "-q", path, "-b", branch, from)
	return err
}

// Aside 在 ref 上另开一份看一看用的，写了什么都不算数。返回的函数把它去掉。
func Aside(top, path, ref string) (func(), error) {
	held, err := alone(top)
	if err != nil {
		return nil, err
	}
	defer held.Close()
	drop := func() { _, _ = Git(top, "worktree", "remove", "--force", path) }
	drop()
	if _, err := Git(top, "worktree", "add", "-f", "-q", "--detach", path, ref); err != nil {
		return nil, err
	}
	return drop, nil
}

// Keep 把打断时留下的改动提交掉，谁的都不丢；合到一半还有冲突的退回去重合。
func Keep(dir string) error {
	if InMerge(dir) {
		if left, err := Git(dir, "diff", "--name-only", "--diff-filter=U"); err != nil || left != "" {
			if _, err := Git(dir, "merge", "--abort"); err != nil {
				return err
			}
		}
	}
	if Dirty(dir) == "" && !InMerge(dir) {
		return nil
	}
	return Commit(dir, "接着做之前留下的改动")
}

// Commit 提交 dir 里的全部改动。
func Commit(dir, msg string) error {
	if _, err := Git(dir, "add", "-A"); err != nil {
		return err
	}
	_, err := Git(dir, "commit", "-q", "-m", msg)
	return err
}

// alone：同一个仓库一次只一个在开、删 worktree，git 自己同时做会互相踩到。
func alone(top string) (*os.File, error) {
	common, err := Git(top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	return lock.Flock(filepath.Join(common, "agentq.lock"), true)
}

// Merge 把 branch 合进 dir 检出的分支。有冲突返回 ErrConflict，现场留着。
func Merge(dir, branch, msg string) error {
	if _, err := Git(dir, "merge", "--no-edit", "-m", msg, branch); err != nil {
		if InMerge(dir) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// InMerge：合并还没做完。
func InMerge(dir string) bool { return OK(dir, "rev-parse", "--verify", "-q", "MERGE_HEAD") }

// Dirty 是没提交的改动，空的是干净。
func Dirty(dir string) string {
	s, err := Git(dir, "status", "--porcelain")
	if err != nil {
		return err.Error()
	}
	return s
}

// Has：dir 检出的分支包含 ref。
func Has(dir, ref string) bool { return OK(dir, "merge-base", "--is-ancestor", ref, "HEAD") }

// Remove 去掉干净的 worktree 和已经合进 into 的分支；有没提交的改动就留着。
func Remove(top, path, branch, into string) error {
	held, err := alone(top)
	if err != nil {
		return err
	}
	defer held.Close()
	if _, err := os.Stat(path); err == nil {
		if d := Dirty(path); d != "" {
			return fmt.Errorf("%s 里有没提交的改动，留着：%s", path, d)
		}
		if _, err := Git(top, "worktree", "remove", "--force", path); err != nil {
			return err
		}
	}
	if !OK(top, "merge-base", "--is-ancestor", "refs/heads/"+branch, into) {
		return fmt.Errorf("分支 %s 没合完，留着", branch)
	}
	_, err = Git(top, "branch", "-q", "-D", branch)
	return err
}

// Land 把 branch 快进到主分支。哪个目录检出着主分支就在那里合，别的改动（被忽略的文件也算）挡着会报错，什么都不会覆盖。
func Land(top, main, branch string) error {
	list, err := Git(top, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, w := range strings.Split(list, "\n\n") {
		if strings.HasSuffix(w, "\nbranch refs/heads/"+main) {
			dir, _, _ := strings.Cut(strings.TrimPrefix(w, "worktree "), "\n")
			_, err := Git(dir, "merge", "-q", "--ff-only", "--no-overwrite-ignore", branch)
			return err
		}
	}
	_, err = Git(top, "fetch", "-q", ".", branch+":"+main)
	return err
}

// Check 在 dir 跑 .agentq/verify。没有这个脚本算没过。日志写到 log。
func Check(ctx context.Context, dir, log string, env []string, timeout time.Duration) (ok bool, note string, err error) {
	script := filepath.Join(dir, Verify)
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		return false, "没有 " + Verify + "：补一个可执行的脚本，一条命令跑这个项目的全部检查（测试、静态检查、构建），退出码 0 算过", nil
	}
	tmp, err := os.MkdirTemp("", "agq")
	if err != nil {
		return false, "", err
	}
	defer wipe(tmp)
	f, err := os.Create(log)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, f, f
	cmd.Env = append(env, "TMPDIR="+tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if cmd.Process != nil { // 测试留下的子进程一起收掉
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return false, fmt.Sprintf("测试超过 %s 没跑完", timeout), nil
	case err == nil:
		return true, "", nil
	case errors.As(err, &exit):
		return false, fmt.Sprintf("测试没过，退出码 %d", exit.ExitCode()), nil
	}
	return false, "", fmt.Errorf("跑 %s：%w", Verify, err)
}

// wipe 删临时目录，没写权限的改回能写再删。
func wipe(dir string) {
	_ = exec.Command("chmod", "-R", "u+w", dir).Run()
	_ = os.RemoveAll(dir)
}
