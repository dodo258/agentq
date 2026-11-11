package evolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// upRig：装过一代（有 receipt.json）、self_project 是一个有两个提交的仓库，现在跑的是第一个提交 A，B 是引擎合并、过了评测门的。
type upRig struct {
	*sigRig
	p    backlog.Project
	a, b string
	st   *state.State
}

func newUpRig(t *testing.T) *upRig {
	t.Helper()
	r := newSigRig(t, func(c *config.Config) { c.SelfProject, c.ExcludeProjects = "agentq", []string{"agentq"} })
	p := selfRepo(t, r, "agentq")
	a := testkit.GitT(t, r.s.Home, p.Dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(p.Dir, "fix.txt"), []byte("修好了"), 0o644)
	testkit.GitT(t, r.s.Home, p.Dir, "add", "-A")
	testkit.GitT(t, r.s.Home, p.Dir, "commit", "-q", "-m", "修")
	b := testkit.GitT(t, r.s.Home, p.Dir, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Dir(r.s.receiptPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.s.receiptPath(), []byte(`{"version":1,"generation":"/g/gen-1"}`), 0o600)
	r.s.Version = "dev-" + a[:7] + "+dirty"
	end := r.now.Add(-48 * time.Hour)
	st := &state.State{Jobs: []*state.Job{{ID: "m1", Project: "agentq", Title: "修", Status: state.StatusDone, StartedAt: end.Add(-time.Hour), EndedAt: &end,
		Gate: &state.GateRecord{State: state.GateMerged, Merge: b, Tally: map[string]*state.GateTally{"eval": {Pass: 1}}}}}}
	return &upRig{sigRig: r, p: p, a: a, b: b, st: st}
}

// job 加一个结束了的任务：开始于 at，状态 status，过门退回了 round 轮。
func (u *upRig) job(id string, at time.Time, status string, round int) {
	end := at.Add(10 * time.Minute)
	u.st.Jobs = append(u.st.Jobs, &state.Job{ID: id, Project: "web", Title: id, Status: status, StartedAt: at, EndedAt: &end, GateRound: round})
}

// 合并过了评测门、是现在这一代的后代：要升级，第一次先演练整代回滚；演练过了就不再演练。
func TestUpgradePlanTarget(t *testing.T) {
	u := newUpRig(t)
	pl := u.s.plan(t.Context(), u.st, u.now)
	if pl.Action != EvUpgrade || pl.Commit != u.b || pl.Job != "m1" || !pl.Drill {
		t.Fatalf("要升级到 B 并先演练：%+v", pl)
	}
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvDrill, To: u.s.Version})
	if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvUpgrade || pl.Drill {
		t.Fatalf("演练过不再演练：%+v", pl)
	}
}

// 不升级的情况：开关关、暂停、没配 self_project、发行版、不是安装引擎装的、评测门没过、已经是最新、不是后代、回滚过、刚失败过。
func TestUpgradePlanSkips(t *testing.T) {
	cases := map[string]func(u *upRig){
		"开关关":    func(u *upRig) { u.s.Cfg.SelfGrowEnabled = false },
		"暂停":     func(u *upRig) { u.s.Cfg.Paused = true },
		"没配项目":   func(u *upRig) { u.s.Cfg.SelfProject = "" },
		"发行版":    func(u *upRig) { u.s.Version = "v0.1.0" },
		"没有安装记录": func(u *upRig) { os.Remove(u.s.receiptPath()) },
		"评测门没过":  func(u *upRig) { u.st.Jobs[0].Gate.Tally = map[string]*state.GateTally{"eval": {Fail: 1}} },
		"已是最新":   func(u *upRig) { u.s.Version = "dev-" + u.b[:7] },
		"不是后代": func(u *upRig) {
			testkit.GitT(t, u.s.Home, u.p.Dir, "checkout", "-q", "--orphan", "other")
			testkit.GitT(t, u.s.Home, u.p.Dir, "commit", "-q", "-m", "别的")
			u.s.Version = "dev-" + testkit.GitT(t, u.s.Home, u.p.Dir, "rev-parse", "HEAD")[:7]
		},
		"回滚过": func(u *upRig) {
			u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-48 * time.Hour), Kind: EvRollback, Commit: u.b})
		},
		"刚失败过":  func(u *upRig) { u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-time.Hour), Kind: EvFail, Commit: u.b}) },
		"刚演练失败": func(u *upRig) { u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-time.Hour), Kind: EvFail}) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			u := newUpRig(t)
			edit(u)
			if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" {
				t.Fatalf("不该升级：%+v", pl)
			}
		})
	}
	u := newUpRig(t)
	u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-7 * time.Hour), Kind: EvFail, Commit: u.b})
	if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvUpgrade {
		t.Fatalf("失败过 6 小时以上再试：%+v", pl)
	}
}

// 观察期：凑够 3 个任务，失败或超时多了就回滚；没变差记「留下」，之后照常看有没有新的可升。
func TestUpgradeObservation(t *testing.T) {
	setup := func(t *testing.T) (*upRig, time.Time) {
		u := newUpRig(t)
		at := u.now.Add(-5 * time.Hour)
		u.s.Version = "dev-" + u.b[:7]
		for i, s := range []string{state.StatusDone, state.StatusDone, state.StatusFailed, state.StatusDone, state.StatusDone} {
			u.job("pre"+string(rune('a'+i)), at.Add(-time.Duration(10-i)*time.Hour), s, 0)
		}
		u.s.appendUpgrade(UpgradeEvent{At: at, Kind: EvUpgrade, From: "dev-" + u.a[:7], To: u.s.Version, Commit: u.b, Job: "m1"})
		return u, at
	}
	t.Run("观察中", func(t *testing.T) {
		u, at := setup(t)
		u.job("p1", at.Add(time.Hour), state.StatusFailed, 0)
		u.job("p2", at.Add(2*time.Hour), state.StatusTimeout, 0)
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" || !strings.Contains(pl.Note, "2/3") {
			t.Fatalf("不够 3 个任务先等：%+v", pl)
		}
	})
	t.Run("变差回滚", func(t *testing.T) {
		u, at := setup(t)
		u.job("p1", at.Add(time.Hour), state.StatusFailed, 0)
		u.job("p2", at.Add(2*time.Hour), state.StatusTimeout, 0)
		u.job("p3", at.Add(3*time.Hour), state.StatusDone, 0)
		pl := u.s.plan(t.Context(), u.st, u.now)
		if pl.Action != EvRollback || pl.Commit != u.b || !strings.Contains(pl.Why, "失败或超时 2 个") {
			t.Fatalf("要回滚：%+v", pl)
		}
	})
	t.Run("退回轮数多回滚", func(t *testing.T) {
		u, at := setup(t)
		for i := range 3 {
			u.job("p"+string(rune('1'+i)), at.Add(time.Duration(i+1)*time.Hour), state.StatusDone, 1)
		}
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvRollback {
			t.Fatalf("每个都退回一轮要回滚：%+v", pl)
		}
	})
	t.Run("没变差留下", func(t *testing.T) {
		u, at := setup(t)
		u.job("p1", at.Add(time.Hour), state.StatusDone, 0)
		u.job("p2", at.Add(2*time.Hour), state.StatusFailed, 0)
		u.job("p3", at.Add(3*time.Hour), state.StatusDone, 0)
		u.job("p4", at.Add(-30*time.Minute), state.StatusFailed, 0) // 升级前开始的不算
		u.st.Jobs = append(u.st.Jobs, &state.Job{ID: "lim", StartedAt: at.Add(time.Hour), EndedAt: &at, Status: state.StatusFailed, LimitHit: true})
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" {
			t.Fatalf("没变差不动：%+v", pl)
		}
		evs, _ := u.s.LoadUpgrades()
		if last := evs[len(evs)-1]; last.Kind != EvKept || last.Commit != u.b {
			t.Fatalf("要记留下：%+v", evs)
		}
		if up := openUpgrade(evs, u.s.Version); up != nil {
			t.Fatal("留下之后不再观察")
		}
	})
	t.Run("三天凑不够按已有的判", func(t *testing.T) {
		u, at := setup(t)
		u.now = at.Add(4 * 24 * time.Hour)
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" {
			t.Fatalf("%+v", pl)
		}
		if evs, _ := u.s.LoadUpgrades(); evs[len(evs)-1].Kind != EvKept {
			t.Fatalf("三天到了记留下：%+v", evs)
		}
	})
	t.Run("用户自己换了版本", func(t *testing.T) {
		u, at := setup(t)
		u.s.Version = "dev-1234567"
		u.job("p1", at.Add(time.Hour), state.StatusFailed, 0)
		u.job("p2", at.Add(2*time.Hour), state.StatusFailed, 0)
		u.job("p3", at.Add(3*time.Hour), state.StatusFailed, 0)
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action == EvRollback {
			t.Fatalf("装的不是升级的那一代，不回滚：%+v", pl)
		}
	})
}

// 有任务在跑、在过门（排队或在跑）：不起 apply，这次不接新活；挡满 2 小时还不空闲就放开接活；有一个 apply 正在做也挡着。
func TestSelfUpdateHoldsWhileBusy(t *testing.T) {
	u := newUpRig(t)
	u.st.Jobs = append(u.st.Jobs, &state.Job{ID: "run1", Project: "web", Status: state.StatusRunning, StartedAt: u.now},
		&state.Job{ID: "gate1", Project: "web", Status: state.StatusDone, StartedAt: u.now, Gate: &state.GateRecord{State: state.GateQueued}})
	hold, note := u.s.SelfUpdate(u.st)
	if !hold || !strings.Contains(note, "等 2 个在跑或在过门的任务做完") {
		t.Fatalf("有任务在跑、在过门要等：%v %s", hold, note)
	}
	if _, err := os.Stat(u.s.upgradeLogPath()); err == nil {
		t.Fatal("不空闲不该起 apply")
	}
	u.now = u.now.Add(119 * time.Minute)
	if hold, _ := u.s.SelfUpdate(u.st); !hold {
		t.Fatal("不到 2 小时还挡着")
	}
	u.now = u.now.Add(time.Minute)
	if hold, note := u.s.SelfUpdate(u.st); hold || !strings.Contains(note, "先照常接活") {
		t.Fatalf("挡满 2 小时放开：%v %s", hold, note)
	}
	u.st.Jobs = u.st.Jobs[:1]
	os.Remove(u.s.holdPath()) // 换一次：apply 正在做
	os.MkdirAll(u.s.evolveDir(), 0o700)
	lock, err := fsx.AcquireLock(u.s.upgradeLockPath(), 0)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if hold, note := u.s.SelfUpdate(u.st); !hold || !strings.Contains(note, "正在做") {
		t.Fatalf("apply 正在做：%v %s", hold, note)
	}
	u.s.Cfg.SelfGrowEnabled = false
	if hold, _ := u.s.SelfUpdate(u.st); hold {
		t.Fatal("关着不挡接活")
	}
}

func TestWorseRule(t *testing.T) {
	for _, c := range []struct {
		before, after observation
		want          bool
	}{
		{observation{5, 0, 0}, observation{3, 1, 0}, false}, // 一个偶然失败不算
		{observation{5, 0, 0}, observation{3, 2, 0}, true},
		{observation{5, 4, 0}, observation{3, 2, 0}, false}, // 比例没升
		{observation{5, 0, 0}, observation{3, 0, 3}, true},  // 平均多退回一轮
		{observation{5, 0, 5}, observation{3, 0, 4}, false},
		{observation{0, 0, 0}, observation{0, 0, 0}, false},
	} {
		if got, why := worse(c.before, c.after); got != c.want {
			t.Errorf("%+v → %+v：%v（%s）", c.before, c.after, got, why)
		}
	}
}

// 回滚过 B：之后合并的 C 带着 B，不升；修 B 的那一条（Fixes[RollbackKey(B)]）合并进来的版本才升；用户自己装了带 B 的版本也算放行。
func TestUpgradeSkipsDescendantsOfRollback(t *testing.T) {
	u := newUpRig(t)
	u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-48 * time.Hour), Kind: EvRollback, Commit: u.b})
	commit := func(name string) string {
		os.WriteFile(filepath.Join(u.p.Dir, name), []byte(name), 0o644)
		testkit.GitT(t, u.s.Home, u.p.Dir, "add", "-A")
		testkit.GitT(t, u.s.Home, u.p.Dir, "commit", "-q", "-m", name)
		return testkit.GitT(t, u.s.Home, u.p.Dir, "rev-parse", "HEAD")
	}
	merged := func(id, c string) {
		end := u.now.Add(-time.Duration(10-len(u.st.Jobs)) * time.Minute)
		u.st.Jobs = append(u.st.Jobs, &state.Job{ID: id, Project: "agentq", Title: id, Status: state.StatusDone, StartedAt: end.Add(-time.Hour), EndedAt: &end,
			Gate: &state.GateRecord{State: state.GateMerged, Merge: c, Tally: map[string]*state.GateTally{"eval": {Pass: 1}}}})
	}
	c := commit("c.txt")
	merged("mc", c)
	pl := u.s.plan(t.Context(), u.st, u.now)
	if pl.Action != "" || !strings.Contains(pl.Note, "带着回滚过的 "+short(u.b)) {
		t.Fatalf("带着回滚过的提交不升：%+v", pl)
	}
	f := commit("fix.txt")
	merged("mf", f)
	os.MkdirAll(u.s.evolveDir(), 0o700)
	u.s.saveSignals(&SignalStore{Fixes: map[string][]string{RollbackKey(u.b): {short(f)}}})
	if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvUpgrade || pl.Commit != f {
		t.Fatalf("修它的那一条合并了要升：%+v", pl)
	}
	os.Remove(u.s.SignalsPath())
	u.s.Version = "dev-" + u.b[:7] // 用户自己装上了 B
	if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvUpgrade || pl.Commit != f {
		t.Fatalf("现在这一代已经带着 B，不再挡：%+v", pl)
	}
}
