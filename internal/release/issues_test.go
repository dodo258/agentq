package release

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// mergeTodo 像引擎合并那样把「待做」里这一条移进「已完成」，写上合并提交。
func mergeTodo(text, line, commit string) string {
	text = strings.Replace(text, "## 待做\n", "## 待做\n\n"+line+"\n", 1)
	return backlog.MarkMerged(text, backlog.Fingerprint(line), backlog.ItemTitle(strings.TrimPrefix(line, "- [ ] ")), "", backlog.MergedLine{Commit: commit})
}

// 发成后只推这一版带出去的 issue：合并提交在上一个发成的版本之后（没发成的版本不算分界），链接属于这个公开仓，
// 「已完成」和归档里的都算；没写合并提交的、别的仓库的、上一版已经带出去的不推。只读查 issue 原文，不贴回复。
func TestRunDraftsShippedIssues(t *testing.T) {
	r := newRig(t, nil)
	head := func() string { return testkit.GitT(t, r.home, r.dir, "rev-parse", "--short=12", "HEAD") }
	r.commit("a.txt", "旧修复")
	old := head()
	cl := r.read(filepath.Join(r.dir, "CHANGELOG.md"))
	os.WriteFile(filepath.Join(r.dir, "CHANGELOG.md"), []byte(cl+"\n"), 0o644)
	testkit.GitT(t, r.home, r.dir, "commit", "-q", "-am", stampSubject("v0.3.0"))
	r.commit("b.txt", "新修复")
	fresh := head()
	os.WriteFile(filepath.Join(r.dir, "CHANGELOG.md"), []byte(cl+"\n\n"), 0o644)
	testkit.GitT(t, r.home, r.dir, "commit", "-q", "-am", stampSubject("v0.3.1")) // 记了 CHANGELOG 却没发成
	r.s.save("demo", Record{Failed: []string{"v0.3.1"}})

	bl := r.read(filepath.Join(r.dir, "docs", "BACKLOG.md"))
	bl = mergeTodo(bl, "- [ ] 上一版修的 —— 说明（issue：https://github.com/o/demo/issues/1）", old)
	bl = mergeTodo(bl, "- [ ] 安装后报错 —— 说明（issue：https://github.com/o/demo/issues/2）", fresh)
	bl = mergeTodo(bl, "- [ ] 别的仓库的 —— 说明（issue：https://github.com/x/other/issues/3）", fresh)
	bl += "- [x] 没写合并提交 —— 说明（issue：https://github.com/o/demo/issues/4）\n"
	bl += "- [x] 正文顺带提到 https://github.com/o/demo/issues/6 —— 合并提交 " + fresh + "\n"
	r.commit("docs/BACKLOG.md", bl)
	r.commit("docs/BACKLOG-archive.md", "# 归档\n\n- [x] 归档里的 —— 说明（Issue: https://github.com/O/Demo/issues/5）；合并提交 "+fresh+"\n")
	if !strings.Contains(r.read(filepath.Join(r.dir, "docs", "BACKLOG.md")), "（issue：https://github.com/o/demo/issues/2）；合并提交 "+fresh) {
		t.Fatalf("合并后条目要留着 issue 标记：\n%s", bl)
	}
	r.file("gh-issue.json", `{"title":"安装后 tick 报错","body":"..."}`)

	r.tick(&state.State{})
	if err := r.s.Run([]string{"demo", "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	var s notify.State
	if err := json.Unmarshal([]byte(r.notices()), &s); err != nil {
		t.Fatal(err)
	}
	var got []string
	for u := range s.Issues {
		got = append(got, u)
	}
	slices.Sort(got)
	if want := []string{"https://github.com/O/Demo/issues/5", "https://github.com/o/demo/issues/2"}; !slices.Equal(got, want) {
		t.Fatalf("推的 issue %v，要 %v", got, want)
	}
	two := s.Issues["https://github.com/o/demo/issues/2"]
	if two.Version != "v0.4.0" || two.Done != "" || two.Title != "安装后 tick 报错" {
		t.Errorf("%+v", two)
	}
	testkit.AssertContains(t, two.Draft, "v0.4.0", "https://github.com/o/demo/releases/tag/v0.4.0", "按 Release 页面的安装说明升级")
	cards := 0
	for _, n := range s.Pending {
		if n.Card != nil && n.Card.IssueURL != "" {
			cards++
		}
	}
	if cards != 2 {
		t.Errorf("每个 issue 一张卡片：%d", cards)
	}
	calls := r.read(filepath.Join(r.home, "gh-calls"))
	if !strings.Contains(calls, "issue view https://github.com/o/demo/issues/2 --json title,body") || strings.Contains(calls, "issue comment") || strings.Contains(calls, "issue close") {
		t.Errorf("只读查原文，不贴回复：\n%s", calls)
	}

	// 同一个 issue 再交过来不再推
	before := len(s.Pending)
	r.s.Notify.IssueReplies(context.Background(), "demo", "o/demo", "v0.4.1", []string{"https://github.com/o/demo/issues/2"})
	json.Unmarshal([]byte(r.notices()), &s)
	if len(s.Pending) != before || s.Issues["https://github.com/o/demo/issues/2"].Version != "v0.4.0" {
		t.Errorf("推过的不再推：%d → %d", before, len(s.Pending))
	}
}
