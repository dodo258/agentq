package onboard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agentq/internal/envlayer"
	"agentq/internal/setup"
)

func (a *Service) envLayer() *envlayer.Layer {
	return envlayer.New(a.Home, a.Assets, a.DefaultBackend())
}

// envFirstTime：这是不是第一次同意。已经同意过的（升级、重复 start）不自动加规则；卸载时清掉、后来重装的算第一次。在记下同意之前问。
func (a *Service) envFirstTime() bool {
	s, err := a.envLayer().Status()
	if err != nil || !s.Supported {
		return false
	}
	if s.State == envlayer.StateRemoved {
		return true
	}
	c, err := setup.ReadConsent(a.Home)
	return err == nil && c == nil && s.State == envlayer.StateNone
}

// envConsentLine 是同意文字里关于规则的那一行，不是第一次就没有。
func (a *Service) envConsentLine() string {
	if !a.envFirstTime() {
		return ""
	}
	s, err := a.envLayer().Status()
	if err != nil {
		return ""
	}
	if !s.ShouldAutoEnable() {
		return "检测到你已有全局规则，保留你的，不加 agentq 的。要加：agentq env on"
	}
	return fmt.Sprintf("会把一份通用工程规则放到 %s，这台机器上所有项目的会话都会读到；项目规则和你自己的规则优先。不想要：agentq env off", a.ruleWhere(s.Dir))
}

// autoEnableEnv 第一次同意之后装规则：有用户自己的规则就不装。装不上不挡启动，说一句。
func (a *Service) autoEnableEnv() {
	l := a.envLayer()
	s, err := l.Status()
	if err != nil {
		fmt.Fprintf(a.Out, "通用工程规则没装（后台照常）：%v。之后可以运行 agentq env on\n", err)
		return
	}
	if !s.ShouldAutoEnable() {
		return
	}
	rep, err := l.Enable()
	if err != nil {
		fmt.Fprintf(a.Out, "通用工程规则没装上（后台照常）：%v。之后可以运行 agentq env on\n", err)
		return
	}
	fmt.Fprintf(a.Out, "通用工程规则放到了 %s。不想要：agentq env off\n", a.ruleWhere(rep.Dir))
	a.printKept(rep)
}

// ruleWhere：规则放在哪。目录带斜杠；块方式是一个文件，写清是接在里面的一段。
func (a *Service) ruleWhere(p string) string {
	if strings.HasSuffix(p, ".md") {
		return a.tilde(p) + " 末尾（带标记的一段，别的内容不动）"
	}
	return a.tilde(p) + "/"
}

// CmdEnv 是 agentq env status|on|off。
func (a *Service) CmdEnv(args []string) error {
	if len(args) != 1 {
		return errors.New("用法：agentq env status|on|off")
	}
	l := a.envLayer()
	switch args[0] {
	case "status":
		return a.envStatus(l)
	case "on":
		rep, err := l.Enable()
		if err != nil {
			return err
		}
		switch {
		case len(rep.Written)+len(rep.Updated) > 0:
			fmt.Fprintf(a.Out, "通用工程规则已装到 %s：%s\n", a.tilde(rep.Dir), strings.Join(append(rep.Written, rep.Updated...), "、"))
		case len(rep.Kept) == 0:
			fmt.Fprintf(a.Out, "通用工程规则已经装在 %s，没有变化。\n", a.tilde(rep.Dir))
		}
		a.printKept(rep)
		return nil
	case "off":
		rep, err := l.Disable()
		if err != nil {
			return err
		}
		if len(rep.Removed) > 0 {
			fmt.Fprintf(a.Out, "通用工程规则已删：%s。之后升级不会加回来，要加：agentq env on\n", strings.Join(rep.Removed, "、"))
		} else if len(rep.Kept) == 0 {
			fmt.Fprintln(a.Out, "通用工程规则本来就没装。之后升级也不会加，要加：agentq env on")
		}
		a.printKept(rep)
		return nil
	}
	return fmt.Errorf("agentq env 不认识 %s；用法：agentq env status|on|off", args[0])
}

func (a *Service) envStatus(l *envlayer.Layer) error {
	s, err := l.Status()
	if err != nil {
		return err
	}
	if !s.Supported {
		fmt.Fprintf(a.Out, "后端 %s 没有用户级规则目录，环境层不装。\n", a.DefaultBackend().Name())
	}
	switch s.State {
	case envlayer.StateOn:
		fmt.Fprintf(a.Out, "通用工程规则：开着（%s）\n", a.tilde(s.Dir))
	case envlayer.StateOff:
		fmt.Fprintln(a.Out, "通用工程规则：关着，升级不加回来。要加：agentq env on")
	case envlayer.StateRemoved:
		fmt.Fprintln(a.Out, "通用工程规则：卸载时清掉了。agentq restore 会装回来，也可以 agentq env on")
	default:
		if s.Supported {
			fmt.Fprintln(a.Out, "通用工程规则：没装。要加：agentq env on")
		}
	}
	for _, f := range s.Files {
		switch f.State {
		case envlayer.FileIntact:
			fmt.Fprintf(a.Out, "  %s：和 agentq 写的一样\n", f.Name)
		case envlayer.FileModified:
			fmt.Fprintf(a.Out, "  %s：内容和 agentq 写的不一样，升级和关闭都不动它\n", f.Name)
		case envlayer.FileMissing, envlayer.FileDeclined:
			fmt.Fprintf(a.Out, "  %s：你删了，升级不写回（agentq env on 会补上）\n", f.Name)
		}
	}
	if len(s.UserRules) > 0 {
		var mine []string
		for _, p := range s.UserRules {
			mine = append(mine, a.tilde(p))
		}
		fmt.Fprintf(a.Out, "你自己的全局规则：%s\n", strings.Join(mine, "、"))
	}
	a.printShadow(s.Shadow)
	return nil
}

func (a *Service) printShadow(shadow string) {
	if shadow != "" {
		fmt.Fprintf(a.Out, "注意：%s 存在，后端读它不读放规则的文件，规则现在不生效。\n", a.tilde(shadow))
	}
}

func (a *Service) printKept(rep envlayer.Report) {
	for _, n := range rep.Kept {
		where := filepath.Join(rep.Dir, n)
		if strings.HasSuffix(rep.Dir, ".md") { // 块方式：改过的是文件里的那一段
			where = rep.Dir
		}
		fmt.Fprintf(a.Out, "%s 你改过，留着没动：%s\n", n, a.tilde(where))
	}
	a.printShadow(rep.Shadow)
}
