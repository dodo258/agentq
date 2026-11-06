// Package env 提供进程内共用的运行环境：路径、时钟、日志、当前配置。每个进程只有一份，所有模块共享同一个指针。
package env

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/proc"
)

// Env 是进程内共用的运行环境。Cfg 运行中会被重载，boot 是进程内缓存，所以每个进程只有一份，大家共享指针。
type Env struct {
	Home string
	Cfg  config.Config
	Now  func() time.Time
	Out  io.Writer
	// 本次开机时间（秒），第一次用到时读，0 = 读不出。
	Boot     int64
	BootRead bool
}

// ForPaths 建一个只用来算路径和标签的临时环境：不读配置文件，没有时钟和输出。安装和服务文件要算别的家目录时用。
func ForPaths(home string, cfg config.Config) *Env { return &Env{Home: home, Cfg: cfg} }

// ForLog 建一个只用来写一行带时间的日志的临时环境：配置读不出、正式的环境建不起来时用。
func ForLog(out io.Writer) *Env { return &Env{Now: time.Now, Out: out} }

// New 建运行环境：确保数据目录在，读配置（文件不存在时写一份）。
func New(home string, out io.Writer) (*Env, error) {
	a := &Env{Home: home, Now: time.Now, Out: out}
	// 只给集成测试用：AGENTQ_TEST_NOW（RFC3339）把这个进程的「现在」拨到那一刻，之后照常走（测补跑窗口等按钟点判断的逻辑）
	if v := os.Getenv("AGENTQ_TEST_NOW"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			t0 := time.Now()
			a.Now = func() time.Time { return t.In(time.Local).Add(time.Since(t0)) }
		}
	}
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(a.DataDir(), 0o700); err != nil {
		return nil, err
	}
	cfg, err := config.Load(a.ConfigPath())
	if err != nil {
		return nil, err
	}
	a.Cfg = cfg
	return a, nil
}

func (a *Env) BinPath() string { return filepath.Join(a.Home, ".local", "bin", "agentq") }

func (a *Env) LaunchdLogPath() string { return filepath.Join(a.DataDir(), "launchd.log") }

func (a *Env) Tilde(p string) string {
	if rel, err := filepath.Rel(a.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

func (a *Env) DataDir() string { return filepath.Join(a.Home, ".agentq") }

func (a *Env) ConfigPath() string { return filepath.Join(a.DataDir(), "config.json") }

func (a *Env) StatePath() string { return filepath.Join(a.DataDir(), "state.json") }

func (a *Env) LockPath() string { return filepath.Join(a.DataDir(), "tick.lock") }

func (a *Env) JobDir(id string) string { return filepath.Join(a.DataDir(), "jobs", id) }

func (a *Env) ProjectsRoot() string { return fsx.ExpandHome(a.Home, a.Cfg.ProjectsRoot) }

// ProjectDir 是这个名字的项目目录：projects_root 下有同名的就是它，否则看 extra_projects，都没有就是 projects_root 下这个位置（调用方会发现不是项目）。
func (a *Env) ProjectDir(name string) string {
	dir := filepath.Join(a.ProjectsRoot(), name)
	if extra := a.Cfg.ExtraProjectDir(name); extra != "" {
		if _, err := os.Lstat(dir); err != nil {
			return extra
		}
	}
	return dir
}

// RootMayBeAbsent：projects_root 不存在算不算「没有项目」。只有默认的 ~/projects 算，新机器上本来就没有。
// 自己配的目录不见了可能是盘没挂上，不知道里面还有什么，要当错误。
func (a *Env) RootMayBeAbsent() bool {
	return a.Cfg.ProjectsRoot == config.Default().ProjectsRoot
}

// BackendFor 建名字对应的后端实例，空名字是默认后端（老任务记录没写后端就是它）。没注册过的名字也有实例，启动被拒绝、结果读不出，见 backend.Get。
func (a *Env) BackendFor(name string) backend.Backend {
	return backend.Get(name, backend.Options{Home: a.Home, Path: fsx.ExpandHome(a.Home, a.Cfg.BackendPath(name))})
}

// DefaultBackend 是新任务默认用的后端。
func (a *Env) DefaultBackend() backend.Backend { return a.BackendFor(a.Cfg.Backend) }

// claudeHost：默认后端是 Claude。这时 agentq 的报告和锁还放 ~/.claude，老用户的路径不变；别的后端不往 ~/.claude 写东西。
func (a *Env) claudeHost() bool { return a.Cfg.Backend == "" || a.Cfg.Backend == backend.Default }

// ReportDir 是每日汇总和早报的目录。切默认后端后老报告留在原处，只有新写的换地方。
func (a *Env) ReportDir() string {
	if a.claudeHost() {
		return filepath.Join(a.Home, ".claude", "nightly")
	}
	return filepath.Join(a.DataDir(), "nightly")
}

// ReportStateDir 放早报已推送的标记。
func (a *Env) ReportStateDir() string {
	if a.claudeHost() {
		return filepath.Join(a.Home, ".claude", "state")
	}
	return filepath.Join(a.DataDir(), "state")
}

// LocksDir 是引擎和 with-lock.sh 共用的锁目录。脚本不读配置，只看 ~/.agentq/locks 在不在，这里守同一条规矩：
// 在就用它；不在时默认后端是 Claude 用 ~/.claude/state/locks，别的后端建它。切回 Claude 后目录还在，两边仍然锁到一处。
func (a *Env) LocksDir() string {
	dir := filepath.Join(a.DataDir(), "locks")
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	if a.claudeHost() {
		return filepath.Join(a.Home, ".claude", "state", "locks")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.Logf("建锁目录 %s 失败：%v", dir, err)
	}
	return dir
}

// HeadlessWord 是给人看的话里怎么称呼无头运行的这个后端，name 空是 Claude（和 Job.Backend 一样）。
func (a *Env) HeadlessWord(name string) string {
	if name == "" || name == backend.Default {
		return "claude -p"
	}
	return name
}

func (a *Env) NightlyPath(t time.Time) string {
	return filepath.Join(a.ReportDir(), t.Format("2006-01-02")+"-agentq.md")
}

func (a *Env) Logf(format string, args ...any) {
	fmt.Fprintf(a.Out, "%s %s\n", a.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// ErrLogged 表示错误已经写进日志，main 只需要以非零状态退出。
var ErrLogged = errors.New("已记录")

// CurBoot 返回本次开机时间（只读一次），读不出返回 0：不判断是否重启过，清进程组照样要核对标记。
func (a *Env) CurBoot() int64 {
	if !a.BootRead {
		a.BootRead = true
		t, err := proc.BootTime()
		if err != nil {
			a.Logf("读开机时间失败，本次不判断任务启动后是否重启过：%v", err)
		}
		a.Boot = t
	}
	return a.Boot
}
