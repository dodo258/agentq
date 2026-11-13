package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
)

func TestRulesPaths(t *testing.T) {
	g, ok := New(backend.Options{}).(backend.GlobalRules)
	if !ok {
		t.Fatal("Claude 后端要有用户级规则目录")
	}
	dir, instructions := g.RulesPaths("/h")
	if dir != "/h/.claude/rules" || len(instructions) != 1 || instructions[0] != "/h/.claude/CLAUDE.md" {
		t.Fatalf("%s %v", dir, instructions)
	}
}

func TestParseResetTime(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 10, 15, 30, 0, 0, loc)                                      // 周六
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m+1, 0, 0, loc) } // 重置后 1 分钟
	cases := []struct {
		in   string
		want time.Time
	}{
		{"You've hit your session limit · resets 3:45pm", at(10, 15, 45)},
		{"You've hit your limit · resets 5pm", at(10, 17, 0)},
		{"You've hit your Opus limit · resets 2pm", at(11, 14, 0)}, // 今天 2 点已过 1 个多小时 → 明天
		{"You've hit your weekly limit · resets Mon 12:00am", at(12, 0, 0)},
		{"You've hit your weekly limit · resets Sat 9am", at(17, 9, 0)}, // 今天就是周六且已过好几个小时 → 下周
		{"5-hour limit reached ∙ resets Oct 12, 9am", at(12, 9, 0)},
		{"You've hit your limit · resets 8pm (UTC)", time.Date(2026, 10, 11, 4, 1, 0, 0, loc)},
		{"Claude AI usage limit reached|" + "1791624600", time.Unix(1791624600, 0).Add(time.Minute)}, // 2026-10-10 17:30 CST
	}
	for _, c := range cases {
		got, ok := ParseResetTime(c.in, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q：得到 %v %v，应为 %v", c.in, got, ok, c.want)
		}
	}
	for _, s := range []string{
		"You've hit your session limit",
		"Claude AI usage limit reached|1760000000", // 已经过去的时间
		"resets 13pm",
		"resets 5pm (Not/AZone)",
		"resets Oct 9, 9am", // 今年的已过 → 明年，超过 8 天不采用
		// 刚过去不到 1 小时：多半是收尾晚了、已经重置，不推到明天/下周
		"You've hit your session limit · resets 3pm",
		"You've hit your weekly limit · resets Sat 2:45pm",
		"resets Oct 10, 3:00pm",
	} {
		if got, ok := ParseResetTime(s, now); ok {
			t.Errorf("%q 不应采用：%v", s, got)
		}
	}
}

func TestMayBeLimit(t *testing.T) {
	parse := func(s string) Result {
		var r Result
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	yes := []string{
		`{"is_error":true,"num_turns":50}`,
		`{"is_error":false,"num_turns":50,"api_error_status":429}`,
		`{"is_error":false,"num_turns":1,"result_index":0}`,
		`{"is_error":false}`,
	}
	no := []string{
		`{"is_error":false,"num_turns":40,"api_error_status":null}`,
		`{"is_error":false,"num_turns":1,"result_index":2}`, // 后台通知回合，不是一开始就撞上限
	}
	for _, s := range yes {
		if !parse(s).mayBeLimit() {
			t.Errorf("%s 应扫上限字样", s)
		}
	}
	for _, s := range no {
		if parse(s).mayBeLimit() {
			t.Errorf("%s 不应扫上限字样", s)
		}
	}
}

// 照真实 result.json（任务 sample-task）删减：顶层 usage 只算主会话，modelUsage 含子代理。
const resultWithUsage = `{"type":"result","is_error":false,"result":"已合并","session_id":"s",
"total_cost_usd":4.6071268000000005,
"usage":{"input_tokens":92,"cache_creation_input_tokens":157333,"cache_read_input_tokens":5681707,"output_tokens":54650},
"modelUsage":{"claude-opus-5-5":{"inputTokens":122,"outputTokens":69768,"cacheReadInputTokens":6388024,"cacheCreationInputTokens":292335,"costUSD":4.6071268000000005}}}`

func TestParseJobUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *backend.JobUsage
	}{
		{"有 modelUsage 用它（含子代理）", resultWithUsage, &backend.JobUsage{Input: 122, Output: 69768, CacheRead: 6388024, CacheWrite: 292335, CostUSD: 4.6071268000000005}},
		{"多个模型相加", `{"total_cost_usd":3,"modelUsage":{"a":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":1},"b":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":2}}}`,
			&backend.JobUsage{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, CostUSD: 3}},
		{"只有顶层 usage", `{"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":7,"cache_creation_input_tokens":8},"total_cost_usd":0.5}`,
			&backend.JobUsage{Input: 5, Output: 6, CacheRead: 7, CacheWrite: 8, CostUSD: 0.5}},
		{"没有 total_cost_usd 用各模型之和", `{"modelUsage":{"a":{"inputTokens":1,"costUSD":0.25},"b":{"inputTokens":1,"costUSD":0.5}}}`,
			&backend.JobUsage{Input: 2, CostUSD: 0.75}},
		{"缺 usage", `{"type":"result","is_error":false,"result":"做完了","session_id":"s"}`, nil},
		{"空文件", ``, nil},
		{"格式不对", `boom`, nil},
	}
	for _, c := range cases {
		got := ParseJobUsage([]byte(c.raw))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s：得到 %+v，应为 %+v", c.name, got, c.want)
		}
	}
}

// Args 是普通任务的参数，配置按 runner.TaskRequest 的填法。
func Args(c config.Config, prompt, project, title, resume, session string) []string {
	return taskArgs(backend.Request{Prompt: prompt, Label: "agentq " + project + " " + title, Resume: resume, Session: session,
		Model: c.Model, Effort: c.Effort, Permission: c.PermissionMode})
}

func TestManualPermissionModeAccepted(t *testing.T) {
	c := config.Default()
	c.PermissionMode = "manual"
	if e := c.Validate(); e != nil {
		t.Fatal("official manual alias rejected", e)
	}
	if !strings.Contains(strings.Join(Args(c, "p", "p", "t", "", ""), " "), "--permission-mode manual") {
		t.Fatal("manual selection not passed to CLI")
	}
}

func TestSetupPermissionSelection(t *testing.T) {
	c := config.Default()
	c.PermissionMode = "default"
	args := strings.Join(Args(c, "p", "p", "t", "", ""), " ")
	if strings.Contains(args, "bypassPermissions") || !strings.Contains(args, "--permission-mode default") {
		t.Fatal(args)
	}
	c.PermissionMode = "invalid"
	if c.Validate() == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestIsUsageLimit(t *testing.T) {
	yes := []string{"Claude AI usage limit reached|1760000000", "API Error: Rate limit exceeded", "5-hour LIMIT REACHED",
		"You've hit your limit · resets 5pm", "You've hit your monthly spend limit"}
	no := []string{"", "合并了，提交 abc1234", "limits of the system", "hit your target, no limits"}
	for _, s := range yes {
		if !IsUsageLimit(s) {
			t.Errorf("应识别为用量上限：%q", s)
		}
	}
	for _, s := range no {
		if IsUsageLimit(s) {
			t.Errorf("不应识别为用量上限：%q", s)
		}
	}
}

// `claude -p /usage` 输出的样子，数字和时区是编的。
const usageSample = `You are currently using your subscription to power your Claude Code usage

Current session: 9% used · resets Oct 11 at 11:29am (America/New_York)
Current week (all models): 17% used · resets Oct 14 at 6:59pm (America/New_York)
Current week (Fable): 3% used · resets Oct 14 at 7pm (America/New_York)

What's contributing to your limits usage?
Last 24h · 120 requests · 6 sessions
  30% of your usage came from subagent-heavy sessions`

func TestParseUsage(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, ny)
	ws, err := ParseUsage(usageSample, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		label, scope string
		used         float64
		reset        time.Time
	}{
		{"session", "", 9, time.Date(2026, 10, 11, 11, 29, 0, 0, ny)},
		{"week (all models)", "", 17, time.Date(2026, 10, 14, 18, 59, 0, 0, ny)},
		{"week (Fable)", "Fable", 3, time.Date(2026, 10, 14, 19, 0, 0, 0, ny)},
	}
	if len(ws) != len(want) {
		t.Fatalf("窗口数 %d：%+v", len(ws), ws)
	}
	for i, w := range want {
		g := ws[i]
		if g.Label != w.label || g.Scope != w.scope || g.Used != w.used || g.ResetsAt == nil || !g.ResetsAt.Equal(w.reset) {
			t.Errorf("第 %d 个窗口 %+v（重置 %v），想要 %+v", i, g, g.ResetsAt, w)
		}
	}

	// 别的写法：带 ANSI 颜色、小数、逗号日期、没有重置时间、重复的行只取第一行
	odd := "\x1b[1mCurrent session: 97.5% used · resets Oct 11, 2:30pm (UTC)\x1b[0m\nCurrent week (Fable): 12% used\nCurrent session: 5% used\n"
	ws, err = ParseUsage(odd, now)
	if err != nil || len(ws) != 2 {
		t.Fatalf("%v %+v", err, ws)
	}
	if ws[0].Used != 97.5 || ws[0].ResetsAt == nil || !ws[0].ResetsAt.Equal(time.Date(2026, 10, 11, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("会话窗口：%+v %v", ws[0], ws[0].ResetsAt)
	}
	if ws[1].Scope != "Fable" || ws[1].Used != 12 || ws[1].ResetsAt != nil {
		t.Errorf("没写重置时间的窗口：%+v", ws[1])
	}

	// 读不懂：一个窗口都没有
	for _, s := range []string{"", "You are currently using your subscription to power your Claude Code usage",
		"Current session: lots used", "Usage: 40%", "Error: not logged in"} {
		if ws, err := ParseUsage(s, now); err == nil {
			t.Errorf("%q 应当看不懂，得到 %+v", s, ws)
		}
	}
}

// 模型和强度留空就跟随 CLI：不传这两个参数（T6 要的）。
func TestBuildOmitsEmptyModel(t *testing.T) {
	inv, _, err := New(backend.Options{Path: "/bin/claude"}).Build(backend.Request{Prompt: "p", Label: "agentq x ", Session: "s", Dir: "/d"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(inv.Args, " ")
	if strings.Contains(joined, "--model") || strings.Contains(joined, "--effort") {
		t.Error(joined)
	}
	if inv.Path != "/bin/claude" || inv.Dir != "/d" || strings.Contains(strings.Join(inv.Record, " "), "-p p") {
		t.Errorf("%+v", inv)
	}
}

func TestParseNormalizes(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) backend.Result {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return New(backend.Options{}).Parse(backend.JobRef{Dir: dir}, backend.Exit{At: time.Date(2026, 10, 10, 15, 30, 0, 0, time.Local)})
	}
	if r := write(`不是 JSON，You've hit your limit · resets 5pm`); r.HaveTerminal || !r.LimitText || r.ResetAt.IsZero() {
		t.Errorf("没有终态也要认出撞限和重置时间：%+v", r)
	}
	if r := write(`{"is_error":true,"subtype":"error_max_budget_usd","result":"rate limit","num_turns":1}`); !r.HaveTerminal || !r.Failed || r.Stop != backend.StopMaxBudget || r.LimitText {
		t.Errorf("撞预算不算撞用量上限：%+v", r)
	}
	if r := write(`{"subtype":"error_max_turns"}`); r.Stop != backend.StopMaxTurns || !r.Stop.SelfLimit() {
		t.Errorf("%+v", r)
	}
	if r := write(`{"api_error_status":429,"result":"x","num_turns":50}`); !r.LimitStructured || r.LimitText || r.HTTPStatus != 429 {
		t.Errorf("%+v", r)
	}
	if r := write(`{"result":"rate limit 的代码改好了","num_turns":50}`); r.LimitStructured || r.LimitText {
		t.Errorf("做了很多回合的结果提到限流不算撞上限：%+v", r)
	}
	if r := write(`{"origin":{"kind":"task-notification"},"session_id":"s","total_cost_usd":1.5}`); !r.Background || r.SessionID != "s" || r.Usage == nil || !r.UsageCumulative {
		t.Errorf("%+v", r)
	}
}

// 同一会话在两个项目目录都有记录（比如 worktree 换了目录）：要取所有记录里最新的，只看第一个会误报疑似卡死。
func TestLastActivityAllProjectDirs(t *testing.T) {
	home := t.TempDir()
	const sid = "s-1"
	touch := func(rel string, at time.Time) {
		t.Helper()
		p := filepath.Join(home, ".claude", "projects", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	touch("-aaa/"+sid+".jsonl", base)
	touch("-zzz/"+sid+".jsonl", base.Add(time.Hour))
	be := New(backend.Options{Home: home})
	got, ok := be.LastActivity(backend.JobRef{SessionID: sid})
	if !ok || !got.Equal(base.Add(time.Hour)) {
		t.Errorf("主会话要取最新的那份：%v %v", got, ok)
	}
	// 子代理记录在排在后面的项目目录里，也算
	touch("-zzz/"+sid+"/subagents/a.jsonl", base.Add(2*time.Hour))
	if got, _ := be.LastActivity(backend.JobRef{SessionID: sid}); !got.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("子代理记录也算：%v", got)
	}
}

// 认证失败要认得出来：门和任务才能暂停重试，不把一串任务都卡成要人点。只看报错结束的，模型正文提到不算。
// Cloudflare 拦截页的 403 按结构化状态码加 API 错误正文认成上游瞬时错误；模型正文（is_error 为假）里提到不算。
func TestParseGatewayChallenge(t *testing.T) {
	b := New(backend.Options{})
	for raw, want := range map[string]bool{
		`{"is_error":true,"api_error_status":403,"num_turns":73,"total_cost_usd":5.9,"result":"Failed to authenticate. API Error: 403 Attention Required! | Cloudflare","session_id":"s"}`: true,
		`{"is_error":true,"api_error_status":502,"result":"API Error: 502 Bad Gateway"}`:                                                                                                   true,
		`{"is_error":true,"api_error_status":403,"result":"Your organization has disabled Claude subscription access for Claude Code"}`:                                                    false,
		`{"is_error":false,"num_turns":40,"result":"修好了 Attention Required! | Cloudflare 的页面识别"}`:                                                                                          false,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := b.Parse(backend.JobRef{Dir: dir}, backend.Exit{}).Upstream(); got != want {
			t.Errorf("%s：%v，要 %v", raw, got, want)
		}
	}
}

func TestParseAuthFailure(t *testing.T) {
	b := New(backend.Options{})
	for raw, want := range map[string]backend.Stop{
		`{"is_error":true,"api_error_status":401,"result":"boom"}`:                  backend.StopAuth,
		`{"is_error":true,"result":"Invalid API key · Please run /login"}`:          backend.StopAuth,
		`{"is_error":true,"result":"Not logged in"}`:                                backend.StopAuth,
		`{"is_error":false,"result":"文档里写 Please run /login"}`:                      backend.StopNone,
		`{"is_error":true,"api_error_status":500,"result":"boom"}`:                  backend.StopNone,
		`{"is_error":true,"api_error_status":403,"result":"organization disabled"}`: backend.StopNone,
		`{"is_error":true,"subtype":"error_max_turns","result":"x"}`:                backend.StopMaxTurns,
		// 真实的 Cloudflare 拦截页：文字说 Failed to authenticate，但不是令牌失效，不能暂停后端
		`{"is_error":true,"api_error_status":403,"result":"Failed to authenticate. API Error: 403 Attention Required! | Cloudflare"}`: backend.StopNone,
		`{"is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 authentication_error"}`:             backend.StopAuth,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := b.Parse(backend.JobRef{Dir: dir}, backend.Exit{}).Stop; got != want {
			t.Errorf("%s：%q，要 %q", raw, got, want)
		}
	}
}
