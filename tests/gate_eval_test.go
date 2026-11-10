package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// selfRepoEnv 把这份源码复制成 projects_root 下的一个项目（agentq 自己的仓库），只开评测门。
// 任务（假 claude 的 READYMERGE TASKSCRIPT）提交前跑 task 这段脚本。go 构建缓存指向本机的，临时 HOME 里不用从头编译。
func selfRepoEnv(t *testing.T, task string) (*testEnv, string) {
	t.Helper()
	e := newEnv(t, nil)
	e.extra = append(e.extra, "TMPDIR="+t.TempDir()) // 评测现场放这里，没过时留着的现场随用例删
	out, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	cache := strings.TrimSpace(string(out))
	for _, rel := range []string{"Library/Caches/go-build", ".cache/go-build"} {
		p := filepath.Join(e.home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(cache, p); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(e.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	if err := os.WriteFile(filepath.Join(e.home, "fake-task.sh"), []byte(task), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.root, "selfrepo")
	files, err := exec.Command("git", "ls-files", "-z", "-c", "-o", "--exclude-standard").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Split(strings.TrimRight(string(files), "\x00"), "\x00") {
		src, err := os.Lstat(f)
		if err != nil || !src.Mode().IsRegular() { // 删掉还没提交的、子模块之类
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, f), b, src.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n- [ ] 改评测相关 READYMERGE TASKSCRIPT —— BRANCH=agent/t-eval\n\n## 等用户决定\n\n## 已完成\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROGRESS.md"), []byte("# 进度\n\n## 当前任务\n无\n"), 0o644)
	os.WriteFile(filepath.Join(dir, gate.GatesFile), []byte(`{"levels":{"patch":"off","scope":"off","test":"off","smoke":"off","review":"off","ui":"off"}}`), 0o644)
	testkit.GitT(t, e.home, dir, "init", "-q", "-b", "main")
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "init")
	return e, dir
}

// gateOnce 起任务、等它交付，再等过门结束收结果。评测门要构建候选、跑一遍评测，等得久一点。
func (e *testEnv) gateOnce(title string) *Job {
	e.t.Helper()
	e.mustRun("tick")
	j := e.jobByTitle(title)
	waitExit(e.t, j.PID, 30*time.Second)
	e.mustRun("pause") // 只看这一条
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Gate == nil || j.Gate.State != state.GateRunning {
		e.t.Fatalf("应该在过门：%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	waitFor(e.t, 5*time.Minute, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	return e.jobByTitle(title)
}

const evalGateTitle = "改评测相关 READYMERGE TASKSCRIPT"

// 验收：agentq 自己的候选改坏了一道程序题，同时把那道题的判分脚本改成直接过：评判用运行中那一代的题，照样判错，退回，理由写清哪题哪领域和耗时。
func TestIntegrationEvalGateRejectsBrokenSelf(t *testing.T) {
	t.Parallel()
	task := `mkdir -p internal/setup
cat > internal/setup/zz_eval_break.go <<'EOF'
package setup

import (
	"fmt"
	"os"
	"slices"
)

func init() {
	if len(os.Args) > 2 && os.Args[1] == "setup" && os.Args[2] == "--plan" && !slices.Contains(os.Args, "--services") {
		fmt.Fprintln(os.Stderr, "故意改坏")
		os.Exit(3)
	}
}
EOF
printf '#!/bin/sh\nexit 0\n' > evals/007-install-existing-skill/check.sh
`
	e, _ := selfRepoEnv(t, task)
	// 基线：运行中这一代（测试编出来的 agentq）跑两道程序题。
	e.mustRun("eval", "run", "007", "008")
	e.mustRun("eval", "baseline")
	j := e.gateOnce(evalGateTitle)
	if j.Gate.State == state.GateMerged {
		t.Fatalf("改坏的候选不能合并：%+v", j.Gate)
	}
	report := e.mustRun("gate", "report", j.ID)
	for _, want := range []string{"eval（error 档）：fail 评测比基线变差", "007-install-existing-skill：分数 100 → 0", "领域 install：平均分 100.0 → 50.0", "耗时 ", "构建 "} {
		if !strings.Contains(report, want) {
			t.Errorf("报告缺 %s：\n%s", want, report)
		}
	}
	if !strings.Contains(j.Gate.Report, "007-install-existing-skill") {
		t.Errorf("退回的理由要写哪题：%s", j.Gate.Report)
	}
}

// 验收：没有基线只记录不挡、提示 agentq eval baseline；候选改了评测题，评测没挡也不自动合并，等用户拍板。
func TestIntegrationEvalGateNoBaselineAndJudgeChangeHolds(t *testing.T) {
	t.Parallel()
	e, dir := selfRepoEnv(t, "echo 多一行 >> evals/README.md\n")
	j := e.gateOnce(evalGateTitle)
	report := e.mustRun("gate", "report", j.ID)
	for _, want := range []string{"eval（error 档）：pass 没有能比的基线，只记录不挡", "agentq eval baseline", "耗时 ", "评判的尺子", "evals/README.md"} {
		if !strings.Contains(report, want) {
			t.Errorf("报告缺 %s：\n%s", want, report)
		}
	}
	if b := e.backlogOf(dir); j.Gate.State != state.GateStuck || !strings.Contains(b, "等你拍板") {
		t.Fatalf("改了尺子要等用户：%+v\n%s", j.Gate, b)
	}
	if got := testkit.GitT(t, e.home, dir, "show", "main:evals/README.md"); strings.Contains(got, "多一行") {
		t.Error("不能合进 main")
	}
}
