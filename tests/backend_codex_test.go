package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentq/internal/chat"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/onboard"
	"agentq/internal/setup"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 假 codex：stress_fake_test.go 编译的那个真可执行文件（ps -E 看得到环境，和真 Codex 一样），按 codex exec 被调时写事件流、从标准输入读提示词。

var (
	codexFakeOnce sync.Once
	codexFakePath string
	codexFakeErr  error
)

func codexFake(t *testing.T) string {
	t.Helper()
	codexFakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agentq-codex-fake-")
		if err != nil {
			codexFakeErr = err
			return
		}
		os.WriteFile(filepath.Join(dir, "main.go"), []byte(stressFakeSrc), 0o644)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644)
		codexFakePath = filepath.Join(dir, "codex")
		c := exec.Command("go", "build", "-o", codexFakePath, ".")
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			codexFakeErr = fmt.Errorf("编译假 codex：%v\n%s", err, out)
		}
	})
	if codexFakeErr != nil {
		t.Fatal(codexFakeErr)
	}
	return codexFakePath
}

type codexCall struct {
	Ev     string   `json:"ev"`
	Kind   string   `json:"kind"`
	Tok    string   `json:"tok"`
	Mode   string   `json:"mode"`
	Resume string   `json:"resume"`
	Job    string   `json:"job"`
	Args   string   `json:"args"`
	Head   string   `json:"prompt_head"`
	Login  []string `json:"login_env"`
	Pid    int      `json:"pid"`
}

func codexCalls(e *testEnv) []codexCall {
	b, _ := os.ReadFile(filepath.Join(e.home, "fake", "calls.log"))
	var out []codexCall
	for _, line := range strings.Split(string(b), "\n") {
		var c codexCall
		if json.Unmarshal([]byte(line), &c) == nil && c.Ev == "start" {
			out = append(out, c)
		}
	}
	return out
}

func codexEnv(t *testing.T, extra map[string]any) *testEnv {
	t.Helper()
	cfg := map[string]any{"backend": "codex", "permission_mode": "danger-full-access",
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}}
	for k, v := range extra {
		cfg[k] = v
	}
	return newEnv(t, cfg)
}

func codexItem(tok, mode string) string {
	return fmt.Sprintf("- [ ] 条目 ITEM-%s —— MODE=%s 验收：看着办", tok, mode)
}

func writeFakeMode(t *testing.T, e *testEnv, name, mode string) {
	t.Helper()
	os.MkdirAll(filepath.Join(e.home, "fake"), 0o755)
	if err := os.WriteFile(filepath.Join(e.home, "fake", name), []byte(mode+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 完整闭环：tick 启动、提示词走标准输入、分支提交、ready.json、测试门、审查门（同样是 Codex）、引擎合并。
func TestCodexFullLoopMerges(t *testing.T) {
	e := codexEnv(t, nil)
	e.extra = append(e.extra, "CODEX_HOME="+filepath.Join(e.home, "ch"), "CODEX_API_KEY=k1", "OPENAI_API_KEY=k2", "ANTHROPIC_API_KEY=k3", "CLAUDE_CODE_OAUTH_TOKEN=k4", "CLAUDE_CONFIG_DIR="+filepath.Join(e.home, ".claude"))
	dir := gateRepo(t, e, "demo", codexItem("aaa", "ok"))
	before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	e.mustRun("tick")
	title := "条目 ITEM-aaa"
	j := e.jobByTitle(title)
	if j.Backend != "codex" {
		t.Fatalf("%+v", j)
	}
	if slices.Contains(j.Args, "--name") || strings.Contains(strings.Join(j.Args, " "), "条目") {
		t.Errorf("提示词和标题不进参数：%q", j.Args)
	}
	j = e.tickUntilGate(title)
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	if got := testkit.GitT(t, e.home, dir, "rev-list", "--count", "--merges", before+"..main"); got != "1" {
		t.Errorf("合并了 %s 次", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "work-ITEM-aaa.txt")); err != nil {
		t.Error("任务的改动没进主目录")
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] 条目 ITEM-aaa") {
		t.Errorf("%s", b)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "jobs", j.ID, "stdin.txt")); err == nil {
		t.Error("提示词文件 CLI 起来之后要删掉")
	}
	// 任务和审查各花一次，审查的用量算在任务头上；只有 token 没有金额
	if j.Usage == nil || !j.Usage.CostUnknown || j.Usage.Input != 120 || j.Usage.CacheRead != 80 || j.Usage.Output != 20 {
		t.Errorf("用量 %+v", j.Usage)
	}
	calls := codexCalls(e)
	if len(calls) != 2 || calls[0].Kind != "task" || calls[1].Kind != "review" {
		t.Fatalf("%+v", calls)
	}
	task, review := calls[0], calls[1]
	if task.Job != j.ID || !strings.Contains(task.Head, "~/.agents/skills/agent-task/SKILL.md") || strings.Contains(task.Head, "~/.claude/skills") {
		t.Errorf("提示词要点 Codex 读得到的技能路径：%+v", task)
	}
	if !strings.Contains(task.Args, `sandbox_mode="danger-full-access"`) || strings.Contains(task.Args, "opus") || strings.Contains(task.Args, " -m ") || strings.Contains(task.Args, "model_reasoning_effort") {
		t.Errorf("任务的参数：%s", task.Args)
	}
	if !strings.Contains(review.Args, `sandbox_mode="workspace-write"`) || !strings.Contains(review.Args, "--output-schema") || !strings.Contains(review.Args, `model_reasoning_effort="medium"`) {
		t.Errorf("审查的参数：%s", review.Args)
	}
	for _, c := range calls {
		if !slices.Equal(c.Login, []string{"CODEX_HOME"}) {
			t.Errorf("只传 CODEX_HOME，别家的登录变量和 API key 都不传：%v", c.Login)
		}
	}
}

// 撤修复验证：把默认后端的 opus 传给项目后端的问题。默认 Claude（model=opus），项目换成 Codex。
func TestCodexProjectBackendGetsNoClaudeDefaults(t *testing.T) {
	e := newEnv(t, map[string]any{"model": "opus", "effort": "high", "permission_mode": "bypassPermissions",
		"project_backends": map[string]any{"demo": "codex"},
		"backends":         map[string]any{"codex": map[string]any{"path": codexFake(t), "model": "gpt-x", "permission": "danger-full-access"}}})
	gateRepo(t, e, "demo", codexItem("bbb", "ok"))
	e.mustRun("tick")
	j := e.jobByTitle("条目 ITEM-bbb")
	waitExit(t, j.PID, 20*time.Second)
	calls := codexCalls(e)
	if len(calls) != 1 {
		t.Fatalf("%+v", calls)
	}
	if strings.Contains(calls[0].Args, "opus") || strings.Contains(calls[0].Args, "bypassPermissions") || strings.Contains(calls[0].Args, "high") {
		t.Errorf("Claude 的配置串给了 Codex：%s", calls[0].Args)
	}
	if !strings.Contains(calls[0].Args, "-m gpt-x") || !strings.Contains(calls[0].Args, `sandbox_mode="danger-full-access"`) {
		t.Errorf("backends.codex 配的要传：%s", calls[0].Args)
	}
}

// 手写的配置只写了权限：模型和强度什么都不传，跟 Codex 自己的配置走。
func TestCodexHandWrittenConfigPassesNothing(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "codex", "permission_mode": "danger-full-access", "backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}})
	gateRepo(t, e, "demo", codexItem("ccc", "ok"))
	e.mustRun("run", "demo")
	j := e.jobByTitle("条目 ITEM-ccc")
	waitExit(t, j.PID, 20*time.Second)
	calls := codexCalls(e)
	if len(calls) != 1 {
		t.Fatalf("%+v", calls)
	}
	for _, bad := range []string{"opus", "sonnet", "high", "bypassPermissions", " -m ", "model_reasoning_effort"} {
		if strings.Contains(calls[0].Args, bad) {
			t.Errorf("不该传 %s：%s", bad, calls[0].Args)
		}
	}
}

// 默认 Codex 没配权限：不起任务，不计连环失败，tick 日志只记一次，status 和 run 都写明要设什么。
func TestCodexPermissionMissingRefuses(t *testing.T) {
	for _, mode := range []any{nil, "default"} {
		cfg := map[string]any{"backend": "codex", "backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}}
		if mode != nil {
			cfg["permission_mode"] = mode
		}
		e := newEnv(t, cfg)
		gateRepo(t, e, "demo", codexItem("ppp", "ok"))
		out, code := e.run("run", "demo")
		if code == 0 || !strings.Contains(out, "permission_mode=danger-full-access") || !strings.Contains(out, "agentq start") {
			t.Fatalf("agentq run 要拒绝并写明：%d %s", code, out)
		}
		ents, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "jobs"))
		if n := len(codexCalls(e)); n != 0 || len(ents) != 0 {
			t.Fatalf("一个进程都不起，不留任务目录：%d %v", n, ents)
		}
		const line = "后端 codex 的权限没放开"
		seen := 0
		for range 3 {
			seen += strings.Count(e.mustRun("tick"), line)
		}
		if seen != 1 {
			t.Errorf("tick 日志同样的原因只记一次：%d", seen)
		}
		st := e.state()
		if len(st.Jobs) != 0 || !st.BreakerUntil.IsZero() || st.FailStreak != 0 {
			t.Errorf("不计连环失败：%+v", st)
		}
		if out := e.mustRun("status"); !strings.Contains(out, "permission_mode=danger-full-access") {
			t.Errorf("status 要写明：\n%s", out)
		}
	}
}

// 按项目用 Codex 没配权限：这个项目不起，别的项目照常；status 写的是 backends.codex.permission；配好就接上。
func TestCodexProjectPermissionMissing(t *testing.T) {
	fake := codexFake(t)
	cfg := map[string]any{"max_parallel": 3, "project_backends": map[string]any{"demo": "codex"}, "backends": map[string]any{"codex": map[string]any{"path": fake}}}
	e := newEnv(t, cfg)
	gateRepo(t, e, "demo", codexItem("qqq", "ok"))
	gateRepo(t, e, "other", "- [ ] 别的活 READYMERGE —— BRANCH=agent/t-o FILE=o.txt")
	const want = "backends.codex.permission=danger-full-access"
	out, code := e.run("run", "demo")
	if code == 0 || !strings.Contains(out, want) {
		t.Fatalf("%d %s", code, out)
	}
	tick := e.mustRun("tick")
	if strings.Count(tick, "后端 codex 的权限没放开") != 1 {
		t.Errorf("%s", tick)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Fatalf("Codex 不该起：%d", n)
	}
	if j := e.jobByTitle("别的活 READYMERGE"); j == nil || j.Backend != "" {
		t.Fatalf("别的项目照常：%+v", j)
	}
	if out := e.mustRun("status"); !strings.Contains(out, want) {
		t.Errorf("status 要写明：\n%s", out)
	}
	if tick := e.mustRun("tick"); strings.Contains(tick, "后端 codex 的权限没放开") {
		t.Errorf("同样的原因不再记：%s", tick)
	}
	cfg["backends"] = map[string]any{"codex": map[string]any{"path": fake, "permission": "danger-full-access"}}
	cfg["projects_root"], cfg["claude_path"], cfg["tick_after_job"], cfg["patrol_enabled"], cfg["notify_enabled"] = e.root, e.fake, false, false, false
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(e.home, ".agentq", "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustRun("run", "demo")
	j := e.jobByTitle("条目 ITEM-qqq")
	waitExit(t, j.PID, 20*time.Second)
	if n := len(codexCalls(e)); n != 1 {
		t.Fatalf("配好就接上：%d", n)
	}
	e.mustRun("pause")
}

// 默认 Codex、项目用 Claude 也一样：Claude 不沿用默认后端的权限，也要配。
func TestCodexDefaultClaudeProjectPermissionMissing(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "codex", "permission_mode": "danger-full-access", "project_backends": map[string]any{"demo": "claude"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}})
	gateRepo(t, e, "demo", "- [ ] 活 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	if out, code := e.run("run", "demo"); code == 0 || !strings.Contains(out, "backends.claude.permission=bypassPermissions") {
		t.Fatalf("%d %s", code, out)
	}
}

// 老配置不受影响：默认 Claude，default 只提示照跑。
func TestPermissionDefaultStillRunsOnClaude(t *testing.T) {
	e := newEnv(t, map[string]any{"permission_mode": "default"})
	gateRepo(t, e, "demo", "- [ ] 活 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	out := e.mustRun("run", "demo")
	if !strings.Contains(out, "已启动任务") {
		t.Fatalf("%s", out)
	}
	waitExit(t, e.jobByTitle("活 READYMERGE").PID, 20*time.Second)
	e.mustRun("pause")
	if out := e.mustRun("status"); strings.Contains(out, "权限没放开") {
		t.Errorf("%s", out)
	}
}

// 默认 Claude、项目 Codex：回复 Codex 任务的通知，Codex 做不到只读，回退普通对话，消息和附件都在。之前路由选了追问，Start 被拒，消息作废、附件被删。
func TestCodexFollowupFallsBackOnProjectBackend(t *testing.T) {
	e, tg := threadEnv(t, map[string]any{"project_backends": map[string]any{"demo": "codex"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t), "permission": "danger-full-access"}}})
	codexHome := filepath.Join(e.home, "ch")
	e.extra = append(e.extra, "CODEX_HOME="+codexHome)
	session := "01a1adf5-1a35-7091-9748-0a21efb18c8d"
	day := filepath.Join(codexHome, "sessions", "2026", "11", "06")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-11-06T07-04-45-"+session+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := testkit.MkProject(t, e.root, "demo", "")
	orig := &Job{ID: "1106-000000-demo", Project: "demo", ProjectDir: dir, Title: "活", SessionID: session, Backend: "codex", Status: state.StatusDone}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	file := filepath.Join(e.home, ".agentq", "inbox", "11-shot.png")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("x"), 0o644)
	s := e.readNotify()
	s.Inbox = []notify.InMsg{{ID: 11, At: time.Now(), Text: "为什么这样", ReplyToID: 900, Files: []string{file}}}
	raw, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), raw, 0o600)
	j := e.tickUntilEnded(func() *Job {
		for _, x := range e.state().Jobs {
			if x.Kind == state.KindChat {
				return x
			}
		}
		return nil
	})
	if j.Backend != "" {
		t.Errorf("回退的对话跑在默认后端：%q", j.Backend)
	}
	for _, x := range e.state().Jobs {
		if x.Kind == state.KindFollowup {
			t.Errorf("Codex 做不到只读，不该追问：%+v", x)
		}
	}
	found := false
	filepath.WalkDir(e.home, func(p string, d os.DirEntry, err error) error {
		found = found || (err == nil && filepath.Base(p) == "11-shot.png")
		return nil
	})
	if !found {
		t.Error("附件不能丢")
	}
	if log := e.fakeLogOf(j); !strings.Contains(log, chat.NoThreadNote) || !strings.Contains(log, "为什么这样") {
		t.Errorf("消息要交给对话任务：%s", log)
	}
	for _, sent := range tg.texts() {
		if strings.Contains(sent, "没能接上") || strings.Contains(sent, "做不到") {
			t.Errorf("不该拒绝：%s", sent)
		}
	}
}

// 反方向：默认 Codex、项目 Claude，回复 Claude 任务的通知能追问就追问。默认后端没登录也不挡，看的是原任务的后端。
func TestClaudeFollowupWorksUnderCodexDefault(t *testing.T) {
	e, _ := threadEnv(t, map[string]any{"backend": "codex", "permission_mode": "danger-full-access", "project_backends": map[string]any{"demo": "claude"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}})
	writeFakeMode(t, e, "authmode", "out")
	dir := testkit.MkProject(t, e.root, "demo", "")
	e.seedSession(threadSession)
	orig := &Job{ID: "1106-000000-demo", Project: "demo", ProjectDir: dir, Title: "活", SessionID: threadSession, Status: state.StatusDone}
	e.seedEnded(orig)
	e.seedThreads(map[int64]notify.Thread{900: notify.JobThread(orig)})
	s := e.readNotify()
	s.Inbox = []notify.InMsg{{ID: 11, At: time.Now(), Text: "为什么 FUPREPLY", ReplyToID: 900}}
	raw, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(e.home, ".agentq", "notify.json"), raw, 0o600)
	j := e.tickUntilEnded(func() *Job {
		for _, x := range e.state().Jobs {
			if x.Kind == state.KindFollowup {
				return x
			}
		}
		return nil
	})
	if j.Backend != "" {
		t.Fatalf("追问跑在原任务的后端：%q", j.Backend)
	}
	if log := e.fakeLogOf(j); !strings.Contains(log, threadSession) {
		t.Errorf("要接原会话：%s", log)
	}
}

func TestCodexPermissionValidatedByBackend(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "codex", "permission_mode": "bypassPermissions"})
	out, code := e.run("status")
	if code == 0 || !strings.Contains(out, "permission_mode") || !strings.Contains(out, "danger-full-access") {
		t.Fatalf("Claude 的值给 Codex 要报错：%d %s", code, out)
	}
	e = newEnv(t, map[string]any{"permission_mode": "danger-full-access"})
	if out, code := e.run("status"); code == 0 || !strings.Contains(out, "bypassPermissions") {
		t.Fatalf("反过来也是：%d %s", code, out)
	}
}

// 做到一半撞限：记中断，冷却过后 exec resume 同一个会话接着做完。
func TestCodexLimitThenResumes(t *testing.T) {
	e := codexEnv(t, map[string]any{"limit_cooldown": "1s"})
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
	if st := e.state(); !st.CooldownUntil.After(j.StartedAt) {
		t.Errorf("要进冷却：%v", st.CooldownUntil)
	}
	session := j.SessionID
	time.Sleep(1100 * time.Millisecond)
	e.mustRun("resume")
	e.mustRun("tick")
	calls := codexCalls(e)
	if len(calls) != 2 || calls[1].Resume != session || !strings.Contains(calls[1].Args, "exec resume "+session) {
		t.Fatalf("%+v", calls)
	}
	var again *Job // 续跑记成新任务
	for _, o := range e.state().Jobs {
		if o.ResumeOf == j.ID {
			again = o
		}
	}
	if again == nil || again.SessionID != session || again.Backend != "codex" {
		t.Fatalf("%+v", again)
	}
	waitExit(t, again.PID, 20*time.Second)
	e.mustRun("tick")
	var last *Job
	for _, o := range e.state().Jobs {
		if o.ID == again.ID {
			last = o
		}
	}
	if last == nil || last.Gate == nil || last.Gate.State != state.GateRunning {
		t.Fatalf("续跑做完要过门：%+v", last)
	}
	waitFor(t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(last))
		return err == nil
	})
	e.mustRun("tick")
	for _, o := range e.state().Jobs {
		if o.ID == again.ID && o.Gate.State != state.GateMerged {
			t.Fatalf("%+v", o.Gate)
		}
	}
}

// 进程被杀：事件流没有终态，从第一个事件取到的会话 id 还能续跑，不当成成功。
func TestCodexKilledMidwayResumes(t *testing.T) {
	e := codexEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("eee", "claimhang"))
	e.mustRun("run", "demo")
	title := "条目 ITEM-eee"
	j := e.jobByTitle(title)
	waitFor(t, 10*time.Second, "假 codex 写出第一个事件", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.home, ".agentq", "jobs", j.ID, "result.json"))
		return strings.Contains(string(b), "turn.started")
	})
	// 活着的时候认得出：命令行没有标记，靠环境
	e.mustRun("pause")
	e.mustRun("tick")
	if got := e.jobByTitle(title); got.Status != state.StatusRunning {
		t.Fatalf("进程还活着，不能当结束了：%s %s", got.Status, got.Summary)
	}
	if out := e.mustRun("status"); strings.Contains(out, "进程已结束") {
		t.Errorf("%s", out)
	}
	if err := syscall.Kill(j.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitExit(t, j.PID, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	e.mustRun("tick")
	got := e.jobByTitle(title)
	if got.Status != state.StatusInterrupted || got.SessionID == "" || !got.ResumePending {
		t.Fatalf("%s session=%q resume=%v：%s", got.Status, got.SessionID, got.ResumePending, got.Summary)
	}
	if !strings.HasPrefix(got.SessionID, "01a1") {
		t.Errorf("会话 id 从事件流第一行取：%s", got.SessionID)
	}
	// 接着跑：同一会话
	e.mustRun("resume")
	e.mustRun("tick")
	stop := func() {
		e.mustRun("pause")
		for _, o := range e.state().Running() { // 续跑的这个还是 claimhang：停掉，再跑不出结果
			e.mustRun("stop", o.ID)
		}
	}
	t.Cleanup(stop) // 断言失败也要停，不然清理临时目录时还有进程在写
	var calls []codexCall
	waitFor(t, 30*time.Second, "续跑的进程写下调用记录", func() bool { // tick 起完就返回，假 CLI 是另一个进程，慢机器上还没写
		calls = codexCalls(e)
		return len(calls) >= 2
	})
	if len(calls) != 2 || calls[1].Resume != got.SessionID {
		t.Fatalf("%+v", calls)
	}
}

// stop：杀掉真进程，不是别的。
func TestCodexStopKills(t *testing.T) {
	e := codexEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("fff", "hang"))
	e.mustRun("run", "demo")
	j := e.jobByTitle("条目 ITEM-fff")
	time.Sleep(300 * time.Millisecond)
	out := e.mustRun("stop", j.ID)
	if !strings.Contains(out, "已停止") {
		t.Fatalf("%s", out)
	}
	waitExit(t, j.PID, 5*time.Second)
	if got := e.jobByTitle("条目 ITEM-fff"); got.Status != state.StatusKilled {
		t.Fatalf("%s", got.Status)
	}
}

// 超时：杀真进程组，按超时收尾。
func TestCodexTimeoutKills(t *testing.T) {
	e := codexEnv(t, map[string]any{"timeout": "1s"})
	gateRepo(t, e, "demo", codexItem("ggg", "hang"))
	e.mustRun("run", "demo")
	j := e.jobByTitle("条目 ITEM-ggg")
	time.Sleep(1200 * time.Millisecond)
	e.mustRun("pause")
	e.mustRun("tick")
	waitExit(t, j.PID, 5*time.Second)
	if got := e.jobByTitle("条目 ITEM-ggg"); got.Status != state.StatusTimeout {
		t.Fatalf("%s %s", got.Status, got.Summary)
	}
}

// pid 被复用：记录的 pid 现在是别的程序。环境读得到、标记不对，就是结束了，按结果收尾，不杀那个程序。
func TestCodexPIDReusedNotKilled(t *testing.T) {
	e := codexEnv(t, map[string]any{"timeout": "1s"})
	gateRepo(t, e, "demo", codexItem("hhh", "quick"))
	e.mustRun("run", "demo")
	title := "条目 ITEM-hhh"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 10*time.Second)
	other := exec.Command(codexFake(t), "__child") // 不是系统程序，环境读得到，没有这个任务的标记
	other.Env = append(os.Environ(), "HOME="+e.home)
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = other.Process.Kill()
		_ = other.Wait()
	}()
	e.editState(func(st *State) {
		for _, x := range st.Jobs {
			if x.ID == j.ID {
				x.PID = other.Process.Pid
			}
		}
	})
	time.Sleep(1100 * time.Millisecond)
	e.mustRun("pause")
	e.mustRun("tick")
	if got := e.jobByTitle(title); got.Status == state.StatusRunning || got.Status == state.StatusTimeout {
		t.Fatalf("pid 已经不是它了，按结果收尾：%s %s", got.Status, got.Summary)
	}
	if syscall.Kill(other.Process.Pid, 0) != nil {
		t.Fatal("把别的程序杀了")
	}
}

// 认不出（macOS 上系统自带的程序环境读不到，外壳也不在了）：不当结束了重复启动，超时后也不杀，只放开名额。
func TestCodexUnsureNeitherKillsNorDuplicates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Linux 上 /proc 总读得到自己用户的进程环境")
	}
	e := codexEnv(t, map[string]any{"timeout": "2s", "retry_after": "1h"})
	gateRepo(t, e, "demo", codexItem("iii", "quick"), codexItem("jjj", "quick"))
	e.mustRun("run", "demo", "ITEM-iii")
	title := "条目 ITEM-iii"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 10*time.Second)
	sleep := exec.Command("/bin/sleep", "30")
	sleep.Env = append(os.Environ(), "AGENTQ_ENV_PROBE=1")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
	}()
	time.Sleep(100 * time.Millisecond)
	if out, err := exec.Command("ps", "-E", "-ww", "-p", strconv.Itoa(sleep.Process.Pid), "-o", "command=").CombinedOutput(); err == nil && strings.Contains(string(out), "AGENTQ_ENV_PROBE=1") {
		t.Skip("这台机器读得到系统程序的环境（比如 SIP 关着的 CI），测不了读不到的情况")
	}
	e.editState(func(st *State) {
		for _, x := range st.Jobs {
			if x.ID == j.ID {
				x.PID = sleep.Process.Pid
				x.StartedAt = time.Now()
			}
		}
	})
	e.mustRun("pause")
	e.mustRun("tick")
	got := e.jobByTitle(title)
	if got.Status != state.StatusRunning {
		t.Fatalf("认不出就当还在跑，不能收尾后再起一个：%s %s", got.Status, got.Summary)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "认不出") {
		t.Errorf("status 要说清：\n%s", out)
	}
	time.Sleep(2100 * time.Millisecond)
	e.mustRun("tick")
	got = e.jobByTitle(title)
	if got.Status != state.StatusTimeout || !strings.Contains(got.Summary, "没杀") {
		t.Fatalf("%s %s", got.Status, got.Summary)
	}
	if syscall.Kill(sleep.Process.Pid, 0) != nil {
		t.Fatal("认不出的进程不能杀")
	}
}

// 认证失败：401 暂停这个后端。login status 说已登录也不解除：它只看本地有没有凭据（真实令牌失效时还是这么说）。
func TestCodexAuthFailurePausesBackend(t *testing.T) {
	e := codexEnv(t, map[string]any{"notify_enabled": true})
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
	if n := len(codexCalls(e)); n != 1 {
		t.Fatalf("暂停期间不起别的任务：%d", n)
	}
	e.mustRun("tick")
	if n := len(codexCalls(e)); n != 1 {
		t.Fatalf("login status 说已登录也不解除：%d", n)
	}
	// 删掉暂停记录就恢复（重新登录后的做法）
	if err := os.Remove(filepath.Join(e.home, ".agentq", "backend-auth", "codex")); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	if n := len(codexCalls(e)); n != 2 {
		t.Fatalf("恢复后要接活：%d", n)
	}
}

// 启动前没登录：不起任务。
func TestCodexNotLoggedInRefuses(t *testing.T) {
	e := codexEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("mmm", "ok"))
	writeFakeMode(t, e, "authmode", "out")
	out, code := e.run("run", "demo")
	if code == 0 || !strings.Contains(out, "codex login") {
		t.Fatalf("%d %s", code, out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Errorf("%d", n)
	}
}

// 审查判不了（输出坏了、进程崩了、结构化输出被接口拒绝）：门不放行，不合并。
func TestCodexReviewUnknownDoesNotMerge(t *testing.T) {
	for _, mode := range []string{"bad", "crash", "reject", "failbare", "failempty", "faildetail", "errbare"} {
		t.Run(mode, func(t *testing.T) {
			e := codexEnv(t, nil)
			dir := gateRepo(t, e, "demo", codexItem("nnn", "ok"))
			before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
			writeFakeMode(t, e, "reviewmode", mode)
			e.mustRun("tick")
			j := e.tickUntilGate("条目 ITEM-nnn")
			if j.Gate == nil || j.Gate.State == state.GateMerged {
				t.Fatalf("判不了不能合并：%+v", j.Gate)
			}
			if j.Gate.State != state.GateStuck {
				t.Errorf("判不了要等用户定：%+v", j.Gate)
			}
			if got := testkit.GitT(t, e.home, dir, "log", "--format=%s", before+"..main"); strings.Contains(got, "合并") {
				t.Errorf("%s", got)
			}
		})
	}
}

// 审查有证据的问题：退回。
func TestCodexReviewBlockReturns(t *testing.T) {
	e := codexEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("ooo", "ok"))
	writeFakeMode(t, e, "reviewmode", "block")
	e.mustRun("tick")
	j := e.tickUntilGate("条目 ITEM-ooo")
	if j.Gate == nil || j.Gate.State != state.GateReturned {
		t.Fatalf("%+v", j.Gate)
	}
	e.mustRun("pause")
	for _, o := range e.state().Running() { // 退回后续跑的那个跑完再结束，不然它还在写临时目录
		waitExit(t, o.PID, 20*time.Second)
	}
}

// 新装选 Codex：技能进 ~/.agents/skills，不碰 ~/.claude；配置写 backend 和路径；doctor 按 Codex 查，说清能力和关掉的任务。
func TestCodexInstallerProjectsSkills(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cli := codexScript(t, i.Home, "Logged in using ChatGPT", 0)
	o.Claude, o.Backend = cli, "codex"
	var out bytes.Buffer
	i.Out = &out
	mustInstall(t, i, o)
	home := i.Home
	if !exists(filepath.Join(home, ".agents", "skills", "agent-task", "SKILL.md")) || !exists(filepath.Join(home, ".agents", "skills", "check", "SKILL.md")) {
		t.Fatal("技能要投影到 Codex 读的目录")
	}
	if exists(filepath.Join(home, ".claude", "skills")) {
		t.Error("只用 Codex 不碰 ~/.claude/skills")
	}
	cfg, _, err := setup.ReadInstallConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "codex" || cfg.BackendPath("codex") != cli || cfg.Model != "" || cfg.Effort != "" || cfg.PermissionMode != "default" {
		t.Fatalf("%+v", cfg)
	}
	r := mustReceipt(t, i)
	if !slices.Equal(r.SkillDirs, []string{filepath.Join(home, ".agents", "skills")}) {
		t.Fatalf("%v", r.SkillDirs)
	}
	out.Reset()
	if err := i.Doctor(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	testkit.AssertContains(t, out.String(), "后端：codex", "已登录（ChatGPT）", "美元上限：无", "额度查询：无", "命令行进程标记：无", "关掉的任务：", "Router", "主动感知", setup.PermissionDefaultHint, "permission_mode=danger-full-access")
	if strings.Contains(out.String(), "待做和 new（要合并）") {
		t.Errorf("Codex 能做要合并的任务：\n%s", out.String())
	}
	// 卸载和重装走同一个回执
	out.Reset()
	mustInstall(t, i, setup.SetupOptions{Scope: "full", Yes: true})
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if exists(filepath.Join(home, ".agents", "skills", "agent-task")) {
		t.Error("卸载要删掉投影")
	}
	// 没卸载前的老回执（没有 skill_dirs）不受影响由其他安装用例锁着
}

// 已有配置不被 --backend 悄悄改。
func TestCodexInstallerKeepsExistingConfig(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	mustInstall(t, i, o)
	err := i.Setup(setup.SetupOptions{Scope: "full", Yes: true, Backend: "codex"})
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("%v", err)
	}
	if e := i.Setup(setup.SetupOptions{Scope: "full", Yes: true, Backend: "nope"}); e == nil || !strings.Contains(e.Error(), "unknown backend") {
		t.Fatalf("%v", e)
	}
}

// 手改配置换到 Codex：doctor 说技能还没投影，不假装没事。
func TestCodexDoctorReportsMissingSkillDir(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	mustInstall(t, i, o)
	cli := codexScript(t, i.Home, "Logged in using ChatGPT", 0)
	testkit.WriteInstall(t, filepath.Join(i.Home, ".agentq", "config.json"), `{"backend":"codex","backends":{"codex":{"path":"`+cli+`"}}}`+"\n", 0o600)
	var out bytes.Buffer
	i.Out = &out
	if err := i.Doctor(); err == nil || !strings.Contains(err.Error(), ".agents") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// 重新 setup 就好了
	mustInstall(t, i, setup.SetupOptions{Scope: "full", Yes: true})
	if !exists(filepath.Join(i.Home, ".agents", "skills", "agent-task", "SKILL.md")) {
		t.Fatal("没投影")
	}
	if err := i.Doctor(); err != nil {
		t.Fatalf("%v", err)
	}
	if !exists(filepath.Join(i.Home, ".agentq", "config.json")) {
		t.Fatal("配置不能丢")
	}
}

// doctor：项目用 Codex、权限没放开时说清楚；~/.claude 下的检查不拦只用 Codex 的人。
func TestCodexDoctorProjectPermissionNote(t *testing.T) {
	i, o := installFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cli := codexScript(t, i.Home, "Logged in using ChatGPT", 0)
	mustInstall(t, i, o)
	testkit.WriteInstall(t, filepath.Join(i.Home, ".agentq", "config.json"), `{"claude_path":"`+o.Claude+`","project_backends":{"p":"codex"},"backends":{"codex":{"path":"`+cli+`"}}}`+"\n", 0o600)
	mustInstall(t, i, setup.SetupOptions{Scope: "full", Yes: true})
	var out bytes.Buffer
	i.Out = &out
	if err := i.Doctor(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	testkit.AssertContains(t, out.String(), "后端 codex 的权限没放开", "backends.codex.permission=danger-full-access", "project_backends 用到的后端")
	if !exists(filepath.Join(i.Home, ".claude", "skills", "agent-task")) || !exists(filepath.Join(i.Home, ".agents", "skills", "agent-task")) {
		t.Error("两个后端的技能目录都要有")
	}
}

// agentq start：用 Codex 的完全权限，同意前说清代码发给谁。没写权限的手写配置也要问。
func TestCodexStartConsent(t *testing.T) {
	a, i, _, out := onboardApp(t)
	cli := codexScript(t, i.Home, "Logged in using ChatGPT", 0)
	testkit.WriteInstall(t, a.ConfigPath(), `{"backend":"codex","projects_root":"`+a.ProjectsRoot()+`","backends":{"codex":{"path":"`+cli+`"}}}`+"\n", 0o600)
	mustInstall(t, i, setup.SetupOptions{Scope: "full", Yes: true})
	a2, err := newApp(i.Home, out)
	if err != nil {
		t.Fatal(err)
	}
	a2.Service.Launchctl = a.Service.Launchctl
	out.Reset()
	err = a2.Onboard.CmdStart(nil, onboard.StartOptions{Installer: i, Interactive: false})
	if err == nil {
		t.Fatalf("不是终端、没 --accept-bypass 不能放开：%s", out.String())
	}
	testkit.AssertContains(t, out.String(), "danger-full-access", "OpenAI")
	raw := testkit.ReadFile(t, a2.ConfigPath())
	if strings.Contains(raw, `"permission_mode"`) {
		t.Errorf("没同意不改：%s", raw)
	}
	out.Reset()
	if err := a2.Onboard.CmdStart([]string{"--accept-bypass"}, onboard.StartOptions{Installer: i}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if raw := testkit.ReadFile(t, a2.ConfigPath()); !strings.Contains(raw, `"permission_mode": "danger-full-access"`) {
		t.Errorf("%s", raw)
	}
	a3, err := newApp(i.Home, out)
	if err != nil {
		t.Fatal(err)
	}
	if p := a3.Cfg.PermissionProblems(); len(p) != 0 {
		t.Errorf("同意之后不用再手配：%v", p)
	}
}

// 环境层：Codex 用带标记的块，真后端、临时目录。
func TestCodexEnvLayerBlock(t *testing.T) {
	a, i, _, out := onboardApp(t)
	cli := codexScript(t, i.Home, "Logged in using ChatGPT", 0)
	testkit.WriteInstall(t, a.ConfigPath(), `{"backend":"codex","projects_root":"`+a.ProjectsRoot()+`","backends":{"codex":{"path":"`+cli+`"}}}`+"\n", 0o600)
	t.Setenv("CODEX_HOME", "")
	a2, err := newApp(i.Home, out)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(i.Home, ".codex", "AGENTS.md")
	original := "我的 Codex 规则\n没有结尾换行"
	testkit.WriteInstall(t, file, original, 0o600)
	out.Reset()
	if err := a2.Onboard.CmdEnv([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "没装", "你自己的全局规则")
	if err := a2.Onboard.CmdEnv([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	core, err := installAssets.ReadFile("rules/core.md")
	if err != nil {
		t.Fatal(err)
	}
	got := testkit.ReadFile(t, file)
	if !strings.HasPrefix(got, original) || !strings.Contains(got, string(core)) || !strings.Contains(got, "agentq:rules:begin") {
		t.Fatalf("%q", got)
	}
	if exists(filepath.Join(i.Home, ".codex", "rules")) {
		t.Error("Codex 没有规则目录，不该建")
	}
	if err := a2.Onboard.CmdEnv([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if got := testkit.ReadFile(t, file); got != original {
		t.Fatalf("关掉要回到原样：%q", got)
	}
}

// codexScript 是探测和登录用的假 codex（不跑任务）。
func codexScript(t *testing.T, home, login string, code int) string {
	t.Helper()
	p := filepath.Join(home, "codex-bin", "codex")
	testkit.WriteInstall(t, p, fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
"--version "*) echo 'codex-cli 0.166.0' ;;
"exec --help") echo '--json --output-schema resume' ;;
"login status") echo '%s'; exit %d ;;
esac
`, login, code), 0o755)
	return p
}

// 夜班、复盘的开机补跑不看暂停，每次 tick 都会走到：权限没配时原因也只记一次，不重判闲时。
func TestCodexPermissionMissingCatchUpLogsOnce(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "codex", "review_enabled": true, "paused": true,
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}})
	gateRepo(t, e, "demo")
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	const line = "后端 codex 的权限没放开"
	seen := 0
	for i := range 3 {
		out := e.tickAt(base, time.Duration(i)*5*time.Minute)
		seen += strings.Count(out, line)
		if strings.Contains(out, "开机后补跑没有启动") {
			t.Errorf("没配好不算补跑失败：\n%s", out)
		}
	}
	if seen != 1 {
		t.Errorf("同样的原因只记一次：%d", seen)
	}
	ents, _ := os.ReadDir(filepath.Join(e.home, ".agentq", "jobs"))
	if n := len(codexCalls(e)); n != 0 || len(ents) != 0 {
		t.Errorf("一个进程都不起：%d %v", n, ents)
	}
}

// 界面门走任务的后端：假 codex 改 .tsx，截图用 --image 附上，截图目录进沙箱可写目录，结论按 schema 读。不调真实 CLI。
func TestCodexUIGate(t *testing.T) {
	e := codexEnv(t, nil)
	dir := gateRepo(t, e, "demo", codexItem("aaa", "uifile"))
	shot := filepath.Join(e.home, "shot.png")
	writeTestPNG(t, shot, false)
	os.WriteFile(filepath.Join(dir, gate.UIScript), []byte("#!/bin/sh\ncp "+shot+" \"$AGENTQ_ARTIFACTS/home.png\"\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "界面脚本")
	e.mustRun("tick")
	j := e.tickUntilGate("条目 ITEM-aaa")
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	report := e.mustRun("gate", "report", j.ID)
	if !strings.Contains(report, "ui（warn 档）：pass") {
		t.Errorf("界面门要跑到模型部分并通过：\n%s", report)
	}
	var ui *codexCall
	for _, c := range codexCalls(e) {
		if c.Kind == "uireview" {
			c := c
			ui = &c
		}
	}
	if ui == nil {
		t.Fatalf("界面审查要走 Codex：%+v", codexCalls(e))
	}
	shots := filepath.Join(e.home, ".agentq", "gate", j.ID, "ui-artifacts")
	for _, want := range []string{"--image=" + filepath.Join(shots, "home.png"), "writable_roots", "ui-artifacts", "--output-schema", `sandbox_mode="workspace-write"`} {
		if !strings.Contains(ui.Args, want) {
			t.Errorf("参数缺 %q：%s", want, ui.Args)
		}
	}
	if strings.Contains(ui.Args, "--add-dir") || strings.Contains(ui.Args, "opus") {
		t.Errorf("Codex 不走 --add-dir，Claude 的默认模型不串过来：%s", ui.Args)
	}
}

// 权限没配：new 不起进程；第一条退回后也不接手，配好了接着做。
func TestCodexPermissionBlocksFirstVersion(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "codex", "backends": map[string]any{"codex": map[string]any{"path": codexFake(t)}}})
	out, code := e.run("new", "做工具")
	if code == 0 || !strings.Contains(out, "permission") {
		t.Errorf("没配权限 new 不起：%d %s", code, out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Errorf("一个进程都不起：%d", n)
	}
	if n := len(e.state().Jobs); n != 0 {
		t.Errorf("%d", n)
	}

	cfg := func(perm bool) {
		path := filepath.Join(e.home, ".agentq", "config.json")
		var m map[string]any
		if err := json.Unmarshal([]byte(testkit.ReadFile(t, path)), &m); err != nil {
			t.Fatal(err)
		}
		c := m["backends"].(map[string]any)["codex"].(map[string]any)
		delete(c, "permission")
		if perm {
			c["permission"] = "danger-full-access"
		}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg(true)
	e.mustRun("new", "FIRSTSTUCK ITEM-first1 ITEM-second1")
	waitFor(t, 30*time.Second, "第一条过门", func() bool {
		e.mustRun("tick")
		return e.state().Jobs[0].Gate != nil
	})
	j := e.state().Jobs[0]
	cfg(false)
	e.waitGate(j)
	if j = e.state().Jobs[0]; j.Gate.State != state.GateReturned {
		t.Fatalf("%+v", j.Gate)
	}
	if jobs := e.state().Jobs; len(jobs) != 1 {
		t.Fatalf("权限没配不接手：%+v", jobs)
	}
	cfg(true)
	e.mustRun("tick")
	jobs := e.state().Jobs
	if len(jobs) != 2 || !jobs[1].Takeover || jobs[1].NewReport == nil {
		t.Fatalf("配好了接手，还是第一版：%+v", jobs)
	}
	waitExit(t, jobs[1].PID, 15*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
}

// 默认后端是 Claude、这个项目用 Codex：界面审查按 Codex 的配置，Claude 的默认模型不串过来。
func TestCodexUIGateUsesBackendConfig(t *testing.T) {
	e := newEnv(t, map[string]any{"model": "opus", "project_backends": map[string]string{"demo": "codex"},
		"backends": map[string]any{"codex": map[string]any{"path": codexFake(t), "permission": "danger-full-access"}}})
	dir := gateRepo(t, e, "demo", codexItem("aaa", "uifile"))
	shot := filepath.Join(e.home, "shot.png")
	writeTestPNG(t, shot, false)
	os.WriteFile(filepath.Join(dir, gate.UIScript), []byte("#!/bin/sh\ncp "+shot+" \"$AGENTQ_ARTIFACTS/home.png\"\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "界面脚本")
	e.mustRun("tick")
	if j := e.tickUntilGate("条目 ITEM-aaa"); j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j)
	}
	found := false
	for _, c := range codexCalls(e) {
		if c.Kind == "uireview" {
			found = true
			if strings.Contains(c.Args, "opus") {
				t.Errorf("Claude 的默认模型串过来了：%s", c.Args)
			}
		}
	}
	if !found {
		t.Fatalf("界面审查要走 Codex：%+v", codexCalls(e))
	}
}
