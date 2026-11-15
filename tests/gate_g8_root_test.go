package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

// Telegram 的「再审一次」按钮：写决定、下次 tick 重新跑门。卡片有这个按钮。
func TestGateCardRerun(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, nil)
	dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
	os.WriteFile(filepath.Join(dir, ".agentq", "verify"), []byte("#!/bin/sh\n[ -f \"$HOME/verify-ok\" ] || exit 1\n"), 0o755)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "门")
	e.mustRun("tick")
	title := "修东西 READYMERGE"
	j := e.jobByTitle(title)
	waitExit(t, j.PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	j = e.jobByTitle(title)
	e.waitGate(j)
	// 第一轮没过是退回。把轮数改成用完了，排回去再过一次，就卡住等用户定
	e.editState(func(st *State) {
		for _, x := range st.Jobs {
			if x.Title == title && x.Gate != nil {
				x.Gate.Round, x.Gate.State, x.ResumePending = 2, state.GateQueued, false
			}
		}
	})
	e.mustRun("tick")
	e.waitGate(e.jobByTitle(title))
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	btns := keyboardOf(t, tg.sentLastWith("门没过"))
	if len(btns) != 4 || !strings.Contains(btns[3].Text, "再审一次") {
		t.Fatalf("%+v", btns)
	}
	os.WriteFile(filepath.Join(e.home, "verify-ok"), nil, 0o644)
	tg.click(50, ownerChat, 1000, btns[3].Data)
	e.mustRun("tick") // 收到按钮
	e.mustRun("tick") // 落决定，起门
	e.waitGate(e.jobByTitle(title))
	j = e.jobByTitle(title)
	if j.Gate.State != state.GateMerged || j.Gate.Round != 2 {
		t.Fatalf("再审过了就合并，不算退回轮数：%+v", j.Gate)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "- [x] 修东西 READYMERGE") || strings.Contains(b, "（卡住") {
		t.Errorf("%s", b)
	}
	if out := e.mustRun("stats"); !strings.Contains(out, "按门（") || !strings.Contains(out, "再审 1 次") {
		t.Errorf("%s", out)
	}
	if !strings.Contains(e.nightly(), "门：用户再审") {
		t.Errorf("%s", e.nightly())
	}
}

// Telegram「状态」：第一版卡住和等商量都写。
func TestDigestFirstStuckAndDiscuss(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, map[string]any{"discuss_enabled": false})
	e.mustRun("new", "NEWFIRSTSTUCK 做个工具")
	waitExit(t, e.state().Jobs[0].PID, 20*time.Second)
	e.mustRun("pause")
	e.mustRun("tick")
	j := e.state().Jobs[0]
	e.waitGate(j)
	e.mustRun("tick")
	before := len(tg.texts())
	tg.msg(60, ownerChat, 60, "状态", "")
	e.mustRun("tick")
	var reply string
	for _, x := range tg.texts()[before:] {
		if strings.Contains(x, "状态") {
			reply = x
		}
	}
	if !strings.Contains(reply, "第一版卡住") || !strings.Contains(reply, "第一版还没合并") || !strings.Contains(reply, "agentq status") {
		t.Errorf("状态摘要要说第一版卡住：\n%s", reply)
	}
	if strings.Contains(reply, "重做：去掉") {
		t.Errorf("解法留给 agentq status：\n%s", reply)
	}
}

// 早报带门的一节：放行的审计行、关着的门。
func TestMorningGateSection(t *testing.T) {
	t.Parallel()
	e, tg := discussEnv(t, map[string]any{"paused": true})
	dir := gateRepo(t, e, "demo")
	os.WriteFile(filepath.Join(dir, ".agentq", "gates.json"), []byte(`{"levels":{"ui":"off"}}`), 0o644)
	testkit.GitT(t, e.home, dir, "add", "-A")
	testkit.GitT(t, e.home, dir, "commit", "-q", "-m", "关界面门")
	now := time.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	os.MkdirAll(filepath.Join(e.home, ".agentq", "gate"), 0o700)
	os.WriteFile(filepath.Join(e.home, ".agentq", "gate", "audit.log"), []byte(noon.Add(-time.Hour).Format(time.RFC3339)+" 放行 1106-x demo「做事」分支 a 提交 b：急着上\n"), 0o600)
	report := filepath.Join(e.home, ".claude", "nightly", noon.Format("2006-01-02")+".md")
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("# 早报\n"), 0o644)
	old := noon.Add(-time.Hour)
	os.Chtimes(report, old, old)
	e.mustRun("tick")
	msg := textfmt.HTMLToPlain(tg.sentLastWith("早报").Get("text"))
	for _, want := range []string{"用户放行 1106-x", "关着的门：demo　ui"} {
		if !strings.Contains(msg, want) {
			t.Errorf("早报缺 %q：\n%s", want, msg)
		}
	}
}

// 审查进程撞上 Cloudflare 拦截页（真实 claude 的 result 行，任务 1113-180354-agentq 那种）：真二进制、真 Claude 后端解析。
// 第一次 403、之后正常：排回去按退避重审，最后按审查结论合并，没标卡住；一直 403：退避用完才卡住，说明是上游错误。
func TestIntegrationReviewGatewayChallenge(t *testing.T) {
	const cf = `{"type":"result","subtype":"success","is_error":true,"api_error_status":403,"num_turns":1,"result":"Failed to authenticate. API Error: 403 Attention Required! | Cloudflare","session_id":"s-rv"}` + "\n"
	title := "修东西 READYMERGE"
	setup := func(t *testing.T) (*testEnv, string, *Job) {
		e := newEnv(t, nil)
		dir := gateRepo(t, e, "demo", "- [ ] 修东西 READYMERGE —— BRANCH=agent/t-a FILE=a.txt")
		os.WriteFile(filepath.Join(e.home, "fake-review.json"), []byte(cf), 0o644)
		e.mustRun("tick")
		j := e.tickUntilGate(title)
		if j.Gate == nil || j.Gate.State != state.GateQueued || j.Gate.Upstream != 1 || j.Gate.Round != 0 {
			t.Fatalf("撞上挑战页要排回去退避：%+v %s", j.Gate, j.Summary)
		}
		if b := e.backlogOf(dir); strings.Contains(b, "（卡住") {
			t.Fatalf("没用完退避不标卡住：%s", b)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".agentq", "backend-auth", "claude")); err == nil {
			t.Error("挑战页不是认证失败，不暂停后端")
		}
		return e, dir, j
	}
	// after：把之后的 agentq 拨到现在之后 d，再 tick 起门、等结果、收结果。
	after := func(e *testEnv, j *Job, d time.Duration) *Job {
		e.extra = append(e.extra, "AGENTQ_TEST_NOW="+time.Now().Add(d).Format(time.RFC3339))
		e.mustRun("tick")
		e.waitGate(e.jobByTitle(title))
		return e.jobByTitle(title)
	}
	t.Run("恢复", func(t *testing.T) {
		t.Parallel()
		e, dir, j := setup(t)
		os.Remove(filepath.Join(e.home, "fake-review.json")) // 上游好了：审查通过
		j = after(e, j, 16*time.Minute)
		if j.Gate.State != state.GateMerged || j.Gate.Upstream != 0 {
			t.Fatalf("按审查结论合并：%+v %s", j.Gate, j.Summary)
		}
		if b := e.backlogOf(dir); !strings.Contains(b, "- [x] 修东西 READYMERGE") || strings.Contains(b, "（卡住") {
			t.Errorf("%s", b)
		}
	})
	t.Run("一直403", func(t *testing.T) {
		t.Parallel()
		e, dir, j := setup(t)
		j = after(e, j, 16*time.Minute)
		if j.Gate.State != state.GateQueued || j.Gate.Upstream != 2 {
			t.Fatalf("第二次还在退避：%+v %s", j.Gate, j.Summary)
		}
		j = after(e, j, 16*time.Minute+61*time.Minute)
		if j.Gate.State != state.GateStuck || j.Gate.Round != 0 {
			t.Fatalf("退避用完才卡住：%+v %s", j.Gate, j.Summary)
		}
		if out := e.mustRun("gate", "report", j.ID); !strings.Contains(out, "上游错误") || !strings.Contains(out, "网关拦截页") {
			t.Errorf("报告要说明是上游错误：%s", out)
		}
		if b := e.backlogOf(dir); !strings.Contains(b, "（卡住") {
			t.Errorf("%s", b)
		}
	})
}
