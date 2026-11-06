package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

const productBacklog = "# 待办\n\n## 待做\n\n- [ ] 做第一版（成品） —— 要能打开\n- [ ] 普通的活 —— 提到（成品）不算\n\n## 等用户决定\n\n## 已完成\n"

func (f *fix) smokeScript(body string) {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.dir, gate.SmokeScript), []byte(body), 0o755)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "烟测脚本")
}

func smokeResult(t *testing.T, res gate.RunResult) gate.Result {
	t.Helper()
	r, ok := resultOf(res, "smoke")
	if !ok {
		t.Fatalf("没跑成品门：%+v", res)
	}
	return r
}

// 只有标了（成品）的条目跑；正文顺带提到的不算。
func TestSmokeOnlyOnProductItems(t *testing.T) {
	f := newFix(t, productBacklog)
	calls := filepath.Join(t.TempDir(), "calls")
	f.smokeScript("#!/bin/sh\necho x >> " + calls + "\nexit 0\n")
	j := f.claim("普通的活", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	if _, ok := resultOf(res, "smoke"); ok || res.Outcome != "merged" {
		t.Fatalf("普通条目不跑：%+v", res)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("脚本不该跑")
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile)); err == nil {
		t.Error("普通条目没有交付单")
	}
}

// 过了生成交付单：做了什么、怎么打开看；通知带上；gate report 看得到。
func TestSmokePassDelivers(t *testing.T) {
	f := newFix(t, productBacklog)
	f.smokeScript("#!/bin/sh\nexit 0\n")
	r := ready("agent/x")
	r.Progress, r.Acceptance = "做出了第一版", "npm start 后开 localhost:3000"
	j := f.claim("做第一版（成品）", "agent/x", map[string]string{"web/a.txt": "a"}, r)
	res := f.run(j)
	if res.Outcome != "merged" || smokeResult(t, res).Outcome != gate.Pass {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	sheet := testkit.ReadFile(t, filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile))
	for _, want := range []string{"# 交付单：demo「做第一版（成品）」", "## 做了什么", "做出了第一版", "要能打开", "web/a.txt", "## 怎么打开看", "npm start 后开 localhost:3000"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("交付单缺 %s：\n%s", want, sheet)
		}
	}
	q := f.endNotices(j)
	if len(q) != 1 || q[len(q)-1].Cat != "done" || !strings.Contains(q[len(q)-1].Text, "可以交付了") || !strings.Contains(strings.Join(q[len(q)-1].Lines, "\n"), "npm start") {
		t.Errorf("通知：%+v", q)
	}
}

// 验收：烟测失败不交付。默认 warn：合并了，没有交付单，告诉用户。error：退回，不合并。
func TestSmokeFailNoDelivery(t *testing.T) {
	f := newFix(t, productBacklog)
	f.smokeScript("#!/bin/sh\necho 起不来\nexit 1\n")
	j := f.claim("做第一版（成品）", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	r := smokeResult(t, res)
	if res.Outcome != "merged" || r.Outcome != gate.Fail || r.Level != gate.LevelWarn {
		t.Fatalf("warn 不挡合并：%+v", res)
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile)); err == nil {
		t.Error("烟测失败不交付")
	}
	q := f.endNotices(j)
	if len(q) != 1 || q[len(q)-1].Cat != "alert" || !strings.Contains(q[len(q)-1].Text, "烟测没过，没有交付单") {
		t.Errorf("要告诉用户：%+v", q)
	}

	e := newFix(t, productBacklog)
	e.smokeScript("#!/bin/sh\nexit 1\n")
	e.levels(`{"levels":{"review":"off","smoke":"error"}}`)
	j = e.claim("做第一版（成品）", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	before := e.main()
	res = e.run(j)
	if res.Outcome != "fail" || e.main() != before {
		t.Fatalf("error 档退回：%+v", res)
	}
	e.collect(j, res)
	if j.Gate.State != state.GateReturned {
		t.Fatalf("%+v", j.Gate)
	}
	if _, err := os.Stat(filepath.Join(e.a.Dir(j.ID), gate.DeliveryFile)); err == nil {
		t.Error("没过不交付")
	}
}

// 没脚本算判不了；warn 不挡，没有交付单。任务第一次新增用任务的版本；默认分支上有的，任务改了也没用。
func TestSmokeScriptVersions(t *testing.T) {
	f := newFix(t, productBacklog)
	j := f.claim("做第一版（成品）", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	res := f.run(j)
	if r := smokeResult(t, res); r.Outcome != gate.Unknown || !strings.Contains(r.Summary, gate.SmokeScript) || res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile)); err == nil {
		t.Error("判不了不交付")
	}

	first := newFix(t, productBacklog)
	j = first.claim("做第一版（成品）", "agent/x", map[string]string{gate.SmokeScript: "#!/bin/sh\nexit 0\n"}, ready("agent/x"))
	res = first.run(j)
	if r := smokeResult(t, res); r.Outcome != gate.Pass || !strings.Contains(strings.Join(r.Evidence, "\n"), "首次由任务新增") {
		t.Fatalf("%+v", res)
	}

	main := newFix(t, productBacklog)
	main.smokeScript("#!/bin/sh\nexit 1\n")
	j = main.claim("做第一版（成品）", "agent/x", map[string]string{gate.SmokeScript: "#!/bin/sh\nexit 0\n"}, ready("agent/x"))
	main.levels(`{"levels":{"review":"off","test":"off","smoke":"error"}}`)
	if res := main.run(j); res.Outcome != "fail" {
		t.Fatalf("任务改门没用：%+v", res)
	}
}

// 新项目第一版（NewReport 不空）也跑，打开方式用报告里的，假设写进交付单。
func TestSmokeFirstVersion(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.smokeScript("#!/bin/sh\nexit 0\n")
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.NewReport = &state.NewReport{Path: f.dir, Assumptions: "只给自己用", View: "make run"}
	res := f.run(j)
	if smokeResult(t, res).Outcome != gate.Pass {
		t.Fatalf("%+v", res)
	}
	f.collect(j, res)
	sheet := testkit.ReadFile(t, filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile))
	for _, want := range []string{"make run", "只给自己用", "看 a.txt"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("缺 %s：\n%s", want, sheet)
		}
	}
	if j.NewReport.Commit == "" || !strings.HasPrefix(res.Merge, j.NewReport.Commit) {
		t.Errorf("报告要记合并提交：%+v", j.NewReport)
	}
}

// 合并之后门进程被杀：下一个只补收尾。交付单还是要出（门的结论按 tree 留着）。撤掉 engineMerged 里读 passed 的那段就要失败。
func TestSmokeDeliverAfterCrash(t *testing.T) {
	f := newFix(t, productBacklog)
	f.smokeScript("#!/bin/sh\nexit 0\n")
	j := f.claim("做第一版（成品）", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	res := f.run(j) // 第一次的结果没收：和被杀一样
	if res.Outcome != "merged" {
		t.Fatalf("补收尾：%+v", res)
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile)); err != nil {
		t.Errorf("被杀后也要有交付单：%v", err)
	}
}

// 放弃：能放弃的和不能的；放弃后状态。
func TestAbandon(t *testing.T) {
	f := newFix(t, fixBacklog)
	ended := f.a.Now()
	running := &state.Job{ID: "r", Project: "demo", ProjectDir: f.dir, Title: "做事", Status: state.StatusRunning}
	plain := &state.Job{ID: "p", Project: "demo", ProjectDir: f.dir, Title: "下一条", Status: state.StatusInterrupted, EndedAt: &ended}
	f.st.Jobs = []*state.Job{running, plain}
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, want string }{{"r", "还在跑"}, {"p", "不是卡在门上"}, {"没有", "没有"}} {
		if err := f.a.CmdGate([]string{"abandon", c.id}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：%v", c.id, err)
		}
	}
	if err := f.a.CmdGate([]string{"abandon"}); err == nil || !strings.Contains(err.Error(), "abandon <任务id>") {
		t.Errorf("%v", err)
	}
	// 决定文件在，跑不了放弃的丢掉，不改记录
	if err := f.a.Decide("p", gate.DecideAbandon, ""); err != nil {
		t.Fatal(err)
	}
	f.a.Collect(f.st)
	if plain.Gate != nil {
		t.Errorf("%+v", plain.Gate)
	}
	if _, err := os.Stat(filepath.Join(f.a.Dir("p"), "decision.json")); err == nil {
		t.Error("决定要丢掉")
	}

	// 卡在门上的普通任务：走同一个函数，条目不动
	g := newFix(t, fixBacklog)
	j := g.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.Gate.State = state.GateStuck
	if err := g.a.Decide(j.ID, gate.DecideAbandon, ""); err != nil {
		t.Fatal(err)
	}
	g.a.Collect(g.st)
	if j.Gate.State != state.GateAbandoned {
		t.Fatalf("%+v", j.Gate)
	}
	if b, _ := os.ReadFile(filepath.Join(g.a.DataDir(), "gate", "audit.log")); !strings.Contains(string(b), "放弃 "+j.ID) {
		t.Errorf("要留痕：%s", b)
	}

	// 新项目第一版：没有门记录的接手任务能放弃，条目标卡住，最新的任务也标上
	h := newFix(t, fixBacklog)
	k := h.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	k.Gate = nil
	k.Status = state.StatusInterrupted
	k.NewReport = &state.NewReport{Path: h.dir}
	if err := h.a.Decide(k.ID, gate.DecideAbandon, ""); err != nil {
		t.Fatal(err)
	}
	// 主目录不在默认分支：放弃做不成，决定留着，下次再做
	// 那条分支上没有这一条：不能当「条目不在了」就算放弃了
	h.git("checkout", "-q", "-b", "别的")
	os.Remove(h.p.BacklogPath())
	h.git("add", "-A")
	h.git("commit", "-q", "-m", "这条分支上没有 BACKLOG")
	h.a.Collect(h.st)
	if k.Gate != nil {
		t.Fatalf("没做成不能算放弃：%+v", k.Gate)
	}
	if _, err := os.Stat(filepath.Join(h.a.Dir(k.ID), "decision.json")); err != nil {
		t.Fatalf("决定要留着：%v", err)
	}
	h.git("checkout", "-q", "main")
	h.a.Collect(h.st)
	if k.Gate == nil || k.Gate.State != state.GateAbandoned {
		t.Fatalf("%+v", k.Gate)
	}
	if !strings.Contains(h.backlog(), "（卡住：用户放弃，任务 "+k.ID+"）") || strings.Contains(h.backlog(), "（进行中：agent/x") {
		t.Errorf("条目要标卡住：\n%s", h.backlog())
	}
}

// 默认分支不是 main 的已有项目：（成品）条目走通，合进 master，有交付单。
func TestSmokeProductOnMaster(t *testing.T) {
	f := newFix(t, productBacklog)
	f.git("branch", "-q", "-m", "main", "master")
	f.def = "master"
	f.smokeScript("#!/bin/sh\nexit 0\n")
	j := f.claim("做第一版（成品）", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.DefaultBranch = "master"
	res := f.run(j)
	if r := smokeResult(t, res); res.Outcome != "merged" || r.Outcome != gate.Pass || !strings.Contains(strings.Join(r.Evidence, "\n"), "用 master 上的") {
		t.Fatalf("%+v", res)
	}
	if !f.isAncestor(res.Merge, "master") {
		t.Error("合进 master")
	}
	f.collect(j, res)
	if _, err := os.Stat(filepath.Join(f.a.Dir(j.ID), gate.DeliveryFile)); err != nil {
		t.Errorf("要有交付单：%v", err)
	}
}
