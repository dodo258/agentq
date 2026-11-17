package evolve

import (
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// 按文件归领域：返工意见、结束的任务、门点名的文件都靠这里归到前端、后端、设计、文档。
//
// 一次改动归到它主体改的领域。合并提交除了主体，还会顺手改记录文件（项目规则让每个任务都写 DECISIONS.md、CHANGELOG.md）
// 和说明文档（README、docs/ 下的参考）。以前每个文件一样分量，认不出主体的语言时（Flutter 的 .dart）只剩顺手改的
// DECISIONS.md 认得出，整条就记成「文档」；认得出的也多记一个「文档」。不相干的返工就这样凑成了「文档返工反复出现」。
// 所以：记录文件不归领域；文档只在这次没改别的代码、设计文件时才算（和 GitHub Linguist 把文档排除在语言统计外是一个道理）；
// 认不出领域的代码文件也算「改了代码」，宁可归不了、去看文字，也不让顺手改的文档顶上。

// 领域：前端、后端、设计、文档由路径归，路径归不了的不算。和英文名的对照见 capability.go 的 domainTable。
const (
	DomainFrontend = "前端"
	DomainBackend  = "后端"
	DomainDesign   = "设计"
	DomainDocs     = "文档"
	domainUnknown  = "未分类"
)

var (
	frontendDirs = []string{"web", "frontend", "ui", "components", "pages", "views", "public", "styles", "site"}
	backendDirs  = []string{"server", "backend", "api", "internal", "cmd", "pkg", "db", "migrations"}
	// 移动端界面：Swift、Kotlin、Dart 文件名或目录带这些词的是界面，别的认不出领域。
	mobileUIDirs  = []string{"views", "ui", "screens", "pages", "widgets"}
	mobileUIWords = []string{"view", "screen", "page", "dialog", "widget"}
	// 认不出领域、但确实是代码的文件：改了它们，顺手改的文档就不算主体。
	otherCodeExts = []string{".swift", ".kt", ".dart", ".c", ".h", ".cc", ".cpp", ".m", ".mm", ".cs", ".sh", ".lua", ".scala", ".ex", ".exs"}
	domainWords   = map[string]*regexp.Regexp{
		DomainDesign:   regexp.MustCompile(`设计|配色|视觉|排版|字体|图标|好看|难看|丑`),
		DomainFrontend: regexp.MustCompile(`界面|页面|前端|按钮|布局|样式|动画|交互|(?i:\bui\b)`),
		DomainBackend:  regexp.MustCompile(`后端|接口|数据库|服务端|(?i:\bapi\b)`),
	}
)

// recordFiles：agent-task 和项目规则让每个任务顺手写的记录文件（取舍、教训、变更说明、交接单、待办），不管在哪一层目录。
// 和退回范围认的记录文件是同一批（gate 的 scope.go）。
var recordFiles = []string{"CHANGELOG.md", "DECISIONS.md", "PROGRESS.md", "LESSONS.md", "BACKLOG.md"}

// filesDomains：一次改动（文件和范围写的目录）归到的领域，排好序。文档只在没有别的代码、设计改动时算。
func filesDomains(paths []string) []string {
	var out []string
	docs, other := false, false
	for _, p := range paths {
		switch d := pathDomain(p); {
		case d == DomainDocs:
			docs = true
		case d != "":
			other = true
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		case slices.Contains(otherCodeExts, strings.ToLower(path.Ext(p))):
			other = true
		}
	}
	if docs && !other {
		return []string{DomainDocs}
	}
	sort.Strings(out)
	return out
}

// pathDomain：一个文件或目录单看归哪个领域，归不了是空。记录文件不归领域。
func pathDomain(p string) string {
	p = strings.TrimSuffix(filepath.ToSlash(p), "/")
	if p == "" || slices.Contains(recordFiles, path.Base(p)) {
		return ""
	}
	p = strings.ToLower(p)
	parts := strings.Split(p, "/")
	hasDir := func(names []string) bool {
		return slices.ContainsFunc(parts, func(seg string) bool { return slices.Contains(names, seg) })
	}
	base := parts[len(parts)-1]
	switch path.Ext(base) {
	case ".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".fig", ".sketch", ".icns":
		return DomainDesign
	case ".tsx", ".jsx", ".vue", ".svelte", ".astro", ".css", ".scss", ".sass", ".less", ".html":
		return DomainFrontend
	case ".go", ".rs", ".py", ".java", ".rb", ".php", ".sql", ".proto":
		return DomainBackend
	case ".swift", ".kt", ".dart":
		if hasDir(mobileUIDirs) || slices.ContainsFunc(mobileUIWords, func(w string) bool { return strings.Contains(base, w) }) {
			return DomainFrontend
		}
		return ""
	case ".ts", ".js", ".mjs":
		if hasDir(backendDirs) {
			return DomainBackend
		}
		return DomainFrontend
	case ".md":
		if hasDir([]string{"design"}) || strings.HasPrefix(base, "design") {
			return DomainDesign
		}
		return DomainDocs
	}
	switch { // 范围写的目录
	case hasDir([]string{"design"}):
		return DomainDesign
	case hasDir(frontendDirs):
		return DomainFrontend
	case hasDir(backendDirs):
		return DomainBackend
	case hasDir([]string{"docs"}):
		return DomainDocs
	}
	return ""
}

// wordDomains：路径归不了时看文字。
func wordDomains(text string) []string {
	var out []string
	for d, re := range domainWords {
		if re.MatchString(text) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}
