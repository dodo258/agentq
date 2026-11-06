// Package proc 管子进程的生死：环境白名单、进程组、开机时间。
package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// 超时或 stop 时，SIGTERM 之后等这么久仍在就 SIGKILL。
const KillGrace = 10 * time.Second

// LeftoverGrace：CLI 退出后清它留下的进程组，SIGTERM 之后等这么久仍在就 SIGKILL。
const LeftoverGrace = 5 * time.Second

// commonEnvKeep 是所有子进程都带的环境变量，其余一律不传：防止 API Key 之类让任务改走 API 计费或别的账号，
// 也防止调用方所在 CLI 会话注入的变量让子进程把自己当成那个会话的子会话。代理变量是在终端里 agentq run 时联网要用的。
var commonEnvKeep = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG",
	"TMPDIR", "SHELL", "SSH_AUTH_SOCK",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
}

// ChildEnvKeep 是环境变量白名单：公共部分，加上各后端注册时声明的登录和配置变量（backend.Register）。
// agentq 自己的子进程（任务外壳、git、核验脚本）用它；真正启动某个 CLI 时只带公共部分和那个后端自己的，见 BackendEnv。
var ChildEnvKeep = func() map[string]bool {
	m := make(map[string]bool, len(commonEnvKeep))
	for _, k := range commonEnvKeep {
		m[k] = true
	}
	return m
}()

// AddChildEnvKeep 把后端声明的变量加进白名单。启动时注册，不和读白名单的代码并发。
func AddChildEnvKeep(names ...string) {
	for _, n := range names {
		ChildEnvKeep[n] = true
	}
}

// RemoveChildEnvKeep 撤销 AddChildEnvKeep（测试用）。
func RemoveChildEnvKeep(names ...string) {
	for _, n := range names {
		if !slices.Contains(commonEnvKeep, n) {
			delete(ChildEnvKeep, n)
		}
	}
}

// ChildEnv 按白名单过滤当前环境。另外保留 LC_*（含 LC_ALL）和 agentq 自己的 AGENTQ_* 变量。
func ChildEnv() []string {
	return filterEnv(func(name string) bool { return ChildEnvKeep[name] })
}

// BackendEnv 是启动 CLI 用的环境：公共部分加这个后端自己的变量。别家的登录变量不传，防止走错账号。
func BackendEnv(keep []string) []string {
	return filterEnv(func(name string) bool { return slices.Contains(commonEnvKeep, name) || slices.Contains(keep, name) })
}

func filterEnv(allow func(name string) bool) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if allow(name) || strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "AGENTQ_") {
			out = append(out, kv)
		}
	}
	return out
}

// NameMarker 出现在被启动进程的命令行里（Request.Label 的开头），用来确认 pid 没被别的程序复用。
// ps 会把控制字符和它认为不可打印的字符转义，名字里有这些时直接比就永远对不上（活着的任务被当成结束了），所以两边都用 psSafe 之后的。
func NameMarker(project string) string { return NamePrefix + PsSafe(project) + " " }

// NamePrefix：不知道项目时认进程标记用（记录丢了，见 runner.KillLostJob）。
const NamePrefix = "agentq "

// PsSafe 把 ps 可能转义的字符换成下划线：只留可打印 ASCII、字母、数字。
func PsSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 0x20 && r < 0x7f) || unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, s)
}

// ProcAlive：kill(pid, 0) 判断进程在不在；再用 ps 排除僵尸进程和 pid 被复用的情况。
func ProcAlive(pid int, marker string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH：不在了；EPERM：属于别的用户，不是我们启动的
	}
	cmd := exec.Command("/bin/ps", "-ww", "-o", "stat=", "-o", "command=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL="+PsLocale)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false // ps 找不到这个进程
		}
		return true // ps 本身跑不起来，只能信 kill(pid, 0)
	}
	line := strings.TrimSpace(string(out))
	if line == "" || strings.HasPrefix(line, "Z") {
		return false
	}
	return strings.Contains(line, marker)
}

// Liveness 是认进程的结论。
type Liveness int

const (
	ProcGone   Liveness = iota // 不在了，或者这个 pid 已经是别的程序
	ProcOurs                   // 就是这个任务的进程
	ProcUnsure                 // 进程在，但认不出是不是它：不能杀，也不能当结束了再起一个
)

// JobLiveness 认命令行里没有标记的进程（Caps.NoArgvLabel）：环境里有 AGENTQ_JOB=<任务 id> 就是它，环境读得到却没有就是 pid 被复用了。
// 环境读不到（macOS 上系统自带的程序）时看任务外壳：外壳命令行带 shellArg（`_job <任务目录> `），它还在等这个 pid，pid 就不可能被复用。
// 外壳也不在就下不了结论，返回 ProcUnsure。
func JobLiveness(pid int, jobID, shellArg string) Liveness {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return ProcGone
	}
	out, err := PsOutput("-ww", "-o", "stat=", "-p", strconv.Itoa(pid))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ProcGone
		}
		return ProcUnsure // ps 本身跑不起来
	}
	if s := strings.TrimSpace(out); s == "" || strings.HasPrefix(s, "Z") {
		return ProcGone
	}
	found, readable := procEnvHas(pid, JobEnv+"="+jobID)
	switch {
	case found:
		return ProcOurs
	case readable:
		return ProcGone
	}
	all, err := PsOutput("-A", "-ww", "-o", "command=")
	if err == nil && strings.Contains(all, shellArg) {
		return ProcOurs
	}
	return ProcUnsure
}

// KillGroup 对进程组发 SIGTERM，grace 之后仍在就 SIGKILL。返回是否用了 SIGKILL。
func KillGroup(pgid int, grace time.Duration) bool {
	if pgid <= 0 {
		return false
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if WaitGroupGone(pgid, grace) {
		return false
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	WaitGroupGone(pgid, 3*time.Second)
	return true
}

func GroupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// JobEnv 是 CLI 和它拉起的所有子进程都继承的环境变量，值是任务 id；收尾清残留进程组时用来认出是这个任务留下的。
const JobEnv = "AGENTQ_JOB"

// GroupMarked：进程组 pgid 里有没有进程带着这个任务的标记（ps -E 显示的环境里有 AGENTQ_JOB=<id>）。
// 不认命令行里的进程标记：它只到项目一级，组号被同项目另一个任务的 CLI 占用时会误杀。
func GroupMarked(pgid int, jobID string) (bool, error) {
	out, err := PsOutput("-A", "-o", "pid=", "-o", "pgid=")
	if err != nil {
		return false, err
	}
	var pids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == strconv.Itoa(pgid) {
			pids = append(pids, f[0])
		}
	}
	if len(pids) == 0 {
		return false, nil
	}
	return procsEnvMarked(pids, JobEnv+"="+jobID)
}

func PsOutput(args ...string) (string, error) {
	cmd := exec.Command("/bin/ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL="+PsLocale)
	out, err := cmd.Output()
	return string(out), err
}

var bootSecRe = regexp.MustCompile(`sec = (\d+)`)

// parseBootTime 解析 sysctl -n kern.boottime 的输出；bootTime 按平台在 internal/proc/proc_darwin.go、internal/proc/proc_linux.go。
func parseBootTime(s string) (int64, error) {
	m := bootSecRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("kern.boottime 的输出看不懂：%q", strings.TrimSpace(s))
	}
	return strconv.ParseInt(m[1], 10, 64)
}

func WaitGroupGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !GroupAlive(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// BoundCmd：调用方拿着锁等外部命令时用。超时杀整个进程组（只杀命令本身的话，它留下的子进程还占着输出管道，读输出的一方永远等不到 EOF，锁也放不开），
// 之后最多再等 5 秒。cmd 必须是 CommandContext 建的。
func BoundCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone // 组里已经没有进程了：命令是自己结束的，别把它算成被取消
	}
	cmd.WaitDelay = 5 * time.Second
}

// ClearLeftovers：命令结束后清掉它留在进程组里的子进程（boundCmd 建的组）。组里还有进程时组号不会被复用；走到 WaitDelay 才返回的就是这种。
func ClearLeftovers(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// SystemctlEnv：ssh、su、cron 进来可能没有 XDG_RUNTIME_DIR，systemctl --user 就找不到用户总线；目录在就补上，不在不编。
func SystemctlEnv(env []string, exists func(string) bool) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			return env
		}
	}
	if dir := "/run/user/" + strconv.Itoa(os.Getuid()); exists(dir) {
		return append(env, "XDG_RUNTIME_DIR="+dir)
	}
	return env
}

// ParseProcStatBtime 取 /proc/stat 的 btime 行（本次开机的 Unix 秒）。
func ParseProcStatBtime(text string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "btime" {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n > 0 {
				return n, nil
			}
		}
	}
	return 0, errors.New("/proc/stat 里没有可用的 btime")
}

// ProcEnvMarked：pids 里有没有进程的 /proc/<pid>/environ 带着 mark（整项相等，不按前缀认）。
// 读不了（已退出、属于别的用户）的当没有：宁可不杀。
func ProcEnvMarked(procRoot string, pids []string, mark string) bool {
	for _, pid := range pids {
		b, err := os.ReadFile(filepath.Join(procRoot, pid, "environ"))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if kv == mark {
				return true
			}
		}
	}
	return false
}

// BootSlack：开机时间相差不超过这么多不算重启（系统校时会让 kern.boottime 变几秒）。
const BootSlack = 60
