package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// rootOrphanEnv：要用巡逻、对话这些上层功能的孤儿用例留根用的夹具。真 git 仓库 + 假时钟（真提交和真文件的时间都是「现在」，把假时钟拨到 3 天后就都算旧了）。
type rootOrphanEnv struct {
	*patrolEnv
	p backlog.Project
}

func newRootOrphanEnv(t *testing.T, todo string) *rootOrphanEnv {
	t.Helper()
	pe := newPatrolEnv(t)
	pe.now = time.Now().Add(72 * time.Hour)
	pe.a.Cfg.NotifyEnabled = true
	dir := testkit.MkProject(t, pe.root, "demo", todo)
	os.RemoveAll(filepath.Join(dir, ".git"))
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/\n"), 0o644)
	home := pe.a.Home
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", ".")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	return &rootOrphanEnv{patrolEnv: pe, p: backlog.Project{Name: "demo", Dir: dir}}
}

func (e *rootOrphanEnv) git(args ...string) string {
	return testkit.GitT(e.t, e.a.Home, e.p.Dir, args...)
}

func (e *rootOrphanEnv) backlog() string {
	b, err := os.ReadFile(e.p.BacklogPath())
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// branch 建分支和 worktree（在 .claude/worktrees/ 下）；commit 为真时在分支上提交一个文件。
func (e *rootOrphanEnv) branch(name string, commit bool) string {
	e.t.Helper()
	wt := filepath.Join(e.p.Dir, ".claude", "worktrees", filepath.Base(name))
	e.git("worktree", "add", "-q", wt, "-b", name, "main")
	if commit {
		os.WriteFile(filepath.Join(wt, "work.txt"), []byte("x\n"), 0o644)
		testkit.GitT(e.t, e.a.Home, wt, "add", ".")
		testkit.GitT(e.t, e.a.Home, wt, "commit", "-q", "-m", "work")
	}
	return wt
}

// marker 生成 hoursAgo 小时前的进行中标记。
func (e *rootOrphanEnv) marker(branch string, hoursAgo int) string {
	return fmt.Sprintf("（进行中：%s %s）", branch, e.now.Add(-time.Duration(hoursAgo)*time.Hour).Format("2006-01-02 15:04"))
}

func (e *rootOrphanEnv) setTodo(line string) {
	e.t.Helper()
	text := "# 待办\n\n## 待做\n\n" + line + "\n\n## 已完成\n"
	if err := os.WriteFile(e.p.BacklogPath(), []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git("add", ".")
	e.git("commit", "-q", "-m", "todo")
}

func (e *rootOrphanEnv) item() backlog.Item {
	e.t.Helper()
	items, err := e.p.Items()
	if err != nil || len(items) != 1 {
		e.t.Fatalf("应有一条待做：%v %+v", err, items)
	}
	return items[0]
}

func (e *rootOrphanEnv) pending() []notify.Notice {
	e.t.Helper()
	s, err := e.a.Notify.LoadNotify()
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Pending
}

func (e *rootOrphanEnv) commits() string { return e.git("rev-list", "--count", "HEAD") }

func (e *rootOrphanEnv) nightlyText() string {
	b, _ := os.ReadFile(e.a.NightlyPath(e.now))
	return string(b)
}

// 用户停掉的 new 留下的项目：孤儿处理也不碰，接手和去标记都算自动做。
func TestScanOrphansSkipsHeldProject(t *testing.T) {
	e := newRootOrphanEnv(t, "")
	e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	ended := e.now.Add(-48 * time.Hour)
	e.st.Jobs = []*Job{{ID: "n", Kind: state.KindNew, Project: newproj.NewJobName, Status: state.StatusKilled, EndedAt: &ended, HeldProjects: []string{e.p.Name}}}
	before, c := e.backlog(), e.commits()
	if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("不该接手：%+v", got)
	}
	if e.backlog() != before || e.commits() != c {
		t.Fatalf("不该改 BACKLOG：\n%s", e.backlog())
	}
}

// 用户停掉的任务：标记还在时不接手、不去标记、不标卡住，只报一次；status和「状态」各列一行；删了标记或有了新任务恢复。
func TestScanOrphansStopped(t *testing.T) {
	setup := func(t *testing.T, takeover bool) *rootOrphanEnv {
		e := newRootOrphanEnv(t, "")
		e.branch("agent/x", true)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
		ended := e.now.Add(-48 * time.Hour)
		e.st.Jobs = []*Job{{ID: "1104-000000-demo", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusKilled, Takeover: takeover, PID: 1, EndedAt: &ended, StartedAt: ended}}
		return e
	}
	t.Run("有未合并内容也不接手", func(t *testing.T) {
		e := setup(t, false)
		before, c := e.backlog(), e.commits()
		for i := 0; i < 3; i++ {
			if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 0 {
				t.Fatalf("不该接手：%+v", got)
			}
		}
		if e.backlog() != before || e.commits() != c {
			t.Fatalf("不该改 BACKLOG：\n%s", e.backlog())
		}
		if n := strings.Count(e.out.String(), "任务 1104-000000-demo 被停止"); n != 1 {
			t.Fatalf("日志只记一次，记了 %d 次：\n%s", n, e.out)
		}
		if s := e.nightlyText(); strings.Count(s, "被停止") != 1 {
			t.Fatalf("汇总只记一次：%s", s)
		}
		list := e.a.Orphan.StoppedOrphans(e.st)
		if len(list) != 1 || list[0].JobID != "1104-000000-demo" || !strings.Contains(list[0].String(), "去掉进行中标记") {
			t.Fatalf("要列出来：%+v", list)
		}
		if d := e.a.Chat.StatusDigest(e.st); !strings.Contains(d, "已停止、标记还在") || !strings.Contains(d, "1104-000000-demo") {
			t.Fatalf("「状态」要列一行：%s", d)
		}
	})
	t.Run("接手任务被停也不标卡住", func(t *testing.T) {
		e := setup(t, true)
		before := e.backlog()
		if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
			t.Fatalf("%+v\n%s", got, e.backlog())
		}
	})
	t.Run("条目正文被改过（指纹变了）也认", func(t *testing.T) {
		e := setup(t, false)
		e.st.Jobs[0].Fingerprint = "以前的指纹"
		if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("记录不过期", func(t *testing.T) {
		e := setup(t, false)
		e.a.Orphan.ScanOrphans(e.st)
		e.now = e.now.Add(state.KeepEndedJobs + 24*time.Hour)
		e.st.Prune(e.now)
		if len(e.st.Jobs) != 1 {
			t.Fatal("报过的停止记录不该过期，否则 30 天后条目被自动接手")
		}
		if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("新任务之后恢复正常", func(t *testing.T) {
		e := setup(t, false)
		e.a.Orphan.ScanOrphans(e.st)
		started := e.st.Jobs[0].StartedAt.Add(time.Hour)
		ended := started.Add(time.Hour)
		e.st.Jobs = append(e.st.Jobs, &Job{ID: "n", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, PID: 2, EndedAt: &ended, StartedAt: started})
		if got := e.a.Orphan.ScanOrphans(e.st); len(got) != 1 {
			t.Fatalf("最近的任务不是被停止的，应接手：%+v", got)
		}
		if e.st.Jobs[0].StopNoted {
			t.Fatal("应解除")
		}
		if l := e.a.Orphan.StoppedOrphans(e.st); len(l) != 0 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("删了标记就解除", func(t *testing.T) {
		e := setup(t, false)
		e.a.Orphan.ScanOrphans(e.st)
		e.setTodo("- [ ] 甲")
		e.a.Orphan.ScanOrphans(e.st)
		if e.st.Jobs[0].StopNoted {
			t.Fatal("删了标记应解除，记录照常过期")
		}
		if l := e.a.Orphan.StoppedOrphans(e.st); len(l) != 0 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("BACKLOG 读不了不解除", func(t *testing.T) {
		e := setup(t, false)
		e.a.Orphan.ScanOrphans(e.st)
		os.Remove(e.p.BacklogPath())
		os.Mkdir(e.p.BacklogPath(), 0o755)
		e.a.Orphan.ScanOrphans(e.st)
		if !e.st.Jobs[0].StopNoted {
			t.Fatal("读不了当未知，不解除")
		}
	})
}

// 接手任务收尾：没做完不续跑；巡逻也不再写接手待做。
func TestTakeoverJobNotResumed(t *testing.T) {
	e := newRootOrphanEnv(t, "")
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	it := e.item()
	st := e.st
	j := finishResult(t, e.a, st, &Job{ID: "t-to", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Item: it.Text, Fingerprint: it.Fingerprint, Takeover: true, SessionID: "s"}, map[string]any{
		"is_error": false, "subtype": "success", "num_turns": 30, "result": "做了一半", "session_id": "s"})
	if j.Status != state.StatusInterrupted || j.ResumePending || !strings.Contains(j.Summary, "接手后仍未完成") {
		t.Fatalf("接手没做完不续跑：%+v", j)
	}
	if action, _ := e.a.Patrol.TakeOverTodo(st, j); !strings.Contains(action, "接手") || strings.Contains(e.backlog(), backlog.TakeOverPrefix) {
		t.Fatalf("巡逻不再写接手待做：%s\n%s", action, e.backlog())
	}
}
