package orphan

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// orphanEnv：真 git 仓库 + 假时钟（真提交和真文件的时间都是「现在」，把假时钟拨到 3 天后就都算旧了）。
type orphanEnv struct {
	t    *testing.T
	a    *Service
	out  *bytes.Buffer
	root string
	now  time.Time
	st   *state.State
	p    backlog.Project
}

// newOrphanApp 建模块实例。
func newOrphanApp(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Backlog: k.Backlog, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner}
}

func newOrphanEnv(t *testing.T, todo string) *orphanEnv {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	// 要在临时仓库里提交：临时 HOME 下的 git 配置给出身份，也没有全局钩子
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
	os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
	var out bytes.Buffer
	en, err := env.New(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a := newOrphanApp(en)
	a.Cfg.ProjectsRoot = root
	a.Cfg.NightEnabled, a.Cfg.ReviewEnabled = true, true
	a.Cfg.NotifyEnabled = true
	e := &orphanEnv{t: t, a: a, out: &out, root: root, st: &state.State{}, now: time.Now().Add(72 * time.Hour)}
	a.Now = func() time.Time { return e.now }
	dir := testkit.MkProject(t, root, "demo", todo)
	os.RemoveAll(filepath.Join(dir, ".git"))
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/\n"), 0o644)
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", ".")
	testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
	e.p = backlog.Project{Name: "demo", Dir: dir}
	return e
}

func (e *orphanEnv) git(args ...string) string { return testkit.GitT(e.t, e.a.Home, e.p.Dir, args...) }

func (e *orphanEnv) backlog() string {
	b, err := os.ReadFile(e.p.BacklogPath())
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// branch 建分支和 worktree（在 .claude/worktrees/ 下）；commit 为真时在分支上提交一个文件。
func (e *orphanEnv) branch(name string, commit bool) string {
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
func (e *orphanEnv) marker(branch string, hoursAgo int) string {
	return fmt.Sprintf("（进行中：%s %s）", branch, e.now.Add(-time.Duration(hoursAgo)*time.Hour).Format("2006-01-02 15:04"))
}

func (e *orphanEnv) setTodo(line string) {
	e.t.Helper()
	text := "# 待办\n\n## 待做\n\n" + line + "\n\n## 已完成\n"
	if err := os.WriteFile(e.p.BacklogPath(), []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git("add", ".")
	e.git("commit", "-q", "-m", "todo")
}

func (e *orphanEnv) item() backlog.Item {
	e.t.Helper()
	items, err := e.p.Items()
	if err != nil || len(items) != 1 {
		e.t.Fatalf("应有一条待做：%v %+v", err, items)
	}
	return items[0]
}

func (e *orphanEnv) pending() []notify.Notice {
	e.t.Helper()
	s, err := e.a.Notify.LoadNotify()
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Pending
}

func (e *orphanEnv) commits() string { return e.git("rev-list", "--count", "HEAD") }

func TestDecideOrphan(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	old := now.Add(-orphanIdle - time.Minute)
	cases := []struct {
		name   string
		f      backlog.OrphanFacts
		active time.Time
		want   orphanAction
	}{
		{"标记 24 小时内", backlog.OrphanFacts{}, now.Add(-orphanIdle + time.Second), orphanLeave},
		{"刚好 24 小时算旧", backlog.OrphanFacts{}, now.Add(-orphanIdle), orphanClear},
		{"未来时间（钟不准）当活跃", backlog.OrphanFacts{}, now.Add(time.Hour), orphanLeave},
		{"没写分支没 worktree", backlog.OrphanFacts{}, old, orphanClear},
		{"写了分支但分支不存在（可能合并后删了）", backlog.OrphanFacts{Named: "agent/x"}, old, orphanClearWait},
		{"分支已合并", backlog.OrphanFacts{Named: "agent/x", Branch: "agent/x"}, old, orphanClearWait},
		{"分支已合并、worktree 干净", backlog.OrphanFacts{Named: "agent/x", Branch: "agent/x", Worktree: "/w"}, old, orphanClearWait},
		{"分支已合并、worktree 有改动", backlog.OrphanFacts{Named: "agent/x", Branch: "agent/x", Worktree: "/w", Dirty: true}, old, orphanTakeover},
		{"分支有未合并提交", backlog.OrphanFacts{Named: "agent/x", Branch: "agent/x", Ahead: 2}, old, orphanTakeover},
		{"活跃时什么内容都不动", backlog.OrphanFacts{Named: "agent/x", Branch: "agent/x", Ahead: 2, Dirty: true}, now.Add(-time.Hour), orphanLeave},
	}
	for _, c := range cases {
		if got := decideOrphan(c.f, c.active, now); got != c.want {
			t.Errorf("%s：得到 %d，应为 %d", c.name, got, c.want)
		}
	}
}

func TestScanOrphansRecentLeftAlone(t *testing.T) {
	// 标记 1 小时前，没写分支：用户可能开着会话，什么都不动
	e := newOrphanEnv(t, "")
	e.setTodo("- [ ] 甲 —— 说明" + e.marker("agent/x", 1))
	before, c := e.backlog(), e.commits()
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("不该接手：%+v", got)
	}
	if e.backlog() != before || e.commits() != c || len(e.pending()) != 0 {
		t.Fatalf("不该改动：\n%s", e.backlog())
	}
}

func TestScanOrphansRecentActivityLeftAlone(t *testing.T) {
	cases := map[string]func(e *orphanEnv){
		"分支提交在 24 小时内": func(e *orphanEnv) { e.branch("agent/x", true) },
		"worktree 文件在 24 小时内改过": func(e *orphanEnv) {
			wt := e.branch("agent/x", true)
			old := time.Now().Add(-time.Hour)
			os.Chtimes(filepath.Join(wt, "work.txt"), old, old)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newOrphanEnv(t, "")
			e.now = time.Now().Add(time.Hour) // 真实提交、文件都在 1 小时前左右；标记写的是 48 小时前
			setup(e)
			e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
			before := e.backlog()
			if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
				t.Fatalf("有活动不该动：%+v\n%s", got, e.backlog())
			}
		})
	}
	// 提交是旧的，只有 worktree 文件新：单独把假时钟拨到 3 天后，再把文件 touch 到那之前 1 小时
	e := newOrphanEnv(t, "")
	wt := e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	recent := e.now.Add(-time.Hour)
	os.Chtimes(filepath.Join(wt, "work.txt"), recent, recent)
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("worktree 文件新，不该接手：%+v", got)
	}
	// 上一个任务刚结束（比如用户正要处理通知）：同样等 24 小时
	os.Chtimes(filepath.Join(wt, "work.txt"), time.Now(), time.Now())
	ended := e.now.Add(-time.Hour)
	e.st.Jobs = []*state.Job{{ID: "x", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, EndedAt: &ended, StartedAt: ended}}
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("上个任务刚结束，不该接手：%+v", got)
	}
}

func TestScanOrphansClear(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.setTodo("- [ ] 甲 —— 说明（进行中）")
	fp := e.item().Fingerprint
	old := e.now.Add(-48 * time.Hour)
	os.Chtimes(e.p.BacklogPath(), old, old)
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("不该接手：%+v", got)
	}
	it := e.item()
	if !it.Eligible() || it.Fingerprint != fp || strings.Contains(e.backlog(), "进行中") {
		t.Fatalf("应去掉标记、指纹不变：%+v\n%s", it, e.backlog())
	}
	if s := e.git("status", "--porcelain"); s != "" {
		t.Fatalf("应提交：%s", s)
	}
	if msg := e.git("log", "-1", "--format=%s"); !strings.Contains(msg, "甲") {
		t.Fatalf("提交说明：%s", msg)
	}
	if !strings.Contains(e.out.String(), "去掉") {
		t.Fatalf("应记日志：%s", e.out)
	}
	// 去掉后这条能被挑到
	if _, ok := e.a.Runner.Pick(e.st, []backlog.Item{it}, "", true); !ok {
		t.Fatal("下次应当普通条目")
	}
}

func TestScanOrphansUnparseableMarkerUsesBacklogTime(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.setTodo("- [ ] 甲（进行中）")
	recent := e.now.Add(-time.Hour)
	os.Chtimes(e.p.BacklogPath(), recent, recent)
	before := e.backlog()
	if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
		t.Fatalf("标记没时间、BACKLOG 刚改过，不该动：%+v", got)
	}
}

func TestScanOrphansBranchMissingClears(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.setTodo("- [ ] 甲" + e.marker("agent/gone", 48))
	e.a.ScanOrphans(e.st)
	it := e.item()
	if !it.Eligible() {
		t.Fatalf("分支不存在应去掉标记：\n%s", e.backlog())
	}
	// 分支可能合并后删了，和已合并一样不猜：等 retry_after，通知用户
	if _, ok := e.a.Runner.Pick(e.st, []backlog.Item{it}, "", true); ok {
		t.Fatal("分支不存在时也要等 retry_after")
	}
	if n := e.pending(); len(n) != 1 || !strings.Contains(strings.Join(n[0].Lines, " "), "agent/gone") {
		t.Fatalf("应通知：%+v", n)
	}
}

func TestScanOrphansMerged(t *testing.T) {
	e := newOrphanEnv(t, "")
	wt := e.branch("agent/x", true)
	e.git("merge", "-q", "--no-ff", "-m", "合并", "agent/x")
	e.setTodo("- [ ] 甲 —— 说明" + e.marker("agent/x", 48))
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("已合并不该接手：%+v", got)
	}
	b := e.backlog()
	if strings.Contains(b, "进行中") || !strings.Contains(b, "- [ ] 甲 —— 说明") || strings.Contains(b, "- [x]") {
		t.Fatalf("应去掉标记、不勾完成：\n%s", b)
	}
	n := e.pending()
	if len(n) != 1 || !strings.Contains(n[0].Text, "甲") || !strings.Contains(n[0].Text, "12h 后重新做") {
		t.Fatalf("通知要写明多久后重新做：%+v", n)
	}
	if lines := strings.Join(n[0].Lines, " "); !strings.Contains(lines, "agent/x") || !strings.Contains(lines, "移到「已完成」") {
		t.Fatalf("通知要写明已经做完就移走：%s", lines)
	}
	// 这一轮不挑，等 retry_after；到点后恢复；agentq run 不受影响
	it := e.item()
	if !it.Eligible() {
		t.Fatalf("%+v", it)
	}
	if _, ok := e.a.Runner.Pick(e.st, []backlog.Item{it}, "", true); ok {
		t.Fatal("去掉标记后要等 retry_after 才挑")
	}
	if _, ok := e.a.Runner.Pick(e.st, []backlog.Item{it}, "", false); !ok {
		t.Fatal("agentq run 不受影响")
	}
	saved, err := state.Load(e.a.StatePath())
	if err != nil || saved.ItemHold[state.HoldKey("demo", it.Fingerprint)].IsZero() {
		t.Fatalf("等待记录要落盘：%v %+v", err, saved)
	}
	later := e.now.Add(e.a.Cfg.RetryAfter.Duration - time.Minute)
	if state.RetryBlocked(e.st, it.Project, it.Fingerprint, later, e.a.Cfg.RetryAfter.Duration) != true {
		t.Fatal("到点前还要等")
	}
	e.now = e.now.Add(e.a.Cfg.RetryAfter.Duration)
	if _, ok := e.a.Runner.Pick(e.st, []backlog.Item{it}, "", true); !ok {
		t.Fatal("到点后应能挑")
	}
	e.st.Prune(e.now)
	if len(e.st.ItemHold) != 0 {
		t.Fatalf("过期的等待记录应清掉：%+v", e.st.ItemHold)
	}
	if !strings.Contains(e.out.String(), "agent/x") {
		t.Fatalf("应记日志：%s", e.out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("worktree 不该被删")
	}
	e.now = e.now.Add(-e.a.Cfg.RetryAfter.Duration) // 汇总按去掉标记那天记
	if s := e.nightlyText(); !strings.Contains(s, "agent/x") {
		t.Fatalf("当天汇总应记一行：%s", s)
	}
}

func (e *orphanEnv) nightlyText() string {
	b, _ := os.ReadFile(e.a.NightlyPath(e.now))
	return string(b)
}

func TestScanOrphansTakeoverFacts(t *testing.T) {
	t.Run("分支有未合并提交", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		wt := e.branch("agent/x", true)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
		before := e.backlog()
		got := e.a.ScanOrphans(e.st)
		if len(got) != 1 || got[0].Facts.Branch != "agent/x" || got[0].Facts.Ahead != 1 || got[0].Project.Name != "demo" {
			t.Fatalf("应接手：%+v", got)
		}
		if testkit.RealPath(t, got[0].Facts.Worktree) != testkit.RealPath(t, wt) {
			t.Fatalf("worktree：%s，应为 %s", got[0].Facts.Worktree, wt)
		}
		if e.backlog() != before {
			t.Fatal("接手前不改 BACKLOG")
		}
	})
	t.Run("worktree 有未提交改动", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		wt := e.branch("agent/x", false)
		os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0o644)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
		got := e.a.ScanOrphans(e.st)
		if len(got) != 1 || !got[0].Facts.Dirty || got[0].Facts.Ahead != 0 {
			t.Fatalf("应接手：%+v", got)
		}
	})
	t.Run("分支在、worktree 已删", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		wt := e.branch("agent/x", true)
		os.RemoveAll(wt)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
		got := e.a.ScanOrphans(e.st)
		if len(got) != 1 || got[0].Facts.Worktree != "" || got[0].Facts.Ahead != 1 {
			t.Fatalf("应接手、没有 worktree：%+v", got)
		}
	})
}

func TestScanOrphansSkips(t *testing.T) {
	setup := func(t *testing.T) *orphanEnv {
		e := newOrphanEnv(t, "")
		e.branch("agent/x", true)
		e.setTodo("- [ ] 甲 —— 说明" + e.marker("agent/x", 48))
		return e
	}
	t.Run("有任务在跑", func(t *testing.T) {
		e := setup(t)
		e.st.Jobs = []*state.Job{{ID: "r", Fingerprint: e.item().Fingerprint, Status: state.StatusRunning}}
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("同标题任务在跑（条目正文被 agent 改过）", func(t *testing.T) {
		e := setup(t)
		e.st.Jobs = []*state.Job{{ID: "r", Project: "demo", Title: "甲", Fingerprint: "别的", Status: state.StatusRunning}}
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("有待续跑", func(t *testing.T) {
		e := setup(t)
		end := e.now.Add(-48 * time.Hour)
		e.st.Jobs = []*state.Job{{ID: "r", Fingerprint: e.item().Fingerprint, Status: state.StatusInterrupted, ResumePending: true, EndedAt: &end}}
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("标了卡住", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.branch("agent/x", true)
		e.setTodo("- [ ] 甲（卡住：agent/x 两次没过）" + e.marker("agent/x", 48))
		before := e.backlog()
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("巡逻写了接手待做", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.branch("agent/x", true)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 48) + "\n- [ ] 接手中断的「甲」 —— 巡逻发现")
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("巡逻已经写了接手待做，不重复接手：%+v", got)
		}
	})
	t.Run("正文顺带提到进行中", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.setTodo("- [ ] 修标记 —— 条目写着（进行中）时不调度；验收：看 README")
		old := e.now.Add(-48 * time.Hour)
		os.Chtimes(e.p.BacklogPath(), old, old)
		before := e.backlog()
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
			t.Fatalf("正文里的字样不是标记：%+v\n%s", got, e.backlog())
		}
	})
	t.Run("注释和已完成里的标记", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		text := "# 待办\n\n## 待做\n\n<!-- - [ ] 甲（进行中） -->\n\n## 已完成\n- [x] 乙（进行中）\n"
		os.WriteFile(e.p.BacklogPath(), []byte(text), 0o644)
		e.git("add", ".")
		e.git("commit", "-q", "-m", "x")
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != text {
			t.Fatalf("%+v", got)
		}
	})
}

func TestScanOrphansErrorsLeaveAlone(t *testing.T) {
	t.Run("提交不了时不留等待记录", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.setTodo("- [ ] 甲" + e.marker("agent/gone", 48))
		e.git("checkout", "-q", "-b", "other")
		e.a.ScanOrphans(e.st)
		if !e.item().InProgress || len(e.st.ItemHold) != 0 {
			t.Fatalf("没去掉标记就不该等：%+v", e.st.ItemHold)
		}
	})
	t.Run("主目录不在 main", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.setTodo("- [ ] 甲（进行中）")
		old := e.now.Add(-48 * time.Hour)
		os.Chtimes(e.p.BacklogPath(), old, old)
		e.git("checkout", "-q", "-b", "other")
		os.Chtimes(e.p.BacklogPath(), old, old)
		e.a.ScanOrphans(e.st)
		if !e.item().InProgress {
			t.Fatal("不在 main 不该改")
		}
		if !strings.Contains(e.out.String(), "不在 main") {
			t.Fatalf("应记日志：%s", e.out)
		}
		// 回到 main 后下一次 tick 能处理
		e.git("checkout", "-q", "main")
		os.Chtimes(e.p.BacklogPath(), old, old)
		e.a.ScanOrphans(e.st)
		if e.item().InProgress {
			t.Fatal("回到 main 后应去掉标记")
		}
	})
	t.Run("BACKLOG 有没提交的改动", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.setTodo("- [ ] 甲（进行中）")
		os.WriteFile(e.p.BacklogPath(), []byte(e.backlog()+"用户正在写\n"), 0o644)
		old := e.now.Add(-48 * time.Hour)
		os.Chtimes(e.p.BacklogPath(), old, old)
		before := e.backlog()
		e.a.ScanOrphans(e.st)
		if e.backlog() != before {
			t.Fatal("有没提交的改动不该动")
		}
	})
	t.Run("不是真仓库", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		os.RemoveAll(filepath.Join(e.p.Dir, ".git"))
		os.MkdirAll(filepath.Join(e.p.Dir, ".git"), 0o755) // 假 .git：scanProjects 认，git 不认
		text := "## 待做\n- [ ] 甲（进行中：agent/x 2020-01-01 10:00）\n"
		os.WriteFile(e.p.BacklogPath(), []byte(text), 0o644)
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != text {
			t.Fatalf("查不了 git 不该动：%+v", got)
		}
		if !strings.Contains(e.out.String(), "查不了") {
			t.Fatalf("应记日志：%s", e.out)
		}
	})
	t.Run("BACKLOG 读不了", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		os.Remove(e.p.BacklogPath())
		os.Mkdir(e.p.BacklogPath(), 0o755)
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("projects_root 不存在", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.a.Cfg.ProjectsRoot = filepath.Join(e.root, "没有")
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
}

// 接手过的条目：任务结束后还标进行中就标卡住并通知；分支和 worktree 都留着；不再接手第二次。
func TestScanOrphansStuckAfterTakeover(t *testing.T) {
	e := newOrphanEnv(t, "")
	wt := e.branch("agent/x", true)
	e.setTodo("- [ ] 甲 —— 说明" + e.marker("agent/x", 48))
	ended := e.now.Add(-time.Minute)
	e.st.Jobs = []*state.Job{{ID: "t", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusInterrupted, Takeover: true, PID: 1, EndedAt: &ended, StartedAt: ended}}
	if got := e.a.ScanOrphans(e.st); len(got) != 0 {
		t.Fatalf("不该第二次接手：%+v", got)
	}
	b := e.backlog()
	if !strings.Contains(b, "（卡住：接手后仍未完成，分支 agent/x）") || strings.Contains(b, "进行中") {
		t.Fatalf("应换成卡住标记：\n%s", b)
	}
	if it := e.item(); !it.Stuck || it.InProgress || it.Eligible() {
		t.Fatalf("解析：%+v", it)
	}
	n := e.pending()
	if len(n) != 1 || n[0].Cat != "alert" || n[0].JobID != "t" || !strings.Contains(n[0].Text, "甲") {
		t.Fatalf("应通知：%+v", n)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("worktree 不该被删")
	}
	if e.git("rev-parse", "--verify", "agent/x") == "" {
		t.Fatal("分支不该被删")
	}
	// 再扫一遍不重复
	c := e.commits()
	e.a.ScanOrphans(e.st)
	if e.commits() != c || len(e.pending()) != 1 {
		t.Fatal("标过卡住就不再处理")
	}
}

// 接手任务结束后又有人在分支上动（用户开了会话）：先不标卡住。
func TestScanOrphansTakeoverThenOwnerActive(t *testing.T) {
	e := newOrphanEnv(t, "")
	wt := e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	ended := e.now.Add(-2 * time.Hour)
	e.st.Jobs = []*state.Job{{ID: "t", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, Takeover: true, PID: 1, EndedAt: &ended, StartedAt: ended}}
	touched := e.now.Add(-time.Hour)
	os.Chtimes(filepath.Join(wt, "work.txt"), touched, touched)
	before := e.backlog()
	if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
		t.Fatalf("用户在动，不该标卡住：%+v\n%s", got, e.backlog())
	}
	// 用户也走了 24 小时
	e.now = e.now.Add(25 * time.Hour)
	if e.a.ScanOrphans(e.st); !e.item().Stuck {
		t.Fatalf("超过 24 小时应标卡住：\n%s", e.backlog())
	}
}

// 接手过的条目用户重新认领（标记时间晚于接手任务结束）：24 小时内不标卡住、不再接手；过了窗口才照常。
func TestScanOrphansReclaimedAfterTakeover(t *testing.T) {
	setup := func(t *testing.T, transient bool) (*orphanEnv, string) {
		e := newOrphanEnv(t, "")
		e.branch("agent/x", true)
		ended := e.now.Add(-48 * time.Hour)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 1)) // 用户 1 小时前写的
		e.st.Jobs = []*state.Job{{ID: "t", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, Takeover: true, Transient: transient, PID: 1, EndedAt: &ended, StartedAt: ended}}
		return e, e.backlog()
	}
	t.Run("普通路径", func(t *testing.T) {
		e, before := setup(t, false)
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
			t.Fatalf("用户刚认领不该动：%+v\n%s", got, e.backlog())
		}
		e.now = e.now.Add(25 * time.Hour)
		if e.a.ScanOrphans(e.st); !e.item().Stuck {
			t.Fatalf("过了 24 小时应标卡住：\n%s", e.backlog())
		}
	})
	t.Run("瞬时错误路径", func(t *testing.T) {
		e, before := setup(t, true)
		if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
			t.Fatalf("用户刚认领不该再接手：%+v", got)
		}
		e.now = e.now.Add(25 * time.Hour)
		if got := e.a.ScanOrphans(e.st); len(got) != 1 {
			t.Fatalf("过了 24 小时可以再接手：%+v", got)
		}
	})
	t.Run("瞬时错误路径：分支刚有人动", func(t *testing.T) {
		e, _ := setup(t, true)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 72))
		e.st.Jobs[0].Fingerprint = e.item().Fingerprint
		wt := filepath.Join(e.p.Dir, ".claude", "worktrees", "x")
		touched := e.now.Add(-time.Hour)
		os.Chtimes(filepath.Join(wt, "work.txt"), touched, touched)
		if got := e.a.ScanOrphans(e.st); len(got) != 0 {
			t.Fatalf("分支有人在动，不该再接手：%+v", got)
		}
	})
	t.Run("标记早于接手任务结束不算", func(t *testing.T) {
		e := newOrphanEnv(t, "")
		e.branch("agent/x", true)
		e.setTodo("- [ ] 甲" + e.marker("agent/x", 3))
		ended := e.now.Add(-time.Hour)
		e.st.Jobs = []*state.Job{{ID: "t", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, Takeover: true, PID: 1, EndedAt: &ended, StartedAt: ended}}
		if e.a.ScanOrphans(e.st); !e.item().Stuck {
			t.Fatalf("应标卡住：\n%s", e.backlog())
		}
	})
}

// 接手任务撞上游瞬时错误（没花钱、没开工）：等退避后再接手，不算用掉那一次；重试用完标卡住。
func TestScanOrphansTakeoverTransient(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	fp := e.item().Fingerprint
	ended := e.now.Add(-time.Minute)
	j1 := &state.Job{ID: "t1", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: fp, Status: state.StatusFailed, Takeover: true, Transient: true, PID: 1, EndedAt: &ended, StartedAt: ended}
	e.st.Jobs = []*state.Job{j1}
	before := e.backlog()
	if got := e.a.ScanOrphans(e.st); len(got) != 0 || e.backlog() != before {
		t.Fatalf("退避期内不动：%+v", got)
	}
	e.now = e.now.Add(16 * time.Minute)
	if got := e.a.ScanOrphans(e.st); len(got) != 1 {
		t.Fatalf("退避过后应再接手：%+v", got)
	}
	end2 := e.now
	e.st.Jobs = append(e.st.Jobs, &state.Job{ID: "t2", Project: "demo", ProjectDir: e.p.Dir, Title: "甲", Fingerprint: fp, Status: state.StatusFailed, Takeover: true, PID: 1, EndedAt: &end2, StartedAt: end2})
	if e.a.ScanOrphans(e.st); !e.item().Stuck {
		t.Fatalf("最后一次不是瞬时错误应标卡住：\n%s", e.backlog())
	}
}

// 接手任务启动失败（进程都没起来）：不算接手过。
func TestScanOrphansTakeoverStartFailedNotSpent(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	ended := e.now.Add(-48 * time.Hour)
	e.st.Jobs = []*state.Job{{ID: "t", Fingerprint: e.item().Fingerprint, Status: state.StatusFailed, Takeover: true, EndedAt: &ended, StartedAt: ended}}
	if got := e.a.ScanOrphans(e.st); len(got) != 1 {
		t.Fatalf("启动失败不算接手过：%+v", got)
	}
}

// 孤儿扫描一个模型调用都不起：假 claude 换成没有的路径也能跑完。
func TestScanOrphansNoClaude(t *testing.T) {
	e := newOrphanEnv(t, "")
	e.a.Cfg.ClaudePath = filepath.Join(e.root, "没有这个程序")
	e.branch("agent/x", true)
	e.setTodo("- [ ] 甲" + e.marker("agent/x", 48))
	if got := e.a.ScanOrphans(e.st); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}
