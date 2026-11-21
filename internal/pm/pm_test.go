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

// 11-15：产品经理只读了过期的 PROGRESS.md（写 11-10 部署）就建议「发到线上」，线上 11-13 早已是新版。
// 说到线上现状的建议，出处要是当场查过的线上地址；只有项目文档佐证的不写。
func TestParseIdeasOnlineNeedsLiveSource(t *testing.T) {
	ideas, err := ParseIdeas(result(`{"suggestions":[
		{"title":"把官网对齐 v0.3.0 发到线上","why":"线上还是 v2","source":"PROGRESS.md 写 11-10 部署","online":"PROGRESS.md 写 11-10 部署"},
		{"title":"发新版","why":"商店还是旧版","source":"CHANGELOG","online":"见 https://example.com 和文档"},
		{"title":"交接文档和线上不一致，更新文档","why":"线上已是 v0.3.0，PROGRESS 还写 11-10 部署","source":"PROGRESS.md 写 11-10 部署","online":"https://site.example/"},
		{"title":"加导出","why":"同类都有","source":"https://example.com/issues/1","online":""},
		{"title":"对齐 Release","why":"Release 已是 v1.2","source":"https://github.com/x/y/releases/tag/v1.2","online":"https://github.com/x/y/releases/tag/v1.2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range ideas {
		titles = append(titles, it.Title)
	}
	if strings.Join(titles, "|") != "交接文档和线上不一致，更新文档|加导出|对齐 Release" {
		t.Fatalf("只有文档佐证的线上判断不要：%q", titles)
	}
	if ideas[0].Source != "https://site.example/；PROGRESS.md 写 11-10 部署" || ideas[0].Online != "https://site.example/" {
		t.Errorf("查过的线上地址要进出处：%+v", ideas[0])
	}
	if ideas[1].Source != "https://example.com/issues/1" || ideas[2].Source != "https://github.com/x/y/releases/tag/v1.2" {
		t.Errorf("不涉及线上、出处已含地址的，出处不变：%+v", ideas[1:])
	}
}

// 推荐的开源项目要当场核对归档和最近提交，搜索结果里的数字不算。
func TestBuildPromptVerifyRepos(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, nil, nil)
	for _, want := range []string{"api.github.com/repos/<owner>/<repo>", "有没有归档", "已归档或一年以上没提交的不当参照", "「已核实」都不算数"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
}

func TestBuildPromptOnlineFromLive(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, nil, nil)
	for _, want := range []string{"已发布、已部署、商店里", "当场打开查的线上来源（网页、GitHub Release、商店页）", "PROGRESS.md", "只有文档佐证的不写成线上事实",
		"「发到线上」", "「交接文档和线上不一致，更新文档」", "online（", "title、why、source、online"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	if !strings.Contains(Schema, `"online":{"type":"string"}`) {
		t.Errorf("结果要有 online：%s", Schema)
	}
}
