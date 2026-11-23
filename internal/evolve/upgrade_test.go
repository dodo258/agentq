package evolve

import (
	"os"
	"path/filepath"
	"slices"
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
		if pl.Action != EvRollback || pl.Commit != u.b || !strings.Contains(pl.Why, "失败、超时或卡在门上 2 个") {
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
	// 11-16 升到 dev-40466ea 后的三个任务都是升级前被门退回、升级后接着做的（带过来 3、1、1 轮），被当成这一代退回的回滚了。
	chain := func(u *upRig, fp string, starts []time.Time, last *state.GateRecord) {
		for i, s := range starts {
			end := s.Add(10 * time.Minute)
			j := &state.Job{ID: fp + string(rune('0'+i)), Project: "web", Title: fp, Fingerprint: fp, Status: state.StatusInterrupted,
				StartedAt: s, EndedAt: &end, GateRound: i, Gate: &state.GateRecord{State: state.GateReturned, Round: i}}
			if i == len(starts)-1 {
				j.Status, j.Gate = state.StatusDone, last
				last.Round = i
			}
			u.st.Jobs = append(u.st.Jobs, j)
		}
	}
	merged := func() *state.GateRecord { return &state.GateRecord{State: state.GateMerged} }
	t.Run("升级前退回的轮数不算进观察期", func(t *testing.T) {
		u, at := setup(t)
		chain(u, "fa", []time.Time{at.Add(-4 * time.Hour), at.Add(-3 * time.Hour), at.Add(-2 * time.Hour), at.Add(time.Hour)}, merged())
		chain(u, "fb", []time.Time{at.Add(-90 * time.Minute), at.Add(2 * time.Hour)}, merged())
		chain(u, "fc", []time.Time{at.Add(-time.Hour), at.Add(3 * time.Hour)}, &state.GateRecord{State: state.GateStuck})
		pl := u.s.plan(t.Context(), u.st, u.now)
		if pl.Action == EvRollback {
			t.Fatalf("退回都发生在升级前，不该回滚：%+v", pl)
		}
		evs, _ := u.s.LoadUpgrades()
		if last := evs[len(evs)-1]; last.Kind != EvKept || !strings.Contains(last.Why, "升级后 3 个任务，失败、超时或卡在门上 1 个，平均过门退回 0.0 轮") {
			t.Fatalf("要记留下、升级后 0 轮：%+v", last)
		}
	})
	t.Run("升级后退回的照算", func(t *testing.T) {
		u, at := setup(t)
		for i, fp := range []string{"fa", "fb", "fc"} {
			s := at.Add(time.Duration(i+1) * time.Hour)
			chain(u, fp, []time.Time{s, s.Add(20 * time.Minute)}, merged())
		}
		pl := u.s.plan(t.Context(), u.st, u.now)
		if pl.Action != EvRollback || !strings.Contains(pl.Why, "升级后 3 个任务，失败、超时或卡在门上 0 个，平均过门退回 1.0 轮") {
			t.Fatalf("升级后每个都退回一轮要回滚：%+v", pl)
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
	t.Run("还在过门的不算，卡在门上的算失败", func(t *testing.T) {
		u, at := setup(t)
		for i, g := range []string{state.GateQueued, state.GateRunning, state.GateStuck} {
			u.job("g"+string(rune('1'+i)), at.Add(time.Duration(i+1)*time.Hour), state.StatusDone, 0)
			u.st.Jobs[len(u.st.Jobs)-1].Gate = &state.GateRecord{State: g}
		}
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" || !strings.Contains(pl.Note, "1/3") {
			t.Fatalf("在过门的不算数，先等：%+v", pl)
		}
		if evs, _ := u.s.LoadUpgrades(); evs[len(evs)-1].Kind == EvKept {
			t.Fatal("门没出结论不能记留下")
		}
		for _, j := range u.st.Jobs[len(u.st.Jobs)-3 : len(u.st.Jobs)-1] {
			j.Gate.State = state.GateStuck
		}
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvRollback || !strings.Contains(pl.Why, "卡在门上 3 个") {
			t.Fatalf("三个都卡在门上要回滚：%+v", pl)
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
		evs, _ := u.s.LoadUpgrades()
		if last := evs[len(evs)-1]; last.Kind != EvUpgrade || last.To != "dev-1234567" || last.From != "dev-"+u.b[:7] || last.Commit != "1234567" || last.Why != manualWhy {
			t.Fatalf("自己换的版本要补记一条手动装的升级（仓库里没有这个提交就记短号）：%+v", last)
		}
	})
}

// 手动装的一代（scripts/update.sh、setup --yes）：升级记录里最后装上的是别的版本，tick 补记一条升级，从这时开始观察，
// 满了没变差记「留下」、发版看得到它的提交；变差照样回滚。
func TestUpgradeManualInstall(t *testing.T) {
	setup := func(t *testing.T) *upRig {
		u := newUpRig(t)
		for i := range 5 {
			u.job("pre"+string(rune('a'+i)), u.now.Add(-time.Duration(10-i)*time.Hour), state.StatusDone, 0)
		}
		u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-48 * time.Hour), Kind: EvKept, To: "dev-" + u.a[:7], Commit: u.a})
		u.s.Version = "dev-" + u.b[:7] // 用户自己装上了 B
		return u
	}
	manual := func(t *testing.T, u *upRig) UpgradeEvent {
		t.Helper()
		evs, _ := u.s.LoadUpgrades()
		var got []UpgradeEvent
		for _, ev := range evs {
			if ev.Why == manualWhy {
				got = append(got, ev)
			}
		}
		if len(got) != 1 {
			t.Fatalf("要补记一条（只一条）手动装的升级：%+v", evs)
		}
		return got[0]
	}
	t.Run("观察期没变差留下", func(t *testing.T) {
		u := setup(t)
		pl := u.s.plan(t.Context(), u.st, u.now)
		if pl.Action != "" || !strings.Contains(pl.Note, "0/3") {
			t.Fatalf("补记后从现在开始观察：%+v", pl)
		}
		ev := manual(t, u)
		if ev.Kind != EvUpgrade || !ev.At.Equal(u.now) || ev.From != "dev-"+u.a[:7] || ev.To != u.s.Version || ev.Commit != u.b || ev.Job != "" {
			t.Fatalf("记录不对：%+v", ev)
		}
		if c, why := u.s.ObservedCommit(); c != "" || !strings.Contains(why, "还没过观察期") {
			t.Fatalf("还在观察：%q %q", c, why)
		}
		u.s.plan(t.Context(), u.st, u.now) // 再看一次不重复记
		manual(t, u)
		for i := range 3 {
			u.job("o"+string(rune('1'+i)), u.now.Add(time.Duration(i+1)*time.Minute), state.StatusDone, 0)
		}
		u.now = u.now.Add(time.Hour)
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != "" {
			t.Fatalf("没变差不动：%+v", pl)
		}
		evs, _ := u.s.LoadUpgrades()
		if last := evs[len(evs)-1]; last.Kind != EvKept || last.To != u.s.Version || last.Commit != u.b {
			t.Fatalf("要记留下：%+v", evs)
		}
		if c, why := u.s.ObservedCommit(); c != u.b[:7] {
			t.Fatalf("过了观察期要返回这一代的提交：%q %q", c, why)
		}
		manual(t, u)
	})
	t.Run("变差回滚", func(t *testing.T) {
		u := setup(t)
		u.s.plan(t.Context(), u.st, u.now)
		for i, st := range []string{state.StatusFailed, state.StatusTimeout, state.StatusDone} {
			u.job("o"+string(rune('1'+i)), u.now.Add(time.Duration(i+1)*time.Minute), st, 0)
		}
		u.now = u.now.Add(time.Hour)
		if pl := u.s.plan(t.Context(), u.st, u.now); pl.Action != EvRollback || pl.Commit != u.b {
			t.Fatalf("手动装的变差了照样回滚：%+v", pl)
		}
	})
	for name, edit := range map[string]func(u *upRig){
		"没有记录":     func(u *upRig) { os.Remove(u.s.upgradesPath()) },
		"装着的就是记录的": func(u *upRig) { u.s.Version = "dev-" + u.a[:7] },
		"回滚后报不出版本": func(u *upRig) {
			u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-time.Hour), Kind: EvRollback, From: "dev-" + u.a[:7], To: unknownPrev, Commit: u.a})
		},
		"没做成的不算": func(u *upRig) {
			u.s.Version = "dev-" + u.a[:7]
			u.s.appendUpgrade(UpgradeEvent{At: u.now.Add(-time.Hour), Kind: EvFail, From: u.s.Version, Commit: u.b})
		},
	} {
		t.Run("不补："+name, func(t *testing.T) {
			u := setup(t)
			edit(u)
			u.s.plan(t.Context(), u.st, u.now)
			if evs, _ := u.s.LoadUpgrades(); slices.ContainsFunc(evs, func(ev UpgradeEvent) bool { return ev.Why == manualWhy }) {
				t.Fatalf("不该补记：%+v", evs)
			}
		})
	}
	t.Run("不补：正在升级", func(t *testing.T) {
		u := setup(t)
		lock, err := fsx.AcquireLock(u.s.upgradeLockPath(), 0)
		if err != nil || lock == nil {
			t.Fatal(err)
		}
		defer lock.Close()
		u.s.plan(t.Context(), u.st, u.now)
		if evs, _ := u.s.LoadUpgrades(); slices.ContainsFunc(evs, func(ev UpgradeEvent) bool { return ev.Why == manualWhy }) {
			t.Fatalf("apply 拿着升级锁（装完还没写记录）时不补记：%+v", evs)
		}
	})
}

// 有任务在跑、在过门（排队或在跑）：不起 apply，这次不接新活，挡多久都一直等到空闲；有一个 apply 正在做也挡着。
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
	u.now = u.now.Add(24 * time.Hour)
	if hold, note := u.s.SelfUpdate(u.st); !hold || !strings.Contains(note, "等 2 个在跑或在过门的任务做完") {
		t.Fatalf("挡多久都不放开，一直等到空闲（放开了就再也等不到空闲）：%v %s", hold, note)
	}
	u.st.Jobs = u.st.Jobs[:1] // 换一次：apply 正在做
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

// 发版看的观察期：最近一条升级、留下、回滚的记录是这一代的「留下」才算过了；演练、失败不算数。
func TestObservedCommit(t *testing.T) {
	u := newUpRig(t)
	u.s.Version = "dev-" + u.a[:7]
	if c, why := u.s.ObservedCommit(); c != "" || !strings.Contains(why, "还没过观察期") {
		t.Fatalf("没有记录：%q %q", c, why)
	}
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvUpgrade, To: u.s.Version})
	if c, _ := u.s.ObservedCommit(); c != "" {
		t.Fatalf("还在观察：%q", c)
	}
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvKept, To: u.s.Version})
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvFail, Why: "演练没过"})
	if c, why := u.s.ObservedCommit(); c != u.a[:7] {
		t.Fatalf("留下了：%q %q", c, why)
	}
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvRollback, From: u.s.Version, To: "dev-1234567"})
	if c, _ := u.s.ObservedCommit(); c != "" {
		t.Fatalf("回滚之后不算：%q", c)
	}
	dirty := "dev-" + u.a[:7] + "+dirty"
	u.s.Version = dirty
	u.s.appendUpgrade(UpgradeEvent{At: u.now, Kind: EvKept, To: dirty})
	if c, why := u.s.ObservedCommit(); c != "" || !strings.Contains(why, "未提交的改动") {
		t.Fatalf("带未提交改动构建的留下了也不算观察过：%q %q", c, why)
	}
	u.s.Version = "v0.3.0"
	if c, why := u.s.ObservedCommit(); c != "" || !strings.Contains(why, "不是从提交构建的") {
		t.Fatalf("发行版：%q %q", c, why)
	}
}

// 卡在门上后被同一条目别的任务取代的，这个任务也没过门：观察期照样算一次不好，不因为后来有人做完就变好看。
func TestObserveCountsSupersededAsBad(t *testing.T) {
	jobs := []*state.Job{
		{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateSuperseded}},
		{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateMerged}},
	}
	if o := observe(jobs); o.N != 2 || o.Bad != 1 {
		t.Fatalf("%+v", o)
	}
}
