package tests

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 派活前：条目提到反复修的模块，合同里加「先按重写流程梳理」；agentq health 能报出来。
func TestIntegrationPatchNoteAndHealth(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true})
	dir := gateRepo(t, e, "demo", "- [ ] 修 core 又算错 READYMERGE —— BRANCH=agent/t-a FILE=a.txt", "- [ ] 不相关的活 NOMERGE")
	for i, msg := range []string{"加 core", "fix: A 算错", "修复：A 还是不对"} {
		os.MkdirAll(filepath.Join(dir, "core"), 0o755)
		os.WriteFile(filepath.Join(dir, "core", "a.go"), []byte(fmt.Sprintf("package core\n\nvar A = %d\n", i)), 0o644)
		testkit.GitT(t, e.home, dir, "add", "-A")
		testkit.GitT(t, e.home, dir, "commit", "-q", "-m", msg)
	}
	if out := e.mustRun("health", "demo"); !strings.Contains(out, "core\t3\t2\t") || !strings.Contains(out, "已修过 2 次，再修要重写") || !strings.Contains(out, "两周返工率") {
		t.Errorf("health 要报出反复修的模块：\n%s", out)
	}
	e.mustRun("run", "demo", "修 core")
	j := e.jobByTitle("修 core 又算错 READYMERGE")
	prompt, err := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), "core（2 次）") || !strings.Contains(string(prompt), "重写流程") {
		t.Errorf("合同要加重写：%v\n%s", err, prompt)
	}
	e.tickUntilGate("修 core 又算错 READYMERGE")
	e.mustRun("run", "demo", "不相关")
	k := e.jobByTitle("不相关的活 NOMERGE")
	prompt, err = os.ReadFile(filepath.Join(jobHome(k, e), "prompt.txt"))
	if err != nil || strings.Contains(string(prompt), "引擎的统计") {
		t.Errorf("条目没提就不加：%v\n%s", err, prompt)
	}
	waitExit(t, k.PID, 20*time.Second)
}

// 界面门走真二进制：_gate 进程把截图目录告诉脚本，空白截图被拦（error 档）；有内容的交给审查进程，默认 warn 不挡。
func TestIntegrationUIGate(t *testing.T) {
	e := newEnv(t, map[string]any{"paused": true})
	dir := gateRepo(t, e, "demo", "- [ ] 改页面 READYMERGE —— BRANCH=agent/t-a FILE=a.css")
	shot := filepath.Join(e.home, "shot.png")
	writeTestPNG(t, shot, false)
	os.WriteFile(filepath.Join(dir, gate.UIScript), []byte("#!/bin/sh\ncp "+shot+" \"$AGENTQ_ARTIFACTS/home.png\"\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "界面脚本")
	e.mustRun("run", "demo")
	title := "改页面 READYMERGE"
	j := e.tickUntilGate(title)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	report := e.mustRun("gate", "report", j.ID)
	if !strings.Contains(report, "ui（warn 档）：pass") {
		t.Errorf("要跑界面门：\n%s", report)
	}

	e2 := newEnv(t, map[string]any{"paused": true})
	dir2 := gateRepo(t, e2, "demo", "- [ ] 改页面 READYMERGE —— BRANCH=agent/t-a FILE=a.css")
	blank := filepath.Join(e2.home, "blank.png")
	writeTestPNG(t, blank, true)
	os.WriteFile(filepath.Join(dir2, gate.UIScript), []byte("#!/bin/sh\ncp "+blank+" \"$AGENTQ_ARTIFACTS/home.png\"\n"), 0o755)
	os.WriteFile(filepath.Join(dir2, gate.GatesFile), []byte(`{"levels":{"ui":"error"}}`), 0o644)
	testkit.GitT(t, e2.home, dir2, "add", "-A")
	testkit.GitT(t, e2.home, dir2, "commit", "-q", "-m", "界面脚本")
	e2.mustRun("run", "demo")
	j = e2.tickUntilGate(title)
	if j.Gate.State != state.GateReturned || !strings.Contains(j.Gate.Report, "home.png 是空白的") {
		t.Fatalf("%+v", j.Gate)
	}
}

func writeTestPNG(t *testing.T, path string, blank bool) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if !blank && y < 20 {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
