package evolve

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func (r *sigRig) signals(kind string) []Signal {
	r.t.Helper()
	st, err := r.s.LoadSignals()
	if err != nil {
		r.t.Fatal(err)
	}
	var out []Signal
	for _, sig := range st.Signals {
		if sig.Kind == kind {
			out = append(out, sig)
		}
	}
	return out
}

// unchanged：再收一次，没有新数据，信号文件一个字节都不变。
func (r *sigRig) unchanged() {
	r.t.Helper()
	raw := testkit.ReadFile(r.t, r.s.SignalsPath())
	r.now = r.now.Add(2 * time.Hour)
	if r.collect() {
		r.t.Fatalf("没有新数据不该写：\n%s", testkit.ReadFile(r.t, r.s.SignalsPath()))
	}
	if testkit.ReadFile(r.t, r.s.SignalsPath()) != raw {
		r.t.Fatal("没有新数据时信号文件变了")
	}
}

func (r *sigRig) proposals() string {
	b, _ := os.ReadFile(r.s.ProposalsPath())
	return string(b)
}

// 用户亲手 stop：记成 user-stop，按任务种类归；agentq 自己按信号停的（记中断）不算。两次写提案。
func TestUserStopSignal(t *testing.T) {
	r := newSigRig(t, nil)
	r.meta(&state.Job{ID: "s1", Project: "web", Title: "首页改版", Status: state.StatusKilled, Summary: "被 agentq stop 停止：方向不对"})
	r.meta(&state.Job{ID: "s2", Project: "web", Title: "自动停的", Status: state.StatusInterrupted, AutoStop: "loop"})
	r.collect()
	sigs := r.signals(KindUserStop)
	if len(sigs) != 1 || sigs[0].Seen != 1 || sigs[0].Evidence[0] != "任务 s1（web）「首页改版」" || !strings.Contains(sigs[0].Summary, "方向不对") {
		t.Fatalf("%+v", sigs)
	}
	r.unchanged()
	r.meta(&state.Job{ID: "s3", Project: "api", Title: "接口限流", Status: state.StatusKilled, Summary: "被 agentq stop 停止"})
	r.collect()
	if sigs = r.signals(KindUserStop); len(sigs) != 1 || sigs[0].Seen != 2 || !sigs[0].Proposed {
		t.Fatalf("两次要提案：%+v", sigs)
	}
	if p := r.proposals(); !strings.Contains(p, "agentq 自身：用户亲手停了任务反复出现") || !strings.Contains(p, "任务 s3（api）") {
		t.Fatalf("提案：\n%s", p)
	}
	r.noCLI()
}

func (r *sigRig) audit(lines ...string) {
	r.t.Helper()
	path := filepath.Join(r.s.DataDir(), "gate", "audit.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			r.t.Fatal(err)
		}
	}
}

// 门卡片上的退回、放弃：从审计日志读，按动作归，卡住的原因取自任务记录；放行、再审不算；同一行只算一次，同一秒后写的也收得到。
func TestUserDecideSignal(t *testing.T) {
	r := newSigRig(t, nil)
	at := r.now.Add(-time.Hour).Format(time.RFC3339)
	r.meta(&state.Job{ID: "g1", Project: "web", Title: "首页改版", Status: state.StatusInterrupted, Gate: &state.GateRecord{State: state.GateStuck, Report: "审查门判不了：输出不是 JSON"}})
	r.audit(at+" 放行 g0 web「旧活」分支 agent/g0 提交 abc：没问题",
		at+" 退回 g1 web「首页改版」分支 agent/g1",
		at+" 再审 g2 web「x」分支 agent/g2 提交 abc（第 1 次）",
		"坏行",
		r.now.Add(-40*24*time.Hour).Format(time.RFC3339)+" 放弃 old web「太老」分支 agent/old 留着")
	r.collect()
	sigs := r.signals(KindUserDecide)
	if len(sigs) != 1 || sigs[0].Seen != 1 || !strings.Contains(sigs[0].Summary, "退回了「首页改版」") || !strings.Contains(sigs[0].Summary, "审查门判不了") ||
		!strings.HasPrefix(sigs[0].Evidence[0], "任务 g1（web）退回 ") {
		t.Fatalf("%+v", sigs)
	}
	r.unchanged()
	r.audit(at+" 放弃 g3 api「接口限流」分支 agent/g3 留着", at+" 退回 g4 api「接口文档」分支 agent/g4")
	r.collect()
	sigs = r.signals(KindUserDecide)
	if len(sigs) != 2 {
		t.Fatalf("退回、放弃各一类：%+v", sigs)
	}
	for _, sig := range sigs {
		want := map[string]int{SignalKey(KindUserDecide, "退回", ""): 2, SignalKey(KindUserDecide, "放弃", ""): 1}[sig.Key]
		if sig.Seen != want {
			t.Fatalf("%s：%d 次，要 %d", sig.Key, sig.Seen, want)
		}
	}
	if p := r.proposals(); !strings.Contains(p, "用户在门卡片上退回或放弃反复出现") {
		t.Fatalf("退回两次要提案：\n%s", p)
	}
	r.noCLI()
}

// reworkRepo 建一个真 git 项目：「首页改版」改了 web/App.tsx，合并提交写进「已完成」；返工意见在 BACKLOG 里。
func reworkRepo(t *testing.T, r *sigRig, name, todo string) string {
	t.Helper()
	dir := testkit.MkProject(t, r.root, name, "")
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "web", "App.tsx"), []byte("x"), 0o644)
	testkit.GitT(t, r.s.Home, dir, "add", "-A")
	testkit.GitT(t, r.s.Home, dir, "commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "web", "App.tsx"), []byte("y"), 0o644)
	testkit.GitT(t, r.s.Home, dir, "commit", "-q", "-am", "首页改版")
	head := testkit.GitT(t, r.s.Home, dir, "rev-parse", "HEAD")
	text := "# 待办\n\n## 待做\n\n" + todo + "\n## 已完成\n\n- [x] 首页改版 —— 合并提交 " + head[:12] + "\n- [x] 接口改造 —— 换协议（范围：internal/api/）\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// 返工：「验收 <标题> 的意见」按改动文件、范围、文字归到领域；同一条意见只算一次；正文顺带提到不算。
func TestReworkSignal(t *testing.T) {
	r := newSigRig(t, nil)
	reworkRepo(t, r, "site", "- [ ] 验收 首页改版 的意见 —— 首屏太挤\n- [ ] 验收 接口改造 的意见 —— 返回字段少了\n- [ ] 验收 品牌页 的意见 —— 配色不好看\n- [ ] 加缓存 —— 和「验收 首页改版 的意见」一起看\n")
	r.collect()
	got := map[string]Signal{}
	for _, sig := range r.signals(KindRework) {
		got[sig.Key] = sig
	}
	fe, be, de := got[SignalKey(KindRework, DomainFrontend, "")], got[SignalKey(KindRework, DomainBackend, "")], got[SignalKey(KindRework, DomainDesign, "")]
	if len(got) != 3 || fe.Seen != 1 || be.Seen != 1 || de.Seen != 1 {
		t.Fatalf("前端（改动文件）、后端（原条目范围）、设计（文字）各一次：%+v", got)
	}
	if fe.Evidence[0] != "项目 site「验收 首页改版 的意见」" || !strings.Contains(fe.Summary, "前端返工（按范围和改动文件）") || !strings.Contains(de.Summary, "按意见的文字") {
		t.Fatalf("%+v %+v", fe, de)
	}
	r.unchanged()
	// 认领、合并后移到「已完成」：还是同一条，不再算
	bl := filepath.Join(r.root, "site", "docs", "BACKLOG.md")
	text := testkit.ReadFile(t, bl)
	text = strings.Replace(text, "- [ ] 验收 首页改版 的意见 —— 首屏太挤\n", "", 1)
	text = strings.Replace(text, "- [ ] 验收 接口改造 的意见 —— 返回字段少了", "- [ ] 验收 接口改造 的意见 —— 返回字段少了（进行中：agent/1110-x 11-10 09:00）", 1)
	text = strings.Replace(text, "## 已完成\n\n", "## 已完成\n\n- [x] 验收 首页改版 的意见 —— 首屏太挤；合并提交 abcdef1234567\n", 1)
	if err := os.WriteFile(bl, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	r.unchanged()
	// 另一个项目又一次前端返工：够两次写提案
	reworkRepo(t, r, "shop", "- [ ] 验收 购物车 的意见 —— 按钮被挡住（范围：src/components/Cart.tsx）\n")
	r.collect()
	got = map[string]Signal{}
	for _, sig := range r.signals(KindRework) {
		got[sig.Key] = sig
	}
	if fe = got[SignalKey(KindRework, DomainFrontend, "")]; fe.Seen != 2 || !fe.Proposed {
		t.Fatalf("%+v", fe)
	}
	if p := r.proposals(); !strings.Contains(p, "验收后返工反复出现：前端返工") {
		t.Fatalf("提案：\n%s", p)
	}
	r.noCLI()
}

func TestPathDomain(t *testing.T) {
	for p, want := range map[string]string{
		"web/App.tsx": DomainFrontend, "src/styles/main.css": DomainFrontend, "src/components/": DomainFrontend, "app/lib/x.ts": DomainFrontend,
		"internal/evolve/": DomainBackend, "server/routes.ts": DomainBackend, "main.go": DomainBackend, "api": DomainBackend,
		"docs/design/home.md": DomainDesign, "assets/logo.svg": DomainDesign, "Views/HomeView.swift": DomainFrontend,
		"README.md": DomainDocs, "docs/": DomainDocs, "Models/Ledger.swift": "", "Makefile": "", "": "",
	} {
		if got := pathDomain(p); got != want {
			t.Errorf("%q：%q，要 %q", p, got, want)
		}
	}
}

func (r *sigRig) patrol(issues ...map[string]any) {
	r.write(filepath.Join(r.s.DataDir(), "patrol.json"), map[string]any{"issues": issues})
}

// 巡逻报过、当天又自己消失：疑似误报。昨天报的今天消失、不算 agentq 问题的（等用户决定）消失都不算。
func TestPatrolGoneSignal(t *testing.T) {
	r := newSigRig(t, nil)
	today := r.now.Add(-2 * time.Hour)
	yesterday := r.now.Add(-26 * time.Hour)
	report := map[string]any{"fingerprint": "report:night:1110", "check": "report", "title": "缺夜班报告", "first_seen": today}
	old := map[string]any{"fingerprint": "launchd:x", "check": "launchd", "title": "服务 x 退出码 1", "first_seen": yesterday}
	decide := map[string]any{"fingerprint": "decide:p", "check": "decide", "title": "等你定", "first_seen": today}
	r.patrol(report, old, decide)
	r.collect()
	if len(r.signals(KindPatrolGone)) != 0 {
		t.Fatal("还没消失")
	}
	r.unchanged()
	r.patrol() // 全都消失了
	r.collect()
	sigs := r.signals(KindPatrolGone)
	if len(sigs) != 1 || !strings.Contains(sigs[0].Summary, "缺夜班报告") || !strings.HasPrefix(sigs[0].Evidence[0], "巡逻 report:night:1110 ") {
		t.Fatalf("只有当天报的、算 agentq 问题的：%+v", sigs)
	}
	r.unchanged()
	// 第二天同样的误报又来一次：两次，写提案
	r.now = r.now.Add(24 * time.Hour)
	report["first_seen"] = r.now.Add(-time.Hour)
	report["fingerprint"] = "report:night:1111"
	r.patrol(report)
	r.collect()
	r.patrol()
	r.collect()
	if sigs = r.signals(KindPatrolGone); len(sigs) != 1 || sigs[0].Seen != 2 || !sigs[0].Proposed {
		t.Fatalf("%+v", sigs)
	}
	r.noCLI()
}

// retryRig：项目 web 的一条待做，最近一次任务（停掉的）ended 前结束。
func retryRig(t *testing.T, edit func(c *config.Config), ago time.Duration) (*sigRig, string) {
	t.Helper()
	r := newSigRig(t, edit)
	line := "- [ ] 首页改版 —— 改首屏"
	testkit.MkProject(t, r.root, "web", line)
	ended := r.now.Add(-ago)
	st := &state.State{Jobs: []*state.Job{{ID: "w1", Project: "web", Title: "首页改版", Fingerprint: backlog.Fingerprint(line), Status: state.StatusKilled, StartedAt: ended.Add(-time.Hour), EndedAt: &ended}}}
	if err := state.Save(r.s.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	return r, line
}

// 同一条目被 retry_after 挡住满 6 小时：记一次（按上次怎么结束的归）；不到 6 小时、暂停、已经过了 retry_after 都不算。
func TestRetryWaitSignal(t *testing.T) {
	r, _ := retryRig(t, nil, 7*time.Hour)
	r.collect()
	sigs := r.signals(KindRetryWait)
	if len(sigs) != 1 || sigs[0].Key != SignalKey(KindRetryWait, state.StatusKilled, "") || sigs[0].Evidence[0] != "项目 web「首页改版」任务 w1 之后" ||
		!strings.Contains(sigs[0].Summary, "已经等了 7 小时") {
		t.Fatalf("%+v", sigs)
	}
	r.unchanged()

	for name, c := range map[string]struct {
		edit func(c *config.Config)
		ago  time.Duration
	}{
		"不到 6 小时":        {nil, 5 * time.Hour},
		"暂停":             {func(c *config.Config) { c.Paused = true }, 7 * time.Hour},
		"过了 retry_after": {nil, 13 * time.Hour},
	} {
		r, _ := retryRig(t, c.edit, c.ago)
		if r.collect() {
			t.Errorf("%s：不该有信号：\n%s", name, testkit.ReadFile(t, r.s.SignalsPath()))
		}
	}
}

// 这几类都没有数据（BACKLOG 没有返工意见、审计日志只有放行、巡逻问题没变、条目没被挡）：信号文件不变，不调 CLI。
func TestUserSignalsNoData(t *testing.T) {
	r := newSigRig(t, nil)
	testkit.MkProject(t, r.root, "web", "- [ ] 首页改版 —— 改首屏，验收 首页改版 的意见写在正文里不算")
	r.audit(r.now.Add(-time.Hour).Format(time.RFC3339) + " 放行 g0 web「旧活」分支 agent/g0 提交 abc：没问题")
	if err := state.Save(r.s.StatePath(), &state.State{}); err != nil {
		t.Fatal(err)
	}
	if r.collect() {
		t.Fatalf("没有数据不该写：\n%s", testkit.ReadFile(t, r.s.SignalsPath()))
	}
	if _, err := os.Stat(r.s.evolveDir()); err == nil {
		t.Fatal("没有数据不该建目录")
	}
	r.patrol(map[string]any{"fingerprint": "launchd:x", "check": "launchd", "title": "服务 x 退出码 1", "first_seen": r.now.Add(-time.Hour)})
	r.collect()
	r.patrol(map[string]any{"fingerprint": "launchd:x", "check": "launchd", "title": "服务 x 退出码 1", "first_seen": r.now.Add(-time.Hour)})
	r.unchanged()
	r.noCLI()
}

// agentq evolve signals 看得到新信号的种类、摘要和证据。
func TestCmdSignalsShowsUserSignals(t *testing.T) {
	r, _ := retryRig(t, nil, 7*time.Hour)
	r.meta(&state.Job{ID: "s1", Project: "web", Title: "首页改版", Status: state.StatusKilled, Summary: "被 agentq stop 停止"})
	r.collect()
	out := r.s.Out.(*bytes.Buffer)
	out.Reset()
	if err := r.s.CmdEvolve([]string{"signals"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user-stop", "retry-wait", "证据：任务 s1（web）「首页改版」", "证据：项目 web「首页改版」任务 w1 之后"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("缺 %q：\n%s", want, out)
		}
	}
}
