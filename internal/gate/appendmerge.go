package gate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// 只往后加的记录文件（任意目录下同名的都算）。两边都在同一处新加了内容才会撞上冲突，两边的都该留着。
var appendOnlyFiles = map[string]bool{"CHANGELOG.md": true, "DECISIONS.md": true, "PROGRESS.md": true}

// markerSize：冲突标记的长度，比 Markdown 里会出现的 ======= 这类行长得多，不会认错。
const markerSize = 64

// resolveAppendOnly 在门的一次性 worktree 里合掉只追加记录文件的冲突：每一处冲突都是两边各自新加的内容（基点那边是空的）时，
// 两边都留着，默认分支的在前、任务的在后；任务新加的标题和默认分支同一处新加的标题一样时并到同一个标题下，不出两个同名标题。
// 有一个文件不是只追加的、有一处冲突改了原有的行、或者合完多出了重复的标题，就一个都不动，返回假，照旧退回任务。
func (a *Service) resolveAppendOnly(ctx context.Context, wt string, files []string) bool {
	for _, f := range files {
		if !appendOnlyFiles[path.Base(f)] {
			return false
		}
	}
	merged := make(map[string]string, len(files))
	for _, f := range files {
		text, ok := a.appendMergeFile(ctx, wt, f)
		if !ok {
			return false
		}
		merged[f] = text
	}
	for _, f := range files {
		if os.WriteFile(filepath.Join(wt, f), []byte(merged[f]), 0o644) != nil {
			return false
		}
		if _, err := a.git(ctx, wt, "add", "--", f); err != nil {
			return false
		}
	}
	return true
}

// appendMergeFile 取出冲突文件的三份（基点、默认分支、任务），用 git merge-file 标出冲突再按只追加的规则合。
func (a *Service) appendMergeFile(ctx context.Context, wt, f string) (string, bool) {
	out, err := a.git(ctx, wt, "checkout-index", "--stage=all", "--temp", "--", f)
	if err != nil {
		return "", false
	}
	names, _, _ := strings.Cut(out, "\t")
	st := strings.Fields(names)
	if len(st) != 3 {
		return "", false
	}
	for _, n := range st {
		if n != "." {
			defer os.Remove(filepath.Join(wt, n))
		}
	}
	if st[1] == "." || st[2] == "." { // 一边删了这个文件：不是追加
		return "", false
	}
	if st[0] == "." { // 两边都新建了这个文件：基点当空文件
		tmp, err := os.CreateTemp(wt, ".merge_base_")
		if err != nil {
			return "", false
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		st[0] = filepath.Base(tmp.Name())
	}
	oursText, err1 := os.ReadFile(filepath.Join(wt, st[1]))
	theirsText, err2 := os.ReadFile(filepath.Join(wt, st[2]))
	if err1 != nil || err2 != nil {
		return "", false
	}
	_, err = a.git(ctx, wt, "merge-file", "--diff3", "--marker-size="+strconv.Itoa(markerSize), "-L", "ours", "-L", "base", "-L", "theirs", st[1], st[0], st[2])
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() < 0 || exit.ExitCode() >= 128) {
		return "", false
	}
	marked, err := os.ReadFile(filepath.Join(wt, st[1]))
	if err != nil {
		return "", false
	}
	text, ok := unionAppends(string(marked))
	if !ok || dupHeadings(text, string(oursText), string(theirsText)) {
		return "", false
	}
	return text, true
}

// unionAppends 按 diff3 冲突标记逐处合：基点部分不空（改了原有的行）就合不了。
func unionAppends(marked string) (string, bool) {
	start, mid, sep, end := strings.Repeat("<", markerSize), strings.Repeat("|", markerSize), strings.Repeat("=", markerSize), strings.Repeat(">", markerSize)
	lines := strings.SplitAfter(marked, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], start) {
			b.WriteString(lines[i])
			continue
		}
		var parts [3][]string // 默认分支、基点、任务
		k := 0
		closed := false
		for i++; i < len(lines); i++ {
			l := lines[i]
			switch {
			case k == 0 && strings.HasPrefix(l, mid):
				k = 1
			case k <= 1 && strings.HasPrefix(l, sep):
				k = 2
			case k == 2 && strings.HasPrefix(l, end):
				closed = true
			default:
				parts[k] = append(parts[k], l)
			}
			if closed {
				break
			}
		}
		if !closed || k != 2 || strings.TrimSpace(strings.Join(parts[1], "")) != "" {
			return "", false
		}
		b.WriteString(joinAppends(parts[0], parts[2]))
	}
	return b.String(), true
}

// section 是冲突里新加的一段：标题行（开头那段没有标题）和下面的内容。
type section struct {
	head string
	body []string
}

func isHeading(l string) bool { return strings.HasPrefix(l, "#") }

func sections(lines []string) []section {
	out := []section{{}}
	for _, l := range lines {
		if isHeading(l) {
			out = append(out, section{head: l})
			continue
		}
		out[len(out)-1].body = append(out[len(out)-1].body, l)
	}
	return out
}

// joinAppends：默认分支新加的在前，任务新加的在后。任务开头没标题的那段跟在默认分支开头那段后面（同属冲突前的那一节，
// 默认分支在这里新开了版本标题时任务的条目也不会被挪进去）；任务的标题和默认分支这里新加的同名时，内容并到那一节末尾。
func joinAppends(ours, theirs []string) string {
	o, t := sections(ours), sections(theirs)
	o[0].body = joinBody(o[0].body, t[0].body)
	for _, ts := range t[1:] {
		merged := false
		for i := range o[1:] {
			if strings.TrimSpace(o[i+1].head) == strings.TrimSpace(ts.head) {
				o[i+1].body = joinBody(o[i+1].body, ts.body)
				merged = true
				break
			}
		}
		if !merged {
			o = append(o, ts)
		}
	}
	var b strings.Builder
	for _, s := range o {
		if out := b.String(); s.head != "" && out != "" && !strings.HasSuffix(out, "\n\n") { // 拼起来的标题前空一行
			b.WriteString(strings.Repeat("\n", 2-strings.Count(out[len(out)-1:], "\n")))
		}
		b.WriteString(s.head)
		b.WriteString(strings.Join(s.body, ""))
	}
	return ensureNewline(b.String())
}

// joinBody 把同一节里两边新加的内容接起来：都是列表条目就紧挨着，不然中间空一行；结尾的空行留较多的那份。
func joinBody(a, b []string) []string {
	blank := func(l string) bool { return strings.TrimSpace(l) == "" }
	aEnd, bStart, bEnd := len(a), 0, len(b)
	for aEnd > 0 && blank(a[aEnd-1]) {
		aEnd--
	}
	for bStart < len(b) && blank(b[bStart]) {
		bStart++
	}
	if bStart == len(b) {
		return a
	}
	if aEnd == 0 {
		return append(append([]string{}, a...), b[bStart:]...)
	}
	for bEnd > bStart && blank(b[bEnd-1]) {
		bEnd--
	}
	trail := max(len(a)-aEnd, len(b)-bEnd)
	out := append([]string{}, a[:aEnd]...)
	out[len(out)-1] = ensureNewline(out[len(out)-1])
	if !isListItem(a[aEnd-1]) || !isListItem(b[bStart]) {
		out = append(out, "\n")
	}
	out = append(out, b[bStart:bEnd]...)
	out[len(out)-1] = ensureNewline(out[len(out)-1])
	for range trail {
		out = append(out, "\n")
	}
	return out
}

// isListItem：列表条目或它的缩进续行。
func isListItem(l string) bool {
	s := strings.TrimLeft(l, " \t")
	return len(s) < len(l) || strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ")
}

func ensureNewline(s string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// dupHeadings：合出来的文件里某个标题比两边各自的都多，就是合出了重复的标题（比如两个「## 未发布」）。
func dupHeadings(merged, ours, theirs string) bool {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			if isHeading(l) {
				m[strings.TrimSpace(l)]++
			}
		}
		return m
	}
	o, t := count(ours), count(theirs)
	for h, n := range count(merged) {
		if n > max(o[h], t[h]) {
			return true
		}
	}
	return false
}
