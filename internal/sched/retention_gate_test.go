package sched

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentq/internal/discuss"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func writeAged(t *testing.T, path, body string, at time.Time) {
	t.Helper()
	testkit.WriteInstall(t, path, body, 0o644)
	setTimes(t, path, at)
}

// 审查记录随 retention_days 清；任务还在记录里的（在跑、在过门、卡住的）不清，空目录顺手去掉。
func TestRetentionReviews(t *testing.T) {
	a, _, now := retentionApp(t)
	old := now.Add(-100 * 24 * time.Hour)
	fresh := now.Add(-time.Hour)
	reviews := filepath.Join(a.DataDir(), "gate", "reviews")
	gone := filepath.Join(reviews, "demo", "aaa.json")
	stuck := filepath.Join(reviews, "demo", "bbb.json")
	recent := filepath.Join(reviews, "demo", "ccc.json")
	bad := filepath.Join(reviews, "demo", "ddd.json")
	ui := filepath.Join(reviews, "demo", "ui", "eee.json")
	onlyOld := filepath.Join(reviews, "other", "fff.json")
	other := filepath.Join(reviews, "demo", "notes.txt")
	writeAged(t, gone, `{"job_id":"1001-120000-demo"}`, old)
	writeAged(t, stuck, `{"job_id":"1002-120000-demo"}`, old)
	writeAged(t, recent, `{"job_id":"1003-120000-demo"}`, fresh)
	writeAged(t, bad, `{坏`, old)
	writeAged(t, ui, `{"job_id":"1004-120000-demo"}`, old)
	writeAged(t, onlyOld, `{"job_id":"1005-120000-other"}`, old)
	writeAged(t, other, "不认识的文件", old)
	st := &state.State{Jobs: []*state.Job{{ID: "1002-120000-demo", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateStuck}}}}
	a.removeOldEntries(st)
	for _, p := range []string{gone, bad, ui, onlyOld} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s 要清", p)
		}
	}
	for _, p := range []string{stuck, recent, other} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s 不该清：%v", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(onlyOld)); err == nil {
		t.Error("空目录要去掉")
	}
	if _, err := os.Lstat(filepath.Dir(ui)); err == nil {
		t.Error("ui 空目录要去掉")
	}
}

// 过门目录：卡住的、排队的不清，已经过期的清。
func TestRetentionGateDirs(t *testing.T) {
	a, _, now := retentionApp(t)
	old := now.Add(-100 * 24 * time.Hour)
	gate := filepath.Join(a.DataDir(), "gate")
	stuck := ageDir(t, filepath.Join(gate, "1002-120000-demo"), old)
	queued := ageDir(t, filepath.Join(gate, "1003-120000-demo"), old)
	gone := ageDir(t, filepath.Join(gate, "1001-120000-demo"), old)
	audit := filepath.Join(gate, "audit.log")
	writeAged(t, audit, "x\n", old)
	ageDir(t, filepath.Join(gate, "1004-120000-demo"), now) // 基准是记录里最新的时间
	st := &state.State{Jobs: []*state.Job{
		{ID: "1002-120000-demo", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateStuck}},
		{ID: "1003-120000-demo", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateQueued}},
	}}
	a.removeOldEntries(st)
	if _, err := os.Lstat(gone); err == nil {
		t.Error("过期的要清")
	}
	for _, p := range []string{stuck, queued, audit} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s 不该清：%v", p, err)
		}
	}
}

// 商量记录：了结的过期后清，还在商量的多旧都留着。
func TestRetentionDiscuss(t *testing.T) {
	a, _, now := retentionApp(t)
	old := now.Add(-100 * 24 * time.Hour)
	dir := filepath.Join(a.DataDir(), "discuss")
	write := func(name, st string) string {
		b, _ := json.Marshal(discuss.Record{Key: name, State: st})
		p := filepath.Join(dir, name+".json")
		writeAged(t, p, string(b), old)
		return p
	}
	done, dropped := write("aaa", discuss.StateDone), write("bbb", discuss.StateDropped)
	waiting, decided := write("ccc", discuss.StateWaiting), write("ddd", discuss.StateDecided)
	writeAged(t, filepath.Join(a.DataDir(), "jobs", "1001-120000-demo", "meta.json"), "{}", now)
	a.removeOldEntries(&state.State{})
	for _, p := range []string{done, dropped} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s 要清", p)
		}
	}
	for _, p := range []string{waiting, decided} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s 不该清：%v", p, err)
		}
	}
}

// 门上卡住的记录过了 30 天也留着，不然 gate pass、gate abandon 找不到它；定了的照常过期。
func TestPruneKeepsStuck(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	ended := now.Add(-40 * 24 * time.Hour)
	st := &state.State{Jobs: []*state.Job{
		{ID: "stuck", Status: state.StatusDone, EndedAt: &ended, Gate: &state.GateRecord{State: state.GateStuck}},
		{ID: "abandoned", Status: state.StatusDone, EndedAt: &ended, Gate: &state.GateRecord{State: state.GateAbandoned}},
		{ID: "merged", Status: state.StatusDone, EndedAt: &ended, Gate: &state.GateRecord{State: state.GateMerged}},
		{ID: "plain", Status: state.StatusDone, EndedAt: &ended},
	}}
	st.Prune(now)
	if len(st.Jobs) != 1 || st.Jobs[0].ID != "stuck" {
		t.Fatalf("%+v", st.Jobs)
	}
}
