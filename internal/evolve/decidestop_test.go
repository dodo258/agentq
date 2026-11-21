package evolve

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
)

// parked 造一个停在门上等用户定的任务：res 是门停下那次的结论（last.json）。
func (r *sigRig) parked(id, project, title, report string, res gate.RunResult) {
	r.meta(&state.Job{ID: id, Project: project, Title: title, Status: state.StatusInterrupted,
		Gate: &state.GateRecord{State: state.GateStuck, Branch: "agent/" + id, Report: report}})
	r.gateResult(id, res)
}

var (
	// 11-16 agentq：门都过了，只因为改了评测的尺子（upgrade.go）等人看；用户看出合进最新 main 后题数要改，退回
	rulerHold = gate.RunResult{Outcome: "hold", Reason: "改了评测题、判分或门的判定（internal/evolve/upgrade.go）：评判的尺子不让候选自己改",
		Risk: &gate.RiskVerdict{Level: gate.RiskBroad, Reasons: []string{"跨 4 个顶层目录：docs、evals、internal、tests"}},
		Results: []gate.Result{
			{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError},
			{Gate: "eval", Outcome: gate.Pass, Level: gate.LevelError, Hold: "改了评测题、判分或门的判定（internal/evolve/upgrade.go）"},
			{Gate: "review", Outcome: gate.Pass, Level: gate.LevelError},
		}}
	// 11-21 huitai：审查门三轮都有实质发现，退回用完；用户补了做法再退回
	reviewExhausted = gate.RunResult{Outcome: "fail", Reason: "review：审查发现 1 个有证据的问题：src/cli/daemon.rs:152",
		Results: []gate.Result{
			{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError},
			{Gate: "review", Outcome: gate.Fail, Level: gate.LevelError, Summary: "审查发现 1 个有证据的问题"},
		}}
)

// 两份真实证据：一次是门都过了、改了尺子等人看时用户退回，一次是门自己挡下、退回用完后用户补了方向退回。
// 后一次是照门的结论走（已经记成退回用完），不该和前一次凑成「用户在门卡片上退回或放弃反复出现」给 agentq 写待做。
func TestDecideSignalOnlyWhenGatePassed(t *testing.T) {
	r := newSigRig(t, nil)
	at := r.now.Add(-time.Hour).Format(time.RFC3339)
	r.parked("1116-122432-agentq", "agentq", "升级后任务变差", rulerHold.Reason, rulerHold)
	r.parked("1121-034521-huitai", "huitai", "守护进程接上通知与全局呼出键", "退回 2 轮还没过，多半是计划（条目拆法）的问题", reviewExhausted)
	r.audit(at+" 退回 1116-122432-agentq agentq「升级后任务变差」分支 agent/1116-upgrade-carried-rounds：从最新 main 合入，改题数",
		at+" 退回 1121-034521-huitai huitai「守护进程接上通知与全局呼出键」分支 agent/1121-daemon-notify-hotkey：像 tmux 的 focus-events 那样做")
	r.collect()
	sigs := r.signals(KindUserDecide)
	if len(sigs) != 1 || sigs[0].Seen != 1 || sigs[0].Key != SignalKey(KindUserDecide, "退回", "门都过了，改了评测的尺子") ||
		!strings.Contains(sigs[0].Summary, "门都过了，改了评测的尺子") || !strings.HasPrefix(sigs[0].Evidence[0], "任务 1116-122432-agentq（agentq）退回 ") {
		t.Fatalf("只记门都过了、用户还退回的那次，按停下等人看的原因归：%+v", sigs)
	}
	if p := r.proposals(); strings.Contains(p, "用户在门卡片上退回或放弃反复出现") {
		t.Fatalf("门挡下后照门的结论退回不算，两次凑不成反复出现：\n%s", p)
	}

	// 独立审查代看出疑问（卡片建议的就是退回）照它退回不算；难撤回等人看时放弃的单算一类
	r.parked("h1", "web", "改表结构", "难撤回", gate.RunResult{Outcome: "hold", Reason: "难撤回",
		Risk:    &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"新增行有删数据的语句：db/x.sql（DELETE FROM）"}},
		Results: []gate.Result{{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError}, {Gate: gate.HoldReviewGate, Outcome: gate.Fail, Level: gate.LevelError}}})
	r.parked("h2", "web", "清理旧数据", "难撤回", gate.RunResult{Outcome: "hold", Reason: "难撤回",
		Risk:    &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"新增行有删数据的语句：db/y.sql（DELETE FROM）"}},
		Results: []gate.Result{{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError}}})
	r.audit(at+" 退回 h1 web「改表结构」分支 agent/h1", at+" 放弃 h2 web「清理旧数据」分支 agent/h2 留着")
	r.collect()
	var keys []string
	for _, sig := range r.signals(KindUserDecide) {
		keys = append(keys, sig.Key)
	}
	slices.Sort(keys)
	want := []string{SignalKey(KindUserDecide, "放弃", "门都过了，难撤回"), SignalKey(KindUserDecide, "退回", "门都过了，改了评测的尺子")}
	if !slices.Equal(keys, want) {
		t.Fatalf("按动作和停下的原因归，审查代有疑问照退的不算：\n%q", keys)
	}

	// 同一个原因又来一次：门说没问题、人又看出了问题，这才是反复出现
	r.parked("1122-000000-agentq", "agentq", "改判分", rulerHold.Reason, rulerHold)
	r.audit(at + " 退回 1122-000000-agentq agentq「改判分」分支 agent/x")
	r.collect()
	if p := r.proposals(); !strings.Contains(p, "用户在门卡片上退回或放弃反复出现") {
		t.Fatalf("同一原因两次要提案：\n%s", p)
	}
	r.noCLI()
}

// signals.json 里已经按动作凑成的那条（11-21 提过案）：按现在的规则重判，门挡下的、归到别的原因的证据去掉、不挪过去，
// 信号删掉；没有门记录的照留。
func TestStaleDecideSignalRechecked(t *testing.T) {
	r := newSigRig(t, nil)
	r.parked("1116-122432-agentq", "agentq", "升级后任务变差", rulerHold.Reason, rulerHold)
	r.parked("1121-034521-huitai", "huitai", "守护进程接上通知与全局呼出键", "退回 2 轮还没过", reviewExhausted)
	day := r.now.Add(-24 * time.Hour)
	key := SignalKey(KindUserDecide, "退回", "")
	evs := []string{"任务 1116-122432-agentq（agentq）退回 11-16 13:14", "任务 1121-034521-huitai（huitai）退回 11-21 03:52", "任务 old（web）退回 11-10 09:00"}
	r.write(r.s.SignalsPath(), &SignalStore{Checked: day, AuditAfter: day, ProposedKey: []string{key}, Signals: []Signal{
		{Kind: KindUserDecide, Key: key, Summary: "用户在门卡片上退回了「守护进程接上通知与全局呼出键」", Evidence: evs, Seen: 3, First: day, Last: day, Proposed: true},
	}})
	r.meta(&state.Job{ID: "ok1", Project: "web"})
	r.collect()
	got := r.signals(KindUserDecide)
	if len(got) != 1 || got[0].Seen != 1 || !slices.Equal(got[0].Evidence, evs[2:]) {
		t.Fatalf("按动作凑的证据重判：门挡下的、改归别类的去掉，没门记录的留着：%+v", got)
	}

	// 按新规则记下的证据，门记录后来按保留天数清掉了：对不出停在哪，照留，不当成改归了别类
	r.audit(r.now.Add(-time.Hour).Format(time.RFC3339) + " 退回 1116-122432-agentq agentq「升级后任务变差」分支 agent/x")
	r.collect()
	if err := os.RemoveAll(r.s.Gate.Dir("1116-122432-agentq")); err != nil {
		t.Fatal(err)
	}
	r.meta(&state.Job{ID: "ok2", Project: "web"})
	r.collect()
	if got := r.signals(KindUserDecide); len(got) != 2 {
		t.Fatalf("门记录清掉的证据要留着：%+v", got)
	}
	r.noCLI()
}
