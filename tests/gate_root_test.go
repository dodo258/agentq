package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// gateRepo 建一个真 git 项目，待做里放 lines。
func gateRepo(t *testing.T, e *testEnv, name string, lines ...string) string {
	t.Helper()
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	dir := filepath.Join(e.root, name)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n"+strings.Join(lines, "\n")+"\n\n## 等用户决定\n\n## 已完成\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROGRESS.md"), []byte("# 进度\n\n## 当前任务\n无\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, gate.VerifyScript), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "init")
	return dir
}

func (e *testEnv) gateResult(j *Job) string {
	return filepath.Join(e.home, ".agentq", "gate", j.ID, "result.json")
}

// tickUntilGate 收尾任务、起门，等门的结果，再 tick 一次收结果。
func (e *testEnv) tickUntilGate(title string) *Job {
	e.t.Helper()
	j := e.jobByTitle(title)
	waitExit(e.t, j.PID, 20*time.Second)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateRunning {
		e.t.Fatalf("应该在过门：%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	waitFor(e.t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	return e.jobByTitle(title)
}

// 验收：假 claude 不合并也能进 main。任务只提交到分支写 ready.json，引擎合并并写收尾。
func TestIntegrationFakeNoMergeGetsToMain(t *testing.T) {
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt", "- [ ] 下一条")
	before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	if got := testkit.GitT(t, e.home, dir, "log", "-1", "--format=%s", "main"); !strings.HasPrefix(got, "待办：") {
		t.Fatalf("任务只提交了认领，不该合并：%s", got)
	}
	e.mustRun("pause") // 下一条的假 claude 不走这条路，这里只看第一条
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Status != state.StatusDone || j.Gate == nil || j.Gate.State != state.GateRunning || j.Gate.Branch != "agent/t-a" {
		t.Fatalf("%s %+v", j.Status, j.Gate)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "过门中：demo「修东西 READYMERGE」") {
		t.Errorf("status 要能看到：\n%s", out)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v", j.Gate)
	}
	if !strings.HasPrefix(j.Summary, "合并提交 ") {
		t.Errorf("摘要：%s", j.Summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Error("任务的改动没进主目录")
	}
	if _, err := os.Stat(filepath.Join(dir, gate.ReadyFile)); err == nil {
		t.Error("ready.json 不进 main")
	}
	short := textTrunc(j.Gate.Merge, 12)
	b := e.backlogOf(dir)
	if !strings.Contains(b, "- [x] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt；合并提交 "+short+"；验收：看 a.txt") || strings.Contains(b, "（进行中") || !strings.Contains(b, "- [ ] 下一条") {
		t.Errorf("%s", b)
	}
	if p := testkit.ReadFile(t, filepath.Join(dir, "PROGRESS.md")); !strings.Contains(p, "做好了 a.txt（"+short+"）") {
		t.Errorf("%s", p)
	}
	if got := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-a"); got != "" {
		t.Errorf("分支要删：%s", got)
	}
	if n := strings.Count(testkit.GitT(t, e.home, dir, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("worktree 要清：%d", n)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", before+"..main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
	if got := testkit.GitT(t, e.home, dir, "status", "--porcelain"); got != "" {
		t.Errorf("主目录要干净：%s", got)
	}
	if out := e.mustRun("gate", "report", j.ID); !strings.Contains(out, "已合并") || !strings.Contains(out, j.Gate.Merge) {
		t.Errorf("%s", out)
	}
	// meta.json 里也有，状态文件损坏重建后还认得
	raw, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j.ID, "meta.json"))
	var m Job
	if err := json.Unmarshal(raw, &m); err != nil || m.Gate == nil || m.Gate.Merge == "" {
		t.Errorf("%v %+v", err, m.Gate)
	}
}

func textTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// 门进程被 SIGKILL：状态还是在过门，下次 tick 重跑，只合并一次。这里让它卡在排 <项目>-main 锁上。
func TestIntegrationGateKilledReruns(t *testing.T) {
	e := newEnv(t, map[string]any{"max_parallel": 3})
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	lock := filepath.Join(e.home, ".claude", "state", "locks", "demo-main")
	os.MkdirAll(lock, 0o755)
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	pid := j.Gate.PID
	if j.Gate.State != state.GateRunning || pid <= 0 {
		t.Fatalf("%+v", j.Gate)
	}
	// tick 再来：门还拿着锁，不重起第二个
	e.mustRun("tick")
	if again := e.jobByTitle(title); again.Gate.PID != pid || again.Gate.State != state.GateRunning {
		t.Fatalf("%+v", again.Gate)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "门进程退出", func() bool { return !proc.ProcAlive(pid, gate.ProcToken+" "+j.ID) })
	e.mustRun("tick") // 收：没结果，排回去，同一次 tick 重起
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateRunning || j.Gate.Crashes != 1 || j.Gate.PID == pid {
		t.Fatalf("%+v", j.Gate)
	}
	os.RemoveAll(lock)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged || j.Gate.Crashes != 0 {
		t.Fatalf("%+v", j.Gate)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", "main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
}

// 门进程过了期限还在（Mac 合盖睡过、卡住）：tick 杀掉它，算一次没结果，排回去重跑，不记失败。
func TestIntegrationGateOverran(t *testing.T) {
	e := newEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	lock := filepath.Join(e.home, ".claude", "state", "locks", "demo-main")
	os.MkdirAll(lock, 0o755)
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	e.mustRun("tick")
	st := e.state()
	for _, x := range st.Jobs {
		if x.Gate != nil {
			x.Gate.Deadline = time.Now().Add(-time.Minute)
		}
	}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	pid := e.jobByTitle(title).Gate.PID
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.Crashes != 1 || j.Gate.PID == pid || proc.ProcAlive(pid, gate.ProcToken+" "+j.ID) {
		t.Fatalf("%+v", j.Gate)
	}
	os.RemoveAll(lock)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
}

// 状态文件损坏：过门的记录从 meta.json 重建，接着走，只合并一次。那次 tick 不起新的门。
func TestIntegrationGateSurvivesCorruptState(t *testing.T) {
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	os.WriteFile(filepath.Join(e.home, ".agentq", "state.json"), []byte("{坏了"), 0o644)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", "main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
}

// 状态文件损坏时门还在排队：重建后那次 tick 不起门（只收尾），下次接着走。
func TestIntegrationQueuedSurvivesCorruptState(t *testing.T) {
	e := newEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	os.WriteFile(filepath.Join(e.home, ".agentq", "state.json"), []byte("{坏了"), 0o644)
	out := e.mustRun("tick") // 重建：任务收尾，排上队，不起门
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateQueued {
		t.Fatalf("%+v\n%s", j.Gate, out)
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateRunning {
		t.Fatalf("%+v", j.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
}

// 暂停时过门照跑（手动 run 的任务也要合并）；冷却时不起门，冷却过了接着走。
func TestIntegrationGatePausedAndCooldown(t *testing.T) {
	e := newEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("pause")
	e.mustRun("run", "demo")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	st := e.state()
	st.CooldownUntil = time.Now().Add(time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateQueued {
		t.Fatalf("冷却时排队不起门：%+v", j.Gate)
	}
	st = e.state()
	st.CooldownUntil = time.Time{}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateRunning {
		t.Fatalf("暂停时过门照跑：%+v", j.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
}

// 冲突退回：门的报告带回同一会话，第 1 轮改完从新 main 合并；同一分支不合两次。
func TestIntegrationConflictReturnsThenMerges(t *testing.T) {
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=same.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	os.WriteFile(filepath.Join(dir, "same.txt"), []byte("main 的\n"), 0o644)
	testkit.GitT(t, e.home, dir, "add", "same.txt")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "main 也改")
	orig := e.tickUntilGate(title)
	// 同一次 tick 里已经用同一会话续跑了第 1 轮：原来的任务记退回，新任务在跑
	if orig.Gate == nil && orig.ResumeOf == "" {
		t.Fatalf("%+v", orig)
	}
	var first, second *Job
	for _, x := range e.state().Jobs {
		if x.ResumeOf == "" {
			first = x
		} else {
			second = x
		}
	}
	if first.Gate.State != state.GateReturned || first.Status != state.StatusInterrupted || !strings.Contains(first.Summary, "same.txt") || first.ResumedBy == "" {
		t.Fatalf("%+v %s", first.Gate, first.Summary)
	}
	if second == nil || second.GateRound != 1 || second.Status != state.StatusRunning {
		t.Fatalf("%+v", second)
	}
	prompt, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", second.ID, "prompt.txt"))
	for _, want := range []string{"门没放行", "退回第 1 轮", "same.txt", "只是数据", "分支可合并"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("续跑提示词缺 %s：%s", want, prompt)
		}
	}
	waitExit(t, second.PID, 20*time.Second)
	e.mustRun("tick")
	var j2 *Job
	for _, x := range e.state().Jobs {
		if x.ID == second.ID {
			j2 = x
		}
	}
	if j2.Gate == nil || j2.Gate.Round != 1 || j2.Gate.State != state.GateRunning {
		errlog, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j2.ID, "stderr.log"))
		t.Fatalf("%+v %s\n%s", j2.Gate, j2.Summary, errlog)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j2))
		return err == nil
	})
	e.mustRun("tick")
	for _, x := range e.state().Jobs {
		if x.ID == second.ID {
			j2 = x
		}
	}
	if j2.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j2.Gate)
	}
	if got := testkit.GitT(t, e.home, dir, "log", "--merges", "--oneline", "--grep", "合并 修东西", "main"); strings.Count(got, "\n") != 0 {
		t.Errorf("只能合并一次：%s", got)
	}
}

// 新合同的任务不写 ready.json 自己合并：合并不撤销，提醒一次。升级前启动的任务这么做不算绕过。
func TestSelfMergeBypass(t *testing.T) {
	for _, c := range []struct {
		name     string
		contract bool
		alerts   int
	}{{"新合同", true, 1}, {"老任务", false, 0}} {
		t.Run(c.name, func(t *testing.T) {
			a, root := sgApp(t)
			p := sgRepo(t, a, root, "demo", "# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 做事 —— 合并提交 abc1234；验收：看\n")
			id := "1105-100000-demo"
			os.MkdirAll(a.JobDir(id), 0o755)
			b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "做完了。\n\n合并提交 abc1234；用户看 README", "session_id": "s", "num_turns": 30})
			os.WriteFile(filepath.Join(a.JobDir(id), "result.json"), b, 0o644)
			j := &Job{ID: id, Project: p.Name, ProjectDir: p.Dir, Title: "做事", Item: "做事", Fingerprint: "fp", Status: state.StatusRunning, StartedAt: a.Now().Add(-time.Hour), SessionID: "s", GateContract: c.contract}
			st := &State{Jobs: []*Job{j}}
			a.Runner.FinishFromResult(st, j)
			if j.Status != state.StatusDone || j.Gate != nil {
				t.Fatalf("%s %+v", j.Status, j.Gate)
			}
			if got := len(pendingAlerts(t, a)); got != c.alerts {
				t.Errorf("提醒 %d 条，要 %d", got, c.alerts)
			}
		})
	}
}

// 老任务迁移：升级前已认领、按旧合同跑的任务，分支上没有 ready.json。收尾按中断走，用新合同续跑，写了 ready.json 就过门，不卡死、不重复合并。
func TestOldClaimedJobMigrates(t *testing.T) {
	a, root := sgApp(t)
	body := "# 待办\n\n## 待做\n\n- [ ] 老活 —— 要什么（进行中：agent/old 2026-11-04 10:00）\n\n## 已完成\n"
	p := sgRepo(t, a, root, "demo", body)
	testkit.GitT(t, a.Home, p.Dir, "branch", "agent/old")
	items, _ := p.Items()
	id := "1104-100000-demo"
	os.MkdirAll(a.JobDir(id), 0o755)
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "还在等审查", "session_id": "s-old", "num_turns": 30})
	os.WriteFile(filepath.Join(a.JobDir(id), "result.json"), b, 0o644)
	// 旧 meta：没有 gate_contract、没有 gate
	old := fmt.Sprintf(`{"id":%q,"project":"demo","project_dir":%q,"item":%q,"title":"老活","fingerprint":%q,"pid":0,"started_at":"2026-11-04T10:00:00+08:00","status":"running","session_id":"s-old"}`,
		id, p.Dir, items[0].Text, items[0].Fingerprint)
	var j Job
	if err := json.Unmarshal([]byte(old), &j); err != nil {
		t.Fatal(err)
	}
	st := &State{Jobs: []*Job{&j}}
	a.Runner.FinishFromResult(st, &j)
	if j.Status != state.StatusInterrupted || !j.ResumePending || j.Gate != nil || !strings.Contains(j.Summary, gate.ReadyFile) {
		t.Fatalf("%s %v %+v：%s", j.Status, j.ResumePending, j.Gate, j.Summary)
	}
	// 老任务已经自己合并并写了「已完成」：直接算完，不进门，不再合并一次
	a2, root2 := sgApp(t)
	p2 := sgRepo(t, a2, root2, "demo", "# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 老活 —— 合并提交 abc1234；验收：看\n")
	id2 := "1104-110000-demo"
	os.MkdirAll(a2.JobDir(id2), 0o755)
	r2, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "做完了。\n\n合并提交 abc1234", "session_id": "s", "num_turns": 30})
	os.WriteFile(filepath.Join(a2.JobDir(id2), "result.json"), r2, 0o644)
	var j2 Job
	json.Unmarshal([]byte(strings.ReplaceAll(old, id, id2)), &j2)
	j2.ProjectDir, j2.Title = p2.Dir, "老活"
	st2 := &State{Jobs: []*Job{&j2}}
	head := testkit.GitT(t, a2.Home, p2.Dir, "rev-parse", "main")
	a2.Runner.FinishFromResult(st2, &j2)
	if j2.Status != state.StatusDone || j2.Gate != nil || !strings.HasPrefix(j2.Summary, "合并提交 abc1234") || testkit.GitT(t, a2.Home, p2.Dir, "rev-parse", "main") != head {
		t.Fatalf("%s %+v %s", j2.Status, j2.Gate, j2.Summary)
	}
}

// 老记录带着核验字段：读得出、写回去不动，显示不变；运行中的老核验迁移后不再占「在跑」。
func TestOldVerifyRecordReadable(t *testing.T) {
	raw := `{"id":"x","project":"demo","project_dir":"/p","item":"i","title":"t","fingerprint":"f","pid":0,"started_at":"2026-11-04T10:00:00+08:00","status":"done","verify":{"commit":"abc","on_main":"yes","review":"no","script":"failed","failed":true,"regression":"修回归：abc"}}`
	var j Job
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	if state.JobLabel(&j) != "合并后核验失败" || !state.VerifyFailed(&j) {
		t.Errorf("%+v", j.Verify)
	}
	b, _ := json.Marshal(&j)
	if !strings.Contains(string(b), `"regression":"修回归：abc"`) || strings.Contains(string(b), `"gate"`) {
		t.Errorf("%s", b)
	}
}

// 孤儿扫描不碰在过门的条目：标着进行中，归门管。
func TestOrphanSkipsGating(t *testing.T) {
	a, root := sgApp(t)
	body := "# 待办\n\n## 待做\n\n- [ ] 活 —— 要什么（进行中：agent/x 2020-01-01 10:00）\n\n## 已完成\n"
	p := sgRepo(t, a, root, "demo", body)
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "-b", "agent/x")
	os.WriteFile(filepath.Join(p.Dir, "b.txt"), []byte("b"), 0o644)
	testkit.GitT(t, a.Home, p.Dir, "add", "-A")
	testkit.GitT(t, a.Home, p.Dir, "commit", "-q", "-m", "x")
	testkit.GitT(t, a.Home, p.Dir, "checkout", "-q", "main")
	items, _ := p.Items()
	real := a.Now
	a.Now = func() time.Time { return real().Add(72 * time.Hour) } // 标记、分支都算旧了，只有门的记录挡着
	ended := real()
	j := &Job{ID: "1105-100000-demo", Project: "demo", ProjectDir: p.Dir, Title: items[0].Title, Item: items[0].Text, Fingerprint: items[0].Fingerprint,
		Status: state.StatusDone, EndedAt: &ended, StartedAt: ended, Gate: &state.GateRecord{State: state.GateQueued, Branch: "agent/x"}}
	st := &State{Jobs: []*Job{j}}
	a.Cfg.ProjectsRoot = root
	if got := a.Orphan.ScanOrphans(st); len(got) != 0 {
		t.Fatalf("在排队的不算孤儿：%+v", got)
	}
	j.Gate.State = state.GateMerged // 合并了、收尾写完了，标记还在：回到孤儿规则
	if got := a.Orphan.ScanOrphans(st); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

// verifyRepo：main 上带这个门脚本的项目。
func verifyRepo(t *testing.T, e *testEnv, name, script string, lines ...string) string {
	t.Helper()
	dir := gateRepo(t, e, name, lines...)
	p := filepath.Join(dir, gate.VerifyScript)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "门脚本")
	return dir
}

// 门进程被 SIGKILL 时脚本还在跑：脚本也杀掉，worktree 清掉，重跑；只合并一次。
func TestIntegrationGateKilledDuringScript(t *testing.T) {
	e := newEnv(t, nil)
	dir := verifyRepo(t, e, "demo", "#!/bin/sh\n[ -f \"$HOME/放行\" ] && exit 0\ntouch \"$HOME/脚本开始了\"\nsleep 300\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "脚本开始", func() bool {
		_, err := os.Stat(filepath.Join(e.home, "脚本开始了"))
		return err == nil
	})
	gd := filepath.Join(e.home, ".agentq", "gate", j.ID)
	var pgid int
	waitFor(t, 10*time.Second, "pgid", func() bool {
		b, err := os.ReadFile(filepath.Join(gd, "pgid"))
		if err != nil {
			return false
		}
		pgid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pgid > 0
	})
	if err := syscall.Kill(j.Gate.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "门进程退出", func() bool { return !proc.ProcAlive(j.Gate.PID, gate.ProcToken+" "+j.ID) })
	os.WriteFile(filepath.Join(e.home, "放行"), nil, 0o644)
	e.mustRun("tick")
	waitFor(t, 15*time.Second, "脚本被杀", func() bool { return !proc.GroupAlive(pgid) })
	j = e.jobByTitle(title)
	if j.Gate.Crashes != 1 || j.Gate.State != state.GateRunning {
		t.Fatalf("%+v", j.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", "main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
	if n := strings.Count(testkit.GitT(t, e.home, dir, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("worktree 没清干净：%d", n)
	}
}

// 门进程 SIGTERM：自己清理干净再退出，不写结果，tick 重跑。
func TestIntegrationGateTerminated(t *testing.T) {
	e := newEnv(t, nil)
	verifyRepo(t, e, "demo", "#!/bin/sh\n[ -f \"$HOME/放行\" ] && exit 0\ntouch \"$HOME/脚本开始了\"\nsleep 300\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "脚本开始", func() bool {
		_, err := os.Stat(filepath.Join(e.home, "脚本开始了"))
		return err == nil
	})
	if err := syscall.Kill(j.Gate.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitExit(t, j.Gate.PID, 30*time.Second)
	gd := filepath.Join(e.home, ".agentq", "gate", j.ID)
	if _, err := os.Stat(filepath.Join(gd, "wt")); err == nil {
		t.Error("自己要清掉 worktree")
	}
	if _, err := os.Stat(filepath.Join(gd, "result.json")); err == nil {
		t.Error("被停了不写结果")
	}
	os.WriteFile(filepath.Join(e.home, "放行"), nil, 0o644)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	if j = e.jobByTitle(title); j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
}

// main 在门跑到一半时前进：用新 main 重建候选，测试门重跑，合并提交的第一父是新 main。
func TestIntegrationMainMovesDuringGate(t *testing.T) {
	e := newEnv(t, nil)
	dir := verifyRepo(t, e, "demo", "#!/bin/sh\necho x >> \"$HOME/跑过\"\nuntil [ -f \"$HOME/放行\" ]; do sleep 0.1; done\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "脚本开始", func() bool {
		_, err := os.Stat(filepath.Join(e.home, "跑过"))
		return err == nil
	})
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("别人的"), 0o644)
	testkit.GitT(t, e.home, dir, "add", "other.txt")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "别人的提交")
	moved := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	os.WriteFile(filepath.Join(e.home, "放行"), nil, 0o644)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-parse", j.Gate.Merge+"^1"); got != moved {
		t.Errorf("要在新 main 上：%s，%s", got, moved)
	}
	if b := testkit.ReadFile(t, filepath.Join(e.home, "跑过")); strings.Count(b, "x") != 2 {
		t.Errorf("测试门要重跑：%q", b)
	}
	for _, f := range []string{"a.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s 不在：%v", f, err)
		}
	}
}

// 连续失败退回：门一直不过，退回两轮后标卡住，发决定卡片，分支留着；同项目下一条照常做；用户放行后合并，写审计。
func TestIntegrationFailRoundsThenStuckThenPass(t *testing.T) {
	e := newEnv(t, map[string]any{"notify_enabled": false})
	dir := verifyRepo(t, e, "demo", "#!/bin/sh\necho 就是不过\nexit 1\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt", "- [ ] 下一条 NOMERGE")
	title := "修东西 READYMERGE"
	e.mustRun("tick")
	current := func() *Job {
		var last *Job
		for _, x := range e.state().Jobs {
			if x.Title == title && (last == nil || x.StartedAt.After(last.StartedAt)) {
				last = x
			}
		}
		return last
	}
	for round := 0; round <= gate.MaxRounds; round++ {
		j := current()
		if j.GateRound != round {
			t.Fatalf("第 %d 轮：%+v", round, j)
		}
		waitExit(t, j.PID, 20*time.Second)
		e.mustRun("tick")
		j = current()
		if j.Gate == nil || j.Gate.State != state.GateRunning {
			t.Fatalf("第 %d 轮要在过门：%+v %s", round, j.Gate, j.Summary)
		}
		waitFor(t, 30*time.Second, "门的结果", func() bool {
			_, err := os.Stat(e.gateResult(j))
			return err == nil
		})
		e.mustRun("tick")
	}
	var stuck *Job
	for _, x := range e.state().Jobs {
		if x.Title == title && x.Gate != nil && x.Gate.State == state.GateStuck {
			stuck = x
		}
	}
	if stuck == nil || stuck.Gate.Round != gate.MaxRounds {
		t.Fatalf("要卡住：%+v", e.state().Jobs)
	}
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（卡住：门没过") || !strings.Contains(b, "分支 agent/t-a") {
		t.Errorf("%s", b)
	}
	if got := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-a"); got == "" {
		t.Error("分支要留着")
	}
	// 下一条照常被挑
	if j := e.jobByTitle("下一条 NOMERGE"); j == nil {
		t.Fatal("卡住不挡下一条")
	}
	out := e.mustRun("gate", "report", stuck.ID)
	if !strings.Contains(out, "门没过，等你定") || !strings.Contains(out, "test-again") {
		t.Errorf("%s", out)
	}
	if st := e.mustRun("status"); !strings.Contains(st, "门没过，等你定") {
		t.Errorf("%s", st)
	}
	if _, code := e.run("gate", "pass", stuck.ID); code == 0 {
		t.Error("放行要写原因")
	}
	e.mustRun("gate", "pass", stuck.ID, "--reason", "脚本本身坏了，我看过改动")
	e.mustRun("tick")
	var again *Job
	for _, x := range e.state().Jobs {
		if x.ID == stuck.ID {
			again = x
		}
	}
	if again.Gate.State != state.GateRunning || again.Gate.Override == "" {
		t.Fatalf("%+v", again.Gate)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(again))
		return err == nil
	})
	e.mustRun("tick")
	for _, x := range e.state().Jobs {
		if x.ID == stuck.ID {
			again = x
		}
	}
	if again.Gate.State != state.GateMerged {
		t.Fatalf("%+v", again.Gate)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] 修东西 READYMERGE —— BRANCH=agent/t-a") {
		t.Errorf("%s", b)
	}
	if a := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "gate", "audit.log")); !strings.Contains(a, "脚本本身坏了") {
		t.Errorf("%s", a)
	}
	if n := e.nightly(); !strings.Contains(n, "放行") {
		t.Errorf("进当天汇总：%s", n)
	}
}

// 审查门真二进制走一遍：有证据的问题退回，提示词带报告；任务改完重审，通过才合并。审查进程挂了不放行。
func TestIntegrationReviewBlocksThenPasses(t *testing.T) {
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	block := `{"type":"result","subtype":"success","is_error":false,"result":"有问题","session_id":"s-rv","structured_output":{"verdict":"block","summary":"会崩","findings":[{"file":"a.txt","line":1,"severity":"blocker","evidence":"第一行会崩","repro":"跑 a.txt 就崩"}]}}` + "\n"
	review := filepath.Join(e.home, "fake-review.json")
	os.WriteFile(review, []byte(block), 0o644)
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	first := e.tickUntilGate(title)
	if first.Gate == nil || first.Gate.State != state.GateReturned || !strings.Contains(first.Summary, "第一行会崩") {
		t.Fatalf("%+v %s", first.Gate, first.Summary)
	}
	os.Remove(review)
	var second *Job
	for _, x := range e.state().Jobs {
		if x.ResumeOf != "" {
			second = x
		}
	}
	if second == nil {
		t.Fatalf("要用同一会话续跑：%+v", e.state().Jobs)
	}
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", second.ID, "prompt.txt"))
	for _, want := range []string{"第一行会崩", "a.txt:1", "跑 a.txt 就崩"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("提示词缺 %s：%s", want, prompt)
		}
	}
	waitExit(t, second.PID, 20*time.Second)
	e.mustRun("tick")
	jobByID := func(id string) *Job {
		for _, x := range e.state().Jobs {
			if x.ID == id {
				return x
			}
		}
		t.Fatalf("没有任务 %s", id)
		return nil
	}
	second = jobByID(second.ID)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(second))
		return err == nil
	})
	e.mustRun("tick")
	second = jobByID(second.ID)
	if second.Gate.State != state.GateMerged {
		t.Fatalf("%+v %s", second.Gate, second.Summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Error("没进主目录")
	}
	// 审查记录有两份，绑定 tree
	entries, err := os.ReadDir(gate.ReviewsDir(filepath.Join(e.home, ".agentq"), "demo"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("%v %v", entries, err)
	}
}

// 审查进程挂了：判不了，分支留着，等用户定，不进 main。
func TestIntegrationReviewCrashDoesNotMerge(t *testing.T) {
	e := newEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	os.WriteFile(filepath.Join(e.home, "fake-review.json"), []byte("{坏了\n"), 0o644)
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.tickUntilGate(title)
	if j.Gate == nil || j.Gate.State != state.GateStuck {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Error("审查没过不能进 main")
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：门没过") {
		t.Errorf("%s", b)
	}
	if got := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-a"); got == "" {
		t.Error("分支要留着")
	}
}

// 不能续跑的后端：门退回后由接手任务接着做，提示词带门的报告，轮数接着算。
func TestGateTakeoverCarriesReport(t *testing.T) {
	a, root := sgApp(t)
	p := sgRepo(t, a, root, "demo", "# 待办\n\n## 待做\n\n- [ ] 活 —— 要什么（进行中：agent/x 2026-11-05 10:00）\n\n## 已完成\n")
	items, _ := p.Items()
	it := items[0]
	ended := a.Now()
	last := &Job{ID: "1105-100000-demo", Project: "demo", ProjectDir: p.Dir, Title: it.Title, Item: it.Text, Fingerprint: it.Fingerprint, Status: state.StatusInterrupted, EndedAt: &ended, StartedAt: ended,
		Gate: &state.GateRecord{State: state.GateReturned, Branch: "agent/x", Round: 1, Report: "门的结论：测试没过"}}
	st := &State{Jobs: []*Job{last}}
	j := &Job{Project: "demo", ProjectDir: p.Dir, Title: it.Title, Item: it.Text, Fingerprint: it.Fingerprint}
	note := a.Runner.GateTakeoverNote(st, j, p, it)
	if !strings.Contains(note, "门的结论：测试没过") || !strings.Contains(note, "退回第 2 轮") || j.GateRound != 2 {
		t.Fatalf("%q %d", note, j.GateRound)
	}
	// 上一个任务不是门退回的：什么都不加
	last.Gate = nil
	j2 := &Job{}
	if note := a.Runner.GateTakeoverNote(st, j2, p, it); note != "" || j2.GateRound != 0 {
		t.Errorf("%q %d", note, j2.GateRound)
	}
}

// 任务写了 ready.json 又自己合并：门一道没跑，不能记成过门合并；提醒用户，main 上残留的 ready.json 删掉，不连累后面的任务。
func TestIntegrationSelfMergeWithReady(t *testing.T) {
	e := newEnv(t, nil)
	dir := verifyRepo(t, e, "demo", "#!/bin/sh\nexit 1\n", "- [ ] 修东西 READYMERGE SELFREADY —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE SELFREADY"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal("假 claude 应该自己合并了")
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateStuck || j.Gate.Merge != "" || !strings.Contains(j.Gate.Report, "不是引擎合的") {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	gd := filepath.Join(e.home, ".agentq", "gate", j.ID)
	if _, err := os.Stat(filepath.Join(gd, "test.log")); err == nil {
		t.Error("没过门的不用再跑")
	}
	if n := e.nightly(); strings.Contains(n, "过门合并") || !strings.Contains(n, "不是引擎合的") {
		t.Errorf("汇总要如实写：%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "门没过，等你定") {
		t.Errorf("%s", out)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：门没过") || strings.Contains(b, "- [x]") {
		t.Errorf("没过门不关条目：%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, gate.ReadyFile)); err == nil {
		t.Error("main 上残留的 ready.json 要删掉")
	}
	if got := testkit.GitT(t, e.home, dir, "status", "--porcelain", "--", gate.ReadyFile); got != "" {
		t.Errorf("删了要提交：%s", got)
	}
	// 用户看过了认这个合并：放行就只补收尾
	e.mustRun("gate", "pass", j.ID, "--reason", "我看过了")
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged || !strings.Contains(e.backlogOf(dir), "- [x] 修东西 READYMERGE SELFREADY") {
		t.Fatalf("%+v\n%s", j.Gate, e.backlogOf(dir))
	}
}

// 排队时别人把分支合了：同样不算过门。
func TestIntegrationBranchMergedWhileQueued(t *testing.T) {
	e := newEnv(t, nil)
	dir := verifyRepo(t, e, "demo", "#!/bin/sh\nexit 1\n", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	st := e.state()
	st.CooldownUntil = time.Now().Add(time.Hour)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick") // 收尾，排上队，冷却时不起门
	testkit.GitT(t, e.home, dir, "merge", "-q", "--no-edit", "agent/t-a")
	st = e.state()
	st.CooldownUntil = time.Time{}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateStuck || strings.Contains(e.nightly(), "过门合并") {
		t.Fatalf("%+v\n%s", j.Gate, e.nightly())
	}
	if _, err := os.Stat(filepath.Join(dir, gate.ReadyFile)); err == nil {
		t.Error("main 上残留的 ready.json 要删掉")
	}
}

// 审查进程认证失败：不算门的结论，不卡住、不发决定卡片，暂停这个后端（通知见 TestAuthFailureNotifiesAndPauses），过一会儿重跑。
func TestIntegrationReviewAuthDefers(t *testing.T) {
	e := newEnv(t, nil)
	gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	os.WriteFile(filepath.Join(e.home, "fake-review.json"), []byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Invalid API key · Please run /login","session_id":"s-rv"}`+"\n"), 0o644)
	e.mustRun("tick")
	j := e.tickUntilGate("修东西 READYMERGE")
	if j.Gate.State != state.GateQueued || j.Gate.NotBefore.IsZero() || strings.Contains(e.backlogOf(filepath.Join(e.root, "demo")), "（卡住") {
		t.Fatalf("%+v", j.Gate)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "backend-auth", "claude")); err != nil {
		t.Errorf("认证失败要暂停这个后端：%v", err)
	}
}

// extra_projects 登记的项目在 projects_root 外面，门和引擎合并照样走：按任务记的项目目录，不拼 projects_root。
func TestIntegrationExtraProjectThroughGate(t *testing.T) {
	outer, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(outer, "far-app")
	e := newEnv(t, map[string]any{"extra_projects": []string{dir}})
	title := "外面的活 READYMERGE"
	inRoot := gateRepo(t, e, "far-app", "- [ ] "+title+" —— BRANCH=agent/t-far FILE=far.txt")
	if err := os.Rename(inRoot, dir); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if j := e.jobByTitle(title); j.ProjectDir != dir {
		t.Fatalf("项目目录：%s", j.ProjectDir)
	}
	j := e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged || j.Gate.WrapErr != "" {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "far.txt")); err != nil {
		t.Error("任务的改动没进主目录")
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] "+title) {
		t.Errorf("条目没关：\n%s", b)
	}
	if out := testkit.GitT(t, e.home, dir, "branch", "--list", "agent/t-far"); out != "" {
		t.Errorf("分支要删：%s", out)
	}
}
