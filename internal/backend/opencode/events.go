package opencode

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

const resultFile = "result.json" // 标准输出，一行一个事件

// maxEventBytes：一行事件的上限，工具输出整段在 part 里。太长的行当没看到。
const maxEventBytes = 32 << 20

// maxToolLogBytes：看进展读事件流的上限，每个 tick 都读。
const maxToolLogBytes = 50 << 20

// event 是 run --format json 的一行：{type, timestamp, sessionID, ...}。part、error 不写死类型：字段长得不对也不能让这一行被整行丢掉。
type event struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Error     json.RawMessage `json:"error"`
}

type part struct {
	Text   string  `json:"text"`
	Reason string  `json:"reason"`
	Tool   string  `json:"tool"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		Reasoning float64 `json:"reasoning"`
		Cache     struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	State struct {
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	} `json:"state"`
}

func (e event) part() part {
	var p part
	if len(e.Part) > 0 {
		_ = json.Unmarshal(e.Part, &p) // 解不开就是空的：这一行还算一个事件
	}
	return p
}

// apiError 是 error 事件的内容：{name, data:{message, statusCode, responseBody, responseHeaders}}。
type apiError struct {
	Name string `json:"name"`
	Data struct {
		Message      string            `json:"message"`
		StatusCode   int               `json:"statusCode"`
		ResponseBody string            `json:"responseBody"`
		Headers      map[string]string `json:"responseHeaders"`
	} `json:"data"`
}

// noReason：错误事件没给原因时的说明。
const noReason = "opencode 报告这一轮失败，没有给原因"

func parseError(raw json.RawMessage) (apiError, string) {
	var e apiError
	if json.Unmarshal(raw, &e) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return e, s
		}
		return e, noReason
	}
	switch {
	case e.Data.Message != "":
		return e, e.Data.Message
	case e.Name != "":
		return e, e.Name
	}
	return e, noReason
}

// scan 逐行读事件流，不是 JSON 的行跳过：被杀时最后一行可能只写了一半。
func scan(dir string, fn func(e event)) error {
	f, err := os.Open(filepath.Join(dir, resultFile))
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && len(line) <= maxEventBytes {
			var e event
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				fn(e)
			}
		}
		if err != nil {
			return nil
		}
	}
}

// usageOf 把一个 step_finish 记成用量。金额是 opencode 按模型价目表算的，订阅和本地模型是 0：算未知，不当成没花钱。
func usageOf(p part) *backend.JobUsage {
	return &backend.JobUsage{
		Input: int64(p.Tokens.Input), CacheRead: int64(p.Tokens.Cache.Read), CacheWrite: int64(p.Tokens.Cache.Write),
		Output: int64(p.Tokens.Output + p.Tokens.Reasoning), CostUSD: p.Cost,
	}
}

func markCostUnknown(u *backend.JobUsage) *backend.JobUsage {
	if u != nil && u.CostUSD == 0 {
		u.CostUnknown = true
	}
	return u
}

// loopContinues：finish 是这些时 opencode 的循环还要接着走，不算结束（session/prompt.ts）。
func loopContinues(reason string) bool {
	return reason == "tool-calls" || reason == "unknown" || reason == ""
}

// Parse：没有单独的终态事件。最后一个 step_finish 的 reason 说明循环结束了算成功的终态，error 事件算失败的终态。
// 官方 issue 记过 run 可能丢最后的 step_finish 还退 0（#26855、#53207，后者 2026-10-30 才关）：这时没有终态，不猜成功，用量也少最后一步，调度层只看 BACKLOG。
// 撞限和认证只看错误通道（error 事件、stderr），不看模型正文。
func (b *Backend) Parse(job backend.JobRef, exit backend.Exit) backend.Result {
	var out backend.Result
	var last, stepText, failure string
	var failed apiError
	var usage *backend.JobUsage
	ended, isFailed := false, false
	err := scan(job.Dir, func(e event) {
		if e.SessionID != "" && out.SessionID == "" {
			out.SessionID = e.SessionID
		}
		switch e.Type {
		case "step_start":
			ended, isFailed, failure, stepText, failed = false, false, "", "", apiError{}
		case "text":
			if t := e.part().Text; t != "" {
				last, stepText = t, t
			}
		case "tool_use":
			ended = false
		case "step_finish":
			p := e.part()
			usage = backend.AddUsage(usage, usageOf(p))
			out.Turns++
			if !isFailed {
				ended = !loopContinues(p.Reason)
			}
		case "error":
			failed, failure = parseError(e.Error)
			ended, isFailed = true, true
		}
	})
	out.Usage = markCostUnknown(usage)
	stderr := fsx.TailLines(filepath.Join(job.Dir, "stderr.log"), 5)
	channel := failure + "\n" + failed.Data.ResponseBody
	if err != nil || !ended {
		channel = stderr + "\n" + channel // 没开工就没事件，错误在 stderr（模型找不到、认证）
	}
	out.HTTPStatus = failed.Data.StatusCode
	switch {
	case usageLimitRe.MatchString(channel) || out.HTTPStatus == 429:
		out.LimitStructured = out.HTTPStatus == 429
		out.LimitText = true
		out.ResetAt = retryAfter(failed.Data.Headers, exit)
	case failed.Name == "ProviderAuthError" || out.HTTPStatus == 401 || authRe.MatchString(channel):
		out.Stop = backend.StopAuth
	}
	if !ended {
		if out.Stop == backend.StopAuth { // 认证失败不算没有终态：收尾要认出它
			out.HaveTerminal, out.Failed, out.Text = true, true, strings.TrimSpace(channel)
		}
		return out
	}
	out.HaveTerminal = true
	out.Failed = isFailed
	if isFailed {
		out.Text = failure // 失败的结果不产出结构化输出：前面那条消息可能写着 pass
		return out
	}
	out.Text = last
	if m := strings.TrimSpace(stepText); strings.HasPrefix(m, "{") && json.Valid([]byte(m)) {
		out.Structured = json.RawMessage(m)
	}
	return out
}

// 文字来自 session/retry.ts 认的那些（rate limit、too many requests、resource exhausted、FreeUsageLimitError），加常见的额度字样。模型来源很多，认不出就落到普通失败和熔断。
var (
	usageLimitRe = regexp.MustCompile(`(?i)rate.?limit|too many requests|quota exceeded|insufficient_quota|usage limit|resource.?exhausted|FreeUsageLimitError`)
	authRe       = regexp.MustCompile(`(?i)unauthorized|invalid api key|invalid x-api-key|authentication failed|not authenticated|no credentials`)
)

// retryAfter 读响应头里的重试时间（和 session/retry.ts 同样的两个头）。日期写法不解析，按 limit_cooldown。
func retryAfter(h map[string]string, exit backend.Exit) time.Time {
	if exit.At.IsZero() {
		return time.Time{}
	}
	if ms, err := strconv.ParseFloat(h["retry-after-ms"], 64); err == nil && ms > 0 {
		return exit.At.Add(time.Duration(ms) * time.Millisecond)
	}
	if s, err := strconv.ParseFloat(h["retry-after"], 64); err == nil && s > 0 {
		return exit.At.Add(time.Duration(s) * time.Second)
	}
	return time.Time{}
}

// LastActivity：事件流是实时追加的，看 agentq 自己保存的文件。
func (b *Backend) LastActivity(job backend.JobRef) (time.Time, bool) {
	fi, err := os.Stat(filepath.Join(job.Dir, resultFile))
	if err != nil {
		return time.Time{}, true
	}
	return fi.ModTime(), true
}

// LiveUsage 实现 backend.UsageStream：step_finish 运行中就有，job_budget_usd 在 tick 里靠它停任务，最多滞后一个 tick。
func (b *Backend) LiveUsage(job backend.JobRef) *backend.JobUsage {
	var usage *backend.JobUsage
	if err := scan(job.Dir, func(e event) {
		if e.Type == "step_finish" {
			usage = backend.AddUsage(usage, usageOf(e.part()))
		}
	}); err != nil {
		return nil
	}
	return markCostUnknown(usage)
}

// ToolEvents 读事件流里做完的工具调用，时间是事件自带的毫秒时间戳。run 只输出主会话的事件（子代理的被它滤掉了），subs 总是空的，「子代理失控」看不到。
func (b *Backend) ToolEvents(job backend.JobRef, _ bool) ([]backend.ToolEvent, [][]backend.ToolEvent, error) {
	if fi, err := os.Stat(filepath.Join(job.Dir, resultFile)); err == nil && fi.Size() > maxToolLogBytes {
		return nil, nil, fmt.Errorf("事件流 %d 字节，太大，不读", fi.Size())
	}
	var main []backend.ToolEvent
	started := false
	err := scan(job.Dir, func(e event) {
		if e.SessionID != "" {
			started = true
		}
		if e.Type != "tool_use" {
			return
		}
		p := e.part()
		name := toolName(p.Tool)
		ev := backend.ToolEvent{At: time.UnixMilli(e.Timestamp), Name: name}
		if name == "Bash" {
			ev.Cmd = p.State.Input.Command
		}
		main = append(main, ev)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("读事件流：%w", err)
	}
	if !started {
		return nil, nil, errors.New("事件流里没有 sessionID，格式可能变了")
	}
	return main, nil, nil
}

func toolName(tool string) string {
	switch tool {
	case "bash":
		return "Bash"
	case "edit", "apply_patch", "patch", "multiedit":
		return "Edit"
	case "write":
		return "Write"
	case "task":
		return "Agent"
	case "read":
		return "Read"
	case "grep":
		return "Grep"
	case "glob":
		return "Glob"
	}
	return "Other"
}
