package evolve

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 自己长大开着、self_project 在 exclude_projects 里：自身信号够次数直接写进它的「待做」并提交，不进「建议」；
// sched 能拿到要起的条目，已经有任务做过的不再给。
func TestSelfGrowProposalToTodo(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject, c.ExcludeProjects = "agentq", []string{"agentq"} })
	p := selfRepo(t, r, "agentq")
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	todo, _, _ := strings.Cut(text, "## 已完成")
	if !strings.Contains(todo, "- [ ] agentq 自身：门判不了反复出现") || strings.Contains(text, "## 建议") || !strings.Contains(todo, "加一道能复现它的评测题") {
		t.Fatalf("要写进待做：\n%s", text)
	}
	if log := testkit.GitT(t, r.s.Home, p.Dir, "log", "--oneline"); !strings.Contains(log, "自己长大") {
		t.Fatalf("要提交：%s", log)
	}
	st := &state.State{}
	items := r.s.SelfItemsToStart(st)
	if len(items) != 1 || items[0].Project != "agentq" || !strings.HasPrefix(items[0].Title, "agentq 自身：门判不了反复出现") {
		t.Fatalf("要给 sched 起：%+v", items)
	}
	st.Jobs = append(st.Jobs, &state.Job{ID: "j1", Project: "agentq", Title: items[0].Title, Status: state.StatusRunning})
	if got := r.s.SelfItemsToStart(st); len(got) != 0 {
		t.Fatalf("已经有任务做过的不再起：%+v", got)
	}
	r.s.MarkSelfItem(items[0], "j1", "")
	if got := r.s.SelfItemsToStart(&state.State{}); len(got) != 0 {
		t.Fatalf("记下任务号的不再起：%+v", got)
	}
	r.noCLI()
}

// 修好一次记一次，同一类信号清零；再出现标「修过 1 次又出现」重新写；修过两次还出现写成重写这一块。
func TestSelfGrowFixedTwiceBecomesRewrite(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	p := selfRepo(t, r, "agentq")
	n := 0
	round := func(wantTitle string) string {
		t.Helper()
		n += 2
		r.unknown(fmt.Sprint("u", n), "web")
		r.unknown(fmt.Sprint("u", n+1), "api")
		r.now = r.now.Add(2 * time.Hour)
		r.collect()
		st, _ := r.s.LoadSignals()
		if len(st.SelfItems) != 1 || !strings.Contains(st.SelfItems[0].Title, wantTitle) {
			t.Fatalf("要写一条「%s」：%+v %+v\n%s\n%s", wantTitle, st.SelfItems, st.Signals, testkit.ReadFile(t, p.BacklogPath()), r.s.Out)
		}
		title := st.SelfItems[0].Title
		r.meta(&state.Job{ID: fmt.Sprint("fix", n), Project: "agentq", Title: title, Gate: &state.GateRecord{State: state.GateMerged, Merge: strings.Repeat(string(rune('a'+n/2)), 40)}})
		r.now = r.now.Add(2 * time.Hour)
		r.collect()
		st, _ = r.s.LoadSignals()
		if len(st.SelfItems) != 0 || len(st.Signals) != 0 {
			t.Fatalf("合并了要清掉条目和这一类信号：%+v %+v", st.SelfItems, st.Signals)
		}
		return title
	}
	round("agentq 自身：门判不了反复出现")
	round("agentq 自身（修过 1 次又出现）：门判不了")
	r.unknown("u91", "web")
	r.unknown("u92", "api")
	r.now = r.now.Add(2 * time.Hour)
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	if !strings.Contains(text, "agentq 自身（重写，修过 2 次又出现）：门判不了") || !strings.Contains(text, "type 写 rewrite") || !strings.Contains(text, "合并提交 bbbbbbbbbbbb、cccccccccccc") {
		t.Fatalf("修过两次还出现要写成重写：\n%s", text)
	}
	st, _ := r.s.LoadSignals()
	if k := st.SelfItems[0].Key; len(st.Fixes[k]) != 2 {
		t.Fatalf("修好的次数：%+v", st.Fixes)
	}
}

// 巡逻报漏洞：一次就写进待做（不等第二次），按模块一条。
func TestSelfGrowVulnOnce(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	p := selfRepo(t, r, "agentq")
	r.write(r.s.patrolPath(), map[string]any{"issues": []patrolIssue{{Fingerprint: "vuln:stdlib", Check: "vuln", Title: "agentq 用的 Go v1.26.3 有已知漏洞 GO-2026-0001，v1.26.4 修了", FirstSeen: r.now}}})
	r.collect()
	text := testkit.ReadFile(t, p.BacklogPath())
	if !strings.Contains(text, "- [ ] agentq 自身：Go 或依赖有已知漏洞：agentq 用的 Go v") || !strings.Contains(text, "govulncheck") {
		t.Fatalf("漏洞一次就写进待做：\n%s", text)
	}
}

// 开关关着：和以前一样，没配 self_project 写 proposals.md，不给 sched 条目。
func TestSelfGrowOffKeepsOldPath(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfGrowEnabled = false })
	r.unknown("u1", "web")
	r.unknown("u2", "api")
	r.collect()
	if !strings.Contains(testkit.ReadFile(t, r.s.ProposalsPath()), "门判不了") {
		t.Fatal("要写 proposals.md")
	}
	if got := r.s.SelfItemsToStart(&state.State{}); len(got) != 0 {
		t.Fatalf("关着不给条目：%+v", got)
	}
}

// 升级后变差回滚：一次就写进待做，Key 带提交号，合并后记在 Fixes[RollbackKey] 里（升级据此放行带着它的版本）。
func TestSelfGrowRollbackItem(t *testing.T) {
	r := newSigRig(t, func(c *config.Config) { c.SelfProject = "agentq" })
	p := selfRepo(t, r, "agentq")
	b := strings.Repeat("b", 40)
	r.s.reportRegression(b, "dev-bbbbbbb", "升级前 5 个任务…；升级后 3 个任务，失败或超时 3 个", r.now)
	text := testkit.ReadFile(t, p.BacklogPath())
	if !strings.Contains(text, "- [ ] agentq 自身：升级后任务变差：升级到 dev-bbbbbbb") || !strings.Contains(text, "不会再自动升级") {
		t.Fatalf("回滚了要写进待做：\n%s", text)
	}
	st, _ := r.s.LoadSignals()
	if len(st.SelfItems) != 1 || st.SelfItems[0].Key != RollbackKey(b) {
		t.Fatalf("%+v", st.SelfItems)
	}
	r.meta(&state.Job{ID: "fix1", Project: "agentq", Title: st.SelfItems[0].Title, Gate: &state.GateRecord{State: state.GateMerged, Merge: strings.Repeat("f", 40)}})
	r.now = r.now.Add(2 * time.Hour)
	r.collect()
	st, _ = r.s.LoadSignals()
	if got := st.Fixes[RollbackKey(b)]; len(got) != 1 || got[0] != strings.Repeat("f", 12) {
		t.Fatalf("修它的合并要记下：%+v", st.Fixes)
	}
}
