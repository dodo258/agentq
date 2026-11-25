package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/notify"
	"agentq/internal/testkit"
)

// 假 gh：记下参数（一行一次）；issue view 回 $HOME/gh-issue.json，查 tag、Release 都有；有 $HOME/gh-fail 就报错。
const issueFakeGH = `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> "$HOME/gh-calls"; echo >> "$HOME/gh-calls"
[ ! -f "$HOME/gh-fail" ] || { echo 'HTTP 502' >&2; exit 1; }
case "$1 $2" in
  "issue view") cat "$HOME/gh-issue.json" ;;
  "api repos/o/demo/git/ref/tags/"*) printf '{"ref":"refs/tags/%s"}' "${2##*/}" ;;
  "api repos/o/demo/releases/tags/"*) printf '{"tag_name":"%s"}' "${2##*/}" ;;
esac
`

// issueEnv：带 Telegram、登记了公开仓的 demo 项目，「已完成」里一条带 issue 标记的条目，假 gh 在 PATH 上。
func issueEnv(t *testing.T, issueJSON string) (*notifyEnv, string) {
	t.Helper()
	e := newNotifyEnv(t, true)
	os.WriteFile(filepath.Join(e.a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(issueFakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(e.a.Home, "gh-issue.json"), []byte(issueJSON), 0o644)
	e.a.Cfg.ReleaseRepos = map[string]string{"demo": "o/demo"}
	p := sgRepo(t, e.a, e.root, "demo", "# 待办\n\n## 待做\n\n## 已完成\n")
	fix := testkit.GitT(t, e.a.Home, p.Dir, "rev-parse", "--short=12", "HEAD")
	files := map[string]string{
		"AGENTS.md":       "# 项目\n\n- 自动发版：条件满足时由 agentq 调 `.agentq/release`\n",
		"CHANGELOG.md":    "# 更新记录\n\n## 未发布\n\n- 修复：安装后 tick 报错\n",
		".agentq/release": "#!/bin/sh\nexit 0\n",
		"docs/BACKLOG.md": "# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 安装后 tick 报错 —— 说明（issue：https://github.com/o/demo/issues/7）；合并提交 " + fix + "\n",
	}
	for f, text := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(p.Dir, f)), 0o755)
		if err := os.WriteFile(filepath.Join(p.Dir, f), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	testkit.GitT(t, e.a.Home, p.Dir, "add", "-A")
	testkit.GitT(t, e.a.Home, p.Dir, "commit", "-q", "-m", "合并修复")
	return e, p.Dir
}

func (e *notifyEnv) ghCalls() string {
	b, _ := os.ReadFile(filepath.Join(e.a.Home, "gh-calls"))
	return string(b)
}

// 发版后推草稿：条目带的 issue 链接随版本发出后，推一张卡片（标题、链接、写好的回复、三个按钮），只读查了原文、没贴回复。
// 点「发」才用 gh 贴回复、不关闭；重复点不再贴。
func TestIssueReplyAfterRelease(t *testing.T) {
	e, _ := issueEnv(t, `{"title":"安装后 tick 报错","body":"装完跑 tick 就退出"}`)
	if err := e.a.Release.Run([]string{"demo", "v0.4.1"}); err != nil {
		t.Fatal(err)
	}
	e.flush()
	form := e.tg.sentLastWith("issue 回复")
	text := form.Get("text")
	testkit.AssertContains(t, text, "安装后 tick 报错", "https://github.com/o/demo/issues/7", "这个问题在 v0.4.1 里处理了", "https://github.com/o/demo/releases/tag/v0.4.1", "不会自动关闭")
	btns := keyboardOf(t, form)
	if len(btns) != 3 || !strings.Contains(btns[0].Text, "发") || !strings.Contains(btns[1].Text, "发并关闭") || !strings.Contains(btns[2].Text, "不发") {
		t.Fatalf("%+v", btns)
	}
	if calls := e.ghCalls(); !strings.Contains(calls, "issue view https://github.com/o/demo/issues/7") || strings.Contains(calls, "comment") || strings.Contains(calls, "close") {
		t.Fatalf("推草稿只读查原文：\n%s", calls)
	}

	e.tg.click(1, ownerChat, 1001, btns[0].Data)
	e.flush()
	calls := e.ghCalls()
	if !strings.Contains(calls, "issue comment https://github.com/o/demo/issues/7 --body 这个问题在 v0.4.1 里处理了") || strings.Contains(calls, "issue close") {
		t.Fatalf("点「发」贴回复、不关闭：\n%s", calls)
	}
	if a := e.tg.lastAnswer(); !strings.Contains(a, "回复已贴到") {
		t.Errorf("%s", a)
	}
	if s := e.state(); s.Issues["https://github.com/o/demo/issues/7"].Done != notify.IssueSent {
		t.Errorf("%+v", s.Issues)
	}
	if len(e.tg.edits) != 1 || !strings.Contains(e.tg.edits[0].Get("text"), "选了 A 发") {
		t.Errorf("%v", e.tg.edits)
	}
	e.tg.click(2, ownerChat, 1001, btns[0].Data)
	e.tg.click(3, ownerChat, 1001, btns[1].Data)
	e.flush()
	if n := strings.Count(e.ghCalls(), "issue comment"); n != 1 || strings.Contains(e.ghCalls(), "issue close") {
		t.Errorf("重复点不再贴：\n%s", e.ghCalls())
	}
}

// 「发并关闭」才关闭；贴失败可以再点；英文 issue 用英文回复。
func TestIssueReplySendClose(t *testing.T) {
	e, _ := issueEnv(t, `{"title":"tick fails after install","body":"..."}`)
	e.a.Notify.IssueReplies(context.Background(), "demo", "o/demo", "v0.4.1", []string{"https://github.com/o/demo/issues/7"})
	e.flush()
	form := e.tg.sentLastWith("issue 回复")
	testkit.AssertContains(t, form.Get("text"), "tick fails after install", "This is addressed in v0.4.1")
	btns := keyboardOf(t, form)
	os.WriteFile(filepath.Join(e.a.Home, "gh-fail"), nil, 0o644)
	e.tg.click(1, ownerChat, 1001, btns[1].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "没贴成") {
		t.Fatalf("%s", a)
	}
	if s := e.state(); s.Issues["https://github.com/o/demo/issues/7"].Done != "" {
		t.Fatalf("没贴成要能再点：%+v", s.Issues)
	}
	os.Remove(filepath.Join(e.a.Home, "gh-fail"))
	e.tg.click(2, ownerChat, 1001, btns[1].Data)
	e.flush()
	calls := e.ghCalls()
	if !strings.Contains(calls, "issue comment https://github.com/o/demo/issues/7 --body This is addressed") || !strings.Contains(calls, "issue close https://github.com/o/demo/issues/7") {
		t.Fatalf("\n%s", calls)
	}
	if s := e.state(); s.Issues["https://github.com/o/demo/issues/7"].Done != notify.IssueClosed {
		t.Errorf("%+v", s.Issues)
	}
}

// 点「不发」：不调 gh 贴回复，记下；同一个 issue 再随新版本交过来也不再推。
func TestIssueReplySkip(t *testing.T) {
	e, _ := issueEnv(t, `{"title":"安装后 tick 报错","body":""}`)
	url := "https://github.com/o/demo/issues/7"
	e.a.Notify.IssueReplies(context.Background(), "demo", "o/demo", "v0.4.1", []string{url})
	e.flush()
	btns := keyboardOf(t, e.tg.sentLastWith("issue 回复"))
	e.tg.click(1, ownerChat, 1001, btns[2].Data)
	e.flush()
	if calls := e.ghCalls(); strings.Contains(calls, "comment") || strings.Contains(calls, "close") {
		t.Fatalf("不发不调 gh 贴：\n%s", calls)
	}
	if a := e.tg.lastAnswer(); !strings.Contains(a, "不再推") {
		t.Errorf("%s", a)
	}
	if s := e.state(); s.Issues[url].Done != notify.IssueSkipped {
		t.Errorf("%+v", s.Issues)
	}
	sent := e.tg.count()
	e.a.Notify.IssueReplies(context.Background(), "demo", "o/demo", "v0.5.0", []string{url})
	e.flush()
	if e.tg.count() != sent {
		t.Errorf("不发的不再推：%q", e.tg.texts()[sent:])
	}
}

// 夜班提示词里的 issue 分诊写清：按项目规则授权、写进哪一节、带固定格式的链接、夜班自己不回复不关闭。
func TestNightPromptIssueTriage(t *testing.T) {
	b, err := os.ReadFile("prompts/night.md")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "issue 分诊") {
			line = l
		}
	}
	testkit.AssertContains(t, line, "项目规则", "只读", "「## 待做」", "「## 建议」", "（issue：https://github.com/<owner>/<repo>/issues/<号>）", "不在 GitHub 上回复、关闭", "点「发」")
	filled := strings.NewReplacer("<owner>/<repo>", "o/demo", "<号>", "1").Replace(line)
	if url, _ := notify.IssueRef(filled); url != "https://github.com/o/demo/issues/1" {
		t.Errorf("提示词写的标记格式要认得出：%q", url)
	}
}
