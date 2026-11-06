package gate

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"agentq/internal/textfmt"
)

// 测试门。能写成命令和退出码的才有阻塞权，这道门全是这种：
//   - 跑 main 上的 .agentq/verify，任务改不了门。首次由任务新增时用任务的版本，并标给审查门看。
//   - 任务动了用户确认过的界面基线（docs/design/approved/）：失败，门说明也不算。
//   - 查 diff：新增跳过、删测试、改 .agentq/。提交说明里没有「门说明：」就算失败；写了原因交给审查门判断。
//   - 修 bug 附了新测试：把非测试文件还原到基点再跑一次，必须失败。撤掉修复还过的测试证明不了修复。
//   - 第一次失败连跑两次确认，不一致标 flaky，留痕。

const (
	// VerifyScript 是项目的检查命令。
	VerifyScript = ".agentq/verify"
	agentqDir    = ".agentq"
	// NoteMark 是提交说明里解释「为什么改测试或门脚本」的固定写法，只认这个写法。
	NoteMark = "门说明："
)

type testGate struct{ d Deps }

func (testGate) Name() string { return "test" }

func (testGate) AppliesTo(Candidate) bool { return true }

func (g testGate) Run(ctx context.Context, c Candidate) Result {
	// 基线只认用户确认过的。这条写得成判断，不看门说明：用户要改，直接提交到默认分支或 gate pass。
	// c.Files 是 --no-renames 算的，搬走基线也会列出来。
	var baseline []string
	for _, f := range c.Files {
		if inBaseline(f) {
			baseline = append(baseline, f)
		}
	}
	if len(baseline) > 0 {
		return Result{Outcome: Fail, Summary: "任务动了 " + BaselineDir + "：基线只认用户确认过的，引擎和任务都不更新。用户要改就直接提交到默认分支，或 agentq gate pass", Evidence: baseline}
	}
	first, note, err := locateScript(ctx, g.d, c, VerifyScript)
	if err != nil {
		return Result{Outcome: Unknown, Summary: "查不了 " + VerifyScript + "：" + textfmt.OneLine(err.Error(), 200)}
	}
	if note == "" && !first {
		return Result{Outcome: Unknown, Summary: "没有能跑的 " + VerifyScript + "：任务合同要求先补上（一条命令跑全部检查，退出码 0 算过）"}
	}
	var evidence []string
	if first {
		evidence = append(evidence, note)
	}
	flags, newTests, firstScripts, err := g.scanDiff(ctx, c, first)
	if err != nil {
		return Result{Outcome: Unknown, Summary: "查不了改动：" + textfmt.OneLine(err.Error(), 200)}
	}
	evidence = append(evidence, firstScripts...)
	if len(flags) > 0 {
		reasons := g.notes(ctx, c)
		if len(reasons) == 0 {
			return Result{Outcome: Fail, Summary: "改了测试或门脚本，提交说明里没写原因（单写一行「" + NoteMark + "原因」）", Evidence: flags}
		}
		evidence = append(evidence, flags...)
		for _, r := range reasons {
			evidence = append(evidence, "任务的说明："+r)
		}
	}
	res := g.runConfirmed(ctx, c, first)
	res.Evidence = append(evidence, res.Evidence...)
	if res.Outcome != Pass {
		return res
	}
	return g.checkFix(ctx, c, first, newTests, res)
}

// runConfirmed 跑脚本。第一次失败连跑两次：两次都过标 flaky 放行（留痕），否则算失败。
func (g testGate) runConfirmed(ctx context.Context, c Candidate, first bool) Result {
	run := func(log string) (ScriptResult, error) {
		if err := prepareCandidate(ctx, g.d, c, first); err != nil {
			return ScriptResult{}, err
		}
		return runProjectScript(ctx, g.d, c, VerifyScript, log, nil), nil
	}
	logPath := func(log string) string { return "日志 " + g.d.Tilde(filepath.Join(c.WorkDir, log)) }
	r, err := run("test.log")
	if err != nil {
		return Result{Outcome: Unknown, Summary: "还原 worktree 失败：" + textfmt.OneLine(err.Error(), 200)}
	}
	switch r.Status {
	case ScriptPassed:
		return Result{Outcome: Pass, Summary: VerifyScript + " 过了", Evidence: []string{"跑了 " + VerifyScript + "，" + logPath("test.log")}}
	case ScriptFailed:
	default:
		return Result{Outcome: Unknown, Summary: VerifyScript + " 判不了：" + textfmt.OneLine(r.Note, 200), Evidence: []string{logPath("test.log")}}
	}
	passes := 0
	for _, log := range []string{"test-again1.log", "test-again2.log"} {
		again, err := run(log)
		if err != nil {
			return Result{Outcome: Unknown, Summary: "还原 worktree 失败：" + textfmt.OneLine(err.Error(), 200)}
		}
		if again.Status == ScriptPassed {
			passes++
		} else if again.Status != ScriptFailed {
			return Result{Outcome: Unknown, Summary: VerifyScript + " 判不了：" + textfmt.OneLine(again.Note, 200), Evidence: []string{logPath(log)}}
		}
	}
	ev := []string{"第一次 " + r.Note + "，" + logPath("test.log") + "；连跑两次过了 " + fmt.Sprint(passes) + " 次，" + logPath("test-again1.log") + "，" + logPath("test-again2.log")}
	switch passes {
	case 2:
		return Result{Outcome: Pass, Summary: VerifyScript + " 不稳定（flaky）：第一次失败，连跑两次都过", Evidence: ev}
	case 1:
		return Result{Outcome: Fail, Summary: VerifyScript + " 没过，结果不一致（flaky）：" + r.Note, Evidence: ev}
	}
	return Result{Outcome: Fail, Summary: VerifyScript + " 没过：" + r.Note, Evidence: ev}
}

// checkFix：修 bug 附了新测试时，把非测试文件还原到基点再跑一次，必须失败。
func (g testGate) checkFix(ctx context.Context, c Candidate, first, newTests bool, res Result) Result {
	if c.Type != "fix" {
		return res
	}
	if !newTests {
		res.Evidence = append(res.Evidence, "修复没带新测试，没做「撤掉修复必须失败」的检查")
		return res
	}
	var code []string
	for _, f := range c.Files {
		if !isTestFile(f) && !strings.HasPrefix(f, agentqDir+"/") {
			code = append(code, f)
		}
	}
	if len(code) == 0 {
		res.Evidence = append(res.Evidence, "只改了测试，没有可撤的修复")
		return res
	}
	if err := prepareCandidate(ctx, g.d, c, first); err != nil {
		return Result{Outcome: Unknown, Summary: "还原 worktree 失败：" + textfmt.OneLine(err.Error(), 200), Evidence: res.Evidence}
	}
	for _, f := range code {
		out, err := g.d.Git(ctx, c.Dir, "ls-tree", c.Base, "--", f)
		if err == nil && out != "" {
			_, err = g.d.Git(ctx, c.Dir, "checkout", "-q", c.Base, "--", f)
		} else if err == nil {
			_, err = g.d.Git(ctx, c.Dir, "rm", "-q", "-f", "--", f)
		}
		if err != nil {
			return Result{Outcome: Unknown, Summary: "撤不掉 " + f + "：" + textfmt.OneLine(err.Error(), 200), Evidence: res.Evidence}
		}
	}
	r := runProjectScript(ctx, g.d, c, VerifyScript, "test-reverted.log", nil)
	log := "日志 " + g.d.Tilde(filepath.Join(c.WorkDir, "test-reverted.log"))
	switch r.Status {
	case ScriptFailed:
		res.Evidence = append(res.Evidence, "撤掉修复后测试失败（"+r.Note+"），新测试真的盖住了这个 bug，"+log)
		return res
	case ScriptPassed:
		return Result{Outcome: Fail, Summary: "撤掉修复测试还是过：新测试证明不了修复，" + log,
			Evidence: append(res.Evidence, "还原到基点的文件："+strings.Join(code, "、"))}
	}
	return Result{Outcome: Unknown, Summary: "撤掉修复后判不了：" + textfmt.OneLine(r.Note, 200), Evidence: append(res.Evidence, log)}
}

var (
	testFileRe = regexp.MustCompile(`(^|/)(tests?|__tests__|specs?|Tests)/|_test\.[a-z]+$|(^|/)test_[^/]*\.py$|\.(test|spec)\.[cm]?[jt]sx?$|Tests?\.(swift|java|kt)$|_spec\.rb$`)
	skipRe     = regexp.MustCompile(`\bt\.Skip(f|Now)?\(|\b(it|test|describe|context)\.(skip|only)\b|\bx(it|describe|test)\(|#\[ignore|pytest\.mark\.(skip|xfail)|unittest\.skip|XCTSkip|@Disabled|@Ignore\b`)
	caseRe     = regexp.MustCompile("\\bfunc (?:\\([^)]*\\) )?([Tt]est\\w+)\\(|\\bdef (test_\\w+)|^\\s*(?:it|test|describe)\\(\\s*[\"'`]([^\"'`]+)|#\\[test\\]")
)

// isTestFile 按路径认测试文件。文档不算：方案放在 docs/specs/，正文里提到 it.skip 很正常。
func isTestFile(path string) bool {
	if strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown") {
		return false
	}
	return testFileRe.MatchString(path)
}

// inlineTests：测试和代码写在同一个文件的语言。跳过也要查，但不算新测试：撤修复会连测试一起撤。
func inlineTests(path string) bool { return strings.HasSuffix(path, ".rs") }

func caseName(line string) string {
	m := caseRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	for _, s := range m[1:] {
		if s != "" {
			return s
		}
	}
	return "#[test]"
}

// scanDiff 查候选相对基点的改动：返回要标出来的事、有没有新增测试、首次新增的界面和成品脚本（不算动门，标给审查）。改名不算删。
func (g testGate) scanDiff(ctx context.Context, c Candidate, first bool) ([]string, bool, []string, error) {
	status, err := g.d.Git(ctx, c.Dir, "diff", "--name-status", "-M", c.Base, c.Merge)
	if err != nil {
		return nil, false, nil, err
	}
	var flags, firstScripts []string
	newTests := false
	for _, line := range strings.Split(status, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		path := f[len(f)-1]
		if path == ReadyFile { // 任务的交付单，main 上残留的也由候选删掉，不算任务改了门
			continue
		}
		switch {
		case f[0] == "D" && isTestFile(path):
			flags = append(flags, "删了测试文件 "+path)
		case f[0] == "A" && isTestFile(path):
			newTests = true
		}
		switch {
		case f[0] == "A" && (path == UIScript || path == SmokeScript):
			firstScripts = append(firstScripts, "首次由任务新增 "+path+"，门用任务的版本：审查时看它是不是真的在查东西")
		case strings.HasPrefix(path, agentqDir+"/") && !(first && path == VerifyScript):
			flags = append(flags, "改了 "+path)
		}
	}
	diff, err := g.d.Git(ctx, c.Dir, "diff", "--unified=0", "-M", c.Base, c.Merge)
	if err != nil {
		return nil, false, nil, err
	}
	var removed, added []string
	file := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimRight(strings.TrimPrefix(line, "+++ "), "\t"), "b/")
			continue
		case strings.HasPrefix(line, "--- "):
			continue
		}
		test := isTestFile(file)
		if !test && !inlineTests(file) {
			continue
		}
		if body, ok := strings.CutPrefix(line, "+"); ok {
			if skipRe.MatchString(body) {
				flags = append(flags, "新增跳过 "+file+"："+textfmt.OneLine(body, 100))
			}
			if name := caseName(body); name != "" && test {
				added = append(added, name)
				newTests = true
			}
		} else if body, ok := strings.CutPrefix(line, "-"); ok {
			if name := caseName(body); name != "" && test {
				removed = append(removed, name)
			}
		}
	}
	for _, name := range removed {
		if !slices.Contains(added, name) && name != "#[test]" {
			flags = append(flags, "删了测试用例 "+name)
		}
	}
	return flags, newTests, firstScripts, nil
}

// notes 取任务提交说明里的「门说明：」。只认独占一行开头的固定写法。
func (g testGate) notes(ctx context.Context, c Candidate) []string {
	body, err := g.d.Git(ctx, c.ProjectDir, "log", "--format=%B", c.Base+".."+c.Head)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), NoteMark); ok && strings.TrimSpace(r) != "" {
			out = append(out, textfmt.OneLine(r, 200))
		}
	}
	return out
}
