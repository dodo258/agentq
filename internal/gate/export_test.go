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

// CapApplies 给测试：清单里的启用条件按项目判断要不要用。
var CapApplies = capApplies

// TestingTitle 给测试：条目标题是不是加测、压测。
var TestingTitle = testingTitle

// DropTmps 给测试：过门结束时删自己的临时目录。
var DropTmps = dropTmps

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

// FoldScope、InScope 给测试。
var (
	FoldScope = foldScope
	InScope   = inScope
)

// SpecRefs、UIPrompt、ReviewPrompt 给测试。
var (
	SpecRefs     = specRefs
	UIPrompt     = uiPrompt
	ReviewPrompt = reviewPrompt
)

// LightFlow、ChangedLines 给测试。
var (
	LightFlow    = lightFlow
	ChangedLines = changedLines
)

// SetCapabilities 给测试：换掉运行中这一代的能力清单和检查脚本。
func SetCapabilities(t *testing.T, caps []Capability, scripts map[string]string) {
	t.Helper()
	oldReg, oldScript := capRegistry, capScript
	capRegistry = func() ([]Capability, error) { return caps, nil }
	capScript = func(name string) ([]byte, error) {
		s, ok := scripts[name]
		if !ok {
			return nil, errNoRegistry
		}
		return []byte(s), nil
	}
	t.Cleanup(func() { capRegistry, capScript = oldReg, oldScript })
}

// HoldReviewable、HoldVerdict、HoldReviewRequest 给测试：独立审查代。
var (
	HoldReviewable    = holdReviewable
	HoldVerdict       = holdVerdict
	HoldReviewRequest = holdReviewRequest
)
