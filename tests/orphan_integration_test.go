package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 集成：真二进制、假 claude、真 git 临时仓库。AGENTQ_TEST_NOW 把 tick 的「现在」拨到以后，真实提交和文件就都算旧了。

// orphanRepo：projects_root 下的真 git 仓库，主目录在 main，.claude/ 不进版本库（和 worktree 一个位置）。
func orphanRepo(t *testing.T, e *testEnv, name, todo string) string {
	t.Helper()
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	dir := testkit.MkProject(t, e.root, name, todo)
	os.RemoveAll(filepath.Join(dir, ".git"))
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/\n"), 0o644)
	testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, e.home, dir, "add", ".")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "init")
	return dir
}

// gitBranch 建分支和 worktree，在分支上提交一个文件。
func gitBranch(t *testing.T, e *testEnv, dir, branch string) string {
	t.Helper()
	wt := filepath.Join(dir, ".claude", "worktrees", filepath.Base(branch))
	testkit.GitT(t, e.home, dir, "worktree", "add", "-q", wt, "-b", branch, "main")
	os.WriteFile(filepath.Join(wt, "work.txt"), []byte("x\n"), 0o644)
	testkit.GitT(t, e.home, wt, "add", ".")
	testkit.GitT(t, e.home, wt, "commit", "-q", "-m", "work")
	return wt
}

// tickAt 在 base+d 这个「现在」跑一次 tick。
func (e *testEnv) tickAt(base time.Time, d time.Duration) string {
	e.t.Helper()
	saved := e.extra
	defer func() { e.extra = saved }()
	e.extra = append(append([]string(nil), saved...), "AGENTQ_TEST_NOW="+base.Add(d).Format(time.RFC3339))
	return e.mustRun("tick")
}

func (e *testEnv) backlogOf(dir string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func oldMarker(branch string) string { return "（进行中：" + branch + " 2026-01-01 10:00）" }

// 接手 master 项目的任务：提示词写明默认分支。
func TestIntegrationOrphanTakeoverOnMaster(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— NOMERGE"+oldMarker("agent/x")+"\n")
	gitBranch(t, e, dir, "agent/x")
	testkit.GitT(t, e.home, dir, "branch", "-q", "-m", "main", "master")
	if out := e.tickAt(time.Now().Add(72*time.Hour), 0); !strings.Contains(out, "接手任务") {
		t.Fatalf("应启动接手任务：\n%s", out)
	}
	j := e.jobByTitle("甲")
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "agent/x", "默认分支是 master")
	waitExit(t, j.PID, 10*time.Second)
}

// 接手任务：写明分支、worktree、从现状接着做；没做完不续跑、不再接手，标卡住；分支和 worktree 留着。
func TestIntegrationOrphanTakeoverThenStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— NOMERGE"+oldMarker("agent/x")+"\n")
	wt := gitBranch(t, e, dir, "agent/x")
	base := time.Now().Add(72 * time.Hour)

	if out := e.tickAt(base, 0); !strings.Contains(out, "启动 1") || !strings.Contains(out, "接手任务") {
		t.Fatalf("应启动接手任务：\n%s", out)
	}
	j := e.jobByTitle("甲")
	if !j.Takeover || j.ResumeOf != "" || j.Status != state.StatusRunning {
		t.Fatalf("接手任务记录：%+v", j)
	}
	prompt, err := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j.ID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent/x", filepath.Base(wt), "从现状接着做", "不重做"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt 应写明 %q：%s", want, prompt)
		}
	}
	if !strings.Contains(e.nightly(), "接手") && !strings.Contains(readNightly(e, base), "接手") {
		t.Errorf("当天汇总应记一行")
	}
	waitExit(t, j.PID, 10*time.Second)

	out := e.tickAt(base, time.Minute)
	if !strings.Contains(out, "收尾 1") || !strings.Contains(out, "启动 0") {
		t.Fatalf("应收尾、不再启动：\n%s", out)
	}
	j = e.jobByTitle("甲")
	if j.Status != state.StatusInterrupted || j.ResumePending || !strings.Contains(j.Summary, "接手后仍未完成") {
		t.Fatalf("接手没做完不续跑：%+v", j)
	}
	b := e.backlogOf(dir)
	if !strings.Contains(b, "（卡住：接手后仍未完成，分支 agent/x）") || strings.Contains(b, "（进行中") {
		t.Fatalf("应标卡住：\n%s", b)
	}
	if st := testkit.GitT(t, e.home, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("BACKLOG 应已提交：%s", st)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("worktree 不该被删")
	}
	if out := e.tickAt(base, 48*time.Hour); !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 1 {
		t.Fatalf("同一指纹只接手 1 次：\n%s\n%+v", out, e.state().Jobs)
	}
}

func readNightly(e *testEnv, t time.Time) string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "nightly", t.Format("2006-01-02")+"-agentq.md"))
	return string(b)
}

// 接手任务做完：不标卡住，不再启动。
func TestIntegrationOrphanTakeoverDone(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— ORPHANDONE"+oldMarker("agent/x")+"\n")
	gitBranch(t, e, dir, "agent/x")
	base := time.Now().Add(72 * time.Hour)
	e.tickAt(base, 0)
	j := e.jobByTitle("甲")
	waitExit(t, j.PID, 10*time.Second)
	if out := e.tickAt(base, time.Minute); !strings.Contains(out, "启动 0") {
		t.Fatalf("\n%s", out)
	}
	j = e.jobByTitle("甲")
	if j.Status != state.StatusDone {
		t.Fatalf("应做完：%+v", j)
	}
	if b := e.backlogOf(dir); strings.Contains(b, "卡住") || !strings.Contains(b, "- [x] 甲") {
		t.Fatalf("\n%s", b)
	}
}

// 接手任务撞上限：冷却过后用同一会话续跑，续跑的还是接手任务；做完不标卡住。
func TestIntegrationOrphanTakeoverLimitResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— LIMITMID"+oldMarker("agent/x")+"\n")
	gitBranch(t, e, dir, "agent/x")
	base := time.Now().Add(72 * time.Hour)
	e.tickAt(base, 0)
	first := e.jobByTitle("甲")
	waitExit(t, first.PID, 10*time.Second)
	if out := e.tickAt(base, time.Minute); !strings.Contains(out, "用量冷却中") {
		t.Fatalf("撞上限应冷却：\n%s", out)
	}
	first = e.jobByTitle("甲")
	if !first.ResumePending || !first.LimitHit || !first.Takeover {
		t.Fatalf("撞上限应等续跑：%+v", first)
	}
	if strings.Contains(e.backlogOf(dir), "卡住") {
		t.Fatal("等续跑时不该标卡住")
	}
	if out := e.tickAt(base, 2*time.Hour); !strings.Contains(out, "续跑任务") {
		t.Fatalf("冷却过后应续跑：\n%s", out)
	}
	jobs := e.state().Jobs
	if len(jobs) != 2 || !jobs[1].Takeover || jobs[1].ResumeOf != first.ID {
		t.Fatalf("续跑要带接手标记：%+v", jobs)
	}
	waitExit(t, jobs[1].PID, 10*time.Second)
	if out := e.tickAt(base, 2*time.Hour+time.Minute); !strings.Contains(out, "启动 0") {
		t.Fatalf("\n%s", out)
	}
	if b := e.backlogOf(dir); strings.Contains(b, "卡住") || !strings.Contains(b, "- [x] 甲") {
		t.Fatalf("\n%s", b)
	}
}

// 用户 agentq stop 的任务：标记留着，24 小时后也不自动拉起；status 列一行、日志只一次；删标记后恢复。
func TestIntegrationOrphanStopped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"timeout": "1000h"})
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— CLAIMFIRST SLEEP=30\n")
	base := time.Now().Add(72 * time.Hour)
	e.mustRun("tick")
	j := e.jobByTitle("甲")
	waitFor(t, 10*time.Second, "认领写下标记", func() bool { return strings.Contains(e.backlogOf(dir), "（进行中") })
	testkit.GitT(t, e.home, dir, "add", ".")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "认领")
	gitBranch(t, e, dir, "agent/t-cf")
	e.mustRun("stop", j.ID)
	if st := e.jobByTitle("甲"); st.Status != state.StatusKilled {
		t.Fatalf("%+v", st)
	}
	before := e.backlogOf(dir)
	first := e.tickAt(base, 0)
	second := e.tickAt(base, time.Hour)
	if !strings.Contains(first, "被停止") || strings.Contains(second, "被停止") {
		t.Fatalf("日志只记一次：\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(first, "启动 0") || !strings.Contains(second, "启动 0") || len(e.state().Jobs) != 1 || e.backlogOf(dir) != before {
		t.Fatalf("不该接手、不该改标记：\n%s\n%s\n%s", first, second, e.backlogOf(dir))
	}
	if out := e.mustRun("status"); !strings.Contains(out, "已停止：demo「甲」") || !strings.Contains(out, j.ID) {
		t.Fatalf("status 要列一行：\n%s", out)
	}
	// 标记还在时 run 挑不到这一条（去掉标记才行）
	if out, code := e.run("run", "demo", "甲"); code == 0 || !strings.Contains(out, "没有包含") {
		t.Fatalf("run 应拒绝：%d\n%s", code, out)
	}
	// 用户删标记：恢复成普通条目，到 retry_after 后 tick 接着挑，status 不再列
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(strings.Replace(before, "（进行中：agent/t-cf 2026-01-01 10:00）", "", 1)), 0o644)
	testkit.GitT(t, e.home, dir, "add", ".")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "用户去掉标记")
	if out := e.tickAt(base, 2*time.Hour); !strings.Contains(out, "启动 1") {
		t.Fatalf("删标记后应接着挑：\n%s", out)
	}
	if out := e.mustRun("status"); strings.Contains(out, "已停止：demo") {
		t.Fatalf("不该再列：\n%s", out)
	}
	if st := e.state(); st.Jobs[0].StopNoted {
		t.Fatalf("应解除：%+v", st.Jobs[0])
	}
	e.mustRun("stop", e.state().Jobs[1].ID)
}

// 冷却、熔断时 tick 走不到启动那段的保存：已报过的停止记录也要留住，不然每次 tick 都记一行。
func TestIntegrationOrphanStoppedLogOnceInCooldown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲"+oldMarker("agent/x")+"\n")
	base := time.Now().Add(72 * time.Hour)
	ended := base.Add(-48 * time.Hour)
	jobID := "1104-000000-demo"
	os.MkdirAll(filepath.Join(e.home, ".agentq", "jobs", jobID), 0o755)
	fp := backlog.ParseBacklog(e.backlogOf(dir))[0].Fingerprint
	st := &State{CooldownUntil: base.Add(24 * time.Hour), Jobs: []*Job{{ID: jobID, Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: fp, Status: state.StatusKilled, PID: 1, StartedAt: ended, EndedAt: &ended}}}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	if out := e.tickAt(base, 0); !strings.Contains(out, "被停止") {
		t.Fatalf("第一次要记：\n%s", out)
	}
	if out := e.tickAt(base, time.Hour); strings.Contains(out, "被停止") {
		t.Fatalf("只记一次：\n%s", out)
	}
}

// 接手过的条目用户重新认领：tick 不标卡住、不接手。
func TestIntegrationOrphanReclaimed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— NOMERGE"+oldMarker("agent/x")+"\n")
	gitBranch(t, e, dir, "agent/x")
	base := time.Now().Add(72 * time.Hour)
	// 真跑接手任务时，收尾和标卡住在同一次 tick，插不进用户的认领；用状态文件造出「接手任务 2 天前结束，用户 1 小时前重新认领」。
	ended := base.Add(-48 * time.Hour)
	fp := backlog.ParseBacklog(e.backlogOf(dir))[0].Fingerprint
	jobID := "1104-000000-demo"
	os.MkdirAll(filepath.Join(e.home, ".agentq", "jobs", jobID), 0o755)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), &State{Jobs: []*Job{{ID: jobID, Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: fp, Status: state.StatusInterrupted, Takeover: true, PID: 1, StartedAt: ended, EndedAt: &ended}}}); err != nil {
		t.Fatal(err)
	}
	claim := "（进行中：agent/x " + base.Add(-time.Hour).Format("2006-01-02 15:04") + "）"
	text := strings.Replace(e.backlogOf(dir), oldMarker("agent/x"), claim, 1)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644)
	testkit.GitT(t, e.home, dir, "add", ".")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "用户认领")
	if out := e.tickAt(base, 0); !strings.Contains(out, "启动 0") || e.backlogOf(dir) != text {
		t.Fatalf("用户重新认领后 24 小时内不动：\n%s\n%s", out, e.backlogOf(dir))
	}
	if out := e.tickAt(base, 25*time.Hour); !strings.Contains(out, "启动 0") || !strings.Contains(e.backlogOf(dir), "（卡住：接手后仍未完成") {
		t.Fatalf("过了 24 小时才标卡住：\n%s\n%s", out, e.backlogOf(dir))
	}
}

// 分支没内容（多半已合并）：去掉标记后这一轮不重做，等 retry_after；冷却时也记得住；只有空标记的同一次就能挑。
func TestIntegrationOrphanMergedWaits(t *testing.T) {
	t.Parallel()
	t.Run("已合并的分支", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— 说明"+oldMarker("agent/x")+"\n")
		gitBranch(t, e, dir, "agent/x")
		testkit.GitT(t, e.home, dir, "merge", "-q", "--no-ff", "-m", "合并", "agent/x")
		base := time.Now().Add(72 * time.Hour)
		out := e.tickAt(base, 0)
		if !strings.Contains(out, "启动 0") || len(e.state().Jobs) != 0 || strings.Contains(e.backlogOf(dir), "（进行中") {
			t.Fatalf("去掉标记、这一轮不重做：\n%s\n%s", out, e.backlogOf(dir))
		}
		if out := e.tickAt(base, 11*time.Hour); !strings.Contains(out, "启动 0") {
			t.Fatalf("retry_after 内不做：\n%s", out)
		}
		if out := e.tickAt(base, 13*time.Hour); !strings.Contains(out, "启动 1") {
			t.Fatalf("retry_after 后回到待做：\n%s", out)
		}
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
	})
	t.Run("分支已删", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— 说明"+oldMarker("agent/gone")+"\n")
		base := time.Now().Add(72 * time.Hour)
		if out := e.tickAt(base, 0); !strings.Contains(out, "启动 0") || strings.Contains(e.backlogOf(dir), "（进行中") {
			t.Fatalf("\n%s", out)
		}
	})
	t.Run("用量冷却时等待记录也留住", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— 说明"+oldMarker("agent/gone")+"\n")
		base := time.Now().Add(72 * time.Hour)
		if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), &State{CooldownUntil: base.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		e.tickAt(base, 0)
		if strings.Contains(e.backlogOf(dir), "（进行中") || len(e.state().ItemHold) != 1 {
			t.Fatalf("%+v\n%s", e.state(), e.backlogOf(dir))
		}
		if out := e.tickAt(base, 2*time.Hour); !strings.Contains(out, "启动 0") {
			t.Fatalf("冷却结束也要等 retry_after：\n%s", out)
		}
	})
	t.Run("只有空标记", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		orphanRepo(t, e, "demo", "- [ ] 甲 —— 说明（进行中）\n")
		if out := e.tickAt(time.Now().Add(72*time.Hour), 0); !strings.Contains(out, "启动 1") {
			t.Fatalf("空标记去掉后同一次就能挑：\n%s", out)
		}
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
	})
	t.Run("agentq run 不受等待影响", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		orphanRepo(t, e, "demo", "- [ ] 甲 —— 说明"+oldMarker("agent/gone")+"\n")
		e.tickAt(time.Now().Add(72*time.Hour), 0)
		e.mustRun("run", "demo", "甲")
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
	})
}

// 不该动的时候：暂停、24 小时内有活动。
func TestIntegrationOrphanGates(t *testing.T) {
	t.Parallel()
	t.Run("暂停时什么都不做", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, map[string]any{"paused": true})
		dir := orphanRepo(t, e, "demo", "- [ ] 甲"+oldMarker("agent/x")+"\n- [ ] 乙（进行中）\n")
		gitBranch(t, e, dir, "agent/x")
		before := e.backlogOf(dir)
		if out := e.tickAt(time.Now().Add(72*time.Hour), 0); !strings.Contains(out, "已暂停") {
			t.Fatalf("\n%s", out)
		}
		if e.backlogOf(dir) != before || len(e.state().Jobs) != 0 {
			t.Fatalf("暂停时不动：\n%s", e.backlogOf(dir))
		}
	})
	t.Run("分支刚提交过", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		dir := orphanRepo(t, e, "demo", "- [ ] 甲"+oldMarker("agent/x")+"\n")
		gitBranch(t, e, dir, "agent/x")
		before := e.backlogOf(dir)
		if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") {
			t.Fatalf("\n%s", out)
		}
		if e.backlogOf(dir) != before || len(e.state().Jobs) != 0 {
			t.Fatalf("有活动时不动：\n%s", e.backlogOf(dir))
		}
	})
	t.Run("目录不是真仓库", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		dir := testkit.MkProject(t, e.root, "demo", "- [ ] 甲"+oldMarker("agent/x")+"\n")
		before := e.backlogOf(dir)
		out := e.mustRun("tick")
		if e.backlogOf(dir) != before || len(e.state().Jobs) != 0 || !strings.Contains(out, "查不了") {
			t.Fatalf("查不了 git 时不动：\n%s", out)
		}
	})
}

// 受名额、冷却、熔断、登录限制：能处理的标记照样处理，接手等条件满足。
func TestIntegrationOrphanTakeoverLimits(t *testing.T) {
	t.Parallel()
	base := time.Now().Add(72 * time.Hour)
	setup := func(t *testing.T, cfg map[string]any) (*testEnv, string) {
		e := newEnv(t, cfg)
		dir := orphanRepo(t, e, "demo", "- [ ] 甲 —— ORPHANDONE"+oldMarker("agent/x")+"\n")
		gitBranch(t, e, dir, "agent/x")
		return e, dir
	}
	writeState := func(t *testing.T, e *testEnv, st *State) {
		if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("用量冷却", func(t *testing.T) {
		t.Parallel()
		e, _ := setup(t, nil)
		writeState(t, e, &State{CooldownUntil: base.Add(time.Hour)})
		if out := e.tickAt(base, 0); !strings.Contains(out, "用量冷却中") || len(e.state().Jobs) != 0 {
			t.Fatalf("\n%s", out)
		}
		if out := e.tickAt(base, 2*time.Hour); !strings.Contains(out, "接手任务") {
			t.Fatalf("冷却过后应接手：\n%s", out)
		}
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
	})
	t.Run("连环失败熔断", func(t *testing.T) {
		t.Parallel()
		e, _ := setup(t, nil)
		writeState(t, e, &State{BreakerUntil: base.Add(time.Hour), BreakerLevel: 1})
		if out := e.tickAt(base, 0); !strings.Contains(out, "连环失败熔断中") || len(e.state().Jobs) != 0 {
			t.Fatalf("\n%s", out)
		}
	})
	t.Run("没登录", func(t *testing.T) {
		t.Parallel()
		e, dir := setup(t, nil)
		before := e.backlogOf(dir)
		e.extra = append(e.extra, "AGENTQ_FAKE_LOGGED_OUT=1")
		if out := e.tickAt(base, 0); !strings.Contains(out, "未登录") || len(e.state().Jobs) != 0 {
			t.Fatalf("\n%s", out)
		}
		if e.backlogOf(dir) != before {
			t.Fatal("没接手不改 BACKLOG")
		}
	})
	t.Run("名额满", func(t *testing.T) {
		t.Parallel()
		e, _ := setup(t, map[string]any{"max_parallel": 1, "timeout": "1000h"})
		testkit.MkProject(t, e.root, "other", "- [ ] 别的 —— SLEEP=30\n")
		// 真实时间 tick：demo 的分支刚提交过，不接手，名额给 other
		e.mustRun("tick")
		busy := e.jobByTitle("别的")
		if out := e.tickAt(base, 0); !strings.Contains(out, "启动 0") || e.findJob("甲") != nil {
			t.Fatalf("名额满不接手：\n%s", out)
		}
		e.mustRun("stop", busy.ID)
		if out := e.tickAt(base, time.Minute); !strings.Contains(out, "接手任务") {
			t.Fatalf("有名额了应接手：\n%s", out)
		}
	})
}

// 真实 422：15 分钟、1 小时重试；用完那次才计连环失败，再等 retry_after。
func TestIntegrationTransientRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— APIERR422\n")
	base := time.Now()
	startedAt := func(d time.Duration, want int) {
		t.Helper()
		out := e.tickAt(base, d)
		jobs := e.state().Jobs
		if len(jobs) != want {
			t.Fatalf("+%s 应有 %d 个任务，有 %d 个：\n%s", d, want, len(jobs), out)
		}
		if j := jobs[len(jobs)-1]; j.Status == state.StatusRunning {
			waitExit(t, j.PID, 10*time.Second)
		}
	}
	startedAt(0, 1)
	startedAt(time.Minute, 1) // 收尾
	j := e.jobByTitle("甲")
	if !j.Transient || j.Status != state.StatusFailed || !strings.Contains(j.Summary, "15m 后重试") {
		t.Fatalf("第一次：%+v", j)
	}
	if st := e.state(); st.FailStreak != 0 || !st.CooldownUntil.IsZero() {
		t.Fatalf("不该计连环失败、不该冷却：%+v", st)
	}
	startedAt(14*time.Minute, 1)
	startedAt(17*time.Minute, 2) // 第一次重试
	startedAt(18*time.Minute, 2) // 收尾
	startedAt(60*time.Minute, 2) // 1 小时内不试
	startedAt(80*time.Minute, 3) // 第二次重试
	startedAt(81*time.Minute, 3) // 收尾
	st := e.state()
	last := st.Jobs[2]
	if last.Transient || !strings.Contains(last.Summary, "已重试 2 次") || st.FailStreak != 1 {
		t.Fatalf("重试用完应回到普通失败：%+v；streak %d", last, st.FailStreak)
	}
	startedAt(6*time.Hour, 3)
	startedAt(14*time.Hour, 4) // retry_after 12 小时后
}

// 真实 403 不短退避，计连环失败；429 进用量冷却。
func TestIntegrationAPIErrorsOther(t *testing.T) {
	t.Parallel()
	t.Run("403", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— APIERR403\n")
		base := time.Now()
		e.tickAt(base, 0)
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
		e.tickAt(base, time.Minute)
		j, st := e.jobByTitle("甲"), e.state()
		if j.Transient || j.LimitHit || st.FailStreak != 1 || !st.CooldownUntil.IsZero() {
			t.Fatalf("%+v streak=%d", j, st.FailStreak)
		}
		e.tickAt(base, 20*time.Minute)
		if len(e.state().Jobs) != 1 {
			t.Fatal("403 不短退避")
		}
	})
	t.Run("429", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		testkit.MkProject(t, e.root, "demo", "- [ ] 甲 —— APIERR429\n")
		base := time.Now()
		e.tickAt(base, 0)
		waitExit(t, e.jobByTitle("甲").PID, 10*time.Second)
		out := e.tickAt(base, time.Minute)
		j, st := e.jobByTitle("甲"), e.state()
		if !j.LimitHit || st.FailStreak != 0 || st.CooldownUntil.IsZero() || !strings.Contains(out, "用量冷却中") {
			t.Fatalf("%+v %+v\n%s", j, st, out)
		}
	})
	t.Run("529 三连不熔断", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, map[string]any{"per_project": 3, "max_parallel": 3})
		testkit.MkProject(t, e.root, "a", "- [ ] 甲 —— APIERR529\n")
		testkit.MkProject(t, e.root, "b", "- [ ] 乙 —— APIERR529\n")
		testkit.MkProject(t, e.root, "c", "- [ ] 丙 —— APIERR529\n")
		base := time.Now()
		e.tickAt(base, 0)
		for _, title := range []string{"甲", "乙", "丙"} {
			waitExit(t, e.jobByTitle(title).PID, 10*time.Second)
		}
		e.tickAt(base, time.Minute)
		st := e.state()
		if st.FailStreak != 0 || !st.BreakerUntil.IsZero() {
			t.Fatalf("瞬时错误不该熔断：%+v", st)
		}
	})
}
