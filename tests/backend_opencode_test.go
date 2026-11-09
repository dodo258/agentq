package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 这些用例用假 CLI（和 Codex 共用一份，按 run --format json 认）。真实的 opencode 没跑过。

func opencodeEnv(t *testing.T, extra map[string]any) *testEnv {
	t.Helper()
	cfg := map[string]any{"backend": "opencode", "permission_mode": "auto",
		"backends": map[string]any{"opencode": map[string]any{"path": codexFake(t)}}}
	for k, v := range extra {
		cfg[k] = v
	}
	return newEnv(t, cfg)
}

// 完整闭环：tick 启动、提示词走标准输入、分支提交、ready.json、测试门、审查门（同样是 opencode，靠提示词要 JSON）、引擎合并。审查记录对得上提交。
func TestOpencodeFullLoopMerges(t *testing.T) {
	e := opencodeEnv(t, nil)
	e.extra = append(e.extra, "OPENCODE_CONFIG="+filepath.Join(e.home, "oc.json"), "OPENAI_API_KEY=k1", "ANTHROPIC_API_KEY=k2", "CODEX_HOME=/x", "CLAUDE_CODE_OAUTH_TOKEN=k3")
	dir := gateRepo(t, e, "demo", codexItem("aaa", "ok"))
	before := testkit.GitT(t, e.home, dir, "rev-parse", "main")
	e.mustRun("tick")
	title := "条目 ITEM-aaa"
	j := e.jobByTitle(title)
	if j.Backend != "opencode" {
		t.Fatalf("%+v", j)
	}
	if strings.Contains(strings.Join(j.Args, " "), "条目") {
		t.Errorf("提示词和标题不进参数：%q", j.Args)
	}
	j = e.tickUntilGate(title)
	if j.Gate == nil || j.Gate.State != state.GateMerged {
		t.Fatalf("应合并：%+v %s", j.Gate, j.Summary)
	}
	if testkit.GitT(t, e.home, dir, "rev-parse", "main") == before {
		t.Fatal("main 没动")
	}
	var task, review *codexCall
	calls := codexCalls(e)
	for i := range calls {
		switch calls[i].Kind {
		case "task":
			task = &calls[i]
		case "review":
			review = &calls[i]
		}
	}
	if task == nil || review == nil {
		t.Fatalf("任务和审查都要起：%+v", calls)
	}
	if !strings.HasPrefix(task.Args, "run --format json") || !strings.Contains(task.Args, "--auto") || strings.Contains(task.Args, "opus") {
		t.Errorf("%s", task.Args)
	}
	if !strings.Contains(task.Head, "~/.agents/skills/agent-task/SKILL.md") || strings.Contains(task.Head, "~/.claude/skills") {
		t.Errorf("提示词要写这个后端的技能目录：%s", task.Head)
	}
	if !strings.Contains(review.Args, "--auto") || !strings.Contains(review.Args, "--variant medium") || strings.Contains(review.Args, "sonnet") { // 小改动走轻量流程也是 medium
		t.Errorf("%s", review.Args)
	}
	for _, c := range []*codexCall{task, review} {
		if len(c.Login) != 1 || c.Login[0] != "OPENCODE_CONFIG" {
			t.Errorf("只传自己的配置变量，不传别家的和 API Key：%v", c.Login)
		}
	}
	entries, err := os.ReadDir(gate.ReviewsDir(filepath.Join(e.home, ".agentq"), "demo"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("要写审查记录：%v %v", entries, err)
	}
	var rec gate.ReviewRecord
	if err := json.Unmarshal(testkit.ReadBytes(t, filepath.Join(gate.ReviewsDir(filepath.Join(e.home, ".agentq"), "demo"), entries[0].Name())), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Backend != "opencode" || rec.Verdict != "pass" || rec.Commit != j.Gate.Head || rec.Commit == "" {
		t.Errorf("%+v，分支尖 %s", rec, j.Gate.Head)
	}
	if j.Usage == nil || j.Usage.Input == 0 || j.Usage.CostUSD == 0 {
		t.Errorf("用量要记：%+v", j.Usage)
	}
}

// 权限没配不起进程：跑了也改不了工作目录外的东西、读不到技能。只读的不受影响。
func TestOpencodePermissionMissing(t *testing.T) {
	e := newEnv(t, map[string]any{"backend": "opencode", "backends": map[string]any{"opencode": map[string]any{"path": codexFake(t)}}})
	gateRepo(t, e, "demo", codexItem("bbb", "ok"))
	if out, code := e.run("run", "demo"); code == 0 || !strings.Contains(out, "permission_mode=auto") {
		t.Fatalf("%d %s", code, out)
	}
	if n := len(codexCalls(e)); n != 0 {
		t.Fatalf("一个进程都不起：%d", n)
	}
}

// 做到一半撞限：记中断，冷却过后同一个会话接着做。
func TestOpencodeLimitThenResumes(t *testing.T) {
	e := opencodeEnv(t, map[string]any{"limit_cooldown": "1s"})
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
	session := j.SessionID
	e.mustRun("resume")
	time.Sleep(1100 * time.Millisecond)
	waitFor(t, 20*time.Second, "续跑", func() bool {
		e.mustRun("tick")
		for _, c := range codexCalls(e) {
			if strings.Contains(c.Args, "--session="+session) {
				return true
			}
		}
		return false
	})
}

// 进程没走完（没有最后的 step_finish）：不当成功，条目还进行中就算中断，同一会话续跑。
func TestOpencodeNoTerminalIsNotSuccess(t *testing.T) {
	e := opencodeEnv(t, nil)
	gateRepo(t, e, "demo", codexItem("eee", "claimdie"))
	e.mustRun("tick")
	title := "条目 ITEM-eee"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	j = e.jobByTitle(title)
	if j.Status == state.StatusDone {
		t.Fatalf("没有终态不能算成功：%s", j.Summary)
	}
}

// 新装选 opencode：技能进 ~/.agents/skills，不碰 ~/.claude。doctor 说清认证查不了、能力、关掉的任务，不报错。
func TestInstallShOpencode(t *testing.T) {
	t.Parallel()
	k := newInstallKit(t, "claude")
	k.writeBin("opencode", `#!/bin/sh
case "$1" in
--version) echo 1.19.5 ;;
run) echo "--format --session --auto" ;;
esac
`)
	k.serveRelease("v1.0.0")
	k.mustInstall("--backend", "opencode")
	if !exists(filepath.Join(k.home, ".agents", "skills", "agent-task", "SKILL.md")) || exists(filepath.Join(k.home, ".claude", "skills")) {
		t.Fatal("技能应只进 ~/.agents/skills")
	}
	out, code := k.agentq(k.home, "doctor")
	if code != 0 {
		t.Fatalf("doctor 不该报错：%d\n%s", code, out)
	}
	testkit.AssertContains(t, out, "后端：opencode 1.19.5", "认证：查不了", "只读和无工具：无", "拒绝规则：无", "审查结论的格式：无", "引擎审查：有", "Router（")
}
