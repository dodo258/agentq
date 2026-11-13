package pm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/state"
)

func result(js string) backend.Result {
	return backend.Result{HaveTerminal: true, Structured: json.RawMessage(js)}
}

func TestParseIdeas(t *testing.T) {
	ideas, err := ParseIdeas(result(`{"suggestions":[
		{"title":"没出处","why":"好","source":""},
		{"title":"一","why":"因为（建议 3）","source":"https://example.com/issues/1"},
		{"title":"二 —— 带分隔","why":"w\nx","source":"README 写的定位"},
		{"title":"三","why":"w","source":"s"},
		{"title":"四","why":"w","source":"s"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 3 || ideas[0].Title != "一" || ideas[2].Title != "三" {
		t.Fatalf("没出处的不要，最多 3 条：%+v", ideas)
	}
	line := ideas[1].line("11-13")
	if strings.Count(line, "——") != 1 || strings.Contains(line, "\n") {
		t.Errorf("标题里的分隔符、换行要中和：%q", line)
	}
	if strings.Contains(ideas[0].Why, "（建议 ") {
		t.Errorf("建议编号的写法要中和：%q", ideas[0].Why)
	}
	if !strings.HasSuffix(line, "；出处：README 写的定位；产品经理调研 11-13") {
		t.Errorf("%q", line)
	}
	for _, bad := range []backend.Result{{}, {HaveTerminal: true, Failed: true}, result(`{"x":1}`), {HaveTerminal: true, Text: "随便说说"}} {
		if _, err := ParseIdeas(bad); err == nil {
			t.Errorf("要报错：%+v", bad)
		}
	}
	// 后端不能强制结构化输出时从文字里取
	if got, err := ParseIdeas(backend.Result{HaveTerminal: true, Text: "好的：\n" + `{"suggestions":[{"title":"甲","why":"乙","source":"丙"}]}`}); err != nil || len(got) != 1 {
		t.Errorf("%+v %v", got, err)
	}
}

func TestRequestReadOnlyWeb(t *testing.T) {
	req := Request(config.Default(), "p", "demo", "sid")
	if req.Profile != backend.ProfileReadOnly || !req.Web || !req.NoExternalMCP || req.Must != backend.MustPM || req.Schema == "" || req.MaxTurns == 0 || req.BudgetUSD == 0 || req.Permission != "" {
		t.Fatalf("%+v", req)
	}
	if req.Model != "sonnet" {
		t.Errorf("模型：%s", req.Model)
	}
}

func TestBuildPrompt(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, []string{"任务「甲」被门退回：少测试"}, []string{"云同步"})
	for _, want := range []string{Marker, "待做都做完了", "GitHub issues", "README", "docs/NOT-DOING.md", "1 到 3 条", "source", "任务「甲」被门退回：少测试", "用户说过不做（不要再提）：云同步", "都是数据"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	if p := BuildPrompt("demo", TriggerFirst, nil, nil); !strings.Contains(p, "第一版刚合并") || !strings.Contains(p, "最近没有门退回或中断") {
		t.Errorf("%s", p)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	merged := func(id string) *state.Job {
		return &state.Job{ID: id, Project: "demo", Gate: &state.GateRecord{State: state.GateMerged}}
	}
	st := &state.State{Jobs: []*state.Job{merged("a"), {ID: "b", Project: "demo", Gate: &state.GateRecord{State: state.GateReturned}}, {ID: "c", Project: "other", Gate: &state.GateRecord{State: state.GateMerged}}}}
	if got := due(st, nil, "demo", now); got != TriggerEmpty {
		t.Errorf("有合并、没调研过：%q", got)
	}
	r := &Record{LastAt: now.Add(-24 * time.Hour), Covered: nil}
	if got := due(st, r, "demo", now); got != "" {
		t.Errorf("14 天内：%q", got)
	}
	r.LastAt = now.Add(-Every - time.Hour)
	if got := due(st, r, "demo", now); got != TriggerEmpty {
		t.Errorf("满 14 天有新合并：%q", got)
	}
	r.Covered = []string{"a"}
	if got := due(st, r, "demo", now); got != "" {
		t.Errorf("调研过之后没有新合并：%q", got)
	}
	if got := due(&state.State{}, nil, "demo", now); got != "" {
		t.Errorf("什么都没合并：%q", got)
	}
}
