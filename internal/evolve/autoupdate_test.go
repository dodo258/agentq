package evolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// autoRig：发行版 v1.0.0，安装引擎装的，打开了自动更新；上次看到发布上最新是 v1.1.0。
func newAutoRig(t *testing.T) (*sigRig, *state.State) {
	t.Helper()
	r := newSigRig(t, func(c *config.Config) { c.AutoUpdate = true })
	if err := os.MkdirAll(filepath.Dir(r.s.receiptPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.s.receiptPath(), []byte(`{"version":1,"generation":"/g/gen-1"}`), 0o600)
	r.s.Version = "v1.0.0"
	if err := r.s.withAuto(func(a *autoState) {
		a.CheckedAt, a.Latest, a.Result = r.now.Add(-time.Hour), "v1.1.0", "发布上最新是 v1.1.0"
	}); err != nil {
		t.Fatal(err)
	}
	return r, &state.State{}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.1.0", "v1.0.0", 1, true},
		{"v1.0.0", "v1.0.0", 0, true},
		{"v0.9.12", "v0.10.0", -1, true},
		{"1.2", "v1.2.0", 0, true},
		{"v1.0.0", "v1.0.0-rc.1", 1, true},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", 1, true},
		{"v1.0.0+abc", "v1.0.0", 0, true},
		{"latest", "v1.0.0", 0, false},
		{"v1.0.0", "dev-abc1234", 0, false},
		{"v1.2.3.beta", "v1.0.0", 0, false},
		{"", "v1.0.0", 0, false},
	}
	for _, c := range cases {
		if got, ok := compareVersions(c.a, c.b); got != c.want || ok != c.ok {
			t.Errorf("compareVersions(%q, %q) = %d %v，应为 %d %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// 有更新的版本、归它管：要装那个版本。
func TestReleasePlanUpgrade(t *testing.T) {
	r, st := newAutoRig(t)
	if pl := r.s.plan(t.Context(), st, r.now); pl.Action != EvUpgrade || pl.Release != "v1.1.0" || pl.Commit != "" || pl.Drill {
		t.Fatalf("要装 v1.1.0：%+v", pl)
	}
	r.s.appendUpgrade(UpgradeEvent{At: r.now.Add(-7 * time.Hour), Kind: EvFail, From: "v1.0.0", To: "v1.1.0", Release: "v1.1.0"})
	if pl := r.s.plan(t.Context(), st, r.now); pl.Action != EvUpgrade {
		t.Fatalf("没装成 6 小时以上再试：%+v", pl)
	}
}

// 不装的情况：关着、暂停、配了 self_project、源码构建、不是安装引擎装的、还没看过、已是最新、发布上的更旧、认不出版本号、回滚过、刚没装成。
func TestReleasePlanSkips(t *testing.T) {
	cases := map[string]func(r *sigRig){
		"关着":             func(r *sigRig) { r.s.Cfg.AutoUpdate = false },
		"暂停":             func(r *sigRig) { r.s.Cfg.Paused = true },
		"配了self_project": func(r *sigRig) { r.s.Cfg.SelfProject = "agentq" },
		"源码构建":           func(r *sigRig) { r.s.Version = "dev-abc1234" },
		"本地构建":           func(r *sigRig) { r.s.Version = "dev" },
		"没有安装记录":         func(r *sigRig) { os.Remove(r.s.receiptPath()) },
		"还没看过":           func(r *sigRig) { os.Remove(r.s.autoPath()) },
		"已是最新":           func(r *sigRig) { r.s.Version = "v1.1.0" },
		"发布上的更旧":         func(r *sigRig) { r.s.Version = "v2.0.0" },
		"认不出": func(r *sigRig) {
			r.s.withAuto(func(a *autoState) { a.Latest = "nightly" })
		},
		"回滚过": func(r *sigRig) {
			r.s.appendUpgrade(UpgradeEvent{At: r.now.Add(-48 * time.Hour), Kind: EvRollback, From: "v1.1.0", To: "v1.0.0", Release: "v1.1.0"})
		},
		"刚没装成": func(r *sigRig) {
			r.s.appendUpgrade(UpgradeEvent{At: r.now.Add(-time.Hour), Kind: EvFail, From: "v1.0.0", To: "v1.1.0", Release: "v1.1.0"})
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			r, st := newAutoRig(t)
			edit(r)
			if pl := r.s.plan(t.Context(), st, r.now); pl.Action != "" {
				t.Fatalf("不该装：%+v", pl)
			}
		})
	}
	// 回滚过的版本挡住的只是它自己：有更新的版本照装
	r, st := newAutoRig(t)
	r.s.appendUpgrade(UpgradeEvent{At: r.now.Add(-48 * time.Hour), Kind: EvRollback, From: "v1.1.0", To: "v1.0.0", Release: "v1.1.0"})
	r.s.withAuto(func(a *autoState) { a.Latest = "v1.2.0" })
	if pl := r.s.plan(t.Context(), st, r.now); pl.Action != EvUpgrade || pl.Release != "v1.2.0" {
		t.Fatalf("更新的版本照装：%+v", pl)
	}
}

// 装上后观察期：变差回滚（带着版本号，不写待做的那条路由 apply 走），没变差记「留下」。
func TestReleaseObservation(t *testing.T) {
	setup := func(t *testing.T) (*sigRig, *state.State, time.Time) {
		r, st := newAutoRig(t)
		at := r.now.Add(-5 * time.Hour)
		r.s.Version = "v1.1.0"
		for i := range 5 {
			end := at.Add(-time.Duration(10-i) * time.Hour)
			st.Jobs = append(st.Jobs, &state.Job{ID: "pre" + string(rune('a'+i)), Project: "web", Status: state.StatusDone, StartedAt: end.Add(-10 * time.Minute), EndedAt: &end})
		}
		r.s.appendUpgrade(UpgradeEvent{At: at, Kind: EvUpgrade, From: "v1.0.0", To: "v1.1.0", Release: "v1.1.0"})
		return r, st, at
	}
	job := func(st *state.State, id string, at time.Time, status string) {
		end := at.Add(10 * time.Minute)
		st.Jobs = append(st.Jobs, &state.Job{ID: id, Project: "web", Status: status, StartedAt: at, EndedAt: &end})
	}
	t.Run("变差回滚", func(t *testing.T) {
		r, st, at := setup(t)
		job(st, "p1", at.Add(time.Hour), state.StatusFailed)
		job(st, "p2", at.Add(2*time.Hour), state.StatusTimeout)
		job(st, "p3", at.Add(3*time.Hour), state.StatusDone)
		if pl := r.s.plan(t.Context(), st, r.now); pl.Action != EvRollback || pl.Release != "v1.1.0" || pl.Commit != "" {
			t.Fatalf("要回滚 v1.1.0：%+v", pl)
		}
	})
	t.Run("没变差留下", func(t *testing.T) {
		r, st, at := setup(t)
		for i, id := range []string{"p1", "p2", "p3"} {
			job(st, id, at.Add(time.Duration(i+1)*time.Hour), state.StatusDone)
		}
		if pl := r.s.plan(t.Context(), st, r.now); pl.Action != "" {
			t.Fatalf("不该动：%+v", pl)
		}
		evs, _ := r.s.LoadUpgrades()
		if last := evs[len(evs)-1]; last.Kind != EvKept || last.Release != "v1.1.0" {
			t.Fatalf("要记留下：%+v", last)
		}
	})
}

// 没装成：同一个版本只推一次，别的版本照推。
func TestAlertReleaseOnce(t *testing.T) {
	r, _ := newAutoRig(t)
	notifyPath := filepath.Join(r.s.DataDir(), "notify.json")
	r.s.alertRelease("v1.1.0", "自动更新：装 v1.1.0 没成")
	if q := testkit.ReadFile(t, notifyPath); !strings.Contains(q, "装 v1.1.0 没成") {
		t.Fatalf("第一次要推：\n%s", q)
	}
	os.Remove(notifyPath) // 推出去了
	r.now = r.now.Add(24 * time.Hour)
	r.s.alertRelease("v1.1.0", "自动更新：装 v1.1.0 没成")
	if _, err := os.Stat(notifyPath); err == nil && strings.Contains(testkit.ReadFile(t, notifyPath), "v1.1.0") {
		t.Fatal("同一个版本第二天不再推")
	}
	r.s.alertRelease("v1.2.0", "自动更新：装 v1.2.0 没成")
	if q := testkit.ReadFile(t, notifyPath); !strings.Contains(q, "装 v1.2.0 没成") {
		t.Fatalf("新版本照推：\n%s", q)
	}
}

// status 那一行：关着不说（由 status 收进「没开」），开着说上次看的时间和结果，不归它管的说为什么。
func TestAutoUpdateLine(t *testing.T) {
	r, _ := newAutoRig(t)
	if l := r.s.AutoUpdateLine(); !strings.Contains(l, "上次看") || !strings.Contains(l, "v1.1.0") || !strings.Contains(l, "空闲时装") {
		t.Fatalf("开着：%s", l)
	}
	r.s.appendUpgrade(UpgradeEvent{Kind: EvRollback, Release: "v1.1.0", At: time.Now()})
	if l := r.s.AutoUpdateLine(); strings.Contains(l, "空闲时装") || !strings.Contains(l, "回滚过") {
		t.Fatalf("回滚过的版本不再说空闲时装：%s", l)
	}
	r.s.Version = "v1.1.0"
	if l := r.s.AutoUpdateLine(); !strings.Contains(l, "已是最新") {
		t.Fatalf("最新：%s", l)
	}
	r.s.Version = "dev-abc1234"
	if l := r.s.AutoUpdateLine(); !strings.Contains(l, "源码构建") || r.s.AutoUpdateApplies() {
		t.Fatalf("源码构建：%s", l)
	}
	r.s.Cfg.AutoUpdate = false
	r.s.Version = "v1.0.0"
	if l := r.s.AutoUpdateLine(); l != "" || !r.s.AutoUpdateApplies() {
		t.Fatalf("关着：%q", l)
	}
}

// 有任务在跑、在过门（排队或在跑）：不装，这次不接新活，等它们做完（不起 agentq evolve apply）。
func TestReleaseWaitsForIdle(t *testing.T) {
	busy := map[string]*state.Job{
		"任务在跑": {ID: "r1", Project: "web", Status: state.StatusRunning},
		"排队过门": {ID: "g1", Project: "web", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateQueued}},
		"正在过门": {ID: "g2", Project: "web", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateRunning}},
	}
	for name, j := range busy {
		t.Run(name, func(t *testing.T) {
			r, st := newAutoRig(t)
			st.Jobs = append(st.Jobs, j)
			hold, note := r.s.SelfUpdate(st)
			if !hold || !strings.Contains(note, "自动更新：装 v1.1.0：等 1 个在跑或在过门的任务做完") {
				t.Fatalf("要等空闲：%v %s", hold, note)
			}
			if _, err := os.Stat(r.s.upgradeLogPath()); err == nil {
				t.Fatal("不空闲不该起 agentq evolve apply")
			}
		})
	}
}
