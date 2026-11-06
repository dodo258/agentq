// Package notify 把通知排队后送达：notify.json、限流合并、早报、决策卡片发送。notify.json 里的 Inbox、Replies、Threads、Cards 归 chat 用，但文件和锁只有一个，类型留在这里。
package notify

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是通知的模块实例。NotifyHeld 是 notify.lock 的重入标记，所以每个进程只能有一份。
type Service struct {
	*env.Env
	Backlog  *backlog.Service
	Telegram *telegram.Service
	// MacNotify 为空时用 osascript 弹本机通知。测试里替换掉。
	MacNotify func(title, body string) error
	// MorningIssues 给早报「出问题」一节提供标题，组装时接到巡逻模块。
	MorningIssues func() []string
	// MorningGates 给早报「门」一节提供行：放行、放弃的审计行，warn 没过的，关着的门。组装时接到门模块，只写事实。
	MorningGates func(st *state.State, now time.Time) []string
	// notifyHeld：本进程正拿着 notify.lock 时的推送状态（见 withNotify）。
	notifyHeld *State
	// filesOut：推送已发出、附件等放开 notify.lock 再发（见 queueNoticeFiles）。
	filesOut []string
}

// Decisions 是 scanDecisions 去掉被排除的项目。
func (a *Service) Decisions() ([]Decision, []string) {
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return nil, []string{"扫描项目目录失败：" + err.Error()}
	}
	return scanDecisionsIn(projects)
}

func WriteInMsgs(b *strings.Builder, inbox []InMsg) {
	for i, m := range inbox {
		fmt.Fprintf(b, "%d. [%s] %s\n", i+1, textfmt.WhenCN(m.At), m.Text)
		if m.Note != "" {
			fmt.Fprintf(b, "   （%s）\n", m.Note)
		}
		if m.ReplyTo != "" {
			fmt.Fprintf(b, "   （这条是回复 agentq 之前发的这条消息：%s）\n", textfmt.OneLine(m.ReplyTo, 600))
		}
		for _, f := range m.Files {
			fmt.Fprintf(b, "   附件：%s\n", f)
		}
	}
}

// ChatReplyTo：对话任务回复的是哪条消息（启动时写在任务目录的 reply-to 里），读不到就不挂回复。
func (a *Service) ChatReplyTo(j *state.Job) int64 {
	b, err := os.ReadFile(filepath.Join(a.JobDir(j.ID), "reply-to"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// NotifyFile：agentq notify file <路径> [说明]，马上把文件发给用户（对话任务里回图片、录屏用，不进推送队列）。
// 在对话任务里执行时（环境变量 AGENTQ_JOB 是那个任务），挂在用户那条消息下面。
func (a *Service) NotifyFile(args []string) error {
	if len(args) < 1 || len(args) > 2 || strings.TrimSpace(args[0]) == "" {
		return errors.New("要写文件路径\n" + NotifyUsage)
	}
	path, err := filepath.Abs(fsx.ExpandHome(a.Home, args[0]))
	if err != nil {
		return err
	}
	caption := ""
	if len(args) == 2 {
		caption = strings.TrimSpace(args[1])
	}
	c, err := a.Telegram.TelegramCreds()
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("没配 Telegram，发不了文件（agentq notify setup 配）；文件在 %s", path)
	}
	var replyTo int64
	if id := os.Getenv(proc.JobEnv); id != "" {
		replyTo = a.ChatReplyTo(&state.Job{ID: filepath.Base(id)})
	}
	if err := a.Telegram.TgSendFile(c, path, caption, replyTo); err != nil {
		return fmt.Errorf("发送失败：%w", err)
	}
	a.Logf("推送：把 %s 发给了用户", a.Tilde(path))
	return nil
}

// 主动找用户：要用户知道的事推到手机（配了 Telegram）或本机通知（没配）。见 README「推送和 Telegram」。
// 推什么：每天的早报摘要（复盘写完、且不早于 08:00）、新的「等用户决定」条目、巡逻发现且没自动处理的问题、
// 普通任务的结果（过门合并了、失败、中断、卡住）、成品送达（agentq notify send，夜班和 agent 调用）、额度 ≥90% 和连环失败熔断。
// 怎么推：要推的消息先记进 ~/.agentq/notify.json 的队列，tick（和 notify send）按规则发：同类 30 分钟内只发一条，
// 后来的攒着、到点合成一条；23:00–08:00 只攒着，08:00 后和早报一起发；每天最多 10 条，早报和「要你定」不受上限挡，
// 超出的普通消息不丢，跟着下一条发出的消息一起带上（或等早报、第二天）；
// 安全问题（备份连续失败等）不等合并窗口、不管夜间和上限，当时就发。
const (
	NotifyMergeWindow = 30 * time.Minute
	NotifyDailyMax    = 10
	notifyQuietFrom   = 23 // 23:00–08:00 只攒着
	notifyQuietTo     = 8
	NotifyMaxLines    = 12                        // 一条消息最多几行（不算空行），超了发摘要和全文路径
	notifyMaxChars    = telegram.TgMaxRunes - 300 // 一条消息最多多少字，留出标题、页脚和全文路径的位置
	notifyPendingMax  = 100
	notifyLockWait    = 30 * time.Second
	MorningStable     = 2 * time.Minute // 早报文件这么久没改才算写完
	notifyErrEvery    = time.Hour       // 同一个推送错误隔这么久才再记一次日志
)

type notifyCat struct{ emoji, Name string }

var NotifyCats = map[string]notifyCat{
	"morning": {"☀️", "早报"},
	"decide":  {"🔴", "要你定"},
	"alert":   {"🔴", "要你知道"},
	"patrol":  {"🟠", "巡逻发现"},
	"done":    {"🟢", "完成"},
	"release": {"📦", "新版可装"},
	"status":  {"📊", "状态"},
	"suggest": {"💡", "闲时建议"},
	"sense":   {"🔎", "主动感知"},
}

// NotifyOrder：队列里能用的类别，也是发送顺序（要用户做的、出问题的在前）。
var NotifyOrder = []string{"alert", "decide", "patrol", "release", "done", "suggest", "sense"}

type Notice struct {
	Cat string `json:"cat"`
	// Key 非空时，队列里同 Key 的旧消息被新的替换（同一件事只推最新的说法）。
	Key   string   `json:"key,omitempty"`
	Text  string   `json:"text"`            // 一句话，纯文本（发送时转义）
	Lines []string `json:"lines,omitempty"` // 补充，纯文本，每条一行
	// Security：安全问题（密钥或生产信息泄露、备份连续失败），当时就推。
	Security bool      `json:"security,omitempty"`
	At       time.Time `json:"at"`
	// Files：跟着这条消息发的截图、录屏、文件（记下时已检查过大小、压缩过），发完消息再逐个发。
	Files []string `json:"files,omitempty"`
	// MacShown：Telegram 发不出去时已经弹过本机通知（继续留在队列里等 Telegram 恢复）。
	MacShown bool `json:"mac_shown,omitempty"`
	// JobID：这条消息只说一个任务时的任务 id，回复这条消息能接上那个任务。
	JobID string `json:"job_id,omitempty"`
	// Project：「要你定」所在的项目，回复这条消息时用（条目指纹在 Key）。
	Project string `json:"project,omitempty"`
	// Card：条目写了固定格式的选项时的决策卡片，单独发、带按钮（见 cards.go）。
	Card *DecideCard `json:"card,omitempty"`
}

type State struct {
	Pending  []Notice             `json:"pending,omitempty"`
	LastSent map[string]time.Time `json:"last_sent,omitempty"`
	// Day/Count：今天（本地日期）推了几条，到 notifyDailyMax 就只攒着。
	Day        string `json:"day,omitempty"`
	Count      int    `json:"count,omitempty"`
	MorningDay string `json:"morning_day,omitempty"` // 最近一次推早报的日期
	// DecideSeen：见过的「等用户决定」条目（指纹 → 第一次见到的日期）。第一次运行只记下、不推，免得一下子推一堆旧的。
	DecideInit bool              `json:"decide_init,omitempty"`
	DecideSeen map[string]string `json:"decide_seen,omitempty"`
	// UpdateOffset：下次 getUpdates 的 offset（比处理过的最大 update_id 大 1）。agentq listen 收消息，它不在跑时 tick 兜底。
	UpdateOffset int64 `json:"update_offset,omitempty"`
	// Inbox：收到、还没交给 claude 的用户消息（上一次对话还在跑时排队）。
	Inbox []InMsg `json:"inbox,omitempty"`
	// Replies：对话任务的回答当时没发出去的，下次 tick 重发（不受合并、夜间和上限限制），留 replyKeep。
	Replies []PendingReply `json:"replies,omitempty"`
	// Threads：发出去的消息 id → 对应的那件事（见 threads.go）。
	Threads map[int64]Thread `json:"threads,omitempty"`
	// Cards：发出去的决策卡片（短 id → 项目、指纹、选项），过期 14 天（见 cards.go）。
	Cards map[string]*DecideCard `json:"cards,omitempty"`
	// LocalRetryAt：本机通知发不出（errLocalNotify）时，到这个时刻（Unix 秒）之前不再试，消息留在队列。
	LocalRetryAt int64     `json:"local_retry_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitempty"`
}

type PendingReply struct {
	Text    string    `json:"text"`
	ReplyTo int64     `json:"reply_to,omitempty"`
	At      time.Time `json:"at"`
	JobID   string    `json:"job_id,omitempty"`
	Thread  *Thread   `json:"thread,omitempty"` // 发出去之后要登记的回复链
	// File：回答太长时的全文文件，跟在文字后面发。Text 为空表示文字已经到了，只差文件。
	File string `json:"file,omitempty"`
}

type InMsg struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
	ReplyTo string    `json:"reply_to,omitempty"` // 用户回复的那条消息的原文
	// ReplyToID：用户回复的那条消息的 id，按它查 threads。
	ReplyToID int64 `json:"reply_to_id,omitempty"`
	// Note：交给对话任务时带的固定字段（回复链判断出来的，比如这条是对正在跑的任务的意见）。
	Note string `json:"note,omitempty"`
	// QueueNoted：已经回过「排队了」，等名额期间不再回。
	QueueNoted bool `json:"queue_noted,omitempty"`
	// Files：用户发来的附件下载到本机的路径（收到时在 ~/.agentq/inbox/，交给 claude 时挪进那次对话任务目录的 files/）。
	Files []string `json:"files,omitempty"`
	// StopJob：这条是「停 <任务id>」：不交给 claude，下一次拿到 tick.lock 时由代码执行（见 serveStops）。
	StopJob string `json:"stop_job,omitempty"`
}

func (a *Service) NotifyPath() string { return filepath.Join(a.DataDir(), "notify.json") }

func (a *Service) notifyLockPath() string { return filepath.Join(a.DataDir(), "notify.lock") }

// LoadNotify：不存在就从空的开始；内容坏了备份成 notify.json.bad-<时间> 再从空的开始（攒着的消息丢了，记日志）。
func (a *Service) LoadNotify() (*State, error) {
	s := &State{}
	b, err := os.ReadFile(a.NotifyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		bak := a.NotifyPath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := fsx.WriteFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.NotifyPath(), err, bak, werr)
		}
		a.Logf("推送：%s 格式不对（%v），已备份为 %s，从空的开始（攒着的消息丢了）", a.NotifyPath(), err, bak)
		// 坏文件留着的话，status、listen 每读一次就再备份一份（文件很大时很快占满磁盘）
		if werr := a.SaveNotify(&State{}); werr != nil {
			a.Logf("推送：重置 %s 失败：%v", a.NotifyPath(), werr)
		}
		return &State{}, nil
	}
	return s, nil
}

func (a *Service) SaveNotify(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(a.NotifyPath(), append(b, '\n'))
}

// WithNotify 拿着 notify.lock 读、改、存推送状态。tick、notify send、巡逻都走这里，互不覆盖。
func (a *Service) WithNotify(fn func(s *State) error) error {
	lock, err := fsx.AcquireLock(a.notifyLockPath(), notifyLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("等了 %s 还拿不到 %s", notifyLockWait, a.notifyLockPath())
	}
	defer lock.Close()
	s, err := a.LoadNotify()
	if err != nil {
		return err
	}
	// 先试着存一次：存不下（磁盘满）时不动手。否则消息发出去了、记录没存上，下一轮还当没发，同一条消息反复发（listen 里是一秒几百条）
	if err := a.SaveNotify(s); err != nil {
		return fmt.Errorf("%s 存不下，这次不处理：%w", a.NotifyPath(), err)
	}
	// 拿着锁期间本进程里再要记消息（比如启动对话任务失败触发熔断），直接记进这份，不再去拿同一把锁
	a.notifyHeld = s
	ferr := fn(s)
	a.notifyHeld = nil
	if err := a.SaveNotify(s); err != nil {
		return err
	}
	return ferr
}

// TaskEndKey 是一条待办的结果通知的 Key：同一条还没发出去又有新结果（失败后重试合并了），只推最新的。
func TaskEndKey(project, fingerprint string) string { return "task-end:" + project + ":" + fingerprint }

// AuthPaused 是后端认证失败、暂停接活的通知。任务和门撞上的是同一件事，Key 相同，只提醒一次。
func AuthPaused(name, jobID, pausePath string, until time.Time) Notice {
	return Notice{Cat: "alert", Key: "backend-auth:" + name, JobID: jobID, Text: "后端 " + name + " 认证失败，暂停接活",
		Lines: []string{"重新登录后删掉 " + pausePath + " 可以提前恢复；到 " + textfmt.WhenCN(until) + " 自动再试", "详情 agentq logs " + jobID}}
}

// Notify 把要推的消息记进队列（不在这里发：tick 末尾统一发，notify send 记完马上发）。出错只记日志，不影响调用方。
func (a *Service) Notify(list ...Notice) {
	if !a.Cfg.NotifyEnabled || len(list) == 0 {
		return
	}
	if a.notifyHeld != nil {
		a.notifyHeld.Add(a.Now(), list...)
		return
	}
	if err := a.WithNotify(func(s *State) error { s.Add(a.Now(), list...); return nil }); err != nil {
		a.Logf("推送：没记下 %d 条要推的消息：%v", len(list), err)
	}
}

func (s *State) Add(now time.Time, list ...Notice) {
	for _, n := range list {
		if n.At.IsZero() {
			n.At = now
		}
		replaced := false
		if n.Key != "" {
			for i := range s.Pending {
				if s.Pending[i].Key == n.Key {
					s.Pending[i], replaced = n, true
					break
				}
			}
		}
		if !replaced {
			s.Pending = append(s.Pending, n)
		}
	}
	if len(s.Pending) > notifyPendingMax {
		s.Pending = s.Pending[len(s.Pending)-notifyPendingMax:]
	}
}

func IsQuiet(t time.Time) bool { return t.Hour() >= notifyQuietFrom || t.Hour() < notifyQuietTo }

// QuietEnd：t 不在夜间就是 t，在夜间就是夜间结束、消息能发出去的时刻。
func QuietEnd(t time.Time) time.Time {
	if !IsQuiet(t) {
		return t
	}
	day := t
	if t.Hour() >= notifyQuietFrom {
		day = t.AddDate(0, 0, 1)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), notifyQuietTo, 0, 0, 0, t.Location())
}

// DropPending 去掉队列里这个 Key 的消息：事情已经过去了，没发的不用发了。
func (a *Service) DropPending(key string) {
	err := a.WithNotifyState(func(s *State) {
		s.Pending = slices.DeleteFunc(s.Pending, func(n Notice) bool { return n.Key == key })
	})
	if err != nil {
		a.Logf("推送：没去掉过期的消息 %s：%v", key, err)
	}
}

// NotifyErr 记推送出的错：同样的错一小时只记一次日志，免得每 5 分钟刷一行。
func (a *Service) NotifyErr(s *State, msg string) {
	now := a.Now()
	if msg == s.LastError && now.Sub(s.LastErrorAt) < notifyErrEvery && !now.Before(s.LastErrorAt) {
		return
	}
	s.LastError, s.LastErrorAt = msg, now
	a.Logf("推送：%s", msg)
}

// macNotify 弹一条本机通知。环境变量 AGENTQ_MACNOTIFY_LOG 非空时改成往那个文件追加一行（测试用，不弹真通知）。
func (a *Service) macNotify(title, body string) error {
	if a.MacNotify != nil {
		return a.MacNotify(title, body)
	}
	if p := os.Getenv("AGENTQ_MACNOTIFY_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "%s｜%s\n", title, strings.ReplaceAll(body, "\n", "⏎"))
		return err
	}
	return localNotify(a, title, body)
}

// Deliver 发一条已排好版的消息：配了 Telegram 走 Telegram，没配（c 为空且没有凭据错误）走本机通知。返回用的通道名。
func (a *Service) Deliver(c *telegram.TgCreds, credErr error, msg string) (string, int64, error) {
	if c != nil {
		id, err := a.Telegram.TgSend(c, msg, 0)
		return "Telegram", id, err
	}
	if credErr != nil {
		return "Telegram", 0, credErr
	}
	title, body, _ := strings.Cut(textfmt.HTMLToPlain(msg), "\n")
	return "本机通知", 0, a.macNotify(strings.TrimSpace(title), strings.TrimSpace(body))
}

// localNotifyRetry：本机通知发不出时隔多久再试。没有桌面会话的机器一直这样，每次 tick 都试只会刷日志。
const localNotifyRetry = time.Hour

// localNotifyWaiting：本机通知刚发不出，还没到再试的时候（时钟往回跳了不算，免得等太久）。
func localNotifyWaiting(s *State, now time.Time, c *telegram.TgCreds, credErr error) bool {
	d := s.LocalRetryAt - now.Unix()
	return c == nil && credErr == nil && d > 0 && d <= int64(localNotifyRetry/time.Second)
}

// FlushNotify 按规则把队列里到点的消息发出去（调用方拿着 notify.lock）。st 可以为空（读不出状态时不推早报）。
func (a *Service) FlushNotify(s *State, st *state.State, c *telegram.TgCreds, credErr error) {
	now := a.Now()
	if day := now.Format("2006-01-02"); s.Day != day {
		s.Day, s.Count = day, 0
	}
	if s.LastSent == nil {
		s.LastSent = map[string]time.Time{}
	}
	if localNotifyWaiting(s, now, c, credErr) {
		return
	}
	quiet := IsQuiet(now)
	if !quiet && st != nil {
		a.PushMorning(s, st, c, credErr)
		if localNotifyWaiting(s, now, c, credErr) {
			return
		}
	}
	for _, cat := range NotifyOrder {
		var group []Notice
		for _, n := range s.Pending {
			if n.Cat == cat {
				group = append(group, n)
			}
		}
		if len(group) == 0 {
			continue
		}
		last := s.LastSent[cat]
		// 到了每天上限（每类按当时的条数算，同一次里前面发的也算）：只有「要你定」和安全问题照发，其余攒着，搭这些消息一起发出去
		capped := s.Count >= NotifyDailyMax
		due := !quiet && (!capped || bypassCap(cat)) && (now.Sub(last) >= NotifyMergeWindow || now.Before(last))
		batch := group
		if !due {
			batch = nil
			for _, n := range group {
				if n.Security {
					batch = append(batch, n)
				}
			}
			if len(batch) == 0 {
				continue
			}
		}
		if due && cat == "decide" && c != nil {
			if batch = a.sendDecideCards(s, st, c, batch, now); len(batch) == 0 {
				continue
			}
		}
		sortNotices(batch)
		var riders []Notice // 上限挡下的普通消息，搭这条一起发（夜里不搭：只有安全问题能在夜里发）
		if capped && !quiet {
			riders = cappedRiders(s.Pending, batch)
		}
		var secs []MsgSection
		for _, n := range append(append([]Notice(nil), batch...), riders...) {
			secs = append(secs, MsgSection{Emoji: NotifyCats[n.Cat].emoji, Head: n.Text, Lines: append(append([]string(nil), n.Lines...), a.fileLines(c, n.Files)...)})
		}
		msg := a.RenderMessage(cat, now, secs, "agentq")
		via, sentID, err := a.Deliver(c, credErr, msg)
		if err != nil {
			a.NotifyErr(s, fmt.Sprintf("%s 发送失败，%d 条留在队列里下次再试：%v", via, len(batch), err))
			if via == "Telegram" {
				a.macFallback(s, batch, msg)
			}
			if errors.Is(err, ErrLocalNotify) {
				s.LocalRetryAt = now.Add(localNotifyRetry).Unix()
				return
			}
			continue
		}
		if len(batch) == 1 && len(riders) == 0 { // 合并了多条的说不清是哪件事，不登记
			if th, ok := a.noticeThread(st, batch[0]); ok {
				s.AddThread(sentID, th, now)
			}
		}
		s.removePending(append(append([]Notice(nil), batch...), riders...))
		s.LastSent[cat] = now
		s.Count++
		s.LastError = ""
		if len(riders) > 0 {
			a.Logf("推送：发出「%s」%d 条，带上上限挡下的 %d 条（%s）", NotifyCats[cat].Name, len(batch), len(riders), via)
		} else {
			a.Logf("推送：发出「%s」%d 条（%s）", NotifyCats[cat].Name, len(batch), via)
		}
		a.queueNoticeFiles(c, batch)
		a.queueNoticeFiles(c, riders)
	}
}

// bypassCap：不受每天条数上限挡的类别（要用户做决定的）。安全问题按 Notice.Security 另算。
func bypassCap(cat string) bool { return cat == "decide" }

// cappedRiders：队列里被每天上限挡下的普通消息（不是「要你定」、不是安全问题、不在这一批里），按类别顺序排好。
func cappedRiders(pending, batch []Notice) []Notice {
	var out []Notice
	for _, n := range pending {
		if bypassCap(n.Cat) || n.Security {
			continue
		}
		in := false
		for _, m := range batch {
			if SameNotice(n, m) {
				in = true
				break
			}
		}
		if !in {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return CatRank(out[i].Cat) < CatRank(out[k].Cat) })
	return out
}

// macFallback：Telegram 发不出去时，没弹过本机通知的消息弹一次（用户在电脑前就能先看到），消息仍留着等 Telegram。
func (a *Service) macFallback(s *State, batch []Notice, msg string) {
	fresh := false
	for _, n := range batch {
		if !n.MacShown {
			fresh = true
		}
	}
	if !fresh {
		return
	}
	title, body, _ := strings.Cut(textfmt.HTMLToPlain(msg), "\n")
	if err := a.macNotify(strings.TrimSpace(title)+"（Telegram 没发出去）", strings.TrimSpace(body)); err != nil {
		a.Logf("推送：本机通知也没弹出来：%v", err)
		if !errors.Is(err, ErrLocalNotify) { // 没有桌面会话时试一次就够，消息仍留着等 Telegram
			return
		}
	}
	for i := range s.Pending {
		for _, n := range batch {
			if SameNotice(s.Pending[i], n) {
				s.Pending[i].MacShown = true
			}
		}
	}
}

// fileLines：没配 Telegram（发本机通知）时，附件写成「附件 <路径>」几行；走 Telegram 时附件另发，不写。
func (a *Service) fileLines(c *telegram.TgCreds, files []string) []string {
	if c != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		out = append(out, "附件 "+a.Tilde(f))
	}
	return out
}

// queueNoticeFiles：消息发到 Telegram 之后，把这批消息带的附件记下，等放开 notify.lock 再由 sendQueuedFiles 发
// （上传一个大录屏可能要几十秒，不能让 notify send 和收消息等着锁）。
func (a *Service) queueNoticeFiles(c *telegram.TgCreds, batch []Notice) {
	if c == nil {
		return
	}
	for _, n := range batch {
		a.filesOut = append(a.filesOut, n.Files...)
	}
}

// SendQueuedFiles 逐个发出 queueNoticeFiles 记下的附件（调用方已放开 notify.lock）。发不出去只记日志，不重发（消息本身已经到了）。
func (a *Service) SendQueuedFiles() {
	files := a.filesOut
	a.filesOut = nil
	if len(files) == 0 {
		return
	}
	c, err := a.Telegram.TelegramCreds()
	if c == nil {
		a.Logf("推送：%d 个附件没发出去：Telegram 凭据用不了 %v", len(files), err)
		return
	}
	for _, f := range files {
		if err := a.Telegram.TgSendFile(c, f, "", 0); err != nil {
			a.Logf("推送：附件 %s 没发出去：%v", a.Tilde(f), c.Redact(err.Error()))
		}
	}
}

func SameNotice(x, y Notice) bool {
	return x.Cat == y.Cat && x.Key == y.Key && x.Text == y.Text && x.At.Equal(y.At)
}

func (s *State) removePending(sent []Notice) {
	kept := s.Pending[:0]
	for _, n := range s.Pending {
		drop := false
		for _, m := range sent {
			if SameNotice(n, m) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, n)
		}
	}
	s.Pending = kept
}

// sortNotices：安全问题在前，其余按记下的先后。
func sortNotices(list []Notice) {
	sort.SliceStable(list, func(i, k int) bool { return list[i].Security && !list[k].Security })
}

type MsgSection struct {
	Emoji string   // 段首的表情，段内不再加
	Head  string   // 段的第一行，纯文本
	Lines []string // 后面几行，纯文本
}

// RenderMessage 排版：标题行「<表情> <b>类别 · 日期</b>」，空一行，各段（段间空一行），最后一行「<i>时间 · 来源</i>」。
// 不算空行超过 notifyMaxLines 或字数超过 notifyMaxChars 时只留前面的，末尾写「其余 N 行见 <全文路径>」，全文存到 ~/.agentq/notify/。
// Telegram 一条最多 4096 字，超了永远发不出去：这条消息一直留在队列里，同类后面的消息也被它拖着发不出去。
func (a *Service) RenderMessage(cat string, now time.Time, secs []MsgSection, source string) string {
	ci := NotifyCats[cat]
	header := ci.emoji + " <b>" + textfmt.Esc(ci.Name) + " · " + textfmt.DateCN(now) + "</b>"
	footer := "<i>" + now.Format("15:04") + " · " + textfmt.Esc(source) + "</i>"
	var lines []string // 空串是段间空行
	for i, s := range secs {
		if i > 0 {
			lines = append(lines, "")
		}
		head := s.Head
		if s.Emoji != "" {
			head = s.Emoji + " " + head
		}
		lines = append(lines, head)
		for _, l := range s.Lines {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
	}
	budget := NotifyMaxLines - 2
	n, chars := 0, 0
	for _, l := range lines {
		if l != "" {
			n++
		}
		chars += utf8.RuneCountInString(l) + 1
	}
	var body []string
	if n <= budget && chars <= notifyMaxChars {
		for _, l := range lines {
			body = append(body, textfmt.Esc(l))
		}
	} else {
		full := textfmt.HTMLToPlain(header) + "\n\n" + strings.Join(lines, "\n") + "\n" + textfmt.HTMLToPlain(footer) + "\n"
		path := a.saveFullText(now, cat, full)
		kept, left, cut := 0, notifyMaxChars, false
		for _, l := range lines {
			if l != "" {
				if kept == budget-1 {
					break
				}
				kept++
			}
			if r := []rune(l); len(r) > left {
				body, cut = append(body, textfmt.Esc(string(r[:left])+"…")), true
				break
			}
			left -= utf8.RuneCountInString(l) + 1
			body = append(body, textfmt.Esc(l))
		}
		for len(body) > 0 && body[len(body)-1] == "" {
			body = body[:len(body)-1]
		}
		if cut && n == kept {
			body = append(body, textfmt.Esc("全文见 "+path))
		} else {
			body = append(body, textfmt.Esc(fmt.Sprintf("其余 %d 行见 %s", n-kept, path)))
		}
	}
	return header + "\n\n" + strings.Join(body, "\n") + "\n" + footer
}

// saveFullText 把超长消息的全文存到 ~/.agentq/notify/<时间>-<类别>.txt，返回给人看的路径（~ 开头）；存不了返回说明。
func (a *Service) saveFullText(now time.Time, cat, text string) string {
	path := filepath.Join(a.DataDir(), "notify", now.Format("20060102-150405")+"-"+cat+".txt")
	if err := fsx.WriteFileAtomic(path, []byte(text)); err != nil {
		a.Logf("推送：存全文 %s 失败：%v", path, err)
		return "（全文没存下：" + err.Error() + "）"
	}
	return a.Tilde(path)
}

// ReportPushedPath：早报推到 Telegram 后写的标记，会话里的早报转告看到它就不再转告。
func (a *Service) ReportPushedPath(day string) string {
	return filepath.Join(a.ReportStateDir(), "report-pushed-"+day)
}

// morningReady：今天还没推过早报、早报文件写完了（存在、morningStable 内没再改、复盘任务没在跑）时返回早报路径。
func (a *Service) morningReady(s *State, st *state.State, now time.Time) (string, bool) {
	day := now.Format("2006-01-02")
	if s.MorningDay == day {
		return "", false
	}
	if _, err := os.Stat(a.ReportPushedPath(day)); err == nil {
		s.MorningDay = day // 别处已经推过（标记在），不再推
		return "", false
	}
	path := filepath.Join(a.ReportDir(), day+".md")
	fi, err := os.Stat(path)
	if err != nil || now.Sub(fi.ModTime()) < MorningStable {
		return "", false
	}
	for _, j := range st.Running() {
		if j.Kind == config.KindReview {
			return "", false
		}
	}
	return path, true
}

// PushMorning 早报摘要：要你定的、出问题的放最前，再写昨晚完成、任务成败和额度，最后是早报第一行；
// 队列里攒着的消息（夜里攒下的）一起带上。不受每天上限挡（但算一条）。推到 Telegram 成功才写 report-pushed 标记。
func (a *Service) PushMorning(s *State, st *state.State, c *telegram.TgCreds, credErr error) {
	now := a.Now()
	path, ok := a.morningReady(s, st, now)
	if !ok {
		return
	}
	day := now.Format("2006-01-02")
	var secs []MsgSection
	held := append([]Notice(nil), s.Pending...)
	sort.SliceStable(held, func(i, k int) bool { return CatRank(held[i].Cat) < CatRank(held[k].Cat) })
	for _, n := range held {
		if n.Cat == "decide" { // 下面「要你定」一段已经列了
			continue
		}
		secs = append(secs, MsgSection{Emoji: NotifyCats[n.Cat].emoji, Head: n.Text})
	}
	all, _ := a.Decisions()
	if decs := Askable(all); len(decs) > 0 {
		sec := MsgSection{Emoji: "🔴", Head: fmt.Sprintf("要你定　%d 条", len(decs))}
		for i, d := range decs {
			if i == 3 {
				sec.Lines = append(sec.Lines, fmt.Sprintf("另有 %d 条", len(decs)-3))
				break
			}
			sec.Lines = append(sec.Lines, d.Project+"　"+textfmt.TruncRunes(d.Title, 40))
		}
		secs = append(secs, sec)
	}
	if titles := a.MorningIssues(); len(titles) > 0 {
		sec := MsgSection{Emoji: "🟠", Head: fmt.Sprintf("出问题　%d 个", len(titles))}
		if len(titles) > 2 {
			titles = append(titles[:2], fmt.Sprintf("另有 %d 个（agentq patrol 看证据）", len(titles)-2))
		}
		sec.Lines = titles
		secs = append(secs, sec)
	}
	if a.MorningGates != nil {
		if lines := a.MorningGates(st, now); len(lines) > 0 {
			secs = append(secs, MsgSection{Emoji: "🚪", Head: "门", Lines: lines})
		}
	}
	var done []string
	counts := map[string]int{}
	for _, j := range st.Jobs {
		if !state.TakesSlot(j.Kind) || j.EndedAt == nil || now.Sub(*j.EndedAt) > 24*time.Hour {
			continue
		}
		if state.VerifyFailed(j) { // 升级前的合并后核验失败不算完成，下面单写一个数
			counts["verify"]++
			continue
		}
		if state.GateUnmerged(j) { // 还没进 main 的不算完成
			counts["gate"]++
			continue
		}
		counts[j.Status]++
		if j.Status == state.StatusDone {
			done = append(done, j.Project+"　"+textfmt.TruncRunes(j.Title, 30))
		}
	}
	if len(done) > 0 {
		sec := MsgSection{Emoji: "🟢", Head: fmt.Sprintf("昨晚完成　%d 条", len(done))}
		if len(done) > 3 {
			done = append(done[:3], fmt.Sprintf("另有 %d 条", len(done)-3))
		}
		sec.Lines = done
		secs = append(secs, sec)
	}
	tasks := fmt.Sprintf("任务　跑完 %d，失败 %d，中断 %d，超时 %d（24 小时内）",
		counts[state.StatusDone], counts[state.StatusFailed], counts[state.StatusInterrupted], counts[state.StatusTimeout])
	if n := counts["verify"]; n > 0 {
		tasks = strings.Replace(tasks, "（24 小时内）", fmt.Sprintf("，合并后核验失败 %d（24 小时内）", n), 1)
	}
	if n := counts["gate"]; n > 0 {
		tasks = strings.Replace(tasks, "（24 小时内）", fmt.Sprintf("，没过门 %d（24 小时内）", n), 1)
	}
	secs = append(secs, MsgSection{Emoji: "📊", Head: tasks, Lines: append([]string{"额度　" + backend.UsageDescribe(st.Usage, now, a.Cfg.Model)}, st.OtherUsageLines(a.Cfg, now)...)})
	if first := reportFirstLine(path); first != "" {
		secs = append(secs, MsgSection{Emoji: "📝", Head: first})
	}
	msg := a.RenderMessage("morning", now, secs, "早报全文 "+a.Tilde(path))
	via, _, err := a.Deliver(c, credErr, msg)
	if err != nil {
		a.NotifyErr(s, fmt.Sprintf("早报发送失败（%s），下次 tick 再试：%v", via, err))
		if errors.Is(err, ErrLocalNotify) {
			s.LocalRetryAt = now.Add(localNotifyRetry).Unix()
		}
		return
	}
	s.MorningDay = day
	s.LastSent["morning"] = now
	s.Count++
	s.Pending = nil
	if c != nil { // 带按钮的卡片早报只列了标题，接着单独发
		s.Pending = cardNotices(held)
	}
	a.queueNoticeFiles(c, held)
	a.Logf("推送：发出早报摘要，带上攒着的 %d 条（%s）", len(held), via)
	if via != "Telegram" {
		return
	}
	if err := fsx.WriteFileAtomic(a.ReportPushedPath(day), []byte(now.Format(time.RFC3339)+"\n")); err != nil {
		a.Logf("推送：早报已推，但写标记 %s 失败（会话里会再转告一次）：%v", a.ReportPushedPath(day), err)
	}
}

func CatRank(cat string) int {
	for i, c := range NotifyOrder {
		if c == cat {
			return i
		}
	}
	return len(NotifyOrder)
}

// reportFirstLine：早报的第一行（一句话总结），去掉 Markdown 的标题井号和加粗。
func reportFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(l, "# "), "**", ""))
		if l != "" {
			return textfmt.TruncRunes(l, 120)
		}
	}
	return ""
}

type Decision struct {
	Project, Title, Text, Fingerprint, Path string
	// Waiting/Cond：条目带「（等条件：…）」标记，前置条件还没满足，现在问用户也定不了。
	// 巡逻、推送、早报和「状态」的「要你定」都跳过它，只在早报详情列一行。
	Waiting bool
	Cond    string
}

// Askable 去掉带等条件标记的条目，剩下现在就要用户定的。
func Askable(decs []Decision) []Decision {
	var out []Decision
	for _, d := range decs {
		if !d.Waiting {
			out = append(out, d)
		}
	}
	return out
}

// ScanDecisions 列出各项目 BACKLOG「等用户决定」里的条目（「- [ ]」开头），指纹按标题算（标题不含等条件标记）。
// 带等条件标记的也列出来（Waiting），用的地方按需用 askable 去掉。读不了的项目返回说明。
func ScanDecisions(root string) ([]Decision, []string) {
	projects, err := backlog.ScanProjects(root)
	if err != nil {
		return nil, []string{"扫描项目目录失败：" + err.Error()}
	}
	return scanDecisionsIn(projects)
}

func scanDecisionsIn(projects []backlog.Project) ([]Decision, []string) {
	var out []Decision
	var fails []string
	for _, p := range projects {
		b, err := os.ReadFile(p.BacklogPath())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			fails = append(fails, p.Name+" 的 BACKLOG 读不了："+err.Error())
			continue
		}
		last := -1 // 刚读到的条目，下一行缩进写选项时接在它后面
		backlog.EachBacklogLine(string(b), func(section, line string) {
			if !backlog.IsDecisionSection(section) {
				last = -1
				return
			}
			if !strings.HasPrefix(line, "- [ ]") {
				switch t := strings.TrimSpace(line); {
				case !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
					last = -1
				case last >= 0 && DecideOptionsLineRe.MatchString(t) && !strings.Contains(out[last].Text, "（选项："):
					out[last].Text += t
				}
				return
			}
			last = -1
			text := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
			if text == "" {
				return
			}
			last = len(out)
			title := DecisionTitle(text)
			sum := sha1.Sum([]byte(title))
			cond, waiting := backlog.WaitCondition(text)
			out = append(out, Decision{Project: p.Name, Title: title, Text: text, Path: p.BacklogPath(),
				Fingerprint: "decide:" + p.Name + ":" + hex.EncodeToString(sum[:6]), Waiting: waiting, Cond: cond})
		})
	}
	return out, fails
}

// WatchDecisions：每次 tick 看一遍各项目「等用户决定」，新出现的条目推一条「要你定」（用户回复那条消息就能拍板）。
// 带等条件标记的不推、也不记进 DecideSeen：标记去掉（条件满足）后它才算新出现，那时再推。
func (a *Service) WatchDecisions(s *State) {
	all, fails := a.Decisions()
	decs := Askable(all)
	if s.DecideSeen == nil {
		s.DecideSeen = map[string]string{}
	}
	now := a.Now()
	cur := map[string]bool{}
	for _, d := range decs {
		cur[d.Fingerprint] = true
		if _, ok := s.DecideSeen[d.Fingerprint]; ok {
			continue
		}
		s.DecideSeen[d.Fingerprint] = now.Format("2006-01-02")
		if !s.DecideInit {
			continue
		}
		s.Add(now, decideNotice(d))
	}
	if len(fails) == 0 { // 有项目读不了时不知道它的条目还在不在，先都记着
		for fp := range s.DecideSeen {
			if !cur[fp] {
				delete(s.DecideSeen, fp)
			}
		}
	}
	s.DecideInit = true
}

const NotifyUsage = `用法：
  agentq notify setup                    配 Telegram（执行 ~/.agentq/secrets/telegram-setup.sh；没有这个脚本就打印手动配置步骤）
  agentq notify test                     立刻发一条测试消息（配了 Telegram 发 Telegram，没配弹本机通知），不占每天的条数
  agentq notify send [--security] [--key=K] [--file=路径]… <类别> <一句话> [补充…]
                                         记一条要推给你的消息并按规则推（类别：decide 要你定、alert 要你知道、
                                         patrol 巡逻发现、release 新版可装、done 完成）；夜班和 agent 用。
                                         --file 附上截图、效果图（按图片发）、录屏（按视频发，超过 50MB 先压缩），跟在消息后面发
  agentq notify file <路径> [说明]        马上把一个文件发给你（Telegram；对话任务里回图片、录屏用，不进队列、不占条数）
  agentq notify status                   用哪个通道、今天推了几条、攒着哪些
`

// NotifyStatus：status 里的一行（读 notify.json，不拿锁）。
func (a *Service) NotifyStatus() string {
	if !a.Cfg.NotifyEnabled {
		return "关（notify_enabled=false）"
	}
	via := "本机通知（没配 Telegram，agentq notify setup 配）"
	if c, err := a.Telegram.TelegramCreds(); err != nil {
		via = "Telegram 凭据用不了：" + err.Error()
	} else if c != nil {
		via = "Telegram"
	}
	s, err := a.LoadNotify()
	if err != nil {
		return via + "；读不了 " + a.NotifyPath() + "：" + err.Error()
	}
	count := 0
	if s.Day == a.Now().Format("2006-01-02") {
		count = s.Count
	}
	return fmt.Sprintf("%s，今天已推 %d/%d 条，攒着 %d 条", via, count, NotifyDailyMax, len(s.Pending))
}

// ErrLocalNotify：本机通知这次没送达，再试也多半一样（没有桌面会话）。调用方把消息留在队列，隔 localNotifyRetry 再试。
var ErrLocalNotify = errors.New("本机通知没送达")

// NotifySendOrLog 是 Linux 的本机通知：没有 notify-send 时只写日志（这台机器就没有这个通道）；
// 有但发不出时返回 errLocalNotify，不算送达。
func NotifySendOrLog(a *Service, title, body string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		a.Logf("本机通知（没有 notify-send，只写日志）：%s｜%s", title, textfmt.OneLine(body, 500))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--app-name=agentq", "--", title, body)
	cmd.Env = NotifySendEnv(os.Environ(), func(p string) bool { _, e := os.Stat(p); return e == nil })
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w：notify-send %v %s", ErrLocalNotify, err, textfmt.OneLine(string(out), 200))
	}
	return nil
}

// NotifySendEnv：后台进程走 childEnv 白名单，没有会话总线的地址，notify-send 找不到桌面。只给 notify-send 补，不进 claude 的白名单。
func NotifySendEnv(env []string, exists func(string) bool) []string {
	env = proc.SystemctlEnv(env, exists)
	dir := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=") && kv != "DBUS_SESSION_BUS_ADDRESS=" {
			return env
		}
		if v, ok := strings.CutPrefix(kv, "XDG_RUNTIME_DIR="); ok {
			dir = v
		}
	}
	if bus := filepath.Join(dir, "bus"); dir != "" && exists(bus) {
		return append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
	}
	return env
}

// EnableNotifyIfConfigured：运行 notify setup 就是要用推送，凭据能用就打开 notify_enabled（只改这一个键），start 不用再多一步。
func (a *Service) EnableNotifyIfConfigured() error {
	c, err := a.Telegram.TelegramCreds()
	if err != nil {
		fmt.Fprintf(a.Out, "凭据还不能用，推送没打开：%v\n", err)
		return nil
	}
	if c == nil {
		fmt.Fprintln(a.Out, "写好后再运行一次 agentq notify setup，会打开推送。")
		return nil
	}
	if err := config.SetKey(a.ConfigPath(), "notify_enabled", "true", func() error {
		return fmt.Errorf("%s 不见了", a.ConfigPath())
	}); err != nil {
		return fmt.Errorf("打开推送失败：%w", err)
	}
	a.Cfg.NotifyEnabled = true
	fmt.Fprintln(a.Out, "推送已打开。运行 agentq start：一起装上常驻的消息监听，发消息随到随回。")
	return nil
}
