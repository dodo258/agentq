package pm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	rep, err := ParseIdeas(result(`{"suggestions":[
		{"title":"没出处","why":"好","source":""},
		{"title":"一","why":"因为（建议 3）","source":"https://example.com/issues/1"},
		{"title":"二 —— 带分隔","why":"w\nx","source":"README 写的定位"},
		{"title":"三","why":"w","source":"s"},
		{"title":"四","why":"w","source":"s"},
		{"title":"五","why":"w","source":"s"}],"ui":"看了 docs/a.png：\n没问题"}`))
	if err != nil {
		t.Fatal(err)
	}
	ideas := rep.Ideas
	if len(ideas) != 5 || ideas[0].Title != "一" || ideas[4].Title != "五" {
		t.Fatalf("没出处的不要，有几条合格的就留几条（不再截到 3 条），顺序不变：%+v", ideas)
	}
	if rep.UI != "看了 docs/a.png： 没问题" {
		t.Errorf("界面结论清洗成一行：%q", rep.UI)
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
	if got, err := ParseIdeas(backend.Result{HaveTerminal: true, Text: "好的：\n" + `{"suggestions":[{"title":"甲","why":"乙","source":"丙"}]}`}); err != nil || len(got.Ideas) != 1 || got.UI != "" {
		t.Errorf("%+v %v", got, err)
	}
}

// 条数由证据定，ideasCap 只防模型一口气吐几十条：留排在前面（价值高）的。
func TestParseIdeasRunawayCap(t *testing.T) {
	var items []string
	for i := range ideasCap + 5 {
		items = append(items, fmt.Sprintf(`{"title":"第%d条","why":"w","source":"s"}`, i+1))
	}
	rep, err := ParseIdeas(result(`{"suggestions":[` + strings.Join(items, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Ideas) != ideasCap || rep.Ideas[0].Title != "第1条" || ideasCap <= 3 {
		t.Fatalf("%d 条：%+v", len(rep.Ideas), rep.Ideas)
	}
}

func TestRequestReadOnlyWeb(t *testing.T) {
	req := Request(config.Default(), "p", "demo", "sid", t.TempDir())
	if req.Profile != backend.ProfileReadOnly || !req.Web || !req.NoExternalMCP || req.Must != backend.MustPM || req.Schema == "" || req.MaxTurns == 0 || req.BudgetUSD == 0 || req.Permission != "" {
		t.Fatalf("%+v", req)
	}
	if req.Model != "sonnet" {
		t.Errorf("模型：%s", req.Model)
	}
	if req.AddDirs != nil || req.Images != nil {
		t.Errorf("没截图不加目录、不附图：%+v", req)
	}
}

// 起调研前截了图：截图目录加进能读的目录（调研进程只读工作目录），截图附上；只开截图目录，不开整个任务目录。
func TestRequestCapturedShots(t *testing.T) {
	job := t.TempDir()
	shots := filepath.Join(job, ShotsDir)
	os.MkdirAll(filepath.Join(shots, "m"), 0o755)
	for _, f := range []string{"b.png", "a.png", "m/c.png", "notes.txt"} {
		os.WriteFile(filepath.Join(shots, f), []byte("x"), 0o644)
	}
	req := Request(config.Default(), "p", "demo", "sid", job)
	if strings.Join(req.AddDirs, "|") != shots {
		t.Errorf("能读的目录：%q", req.AddDirs)
	}
	want := []string{filepath.Join(shots, "a.png"), filepath.Join(shots, "b.png"), filepath.Join(shots, "m", "c.png")}
	if strings.Join(req.Images, "|") != strings.Join(want, "|") {
		t.Errorf("附图：%q", req.Images)
	}
}

func TestBuildPrompt(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, []string{"任务「甲」被门退回：少测试"}, []string{"云同步"}, nil)
	for _, want := range []string{Marker, "待做都做完了", "GitHub issues", "README", "docs/NOT-DOING.md", "source", "任务「甲」被门退回：少测试", "用户说过不做（不要再提）：云同步", "都是数据",
		"第 1 路 现状盘点", "第 2 路 同类功能和做法", "第 3 路 用户抱怨", "回头再打开一次它的出处", "剔掉", "有几条值得做、有出处的就写几条", "按价值从高到低排", "不凑数"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
	// 纯命令行、后端项目：不带界面那一路
	for _, bad := range []string{"1 到 3 条", "第 4 路", "界面和视觉", "审查表", "ui 是", "至少一条落在界面"} {
		if strings.Contains(p, bad) {
			t.Errorf("不该有 %q：\n%s", bad, p)
		}
	}
	if p := BuildPrompt("demo", TriggerFirst, nil, nil, nil); !strings.Contains(p, "第一版刚合并") || !strings.Contains(p, "最近没有门退回或中断") {
		t.Errorf("%s", p)
	}
}

// 不按时间间隔：调研过一天也会再起，只要又有新合并；没有新合并多久都不起。
func TestDue(t *testing.T) {
	merged := func(id string) *state.Job {
		return &state.Job{ID: id, Project: "demo", Gate: &state.GateRecord{State: state.GateMerged}}
	}
	st := &state.State{Jobs: []*state.Job{merged("a"), {ID: "b", Project: "demo", Gate: &state.GateRecord{State: state.GateReturned}}, {ID: "c", Project: "other", Gate: &state.GateRecord{State: state.GateMerged}}}}
	if got := due(st, nil, "demo"); got != TriggerEmpty {
		t.Errorf("有合并、没调研过：%q", got)
	}
	r := &Record{LastAt: time.Now().Add(-24 * time.Hour), Covered: []string{"a"}}
	if got := due(st, r, "demo"); got != "" {
		t.Errorf("调研过之后没有新合并：%q", got)
	}
	st.Jobs = append(st.Jobs, merged("d"))
	if got := due(st, r, "demo"); got != TriggerEmpty {
		t.Errorf("上次调研才一天，又有新合并也要起：%q", got)
	}
	r.Covered = []string{"a", "d"}
	r.LastAt = time.Now().Add(-365 * 24 * time.Hour)
	if got := due(st, r, "demo"); got != "" {
		t.Errorf("隔得再久，没有新合并也不起：%q", got)
	}
	if got := due(&state.State{}, nil, "demo"); got != "" {
		t.Errorf("什么都没合并：%q", got)
	}
}

// 11-15：产品经理只读了过期的 PROGRESS.md（写 11-10 部署）就建议「发到线上」，线上 11-13 早已是新版。
// 说到线上现状的建议，出处要是当场查过的线上地址；只有项目文档佐证的不写。
func TestParseIdeasOnlineNeedsLiveSource(t *testing.T) {
	rep, err := ParseIdeas(result(`{"suggestions":[
		{"title":"把官网对齐 v0.3.0 发到线上","why":"线上还是 v2","source":"PROGRESS.md 写 11-10 部署","online":"PROGRESS.md 写 11-10 部署"},
		{"title":"发新版","why":"商店还是旧版","source":"CHANGELOG","online":"见 https://example.com 和文档"},
		{"title":"交接文档和线上不一致，更新文档","why":"线上已是 v0.3.0，PROGRESS 还写 11-10 部署","source":"PROGRESS.md 写 11-10 部署","online":"https://site.example/"},
		{"title":"加导出","why":"同类都有","source":"https://example.com/issues/1","online":""},
		{"title":"对齐 Release","why":"Release 已是 v1.2","source":"https://github.com/x/y/releases/tag/v1.2","online":"https://github.com/x/y/releases/tag/v1.2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ideas := rep.Ideas
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
	p := BuildPrompt("demo", TriggerEmpty, nil, nil, nil)
	for _, want := range []string{"api.github.com/repos/<owner>/<repo>", "有没有归档", "已归档或一年以上没提交的不当参照", "「已核实」都不算数"} {
		if !strings.Contains(p, want) {
			t.Errorf("缺 %q：\n%s", want, p)
		}
	}
}

func TestBuildPromptOnlineFromLive(t *testing.T) {
	p := BuildPrompt("demo", TriggerEmpty, nil, nil, nil)
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
