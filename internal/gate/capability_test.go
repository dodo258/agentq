package gate_test

import (
	"os"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/testkit"
)

const capJSON = `{"capabilities":[{"name":"web-perf","domain":"frontend","knowledge":["rules/web-perf.md"],"cases":["030-web-perf"],"when":{"changed":["*.tsx"]},"added":"2026-11-11"}]}`

func capReady() gate.Ready {
	r := ready("agent/x")
	r.Type = "capability"
	return r
}

// 验收：type 是 capability 的交付，没在能力清单登记新能力、或没新加该领域的考题，评测门直接退回，不评判。
func TestEvalGateCapabilityNeedsRegistryAndCase(t *testing.T) {
	f := newFix(t, fixBacklog)
	calls := f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
	j := f.claim("做事", "agent/x", map[string]string{"rules/web-perf.md": "懒加载\n"}, capReady())
	res := f.run(j)
	if r := evalResult(t, res); res.Outcome != "fail" || r.Outcome != gate.Fail || !strings.Contains(r.Summary, "没有新登记的能力") {
		t.Fatalf("没登记：%+v", r)
	}

	f = newFix(t, fixBacklog)
	calls = f.selfRepo(&gate.EvalVerdict{Cases: 8, Baseline: true, Compared: 8})
	j = f.claim("做事", "agent/x", map[string]string{gate.CapabilitiesFile: capJSON, "rules/web-perf.md": "懒加载\n",
		"evals/030-api-load/case.json": `{"domain":"backend","kind":"program"}`, "evals/030-api-load/check.sh": "exit 1\n"}, capReady())
	res = f.run(j)
	r := evalResult(t, res)
	if res.Outcome != "fail" || r.Outcome != gate.Fail || !strings.Contains(r.Summary, "新能力没新加该领域的考题：web-perf（frontend）") || len(r.Files) == 0 || r.Files[0] != "evals/" {
		t.Fatalf("考题领域不对：%+v", r)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("交付不全不用评判")
	}
}

// 验收：登记了、带了同领域的考题，交给运行中这一代评判（新题、领域都传过去）；它说领域没涨或别的领域掉了就退回，涨了才合并。
func TestEvalGateCapabilityJudged(t *testing.T) {
	files := map[string]string{gate.CapabilitiesFile: capJSON, "rules/web-perf.md": "懒加载\n",
		"evals/030-web-perf/case.json": `{"domain":"frontend","kind":"program"}`, "evals/030-web-perf/check.sh": "exit 1\n"}
	for _, worse := range []string{"领域 前端（frontend）：加上 1 道新题后通过 1/2 → 1/2，平均分 50.0 → 50.0，没涨", "领域 backend：平均分 100.0 → 50.0"} {
		f := newFix(t, fixBacklog)
		f.selfRepo(&gate.EvalVerdict{Cases: 9, Baseline: true, Compared: 8, Worse: []string{worse}})
		j := f.claim("做事", "agent/x", files, capReady())
		res := f.run(j)
		if r := evalResult(t, res); res.Outcome != "fail" || r.Outcome != gate.Fail || !strings.Contains(r.Summary, worse) {
			t.Fatalf("%s 要退回：%+v", worse, r)
		}
	}
	f := newFix(t, fixBacklog)
	calls := f.selfRepo(&gate.EvalVerdict{Cases: 9, Baseline: true, Compared: 8, Better: []string{"领域 前端（frontend）：加上 1 道新题后通过 1/2 → 2/2"}})
	j := f.claim("做事", "agent/x", files, capReady())
	res := f.run(j)
	if r := evalResult(t, res); res.Outcome != "merged" || r.Hold != "" {
		t.Fatalf("涨了要合并：%+v", res)
	}
	got := testkit.ReadFile(t, calls)
	if !strings.Contains(got, "/evals --new 030-web-perf --domain frontend") || !strings.Contains(got, " --new-dir ") {
		t.Fatalf("新题和领域要交给评判：%s", got)
	}
}

// 能力门：按 when 判断这次用不用；检查脚本取自运行中这一代，报了问题是 warn 档，照样合并，结果里记下哪项能力报的；用不上的不跑。
func TestCapabilityGate(t *testing.T) {
	gate.SetCapabilities(t, []gate.Capability{
		{Name: "web-lint", Domain: "frontend", Cases: []string{"030-x"}, Check: "web-lint.sh", When: gate.CapWhen{Changed: []string{"web/**"}}, Added: "2026-11-11"},
		{Name: "go-notes", Domain: "backend", Cases: []string{"031-x"}, Knowledge: []string{"rules/go.md"}, When: gate.CapWhen{Files: []string{"go.mod"}}, Added: "2026-11-11"},
	}, map[string]string{"web-lint.sh": "#!/bin/sh\n[ -n \"$AGENTQ_BASE\" ] || exit 9\necho 按钮没有 aria-label\nexit 1\n"})
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"web/a.tsx": "x\n"}, ready("agent/x"))
	res := f.run(j)
	r, ok := resultOf(res, "capability")
	if !ok || res.Outcome != "merged" || r.Outcome != gate.Fail || r.Level != gate.LevelWarn || !strings.Contains(r.Summary, "能力检查报出问题：web-lint") {
		t.Fatalf("%+v", res)
	}
	if len(r.Caps) != 1 || r.Caps[0].Name != "web-lint" || r.Caps[0].Outcome != gate.Fail || !strings.Contains(strings.Join(r.Evidence, "\n"), "cap-web-lint.log") {
		t.Fatalf("%+v", r)
	}

	f = newFix(t, fixBacklog)
	f.commitMain(map[string]string{"go.mod": "module demo\n"})
	j = f.claim("做事", "agent/x", map[string]string{"a.go": "package demo\n"}, ready("agent/x"))
	res = f.run(j)
	if r, ok := resultOf(res, "capability"); !ok || r.Outcome != gate.Pass || len(r.Caps) != 1 || r.Caps[0].Name != "go-notes" || !strings.Contains(r.Summary, "用上的能力：go-notes") {
		t.Fatalf("只有知识的能力也记用上了：%+v", res)
	}

	f = newFix(t, fixBacklog)
	j = f.claim("做事", "agent/x", map[string]string{"a.txt": "x\n"}, ready("agent/x"))
	if res := f.run(j); !strings.Contains(strings.Join(res.Notes, "；"), "capability 门不适用") {
		t.Fatalf("用不上不跑：%+v", res)
	}
}

// 清单格式：名字、领域、考题、启用条件、日期都要有；仓库里那份读得出来。
func TestParseCapabilities(t *testing.T) {
	if _, err := gate.Capabilities(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"没考题":  `{"capabilities":[{"name":"a","domain":"frontend","when":{"files":["x"]},"added":"2026-11-11"}]}`,
		"没条件":  `{"capabilities":[{"name":"a","domain":"frontend","cases":["030-x"],"added":"2026-11-11"}]}`,
		"日期":   `{"capabilities":[{"name":"a","domain":"frontend","cases":["030-x"],"when":{"files":["x"]},"added":"11-11"}]}`,
		"重名":   `{"capabilities":[{"name":"a","domain":"d","cases":["c"],"when":{"files":["x"]},"added":"2026-11-11"},{"name":"a","domain":"d","cases":["c"],"when":{"files":["x"]},"added":"2026-11-11"}]}`,
		"拼错字段": `{"capabilities":[{"name":"a","domian":"d","cases":["c"],"when":{"files":["x"]},"added":"2026-11-11"}]}`,
		"脚本路径": `{"capabilities":[{"name":"a","domain":"d","cases":["c"],"when":{"files":["x"]},"added":"2026-11-11","check":"../x.sh"}]}`,
	}
	for name, raw := range bad {
		if _, err := gate.ParseCapabilities([]byte(raw)); err == nil {
			t.Errorf("%s：要报错", name)
		}
	}
}
