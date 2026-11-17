package notify

import (
	"strings"
	"testing"
)

// 只认固定标记：括号紧挨「issue」接冒号再接一个 issue 链接；正文顺带写的链接、PR 链接、写错的不算。
func TestIssueRef(t *testing.T) {
	cases := []struct{ text, url, repo string }{
		{"修 tick 报错 —— 说明（issue：https://github.com/o/demo/issues/12）", "https://github.com/o/demo/issues/12", "o/demo"},
		{"标题 —— 说明 (Issue: https://github.com/O/De.mo/issues/3)；合并提交 abc1234", "https://github.com/O/De.mo/issues/3", "O/De.mo"},
		{"标题 —— 见 https://github.com/o/demo/issues/12 的讨论", "", ""},
		{"标题 —— 说明（issue：https://github.com/o/demo/pull/12）", "", ""},
		{"标题 —— 说明（issue：#12）", "", ""},
		{"标题 —— 修 issue 的事", "", ""},
	}
	for _, c := range cases {
		if url, repo := IssueRef(c.text); url != c.url || repo != c.repo {
			t.Errorf("%q → %q %q，要 %q %q", c.text, url, repo, c.url, c.repo)
		}
	}
}

// 中英按 issue 原文：标题有汉字是中文；标题没有字母看正文；英文标题里贴了中文日志还是英文。
func TestIssueChinese(t *testing.T) {
	cases := []struct {
		title, body string
		zh          bool
	}{
		{"安装后 tick 报错", "", true},
		{"tick fails after install", "", false},
		{"tick fails after install", "日志：找不到配置", false},
		{"#12 ???", "装完以后跑不起来", true},
		{"", "it crashes", false},
	}
	for _, c := range cases {
		if got := issueChinese(c.title, c.body); got != c.zh {
			t.Errorf("%q %q → %v", c.title, c.body, got)
		}
	}
}

// 草稿写版本、Release 链接、怎么升级：agentq 自己写 agentq update，别的项目看 Release 页。
func TestIssueDraft(t *testing.T) {
	link := "https://github.com/o/demo/releases/tag/v0.4.0"
	for _, c := range []struct {
		zh, self bool
		want     []string
	}{
		{true, true, []string{"v0.4.0", link, "`agentq update`", "请在这里回复"}},
		{true, false, []string{"v0.4.0", link, "按 Release 页面的安装说明"}},
		{false, true, []string{"addressed in v0.4.0", link, "run `agentq update`"}},
		{false, false, []string{"addressed in v0.4.0", link, "follow the install notes"}},
	} {
		d := IssueDraft(c.zh, c.self, "v0.4.0", link)
		for _, w := range c.want {
			if !strings.Contains(d, w) {
				t.Errorf("zh=%v self=%v 草稿缺 %q：\n%s", c.zh, c.self, w, d)
			}
		}
	}
}
