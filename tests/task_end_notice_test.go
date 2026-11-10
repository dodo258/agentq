package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// noticeEnv：开着推送、本机通知写到自己的文件，时间拨到中午（夜里只攒着）。
func noticeEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	e := newEnv(t, map[string]any{"notify_enabled": true})
	mac := filepath.Join(e.home, "macnotify.log")
	e.extra = append(e.extra, "AGENTQ_MACNOTIFY_LOG="+mac)
	e.atNoon()
	return e, mac
}

func macLines(t *testing.T, mac string) string {
	t.Helper()
	b, err := os.ReadFile(mac)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// 门合并成功才说做完：任务进程退出时不说，合并后推一条，带项目、标题、合并提交、验收。没配 Telegram 走本机通知。
func TestIntegrationDoneNoticeAfterMerge(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "修东西 READYMERGE"
	gateRepo(t, e, "demo", "- [ ] "+title+" —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("tick") // 收尾，起门
	if got := macLines(t, mac); strings.Contains(got, "做完了") {
		t.Fatalf("还没合并，不能说做完：\n%s", got)
	}
	j = e.jobByTitle(title)
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	got := macLines(t, mac)
	testkit.AssertContains(t, got, "完成", "demo「修东西 READYMERGE」做完了，已合并", "合并提交 "+j.Gate.Merge[:12], "验收：看 a.txt")
	if n := strings.Count(got, "做完了"); n != 1 {
		t.Fatalf("只推一条：\n%s", got)
	}
	e.mustRun("tick")
	if n := strings.Count(macLines(t, mac), "做完了"); n != 1 {
		t.Fatalf("下一次 tick 不再推：%d", n)
	}
}

// 夜里合并的只攒着：不弹，留在队列；白天发出去。
func TestIntegrationDoneNoticeHeldAtNight(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	y, m, d := time.Now().Date()
	night := time.Date(y, m, d, 2, 0, 0, 0, time.Local)
	setNow := func(at time.Time) { e.extra = append(e.extra, "AGENTQ_TEST_NOW="+at.Format(time.RFC3339)) }
	setNow(night)
	title := "夜里的活 READYMERGE"
	gateRepo(t, e, "demo", "- [ ] "+title+" —— BRANCH=agent/t-a FILE=a.txt")
	e.mustRun("tick")
	j := e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v %s", j.Gate, j.Summary)
	}
	if got := macLines(t, mac); strings.Contains(got, "做完了") {
		t.Fatalf("夜里不弹：\n%s", got)
	}
	var s notify.State
	if err := json.Unmarshal(testkit.ReadBytes(t, filepath.Join(e.home, ".agentq", "notify.json")), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 1 || s.Pending[0].Cat != "done" {
		t.Fatalf("要攒在队列里：%+v", s.Pending)
	}
	setNow(night.Add(7 * time.Hour))
	e.mustRun("tick")
	testkit.AssertContains(t, macLines(t, mac), "做完了，已合并")
}

// 失败、中断、卡住也说；会自动续跑的中断不说，到了结果再说。
func TestIntegrationEndNoticeFailures(t *testing.T) {
	t.Parallel()
	t.Run("失败", func(t *testing.T) {
		t.Parallel()
		e, mac := noticeEnv(t)
		testkit.MkProject(t, e.root, "demo", "- [ ] 出错的活 —— APIERR403\n")
		e.mustRun("tick")
		waitExit(t, e.jobByTitle("出错的活").PID, 10*time.Second)
		e.mustRun("tick")
		testkit.AssertContains(t, macLines(t, mac), "要你知道", "demo「出错的活」失败", "详情 agentq logs")
	})
	t.Run("中断：第一次自动续跑不说，续跑后还中断才说", func(t *testing.T) {
		t.Parallel()
		e, mac := noticeEnv(t)
		testkit.MkProject(t, e.root, "demo", "- [ ] 再中断 —— INTERRUPT AGAIN\n")
		e.mustRun("tick")
		waitExit(t, e.jobByTitle("再中断").PID, 10*time.Second)
		e.mustRun("tick")
		if got := macLines(t, mac); strings.Contains(got, "再中断") {
			t.Fatalf("会自动续跑，这时不说：\n%s", got)
		}
		waitExit(t, e.state().Jobs[1].PID, 10*time.Second)
		e.mustRun("tick")
		testkit.AssertContains(t, macLines(t, mac), "demo「再中断」中断")
	})
	t.Run("任务自己标了卡住", func(t *testing.T) {
		t.Parallel()
		e, mac := noticeEnv(t)
		testkit.MkProject(t, e.root, "demo", "- [ ] 卡住的活 —— STUCKMARK\n")
		e.mustRun("tick")
		waitExit(t, e.jobByTitle("卡住的活").PID, 10*time.Second)
		e.mustRun("tick")
		testkit.AssertContains(t, macLines(t, mac), "demo「卡住的活」卡住了", "两次没过")
	})
	t.Run("门卡住只有决定卡片", func(t *testing.T) {
		t.Parallel()
		e, mac := noticeEnv(t)
		dir := gateRepo(t, e, "demo", "- [ ] 绕过门 READYMERGE SELFREADY —— BRANCH=agent/t-s FILE=s.txt")
		e.mustRun("tick")
		j := e.jobByTitle("绕过门 READYMERGE SELFREADY")
		waitExit(t, j.PID, 20*time.Second)
		e.mustRun("tick")
		j = e.jobByTitle("绕过门 READYMERGE SELFREADY")
		if j.Gate == nil || j.Gate.State != state.GateStuck {
			t.Fatalf("%+v %s\n%s", j.Gate, j.Summary, e.backlogOf(dir))
		}
		got := macLines(t, mac)
		testkit.AssertContains(t, got, "要你定", "门没过")
		if strings.Contains(got, "跑完") || strings.Contains(got, "做完了") || strings.Contains(got, "要你知道") {
			t.Fatalf("不重复发：\n%s", got)
		}
	})
}

// 每种结束方式推不推。进程内跑 Finish，只看「任务结果」这个 Key 的那一条。
func TestNoticeEndedTable(t *testing.T) {
	t.Parallel()
	const stuckTodo = "- [ ] 甲 —— 要什么（卡住：两次没过）\n"
	cases := []struct {
		name, todo, done string
		job              state.Job
		status           string
		cat, text        string // cat 为空表示不推
	}{
		{name: "失败", status: state.StatusFailed, cat: "alert", text: "「甲」失败"},
		{name: "超时", status: state.StatusTimeout, cat: "alert", text: "「甲」超时"},
		{name: "中断等人", status: state.StatusInterrupted, cat: "alert", text: "「甲」中断"},
		{name: "会续跑", status: state.StatusInterrupted, job: state.Job{ResumePending: true}},
		{name: "接手任务中断：孤儿扫描会说", status: state.StatusInterrupted, job: state.Job{Takeover: true}},
		{name: "用户停的", status: state.StatusKilled},
		{name: "撞上限", status: state.StatusFailed, job: state.Job{LimitHit: true}},
		{name: "瞬时错误会重试", status: state.StatusFailed, job: state.Job{Transient: true}},
		{name: "撞预算有自己的通知", status: state.StatusInterrupted, job: state.Job{BudgetHit: true}},
		{name: "自动停有自己的通知", status: state.StatusInterrupted, job: state.Job{AutoStop: "loop"}},
		{name: "过门中：门说", status: state.StatusDone, job: state.Job{Gate: &state.GateRecord{State: state.GateQueued}}},
		{name: "门卡住：卡片说", status: state.StatusDone, job: state.Job{Gate: &state.GateRecord{State: state.GateStuck}}},
		{name: "放下了：StuckPending 有自己的通知", status: state.StatusInterrupted, job: state.Job{StuckPending: "连续两次报告做完"}},
		{name: "任务标了卡住", todo: stuckTodo, status: state.StatusDone, cat: "alert", text: "「甲」卡住了"},
		{name: "跑完没合并", todo: "\n", status: state.StatusDone, cat: "alert", text: "「甲」跑完了，没有要合并的分支"},
		{name: "新合同自己合并：绕过门的提醒说", todo: "\n", done: "- [x] 甲 —— 合并提交 abc1234\n", status: state.StatusDone, job: state.Job{GateContract: true}},
		{name: "老合同自己合并", todo: "\n", done: "- [x] 甲 —— 合并提交 abc1234\n", status: state.StatusDone, cat: "done", text: "「甲」做完了，已合并"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, dir := noticeApp(t, c.todo, c.done)
			j := c.job
			j.ID, j.Project, j.ProjectDir, j.Title, j.Fingerprint = "0315-100000-demo", "demo", dir, "甲", "fp"
			j.StartedAt = a.Now()
			if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
				t.Fatal(err)
			}
			st := &state.State{Jobs: []*state.Job{&j}}
			a.Runner.Finish(st, &j, c.status, "摘要")
			var got []notify.Notice
			for _, n := range pendingNotices(t, a) {
				if n.Key == notify.TaskEndKey("demo", "fp") {
					got = append(got, n)
				}
			}
			if c.cat == "" {
				if len(got) != 0 {
					t.Fatalf("不该推：%+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Cat != c.cat || !strings.Contains(got[0].Text, c.text) || got[0].JobID != j.ID {
				t.Fatalf("想要 %s %s，得到 %+v", c.cat, c.text, got)
			}
		})
	}
}

// 别的 Kind 有自己的通知或者不算「任务做完」，这里不多发。
func TestNoticeEndedOtherKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{state.KindChat, state.KindFollowup, state.KindPatrol, state.KindDiscuss, state.KindNew, config.KindNight, config.KindReview} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			a, dir := noticeApp(t, "- [ ] 甲\n", "")
			j := &state.Job{ID: "0315-100000-demo", Kind: kind, Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: "fp", StartedAt: a.Now()}
			if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
				t.Fatal(err)
			}
			a.Runner.Finish(&state.State{Jobs: []*state.Job{j}}, j, state.StatusFailed, "摘要")
			for _, n := range pendingNotices(t, a) {
				if n.Key == notify.TaskEndKey("demo", "fp") {
					t.Fatalf("不该推：%+v", n)
				}
			}
		})
	}
}

// 关着推送什么都不记。
func TestNoticeEndedNotifyOff(t *testing.T) {
	t.Parallel()
	a, dir := noticeApp(t, "- [ ] 甲\n", "")
	a.Cfg.NotifyEnabled = false
	j := &state.Job{ID: "0315-100000-demo", Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: "fp", StartedAt: a.Now()}
	if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Runner.Finish(&state.State{Jobs: []*state.Job{j}}, j, state.StatusFailed, "摘要")
	if got := pendingNotices(t, a); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func noticeApp(t *testing.T, todo, done string) (*App, string) {
	t.Helper()
	home := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(home, ".agentq", "config.json"), "{}\n", 0o600)
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg.ProjectsRoot = filepath.Join(home, "projects")
	a.Cfg.NotifyEnabled = true
	dir := testkit.MkProject(t, a.ProjectsRoot(), "demo", todo)
	if done != "" {
		b := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md")) + done
		testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), b, 0o644)
	}
	return a, dir
}

func pendingNotices(t *testing.T, a *App) []notify.Notice {
	t.Helper()
	s, err := a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	return s.Pending
}
