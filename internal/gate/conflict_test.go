package gate_test

import (
	"fmt"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// conflicted 认领一条改 a.txt 的任务，再让 main 上也改 a.txt，门真跑一次：只会撞上合并冲突。
func conflicted(t *testing.T) (*fix, *state.Job, gate.RunResult) {
	t.Helper()
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "任务的\n"}, ready("agent/x"))
	f.commitMain(map[string]string{"a.txt": "并行任务先合进来的\n"})
	res := f.run(j)
	if res.Outcome != "fail" || !res.Conflict || !strings.Contains(res.Reason, "有冲突") {
		t.Fatalf("要是合并冲突：%+v", res)
	}
	return f, j, res
}

// 照任务 1123-000756-huitai：并行任务先合了 main，交付的分支撞上冲突。这不是改动或拆法的问题：
// 退回接着合，不加轮数，到了最后一轮也不标卡住、不说「多半是计划的问题」。
func TestConflictReturnKeepsRound(t *testing.T) {
	for _, round := range []int{gate.MaxRounds - 1, gate.MaxRounds} {
		t.Run(fmt.Sprint("第", round, "轮"), func(t *testing.T) {
			f, j, res := conflicted(t)
			j.Gate.Round = round
			f.collect(j, res)
			g := j.Gate
			if g.State != state.GateReturned || g.Round != round || g.Conflicts != 1 || j.Status != state.StatusInterrupted || !j.ResumePending || j.StuckPending != "" {
				t.Fatalf("要退回续跑、轮数不变：%+v %+v", g, j)
			}
			if strings.Contains(j.Summary, "退回第") || !strings.Contains(j.Summary, "不占退回轮数") {
				t.Errorf("摘要：%s", j.Summary)
			}
			note := f.a.ReturnNote(j)
			if !strings.Contains(note, "不算退回轮数") || !strings.Contains(note, "git merge") || strings.Contains(note, "退回第") {
				t.Errorf("退回提示：%s", note)
			}
			if b := f.backlog(); strings.Contains(b, "（卡住") {
				t.Errorf("不标卡住：\n%s", b)
			}
		})
	}
}

// 连着冲突到上限才停：原因写 main 一直在动，不是计划的问题；卡片建议退回再合，不建议放弃重拆。
func TestConflictLimitSaysMainMoving(t *testing.T) {
	f, j, res := conflicted(t)
	f.a.Cfg.NotifyEnabled = true
	j.GateConflicts = gate.MaxConflicts - 1
	f.collect(j, res)
	g := j.Gate
	if g.State != state.GateStuck {
		t.Fatalf("到上限要停：%+v", g)
	}
	if !strings.Contains(g.Report, "main 一直在动") || !strings.Contains(g.Report, fmt.Sprintf("连续 %d 次", gate.MaxConflicts)) || gate.IsPlanReport(g.Report) {
		t.Errorf("报告：%s", g.Report)
	}
	if strings.Contains(j.StuckPending, "条目拆法") || !strings.Contains(j.StuckPending, "main 一直在动") {
		t.Errorf("卡住标记：%s", j.StuckPending)
	}
	if card := stuckCard(t, f); card.Recommend != "B" {
		t.Errorf("卡片建议：%+v", card)
	}
}

// 冲突之后被检查真退回：照旧占一轮，连续冲突的次数清零；轮数用完照旧是计划的问题。
func TestReviewReturnAfterConflictsCounts(t *testing.T) {
	review := gate.RunResult{Outcome: "fail", Reason: "review：审查发现 1 个问题", Results: []gate.Result{{Gate: "review", Outcome: gate.Fail, Level: gate.LevelError, Summary: "审查发现 1 个问题"}}}
	f := newFix(t, fixBacklog)
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j.GateConflicts = gate.MaxConflicts - 1
	j.Gate.Round = gate.MaxRounds - 1
	f.collect(j, review)
	if g := j.Gate; g.State != state.GateReturned || g.Conflicts != 0 || !strings.Contains(j.Summary, fmt.Sprintf("退回第 %d 轮", gate.MaxRounds)) {
		t.Fatalf("真退回占一轮：%+v %s", g, j.Summary)
	}

	f2 := newFix(t, fixBacklog)
	j2 := f2.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, ready("agent/x"))
	j2.GateConflicts = 2
	j2.Gate.Round = gate.MaxRounds
	f2.collect(j2, review)
	if g := j2.Gate; g.State != state.GateStuck || !gate.IsPlanReport(g.Report) || !strings.Contains(j2.StuckPending, "条目拆法") {
		t.Fatalf("用完了照旧是计划的问题：%+v %s", g, j2.StuckPending)
	}
}
