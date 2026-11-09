package gate

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"agentq/internal/textfmt"
)

// 退回时限定范围：门没过，引擎定下一轮只该改哪些文件，写进退回提示；下一轮由 scope 门查有没有越界。
//   - 范围 = 没过的门在 Result.Files 里点名的文件；都没点名就用这一轮任务改的文件（Candidate.Files）；再加这些文件同目录下已有的测试文件。
//   - 同目录新加的测试文件也算范围内。多于 maxScope 个按目录折叠（「dir/」表示这个目录下都算），折叠后还多就不限定。
//   - scope 门只在退回之后的轮次跑：比较上一轮分支尖到这次分支尖之间任务自己的提交改的文件。从默认分支合进来的提交不算。

const maxScope = 30

// planAdvice 是退回用完还没过时的说法：多半是条目拆法的问题，不是再改一轮能解决的。
const planAdvice = "多半是计划（条目拆法）的问题：建议重新拆或改验收"

// planReason：到上限转用户时的原因，通知、卡住标记、报告都用它。
func planReason(rounds int, why string) string {
	s := fmt.Sprintf("退回 %d 轮还没过，%s", rounds, planAdvice)
	if why = textfmt.OneLine(why, 200); why != "" {
		s += "。最后一次：" + why
	}
	return s
}

// returnScope 算退回后的范围。rev 是看同目录测试文件的提交（候选合并提交或分支尖），空的就不加测试文件。算不出返回空：不限定。
// 只收挡住合并的门点名的文件：按这次实际生效的档位（影响面广时 warn 升了 error 的也算），warn 档没过只是记录，不收窄范围。
func (a *Service) returnScope(ctx context.Context, dir, rev string, results []Result, changed []string) []string {
	var named []string
	for _, r := range results {
		if r.Level == LevelError && r.Outcome == Fail && r.Defer == "" {
			named = append(named, r.Files...)
		}
	}
	if len(named) == 0 {
		named = changed
	}
	files := cleanScope(append(named, codeForTests(named, changed)...))
	if len(files) == 0 {
		return nil
	}
	if rev != "" {
		files = append(files, a.siblingTests(ctx, dir, rev, files)...)
	}
	return foldScope(cleanScope(files))
}

// codeForTests：点名的是测试文件时（测试门的日志多半只报测试文件的行号），要修的常常是被测的代码。
// 把这一轮改过的、和点名的测试同目录（测试放在 tests/ 这类目录时也看上一层）的非测试文件加进来；
// 点名的全是测试文件又一个也对不上时，加上这一轮改的全部文件，宁可宽一点也不卡住正常的修复。
func codeForTests(named, changed []string) []string {
	var out []string
	allTests := true
	for _, f := range named {
		if !isTestFile(f) {
			allTests = false
			continue
		}
		dirs := []string{path.Dir(f)}
		if b := path.Base(dirs[0]); slices.Contains([]string{"test", "tests", "__tests__", "spec", "specs", "Tests"}, b) {
			dirs = append(dirs, path.Dir(dirs[0]))
		}
		for _, c := range changed {
			if !isTestFile(c) && !slices.Contains(recordFiles, path.Base(c)) && slices.Contains(dirs, path.Dir(c)) { // 记录文件本来就在范围里，不算对上了被测代码
				out = append(out, c)
			}
		}
	}
	if allTests && len(named) > 0 && len(out) == 0 {
		return changed
	}
	return out
}

// cleanScope 去重、排序，去掉空的和 ready.json。
func cleanScope(files []string) []string {
	var out []string
	for _, f := range files {
		f = strings.TrimPrefix(strings.TrimSpace(f), "./")
		if f == "" || f == ReadyFile || slices.Contains(out, f) {
			continue
		}
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// siblingTests：files 同目录下已有的测试文件。只看这一层，子目录（比如 tests/）不展开。
func (a *Service) siblingTests(ctx context.Context, dir, rev string, files []string) []string {
	var dirs []string
	for _, f := range files {
		if d := path.Dir(f); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	var out []string
	for _, d := range dirs {
		args := []string{"ls-tree", rev}
		if d != "." {
			args = append(args, "--", d+"/")
		}
		list, err := a.git(ctx, dir, args...)
		if err != nil {
			a.Logf("门：查 %s 下的测试文件失败，范围里不加：%v", d, err)
			continue
		}
		for _, line := range strings.Split(list, "\n") {
			meta, name, ok := strings.Cut(line, "\t")
			if ok && strings.Contains(meta, " blob ") && isTestFile(name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// foldScope：多于 maxScope 个按目录折叠，还多就按顶层目录，再多就不限定（范围宽到没意义）。根目录的文件不折叠。
func foldScope(files []string) []string {
	if len(files) <= maxScope {
		return files
	}
	for _, depth := range []int{-1, 1} {
		var out []string
		for _, f := range files {
			d := path.Dir(f)
			if d == "." {
				d = f
			} else {
				if depth > 0 {
					d, _, _ = strings.Cut(d, "/")
				}
				d += "/"
			}
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
		if len(out) <= maxScope {
			slices.Sort(out)
			return out
		}
	}
	return nil
}

// recordFiles 是 agent-task 和项目规则让任务顺手写的记录文件（取舍、教训、变更说明、交接单、待办），不管放在哪一层目录都不算越界。
// ready.json 是交付单，也总在范围里。
var recordFiles = []string{"CHANGELOG.md", "DECISIONS.md", "PROGRESS.md", "LESSONS.md", "BACKLOG.md"}

// inScope：f 在范围里。「dir/」管这个目录下所有文件；文件管它自己和同目录的测试文件（新加的也算）。记录文件和 ready.json 总在范围里。
func inScope(f string, scope []string) bool {
	if f == ReadyFile || slices.Contains(recordFiles, path.Base(f)) {
		return true
	}
	for _, s := range scope {
		switch {
		case strings.HasSuffix(s, "/"):
			if strings.HasPrefix(f, s) {
				return true
			}
		case f == s:
			return true
		case isTestFile(f) && path.Dir(f) == path.Dir(s):
			return true
		}
	}
	return false
}

// scopeLine 是退回提示里的范围那一行。
func scopeLine(scope []string) string {
	return "范围：只改这些文件：" + strings.Join(scope, "、") + "（同目录的测试文件也算；以「/」结尾的是整个目录；CHANGELOG.md、DECISIONS.md、PROGRESS.md、LESSONS.md、BACKLOG.md 这类记录文件照常写）。要改范围外的文件就不要写 ready.json，按 agent-task 写卡住标记说明为什么。"
}

type scopeGate struct{ d Deps }

func (scopeGate) Name() string { return "scope" }

// AppliesTo：只在退回限定了范围之后的轮次跑。
func (scopeGate) AppliesTo(c Candidate) bool { return len(c.Scope) > 0 && c.ScopeFrom != "" }

func (g scopeGate) Run(ctx context.Context, c Candidate) Result {
	changed, how, err := g.changedSince(ctx, c)
	if err != nil {
		return Result{Outcome: Unknown, Summary: "查不了这一轮改了哪些文件：" + textfmt.OneLine(err.Error(), 200)}
	}
	var outside []string
	for _, f := range changed {
		if !inScope(f, c.Scope) {
			outside = append(outside, f)
		}
	}
	scope := "范围：" + strings.Join(c.Scope, "、")
	if len(outside) == 0 {
		return Result{Outcome: Pass, Summary: fmt.Sprintf("这一轮改的 %d 个文件都在退回时定的范围里", len(changed)), Evidence: []string{how, scope}}
	}
	ev := []string{how, scope}
	for _, f := range outside {
		ev = append(ev, "范围外："+f)
	}
	return Result{Outcome: Fail, Summary: "改了退回时定的范围外的文件：" + textfmt.OneLine(strings.Join(outside, "、"), 200) + "。要改范围外的就按 agent-task 写卡住标记说明为什么", Evidence: ev, Files: outside}
}

// changedSince：上一轮分支尖之后任务自己的提交改的文件。只看不在默认分支上的非合并提交：合 main 带来的提交在默认分支上，不算。
// 上一轮分支尖不在这次分支尖的历史里（分支被改写过）时退一步：两个分支尖之间改了、又和默认分支不一样的文件。
func (g scopeGate) changedSince(ctx context.Context, c Candidate) ([]string, string, error) {
	var out string
	var how string
	if _, err := g.d.Git(ctx, c.ProjectDir, "merge-base", "--is-ancestor", c.ScopeFrom, c.Head); err == nil {
		out, err = g.d.Git(ctx, c.ProjectDir, "log", "--no-merges", "--no-renames", "--format=", "--name-only", c.Head, "^"+c.ScopeFrom, "^"+c.Base)
		if err != nil {
			return nil, "", err
		}
		how = "比较 " + textfmt.TruncRunes(c.ScopeFrom, 12) + " 之后任务自己的提交（合 " + c.DefaultBranch + " 带来的不算）"
	} else {
		diff, err := g.d.Git(ctx, c.ProjectDir, "diff", "--name-only", "--no-renames", c.ScopeFrom, c.Head)
		if err != nil {
			return nil, "", err
		}
		for _, f := range strings.Split(diff, "\n") {
			if slices.Contains(c.Files, f) {
				out += f + "\n"
			}
		}
		how = "分支被改写过：比较 " + textfmt.TruncRunes(c.ScopeFrom, 12) + " 和这次分支尖之间改了、又和 " + c.DefaultBranch + " 不一样的文件"
	}
	return cleanScope(strings.Split(out, "\n")), how, nil
}

// logFileRe 认输出里的「路径:行号」和 Python 的「File "路径", line N」。
var logFileRe = regexp.MustCompile(`([^\s"'()\[\]<>:,]+\.[A-Za-z][A-Za-z0-9]{0,9})(?::\d+|", line \d+)`)

// maxLogScan：日志最多看这么多字节（从尾部），出错的地方一般在最后。
const maxLogScan = 1 << 20

// filesInLog 从门脚本的日志里认出仓库里的文件：完整路径直接认；只有文件名或包内路径（go test 的输出）的，按后缀找，
// 优先这次改过的文件，其次改过的文件同目录的，剩下的只有一个匹配才算。认不出的不硬凑。
func filesInLog(ctx context.Context, d Deps, c Candidate, logPath string) []string {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	if len(raw) > maxLogScan {
		raw = raw[len(raw)-maxLogScan:]
	}
	list, err := d.Git(ctx, c.Dir, "ls-files") // 门跑前 worktree 还原成了候选，索引就是候选的文件
	if err != nil {
		return nil
	}
	tracked := strings.Split(list, "\n")
	roots := []string{c.Dir + "/"}
	if real, err := filepath.EvalSymlinks(c.Dir); err == nil && real != c.Dir {
		roots = append(roots, real+"/")
	}
	dirs := map[string]bool{}
	for _, f := range c.Files {
		dirs[path.Dir(f)] = true
	}
	var out []string
	for _, m := range logFileRe.FindAllStringSubmatch(string(raw), -1) {
		tok := m[1]
		for _, r := range roots {
			tok = strings.TrimPrefix(tok, r)
		}
		tok = strings.TrimPrefix(tok, "./")
		if f := pickTracked(tok, tracked, c.Files, dirs); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
			if len(out) >= maxScope {
				break
			}
		}
	}
	return out
}

func pickTracked(tok string, tracked, changed []string, dirs map[string]bool) string {
	if tok == "" || strings.HasPrefix(tok, "/") {
		return ""
	}
	if slices.Contains(tracked, tok) {
		return tok
	}
	var hits []string
	for _, f := range tracked {
		if strings.HasSuffix(f, "/"+tok) {
			hits = append(hits, f)
		}
	}
	for _, prefer := range []func(string) bool{
		func(f string) bool { return slices.Contains(changed, f) },
		func(f string) bool { return dirs[path.Dir(f)] },
	} {
		var sel []string
		for _, f := range hits {
			if prefer(f) {
				sel = append(sel, f)
			}
		}
		if len(sel) == 1 {
			return sel[0]
		}
		if len(sel) > 1 {
			return ""
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}
