package runner

import (
	"slices"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// 退回的任务接着做：轮数加一，带上退回时定的范围和上一轮的分支尖；同一轮中断的续跑范围照旧；卡在门上的不再限定。
func TestCarryGate(t *testing.T) {
	returned := &state.Job{Gate: &state.GateRecord{State: state.GateReturned, Round: 0, Head: "h1", Scope: []string{"a.go"}}}
	j := &state.Job{}
	carryGate(j, returned)
	if j.GateRound != 1 || !slices.Equal(j.GateScope, []string{"a.go"}) || j.GateScopeFrom != "h1" {
		t.Fatalf("退回：%+v", j)
	}
	again := &state.Job{}
	carryGate(again, j) // 第 2 轮做到一半中断，续跑
	if again.GateRound != 1 || !slices.Equal(again.GateScope, []string{"a.go"}) || again.GateScopeFrom != "h1" {
		t.Fatalf("续跑：%+v", again)
	}
	noScope := &state.Job{Gate: &state.GateRecord{State: state.GateReturned, Round: 1, Head: "h2"}}
	k := &state.Job{GateScope: []string{"旧"}, GateScopeFrom: "旧"}
	carryGate(k, noScope)
	if k.GateRound != 2 || k.GateScope != nil || k.GateScopeFrom != "" {
		t.Fatalf("没定范围的退回不限定：%+v", k)
	}
	stuck := &state.Job{GateRound: 2, GateScope: []string{"a.go"}, GateScopeFrom: "h1", Gate: &state.GateRecord{State: state.GateStuck, Round: 2}}
	s := &state.Job{}
	carryGate(s, stuck)
	if s.GateRound != 2 || s.GateScope != nil {
		t.Fatalf("卡住后重做：%+v", s)
	}
}

// 只因和最新默认分支冲突退回：轮数不变（到了最后一轮也一样），带上连续冲突的次数；中途中断的续跑照带；被检查退回、卡住后重做的清零。
func TestCarryGateConflict(t *testing.T) {
	last := gate.MaxRounds
	conflict := &state.Job{GateConflicts: 1, Gate: &state.GateRecord{State: state.GateReturned, Round: last, Conflicts: 2, Head: "h3", Scope: []string{"a.go"}}}
	j := &state.Job{}
	carryGate(j, conflict)
	if j.GateRound != last || j.GateConflicts != 2 || !slices.Equal(j.GateScope, []string{"a.go"}) || j.GateScopeFrom != "h3" {
		t.Fatalf("冲突退回：%+v", j)
	}
	again := &state.Job{}
	carryGate(again, j) // 合冲突做到一半中断，续跑
	if again.GateRound != last || again.GateConflicts != 2 {
		t.Fatalf("续跑：%+v", again)
	}
	reviewed := &state.Job{GateConflicts: 2, Gate: &state.GateRecord{State: state.GateReturned, Round: 1}}
	r := &state.Job{}
	carryGate(r, reviewed)
	if r.GateRound != 2 || r.GateConflicts != 0 {
		t.Fatalf("被检查退回：%+v", r)
	}
	stuck := &state.Job{GateRound: 1, GateConflicts: 4, Gate: &state.GateRecord{State: state.GateStuck, Round: 1}}
	s := &state.Job{}
	carryGate(s, stuck)
	if s.GateRound != 1 || s.GateConflicts != 0 {
		t.Fatalf("卡住后重做：%+v", s)
	}
}
