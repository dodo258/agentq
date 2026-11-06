package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Telegram 秒回：常驻进程 agentq listen（launchd 服务 io.agentq.agentq.listen，KeepAlive，挂了 launchd 拉起）
// 用 getUpdates 长轮询（timeout 50 秒）收用户的消息，收到马上处理：能用代码答的（见 fastpath.go）当场回；其余先回一句「收到」
// （有排队的说前面还有几条），附件（图片、录屏、文件，20MB 内）下载下来，消息进 notify.json 的收件箱，
// 再试着启动对话任务（同一时间只有一个）。对话任务结束时任务外壳触发的 tick 回发回答，并接着启动排队的下一批。
// listen 拿着 listen.lock 期间 tick 不收消息；listen 没在跑（没装、挂了还没被拉起）时 tick 每 5 分钟短轮询兜底。
// 两边靠这把锁不会同时 getUpdates；offset 存在 notify.json 里，收到就确认，同一条不会处理两次。
const (
	listenPollWait  = 50               // getUpdates 长轮询挂多少秒
	ListenIdleWait  = time.Minute      // 没配 Telegram、推送关着、配置读不出时隔多久再看
	listenRetryMax  = time.Minute      // 收消息出错时最多隔多久再试
	listenServeWait = 10 * time.Second // 启动对话任务前等 tick.lock 最多这么久；等不到就由正在跑的 tick 接着启动
	listenNoteEvery = time.Hour        // 同一个问题隔这么久才再记一次日志
)

func ListenLabel() string { return config.LaunchdLabel + ".listen" }

func (a *Service) ListenLockPath() string { return filepath.Join(a.DataDir(), "listen.lock") }

func (a *Service) listenPidPath() string { return filepath.Join(a.DataDir(), "listen.pid") }

func (a *Service) InboxDir() string { return filepath.Join(a.DataDir(), "inbox") }

func (a *Service) ChatFilesDir(id string) string { return filepath.Join(a.JobDir(id), "files") }

// listener：常驻循环里要跨轮记住的东西。
type listener struct {
	fails    int
	note     string
	noteAt   time.Time
	self     string
	selfInfo os.FileInfo
}

// say 记一行日志；同一句话 listenNoteEvery 内只记一次。
func (l *listener) say(a *Service, msg string) {
	now := a.Now()
	if msg == l.note && now.Sub(l.noteAt) < listenNoteEvery {
		return
	}
	l.note, l.noteAt = msg, now
	a.Logf("监听：%s", msg)
}

// CmdListen 是 agentq listen：一直跑，不返回（换了新版本时返回 nil，让 launchd 用新版本重启）。
func (a *Service) CmdListen() error {
	lock, err := fsx.AcquireLock(a.ListenLockPath(), 0)
	if err != nil {
		return err
	}
	if lock == nil {
		a.Logf("监听：已经有一个 agentq listen 在跑，等它退出")
		for lock == nil {
			if lock, err = fsx.AcquireLock(a.ListenLockPath(), time.Hour); err != nil {
				return err
			}
		}
	}
	defer lock.Close()
	if err := fsx.WriteFileAtomic(a.listenPidPath(), []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
		a.Logf("监听：写 %s 失败（不影响收消息）：%v", a.listenPidPath(), err)
	}
	l := &listener{}
	if self, err := os.Executable(); err == nil {
		l.self = self
		l.selfInfo, _ = os.Stat(self)
	}
	a.Logf("监听：启动（pid %d），长轮询收 Telegram 消息", os.Getpid())
	for {
		if l.selfInfo != nil {
			if fi, err := os.Stat(l.self); err == nil && !os.SameFile(fi, l.selfInfo) {
				a.Logf("监听：%s 换了新版本，退出，由 %s 用新版本重启", l.self, a.Service.Svc().Words().Name)
				return nil
			}
		}
		if d := a.listenStep(l); d > 0 {
			time.Sleep(d)
		}
	}
}

// listenStep 收一轮：每轮重读配置（改了 notify_enabled、换了凭据不用重启），长轮询一次，处理收到的，
// 再看收件箱里有没有等着交给 claude 的。返回下一轮前要歇多久。
func (a *Service) listenStep(l *listener) time.Duration {
	cfg, err := config.Load(a.ConfigPath())
	if err != nil {
		l.say(a, "配置读不出，先不收消息："+err.Error())
		return ListenIdleWait
	}
	a.Cfg = cfg
	if !cfg.NotifyEnabled {
		l.say(a, "推送关着（notify_enabled=false），不收消息")
		return ListenIdleWait
	}
	c, err := a.Telegram.TelegramCreds()
	if c == nil {
		if err != nil {
			l.say(a, "Telegram 凭据用不了，不收消息："+err.Error())
		} else {
			l.say(a, "没配 Telegram（agentq notify setup 配），不收消息")
		}
		return ListenIdleWait
	}
	s, err := a.Notify.LoadNotify()
	if err != nil {
		l.say(a, "读不了 "+a.Notify.NotifyPath()+"，先不收消息："+err.Error())
		return ListenIdleWait
	}
	ups, err := a.Telegram.TgUpdates(c, s.UpdateOffset, listenPollWait)
	if err != nil {
		l.fails++
		d := min(time.Duration(1<<min(l.fails, 6))*time.Second, listenRetryMax)
		var te *telegram.TgError
		if errors.As(err, &te) && te.Code == 409 { // 设了 webhook，或者别处也在 getUpdates
			d = listenRetryMax
		}
		l.say(a, "收消息失败，过一会儿再试："+err.Error())
		return d
	}
	if l.fails > 0 {
		a.Logf("监听：收消息恢复了")
		l.fails, l.note = 0, ""
	}
	if len(ups) > 0 && !a.TakeUpdatesSafe(c, s.UpdateOffset == 0, ups) {
		return listenRetryMax // 存不下：这几条下次还会收到。不歇的话 getUpdates 马上又返回同样的，就是死循环
	}
	a.listenServe(c)
	return 0
}

// listenServe：收件箱里有消息时拿 tick.lock 试着启动对话任务（上一个还在跑就不启动，它结束时的 tick 会接上）。
func (a *Service) listenServe(c *telegram.TgCreds) {
	if s, err := a.Notify.LoadNotify(); err != nil || len(s.Inbox) == 0 {
		return
	}
	lock, err := fsx.AcquireLock(a.LockPath(), listenServeWait)
	if err != nil || lock == nil {
		return // tick 正在跑：它末尾会接着处理收件箱
	}
	defer lock.Close()
	st, err := state.Load(a.StatePath())
	if err != nil {
		a.Logf("监听：状态文件读不出，先不启动对话任务，等 tick 处理：%v", err)
		return
	}
	if err := a.Notify.WithNotify(func(s *notify.State) error { a.ServeInbox(s, st, c); return nil }); err != nil {
		a.Logf("监听：%v", err)
	}
}

// PollIfNoListener：tick 兜底收消息。listen 在跑（拿着 listen.lock）就不收，免得两处抢同一批更新；
// 不在跑时拿着这把锁短轮询一次，期间 listen 被拉起也会先等锁。
func (a *Service) PollIfNoListener(c *telegram.TgCreds) {
	lock, err := fsx.AcquireLock(a.ListenLockPath(), 0)
	if err != nil || lock == nil {
		return
	}
	defer lock.Close()
	s, err := a.Notify.LoadNotify()
	if err != nil {
		a.Logf("推送：%v", err)
		return
	}
	ups, err := a.Telegram.TgUpdates(c, s.UpdateOffset, 0)
	if err != nil {
		_ = a.Notify.WithNotify(func(s *notify.State) error {
			a.Notify.NotifyErr(s, "收 Telegram 消息失败（下次 tick 再收）："+err.Error())
			return nil
		})
		return
	}
	if len(ups) > 0 {
		a.TakeUpdatesSafe(c, s.UpdateOffset == 0, ups)
	}
}

// ListenRunning：有 agentq listen 拿着 listen.lock 时返回它的 pid（读不到 pid 文件返回 0）和真。
func (a *Service) ListenRunning() (int, bool) {
	lock, err := fsx.AcquireLock(a.ListenLockPath(), 0)
	if err != nil {
		return 0, false
	}
	if lock != nil {
		lock.Close()
		return 0, false
	}
	b, _ := os.ReadFile(a.listenPidPath())
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, true
}

// ListenStatus：status 里「监听」一行；没配 Telegram 或推送关着时返回空（不显示）。
func (a *Service) ListenStatus() string {
	if !a.Cfg.NotifyEnabled {
		return ""
	}
	if c, _ := a.Telegram.TelegramCreds(); c == nil {
		return ""
	}
	if pid, ok := a.ListenRunning(); ok {
		if pid > 0 {
			return fmt.Sprintf("在跑（pid %d），你的 Telegram 消息随到随回", pid)
		}
		return "在跑，你的 Telegram 消息随到随回"
	}
	return "没在跑，tick 每 5 分钟兜底收一次消息（" + a.Service.ServiceInstallCommand() + " 装上常驻的 " + a.Service.ListenLabel() + "）"
}

const (
	AckNow       = "收到，交给 claude 了，答完回你。"
	AckQueued    = "收到，前面还有 %d 条在答，答完就轮到这条。"
	ackUnknown   = "现在只看得懂文字、图片、视频和文件，换成一句话发我。"
	AckVoice     = "语音听不了，打字发我。"
	textOnlyFile = "（没写字，只发了附件）"
)

// TakeUpdatesSafe：处理一批更新时出了意外（panic），记日志并把 offset 推过这批，
// 免得一条怪消息让 listen 反复崩溃重启、后面的消息全收不到。
// 返回 false 表示这批没处理（存不下），下次还会收到。
func (a *Service) TakeUpdatesSafe(c *telegram.TgCreds, first bool, ups []telegram.TgUpdate) (ok bool) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		ok = true
		last := ups[len(ups)-1].UpdateID
		for _, u := range ups {
			last = max(last, u.UpdateID)
		}
		a.Logf("Telegram：处理 %d 条更新时出错，跳过这批（offset 推到 %d）：%v", len(ups), last+1, p)
		if err := a.Notify.WithNotify(func(s *notify.State) error { s.UpdateOffset = max(s.UpdateOffset, last+1); return nil }); err != nil {
			a.Logf("Telegram：跳过这批时存 offset 失败：%v", err)
		}
	}()
	return a.TakeUpdates(c, first, ups)
}

// TakeUpdates 处理一批更新：只认用户的 chat id；第一次收消息（还没有 offset）时跳过配机器人那会儿的旧消息；
// 能用代码答的当场回（只认整句，回复别的消息、带附件的不算，见 fastpath.go）；附件先下载；其余进收件箱并回一句「收到」。offset 和收件箱一起存进 notify.json（拿着 notify.lock，
// 下载和发消息都在锁外，免得卡住 notify send）。没存下时 offset 不前进，下次会再收到这几条，返回 false。
// 代码直接答的消息是当场回的，要先确认存得下：存不下时一条都不处理，免得同一条消息每收到一次就回一次。
func (a *Service) TakeUpdates(c *telegram.TgCreds, first bool, ups []telegram.TgUpdate) bool {
	if err := a.Notify.WithNotify(func(*notify.State) error { return nil }); err != nil {
		a.Logf("Telegram：收到 %d 条消息，但推送状态存不下，这次不处理（下次再收一遍）：%v", len(ups), err)
		return false
	}
	maxID := int64(-1)
	var queue []notify.InMsg
	var group []string // 和 queue 一一对应：相册的几张图只回一次「收到」
	reply := func(text string, to int64, what string) {
		if _, err := a.Telegram.TgSend(c, text, to); err != nil {
			a.Logf("Telegram：回复用户（%s）失败：%v", what, err)
		}
	}
	for _, u := range ups {
		maxID = max(maxID, u.UpdateID)
		if u.CallbackQuery != nil { // 按钮：代码处理，不起模型（见 cards.go）
			a.handleCallback(c, u.CallbackQuery)
			continue
		}
		m := u.Message
		if m == nil {
			continue
		}
		if m.Chat.ID != c.ChatID {
			a.Logf("Telegram：忽略 chat id %d 发来的消息（只认 telegram.env 里配的那一个）", m.Chat.ID)
			continue
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			text = strings.TrimSpace(m.Caption)
		}
		if first && a.Now().Sub(time.Unix(m.Date, 0)) > chatFirstPollMaxAge {
			a.Logf("Telegram：第一次收消息，跳过 %s 发的旧消息「%s」", textfmt.WhenCN(time.Unix(m.Date, 0)), textfmt.OneLine(text, 20))
			continue
		}
		if isStatusAsk(text) && !telegram.HasAttachment(m) {
			digest := "📊 状态读不出：" + a.StatePath()
			if st, err := state.Load(a.StatePath()); err == nil {
				digest = a.StatusDigest(st)
			}
			if _, err := a.Telegram.TgSend(c, digest, m.MessageID); err != nil {
				a.Logf("Telegram：回复「状态」失败：%v", err)
			} else {
				a.Logf("Telegram：回了用户的「%s」（状态摘要）", textfmt.OneLine(text, 20))
			}
			continue
		}
		stopJob := ""
		if !telegram.HasAttachment(m) && m.ReplyTo == nil {
			switch cmd, id := parseFastCmd(text); cmd {
			case fastNone:
			case fastStop:
				if _, err := state.Load(a.StatePath()); err != nil {
					reply(textfmt.Esc("状态文件读不出，没法停任务："+err.Error()), m.MessageID, "停任务")
					continue
				}
				stopJob = id // 要拿 tick.lock：放进收件箱，由 serveStops 执行
			default:
				reply(a.fastAnswer(cmd), m.MessageID, "「"+textfmt.OneLine(text, 20)+"」")
				a.Logf("Telegram：回了用户的「%s」（代码直接答）", textfmt.OneLine(text, 20))
				continue
			}
		}
		if nums, ok := suggest.ParseDoSuggest(text); ok && !telegram.HasAttachment(m) && a.Suggest.DoSuggestApplies(m, nums) {
			// 「做 1 3」：把闲时找活留在「建议」里的条目移进「待做」。代码就能答，不起模型
			if _, err := a.Telegram.TgSend(c, a.Suggest.DoSuggestions(nums), m.MessageID); err != nil {
				a.Logf("Telegram：回复「%s」失败：%v", textfmt.OneLine(text, 20), err)
			} else {
				a.Logf("Telegram：回了用户的「%s」（闲时建议）", textfmt.OneLine(text, 20))
			}
			continue
		}
		if nums, ok := suggest.ParseNotDoSuggest(text); ok && !telegram.HasAttachment(m) && a.Suggest.DoSuggestApplies(m, nums) {
			// 「不做 1 3」：写进项目的不做清单。和「做 N」共用校验，一个有效编号都没有时放行给模型
			if _, err := a.Telegram.TgSend(c, a.Suggest.RejectSuggestions(nums, text), m.MessageID); err != nil {
				a.Logf("Telegram：回复「%s」失败：%v", textfmt.OneLine(text, 20), err)
			} else {
				a.Logf("Telegram：回了用户的「%s」（不做清单）", textfmt.OneLine(text, 20))
			}
			continue
		}
		if a.Release.ReleaseIntercept(c, m) { // 发版提示的回复和「发」：代码处理，不交给模型
			continue
		}
		files, notes := a.fetchAttachments(c, m)
		for _, n := range notes {
			reply(textfmt.Esc(n), m.MessageID, "附件说明")
		}
		if text == "" && len(files) == 0 {
			if len(notes) == 0 {
				reply(ackUnknown, m.MessageID, "看不懂的消息")
			}
			continue
		}
		if text == "" {
			text = textOnlyFile
		}
		in := notify.InMsg{ID: m.MessageID, At: time.Unix(m.Date, 0), Text: text, Files: files, StopJob: stopJob}
		if m.ReplyTo != nil {
			in.ReplyToID = m.ReplyTo.MessageID
			in.ReplyTo = strings.TrimSpace(m.ReplyTo.Text)
			if in.ReplyTo == "" {
				in.ReplyTo = strings.TrimSpace(m.ReplyTo.Caption)
			}
		}
		queue = append(queue, in)
		group = append(group, m.MediaGroupID)
	}
	if maxID < 0 {
		return true
	}
	err := a.Notify.WithNotify(func(s *notify.State) error {
		s.UpdateOffset = max(s.UpdateOffset, maxID+1)
		s.Inbox = append(s.Inbox, queue...)
		return nil
	})
	if err != nil {
		a.Logf("Telegram：收到的 %d 条消息没存下（下次再收一遍）：%v", len(ups), err)
		return false
	}
	if st, err := state.Load(a.StatePath()); err == nil && a.chatRefusal(st, a.Now()) != "" {
		return true // 马上就会回「这条现在没法交给 claude」和原因，不先说「收到」
	}
	// 排着的消息会和这条一起交给下一个对话任务，所以「前面」只算正在答的那几条
	ahead := a.answeringCount()
	for i, in := range queue {
		if i > 0 && group[i] != "" && group[i] == group[i-1] {
			continue
		}
		if in.StopJob != "" {
			a.Logf("Telegram：收到用户的「%s」，马上停", textfmt.OneLine(in.Text, 40))
			continue
		}
		ack := strings.ReplaceAll(AckNow, "claude", a.DefaultBackend().Name())
		if ahead > 0 {
			ack = fmt.Sprintf(AckQueued, ahead)
		}
		reply(ack, in.ID, "收到")
		a.Logf("Telegram：收到用户的消息「%s」（附件 %d 个），排队交给 %s", textfmt.OneLine(in.Text, 40), len(in.Files), a.DefaultBackend().Name())
	}
	return true
}

// fetchAttachments 把消息里的图片、录屏、文件下载到 ~/.agentq/inbox/<消息号>-<文件名>（启动对话任务时挪进任务目录）。
// 超过 20MB 的、下载失败的、语音，各返回一句回给用户的说明。
func (a *Service) fetchAttachments(c *telegram.TgCreds, m *telegram.TgMsg) (files, notes []string) {
	type att struct {
		f          *telegram.TgFile
		what, name string
	}
	var list []att
	if len(m.Photo) > 0 {
		list = append(list, att{&m.Photo[len(m.Photo)-1], "图片", "photo"})
	}
	for _, x := range []att{{m.Video, "视频", "video"}, {m.Animation, "动图", "animation"}, {m.VideoNote, "视频", "video_note"},
		{m.Document, "文件", "file"}, {m.Audio, "音频", "audio"}} {
		if x.f != nil {
			if x.f.FileName != "" {
				x.name = x.f.FileName
			}
			list = append(list, x)
		}
	}
	if m.Voice != nil {
		notes = append(notes, AckVoice)
	}
	for _, x := range list {
		if x.f.FileSize > telegram.TgDownloadMax {
			notes = append(notes, fmt.Sprintf("这个%s有 %s，超过 Telegram 给机器人下载的上限 20 MB，没收到；压缩一下再发，或者放到电脑上告诉我路径。",
				x.what, textfmt.HumanSize(x.f.FileSize)))
			continue
		}
		dest := filepath.Join(a.InboxDir(), fmt.Sprintf("%d-%s", m.MessageID, SafeFileName(x.name)))
		remote, err := a.Telegram.TgDownload(c, x.f.FileID, dest)
		if err != nil {
			a.Logf("Telegram：用户发的%s没下载下来：%v", x.what, err)
			notes = append(notes, fmt.Sprintf("这个%s没下载下来（%s），再发一次试试。", x.what, textfmt.OneLine(err.Error(), 80)))
			continue
		}
		if filepath.Ext(dest) == "" && filepath.Ext(remote) != "" {
			if err := os.Rename(dest, dest+filepath.Ext(remote)); err == nil {
				dest += filepath.Ext(remote)
			}
		}
		files = append(files, dest)
	}
	return files, notes
}

// SafeFileName：只取文件名本身，去掉路径分隔符和开头的点，最长 80 字。
func SafeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r < ' ' {
			return '_'
		}
		return r
	}, filepath.Base(name))
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "file"
	}
	if r := []rune(name); len(r) > 80 {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 16 { // 「v1.2 一整句话」这种，点后面不是扩展名
			ext = ""
		}
		name = string(r[:80-len([]rune(ext))]) + ext
	}
	return name
}

// answeringCount：正在跑的对话任务手上有几条用户的消息（读任务目录的 inbox.json；读不到算 1），没在跑返回 0。
func (a *Service) answeringCount() int {
	st, err := state.Load(a.StatePath())
	if err != nil {
		return 0
	}
	for _, j := range st.Running() {
		if j.Kind != state.KindChat && j.Kind != state.KindRouter {
			continue
		}
		var msgs []notify.InMsg
		if b, err := os.ReadFile(filepath.Join(a.JobDir(j.ID), "inbox.json")); err == nil && json.Unmarshal(b, &msgs) == nil && len(msgs) > 0 {
			return len(msgs)
		}
		return 1
	}
	return 0
}
