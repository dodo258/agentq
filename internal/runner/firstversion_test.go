package runner

import (
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/state"
)

// 第一版挡不挡后面的条目：看条目现状。第一条在「待做」且没合并、没放弃才挡。
func TestFirstVersionHold(t *testing.T) {
	report := func() *state.NewReport { return &state.NewReport{Path: "/p/clip"} }
	first := backlog.Item{Title: "第一条", Fingerprint: "fp1"}
	second := backlog.Item{Title: "第二条", Fingerprint: "fp2"}
	now := time.Now()
	job := func(gate string) *state.Job {
		j := &state.Job{Project: "clip", Fingerprint: "fp1", Title: "第一条", NewReport: report()}
		if gate != "" {
			j.Gate = &state.GateRecord{State: gate}
		}
		return j
	}
	takeover := func(gate string) *state.Job {
		j := job(gate)
		j.StartedAt = now
		return j
	}
	old := func(gate string) *state.Job {
		j := job(gate)
		j.StartedAt = now.Add(-time.Hour)
		return j
	}
	both := []backlog.Item{first, second}
	cases := []struct {
		name  string
		jobs  []*state.Job
		items []backlog.Item
		held  bool
	}{
		{"排队", []*state.Job{job(state.GateQueued)}, both, true},
		{"过门中", []*state.Job{job(state.GateRunning)}, both, true},
		{"退回", []*state.Job{job(state.GateReturned)}, both, true},
		{"卡住", []*state.Job{job(state.GateStuck)}, both, true},
		{"合并了", []*state.Job{job(state.GateMerged)}, both, false},
		{"放弃了", []*state.Job{job(state.GateAbandoned)}, both, false},
		{"接手的还在做", []*state.Job{old(state.GateReturned), takeover("")}, both, true},
		{"接手的合并了", []*state.Job{old(state.GateReturned), takeover(state.GateMerged)}, both, false},
		{"接手的放弃了", []*state.Job{old(state.GateReturned), takeover(state.GateAbandoned)}, both, false},
		{"接手的结束了，条目还在", []*state.Job{{Project: "clip", Fingerprint: "fp1", Title: "第一条", NewReport: report(), Status: state.StatusInterrupted}}, both, true},
		{"标了卡住，指纹和标题都变了", []*state.Job{job(state.GateStuck)}, []backlog.Item{{Title: "第一条（卡住：门没过，任务 x）", Fingerprint: "变了"}, second}, true},
		{"第一条删了", []*state.Job{job(state.GateStuck)}, []backlog.Item{second}, false},
		{"没改认的 new", []*state.Job{{Kind: state.KindNew, FirstItem: &state.FirstItem{Project: "clip", Fingerprint: "fp1", Title: "第一条"}}}, both, true},
		{"没改认的 new，第一条删了", []*state.Job{{Kind: state.KindNew, FirstItem: &state.FirstItem{Project: "clip", Fingerprint: "fp1", Title: "第一条"}}}, []backlog.Item{second}, false},
		{"升级前自己合并的 new", []*state.Job{{Kind: state.KindNew, Project: "新项目", Status: state.StatusDone, NewReport: report()}}, both, false},
		{"别的项目", []*state.Job{{Project: "other", Fingerprint: "fp1", Title: "第一条", NewReport: &state.NewReport{Path: "/p/other"}, Gate: &state.GateRecord{State: state.GateQueued}}}, both, false},
	}
	for _, c := range cases {
		h, held := FirstVersionHold(&state.State{Jobs: c.jobs}, backlog.Project{Name: "clip", Dir: "/p/clip"}, c.items)
		if held != c.held {
			t.Errorf("%s：%v，要 %v", c.name, held, c.held)
		}
		if held && !strings.HasPrefix(h.Item.Title, "第一条") {
			t.Errorf("%s：%+v", c.name, h.Item)
		}
	}
}
