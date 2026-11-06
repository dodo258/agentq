package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// archAllowed 是模块表（AGENTS.md 给人看）：包 → 它的代码（产品和测试）可以 import 的 internal 包。
// 没出现在这里的 internal 包一律不许建：要加模块、加依赖先改这张表。
var archAllowed = map[string]string{
	"textfmt":          "",
	"fsx":              "",
	"proc":             "",
	"config":           "textfmt fsx backend",
	"env":              "fsx proc config backend",
	"backend":          "textfmt proc",
	"backend/claude":   "textfmt fsx proc config backend",
	"backend/codex":    "fsx proc backend",
	"backend/opencode": "fsx backend",
	"gitx":             "textfmt proc env",
	"state":            "textfmt fsx config backend",
	"backlog":          "textfmt fsx env gitx",
	"telegram":         "textfmt env",
	"asc":              "textfmt env backlog",
	"quality":          "textfmt env gitx backlog",
	"ledger":           "textfmt fsx env backend state quality",
	"service":          "textfmt fsx proc config env",
	"envlayer":         "fsx backend",
	"notify":           "textfmt fsx proc config env backend state backlog telegram",
	"gate":             "textfmt fsx proc config env backend gitx state backlog ledger notify",
	"runner":           "textfmt fsx proc config env backend gitx state backlog ledger notify gate",
	"orphan":           "textfmt env state backlog ledger notify runner",
	"inbox":            "textfmt fsx config env backend state backlog ledger notify runner",
	"discuss":          "textfmt fsx proc config env backend gitx state backlog telegram ledger notify runner",
	"newproj":          "textfmt env backend gitx state backlog ledger notify runner discuss",
	"suggest":          "textfmt fsx proc config env backend gitx state backlog telegram notify runner",
	"release":          "textfmt fsx config env backend state backlog telegram asc ledger notify runner",
	"sense":            "textfmt fsx proc config env backend gitx state backlog asc ledger notify runner suggest release",
	"routine":          "textfmt fsx proc config env backend gitx state backlog ledger service notify runner inbox sense",
	"patrol":           "textfmt fsx config env backend gitx state backlog telegram ledger service notify runner",
	"chat":             "textfmt fsx proc config env backend gitx state backlog telegram ledger service notify gate runner orphan inbox discuss newproj suggest release",
	"stats":            "textfmt config env backend state ledger",
	"evolve":           "textfmt fsx proc config env backend state backlog",
	"sched":            "textfmt fsx config env backend state backlog telegram ledger notify gate runner orphan inbox discuss newproj suggest release routine patrol chat",
	"setup":            "fsx proc config backend state service envlayer",
	"onboard":          "textfmt fsx config env backend gitx state backlog telegram service envlayer runner sense sched setup",
	"app":              "textfmt fsx proc config env backend backend/claude backend/codex backend/opencode gitx state backlog telegram asc quality ledger service notify gate runner orphan inbox discuss newproj suggest release sense routine patrol chat stats sched setup onboard evolve",
	// main 是根目录的产品代码：入口和编译进二进制的资产。
	"main": "textfmt config env runner chat setup onboard app",
	// testkit 只给测试 import。它 import backend/claude 是为了注册 Claude 后端，给测试看 argv。
	"testkit": "fsx gitx backend backend/claude",
	// testkit/stack 只给 runner 上层模块的测试 import，建真实的 runner 和它下层的模块。
	"testkit/stack": "env backend/claude gitx backlog telegram asc quality ledger service notify gate runner",
}

// walkInternal 遍历 internal 下的每个 Go 文件。
func walkInternal(t *testing.T, fn func(pkg, path string, test bool)) {
	t.Helper()
	err := filepath.WalkDir("internal", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		pkg := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(p)), "internal/")
		fn(pkg, p, strings.HasSuffix(p, "_test.go"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rootProduct 是根目录的产品文件。
func rootProduct(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range all {
		if !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
	}
	return out
}

// 依赖方向：只许 import 模块表写明的包，第三方依赖一个都不要；模块表里写了的依赖真要有人用，表和代码对得上。
// 根目录只查产品代码，tests/ 的跨模块行为用例由 TestArchTestsDir 查。
func TestArchImports(t *testing.T) {
	used := map[string]map[string]bool{}
	check := func(pkg, path string, test bool) {
		allowed, known := archAllowed[pkg]
		if !known {
			t.Errorf("%s：包 %s 不在模块表里", path, pkg)
			return
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Error(err)
			return
		}
		if used[pkg] == nil {
			used[pkg] = map[string]bool{}
		}
		set := strings.Fields(allowed)
		for _, s := range f.Imports {
			imp, _ := strconv.Unquote(s.Path.Value)
			rest, internal := strings.CutPrefix(imp, "agentq/internal/")
			if !internal {
				if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") || imp == "agentq" {
					t.Errorf("%s 不该 import %s：模块只依赖标准库和 internal", path, imp)
				}
				continue
			}
			if rest == pkg {
				continue
			}
			if test && (rest == "testkit" || rest == "testkit/stack") { // 模块的测试要看 Claude 的 argv、拒绝规则，都经 testkit，不直接 import 实现包
				continue
			}
			used[pkg][rest] = true
			if !slices.Contains(set, rest) {
				t.Errorf("%s 不该依赖 %s（模块表：%s → %q）", path, rest, pkg, allowed)
			}
		}
	}
	walkInternal(t, check)
	for _, p := range rootProduct(t) {
		check("main", p, false)
	}
	for pkg, allowed := range archAllowed {
		if used[pkg] == nil {
			t.Errorf("模块表里的 %s 没有代码", pkg)
			continue
		}
		for _, d := range strings.Fields(allowed) {
			if !used[pkg][d] {
				t.Errorf("模块表写了 %s → %s，代码里没有：表要和代码一样", pkg, d)
			}
		}
	}
}

// 后端的实现包只有组装层能 import（注册）。产品代码都走 backend.Backend。
func TestArchBackendImpls(t *testing.T) {
	check := func(pkg, path string) {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Error(err)
			return
		}
		for _, s := range f.Imports {
			imp, _ := strconv.Unquote(s.Path.Value)
			if strings.HasPrefix(imp, "agentq/internal/backend/") && pkg != "app" && !strings.HasPrefix(pkg, "testkit") {
				t.Errorf("产品代码 %s import 了 %s：只有 internal/app 能 import 后端实现", path, imp)
			}
		}
	}
	walkInternal(t, func(pkg, path string, test bool) {
		if !test && !strings.HasPrefix(pkg, "backend/") {
			check(pkg, path)
		}
	})
	for _, p := range rootProduct(t) {
		check("main", p)
	}
}

// 后端的 CLI 专有的东西（参数、结果格式、会话文件位置）只能出现在各自的 internal/backend/<名字>。查 Go 字符串字面量，
// 再查源码里的路径写法（注释也不能写死，免得别的模块照着抄）。提示词和帮助里提到 claude -p 不算，那是 B4 的事。
var (
	archClaudeFlags = regexp.MustCompile(`(^|[^-\w])--(print|output-format|session-id|model|effort|permission-mode|disallowed-tools|allowed-tools|max-budget-usd|max-turns|json-schema|tools|strict-mcp-config|settings|setting-sources|no-session-persistence|add-dir|resume)\b`)
	archClaudeExact = map[string]bool{"-p": true, "-r": true, "stream-json": true, "task-notification": true, "api_error_status": true, "is_error": true, "structured_output": true, "total_cost_usd": true, "modelUsage": true}
	archClaudeSrc   = regexp.MustCompile(`"\.claude", "projects"|\.claude/projects|error_max`)
)

// Codex CLI 专有的东西同样只能出现在 internal/backend/codex。提示词和文字里说到 codex 不算，查的是参数、事件名、会话文件位置。
var (
	archCodexFlags = regexp.MustCompile(`(^|[^-\w])--(output-schema|output-last-message|ignore-user-config|ignore-rules|skip-git-repo-check|dangerously-bypass-approvals-and-sandbox|approve-for-me)\b`)
	archCodexExact = map[string]bool{"thread.started": true, "turn.started": true, "turn.completed": true, "turn.failed": true, "item.completed": true, "agent_message": true, "command_execution": true,
		"danger-full-access": true, "workspace-write": true, "read-only": true, "CODEX_HOME": true, "CODEX_API_KEY": true}
	archCodexSrc = regexp.MustCompile(`\.codex/sessions|rollout-\*|approval_policy|sandbox_mode|model_reasoning_effort`)
)

// opencode 同样。--format、--session、--model 别处也用（git log --format、Claude），查的是它专有的。
var (
	archOpencodeFlags = regexp.MustCompile(`(^|[^-\w])--(auto|variant|session=|thinking|attach|dangerously-skip-permissions)\b`)
	archOpencodeExact = map[string]bool{"step_start": true, "step_finish": true, "tool_use": true, "ProviderAuthError": true, "OPENCODE_CONFIG": true, "OPENCODE_CONFIG_DIR": true, "OPENCODE_DB": true}
	archOpencodeSrc   = regexp.MustCompile(`\.config", "opencode"|\.config/opencode|\.local/share/opencode|OPENCODE_`)
)

func TestArchClaudeFlagsOnlyInBackend(t *testing.T) {
	check := func(pkg, path string) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		if m := archClaudeSrc.Find(src); m != nil {
			t.Errorf("%s 写了 Claude 专有的 %s：只有 internal/backend/claude 能写", path, m)
		}
		if m := archCodexSrc.Find(src); m != nil {
			t.Errorf("%s 写了 Codex 专有的 %s：只有 internal/backend/codex 能写", path, m)
		}
		if m := archOpencodeSrc.Find(src); m != nil {
			t.Errorf("%s 写了 opencode 专有的 %s：只有 internal/backend/opencode 能写", path, m)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Error(err)
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if _, ok := n.(*ast.ImportSpec); ok {
				return false
			}
			if fl, ok := n.(*ast.Field); ok && fl.Tag != nil { // JSON 标签不是 CLI 参数
				fl.Tag = nil
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if s == "-p" && pkg == "proc" { // ps -p，不是 Claude 的
				return true
			}
			if m := archClaudeFlags.FindString(s); m != "" || archClaudeExact[s] {
				t.Errorf("%s:%d 的字面量有 Claude CLI 专有的 %q：只有 internal/backend/claude 能写", path, fset.Position(lit.Pos()).Line, strings.TrimSpace(m+map[bool]string{true: s}[archClaudeExact[s]]))
			}
			if m := archOpencodeFlags.FindString(s); m != "" || archOpencodeExact[s] {
				t.Errorf("%s:%d 的字面量有 opencode CLI 专有的 %q：只有 internal/backend/opencode 能写", path, fset.Position(lit.Pos()).Line, strings.TrimSpace(m+map[bool]string{true: s}[archOpencodeExact[s]]))
			}
			if m := archCodexFlags.FindString(s); m != "" || archCodexExact[s] {
				t.Errorf("%s:%d 的字面量有 Codex CLI 专有的 %q：只有 internal/backend/codex 能写", path, fset.Position(lit.Pos()).Line, strings.TrimSpace(m+map[bool]string{true: s}[archCodexExact[s]]))
			}
			return true
		})
	}
	walkInternal(t, func(pkg, path string, test bool) {
		if !test && !strings.HasPrefix(pkg, "backend/") && !strings.HasPrefix(pkg, "testkit") {
			check(pkg, path)
		}
	})
	for _, p := range rootProduct(t) {
		check("main", p)
	}
	// Claude 的东西也不能跑到 Codex 的包里，反过来也一样：各管各的
	for _, c := range []struct {
		dir string
		re  *regexp.Regexp
	}{{"internal/backend/codex", archClaudeSrc}, {"internal/backend/claude", archCodexSrc}, {"internal/backend/opencode", archClaudeSrc}, {"internal/backend/opencode", archCodexSrc},
		{"internal/backend/claude", archOpencodeSrc}, {"internal/backend/codex", archOpencodeSrc}} {
		files, err := filepath.Glob(filepath.Join(c.dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if m := c.re.Find(src); m != nil {
				t.Errorf("%s 写了别家后端专有的 %s", f, m)
			}
		}
	}
}

// 根目录的产品代码只有入口和编译进二进制的资产，别的都进模块。
func TestArchRootOnlyMain(t *testing.T) {
	want := []string{"assets.go", "main.go"}
	if got := rootProduct(t); !slices.Equal(got, want) {
		t.Errorf("根目录的产品文件应该只有 %v，实际 %v", want, got)
	}
}

// 行为用例都在 tests/：根目录只留查编译进二进制的那一个，tests/ 只放用例（不许长产品代码）、只依赖标准库和 internal，别的包也不能 import 它。
func TestArchTestsDir(t *testing.T) {
	all, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"assets_test.go"}; !slices.Equal(all, want) {
		t.Errorf("根目录的用例应该只有 %v，实际 %v：其余放 tests/", want, all)
	}
	files, err := filepath.Glob(filepath.Join("tests", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("tests/ 没有用例")
	}
	for _, p := range files {
		if !strings.HasSuffix(p, "_test.go") {
			t.Errorf("%s 不是用例：tests/ 不放产品代码", p)
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			t.Error(err)
			continue
		}
		if f.Name.Name != "tests" {
			t.Errorf("%s 的包名是 %s，应该是 tests", p, f.Name.Name)
		}
		for _, s := range f.Imports {
			imp, _ := strconv.Unquote(s.Path.Value)
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") || imp == "agentq" {
				t.Errorf("%s 不该 import %s：用例只依赖标准库和 internal", p, imp)
			}
		}
	}
	walkInternal(t, func(pkg, path string, test bool) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(src), `"agentq/tests"`) {
			t.Errorf("%s import 了 agentq/tests", path)
		}
	})
}

// testkit 是测试夹具，产品代码（根目录和 internal）都不能用。
func TestArchTestkitOnlyInTests(t *testing.T) {
	check := func(path string) {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Error(err)
			return
		}
		for _, s := range f.Imports {
			if imp, _ := strconv.Unquote(s.Path.Value); imp == "agentq/internal/testkit" || imp == "agentq/internal/testkit/stack" {
				t.Errorf("产品代码 %s import 了 testkit", path)
			}
		}
	}
	walkInternal(t, func(pkg, path string, test bool) {
		if !test && !strings.HasPrefix(pkg, "testkit") {
			check(path)
		}
	})
	for _, p := range rootProduct(t) {
		check(p)
	}
}

// app.Wire 最后四句的顺序是固定的：方法值在接线时就绑定接收者，排在建实例之前绑到的是 nil，调用才崩，所以 Hooks 最后。
func TestArchWireOrder(t *testing.T) {
	src, err := os.ReadFile("internal/app/wire.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wire.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "Wire" {
			body = fn.Body.List
		}
	}
	want := []string{"a.Sense.TriageSkill", "a.Routine.RoutineText", "a.Notify.MorningIssues", "a.Runner.Hooks"}
	if len(body) < len(want)+1 {
		t.Fatalf("Wire 只有 %d 句", len(body))
	}
	last := body[len(body)-len(want)-1 : len(body)-1]
	for i, s := range last {
		as, ok := s.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			t.Errorf("倒数第 %d 句不是赋值", len(want)+1-i)
			continue
		}
		lhs := string(src[fset.Position(as.Lhs[0].Pos()).Offset:fset.Position(as.Lhs[0].End()).Offset])
		if lhs != want[i] {
			t.Errorf("Wire 最后四句应该是 %v，第 %d 句是 %s", want, i+1, lhs)
		}
	}
}

// 模块表在 AGENTS.md 里给人看：每个模块都要写到。
func TestArchDocumented(t *testing.T) {
	b, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	for pkg := range archAllowed {
		if pkg != "main" && !strings.Contains(string(b), "`"+pkg+"`") {
			t.Errorf("AGENTS.md 的模块表没写 %s", pkg)
		}
	}
}

// 单例：notifyHeld、a.Cfg 重载、boot 缓存这些进程内状态只能有一份，所以模块实例只由组装层建。
// Env 的字面量只在 env 里；建 Env、建临时服务实例的入口各有一个固定的调用方。
var (
	archServiceLit = regexp.MustCompile(`\b[a-z]+\.Service\{`)
	archEnvLit     = regexp.MustCompile(`\benv\.Env\{`)
	archEnvEntry   = map[*regexp.Regexp]string{
		regexp.MustCompile(`\benv\.New\(`):          "app",     // 这个进程的 Env
		regexp.MustCompile(`\benv\.ForPaths\(`):     "service", // 只算路径和标签
		regexp.MustCompile(`\benv\.ForLog\(`):       "main",    // 配置读不出时写一行日志
		regexp.MustCompile(`\bservice\.ForPaths\(`): "setup",   // 安装引擎看服务
	}
)

func TestArchSingletons(t *testing.T) {
	check := func(pkg, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		if m := archServiceLit.Find(b); m != nil && pkg != "app" {
			t.Errorf("%s 自己构造了模块实例 %s：只有 internal/app 能建", path, m)
		}
		if m := archEnvLit.Find(b); m != nil {
			t.Errorf("%s 自己构造了 %s：Env 每个进程只有一份", path, m)
		}
		for re, owner := range archEnvEntry {
			if m := re.Find(b); m != nil && pkg != owner {
				t.Errorf("%s 用了 %s：只有 %s 能用", path, m, owner)
			}
		}
	}
	walkInternal(t, func(pkg, path string, test bool) {
		if !test && !strings.HasPrefix(pkg, "testkit") {
			check(pkg, path)
		}
	})
	root, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range root {
		if !strings.HasSuffix(p, "_test.go") {
			check("main", p)
		}
	}
}

// 模块表自己不能有环，否则按表迁移走不通。
func TestArchTableIsAcyclic(t *testing.T) {
	var names []string
	for n := range archAllowed {
		names = append(names, n)
	}
	sort.Strings(names)
	state := map[string]int{}
	var visit func(n string, path []string)
	visit = func(n string, path []string) {
		switch state[n] {
		case 1:
			t.Errorf("模块表有环：%s", strings.Join(append(path, n), " → "))
			return
		case 2:
			return
		}
		state[n] = 1
		for _, d := range strings.Fields(archAllowed[n]) {
			if _, ok := archAllowed[d]; !ok {
				t.Errorf("模块表：%s 依赖的 %s 没有定义", n, d)
				continue
			}
			visit(d, append(path, n))
		}
		state[n] = 2
	}
	for _, n := range names {
		visit(n, nil)
	}
}
