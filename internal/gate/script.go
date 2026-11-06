package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

const (
	LogMax    = 2 << 20         // 一个脚本的输出最多留这么多
	gitWait   = 5 * time.Minute // 建 worktree、合并的上限；tick 判断后台进程挂死时算进去
	cleanWait = 2 * time.Minute // 清理 worktree 的上限
)

// ScriptStatus 是脚本跑完的结论。
type ScriptStatus string

const (
	ScriptPassed  ScriptStatus = "passed"
	ScriptFailed  ScriptStatus = "failed"
	ScriptTimeout ScriptStatus = "timeout"
	ScriptUnknown ScriptStatus = "unknown" // 没跑起来、被信号杀了、门进程被停了：判断不了不罚
)

// ScriptCannotRun：脚本用这个退出码说「这次跑不了」（工具没有、依赖装不上）。判不了，不是提交有问题。和 git bisect 跳过用的是同一个数。
const ScriptCannotRun = 125

// ScriptOpts 是一次隔离运行：独立进程组、超时、输出落盘有上限。
type ScriptOpts struct {
	// Script 是要跑的可执行文件。
	Script string
	// Dir 是工作目录。
	Dir string
	// Log 是输出文件，覆盖写。
	Log string
	// Timeout 用单调时钟算，机器睡着不算。
	Timeout time.Duration
	// WorkDir 放 pgid 文件：门进程被 SIGKILL 后，tick 靠它和 Mark 认出留下的进程组。
	WorkDir string
	// Mark 是脚本和它拉起的进程环境里 AGENTQ_JOB 的值。
	Mark string
	// Env 是额外的环境变量。
	Env []string
}

// ScriptResult 是脚本的结果。
type ScriptResult struct {
	Status   ScriptStatus
	ExitCode int
	Note     string
}

// limitWriter 写到上限就丢弃后面的：脚本输出再多也撑不爆磁盘。
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return len(p), nil
	}
	q := p
	if int64(len(q)) > l.left {
		q = q[:l.left]
	}
	n, err := l.w.Write(q)
	l.left -= int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// RunScript 跑脚本。脚本和它拉起的进程在自己的进程组：超时、门进程被 SIGTERM 时先 SIGTERM，宽限后 SIGKILL。
func RunScript(ctx context.Context, o ScriptOpts) ScriptResult {
	fi, err := os.Lstat(o.Script)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return ScriptResult{Status: ScriptUnknown, Note: o.Script + " 不是可执行文件"}
	}
	logf, err := os.OpenFile(o.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return ScriptResult{Status: ScriptUnknown, Note: "输出文件打不开：" + err.Error()}
	}
	defer logf.Close()
	run, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	out := &limitWriter{w: logf, left: LogMax}
	cmd := exec.CommandContext(run, o.Script)
	cmd.Dir = o.Dir
	cmd.Env = append(append(proc.ChildEnv(), proc.JobEnv+"="+o.Mark), o.Env...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = proc.KillGrace
	if err := cmd.Start(); err != nil {
		return ScriptResult{Status: ScriptUnknown, Note: "脚本跑不起来：" + textfmt.OneLine(err.Error(), 200)}
	}
	pgid := cmd.Process.Pid
	if err := fsx.WriteFileAtomic(filepath.Join(o.WorkDir, "pgid"), []byte(strconv.Itoa(pgid)+"\n")); err != nil {
		fmt.Fprintf(logf, "记不下进程组号：%v\n", err)
	}
	waitErr := cmd.Wait()
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // 宽限过了还在的、脚本留下的后台进程：组里还有进程时组号不会被复用
	ps := cmd.ProcessState
	switch {
	case ps != nil && ps.Success():
		return ScriptResult{Status: ScriptPassed}
	case ctx.Err() != nil:
		return ScriptResult{Status: ScriptUnknown, ExitCode: -1, Note: "门进程被停了，脚本没跑完"}
	case errors.Is(run.Err(), context.DeadlineExceeded): // 定时器走单调时钟，机器睡着不算
		return ScriptResult{Status: ScriptTimeout, ExitCode: -1, Note: "超过 " + textfmt.FormatDuration(o.Timeout) + " 没跑完，已终止"}
	case ps != nil && ps.Exited() && ps.ExitCode() == ScriptCannotRun:
		return ScriptResult{Status: ScriptUnknown, ExitCode: ScriptCannotRun, Note: "脚本说这次跑不了（退出码 " + strconv.Itoa(ScriptCannotRun) + "，比如工具没有、依赖装不上），不算提交有问题"}
	case ps != nil && ps.Exited():
		return ScriptResult{Status: ScriptFailed, ExitCode: ps.ExitCode(), Note: "退出码 " + strconv.Itoa(ps.ExitCode())}
	}
	// 被别的信号杀了（外面有人杀、系统回收内存）：说明不了提交有问题
	return ScriptResult{Status: ScriptUnknown, ExitCode: -1, Note: "脚本被信号终止，结果未知：" + textfmt.OneLine(fmt.Sprint(waitErr), 200)}
}

// CLIOpts 是在门里起一次后端 CLI。
type CLIOpts struct {
	// OutDir 放 CLI 的标准输出 result.json 和 stderr.log，后端的 Parse 读这个目录。
	OutDir string
	// Timeout 用单调时钟算。
	Timeout time.Duration
	// WorkDir 放 pgid 文件，Mark 是 AGENTQ_JOB 的值，和 ScriptOpts 一样。
	WorkDir string
	Mark    string
}

// RunCLI 跑一次后端 CLI，等它结束：独立进程组，超时和门进程被 SIGTERM 时先 SIGTERM，宽限后 SIGKILL。环境只带这个后端自己的变量。
// 返回非 nil 表示起不来，或者没跑完就被停了；CLI 自己报错退出不算，结论让后端的 Parse 读。
func RunCLI(ctx context.Context, inv backend.Invocation, o CLIOpts) error {
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(o.OutDir, "result.json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(o.OutDir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer errf.Close()
	run, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(run, inv.Path, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = append(proc.BackendEnv(inv.EnvKeep), proc.JobEnv+"="+o.Mark)
	cmd.Stdout, cmd.Stderr = out, errf
	if inv.Stdin != "" {
		cmd.Stdin = strings.NewReader(inv.Stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = proc.KillGrace
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("进程起不来：%w", err)
	}
	pgid := cmd.Process.Pid
	werr := fsx.WriteFileAtomic(filepath.Join(o.WorkDir, "pgid"), []byte(strconv.Itoa(pgid)+"\n"))
	waitErr := cmd.Wait()
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // 宽限过了还在的、留下的后台进程
	if ctx.Err() != nil || run.Err() != nil {
		return fmt.Errorf("进程没跑完（上限 %s）：%v", textfmt.FormatDuration(o.Timeout), waitErr)
	}
	if werr != nil {
		return fmt.Errorf("记不下进程组号：%w", werr)
	}
	return nil
}

// RemoveWorktree：git 记录和目录都清掉，可以重复调用。--force 两次：被杀时 worktree 可能还带着初始化的锁。
// 目录已经没了也要 prune，否则 git 里还记着它。只读的（脚本改的权限、外面动过）先改回能写：这个目录是 agentq 自己建的一次性检出。
func RemoveWorktree(project, wt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanWait)
	defer cancel()
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", project, "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = proc.ChildEnv()
		cmd.WaitDelay = 5 * time.Second
		return cmd.Run()
	}
	if _, err := os.Lstat(wt); errors.Is(err, os.ErrNotExist) {
		return run("worktree", "prune")
	}
	if run("worktree", "remove", "--force", "--force", wt) == nil {
		return nil
	}
	makeTreeWritable(filepath.Dir(wt), wt)
	if run("worktree", "remove", "--force", "--force", wt) == nil {
		return nil
	}
	if err := os.RemoveAll(wt); err != nil {
		return err
	}
	return run("worktree", "prune")
}

// makeTreeWritable 把 parent 和 root 下所有目录改回自己能写。不跟软链接（WalkDir 不进链接），改不动的跳过，后面的删除会报错。
func makeTreeWritable(parent, root string) {
	_ = os.Chmod(parent, 0o700)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() { // WalkDir 先回调目录本身再读内容，这里改完权限就进得去
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}
