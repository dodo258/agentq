package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/patrol"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// 推送和 Telegram 的单元测试：进程内跑，Telegram 换成本地假服务，本机通知换成记录函数，HOME 和 projects_root 是临时目录。

const (
	fakeToken = "123456:FAKE-token_abc"
	ownerChat = int64(777)
)

// fakeTG 是假的 Telegram Bot API：记下 sendMessage 和上传的文件，getUpdates 按 offset 返回排好的更新
// （timeout > 0 时像真服务一样挂着，等到有新更新或到时间才返回），getFile 和文件下载用 files 里的内容。
type fakeTG struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	sent       []url.Values
	uploads    []fakeUpload
	offsets    []string
	timeouts   []string
	polling    int  // 正挂着的 getUpdates 个数
	closed     bool // 测试结束
	updates    []map[string]any
	files      map[string][]byte // file_id → 内容
	failSend   int               // 接下来这么多次 sendMessage 返回 500
	rejectHTML bool              // 带 parse_mode=HTML 的 sendMessage 一律 400 can't parse entities
	rejectPic  bool              // sendPhoto、sendVideo 一律 400（像截图太长、宽高比超过 20）
	errDesc    string            // 非空时每次调用都返回 ok:false，描述是它
	failUpload int               // 接下来这么多次 sendDocument 返回 500
	answers    []url.Values      // answerCallbackQuery
	edits      []url.Values      // editMessageText
	failAnswer bool              // answerCallbackQuery 返回 500
	failEdit   bool              // editMessageText 返回 400
	allowed    []string          // getUpdates 的 allowed_updates
}

type fakeUpload struct {
	Method, Field, Name string
	Form                url.Values
	Data                []byte
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		f.closed = true // 挂着的长轮询马上返回，Close 不用等满 timeout
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func (f *fakeTG) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(80 << 20)
	} else {
		_ = r.ParseForm()
	}
	reply := func(code int, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+fakeToken+"/files/"); ok {
		if b, ok := f.files[id]; ok {
			w.Write(b)
			return
		}
		w.WriteHeader(404)
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+fakeToken+"/")
	if !ok {
		reply(404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
		return
	}
	if f.errDesc != "" {
		reply(401, map[string]any{"ok": false, "error_code": 401, "description": f.errDesc})
		return
	}
	switch method {
	case "sendMessage":
		if f.failSend > 0 {
			f.failSend--
			reply(500, map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
			return
		}
		if f.rejectHTML && r.Form.Get("parse_mode") == "HTML" {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities: Unsupported start tag"})
			return
		}
		f.sent = append(f.sent, r.Form)
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": 1000 + len(f.sent)}})
	case "sendPhoto", "sendVideo", "sendDocument":
		field := map[string]string{"sendPhoto": "photo", "sendVideo": "video", "sendDocument": "document"}[method]
		if f.rejectPic && method != "sendDocument" {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: PHOTO_INVALID_DIMENSIONS"})
			return
		}
		if method == "sendDocument" && f.failUpload > 0 {
			f.failUpload--
			reply(500, map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
			return
		}
		up := fakeUpload{Method: method, Field: field, Form: r.Form}
		if r.MultipartForm != nil {
			if fhs := r.MultipartForm.File[field]; len(fhs) == 1 {
				up.Name = fhs[0].Filename
				if fh, err := fhs[0].Open(); err == nil {
					var buf bytes.Buffer
					buf.ReadFrom(fh)
					fh.Close()
					up.Data = buf.Bytes()
				}
			}
		}
		f.uploads = append(f.uploads, up)
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": 2000 + len(f.uploads)}})
	case "answerCallbackQuery":
		if f.failAnswer {
			reply(500, map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
			return
		}
		f.answers = append(f.answers, r.Form)
		reply(200, map[string]any{"ok": true, "result": true})
	case "editMessageText":
		if f.failEdit {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be edited"})
			return
		}
		f.edits = append(f.edits, r.Form)
		reply(200, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	case "getFile":
		id := r.Form.Get("file_id")
		b, ok := f.files[id]
		if !ok {
			reply(400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: invalid file_id"})
			return
		}
		reply(200, map[string]any{"ok": true, "result": map[string]any{"file_id": id, "file_size": len(b), "file_path": "files/" + id}})
	case "getUpdates":
		f.offsets = append(f.offsets, r.Form.Get("offset"))
		f.timeouts = append(f.timeouts, r.Form.Get("timeout"))
		f.allowed = append(f.allowed, r.Form.Get("allowed_updates"))
		var off int64
		fmt.Sscan(r.Form.Get("offset"), &off)
		var wait int
		fmt.Sscan(r.Form.Get("timeout"), &wait)
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		var out []map[string]any
		for {
			for _, u := range f.updates {
				if id := int64(u["update_id"].(int)); id >= off {
					out = append(out, u)
				}
			}
			if out != nil || !time.Now().Before(deadline) || r.Context().Err() != nil || f.closed {
				break // 有新更新、到时间、对方断开（listen 被停掉）、测试结束
			}
			f.polling++
			f.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			f.mu.Lock()
			f.polling--
		}
		if out == nil {
			out = []map[string]any{}
		}
		reply(200, map[string]any{"ok": true, "result": out})
	default:
		reply(404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found: method " + method})
	}
}

// msg 排一条更新：chat 是发消息的 chat id，replyTo 非空时是回复 agentq 之前那条消息。
func (f *fakeTG) msg(updateID int, chat int64, msgID int, text, replyTo string) {
	f.msgAt(updateID, chat, msgID, text, replyTo, fakeMsgDate())
}

// fakeMsgDate 是假消息的发送时间。不少用例把时钟定在当天中午（atNoon、runawayEnv，再往后拨一两小时），
// 上午跑时真实时间比假时钟早好几小时，第一次收消息会被当成太旧的消息跳过：用例的结果不能看跑的时候是几点。
func fakeMsgDate() time.Time {
	now := time.Now()
	y, m, d := now.Date()
	if afternoon := time.Date(y, m, d, 14, 0, 0, 0, time.Local); now.Before(afternoon) {
		return afternoon
	}
	return now
}

func (f *fakeTG) msgAt(updateID int, chat int64, msgID int, text, replyTo string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]any{"message_id": msgID, "date": at.Unix(), "text": text, "chat": map[string]any{"id": chat, "type": "private"},
		"from": map[string]any{"id": chat, "is_bot": false}}
	if replyTo != "" {
		m["reply_to_message"] = map[string]any{"message_id": 5, "date": time.Now().Unix(), "text": replyTo, "chat": map[string]any{"id": chat}}
	}
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "message": m})
}

// media 排一条带附件的更新：kind 是 photo/video/document/voice，size 是消息里报的 file_size（0 就用内容长度），
// data 非空时登记成可下载的文件（file_id 为 f<更新号>）。
func (f *fakeTG) media(updateID int, msgID int, kind, caption, name string, data []byte, size int, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size == 0 {
		size = len(data)
	}
	id := fmt.Sprintf("f%d", updateID)
	if data != nil {
		if f.files == nil {
			f.files = map[string][]byte{}
		}
		f.files[id] = data
	}
	file := map[string]any{"file_id": id, "file_unique_id": "u" + id, "file_size": size}
	if name != "" {
		file["file_name"] = name
	}
	m := map[string]any{"message_id": msgID, "date": fakeMsgDate().Unix(), "chat": map[string]any{"id": ownerChat, "type": "private"}}
	if caption != "" {
		m["caption"] = caption
	}
	if group != "" {
		m["media_group_id"] = group
	}
	if kind == "photo" {
		small := map[string]any{"file_id": "small", "file_unique_id": "s", "file_size": 10, "width": 90, "height": 90}
		file["width"], file["height"] = 1280, 960
		m["photo"] = []any{small, file}
	} else {
		m[kind] = file
	}
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "message": m})
}

func (f *fakeTG) uploaded() []fakeUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeUpload(nil), f.uploads...)
}

func (f *fakeTG) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, v := range f.sent {
		out = append(out, v.Get("text"))
	}
	return out
}

func (f *fakeTG) sentLast() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return url.Values{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type notifyEnv struct {
	t     *testing.T
	a     *App
	out   *bytes.Buffer
	tg    *fakeTG
	root  string
	now   time.Time
	mac   []string
	chats []string // StartChat 收到的 prompt
	st    *State
}

// newNotifyEnv：withTG 为真时写好 telegram.env（权限 600）并指向假服务，否则只有本机通知。
func newNotifyEnv(t *testing.T, withTG bool) *notifyEnv {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	var out bytes.Buffer
	a, err := newLegacyConfigApp(t, home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg.ProjectsRoot = root
	e := &notifyEnv{t: t, a: a, out: &out, root: root, st: &State{},
		now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)}
	a.Now = func() time.Time { return e.now }
	a.Notify.MacNotify = func(title, body string) error { e.mac = append(e.mac, title+"｜"+body); return nil }
	a.Chat.StartChat = func(st *State, id, prompt string) (*Job, error) {
		e.chats = append(e.chats, prompt)
		j := &Job{ID: id, Kind: state.KindChat, Project: chat.ChatJobName, Status: state.StatusRunning, StartedAt: e.now}
		st.Jobs = append(st.Jobs, j)
		return j, nil
	}
	if withTG {
		e.tg = newFakeTG(t)
		a.Telegram.TelegramAPI = e.tg.srv.URL
		os.MkdirAll(filepath.Join(home, ".agentq", "secrets"), 0o700)
		env := fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)
		if err := os.WriteFile(a.Telegram.TelegramEnvPath(), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// flush 模拟一次 tick 末尾的推送（第一次运行会先把现有拍板条目记下、不推）。
func (e *notifyEnv) flush() {
	e.t.Helper()
	// 真 tick 在推送前已经存过 state.json（收消息时读它判断有没有对话在跑、要不要先说「收到」）
	if err := state.Save(e.a.StatePath(), e.st); err != nil {
		e.t.Fatal(err)
	}
	e.a.Sched.NotifyTick(e.st)
}

// acks 是回给用户的「收到」类短句（不是排好版的消息）。
func (f *fakeTG) acks() []string {
	var out []string
	for _, t := range f.texts() {
		if strings.HasPrefix(t, "收到，") {
			out = append(out, t)
		}
	}
	return out
}

// replies 是除「收到」以外发给用户的消息。
func (f *fakeTG) replies() []string {
	var out []string
	for _, t := range f.texts() {
		if !strings.HasPrefix(t, "收到，") {
			out = append(out, t)
		}
	}
	return out
}

func (e *notifyEnv) state() *notify.State {
	e.t.Helper()
	s, err := e.a.Notify.LoadNotify()
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *notifyEnv) at(h, m int) {
	e.now = time.Date(e.now.Year(), e.now.Month(), e.now.Day(), h, m, 0, 0, time.Local)
}

func (e *notifyEnv) write(path, text string) {
	e.t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *notifyEnv) report(day time.Time, text string) string {
	p := filepath.Join(e.a.Home, ".claude", "nightly", day.Format("2006-01-02")+".md")
	e.write(p, text)
	old := day.Add(-2 * time.Hour)
	os.Chtimes(p, old, old)
	return p
}

func lines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// 同类 30 分钟内只发一条：第一条马上发，之后的攒着，到点合成一条；别的类别不受影响。
func TestNotifyMergeSameCategory(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "问题甲", Lines: []string{"证据甲"}})
	e.flush()
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "问题甲") || !strings.HasPrefix(got[0], "🟠 <b>巡逻发现 · 10月11日</b>") {
		t.Fatalf("第一条应马上发、标题行按格式：%q", got)
	}
	if v := e.tg.sent[0]; v.Get("chat_id") != fmt.Sprint(ownerChat) || v.Get("parse_mode") != "HTML" {
		t.Fatalf("要发给用户的 chat id、用 HTML：%v", v)
	}
	e.at(12, 10)
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "问题乙"})
	e.a.Notify.Notify(notify.Notice{Cat: "decide", Text: "demo　选 A 还是 B"})
	e.flush()
	got := e.tg.texts()
	if len(got) != 2 || !strings.Contains(got[1], "要你定") || strings.Contains(strings.Join(got, ""), "问题乙") {
		t.Fatalf("12:10 只该发别的类别（要你定），巡逻的攒着：%q", got)
	}
	e.at(12, 20)
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "问题丙"})
	e.flush()
	if e.tg.count() != 2 {
		t.Fatalf("12:20 还在 30 分钟内，不该发：%q", e.tg.texts())
	}
	e.at(12, 31)
	e.flush()
	got = e.tg.texts()
	if len(got) != 3 || !strings.Contains(got[2], "问题乙") || !strings.Contains(got[2], "问题丙") {
		t.Fatalf("12:31 应把乙、丙合成一条：%q", got)
	}
	if !strings.HasSuffix(got[2], "<i>12:31 · agentq</i>") {
		t.Fatalf("最后一行应是「<i>时间 · 来源</i>」：%q", got[2])
	}
	if s := e.state(); len(s.Pending) != 0 || s.Count != 3 {
		t.Fatalf("发完队列应为空、今天 3 条：%+v", s)
	}
	if len(e.mac) != 0 {
		t.Fatalf("配了 Telegram 不该弹本机通知：%v", e.mac)
	}
}

// 23:00–08:00 只攒着；安全问题当时就推；08:00 后早报写好了就和早报一起发，并写 report-pushed 标记。
func TestNotifyQuietHoursAndMorning(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.at(23, 30)
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "夜里的问题"})
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "额度快用完"})
	e.flush()
	if e.tg.count() != 0 {
		t.Fatalf("23:30 不该推：%q", e.tg.texts())
	}
	e.now = e.now.Add(3 * time.Hour) // 02:30
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "备份连续失败", Security: true})
	e.flush()
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "备份连续失败") || strings.Contains(got[0], "夜里的问题") {
		t.Fatalf("安全问题应当时单独推，别的接着攒：%q", got)
	}
	e.at(7, 59)
	e.flush()
	if e.tg.count() != 1 {
		t.Fatalf("07:59 不该推：%q", e.tg.texts())
	}
	// 早报还在写（刚改过）：08:00 不推早报，攒着的按类别发
	day := e.now
	rp := filepath.Join(e.a.Home, ".claude", "nightly", day.Format("2006-01-02")+".md")
	e.write(rp, "# 昨晚合并 3 条，没有要你定的\n\n正文\n")
	os.Chtimes(rp, e.now, e.now)
	e.at(8, 0)
	e.now = e.now.Add(30 * time.Second)
	e.flush()
	got := e.tg.texts()
	if len(got) != 3 || !strings.Contains(got[1], "额度快用完") || !strings.Contains(got[2], "夜里的问题") {
		t.Fatalf("早报没写完时，08:00 攒着的按类别各发一条（要你知道在前）：%q", got)
	}
	if _, err := os.Stat(e.a.Notify.ReportPushedPath(day.Format("2006-01-02"))); err == nil {
		t.Fatal("早报没推，不该写标记")
	}
	// 早报写完了：推一次早报，带上新攒的消息，写标记
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "八点后的新问题"})
	e.at(8, 5)
	e.flush()
	got = e.tg.texts()
	if len(got) != 4 || !strings.HasPrefix(got[3], "☀️ <b>早报 · ") || !strings.Contains(got[3], "八点后的新问题") ||
		!strings.Contains(got[3], "昨晚合并 3 条，没有要你定的") || !strings.Contains(got[3], "早报全文 ~/.claude/nightly/") {
		t.Fatalf("早报应带上攒着的消息、早报第一行和全文路径：%q", got)
	}
	if lines(got[3]) > notify.NotifyMaxLines {
		t.Fatalf("早报超过 %d 行：%q", notify.NotifyMaxLines, got[3])
	}
	if _, err := os.Stat(e.a.Notify.ReportPushedPath(day.Format("2006-01-02"))); err != nil {
		t.Fatalf("早报推成功后应写标记：%v", err)
	}
	e.at(9, 0)
	e.flush()
	if e.tg.count() != 4 {
		t.Fatalf("一天只推一次早报：%q", e.tg.texts())
	}
}

// 早报：复盘还在跑不推；08:00 前不推；别处推过（有标记）不推；要你定的放最前。
func TestMorningConditions(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	testkit.MkProject(t, e.root, "demo", "")
	bl := filepath.Join(e.root, "demo", "docs", "BACKLOG.md")
	b, _ := os.ReadFile(bl)
	os.WriteFile(bl, []byte(strings.Replace(string(b), "## 已完成", "## 等用户决定\n\n- [ ] 选数据库 —— A 还是 B；推荐 A；分支 agent/x\n\n## 已完成", 1)), 0o644)
	end := e.now.Add(-3 * time.Hour)
	e.st.Jobs = []*Job{
		{ID: "j1", Project: "demo", Title: "加按月对比", Status: state.StatusDone, EndedAt: &end},
		{ID: "j2", Project: "demo", Title: "修崩溃", Status: state.StatusFailed, EndedAt: &end},
		{ID: "r1", Kind: config.KindReview, Project: "复盘", Status: state.StatusRunning},
	}
	// 巡逻现有的问题：早报「出问题」只列任务的，基础设施类由看门狗推
	ps := &patrol.PatrolState{Issues: []patrol.PatrolIssue{
		{Fingerprint: "disk", Check: "disk", Title: "磁盘剩余不到 20 GB"},
		{Fingerprint: "autopush:/p/a/.git@x", Check: "autopush", Title: "自动备份推送失败：a"},
		{Fingerprint: "job:j2", Check: "job", Title: "任务失败：demo「修崩溃」"},
	}}
	if err := e.a.Patrol.SavePatrol(ps); err != nil {
		t.Fatal(err)
	}
	reset := e.now.Add(48 * time.Hour)
	e.st.SetUsageOf("", &backend.UsageInfo{CheckedAt: e.now.Add(2 * time.Hour), Windows: []backend.UsageWindow{
		{Label: "session", Used: 44, ResetsAt: &reset}, {Label: "week (Fable)", Scope: "Fable", Used: 100, ResetsAt: &reset}}})
	e.at(7, 0)
	e.report(e.now, "早报第一行\n")
	e.flush()
	e.at(9, 0)
	e.flush()
	if e.tg.count() != 0 {
		t.Fatalf("08:00 前、复盘在跑时都不该推早报：%q", e.tg.texts())
	}
	e.st.Jobs[2].Status = state.StatusDone
	e.flush()
	got := e.tg.texts()
	if len(got) != 1 {
		t.Fatalf("复盘跑完应推早报：%q", got)
	}
	m := textfmt.HTMLToPlain(got[0])
	iDecide, iDone := strings.Index(m, "🔴 要你定　1 条"), strings.Index(m, "🟢 昨晚完成　1 条")
	if iDecide < 0 || iDone < 0 || iDecide > iDone || !strings.Contains(m, "demo　选数据库") ||
		!strings.Contains(m, "跑完 1，失败 1") || !strings.Contains(m, "demo　加按月对比") {
		t.Fatalf("早报内容不对（要你定在前、完成、任务成败）：%s", m)
	}
	// 早报第一行是标题下的结论；额度进度条放最后；只限 Fable、任务不用的窗口不列
	iLead, iTasks, iUsage := strings.Index(m, "早报第一行"), strings.Index(m, "跑完 1，失败 1"), strings.Index(m, "📊 额度")
	if iLead < 0 || iLead > iDecide || iUsage < iTasks || !strings.Contains(m, "会话 ▰▰▰▰▱▱▱▱▱▱   44%") || strings.Contains(m, "Fable") {
		t.Fatalf("早报顺序或额度不对：%s", m)
	}
	if !strings.Contains(m, "出问题　1 个") || !strings.Contains(m, "任务失败：demo「修崩溃」") || strings.Contains(m, "磁盘") || strings.Contains(m, "自动备份推送失败") {
		t.Fatalf("早报「出问题」只列任务的：%s", m)
	}
	// 第二天：别处已经写了标记，不再推
	e.now = e.now.Add(24 * time.Hour)
	e.report(e.now, "第二天的早报\n")
	e.write(e.a.Notify.ReportPushedPath(e.now.Format("2006-01-02")), "x")
	e.flush()
	if e.tg.count() != 1 {
		t.Fatalf("有标记就不再推：%q", e.tg.texts())
	}
}

// 每天最多 10 条：第 11 条起攒着（安全问题照推），第二天 08:00 后再发。
func TestNotifyDailyCap(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.at(9, 0)
	for i := 0; i < 11; i++ {
		e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: fmt.Sprintf("问题%d", i)})
		e.flush()
		e.now = e.now.Add(31 * time.Minute)
	}
	if e.tg.count() != notify.NotifyDailyMax {
		t.Fatalf("一天最多 %d 条，发了 %d", notify.NotifyDailyMax, e.tg.count())
	}
	if s := e.state(); len(s.Pending) != 1 || s.Pending[0].Text != "问题10" {
		t.Fatalf("超出上限的应攒着：%+v", s.Pending)
	}
	// 安全问题不受上限，攒着的普通消息搭它一起发，不丢
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "密钥泄露", Security: true})
	e.flush()
	got := e.tg.texts()
	if len(got) != notify.NotifyDailyMax+1 || !strings.HasPrefix(got[len(got)-1], "🔴 <b>要你知道") ||
		!strings.Contains(got[len(got)-1], "密钥泄露") || !strings.Contains(textfmt.HTMLToPlain(got[len(got)-1]), "🟠 问题10") ||
		strings.Index(got[len(got)-1], "密钥泄露") > strings.Index(got[len(got)-1], "问题10") {
		t.Fatalf("安全问题不受上限，攒着的跟在后面一起发：%q", got)
	}
	if s := e.state(); len(s.Pending) != 0 {
		t.Fatalf("搭车发出的应从队列去掉：%+v", s.Pending)
	}
	// 「要你定」不受上限（tick 扫 BACKLOG 发现的新条目），同样带上攒着的
	testkit.MkProject(t, e.root, "demo", "")
	e.flush() // 第一次扫拍板：只记下
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "问题11"}, notify.Notice{Cat: "release", Text: "新版可装"})
	bl := filepath.Join(e.root, "demo", "docs", "BACKLOG.md")
	b, _ := os.ReadFile(bl)
	os.WriteFile(bl, []byte(strings.Replace(string(b), "## 已完成", "## 等用户决定\n\n- [ ] 上架定价 —— 推荐免费\n\n## 已完成", 1)), 0o644)
	e.now = e.now.Add(time.Minute)
	e.flush()
	got = e.tg.texts()
	last := got[len(got)-1]
	if plain := textfmt.HTMLToPlain(last); len(got) != notify.NotifyDailyMax+2 || !strings.HasPrefix(last, "🔴 <b>要你定") ||
		!strings.Contains(plain, "demo　上架定价") || !strings.Contains(plain, "🟠 问题11") || !strings.Contains(plain, "📦 新版可装") {
		t.Fatalf("上限后「要你定」照发，并带上攒着的：%q", got)
	}
	// 夜里：安全问题照发，但普通消息不搭车（夜里只攒着）
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "夜里的问题"})
	e.at(23, 30)
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "备份连续失败", Security: true})
	e.flush()
	got = e.tg.texts()
	if last := got[len(got)-1]; len(got) != notify.NotifyDailyMax+3 || !strings.Contains(last, "备份连续失败") || strings.Contains(last, "夜里的问题") {
		t.Fatalf("夜里安全问题单独发：%q", got)
	}
	e.now = time.Date(2026, 10, 12, 8, 1, 0, 0, time.Local)
	e.flush()
	if got := e.tg.texts(); !strings.Contains(got[len(got)-1], "夜里的问题") {
		t.Fatalf("第二天应发出攒着的：%q", got)
	}
}

// 已发 9 条时几类同时到点：一次只发到上限，后面几类攒着，搭下一条要紧消息一起发。
func TestNotifyDailyCapSameFlush(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.at(9, 0)
	e.a.Notify.WithNotify(func(s *notify.State) error {
		s.Day, s.Count = e.now.Format("2006-01-02"), notify.NotifyDailyMax-1
		return nil
	})
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "额度快用完"}, notify.Notice{Cat: "patrol", Text: "磁盘不够"},
		notify.Notice{Cat: "release", Text: "新版可装"}, notify.Notice{Cat: "done", Text: "第一版做好了"})
	e.flush()
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "额度快用完") {
		t.Fatalf("只该发到上限（1 条）：%q", got)
	}
	if s := e.state(); len(s.Pending) != 3 || s.Count != notify.NotifyDailyMax {
		t.Fatalf("其余 3 条攒着、今天 %d 条：%+v（%d）", notify.NotifyDailyMax, s.Pending, s.Count)
	}
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "密钥泄露", Security: true})
	e.flush()
	got := e.tg.texts()
	if m := got[len(got)-1]; len(got) != 2 || !strings.Contains(m, "密钥泄露") || !strings.Contains(m, "磁盘不够") ||
		!strings.Contains(m, "新版可装") || !strings.Contains(m, "第一版做好了") {
		t.Fatalf("攒着的应搭安全问题一起发：%q", got)
	}
}

// 只认用户的 chat id：别人发的不回、不处理；所有消息只发给用户。「状态」当场回，不启动 claude。
func TestTelegramOnlyOwnerAndStatus(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	testkit.MkProject(t, e.root, "demo", "- [ ] 加功能 —— x\n")
	e.tg.msg(10, 999, 1, "状态", "")
	e.tg.msg(11, 999, 2, "demo：删库", "")
	e.tg.msg(12, ownerChat, 3, "状态", "")
	e.flush()
	got := e.tg.texts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "📊 <b>状态 · 10月11日</b>") || !strings.Contains(got[0], "demo 1") {
		t.Fatalf("只应回用户一条状态摘要：%q", got)
	}
	for _, v := range e.tg.sent {
		if v.Get("chat_id") != fmt.Sprint(ownerChat) {
			t.Fatalf("发给了别人：%v", v)
		}
	}
	if rp := e.tg.sent[0].Get("reply_parameters"); !strings.Contains(rp, `"message_id":3`) {
		t.Fatalf("应作为对用户那条消息的回复：%q", rp)
	}
	if len(e.chats) != 0 {
		t.Fatalf("别人的消息、「状态」都不该启动 claude：%q", e.chats)
	}
	if !strings.Contains(e.out.String(), "忽略 chat id 999") {
		t.Fatalf("日志应记下忽略了别人的消息：%s", e.out.String())
	}
	if s := e.state(); s.UpdateOffset != 13 || len(s.Inbox) != 0 {
		t.Fatalf("offset 应确认到 13、收件箱为空：%+v", s)
	}
	e.flush()
	if e.tg.count() != 1 || e.tg.offsets[len(e.tg.offsets)-1] != "13" {
		t.Fatalf("同一条不该处理两次：%q %v", e.tg.texts(), e.tg.offsets)
	}
}

// 派活、拍板、问问题都交给一次 claude：prompt 带上原话和回复的那条；对话任务结束后把回答回发给用户。
func TestTelegramChatJob(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.msg(20, ownerChat, 7, "demo：加个按月对比", "")
	e.tg.msg(21, ownerChat, 8, "选 A", "🔴 要你定 · 10月11日\ndemo　选数据库")
	e.flush()
	if len(e.chats) != 1 {
		t.Fatalf("两条消息应一起交给一次 claude：%d", len(e.chats))
	}
	p := e.chats[0]
	for _, want := range []string{"demo：加个按月对比", "选 A", "这条是回复 agentq 之前发的这条消息：🔴 要你定 · 10月11日 demo 选数据库", "dispatch", "<b>类别 · 10月11日</b>",
		"现在是 10-11 周日 12:00", "+%u` 输出 1 是周一"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 里缺「%s」：\n%s", want, p)
		}
	}
	if got := e.tg.texts(); len(got) != 2 || got[0] != chat.AckNow || got[1] != chat.AckNow {
		t.Fatalf("每条先回一句「收到」（同一批一起答，不算排队）：%q", got)
	}
	if rp := e.tg.sent[1].Get("reply_parameters"); !strings.Contains(rp, `"message_id":8`) {
		t.Fatalf("「收到」应挂在那条消息下：%q", rp)
	}
	// 上一个对话还在跑（手上 2 条）：新消息排队，不再启动，告诉用户前面还有 2 条
	e.tg.msg(22, ownerChat, 9, "还有，my-app 的测试修了没", "")
	e.flush()
	if len(e.chats) != 1 || len(e.state().Inbox) != 1 {
		t.Fatalf("对话在跑时新消息应排队：chats=%d inbox=%+v", len(e.chats), e.state().Inbox)
	}
	if acks := e.tg.acks(); len(acks) != 3 || acks[2] != fmt.Sprintf(chat.AckQueued, 2) {
		t.Fatalf("排队的应说前面还有 2 条：%q", acks)
	}
	// 对话任务结束：回答原样回发，挂在最后一条消息下面
	j := e.st.Jobs[0]
	e.write(filepath.Join(e.a.JobDir(j.ID), "result.json"), `{"is_error":false,"result":"🟢 <b>已派出 · 10月11日</b>\n\n🟢 demo　加个按月对比，任务 x\n<i>12:05 · agentq 对话</i>","session_id":"s"}`)
	e.a.Runner.FinishFromResult(e.st, j)
	got := e.tg.replies()
	if len(got) != 1 || !strings.HasPrefix(got[0], "🟢 <b>已派出") {
		t.Fatalf("应把 claude 的回答发给用户：%q", got)
	}
	if rp := e.tg.sent[len(e.tg.sent)-1].Get("reply_parameters"); !strings.Contains(rp, `"message_id":8`) {
		t.Fatalf("应回复在最后一条消息下：%q", rp)
	}
	// 下一次 tick 接着处理排队的那条
	e.flush()
	if len(e.chats) != 2 || !strings.Contains(e.chats[1], "my-app 的测试修了没") {
		t.Fatalf("排队的消息应在上一个对话结束后处理：%q", e.chats)
	}
	// 对话任务失败：回原因
	j2 := e.st.Jobs[1]
	e.write(filepath.Join(e.a.JobDir(j2.ID), "result.json"), `{"is_error":true,"result":"出错了"}`)
	e.a.Runner.FinishFromResult(e.st, j2)
	if got := e.tg.replies(); len(got) != 2 || !strings.Contains(got[1], "这条没答上来：失败") || !strings.Contains(got[1], "agentq logs "+j2.ID) {
		t.Fatalf("没答上来要回原因：%q", got)
	}
}

// 冷却中、额度 ≥98% 时不启动 claude，直接回用户原因。
func TestTelegramChatRefused(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.st.ExtendCooldown("", e.now.Add(time.Hour))
	e.tg.msg(30, ownerChat, 4, "帮我查查 my-app 昨天的崩溃", "")
	e.flush()
	got := e.tg.texts()
	if len(e.chats) != 0 || len(got) != 1 || !strings.Contains(got[0], "用量冷却中") || !strings.HasPrefix(got[0], "🔴 <b>要你知道") {
		t.Fatalf("冷却中应直接回原因：chats=%d %q", len(e.chats), got)
	}
	if len(e.state().Inbox) != 0 {
		t.Fatal("回了原因后不再排队")
	}
}

// token 不能出现在日志、状态文件和报错里（Go 的 http 报错带完整 URL）。
func TestTelegramTokenRedacted(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.errDesc = "Unauthorized: bot" + fakeToken + " revoked"
	e.a.Notify.Notify(notify.Notice{Cat: "patrol", Text: "问题"})
	e.flush()
	e.tg.srv.Close() // 网络错误：报错里有 URL
	e.at(13, 0)
	e.flush()
	b, _ := os.ReadFile(e.a.Notify.NotifyPath())
	for name, s := range map[string]string{"日志": e.out.String(), "notify.json": string(b)} {
		if strings.Contains(s, fakeToken) || strings.Contains(s, "FAKE-token") {
			t.Fatalf("%s 里有 token：%s", name, s)
		}
	}
	if !strings.Contains(e.out.String(), "bot***") && !strings.Contains(e.out.String(), "***") {
		t.Fatalf("报错应打码后记下：%s", e.out.String())
	}
	if s := e.state(); len(s.Pending) != 1 {
		t.Fatalf("发不出去应留在队列：%+v", s.Pending)
	}
}

// Telegram 发不出去：弹一次本机通知（只弹一次），消息留着，恢复后补发。
func TestTelegramFailFallsBackToMac(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.failSend = 2
	e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "熔断了"})
	e.flush()
	e.at(12, 5)
	e.flush()
	if len(e.mac) != 1 || !strings.Contains(e.mac[0], "Telegram 没发出去") || !strings.Contains(e.mac[0], "熔断了") {
		t.Fatalf("应只弹一次本机通知：%v", e.mac)
	}
	e.at(12, 10)
	e.flush()
	if got := e.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "熔断了") {
		t.Fatalf("恢复后应补发：%q", got)
	}
}

// HTML 解析不了时去掉标签按纯文本重发。
func TestTelegramPlainFallback(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.rejectHTML = true
	c, _ := e.a.Telegram.TelegramCreds()
	if _, err := e.a.Telegram.TgSend(c, "<b>标题</b>\n<x> &amp; 正文", 0); err != nil {
		t.Fatal(err)
	}
	v := e.tg.sent[0]
	if v.Get("parse_mode") != "" || v.Get("text") != "标题\n & 正文" {
		t.Fatalf("应按纯文本重发：%v", v)
	}
}

// 没配 Telegram：弹本机通知，不收消息；凭据文件权限不对：不用它，报错。
func TestNotifyMacOnlyAndBadPerm(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, false)
	e.a.Notify.Notify(notify.Notice{Cat: "release", Text: "demo 1.0 (21) 可装", Lines: []string{"TestFlight 打开就能装"}})
	e.flush()
	if len(e.mac) != 1 || !strings.HasPrefix(e.mac[0], "📦 新版可装 · 10月11日｜") || !strings.Contains(e.mac[0], "demo 1.0 (21) 可装") {
		t.Fatalf("没配 Telegram 应弹本机通知：%v", e.mac)
	}
	if !strings.Contains(e.a.Notify.NotifyStatus(), "本机通知") {
		t.Fatalf("status 应显示用本机通知：%s", e.a.Notify.NotifyStatus())
	}
	// 权限 644 的凭据不用
	os.MkdirAll(filepath.Dir(e.a.Telegram.TelegramEnvPath()), 0o700)
	os.WriteFile(e.a.Telegram.TelegramEnvPath(), []byte("TELEGRAM_BOT_TOKEN="+fakeToken+"\nTELEGRAM_CHAT_ID=1\n"), 0o644)
	os.Chmod(e.a.Telegram.TelegramEnvPath(), 0o644)
	if c, err := e.a.Telegram.TelegramCreds(); c != nil || err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("权限不对应报错：%v %v", c, err)
	}
	if strings.Contains(e.a.Notify.NotifyStatus(), fakeToken) {
		t.Fatal("status 里有 token")
	}
}

// 新的「等用户决定」条目推一条「要你定」；第一次运行只记下不推；条目不变不重推。
func TestWatchDecisions(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	testkit.MkProject(t, e.root, "demo", "")
	bl := filepath.Join(e.root, "demo", "docs", "BACKLOG.md")
	base, _ := os.ReadFile(bl)
	setDecide := func(items string) {
		os.WriteFile(bl, []byte(strings.Replace(string(base), "## 已完成", "## 等用户决定\n\n"+items+"\n## 已完成", 1)), 0o644)
	}
	setDecide("- [ ] 旧的拍板 —— 早就在了\n")
	e.flush()
	if e.tg.count() != 0 {
		t.Fatalf("第一次运行不推旧条目：%q", e.tg.texts())
	}
	setDecide("- [ ] 旧的拍板 —— 早就在了\n- [ ] 上架定价 —— 免费还是 6 元；推荐免费；分支 agent/price\n")
	e.flush()
	got := e.tg.texts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "🔴 <b>要你定 · ") || !strings.Contains(got[0], "demo　上架定价") ||
		!strings.Contains(got[0], "推荐免费") || strings.Contains(got[0], "旧的拍板") {
		t.Fatalf("新条目应推一条要你定：%q", got)
	}
	e.at(13, 0)
	e.flush()
	if e.tg.count() != 1 {
		t.Fatalf("同一条不重推：%q", e.tg.texts())
	}
}

// 带「（等条件：…）」的拍板条目不推、不进早报和「状态」的要你定；正文顺带提到「等条件」的照常推；标记去掉后才推。
func TestDecisionsWaitCondition(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	testkit.MkProject(t, e.root, "demo", "")
	bl := filepath.Join(e.root, "demo", "docs", "BACKLOG.md")
	base, _ := os.ReadFile(bl)
	setDecide := func(items string) {
		os.WriteFile(bl, []byte(strings.Replace(string(base), "## 已完成", "## 等用户决定\n\n"+items+"\n## 已完成", 1)), 0o644)
	}
	const waiting = "- [ ] v4 切换发布 —— 是否发布；推荐发布（等条件：前面 7 条都合并）\n  - 子项写在下面\n"
	const mention = "- [ ] 推送规则 —— 正文顺带提到等条件：带标记的跳过；要不要也跳过早报？推荐跳过\n"
	e.flush() // 第一次运行
	setDecide(waiting + mention)
	e.flush()
	got := e.tg.texts()
	if len(got) != 1 || !strings.Contains(got[0], "demo　推送规则") || strings.Contains(got[0], "v4 切换发布") {
		t.Fatalf("只推没带标记的那条：%q", got)
	}
	// 「状态」和早报的要你定都不算等条件的
	if d := e.a.Chat.StatusDigest(e.st); !strings.Contains(d, "要你定　1 条") || strings.Contains(d, "v4 切换发布") {
		t.Fatalf("状态里的要你定不该有等条件的：%s", d)
	}
	e.at(9, 0)
	e.report(e.now, "早报第一行\n")
	e.flush()
	got = e.tg.texts()
	if m := got[len(got)-1]; !strings.HasPrefix(m, "☀️ <b>早报") || !strings.Contains(m, "要你定　1 条") || strings.Contains(m, "v4 切换发布") {
		t.Fatalf("早报的要你定不该有等条件的：%s", m)
	}
	// 条件满足，标记去掉：这时才推
	setDecide(strings.Replace(waiting, "（等条件：前面 7 条都合并）", "", 1) + mention)
	e.at(10, 0)
	e.flush()
	got = e.tg.texts()
	if m := got[len(got)-1]; !strings.HasPrefix(m, "🔴 <b>要你定") || !strings.Contains(m, "demo　v4 切换发布") {
		t.Fatalf("标记去掉后应推：%q", got)
	}
}

// 超过 12 行：发前面的摘要和全文路径，全文存进文件。
func TestRenderOverflow(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, false)
	var secs []notify.MsgSection
	for i := 0; i < 8; i++ {
		secs = append(secs, notify.MsgSection{Emoji: "🟠", Head: fmt.Sprintf("问题 %d <x>", i), Lines: []string{"证据"}})
	}
	m := e.a.Notify.RenderMessage("patrol", e.now, secs, "agentq")
	if lines(m) != notify.NotifyMaxLines || !strings.Contains(m, "其余 ") || !strings.Contains(m, "&lt;x&gt;") {
		t.Fatalf("应截到 %d 行、带全文路径、转义：%s", notify.NotifyMaxLines, m)
	}
	files, _ := filepath.Glob(filepath.Join(e.a.DataDir(), "notify", "*-patrol.txt"))
	if len(files) != 1 {
		t.Fatalf("全文应存进文件：%v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "问题 7 <x>") {
		t.Fatalf("全文不全：%s", b)
	}
}

// 版式：标题下一句结论；每段「表情 <b>段名</b>」，内容进带竖条的块，行首项目名加粗；额度进度条进 <pre>，超长截断时也留着。
func TestRenderLayout(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, false)
	usage := notify.MsgSection{Emoji: "📊", Head: "额度", Pre: true, Lines: []string{"会话 ▰▱▱▱▱▱▱▱▱▱   1%  今天 19:00 重置"}}
	m := e.a.Notify.RenderMessage("morning", e.now, []notify.MsgSection{
		{Lead: true, Head: "昨晚做完 2 条 <都过门>"},
		{Emoji: "🔴", Head: "要你定　1 条", Lines: []string{"demo　选数据库", "3　编号不加粗"}},
		usage,
	}, "agentq")
	want := "☀️ <b>早报 · " + textfmt.DateCN(e.now) + "</b>\n\n昨晚做完 2 条 &lt;都过门&gt;\n\n🔴 <b>要你定　1 条</b>\n" +
		"<blockquote><b>demo</b>　选数据库\n3　编号不加粗</blockquote>\n\n📊 <b>额度</b>\n<pre>会话 ▰▱▱▱▱▱▱▱▱▱   1%  今天 19:00 重置</pre>\n<i>" + e.now.Format("15:04") + " · agentq</i>"
	if m != want {
		t.Fatalf("得到：\n%s\n应为：\n%s", m, want)
	}
	var secs []notify.MsgSection
	for i := 0; i < 8; i++ {
		secs = append(secs, notify.MsgSection{Emoji: "🟠", Head: fmt.Sprintf("问题 %d", i), Lines: []string{"证据"}})
	}
	m = e.a.Notify.RenderMessage("patrol", e.now, append(secs, usage), "agentq")
	if !strings.Contains(m, "其余 ") || !strings.Contains(m, "<pre>会话 ▰▱▱▱▱▱▱▱▱▱") || strings.Count(m, "<blockquote>") != strings.Count(m, "</blockquote>") {
		t.Fatalf("超长截断时额度进度条照留、块都闭合：%s", m)
	}
}

func TestNotifySendCommand(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	if err := e.a.CmdNotify([]string{"send", "nope", "x"}); err == nil {
		t.Fatal("不认识的类别应报错")
	}
	if err := e.a.CmdNotify([]string{"send", "--key=k1", "release", "demo 1.0 (21) 可装", "TestFlight 打开就能装"}); err != nil {
		t.Fatal(err)
	}
	if got := e.tg.texts(); len(got) != 1 || !strings.HasPrefix(got[0], "📦 <b>新版可装") || !strings.Contains(got[0], "TestFlight 打开就能装") {
		t.Fatalf("send 应马上推：%q", got)
	}
	e.at(23, 30)
	e.out.Reset()
	if err := e.a.CmdNotify([]string{"send", "done", "demo　第一版做好了"}); err != nil {
		t.Fatal(err)
	}
	if e.tg.count() != 1 || !strings.Contains(e.out.String(), "稍后推") {
		t.Fatalf("夜里应攒着并说明：%q %s", e.tg.texts(), e.out.String())
	}
}

// 集成：真的 agentq 二进制 + 假 Telegram + 假 claude，agentq listen 没在跑时 tick 兜底收消息。tick 收到用户的「状态」当场回；
// 派活的话先回「收到」再交给对话任务，任务写了 BACKLOG、结束后（任务外壳触发的那次 tick 收尾）把回答回发给用户；别人发的不理。
func TestIntegrationTelegram(t *testing.T) {
	t.Parallel()
	// 暂停着：对话任务照常（用户亲口问的），而且对话写进 BACKLOG 的新条目不会被 tick 接着启动成任务
	// （那个任务的外壳不在用例的等待名单里，用例结束后还在写临时 HOME）
	e := newEnv(t, map[string]any{"notify_enabled": true, "tick_after_job": true, "paused": true})
	testkit.MkProject(t, e.root, "demo", "")
	tg := newFakeTG(t)
	e.extra = append(e.extra, "AGENTQ_TELEGRAM_API="+tg.srv.URL)
	sec := filepath.Join(e.home, ".agentq", "secrets")
	os.MkdirAll(sec, 0o700)
	os.WriteFile(filepath.Join(sec, "telegram.env"), []byte(fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%d\n", fakeToken, ownerChat)), 0o600)
	tg.msg(1, 4242, 1, "状态", "")
	tg.msg(2, ownerChat, 2, "状态", "")
	tg.msg(3, ownerChat, 3, "demo：加个按月对比 TGDISPATCH", "")
	out := e.mustRun("tick")
	if strings.Contains(out, fakeToken) {
		t.Fatalf("输出里有 token：%s", out)
	}
	// 等最后写出的那样东西：收尾时先回发回答、tick 最后才存 state.json，所以等 state 里对话任务记成结束（LESSONS）
	chatEnded := func() bool {
		st, err := state.Load(filepath.Join(e.home, ".agentq", "state.json"))
		if err != nil {
			return false
		}
		for _, j := range st.Jobs {
			if j.Kind == state.KindChat && j.Status != state.StatusRunning {
				return true
			}
		}
		return false
	}
	waitFor(t, 60*time.Second, "对话任务收尾、回答发给用户", func() bool { return tg.count() >= 3 && chatEnded() })
	got := tg.texts()
	if len(got) != 3 || !strings.HasPrefix(got[0], "📊 <b>状态 · ") || got[1] != chat.AckNow || !strings.HasPrefix(got[2], "🟢 <b>已派出") {
		t.Fatalf("应先回状态、再说收到、最后回派活结果：%q", got)
	}
	for _, v := range tg.sent {
		if v.Get("chat_id") != fmt.Sprint(ownerChat) {
			t.Fatalf("发给了别人：%v", v)
		}
	}
	if rp := tg.sent[2].Get("reply_parameters"); !strings.Contains(rp, `"message_id":3`) {
		t.Fatalf("派活的回答应挂在那条消息下：%q", rp)
	}
	b, _ := os.ReadFile(filepath.Join(e.root, "demo", "docs", "BACKLOG.md"))
	if !strings.Contains(string(b), "- [ ] 加个按月对比 —— 来自 Telegram") {
		t.Fatalf("对话任务应写进 BACKLOG：%s", b)
	}
	var chat *Job
	for _, j := range e.state().Jobs {
		if j.Kind == state.KindChat {
			chat = j
		}
	}
	if chat == nil || chat.Status != state.StatusDone {
		t.Fatalf("对话任务应记成跑完：%+v", chat)
	}
	logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	var prompt string
	for _, f := range logs {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "TGDISPATCH") {
			prompt = string(b)
		}
	}
	if !strings.Contains(prompt, "PWD["+testkit.RealPath(t, e.root)+"]") {
		t.Fatalf("对话任务应在 projects_root 里跑：%s", prompt)
	}
	if !strings.Contains(e.launchdLog()+out, "忽略 chat id 4242") {
		t.Fatalf("应记下忽略了别人的消息：%s", out)
	}
}

// 第一次收消息时，配机器人那会儿发的旧消息只确认、不处理；之后来的照常处理。
func TestTelegramFirstPollSkipsOld(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.msgAt(40, ownerChat, 1, "你好", "", e.now.Add(-2*time.Hour))
	e.tg.msgAt(41, ownerChat, 2, "状态", "", e.now.Add(-time.Minute))
	e.flush()
	if len(e.chats) != 0 || e.tg.count() != 1 || e.state().UpdateOffset != 42 {
		t.Fatalf("旧消息应跳过、新的「状态」照回：chats=%d sent=%q", len(e.chats), e.tg.texts())
	}
	e.tg.msgAt(42, ownerChat, 3, "很久以前排的", "", e.now.Add(-3*time.Hour))
	e.flush()
	if len(e.chats) != 1 {
		t.Fatal("有了 offset 之后，旧消息也照常处理（agentq 停过一阵时不能丢用户的话）")
	}
}

// 拿着推送锁时本进程里再记消息（如启动对话任务失败触发熔断）不能卡在等同一把锁上。
func TestNotifyReentrant(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.a.Chat.StartChat = func(st *State, id, prompt string) (*Job, error) {
		e.a.Notify.Notify(notify.Notice{Cat: "alert", Text: "熔断了"})
		return nil, fmt.Errorf("启动失败")
	}
	e.tg.msg(50, ownerChat, 1, "查一下", "")
	start := time.Now()
	e.flush()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("等了 %s，应该直接记进去", d)
	}
	got := e.tg.replies()
	if len(got) != 2 || !strings.Contains(got[0], "没能启动 claude：启动失败") || !strings.Contains(got[1], "熔断了") {
		t.Fatalf("应回启动失败的原因，并推出熔断：%q", got)
	}
}

// 对话任务的回答当时没发出去：存下来，下次 tick 补发。
func TestChatReplyRetried(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	e.tg.msg(60, ownerChat, 6, "查一下", "")
	e.flush()
	j := e.st.Jobs[0]
	e.write(filepath.Join(e.a.JobDir(j.ID), "result.json"), `{"is_error":false,"result":"📊 <b>状态 · 10月11日</b>\n\n📊 查完了\n<i>12:01 · agentq 对话</i>"}`)
	e.tg.failSend = 1
	e.a.Runner.FinishFromResult(e.st, j)
	if len(e.tg.replies()) != 0 || len(e.state().Replies) != 1 {
		t.Fatalf("发不出去应存下来：sent=%q replies=%+v", e.tg.texts(), e.state().Replies)
	}
	e.at(12, 5)
	e.flush()
	if got := e.tg.replies(); len(got) != 1 || !strings.Contains(got[0], "查完了") || !strings.Contains(e.tg.sent[len(e.tg.sent)-1].Get("reply_parameters"), `"message_id":6`) {
		t.Fatalf("下次 tick 应补发并挂在原消息下：%q", got)
	}
	if len(e.state().Replies) != 0 {
		t.Fatal("补发后应清掉")
	}
}
