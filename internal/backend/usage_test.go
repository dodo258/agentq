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
	if got := UsageDescribe(u, reset.Add(-48*time.Hour), "opus"); !strings.Contains(got, "（10-13 周二 21:00 重置）") {
		t.Errorf("额度说明里的重置时间应带星期：%q", got)
	}
	// 模型跟随 CLI：只限某个模型的窗口不计入（不知道实际用哪个），话要说全
	scoped := &UsageInfo{Windows: []UsageWindow{{Label: "week (Fable)", Scope: "Fable", Used: 100, ResetsAt: &reset}}}
	got := UsageDescribe(scoped, reset.Add(-48*time.Hour), "")
	if !strings.Contains(got, "任务跟随 CLI 选的模型，不计入") || strings.Contains(got, "任务用 ，") {
		t.Errorf("%q", got)
	}
	if lvl, _, _ := UsageLevel(scoped, reset.Add(-48*time.Hour), ""); lvl != UsageOK {
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
		if got, _, _ := UsageLevel(c.u, c.at, c.model); got != c.want {
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
	lvl, until, why := UsageLevel(u, now, "opus")
	if lvl != UsageStop || !until.Equal(later.Add(UsageResetSlack)) || why != "会话 95%、本周 91%" {
		t.Errorf("%d %v %q", lvl, until, why)
	}
	u.Windows[1].ResetsAt = nil
	if _, until, _ := UsageLevel(u, now, "opus"); !until.IsZero() {
		t.Errorf("有窗口没写重置时间时不该给恢复时刻：%v", until)
	}
	note := UsageGateNote(UsageRefuse, reset, "会话 99%")
	if !strings.Contains(note, "agentq run 也拒绝") || !strings.Contains(note, textfmt.WhenCN(reset)+" 重置后自动恢复") {
		t.Errorf("说明：%s", note)
	}
}
