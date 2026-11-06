package tests

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/testkit"
)

// cdEnv：一个带 Telegram 的项目，已经过了第一次扫描（第一次只记下、不推）。
func cdEnv(t *testing.T, decide string) (*notifyEnv, backlog.Project) {
	t.Helper()
	e := newNotifyEnv(t, true)
	os.WriteFile(filepath.Join(e.a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	p := sgRepo(t, e.a, e.root, "demo", "# 待办\n\n## 待做\n\n- [ ] 已有\n\n## 等用户决定\n\n"+decide+"\n## 已完成\n")
	return e, p
}

func addDecision(t *testing.T, e *notifyEnv, p backlog.Project, line string) {
	t.Helper()
	b, _ := os.ReadFile(p.BacklogPath())
	text := strings.Replace(string(b), "## 已完成", line+"\n## 已完成", 1)
	if err := os.WriteFile(p.BacklogPath(), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.a.Home, p.Dir, "add", "-A")
	testkit.GitT(t, e.a.Home, p.Dir, "commit", "-q", "-m", "add")
}

type inlineButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

func keyboardOf(t *testing.T, form url.Values) []inlineButton {
	t.Helper()
	var m struct {
		Rows [][]inlineButton `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(form.Get("reply_markup")), &m); err != nil {
		t.Fatalf("键盘看不懂：%v %q", err, form.Get("reply_markup"))
	}
	var out []inlineButton
	for _, r := range m.Rows {
		out = append(out, r...)
	}
	return out
}

func (f *fakeTG) click(updateID int, from int64, msgID int, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "callback_query": map[string]any{
		"id": "cb", "from": map[string]any{"id": from, "is_bot": false}, "data": data, "chat_instance": "x",
		"message": map[string]any{"message_id": msgID, "date": time.Now().Unix(), "chat": map[string]any{"id": ownerChat, "type": "private"}},
	}})
}

func (f *fakeTG) lastAnswer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return "<没回应>"
	}
	return f.answers[len(f.answers)-1].Get("text")
}

// 带固定格式的推按钮：每个选项一个，推荐的标出来，data 不超过 64 字节，映射存盘；回复链照样登记。
func TestCardPush(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明一下（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	if e.tg.count() != 1 {
		t.Fatalf("%q", e.tg.texts())
	}
	form := e.tg.sentLast()
	btns := keyboardOf(t, form)
	if len(btns) != 2 || !strings.Contains(btns[0].Text, "A 用标准库") || !strings.Contains(btns[0].Text, "推荐") || strings.Contains(btns[1].Text, "推荐") {
		t.Fatalf("%+v", btns)
	}
	for _, b := range btns {
		if len(b.Data) == 0 || len(b.Data) > 64 {
			t.Errorf("callback_data 长度 %d", len(b.Data))
		}
	}
	text := form.Get("text")
	if !strings.Contains(text, "用哪个库") || !strings.Contains(text, "说明一下") || !strings.Contains(text, "A 用标准库（推荐）") || strings.Contains(text, "选项：") {
		t.Errorf("%s", text)
	}
	s := e.state()
	if len(s.Cards) != 1 || len(s.Pending) != 0 || len(s.Threads) != 1 {
		t.Errorf("%+v", s)
	}
	// 不重复推
	e.flush()
	if e.tg.count() != 1 {
		t.Errorf("%q", e.tg.texts())
	}
}

// 没写格式或写错的：不带按钮，文字和原来逐字一样。
func TestCardFallsBackToText(t *testing.T) {
	for _, line := range []string{
		"- [ ] 用哪个库 —— 说明一下",
		"- [ ] 用哪个库 —— 说明一下（选项：A 只有一个；推荐 A）",
	} {
		e, p := cdEnv(t, "")
		e.flush()
		addDecision(t, e, p, line)
		e.flush()
		form := e.tg.sentLast()
		if form.Get("reply_markup") != "" {
			t.Errorf("%q 不该带按钮：%s", line, form.Get("reply_markup"))
		}
		text := form.Get("text")
		if !strings.Contains(text, "说明一下") || !strings.Contains(text, "回复这条消息说选哪个，就按你的决定接着做") {
			t.Errorf("%s", text)
		}
		if len(e.state().Cards) != 0 {
			t.Errorf("%+v", e.state().Cards)
		}
	}
}

// 点按钮：写进 BACKLOG 并提交（和 decide 同一条路）、消息改成结论、不起模型；重复点只生效一次。
func TestCardClick(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	btns := keyboardOf(t, e.tg.sentLast())
	before := commits(t, e.a, p)
	e.tg.click(1, ownerChat, 1001, btns[1].Data)
	e.flush()
	got := backlogOf(t, p)
	if !strings.Contains(got, "- [ ] 用哪个库 —— 说明（用户决定：B 用第三方）\n- [ ] 已有") {
		t.Fatalf("%s", got)
	}
	if commits(t, e.a, p) != before+1 {
		t.Error("要提交一次")
	}
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已定") || !strings.Contains(a, "B 用第三方") {
		t.Errorf("%s", a)
	}
	if len(e.tg.edits) != 1 {
		t.Fatalf("%v", e.tg.edits)
	}
	edit := e.tg.edits[0]
	if edit.Get("message_id") != "1001" || !strings.Contains(edit.Get("text"), "已定") || !strings.Contains(edit.Get("text"), "选了 B 用第三方") || edit.Get("reply_markup") != `{"inline_keyboard":[]}` {
		t.Errorf("%v", edit)
	}
	if len(e.chats) != 0 || len(e.tg.acks()) != 0 {
		t.Errorf("按钮不起模型：%v %q", e.chats, e.tg.acks())
	}
	if !strings.Contains(e.out.String(), "用户点按钮定了") {
		t.Errorf("%s", e.out.String())
	}
	// 重复点、点另一个：都只生效一次
	e.tg.click(2, ownerChat, 1001, btns[1].Data)
	e.tg.click(3, ownerChat, 1001, btns[0].Data)
	e.flush()
	if commits(t, e.a, p) != before+1 || strings.Contains(backlogOf(t, p), "用户决定：A") {
		t.Errorf("%s", backlogOf(t, p))
	}
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已经定了：B 用第三方") {
		t.Errorf("%s", a)
	}
	if len(e.tg.edits) != 1 {
		t.Errorf("%v", e.tg.edits)
	}
}

// 不是用户、过期、乱写的 data、指纹变了、条目没了：都要回应，都不生效。
func TestCardRefused(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	data := keyboardOf(t, e.tg.sentLast())[0].Data
	unchanged := func(what string) {
		t.Helper()
		if strings.Contains(backlogOf(t, p), "（用户决定") {
			t.Errorf("%s：不该生效：%s", what, backlogOf(t, p))
		}
		if len(e.tg.edits) != 0 {
			t.Errorf("%s：不该改消息", what)
		}
	}
	upd := 1
	click := func(from int64, d string) {
		t.Helper()
		e.tg.click(upd, from, 1001, d)
		upd++
		e.flush()
	}
	click(ownerChat+1, data)
	unchanged("不是用户")
	if a := e.tg.lastAnswer(); !strings.Contains(a, "只有配好的账号") {
		t.Errorf("%s", a)
	}
	for _, bad := range []string{"", "dc:", "dc:zzz:A", "dc:" + strings.Repeat("0", 12) + ":A", data[:len(data)-1] + "Z", "发", "release:go"} {
		click(ownerChat, bad)
		unchanged("data " + bad)
		if a := e.tg.lastAnswer(); a != notify.CardExpired {
			t.Errorf("%q → %s", bad, a)
		}
	}
	// 条目改过了（选项变了，标题没变）
	b, _ := os.ReadFile(p.BacklogPath())
	changed := strings.Replace(string(b), "B 用第三方", "B 用别的", 1)
	os.WriteFile(p.BacklogPath(), []byte(changed), 0o644)
	testkit.GitT(t, e.a.Home, p.Dir, "add", "-A")
	testkit.GitT(t, e.a.Home, p.Dir, "commit", "-q", "-m", "change")
	click(ownerChat, data)
	unchanged("改过了")
	if a := e.tg.lastAnswer(); a != notify.CardChanged {
		t.Errorf("%s", a)
	}
	// 条目没了
	os.WriteFile(p.BacklogPath(), b, 0o644)
	removed := strings.Replace(string(b), "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）\n", "", 1)
	os.WriteFile(p.BacklogPath(), []byte(removed), 0o644)
	click(ownerChat, data)
	if a := e.tg.lastAnswer(); a != notify.CardChanged {
		t.Errorf("%s", a)
	}
	// 过期（14 天）
	os.WriteFile(p.BacklogPath(), b, 0o644)
	e.now = e.now.Add(notify.CardKeep + time.Hour)
	click(ownerChat, data)
	unchanged("过期")
	if a := e.tg.lastAnswer(); a != notify.CardExpired {
		t.Errorf("%s", a)
	}
	if id := notify.CardDataRe.FindStringSubmatch(data)[1]; e.state().Cards[id] != nil {
		t.Errorf("过期的要清掉：%+v", e.state().Cards)
	}
}

// 主目录脏：没定成就不算定了，能再点。
func TestCardCommitFails(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	data := keyboardOf(t, e.tg.sentLast())[0].Data
	b, _ := os.ReadFile(p.BacklogPath())
	dirty := string(b) + "\n用户正在改\n"
	os.WriteFile(p.BacklogPath(), []byte(dirty), 0o644)
	e.tg.click(1, ownerChat, 1001, data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "没定成") {
		t.Errorf("%s", a)
	}
	if backlogOf(t, p) != dirty || len(e.tg.edits) != 0 {
		t.Errorf("%s", backlogOf(t, p))
	}
	os.WriteFile(p.BacklogPath(), b, 0o644)
	e.tg.click(2, ownerChat, 1001, data)
	e.flush()
	if !strings.Contains(backlogOf(t, p), "用户决定：A 用标准库") {
		t.Errorf("%s", backlogOf(t, p))
	}
}

// Telegram 接口失败：已生效的决定不回滚，只记日志，不重试。
func TestCardTelegramFails(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	data := keyboardOf(t, e.tg.sentLast())[0].Data
	e.tg.failEdit, e.tg.failAnswer = true, true
	e.tg.click(1, ownerChat, 1001, data)
	e.flush()
	if !strings.Contains(backlogOf(t, p), "用户决定：A 用标准库") {
		t.Errorf("%s", backlogOf(t, p))
	}
	log := e.out.String()
	if !strings.Contains(log, "消息没改成结论") || !strings.Contains(log, "回应按钮失败") || strings.Contains(log, fakeToken) {
		t.Errorf("%s", log)
	}
	if s := e.state(); s.UpdateOffset != 2 {
		t.Errorf("offset 要往前走，不重放：%d", s.UpdateOffset)
	}
}

// 发卡片失败留在队列里，下次再发；卡片不合并。
func TestCardSendRetryAndSeparate(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 第一件（选项：A 一｜B 二；推荐 A）\n- [ ] 第二件（选项：A 三｜B 四；推荐 B）\n- [ ] 第三件 —— 没按钮")
	e.tg.failSend = 1 // 第一张卡片发失败：两张都留着，没按钮的那条照发
	e.flush()
	if s := e.state(); len(s.Pending) != 2 || len(s.Cards) != 0 {
		t.Fatalf("%+v", s)
	}
	e.now = e.now.Add(notify.NotifyMergeWindow + time.Minute)
	e.flush()
	s := e.state()
	if len(s.Pending) != 0 || len(s.Cards) != 2 {
		t.Fatalf("%+v", s)
	}
	texts := e.tg.texts()
	if len(texts) != 3 {
		t.Fatalf("两张卡片各一条、没按钮的一条：%q", texts)
	}
	if !strings.Contains(texts[0], "第三件") || !strings.Contains(texts[1], "第一件") || strings.Contains(texts[1], "第二件") || !strings.Contains(texts[2], "第二件") {
		t.Errorf("%q", texts)
	}
	e.tg.mu.Lock()
	plain := e.tg.sent[0]
	e.tg.mu.Unlock()
	if plain.Get("reply_markup") != "" {
		t.Errorf("%v", plain)
	}
}

// 夜里攒的卡片：早报只列标题，按钮接着单独发，不丢。
func TestCardSurvivesMorning(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	e.now = time.Date(2026, 10, 12, 2, 0, 0, 0, time.Local)
	addDecision(t, e, p, "- [ ] 用哪个库（选项：A 一｜B 二；推荐 A）")
	e.flush()
	if e.tg.count() != 0 || len(e.state().Pending) != 1 {
		t.Fatalf("夜里只攒着：%q", e.tg.texts())
	}
	e.now = time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	report := filepath.Join(e.a.Home, ".claude", "nightly", "2026-10-12.md")
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("# 早报\n"), 0o644)
	old := e.now.Add(-time.Hour)
	os.Chtimes(report, old, old)
	e.flush()
	texts := e.tg.texts()
	if len(texts) != 2 || !strings.Contains(texts[0], "早报") {
		t.Fatalf("%q", texts)
	}
	if btns := keyboardOf(t, e.tg.sentLast()); len(btns) != 2 {
		t.Errorf("%+v", btns)
	}
}

// getUpdates 要带 callback_query，原来的 message 还在。
func TestAllowedUpdates(t *testing.T) {
	e, _ := cdEnv(t, "")
	e.flush()
	if len(e.tg.allowed) == 0 || e.tg.allowed[0] != `["message","callback_query"]` {
		t.Errorf("%v", e.tg.allowed)
	}
}

// 对发版提示消息点假按钮提交不了（提示本身不带按钮见 TestReleaseFlow 那边的断言）。
func TestReleaseOfferIgnoresButtons(t *testing.T) {
	r := newRelEnv(t, nil)
	r.seedOffered(777, nil)
	r.tg.click(50, ownerChat, 777, "发")
	r.tg.click(51, ownerChat, 777, "dc:000000000000:A")
	r.mustRun("tick")
	r.noWrites()
	if a := r.tg.lastAnswer(); a != notify.CardExpired {
		t.Errorf("%s", a)
	}
}

// notify.json 写坏成 "cards":{"x":null} 也不能崩：点按钮照常回应。
func TestCardNullInNotifyJSON(t *testing.T) {
	e, p := cdEnv(t, "")
	e.flush()
	addDecision(t, e, p, "- [ ] 用哪个库 —— 说明（选项：A 用标准库｜B 用第三方；推荐 A）")
	e.flush()
	data := keyboardOf(t, e.tg.sentLast())[0].Data
	raw, err := os.ReadFile(e.a.Notify.NotifyPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["cards"].(map[string]any)["x"] = nil
	b, _ := json.Marshal(m)
	if err := os.WriteFile(e.a.Notify.NotifyPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.tg.click(1, ownerChat, 1001, "dc:"+strings.Repeat("0", 12)+":A") // 没有的 id
	e.flush()
	if a := e.tg.lastAnswer(); a != notify.CardExpired {
		t.Errorf("%s", a)
	}
	e.tg.click(2, ownerChat, 1001, data) // 好的卡片不受影响
	e.flush()
	if !strings.Contains(backlogOf(t, p), "用户决定：A 用标准库") {
		t.Errorf("%s", backlogOf(t, p))
	}
	if strings.Contains(e.out.String(), "跳过这批") {
		t.Errorf("不该 panic：%s", e.out.String())
	}
	if e.state().Cards["x"] != nil {
		t.Error("空的要清掉")
	}
}

func TestCardMarkupSize(t *testing.T) {
	card := &notify.DecideCard{ID: "abcdef012345", Recommend: "A", Options: []notify.DecideOption{{Label: "A", Text: strings.Repeat("长", 300)}, {Label: "B", Text: "短"}}}
	raw, err := notify.CardMarkup(card)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Rows [][]inlineButton `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if len([]rune(m.Rows[0][0].Text)) > notify.CardButtonText+10 {
		t.Errorf("按钮太长：%s", m.Rows[0][0].Text)
	}
	if !notify.CardDataRe.MatchString(m.Rows[0][0].Data) || len(m.Rows[0][0].Data) > 64 {
		t.Errorf("%s", m.Rows[0][0].Data)
	}
}
