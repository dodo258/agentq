package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
)

const resultFile = "result.json" // 标准输出，这里是一行一个事件

// maxEventBytes：一行事件的上限。命令输出整段进 aggregated_output，可能很长，太长的行当没看到。
const maxEventBytes = 32 << 20

// maxToolLogBytes：看进展读事件流的上限，和 Claude 的会话记录一样。
const maxToolLogBytes = 50 << 20

type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	// Message、Error 不写死类型：失败事件的字段长得不对也不能让这一行被整行丢掉，丢了就看不到它失败了。
	Message json.RawMessage `json:"message"`
	Error   json.RawMessage `json:"error"`
	Usage   *struct {
		Input      int64 `json:"input_tokens"`
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
		Reasoning  int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Item *item `json:"item"`
}

type item struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Tool    any    `json:"tool"`
}

// noReason：失败事件没给原因时的说明。
const noReason = "Codex 报告这一轮失败，没有给原因"

// errorText 取失败事件的文字：字符串，或带 message 的对象。取不到就是空，调用方给默认说明。
func errorText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Message
	}
	return ""
}

// scan 逐行读事件流，fn 收到这一行的开头位置和事件。不是 JSON 的行跳过：被杀时最后一行可能只写了一半。
func scan(dir string, fn func(off int64, e event)) error {
	f, err := os.Open(filepath.Join(dir, resultFile))
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && len(line) <= maxEventBytes {
			var e event
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				fn(off, e)
			}
		}
		off += int64(len(line))
		if err != nil {
			return nil
		}
	}
}

// Parse：turn.completed 是成功的终态，turn.failed 是失败的终态。走完一轮后又开了一轮（Stop 钩子把模型叫回来）就看最后一轮。
// 进程在中途死了就没有终态，不猜成功。error 事件里 Reconnecting 是重试中的提示，不算结果；别的 error 事件是最后一个事件时算失败结束。
// 撞限和认证只看错误通道（turn.failed、error、stderr），不看模型正文。item 里 type=error 是提示（比如配置有不认识的键），不是失败。
func (b *Backend) Parse(job backend.JobRef, exit backend.Exit) backend.Result {
	var out backend.Result
	var last, turnMessage, failure, lastError string // turnMessage 只看最后一轮：结构化输出不能拿前一轮的
	var usage *backend.JobUsage
	ended, failed, errored := false, false, false
	err := scan(job.Dir, func(_ int64, e event) {
		switch e.Type {
		case "thread.started":
			out.SessionID = e.ThreadID
		case "turn.started":
			ended, failed, errored, failure, turnMessage = false, false, false, "", ""
		case "turn.completed":
			ended, errored = true, false // 不动 failed：只有新开一轮才算翻过去
			if e.Usage != nil {
				usage = backend.AddUsage(usage, &backend.JobUsage{
					Input: e.Usage.Input - e.Usage.Cached, CacheRead: e.Usage.Cached, CacheWrite: e.Usage.CacheWrite,
					Output: e.Usage.Output + e.Usage.Reasoning, CostUnknown: true,
				})
			}
		case "turn.failed":
			ended, failed, errored = true, true, false
			if failure = errorText(e.Error); failure == "" {
				failure = noReason
			}
		case "error":
			if text := errorText(e.Message); !strings.HasPrefix(text, "Reconnecting") {
				lastError, errored = text, true
				if lastError == "" {
					lastError = noReason
				}
			}
		case "item.completed":
			errored = false
			if e.Item != nil && e.Item.Type == "agent_message" {
				last, turnMessage = e.Item.Text, e.Item.Text
			}
		}
	})
	out.Usage = usage
	stderr := fsx.TailLines(filepath.Join(job.Dir, "stderr.log"), 5)
	channel := failure
	switch {
	case err != nil: // 输出都没有：起不来，错误在 stderr
		channel = stderr
	case errored && !failed:
		channel, failure = lastError, lastError
		ended, failed = true, true
	case !ended:
		channel = stderr + "\n" + lastError
	}
	out.HTTPStatus = httpStatus(channel)
	switch {
	case usageLimitRe.MatchString(channel) || out.HTTPStatus == 429:
		out.LimitStructured = out.HTTPStatus == 429
		out.LimitText = true
		out.ResetAt = resetAt(channel, exit)
	case authFailed(channel, out.HTTPStatus):
		out.Stop = backend.StopAuth
	}
	if !ended {
		if out.Stop == backend.StopAuth { // 认证失败不算没有终态：收尾要认出它
			out.HaveTerminal, out.Failed, out.Text = true, true, strings.TrimSpace(channel)
		}
		return out
	}
	out.HaveTerminal = true
	out.Failed = failed
	if failed {
		out.Text = failure // 失败的结果不产出结构化输出：前面那条消息可能写着 pass
		return out
	}
	out.Text = last
	if m := strings.TrimSpace(turnMessage); strings.HasPrefix(m, "{") && json.Valid([]byte(m)) {
		out.Structured = json.RawMessage(m)
	}
	return out
}

// 错误文字来自 codex-rs/protocol/src/error.rs。撇号是弯的（You’ve），不写进正则。
var (
	usageLimitRe = regexp.MustCompile(`(?i)hit your usage limit|usage limit reached|out of credits|hit your spend cap|quota exceeded|rate limit exceeded`)
	httpStatusRe = regexp.MustCompile(`(?:unexpected status|last status:?|"status":)\s*(\d{3})`)
	authRe       = regexp.MustCompile(`(?i)refresh token|sign in again|log in again|not logged in|To use Codex with your ChatGPT plan`)
)

func httpStatus(text string) int {
	m := httpStatusRe.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return 0
	}
	n, err := strconv.Atoi(m[len(m)-1][1])
	if err != nil {
		return 0
	}
	return n
}

// authFailed：和 Claude 一样只认 401 和明确的文字。403 这类权限错误走普通失败：可能只是这个模型没开通，暂停整个后端太重。
func authFailed(text string, status int) bool {
	return status == 401 || authRe.MatchString(text)
}

// resetAtRe：Try again at 3:05 PM. 或 try again at Mar 2nd, 2026 3:05 PM.（本地时间，同一天不带日期）
var resetAtRe = regexp.MustCompile(`(?i)try again at (?:([A-Z][a-z]{2} \d{1,2})(?:st|nd|rd|th), (\d{4}) )?(\d{1,2}:\d{2} [AP]M)`)

// resetSlack：到了提示的时刻再多等一分钟，本机时钟可能比服务端快一点。
const resetSlack = time.Minute

func resetAt(text string, exit backend.Exit) time.Time {
	if exit.At.IsZero() {
		return time.Time{}
	}
	t, ok := parseResetTime(text, exit.At)
	if !ok {
		return time.Time{}
	}
	return t
}

// parseResetTime 解析文字里的重置时刻，now 是 CLI 退出的时刻。只有钟点的算今天，已经过了（跨了午夜）算明天。
func parseResetTime(text string, now time.Time) (time.Time, bool) {
	m := resetAtRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	loc := now.Location()
	if m[1] != "" {
		t, err := time.ParseInLocation("Jan 2 2006 3:04 PM", m[1]+" "+m[2]+" "+m[3], loc)
		if err != nil || t.Before(now.Add(-time.Hour)) || t.After(now.Add(8*24*time.Hour)) {
			return time.Time{}, false
		}
		return t.Add(resetSlack), true
	}
	clock, err := time.ParseInLocation("3:04 PM", m[3], loc)
	if err != nil {
		return time.Time{}, false
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if t.Before(now.Add(-time.Hour)) {
		t = t.AddDate(0, 0, 1)
	}
	return t.Add(resetSlack), true
}

// LastActivity：事件流是实时追加的，看 agentq 自己保存的文件，不碰 Codex 的内部文件。顺手记下大小和时刻，ToolEvents 要用：事件没有时间戳。
func (b *Backend) LastActivity(job backend.JobRef) (time.Time, bool) {
	backend.NoteSeen(job.Dir, resultFile, time.Now())
	fi, err := os.Stat(filepath.Join(job.Dir, resultFile))
	if err != nil {
		return time.Time{}, true
	}
	return fi.ModTime(), true
}

// ToolEvents 读事件流里做完的工具调用，时间取首次被 tick 看到的时刻（误差最多一个 tick）。工具名映射成规范名。
// Codex 的子代理（collab_tool_call）记在同一条事件流里，没有单独的记录，subs 总是空的，「子代理失控」看不到。
func (b *Backend) ToolEvents(job backend.JobRef, withSubs bool) ([]backend.ToolEvent, [][]backend.ToolEvent, error) {
	backend.NoteSeen(job.Dir, resultFile, time.Now())
	if fi, err := os.Stat(filepath.Join(job.Dir, resultFile)); err == nil && fi.Size() > maxToolLogBytes { // 每个 tick 都读，太大的当未知，不拖着 tick 的锁
		return nil, nil, fmt.Errorf("事件流 %d 字节，太大，不读", fi.Size())
	}
	var main []backend.ToolEvent
	started := false
	err := scan(job.Dir, func(off int64, e event) {
		if e.Type == "thread.started" {
			started = true
		}
		if e.Type != "item.completed" || e.Item == nil {
			return
		}
		name := toolName(e.Item.Type)
		if name == "" {
			return
		}
		at, err := backend.SeenAt(job.Dir, resultFile, off)
		if err != nil {
			return
		}
		ev := backend.ToolEvent{At: at, Name: name}
		if name == "Bash" {
			ev.Cmd = e.Item.Command
		}
		main = append(main, ev)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("读事件流：%w", err)
	}
	if !started {
		return nil, nil, errors.New("事件流里没有 thread.started，格式可能变了")
	}
	return main, nil, nil
}

func toolName(itemType string) string {
	switch itemType {
	case "command_execution":
		return "Bash"
	case "file_change":
		return "Edit"
	case "collab_tool_call":
		return "Agent"
	case "mcp_tool_call", "web_search":
		return "Other"
	}
	return ""
}
