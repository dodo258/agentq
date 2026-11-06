package tests

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentq/internal/chat"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// Telegram 秒回（agentq listen）和收发图片、录屏的测试。假 Telegram 见 notify_test.go 的 fakeTG。

// 集成：真二进制常驻 agentq listen，假 Telegram 长轮询。验收要求的几件事一个用例走完：
// 长轮询（timeout 50）收到「状态」几秒内回；别人发的不理；派活先回「收到」再交给 claude，答完回发；
// 第一个还在答时来的消息说「前面还有 1 条在答」、不另起 claude，答完由任务结束后的 tick 接着处理；
// 重启 listen 后 offset 接着上次的，同一条不处理两次。
func TestIntegrationListen(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"notify_enabled": true, "tick_after_job": true, "paused": true})
	testkit.MkProject(t, e.root, "demo", "")
	tg := newFakeTG(t)
	e.extra = append(e.extra, "AGENTQ_TELEGRAM_API="+tg.srv.URL)
	sec := filepath.Join(e.home, ".agentq", "secrets")
	os.MkdirAll(sec, 0o700)
	os.WriteFile(filepath.Join(sec, "telegram.env"), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)), 0o600)

	var logBuf syncBuffer
	startListen := func() *exec.Cmd {
		cmd := exec.Command(agentqBin, "listen")
		var envs []string
		for _, kv := range os.Environ() {
			if name, _, _ := strings.Cut(kv, "="); name != "HOME" {
				envs = append(envs, kv)
			}
		}
		cmd.Env = append(append(envs, "HOME="+e.home, "AGENTQ_FAKE_LOG="+e.fakeLg), e.extra...)
		cmd.Stdout, cmd.Stderr = &logBuf, &logBuf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	stop := func(cmd *exec.Cmd) {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}
	l := startListen()
	defer func() { stop(l) }()
	waitFor(t, 10*time.Second, "listen 挂起长轮询", func() bool {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		return tg.polling > 0
	})

	// 长轮询中途来消息：别人发的不理，用户的「状态」几秒内就回
	sentAt := time.Now()
	tg.msg(1, 4242, 1, "状态", "")
	tg.msg(2, ownerChat, 2, "状态", "")
	waitFor(t, 10*time.Second, "回复「状态」", func() bool { return tg.count() >= 1 })
	if d := time.Since(sentAt); d > 5*time.Second {
		t.Fatalf("「状态」%s 才回，应随到随回", d)
	}
	if got := tg.texts(); !strings.HasPrefix(got[0], "📊 <b>状态 · ") {
		t.Fatalf("应回状态摘要：%q", got)
	}
	tg.mu.Lock()
	if tg.timeouts[0] != "50" {
		t.Errorf("应长轮询 timeout=50：%v", tg.timeouts)
	}
	tg.mu.Unlock()

	// 派活：先回「收到」，对话任务在跑时再来一条，告诉用户前面还有 1 条，不另起 claude
	tg.msg(3, ownerChat, 3, "demo：加个按月对比 TGDISPATCH SLEEP=3", "")
	waitFor(t, 10*time.Second, "回「收到」", func() bool { return tg.count() >= 2 })
	waitFor(t, 15*time.Second, "对话任务启动", func() bool { return len(chatJobs(e)) == 1 })
	tg.msg(4, ownerChat, 4, "还有 my-app 的测试修了没 QUEUED", "")
	waitFor(t, 10*time.Second, "回排队的「收到」", func() bool { return tg.count() >= 3 })
	if got := tg.texts(); got[1] != chat.AckNow || got[2] != fmt.Sprintf(chat.AckQueued, 1) {
		t.Fatalf("应先说收到，排队的说前面还有 1 条在答：%q", got)
	}
	// 第一个答完：回发回答，再由任务结束后的 tick 启动排队的那条，答完也回发；任何时候最多 1 个对话任务在跑
	waitFor(t, 60*time.Second, "两次对话都收尾、回答都发出", func() bool {
		jobs := chatJobs(e)
		running := 0
		for _, j := range jobs {
			if j.Status == state.StatusRunning {
				running++
			}
		}
		if running > 1 {
			t.Fatalf("同时有 %d 个对话任务在跑", running)
		}
		return len(jobs) == 2 && running == 0 && tg.count() >= 5
	})
	got := tg.texts()
	if len(got) != 5 || !strings.HasPrefix(got[3], "🟢 <b>已派出") || !strings.Contains(got[4], "做完了") {
		t.Fatalf("两次回答应依次发给用户：%q", got)
	}
	if rp := tg.sent[4].Get("reply_parameters"); !strings.Contains(rp, `"message_id":4`) {
		t.Fatalf("第二次回答应挂在排队那条下面：%q", rp)
	}
	for _, v := range tg.sent {
		if v.Get("chat_id") != fmt.Sprint(ownerChat) {
			t.Fatalf("发给了别人：%v", v)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "demo", "docs", "BACKLOG.md")); !strings.Contains(string(b), "加个按月对比 —— 来自 Telegram") {
		t.Fatalf("对话任务应写进 BACKLOG：%s", b)
	}

	// 重启 listen：接着上次的 offset，前面几条不再处理
	stop(l)
	tg.mu.Lock()
	before := len(tg.offsets)
	tg.mu.Unlock()
	l = startListen()
	waitFor(t, 10*time.Second, "重启后挂起长轮询", func() bool {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		return len(tg.offsets) > before && tg.polling > 0
	})
	tg.mu.Lock()
	off := tg.offsets[before]
	tg.mu.Unlock()
	if off != "5" || tg.count() != 5 {
		t.Fatalf("重启后应从 offset 5 收、不重复回复：offset=%s sent=%q", off, tg.texts())
	}
	out := logBuf.String() + e.launchdLog()
	if strings.Contains(out, fakeToken) {
		t.Fatalf("日志里有 token：%s", out)
	}
	if !strings.Contains(out, "忽略 chat id 4242") {
		t.Fatalf("应记下忽略了别人的消息：%s", out)
	}
}

// syncBuffer：接子进程输出、又要在子进程还活着时读的 Buffer。exec 用自己的 goroutine 往 Stdout、Stderr 写。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func chatJobs(e *testEnv) []*Job {
	st, err := state.Load(filepath.Join(e.home, ".agentq", "state.json"))
	if err != nil {
		return nil
	}
	var out []*Job
	for _, j := range st.Jobs {
		if j.Kind == state.KindChat {
			out = append(out, j)
		}
	}
	return out
}

// listen 在跑（拿着 listen.lock）时 tick 不收消息，免得两处抢同一批更新；listen 不在时 tick 兜底收。
func TestTickPollsOnlyWithoutListener(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.msg(1, ownerChat, 1, "状态", "")
	lock, err := fsx.AcquireLock(e.a.Chat.ListenLockPath(), 0)
	if err != nil || lock == nil {
		t.Fatal("拿不到 listen.lock", err)
	}
	e.flush()
	if len(e.tg.offsets) != 0 || e.tg.count() != 0 {
		t.Fatalf("listen 在跑时 tick 不该 getUpdates：offsets=%v sent=%q", e.tg.offsets, e.tg.texts())
	}
	if _, ok := e.a.Chat.ListenRunning(); !ok {
		t.Fatal("拿着 listen.lock 时应认为 listen 在跑")
	}
	lock.Close()
	e.flush()
	if e.tg.count() != 1 || e.tg.timeouts[0] != "0" {
		t.Fatalf("listen 不在时 tick 应短轮询兜底：sent=%q timeouts=%v", e.tg.texts(), e.tg.timeouts)
	}
}

// 收：用户发来的截图（带说明）下载下来，交给 claude 时挪进那次对话任务目录的 files/，路径写进 prompt；
// 超过 20MB 的不下载、回一句说明；语音回一句听不了；相册的几张图只回一次「收到」。
func TestListenReceivesAttachments(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	png := []byte("\x89PNG fake screenshot")
	e.tg.media(1, 11, "photo", "这里怎么歪了", "", png, 0, "")
	e.tg.media(2, 12, "document", "", "录屏.mov", nil, 25<<20, "")
	e.tg.media(3, 13, "voice", "", "", nil, 1000, "")
	e.tg.media(4, 14, "photo", "两张对比", "", []byte("a"), 0, "g1")
	e.tg.media(5, 15, "photo", "", "", []byte("b"), 0, "g1")
	e.tg.media(6, 16, "document", "看看这个日志", "../../etc/app.log", []byte("log"), 0, "")
	e.flush()

	texts := e.tg.texts()
	var big, voice int
	for _, s := range texts {
		if strings.Contains(s, "25.0 MB") && strings.Contains(s, "20 MB") {
			big++
		}
		if s == chat.AckVoice {
			voice++
		}
	}
	if big != 1 || voice != 1 {
		t.Fatalf("太大的、语音各回一句说明：%q", texts)
	}
	if acks := e.tg.acks(); len(acks) != 3 {
		t.Fatalf("截图、相册（一次）、日志各回一次「收到」：%q", acks)
	}
	if len(e.chats) != 1 {
		t.Fatalf("应交给一次 claude：%d", len(e.chats))
	}
	j := e.st.Jobs[0]
	shot := filepath.Join(e.a.Chat.ChatFilesDir(j.ID), "11-photo")
	if b, err := os.ReadFile(shot); err != nil || !bytes.Equal(b, png) {
		t.Fatalf("截图应存进任务目录 %s：%v", shot, err)
	}
	p := e.chats[0]
	for _, want := range []string{"这里怎么歪了\n   附件：" + shot, "两张对比\n   附件：", "（没写字，只发了附件）\n   附件：", "16-app.log", "agentq notify file", "BACKLOG 条目"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 里缺「%s」：\n%s", want, p)
		}
	}
	if strings.Contains(p, "录屏.mov") || strings.Contains(p, "etc/app.log") {
		t.Fatalf("没下载的不该写进 prompt，文件名不能带路径：\n%s", p)
	}
	if left, _ := os.ReadDir(e.a.Chat.InboxDir()); len(left) != 0 {
		t.Fatalf("交给 claude 后 inbox 里不该留文件：%v", left)
	}
}

// 发：图片走 sendPhoto（被拒就改按文件发），录屏走 sendVideo，超过 50MB 先压缩，压不下就发一句带路径的话；
// agentq notify file 在对话任务里执行时挂在用户那条消息下面。
func TestSendFiles(t *testing.T) { // 不并行：要设 AGENTQ_JOB 环境变量
	e := newNotifyEnv(t, true)
	c, _ := e.a.Telegram.TelegramCreds()
	dir := t.TempDir()
	shot := filepath.Join(dir, "after.png")
	os.WriteFile(shot, []byte("png-bytes"), 0o644)
	if err := e.a.Telegram.TgSendFile(c, shot, "改完的样子", 0); err != nil {
		t.Fatal(err)
	}
	ups := e.tg.uploaded()
	if len(ups) != 1 || ups[0].Method != "sendPhoto" || ups[0].Name != "after.png" || string(ups[0].Data) != "png-bytes" ||
		ups[0].Form.Get("caption") != "改完的样子" || ups[0].Form.Get("chat_id") != fmt.Sprint(ownerChat) {
		t.Fatalf("截图应按图片发：%+v", ups)
	}

	e.tg.rejectPic = true
	if err := e.a.Telegram.TgSendFile(c, shot, "", 0); err != nil {
		t.Fatal(err)
	}
	if ups = e.tg.uploaded(); len(ups) != 2 || ups[1].Method != "sendDocument" {
		t.Fatalf("sendPhoto 被拒应改按文件发：%+v", ups)
	}
	e.tg.rejectPic = false

	// 超过 50MB 的录屏：压缩后发；压不下就发路径
	big := filepath.Join(dir, "rec.mov")
	f, _ := os.Create(big)
	f.Truncate(60 << 20)
	f.Close()
	e.a.Telegram.Compress = func(src, out string) (string, error) {
		p := filepath.Join(out, "small.mp4")
		return p, os.WriteFile(p, []byte("mp4"), 0o644)
	}
	if err := e.a.Telegram.TgSendFile(c, big, "", 0); err != nil {
		t.Fatal(err)
	}
	if ups = e.tg.uploaded(); len(ups) != 3 || ups[2].Method != "sendVideo" || ups[2].Name != "small.mp4" {
		t.Fatalf("大录屏应压缩后按视频发：%+v", ups)
	}
	e.a.Telegram.Compress = func(src, out string) (string, error) { return "", fmt.Errorf("没有 ffmpeg") }
	if err := e.a.Telegram.TgSendFile(c, big, "", 0); err != nil {
		t.Fatal(err)
	}
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "60.0 MB") || !strings.Contains(got[0], big) || !strings.Contains(got[0], "没有 ffmpeg") {
		t.Fatalf("压不下应发一句带路径的话：%q", got)
	}

	// notify file：在对话任务里执行，挂在用户的消息下
	e.write(filepath.Join(e.a.JobDir("1011-120000-Telegram对话"), "reply-to"), "42\n")
	t.Setenv(proc.JobEnv, "1011-120000-Telegram对话")
	if err := e.a.CmdNotify([]string{"file", shot, "效果图"}); err != nil {
		t.Fatal(err)
	}
	if ups = e.tg.uploaded(); len(ups) != 4 || !strings.Contains(ups[3].Form.Get("reply_parameters"), `"message_id":42`) {
		t.Fatalf("notify file 应挂在用户那条消息下：%+v", ups[len(ups)-1])
	}
	if err := e.a.CmdNotify([]string{"file", filepath.Join(dir, "nope.png")}); err == nil || !strings.Contains(err.Error(), "文件不在") {
		t.Fatalf("文件不在应报错：%v", err)
	}
}

// notify send --file：附件跟在那条推送后面发；没配 Telegram 时把路径写进本机通知。
func TestNotifySendWithFile(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	shot := filepath.Join(t.TempDir(), "v2.png")
	os.WriteFile(shot, []byte("png"), 0o644)
	if err := e.a.CmdNotify([]string{"send", "--file=" + shot, "done", "demo　第一版做好了", "打开看看"}); err != nil {
		t.Fatal(err)
	}
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "第一版做好了") || strings.Contains(got[0], "附件") {
		t.Fatalf("消息照常发，附件不写进正文：%q", got)
	}
	if ups := e.tg.uploaded(); len(ups) != 1 || ups[0].Method != "sendPhoto" {
		t.Fatalf("附件应跟在消息后按图片发：%+v", ups)
	}

	m := newNotifyEnv(t, false)
	if err := m.a.CmdNotify([]string{"send", "--file=" + shot, "done", "demo　第一版做好了"}); err != nil {
		t.Fatal(err)
	}
	if len(m.mac) != 1 || !strings.Contains(m.mac[0], "附件 "+shot) {
		t.Fatalf("本机通知里应写附件路径：%q", m.mac)
	}
	if err := m.a.CmdNotify([]string{"send", "--file=/no/such.png", "done", "x"}); err == nil {
		t.Fatal("附件不在应报错")
	}
}

// 巡逻：配了 Telegram 时，监听服务没装上、装着却没在跑都报；在跑不报。
func TestPatrolListenService(t *testing.T) {
	t.Parallel()
	e := newPatrolEnv(t)
	os.MkdirAll(filepath.Join(e.a.Home, ".agentq", "secrets"), 0o700)
	os.WriteFile(e.a.Telegram.TelegramEnvPath(), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)), 0o600)
	base := e.launchd
	titles := func() string {
		r := e.patrol()
		var out []string
		for _, is := range append(r.New, r.Old...) {
			out = append(out, is.Title)
		}
		return strings.Join(out, "；")
	}
	if got := titles(); !strings.Contains(got, "Telegram 监听 io.agentq.agentq.listen 没装上") {
		t.Fatalf("没装上应报：%s", got)
	}
	e.launchd = base + "-\t0\t" + chat.ListenLabel() + "\n"
	e.now = e.now.Add(2 * time.Hour)
	if got := titles(); !strings.Contains(got, "没在跑") {
		t.Fatalf("装着没在跑应报：%s", got)
	}
	e.launchd = base + "4321\t0\t" + chat.ListenLabel() + "\n"
	e.now = e.now.Add(2 * time.Hour)
	if got := titles(); strings.Contains(got, "监听") {
		t.Fatalf("在跑不该报：%s", got)
	}
}

// 文件名超长、点后面又是一整句话（不是扩展名）时不能崩：截前 80 个字；真扩展名保留。
func TestSafeFileNameLong(t *testing.T) {
	t.Parallel()
	sentence := "v1.2 " + strings.Repeat("仪表盘布局在小屏上歪了", 10)
	if got := chat.SafeFileName(sentence); len([]rune(got)) != 80 || !strings.HasPrefix(got, "v1.2 ") {
		t.Fatalf("应截成前 80 个字：%q", got)
	}
	long := strings.Repeat("长", 100) + ".png"
	if got := chat.SafeFileName(long); len([]rune(got)) != 80 || !strings.HasSuffix(got, ".png") {
		t.Fatalf("真扩展名应保留：%q", got)
	}
	// 走一遍收消息：这样的文件照常下载、回「收到」，offset 前进
	e := newNotifyEnv(t, true)
	e.tg.media(1, 21, "document", "看看", sentence, []byte("x"), 0, "")
	e.flush()
	if acks := e.tg.acks(); len(acks) != 1 || e.state().UpdateOffset != 2 || len(e.chats) != 1 {
		t.Fatalf("长文件名的消息应照常处理：acks=%q offset=%d chats=%d", acks, e.state().UpdateOffset, len(e.chats))
	}
}
