package evolve

import (
	"os"
	"testing"
	"testing/fstest"

	"agentq/internal/config"
	"agentq/internal/state"
)

func fakeSc(id string, points int) Score {
	return Score{Case: id, Domain: "backend", Mode: ModeFake, Points: points, Correct: points == 100}
}

// 新题通过就补进：基线里没有、这次通过了的并进去，带上分，不留现场路径。
func TestFillBaselineAddsNewPassed(t *testing.T) {
	base := []Score{fakeSc("001-a", 100)}
	cur := []Score{fakeSc("001-a", 100), {Case: "022-b", Domain: "install", Kind: KindProgram, Mode: ModeProgram, Points: 100, Correct: true, Kept: "/tmp/x"}}
	got, added := FillBaseline(base, cur)
	if len(added) != 1 || added[0] != "022-b" || len(got) != 2 {
		t.Fatalf("%v %+v", added, got)
	}
	if got[1].Case != "022-b" || got[1].Mode != ModeProgram || got[1].Points != 100 || got[1].Kept != "" {
		t.Fatalf("%+v", got[1])
	}
}

// 新题没过、判不了、没跑的都不补。
func TestFillBaselineSkipsNewFailed(t *testing.T) {
	base := []Score{fakeSc("001-a", 100)}
	cur := []Score{fakeSc("022-b", 50), {Case: "023-c", Mode: ModeFake, Error: "agentq setup 退出码 1"}, {Case: "024-d", Mode: ModeFake, Skipped: "要 go"}}
	got, added := FillBaseline(base, cur)
	if len(added) != 0 || len(got) != 1 || got[0].Case != "001-a" {
		t.Fatalf("%v %+v", added, got)
	}
}

// 已有题的分不动：这次跑低了、跑高了都不改；同一题换一种跑法算新的。
func TestFillBaselineKeepsExisting(t *testing.T) {
	base := []Score{fakeSc("001-a", 100), fakeSc("002-b", 50), {Case: "003-c", Mode: ModeReal, Points: 100, Correct: true}}
	cur := []Score{fakeSc("001-a", 0), fakeSc("002-b", 100), fakeSc("003-c", 100)}
	got, added := FillBaseline(base, cur)
	if len(added) != 1 || added[0] != "003-c" || len(got) != 4 {
		t.Fatalf("%v %+v", added, got)
	}
	for _, s := range got {
		switch s.Case + "|" + s.Mode {
		case "001-a|fake":
			if s.Points != 100 {
				t.Errorf("已有题的分被改低：%+v", s)
			}
		case "002-b|fake":
			if s.Points != 50 {
				t.Errorf("已有题的分被改了：%+v", s)
			}
		}
	}
}

func fillEvals() fstest.MapFS {
	return fstest.MapFS{
		"evals/001-impl/case.json":               file(`{"domain":"backend","kind":"task","request":"做一件事","expect":{"merged":true}}`),
		"evals/001-impl/fake.sh":                 file("#!/bin/sh\n"),
		"evals/001-impl/_project/.agentq/verify": file("#!/bin/sh\n"),
		"evals/003-real/_project/.agentq/verify": file("#!/bin/sh\n"),
		"evals/002-skill/case.json":              file(`{"domain":"install","kind":"program"}`),
		"evals/002-skill/check.sh":               file("exit 0\n"),
		"evals/003-real/case.json":               file(`{"domain":"backend","kind":"task","request":"只能真跑","expect":{"merged":true}}`),
		"evals/044-newer/case.json":              file(`{"domain":"install","kind":"program"}`),
		"evals/044-newer/check.sh":               file("exit 0\n"),
		"evals/_fake/claude.sh":                  file("#!/bin/sh\n"),
		"evals/README.md":                        file("不是题"),
	}
}

// 缺哪些题：--fake 跑得了、基线里没有同一题同一种跑法的；程序题按 program 认，没有假做法的模型题不算缺。
func TestMissingCases(t *testing.T) {
	cases, err := LoadCases(mustSub(t, fillEvals()), false)
	if err != nil {
		t.Fatal(err)
	}
	base := []Score{{Case: "001-impl", Mode: ModeFake}, {Case: "002-skill", Mode: ModeProgram}, {Case: "044-newer", Mode: ModeReal}}
	got := missingCases(cases, base)
	if len(got) != 1 || got[0].ID != "044-newer" {
		t.Fatalf("%+v", got)
	}
}

func mustSub(t *testing.T, f fstest.MapFS) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	for k, v := range f {
		out[k[len("evals/"):]] = v
	}
	return out
}

// tick 什么时候起补基线：自动装新一代、有基线、有缺的题、这一代没试过、不在观察期、空闲、没在补。
func TestFillDue(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	r.s.Evals, r.s.Version = fillEvals(), "dev-aaaaaaa"
	st := &state.State{}
	want := func(reason string) {
		t.Helper()
		if got := r.s.fillDue(st); got != reason {
			t.Fatalf("要 %q，得到 %q", reason, got)
		}
	}
	want("还没有基线")
	r.write(r.s.baselinePath(), Baseline{Scores: []Score{{Case: "001-impl", Mode: ModeFake}, {Case: "002-skill", Mode: ModeProgram}}})
	want("")

	st.Jobs = []*state.Job{{ID: "j1", Project: "web", Status: state.StatusRunning, StartedAt: r.now}}
	want("有任务在跑或在过门")
	st.Jobs = []*state.Job{{ID: "j1", Project: "web", Status: state.StatusDone, StartedAt: r.now, Gate: &state.GateRecord{State: state.GateRunning}}}
	want("有任务在跑或在过门")
	st.Jobs = nil

	r.s.appendUpgrade(UpgradeEvent{At: r.now, Kind: EvUpgrade, To: r.s.Version})
	want("这一代还在观察期")
	r.s.appendUpgrade(UpgradeEvent{At: r.now, Kind: EvKept, To: r.s.Version})
	want("")

	// 这一代试过没进的不再跑；换了一代再试一次
	r.write(r.s.fillStatePath(), fillState{Version: r.s.Version, Tried: []string{"044-newer"}})
	want("基线里没有缺的题")
	r.s.Version = "dev-bbbbbbb"
	want("")

	r.s.Cfg.SelfProject = ""
	want("新一代不是自动装的")
}

// cmdFill 没有基线时不建：--fill 只往已有的基线里补。
func TestCmdFillNeedsBaseline(t *testing.T) {
	r := newSigRig(t, nil)
	r.s.Evals = fillEvals()
	err := r.s.CmdEval([]string{"baseline", "--fill"})
	if err == nil {
		t.Fatal("没有基线要报错")
	}
	if _, serr := os.Stat(r.s.baselinePath()); serr == nil {
		t.Fatal("没有基线时不该建")
	}
	if err := r.s.CmdEval([]string{"baseline", "--fill", "x"}); err == nil {
		t.Fatal("--fill 不带别的参数")
	}
	if _, err := os.Stat(r.s.fillStatePath()); err == nil {
		t.Fatal("没跑不该记试过")
	}
}
