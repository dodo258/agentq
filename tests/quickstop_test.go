package tests

import (
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/evolve"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// agentq stop 记下来源和停时额度，自我进化据此认「刚启动就被停」：假时钟下亲手派的 7 秒停算、超过 3 分钟不算，不是亲手派的不算。
func TestQuickStopFromCLI(t *testing.T) {
	fastLookup(t)
	a, _ := newTestApp(t)
	a.Cfg.ProjectsRoot, a.Cfg.UsageStopSessionPct = t.TempDir(), 70
	now := time.Now().Truncate(time.Second)
	a.Now = func() time.Time { return now }
	job := func(id string, manual bool, age time.Duration) *Job {
		j := lostJob(a, id)
		j.Manual, j.StartedAt, j.Title = manual, now.Add(-age), "条目 "+id
		return j
	}
	st := &State{Jobs: []*Job{job("1115-150444-alpha", true, 7*time.Second), job("1115-150000-alpha", true, 3*time.Minute+time.Second), job("1115-150100-alpha", false, 10*time.Second)}}
	st.SetUsageOf("", &backend.UsageInfo{CheckedAt: now, Windows: []backend.UsageWindow{{Label: "session", Used: 72}}})
	if err := state.Save(a.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	for _, j := range st.Jobs {
		if err := a.CmdStop(j.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := state.Load(a.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range got.Jobs {
		if j.Status != state.StatusKilled || j.StopVia != state.StopViaCLI || !strings.Contains(j.StopUsage, "到了后台上限") || !strings.Contains(j.StopUsage, "后台上限 70%") {
			t.Fatalf("要记成命令行停、停时到了后台上限：%+v", j)
		}
	}
	if _, err := a.Evolve.Collect(true); err != nil {
		t.Fatal(err)
	}
	sigs := kindSignals(t, a, evolve.KindQuickStop)
	if len(sigs) != 1 || sigs[0].Seen != 1 || !strings.Contains(sigs[0].Evidence[0], "任务 1115-150444-alpha（alpha）") ||
		!strings.Contains(sigs[0].Evidence[0], "启动 7 秒后被停（命令行），停时到了后台上限") {
		t.Fatalf("只有 7 秒那个算：%+v", sigs)
	}
	if stops := kindSignals(t, a, evolve.KindUserStop); len(stops) != 1 || stops[0].Seen != 2 {
		t.Fatalf("另两个照旧是 user-stop：%+v", stops)
	}
}

func kindSignals(t *testing.T, a *App, kind string) []evolve.Signal {
	t.Helper()
	st, err := a.Evolve.LoadSignals()
	if err != nil {
		t.Fatal(err)
	}
	var out []evolve.Signal
	for _, s := range st.Signals {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// 派活和 agent-task 技能：用 agentq 时发现说法和行为对不上，用 agentq add 往 self_project 记一条使用反馈，写清命令、输出、理解、后果，不贴会话、不写隐私，一次会话只记一次。
func TestSkillsUsageFeedback(t *testing.T) {
	for _, name := range []string{"skills/dispatch/SKILL.md", "skills/agent-task/SKILL.md"} {
		b, err := installAssets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		testkit.AssertContains(t, string(b), "说明、输出和实际行为对不上", `agentq add --project <self_project 名> "（使用反馈）`,
			"跑了 <命令>；看到 <输出里那一句>；我理解成 <…>；后果 <…>", "不贴会话原文", "不写用户隐私", "同一件事一次会话只记一次", "没配就不记")
	}
}
