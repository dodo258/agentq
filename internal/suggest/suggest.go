// Package suggest 管 BACKLOG「## 建议」的编号、回「做 N」「不做 N」和 docs/NOT-DOING.md 清单。
package suggest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/notify"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是「## 建议」的模块实例。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Gitx    *gitx.Service
	Notify  *notify.Service
}

// 「## 建议」放等用户点头的条目（自我进化的提案写在这里）：tick 只看「待做」，「建议」里的不会被挑中，
// 用户在 Telegram 回「做 1 3」才移进「待做」，回「不做 N」记进 docs/NOT-DOING.md。
// 编号和索引存 ~/.agentq/suggestions.json；BACKLOG 才是真相，条目被删、改过、移走，编号就失效。
const (
	suggestKeepDays = 30 // 索引里的建议留多久：期间不会再提同一个标题
	suggestPastMax  = 500
	suggestSection  = "建议"
	suggestLockWait = 30 * time.Second
)

// SuggestEntry 是留在「## 建议」里等用户点头的一条。
type SuggestEntry struct {
	N           int       `json:"n"`
	Project     string    `json:"project"`
	Fingerprint string    `json:"fingerprint"` // 写进 BACKLOG 的那一行的指纹
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
}

type SuggestStore struct {
	Next  int            `json:"next"`
	Items []SuggestEntry `json:"items"`
	// Past：编号已经作废（用户删了、移走、改了标题）的建议。编号作废后标题还留 suggestKeepDays 天，继续用于判重：
	// 删掉是用户唯一的拒绝方式，不能第二天又冒出来。
	Past []SuggestEntry `json:"past,omitempty"`
}

func (a *Service) SuggestionsPath() string { return filepath.Join(a.DataDir(), "suggestions.json") }

func (a *Service) suggestLockPath() string { return filepath.Join(a.DataDir(), "suggestions.lock") }

// LoadSuggest：文件不存在是正常的；读不了返回错误；内容坏了备份成 .bad-<时间> 后当空的，不崩。
func (a *Service) LoadSuggest() (*SuggestStore, error) {
	b, err := os.ReadFile(a.SuggestionsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &SuggestStore{Next: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &SuggestStore{}
	if err := json.Unmarshal(b, s); err != nil {
		bak := a.SuggestionsPath() + ".bad-" + a.Now().Format("20060102-150405")
		if werr := fsx.WriteFileAtomic(bak, b); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），备份到 %s 也失败：%w", a.SuggestionsPath(), err, bak, werr)
		}
		// 备份后把坏文件重置成空的：只备份这一次、只记这一次日志，后面的读取是正常的空索引
		if werr := fsx.WriteFileAtomic(a.SuggestionsPath(), []byte("{\"next\": 1}\n")); werr != nil {
			return nil, fmt.Errorf("%s 格式不对（%v），已备份为 %s，但重置它失败：%w", a.SuggestionsPath(), err, bak, werr)
		}
		a.Logf("建议：%s 格式不对（%v），已备份为 %s 并重置成空的（编号作废）", a.SuggestionsPath(), err, bak)
		return &SuggestStore{Next: 1}, nil
	}
	s.sanitize()
	return s, nil
}

// sanitize：文件被手改过也不能让编号重复或倒退。
func (s *SuggestStore) sanitize() {
	kept := s.Items[:0]
	seen := map[int]bool{}
	for _, e := range s.Items {
		if e.N <= 0 || seen[e.N] || e.Project == "" || e.Fingerprint == "" {
			continue
		}
		seen[e.N] = true
		kept = append(kept, e)
		s.Next = max(s.Next, e.N+1)
	}
	s.Items = kept
	s.Next = max(s.Next, 1)
}

func (s *SuggestStore) Find(n int) *SuggestEntry {
	for i := range s.Items {
		if s.Items[i].N == n {
			return &s.Items[i]
		}
	}
	return nil
}

func (s *SuggestStore) remove(n int) {
	s.Items = slices.DeleteFunc(s.Items, func(e SuggestEntry) bool { return e.N == n })
}

func (s *SuggestStore) Prune(now time.Time) {
	aged := func(e SuggestEntry) bool { return now.Sub(e.CreatedAt) > suggestKeepDays*24*time.Hour }
	s.Items = slices.DeleteFunc(s.Items, aged)
	s.Past = slices.DeleteFunc(s.Past, aged)
	if len(s.Past) > suggestPastMax {
		s.Past = s.Past[len(s.Past)-suggestPastMax:]
	}
}

// retire：编号作废，标题留在 Past 里继续判重。
func (s *SuggestStore) retire(n int) {
	if e := s.Find(n); e != nil {
		s.Past = append(s.Past, *e)
	}
	s.remove(n)
}

// proposedTitles：同项目 suggestKeepDays 天内提过的建议标题（编号还有效的和已作废的）。
func (s *SuggestStore) Proposed(project string) []SuggestEntry {
	var out []SuggestEntry
	for _, e := range slices.Concat(s.Items, s.Past) {
		if project == "" || e.Project == project {
			out = append(out, e)
		}
	}
	return out
}

// WithSuggest 拿着 suggestions.lock 读、改、原子写回。只做短的读写，不在里面跑 git 或发消息。
func (a *Service) WithSuggest(fn func(s *SuggestStore) error) error {
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(a.suggestLockPath(), suggestLockWait)
	if err != nil {
		return fmt.Errorf("拿 %s：%w", a.suggestLockPath(), err)
	}
	if lock == nil {
		return fmt.Errorf("等了 %s 还拿不到 %s", suggestLockWait, a.suggestLockPath())
	}
	defer lock.Close()
	s, err := a.LoadSuggest()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(a.SuggestionsPath(), append(b, '\n'))
}

// InsertSuggestions 把 newLines 放进「## 建议」节末尾；节不存在就新建，放在「等用户决定」之后（没有它就放在「待做」之后，再没有就放文件末尾）。
func InsertSuggestions(text string, newLines []string) string {
	lines := strings.Split(text, "\n")
	hs := backlog.BacklogHeaders(lines)
	find := func(prefixes ...string) int {
		for k, h := range hs {
			for _, p := range prefixes {
				if strings.HasPrefix(h.Name, p) {
					return k
				}
			}
		}
		return -1
	}
	_, open := backlog.CommentStates(lines)
	// settle：插入点落在没关的注释里时顺延到注释关闭之后（关不上就留着，写完的核对会拒绝）
	settle := func(at, limit int) int {
		for at >= 0 && at < len(lines) && open[at] && at+1 < limit {
			at++
		}
		return at
	}
	var out []string
	if k := find(suggestSection); k >= 0 {
		end := backlog.SectionEnd(hs, k, len(lines))
		at := settle(backlog.LastContent(lines, hs[k].Idx+1, end), end)
		ins := newLines
		if at < hs[k].Idx+1 { // 节里还没有内容：标题后空一行
			at = hs[k].Idx
			ins = append([]string{""}, newLines...)
		}
		out = append(append(append(out, lines[:at+1]...), ins...), lines[at+1:]...)
		return strings.Join(out, "\n")
	}
	at := len(lines) - 1
	decision := -1
	for k, h := range hs {
		if backlog.IsDecisionSection(h.Name) {
			decision = k
			break
		}
	}
	if k := decision; k >= 0 {
		end := backlog.SectionEnd(hs, k, len(lines))
		at = settle(backlog.LastContent(lines, hs[k].Idx+1, end), end)
		if at < hs[k].Idx+1 {
			at = hs[k].Idx
		}
	} else if k := find("待做"); k >= 0 {
		end := backlog.SectionEnd(hs, k, len(lines))
		at = settle(backlog.LastContent(lines, hs[k].Idx+1, end), end)
		if at < hs[k].Idx+1 {
			at = hs[k].Idx
		}
	} else {
		at = settle(backlog.LastContent(lines, 0, len(lines)), len(lines))
	}
	block := append([]string{"", "## " + suggestSection, ""}, newLines...)
	out = append(append(append(out, lines[:at+1]...), block...), lines[at+1:]...)
	return strings.Join(out, "\n")
}

// ErrSuggestGone：「建议」里已经没有这一条（被删、改过或移走）。
var ErrSuggestGone = errors.New("「建议」里已经没有这一条")

// findSuggestionLine 在「## 建议」里找指纹为 fp 的条目（找不到再按标题精确匹配），返回行号，没有返回 -1。注释里的不算。
func findSuggestionLine(lines []string, fp, title string) int {
	hs := backlog.BacklogHeaders(lines)
	skip, _ := backlog.CommentStates(lines)
	hit, byTitle := -1, -1
	for k, h := range hs {
		if !strings.HasPrefix(h.Name, suggestSection) {
			continue
		}
		for i := h.Idx + 1; i < backlog.SectionEnd(hs, k, len(lines)); i++ {
			l := strings.TrimRight(lines[i], "\r")
			if skip[i] || !strings.HasPrefix(l, "- [ ]") {
				continue
			}
			if backlog.Fingerprint(l) == fp {
				hit = i
			} else if byTitle < 0 && title != "" && backlog.ItemTitle(strings.TrimSpace(strings.TrimPrefix(l, "- [ ]"))) == title {
				byTitle = i
			}
		}
	}
	if hit < 0 {
		return byTitle
	}
	return hit
}

// moveSuggestion 把「## 建议」里指纹为 fp 的条目（找不到再按标题精确匹配）移到「待做」末尾，去掉「（建议 N）」标记。
func moveSuggestion(text, fp, title string) (string, error) {
	lines := strings.Split(text, "\n")
	hit := findSuggestionLine(lines, fp, title)
	if hit < 0 {
		return "", ErrSuggestGone
	}
	line := strings.TrimRight(lines[hit], "\r")
	line = strings.TrimRight(backlog.SuggestTagRe.ReplaceAllString(line, ""), " ")
	rest := append(append([]string{}, lines[:hit]...), lines[hit+1:]...)
	text2, ok := backlog.InsertTodo(strings.Join(rest, "\n"), line)
	if !ok {
		return "", errors.New("BACKLOG 里没有「## 待做」一节，或它的末尾卡在没关的注释里")
	}
	if err := backlog.VerifyTodoPresent(text2, []string{line}); err != nil {
		return "", err
	}
	return text2, nil
}

// VerifySuggestionPresent：同上，核对条目在「## 建议」里。
func VerifySuggestionPresent(text string, lines []string) error {
	have := map[string]bool{}
	backlog.EachBacklogLine(text, func(section, line string) {
		if strings.HasPrefix(section, suggestSection) && strings.HasPrefix(line, "- [ ]") {
			have[backlog.Fingerprint(line)] = true
		}
	})
	for _, l := range lines {
		if !have[backlog.Fingerprint(l)] {
			return errors.New("写完核对时条目没有出现在「## 建议」里（多半落进了 HTML 注释），已放弃写入")
		}
	}
	return nil
}

// SuggestUnwritable：这个项目结构上写不进 BACKLOG（没有 docs/BACKLOG.md、主目录不在默认分支、没有可写的「## 待做」），返回原因。
// 这几种不会自己好，要排除在外：不算进闲不闲、不算变化、不给模型看，否则天天跑、天天花钱。
// BACKLOG 有未提交改动、拿不到锁这类临时失败不在这里，仍按“写不进去，下次重试”处理。读不出来的不排除（不知道）。
func (a *Service) SuggestUnwritable(p backlog.Project) string {
	b, err := os.ReadFile(p.BacklogPath())
	if errors.Is(err, fs.ErrNotExist) {
		return "没有 docs/BACKLOG.md"
	}
	if err != nil {
		return ""
	}
	if cur, def, ok := a.Backlog.OnDefault(p); !ok {
		return backlog.NotOnDefaultReason(cur, def)
	}
	if _, ok := backlog.InsertTodo(string(b), "x"); !ok {
		return "BACKLOG 里没有可写的「## 待做」一节"
	}
	return ""
}

// ReserveSuggestNumbers 预留 n 个连续编号（先预留再写 BACKLOG：写失败只浪费编号，不会重号）。返回第一个。
func (a *Service) ReserveSuggestNumbers(n int) (int, error) {
	first := 0
	err := a.WithSuggest(func(s *SuggestStore) error {
		first = s.Next
		s.Next += n
		return nil
	})
	return first, err
}

// SuggestNumbers 取「编号　项目　标题」里的编号，最多 limit 个。
func SuggestNumbers(proposed []string, limit int) []string {
	var out []string
	for _, p := range proposed {
		if n, _, ok := strings.Cut(p, "　"); ok {
			out = append(out, n)
		}
		if len(out) == limit {
			break
		}
	}
	return out
}

var doSuggestRe = regexp.MustCompile(`^做\s*(\d{1,6}(?:[\s,，、]+\d{1,6})*)$`)

// ParseDoSuggest：整条消息就是「做 1 3」（数字之间空格、逗号、顿号都行），别的话都不认。去重、最多 20 个、保持顺序。
func ParseDoSuggest(text string) ([]int, bool) { return parseSuggestNums(text, doSuggestRe) }

func parseSuggestNums(text string, re *regexp.Regexp) ([]int, bool) {
	t := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), "。.！!"))
	m := re.FindStringSubmatch(t)
	if m == nil {
		return nil, false
	}
	var out []int
	seen := map[int]bool{}
	fields := strings.FieldsFunc(m[1], func(r rune) bool { return unicode.IsSpace(r) || r == ',' || r == '，' || r == '、' })
	if len(fields) > 20 {
		return nil, false
	}
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// expire 把索引里已经失效的去掉：超过 suggestKeepDays 天的，项目不在了或 BACKLOG 没了的，「## 建议」里已经找不到的（被删、改了标题或移走）。
// 读不出来（项目目录扫不了、BACKLOG 读错）的保留：不知道就不动。返回是否有变化。
func (a *Service) expire(s *SuggestStore, now time.Time) bool {
	projects, perr := a.Backlog.ActiveProjects()
	byName := map[string]backlog.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	lines := map[string][]string{} // 项目 → BACKLOG 行；nil 表示读不出，保留
	before := len(s.Items) + len(s.Past)
	var kept, retired []SuggestEntry
	aged := func(e SuggestEntry) bool { return now.Sub(e.CreatedAt) > suggestKeepDays*24*time.Hour }
	for _, e := range s.Items {
		if aged(e) {
			continue
		}
		p, ok := byName[e.Project]
		if !ok {
			if perr != nil {
				kept = append(kept, e)
			} else {
				retired = append(retired, e)
			}
			continue
		}
		ls, seen := lines[e.Project]
		if !seen {
			// 主目录不在默认分支（临时切了分支）时读到的不是真正的 BACKLOG：不清理，和 commitBacklog 的前提一致
			if _, _, ok := a.Backlog.OnDefault(p); ok {
				b, err := os.ReadFile(p.BacklogPath())
				switch {
				case err == nil:
					ls = strings.Split(string(b), "\n")
				case errors.Is(err, fs.ErrNotExist):
					ls = []string{}
				}
			}
			lines[e.Project] = ls
		}
		switch {
		case ls == nil || findSuggestionLine(ls, e.Fingerprint, e.Title) >= 0:
			kept = append(kept, e)
		default:
			retired = append(retired, e) // 编号作废，标题留着判重
		}
	}
	s.Items = kept
	s.Past = append(s.Past, retired...)
	s.Prune(now)
	return len(s.Items)+len(s.Past) != before || len(retired) > 0
}

// LiveSuggestions 读索引并清掉失效的条目；save 为真时把清理结果存回去（拿 suggestions.lock），为假时只在内存里清（status 用，不写文件）。
func (a *Service) LiveSuggestions(save bool) (*SuggestStore, error) {
	if _, err := os.Stat(a.SuggestionsPath()); err != nil {
		return &SuggestStore{Next: 1}, nil
	}
	if !save {
		s, err := a.LoadSuggest()
		if err != nil {
			return nil, err
		}
		a.expire(s, a.Now())
		return s, nil
	}
	var out *SuggestStore
	err := a.WithSuggest(func(s *SuggestStore) error {
		a.expire(s, a.Now())
		out = s
		return nil
	})
	return out, err
}

// hasSuggestions：索引里有还在 BACKLOG「建议」里等用户点头的建议。读不了当没有。
func (a *Service) hasSuggestions() bool {
	s, err := a.LiveSuggestions(true)
	return err == nil && len(s.Items) > 0
}

// DoSuggestApplies：这条「做 N」该由代码处理：编号里至少有一个是索引里还有效的建议，且不是在回复某条已登记的回复链消息（那是在回那件事，交给回复链）。
func (a *Service) DoSuggestApplies(m *telegram.TgMsg, nums []int) bool {
	s, err := a.LiveSuggestions(true)
	if err != nil {
		return false
	}
	any := false
	for _, n := range nums {
		any = any || s.Find(n) != nil
	}
	if !any { // 一个有效编号都没有：不是在说这些建议，放行给模型
		return false
	}
	if m.ReplyTo == nil {
		return true
	}
	ns, err := a.Notify.LoadNotify()
	if err != nil {
		return true
	}
	_, threaded := ns.Threads[m.ReplyTo.MessageID]
	return !threaded
}

// DoSuggestions 把编号对应的建议从「建议」移进「待做」并逐个提交，返回回复用户的消息（HTML）。
// 条目已被删、改过或移走、编号不存在都算失效，不报错；提交失败（主目录脏、不在 main）的留着，下次还能做。
func (a *Service) DoSuggestions(nums []int) string {
	done, gone, failed := a.eachSuggestion(nums, "做", func(p backlog.Project, e SuggestEntry) error {
		return a.Backlog.CommitBacklog(p, "待办："+textfmt.TruncRunes(e.Title, 40)+"（闲时建议，用户确认）", func(old string) (string, error) {
			return moveSuggestion(old, e.Fingerprint, e.Title)
		})
	}, func(n int, e SuggestEntry) {
		if err := a.WithSuggest(func(s *SuggestStore) error { s.remove(n); return nil }); err != nil {
			a.Logf("建议：编号 %d 处理完了，但索引没改成：%v", n, err)
		}
		a.Logf("建议：用户确认了建议 %d，%s 的「%s」已移进待做", n, e.Project, textfmt.TruncRunes(e.Title, 30))
	})
	return a.suggestReply("已移进「待做」", done, gone, failed)
}

// retireSuggestion：编号作废，标题留着继续判重。
func (a *Service) retireSuggestion(n int) {
	if err := a.WithSuggest(func(s *SuggestStore) error { s.retire(n); return nil }); err != nil {
		a.Logf("建议：编号 %d 已失效，但索引没改成：%v", n, err)
	}
}

// eachSuggestion 逐个处理编号：apply 改项目文件（「建议」里没了返回 errSuggestGone），成功后调 finish 处理索引。verb 是用户说的字（做、不做），写日志用。
func (a *Service) eachSuggestion(nums []int, verb string, apply func(p backlog.Project, e SuggestEntry) error, finish func(n int, e SuggestEntry)) (done, gone, failed []string) {
	projects, perr := a.Backlog.ActiveProjects()
	byName := map[string]backlog.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	for _, n := range nums {
		var e SuggestEntry
		found := false
		if err := a.WithSuggest(func(s *SuggestStore) error {
			if x := s.Find(n); x != nil {
				e, found = *x, true
			}
			return nil
		}); err != nil {
			failed = append(failed, fmt.Sprintf("%d（索引读不了：%s）", n, textfmt.OneLine(err.Error(), 60)))
			continue
		}
		if !found {
			gone = append(gone, fmt.Sprintf("%d（没有这个编号，或已经处理过）", n))
			continue
		}
		p, ok := byName[e.Project]
		if !ok {
			if perr != nil {
				failed = append(failed, fmt.Sprintf("%d（扫描项目目录失败）", n))
				continue
			}
			a.retireSuggestion(n)
			gone = append(gone, fmt.Sprintf("%d（项目 %s 已经不在了）", n, textfmt.OneLine(e.Project, 30)))
			continue
		}
		err := apply(p, e)
		switch {
		case errors.Is(err, ErrSuggestGone):
			a.retireSuggestion(n)
			gone = append(gone, fmt.Sprintf("%d（%s 的「建议」里已经没有这一条，被删、改过或移走了）", n, e.Project))
		case err != nil:
			a.Logf("建议：%s %d 失败：%v", verb, n, err)
			failed = append(failed, fmt.Sprintf("%d（%s）", n, textfmt.OneLine(err.Error(), 80)))
		default:
			finish(n, e)
			done = append(done, fmt.Sprintf("%d　%s　%s", n, e.Project, textfmt.TruncRunes(e.Title, 40)))
		}
	}
	return done, gone, failed
}

func (a *Service) suggestReply(head string, done, gone, failed []string) string {
	var secs []notify.MsgSection
	if len(done) > 0 {
		secs = append(secs, notify.MsgSection{Emoji: "🟢", Head: fmt.Sprintf("%s　%d 条", head, len(done)), Lines: done})
	}
	if len(gone) > 0 {
		secs = append(secs, notify.MsgSection{Emoji: "🟠", Head: "编号失效　没动", Lines: gone})
	}
	if len(failed) > 0 {
		secs = append(secs, notify.MsgSection{Emoji: "🔴", Head: "没能移", Lines: failed})
	}
	return a.Notify.RenderMessage("done", a.Now(), secs, "agentq")
}
