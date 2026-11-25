// Package claude 起一次 Claude Code 会话：续上、撞额度自己等、上游出错自己重试。
package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Req 是一次会话。Session 非空是续上它。
type Req struct {
	Dir     string
	Prompt  string
	Session string
	Resume  bool
	Schema  string
	Effort  string
	Model   string
	Name    string
	// Out 是这次的输出放哪，不含扩展名。
	Out string
	Log func(string, ...any)
}

// Reply 是会话的结果。
type Reply struct {
	Text    string
	JSON    json.RawMessage
	Session string
	Tokens  int64
}

// Runner 起会话。Path 是 claude 程序。
type Runner struct {
	Path    string
	Timeout time.Duration
	// Sleep 等额度用，测试里换掉。
	Sleep func(context.Context, time.Duration) error
}

// keep：传给子进程的环境变量。桌面端会话里的 CLAUDE 变量不能带过去。
var keep = []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG", "LC_ALL", "TMPDIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "GOTOOLCHAIN", "GOROOT", "CODEX_HOME"}

const tokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// Env 是给项目脚本和别家程序的环境：干净，不带登录凭据。AGENTQ_ 开头的是 agentq 自己的设置。
func Env() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keep, k) || strings.HasPrefix(k, "AGENTQ_") {
			out = append(out, kv)
		}
	}
	return out
}

// Own 是给 agentq 自己和 Claude 的，多带登录凭据。
func Own() []string {
	if v := os.Getenv(tokenEnv); v != "" {
		return append(Env(), tokenEnv+"="+v)
	}
	return Env()
}

// NewSession 生成会话号。
func NewSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type output struct {
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	SessionID  string          `json:"session_id"`
	Status     json.RawMessage `json:"api_error_status"`
	Turns      int             `json:"num_turns"`
	Structured json.RawMessage `json:"structured_output"`
	Models     map[string]struct {
		Input  int64 `json:"inputTokens"`
		Output int64 `json:"outputTokens"`
		Read   int64 `json:"cacheReadInputTokens"`
		Write  int64 `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

const (
	upstreamTries = 4
	goOn          = "接着做完。刚才这次会话被打断了，先看一眼现在的进度，别从头来。"
)

// Run 跑到会话正常结束。撞额度睡到重置再续，上游出错退避再续；认证失败、连续出错返回错误。
func (r Runner) Run(ctx context.Context, q Req) (Reply, error) {
	q.Session = cmp.Or(q.Session, NewSession())
	var rep Reply
	fails := 0
	for n := 1; ; n++ {
		out, err := r.once(ctx, q, fmt.Sprintf("%s-%d", q.Out, n))
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		for _, m := range out.Models {
			rep.Tokens += m.Input + m.Output + m.Read + m.Write
		}
		started := out.SessionID != "" && out.Turns > 0
		switch {
		case err == nil && !out.IsError:
			rep.Text, rep.JSON, rep.Session = out.Result, out.Structured, q.Session
			return rep, nil
		case limited(out):
			until := ResetAt(out.Result, time.Now())
			q.Log("撞了额度上限，等到 %s", until.Format("01-02 15:04"))
			if err := r.sleep(ctx, time.Until(until)); err != nil {
				return rep, err
			}
		case authRe.MatchString(out.Result) || status(out) == 401:
			return rep, fmt.Errorf("Claude 没登录或登录过期：%s", line(out.Result))
		default:
			why := line(cmp.Or(out.Result, fmt.Sprint(err)))
			if fails++; fails >= upstreamTries {
				return rep, fmt.Errorf("会话连续 %d 次没跑成：%s", fails, why)
			}
			q.Log("会话没跑成（%s），稍后再试", why)
			if err := r.sleep(ctx, time.Duration(fails*fails)*time.Minute); err != nil {
				return rep, err
			}
		}
		if started { // 做到一半的续上；没开起来的换个会话号重来
			q.Resume = true
			if q.Schema == "" {
				q.Prompt = goOn
			}
		} else if !q.Resume {
			q.Session = NewSession()
		}
	}
}

func (r Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(max(d, time.Second)):
		return nil
	}
}

func (r Runner) once(ctx context.Context, q Req, base string) (output, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	open := "--session-id"
	if q.Resume {
		open = "-r"
	}
	args := []string{open, q.Session, "--name", q.Name, "-p", q.Prompt, "--model", q.Model, "--effort", q.Effort, "--permission-mode", "bypassPermissions", "--output-format", "json", "--disallowed-tools", "Monitor"}
	if q.Schema != "" {
		args = append(args, "--json-schema", q.Schema)
	}
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.Dir, cmd.Env = q.Dir, Own()
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err := os.WriteFile(base+".json", b, 0o600); err != nil {
		q.Log("会话输出没存下：%v", err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return output{}, fmt.Errorf("超过 %s 没做完", r.Timeout)
	}
	var out output
	if jerr := json.Unmarshal(b, &out); jerr != nil {
		return out, errors.Join(err, fmt.Errorf("输出读不懂：%s", line(string(b)+stderr.String())))
	}
	return out, nil
}

var (
	limitRe = regexp.MustCompile(`(?i)usage limit|rate limit|limit reached|hit your (\w+ ){0,3}limit|request rejected \(429\)`)
	authRe  = regexp.MustCompile(`(?i)please run /login|not logged in|invalid api key|oauth token has expired`)
	clockRe = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)
)

func status(o output) int {
	n, _ := strconv.Atoi(strings.Trim(string(o.Status), `"`))
	return n
}

func limited(o output) bool {
	return status(o) == 429 || (o.IsError || o.Turns <= 2) && limitRe.MatchString(o.Result)
}

// ResetAt 从额度提示里读什么时候重置，读不出就过半小时再试。
func ResetAt(text string, now time.Time) time.Time {
	if m := clockRe.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h %= 12; strings.EqualFold(m[3], "pm") {
			h += 12
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if t.Sub(now) < 6*time.Hour { // 写着「Mon 12am」的是几天后：先按半小时试
			return t.Add(time.Minute)
		}
	}
	return now.Add(30 * time.Minute)
}

func line(s string) string { return Short(s, 200) }

// Short 并成一行，最多 n 个字。
func Short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// Find 找程序，找不到返回空。
func Find(name string) string {
	home, _ := os.UserHomeDir()
	for _, d := range []string{"", home + "/.npm-global/bin/", home + "/.local/bin/", home + "/.claude/local/"} {
		if p, err := exec.LookPath(d + name); err == nil {
			return p
		}
	}
	return ""
}
