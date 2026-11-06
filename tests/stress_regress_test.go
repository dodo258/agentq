package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"agentq/internal/backend/claude"
	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// 暴力测试（P9）找到的 bug 的回归用例：不需要 AGENTQ_STRESS，平时就跑。

// tick 起了 claude、还没写状态就被杀：下一次 tick 认领它，不能再起一个同样的任务。
func TestRegressKilledTickNoDuplicate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 启动窗口 —— KILLTICK SLEEP=5\n")
	e.run("tick") // 被假 claude 杀掉
	var claudePid int
	waitFor(t, 5*time.Second, "claude.pid", func() bool {
		metas, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "jobs", "*", "claude.pid"))
		if len(metas) != 1 {
			return false
		}
		b, _ := os.ReadFile(metas[0])
		fmt.Sscan(string(b), &claudePid)
		return claudePid > 0
	})
	e.mustRun("tick")
	entries, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "jobs"))
	if len(entries) != 1 {
		t.Fatalf("同一条目被起了 %d 次：%v", len(entries), entries)
	}
	j := e.jobByTitle("启动窗口")
	if j.Status != state.StatusRunning || j.PID != claudePid {
		t.Fatalf("应认领正在跑的 claude（pid %d）：%+v", claudePid, j)
	}
	waitExit(t, claudePid, 15*time.Second)
	e.mustRun("tick")
	if got := e.jobByTitle("启动窗口"); got.Status != state.StatusDone {
		t.Errorf("认领后应按结果正常收尾：%+v", got)
	}
}

// 记录写了、claude 没起来（tick 在起外壳之前被杀）：时限内等着，过了时限记启动中断，不永远占名额。
func TestRegressStartInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := testkit.MkProject(t, e.root, "alpha", "- [ ] 没起来 —— x\n")
	id := "1104-000000-alpha"
	if err := os.MkdirAll(filepath.Join(e.home, ".agentq", "jobs", id), 0o755); err != nil {
		t.Fatal(err)
	}
	it := backlog.ParseBacklog("## 待做\n- [ ] 没起来 —— x\n")[0]
	// tick 写完 meta.json 和「正在启动」记录就被杀了：state.json 里还没有这个任务
	job := Job{ID: id, Project: "alpha", ProjectDir: dir, Item: it.Text, Title: it.Title, Fingerprint: it.Fingerprint, Status: state.StatusRunning, StartedAt: time.Now().Add(-time.Second)}
	b, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "jobs", id, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".agentq", "starting"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "starting", id), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if j := e.jobByTitle("没起来"); j.Status != state.StatusRunning {
		t.Fatalf("时限内要等外壳：%+v", j)
	}
	if n, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "jobs")); len(n) != 1 {
		t.Fatalf("时限内不该再起一个：%v", n)
	}
	// 接回来的这次 tick 也可能被杀：存进 state.json 之前记录不能清
	if n, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "starting")); len(n) != 1 {
		t.Fatalf("这次 tick 接回来的，记录要到下一次才清：%v", n)
	}
	e.mustRun("tick")
	if n, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "starting")); len(n) != 0 {
		t.Fatalf("已经存进 state.json，记录应该清掉：%v", n)
	}
	st := e.state()
	st.Jobs[0].StartedAt = time.Now().Add(-runner.ShellReportWait - time.Second)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if j := e.jobByTitle("没起来"); j.Status != state.StatusFailed || !strings.Contains(j.Summary, "启动中断") {
		t.Fatalf("过了时限应记启动中断：%+v", j)
	}
}

// claude 退出后留下不理 SIGTERM 的子进程：外壳要 SIGKILL 清掉，否则没有人再管它们。
func TestRegressShellKillsTermIgnoringChild(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 留硬骨头 —— TERMCHILD\n")
	e.mustRun("tick")
	j := e.jobByTitle("留硬骨头")
	waitExit(t, j.PID, 10*time.Second)
	if !proc.WaitGroupGone(j.PID, proc.LeftoverGrace+5*time.Second) {
		syscall.Kill(-j.PID, syscall.SIGKILL)
		t.Fatal("不理 SIGTERM 的残留进程没被清掉")
	}
	waitFor(t, 5*time.Second, "launchd.log 记下 SIGKILL", func() bool {
		return strings.Contains(e.launchdLog(), "已发 SIGTERM 后 SIGKILL")
	})
}

// 名字里有 ps 会转义的字符（制表符、换行、表情）：活着的任务不能被当成结束了，也不能再起一个。
func TestRegressWeirdProjectNameStaysAlive(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"tab\there", "new\nline", "🙂emoji", "has space", `quo"te`, "$HOME", "-dash"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			testkit.MkProject(t, e.root, name, "- [ ] 睡一会 —— SLEEP=6\n")
			e.mustRun("tick")
			j := e.jobByTitle("睡一会")
			e.mustRun("tick")
			got := e.jobByTitle("睡一会")
			if got.Status != state.StatusRunning {
				t.Fatalf("进程还活着就被当成结束了：%+v", got)
			}
			if strings.ContainsAny(got.ID, " \t\n") {
				t.Errorf("任务 id 要能直接复制：%q", got.ID)
			}
			waitExit(t, j.PID, 15*time.Second)
			e.mustRun("tick")
			if got := e.jobByTitle("睡一会"); got.Status != state.StatusDone {
				t.Errorf("应正常收尾：%+v", got)
			}
		})
	}
}

// state.json 被写成 null、{}：不是状态文件，要按损坏处理（备份、按 meta.json 重建），不能当成空状态把运行中的任务忘了。
func TestRegressStateNullIsCorrupt(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"null", "{}", `{"cooldown_until":"2026-01-01T00:00:00Z"}`} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			testkit.MkProject(t, e.root, "alpha", "- [ ] 睡一会 —— SLEEP=4\n- [ ] 第二条 —— x\n")
			e.mustRun("tick")
			j := e.jobByTitle("睡一会")
			path := filepath.Join(e.home, ".agentq", "state.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			out := e.mustRun("tick")
			if !strings.Contains(out, "状态文件损坏") {
				t.Fatalf("应按损坏处理：%s", out)
			}
			if got := e.jobByTitle("睡一会"); got.Status != state.StatusRunning || got.PID != j.PID {
				t.Fatalf("运行中的任务不能忘：%+v", got)
			}
			if n, _ := filepath.Glob(path + ".bad-*"); len(n) != 1 {
				t.Errorf("坏文件要备份：%v", n)
			}
			waitExit(t, j.PID, 15*time.Second)
		})
	}
}

// 时钟跳到未来又跳回来：熔断、冷却、未来的结束时间不能把调度卡到那一天，超时要生效。
func TestRegressClockJumpBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "1s"})
	dir := testkit.MkProject(t, e.root, "alpha", "- [ ] 新条目 —— x\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 睡很久 —— SLEEP=30\n")
	future := time.Now().AddDate(10, 0, 0)
	e.extra = []string{"AGENTQ_TEST_NOW=" + future.Format(time.RFC3339)}
	e.mustRun("run", "beta") // 启动时间记在十年后
	e.extra = nil
	st := e.state()
	ended := future
	it := backlog.ParseBacklog("## 待做\n- [ ] 新条目 —— x\n")[0]
	st.Jobs = append(st.Jobs, &Job{ID: "old", Project: "alpha", ProjectDir: dir, Item: it.Text, Title: it.Title, Fingerprint: it.Fingerprint, Status: state.StatusFailed, StartedAt: future, EndedAt: &ended})
	st.BreakerUntil, st.CooldownUntil = future, future
	st.ItemHold = map[string]time.Time{"x": future}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	beta := e.jobByTitle("睡很久")
	out := e.mustRun("tick")
	if !strings.Contains(out, "时钟跳变") {
		t.Errorf("应记日志：%s", out)
	}
	time.Sleep(1200 * time.Millisecond)
	e.mustRun("tick")
	if got := e.jobByTitle("睡很久"); got.Status != state.StatusTimeout {
		t.Errorf("跳回来之后超时要生效：%+v", got)
	}
	waitExit(t, beta.PID, 15*time.Second)
	st = e.state()
	if st.BreakerUntil.After(time.Now().Add(5*time.Hour)) || st.CooldownUntil.After(time.Now().Add(9*24*time.Hour)) || st.ItemHold["x"].After(time.Now().Add(13*time.Hour)) {
		t.Errorf("未来的时间要改回来：%+v %+v %+v", st.BreakerUntil, st.CooldownUntil, st.ItemHold)
	}
	for _, j := range st.Jobs {
		if j.ID == "old" && j.EndedAt.After(time.Now().Add(time.Hour)) {
			t.Errorf("未来的结束时间要改回来：%v", j.EndedAt)
		}
	}
}

// BACKLOG 是软链接：改它写到指向的文件、提交指向的文件，链接还是链接，权限不变。
func TestRegressBacklogSymlink(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "alpha", "- [ ] 旧标记 —— x（进行中：2020-01-01 10:00）\n")
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	real := filepath.Join(dir, "docs", "real.md")
	if err := os.Rename(p, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	a := wireApp(&App{Env: &env.Env{Home: e.home, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "symlink"}} {
		if out, err := a.Gitx.Git(dir, args...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	pr := backlog.Project{Name: "alpha", Dir: dir}
	if err := a.Backlog.CommitBacklog(pr, "改", func(old string) (string, error) {
		return strings.ReplaceAll(old, "（进行中：2020-01-01 10:00）", ""), nil
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("软链接被换掉了：%v %v", fi, err)
	}
	b, _ := os.ReadFile(real)
	if strings.Contains(string(b), "进行中") {
		t.Errorf("内容没写到指向的文件：%s", b)
	}
	if out, err := a.Gitx.Git(dir, "status", "--porcelain"); err != nil || out != "" {
		t.Errorf("应该提交干净：%q %v", out, err)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o644 {
		t.Errorf("权限变了：%o", fi.Mode().Perm())
	}
	// 指向仓库外：不改、不换链接
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("## 待做\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	if err := a.Backlog.CommitBacklog(pr, "改", func(old string) (string, error) { return old + "x", nil }); err == nil {
		t.Error("指向仓库外的应报错")
	}
	if b, _ := os.ReadFile(outside); string(b) != "## 待做\n" {
		t.Errorf("不该改：%s", b)
	}
}

// 占锁的进程在建目录和写 pid 之间被杀：没有 pid 的旧锁不能永远占着。
func TestRegressMainLockWithoutPid(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &bytes.Buffer{}}})
	dir := filepath.Join(home, ".claude", "state", "locks", "x-main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * backlog.MainLockNoPidStale)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := a.Backlog.LockMain("x-main", 2*time.Second)
	if err != nil {
		t.Fatalf("旧锁应该被清掉：%v", err)
	}
	unlock()
	// 刚建的没有 pid 的不算旧锁（对方可能正要写 pid）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Backlog.LockMain("x-main", 300*time.Millisecond); err == nil {
		t.Fatal("刚建的锁不能抢")
	}
}

// 磁盘满（notify.json 存不下）：listen 不能把同一条消息回一遍又一遍。用真的小磁盘太重，这里把 ~/.agentq 换成存不下的样子：
// notify.json 的临时文件建不出来（目录只读，newApp 只在启动时改回权限）。
func TestRegressCannotSaveNoReplyLoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := newFakeTG(t)
	a := wireApp(&App{Env: &env.Env{Home: dir, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}, Telegram: &telegram.Service{TelegramAPI: f.srv.URL}})
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &telegram.TgCreds{Token: fakeToken, ChatID: 42}
	ups := []telegram.TgUpdate{{UpdateID: 7, Message: &telegram.TgMsg{MessageID: 9, Date: time.Now().Unix(), Text: "状态", Chat: struct {
		ID int64 `json:"id"`
	}{ID: 42}}}}
	if err := os.Chmod(a.DataDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(a.DataDir(), 0o700)
	for i := 0; i < 3; i++ {
		if a.Chat.TakeUpdatesSafe(c, false, ups) {
			t.Fatal("存不下时应返回 false，让 listen 歇一会儿")
		}
	}
	if got := f.texts(); len(got) != 0 {
		t.Fatalf("存不下时不该回消息（否则每收一次回一次）：%v", got)
	}
	os.Chmod(a.DataDir(), 0o700)
	if !a.Chat.TakeUpdatesSafe(c, false, ups) {
		t.Fatal("存得下时应处理")
	}
	if got := f.texts(); len(got) != 1 {
		t.Fatalf("恢复后回一次：%v", got)
	}
}

// claude auth status 挂住、子进程占着输出管道：tick 拿着锁在等它，要能按时返回，子进程也要清掉。
func TestRegressLoginHangHoldingPipe(t *testing.T) {
	old := claude.LoginWait
	claude.LoginWait = 500 * time.Millisecond
	defer func() { claude.LoginWait = old }()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ok, err := claude.LoggedIn(context.Background(), script)
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("挂了 %s", d)
	}
	if ok || err == nil {
		t.Errorf("挂住应报错：%v %v", ok, err)
	}
	b, _ := os.ReadFile(pidFile)
	var pid int
	fmt.Sscan(string(b), &pid)
	if pid <= 0 {
		t.Fatalf("子进程 pid：%q", b)
	}
	waitExit(t, pid, 5*time.Second)
}

// 超长的一句话进推送队列：要截断发出去、全文存文件，不能一直发不出去把同类的消息全堵住。
func TestRegressLongNoticeNotStuck(t *testing.T) {
	t.Parallel()
	f := newFakeTG(t)
	home := t.TempDir()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	a := wireApp(&App{Env: &env.Env{Home: home, Now: func() time.Time { return now }, Out: &bytes.Buffer{}, Cfg: config.Default()}, Telegram: &telegram.Service{TelegramAPI: f.srv.URL}})
	c := &telegram.TgCreds{Token: fakeToken, ChatID: 42}
	s := &notify.State{}
	long := strings.Repeat("很长的<一句>话", 2000)
	s.Add(now, notify.Notice{Cat: "done", Text: long}, notify.Notice{Cat: "done", Text: "后面的短消息"})
	s.Add(now, notify.Notice{Cat: "patrol", Text: "短标题", Lines: []string{strings.Repeat("补充", 5000)}})
	a.Notify.FlushNotify(s, nil, c, nil)
	if len(s.Pending) != 0 {
		t.Fatalf("消息应该都发出去：%d 条还在", len(s.Pending))
	}
	texts := f.texts()
	if len(texts) != 2 {
		t.Fatalf("应发 2 条：%d", len(texts))
	}
	for _, m := range texts {
		if n := len([]rune(textfmt.HTMLToPlain(m))); n > 4096 {
			t.Errorf("超过 4096 字：%d", n)
		}
		if !strings.Contains(m, "agentq") {
			t.Errorf("页脚被截掉了")
		}
		if !strings.Contains(m, "见 ~/.agentq/notify/") {
			t.Errorf("要写全文路径：%s", textfmt.TruncRunes(m, 100))
		}
	}
	files, _ := filepath.Glob(filepath.Join(home, ".agentq", "notify", "*"))
	if len(files) == 0 {
		t.Fatal("全文要存文件")
	}
	// 排版也不能把 HTML 转义切开
	if strings.Contains(texts[0], "&l…") || strings.Contains(texts[0], "&…") {
		t.Errorf("转义被切开：%s", textfmt.TruncRunes(texts[0], 200))
	}
}

// Telegram 报太长（别的路径漏过来的）：截断成纯文本再发一次。
func TestRegressTooLongRetriesPlain(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if len([]rune(r.Form.Get("text"))) > 4096 {
			w.WriteHeader(400)
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer srv.Close()
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}}, Telegram: &telegram.Service{TelegramAPI: srv.URL}})
	id, err := a.Telegram.TgSend(&telegram.TgCreds{Token: "123:token", ChatID: 1}, strings.Repeat("长", 5000), 0)
	if err != nil || id != 9 {
		t.Fatalf("%d %v", id, err)
	}
}

// 两个项目里一样文字的条目（比如「更新 README」）：是两件事。一个在跑、一个刚失败，都不能挡着另一个。
func TestRegressSameTextDifferentProjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 更新 README —— SLEEP=3\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 更新 README —— SLEEP=3\n")
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 2") {
		t.Fatalf("两个项目的条目都该启动：%s", out)
	}
	var pids []int
	for _, j := range e.state().Jobs {
		pids = append(pids, j.PID)
	}
	for _, pid := range pids {
		waitExit(t, pid, 15*time.Second)
	}
	e.mustRun("tick")
	// 另一个项目里新放一条同样文字的：alpha 刚结束，不算它试过
	testkit.MkProject(t, e.root, "gamma", "- [ ] 更新 README —— SLEEP=3\n")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("别的项目做过同样文字的条目，不该挡着这个：%s", out)
	}
	for _, j := range e.state().Running() {
		waitExit(t, j.PID, 15*time.Second)
	}
}

// notify.json、patrol.json 坏了：备份一次就够，不能每读一次（status、listen 每轮）备份一份。
func TestRegressCorruptBackupOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Now()
	a := wireApp(&App{Env: &env.Env{Home: home, Now: func() time.Time { return now }, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a.Notify.NotifyPath(), a.Patrol.PatrolPath()} {
		if err := os.WriteFile(path, []byte("{坏了"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := a.Notify.LoadNotify(); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Patrol.LoadPatrol(); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
	}
	for _, path := range []string{a.Notify.NotifyPath(), a.Patrol.PatrolPath()} {
		if n, _ := filepath.Glob(path + ".bad-*"); len(n) != 1 {
			t.Errorf("%s 应只备份一次：%v", path, n)
		}
	}
}

// Telegram 挂住（连上了不回）：第一条超时之后这一轮不再一条条等超时，否则拿着锁好几分钟，notify send 拿不到锁，消息就丢了。
func TestRegressTelegramHangFailsFast(t *testing.T) {
	old := telegram.TgCallWait
	telegram.TgCallWait = 300 * time.Millisecond
	defer func() { telegram.TgCallWait = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}, Telegram: &telegram.Service{TelegramAPI: srv.URL}})
	c := &telegram.TgCreds{Token: "123:token", ChatID: 1}
	s := &notify.State{}
	for _, cat := range notify.NotifyOrder {
		s.Add(time.Now(), notify.Notice{Cat: cat, Text: "攒着的 " + cat})
	}
	start := time.Now()
	a.Notify.FlushNotify(s, nil, c, nil)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("七类消息一条条等超时，用了 %s", d)
	}
	if len(s.Pending) != len(notify.NotifyOrder) {
		t.Fatalf("发不出去的消息要留着：%d", len(s.Pending))
	}
	// 长轮询不受影响，通了就解除
	a.Telegram.TgDownAt = time.Now().Add(-telegram.TgDownFor)
	if a.Telegram.TgIsDown() {
		t.Error("过了时限应解除")
	}
}

// claude auth status 自己退出了，但留下子进程占着输出：等一会儿就返回，子进程也要清掉，否则每次 tick 留一个。
func TestRegressLoginLeavesChild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\necho '{\"loggedIn\": true}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ok, err := claude.LoggedIn(context.Background(), script)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("挂了 %s", d)
	}
	if !ok {
		t.Errorf("输出是登录了：%v %v", ok, err)
	}
	b, _ := os.ReadFile(pidFile)
	var pid int
	fmt.Sscan(string(b), &pid)
	if pid <= 0 {
		t.Fatalf("子进程 pid：%q", b)
	}
	waitExit(t, pid, 3*time.Second)
}

// 接回任务的 tick 还没存状态就又被杀：记录还在，下一个 tick 还能接回来（暴力测试 killstart 连杀两次撞到）。
func TestRegressAdoptSurvivesSecondKill(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	id := "1104-000000-alpha"
	j := &Job{ID: id, Project: "alpha", Status: state.StatusRunning, StartedAt: time.Now()}
	if err := os.MkdirAll(a.JobDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Ledger.WriteMeta(j)
	if err := a.Runner.MarkStarting(id); err != nil {
		t.Fatal(err)
	}
	// 第一个 tick 接回来，还没存就没了：内存里的 st 丢掉
	a.Runner.AdoptStarting(&State{})
	st := &State{}
	a.Runner.AdoptStarting(st)
	if len(st.Jobs) != 1 || st.Jobs[0].ID != id {
		t.Fatalf("第二个 tick 要还能接回来：%+v", st.Jobs)
	}
	// 存进去了：下一次清记录，不重复接
	a.Runner.AdoptStarting(st)
	if len(st.Jobs) != 1 {
		t.Fatalf("重复接了：%d", len(st.Jobs))
	}
	if n, _ := os.ReadDir(a.Runner.StartingDir()); len(n) != 0 {
		t.Fatalf("记录应该清掉：%v", n)
	}
}

// agentq pause、resume 只改 paused 这一个键：不认识的键、别的值、顺序、排版、手改的内容都不动。
func TestRegressPauseKeepsConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	path := filepath.Join(e.home, ".agentq", "config.json")
	old, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(old, &cfg); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{
  "my_future_key": {"paused": "里面的不算", "x": [1, 2]},
  "projects_root": %q,
  "claude_path": %q,
  "note": "paused: true 也不算",
  "tick_after_job":   false,
  "patrol_enabled": false,
  "notify_enabled": false
}
`, cfg["projects_root"], cfg["claude_path"])
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	e.mustRun("pause")
	got, _ := os.ReadFile(path)
	if want := strings.Replace(body, "{\n", "{\n  \"paused\": true,\n", 1); string(got) != want {
		t.Fatalf("只该加 paused 这一个键：\n%s\n想要：\n%s", got, want)
	}
	e.mustRun("resume")
	got, _ = os.ReadFile(path)
	if want := strings.Replace(body, "{\n", "{\n  \"paused\": false,\n", 1); string(got) != want {
		t.Fatalf("resume 也只改这一个值：\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("权限变了：%o", fi.Mode().Perm())
	}
}

// BACKLOG 有没提交的改动：巡逻说清哪个项目、多久了、怎么处理；短的不报；别的项目照常被 tick 做。
func TestRegressDirtyBacklogReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dirty := orphanRepo(t, e, "alpha", "- [ ] 甲 —— x（进行中：2020-01-01 10:00）\n")
	clean := orphanRepo(t, e, "beta", "- [ ] 乙 —— x\n")
	_ = clean
	p := filepath.Join(dirty, "docs", "BACKLOG.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("没提交的一行\n")
	f.Close()
	a := wireApp(&App{Env: &env.Env{Home: e.home, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	a.Cfg.ProjectsRoot = e.root
	fail := func(prefix, note string) { t.Errorf("检查失败：%s %s", prefix, note) }
	if got := a.Patrol.CheckDirtyBacklogs(time.Now(), fail); len(got) != 0 {
		t.Fatalf("刚改的不报：%+v", got)
	}
	got := a.Patrol.CheckDirtyBacklogs(time.Now().Add(3*time.Hour), fail)
	if len(got) != 1 || got[0].Fingerprint != "backlog-dirty:alpha" {
		t.Fatalf("%+v", got)
	}
	for _, want := range []string{"alpha", "3 小时"} {
		if !strings.Contains(got[0].Title, want) {
			t.Errorf("标题缺 %q：%s", want, got[0].Title)
		}
	}
	for _, want := range []string{"不自动提交", "git diff", "git checkout", "别的项目不受影响"} {
		if !strings.Contains(got[0].Action, want) {
			t.Errorf("处理办法缺 %q：%s", want, got[0].Action)
		}
	}
	// tick 不被它卡住：别的项目当次就启动
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动任务") || !strings.Contains(out, "beta") {
		t.Errorf("别的项目应照常启动：%s", out)
	}
}

// Router 动作至多一次：收尾走第二遍（上次存状态前被杀）时，做完的不重做，开始了没做完的不重做且说明状态不确定。
func TestRegressRouterActionsAtMostOnce(t *testing.T) {
	t.Parallel()
	f := newFakeTG(t)
	home := t.TempDir()
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}, Telegram: &telegram.Service{TelegramAPI: f.srv.URL}})
	if err := os.MkdirAll(filepath.Join(home, ".agentq", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Telegram.TelegramEnvPath(), []byte("TELEGRAM_BOT_TOKEN="+fakeToken+"\nTELEGRAM_CHAT_ID=42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "1104-000000-router"
	if err := os.MkdirAll(a.JobDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	result := `{"type":"result","is_error":false,"result":"","structured_output":{"reply":"好的","actions":[{"type":"inbox","text":"第一条 —— x"},{"type":"inbox","text":"第二条 —— x"},{"type":"inbox","text":"第三条 —— x"}]}}`
	if err := os.WriteFile(filepath.Join(a.JobDir(id), "result.json"), []byte(result), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第一条做完了，第二条开始了没记上做完：进程在这时被杀
	done := a.Chat.LoadRouterDone(id)
	if err := a.Chat.MarkRouterAct(id, done, 0, chat.RouterActDone); err != nil {
		t.Fatal(err)
	}
	if err := a.Chat.MarkRouterAct(id, done, 1, chat.RouterActStarted); err != nil {
		t.Fatal(err)
	}
	j := &Job{ID: id, Kind: state.KindRouter, Project: chat.ChatJobName, Status: state.StatusDone}
	a.Chat.FinishRouter(&State{}, j)
	inbox, _ := os.ReadFile(a.Inbox.InboxPath())
	if strings.Contains(string(inbox), "第一条") || strings.Contains(string(inbox), "第二条") {
		t.Errorf("有记录的动作不该再做：%s", inbox)
	}
	if !strings.Contains(string(inbox), "第三条") {
		t.Errorf("没有记录的动作要做：%s", inbox)
	}
	texts := f.texts()
	if len(texts) != 1 || !strings.Contains(texts[0], "不确定做没做成") {
		t.Errorf("回复要说明状态不确定：%v", texts)
	}
	// 再走一遍：第三条也不重做
	before, _ := os.ReadFile(a.Inbox.InboxPath())
	a.Chat.FinishRouter(&State{}, j)
	if after, _ := os.ReadFile(a.Inbox.InboxPath()); string(after) != string(before) {
		t.Errorf("重做了：%s", after)
	}
	// 记录写坏：全都不确定，一个都不做
	if err := os.WriteFile(a.Chat.RouterDonePath(id), []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Chat.FinishRouter(&State{}, j)
	if after, _ := os.ReadFile(a.Inbox.InboxPath()); string(after) != string(before) {
		t.Errorf("记录坏了不该做：%s", after)
	}
}

func gateTestRepo(t *testing.T) (dir, wt string, a *App) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	home := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a = wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()}})
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := a.Gitx.Git(dir, args...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	wt = filepath.Join(t.TempDir(), "v", "wt")
	if out, err := a.Gitx.Git(dir, "worktree", "add", "--detach", "--quiet", wt, "HEAD"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return dir, wt, a
}

// 门的 worktree 被删了：git 里的记录也要清。只读的（目录和上一层）也要清。
func TestRegressGateWorktreeCleanup(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, a *App, dir, wt string) {
		t.Helper()
		if err := gate.RemoveWorktree(dir, wt); err != nil {
			t.Fatal(err)
		}
		out, err := a.Gitx.Git(dir, "worktree", "list", "--porcelain")
		if err != nil || strings.Count(out, "worktree ") != 1 {
			t.Fatalf("worktree 残留：%v\n%s", err, out)
		}
		if _, err := os.Lstat(wt); !os.IsNotExist(err) {
			t.Fatalf("目录还在：%v", err)
		}
	}
	t.Run("被删了", func(t *testing.T) {
		dir, wt, a := gateTestRepo(t)
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		check(t, a, dir, wt)
	})
	t.Run("只读", func(t *testing.T) {
		dir, wt, a := gateTestRepo(t)
		if err := os.MkdirAll(filepath.Join(wt, "sub", "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{filepath.Join(wt, "sub", "deep"), filepath.Join(wt, "sub"), wt, filepath.Dir(wt)} {
			if err := os.Chmod(p, 0o555); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { os.Chmod(filepath.Dir(wt), 0o755) })
		check(t, a, dir, wt)
	})
}

// 结果收了之后 worktree 还在（比如当时删不动）：之后的 tick 继续清，不只试一次。
func TestRegressGateCleanupRetried(t *testing.T) {
	t.Parallel()
	dir, _, a := gateTestRepo(t)
	vd := a.Gate.Dir("job1")
	if err := os.MkdirAll(vd, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := gate.Worktree(vd)
	if out, err := a.Gitx.Git(dir, "worktree", "add", "--detach", "--quiet", wt, "HEAD"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.MkdirAll(a.JobDir("job1"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{{ID: "job1", ProjectDir: dir, Project: "x", Gate: &state.GateRecord{State: state.GateStuck}}}}
	a.Gate.Collect(st)
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("应该清掉：%v", err)
	}
}

// 不做清单是指向不存在文件的软链接：写不进去时不能把链接删了。
func TestRegressNotDoingDanglingLink(t *testing.T) {
	t.Parallel()
	dir, _, a := gateTestRepo(t)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n"), 0o644)
	link := filepath.Join(dir, "docs", "NOT-DOING.md")
	if err := os.Symlink("/nonexistent/not-doing.md", link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := a.Gitx.Git(dir, args...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	p := backlog.Project{Name: "repo", Dir: dir}
	err := a.Suggest.CommitNotDoing(p, "不做", &backlog.NotDoingEntry{Title: "甲", Quote: "不要", Date: "2026-11-04"}, func(old string) (string, error) { return old, nil })
	if err == nil {
		t.Error("写不进去应报错")
	}
	fi, lerr := os.Lstat(link)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("软链接不能动：%v %v", fi, lerr)
	}
}

// 任务报告做完了，条目却一直留在「待做」：第一次等 retry_after 再试，第二次还这样就标卡住，不能每 12 小时重做一遍一直花钱。
func TestRegressReportedDoneButStillListed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "alpha", "- [ ] 没移走 —— 做不完\n")
	start := time.Now()
	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Format(time.RFC3339)}
	e.mustRun("tick")
	first := e.jobByTitle("没移走")
	waitExit(t, first.PID, 10*time.Second)
	e.mustRun("tick")
	if j := e.jobByTitle("没移走"); j.Status != state.StatusDone || !j.StillListed || j.StuckPending != "" {
		t.Fatalf("第一次只记下：%+v", j)
	}
	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(13*time.Hour).Format(time.RFC3339)}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("过了 retry_after 再试一次：%s", out)
	}
	var second *Job
	for _, j := range e.state().Jobs {
		if j.Status == state.StatusRunning {
			second = j
		}
	}
	if second == nil {
		t.Fatal("没有第二次")
	}
	waitExit(t, second.PID, 10*time.Second)
	e.mustRun("tick")
	b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	if !strings.Contains(string(b), "（卡住：连续两次报告做完") {
		t.Fatalf("第二次要标卡住：%s", b)
	}
	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(40*time.Hour).Format(time.RFC3339)}
	if out := e.mustRun("tick"); strings.Contains(out, "启动 1") {
		t.Fatalf("标了卡住不该再起：%s", out)
	}
	// 中间正常做完过的不算连续：别的项目同样文字的也不算
	if len(e.state().Jobs) != 2 {
		t.Fatalf("只该跑两次：%d", len(e.state().Jobs))
	}
}

// 同标题的前一条已卡住或被别人认领：卡住标记要落到任务做的那条上，不能打错条，也不能因此一直重做。
func TestRegressStillListedSameTitle(t *testing.T) {
	t.Parallel()
	start := time.Now()
	for name, first := range map[string]string{
		"前一条已卡住": "- [ ] 同名 —— 细节一（卡住：别的原因）\n",
		"前一条进行中": "- [ ] 同名 —— 细节一（进行中：agent/x " + start.Format("2006-01-02 15:04") + "）\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			dir := orphanRepo(t, e, "alpha", first+"- [ ] 同名 —— 细节二\n")
			run := func(at time.Time) {
				t.Helper()
				e.extra = []string{"AGENTQ_TEST_NOW=" + at.Format(time.RFC3339)}
				if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
					t.Fatalf("该起一个：%s", out)
				}
				for _, j := range e.state().Jobs {
					if j.Status == state.StatusRunning {
						waitExit(t, j.PID, 10*time.Second)
					}
				}
				e.mustRun("tick")
			}
			run(start)
			run(start.Add(13 * time.Hour))
			e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(14*time.Hour).Format(time.RFC3339)}
			if out := e.mustRun("tick"); strings.Contains(out, "启动 1") {
				t.Errorf("标了卡住不该再起：%s", out)
			}
			b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
			var one, two string
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, "细节一") {
					one = l
				}
				if strings.Contains(l, "细节二") {
					two = l
				}
			}
			if !strings.Contains(one, strings.TrimSpace(strings.TrimPrefix(first, "- [ ] "))) {
				t.Errorf("前一条不该被改：%s", one)
			}
			if !strings.Contains(two, "（卡住：连续两次报告做完") {
				t.Errorf("这一条要标卡住：%s", two)
			}
			if n := len(e.state().Jobs); n != 2 {
				t.Errorf("只该跑两次：%d", n)
			}
		})
	}
}

// 撞用量上限的那次没开工，不算第二次「报告做完」。
func TestRegressLimitHitNotStillListed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "alpha", "- [ ] 没开工 —— HITOK\n")
	start := time.Now()
	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Format(time.RFC3339)}
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("没开工").PID, 10*time.Second)
	e.mustRun("tick")
	// 把第一次改成「报告做完、条目还在」，第二次才是撞上限
	path := filepath.Join(e.home, ".agentq", "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	job := st["jobs"].([]any)[0].(map[string]any)
	delete(job, "limit_hit")
	job["still_listed"] = true
	delete(st, "cooldown_until")
	raw, _ = json.Marshal(st)
	os.WriteFile(path, raw, 0o644)

	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(13*time.Hour).Format(time.RFC3339)}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("过了 retry_after 该再起：%s", out)
	}
	for _, j := range e.state().Jobs {
		if j.Status == state.StatusRunning {
			waitExit(t, j.PID, 10*time.Second)
		}
	}
	e.mustRun("tick")
	var second *Job
	for _, j := range e.state().Jobs {
		if j.ID != job["id"] {
			second = j
		}
	}
	if second == nil || !second.LimitHit {
		t.Fatalf("第二次该是撞上限：%+v", second)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	if second.StillListed || second.StuckPending != "" || strings.Contains(string(b), "卡住") {
		t.Errorf("撞上限不算报告做完：%+v\n%s", second, b)
	}
}

// 条目里有不是 UTF-8 的字节：进了 claude 的 argv 之后，macOS 的 ps 在 UTF-8 区域下输出会变乱并截断，
// 看不到 --name 标记，活着的任务被当成死了，名额空出来又接一个，超过 max_parallel（种子 801 第 31 轮）。
// 坏字节可能在正文（进 prompt），也可能在标题（进 prompt 和 --name）；任务箱手写的条目走同一个入口。
func TestRegressInvalidUTF8ItemStaysAlive(t *testing.T) {
	assertAlive := func(t *testing.T, e *testEnv) {
		t.Helper()
		e.mustRun("tick")
		e.mustRun("tick")
		st := e.state()
		if len(st.Jobs) != 1 {
			t.Fatalf("应该只有一个任务：%s", fmtJobs(st))
		}
		j := st.Jobs[0]
		defer syscall.Kill(-j.PID, syscall.SIGKILL) // 判错时 stop 不会杀它
		if j.Status != state.StatusRunning {
			t.Errorf("任务还在跑，不能收尾：%s %s", j.Status, j.Summary)
		}
		if !proc.ProcAlive(j.PID, proc.NameMarker(j.Project)) {
			t.Errorf("认不出活着的任务 pid %d", j.PID)
		}
		for _, arg := range j.Args {
			if !utf8.ValidString(arg) {
				t.Errorf("参数里还有坏字节：%q", arg)
			}
		}
		if _, code := e.run("stop", j.ID); code != 0 && j.Status == state.StatusRunning {
			t.Errorf("stop 退出码 %d", code)
		}
	}
	t.Run("正文", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		testkit.MkProject(t, e.root, "alpha", "- [ ] 坏字节 —— SLEEP=30 \xff\xfe\xc0\n")
		assertAlive(t, e)
	})
	t.Run("标题", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		testkit.MkProject(t, e.root, "alpha", "- [ ] 坏\xff标题 —— SLEEP=30\n")
		assertAlive(t, e)
	})
	t.Run("任务箱", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		e.writeInbox("## 待做\n- [ ] 坏\xff标题 —— SLEEP=30\n")
		assertAlive(t, e)
	})
}

// tick 读了配置之后可能等很久（等锁、杀不理 SIGTERM 的进程组），这期间用户 pause 了，它不能再接活（种子 802 第 28 轮：pause 之后 19 秒还启动了 3 个）。
func TestRegressPauseDuringTickStartsNothing(t *testing.T) {
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=30\n")
	var out bytes.Buffer
	a, err := newApp(e.home, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv("AGENTQ_FAKE_LOG", e.fakeLg)
	old := runner.JobExecutable
	runner.JobExecutable = func() (string, error) { return agentqBin, nil }
	t.Cleanup(func() { runner.JobExecutable = old })
	if err := config.SetPaused(a.ConfigPath(), true); err != nil { // a 手里还是 pause 之前读的配置
		t.Fatal(err)
	}
	if err := a.Sched.Tick(""); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	for _, j := range st.Jobs {
		syscall.Kill(-j.PID, syscall.SIGKILL)
	}
	if len(st.Jobs) != 0 {
		t.Errorf("已经暂停了，不该启动：%s\n%s", fmtJobs(st), out.String())
	}
	if !strings.Contains(out.String(), "已暂停") {
		t.Errorf("日志要说已暂停：\n%s", out.String())
	}
}

// 登录检查时换一下假 claude：查登录的那一刻跑 during，之后照常。用来造「tick 已经过了开头的重读，还没启动任务」这个时刻。
func duringAuthCheck(t *testing.T, e *testEnv, during string) {
	t.Helper()
	orig := e.fake + ".orig"
	if err := os.Rename(e.fake, orig); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = auth ]; then %s; fi\nexec %q \"$@\"\n", during, orig)
	if err := os.WriteFile(e.fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 查登录和额度最多要一分多钟，这期间 pause 了，同一次 tick 不能再启动任务。
func TestRegressPauseDuringReadyStartsNothing(t *testing.T) {
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=30\n")
	duringAuthCheck(t, e, fmt.Sprintf("%q pause >/dev/null 2>&1", agentqBin))
	out := e.mustRun("tick")
	st := e.state()
	for _, j := range st.Jobs {
		syscall.Kill(-j.PID, syscall.SIGKILL)
	}
	if len(st.Jobs) != 0 {
		t.Errorf("已经暂停了，不该启动：%s\n%s", fmtJobs(st), out)
	}
}

// 接活之前配置坏了：不知道有没有暂停，这次不接活，要记日志。
func TestRegressConfigBrokenDuringReadyStartsNothing(t *testing.T) {
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=30\n")
	duringAuthCheck(t, e, fmt.Sprintf("printf '{坏' > %q", filepath.Join(e.home, ".agentq", "config.json")))
	out := e.mustRun("tick")
	st := e.state()
	for _, j := range st.Jobs {
		syscall.Kill(-j.PID, syscall.SIGKILL)
	}
	if len(st.Jobs) != 0 {
		t.Errorf("配置读不出，不该启动：%s\n%s", fmtJobs(st), out)
	}
	if !strings.Contains(out, "配置读不出") {
		t.Errorf("日志要说配置读不出：\n%s", out)
	}
}

// 引擎写的「已完成」丢了条目正文：用户的决定、引用的提交都没了，质量统计要看它（暴力测试 892 cards）。
func TestRegressMergedLineKeepsBody(t *testing.T) {
	t.Parallel()
	text := "## 待做\n\n- [ ] 定 ITEM —— 说明（用户决定：A 做法一）（进行中：agent/x 10-10 03:00）\n\n## 已完成\n"
	got := backlog.MarkMerged(text, "x", "定 ITEM", "", backlog.MergedLine{Commit: "abc123456789", Acceptance: "看"})
	if !strings.Contains(got, "- [x] 定 ITEM —— 说明（用户决定：A 做法一）；合并提交 abc123456789；验收：看") {
		t.Errorf("%s", got)
	}
}

// 主目录挡着合并的任务每次 tick 都重起门进程，占着名额，max_parallel 小的时候待做永远接不上（暴力测试 892 backlog）。
func TestRegressWaitingGateFreesSlot(t *testing.T) {
	t.Parallel()
	_, _, a := gateTestRepo(t)
	dir := a.Gate.Dir("job1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(a.JobDir("job1"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"outcome":"wait","reason":"主目录不在 main"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Gate.SelfPath = "/usr/bin/true"
	a.Cfg.MaxParallel = 1
	st := &State{Jobs: []*Job{{ID: "job1", Project: "x", ProjectDir: t.TempDir(), Gate: &state.GateRecord{State: state.GateRunning}}}}
	a.Gate.Collect(st)
	if got := a.Gate.StartQueued(st); got != 0 {
		t.Fatalf("隔一个定时 tick 再试：%d", got)
	}
	if ok, why := state.Capacity(st, a.Cfg, "x"); !ok {
		t.Errorf("名额要空出来：%s", why)
	}
}

// BACKLOG 尾巴卡在没关的注释里：引擎每合并一条就多加一个「## 已完成」，都写进注释里（暴力测试 893 backlog）。
func TestRegressMergedInOpenComment(t *testing.T) {
	t.Parallel()
	text := "## 待做\n\n- [ ] 甲\n- [ ] 乙\n<!-- 没关\n"
	got := backlog.MarkMerged(text, "x", "甲", "", backlog.MergedLine{Commit: "c1"})
	got = backlog.MarkMerged(got, "x", "乙", "", backlog.MergedLine{Commit: "c2"})
	if n := strings.Count(got, "\n## 已完成"); n != 1 {
		t.Errorf("多出了 %d 节：%s", n, got)
	}
	if strings.Index(got, "合并提交 c2") > strings.Index(got, "<!-- 没关") {
		t.Errorf("写进注释等于没写：%s", got)
	}
}

// 暴力测试 stoplock：tick 卡住拿着锁时 status 还写「调度：正常」，用户只看得到任务一直不动。status 要说出谁拿着。
func TestRegressStatusSaysWhoHoldsTickLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if out := e.mustRun("status"); strings.Contains(out, "拿着 tick.lock") {
		t.Errorf("没人拿锁时不该提：\n%s", out)
	}
	lock, err := fsx.AcquireLock(filepath.Join(e.home, ".agentq", "tick.lock"), 0)
	if err != nil || lock == nil {
		t.Fatalf("测试拿锁失败：%v", err)
	}
	testkit.AssertContains(t, e.mustRun("status"), fmt.Sprintf("pid %d 拿着 tick.lock", os.Getpid()), "一直这样就是卡住了")
	lock.Close()
	if out := e.mustRun("status"); strings.Contains(out, "拿着 tick.lock") {
		t.Errorf("放开后不该提：\n%s", out)
	}
}

// 暴力测试 corruptboth：state.json 和运行中任务的 meta.json 一起坏了，记录全丢，进程还活着，没人收尾也没人管超时，
// 下一次 tick 又起一个做同一条，名额也超了。重建时要杀掉，留痕写清，下次 tick 再接这一条时只有一个进程在做。
func TestRegressCorruptStateAndMetaKillsLostJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n")
	e.mustRun("tick")
	j := e.jobByTitle("睡很久")
	if err := syscall.Kill(-j.PID, 0); err != nil {
		t.Fatalf("进程组 %d 应该在跑：%v", j.PID, err)
	}
	data := filepath.Join(e.home, ".agentq")
	for _, p := range []string{filepath.Join(data, "state.json"), filepath.Join(data, "jobs", j.ID, "meta.json")} {
		if err := os.WriteFile(p, []byte(`{"jobs": [ {"id": `), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testkit.AssertContains(t, e.mustRun("tick"), "本次只收尾不启动")
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Errorf("记录丢了的任务，进程组 %d 应已被杀掉", j.PID)
	}
	testkit.AssertContains(t, e.nightly(), "1 个任务的 meta.json 读不出，没算进去："+j.ID, "还活着的进程已杀掉", j.ID+"（")
	e.mustRun("tick")
	if got := e.jobByTitle("睡很久"); got.ID == j.ID || got.Status != state.StatusRunning {
		t.Errorf("下次 tick 应重新接这一条：%+v", got)
	}
}

// corruptLost 弄坏状态文件和一个任务的 meta.json，跑一次 tick。
func corruptLost(t *testing.T, e *testEnv, id string) {
	t.Helper()
	data := filepath.Join(e.home, ".agentq")
	if err := os.MkdirAll(filepath.Join(data, "jobs", id), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(data, "state.json"), filepath.Join(data, "jobs", id, "meta.json")} {
		if err := os.WriteFile(p, []byte(`{"jobs": [ {"id": `), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.mustRun("tick")
}

// bystander 起一个和这个安装无关的长进程，自己一个进程组。返回的 channel 在它退出时关闭（要回收，不然被杀了也是僵尸，kill -0 还能成功）。
func bystander(t *testing.T, script string, env []string, args ...string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	c := exec.Command(script, args...)
	c.Env = env
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		c.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		<-done
	})
	time.Sleep(200 * time.Millisecond)
	return c, done
}

func stillAlive(t *testing.T, c *exec.Cmd, done chan struct{}, why string) {
	t.Helper()
	select {
	case <-done:
		t.Errorf("%s：进程 %d 被杀了", why, c.Process.Pid)
	case <-time.After(500 * time.Millisecond):
	}
}

// 老任务留下的 claude.pid，pid 早已是别人的（比如前台开在 agentq 目录里的 claude）：外壳不在就不信它。
func TestLostJobStalePidFileNotKilled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, done := bystander(t, script, os.Environ(), "--add-dir", "/x/projects/agentq", "--verbose")
	id := "0101-000000-old"
	jobs := filepath.Join(e.home, ".agentq", "jobs", id)
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, "claude.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	corruptLost(t, e, id)
	stillAlive(t, c, done, "外壳不在时 claude.pid 不可信")
}

// 别的安装（另一个 HOME）有同名任务：不能互杀。任务 id 不含 HOME。
func TestLostJobOtherHomeNotKilled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	id := "1106-120000-alpha"
	env := []string{"HOME=" + t.TempDir(), "AGENTQ_JOB=" + id, "AGENTQ_FAKE_SLEEP=60", "PATH=" + os.Getenv("PATH")}
	c, done := bystander(t, sleeperBin(t), env, "60")
	corruptLost(t, e, id)
	stillAlive(t, c, done, "别的安装的任务")
}

// meta.json 读不了（权限、IO）不等于内容坏了：任务可能好好的，当未知，不杀。
func TestLostJobUnreadableMetaIsUnknown(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 睡很久 —— SLEEP=30\n")
	e.mustRun("tick")
	j := e.jobByTitle("睡很久")
	data := filepath.Join(e.home, ".agentq")
	if err := os.Chmod(filepath.Join(data, "jobs", j.ID, "meta.json"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(data, "jobs", j.ID, "meta.json"), 0o644)
		syscall.Kill(-j.PID, syscall.SIGKILL)
	})
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(`{"jobs": [ {"id": `), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run("tick")
	e.run("tick")
	if err := syscall.Kill(-j.PID, 0); err != nil {
		t.Errorf("meta.json 只是读不了，任务不能杀：%v", err)
	}
	if bad, _ := filepath.Glob(filepath.Join(data, "state.json.bad-*")); len(bad) != 0 {
		t.Errorf("当未知时不备份，不然每次 tick 多一份：%v", bad)
	}
}

// sleeperBin 编一个能被 ps -E 读到环境的小程序（macOS 系统自带的 sleep、sh 读不到）。
func sleeperBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	code := "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(60 * time.Second) }\n"
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "sleeper")
	c := exec.Command("go", "build", "-o", bin, src)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return bin
}

// 任务目录里有改不好的东西（meta.json 是目录、任务目录是普通文件）：算记录丢了，状态照样重建，不能永远卡住、一次次备份。
func TestRecoverStateOddJobDirs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	data := filepath.Join(e.home, ".agentq")
	if err := os.MkdirAll(filepath.Join(data, "jobs", "dir-meta", "meta.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "jobs", "file-not-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(`{"jobs":[{"id":"x","status":"done","ended_at":"bad"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	e.mustRun("tick")
	if _, err := state.Load(filepath.Join(data, "state.json")); err != nil {
		t.Errorf("状态要重建好：%v", err)
	}
	if bad, _ := filepath.Glob(filepath.Join(data, "state.json.bad-*")); len(bad) != 1 {
		t.Errorf("只备份一次：%v", bad)
	}
}
