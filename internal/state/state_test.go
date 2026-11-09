package state

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
)

// 老状态文件没有新字段：读得出，照跑；新字段是零值时不写进文件。
func TestRoutineMarkCompat(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"jobs":[],"cooldown_until":"0001-01-01T00:00:00Z","breaker_until":"0001-01-01T00:00:00Z"}`), &st); err != nil || st.RoutineMarks != nil {
		t.Fatalf("%v %+v", err, st)
	}
	b, err := json.Marshal(&State{Jobs: []*Job{{ID: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"routine_marks", "budget_hit", "auto_stop", "stuck_pending", "stuck_marked", "stuck_branch", "stuck_worktree", "handoff"} {
		if strings.Contains(string(b), key) {
			t.Errorf("零值不写：%s", b)
		}
	}
}

// 正常写出的状态（一个任务都没有时 jobs 是 null）不算损坏。
func TestRegressEmptySavedStateIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saveState 写出的空状态被当成损坏：%v", err)
	}
}

// 孤儿标记去掉后的等待按项目算：别的项目里同样文字的条目不用等。老状态文件里只有指纹的键照样认。
func TestRegressItemHoldPerProject(t *testing.T) {
	t.Parallel()
	now := time.Now()
	st := &State{ItemHold: map[string]time.Time{HoldKey("alpha", "fp"): now.Add(time.Hour)}}
	if !RetryBlocked(st, "alpha", "fp", now, time.Hour) {
		t.Error("alpha 要等")
	}
	if RetryBlocked(st, "beta", "fp", now, time.Hour) {
		t.Error("beta 不用等")
	}
	if !RetryBlocked(&State{ItemHold: map[string]time.Time{"fp": now.Add(time.Hour)}}, "beta", "fp", now, time.Hour) {
		t.Error("老状态文件的键要认")
	}
}

// 边界：retry_after 比退避还短时不会更慢；普通失败打断连续计数；撞上限不打断也不算。
func TestTransientRetryEdges(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	ended := func(d time.Duration, trans, limit bool) *Job {
		return &Job{Fingerprint: "fp", Status: StatusFailed, EndedAt: at(d), Transient: trans, LimitHit: limit}
	}
	st := &State{Jobs: []*Job{ended(-time.Minute, true, false)}}
	if RetryBlocked(st, "", "fp", now, 30*time.Second) {
		t.Error("retry_after 比退避短时按 retry_after")
	}
	if !RetryBlocked(st, "", "fp", now, 12*time.Hour) {
		t.Error("15 分钟内应挡住")
	}
	st = &State{Jobs: []*Job{ended(-3*time.Hour, true, false), ended(-2*time.Hour, true, false), ended(-time.Hour, false, false), ended(-time.Minute, true, false)}}
	if got := TransientStreak(st, "", "fp"); got != 1 {
		t.Errorf("普通失败应打断连续计数：%d", got)
	}
	st = &State{Jobs: []*Job{ended(-3*time.Hour, true, false), ended(-2*time.Hour, false, true), ended(-time.Hour, true, false)}}
	if got := TransientStreak(st, "", "fp"); got != 2 {
		t.Errorf("撞上限不打断计数：%d", got)
	}
	st = &State{Jobs: []*Job{{Fingerprint: "fp", Status: StatusRunning}, ended(-time.Hour, true, false)}}
	if got := TransientStreak(st, "", "fp"); got != 1 {
		t.Errorf("运行中的不算：%d", got)
	}
	if got := TransientStreak(&State{}, "", ""); got != 0 {
		t.Errorf("空指纹：%d", got)
	}
}

func TestCapacity(t *testing.T) {
	cfg := config.Default() // max_parallel 2
	cfg.PerProject = 1      // 这里只看名额怎么算；默认 2 时同项目第二条还要看范围，由 runner.Pick 判
	st := &State{Jobs: []*Job{
		{Project: "a", Status: StatusRunning},
		{Project: "b", Status: StatusDone},
	}}
	if ok, _ := Capacity(st, cfg, "a"); ok {
		t.Error("项目 a 已有 1 个在跑，不应再启动")
	}
	if ok, _ := Capacity(st, cfg, "b"); !ok {
		t.Error("项目 b 没有在跑的，全局 1/2，应能启动")
	}
	st.Jobs = append(st.Jobs, &Job{Project: "c", Status: StatusRunning})
	if ok, why := Capacity(st, cfg, "b"); ok || !strings.Contains(why, "max_parallel") {
		t.Errorf("全局 2/2 应已满：%v %s", ok, why)
	}
	cfg.PerProject = 2
	cfg.MaxParallel = 5
	if ok, _ := Capacity(st, cfg, "a"); !ok {
		t.Error("per_project=2 时项目 a 还能再启动一个")
	}
	// 夜班、复盘不占名额
	cfg = config.Default()
	st.Jobs = []*Job{{Project: "a", Status: StatusRunning}, {Project: "夜班", Kind: config.KindNight, Status: StatusRunning},
		{Project: "复盘", Kind: config.KindReview, Status: StatusRunning}}
	if ok, why := Capacity(st, cfg, "b"); !ok {
		t.Errorf("夜班、复盘不应占全局名额：%s", why)
	}
}
