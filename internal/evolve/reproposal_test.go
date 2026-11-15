package evolve

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// fileLineOf 是 proposals.md 里含 want 的那一行。
func fileLineOf(t *testing.T, r *sigRig, want string) string {
	t.Helper()
	for _, l := range strings.Split(testkit.ReadFile(t, r.s.ProposalsPath()), "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	t.Fatalf("proposals.md 里没有 %q：\n%s", want, testkit.ReadFile(t, r.s.ProposalsPath()))
	return ""
}

// 没配 self_project 时提案只写进 proposals.md；配上后第一次收信号：近 14 天还出现过的写进待做一次，
// 14 天没出现的、对不上信号的标「已过时」，不进待做；之后再收不重复写。
func TestReproposalAfterSelfProjectOn(t *testing.T) {
	r := newSigRig(t, nil)
	r.now = time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	for i, id := range []string{"t1", "t2"} {
		r.meta(&state.Job{ID: id, Project: []string{"web", "api"}[i], Status: state.StatusTimeout, Summary: "跑了四个小时"})
	}
	r.collect()
	r.now = time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	f, err := os.OpenFile(r.s.ProposalsPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("- 2026-10-20 09:00　agentq 自身：巡逻发现问题反复出现：早就不报了 —— 同类信号 2 次；证据：巡逻 gone:1 10-19 08:00\n")
	f.Close()
	if st, _ := r.s.LoadSignals(); len(st.ProposedKey) != 2 {
		t.Fatalf("没配时两条都写进 proposals.md、记成提过：%+v", st.ProposedKey)
	}

	r.s.Cfg.SelfProject, r.s.Cfg.ExcludeProjects = "agentq", []string{"agentq"}
	p := selfRepo(t, r, "agentq")
	r.now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local) // 门判不了 10 天前还出现，超时 19 天没出现
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	todo, _, _ := strings.Cut(text, "## 已完成")
	if strings.Count(todo, "- [ ] agentq 自身：门判不了反复出现") != 1 || strings.Contains(text, "任务超时") || strings.Contains(text, "早就不报了") {
		t.Fatalf("还在的进待做一次，过时的不进：\n%s", text)
	}
	if l := fileLineOf(t, r, "门判不了反复出现"); !strings.HasSuffix(l, "（已转进 agentq 的待做 11-20）") {
		t.Fatalf("转过去的要标：%s", l)
	}
	for _, want := range []string{"任务超时反复出现", "早就不报了"} {
		if l := fileLineOf(t, r, want); !strings.Contains(l, "（已过时 11-20：近 14 天没再出现）") {
			t.Fatalf("过时的要标：%s", l)
		}
	}
	if items := r.s.SelfItemsToStart(&state.State{}); len(items) != 1 || !strings.Contains(items[0].Title, "门判不了") {
		t.Fatalf("转进待做的照自己长大接着做：%+v", items)
	}
	if day := testkit.ReadFile(t, r.s.NightlyPath(r.now)); !strings.Contains(day, "1 条转进 agentq 的待做，2 条已过时") {
		t.Fatalf("当天汇总要一行：%s", day)
	}

	file := testkit.ReadFile(t, r.s.ProposalsPath())
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "ok2", Project: "web"})
	r.collect()
	if got := testkit.ReadFile(t, p.BacklogPath()); got != text {
		t.Fatalf("复审只一次：\n%s", got)
	}
	if got := testkit.ReadFile(t, r.s.ProposalsPath()); got != file {
		t.Fatalf("标过的不再动：\n%s", got)
	}
	r.noCLI()
}

// 自己长大关着、配上 self_project：旧提案转进「建议」，不进待做。
func TestReproposalToSuggestWhenSelfGrowOff(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfGrowEnabled = false })
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	r.s.Cfg.SelfProject = "agentq"
	p := selfRepo(t, r, "agentq")
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	todo, sug, _ := strings.Cut(text, "## 建议")
	if strings.Contains(todo, "agentq 自身") || strings.Count(sug, "agentq 自身：门判不了反复出现") != 1 {
		t.Fatalf("要进「建议」不进待做：\n%s", text)
	}
	if l := fileLineOf(t, r, "门判不了反复出现"); !strings.Contains(l, "（已转进 agentq 的「建议」") {
		t.Fatalf("转过去的要标：%s", l)
	}
}

// 没配 self_project：照旧只写 proposals.md，不复审、不标。
func TestReproposalNotWithoutSelfProject(t *testing.T) {
	r := newSigRig(t, nil)
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	before := testkit.ReadFile(t, r.s.ProposalsPath())
	r.now = r.now.Add(20 * 24 * time.Hour)
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	if got := testkit.ReadFile(t, r.s.ProposalsPath()); got != before || strings.Contains(got, markStale) {
		t.Fatalf("没配不复审：\n%s", got)
	}
}

// overridden 造一次放行合并：passed 非空时门目录里写一份门都过了的记录（带不带 Hold 由 hold 定）。
func (r *sigRig) overridden(id, fp string, reruns int, passed bool, hold string) {
	if passed {
		r.write(filepath.Join(r.s.Gate.Dir(id), "passed-0123456789abcdef.json"), map[string]any{"results": []gate.Result{
			{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError, Summary: ".agentq/verify 过了"},
			{Gate: "eval", Outcome: gate.Pass, Level: gate.LevelError, Summary: "评测没有变差", Hold: hold},
		}})
	}
	r.meta(&state.Job{ID: id, Project: "agentq", Fingerprint: fp, Title: "标题 " + fp,
		Gate: &state.GateRecord{State: state.GateMerged, Passes: 1, Reruns: reruns, Override: "看过 " + id + "，判定没变", Merge: "abcd" + id}})
	r.gateResult(id, gate.RunResult{Outcome: "merged", Merge: "abcd" + id, Results: []gate.Result{{Gate: "override", Outcome: gate.Pass, Level: gate.LevelError, Summary: "用户放行"}}})
}

func overrideSignals(t *testing.T, r *sigRig) []Signal {
	t.Helper()
	st, err := r.s.LoadSignals()
	if err != nil {
		t.Fatal(err)
	}
	var out []Signal
	for _, sig := range st.Signals {
		if sig.Kind == KindGateOverride {
			out = append(out, sig)
		}
	}
	return out
}

// 门都过了、只因为改了尺子等人看，用户放行的：不记 gate-override；之前那一轮被门拦过的照样算确认（门是对的）。
func TestRulerHoldOverrideNotSignal(t *testing.T) {
	r := newSigRig(t, nil)
	r.blocked("h0", "agentq", "fph", "go test ./internal/x 失败：TestFoo")
	r.overridden("h1", "fph", 0, true, "改了评测题、判分或门的判定（internal/evolve/judge.go）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行")
	r.overridden("h2", "fpi", 0, true, "改了评测题、判分或门的判定（internal/gate/queue.go）：评判的尺子不让候选自己改")
	r.collect()
	if got := overrideSignals(t, r); len(got) != 0 {
		t.Fatalf("改了尺子等人放行的不是门的问题：%+v", got)
	}
	if cs, err := r.s.LoadConstraints("agentq"); err != nil || len(cs) != 1 {
		t.Fatalf("前一轮的拦截门是对的，照样确认：%v %+v", err, cs)
	}
	r.noCLI()
}

// 门判不了、门出错后用户放行的（没有门都过了的记录），和再审过才放行的：照旧记 gate-override。
func TestOverrideAfterUnknownStillSignal(t *testing.T) {
	r := newSigRig(t, nil)
	r.overridden("k1", "fpk", 0, false, "")
	r.overridden("k2", "fpl", 1, true, "改了评测题、判分或门的判定（internal/evolve/judge.go）：评判的尺子不让候选自己改")
	r.overridden("k3", "fpm", 0, true, "") // 门都过了但不是门要人看（比如难撤回），也照旧
	r.collect()
	got := overrideSignals(t, r)
	var ev []string
	for _, sig := range got {
		ev = append(ev, sig.Evidence...)
	}
	joined := strings.Join(ev, "、")
	for _, id := range []string{"k1", "k2", "k3"} {
		if !strings.Contains(joined, "任务 "+id) {
			t.Fatalf("%s 要记成放行信号：%+v", id, got)
		}
	}
}

// 11-11 的两次「改了尺子等人看」的放行是在不记这类放行之前记下的，信号一直留在 signals.json；11-15 配上 self_project 后
// 复审旧提案把它原样转进了待做。按现在的规则重判：这样的证据去掉，一份不剩的信号删掉，旧提案标「已过时」、不进待做；
// 同一条信号里门判不了后放行的证据照留。
func TestStaleRulerOverrideNotReproposed(t *testing.T) {
	r := newSigRig(t, nil)
	hold := "改了评测题、判分或门的判定（internal/setup/setup.go）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行"
	r.overridden("h1", "fph", 0, true, hold)
	r.overridden("h2", "fpi", 0, true, hold)
	r.overridden("k1", "fpk", 0, false, "")
	ev := func(id string) string { return "任务 " + id + "（agentq）" }
	files := map[string]string{}
	for _, id := range []string{"h1", "h2", "k1"} { // 这几份都读过了：信号是当时按旧规则记的
		for _, f := range []string{filepath.Join("jobs", id, "meta.json"), filepath.Join("gate", id, "last.json")} {
			fi, err := os.Stat(filepath.Join(r.s.DataDir(), f))
			if err != nil {
				t.Fatal(err)
			}
			files[f] = fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	day := r.now.Add(-24 * time.Hour)
	stale := SignalKey(KindGateOverride, "test", "改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）")
	mixed := SignalKey(KindGateOverride, "", "看过，判定没变")
	r.write(r.s.SignalsPath(), &SignalStore{Checked: day, Files: files, ProposedKey: []string{stale}, Signals: []Signal{
		{Kind: KindGateOverride, Key: stale, Summary: "用户放行了test 门拦下的任务：改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）",
			Evidence: []string{ev("h1"), ev("h2")}, Seen: 2, First: day, Last: day, Proposed: true},
		{Kind: KindGateOverride, Key: mixed, Summary: "用户放行了门拦下的任务：看过，判定没变", Evidence: []string{ev("h1"), ev("k1")}, Seen: 2, First: day, Last: day},
	}})
	os.WriteFile(r.s.ProposalsPath(), []byte(proposalsHead+"- 2026-11-07 22:09　agentq 自身：用户放行了门拦下的任务反复出现：用户放行了test 门拦下的任务：改了测试或门脚本，提交说明 —— 同类信号 2 次，11-07 到 11-07：用户放行了test 门拦下的任务：改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）；证据：任务 h1（agentq）、任务 h2（agentq）；自我进化 L0 提案，只提案不改代码，做不做你定\n"), 0o600)

	r.s.Cfg.SelfProject, r.s.Cfg.ExcludeProjects = "agentq", []string{"agentq"}
	p := selfRepo(t, r, "agentq")
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	if todo, _, _ := strings.Cut(testkit.ReadFile(t, p.BacklogPath()), "## 已完成"); strings.Contains(todo, "用户放行了门拦下的任务") {
		t.Fatalf("按旧规则记的尺子放行不该转进待做：\n%s", todo)
	}
	if l := fileLineOf(t, r, "用户放行了门拦下的任务反复出现"); !strings.Contains(l, markStale) {
		t.Fatalf("旧提案要标「已过时」：%s", l)
	}
	got := overrideSignals(t, r)
	if len(got) != 1 || got[0].Key != mixed || got[0].Seen != 1 || !slices.Equal(got[0].Evidence, []string{ev("k1")}) {
		t.Fatalf("尺子放行的证据去掉，门判不了后放行的留着：%+v", got)
	}
	r.noCLI()
}

// 放行信号写的是这个任务自己被拦下的门；前一轮（退回后换了任务号）被拦的已经改过、门也过了，不是这次放行的。
func TestOverrideNamesOwnBlock(t *testing.T) {
	r := newSigRig(t, nil)
	r.blocked("a0", "agentq", "fpa", "改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）")
	r.blocked("b0", "agentq", "fpb", "改了测试或门脚本，提交说明里没写原因（单写一行「门说明：原因」）")
	r.blocked("c1", "agentq", "fpc", "go test ./internal/x 失败：TestFoo")
	r.collect()
	r.overridden("a1", "fpa", 0, false, "")
	r.overridden("b1", "fpb", 0, false, "")
	r.overridden("c1", "fpc", 0, false, "")
	r.collect()
	var sums []string
	for _, sig := range overrideSignals(t, r) {
		if sig.Seen != 1 {
			t.Fatalf("三次放行原因各不相同，不该凑成一类：%+v", sig)
		}
		sums = append(sums, sig.Summary)
	}
	slices.Sort(sums)
	want := []string{"用户放行了test 门拦下的任务：go test ./internal/x 失败：TestFoo", "用户放行了门拦下的任务：看过 a1，判定没变", "用户放行了门拦下的任务：看过 b1，判定没变"}
	if !slices.Equal(sums, want) {
		t.Fatalf("放行信号要写这个任务自己被拦的门：\n%q", sums)
	}
	r.noCLI()
}
