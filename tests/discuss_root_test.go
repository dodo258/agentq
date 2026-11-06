package tests

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/discuss"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 大方向商量的用例：G7 的验收。假 claude 里生成问题的那次看 $HOME/fake-discuss.json。

const discussItem = "- [ ] 做个主页（大方向） —— 要好看；验收：打开看"
const otherItem = "- [ ] 别的活 —— 普通的活；验收：看"

// sentLastWith：最后一条文字里有 text 的消息。
func (f *fakeTG) sentLastWith(text string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if strings.Contains(f.sent[i].Get("text"), text) {
			return f.sent[i]
		}
	}
	return url.Values{}
}

// msgIDWith：最后一条文字里有 text 的消息的 id（假 Telegram 按发送顺序编号）。
func (f *fakeTG) msgIDWith(text string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if strings.Contains(f.sent[i].Get("text"), text) {
			return int64(1000 + i + 1)
		}
	}
	return 0
}

func discussEnv(t *testing.T, cfg map[string]any) (*testEnv, *fakeTG) {
	t.Helper()
	full := map[string]any{"paused": false, "max_parallel": 3}
	for k, v := range cfg {
		full[k] = v
	}
	e, tg := threadEnv(t, full)
	e.atNoon()
	return e, tg
}

func discussJobs(e *testEnv) []*Job {
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Kind == state.KindDiscuss {
			out = append(out, j)
		}
	}
	return out
}

// askedOnce：tick 起了生成问题的任务，等它结束，再 tick 一次把卡片发出去。
func askedOnce(t *testing.T, e *testEnv) *Job {
	t.Helper()
	e.mustRun("tick")
	jobs := discussJobs(e)
	if len(jobs) != 1 {
		t.Fatalf("要起一个生成问题的任务：%d 个\n%s", len(jobs), e.launchdLog())
	}
	waitExit(t, jobs[0].PID, 20*time.Second)
	e.mustRun("tick")
	return jobs[0]
}

func discussRecords(t *testing.T, e *testEnv) []discuss.Record {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.home, ".agentq", "discuss", "*.json"))
	var out []discuss.Record
	for _, f := range files {
		var r discuss.Record
		if err := json.Unmarshal([]byte(testkit.ReadFile(t, f)), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *testEnv) later(d time.Duration) {
	var kept []string
	for _, kv := range e.extra {
		if !strings.HasPrefix(kv, "AGENTQ_TEST_NOW=") {
			kept = append(kept, kv)
		}
	}
	now := time.Now()
	e.extra = append(kept, "AGENTQ_TEST_NOW="+time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).Add(d).Format(time.RFC3339))
}

func promptOf(t *testing.T, e *testEnv, j *Job) string {
	t.Helper()
	return testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
}

// 没配 Telegram：立刻按默认，不调模型，方向记进条目，当天开工，留痕。
func TestDiscussNoTelegramDefaultsNow(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"notify_enabled": true})
	dir := gateRepo(t, e, "demo", discussItem)
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 0 || len(discussJobs(e)) != 0 {
		t.Errorf("没配 Telegram 不调模型：%d", n)
	}
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（方向：没问，按默认开工(没配 Telegram，问不了)") {
		t.Fatalf("方向要写进条目：\n%s", b)
	}
	if !strings.Contains(testkit.GitT(t, e.home, dir, "log", "-3", "--format=%s", "main"), "定了大方向") {
		t.Errorf("要提交")
	}
	j := e.jobByTitle("做个主页（大方向）")
	if !strings.Contains(promptOf(t, e, j), "开工前定的大方向") {
		t.Errorf("任务合同要说：%s", promptOf(t, e, j))
	}
	if !strings.Contains(e.nightly(), "大方向：demo「做个主页（大方向）」用了默认（没配 Telegram，问不了）") {
		t.Errorf("当天汇总要留痕：\n%s", e.nightly())
	}
}

// 超时走默认并留痕；同一条只问一次；不挡别的条目。
func TestDiscussTimeoutUsesDefault(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem, otherItem)
	e.mustRun("tick")
	if e.findJob("别的活") == nil {
		t.Fatalf("等商量不挡别的条目：\n%s", e.launchdLog())
	}
	if e.findJob("做个主页（大方向）") != nil {
		t.Fatal("这一条要等")
	}
	jobs := discussJobs(e)
	if len(jobs) != 1 {
		t.Fatalf("%d", len(jobs))
	}
	waitExit(t, jobs[0].PID, 20*time.Second)
	out := e.mustRun("tick")
	sent := strings.Join(tg.texts(), "\n")
	for _, want := range []string{"开工前商量大方向", "想要什么感觉", "默认：简洁", "方向 A：纸墨风", "没回就按默认开工"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("卡片缺 %q：\n%s\n%s", want, sent, out)
		}
	}
	btns := keyboardOf(t, tg.sentLastWith("开工前商量大方向"))
	if len(btns) != 3 || !strings.Contains(btns[0].Text, "纸墨风") || !strings.Contains(btns[2].Text, discuss.UseDefault) {
		t.Fatalf("%+v", btns)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "等商量：demo「做个主页（大方向）」等你回") {
		t.Errorf("status 要说在等什么：\n%s", out)
	}
	if out := e.mustRun("discuss"); !strings.Contains(out, "想要什么感觉（默认：简洁）") {
		t.Errorf("%s", out)
	}
	// 反复 tick、重启都不再问
	for range 3 {
		e.mustRun("tick")
	}
	if n := len(discussJobs(e)); n != 1 {
		t.Errorf("只问一次：调了 %d 次", n)
	}
	if n := strings.Count(strings.Join(tg.texts(), "\n"), "开工前商量大方向"); n != 1 {
		t.Errorf("卡片只发一张：%d", n)
	}
	// 还没到点
	e.later(time.Hour)
	e.mustRun("tick")
	if strings.Contains(e.backlogOf(dir), "（方向：") {
		t.Fatal("还没到点")
	}
	// 到点按默认，当次开工
	e.later(2*time.Hour + time.Minute)
	e.mustRun("tick")
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（方向：按默认开工(等到 ") || !strings.Contains(b, "想要什么感觉 → 简洁") {
		t.Fatalf("%s", b)
	}
	j := e.findJob("做个主页（大方向）")
	if j == nil {
		t.Fatalf("到点要开工：\n%s", e.launchdLog())
	}
	if !strings.Contains(e.nightly(), "大方向：demo「做个主页（大方向）」用了默认") {
		t.Errorf("%s", e.nightly())
	}
	if out := e.mustRun("status"); strings.Contains(out, "等商量：") {
		t.Errorf("不用再等了：\n%s", out)
	}
	if n := len(discussJobs(e)); n != 1 {
		t.Errorf("默认开工不再调模型：%d", n)
	}
}

// 点按钮：方向写进条目，下次 tick 开工；再点说已经定了；回答过的不再问；审查提示词看得到。
func TestDiscussButton(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem)
	askedOnce(t, e)
	btns := keyboardOf(t, tg.sentLastWith("开工前商量大方向"))
	tg.click(1, ownerChat, 1000, btns[1].Data)
	e.mustRun("tick") // 这次 tick 收到按钮，下次落
	if ans := tg.lastAnswer(); !strings.Contains(ans, "已定") {
		t.Fatalf("%q", ans)
	}
	e.mustRun("tick")
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（方向：用户定：终端风；问的问题和默认答案：想要什么感觉 → 简洁）") {
		t.Fatalf("%s", b)
	}
	j := e.findJob("做个主页（大方向）")
	if j == nil {
		t.Fatalf("要开工：\n%s", e.launchdLog())
	}
	if !strings.Contains(promptOf(t, e, j), "用户定：终端风") {
		t.Errorf("任务合同看得到：%s", promptOf(t, e, j))
	}
	tg.click(2, ownerChat, 1000, btns[0].Data)
	e.mustRun("tick")
	if ans := tg.lastAnswer(); !strings.Contains(ans, "已经定了") {
		t.Errorf("%q", ans)
	}
	if n := len(discussJobs(e)); n != 1 {
		t.Errorf("回答过的不再问：%d", n)
	}
	if !strings.Contains(e.nightly(), "用户定了：用户定：终端风") {
		t.Errorf("%s", e.nightly())
	}
}

// 回复卡片：不起模型，直接算回答。回「随便」就是用默认。过期了再回，告诉用户。
func TestDiscussReply(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ reply, want string }{
		{"要 Go 后端，界面走手机优先", "（方向：用户定：要 Go 后端，界面走手机优先"},
		{"随便", "（方向：按默认开工(用户回「随便」)：想要什么感觉 → 简洁"},
	} {
		e, tg := discussEnv(t, nil)
		dir := gateRepo(t, e, "demo", discussItem)
		askedOnce(t, e)
		id := tg.msgIDWith("开工前商量大方向")
		before := len(e.claudeCalls())
		tg.replyMsg(1, 101, c.reply, id)
		e.mustRun("tick")
		if n := len(e.claudeCalls()); n != before {
			t.Errorf("回答不起模型：%d", n)
		}
		if !strings.Contains(strings.Join(tg.texts(), "\n"), "记下了") {
			t.Errorf("%q", tg.texts())
		}
		e.mustRun("tick")
		if b := e.backlogOf(dir); !strings.Contains(b, c.want) {
			t.Errorf("%s", b)
		}
		if e.findJob("做个主页（大方向）") == nil {
			t.Errorf("要开工")
		}
		tg.replyMsg(2, 102, "再改一下", id)
		e.mustRun("tick")
		if !strings.Contains(strings.Join(tg.texts(), "\n"), "已经不等回答了") {
			t.Errorf("过期的回答要说明：%q", tg.texts())
		}
	}
}

// 坏回复、进程崩了、没有问题：不崩，按默认开工，不重试。
func TestDiscussBadReplies(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ out, want string }{
		"不是JSON": {`{"type":"result","is_error":false,"result":"我觉得可以问问","session_id":"x"}`, "问题生成失败"},
		"报错":     {`{"type":"result","is_error":true,"result":"炸了","session_id":"x"}`, "问题生成失败"},
		"半截":     {`{"type":"res`, "问题生成失败"},
		"空字段":    {`{"type":"result","is_error":false,"result":"","session_id":"x","structured_output":{"questions":[{"text":"","default":""}],"directions":[]}}`, "问题生成失败"},
		"类型不对":   {`{"type":"result","is_error":false,"result":"","session_id":"x","structured_output":{"questions":"很多","directions":3}}`, "问题生成失败"},
		"没什么要问":  {`{"type":"result","is_error":false,"result":"","session_id":"x","structured_output":{"questions":[],"directions":[]}}`, "模型说没什么要问"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, _ := discussEnv(t, nil)
			dir := gateRepo(t, e, "demo", discussItem)
			os.WriteFile(filepath.Join(e.home, "fake-discuss.json"), []byte(c.out+"\n"), 0o644)
			e.mustRun("tick")
			j := discussJobs(e)[0]
			waitExit(t, j.PID, 20*time.Second)
			e.mustRun("tick")
			b := e.backlogOf(dir)
			if !strings.Contains(b, "（方向：") || !strings.Contains(b, c.want) {
				t.Fatalf("%s\n%s", b, e.launchdLog())
			}
			if e.findJob("做个主页（大方向）") == nil {
				t.Errorf("坏回复也要开工")
			}
			if n := len(discussJobs(e)); n != 1 {
				t.Errorf("不重试：%d", n)
			}
		})
	}
}

// 暂停、额度到了、已经定了方向、开关关着：不调模型。
func TestDiscussNoSpend(t *testing.T) {
	t.Parallel()
	t.Run("暂停", func(t *testing.T) {
		e, _ := discussEnv(t, map[string]any{"paused": true})
		gateRepo(t, e, "demo", discussItem)
		e.mustRun("tick")
		if n := len(discussJobs(e)); n != 0 {
			t.Errorf("%d", n)
		}
	})
	t.Run("没有标记", func(t *testing.T) {
		e, _ := discussEnv(t, nil)
		gateRepo(t, e, "demo", otherItem)
		e.mustRun("tick")
		if n := len(discussJobs(e)); n != 0 || len(discussRecords(t, e)) != 0 {
			t.Errorf("%d", n)
		}
	})
	t.Run("前台问过", func(t *testing.T) { // dispatch 在会话里问一次，把方向写进条目：后台不再问
		e, _ := discussEnv(t, nil)
		gateRepo(t, e, "demo", "- [ ] 做个主页（大方向） —— 要好看（方向：用户定：纸墨风）")
		e.mustRun("tick")
		j := e.findJob("做个主页（大方向）")
		if n := len(discussJobs(e)); n != 0 || j == nil {
			t.Fatalf("%d %v", n, j)
		}
		if !strings.Contains(promptOf(t, e, j), "用户定：纸墨风") || !strings.Contains(promptOf(t, e, j), "开工前定的大方向") {
			t.Errorf("%s", promptOf(t, e, j))
		}
	})
	t.Run("开关关着", func(t *testing.T) {
		e, _ := discussEnv(t, map[string]any{"discuss_enabled": false})
		dir := gateRepo(t, e, "demo", discussItem)
		e.mustRun("tick")
		if n := len(discussJobs(e)); n != 0 || strings.Contains(e.backlogOf(dir), "（方向：") || e.findJob("做个主页（大方向）") == nil {
			t.Errorf("%d\n%s", n, e.backlogOf(dir))
		}
	})
	t.Run("额度到了", func(t *testing.T) {
		e, _ := discussEnv(t, nil)
		gateRepo(t, e, "demo", discussItem)
		e.editState(func(st *State) {
			y, m, d := time.Now().Date()
			st.Usage = &backend.UsageInfo{CheckedAt: time.Date(y, m, d, 12, 0, 0, 0, time.Local), Windows: []backend.UsageWindow{{Label: "session", Used: 99}}}
		})
		e.mustRun("tick")
		if n := len(discussJobs(e)); n != 0 || len(discussRecords(t, e)) != 0 {
			t.Errorf("额度到了不问，下次再问：%d %+v", n, discussRecords(t, e))
		}
	})
	t.Run("条目没了", func(t *testing.T) {
		e, tg := discussEnv(t, nil)
		dir := gateRepo(t, e, "demo", discussItem)
		askedOnce(t, e)
		_ = tg
		os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n## 已完成\n"), 0o644)
		testkit.GitT(t, e.home, dir, "add", "-A")
		testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "用户删了")
		e.later(3 * time.Hour)
		e.mustRun("tick")
		rs := discussRecords(t, e)
		if len(rs) != 1 || rs[0].State != discuss.StateDropped || strings.Contains(e.backlogOf(dir), "（方向：") {
			t.Errorf("%+v\n%s", rs, e.backlogOf(dir))
		}
	})
}

// 夜里卡片发不出去：等待从能发的时候算，不然用户没看到就按默认了。
func TestDiscussNightWaitsForMorning(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem)
	now := time.Now()
	night := time.Date(now.Year(), now.Month(), now.Day(), 23, 30, 0, 0, now.Location())
	var kept []string
	for _, kv := range e.extra {
		if !strings.HasPrefix(kv, "AGENTQ_TEST_NOW=") {
			kept = append(kept, kv)
		}
	}
	e.extra = append(kept, "AGENTQ_TEST_NOW="+night.Format(time.RFC3339))
	askedOnce(t, e)
	rs := discussRecords(t, e)
	morning := time.Date(now.Year(), now.Month(), now.Day()+1, 10, 0, 0, 0, now.Location())
	if len(rs) != 1 || rs[0].State != discuss.StateWaiting || !rs[0].Until.Equal(morning) {
		t.Fatalf("要等到 %v：%+v", morning, rs)
	}
	e.extra = append(kept, "AGENTQ_TEST_NOW="+night.Add(3*time.Hour).Format(time.RFC3339))
	e.mustRun("tick")
	if strings.Contains(e.backlogOf(dir), "（方向：") {
		t.Errorf("夜里过了 discuss_wait 也不按默认：\n%s", e.backlogOf(dir))
	}
}

// 生成问题的进程还在跑：下次 tick 认得出，不当它结束了，也不多起一个。
func TestDiscussJobStaysAlive(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 做个主页（大方向） —— SLEEP=20")
	e.mustRun("tick")
	e.mustRun("tick")
	jobs := discussJobs(e)
	if len(jobs) != 1 || jobs[0].Status != state.StatusRunning {
		t.Fatalf("还在跑：%+v", jobs)
	}
	if rs := discussRecords(t, e); len(rs) != 1 || rs[0].State != discuss.StateAsking {
		t.Errorf("%+v", rs)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "在生成问题") {
		t.Errorf("%s", out)
	}
	e.mustRun("stop", jobs[0].ID)
	if rs := discussRecords(t, e); rs[0].State != discuss.StateDecided || !strings.Contains(rs[0].Why, "问题生成失败") {
		t.Errorf("停掉了就按默认：%+v", rs[0])
	}
}

// 进程在落记录之后、起任务之前被杀：不再花钱问，按默认。
func TestDiscussLostJobDefaults(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem)
	rec := discuss.Record{Key: discuss.ItemKey("demo", "做个主页（大方向）"), Kind: discuss.KindItem, Project: "demo", Dir: dir, Title: "做个主页（大方向）", State: discuss.StateAsking, AskedAt: time.Now()}
	b, _ := json.Marshal(rec)
	os.MkdirAll(filepath.Join(e.home, ".agentq", "discuss"), 0o700)
	os.WriteFile(filepath.Join(e.home, ".agentq", "discuss", rec.Key+".json"), b, 0o644)
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 0 {
		t.Errorf("%d", n)
	}
	if !strings.Contains(e.backlogOf(dir), "生成问题的进程没了") || e.findJob("做个主页（大方向）") == nil {
		t.Errorf("%s", e.backlogOf(dir))
	}
}

// 记录文件坏了：不知道问过没有，不再花钱，按默认。
func TestDiscussBadRecord(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem)
	os.MkdirAll(filepath.Join(e.home, ".agentq", "discuss"), 0o700)
	os.WriteFile(filepath.Join(e.home, ".agentq", "discuss", discuss.ItemKey("demo", "做个主页（大方向）")+".json"), []byte("{坏"), 0o644)
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 0 {
		t.Errorf("%d", n)
	}
	if !strings.Contains(e.backlogOf(dir), "记录读不了") {
		t.Errorf("%s", e.backlogOf(dir))
	}
}

// 后端做不到（Codex 没有无工具模式）：不失败，按默认，如实记。
func TestDiscussBackendLacks(t *testing.T) {
	t.Parallel()
	fake := codexFake(t)
	e, _ := discussEnv(t, map[string]any{"project_backends": map[string]any{"demo": "codex"}, "backends": map[string]any{"codex": map[string]any{"path": fake, "permission": "danger-full-access"}}})
	dir := gateRepo(t, e, "demo", "- [ ] 条目 ITEM-qqq（大方向） —— MODE=ok 验收：看着办")
	out := e.mustRun("tick")
	if len(discussJobs(e)) != 0 {
		t.Errorf("不该起：%+v", discussJobs(e))
	}
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（方向：") || !strings.Contains(b, "后端生成不了问题") {
		t.Fatalf("%s\n%s", b, out)
	}
}

// agentq new：配了 Telegram 先问，到点按默认开工，方向进提示词；--direction 已经定了就直接开工；没配 Telegram 和以前一样。
func TestDiscussNew(t *testing.T) {
	t.Parallel()
	t.Run("问了再开工", func(t *testing.T) {
		e, tg := discussEnv(t, nil)
		out := e.mustRun("new", "NEWOK 做个工具")
		if !strings.Contains(out, "先生成大方向问题") {
			t.Fatalf("%s", out)
		}
		if len(e.state().Jobs) != 1 || e.state().Jobs[0].Kind != state.KindDiscuss {
			t.Fatalf("%+v", e.state().Jobs)
		}
		if again := e.mustRun("new", "NEWOK 做个工具"); !strings.Contains(again, "已经在商量") {
			t.Errorf("同一句话不再问：%s", again)
		}
		waitExit(t, e.state().Jobs[0].PID, 20*time.Second)
		e.mustRun("tick")
		if !strings.Contains(strings.Join(tg.texts(), "\n"), "开工前商量大方向") {
			t.Fatalf("%q", tg.texts())
		}
		e.later(2*time.Hour + time.Minute)
		e.mustRun("tick")
		var news []*Job
		for _, j := range e.state().Jobs {
			if j.Kind == state.KindNew || j.NewReport != nil {
				news = append(news, j)
			}
		}
		if len(news) != 1 {
			t.Fatalf("到点要开工：%+v\n%s", e.state().Jobs, e.launchdLog())
		}
		if p := promptOf(t, e, news[0]); !strings.Contains(p, "开工前定的大方向") || !strings.Contains(p, "想要什么感觉 → 简洁") || !strings.Contains(p, "（方向：按默认开工") {
			t.Errorf("%s", p)
		}
		if n := len(discussJobs(e)); n != 1 {
			t.Errorf("%d", n)
		}
	})
	t.Run("直接给方向", func(t *testing.T) {
		e, _ := discussEnv(t, nil)
		e.mustRun("new", "--direction", "纸墨风，Go 后端", "NEWOK 做个工具")
		j := e.state().Jobs[0]
		if j.Kind != state.KindNew || j.Direction != "纸墨风，Go 后端" {
			t.Fatalf("%+v", j)
		}
		if p := promptOf(t, e, j); !strings.Contains(p, "纸墨风，Go 后端") {
			t.Errorf("%s", p)
		}
		if n := len(discussJobs(e)); n != 0 {
			t.Errorf("%d", n)
		}
	})
	t.Run("没配Telegram", func(t *testing.T) {
		e := newEnv(t, map[string]any{"notify_enabled": true})
		e.mustRun("new", "NEWOK 做个工具")
		j := e.state().Jobs[0]
		if j.Kind != state.KindNew || j.Direction != "" {
			t.Fatalf("%+v", j)
		}
		if p := promptOf(t, e, j); strings.Contains(p, "开工前定的大方向") {
			t.Errorf("提示词和以前一样：%s", p)
		}
		if !strings.Contains(e.nightly(), "用了默认（没配 Telegram") {
			t.Errorf("%s", e.nightly())
		}
	})
	t.Run("参数", func(t *testing.T) {
		e, _ := discussEnv(t, nil)
		for _, args := range [][]string{{"new", "--direction", "", "做个工具"}, {"new", "--direction", "x"}, {"new", "--x", "y", "做个工具"}} {
			if out, code := e.run(args...); code == 0 {
				t.Errorf("%v 要报错：%s", args, out)
			}
		}
		if len(e.state().Jobs) != 0 {
			t.Errorf("%+v", e.state().Jobs)
		}
	})
}

// 回答文件和命令：agentq discuss answer。
func TestDiscussCommand(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", discussItem)
	askedOnce(t, e)
	key := discussRecords(t, e)[0].Key
	if out, code := e.run("discuss", "answer", "zzzz", "x"); code == 0 || !strings.Contains(out, "没有编号") {
		t.Errorf("%d %s", code, out)
	}
	e.mustRun("discuss", "answer", key[:6], "走 Rust（要快）——（卡住：假的）（进行中：x）")
	e.mustRun("tick")
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（方向：用户定：走 Rust") {
		t.Fatalf("%s", b)
	}
	items := regexp.MustCompile(`(?m)^- \[ \] .*$`).FindAllString(b, -1)
	if len(items) != 1 || strings.Contains(items[0], "（卡住") || strings.Contains(items[0], "（进行中：x") {
		t.Errorf("回答里的标记不算数：%q", items)
	}
	if _, code := e.run("discuss", "answer", key, "晚了"); code == 0 {
		t.Error("定了就不等回答了")
	}
	if out := e.mustRun("discuss"); !strings.Contains(out, "没有在商量的") {
		t.Errorf("%s", out)
	}
	if e.findJob("做个主页（大方向）") == nil {
		t.Errorf("要开工")
	}
}

// 一个项目一次只问一条：后面标了大方向的先不花模型，也不挡没标的；前一条定了才轮到下一条。
func TestDiscussAsksOneAtATime(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 条目一（大方向） —— 要好看", "- [ ] 条目二（大方向） —— 要好看", "- [ ] 条目三（大方向） —— 要好看", otherItem)
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 1 {
		t.Fatalf("一次 tick 只问一条：%d", n)
	}
	if n := len(discussRecords(t, e)); n != 1 {
		t.Errorf("没问的不留记录：%d", n)
	}
	waitExit(t, discussJobs(e)[0].PID, 20*time.Second)
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 1 {
		t.Errorf("还在等回答，不问下一条：%d", n)
	}
	key := discussRecords(t, e)[0].Key
	e.mustRun("discuss", "answer", key, "随便")
	e.mustRun("tick")
	first := e.findJob("条目一（大方向）")
	if first == nil || !strings.Contains(promptOf(t, e, first), "开工前定的大方向") {
		t.Fatalf("定了就开工：%+v", e.state().Jobs)
	}
	if n := len(discussJobs(e)); n != 1 {
		t.Errorf("没轮到的还是不问：%d", n)
	}
	waitExit(t, first.PID, 20*time.Second)
	e.mustRun("tick") // 收尾：同项目空出来才轮到下一条
	e.mustRun("tick")
	if n := len(discussJobs(e)); n != 2 {
		t.Errorf("前一条开工了，轮到下一条：%d\n%s", n, e.launchdLog())
	}
}

// 照提示带 --direction 开工：同一句话在等的那条了结，到点不再起一个新项目，卡片也撤掉。
func TestDiscussNewDirectedSettlesWaiting(t *testing.T) {
	t.Parallel()
	e, _ := discussEnv(t, nil)
	e.mustRun("new", "NEWOK 做个工具")
	waitExit(t, discussJobs(e)[0].PID, 20*time.Second)
	e.mustRun("tick")
	e.mustRun("new", "--direction", "随便", "NEWOK 做个工具")
	count := func() (n int) {
		for _, j := range e.state().Jobs {
			if j.Kind == state.KindNew || j.NewReport != nil { // 改认第一条之后就不是 KindNew 了
				n++
				waitExit(t, j.PID, 30*time.Second)
			}
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("%d", n)
	}
	e.mustRun("tick")                  // 收尾，排上门
	for _, j := range e.state().Jobs { // 第一版要过门：等门进程写完，不然它还在往临时目录写
		if j.NewReport != nil || j.Kind == state.KindNew {
			e.waitGate(j)
		}
	}
	e.later(2*time.Hour + time.Minute)
	e.mustRun("tick")
	e.mustRun("tick")
	if n := count(); n != 1 {
		t.Errorf("同一句话只起一个新项目：%d", n)
	}
	if out := e.mustRun("discuss"); strings.Contains(out, "等你回") {
		t.Errorf("不再等回答：%s", out)
	}
}
