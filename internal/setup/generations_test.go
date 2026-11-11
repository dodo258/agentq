package setup

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// 装完只留现在、上一代和最近 3 代；别的目录（不是 gen- 开头的、文件）不碰。
func TestPruneGenerations(t *testing.T) {
	i := &Installer{Home: t.TempDir(), Out: io.Discard}
	dir := filepath.Join(i.Share(), "generations")
	now := time.Now()
	var gens []string
	for n := range 7 {
		p := filepath.Join(dir, "gen-"+string(rune('a'+n)))
		if err := os.MkdirAll(filepath.Join(p, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-time.Duration(n) * time.Hour) // gen-a 最新
		os.Chtimes(p, at, at)
		gens = append(gens, p)
	}
	os.MkdirAll(filepath.Join(dir, "fixture"), 0o700)
	os.WriteFile(filepath.Join(dir, "gen-file"), nil, 0o600)
	i.pruneGenerations(gens[6], gens[5]) // 现在和上一代故意是最旧的两个
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{"fixture", "gen-a", "gen-b", "gen-c", "gen-f", "gen-file", "gen-g"}
	if !slices.Equal(left, want) {
		t.Fatalf("留下的：%v，要 %v", left, want)
	}
}
