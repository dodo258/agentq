package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/testkit"
)

// 真二进制的 tick：建议写进去不满 suggest_auto_after 不动；满了在挑活之前移进「待做」，同一次 tick 就开工；暂停时不动。
func TestTickAutoDoSuggestion(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	dir := orphanRepo(t, e, "alpha", "")
	line := "- [ ] 建议甲 —— 要做的事；好处：好处；风险：风险（建议 1）"
	bpath := filepath.Join(dir, "docs", "BACKLOG.md")
	if err := os.WriteFile(bpath, []byte("# 待办\n\n## 待做\n\n## 建议\n\n"+line+"\n\n## 已完成\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, e.home, dir, "commit", "-qam", "建议")
	start := time.Now()
	idx := map[string]any{"next": 2, "items": []map[string]any{{"n": 1, "project": "alpha", "fingerprint": backlog.Fingerprint(line), "title": "建议甲", "created_at": start}}}
	raw, _ := json.Marshal(idx)
	os.MkdirAll(filepath.Join(e.home, ".agentq"), 0o700)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "suggestions.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	backlogText := func() string { b, _ := os.ReadFile(bpath); return string(b) }

	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(23*time.Hour).Format(time.RFC3339)}
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 0") || !strings.Contains(backlogText(), line) {
		t.Fatalf("不满 24 小时不动：%s\n%s", out, backlogText())
	}
	e.mustRun("pause")
	e.extra = []string{"AGENTQ_TEST_NOW=" + start.Add(25*time.Hour).Format(time.RFC3339)}
	if e.mustRun("tick"); !strings.Contains(backlogText(), line) {
		t.Fatalf("暂停时不动：\n%s", backlogText())
	}
	e.mustRun("resume")
	if out := e.mustRun("tick"); !strings.Contains(out, "启动 1") {
		t.Fatalf("满 24 小时移进待做，这次就开工：%s\n%s", out, backlogText())
	}
	if j := e.jobByTitle("建议甲"); j == nil {
		t.Fatal("应启动建议甲")
	}
	if b := backlogText(); strings.Contains(b, "（建议 1）") || len(backlog.ParseBacklog(b)) != 1 {
		t.Errorf("条目在待做里，去掉了编号：\n%s", b)
	}
}
