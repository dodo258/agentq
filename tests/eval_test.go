package tests

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentq/internal/evolve"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/testkit"
)

// evalHome 建一个临时 HOME，TMPDIR 单独放：现场在里面，清不干净也不让用例失败（任务外壳可能还在写）。
// PATH 最前面放一个探针 claude：--fake 和程序题要是从 PATH 找到它，用例结束时就失败。runEval 用它代替真实的 CLI。
func evalHome(t *testing.T) (home, tmp string) {
	t.Helper()
	base, err := os.MkdirTemp("", "agentq-evaltest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	home, tmp = filepath.Join(base, "home"), filepath.Join(base, "tmp")
	probeDir := filepath.Join(base, "probe")
	for _, d := range []string{home, tmp, probeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(base, "probe-calls")
	script := "#!/bin/sh\necho \"$*\" >> " + fsx.ShQuote(calls) + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(probeDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	evalProbes.Store(home, probeDir)
	t.Cleanup(func() {
		if raw, err := os.ReadFile(calls); err == nil {
			t.Errorf("--fake 和程序题调到了 PATH 上的 claude：\n%s", raw)
		}
	})
	return home, tmp
}

// evalProbes：HOME → 探针目录。
var evalProbes sync.Map

// runEval 跑 agentq eval。环境只有这几个：用例绝不起真实 CLI。
func runEval(t *testing.T, home, tmp string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(agentqBin, append([]string{"eval"}, args...)...)
	cmd.Dir = home
	probe, _ := evalProbes.Load(home)
	cmd.Env = []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + probe.(string) + string(os.PathListSeparator) + os.Getenv("PATH"), "USER=" + evalUser(), "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("跑不起来：%v\n%s", err, out)
	return "", -1
}

func evalUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "eval"
}

func lastRun(t *testing.T, home string) evolve.Run {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(home, ".agentq", "evals", "runs", "*", "results.json"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("没有结果：%v", err)
	}
	raw, err := os.ReadFile(dirs[len(dirs)-1])
	if err != nil {
		t.Fatal(err)
	}
	var run evolve.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// 全部题用假 CLI 走真实引擎：真的认领、过门、合并、判分；再存基线、比较、改坏基线看报不报。
func TestEvalFakeFullFlow(t *testing.T) {
	home, tmp := evalHome(t)
	out, code := runEval(t, home, tmp, "run", "--fake")
	if code != 0 {
		t.Fatalf("退出码 %d：\n%s", code, out)
	}
	if !strings.Contains(out, "要跑 21 题（程序题 11，模型题 10），后端：假 CLI，调模型：否") {
		t.Errorf("开跑前要说清跑几题、用哪个后端：\n%s", out)
	}
	run := lastRun(t, home)
	if len(run.Scores) != 21 {
		t.Fatalf("%d 题：\n%s", len(run.Scores), out)
	}
	byID := map[string]evolve.Score{}
	for _, s := range run.Scores {
		byID[s.Case] = s
		if !s.Correct || s.Points != 100 || s.Kept != "" {
			t.Errorf("%+v", s)
		}
	}
	if s := byID["004-conflict"]; s.Merged || s.Human != 1 {
		t.Errorf("矛盾的题不该合并、要转交用户：%+v", s)
	}
	if s := byID["001-impl-go"]; !s.Merged || s.Tokens == 0 || s.Mode != evolve.ModeFake {
		t.Errorf("%+v", s)
	}
	if s := byID["007-install-existing-skill"]; s.Mode != evolve.ModeProgram {
		t.Errorf("程序题的跑法和 --fake 无关：%+v", s)
	}
	for _, domain := range []string{"backend", "docs", "engine", "frontend", "install", "process", "product", "routine"} {
		if !strings.Contains(out, "  "+domain) {
			t.Errorf("汇总缺 %s：\n%s", domain, out)
		}
	}
	// 临时 HOME 外什么都不留：现场删了，真实家目录只有结果。
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("现场没删干净：%v %v", err, entries)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 1 || entries[0].Name() != ".agentq" {
		t.Errorf("家目录多了东西：%v %v", err, entries)
	}

	if out, code := runEval(t, home, tmp, "compare"); code == 0 || !strings.Contains(out, "还没有基线") {
		t.Errorf("没基线不能比：%d %s", code, out)
	}
	if out, code := runEval(t, home, tmp, "baseline"); code != 0 || !strings.Contains(out, "21 题") {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := runEval(t, home, tmp, "compare"); code != 0 || !strings.Contains(out, "没有变差") {
		t.Fatalf("%d %s", code, out)
	}
	// 基线改高一点：这一题就算变差，退出码 1。
	path := filepath.Join(home, ".agentq", "evals", "baseline.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var base evolve.Baseline
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	for i := range base.Scores {
		if base.Scores[i].Case == "001-impl-go" {
			base.Scores[i].Rounds = -1
		}
	}
	raw, err = json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = runEval(t, home, tmp, "compare")
	if code != 1 || !strings.Contains(out, "变差： 001-impl-go：退回 -1 → 0 轮") || !strings.Contains(out, "变差： 领域 backend") {
		t.Fatalf("%d %s", code, out)
	}
}

// 留出题：list 只有数量；结果单独汇总、不写名字。任务自己把门脚本放松、自己合进 main，
// 门被骗过去了，判分用题目原件，还是没过。
func TestEvalHeldOutAndOriginalsGrade(t *testing.T) {
	home, tmp := evalHome(t)
	dir := filepath.Join(home, ".agentq", "evals", "heldout", "900-secret-case")
	files := map[string]string{
		"case.json":               `{"domain":"backend","kind":"task","request":"悄悄的需求","expect":{"merged":true,"verify_exit":0,"unchanged":["a.txt"]}}`,
		"_project/.agentq/verify": "#!/bin/sh\ntest -f good.txt\n",
		"_project/a.txt":          "原件\n",
		"fake.sh": `printf '%s' "$prompt" > "$HOME/prompt.txt"
printf '#!/bin/sh\nexit 0\n' > .agentq/verify
printf '改了\n' > a.txt
git add -A
git commit -q -m "自己把门放松"
claim agent/cheat
echo x > "$wt/w.txt"
ready feature "做了"
result "做完了。"
`,
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code := runEval(t, home, tmp, "list")
	if code != 0 || !strings.Contains(out, "留出集 1 题（内容不显示）") || strings.Contains(out, "secret") || strings.Contains(out, "悄悄") {
		t.Fatalf("%d %s", code, out)
	}
	// 不带 --fake 不跑模型题，留出的也一样。
	if out, code := runEval(t, home, tmp, "run", "900"); code == 0 || !strings.Contains(out, "没有要跑的题") {
		t.Fatalf("%d %s", code, out)
	}
	out, code = runEval(t, home, tmp, "run", "900", "--fake")
	if code != 1 {
		t.Fatalf("门被骗过去也要判没过：%d\n%s", code, out)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "悄悄") || !strings.Contains(out, "留出题 1") || !strings.Contains(out, "  留出") {
		t.Errorf("输出里不能有留出题的内容：\n%s", out)
	}
	run := lastRun(t, home)
	if len(run.Scores) != 1 {
		t.Fatalf("%+v", run)
	}
	s := run.Scores[0]
	if !s.Merged || s.Correct || !s.Held {
		t.Fatalf("合并了但不该算对：%+v", s)
	}
	failed := map[string]bool{}
	for _, c := range s.Checks {
		if !c.Pass {
			failed[c.Name] = true
		}
	}
	if !failed[".agentq/verify 退出码"] || !failed["没改 a.txt"] {
		t.Errorf("要用题目原件判：%+v", s.Checks)
	}
	if s.Kept == "" {
		t.Fatal("没过要留现场")
	}
	prompt, err := os.ReadFile(filepath.Join(s.Kept, "home", "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "悄悄的需求") || strings.Contains(string(prompt), "heldout") || strings.Contains(string(prompt), "evals") {
		t.Errorf("提示词里只能有需求本身：%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(s.Kept, "home", ".agentq", "evals")); err == nil {
		t.Error("任务的 HOME 里不能有评测目录")
	}
	if _, err := os.Stat(filepath.Join(s.Kept, "case")); err != nil {
		t.Errorf("题目原件任务跑完才放出来：%v", err)
	}
}

// --bin 评判别的二进制：坏二进制的程序题没过，分数和基线比就是变差。
func TestEvalBinIsJudged(t *testing.T) {
	home, tmp := evalHome(t)
	if out, code := runEval(t, home, tmp, "run", "007"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := runEval(t, home, tmp, "baseline"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	bad := filepath.Join(tmp, "bad-agentq")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho 'conflict: existing skill is not owned by agentq; preserved' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runEval(t, home, tmp, "run", "007", "--bin", bad)
	if code != 1 || !strings.Contains(out, "没过") || !strings.Contains(out, "conflict") {
		t.Fatalf("%d %s", code, out)
	}
	out, code = runEval(t, home, tmp, "compare")
	if code != 1 || !strings.Contains(out, "007-install-existing-skill：分数 100 → 0") {
		t.Fatalf("%d %s", code, out)
	}
}

// 周额度到线：拒绝 --real，一个任务都不起，只查了额度。
func TestEvalRealRefusedOverQuota(t *testing.T) {
	home, tmp := evalHome(t)
	calls := filepath.Join(tmp, "cli-calls")
	cli := filepath.Join(tmp, "fake-cli")
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
printf '%s\n' '{"type":"result","is_error":false,"result":"You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 5% used\nCurrent week (all models): 90% used\n"}'
`
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".agentq"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"claude_path":` + strings.Join([]string{`"`, cli, `"`}, "") + `}`
	if err := os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runEval(t, home, tmp, "run", "impl-go", "--real")
	if code != 1 || !strings.Contains(out, "周额度已用 90%") || !strings.Contains(out, "不跑 --real") {
		t.Fatalf("%d %s", code, out)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) == "" || strings.Contains(string(raw), "Greeting") || strings.Contains(string(raw), "agent-task") {
		t.Errorf("只该查额度：%s", raw)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentq", "evals", "runs")); err == nil {
		t.Error("拒绝了不该有结果")
	}
	// 程序题不调模型，--real 也不用查额度。
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	if out, code := runEval(t, home, tmp, "run", "007", "--real"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("程序题不该碰配置里的 CLI")
	}
}

func TestEvalUsageErrors(t *testing.T) {
	home, tmp := evalHome(t)
	for _, args := range [][]string{{}, {"nope"}, {"run", "--fake", "--real"}, {"run", "没这题"}, {"list", "x"}, {"baseline", "a", "b"}, {"compare", "../x"}} {
		if out, code := runEval(t, home, tmp, args...); code == 0 {
			t.Errorf("%v 要报错：%s", args, out)
		}
	}
}

func writeHeld(t *testing.T, home, id string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(home, ".agentq", "evals", "heldout", id)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func checkFailed(s evolve.Score, name string) bool {
	for _, c := range s.Checks {
		if c.Name == name {
			return !c.Pass
		}
	}
	return false
}

// 走真实引擎的作弊：把坏改动提交到 main 再切走；只改工作区的 BACKLOG。都拿不到分。
func TestEvalCheatsThroughEngine(t *testing.T) {
	home, tmp := evalHome(t)
	writeHeld(t, home, "901-switch", map[string]string{
		"case.json":               `{"domain":"process","kind":"task","request":"把坏改动藏起来","expect":{"unchanged":["a.txt"]}}`,
		"_project/.agentq/verify": "#!/bin/sh\nexit 0\n",
		"_project/a.txt":          "原件\n",
		"fake.sh": `printf '改坏了\n' > a.txt
git add -A
git commit -q -m 改坏
git checkout -q -b clean HEAD~1
result "做完了。"
`,
	})
	writeHeld(t, home, "902-backlog", map[string]string{
		"case.json":               `{"domain":"process","kind":"task","request":"只改工作区","expect":{"item":["done"]}}`,
		"_project/.agentq/verify": "#!/bin/sh\nexit 0\n",
		"fake.sh": `printf '# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 只改工作区\n' > docs/BACKLOG.md
result "做完了。"
`,
	})
	out, code := runEval(t, home, tmp, "run", "901", "902", "--fake")
	if code != 1 {
		t.Fatalf("%d\n%s", code, out)
	}
	run := lastRun(t, home)
	if len(run.Scores) != 2 {
		t.Fatalf("%+v", run)
	}
	for _, s := range run.Scores {
		if s.Correct || s.Error != "" {
			t.Errorf("%+v", s)
		}
	}
	if !checkFailed(run.Scores[0], "没改 a.txt") || !checkFailed(run.Scores[0], "改动都过了门") {
		t.Errorf("%+v", run.Scores[0])
	}
	if !checkFailed(run.Scores[1], "条目状态") {
		t.Errorf("%+v", run.Scores[1])
	}
}

// 下次 run 清超过 7 天的现场，带标记的才清；新的不动。
func TestEvalCleansStaleSandboxes(t *testing.T) {
	home, tmp := evalHome(t)
	old := filepath.Join(tmp, "aq-old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(old, ".agentq-eval")
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(mark, at, at); err != nil {
		t.Fatal(err)
	}
	unmarked := filepath.Join(tmp, "aq-unmarked")
	if err := os.MkdirAll(unmarked, 0o700); err != nil {
		t.Fatal(err)
	}
	out, code := runEval(t, home, tmp, "run", "007")
	if code != 0 || !strings.Contains(out, "清掉了 1 个超过 7 天的旧现场") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("旧现场要清")
	}
	if _, err := os.Stat(unmarked); err != nil {
		t.Errorf("没标记的不是自家的：%v", err)
	}
}

// eval judge：评测门用的那一步。分组变差即使平均涨了也算变差；结果不进 runs/（不会被 eval baseline 当成最近一次）；没有基线只记录。
func TestEvalJudgeGroupWorseEvenIfAverageUp(t *testing.T) {
	home, tmp := evalHome(t)
	if out, code := runEval(t, home, tmp, "run", "--fake", "001", "002", "007"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := runEval(t, home, tmp, "baseline"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	// 基线里 backend 两题都没过：这次都过了，平均分从 33 涨到 67；install 那题变差，照样拒。
	path := filepath.Join(home, ".agentq", "evals", "baseline.json")
	var base evolve.Baseline
	if err := json.Unmarshal([]byte(testkit.ReadFile(t, path)), &base); err != nil {
		t.Fatal(err)
	}
	for i, s := range base.Scores {
		if s.Domain == "backend" {
			base.Scores[i].Points, base.Scores[i].Correct = 0, false
			for k := range s.Checks {
				base.Scores[i].Checks[k].Pass = false
			}
		}
	}
	raw, _ := json.Marshal(base)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 坏候选：setup --plan 失败，别的照真的走（程序题 007 判错，模型题的现场照常布置）。
	bad := filepath.Join(tmp, "bad-agentq")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\n[ \"$1 $2\" = 'setup --plan' ] && { echo 装不上 >&2; exit 3; }\nexec "+fsx.ShQuote(agentqBin)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(tmp, "verdict.json")
	out, code := runEval(t, home, tmp, "judge", "--bin", bad, "--out", verdictPath, "001", "002", "007")
	if code != 1 || !strings.Contains(out, "题目和判分取自运行中这一代") {
		t.Fatalf("%d %s", code, out)
	}
	var v gate.EvalVerdict
	if err := json.Unmarshal([]byte(testkit.ReadFile(t, verdictPath)), &v); err != nil {
		t.Fatal(err)
	}
	worse := strings.Join(v.Worse, "\n")
	if !v.Baseline || v.Compared != 3 || len(v.Better) < 2 || !strings.Contains(worse, "007-install-existing-skill：分数 100 → 0") || !strings.Contains(worse, "领域 install：平均分 100.0 → 0.0") {
		t.Fatalf("%+v\n%s", v, out)
	}
	if strings.Contains(worse, "backend") {
		t.Errorf("backend 变好了：%s", worse)
	}
	if runs, _ := filepath.Glob(filepath.Join(home, ".agentq", "evals", "runs", "*")); len(runs) != 1 {
		t.Errorf("judge 的结果不进 runs/：%v", runs)
	}

	// 没有基线：只记录，退出码 0，告诉怎么建。
	home2, tmp2 := evalHome(t)
	out, code = runEval(t, home2, tmp2, "judge", "--bin", bad, "--out", verdictPath, "007")
	if code != 0 || !strings.Contains(out, "没有能比的基线") {
		t.Fatalf("%d %s", code, out)
	}
	v = gate.EvalVerdict{}
	if err := json.Unmarshal([]byte(testkit.ReadFile(t, verdictPath)), &v); err != nil {
		t.Fatal(err)
	}
	if v.Baseline || len(v.Worse) != 0 || !strings.Contains(v.Hint, "agentq eval baseline") || len(v.Failed) != 1 {
		t.Fatalf("%+v", v)
	}
	if out, code := runEval(t, home2, tmp2, "judge", "007"); code == 0 || !strings.Contains(out, "--bin") {
		t.Errorf("要写 --bin：%d %s", code, out)
	}
}
