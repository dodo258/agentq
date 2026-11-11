package evolve

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// task 造一条结束了的待做：范围写在条目里，按它归领域；fail 为真时测试门 error 档拦过一次，门点名 file。
func (r *sigRig) task(id, project, scope, file string, fail bool) {
	r.t.Helper()
	end := r.now.Add(-time.Hour)
	j := &state.Job{ID: id, Project: project, Fingerprint: "fp-" + id, Title: "活 " + id, Item: "活 " + id + " —— 做（范围：" + scope + "）", EndedAt: &end}
	if fail {
		j.Status = state.StatusInterrupted
		j.Gate = &state.GateRecord{State: state.GateReturned}
		r.gateResult(id, gate.RunResult{Outcome: "fail", Results: []gate.Result{{Gate: "review", Outcome: gate.Fail, Level: gate.LevelError, Summary: "审查：" + file + " 第 3 行状态没清", Files: []string{file}}}})
	}
	r.meta(j)
}

func (r *sigRig) store() *SignalStore {
	r.t.Helper()
	st, err := r.s.LoadSignals()
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

// 验收：前端反复被门拦、返工，问题率远高于后端，报出「能力缺口：前端」，带任务号和门的结论，进 self_project 的待做，只写一条；
// 后端正常不报。合并之后这个领域清零，再出问题是新的缺口，标「补过 1 次又出现」。全程不调模型。
func TestCapabilityGapToTodoOnce(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject, c.SelfGrowEnabled = "agentq", true })
	r.now = time.Now() // 门结果按文件时间记：钟要和文件时间对得上，合并时才清得到之前的
	p := selfRepo(t, r, "agentq")
	for _, id := range []string{"f1", "f2", "f3"} {
		r.task(id, "web", "web/", "web/src/App.tsx", true)
	}
	r.task("f4", "web", "web/", "", false)
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		r.task(id, "api", "server/", "", false)
	}
	r.task("b6", "api", "server/", "server/main.go", true)
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	if strings.Count(text, "能力缺口：前端") != 1 || strings.Contains(text, "能力缺口：后端") {
		t.Fatalf("前端报一条，后端不报：\n%s", text)
	}
	for _, want := range []string{"任务 f1（web）review 门没过：审查：web/src/App.tsx", "frontend", "prior-art", "type 写 capability", gate.CapabilitiesFile, "前端（frontend）近 30 天问题 3 次"} {
		if !strings.Contains(text, want) {
			t.Errorf("条目缺 %q：\n%s", want, text)
		}
	}
	stats := domainStats(r.store().Domains, r.now)
	for _, d := range stats {
		if d.Gap != (d.Domain == DomainFrontend) {
			t.Fatalf("只有前端是缺口：%+v", stats)
		}
	}

	// 前端又被拦：还是那一条
	r.task("f5", "web", "web/", "web/src/Nav.tsx", true)
	r.collect()
	if text2 := testkit.ReadFile(t, p.BacklogPath()); strings.Count(text2, "能力缺口：前端") != 1 {
		t.Fatalf("同一领域只写一次：\n%s", text2)
	}

	// 缺口条目合并：前端清零
	end := r.now.Add(time.Minute)
	r.now = r.now.Add(2 * time.Minute)
	r.meta(&state.Job{ID: "g1", Project: "agentq", Fingerprint: "fp-g", Title: "能力缺口：前端", EndedAt: &end, Gate: &state.GateRecord{State: state.GateMerged, Merge: "abcdef0123456789"}})
	r.collect()
	st := r.store()
	if slices.ContainsFunc(st.Domains, func(e DomainEvent) bool { return e.Domain == DomainFrontend }) || slices.ContainsFunc(st.Signals, func(s Signal) bool { return s.Kind == KindGap }) {
		t.Fatalf("合并后前端清零：%+v %+v", st.Domains, st.Signals)
	}
	if fixes := st.Fixes[gapKey(DomainFrontend)]; len(fixes) != 1 || fixes[0] != "abcdef012345" {
		t.Fatalf("记一次修好：%+v", st.Fixes)
	}

	// 之后又反复出问题：新的缺口，标次数
	r.now = r.now.Add(time.Hour)
	for _, id := range []string{"h1", "h2", "h3"} {
		r.task(id, "web", "web/", "web/a.css", true)
	}
	r.collect()
	if text3 := testkit.ReadFile(t, p.BacklogPath()); !strings.Contains(text3, "能力缺口：前端（补过 1 次又出现）") {
		t.Fatalf("修好后再出现要重新写：\n%s", text3)
	}
	r.noCLI()
}

// 各领域问题率差不多、或问题不够多：不报缺口，也不写任何条目。
func TestCapabilityGapNotForNormalDomains(t *testing.T) {
	r := newSigRig(t, nil)
	for i, id := range []string{"f1", "f2", "f3", "f4", "f5", "f6"} {
		r.task(id, "web", "web/", "web/a.tsx", i < 2)
	}
	for i, id := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
		r.task(id, "api", "server/", "server/a.go", i < 2)
	}
	r.collect()
	for _, d := range domainStats(r.store().Domains, r.now) {
		if d.Gap {
			t.Fatalf("不该是缺口：%+v", d)
		}
	}
	if p := r.proposals(); strings.Contains(p, "能力缺口") {
		t.Fatalf("不报：\n%s", p)
	}
	// 3 次问题，但和后端一样高：也不报
	r.task("f7", "web", "web/", "web/b.tsx", true)
	r.task("b7", "api", "server/", "server/b.go", true)
	r.collect()
	if p := r.proposals(); strings.Contains(p, "能力缺口") {
		t.Fatalf("比例没有明显高于其他领域，不报：\n%s", p)
	}
}

// 返工意见按领域记进账，和门没过一起算；没配 self_project 时写进 proposals.md。evolve signals 看得到按领域的账和缺口。
func TestCapabilityGapFromReworkToFile(t *testing.T) {
	r := newSigRig(t, nil)
	p := selfRepo(t, r, "site")
	os.WriteFile(p.BacklogPath(), []byte("# 待办\n\n## 待做\n\n- [ ] 验收 首页 的意见 —— 配色像模板（范围：design/）\n- [ ] 验收 关于页 的意见 —— 图标难看（范围：design/）\n\n## 已完成\n"), 0o644)
	r.task("d1", "site", "design/", "design/home.svg", true)
	r.task("d2", "site", "design/", "", false)
	r.task("b1", "api", "server/", "", false)
	r.task("b2", "api", "server/", "", false)
	r.collect()
	pr := r.proposals()
	_, gapLine, _ := strings.Cut(pr, "能力缺口：设计 —— ")
	if strings.Count(pr, "能力缺口：设计") != 1 || !strings.Contains(gapLine, "项目 site「验收 首页") || !strings.Contains(gapLine, "domain 写 design") {
		t.Fatalf("返工加门没过够了要报：\n%s", pr)
	}
	r.s.Out.(interface{ Reset() }).Reset()
	if err := r.s.CmdEvolve([]string{"signals"}); err != nil {
		t.Fatal(err)
	}
	out := r.s.Out.(interface{ String() string }).String()
	for _, want := range []string{"capability-gap", "设计（design）", "返工 2", "门没过 1", "缺口，已写「能力缺口：设计」", "后端（backend）", "能力：还没有登记的能力"} {
		if !strings.Contains(out, want) {
			t.Errorf("evolve signals 缺 %q：\n%s", want, out)
		}
	}
	r.noCLI()
}

// 超时、卡在门上也按条目的领域算问题。
func TestDomainTimeoutAndStuck(t *testing.T) {
	r := newSigRig(t, nil)
	end := r.now.Add(-time.Hour)
	r.meta(&state.Job{ID: "t1", Project: "web", Fingerprint: "a", Title: "页面", Item: "页面 —— 做（范围：web/）", EndedAt: &end, Status: state.StatusTimeout})
	r.meta(&state.Job{ID: "t2", Project: "web", Fingerprint: "b", Title: "按钮布局", Item: "按钮布局 —— 改", EndedAt: &end, Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateStuck, Report: "退回 2 轮还没过"}})
	r.collect()
	stats := domainStats(r.store().Domains, r.now)
	if len(stats) != 1 || stats[0].Domain != DomainFrontend || stats[0].Tasks != 2 || stats[0].Stuck != 2 {
		t.Fatalf("%+v", stats)
	}
}

func withCaps(t *testing.T, caps []gate.Capability) {
	t.Helper()
	old := capList
	capList = func() ([]gate.Capability, error) { return caps, nil }
	t.Cleanup(func() { capList = old })
}

// 验收：一项能力 61 天没帮上忙，写「撤掉能力：<名字>」（没有别的新数据也写），只写一次，不删文件；新加的、最近帮上忙的不写。
func TestCapabilityRetireIdle(t *testing.T) {
	r := newSigRig(t, nil)
	day := func(n int) string { return r.now.AddDate(0, 0, -n).Format("2006-01-02") }
	withCaps(t, []gate.Capability{
		{Name: "web-perf", Domain: "frontend", Cases: []string{"030-x"}, Added: day(61), Knowledge: []string{"rules/web-perf.md"}},
		{Name: "fresh", Domain: "backend", Cases: []string{"031-x"}, Added: day(10)},
		{Name: "used", Domain: "backend", Cases: []string{"032-x"}, Added: day(90)},
	})
	// used 20 天前帮上过忙：用上它的任务过门合并了
	r.gateResult("u1", gate.RunResult{Outcome: "merged", Results: []gate.Result{{Gate: "capability", Outcome: gate.Pass, Level: gate.LevelWarn, Caps: []gate.CapRun{{Name: "used", Outcome: gate.Pass}}}}})
	end := r.now.AddDate(0, 0, -20)
	r.meta(&state.Job{ID: "u1", Project: "api", Fingerprint: "u", Title: "接口", EndedAt: &end, Gate: &state.GateRecord{State: state.GateMerged, Merge: "1111111"}})
	r.collect()
	pr := r.proposals()
	if strings.Count(pr, "撤掉能力：web-perf") != 1 || !strings.Contains(pr, "61 天") || !strings.Contains(pr, "不自动删文件") || strings.Contains(pr, "撤掉能力：fresh") || strings.Contains(pr, "撤掉能力：used") {
		t.Fatalf("只撤 web-perf：\n%s", pr)
	}
	if u := r.store().Caps["used"]; u == nil || !strings.Contains(u.Help, "任务 u1（api）用上这项能力、过门合并") {
		t.Fatalf("记下帮上忙的证据：%+v", u)
	}
	r.unchanged()
	if pr2 := r.proposals(); pr2 != pr {
		t.Fatalf("只写一次：\n%s", pr2)
	}

	// 只是时间过去：没有新文件，也要写
	r2 := newSigRig(t, nil)
	r2.collect()
	r2.now = r2.now.Add(2 * time.Hour)
	withCaps(t, []gate.Capability{{Name: "old", Domain: "docs", Cases: []string{"033-x"}, Added: r2.now.AddDate(0, 0, -61).Format("2006-01-02")}})
	if !r2.collect() || !strings.Contains(r2.proposals(), "撤掉能力：old") {
		t.Fatalf("到期了要写：%s", r2.proposals())
	}
	r2.noCLI()
}

// 检查报了问题、同一条后来修好：算帮上忙；报了问题照样合并或被放弃：算放过，放过一半以上写撤掉（误报多）。
func TestCapabilityCheckHelpAndNoise(t *testing.T) {
	r := newSigRig(t, nil)
	withCaps(t, []gate.Capability{{Name: "a11y", Domain: "frontend", Cases: []string{"030-x"}, Added: r.now.AddDate(0, 0, -5).Format("2006-01-02"), Check: "a11y.sh"}})
	capRes := func(id string, o gate.Outcome) {
		r.gateResult(id, gate.RunResult{Outcome: "merged", Results: []gate.Result{{Gate: "capability", Outcome: o, Level: gate.LevelWarn, Caps: []gate.CapRun{{Name: "a11y", Outcome: o}}}}})
	}
	capRes("x1", gate.Fail)
	r.meta(&state.Job{ID: "x1", Project: "web", Fingerprint: "fx", Title: "表单", Gate: &state.GateRecord{State: state.GateReturned}})
	capRes("x2", gate.Pass)
	r.meta(&state.Job{ID: "x2", Project: "web", Fingerprint: "fx", Title: "表单", Gate: &state.GateRecord{State: state.GateMerged, Merge: "2222222"}})
	r.collect()
	u := r.store().Caps["a11y"]
	if u == nil || u.Flags != 1 || u.Ignored != 0 || !strings.Contains(u.Help, "任务 x2（web）：检查报的问题之后修好了") {
		t.Fatalf("报了又修好是帮上忙：%+v", u)
	}
	for i, id := range []string{"y1", "y2", "y3", "y4"} {
		capRes(id, gate.Fail)
		g := &state.GateRecord{State: state.GateMerged, Merge: "3333333"}
		if i == 3 {
			g = &state.GateRecord{State: state.GateAbandoned}
		}
		r.meta(&state.Job{ID: id, Project: "web", Fingerprint: "f" + id, Title: "活 " + id, Gate: g})
	}
	r.collect()
	u = r.store().Caps["a11y"]
	if u.Flags != 5 || u.Ignored != 4 || len(u.Open) != 0 {
		t.Fatalf("%+v", u)
	}
	if pr := r.proposals(); !strings.Contains(pr, "撤掉能力：a11y") || !strings.Contains(pr, "误报多") {
		t.Fatalf("误报多要写撤掉：\n%s", pr)
	}
}

// 领域表：评测题用的领域都对得上中文名；缺口、能力清单用英文名。
func TestDomainTable(t *testing.T) {
	if DomainLabel("frontend") != "前端（frontend）" || DomainLabel(DomainDesign) != "设计（design）" || DomainLabel("别的") != "别的" {
		t.Fatal(DomainLabel("frontend"), DomainLabel(DomainDesign))
	}
	cases, err := LoadCases(os.DirFS(filepath.Join("..", "..", "evals")), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if DomainCN(c.Domain) == c.Domain {
			t.Errorf("题 %s 的领域 %s 不在领域表里", c.ID, c.Domain)
		}
	}
}

// 仓库里的能力清单：读得出来，每项的领域在表里，知识文件、检查脚本、考题都在。
func TestCapabilityRegistryConsistent(t *testing.T) {
	caps, err := gate.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	for _, c := range caps {
		if DomainCN(c.Domain) == c.Domain {
			t.Errorf("能力 %s 的领域 %s 不在领域表里", c.Name, c.Domain)
		}
		files := slices.Clone(c.Knowledge)
		if c.Check != "" {
			files = append(files, filepath.Join(filepath.Dir(gate.CapabilitiesFile), c.Check))
		}
		for _, cs := range c.Cases {
			files = append(files, filepath.Join("evals", cs, "case.json"))
		}
		for _, f := range files {
			if _, err := fs.Stat(os.DirFS(root), filepath.ToSlash(f)); err != nil {
				t.Errorf("能力 %s 登记的 %s 不在：%v", c.Name, f, err)
			}
		}
		if len(c.Knowledge) == 0 && c.Check == "" {
			t.Errorf("能力 %s 既没有知识也没有检查", c.Name)
		}
	}
}

// 验收（评测门的结论）：长能力要这个领域加上新题后涨分；新题在运行中这一代上就过了、或候选上也没过，都是没涨；别的领域掉了照样算变差。
func TestCapabilityVerdict(t *testing.T) {
	sc := func(c, d string, pts int) Score {
		return Score{Case: c, Domain: d, Mode: ModeFake, Points: pts, Correct: pts == 100}
	}
	base := []Score{sc("006-ui-page", "frontend", 100), sc("018-ui-dark-mode", "frontend", 50), sc("001-impl-go", "backend", 100)}
	run := func(cur []Score, before, after int, domains ...string) []string {
		v := judgeVerdict(base, true, cur)
		capabilityVerdict(&v, domains, base, cur, []Score{sc("030-web-perf", "frontend", before)}, []Score{sc("030-web-perf", "frontend", after)})
		return v.Worse
	}
	if w := run(base, 0, 100, "frontend"); len(w) != 0 {
		t.Fatalf("新题在这一代没过、候选过了，领域涨了：%v", w)
	}
	if w := run(base, 0, 0, "frontend"); len(w) != 1 || !strings.Contains(w[0], "领域 前端（frontend）") || !strings.Contains(w[0], "没涨") {
		t.Fatalf("候选也没过：%v", w)
	}
	if w := run(base, 100, 100, "frontend"); len(w) != 1 || !strings.Contains(w[0], "没涨") {
		t.Fatalf("新题这一代就过了，证明不了缺：%v", w)
	}
	dropped := []Score{sc("006-ui-page", "frontend", 100), sc("018-ui-dark-mode", "frontend", 50), sc("001-impl-go", "backend", 0)}
	if w := run(dropped, 0, 100, "frontend"); !slices.ContainsFunc(w, func(l string) bool { return strings.Contains(l, "领域 backend") }) {
		t.Fatalf("别的领域掉了要拒：%v", w)
	}
	if w := run(base, 0, 100, "design"); len(w) != 1 || !strings.Contains(w[0], "设计（design）：没有新加这个领域的考题") {
		t.Fatalf("新题不是这个领域的：%v", w)
	}
	v := judgeVerdict(nil, false, nil)
	capabilityVerdict(&v, []string{"frontend"}, nil, nil, []Score{sc("030-web-perf", "frontend", 0)}, []Score{sc("030-web-perf", "frontend", 0)})
	if len(v.Worse) != 1 || v.Baseline {
		t.Fatalf("没有基线也要涨：%+v", v)
	}
}

// 候选新加的题从磁盘读：运行中这一代已有的题号不认（题只加不改），读不了的报错。
func TestLoadNewCases(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "030-web-perf"), 0o755)
	os.WriteFile(filepath.Join(dir, "030-web-perf", "case.json"), []byte(`{"domain":"frontend","kind":"program"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "030-web-perf", "check.sh"), []byte("exit 1\n"), 0o755)
	cs, err := loadNewCases(dir, []string{"030-web-perf"}, nil)
	if err != nil || len(cs) != 1 || cs[0].Domain != "frontend" {
		t.Fatalf("%v %+v", err, cs)
	}
	if _, err := loadNewCases(dir, []string{"030-web-perf"}, cs); err == nil || !strings.Contains(err.Error(), "只加不改") {
		t.Fatalf("已有的题：%v", err)
	}
	if _, err := loadNewCases(dir, []string{"031-none"}, nil); err == nil {
		t.Fatal("没有的题要报错")
	}
	if _, err := loadNewCases("", []string{"030-web-perf"}, nil); err == nil {
		t.Fatal("没写 --new-dir 要报错")
	}
}
