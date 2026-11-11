package runner_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// 任务自己标了普通卡住：没暂停时去掉标记、记下要再接、不推；暂停时 tick 不会再接，标记留着、照常推「卡住了」。
func TestRetakeSkippedWhilePaused(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "没暂停", true: "暂停"}[paused], func(t *testing.T) {
			home := t.TempDir()
			os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
			os.MkdirAll(filepath.Join(home, ".agentq"), 0o700)
			os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600)
			e, err := env.New(home, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			e.Cfg.ProjectsRoot, e.Cfg.NotifyEnabled, e.Cfg.Paused = t.TempDir(), true, paused
			k := stack.New(e)
			dir := filepath.Join(e.Cfg.ProjectsRoot, "demo")
			os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
			item := "难的活 —— 要什么"
			mark := "（卡住：测试一直超时，分支 agent/x）"
			os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n- [ ] "+item+mark+"\n\n## 已完成\n"), 0o644)
			testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
			testkit.GitT(t, home, dir, "add", "-A")
			testkit.GitT(t, home, dir, "commit", "-q", "-m", "init")
			j := &state.Job{ID: "1111-100000-demo", Project: "demo", ProjectDir: dir, DefaultBranch: "main", Item: item, Title: "难的活",
				Fingerprint: backlog.Fingerprint("- [ ] " + item), Status: state.StatusRunning, StartedAt: e.Now()}
			os.MkdirAll(e.JobDir(j.ID), 0o755)
			os.WriteFile(filepath.Join(e.JobDir(j.ID), "result.json"), []byte(`{"type":"result","subtype":"success","is_error":false,"result":"试了两种做法。\n\n没合并，分支 agent/x；卡在测试超时。","session_id":"s"}`+"\n"), 0o644)
			st := &state.State{Jobs: []*state.Job{j}}
			if !paused { // 之前有一次报告做完、条目还在待做：去掉卡住标记要再接，不能被算成第二次「报告做完还在待做」
				ended := e.Now().Add(-time.Hour)
				st.Jobs = append([]*state.Job{{ID: "1111-090000-demo", Project: "demo", ProjectDir: dir, Item: item, Title: "难的活", Fingerprint: j.Fingerprint,
					Status: state.StatusDone, StartedAt: ended.Add(-time.Hour), EndedAt: &ended, StillListed: true}}, st.Jobs...)
			}
			k.Runner.FinishFromResult(st, j)

			b := testkit.ReadFile(t, filepath.Join(dir, "docs", "BACKLOG.md"))
			ns, err := k.Notify.LoadNotify()
			if err != nil {
				t.Fatal(err)
			}
			var stuckNotice, stillNotice bool
			for _, n := range ns.Pending {
				stuckNotice = stuckNotice || strings.Contains(n.Text, "「难的活」卡住了")
				stillNotice = stillNotice || strings.Contains(n.Text, "连续两次报告做完")
			}
			if paused {
				if j.Retake != "" || !strings.Contains(b, mark) || !stuckNotice {
					t.Fatalf("暂停时不再接，标记留着并推送：retake=%q 推了=%v\n%s", j.Retake, stuckNotice, b)
				}
				return
			}
			if j.Retake == "" || j.StuckBranch != "agent/x" || strings.Contains(b, "卡住") || stuckNotice {
				t.Fatalf("没暂停时去掉标记等再接、不推：retake=%q branch=%q 推了=%v\n%s", j.Retake, j.StuckBranch, stuckNotice, b)
			}
			if j.StillListed || j.StuckPending != "" || stillNotice {
				t.Fatalf("要再接的不算「报告做完还在待做」：still=%v pending=%q 推了=%v", j.StillListed, j.StuckPending, stillNotice)
			}
		})
	}
}
