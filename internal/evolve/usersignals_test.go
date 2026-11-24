package evolve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/gate"
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

// 改需求不算返工：11-23 huitai 用户嫌原条目点名要的 ⌃] 难按、改成 ⌃Q，交付没错，却记成前端、后端返工各一次，
// 和不相干的返工凑成两条「验收后返工反复出现」写进 agentq 的待做。dispatch 在标题末尾标（改需求）的，不记信号、不进按领域的账；
// 没标的照算，摘要里别的项目的意见原文放在引号里。
func TestReworkChangeRequestNotCounted(t *testing.T) {
	r := newSigRig(t, nil)
	reworkRepo(t, r, "site", "- [ ] 验收 首页改版 的意见（改需求） —— 用户嫌按钮太小，改大一号（范围：web/）\n"+
		"- [x] 验收 首页改版 的意见（改需求） —— 再加一个暗色主题；合并提交 abcdef1234567\n")
	reworkRepo(t, r, "shop", "- [ ] 验收 首页改版 的意见 —— 手机宽度下按钮被挡住，没做到原验收\n")
	r.collect()
	sigs := r.signals(KindRework)
	if len(sigs) != 1 || sigs[0].Seen != 1 || sigs[0].Evidence[0] != "项目 shop「验收 首页改版 的意见」" {
		t.Fatalf("只有 shop 那条没标（改需求）的算返工：%+v", sigs)
	}
	if want := "前端返工（按范围和改动文件），最近一次：项目 shop「验收 首页改版 的意见」，意见原文「手机宽度下按钮被挡住，没做到原验收」"; sigs[0].Summary != want {
		t.Fatalf("摘要：\n%s\n要\n%s", sigs[0].Summary, want)
	}
	st, err := r.s.LoadSignals()
	if err != nil {
		t.Fatal(err)
	}
	var evs []DomainEvent
	for _, ev := range st.Domains {
		if ev.Kind == evRework {
			evs = append(evs, ev)
		}
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Evidence, "shop") {
		t.Fatalf("按领域的账只记 shop 那条返工：%+v", evs)
	}
	if p := r.proposals(); strings.Contains(p, "返工") {
		t.Fatalf("一次返工不该提案：\n%s", p)
	}
	r.noCLI()
}

// 合并提交顺手改的记录文件不算文档：11-16 两条不相干的返工（官网的样式、Flutter App 的密码页）因为合并提交都改了 DECISIONS.md，
// 凑成了「文档返工反复出现」。
func TestReworkRecordFilesNotDocs(t *testing.T) {
	r := newSigRig(t, nil)
	dir := testkit.MkProject(t, r.root, "tracker", "")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "lib", "app_lock.dart"), []byte("x"), 0o644)
	testkit.GitT(t, r.s.Home, dir, "add", "-A")
	testkit.GitT(t, r.s.Home, dir, "commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "lib", "app_lock.dart"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(dir, "DECISIONS.md"), []byte("# 决定\n密码页的取舍\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# 变更\n- 密码页\n"), 0o644)
	testkit.GitT(t, r.s.Home, dir, "add", "-A")
	testkit.GitT(t, r.s.Home, dir, "commit", "-q", "-m", "密码页重做")
	head := testkit.GitT(t, r.s.Home, dir, "rev-parse", "HEAD")
	text := "# 待办\n\n## 待做\n\n- [ ] 验收 密码页重做 的意见 —— 改密码、关密码要有明确入口\n\n## 已完成\n\n- [x] 密码页重做 —— 合并提交 " + head[:12] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	r.collect()
	for _, sig := range r.signals(KindRework) {
		if sig.Key == SignalKey(KindRework, DomainDocs, "") {
			t.Fatalf("合并提交只顺手改了记录文件，不该记成文档返工：%+v", sig)
		}
	}
	if len(r.signals(KindRework)) != 1 {
		t.Fatalf("意见本身照记一次：%+v", r.signals(KindRework))
	}
	r.noCLI()
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

// 亲手派的任务启动 3 分钟内（含）被停：单算 quick-stop，一次就写提案，证据带谁停的和停时额度；超过 3 分钟、不是亲手派的照旧是 user-stop。
func TestQuickStopSignal(t *testing.T) {
	r := newSigRig(t, nil)
	start := r.now.Add(-time.Hour)
	stopped := func(id string, manual bool, after time.Duration, via, usage string) {
		end := start.Add(after)
		r.meta(&state.Job{ID: id, Project: "web", Title: "条目 " + id, Manual: manual, Status: state.StatusKilled, StartedAt: start, EndedAt: &end,
			Summary: "被 agentq stop 停止（SIGTERM）", StopVia: via, StopUsage: usage})
	}
	stopped("q1", true, 7*time.Second, state.StopViaCLI, "到了后台上限（会话 72%，后台上限 70%）")
	stopped("q2", true, quickStopWithin, state.StopViaTelegram, "没到后台上限（会话 40%）")
	stopped("q3", true, quickStopWithin+time.Second, state.StopViaCLI, "")
	stopped("q4", false, 10*time.Second, state.StopViaCLI, "")
	r.collect()
	quick := r.signals(KindQuickStop)
	if len(quick) != 1 || quick[0].Seen != 2 || !quick[0].Proposed {
		t.Fatalf("7 秒、正好 3 分钟各算一次，一次就提案：%+v", quick)
	}
	ev := strings.Join(quick[0].Evidence, "\n")
	for _, want := range []string{"任务 q1（web）「条目 q1」启动 7 秒后被停（命令行），停时到了后台上限（会话 72%，后台上限 70%）", "任务 q2（web）「条目 q2」启动 3 分钟后被停（Telegram），停时没到后台上限"} {
		if !strings.Contains(ev, want) {
			t.Errorf("证据缺「%s」：\n%s", want, ev)
		}
	}
	if stops := r.signals(KindUserStop); len(stops) != 1 || stops[0].Seen != 2 || strings.Contains(strings.Join(stops[0].Evidence, " "), "q1") {
		t.Fatalf("超过 3 分钟、不是亲手派的才是 user-stop：%+v", stops)
	}
	if p := r.proposals(); !strings.Contains(p, "亲手派的任务刚启动就被停") || !strings.Contains(p, "被停（命令行）") {
		t.Fatalf("提案：\n%s", p)
	}
	r.unchanged()
	// 老记录没记来源和额度：照样算，写明没记
	stopped("q5", true, time.Minute, "", "")
	r.collect()
	if quick = r.signals(KindQuickStop); quick[0].Seen != 3 || !strings.Contains(quick[0].Summary, "被停（来源没记），停时额度没记") {
		t.Fatalf("%+v", quick)
	}
	r.noCLI()
}

// 用户停了任务、30 分钟内在同一个项目亲手再派一条（改了条目重派、换一条替它）：是改需求，不记 user-stop。
// 停了不到 30 分钟又没重派的先不收，过了 30 分钟还没重派才记；后来自动接的、别的项目派的、超过 30 分钟才派的都不算重派。
func TestUserStopRedoNotSignal(t *testing.T) {
	r := newSigRig(t, nil)
	at := func(d time.Duration) *time.Time { x := r.now.Add(d); return &x }
	stop := func(id, project string, start, end time.Duration) {
		r.meta(&state.Job{ID: id, Project: project, Title: "条目 " + id, Manual: true, Status: state.StatusKilled,
			StartedAt: *at(start), EndedAt: at(end), Summary: "被 agentq stop 停止（SIGTERM）"})
	}
	run := func(id, project string, start time.Duration, manual bool) {
		r.meta(&state.Job{ID: id, Project: project, Title: "条目 " + id, Manual: manual, StartedAt: *at(start), EndedAt: at(start + 10*time.Minute)})
	}
	stop("改了重派", "web", -3*time.Hour, -170*time.Minute)
	run("重派", "web", -169*time.Minute, true)
	stop("换一条", "api", -2*time.Hour, -110*time.Minute)
	run("替它的", "api", -110*time.Minute+redoWithin, true)
	stop("自动接的不算", "ios", -3*time.Hour, -170*time.Minute)
	run("tick 挑的", "ios", -169*time.Minute, false)
	run("别的项目", "ops", -169*time.Minute, true)
	stop("太晚才派", "cli", -3*time.Hour, -170*time.Minute)
	run("隔了很久", "cli", -170*time.Minute+redoWithin+time.Second, true)
	stop("刚停", "web", -20*time.Minute, -10*time.Minute)
	r.collect()
	got := ""
	for _, sig := range r.signals(KindUserStop) {
		got += strings.Join(sig.Evidence, "\n")
	}
	for _, want := range []string{"自动接的不算", "太晚才派"} {
		if !strings.Contains(got, want) {
			t.Errorf("没重派要记「%s」：\n%s", want, got)
		}
	}
	for _, not := range []string{"改了重派", "换一条", "刚停"} {
		if strings.Contains(got, not) {
			t.Errorf("「%s」不该记：\n%s", not, got)
		}
	}
	// 刚停的：过了 30 分钟还是没人重派，下次收集记上
	r.now = r.now.Add(time.Hour)
	if !r.collect() {
		t.Fatal("先不收的那条下次要再看")
	}
	if sigs := r.signals(KindUserStop); len(sigs) != 1 || sigs[0].Seen != 3 || !strings.Contains(strings.Join(sigs[0].Evidence, " "), "刚停") {
		t.Fatalf("%+v", sigs)
	}
	r.unchanged()
	r.noCLI()
}

// 11-10 到 11-15 那条 user-stop 是按旧规则记的：两次停完几分钟内亲手重派（改需求），一次启动 7 秒就停（现在归 quick-stop）。
// 每次收信号按现在的规则重判按任务记录判的信号：这些证据从 user-stop 去掉，一份不剩的删掉；真停的、刚启动就停的照留。
func TestStaleStopSignalRechecked(t *testing.T) {
	r := newSigRig(t, nil)
	at := func(d time.Duration) *time.Time { x := r.now.Add(d); return &x }
	stopped := func(id, project string, start, end time.Duration) {
		r.meta(&state.Job{ID: id, Project: project, Title: "条目 " + id, Manual: true, Status: state.StatusKilled,
			StartedAt: *at(start), EndedAt: at(end), Summary: "被 agentq stop 停止（SIGTERM）"})
	}
	day := -48 * time.Hour
	stopped("改了重派", "web", day, day+6*time.Minute)
	r.meta(&state.Job{ID: "重派", Project: "web", Manual: true, StartedAt: *at(day + 7*time.Minute), EndedAt: at(day + 30*time.Minute)})
	stopped("七秒", "app", day, day+7*time.Second)
	stopped("真停", "api", day, day+10*time.Minute)
	stopped("只剩重派", "cli", day, day+5*time.Minute)
	r.meta(&state.Job{ID: "替它", Project: "cli", Manual: true, StartedAt: *at(day + 9*time.Minute), EndedAt: at(day + 20*time.Minute)})
	ev := func(id, project string) string {
		return "任务 " + id + "（" + project + "）「条目 " + id + "」"
	}
	r.collect() // 先按现在的规则读一遍任务记录，再换成按旧规则记下的信号
	st := r.store()
	when := r.now.Add(day)
	userStop := SignalKey(KindUserStop, "待做", "")
	st.Signals = []Signal{
		{Kind: KindUserStop, Key: userStop, Summary: "用户停了待做任务", Evidence: []string{ev("改了重派", "web"), ev("七秒", "app"), ev("真停", "api")},
			Seen: 3, First: when, Last: when, Proposed: true},
		{Kind: KindUserStop, Key: SignalKey(KindUserStop, "新项目", ""), Summary: "用户停了新项目任务", Evidence: []string{ev("只剩重派", "cli")}, Seen: 1, First: when, Last: when},
		{Kind: KindQuickStop, Key: SignalKey(KindQuickStop, "", ""), Summary: "亲手派的刚启动就被停", Evidence: []string{ev("七秒", "app") + "启动 7 秒后被停"}, Seen: 1, First: when, Last: when, Proposed: true},
	}
	r.write(r.s.SignalsPath(), st)
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "新数据", Project: "ops"})
	r.collect()
	stops := r.signals(KindUserStop)
	if len(stops) != 1 || stops[0].Key != userStop || stops[0].Seen != 1 || !slices.Equal(stops[0].Evidence, []string{ev("真停", "api")}) {
		t.Fatalf("user-stop 只剩真停的那份，只剩重派的那条删掉：%+v", stops)
	}
	if quick := r.signals(KindQuickStop); len(quick) != 1 || quick[0].Seen != 1 {
		t.Fatalf("刚启动就停的照留：%+v", quick)
	}
	r.noCLI()
}

// 11-16 到 11-18 那条 user-stop 的三份证据：两次是派活的会话把只评估的建议写进了待做，tick 接了，用户停掉后一分钟内
// 移到「以后再说」（撤回）；一次是让路给别的项目，之后去掉标记接着做完、合并进「已完成」。停的那一条已经不在「待做」里的
// 都是改计划，不记，按旧规则记过的每次收信号去掉；还留在「待做」里的、不是待做任务的、看不到 BACKLOG 的照记。
func TestUserStopItemGoneNotSignal(t *testing.T) {
	r := newSigRig(t, nil)
	at := func(d time.Duration) *time.Time { x := r.now.Add(d); return &x }
	dir, home := t.TempDir(), os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	backlogText := func(todo, later, done string) { // 写进 BACKLOG 并提交：停的那一条在不在 git 历史里要看提交
		t.Helper()
		text := "# 待办\n\n## 待做\n\n" + todo + "\n## 以后再说\n\n" + later + "\n## 已完成\n\n" + done
		if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		testkit.GitT(t, home, dir, "add", "-A")
		testkit.GitT(t, home, dir, "commit", "-q", "-m", "待办")
	}
	line := func(title string) string { return title + " —— 正文" }
	stopped := func(id, title, kind, projectDir string, run time.Duration) {
		r.meta(&state.Job{ID: id, Project: "reader", ProjectDir: projectDir, Title: title, Kind: kind, Fingerprint: backlog.Fingerprint("- [ ] " + line(title)),
			Status: state.StatusKilled, StartedAt: *at(-2*time.Hour - run), EndedAt: at(-2 * time.Hour), Summary: "被 agentq stop 停止（SIGTERM）"})
	}
	stopped("撤回", "滚动误呼菜单", "", dir, 3*time.Minute)
	stopped("十秒撤回", "左边缘滑动退出", "", dir, 10*time.Second)
	stopped("让路", "门停住先审查", "", dir, 15*time.Minute)
	stopped("还在待做", "书名识别", "", dir, 5*time.Minute)
	stopped("没有目录", "旧任务", "", "", 5*time.Minute)
	stopped("读不了", "别的项目", "", filepath.Join(dir, "没有这个目录"), 5*time.Minute)
	stopped("新项目", "门停住先审查", state.KindNew, dir, 5*time.Minute)
	stopped("从没待过", "加日志", "", dir, 5*time.Minute)
	// 接活时四条都在待做；之后两条撤到「以后再说」（再删掉也一样）、一条接着做完合并、一条停着没人管
	backlogText("- [ ] "+line("滚动误呼菜单")+"\n- [ ] "+line("左边缘滑动退出")+"\n- [ ] "+line("门停住先审查")+"\n- [ ] "+line("书名识别")+"\n", "", "")
	backlogText(
		"- [ ] "+line("书名识别")+"（进行中：agent/1118-title 2026-11-18 18:00）\n",
		"- [ ] "+line("滚动误呼菜单")+"\n- [ ] "+line("左边缘滑动退出")+"\n",
		"- [x] "+line("门停住先审查")+"；合并提交 abcdef123456\n")
	r.collect()
	ids := func() string {
		var out []string
		for _, sig := range r.signals(KindUserStop) {
			for _, ev := range sig.Evidence {
				id, _ := evidenceJob(ev)
				out = append(out, id)
			}
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	if got, want := ids(), "从没待过 新项目 没有目录 读不了 还在待做"; got != want {
		t.Fatalf("user-stop 证据 %q，要 %q", got, want)
	}

	// 按旧规则记过的：三份真实形状的证据和还在待做的那份。下次收信号时撤回、让路的去掉，还在待做的留着。
	st := r.store()
	key := SignalKey(KindUserStop, "待做", "")
	ev := func(id, title string) string { return "任务 " + id + "（reader）「" + title + "」" }
	when := r.now.Add(-2 * time.Hour)
	st.Signals = slices.DeleteFunc(st.Signals, func(sig Signal) bool { return sig.Kind == KindUserStop })
	st.Signals = append(st.Signals, Signal{Kind: KindUserStop, Key: key, Summary: "用户停了待做任务",
		Evidence: []string{ev("让路", "门停住先审查"), ev("十秒撤回", "左边缘滑动退出"), ev("撤回", "滚动误呼菜单"), ev("还在待做", "书名识别")},
		Seen:     4, First: when, Last: when, Proposed: true})
	r.write(r.s.SignalsPath(), st)
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "新数据", Project: "ops"})
	r.collect()
	if sigs := r.signals(KindUserStop); len(sigs) != 1 || sigs[0].Seen != 1 || !slices.Equal(sigs[0].Evidence, []string{ev("还在待做", "书名识别")}) {
		t.Fatalf("只剩还在待做的那份：%+v", sigs)
	}

	// 还在待做的那条后来也撤回、删掉了：再收一次，整条信号删掉
	backlogText("", "", "")
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "又一条新数据", Project: "ops"})
	r.collect()
	if sigs := r.signals(KindUserStop); len(sigs) != 0 {
		t.Fatalf("一份不剩要删掉：%+v", sigs)
	}
	r.noCLI()
}

// 11-20 那次 quick-stop：派活的会话亲手 run 了一条，用户同一秒说「别做了」，会话 5 秒后 stop、十几秒后把那一条撤出待做。
// 是用户改了主意，不是说明带偏，不记 quick-stop；按旧规则记过的下次收信号去掉。停了还留在待做里的照记（11-15 那次的形状）。
func TestQuickStopItemGoneNotSignal(t *testing.T) {
	r := newSigRig(t, nil)
	at := func(d time.Duration) *time.Time { x := r.now.Add(d); return &x }
	dir, home := t.TempDir(), os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
	backlogText := func(todo string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n"+todo+"\n## 已完成\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		testkit.GitT(t, home, dir, "add", "-A")
		testkit.GitT(t, home, dir, "commit", "-q", "-m", "待办")
	}
	line := func(title string) string { return "- [ ] " + title + " —— 正文" }
	stopped := func(id, title string) {
		r.meta(&state.Job{ID: id, Project: "tracker", ProjectDir: dir, Title: title, Fingerprint: backlog.Fingerprint(line(title)), Manual: true,
			Status: state.StatusKilled, StartedAt: *at(-time.Hour), EndedAt: at(-time.Hour + 5*time.Second),
			Summary: "被 agentq stop 停止（SIGTERM）", StopVia: state.StopViaCLI, StopUsage: "没到后台上限（会话 6%，后台上限 85%）"})
	}
	backlogText(line("移植专用版") + "\n" + line("加导出") + "\n")
	stopped("改主意", "移植专用版")
	stopped("说明带偏", "加导出")
	backlogText(line("加导出") + "（进行中：agent/1120-export 2026-11-20 01:34）\n")
	r.collect()
	quick := r.signals(KindQuickStop)
	if len(quick) != 1 || quick[0].Seen != 1 || !strings.Contains(quick[0].Evidence[0], "任务 说明带偏（tracker）") || !quick[0].Proposed {
		t.Fatalf("只记还留在待做的那条，一次就提案：%+v", quick)
	}
	if stops := r.signals(KindUserStop); len(stops) != 0 {
		t.Fatalf("刚启动就停的不进 user-stop：%+v", stops)
	}

	// 按旧规则记过的：撤走那条的证据下次收信号去掉，还在待做的那份留着
	st := r.store()
	st.Signals[slices.IndexFunc(st.Signals, func(s Signal) bool { return s.Kind == KindQuickStop })].Evidence = []string{
		"任务 改主意（tracker）「移植专用版」启动 5 秒后被停", "任务 说明带偏（tracker）「加导出」启动 5 秒后被停"}
	st.Signals[slices.IndexFunc(st.Signals, func(s Signal) bool { return s.Kind == KindQuickStop })].Seen = 2
	r.write(r.s.SignalsPath(), st)
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "新数据", Project: "ops"})
	r.collect()
	if quick = r.signals(KindQuickStop); len(quick) != 1 || quick[0].Seen != 1 || !strings.Contains(quick[0].Evidence[0], "说明带偏") {
		t.Fatalf("撤走的那份要去掉：%+v", quick)
	}
	r.noCLI()
}

// 11-16 到 11-21 那条「退回用完还没过反复出现」的三份证据：一次是检查自己出了故障（评测题布置不起来），一次是门都过了只因
// 难撤回等人拍板，只有一次是改动真没过。只有门写了「退回 N 轮还没过，多半是计划……」的才记成 plan，摘要就是门的报告，不再包一层；
// 按旧规则记过的证据每次收信号重判，不算的去掉。
func TestPlanSignalOnlyChangeStops(t *testing.T) {
	r := newSigRig(t, nil)
	plan := fmt.Sprintf("退回 %d 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收。最后一次：review：审查发现 1 个有证据的问题", gate.MaxRounds)
	stuck := func(id, project, report string) {
		r.meta(&state.Job{ID: id, Project: project, Title: "条目 " + id, Status: state.StatusInterrupted,
			Gate: &state.GateRecord{State: state.GateStuck, Branch: "agent/" + id, Round: gate.MaxRounds, Report: report}})
	}
	stuck("真没过", "huitai", plan)
	stuck("等拍板", "huitai", "难撤回，门过了也不自动合并，等你看过再放行：路径命中 *.sql：tests/fixtures/codex/state.sql")
	stuck("判不了", "agentq", "eval：评测比基线变差 1 处：001-impl-go：这次判不了：agentq setup 退出码 1")
	r.collect()
	plans := r.signals(KindPlan)
	if len(plans) != 1 || plans[0].Seen != 1 || !slices.Equal(plans[0].Evidence, []string{"任务 真没过（huitai）"}) {
		t.Fatalf("只有改动真没过的记成 plan：%+v", plans)
	}
	if plans[0].Summary != plan {
		t.Errorf("摘要就是门的报告，不再包一层：%s", plans[0].Summary)
	}
	if stucks := r.signals(KindStuck); len(stucks) != 2 {
		t.Errorf("另两次照旧记成卡在门上（项目自己的事）：%+v", stucks)
	}
	if strings.Contains(r.proposals(), "退回用完") {
		t.Errorf("一次不写提案：\n%s", r.proposals())
	}

	// 按旧规则记下的：三份凑成一条 plan，重判后只剩真没过的那份
	st := r.store()
	st.Signals = append(slices.DeleteFunc(st.Signals, func(sig Signal) bool { return sig.Kind == KindPlan }),
		Signal{Kind: KindPlan, Key: SignalKey(KindPlan, "", ""), Summary: "退回 2 轮还没过，多半是条目拆法或验收的问题：" + plan,
			Evidence: []string{"任务 判不了（agentq）", "任务 真没过（huitai）", "任务 等拍板（huitai）"}, Seen: 3, First: r.now, Last: r.now, Proposed: true})
	r.write(r.s.SignalsPath(), st)
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "新数据", Project: "ops"})
	r.collect()
	if plans := r.signals(KindPlan); len(plans) != 1 || plans[0].Seen != 1 || !slices.Equal(plans[0].Evidence, []string{"任务 真没过（huitai）"}) {
		t.Fatalf("重判后只剩真没过的那份：%+v", plans)
	}
	r.noCLI()
}

// 照任务 1123-000756-huitai：冲突不占轮数以前的版本，最后一轮只因合并冲突也写成「退回用完」，任务记录里只有 merge 的结论。
// 没有一道检查看过改动，不是条目拆法的问题：收的时候不记，以前记下的重判时去掉。
func TestPlanSignalConflictOnlyNotPlan(t *testing.T) {
	r := newSigRig(t, nil)
	plan := fmt.Sprintf("退回 %d 轮还没过，多半是计划（条目拆法）的问题：建议重新拆或改验收。最后一次：", gate.MaxRounds)
	stuck := func(id, report string, tally map[string]*state.GateTally) {
		r.meta(&state.Job{ID: id, Project: "huitai", Title: "条目 " + id, Status: state.StatusInterrupted,
			Gate: &state.GateRecord{State: state.GateStuck, Branch: "agent/" + id, Round: gate.MaxRounds, Report: report, Tally: tally}})
	}
	stuck("只冲突", plan+"和最新 main 有冲突：AGENTS.md", map[string]*state.GateTally{"merge": {Fail: 1}})
	stuck("审查没过", plan+"review：审查发现 1 个有证据的问题", map[string]*state.GateTally{"test": {Pass: 1}, "review": {Fail: 1}})
	stuck("冲突后没过", plan+"review：审查发现 1 个有证据的问题", map[string]*state.GateTally{"merge": {Fail: 1}, "review": {Fail: 1}})
	r.collect()
	want := []string{"任务 冲突后没过（huitai）", "任务 审查没过（huitai）"}
	got := func() []string {
		plans := r.signals(KindPlan)
		if len(plans) != 1 {
			t.Fatalf("%+v", plans)
		}
		return slices.Sorted(slices.Values(plans[0].Evidence))
	}
	if ev := got(); !slices.Equal(ev, want) {
		t.Fatalf("%v", ev)
	}

	// 旧规则记下的：重判时去掉只冲突的那份
	st := r.store()
	i := slices.IndexFunc(st.Signals, func(sig Signal) bool { return sig.Kind == KindPlan })
	st.Signals[i].Evidence = append(st.Signals[i].Evidence, "任务 只冲突（huitai）")
	st.Signals[i].Seen++
	r.write(r.s.SignalsPath(), st)
	r.now = r.now.Add(2 * time.Hour)
	r.meta(&state.Job{ID: "新数据", Project: "ops"})
	r.collect()
	if ev := got(); !slices.Equal(ev, want) {
		t.Fatalf("%v", ev)
	}
}
