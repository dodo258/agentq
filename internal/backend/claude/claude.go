// Package claude 是 Claude Code 后端：拼启动参数、解析结果和用量、读订阅额度、读会话记录、查登录。
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// 用量上限提示里的重置时间。官方文档（code.claude.com/docs/en/errors「Usage limits」）的写法：
// 「You've hit your session limit · resets 3:45pm」「… · resets Mon 12:00am」，有的带时区「resets 8pm (Africa/Cairo)」；
// 旧版 CLI 是「Claude AI usage limit reached|<unix 秒>」。
var (
	resetsRe = regexp.MustCompile(`(?i)\bresets\s+(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?\s+|(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)\b(?:\s*\(([^)]+)\))?`)
	epochRe  = regexp.MustCompile(`limit reached\|(\d{10})\b`)
)

// ParseResetTime 从上限提示里解析重置时间，返回重置后 1 分钟（到点就能用）。
// 解析不出、或不在 (now, now+8 天] 之内（weekly 上限最长一周）就返回 false，调用方按 limit_cooldown 冷却。
// 提示里的时刻刚过去不到 1 小时，多半是任务结束到收尾之间跨过了重置点，也返回 false，免得多等一天或一周。
func ParseResetTime(s string, now time.Time) (time.Time, bool) {
	var t time.Time
	if m := epochRe.FindStringSubmatch(s); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		t = time.Unix(sec, 0)
	} else if m := resetsRe.FindStringSubmatch(s); m != nil {
		loc := now.Location()
		if m[7] != "" {
			l, err := time.LoadLocation(strings.TrimSpace(m[7]))
			if err != nil {
				return time.Time{}, false
			}
			loc = l
		}
		hour, _ := strconv.Atoi(m[4])
		minute := 0
		if m[5] != "" {
			minute, _ = strconv.Atoi(m[5])
		}
		if hour < 1 || hour > 12 || minute > 59 {
			return time.Time{}, false
		}
		hour %= 12
		if strings.EqualFold(m[6], "pm") {
			hour += 12
		}
		n := now.In(loc)
		switch {
		case m[2] != "": // 「resets Oct 14, 9am」
			mon := time.Month(strings.Index("janfebmaraprmayjunjulaugsepoctnovdec", strings.ToLower(m[2]))/3 + 1)
			day, _ := strconv.Atoi(m[3])
			t = time.Date(n.Year(), mon, day, hour, minute, 0, 0, loc)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(1, 0, 0)
			}
		case m[1] != "": // 「resets Mon 12:00am」
			want := strings.Index("sunmontuewedthufrisat", strings.ToLower(m[1])) / 3
			t = time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
			t = t.AddDate(0, 0, (want-int(n.Weekday())+7)%7)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(0, 0, 7)
			}
		default: // 「resets 3:45pm」：今天的这个点已过就是明天
			t = time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
			if justPassed(t, now) {
				return time.Time{}, false
			}
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
		}
	} else {
		return time.Time{}, false
	}
	if !t.After(now) || t.After(now.Add(8*24*time.Hour)) {
		return time.Time{}, false
	}
	return t.Add(time.Minute), true
}

func justPassed(t, now time.Time) bool { return !t.After(now) && now.Sub(t) <= time.Hour }

const budgetSubtype = "error_max_budget_usd"

func (r Result) BudgetHit() bool { return r.Subtype == budgetSubtype }

// BudgetArgs：0 表示不限，不传这个参数。
func BudgetArgs(usd float64) []string {
	if usd <= 0 {
		return nil
	}
	return []string{"--max-budget-usd", strconv.FormatFloat(usd, 'f', -1, 64)}
}

func ReadResult(path string) (Result, error) {
	var res Result
	b, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(b, &res)
}

// resultUsage 是 result.json 里和用量有关的字段。顶层 usage 只算主会话，不含子代理；
// modelUsage 按模型列出整个进程（含子代理）的用量，各模型 costUSD 之和等于 total_cost_usd。
type resultUsage struct {
	Usage *struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

// ParseJobUsage 从 result.json 内容取用量：有 modelUsage 就按模型求和（含子代理），否则用顶层 usage；
// 费用取 total_cost_usd，没有就用各模型 costUSD 之和。三样都没有返回 nil（缺 usage 的结果文件）。
func ParseJobUsage(raw []byte) *backend.JobUsage {
	var r resultUsage
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	var u backend.JobUsage
	switch {
	case len(r.ModelUsage) > 0:
		for _, m := range r.ModelUsage {
			u.Input += m.Input
			u.Output += m.Output
			u.CacheRead += m.CacheRead
			u.CacheWrite += m.CacheWrite
			u.CostUSD += m.CostUSD
		}
	case r.Usage != nil:
		u = backend.JobUsage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
	case r.TotalCostUSD == nil:
		return nil
	}
	if r.TotalCostUSD != nil {
		u.CostUSD = *r.TotalCostUSD
	}
	return &u
}

// LoginWait：claude auth status 最多等多久。测试里改小。
var LoginWait = 30 * time.Second

// LoggedIn 执行 `claude auth status`，只有明确 loggedIn=true 才返回真。
func LoggedIn(ctx context.Context, claude string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, LoginWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "auth", "status")
	cmd.Env = proc.BackendEnv(envKeep)
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.Output()
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil || st.LoggedIn == nil {
		if err != nil {
			return false, fmt.Errorf("执行 %s auth status 失败：%w", claude, err)
		}
		return false, fmt.Errorf("%s auth status 的输出看不懂", claude)
	}
	return *st.LoggedIn, nil
}

const SessionLogMaxBytes = 50 << 20

// ToolEvent 是会话记录里的一次工具调用。
var errSessionTooBig = errors.New("文件超过上限")

// ParseSessionLog 读一个 jsonl 会话记录里的工具调用，只认 type=assistant 的 message.content[].tool_use；不认识的行跳过。
func ParseSessionLog(path string) ([]backend.ToolEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > SessionLogMaxBytes {
		return nil, fmt.Errorf("%w（%s）", errSessionTooBig, textfmt.HumanSize(fi.Size()))
	}
	var out []backend.ToolEvent
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"tool_use"`)) {
			out = append(out, lineToolEvents(line)...)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func lineToolEvents(line []byte) []backend.ToolEvent {
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return nil
	}
	var parts []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if json.Unmarshal(rec.Message.Content, &parts) != nil {
		return nil
	}
	var out []backend.ToolEvent
	for _, p := range parts {
		if p.Type == "tool_use" && p.Name != "" {
			out = append(out, backend.ToolEvent{At: at, Name: p.Name, Cmd: p.Input.Command})
		}
	}
	return out
}

var SafeSessionRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

type Result struct {
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	// 最后一个回合是后台通知（task-notification）触发的时候才有；正常任务 result_index 为 0、没有 origin。
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	ResultIndex int `json:"result_index"`
	NumTurns    int `json:"num_turns"`
	// 正常结束时是 null；API 报错结束时才有值（具体类型没见过，只看有没有）。
	APIErrorStatus json.RawMessage `json:"api_error_status"`
	Subtype        string          `json:"subtype"`
	// StructuredOutput：带 --json-schema 启动时的结构化结果。
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// mayBeLimit：只有报错结束、API 报了错、或回合数很少时才扫用量上限字样。正常做了很多回合的任务，
// 结果里提到 rate limit（比如修的就是限流代码）不算撞上限。
func (r Result) mayBeLimit() bool {
	api := strings.TrimSpace(string(r.APIErrorStatus))
	return r.IsError || (api != "" && api != "null") || (r.NumTurns <= backend.LimitScanTurns && r.ResultIndex == 0)
}

// 方案列的三种字样，加上 CLI 现行文案 You've hit your limit、You've hit your monthly spend limit，
// 和官方文档（code.claude.com/docs/en/errors）里 429 的 Request rejected (429)。结构化字段优先，见 hitUsageLimit。
var limitRe = regexp.MustCompile(`(?i)usage limit|rate limit|limit reached|hit your (\w+ ){0,3}limit|request rejected \(429\)|api error: 429`)

func IsUsageLimit(s string) bool { return limitRe.MatchString(s) }

// authFailedRe：CLI 报错结束时认证失败的说法。只在 is_error 为真时看，模型正文提到这些字样不算。
var authFailedRe = regexp.MustCompile(`(?i)please run /login|not logged in|invalid api key|authentication[_ ]error|oauth token has expired`)

// authFailed：这次是认证失败结束的。
func (r Result) authFailed() bool {
	return r.IsError && (r.ApiStatus() == 401 || authFailedRe.MatchString(r.Result))
}

// ApiStatus 取 api_error_status 的数字（数字或数字字符串）；null、缺失、读不懂返回 0。
func (r Result) ApiStatus() int {
	s := strings.Trim(strings.TrimSpace(string(r.APIErrorStatus)), `"`)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// HitUsageLimit：结构化字段优先，429 就是撞上限；回合、预算上限（error_max_*）是自己设的，不算；其余看字样。
func (r Result) HitUsageLimit() bool {
	if r.ApiStatus() == 429 {
		return true
	}
	if strings.HasPrefix(r.Subtype, "error_max") {
		return false
	}
	return r.mayBeLimit() && IsUsageLimit(r.Result)
}

// 订阅额度：领活前用 `claude -p /usage`（Claude Code 自己回答的本地命令，不问模型、不花 token）读各窗口已用百分比和重置时间，
// 快用完就不领新活。做法学 magpie（yetone/magpie internal/provider/claude_usage_cli.go）。
const usageReadWait = 60 * time.Second

// 「Current session: 9% used · resets Oct 11 at 11:29am (America/New_York)」
// 「Current week (all models): 17% used · resets Oct 14 at 6:59pm (America/New_York)」「Current week (Fable): 3% used」
var usageLineRe = regexp.MustCompile(`^Current (session|week(?: \(([^)]+)\))?):\s*([0-9.]+)% used(?:\s*·\s*(resets .+))?$`)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// ParseUsage 从 /usage 的文字里取各窗口。一个窗口都没有就是看不懂，返回错误（调用方当未知）。
// 重置时间按 parseResetTime 的写法解析（它返回重置后 1 分钟，这里退回整点），换到本机时区显示，解析不出就不记。
func ParseUsage(text string, now time.Time) ([]backend.UsageWindow, error) {
	text = strings.TrimSpace(ansiRe.ReplaceAllString(text, ""))
	var out []backend.UsageWindow
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		m := usageLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || seen[m[1]] {
			continue
		}
		used, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		seen[m[1]] = true
		w := backend.UsageWindow{Label: m[1], Used: used}
		if s := strings.TrimSpace(m[2]); s != "" && !strings.EqualFold(s, "all models") {
			w.Scope = s
		}
		if m[4] != "" {
			if t, ok := ParseResetTime(m[4], now); ok {
				t = t.Add(-time.Minute).In(now.Location())
				w.ResetsAt = &t
			}
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		first, _, _ := strings.Cut(text, "\n")
		if first == "" {
			return nil, errors.New("/usage 没有输出")
		}
		return nil, fmt.Errorf("看不懂 /usage 的输出（没有「Current session/week … %% used」行）：%s", textfmt.OneLine(first, 120))
	}
	return out, nil
}

// readUsageText 执行 `claude -p /usage`，返回结果文字。在空的临时目录里跑、不带工具、不读用户和项目的设置
// （免得项目 hook 拖慢或输出混进来）、不存会话记录。环境变量照任务的白名单：不会带上
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC（带上它 /usage 不联网，只给一小时内别处留下的旧读数）。
func readUsageText(ctx context.Context, claude string) (string, error) {
	tmp, err := os.MkdirTemp("", "agentq-usage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, usageReadWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p", "/usage", "--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence")
	cmd.Dir = tmp
	cmd.Env = proc.BackendEnv(envKeep)
	cmd.Stdin = strings.NewReader("")
	proc.BoundCmd(cmd) // tick 这时拿着锁
	defer proc.ClearLeftovers(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var res Result
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err == nil && (res.Result != "" || res.IsError) {
		if res.IsError {
			return "", errors.New("claude -p /usage 报错：" + textfmt.OneLine(res.Result, 200))
		}
		return res.Result, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	if runErr != nil {
		if msg != "" {
			return "", fmt.Errorf("执行 claude -p /usage 失败：%v：%s", runErr, textfmt.OneLine(msg, 200))
		}
		return "", fmt.Errorf("执行 claude -p /usage 失败：%w", runErr)
	}
	return "", errors.New("claude -p /usage 的输出不是 JSON 结果：" + textfmt.OneLine(msg, 200))
}

const RouterDeny = `{"permissions":{"deny":["Read(**/.env*)","Read(**/secrets/**)","Read(**/.agentq/sense/**)"]}}`
