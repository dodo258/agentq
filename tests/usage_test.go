package tests

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// 缓存 10 分钟；读完之后有窗口过了重置时刻就提前重读；每天第一次读到、档位变了才写当天汇总。
func TestUsageCacheAndNightly(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	text, calls := "", 0
	var readErr error
	a := wireApp(&App{Env: &env.Env{Home: home, Cfg: config.Default(), Now: func() time.Time { return now }, Out: io.Discard}, Runner: &runner.Service{UsageText: func() (string, error) { calls++; return text, readErr }}})
	st := &State{}
	nightly := func() string {
		b, _ := os.ReadFile(a.NightlyPath(now))
		return string(b)
	}

	text = "Current session: 50% used · resets 2pm\nCurrent week (all models): 95% used · resets 12:30pm\n"
	u := a.Runner.Usage(st)
	if calls != 1 || st.Usage != u || u.Err != "" || len(u.Windows) != 2 {
		t.Fatalf("第一次应读：calls=%d %+v", calls, u)
	}
	if n := nightly(); strings.Count(n, "额度：") != 1 || !strings.Contains(n, "本周 95%（10-11 周日 12:30 重置）") ||
		!strings.Contains(n, "不启动新任务（正在跑的不动）") {
		t.Errorf("挡住时应写汇总：\n%s", n)
	}

	now = now.Add(9 * time.Minute)
	a.Runner.Usage(st)
	if calls != 1 {
		t.Errorf("10 分钟内应用缓存：calls=%d", calls)
	}

	// 12:30 重置、12:31 起算恢复：缓存还没到 10 分钟也重读；新读数正常 → 汇总写「已恢复」
	now = time.Date(2026, 10, 11, 12, 31, 0, 0, time.Local)
	text = "Current session: 50% used · resets 2pm\nCurrent week (all models): 1% used · resets Oct 18 at 12:30pm\n"
	if !st.Usage.Stale(now) {
		t.Fatal("过了重置时刻应算过期")
	}
	u = a.Runner.Usage(st)
	if calls != 2 {
		t.Errorf("过了重置时刻应重读：calls=%d", calls)
	}
	if lvl, _, _ := backend.UsageLevel(u, now, "opus"); lvl != backend.UsageOK {
		t.Errorf("重读后应正常：%d", lvl)
	}
	if n := nightly(); strings.Count(n, "额度：") != 2 || !strings.Contains(n, "已恢复，照常接活") {
		t.Errorf("恢复时应写汇总：\n%s", n)
	}

	// 档位没变：只更新缓存，不写汇总
	now = now.Add(10 * time.Minute)
	a.Runner.Usage(st)
	if calls != 3 || strings.Count(nightly(), "额度：") != 2 {
		t.Errorf("档位没变不该写汇总：calls=%d\n%s", calls, nightly())
	}

	// 读失败：当未知，记原因，写一行
	now = now.Add(10 * time.Minute)
	readErr = errors.New("执行 claude -p /usage 失败：exit status 1")
	u = a.Runner.Usage(st)
	if lvl, _, _ := backend.UsageLevel(u, now, "opus"); lvl != backend.UsageUnknown || !strings.Contains(u.Err, "exit status 1") {
		t.Errorf("读失败应当未知：%+v", u)
	}
	if n := nightly(); strings.Count(n, "额度：") != 3 || !strings.Contains(n, "未知（执行 claude -p /usage 失败") {
		t.Errorf("变成未知时应写汇总：\n%s", n)
	}
	if !strings.Contains(backend.UsageDescribe(u, now, "opus"), "照常接活") {
		t.Errorf("describe：%s", backend.UsageDescribe(u, now, "opus"))
	}

	// 时钟往回拨也重读
	now = now.Add(-time.Hour)
	readErr = nil
	a.Runner.Usage(st)
	if calls != 5 {
		t.Errorf("时钟往回拨应重读：calls=%d", calls)
	}
}

// 写一份假 claude 的 /usage 输出（JSON 结果，result 是给定文字）。
func (e *testEnv) setUsage(text string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "local_command": "usage"})
	if err := os.WriteFile(filepath.Join(e.home, "fake-usage.json"), append(b, '\n'), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) usageCalls() []string {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake-usage-calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// 让 state.json 里的额度读数过期，下次启动前重读。
func (e *testEnv) expireUsage() {
	e.t.Helper()
	st := e.state()
	st.Usage.CheckedAt = st.Usage.CheckedAt.Add(-time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		e.t.Fatal(err)
	}
}

// weekReset：假 /usage 里周窗口的重置时刻（两天后的整分钟，UTC 写法）。
var weekReset = time.Now().Add(48 * time.Hour).Truncate(time.Minute)

func usageLine(pct string) string {
	reset := time.Now().Add(3 * time.Hour).UTC().Format("3:04pm")
	week := weekReset.UTC().Format("Jan 2 at 3:04pm")
	return "You are currently using your subscription to power your Claude Code usage\n\n" +
		"Current session: " + pct + "% used · resets " + reset + " (UTC)\n" +
		"Current week (all models): 10% used · resets " + week + " (UTC)\n" +
		"Current week (Fable): 100% used · resets " + week + " (UTC)\n"
}

// 89% 照常启动；涨到 90% 后 tick 不再启动新任务（正在跑的不动），run 仍可用；98% run 也拒绝。
func TestIntegrationUsageGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"max_parallel": 5, "per_project": 3})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=20\n- [ ] 甲二 —— SLEEP=20\n- [ ] 甲三 —— SLEEP=20\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙一 —— SLEEP=20\n")

	e.setUsage(usageLine("89"))
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 2") {
		t.Fatalf("89%% 应照常启动（只限 Fable 的窗口 100%% 不管 opus 任务）：\n%s", out)
	}
	calls := e.usageCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "--no-session-persistence") || !strings.Contains(calls[0], "--setting-sources") ||
		!strings.Contains(calls[0], "PWD="+testkit.RealPath(t, os.TempDir())) || !strings.Contains(calls[0], "agentq-usage-") {
		t.Errorf("一次 tick 只读一次额度，在临时目录里、不读设置、不存会话：%q", calls)
	}
	st := e.state()
	if st.Usage == nil || len(st.Usage.Windows) != 3 || st.Usage.Windows[0].Used != 89 {
		t.Errorf("额度应写进 state：%+v", st.Usage)
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 89%") || !strings.Contains(n, "本周（Fable） 100%") || !strings.Contains(n, "不计入") {
		t.Errorf("当天第一次读到应写汇总：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") || !strings.Contains(out, "额度：会话 89%") ||
		!strings.Contains(out, "本周 10%（"+textfmt.WhenCN(weekReset)+" 重置）") {
		t.Errorf("status 应显示各窗口用量和重置时间：\n%s", out)
	}
	if len(e.usageCalls()) != 1 {
		t.Errorf("缓存没过期时 status 不重读：%q", e.usageCalls())
	}
	// 涨到 90%：不再启动新任务（甲二本来可以接），正在跑的不动；run 仍可用
	e.setUsage(usageLine("90"))
	e.expireUsage()
	out = e.mustRun("tick")
	if !strings.Contains(out, "启动 0") || !strings.Contains(out, "订阅额度快用完（会话 90%，≥90%）") || !strings.Contains(out, "重置后自动恢复") {
		t.Fatalf("90%% 不应启动：\n%s", out)
	}
	if n := countRunning(e.state()); n != 2 {
		t.Errorf("正在跑的不动：运行中 %d", n)
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 90%") || !strings.Contains(n, "不启动新任务（正在跑的不动）") {
		t.Errorf("档位变了应写汇总：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "调度：订阅额度快用完") {
		t.Errorf("status 调度行应标出：\n%s", out)
	}
	out = e.mustRun("run", "alpha")
	if !strings.Contains(out, "已启动任务") || !strings.Contains(out, "agentq run 仍可用") {
		t.Errorf("90%% 时 run 仍可用并提示：\n%s", out)
	}

	// 98%：run 也拒绝
	e.setUsage(usageLine("98"))
	e.expireUsage()
	before := len(e.state().Jobs)
	out, code := e.run("run", "alpha")
	if code == 0 || !strings.Contains(out, "agentq run 也拒绝") {
		t.Errorf("98%% 时 run 应拒绝：%d\n%s", code, out)
	}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "≥98%") {
		t.Errorf("98%% 时 tick 不启动：\n%s", out)
	}
	if n := len(e.state().Jobs); n != before {
		t.Errorf("98%% 时不应有新任务：%d → %d", before, n)
	}
	for _, j := range e.state().Running() {
		e.mustRun("stop", j.ID)
	}
}

// 中断的任务等着续跑时额度涨到 ≥90%：续跑也算新任务，不启动；中断的那条仍等着续跑。
func TestIntegrationUsageBlocksResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n")
	e.setUsage(usageLine("50"))
	e.mustRun("tick")
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	e.setUsage(usageLine("92"))
	e.expireUsage()
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1，启动 0") || !strings.Contains(out, "订阅额度快用完（会话 92%") {
		t.Fatalf("额度 ≥90%% 时不续跑：\n%s", out)
	}
	st := e.state()
	if len(st.Jobs) != 1 || st.Jobs[0].Status != state.StatusInterrupted || !st.Jobs[0].ResumePending {
		t.Errorf("中断的任务应仍等着续跑：%+v", st.Jobs[0])
	}
}

// 读数挡着、但读完之后过了重置时刻：缓存没到 10 分钟也重读，照常启动；没过重置时刻就用缓存、不启动。
func TestIntegrationUsageResetRecovers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n")
	statePath := filepath.Join(e.home, ".agentq", "state.json")
	now := time.Now()
	future, past := now.Add(5*time.Minute), now.Add(-2*time.Minute)
	seed := func(reset time.Time) {
		st := &State{Usage: &backend.UsageInfo{CheckedAt: now.Add(-5 * time.Minute), NotedLevel: backend.UsageRefuse, NotedDay: now.Format("2006-01-02"),
			Windows: []backend.UsageWindow{{Label: "session", Used: 99, ResetsAt: &reset}}}}
		if err := state.Save(statePath, st); err != nil {
			t.Fatal(err)
		}
	}
	e.setUsage(usageLine("3"))

	seed(future)
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(out, "会话 99%") {
		t.Fatalf("重置前用缓存、不启动：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.home, "fake-usage-calls")); err == nil {
		t.Errorf("缓存没过期、没到重置时刻，不应重读：%q", e.usageCalls())
	}

	seed(past)
	out := e.mustRun("tick")
	if !strings.Contains(out, "启动 1") {
		t.Fatalf("过了重置时刻应自动恢复：\n%s", out)
	}
	if len(e.usageCalls()) != 1 {
		t.Errorf("过了重置时刻应重读一次：%q", e.usageCalls())
	}
	if n := e.nightly(); !strings.Contains(n, "额度：会话 3%") || !strings.Contains(n, "已恢复，照常接活") {
		t.Errorf("恢复应写汇总：\n%s", n)
	}
	waitExit(t, e.jobByTitle("甲一").PID, 10*time.Second)
}

// /usage 输出读不懂、报错、不是 JSON：当未知照常跑，status 标出。
func TestIntegrationUsageUnknown(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"读不懂":     `{"type":"result","is_error":false,"result":"Usage is temporarily unavailable."}`,
		"报错":      `{"type":"result","is_error":true,"result":"API Error: 500"}`,
		"不是 JSON": `Error: unexpected`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— SLEEP=1\n")
			if err := os.WriteFile(filepath.Join(e.home, "fake-usage.json"), []byte(raw+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out := e.mustRun("tick")
			if !strings.Contains(out, "启动 1") || !strings.Contains(out, "额度未知，照常启动") {
				t.Fatalf("读不懂应照常启动：\n%s", out)
			}
			st := e.state()
			if st.Usage == nil || st.Usage.Err == "" {
				t.Errorf("state 应记下读失败：%+v", st.Usage)
			}
			if out := e.mustRun("status"); !strings.Contains(out, "调度：正常") || !strings.Contains(out, "额度：未知（") {
				t.Errorf("status 应标出未知：\n%s", out)
			}
			if n := e.nightly(); !strings.Contains(n, "额度：未知（") {
				t.Errorf("汇总应记下未知：\n%s", n)
			}
			waitExit(t, e.jobByTitle("甲一").PID, 10*time.Second)
		})
	}
}

// 夜班定点叫醒时额度 ≥90% 不启动、汇总记一行；手动执行到 98% 才拒绝。
func TestIntegrationUsageRoutine(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true})
	e.setUsage(usageLine("95"))
	out, code := e.run("night", "--scheduled")
	if code == 0 || !strings.Contains(out, "夜班没有启动：订阅额度快用完") {
		t.Errorf("95%% 时定点夜班不应启动：%d\n%s", code, out)
	}
	if n := e.nightly(); !strings.Contains(n, "夜班没有启动：订阅额度快用完") {
		t.Errorf("汇总应记一行：\n%s", n)
	}
	e.setUsage(usageLine("98"))
	e.expireUsage()
	if out, code := e.run("night"); code == 0 || !strings.Contains(out, "agentq run 也拒绝") {
		t.Errorf("98%% 时手动夜班也拒绝：%d\n%s", code, out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
}
