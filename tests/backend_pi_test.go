package tests

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 假 pi 和假 codex 是同一个编译好的假 CLI（ps -E 看得到环境）：按 pi -p --mode json 被调时写 pi 的事件流、从标准输入读提示词。

func piEnv(t *testing.T, extra map[string]any) *testEnv {
	t.Helper()
	cfg := map[string]any{"backend": "pi", "permission_mode": "full",
		"backends": map[string]any{"pi": map[string]any{"path": codexFake(t)}}}
	for k, v := range extra {
		cfg[k] = v
	}
	e := newEnv(t, cfg)
	// 选过模型来源：启动前会跑 pi auth check
	testkit.WriteInstall(t, filepath.Join(e.home, ".pi", "agent", "settings.json"), `{"defaultProvider":"fakep","defaultModel":"m"}`+"\n", 0o644)
	return e
}

func authCalls(e *testEnv) []string {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake", "calls.log"))
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"ev":"auth"`) {
			out = append(out, line)
		}
	}
	return out
}

// 完整闭环：tick 启动、提示词走标准输入、预设会话 id、分支提交、ready.json、测试门、审查门（同样是 pi，靠提示词要 JSON）、引擎合并。
func TestPiFullLoopMerges(t *testing.T) {
	e := piEnv(t, nil)
	e.extra = append(e.extra, "PI_CODING_AGENT_DIR="+filepath.Join(e.home, ".pi", "agent"), "DEEPSEEK_API_KEY=k1", "OPENAI_API_KEY=k2", "ANTHROPIC_API_KEY=k3", "CODEX_HOME="+filepath.Join(e.home, "ch"))
	dir := gateRepo(t, e, "demo", codexItem("aaa", "ok"))
	before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	e.mustRun("tick")
	title := "条目 ITEM-aaa"
	j := e.jobByTitle(title)
	if j.Backend != "pi" || j.SessionID == "" {
		t.Fatalf("%+v", j)
	}
	if strings.Contains(strings.Join(j.Args, " "), "条目 ITEM-aaa —") {
		t.Errorf("提示词不进参数：%q", j.Args)
	}
	j = e.tickUntilGate(title)
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", before+"..main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] 条目 ITEM-aaa") {
		t.Errorf("%s", b)
	}
	// 任务和审查各花一次，金额有
	if j.Usage == nil || j.Usage.CostUnknown || j.Usage.Input != 200 || j.Usage.CacheRead != 80 || j.Usage.Output != 14 {
		t.Errorf("用量 %+v", j.Usage)
	}
	calls := codexCalls(e)
	if len(calls) != 2 || calls[0].Kind != "task" || calls[1].Kind != "review" {
		t.Fatalf("%+v", calls)
	}
	task, review := calls[0], calls[1]
	if task.Job != j.ID || !strings.Contains(task.Head, "~/.agents/skills/agent-task/SKILL.md") || strings.Contains(task.Head, "~/.claude/skills") {
		t.Errorf("提示词要点 pi 读得到的技能路径：%+v", task)
	}
	if !strings.HasPrefix(task.Args, "-p --mode json --session-id "+j.SessionID) || strings.Contains(task.Args, "--no-approve") || strings.Contains(task.Args, "--model") || strings.Contains(task.Args, "--thinking") || strings.Contains(task.Args, "full") {
		t.Errorf("任务的参数（没配模型就跟随 pi 自己的设置，权限不变成参数）：%s", task.Args)
	}
	if !strings.Contains(review.Args, "--no-approve") || !strings.Contains(review.Args, "--thinking medium") {
		t.Errorf("审查的参数：%s", review.Args)
	}
	for _, c := range calls {
		if !slices.Equal(c.Login, []string{"PI_CODING_AGENT_DIR"}) {
			t.Errorf("只传 pi 的配置目录，API key 和别家的登录变量都不传：%v", c.Login)
		}
	}
	if a := authCalls(e); len(a) == 0 || !strings.Contains(a[0], "auth check --provider fakep --json") {
		t.Errorf("启动前查默认模型来源的凭据：%v", a)
	}
}

// 进程被杀：没有 agent_settled，不当成功；用启动时预设的会话 id 续跑（--session）。
func TestPiKilledMidwayResumes(t *testing.T) {
	e := piEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("eee", "claimhang"))
	e.mustRun("run", "demo")
	title := "条目 ITEM-eee"
	j := e.jobByTitle(title)
	preset := j.SessionID
	waitFor(t, 10*time.Second, "假 pi 写出 session 头", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j.ID, "result.json"))
		return strings.Contains(string(b), `"type":"session"`)
	})
	e.mustRun("pause")
	e.mustRun("tick")
	if got := e.jobByTitle(title); got.Status != state.StatusRunning {
		t.Fatalf("进程还活着，不能当结束了：%s %s", got.Status, got.Summary)
	}
	if err := syscall.Kill(j.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitExit(t, j.PID, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	e.mustRun("tick")
	got := e.jobByTitle(title)
	if got.Status != state.StatusInterrupted || got.SessionID != preset || got.ResumedBy == "" {
		t.Fatalf("%s session=%q by=%q：%s", got.Status, got.SessionID, got.ResumedBy, got.Summary)
	}
	t.Cleanup(func() {
		e.mustRun("pause")
		for _, o := range e.state().Running() {
			e.mustRun("stop", o.ID)
		}
	})
	var calls []codexCall
	waitFor(t, 30*time.Second, "续跑的进程写下调用记录", func() bool {
		calls = codexCalls(e)
		return len(calls) >= 2
	})
	if len(calls) != 2 || calls[1].Resume != preset || !strings.Contains(calls[1].Args, "--session "+preset) || strings.Contains(calls[1].Args, "--session-id") {
		t.Fatalf("%+v", calls)
	}
}

// 做到一半撞限（429 只在事件里，退出码 0）：记中断、冷却，过后同一会话接着做完并合并。
func TestPiLimitThenResumes(t *testing.T) {
	e := piEnv(t, map[string]any{"limit_cooldown": "1s"})
	gateRepo(t, e, "demo", codexItem("ddd", "limitmid"))
	e.mustRun("tick")
	title := "条目 ITEM-ddd"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Status != state.StatusInterrupted || !j.LimitHit || !j.ResumePending || j.SessionID == "" {
		t.Fatalf("%s limit=%v resume=%v session=%q：%s", j.Status, j.LimitHit, j.ResumePending, j.SessionID, j.Summary)
	}
	time.Sleep(1100 * time.Millisecond)
	e.mustRun("resume")
	e.mustRun("tick")
	calls := codexCalls(e)
	if len(calls) != 2 || calls[1].Resume != j.SessionID {
		t.Fatalf("%+v", calls)
	}
	var again *Job // 续跑记成新任务
	for _, o := range e.state().Jobs {
		if o.ResumeOf == j.ID {
			again = o
		}
	}
	if again == nil || again.SessionID != j.SessionID || again.Backend != "pi" {
		t.Fatalf("%+v", again)
	}
	waitExit(t, again.PID, 20*time.Second)
	e.mustRun("tick")
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		for _, o := range e.state().Jobs {
			if o.ID == again.ID && o.Gate != nil {
				_, err := os.Stat(e.gateResult(o))
				return err == nil
			}
		}
		return false
	})
	e.mustRun("tick")
	for _, o := range e.state().Jobs {
		if o.ID == again.ID && (o.Gate == nil || o.Gate.State != state.GateMerged) {
			t.Fatalf("%+v", o.Gate)
		}
	}
}

// 认证失败（401 在 message_end 的 errorMessage 里）：暂停这个后端，auth check 说 ready 也不解除。
func TestPiAuthFailurePausesBackend(t *testing.T) {
	e := piEnv(t, map[string]any{"notify_enabled": true})
	gateRepo(t, e, "demo", codexItem("kkk", "e401"), codexItem("lll", "ok"))
	e.mustRun("run", "demo", "ITEM-kkk")
	title := "条目 ITEM-kkk"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 10*time.Second)
	e.mustRun("tick")
	got := e.jobByTitle(title)
	if got.Status != state.StatusFailed || !got.LimitHit || !strings.Contains(got.Summary, "认证失败") {
		t.Fatalf("%s %v %s", got.Status, got.LimitHit, got.Summary)
	}
	e.mustRun("tick")
	if n := len(codexCalls(e)); n != 1 {
		t.Fatalf("暂停期间不起别的任务：%d", n)
	}
	if err := os.Remove(filepath.Join(e.home, ".agentq", "backend-auth", "pi")); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if n := len(codexCalls(e)); n != 2 {
		t.Fatalf("恢复后要接活：%d", n)
	}
}

// 启动前 auth check 说没凭据：不起任务，告诉用户登录命令。
func TestPiNotLoggedInRefuses(t *testing.T) {
	e := piEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("mmm", "ok"))
	writeFakeMode(t, e, "authmode", "out")
	out, code := e.run("run", "demo")
	if code == 0 || !strings.Contains(out, "pi auth login fakep") {
		t.Fatalf("%d %s", code, out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Errorf("%d", n)
	}
}

// backends.pi.model 写了「提供商/模型」：启动前按这个模型查登录，不查默认来源。
func TestPiAuthChecksConfiguredModel(t *testing.T) {
	e := piEnv(t, map[string]any{"backends": map[string]any{"pi": map[string]any{"path": codexFake(t), "model": "otherp/m1"}}})
	gateRepo(t, e, "demo", codexItem("mmm", "ok"))
	writeFakeMode(t, e, "authmode", "out")
	if out, code := e.run("run", "demo"); code == 0 {
		t.Fatalf("%d %s", code, out)
	}
	calls := authCalls(e)
	if len(calls) == 0 || !strings.Contains(calls[0], "auth check --model otherp/m1 --json") {
		t.Fatalf("%v", calls)
	}
}

// 没确认过：不起任务，写明要设什么。pi 本来就不问，permission_mode=full 只记用户确认过。
func TestPiPermissionMissingRefuses(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "pi", "backends": map[string]any{"pi": map[string]any{"path": codexFake(t)}}})
	gateRepo(t, e, "demo", codexItem("ppp", "ok"))
	out, code := e.run("run", "demo")
	if code == 0 || !strings.Contains(out, "permission_mode=full") {
		t.Fatalf("%d %s", code, out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Errorf("%d", n)
	}
}

// 审查给不出整条 JSON（崩了、只有文字）：判不了，不合并。
func TestPiReviewUnknownDoesNotMerge(t *testing.T) {
	for _, mode := range []string{"bad", "crash"} {
		t.Run(mode, func(t *testing.T) {
			e := piEnv(t, nil)
			dir := gateRepo(t, e, "demo", codexItem("nnn", "ok"))
			before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
			writeFakeMode(t, e, "reviewmode", mode)
			e.mustRun("tick")
			j := e.tickUntilGate("条目 ITEM-nnn")
			if j.Gate == nil || j.Gate.State != state.GateStuck {
				t.Fatalf("判不了要等用户定：%+v", j.Gate)
			}
			if got := testkit.GitT(t, e.home, dir, "log", "--format=%s", before+"..main"); strings.Contains(got, "合并") {
				t.Errorf("%s", got)
			}
		})
	}
}

func piScript(t *testing.T, home, auth string, code int) string {
	t.Helper()
	p := filepath.Join(home, "pi-bin", "pi")
	testkit.WriteInstall(t, p, fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
"--version "*) echo '1.5.1' ;;
"--help "*) echo '--mode --session-id --no-approve' ;;
"auth check") echo '%s'; exit %d ;;
esac
`, auth, code), 0o755)
	testkit.WriteInstall(t, filepath.Join(home, ".pi", "agent", "settings.json"), `{"defaultProvider":"fakep"}`+"\n", 0o644)
	return p
}

const piReady = `{"status":"ready","provider":"fakep","authType":"api_key"}`

// 新装选 pi：技能进 ~/.agents/skills，配置写 backend 和路径；doctor 说清能力和关掉的任务。
func TestPiInstallerAndDoctor(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cli := piScript(t, i.Home, piReady, 0)
	o.Claude, o.Backend = cli, "pi"
	var out bytes.Buffer
	i.Out = &out
	mustInstall(t, i, o)
	home := i.Home
	if !exists(filepath.Join(home, ".agents", "skills", "agent-task", "SKILL.md")) || exists(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("技能要投影到 pi 读的 ~/.agents/skills，不碰 ~/.claude")
	}
	cfg, _, err := setup.ReadInstallConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "pi" || cfg.BackendPath("pi") != cli || cfg.Model != "" || cfg.Effort != "" {
		t.Fatalf("%+v", cfg)
	}
	out.Reset()
	if err := i.Doctor(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	testkit.AssertContains(t, out.String(), "后端：pi", "pi 1.5.1", "美元上限：无", "只读和无工具：无", "命令行进程标记：无", "审查结论的格式", "关掉的任务：", "大方向商量", "permission_mode=full")
	if strings.Contains(out.String(), "待做和 new（要合并）") {
		t.Errorf("pi 能做要合并的任务：\n%s", out.String())
	}
}

// agentq start：同意前如实说 pi 没有权限确认和沙箱，同意后写 full。
func TestPiStartConsent(t *testing.T) {
	a, i, _, out := onboardApp(t)
	cli := piScript(t, i.Home, piReady, 0)
	testkit.WriteInstall(t, a.ConfigPath(), `{"backend":"pi","projects_root":"`+a.ProjectsRoot()+`","backends":{"pi":{"path":"`+cli+`"}}}`+"\n", 0o600)
	mustInstall(t, i, setup.SetupOptions{Scope: "full", Yes: true})
	a2, err := newApp(i.Home, out)
	if err != nil {
		t.Fatal(err)
	}
	a2.Service.Launchctl = a.Service.Launchctl
	out.Reset()
	if err := a2.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i, Interactive: false}); err == nil {
		t.Fatalf("不是终端、没 --accept-bypass 不能放开：%s", out.String())
	}
	testkit.AssertContains(t, out.String(), "后端是 pi", "没有权限确认", "没有沙箱")
	out.Reset()
	if err := a2.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if raw := testkit.ReadFile(t, a2.ConfigPath()); !strings.Contains(raw, `"permission_mode": "full"`) {
		t.Errorf("%s", raw)
	}
}
