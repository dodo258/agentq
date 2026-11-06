package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/runner"
	"agentq/internal/state"
)

func TestRecordOutcome(t *testing.T) {
	cfg := config.Default()
	now := time.Date(2026, 10, 10, 3, 0, 0, 0, time.Local)
	st := &State{}
	rec := func(status string, ran time.Duration, limit bool) (bool, time.Duration) {
		return runner.RecordOutcome(st, cfg, &Job{ID: "x", Status: status, LimitHit: limit}, ran, now)
	}
	quick := func() (bool, time.Duration) { return rec(state.StatusFailed, 30*time.Second, false) }

	// 前两个秒退只计数；中间夹一个被 stop 的、一个撞上限的，不清零也不计
	quick()
	rec(state.StatusKilled, time.Second, false)
	rec(state.StatusFailed, time.Second, true)
	if tripped, _ := quick(); tripped || st.FailStreak != 2 {
		t.Fatalf("两个秒退不应熔断：streak %d", st.FailStreak)
	}
	// 第三个（中断也算没做完）熔断 30 分钟
	if tripped, d := rec(state.StatusInterrupted, 2*time.Minute, false); !tripped || d != 30*time.Minute {
		t.Fatalf("第三个秒退应熔断 30m：%v %s", tripped, d)
	}
	if !st.BreakerUntil.Equal(now.Add(30*time.Minute)) || st.BreakerLevel != 1 || st.FailStreak != 0 {
		t.Fatalf("熔断状态：%+v", st)
	}
	// 熔断中结束的秒退不计
	if tripped, _ := quick(); tripped || st.FailStreak != 0 {
		t.Fatalf("熔断中的秒退不应计数：%+v", st)
	}
	// 熔断到期后第一个秒退直接再熔断、时长翻倍，上限 4 小时
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		now = st.BreakerUntil
		if tripped, d := quick(); !tripped || d != want {
			t.Fatalf("应熔断 %s：%v %s", want, tripped, d)
		}
	}
	// 熔断中有任务跑完（比如用户 run 试跑成功）：解除熔断，计数和翻倍都清零，之后又要连续 3 个才熔断
	rec(state.StatusDone, 10*time.Second, false)
	if !st.BreakerUntil.IsZero() || st.BreakerLevel != 0 || st.FailStreak != 0 {
		t.Fatalf("跑完应解除熔断并清零：%+v", st)
	}
	if tripped, _ := quick(); tripped {
		t.Fatal("清零后第一个秒退不应熔断")
	}
	// 跑了超过 breaker_quick 才失败的也清零
	rec(state.StatusFailed, 5*time.Minute, false)
	if st.FailStreak != 0 {
		t.Fatalf("跑了一阵才失败应清零：%+v", st)
	}
}

// 秒退按 claude 实际退出的时间算（结果文件的修改时间），不按收尾时间：只靠定时 tick 时收尾可能晚好几分钟。
func TestExitedAt(t *testing.T) {
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Now: time.Now}})
	start := time.Now().Add(-10 * time.Minute)
	ended := time.Now()
	j := &Job{ID: "1010-000000-x", StartedAt: start, EndedAt: &ended}
	if got := a.Runner.ExitedAt(j); !got.Equal(ended) {
		t.Errorf("没有结果文件时应用收尾时间：%v", got)
	}
	os.MkdirAll(a.JobDir(j.ID), 0o755)
	p := filepath.Join(a.JobDir(j.ID), "result.json")
	os.WriteFile(p, []byte("{}"), 0o644)
	exited := start.Add(20 * time.Second)
	os.Chtimes(p, exited, exited)
	if got := a.Runner.ExitedAt(j); !got.Equal(exited) {
		t.Errorf("应取 result.json 的修改时间：%v，应为 %v", got, exited)
	}
	// 超时被杀的任务：result.json 是启动时建的空文件，修改时间在启动时刻，不能拿来算，否则会被当成秒退
	os.WriteFile(p, nil, 0o644)
	os.Chtimes(p, start.Add(50*time.Millisecond), start.Add(50*time.Millisecond))
	if got := a.Runner.ExitedAt(j); !got.Equal(ended) {
		t.Errorf("空的 result.json 不应采用：%v", got)
	}
	st := &State{}
	j.Status = state.StatusTimeout
	for i := 0; i < 3; i++ {
		if tripped, _ := runner.RecordOutcome(st, config.Default(), j, a.Runner.ExitedAt(j).Sub(j.StartedAt), ended); tripped {
			t.Fatal("超时的任务不应算秒退")
		}
	}
}

func TestResumeDepth(t *testing.T) {
	st := &State{Jobs: []*Job{{ID: "a"}, {ID: "b", ResumeOf: "a"}, {ID: "c", ResumeOf: "b"}}}
	for i, want := range []int{0, 1, 2} {
		if got := runner.ResumeDepth(st, st.Jobs[i]); got != want {
			t.Errorf("%s 的续跑深度应为 %d：%d", st.Jobs[i].ID, want, got)
		}
	}
}

// 夜班、复盘：秒退照样计数，跑完或跑了一阵才结束不清零、不解除熔断（它们正常不说明待做任务好了）。
func TestRecordOutcomeRoutine(t *testing.T) {
	cfg := config.Default()
	now := time.Date(2026, 10, 11, 6, 45, 0, 0, time.Local)
	st := &State{FailStreak: 1, BreakerLevel: 1}
	if tripped, _ := runner.RecordOutcome(st, cfg, &Job{Kind: config.KindReview, Status: state.StatusDone}, 2*time.Hour, now); tripped ||
		st.FailStreak != 1 || st.BreakerLevel != 1 {
		t.Fatalf("复盘跑完不应清零：%+v", st)
	}
	st.BreakerUntil = now.Add(time.Hour)
	runner.RecordOutcome(st, cfg, &Job{Kind: config.KindNight, Status: state.StatusDone}, 4*time.Hour, now)
	if !st.BreakerUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("夜班跑完不应解除熔断：%+v", st)
	}
	st.BreakerUntil = time.Time{}
	if tripped, _ := runner.RecordOutcome(st, cfg, &Job{Kind: config.KindNight, Status: state.StatusFailed}, 10*time.Second, now); !tripped {
		t.Fatalf("熔断过一次后夜班秒退应再熔断：%+v", st)
	}
}
