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

// 用户亲手 stop、门卡片上退回、验收后返工：tick 只读本机数据收成信号（不调模型），agentq evolve signals 看得到种类、领域和证据。
func TestIntegrationUserSignals(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true})
	gateRepo(t, e, "demo", "- [ ] 甲 —— SLEEP=120", "- [ ] 验收 首页改版 的意见 —— 按钮错位（范围：web/）")
	e.mustRun("run", "demo", "甲")
	j := e.jobByTitle("甲")
	e.mustRun("stop", j.ID)
	waitExit(t, j.PID, 20*time.Second)
	audit := filepath.Join(e.home, ".agentq", "gate", "audit.log")
	os.MkdirAll(filepath.Dir(audit), 0o700)
	if err := os.WriteFile(audit, []byte(time.Now().Add(-time.Hour).Format(time.RFC3339)+" 退回 g1 demo「旧活」分支 agent/g1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := fakeCalls(t, e)
	e.mustRun("tick")
	if n := fakeCalls(t, e); n != calls {
		t.Fatalf("收信号不该调模型：%d → %d", calls, n)
	}
	sig := e.mustRun("evolve", "signals")
	for _, want := range []string{"user-stop", "证据：任务 " + j.ID + "（demo）「甲」", "user-decide", "退回了「旧活」", "rework", "前端返工", "项目 demo「验收 首页改版 的意见」"} {
		if !strings.Contains(sig, want) {
			t.Errorf("evolve signals 缺 %q：\n%s", want, sig)
		}
	}
	raw, err := os.ReadFile(filepath.Join(e.home, ".agentq", "evolve", "signals.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if again, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "evolve", "signals.json")); string(again) != string(raw) {
		t.Error("没有新数据，信号文件不该变")
	}
}

// 验收（E7）：前端条目反复被门拦，tick 收信号（不调模型）报出「能力缺口：前端」，带任务号和门的结论；后端正常不报；
// agentq evolve signals 看得到按领域的账和缺口。同一领域只写一次见 internal/evolve 的 TestCapabilityGapToTodoOnce。
func TestIntegrationCapabilityGap(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true})
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
	end := time.Now().Add(-2 * time.Hour)
	job := func(id, scope, file string) {
		j := Job{ID: id, Project: "demo", Fingerprint: "fp-" + id, Title: "活 " + id, Item: "活 " + id + " —— 做（范围：" + scope + "）", Status: state.StatusDone, EndedAt: &end}
		if file != "" {
			j.Status, j.Gate = state.StatusInterrupted, &state.GateRecord{State: state.GateReturned}
			put(filepath.Join(data, "gate", id, "last.json"), gate.RunResult{Outcome: "fail", Results: []gate.Result{{Gate: "review", Outcome: gate.Fail, Level: gate.LevelError, Summary: "审查：" + file + " 状态没清", Files: []string{file}}}})
		}
		put(filepath.Join(data, "jobs", id, "meta.json"), j)
	}
	job("w1", "web/", "web/App.tsx")
	job("w2", "web/", "web/Nav.tsx")
	job("w3", "web/", "web/Form.tsx")
	job("w4", "web/", "")
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		job(id, "server/", "")
	}
	e.mustRun("tick")
	if n := fakeCalls(t, e); n != 0 {
		t.Fatalf("收信号不该调模型：%d 次", n)
	}
	sig := e.mustRun("evolve", "signals")
	for _, want := range []string{"capability-gap", "前端（frontend）", "缺口，已写「能力缺口：前端」", "后端（backend）", "任务 w1（demo）review 门没过"} {
		if !strings.Contains(sig, want) {
			t.Errorf("evolve signals 缺 %q：\n%s", want, sig)
		}
	}
	prop, err := os.ReadFile(filepath.Join(data, "evolve", "proposals.md"))
	if err != nil || strings.Count(string(prop), "能力缺口：前端") != 1 || strings.Contains(string(prop), "能力缺口：后端") {
		t.Fatalf("只写前端一条：%v\n%s", err, prop)
	}
}
