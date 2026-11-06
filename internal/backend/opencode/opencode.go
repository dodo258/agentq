// Package opencode 是 opencode CLI 的后端。参数、事件流格式只写在这里。
//
// 真实的 opencode 没跑过：下面都是按 v1.19.5（2026-10-28）的官方文档和源码写的，用假 CLI 测，矩阵里标「实验」。
// 依据：
//   - 命令和环境变量：https://opencode.ai/docs/cli/
//   - run 的参数和事件：https://github.com/anomalyco/opencode/blob/v1.19.5/packages/opencode/src/cli/cmd/run.ts
//   - step_finish、错误的结构：https://github.com/anomalyco/opencode/blob/v1.19.5/packages/schema/src/v1/session.ts
//   - 什么 finish 算结束（tool-calls、unknown 不算）：https://github.com/anomalyco/opencode/blob/v1.19.5/packages/opencode/src/session/prompt.ts
//   - 权限（--auto 只放开 ask，明确的 deny 还生效）：https://opencode.ai/docs/permissions/
//   - 技能目录：https://opencode.ai/docs/skills/
//   - 用户级说明：https://opencode.ai/docs/rules/
//   - 配置目录（xdg-basedir）：https://github.com/anomalyco/opencode/blob/v1.19.5/packages/core/src/global.ts
package opencode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

// Name 是这个后端的注册名。
const Name = "opencode"

func init() { backend.Register(Name, New) }

// envKeep：配置和数据位置。各模型来源的 API Key 不放行：放了就可能换收费入口，走 opencode auth login 存的凭据。
// XDG_* 不放行：外壳和 git 都走全集，放了 git 就改读 $XDG_CONFIG_HOME/git/config，老用户的行为会变。所以后台任务里 opencode 用默认目录。
var envKeep = []string{"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_DB"}

// 权限值：default 是不传，要批准的动作（工作目录外、.env、doom_loop）run 会自动拒绝；auto 是 --auto。
const (
	permDefault = "default"
	permAuto    = "auto"
)

const installCommand = "npm install -g opencode-ai"

// Backend 是 opencode CLI 的实现。
type Backend struct{ o backend.Options }

// New 建实例。
func New(o backend.Options) backend.Backend { return &Backend{o: o} }

func (b *Backend) Name() string { return Name }

// Caps：只声明文档和源码写明、又不是安全边界的。
//   - 没有 ToolProfiles、DenyRules、NoExternalMCP：权限规则确实是 CLI 强制的（agent 的 permission、--agent），但没真实跑过，读边界和 MCP 隔离都没验证，这些是安全边界，不能靠文档开。要它们的任务不启动，真实验收过再开。
//   - 没有 Schema：run 没有这个参数。审查门靠提示词要 JSON，校验不过判不了，见 SchemaByPrompt。
//   - 没有 AuthProbe：auth list 只列存下的凭据，配置和环境里的模型来源看不到，本地模型不用登录，空的不等于没登录。
//   - NoArgvLabel：npm 版是 node 外壳再起原生程序，--title 还能放进命令行，但原生程序改不改进程名没验证。靠 AGENTQ_JOB 认，这条路 Codex 上实测过。
func (b *Backend) Caps() backend.Caps {
	return backend.Caps{Resume: true, SchemaByPrompt: true, Usage: true, Cost: true, LimitSignal: true, Skills: true, ReviewProfile: true, ImageInput: true, NoArgvLabel: true}
}

func (b *Backend) EnvKeep() []string { return envKeep }

// PermissionModes 实现 backend.Permissions。
func (b *Backend) PermissionModes() []string { return []string{permDefault, permAuto} }

// FullPermission：不加 --auto 时要批准的动作一律被拒，读不到工作目录外的技能。
func (b *Backend) FullPermission() string { return permAuto }

// PermissionNote 实现 backend.Permissions。
func (b *Backend) PermissionNote() string {
	return "后端是 opencode：代码会发给你配的模型来源。--auto 放开所有没明确 deny 的权限（工作目录外、.env），没有操作系统沙箱。这个后端在 agentq 里是实验：真实的 opencode 没跑过。"
}

// SkillsDir：官方列的位置有 ~/.config/opencode/skills、~/.claude/skills、~/.agents/skills，后两个是兼容读，和 Codex 共用一份。
func (b *Backend) SkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// RulesFile 实现 backend.RulesBlock：用户级说明是配置目录的 AGENTS.md。它存在时 opencode 不再读 ~/.claude/CLAUDE.md（官方：每一类第一个找到的算）。
func (b *Backend) RulesFile(home string) (string, string) {
	return filepath.Join(home, ".config", "opencode", "AGENTS.md"), ""
}

// ResumeCommand 实现 backend.ResumeHint。
func (b *Backend) ResumeCommand(session string) string { return "opencode --session=" + session }

func (b *Backend) path() string {
	if b.o.Path != "" {
		return b.o.Path
	}
	return Name
}

// schemaNote：run 没有强制结构化输出的参数，提示词里要。结果里取最后一条消息，按 schema 校验，不合格当这次没结果。
const schemaNote = "\n\n最后一条消息只输出一个符合下面 JSON Schema 的 JSON 对象，不要别的文字：\n"

// Build：提示词走标准输入（run 没终端时读 stdin）。--file 是数组参数，带等号才不会把后面的吃掉。--session 带等号：id 来自输出，不让它被当成参数。
// 审查进程要读工作目录外的技能、跑命令，没人批准，一律 --auto：跑在一次性 worktree 里，没有 Codex 那样的操作系统沙箱，能力和 Claude 的审查进程一样。
func (b *Backend) Build(req backend.Request) (backend.Invocation, []backend.Degradation, error) {
	if err := backend.CheckMust(b, req); err != nil {
		return backend.Invocation{}, nil, err
	}
	args := []string{"run", "--format", "json"}
	if req.Resume != "" {
		args = append(args, "--session="+req.Resume)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--variant", req.Effort)
	}
	if req.Permission == permAuto || req.Profile == backend.ProfileReview {
		args = append(args, "--auto")
	}
	for _, img := range req.Images {
		args = append(args, "--file="+img)
	}
	prompt := req.Prompt
	if req.Schema != "" {
		prompt += schemaNote + req.Schema
	}
	return backend.Invocation{Path: b.path(), Args: args, Record: append([]string(nil), args...), Dir: req.Dir, EnvKeep: envKeep, Stdin: prompt}, backend.Degrade(b, req), nil
}

// Auth：查不了（见 Caps），照常启动，第一次因认证失败由 Parse 认出来，暂停这个后端。
func (b *Backend) Auth(context.Context) backend.Auth { return backend.Auth{State: backend.AuthUnknown} }

// Probe 找 CLI，看 run --help 有没有要用的参数。--version 只打版本号，没有名字可认。认证查不了，不报错。
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
		return r, fmt.Errorf("no opencode CLI found. Official install: %s; then configure a model provider with opencode auth login: %w", installCommand, backend.ErrNoCLI)
	}
	out, err := backend.ProbeCommand(ctx, env.Home, p, env.Env, "--version")
	if err != nil {
		return r, fmt.Errorf("%s --version failed: %w", p, backend.ErrNoCLI)
	}
	r.Path, r.Version = p, strings.TrimSpace(string(out))
	out, err = backend.ProbeCommand(ctx, env.Home, p, env.Env, "run", "--help")
	if err != nil {
		return r, errors.New("opencode run --help failed")
	}
	text := string(out)
	r.Headless = strings.Contains(text, "--format") && strings.Contains(text, "--session") && strings.Contains(text, "--auto")
	if !r.Headless {
		return r, errors.New("CLI lacks required run --format/--session/--auto; update via its official installation owner")
	}
	return r, nil
}

// CanResume：会话存在 opencode 的数据库里，不起进程查不了。这里只有回复链问（要不要接原对话），查不了就当接不上，走普通对话。
func (b *Backend) CanResume(string) bool { return false }

func (b *Backend) RecordedModel(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" {
			return args[i+1]
		}
	}
	return ""
}
