package release

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"

	"agentq/internal/backlog"
	"agentq/internal/notify"
)

// stampSubject 是 stamp 记 CHANGELOG 那次提交的说明；shippedIssues 按它找上一版的位置。
func stampSubject(version string) string {
	return "发版 " + version + "：CHANGELOG「未发布」记成 " + version
}

var stampSubjectRe = regexp.MustCompile(`^发版 (v\d+\.\d+\.\d+)：CHANGELOG「未发布」记成 `)

// prevStamp：上一个发成的版本（不在 skip 里）记 CHANGELOG 的提交；还没自动发成过返回空。
func (s *Service) prevStamp(p backlog.Project, skip []string) string {
	out, err := s.Gitx.Git(p.Dir, "log", "--format=%H %s", "--grep=^发版 v", "--", "CHANGELOG.md")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		hash, subject, _ := strings.Cut(l, " ")
		if m := stampSubjectRe.FindStringSubmatch(subject); m != nil && !slices.Contains(skip, m[1]) {
			return hash
		}
	}
	return ""
}

// shippedIssues：这一版带出去的条目写的 issue 链接（只要公开仓 repo 自己的）。条目在「已完成」或归档里，
// 合并提交在默认分支上、不在上一个发成的版本里（failed 是记过 CHANGELOG 却没发成的版本）。没写合并提交的认不出是哪一版，不算。
func (s *Service) shippedIssues(p backlog.Project, repo, version string, failed []string) []string {
	prev := s.prevStamp(p, append([]string{version}, failed...))
	in := func(commit, rev string) bool {
		_, err := s.Gitx.Git(p.Dir, "merge-base", "--is-ancestor", commit, rev)
		return err == nil
	}
	var urls []string
	for _, f := range []string{p.BacklogPath(), p.ArchivePath()} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, e := range backlog.BacklogEntries(string(b)) {
			url, r := notify.IssueRef(e.Text)
			m := backlog.LastMergedCommit(e.Text)
			if !e.Done || url == "" || !strings.EqualFold(r, repo) || m == nil || slices.Contains(urls, url) {
				continue
			}
			if c := e.Text[m[2]:m[3]]; in(c, "HEAD") && (prev == "" || !in(c, prev)) {
				urls = append(urls, url)
			}
		}
	}
	return urls
}

// issueReplies：发成后，这一版带出去的 issue 交给推送起草回复，等用户点「发」。
func (s *Service) issueReplies(p backlog.Project, repo, version string, failed []string) {
	if urls := s.shippedIssues(p, repo, version, failed); len(urls) > 0 {
		s.Notify.IssueReplies(context.Background(), p.Name, repo, version, urls)
	}
}
