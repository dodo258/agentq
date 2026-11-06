package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

// Name 是这个后端的注册名。
const Name = backend.Default

func init() { backend.Register(Name, New) }

// envKeep：用户可能有意设置的登录和配置变量。ANTHROPIC_API_KEY 不放行，防止任务改走 API 计费。
var envKeep = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"}

// Backend 是 Claude Code 的实现。
type Backend struct{ o backend.Options }

// New 建实例。
func New(o backend.Options) backend.Backend { return &Backend{o: o} }

func (b *Backend) Name() string { return Name }

func (b *Backend) Caps() backend.Caps {
	return backend.Caps{
		Resume: true, PresetSession: true, HardBudget: true, MaxTurns: true, Schema: true, ToolProfiles: true, DenyRules: true,
		NoExternalMCP: true, Usage: true, Cost: true, LimitSignal: true, AuthProbe: true, Skills: true, ReviewProfile: true, ImageInput: true, Monitor: true,
	}
}

func (b *Backend) EnvKeep() []string { return envKeep }

// ReleaseDeny：准备任务权限是 bypassPermissions（要改版本号、跑项目脚本），这几条挡在提交接口前面。官方文档说 Bash 规则是字符串匹配，
// 写法一变就挡不住（如换成变量），所以它只是多一道；真正的边界见 README「发版助手」的剩余风险。
const ReleaseDeny = `{"permissions":{"deny":["Bash(*reviewSubmission*)","Bash(*appStoreVersionSubmission*)","WebFetch(domain:api.appstoreconnect.apple.com)"]}}`

func denySettings(d backend.DenyPolicy) string {
	switch d {
	case backend.DenySecrets:
		return RouterDeny
	case backend.DenyStoreSubmit:
		return ReleaseDeny
	}
	return ""
}

// Build 按 Profile 拼参数。两种写法的参数顺序不同，是历史留下的，argv 快照（testdata/golden/argv.txt）固定着。
func (b *Backend) Build(req backend.Request) (backend.Invocation, []backend.Degradation, error) {
	if err := backend.CheckMust(b, req); err != nil {
		return backend.Invocation{}, nil, err
	}
	var args []string
	switch req.Profile {
	case backend.ProfileReadOnly, backend.ProfileNoTools:
		args = restrictedArgs(req)
	default:
		args = taskArgs(req)
	}
	return backend.Invocation{Path: b.o.Path, Args: args, Record: redactPrompt(args), Dir: req.Dir, EnvKeep: envKeep}, nil, nil
}

// taskArgs 是能动手的任务的参数：resume 非空时用 -r 续跑那个会话（会话 id 不变），否则用 --session-id 指定新会话的 id。
// 禁用 Monitor：-p 进程在回合结束时就退出，用 Monitor 等通知会中途退出（实测这个参数对子代理同样生效）。
func taskArgs(req backend.Request) []string {
	var args []string
	switch {
	case req.Resume != "":
		args = append(args, "-r", req.Resume)
	case req.Session != "":
		args = append(args, "--session-id", req.Session)
	}
	args = append(append(args, "-p", req.Prompt), modelArgs(req)...)
	if req.Permission != "" {
		args = append(args, "--permission-mode", req.Permission)
	}
	args = append(args, "--output-format", "json", "--disallowed-tools", "Monitor", "--name", req.Label)
	if req.Schema == "" {
		args = append(args, BudgetArgs(req.BudgetUSD)...)
		return append(args, addDirArgs(req)...)
	}
	args = append(args, addDirArgs(req)...)
	args = append(args, "--json-schema", req.Schema)
	if s := denySettings(req.Deny); s != "" {
		args = append(args, "--settings", s)
	}
	return append(args, BudgetArgs(req.BudgetUSD)...)
}

// restrictedArgs 是只读或没有工具的任务的参数。绝不传 --permission-mode：读文件的边界（工作目录外拒绝、.env 拒绝）是默认权限模式下实测的，放开就没了。
func restrictedArgs(req backend.Request) []string {
	var args []string
	if req.Resume != "" {
		args = append(args, "-r", req.Resume, "-p", req.Prompt)
	} else {
		args = append(args, "-p", req.Prompt)
		if req.Session != "" {
			args = append(args, "--session-id", req.Session)
		}
	}
	args = append(args, modelArgs(req)...)
	tools := ""
	if req.Profile == backend.ProfileReadOnly {
		tools = "Read,Grep,Glob"
	}
	args = append(args, "--tools", tools)
	if req.NoExternalMCP {
		args = append(args, "--strict-mcp-config")
	}
	if s := denySettings(req.Deny); s != "" {
		args = append(args, "--settings", s)
	}
	if req.Resume == "" { // 接原会话的只读任务，附件目录在最后
		args = append(args, addDirArgs(req)...)
	}
	if req.Schema != "" {
		args = append(args, "--json-schema", req.Schema)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	args = append(args, BudgetArgs(req.BudgetUSD)...)
	args = append(args, "--output-format", "json", "--name", req.Label)
	if req.Resume != "" {
		args = append(args, addDirArgs(req)...)
	}
	return args
}

// modelArgs：模型和强度留空就跟随用户在 CLI 里选的，不传。
func modelArgs(req backend.Request) []string {
	var args []string
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return args
}

func addDirArgs(req backend.Request) []string {
	var args []string
	for _, d := range req.AddDirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

// redactPrompt 在 meta.json 里把提示词换成「见 prompt.txt」，免得重复存一大段。
func redactPrompt(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-p" {
			out[i+1] = "<见 prompt.txt>"
		}
	}
	return out
}

// Parse 读 result.json：--output-format json 只在退出时写一次。读不出或不是 JSON 就没有终态，撞限字样从原文和 stderr 末尾里找。
func (b *Backend) Parse(job backend.JobRef, exit backend.Exit) backend.Result {
	raw, rerr := os.ReadFile(filepath.Join(job.Dir, "result.json"))
	var out backend.Result
	if rerr == nil {
		out.Usage = ParseJobUsage(raw)
		out.UsageCumulative = true // 续跑时 modelUsage 和 total_cost_usd 是整个会话累计的（实测）
	}
	var r Result
	if rerr != nil || json.Unmarshal(raw, &r) != nil {
		scan := string(raw) + "\n" + fsx.TailLines(filepath.Join(job.Dir, "stderr.log"), 5)
		out.LimitText = IsUsageLimit(scan)
		if out.LimitText {
			out.ResetAt = resetAt(scan, exit)
		}
		return out
	}
	out.HaveTerminal = true
	out.Failed = r.IsError
	out.Text = r.Result
	out.SessionID = r.SessionID
	out.Turns = r.NumTurns
	out.HTTPStatus = r.ApiStatus()
	out.Structured = r.StructuredOutput
	out.Background = r.Origin.Kind == "task-notification" || r.ResultIndex > 0
	selfLimit := strings.HasPrefix(r.Subtype, "error_max")
	switch {
	case r.BudgetHit():
		out.Stop = backend.StopMaxBudget
	case selfLimit:
		out.Stop = backend.StopMaxTurns
	case r.authFailed():
		out.Stop = backend.StopAuth
	}
	out.LimitStructured = r.ApiStatus() == 429
	out.LimitText = !selfLimit && r.mayBeLimit() && IsUsageLimit(r.Result)
	if out.LimitStructured || out.LimitText {
		out.ResetAt = resetAt(r.Result, exit)
	}
	return out
}

func resetAt(text string, exit backend.Exit) time.Time {
	if exit.At.IsZero() {
		return time.Time{}
	}
	t, ok := ParseResetTime(text, exit.At)
	if !ok {
		return time.Time{}
	}
	return t
}

// SessionFiles 找任务的会话记录：主会话和 subagents/*.jsonl。这是 Claude Code 的内部格式，官方不保证。
func SessionFiles(home, sessionID string) (main string, subs []string) {
	base := filepath.Join(home, ".claude", "projects", "*", sessionID)
	top, _ := filepath.Glob(base + ".jsonl")
	subs, _ = filepath.Glob(filepath.Join(base, "subagents", "*.jsonl"))
	if len(top) > 0 {
		main = top[0]
	}
	return main, subs
}

// LastActivity：~/.claude/projects/*/<会话id>.jsonl 和它的子代理记录里最新的修改时间。没有会话 id（旧版本启动的任务）时判断不了。
func (b *Backend) LastActivity(job backend.JobRef) (time.Time, bool) {
	if !SafeSessionRe.MatchString(job.SessionID) {
		return time.Time{}, false
	}
	// 同一会话可能在几个项目目录都有记录：全部看，不能只看 SessionFiles 挑的第一个，否则后写的那份算不上，误报疑似卡死
	base := filepath.Join(b.o.Home, ".claude", "projects", "*", job.SessionID)
	top, _ := filepath.Glob(base + ".jsonl")
	subs, _ := filepath.Glob(filepath.Join(base, "subagents", "*.jsonl"))
	var last time.Time
	for _, f := range append(top, subs...) {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last, true
}

func (b *Backend) CanResume(session string) bool {
	if !SafeSessionRe.MatchString(session) {
		return false
	}
	main, _ := SessionFiles(b.o.Home, session)
	return main != ""
}

// ResumeCommand 实现 backend.ResumeHint。
func (b *Backend) ResumeCommand(session string) string { return "claude -r " + session }

func (b *Backend) RecordedModel(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" {
			return args[i+1]
		}
	}
	return ""
}

// ToolEvents 读会话记录里的工具调用。读不了、认不出（主会话里一次工具调用都认不出）返回错误，调用方当未知。
func (b *Backend) ToolEvents(job backend.JobRef, withSubs bool) ([]backend.ToolEvent, [][]backend.ToolEvent, error) {
	if !SafeSessionRe.MatchString(job.SessionID) {
		return nil, nil, errors.New("没有会话 id")
	}
	mainPath, subPaths := SessionFiles(b.o.Home, job.SessionID)
	if mainPath == "" {
		return nil, nil, errors.New("找不到主会话记录")
	}
	main, err := ParseSessionLog(mainPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%s：%w", filepath.Base(mainPath), err)
	}
	if len(main) == 0 {
		return nil, nil, fmt.Errorf("%s 里认不出工具调用，格式可能变了", filepath.Base(mainPath))
	}
	if !withSubs {
		return main, nil, nil
	}
	subs := make([][]backend.ToolEvent, 0, len(subPaths))
	for _, p := range subPaths {
		s, err := ParseSessionLog(p)
		if err != nil {
			return nil, nil, fmt.Errorf("子代理记录 %s：%w", filepath.Base(p), err)
		}
		subs = append(subs, s)
	}
	return main, subs, nil
}

func (b *Backend) QuotaText(ctx context.Context) (string, error) { return readUsageText(ctx, b.o.Path) }

func (b *Backend) ParseQuota(text string, now time.Time) ([]backend.UsageWindow, error) {
	return ParseUsage(text, now)
}

// Auth 只有明确 loggedIn=true 才算登录；没得出结论不启动。
func (b *Backend) Auth(ctx context.Context) backend.Auth {
	ok, err := LoggedIn(ctx, b.o.Path)
	switch {
	case err != nil:
		return backend.Auth{State: backend.AuthError, Detail: "确认终端版 Claude 登录状态失败，不启动：" + err.Error()}
	case !ok:
		return backend.Auth{State: backend.AuthOut, Detail: "终端版 Claude 未登录（claude auth status 显示 loggedIn=false），不启动"}
	}
	return backend.Auth{State: backend.AuthIn}
}

// Probe 找能独立运行的 CLI（桌面端里带的不算），确认无头参数都在，再查认证。
func (b *Backend) Probe(ctx context.Context, env backend.ProbeEnv) (backend.Probe, error) {
	var r backend.Probe
	run := func(path string, args ...string) ([]byte, error) {
		return backend.ProbeCommand(ctx, env.Home, path, env.Env, args...)
	}
	var candidates []string
	if env.Explicit != "" {
		candidates = []string{fsx.ExpandHome(env.Home, env.Explicit)}
	} else {
		if p, e := backend.ProbePath(env.Home, "claude", env.Env); e == nil {
			candidates = append(candidates, p)
		}
		candidates = append(append(candidates, filepath.Join(env.Home, ".local", "bin", "claude")), fixedCLIPaths...)
	}
	for _, p := range candidates {
		p, e := backend.ProbePath(env.Home, p, env.Env)
		if e != nil {
			continue
		}
		p, e = filepath.Abs(p)
		if e != nil {
			continue
		}
		resolved, e := filepath.EvalSymlinks(p)
		if e != nil {
			continue
		}
		if strings.Contains(resolved, ".app/Contents/") {
			continue
		}
		out, e := run(p, "--version")
		if e != nil || !strings.Contains(string(out), "Claude Code") {
			continue
		}
		r.Path = p
		r.Version = strings.TrimSpace(string(out))
		break
	}
	if r.Path == "" {
		return r, fmt.Errorf("no independently callable Claude CLI; Desktop app/ZIP is not an engine. Official install: %s; then %s: %w", cliInstallCommand, cliLoginCommand, backend.ErrNoCLI)
	}
	out, e := run(r.Path, "--help")
	if e != nil {
		return r, errors.New("Claude CLI help failed")
	}
	text := string(out)
	r.Headless = strings.Contains(text, "--print") && strings.Contains(text, "--output-format") && strings.Contains(text, "--session-id") && strings.Contains(text, "--effort")
	if !r.Headless {
		return r, errors.New("CLI lacks required print/JSON/session/effort capability; update via its official installation owner")
	}
	out, e = run(r.Path, "auth", "status")
	var a struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &a) == nil {
		r.Authenticated = a.LoggedIn
		r.AuthMethod = a.AuthMethod
	}
	if e != nil || !r.Authenticated {
		return r, fmt.Errorf("CLI authentication unavailable in the background environment; run %s, then retry. No credentials were requested or changed: %w", cliLoginCommand, backend.ErrNotLoggedIn)
	}
	return r, nil
}
