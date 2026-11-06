package main

import (
	"bytes"
	"io/fs"
	"os"
	"testing"

	"agentq/internal/config"
	"agentq/internal/testkit"
)

// tests/ 不能 import main，用例照 testkit 的两张表从磁盘读资产。这里查编译进二进制的和那份一个字节都不差，否则用例验的就不是产品。
func TestEmbeddedAssetsMatchRepo(t *testing.T) {
	compare := func(name string, embedded fs.FS, patterns []string) {
		t.Helper()
		disk, err := testkit.EmbedView(".", patterns...)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		err = fs.WalkDir(embedded, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			seen[p] = true
			want, err := fs.ReadFile(embedded, p)
			if err != nil {
				return err
			}
			file, ok := disk[p]
			if !ok {
				t.Errorf("%s：%s 编译进了二进制，磁盘那份没有", name, p)
			} else if !bytes.Equal(file.Data, want) {
				t.Errorf("%s：%s 内容和磁盘不一样", name, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) == 0 {
			t.Errorf("%s 一个文件都没有", name)
		}
		for p := range disk {
			if !seen[p] {
				t.Errorf("%s：磁盘那份多了 %s，没编译进二进制", name, p)
			}
		}
	}
	compare("installAssets", installAssets, testkit.InstallAssetPatterns)
	compare("evalAssets", evalAssets, testkit.EvalAssetPatterns)

	for path, got := range map[string]string{
		"skills/feedback-triage/SKILL.md": feedbackTriageSkill,
		"prompts/night.md":                routineText[config.KindNight],
		"prompts/review.md":               routineText[config.KindReview],
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s 和编译进二进制的不一样", path)
		}
	}
}
