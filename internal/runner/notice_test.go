package runner_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// 收尾通知只按「这一条接下来等谁」分类：交给用户的（卡住、放下）是要你定，告诉一声的是要你知道，合并了是完成，引擎接着做的不推。
func TestEndNoticeByWhoIsNext(t *testing.T) {
	const (
		stuckTodo   = "- [ ] 甲 —— 要什么（卡住：等用户：验收取舍，分支 agent/x）（选项：A 改家长页｜B 先合判断层；推荐 B）\n"
		plainTodo   = "- [ ] 甲 —— 要什么\n"
		doneSection = "- [x] 甲 —— 合并提交 abc1234\n"
	)
	cases := []struct {
		name, todo, done string
		job              state.Job
		status           string
		key              string // 空：不推
		cat              string
		want             []string
	}{
		{name: "失败：告诉一声", todo: plainTodo, status: state.StatusFailed, key: "task-end", cat: "alert", want: []string{"demo「甲」失败", "详情 agentq logs"}},
		{name: "超时：告诉一声", todo: plainTodo, status: state.StatusTimeout, key: "task-end", cat: "alert", want: []string{"demo「甲」超时"}},
		{name: "跑完没分支：告诉一声", todo: "\n", status: state.StatusDone, key: "task-end", cat: "alert", want: []string{"demo「甲」跑完了，没有要合并的分支"}},
		{name: "交给用户的卡住：要你定，带选项", todo: stuckTodo, status: state.StatusDone, key: "task-end", cat: "decide",
			want: []string{"demo「甲」卡住了", "等用户：验收取舍", "A 改家长页", "B 先合判断层（推荐）"}},
		{name: "老合同自己合并：完成", todo: "\n", done: doneSection, status: state.StatusDone, key: "task-end", cat: "done", want: []string{"demo「甲」做完了，已合并", "合并提交 abc1234"}},
		{name: "会续跑：不推", todo: plainTodo, status: state.StatusInterrupted, job: state.Job{ResumePending: true}},
		{name: "引擎再接：不推", todo: plainTodo, status: state.StatusDone, job: state.Job{Retake: "测试一直超时"}},
		{name: "瞬时错误会重试：不推", todo: plainTodo, status: state.StatusFailed, job: state.Job{Transient: true}},
		{name: "用户自己停的：不推", todo: plainTodo, status: state.StatusKilled},
		{name: "门在管：不推", todo: plainTodo, status: state.StatusDone, job: state.Job{Gate: &state.GateRecord{State: state.GateQueued}}},
		{name: "撞预算会续跑：告诉一声", todo: plainTodo, status: state.StatusInterrupted, job: state.Job{BudgetHit: true, ResumePending: true},
			key: "budget", cat: "alert", want: []string{"撞到预算上限，会接着做一次"}},
		{name: "续跑后又撞预算放下了：要你定", todo: plainTodo, status: state.StatusInterrupted, job: state.Job{BudgetHit: true, StuckPending: "续跑后又撞预算上限"},
			key: "budget", cat: "decide", want: []string{"续跑后又撞预算上限，放下了", "回复这条消息，接回原会话和分支"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, e, dir := noticeStack(t, c.todo, c.done)
			j := c.job
			j.ID, j.Project, j.ProjectDir, j.Title, j.Fingerprint, j.StartedAt = "1118-201427-demo", "demo", dir, "甲", "fp", e.Now()
			os.MkdirAll(e.JobDir(j.ID), 0o755)
			k.Runner.Finish(&state.State{Jobs: []*state.Job{&j}}, &j, c.status, "摘要")
			ns, err := k.Notify.LoadNotify()
			if err != nil {
				t.Fatal(err)
			}
			var got []notify.Notice
			for _, n := range ns.Pending {
				if n.Key == notify.TaskEndKey("demo", "fp") || n.Key == "budget:"+j.ID {
					got = append(got, n)
				}
			}
			if c.key == "" {
				if len(got) != 0 {
					t.Fatalf("不该推：%+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Cat != c.cat || got[0].JobID != j.ID || !strings.HasPrefix(got[0].Key, c.key) {
				t.Fatalf("想要一条 %s（%s），得到 %+v", c.cat, c.key, got)
			}
			all := got[0].Text + "\n" + strings.Join(got[0].Lines, "\n")
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Fatalf("没有 %q：\n%s", w, all)
				}
			}
		})
	}
}

// 连续两次报告做完、条目还在待做：标卡住交给用户，要你定。
func TestEndNoticeStillListedIsDecide(t *testing.T) {
	k, e, dir := noticeStack(t, "- [ ] 甲 —— 要什么\n", "")
	ended := e.Now()
	prev := &state.Job{ID: "1118-100000-demo", Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: backlog.Fingerprint("- [ ] 甲 —— 要什么"),
		Status: state.StatusDone, StartedAt: ended, EndedAt: &ended, StillListed: true}
	j := &state.Job{ID: "1118-201427-demo", Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: prev.Fingerprint, Status: state.StatusRunning, StartedAt: e.Now()}
	os.MkdirAll(e.JobDir(j.ID), 0o755)
	os.WriteFile(filepath.Join(e.JobDir(j.ID), "result.json"), []byte(`{"type":"result","subtype":"success","is_error":false,"result":"做了。\n\n没合并，分支 agent/x。","session_id":"s"}`+"\n"), 0o644)
	k.Runner.FinishFromResult(&state.State{Jobs: []*state.Job{prev, j}}, j)
	ns, err := k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns.Pending {
		if strings.HasPrefix(n.Key, "still-listed:") {
			if n.Cat != "decide" || !strings.Contains(n.Text, "连续两次报告做完") {
				t.Fatalf("%+v", n)
			}
			return
		}
	}
	t.Fatalf("没推：%+v", ns.Pending)
}

func noticeStack(t *testing.T, todo, done string) (*stack.Stack, *env.Env, string) {
	t.Helper()
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
	os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
	e, err := env.New(home, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	e.Cfg.ProjectsRoot, e.Cfg.NotifyEnabled = t.TempDir(), true
	k := stack.New(e)
	dir := filepath.Join(e.Cfg.ProjectsRoot, "demo")
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n"+todo+"\n## 已完成\n\n"+done), 0o644)
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	return k, e, dir
}
