package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentq/internal/gitx"
	"agentq/internal/textfmt"
)

const (
	subtractWindow     = 7 * 24 * time.Hour
	subtractNetMin     = 50 // 净删这么多行才算明显
	subtractPerProject = 3  // 早报里每个项目最多列几条
	subtractMaxLines   = 6  // 这一节最多几行，多的写「另有」
	subtractGitWait    = 30 * time.Second
)

// NoSubtract 是 7 天里一条减法都没有时这一节唯一的一行。
const NoSubtract = "本周没有做减法"

// subtraction 是默认分支上一个做了减法的提交（引擎的合并提交按它和第一父的差算）。
type subtraction struct {
	Hash, Subject string
	Net           int      // 净删行数：删的减加的，锁文件不算
	Files         []string // 删掉的文件（改名不算）
	Deps          []string // 少了的依赖
}

// lockFiles 的行数随依赖升级大起大落，不算进净删。
var lockFiles = []string{"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "Podfile.lock", "Gemfile.lock", "poetry.lock", "uv.lock", "Package.resolved"}

// MorningSubtract 是早报「本周做的减法」一节：只在周一出，列过去 7 天各项目（跳过 exclude_projects）合进默认分支的提交里
// 净删行数明显的、删了文件的、少了依赖的，每行带提交号和提交标题；一条都没有就只有 NoSubtract。不是周一返回 nil。
// 只读 git 历史；「待办：」开头的是 BACKLOG 记账提交，不算。查不了的项目跳过并记日志。
func (a *Service) MorningSubtract(now time.Time) []string {
	if now.Weekday() != time.Monday {
		return nil
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		a.Logf("早报：扫项目不全，「本周做的减法」只算扫到的：%v", err)
	}
	var lines []string
	total, more := 0, map[string]bool{}
	for _, p := range projects {
		subs, err := a.projectSubtractions(p.Dir, now.Add(-subtractWindow))
		if err != nil {
			a.Logf("早报：查 %s 的减法失败，跳过：%v", p.Name, err)
			continue
		}
		total += len(subs)
		slices.SortStableFunc(subs, func(x, y subtraction) int { return y.Net - x.Net })
		for i, s := range subs {
			if i >= subtractPerProject || len(lines) >= subtractMaxLines {
				more[p.Name] = true
				continue
			}
			lines = append(lines, p.Name+"　"+s.Hash+" "+textfmt.TruncRunes(s.Subject, 30)+"（"+s.facts()+"）")
		}
	}
	if total == 0 {
		return []string{NoSubtract}
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("另有 %d 条（%d 个项目）", total-len(lines), len(more)))
	}
	return lines
}

// facts 是一条减法的事实：净删、删的文件、少的依赖。
func (s subtraction) facts() string {
	var out []string
	if s.Net > 0 {
		out = append(out, fmt.Sprintf("净删 %d 行", s.Net))
	}
	if n := len(s.Files); n > 0 {
		f := "删 " + strings.Join(s.Files[:min(n, 2)], "、")
		if n > 2 {
			f += fmt.Sprintf(" 等 %d 个文件", n)
		}
		out = append(out, f)
	}
	if n := len(s.Deps); n > 0 {
		d := "少依赖 " + strings.Join(s.Deps[:min(n, 3)], "、")
		if n > 3 {
			d += fmt.Sprintf(" 等 %d 个", n)
		}
		out = append(out, d)
	}
	return strings.Join(out, "；")
}

// projectSubtractions 查项目默认分支上 since 之后（按提交时间）沿第一父的提交里做了减法的。没有提交的仓库没有。
func (a *Service) projectSubtractions(dir string, since time.Time) ([]subtraction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), subtractGitWait)
	defer cancel()
	branch, err := gitx.DefaultBranch(ctx, a.Home, dir)
	if err != nil {
		return nil, err
	}
	if _, err := a.Gitx.RoutineGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}"); err != nil {
		return nil, nil // 还没有提交
	}
	out, err := a.Gitx.RoutineGit(ctx, dir, "log", "--first-parent", "--diff-merges=first-parent", "-M", "--numstat", "--summary",
		"--since="+since.Format("2006-01-02 15:04:05 -0700"), "--format=%x01%H%x00%h%x00%s", "refs/heads/"+branch, "--")
	if err != nil {
		return nil, fmt.Errorf("git log %s：%w", branch, err)
	}
	var subs []subtraction
	for _, c := range parseSubtractLog(out) {
		if strings.HasPrefix(c.Subject, "待办：") {
			continue
		}
		s := subtraction{Hash: c.Short, Subject: c.Subject, Net: c.Deleted - c.Added, Files: c.DeletedFiles}
		for _, m := range c.Manifests {
			s.Deps = append(s.Deps, a.removedDeps(ctx, dir, c.Full, m)...)
		}
		if s.Net >= subtractNetMin || len(s.Files) > 0 && s.Net >= 0 || len(s.Deps) > 0 {
			subs = append(subs, s)
		}
	}
	return subs, nil
}

// logCommit 是 git log --numstat --summary 里的一个提交。
type logCommit struct {
	Full, Short, Subject string
	Added, Deleted       int
	DeletedFiles         []string
	Manifests            []string // 有删行的依赖清单文件
}

// parseSubtractLog 解析 projectSubtractions 那条 git log 的输出。每个提交以 \x01 开头。
func parseSubtractLog(out string) []logCommit {
	var cs []logCommit
	for _, rec := range strings.Split(out, "\x01") {
		head, body, _ := strings.Cut(rec, "\n")
		f := strings.SplitN(head, "\x00", 3)
		if len(f) != 3 {
			continue
		}
		c := logCommit{Full: f[0], Short: f[1], Subject: f[2]}
		for _, l := range strings.Split(body, "\n") {
			if p, ok := strings.CutPrefix(l, " delete mode "); ok {
				if _, name, ok := strings.Cut(p, " "); ok {
					c.DeletedFiles = append(c.DeletedFiles, name)
				}
				continue
			}
			n := strings.SplitN(l, "\t", 3)
			if len(n) != 3 {
				continue
			}
			add, err1 := strconv.Atoi(n[0])
			del, err2 := strconv.Atoi(n[1])
			if err1 != nil || err2 != nil { // 二进制文件是「-」
				continue
			}
			base := path.Base(n[2])
			if slices.Contains(lockFiles, base) {
				continue
			}
			c.Added += add
			c.Deleted += del
			if del > 0 && depsParser(base) != nil && !strings.Contains(n[2], "=>") {
				c.Manifests = append(c.Manifests, n[2])
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// removedDeps：提交 c 比它的第一父少了 file 里的哪些依赖。读不了（根提交、新文件）当原来没有。
func (a *Service) removedDeps(ctx context.Context, dir, c, file string) []string {
	parse := depsParser(path.Base(file))
	before, err := a.Gitx.RoutineGit(ctx, dir, "show", c+"^1:"+file)
	if err != nil {
		return nil
	}
	after, _ := a.Gitx.RoutineGit(ctx, dir, "show", c+":"+file) // 删掉的文件读不到，依赖全算少了
	now := parse(after)
	var out []string
	for _, d := range parse(before) {
		if !slices.Contains(now, d) {
			out = append(out, d)
		}
	}
	return out
}

// depsParser：认得的依赖清单文件返回解析函数，否则 nil。只认直接依赖。
func depsParser(base string) func(string) []string {
	switch {
	case base == "go.mod":
		return goModDeps
	case base == "package.json":
		return packageJSONDeps
	case base == "Cargo.toml":
		return cargoDeps
	case strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt"):
		return requirementsDeps
	}
	return nil
}

func goModDeps(text string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "require (":
			in = true
			continue
		case in && l == ")":
			in = false
			continue
		}
		if s, ok := strings.CutPrefix(l, "require "); ok && !strings.HasPrefix(s, "(") {
			l, in = s, false
		} else if !in {
			continue
		}
		if f := strings.Fields(l); len(f) >= 2 && !strings.Contains(l, "// indirect") {
			out = append(out, f[0])
		}
	}
	return out
}

func packageJSONDeps(text string) []string {
	var pkg map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &pkg) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]any
		if json.Unmarshal(pkg[k], &deps) == nil {
			for name := range deps {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

var (
	cargoSection = regexp.MustCompile(`^\[(?:.+\.)?(?:dependencies|dev-dependencies|build-dependencies)(?:\.([A-Za-z0-9_-]+))?\]$`)
	cargoKey     = regexp.MustCompile(`^([A-Za-z0-9_-]+)\s*=`)
	reqName      = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)`)
)

func cargoDeps(text string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			m := cargoSection.FindStringSubmatch(l)
			in = m != nil && m[1] == ""
			if m != nil && m[1] != "" { // [dependencies.serde] 这种一节一个依赖
				out = append(out, m[1])
			}
			continue
		}
		if m := cargoKey.FindStringSubmatch(l); in && m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func requirementsDeps(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := reqName.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, strings.ToLower(m[1]))
		}
	}
	return out
}
