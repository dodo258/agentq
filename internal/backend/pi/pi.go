// Package pi 是 pi（npm 包 @earendil-works/pi-coding-agent）的后端。参数、事件流格式、会话文件位置都只写在这里。
//
// 依据是 pi 1.5.1 包里自带的官方文档（npm 包的 docs/ 目录，源码仓库 github.com/earendil-works/pi 的 packages/coding-agent/docs）：
//   - 参数：cli.md（-p、--mode json、--session-id、--session、--model、--thinking、--tools、--no-approve、--name、@文件）
//   - 无头集成：cli-integration.md（JSON 模式失败和中止只在事件里，退出码可能是 0）
//   - 事件：json.md（第一行 session 头，agent_settled 是终态）；message-types.md（stopReason、errorMessage、usage）
//   - 会话：sessions.md（默认在 <配置目录>/sessions，按工作目录分子目录）
//   - 项目信任：security.md（-p 和 JSON 模式不弹确认：没保存过决定就按 defaultProjectTrust，默认跳过）
//   - 配置目录和用户级说明：configuration.md、environment-variables.md（PI_CODING_AGENT_DIR、PI_CODING_AGENT_SESSION_DIR）
//   - 技能：skills.md（~/.agents/skills 和 <配置目录>/skills）
//   - 凭据：cli.md 的 pi auth check（ready、not_ready、invalid，退出码 0、1、2）
//
// 文档没写清的按 pi 1.5.1 实测（macOS，docs/backends.md「pi 的实际情况」有记录）。
package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
	"agentq/internal/proc"
)

// Name 是这个后端的注册名。
const Name = "pi"

func init() { backend.Register(Name, New) }

// envKeep：配置目录、会话目录、包目录（Nix 这类装法要它）。各模型来源的 API Key 不放行：放了就可能换收费入口，凭据走 pi 自己存的 auth.json、models.json。
var envKeep = []string{"PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "PI_PACKAGE_DIR"}

// 权限值：pi 没有权限模式，也没有沙箱，工具一律以本机用户身份直接运行。这两个值只记用户有没有确认过这一点，不变成参数。
const (
	permDefault = "default"
	permFull    = "full"
)

const (
	installCommand = "npm install -g @earendil-works/pi-coding-agent"
	loginCommand   = "pi auth login <提供商>"
)

// authWait：pi auth check 最多等多久（OAuth 凭据过期时它会联网刷新）。
const authWait = 30 * time.Second

// Backend 是 pi 的实现。
type Backend struct{ o backend.Options }

// New 建实例。
func New(o backend.Options) backend.Backend { return &Backend{o: o} }

func (b *Backend) Name() string { return Name }

// Caps：有的都是实测过的。
//   - 没有 ToolProfiles：--tools read,grep,find,ls 和 --no-tools 确实只把这些工具交给模型，但 read 能读工作目录外的任何文件（实测），读密钥挡不住，要它的任务不启动。
//   - 没有 NoExternalMCP：要它的任务（追问、大方向商量、巡逻分析）同时要 ToolProfiles，pi 给不了，声明了也用不上；--no-mcp 加 --no-extensions 能不能完全隔离没实测。
//   - 没有 Schema：没有强制结构化输出的参数，审查门靠提示词要 JSON，见 SchemaByPrompt。
//   - 没有 HardBudget、MaxTurns：没有这两个参数。运行中读得到用量和金额（UsageStream），tick 替它看预算。
//   - NoArgvLabel：pi 启动就把进程名改成 pi（process.title），macOS 上 ps 连命令行带环境都读不到（实测），靠任务外壳认进程。
func (b *Backend) Caps() backend.Caps {
	return backend.Caps{Resume: true, PresetSession: true, SchemaByPrompt: true, Usage: true, Cost: true, LimitSignal: true, AuthProbe: true, Skills: true, ReviewProfile: true, ImageInput: true, NoArgvLabel: true}
}

func (b *Backend) EnvKeep() []string { return envKeep }

// PermissionModes 实现 backend.Permissions。
func (b *Backend) PermissionModes() []string { return []string{permDefault, permFull} }

// FullPermission：pi 本来就不问，full 只表示用户在 agentq start 时确认过。
func (b *Backend) FullPermission() string { return permFull }

// PermissionNote 实现 backend.Permissions。
func (b *Backend) PermissionNote() string {
	return "后端是 pi：代码会发给你在 pi 里配的模型来源（settings.json 的 defaultProvider，或 backends.pi.model）。pi 没有权限确认，也没有沙箱，读写文件、跑命令都直接以你这个用户的身份进行；agentq 加不了限制，这里同意的就是这一点。"
}

// SkillsDir：pi 读 ~/.agents/skills（skills.md），和 Codex 共用一份。
func (b *Backend) SkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// contextFiles 是配置目录里用户级说明的候选，pi 只读第一个存在的（resource-loader.js 的 loadContextFileFromDir）。
var contextFiles = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// RulesFile 实现 backend.RulesBlock：块放进 pi 会读的那个文件。用户已经有 AGENTS.md 或 CLAUDE.md 就放进去，都没有才建 AGENTS.md：
// 新建的 AGENTS.md 排在 CLAUDE.md 前面，会让用户原来的 CLAUDE.md 不再被读。AGENTS.override.md 存在时别的都不读。
func (b *Backend) RulesFile(home string) (string, string) {
	dir := agentDir(home)
	for _, n := range contextFiles[1:] {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, filepath.Join(dir, contextFiles[0])
		}
	}
	return filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, contextFiles[0])
}

// IdentityLinks 实现 backend.IdentityLinker：凭据（auth.json、models-store.json）、自定义模型来源（models.json）、默认模型（settings.json）。
// pi 写这几个文件都是原地 writeFileSync（auth-storage.js、settings-manager.js），不是替换，链接不会被换掉。
func (b *Backend) IdentityLinks() []string {
	return []string{".pi/agent/auth.json", ".pi/agent/models-store.json", ".pi/agent/models.json", ".pi/agent/settings.json"}
}

// ResumeCommand 实现 backend.ResumeHint：--session 按 id 找会话，在它原来的工作目录接着跑（cli.md）。
func (b *Backend) ResumeCommand(session string) string { return "pi --session " + session }

// agentDir 是 pi 的配置目录：PI_CODING_AGENT_DIR，默认 ~/.pi/agent。
func agentDir(home string) string {
	return agentDirIn(home, os.Getenv("PI_CODING_AGENT_DIR"))
}

func agentDirIn(home, env string) string {
	if env != "" {
		return fsx.ExpandHome(home, env)
	}
	return filepath.Join(home, ".pi", "agent")
}

func (b *Backend) path() string {
	if b.o.Path != "" {
		return b.o.Path
	}
	return Name
}

// Build：提示词走标准输入（实测没有消息参数时标准输入就是提示词，不进 ps，也没有参数长度上限）。
//   - 新会话 --session-id 指定 id（不存在就建），续跑 --session <id>（实测 SIGTERM 之后接得上）。
//   - 图片用 @路径 附在这次的第一条消息上（实测，路径带逗号也行）。
//   - 结构化输出提示词里要（runner 已经加过的不再加）。
//   - 项目信任：任务跟随用户自己在 pi 里保存的决定（没保存就跳过 .pi 里的扩展和设置，security.md）；审查进程一律 --no-approve：
//     候选分支上的 .pi 是任务写的，信任了就会在审查进程里跑任务写的扩展。
//   - 权限值不变成参数：pi 没有这个开关。
func (b *Backend) Build(req backend.Request) (backend.Invocation, []backend.Degradation, error) {
	if err := backend.CheckMust(b, req); err != nil {
		return backend.Invocation{}, nil, err
	}
	args := []string{"-p", "--mode", "json"}
	switch {
	case req.Resume != "":
		args = append(args, "--session", req.Resume)
	case req.Session != "":
		args = append(args, "--session-id", req.Session)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--thinking", req.Effort) // 取值和 agentq 的一样（low 到 max），模型做不到的 pi 自己往下压
	}
	if req.Profile == backend.ProfileReview {
		args = append(args, "--no-approve")
	}
	if req.Label != "" {
		args = append(args, "--name", req.Label) // 只是会话的显示名，pi -r 里认得出；认进程靠不了它，见 Caps
	}
	for _, img := range req.Images {
		args = append(args, "@"+img)
	}
	prompt := req.Prompt
	if req.Schema != "" && !strings.HasSuffix(prompt, backend.SchemaPrompt(req.Schema)) {
		prompt += backend.SchemaPrompt(req.Schema)
	}
	return backend.Invocation{Path: b.path(), Args: args, Record: append([]string(nil), args...), Dir: req.Dir, EnvKeep: envKeep, Stdin: prompt}, backend.Degrade(b, req), nil
}

// authResult 是 pi auth check --json 的输出。
type authResult struct {
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
}

// defaultProvider 读用户级 settings.json 的 defaultProvider。读不到就是空：这台机器还没选过模型来源。
func defaultProvider(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		DefaultProvider string `json:"defaultProvider"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.DefaultProvider
}

// checkAuth 跑 pi auth check。ready 退 0、not_ready 退 1、invalid 退 2（cli.md），输出都是一个 JSON 对象。
func checkAuth(ctx context.Context, path, dir string, env []string, provider string) (authResult, error) {
	ctx, cancel := context.WithTimeout(ctx, authWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "check", "--provider", provider, "--json")
	cmd.Dir, cmd.Env = dir, env
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.Output()
	var r authResult
	if jerr := json.Unmarshal(out, &r); jerr != nil || r.Status == "" {
		if err == nil {
			err = fmt.Errorf("pi auth check 的输出看不懂：%s", strings.TrimSpace(string(out)))
		}
		return r, err
	}
	return r, nil
}

// Auth：看默认模型来源的凭据在不在。只说明本地配了（API key 不验证），算 Unverified；没选过模型来源就查不了，照常启动。
// backends.pi.model 换了别家来源时查的还是默认那家：实例里没有配置，401 由 Parse 认出来。
func (b *Backend) Auth(ctx context.Context) backend.Auth {
	provider := defaultProvider(agentDir(b.o.Home))
	if provider == "" {
		return backend.Auth{State: backend.AuthUnknown}
	}
	r, err := checkAuth(ctx, b.path(), "", proc.BackendEnv(envKeep), provider)
	switch {
	case err != nil:
		return backend.Auth{State: backend.AuthError, Detail: "确认 pi 的登录状态失败，不启动：" + err.Error()}
	case r.Status == "ready":
		return backend.Auth{State: backend.AuthIn, Unverified: true}
	}
	return backend.Auth{State: backend.AuthOut, Detail: fmt.Sprintf("pi 的模型来源 %s 没有可用的凭据（pi auth check：%s %s），不启动；运行 pi auth login %s", provider, r.Status, r.Reason, provider)}
}

// versionRe：pi --version 只打版本号。
var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// Probe 找 CLI，看 --help 里有没有要用的参数，再查默认模型来源的凭据。
func (b *Backend) Probe(ctx context.Context, env backend.ProbeEnv) (backend.Probe, error) {
	var r backend.Probe
	name := Name
	if env.Explicit != "" {
		name = fsx.ExpandHome(env.Home, env.Explicit)
	}
	p, err := backend.ProbePath(env.Home, name, env.Env)
	if err == nil {
		p, err = filepath.Abs(p)
	}
	if err != nil {
		return r, fmt.Errorf("no pi CLI found. Official install: %s; then pick a model provider with %s: %w", installCommand, loginCommand, backend.ErrNoCLI)
	}
	out, err := backend.ProbeCommand(ctx, env.Home, p, env.Env, "--version")
	v := strings.TrimSpace(string(out))
	if err != nil || !versionRe.MatchString(v) {
		return r, fmt.Errorf("%s --version failed or is not pi (it needs node on PATH): %w", p, backend.ErrNoCLI)
	}
	r.Path, r.Version = p, "pi "+v
	out, err = backend.ProbeCommand(ctx, env.Home, p, env.Env, "--help")
	text := string(out)
	r.Headless = err == nil && strings.Contains(text, "--mode") && strings.Contains(text, "--session-id") && strings.Contains(text, "--no-approve")
	if !r.Headless {
		return r, fmt.Errorf("pi lacks --mode json, --session-id or --no-approve; update it with %s", installCommand)
	}
	dir := agentDirIn(env.Home, envValue(env.Env, "PI_CODING_AGENT_DIR"))
	provider := defaultProvider(dir)
	if provider == "" {
		return r, fmt.Errorf("pi has no default model provider (defaultProvider in %s); run pi once and pick a model, or %s: %w", filepath.Join(dir, "settings.json"), loginCommand, backend.ErrNotLoggedIn)
	}
	a, err := checkAuth(ctx, p, env.Home, env.Env, provider)
	if err == nil && a.Status == "ready" {
		r.Authenticated, r.AuthMethod = true, provider
		return r, nil
	}
	return r, fmt.Errorf("pi provider %s has no usable credentials in the background environment; run pi auth login %s, then retry. No credentials were requested or changed: %w", provider, provider, backend.ErrNotLoggedIn)
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// CanResume：会话文件叫 <时间>_<id>.jsonl（实测）。默认在 <配置目录>/sessions/<工作目录编码>/ 下，PI_CODING_AGENT_SESSION_DIR 指定时直接在那个目录。
// settings.json 的 sessionDir 不看：找不到只是不走回复链，不影响任务续跑。
func (b *Backend) CanResume(session string) bool {
	if !safeSession(session) {
		return false
	}
	dir := filepath.Join(agentDir(b.o.Home), "sessions")
	if v := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); v != "" {
		dir = fsx.ExpandHome(b.o.Home, v)
	}
	for _, pat := range []string{"*_" + session + ".jsonl", filepath.Join("*", "*_"+session+".jsonl")} {
		if hits, err := filepath.Glob(filepath.Join(dir, pat)); err == nil && len(hits) > 0 {
			return true
		}
	}
	return false
}

// safeSession：会话 id 只能有字母、数字、.、_、-，首尾是字母或数字（cli.md）。拿来拼 glob，别的字符都不收。
func safeSession(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func (b *Backend) RecordedModel(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" {
			return args[i+1]
		}
	}
	return ""
}
