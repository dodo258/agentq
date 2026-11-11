package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/proc"
	"agentq/internal/setup"
	statepkg "agentq/internal/state"
	"agentq/internal/testkit"
)

// 真实 pi 验收，必须显式打开：AGENTQ_REAL_PI=1 go test -count=1 -v -run '^TestRealPi' ./tests
// 花本机 pi 配的模型额度（跟随 ~/.pi/agent/settings.json 的 defaultProvider、defaultModel）。
// 临时 HOME、临时 Git 项目。凭据和模型设置只软链接回真实的 ~/.pi/agent（auth.json、models-store.json、models.json、settings.json，pi 都是原地写），
// 不复制；会话、信任记录、扩展都在临时 HOME 里，用户的扩展不加载。
func realPiEnv(t *testing.T) *realCodex {
	t.Helper()
	if os.Getenv("AGENTQ_REAL_PI") != "1" {
		t.Skip("真实 pi 验收要 AGENTQ_REAL_PI=1")
	}
	cli, err := exec.LookPath("pi")
	if err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	realAgent := os.Getenv("PI_CODING_AGENT_DIR")
	if realAgent == "" {
		realAgent = filepath.Join(userHome, ".pi", "agent")
	}
	base, err := os.MkdirTemp("", "agentq-real-pi-*")
	if err != nil {
		t.Fatal(err)
	}
	base, _ = filepath.EvalSymlinks(base)
	t.Log("材料留在（不进仓库）：", base)
	r := &realCodex{t: t, base: base, home: filepath.Join(base, "home"), backend: "pi"}
	t.Cleanup(func() { // 用例半路失败时别把真实的 pi 留在后台花额度
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
	agent := filepath.Join(r.home, ".pi", "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"auth.json", "models-store.json", "models.json", "settings.json"} {
		if _, err := os.Stat(filepath.Join(realAgent, f)); err != nil {
			continue
		}
		if err := os.Symlink(filepath.Join(realAgent, f), filepath.Join(agent, f)); err != nil {
			t.Fatal(err)
		}
	}
	r.env = setup.InstallEnv(r.home)
	r.env = append(r.env, "PATH="+filepath.Dir(cli)+":"+os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", config.TestTuningEnv+`={"tick_after_job":false}`)
	testkit.WriteInstall(t, filepath.Join(r.home, ".gitconfig"), "[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n[core]\n\thooksPath = /dev/null\n", 0o644)
	out := r.run(r.home, agentqBin, "setup", "--yes", "--backend", "pi", "--backend-path", cli)
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

const realPiCLI = `#!/bin/sh
case "$1" in
  version) echo 1.0 ;;
  *) echo "用法：sh cli.sh <命令>" >&2; exit 1 ;;
esac
`

// setPiProject：一个最小的命令行小工具，待做是加一个 hello 命令；测试门跑 sh cli.sh hello。
func (r *realCodex) setPiProject(item string) {
	r.artifact = "cli.sh"
	r.setProjectWith(item, "full", "#!/bin/sh\n[ \"$(sh cli.sh hello)\" = hello ]\n", map[string]string{"cli.sh": realPiCLI})
}

func (r *realCodex) assertHello() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "cli.sh", "hello")
	c.Dir = r.project
	out, err := c.Output()
	if err != nil || strings.TrimSpace(string(out)) != "hello" {
		r.t.Errorf("主目录里 sh cli.sh hello：%q %v", out, err)
	}
}

// 闭环：tick 启动 pi、分支提交、写 ready.json、过测试门和审查门（同样是 pi）、引擎合并。
func TestRealPiClosedLoop(t *testing.T) {
	r := realPiEnv(t)
	r.setPiProject("- [ ] 给 cli.sh 加一个 hello 命令：sh cli.sh hello 输出 hello —— 验收：sh cli.sh hello 输出 hello")
	r.run(r.home, r.bin, "resume")
	j := r.waitGate(30 * time.Minute)
	r.report(j)
	r.assertMerged(j)
	r.assertHello()
	t.Logf("起了 %d 个任务", len(r.state().Jobs))
}

// SIGTERM 之后：记中断，--session 接回同一个会话做完，过门合并。
func TestRealPiSigtermResume(t *testing.T) {
	r := realPiEnv(t)
	r.setPiProject("- [ ] 给 cli.sh 加一个 hello 命令：sh cli.sh hello 输出 hello，提交前运行一次 sleep 45 —— 验收：sh cli.sh hello 输出 hello")
	r.run(r.home, r.bin, "resume")
	r.run(r.home, r.bin, "tick")
	st := r.state()
	if len(st.Jobs) != 1 {
		t.Fatalf("没起任务：%+v", st.Jobs)
	}
	first := st.Jobs[0]
	backlog := filepath.Join(r.project, "docs", "BACKLOG.md")
	deadline := time.Now().Add(15 * time.Minute)
	for !strings.Contains(string(mustReadFile(t, backlog)), "（进行中") { // 认领了才算做到一半
		if time.Now().After(deadline) {
			t.Fatalf("等不到认领：\n%s", tailOf(r.events(first), 1500))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := syscall.Kill(first.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	t.Logf("发了 SIGTERM，pid %d，会话 %s", first.PID, first.SessionID)
	r.run(r.home, r.bin, "pause")
	deadline = time.Now().Add(time.Minute)
	for syscall.Kill(first.PID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("SIGTERM 之后一分钟还没退")
		}
		time.Sleep(time.Second)
	}
	r.run(r.home, r.bin, "tick")
	st = r.state()
	first = st.Jobs[0]
	t.Logf("收尾：%s，续跑=%v，会话 %s：%s", first.Status, first.ResumePending, first.SessionID, sanitizeReal(first.Summary, r))
	if first.Status != statepkg.StatusInterrupted || !first.ResumePending || first.SessionID == "" {
		t.Fatalf("没记成可续跑的中断：\n%s", tailOf(r.events(first), 1500))
	}
	r.run(r.home, r.bin, "resume")
	j := r.waitGate(30 * time.Minute)
	r.report(j)
	found := false
	for _, x := range r.state().Jobs {
		if x.ResumeOf == first.ID {
			found = true
			if x.SessionID != first.SessionID || !strings.Contains(strings.Join(x.Args, " "), "--session "+first.SessionID) {
				t.Errorf("没接回原会话：%s %v", x.SessionID, x.Args)
			}
		}
	}
	if !found {
		t.Error("没有续跑任务")
	}
	r.assertMerged(j)
	r.assertHello()
}
