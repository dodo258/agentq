package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// seedConfirmedBlocks 造假数据：demo 的两条各被测试门 error 档拦过一次，一条后来合并了，一条用户放弃了（门拦得对）。
func seedConfirmedBlocks(t *testing.T, e *testEnv) {
	t.Helper()
	data := filepath.Join(e.home, ".agentq")
	at := time.Now().Add(-time.Hour)
	put := func(path string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		os.Chtimes(path, at, at)
	}
	fail := gate.RunResult{Outcome: "fail", Head: "0123456789abcdef", Results: []gate.Result{{Gate: "test", Outcome: gate.Fail, Level: gate.LevelError, Summary: "go test ./internal/a 失败：TestAmount 第 12 行"}}}
	put(filepath.Join(data, "jobs", "x1", "meta.json"), Job{ID: "x1", Project: "demo", Fingerprint: "fx", Title: "旧活一", Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateReturned}})
	put(filepath.Join(data, "gate", "x1", "last.json"), fail)
	put(filepath.Join(data, "jobs", "x2", "meta.json"), Job{ID: "x2", Project: "demo", Fingerprint: "fx", Title: "旧活一", Status: state.StatusDone, ResumeOf: "x1", Gate: &state.GateRecord{State: state.GateMerged, Merge: "abcdef0123456789"}})
	fail.Results[0].Summary = "go test ./internal/b/c 失败：TestAmount 第 98 行"
	put(filepath.Join(data, "gate", "y1", "last.json"), fail)
	put(filepath.Join(data, "jobs", "y1", "meta.json"), Job{ID: "y1", Project: "demo", Fingerprint: "fy", Title: "旧活二", Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateAbandoned}})
}

func fakeCalls(t *testing.T, e *testEnv) int {
	t.Helper()
	entries, err := os.ReadDir(e.fakeLg)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// 门拦截确认过两次 → tick 收信号（不调模型）→ 下一条同项目任务的合同末尾带上约束和出处；dispatch 用的命令打印同样内容。
func TestIntegrationLearnedConstraints(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true})
	gateRepo(t, e, "demo", "- [ ] 新的活 NOMERGE")
	seedConfirmedBlocks(t, e)
	e.mustRun("tick")
	if n := fakeCalls(t, e); n != 0 {
		t.Fatalf("收信号不该调模型：%d 次", n)
	}
	out := e.mustRun("evolve", "constraints", "demo")
	for _, want := range []string{"这个项目之前被门拦过的事", "test 门：go test", "2 次", "任务 x1", "提交 abcdef012345", "任务 y1（用户放弃）"} {
		if !strings.Contains(out, want) {
			t.Errorf("evolve constraints 缺 %q：\n%s", want, out)
		}
	}
	if sig := e.mustRun("evolve", "signals"); !strings.Contains(sig, "gate-fail") || !strings.Contains(sig, "proposals.md") {
		t.Errorf("evolve signals：\n%s", sig)
	}
	e.mustRun("run", "demo")
	j := e.jobByTitle("新的活 NOMERGE")
	prompt, err := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), "这个项目之前被门拦过的事") || !strings.Contains(string(prompt), "test 门：go test") || !strings.Contains(string(prompt), "出处：任务 x1") {
		t.Errorf("合同末尾要带约束：%v\n%s", err, prompt)
	}
	waitExit(t, j.PID, 20*time.Second)
}

// learned_constraints_enabled 关着：不学、合同一字不变（和没有这个功能时一样）。
func TestIntegrationLearnedConstraintsOff(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true, "learned_constraints_enabled": false})
	gateRepo(t, e, "demo", "- [ ] 新的活 NOMERGE")
	seedConfirmedBlocks(t, e)
	e.mustRun("tick")
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "evolve", "constraints", "demo.json")); err == nil {
		t.Error("关着不该学")
	}
	e.mustRun("run", "demo")
	j := e.jobByTitle("新的活 NOMERGE")
	prompt, err := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if err != nil || strings.Contains(string(prompt), "被门拦过") {
		t.Errorf("关着合同不带：%v\n%s", err, prompt)
	}
	waitExit(t, j.PID, 20*time.Second)
}
