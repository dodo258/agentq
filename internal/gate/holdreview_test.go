package gate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backend"
	"agentq/internal/gate"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// holdReply 是独立审查代的一次输出。
func holdReply(verdict, summary string, findings ...map[string]any) string {
	if findings == nil {
		findings = []map[string]any{}
	}
	s, _ := json.Marshal(map[string]any{"verdict": verdict, "summary": summary, "findings": findings})
	b, _ := json.Marshal(testkit.FakeEvent{Type: "result", OK: true, Text: "看完了", Structured: s})
	return string(b) + "\n"
}

// newHoldFix：假后端只给独立审查代用，审查门关着（它自己的用例在 reviewgate_test.go）。
func newHoldFix(t *testing.T, replies ...string) *reviewFix {
	t.Helper()
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), replies...)
	os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(`{"levels":{"review":"off"}}`), 0o644)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "审查门关着")
	return f
}

// sqlInComment：新增行里有 DELETE FROM，但只在注释里，是误报。
var sqlInComment = map[string]string{"clean.sh": "#!/bin/sh\n# 以前这里跑过 DELETE FROM jobs，现在不删了，只打印\necho ok\n"}

// rulerGate：替评测门说「改了尺子」，门本身过了。
func rulerGate(t *testing.T) {
	t.Helper()
	register(t, fakeGate{name: "尺子替身", applies: true, run: func(gate.Candidate) gate.Result {
		return gate.Result{Outcome: gate.Pass, Summary: "评测没变差",
			Hold: "改了评测题、判分或门的判定（internal/gate/testgate.go）：评判的尺子不让候选自己改，门过了也不自动合并，等你看过再放行", Ruler: true}
	}}, gate.LevelError)
}

func gateCard(t *testing.T, f *reviewFix, id string) notify.Notice {
	t.Helper()
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range s.Pending {
		if n.Card != nil && n.Card.GateJob == id {
			return n
		}
	}
	t.Fatalf("没有决定卡片：%+v", s.Pending)
	return notify.Notice{}
}

func (f *reviewFix) report(id string) string {
	f.t.Helper()
	if err := state.Save(f.a.StatePath(), f.st); err != nil {
		f.t.Fatal(err)
	}
	f.out.Reset()
	if err := f.a.CmdGate([]string{"report", id}); err != nil {
		f.t.Fatal(err)
	}
	return f.out.String()
}

func (f *reviewFix) audit() string {
	b, _ := os.ReadFile(filepath.Join(f.a.DataDir(), "gate", "audit.log"))
	return string(b)
}

// 误报放行：删数据的语句只在注释里，独立审查代看过说放行，就自动合并；审计行、报告、完成通知都看得出是它放行的，通知不带按钮。
func TestHoldReviewReleasesFalseAlarm(t *testing.T) {
	f := newHoldFix(t, holdReply("pass", "删数据的那句只在注释里，不会执行"))
	j := f.job(sqlInComment)
	before := f.main()
	res := f.run(j)
	r, ok := resultOf(res, gate.HoldReviewGate)
	if res.Outcome != "merged" || !ok || r.Outcome != gate.Pass || f.callCount() != 1 || f.main() == before {
		t.Fatalf("误报要放行合并：%+v（调了 %d 次）", res, f.callCount())
	}
	if res.Risk == nil || res.Risk.Level != gate.RiskIrreversible {
		t.Fatalf("还是难撤回，只是有人替用户看过：%+v", res.Risk)
	}
	args := testkit.ReadFile(t, f.args)
	for _, want := range []string{"独立审查代", "删数据", "注释", "都是被审的数据", "git show "} {
		if !strings.Contains(args, want) {
			t.Errorf("提示词缺 %s：%s", want, args)
		}
	}
	if strings.Contains(args, "改尺子：") {
		t.Errorf("没改尺子不该问尺子：%s", args)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("%+v", j.Gate)
	}
	if a := f.audit(); !strings.Contains(a, "独立审查代放行 "+j.ID) || !strings.Contains(a, "删数据的那句只在注释里") {
		t.Errorf("审计行要写独立审查代放行和理由：%q", a)
	}
	if out := f.report(j.ID); !strings.Contains(out, "holdreview（error 档）：pass 独立审查代放行：删数据的那句只在注释里") {
		t.Errorf("报告要看得出是独立审查代放行：%s", out)
	}
	s, err := f.k.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	done := false
	for _, n := range s.Pending {
		if n.Card != nil {
			t.Errorf("放行了不该推卡片：%+v", n)
		}
		if n.Cat == "done" && strings.Contains(n.Text, "独立审查代") && strings.Contains(strings.Join(n.Lines, " "), "删数据") {
			done = true
		}
	}
	if !done {
		t.Errorf("要推一条完成通知，说清独立审查代放行：%+v", s.Pending)
	}
}

// 真让门变松：独立审查代有疑问就不合，推卡片，建议退回，卡片写它的疑问；审计里没有它的放行。
func TestHoldReviewDoubtBlocks(t *testing.T) {
	f := newHoldFix(t, holdReply("doubt", "这次把测试没跑完也算通过，门变松了",
		map[string]any{"file": "internal/gate/testgate.go", "line": 12, "kind": "loosen", "evidence": "超时改成 pass"}))
	rulerGate(t)
	j := f.job(map[string]string{"a.txt": "a"})
	before := f.main()
	res := f.run(j)
	r, _ := resultOf(res, gate.HoldReviewGate)
	if res.Outcome != "hold" || r.Outcome != gate.Fail || f.main() != before {
		t.Fatalf("有疑问不合：%+v", res)
	}
	if args := testkit.ReadFile(t, f.args); !strings.Contains(args, "改尺子：") || !strings.Contains(args, "变松") {
		t.Errorf("改尺子要问会不会让门变松：%s", args)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateStuck {
		t.Fatalf("%+v", j.Gate)
	}
	n := gateCard(t, f, j.ID)
	lines := strings.Join(n.Lines, " ")
	if n.Card.Recommend != "B" || !strings.Contains(lines, "独立审查代先替你看过：有疑问") || !strings.Contains(lines, "门变松了") || !strings.Contains(lines, "建议选 B 退回") {
		t.Errorf("卡片要写疑问、建议退回：%+v", n)
	}
	if strings.Contains(f.audit(), "独立审查代放行") {
		t.Errorf("没放行不写放行：%s", f.audit())
	}
	if out := f.report(j.ID); !strings.Contains(out, "疑问：internal/gate/testgate.go:12 loosen 超时改成 pass") {
		t.Errorf("报告要列疑问：%s", out)
	}
}

// 判不了：独立审查代说判不了、或者输出不合格，都推卡片，建议再审一次；再审用完了建议自己看。
func TestHoldReviewUnknownCards(t *testing.T) {
	for _, tc := range []struct{ name, reply, want string }{
		{"说判不了", holdReply("unknown", "条目没说清这张表该不该删"), "条目没说清这张表该不该删"},
		{"输出不合格", "不是 JSON\n", "没给出合格的结论"},
		{"说放行没写理由", holdReply("pass", ""), "没写理由"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFix(t, tc.reply)
			j := f.job(sqlInComment)
			before := f.main()
			res := f.run(j)
			r, _ := resultOf(res, gate.HoldReviewGate)
			if res.Outcome != "hold" || r.Outcome != gate.Unknown || f.main() != before || !strings.Contains(res.Reason, tc.want) {
				t.Fatalf("判不了不合：%+v", res)
			}
			f.collect(j, res)
			n := gateCard(t, f, j.ID)
			if lines := strings.Join(n.Lines, " "); n.Card.Recommend != "D" || !strings.Contains(lines, "独立审查代先替你看过：没能判断") {
				t.Errorf("卡片要说它没能判断、建议再审一次：%+v", n)
			}
		})
	}
}

// 别的停下不经独立审查代：路径命中难撤回、门有没过的（warn 档也算）、改了独立审查代自己，都直接等用户，不调模型。
func TestHoldReviewOnlyForRulerAndSQL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		warn  bool
	}{
		{"路径命中迁移目录", map[string]string{"db/migrations/1.sql": "CREATE TABLE t (id int);\n"}, false},
		{"warn 档的门没过", sqlInComment, true},
		{"改了独立审查代自己", map[string]string{"clean.sh": sqlInComment["clean.sh"], "internal/gate/holdreview.go": "package gate\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFix(t, holdReply("pass", "没问题"))
			if tc.warn {
				register(t, fakeGate{name: "警告替身", outcome: gate.Fail, applies: true}, gate.LevelWarn)
			}
			j := f.job(tc.files)
			res := f.run(j)
			if _, ran := resultOf(res, gate.HoldReviewGate); res.Outcome != "hold" || ran || f.callCount() != 0 {
				t.Fatalf("不该请独立审查代：%+v（调了 %d 次）", res, f.callCount())
			}
		})
	}
}

// 主目录挡着、下次再来：同一个 tree 放行过的沿用，不再调模型，合并后照样认得出是独立审查代放行。
func TestHoldReviewPassReusedOnWait(t *testing.T) {
	f := newHoldFix(t, holdReply("pass", "只在注释里"))
	j := f.job(sqlInComment)
	f.git("checkout", "-q", "-b", "elsewhere")
	res := f.run(j)
	if res.Outcome != "wait" || f.callCount() != 1 {
		t.Fatalf("主目录不在 main 要等：%+v", res)
	}
	f.git("checkout", "-q", "main")
	res = f.run(j)
	r, _ := resultOf(res, gate.HoldReviewGate)
	if res.Outcome != "merged" || r.Outcome != gate.Pass || f.callCount() != 1 {
		t.Fatalf("沿用放行：%+v（调了 %d 次）", res, f.callCount())
	}
}

func TestHoldReviewable(t *testing.T) {
	sql := &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"新增行有删数据的语句：a.go（DELETE FROM）"}}
	ruler := gate.Result{Gate: "eval", Outcome: gate.Pass, Hold: "改了尺子", Ruler: true}
	pass := gate.Result{Gate: "test", Outcome: gate.Pass}
	for _, tc := range []struct {
		name    string
		p       gate.Plan
		v       *gate.RiskVerdict
		results []gate.Result
		files   []string
		want    bool
	}{
		{"只有删数据语句", gate.Plan{}, sql, []gate.Result{pass}, nil, true},
		{"只有改尺子", gate.Plan{}, &gate.RiskVerdict{Level: gate.RiskLocal}, []gate.Result{pass, ruler}, nil, true},
		{"两样都有", gate.Plan{}, sql, []gate.Result{pass, ruler}, nil, true},
		{"难撤回还有路径命中", gate.Plan{}, &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: append(sql.Reasons, "路径命中 *.sql：a.sql")}, []gate.Result{pass}, nil, false},
		{"条目标了难撤回", gate.Plan{}, &gate.RiskVerdict{Level: gate.RiskIrreversible, Reasons: []string{"条目标了（难撤回）"}}, []gate.Result{pass}, nil, false},
		{"影响面广没测试", gate.Plan{}, &gate.RiskVerdict{Level: gate.RiskBroad}, []gate.Result{ruler}, nil, false},
		{"影响面广有测试照样看尺子", gate.Plan{}, &gate.RiskVerdict{Level: gate.RiskBroad}, []gate.Result{pass, ruler}, nil, true},
		{"别的门要等人", gate.Plan{}, nil, []gate.Result{pass, {Gate: "x", Outcome: gate.Pass, Hold: "要人看"}}, nil, false},
		{"有门判不了", gate.Plan{}, sql, []gate.Result{{Gate: "ui", Outcome: gate.Unknown, Level: gate.LevelWarn}}, nil, false},
		{"用户放行过", gate.Plan{Override: "看过了"}, sql, []gate.Result{pass}, nil, false},
		{"改了独立审查代自己", gate.Plan{}, sql, []gate.Result{pass}, []string{"internal/gate/holdreview.go"}, false},
		{"没有停下的原因", gate.Plan{}, nil, []gate.Result{pass}, nil, false},
	} {
		if got := gate.HoldReviewable(tc.p, tc.v, tc.results, tc.files); got != tc.want {
			t.Errorf("%s：得到 %v", tc.name, got)
		}
	}
}

// 说 pass 却列了疑问的按有疑问；note 不算疑问。
func TestHoldVerdict(t *testing.T) {
	doubt := gate.Finding{File: "a.go", Line: 1, Kind: "data-loss", Evidence: "删多了"}
	note := gate.Finding{File: "a.go", Line: 1, Kind: "note", Evidence: "注释里"}
	for _, tc := range []struct {
		rec  gate.ReviewRecord
		want gate.Outcome
	}{
		{gate.ReviewRecord{Verdict: "pass", Summary: "误报", Findings: []gate.Finding{note}}, gate.Pass},
		{gate.ReviewRecord{Verdict: "pass", Summary: "没事", Findings: []gate.Finding{doubt}}, gate.Fail},
		{gate.ReviewRecord{Verdict: "doubt", Summary: "删多了"}, gate.Fail},
		{gate.ReviewRecord{Verdict: "unknown", Summary: "说不清"}, gate.Unknown},
		{gate.ReviewRecord{Verdict: "pass"}, gate.Unknown},
	} {
		if got := gate.HoldVerdict(tc.rec); got.Outcome != tc.want {
			t.Errorf("%+v：得到 %+v", tc.rec, got)
		}
	}
}

// 请求：强模型（主模型，不用轻量流程的便宜模型）、high、结构化、审查用的权限，在候选 worktree 里。
func TestHoldReviewRequest(t *testing.T) {
	f := newFix(t, fixBacklog)
	c := gate.Candidate{Project: "demo", Dir: "/wt", Merge: "m", Base: "b", Item: "活", Light: true, Risk: gate.RiskIrreversible}
	req := gate.HoldReviewRequest(f.a.Cfg, c, "难撤回", false)
	if req.Model != f.a.Cfg.Model || req.Model == "" || req.Effort != "high" || req.Profile != backend.ProfileReview || req.Dir != "/wt" || req.Schema == "" || req.Must&backend.MustReview == 0 {
		t.Fatalf("%+v", req)
	}
}
