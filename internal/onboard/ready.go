package onboard

import (
	"bytes"
	"fmt"
	"slices"

	"agentq/internal/config"
	"agentq/internal/setup"
)

// agentq start --check：派活技能在聊天里交任务前先问这一句，按第一行决定下一步，不用用户敲命令。只读。

// ReadyState 是 agentq start --check 第一行的状态词。
type ReadyState string

const (
	ReadyOK         ReadyState = "ready"       // 后台在跑，直接派活
	ReadyNotRunning ReadyState = "not-running" // 同意过、不是用户暂停的，服务没在跑（被卸、重启后没加载、装的时候 CLI 还没弄好）：agentq start 拉起，不用问
	ReadyPaused     ReadyState = "paused"      // 用户自己暂停的：不擅自打开；照常写待做，明确派的那条用 agentq run 做
	ReadyOff        ReadyState = "off"         // 没同意过（装的时候带了 --paused，或老安装从没开过）：写待做，用户说开才开
	ReadyNeedCLI    ReadyState = "need-cli"    // 官方 CLI 没装或没登录：只能用户在终端做
	ReadySkillsOnly ReadyState = "skills-only" // 只装了技能，没有后台程序
)

// cmdCheck 打印状态词、projects_root 和给人看的话。安装记录读不了就报错，不猜。
func (a *Service) cmdCheck(i *setup.Installer) error {
	st, text, err := a.readiness(i)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, st)
	fmt.Fprintf(a.Out, "projects_root: %s\n", a.ProjectsRoot())
	fmt.Fprint(a.Out, text)
	return nil
}

// readiness 按顺序看：程序、CLI、同意、暂停、服务。前一项不过就不看后一项。
func (a *Service) readiness(i *setup.Installer) (ReadyState, string, error) {
	r, err := i.Receipt()
	if err != nil {
		return "", "", fmt.Errorf("读安装记录失败，不知道后台的情况：%w", err)
	}
	if r.Scope != "full" {
		return ReadySkillsOnly, "只装了技能（--scope skills），没有后台程序。要后台接活：先 agentq remove --yes，再不带 --scope skills 重新安装。\n", nil
	}
	if engine, _ := a.probeEngine(); engine != engineReady {
		var b bytes.Buffer
		out := a.Out
		a.Out = &b
		a.printCLISteps(engine)
		a.Out = out
		if engine == engineBroken {
			b.WriteString("后台用的 CLI 跑不起来，运行 agentq doctor 看原因。\n")
		}
		b.WriteString("弄好之后再说一次要做什么，后台会自己开起来。\n")
		return ReadyNeedCLI, b.String(), nil
	}
	if !a.permissionGranted() {
		return ReadyOff, "后台没开：安装时选了先不跑（--paused），或者这是还没开过后台的老安装。用户说要开再运行 agentq start --accept-bypass。\n", nil
	}
	if a.Cfg.Paused {
		switch c, err := setup.ReadConsent(a.Home); {
		case slices.Contains(r.Services, a.Service.ServiceLabel()):
			return ReadyPaused, "后台被暂停了（agentq pause），待做不会自动开始；用户点名要做的可以 agentq run。恢复：agentq resume。\n", nil
		case err != nil || c == nil: // 装之前就暂停着的老配置：是用户的开关
			return ReadyOff, "后台没开：这台机器原来的配置是暂停的。用户说要开再运行 agentq start --accept-bypass。\n", nil
		}
		// 安装时同意了，但当时 CLI 没弄好，后台没开成
	}
	if a.Cfg.Paused || !a.servicesRunning(i, r) {
		return ReadyNotRunning, "后台没在跑（服务没加载或装的时候没开成）。运行 agentq start 拉起，不用问。\n", nil
	}
	return ReadyOK, "后台在跑，每 5 分钟看一次待做。\n", nil
}

// permissionGranted：权限已经放开到无人值守能干活，就是同意过。装完即运行和 agentq start 同意的最后一步都是放开权限；有同意记录之前的老配置没写这个键，默认就是完全权限。
func (a *Service) permissionGranted() bool {
	return a.Cfg.PermissionMode == a.fullPermission() || a.Cfg.PermissionMode == "" && !setup.NeedsConsent(a.Cfg)
}

// servicesRunning：调度和夜班、复盘三个服务都装着、也加载着。Telegram 监听是可选的，不算在里面。
func (a *Service) servicesRunning(i *setup.Installer, r setup.InstallReceipt) bool {
	for _, label := range []string{a.Service.ServiceLabel(), a.Service.RoutineLabel(config.KindNight), a.Service.RoutineLabel(config.KindReview)} {
		if !slices.Contains(r.Services, label) || !i.Loaded(label) {
			return false
		}
	}
	return true
}
