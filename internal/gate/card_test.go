package gate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
)

// 卡片正文不许出现的内部说法：门的英文名、退出码、文件路径和文件名。
var cardJargon = regexp.MustCompile(`(?i)eval|baseline|patch|scope|退出码|[A-Za-z0-9_.~-]+/[A-Za-z0-9_.-]+|\.(?:go|sh|json|md|log)\b`)

// gateNotice 是这个任务的决定卡片那条通知。
func gateNotice(t *testing.T, f *fix, j *state.Job) notify.Notice {
	t.Helper()
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range s.Pending {
		if n.Card != nil && n.Card.GateJob == j.ID {
			return n
		}
	}
	t.Fatalf("没有卡片：%+v", s.Pending)
	return notify.Notice{}
}

// checkPlainCard：正文是大白话，有推荐的选项和一句理由，细节指向 gate report。
func checkPlainCard(t *testing.T, n notify.Notice, j *state.Job, rec string, want ...string) string {
	t.Helper()
	body := strings.Join(n.Lines, "\n")
	text := n.Text + "\n" + body
	if m := cardJargon.FindAllString(body, -1); len(m) > 0 {
		t.Errorf("卡片正文有内部说法 %q：\n%s", m, text)
	}
	if n.Card.Recommend != rec {
		t.Errorf("推荐 %q，要 %q：\n%s", n.Card.Recommend, rec, text)
	}
	var option string
	for _, o := range n.Card.Options {
		if o.Label == rec {
			option = o.Label + " " + o.Text
		}
	}
	if option == "" || !strings.Contains(body, "建议选 "+option+"：") {
		t.Errorf("要写建议选 %s 和理由：\n%s", option, text)
	}
	for _, w := range append(want, "这次改的：", "为什么停：", "细节和原始报错：agentq gate report "+j.ID) {
		if !strings.Contains(text, w) {
			t.Errorf("卡片缺 %q：\n%s", w, text)
		}
	}
	return text
}

// 照任务 1115-234416-agentq：退回用完，最后一次只是评测题现场布置不起来（setup 退出码 1），测试过了。
// 卡片要说是检查出了故障、判不了，建议再审一次；原始报错和日志路径只在 gate report 里。
func TestGateCardCheckBroken(t *testing.T) {
	const why = `评测比基线变差 1 处：001-impl-go：这次判不了：agentq setup 退出码 1：{ "os_version": "27.2.1", "cli_path": "/private/var/folders/ns/T/aq-17542654；耗时 5m25s（构建 0s，评测 5m25s）`
	brokenRun := func(unjudged bool) gate.RunResult {
		return gate.RunResult{Outcome: "fail", Reason: "eval：" + why,
			Risk: &gate.RiskVerdict{Level: gate.RiskBroad, Reasons: []string{"跨 4 个顶层目录：docs、evals、internal、tests"}},
			Results: []gate.Result{
				{Gate: "scope", Outcome: gate.Pass, Level: gate.LevelError, Summary: "这一轮改的 1 个文件都在退回时定的范围里"},
				{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError, Summary: ".agentq/verify 过了", Evidence: []string{"跑了 .agentq/verify，日志 ~/.agentq/gate/x/test.log"}},
				{Gate: "eval", Outcome: gate.Fail, Level: gate.LevelError, Summary: why, Unjudged: unjudged, Evidence: []string{"结论 ~/.agentq/gate/x/eval/verdict.json"}},
			},
			Notes: []string{"patch 门这次按 error 算：影响面广"}}
	}
	setup := func(t *testing.T) (*fix, *state.Job) {
		f := newFix(t, fixBacklog)
		r := ready("agent/x")
		r.Type = "rewrite"
		r.Progress = "巡逻的处置部分重写进 internal/patrol/policy.go（推送、早报、分析、后果关系一处定）；夜班或复盘任务失败引起的缺报告记成那次任务的后果；新评测题 034；第 2 轮只补了越界说明，没改代码"
		j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, r)
		j.Gate.Round = gate.MaxRounds
		return f, j
	}

	f, j := setup(t)
	os.MkdirAll(f.a.JobDir(j.ID), 0o755)
	os.WriteFile(filepath.Join(f.a.JobDir(j.ID), "result.json"), []byte(`{"type":"result","subtype":"success","is_error":false,"result":"改好了。\n\n分支可以合并了，分支名 agent/x；评测门那题是环境问题。","session_id":"s"}`+"\n"), 0o644)
	f.collect(j, brokenRun(true))
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	text := checkPlainCard(t, gateNotice(t, f, j), j, "D", "检查出故障", "不是改动查出了问题", "判不了", "已经过了的：测试", "巡逻的处置部分重写（推送、早报、分析、后果关系一处定）")
	if strings.Contains(text, "门没过") || strings.Contains(text, "改了 2 轮") {
		t.Errorf("判不了不能说成改动没过：\n%s", text)
	}

	// 细节都在 gate report：原始报错、日志路径、任务自己的说明
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", j.ID}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"agentq setup 退出码 1", "verdict.json", "分支 agent/x", "任务最后一轮自己的说明：分支可以合并了，分支名 agent/x；评测门那题是环境问题。"} {
		if !strings.Contains(f.out.String(), w) {
			t.Errorf("报告缺 %q：\n%s", w, f.out.String())
		}
	}

	// 同样的结论，评测题是真的答错了（不是没跑起来）：是改动的问题，退回用完建议放弃重拆
	f2, j2 := setup(t)
	f2.collect(j2, brokenRun(false))
	text = checkPlainCard(t, gateNotice(t, f2, j2), j2, "C", "门没过", "改动本身有问题", "改了 2 轮还没过", "拆小")
	if strings.Contains(text, "判不了") {
		t.Errorf("答错了不是判不了：\n%s", text)
	}
}

// 照任务 1115-235254-agentq：门都过了，但新加的检查脚本注释里有 DELETE FROM（难撤回），又改了门的判定（评测门要人放行）。
// 卡片要说检查都过了、停下是规矩，两处原因用人话说，建议放行并说为什么。
func TestGateCardHold(t *testing.T) {
	f := newFix(t, fixBacklog)
	r := ready("agent/x")
	r.Type = "fix"
	r.Progress = "审查门按问题类别判阻塞：审查结论每条发现写 kind（wrong/crash/data-loss/security/regression/rule/design/note），不是 note 且证据齐的都退回；新加评测题 034"
	j := f.claim("做事", "agent/x", map[string]string{"a.txt": "a"}, r)
	hold := "改了评测题、判分或门的判定（internal/gate/reviewgate.go）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行"
	f.collect(j, gate.RunResult{Outcome: "hold",
		Reason: "难撤回，门过了也不自动合并，等你看过再放行：新增行有删数据的语句：evals/034-review-problem-kind-blocks/check.sh（DELETE FROM）；" + hold,
		Risk:   &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"新增行有删数据的语句：evals/034-review-problem-kind-blocks/check.sh（DELETE FROM）"}},
		Results: []gate.Result{
			{Gate: "patch", Outcome: gate.Fail, Level: gate.LevelWarn, Summary: "第五次打补丁：internal/gate 最近 30 天已修过四次以上，这次又是修复", Files: []string{"internal/gate/reviewgate.go"}},
			{Gate: "test", Outcome: gate.Pass, Level: gate.LevelError, Summary: ".agentq/verify 过了"},
			{Gate: "eval", Outcome: gate.Pass, Level: gate.LevelError, Summary: "评测没有变差：跑了 35 题", Hold: hold},
			{Gate: "review", Outcome: gate.Pass, Level: gate.LevelError, Summary: "没有会阻塞合并的问题"},
		}})
	if j.Gate.State != state.GateStuck || !strings.Contains(j.StuckPending, "等你拍板") {
		t.Fatalf("%+v %s", j.Gate, j.StuckPending)
	}
	text := checkPlainCard(t, gateNotice(t, f, j), j, "A", "等你拍板", "检查都过了（测试、自检考题、代码审查）", "难撤回", "检查自己用的规则", "停下是规矩，不是查出了问题",
		"审查门按问题类别判阻塞：审查结论每条发现写 kind，不是 note 且证据齐的都退回")
	if strings.Contains(text, "DELETE") {
		t.Errorf("命中的原文放报告里：\n%s", text)
	}
}
