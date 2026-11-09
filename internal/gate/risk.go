package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"agentq/internal/backlog"
	"agentq/internal/textfmt"
)

// Risk 是改错了多难撤回。放行看这个，不看模型有多大把握。
type Risk string

const (
	// RiskLocal：可撤回且局部，照现有门。
	RiskLocal Risk = "local"
	// RiskBroad：影响面广。确定性门（patch、test、smoke）warn 当 error；测试门没跑就判不了，等用户定。
	RiskBroad Risk = "broad"
	// RiskIrreversible：难撤回。门照跑，无论结论都不自动合并，等用户 agentq gate pass。没有配置能让它自动合。
	RiskIrreversible Risk = "irreversible"
)

// RiskFile 是项目加规则的文件，读基点（默认分支）上的那份，任务分支上的改动不算。
const RiskFile = ".agentq/risk.json"

// RiskRules 是 .agentq/risk.json：路径 glob，追加到默认规则上，不能去掉默认规则。
// glob 不带「/」的只比文件名；带「/」的按整条路径比，「**」匹配任意层目录。
type RiskRules struct {
	Irreversible []string `json:"irreversible"`
	Broad        []string `json:"broad"`
}

// RiskVerdict 是这次的档位和依据，写进 result.json 和门报告。
type RiskVerdict struct {
	Level   Risk     `json:"level"`
	Reasons []string `json:"reasons,omitempty"`
	// Light、Flow：走不走轻量流程（见 light.go）和理由。
	Light bool   `json:"light,omitempty"`
	Flow  string `json:"flow,omitempty"`
}

var (
	defaultIrreversible = []string{"**/migrations/**", "**/migrate/**", "*.sql", "**/deploy/**"}
	// depManifests：依赖清单，只比文件名。锁文件跟着清单变，不单算。
	depManifests = []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "requirements.txt", "Package.swift", "Podfile", "Gemfile", "pom.xml", "build.gradle", "build.gradle.kts"}
	// schemaFiles：接口和数据结构定义。
	schemaFiles = []string{"*.proto", "openapi*.yaml", "openapi*.yml", "*.graphql", "schema*.json"}
	// dangerSQL：新增行里删数据的语句，不分大小写。TRUNCATE 要后面跟表名，免得 f.Truncate(0) 这种代码误判。
	dangerSQL = regexp.MustCompile(`(?i)\b(drop\s+table|drop\s+column|delete\s+from|truncate\s+(table\s+)?[\w"` + "`" + `\[])`)
	// deterministicGates：影响面广时 warn 升 error 的门。审查和界面门调模型，不算确定性的。
	deterministicGates = []string{"patch", "test", "smoke"}
)

const (
	broadFiles   = 15 // 改动文件超过这么多算影响面广
	broadTopDirs = 4  // 跨这么多个以上顶层目录算影响面广（根目录的文件不算一个目录）
	reasonFiles  = 5  // 依据里每条最多列几个文件
)

// Classify 只看候选的文件列表、删掉的文件、每个文件新增的行和条目原文，不调模型。返回档位和依据。
// added 给一个路径返回它新增的行（可以是空）。
func Classify(files, deleted []string, added func(path string) string, item string, r RiskRules) (Risk, []string) {
	var hard []string
	for _, pat := range append(slices.Clone(defaultIrreversible), r.Irreversible...) {
		if hit := matching(files, pat); len(hit) > 0 {
			hard = append(hard, "路径命中 "+pat+"："+listFiles(hit))
		}
	}
	if added != nil {
		var sql []string
		for _, f := range files {
			if notExecuted(f) {
				continue
			}
			if m := dangerSQL.FindString(added(f)); m != "" {
				sql = append(sql, f+"（"+sqlKeyword(m)+"）")
			}
		}
		if len(sql) > 0 {
			hard = append(hard, "新增行有删数据的语句："+listFiles(sql))
		}
	}
	if backlog.IrreversibleItem(item) {
		hard = append(hard, "条目标了"+backlog.IrreversibleMark)
	}
	if len(hard) > 0 {
		return RiskIrreversible, hard
	}
	var broad []string
	for _, pat := range r.Broad {
		if hit := matching(files, pat); len(hit) > 0 {
			broad = append(broad, "路径命中 "+pat+"："+listFiles(hit))
		}
	}
	var deps, schema []string
	for _, f := range files {
		if slices.Contains(depManifests, path.Base(f)) {
			deps = append(deps, f)
		}
		if slices.ContainsFunc(schemaFiles, func(p string) bool { return globMatch(p, f) }) {
			schema = append(schema, f)
		}
	}
	if len(deps) > 0 {
		broad = append(broad, "改了依赖清单："+listFiles(deps))
	}
	if len(schema) > 0 {
		broad = append(broad, "改了接口或数据结构定义："+listFiles(schema))
	}
	if len(deleted) > 0 {
		broad = append(broad, "删了文件："+listFiles(deleted))
	}
	if len(files) > broadFiles {
		broad = append(broad, fmt.Sprintf("改了 %d 个文件（超过 %d 个）", len(files), broadFiles))
	}
	if tops := topDirs(files); len(tops) >= broadTopDirs {
		broad = append(broad, fmt.Sprintf("跨 %d 个顶层目录：%s", len(tops), strings.Join(tops, "、")))
	}
	if len(broad) > 0 {
		return RiskBroad, broad
	}
	return RiskLocal, nil
}

// notExecuted：文档和测试文件，里面写到删数据的语句不会在用户的数据上执行，不扫新增行。路径规则照样算。
func notExecuted(f string) bool {
	base := path.Base(f)
	if strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".txt") || strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	dirs := strings.Split(path.Dir(f), "/")
	return slices.ContainsFunc(dirs, func(d string) bool { return d == "docs" || d == "test" || d == "tests" || d == "testdata" })
}

// sqlKeyword：命中的语句写成大写关键字，TRUNCATE 后面带出来的表名首字不要。
func sqlKeyword(m string) string {
	f := strings.Fields(strings.ToUpper(m))
	if f[0] == "TRUNCATE" {
		return "TRUNCATE"
	}
	return strings.Join(f, " ")
}

func matching(files []string, pat string) []string {
	var hit []string
	for _, f := range files {
		if globMatch(pat, f) {
			hit = append(hit, f)
		}
	}
	return hit
}

func listFiles(files []string) string {
	if len(files) <= reasonFiles {
		return strings.Join(files, "、")
	}
	return strings.Join(files[:reasonFiles], "、") + fmt.Sprintf(" 等 %d 个", len(files))
}

func topDirs(files []string) []string {
	var tops []string
	for _, f := range files {
		if top, _, ok := strings.Cut(f, "/"); ok && !slices.Contains(tops, top) {
			tops = append(tops, top)
		}
	}
	slices.Sort(tops)
	return tops
}

// globMatch：不带「/」的模式只比文件名；带「/」的按段比，「**」匹配任意层（结尾的「**」至少一层）。坏模式不匹配，ParseRiskRules 已经拦下。
func globMatch(pat, name string) bool {
	if !strings.Contains(pat, "/") {
		ok, _ := path.Match(pat, path.Base(name))
		return ok
	}
	return matchSegs(strings.Split(pat, "/"), strings.Split(name, "/"))
}

func matchSegs(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return len(n) > 0
			}
			for i := 0; i <= len(n); i++ {
				if matchSegs(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], n[0]); !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}

// ParseRiskRules 解析 .agentq/risk.json。写坏了报错，调用方这次按难撤回。
func ParseRiskRules(raw []byte) (RiskRules, error) {
	var r RiskRules
	if err := json.Unmarshal(raw, &r); err != nil {
		return RiskRules{}, fmt.Errorf("不是合法的 JSON：%w", err)
	}
	for _, pat := range append(slices.Clone(r.Irreversible), r.Broad...) {
		if strings.TrimSpace(pat) == "" {
			return RiskRules{}, fmt.Errorf("有空的规则")
		}
		for _, seg := range strings.Split(pat, "/") {
			if _, err := path.Match(seg, ""); err != nil {
				return RiskRules{}, fmt.Errorf("规则 %q 写错了：%w", pat, err)
			}
		}
	}
	return r, nil
}

// riskRulesAt 读基点上的 .agentq/risk.json，和 levelsAt 读 gates.json 一样：任务分支上的改动不算。
// 没有这个文件是正常的，用默认规则；有但读不了、写坏了返回错误：项目列的难撤回路径不能悄悄丢掉。
func (a *Service) riskRulesAt(ctx context.Context, dir, base string) (RiskRules, error) {
	if _, err := a.git(ctx, dir, "cat-file", "-e", base+":"+RiskFile); err != nil {
		return RiskRules{}, nil
	}
	raw, err := a.git(ctx, dir, "show", base+":"+RiskFile)
	if err != nil {
		return RiskRules{}, fmt.Errorf("读不了：%w", err)
	}
	return ParseRiskRules([]byte(raw))
}

// classify 给候选定档位。项目规则写坏了、改动读不出都按难撤回：宁可多等一次用户。
func (a *Service) classify(ctx context.Context, c Candidate) *RiskVerdict {
	rules, err := a.riskRulesAt(ctx, c.ProjectDir, c.Base)
	if err != nil {
		return &RiskVerdict{Level: RiskIrreversible, Reasons: []string{RiskFile + " 有问题：" + textfmt.OneLine(err.Error(), 200) + "，修好之前一律等你拍板"}}
	}
	diff, err := a.git(ctx, c.Dir, "diff", "--no-renames", "--no-color", "--no-ext-diff", "--unified=0", c.Base, c.Merge)
	if err != nil {
		return &RiskVerdict{Level: RiskIrreversible, Reasons: []string{"改动内容读不出，按难撤回：" + textfmt.OneLine(err.Error(), 200)}}
	}
	names, err := a.git(ctx, c.Dir, "diff", "--name-only", "--no-renames", "--diff-filter=D", c.Base, c.Merge)
	if err != nil {
		return &RiskVerdict{Level: RiskIrreversible, Reasons: []string{"删掉的文件读不出，按难撤回：" + textfmt.OneLine(err.Error(), 200)}}
	}
	var deleted []string
	if names != "" {
		deleted = slices.DeleteFunc(strings.Split(names, "\n"), func(f string) bool { return f == ReadyFile })
	}
	lines := addedLines(diff)
	lvl, reasons := Classify(c.Files, deleted, func(p string) string { return lines[p] }, c.Item, rules)
	light, flow := lightFlow(c, lvl, diff)
	return &RiskVerdict{Level: lvl, Reasons: reasons, Light: light, Flow: flow}
}

// addedLines 从 git diff 里按文件取新增的行。文件头（diff --git 到 @@ 之间）的「+++ b/路径」定文件，之后「+」开头的是新增行。
func addedLines(diff string) map[string]string {
	out := map[string]string{}
	cur, header := "", false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur, header = "", true
		case header && strings.HasPrefix(l, "+++ "):
			cur = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/"), "\t")
			if cur == "/dev/null" {
				cur = ""
			}
		case strings.HasPrefix(l, "@@"):
			header = false
		case !header && cur != "" && strings.HasPrefix(l, "+"):
			out[cur] += l[1:] + "\n"
		}
	}
	return out
}

// holdReason：门都过了还不能自动合的原因，能合是空。用户放行过的不再挡。
func holdReason(p Plan, v *RiskVerdict, results []Result) string {
	if p.Override != "" || v == nil {
		return ""
	}
	switch v.Level {
	case RiskIrreversible:
		return "难撤回，门过了也不自动合并，等你看过再放行：" + strings.Join(v.Reasons, "；")
	case RiskBroad:
		if _, ran := resultNamed(results, "test"); !ran {
			return "影响面广但没有确定性检查（测试门关着或不适用），判不了，等你定：" + strings.Join(v.Reasons, "；")
		}
	}
	return ""
}

func riskLabel(r Risk) string {
	switch r {
	case RiskIrreversible:
		return "难撤回"
	case RiskBroad:
		return "影响面广"
	case RiskLocal:
		return "局部"
	}
	return string(r)
}
