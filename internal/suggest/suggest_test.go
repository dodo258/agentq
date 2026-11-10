package suggest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/telegram"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

func TestParseNotDoSuggest(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int
	}{
		{"不做 1", []int{1}},
		{"不做1 3", []int{1, 3}},
		{" 不做 1，3、5。", []int{1, 3, 5}},
		{"不做 3 3", []int{3}},
		{"做 1", nil},
		{"不做", nil},
		{"不做这个", nil},
		{"不做 1 吧", nil},
		{"先不做 1", nil},
		{"不做 0", nil},
	} {
		got, ok := ParseNotDoSuggest(c.in)
		if ok != (c.want != nil) || len(got) != len(c.want) {
			t.Errorf("%q → %v %v", c.in, got, ok)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q → %v", c.in, got)
			}
		}
	}
	// 「做 N」的写法没变，也不会把「不做」认成「做」
	if _, ok := ParseDoSuggest("不做 1"); ok {
		t.Error("不做 不能当 做")
	}
}

func TestInsertSuggestions(t *testing.T) {
	lines := []string{"- [ ] 新一 —— x（建议 1）", "- [ ] 新二 —— y（建议 2）"}
	cases := map[string]struct{ in, want string }{
		"新建在等用户决定之后": {
			"# 待办\n\n## 待做\n\n- [ ] 甲\n\n## 等用户决定\n\n- [ ] 拍板一\n\n## 以后再说\n\n- [ ] 晚点\n\n## 已完成\n",
			"# 待办\n\n## 待做\n\n- [ ] 甲\n\n## 等用户决定\n\n- [ ] 拍板一\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n\n## 以后再说\n\n- [ ] 晚点\n\n## 已完成\n",
		},
		"没有拍板节放在待做之后": {
			"# 待办\n\n## 待做\n\n- [ ] 甲\n\n## 已完成\n",
			"# 待办\n\n## 待做\n\n- [ ] 甲\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n\n## 已完成\n",
		},
		"已有建议节往末尾加": {
			"## 待做\n\n## 建议\n\n- [ ] 旧\n\n## 已完成\n",
			"## 待做\n\n## 建议\n\n- [ ] 旧\n" + strings.Join(lines, "\n") + "\n\n## 已完成\n",
		},
		"建议节是空的": {
			"## 待做\n\n## 建议\n\n## 已完成\n",
			"## 待做\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n\n## 已完成\n",
		},
		"待做节是最后一节": {
			"# 待办\n\n## 待做\n\n- [ ] 甲\n",
			"# 待办\n\n## 待做\n\n- [ ] 甲\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n",
		},
		"注释里的假标题不算": {
			"## 待做\n\n<!--\n## 建议\n## 等用户决定\n-->\n- [ ] 甲\n\n## 已完成\n",
			"## 待做\n\n<!--\n## 建议\n## 等用户决定\n-->\n- [ ] 甲\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n\n## 已完成\n",
		},
		"没有任何节": {"# 只有标题\n", "# 只有标题\n\n## 建议\n\n" + strings.Join(lines, "\n") + "\n"},
		"空文件":   {"", "\n## 建议\n\n" + strings.Join(lines, "\n") + "\n"},
	}
	for name, c := range cases {
		got := InsertSuggestions(c.in, lines)
		if got != c.want {
			t.Errorf("%s：\n得到 %q\n要   %q", name, got, c.want)
		}
		// 不管放哪，tick 只看「待做」：建议里的条目绝不能被解析成待做
		for _, it := range bl.ParseBacklog(got) {
			if strings.Contains(it.Text, "新一") || strings.Contains(it.Text, "新二") {
				t.Errorf("%s：建议里的条目被当成待做：%+v", name, it)
			}
		}
	}
}

func TestMoveSuggestion(t *testing.T) {
	line := "- [ ] 改排序 —— 为什么；好处：快；风险：低（建议 3）"
	text := "## 待做\n\n- [ ] 甲\n\n## 建议\n\n" + line + "\n- [ ] 别的 —— z（建议 4）\n\n## 已完成\n"
	got, err := moveSuggestion(text, bl.Fingerprint(line), "改排序")
	if err != nil {
		t.Fatal(err)
	}
	want := "## 待做\n\n- [ ] 甲\n- [ ] 改排序 —— 为什么；好处：快；风险：低\n\n## 建议\n\n- [ ] 别的 —— z（建议 4）\n\n## 已完成\n"
	if got != want {
		t.Errorf("得到 %q\n要   %q", got, want)
	}
	items := bl.ParseBacklog(got)
	if len(items) != 2 || items[1].Title != "改排序" || !items[1].Eligible() {
		t.Errorf("移过去的应是一条可做的待做：%+v", items)
	}
	// 被改过（指纹对不上）但标题没变：按标题找到
	edited := strings.Replace(text, "好处：快", "好处：很快", 1)
	if got, err := moveSuggestion(edited, bl.Fingerprint(line), "改排序"); err != nil || !strings.Contains(got, "- [ ] 改排序 —— 为什么；好处：很快；风险：低\n") {
		t.Errorf("改过的按标题找：%q %v", got, err)
	}
	// 失效：被删、被改了标题、已经不在建议节（在待做里同名也不算）、在注释里
	gone := map[string]string{
		"被删":     "## 待做\n\n## 建议\n\n## 已完成\n",
		"改了标题":   strings.Replace(text, "改排序", "改成别的", 1),
		"已在待做里":  "## 待做\n\n" + line + "\n\n## 建议\n\n## 已完成\n",
		"在已完成里":  "## 待做\n\n## 建议\n\n## 已完成\n\n" + line + "\n",
		"在注释里":   "## 待做\n\n## 建议\n\n<!--\n" + line + "\n-->\n\n## 已完成\n",
		"没有建议节":  "## 待做\n\n## 已完成\n",
		"文件是空的":  "",
		"建议在待做前": "",
	}
	delete(gone, "建议在待做前")
	for name, in := range gone {
		if got, err := moveSuggestion(in, bl.Fingerprint(line), "改排序"); err != ErrSuggestGone {
			t.Errorf("%s：应是 errSuggestGone，得到 %q %v", name, got, err)
		}
	}
	// 没有「待做」节：报错，不丢条目（调用方不改文件）
	if _, err := moveSuggestion("## 建议\n\n"+line+"\n", bl.Fingerprint(line), "改排序"); err == nil || err == ErrSuggestGone {
		t.Errorf("没有待做节应报别的错：%v", err)
	}
}

func TestParseDoSuggest(t *testing.T) {
	ok := map[string][]int{
		"做 1 3":     {1, 3},
		"做1 3":      {1, 3},
		"做 1,3":     {1, 3},
		"做 1，3":     {1, 3},
		"做 1、3":     {1, 3},
		"  做 2  ":   {2},
		"做 2。":      {2},
		"做 3 3 1 3": {3, 1},
		"做 12 007":  {12, 7},
	}
	for in, want := range ok {
		got, isDo := ParseDoSuggest(in)
		if !isDo || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q → %v %v，要 %v", in, got, isDo, want)
		}
	}
	for _, in := range []string{"", "做", "做点什么", "做 一", "做 1 做 3", "帮我做 1", "做 1 然后停", "做 0", "做 -1", "做 1234567", "做 1 3 abc", "do 1", "做\n1\n再说", "做 " + strings.Repeat("1 ", 21)} {
		if got, isDo := ParseDoSuggest(in); isDo {
			t.Errorf("%q 不该认，得到 %v", in, got)
		}
	}
}

// wireSuggest 建模块实例。
func wireSuggest(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Notify: k.Notify}
}

// newSuggestApp 带 projects_root 的实例，HOME 也指到临时目录，git 配置给出身份。
func newSuggestApp(t *testing.T) (*Service, string) {
	t.Helper()
	var out bytes.Buffer
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	cfg := config.Default()
	cfg.ProjectsRoot = root
	return wireSuggest(&env.Env{Home: home, Cfg: cfg, Now: time.Now, Out: &out}), root
}

func suggestRepo(t *testing.T, a *Service, root, name, backlog string) bl.Project {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, dir, "add", "-A")
	testkit.GitT(t, a.Home, dir, "commit", "-q", "-m", "init")
	return bl.Project{Name: name, Dir: dir}
}

const suggestBacklogText = "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 已完成\n\n- [x] 做过的事 —— 合并提交 abc1234\n"

type seedItem struct{ project, title string }

func suggestItemOf(project, title, _ string) seedItem { return seedItem{project, title} }

// seed 照自我进化写提案的做法，把建议写进项目「## 建议」并登记编号（每个项目一次提交）。
func (a *Service) seed(t *testing.T, items ...seedItem) {
	t.Helper()
	first, err := a.ReserveSuggestNumbers(len(items))
	if err != nil {
		t.Fatal(err)
	}
	var entries []SuggestEntry
	byProject := map[string][]string{}
	var order []string
	for i, it := range items {
		line := fmt.Sprintf("- [ ] %s —— 要做的事；好处：好处；风险：风险（建议 %d）", it.title, first+i)
		if byProject[it.project] == nil {
			order = append(order, it.project)
		}
		byProject[it.project] = append(byProject[it.project], line)
		entries = append(entries, SuggestEntry{N: first + i, Project: it.project, Fingerprint: bl.Fingerprint(line), Title: it.title, CreatedAt: a.Now()})
	}
	for _, name := range order {
		p := bl.Project{Name: name, Dir: filepath.Join(a.Cfg.ProjectsRoot, name)}
		if err := a.Backlog.CommitBacklog(p, "待办：建议", func(old string) (string, error) { return InsertSuggestions(old, byProject[name]), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.WithSuggest(func(st *SuggestStore) error { st.Items = append(st.Items, entries...); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (a *Service) suggestMustStore(t *testing.T) *SuggestStore {
	t.Helper()
	s, err := a.LoadSuggest()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func suggestCommits(t *testing.T, a *Service, p bl.Project) int {
	t.Helper()
	var n int
	fmt.Sscan(testkit.GitT(t, a.Home, p.Dir, "rev-list", "--count", "HEAD"), &n)
	return n
}

func suggestBacklogOf(t *testing.T, p bl.Project) string {
	t.Helper()
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func suggestMust(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestSuggestStoreCorrupt(t *testing.T) {
	a, _ := newSuggestApp(t)
	os.MkdirAll(a.DataDir(), 0o755)
	if err := os.WriteFile(a.SuggestionsPath(), []byte("{坏了"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := a.LoadSuggest()
	if err != nil || len(s.Items) != 0 || s.Next != 1 {
		t.Fatalf("坏文件当空的：%+v %v", s, err)
	}
	baks, _ := filepath.Glob(a.SuggestionsPath() + ".bad-*")
	if len(baks) != 1 || string(suggestMust(os.ReadFile(baks[0]))) != "{坏了" {
		t.Errorf("应备份坏文件：%v", baks)
	}
	if a.hasSuggestions() {
		t.Error("坏文件当没有建议")
	}
	// 只备份一次、只记一次日志：备份后文件已重置成正常的空索引，后面的读取不再备份
	for i := 0; i < 3; i++ {
		time.Sleep(1100 * time.Millisecond) // 备份文件名精确到秒，不隔开就看不出是不是又备份了
		if _, err := a.LoadSuggest(); err != nil {
			t.Fatal(err)
		}
	}
	if baks, _ := filepath.Glob(a.SuggestionsPath() + ".bad-*"); len(baks) != 1 {
		t.Errorf("反复读取不该反复备份：%v", baks)
	}
	if n := strings.Count(a.Out.(*bytes.Buffer).String(), "格式不对"); n != 1 {
		t.Errorf("日志只记一次：%d", n)
	}
	var probe SuggestStore
	if err := json.Unmarshal(suggestMust(os.ReadFile(a.SuggestionsPath())), &probe); err != nil || probe.Next != 1 {
		t.Errorf("文件应已重置成合法的空索引：%v %+v", err, probe)
	}
	// 能读但内容不合理：编号重复、非正数、缺项目都丢掉；Next 不倒退
	raw := `{"next":2,"items":[{"n":5,"project":"a","fingerprint":"x","title":"t"},{"n":5,"project":"a","fingerprint":"y","title":"重复"},{"n":-1,"project":"a","fingerprint":"z"},{"n":6,"project":"","fingerprint":"z"}]}`
	if err := os.WriteFile(a.SuggestionsPath(), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = a.LoadSuggest()
	if err != nil || len(s.Items) != 1 || s.Items[0].Title != "t" || s.Next != 6 {
		t.Errorf("清理后：%+v %v", s, err)
	}
	// 读不了（是目录）：返回错误，不当空的覆盖
	os.Remove(a.SuggestionsPath())
	os.MkdirAll(a.SuggestionsPath(), 0o755)
	if _, err := a.LoadSuggest(); err == nil {
		t.Error("读不了应报错")
	}
	if a.hasSuggestions() {
		t.Error("读不了当没有建议（不拦截消息）")
	}
}

func TestDoSuggestions(t *testing.T) {
	a, root := newSuggestApp(t)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	beta := suggestRepo(t, a, root, "beta", suggestBacklogText)
	a.seed(t,
		suggestItemOf("alpha", "建议甲", "product"), suggestItemOf("alpha", "建议乙", "product"), suggestItemOf("beta", "建议丙", "product"), suggestItemOf("beta", "建议丁", "product"),
	)
	if !a.hasSuggestions() || len(a.suggestMustStore(t).Items) != 4 {
		t.Fatal("准备：4 条建议")
	}
	// 建议乙被用户删了；建议丁被用户改了字（标题没变）
	ab := strings.Replace(suggestBacklogOf(t, alpha), "- [ ] 建议乙 —— 要做的事；好处：好处；风险：风险（建议 2）\n", "", 1)
	os.WriteFile(alpha.BacklogPath(), []byte(ab), 0o644)
	testkit.GitT(t, a.Home, alpha.Dir, "commit", "-qam", "删一条")
	bb := strings.Replace(suggestBacklogOf(t, beta), "建议丁 —— 要做的事", "建议丁 —— 用户改过的说明", 1)
	os.WriteFile(beta.BacklogPath(), []byte(bb), 0o644)
	testkit.GitT(t, a.Home, beta.Dir, "commit", "-qam", "改一条")
	na, nb := suggestCommits(t, a, alpha), suggestCommits(t, a, beta)

	msg := a.DoSuggestions([]int{1, 2, 3, 4, 99})
	for _, want := range []string{"已移进「待做」　3 条", "1　alpha　建议甲", "3　beta　建议丙", "4　beta　建议丁", "编号失效", "2（alpha 的「建议」里已经没有这一条", "99（没有这个编号"} {
		if !strings.Contains(msg, want) {
			t.Errorf("回复缺 %q：\n%s", want, msg)
		}
	}
	ab = suggestBacklogOf(t, alpha)
	if got := bl.ParseBacklog(ab); len(got) != 1 || got[0].Title != "建议甲" || strings.Contains(ab, "（建议 ") || strings.Contains(ab, "建议乙") {
		t.Errorf("alpha：\n%s", ab)
	}
	bb = suggestBacklogOf(t, beta)
	if got := bl.ParseBacklog(bb); len(got) != 2 || !strings.Contains(bb, "用户改过的说明") || strings.Contains(bb, "（建议 ") {
		t.Errorf("beta：\n%s", bb)
	}
	if suggestCommits(t, a, alpha) != na+1 || suggestCommits(t, a, beta) != nb+2 || testkit.GitT(t, a.Home, beta.Dir, "status", "--porcelain") != "" {
		t.Errorf("每个移动一次提交，工作区干净")
	}
	if s := a.suggestMustStore(t); len(s.Items) != 0 {
		t.Errorf("处理过的和失效的都从索引里去掉：%+v", s.Items)
	}
	if a.hasSuggestions() {
		t.Error("没有建议了不再拦截")
	}
	// 再发一遍：全部失效，不报错、不改文件
	msg = a.DoSuggestions([]int{1, 3})
	if strings.Contains(msg, "已移进") || !strings.Contains(msg, "编号失效") || suggestCommits(t, a, alpha) != na+1 {
		t.Errorf("再发一遍：\n%s", msg)
	}
}

func TestDoSuggestionsCommitFailureKeepsEntry(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "建议甲", "product"), suggestItemOf("alpha", "建议乙", "product"))
	// 主目录有没提交的 BACKLOG 改动：commitBacklog 拒绝，条目留着，下次还能做
	dirty := suggestBacklogOf(t, p) + "\n用户正在改\n"
	os.WriteFile(p.BacklogPath(), []byte(dirty), 0o644)
	msg := a.DoSuggestions([]int{1})
	if !strings.Contains(msg, "没能移") || strings.Contains(msg, "编号失效") || suggestBacklogOf(t, p) != dirty || len(a.suggestMustStore(t).Items) != 2 {
		t.Errorf("提交被拒：\n%s", msg)
	}
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "--", "docs/BACKLOG.md")
	if msg := a.DoSuggestions([]int{1}); !strings.Contains(msg, "已移进「待做」　1 条") {
		t.Errorf("恢复后能做：\n%s", msg)
	}
	// 项目目录没了：编号失效
	os.RemoveAll(p.Dir)
	if msg := a.DoSuggestions([]int{2}); !strings.Contains(msg, "项目 alpha 已经不在了") || a.hasSuggestions() {
		t.Errorf("项目没了：\n%s", msg)
	}
}

// 「做 N」只在索引里有建议时拦截；回复的是已登记的回复链消息（如回「要你定」时说“做 1”）时交给回复链。
func TestDoSuggestApplies(t *testing.T) {
	a, root := newSuggestApp(t)
	suggestRepo(t, a, root, "alpha", suggestBacklogText)
	plain := &telegram.TgMsg{MessageID: 10, Text: "做 1"}
	if a.DoSuggestApplies(plain, []int{1}) {
		t.Error("没有建议时不拦截")
	}
	a.seed(t, suggestItemOf("alpha", "建议甲", "product"))
	if !a.DoSuggestApplies(plain, []int{1}) {
		t.Error("有建议时拦截")
	}
	if !a.DoSuggestApplies(plain, []int{7, 1}) {
		t.Error("部分有效也拦截（有效的做，失效的说明）")
	}
	if a.DoSuggestApplies(plain, []int{7, 8}) {
		t.Error("一个有效编号都没有：放行给模型")
	}
	replyTo := func(id int64) *telegram.TgMsg {
		return &telegram.TgMsg{MessageID: 11, Text: "做 1", ReplyTo: &telegram.TgMsg{MessageID: id}}
	}
	if !a.DoSuggestApplies(replyTo(500), []int{1}) {
		t.Error("回复的是没登记的消息（如那条建议推送）照样拦截")
	}
	if err := a.Notify.WithNotify(func(s *notify.State) error {
		s.AddThread(501, notify.Thread{Kind: notify.ThreadDecide, Project: "alpha", Fingerprint: "decide:alpha:x"}, a.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.DoSuggestApplies(replyTo(501), []int{1}) {
		t.Error("回复的是已登记的回复链消息时交给回复链")
	}
	if !a.DoSuggestApplies(replyTo(500), []int{1}) {
		t.Error("别的消息不受影响")
	}
}

// 索引里失效的编号会被清掉：BACKLOG「建议」里已经找不到的、项目没了的、超过 30 天的；读不出的保留。
func TestSuggestIndexExpires(t *testing.T) {
	a, root := newSuggestApp(t)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "留着", "product"), suggestItemOf("alpha", "被删", "product"), suggestItemOf("alpha", "被移走", "product"))
	b := suggestBacklogOf(t, alpha)
	b = strings.Replace(b, "- [ ] 被删 —— 要做的事；好处：好处；风险：风险（建议 2）\n", "", 1)
	b = strings.Replace(b, "- [ ] 被移走 —— 要做的事；好处：好处；风险：风险（建议 3）\n", "", 1)
	b = strings.Replace(b, "## 待做\n", "## 待做\n\n- [ ] 被移走 —— 要做的事；好处：好处；风险：风险\n", 1)
	os.WriteFile(alpha.BacklogPath(), []byte(b), 0o644)
	testkit.GitT(t, a.Home, alpha.Dir, "commit", "-qam", "用户整理")
	// 再塞三条：项目没了的、超过 30 天的、BACKLOG 读不出的项目
	old := a.Now().Add(-31 * 24 * time.Hour)
	s := a.suggestMustStore(t)
	s.Items = append(s.Items,
		SuggestEntry{N: 10, Project: "gone", Fingerprint: "x", Title: "t", CreatedAt: a.Now()},
		SuggestEntry{N: 11, Project: "alpha", Fingerprint: s.Items[0].Fingerprint, Title: s.Items[0].Title, CreatedAt: old})
	os.WriteFile(a.SuggestionsPath(), suggestMust(json.Marshal(s)), 0o644)
	live, err := a.LiveSuggestions(true)
	if err != nil || len(live.Items) != 1 || live.Items[0].N != 1 {
		t.Fatalf("清理后只剩留着的：%+v %v", live, err)
	}
	if got := a.suggestMustStore(t); len(got.Items) != 1 || got.Next < 12 {
		t.Errorf("清理结果存回去，编号不回收：%+v", got)
	}
	// 项目目录读不了（projects_root 扫不了）：不知道就不动，只清超过 30 天的
	a.Cfg.ProjectsRoot = filepath.Join(root, "不存在")
	if live, _ := a.LiveSuggestions(true); len(live.Items) != 1 {
		t.Errorf("读不出的保留：%+v", live.Items)
	}
	// 配了 extra_projects 也一样：列表不全，不能因为不在列表里就清掉
	extra := filepath.Join(root, "外面", "ext")
	if err := os.MkdirAll(filepath.Join(extra, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Cfg.ExtraProjects = []string{extra}
	if live, _ := a.LiveSuggestions(true); len(live.Items) != 1 {
		t.Errorf("有 extra 时读不出的也要保留：%+v", live.Items)
	}
}

// 用户删掉建议：编号作废，标题留 30 天继续判重。
func TestDeletedSuggestionStaysRejected(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "改默认排序", "product"), suggestItemOf("alpha", "改配色", "product"))
	b := strings.Replace(suggestBacklogOf(t, p), "- [ ] 改默认排序 —— 要做的事；好处：好处；风险：风险（建议 1）\n", "", 1)
	os.WriteFile(p.BacklogPath(), []byte(b), 0o644)
	testkit.GitT(t, a.Home, p.Dir, "commit", "-qam", "用户删掉不要的建议")

	live, err := a.LiveSuggestions(true)
	if err != nil || len(live.Items) != 1 || live.Items[0].Title != "改配色" {
		t.Fatalf("编号作废：%+v %v", live.Items, err)
	}
	if len(live.Past) != 1 || live.Past[0].Title != "改默认排序" {
		t.Fatalf("标题留着：%+v", live.Past)
	}
	if got := a.DoSuggestions([]int{1}); !strings.Contains(got, "编号失效") {
		t.Errorf("作废的编号：%s", got)
	}
	// 「做 N」时发现条目没了：同样作废编号、留标题
	b = strings.Replace(suggestBacklogOf(t, p), "- [ ] 改配色 —— 要做的事；好处：好处；风险：风险（建议 2）\n", "", 1)
	os.WriteFile(p.BacklogPath(), []byte(b), 0o644)
	testkit.GitT(t, a.Home, p.Dir, "commit", "-qam", "又删一条")
	a.DoSuggestions([]int{2})
	got := a.suggestMustStore(t)
	if len(got.Items) != 0 || len(got.Proposed("alpha")) != 2 {
		t.Errorf("做 N 发现没了：items=%d proposed=%d", len(got.Items), len(got.Proposed("alpha")))
	}
}

// 主目录临时切到别的分支：读到的不是真正的 BACKLOG，不清理编号；切回 main 后编号仍然有效。
func TestSuggestIndexKeptWhileOffMain(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "留着", "product"))
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
	testkit.GitT(t, a.Home, p.Dir, "rm", "-q", "-f", "docs/BACKLOG.md")
	testkit.GitT(t, a.Home, p.Dir, "commit", "-q", "-m", "这个分支上没有 BACKLOG")
	live, err := a.LiveSuggestions(true)
	if err != nil || len(live.Items) != 1 || len(live.Past) != 0 {
		t.Fatalf("不在 main 不清理：%+v %v", live, err)
	}
	if !a.DoSuggestApplies(&telegram.TgMsg{}, []int{1}) {
		t.Error("编号仍然有效")
	}
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "main")
	if live, _ := a.LiveSuggestions(true); len(live.Items) != 1 {
		t.Errorf("切回 main 后仍有效：%+v", live.Items)
	}
	if msg := a.DoSuggestions([]int{1}); !strings.Contains(msg, "已移进「待做」") {
		t.Errorf("切回后能做：%s", msg)
	}
}
