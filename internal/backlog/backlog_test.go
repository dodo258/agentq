package backlog

import (
	"strings"
	"testing"
	"time"
)

func TestLowRiskItem(t *testing.T) {
	for text, want := range map[string]bool{
		"（低风险）补测试 —— 给 x 加用例":                                true,
		"补测试（低风险） —— 给 x 加用例":                                true,
		"补测试（低风险）":                                           true,
		"（低风险）补测试（进行中：agent/x 10-10）":                        true,
		"补测试（低风险）（进行中：agent/x 10-10）":                        true,
		"补测试 —— 给 x 加用例（闲时找活 11-04）（低风险）":                    true,
		"补测试 —— 给 x 加用例（闲时找活 11-04）（低风险）（进行中：agent/x 10-10）": true,
		"补文档 —— 说明（低风险）标记怎么用":                                false,
		"改解析（低风险）标记 —— 只认固定写法":                               false,
		"补测试(低风险) —— 半角括号不算":                                 false,
		"低风险补测试 —— 没括号不算":                                    false,
		"补测试 —— 低风险":                                         false,
		"这个活风险很低，（低风险）这几个字在正文 —— 真的":                         false,
		"": false,
	} {
		if got := LowRiskItem(text); got != want {
			t.Errorf("%q：%v", text, got)
		}
	}
}

func TestNotDoingFormat(t *testing.T) {
	text := AppendNotDoing("", NotDoingEntry{"不做夜间模式", "用户回「不做 2」", "2026-11-04"})
	if !strings.HasPrefix(text, "# 不做清单") || !strings.Contains(text, "## 不做夜间模式\n- 原话：用户回「不做 2」\n- 日期：2026-11-04\n") {
		t.Errorf("%s", text)
	}
	text = AppendNotDoing(text, NotDoingEntry{"第二条", "原话二", "2026-11-05"})
	got := ParseNotDoing(text + "\n## 手写的只有标题\n随便写点\n- 原话没冒号\n\n##不是标题\n")
	if len(got) != 3 || got[0].Quote != "用户回「不做 2」" || got[1].Date != "2026-11-05" || got[2].Title != "手写的只有标题" || got[2].Quote != "" {
		t.Errorf("%+v", got)
	}
	if len(ParseNotDoing("")) != 0 || len(ParseNotDoing("乱七八糟\x00\n## \n")) != 0 {
		t.Error("坏内容当没有")
	}
}

func TestMarkerBranch(t *testing.T) {
	cases := map[string]string{
		"甲（进行中：agent/1011-msg-layout 2026-10-11 14:29）": "agent/1011-msg-layout",
		"甲（进行中: agent/x）":                               "agent/x",
		"甲（进行中）":                                        "",
		"甲（进行中：--upload-pack=evil 10-10）":               "",
		"甲（进行中：main 10-10）":                             "",
		"甲（进行中：master 10-10）":                           "",
		"甲（进行中：2026-10-11 14:29）":                       "",
		"甲（进行中：10-11 14:29）":                            "",
		"甲（进行中：2026-10-11T14:29）":                       "",
		"正文提到进行中":                                       "",
	}
	for in, want := range cases {
		if got := MarkerBranch(in); got != want {
			t.Errorf("%s：得到 %q，应为 %q", in, got, want)
		}
	}
}

func TestMarkerTime(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.Local)
	at := func(y, mo, d, h, mi int) time.Time { return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.Local) }
	ok := map[string]time.Time{
		"（进行中：agent/x 2026-02-11 14:29）":        at(2026, 2, 11, 14, 29),
		"（进行中：agent/x 2026-02-11T14:29）":        at(2026, 2, 11, 14, 29),
		"（进行中：agent/x 2026-02-11 14:29:30）":     at(2026, 2, 11, 14, 29),
		"（进行中：agent/x 02-11 14:29）":             at(2026, 2, 11, 14, 29),
		"（进行中：agent/x 10-10 03:00）":             at(2025, 10, 10, 3, 0),  // 不写年份、今年还没到 → 去年
		"（进行中：agent/x 2026-02-11）":              at(2026, 2, 11, 23, 59), // 只有日期按当天最晚算（宁可多等）
		"（进行中：agent/1011-x-2026-01-01-y 14:29）": {},                      // 分支名里的日期不算：下面单独断言
	}
	for in, want := range ok {
		got, found := MarkerTime(in, now)
		if want.IsZero() {
			if found {
				t.Errorf("%s 不该解析出时间：%v", in, got)
			}
			continue
		}
		if !found || !got.Equal(want) {
			t.Errorf("%s：得到 %v %v，应为 %v", in, got, found, want)
		}
	}
	for _, in := range []string{"（进行中）", "（进行中：agent/x）", "（进行中：agent/x 13-45 99:99）", "没有标记 2026-01-01 10:00", "（进行中：agent/x 2026-13-45 10:00）"} {
		if got, found := MarkerTime(in, now); found {
			t.Errorf("%s 不该解析出时间：%v", in, got)
		}
	}
}

func TestEditTodoLine(t *testing.T) {
	line := "- [ ] 甲（进行中：agent/x 10-10 03:00）"
	it := ParseBacklog("## 待做\n" + line + "\n")[0]
	text := "# t\n<!--\n" + line + "\n-->\n## 待做\r\n" + line + "\r\n- [ ] 乙\n## 已完成\n" + line + "\n"
	got, ok := EditTodoLine(text, it.Fingerprint, it.Text, func(l string) string { return l + "X" })
	want := "# t\n<!--\n" + line + "\n-->\n## 待做\r\n" + line + "X\r\n- [ ] 乙\n## 已完成\n" + line + "\n"
	if !ok || got != want {
		t.Fatalf("只应改待做节里的那一行、保留换行符：%v\n%q", ok, got)
	}
	if got, ok := EditTodoLine(text, it.Fingerprint, it.Text+"变了", func(l string) string { return "X" }); ok || got != text {
		t.Fatal("条目原文变了不该改")
	}
	if got, ok := EditTodoLine(text, "没有的指纹", it.Text, func(l string) string { return "X" }); ok || got != text {
		t.Fatal("指纹不在不该改")
	}
}

func TestInsertTodo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"## 待做\n\n- [ ] 甲\n- [ ] 乙\n  续行\n\n## 已完成\n- [x] 丙\n", "## 待做\n\n- [ ] 甲\n- [ ] 乙\nNEW\n  续行\n\n## 已完成\n- [x] 丙\n"},
		{"# 待办\n\n## 待做\n\n从上往下做。\n\n<!-- 模板 -->\n\n\n## 等用户决定\n", "# 待办\n\n## 待做\n\n从上往下做。\n\n<!-- 模板 -->\nNEW\n\n\n## 等用户决定\n"},
		{"## 待做\n- [ ] 甲", "## 待做\n- [ ] 甲\nNEW"},
	}
	for _, c := range cases {
		got, ok := InsertTodo(c.in, "NEW")
		if !ok || got != c.want {
			t.Errorf("insertTodo(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if _, ok := InsertTodo("## 已完成\n", "NEW"); ok {
		t.Errorf("没有待做节应返回 false")
	}
}

// HTML 注释里的模板示例不能被当成插入点：条目写进注释却报告成功，是最糟的结果。
func TestInsertTodoSkipsComments(t *testing.T) {
	line := "- [ ] 新条目 —— x"
	cases := map[string]struct{ in, want string }{
		"多行注释里有示例条目": {
			"# 待办\n\n## 待做\n\n<!-- 例子：\n- [ ] 标题 —— 要什么\n-->\n\n## 已完成\n",
			"# 待办\n\n## 待做\n\n<!-- 例子：\n- [ ] 标题 —— 要什么\n-->\n" + line + "\n\n## 已完成\n",
		},
		"真条目在注释前": {
			"## 待做\n\n- [ ] 真的\n<!--\n- [ ] 示例\n-->\n\n## 已完成\n",
			"## 待做\n\n- [ ] 真的\n" + line + "\n<!--\n- [ ] 示例\n-->\n\n## 已完成\n",
		},
		"真条目在注释后": {
			"## 待做\n\n<!--\n- [ ] 示例\n-->\n- [ ] 真的\n\n## 已完成\n",
			"## 待做\n\n<!--\n- [ ] 示例\n-->\n- [ ] 真的\n" + line + "\n\n## 已完成\n",
		},
		"单行注释（原有写法）": {
			"## 待做\n\n<!-- - [ ] 模板 -->\n\n## 已完成\n",
			"## 待做\n\n<!-- - [ ] 模板 -->\n" + line + "\n\n## 已完成\n",
		},
		"注释里有假的节标题": {
			"<!--\n## 待做\n-->\n## 待做\n\n- [ ] 真的\n\n## 已完成\n",
			"<!--\n## 待做\n-->\n## 待做\n\n- [ ] 真的\n" + line + "\n\n## 已完成\n",
		},
		"注释里有假的下一节标题": {
			"## 待做\n\n- [ ] 真的\n<!--\n## 已完成\n-->\n- [ ] 还是待做\n\n## 已完成\n",
			"## 待做\n\n- [ ] 真的\n<!--\n## 已完成\n-->\n- [ ] 还是待做\n" + line + "\n\n## 已完成\n",
		},
	}
	for name, c := range cases {
		got, ok := InsertTodo(c.in, line)
		if !ok || got != c.want {
			t.Errorf("%s：\n得到 %q (%v)\n要   %q", name, got, ok, c.want)
		}
		if items := ParseBacklog(got); len(items) == 0 || items[len(items)-1].Title != "新条目" {
			t.Errorf("%s：新条目没进待做：%+v", name, items)
		}
	}
	// 节里的注释一直没关：哪里都插不进去，返回 false（调用方不写）
	if got, ok := InsertTodo("## 待做\n\n<!--\n- [ ] 示例\n\n## 已完成\n", line); ok {
		t.Errorf("没关的注释应拒绝：%q", got)
	}
	// 核对：行不在待做里就报错
	if err := VerifyTodoPresent("## 待做\n\n<!--\n"+line+"\n-->\n", []string{line}); err == nil {
		t.Error("落进注释的应被核对出来")
	}
	if err := VerifyTodoPresent("## 待做\n\n"+line+"\n", []string{line}); err != nil {
		t.Error(err)
	}
}

// 模型写的待办文字不能破坏 BACKLOG 解析：写进去之后这一条可挑，后面的条目还解析得到。
func TestCleanTodoText(t *testing.T) {
	for _, bad := range []string{
		"修 A <!<!---- 注释",
		"修 B <!-- 注释",
		"修 C（进行中",
		"修 D（进行（进行中）中）",
		"修 E（进行中：x 10-10 03:00）",
		"修 F（卡住：等人）",
		"修 G (卡住)",
		"修 H —— 卡住",
		"修 I\n## 已完成\n- [ ] 伪造",
	} {
		text := "## 待做\n\n- [ ] " + CleanTodoText(bad, 800) + "\n- [ ] 后来的活\n\n## 已完成\n"
		items := ParseBacklog(text)
		if len(items) != 2 || !items[0].Eligible() || items[1].Title != "后来的活" {
			t.Errorf("%q → %+v", bad, items)
		}
	}
}

func TestParseBacklog(t *testing.T) {
	text := `# 待办

## 待做

从上往下做。

<!-- - [ ] 注释里的模板 —— 不算 -->
<!--
- [ ] 多行注释里的 —— 不算
-->
- [ ] 第一条 —— 要什么；验收：……
- [ ] 第二条（进行中：agent/1010-x 2026-10-10 09:00）
- [ ] 第三条 —— 试过两次（卡住：agent/1010-y 换了两次思路测试仍红）
  - [ ] 缩进的子项不算
- [x] 勾掉的不算
- [ ] 第四条 <!-- 行尾注释 --> —— 照常算

### 小标题不结束待做节
- [ ] 第五条

## 等用户决定
- [ ] 别的节不算

## 待做（第二个同名节也算）
- [ ] 第六条
## 已完成
- [ ] 不算
`
	items := ParseBacklog(text)
	var got []string
	for _, it := range items {
		got = append(got, it.Title)
	}
	want := []string{"第一条", "第二条", "第三条", "第四条 <!-- 行尾注释 -->", "第五条", "第六条"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("标题\n got %q\nwant %q", got, want)
	}
	if !items[1].InProgress || items[1].Eligible() {
		t.Errorf("第二条应为进行中、不可做：%+v", items[1])
	}
	if !items[2].Stuck || items[2].Eligible() {
		t.Errorf("第三条应为卡住、不可做：%+v", items[2])
	}
	if !items[0].Eligible() || items[0].Text != "第一条 —— 要什么；验收：……" {
		t.Errorf("第一条：%+v", items[0])
	}
}

// 卡住只认括号标记和行尾标记；正文提到这个词的条目照常可挑。
func TestStuckMarker(t *testing.T) {
	cases := []struct {
		text  string
		stuck bool
	}{
		{"甲 —— 验收：标了卡住的条目要跳过，正文提到的照常可挑", false},
		{"甲 —— 进程会卡住；验收：卡住时 status 有提示", false},
		{"甲 —— 别把「卡住」当标记", false},
		{"甲 —— 任务不再卡住（进行中：agent/1010-x 10-10 03:00）", false},
		{"甲 —— 查任务卡住", false},
		{"甲 —— 2026 卡住率", false},
		{"甲 —— 验收：（卡住时 status 有提示）", false},
		{"修 [卡住检测](docs/x.md) 的误报", false},
		{"修 claude -p 卡住", false},
		{"甲 —— x **卡住**", false},
		{"甲 —— x（卡住：agent/1010-x 测试两次没过）", true},
		{"甲 —— x（卡住）（进行中：agent/1010-x 10-10 03:00）", true},
		{"甲 —— x (卡住: agent/1010-x)", true},
		{"甲 —— x【卡住】", true},
		{"甲 —— x [卡住] 等会再看", true},
		{"甲 —— x（卡住 agent/1010-x）", true},
		{"甲 —— x；卡住。", true},
		{"甲 —— x —— 卡住", true},
		{"甲 —— x；卡住（进行中：agent/t-stuck 10-10 03:00）", true},
		{"卡住", true},
	}
	for _, c := range cases {
		items := ParseBacklog("## 待做\n- [ ] " + c.text + "\n")
		if len(items) != 1 {
			t.Fatalf("%q：解析出 %d 条", c.text, len(items))
		}
		if items[0].Stuck != c.stuck {
			t.Errorf("%q：Stuck=%v，应为 %v", c.text, items[0].Stuck, c.stuck)
		}
		if !items[0].InProgress && items[0].Eligible() == c.stuck {
			t.Errorf("%q：Eligible=%v", c.text, items[0].Eligible())
		}
	}
}

// 等条件只认括号紧挨「等条件」再接冒号的标记；正文顺带提到这个词不算。标题里不带标记。
func TestWaitCondition(t *testing.T) {
	cases := []struct {
		text, cond string
		ok         bool
	}{
		{"v4 发布 —— 推荐发布（等条件：前面 7 条都合并）", "前面 7 条都合并", true},
		{"v4 发布（等条件：前面 7 条都合并） —— 推荐发布", "前面 7 条都合并", true},
		{"v4 发布 —— x (等条件: 用户验收完)", "用户验收完", true},
		{"v4 发布 —— x（ 等条件 ： a ）（进行中：agent/x 10-11 09:00）", "a", true},
		{"推送不催 —— 约定固定标记等条件：带这个标记的跳过", "", false},
		{"推送不催 —— 正文里提到「等条件」标记的用法", "", false},
		{"甲 —— 等条件满足后再推", "", false},
		{"甲 —— x（等条件）", "", false},
		{"甲 —— x（等条件满足后再说）", "", false},
	}
	for _, c := range cases {
		cond, ok := WaitCondition(c.text)
		if ok != c.ok || cond != c.cond {
			t.Errorf("%q：得到 (%q, %v)，应为 (%q, %v)", c.text, cond, ok, c.cond, c.ok)
		}
	}
	if got := ItemTitle("v4 发布（等条件：前面 7 条都合并） —— 推荐发布"); got != "v4 发布" {
		t.Errorf("标题应去掉等条件标记：%q", got)
	}
}

func TestParseBacklogEdgeCases(t *testing.T) {
	cases := map[string]string{
		"空文件":       "",
		"没有待做节":     "# 待办\n\n## 已完成\n- [ ] 不算\n",
		"待做节为空":     "## 待做\n\n## 已完成\n- [ ] x\n",
		"注释没闭合吞掉后面": "## 待做\n<!--\n- [ ] 不算\n",
	}
	for name, text := range cases {
		if items := ParseBacklog(text); len(items) != 0 {
			t.Errorf("%s：应没有条目，得到 %+v", name, items)
		}
	}
	// CRLF 换行
	items := ParseBacklog("## 待做\r\n- [ ] 甲 —— x\r\n")
	if len(items) != 1 || items[0].Title != "甲" {
		t.Errorf("CRLF：%+v", items)
	}
}

func TestFingerprintIgnoresInProgress(t *testing.T) {
	a := Fingerprint("- [ ] 第一条 —— 验收：x")
	b := Fingerprint("- [ ] 第一条 —— 验收：x（进行中：agent/1010-a 2026-10-10 10:00）")
	c := Fingerprint("- [ ] 第一条 —— 验收：y")
	if a != b {
		t.Errorf("去掉进行中标记后指纹应相同：%s %s", a, b)
	}
	if a == c {
		t.Errorf("原文不同指纹应不同")
	}
	if len(a) != 40 {
		t.Errorf("应为 sha1 十六进制：%s", a)
	}
}

func TestDoneMergeCommit(t *testing.T) {
	text := "# 待办\n\n## 待做\n\n- [ ] 甲 —— 合并提交 1111111 写在待做里不算\n\n## 已完成\n" +
		"<!-- - [x] 标题 —— 合并提交 abc1234；验收：… -->\n" +
		"- [x] 乙 —— 2026-10-10 合并，提交 aaa1111（PR #1）\n" +
		"- [x] 丙 —— 合并提交 bbb2222（ccc3333 + bbb2222）；验收：看 README\n" +
		"- [X] 丁 —— 合并提交：`ddd4444`；验收：…\n" +
		"- [x] 戊 —— 提到合并提交号要写成 abc 的格式\n" +
		"- [x] 丙 —— 合并提交 2222222；同名的旧条目\n" +
		"\n## 以后再说\n\n- [x] 己 —— 合并提交 3333333\n"
	cases := map[string]string{"甲": "", "乙": "", "丙": "bbb2222", "丁": "ddd4444", "戊": "", "己": "", "标题": "", "没有": ""}
	for title, want := range cases {
		if got := DoneMergeCommit(text, title); got != want {
			t.Errorf("%s 得到 %q，应为 %q", title, got, want)
		}
	}
}
