// Package backlog 管理项目的 BACKLOG 文本：解析、编辑、提交、归档计划、不做清单。
package backlog

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/textfmt"
)

// Service 是读写 BACKLOG 用的模块实例。
type Service struct {
	*env.Env
	Gitx *gitx.Service
}

const archiveKeepFirst = 20 // 「已完成」最前面保留的条数（新的在前）

// 合并提交在这个时间内的条目也保留。要大于 quality 的 7 天窗口，一次满意率和返工率才不受归档影响。
const ArchiveKeepFor = 14 * 24 * time.Hour

// 进行中标记写的时间超过这么久就当过期标记：超时、被杀的任务留下的标记没有人清。
const ArchiveMarkFresh = 24 * time.Hour

const ArchiveHead = "# 待办归档\n\n`agentq backlog archive` 从 BACKLOG「已完成」移来的旧条目，新的在前。\n\n"

var markTimeRe = regexp.MustCompile(`（进行中[：:]\s*\S+\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2})\s*）`)

func (p Project) ArchivePath() string { return filepath.Join(p.Dir, "docs", "BACKLOG-archive.md") }

type archivePlan struct {
	Kept, Moved int
	Backlog     string // 归档后的 BACKLOG
	Archive     string // 归档后的归档文件；Moved=0 时不用写
}

// PlanArchive 只动「已完成」节里 HTML 注释之外的「- [x]」行和紧跟的缩进续行，其余字节原样留下。
// commitTime 查合并提交的时间，查不到返回 ok=false。
func PlanArchive(backlog, archive string, commitTime func(hash string) (time.Time, bool), now time.Time) archivePlan {
	var plan archivePlan
	var kept, moved []string
	inDone := false
	var cm CommentTracker
	lines := strings.SplitAfter(backlog, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		if cm.Skip(line) {
			kept = append(kept, lines[i])
			continue
		}
		if strings.HasPrefix(line, "## ") {
			inDone = strings.HasPrefix(strings.TrimSpace(line[3:]), "已完成")
		}
		// 这一行本身开了跨行注释的不算条目：移走它会让后面的注释失去开头
		if !inDone || cm.in || !(strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]")) {
			kept = append(kept, lines[i])
			continue
		}
		end := i + 1
		for end < len(lines) && isContinuation(lines[end]) {
			end++
		}
		entry := lines[i:end]
		first := strings.TrimSpace(line[len("- [x]"):])
		plan.Kept++ // 先按保留算，要移走时再减
		if plan.Kept+plan.Moved <= archiveKeepFirst || archiveKeeps(first, strings.Join(entry, ""), commitTime, now) {
			kept = append(kept, entry...)
		} else {
			plan.Kept--
			plan.Moved++
			moved = append(moved, entry...)
		}
		i = end - 1
	}
	plan.Backlog = strings.Join(kept, "")
	if plan.Moved > 0 {
		plan.Archive = PrependArchive(archive, moved)
	}
	return plan
}

// isContinuation：条目的续行是紧跟着的缩进行；空行、注释都不是。
func isContinuation(raw string) bool {
	return (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) && strings.TrimSpace(raw) != "" && !strings.Contains(raw, "<!--")
}

// archiveKeeps 判断排在前 archiveKeepFirst 条之外的条目要不要留下：合并提交在 archiveKeepFor 内的留下；
// 查不到合并时间的验收意见也留下，它可能正指向窗口内的条目，移走会改变那一条的一次满意率。其余移走。
func archiveKeeps(first, whole string, commitTime func(string) (time.Time, bool), now time.Time) bool {
	if m := LastMergedCommit(whole); m != nil {
		if t, ok := commitTime(whole[m[2]:m[3]]); ok {
			return now.Sub(t) <= ArchiveKeepFor
		}
	}
	return len(FeedbackTargets(first)) > 0
}

// PrependArchive 把条目按原顺序放到归档文件第一条之前（文件头留在最上面）。
func PrependArchive(old string, moved []string) string {
	var b strings.Builder
	for _, l := range moved {
		b.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			b.WriteByte('\n')
		}
	}
	if old == "" {
		return ArchiveHead + b.String()
	}
	at := len(old)
	if !strings.HasSuffix(old, "\n") {
		old += "\n"
		at = len(old)
	}
	for off := 0; off < len(old); {
		if strings.HasPrefix(old[off:], "- [x]") || strings.HasPrefix(old[off:], "- [X]") {
			at = off
			break
		}
		n := strings.IndexByte(old[off:], '\n')
		if n < 0 {
			break
		}
		off += n + 1
	}
	return old[:at] + b.String() + old[at:]
}

// StaleMark：条目的进行中标记写了时间，并且已经超过 archiveMarkFresh。读不出时间的不算过期。
func StaleMark(text string, now time.Time) bool {
	m := markTimeRe.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", m[1], now.Location())
	return err == nil && now.Sub(t) > ArchiveMarkFresh
}

type Item struct {
	// Project：这一条所在的项目（Project.items 填）。指纹只按原文算，不同项目里一样的文字指纹相同，比较任务时要连项目一起比。
	Project     string
	Text        string // 「- [ ] 」之后的原文
	Title       string // 「——」之前的部分，去掉进行中标记
	Fingerprint string
	InProgress  bool
	Stuck       bool
	// Deps：（依赖：…）写的字，一个标记一段；Scope：（范围：…）写的路径前缀。见 ItemDeps、ItemScope。
	Deps  []string
	Scope []string
	// DepsWaiting：还没满足的依赖（「已完成」里没有标题含这段字的条目）；DepsMissing 是其中整份 BACKLOG 都找不到的，多半写错了。Items 按整份 BACKLOG 和归档填。
	DepsWaiting []string
	DepsMissing []string
}

func (it Item) Eligible() bool { return !it.InProgress && !it.Stuck }

// RunKeyword：给用户敲 `agentq run <项目> "<关键词>"` 用的一段字：在「——」前的标题部分里，取第一段不含 shell 双引号里
// 要转义的字符、也不在（…）标记里面的文字，最多 12 个字。一定是原文的一段，run 按原文找得到这一条；
// 不取标记里的字（如「低风险」），免得 run 挑到前面另一条同样标了的条目。
func (it Item) RunKeyword() string {
	head, _, _ := strings.Cut(it.Text, "——")
	depth := 0
	var seg []rune
	flush := func() string {
		s := strings.TrimSpace(string(seg))
		seg = seg[:0]
		if r := []rune(s); len(r) >= 2 {
			return strings.TrimSpace(string(r[:min(len(r), 12)]))
		}
		return ""
	}
	for _, r := range head + "\x00" {
		switch {
		case r == '（':
			depth++
		case r == '）' && depth > 0:
			depth--
			continue
		case depth == 0 && r != '\x00' && !strings.ContainsRune("\"$`\\）", r):
			seg = append(seg, r)
			continue
		case depth > 0:
			continue
		}
		if s := flush(); s != "" {
			return s
		}
	}
	return ""
}

// StuckMark 是条目上的（卡住：…）标记，别的写法取不到时返回空。
func (it Item) StuckMark() string { return stuckMarkRe.FindString(it.Text) }

var InProgressRe = regexp.MustCompile(`（进行中[^）]*）`)

// 卡住标记只认 agent-task 写的两种形式，正文里提到这个词（如验收标准）不算：
//   - 括号标记：括号紧挨着「卡住」、后面接冒号、右括号或空白——（卡住：…）、(卡住)、【卡住】、[卡住 …]，位置不限；
//   - 行尾标记：去掉「（进行中…）」、行尾空白和句末标点后以「卡住」结尾，前面是行首、破折号（— 或 ——）或「；，。|」。
var (
	stuckMarkRe    = regexp.MustCompile(`（卡住[^）]*）`)
	stuckBracketRe = regexp.MustCompile(`[（(【\[]\s*卡住(\s|[：:）)】\]]|$)`)
	stuckTailRe    = regexp.MustCompile(`(^|[—；;，,。|])\s*卡住$`)
)

// 等条件标记：「等用户决定」里的条目写着 `（等条件：…）`，表示要等前置条件满足才轮到用户定（如「前面 7 条都合并」）。
// 只认括号紧挨「等条件」再接冒号的写法（全角或半角括号、冒号都行），正文里顺带提到这个词不算；条件由做完最后一条前置任务的 agent 去掉标记。
var waitCondRe = regexp.MustCompile(`[（(]\s*等条件\s*[：:]\s*([^）)]*)[）)]`)

// 「等用户决定」一节。老项目的节标题写成「等某某拍板」，继续认，不用改文件。
var legacyDecisionRe = regexp.MustCompile(`^等\p{Han}{1,4}拍板`)

func IsDecisionSection(section string) bool {
	return strings.HasPrefix(section, "等用户决定") || legacyDecisionRe.MatchString(section)
}

// WaitCondition 返回条目里等条件标记写的条件（去掉首尾空白）；没有标记返回 ok=false。
func WaitCondition(text string) (cond string, ok bool) {
	m := waitCondRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

func isStuck(text string) bool {
	if stuckBracketRe.MatchString(text) {
		return true
	}
	t := strings.TrimSpace(InProgressRe.ReplaceAllString(text, ""))
	return stuckTailRe.MatchString(strings.TrimRight(t, " \t。.；;，,！!"))
}

// Fingerprint = 条目原文去掉「（进行中…）」后的 sha1。
func Fingerprint(line string) string {
	s := strings.TrimSpace(InProgressRe.ReplaceAllString(line, ""))
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SameItemText：条目是不是这一条。指纹和标题都连着卡住标记算（标题没有「——」时），写上、去掉标记都变，比标题要去掉标记。
func SameItemText(it Item, fp, title string) bool {
	return it.Fingerprint == fp || (title != "" && dropStuckMark(it.Title) == dropStuckMark(title))
}

func dropStuckMark(s string) string { return strings.TrimSpace(stuckMarkRe.ReplaceAllString(s, "")) }

// ParseBacklog 只看「## 待做」到下一个「## 」之间、行首是「- [ ]」的行，跳过 HTML 注释。
func ParseBacklog(text string) []Item {
	var items []Item
	EachBacklogLine(text, func(section, line string) {
		if !strings.HasPrefix(section, "待做") || !strings.HasPrefix(line, "- [ ]") {
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
		if text == "" {
			return
		}
		items = append(items, Item{
			Text:        text,
			Title:       ItemTitle(text),
			Fingerprint: Fingerprint(line),
			InProgress:  strings.Contains(line, "（进行中"),
			Stuck:       isStuck(text),
			Deps:        ItemDeps(text),
			Scope:       ItemScope(text),
		})
	})
	resolveDeps(items, text, "")
	return items
}

// 依赖和范围标记只认括号紧挨「依赖」「范围」再接冒号的写法（全角或半角括号、冒号都行），正文里顺带提到不算，
// dispatch 模板里「；范围：已知页面/文件」这种不带括号的写法也不算。
var (
	depRe   = regexp.MustCompile(`[（(]\s*依赖\s*[：:]\s*([^）)]*)[）)]`)
	scopeRe = regexp.MustCompile(`[（(]\s*范围\s*[：:]\s*([^）)]*)[）)]`)
	scopeSp = regexp.MustCompile(`[,，、]`)
)

// ItemDeps：条目写的依赖，每个（依赖：…）一段，去掉首尾空白，空的不算。
func ItemDeps(text string) []string {
	var out []string
	for _, m := range depRe.FindAllStringSubmatch(text, -1) {
		if d := strings.TrimSpace(m[1]); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// ItemScope：条目写的范围，路径前缀，逗号或顿号分隔，可以写多个标记。去掉反引号、开头的「./」「/」；只写「.」「/」的是整个仓库，记成空串（是任何路径的前缀）。
func ItemScope(text string) []string {
	var out []string
	for _, m := range scopeRe.FindAllStringSubmatch(text, -1) {
		for _, s := range scopeSp.Split(m[1], -1) {
			s = strings.Trim(strings.TrimSpace(s), "`")
			if s == "" {
				continue
			}
			for strings.HasPrefix(s, "./") {
				s = s[2:]
			}
			s = strings.TrimLeft(s, "/")
			if s == "." {
				s = ""
			}
			out = append(out, s)
		}
	}
	return out
}

// ScopesOverlap：两组范围有一对是另一条的前缀（相等也算）。只比字符串前缀，宁可多判重叠：多判只是少并行。
func ScopesOverlap(a, b []string) (string, string, bool) {
	for _, x := range a {
		for _, y := range b {
			if strings.HasPrefix(x, y) || strings.HasPrefix(y, x) {
				return x, y, true
			}
		}
	}
	return "", "", false
}

// ParallelBlock：同项目有这些条目（running，任务记的条目原文）在跑时，it 能不能一起跑；能返回空，不能返回原因。
// 要 it 写了范围、在跑的每条都写了范围、两两不重叠；没写范围的当会撞。
func ParallelBlock(it Item, running []string) string {
	if len(running) == 0 {
		return ""
	}
	if len(it.Scope) == 0 {
		return "没写（范围：…），不和在跑的任务并行"
	}
	for _, r := range running {
		sc := ItemScope(r)
		if len(sc) == 0 {
			return "在跑的「" + ItemTitle(r) + "」没写范围，不并行"
		}
		if x, y, ok := ScopesOverlap(it.Scope, sc); ok {
			return fmt.Sprintf("范围 %s 和在跑的「%s」的 %s 重叠，不并行", x, ItemTitle(r), y)
		}
	}
	return ""
}

// resolveDeps 填每条的 DepsWaiting、DepsMissing。「已完成」节（和归档）里的「- [x]」标题含这段字才算满足；
// 别的节里还没勾的条目（不算自己）有匹配就是在等它，两边都没有就是找不到。
func resolveDeps(items []Item, text, archive string) {
	var done, open []string
	var openFP []string
	EachBacklogLine(text, func(section, line string) {
		switch {
		case strings.HasPrefix(section, "已完成") && (strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]")):
			done = append(done, ItemTitle(strings.TrimSpace(line[len("- [x]"):])))
		case !strings.HasPrefix(section, "已完成") && strings.HasPrefix(line, "- [ ]"):
			open = append(open, ItemTitle(strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))))
			openFP = append(openFP, Fingerprint(line))
		}
	})
	EachBacklogLine(archive, func(_, line string) {
		if strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]") {
			done = append(done, ItemTitle(strings.TrimSpace(line[len("- [x]"):])))
		}
	})
	contains := func(titles []string, d string, skip string) bool {
		for i, t := range titles {
			if strings.Contains(t, d) && (skip == "" || openFP[i] != skip) {
				return true
			}
		}
		return false
	}
	for i := range items {
		items[i].DepsWaiting, items[i].DepsMissing = nil, nil
		for _, d := range items[i].Deps {
			if contains(done, d, "") {
				continue
			}
			items[i].DepsWaiting = append(items[i].DepsWaiting, d)
			if !contains(open, d, items[i].Fingerprint) {
				items[i].DepsMissing = append(items[i].DepsMissing, d)
			}
		}
	}
}

func hasDeps(items []Item) bool {
	return slices.ContainsFunc(items, func(it Item) bool { return len(it.Deps) > 0 })
}

// DepNote：依赖找不到时给 status 的一句话，没有是空。
func (it Item) DepNote() string {
	if len(it.DepsMissing) == 0 {
		return ""
	}
	return "「" + it.Title + "」依赖『" + strings.Join(it.DepsMissing, "』『") + "』找不到：BACKLOG 和归档里都没有标题含这段字的条目，写错了就改，不改会一直等"
}

// doneCommitRe：agent-task 移到「已完成」时写的「合并提交 <号>」。
var doneCommitRe = regexp.MustCompile("合并提交\\s*[:：]?\\s*`?([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")

// DoneMergeCommit 在「## 已完成」里找标题等于 title 的「- [x]」条目，返回第一条写的合并提交号；没有返回空。
func DoneMergeCommit(text, title string) string {
	inDone := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			inDone = strings.HasPrefix(strings.TrimSpace(line[3:]), "已完成")
			continue
		}
		if !inDone || !(strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]")) {
			continue
		}
		t := strings.TrimSpace(line[len("- [x]"):])
		if ItemTitle(t) != title {
			continue
		}
		if m := doneCommitRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

func ItemTitle(text string) string {
	t := stripMarks(waitCondRe.ReplaceAllString(InProgressRe.ReplaceAllString(text, ""), ""))
	if i := strings.Index(t, "——"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// lowRiskMark：固定标记，这一条用 Config.LowRiskModel。只认三个位置：标题开头、标题结尾、条目行尾。
// 进行中标记不算在内。正文中间、标题中间顺带提到都不算（LESSONS：自由文本只认固定格式）。
const lowRiskMark = SuggestLowRiskMark

func LowRiskItem(text string) bool { return markedItem(text, lowRiskMark) }

// ProductMark 标在条目上：这一条要出能给人看的成品，过成品门、出交付单。
const ProductMark = "（成品）"

// ProductItem：条目标了（成品）。写法同（低风险），正文顺带提到的不算。
func ProductItem(text string) bool { return markedItem(text, ProductMark) }

// IrreversibleMark 标在条目上：这一条改错了难撤回，门过了也等用户拍板才合。
const IrreversibleMark = "（难撤回）"

// IrreversibleItem：条目标了（难撤回）。写法同（低风险），正文顺带提到的不算。
func IrreversibleItem(text string) bool { return markedItem(text, IrreversibleMark) }

func markedItem(text, mark string) bool {
	t := ItemTitle(text)
	return strings.HasPrefix(t, mark) || strings.HasSuffix(t, mark) ||
		strings.HasSuffix(strings.TrimSpace(stripMarks(InProgressRe.ReplaceAllString(text, ""))), mark)
}

// stripMarks 去掉方向、依赖、范围标记：它们写在哪都不改变标题，也不挡（低风险）这类标记的位置判断。
func stripMarks(s string) string {
	return scopeRe.ReplaceAllString(depRe.ReplaceAllString(directionRe.ReplaceAllString(s, ""), ""), "")
}

// DiscussMark 标在条目上：这一条开工前要和用户商量大方向（设计、界面、前后端选型）。写法同（低风险）。
const DiscussMark = "（大方向）"

// DiscussItem：条目标了（大方向）。
func DiscussItem(text string) bool { return markedItem(text, DiscussMark) }

// directionRe：选定的大方向，写在条目行尾，只认这个固定写法。写进条目后标题和（低风险）这些标记都不受它影响。
var directionRe = regexp.MustCompile(`（方向：[^）]*）`)

// directionNeutral：方向是模型或用户写的话，不能被解析成别的标记，也不能截断自己。
var directionNeutral = strings.NewReplacer("（", "(", "）", ")", "等条件", "等 条件", "——", "—")

// DirectionMark 是写进条目的方向标记，一行，n 字以内。
func DirectionMark(text string, n int) string {
	return "（方向：" + CleanTodoText(directionNeutral.Replace(text), n) + "）"
}

// Direction：条目写的大方向，没有是空。
func Direction(text string) string {
	m := directionRe.FindString(text)
	if m == "" {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(m, "（方向："), "）")
}

var (
	noDowngradeRe = regexp.MustCompile(`(?i)重写|重构|迁移|审查|审计|rewrite|refactor|migrat|review|audit`)
)

// NoDowngradeItem：标题提到重写、重构、迁移、审查这类活，标了（低风险）也不降模型。这里故意不要求固定格式（LESSONS 那条管的是判断状态）：多认了只是不省额度，不会做坏。
func NoDowngradeItem(text string) bool { return noDowngradeRe.MatchString(ItemTitle(text)) }

// heavyRe：要用长任务强度的活（重写、重构、迁移）。比 noDowngradeRe 窄：标题里顺带提到「审查」「审计」的不算长任务。
var heavyRe = regexp.MustCompile(`(?i)重写|重构|迁移|rewrite|refactor|migrat`)

// HeavyItem：重写、重构、迁移，或标了（难撤回）的条目，用 Config.HeavyEffort。
func HeavyItem(text string) bool {
	return heavyRe.MatchString(ItemTitle(text)) || IrreversibleItem(text)
}

type Project struct {
	Name string
	Dir  string
}

func (p Project) BacklogPath() string { return filepath.Join(p.Dir, "docs", "BACKLOG.md") }

// Items 读项目 BACKLOG；文件不存在当作没有条目。
func (p Project) Items() ([]Item, error) {
	b, err := os.ReadFile(p.BacklogPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	items := ParseBacklog(string(b))
	if hasDeps(items) { // 依赖也认已经归档的条目；归档读不出就只看 BACKLOG（多等，不会误放）
		arch, _ := os.ReadFile(p.ArchivePath())
		resolveDeps(items, string(b), string(arch))
	}
	for i := range items {
		items[i].Project = p.Name
	}
	return items, nil
}

// Excluded：这个项目在 exclude_projects 里，自动流程不碰它。
func (a *Service) Excluded(name string) bool { return slices.Contains(a.Cfg.ExcludeProjects, name) }

// ActiveProjects 是自动流程看的项目：tick、巡逻、自我进化的提案都走这里，排除 exclude_projects。明确点名的命令（run、add、init）另走。
// 错误的意思和 AllProjects 一样：返回的不全。
func (a *Service) ActiveProjects() ([]Project, error) {
	projects, err := a.AllProjects()
	return slices.DeleteFunc(projects, func(p Project) bool { return a.Excluded(p.Name) }), err
}

// AllProjects 是 projects_root 下一层加 extra_projects，按名字排序，不看 exclude_projects。
// projects_root 不存在时按 RootMayBeAbsent 算没有项目。登记过但已经不在或不再是仓库的跳过，agentq status 说明。
// 扫不了 projects_root 时 extra_projects 的项目照样返回，同时返回错误：列表不全，「不在列表里」不等于项目没了。
func (a *Service) AllProjects() ([]Project, error) {
	projects, err := ScanProjects(a.ProjectsRoot())
	if errors.Is(err, fs.ErrNotExist) && a.RootMayBeAbsent() {
		err = nil
	}
	for _, dir := range a.Cfg.ExtraProjects {
		name := filepath.Base(dir)
		if !strings.HasPrefix(name, ".") && a.ProjectDir(name) == dir && gitx.IsGitMain(dir) &&
			!slices.ContainsFunc(projects, func(p Project) bool { return p.Name == name }) {
			projects = append(projects, Project{Name: name, Dir: dir})
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, err
}

// MissingExtras 是 extra_projects 里已经用不了的目录，给 status 说明。
func (a *Service) MissingExtras() []string {
	var out []string
	for _, dir := range a.Cfg.ExtraProjects {
		if a.ProjectDir(filepath.Base(dir)) != dir || !gitx.IsGitMain(dir) {
			out = append(out, dir)
		}
	}
	return out
}

// ScanProjects 列出 root 下一层、按名字排序的 git 主目录，跳过以 . 开头的目录。
func ScanProjects(root string) ([]Project, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Project
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(root, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if !gitx.IsGitMain(dir) {
			continue
		}
		out = append(out, Project{Name: name, Dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

const notDoingHeader = "# 不做清单\n\n用户明确不做的事。自我进化的提案按标题永久判重，任务开工前会看。\n"

type NotDoingEntry struct {
	Title string
	Quote string
	Date  string
}

func (p Project) NotDoingPath() string { return filepath.Join(p.Dir, "docs", "NOT-DOING.md") }

// ParseNotDoing 认固定格式：「## 标题」，下面「- 原话：」「- 日期：」。用户手写的只有标题也算一条。
func ParseNotDoing(text string) []NotDoingEntry {
	var out []NotDoingEntry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "## "):
			if t := strings.TrimSpace(line[3:]); t != "" {
				out = append(out, NotDoingEntry{Title: t})
			}
		case len(out) > 0 && strings.HasPrefix(line, "- 原话："):
			out[len(out)-1].Quote = strings.TrimSpace(strings.TrimPrefix(line, "- 原话："))
		case len(out) > 0 && strings.HasPrefix(line, "- 日期："):
			out[len(out)-1].Date = strings.TrimSpace(strings.TrimPrefix(line, "- 日期："))
		}
	}
	return out
}

func AppendNotDoing(text string, e NotDoingEntry) string {
	if strings.TrimSpace(text) == "" {
		text = notDoingHeader
	}
	return strings.TrimRight(text, "\n") + "\n\n## " + e.Title + "\n- 原话：" + e.Quote + "\n- 日期：" + e.Date + "\n"
}

// NotDoingEntries：文件不存在当空的；读不了返回错误。
func NotDoingEntries(p Project) ([]NotDoingEntry, error) {
	b, err := os.ReadFile(p.NotDoingPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseNotDoing(string(b)), nil
}

// OrphanFacts 是判断一条孤儿标记时查到的 git 事实。
type OrphanFacts struct {
	Named    string    // 标记里写的分支名，没写为空
	Branch   string    // Named 且确实存在的分支，不存在为空
	Default  string    // 项目的默认分支
	Ahead    int       // Branch 比默认分支多几个提交
	Worktree string    // Branch 的 worktree 目录，没有为空
	Dirty    bool      // Worktree 有未提交改动（含未跟踪文件）
	Activity time.Time // 分支最后提交时间和 worktree 文件修改时间里最新的（找到晚于 since 的就不再找）
}

// MarkerBranch 从进行中标记里取分支名。main、master 不算（不是任务分支；默认分支叫别的时 OrphanFacts 再去掉），「-」开头的不当分支（防止被 git 当成参数），
// 紧跟冒号的是日期也不算（`（进行中：2026-10-11 14:29）`）。
func MarkerBranch(text string) string {
	m := BranchRe.FindStringSubmatch(text)
	if m == nil || strings.HasPrefix(m[1], "-") || m[1] == "main" || m[1] == "master" ||
		markerFullDateRe.MatchString(m[1]) || markerDateTimeRe.MatchString(m[1]) || markerShortDateRe.MatchString(m[1]) {
		return ""
	}
	return m[1]
}

var (
	markerFullDateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	markerDateTimeRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	markerShortDateRe = regexp.MustCompile(`^\d{1,2}-\d{1,2}$`)
	markerClockRe     = regexp.MustCompile(`^\d{2}:\d{2}`)
)

// MarkerTime 从进行中标记里取时间（agent-task 写的 `2026-10-11 14:29`，也认 T 分隔和不写年份的 `10-11 14:29`）。
// 只看标记里以空白分开的整段，分支名里的数字不算。不写年份时取不晚于现在的那一年；只有日期时按当天最后一分钟算（宁可多等）。
func MarkerTime(text string, now time.Time) (time.Time, bool) {
	m := InProgressRe.FindString(text)
	if m == "" {
		return time.Time{}, false
	}
	loc := now.Location()
	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(m, "（进行中"), "）"))
	clock := func(i int) string {
		if i < len(fields) && markerClockRe.MatchString(fields[i]) {
			return fields[i][:5]
		}
		return ""
	}
	for i, f := range fields {
		f = strings.TrimLeft(f, "：:")
		switch {
		case markerDateTimeRe.MatchString(f):
			if t, err := time.ParseInLocation("2006-01-02T15:04", f[:16], loc); err == nil {
				return t, true
			}
		case markerFullDateRe.MatchString(f):
			c := clock(i + 1)
			if c == "" {
				c = "23:59"
			}
			if t, err := time.ParseInLocation("2006-01-02 15:04", f+" "+c, loc); err == nil {
				return t, true
			}
		case markerShortDateRe.MatchString(f) && clock(i+1) != "":
			t, err := time.ParseInLocation("2006-1-2 15:04", fmt.Sprintf("%d-%s %s", now.Year(), f, clock(i+1)), loc)
			if err != nil {
				continue
			}
			if t.After(now) {
				t = t.AddDate(-1, 0, 0)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// EditTodoLine 改「待做」节里指纹为 fp、原文仍是 want 的那一行（保留行尾的 \r），跳过注释。条目变了就不改，返回 false。
func EditTodoLine(text, fp, want string, fn func(line string) string) (string, bool) {
	lines := strings.Split(text, "\n")
	section := ""
	var cm CommentTracker
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if cm.Skip(line) {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line[3:])
		}
		if !strings.HasPrefix(section, "待做") || !strings.HasPrefix(line, "- [ ]") || Fingerprint(line) != fp {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "- [ ]")) != want {
			return text, false
		}
		lines[i] = fn(line) + raw[len(line):]
		return strings.Join(lines, "\n"), true
	}
	return text, false
}

// MarkerAtEnd：标记在行尾才算（agent-task 写在条目后面）。正文里顺带提到「（进行中）」的条目不当孤儿，不去改正文。
// 引擎写在行尾的（方向：…）跟在后面也算：认领时标记常插在它前面（11-12 新项目第一条被门退回后因此等了 24 小时）。
var MarkerAtEnd = regexp.MustCompile(`（进行中[^）]*）(（方向：[^）]*）)?[\s。.；;，,！!]*$`)

func DropMarker(line string) string {
	return strings.TrimRight(InProgressRe.ReplaceAllString(line, ""), " \t")
}

// OrphanFacts 查标记写的分支的现状。查不了（不是 git 仓库、认不出默认分支）返回错误，调用方当未知、不动。
func (a *Service) OrphanFacts(p Project, it Item, since time.Time) (OrphanFacts, error) {
	return a.OrphanFactsOn(p, it, since, "")
}

// OrphanFactsOn 是 OrphanFacts，这一条上一个任务记下了基于哪条分支就比那条，没有才现认。
func (a *Service) OrphanFactsOn(p Project, it Item, since time.Time, recorded string) (OrphanFacts, error) {
	f := OrphanFacts{Named: MarkerBranch(it.Text)}
	git := func(args ...string) (string, error) { return a.Gitx.Git(p.Dir, args...) }
	def := recorded
	if def == "" {
		var err error
		if def, err = a.Gitx.DefaultBranch(p.Dir); err != nil {
			return f, err
		}
	}
	f.Default = def
	if out, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+def); err != nil {
		return f, fmt.Errorf("查 %s：%w %s", def, err, textfmt.OneLine(out, 100))
	}
	if f.Named == def { // 标记写的是默认分支本身，不是任务分支
		f.Named = ""
	}
	if f.Named == "" {
		return f, nil
	}
	ref := "refs/heads/" + f.Named
	if out, err := git("rev-parse", "--verify", "--quiet", ref); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 { // --quiet 下分支不存在就是退出码 1
			return f, nil
		}
		return f, fmt.Errorf("查分支 %s：%w %s", f.Named, err, textfmt.OneLine(out, 100))
	}
	f.Branch = f.Named
	out, err := git("rev-list", "--count", "refs/heads/"+def+".."+ref)
	if err != nil {
		return f, fmt.Errorf("查分支 %s 比 %s 多几个提交：%w %s", f.Named, def, err, textfmt.OneLine(out, 100))
	}
	if f.Ahead, err = strconv.Atoi(out); err != nil {
		return f, fmt.Errorf("查分支 %s 比 %s 多几个提交：%w", f.Named, def, err)
	}
	out, err = git("log", "-1", "--format=%ct", ref)
	if err != nil {
		return f, fmt.Errorf("查分支 %s 的最后提交：%w %s", f.Named, err, textfmt.OneLine(out, 100))
	}
	sec, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return f, fmt.Errorf("查分支 %s 的最后提交：%w", f.Named, err)
	}
	f.Activity = time.Unix(sec, 0)
	out, err = git("worktree", "list", "--porcelain")
	if err != nil {
		return f, fmt.Errorf("查 worktree：%w %s", err, textfmt.OneLine(out, 100))
	}
	f.Worktree = gitx.WorktreeOf(out, ref)
	if f.Worktree == "" {
		return f, nil
	}
	out, err = a.Gitx.Git(f.Worktree, "status", "--porcelain")
	if err != nil {
		return f, fmt.Errorf("查 worktree %s 的改动：%w %s", f.Worktree, err, textfmt.OneLine(out, 100))
	}
	f.Dirty = out != ""
	if t := gitx.LatestMod(f.Worktree, since); t.After(f.Activity) {
		f.Activity = t
	}
	return f, nil
}

// MainLockWait：写接手待做时排队等 <项目>-main 锁最多多久（tick 拿着自己的锁，不能等太久）。测试里改小。
var MainLockWait = 2 * time.Minute

// MainLockNoPidStale：锁目录里没有 pid 文件超过这么久，当旧锁清掉。
const MainLockNoPidStale = time.Minute

// CommitTodo 在「待做」节末尾插入 line，只提交这个文件；BACKLOG 里已经提到 jobID 就不写。没写、但不算出错时返回说明（如已经写过）。
func (a *Service) CommitTodo(p Project, jobID, line, commitMsg string) (string, error) {
	note := ""
	err := a.CommitBacklog(p, commitMsg, func(old string) (string, error) {
		if strings.Contains(old, jobID) {
			note = "BACKLOG 里已经有提到任务 " + jobID + " 的条目，没再写"
			return old, nil
		}
		text, ok := InsertTodo(old, line)
		if !ok {
			return "", errors.New("BACKLOG 里没有「## 待做」一节")
		}
		return text, nil
	})
	return note, err
}

// CommitBacklog 拿着 <项目>-main 锁（和别的任务、夜班、复盘排队改主目录）：主目录在默认分支、BACKLOG 没有没提交的改动时，
// 按 edit 改 BACKLOG 并只提交这个文件；edit 没改内容就什么都不做；提交失败把文件改回去。
// 自动写 BACKLOG 的路都走这里，exclude_projects 的硬约束守在这一个地方。例外是用户点名的：agentq add、回复被放下的任务走 CommitBacklogNamed，agentq run 的任务过门后引擎写收尾走 CommitBacklogLocked。
func (a *Service) CommitBacklog(p Project, commitMsg string, edit func(old string) (string, error)) error {
	if a.Excluded(p.Name) {
		return fmt.Errorf("%s 在 exclude_projects 里，agentq 不自动写它的 BACKLOG", p.Name)
	}
	return a.CommitBacklogNamed(p, commitMsg, edit)
}

// CommitBacklogNamed 是 commitBacklog 本身，给用户明确点名这个项目的入口用（agentq add、回复被放下的任务）。
func (a *Service) CommitBacklogNamed(p Project, commitMsg string, edit func(old string) (string, error)) error {
	unlock, err := a.LockMain(p.Name+"-main", MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	return a.CommitBacklogLocked(p, commitMsg, edit)
}

// CommitBacklogNamedOn 是 CommitBacklogNamed，分支是任务记下的，不现认。
func (a *Service) CommitBacklogNamedOn(p Project, def, commitMsg string, edit func(old string) (string, error)) error {
	unlock, err := a.LockMain(p.Name+"-main", MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	return a.CommitBacklogOn(p, def, commitMsg, edit)
}

// CommitBacklogLocked 是 CommitBacklogNamed 的本体，调用方已经拿着 <项目>-main 锁。also 是调用方已经写好、要进同一个提交的别的文件（仓库相对路径）。
func (a *Service) CommitBacklogLocked(p Project, commitMsg string, edit func(old string) (string, error), also ...string) error {
	def, err := a.Gitx.DefaultBranch(p.Dir)
	if err != nil {
		return err
	}
	return a.CommitBacklogOn(p, def, commitMsg, edit, also...)
}

// CommitBacklogOn 是 CommitBacklogLocked 的本体，默认分支已经认好：同一次过门里前后要用同一个名字。
func (a *Service) CommitBacklogOn(p Project, def, commitMsg string, edit func(old string) (string, error), also ...string) error {
	git := func(args ...string) (string, error) { return a.Gitx.Git(p.Dir, args...) }
	if br, err := git("symbolic-ref", "--short", "HEAD"); err != nil || br != def {
		return fmt.Errorf("主目录不在 %s（%s）", def, textfmt.OneLine(br, 60))
	}
	rel, err := BacklogRepoPath(p)
	if err != nil {
		return err
	}
	if out, err := git("status", "--porcelain", "--", rel); err != nil || out != "" {
		return fmt.Errorf("BACKLOG 有没提交的改动或查不了 git 状态（%s）", textfmt.OneLine(out, 100))
	}
	b, err := os.ReadFile(p.BacklogPath())
	if err != nil {
		return err
	}
	text, err := edit(string(b))
	if err != nil || text == string(b) {
		return err
	}
	if err := fsx.WriteKeepMode(p.BacklogPath(), []byte(text)); err != nil {
		return err
	}
	if out, err := git(append([]string{"commit", "-q", "-m", commitMsg, "--", rel}, also...)...); err != nil {
		// 只在文件还是自己写的样子时改回去，免得盖掉别人刚写的
		if cur, rerr := os.ReadFile(p.BacklogPath()); rerr == nil && string(cur) == text {
			_ = fsx.WriteKeepMode(p.BacklogPath(), b)
		}
		return fmt.Errorf("提交失败，BACKLOG 已改回：%v %s", err, textfmt.OneLine(out, 200))
	}
	return nil
}

// BacklogRepoPath：BACKLOG 在仓库里真正要提交的路径。软链接指向的文件才是内容；指向仓库外的提交不了，不写。
func BacklogRepoPath(p Project) (string, error) {
	real, err := filepath.EvalSymlinks(p.BacklogPath())
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(p.Dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("BACKLOG 是指向仓库外的软链接（%s），提交不了，没改", real)
	}
	return rel, nil
}

// LockMain 按 with-lock.sh 的约定占锁：目录 <LocksDir>/<名字>，里面 pid 写持有进程、owner 写这次占锁的令牌。
// 旧锁（持有进程不在了，或建了很久还没有 pid）只由建出 .<名字>.reap 的那一个清理者在里面重看一遍、仍是旧锁才删，
// 免得几个等锁的都看到旧锁、先后删掉别人刚占上的新锁。释放只删 owner 还是自己的锁。最多等 wait，返回释放函数。
func (a *Service) LockMain(name string, wait time.Duration) (func(), error) {
	locks := a.LocksDir()
	dir := filepath.Join(locks, name)
	reap := filepath.Join(locks, "."+name+".reap")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		return nil, err
	}
	owner := fmt.Sprintf("%d.%d.%d", os.Getpid(), rand.Uint64(), time.Now().UnixNano())
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "owner"), []byte(owner+"\n"), 0o644)
			return func() {
				if b, _ := os.ReadFile(filepath.Join(dir, "owner")); strings.TrimSpace(string(b)) == owner {
					os.RemoveAll(dir)
				}
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		stale, holder := staleLock(dir)
		if stale && reapLock(dir, reap) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等了 %s 还拿不到锁 %s（持有进程 %s）", wait, dir, holder)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// staleLock：持有进程不在了；或者没有 pid（占锁的进程在建目录和写 pid 之间被杀了，写 pid 只要一瞬间，这么久还没有就是没了）。
func staleLock(dir string) (stale bool, holder string) {
	b, _ := os.ReadFile(filepath.Join(dir, "pid"))
	holder = strings.TrimSpace(string(b))
	if pid, err := strconv.Atoi(holder); err == nil {
		return pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH), holder
	}
	fi, err := os.Stat(dir)
	return err == nil && time.Since(fi.ModTime()) > MainLockNoPidStale, holder
}

// reapLock：建出 reap 目录才有清理权，在里面重看一遍，仍是旧锁才删；拿不到清理权返回 false。
// 清理者在建 reap 和删 reap 之间被杀会留下它，旧过 MainLockNoPidStale 就删掉，下一轮再抢。
func reapLock(dir, reap string) bool {
	if err := os.Mkdir(reap, 0o755); err != nil {
		if fi, serr := os.Stat(reap); serr == nil && time.Since(fi.ModTime()) > MainLockNoPidStale {
			_ = os.Remove(reap)
		}
		return false
	}
	defer os.Remove(reap)
	if stale, _ := staleLock(dir); stale {
		os.RemoveAll(dir)
	}
	return true
}

// CommentStates 逐行给出：skip 这一行整行在 HTML 注释里；open 处理完这一行后仍在一个没关的注释里。
func CommentStates(lines []string) (skip, open []bool) {
	skip, open = make([]bool, len(lines)), make([]bool, len(lines))
	var cm CommentTracker
	for i, l := range lines {
		skip[i] = cm.Skip(strings.TrimRight(l, "\r"))
		open[i] = cm.in
	}
	return skip, open
}

// InsertTodo 把 line 插到「## 待做」节最后一个条目之后；节里没有条目就插在节末最后一个非空行之后。
// HTML 注释里的标题和条目（模板示例）不算；插入点落在没关的注释里时顺延到注释关闭之后，节内关不上就返回 false。
func InsertTodo(text, line string) (string, bool) {
	lines := strings.Split(text, "\n")
	skip, open := CommentStates(lines)
	start := -1
	for i, l := range lines {
		if !skip[i] && strings.HasPrefix(l, "## ") && strings.HasPrefix(strings.TrimSpace(l[3:]), "待做") {
			start = i
			break
		}
	}
	if start < 0 {
		return text, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if !skip[i] && strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	at := start
	for i := start + 1; i < end; i++ {
		if !skip[i] && strings.HasPrefix(lines[i], "- [") {
			at = i
		}
	}
	if at == start {
		for i := end - 1; i > start; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				at = i
				break
			}
		}
	}
	for open[at] && at+1 < end {
		at++
	}
	if open[at] {
		return text, false
	}
	out := append([]string{}, lines[:at+1]...)
	out = append(out, line)
	out = append(out, lines[at+1:]...)
	return strings.Join(out, "\n"), true
}

var mergedCommitRe = regexp.MustCompile("合并(?:提交|[，,]\\s*提交)[\\s:：`*]*([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")

// 不带引号的写法要求「验收」后面有空白：「验收后按审查人的意见改」这类正文不算
var feedbackRe = regexp.MustCompile(`验收(?:[ \t]*「([^」]+)」|[ \t]+([^\s：:；;。，,「」][^；;。「」]*?))[ \t]*的意见`)

// LastMergedCommit 取条目里最后一处「合并提交 <号>」的位置（FindStringSubmatchIndex 的格式）：
// 合并说明写在需求后面，需求里顺带写的「修上次合并提交 X 引入的…」不算本条的合并提交。
func LastMergedCommit(text string) []int {
	all := mergedCommitRe.FindAllStringSubmatchIndex(text, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// BacklogEntry 是 BACKLOG 里的一条（任何一节、勾没勾都算），HTML 注释里的不算。
type BacklogEntry struct {
	Section string // 「## 」后面的节名
	Done    bool   // 「- [x]」
	Text    string // 「- [ ] 」「- [x] 」之后的原文
	Title   string
}

// CommentTracker 跟踪 BACKLOG 里的 HTML 注释。
type CommentTracker struct{ in bool }

// Skip 判断这一行整行都在注释里（调用方跳过它），并更新状态；行中间开始的跨行注释，这一行本身不跳过，cm.in 变真。
func (c *CommentTracker) Skip(line string) bool {
	if c.in {
		if strings.Contains(line, "-->") {
			c.in = false
		}
		return true
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "<!--") {
		if !strings.Contains(trimmed[4:], "-->") {
			c.in = true
		}
		return true
	}
	if start := strings.Index(line, "<!--"); start >= 0 && !strings.Contains(line[start:], "-->") {
		c.in = true
	}
	return false
}

// EachBacklogLine 逐行给出 BACKLOG 里 HTML 注释之外的行和它所在的节名（标题行本身也给出）。
func EachBacklogLine(text string, f func(section, line string)) {
	section := ""
	var cm CommentTracker
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if cm.Skip(line) {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line[3:])
		}
		f(section, line)
	}
}

func BacklogEntries(text string) []BacklogEntry {
	var out []BacklogEntry
	EachBacklogLine(text, func(section, line string) {
		var done bool
		switch {
		case strings.HasPrefix(line, "- [ ]"):
		case strings.HasPrefix(line, "- [x]"), strings.HasPrefix(line, "- [X]"):
			done = true
		default:
			return
		}
		t := strings.TrimSpace(line[len("- [ ]"):])
		if t == "" {
			return
		}
		out = append(out, BacklogEntry{Section: section, Done: done, Text: t, Title: ItemTitle(t)})
	})
	return out
}

// FeedbackTargets 取条目里「验收 <标题> 的意见」的标题；模板占位（<…>）不算。
func FeedbackTargets(text string) []string {
	var out []string
	for _, m := range feedbackRe.FindAllStringSubmatch(text, -1) {
		x := strings.TrimSpace(m[1] + m[2])
		if x == "" || strings.HasPrefix(x, "<") && strings.HasSuffix(x, ">") {
			continue
		}
		out = append(out, x)
	}
	return out
}

var normReplacer = strings.NewReplacer("`", "", "*", "", "「", "", "」", "", "“", "", "”", "", "\"", "", "'", "")

func NormTitle(s string) string {
	return strings.ToLower(normReplacer.Replace(strings.Join(strings.Fields(s), "")))
}

// GithubRepos 从 git remote -v 的输出取 github.com 上的仓库（owner/name，去重）。
func GithubRepos(remotes string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(remotes, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if m := gitx.GithubRemoteRe.FindStringSubmatch(f[1]); m != nil {
			if repo := m[1] + "/" + m[2]; !seen[repo] {
				seen[repo] = true
				out = append(out, repo)
			}
		}
	}
	return out
}

const SuggestLowRiskMark = "（低风险）"

// suggestNeutral 在 cleanTodoText 之外再改写几种会被 BACKLOG 解析当成标记的写法：等条件、「验收…的意见」、「——」（标题到此为止）。
var suggestNeutral = strings.NewReplacer("等条件", "等 条件", "的意见", "的 意见", "——", "—", "（建议 ", "(建议 ")

// SuggestField 清洗模型给的一段文字：去控制字符和非法编码、压成一行、截断、中和标记写法。
func SuggestField(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(suggestNeutral.Replace(CleanTodoText(s, n)))
}

// TitleKey：判断标题重复用。小写、去掉空白和标点。
func TitleKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

var SuggestTagRe = regexp.MustCompile(`（建议 \d+）\s*$`)

type backlogHeader struct {
	Idx  int
	Name string
}

// BacklogHeaders 列出 HTML 注释之外的「## 」标题。
func BacklogHeaders(lines []string) []backlogHeader {
	var out []backlogHeader
	var cm CommentTracker
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if cm.Skip(l) {
			continue
		}
		if strings.HasPrefix(l, "## ") {
			out = append(out, backlogHeader{i, strings.TrimSpace(l[3:])})
		}
	}
	return out
}

// SectionEnd：第 k 个标题的节在哪一行结束（下一个标题的行号，或文件末尾）。
func SectionEnd(hs []backlogHeader, k, n int) int {
	if k+1 < len(hs) {
		return hs[k+1].Idx
	}
	return n
}

// LastContent 返回 [from, to) 里最后一个非空行的行号，没有返回 from-1。
func LastContent(lines []string, from, to int) int {
	for i := to - 1; i >= from; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return from - 1
}

// BacklogTitleKeys：BACKLOG 里已有条目的标题（待做、建议、等用户决定、已完成……），用来判重。
// 「以后再说」不算：它正是建议的第一来源，模型照它的标题提出来不是重复；提出来之后原条目留在原处，
// 同一标题已经在待做、建议或已完成里，下次就算重复。
func BacklogTitleKeys(text string) map[string]bool {
	keys := map[string]bool{}
	for _, e := range BacklogEntries(text) {
		if strings.HasPrefix(e.Section, "以后再说") {
			continue
		}
		keys[TitleKey(e.Title)] = true
	}
	return keys
}

// VerifyTodoPresent：写完之后用 parseBacklog 核对，这些行真的在「## 待做」里（没有被插进注释之类的地方）。
func VerifyTodoPresent(text string, lines []string) error {
	have := map[string]bool{}
	for _, it := range ParseBacklog(text) {
		have[it.Fingerprint] = true
	}
	for _, l := range lines {
		if !have[Fingerprint(l)] {
			return errors.New("写完核对时条目没有出现在「## 待做」里（多半落进了 HTML 注释），已放弃写入")
		}
	}
	return nil
}

// OnDefault：主目录当前是不是在默认分支上。返回当前分支名（分离 HEAD 或读不出时为空）和默认分支（认不出时为空）。
func (a *Service) OnDefault(p Project) (cur, def string, ok bool) {
	def, err := a.Gitx.DefaultBranch(p.Dir)
	if err != nil {
		def = ""
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, ".git", "HEAD"))
	if err != nil {
		return "", def, false
	}
	cur, isRef := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !isRef {
		cur = ""
	}
	return cur, def, cur != "" && cur == def
}

// NotOnDefaultReason 是 OnDefault 为假时给人看的那句话。
func NotOnDefaultReason(cur, def string) string {
	if def == "" {
		return "认不出默认分支（当前在 " + textfmt.OneLine(cur, 30) + "）"
	}
	return "主目录不在 " + def + "（" + textfmt.OneLine(cur, 30) + "）"
}

// todoNeutral 改写模型文字里会被 BACKLOG 解析当成标记的写法：进行中、卡住（这一条会永远不被挑）、注释开头（后面的条目全被当成注释）。
// 用替换不用删除：删除后前后文字可能拼回一个标记。
var todoNeutral = strings.NewReplacer("（进行中", "(进行中", "卡住", "卡 住", "<!--", "< !--")

func CleanTodoText(s string, n int) string {
	return strings.TrimSpace(todoNeutral.Replace(textfmt.OneLine(s, n)))
}

var BranchRe = regexp.MustCompile(`（进行中[：:]\s*([^\s）]+)`)

const TakeOverPrefix = "接手中断的「"
