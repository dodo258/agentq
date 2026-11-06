// Package service 安装、查询后台服务：macOS 走 launchd，Linux 走 systemd --user。
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// Service 是管后台服务的模块实例。
type Service struct {
	*env.Env
	// Launchctl 为空时执行 /bin/launchctl；测试里替换掉。
	Launchctl func(args ...string) ([]byte, error)
	// Systemctl、Loginctl 为空时 Linux 执行真实命令（Systemctl 的参数不含 --user）；测试里替换掉。
	Systemctl func(args ...string) ([]byte, error)
	Loginctl  func(args ...string) ([]byte, error)
}

// RoutineStatus：夜班/复盘的定点时刻和开关。
func (a *Service) RoutineStatus(kind string) string {
	r := config.Routines[kind]
	if r.Enabled(a.Cfg) {
		return fmt.Sprintf("%s %s 开（%s=true）", r.Name, r.At(), r.Key)
	}
	return fmt.Sprintf("%s %s 关（%s=false，%s 叫醒也不执行）", r.Name, r.At(), r.Key, a.Svc().Words().Name)
}

// ServiceLabel 保留本机已安装服务的标识，新安装使用通用默认值。
func (a *Service) ServiceLabel() string {
	if a.Cfg.LaunchdLabel != "" {
		return a.Cfg.LaunchdLabel
	}
	return config.LaunchdLabel
}

func (a *Service) RoutineLabel(kind string) string { return a.ServiceLabel() + "." + kind }

func (a *Service) ListenLabel() string { return a.ServiceLabel() + ".listen" }

func (a *Service) LaunchdPrefix() string {
	label := a.ServiceLabel()
	return label[:strings.LastIndex(label, ".")+1]
}

func (a *Service) PlistPath() string { return a.PlistPathFor(a.ServiceLabel()) }

func (a *Service) PlistPathFor(label string) string {
	return filepath.Join(a.Home, "Library", "LaunchAgents", label+".plist")
}

// routineLabel：夜班、复盘的 launchd 服务名，如 io.agentq.agentq.night。
func routineLabel(kind string) string { return config.LaunchdLabel + "." + kind }

// launchdService 是 install 装的一个 launchd 服务。
type launchdService struct {
	Label string
	Plist string // plist 内容
}

// Services：每 5 分钟的 tick，夜班、复盘两个定点任务，常驻的 Telegram 监听。
func (a *Service) Services() []launchdService {
	return []launchdService{
		{a.ServiceLabel(), a.PlistContent()},
		{a.RoutineLabel(config.KindNight), a.RoutinePlistContent(config.Routines[config.KindNight])},
		{a.RoutineLabel(config.KindReview), a.RoutinePlistContent(config.Routines[config.KindReview])},
		{a.ListenLabel(), a.ListenPlistContent()},
	}
}

func (a *Service) GuardPath() string { return filepath.Join(a.DataDir(), "tick-guard.sh") }

func (a *Service) GuardFailsPath() string { return filepath.Join(a.DataDir(), "guard-fails") }

func (a *Service) LaunchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (a *Service) LaunchdPATH() string {
	return strings.Join([]string{
		filepath.Join(a.Home, ".local", "bin"), filepath.Join(a.Home, ".npm-global", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}, ":")
}

// LaunchdEnv 是安装时检查 CLI 用的环境，要和服务真正拿到的一样：plist 声明的 HOME、PATH，加上 launchd 和 systemd 自己给的 USER、LOGNAME
// （macOS 上 CLI 靠 USER 去钥匙串取订阅凭据，少了就判成没登录）。不继承安装 shell 的 token 或代理。
func (a *Service) LaunchdEnv() []string {
	env := []string{"HOME=" + a.Home, "PATH=" + a.LaunchdPATH()}
	if u, err := user.Current(); err == nil && u.Username != "" {
		env = append(env, "USER="+u.Username, "LOGNAME="+u.Username)
	}
	return env
}

// GuardContent 是 launchd 调用的保护脚本：执行 agentq tick，记连续非 0 退出的次数（scripts/update.sh 装新版本时清零），
// 连续 2 次就把 scripts/update.sh 留下的 agentq.prev 原子改名换回来（坏的留成 agentq.bad），在当天汇总写一行「已自动回滚」。
// agentq.prev 换回来后就没了，换回的版本再失败也不会来回换；没有 agentq.prev 时第 2 次失败写一行报警。
// 汇总的文件名、标题和行格式和 appendNightlyLine 一致。
func (a *Service) GuardContent() string {
	return `#!/bin/sh
# agentq launchd 保护脚本，由 agentq install 生成，改了会被下次 install 覆盖。
bin=` + fsx.ShQuote(a.BinPath()) + `
fails=` + fsx.ShQuote(a.GuardFailsPath()) + `
nightly_dir=` + fsx.ShQuote(a.ReportDir()) + `

"$bin" tick
code=$?
if [ "$code" -eq 0 ]; then
	rm -f "$fails"
	exit 0
fi
n=$(cat "$fails" 2>/dev/null)
case "$n" in '' | *[!0-9]*) n=0 ;; esac
n=$((n + 1))
echo "$n" >"$fails"
echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：agentq tick 退出码 $code（连续第 $n 次非 0）"
[ "$n" -eq 2 ] || exit "$code"

summary() {
	day=$(date +%Y-%m-%d)
	f="$nightly_dir/$day-agentq.md"
	mkdir -p "$nightly_dir"
	[ -s "$f" ] || printf '# agentq 任务汇总 %s\n\n' "$day" >>"$f"
	printf -- '- %s %s\n' "$(date +%H:%M)" "$1" >>"$f"
}
prev_time=$(` + guardPrevTimeCmd + `)
if [ -f "$bin.prev" ] && { cp -p "$bin" "$bin.bad" 2>/dev/null; mv -f "$bin.prev" "$bin"; }; then
	rm -f "$fails"
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：已自动回滚，$bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad"
	summary "已自动回滚：agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），已把 $bin.prev（$prev_time 构建）换回 $bin，出错的版本留在 $bin.bad；原因看 ~/.agentq/launchd.log"
else
	echo "$(date '+%Y-%m-%d %H:%M:%S') 保护脚本：没有可换回的 $bin.prev（或换回失败），没有回滚"
	summary "agentq tick 连续 2 次非 0 退出（最后一次退出码 $code），没有可换回的 $bin.prev，没有回滚，要人看 ~/.agentq/launchd.log"
fi
exit "$code"
`
}

// plist 按方案：StartInterval 300、RunAtLoad、PATH、日志到 ~/.agentq/launchd.log；
// ProgramArguments 是 /bin/sh 保护脚本（它再执行 agentq tick，连续失败时自动回滚），不直接调 agentq。
// AbandonProcessGroup：tick 退出时 launchd 不去杀同组进程（claude 本来就在自己的进程组里，这里再保险一层）。
func (a *Service) PlistContent() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.ServiceLabel() + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>` + fsx.XmlEscape(a.GuardPath()) + `</string>
	</array>
	<key>StartInterval</key>
	<integer>300</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + fsx.XmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + fsx.XmlEscape(a.LaunchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
</dict>
</plist>
`
}

// RoutinePlistContent：每天 r.hour:r.minute 执行 `agentq night|review --scheduled`（开关没打开时它只记一行日志）。
// 不经保护脚本（回滚由每 5 分钟的 tick 管），没有 RunAtLoad（装上时不能立刻跑一次夜班）；PATH、日志同 tick 的 plist。
// 机器在那个时刻睡着时，launchd 在醒来后补跑一次。
func (a *Service) RoutinePlistContent(r config.Routine) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.RoutineLabel(r.Kind) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + fsx.XmlEscape(a.BinPath()) + `</string>
		<string>` + r.Kind + `</string>
		<string>--scheduled</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>` + fmt.Sprint(r.Hour) + `</integer>
		<key>Minute</key>
		<integer>` + fmt.Sprint(r.Minute) + `</integer>
	</dict>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + fsx.XmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + fsx.XmlEscape(a.LaunchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
</dict>
</plist>
`
}

// ListenPlistContent：常驻执行 `agentq listen`。KeepAlive：不管怎么退出（挂了、换了新版本主动退出）launchd 都拉起来，
// 两次启动至少隔 ThrottleInterval 10 秒；没配 Telegram 时它自己歇着不退出，不会被反复拉起。
// 不经保护脚本（回滚由每 5 分钟的 tick 管，换回旧版本后它也会发现文件变了自己重启）；PATH、日志同 tick 的 plist。
func (a *Service) ListenPlistContent() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + a.ListenLabel() + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + fsx.XmlEscape(a.BinPath()) + `</string>
		<string>listen</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + fsx.XmlEscape(a.Home) + `</string>
		<key>PATH</key>
		<string>` + fsx.XmlEscape(a.LaunchdPATH()) + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + fsx.XmlEscape(a.LaunchdLogPath()) + `</string>
</dict>
</plist>
`
}

// runLaunchctl 默认执行 /bin/launchctl；测试里换成记录参数的假函数，绝不碰真实 launchd。
func runLaunchctl(args ...string) ([]byte, error) {
	return exec.Command("/bin/launchctl", args...).CombinedOutput()
}

func (a *Service) launchctl(args ...string) ([]byte, error) {
	if a.Launchctl != nil {
		return a.Launchctl(args...)
	}
	return runLaunchctl(args...)
}

// ForPaths 建一个只用来算路径、标签和查服务的临时实例：安装引擎用，不读配置文件，没有时钟和输出。假命令照传。
func ForPaths(home string, cfg config.Config, launchctl, systemctl, loginctl func(...string) ([]byte, error)) *Service {
	return &Service{Env: env.ForPaths(home, cfg), Launchctl: launchctl, Systemctl: systemctl, Loginctl: loginctl}
}

// CheckRealLaunchd：没有替换 Launchctl、Systemctl 时会动真实服务，只允许 Home 是当前用户真实家目录（防测试误装）。
func (a *Service) CheckRealLaunchd() error {
	if a.Launchctl != nil || a.Systemctl != nil {
		return nil
	}
	tool := a.Svc().Words().Tool
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("查不到当前用户的家目录，不调用 %s：%w", tool, err)
	}
	if filepath.Clean(u.HomeDir) != filepath.Clean(a.Home) {
		return fmt.Errorf("HOME=%s 不是当前用户的家目录 %s，不调用真实 %s", a.Home, u.HomeDir, tool)
	}
	return nil
}

func (a *Service) serviceTarget(label string) string { return a.LaunchdDomain() + "/" + label }

func (a *Service) serviceLoaded(label string) bool { return a.Svc().IsLoaded(label) }

// CmdInstall 先写好保护脚本和所有 plist，再逐个服务：已装着就 bootout，再 bootstrap；可重复执行。
func (a *Service) CmdInstall() error {
	if _, err := os.Stat(filepath.Join(a.Home, ".local", "share", "agentq", "receipt.json")); err == nil {
		return fmt.Errorf("managed installation: use agentq setup --services --yes; legacy install would overwrite its guard")
	}
	if err := a.CheckRealLaunchd(); err != nil {
		return err
	}
	m := a.Svc()
	w := m.Words()
	if err := m.Available(); err != nil {
		return err
	}
	if fi, err := os.Stat(a.BinPath()); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("没找到可执行的 %s，先在项目目录执行：scripts/update.sh", a.BinPath())
	}
	// 先写保护脚本和 plist：写失败时旧服务还在跑，不会落到「旧的卸了、新的没装」
	if err := fsx.WriteFileAtomic(a.GuardPath(), []byte(a.GuardContent())); err != nil {
		return err
	}
	svcs := m.Specs()
	var labels []string
	for _, s := range svcs {
		labels = append(labels, s.Label)
		for _, f := range s.Files {
			if err := fsx.WriteFileAtomic(f.Path, []byte(f.Content)); err != nil {
				return err
			}
		}
	}
	if err := m.Reload(); err != nil {
		return err
	}
	for _, s := range svcs {
		if a.serviceLoaded(s.Label) {
			if out, err := m.Unload(s.Label); err != nil {
				return fmt.Errorf("卸下旧的 %s 失败（新 %s 已写好，旧服务仍在）：%v %s", s.Label, w.UnitFile, err, strings.TrimSpace(string(out)))
			}
		}
		if out, err := m.Load(s.Label); err != nil {
			return fmt.Errorf("%s %s %s 失败%s：%v %s", w.Tool, w.LoadVerb, s.Label, w.loadNote, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Fprintf(a.Out, "已安装：%s\n%s 每 5 分钟经保护脚本 %s 执行一次 %s tick（装上时先执行一次；连续 2 次失败自动换回 agentq.prev）\n日志：%s\n",
		svcs[0].Files[0].Path, w.Name, a.GuardPath(), a.BinPath(), a.LaunchdLogPath())
	fmt.Fprintf(a.Out, "定点任务：%s、%s（%s；%s）\n", a.RoutineLabel(config.KindNight), a.RoutineLabel(config.KindReview),
		a.RoutineStatus(config.KindNight), a.RoutineStatus(config.KindReview))
	fmt.Fprintf(a.Out, "常驻服务：%s（%s 保持运行，收你的 Telegram 消息；没配 Telegram 时只歇着）\n", a.ListenLabel(), w.Name)
	if notes, err := m.Doctor(labels); err == nil {
		for _, n := range notes {
			fmt.Fprintln(a.Out, n)
		}
	}
	if a.Cfg.Paused {
		fmt.Fprintln(a.Out, "注意：当前已暂停（paused=true），tick 只收尾不启动；要开始自动接活执行 agentq resume")
	}
	return nil
}

// CmdUninstall bootout 并删掉所有 plist；~/.agentq 里的配置、状态和任务记录保留。
func (a *Service) CmdUninstall() error {
	if _, err := os.Stat(filepath.Join(a.Home, ".local", "share", "agentq", "receipt.json")); err == nil {
		return fmt.Errorf("managed installation: use agentq remove --yes (keeps data/config and supports restore)")
	}
	if err := a.CheckRealLaunchd(); err != nil {
		return err
	}
	m := a.Svc()
	w := m.Words()
	if err := m.Available(); err != nil {
		return err
	}
	for _, s := range m.Specs() {
		if a.serviceLoaded(s.Label) {
			if out, err := m.Unload(s.Label); err != nil {
				return fmt.Errorf("%s %s %s 失败：%v %s", w.Tool, w.UnloadVerb, s.Label, err, strings.TrimSpace(string(out)))
			}
		}
		for _, f := range s.Files {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := m.Reload(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "已卸载 %s、夜班和复盘两个定点任务、Telegram 监听（%s 里的配置、状态和任务记录保留；正在跑的任务不受影响，它们在自己的进程组里，%s 卸下服务时不会一起清掉）\n"+
		"注意：还在跑的任务结束时仍会 tick 一次、接下一条待做；要彻底不接新活，再执行 agentq pause\n", a.ServiceLabel(), a.DataDir(), w.Name)
	return nil
}

func AuxiliaryService(home string, cfg config.Config, kind string, interval int) launchdService {
	a := &Service{Env: env.ForPaths(home, cfg)}
	label := a.ServiceLabel() + "." + kind
	args := "<string>" + fsx.XmlEscape(a.BinPath()) + "</string><string>maintenance</string><string>" + kind + "</string>"
	if kind == "watchdog" {
		args = "<string>/bin/sh</string><string>" + fsx.XmlEscape(filepath.Join(home, ".local", "share", "agentq", "support", "scripts", "watchdog.sh")) + "</string>"
	}
	return launchdService{label, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + fsx.XmlEscape(label) + `</string>
<key>ProgramArguments</key><array>` + args + `</array>
<key>StartInterval</key><integer>` + fmt.Sprint(interval) + `</integer><key>AbandonProcessGroup</key><true/>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>` + fsx.XmlEscape(home) + `</string><key>PATH</key><string>` + fsx.XmlEscape(a.LaunchdPATH()) + `</string></dict>
<key>StandardOutPath</key><string>` + fsx.XmlEscape(filepath.Join(home, ".agentq", kind+".log")) + `</string>
<key>StandardErrorPath</key><string>` + fsx.XmlEscape(filepath.Join(home, ".agentq", kind+".log")) + `</string></dict></plist>
`}
}

// launchdLogTail：从服务的 plist 取日志路径（先 StandardErrorPath 再 StandardOutPath），返回末尾 3 行；取不到返回空。
func (a *Service) launchdLogTail(label string) string {
	plist := a.PlistPathFor(label)
	for _, key := range []string{"StandardErrorPath", "StandardOutPath"} {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
		p := strings.TrimSpace(string(out))
		if err != nil || p == "" {
			continue
		}
		if t := strings.TrimSpace(fsx.TailLines(p, 3)); t != "" {
			return "日志 " + p + " 末尾：" + textfmt.OneLine(t, 200)
		}
	}
	return ""
}

// serviceFile 是装一个后台服务要写的一个文件。launchd 一个服务一个 plist；systemd 定时任务是 .service 加 .timer 两个。
type serviceFile struct {
	Path    string
	Content string
}

// ServiceSpec 是一个后台服务：label 是平台无关的名字（收据、日志、巡逻指纹都用它），files 是它在这个平台上的文件。
type ServiceSpec struct {
	Label string
	Files []serviceFile
}

// ServiceState 是巡逻看到的一个服务：pid 为 "-" 表示现在没在跑，code 是上次退出码（"-" 表示没退出过，负数是被信号终止）。
type ServiceState struct {
	Label, Pid, Code string
}

// serviceWords 是给人看的文字里真正因平台不同的词。
type serviceWords struct {
	Name       string // launchd、systemd --user
	List       string // 巡逻证据里状态的来源
	Tool       string // 被调用的命令
	LoadVerb   string
	UnloadVerb string
	loadNote   string // 装失败时的说明，带括号
	UnitFile   string // plist、unit
	KeepAlive  string // 常驻服务靠什么拉起
}

// ServiceManager 只抽各平台真正不同的那几件事：服务文件长什么样、怎么装卸、怎么读状态。
// 流程（先写文件再逐个装、事务回滚、巡逻判断）留在调用方，两个平台共用。
type ServiceManager interface {
	Words() serviceWords
	// available：这台机器现在能不能管服务。不能时返回说清原因的错误，绝不静默跳过。
	Available() error
	// specs 顺序固定：tick、夜班、复盘、Telegram 监听。
	Specs() []ServiceSpec
	AuxSpec(kind string, interval int) ServiceSpec
	IsLoaded(label string) bool
	Load(label string) ([]byte, error)
	Unload(label string) ([]byte, error)
	// reload：服务文件改过之后、装服务之前调用。launchd 不需要。
	Reload() error
	List() ([]ServiceState, error)
	LogTail(label string) string
	// doctor 给 agentq doctor 用：只读，返回要提示用户的话；labels 里的服务不正常时返回错误。
	Doctor(labels []string) ([]string, error)
}

// Svc 选这次用的服务管理：测试换了哪个假命令就用哪个，否则按编译平台（service_darwin.go、service_linux.go）。
func (a *Service) Svc() ServiceManager {
	switch {
	case a.Systemctl != nil:
		return SystemdManager{a: a, ctl: a.Systemctl, loginctl: a.Loginctl}
	case a.Launchctl != nil:
		return LaunchdManager{a: a, ctl: a.Launchctl}
	}
	return defaultServiceManager(a)
}

// LaunchdManager 是 macOS 的实现，plist 内容和调用的 launchctl 参数都在 internal/service/service.go。
type LaunchdManager struct {
	a   *Service
	ctl func(args ...string) ([]byte, error)
}

func (m LaunchdManager) Words() serviceWords {
	return serviceWords{
		Name: "launchd", List: "launchctl list", Tool: "launchctl", LoadVerb: "bootstrap", UnloadVerb: "bootout",
		loadNote: "（它现在没装着，再执行一次 agentq install；刚卸下旧服务时偶尔会报 5: Input/output error）",
		UnitFile: "plist", KeepAlive: "KeepAlive",
	}
}

func (m LaunchdManager) Available() error { return nil }

func (m LaunchdManager) Specs() []ServiceSpec {
	var out []ServiceSpec
	for _, s := range m.a.Services() {
		out = append(out, m.spec(s))
	}
	return out
}

func (m LaunchdManager) spec(s launchdService) ServiceSpec {
	return ServiceSpec{s.Label, []serviceFile{{m.a.PlistPathFor(s.Label), s.Plist}}}
}

func (m LaunchdManager) AuxSpec(kind string, interval int) ServiceSpec {
	return m.spec(AuxiliaryService(m.a.Home, m.a.Cfg, kind, interval))
}

func (m LaunchdManager) IsLoaded(label string) bool {
	_, err := m.ctl("print", m.a.serviceTarget(label))
	return err == nil
}

func (m LaunchdManager) Load(label string) ([]byte, error) {
	return m.ctl("bootstrap", m.a.LaunchdDomain(), m.a.PlistPathFor(label))
}

func (m LaunchdManager) Unload(label string) ([]byte, error) {
	return m.ctl("bootout", m.a.serviceTarget(label))
}

func (m LaunchdManager) Reload() error { return nil }

func (m LaunchdManager) List() ([]ServiceState, error) {
	out, err := m.ctl("list")
	if err != nil {
		return nil, fmt.Errorf("%s %s", err, out)
	}
	var states []ServiceState
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 {
			states = append(states, ServiceState{Label: f[2], Pid: f[0], Code: f[1]})
		}
	}
	return states, nil
}

func (m LaunchdManager) LogTail(label string) string { return m.a.launchdLogTail(label) }

func (m LaunchdManager) Doctor(labels []string) ([]string, error) { return nil, nil }

const systemctlTimeout = 2 * time.Minute

// SystemdManager 管 `systemd --user` 下的服务。定时任务是 <label>.timer 加 <label>.service，常驻服务只有 <label>.service。
type SystemdManager struct {
	a        *Service
	ctl      func(args ...string) ([]byte, error) // 不含 --user
	loginctl func(args ...string) ([]byte, error)
}

func (m SystemdManager) Words() serviceWords {
	return serviceWords{
		Name: "systemd --user", List: "systemctl --user show", Tool: "systemctl", LoadVerb: "enable", UnloadVerb: "disable",
		loadNote: "（它现在没装着，再执行一次 agentq install）",
		UnitFile: "unit", KeepAlive: "Restart=always",
	}
}

func (m SystemdManager) unitDir() string {
	return filepath.Join(m.a.Home, ".config", "systemd", "user")
}

// daemon：常驻服务（Telegram 监听）只有 .service，其他都是定时任务。
func daemonLabel(label string) bool { return strings.HasSuffix(label, ".listen") }

// anchorUnit 是 enable 和判断「装着没有」看的那个 unit。
func anchorUnit(label string) string {
	if daemonLabel(label) {
		return label + ".service"
	}
	return label + ".timer"
}

func (m SystemdManager) Available() error {
	out, err := m.ctl("show-environment")
	if err == nil {
		return nil
	}
	return fmt.Errorf("systemctl --user is not usable: %s. agentq's background services need a running systemd user instance. "+
		"WSL: set [boot] systemd=true in /etc/wsl.conf, then restart WSL. Containers usually have no systemd. "+
		"Over ssh or su: check XDG_RUNTIME_DIR and that a user session exists (loginctl). "+
		"No service was changed; manual commands such as agentq tick and agentq run still work",
		textfmt.OneLine(strings.TrimSpace(err.Error()+" "+string(out)), 300))
}

// systemdQuote 按 ExecStart 的规则把一个参数包成双引号：% 是占位符，$ 会被展开成环境变量，都要写两个。
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`
}

// systemdEnvQuote 是 Environment= 的版本：这里不展开 $。
func systemdEnvQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
}

// serviceSection 是所有 service 共用的部分。
// KillMode=process 是 launchd AbandonProcessGroup 的对应：任务外壳和 claude 是 tick 拉起的后台进程，还在同一个 cgroup 里，
// 默认的 control-group 会在 oneshot 主进程退出时把它们一起杀掉。
func (m SystemdManager) serviceSection(desc string, args []string, logPath string, extra ...string) string {
	quoted := make([]string, len(args))
	for n, s := range args {
		quoted[n] = systemdQuote(s)
	}
	logValue := strings.ReplaceAll(logPath, "%", "%%")
	lines := []string{
		"ExecStart=" + strings.Join(quoted, " "),
		"Environment=" + systemdEnvQuote("HOME="+m.a.Home),
		"Environment=" + systemdEnvQuote("PATH="+m.a.LaunchdPATH()),
		"StandardOutput=append:" + logValue,
		"StandardError=append:" + logValue,
		"KillMode=process",
	}
	lines = append(extra, lines...)
	return "[Service]\n" + strings.Join(lines, "\n") + "\n"
}

// scheduled：<label>.service（oneshot）加 <label>.timer。
// StartLimitIntervalSec=0：默认 10 秒内启动 5 次就拒绝（start-limit-hit），连着手动跑或重装时 tick 会失败；launchd 没有这种限制，节奏本来就由 timer 管。
func (m SystemdManager) scheduled(label, desc string, args []string, logPath string, timer ...string) ServiceSpec {
	service := "# 由 agentq 生成，改了会被下次安装覆盖。\n[Unit]\nDescription=" + desc + "\nStartLimitIntervalSec=0\n\n" +
		m.serviceSection(desc, args, logPath, "Type=oneshot")
	timerUnit := "# 由 agentq 生成，改了会被下次安装覆盖。\n[Unit]\nDescription=" + desc + " timer\n\n[Timer]\n" +
		strings.Join(timer, "\n") + "\n\n[Install]\nWantedBy=timers.target\n"
	return ServiceSpec{label, []serviceFile{
		{filepath.Join(m.unitDir(), label+".service"), service},
		{filepath.Join(m.unitDir(), label+".timer"), timerUnit},
	}}
}

func (m SystemdManager) Specs() []ServiceSpec {
	a := m.a
	listen := a.ListenLabel()
	return []ServiceSpec{
		// OnActiveSec=0 对应 RunAtLoad：装上和用户管理器启动时先跑一次；OnCalendar 按钟点每 5 分钟，不依赖上一次什么时候跑的。
		m.scheduled(a.ServiceLabel(), "agentq tick", []string{"/bin/sh", a.GuardPath()}, a.LaunchdLogPath(),
			"OnActiveSec=0", "OnCalendar=*:0/5", "AccuracySec=30s"),
		m.routineSpec(config.Routines[config.KindNight]),
		m.routineSpec(config.Routines[config.KindReview]),
		// Restart=always 对应 KeepAlive；StartLimitIntervalSec=0：一直拉，不因为连续失败放弃。
		{listen, []serviceFile{{filepath.Join(m.unitDir(), listen+".service"),
			"# 由 agentq 生成，改了会被下次安装覆盖。\n[Unit]\nDescription=agentq listen\nStartLimitIntervalSec=0\n\n" +
				m.serviceSection("agentq listen", []string{a.BinPath(), "listen"}, a.LaunchdLogPath(), "Type=simple", "Restart=always", "RestartSec=10") +
				"\n[Install]\nWantedBy=default.target\n"}}},
	}
}

// routineSpec：每天定点。不写 Persistent：装上时不能立刻跑，关机错过由 tick 的 catchUpRoutines 按窗口补（夜班不挤到白天）。
// 机器睡着错过时，systemd 在醒来后补跑一次，和 launchd 一样。
func (m SystemdManager) routineSpec(r config.Routine) ServiceSpec {
	a := m.a
	return m.scheduled(a.RoutineLabel(r.Kind), "agentq "+r.Kind, []string{a.BinPath(), r.Kind, "--scheduled"}, a.LaunchdLogPath(),
		fmt.Sprintf("OnCalendar=*-*-* %02d:%02d:00", r.Hour, r.Minute))
}

func (m SystemdManager) AuxSpec(kind string, interval int) ServiceSpec {
	a := m.a
	args := []string{a.BinPath(), "maintenance", kind}
	if kind == "watchdog" {
		args = []string{"/bin/sh", filepath.Join(a.Home, ".local", "share", "agentq", "support", "scripts", "watchdog.sh")}
	}
	return m.scheduled(a.ServiceLabel()+"."+kind, "agentq "+kind, args, filepath.Join(a.Home, ".agentq", kind+".log"),
		fmt.Sprintf("OnActiveSec=%ds", interval), fmt.Sprintf("OnUnitActiveSec=%ds", interval))
}

const systemdShowProps = "--property=Id,LoadState,ActiveState,UnitFileState,MainPID,ExecMainStatus,Result"

// parseSystemdShow 解析 systemctl show 的输出：多个 unit 用空行分开，每段以 Id= 认。
func parseSystemdShow(text string) map[string]map[string]string {
	units := map[string]map[string]string{}
	for _, block := range strings.Split(text, "\n\n") {
		props := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if id := props["Id"]; id != "" {
			units[id] = props
		}
	}
	return units
}

// unitLoaded：unit 在，且已 enable 或正在运行。只有文件没 enable 不算装着。
func unitLoaded(p map[string]string) bool {
	if p == nil || p["LoadState"] != "loaded" {
		return false
	}
	return p["UnitFileState"] == "enabled" || p["ActiveState"] == "active" || p["ActiveState"] == "activating"
}

func (m SystemdManager) IsLoaded(label string) bool {
	unit := anchorUnit(label)
	out, err := m.ctl("show", systemdShowProps, unit)
	if err != nil {
		return false
	}
	return unitLoaded(parseSystemdShow(string(out))[unit])
}

func (m SystemdManager) Load(label string) ([]byte, error) {
	return m.ctl("enable", "--now", anchorUnit(label))
}

// Unload 停 timer 和 service、取消 enable。任务在 KillMode=process 下不受影响。
func (m SystemdManager) Unload(label string) ([]byte, error) {
	unit := anchorUnit(label)
	out, err := m.ctl("disable", "--now", unit)
	if err != nil {
		// unit 文件已经不在（事务中断后恢复）时 disable 报错，但这时只要停下来
		if _, statErr := os.Stat(filepath.Join(m.unitDir(), unit)); !errors.Is(statErr, os.ErrNotExist) {
			return out, err
		}
		if out, err = m.ctl("stop", unit); err != nil {
			return out, err
		}
	}
	if !daemonLabel(label) {
		if out, err = m.ctl("stop", label+".service"); err != nil {
			return out, err
		}
	}
	// reset-failed 只是清掉上次失败的显示，unit 没失败时会报错，不影响结果
	_, _ = m.ctl("reset-failed", label+".service")
	return nil, nil
}

func (m SystemdManager) Reload() error {
	if out, err := m.ctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// systemdExitCode 把 Result 和 ExecMainStatus 换成和 launchctl list 一样的退出码：被信号终止是负数，
// 其他没有退出码的失败（超时、拉起次数超限）记 1。
func systemdExitCode(result, status string) string {
	switch result {
	case "", "success":
		return "0"
	case "signal", "core-dump":
		return "-" + status
	}
	if status == "" || status == "0" {
		return "1"
	}
	return status
}

// List 只查 agentq 自己的服务，读不到的（没装）不列。
func (m SystemdManager) List() ([]ServiceState, error) {
	a := m.a
	labels := []string{a.ServiceLabel(), a.RoutineLabel(config.KindNight), a.RoutineLabel(config.KindReview), a.ListenLabel(), a.ServiceLabel() + ".backup", a.ServiceLabel() + ".watchdog"}
	args := []string{"show", systemdShowProps}
	for _, label := range labels {
		args = append(args, label+".service")
		if !daemonLabel(label) {
			args = append(args, label+".timer")
		}
	}
	out, err := m.ctl(args...)
	if err != nil {
		return nil, fmt.Errorf("%s %s", err, out)
	}
	units := parseSystemdShow(string(out))
	var states []ServiceState
	for _, label := range labels {
		if !unitLoaded(units[anchorUnit(label)]) {
			continue
		}
		svc := units[label+".service"]
		pid := svc["MainPID"]
		if pid == "" || pid == "0" {
			pid = "-"
		}
		states = append(states, ServiceState{Label: label, Pid: pid, Code: systemdExitCode(svc["Result"], svc["ExecMainStatus"])})
	}
	return states, nil
}

// LogTail：从 unit 文件取日志路径，返回末尾 3 行；取不到返回空。
func (m SystemdManager) LogTail(label string) string {
	b, err := os.ReadFile(filepath.Join(m.unitDir(), label+".service"))
	if err != nil {
		return ""
	}
	for _, key := range []string{"StandardError=append:", "StandardOutput=append:"} {
		for _, line := range strings.Split(string(b), "\n") {
			p, ok := strings.CutPrefix(line, key)
			if !ok {
				continue
			}
			p = strings.ReplaceAll(p, "%%", "%")
			if t := strings.TrimSpace(fsx.TailLines(p, 3)); t != "" {
				return "日志 " + p + " 末尾：" + textfmt.OneLine(t, 200)
			}
		}
	}
	return ""
}

// Doctor 只读检查。linger 没开只提示，开不开由用户决定。
func (m SystemdManager) Doctor(labels []string) ([]string, error) {
	notes := []string{"Platform: Linux. iOS builds, Xcode and code signing require macOS; tasks that need them must run on a Mac."}
	if len(labels) == 0 {
		return notes, nil
	}
	if err := m.Available(); err != nil {
		return notes, err
	}
	for _, label := range labels {
		if !m.IsLoaded(label) {
			return notes, fmt.Errorf("service %s is in the install receipt but not enabled (systemctl --user status %s); repeat agentq setup with the original service options", label, anchorUnit(label))
		}
	}
	if m.loginctl == nil {
		return append(notes, "Cannot read linger: loginctl unavailable. Without linger, services and running tasks stop when you log out; check loginctl enable-linger yourself."), nil
	}
	out, err := m.loginctl("show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value")
	switch {
	case err != nil:
		notes = append(notes, "Cannot read linger: "+textfmt.OneLine(err.Error(), 200)+". Without linger, services and running tasks stop when you log out; check loginctl enable-linger yourself.")
	case strings.TrimSpace(string(out)) != "yes":
		notes = append(notes, "Linger is off: services and running tasks stop when you log out, and start again only when you log in. To keep them running, run this yourself: loginctl enable-linger. agentq does not change it.")
	}
	return notes, nil
}

// RunSystemctl 执行真实的 systemctl --user；测试里换成假函数或 PATH 里的假命令。
func RunSystemctl(args ...string) ([]byte, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, fmt.Errorf("systemctl not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"--user"}, args...)...)
	cmd.Env = proc.SystemctlEnv(os.Environ(), func(p string) bool { _, e := os.Stat(p); return e == nil })
	return cmd.CombinedOutput()
}

func runLoginctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "loginctl", args...).Output()
}

// ParseOSRelease 取 /etc/os-release 的 PRETTY_NAME，没有返回空。
func ParseOSRelease(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// TestLaunchctl 只给测试用：AGENTQ_TEST_LAUNCHCTL 指向假命令时，真实二进制不碰真实的 launchctl。没设时返回 nil。
func TestLaunchctl() func(...string) ([]byte, error) {
	p := os.Getenv("AGENTQ_TEST_LAUNCHCTL")
	if p == "" {
		return nil
	}
	return func(args ...string) ([]byte, error) { return exec.Command(p, args...).CombinedOutput() }
}

// ServiceInstallCommand 是提示里写的装监听命令：受管安装走 start（agentq install 会拒绝），旧版只装二进制的还是 install。
func (a *Service) ServiceInstallCommand() string {
	if _, err := os.Stat(filepath.Join(a.Home, ".local", "share", "agentq", "receipt.json")); err == nil {
		return "agentq start"
	}
	return "agentq install"
}
