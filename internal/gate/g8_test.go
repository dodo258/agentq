package gate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 按门的次数记到任务上；warn 档没过算「会挡下」；同一个 tree 重放不多算；放行不算门跑了。
func TestTally(t *testing.T) {
	f := newFix(t, fixBacklog)
	register(t, fakeGate{name: "警告门", outcome: gate.Fail, applies: true}, gate.LevelWarn)
	register(t, fakeGate{name: "判不了门", outcome: gate.Unknown, applies: true}, gate.LevelWarn)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	dirty := filepath.Join(f.dir, "a.txt")
	os.WriteFile(dirty, []byte("主目录的改动\n"), 0o644) // 主目录挡着：门都过了，合不了
	res := f.run(j)
	if res.Outcome != "wait" {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	tally := j.Gate.Tally
	if tally["警告门"] == nil || tally["警告门"].Fail != 1 || tally["警告门"].WouldBlock != 1 || tally["判不了门"].Unknown != 1 || tally["判不了门"].WouldBlock != 1 || tally["test"].Pass != 1 {
		t.Fatalf("%+v", tally)
	}
	os.Remove(dirty)
	res = f.run(j) // 同一个 tree：结果沿用，不算又跑了一次
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if got := j.Gate.Tally["警告门"]; got.Fail != 1 || got.WouldBlock != 1 || j.Gate.Tally["test"].Pass != 1 {
		t.Errorf("重放不多算：%+v", got)
	}
	// 落盘后读得回来
	b, _ := json.Marshal(j)
	var back state.Job
	if err := json.Unmarshal(b, &back); err != nil || back.Gate.Tally["警告门"].Fail != 1 {
		t.Errorf("%v %+v", err, back.Gate)
	}
}

// main 动过重建时，前面那次跑的门也算。
func TestTallyAcrossRebuild(t *testing.T) {
	f := newFix(t, fixBacklog)
	moved := false
	register(t, fakeGate{name: "假门", applies: true, run: func(c gate.Candidate) gate.Result {
		if !moved {
			moved = true
			os.WriteFile(filepath.Join(f.dir, "user.txt"), []byte("用户的\n"), 0o644)
			testkit.GitT(t, f.home, f.dir, "add", "user.txt")
			testkit.GitT(t, f.home, f.dir, "commit", "-q", "-m", "用户在 main 上提交")
		}
		return gate.Result{Outcome: gate.Pass, Summary: "过"}
	}}, gate.LevelError)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	f.collect(j, res)
	if res.Outcome != "merged" || j.Gate.Tally["假门"].Pass != 2 {
		t.Fatalf("%+v %+v", res, j.Gate.Tally["假门"])
	}
}

// saveState：命令是从状态文件认任务的。
func (f *fix) saveState() {
	f.t.Helper()
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		f.t.Fatal(err)
	}
}

func http529() string {
	b, _ := json.Marshal(testkit.FakeEvent{Type: "result", OK: false, Text: "API Error: 529", Status: 529})
	return string(b) + "\n"
}

// 上游 5xx：不当场重试，排回去，15 分钟、1 小时后重跑；连续 3 次判不了，不放行；次数写进统计。
func TestUpstreamBackoff(t *testing.T) {
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), http529())
	j := f.job(newFiles())
	base := f.a.Now()
	for i, wait := range []time.Duration{15 * time.Minute, time.Hour} {
		res := f.run(j)
		r := reviewResult(t, res)
		if res.Outcome != "defer" || !r.Upstream || r.Defer == "" {
			t.Fatalf("第 %d 次：%+v", i+1, res)
		}
		if f.callCount() != i+1 {
			t.Fatalf("不当场重试：调了 %d 次", f.callCount())
		}
		f.collect(j, res)
		if j.Gate.State != state.GateQueued || j.Gate.Upstream != i+1 {
			t.Fatalf("%+v", j.Gate)
		}
		if d := j.Gate.NotBefore.Sub(base); d < wait-time.Minute || d > wait+time.Minute {
			t.Errorf("第 %d 次要等 %s：%s", i+1, wait, d)
		}
		if st := f.st; !st.CooldownUntil.IsZero() {
			t.Errorf("5xx 不是撞限，不停别的任务：%v", st.CooldownUntil)
		}
	}
	res := f.run(j)
	r := reviewResult(t, res)
	if res.Outcome != "unknown" || r.Upstream || r.Defer != "" || !strings.Contains(r.Summary, "连续 3 次") || f.callCount() != 3 {
		t.Fatalf("%+v %d", res, f.callCount())
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck || j.Gate.Upstream != 0 || j.Gate.Round != 0 {
		t.Fatalf("判不了等用户定，不算退回轮数：%+v", j.Gate)
	}
	if got := j.Gate.Tally["review"]; got.Deferred != 2 || got.Unknown != 1 {
		t.Errorf("%+v", got)
	}
}

// 中间好了：照常合并，计数清零。
func TestUpstreamRecovers(t *testing.T) {
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), http529(), reply("pass"))
	j := f.job(newFiles())
	f.collect(j, f.run(j))
	if j.Gate.Upstream != 1 {
		t.Fatalf("%+v", j.Gate)
	}
	res := f.run(j)
	f.collect(j, res)
	if res.Outcome != "merged" || j.Gate.Upstream != 0 || j.Gate.State != state.GateMerged {
		t.Fatalf("%+v %+v", res, j.Gate)
	}
}

// warn 档的门撞 5xx 用完次数：只记录，照常合并。
func TestUpstreamWarnLevel(t *testing.T) {
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), http529())
	os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(`{"levels":{"review":"warn"}}`), 0o644)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "审查门 warn")
	j := f.job(newFiles())
	j.Gate.Upstream = gate.MaxUpstream
	res := f.run(j)
	f.collect(j, res)
	if res.Outcome != "merged" || j.Gate.Tally["review"].WouldBlock != 1 {
		t.Fatalf("%+v %+v", res, j.Gate.Tally["review"])
	}
}

func stuckCard(t *testing.T, f *fix) *notify.DecideCard {
	t.Helper()
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range s.Pending {
		if n.Card != nil {
			return n.Card
		}
	}
	t.Fatal("没有卡片")
	return nil
}

// 再审一次：不沿用旧结论，通过就合并，不算退回轮数。
func TestRerunPasses(t *testing.T) {
	block := reply("block", finding(newFile, 1, "blocker", "这里错了", "跑一下"))
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), block, reply("pass"))
	j := f.job(newFiles())
	j.Gate.Round = gate.MaxRounds // 退回轮数用完了，卡住等用户定
	f.collect(j, f.run(j))
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	card := stuckCard(t, f.fix)
	if len(card.Options) != 4 || card.Options[3].Label != "D" || card.Options[3].Text != "再审一次" {
		t.Fatalf("%+v", card)
	}
	f.saveState()
	if err := f.a.CmdGate([]string{"rerun", j.ID}); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.State != state.GateQueued || !j.Gate.Rerun || j.Gate.Round != gate.MaxRounds || j.Gate.Reruns != 1 {
		t.Fatalf("%+v", j.Gate)
	}
	if p := gate.PlanOf(f.a, j); !p.Rerun {
		t.Error("计划要带上再审")
	}
	res := f.run(j)
	if res.Outcome != "merged" || f.callCount() != 2 {
		t.Fatalf("再审不沿用旧结论：%+v，调了 %d 次", res, f.callCount())
	}
	f.collect(j, res)
	if j.Gate.State != state.GateMerged || j.Gate.Rerun {
		t.Fatalf("%+v", j.Gate)
	}
	if b := testkit.ReadFile(t, filepath.Join(f.a.DataDir(), "gate", "audit.log")); !strings.Contains(b, "再审 "+j.ID) {
		t.Errorf("要留痕：%s", b)
	}
}

// 再审没过：还是等用户定，不退回，不消耗轮数。同一提交有上限，到了卡片不再给按钮，命令拒绝。
func TestRerunLimit(t *testing.T) {
	block := reply("block", finding(newFile, 1, "blocker", "这里错了", "跑一下"))
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), block)
	j := f.job(newFiles())
	f.collect(j, gate.RunResult{Outcome: "unknown", Reason: "判不了", Head: j.Gate.Head})
	for i := 1; i <= gate.MaxReruns; i++ {
		f.saveState()
		if err := f.a.CmdGate([]string{"rerun", j.ID}); err != nil {
			t.Fatalf("第 %d 次：%v", i, err)
		}
		f.a.Collect(f.st)
		res := f.run(j)
		if res.Outcome != "fail" {
			t.Fatalf("%+v", res)
		}
		f.collect(j, res)
		if j.Gate.State != state.GateStuck || j.Gate.Round != 0 || j.GateRound != 0 || j.Status == state.StatusInterrupted || j.Gate.Rerun {
			t.Fatalf("再审没过不退回：%s %+v", j.Status, j.Gate)
		}
	}
	if f.callCount() != gate.MaxReruns {
		t.Errorf("每次再审调一次模型：%d", f.callCount())
	}
	f.saveState()
	err := f.a.CmdGate([]string{"rerun", j.ID})
	if err == nil || !strings.Contains(err.Error(), "已经再审过") {
		t.Fatalf("%v", err)
	}
	if card := stuckCard(t, f.fix); len(card.Options) != 3 {
		t.Errorf("次数到了不给按钮：%+v", card)
	}
	// 卡片点得早、写到决定文件里的：tick 落的时候再查一次
	if err := f.a.Decide(j.ID, gate.DecideRerun, ""); err != nil {
		t.Fatal(err)
	}
	calls := f.callCount()
	f.a.Collect(f.st)
	if j.Gate.State != state.GateStuck || f.callCount() != calls {
		t.Errorf("%+v", j.Gate)
	}
	// 没卡在门上的不能再审
	k := &state.Job{ID: "1105-100099-demo", Gate: &state.GateRecord{State: state.GateMerged, Head: "x"}}
	f.st.Jobs = append(f.st.Jobs, k)
	f.saveState()
	if err := f.a.CmdGate([]string{"rerun", k.ID}); err == nil {
		t.Error("合并了不能再审")
	}
}

// 用户看过后在分支上补了提交再要再审：改看分支现在的最新提交（放行也只合它），次数重新算。
// 分支上读不出合格交付单的（比如补提交删了 ready.json）：还看原来的提交。
func TestRerunNewHead(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	old := j.Gate.Head
	j.Gate.State, j.Gate.RerunHead, j.Gate.Reruns = state.GateStuck, old, gate.MaxReruns
	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	f.commit(wt, map[string]string{"a.txt": "改对了"}, gate.Ready{})
	tip := testkit.GitT(t, f.home, wt, "rev-parse", "HEAD")
	f.saveState()
	if err := f.a.CmdGate([]string{"rerun", j.ID}); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.Head != tip || j.Gate.RerunHead != tip || j.Gate.Reruns != 1 || j.Gate.State != state.GateQueued {
		t.Fatalf("要看补的提交 %s：%+v", tip, j.Gate)
	}
	if p := gate.PlanOf(f.a, j); p.Head != tip {
		t.Errorf("门进程要合补的提交：%s", p.Head)
	}

	j.Gate.State = state.GateStuck
	testkit.GitT(t, f.home, wt, "rm", "-q", gate.ReadyFile)
	testkit.GitT(t, f.home, wt, "commit", "-q", "-m", "删交付单")
	f.saveState()
	if err := f.a.CmdGate([]string{"rerun", j.ID}); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if j.Gate.Head != tip || j.Gate.Reruns != 2 {
		t.Errorf("交付单没了还看原来的提交：%+v", j.Gate)
	}
}

// 早报的门一节：24 小时内的决定、warn 没过的、关着的门，只写事实；旧的不写；用了 gates.json 的默认档位不算关。
func TestMorningLines(t *testing.T) {
	f := newFix(t, fixBacklog)
	now := f.a.Now()
	path := filepath.Join(f.a.DataDir(), "gate", "audit.log")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(now.Add(-48*time.Hour).Format(time.RFC3339)+" 放行 老任务 不该出现\n坏的一行\n"+now.Add(-time.Hour).Format(time.RFC3339)+" 放行 1105-a demo「做事」分支 x：急\n"+now.Add(-time.Minute).Format(time.RFC3339)+" 放弃 1105-b demo「别的」分支 y 留着\n"), 0o600)
	recent := now.Add(-time.Hour)
	f.st.Jobs = append(f.st.Jobs,
		&state.Job{ID: "w1", Project: "demo", Title: "警告的", Gate: &state.GateRecord{StartedAt: recent, Tally: map[string]*state.GateTally{"ui": {Fail: 1, WouldBlock: 1}, "smoke": {Unknown: 1, WouldBlock: 1}, "test": {Pass: 1}}}},
		&state.Job{ID: "w2", Project: "demo", Title: "很早的", Gate: &state.GateRecord{StartedAt: now.Add(-48 * time.Hour), Tally: map[string]*state.GateTally{"ui": {WouldBlock: 1}}}},
		&state.Job{ID: "w3", Project: "demo", Title: "都过了", Gate: &state.GateRecord{StartedAt: recent, Tally: map[string]*state.GateTally{"ui": {Pass: 1}}}},
		&state.Job{ID: "w4", Project: "demo", Title: "老记录", Gate: &state.GateRecord{StartedAt: recent}},
	)
	text := strings.Join(f.a.MorningLines(f.st, now), "\n")
	for _, want := range []string{"用户放行 1105-a", "用户放弃 1105-b", "warn 没过：demo「警告的」smoke、ui", "关着的门：demo　review"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺 %q：\n%s", want, text)
		}
	}
	for _, bad := range []string{"老任务", "坏的一行", "很早的", "都过了", "老记录", "$", "美元", "费用"} {
		if strings.Contains(text, bad) {
			t.Errorf("不该有 %q：\n%s", bad, text)
		}
	}
	// 太多了收起来
	var b strings.Builder
	for i := 0; i < 8; i++ {
		b.WriteString(now.Add(-time.Minute).Format(time.RFC3339) + " 放行 x\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	if text := strings.Join(f.a.MorningLines(&state.State{}, now), "\n"); strings.Count(text, "放行 x") != 5 || !strings.Contains(text, "另有 3 条") {
		t.Errorf("%s", text)
	}
}

// 合并时哪些门关着，写进当天汇总。
func TestOffGatesInNightly(t *testing.T) {
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	f.collect(j, f.run(j))
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	b := testkit.ReadFile(t, f.k.Ledger.NightlyPath(f.a.Now()))
	if !strings.Contains(b, "合并时这些门关着：review") {
		t.Errorf("%s", b)
	}
}

// 审查提示词看得到定下的大方向；没定时不多一行。
func TestReviewSeesDirection(t *testing.T) {
	body := "# 待办\n\n## 待做\n\n- [ ] 做事 —— 要什么" + backlog.DirectionMark("用户定：纸墨风", 100) + "\n\n## 已完成\n"
	f := newReviewFix(t, body, testkit.AllCaps(), reply("pass"))
	f.run(f.job(newFiles()))
	if args := testkit.ReadFile(t, f.args); !strings.Contains(args, "开工前定的大方向：用户定：纸墨风") {
		t.Errorf("%s", args)
	}
	g := newReviewFix(t, fixBacklog, testkit.AllCaps(), reply("pass"))
	g.run(g.job(newFiles()))
	if args := testkit.ReadFile(t, g.args); strings.Contains(args, "开工前定的大方向") {
		t.Errorf("%s", args)
	}
}

// 记录读得出是哪个任务的，读不出是空。
func TestReviewOwner(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.json")
	os.WriteFile(p, []byte(`{"job_id":"1105-1-demo"}`), 0o644)
	if got := gate.ReviewOwner(p); got != "1105-1-demo" {
		t.Errorf("%q", got)
	}
	os.WriteFile(p, []byte(`{坏`), 0o644)
	if got := gate.ReviewOwner(p); got != "" {
		t.Errorf("%q", got)
	}
	if got := gate.ReviewOwner(filepath.Join(dir, "没有")); got != "" {
		t.Errorf("%q", got)
	}
}
