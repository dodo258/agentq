package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// supersededEnv：暂停着的 agentq，一个待做里放 backlogLines 的项目 demo。
func supersededEnv(t *testing.T, backlogLines ...string) (*testEnv, string) {
	t.Helper()
	e := newEnv(t, map[string]any{"paused": true})
	return e, gateRepo(t, e, "demo", backlogLines...)
}

// saveStuck 写 state.json，推送队列里给每个卡在门上的任务攒一张「门没过，等你定」。
func (e *testEnv) saveStuck(jobs ...*Job) {
	e.t.Helper()
	var pending []notify.Notice
	for _, j := range jobs {
		if j.Gate != nil && j.Gate.State == state.GateStuck {
			pending = append(pending, notify.Notice{Cat: "decide", Key: "gate:" + j.ID, JobID: j.ID, Text: j.Project + "　「" + j.Title + "」门没过，等你定"})
		}
	}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), &State{Jobs: jobs}); err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(notify.State{Pending: pending})
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func stuckJob(id, dir, title, fp string, started time.Time) *Job {
	ended := started.Add(20 * time.Minute)
	return &Job{ID: id, Project: "demo", ProjectDir: dir, DefaultBranch: "main", Title: title, Item: title + " —— 要什么", Fingerprint: fp,
		Status: state.StatusDone, StartedAt: started, EndedAt: &ended, StuckMarked: true, StuckBranch: "agent/1122-fix",
		Gate: &state.GateRecord{State: state.GateStuck, Branch: "agent/1122-fix", Head: "0123456789abcdef", Round: 2, Report: "退回 2 轮还没过"}}
}

func (e *testEnv) pendingKeys() []string {
	e.t.Helper()
	var s notify.State
	if err := json.Unmarshal([]byte(testkit.ReadFile(e.t, filepath.Join(e.home, ".agentq", "notify.json"))), &s); err != nil {
		e.t.Fatal(err)
	}
	var keys []string
	for _, n := range s.Pending {
		keys = append(keys, n.Key)
	}
	return keys
}

func jobByID(t *testing.T, st *State, id string) *Job {
	t.Helper()
	j, err := st.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// 11-23 huitai：旧任务门 stuck，同一条目后来另起的任务合并了。tick 后旧门标成已被取代（写明被哪个任务、哪个合并提交），
// status 不再列「等你定」，攒着的那张卡片撤掉；同项目别的条目卡在门上的照常列、照常推。
func TestGateSupersededByLaterMerge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	const merge = "202013523e5c94c91bc2a14419c968b62e9578bf"
	e, dir := supersededEnv(t, "- [ ] 修东西 —— 要什么", "- [ ] 别的事 —— 要什么")
	ended := now.Add(-time.Hour)
	later := &Job{ID: "1123-002603-demo", Project: "demo", ProjectDir: dir, DefaultBranch: "main", Title: "修东西", Fingerprint: "fp-a",
		Status: state.StatusDone, StartedAt: now.Add(-2 * time.Hour), EndedAt: &ended, Manual: true,
		Gate: &state.GateRecord{State: state.GateMerged, Branch: "agent/1122-fix", Merge: merge}}
	e.saveStuck(stuckJob("1123-000756-demo", dir, "修东西", "fp-a", now.Add(-3*time.Hour)),
		stuckJob("1123-000900-demo", dir, "别的事", "fp-b", now.Add(-3*time.Hour)), later)
	if out := e.mustRun("status"); !strings.Contains(out, "门没过，等你定：demo「修东西」") {
		t.Fatalf("tick 之前照常列：\n%s", out)
	}
	tick := e.mustRun("tick")
	st := e.state()
	g := jobByID(t, st, "1123-000756-demo").Gate
	if g.State != state.GateSuperseded || g.SupersededBy != "1123-002603-demo" || g.SupersededMerge != merge {
		t.Fatalf("%+v", g)
	}
	out := e.mustRun("status")
	if strings.Contains(out, "等你定：demo「修东西」") || !strings.Contains(out, "门没过，等你定：demo「别的事」") {
		t.Errorf("status：\n%s", out)
	}
	keys := e.pendingKeys()
	if slices.Contains(keys, "gate:1123-000756-demo") || !slices.Contains(keys, "gate:1123-000900-demo") {
		t.Errorf("推送队列：%v", keys)
	}
	report := e.mustRun("gate", "report", "1123-000756-demo")
	if !strings.Contains(report, "已被取代，不用再定") || !strings.Contains(report, "任务 1123-002603-demo（合并提交 202013523e5c）") {
		t.Errorf("%s", report)
	}
	meta := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", "1123-000756-demo", "meta.json"))
	if !strings.Contains(meta, `"superseded_by": "1123-002603-demo"`) {
		t.Errorf("meta.json 要落盘：%s", meta)
	}
	if out, code := e.run("gate", "pass", "1123-000756-demo", "--reason", "x"); code == 0 || !strings.Contains(out, "不是卡在门上") {
		t.Errorf("已被取代的不能再放行：%d %s", code, out)
	}
	if log := tick + e.launchdLog(); !strings.Contains(log, "任务 1123-000756-demo 已被任务 1123-002603-demo（合并提交 202013523e5c）取代") {
		t.Errorf("日志：%s", log)
	}
	if g := jobByID(t, e.state(), "1123-000900-demo").Gate; g.State != state.GateStuck {
		t.Errorf("没被取代的照旧等你定：%+v", g)
	}
}

// 条目已经勾成完成、写着这个任务开始之后的合并提交（比如前台会话手动合并，或做完它的任务记录已经清掉）：一样撤掉。
// 「已完成」里同名条目写的是这个任务开始之前的提交（以前同名的旧条目）、或「待做」里还有同名条目，都不算。
func TestGateSupersededByFinishedItem(t *testing.T) {
	t.Parallel()
	now := time.Now()
	e, dir := supersededEnv(t, "- [ ] 还在做 —— 要什么")
	// 已完成：修东西（这次的合并）、老条目（以前同名的）、还在做（同名的旧条目，待做里还有一条）
	b := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md"))
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(b+"- [x] 老条目 —— 要什么；合并提交 PLACEHOLDER\n- [x] 还在做 —— 要什么；合并提交 PLACEHOLDER\n"), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "合并 老条目")
	commit := testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
	b = strings.ReplaceAll(testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")), "PLACEHOLDER", commit)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(b+"- [x] 修东西 —— 要什么；合并提交 "+commit[:12]+"\n"), 0o644)
	testkit.GitT(t, e.home, dir, "commit", "-q", "-am", "合并 修东西")
	e.saveStuck(stuckJob("1123-000756-demo", dir, "修东西", "fp-a", now.Add(-3*time.Hour)),
		stuckJob("1123-000800-demo", dir, "老条目", "fp-b", now.Add(time.Hour)), // 合并提交比任务早
		stuckJob("1123-000900-demo", dir, "还在做", "fp-c", now.Add(-3*time.Hour)))

	e.mustRun("tick")
	st := e.state()
	if g := jobByID(t, st, "1123-000756-demo").Gate; g.State != state.GateSuperseded || g.SupersededBy != "" || g.SupersededMerge != commit {
		t.Fatalf("%+v", g)
	}
	for _, id := range []string{"1123-000800-demo", "1123-000900-demo"} {
		if g := jobByID(t, st, id).Gate; g.State != state.GateStuck {
			t.Errorf("%s 不该撤：%+v", id, g)
		}
	}
	out := e.mustRun("status")
	if strings.Contains(out, "等你定：demo「修东西」") || !strings.Contains(out, "等你定：demo「老条目」") || !strings.Contains(out, "等你定：demo「还在做」") {
		t.Errorf("status：\n%s", out)
	}
	if keys := e.pendingKeys(); slices.Contains(keys, "gate:1123-000756-demo") || len(keys) != 2 {
		t.Errorf("推送队列：%v", keys)
	}
	if report := e.mustRun("gate", "report", "1123-000756-demo"); !strings.Contains(report, "已完成的同一条目（合并提交 "+commit[:12]+"）") {
		t.Errorf("%s", report)
	}
}

// 卡片已经推出去了：同一条目被别的任务做完后，那次推送把卡片改成结论、去掉按钮，再点回已经定过。
func TestGateCardSuperseded(t *testing.T) {
	e, j, btns, msgID := gateCardEnv(t)
	j.Gate.State, j.Gate.SupersededBy, j.Gate.SupersededMerge = state.GateSuperseded, "1117-010000-demo", "202013523e5c94c91bc2a14419c968b62e9578bf"
	e.flush()
	want := "同一条目已经由任务 1117-010000-demo（合并提交 202013523e5c）做完，不用再定"
	if text := settledEdit(t, e, msgID); !strings.Contains(text, want) {
		t.Errorf("%s", text)
	}
	e.tg.click(1, ownerChat, int(msgID), btns[0].Data)
	e.flush()
	if a := e.tg.lastAnswer(); !strings.Contains(a, "已经定过了："+want) {
		t.Errorf("%s", a)
	}
	if _, _, ok := e.a.Gate.PendingDecision(j.ID); ok {
		t.Error("已被取代的不再写决定")
	}
}
