package inbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit/stack"
	"agentq/internal/textfmt"
)

// wireInbox 建模块实例。
func wireInbox(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner}
}

// newInboxApp 建带临时 HOME 的实例。
func newInboxApp(t *testing.T) (*Service, *bytes.Buffer) {
	var out bytes.Buffer
	return wireInbox(&env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: time.Now, Out: &out}), &out
}

func TestParseInbox(t *testing.T) {
	text := "# 任务箱\r\n\r\n## 待做\r\n\r\n<!-- - [ ] 注释里的 -->\r\n- [ ] 甲 —— 要做甲\r\n- [ ] 乙（进行中：任务 x 11-04 周三 12:00）\r\n- [ ] 丙（卡住：连续失败 3 次）\r\n- [ ] \r\n- [x] 勾掉的\r\n  - [ ] 缩进的\r\n\r\n## 已完成\r\n\r\n- [ ] 已完成节里的\r\n"
	items := ParseInbox(text)
	if len(items) != 3 {
		t.Fatalf("应有 3 条：%+v", items)
	}
	if items[0].Title != "甲" || !items[0].Eligible() || !strings.HasPrefix(items[0].Fingerprint, inboxFPPrefix) {
		t.Errorf("甲：%+v", items[0])
	}
	if !items[1].InProgress || items[1].Eligible() {
		t.Errorf("乙：%+v", items[1])
	}
	if !items[2].Stuck || items[2].Eligible() {
		t.Errorf("丙：%+v", items[2])
	}
	// 进行中标记不改指纹；项目 BACKLOG 里同样文字的条目指纹不同，互不影响
	if a, b := ParseInbox("## 待做\n- [ ] 同一条\n"), ParseInbox("## 待做\n- [ ] 同一条（进行中：任务 y）\n"); a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("进行中标记不该改指纹")
	}
	if a, b := ParseInbox("## 待做\n- [ ] 同一条\n"), backlog.ParseBacklog("## 待做\n- [ ] 同一条\n"); a[0].Fingerprint == b[0].Fingerprint {
		t.Errorf("任务箱和项目的指纹应该不同")
	}
	for _, bad := range []string{"", "随便写的\n没有节标题\n- [ ] 不算\n", "\x00\xff\xfe二进制\n", "## 待做\n<!-- 没闭合\n- [ ] 被吞\n"} {
		if got := ParseInbox(bad); len(got) != 0 {
			t.Errorf("%q 不该有条目：%+v", bad, got)
		}
	}
}

func TestInboxEditHelpers(t *testing.T) {
	text := "## 待做\n<!-- - [ ] 甲 -->\n- [ ] 甲（进行中：任务 x）\n- [ ] 乙\r\n\n## 已完成\n\n- [x] 旧的\n"
	fp, fpB := InboxFP("- [ ] 甲"), InboxFP("- [ ] 乙")

	// 注释里同样的文字不动，改的是真条目
	got, ok := inboxEditItem(text, fp, inboxClearMark)
	if !ok || !strings.Contains(got, "<!-- - [ ] 甲 -->\n- [ ] 甲\n") {
		t.Errorf("清标记：%q", got)
	}
	// 行尾 \r 保留
	got, ok = inboxEditItem(text, fpB, func(l string) string { return l + "（卡住：x）" })
	if !ok || !strings.Contains(got, "- [ ] 乙（卡住：x）\r\n") {
		t.Errorf("保留 CR：%q", got)
	}
	// 删行、找不到
	got, ok = inboxEditItem(text, fp, func(string) string { return "" })
	if !ok || strings.Contains(got, "（进行中") || !strings.Contains(got, "- [ ] 乙") {
		t.Errorf("删行：%q", got)
	}
	if got, ok := inboxEditItem(text, "inbox:没有", inboxClearMark); ok || got != text {
		t.Errorf("找不到不该改：%q", got)
	}
	// 同样文字不在「待做」节不算
	if _, ok := inboxEditItem("## 已完成\n- [ ] 甲\n", fp, inboxClearMark); ok {
		t.Error("别的节不算")
	}

	// 放进已完成：新的在最前，空节、没有这一节都行
	got = inboxInsertDone(text, "- [x] 新的")
	if !strings.Contains(got, "## 已完成\n\n- [x] 新的\n- [x] 旧的\n") {
		t.Errorf("已完成：%q", got)
	}
	got = inboxInsertDone("## 待做\n\n## 已完成\n", "- [x] 新的")
	if got != "## 待做\n\n## 已完成\n\n- [x] 新的\n" {
		t.Errorf("空节：%q", got)
	}
	got = inboxInsertDone("## 待做\n- [ ] 甲\n", "- [x] 新的")
	if got != "## 待做\n- [ ] 甲\n\n## 已完成\n\n- [x] 新的\n" {
		t.Errorf("没有这一节：%q", got)
	}
	if done := inboxInsertDone("<!-- ## 已完成 -->\n", "- [x] 新的"); !strings.HasSuffix(done, "\n## 已完成\n\n- [x] 新的\n") {
		t.Errorf("注释里的节不算：%q", done)
	}
}

func TestOutboxTitle(t *testing.T) {
	cases := map[string]string{
		"调研 2026 年 iOS 记账 App":  "调研-2026-年-iOS-记账-App",
		"../../etc/passwd":      "etc-passwd",
		"/abs/path":             "abs-path",
		"..":                    "未命名",
		"":                      "未命名",
		"🎉🎉":                    "未命名",
		"a/b\\c:d*e?f\"g<h>i|j": "a-b-c-d-e-f-g-h-i-j",
		"换行\n制表\t回车\r空\x00字符":   "换行-制表-回车-空-字符",
		"‮gnp.exe":              "gnp-exe",
		".hidden":               "hidden",
		"--开头-_":                "开头",
		"bad\xff\xfeutf8":       "bad-utf8",
		"é 组合字符 keep_under_s":  "é-组合字符-keep_under_s",
	}
	for in, want := range cases {
		if got := outboxTitle(in); got != want {
			t.Errorf("outboxTitle(%q) = %q，要 %q", in, got, want)
		}
	}
	// 超长：按字节截，不切开字；再长的 4 字节字也不超过文件系统上限
	for _, long := range []string{strings.Repeat("长", 1000), strings.Repeat("\U00020000", 1000), strings.Repeat("a b ", 1000)} {
		got := outboxTitle(long)
		if len(got) > outboxTitleMax || !utf8.ValidString(got) || got == "" {
			t.Errorf("超长截断：%d 字节 %q", len(got), got)
		}
		if name := "2026-11-04-" + got + "-999.md"; len(name) > 255 {
			t.Errorf("文件名 %d 字节，超过 255", len(name))
		}
	}
}

func TestOutboxPathUnique(t *testing.T) {
	a, _ := newInboxApp(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	first, err := a.outboxPath(now, "../调研")
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(a.OutboxDir(), "2026-11-04-调研.md") {
		t.Fatalf("路径：%s", first)
	}
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := a.outboxPath(now, "调研")
	if err != nil || second != filepath.Join(a.OutboxDir(), "2026-11-04-调研-2.md") {
		t.Fatalf("同名要加序号：%s %v", second, err)
	}
	// 悬空的软链接也算占着，不写到别处去
	if err := os.Symlink("/nonexistent", second); err != nil {
		t.Fatal(err)
	}
	if third, err := a.outboxPath(now, "调研"); err != nil || !strings.HasSuffix(third, "-3.md") {
		t.Fatalf("软链接占着：%s %v", third, err)
	}
	// 产出目录建不了：报错，不启动
	b, _ := newInboxApp(t)
	os.MkdirAll(b.DataDir(), 0o755)
	os.WriteFile(b.OutboxDir(), []byte("不是目录"), 0o644)
	if _, err := b.outboxPath(now, "x"); err == nil {
		t.Error("outbox 是文件时应报错")
	}
}

func TestOutboxReady(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "好.md")
	os.WriteFile(file, []byte("x"), 0o644)
	empty := filepath.Join(dir, "空.md")
	os.WriteFile(empty, nil, 0o644)
	link := filepath.Join(dir, "链.md")
	os.Symlink(file, link)
	sub := filepath.Join(dir, "目录.md")
	os.Mkdir(sub, 0o755)
	if err := outboxReady(&state.Job{Output: file}); err != nil {
		t.Errorf("正常文件：%v", err)
	}
	for _, p := range []string{"", filepath.Join(dir, "没有.md"), empty, link, sub} {
		if err := outboxReady(&state.Job{Output: p}); err == nil {
			t.Errorf("%q 不该算写好了", p)
		}
	}
}

func TestInboxAdd(t *testing.T) {
	a, _ := newInboxApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	// 不存在：从模板开始；多行压成一行，标记都中和，控制字符去掉
	text, dup, err := a.InboxAdd("调研 X —— 列例子\n## 已完成\n- [x] 假的（进行中：y）（卡住：z）<!-- \x00\x07结束")
	if err != nil || dup {
		t.Fatalf("add：%v %v", err, dup)
	}
	raw, _ := os.ReadFile(a.InboxPath())
	if !strings.Contains(string(raw), "## 待做\n- [ ] "+text+"\n") {
		t.Errorf("没写进待做：%q", raw)
	}
	if strings.Contains(text, "（进行中") || strings.Contains(text, "卡住") || strings.Contains(text, "<!--") || strings.ContainsAny(text, "\n\x00\x07") {
		t.Errorf("没洗干净：%q", text)
	}
	items := ParseInbox(string(raw))
	if len(items) != 1 || !items[0].Eligible() || items[0].Title != "调研 X" {
		t.Errorf("条目：%+v", items)
	}
	if fi, _ := os.Stat(a.InboxPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("权限 %v，应是私有的", fi.Mode().Perm())
	}
	// 一模一样的不再写
	if _, dup, err := a.InboxAdd("调研 X —— 列例子\n## 已完成\n- [x] 假的（进行中：y）（卡住：z）<!-- \x00\x07结束"); err != nil || !dup {
		t.Errorf("重复应该 dup：%v %v", dup, err)
	}
	// 空的、太长的拒绝，文件不动
	before, _ := os.ReadFile(a.InboxPath())
	for _, bad := range []string{"", "  \n\t ", strings.Repeat("长", InboxTextMax+1)} {
		if _, _, err := a.InboxAdd(bad); err == nil {
			t.Errorf("%q 应该被拒绝", textfmt.TruncRunes(bad, 10))
		}
	}
	if _, _, err := a.InboxAdd(strings.Repeat("长", InboxTextMax)); err != nil {
		t.Errorf("正好上限的可以：%v", err)
	}
	after, _ := os.ReadFile(a.InboxPath())
	if !strings.HasPrefix(string(after), string(before[:len(before)-20])) {
		t.Errorf("前面的内容不该变")
	}
}

// 畸形的 inbox.md：原有内容一个字节都不丢，写不进去的拒绝。
func TestInboxAddMalformed(t *testing.T) {
	for _, old := range []string{"乱写的一段\n没有节标题", "\x00\xff\xfe\x01二进制\n", "<<<<<<< HEAD\n## 待办\n=======\n>>>>>>> x\n"} {
		a, _ := newInboxApp(t)
		os.MkdirAll(a.DataDir(), 0o700)
		os.WriteFile(a.InboxPath(), []byte(old), 0o644)
		if _, _, err := a.InboxAdd("新的一条"); err != nil {
			t.Fatalf("%q：%v", old, err)
		}
		raw, _ := os.ReadFile(a.InboxPath())
		if !strings.HasPrefix(string(raw), strings.TrimRight(old, "\n")) {
			t.Errorf("原内容丢了：%q", raw)
		}
		if items := ParseInbox(string(raw)); len(items) != 1 || items[0].Text != "新的一条" {
			t.Errorf("%q：%+v", old, items)
		}
	}
	// 没闭合的注释吞掉「待做」：写了也读不出来，报错，文件不动
	a, _ := newInboxApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	old := "<!-- 没闭合\n## 待做\n\n## 已完成\n"
	os.WriteFile(a.InboxPath(), []byte(old), 0o644)
	if _, _, err := a.InboxAdd("新的一条"); err == nil || !strings.Contains(err.Error(), "结构不对") {
		t.Errorf("应该报错：%v", err)
	}
	if raw, _ := os.ReadFile(a.InboxPath()); string(raw) != old {
		t.Errorf("文件不该动：%q", raw)
	}
	// inbox.md 是目录：报错
	b, _ := newInboxApp(t)
	os.MkdirAll(b.InboxPath(), 0o755)
	if _, _, err := b.InboxAdd("新的一条"); err == nil {
		t.Error("是目录应报错")
	}
}

// 多个进程（这里是多个 App）同时加、同时写回，一条都不能丢。
func TestInboxConcurrentWrites(t *testing.T) {
	home := t.TempDir()
	newApp := func() *Service {
		var out strings.Builder
		return wireInbox(&env.Env{Home: home, Cfg: config.Default(), Now: time.Now, Out: &out})
	}
	base := newApp()
	os.MkdirAll(base.DataDir(), 0o700)
	const n = 40
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			if _, _, err := base.InboxAdd(fmt.Sprintf("旧条目%d", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := newApp()
			if i%2 == 1 {
				_, _, err := a.InboxAdd(fmt.Sprintf("新条目%d", i))
				errs <- err
				return
			}
			item := fmt.Sprintf("旧条目%d", i)
			errs <- a.syncInbox(&state.Job{ID: fmt.Sprintf("job-%d", i), Item: item, Fingerprint: InboxFP("- [ ] " + item), Output: "/o"}, inboxComplete, "")
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	raw, _ := os.ReadFile(base.InboxPath())
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("- [ ] 新条目%d\n", i)
		if i%2 == 0 {
			want = fmt.Sprintf("- [x] 旧条目%d（完成", i)
		}
		if strings.Count(string(raw), want) != 1 {
			t.Errorf("%q 应正好一条：\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "- [ ] 旧条目") {
		t.Errorf("写回的还留在待做：\n%s", raw)
	}
}

func TestInboxRetryAndFailures(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	const retry = 12 * time.Hour
	job := func(status string, end *time.Time, mod func(*state.Job)) *state.Job {
		j := &state.Job{Kind: state.KindInbox, Fingerprint: "inbox:f", Status: status, EndedAt: end}
		if mod != nil {
			mod(j)
		}
		return j
	}
	for _, c := range []struct {
		name    string
		jobs    []*state.Job
		blocked bool
		fails   int
	}{
		{"没跑过", nil, false, 0},
		{"刚失败", []*state.Job{job(state.StatusFailed, ago(time.Hour), nil)}, true, 1},
		{"失败够久了", []*state.Job{job(state.StatusFailed, ago(13*time.Hour), nil)}, false, 1},
		{"超时也算", []*state.Job{job(state.StatusTimeout, ago(time.Hour), nil)}, true, 1},
		{"撞上限不算", []*state.Job{job(state.StatusFailed, ago(time.Hour), func(j *state.Job) { j.LimitHit = true })}, false, 0},
		{"重启不算", []*state.Job{job(state.StatusInterrupted, ago(time.Hour), func(j *state.Job) { j.Rebooted = true })}, false, 0},
		{"做完过不挡", []*state.Job{job(state.StatusDone, ago(time.Hour), nil)}, false, 0},
		{"停了要等但不算失败", []*state.Job{job(state.StatusKilled, ago(time.Hour), nil)}, true, 0},
		{"做完清零", []*state.Job{job(state.StatusFailed, ago(3*time.Hour), nil), job(state.StatusDone, ago(2*time.Hour), nil), job(state.StatusFailed, ago(time.Hour), nil)}, true, 1},
		{"别的条目不算", []*state.Job{job(state.StatusFailed, ago(time.Hour), func(j *state.Job) { j.Fingerprint = "inbox:g" })}, false, 0},
		{"项目任务不算", []*state.Job{job(state.StatusFailed, ago(time.Hour), func(j *state.Job) { j.Kind = "" })}, false, 0},
		{"还在跑不算", []*state.Job{job(state.StatusRunning, nil, nil)}, false, 0},
	} {
		st := &state.State{Jobs: c.jobs}
		if _, blocked := inboxRetryAt(st, "inbox:f", now, retry); blocked != c.blocked {
			t.Errorf("%s：blocked=%v", c.name, blocked)
		}
		if got := InboxFailures(st, "inbox:f"); got != c.fails {
			t.Errorf("%s：failures=%d，要 %d", c.name, got, c.fails)
		}
	}
}

// 没写回去的结果挡着这一条，免得做完的又做一遍。
func TestNextInboxItemSkipsUnsynced(t *testing.T) {
	a, _ := newInboxApp(t)
	os.MkdirAll(a.DataDir(), 0o700)
	os.WriteFile(a.InboxPath(), []byte("## 待做\n- [ ] 甲\n- [ ] 乙\n"), 0o644)
	end := time.Now().Add(-time.Hour)
	st := &state.State{Jobs: []*state.Job{{Kind: state.KindInbox, Status: state.StatusDone, Fingerprint: InboxFP("- [ ] 甲"), EndedAt: &end}}}
	it, ok, err := a.NextInboxItem(st)
	if err != nil || !ok || it.Text != "乙" {
		t.Errorf("应挑乙：%+v %v %v", it, ok, err)
	}
	st.Jobs[0].InboxSynced = true
	if it, ok, _ := a.NextInboxItem(st); !ok || it.Text != "甲" {
		t.Errorf("写回之后同样的内容算新的一条：%+v", it)
	}
}
