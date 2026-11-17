package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/testkit"
)

// 自动发版走真二进制：tick 判条件、起脱离 tick 的 agentq _release，后台进程跑项目的 .agentq/release（检查、真发各一次），
// 记 CHANGELOG、记发成；下一次 tick 不再发。临时 HOME、临时 projects_root、假 gh、假发版脚本。
func TestReleaseTickRunsScriptOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"paused": false, "release_repos": map[string]string{"demo": "o/demo"}})
	dir := orphanRepo(t, e, "demo", "")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# demo\n\n- 自动发版：条件满足时发\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# 更新记录\n\n## 未发布\n\n- 新功能甲\n\n## v0.3.0\n\n- 旧\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agentq", "release"), []byte("#!/bin/sh\necho \"$* $AGENTQ_RELEASE_REPO\" >> \"$HOME/release-calls\"\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "发版脚本")
	bin := t.TempDir()
	gh := "#!/bin/sh\necho \"$*\" >> \"$HOME/gh-calls\"\ncase \"$1\" in\n" +
		"  release) echo '[{\"tagName\":\"v0.3.0\",\"publishedAt\":\"2026-01-01T00:00:00Z\"}]' ;;\n" +
		"  api) echo '{\"total_count\":1,\"check_runs\":[{\"name\":\"linux\",\"status\":\"completed\",\"conclusion\":\"success\"}]}' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	e.extra = append(e.extra, "PATH="+bin+":"+os.Getenv("PATH"))

	out := e.mustRun("tick")
	if !strings.Contains(out, "自动发版 demo：开始发 v0.4.0") {
		t.Fatalf("要开始发：\n%s", out)
	}
	// 后台进程最后写的是日志里「发了」那一行（记录文件在它之前写），等它出现再断言
	logPath := filepath.Join(e.home, ".agentq", "release", "demo.log")
	waitFor(t, 20*time.Second, "后台进程发完", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "自动发版 demo：发了 v0.4.0")
	})
	var rec struct {
		State, Version string
	}
	if b, err := os.ReadFile(filepath.Join(e.home, ".agentq", "release", "demo.json")); err != nil || json.Unmarshal(b, &rec) != nil || rec.State != "done" || rec.Version != "v0.4.0" {
		t.Fatalf("记成发成：%+v %v", rec, err)
	}
	calls, _ := os.ReadFile(filepath.Join(e.home, "release-calls"))
	if string(calls) != "--check v0.4.0 o/demo\nv0.4.0 o/demo\n" {
		t.Fatalf("脚本要检查、真发各一次：\n%s", calls)
	}
	if cl := testkit.ReadFile(t, filepath.Join(dir, "CHANGELOG.md")); !strings.Contains(cl, "## 未发布\n\n## v0.4.0\n\n- 新功能甲") {
		t.Errorf("CHANGELOG 要记成 v0.4.0：\n%s", cl)
	}
	testkit.AssertContains(t, testkit.ReadFile(t, logPath), "自动发版 demo：跑 .agentq/release --check v0.4.0", "自动发版 demo：跑 .agentq/release v0.4.0")

	out = e.mustRun("tick")
	if strings.Contains(out, "开始发") {
		t.Fatalf("发过了不再发：\n%s", out)
	}
	if calls2, _ := os.ReadFile(filepath.Join(e.home, "release-calls")); string(calls2) != string(calls) {
		t.Errorf("脚本只调这一次：\n%s", calls2)
	}
}
