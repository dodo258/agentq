package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func writeSessionLog(t *testing.T, a *App, session, content string) string {
	t.Helper()
	path := filepath.Join(a.Home, ".claude", "projects", "-p", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func toolUseLine(name string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": "2026-11-04T10:00:00Z",
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}}}}})
	return string(b) + "\n"
}

func worktreeCount(t *testing.T, a *App, dir string) int {
	t.Helper()
	return strings.Count(testkit.GitT(t, a.Home, dir, "worktree", "list", "--porcelain"), "worktree ")
}

func pendingAlerts(t *testing.T, a *App) []notify.Notice {
	t.Helper()
	s, err := a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	var out []notify.Notice
	for _, n := range s.Pending {
		if n.Cat == "alert" {
			out = append(out, n)
		}
	}
	return out
}

// 升级前的任务记录带着核验失败：早报不把它列进「昨晚完成」，单独写个数；其余逐字不变。
func TestMorningExcludesVerifyFailed(t *testing.T) {
	e := newNotifyEnv(t, true)
	e.now = time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local)
	report := filepath.Join(e.a.Home, ".claude", "nightly", e.now.Format("2006-01-02")+".md")
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("# 早报\n"), 0o644)
	old := e.now.Add(-time.Hour)
	os.Chtimes(report, old, old)
	ended := e.now.Add(-time.Hour)
	e.st.Jobs = []*Job{
		{ID: "a", Project: "demo", Title: "好的", Status: state.StatusDone, EndedAt: &ended, StartedAt: ended},
		{ID: "b", Project: "demo", Title: "坏的", Status: state.StatusDone, EndedAt: &ended, StartedAt: ended, Verify: &state.MergeVerify{Failed: true}},
	}
	e.flush()
	got := strings.Join(e.tg.texts(), "\n")
	if !strings.Contains(got, "昨晚完成　1 条") || strings.Contains(got, "坏的") || !strings.Contains(got, "合并后核验失败 1") {
		t.Errorf("%s", got)
	}
}
