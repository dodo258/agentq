package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// noteTask：第一次把测试函数 TestOld 改名成 TestNew（算删用例），提交说明不写门说明；
// 被要回补的那一轮追加一个只带门说明的空提交，提示词带 NONOTE 时什么都不补。
const noteTask = `case "$FAKE_PROMPT" in
  *引擎进门前先按测试门查了一遍*)
    case "$FAKE_PROMPT" in *NONOTE*) ;; *) git -c user.name=t -c user.email=t commit -q --allow-empty -m 补说明 -m "门说明：TestOld 改名成 TestNew，测的还是同一件事" ;; esac ;;
  *) sed -i.bak 's/TestOld/TestNew/' a_test.go && rm -f a_test.go.bak ;;
esac
`

// 验收：任务改名了测试函数、没写门说明。交了 ready.json 进门之前就被要回补：同一会话续一轮只补说明，
// 不进门、不算审查轮次；补上了门只跑一次、第 0 轮合并。补了还缺照常进门，由测试门退回。
func TestIntegrationNoteAskedBeforeGate(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		fixes       bool
	}{
		{"补上了", "改名测试 READYMERGE TASKSCRIPT", true},
		{"补了还缺", "改名测试 READYMERGE TASKSCRIPT NONOTE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, map[string]any{"notify_enabled": false})
			dir := gateRepo(t, e, "demo", "- [ ] "+tc.title+" —— BRANCH=agent/t-a FILE=a.txt")
			os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package a\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n"), 0o644)
			testkit.GitT(t, e.home, dir, "add", "-A")
			testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "加测试")
			os.WriteFile(filepath.Join(e.home, "fake-task.sh"), []byte(noteTask), 0o755)

			e.mustRun("tick")
			first := e.jobByTitle(tc.title)
			waitExit(t, first.PID, 20*time.Second)
			e.mustRun("tick")
			var resumed *Job
			for _, j := range e.state().Jobs {
				switch {
				case j.ID == first.ID:
					first = j
				case j.ResumeOf == first.ID:
					resumed = j
				}
			}
			if first.Gate != nil || first.Status != state.StatusInterrupted || !strings.Contains(strings.Join(first.NoteAsk, "；"), "删了测试用例 TestOld") {
				t.Fatalf("进门前就要回补，不进门：%s %+v %q；%s", first.Status, first.Gate, first.NoteAsk, first.Summary)
			}
			testkit.AssertContains(t, first.Summary, "进门前查出缺门说明")
			if _, err := os.Stat(filepath.Join(e.home, ".agentq", "gate", first.ID)); err == nil {
				t.Fatal("第一次交付不该起门")
			}
			if resumed == nil || resumed.GateRound != 0 || resumed.SessionID != first.SessionID {
				t.Fatalf("要用同一会话续一轮、轮次不加：%+v", resumed)
			}
			prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", resumed.ID, "prompt.txt"))
			testkit.AssertContains(t, prompt, "这一轮只补说明，不进门、不算审查轮次")
			testkit.AssertContains(t, prompt, "删了测试用例 TestOld")

			j := e.tickUntilGateJob(resumed.ID)
			tally := j.Gate.Tally["test"]
			if tally == nil || tally.Pass+tally.Fail+tally.Unknown != 1 {
				t.Fatalf("测试门只跑一次：%+v", j.Gate.Tally)
			}
			if tc.fixes {
				if j.Gate.State != state.GateMerged || j.Gate.Round != 0 || tally.Pass != 1 {
					t.Fatalf("补上了就第 0 轮合并：%+v", j.Gate)
				}
				return
			}
			if j.Gate.State != state.GateReturned || j.Gate.Round != 0 || tally.Fail != 1 || !strings.Contains(j.Gate.Report, "门说明") {
				t.Fatalf("补了还缺照常进门，由测试门退回：%+v", j.Gate)
			}
		})
	}
}
