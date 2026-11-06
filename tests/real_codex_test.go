package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/proc"
	"agentq/internal/setup"
	statepkg "agentq/internal/state"
	"agentq/internal/testkit"
)

// 真实 Codex 验收，必须显式打开：AGENTQ_REAL_CODEX=1 go test -count=1 -v -run '^TestRealCodex' .
// 消耗已登录 ChatGPT 账号的额度，提示词极小。临时 HOME、临时 Git 项目；CODEX_HOME 指向现有登录，只读使用（Codex 自己会写会话记录，这改不了）。
// 不用用户真实的 config.toml 和钩子：包一层脚本带 --ignore-user-config、--disable hooks。
// 钩子关掉是实测的教训：用户的 Stop 钩子在临时 HOME 下找不到脚本，每次都拦下结束，模型被反复叫回来，一直花额度。
// Codex 在可写沙箱下会把项目目录写进 $CODEX_HOME/config.toml（trust_level），--ignore-user-config 也挡不住：用例结束时把自己这个临时目录的那几段删掉，别的字节不动。
type realCodex struct {
	t       *testing.T
	base    string
	home    string
	env     []string
	bin     string
	project string
}

func realCodexEnv(t *testing.T) *realCodex {
	t.Helper()
	if os.Getenv("AGENTQ_REAL_CODEX") != "1" {
		t.Skip("真实 Codex 验收要 AGENTQ_REAL_CODEX=1")
	}
	cli, err := exec.LookPath("codex")
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
	base, err := os.MkdirTemp("", "agentq-real-codex-*")
	if err != nil {
		t.Fatal(err)
	}
	base, _ = filepath.EvalSymlinks(base)
	t.Log("材料留在（不进仓库）：", base)
	r := &realCodex{t: t, base: base, home: filepath.Join(base, "home")}
	t.Cleanup(func() { removeTrustEntries(t, filepath.Join(codexHome, "config.toml"), base) })
	t.Cleanup(func() { // 用例半路失败时别把真实的 Codex 留在后台花额度
		st, err := statepkg.Load(filepath.Join(r.home, ".agentq", "state.json"))
		if err != nil {
			return
		}
		for _, j := range st.Running() {
			if j.PID > 0 {
				proc.KillGroup(j.PID, proc.KillGrace)
			}
		}
	})
	if err := os.MkdirAll(r.home, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(base, "bin", "codex")
	testkit.WriteInstall(t, wrapper, fmt.Sprintf(`#!/bin/sh
# 带上隔离参数再 exec 真的 codex：pid 不变，环境不变
flags='--ignore-user-config --disable hooks'
if [ "$1" = exec ] && [ "$2" = resume ]; then
  id=$3; shift 3
  exec %[1]q exec resume "$id" $flags "$@"
fi
if [ "$1" = exec ] && [ "$2" != --help ]; then
  shift
  exec %[1]q exec $flags "$@"
fi
exec %[1]q "$@"
`, cli), 0o755)
	r.env = setup.InstallEnv(r.home)
	r.env = append(r.env, "PATH="+filepath.Dir(wrapper)+":"+os.Getenv("PATH"), "CODEX_HOME="+codexHome, "GIT_CONFIG_NOSYSTEM=1")
	testkit.WriteInstall(t, filepath.Join(r.home, ".gitconfig"), "[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n[core]\n\thooksPath = /dev/null\n", 0o644)
	out := r.run(r.home, agentqBin, "setup", "--yes", "--backend", "codex", "--backend-path", wrapper)
	os.WriteFile(filepath.Join(base, "install.log"), out, 0o600)
	r.bin = filepath.Join(r.home, ".local", "bin", "agentq")
	if !exists(filepath.Join(r.home, ".agents", "skills", "agent-task", "SKILL.md")) || !exists(filepath.Join(r.home, ".agents", "skills", "check", "SKILL.md")) {
		t.Fatal("技能没投影到 ~/.agents/skills")
	}
	doctor := r.run(r.home, r.bin, "doctor")
	os.WriteFile(filepath.Join(base, "doctor.log"), doctor, 0o600)
	t.Logf("doctor：\n%s", sanitizeReal(string(doctor), r))
	return r
}

// removeTrustEntries 删掉 config.toml 里 prefix 下目录的信任记录。原地写，权限和扩展属性不变。
func removeTrustEntries(t *testing.T, path, prefix string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Logf("读不了 %s，信任记录没清：%v", filepath.Base(path), err)
		return
	}
	re := regexp.MustCompile(`\n?\[projects\."` + regexp.QuoteMeta(prefix) + `[^"]*"\]\ntrust_level = "trusted"\n`)
	out := re.ReplaceAll(b, nil)
	if bytes.Equal(out, b) {
		return
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Logf("信任记录没清：%v", err)
	}
}

func sanitizeReal(s string, r *realCodex) string {
	s = strings.ReplaceAll(s, r.base, "<临时目录>")
	if h, err := os.UserHomeDir(); err == nil {
		s = strings.ReplaceAll(s, h, "~")
	}
	return s
}

func (r *realCodex) run(dir, name string, args ...string) []byte {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = r.env
	b, err := c.CombinedOutput()
	if err != nil {
		os.WriteFile(filepath.Join(r.base, "failure.log"), b, 0o600)
		r.t.Fatalf("%s %v 失败：%v；\n%s", filepath.Base(name), args, err, sanitizeReal(string(b), r))
	}
	return b
}

// project 建临时项目和配置，待做里放一条。
func (r *realCodex) setProject(item string) {
	t := r.t
	cfgPath := filepath.Join(r.home, ".agentq", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(r.base, "projects")
	cfg["projects_root"] = root
	cfg["permission_mode"] = "danger-full-access"
	cfg["tick_after_job"] = false
	cfg["timeout"] = "20m"
	b, _ := json.MarshalIndent(cfg, "", "  ")
	testkit.WriteInstall(t, cfgPath, string(b)+"\n", 0o600)
	dir := filepath.Join(root, "fixture")
	r.project = dir
	testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n"+item+"\n\n## 等用户决定\n\n## 已完成\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, "PROGRESS.md"), "# 进度\n\n## 当前任务\n无\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, "README.md"), "fixture\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, ".gitignore"), ".claude/worktrees/\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, "AGENTS.md"), "# 验收项目\n只有这个临时仓库获得授权。没有远程，不发消息，不改全局配置。\n", 0o644)
	testkit.WriteInstall(t, filepath.Join(dir, gate.VerifyScript), "#!/bin/sh\n[ -f a.txt ] && grep -q hi a.txt\n", 0o755)
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		r.run(dir, "/usr/bin/git", a...)
	}
}

func (r *realCodex) state() *statepkg.State {
	r.t.Helper()
	st, err := statepkg.Load(filepath.Join(r.home, ".agentq", "state.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *realCodex) events(j *statepkg.Job) string {
	b, _ := os.ReadFile(filepath.Join(r.home, ".agentq", "jobs", j.ID, "result.json"))
	return string(b)
}

// waitGate 反复 tick，直到这一条走到门的结论。
func (r *realCodex) waitGate(limit time.Duration) *statepkg.Job {
	r.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		out := r.run(r.home, r.bin, "tick")
		os.WriteFile(filepath.Join(r.base, "last-tick.log"), out, 0o600)
		st := r.state()
		for _, j := range st.Jobs {
			if j.Gate != nil && j.Gate.State != statepkg.GateQueued && j.Gate.State != statepkg.GateRunning {
				return j
			}
		}
		if len(st.Running()) == 0 && len(st.Jobs) > 0 && !anyGate(st) && len(st.PendingResumes()) == 0 {
			last := st.Jobs[len(st.Jobs)-1]
			r.t.Fatalf("任务结束了但没进门：%s：%s\n事件：\n%s", last.Status, sanitizeReal(last.Summary, r), tailOf(r.events(last), 1500))
		}
		time.Sleep(10 * time.Second)
	}
	r.t.Fatalf("等了 %s 还没到门的结论；%+v", limit, sanitizeReal(string(r.run(r.home, r.bin, "status")), r))
	return nil
}

func anyGate(st *statepkg.State) bool {
	for _, j := range st.Jobs {
		if j.Gate != nil {
			return true
		}
	}
	return false
}

func (r *realCodex) report(j *statepkg.Job) {
	t := r.t
	t.Logf("任务 %s：后端 %s，状态 %s，会话 %s，用量 %+v", j.ID, j.Backend, j.Status, j.SessionID, j.Usage)
	t.Logf("参数：%s", sanitizeReal(strings.Join(j.Args, " "), r))
	t.Logf("摘要：%s", sanitizeReal(j.Summary, r))
	if j.Gate != nil {
		t.Logf("门：状态 %s，分支 %s，合并提交 %s", j.Gate.State, j.Gate.Branch, j.Gate.Merge)
		t.Logf("门报告：\n%s", sanitizeReal(string(r.run(r.home, r.bin, "gate", "report", j.ID)), r))
	}
	reviews, _ := filepath.Glob(filepath.Join(r.home, ".agentq", "gate", "reviews", "fixture", "*.json"))
	for _, p := range reviews {
		b, _ := os.ReadFile(p)
		t.Logf("审查记录：%s", sanitizeReal(string(b), r))
	}
}

func (r *realCodex) assertMerged(j *statepkg.Job) {
	t := r.t
	if j.Gate == nil || j.Gate.State != statepkg.GateMerged {
		t.Fatalf("没有合并：%+v", j.Gate)
	}
	if !exists(filepath.Join(r.project, "a.txt")) {
		t.Error("a.txt 没进主目录")
	}
	reviews, _ := filepath.Glob(filepath.Join(r.home, ".agentq", "gate", "reviews", "fixture", "*.json"))
	if len(reviews) == 0 {
		t.Error("没有审查记录：审查门没真跑")
	}
	for _, p := range reviews {
		var rec gate.ReviewRecord
		b, _ := os.ReadFile(p)
		if json.Unmarshal(b, &rec) != nil || rec.Backend != "codex" || rec.Verdict == "" || rec.Verdict == "unknown" {
			t.Errorf("审查记录不对：%s", b)
		}
	}
	if j.Backend != "codex" || j.SessionID == "" {
		t.Errorf("%+v", j)
	}
	if b := string(mustReadFile(t, filepath.Join(r.project, "docs", "BACKLOG.md"))); !strings.Contains(b, "- [x] ") || strings.Contains(b, "（进行中") {
		t.Errorf("BACKLOG 没收尾：%s", b)
	}
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 闭环：tick 启动 Codex、分支提交、写 ready.json、过测试门和审查门、引擎合并。
func TestRealCodexClosedLoop(t *testing.T) {
	r := realCodexEnv(t)
	r.setProject("- [ ] 在 a.txt 写一行 hi 并提交 —— 验收：a.txt 里有 hi")
	r.run(r.home, r.bin, "resume")
	j := r.waitGate(20 * time.Minute)
	r.report(j)
	r.assertMerged(j)
	t.Logf("起了 %d 个任务", len(r.state().Jobs))
}

// SIGTERM 之后：记中断，exec resume 同一个会话接着做完，过门合并。
func TestRealCodexSigtermResume(t *testing.T) {
	r := realCodexEnv(t)
	r.setProject("- [ ] 在 a.txt 写一行 hi 并提交，提交前运行一次 sleep 45 —— 验收：a.txt 里有 hi")
	r.run(r.home, r.bin, "resume")
	r.run(r.home, r.bin, "tick")
	st := r.state()
	if len(st.Jobs) != 1 {
		t.Fatalf("没起任务：%+v", st.Jobs)
	}
	first := st.Jobs[0]
	backlog := filepath.Join(r.project, "docs", "BACKLOG.md")
	deadline := time.Now().Add(10 * time.Minute)
	for !strings.Contains(string(mustReadFile(t, backlog)), "（进行中") { // 认领了才算做到一半，没认领的被杀只是白起一次
		if time.Now().After(deadline) {
			t.Fatalf("等不到认领：\n%s", tailOf(r.events(first), 1500))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := syscall.Kill(first.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	t.Logf("发了 SIGTERM，pid %d", first.PID)
	r.run(r.home, r.bin, "pause")
	deadline = time.Now().Add(time.Minute)
	for syscall.Kill(first.PID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("SIGTERM 之后一分钟还没退")
		}
		time.Sleep(time.Second)
	}
	if out, err := exec.Command("/bin/ps", "-A", "-ww", "-o", "pid=,pgid=,command=").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "sleep 45") {
				t.Logf("SIGTERM 之后还在的子进程：%s", line)
			}
		}
	}
	r.run(r.home, r.bin, "tick")
	st = r.state()
	first = st.Jobs[0]
	t.Logf("收尾：%s，续跑=%v，会话 %s：%s", first.Status, first.ResumePending, first.SessionID, sanitizeReal(first.Summary, r))
	if first.Status != statepkg.StatusInterrupted || !first.ResumePending || first.SessionID == "" {
		t.Fatalf("没记成可续跑的中断：\n%s", tailOf(r.events(first), 1500))
	}
	r.run(r.home, r.bin, "resume")
	j := r.waitGate(20 * time.Minute)
	r.report(j)
	st = r.state()
	found := false
	for _, x := range st.Jobs {
		if x.ResumeOf == first.ID {
			found = true
			if x.SessionID != first.SessionID {
				t.Errorf("换了会话：%s", x.SessionID)
			}
			if !strings.Contains(strings.Join(x.Args, " "), "resume "+first.SessionID) {
				t.Errorf("%v", x.Args)
			}
		}
	}
	if !found {
		t.Error("没有续跑任务")
	}
	r.assertMerged(j)
}
