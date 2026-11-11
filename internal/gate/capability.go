package gate

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"agentq/internal/textfmt"
)

// 能力清单（自我进化 E7）：agentq 长出的每项能力是一组东西——知识（skills/ 或 rules/ 里的文件）、检查（能写成退出码的脚本）、
// 考题（evals/ 里的题）、怎么判断要不要用。清单是 capabilities/capabilities.json，检查脚本放在同一目录，都编译进二进制：
// 能力门跑的是运行中这一代的检查，候选分支上新加的检查合并、升级之后才起作用。说明见 docs/reference/features.md 的能力门一节。

// CapabilitiesFile 是清单在仓库里的位置。评测门对 type 是 capability 的交付读基点和候选上的这份，比出新登记的能力。
const CapabilitiesFile = "internal/gate/capabilities/capabilities.json"

//go:embed capabilities
var capFS embed.FS

// Capability 是清单里的一项。
type Capability struct {
	Name string `json:"name"`
	// Domain 是英文领域名，和评测题 case.json 的 domain 是同一套（evolve 的领域表对上中文名）。
	Domain string `json:"domain"`
	// Knowledge 是仓库里的知识文件（skills/ 或 rules/ 下）。
	Knowledge []string `json:"knowledge"`
	// Check 是同目录下的检查脚本名，退出码非 0 算报出问题、125 算这次跑不了。没有就只有知识。
	Check string `json:"check,omitempty"`
	// Cases 是这项能力的考题（evals/ 下的题目录名）。
	Cases []string `json:"cases"`
	// When 是怎么判断这个项目要不要用。
	When CapWhen `json:"when"`
	// Added 是加进来的日期（2006-01-02）。
	Added string `json:"added"`
}

// CapWhen：两项都写时都要满足；至少写一项，不然每个项目都跑。
type CapWhen struct {
	// Files：项目根目录有其中任一（可写通配，如 *.xcodeproj）。
	Files []string `json:"files,omitempty"`
	// Changed：这次改的文件里有匹配其中任一的（通配同 judgeFiles，如 web/**、*.tsx）。
	Changed []string `json:"changed,omitempty"`
}

// CapRun 是能力门里一项能力这次的结果，进 Result.Caps。evolve 靠它记能力有没有帮上忙。
type CapRun struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"` // pass：用上了（检查过了或没有检查）；fail：检查报出问题；unknown：检查跑不了
}

var capNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// AddedDate 是 Added 解析成的日期，写错是零值。
func (c Capability) AddedDate() time.Time {
	t, _ := time.ParseInLocation("2006-01-02", c.Added, time.Local)
	return t
}

// ParseCapabilities 解析清单。写坏了报错：清单是 agentq 自己的源码，坏了是候选的问题，不悄悄少几项。
func ParseCapabilities(raw []byte) ([]Capability, error) {
	var doc struct {
		Capabilities []Capability `json:"capabilities"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("能力清单不是合法的 JSON：%w", err)
	}
	seen := map[string]bool{}
	for _, c := range doc.Capabilities {
		switch {
		case !capNameRe.MatchString(c.Name):
			return nil, fmt.Errorf("能力名 %q 要是小写字母、数字和连字符", c.Name)
		case seen[c.Name]:
			return nil, fmt.Errorf("能力 %s 登记了两次", c.Name)
		case c.Domain == "":
			return nil, fmt.Errorf("能力 %s 没写 domain", c.Name)
		case len(c.Cases) == 0:
			return nil, fmt.Errorf("能力 %s 没写考题（cases）：没考题的能力不接受", c.Name)
		case len(c.When.Files) == 0 && len(c.When.Changed) == 0:
			return nil, fmt.Errorf("能力 %s 没写 when：要按项目判断用不用", c.Name)
		case c.AddedDate().IsZero():
			return nil, fmt.Errorf("能力 %s 的 added %q 要写成 2006-01-02", c.Name, c.Added)
		case c.Check != "" && (c.Check != path.Base(c.Check) || strings.HasPrefix(c.Check, ".")):
			return nil, fmt.Errorf("能力 %s 的 check 只写同目录下的脚本名", c.Name)
		}
		seen[c.Name] = true
	}
	return doc.Capabilities, nil
}

// capRegistry 是运行中这一代的清单。测试换掉它。
var capRegistry = func() ([]Capability, error) {
	raw, err := capFS.ReadFile("capabilities/capabilities.json")
	if err != nil {
		return nil, fmt.Errorf("读能力清单：%w", err)
	}
	return ParseCapabilities(raw)
}

// capScript 读这一代的检查脚本。测试换掉 capRegistry 时一起给脚本。
var capScript = func(name string) ([]byte, error) { return capFS.ReadFile("capabilities/" + name) }

// Capabilities 是运行中这一代登记的能力。
func Capabilities() ([]Capability, error) { return capRegistry() }

type capGate struct{ d Deps }

func (capGate) Name() string { return "capability" }

func (g capGate) AppliesTo(c Candidate) bool {
	return len(g.applicable(context.Background(), c)) > 0
}

// applicable 是这个候选要用的能力。清单读不了就一项都不用：能力门只是 warn 档的补充，清单坏了由测试挡。
func (g capGate) applicable(ctx context.Context, c Candidate) []Capability {
	caps, err := Capabilities()
	if err != nil || len(caps) == 0 {
		return nil
	}
	rev := c.Merge
	if rev == "" {
		rev = c.Head
	}
	var root []string
	if out, err := g.d.Git(ctx, c.ProjectDir, "ls-tree", "--name-only", rev); err == nil {
		root = strings.Fields(out)
	}
	var out []Capability
	for _, cp := range caps {
		if capApplies(cp.When, root, c.Files) {
			out = append(out, cp)
		}
	}
	return out
}

// capApplies：root 是项目根目录的文件名，changed 是这次改的文件。
func capApplies(w CapWhen, root, changed []string) bool {
	if len(w.Files) > 0 && !slices.ContainsFunc(w.Files, func(p string) bool {
		return slices.ContainsFunc(root, func(f string) bool { ok, _ := path.Match(p, f); return ok })
	}) {
		return false
	}
	if len(w.Changed) > 0 && !slices.ContainsFunc(w.Changed, func(p string) bool {
		return slices.ContainsFunc(changed, func(f string) bool { return globMatch(p, f) })
	}) {
		return false
	}
	return true
}

func (g capGate) Run(ctx context.Context, c Candidate) Result {
	caps := g.applicable(ctx, c)
	r := Result{Outcome: Pass}
	var flagged, unknown, used []string
	for _, cp := range caps {
		run := CapRun{Name: cp.Name, Outcome: Pass}
		if cp.Check != "" {
			var note string
			run.Outcome, note = g.check(ctx, c, cp)
			r.Evidence = append(r.Evidence, "能力 "+cp.Name+" 的检查（"+cp.Check+"）："+note)
		}
		switch run.Outcome {
		case Fail:
			flagged = append(flagged, cp.Name)
		case Unknown:
			unknown = append(unknown, cp.Name)
		default:
			used = append(used, cp.Name)
		}
		r.Caps = append(r.Caps, run)
	}
	switch {
	case len(flagged) > 0:
		r.Outcome = Fail
		r.Summary = "能力检查报出问题：" + strings.Join(flagged, "、")
	case len(unknown) > 0:
		r.Outcome = Unknown
		r.Summary = "能力检查跑不了：" + strings.Join(unknown, "、")
	default:
		r.Summary = "用上的能力：" + strings.Join(used, "、")
	}
	return r
}

// check 跑一项能力的检查：脚本取自运行中这一代，在候选 worktree 里跑，AGENTQ_BASE 是默认分支基点。
func (g capGate) check(ctx context.Context, c Candidate, cp Capability) (Outcome, string) {
	body, err := capScript(cp.Check)
	if err != nil {
		return Unknown, "这一代没有这个脚本：" + textfmt.OneLine(err.Error(), 200)
	}
	script := filepath.Join(c.WorkDir, "cap-"+cp.Name+".sh")
	if err := os.WriteFile(script, body, 0o700); err != nil {
		return Unknown, "写不了脚本：" + textfmt.OneLine(err.Error(), 200)
	}
	defer os.Remove(script)
	log := filepath.Join(c.WorkDir, "cap-"+cp.Name+".log")
	res := g.d.Script(ctx, ScriptOpts{Script: script, Dir: c.Dir, Log: log, Timeout: c.Timeout, WorkDir: c.WorkDir, Mark: gateMark(c.JobID), Env: []string{"AGENTQ_BASE=" + c.Base}})
	note := res.Note + "，日志 " + g.d.Tilde(log)
	switch res.Status {
	case ScriptPassed:
		return Pass, note
	case ScriptFailed:
		return Fail, note
	}
	return Unknown, note
}

// errNoRegistry：基点上还没有能力清单（老代码）。
var errNoRegistry = errors.New("没有能力清单")

// capabilitiesAt 读某个提交上的清单。
func capabilitiesAt(ctx context.Context, d Deps, dir, rev string) ([]Capability, error) {
	if _, err := d.Git(ctx, dir, "cat-file", "-e", rev+":"+CapabilitiesFile); err != nil {
		return nil, errNoRegistry
	}
	raw, err := d.Git(ctx, dir, "show", rev+":"+CapabilitiesFile)
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", CapabilitiesFile, err)
	}
	return ParseCapabilities([]byte(raw))
}
