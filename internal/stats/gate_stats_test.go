package stats

import (
	"bytes"
	"strings"
	"testing"

	"agentq/internal/backend"
	"agentq/internal/state"
)

// 按门统计：次数合得上，warn 档会挡下多少，退回、放行；旧记录没有这些字段也读得出，单独说一句。
func TestStatsByGate(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "a", Gate: &state.GateRecord{State: state.GateMerged, Passes: 1, Tally: map[string]*state.GateTally{
		"test":   {Pass: 2},
		"review": {Pass: 1, Deferred: 2, Usage: &backend.JobUsage{Input: 1000, Output: 500, CostUSD: 3}},
		"ui":     {Fail: 1, WouldBlock: 1},
	}}}, "")
	f.job(state.Job{ID: "b", Gate: &state.GateRecord{State: state.GateReturned, Tally: map[string]*state.GateTally{"test": {Fail: 1}}}}, "")
	f.job(state.Job{ID: "c", Gate: &state.GateRecord{State: state.GateStuck, Reruns: 1, Tally: map[string]*state.GateTally{"review": {Unknown: 1}, "smoke": {Unknown: 2, WouldBlock: 2}}}}, "")
	f.job(state.Job{ID: "d", Gate: &state.GateRecord{State: state.GateAbandoned}}, "") // 升级前的记录，没有次数
	f.job(state.Job{ID: "e"}, "")                                                      // 没写 ready.json
	res, err := f.a.ComputeStats(7)
	if err != nil {
		t.Fatal(err)
	}
	g := res.Gates
	if g.Jobs != 4 || g.Merged != 1 || g.Returned != 1 || g.Stuck != 1 || g.Abandoned != 1 || g.Passes != 1 || g.Reruns != 1 || g.NoTally != 1 {
		t.Errorf("%+v", g)
	}
	if x := g.Tally["test"]; x.Pass != 2 || x.Fail != 1 {
		t.Errorf("%+v", x)
	}
	if x := g.Tally["review"]; x.Pass != 1 || x.Unknown != 1 || x.Deferred != 2 || x.Usage.Input != 1000 {
		t.Errorf("%+v", x)
	}
	var b strings.Builder
	res.print(&b)
	out := b.String()
	for _, want := range []string{"按门（4 次过门记录，合并 1，退回 1，卡住 1，放弃 1，用户放行 1 次，再审 1 次）", "test　通过 2　失败 1　判不了 0", "review　通过 1　失败 0　判不了 1　排回重跑 2　token 1.5K", "ui　通过 0　失败 1　判不了 0　warn 档如果阻塞会挡下 1", "smoke　通过 0　失败 0　判不了 2　warn 档如果阻塞会挡下 2", "1 条记录是升级前跑的"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q：\n%s", want, out)
		}
	}
	gateSection := out[strings.Index(out, "按门（"):]
	gateSection = gateSection[:strings.Index(gateSection, "最后一列是 token 占比")]
	if strings.Contains(gateSection, "$") {
		t.Errorf("门的统计不写费用：\n%s", gateSection)
	}
}

// 没有过门记录时不多一段，输出和原来一样。
func TestStatsNoGates(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "a"}, "")
	if err := f.a.CmdStats(nil); err != nil {
		t.Fatal(err)
	}
	if out := f.a.Out.(*bytes.Buffer).String(); strings.Contains(out, "按门") {
		t.Errorf("%s", out)
	}
}

// meta 里写坏的门统计不崩。
func TestStatsBadTally(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "a", Gate: &state.GateRecord{State: state.GateMerged, Tally: map[string]*state.GateTally{"test": nil}}}, "")
	if _, err := f.a.ComputeStats(7); err != nil {
		t.Fatal(err)
	}
}
