package evolve

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

func gradeCase(e Expect) Case {
	return Case{ID: "001-x", Domain: "backend", Kind: KindTask, Expect: e, fsys: fstest.MapFS{}}
}

func checkNames(s Score, pass bool) []string {
	var out []string
	for _, c := range s.Checks {
		if c.Pass == pass {
			out = append(out, c.Name)
		}
	}
	return out
}

// 正常：全部对得上就是 100 分。
func TestGradeAllPass(t *testing.T) {
	c := gradeCase(Expect{Merged: boolp(true), Item: []string{"done"}, MainHas: []string{"a.go"}, MainLacks: []string{"b.go"}, Unchanged: []string{"a_test.go"},
		VerifyExit: intp(0), GatesRan: []string{"test"}, GatesBlocked: []string{"review"}, Evidence: []string{"撤掉修复"}, Shots: boolp(true), Delivery: boolp(true), MaxRounds: intp(1), Human: boolp(false)})
	s := Grade(c, Facts{Merged: true, Item: "done", Files: map[string]bool{"a.go": true}, VerifyExit: intp(0), Ran: map[string]bool{"test": true}, Blocked: map[string]bool{"review": true},
		Evidence: "撤掉修复后测试失败", Shots: 2, Delivery: true, Rounds: 1})
	if !s.Correct || s.Points != 100 || len(checkNames(s, false)) != 0 {
		t.Fatalf("%+v", s)
	}
}

// 边界：期望不合并就要真的没合并；转交用户的题 Human 要大于 0；退回轮数正好到上限算过。
func TestGradeBoundaries(t *testing.T) {
	s := Grade(gradeCase(Expect{Merged: boolp(false), Human: boolp(true), MaxRounds: intp(2)}), Facts{Merged: true, Human: 0, Rounds: 2})
	if s.Correct || s.Points != 33 {
		t.Fatalf("%+v", s)
	}
	if got := strings.Join(checkNames(s, false), ","); got != "合并,要人定" {
		t.Fatalf("没过的：%s", got)
	}
	over := Grade(gradeCase(Expect{MaxRounds: intp(2)}), Facts{Rounds: 3})
	if over.Correct || !strings.Contains(over.Checks[0].Detail, "最多 2，实际 3") {
		t.Fatalf("%+v", over)
	}
}

// 出错：main 读不出来时，main 上的判断一律不过，尤其是「没有某文件」和「没改某文件」不能白送。
func TestGradeNoMainGivesNothing(t *testing.T) {
	s := Grade(gradeCase(Expect{MainHas: []string{"a"}, MainLacks: []string{"b"}, Unchanged: []string{"c"}, VerifyExit: intp(0)}), Facts{NoMain: true})
	if s.Correct || s.Points != 0 {
		t.Fatalf("%+v", s)
	}
}

// 题目带判分脚本：退出码不是 0 就不对；没跑不能算过。
func TestGradeCheckScript(t *testing.T) {
	prog := Case{ID: "007-x", Domain: "install", Kind: KindProgram, fsys: fstest.MapFS{"check.sh": {}}}
	if s := Grade(prog, Facts{CheckExit: intp(0)}); !s.Correct || s.Points != 100 {
		t.Fatalf("%+v", s)
	}
	bad := Grade(prog, Facts{CheckExit: intp(1), CheckNote: "conflict: 技能不是我们的"})
	if bad.Correct || !strings.Contains(bad.Checks[0].Detail, "conflict: 技能不是我们的") {
		t.Fatalf("%+v", bad)
	}
	if s := Grade(prog, Facts{}); s.Correct {
		t.Fatalf("没跑不能算过：%+v", s)
	}
}

func sc(id, domain string, points, rounds int) Score {
	return Score{Case: id, Domain: domain, Mode: ModeReal, Points: points, Correct: points == 100, Rounds: rounds}
}

// 平均涨了也要报：一题掉了，别的题涨得再多也算变差。这条是 compare 的意义。
func TestCompareOneCaseWorseEvenIfAverageUp(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0), sc("b", "frontend", 0, 0), sc("c", "frontend", 0, 0)}
	cur := []Score{sc("a", "backend", 50, 0), sc("b", "frontend", 100, 0), sc("c", "frontend", 100, 0)}
	cmp := Compare(base, cur)
	joined := strings.Join(cmp.Worse, "\n")
	if !strings.Contains(joined, "a：分数 100 → 50") || !strings.Contains(joined, "领域 backend：平均分 100.0 → 50.0") {
		t.Fatalf("%+v", cmp)
	}
	if len(cmp.Better) != 2 {
		t.Fatalf("%+v", cmp)
	}
}

// 基线 A 过 B 没过，这次 A 没过 B 过：分数一样，能力变了，要报。
func TestCompareSwappedChecksAreWorse(t *testing.T) {
	b := sc("a", "backend", 50, 0)
	b.Checks = []Check{{Name: "甲", Pass: true}, {Name: "乙", Pass: false}}
	c := b
	c.Checks = []Check{{Name: "甲", Pass: false}, {Name: "乙", Pass: true}}
	cmp := Compare([]Score{b}, []Score{c})
	if len(cmp.Worse) != 1 || !strings.Contains(cmp.Worse[0], "从过变没过：甲") {
		t.Fatalf("%+v", cmp)
	}
	// 一直没过的不算新变差；这次才冒出来又没过的算。
	c.Checks = []Check{{Name: "甲", Pass: true}, {Name: "乙", Pass: false}}
	if cmp = Compare([]Score{b}, []Score{c}); len(cmp.Worse) != 0 {
		t.Fatalf("%+v", cmp)
	}
	c.Checks = append(c.Checks, Check{Name: "改动都过了门", Pass: false})
	if cmp = Compare([]Score{b}, []Score{c}); len(cmp.Worse) != 1 || !strings.Contains(cmp.Worse[0], "改动都过了门") {
		t.Fatalf("%+v", cmp)
	}
}

// 领域掉了但每题都没有更差不会发生；反过来同一领域一题涨一题掉，题要报，领域持平不报。
func TestCompareDomainFlat(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0), sc("b", "backend", 50, 0)}
	cur := []Score{sc("a", "backend", 50, 0), sc("b", "backend", 100, 0)}
	cmp := Compare(base, cur)
	if len(cmp.Worse) != 1 || !strings.HasPrefix(cmp.Worse[0], "a：") {
		t.Fatalf("%+v", cmp)
	}
}

func TestCompareRoundsAndHumanAreWorse(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0)}
	cur := []Score{sc("a", "backend", 100, 2)}
	cur[0].Human = 1
	cmp := Compare(base, cur)
	joined := strings.Join(cmp.Worse, "\n")
	if !strings.Contains(joined, "退回 0 → 2 轮") || !strings.Contains(joined, "要人插手 0 → 1 次") || !strings.Contains(joined, "领域 backend：平均退回") {
		t.Fatalf("%+v", cmp)
	}
}

// 边界：一样的结果没有变差；新题、少跑的题、判不了的基线都只提醒，不算比过。
func TestCompareNotes(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0), sc("gone", "backend", 100, 0), {Case: "err", Domain: "backend", Mode: ModeReal, Error: "撞限"}}
	cur := []Score{sc("a", "backend", 100, 0), sc("new", "backend", 0, 0), sc("err", "backend", 100, 0), {Case: "gone2", Domain: "x", Mode: ModeReal, Skipped: "没 go"}}
	cmp := Compare(base, cur)
	if len(cmp.Worse) != 0 || cmp.Compared != 1 {
		t.Fatalf("%+v", cmp)
	}
	joined := strings.Join(cmp.Notes, "\n")
	for _, want := range []string{"new：基线里没有这题", "gone：这次没跑", "err：基线那次判不了"} {
		if !strings.Contains(joined, want) {
			t.Errorf("少了 %s：%s", want, joined)
		}
	}
}

// 不同跑法不比：假 CLI 的分和真实模型的分没有可比性。
func TestCompareOnlySameMode(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0)}
	cur := []Score{{Case: "a", Domain: "backend", Mode: ModeFake, Points: 0}}
	cmp := Compare(base, cur)
	if len(cmp.Worse) != 0 || cmp.Compared != 0 {
		t.Fatalf("%+v", cmp)
	}
}

// 出错：这次判不了算变差，没跑成不能蒙混过去。
func TestCompareErrorIsWorse(t *testing.T) {
	cmp := Compare([]Score{sc("a", "backend", 100, 0)}, []Score{{Case: "a", Domain: "backend", Mode: ModeReal, Error: "没起任务"}})
	if len(cmp.Worse) != 1 || !strings.Contains(cmp.Worse[0], "没起任务") {
		t.Fatalf("%+v", cmp)
	}
}

// token 和耗时波动大：超过阈值也只提醒，不算变差；阈值内不吵。
func TestCompareTokensOnlyNote(t *testing.T) {
	b := sc("a", "backend", 100, 0)
	b.Tokens, b.Duration = 100_000, 2*time.Minute
	c := b
	c.Tokens, c.Duration = 200_000, 10*time.Minute
	cmp := Compare([]Score{b}, []Score{c})
	if len(cmp.Worse) != 0 || len(cmp.Notes) != 2 {
		t.Fatalf("%+v", cmp)
	}
	c.Tokens, c.Duration = 130_000, 150*time.Second
	if cmp = Compare([]Score{b}, []Score{c}); len(cmp.Notes) != 0 {
		t.Fatalf("%+v", cmp)
	}
}

// 留出题不写名字，也不混进各领域。
func TestHeldHiddenInCompareAndSummary(t *testing.T) {
	b := sc("900-secret", "backend", 100, 0)
	b.Held = true
	c := b
	c.Points, c.Correct = 0, false
	c.Checks = []Check{{Name: "合并", Pass: false}}
	cmp := Compare([]Score{b}, []Score{c})
	joined := strings.Join(cmp.Worse, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "backend") || strings.Contains(joined, "合并") || !strings.Contains(joined, "留出题 1：分数 100 → 0") || !strings.Contains(joined, "领域 留出") {
		t.Fatalf("%+v", cmp)
	}
	// 判不了的原因、分数不变时的检查项也不说。
	e := b
	e.Points, e.Error = 0, "没跑成：a.txt 的判分脚本"
	if cmp = Compare([]Score{b}, []Score{e}); strings.Contains(strings.Join(cmp.Worse, "\n"), "a.txt") {
		t.Fatalf("%+v", cmp)
	}
	swapB, swapC := b, b
	swapB.Points, swapB.Checks = 50, []Check{{Name: "没改 a.txt", Pass: true}, {Name: "合并", Pass: false}}
	swapC.Points, swapC.Checks = 50, []Check{{Name: "没改 a.txt", Pass: false}, {Name: "合并", Pass: true}}
	cmp = Compare([]Score{swapB}, []Score{swapC})
	joined = strings.Join(cmp.Worse, "\n")
	if !strings.Contains(joined, "留出题 1：有 1 项检查从过变没过") || strings.Contains(joined, "a.txt") {
		t.Fatalf("%+v", cmp)
	}
	ds := Summarize([]Score{sc("a", "backend", 100, 0), c})
	if len(ds) != 2 || ds[0].Domain != "backend" || ds[1].Domain != HeldDomain || ds[1].Correct != 0 {
		t.Fatalf("%+v", ds)
	}
}

func TestSummarizeSkipsUnjudgedInAverage(t *testing.T) {
	ds := Summarize([]Score{sc("a", "backend", 100, 1), {Case: "b", Domain: "backend", Error: "撞限"}, {Case: "c", Domain: "backend", Skipped: "没 go"}})
	if len(ds) != 1 || ds[0].Cases != 3 || ds[0].Unjudged != 2 || ds[0].Points != 100 || ds[0].Rounds != 1 {
		t.Fatalf("%+v", ds)
	}
}

// 基线只换同一题同一种跑法，判不了的不进基线，只跑几题不冲掉其余的。
func TestMergeBaseline(t *testing.T) {
	base := []Score{sc("a", "backend", 100, 0), sc("b", "backend", 50, 0)}
	cur := []Score{sc("b", "backend", 100, 0), {Case: "a", Domain: "backend", Mode: ModeReal, Error: "撞限"}, {Case: "a", Domain: "backend", Mode: ModeFake, Points: 100, Kept: "/tmp/现场"}}
	got := MergeBaseline(base, cur)
	if got[0].Kept != "" {
		t.Errorf("基线不留临时路径：%+v", got[0])
	}
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[0].Case != "a" || got[0].Mode != ModeFake || got[1].Mode != ModeReal || got[1].Points != 100 || got[2].Points != 100 {
		t.Fatalf("%+v", got)
	}
}
