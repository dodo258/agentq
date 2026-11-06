package suggest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/textfmt"
)

// 「不做」清单：用户说不做的事写进项目 docs/NOT-DOING.md（标题、原话、日期）。闲时找活和主动感知按标题永久判重（「以前提过」只留 30 天，这里不过期），
// 提示词也带上这份清单；agent-task 开工前看它。入口：用户回「不做 N」（代码处理，不起模型）和 Router 的 reject 动作。
// 内容是用户的话，但写入前也走 cleanTodoText，免得拼出 BACKLOG 的标记。
const (
	NotDoingRel       = "docs/NOT-DOING.md"
	NotDoingTextMax   = 300
	notDoingPromptMax = 40   // 提示词里最多列多少条（留最新的）
	notDoingPromptLen = 2000 // 提示词里这份清单的总字数上限
)

// NotDoingKeys 用来判重（标题，和 titleKey 同样的归一化）。读不了只记日志，当没有：判重是省用户的事，不能挡住别的。
func (a *Service) NotDoingKeys(p backlog.Project) map[string]bool {
	entries, err := backlog.NotDoingEntries(p)
	if err != nil {
		a.Logf("不做清单：%s 的 %s 读不了，这次不按它判重：%v", p.Name, NotDoingRel, err)
	}
	keys := map[string]bool{}
	for _, e := range entries {
		keys[backlog.TitleKey(e.Title)] = true
	}
	return keys
}

// NotDoingBlock 拼提示词里的这份清单。总量有上限，超了留最新的（文件按时间追加，同一天保持文件里的先后）。没有条目返回空。
func (a *Service) NotDoingBlock(head string, names []string) string {
	type row struct {
		date, line string
	}
	var rows []row
	for _, n := range names {
		entries, err := backlog.NotDoingEntries(backlog.Project{Name: n, Dir: a.ProjectDir(n)})
		if err != nil {
			a.Logf("不做清单：%s 的 %s 读不了，提示词里不带：%v", n, NotDoingRel, err)
			continue
		}
		for _, e := range entries {
			rows = append(rows, row{e.Date, "- " + textfmt.OneLine(n, 100) + "：" + backlog.CleanTodoText(e.Title, 60)})
		}
	}
	sort.SliceStable(rows, func(i, k int) bool { return rows[i].date < rows[k].date })
	var kept []string
	used := 0
	for i := len(rows) - 1; i >= 0 && len(kept) < notDoingPromptMax; i-- {
		used += len([]rune(rows[i].line))
		if used > notDoingPromptLen {
			break
		}
		kept = append(kept, rows[i].line)
	}
	if len(kept) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(head + "\n")
	for i := len(kept) - 1; i >= 0; i-- {
		b.WriteString(kept[i] + "\n")
	}
	return b.String()
}

// CommitNotDoing 拿着 <项目>-main 锁，把一条写进 docs/NOT-DOING.md（同标题已有就不重复写），同时按 editBacklog 改 BACKLOG，一起提交这两个文件。
// 前提和 commitBacklog 一样：主目录在默认分支、这两个文件没有没提交的改动。提交失败两个文件都改回去。
func (a *Service) CommitNotDoing(p backlog.Project, commitMsg string, e *backlog.NotDoingEntry, editBacklog func(old string) (string, error)) error {
	unlock, err := a.Backlog.LockMain(p.Name+"-main", backlog.MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	git := func(args ...string) (string, error) { return a.Gitx.Git(p.Dir, args...) }
	def, err := a.Gitx.DefaultBranch(p.Dir)
	if err != nil {
		return err
	}
	if br, err := git("symbolic-ref", "--short", "HEAD"); err != nil || br != def {
		return fmt.Errorf("主目录不在 %s（%s）", def, textfmt.OneLine(br, 60))
	}
	backlogRel, err := backlog.BacklogRepoPath(p) // BACKLOG 是软链接时要提交它指向的文件，同 commitBacklog
	if errors.Is(err, fs.ErrNotExist) {
		backlogRel = "docs/BACKLOG.md"
	} else if err != nil {
		return err
	}
	if out, err := git("status", "--porcelain", "--", backlogRel, NotDoingRel); err != nil || out != "" {
		return fmt.Errorf("BACKLOG 或不做清单有没提交的改动，或查不了 git 状态（%s）", textfmt.OneLine(out, 100))
	}
	oldNot, err := os.ReadFile(p.NotDoingPath())
	hadNot := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !hadNot { // 读不到但路径上有东西：指向不存在的软链接。写不进去，也不能当成没有文件，撤销时会把链接删掉
		if _, lerr := os.Lstat(p.NotDoingPath()); lerr == nil {
			return fmt.Errorf("%s 是指向不存在文件的软链接，没改", NotDoingRel)
		}
	}
	oldBacklog, err := os.ReadFile(p.BacklogPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	newBacklog, err := editBacklog(string(oldBacklog)) // e 的原话可能在这里才算出来
	if err != nil {
		return err
	}
	newNot := string(oldNot)
	dup := false
	for _, x := range backlog.ParseNotDoing(newNot) {
		dup = dup || backlog.TitleKey(x.Title) == backlog.TitleKey(e.Title)
	}
	if !dup {
		newNot = backlog.AppendNotDoing(newNot, *e)
	}
	var paths []string
	if newBacklog != string(oldBacklog) {
		paths = append(paths, backlogRel)
	}
	if newNot != string(oldNot) {
		paths = append(paths, NotDoingRel)
	}
	if len(paths) == 0 {
		return nil
	}
	restore := func() {
		if newNot != string(oldNot) {
			if out, err := git("reset", "-q", "--", NotDoingRel); err != nil {
				a.Logf("不做清单：%s 撤销暂存失败：%v %s", p.Name, err, textfmt.OneLine(out, 120))
			}
			if hadNot {
				_ = fsx.WriteFileAtomic(p.NotDoingPath(), oldNot)
			} else {
				_ = os.Remove(p.NotDoingPath())
			}
		}
		if newBacklog != string(oldBacklog) {
			_ = fsx.WriteKeepMode(p.BacklogPath(), oldBacklog)
		}
	}
	if newBacklog != string(oldBacklog) {
		if err := fsx.WriteKeepMode(p.BacklogPath(), []byte(newBacklog)); err != nil {
			return err
		}
	}
	if newNot != string(oldNot) {
		if err := fsx.WriteFileAtomic(p.NotDoingPath(), []byte(newNot)); err != nil {
			restore()
			return err
		}
		if out, err := git("add", "--", NotDoingRel); err != nil { // 新文件要先 add，commit 的路径才认
			restore()
			return fmt.Errorf("暂存不做清单失败，已改回：%v %s", err, textfmt.OneLine(out, 200))
		}
	}
	if out, err := git(append([]string{"commit", "-q", "-m", commitMsg, "--"}, paths...)...); err != nil {
		restore()
		return fmt.Errorf("提交失败，已改回：%v %s", err, textfmt.OneLine(out, 200))
	}
	return nil
}

var notDoSuggestRe = regexp.MustCompile(`^不做\s*(\d{1,6}(?:[\s,，、]+\d{1,6})*)$`)

// ParseNotDoSuggest：整条消息就是「不做 1 3」。写法和「做 1 3」一样。
func ParseNotDoSuggest(text string) ([]int, bool) { return parseSuggestNums(text, notDoSuggestRe) }

// RemoveSuggestion 把「## 建议」里这一条删掉，返回新内容和这一行的正文（去掉编号标记）。
func RemoveSuggestion(text, fp, title string) (string, string, error) {
	lines := strings.Split(text, "\n")
	hit := findSuggestionLine(lines, fp, title)
	if hit < 0 {
		return "", "", ErrSuggestGone
	}
	body := strings.TrimRight(lines[hit], "\r")
	body = strings.TrimSpace(strings.TrimPrefix(backlog.SuggestTagRe.ReplaceAllString(body, ""), "- [ ]"))
	return strings.Join(append(append([]string{}, lines[:hit]...), lines[hit+1:]...), "\n"), body, nil
}

// RejectSuggestions 把编号对应的建议从「建议」移进不做清单，各提交一次，返回回复用户的消息（HTML）。失效和提交失败的处理同 doSuggestions。
func (a *Service) RejectSuggestions(nums []int, said string) string {
	said = textfmt.OneLine(said, 40)
	done, gone, failed := a.eachSuggestion(nums, "不做", func(p backlog.Project, e SuggestEntry) error {
		entry := &backlog.NotDoingEntry{Title: backlog.CleanTodoText(e.Title, NotDoingTextMax), Date: a.Now().Format("2006-01-02")}
		return a.CommitNotDoing(p, "待办："+textfmt.TruncRunes(e.Title, 40)+"（用户说不做）", entry, func(old string) (string, error) {
			text, body, err := RemoveSuggestion(old, e.Fingerprint, e.Title)
			if err != nil {
				return "", err
			}
			entry.Quote = backlog.CleanTodoText("用户回「"+said+"」；条目："+body, NotDoingTextMax)
			return text, nil
		})
	}, func(n int, e SuggestEntry) {
		a.retireSuggestion(n) // 编号作废；标题在 Past 里留 30 天，不做清单里一直留着
		a.Logf("闲时找活：用户不做建议 %d，%s 的「%s」已写进不做清单", n, e.Project, textfmt.TruncRunes(e.Title, 30))
	})
	return a.suggestReply("已写进「不做」清单，以后不再提", done, gone, failed)
}
