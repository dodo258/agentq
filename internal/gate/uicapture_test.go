package gate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/testkit"
)

// captureRepo：main 上带一个 .agentq/ui（mode 是它的权限），返回项目目录和 home。
func captureRepo(t *testing.T, script string, mode os.FileMode) (string, string) {
	t.Helper()
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	dir := filepath.Join(t.TempDir(), "demo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644)
	if script != "" {
		os.MkdirAll(filepath.Join(dir, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(dir, gate.UIScript), []byte(script), mode)
	}
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, home, dir, "add", "-A")
	testkit.GitT(t, home, dir, "commit", "-qm", "init")
	return dir, home
}

func capture(t *testing.T, dir, home string, timeout time.Duration) (gate.UICapture, bool, string) {
	t.Helper()
	job := t.TempDir()
	c, ran := gate.CaptureUI(context.Background(), gate.CaptureOpts{Home: home, Project: dir, Branch: "main",
		WorkDir: filepath.Join(job, "ui"), Shots: filepath.Join(job, "screenshots"), Mark: "pm-ui-t", Timeout: timeout})
	return c, ran, job
}

// 产品经理调研前截图：跑默认分支上的 .agentq/ui，截图留在给的目录，一次性 worktree 跑完就删。
func TestCaptureUI(t *testing.T) {
	src := t.TempDir()
	writePNG(t, filepath.Join(src, "home.png"), 200, 100, 500)
	writePNG(t, filepath.Join(src, "blank.png"), 200, 100, 0)
	script := "#!/bin/sh\ncp " + filepath.Join(src, "home.png") + " \"$AGENTQ_ARTIFACTS/home.png\"\nmkdir -p \"$AGENTQ_ARTIFACTS/m\"\ncp " + filepath.Join(src, "blank.png") + " \"$AGENTQ_ARTIFACTS/m/blank.png\"\n"
	dir, home := captureRepo(t, script, 0o755)
	// 主目录上的改动不算：跑的是 main 上提交的那份
	os.WriteFile(filepath.Join(dir, gate.UIScript), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	c, ran, job := capture(t, dir, home, time.Minute)
	if !ran {
		t.Fatal("main 上有 .agentq/ui 要跑")
	}
	shots := filepath.Join(job, "screenshots")
	if strings.Join(c.Shots, "|") != filepath.Join(shots, "home.png")+"|"+filepath.Join(shots, "m", "blank.png") {
		t.Fatalf("截图：%q；%s", c.Shots, c.Problem)
	}
	if !strings.Contains(c.Problem, "1 张截图有问题") || !strings.Contains(c.Problem, "blank.png 是空白的") {
		t.Errorf("空白的写明：%q", c.Problem)
	}
	if got := gate.CapturedShots(shots); strings.Join(got, "|") != strings.Join(c.Shots, "|") {
		t.Errorf("CapturedShots 和截到的一致：%q", got)
	}
	if _, err := os.Stat(filepath.Join(job, "ui", "wt")); !os.IsNotExist(err) {
		t.Errorf("一次性 worktree 要删：%v", err)
	}
	if out := testkit.GitT(t, home, dir, "worktree", "list"); strings.Contains(out, "wt") {
		t.Errorf("git 里也不记着：%s", out)
	}
	if gate.CapturedShots(filepath.Join(job, "没有")) != nil {
		t.Errorf("目录不在是空的")
	}
}

func TestCaptureUIFailures(t *testing.T) {
	dir, home := captureRepo(t, "", 0)
	if _, ran, job := capture(t, dir, home, time.Minute); ran {
		t.Errorf("没有 .agentq/ui 不跑")
	} else if ents, _ := os.ReadDir(job); len(ents) != 0 {
		t.Errorf("不跑就什么都不建：%v", ents)
	}
	for _, c := range []struct {
		name, script string
		mode         os.FileMode
		timeout      time.Duration
		want         string
	}{
		{"退出码不是 0", "#!/bin/sh\necho 坏了\nexit 3\n", 0o755, time.Minute, ".agentq/ui 没过：退出码 3"},
		{"没截图", "#!/bin/sh\nexit 0\n", 0o755, time.Minute, "里没有 PNG 截图"},
		{"超时", "#!/bin/sh\nsleep 30\n", 0o755, time.Second, ".agentq/ui 判不了：超过"},
		{"跑不了", "#!/bin/sh\nexit 125\n", 0o755, time.Minute, "判不了"},
		{"没有可执行权限", "#!/bin/sh\nexit 0\n", 0o644, time.Minute, "没有可执行权限，没跑"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, home := captureRepo(t, c.script, c.mode)
			got, ran, _ := capture(t, dir, home, c.timeout)
			if !ran || len(got.Shots) != 0 || !strings.Contains(got.Problem, c.want) {
				t.Fatalf("%v %+v", ran, got)
			}
		})
	}
}
