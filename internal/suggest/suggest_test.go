package suggest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/state"
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

// 永久判重：和 30 天的 past 并存；过了 30 天（past 清掉了）还挡得住。
func TestSuggestSkipsNotDoing(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "demo", suggestBacklogText)
	if err := os.WriteFile(p.NotDoingPath(), []byte(bl.AppendNotDoing("", bl.NotDoingEntry{Title: "加 夜间模式!", Quote: "不要", Date: "2020-01-01"})), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, p.Dir, "add", "-A")
	testkit.GitT(t, a.Home, p.Dir, "commit", "-q", "-m", "nd")
	j := &state.Job{ID: "1104-sg", SuggestProjects: []string{"demo"}}
	res := a.applySuggestions(j, []SuggestItem{suggestItemOf("demo", "加夜间模式", "product"), suggestItemOf("demo", "补个测试", "test")})
	got := suggestBacklogOf(t, p)
	if strings.Contains(got, "夜间模式") || !strings.Contains(got, "补个测试") {
		t.Errorf("%s", got)
	}
	if !strings.Contains(strings.Join(res.Skipped, "；"), "不做清单") {
		t.Errorf("%+v", res)
	}
	// 另一个项目同标题不受影响
	q := suggestRepo(t, a, root, "other", suggestBacklogText)
	j2 := &state.Job{ID: "1104-sg2", SuggestProjects: []string{"other"}}
	a.applySuggestions(j2, []SuggestItem{suggestItemOf("other", "加夜间模式", "product")})
	if !strings.Contains(suggestBacklogOf(t, q), "加夜间模式") {
		t.Error("只按本项目判重")
	}
	// 提示词带上，没有清单的项目不多这一段
	prompt := a.suggestPrompt(a.Now(), []string{"demo", "other"}, nil)
	if !strings.Contains(prompt, "用户明确不做的事") || !strings.Contains(prompt, "demo：加 夜间模式") {
		t.Errorf("%s", prompt)
	}
	if strings.Contains(a.suggestPrompt(a.Now(), []string{"other"}, nil), "用户明确不做") {
		t.Error("没有清单不多这一段")
	}
	// 读不了（目录占了文件名）：只记日志，不挡别的
	os.Remove(q.NotDoingPath())
	os.MkdirAll(q.NotDoingPath(), 0o755)
	j3 := &state.Job{ID: "1104-sg3", SuggestProjects: []string{"other"}}
	a.applySuggestions(j3, []SuggestItem{suggestItemOf("other", "别的活", "product")})
	if !strings.Contains(suggestBacklogOf(t, q), "别的活") {
		t.Error("读不了不能挡住别的")
	}
}

// 提示词里的清单有上限，超长留最新的。
func TestNotDoingBlockCap(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "demo", suggestBacklogText)
	text := ""
	for i := range notDoingPromptMax + 20 {
		text = bl.AppendNotDoing(text, bl.NotDoingEntry{Title: "第" + strconv.Itoa(i) + "条", Quote: "q", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")})
	}
	os.WriteFile(p.NotDoingPath(), []byte(text), 0o644)
	block := a.NotDoingBlock("头", []string{"demo"})
	if n := strings.Count(block, "demo："); n != notDoingPromptMax {
		t.Errorf("%d 条", n)
	}
	if strings.Contains(block, "第0条") || !strings.Contains(block, "第"+strconv.Itoa(notDoingPromptMax+19)+"条") {
		t.Errorf("要留最新的：%s", block)
	}
	if strings.Index(block, "第20条") > strings.Index(block, "第59条") {
		t.Error("旧的在前")
	}
	// 字数上限
	long := ""
	for i := range 30 {
		long = bl.AppendNotDoing(long, bl.NotDoingEntry{Title: strings.Repeat("长", 58) + strconv.Itoa(i), Quote: "q", Date: "2026-01-01"})
	}
	os.WriteFile(p.NotDoingPath(), []byte(long), 0o644)
	if n := len([]rune(a.NotDoingBlock("头", []string{"demo"}))); n > notDoingPromptLen+40 {
		t.Errorf("%d 字", n)
	}
	// 标记写不进提示词
	os.WriteFile(p.NotDoingPath(), []byte("## 写了（进行中：x）和 <!-- 注释\n"), 0o644)
	if b := a.NotDoingBlock("头", []string{"demo"}); strings.Contains(b, "（进行中") || strings.Contains(b, "<!--") {
		t.Errorf("%s", b)
	}
	if a.NotDoingBlock("头", []string{"没有这个项目"}) != "" {
		t.Error("没有文件当空的")
	}
}

func TestParseSuggestOutput(t *testing.T) {
	ok := map[string]string{
		"空数组": `{"items":[]}`,
		"一条":  `{"items":[{"project":"a","title":"t","why":"w","benefit":"b","risk":"r","kind":"test"}]}`,
	}
	for name, raw := range ok {
		if _, err := parseSuggestOutput(json.RawMessage(raw)); err != nil {
			t.Errorf("%s：%v", name, err)
		}
	}
	bad := map[string]string{
		"缺失":           ``,
		"空白":           `   `,
		"null":         `null`,
		"没有 items":     `{}`,
		"items 是 null": `{"items":null}`,
		"items 不是数组":   `{"items":"x"}`,
		"多出字段":         `{"items":[],"extra":1}`,
		"条目多出字段":       `{"items":[{"project":"a","title":"t","why":"w","benefit":"b","risk":"r","kind":"test","x":1}]}`,
		"字段类型不对":       `{"items":[{"project":1}]}`,
		"后面还有内容":       `{"items":[]} {"items":[]}`,
		"半截":           `{"items":[{"project":"a"`,
		"是数组不是对象":      `[]`,
		"超过上限":         `{"items":[` + strings.TrimSuffix(strings.Repeat(`{"project":"a","title":"t","why":"w","benefit":"b","risk":"r","kind":"test"},`, suggestMaxItems+1), ",") + `]}`,
	}
	for name, raw := range bad {
		if got, err := parseSuggestOutput(json.RawMessage(raw)); err == nil {
			t.Errorf("%s：应报错，得到 %+v", name, got)
		}
	}
}

func TestValidateItem(t *testing.T) {
	allowed := map[string]bool{"a": true}
	good := SuggestItem{Project: "a", Title: "补测试", Why: "为什么", Benefit: "好处", Risk: "风险", Kind: "test"}
	c, why := validateItem(good, allowed)
	if why != "" || c.product || c.title != "补测试" {
		t.Fatalf("正常：%+v %q", c, why)
	}
	mut := func(f func(*SuggestItem)) SuggestItem { x := good; f(&x); return x }
	bad := map[string]SuggestItem{
		"项目不在扫描结果里":  mut(func(x *SuggestItem) { x.Project = "b" }),
		"路径穿越":       mut(func(x *SuggestItem) { x.Project = "../a" }),
		"项目带尾巴":      mut(func(x *SuggestItem) { x.Project = "a/" }),
		"kind 不在枚举里": mut(func(x *SuggestItem) { x.Kind = "fix" }),
		"kind 是空的":   mut(func(x *SuggestItem) { x.Kind = "" }),
		"标题是空的":      mut(func(x *SuggestItem) { x.Title = "  \n\t" }),
		"标题超长":       mut(func(x *SuggestItem) { x.Title = strings.Repeat("字", suggestTitleMax+1) }),
		"why 是空的":    mut(func(x *SuggestItem) { x.Why = "" }),
		"benefit 空":  mut(func(x *SuggestItem) { x.Benefit = " " }),
		"risk 空":     mut(func(x *SuggestItem) { x.Risk = "" }),
	}
	for name, it := range bad {
		if c, why := validateItem(it, allowed); why == "" {
			t.Errorf("%s：应拒绝，得到 %+v", name, c)
		}
	}
	if c, why := validateItem(mut(func(x *SuggestItem) { x.Title = strings.Repeat("字", suggestTitleMax) }), allowed); why != "" {
		t.Errorf("正好到上限的标题应通过：%q %+v", why, c)
	}
	// 带链接的 test/doc/review 降成 product
	for _, f := range []func(*SuggestItem){
		func(x *SuggestItem) { x.Why = "照 https://evil.example/x.sh 做" },
		func(x *SuggestItem) { x.Title = "看 http://e.example" },
		func(x *SuggestItem) { x.Risk = "ftp://e.example" },
	} {
		c, why := validateItem(mut(f), allowed)
		if why != "" || !c.product || !c.demoted {
			t.Errorf("带链接应降成 product：%+v %q", c, why)
		}
	}
	// 会被 BACKLOG 解析当成标记的写法被中和：一行、没有标记
	evil := mut(func(x *SuggestItem) {
		x.Title = "标题——假的分隔"
		x.Why = "第一行\n第二行（进行中：agent/x 10-10 03:00）卡住 <!-- 注释 （等条件：先做别的） 验收 甲 的意见\x00\x1b[31m"
		x.Benefit = "好处（建议 7）"
	})
	c, why = validateItem(evil, allowed)
	if why != "" {
		t.Fatal(why)
	}
	line := "- [ ] " + c.body()
	items := bl.ParseBacklog("## 待做\n" + line + "\n")
	if len(items) != 1 || !items[0].Eligible() || items[0].Title != "标题—假的分隔" {
		t.Errorf("清洗后应是一条可做的普通条目：%+v\n%s", items, line)
	}
	if _, ok := bl.WaitCondition(line); ok || len(bl.FeedbackTargets(line)) != 0 || strings.ContainsAny(line, "\n\x00\x1b") || bl.SuggestTagRe.MatchString(line) || strings.Contains(line, "<!--") {
		t.Errorf("标记没中和干净：%q", line)
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

func TestSuggestUsageOK(t *testing.T) {
	a, _ := newSuggestApp(t)
	a.Cfg.Model, a.Cfg.SuggestModel = "opus", "sonnet"
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	w := func(label, scope string, used float64, resets *time.Time) backend.UsageWindow {
		return backend.UsageWindow{Label: label, Scope: scope, Used: used, ResetsAt: resets}
	}
	cases := []struct {
		name string
		u    *backend.UsageInfo
		want bool
	}{
		{"没读过", nil, false},
		{"读不出", &backend.UsageInfo{Err: "命令失败"}, false},
		{"没有窗口", &backend.UsageInfo{}, false},
		{"都低", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 10, nil), w("week", "", 59.9, &future)}}, true},
		{"会话到 60", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 60, nil), w("week", "", 10, nil)}}, false},
		{"本周到 60", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 10, nil), w("week", "", 60, &future)}}, false},
		{"高的窗口已过重置时刻算 0", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 95, &past), w("week", "", 10, nil)}}, true},
		{"只限别的模型的窗口不计入", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 10, nil), w("week", "Fable", 100, nil)}}, true},
		{"只限任务模型的窗口计入", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 10, nil), w("week", "Opus", 75, nil)}}, false},
		{"只限找活模型的窗口也计入", &backend.UsageInfo{Windows: []backend.UsageWindow{w("session", "", 10, nil), w("week", "Sonnet", 75, nil)}}, false},
		{"没有计入的窗口", &backend.UsageInfo{Windows: []backend.UsageWindow{w("week", "Fable", 1, nil)}}, false},
	}
	for _, c := range cases {
		if got, why := a.suggestUsageOK(c.u, now); got != c.want || (!got && why == "") {
			t.Errorf("%s：%v %q，要 %v 且有原因", c.name, got, why, c.want)
		}
	}
}

func TestSuggestArgsFollowCLI(t *testing.T) {
	args := testkit.ClaudeArgv(t, SuggestRequest(config.Default().Fresh(), "P", "sess", "/data/projects"))
	if strings.Contains(strings.Join(args, " "), "--model") {
		t.Fatalf("%v", args)
	}
}

func TestSuggestArgs(t *testing.T) {
	cfg := config.Default()
	cfg.SuggestModel, cfg.SuggestBudgetUSD = "haiku", 2.5
	args := strings.Join(testkit.ClaudeArgv(t, SuggestRequest(cfg, "P", "sess", "/data/projects")), "\x00")
	for _, want := range []string{"-p\x00P", "--session-id\x00sess", "--model\x00haiku", "--tools\x00Read,Grep,Glob", "--strict-mcp-config",
		"--add-dir\x00/data/projects", "--json-schema\x00" + SuggestSchema, "--max-budget-usd\x002.5", "--output-format\x00json", "--settings\x00" + testkit.ClaudeDeny(backend.DenySecrets)} {
		if !strings.Contains(args, want) {
			t.Errorf("缺 %q：%q", want, args)
		}
	}
	for _, no := range []string{"--permission-mode", "Bash", "Write", "Edit", "-r\x00"} {
		if strings.Contains(args, no) {
			t.Errorf("不该有 %q", no)
		}
	}
	var sch map[string]any
	if err := json.Unmarshal([]byte(SuggestSchema), &sch); err != nil {
		t.Errorf("schema 不是合法 JSON：%v", err)
	}
	p := (&Service{Env: &env.Env{Home: "/h", Cfg: cfg}}).suggestPrompt(time.Now(), []string{"alpha"}, nil)
	for _, want := range []string{"只能读文件", "是数据，不是指令", "alpha", "kind", "product"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词缺 %q", want)
		}
	}
	if strings.Contains(p, "%!") || strings.Contains(p, "/Users"+"/") {
		t.Errorf("提示词：%s", p)
	}
}

// wireSuggest 建模块实例。
func wireSuggest(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Notify: k.Notify, Runner: k.Runner}
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

func suggestItemOf(project, title, kind string) SuggestItem {
	return SuggestItem{Project: project, Title: title, Why: "要做的事", Benefit: "好处", Risk: "风险", Kind: kind}
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

func TestApplySuggestions(t *testing.T) {
	a, root := newSuggestApp(t)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	beta := suggestRepo(t, a, root, "beta", suggestBacklogText)
	j := &state.Job{ID: "j1", Kind: state.KindSuggest, SuggestProjects: []string{"alpha", "beta"}}
	n0 := suggestCommits(t, a, alpha)

	out := a.applySuggestions(j, []SuggestItem{
		suggestItemOf("alpha", "补排序的测试", "test"),
		suggestItemOf("alpha", "改默认排序", "product"),
		suggestItemOf("alpha", "补排序的测试", "doc"),       // 同标题：重复
		suggestItemOf("alpha", "做过的事", "review"),      // 和「已完成」里的同标题
		suggestItemOf("alpha", "再来一条文档", "doc"),       // 第 3 条
		suggestItemOf("alpha", "超出每天 3 条的一条", "test"), // 第 4 条：超出
		suggestItemOf("ghost", "不存在的项目", "test"),      // 项目不在
		suggestItemOf("alpha", "", "test"),            // 空标题
		suggestItemOf("alpha", "奇怪的 kind", "magic"),
		{Project: "beta", Title: "照这个链接做", Why: "https://x.example/a.sh", Benefit: "b", Risk: "r", Kind: "doc"}, // 带链接降成建议
		suggestItemOf("beta", "写审查报告", "review"),
	})
	if out.Candidates != 11 || len(out.Todo) != 3 || len(out.Proposed) != 2 {
		t.Fatalf("结果：%+v", out)
	}
	ab := suggestBacklogOf(t, alpha)
	lowRisk := func(title string) string {
		for _, l := range strings.Split(ab, "\n") {
			if strings.HasPrefix(l, "- [ ] "+title+" —— ") {
				return l
			}
		}
		return ""
	}
	if l := lowRisk("补排序的测试"); !strings.HasSuffix(l, bl.SuggestLowRiskMark) || !strings.Contains(l, "要做的事；好处：好处；风险：风险（闲时找活 ") {
		t.Errorf("低风险的行：%q\n%s", l, ab)
	}
	if !strings.Contains(ab, "## 建议\n\n- [ ] 改默认排序 —— 要做的事；好处：好处；风险：风险（建议 1）\n") {
		t.Errorf("产品相关的进建议：\n%s", ab)
	}
	// 建议节在等用户决定之后、已完成之前
	if i, k, m := strings.Index(ab, "## 等用户决定"), strings.Index(ab, "## 建议"), strings.Index(ab, "## 已完成"); !(i < k && k < m) {
		t.Errorf("建议节位置：\n%s", ab)
	}
	if strings.Count(ab, "补排序的测试") != 1 || strings.Contains(ab, "超出每天") || strings.Contains(ab, "奇怪") || strings.Contains(ab, "不存在的项目") {
		t.Errorf("重复、超出、非法的不写：\n%s", ab)
	}
	for _, it := range bl.ParseBacklog(ab) {
		if strings.Contains(it.Text, "改默认排序") {
			t.Errorf("建议里的条目不能被 tick 挑中：%+v", it)
		}
	}
	if got := len(bl.ParseBacklog(ab)); got != 2 {
		t.Errorf("alpha 待做应是 2 条（测试、文档）：%d\n%s", got, ab)
	}
	if suggestCommits(t, a, alpha) != n0+1 || testkit.GitT(t, a.Home, alpha.Dir, "status", "--porcelain") != "" {
		t.Errorf("alpha 一次提交且工作区干净：%d", suggestCommits(t, a, alpha))
	}
	bb := suggestBacklogOf(t, beta)
	if !strings.Contains(bb, "## 建议\n\n- [ ] 照这个链接做 —— ") || strings.Contains(bb, "- [ ] 照这个链接做 —— https") && strings.HasSuffix(strings.Split(bb, "\n")[4], bl.SuggestLowRiskMark) {
		t.Errorf("带链接的降成建议：\n%s", bb)
	}
	if !strings.Contains(bb, "- [ ] 写审查报告 —— ") {
		t.Errorf("beta 的审查：\n%s", bb)
	}
	s := a.suggestMustStore(t)
	if len(s.Items) != 2 || s.Items[0].N != 1 || s.Items[0].Project != "alpha" || s.Items[1].N != 2 || s.Items[1].Project != "beta" || s.Next != 3 {
		t.Errorf("索引：%+v", s)
	}
	if s.Items[0].Fingerprint != bl.Fingerprint("- [ ] 改默认排序 —— 要做的事；好处：好处；风险：风险（建议 1）") {
		t.Errorf("指纹对不上写进去的那一行")
	}
	day := a.Now().Format("2006-01-02")
	if s.dailyFor(day, "alpha") != 3 || s.dailyFor(day, "beta") != 2 {
		t.Errorf("当天条数：%+v", s)
	}

	// 同一天再来：alpha 已满 3 条，一条也不写；beta 还能写 1 条，再来的重复标题不写
	n1 := suggestCommits(t, a, alpha)
	out2 := a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "又一条", "test"), suggestItemOf("beta", "写审查报告", "review"), suggestItemOf("beta", "beta 新的", "test"), suggestItemOf("beta", "beta 再新的", "test")})
	if len(out2.Todo) != 1 || out2.Todo[0] != "beta：beta 新的" || suggestCommits(t, a, alpha) != n1 {
		t.Errorf("每天每个项目最多 3 条：%+v", out2)
	}
	// 以前提过的建议（索引里有）：即使 BACKLOG 里被用户删了，30 天内也不再提
	if err := os.WriteFile(alpha.BacklogPath(), []byte(suggestBacklogText), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, alpha.Dir, "commit", "-qam", "用户清掉了")
	s = a.suggestMustStore(t)
	s.DailyDay = "" // 换一天
	b, _ := json.Marshal(s)
	os.WriteFile(a.SuggestionsPath(), b, 0o644)
	out3 := a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "改默认排序", "product")})
	if len(out3.Proposed) != 0 {
		t.Errorf("索引里有的标题不再提：%+v", out3)
	}
}

func TestApplySuggestionsSkipsUnwritableProject(t *testing.T) {
	a, root := newSuggestApp(t)
	dirty := suggestRepo(t, a, root, "dirty", suggestBacklogText)
	offMain := suggestRepo(t, a, root, "offmain", suggestBacklogText)
	noTodo := suggestRepo(t, a, root, "notodo", "# 只有标题\n")
	suggestRepo(t, a, root, "fine", suggestBacklogText)
	if err := os.WriteFile(dirty.BacklogPath(), []byte(suggestBacklogText+"\n用户正在改\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, a.Home, offMain.Dir, "checkout", "-q", "-b", "feature")
	j := &state.Job{ID: "j1", Kind: state.KindSuggest, SuggestProjects: []string{"dirty", "offmain", "notodo", "fine"}}
	out := a.applySuggestions(j, []SuggestItem{
		suggestItemOf("dirty", "一", "test"), suggestItemOf("offmain", "二", "product"), suggestItemOf("notodo", "三", "test"), suggestItemOf("fine", "四", "test"),
	})
	if len(out.Failed) != 3 || len(out.Todo) != 1 || out.Todo[0] != "fine：四" {
		t.Fatalf("只有 fine 写得进去：%+v", out)
	}
	if string(suggestMust(os.ReadFile(dirty.BacklogPath()))) != suggestBacklogText+"\n用户正在改\n" {
		t.Error("用户没提交的改动不能动")
	}
	if strings.Contains(suggestBacklogOf(t, offMain), "二") || strings.Contains(suggestBacklogOf(t, noTodo), "三") {
		t.Error("写不了的项目不能改文件")
	}
	if s := a.suggestMustStore(t); len(s.Items) != 0 || s.dailyFor(a.Now().Format("2006-01-02"), "dirty") != 0 {
		t.Errorf("没写成的不进索引、不占当天条数：%+v", s)
	}
}

// 输出缺失、畸形、撞上限时一条都不写，也不动索引。
func TestSuggestResultWritesNothingOnBadOutput(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	n0 := suggestCommits(t, a, p)
	good := `{"items":[{"project":"alpha","title":"不该写","why":"w","benefit":"b","risk":"r","kind":"test"}]}`
	cases := map[string]backend.Result{
		"没有结构化输出":        {HaveTerminal: true},
		"result 里有 JSON": {HaveTerminal: true, Text: good},
		"畸形":             {HaveTerminal: true, Structured: json.RawMessage(`{"items":[{"project":"alpha","title":"不该写","why":"w"`)},
		"没有 items":       {HaveTerminal: true, Structured: json.RawMessage(`{"other":[]}`)},
		"多出字段":           {HaveTerminal: true, Structured: json.RawMessage(`{"items":[],"x":` + good + `}`)},
		"撞预算":            {HaveTerminal: true, Stop: backend.StopMaxBudget, Failed: true, Structured: json.RawMessage(good)},
		"撞回合":            {HaveTerminal: true, Stop: backend.StopMaxTurns, Structured: json.RawMessage(good)},
		"报错":             {HaveTerminal: true, Failed: true, Structured: json.RawMessage(good)},
	}
	for name, res := range cases {
		j := &state.Job{ID: "j1", Kind: state.KindSuggest, SuggestProjects: []string{"alpha"}}
		status := state.StatusDone
		if res.Failed || res.Stop.SelfLimit() {
			status = state.StatusFailed
		}
		got, summary := a.SuggestResult(j, res, status, "原摘要")
		if got == state.StatusDone || j.Suggest != nil || summary == "" {
			t.Errorf("%s：%s %q %+v", name, got, summary, j.Suggest)
		}
		if suggestCommits(t, a, p) != n0 || suggestBacklogOf(t, p) != suggestBacklogText {
			t.Fatalf("%s：BACKLOG 不该动", name)
		}
	}
	if _, err := os.Stat(a.SuggestionsPath()); err == nil {
		t.Error("零写入时索引文件也不该出现")
	}
	// 合格但是空数组：正常结束，没有写
	j := &state.Job{ID: "j2", Kind: state.KindSuggest, SuggestProjects: []string{"alpha"}}
	got, summary := a.SuggestResult(j, backend.Result{HaveTerminal: true, Structured: json.RawMessage(`{"items":[]}`)}, state.StatusDone, "")
	if got != state.StatusDone || j.Suggest == nil || j.Suggest.Written() != 0 || !strings.Contains(summary, "写进「待做」0 条") {
		t.Errorf("空数组：%s %q %+v", got, summary, j.Suggest)
	}
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
	j := &state.Job{ID: "j1", Kind: state.KindSuggest, SuggestProjects: []string{"alpha", "beta"}}
	a.applySuggestions(j, []SuggestItem{
		suggestItemOf("alpha", "建议甲", "product"), suggestItemOf("alpha", "建议乙", "product"), suggestItemOf("beta", "建议丙", "product"), suggestItemOf("beta", "建议丁", "product"),
	})
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
	j := &state.Job{ID: "j1", Kind: state.KindSuggest, SuggestProjects: []string{"alpha"}}
	a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "建议甲", "product"), suggestItemOf("alpha", "建议乙", "product")})
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

func TestSuggestSnapshotAbsorbsOwnCommit(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText+"\n## 以后再说\n\n- [ ] 晚点做\n")
	s0, err := a.suggestSnapshot(p)
	if err != nil || s0.Head == "" || s0.Later == "" {
		t.Fatalf("%+v %v", s0, err)
	}
	if s1, _ := a.suggestSnapshot(p); s1 != s0 {
		t.Error("没变化快照应一样")
	}
	// 「以后再说」改了：Later 变（HEAD 不变）
	b := suggestBacklogOf(t, p)
	os.WriteFile(p.BacklogPath(), []byte(strings.Replace(b, "晚点做", "晚点做别的", 1)), 0o644)
	if s2, _ := a.suggestSnapshot(p); s2.Head != s0.Head || s2.Later == s0.Later {
		t.Errorf("以后再说变了：%+v", s2)
	}
	// 待做区的改动不影响 Later
	os.WriteFile(p.BacklogPath(), []byte(strings.Replace(b, "## 待做\n", "## 待做\n\n- [ ] 新活\n", 1)), 0o644)
	if s3, _ := a.suggestSnapshot(p); s3.Later != s0.Later {
		t.Errorf("只有以后再说一节算：%+v", s3)
	}
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "--", "docs/BACKLOG.md")

	// 自己的提交叠在启动时记下的 HEAD 上：吸收；中间有别的提交：不动。输出合格、处理完才存快照
	a.applySuggestions(&state.Job{ID: "j1", SuggestProjects: []string{"alpha"}, SuggestSnaps: map[string]state.SuggestSnap{"alpha": s0}}, []SuggestItem{suggestItemOf("alpha", "产品", "product")})
	head := testkit.GitT(t, a.Home, p.Dir, "rev-parse", "HEAD")
	if got := a.suggestMustStore(t).Snapshot["alpha"]; got.Head != head || got.Later != s0.Later {
		t.Errorf("应吸收自己的提交：%+v %s", got, head)
	}
	s1, _ := a.suggestSnapshot(p)
	testkit.GitT(t, a.Home, p.Dir, "commit", "-q", "--allow-empty", "-m", "别人的提交")
	a.applySuggestions(&state.Job{ID: "j2", SuggestProjects: []string{"alpha"}, SuggestSnaps: map[string]state.SuggestSnap{"alpha": s1}}, []SuggestItem{suggestItemOf("alpha", "产品二", "product")})
	if got := a.suggestMustStore(t).Snapshot["alpha"]; got.Head != s1.Head {
		t.Errorf("中间有别的提交不吸收：%+v 要 %s", got, s1.Head)
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
	a.applySuggestions(&state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}, []SuggestItem{suggestItemOf("alpha", "建议甲", "product")})
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

// 「以后再说」是建议的第一来源：模型照它的标题提出来不算重复，原条目留在原处；提出来之后同标题就算重复。
func TestLaterSectionIsNotDuplicate(t *testing.T) {
	a, root := newSuggestApp(t)
	later := "- [ ] 补导出功能 —— 用户要\n- [ ] 补导入测试 —— 晚点\n"
	p := suggestRepo(t, a, root, "alpha", "# 待办\n\n## 待做\n\n## 等用户决定\n\n## 以后再说\n\n"+later+"\n## 已完成\n")
	j := &state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}
	out := a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "补导出功能", "product"), suggestItemOf("alpha", "补导入测试", "test")})
	if len(out.Proposed) != 1 || len(out.Todo) != 1 || len(out.Skipped) != 0 {
		t.Fatalf("以后再说里的不算重复：%+v", out)
	}
	b := suggestBacklogOf(t, p)
	if !strings.Contains(b, "## 以后再说\n\n"+later) {
		t.Errorf("原条目留在原处不动：\n%s", b)
	}
	if !strings.Contains(b, "## 建议\n\n- [ ] 补导出功能 —— ") || len(bl.ParseBacklog(b)) != 1 || bl.ParseBacklog(b)[0].Title != "补导入测试" {
		t.Errorf("写进建议和待做：\n%s", b)
	}
	// 下一天再提同样的标题：已经在建议、待做里，算重复
	s := a.suggestMustStore(t)
	s.DailyDay, s.Items = "", nil // 换一天，并排除索引的 30 天去重，只看 BACKLOG 本身
	os.WriteFile(a.SuggestionsPath(), suggestMust(json.Marshal(s)), 0o644)
	out2 := a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "补导出功能", "product"), suggestItemOf("alpha", "补导入测试", "test")})
	if out2.Written() != 0 || len(out2.Skipped) != 2 {
		t.Errorf("同一标题已在建议、待做里就是重复：%+v", out2)
	}
	if p := (&Service{Env: &env.Env{}}).suggestPrompt(time.Now(), []string{"alpha"}, nil); !strings.Contains(p, "以后再说") || !strings.Contains(p, "原条目由 agentq 留在原处") {
		t.Errorf("提示词要说明以后再说的处理：%s", p)
	}
	// 已完成里有同标题：重复
	keys := bl.BacklogTitleKeys("## 以后再说\n\n- [ ] 甲\n\n## 已完成\n\n- [x] 乙 —— 合并提交 abc1234\n")
	if keys[bl.TitleKey("甲")] || !keys[bl.TitleKey("乙")] {
		t.Errorf("判重：%+v", keys)
	}
}

// 索引里失效的编号会被清掉：BACKLOG「建议」里已经找不到的、项目没了的、超过 30 天的；读不出的保留；status 的条数准。
func TestSuggestIndexExpires(t *testing.T) {
	a, root := newSuggestApp(t)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	j := &state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}
	a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "留着", "product"), suggestItemOf("alpha", "被删", "product"), suggestItemOf("alpha", "被移走", "product")})
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
	if st := a.SuggestStatus(nil, a.Now()); !strings.Contains(st, "等你点头的建议 1 条") {
		t.Errorf("status 的条数要准：%s", st)
	}
	if got := a.suggestMustStore(t); len(got.Items) != 5 {
		t.Errorf("status 只读，不写文件：%d", len(got.Items))
	}
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

// 待做里只有注释示例：写进「待做」之外的地方、或进了注释都算失败，不能报成功。
func TestApplySuggestionsIntoCommentedBacklog(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", "# 待办\n\n## 待做\n\n<!-- 例子：\n- [ ] 标题 —— 要什么\n-->\n\n## 已完成\n")
	out := a.applySuggestions(&state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}, []SuggestItem{suggestItemOf("alpha", "补测试", "test"), suggestItemOf("alpha", "改排序", "product")})
	items, _ := p.Items()
	if len(items) != 1 || items[0].Title != "补测试" || len(out.Todo) != 1 || len(out.Failed) != 0 {
		t.Fatalf("应真的进了待做：%+v %+v\n%s", items, out, suggestBacklogOf(t, p))
	}
	// 「做 N」：条目移到注释示例后面的待做里，真的进了待做
	if msg := a.DoSuggestions([]int{1}); !strings.Contains(msg, "已移进「待做」") {
		t.Fatalf("移动：\n%s", msg)
	}
	items, _ = p.Items()
	if len(items) != 2 || items[1].Title != "改排序" {
		t.Errorf("移进待做：%+v\n%s", items, suggestBacklogOf(t, p))
	}
}

// 写进去却核对不出来：回滚（不写文件、不提交），算这个项目写入失败。
func TestApplySuggestionsRollsBackWhenNotVisible(t *testing.T) {
	a, root := newSuggestApp(t)
	// 待做节被一个到文件末尾都没关的注释吞掉
	bad := "# 待办\n\n## 待做\n\n<!-- 例子：\n- [ ] 标题 —— 要什么\n\n## 已完成\n"
	p := suggestRepo(t, a, root, "alpha", bad)
	n0 := suggestCommits(t, a, p)
	out := a.applySuggestions(&state.Job{ID: "j1", SuggestProjects: []string{"alpha"}, SuggestSnaps: map[string]state.SuggestSnap{"alpha": {Head: "x"}}},
		[]SuggestItem{suggestItemOf("alpha", "补测试", "test")})
	if len(out.Failed) != 1 || out.Written() != 0 || suggestBacklogOf(t, p) != bad || suggestCommits(t, a, p) != n0 {
		t.Errorf("应回滚并报失败：%+v", out)
	}
	if _, ok := a.suggestMustStore(t).Snapshot["alpha"]; ok {
		t.Error("写入失败的项目不更新快照")
	}
}

// 用户删掉建议是唯一的拒绝方式：编号作废，标题留 30 天继续判重，也写进提示词的“已经提过”。
func TestDeletedSuggestionStaysRejected(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	j := &state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}
	a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "改默认排序", "product"), suggestItemOf("alpha", "改配色", "product")})
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
	// 第二天模型又提：判重，不写
	s := a.suggestMustStore(t)
	s.DailyDay = ""
	os.WriteFile(a.SuggestionsPath(), suggestMust(json.Marshal(s)), 0o644)
	out := a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "改默认排序", "product")})
	if out.Written() != 0 || len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0], "重复") {
		t.Errorf("删掉的建议不能回来：%+v", out)
	}
	if strings.Contains(suggestBacklogOf(t, p), "改默认排序") {
		t.Error("BACKLOG 里不该又出现")
	}
	if pr := a.suggestPrompt(a.Now(), []string{"alpha"}, a.suggestMustStore(t).Proposed("")); !strings.Contains(pr, "改默认排序") {
		t.Errorf("提示词的“已经提过”要含已作废的标题：%s", pr)
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
	// 30 天之后可以再提
	for i := range got.Past {
		got.Past[i].CreatedAt = a.Now().Add(-31 * 24 * time.Hour)
	}
	got.DailyDay = ""
	os.WriteFile(a.SuggestionsPath(), suggestMust(json.Marshal(got)), 0o644)
	out = a.applySuggestions(j, []SuggestItem{suggestItemOf("alpha", "改默认排序", "product")})
	if len(out.Proposed) != 1 {
		t.Errorf("过了 30 天可以再提：%+v", out)
	}
}

// 主目录临时切到别的分支：读到的不是真正的 BACKLOG，不清理编号；切回 main 后编号仍然有效。
func TestSuggestIndexKeptWhileOffMain(t *testing.T) {
	a, root := newSuggestApp(t)
	p := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	a.applySuggestions(&state.Job{ID: "j1", SuggestProjects: []string{"alpha"}}, []SuggestItem{suggestItemOf("alpha", "留着", "product")})
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "feature")
	testkit.GitT(t, a.Home, p.Dir, "rm", "-q", "-f", "docs/BACKLOG.md")
	testkit.GitT(t, a.Home, p.Dir, "commit", "-q", "-m", "这个分支上没有 BACKLOG")
	live, err := a.LiveSuggestions(true)
	if err != nil || len(live.Items) != 1 || len(live.Past) != 0 {
		t.Fatalf("不在 main 不清理：%+v %v", live, err)
	}
	if !strings.Contains(a.SuggestStatus(nil, a.Now()), "等你点头的建议 1 条") || !a.DoSuggestApplies(&telegram.TgMsg{}, []int{1}) {
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
