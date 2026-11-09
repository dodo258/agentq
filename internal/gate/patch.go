package gate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/gitx"
	"agentq/internal/proc"
	"agentq/internal/textfmt"
)

// 补丁门和 agentq health：纯 git 统计，不调模型。
//   - 派活前：条目看得出要改的模块已修过两次，合同里加「先按重写流程梳理」。看不出就在门这里判。
//   - 合并前：这次是修复（ready.json 的 type），改到的模块 30 天内已修过两次，就是第三次打补丁。warn 档记录，error 档退回，合同改成重写该模块。
//   - 两周返工率（GitClear 口径）只进 health，不阻塞。
//
// 阈值没有公开先例，是我们自己定的，要校准，不对外承诺数字。

const (
	healthWindow = 30 * 24 * time.Hour
	reworkWindow = 14 * 24 * time.Hour
	// patchLimit：上次重写之后已经修过这么多次，再修就是第三次打补丁。
	patchLimit = 2
	// TypeTrailer 是引擎写进合并提交的任务类型：ready.json 不进默认分支，统计靠这一行认。
	TypeTrailer = "Agentq-Type"
	// patchNoteWait：派活前查历史的上限，tick 拿着锁。
	patchNoteWait = 20 * time.Second
	// healthWait：health 算一个项目的上限。
	healthWait = 2 * time.Minute
	// healthRows：health 每个项目最多列几个模块。
	healthRows = 20
)

// CommitKind 是一个提交算什么。
type CommitKind int

const (
	KindOther CommitKind = iota
	KindFix
	KindRewrite
)

// fileChange 是提交里一个文件的改动。
type fileChange struct {
	Path           string
	Added, Removed int
	Created        bool
}

// commitInfo 是默认分支第一父链上的一个提交，合并提交按相对第一父的改动算：一条任务算一次。
type commitInfo struct {
	Hash    string
	When    time.Time
	Message string
	Files   []fileChange
}

// ModuleStat 是一个模块在统计窗口里的情况。
type ModuleStat struct {
	Module string
	// Changes 是改动次数，Fixes 是其中修复提交的次数，Lines 是增删行数。
	Changes, Fixes, Lines int
	// Patches 是上次重写之后的修复次数：补丁门看这个，重写过的模块从零算。
	Patches int
	// PatchCommits 是这些修复提交（短号加标题），新的在前。
	PatchCommits []string
}

// ModuleOf 是文件所属的模块：所在目录；根目录的文件各算一个（单目录项目按文件）。不是代码的（文档、测试、锁文件、点开头的目录、依赖和构建产物）返回空。
func ModuleOf(path string) string {
	switch {
	case path == "" || isTestFile(path):
		return ""
	case strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown"):
		return ""
	}
	parts := strings.Split(path, "/")
	if slices.Contains(lockFiles, parts[len(parts)-1]) {
		return ""
	}
	if len(parts) == 1 {
		return path
	}
	for _, d := range parts[:len(parts)-1] {
		if strings.HasPrefix(d, ".") || slices.Contains(nonCodeDirs, d) {
			return ""
		}
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

var (
	lockFiles   = []string{"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "Cargo.lock", "Podfile.lock", "Package.resolved", "poetry.lock", "uv.lock", "composer.lock", "Gemfile.lock"}
	nonCodeDirs = []string{"docs", "doc", "vendor", "node_modules", "dist", "build", "out", "target", "Pods", "testdata"}

	convTypeRe = regexp.MustCompile(`^([A-Za-z]+)(\([^)]*\))?!?[:：]`)
	fixVerbRe  = regexp.MustCompile(`(?i)^(fix(es|ed|ing)?\b|bug\s?fix|hot\s?fix|修复|修正|修好|修掉|修了)`)
	rewriteRe  = regexp.MustCompile(`(?i)^(rewrite\b|重写)`)
	revertRe   = regexp.MustCompile(`^(Revert "|撤销|回滚|还原)`)
	closesRe   = regexp.MustCompile(`(?i)\bfix(es|ed)?[:：]?\s+([\w.-]+/[\w.-]+)?#\d+`)
	mergeFixRe = regexp.MustCompile(`(?i)^merge (pull request #\d+ from \S+?/|(remote-tracking )?branch '(origin/)?)([\w.-]+/)*(bug|hot)?fix[-/_]`)
	trivialRe  = regexp.MustCompile(`(?i)typo|lint|wording|formatting|拼写|错别字|格式|注释|文案`)
	bugWordRe  = regexp.MustCompile(`(?i)\b(bug|crash(es|ed)?|regression|broken|panic)\b|崩溃|闪退|回归|报错|卡死|死锁|竞态|丢数据`)
)

// notFixTypes：Conventional Commits 里明说不是修复的类型。说明里顺带提到 bug 也不算。
var notFixTypes = []string{"feat", "feature", "docs", "doc", "test", "tests", "chore", "refactor", "style", "ci", "build", "perf", "release", "wip"}

// ClassifyCommit 认一个提交是不是修复、是不是重写。从硬到软，先到先得：
//  1. 引擎写的 Agentq-Type（来自 ready.json 的 type），有就只信它；
//  2. Conventional Commits 的类型；
//  3. 撤销提交、「fixes #12」这种关 issue 的写法、修复分支的合并提交；
//  4. 标题以修复动词开头（改错别字、lint 这类不算）；
//  5. 标题里只是提到 bug、崩溃，要改动也像修复才算：删改了已有代码，没有新建代码文件。
//
// 正文里顺带提到的单个词不算，「fix typo」不算，「加 bug 报告页」（新文件）不算。判断不了的算不是修复，只会少挡，不会误伤。
func ClassifyCommit(msg string, files []fileChange) CommitKind {
	if t := typeTrailer(msg); t != "" {
		switch t {
		case "fix":
			return KindFix
		case "rewrite":
			return KindRewrite
		}
		return KindOther
	}
	subject, body, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	subject = strings.TrimPrefix(strings.TrimSpace(subject), "合并 ") // 升级前的引擎合并提交，标题是「合并 <条目标题>」
	if strings.HasPrefix(subject, "Merge ") {
		if mergeFixRe.MatchString(subject) {
			return KindFix
		}
		if line := firstLine(body); line != "" { // 平台合并提交的第一行正文是 PR 标题
			subject = line
		}
	}
	if m := convTypeRe.FindStringSubmatch(subject); m != nil {
		t := strings.ToLower(m[1])
		switch {
		case t == "rewrite":
			return KindRewrite
		case t == "fix" || t == "bugfix" || t == "hotfix":
			if trivialRe.MatchString(subject) {
				return KindOther
			}
			return KindFix
		case slices.Contains(notFixTypes, t):
			return KindOther
		}
	}
	switch {
	case rewriteRe.MatchString(subject):
		return KindRewrite
	case revertRe.MatchString(subject), closesRe.MatchString(msg):
		return KindFix
	case trivialRe.MatchString(subject):
		return KindOther
	case fixVerbRe.MatchString(subject):
		return KindFix
	case bugWordRe.MatchString(subject) && editsExistingCode(files):
		return KindFix
	}
	return KindOther
}

func firstLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// typeTrailer 取最后一行 Agentq-Type。任务的提交说明会带进合并提交正文，引擎写的在最后。
func typeTrailer(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), TypeTrailer+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// editsExistingCode：删改了已有代码，没有新建代码文件。
func editsExistingCode(files []fileChange) bool {
	removed := false
	for _, f := range files {
		if ModuleOf(f.Path) == "" {
			continue
		}
		if f.Created {
			return false
		}
		removed = removed || f.Removed > 0
	}
	return removed
}

// gitFunc 和 Deps.Git 一样。
type gitFunc func(ctx context.Context, dir string, args ...string) (string, error)

// readCommits 读 ref 第一父链上 since 之后的提交，新的在前。-m 加 --first-parent：合并提交只算相对第一父的改动。
func readCommits(ctx context.Context, git gitFunc, dir, ref string, since time.Time) ([]commitInfo, error) {
	out, err := git(ctx, dir, "log", "--first-parent", "-m", "--no-renames", "--numstat", "--summary", "--format=%x1e%H%x1f%ct%x1f%B%x1f",
		"--since="+since.Format(time.RFC3339), ref)
	if err != nil {
		return nil, err
	}
	var commits []commitInfo
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) != 4 {
			continue
		}
		ct, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("读提交时间：%w", err)
		}
		commits = append(commits, commitInfo{Hash: f[0], When: time.Unix(ct, 0), Message: f[2], Files: parseFileChanges(f[3])})
	}
	return commits, nil
}

func parseFileChanges(text string) []fileChange {
	var files []fileChange
	created := map[string]bool{}
	for _, l := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(l, " create mode "); ok {
			if _, path, ok := strings.Cut(rest, " "); ok {
				created[path] = true
			}
			continue
		}
		f := strings.SplitN(l, "\t", 3)
		if len(f) != 3 {
			continue
		}
		add, _ := strconv.Atoi(f[0]) // 二进制文件是 -，算 0 行
		del, _ := strconv.Atoi(f[1])
		files = append(files, fileChange{Path: f[2], Added: add, Removed: del})
	}
	for i := range files {
		files[i].Created = created[files[i].Path]
	}
	return files
}

// moduleStats 统计 ref 上 since 之后每个模块的改动。重写之前的修复不算进 Patches：重写就是为了清掉它们。
func moduleStats(ctx context.Context, git gitFunc, dir, ref string, since time.Time) ([]ModuleStat, error) {
	commits, err := readCommits(ctx, git, dir, ref, since)
	if err != nil {
		return nil, err
	}
	byName := map[string]*ModuleStat{}
	rewritten := map[string]bool{}
	for _, c := range commits { // 新的在前：走到重写就不再往 Patches 里加
		kind := ClassifyCommit(c.Message, c.Files)
		seen := map[string]bool{}
		for _, f := range c.Files {
			name := ModuleOf(f.Path)
			if name == "" {
				continue
			}
			m := byName[name]
			if m == nil {
				m = &ModuleStat{Module: name}
				byName[name] = m
			}
			m.Lines += f.Added + f.Removed
			if seen[name] {
				continue
			}
			seen[name] = true
			m.Changes++
			if kind == KindFix {
				m.Fixes++
				if !rewritten[name] {
					m.Patches++
					m.PatchCommits = append(m.PatchCommits, textfmt.TruncRunes(c.Hash, 12)+" "+textfmt.OneLine(firstLine(c.Message), 80))
				}
			}
		}
		if kind == KindRewrite {
			for name := range seen {
				rewritten[name] = true
			}
		}
	}
	out := make([]ModuleStat, 0, len(byName))
	for _, m := range byName {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, k int) bool {
		a, b := out[i], out[k]
		switch {
		case a.Patches != b.Patches:
			return a.Patches > b.Patches
		case a.Changes != b.Changes:
			return a.Changes > b.Changes
		}
		return a.Module < b.Module
	})
	return out, nil
}

type patchGate struct{ d Deps }

func (patchGate) Name() string { return "patch" }

// AppliesTo：只看修复。类型由任务在 ready.json 里写，rewrite 就是合同要它做的，feature、docs 不是补丁。
func (patchGate) AppliesTo(c Candidate) bool { return c.Type == "fix" }

func (g patchGate) Run(ctx context.Context, c Candidate) Result {
	stats, err := moduleStats(ctx, g.d.Git, c.ProjectDir, c.Base, g.d.Now().Add(-healthWindow))
	if err != nil {
		return Result{Outcome: Unknown, Summary: "查不了改动历史：" + textfmt.OneLine(err.Error(), 200)}
	}
	touched := map[string]bool{}
	for _, f := range c.Files {
		if m := ModuleOf(f); m != "" {
			touched[m] = true
		}
	}
	var names, evidence []string
	for _, m := range stats {
		if !touched[m.Module] || m.Patches < patchLimit {
			continue
		}
		names = append(names, m.Module)
		evidence = append(evidence, fmt.Sprintf("模块 %s 最近 30 天修过 %d 次：%s", m.Module, m.Patches, strings.Join(m.PatchCommits, "；")))
	}
	if len(names) == 0 {
		return Result{Outcome: Pass, Summary: "这次改到的模块最近 30 天没有反复修"}
	}
	var files []string
	for _, f := range c.Files {
		if slices.Contains(names, ModuleOf(f)) {
			files = append(files, f)
		}
	}
	return Result{Outcome: Fail, Summary: "第三次打补丁：" + strings.Join(names, "、") + " 最近 30 天已修过两次以上，这次又是修复", Evidence: evidence, Files: files}
}

// rewriteContract 是 error 档退回时改的合同。引擎不改代码，重写由任务做。
const rewriteContract = "这次的合同改了：补丁门判定这是第三次打补丁，不再接着修。按 agent-task 第 2 节重写报告里点名的模块：重新梳理它的需求和思路，写清为什么老出错、设计哪里不对；先用测试固定要保留的行为，再重写这个模块。一个任务做不完就拆成待做。改完 ready.json 的 type 写 rewrite。"

// PatchNote 是派活前接在合同后面的话：条目看得出要改的模块已修过两次。def 是这条任务记下的分支。看不出、查不了都是空，到门那里再判。
func (a *Service) PatchNote(dir, def, item string) string {
	ctx, cancel := context.WithTimeout(context.Background(), patchNoteWait)
	defer cancel()
	ref := "refs/heads/" + def
	levels, _ := a.levelsAt(ctx, dir, ref)
	if levelOf("patch", levels) == LevelOff {
		return ""
	}
	stats, err := moduleStats(ctx, a.git, dir, ref, a.Now().Add(-healthWindow))
	if err != nil {
		return ""
	}
	var hot []string
	for _, m := range stats {
		if m.Patches >= patchLimit && itemMentions(item, m.Module) {
			hot = append(hot, fmt.Sprintf("%s（%d 次）", m.Module, m.Patches))
		}
	}
	if len(hot) == 0 {
		return ""
	}
	return "\n\n引擎的统计：这一条看起来要改的模块最近 30 天已经修过两次以上：" + strings.Join(hot, "、") + "。这次如果又是修这些模块，先按 agent-task 第 2 节的重写流程梳理，不接着打补丁，ready.json 的 type 写 rewrite。这一条其实不动它们就不用管。"
}

// itemMentions：条目提到了这个模块：写了完整路径，或者写了模块目录名（前后不是字母数字）。只用来多写一句提醒，多认了不做坏事。
func itemMentions(item, module string) bool {
	if strings.ContainsAny(module, "/.") && strings.Contains(item, module) {
		return true
	}
	base := module[strings.LastIndexByte(module, '/')+1:]
	if len(base) < 3 {
		return false
	}
	re, err := regexp.Compile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(base) + `($|[^A-Za-z0-9_])`)
	return err == nil && re.MatchString(item)
}

// Rework 是两周返工率：窗口内新增的行，有多少在两周内又被删改（GitClear 的 churn 口径）。
type Rework struct {
	Added, Reworked int
}

// reworkStats 按第一父链从旧到新重放 diff，记每一行是什么时候加的。只算代码文件，窗口之前就有的行不算。
func (a *Service) reworkStats(ctx context.Context, dir, ref string, since time.Time) (Rework, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "-c", "core.quotepath=off", "log", "--first-parent", "-m", "--reverse", "-M", "--patch", "--unified=0", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/",
		"--format=%x1e%ct", "--since="+since.Format(time.RFC3339), ref)
	cmd.Env = gitx.GitEnv(a.Home)
	proc.BoundCmd(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Rework{}, fmt.Errorf("读 git log：%w", err)
	}
	if err := cmd.Start(); err != nil {
		return Rework{}, fmt.Errorf("跑 git log：%w", err)
	}
	r, parseErr := replayRework(stdout)
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout) // 读完才能等它退出
	}
	if err := cmd.Wait(); err != nil {
		return Rework{}, fmt.Errorf("跑 git log：%w", err)
	}
	return r, parseErr
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// replayRework 解析 git log -p --unified=0。文件内容行不解析：按 hunk 头的行数跳过，内容长得像 diff 头也不会认错。
func replayRework(src io.Reader) (Rework, error) {
	in := bufio.NewReaderSize(src, 1<<16)
	var res Rework
	born := map[string][]int64{} // 文件 -> 每一行是什么时候加的，0 是窗口之前就有
	var now int64
	var from, to string
	for {
		line, err := in.ReadString('\n')
		if line == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			return res, err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "\x1e"):
			if now, err = strconv.ParseInt(line[1:], 10, 64); err != nil {
				return res, fmt.Errorf("读提交时间：%w", err)
			}
		case strings.HasPrefix(line, "diff --git "):
			from, to = "", ""
		case strings.HasPrefix(line, "rename from "):
			from = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			to = strings.TrimPrefix(line, "rename to ")
			if from != "" {
				born[to] = born[from]
				delete(born, from)
			}
		case strings.HasPrefix(line, "--- "):
			if p, ok := strings.CutPrefix(strings.TrimRight(line[4:], "\t"), "a/"); ok && from == "" {
				from = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p, ok := strings.CutPrefix(strings.TrimRight(line[4:], "\t"), "b/"); ok {
				to = p
			} else if to == "" { // 删除文件：+++ /dev/null
				to = from
			}
		case strings.HasPrefix(line, "@@ "):
			m := hunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			oldN, newStart, newN := hunkCount(m[2]), atoi(m[3]), hunkCount(m[4])
			for i := 0; i < oldN+newN; { // 跳过内容。「\\ No newline at end of file」不算行数
				l, err := in.ReadString('\n')
				if err != nil {
					break
				}
				if !strings.HasPrefix(l, "\\") {
					i++
				}
			}
			if ModuleOf(to) == "" {
				continue
			}
			lines, added, reworked := applyHunk(born[to], oldN, newStart, newN, now)
			born[to] = lines
			res.Added += added
			res.Reworked += reworked
		}
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s) // 正则保证了是数字
	return n
}

func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}

// applyHunk 把一段改动重放到行列表上，返回新列表、新增行数和其中返工的行数。hunk 按行号升序来，前面的已经换算成新行号。
// 列表比文件短时用 0（窗口之前就有）补齐。
func applyHunk(lines []int64, oldN, newStart, newN int, now int64) ([]int64, int, int) {
	at := newStart // 只删不加时 newStart 是删除位置的前一行
	if newN > 0 {
		at = newStart - 1
	}
	if at < 0 {
		at = 0
	}
	if need := at + oldN; len(lines) < need {
		lines = append(lines, make([]int64, need-len(lines))...)
	}
	reworked := 0
	for _, t := range lines[at : at+oldN] {
		if t > 0 && now-t <= int64(reworkWindow/time.Second) {
			reworked++
		}
	}
	fresh := make([]int64, newN)
	for i := range fresh {
		fresh[i] = now
	}
	return slices.Concat(lines[:at], fresh, lines[at+oldN:]), newN, reworked
}

// CmdHealth：agentq health [项目]。只读，不调模型。
func (a *Service) CmdHealth(args []string) error {
	projects, err := a.Backlog.AllProjects()
	if err != nil {
		return fmt.Errorf("projects_root %s 扫不了：%w", a.ProjectsRoot(), err)
	}
	if len(args) == 1 {
		i := slices.IndexFunc(projects, func(p backlog.Project) bool { return p.Name == args[0] })
		if i < 0 {
			return fmt.Errorf("没有项目 %s（agentq projects 看有哪些）", args[0])
		}
		projects = projects[i : i+1]
	}
	if len(projects) == 0 {
		fmt.Fprintln(a.Out, "还没有项目")
		return nil
	}
	for i, p := range projects {
		if i > 0 {
			fmt.Fprintln(a.Out)
		}
		a.printHealth(p)
	}
	fmt.Fprintln(a.Out, "\n修复提交按 ready.json 的类型、提交说明的写法和改动的样子认，判断不了的不算。「再修要重写」的阈值是 agentq 自己定的，还在校准。")
	return nil
}

func (a *Service) printHealth(p backlog.Project) {
	ctx, cancel := context.WithTimeout(context.Background(), healthWait)
	defer cancel()
	def, err := a.Gitx.DefaultBranch(p.Dir)
	if err != nil {
		fmt.Fprintf(a.Out, "%s：认不出默认分支：%s\n", p.Name, textfmt.OneLine(err.Error(), 200))
		return
	}
	ref := "refs/heads/" + def
	if _, err := a.git(ctx, p.Dir, "rev-parse", "--verify", "--quiet", ref); err != nil {
		fmt.Fprintf(a.Out, "%s：%s 还没有提交\n", p.Name, def)
		return
	}
	since := a.Now().Add(-healthWindow)
	stats, err := moduleStats(ctx, a.git, p.Dir, ref, since)
	if err != nil {
		fmt.Fprintf(a.Out, "%s：查不了历史：%s\n", p.Name, textfmt.OneLine(err.Error(), 200))
		return
	}
	fmt.Fprintf(a.Out, "%s（%s，最近 30 天）\n", p.Name, def)
	if len(stats) == 0 {
		fmt.Fprintln(a.Out, "  没有代码改动")
		return
	}
	fmt.Fprintln(a.Out, "  模块\t改动\t修复\t行数")
	for i, m := range stats {
		if i == healthRows {
			fmt.Fprintf(a.Out, "  还有 %d 个模块没列\n", len(stats)-healthRows)
			break
		}
		note := ""
		if m.Patches >= patchLimit {
			note = fmt.Sprintf("\t已修过 %d 次，再修要重写", m.Patches)
		}
		fmt.Fprintf(a.Out, "  %s\t%d\t%d\t%d%s\n", m.Module, m.Changes, m.Fixes, m.Lines, note)
	}
	r, err := a.reworkStats(ctx, p.Dir, ref, since)
	switch {
	case err != nil:
		fmt.Fprintf(a.Out, "  两周返工率算不出：%s\n", textfmt.OneLine(err.Error(), 200))
	case r.Added == 0:
		fmt.Fprintln(a.Out, "  两周返工率：没有新增的代码行")
	default:
		fmt.Fprintf(a.Out, "  两周返工率：%d%%（新增 %d 行，两周内又被改写 %d 行），只作统计\n", r.Reworked*100/r.Added, r.Added, r.Reworked)
	}
}
