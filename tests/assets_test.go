package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing/fstest"

	"agentq/internal/app"
	"agentq/internal/config"
	"agentq/internal/testkit"
)

// 根目录 assets.go 的同名资产，从磁盘读（用例 import 不了 main）。根目录的 TestEmbeddedAssetsMatchRepo 查和编译进二进制的一样。
var (
	installAssets       fstest.MapFS
	evalAssets          fstest.MapFS
	feedbackTriageSkill string
	nightPrompt         string
	reviewPrompt        string
	routineText         map[string]string
)

// enterRepo 把工作目录切到仓库根，再读资产：用例里的 README.md、scripts/、go build . 都按仓库根写。
func enterRepo() error {
	root, err := testkit.RepoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("切到仓库根：%w", err)
	}
	if installAssets, err = testkit.EmbedView(root, testkit.InstallAssetPatterns...); err != nil {
		return err
	}
	if evalAssets, err = testkit.EmbedView(root, testkit.EvalAssetPatterns...); err != nil {
		return err
	}
	for path, dst := range map[string]*string{
		"skills/feedback-triage/SKILL.md": &feedbackTriageSkill,
		"prompts/night.md":                &nightPrompt,
		"prompts/review.md":               &reviewPrompt,
	} {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		*dst = string(b)
	}
	routineText = map[string]string{config.KindNight: nightPrompt, config.KindReview: reviewPrompt}
	return nil
}

// builtinAssets 和根目录的同名函数一样，把这些交给组装层。
func builtinAssets() app.Assets {
	return app.Assets{TriageSkill: feedbackTriageSkill, RoutineText: routineText, Install: installAssets, Evals: evalAssets}
}
