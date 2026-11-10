package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
//   - 候选改了评测题、判分、门的判定或 .agentq/，门过了也不自动合并，等用户放行。

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

// 候选改了这些就不自动合并：评判它的尺子（评测题、判分和比较、门的判定）和项目门脚本。测试文件不算尺子。
var judgeFiles = []string{
	"evals/**", "assets.go", ".agentq/**",
	"internal/gate/*.go",
	"internal/evolve/case.go", "internal/evolve/score.go", "internal/evolve/harness.go", "internal/evolve/cmd.go", "internal/evolve/judge.go",
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

// changedJudge 是这次改到的尺子文件。
func changedJudge(files []string) []string {
	var hit []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
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
	r, build := g.judge(ctx, c)
	took := time.Since(start).Round(time.Second)
	r.Summary += "；耗时 " + took.String()
	if build > 0 {
		r.Summary += fmt.Sprintf("（构建 %s，评测 %s）", build.Round(time.Second), (took - build.Round(time.Second)).Round(time.Second))
	}
	if hit := changedJudge(c.Files); len(hit) > 0 {
		r.Hold = "改了评测题、判分或门的判定（" + listFiles(hit) + "）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行"
		r.Evidence = append(r.Evidence, "改到的尺子："+strings.Join(hit, "、"))
	}
	return r
}

// judge 构建候选、让运行中这一代评判它。第二个返回值是构建用了多久，没构建是 0。
func (g evalGate) judge(ctx context.Context, c Candidate) (Result, time.Duration) {
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
	r := g.d.Script(ctx, ScriptOpts{Script: self, Args: []string{"eval", "judge", "--bin", bin, "--out", out}, Dir: work, Log: evalLog, Timeout: c.Timeout, WorkDir: c.WorkDir, Mark: gateMark(c.JobID)})
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
