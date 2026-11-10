package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/proc"
)

// 官方安装和登录命令，来源 https://code.claude.com/docs/en/setup 和 cli-reference。文档、脚本、提示都用这几条。
const (
	cliInstallURL     = "https://claude.ai/install.sh"
	cliInstallCommand = "curl -fsSL " + cliInstallURL + " | bash"
	cliBrewCommand    = "brew install --cask claude-code"
	cliNpmCommand     = "npm install -g @anthropic-ai/claude-code"
	cliLoginCommand   = "claude auth login"
)

// fixedCLIPaths 是 PATH 和 ~/.local/bin 之外还看的位置。
var fixedCLIPaths = []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude"}

// CLIHelp 实现 backend.CLISetup。
func (b *Backend) CLIHelp() backend.CLIHelp {
	return backend.CLIHelp{Name: "Claude CLI", Command: "claude", Install: cliInstallCommand, Alternatives: []string{cliBrewCommand, cliNpmCommand}, Login: cliLoginCommand, Desktop: "Claude 桌面端"}
}

// IdentityLinks 实现 backend.IdentityLinker：macOS 上订阅登录在钥匙串里，CLI 按 HOME 找钥匙串目录。
// ~/.claude/.credentials.json 不算：CLI 刷新登录时原子替换它，换掉链接后真实家目录那份就失效了。Linux 上的订阅登录所以还不支持。
func (b *Backend) IdentityLinks() []string { return []string{"Library/Keychains"} }

// RulesPaths 实现 backend.GlobalRules：~/.claude/rules 对本机每个项目生效，.md 递归发现，支持 paths 按文件类型加载（https://code.claude.com/docs/en/memory）。
func (b *Backend) RulesPaths(home string) (string, []string) {
	return filepath.Join(home, ".claude", "rules"), []string{filepath.Join(home, ".claude", "CLAUDE.md")}
}

// PermissionModes 实现 backend.Permissions。
func (b *Backend) PermissionModes() []string {
	return []string{"default", "manual", "acceptEdits", "auto", "dontAsk", "plan", "bypassPermissions"}
}

// FullPermission 实现 backend.Permissions。
func (b *Backend) FullPermission() string { return "bypassPermissions" }

// PermissionNote 实现 backend.Permissions。同意文字里已经讲清了 Claude 的情况，不用多说。
func (b *Backend) PermissionNote() string { return "" }

// SkillsDir 实现 backend.SkillHome。
func (b *Backend) SkillsDir(home string) string { return filepath.Join(home, ".claude", "skills") }

// DesktopInstalled：有没有 Claude 桌面端。只用来把话说清楚，它私有目录里的程序路径未公开，从不调用。
func (b *Backend) DesktopInstalled(home string) bool {
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		if st, e := os.Stat(filepath.Join(dir, "Claude.app")); e == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// InstallCLI 运行官方安装命令。和文档的 curl | bash 等价，只是先把脚本下完：管道里 curl 失败时 bash 收到空输入照样退出 0，那样看不出失败。
func (b *Backend) InstallCLI(ctx context.Context, stdout, stderr io.Writer) error {
	download, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var failure bytes.Buffer
	curl := exec.CommandContext(download, "curl", "-fsSL", "--connect-timeout", "15", "--max-time", "170", cliInstallURL)
	curl.Stderr = &failure
	script, e := curl.Output()
	if e != nil {
		return fmt.Errorf("下载官方安装脚本失败：%w\n%s", e, strings.TrimSpace(failure.String()))
	}
	if len(bytes.TrimSpace(script)) == 0 {
		return errors.New("官方安装脚本是空的")
	}
	install, cancelInstall := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelInstall()
	bash := exec.CommandContext(install, "bash")
	bash.Stdin = bytes.NewReader(script)
	bash.Stdout = stdout
	bash.Stderr = stderr
	if e := bash.Run(); e != nil {
		return fmt.Errorf("官方安装脚本没有跑完：%w", e)
	}
	return nil
}

// loginEnv 是登录要的环境：白名单之外只多打开浏览器要的几个变量。不带 API key，登录的就是后台任务要用的那个身份。
func loginEnv() []string {
	env := proc.ChildEnv()
	for _, name := range []string{"TERM", "DISPLAY", "WAYLAND_DISPLAY", "BROWSER", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// LoginCLI 在当前终端运行 claude auth login，它会打开浏览器。登没登上由调用方再探测一次，这里只报错。
func (b *Backend) LoginCLI(ctx context.Context, path string, in io.Reader, stdout, stderr io.Writer) error {
	login, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(login, path, "auth", "login")
	cmd.Env = loginEnv()
	cmd.Stdin = in
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if e := cmd.Run(); e != nil {
		return fmt.Errorf("%s 没有成功：%w", cliLoginCommand, e)
	}
	return nil
}
