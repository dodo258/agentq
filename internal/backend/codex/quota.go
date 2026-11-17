package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/proc"
)

// 额度从 `codex app-server` 读：stdio 上一行一条 JSON-RPC（不带 "jsonrpc" 字段），先 initialize、initialized，
// 再 account/rateLimits/read（https://learn.chatgpt.com/docs/app-server）。这个方法只查 ChatGPT 账号的用量，不开会话、不调模型。
// 返回的 rateLimits 是主额度桶，primary、secondary 各是一个窗口（usedPercent、windowDurationMins、resetsAt 秒），
// 账号没有的窗口是 null（实测 codex-cli 0.169.0 的 Pro 账号只有一个 10080 分钟的周窗口）。
const quotaReqID = 1

// quotaRequest：excludeResetCreditDetails 是给后台轮询用的，省掉额外的重置券明细查询（协议 schema 的说明）。
var quotaRequest = `{"method":"initialize","id":0,"params":{"clientInfo":{"name":"agentq","title":"agentq","version":"1"}}}
{"method":"initialized","params":{}}
{"method":"account/rateLimits/read","id":1,"params":{"excludeResetCreditDetails":true}}
`

const (
	quotaWait     = 30 * time.Second // 整个读额度最多等多久
	quotaExitWait = 3 * time.Second  // 拿到结果、关了标准输入后等 app-server 自己退出多久，过了就杀
)

// QuotaText 实现 backend.QuotaReader：起一次 app-server，返回 account/rateLimits/read 那条响应的原文。
// 在空的临时目录里跑，环境照任务的白名单（只带 CODEX_HOME，不带 API Key，不会换成 API 计费）。
func (b *Backend) QuotaText(ctx context.Context) (string, error) {
	tmp, err := os.MkdirTemp("", "agentq-codex-quota-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, quotaWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.path(), "app-server")
	cmd.Dir = tmp
	cmd.Env = proc.BackendEnv(envKeep)
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out := &respWriter{found: make(chan string, 1)}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动 codex app-server 失败：%w", err)
	}
	waitc := make(chan error, 1)
	go func() { waitc <- cmd.Wait() }()
	_, _ = stdin.Write([]byte(quotaRequest)) // 进程先退了写不进去，下面按退出处理

	var line string
	var waitErr error
	exited := false
	select {
	case line = <-out.found:
	case waitErr = <-waitc:
		exited = true
		select {
		case line = <-out.found:
		default:
		}
	}
	stdin.Close() // 标准输入关了 app-server 自己退出
	if !exited {
		select {
		case <-waitc:
		case <-time.After(quotaExitWait):
			proc.ClearLeftovers(cmd)
			<-waitc
		}
	}
	if line != "" {
		return line, nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("codex app-server 读额度超时（%v）", quotaWait)
	}
	msg := oneLine(strings.TrimSpace(stderr.String()), 200)
	if waitErr != nil {
		msg = strings.TrimSuffix(waitErr.Error()+"："+msg, "：")
	}
	return "", errors.New("codex app-server 没返回额度：" + msg)
}

// respWriter 接 app-server 的标准输出，按行找 account/rateLimits/read 的响应（id 对上、没有 method；带 method 的是服务端发来的请求或通知）。
// 只由 exec 拷贝输出的那一个 goroutine 写。
type respWriter struct {
	buf   []byte
	found chan string
	done  bool
}

func (w *respWriter) Write(p []byte) (int, error) {
	if w.done {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := bytes.TrimSpace(w.buf[:i])
		w.buf = w.buf[i+1:]
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &m) == nil && m.Method == "" && string(m.ID) == fmt.Sprint(quotaReqID) {
			w.done, w.buf = true, nil
			w.found <- string(line)
			return len(p), nil
		}
	}
}

type rateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

// sessionMaxMins：不超过一天的窗口按会话窗口算（Codex 是 5 小时），用 usage_stop_session_pct；更长的按周窗口，用 usage_stop_week_pct。
const sessionMaxMins = 24 * 60

// ParseQuota 实现 backend.QuotaReader：按实际返回的窗口算，账号没有的窗口不编。一个窗口都没有、报错、看不懂都返回错误（调用方当未知）。
func (b *Backend) ParseQuota(text string, now time.Time) ([]backend.UsageWindow, error) {
	var resp struct {
		Result *struct {
			RateLimits *struct {
				Primary   *rateWindow `json:"primary"`
				Secondary *rateWindow `json:"secondary"`
			} `json:"rateLimits"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &resp); err != nil {
		return nil, fmt.Errorf("看不懂 codex app-server 的额度响应：%s", oneLine(text, 120))
	}
	if resp.Error != nil {
		return nil, errors.New("codex app-server 读额度报错：" + oneLine(resp.Error.Message, 200))
	}
	if resp.Result == nil || resp.Result.RateLimits == nil {
		return nil, fmt.Errorf("codex app-server 的额度响应里没有 rateLimits：%s", oneLine(text, 120))
	}
	var out []backend.UsageWindow
	for i, w := range []*rateWindow{resp.Result.RateLimits.Primary, resp.Result.RateLimits.Secondary} {
		if w == nil {
			continue
		}
		// 没写窗口长度时按老的约定：primary 是 5 小时，secondary 是周。
		label := "week"
		if d := w.WindowDurationMins; d != nil && *d > 0 {
			if *d <= sessionMaxMins {
				label = "session"
			}
		} else if i == 0 {
			label = "session"
		}
		uw := backend.UsageWindow{Label: label, Used: w.UsedPercent}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			t := time.Unix(*w.ResetsAt, 0).In(now.Location())
			uw.ResetsAt = &t
		}
		out = append(out, uw)
	}
	if len(out) == 0 {
		return nil, errors.New("codex app-server 没返回任何额度窗口")
	}
	return out, nil
}

// oneLine：报错原文压成一行、最多 n 个字，写进 status 和日志。
func oneLine(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
