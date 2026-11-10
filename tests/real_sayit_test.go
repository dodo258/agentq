package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	statepkg "agentq/internal/state"
	"agentq/internal/testkit"
)

// 装好，开口说（真实 CLI）：临时 HOME 里从仓库的 install.sh 装（服务走假 launchctl，真实 launchd 不接受临时 HOME），
// 然后在 projects_root 外两个不是 Git 仓库的新目录里，分别用 claude -p 和 codex exec 只说一句「给这个项目加一个 hello 命令」，
// 不提派活、不提 agentq。要看到：目录被接入（git init、BACKLOG 有一条 hello）、任务起来了。起来就停掉，不把额度花在做完上。
// 再问 claude 一个问题，不该进后台。
// 身份：Claude 走 macOS 钥匙串，临时 HOME 里放一个指向它的链接（和任务外壳的做法一样，不复制）；Codex 的 CODEX_HOME 指向现有登录，
// 只读使用，结束时删掉它写进 config.toml 的临时目录信任记录。会消耗两家的额度，必须显式 AGENTQ_REAL_E2E=1。
func TestRealSayItE2E(t *testing.T) {
	if os.Getenv("AGENTQ_REAL_E2E") != "1" {
		t.Skip("真实验收要 AGENTQ_REAL_E2E=1")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("Claude 身份靠 macOS 钥匙串")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(userHome, ".codex")
	}
	base, err := os.MkdirTemp("", "agentq-real-sayit-*")
	if err != nil {
		t.Fatal(err)
	}
	base, _ = filepath.EvalSymlinks(base)
	t.Log("材料留在（不进仓库）：", base)
	t.Cleanup(func() { removeTrustEntries(t, filepath.Join(codexHome, "config.toml"), base) })
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
		t.Fatal(err)
	}
	keychains := filepath.Join(home, "Library", "Keychains")
	if err := os.Symlink(filepath.Join(userHome, "Library", "Keychains"), keychains); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(keychains) }) // 材料留着，指向钥匙串的链接不留
	testkit.WriteInstall(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n", 0o644)
	launchctl := filepath.Join(base, "bin", "launchctl")
	testkit.WriteInstall(t, launchctl, installFakeLaunchctl, 0o755)
	agentq := filepath.Join(home, ".local", "bin", "agentq")
	dirs := []string{filepath.Join(home, ".local", "bin"), filepath.Dir(claude), filepath.Dir(codex), filepath.Dir(gobin)}
	if node, err := exec.LookPath("node"); err == nil { // npm 装的 codex 是 node 脚本
		dirs = append(dirs, filepath.Dir(node))
	}
	path := strings.Join(append(dirs, "/usr/bin", "/bin", "/usr/sbin"), ":")
	env := []string{"HOME=" + home, "PATH=" + path, "USER=" + os.Getenv("USER"), "LANG=en_US.UTF-8", "TMPDIR=" + os.TempDir(),
		"AGENTQ_TEST_LAUNCHCTL=" + launchctl, "CODEX_HOME=" + codexHome, "GIT_CONFIG_NOSYSTEM=1", "SHELL=/bin/zsh"}
	t.Cleanup(func() { // 半路失败时别把真实任务留在后台花额度
		st, err := statepkg.Load(filepath.Join(home, ".agentq", "state.json"))
		if err != nil {
			return
		}
		for _, j := range st.Running() {
			c := exec.Command(agentq, "stop", j.ID)
			c.Env = env // 只动临时 HOME 里的任务
			c.Run()
		}
	})
	run := func(dir, log string, timeout time.Duration, name string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c := exec.CommandContext(ctx, name, args...)
		c.Dir, c.Env = dir, env
		b, err := c.CombinedOutput()
		os.WriteFile(filepath.Join(base, log), b, 0o600)
		if err != nil {
			t.Fatalf("%s 失败：%v，输出在 %s", filepath.Base(name), err, log)
		}
		return string(b)
	}
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !exists(codexHome) {
		t.Fatalf("没有 %s：这台机器没登录过 Codex", codexHome)
	}
	out := run(home, "install.log", 10*time.Minute, "/bin/sh", filepath.Join(repo, "install.sh"))
	t.Logf("install.sh：\n%s", strings.ReplaceAll(out, base, "<临时目录>"))
	testkit.AssertContains(t, out, "后台已启动")
	for _, dir := range []string{".claude", ".agents"} {
		if !exists(filepath.Join(home, dir, "skills", "dispatch", "SKILL.md")) {
			t.Fatalf("%s 下没有派活技能", dir)
		}
	}

	waitJob := func(project string) *Job {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			st, err := statepkg.Load(filepath.Join(home, ".agentq", "state.json"))
			if err == nil {
				for _, j := range st.Jobs {
					if j.Project == project {
						return j
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
		return nil
	}
	checkProject := func(dir, name, transcript string) {
		t.Helper()
		if !exists(filepath.Join(dir, ".git")) {
			t.Fatalf("%s 没接入成 Git 仓库，见 %s", name, transcript)
		}
		b, err := os.ReadFile(filepath.Join(dir, "docs", "BACKLOG.md"))
		if err != nil {
			t.Fatalf("%s 没有 BACKLOG：%v，见 %s", name, err, transcript)
		}
		var hello string
		for _, it := range bl.ParseBacklog(string(b)) {
			if strings.Contains(strings.ToLower(it.Text), "hello") {
				hello = it.Text
			}
		}
		if hello == "" {
			t.Fatalf("%s 的待做里没有 hello：\n%s", name, b)
		}
		t.Logf("%s 写出的待做：%s", name, hello)
		j := waitJob(name)
		if j == nil {
			t.Fatalf("%s 没起任务，见 %s", name, transcript)
		}
		t.Logf("%s 起了任务 %s（%s）", name, j.ID, j.Status)
		out := run(dir, "stop-"+name+".log", time.Minute, agentq, "stop", j.ID)
		t.Logf("停掉：%s", strings.TrimSpace(out))
	}
	newDir := func(name string) string {
		dir := filepath.Join(base, "work", name)
		testkit.WriteInstall(t, filepath.Join(dir, "main.py"), "import sys\n\nif __name__ == \"__main__\":\n    print(\"usage: main.py <command>\")\n", 0o644)
		return dir
	}
	const ask = "给这个项目加一个 hello 命令"

	// Claude Code：projects_root 外、不是 Git 仓库。
	cdir := newDir("claude-app")
	out = run(cdir, "claude.jsonl", 10*time.Minute, claude, "-p", ask, "--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose")
	if !strings.Contains(out, `"skill":"dispatch"`) {
		t.Fatal("claude 没调派活技能，见 claude.jsonl")
	}
	t.Logf("claude 最后回答：%s", lastResult(out))
	checkProject(cdir, "claude-app", "claude.jsonl")

	// 只是问问：不进后台，BACKLOG 不多一条。
	before := testkit.ReadFile(t, filepath.Join(cdir, "docs", "BACKLOG.md"))
	out = run(cdir, "claude-ask.jsonl", 5*time.Minute, claude, "-p", "main.py 现在能做什么？一句话回答。", "--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose")
	if testkit.ReadFile(t, filepath.Join(cdir, "docs", "BACKLOG.md")) != before {
		t.Fatal("只是问问不该写待做")
	}
	t.Logf("claude 问答：%s", lastResult(out))

	// Codex：projects_root 外、不是 Git 仓库（codex exec 要 --skip-git-repo-check 才肯在这里跑）。
	xdir := newDir("codex-app")
	out = run(xdir, "codex.log", 10*time.Minute, codex, "exec", "--ignore-user-config", "--disable", "hooks", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", ask)
	t.Logf("codex 输出末尾：\n%s", tailOf(strings.ReplaceAll(out, base, "<临时目录>"), 1500))
	checkProject(xdir, "codex-app", "codex.log")
}

// lastResult 取 stream-json 里最后的 result 文字。
func lastResult(jsonl string) string {
	i := strings.LastIndex(jsonl, `"result":"`)
	if i < 0 {
		return "（没有 result）"
	}
	s := jsonl[i+len(`"result":"`):]
	if j := strings.Index(s, `","`); j >= 0 {
		s = s[:j]
	}
	return strings.ReplaceAll(s, `\n`, "\n")
}
