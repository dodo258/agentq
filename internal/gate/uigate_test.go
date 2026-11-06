package gate_test

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backend"
	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// pngBytes 写一张 PNG。marks 个像素涂成别的颜色，其余是白底。
func writePNG(t *testing.T, path string, w, h, marks int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	for i := 0; i < marks; i++ {
		img.Set(i%w, i/w, color.Black)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// uiFix 打开界面门（error 档好看结论，warn 档的用例自己调），审查门关着，界面审查进程用假后端。
type uiFix struct {
	*reviewFix
	shots string // 脚本从这里拷截图
}

func newUIFix(t *testing.T, levels string, replies ...string) *uiFix {
	t.Helper()
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), replies...)
	f.levels(levels)
	shots := t.TempDir()
	return &uiFix{reviewFix: f, shots: shots}
}

const uiOn = `{"levels":{"review":"off","ui":"error"}}`

// claim 都用假后端：界面审查进程不能起真的 CLI。
func (f *uiFix) claim(title, branch string, files map[string]string, r gate.Ready) *state.Job {
	f.t.Helper()
	j := f.fix.claim(title, branch, files, r)
	j.Backend = f.name
	return j
}

// script 在默认分支上放 .agentq/ui：把 shots 目录拷进 $AGENTQ_ARTIFACTS。
func (f *uiFix) script(body string) {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.dir, gate.UIScript), []byte(body), 0o755)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "界面脚本")
}

func (f *uiFix) copyShots() string {
	return "#!/bin/sh\ncp " + f.shots + "/* \"$AGENTQ_ARTIFACTS/\"\n"
}

func uiResult(t *testing.T, res gate.RunResult) gate.Result {
	t.Helper()
	r, ok := resultOf(res, "ui")
	if !ok {
		t.Fatalf("没跑界面门：%+v", res)
	}
	return r
}

func uiReply(verdict, skill string, findings ...map[string]any) string {
	if findings == nil {
		findings = []map[string]any{}
	}
	s, _ := json.Marshal(map[string]any{"verdict": verdict, "skill": skill, "summary": "看过截图了", "findings": findings})
	b, _ := json.Marshal(testkit.FakeEvent{Type: "result", OK: true, Text: "审完了", Structured: s})
	return string(b) + "\n"
}

func uiFinding(shot, kind, severity, evidence string) map[string]any {
	return map[string]any{"screenshot": shot, "kind": kind, "severity": severity, "evidence": evidence}
}

func uiFiles() map[string]string { return map[string]string{"web/App.tsx": "export default 1\n"} }

// 非界面改动不跑。
func TestUIGateNotApplies(t *testing.T) {
	f := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	for _, files := range []map[string]string{
		{"core/a.go": "1\n"},
		{"web/App.test.tsx": "1\n"},
		{"web/lib.ts": "1\n"},
	} {
		g := gate.UIGate(gate.Deps{Env: f.a.Env, Git: f.a.GitForTest()})
		var list []string
		for name := range files {
			list = append(list, name)
		}
		if g.AppliesTo(gate.Candidate{Files: list, ProjectDir: f.dir, Base: "refs/heads/main"}) {
			t.Errorf("%v 不是界面改动", list)
		}
	}
	j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "1\n"}, ready("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if _, ok := resultOf(res, "ui"); ok || f.callCount() != 0 {
		t.Errorf("不跑界面门：%+v %d", res.Results, f.callCount())
	}
	if !strings.Contains(strings.Join(res.Notes, "；"), "ui 门不适用") {
		t.Errorf("%v", res.Notes)
	}
}

// 哪些算界面文件。
func TestUIGateApplies(t *testing.T) {
	f := newUIFix(t, uiOn)
	g := gate.UIGate(gate.Deps{Env: f.a.Env, Git: f.a.GitForTest()})
	for _, name := range []string{"a.tsx", "web/a.vue", "s/a.css", "Sources/HomeView.swift"} {
		if !g.AppliesTo(gate.Candidate{Files: []string{"x.go", name}, ProjectDir: f.dir, Base: "refs/heads/main"}) {
			t.Errorf("%s 算界面改动", name)
		}
	}
	for _, name := range []string{"Sources/Home.swift", ".agentq/a.css", "a.test.tsx", "docs/design/approved/home.png"} {
		if g.AppliesTo(gate.Candidate{Files: []string{name}, ProjectDir: f.dir, Base: "refs/heads/main"}) {
			t.Errorf("%s 不算界面改动", name)
		}
	}
}

// 可配置：读默认分支上的，任务改不了。写了就替换默认的；写坏了用默认的。
func TestUIGateConfigurable(t *testing.T) {
	f := newUIFix(t, `{"levels":{"review":"off"},"ui_files":["Screen.swift"]}`)
	g := gate.UIGate(gate.Deps{Env: f.a.Env, Git: f.a.GitForTest()})
	c := func(name string) gate.Candidate {
		return gate.Candidate{Files: []string{name}, ProjectDir: f.dir, Base: "refs/heads/main"}
	}
	if !g.AppliesTo(c("HomeScreen.swift")) || g.AppliesTo(c("a.tsx")) {
		t.Error("配置要替换默认的")
	}
	f.levels(`{"levels":`)
	if !g.AppliesTo(c("a.tsx")) || g.AppliesTo(c("HomeScreen.swift")) {
		t.Error("写坏了用默认的")
	}
	f.levels(`{"ui_files":[]}`)
	if g.AppliesTo(c("a.tsx")) {
		t.Error("空列表：什么都不算界面文件")
	}
}

// 验收：无脚本是 unknown。默认 warn 不挡合并；error 档判不了不放行。
func TestUIGateNoScript(t *testing.T) {
	f := newUIFix(t, `{"levels":{"review":"off"}}`)
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	r := uiResult(t, res)
	if res.Outcome != "merged" || r.Outcome != gate.Unknown || r.Level != gate.LevelWarn || !strings.Contains(r.Summary, gate.UIScript) {
		t.Fatalf("默认 warn，不挡：%+v", res)
	}
	if f.callCount() != 0 {
		t.Errorf("没有截图不调模型：%d", f.callCount())
	}

	e := newUIFix(t, uiOn)
	j = e.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	if res := e.run(j); res.Outcome != "unknown" {
		t.Fatalf("error 档判不了不放行：%+v", res)
	}
}

// 验收：空白截图被拦。没调模型。
func TestUIGateBlankScreenshot(t *testing.T) {
	f := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	f.script(f.copyShots())
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	writePNG(t, filepath.Join(f.shots, "blank.png"), 200, 100, 0)
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	r := uiResult(t, res)
	if res.Outcome != "fail" || r.Outcome != gate.Fail || !strings.Contains(r.Summary, "blank.png 是空白的") || strings.Contains(r.Summary, "ok.png") {
		t.Fatalf("%+v", res)
	}
	if f.callCount() != 0 {
		t.Errorf("空白就不用花额度：%d", f.callCount())
	}
}

// 空白的边界：几乎空白算（一个转圈的图标），一行字不算；取样不漏。
func TestUIGateBlankBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		w, h  int
		marks int
		bad   string
	}{
		{"全白", 100, 100, 0, "空白"},
		{"万分之五", 200, 100, 10, "空白"},
		{"千分之二", 200, 100, 40, ""},
		{"很大的图取样", 3000, 2000, 60000, ""},
		{"很大的全白", 3000, 2000, 0, "空白"},
		{"一个像素", 1, 1, 0, "空白"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.png")
			writePNG(t, p, c.w, c.h, c.marks)
			if got := gate.ShotProblem(p); !strings.Contains(got, c.bad) || (c.bad == "" && got != "") {
				t.Errorf("%q，要 %q", got, c.bad)
			}
		})
	}
	bad := filepath.Join(t.TempDir(), "bad.png")
	os.WriteFile(bad, []byte("不是图片"), 0o644)
	if got := gate.ShotProblem(bad); !strings.Contains(got, "解开") {
		t.Errorf("解不开：%q", got)
	}
	// 头是好的、后面截断
	cut := filepath.Join(t.TempDir(), "cut.png")
	writePNG(t, cut, 100, 100, 100)
	b, _ := os.ReadFile(cut)
	os.WriteFile(cut, b[:len(b)/2], 0o644)
	if got := gate.ShotProblem(cut); got == "" {
		t.Error("截断的要报")
	}
	// 声明的尺寸超大：不解码
	huge := filepath.Join(t.TempDir(), "huge.png")
	writePNG(t, huge, 10, 10, 10)
	b, _ = os.ReadFile(huge)
	copy(b[16:24], []byte{0x7f, 0xff, 0xff, 0xff, 0x7f, 0xff, 0xff, 0xff})
	os.WriteFile(huge, b, 0o644)
	if got := gate.ShotProblem(huge); got == "" {
		t.Error("超大的要报")
	}
}

// 脚本的各种结果。退出码 0 但没截图算失败；脚本失败算失败；125 判不了。
func TestUIGateScriptOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		outcome string
		has     string
	}{
		{"没截图", "#!/bin/sh\nexit 0\n", "fail", "没有 PNG"},
		{"只有别的格式", "#!/bin/sh\necho x > \"$AGENTQ_ARTIFACTS/a.jpg\"\n", "fail", "没有 PNG"},
		{"失败", "#!/bin/sh\necho 浏览器没了\nexit 1\n", "fail", "退出码 1"},
		{"跑不了", "#!/bin/sh\nexit 125\n", "unknown", "判不了"},
		{"软链接不算", "#!/bin/sh\nln -s /etc/hosts \"$AGENTQ_ARTIFACTS/a.png\"\n", "fail", "没有 PNG"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newUIFix(t, uiOn)
			f.script(c.script)
			j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
			res := f.run(j)
			if res.Outcome != c.outcome || !strings.Contains(uiResult(t, res).Summary, c.has) {
				t.Fatalf("%+v", res)
			}
		})
	}
}

// 门跑默认分支上的脚本，任务改了也没用；第一次由任务新增时用任务的版本，标给审查。
func TestUIGateRunsScriptFromMain(t *testing.T) {
	f := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	marker := filepath.Join(f.shots, "任务的脚本跑了")
	files := uiFiles()
	files[gate.UIScript] = "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	j := f.claim("做事", "agent/x", files, ready("agent/x"))
	// 改门脚本要写门说明，这里只看界面门：测试门关掉
	f.levels(`{"levels":{"review":"off","test":"off","ui":"error"}}`)
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("跑了任务改的脚本")
	}
	if !strings.Contains(strings.Join(uiResult(t, res).Evidence, "\n"), "用 main 上的 "+gate.UIScript) {
		t.Errorf("%+v", uiResult(t, res).Evidence)
	}
}

func TestUIGateFirstScriptByTask(t *testing.T) {
	f := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	shot := filepath.Join(f.shots, "ok.png")
	writePNG(t, shot, 200, 100, 500)
	files := uiFiles()
	files[gate.UIScript] = "#!/bin/sh\ncp " + shot + " \"$AGENTQ_ARTIFACTS/\"\n"
	j := f.claim("做事", "agent/x", files, ready("agent/x")) // 提交说明没写门说明：首次新增不算动门
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	r := uiResult(t, res)
	if !strings.Contains(strings.Join(r.Evidence, "\n"), "首次由任务新增 "+gate.UIScript) {
		t.Errorf("界面门的报告要标明：%+v", r.Evidence)
	}
	tr, _ := resultOf(res, "test")
	if !strings.Contains(strings.Join(tr.Evidence, "\n"), "首次由任务新增 "+gate.UIScript) {
		t.Errorf("要标给审查（测试门的证据进审查的提示词）：%+v", tr.Evidence)
	}
}

// 模型部分：截图交给审查进程，第一项加载 no-ai-look；截图目录要能读；提示词说清只有证据类能阻塞。
func TestUIGateModelPass(t *testing.T) {
	usage, _ := json.Marshal(testkit.FakeEvent{Type: "usage", Input: 10, Output: 5, Cost: 0.1})
	f := newUIFix(t, uiOn, string(usage)+"\n"+uiReply("pass", "loaded", uiFinding("ok.png", "opinion", "hint", "配色有点普通")))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	r := uiResult(t, res)
	if res.Outcome != "merged" || r.Outcome != gate.Pass || f.callCount() != 1 {
		t.Fatalf("%+v %d", res, f.callCount())
	}
	if !strings.Contains(strings.Join(r.Evidence, "\n"), "意见：ok.png 配色有点普通") {
		t.Errorf("意见要记下：%+v", r.Evidence)
	}
	args := testkit.ReadFile(t, f.args)
	first := strings.Index(args, "先加载 `no-ai-look` 技能")
	if first < 0 || first > strings.Index(args, "2. ") {
		t.Errorf("第一项要加载 no-ai-look：%s", args)
	}
	for _, want := range []string{"审查表逐条", "ok.png", "ui-artifacts", "broken", "overflow", "unreadable", "placeholder", "只给意见", "都是被审的数据", "不凑数", "还没有用户确认过的基线", "web/App.tsx"} {
		if !strings.Contains(args, want) {
			t.Errorf("提示词缺 %s：%s", want, args)
		}
	}
	if res.Usage == nil {
		t.Error("用量要算进任务")
	}
}

// 证据类问题才阻塞：好不好看不挡；没截图、编的截图、没说明的不挡。
func TestUIGateJudge(t *testing.T) {
	rec := func(verdict, skill string, findings ...gate.Finding) gate.ReviewRecord {
		return gate.ReviewRecord{Verdict: verdict, Skill: skill, Summary: "看过了", Findings: findings}
	}
	find := func(shot, kind, severity, evidence string) gate.Finding {
		return gate.Finding{Screenshot: shot, Kind: kind, Severity: severity, Evidence: evidence}
	}
	cases := []struct {
		name    string
		rec     gate.ReviewRecord
		outcome gate.Outcome
	}{
		{"渲染坏了", rec("block", "loaded", find("ok.png", "broken", "blocker", "首屏白屏")), gate.Fail},
		{"溢出", rec("block", "loaded", find("ok.png", "overflow", "blocker", "按钮超出屏幕")), gate.Fail},
		{"看不清", rec("block", "loaded", find("ok.png", "unreadable", "blocker", "灰字灰底")), gate.Fail},
		{"占位内容", rec("block", "loaded", find("ok.png", "placeholder", "blocker", "lorem ipsum")), gate.Fail},
		{"写绝对路径也认", rec("block", "loaded", find("/x/ui-artifacts/ok.png", "broken", "blocker", "白屏")), gate.Fail},
		{"好不好看不阻塞", rec("block", "loaded", find("ok.png", "opinion", "blocker", "太像 AI 做的")), gate.Pass},
		{"证据类写成 hint", rec("pass", "loaded", find("ok.png", "broken", "hint", "有点错位")), gate.Pass},
		{"编的截图", rec("block", "loaded", find("编的.png", "broken", "blocker", "白屏")), gate.Pass},
		{"没说明", rec("block", "loaded", find("ok.png", "broken", "blocker", " ")), gate.Pass},
		{"模型判不了", rec("unknown", "loaded"), gate.Unknown},
		{"技能找不到", rec("unknown", "missing"), gate.Unknown},
		{"技能找不到却说过了", rec("pass", "missing"), gate.Unknown},
		{"没说技能", rec("pass", ""), gate.Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := gate.JudgeUI(c.rec, []string{"ok.png"}); r.Outcome != c.outcome {
				t.Fatalf("%+v", r)
			}
		})
	}
	// 端到端一条：error 档有证据的问题退回
	f := newUIFix(t, uiOn, uiReply("block", "loaded", uiFinding("ok.png", "broken", "blocker", "首屏白屏")))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	if res := f.run(j); res.Outcome != "fail" || uiResult(t, res).Outcome != gate.Fail {
		t.Fatalf("%+v", res)
	}
}

// warn 档：证据类问题也只记录。
func TestUIGateWarnDoesNotBlock(t *testing.T) {
	f := newUIFix(t, `{"levels":{"review":"off"}}`, uiReply("block", "loaded", uiFinding("ok.png", "broken", "blocker", "白屏")))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	if r := uiResult(t, res); res.Outcome != "merged" || r.Outcome != gate.Fail || r.Level != gate.LevelWarn {
		t.Fatalf("%+v", res)
	}
}

// 技能找不到不留记录，装好之后要能重审。坏 JSON 重试一次还不行算 unknown，审查进程挂了不放行。
func TestUIGateModelFailures(t *testing.T) {
	f := newUIFix(t, uiOn, "不是 JSON\n")
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	if r := uiResult(t, res); res.Outcome != "unknown" || r.Outcome != gate.Unknown || f.callCount() != 2 {
		t.Fatalf("坏输出重试一次：%+v %d", res, f.callCount())
	}

	g := newUIFix(t, uiOn, uiReply("unknown", "missing"), uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(g.shots, "ok.png"), 200, 100, 500)
	g.script(g.copyShots())
	j = g.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	if res := g.run(j); res.Outcome != "unknown" {
		t.Fatalf("%+v", res)
	}
	if res := g.run(j); res.Outcome != "merged" || g.callCount() != 2 {
		t.Fatalf("装好之后要重审：%+v %d", res, g.callCount())
	}
}

// 撞用量上限、认证失败走现有的推迟，不算结论；用量算进任务。
func TestUIGateDefer(t *testing.T) {
	limit, _ := json.Marshal(testkit.FakeEvent{Type: "result", OK: false, Text: "You've hit your session limit", Status: 429})
	f := newUIFix(t, uiOn, string(limit)+"\n")
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	if r := uiResult(t, res); res.Outcome != "defer" || !r.Limit || r.Defer == "" {
		t.Fatalf("%+v", res)
	}
}

// 同一个 tree 沿用结论，不再花额度；截图照样重新检查。
func TestUIGateReuse(t *testing.T) {
	f := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	// main 动过：候选重建，任务改动没变
	moved := false
	register(t, fakeGate{name: "推一下", applies: true, outcome: gate.Pass, run: func(c gate.Candidate) gate.Result {
		if !moved {
			moved = true
			f.commitAt(f.dir, 0, "用户在 main 上提交", map[string]string{"user.txt": "1\n"})
		}
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	res := f.run(j)
	r := uiResult(t, res)
	if res.Outcome != "merged" || f.callCount() != 1 || !strings.Contains(strings.Join(r.Evidence, "\n"), "界面审查结论沿用") {
		t.Fatalf("%+v %d", res, f.callCount())
	}
}

// 基线只认默认分支上的。任务动基线由测试门挡（默认 error），界面门 warn 档也合并不了，门说明也不算；搬走（--no-renames 下是删加）一样。
func TestBaselineBlockedByTestGate(t *testing.T) {
	setup := func(t *testing.T) *uiFix {
		f := newUIFix(t, `{"levels":{"review":"off"}}`)
		base := filepath.Join(f.dir, gate.BaselineDir)
		os.MkdirAll(base, 0o755)
		writePNG(t, filepath.Join(base, "home.png"), 200, 100, 500)
		f.git("add", "-A")
		f.git("commit", "-q", "-m", "用户确认的基线")
		return f
	}
	check := func(t *testing.T, f *uiFix, res gate.RunResult) {
		t.Helper()
		r, ok := resultOf(res, "test")
		if res.Outcome != "fail" || !ok || r.Level != gate.LevelError || !strings.Contains(r.Summary, "基线只认用户确认过的") || !strings.Contains(strings.Join(r.Evidence, "\n"), "home.png") {
			t.Fatalf("%+v", res)
		}
		if _, ok := resultOf(res, "ui"); ok {
			t.Errorf("测试门就挡住了：%+v", res.Results)
		}
	}
	t.Run("改", func(t *testing.T) {
		f := setup(t)
		j := f.claim("做事", "agent/x", map[string]string{"web/App.tsx": "1\n", gate.BaselineDir + "/home.png": "任务换掉了\n"}, ready("agent/x"))
		before := f.main()
		check(t, f, f.run(j))
		if f.main() != before {
			t.Error("不能合并")
		}
	})
	t.Run("新加", func(t *testing.T) {
		f := setup(t)
		j := f.claim("做事", "agent/x", map[string]string{gate.BaselineDir + "/new.png": "x\n", gate.BaselineDir + "/home.png": "x\n"}, ready("agent/x"))
		check(t, f, f.run(j))
	})
	t.Run("搬走", func(t *testing.T) {
		f := setup(t)
		j := f.claim("做事", "agent/x", map[string]string{"web/App.tsx": "1\n"}, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		os.MkdirAll(filepath.Join(wt, "assets"), 0o755)
		testkit.GitT(t, f.home, wt, "mv", gate.BaselineDir+"/home.png", "assets/home.png")
		f.commit(wt, nil, ready("agent/x"))
		check(t, f, f.run(j))
	})
	t.Run("门说明也不算", func(t *testing.T) {
		f := setup(t)
		j := f.claim("做事", "agent/x", map[string]string{gate.BaselineDir + "/home.png": "x\n"}, gate.Ready{})
		wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
		os.WriteFile(filepath.Join(wt, "b.txt"), []byte("x"), 0o644)
		testkit.GitT(t, f.home, wt, "add", "-A")
		testkit.GitT(t, f.home, wt, "commit", "-q", "-m", "改基线\n\n"+gate.NoteMark+"用户要的")
		f.commit(wt, nil, ready("agent/x"))
		check(t, f, f.run(j))
	})
	t.Run("用户放行", func(t *testing.T) {
		f := setup(t)
		j := f.claim("做事", "agent/x", map[string]string{gate.BaselineDir + "/home.png": "x\n"}, ready("agent/x"))
		j.Gate.Override = "用户确认过"
		if res := f.run(j); res.Outcome != "merged" {
			t.Fatalf("%+v", res)
		}
	})
}

// 模型部分对照基线
func TestUIGateBaseline(t *testing.T) {
	g := newUIFix(t, uiOn, uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(g.shots, "ok.png"), 200, 100, 500)
	base := filepath.Join(g.dir, gate.BaselineDir)
	os.MkdirAll(base, 0o755)
	writePNG(t, filepath.Join(base, "home.png"), 200, 100, 500)
	g.script(g.copyShots())
	j := g.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	before := g.git("rev-parse", "main:"+gate.BaselineDir+"/home.png")
	res := g.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	args := testkit.ReadFile(t, g.args)
	if !strings.Contains(args, gate.BaselineDir+"/home.png") || !strings.Contains(args, "只有用户能更新基线") {
		t.Errorf("提示词要列基线：%s", args)
	}
	if after := g.git("rev-parse", "main:"+gate.BaselineDir+"/home.png"); after != before {
		t.Error("引擎不更新基线")
	}
}

// 请求：截图目录走 AddDirs，同样的 ProfileReview、结构化、medium。
func TestUIReviewRequest(t *testing.T) {
	f := newFix(t, fixBacklog)
	c := gate.Candidate{Project: "demo", Dir: "/wt", Merge: "m", Base: "b", Item: "活", Files: []string{"a.tsx"}}
	req := gate.UIReviewRequest(f.a.Cfg, c, "/shots", []string{"a.png"}, nil)
	if req.Profile != backend.ProfileReview || req.Effort != "medium" || req.Dir != "/wt" || req.Schema == "" || req.Must&backend.MustReview == 0 || len(req.AddDirs) != 1 || req.AddDirs[0] != "/shots" {
		t.Fatalf("%+v", req)
	}
	args := strings.Join(testkit.ClaudeArgv(t, req), " ")
	for _, want := range []string{"--add-dir /shots", "--effort medium", "--json-schema", "--name agentq demo 界面审查"} {
		if !strings.Contains(args, want) {
			t.Errorf("缺 %s：%s", want, args)
		}
	}
}

// 默认分支叫 master：一样。脚本从 master 上取。
func TestUIGateOnMaster(t *testing.T) {
	f := masterFix(t)
	f.levels(`{"levels":{"review":"off","ui":"error"}}`)
	os.WriteFile(filepath.Join(f.dir, gate.UIScript), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "界面脚本")
	files := uiFiles()
	j := f.claim("做事", "agent/x", files, ready("agent/x"))
	res := f.run(j)
	r, ok := resultOf(res, "ui")
	if !ok || res.Outcome != "fail" || !strings.Contains(strings.Join(r.Evidence, "\n"), "用 master 上的") {
		t.Fatalf("%+v", res)
	}
}

// 能力不够：后端看不到图片，界面门判不了，不放行，不起进程。
func TestUIGateBackendCannotSeeImages(t *testing.T) {
	caps := testkit.AllCaps()
	caps.ImageInput = false
	base := newReviewFix(t, fixBacklog, caps, uiReply("pass", "loaded"))
	base.levels(uiOn)
	f := &uiFix{reviewFix: base, shots: t.TempDir()}
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	res := f.run(j)
	r := uiResult(t, res)
	if res.Outcome != "unknown" || r.Outcome != gate.Unknown || !strings.Contains(r.Summary, "看不到本地图片") || f.callCount() != 0 {
		t.Fatalf("%+v %d", res, f.callCount())
	}
}

// 请求把截图列进 Images，最多给模型看 maxShotFiles 张。
func TestUIReviewRequestImages(t *testing.T) {
	f := newFix(t, fixBacklog)
	var shots []string
	for i := 0; i < 30; i++ {
		shots = append(shots, fmt.Sprintf("s%02d.png", i))
	}
	req := gate.UIReviewRequest(f.a.Cfg, gate.Candidate{Project: "demo", Dir: "/wt"}, "/shots", shots, nil)
	if len(req.Images) != 20 || req.Images[0] != "/shots/s00.png" || req.Must&backend.MustImages == 0 {
		t.Fatalf("%+v", req.Images)
	}
}

// 所有门都按任务记下的分支：脚本、档位、补丁历史、基点都取 trunk 上的，main 上的不算。
func TestGatesUseRecordedBranch(t *testing.T) {
	f := newUIFix(t, `{"levels":{"review":"off"}}`, uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	// 分叉前 main 和 trunk 一样；main 上：修过两次 core、界面门和成品门 error 且脚本失败
	f.git("branch", "trunk")
	patched(f.fix)
	os.WriteFile(filepath.Join(f.dir, gate.UIScript), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(f.dir, gate.SmokeScript), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	f.levels(`{"levels":{"review":"off","patch":"error","ui":"error","smoke":"error"}}`)
	// trunk 上：脚本都过，档位 warn
	f.git("checkout", "-q", "trunk")
	os.WriteFile(filepath.Join(f.dir, gate.UIScript), []byte(f.copyShots()), 0o755)
	os.WriteFile(filepath.Join(f.dir, gate.SmokeScript), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	f.levels(`{"levels":{"review":"off"}}`)
	f.def = "trunk"
	// 认领时默认分支是 trunk，任务记下它；之后 origin/HEAD 改指 main，门也不跟着变
	f.git("update-ref", "refs/remotes/origin/trunk", "refs/heads/trunk")
	f.git("update-ref", "refs/remotes/origin/main", "refs/heads/main")
	f.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	r := fixReady("agent/x")
	j := f.claim("做事", "agent/x", map[string]string{"core/a.tsx": "1\n"}, r)
	j.DefaultBranch = "trunk"
	f.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	j.NewReport = &state.NewReport{Path: f.dir}
	mainBefore := f.git("rev-parse", "main")
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	if pr, ok := resultOf(res, "patch"); !ok || pr.Outcome != gate.Pass || pr.Level != gate.LevelWarn {
		t.Errorf("补丁历史和档位看 trunk：%+v", res.Results)
	}
	if ur, ok := resultOf(res, "ui"); !ok || ur.Outcome != gate.Pass || !strings.Contains(strings.Join(ur.Evidence, "\n"), "用 trunk 上的") {
		t.Errorf("界面脚本看 trunk：%+v", res.Results)
	}
	if sr, ok := resultOf(res, "smoke"); !ok || sr.Outcome != gate.Pass || !strings.Contains(strings.Join(sr.Evidence, "\n"), "用 trunk 上的") {
		t.Errorf("烟测脚本看 trunk：%+v", res.Results)
	}
	if f.git("rev-parse", "main") != mainBefore {
		t.Error("main 不能动")
	}
	if !f.isAncestor(res.Merge, "trunk") {
		t.Error("合进 trunk")
	}
}

// 主目录不在记下的分支上：门跑完不合并，走 WaitBranch，切回来下次合。
func TestGatesWaitForRecordedBranch(t *testing.T) {
	f := newUIFix(t, `{"levels":{"review":"off"}}`, uiReply("pass", "loaded"))
	writePNG(t, filepath.Join(f.shots, "ok.png"), 200, 100, 500)
	f.script(f.copyShots())
	j := f.claim("做事", "agent/x", uiFiles(), ready("agent/x"))
	j.DefaultBranch = "main"
	f.git("checkout", "-q", "-b", "别的")
	res := f.run(j)
	if res.Outcome != "wait" || res.WaitBranch != "main" {
		t.Fatalf("%+v", res)
	}
	if _, ok := resultOf(res, "ui"); !ok {
		t.Errorf("门照跑：%+v", res.Results)
	}
	f.git("checkout", "-q", "main")
	if res := f.run(j); res.Outcome != "merged" {
		t.Fatalf("切回来就合：%+v", res)
	}
}
