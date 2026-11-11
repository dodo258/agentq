package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"agentq/internal/textfmt"
)

// 评测门：只管 agentq 自己的仓库，是自我进化的候选评判。说明见 docs/reference/features.md 的评测门一节。
//   - 用合并态构建候选二进制，由运行中这一代跑 agentq eval judge：题目和判分是运行中这一代编译进去的，候选分支上改的题不算。
//   - 跑法是 --fake（含程序题），不调模型；和 ~/.agentq/evals/baseline.json 比，任一题或任一领域变差就退回。
//   - 没有能比的基线只记录不挡。
//   - 候选改了评测题、判分、门的判定或 .agentq/，门过了也不自动合并，等用户放行。只是新加整道题（evals/ 下基点没有的新目录）不算改尺子：题只加不改。
//   - 修过就记住：type 是 fix、改了 Go 代码的，必须同时新加一道题（能复现它，修之前那一代会判没过），没有就退回。
//   - 长能力（type 是 capability）：能力清单要新登记一项，并新加这个领域的考题，没有就退回；新题由运行中这一代分别在自己和候选上跑，
//     这个领域加上新题后的通过率或平均分要比基线（运行中这一代）高，别的领域照常不许变差，见 evolve 的 capabilityVerdict。

// EvalVerdict 是 agentq eval judge 写的结论文件，评测门读它。变差的判断在 evolve 里做，这里只照结论定门的结果。
type EvalVerdict struct {
	// Cases 是跑了几题，Failed 是这次没过或判不了的题（留出题只写编号）。
	Cases  int      `json:"cases"`
	Failed []string `json:"failed,omitempty"`
	// Baseline：有能比的基线（比上了至少一题）。没有时 Hint 写怎么建。
	Baseline bool   `json:"baseline"`
	Hint     string `json:"hint,omitempty"`
	Compared int    `json:"compared"`
	// Worse 非空就是变差了：哪题、哪个领域、从多少到多少。
	Worse  []string `json:"worse,omitempty"`
	Better []string `json:"better,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// 候选改了这些就不自动合并：评判它的尺子（评测题、判分和比较、门的判定）、项目门脚本，
// 和合并后自己升级、判断要不要回滚的那几处（不然候选能把回滚的线改松，过门后自己装上）。测试文件不算尺子。
var judgeFiles = []string{
	"evals/**", "assets.go", ".agentq/**",
	"internal/gate/*.go",
	"internal/evolve/case.go", "internal/evolve/score.go", "internal/evolve/harness.go", "internal/evolve/cmd.go", "internal/evolve/judge.go",
	"internal/evolve/upgrade.go", "internal/evolve/apply.go", "internal/evolve/selfgrow.go", "internal/setup/**",
}

// selfModule 认 agentq 自己的 go.mod。
var selfModule = regexp.MustCompile(`(?m)^module\s+agentq\s*$`)

type evalGate struct{ d Deps }

func (evalGate) Name() string { return "eval" }

// AppliesTo 只看基点：任务改 go.mod 或删评测集躲不开这道门。
func (g evalGate) AppliesTo(c Candidate) bool {
	return isSelfRepo(context.Background(), g.d, c.ProjectDir, c.Base)
}

// isSelfRepo：基点上 go.mod 是 module agentq，且有评测集和 internal/evolve。
func isSelfRepo(ctx context.Context, d Deps, dir, base string) bool {
	mod, err := d.Git(ctx, dir, "show", base+":go.mod")
	if err != nil || !selfModule.MatchString(mod) {
		return false
	}
	for _, p := range []string{"evals", "internal/evolve"} {
		if _, err := d.Git(ctx, dir, "cat-file", "-e", base+":"+p); err != nil {
			return false
		}
	}
	return true
}

// changedJudge 是这次改到的尺子文件。新加的整道题（added 里的目录）不算。
func changedJudge(files, added []string) []string {
	var hit []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || slices.ContainsFunc(added, func(d string) bool { return strings.HasPrefix(f, d+"/") }) {
			continue
		}
		if slices.ContainsFunc(judgeFiles, func(p string) bool { return globMatch(p, f) }) {
			hit = append(hit, f)
		}
	}
	return hit
}

func (g evalGate) Run(ctx context.Context, c Candidate) Result {
	start := time.Now()
	added := g.newCases(ctx, c)
	var extra []string
	if c.Type == "capability" {
		var why string
		if extra, why = g.capabilityArgs(ctx, c, added); why != "" {
			return Result{Outcome: Fail, Summary: why, Files: []string{"evals/", CapabilitiesFile}}
		}
	}
	r, build := g.judge(ctx, c, extra)
	took := time.Since(start).Round(time.Second)
	r.Summary += "；耗时 " + took.String()
	if build > 0 {
		r.Summary += fmt.Sprintf("（构建 %s，评测 %s）", build.Round(time.Second), (took - build.Round(time.Second)).Round(time.Second))
	}
	if len(added) > 0 {
		r.Evidence = append(r.Evidence, "新加的评测题："+strings.Join(added, "、")+"（只加不改，下一代起用它评判）")
	}
	if c.Type == "fix" && len(added) == 0 && touchesCode(c.Files) {
		r.Outcome = Fail
		r.Summary = "修了 bug 没加评测题：每个修好的真实问题要在 evals/ 新加一道能复现它的题（新目录，只加不改；修之前那一代跑它要判没过），同一个错才不会再犯；" + r.Summary
		r.Files = []string{"evals/"}
	}
	if hit := changedJudge(c.Files, added); len(hit) > 0 {
		r.Hold = "改了评测题、判分或门的判定（" + listFiles(hit) + "）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行"
		r.Evidence = append(r.Evidence, "改到的尺子："+strings.Join(hit, "、"))
	}
	return r
}

// judge 构建候选、让运行中这一代评判它。第二个返回值是构建用了多久，没构建是 0。
func (g evalGate) judge(ctx context.Context, c Candidate, extra []string) (Result, time.Duration) {
	self, err := g.d.Self()
	if err == nil {
		self, err = filepath.EvalSymlinks(self) // 装好的 agentq 常是指向当前一代的软链接，脚本要真实文件
	}
	if err != nil {
		return Result{Outcome: Unknown, Summary: "找不到运行中的 agentq，评判不了：" + textfmt.OneLine(err.Error(), 200)}, 0
	}
	goBin, err := exec.LookPath("go")
	if err == nil {
		goBin, err = filepath.EvalSymlinks(goBin)
	}
	if err != nil {
		return Result{Outcome: Unknown, Summary: "找不到 go，构建不了候选：" + textfmt.OneLine(err.Error(), 200)}, 0
	}
	work := filepath.Join(c.WorkDir, "eval")
	if err := os.RemoveAll(work); err != nil {
		return Result{Outcome: Unknown, Summary: "清不掉上次的评测目录：" + textfmt.OneLine(err.Error(), 200)}, 0
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return Result{Outcome: Unknown, Summary: "建不了评测目录：" + textfmt.OneLine(err.Error(), 200)}, 0
	}
	bin := filepath.Join(work, "agentq")
	defer os.Remove(bin) // 只这一次用，十来 MB 不留；删不掉下次过门开头清
	buildLog := filepath.Join(c.WorkDir, "eval-build.log")
	logNote := func(p string) string { return "日志 " + g.d.Tilde(p) }

	started := time.Now()
	b := g.d.Script(ctx, ScriptOpts{Script: goBin, Args: []string{"build", "-o", bin, "."}, Dir: c.Dir, Log: buildLog, Timeout: c.Timeout, WorkDir: c.WorkDir, Mark: gateMark(c.JobID)})
	build := time.Since(started)
	switch b.Status {
	case ScriptPassed:
	case ScriptFailed:
		return Result{Outcome: Fail, Summary: "候选构建不了（go build " + b.Note + "）", Evidence: []string{logNote(buildLog)}}, build
	default:
		return Result{Outcome: Unknown, Summary: "候选构建判不了：" + textfmt.OneLine(b.Note, 200), Evidence: []string{logNote(buildLog)}}, build
	}

	out := filepath.Join(work, "verdict.json")
	evalLog := filepath.Join(c.WorkDir, "eval.log")
	r := g.d.Script(ctx, ScriptOpts{Script: self, Args: append([]string{"eval", "judge", "--bin", bin, "--out", out}, extra...), Dir: work, Log: evalLog, Timeout: c.Timeout, WorkDir: c.WorkDir, Mark: gateMark(c.JobID)})
	evidence := []string{"运行中的 agentq（" + g.d.Tilde(self) + "）用它自己的题评判候选：agentq eval judge --fake，" + logNote(evalLog)}
	if r.Status != ScriptPassed && r.Status != ScriptFailed { // 超时、被停：结论文件可能是半截的
		return Result{Outcome: Unknown, Summary: "评测判不了：" + textfmt.OneLine(r.Note, 200), Evidence: evidence}, build
	}
	raw, err := os.ReadFile(out)
	var v EvalVerdict
	if err == nil {
		err = json.Unmarshal(raw, &v)
	}
	if err != nil {
		return Result{Outcome: Unknown, Summary: "评测没给出结论（" + r.Note + "）：" + textfmt.OneLine(err.Error(), 200) + "，看日志", Evidence: evidence}, build
	}
	evidence = append(evidence, "结论 "+g.d.Tilde(out))
	return verdictResult(v, evidence), build
}

// verdictResult 把评测结论换成门的结果。
func verdictResult(v EvalVerdict, evidence []string) Result {
	for _, l := range v.Worse {
		evidence = append(evidence, "变差："+l)
	}
	for _, l := range v.Notes {
		evidence = append(evidence, "注意："+l)
	}
	if len(v.Worse) > 0 {
		return Result{Outcome: Fail, Summary: fmt.Sprintf("评测比基线变差 %d 处：%s", len(v.Worse), strings.Join(firstN(v.Worse, 4), "；")), Evidence: evidence}
	}
	failed := ""
	if len(v.Failed) > 0 {
		failed = "，这次没过：" + strings.Join(firstN(v.Failed, 4), "、")
	}
	if !v.Baseline {
		return Result{Outcome: Pass, Summary: fmt.Sprintf("没有能比的基线，只记录不挡：跑了 %d 题%s；%s", v.Cases, failed, v.Hint), Evidence: evidence}
	}
	return Result{Outcome: Pass, Summary: fmt.Sprintf("评测没有变差：跑了 %d 题，和基线比了 %d 题%s", v.Cases, v.Compared, failed), Evidence: evidence}
}

func firstN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return append(slices.Clone(lines[:n]), fmt.Sprintf("等 %d 处", len(lines)))
}

// caseDir 是评测题目录的样子（和 internal/evolve 认题的规则一样：题号-短名）。
var caseDir = regexp.MustCompile(`^evals/[0-9]+-[a-z0-9][a-z0-9-]*$`)

// newCases 是这次新加的整道题：evals/ 下基点没有的题目录，里面的文件全是新加的。认不出（git 出错）就当没有。
func (g evalGate) newCases(ctx context.Context, c Candidate) []string {
	tip := c.Merge
	if tip == "" {
		tip = c.Head
	}
	out, err := g.d.Git(ctx, c.ProjectDir, "diff", "--name-status", "--no-renames", c.Base, tip, "--", "evals/")
	if err != nil {
		return nil
	}
	added := map[string]bool{} // 题目录 → 里面改到的文件是不是全是新加的
	for _, line := range strings.Split(out, "\n") {
		status, file, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		parts := strings.SplitN(file, "/", 3)
		if len(parts) < 3 {
			continue
		}
		dir := parts[0] + "/" + parts[1]
		if status != "A" || !caseDir.MatchString(dir) {
			added[dir] = false
		} else if _, seen := added[dir]; !seen {
			added[dir] = true
		}
	}
	var dirs []string
	for d, ok := range added {
		if !ok {
			continue
		}
		if _, err := g.d.Git(ctx, c.ProjectDir, "cat-file", "-e", c.Base+":"+d); err == nil {
			continue // 基点上已经有这道题：往里加文件也是改题
		}
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	return dirs
}

// touchesCode：改了 Go 代码（测试和评测题不算）。
func touchesCode(files []string) bool {
	return slices.ContainsFunc(files, func(f string) bool {
		return strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") && !strings.HasPrefix(f, "evals/")
	})
}

// capabilityArgs：type 是 capability 的交付。候选上的能力清单要比基点多登记至少一项，每项新能力都要有同领域的新考题。
// 返回给 eval judge 加的参数（新题在候选 worktree 里的位置、题号、要涨分的领域）；不合要求时返回退回的原因。
func (g evalGate) capabilityArgs(ctx context.Context, c Candidate, added []string) ([]string, string) {
	tip := c.Merge
	if tip == "" {
		tip = c.Head
	}
	cur, err := capabilitiesAt(ctx, g.d, c.ProjectDir, tip)
	if err != nil && !errors.Is(err, errNoRegistry) {
		return nil, "type 是 capability，候选上的能力清单（" + CapabilitiesFile + "）读不了：" + textfmt.OneLine(err.Error(), 200)
	}
	old, _ := capabilitiesAt(ctx, g.d, c.ProjectDir, c.Base) // 基点没有或读不了：都算新登记的
	var fresh []Capability
	for _, cp := range cur {
		if !slices.ContainsFunc(old, func(o Capability) bool { return o.Name == cp.Name }) {
			fresh = append(fresh, cp)
		}
	}
	if len(fresh) == 0 {
		return nil, "type 是 capability，能力清单（" + CapabilitiesFile + "）里却没有新登记的能力：名字、领域、知识文件、检查、考题、启用条件、日期都要登记"
	}
	domainOf := map[string]string{}
	for _, d := range added {
		raw, err := g.d.Git(ctx, c.ProjectDir, "show", tip+":"+d+"/case.json")
		var cs struct {
			Domain string `json:"domain"`
		}
		if err == nil && json.Unmarshal([]byte(raw), &cs) == nil {
			domainOf[d] = cs.Domain
		}
	}
	var missing, domains []string
	for _, cp := range fresh {
		has := false
		for _, dom := range domainOf {
			has = has || dom == cp.Domain
		}
		if !has {
			missing = append(missing, cp.Name+"（"+cp.Domain+"）")
		}
		if !slices.Contains(domains, cp.Domain) {
			domains = append(domains, cp.Domain)
		}
	}
	if len(missing) > 0 {
		return nil, "新能力没新加该领域的考题：" + strings.Join(missing, "、") + "。先在 evals/ 新加这个领域的题（domain 写同一个），在当前这一代上判没过，再做能力"
	}
	args := []string{"--new-dir", filepath.Join(c.Dir, "evals")}
	for _, d := range added {
		args = append(args, "--new", path.Base(d))
	}
	for _, d := range domains {
		args = append(args, "--domain", d)
	}
	return args, ""
}
