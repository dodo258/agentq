package pi

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

// maxEventBytes：一行事件的上限。agent_end 带着整轮的消息，可能很长，太长的行当没看到。
const maxEventBytes = 32 << 20

// maxToolLogBytes：看进展读事件流的上限，和 Codex 一样。
const maxToolLogBytes = 50 << 20

// event 是 JSON 模式的一行（json.md）。只解要用的字段。
type event struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`      // session 头
	Aborted  bool            `json:"aborted"` // agent_settled
	Message  *message        `json:"message"` // message_end
	ToolName string          `json:"toolName"`
	Args     json.RawMessage `json:"args"` // tool_execution_start
	Result   *struct {       // compaction_end
		Usage *usage `json:"usage"`
	} `json:"result"`
}

// message 是 message-types.md 的 AgentMessage。Content 不写死类型：用户消息是字符串或数组，助手消息是数组。
type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        *usage          `json:"usage"`
}

type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"` // 已经含 reasoning
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// text 是助手消息里的文字块，思考块不算。
func (m *message) text() string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
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

// run 是一次运行从事件流里读出来的。
type run struct {
	session  string
	last     *message // 最后一条助手消息
	settled  bool
	aborted  bool
	turns    int
	usage    *backend.JobUsage
	anyCost  bool
	anyToken bool
}

func (r *run) add(u *usage) {
	if u == nil {
		return
	}
	r.usage = backend.AddUsage(r.usage, &backend.JobUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.Cost.Total})
	r.anyCost = r.anyCost || u.Cost.Total > 0
	r.anyToken = r.anyToken || u.Input+u.Output+u.CacheRead+u.CacheWrite > 0
}

// read 读整条事件流。重试（auto_retry_*）时前面失败的那条助手消息也在流里，以最后一条为准；每条助手消息的用量是那一次调用的，加起来。
func read(dir string) (run, error) {
	var r run
	err := scan(dir, func(_ int64, e event) {
		switch e.Type {
		case "session":
			if r.session == "" {
				r.session = e.ID
			}
		case "message_end":
			if e.Message != nil && e.Message.Role == "assistant" {
				r.last = e.Message
				r.add(e.Message.Usage)
			}
		case "turn_end":
			r.turns++
		case "compaction_end":
			if e.Result != nil {
				r.add(e.Result.Usage)
			}
		case "agent_settled":
			r.settled, r.aborted = true, e.Aborted
		}
	})
	if r.usage != nil && r.anyToken && !r.anyCost { // pi 按模型价目表算金额，价目表里没有这个模型就一直是 0：当未知，不当免费
		r.usage.CostUnknown = true
	}
	return r, err
}

// Parse：agent_settled 是终态（json.md：之后没有自动重试和排队的活）。成功只认最后一条助手消息 stopReason=stop 且没有中止；
// 失败、中止、截断都只在事件里，退出码常是 0（实测），不看退出码。进程被杀（实测 SIGTERM 退 143）没有 agent_settled，不猜成功。
// 撞限和认证只看错误通道：助手消息的 errorMessage（pi 写成「状态码: 原文」，实测）和 stderr，不看模型正文。
func (b *Backend) Parse(job backend.JobRef, exit backend.Exit) backend.Result {
	var out backend.Result
	r, err := read(job.Dir)
	out.SessionID, out.Usage, out.Turns = r.session, r.usage, r.turns
	stderr := fsx.TailLines(filepath.Join(job.Dir, "stderr.log"), 5)
	channel := stderr
	switch {
	case err == nil && r.settled && !r.aborted && r.last != nil && r.last.StopReason == "stop":
		channel = "" // 正常结束：stderr 里有扩展转过来的输出，正好带「rate limit」这类字也不算撞限或认证失败
	case err == nil && r.last != nil && r.last.StopReason != "stop":
		channel = r.last.ErrorMessage + "\n" + stderr
	}
	out.HTTPStatus = httpStatus(channel)
	out.GatewayChallenge = backend.GatewayChallenge(channel)
	switch {
	case out.HTTPStatus == 429 || usageLimitRe.MatchString(channel):
		out.LimitStructured = out.HTTPStatus == 429
		out.LimitText = true
	case (out.HTTPStatus == 401 || authRe.MatchString(channel)) && !out.GatewayChallenge:
		out.Stop = backend.StopAuth
	}
	if !r.settled {
		if out.Stop == backend.StopAuth { // 认证失败不算没有终态：收尾要认出它（没有凭据时 pi 只写 session 头和 stderr 就退出，实测）
			out.HaveTerminal, out.Failed, out.Text = true, true, strings.TrimSpace(channel)
		}
		return out
	}
	out.HaveTerminal = true
	switch {
	case r.last == nil:
		out.Failed, out.Text = true, "pi 结束了，但没有一条回答"
	case r.aborted || r.last.StopReason == "aborted":
		out.Failed, out.Text = true, "pi 这次运行被中止了"
	case r.last.StopReason == "error":
		out.Failed, out.Text = true, strings.TrimSpace(r.last.ErrorMessage)
		if out.Text == "" {
			out.Text = "pi 报告这次回答出错，没有给原因"
		}
	case r.last.StopReason == "length":
		out.Failed, out.Text = true, "回答到了模型的输出长度上限，被截断了（stopReason=length）：\n"+r.last.text()
	case r.last.StopReason != "stop":
		out.Failed, out.Text = true, "pi 的最后一条回答没有正常结束（stopReason="+r.last.StopReason+"）"
	}
	if out.Failed { // 失败的结果不产出结构化输出
		return out
	}
	out.Text = r.last.text()
	if js, ok := wholeJSON(out.Text); ok {
		out.Structured = js
	}
	return out
}

// wholeJSON：整条消息就是一个 JSON 对象（可以包在一个 ``` 代码块里）。只认整条：从文字里抠的话，diff 里一段伪造的 JSON 就能当结论。
func wholeJSON(text string) (json.RawMessage, bool) {
	m := strings.TrimSpace(text)
	if strings.HasPrefix(m, "```") && strings.HasSuffix(m, "```") && len(m) > 6 {
		m = strings.TrimSpace(strings.TrimSuffix(m, "```"))
		_, body, ok := strings.Cut(m, "\n") // 去掉第一行的 ``` 或 ```json
		if !ok {
			return nil, false
		}
		m = strings.TrimSpace(body)
	}
	if !strings.HasPrefix(m, "{") || !json.Valid([]byte(m)) {
		return nil, false
	}
	return json.RawMessage(m), true
}

// 错误文字：pi 把上游的错误写成「状态码: 原文」（实测 openai-completions 的 401、429、500），没有凭据时 stderr 是 No API key found for <提供商>（实测）。
var (
	httpStatusRe = regexp.MustCompile(`(?m)^(\d{3}):`)
	usageLimitRe = regexp.MustCompile(`(?i)usage limit|rate limit|quota exceeded|exceeded your current quota|insufficient_quota|out of credits`)
	authRe       = regexp.MustCompile(`(?i)no api key found|invalid api key|incorrect api key|invalid_api_key|missing authentication|authentication_error|not logged in|log ?in again`)
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

// LastActivity：事件流是实时追加的，看 agentq 自己保存的文件，不碰 pi 的会话文件。顺手记下大小和时刻，ToolEvents 要用：事件没有时间戳。
func (b *Backend) LastActivity(job backend.JobRef) (time.Time, bool) {
	backend.NoteSeen(job.Dir, resultFile, time.Now())
	fi, err := os.Stat(filepath.Join(job.Dir, resultFile))
	if err != nil {
		return time.Time{}, true
	}
	return fi.ModTime(), true
}

// LiveUsage 实现 backend.UsageStream：运行中已经结束的回答花了多少。tick 靠它替没有美元上限的 pi 看预算。
func (b *Backend) LiveUsage(job backend.JobRef) *backend.JobUsage {
	if fi, err := os.Stat(filepath.Join(job.Dir, resultFile)); err != nil || fi.Size() > maxToolLogBytes {
		return nil
	}
	r, err := read(job.Dir)
	if err != nil {
		return nil
	}
	return r.usage
}

// ToolEvents 读事件流里开始的工具调用，时间取首次被 tick 看到的时刻（误差最多一个 tick）。工具名映射成规范名。
// pi 没有子代理，subs 总是空的。
func (b *Backend) ToolEvents(job backend.JobRef, withSubs bool) ([]backend.ToolEvent, [][]backend.ToolEvent, error) {
	backend.NoteSeen(job.Dir, resultFile, time.Now())
	if fi, err := os.Stat(filepath.Join(job.Dir, resultFile)); err == nil && fi.Size() > maxToolLogBytes { // 每个 tick 都读，太大的当未知，不拖着 tick 的锁
		return nil, nil, fmt.Errorf("事件流 %d 字节，太大，不读", fi.Size())
	}
	var main []backend.ToolEvent
	started := false
	err := scan(job.Dir, func(off int64, e event) {
		if e.Type == "session" {
			started = true
		}
		if e.Type != "tool_execution_start" {
			return
		}
		at, err := backend.SeenAt(job.Dir, resultFile, off)
		if err != nil {
			return
		}
		ev := backend.ToolEvent{At: at, Name: toolName(e.ToolName)}
		if ev.Name == "Bash" {
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(e.Args, &a) == nil {
				ev.Cmd = a.Command
			}
		}
		main = append(main, ev)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("读事件流：%w", err)
	}
	if !started {
		return nil, nil, errors.New("事件流里没有 session 头，格式可能变了")
	}
	return main, nil, nil
}

// toolName：内置工具见 cli.md 的工具表。读和找的工具不影响进展判断，都记 Other。
func toolName(name string) string {
	switch name {
	case "bash", "powershell":
		return "Bash"
	case "edit":
		return "Edit"
	case "write":
		return "Write"
	}
	return "Other"
}
