package gate

import (
	"context"
	"testing"
	"time"
)

// WriteResult 是门进程最后一步，测试在进程内走流程时用。
var WriteResult = writeResult

// NoteProgress 给测试。
var NoteProgress = noteProgress

// FindReview 给测试。
var FindReview = findReview

// FileChanges 给测试：解析 --numstat --summary 的输出。
var FileChanges = parseFileChanges

// PatchGate 给测试。
func PatchGate(d Deps) Gate { return patchGate{d} }

// UIGate 给测试。
func UIGate(d Deps) Gate { return uiGate{d} }

// JudgeUI 给测试。
var JudgeUI = judgeUI

// ShotProblem 给测试。
var ShotProblem = shotProblem

// GitForTest 给测试：和门用的同一个 git。
func (a *Service) GitForTest() func(ctx context.Context, dir string, args ...string) (string, error) {
	return a.git
}

// ModuleStats 给测试。
func ModuleStats(t *testing.T, a *Service, dir, ref string, since time.Time) []ModuleStat {
	t.Helper()
	stats, err := moduleStats(context.Background(), a.git, dir, ref, since)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// ReworkStats 给测试。
func ReworkStats(t *testing.T, a *Service, dir, ref string, since time.Time) Rework {
	t.Helper()
	r, err := a.reworkStats(context.Background(), dir, ref, since)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var IsInstructionFile = isInstructionFile
