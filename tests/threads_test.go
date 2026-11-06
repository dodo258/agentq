package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

const threadSession = "11111111-2222-4333-8444-555555555555"

func (f *fakeTG) replyMsg(updateID, msgID int, text string, replyToID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := fakeMsgDate().Unix()
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "message": map[string]any{
		"message_id": msgID, "date": now, "text": text, "chat": map[string]any{"id": ownerChat, "type": "private"},
		"reply_to_message": map[string]any{"message_id": replyToID, "date": now, "text": "agentq 之前发的", "chat": map[string]any{"id": ownerChat}},
	}})
}

func threadEnv(t *testing.T, cfg map[string]any) (*testEnv, *fakeTG) {
	t.Helper()
	full := map[string]any{"notify_enabled": true, "paused": true}
	for k, v := range cfg {
		full[k] = v
	}
	e := newEnv(t, full)
	tg := newFakeTG(t)
	e.extra = append(e.extra, "AGENTQ_TELEGRAM_API="+tg.srv.URL)
	sec := filepath.Join(e.home, ".agentq", "secrets")
	os.MkdirAll(sec, 0o700)
	os.WriteFile(filepath.Join(sec, "telegram.env"), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)), 0o600)
	return e, tg
}

func (e *testEnv) seedThreads(threads map[int64]notify.Thread) {
	e.t.Helper()
	for id, th := range threads {
		th.At = time.Now()
		threads[id] = th
	}
	b, err := json.Marshal(&notify.State{DecideInit: true, Threads: threads})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) readNotify() *notify.State {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "notify.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var s notify.State
	if err := json.Unmarshal(b, &s); err != nil {
		e.t.Fatal(err)
	}
	return &s
}

func (e *testEnv) seedSession(id string) {
	e.t.Helper()
	p := filepath.Join(e.home, ".claude", "projects", "-x", id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// seedEnded 在 state.json 写一个已结束的任务（失败、中断之类由调用方定）。
func (e *testEnv) seedEnded(j *Job) {
	e.t.Helper()
	end := time.Now().Add(-time.Hour)
	j.StartedAt, j.EndedAt = end.Add(-time.Hour), &end
	st := e.state()
	st.Jobs = append(st.Jobs, j)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) fakeLogOf(j *Job) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j.PID)))
	if err != nil {
		e.t.Fatalf("任务 %s 没有假 claude 记录：%v", j.ID, err)
	}
	return string(b)
}

func (e *testEnv) newJobs(skip ...string) []*Job {
	var out []*Job
next:
	for _, j := range e.state().Jobs {
		for _, s := range skip {
			if j.ID == s {
				continue next
			}
		}
		out = append(out, j)
	}
	return out
}

// tickUntilEnded：tick 把回复收进来、启动任务，等任务结束再 tick 收尾（回发回答）。
func (e *testEnv) tickUntilEnded(j func() *Job) *Job {
	e.t.Helper()
	var got *Job
	waitFor(e.t, 20*time.Second, "任务结束并收尾", func() bool {
		e.mustRun("tick")
		got = j()
		return got != nil && got.Status != state.StatusRunning
	})
	return got
}

// 回复对话回答：-r 接原会话，权限和现在的对话一样，回答再登记、挂在用户那条下面。
func TestThreadChat(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	e.seedSession(threadSession)
	e.seedThreads(map[int64]notify.Thread{900: {Kind: chat.ThreadChat, JobID: "old", SessionID: threadSession, At: time.Now()}})
	tg.replyMsg(1, 11, "刚才那个再说细一点", 900)
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	log := e.fakeLogOf(j)
	for _, want := range []string{"ARG[-r]\nARG[" + threadSession + "]", "ARG[--permission-mode]\nARG[bypassPermissions]", "PWD[" + testkit.RealPath(t, e.root) + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--session-id]") || strings.Contains(log, "ARG[--tools]") {
		t.Errorf("接原会话不该新建、也不收窄工具：\n%s", log)
	}
	sent := tg.sentLast()
	if !strings.Contains(sent.Get("reply_parameters"), `"message_id":11`) {
		t.Errorf("回答应挂在用户那条下面：%v", sent)
	}
	s := e.readNotify()
	var again notify.Thread
	for id, th := range s.Threads {
		if id != 900 {
			again = th
		}
	}
	if again.Kind != chat.ThreadChat || again.JobID != j.ID {
		t.Errorf("回答应再登记，能一路回复：%+v", s.Threads)
	}
}

// 回复正在跑的任务：不碰那个会话，进收件箱，带固定字段。
func TestThreadJobRunning(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	testkit.MkProject(t, e.root, "demo", "- [ ] 长活 SLEEP=30\n")
	e.mustRun("run", "demo", "长活")
	task := e.jobByTitle("长活 SLEEP=30")
	defer e.mustRun("stop", task.ID)
	e.seedSession(task.SessionID)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(task)})
	tg.replyMsg(1, 11, "顺手把 README 也改了", 900)
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	log := e.fakeLogOf(j)
	if !strings.Contains(log, "这条是对正在跑的任务 "+task.ID+" 的意见") {
		t.Errorf("缺固定字段：\n%s", log)
	}
	if strings.Contains(log, "ARG[-r]") || !strings.Contains(log, "ARG[--session-id]") {
		t.Errorf("不该接那个会话：\n%s", log)
	}
	if got := e.jobByTitle("长活 SLEEP=30"); got.Status != state.StatusRunning {
		t.Errorf("不该动在跑的任务：%+v", got)
	}
}

// BACKLOG 还标进行中：写模式续跑，占名额；没名额回「排队了」（只回一次），有名额再接。
func TestThreadJobInProgress(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"max_parallel": 1})
	item := "修登录页（进行中：agent/x 2026-11-04 10:00）"
	dir := testkit.MkProject(t, e.root, "demo", "- [ ] "+item+"\n")
	testkit.MkProject(t, e.root, "other", "- [ ] 占名额 SLEEP=30\n")
	e.seedSession(threadSession)
	orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Item: item, Title: "修登录页", Fingerprint: backlog.Fingerprint("- [ ] " + item),
		SessionID: threadSession, Status: state.StatusFailed}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	e.mustRun("run", "other", "占名额")
	busy := e.jobByTitle("占名额 SLEEP=30")

	tg.replyMsg(1, 11, "接着做，别改接口", 900)
	e.mustRun("tick")
	e.mustRun("tick")
	var queued int
	for _, x := range tg.texts() {
		if strings.Contains(x, "排队了") {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("没名额应只回一次「排队了」：%q", tg.texts())
	}
	if s := e.readNotify(); len(s.Inbox) != 1 || !s.Inbox[0].QueueNoted {
		t.Fatalf("应留在收件箱等名额：%+v", s.Inbox)
	}
	if len(e.state().Tasks()) != 1 {
		t.Fatal("没名额不该启动")
	}

	e.mustRun("stop", busy.ID)
	e.mustRun("tick")
	var res *Job
	for _, j := range e.state().Jobs {
		if j.ResumeOf == orig.ID {
			res = j
		}
	}
	if res == nil || res.Kind != "" || !res.FromThread {
		t.Fatalf("应写模式续跑：%+v", e.state().Jobs)
	}
	log := e.fakeLogOf(res)
	for _, want := range []string{"ARG[-r]\nARG[" + threadSession + "]", "ARG[--permission-mode]\nARG[bypassPermissions]", "接着做，别改接口", "PWD[" + testkit.RealPath(t, dir) + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--tools]") {
		t.Errorf("写模式不收窄工具：\n%s", log)
	}
	if got := e.state(); len(got.Tasks()) != 1 && res.Status == state.StatusRunning {
		t.Errorf("续跑要占名额：%+v", got.Tasks())
	}
	if s := e.readNotify(); len(s.Inbox) != 0 {
		t.Errorf("处理完不该留在收件箱：%+v", s.Inbox)
	}
	if !strings.Contains(strings.Join(tg.texts(), "\n"), "已接回任务 "+orig.ID) {
		t.Errorf("应告诉用户接回了：%q", tg.texts())
	}
}

// 已结束的任务：只读续跑。参数卡死、不占名额（满了也能跑）、不传 permission_mode；回答挂在用户那条下面，再登记。
func TestThreadJobFollowup(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"max_parallel": 1})
	dir := testkit.MkProject(t, e.root, "demo", "")
	testkit.MkProject(t, e.root, "other", "- [ ] 占名额 SLEEP=30\n")
	e.seedSession(threadSession)
	orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Item: "已合并的活", Title: "已合并的活", Fingerprint: "fp-gone", SessionID: threadSession, Status: state.StatusDone}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	e.mustRun("run", "other", "占名额")
	busy := e.jobByTitle("占名额 SLEEP=30")
	defer e.mustRun("stop", busy.ID)

	tg.replyMsg(1, 11, "这个合了没 FUPREPLY", 900)
	follow := func() *Job {
		for _, j := range e.state().Jobs {
			if j.Kind == state.KindFollowup {
				return j
			}
		}
		return nil
	}
	j := e.tickUntilEnded(follow)
	if j.ResumeOf != orig.ID || j.Fingerprint != "" {
		t.Errorf("追问记录：%+v", j)
	}
	log := e.fakeLogOf(j)
	for _, want := range []string{"ARG[-r]\nARG[" + threadSession + "]", "ARG[--tools]\nARG[Read,Grep,Glob]", "ARG[--strict-mcp-config]", "ARG[--json-schema]", "ARG[--max-budget-usd]\nARG[1]", "PWD[" + testkit.RealPath(t, dir) + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--permission-mode]") {
		t.Errorf("只读续跑不能传 permission_mode：\n%s", log)
	}
	sent := tg.sentLast()
	if sent.Get("text") != "已经合并了，看 README" || !strings.Contains(sent.Get("reply_parameters"), `"message_id":11`) {
		t.Errorf("回答：%v", sent)
	}
	found := false
	for id, th := range e.readNotify().Threads {
		if id != 900 && th.Kind == notify.ThreadJob && th.SessionID == "s-fup" {
			found = true
		}
	}
	if !found {
		t.Errorf("回答应再登记：%+v", e.readNotify().Threads)
	}
	if got := e.jobByTitle("占名额 SLEEP=30"); got.Status != state.StatusRunning {
		t.Error("不该影响占名额的任务")
	}
}

// todo 由 agentq 写进那个项目的「待做」并提交，注明来源任务；模型写的进行中标记去掉。写不进去要告诉用户。
func TestThreadFollowupTodo(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	dir := testkit.MkProject(t, e.root, "demo", "")
	os.RemoveAll(filepath.Join(dir, ".git"))
	testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "init")
	e.seedSession(threadSession)
	orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Item: "已合并的活", Title: "已合并的活", Fingerprint: "fp-gone", SessionID: threadSession, Status: state.StatusDone}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	tg.replyMsg(1, 11, "顺手修一下 FUPTODO", 900)
	j := e.tickUntilEnded(func() *Job {
		for _, j := range e.state().Jobs {
			if j.Kind == state.KindFollowup {
				return j
			}
		}
		return nil
	})
	b, err := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	items := backlog.ParseBacklog(string(b))
	if len(items) != 1 || !items[0].Eligible() || !strings.Contains(items[0].Text, "清缓存 —— 启动时清一次") ||
		!strings.Contains(items[0].Text, "来源任务 "+orig.ID) || !strings.Contains(items[0].Text, "追问任务 "+j.ID) {
		t.Fatalf("待做：%+v\n%s", items, b)
	}
	if msg := testkit.GitT(t, e.home, dir, "log", "-1", "--format=%s"); !strings.Contains(msg, "Telegram 追问") {
		t.Errorf("应提交：%s", msg)
	}
	got := tg.sentLast().Get("text")
	if !strings.Contains(got, "原因是缓存没清") || !strings.Contains(got, "已写进 demo 的「待做」") {
		t.Errorf("回答：%q", got)
	}
}

// 回复单条「要你定」：进收件箱，带项目和条目。
func TestThreadDecide(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	dir := testkit.MkProject(t, e.root, "demo", "")
	raw, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Replace(string(raw), "## 已完成", "## 等用户决定\n\n- [ ] 用哪种格式 —— A 或 B\n\n## 已完成", 1)), 0o644)
	all, _ := notify.ScanDecisions(e.root)
	if len(all) != 1 {
		t.Fatalf("拍板条目：%+v", all)
	}
	e.seedThreads(map[int64]notify.Thread{900: {Kind: notify.ThreadDecide, Project: "demo", Fingerprint: all[0].Fingerprint, At: time.Now()}})
	tg.replyMsg(1, 11, "选 A", 900)
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	if log := e.fakeLogOf(j); !strings.Contains(log, "这条是回复「要你定」：项目 demo，条目「用哪种格式」") {
		t.Errorf("缺固定字段：\n%s", log)
	}
}

// 查不到的走现有流程：带原文的新对话。
func TestThreadUnknown(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	e.seedThreads(nil)
	tg.replyMsg(1, 11, "这个呢", 12345)
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	log := e.fakeLogOf(j)
	if !strings.Contains(log, "这条是回复 agentq 之前发的这条消息") || strings.Contains(log, "ARG[-r]") || strings.Contains(log, chat.NoThreadNote) {
		t.Errorf("应走现有流程：\n%s", log)
	}
}

// 续不上：会话文件没了直接按新消息处理；进程秒退放回收件箱再按新消息处理。撞预算不算续不上。
func TestThreadFallback(t *testing.T) {
	t.Parallel()
	t.Run("会话文件没了", func(t *testing.T) {
		t.Parallel()
		e, tg := threadEnv(t, nil)
		dir := testkit.MkProject(t, e.root, "demo", "")
		orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Title: "活", SessionID: threadSession, Status: state.StatusDone}
		e.seedEnded(orig)
		e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig), 901: {Kind: chat.ThreadChat, SessionID: threadSession, At: time.Now()}})
		tg.replyMsg(1, 11, "接着说", 900)
		tg.replyMsg(2, 12, "再说", 901)
		j := e.tickUntilEnded(func() *Job {
			if c := chatJobs(e); len(c) == 1 {
				return c[0]
			}
			return nil
		})
		log := e.fakeLogOf(j)
		if strings.Contains(log, "ARG[-r]") || strings.Count(log, chat.NoThreadNote) != 2 {
			t.Errorf("都应按新消息处理并说明：\n%s", log)
		}
	})
	t.Run("进程秒退", func(t *testing.T) {
		t.Parallel()
		e, tg := threadEnv(t, nil)
		e.seedSession(threadSession)
		e.seedThreads(map[int64]notify.Thread{900: {Kind: chat.ThreadChat, SessionID: threadSession, At: time.Now()}})
		tg.replyMsg(1, 11, "接着说 FAIL", 900)
		var fresh *Job
		waitFor(t, 20*time.Second, "秒退后按新消息再起一次并收尾", func() bool {
			e.mustRun("tick")
			c := chatJobs(e)
			if len(c) == 2 && c[1].Status != state.StatusRunning {
				fresh = c[1]
				return true
			}
			return false
		})
		if log := e.fakeLogOf(fresh); strings.Contains(log, "ARG[-r]") || !strings.Contains(log, chat.NoThreadNote) {
			t.Errorf("第二次应是新会话并说明：\n%s", log)
		}
		if n := strings.Count(strings.Join(tg.texts(), "\n"), "这条没答上来"); n != 1 {
			t.Errorf("只有第二次失败才回用户：%q", tg.texts())
		}
	})
	t.Run("撞预算", func(t *testing.T) {
		t.Parallel()
		e, tg := threadEnv(t, nil)
		dir := testkit.MkProject(t, e.root, "demo", "")
		e.seedSession(threadSession)
		orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Title: "活", SessionID: threadSession, Status: state.StatusDone}
		e.seedEnded(orig)
		e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
		tg.replyMsg(1, 11, "详细查一下 FUPBUDGET", 900)
		j := e.tickUntilEnded(func() *Job {
			for _, j := range e.state().Jobs {
				if j.Kind == state.KindFollowup {
					return j
				}
			}
			return nil
		})
		if j.Status != state.StatusFailed || len(chatJobs(e)) != 0 || !strings.Contains(tg.sentLast().Get("text"), "这条没答上来") {
			t.Errorf("撞预算回没答完、不重试：%+v %q", j, tg.texts())
		}
	})
}

// 只点名一个任务的提醒登记，合并了多条的不登记；单条「要你定」登记；过期清理和 500 条上限；旧 notify.json 能读。
func TestThreadRegistration(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.st.Jobs = []*Job{{ID: "j1", Project: "p", ProjectDir: "/x", SessionID: "s1", Fingerprint: "fp1", Status: state.StatusFailed}}
	if err := state.Save(e.a.StatePath(), e.st); err != nil {
		t.Fatal(err)
	}
	e.flush()
	send := func(list ...notify.Notice) *notify.State {
		t.Helper()
		e.a.Notify.Notify(list...)
		e.now = e.now.Add(notify.NotifyMergeWindow + time.Minute)
		e.flush()
		s, err := e.a.Notify.LoadNotify()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := send(notify.Notice{Cat: "patrol", Text: "任务 j1 无改动", JobID: "j1"})
	if len(s.Threads) != 1 {
		t.Fatalf("单条提醒应登记：%+v", s.Threads)
	}
	for _, th := range s.Threads {
		if th.Kind != notify.ThreadJob || th.JobID != "j1" || th.SessionID != "s1" || th.ProjectDir != "/x" || th.Fingerprint != "fp1" {
			t.Errorf("登记内容：%+v", th)
		}
	}
	s = send(notify.Notice{Cat: "patrol", Text: "a", JobID: "j1"}, notify.Notice{Cat: "patrol", Text: "b", JobID: "j1"})
	if len(s.Threads) != 1 {
		t.Errorf("合并了多条的不登记：%+v", s.Threads)
	}
	s = send(notify.Notice{Cat: "patrol", Text: "没点名任务"})
	if len(s.Threads) != 1 {
		t.Errorf("没点名任务的不登记：%+v", s.Threads)
	}
	s = send(notify.Notice{Cat: "decide", Key: "fp9", Project: "p", Text: "p　选哪个"})
	found := false
	for _, th := range s.Threads {
		if th.Kind == notify.ThreadDecide && th.Project == "p" && th.Fingerprint == "fp9" {
			found = true
		}
	}
	if !found {
		t.Errorf("单条要你定应登记：%+v", s.Threads)
	}

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	var st notify.State
	st.AddThread(0, notify.Thread{Kind: notify.ThreadJob}, now)
	if len(st.Threads) != 0 {
		t.Error("没走 Telegram（id 0）不登记")
	}
	st.Threads = map[int64]notify.Thread{1: {At: now.Add(-notify.ThreadKeep - time.Minute)}, 2: {At: now.Add(-notify.ThreadKeep)}}
	st.PruneThreads(now)
	if _, ok := st.Threads[1]; ok || len(st.Threads) != 1 {
		t.Errorf("超过 14 天的去掉：%+v", st.Threads)
	}
	for i := range notify.ThreadMax + 5 {
		st.AddThread(int64(100+i), notify.Thread{}, now.Add(time.Duration(i)*time.Second))
	}
	if len(st.Threads) != notify.ThreadMax {
		t.Errorf("最多 %d 条：%d", notify.ThreadMax, len(st.Threads))
	}
	if _, ok := st.Threads[100]; ok {
		t.Error("应去掉最早的")
	}
	if _, ok := st.Threads[int64(100+notify.ThreadMax+4)]; !ok {
		t.Error("最新的应留下")
	}

	var old notify.State
	if err := json.Unmarshal([]byte(`{"day":"2026-11-04","count":1}`), &old); err != nil || old.Threads != nil {
		t.Errorf("旧 notify.json：%v %+v", err, old)
	}
	raw, err := json.Marshal(&notify.State{Threads: map[int64]notify.Thread{7: {Kind: chat.ThreadChat}}})
	if err != nil {
		t.Fatal(err)
	}
	var back notify.State
	if err := json.Unmarshal(raw, &back); err != nil || back.Threads[7].Kind != chat.ThreadChat {
		t.Errorf("往返：%v %+v", err, back)
	}
}

// notify.json 损坏时现有兜底不变：备份、从空的开始，收消息照常。
func TestThreadCorruptNotify(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), []byte(`{"threads":坏`), 0o600); err != nil {
		t.Fatal(err)
	}
	tg.msg(1, ownerChat, 11, "查一下", "")
	// 等任务收尾再结束用例：任务外壳还在写临时 HOME 时删目录会失败
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	baks, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "notify.json.bad-*"))
	if len(baks) != 1 {
		t.Errorf("应备份坏文件：%v", baks)
	}
	if j.Status != state.StatusDone {
		t.Errorf("收消息照常：%+v", j)
	}
}

// 回答发送失败留着重发时，发出去之后也登记。
func TestThreadPendingReplyRegisters(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	th := notify.Thread{Kind: chat.ThreadChat, JobID: "j", SessionID: "s"}
	s := &notify.State{Replies: []notify.PendingReply{{Text: "答", At: e.now, JobID: "j", Thread: &th}}}
	c, err := e.a.Telegram.TelegramCreds()
	if err != nil {
		t.Fatal(err)
	}
	e.a.Chat.RetryReplies(s, c)
	if len(s.Replies) != 0 || len(s.Threads) != 1 {
		t.Errorf("重发后应登记：%+v", s)
	}
}

// 带附件的只读续跑：只放这次的附件目录。
func TestThreadFollowupFiles(t *testing.T) {
	t.Parallel()
	e, _ := threadEnv(t, nil)
	dir := testkit.MkProject(t, e.root, "demo", "")
	e.seedSession(threadSession)
	orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Title: "活", SessionID: threadSession, Status: state.StatusDone}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	file := filepath.Join(e.home, ".agentq", "inbox", "11-shot.png")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("x"), 0o644)
	s := e.readNotify()
	s.Inbox = []notify.InMsg{{ID: 11, At: time.Now(), Text: "看这张图 FUPREPLY", ReplyToID: 900, Files: []string{file}}}
	b, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), b, 0o600)
	j := e.tickUntilEnded(func() *Job {
		for _, j := range e.state().Jobs {
			if j.Kind == state.KindFollowup {
				return j
			}
		}
		return nil
	})
	want := filepath.Join(e.home, ".agentq", "jobs", j.ID, "files")
	log := e.fakeLogOf(j)
	if !strings.Contains(log, "ARG[--add-dir]\nARG["+want+"]") || !strings.Contains(log, filepath.Join(want, "11-shot.png")) {
		t.Errorf("应放开这次的附件目录：\n%s", log)
	}
}

// 排着队的回复优先于新条目：名额空出来时 tick 不接新条目，留给它。
func TestThreadQueuedBeforeSchedule(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, map[string]any{"max_parallel": 1, "paused": false})
	item := "修登录页（进行中：agent/x 2026-11-04 10:00）"
	dir := testkit.MkProject(t, e.root, "demo", "- [ ] "+item+"\n")
	testkit.MkProject(t, e.root, "other", "- [ ] 占名额 SLEEP=30\n- [ ] 下一条 SLEEP=30\n")
	e.seedSession(threadSession)
	orig := &Job{ID: "1104-000000-demo", Project: "demo", ProjectDir: dir, Item: item, Title: "修登录页", Fingerprint: backlog.Fingerprint("- [ ] " + item),
		SessionID: threadSession, Status: state.StatusFailed}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	e.mustRun("run", "other", "占名额")
	busy := e.jobByTitle("占名额 SLEEP=30")
	tg.replyMsg(1, 11, "接着做", 900)
	e.mustRun("tick")
	if s := e.readNotify(); len(s.Inbox) != 1 || !s.Inbox[0].QueueNoted {
		t.Fatalf("应排队：%+v", s.Inbox)
	}
	e.mustRun("stop", busy.ID)
	e.mustRun("tick")
	running := e.state().Tasks()
	defer func() {
		for _, j := range running {
			e.run("stop", j.ID)
		}
	}()
	if len(running) != 1 || running[0].ResumeOf != orig.ID {
		t.Errorf("空出来的名额应给回复：%+v", running)
	}
	if s := e.readNotify(); len(s.Inbox) != 0 {
		t.Errorf("收件箱：%+v", s.Inbox)
	}
}

// 接原对话时减掉会话累计的上一次用量。
func TestThreadChatResumeOf(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	e.seedSession(threadSession)
	prev := &Job{ID: "1104-000000-chat", Kind: state.KindChat, Project: chat.ChatJobName, SessionID: threadSession, Status: state.StatusDone}
	e.seedEnded(prev)
	e.seedThreads(map[int64]notify.Thread{900: {Kind: chat.ThreadChat, JobID: prev.ID, SessionID: threadSession}})
	tg.replyMsg(1, 11, "接着说", 900)
	j := e.tickUntilEnded(func() *Job {
		for _, j := range chatJobs(e) {
			if j.ID != prev.ID {
				return j
			}
		}
		return nil
	})
	if j.ResumeOf != prev.ID {
		t.Errorf("ResumeOf：%+v", j)
	}
}
