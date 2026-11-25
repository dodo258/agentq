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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

const (
	LogMax    = 2 << 20         // 一个脚本的输出最多留这么多
	gitWait   = 5 * time.Minute // 建 worktree、合并的上限；tick 判断后台进程挂死时算进去
	cleanWait = 2 * time.Minute // 清理 worktree 的上限
	// leftoverWait：SIGKILL 之后等进程组真的没了，临时目录才好删。
	leftoverWait = 3 * time.Second
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
	// Script 是要跑的可执行文件，Args 是它的参数。
	Script string
	Args   []string
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
	// Tmps 登记这次运行的临时目录（见 ownTmp）。空的不另建，用调用方的 TMPDIR。
	Tmps proc.TmpStore
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
	cmd := exec.CommandContext(run, o.Script, o.Args...)
	cmd.Dir = o.Dir
	cmd.Env = append(ownTmp(o.Tmps, o.Mark, append(proc.ChildEnv(), proc.JobEnv+"="+o.Mark), logf), o.Env...)
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
	proc.WaitGroupGone(pgid, leftoverWait)
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
	// Tmps 同 ScriptOpts。
	Tmps proc.TmpStore
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
	cmd.Env = ownTmp(o.Tmps, o.Mark, append(proc.BackendEnv(inv.EnvKeep), proc.JobEnv+"="+o.Mark), errf)
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
	proc.WaitGroupGone(pgid, leftoverWait)
	if ctx.Err() != nil || run.Err() != nil {
		return fmt.Errorf("进程没跑完（上限 %s）：%v", textfmt.FormatDuration(o.Timeout), waitErr)
	}
	if werr != nil {
		return fmt.Errorf("记不下进程组号：%w", werr)
	}
	return nil
}

// ownTmp 给一次运行（一个脚本、一次 CLI）建自己的临时目录，返回换好 TMPDIR 的环境。这次过门（截图）结束时由 dropTmps 一起删。
// 建不了就用原来的 TMPDIR 照跑，原因写进 log。
func ownTmp(tmps proc.TmpStore, mark string, env []string, log io.Writer) []string {
	if tmps.Dir == "" {
		return env
	}
	tmps.Leave() // 同 runner：任务里执行的 tick 跑截图，不建在那个任务的目录里
	t, err := tmps.New("", mark)
	if err != nil {
		fmt.Fprintf(log, "建不了自己的临时目录，用原来的：%v\n", err)
		return env
	}
	return t.Env(env)
}

// dropTmps 删带这个标记的临时目录，调用前进程组都清完了。还有进程在用、查不了、删不掉的登记还在，由 tick 补清并记原因（runner.SweepTmp）。
// 攒到最后一起删：查一遍进程要零点几秒，测试门一次过门要跑好几个脚本。
func dropTmps(tmps proc.TmpStore, mark string) {
	list, _ := tmps.List()
	list = slices.DeleteFunc(list, func(t proc.Tmp) bool { return t.Job != "" || t.Marks[0] != mark })
	if len(list) == 0 {
		return
	}
	scan, err := proc.ScanTmp(context.Background()) // 门进程被停时它的 ctx 已经取消，收尾照样要查完
	if err != nil {
		return
	}
	for _, t := range list {
		if scan.Users(t) == "" {
			_ = tmps.Remove(t)
		}
	}
}

// RunHeld：这个 AGENTQ_JOB 标记的门还在跑，或者它留下的进程组里还有进程。不是门的标记返回假。
// 先看状态：同一次 tick 刚起的门进程正要拿锁，这时试拿一下会让它以为已经有一个在跑、退出。
func (a *Service) RunHeld(st *state.State, mark string) bool {
	id, ok := strings.CutPrefix(mark, gateMark(""))
	if !ok {
		return false
	}
	dir := a.Dir(id)
	if slices.ContainsFunc(st.Gating(), func(j *state.Job) bool { return j.ID == id }) || procHeld(dir) {
		return true
	}
	b, err := os.ReadFile(filepath.Join(dir, "pgid"))
	if err != nil {
		return false
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && proc.GroupLeft(pgid, mark) != proc.ProcGone
}

// RemoveWorktree：git 记录和目录都清掉，可以重复调用。--force 两次：被杀在 git worktree add 半路上的还带着 initializing 锁。
// 目录已经没了的也用 remove 清记录：prune 不清带锁的，留着它下次 add 就报「丢失但锁定的工作区」。没有记录时 remove 会报错，由 prune 收尾。
// 只读的（脚本改的权限、外面动过）先改回能写：这个目录是 agentq 自己建的一次性检出。
func RemoveWorktree(project, wt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanWait)
	defer cancel()
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", project, "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = proc.ChildEnv()
		cmd.WaitDelay = 5 * time.Second
		return cmd.Run()
	}
	remove := func() bool { return run("worktree", "remove", "--force", "--force", wt) == nil }
	if remove() {
		return nil
	}
	if _, err := os.Lstat(wt); !errors.Is(err, os.ErrNotExist) {
		makeTreeWritable(filepath.Dir(wt), wt)
		if remove() {
			return nil
		}
		if err := os.RemoveAll(wt); err != nil {
			return err
		}
		if remove() { // 检出坏了的（里面没有 .git）删掉目录后 git 才肯清记录
			return nil
		}
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
