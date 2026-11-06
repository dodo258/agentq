package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/chat"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func replyJob(e *notifyEnv, id string) *Job {
	e.t.Helper()
	j := &Job{ID: id, Kind: state.KindChat}
	if err := os.MkdirAll(e.a.JobDir(id), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.a.JobDir(id), "reply-to"), []byte("77\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// 超过 600 字：全文落文件、真发出去、消息只留前部；标签和实体完整。回复链照常登记。
func TestSendReplyCap(t *testing.T) {
	e := newNotifyEnv(t, true)
	j := replyJob(e, "1104-long")
	long := "📊 <b>状态</b>\n\n" + strings.Repeat("<b>长&amp;</b>", 400)
	e.a.Chat.SendReply(j, long, "已启动任务 x", &notify.Thread{Kind: chat.ThreadChat, JobID: j.ID})
	got := e.tg.sentLast().Get("text")
	if v := chat.HtmlVisible(got); v > chat.ReplyMaxRunes+len([]rune("已启动任务 x"))+1 {
		t.Errorf("超过上限：%d", v)
	}
	if !strings.Contains(got, "全文见文件 ~/.agentq/replies/1104-long.md") || !strings.HasSuffix(got, "已启动任务 x") {
		t.Errorf("%s", got)
	}
	if strings.Count(got, "<b>") != strings.Count(got, "</b>") {
		t.Errorf("标签要配对：%s", got)
	}
	for _, bad := range []string{"&am\n", "&amp\n", "<b\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("切坏了：%s", got)
		}
	}
	if !strings.Contains(e.tg.sentLast().Get("reply_parameters"), `"message_id":77`) {
		t.Errorf("挂回复：%v", e.tg.sentLast())
	}
	path := e.a.Chat.ReplyFilePath(j.ID)
	b, err := os.ReadFile(path)
	if err != nil || strings.Count(string(b), "长&") != 400 || !strings.HasSuffix(string(b), "已启动任务 x\n") || strings.Contains(string(b), "<b>") {
		t.Errorf("全文：%v %d", err, len(b))
	}
	ups := e.tg.uploaded()
	if len(ups) != 1 || ups[0].Method != "sendDocument" || ups[0].Name != "1104-long.md" || !strings.Contains(ups[0].Form.Get("reply_parameters"), `"message_id":77`) || string(ups[0].Data) != string(b) {
		t.Errorf("文件要真发出去：%+v", ups)
	}
	s, _ := e.a.Notify.LoadNotify()
	if len(s.Threads) != 1 || len(s.Replies) != 0 {
		t.Errorf("%+v", s)
	}
}

// 不超过：逐字不变，不写文件，不上传。600 字整是边界。
func TestSendReplyUnderCap(t *testing.T) {
	e := newNotifyEnv(t, true)
	for i, text := range []string{"🟢 <b>已派出</b>", strings.Repeat("字", chat.ReplyMaxRunes), "<b>" + strings.Repeat("&lt;", chat.ReplyMaxRunes) + "</b>"} {
		id := "1104-s" + string(rune('a'+i))
		e.a.Chat.SendReply(replyJob(e, id), text, "", nil)
		if got := e.tg.sentLast().Get("text"); got != text {
			t.Errorf("逐字不变：%q", got)
		}
		if _, err := os.Stat(e.a.Chat.ReplyFilePath(id)); err == nil {
			t.Error("不该写文件")
		}
	}
	if ups := e.tg.uploaded(); len(ups) != 0 {
		t.Errorf("%+v", ups)
	}
	id := "1104-over"
	e.a.Chat.SendReply(replyJob(e, id), strings.Repeat("字", chat.ReplyMaxRunes+1), "", nil)
	if _, err := os.Stat(e.a.Chat.ReplyFilePath(id)); err != nil {
		t.Errorf("601 字要落文件：%v", err)
	}
}

// 文件发不出去：文字照发，路径写在里面，文件还在；下次 tick 重发文件，不重发文字。
func TestSendReplyFileFails(t *testing.T) {
	e := newNotifyEnv(t, true)
	j := replyJob(e, "1104-upfail")
	e.tg.failUpload = 1
	if err := e.a.Notify.WithNotify(func(s *notify.State) error {
		e.a.Chat.SendReply(j, strings.Repeat("字", 700), "", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if e.tg.count() != 1 || !strings.Contains(e.tg.sentLast().Get("text"), "全文见文件 ~/.agentq/replies/1104-upfail.md") {
		t.Fatalf("%q", e.tg.texts())
	}
	if _, err := os.Stat(e.a.Chat.ReplyFilePath(j.ID)); err != nil {
		t.Error("文件要在")
	}
	if len(e.tg.uploaded()) != 0 {
		t.Fatal("这次没传上")
	}
	if s := e.state(); len(s.Replies) != 1 || s.Replies[0].Text != "" || s.Replies[0].File == "" {
		t.Fatalf("要存下重发：%+v", s.Replies)
	}
	// 还是失败：留着
	e.tg.failUpload = 1
	e.flush()
	if s := e.state(); len(s.Replies) != 1 || e.tg.count() != 1 {
		t.Errorf("%+v %q", s.Replies, e.tg.texts())
	}
	e.flush()
	if s := e.state(); len(s.Replies) != 0 || len(e.tg.uploaded()) != 1 || e.tg.count() != 1 {
		t.Errorf("补发文件，不重发文字：%+v %q", s.Replies, e.tg.texts())
	}
}

// 文字也发不出去：文字和文件一起存下，下次都发，回复链补登记。
func TestSendReplyTextFails(t *testing.T) {
	e := newNotifyEnv(t, true)
	j := replyJob(e, "1104-txtfail")
	e.tg.failSend = 1
	th := &notify.Thread{Kind: chat.ThreadChat, JobID: j.ID}
	e.a.Chat.SendReply(j, strings.Repeat("字", 700), "", th)
	if s := e.state(); len(s.Replies) != 1 || s.Replies[0].Text == "" || s.Replies[0].File == "" {
		t.Fatalf("%+v", s.Replies)
	}
	if len(e.tg.uploaded()) != 0 {
		t.Fatal("文字到了才发文件")
	}
	e.flush()
	if s := e.state(); len(s.Replies) != 0 || len(s.Threads) != 1 || e.tg.count() != 1 || len(e.tg.uploaded()) != 1 {
		t.Errorf("%+v %q %d", s, e.tg.texts(), len(e.tg.uploaded()))
	}
}

// 全文写不了：照样发前部，告诉用户原文去哪看，不发文件，不重试。
func TestSendReplySaveFails(t *testing.T) {
	e := newNotifyEnv(t, true)
	j := replyJob(e, "1104-nosave")
	if err := os.WriteFile(filepath.Join(e.a.DataDir(), "replies"), []byte("占着"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.Chat.SendReply(j, strings.Repeat("字", 700), "", nil)
	got := e.tg.sentLast().Get("text")
	if !strings.Contains(got, "全文没存下，原文看 agentq logs 1104-nosave") || strings.Contains(got, "全文见文件") {
		t.Errorf("%s", got)
	}
	if s := e.state(); len(s.Replies) != 0 || len(e.tg.uploaded()) != 0 {
		t.Errorf("%+v", s.Replies)
	}
}

// 上限只管对话类回答：巡逻推送、早报不走 sendReply，长的也不落 replies。
func TestReplyCapOnlyReplies(t *testing.T) {
	e := newNotifyEnv(t, true)
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Key: "x", Text: strings.Repeat("长", 800)})
	e.flush()
	if got := e.tg.sentLast().Get("text"); !strings.Contains(got, strings.Repeat("长", 800)) {
		t.Errorf("推送不截：%d", len(got))
	}
	if _, err := os.Stat(filepath.Join(e.a.DataDir(), "replies")); err == nil {
		t.Error("不该有 replies 目录")
	}
	// 早报摘要
	day := e.now.Format("2006-01-02")
	report := filepath.Join(e.a.Home, ".claude", "nightly", day+".md")
	e.write(report, strings.Repeat("早", 50)+"\n"+strings.Repeat("报", 2000)+"\n")
	os.Chtimes(report, e.now.Add(-notify.MorningStable*2), e.now.Add(-notify.MorningStable*2))
	e.flush()
	if got := strings.Join(e.tg.texts(), "\n"); strings.Contains(got, "全文见文件") {
		t.Errorf("%s", got)
	}
}

// Router、对话：端到端走真进程。超长的回答落文件并发出去，动作照做，滚动记录和全量记录都在。
func TestRouterLongReplyAndHistory(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, nil)
	testkit.MkProject(t, e.root, "demo", "")
	long := "<b>头</b>\n" + strings.Repeat("内容很长&amp;", 200)
	e.routerResult(routerOK(long))
	j := e.sendToRouter(tg, 1, 11, "ROUTERFILE 第一问")
	got := tg.sentLast().Get("text")
	if !strings.Contains(got, "全文见文件 ~/.agentq/replies/"+j.ID+".md") || chat.HtmlVisible(got) > chat.ReplyMaxRunes {
		t.Errorf("%d：%s", chat.HtmlVisible(got), got)
	}
	if ups := tg.uploaded(); len(ups) != 1 || ups[0].Name != j.ID+".md" {
		t.Errorf("%+v", ups)
	}
	raw, err := os.ReadFile(filepath.Join(e.home, ".agentq", "router", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var turn chat.RouterTurn
	if err := json.Unmarshal(raw, &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Job != j.ID || turn.In != "ROUTERFILE 第一问" || strings.Count(turn.Reply, "内容很长&") != 200 {
		t.Errorf("全量记录不截断：%+v", turn)
	}
	recent, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "router", "recent.jsonl"))
	var short chat.RouterTurn
	if err := json.Unmarshal(recent, &short); err != nil || len([]rune(short.Reply)) > chat.RouterFieldMax {
		t.Errorf("滚动记录还是截断的：%v %d", err, len([]rune(short.Reply)))
	}
	// 下一轮：提示词告诉它去哪查，--add-dir 放行
	e.routerResult(routerOK("好"))
	j2 := e.sendToRouter(tg, 2, 12, "ROUTERFILE 第二问")
	log := e.fakeLogOf(j2)
	for _, want := range []string{"Grep 它", filepath.Join(e.home, ".agentq", "router", "history.jsonl"), "meta.json", "ARG[--add-dir]\nARG[" + filepath.Join(e.home, ".agentq", "router") + "]"} {
		if !strings.Contains(log, want) {
			t.Errorf("缺 %q：\n%s", want, log)
		}
	}
	raw, _ = os.ReadFile(filepath.Join(e.home, ".agentq", "router", "history.jsonl"))
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Errorf("只追加：%d 行", n)
	}
}

// 全量记录内联 job_ids，下一轮的提示词也带上（跳过的动作不记）；写不了不影响回答。
func TestRouterHistoryJobIDs(t *testing.T) {
	t.Parallel()
	e, tg := routerEnv(t, nil)
	gitProject(t, e, "demo", "- [ ] 甲 —— SLEEP=30\n")
	e.routerResult(routerOK("派了", chat.RouterAction{Type: "run", Project: "demo"}))
	e.sendToRouter(tg, 1, 11, "ROUTERFILE 做一下")
	raw, err := os.ReadFile(filepath.Join(e.home, ".agentq", "router", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var turn chat.RouterTurn
	if err := json.Unmarshal(raw, &turn); err != nil {
		t.Fatal(err)
	}
	task := e.jobByTitle("甲")
	if len(turn.JobIDs) != 1 || turn.JobIDs[0] != task.ID || len(turn.Actions) != 1 {
		t.Fatalf("%+v", turn)
	}
	e.routerResult(routerOK("好"))
	j2 := e.sendToRouter(tg, 2, 12, "ROUTERFILE 下一问")
	if log := e.fakeLogOf(j2); !strings.Contains(log, "任务："+task.ID) {
		t.Errorf("提示词要内联任务 id：\n%s", log)
	}
	e.mustRun("stop", task.ID)
}
