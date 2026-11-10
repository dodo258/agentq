package backend

import (
	"strings"
	"testing"
	"time"

	"agentq/internal/textfmt"
)

func TestTransientStatus(t *testing.T) {
	yes := []int{422, 500, 502, 503, 504, 529, 599}
	no := []int{0, 200, 400, 401, 403, 404, 408, 409, 429, 600}
	for _, c := range yes {
		if !TransientStatus(c) {
			t.Errorf("%d 应算瞬时错误", c)
		}
	}
	for _, c := range no {
		if TransientStatus(c) {
			t.Errorf("%d 不应算瞬时错误", c)
		}
	}
}

// 给人看的时间带程序算好的星期。
func TestWhenCN(t *testing.T) {
	cases := map[string]time.Time{
		"10-13 周二 21:00": time.Date(2026, 10, 13, 21, 0, 0, 0, time.Local),
		"10-11 周日 09:05": time.Date(2026, 10, 11, 9, 5, 0, 0, time.Local),
		"01-01 周五 00:00": time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local),
	}
	for want, at := range cases {
		if got := textfmt.WhenCN(at); got != want {
			t.Errorf("%v：%q，应为 %q", at, got, want)
		}
	}
	reset := time.Date(2026, 10, 13, 21, 0, 0, 0, time.Local)
	u := &UsageInfo{Windows: []UsageWindow{{Label: "week", Used: 95, ResetsAt: &reset}}}
	if got := UsageDescribe(u, reset.Add(-48*time.Hour), UsagePolicy{Model: "opus"}); !strings.Contains(got, "（10-13 周二 21:00 重置，后台上限 90%）") {
		t.Errorf("额度说明里的重置时间应带星期：%q", got)
	}
	// 模型跟随 CLI：只限某个模型的窗口不计入（不知道实际用哪个），话要说全
	scoped := &UsageInfo{Windows: []UsageWindow{{Label: "week (Fable)", Scope: "Fable", Used: 100, ResetsAt: &reset}}}
	got := UsageDescribe(scoped, reset.Add(-48*time.Hour), UsagePolicy{})
	if !strings.Contains(got, "任务跟随 CLI 选的模型，不计入") || strings.Contains(got, "任务用 ，") {
		t.Errorf("%q", got)
	}
	if lvl, _, _ := UsageLevel(scoped, reset.Add(-48*time.Hour), UsagePolicy{}); lvl != UsageOK {
		t.Errorf("%d", lvl)
	}
}

func usageAt(used float64, reset *time.Time, scope string) *UsageInfo {
	label := "session"
	if scope != "" {
		label = "week (" + scope + ")"
	}
	return &UsageInfo{Windows: []UsageWindow{{Label: label, Scope: scope, Used: used, ResetsAt: reset}}}
}

func TestUsageLevel(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	reset := now.Add(2 * time.Hour)
	cases := []struct {
		name  string
		u     *UsageInfo
		model string
		at    time.Time
		want  int
	}{
		{"没读过", nil, "opus", now, UsageUnknown},
		{"读失败", &UsageInfo{Err: "看不懂"}, "opus", now, UsageUnknown},
		{"89% 照常", usageAt(89, &reset, ""), "opus", now, UsageOK},
		{"89.9% 照常", usageAt(89.9, &reset, ""), "opus", now, UsageOK},
		{"90% 停止接活", usageAt(90, &reset, ""), "opus", now, UsageStop},
		{"97% 停止接活", usageAt(97, &reset, ""), "opus", now, UsageStop},
		{"98% 连 run 也拒绝", usageAt(98, &reset, ""), "opus", now, UsageRefuse},
		{"100% 没写重置时间", usageAt(100, nil, ""), "opus", now, UsageRefuse},
		{"重置时刻刚到还不算", usageAt(99, &reset, ""), "opus", reset.Add(30 * time.Second), UsageRefuse},
		{"过了重置时刻自动恢复", usageAt(99, &reset, ""), "opus", reset.Add(time.Minute), UsageOK},
		{"只限 Fable 的窗口不管 opus 任务", usageAt(99, &reset, "Fable"), "opus", now, UsageOK},
		{"只限 Fable 的窗口管 Fable 任务", usageAt(99, &reset, "Fable"), "claude-fable-5-1", now, UsageRefuse},
		{"Sonnet only 的写法", usageAt(95, &reset, "Sonnet only"), "sonnet", now, UsageStop},
	}
	for _, c := range cases {
		if got, _, _ := UsageLevel(c.u, c.at, UsagePolicy{Model: c.model}); got != c.want {
			t.Errorf("%s：档位 %d，想要 %d", c.name, got, c.want)
		}
	}

	// 挡住时给出最晚的恢复时刻；有窗口没写重置时间就给零值（等下次读）
	later := now.Add(48 * time.Hour)
	u := &UsageInfo{Windows: []UsageWindow{
		{Label: "session", Used: 95, ResetsAt: &reset},
		{Label: "week", Used: 91, ResetsAt: &later},
		{Label: "week (Fable)", Scope: "Fable", Used: 100},
	}}
	lvl, until, why := UsageLevel(u, now, UsagePolicy{Model: "opus"})
	if lvl != UsageStop || !until.Equal(later.Add(UsageResetSlack)) || why != "会话 95%，后台上限 90%、本周 91%，后台上限 90%" {
		t.Errorf("%d %v %q", lvl, until, why)
	}
	u.Windows[1].ResetsAt = nil
	if _, until, _ := UsageLevel(u, now, UsagePolicy{Model: "opus"}); !until.IsZero() {
		t.Errorf("有窗口没写重置时间时不该给恢复时刻：%v", until)
	}
	// 后台上限：会话、周分开配，没配的当 90
	pol := UsagePolicy{Model: "opus", Session: 70, Week: 95}
	u = &UsageInfo{Windows: []UsageWindow{{Label: "session", Used: 69.9, ResetsAt: &reset}, {Label: "week", Used: 94, ResetsAt: &later}}}
	if lvl, _, _ := UsageLevel(u, now, pol); lvl != UsageOK {
		t.Errorf("都没到上限：%d", lvl)
	}
	u.Windows[0].Used = 70
	if lvl, until, why := UsageLevel(u, now, pol); lvl != UsageStop || !until.Equal(reset.Add(UsageResetSlack)) || why != "会话 70%，后台上限 70%" {
		t.Errorf("会话到 70 停：%d %v %q", lvl, until, why)
	}
	if got := UsageDescribe(u, now, pol); !strings.Contains(got, "会话 70%（"+textfmt.WhenCN(reset)+" 重置，后台上限 70%）") || !strings.Contains(got, "后台上限 95%") {
		t.Errorf("describe 要写上限：%s", got)
	}
	if lvl, _, _ := UsageLevel(u, now, UsagePolicy{Model: "opus", Week: 95}); lvl != UsageOK {
		t.Errorf("会话没配当 90：%d", lvl)
	}
	if note := UsageGateNote(UsageStop, reset, "会话 70%，后台上限 70%"); !strings.Contains(note, "后台上限") || !strings.Contains(note, "agentq run 仍可用") {
		t.Errorf("说明：%s", note)
	}
	note := UsageGateNote(UsageRefuse, reset, "会话 99%")
	if !strings.Contains(note, "agentq run 也拒绝") || !strings.Contains(note, textfmt.WhenCN(reset)+" 重置后自动恢复") {
		t.Errorf("说明：%s", note)
	}
}

func TestBar(t *testing.T) {
	for p, want := range map[float64]string{0: "▱▱▱▱▱▱▱▱▱▱", 1: "▰▱▱▱▱▱▱▱▱▱", 44: "▰▰▰▰▱▱▱▱▱▱", 95: "▰▰▰▰▰▰▰▰▰▰", 100: "▰▰▰▰▰▰▰▰▰▰", 120: "▰▰▰▰▰▰▰▰▰▰"} {
		if got := Bar(p); got != want {
			t.Errorf("%v%%：%s，应为 %s", p, got, want)
		}
	}
}

// 推送里的额度：会限制任务的窗口每个一行、名字对齐；当天写「今天」；到后台上限加 ⚠️；没人用的只限模型窗口不列。
func TestUsageBars(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, loc)
	today, later := now.Add(5*time.Hour), time.Date(2026, 10, 13, 21, 0, 0, 0, loc)
	u := &UsageInfo{Windows: []UsageWindow{
		{Label: "session", Used: 1, ResetsAt: &today},
		{Label: "week", Used: 95, ResetsAt: &later},
		{Label: "week (Fable)", Scope: "Fable", Used: 100, ResetsAt: &later},
		{Label: "week (Sonnet)", Scope: "Sonnet", Used: 44, ResetsAt: &later},
	}}
	got := UsageBars(u, now, UsagePolicy{Model: "opus", Also: []string{"sonnet"}})
	want := []string{
		"会话           ▰▱▱▱▱▱▱▱▱▱    1%  今天 19:00 重置",
		"本周           ▰▰▰▰▰▰▰▰▰▰   95%  10-13 周二 21:00 重置 ⚠️",
		"本周（Sonnet） ▰▰▰▰▱▱▱▱▱▱   44%  10-13 周二 21:00 重置",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("得到：\n%s\n应为：\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := UsageBars(u, now, UsagePolicy{Model: "opus", Session: 1}); !strings.HasSuffix(got[0], " ⚠️") || len(got) != 2 {
		t.Errorf("会话后台上限 1%% 时会话行应加 ⚠️，不用 Sonnet 时不列它：%q", got)
	}
	if got := UsageBars(nil, now, UsagePolicy{}); len(got) != 1 || got[0] != "还没读过" {
		t.Errorf("%q", got)
	}
}
