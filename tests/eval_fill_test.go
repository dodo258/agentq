package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/evolve"
	"agentq/internal/fsx"
)

// 验收：自己长大开着、已有基线、新一代带来了基线里没有的题，tick 空闲时自己起 agentq eval baseline --fill：
// 通过的新题补进基线，没过的（留出的一道故意判错的程序题）不进，已有题的分一分不动；补过之后同一代的 tick 不再起；
// 再拿这几题跑一次比，不再出现「基线里没有这题」。不调模型。
func TestIntegrationTickFillsBaseline(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"self_project": "agentq"})
	e.extra = append(e.extra, "TMPDIR="+t.TempDir())
	evalDir := filepath.Join(e.home, ".agentq", "evals")
	held := filepath.Join(evalDir, "heldout", "900-held-fail")
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(held, "case.json"), []byte(`{"domain":"engine","kind":"program","expect":{}}`), 0o600)
	os.WriteFile(filepath.Join(held, "check.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o600)

	// 基线：除了 024、038 和留出题，--fake 跑得了的题都有；001 的分故意写成 37，补完要原样留着。
	cases, err := evolve.LoadCases(os.DirFS("evals"), false)
	if err != nil {
		t.Fatal(err)
	}
	newIDs := []string{"024-cooldown-follows-backend", "038-patrol-backup-not-self"}
	var base evolve.Baseline
	for _, c := range cases {
		mode := evolve.ModeFake
		switch {
		case c.ID == newIDs[0] || c.ID == newIDs[1]:
			continue
		case c.Kind == evolve.KindProgram:
			mode = evolve.ModeProgram
		case !c.HasFake():
			continue
		}
		base.Scores = append(base.Scores, evolve.Score{Case: c.ID, Domain: c.Domain, Kind: c.Kind, Mode: mode, Points: 37})
	}
	before := len(base.Scores)
	raw, _ := json.Marshal(base)
	if err := os.WriteFile(filepath.Join(evalDir, "baseline.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	e.mustRun("tick")
	fillState := filepath.Join(evalDir, "fill.json")
	waitFor(t, 3*time.Minute, "补基线跑完", func() bool {
		_, err := os.Stat(fillState)
		_, _, busy := fsx.LockHolder(filepath.Join(evalDir, "fill.lock"))
		return err == nil && !busy
	})
	log := readFile(t, filepath.Join(evalDir, "fill.log"))
	if !strings.Contains(log, "补进 2 题") || !strings.Contains(log, "1 题没过或判不了") || strings.Contains(log, "900-held-fail") {
		t.Fatalf("fill.log：\n%s", log)
	}
	var got evolve.Baseline
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(evalDir, "baseline.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Scores) != before+2 {
		t.Fatalf("要补进 2 题：%d → %d", before, len(got.Scores))
	}
	for _, s := range got.Scores {
		switch {
		case s.Case == "900-held-fail":
			t.Errorf("没过的不能进基线：%+v", s)
		case s.Case == newIDs[0] || s.Case == newIDs[1]:
			if !s.Correct || s.Mode != evolve.ModeProgram || s.Points != 100 {
				t.Errorf("补进的分不对：%+v", s)
			}
		case s.Points != 37:
			t.Errorf("已有题的分被改了：%+v", s)
		}
	}

	// 同一代：没过的那题已经试过，tick 不再起
	e.mustRun("tick")
	time.Sleep(2 * time.Second)
	if n := strings.Count(readFile(t, filepath.Join(evalDir, "fill.log")), "基线里缺"); n != 1 {
		t.Fatalf("同一代只该补一次，起了 %d 次", n)
	}

	// 门报告的比法：这两题不再是「基线里没有这题」
	e.mustRun("eval", "run", "024", "038")
	cmp, _ := e.run("eval", "compare")
	if strings.Contains(cmp, "基线里没有这题") || !strings.Contains(cmp, "比了 2 题") {
		t.Fatalf("compare：\n%s", cmp)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
