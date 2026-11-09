package evolve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// sigRig 是一个临时 HOME：假的门结果、任务记录、巡逻文件都写在它的 ~/.agentq 下，钟是假的。
type sigRig struct {
	t     *testing.T
	s     *Service
	now   time.Time
	mtime time.Time // 下一份文件的修改时间：真实时钟往回拨，一份比一份晚一秒
	root  string
	cli   string // 假 CLI 的调用记录：收信号不该有
}

func newSigRig(t *testing.T, edit func(c *config.Config)) *sigRig {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	r := &sigRig{t: t, now: time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local), mtime: time.Now().Add(-time.Hour), root: root}
	r.cli = filepath.Join(home, "cli-calls")
	fake := filepath.Join(home, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> "+r.cli+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ProjectsRoot = root
	cfg.ClaudePath = fake
	if edit != nil {
		edit(&cfg)
	}
	e := &env.Env{Home: home, Cfg: cfg, Now: func() time.Time { return r.now }, Out: &bytes.Buffer{}}
	k := stack.New(e)
	sg := &suggest.Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Notify: k.Notify, Runner: k.Runner}
	r.s = &Service{Env: e, Backlog: k.Backlog, Gate: k.Gate, Ledger: k.Ledger, Notify: k.Notify, Suggest: sg}
	return r
}

func (r *sigRig) write(path string, v any) {
	r.t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.mtime = r.mtime.Add(time.Second)
	if err := os.Chtimes(path, r.mtime, r.mtime); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sigRig) meta(j *state.Job) {
	if j.Status == "" {
		j.Status = state.StatusDone
	}
	r.write(filepath.Join(r.s.JobDir(j.ID), "meta.json"), j)
}

func (r *sigRig) gateResult(id string, res gate.RunResult) {
	r.write(filepath.Join(r.s.Gate.Dir(id), "last.json"), res)
}

// blocked 造一次门拦截：任务 id 的测试门 error 档没过，退回了。
func (r *sigRig) blocked(id, project, fp, summary string) {
	r.meta(&state.Job{ID: id, Project: project, Fingerprint: fp, Title: "标题 " + fp, Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateReturned, Branch: "agent/" + id}})
	r.gateResult(id, gate.RunResult{Outcome: "fail", Reason: "test 门没过", Head: "1234567890abcdef",
		Results: []gate.Result{{Gate: "test", Outcome: gate.Fail, Level: gate.LevelError, Summary: summary}, {Gate: "review", Outcome: gate.Pass, Level: gate.LevelError}}})
}

func (r *sigRig) collect() bool {
	r.t.Helper()
	wrote, err := r.s.Collect(true)
	if err != nil {
		r.t.Fatal(err)
	}
	return wrote
}

func (r *sigRig) noCLI() {
	r.t.Helper()
	if _, err := os.Stat(r.cli); err == nil {
		r.t.Fatalf("收信号调了 CLI：%s", testkit.ReadFile(r.t, r.cli))
	}
}

// 同一原因的门拦截确认两次（一次后来合并了，一次用户放弃）：约束只有一条，两次、带出处，新任务合同末尾带上。
func TestConfirmedBlocksBecomeConstraint(t *testing.T) {
	r := newSigRig(t, nil)
	r.blocked("a1", "web", "fpa", "go test ./internal/x 失败：TestFoo 第 12 行")
	r.meta(&state.Job{ID: "a2", Project: "web", Fingerprint: "fpa", Title: "标题 fpa", ResumeOf: "a1", Gate: &state.GateRecord{State: state.GateMerged, Merge: "abcdef0123456789"}})
	if !r.collect() {
		t.Fatal("有新数据要写")
	}
	cs, err := r.s.LoadConstraints("web")
	if err != nil || len(cs) != 1 || cs[0].Seen != 1 {
		t.Fatalf("第一次确认：%v %+v", err, cs)
	}
	r.now = r.now.Add(2 * time.Hour)
	r.blocked("b1", "web", "fpb", "go test ./internal/y/z 失败：TestFoo 第 40 行")
	r.meta(&state.Job{ID: "b1", Project: "web", Fingerprint: "fpb", Title: "标题 fpb", Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateAbandoned, Head: "fedcba9876543210"}})
	r.collect()
	cs, _ = r.s.LoadConstraints("web")
	if len(cs) != 1 || cs[0].Seen != 2 {
		t.Fatalf("同类要并成一条：%+v", cs)
	}
	note := r.s.ConstraintsNote("web")
	testkit.AssertContains(t, note, constraintsHead, "test 门：go test", "2 次", "任务 a1", "任务 a2", "提交 abcdef012345", "任务 b1（用户放弃）")
	if !strings.HasPrefix(note, "\n\n") {
		t.Errorf("接在合同末尾要空一行：%q", note)
	}
	if r.s.ConstraintsNote("api") != "" {
		t.Error("别的项目没有约束")
	}
	// 再收一次：同样的数据不重复算
	r.collect()
	if cs, _ = r.s.LoadConstraints("web"); cs[0].Seen != 2 {
		t.Fatalf("重读了：%+v", cs)
	}
	r.noCLI()
}

// 用户 gate pass 放行的、再审过了的不学；放行记成 gate-override 信号。
func TestOverrideIsNotConstraint(t *testing.T) {
	r := newSigRig(t, nil)
	r.blocked("c1", "api", "fpc", "lint 报错")
	r.meta(&state.Job{ID: "c1", Project: "api", Fingerprint: "fpc", Title: "标题 fpc", Gate: &state.GateRecord{State: state.GateMerged, Passes: 1, Override: "门误报", Merge: "aaaa"}})
	r.blocked("d1", "api", "fpd", "lint 报错")
	r.meta(&state.Job{ID: "d2", Project: "api", Fingerprint: "fpd", Title: "标题 fpd", Gate: &state.GateRecord{State: state.GateMerged, Reruns: 1, Merge: "bbbb"}})
	r.collect()
	if cs, err := r.s.LoadConstraints("api"); err != nil || len(cs) != 0 {
		t.Fatalf("放行、再审的不学：%v %+v", err, cs)
	}
	st, _ := r.s.LoadSignals()
	var override *Signal
	for i := range st.Signals {
		if st.Signals[i].Kind == KindGateOverride {
			override = &st.Signals[i]
		}
	}
	if override == nil || override.Project != "" || !strings.Contains(override.Summary, "lint 报错") || len(st.Pending) != 0 {
		t.Fatalf("放行要记成 agentq 的信号、等确认的清掉：%+v", st)
	}
}

func TestConstraintsCapAndExpire(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var items []Constraint
	for i := 0; i < constraintMax+2; i++ {
		c := confirmed{project: "web", pending: pendingFail{Gate: "test", Key: fmt.Sprintf("k%d", i), Summary: fmt.Sprintf("原因 %d", i)}, source: []string{fmt.Sprintf("任务 j%d", i)}}
		items = mergeConstraint(items, c, t0.Add(time.Duration(i)*time.Hour))
	}
	if len(items) != constraintMax || items[0].Key != "k11" || items[len(items)-1].Key != "k2" {
		t.Fatalf("超过 %d 条丢最旧的：%+v", constraintMax, items)
	}
	// k5 又出现一次：排到最前，不另写
	items = mergeConstraint(items, confirmed{project: "web", pending: pendingFail{Gate: "test", Key: "k5", Summary: "原因 5"}, source: []string{"任务 again"}}, t0.Add(20*time.Hour))
	if len(items) != constraintMax || items[0].Key != "k5" || items[0].Seen != 2 || len(items[0].Source) != 2 {
		t.Fatalf("同 Key 合并：%+v", items[0])
	}
	// 30 天没再出现的删掉：读的时候就不算，写的时候删
	left := pruneConstraints(items, t0.Add(20*time.Hour+constraintKeepDays*24*time.Hour+time.Minute))
	if len(left) != 0 {
		t.Fatalf("过期要删：%+v", left)
	}
	left = pruneConstraints(items, t0.Add(11*time.Hour+constraintKeepDays*24*time.Hour+time.Minute))
	if len(left) != 1 || left[0].Key != "k5" {
		t.Fatalf("只留没过期的：%+v", left)
	}

	r := newSigRig(t, nil)
	if err := r.s.saveConstraints("web", items); err != nil {
		t.Fatal(err)
	}
	if r.s.ConstraintsNote("web") == "" {
		t.Fatal("没过期要带上")
	}
	r.now = t0.Add(60 * 24 * time.Hour)
	if note := r.s.ConstraintsNote("web"); note != "" {
		t.Fatalf("过期了不带：%s", note)
	}
}

// 同 Key 的 agentq 自身信号两次：写一条提案，只写一次。没配 self_project 写 proposals.md 和当天汇总。
func TestSelfSignalProposalToFile(t *testing.T) {
	r := newSigRig(t, nil)
	for i, id := range []string{"t1", "t2"} {
		r.meta(&state.Job{ID: id, Project: []string{"web", "api"}[i], Status: state.StatusTimeout, Summary: fmt.Sprintf("超过 %d 小时", 4+i)})
	}
	r.collect()
	b, err := os.ReadFile(r.s.ProposalsPath())
	if err != nil || strings.Count(string(b), "agentq 自身：任务超时反复出现") != 1 || !strings.Contains(string(b), "任务 t1（web）") {
		t.Fatalf("要写一条提案：%v\n%s", err, b)
	}
	if day := testkit.ReadFile(t, r.s.NightlyPath(r.now)); !strings.Contains(day, "自我进化：提案「agentq 自身：任务超时反复出现") {
		t.Fatalf("当天汇总要一行：%s", day)
	}
	r.meta(&state.Job{ID: "t3", Project: "web", Status: state.StatusTimeout, Summary: "超过 6 小时"})
	r.collect()
	if b2 := testkit.ReadFile(t, r.s.ProposalsPath()); b2 != string(b) {
		t.Fatalf("同 Key 只提一次：\n%s", b2)
	}
	st, _ := r.s.LoadSignals()
	if len(st.Signals) != 1 || st.Signals[0].Seen != 3 || !st.Signals[0].Proposed {
		t.Fatalf("%+v", st.Signals)
	}
	r.noCLI()
}

func selfRepo(t *testing.T, r *sigRig, name string) backlog.Project {
	t.Helper()
	dir := filepath.Join(r.root, name)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, r.s.Home, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n## 已完成\n"), 0o644)
	testkit.GitT(t, r.s.Home, dir, "add", "-A")
	testkit.GitT(t, r.s.Home, dir, "commit", "-q", "-m", "init")
	return backlog.Project{Name: name, Dir: dir}
}

func (r *sigRig) unknown(id, project string) {
	r.meta(&state.Job{ID: id, Project: project, Gate: &state.GateRecord{State: state.GateMerged, Merge: "cccc"}})
	r.gateResult(id, gate.RunResult{Outcome: "merged", Results: []gate.Result{{Gate: "review", Outcome: gate.Unknown, Level: gate.LevelWarn, Summary: "审查没给结论：输出不是 JSON（第 " + id + " 次）"}}})
}

// 配了 self_project：写进它的「## 建议」，编号走闲时找活的索引，回「做 N」能用；同 Key 只写一次。
func TestSelfSignalProposalToBacklog(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	p := selfRepo(t, r, "agentq")
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	if strings.Count(text, "agentq 自身：门判不了反复出现") != 1 || !strings.Contains(text, "## 建议") || !strings.Contains(text, "（建议 1）") {
		t.Fatalf("要写进「建议」：\n%s", text)
	}
	store, err := r.s.Suggest.LoadSuggest()
	if err != nil || len(store.Items) != 1 || store.Items[0].Project != "agentq" || store.Items[0].N != 1 {
		t.Fatalf("编号要进索引：%v %+v", err, store)
	}
	if log := testkit.GitT(t, r.s.Home, p.Dir, "log", "--oneline"); !strings.Contains(log, "自我进化的提案") {
		t.Fatalf("要提交：%s", log)
	}
	r.unknown("u3", "web")
	r.collect()
	if text2 := testkit.ReadFile(t, p.BacklogPath()); text2 != text {
		t.Fatalf("同 Key 只写一次：\n%s", text2)
	}
	if _, err := os.Stat(r.s.ProposalsPath()); err == nil {
		t.Fatal("配了 self_project 不写 proposals.md")
	}
	r.noCLI()
}

// 用户说过不做的不提。
func TestSelfSignalProposalNotDoing(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	p := selfRepo(t, r, "agentq")
	sig := Signal{Kind: KindGateUnknown, Summary: "review 门判不了：审查没给结论：输出不是 JSON（第 u1 次）"}
	title := makeProposal(&sig).title
	os.WriteFile(p.NotDoingPath(), []byte(backlog.AppendNotDoing("", backlog.NotDoingEntry{Title: title, Quote: "不做", Date: "2026-11-01"})), 0o644)
	testkit.GitT(t, r.s.Home, p.Dir, "add", "-A")
	testkit.GitT(t, r.s.Home, p.Dir, "commit", "-q", "-m", "不做")
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	if text := testkit.ReadFile(t, p.BacklogPath()); strings.Contains(text, "agentq 自身") {
		t.Fatalf("不做清单里的不提：\n%s", text)
	}
	if st, _ := r.s.LoadSignals(); len(st.Signals) != 1 || !st.Signals[0].Proposed {
		t.Fatalf("在不做清单里也算提过，不再试：%+v", st.Signals)
	}
}

// 没有新数据：不写文件、不调 CLI；控频：不满一小时不收，满了再收。
func TestCollectNoNewData(t *testing.T) {
	r := newSigRig(t, nil)
	if r.collect() {
		t.Fatal("什么都没有不该写")
	}
	if _, err := os.Stat(r.s.evolveDir()); err == nil {
		t.Fatal("什么都没有不该建目录")
	}
	r.blocked("a1", "web", "fpa", "失败")
	r.collect()
	before, _ := os.Stat(r.s.SignalsPath())
	raw := testkit.ReadFile(t, r.s.SignalsPath())
	r.now = r.now.Add(3 * time.Hour)
	if r.collect() {
		t.Fatal("没有新数据不该写")
	}
	after, _ := os.Stat(r.s.SignalsPath())
	if testkit.ReadFile(t, r.s.SignalsPath()) != raw || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("没有新数据时文件变了")
	}
	// 控频：新数据来了，不满一小时的 tick 不收
	r.blocked("b1", "web", "fpb", "失败")
	r.now = r.now.Add(-150 * time.Minute) // 离上次收到新数据 30 分钟
	r.s.CollectIfDue()
	if testkit.ReadFile(t, r.s.SignalsPath()) != raw {
		t.Fatal("不满一小时不该收")
	}
	r.now = r.now.Add(time.Hour)
	r.s.CollectIfDue()
	if testkit.ReadFile(t, r.s.SignalsPath()) == raw {
		t.Fatal("满一小时要收")
	}
	r.noCLI()
}

// 开关关掉：不收、不写；约束不进合同，也不再学。
func TestEvolveSwitchesOff(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.EvolveSignalsEnabled = false })
	r.blocked("a1", "web", "fpa", "失败")
	r.meta(&state.Job{ID: "a2", Project: "web", Fingerprint: "fpa", Gate: &state.GateRecord{State: state.GateMerged}})
	r.s.CollectIfDue()
	if r.collect() {
		t.Fatal("关着不收")
	}
	if _, err := os.Stat(r.s.evolveDir()); err == nil {
		t.Fatal("关着不写文件")
	}

	r = newSigRig(t, func(c *config.Config) { c.LearnedConstraintsEnabled = false })
	r.blocked("a1", "web", "fpa", "失败")
	r.meta(&state.Job{ID: "a2", Project: "web", Fingerprint: "fpa", Gate: &state.GateRecord{State: state.GateMerged}})
	r.collect()
	if _, err := os.Stat(filepath.Join(r.s.constraintsDir(), "web.json")); err == nil {
		t.Fatal("约束关着不学")
	}
	if err := r.s.saveConstraints("web", []Constraint{{Project: "web", Key: "k", Text: "test 门：失败", Seen: 1, Last: r.now}}); err != nil {
		t.Fatal(err)
	}
	if note := r.s.ConstraintsNote("web"); note != "" {
		t.Fatalf("约束关着，合同一字不变：%q", note)
	}
}

func TestSignalKey(t *testing.T) {
	a := SignalKey(KindGateFail, "test", "go test ./internal/x 失败：TestFoo 第 12 行，提交 abc1234def")
	b := SignalKey(KindGateFail, "test", "go test ./internal/y/z 失败：TestFoo 第 40 行，提交 9876543ab")
	if a != b {
		t.Fatalf("换了路径、行号、提交要归到一起：\n%s\n%s", a, b)
	}
	if a == SignalKey(KindGateFail, "review", "go test ./internal/x 失败：TestFoo 第 12 行") || a == SignalKey(KindGateFail, "test", "lint 报错") {
		t.Fatal("门名、原因不同不能归到一起")
	}
	long := SignalKey(KindGateFail, "test", strings.Repeat("长", 200))
	if n := len([]rune(strings.TrimPrefix(long, KindGateFail+"|test|"))); n != keyReasonRunes {
		t.Fatalf("原因只取前 %d 字：%d", keyReasonRunes, n)
	}
}

// 巡逻：上次之后新发现的才算；等用户决定、额度、任务失败不算。
func TestPatrolSignals(t *testing.T) {
	r := newSigRig(t, nil)
	t1 := r.now.Add(-time.Hour)
	issues := []map[string]any{
		{"fingerprint": "launchd:x:1", "check": "launchd", "title": "服务 x 退出码 1", "first_seen": t1},
		{"fingerprint": "decide:p", "check": "decide", "title": "等你定", "first_seen": t1},
		{"fingerprint": "disk", "check": "disk", "title": "磁盘只剩 5 GB", "first_seen": t1},
	}
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": issues})
	r.collect()
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": append(issues, map[string]any{"fingerprint": "launchd:x:2", "check": "launchd", "title": "服务 x 退出码 2", "first_seen": r.now})})
	r.collect()
	st, _ := r.s.LoadSignals()
	if len(st.Signals) != 1 || st.Signals[0].Kind != KindPatrol || st.Signals[0].Seen != 2 {
		t.Fatalf("%+v", st.Signals)
	}
	if b, err := os.ReadFile(r.s.ProposalsPath()); err != nil || !strings.Contains(string(b), "巡逻发现问题反复出现") {
		t.Fatalf("巡逻两次也是 agentq 的问题：%v %s", err, b)
	}
}

// 信号文件坏了：命令报错；收集时备份坏文件、从空的开始，不卡住以后每个 tick。
func TestCollectBadSignalsFile(t *testing.T) {
	r := newSigRig(t, nil)
	os.MkdirAll(r.s.evolveDir(), 0o700)
	os.WriteFile(r.s.SignalsPath(), []byte("{坏"), 0o600)
	if _, err := r.s.LoadSignals(); err == nil {
		t.Fatal("坏文件要报错")
	}
	r.blocked("a1", "web", "fpa", "失败")
	r.collect()
	st, err := r.s.LoadSignals()
	if err != nil || len(st.Signals) != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	baks, _ := filepath.Glob(r.s.SignalsPath() + ".bad-*")
	if len(baks) != 1 || testkit.ReadFile(t, baks[0]) != "{坏" {
		t.Fatalf("坏文件要备份：%v", baks)
	}
}

// 放行的任务合并后收尾还会再写 meta.json：再读一次，信号和提案不能多出一类。
func TestOverrideRereadCountsOnce(t *testing.T) {
	r := newSigRig(t, nil)
	merged := func(id, fp, wrapErr string) {
		r.meta(&state.Job{ID: id, Project: "api", Fingerprint: fp, Title: "标题 " + fp, Gate: &state.GateRecord{State: state.GateMerged, Passes: 1, Override: "门误报，" + id, Merge: "aaaa", WrapErr: wrapErr}})
	}
	r.blocked("c1", "api", "fpc", "lint 报错")
	merged("c1", "fpc", "")
	r.blocked("d1", "api", "fpd", "lint 报错")
	merged("d1", "fpd", "")
	r.collect()
	r.now = r.now.Add(2 * time.Hour)
	merged("c1", "fpc", "收尾没写上") // 改了内容、时间也新了
	merged("d1", "fpd", "收尾没写上")
	r.collect()
	st, _ := r.s.LoadSignals()
	var overrides []Signal
	for _, sig := range st.Signals {
		if sig.Kind == KindGateOverride {
			overrides = append(overrides, sig)
		}
	}
	if len(overrides) != 1 || overrides[0].Seen != 2 {
		t.Fatalf("同一次放行只算一次：%+v", overrides)
	}
	if b := testkit.ReadFile(t, r.s.ProposalsPath()); strings.Count(b, "agentq 自身：") != 1 {
		t.Fatalf("只该有一条提案：\n%s", b)
	}
}

// 巡逻每小时重写 patrol.json：没有算 agentq 问题的新发现，不算新数据，不写文件。
func TestPatrolRewriteNoNewData(t *testing.T) {
	r := newSigRig(t, nil)
	old := r.now.Add(-time.Hour)
	issues := []map[string]any{
		{"fingerprint": "launchd:x:1", "check": "launchd", "title": "服务 x 退出码 1", "first_seen": old},
		{"fingerprint": "decide:p", "check": "decide", "title": "等你定", "first_seen": old},
	}
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": issues})
	if !r.collect() {
		t.Fatal("新发现要收")
	}
	raw := testkit.ReadFile(t, r.s.SignalsPath())
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": issues})
	if r.collect() {
		t.Fatal("同样的问题重写一遍不算新数据")
	}
	issues = append(issues, map[string]any{"fingerprint": "decide:q", "check": "decide", "title": "又一条等你定", "first_seen": r.now})
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": issues})
	if r.collect() || testkit.ReadFile(t, r.s.SignalsPath()) != raw {
		t.Fatal("不算 agentq 问题的新发现不该写")
	}
}

// 门结果改名成 last.json 保留写的时间，可能比上次读过的文件都早：按文件记，照样读到。
func TestGateResultOlderThanLastSeen(t *testing.T) {
	r := newSigRig(t, nil)
	r.meta(&state.Job{ID: "z0", Project: "web", Status: state.StatusDone})
	r.collect()
	r.mtime = r.mtime.Add(-30 * time.Minute) // 比上次读过的都早
	r.blocked("a1", "web", "fpa", "失败")
	r.meta(&state.Job{ID: "a2", Project: "web", Fingerprint: "fpa", Gate: &state.GateRecord{State: state.GateMerged, Merge: "abcd"}})
	if !r.collect() {
		t.Fatal("要读到")
	}
	if cs, _ := r.s.LoadConstraints("web"); len(cs) != 1 {
		t.Fatalf("改名后的门结果漏了：%+v", cs)
	}
}
