package runner

import (
	"errors"
	"fmt"
	"io"
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
)

// shellEnvKeep 是任务外壳的环境变量：这个任务的后端要传给 CLI 哪些额外变量（逗号分隔）。外壳读完就从自己的环境里去掉。
// 外壳本身带所有后端的变量，因为它最后变成的 tick 可能起别的后端的任务。
const shellEnvKeep = "AGENTQ_SHELL_ENV_KEEP"

// shellStdin 是任务外壳的环境变量：喂给 CLI 标准输入的文件（Invocation.Stdin 落盘的），CLI 起来之后删掉。提示词不走外壳的参数，也就不进 ps。
const shellStdin = "AGENTQ_SHELL_STDIN"

// StdinFile：Invocation.Stdin 落盘的文件名，在任务目录里。
const StdinFile = "stdin.txt"

// ErrShellLost：外壳没报 pid 就没了，claude 可能已经起来了。外壳明确报了启动失败的不算这种。
var ErrShellLost = errors.New("任务外壳没有报回 CLI 的 pid")

// StartCLI 在 dir 里启动 CLI，新进程组，环境里带 AGENTQ_JOB=<jobID>，stdout/stderr 写进给定文件，不等它结束。
// stdinPath 非空时 CLI 的标准输入读这个文件，打不开算启动失败：丢了提示词的 CLI 会空跑或等输入。
func StartCLI(cli, dir, jobID string, args, keep []string, stdinPath, stdoutPath, stderrPath string) (*exec.Cmd, error) {
	var stdin *os.File
	if stdinPath != "" {
		f, err := os.Open(stdinPath)
		if err != nil {
			return nil, fmt.Errorf("打不开提示词文件：%w", err)
		}
		defer f.Close()
		stdin = f
	}
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.Command(cli, args...)
	cmd.Dir = dir
	cmd.Env = append(proc.BackendEnv(keep), proc.JobEnv+"="+jobID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if stdin != nil { // 不能把空的 *os.File 赋给接口：exec 会当成有输入
		cmd.Stdin = stdin
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// 外壳启动后多久内必须报回 claude 的 pid。
const ShellReportWait = 30 * time.Second

// ClaudePidFile：外壳起了 claude 就写在任务目录里。启动方（tick、run、listen）报 pid 前被杀时，下次 tick 靠它认领。
const ClaudePidFile = "claude.pid"

// StartJob 经任务外壳（agentq _job）启动 inv，返回 CLI 的 pid。外壳在自己的进程组里等 CLI 结束，
// 再换成 `agentq tick --after <任务id>`，让排队的待做马上接上；外壳和那次 tick 的输出追加到 logPath。
// claude 仍在它自己的进程组里，收尾、超时、stop 都照旧只看 claude 的 pid 和进程组。
func StartJob(self, jobDir string, inv backend.Invocation, logPath string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	childEnv := append(proc.ChildEnv(), shellEnvKeep+"="+strings.Join(inv.EnvKeep, ","))
	if inv.Stdin != "" {
		path := filepath.Join(jobDir, StdinFile)
		if err := os.WriteFile(path, []byte(inv.Stdin), 0o600); err != nil {
			return 0, fmt.Errorf("写提示词文件失败：%w", err)
		}
		childEnv = append(childEnv, shellStdin+"="+path)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	cmd := exec.Command(self, append([]string{"_job", jobDir, inv.Dir, inv.Path}, inv.Args...)...)
	cmd.Dir = inv.Dir
	cmd.Env = childEnv
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.ExtraFiles = []*os.File{w} // 外壳里是 fd 3，用来报 pid
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		return 0, err
	}
	_ = r.SetReadDeadline(time.Now().Add(ShellReportWait))
	b, rerr := io.ReadAll(r)
	msg := strings.TrimSpace(string(b))
	if s, ok := strings.CutPrefix(msg, "pid "); ok {
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			// agentq 进程本身很快退出，外壳交给 launchd 回收；这里只是让 agentq 还活着时不留僵尸。
			go cmd.Wait()
			return pid, nil
		}
	}
	// 没拿到 pid 就别留下一个没人管的外壳。还没 Wait，外壳退出了也只是僵尸，组号不会被复用。
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	if s, ok := strings.CutPrefix(msg, "err "); ok {
		return 0, errors.New(s)
	}
	if rerr != nil {
		return 0, fmt.Errorf("%w：%v", ErrShellLost, rerr)
	}
	return 0, fmt.Errorf("%w（收到 %q）", ErrShellLost, msg)
}

// RunJobShell 是任务外壳本身：agentq _job <任务目录> <项目目录> <CLI> [参数…]。它不认识任何 CLI，只执行 Invocation。
// 在项目目录启动 claude（新进程组，输出写进任务目录），经 fd 3 报回 pid；等 claude 结束后
// exec 成 `agentq tick --after <任务id>`。那次 tick 和定时 tick 共用 tick.lock，不会重叠。
func RunJobShell(args []string, stderr io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(stderr, "agentq _job：内部命令，由 tick/run 启动任务时调用")
		return 2
	}
	jobDir, projectDir, cli := args[0], args[1], args[2]
	var keep []string
	if v := os.Getenv(shellEnvKeep); v != "" {
		keep = strings.Split(v, ",")
	}
	os.Unsetenv(shellEnvKeep)
	stdinPath := os.Getenv(shellStdin)
	os.Unsetenv(shellStdin)
	syscall.CloseOnExec(3) // 别让 claude 继承报 pid 的管道，否则启动方要等 claude 结束才读到 EOF
	report := os.NewFile(3, "report")
	id := filepath.Base(jobDir)
	cmd, err := StartCLI(cli, projectDir, id, args[3:], keep, stdinPath, filepath.Join(jobDir, "result.json"), filepath.Join(jobDir, "stderr.log"))
	if err != nil {
		fmt.Fprintf(report, "err %v\n", err)
		report.Close()
		return 1
	}
	if stdinPath != "" {
		os.Remove(stdinPath) // CLI 已经开着它了；同样的内容在 prompt.txt
	}
	if err := fsx.WriteFileAtomic(filepath.Join(jobDir, ClaudePidFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n")); err != nil {
		fmt.Fprintf(stderr, "%s 任务 %s 写 %s 失败：%v\n", time.Now().Format("2006-01-02 15:04:05"), id, ClaudePidFile, err)
	}
	fmt.Fprintf(report, "pid %d\n", cmd.Process.Pid)
	report.Close()
	_ = cmd.Wait()
	// 外壳刚等到 claude 退出：组里还有进程时组号不会被复用，组里没进程了 kill 也只是 ESRCH，
	// 所以在这里清残留进程组不会误杀，不用像 tick 那样核对标记（tick 收尾时离退出可能已经很久）。
	pgid := cmd.Process.Pid
	if proc.GroupAlive(pgid) {
		how := "SIGTERM"
		if proc.KillGroup(pgid, proc.LeftoverGrace) { // 不理 SIGTERM 的不能留着：没有别的进程还会管它们
			how = "SIGTERM 后 SIGKILL"
		}
		fmt.Fprintf(stderr, "%s 任务 %s 的 %s 已退出，进程组 %d 还有残留进程，已发 %s\n",
			time.Now().Format("2006-01-02 15:04:05"), id, filepath.Base(cmd.Path), pgid, how)
	}
	self, err := os.Executable()
	if err == nil {
		// exec 而不是在本进程里跑：期间重新 go build 过的话，用的是新版本
		err = syscall.Exec(self, []string{self, "tick", "--after", id}, os.Environ())
	}
	fmt.Fprintf(stderr, "%s 任务 %s 结束后执行 agentq tick 失败（等下次定时 tick）：%v\n",
		time.Now().Format("2006-01-02 15:04:05"), id, err)
	return 1
}
