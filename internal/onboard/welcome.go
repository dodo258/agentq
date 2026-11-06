package onboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/setup"
	"agentq/internal/state"
)

// IdleHint 是启动后没有待做时唯一要说的话（后端是 Claude 时）。别的后端见 idleHint。
const IdleHint = "在项目目录打开 Claude Code，说一句要做什么就行。比如：把这个项目的 README 补全。"

func (a *Service) idleHint() string {
	if name := a.DefaultBackend().Name(); name != backend.Default {
		return "在项目目录运行 " + name + "，说一句要做什么就行。比如：把这个项目的 README 补全。"
	}
	return IdleHint
}

// consentBody 是同意文字。改它就要加 setup.ConsentTextVersion。
const consentBody = `agentq 装好了。
· 你交代的任务由官方 CLI 的独立进程完成，它们能读写这个用户的所有文件（包括本地密钥）、运行命令。
· 任务内容会发给你所用的模型服务，用你自己的账号额度。agentq 本身不收集数据。
· 没有待做时什么都不做，不消耗额度。
· 暂停：agentq pause　卸载：agentq remove --yes
`

const consentPrompt = "按回车同意并启动，输入 n 只安装不启动："

// 同意的方式，写进 consent.json。
const (
	consentEnter     = "enter"
	consentAssumeYes = "assume-yes"
)

// engineState 是官方 CLI 现在的情况。别的毛病（版本太旧等）由 startChecks 报。
type engineState int

const (
	engineReady engineState = iota
	engineMissing
	engineLoggedOut
	engineBroken
)

// probeEngine 看 CLI。配置里还是默认的 claude 时按安装引擎的候选路径找，它可能在还没进 PATH 的 ~/.local/bin。
func (a *Service) probeEngine() (engineState, string) {
	explicit := a.Cfg.BackendPath(a.Cfg.Backend)
	if a.usesDefaultCLIPath() {
		explicit = ""
	}
	r, err := setup.ProbeBackendEnv(a.Cfg.Backend, a.Home, explicit, setup.InstallEnv(a.Home))
	_, canSetUp := a.cliSetup()
	switch {
	case err == nil:
		return engineReady, r.Path
	case errors.Is(err, backend.ErrNoCLI) && canSetUp:
		return engineMissing, r.Path
	case errors.Is(err, backend.ErrNotLoggedIn) && canSetUp:
		return engineLoggedOut, r.Path
	}
	return engineBroken, r.Path
}

// usesDefaultCLIPath：默认后端，配置里还是默认的命令名。
func (a *Service) usesDefaultCLIPath() bool {
	return (a.Cfg.Backend == "" || a.Cfg.Backend == backend.Default) && a.Cfg.ClaudePath == config.Default().ClaudePath
}

// cliSetup 是这个后端带用户装 CLI、登录的能力，别的后端可能没有。
func (a *Service) cliSetup() (backend.CLISetup, bool) {
	s, ok := backend.Get(a.Cfg.Backend, backend.Options{Home: a.Home}).(backend.CLISetup)
	return s, ok
}

// cliHelp 是给用户看的命令；没有这个能力时是空的，engineMissing、engineLoggedOut 也不会出现。
func (a *Service) cliHelp() backend.CLIHelp {
	if s, ok := a.cliSetup(); ok {
		return s.CLIHelp()
	}
	return backend.CLIHelp{}
}

// cmdWelcome 是一键脚本把终端交过来之后的那一步。只有明确同意才启动：回车、--assume-yes、AGENTQ_ASSUME_YES=1。
// 自动化环境和没有终端都只装不启动，退出 0：Homebrew 这类工具在这里默认继续，但它们改的是配置文件，这里授予的是代码执行权。
func (a *Service) cmdWelcome(assumeYes bool, o StartOptions, i *setup.Installer) error {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	engine, _ := a.probeEngine()
	method := ""
	switch {
	case assumeYes || getenv("AGENTQ_ASSUME_YES") == "1":
		method = consentAssumeYes
	case getenv("CI") != "" || getenv("NONINTERACTIVE") != "":
		a.installOnly("这是自动化环境（CI 或 NONINTERACTIVE），只安装，没有启动。", engine, getenv)
		return nil
	case !o.Interactive:
		a.installOnly("这里没有终端，没法问你，只安装，没有启动。", engine, getenv)
		return nil
	default:
		fmt.Fprint(a.Out, a.consentText(engine, getenv)+"\n")
		ok, err := askConsent(o.In, a.Out)
		if err != nil {
			return fmt.Errorf("读回答失败，没有启动：%w", err)
		}
		if !ok {
			a.installOnly("好，只安装，没有启动。", engine, getenv)
			return nil
		}
		method = consentEnter
	}
	if err := a.prepareEngine(context.Background(), engine, o); err != nil {
		return err
	}
	a.ensurePath(getenv)
	if err := a.startChecks(i); err != nil {
		return err
	}
	first := a.envFirstTime()
	openNotify := a.notifyOffByInstall(i)
	if err := a.recordConsent(method); err != nil {
		return err
	}
	if first {
		a.autoEnableEnv()
	}
	if openNotify {
		a.enableNotify()
	}
	if a.Cfg.PermissionMode != a.fullPermission() {
		if err := a.setFullPermission(); err != nil {
			return err
		}
	}
	if _, err := a.ensureServices(i); err != nil {
		return err
	}
	if err := a.startQuietly(); err != nil {
		return err
	}
	if a.hasWork() {
		a.StartReport()
		return nil
	}
	fmt.Fprintf(a.Out, "%s 后台已启动，每 5 分钟看一次待做。\n%s\n", checkMark(getenv), a.idleHint())
	return nil
}

// notifyOffByInstall：第一次同意，推送开关的「关」是安装时默认写下的（配置是这次安装新建的）。升级前就有的配置里的 false 是用户的选择，不动；读不了就当是用户的。
func (a *Service) notifyOffByInstall(i *setup.Installer) bool {
	if a.Cfg.NotifyEnabled {
		return false
	}
	c, err := setup.ReadConsent(a.Home)
	if err != nil || c != nil {
		return false
	}
	r, err := i.Receipt()
	return err == nil && r.ConfigCreated
}

// enableNotify 打开推送，只改这一个键。没配 Telegram 就是本机通知。打不开不影响启动，说一声。
func (a *Service) enableNotify() {
	if err := config.SetKey(a.ConfigPath(), "notify_enabled", "true", func() error {
		return fmt.Errorf("%s 不见了", a.ConfigPath())
	}); err != nil {
		fmt.Fprintf(a.Out, "通知没打开（后台照常）：%v\n", err)
		return
	}
	a.Cfg.NotifyEnabled = true
	fmt.Fprintf(a.Out, "任务做完会通知你（没配 Telegram 就弹本机通知）。不想要：把 %s 里的 notify_enabled 改成 false。\n", a.tilde(a.ConfigPath()))
}

// startQuietly 取消暂停、立刻看一次待做。调度日志是给 launchd.log 看的，第一屏不打；失败时才放出来。
func (a *Service) startQuietly() error {
	out := a.Out
	var log bytes.Buffer
	a.Out = &log
	defer func() { a.Out = out }()
	err := a.Sched.CmdSetPaused(false)
	if err != nil {
		err = fmt.Errorf("服务已装上，但取消暂停失败：%w", err)
	} else {
		err = a.startFirstTick()
	}
	if err != nil {
		out.Write(log.Bytes())
	}
	return err
}

// consentText 是固定的同意文字，加上这次还要多做的事。多做的事不改固定文字的版本。
func (a *Service) consentText(engine engineState, getenv func(string) string) string {
	var b strings.Builder
	b.WriteString(consentBody)
	help := a.cliHelp()
	if engine == engineMissing {
		if s, ok := a.cliSetup(); ok && s.DesktopInstalled(a.Home) {
			fmt.Fprintf(&b, "· 检测到 %s。后台任务要独立的命令行程序，桌面端不能代替。\n", help.Desktop)
		}
		fmt.Fprintf(&b, "· 还要安装官方 %s。按回车会运行官方命令：%s\n", help.Name, help.Install)
	}
	if engine == engineMissing || engine == engineLoggedOut {
		fmt.Fprintf(&b, "· %s 要登录。按回车后会打开浏览器（%s）；没登录不开后台。\n", help.Name, help.Login)
	}
	if !onPath(a.binDir(), getenv("PATH")) {
		if rc := setup.RCForPath(a.Home, getenv("SHELL"), getenv("ZDOTDIR")); rc != "" {
			fmt.Fprintf(&b, "· 会往 %s 加带标记的一行，把 ~/.local/bin 放进 PATH。改前备份，卸载时删掉。\n", a.tilde(rc))
		}
	}
	if line := a.envConsentLine(); line != "" {
		b.WriteString("· " + line + "\n")
	}
	return b.String()
}

// askConsent 只有空行算同意：n、其他输入、EOF 都不算，Ctrl-C 直接结束进程。提问前清掉终端里攒下的输入，等下载时多按的回车不能算回答。
func askConsent(in io.Reader, out io.Writer) (bool, error) {
	if f, ok := in.(*os.File); ok {
		flushTerminalInput(f)
	}
	fmt.Fprint(out, consentPrompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(out)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return line == "\n" || line == "\r\n", nil
}

// prepareEngine 同意之后把 CLI 弄好：没有就运行官方命令，没登录就登录。没有终端时不装不登录，只给命令。
func (a *Service) prepareEngine(ctx context.Context, engine engineState, o StartOptions) error {
	s, canSetUp := a.cliSetup()
	help := a.cliHelp()
	if engine == engineMissing || engine == engineLoggedOut {
		if !o.Interactive {
			a.printCLISteps(engine)
			return errors.New("这里没有终端，没有替你装 CLI 或登录，后台没开")
		}
	}
	if engine == engineMissing {
		fmt.Fprintf(a.Out, "运行官方安装命令：%s\n", help.Install)
		err := s.InstallCLI(ctx, a.Out, a.Out)
		if err == nil {
			if engine, _ = a.probeEngine(); engine == engineMissing {
				err = fmt.Errorf("官方安装脚本跑完了，但还是找不到 %s", help.Command)
			}
		}
		if err != nil {
			fmt.Fprintf(a.Out, "%v\n", err)
			fmt.Fprintf(a.Out, "%s 没装上，什么都没改，后台没开。网络不通可以用代理或换网络重试，也可以换一种官方方式：\n", help.Name)
			for _, alt := range help.Alternatives {
				fmt.Fprintf(a.Out, "  %s\n", alt)
			}
			fmt.Fprintf(a.Out, "装好后运行：\n  %s\n  agentq start\n", help.Login)
			return fmt.Errorf("%s 没装上", help.Name)
		}
	}
	if engine == engineLoggedOut && canSetUp {
		_, path := a.probeEngine()
		if err := s.LoginCLI(ctx, path, o.In, a.Out, a.Out); err != nil {
			fmt.Fprintf(a.Out, "%v\n", err)
		}
		if engine, _ = a.probeEngine(); engine != engineReady {
			return fmt.Errorf("%s 还没登录，后台没开。登录后运行 agentq start：\n  %s\n  agentq start", help.Name, help.Login)
		}
	}
	if engine == engineReady {
		_, path := a.probeEngine()
		return a.adoptCLIPath(path)
	}
	return nil
}

// adoptCLIPath：配置还是默认的 claude 时记下真实路径，服务的 PATH 不一定找得到它。和新装时 setup 写的一样。
func (a *Service) adoptCLIPath(path string) error {
	if path == "" || !a.usesDefaultCLIPath() {
		return nil
	}
	if err := config.SetKey(a.ConfigPath(), "claude_path", strconv.Quote(path), func() error {
		return fmt.Errorf("%s 不见了", a.ConfigPath())
	}); err != nil {
		return fmt.Errorf("记下 CLI 路径失败，后台没开：%w", err)
	}
	a.Cfg.ClaudePath = path
	return nil
}

// ensurePath：~/.local/bin 不在 PATH 时加带标记的一行。改不了不算失败，给手动的那一行。
func (a *Service) ensurePath(getenv func(string) string) {
	if onPath(a.binDir(), getenv("PATH")) {
		return
	}
	change, err := setup.AddPathLine(a.Home, getenv("SHELL"), getenv("ZDOTDIR"), a.Now())
	switch {
	case err != nil:
		fmt.Fprintf(a.Out, "没能改 shell 配置文件：%v\n", err)
		a.pathHint(getenv)
	case change.Changed && change.Backup != "":
		fmt.Fprintf(a.Out, "%s 把 ~/.local/bin 加进了 %s（原文件备份在 %s），新开的终端生效。\n", checkMark(getenv), a.tilde(change.RC), a.tilde(change.Backup))
	case change.Changed:
		fmt.Fprintf(a.Out, "%s 建了 %s，把 ~/.local/bin 放进 PATH，新开的终端生效。\n", checkMark(getenv), a.tilde(change.RC))
	case change.Reason != "":
		fmt.Fprintf(a.Out, "没有改 shell 配置文件：%s。\n", change.Reason)
		a.pathHint(getenv)
	default:
		fmt.Fprintf(a.Out, "%s 里已经把 ~/.local/bin 放进 PATH，新开的终端生效。还是找不到 agentq 就用全路径 ~/.local/bin/agentq。\n", a.tilde(change.RC))
	}
}

// recordConsent 记下同意。已有记录不覆盖：那是用户第一次同意时的文字版本。
func (a *Service) recordConsent(method string) error {
	if c, err := setup.ReadConsent(a.Home); err == nil && c != nil {
		return nil
	}
	return setup.WriteConsent(a.Home, setup.Consent{TextVersion: setup.ConsentTextVersion, Time: a.Now().UTC().Format(time.RFC3339), Method: method})
}

// installOnly 说明为什么没启动、之后怎么启动。
func (a *Service) installOnly(why string, engine engineState, getenv func(string) string) {
	fmt.Fprintln(a.Out, why)
	a.printCLISteps(engine)
	fmt.Fprintln(a.Out, "准备好了运行这一条（会讲清权限，由你确认）：")
	fmt.Fprintln(a.Out, "  agentq start")
	a.pathHint(getenv)
}

// printCLISteps 给 CLI 还差的那几条官方命令。
func (a *Service) printCLISteps(engine engineState) {
	help := a.cliHelp()
	switch engine {
	case engineMissing:
		fmt.Fprintf(a.Out, "还差官方 %s（后台任务靠它运行，桌面端不能代替）。在终端运行：\n  %s\n  %s\n", help.Name, help.Install, help.Login)
	case engineLoggedOut:
		fmt.Fprintf(a.Out, "%s 还没登录。在终端运行：\n  %s\n", help.Name, help.Login)
	}
}

// pathHint：~/.local/bin 不在 PATH 时给要加的那一行。
func (a *Service) pathHint(getenv func(string) string) {
	if onPath(a.binDir(), getenv("PATH")) {
		return
	}
	fmt.Fprintln(a.Out, "~/.local/bin 不在 PATH 里。把这一行加到 shell 配置文件（zsh 是 ~/.zshrc，bash 是 ~/.bashrc）后重开终端：")
	fmt.Fprintln(a.Out, `  export PATH="$HOME/.local/bin:$PATH"`)
}

func (a *Service) binDir() string { return filepath.Join(a.Home, ".local", "bin") }

// tilde 把家目录写成 ~，读起来短。
func (a *Service) tilde(path string) string {
	if rest, ok := strings.CutPrefix(path, a.Home+string(os.PathSeparator)); ok {
		return "~/" + rest
	}
	return path
}

func onPath(dir, path string) bool {
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return true
		}
	}
	return false
}

// hasWork：有在跑的任务或可接的待做。读不了就当有，交给 StartReport 如实说。
func (a *Service) hasWork() bool {
	st, err := state.Load(a.StatePath())
	if err != nil || len(st.Tasks()) > 0 {
		return true
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil && !errors.Is(err, os.ErrNotExist) { // 目录不见了就看 extra_projects 还有没有活
		return true
	}
	for _, p := range projects {
		items, err := p.Items()
		if err != nil {
			return true
		}
		for _, it := range items {
			if it.Eligible() {
				return true
			}
		}
	}
	return false
}

// checkMark 是步骤行前面的勾，和 install.sh 一样：终端里才上色，遵守 NO_COLOR。
func checkMark(getenv func(string) string) string {
	if getenv("NO_COLOR") != "" || !IsTerminal(os.Stdout) {
		return "✓"
	}
	return "\033[32m✓\033[0m"
}
