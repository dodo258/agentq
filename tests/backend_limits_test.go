package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 混用：默认 Claude、cx 项目用 Codex。
func mixedClaudeDefault(t *testing.T, extra map[string]any) *testEnv {
	t.Helper()
	cfg := map[string]any{"max_parallel": 5, "project_backends": map[string]any{"cx": "codex"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t), "permission": "danger-full-access"}}}
	for k, v := range extra {
		cfg[k] = v
	}
	return newEnv(t, cfg)
}

// 混用：默认 Codex、cl 项目用 Claude。
func mixedCodexDefault(t *testing.T, extra map[string]any) *testEnv {
	t.Helper()
	cfg := map[string]any{"backend": "codex", "permission_mode": "danger-full-access", "max_parallel": 5, "project_backends": map[string]any{"cl": "claude"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}, "claude": map[string]any{"permission": "bypassPermissions"}}}
	for k, v := range extra {
		cfg[k] = v
	}
	return newEnv(t, cfg)
}

func projectJob(e *testEnv, project string) *Job {
	for _, j := range e.state().Jobs {
		if j.Project == project {
			return j
		}
	}
	return nil
}

// Codex 撞限只停 Codex 的项目：Claude 的项目照常接活，冷却记在 backend_limits，全局那个键不动。
func TestLimitOnOtherBackendDoesNotStopDefault(t *testing.T) {
	e := mixedClaudeDefault(t, nil)
	gateRepo(t, e, "cx", codexItem("aaa", "limitmid"))
	e.mustRun("tick")
	j := e.tickUntilEnded(func() *Job { return projectJob(e, "cx") })
	if !j.LimitHit {
		t.Fatalf("应撞限：%s %s", j.Status, j.Summary)
	}
	st := e.state()
	if !st.CooldownUntil.IsZero() {
		t.Fatalf("默认后端不该冷却：%v", st.CooldownUntil)
	}
	l := st.BackendLimits["codex"]
	if l == nil || !l.CooldownUntil.After(time.Now()) {
		t.Fatalf("Codex 的冷却没记下：%+v", l)
	}
	testkit.MkProject(t, e.root, "zz", "- [ ] 甲一 —— 正常\n")
	out := e.mustRun("tick")
	if projectJob(e, "zz") == nil {
		t.Fatalf("Claude 的项目应照常接活：\n%s", out)
	}
	testkit.AssertContains(t, out, "后端 codex 用量冷却中")
	for _, x := range e.state().Jobs {
		if x.Project == "cx" && x.ID != j.ID {
			t.Fatalf("冷却中不该续跑 cx：%s", x.ID)
		}
	}
	testkit.AssertContains(t, e.mustRun("status"), "后端 codex 用量冷却中")
}

// 反过来：默认 Codex，Claude 项目撞限也只停 Claude。没写后端的任务记录是 Claude，不能记到默认后端头上。
func TestLimitOnClaudeProjectDoesNotStopCodexDefault(t *testing.T) {
	e := mixedCodexDefault(t, nil)
	testkit.MkProject(t, e.root, "cl", "- [ ] 撞上限 —— LIMIT\n")
	e.mustRun("tick")
	j := e.tickUntilEnded(func() *Job { return projectJob(e, "cl") })
	if !j.LimitHit {
		t.Fatalf("应撞限：%s %s", j.Status, j.Summary)
	}
	st := e.state()
	if !st.CooldownUntil.IsZero() || st.BackendLimits["claude"] == nil || !st.BackendLimits["claude"].CooldownUntil.After(time.Now()) {
		t.Fatalf("冷却记错后端：%v %+v", st.CooldownUntil, st.BackendLimits)
	}
	gateRepo(t, e, "cx", codexItem("bbb", "slow"))
	out := e.mustRun("tick")
	if projectJob(e, "cx") == nil {
		t.Fatalf("Codex 的项目应照常接活：\n%s", out)
	}
}

// 默认 Codex 时也读 Claude 项目的额度：到门槛的不领活，Codex 的项目不受影响。之前额度门只看默认后端。
func TestUsageGatePerBackend(t *testing.T) {
	e := mixedCodexDefault(t, nil)
	e.setUsage(usageLine("99"))
	testkit.MkProject(t, e.root, "cl", "- [ ] 甲 —— 正常\n")
	gateRepo(t, e, "cx", codexItem("ccc", "slow"))
	out := e.mustRun("tick")
	if projectJob(e, "cl") != nil {
		t.Fatalf("Claude 额度 99%% 不该领活：\n%s", out)
	}
	if projectJob(e, "cx") == nil {
		t.Fatalf("Codex 不受 Claude 额度影响：\n%s", out)
	}
	testkit.AssertContains(t, out, "后端 claude：")
	st := e.state()
	if st.BackendLimits["claude"] == nil || st.BackendLimits["claude"].Usage == nil {
		t.Fatalf("Claude 的额度该记在 backend_limits：%+v", st.BackendLimits)
	}
	if st.Usage == nil || st.Usage.Err == "" {
		t.Fatalf("默认后端 Codex 读不了额度，记未知：%+v", st.Usage)
	}
	testkit.AssertContains(t, e.mustRun("status"), "额度（claude）：")
	if out, code := e.run("run", "cl"); code == 0 || !strings.Contains(out, "额度") {
		t.Fatalf("手动 run 也按 Claude 的额度拒绝：%d %s", code, out)
	}
}

// 老 state.json 只有 cooldown_until：读出来是默认后端的。别的后端的项目照常接活。
func TestOldStateCooldownBelongsToDefault(t *testing.T) {
	e := mixedClaudeDefault(t, nil)
	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	old := `{"jobs":[],"cooldown_until":"` + until + `","breaker_until":"0001-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "state.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.MkProject(t, e.root, "aa", "- [ ] 甲 —— 正常\n")
	gateRepo(t, e, "cx", codexItem("ddd", "slow"))
	out := e.mustRun("tick")
	if projectJob(e, "aa") != nil {
		t.Fatalf("默认后端冷却中不该接活：\n%s", out)
	}
	if projectJob(e, "cx") == nil {
		t.Fatalf("Codex 不受默认后端冷却影响：\n%s", out)
	}
	testkit.AssertContains(t, out, "后端 claude 用量冷却中")
	if !e.state().CooldownOf("codex", "").IsZero() {
		t.Fatal("老文件的冷却不算到 Codex 头上")
	}
}

// 写和读回：空的时候省略，有的时候认得；都在冷却时 tick 和只用一种后端时一样什么都不起。
func TestBackendLimitsRoundTrip(t *testing.T) {
	b, err := json.Marshal(&state.State{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "backend_limits") {
		t.Fatalf("空的应省略：%s", b)
	}
	st := &state.State{}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	st.ExtendCooldown("codex", "", until)
	st.ExtendCooldown("codex", "", until.Add(-time.Minute)) // 只往后推
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back state.State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.CooldownOf("codex", "").Equal(until) || !back.CooldownOf("", "").IsZero() || !back.CooldownOf("claude", "codex").IsZero() {
		t.Fatalf("读回不对：%s", b)
	}
	if !back.CooldownOf("codex", "codex").IsZero() {
		t.Fatalf("默认后端的冷却看 cooldown_until：%s", b)
	}
}

// 全都冷却时不起任务；时钟跳回来后别的后端的冷却也要收拾。
func TestAllBackendsCoolingAndClamp(t *testing.T) {
	e := mixedClaudeDefault(t, nil)
	far := time.Now().Add(365 * 24 * time.Hour)
	st := e.state()
	st.CooldownUntil = time.Now().Add(time.Hour)
	st.ExtendCooldown("codex", "", far)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	testkit.MkProject(t, e.root, "aa", "- [ ] 甲 —— 正常\n")
	gateRepo(t, e, "cx", codexItem("eee", "slow"))
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "用量冷却")
	if projectJob(e, "aa") != nil || projectJob(e, "cx") != nil {
		t.Fatalf("都在冷却不该起任务：\n%s", out)
	}
	if got := e.state().BackendLimits["codex"].CooldownUntil; got.After(far.Add(-time.Hour)) {
		t.Fatalf("超过上限的冷却应收拾：%v", got)
	}
}

// 默认后端冷却时，别的后端的任务照样过门合并：门用任务的后端。
func TestGateNotHeldByOtherBackendCooldown(t *testing.T) {
	e := mixedClaudeDefault(t, nil)
	gateRepo(t, e, "cx", codexItem("fff", "ok"))
	e.mustRun("tick")
	st := e.state()
	st.CooldownUntil = time.Now().Add(time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	j := e.tickUntilGate("条目 ITEM-fff")
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("应合并：%+v %s", j.Gate, j.Summary)
	}
}

// 只用一种后端：不会多读别的后端的额度，也不写新键。
func TestSingleBackendNoOtherUsage(t *testing.T) {
	e := newEnv(t, nil)
	e.setUsage(usageLine("10"))
	testkit.MkProject(t, e.root, "aa", "- [ ] 甲 —— 正常\n")
	e.mustRun("tick")
	b := testkit.ReadBytes(t, filepath.Join(e.home, ".agentq", "state.json"))
	if strings.Contains(string(b), "backend_limits") {
		t.Fatalf("只用一种后端不写这个键：\n%s", b)
	}
	if out := e.mustRun("status"); strings.Contains(out, "额度（") {
		t.Fatalf("只有一个后端不多一行：\n%s", out)
	}
}
