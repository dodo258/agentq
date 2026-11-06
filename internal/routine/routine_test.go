package routine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/inbox"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// wireRoutine 建模块实例。
func wireRoutine(e *env.Env) *Service {
	k := stack.New(e)
	ib := &inbox.Service{Env: e, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner}
	return &Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Inbox: ib, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner, Service: k.Service}
}

// newRoutineApp 带临时 HOME 的实例。
func newRoutineApp(t *testing.T) *Service {
	t.Helper()
	return wireRoutine(&env.Env{Home: t.TempDir(), Now: time.Now, Out: &bytes.Buffer{}, Cfg: config.Default()})
}

// 单元：别的任务在跑、ctx 超时都算有事；没有 github 远程的项目不查 gh。
func TestRoutineBusyEdges(t *testing.T) {
	a := newRoutineApp(t)
	root := t.TempDir()
	a.Cfg.ProjectsRoot = root
	a.Now = func() time.Time { return time.Date(2026, 11, 5, 1, 0, 0, 0, time.Local) }
	os.WriteFile(filepath.Join(a.Home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n"), 0o644)
	dir := testkit.MkProject(t, root, "demo", "")
	os.RemoveAll(filepath.Join(dir, ".git"))
	testkit.GitT(t, a.Home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, a.Home, dir, "add", ".")
	testkit.GitT(t, a.Home, dir, "commit", "-q", "-m", "init")
	head := testkit.GitT(t, a.Home, dir, "rev-parse", "HEAD")
	end := a.Now().Add(-24 * time.Hour)
	st := &state.State{Jobs: []*state.Job{{ID: "n1", Kind: config.KindNight, Status: state.StatusDone, StartedAt: end, EndedAt: &end}},
		RoutineMarks: map[string]*state.RoutineMark{config.KindNight: {JobID: "n1", At: end, Heads: map[string]string{"demo": head}}}}
	r := config.Routines[config.KindNight]
	if why := a.routineBusy(context.Background(), st, r); why != "" {
		t.Fatalf("没事：%s", why)
	}
	// ctx 已经超时：查不出，照跑
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if why := a.routineBusy(ctx, st, r); !strings.HasPrefix(why, "查不出") {
		t.Errorf("超时算查不出：%q", why)
	}
	// 别的任务在跑
	st.Jobs = append(st.Jobs, &state.Job{ID: "t", Status: state.StatusRunning, StartedAt: end})
	if why := a.routineBusy(context.Background(), st, r); why != "有任务在跑" {
		t.Errorf("%q", why)
	}
	st.Jobs = st.Jobs[:1]
	// 时钟往回拨
	st.RoutineMarks[config.KindNight].At = a.Now().Add(time.Hour)
	if why := a.routineBusy(context.Background(), st, r); !strings.Contains(why, "时钟") {
		t.Errorf("%q", why)
	}
	st.RoutineMarks[config.KindNight].At = end
	// projects_root 没了
	a.Cfg.ProjectsRoot = filepath.Join(root, "没有")
	if why := a.routineBusy(context.Background(), st, r); !strings.HasPrefix(why, "查不出") {
		t.Errorf("%q", why)
	}
}

func TestReportWrittenSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.md")
	start := time.Now().Add(-time.Hour)
	if reportWrittenSince(path, start) {
		t.Error("不存在")
	}
	os.WriteFile(path, []byte("真报告\n"), 0o644)
	if !reportWrittenSince(path, start) {
		t.Error("启动后写的算")
	}
	old := start.Add(-time.Minute)
	os.Chtimes(path, old, old)
	if reportWrittenSince(path, start) {
		t.Error("启动前就有的不算")
	}
	same := start.Truncate(time.Second)
	os.Chtimes(path, same, same)
	if !reportWrittenSince(path, start) {
		t.Error("同一秒写的算（文件系统可能只精确到秒）")
	}
	os.WriteFile(path, []byte("没事\n\n"+idleReportMark+"\n"), 0o644)
	if reportWrittenSince(path, start) {
		t.Error("占位报告不算")
	}
	os.WriteFile(path, []byte("没事\n\n"+idleReportMark+"\n\n# 真报告\n正文\n"), 0o644)
	if !reportWrittenSince(path, start) {
		t.Error("接在占位后面写的也算任务写的")
	}
	os.WriteFile(path, []byte("正文提到 "+idleReportMark+" 这几个字\n结尾\n"), 0o644)
	if !reportWrittenSince(path, start) {
		t.Error("正文提到不算占位")
	}
}
