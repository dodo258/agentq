package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"

	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// issue 回复草稿：夜班分诊公开仓 issue 时在条目末尾写固定标记（issue：<链接>）；那一条合并、随某个版本自动发出后，
// 发版模块把这一版带出去的链接交给 IssueReplies：起草回复（修在哪个版本、怎么升级，中英按 issue 原文），推一张卡片，
// 按钮「发」「发并关闭 issue」「不发」。只有用户点了「发」才用 gh 贴回复；关闭只走「发并关闭」那个按钮；
// 点过的（发了、不发）记在 notify.json 的 issues 里，同一个 issue 以后不再推。

// IssueRefRe：条目里的 issue 标记，只认括号紧挨「issue」再接冒号、里面是一个 GitHub issue 链接的写法（全角半角都行）。
var IssueRefRe = regexp.MustCompile(`[（(]\s*(?i:issue)\s*[：:]\s*(https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/issues/(\d+))\s*[）)]`)

// IssueRef：条目里第一个 issue 标记的链接和仓库（owner/repo）；没有返回空。
func IssueRef(text string) (url, repo string) {
	m := IssueRefRe.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// IssueReply.Done 的值。空表示还在等用户点。
const (
	IssueSending = "sending" // 正在贴：别的进程再点不重复贴
	IssueSent    = "sent"
	IssueClosed  = "closed" // 贴了回复并关闭
	IssueSkipped = "skipped"
)

// 按钮对应的动作。
const (
	IssueActSend      = "send"
	IssueActSendClose = "send-close"
	IssueActSkip      = "skip"
)

// IssueReply 是一条等用户点头的 issue 回复草稿。
type IssueReply struct {
	Project string    `json:"project"`
	Version string    `json:"version"`
	Title   string    `json:"title,omitempty"`
	Draft   string    `json:"draft"`
	At      time.Time `json:"at"`
	Done    string    `json:"done,omitempty"`
}

// ErrIssueAnswered：这条已经点过了。
var ErrIssueAnswered = errors.New("这条 issue 回复已经定了")

const issueGhWait = 20 * time.Second

// issueGh 跑一次 gh。gh 的登录在用户 HOME 下，环境照发版模块（只换 HOME，关掉交互和更新提示）。
func (a *Service) issueGh(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, issueGhWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "HOME="+a.Home, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = textfmt.OneLine(string(ee.Stderr), 200)
		}
		return nil, fmt.Errorf("gh %s %s：%s", args[0], args[1], msg)
	}
	return out, nil
}

// issueChinese：issue 原文是中文。标题有汉字就算；标题没有字母时看正文。
func issueChinese(title, body string) bool {
	han := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) })
	}
	if han(title) {
		return true
	}
	return !strings.ContainsFunc(title, unicode.IsLetter) && han(body)
}

// IssueDraft 拼回复：修在哪个版本（带 Release 链接）、怎么升级。agentq 自己用 `agentq update`，别的项目看 Release 页的说明。
func IssueDraft(zh, self bool, version, link string) string {
	if zh {
		up := "按 Release 页面的安装说明升级到这一版。"
		if self {
			up = "升级：运行 `agentq update`。"
		}
		return "这个问题在 " + version + " 里处理了：" + link + "\n" + up + "\n升级后还有问题的话，请在这里回复。"
	}
	up := "To upgrade, follow the install notes on the release page."
	if self {
		up = "To upgrade, run `agentq update`."
	}
	return "This is addressed in " + version + ": " + link + "\n" + up + "\nIf you still see the problem after upgrading, please reply here."
}

// IssueReplies：发版模块发成 version 后调用，urls 是这一版带出去的条目写的 issue 链接（都属于公开仓 repo）。
// 每个没推过的起草一条回复、推一张卡片；推送关着就只记日志。gh 查不到原文时用链接当标题、草稿用英文，卡片上写明。
func (a *Service) IssueReplies(ctx context.Context, project, repo, version string, urls []string) {
	if len(urls) == 0 {
		return
	}
	if !a.Cfg.NotifyEnabled {
		a.Logf("issue 回复：%s %s 带出去 %d 个 issue，推送关着，草稿没推", project, version, len(urls))
		return
	}
	link := "https://github.com/" + repo + "/releases/tag/" + version
	self := project == a.Cfg.SelfProject
	var known map[string]*IssueReply
	if err := a.WithNotify(func(s *State) error { known = s.Issues; return nil }); err != nil {
		a.Logf("issue 回复：读不了 %s，草稿没推：%v", a.NotifyPath(), err)
		return
	}
	type draft struct {
		url  string
		r    IssueReply
		note string
	}
	var drafts []draft
	for _, u := range urls {
		if known[u] != nil {
			continue
		}
		d := draft{url: u, r: IssueReply{Project: project, Version: version}}
		title, body, err := a.issueText(ctx, u)
		if err != nil {
			d.note = "取不到 issue 原文（" + textfmt.OneLine(err.Error(), 120) + "），草稿先用英文"
			title = "#" + u[strings.LastIndex(u, "/")+1:]
		}
		d.r.Title = title
		d.r.Draft = IssueDraft(err == nil && issueChinese(title, body), self, version, link)
		drafts = append(drafts, d)
	}
	if len(drafts) == 0 {
		return
	}
	if err := a.WithNotify(func(s *State) error {
		if s.Issues == nil {
			s.Issues = map[string]*IssueReply{}
		}
		for _, d := range drafts {
			if s.Issues[d.url] != nil {
				continue
			}
			r := d.r
			r.At = a.Now()
			s.Issues[d.url] = &r
			lines := []string{d.url, "随 " + version + " 发出；写好的回复（点「发」才贴到 GitHub）："}
			lines = append(lines, strings.Split(r.Draft, "\n")...)
			if d.note != "" {
				lines = append(lines, d.note)
			}
			lines = append(lines, "不会自动关闭 issue；要关就点「发并关闭」")
			s.Add(a.Now(), Notice{Cat: "decide", Key: "issue:" + d.url, Project: project,
				Text:  project + "　issue 回复：" + textfmt.TruncRunes(r.Title, 60),
				Lines: lines,
				Card: &DecideCard{Project: project, Fingerprint: "issue:" + d.url, Title: "issue 回复：" + r.Title, Recommend: "A", IssueURL: d.url,
					Options: []DecideOption{{Label: "A", Text: "发"}, {Label: "B", Text: "发并关闭 issue"}, {Label: "C", Text: "不发"}}}})
			a.Logf("issue 回复：%s %s 起草了 %s 的回复，等你点", project, version, d.url)
		}
		return nil
	}); err != nil {
		a.Logf("issue 回复：草稿没记下：%v", err)
	}
}

// issueText：只读查 issue 的标题和正文。
func (a *Service) issueText(ctx context.Context, url string) (title, body string, err error) {
	out, err := a.issueGh(ctx, "issue", "view", url, "--json", "title,body")
	if err != nil {
		return "", "", err
	}
	var v struct{ Title, Body string }
	if err := json.Unmarshal(out, &v); err != nil {
		return "", "", fmt.Errorf("gh issue view 的输出看不懂：%w", err)
	}
	return textfmt.OneLine(v.Title, 200), v.Body, nil
}

// AnswerIssue：用户点了按钮。发（可带关闭）才调 gh；不发只记下。返回给用户看的结论。
// 贴回复失败时记录退回「等点」，可以再点；回复贴上了、关闭没成，算已发，结论里说关闭没成。
func (a *Service) AnswerIssue(ctx context.Context, url, act string) (string, error) {
	var r IssueReply
	if err := a.WithNotify(func(s *State) error {
		x := s.Issues[url]
		if x == nil {
			return errors.New("找不到这条 issue 回复的记录")
		}
		if x.Done != "" {
			return ErrIssueAnswered
		}
		if act == IssueActSkip {
			x.Done = IssueSkipped
		} else {
			x.Done = IssueSending
		}
		r = *x
		return nil
	}); err != nil {
		return "", err
	}
	if act == IssueActSkip {
		a.Logf("issue 回复：%s 不发，以后不再推", url)
		return "不发，以后不再推", nil
	}
	done, note := IssueSent, "回复已贴到 "+url
	_, err := a.issueGh(ctx, "issue", "comment", url, "--body", r.Draft)
	if err == nil && act == IssueActSendClose {
		if _, cerr := a.issueGh(ctx, "issue", "close", url); cerr != nil {
			note += "；关闭没成：" + textfmt.OneLine(cerr.Error(), 120)
		} else {
			done, note = IssueClosed, note+"，issue 已关闭"
		}
	}
	if err != nil {
		done = ""
	}
	if serr := a.WithNotify(func(s *State) error {
		if x := s.Issues[url]; x != nil {
			x.Done = done
		}
		return nil
	}); serr != nil {
		a.Logf("issue 回复：%s 的结果没存下：%v", url, serr)
	}
	if err != nil {
		a.Logf("issue 回复：%s 没贴成：%v", url, err)
		return "", err
	}
	a.Logf("issue 回复：%s", note)
	return note, nil
}
