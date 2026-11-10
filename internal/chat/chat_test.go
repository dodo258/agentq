package chat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

func TestMoveDecisionOptions(t *testing.T) {
	const in = "## 待做\n\n- [ ] 已有\n\n## 等用户决定\n\n- [ ] 第一件 —— 说明（选项：A 一｜B 二；推荐 A）\n- [ ] 第二件\n  留着的说明\n  （选项：A 三｜B 四；推荐 B）\n- [ ] 第三件（选项：A 五｜B 六；推荐 B）\n"
	got, ok := MoveDecision(in, "第二件", "B 四")
	if !ok || strings.Contains(got, "A 三") || !strings.Contains(got, "- [ ] 第二件（用户决定：B 四）\n- [ ] 已有") || !strings.Contains(got, "留着的说明") {
		t.Errorf("%v\n%s", ok, got)
	}
	got, ok = MoveDecision(in, "第三件", "B 六")
	if !ok || !strings.Contains(got, "- [ ] 第三件（用户决定：B 六）") || strings.Contains(got, "A 五") {
		t.Errorf("%v\n%s", ok, got)
	}
	// 其他条目的选项不动
	if !strings.Contains(got, "第一件 —— 说明（选项：A 一｜B 二；推荐 A）") {
		t.Errorf("%s", got)
	}
}

func TestParseFastCmd(t *testing.T) {
	for _, c := range []struct {
		in  string
		cmd fastCmd
		id  string
	}{
		{"暂停", fastPause, ""},
		{" 暂停。\n", fastPause, ""},
		{"恢复", fastResume, ""},
		{"恢复.", fastResume, ""},
		{"额度", fastUsage, ""},
		{"额度？", fastUsage, ""},
		{"在跑什么", fastRunning, ""},
		{"在跑什么?", fastRunning, ""},
		{"停 1104-120000-demo", fastStop, "1104-120000-demo"},
		{"停　1104-120000-demo", fastStop, "1104-120000-demo"}, // 全角空格
		{"停 1104-120000-demo。", fastStop, "1104-120000-demo"},
		{"停 1104-12", fastStop, "1104-12"},
		{"停 1104", fastStop, "1104"},
		{"停 1104-120000-Telegram对话", fastStop, "1104-120000-Telegram对话"},
		// 不是整句、会改状态的问句、不是任务 id：都交给模型
		{"暂停一下", fastNone, ""},
		{"暂停？", fastNone, ""},
		{"恢复?", fastNone, ""},
		{"先暂停，晚点恢复", fastNone, ""},
		{"恢复 demo 项目", fastNone, ""},
		{"额度还够吗", fastNone, ""},
		{"在跑什么任务", fastNone, ""},
		{"现在在跑什么", fastNone, ""},
		{"停", fastNone, ""},
		{"停 下来", fastNone, ""},
		{"停 一下", fastNone, ""},
		{"停止 1104-120000-demo", fastNone, ""},
		{"停 1104-120000-demo 吧", fastNone, ""},
		{"停 1104-120000-demo？", fastNone, ""},
		{"停 110", fastNone, ""},
		{"状态", fastNone, ""}, // 「状态」走原来的 isStatusAsk
		{"", fastNone, ""},
	} {
		cmd, id := parseFastCmd(c.in)
		if cmd != c.cmd || id != c.id {
			t.Errorf("%q → %d %q，要 %d %q", c.in, cmd, id, c.cmd, c.id)
		}
	}
}

// 对话提示词：判断不出项目时问一句。
func TestChatPromptUnchanged(t *testing.T) {
	p := chatPrompt(nil, time.Now())
	if strings.Contains(p, "date -j") {
		t.Errorf("提示词不能写死 BSD 的 date：\n%s", p)
	}
	if strings.Contains(p, "任务箱") || strings.Contains(p, "inbox") || !strings.Contains(p, "判断不出是哪个项目时，运行 agentq projects 列出项目问一句") {
		t.Errorf("对话提示词变了：\n%s", p)
	}
}

func TestHTMLVisible(t *testing.T) {
	for in, want := range map[string]int{
		"":                         0,
		"abc":                      3,
		"中文":                       2,
		"<b>粗</b>":                 1,
		"a &lt; b":                 5,
		"&amp;&#39;&#x27;":         3,
		"a < b":                    5, // 没转义的尖括号算字
		"x & y":                    5,
		"&nosemicolon":             12,
		`<a href="http://x">链</a>`: 1,
		"<i>1</i><code>2</code>":   2,
	} {
		if got := HtmlVisible(in); got != want {
			t.Errorf("%q：%d，要 %d", in, got, want)
		}
	}
}

func TestCutHTML(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 3, "abc"},
		{"中文测试", 2, "中文"},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		// 不从标签中间切
		{"ab<b>cd</b>ef", 2, "ab"},
		{"ab<b>cd</b>ef", 3, "ab<b>c</b>"},
		{"ab<b>cd</b>ef", 4, "ab<b>cd</b>"},
		{"ab<b>cd</b>ef", 5, "ab<b>cd</b>e"},
		// 不从实体中间切，实体算一个字
		{"a&lt;&gt;b", 1, "a"},
		{"a&lt;&gt;b", 2, "a&lt;"},
		{"a&lt;&gt;b", 3, "a&lt;&gt;"},
		{"&#39;x", 1, "&#39;"},
		// 嵌套、带属性：倒着闭合
		{"<b>x<i>yz</i></b>", 2, "<b>x<i>y</i></b>"},
		{`<a href="u">link</a>`, 2, `<a href="u">li</a>`},
		{"<pre><code>abc</code></pre>", 1, "<pre><code>a</code></pre>"},
		// 切点后面紧跟开标签：不留空标签
		{"ab<b>cd</b>", 2, "ab"},
		// 已经闭合的不重复闭合
		{"<b>a</b><i>b</i>c", 2, "<b>a</b><i>b</i>"},
		// 没闭合的尖括号、没分号的 &：算普通字
		{"a < b > c", 3, "a <"},
		{"x & y", 2, "x"},
		// 乱闭合：多余的闭合标签不报错
		{"a</b>bc", 2, "a</b>b"},
		// 大写标签
		{"<B>abc</B>", 1, "<B>a</b>"},
		{"abc\n\ndef", 4, "abc"},
	} {
		if got := cutHTML(c.in, c.n); got != c.want {
			t.Errorf("cutHTML(%q, %d) = %q，要 %q", c.in, c.n, got, c.want)
		}
	}
}

// wireChat 建模块实例。
func wireChat(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Backlog: k.Backlog, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner, Service: k.Service, Telegram: k.Telegram}
}

// newChatApp 带临时 HOME 的实例，这些用例不连 Telegram。
func newChatApp(t *testing.T) (*Service, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return wireChat(&env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: time.Now, Out: out}), out
}

func TestMoveDecision(t *testing.T) {
	in := "## 待做\n\n说明\n\n- [ ] 第一条\n\n## 等用户决定\n\n<!-- - [ ] 选A -->\n- [ ] 别的\n- [ ] 选A —— 选项（分支 agent/x）\n\n## 已完成\n"
	got, ok := MoveDecision(in, "选A", "就选 A")
	want := "## 待做\n\n说明\n\n- [ ] 选A —— 选项（分支 agent/x）（用户决定：就选 A）\n- [ ] 第一条\n\n## 等用户决定\n\n<!-- - [ ] 选A -->\n- [ ] 别的\n\n## 已完成\n"
	if !ok || got != want {
		t.Errorf("%v\n%q\n%q", ok, got, want)
	}
	if _, ok := MoveDecision(in, "没有", "x"); ok {
		t.Error("找不到应返回假")
	}
	if _, ok := MoveDecision("## 等用户决定\n- [ ] 选A\n", "选A", "x"); ok {
		t.Error("没有待做应返回假")
	}
	empty := "## 待做\n\n## 等用户决定\n- [ ] 选A\n"
	if got, ok := MoveDecision(empty, "选A", "x"); !ok || !strings.HasPrefix(got, "## 待做\n- [ ] 选A（用户决定：x）\n") {
		t.Errorf("待做是空的：%q", got)
	}
}

// 攒了一大堆消息：一批不能超过命令行放得下的长度，否则整批都起不来、都被退回去。多的留到下一批，顺序不变。
func TestRegressChatBatchFits(t *testing.T) {
	t.Parallel()
	var msgs []notify.InMsg
	for i := 0; i < 300; i++ {
		msgs = append(msgs, notify.InMsg{ID: int64(i), Text: strings.Repeat("长", 1300)})
	}
	batch, later, _ := chatBatch(&notify.State{}, msgs)
	if len(batch) == 0 || len(batch)+len(later) != len(msgs) {
		t.Fatalf("%d + %d", len(batch), len(later))
	}
	if p := chatPrompt(batch, time.Now()); len(p) > 256<<10 {
		t.Fatalf("提示词 %d 字节，放不进命令行", len(p))
	}
	if later[0].ID != batch[len(batch)-1].ID+1 {
		t.Errorf("顺序乱了：%d %d", batch[len(batch)-1].ID, later[0].ID)
	}
	// 一条本身就很长也要交出去，不能卡死
	one, rest, _ := chatBatch(&notify.State{}, []notify.InMsg{{ID: 1, Text: strings.Repeat("长", chatBatchMax)}, {ID: 2, Text: "x"}})
	if len(one) != 1 || len(rest) != 1 {
		t.Errorf("%d %d", len(one), len(rest))
	}
}

func TestFollowupArgs(t *testing.T) {
	cfg := config.Default()
	args := strings.Join(testkit.ClaudeArgv(t, FollowupRequest(cfg, "问", "p", "标题", "s1", "")), " ")
	if strings.Contains(args, "--add-dir") {
		t.Errorf("没有附件不加目录：%s", args)
	}
	if got := strings.Join(testkit.ClaudeArgv(t, FollowupRequest(cfg, "问", "p", "标题", "s1", "/j/files")), " "); !strings.Contains(got, "--add-dir /j/files") {
		t.Errorf("有附件只加这个目录：%s", got)
	}
	if strings.Contains(args, "--permission-mode") || strings.Contains(args, "bypass") {
		t.Errorf("只读续跑不能带权限模式：%s", args)
	}
	if !strings.Contains(args, "--model sonnet") || !strings.Contains(args, "--effort medium") {
		t.Errorf("只读答问用 sonnet、medium：%s", args)
	}
	cfg.FollowupModel = ""
	if r := FollowupRequest(cfg, "问", "p", "标题", "s1", ""); r.Model != cfg.Model || r.Effort != cfg.Effort {
		t.Errorf("没配答问模型照主模型：%s %s", r.Model, r.Effort)
	}
	for _, want := range []string{"-r s1", "--tools Read,Grep,Glob", "--strict-mcp-config", "--max-budget-usd 1", "--output-format json", "--name " + proc.NameMarker("p")} {
		if !strings.Contains(args, want) {
			t.Errorf("缺 %s：%s", want, args)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(FollowupSchema), &schema); err != nil {
		t.Errorf("schema：%v", err)
	}
}
