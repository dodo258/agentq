// Package codex 是 Codex CLI 的后端。参数、事件流格式、会话文件位置都只写在这里。
//
// 依据：
//   - 非交互和事件：https://learn.chatgpt.com/docs/non-interactive-mode
//   - 事件结构：https://github.com/openai/codex/blob/main/codex-rs/exec/src/exec_events.rs
//   - 错误文字：https://github.com/openai/codex/blob/main/codex-rs/protocol/src/error.rs
//   - 沙箱：https://learn.chatgpt.com/docs/agent-approvals-security
//   - 技能：https://learn.chatgpt.com/docs/build-skills
//   - 用户级说明：https://learn.chatgpt.com/docs/agent-configuration/agents-md
//
// 文档没写清的按 codex-cli 0.166.0 实测（docs/backends.md 有记录）。
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
	"agentq/internal/proc"
)

// Name 是这个后端的注册名。
const Name = "codex"

func init() { backend.Register(Name, New) }

// envKeep：ChatGPT 登录只要知道 CODEX_HOME。CODEX_API_KEY、OPENAI_API_KEY 不放行：放了任务就改走 API 计费。
var envKeep = []string{"CODEX_HOME"}

// 权限值就是 Codex 沙箱的名字，default 是不传，跟 Codex 自己的配置走（没配时 exec 是只读）。
const (
	permDefault = "default"
	permFull    = "danger-full-access"
	permWrite   = "workspace-write"
	permRead    = "read-only"
)

const (
	installCommand = "npm install -g @openai/codex"
	loginCommand   = "codex login"
)

// loginWait：codex login status 最多等多久。
const loginWait = 30 * time.Second

// Backend 是 Codex CLI 的实现。
type Backend struct{ o backend.Options }

// New 建实例。
func New(o backend.Options) backend.Backend { return &Backend{o: o} }

func (b *Backend) Name() string { return Name }

// Caps：有的都是实测过的。
//   - 没有 ToolProfiles：read-only 沙箱只禁写不限读，也没有「无工具」，读密钥挡不住，要它的任务不启动。
//   - 没有 NoExternalMCP：--ignore-user-config 同时丢掉整份用户配置，不算隔离；只有上面那些任务要它。
//   - 没有 Monitor：Codex 没有这个工具。没有 PresetSession：会话 id 从第一个事件取。
func (b *Backend) Caps() backend.Caps {
	return backend.Caps{Resume: true, Schema: true, Usage: true, LimitSignal: true, AuthProbe: true, Skills: true, ReviewProfile: true, ImageInput: true, NoArgvLabel: true}
}

func (b *Backend) EnvKeep() []string { return envKeep }

// PermissionModes 实现 backend.Permissions。
func (b *Backend) PermissionModes() []string {
	return []string{permDefault, permRead, permWrite, permFull}
}

// FullPermission：workspace-write 下 .git 只读、默认不联网，提交不了，无人值守要关沙箱。
func (b *Backend) FullPermission() string { return permFull }

// PermissionNote 实现 backend.Permissions。
func (b *Backend) PermissionNote() string {
	return "后端是 Codex：代码会发给 OpenAI。workspace-write 沙箱里 .git 只读，提交不了，所以无人值守要关掉操作系统沙箱（danger-full-access）。"
}

// SkillsDir：官方列的用户级位置是 $HOME/.agents/skills，没有 ~/.codex/skills。
func (b *Backend) SkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// RulesFile 实现 backend.RulesBlock：Codex 没有规则目录，用户级说明只有 $CODEX_HOME/AGENTS.md，AGENTS.override.md 存在时不读它。
func (b *Backend) RulesFile(home string) (string, string) {
	dir := codexHome(home)
	return filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, "AGENTS.override.md")
}

func codexHome(home string) string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return fsx.ExpandHome(home, v)
	}
	return filepath.Join(home, ".codex")
}

func (b *Backend) path() string {
	if b.o.Path != "" {
		return b.o.Path
	}
	return Name
}

// Build：提示词走标准输入。图片用 --image 附在第一条消息上（codex exec --help、官方命令表）。schema 要是文件，放 WorkDir。沙箱和审批都用 -c 传：exec resume 没有 -s。
func (b *Backend) Build(req backend.Request) (backend.Invocation, []backend.Degradation, error) {
	if err := backend.CheckMust(b, req); err != nil {
		return backend.Invocation{}, nil, err
	}
	args := []string{"exec"}
	if req.Resume != "" {
		args = append(args, "resume", req.Resume)
	}
	// 没人能批准，要批准的动作直接失败，不能等。--skip-git-repo-check：new 在 projects_root 里建项目，那里不是 Git 仓库；要不要跑、在哪跑 agentq 自己查过了
	args = append(args, "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`)
	if s := sandbox(req); s != "" {
		args = append(args, "-c", "sandbox_mode="+strconv.Quote(s))
	}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(req.Effort))
	}
	if len(req.AddDirs) > 0 {
		roots, err := json.Marshal(req.AddDirs)
		if err != nil {
			return backend.Invocation{}, nil, err
		}
		args = append(args, "-c", "sandbox_workspace_write.writable_roots="+string(roots)) // JSON 数组也是合法的 TOML；resume 没有 --add-dir
	}
	for _, img := range req.Images {
		if strings.Contains(img, ",") { // --image 的值按逗号分多个路径（官方命令表、codex exec --help），路径带逗号传不对
			return backend.Invocation{}, nil, fmt.Errorf("%w：图片路径带逗号，传不给 Codex：%s", backend.ErrUnsupported, img)
		}
		args = append(args, "--image="+img) // 带等号：这个参数能吃多个值，分开写会把后面的 - 也吃掉
	}
	if req.Schema != "" {
		path, err := writeSchema(req.Schema, req.WorkDir)
		if err != nil {
			return backend.Invocation{}, nil, err
		}
		args = append(args, "--output-schema", path)
	}
	args = append(args, "-")
	return backend.Invocation{Path: b.path(), Args: args, Record: append([]string(nil), args...), Dir: req.Dir, EnvKeep: envKeep, Stdin: req.Prompt}, backend.Degrade(b, req), nil
}

// sandbox：审查进程跑在一次性 worktree 里，workspace-write 正好：能写工作目录，碰不到 .git（实测提交不了），也碰不到别的地方。
// 其他按配置的权限，default 和空都不传。
func sandbox(req backend.Request) string {
	if req.Profile == backend.ProfileReview {
		return permWrite
	}
	if req.Permission == permDefault {
		return ""
	}
	return req.Permission
}

// schemaFile 是 --output-schema 读的文件，在任务目录里。
const schemaFile = "codex-output-schema.json"

// writeSchema：Codex 按严格模式把 schema 交给 API，每个对象都要 additionalProperties:false，properties 都要进 required，否则直接 400（实测）。
func writeSchema(schema, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("结构化输出要有落盘目录（Request.WorkDir）")
	}
	var v any
	if err := json.Unmarshal([]byte(schema), &v); err != nil {
		return "", fmt.Errorf("schema 不是合法的 JSON：%w", err)
	}
	b, err := json.Marshal(strictSchema(v))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, schemaFile)
	if err := fsx.WriteFileAtomic(path, b); err != nil {
		return "", fmt.Errorf("写 schema 文件失败：%w", err)
	}
	return path, nil
}

func strictSchema(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = strictSchema(x[i])
		}
	case map[string]any:
		for k, e := range x {
			x[k] = strictSchema(e)
		}
		if x["type"] == "object" {
			props, _ := x["properties"].(map[string]any)
			names := make([]any, 0, len(props))
			for _, n := range slices.Sorted(maps.Keys(props)) {
				names = append(names, n)
			}
			x["additionalProperties"] = false
			x["required"] = names
		}
	}
	return v
}

// Auth：codex login status 已登录退 0、没登录退 1 打 Not logged in（实测）。它只看本地有没有凭据，不验证（令牌失效时还是说已登录），所以算 Unverified。
func (b *Backend) Auth(ctx context.Context) backend.Auth {
	ctx, cancel := context.WithTimeout(ctx, loginWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.path(), "login", "status")
	cmd.Env = proc.BackendEnv(envKeep)
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.ToLower(string(out))
	switch {
	case strings.Contains(text, "not logged in"):
		return backend.Auth{State: backend.AuthOut, Detail: "Codex CLI 没登录（codex login status 显示 Not logged in），不启动；运行 " + loginCommand}
	case err == nil && strings.Contains(text, "logged in"):
		return backend.Auth{State: backend.AuthIn, Unverified: true}
	case err != nil:
		return backend.Auth{State: backend.AuthError, Detail: "确认 Codex CLI 登录状态失败，不启动：" + err.Error()}
	}
	return backend.Auth{State: backend.AuthError, Detail: "codex login status 的输出看不懂，不启动"}
}

// Probe 找能独立运行的 CLI（桌面端里带的不算），确认无头参数都在，再查认证。
func (b *Backend) Probe(ctx context.Context, env backend.ProbeEnv) (backend.Probe, error) {
	var r backend.Probe
	run := func(path string, args ...string) ([]byte, error) {
		return backend.ProbeCommand(ctx, env.Home, path, env.Env, args...)
	}
	name := Name
	if env.Explicit != "" {
		name = fsx.ExpandHome(env.Home, env.Explicit)
	}
	p, err := backend.ProbePath(env.Home, name, env.Env)
	if err == nil {
		p, err = filepath.Abs(p)
	}
	var resolved string
	if err == nil {
		resolved, err = filepath.EvalSymlinks(p)
	}
	if err != nil || strings.Contains(resolved, ".app/Contents/") {
		return r, fmt.Errorf("no independently callable Codex CLI; a desktop app is not an engine. Official install: %s; then %s: %w", installCommand, loginCommand, backend.ErrNoCLI)
	}
	out, err := run(p, "--version")
	if err != nil || !strings.Contains(string(out), "codex") {
		return r, fmt.Errorf("%s --version failed or is not the Codex CLI: %w", p, backend.ErrNoCLI)
	}
	r.Path, r.Version = p, strings.TrimSpace(string(out))
	out, err = run(p, "exec", "--help")
	if err != nil {
		return r, errors.New("Codex CLI exec --help failed")
	}
	text := string(out)
	r.Headless = strings.Contains(text, "--json") && strings.Contains(text, "--output-schema") && strings.Contains(text, "resume")
	if !r.Headless {
		return r, errors.New("CLI lacks required exec/JSON/schema/resume capability; update via its official installation owner")
	}
	out, err = loginStatus(ctx, env, p)
	text = string(out)
	if low := strings.ToLower(text); err == nil && strings.Contains(low, "logged in") && !strings.Contains(low, "not logged in") {
		r.Authenticated = true
		switch {
		case strings.Contains(text, "ChatGPT"):
			r.AuthMethod = "ChatGPT"
		case strings.Contains(strings.ToLower(text), "api key"):
			r.AuthMethod = "API key" // 只记种类：这一行可能带密钥的片段
		}
		return r, nil
	}
	return r, fmt.Errorf("CLI authentication unavailable in the background environment; run %s, then retry. No credentials were requested or changed: %w", loginCommand, backend.ErrNotLoggedIn)
}

// loginStatus：登录状态可能打在标准错误（不同版本不一样），两个都看。
func loginStatus(ctx context.Context, env backend.ProbeEnv, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, backend.ProbeWait)
	defer cancel()
	c := exec.CommandContext(ctx, path, "login", "status")
	c.Dir = env.Home
	c.Env = env.Env
	return c.CombinedOutput()
}

// CanResume：会话文件在 $CODEX_HOME/sessions/<年>/<月>/<日>/rollout-*-<id>.jsonl（实测，官方没承诺这个位置）。exec resume 找不到时报 no rollout found。
func (b *Backend) CanResume(session string) bool {
	if !safeSession(session) {
		return false
	}
	hits, err := filepath.Glob(filepath.Join(codexHome(b.o.Home), "sessions", "*", "*", "*", "rollout-*-"+session+".jsonl"))
	return err == nil && len(hits) > 0
}

func safeSession(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// ResumeCommand 实现 backend.ResumeHint：交互式接着看用 codex resume（官方命令表）。
func (b *Backend) ResumeCommand(session string) string { return "codex resume " + session }

func (b *Backend) RecordedModel(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-m" {
			return args[i+1]
		}
	}
	return ""
}
