package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/testkit"
)

// 假 codex：install.sh 走到 setup、doctor 探测用。不调真实 CLI。
const installFakeCodex = `#!/bin/sh
case "$1 $2" in
"--version "*) echo "codex-cli 0.166.0" ;;
"exec --help") echo "--json --output-schema resume" ;;
"login status") echo "Logged in using ChatGPT" ;;
*) exit 0 ;;
esac
`

// install.sh --backend：和 setup --backend 一样，技能进这个后端的目录，配置写下它，不碰 ~/.claude。
func TestInstallShBackendFlag(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t, "claude")
	k.writeBin("codex", installFakeCodex)
	k.serveRelease("v1.0.0")
	k.mustInstall("--backend", "codex")
	if !exists(filepath.Join(k.home, ".agents", "skills", "agent-task", "SKILL.md")) {
		t.Fatal("技能应进 ~/.agents/skills")
	}
	if exists(filepath.Join(k.home, ".claude", "skills")) {
		t.Fatal("只选 Codex 不该往 ~/.claude 放技能")
	}
	b := testkit.ReadBytes(t, k.configPath())
	if !strings.Contains(string(b), `"backend": "codex"`) {
		t.Fatalf("配置没写 backend：\n%s", b)
	}
	// 只用 Codex 时，坏掉的 Claude 设置文件不关 agentq 的事（安装时已经不查，doctor 也要一样）
	os.MkdirAll(filepath.Join(k.home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(k.home, ".claude", "settings.json"), []byte("{坏的"), 0o644)
	if out, _ := k.agentq(k.home, "doctor"); strings.Contains(out, "invalid Claude settings") {
		t.Fatalf("doctor 不该查 Claude 的设置：\n%s", out)
	}
	help, code := k.run("--help")
	if code != 0 || !strings.Contains(help, "--backend <名字>") {
		t.Fatalf("帮助要说 --backend：%d\n%s", code, help)
	}
}

// 默认后端不是 Claude：提示词不说 claude -p，也不让任务往 ~/.claude 写报告；引擎和技能的锁在同一处，不新建 ~/.claude。
func TestNonClaudeHostPromptReportsLocks(t *testing.T) {
	e := codexEnv(t, map[string]any{"review_enabled": true})
	gateRepo(t, e, "demo")
	e.mustRun("review")
	st := e.state()
	if len(st.Jobs) == 0 {
		t.Fatal("复盘没启动")
	}
	prompt := string(testkit.ReadBytes(t, filepath.Join(e.home, ".agentq", "jobs", st.Jobs[0].ID, "prompt.txt")))
	testkit.AssertContains(t, prompt, "~/.agentq/nightly/", "~/.agents/skills/nightly-review/SKILL.md", "codex")
	if strings.Contains(prompt, "claude -p") || strings.Contains(prompt, "~/.claude/") {
		t.Fatalf("提示词还在说 Claude：\n%s", prompt)
	}
	locks := filepath.Join(e.home, ".agentq", "locks")
	if !isDir(locks) {
		t.Fatal("启动任务时要建好 ~/.agentq/locks，with-lock.sh 靠它认")
	}
	// with-lock.sh 和引擎同一处
	script, err := filepath.Abs(filepath.Join("support", "scripts", "with-lock.sh"))
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", script, "res", "ls", locks)
	c.Env = append(os.Environ(), "HOME="+e.home)
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "res") {
		t.Fatalf("with-lock.sh 应该锁在 %s：%v\n%s", locks, err, out)
	}
	if isDir(filepath.Join(e.home, ".claude", "nightly")) || isDir(filepath.Join(e.home, ".claude", "state")) {
		t.Fatal("默认后端不是 Claude 时不往 ~/.claude 写报告和锁")
	}
	// 收尾写当天汇总也在新目录
	e.tickUntilEnded(func() *Job { return e.state().Jobs[0] })
	day := time.Now().Format("2006-01-02")
	if !exists(filepath.Join(e.home, ".agentq", "nightly", day+"-agentq.md")) {
		t.Fatal("当天汇总应写 ~/.agentq/nightly")
	}
}

// 默认后端是 Claude 的老配置：提示词、报告和锁的位置一个字不变。
func TestClaudeHostPathsUnchanged(t *testing.T) {
	e := newEnv(t, map[string]any{"review_enabled": true})
	gateRepo(t, e, "demo")
	e.mustRun("review")
	st := e.state()
	if len(st.Jobs) == 0 {
		t.Fatal("复盘没启动")
	}
	prompt := string(testkit.ReadBytes(t, filepath.Join(e.home, ".agentq", "jobs", st.Jobs[0].ID, "prompt.txt")))
	testkit.AssertContains(t, prompt, "~/.claude/nightly/", "~/.claude/skills/nightly-review/SKILL.md", "claude -p")
	if isDir(filepath.Join(e.home, ".agentq", "locks")) || isDir(filepath.Join(e.home, ".agentq", "nightly")) {
		t.Fatal("默认后端是 Claude 时不该建新目录")
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// 假 pi：install.sh 走到 setup、doctor 探测用。不调真实 CLI。
const installFakePi = `#!/bin/sh
case "$1 $2" in
"--version "*) echo "1.5.1" ;;
"--help "*) echo "--mode --session-id --no-approve" ;;
"auth check") echo '{"status":"ready","provider":"fakep","authType":"api_key"}' ;;
*) exit 0 ;;
esac
`

// install.sh --backend pi：技能进 ~/.agents/skills，配置写下 pi，不碰 ~/.claude。
func TestInstallShBackendPi(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t, "claude")
	k.writeBin("pi", installFakePi)
	testkit.WriteInstall(t, filepath.Join(k.home, ".pi", "agent", "settings.json"), `{"defaultProvider":"fakep"}`+"\n", 0o644)
	k.serveRelease("v1.0.0")
	k.mustInstall("--backend", "pi")
	if !exists(filepath.Join(k.home, ".agents", "skills", "agent-task", "SKILL.md")) || exists(filepath.Join(k.home, ".claude", "skills")) {
		t.Fatal("技能应进 ~/.agents/skills，不往 ~/.claude 放")
	}
	if b := testkit.ReadBytes(t, k.configPath()); !strings.Contains(string(b), `"backend": "pi"`) {
		t.Fatalf("配置没写 backend：\n%s", b)
	}
	if out, _ := k.agentq(k.home, "doctor"); !strings.Contains(out, "后端：pi") {
		t.Fatalf("doctor 按 pi 查：\n%s", out)
	}
}
