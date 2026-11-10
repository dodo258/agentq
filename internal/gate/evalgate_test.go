package gate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/testkit"
)

// selfRepo 把 demo 变成 agentq 自己的仓库（基点上 go.mod 是 module agentq，有 evals/ 和 internal/evolve/），
// 运行中的 agentq 换成一个脚本：核对参数和候选二进制，把 verdict 原样写成结论。返回记参数的文件。
func (f *fix) selfRepo(v *gate.EvalVerdict) string {
	f.t.Helper()
	f.commitMain(map[string]string{
		"go.mod":                    "module agentq\n\ngo 1.21\n",
		"main.go":                   "package main\n\nfunc main() {}\n",
		"evals/README.md":           "题\n",
		"internal/evolve/README.md": "判分\n",
	})
	tmp := f.t.TempDir()
	calls := filepath.Join(tmp, "calls")
	body := "#!/bin/sh\necho \"$*\" >> " + fsx.ShQuote(calls) + "\n" +
		"[ \"$1 $2 $3 $5\" = 'eval judge --bin --out' ] || exit 7\n" +
		"[ -x \"$4\" ] || { echo 候选没建出来; exit 8; }\n"
	if v != nil {
		raw, _ := json.Marshal(v)
		src := filepath.Join(tmp, "verdict.json")
		os.WriteFile(src, raw, 0o600)
		body += "cp " + fsx.ShQuote(src) + " \"$6\"\n"
		if len(v.Worse) > 0 {
			body += "exit 1\n"
		}
	}
	self := filepath.Join(tmp, "agentq")
	if err := os.WriteFile(self, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.a.SelfPath = self
	return calls
}

func evalResult(t *testing.T, res gate.RunResult) gate.Result {
	t.Helper()
	r, ok := resultOf(res, "eval")
	if !ok {
		t.Fatalf("没跑评测门：%+v", res)
	}
	return r
}

// 别的项目不跑评测门，照原样合并。
func TestEvalGateOnlySelfRepo(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := filepath.Join(t.TempDir(), "calls")
	f.a.SelfPath = filepath.Join(t.TempDir(), "never")
	j := f.claim("做事", "agent/x", map[string]string{"evals/a.txt": "a", "go.mod": "module agentq\n"}, ready("agent/x"))
	res := f.run(j)
	if _, ok := resultOf(res, "eval"); ok || res.Outcome != "merged" {
		t.Fatalf("不是 agentq 的仓库（任务分支上改成 agentq 也不算）：%+v", res)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("不该评判")
	}
}

// 变差：退回，理由写清哪题哪领域，写明耗时；用运行中的那个 agentq 评判建出来的候选。
func TestEvalGateWorseFails(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8, Failed: []string{"007-install-existing-skill"},
		Worse: []string{"007-install-existing-skill：分数 100 → 0", "领域 install：平均分 100.0 → 50.0"}, Better: []string{"001-impl-go：分数 0 → 100"}})
	j := f.claim("做事", "agent/x", map[string]string{"internal/setup/a.go": "package setup\n"}, ready("agent/x"))
	res := f.run(j)
	r := evalResult(t, res)
	if res.Outcome != "fail" || r.Outcome != gate.Fail {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"评测比基线变差 2 处", "007-install-existing-skill：分数 100 → 0", "领域 install", "耗时 "} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("理由缺 %s：%s", want, r.Summary)
		}
	}
	if r.Hold != "" {
		t.Errorf("没改尺子：%s", r.Hold)
	}
	got := testkit.ReadFile(t, calls)
	if !strings.Contains(got, "eval judge --bin "+filepath.Join(f.a.Dir(j.ID), "eval", "agentq")) {
		t.Errorf("参数：%s", got)
	}
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), "eval", "agentq")); err == nil {
		t.Error("候选二进制用完要删")
	}
}

// 没有能比的基线：只记录不挡，提示怎么建基线。
func TestEvalGateNoBaselineRecords(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.selfRepo(&gate.EvalVerdict{Cases: 8, Hint: "还没有基线：先 agentq eval run --fake，再 agentq eval baseline"})
	j := f.claim("做事", "agent/x", map[string]string{"internal/setup/a.go": "package setup\n"}, ready("agent/x"))
	res := f.run(j)
	r := evalResult(t, res)
	if res.Outcome != "merged" || r.Outcome != gate.Pass || !strings.Contains(r.Summary, "只记录不挡") || !strings.Contains(r.Summary, "agentq eval baseline") || !strings.Contains(r.Summary, "耗时 ") {
		t.Fatalf("%+v", res)
	}
}

// 改了评测题、判分、门的判定或 .agentq/：评测没变差也不自动合并，等用户拍板；放行之后照合。测试文件不算尺子。
func TestEvalGateJudgeChangesHold(t *testing.T) {
	for _, file := range []string{"evals/001-impl-go/case.json", "internal/evolve/score.go", "internal/gate/testgate.go", ".agentq/gates.json", "assets.go"} {
		t.Run(file, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
			body := "x\n"
			if strings.HasSuffix(file, ".go") {
				body = "package " + filepath.Base(filepath.Dir(file)) + "\n"
				if file == "assets.go" {
					body = "package main\n"
				}
			}
			if strings.HasPrefix(file, ".agentq/") { // 测试门对 .agentq/ 要门说明，这里只看评测门：测试门关掉
				body = `{"levels":{"review":"off","test":"off","eval":"warn"}}`
				f.commitMain(map[string]string{file: `{"levels":{"review":"off","test":"off"}}`})
			}
			j := f.claim("做事", "agent/x", map[string]string{file: body}, ready("agent/x"))
			res := f.run(j)
			r := evalResult(t, res)
			if res.Outcome != "hold" || r.Outcome != gate.Pass || !strings.Contains(r.Hold, file) || !strings.Contains(res.Reason, "评判的尺子") {
				t.Fatalf("%+v", res)
			}
			f.collect(j, res)
			if j.Gate.State != "stuck" || !strings.Contains(j.StuckPending, "等你拍板") {
				t.Fatalf("%+v %s", j.Gate, j.StuckPending)
			}
		})
	}
	f := newFix(t, fixBacklog)
	f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
	j := f.claim("做事", "agent/x", map[string]string{"internal/gate/x_test.go": "package gate\n", "internal/evolve/signals.go": "package evolve\n"}, ready("agent/x"))
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("测试文件和信号收集不是尺子：%+v", res)
	}
}

// 候选编译不过：退回，带构建日志。
func TestEvalGateBuildFails(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
	j := f.claim("做事", "agent/x", map[string]string{"broken.go": "package main\n\nfunc main() {}\n"}, ready("agent/x"))
	res := f.run(j)
	r := evalResult(t, res)
	if res.Outcome != "fail" || !strings.Contains(r.Summary, "候选构建不了") || !strings.Contains(strings.Join(r.Evidence, "\n"), "eval-build.log") {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("没建出来不评判")
	}
}

// 评判没给出结论（运行中的 agentq 起不来、没写结论）：判不了，不放行。
func TestEvalGateNoVerdictUnknown(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.selfRepo(nil)
	j := f.claim("做事", "agent/x", map[string]string{"internal/setup/a.go": "package setup\n"}, ready("agent/x"))
	res := f.run(j)
	if r := evalResult(t, res); res.Outcome != "unknown" || r.Outcome != gate.Unknown || !strings.Contains(r.Summary, "评测没给出结论") {
		t.Fatalf("%+v", res)
	}
}
