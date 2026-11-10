package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/routine"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 例行任务无事跳过。全部用临时 HOME、真 git 临时仓库、假 claude、假 gh。

// atSign：假远程地址里的 @，写法同 privacy_test.go（仓库扫描把「x@y.z」当邮箱）。
const atSign = string(rune(64))

const fakeGH = `#!/bin/sh
echo "$*" >> "$HOME/fake-gh-calls"
if [ -f "$HOME/fake-gh-fail" ]; then echo boom >&2; exit 1; fi
if [ -f "$HOME/fake-gh-gone" ]; then echo "GraphQL: Could not resolve to a Repository with the name 'o/demo'. (repository)" >&2; exit 1; fi
if [ -f "$HOME/fake-gh.json" ]; then cat "$HOME/fake-gh.json"; else echo '[]'; fi
`

// idleEnv：一个真 git 项目（待做空、有 github 远程）、假 gh。at 是当天 kind 的定点时刻。
func idleEnv(t *testing.T, kind string) (*testEnv, string, time.Time) {
	t.Helper()
	key := map[string]string{config.KindNight: "night_enabled", config.KindReview: "review_enabled"}[kind]
	e := newEnv(t, map[string]any{key: true})
	dir := orphanRepo(t, e, "demo", "")
	testkit.GitT(t, e.home, dir, "remote", "add", "backup", "git"+atSign+"github.com:o/demo.git")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	e.extra = append(e.extra, "PATH="+bin+":"+os.Getenv("PATH"))
	hh, mm := config.Routines[kind].Hour, config.Routines[kind].Minute
	// 拨到过去：报告是真实时间写的，修改时间要晚于（拨过的）任务启动时间才算这次写的
	return e, dir, time.Date(2026, 10, 5, hh, mm, 0, 0, time.Local)
}

func (e *testEnv) runAt(at time.Time, args ...string) string {
	e.t.Helper()
	saved := e.extra
	defer func() { e.extra = saved }()
	e.extra = append(append([]string(nil), saved...), "AGENTQ_TEST_NOW="+at.Format(time.RFC3339))
	return e.mustRun(args...)
}

func (e *testEnv) reportPath(kind string, at time.Time) string {
	return filepath.Join(e.home, ".claude", "nightly", at.Format("2006-01-02")+config.Routines[kind].Report+".md")
}

func (e *testEnv) claudeCalls() []string {
	b, err := os.ReadFile(filepath.Join(e.home, "fake-claude-calls"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// baselineRoutine 定点跑一次（第一次没有记录，照跑），跑完收尾，清掉调用记录。
func (e *testEnv) baselineRoutine(kind string, at time.Time) *Job {
	e.t.Helper()
	saved := e.extra
	e.extra = append(append([]string(nil), saved...), "AGENTQ_FAKE_WRITE="+e.reportPath(kind, at))
	defer func() { e.extra = saved }()
	if out := e.runAt(at, kind, "--scheduled"); !strings.Contains(out, "照跑：没有上次的记录") || !strings.Contains(out, "已启动") {
		e.t.Fatalf("第一次没有记录，应照跑：\n%s", out)
	}
	j := e.routineJob(kind)
	waitExit(e.t, j.PID, 10*time.Second)
	e.runAt(at.Add(time.Minute), "tick")
	j = e.routineJob(kind)
	if j.Status != state.StatusDone {
		e.e2eFail("基线应跑完", j)
	}
	if st := e.state(); st.RoutineMarks[kind] == nil || st.RoutineMarks[kind].JobID != j.ID {
		e.t.Fatalf("应记下现状：%+v", st.RoutineMarks)
	}
	os.Remove(filepath.Join(e.home, "fake-claude-calls"))
	return j
}

func (e *testEnv) e2eFail(why string, j *Job) {
	e.t.Helper()
	e.t.Fatalf("%s：%+v", why, j)
}

// assertIdle：定点叫醒一个进程都不起，不新增任务，写占位报告、记一行日志。
func (e *testEnv) assertIdle(kind string, at time.Time) {
	e.t.Helper()
	before := len(e.routineJobs(kind))
	out := e.runAt(at, kind, "--scheduled")
	if !strings.Contains(out, "确认没有新东西，跳过，不起 claude") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		e.t.Errorf("跳过时只记一行日志：\n%s", out)
	}
	if calls := e.claudeCalls(); len(calls) != 0 {
		e.t.Errorf("不该起任何 claude 进程：%v", calls)
	}
	if n := len(e.routineJobs(kind)); n != before {
		e.t.Errorf("不该有新任务：%d → %d", before, n)
	}
	b, err := os.ReadFile(e.reportPath(kind, at))
	if err != nil || !strings.Contains(string(b), "没起模型") {
		e.t.Errorf("应写占位报告：%v %s", err, b)
	}
}

// assertRuns：有事时和原来一样：启动 claude，参数、提示词不变。
func (e *testEnv) assertRuns(kind string, at time.Time, why string) *Job {
	e.t.Helper()
	before := len(e.routineJobs(kind))
	out := e.runAt(at, kind, "--scheduled")
	if !strings.Contains(out, "已启动：任务") {
		e.t.Fatalf("有事应照跑：\n%s", out)
	}
	if why != "" && !strings.Contains(out, why) {
		e.t.Errorf("日志应写原因 %q：\n%s", why, out)
	}
	jobs := e.routineJobs(kind)
	if len(jobs) != before+1 {
		e.t.Fatalf("应新增一个任务：%+v", jobs)
	}
	j := jobs[len(jobs)-1]
	prompt, err := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if err != nil || string(prompt) != routine.RoutinePromptPrefix+routineText[config.Routines[kind].Kind]+"\n" {
		e.t.Errorf("提示词应和原来逐字相同：%v\n%s", err, prompt)
	}
	waitExit(e.t, j.PID, 10*time.Second)
	if _, err := os.Stat(e.reportPath(kind, at)); err == nil {
		e.t.Errorf("有事时不该写占位报告")
	}
	return j
}

func commitFile(t *testing.T, e *testEnv, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, dir, "add", ".")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "改 "+name)
}

func (e *testEnv) editState(fn func(st *State)) {
	e.t.Helper()
	st := e.state()
	fn(st)
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		e.t.Fatal(err)
	}
}

// 夜班无事：一个进程都不起；gh 查了（只读、参数对）；补跑、手动都对。
func TestIntegrationNightIdle(t *testing.T) {
	t.Parallel()
	e, dir, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	os.Remove(filepath.Join(e.home, "fake-gh-calls"))
	day2 := day1.AddDate(0, 0, 1)
	e.assertIdle(config.KindNight, day2)
	gh, _ := os.ReadFile(filepath.Join(e.home, "fake-gh-calls"))
	if string(gh) != "issue list --repo o/demo --state all --limit 1 --search sort:updated-desc --json updatedAt\n" {
		t.Errorf("gh 调用：%q", gh)
	}
	// 只改 LESSONS.md（复盘自己的提交）不算有事
	commitFile(t, e, dir, "LESSONS.md", "# 教训\n")
	e.assertIdle(config.KindNight, day2.AddDate(0, 0, 1))
	// 手动不受影响：照样启动，prompt 一样
	day4 := day2.AddDate(0, 0, 2)
	out := e.runAt(day4, "night")
	if !strings.Contains(out, "夜班已启动") || strings.Contains(out, "跳过") || strings.Contains(out, "照跑") {
		t.Errorf("手动不过这一关：\n%s", out)
	}
	j := e.routineJobs(config.KindNight)
	waitExit(t, j[len(j)-1].PID, 10*time.Second)
	if _, err := os.Stat(e.reportPath(config.KindNight, day4)); err == nil {
		t.Error("手动不写占位报告")
	}
}

// 远程指向的仓库删了：gh 明确说找不到是确定的现状，记下后照样能判「没事」，不会因为一个旧远程天天照跑。
func TestIntegrationNightIdleRepoGone(t *testing.T) {
	t.Parallel()
	e, _, day1 := idleEnv(t, config.KindNight)
	os.WriteFile(filepath.Join(e.home, "fake-gh-gone"), nil, 0o644)
	e.baselineRoutine(config.KindNight, day1)
	e.assertIdle(config.KindNight, day1.AddDate(0, 0, 1))
}

// 每一项查出有事、或查不出：都照跑，提示词和原来逐字相同。
func TestIntegrationNightRunsWhenBusy(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		why     string
		prepare func(t *testing.T, e *testEnv, dir string)
	}{
		"待做有条目": {"的「待做」有条目", func(t *testing.T, e *testEnv, dir string) {
			writeTodo(t, dir, "- [ ] 甲\n")
		}},
		"只有卡住的条目": {"的「待做」有条目", func(t *testing.T, e *testEnv, dir string) {
			writeTodo(t, dir, "- [ ] 甲（卡住：两次没过）\n")
		}},
		"等用户决定": {"的「等用户决定」有条目", func(t *testing.T, e *testEnv, dir string) {
			appendBacklog(t, dir, "\n## 等用户决定\n\n- [ ] 选哪个 —— A 或 B\n")
		}},
		"等用户决定带等条件": {"的「等用户决定」有条目", func(t *testing.T, e *testEnv, dir string) {
			appendBacklog(t, dir, "\n## 等用户决定\n\n- [ ] 选哪个（等条件：前面合并）\n")
		}},
		"HEAD 变了": {"的 HEAD 变了", func(t *testing.T, e *testEnv, dir string) {
			commitFile(t, e, dir, "code.go", "package x\n")
		}},
		"改代码顺带改 LESSONS": {"的 HEAD 变了", func(t *testing.T, e *testEnv, dir string) {
			commitFile(t, e, dir, "LESSONS.md", "x\n")
			commitFile(t, e, dir, "code.go", "package x\n")
		}},
		"改了又改回来": {"的 HEAD 变了", func(t *testing.T, e *testEnv, dir string) {
			commitFile(t, e, dir, "code.go", "package x\n")
			testkit.GitT(t, e.home, dir, "rm", "-q", "code.go")
			testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "删了")
		}},
		"HEAD 回退了": {"的 HEAD 变了", func(t *testing.T, e *testEnv, dir string) {
			commitFile(t, e, dir, "code.go", "package x\n")
			st := e.state()
			st.RoutineMarks[config.KindNight].Heads["demo"] = testkit.GitT(t, e.home, dir, "rev-parse", "HEAD")
			state.Save(filepath.Join(e.home, ".agentq", "state.json"), st)
			testkit.GitT(t, e.home, dir, "reset", "-q", "--hard", "HEAD~1")
		}},
		"多了项目": {"多了项目 other", func(t *testing.T, e *testEnv, dir string) {
			orphanRepo(t, e, "other", "")
		}},
		"项目不在了": {"项目 demo 不在了", func(t *testing.T, e *testEnv, dir string) {
			os.RemoveAll(dir)
			orphanRepo(t, e, "other2", "")
			e.editState(func(st *State) { st.RoutineMarks[config.KindNight].Heads["other2"] = "x" })
		}},
		"有新 issue": {"有新 issue", func(t *testing.T, e *testEnv, dir string) {
			os.WriteFile(filepath.Join(e.home, "fake-gh.json"), []byte(`[{"updatedAt":"2026-11-05T00:00:00Z"}]`), 0o644)
		}},
		"gh 报错": {"查不出：gh issue list", func(t *testing.T, e *testEnv, dir string) {
			os.WriteFile(filepath.Join(e.home, "fake-gh-fail"), nil, 0o644)
		}},
		"gh 输出看不懂": {"查不出：gh issue list", func(t *testing.T, e *testEnv, dir string) {
			os.WriteFile(filepath.Join(e.home, "fake-gh.json"), []byte("不是 JSON"), 0o644)
		}},
		"gh 不存在": {"查不出：gh issue list", func(t *testing.T, e *testEnv, dir string) {
			e.extra = append(e.extra, "PATH=/usr/bin:/bin")
		}},
		"BACKLOG 读不了": {"查不出：demo 的 BACKLOG", func(t *testing.T, e *testEnv, dir string) {
			p := filepath.Join(dir, "docs", "BACKLOG.md")
			os.Remove(p)
			os.Mkdir(p, 0o755)
		}},
		"git 坏了": {"查不出：读 demo 的 HEAD", func(t *testing.T, e *testEnv, dir string) {
			os.Remove(filepath.Join(dir, ".git", "HEAD"))
		}},
		"上次没跑完": {"上次夜班没跑完", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) { st.Jobs[0].Status = state.StatusFailed })
		}},
		"上次撞额度": {"上次夜班没跑完", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) { st.Jobs[0].LimitHit = true })
		}},
		"上次之后又有一次没记现状": {"上次夜班的任务记录对不上", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) {
				end := st.Jobs[0].EndedAt.Add(time.Hour)
				st.Jobs = append(st.Jobs, &Job{ID: "later", Kind: config.KindNight, Status: state.StatusDone, StartedAt: end, EndedAt: &end})
			})
		}},
		"老状态文件没有记录": {"没有上次的记录", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) { st.RoutineMarks = nil })
		}},
		"任务记录过期了": {"任务记录对不上", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) { st.Jobs = nil })
		}},
		"有任务等着续跑": {"有任务等着续跑", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) {
				st.Jobs = append(st.Jobs, &Job{ID: "x", Project: "demo", ProjectDir: dir, Status: state.StatusInterrupted, ResumePending: true, StartedAt: st.Jobs[0].StartedAt})
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, dir, day1 := idleEnv(t, config.KindNight)
			e.baselineRoutine(config.KindNight, day1)
			c.prepare(t, e, dir)
			e.assertRuns(config.KindNight, day1.AddDate(0, 0, 1), c.why)
		})
	}
}

func writeTodo(t *testing.T, dir, line string) {
	t.Helper()
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "## 待做\n", "## 待做\n\n"+line, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendBacklog(t *testing.T, dir, text string) {
	t.Helper()
	p := filepath.Join(dir, "docs", "BACKLOG.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// 复盘：上次复盘之后没有任务结束、HEAD 没变才跳过；复盘自己只改 LESSONS.md 的提交不算，否则永远跳不过。
func TestIntegrationReviewIdle(t *testing.T) {
	t.Parallel()
	e, dir, day1 := idleEnv(t, config.KindReview)
	e.baselineRoutine(config.KindReview, day1)
	commitFile(t, e, dir, "LESSONS.md", "reviewed-through\n") // 复盘自己的提交
	day2 := day1.AddDate(0, 0, 1)
	e.assertIdle(config.KindReview, day2)
	if b, _ := os.ReadFile(filepath.Join(e.home, "fake-gh-calls")); !strings.Contains(string(b), "issue list --repo o/demo") {
		t.Errorf("复盘也要查 issue：%s", b)
	}
	// 商量大方向结束不算：它不改代码
	e.editState(func(st *State) {
		end := day2.Add(2 * time.Hour)
		st.Jobs = append(st.Jobs, &Job{ID: "x-discuss", Kind: state.KindDiscuss, Project: "demo", Status: state.StatusDone, StartedAt: end, EndedAt: &end})
	})
	// 复盘只认这几样：待做有条目不算（那是夜班的事）
	writeTodo(t, dir, "- [ ] 甲（卡住：x）\n")
	e.assertIdle(config.KindReview, day2.AddDate(0, 0, 1))
}

func TestIntegrationReviewRunsWhenBusy(t *testing.T) {
	t.Parallel()
	ended := func(kind string) func(t *testing.T, e *testEnv, dir string) {
		return func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) {
				end := st.Jobs[0].StartedAt.Add(2 * time.Hour)
				st.Jobs = append(st.Jobs, &Job{ID: "x-" + kind, Kind: kind, Project: "demo", Status: state.StatusDone, StartedAt: end, EndedAt: &end})
			})
		}
	}
	for name, c := range map[string]struct {
		why     string
		prepare func(t *testing.T, e *testEnv, dir string)
	}{
		"待做任务结束了": {"上次复盘之后有任务结束", ended("")},
		"夜班结束了":   {"上次复盘之后有任务结束", ended(config.KindNight)},
		"对话结束了":   {"上次复盘之后有任务结束", ended(state.KindChat)},
		"失败的任务也算": {"上次复盘之后有任务结束", func(t *testing.T, e *testEnv, dir string) {
			ended("")(t, e, dir)
			e.editState(func(st *State) { st.Jobs[1].Status = state.StatusFailed })
		}},
		"HEAD 变了": {"的 HEAD 变了", func(t *testing.T, e *testEnv, dir string) { commitFile(t, e, dir, "code.go", "x\n") }},
		"git 坏了":  {"查不出：读 demo 的 HEAD", func(t *testing.T, e *testEnv, dir string) { os.Remove(filepath.Join(dir, ".git", "HEAD")) }},
		"上次没写出报告": {"上次复盘没跑完", func(t *testing.T, e *testEnv, dir string) {
			e.editState(func(st *State) { st.Jobs[0].Status = state.StatusInterrupted })
		}},
		"老状态文件没有记录": {"没有上次的记录", func(t *testing.T, e *testEnv, dir string) { e.editState(func(st *State) { st.RoutineMarks = nil }) }},
		"有新 issue": {"有新 issue", func(t *testing.T, e *testEnv, dir string) {
			os.WriteFile(filepath.Join(e.home, "fake-gh.json"), []byte(`[{"updatedAt":"2026-11-05T00:00:00Z"}]`), 0o644)
		}},
		"gh 报错": {"查不出：gh issue list", func(t *testing.T, e *testEnv, dir string) {
			os.WriteFile(filepath.Join(e.home, "fake-gh-fail"), nil, 0o644)
		}},
		"gh 不存在": {"查不出：gh issue list", func(t *testing.T, e *testEnv, dir string) {
			e.extra = append(e.extra, "PATH=/usr/bin:/bin")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, dir, day1 := idleEnv(t, config.KindReview)
			e.baselineRoutine(config.KindReview, day1)
			c.prepare(t, e, dir)
			e.assertRuns(config.KindReview, day1.AddDate(0, 0, 1), c.why)
		})
	}
}

// 跑了之后基线更新：第二天的改动触发一次，第三天又没事了就跳过。上次没跑完不会让第二天跳过。
func TestIntegrationRoutineBaselineMoves(t *testing.T) {
	t.Parallel()
	e, dir, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	commitFile(t, e, dir, "code.go", "x\n")
	day2 := day1.AddDate(0, 0, 1)
	saved := e.extra
	e.extra = append(append([]string(nil), saved...), "AGENTQ_FAKE_WRITE="+e.reportPath(config.KindNight, day2))
	out := e.runAt(day2, "night", "--scheduled")
	e.extra = saved
	if !strings.Contains(out, "的 HEAD 变了") {
		t.Fatalf("\n%s", out)
	}
	j := e.routineJobs(config.KindNight)[1]
	waitExit(t, j.PID, 10*time.Second)
	e.runAt(day2.Add(time.Minute), "tick")
	os.Remove(filepath.Join(e.home, "fake-claude-calls"))
	e.assertIdle(config.KindNight, day2.AddDate(0, 0, 1))
	if got := e.state().RoutineMarks[config.KindNight].JobID; got != j.ID {
		t.Errorf("跳过不改基线：%s", got)
	}

	// 没写出报告（中断）：基线记了，但上次没跑完，第二天照跑
	e2, _, d1 := idleEnv(t, config.KindNight)
	if out := e2.runAt(d1, "night", "--scheduled"); !strings.Contains(out, "已启动") {
		t.Fatalf("\n%s", out)
	}
	waitExit(t, e2.routineJob(config.KindNight).PID, 10*time.Second)
	e2.runAt(d1.Add(time.Minute), "tick")
	if got := e2.routineJob(config.KindNight); got.Status != state.StatusInterrupted {
		t.Fatalf("%+v", got)
	}
	e2.assertRuns(config.KindNight, d1.AddDate(0, 0, 1), "上次夜班没跑完")
}

// 跳过之后：tick 不补跑（一个进程都不起）；已有报告不覆盖；开关关着照旧只记一行。
func TestIntegrationRoutineSkipNoCatchUp(t *testing.T) {
	t.Parallel()
	e, _, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	day2 := day1.AddDate(0, 0, 1)
	e.assertIdle(config.KindNight, day2)
	for _, d := range []time.Duration{20 * time.Minute, 3 * time.Hour} {
		if out := e.runAt(day2.Add(d), "tick"); strings.Contains(out, "补跑") {
			t.Errorf("跳过后不该补跑：\n%s", out)
		}
	}
	if calls := e.claudeCalls(); len(calls) != 0 {
		t.Errorf("不该起进程：%v", calls)
	}

	// 关机错过了定点：tick 发现错过时也先判断，没事就不起
	day3 := day2.AddDate(0, 0, 1)
	if out := e.runAt(day3.Add(time.Hour), "tick"); strings.Contains(out, "开机后补跑：") || !strings.Contains(out, "确认没有新东西") {
		t.Errorf("没事不补跑：\n%s", out)
	}
	if calls := e.claudeCalls(); len(calls) != 0 {
		t.Errorf("不该起进程：%v", calls)
	}
	if _, err := os.Stat(e.reportPath(config.KindNight, day3)); err != nil {
		t.Errorf("补跑判断跳过也写占位报告，今天不再算错过：%v", err)
	}

	// 已经有报告（别处跑的）：不覆盖
	day4 := day3.AddDate(0, 0, 1)
	p := e.reportPath(config.KindNight, day4)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("桌面端写的\n"), 0o644)
	if out := e.runAt(day4, "night", "--scheduled"); !strings.Contains(out, "跳过") {
		t.Errorf("\n%s", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "桌面端写的\n" {
		t.Errorf("不该覆盖：%s", b)
	}
}

// 补跑时有事：照原样补跑（提示词带补跑说明），并记下基线。
func TestIntegrationCatchUpRunsWhenBusy(t *testing.T) {
	t.Parallel()
	e, dir, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	commitFile(t, e, dir, "code.go", "x\n")
	day2 := day1.AddDate(0, 0, 1)
	out := e.runAt(day2.Add(time.Hour), "tick")
	if !strings.Contains(out, "夜班开机后补跑：") {
		t.Fatalf("\n%s", out)
	}
	jobs := e.routineJobs(config.KindNight)
	j := jobs[len(jobs)-1]
	prompt, _ := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if !strings.HasPrefix(string(prompt), routine.RoutinePromptPrefix+routine.CatchUpNightNote) {
		t.Errorf("%s", prompt)
	}
	if got := e.state().RoutineMarks[config.KindNight].JobID; got != j.ID {
		t.Errorf("补跑也记基线：%s", got)
	}
	waitExit(t, j.PID, 10*time.Second)
}

// 占位报告写不了：照跑（不能又没跑又没报告）。
func TestIntegrationRoutineIdleReportUnwritable(t *testing.T) {
	t.Parallel()
	e, _, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	nightly := filepath.Join(e.home, ".claude", "nightly")
	if err := os.Chmod(nightly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(nightly, 0o755) })
	e.assertRunsNoReportCheck(config.KindNight, day1.AddDate(0, 0, 1), "占位报告写不了")
}

func (e *testEnv) assertRunsNoReportCheck(kind string, at time.Time, why string) {
	e.t.Helper()
	out := e.runAt(at, kind, "--scheduled")
	if !strings.Contains(out, "已启动：任务") || !strings.Contains(out, why) {
		e.t.Fatalf("\n%s", out)
	}
	jobs := e.routineJobs(kind)
	waitExit(e.t, jobs[len(jobs)-1].PID, 10*time.Second)
}

func TestGithubRepos(t *testing.T) {
	got := backlog.GithubRepos(strings.ReplaceAll(`backup	git<at>github.com:acme/alpha.git (fetch)
backup	git<at>github.com:acme/alpha.git (push)
origin	https://github.com/acme/beta (fetch)
up	ssh://git<at>github.com/acme/gamma.git (fetch)
tok	https://x-token<at>github.com/acme/delta.git/ (fetch)
lab	git<at>gitlab.com:acme/nope.git (fetch)
fake	https://notgithub.com/acme/nope.git (fetch)
sub	https://github.com.evil.io/acme/nope.git (fetch)
local	/srv/git/github.com/ (fetch)
`, "<at>", atSign))
	want := "acme/alpha acme/beta acme/gamma acme/delta"
	if strings.Join(got, " ") != want {
		t.Errorf("%v", got)
	}
	if backlog.GithubRepos("") != nil {
		t.Error("没有远程")
	}
}

// 占位报告：巡逻不报缺报告，早报照常推（第一行说没事）。这两样没处理的话，跳过会换来告警和一次分析。
func TestIdleReportSatisfiesPatrolAndMorning(t *testing.T) {
	e := newNotifyEnv(t, true)
	e.a.Cfg.NightEnabled, e.a.Cfg.ReviewEnabled = true, true
	e.at(9, 0)
	if issues := e.a.Patrol.CheckReports(e.st, e.now); len(issues) != 2 {
		t.Fatalf("没有报告时应报两条：%+v", issues)
	}
	for _, kind := range []string{config.KindNight, config.KindReview} {
		if err := e.a.Routine.WriteIdleReport(config.Routines[kind]); err != nil {
			t.Fatal(err)
		}
	}
	if issues := e.a.Patrol.CheckReports(e.st, e.now); len(issues) != 0 {
		t.Errorf("占位报告算有报告：%+v", issues)
	}
	if missed, _ := e.a.Routine.RoutineMissed(e.st, config.Routines[config.KindReview], e.now); missed {
		t.Error("占位报告算有报告，不算错过")
	}
	path := filepath.Join(e.a.Home, ".claude", "nightly", e.now.Format("2006-01-02")+".md")
	old := e.now.Add(-time.Hour)
	os.Chtimes(path, old, old)
	e.flush()
	if got := strings.Join(e.tg.texts(), "\n"); !strings.Contains(got, "今天没有要复盘的") {
		t.Errorf("早报照常推：%s", got)
	}
}

// 到点时开关关着、当天才打开：那天本来就不该有报告，巡逻不报缺、tick 不补跑；第二天照常要求。
func TestRoutineSwitchedOnLaterSameDay(t *testing.T) {
	e := newNotifyEnv(t, false)
	for _, kind := range []string{config.KindNight, config.KindReview} {
		r := config.Routines[kind]
		e.at(r.Hour, r.Minute)
		if err := e.a.Routine.CmdRoutine(kind, true); err != nil { // 开关关着：定点叫醒只记一行
			t.Fatal(err)
		}
	}
	e.a.Cfg.NightEnabled, e.a.Cfg.ReviewEnabled = true, true
	e.at(9, 0)
	if issues := e.a.Patrol.CheckReports(e.st, e.now); len(issues) != 0 {
		t.Errorf("到点时开关关着，当天不报缺报告：%+v", issues)
	}
	for _, kind := range []string{config.KindNight, config.KindReview} {
		if missed, _ := e.a.Routine.RoutineMissed(e.st, config.Routines[kind], e.now); missed {
			t.Errorf("%s 到点时开关关着，当天不补跑", kind)
		}
	}
	e.now = e.now.AddDate(0, 0, 1)
	if issues := e.a.Patrol.CheckReports(e.st, e.now); len(issues) != 2 {
		t.Errorf("第二天开关一直开着，没报告要报两条：%+v", issues)
	}
	if missed, _ := e.a.Routine.RoutineMissed(e.st, config.Routines[config.KindReview], e.now); !missed {
		t.Error("第二天错过的复盘照常补跑（夜班 08:00 后不补，这里看复盘）")
	}
}

// 占位报告不算任务写的：手动夜班正常退出却没写报告，要记中断，第二天照跑（不能被当成没事跳过）。
func TestIntegrationPlaceholderNotTheJobsReport(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{config.KindNight, config.KindReview} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			e, _, day1 := idleEnv(t, kind)
			e.baselineRoutine(kind, day1)
			day2 := day1.AddDate(0, 0, 1)
			e.assertIdle(kind, day2) // 当天先跳过，占位报告在了
			manual := day2.Add(9 * time.Hour)
			e.runAt(manual, kind)
			jobs := e.routineJobs(kind)
			j := jobs[len(jobs)-1]
			waitExit(t, j.PID, 10*time.Second)
			e.runAt(manual.Add(time.Minute), "tick")
			j = e.routineJobs(kind)[len(jobs)-1]
			if j.Status != state.StatusInterrupted || !strings.Contains(j.Summary, "没写出报告") {
				t.Fatalf("占位报告不算这次写的：%+v", j)
			}
			out := e.runAt(day2.AddDate(0, 0, 1), kind, "--scheduled")
			if strings.Contains(out, "跳过") || !strings.Contains(out, "没跑完") || !strings.Contains(out, "已启动") {
				t.Errorf("第二天要照跑：\n%s", out)
			}
			js := e.routineJobs(kind)
			waitExit(t, js[len(js)-1].PID, 10*time.Second)
		})
	}
	// 手动跑时真写了报告（盖掉占位）：算跑完，第二天没事照常跳过
	e, _, day1 := idleEnv(t, config.KindNight)
	e.baselineRoutine(config.KindNight, day1)
	day2 := day1.AddDate(0, 0, 1)
	e.assertIdle(config.KindNight, day2)
	saved := e.extra
	e.extra = append(append([]string(nil), saved...), "AGENTQ_FAKE_WRITE="+e.reportPath(config.KindNight, day2))
	manual := day2.Add(9 * time.Hour)
	e.runAt(manual, "night")
	e.extra = saved
	jobs := e.routineJobs(config.KindNight)
	waitExit(t, jobs[len(jobs)-1].PID, 10*time.Second)
	e.runAt(manual.Add(time.Minute), "tick")
	if j := e.routineJobs(config.KindNight)[len(jobs)-1]; j.Status != state.StatusDone {
		t.Errorf("写了报告算跑完：%+v", j)
	}
	os.Remove(filepath.Join(e.home, "fake-claude-calls"))
	e.assertIdle(config.KindNight, day2.AddDate(0, 0, 1))
}
