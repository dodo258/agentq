package runner

import (
	"slices"
	"testing"

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
