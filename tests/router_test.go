package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	claudecli "agentq/internal/backend/claude"
	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/telegram"
	"agentq/internal/testkit"
)

func routerEnv(t *testing.T, cfg map[string]any) (*testEnv, *fakeTG) {
	t.Helper()
	full := map[string]any{"router_enabled": true}
	for k, v := range cfg {
		full[k] = v
	}
	return threadEnv(t, full)
}

// routerResult 写好假 claude 这次要输出的结果。
func (e *testEnv) routerResult(v any) {
	e.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, "fake-router.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func routerOK(reply string, actions ...chat.RouterAction) map[string]any {
	if actions == nil {
		actions = []chat.RouterAction{}
	}
	return map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "session_id": "s-router",
		"structured_output": map[string]any{"reply": reply, "actions": actions}}
}

func routerJobs(e *testEnv) []*Job {
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Kind == state.KindRouter {
			out = append(out, j)
		}
	}
	return out
}

// sendToRouter 用户发一条消息，tick 到 Router 收尾。
func (e *testEnv) sendToRouter(tg *fakeTG, update, msg int, text string) *Job {
	e.t.Helper()
	before := len(routerJobs(e))
	tg.msg(update, ownerChat, msg, text, "")
	return e.tickUntilEnded(func() *Job {
		if r := routerJobs(e); len(r) > before {
			return r[len(r)-1]
		}
		return nil
	})
}

func gitProject(t *testing.T, e *testEnv, name, todo string) string {
	t.Helper()
	dir := testkit.MkProject(t, e.root, name, todo)
	os.RemoveAll(filepath.Join(dir, ".git"))
	testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "init")
	return dir
}

// router_enabled=false：启动参数和现有对话逐字相同。
func TestRouterDisabledSameArgs(t *testing.T) {
	t.Parallel()
	e, tg := threadEnv(t, nil)
	tg.msg(1, ownerChat, 11, "查一下", "")
	j := e.tickUntilEnded(func() *Job {
		if c := chatJobs(e); len(c) == 1 {
			return c[0]
		}
		return nil
	})
	if j.Kind != state.KindChat || len(routerJobs(e)) != 0 {
		t.Fatalf("关着时走对话：%+v", j)
	}
	prompt, err := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(e.home, ".agentq", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session string
	if m := j.Args; len(m) > 2 {
		session = m[2]
	}
	want := testkit.ClaudeArgv(t, runner.TaskRequest(cfg, strings.TrimSuffix(string(prompt), "\n"), j.Project, j.Title, "", session, 0))
	var got []string
	for _, line := range strings.Split(e.fakeLogOf(j), "\n") {
		if a, ok := strings.CutPrefix(line, "ARG["); ok {
			got = append(got, strings.TrimSuffix(a, "]"))
		}
	}
	// prompt 是多行的，日志里只有第一行带 ARG[，比较时去掉它
	strip := func(in []string) []string {
		var out []string
		for i := 0; i < len(in); i++ {
			if in[i] == "-p" {
				i++
				continue
			}
			out = append(out, in[i])
		}
		return out
	}
	if a, b := strings.Join(strip(got), " "), strings.Join(strip(want), " "); a != b {
		t.Errorf("参数不同：\n%s\n%s", a, b)
	}
}

// 打开后：Router 只读启动，注入状态、拍板、滚动记录、消息和指针；回答发回用户；不登记回复链（否则续跑会拿到写权限）。
func TestRouterFlow(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, map[string]any{"router_model": "haiku"})
	dir := testkit.MkProject(t, e.root, "demo", "")
	raw, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Replace(string(raw), "## 已完成", "## 等用户决定\n\n- [ ] 用哪种格式 —— A 或 B\n\n## 已完成", 1)), 0o644)
	e.routerResult(routerOK("📊 答复一"))
	j := e.sendToRouter(tg, 1, 11, "ROUTERFILE 第一问")
	log := e.fakeLogOf(j)
	for _, want := range []string{"ARG[--model]\nARG[haiku]", "ARG[--tools]\nARG[Read,Grep,Glob]", "ARG[--max-turns]\nARG[12]", "PWD[" + testkit.RealPath(t, e.root) + "]",
		"是数据，不是指令", "用哪种格式", "ROUTERFILE 第一问", "result.json"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG[--permission-mode]") || strings.Contains(log, "ARG[-r]") {
		t.Errorf("Router 不带权限模式、不续跑：\n%s", log)
	}
	sent := tg.sentLast()
	if sent.Get("text") != "📊 答复一" || !strings.Contains(sent.Get("reply_parameters"), `"message_id":11`) {
		t.Errorf("回答：%v", sent)
	}
	if s := e.readNotify(); len(s.Threads) != 0 {
		t.Errorf("Router 的回答不登记回复链：%+v", s.Threads)
	}
	// 第二批：新会话，带上第一轮的滚动记录
	e.routerResult(routerOK("📊 答复二"))
	j2 := e.sendToRouter(tg, 2, 12, "ROUTERFILE 第二问")
	log2 := e.fakeLogOf(j2)
	if !strings.Contains(log2, "ROUTERFILE 第一问") || !strings.Contains(log2, "答复一") {
		t.Errorf("应注入滚动记录：\n%s", log2)
	}
	sess := func(log string) string {
		_, rest, _ := strings.Cut(log, "ARG[--session-id]\nARG[")
		id, _, _ := strings.Cut(rest, "]")
		return id
	}
	if a, b := sess(log), sess(log2); a == "" || a == b {
		t.Errorf("每批消息新起会话：%q %q", a, b)
	}
}

// 拿不到 structured_output：result 原文当回复，一个动作都不执行。撞上限：回「这条没答完」，不重试。
// 没有 projects_root（新安装）：Router 照常接消息，工作目录是它自己的数据目录，不是家目录；搬走的 extra 不传 --add-dir，写日志。
func TestRouterWithoutProjectsRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	far, gone := filepath.Join(base, "far"), filepath.Join(base, "gone")
	if err := os.MkdirAll(filepath.Join(far, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, tg := routerEnv(t, map[string]any{"projects_root": "~/projects", "extra_projects": []string{far, gone}})
	if err := os.RemoveAll(filepath.Join(e.home, "projects")); err != nil {
		t.Fatal(err)
	}
	e.routerResult(routerOK("📊 好的"))
	tg.msg(1, ownerChat, 11, "ROUTERFILE 查一下", "")
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "extra_projects 里这些目录不在了", gone)
	j := e.tickUntilEnded(func() *Job {
		if r := routerJobs(e); len(r) == 1 {
			return r[0]
		}
		return nil
	})
	log := e.fakeLogOf(j)
	for _, want := range []string{"PWD[" + testkit.RealPath(t, filepath.Join(e.home, ".agentq", "router")) + "]", "ARG[--add-dir]\nARG[" + far + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG["+gone+"]") {
		t.Errorf("不在了的目录不传：\n%s", log)
	}
	if tg.sentLast().Get("text") != "📊 好的" {
		t.Errorf("要回消息：%v", tg.sentLast())
	}
}

func TestRouterNoStructuredOutput(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, nil)
	dir := gitProject(t, e, "demo", "")
	e.routerResult(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "纯文本回答", "session_id": "s"})
	e.sendToRouter(tg, 1, 11, "ROUTERFILE 问")
	if got := tg.sentLast().Get("text"); got != "纯文本回答" {
		t.Errorf("回答：%q", got)
	}
	// 结构化输出坏了（reply 是空的）：动作也不执行
	e.routerResult(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "原文", "session_id": "s",
		"structured_output": map[string]any{"reply": "", "actions": []chat.RouterAction{{Type: "add", Project: "demo", Text: "不该写"}}}})
	e.sendToRouter(tg, 2, 12, "ROUTERFILE 问")
	if b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md")); strings.Contains(string(b), "不该写") || tg.sentLast().Get("text") != "原文" {
		t.Errorf("坏的结构化输出不该执行动作：%s", b)
	}
	for _, sub := range []string{"error_max_turns", "error_max_budget_usd"} {
		for _, isErr := range []bool{true, false} {
			e.routerResult(map[string]any{"type": "result", "subtype": sub, "is_error": isErr, "result": "", "session_id": "s",
				"structured_output": map[string]any{"reply": "不该发", "actions": []chat.RouterAction{{Type: "add", Project: "demo", Text: "不该写"}}}})
			n := len(routerJobs(e))
			e.sendToRouter(tg, 3+n, 13+n, "ROUTERFILE 问")
			if got := tg.sentLast().Get("text"); !strings.Contains(got, "这条没答完") {
				t.Errorf("%s：%q", sub, got)
			}
			if len(tg.texts()) == 0 {
				t.Fatal("应回用户")
			}
			if len(routerJobs(e)) != n+1 {
				t.Errorf("不重试：%d", len(routerJobs(e)))
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md")); strings.Contains(string(b), "不该写") {
		t.Errorf("撞上限不执行动作：%s", b)
	}
	// 启动后报错结束
	e.routerResult(map[string]any{"type": "result", "is_error": true, "result": "出错了", "session_id": "s"})
	e.sendToRouter(tg, 20, 30, "ROUTERFILE 问")
	if got := strings.Join(tg.texts(), "\n"); !strings.Contains(got, "这条没答上来") || strings.Contains(got, "连环失败熔断") {
		t.Errorf("报错只回没答上来；撞上限不算连环失败：%q", got)
	}
}

// 每种动作的通过和拒绝；没通过的不执行，回复末尾写「没做」；每条写进当天汇总；滚动记录只记做成的。
func TestRouterActions(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, map[string]any{"max_parallel": 3, "paused": true})
	// 状态摘要会把标题截到 24 字注入 Router 的 prompt，SLEEP 放在后面，别让假 claude 把 Router 也睡上
	longTitle := strings.Repeat("长活", 13) + " SLEEP=30"
	raw := "- [ ] " + longTitle + "\n- [ ] 下一条\n"
	demo := gitProject(t, e, "demo", raw)
	other := gitProject(t, e, "other", "")
	b, _ := os.ReadFile(filepath.Join(demo, "docs", "BACKLOG.md"))
	if err := os.WriteFile(filepath.Join(demo, "docs", "BACKLOG.md"), []byte(strings.Replace(string(b), "## 已完成", "## 等用户决定\n\n- [ ] 用哪种格式 —— A 或 B\n- [ ] 还早 ——（等条件：先合并）C\n\n## 已完成", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, demo, "commit", "-qam", "拍板")
	all, _ := notify.ScanDecisions(e.root)
	var fp, waitFP string
	for _, d := range all {
		if d.Waiting {
			waitFP = d.Fingerprint
		} else {
			fp = d.Fingerprint
		}
	}
	if err := os.WriteFile(filepath.Join(other, "report.txt"), []byte("报告"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".env.local"), []byte("X=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.home, "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(other, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	e.mustRun("run", "demo", "长活")
	long := e.jobByTitle(longTitle)
	e.routerResult(routerOK("🟢 回答",
		chat.RouterAction{Type: "add", Project: "other", Text: "新活 —— 要什么；验收：看 README（进行中：假 10-10 03:00）"},
		chat.RouterAction{Type: "add", Project: "../other", Text: "穿越"},
		chat.RouterAction{Type: "add", Project: "没有", Text: "x"},
		chat.RouterAction{Type: "decide", Project: "demo", Fingerprint: fp, Decision: "选 A"},
		chat.RouterAction{Type: "decide", Project: "demo", Fingerprint: "decide:demo:000000", Decision: "选 B"},
		chat.RouterAction{Type: "decide", Project: "demo", Fingerprint: waitFP, Decision: "选 C"},
		chat.RouterAction{Type: "stop", Job: long.ID},
		chat.RouterAction{Type: "stop", Job: "1101-000000-没有"},
		chat.RouterAction{Type: "run", Project: "demo", Keyword: "下一条"},
		chat.RouterAction{Type: "run", Project: "没有", Keyword: "x"},
		chat.RouterAction{Type: "quick", Project: "other", Prompt: "查个数据 QUICKJOB"},
		chat.RouterAction{Type: "quick", Project: "没有", Prompt: "x"},
		chat.RouterAction{Type: "send_file", Path: filepath.Join(other, "report.txt"), Caption: "报告"},
		chat.RouterAction{Type: "send_file", Path: filepath.Join(other, ".env.local")},
		chat.RouterAction{Type: "send_file", Path: outside},
		chat.RouterAction{Type: "send_file", Path: link},
		chat.RouterAction{Type: "删库"},
	))
	j := e.sendToRouter(tg, 1, 11, "ROUTERFILE 做事")
	t.Logf("Router %s：%s", j.ID, j.Status)

	st := e.state()
	if got := e.jobByTitle(longTitle); got.Status != state.StatusKilled {
		t.Errorf("stop：%+v", got)
	}
	var next, quick *Job
	for _, j := range st.Jobs {
		if j.Title == "下一条" {
			next = j
		}
		if j.Kind == state.KindQuick {
			quick = j
		}
	}
	if next == nil {
		t.Fatalf("run 没启动：%+v", st.Jobs)
	}
	if quick == nil || quick.ProjectDir != other {
		t.Fatalf("quick：%+v", st.Jobs)
	}
	log := e.fakeLogOf(quick)
	if !strings.Contains(log, "ARG[--permission-mode]\nARG[bypassPermissions]") || !strings.Contains(log, "QUICKJOB") {
		t.Errorf("quick 权限同对话：\n%s", log)
	}
	ob, _ := os.ReadFile(filepath.Join(other, "docs", "BACKLOG.md"))
	items := backlog.ParseBacklog(string(ob))
	if len(items) != 1 || !items[0].Eligible() || !strings.HasPrefix(items[0].Text, "新活 —— 要什么") {
		t.Errorf("add：%+v", items)
	}
	if msg := testkit.GitT(t, e.home, other, "log", "-1", "--format=%s"); !strings.Contains(msg, "Telegram") {
		t.Errorf("add 应提交：%s", msg)
	}
	db, _ := os.ReadFile(filepath.Join(demo, "docs", "BACKLOG.md"))
	di := backlog.ParseBacklog(string(db))
	if len(di) == 0 || !strings.HasPrefix(di[0].Text, "用哪种格式 —— A 或 B（用户决定：选 A）") {
		t.Errorf("decide 应移到待做顶部：%+v", di)
	}
	remain, _ := notify.ScanDecisions(e.root)
	if len(remain) != 1 || !remain[0].Waiting {
		t.Errorf("等条件的不该动：%+v", remain)
	}
	if ups := tg.uploaded(); len(ups) != 1 || ups[0].Name != "report.txt" {
		t.Errorf("send_file：%+v", ups)
	}
	var reply string
	for _, x := range tg.texts() {
		if strings.HasPrefix(x, "🟢 回答") {
			reply = x
		}
	}
	for _, want := range []string{"没有项目", "匹配到 0 条", "没有任务", "命中 deny 规则", "只能发 projects_root", "不认识的动作", "派了小任务", "已启动任务 " + next.ID} {
		if !strings.Contains(reply, want) {
			t.Errorf("回复缺 %q：\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "add other") || strings.Contains(reply, "没做：send_file "+filepath.Join(other, "report.txt")) {
		t.Errorf("做成的不该列在没做里：\n%s", reply)
	}
	night := e.nightly()
	if n := strings.Count(night, "Router 动作"); n != 17 {
		t.Errorf("每条动作都写进汇总，得到 %d 条：\n%s", n, night)
	}
	rec, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "router", "recent.jsonl"))
	var turn chat.RouterTurn
	if err := json.Unmarshal(rec, &turn); err != nil {
		t.Fatalf("滚动记录：%v %s", err, rec)
	}
	if len(turn.Actions) != 6 || len(turn.JobIDs) != 3 || !strings.Contains(turn.In, "做事") {
		t.Errorf("滚动记录：%+v", turn)
	}
	for _, id := range []string{next.ID, quick.ID} {
		e.run("stop", id)
	}
}

// 滚动记录：截断、最多 15 轮、总长 3000 字、坏行跳过、文件坏了当空的。
func TestRouterRecent(t *testing.T) {
	a, _ := newTestApp(t)
	if got := a.Chat.LoadRecent(); got != nil {
		t.Errorf("没有文件：%+v", got)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	long := strings.Repeat("字", chat.RouterFieldMax+50)
	a.Chat.AppendRecent(chat.RouterTurn{At: now, In: long, Reply: "<b>" + long + "</b>"})
	got := a.Chat.LoadRecent()
	if len(got) != 1 || len([]rune(got[0].In)) != chat.RouterFieldMax || len([]rune(got[0].Reply)) != chat.RouterFieldMax || strings.Contains(got[0].Reply, "<b>") {
		t.Errorf("字段截断：%+v", got)
	}
	for i := range 40 {
		a.Chat.AppendRecent(chat.RouterTurn{At: now, In: fmt.Sprintf("问%d", i), Reply: "答"})
	}
	got = a.Chat.LoadRecent()
	if len(got) != chat.RouterRecentMax || got[chat.RouterRecentMax-1].In != "问39" || got[0].In != "问25" {
		t.Errorf("最多 15 轮、旧的在前：%+v", got)
	}
	b, _ := os.ReadFile(a.Chat.RouterRecentPath())
	if n := strings.Count(string(b), "\n"); n != chat.RouterRecentKep {
		t.Errorf("文件只留 %d 轮：%d", chat.RouterRecentKep, n)
	}
	var big []string
	for i := range 15 {
		line, _ := json.Marshal(chat.RouterTurn{At: now, In: fmt.Sprintf("%d%s", i, strings.Repeat("长", chat.RouterFieldMax-5)), Reply: strings.Repeat("长", chat.RouterFieldMax)})
		big = append(big, string(line))
	}
	os.WriteFile(a.Chat.RouterRecentPath(), []byte(strings.Join(big, "\n")+"\n"), 0o600)
	got = a.Chat.LoadRecent()
	total := 0
	for _, t := range got {
		total += len([]rune(t.In)) + len([]rune(t.Reply))
	}
	if total > chat.RouterRecentLen || len(got) == 0 || !strings.HasPrefix(got[len(got)-1].In, "14") {
		t.Errorf("总长上限，留最新的：%d 字 %d 轮", total, len(got))
	}
	os.WriteFile(a.Chat.RouterRecentPath(), []byte("坏\n{\"in\":\"好\"}\n{坏\n"), 0o600)
	if got := a.Chat.LoadRecent(); len(got) != 1 || got[0].In != "好" {
		t.Errorf("坏行跳过：%+v", got)
	}
	os.WriteFile(a.Chat.RouterRecentPath(), []byte("全坏"), 0o600)
	if got := a.Chat.LoadRecent(); len(got) != 0 {
		t.Errorf("全坏当空的：%+v", got)
	}
	a.Chat.AppendRecent(chat.RouterTurn{At: now, In: "接着写", Reply: "答"})
	if got := a.Chat.LoadRecent(); len(got) != 1 || got[0].In != "接着写" {
		t.Errorf("坏文件后还能接着写：%+v", got)
	}
}

// Router 收尾时 stop 了比它新的任务：同一次 tick 不能再把那个任务收尾一遍（记成失败、算连环失败）。
func TestRouterStopsNewerJob(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, nil)
	testkit.MkProject(t, e.root, "demo", "- [ ] 长活 SLEEP=30\n")
	e.routerResult(routerOK("先占位"))
	tg.msg(1, ownerChat, 11, "ROUTERFILE SLEEP=2", "")
	e.mustRun("tick")
	if len(routerJobs(e)) != 1 {
		t.Fatalf("Router 应已启动：%+v", e.state().Jobs)
	}
	e.mustRun("run", "demo", "长活")
	long := e.jobByTitle("长活 SLEEP=30")
	e.routerResult(routerOK("停了", chat.RouterAction{Type: "stop", Job: long.ID}))
	waitFor(t, 20*time.Second, "Router 收尾", func() bool {
		e.mustRun("tick")
		return routerJobs(e)[0].Status != state.StatusRunning
	})
	got := e.jobByTitle("长活 SLEEP=30")
	if got.Status != state.StatusKilled || e.state().FailStreak != 0 {
		t.Errorf("被停的任务应记已停止、不算失败：%+v，连环失败 %d", got, e.state().FailStreak)
	}
	if n := strings.Count(e.nightly(), "「长活 SLEEP=30」"); n != 1 {
		t.Errorf("汇总只记一次，得到 %d：\n%s", n, e.nightly())
	}
}

// 回答太长要截断时，agentq 自己补的那句（写进了待做、没做成的动作）还在。
func TestSendReplyKeepsTail(t *testing.T) {
	e := newNotifyEnv(t, true)
	j := &Job{ID: "1104-tail", Kind: state.KindRouter}
	os.MkdirAll(e.a.JobDir(j.ID), 0o755)
	e.a.Chat.SendReply(j, strings.Repeat("长", telegram.TgMaxRunes+100), "没做：add demo", nil)
	got := e.tg.sentLast().Get("text")
	if !strings.Contains(got, "全文见文件") || !strings.HasSuffix(got, "没做：add demo") || len([]rune(got)) > chat.ReplyMaxRunes+50 {
		t.Errorf("%d 字：%s", len([]rune(got)), got[len(got)-80:])
	}
}

// 真实 claude -p 验证注入和 schema 走得通：AGENTQ_ROUTER_REAL=1 才跑，会花额度。临时 HOME 下没有登录，所以 claude 用真实身份，
// 工作目录、--add-dir、项目和消息都是临时目录里的假数据，不碰真实 BACKLOG。
func TestRouterReal(t *testing.T) {
	if os.Getenv("AGENTQ_ROUTER_REAL") != "1" {
		t.Skip("设 AGENTQ_ROUTER_REAL=1 才跑")
	}
	a, _ := newTestApp(t)
	a.Cfg.ProjectsRoot = filepath.Join(a.Home, "projects")
	dir := testkit.MkProject(t, a.Cfg.ProjectsRoot, "demo", "- [ ] 已有条目 —— 正文\n")
	if err := os.WriteFile(filepath.Join(dir, "PROGRESS.md"), []byte("# 进度\n当前版本号是 7.3.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(a.DataDir(), "jobs"), filepath.Join(a.Home, ".claude", "nightly")} {
		os.MkdirAll(d, 0o755)
	}
	a.Chat.AppendRecent(chat.RouterTurn{At: time.Now(), In: "demo 项目叫什么", Reply: "叫 demo"})
	inbox := []notify.InMsg{{ID: 1, At: time.Now(), Text: "demo 的当前版本号是多少？另外给 demo 加一条待做：把 README 翻成英文。"}}
	prompt := a.Chat.RouterPrompt(&State{}, inbox, time.Now())
	session, err := backend.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	claude := os.Getenv("AGENTQ_ROUTER_CLAUDE")
	if claude == "" {
		claude = "claude"
	}
	cmd := exec.Command(claude, testkit.ClaudeArgv(t, chat.RouterRequest(cfg, a.Home, prompt, session))...)
	cmd.Dir = a.Cfg.ProjectsRoot
	cmd.Env = proc.ChildEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("claude：%v\n%s", err, out)
	}
	var res struct {
		claudecli.Result
		Cost float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("结果看不懂：%v\n%s", err, out)
	}
	t.Logf("subtype=%s is_error=%v 回合=%d 费用=$%.4f", res.Subtype, res.IsError, res.NumTurns, res.Cost)
	t.Logf("structured_output=%s", res.StructuredOutput)
	var so struct {
		Reply   string              `json:"reply"`
		Actions []chat.RouterAction `json:"actions"`
	}
	if err := json.Unmarshal(res.StructuredOutput, &so); err != nil || so.Reply == "" {
		t.Fatalf("应有结构化输出：%v\n%s", err, out)
	}
	if !strings.Contains(so.Reply, "7.3.1") {
		t.Errorf("应读到假项目里的版本号：%s", so.Reply)
	}
	hasAdd := false
	for _, x := range so.Actions {
		if x.Type == "add" && x.Project == "demo" {
			hasAdd = true
		}
	}
	if !hasAdd {
		t.Errorf("应提 add 动作：%+v", so.Actions)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md")); strings.Contains(string(b), "英文") {
		t.Errorf("Router 自己不该写文件：%s", b)
	}
}
