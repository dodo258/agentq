package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// 暂停、恢复：不起模型，回复写明做了什么；只改 paused，别的配置不动（包括长轮询期间用户改的）。
func TestFastPauseResume(t *testing.T) {
	e := newNotifyEnv(t, true)
	if err := config.Save(e.a.ConfigPath(), e.a.Cfg); err != nil {
		t.Fatal(err)
	}
	// listen 手上的配置是长轮询前读的，这期间用户改了 max_parallel
	changed := e.a.Cfg
	changed.MaxParallel = 7
	if err := config.Save(e.a.ConfigPath(), changed); err != nil {
		t.Fatal(err)
	}
	e.st.Jobs = []*Job{{ID: "1011-120000-demo", Project: "demo", Status: state.StatusRunning, StartedAt: e.now}}
	e.tg.msg(1, ownerChat, 11, "暂停", "")
	e.flush()
	cfg, err := config.Load(e.a.ConfigPath())
	if err != nil || !cfg.Paused || cfg.MaxParallel != 7 {
		t.Fatalf("应只改 paused：%v %+v", err, cfg)
	}
	if !e.a.Cfg.Paused {
		t.Error("进程里的配置也要改")
	}
	got := e.tg.texts()
	if len(got) != 1 || !strings.Contains(got[0], "已暂停：不再接新活") || !strings.Contains(got[0], "在跑的 1 个任务继续跑到结束") || !strings.Contains(got[0], "发「恢复」恢复") {
		t.Fatalf("回复要写明做了什么：%q", got)
	}
	if len(e.chats) != 0 || len(e.tg.acks()) != 0 {
		t.Fatalf("不该起模型、不该回收到：%v %q", e.chats, e.tg.acks())
	}
	if rp := e.tg.sentLast().Get("reply_parameters"); !strings.Contains(rp, `"message_id":11`) {
		t.Errorf("挂在用户那条下面：%s", rp)
	}
	// 再说一次：没改，也如实说
	e.tg.msg(2, ownerChat, 12, "暂停。", "")
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "本来就暂停着，没改") {
		t.Errorf("%s", got)
	}
	// 恢复：写明下次 tick 接活；还在冷却要说
	e.st.CooldownUntil = e.now.Add(time.Hour)
	e.tg.msg(3, ownerChat, 13, "恢复", "")
	e.flush()
	got2 := e.tg.sentLast().Get("text")
	if !strings.Contains(got2, "已恢复（paused=false）") || !strings.Contains(got2, "下次 tick 起接活") || !strings.Contains(got2, "不过现在：用量冷却中") {
		t.Errorf("%s", got2)
	}
	if cfg, _ := config.Load(e.a.ConfigPath()); cfg.Paused || cfg.MaxParallel != 7 {
		t.Errorf("%+v", cfg)
	}
	e.tg.msg(4, ownerChat, 14, "恢复", "")
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "本来就没暂停，没改") {
		t.Errorf("%s", got)
	}
}

// 配置读不出：不改，如实回。不能假装暂停了。
func TestFastPauseConfigBroken(t *testing.T) {
	e := newNotifyEnv(t, true)
	if err := os.WriteFile(e.a.ConfigPath(), []byte("{坏了"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.tg.msg(1, ownerChat, 11, "暂停", "")
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "配置读不出，没改") {
		t.Errorf("%s", got)
	}
	if e.a.Cfg.Paused {
		t.Error("没写进去就不该算暂停了")
	}
}

// 额度：缓存新鲜用缓存；过期现读（本地命令，不问模型）；读不出如实说。
func TestFastUsage(t *testing.T) {
	e := newNotifyEnv(t, true)
	calls := 0
	e.a.Runner.UsageText = func() (string, error) {
		calls++
		return "Current session: 91% used · resets 3:45pm\nCurrent week (all models): 40% used", nil
	}
	e.st.Usage = &backend.UsageInfo{CheckedAt: e.now, Windows: []backend.UsageWindow{{Label: "session", Used: 12}}}
	e.tg.msg(1, ownerChat, 11, "额度", "")
	e.flush()
	got := e.tg.sentLast().Get("text")
	if p := textfmt.HTMLToPlain(got); !strings.Contains(p, "会话 ▰▱▱▱▱▱▱▱▱▱   12%") || calls != 0 {
		t.Errorf("缓存新鲜不重读：%d %s", calls, got)
	}
	e.now = e.now.Add(backend.UsageCacheFor)
	e.tg.msg(2, ownerChat, 12, "额度？", "")
	e.flush()
	got = e.tg.sentLast().Get("text")
	if !strings.Contains(got, "会话 91%") || !strings.Contains(got, "订阅额度到了后台上限") || calls != 1 {
		t.Errorf("过期现读：%d %s", calls, got)
	}
	e.a.Runner.UsageText = func() (string, error) { return "", fmt.Errorf("读不了") }
	e.tg.msg(3, ownerChat, 13, "额度", "")
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "未知（读不了）") {
		t.Errorf("%s", got)
	}
	if len(e.chats) != 0 {
		t.Errorf("不该起模型：%v", e.chats)
	}
}

// 在跑什么：占名额的在前，id 写全；没有就说没有；状态读不出如实说。
func TestFastRunning(t *testing.T) {
	e := newNotifyEnv(t, true)
	e.tg.msg(1, ownerChat, 11, "在跑什么", "")
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "现在没有任务在跑") {
		t.Errorf("%s", got)
	}
	e.st.Jobs = []*Job{
		{ID: "1011-110000-夜班", Kind: config.KindNight, Project: "夜班", Title: "agentq night", Status: state.StatusRunning, StartedAt: e.now.Add(-time.Hour)},
		{ID: "1011-113000-demo", Project: "demo", Title: "甲 <b>", Status: state.StatusRunning, StartedAt: e.now.Add(-30 * time.Minute), ResumeOf: "x"},
		{ID: "1011-100000-demo", Project: "demo", Title: "结束了", Status: state.StatusDone, StartedAt: e.now},
	}
	e.tg.msg(2, ownerChat, 12, "在跑什么？", "")
	e.flush()
	got := e.tg.sentLast().Get("text")
	if !strings.Contains(got, "在跑　2 个（占名额 1/2）") || !strings.Contains(got, "1011-113000-demo　demo「甲 &lt;b&gt;」续跑中 30 分钟") || !strings.Contains(got, "1011-110000-夜班") || strings.Contains(got, "结束了") || !strings.Contains(got, "停 &lt;任务id&gt;") {
		t.Errorf("%s", got)
	}
	if strings.Index(got, "1011-113000-demo") > strings.Index(got, "1011-110000-夜班") {
		t.Errorf("占名额的在前：%s", got)
	}
	if err := os.WriteFile(e.a.StatePath(), []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.a.Chat.RunningDigest(); !strings.Contains(got, "状态文件读不出") {
		t.Errorf("%s", got)
	}
}

// 回复别的消息、带附件的，哪怕文字一样也交给模型：可能在说那件事。
func TestFastNotForRepliesAndAttachments(t *testing.T) {
	for _, text := range []string{"暂停", "恢复", "额度", "在跑什么", "停 1104-120000-demo"} {
		t.Run(text, func(t *testing.T) {
			e := newNotifyEnv(t, true)
			if err := config.Save(e.a.ConfigPath(), e.a.Cfg); err != nil {
				t.Fatal(err)
			}
			e.tg.msg(1, ownerChat, 11, text, "agentq 之前发的")
			e.tg.media(2, 12, "photo", text, "", []byte("x"), 0, "")
			e.flush()
			if len(e.chats) != 1 || !strings.Contains(e.chats[0], text) {
				t.Fatalf("应交给一次 claude：%v", e.chats)
			}
			if acks := e.tg.acks(); len(acks) != 2 {
				t.Errorf("都回「收到」：%q", acks)
			}
			if e.a.Cfg.Paused {
				t.Error("不该暂停")
			}
		})
	}
}

// 别人发的快捷命令不理；第一次收消息跳过旧的「暂停」。
func TestFastOnlyOwnerAndFresh(t *testing.T) {
	e := newNotifyEnv(t, true)
	if err := config.Save(e.a.ConfigPath(), e.a.Cfg); err != nil {
		t.Fatal(err)
	}
	e.a.Now = time.Now
	e.tg.msg(1, 4242, 11, "暂停", "")
	e.tg.msgAt(2, ownerChat, 12, "暂停", "", time.Now().Add(-time.Hour))
	if err := state.Save(e.a.StatePath(), e.st); err != nil {
		t.Fatal(err)
	}
	e.a.Sched.NotifyTick(e.st)
	if cfg, _ := config.Load(e.a.ConfigPath()); cfg.Paused || e.tg.count() != 0 {
		t.Errorf("不该暂停、不该回：%+v %q", cfg, e.tg.texts())
	}
}

// 集成：真二进制、常驻 listen、假 Telegram。暂停、恢复、额度、在跑什么、停任务都不起模型（假 claude 只被 /usage 和跑着的任务调用），
// 回得快；别的话照常交给模型。
func TestIntegrationListenFastPath(t *testing.T) {
	t.Parallel()
	{
		{
			e, tg := threadEnv(t, map[string]any{"paused": false, "tick_after_job": false})
			testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— SLEEP=60\n")
			os.WriteFile(filepath.Join(e.home, "fake-usage.json"), []byte(`{"type":"result","is_error":false,"result":"Current session: 33% used"}`), 0o644)
			e.mustRun("tick")
			job := e.jobByTitle("甲")
			os.Remove(filepath.Join(e.home, "fake-claude-calls"))

			var logBuf syncBuffer
			cmd := exec.Command(agentqBin, "listen")
			var envs []string
			for _, kv := range os.Environ() {
				if name, _, _ := strings.Cut(kv, "="); name != "HOME" {
					envs = append(envs, kv)
				}
			}
			cmd.Env = append(append(envs, "HOME="+e.home, "AGENTQ_FAKE_LOG="+e.fakeLg, config.TestTuningEnv+"="+e.tuning), e.extra...)
			cmd.Stdout, cmd.Stderr = &logBuf, &logBuf
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cmd.Process.Signal(syscall.SIGTERM)
				cmd.Wait()
			}()
			waitFor(t, 10*time.Second, "listen 挂起长轮询", func() bool {
				tg.mu.Lock()
				defer tg.mu.Unlock()
				return tg.polling > 0
			})
			ask := func(update int, text, want string) string {
				t.Helper()
				before := tg.count()
				sent := time.Now()
				tg.msg(update, ownerChat, 100+update, text, "")
				waitFor(t, 15*time.Second, "回「"+text+"」", func() bool { return tg.count() > before })
				if d := time.Since(sent); d > 13*time.Second {
					t.Errorf("「%s」%s 才回", text, d)
				}
				got := tg.sentLast().Get("text")
				if !strings.Contains(got, want) {
					t.Errorf("「%s」的回答缺 %q：%s", text, want, got)
				}
				return got
			}
			ask(1, "暂停", "已暂停")
			if cfg, _ := config.Load(filepath.Join(e.home, ".agentq", "config.json")); !cfg.Paused {
				t.Error("应已暂停")
			}
			ask(2, "在跑什么", job.ID)
			ask(3, "额度", "33%")
			ask(4, "恢复", "已恢复")
			if cfg, _ := config.Load(filepath.Join(e.home, ".agentq", "config.json")); cfg.Paused {
				t.Error("应已恢复")
			}
			ask(5, "停 "+job.ID, "已停止任务 "+job.ID)
			if j := e.jobByTitle("甲"); j.Status != state.StatusKilled {
				t.Errorf("应是用户停的：%+v", j)
			}
			if syscall.Kill(job.PID, 0) == nil {
				waitExit(t, job.PID, 5*time.Second)
			}
			ask(6, "停 "+job.ID, "没停：任务 "+job.ID+" 已经已停止")
			ask(7, "停 1234-567890-nope", "没停：没有任务")
			calls, _ := os.ReadFile(filepath.Join(e.home, "fake-claude-calls"))
			if s := strings.TrimSpace(string(calls)); s != "" { // 额度缓存还新鲜（tick 刚读过），连 /usage 都不用起
				t.Errorf("不该起进程：%q", s)
			}
			if out := logBuf.String(); strings.Contains(out, "启动对话任务") {
				t.Errorf("不该起模型：%s", out)
			}
			if n := len(chatJobs(e)); n != 0 {
				t.Errorf("不该有对话任务：%d", n)
			}
			for _, s := range tg.texts() {
				if strings.HasPrefix(s, "收到，") {
					t.Errorf("快捷命令不回收到：%q", s)
				}
			}
			// 别的话照常交给模型，能力不减
			tg.msg(8, ownerChat, 108, "暂停一下行不行", "")
			waitFor(t, 20*time.Second, "交给模型", func() bool { return len(chatJobs(e)) == 1 })
			if acks := tg.acks(); len(acks) != 1 {
				t.Errorf("要回收到：%q", acks)
			}
			for _, j := range chatJobs(e) {
				waitExit(t, j.PID, 15*time.Second)
			}
		}
	}
}

// 「停 <id>」：对话任务在跑时也不等；额度冷却时也能停；和 agentq stop 一样算用户停的（孤儿逻辑不接手）。
func TestFastStopBypassesQueue(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"paused": false})
	testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— SLEEP=60\n")
	e.mustRun("tick")
	job := e.jobByTitle("甲")
	st := e.state()
	st.CooldownUntil = time.Now().Add(time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	tg.msg(1, ownerChat, 11, "停 "+job.ID, "")
	tg.msg(2, ownerChat, 12, "这条在冷却，要回原因", "")
	e.mustRun("tick")
	if j := e.jobByTitle("甲"); j.Status != state.StatusKilled {
		t.Fatalf("应停了：%+v", j)
	}
	got := strings.Join(tg.texts(), "\n")
	if !strings.Contains(got, "已停止任务 "+job.ID) || !strings.Contains(got, "这条现在没法交给 claude") {
		t.Errorf("%s", got)
	}
	if strings.Contains(got, "收到，") {
		t.Errorf("冷却时不说收到：%s", got)
	}
	if s := e.readNotify(); len(s.Inbox) != 0 {
		t.Errorf("收件箱应清空：%+v", s.Inbox)
	}
}

// 状态读不了：不进收件箱，如实回。
func TestFastStopStateUnreadable(t *testing.T) {
	e := newNotifyEnv(t, true)
	if err := os.WriteFile(e.a.StatePath(), []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := e.a.Telegram.TelegramCreds()
	if err != nil {
		t.Fatal(err)
	}
	e.tg.msg(1, ownerChat, 11, "停 1104-120000-demo", "")
	ups, err := e.a.Telegram.TgUpdates(c, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.a.Chat.TakeUpdates(c, false, ups)
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, "状态文件读不出，没法停任务") {
		t.Errorf("%s", got)
	}
	if s := e.state(); len(s.Inbox) != 0 {
		t.Errorf("不该排队：%+v", s.Inbox)
	}
}

// 老的 notify.json 没有 stop_job：照常读；排队的「停」写进文件后下一次还在（listen 这次拿不到 tick.lock 也不丢）。
func TestFastStopPersisted(t *testing.T) {
	var s notify.State
	if err := json.Unmarshal([]byte(`{"inbox":[{"id":1,"text":"你好"}]}`), &s); err != nil || s.Inbox[0].StopJob != "" {
		t.Fatalf("%v %+v", err, s)
	}
	e := newNotifyEnv(t, true)
	lock, err := fsx.AcquireLock(e.a.LockPath(), 0)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	defer lock.Close()
	c, _ := e.a.Telegram.TelegramCreds()
	if err := state.Save(e.a.StatePath(), e.st); err != nil {
		t.Fatal(err)
	}
	e.tg.msg(1, ownerChat, 11, "停 1104-120000-demo", "")
	ups, err := e.a.Telegram.TgUpdates(c, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.a.Chat.TakeUpdates(c, false, ups)
	if got := e.state(); len(got.Inbox) != 1 || got.Inbox[0].StopJob != "1104-120000-demo" {
		t.Errorf("%+v", got.Inbox)
	}
	if e.tg.count() != 0 {
		t.Errorf("执行完才回：%q", e.tg.texts())
	}
}
