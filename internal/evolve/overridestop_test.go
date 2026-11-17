package evolve

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// buttonPass 造一次 Telegram 按钮放行后合并的任务：放行理由都是同一句「Telegram 按钮放行」。
// tally 是这个任务过门时各门的结论；passed 为真时门目录里有门都过了的记录（没有 Hold）；risk 是 last.json 里的风险档位。
func (r *sigRig) buttonPass(id, project, fp string, tally map[string]*state.GateTally, passed bool, risk *gate.RiskVerdict) {
	if passed {
		r.write(filepath.Join(r.s.Gate.Dir(id), "passed-0123456789abcdef.json"), map[string]any{"results": []gate.Result{
			{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError, Summary: ".agentq/verify 过了"},
			{Gate: "review", Outcome: gate.Pass, Level: gate.LevelError, Summary: "按方案实现"},
		}})
	}
	r.meta(&state.Job{ID: id, Project: project, Fingerprint: fp, Title: "标题 " + fp,
		Gate: &state.GateRecord{State: state.GateMerged, Passes: 1, Override: "Telegram 按钮放行", Merge: "abcd" + id, Tally: tally}})
	r.gateResult(id, gate.RunResult{Outcome: "merged", Merge: "abcd" + id, Risk: risk,
		Results: []gate.Result{{Gate: "override", Outcome: gate.Pass, Level: gate.LevelError, Summary: "用户放行：Telegram 按钮放行"}}})
}

var (
	reviewUnknown = map[string]*state.GateTally{"test": {Pass: 1}, "review": {Unknown: 1}}
	allPassed     = map[string]*state.GateTally{"test": {Pass: 1}, "review": {Pass: 1}, "ui": {Pass: 1}}
	truncateRisk  = &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"新增行有删数据的语句：parent-web/src/components/PlaceList.tsx（TRUNCATE）"}}
)

// 11-13 repo-tidy 是审查撞 Cloudflare 403 判不了后放行，11-16 gps-tracker 是门都过了、因为「难撤回」等人看后放行；
// 两次都点的 Telegram 按钮，放行理由是同一句，按理由归类凑成了「反复出现」。放行信号要按门当时停在哪归类。
func TestOverrideKeyedByStopNotReason(t *testing.T) {
	r := newSigRig(t, nil)
	r.buttonPass("1113-180958-repo-tidy", "repo-tidy", "fpa", reviewUnknown, false, &gate.RiskVerdict{Level: gate.RiskBroad, Reasons: []string{"改了 19 个文件（超过 15 个）"}})
	r.buttonPass("1116-142607-gps-tracker", "gps-tracker", "fpb", allPassed, true, truncateRisk)
	r.collect()
	got := overrideSignals(t, r)
	var sums []string
	for _, sig := range got {
		if sig.Seen != 1 {
			t.Fatalf("门停的原因不同，不该因为都点了按钮凑成一类：%+v", sig)
		}
		sums = append(sums, sig.Summary)
	}
	slices.Sort(sums)
	want := []string{"用户放行了review 门拦下的任务：判不了", "用户放行了门拦下的任务：难撤回：新增行有删数据的语句：parent-web/src/components/PlaceList.tsx（TRUNCATE）"}
	if !slices.Equal(sums, want) {
		t.Fatalf("放行信号要写门当时停在哪：\n%q", sums)
	}
	if strings.Contains(strings.Join(sums, ""), "Telegram") {
		t.Fatalf("放行理由不进归类：%q", sums)
	}

	// 同一道门又判不了、又点按钮放行：这才是反复出现
	r.buttonPass("1117-090000-web", "web", "fpc", map[string]*state.GateTally{"review": {Unknown: 2, Deferred: 1}}, false, nil)
	r.collect()
	for _, sig := range overrideSignals(t, r) {
		if strings.Contains(sig.Summary, "review 门") && sig.Seen != 2 {
			t.Fatalf("同一道门判不了后放行两次要归成一类：%+v", sig)
		}
	}
	r.noCLI()
}

// 门没过（error 档）后放行的写「没过」；warn 档没过不挡合并，不算门停在那。升级前没有 Tally 的记录照旧按放行理由。
func TestOverrideStopFromTally(t *testing.T) {
	r := newSigRig(t, nil)
	r.buttonPass("f1", "web", "fpf", map[string]*state.GateTally{"test": {Fail: 1}, "ui": {Fail: 1, WouldBlock: 1}}, false, nil)
	r.buttonPass("o1", "web", "fpo", nil, false, nil)
	r.collect()
	var sums []string
	for _, sig := range overrideSignals(t, r) {
		sums = append(sums, sig.Summary)
	}
	slices.Sort(sums)
	want := []string{"用户放行了test 门拦下的任务：没过", "用户放行了门拦下的任务：Telegram 按钮放行"}
	if !slices.Equal(sums, want) {
		t.Fatalf("按 Tally 认停在哪道门：\n%q", sums)
	}
}

// signals.json 里已经按放行理由凑成的那条（11-16 提过案）：按现在的规则重判，归到别处的证据去掉、不挪过去，信号删掉；
// 没有 Tally 的旧记录对不出停在哪，照留。
func TestStaleButtonOverrideSignalDropped(t *testing.T) {
	r := newSigRig(t, nil)
	r.buttonPass("1113-180958-repo-tidy", "repo-tidy", "fpa", reviewUnknown, false, nil)
	r.buttonPass("1116-142607-gps-tracker", "gps-tracker", "fpb", allPassed, true, truncateRisk)
	r.buttonPass("1110-000000-old", "old", "fpo", nil, false, nil)
	ev := func(id, p string) string { return "任务 " + id + "（" + p + "）" }
	files := map[string]string{}
	for _, id := range []string{"1113-180958-repo-tidy", "1116-142607-gps-tracker", "1110-000000-old"} {
		for _, f := range []string{filepath.Join("jobs", id, "meta.json"), filepath.Join("gate", id, "last.json")} {
			fi, err := os.Stat(filepath.Join(r.s.DataDir(), f))
			if err != nil {
				t.Fatal(err)
			}
			files[f] = fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	day := r.now.Add(-24 * time.Hour)
	key := SignalKey(KindGateOverride, "", "Telegram 按钮放行")
	r.write(r.s.SignalsPath(), &SignalStore{Checked: day, Files: files, ProposedKey: []string{key}, Signals: []Signal{
		{Kind: KindGateOverride, Key: key, Summary: "用户放行了门拦下的任务：Telegram 按钮放行",
			Evidence: []string{ev("1113-180958-repo-tidy", "repo-tidy"), ev("1116-142607-gps-tracker", "gps-tracker"), ev("1110-000000-old", "old")},
			Seen:     3, First: day, Last: day, Proposed: true},
	}})
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	got := overrideSignals(t, r)
	if len(got) != 1 || got[0].Key != key || got[0].Seen != 1 || !slices.Equal(got[0].Evidence, []string{ev("1110-000000-old", "old")}) {
		t.Fatalf("按理由凑的证据去掉，对不出停在哪的旧记录留着，不挪去别的信号：%+v", got)
	}
	r.noCLI()
}
