package backend

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentq/internal/textfmt"
)

// NewSessionID 生成一个随机 UUID（v4），启动时预先指定会话，tick 才能找到会话记录看它有没有在长。
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// 看进展的阈值，来历见 internal/runner/progress.go。
const RunawayLogs = 25

const runawayNested = 8

const noChangeFor = 90 * time.Minute

const RepeatCommand = 6

const mainIdleFor = 45 * time.Minute

const SigRunaway = "runaway"

const SigNoChange = "no_change"

const SigRepeat = "repeat"

const SigMainIdle = "main_idle"

var ProgressNames = map[string]string{
	SigRunaway:  "子代理失控",
	SigNoChange: "无改动",
	SigRepeat:   "原地重复",
	SigMainIdle: "主会话空等",
}

// ToolEvent 是任务的一次工具调用。Name 用规范名：改文件 Edit、Write、MultiEdit、NotebookEdit，跑命令 Bash，派子代理 Agent、Task。
// 工具名不一样的后端在 ToolLog 里映射成这些。
type ToolEvent struct {
	At   time.Time
	Name string
	Cmd  string // Bash 的命令
}

var gitCommitRe = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)

func isEditTool(name string) bool {
	return name == "Edit" || name == "Write" || name == "MultiEdit" || name == "NotebookEdit"
}

func IsAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// ProgressHit 是一个命中的信号和它的证据。
type ProgressHit struct {
	Signal   string
	Evidence string
}

// EvalProgress 按 now 这一刻判断四种信号。main 是主会话的工具调用，subs 是各子代理日志的工具调用；
// 早于 start 的调用不算（续跑接着写旧会话）。纯函数，真实历史回测也用它按时间回放。
func EvalProgress(main []ToolEvent, subs [][]ToolEvent, start, now time.Time) []ProgressHit {
	within := func(list []ToolEvent) []ToolEvent {
		var out []ToolEvent
		for _, e := range list {
			if !e.At.Before(start) && !e.At.After(now) {
				out = append(out, e)
			}
		}
		return out
	}
	main = within(main)
	logs, nested := 0, 0
	lastChange, lastSub := start, time.Time{}
	note := func(list []ToolEvent) {
		for _, e := range list {
			if isEditTool(e.Name) || gitCommitRe.MatchString(e.Cmd) {
				if e.At.After(lastChange) {
					lastChange = e.At
				}
			}
		}
	}
	note(main)
	for _, s := range subs {
		s = within(s)
		if len(s) == 0 {
			continue
		}
		logs++
		note(s)
		if last := s[len(s)-1].At; last.After(lastSub) {
			lastSub = last
		}
		for _, e := range s {
			if IsAgentTool(e.Name) {
				nested++
				break
			}
		}
	}

	var hits []ProgressHit
	if logs >= RunawayLogs || nested >= runawayNested {
		hits = append(hits, ProgressHit{SigRunaway, fmt.Sprintf("子代理 %d 个，其中 %d 个再派子代理", logs, nested)})
	}
	if d := now.Sub(lastChange); d >= noChangeFor {
		hits = append(hits, ProgressHit{SigNoChange, fmt.Sprintf("主会话和子代理合计 %s 没有改文件或提交", textfmt.FormatIdle(d))})
	}
	// 证据要写进汇总和推送，命令里可能有密钥，只报次数；命令本身在任务的会话记录里
	if n := repeatedCommand(main); n >= RepeatCommand {
		hits = append(hits, ProgressHit{SigRepeat, fmt.Sprintf("同一条命令跑了 %d 次，中间没改文件", n)})
	}
	lastMain := start
	if len(main) > 0 {
		lastMain = main[len(main)-1].At
	}
	// 子代理也停了就是完全静止，归现有的「疑似卡死」
	if d := now.Sub(lastMain); d >= mainIdleFor && lastSub.After(lastMain) && now.Sub(lastSub) < mainIdleFor {
		hits = append(hits, ProgressHit{SigMainIdle, fmt.Sprintf("主会话 %s 没有工具调用，子代理 %s 前还在动", textfmt.FormatIdle(d), textfmt.FormatIdle(now.Sub(lastSub)))})
	}
	return hits
}

// repeatedCommand 返回没被 Edit/Write 打断的情况下，同一条 Bash 命令（压缩空白后比较）最多重复了几次。
func repeatedCommand(events []ToolEvent) int {
	counts := map[string]int{}
	best := 0
	for _, e := range events {
		if isEditTool(e.Name) {
			clear(counts)
			continue
		}
		if e.Name != "Bash" {
			continue
		}
		cmd := strings.Join(strings.Fields(e.Cmd), " ")
		if cmd == "" {
			continue
		}
		counts[cmd]++
		best = max(best, counts[cmd])
	}
	return best
}
