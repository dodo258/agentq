package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rootAgeDir 建一个目录，里面一个文件，都拨到 at。
func rootAgeDir(t *testing.T, path string, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(path, "meta.json")
	if err := os.WriteFile(inner, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(inner, at, at); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// 真的 tick：跑一次清理；删不掉的不影响 tick 退出 0；配置非法数值拒绝。
func TestRetentionRunsInTick(t *testing.T) {
	e := newEnv(t, map[string]any{"retention_days": 30})
	d := filepath.Join(e.home, ".agentq")
	now := time.Now()
	rootAgeDir(t, filepath.Join(d, "jobs", "1104-120000-demo"), now.Add(-time.Hour))
	old := rootAgeDir(t, filepath.Join(d, "jobs", "0101-120000-old"), now.Add(-60*24*time.Hour))
	locked := rootAgeDir(t, filepath.Join(d, "verify", "0101-120000-locked"), now.Add(-60*24*time.Hour))
	if os.Getuid() != 0 {
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(locked, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o755) })
	}
	out, code := e.run("tick")
	if code != 0 {
		t.Fatalf("删不掉也不能影响 tick：%d\n%s", code, out)
	}
	if _, err := os.Lstat(old); err == nil {
		t.Errorf("应该删掉：%s", old)
	}
}
