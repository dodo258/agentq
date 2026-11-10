package sched

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
	"agentq/internal/textfmt"
)

const retentionTestID = "1104-120000-demo"

// wireSched 建模块实例。
func wireSched(e *env.Env) *Service {
	return &Service{Env: e, Ledger: stack.New(e).Ledger}
}

func retentionApp(t *testing.T) (*Service, *bytes.Buffer, time.Time) {
	t.Helper()
	out := &bytes.Buffer{}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	a := wireSched(&env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: func() time.Time { return now }, Out: out})
	return a, out, now
}

// ageDir 建一个目录，里面一个文件，都拨到 at。
func ageDir(t *testing.T, path string, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(path, "meta.json")
	testkit.WriteInstall(t, inner, "{}", 0o644)
	setTimes(t, inner, at)
	setTimes(t, path, at)
	return path
}

func ageFile(t *testing.T, path string, at time.Time) string {
	t.Helper()
	testkit.WriteInstall(t, path, "x", 0o644)
	setTimes(t, path, at)
	return path
}

func setTimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func wantGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("应该删掉：%s", p)
		}
	}
}

func wantKept(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("不该删：%s（%v）", p, err)
		}
	}
}

// 超过天数的都删；没超过的、不是 agentq 自己命名的和别的文件都留着。
func TestRetentionDeletesOldKeepsTheRest(t *testing.T) {
	a, out, now := retentionApp(t)
	d := a.DataDir()
	old, fresh := now.Add(-120*24*time.Hour), now.Add(-24*time.Hour)
	oldID, newID := "0701-120000-demo", retentionTestID
	gone := []string{
		ageDir(t, filepath.Join(d, "jobs", oldID), old),
		ageDir(t, filepath.Join(d, "verify", oldID), old),
		ageFile(t, filepath.Join(d, "replies", oldID+".md"), old),
	}
	kept := []string{
		ageDir(t, filepath.Join(d, "jobs", newID), fresh),
		ageDir(t, filepath.Join(d, "verify", newID), fresh),
		ageFile(t, filepath.Join(d, "replies", newID+".md"), fresh),
		// 不是 agentq 命名的：不认识的东西不动
		ageDir(t, filepath.Join(d, "jobs", "my-notes"), old),
		ageFile(t, filepath.Join(d, "replies", "keep.txt"), old),
		// 不在清理范围：用户的成果、状态
		ageFile(t, filepath.Join(d, "patrol.json"), old),
	}
	a.retention(&state.State{})
	wantGone(t, gone...)
	wantKept(t, kept...)
	testkit.AssertContains(t, out.String(), "清理", "6")
}

// 刚好到天数的留着，多一点才删；0 表示不删。
func TestRetentionDaysBoundary(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now.Add(-time.Hour))
	edge := ageDir(t, filepath.Join(d, "jobs", "0806-120000-edge"), now.Add(-time.Hour).Add(-90*24*time.Hour))
	over := ageDir(t, filepath.Join(d, "jobs", "0806-110000-over"), now.Add(-time.Hour).Add(-90*24*time.Hour-time.Minute))
	a.retention(&state.State{})
	wantKept(t, edge)
	wantGone(t, over)

	a, _, now = retentionApp(t)
	a.Cfg.RetentionDays = 0
	d = a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	never := ageDir(t, filepath.Join(d, "jobs", "0101-120000-old"), now.Add(-1000*24*time.Hour))
	a.retention(&state.State{})
	wantKept(t, never)

	a, _, now = retentionApp(t)
	a.Cfg.RetentionDays = 7
	d = a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	week := ageDir(t, filepath.Join(d, "jobs", "1020-120000-w"), now.Add(-10*24*time.Hour))
	a.retention(&state.State{})
	wantGone(t, week)
}

// 目录里的文件比目录新：按新的算（任务目录里的文件后来还在写）。
func TestRetentionUsesNewestDirectChild(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	job := ageDir(t, filepath.Join(d, "jobs", "0701-120000-demo"), now.Add(-120*24*time.Hour))
	setTimes(t, filepath.Join(job, "meta.json"), now.Add(-24*time.Hour))
	setTimes(t, job, now.Add(-120*24*time.Hour))
	a.retention(&state.State{})
	wantKept(t, job)
}

// state.json 还引用的都不删：运行中、等续跑、HeldProjects、StuckPending、普通结束的记录，连同被续跑的那一个。
func TestRetentionKeepsWhatStateReferences(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	old := now.Add(-300 * 24 * time.Hour)
	ended := now.Add(-200 * 24 * time.Hour)
	st := &state.State{Jobs: []*state.Job{
		{ID: "0101-000001-running", Status: state.StatusRunning},
		{ID: "0101-000002-resume", Status: state.StatusInterrupted, ResumePending: true, EndedAt: &ended},
		{ID: "0101-000003-held", Status: state.StatusKilled, HeldProjects: []string{"x"}, EndedAt: &ended},
		{ID: "0101-000004-stuck", Status: state.StatusFailed, StuckPending: "原因", EndedAt: &ended},
		{ID: "0101-000005-ended", Status: state.StatusDone, ResumeOf: "0101-000006-orig", EndedAt: &ended},
	}}
	ids := []string{"0101-000001-running", "0101-000002-resume", "0101-000003-held", "0101-000004-stuck", "0101-000005-ended", "0101-000006-orig"}
	var kept []string
	for _, id := range ids {
		kept = append(kept, ageDir(t, filepath.Join(d, "jobs", id), old))
		kept = append(kept, ageDir(t, filepath.Join(d, "verify", id), old))
		kept = append(kept, ageFile(t, filepath.Join(d, "replies", id+".md"), old))
	}
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	gone := ageDir(t, filepath.Join(d, "jobs", "0101-000007-free"), old)
	a.retention(st)
	wantKept(t, kept...)
	wantGone(t, gone)
}

// 门和升级前核验的一次性 worktree 还在：不删（Git 里还登记着，要走 gate.RemoveWorktree）。
func TestRetentionKeepsGateWorktree(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	old := now.Add(-120 * 24 * time.Hour)
	for _, parent := range []string{"gate", "verify"} {
		dir := ageDir(t, filepath.Join(d, parent, "0701-120000-demo"), old)
		ageDir(t, gate.Worktree(dir), old)
		setTimes(t, dir, old)
		a.retention(&state.State{})
		wantKept(t, dir)
		os.Remove(a.retentionDayPath())
	}
}

// 目录都不存在：什么也不做，不报错。
func TestRetentionNothingThere(t *testing.T) {
	a, out, _ := retentionApp(t)
	a.retention(&state.State{})
	if strings.Contains(out.String(), "失败") {
		t.Fatalf("不该报错：%s", out)
	}
}

// 删不掉的只记日志，别的照删。
func TestRetentionDeleteFailureIsOnlyLogged(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受权限限制")
	}
	a, out, now := retentionApp(t)
	d := a.DataDir()
	old := now.Add(-120 * 24 * time.Hour)
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	stuck := ageDir(t, filepath.Join(d, "jobs", "0701-120000-locked"), old)
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	setTimes(t, stuck, old)
	t.Cleanup(func() { os.Chmod(stuck, 0o755) })
	other := ageDir(t, filepath.Join(d, "verify", "0701-120000-other"), old)
	a.retention(&state.State{})
	wantKept(t, stuck)
	wantGone(t, other)
	testkit.AssertContains(t, out.String(), "删不掉", "0701-120000-locked")
}

// 不跟软链接：清理目录本身是软链接时不进去；条目是软链接时不动它，也不动它指向的东西；条目里的软链接只算它自己。
func TestRetentionDoesNotFollowSymlinks(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	old := now.Add(-120 * 24 * time.Hour)
	outside := t.TempDir()
	victim := ageDir(t, filepath.Join(outside, "0701-120000-victim"), old)

	// jobs 是指向外面的软链接
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(d, "jobs")); err != nil {
		t.Fatal(err)
	}
	// verify 下的条目是软链接
	if err := os.MkdirAll(filepath.Join(d, "verify"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "verify", "0701-120000-link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	// 普通条目里有指向外面的软链接：删条目，不碰外面
	outsideFile := ageFile(t, filepath.Join(outside, "secret.txt"), old)
	entry := ageDir(t, filepath.Join(d, "gate", "0701-120000-in"), old)
	sub := filepath.Join(entry, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(sub, "link")); err != nil {
		t.Fatal(err)
	}
	setTimes(t, sub, old)
	setTimes(t, entry, old)
	// replies 里的软链接
	if err := os.MkdirAll(filepath.Join(d, "replies"), 0o755); err != nil {
		t.Fatal(err)
	}
	replyLink := filepath.Join(d, "replies", "0701-120000-r.md")
	if err := os.Symlink(outsideFile, replyLink); err != nil {
		t.Fatal(err)
	}
	ageFile(t, filepath.Join(d, "replies", retentionTestID+".md"), now)

	a.retention(&state.State{})
	wantKept(t, victim, filepath.Join(victim, "meta.json"), link, outsideFile, replyLink)
	wantGone(t, entry)
}

// 时钟往前跳一年：按记录里最新的时间算，不把全部记录当过期。
func TestRetentionClockJumpForwardKeepsAll(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	kept := []string{
		ageDir(t, filepath.Join(d, "jobs", retentionTestID), now.Add(-time.Hour)),
		ageDir(t, filepath.Join(d, "jobs", "1001-120000-a"), now.Add(-30*24*time.Hour)),
		ageFile(t, filepath.Join(d, "replies", "1001-120000-a.md"), now.Add(-30*24*time.Hour)),
	}
	really := ageDir(t, filepath.Join(d, "jobs", "0701-120000-really-old"), now.Add(-120*24*time.Hour))
	jumped := now.Add(365 * 24 * time.Hour)
	a.Now = func() time.Time { return jumped }
	a.retention(&state.State{})
	wantKept(t, kept...)
	wantGone(t, really) // 相对最新的记录确实过期
}

// 时钟往回跳：比现在还新的记录不算过期，也不会把基准抬到现在之后。
func TestRetentionClockJumpBackKeepsAll(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	future := ageDir(t, filepath.Join(d, "jobs", "1230-120000-future"), now.Add(400*24*time.Hour))
	recent := ageDir(t, filepath.Join(d, "jobs", retentionTestID), now.Add(-24*time.Hour))
	a.retention(&state.State{})
	wantKept(t, future, recent)

	// 只有未来的记录
	a, _, now = retentionApp(t)
	only := ageDir(t, filepath.Join(a.DataDir(), "jobs", "1230-120000-only"), now.Add(400*24*time.Hour))
	a.retention(&state.State{})
	wantKept(t, only)
}

// 每天最多一次：同一天不重复扫，第二天再扫。
func TestRetentionOncePerDay(t *testing.T) {
	a, _, now := retentionApp(t)
	d := a.DataDir()
	ageDir(t, filepath.Join(d, "jobs", retentionTestID), now)
	a.retention(&state.State{})
	later := ageDir(t, filepath.Join(d, "jobs", "0701-120000-later"), now.Add(-120*24*time.Hour))
	a.retention(&state.State{})
	wantKept(t, later)
	next := now.Add(24 * time.Hour)
	a.Now = func() time.Time { return next }
	ageDir(t, filepath.Join(d, "jobs", "1105-120000-today"), next)
	a.retention(&state.State{})
	wantGone(t, later)
}

// launchd.log 超过上限只留尾部：从整行开始，同一个文件（常驻服务还开着它），最新的行还在。
func TestRetentionTrimsLog(t *testing.T) {
	a, _, _ := retentionApp(t)
	path := a.LaunchdLogPath()
	line := strings.Repeat("x", 99) + "\n"
	var b strings.Builder
	for b.Len() < retentionLogMax+1 {
		b.WriteString(line)
	}
	testkit.WriteInstall(t, path, "最早的一行\n"+b.String()+"最新的一行\n", 0o644)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a.retention(&state.State{})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("要在原文件上截断：常驻服务的文件描述符还指着它")
	}
	if after.Size() > retentionLogKeep || after.Size() == 0 {
		t.Fatalf("大小不对：%d", after.Size())
	}
	got := testkit.ReadFile(t, path)
	if !strings.HasSuffix(got, "最新的一行\n") || strings.Contains(got, "最早的一行") {
		t.Fatal("应只留尾部")
	}
	first, _, _ := strings.Cut(got, "\n")
	if first != strings.Repeat("x", 99) {
		t.Fatalf("应从整行开始：%q", textfmt.TruncRunes(first, 20))
	}
}

// 没到上限不动；刚好到上限不动；一行也没有换行的长日志照样截；软链接不跟；retention_days=0 也截日志。
func TestRetentionLogEdges(t *testing.T) {
	a, _, _ := retentionApp(t)
	path := a.LaunchdLogPath()
	exact := strings.Repeat("y", retentionLogMax-1) + "\n"
	testkit.WriteInstall(t, path, exact, 0o644)
	a.retention(&state.State{})
	if fi, err := os.Stat(path); err != nil || fi.Size() != retentionLogMax {
		t.Fatalf("刚好到上限不该动：%v %v", fi, err)
	}

	a, _, _ = retentionApp(t)
	a.Cfg.RetentionDays = 0
	path = a.LaunchdLogPath()
	testkit.WriteInstall(t, path, strings.Repeat("z", retentionLogMax+10), 0o644)
	a.retention(&state.State{})
	if fi, err := os.Stat(path); err != nil || fi.Size() > retentionLogKeep || fi.Size() == 0 {
		t.Fatalf("没有换行也要截：%v %v", fi, err)
	}

	a, _, _ = retentionApp(t)
	target := filepath.Join(t.TempDir(), "outside.log")
	big := strings.Repeat("z", retentionLogMax+10)
	testkit.WriteInstall(t, target, big, 0o644)
	if err := os.MkdirAll(a.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, a.LaunchdLogPath()); err != nil {
		t.Fatal(err)
	}
	a.retention(&state.State{})
	if fi, err := os.Stat(target); err != nil || fi.Size() != int64(len(big)) {
		t.Fatalf("软链接不跟：%v %v", fi, err)
	}
}
