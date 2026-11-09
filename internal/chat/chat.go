// Package chat 接用户发来的消息后决定怎么回：长轮询、Fast Path、Router、回复链、对话任务和决策卡片的回调。
package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/discuss"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/inbox"
	"agentq/internal/ledger"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/orphan"
	"agentq/internal/release"
	"agentq/internal/runner"
	"agentq/internal/service"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是消息处理的模块实例。
type Service struct {
	*env.Env
	Backlog  *backlog.Service
	Discuss  *discuss.Service
	Gate     *gate.Service
	Inbox    *inbox.Service
	Ledger   *ledger.Service
	Newproj  *newproj.Service
	Notify   *notify.Service
	Orphan   *orphan.Service
	Release  *release.Service
	Runner   *runner.Service
	Service  *service.Service
	Suggest  *suggest.Service
	Telegram *telegram.Service
	// StartChat 为空时经任务外壳启动 Telegram 对话任务。测试里替换掉。
	StartChat func(st *state.State, id, prompt string) (*state.Job, error)
}

// Telegram 双向：agentq listen 长轮询收用户发来的消息（见 listen.go，只认 telegram.env 里的 chat id）。
// 「状态」「暂停」「恢复」「额度」「在跑什么」「停 <任务id>」由代码直接答（见 fastpath.go，不花额度）；其余交给一次 claude -p（带用户的全局规则和技能）理解意图：
// 派活、拍板、问问题、小事当场办。它的最终回答原样回发给用户。同一时间只跑一个对话任务，期间来的消息排队，下一次一起交给它。
const (
	ChatJobName = "Telegram对话" // 也是任务 id 的一部分，不能有空格（agentq logs <id> 要能直接复制）
	// chatFirstPollMaxAge：第一次收消息（还没有 offset）时，比这更早的消息不处理。
	chatFirstPollMaxAge = 10 * time.Minute
)

// isStatusAsk：只认这几个固定说法（整条消息就是它），别的话都交给 claude 理解。
func isStatusAsk(text string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.TrimRight(text, "。.？?"))) {
	case "状态", "status", "/status", "/start":
		return true
	}
	return false
}

// ServeInbox：有排队的消息时处理它们（调用方拿着 tick.lock 和 notify.lock：tick 末尾，或 agentq listen 收到消息后）。
// 回复了已登记消息的先按回复链分流（threads.go）；其余的、没有对话任务在跑时启动一次 claude -p 处理：接原对话的一次只接一个会话，
// 别的留到下一次。不看暂停和熔断（用户亲口问的，同 agentq run），用量冷却、额度 ≥98%、没登录时不启动，直接回用户原因。
// 附件挪进这次对话任务目录的 files/，路径写进 prompt。
func (a *Service) ServeInbox(s *notify.State, st *state.State, c *telegram.TgCreds) {
	a.serveStops(s, st, c)
	if len(s.Inbox) == 0 {
		return
	}
	s.PruneThreads(a.Now())
	rest, held := a.serveReplies(s, st, c)
	s.Inbox = held
	if len(rest) == 0 {
		return
	}
	for _, j := range st.Running() {
		if j.Kind == state.KindChat || j.Kind == state.KindRouter {
			s.Inbox = append(rest, held...)
			return // 上一个还在跑，结束后的那次 tick 接着处理
		}
	}
	batch, later, session := chatBatch(s, rest)
	s.Inbox = append(later, held...)
	last := batch[len(batch)-1].ID
	refuse := func(why string) { a.refuseMsgs(s, c, batch, why) }
	now := a.Now()
	if why := a.chatRefusal(st, now); why != "" {
		refuse(why)
		return
	}
	id, err := a.Runner.NewJobID(ChatJobName)
	if err != nil {
		refuse("建不了任务目录：" + err.Error())
		return
	}
	inbox := a.moveChatFiles(id, batch)
	if b, err := json.MarshalIndent(inbox, "", "  "); err == nil {
		_ = fsx.WriteFileAtomic(filepath.Join(a.JobDir(id), "inbox.json"), append(b, '\n'))
	}
	start, prompt := a.StartChat, chatPrompt(inbox, now)
	if start == nil || session != "" {
		start = func(st *state.State, id, prompt string) (*state.Job, error) {
			return a.startChatJob(st, id, prompt, session)
		}
	}
	if a.Cfg.RouterEnabled && session == "" && a.Runner.Lacks(backend.MustRouter) == "" { // 后端做不到 Router 的硬要求时走对话任务
		start, prompt = a.startRouterJob, a.RouterPrompt(st, inbox, now)
	}
	j, err := start(st, id, prompt)
	if err != nil {
		refuse("没能启动 " + a.DefaultBackend().Name() + "：" + err.Error())
		return
	}
	if err := fsx.WriteFileAtomic(filepath.Join(a.JobDir(j.ID), "reply-to"), []byte(strconv.FormatInt(last, 10)+"\n")); err != nil {
		a.Logf("Telegram：对话任务 %s 记不下回复哪条消息（回答照发，只是不挂在那条下面）：%v", j.ID, err)
	}
	a.Logf("Telegram：启动对话任务 %s 处理用户的 %d 条消息", j.ID, len(batch))
}

// chatBatchMax：一批消息最多多少字节。提示词走命令行参数，放不下就起不来（E2BIG），那时整批都要退回去；多的留到下一批。
const chatBatchMax = 100 << 10

// chatBatch 挑这次交给对话任务的消息：第一条要接原对话的，只取回复同一次对话的；否则取所有不用接原对话的。session 是要接的会话。
func chatBatch(s *notify.State, msgs []notify.InMsg) (batch, later []notify.InMsg, session string) {
	sessionOf := func(m notify.InMsg) string {
		if th, ok := s.Threads[m.ReplyToID]; ok && th.Kind == ThreadChat {
			return th.SessionID
		}
		return ""
	}
	session = sessionOf(msgs[0])
	size, full := 0, false
	for _, m := range msgs {
		n := len(m.Text) + len(m.ReplyTo) + len(m.Note) + 200
		if sessionOf(m) == session && !full && (len(batch) == 0 || size+n <= chatBatchMax) {
			batch = append(batch, m)
			size += n
		} else {
			full = full || sessionOf(m) == session // 后面的也留着，不插队

			later = append(later, m)
		}
	}
	return batch, later, session
}

// refuseMsgs：这几条现在交不出去，回用户原因并丢掉它们的附件。
func (a *Service) refuseMsgs(s *notify.State, c *telegram.TgCreds, msgs []notify.InMsg, why string) {
	msg := a.Notify.RenderMessage("alert", a.Now(), []notify.MsgSection{{Emoji: "🔴", Head: "这条现在没法交给 " + a.DefaultBackend().Name(), Lines: []string{why, "发「状态」看调度情况"}}}, "agentq 对话")
	if _, err := a.Telegram.TgSend(c, msg, msgs[len(msgs)-1].ID); err != nil {
		a.Notify.NotifyErr(s, "回复用户失败："+err.Error())
	}
	for _, m := range msgs {
		for _, f := range m.Files {
			os.Remove(f)
		}
	}
}

// chatRefusal：现在不能启动对话任务的原因（用量冷却中、额度 ≥98%），能启动返回空。
func (a *Service) chatRefusal(st *state.State, now time.Time) string {
	if now.Before(st.CooldownUntil) {
		return "用量冷却中，到 " + textfmt.WhenCN(st.CooldownUntil)
	}
	if lvl, until, why := backend.UsageLevel(st.Usage, now, a.Cfg.UsagePolicy()); lvl >= backend.UsageRefuse {
		return backend.UsageGateNote(lvl, until, why)
	}
	return ""
}

// moveChatFiles 把收件箱里的附件从 ~/.agentq/inbox/ 挪进对话任务目录的 files/，返回换好路径的消息。挪不动的保留原路径。
func (a *Service) moveChatFiles(id string, inbox []notify.InMsg) []notify.InMsg {
	out := make([]notify.InMsg, len(inbox))
	for i, m := range inbox {
		out[i] = m
		out[i].Files = nil
		for _, f := range m.Files {
			dest := filepath.Join(a.ChatFilesDir(id), filepath.Base(f))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err == nil {
				if err = os.Rename(f, dest); err == nil {
					f = dest
				}
			}
			out[i].Files = append(out[i].Files, f)
		}
	}
	return out
}

// startChatJob 经任务外壳启动对话任务（同巡逻分析：记在 state.json，不占并发名额）。id 是 serveInbox 建好的任务目录名。
func (a *Service) startChatJob(st *state.State, id, prompt, session string) (*state.Job, error) {
	if ok, why := a.Runner.CheckLogin(); !ok {
		return nil, errors.New(why)
	}
	dir := a.ProjectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = a.Home
	}
	j := &state.Job{ID: id, Kind: state.KindChat, Project: ChatJobName, ProjectDir: dir, Item: "agentq telegram", Title: "agentq telegram 对话",
		SessionID: session, FromThread: session != ""}
	if session != "" {
		if prev := lastOnSession(st, session); prev != nil {
			j.ResumeOf = prev.ID // 会话累计的用量要减掉上一次
		}
	}
	return a.Runner.Start(st, j, prompt, session)
}

const ChatPromptHead = "你由 agentq 在无人值守模式下用 claude -p 启动，替用户处理他在 Telegram 上发来的消息（agentq 已核对是用户本人的 chat id）。" +
	"回合一结束进程就退出：一切等待都在前台做完，不用后台命令、Monitor 或「等通知」。\n\n" +
	"现在是 %s。回答里的日期和星期照抄 agentq 输出里的写法（如「10-13 周二 21:00」，星期是程序算好的）；" +
	"要写别的日期是星期几，用 `date` 命令查（`+%%u` 输出 1 是周一，7 是周日；指定日期的参数按当前系统写），不自己推算。\n\n" +
	"按用户的全局规则和技能理解他要什么，用自然语言理解，不要求固定命令：\n" +
	"- 派活（如「某项目：加个按月对比」）：按 dispatch 技能写进那个项目的 docs/BACKLOG.md 并执行 agentq run，不等结果；回复已派出（任务 id）和做完去哪看。\n" +
	"- 拍板（回复了某条「要你定」，或说了某条选哪个）：按 dispatch 技能把决定写回那个项目「等用户决定」里对应的条目、移回「待做」顶部（保留分支名）并派出。\n" +
	"- 问状态或问问题：查完简短回答（agentq status、agentq logs、各项目 BACKLOG 和 PROGRESS、~/.claude/nightly 里的汇总和早报）。\n" +
	"- 小事当场办：查个数据、改句配置这类合理又能撤回的直接做；付款、对外发布、删数据这类必须等用户的不做，说明原因；提交 App Store 审核你做不了，只有用户回复那条 📦 提示才提交。\n" +
	"判断不出是哪个项目时，运行 agentq projects 列出项目问一句。~/.agentq/config.json 的 exclude_projects 列出的项目，用户没点名就不碰。\n\n" +
	"附件：用户发来的截图、录屏、文件已经下载好，路径写在那条消息下面。图片用 Read 直接看；视频有 ffmpeg 就抽几帧看，" +
	"没有 ffmpeg 时 macOS 上用 `qlmanage -t -s 1600 -o <目录> <视频>` 取一帧看，别的系统如实说看不了；派活时把附件路径原样写进 BACKLOG 条目，做的人才看得到。\n" +
	"要给用户看图片或录屏（界面截图、效果图、录屏）：执行 `agentq notify file <绝对路径> [一句说明]`，马上发出、挂在用户的消息下面" +
	"（图片按图片发，录屏按视频发，太大会先压缩，压不下就改发文件路径）；最终回答里提一句发了什么。\n\n" +
	"你的最终回答会原样作为一条 Telegram 消息（HTML parse_mode）发给用户，只写消息本身，不要别的话：\n" +
	"- 第一行 `<表情> <b>类别 · %s</b>`：派活、拍板用「🟢 已派出」，回答问题用「📊 状态」，出了问题或做不了用「🔴 要你知道」；\n" +
	"- 空一行，每段开头一个表情当标记，段内不再加；项目名后用全角空格「　」再写内容；要用户做的放最前；\n" +
	"- 最后一行 `<i>%s · agentq 对话</i>`；\n" +
	"- 不用感叹号、不客套，不超过 12 行；只用 <b> <i> <code> 标签，正文里的 < > & 写成 &lt; &gt; &amp;。\n\n" +
	"用户的消息（按时间先后）：\n"

func chatPrompt(inbox []notify.InMsg, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, ChatPromptHead, textfmt.WhenCN(now), textfmt.DateCN(now), now.Format("15:04"))
	notify.WriteInMsgs(&b, inbox)
	return b.String()
}

// ReplyChat：对话任务结束时把它的最终回答回发给用户（作为对最后一条消息的回复）。没做成就回原因。
func (a *Service) ReplyChat(j *state.Job) {
	text, todo := "", ""
	if j.Status == state.StatusDone {
		if res := a.Ledger.ReadResult(j); res.HaveTerminal {
			text = strings.TrimSpace(res.Text)
			if j.Kind == state.KindFollowup {
				text, todo = followupAnswer(res)
			}
		}
	}
	if text == "" {
		text = a.notAnswered(j)
	}
	tail := ""
	if todo != "" {
		tail = a.FollowupTodo(j, todo)
	}
	th := replyThread(j)
	a.SendReply(j, text, tail, &th)
}

// notAnswered：这次没答上来时回用户的话。
func (a *Service) notAnswered(j *state.Job) string {
	return a.Notify.RenderMessage("alert", a.Now(), []notify.MsgSection{{Emoji: "🔴", Head: "这条没答上来：" + state.StatusLabel(j.Status),
		Lines: []string{textfmt.OneLine(j.Summary, 200), "详情：agentq logs " + j.ID}}}, "agentq 对话")
}

// SendReply 把回答发给用户（挂在 reply-to 记的那条消息下面），发出后登记 th（空的不登记）；发不出去存下来下次 tick 重发。
// tail 是 agentq 自己补的纯文本（写进了待做、没做成的动作），回答太长要截断时也留着。
// 不进推送队列：用户刚问的，不受合并、夜间和每天上限的限制。
func (a *Service) SendReply(j *state.Job, text, tail string, th *notify.Thread) {
	c, err := a.Telegram.TelegramCreds()
	if c == nil {
		if err != nil {
			a.Logf("Telegram：对话任务 %s 的回答发不出去：%v", j.ID, err)
		}
		return
	}
	tail = textfmt.TruncRunes(tail, replyTailMax)
	text, file := a.capReply(j, text, tail)
	if tail != "" {
		text = strings.TrimRight(text, "\n") + "\n" + textfmt.Esc(tail)
	}
	replyTo := a.Notify.ChatReplyTo(j)
	id, err := a.Telegram.TgSend(c, text, replyTo)
	if err != nil {
		a.Logf("Telegram：对话任务 %s 的回答发送失败，下次 tick 重发：%v", j.ID, c.Redact(err.Error()))
		r := notify.PendingReply{Text: text, ReplyTo: replyTo, At: a.Now(), JobID: j.ID, Thread: th, File: file}
		if err := a.Notify.WithNotifyState(func(s *notify.State) { s.Replies = append(s.Replies, r) }); err != nil {
			a.Logf("Telegram：对话任务 %s 的回答也没能存下来重发：%v", j.ID, err)
		}
		return
	}
	if th != nil {
		if err := a.Notify.WithNotifyState(func(s *notify.State) { s.AddThread(id, *th, a.Now()) }); err != nil {
			a.Logf("Telegram：对话任务 %s 的回答没能登记回复链（再回复这条会按新消息处理）：%v", j.ID, err)
		}
	}
	a.Logf("Telegram：已把对话任务 %s 的回答发给用户", j.ID)
	if file != "" {
		a.sendReplyFile(c, j.ID, file, replyTo)
	}
}

// sendReplyFile 发全文文件。发不出去也不丢：文件还在，消息里写着路径；存下来下次 tick 重发。
func (a *Service) sendReplyFile(c *telegram.TgCreds, jobID, file string, replyTo int64) {
	err := a.Telegram.TgSendFile(c, file, replyFileCaption, replyTo)
	if err == nil {
		return
	}
	a.Logf("Telegram：对话任务 %s 的全文 %s 没发出去，下次 tick 重发：%v", jobID, a.Tilde(file), c.Redact(err.Error()))
	r := notify.PendingReply{ReplyTo: replyTo, At: a.Now(), JobID: jobID, File: file}
	if err := a.Notify.WithNotifyState(func(s *notify.State) { s.Replies = append(s.Replies, r) }); err != nil {
		a.Logf("Telegram：对话任务 %s 的全文也没能存下来重发（文件还在 %s）：%v", jobID, a.Tilde(file), err)
	}
}

// replyThread：回答发出后登记的内容，用户可以一路回复下去。只读续跑的回答接回来源条目。
func replyThread(j *state.Job) notify.Thread {
	if j.Kind == state.KindFollowup {
		return notify.Thread{Kind: notify.ThreadJob, JobID: j.ID, SessionID: j.SessionID, ProjectDir: j.ThreadDir, Project: j.Project, Fingerprint: j.ThreadFP}
	}
	return notify.Thread{Kind: ThreadChat, JobID: j.ID, SessionID: j.SessionID, ProjectDir: j.ProjectDir, Project: j.Project}
}

// replyTailMax：sendReply 的 tail 最多多少字，转义后也不会超过 Telegram 的单条上限。
const replyTailMax = 1000

// replyMaxRunes：对话、Router、追问的回答的硬上限（按用户看到的字数算，标签不算、实体算一个字）。提示词的版式要求只是软约束。
// 只管 sendReply：早报、推送、任务箱结论有各自的版式，不受它影响。
const (
	ReplyMaxRunes    = 600
	replyFileCaption = "全文"
)

func (a *Service) ReplyFilePath(id string) string {
	return filepath.Join(a.DataDir(), "replies", filepath.Base(id)+".md")
}

// capReply：超过上限时全文落 ~/.agentq/replies/<任务id>.md，返回保留前部的消息和要发的文件。tail 也写进文件。
// 文件写不了时照样只发前部，告诉用户原文去哪看（result.json 里一直有）。没超过时原样返回，一个字都不动。
func (a *Service) capReply(j *state.Job, text, tail string) (string, string) {
	if HtmlVisible(text) <= ReplyMaxRunes {
		return text, ""
	}
	path := a.ReplyFilePath(j.ID)
	body := textfmt.HTMLToPlain(text)
	if tail != "" {
		body += "\n\n" + tail
	}
	note, file := "…（全文见文件 "+a.Tilde(path)+"）", path
	if err := fsx.WriteFileAtomic(path, []byte(body+"\n")); err != nil {
		a.Logf("Telegram：对话任务 %s 的全文存不下：%v", j.ID, err)
		note, file = "…（太长了，全文没存下，原文看 agentq logs "+j.ID+"）", ""
	}
	return cutHTML(text, ReplyMaxRunes-len([]rune(note))-1) + "\n" + textfmt.Esc(note), file // 1 是换行，加上这句也不超过上限
}

var (
	htmlTagAt    = regexp.MustCompile(`^</?([a-zA-Z][a-zA-Z0-9-]*)[^<>]*>`)
	htmlEntityAt = regexp.MustCompile(`^&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]*);`)
)

// htmlToken 取 s 开头的一个单位：标签（tag 为真，用户看不到）；实体或一个字（算一个字）。没闭合的尖括号、没分号的 & 算普通字。
func htmlToken(s string) (size int, tag bool, name string) {
	switch s[0] {
	case '<':
		if m := htmlTagAt.FindStringSubmatch(s); m != nil {
			return len(m[0]), true, strings.ToLower(m[1])
		}
	case '&':
		if m := htmlEntityAt.FindString(s); m != "" {
			return len(m), false, ""
		}
	}
	_, w := utf8.DecodeRuneInString(s)
	return w, false, ""
}

// HtmlVisible：用户看到的字数。
func HtmlVisible(s string) int {
	n := 0
	for i := 0; i < len(s); {
		size, tag, _ := htmlToken(s[i:])
		if !tag {
			n++
		}
		i += size
	}
	return n
}

// cutHTML 保留前 n 个字：不从标签或实体中间切，还开着的标签补上闭合（否则 Telegram 整条拒收）。切点后面紧跟的标签不要。
func cutHTML(s string, n int) string {
	var open, atEnd []string
	end, seen := 0, 0
scan:
	for i := 0; i < len(s); {
		size, tag, name := htmlToken(s[i:])
		switch {
		case !tag:
			if seen == n {
				break scan
			}
			seen++
			end, atEnd = i+size, slices.Clone(open)
		case strings.HasPrefix(s[i:], "</"):
			if at := slices.Index(open, name); at >= 0 {
				for k := len(open) - 1; k >= at; k-- { // 从里往外找最近的同名标签
					if open[k] == name {
						open = open[:k]
						break
					}
				}
			}
		case !strings.HasSuffix(s[i:i+size], "/>"):
			open = append(open, name)
		}
		i += size
	}
	out := strings.TrimRight(s[:end], " \t\n")
	for k := len(atEnd) - 1; k >= 0; k-- {
		out += "</" + atEnd[k] + ">"
	}
	return out
}

// replyKeep：没发出去的回答最多重发多久，过了就放弃（只记日志）。
const replyKeep = 24 * time.Hour

// RetryReplies：重发之前没发出去的对话回答（调用方拿着 notify.lock）。
func (a *Service) RetryReplies(s *notify.State, c *telegram.TgCreds) {
	kept := s.Replies[:0]
	for _, r := range s.Replies {
		if a.Now().Sub(r.At) > replyKeep {
			a.Logf("Telegram：对话任务 %s 的回答 %s 起一直没发出去，放弃（原文 agentq logs %s）", r.JobID, textfmt.FormatDuration(replyKeep), r.JobID)
			continue
		}
		if r.Text != "" { // 为空时文字已经到了，只差全文文件
			id, err := a.Telegram.TgSend(c, r.Text, r.ReplyTo)
			if err != nil {
				a.Notify.NotifyErr(s, "重发对话回答失败（下次 tick 再试）："+err.Error())
				kept = append(kept, r)
				continue
			}
			if r.Thread != nil {
				s.AddThread(id, *r.Thread, a.Now())
			}
			a.Logf("Telegram：补发了对话任务 %s 的回答", r.JobID)
		}
		if r.File != "" {
			if err := a.Telegram.TgSendFile(c, r.File, replyFileCaption, r.ReplyTo); err != nil {
				a.Notify.NotifyErr(s, "重发对话回答的全文失败（下次 tick 再试，文件在 "+a.Tilde(r.File)+"）："+c.Redact(err.Error()))
				r.Text, r.Thread = "", nil
				kept = append(kept, r)
				continue
			}
			a.Logf("Telegram：补发了对话任务 %s 的全文", r.JobID)
		}
	}
	s.Replies = kept
}

// firstHoldHeads：status 那一段里说卡在哪的行，解法的缩进行留给 agentq status：摘要只有十来行。
func firstHoldHeads(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, " ") {
			out = append(out, l)
		}
	}
	return out
}

// StatusDigest：「状态」的回答，按推送格式压成十来行。
func (a *Service) StatusDigest(st *state.State) string {
	now := a.Now()
	var secs []notify.MsgSection
	all, _ := a.Notify.Decisions()
	if decs := notify.Askable(all); len(decs) > 0 {
		sec := notify.MsgSection{Emoji: "🔴", Head: fmt.Sprintf("要你定　%d 条", len(decs))}
		for i, d := range decs {
			if i == 2 {
				break
			}
			sec.Lines = append(sec.Lines, d.Project+"　"+textfmt.TruncRunes(d.Title, 30))
		}
		secs = append(secs, sec)
	}
	ulvl, uuntil, uwhy := backend.UsageLevel(st.Usage, now, a.Cfg.UsagePolicy())
	secs = append(secs, notify.MsgSection{Emoji: "⚙️", Head: "调度　" + a.Runner.SchedNote(st, nil, now, ulvl, uuntil, uwhy),
		Lines: append([]string{"额度　" + backend.UsageDescribe(st.Usage, now, a.Cfg.UsagePolicy())}, st.OtherUsageLines(a.Cfg, now)...)})
	if lines := a.Discuss.StatusLines(); len(lines) > 0 {
		secs = append(secs, notify.MsgSection{Emoji: "💬", Head: fmt.Sprintf("等商量　%d 条", len(lines)), Lines: lines})
	}
	if lines := firstHoldHeads(a.Runner.FirstHoldLines(st)); len(lines) > 0 {
		secs = append(secs, notify.MsgSection{Emoji: "🚧", Head: "第一版卡住", Lines: append(lines, "怎么办看 agentq status")})
	}
	running := st.Tasks()
	sec := notify.MsgSection{Emoji: "🔧", Head: fmt.Sprintf("在做　%d/%d", len(running), a.Cfg.MaxParallel)}
	for _, j := range running {
		sec.Lines = append(sec.Lines, fmt.Sprintf("%s　%s（%s）", j.Project, textfmt.TruncRunes(j.Title, 24), textfmt.FormatIdle(now.Sub(j.StartedAt))))
	}
	secs = append(secs, sec)
	if projects, err := a.Backlog.ActiveProjects(); err == nil {
		var todo []string
		for _, p := range projects {
			if items, err := p.Items(); err == nil && len(items) > 0 {
				todo = append(todo, fmt.Sprintf("%s %d", p.Name, len(items)))
			}
		}
		if items, err := a.Inbox.ReadInboxItems(); err == nil && len(items) > 0 {
			todo = append(todo, fmt.Sprintf("%s %d", inbox.InboxJobName, len(items)))
		}
		if len(todo) > 0 {
			secs = append(secs, notify.MsgSection{Emoji: "📋", Head: "待做　" + strings.Join(todo, "，")})
		}
	}
	if stopped := a.Orphan.StoppedOrphans(st); len(stopped) > 0 {
		sec := notify.MsgSection{Emoji: "⏸", Head: fmt.Sprintf("已停止、标记还在　%d 条", len(stopped))}
		for _, s := range stopped {
			sec.Lines = append(sec.Lines, s.String())
		}
		secs = append(secs, sec)
	}
	var ended []*state.Job
	for _, j := range st.Jobs {
		if state.TakesSlot(j.Kind) && j.Status != state.StatusRunning && j.EndedAt != nil {
			ended = append(ended, j)
		}
	}
	if len(ended) > 0 {
		state.SortJobsByEnd(ended)
		sec := notify.MsgSection{Emoji: "🟢", Head: "最近结束"}
		for i, j := range ended {
			if i == 2 {
				break
			}
			sec.Lines = append(sec.Lines, fmt.Sprintf("%s　%s %s", j.Project, textfmt.TruncRunes(j.Title, 24), state.StatusLabel(j.Status)))
		}
		secs = append(secs, sec)
	}
	return a.Notify.RenderMessage("status", now, secs, "agentq status")
}
