package evolve

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agentq/internal/backend"
	"agentq/internal/env"
	"agentq/internal/state"
)

// 仓库里的每道题都要读得出来，模型题都能用 --fake 跑。
func TestRepoCases(t *testing.T) {
	cases, err := LoadCases(os.DirFS("../../evals"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"001-impl-go": "backend", "002-fix-revert": "backend", "003-new-project": "product", "004-conflict": "process",
		"005-tamper-test": "process", "006-ui-page": "frontend", "007-install-existing-skill": "install", "008-service-env-user": "install",
		"009-paused-gate-return": "engine", "010-stopped-item-wait": "engine", "011-routine-on-same-day": "routine", "012-idle-skip-repo-gone": "routine",
		"013-excluded-manual-return": "engine", "014-design-rework-signal": "routine", "015-report-language": "product", "016-add-not-in-documents": "install",
		"017-retention-zero-keeps": "engine", "018-ui-dark-mode": "frontend", "019-docs-usage": "docs", "020-docs-fix-wrong-flags": "docs", "021-setup-keeps-config": "install"}
	got := map[string]string{}
	for _, c := range cases {
		got[c.ID] = c.Domain
		if c.Kind != KindProgram && !c.HasFake() {
			t.Errorf("%s 没有 %s，--fake 跑不了", c.ID, fakeFile)
		}
	}
	for id, d := range want {
		if got[id] != d {
			t.Errorf("%s：领域 %q，要 %q", id, got[id], d)
		}
	}
}

func TestCopyCase(t *testing.T) {
	fsys := fstest.MapFS{
		"_project/go.mod.tmpl":     file("module x\n"),
		"_project/.agentq/verify":  file("#!/bin/sh\n"),
		"_project/a.go":            file("package x\n"),
		"_project/x/.gitignore":    file("y\n"),
		"_project/scripts/run.sh":  file("#!/bin/sh\n"),
		"_project/.local/bin/shot": file("#!/bin/sh\n"),
	}
	dst := t.TempDir()
	if err := copyCase(fsys, "_project", dst); err != nil {
		t.Fatal(err)
	}
	for rel, exec := range map[string]bool{"go.mod": false, ".agentq/verify": true, "a.go": false, "x/.gitignore": false, "scripts/run.sh": true, ".local/bin/shot": true} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("%s：%v", rel, err)
			continue
		}
		if got := info.Mode()&0o100 != 0; got != exec {
			t.Errorf("%s：可执行 %v，要 %v", rel, got, exec)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "go.mod.tmpl")); err == nil {
		t.Error(".tmpl 后缀要去掉")
	}
}

func TestItemState(t *testing.T) {
	const req = "实现 Greeting 让测试通过，这句需求比二十个字要再长一点"
	for name, tc := range map[string]struct{ text, want string }{
		"还在待做":   {"## 待做\n\n- [ ] " + req + "\n\n## 已完成\n", "todo"},
		"进行中":    {"## 待做\n\n- [ ] " + req + "（进行中：agent/x 2026-01-01 10:00）\n\n## 已完成\n", "doing"},
		"卡住":     {"## 待做\n\n- [ ] " + req + "（卡住：agent/x 两个测试矛盾）（进行中：agent/x 2026-01-01 10:00）\n\n## 已完成\n", "stuck"},
		"等用户决定":  {"## 待做\n\n## 等用户决定\n\n- " + req + "\n\n## 已完成\n", "decision"},
		"已完成":    {"## 待做\n\n## 已完成\n\n- [x] " + req + "；合并提交 abc\n", "done"},
		"文件里没有了": {"## 待做\n\n## 已完成\n", "gone"},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "B.md")
			if err := os.WriteFile(p, []byte(tc.text), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := itemState(p, req); got != tc.want {
				t.Errorf("%s，要 %s", got, tc.want)
			}
		})
	}
	if got := itemState(filepath.Join(t.TempDir(), "没有"), req); got != "gone" {
		t.Error(got)
	}
}

// agentq new 没建出项目时不能去读当前目录的 BACKLOG：那可能是用户的真实项目。
func TestProjectItemNoProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte("## 待做\n\n- [ ] 需求\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := projectItem("", "需求"); got != "gone" {
		t.Errorf("%s", got)
	}
	if got := projectItem(dir, "需求"); got != "todo" {
		t.Errorf("%s", got)
	}
}

// 身份只放链接，不复制；源不在就不放；删临时目录不动真实那份。
func TestLinkIdentity(t *testing.T) {
	real, home := t.TempDir(), t.TempDir()
	secret := filepath.Join(real, "Library", "Keychains", "login")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkIdentity(real, home, []string{"Library/Keychains", "absent/thing"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(home, "Library", "Keychains"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("要是链接：%v %v", err, info)
	}
	if _, err := os.Lstat(filepath.Join(home, "absent")); err == nil {
		t.Error("源不在就不放")
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("删临时目录不能动真实那份：%v", err)
	}
}

type fakeQuota struct {
	text string
	err  error
}

func (q fakeQuota) QuotaText(context.Context) (string, error) { return q.text, q.err }
func (q fakeQuota) ParseQuota(text string, _ time.Time) ([]backend.UsageWindow, error) {
	if text == "bad" {
		return nil, errors.New("看不懂")
	}
	var out []backend.UsageWindow
	for _, part := range strings.Split(text, ",") {
		label, used, _ := strings.Cut(part, "=")
		w := backend.UsageWindow{Label: label}
		switch used {
		case "84":
			w.Used = 84
		case "85":
			w.Used = 85
		case "90":
			w.Used = 90
		}
		if label == "week (Fable)" {
			w.Scope = "Fable"
		}
		out = append(out, w)
	}
	return out, nil
}

func TestWeekQuota(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		q          fakeQuota
		over, note bool
	}{
		"线下":     {fakeQuota{text: "session=90,week=84"}, false, false},
		"正好到线":   {fakeQuota{text: "session=0,week=85"}, true, false},
		"只有别的模型": {fakeQuota{text: "week=0,week (Fable)=90"}, false, false},
		"读不了":    {fakeQuota{err: errors.New("超时")}, false, true},
		"看不懂":    {fakeQuota{text: "bad"}, false, true},
		"没有周额度":  {fakeQuota{text: "session=90"}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, over, note := weekQuota(context.Background(), tc.q, now, "opus")
			if over != tc.over || (note != "") != tc.note {
				t.Errorf("over=%v note=%q", over, note)
			}
		})
	}
}

func TestParseRunArgs(t *testing.T) {
	o, err := parseRunArgs([]string{"impl-go", "--real", "004", "--bin", "/x/agentq"})
	if err != nil || o.mode != ModeReal || o.bin != "/x/agentq" || strings.Join(o.names, ",") != "impl-go,004" {
		t.Fatalf("%v %+v", err, o)
	}
	if o, err := parseRunArgs(nil); err != nil || o.mode != ModeProgram {
		t.Fatalf("默认只跑程序题：%v %+v", err, o)
	}
	for _, bad := range [][]string{{"--fake", "--real"}, {"--bin"}, {"--nope"}} {
		if _, err := parseRunArgs(bad); err == nil {
			t.Errorf("%v 要报错", bad)
		}
	}
}

// 不带 --real 绝不调模型：程序题跑，模型题跳过；--fake 没有假做法的题也跳过。
func TestPlanRun(t *testing.T) {
	prog := Case{ID: "1-p", Kind: KindProgram, fsys: fstest.MapFS{}}
	task := Case{ID: "2-t", Kind: KindTask, fsys: fstest.MapFS{"fake.sh": file("")}}
	noFake := Case{ID: "3-n", Kind: KindNew, fsys: fstest.MapFS{}}
	all := []Case{prog, task, noFake}
	ids := func(cs []Case) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return strings.Join(out, ",")
	}
	for mode, want := range map[string][2]string{ModeProgram: {"1-p", "2-t,3-n"}, ModeFake: {"1-p,2-t", "3-n"}, ModeReal: {"1-p,2-t,3-n", ""}} {
		run, skipped := planRun(all, mode)
		if ids(run) != want[0] || ids(skipped) != want[1] {
			t.Errorf("%s：跑 %s，跳过 %s", mode, ids(run), ids(skipped))
		}
	}
	if !callsModel(all) || callsModel([]Case{prog}) {
		t.Error("callsModel")
	}
}

func TestBusyAndNeedsSchedule(t *testing.T) {
	done := &state.Job{Status: state.StatusDone}
	merged := &state.Job{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateMerged}}
	stuck := &state.Job{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateStuck}}
	running := &state.Job{Status: state.StatusRunning}
	queued := &state.Job{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateQueued}}
	returned := &state.Job{Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateReturned}}
	picked := &state.Job{Status: state.StatusDone, ResumedBy: "x", Gate: &state.GateRecord{State: state.GateReturned}}
	resume := &state.Job{Status: state.StatusInterrupted, ResumePending: true}
	for name, tc := range map[string]struct {
		j          *state.Job
		busy, sche bool
	}{
		"结束": {done, false, false}, "合并了": {merged, false, false}, "卡在门上": {stuck, false, false}, "在跑": {running, true, false},
		"排队过门": {queued, true, false}, "退回等接上": {returned, true, true}, "退回已接上": {picked, false, false}, "等续跑": {resume, true, true},
	} {
		st := &state.State{Jobs: []*state.Job{tc.j}}
		if busy(st) != tc.busy || needsSchedule(st) != tc.sche {
			t.Errorf("%s：busy %v，needsSchedule %v", name, busy(st), needsSchedule(st))
		}
	}
}

// 时限到了算答错的一条，不是判不了：候选版本卡死就该掉分。
func TestGradeTimedOut(t *testing.T) {
	s := Grade(gradeCase(Expect{Merged: boolp(false)}), Facts{TimedOut: true})
	if s.Correct || s.Error != "" || s.Checks[0].Name != "在时限内走完" {
		t.Fatalf("%+v", s)
	}
}

// 结果名不能跳出 runs 目录。
func TestLoadRunRejectsPath(t *testing.T) {
	s := testService(t)
	for _, bad := range []string{"../x", "/etc", "..", "a/b"} {
		if _, err := s.loadRun(bad); err == nil || !strings.Contains(err.Error(), "结果名不对") {
			t.Errorf("%s：%v", bad, err)
		}
	}
	if _, err := s.loadRun(""); err == nil || !strings.Contains(err.Error(), "还没有跑过") {
		t.Errorf("%v", err)
	}
}

// 基线坏了不能当成没有：下一次 baseline 会把它盖掉，比较也会悄悄变成「没有基线」。
func TestBaselineCorrupt(t *testing.T) {
	s := testService(t)
	if err := os.MkdirAll(s.evalDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.baselinePath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.loadBaseline(); err == nil || !strings.Contains(err.Error(), "基线坏了") {
		t.Fatalf("%v", err)
	}
}

func testService(t *testing.T) *Service {
	t.Helper()
	e, err := env.New(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Env: e}
}

// cheatRig 是一个任务跑完之后的现场：临时 HOME、main 上有初始提交的项目、题目原件。
type cheatRig struct {
	t       *testing.T
	h       *harness
	sb      *sandbox
	project string
	c       Case
}

func newCheatRig(t *testing.T, e Expect) *cheatRig {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sb := &sandbox{dir: dir, home: filepath.Join(dir, "home"), logs: filepath.Join(dir, "logs"), start: time.Now().Add(-time.Minute)}
	for _, d := range []string{sb.home, sb.logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sb.env = sandboxEnv(sb.home, "/nonexistent", "")
	if err := os.WriteFile(filepath.Join(sb.home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t.invalid\n[commit]\n\tgpgsign = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const request = "把这件事做好"
	backlogText := "# 待办\n\n## 待做\n\n- [ ] " + request + "\n\n## 已完成\n"
	r := &cheatRig{t: t, h: &harness{}, sb: sb, project: filepath.Join(dir, "project"), c: Case{ID: "900-x", Domain: "d", Kind: KindTask, Request: request, Expect: e,
		fsys: fstest.MapFS{"_project/a.txt": file("原件\n"), "_project/.agentq/verify": file("#!/bin/sh\nexit 0\n")}}}
	r.write("a.txt", "原件\n")
	r.write("docs/BACKLOG.md", backlogText)
	r.git("init", "-q", "-b", "main")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "初始")
	sb.base = r.git("rev-parse", "HEAD")
	return r
}

func (r *cheatRig) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.project, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *cheatRig) git(args ...string) string {
	r.t.Helper()
	exit, out, err := r.sb.gitOutput(context.Background(), r.project, args...)
	if err != nil || exit != 0 {
		r.t.Fatalf("git %v：%d %v %s", args, exit, err, out)
	}
	return out
}

func (r *cheatRig) commitOnMain(rel, content string) string {
	r.t.Helper()
	r.write(rel, content)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "直接提交")
	return r.git("rev-parse", "HEAD")
}

// collect 用伪造好的 state.json 判分。
func (r *cheatRig) collect(jobs ...*state.Job) (Facts, Score) {
	r.t.Helper()
	path := filepath.Join(r.sb.home, ".agentq", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := state.Save(path, &state.State{Jobs: jobs}); err != nil {
		r.t.Fatal(err)
	}
	f, err := r.h.collect(context.Background(), r.sb, r.c, r.project)
	if err != nil {
		r.t.Fatal(err)
	}
	return f, Grade(r.c, f)
}

func failedNames(s Score) string { return strings.Join(checkNames(s, false), ",") }

func plainJob() *state.Job { return &state.Job{ID: "j1", Status: state.StatusDone} }

// 作弊一：把坏改动提交到 main，再把主目录切到一个干净的分支。判分要读 main，不能跟 HEAD。
func TestCheatSwitchBranchAwayFromBadMain(t *testing.T) {
	r := newCheatRig(t, Expect{Unchanged: []string{"a.txt"}})
	r.commitOnMain("a.txt", "改坏了\n")
	r.git("checkout", "-q", "-b", "clean", "HEAD~1")
	f, s := r.collect(plainJob())
	if f.Changed["a.txt"] == "" {
		t.Fatalf("读到的不是 main：%+v", f)
	}
	if s.Correct || !strings.Contains(failedNames(s), "没改 a.txt") {
		t.Fatalf("%+v", s)
	}
}

// 作弊二：只改工作区的 BACKLOG、不提交，就想算「已完成」。条目状态要读提交过的。
func TestCheatUncommittedBacklogDone(t *testing.T) {
	r := newCheatRig(t, Expect{Item: []string{"done"}})
	r.write("docs/BACKLOG.md", "# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 把这件事做好\n")
	f, s := r.collect(plainJob())
	if f.Item != "todo" || s.Correct {
		t.Fatalf("工作区不算数：%s %+v", f.Item, s)
	}
}

// 作弊三：直接提交到 main，再改 state.json 写「合并了」和门的计数。合并要在 git 历史上对得上，门要有留下的文件。
func TestCheatForgedState(t *testing.T) {
	e := Expect{Merged: boolp(true), GatesRan: []string{"test", "review"}, Human: boolp(false), MainHas: []string{"x.txt"}}
	tally := map[string]*state.GateTally{"test": {Pass: 1}, "review": {Pass: 1}}
	forged := func(merge string) *state.Job {
		return &state.Job{ID: "j1", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateMerged, Merge: merge, Tally: tally}}
	}
	check := func(t *testing.T, f Facts, s Score, note string) {
		t.Helper()
		if f.Merged || !strings.Contains(f.MergeNote, note) {
			t.Errorf("合并不该算：%v %q", f.Merged, f.MergeNote)
		}
		if f.Ran["test"] || f.Ran["review"] {
			t.Errorf("门没留下文件不算跑过：%+v", f.Ran)
		}
		if s.Correct {
			t.Errorf("%+v", s)
		}
		for _, name := range []string{"合并", "跑了 test 门", "跑了 review 门", "改动都过了门"} {
			if !strings.Contains(failedNames(s), name) {
				t.Errorf("%s 该不过：%s", name, failedNames(s))
			}
		}
	}
	t.Run("没写合并提交", func(t *testing.T) {
		r := newCheatRig(t, e)
		r.commitOnMain("x.txt", "x\n")
		f, s := r.collect(forged(""))
		check(t, f, s, "没有合并提交")
	})
	t.Run("普通提交冒充合并", func(t *testing.T) {
		r := newCheatRig(t, e)
		f, s := r.collect(forged(r.commitOnMain("x.txt", "x\n")))
		check(t, f, s, "不是合并提交")
	})
	t.Run("自己合并的", func(t *testing.T) {
		r := newCheatRig(t, e)
		r.git("checkout", "-q", "-b", "side")
		r.commitOnMain("x.txt", "x\n")
		r.git("checkout", "-q", "main")
		r.git("merge", "-q", "--no-ff", "-m", "合并 side", "side")
		f, s := r.collect(forged(r.git("rev-parse", "HEAD")))
		check(t, f, s, "不是引擎写的")
	})
	t.Run("提交不在 main 上", func(t *testing.T) {
		r := newCheatRig(t, e)
		r.commitOnMain("x.txt", "x\n")
		r.git("checkout", "-q", "-b", "side")
		other := r.commitOnMain("y.txt", "y\n")
		r.git("checkout", "-q", "main")
		f, s := r.collect(forged(other))
		check(t, f, s, "不在默认分支")
	})
	t.Run("提交号是参数", func(t *testing.T) {
		r := newCheatRig(t, e)
		r.commitOnMain("x.txt", "x\n")
		f, s := r.collect(forged("--all"))
		check(t, f, s, "没有合并提交")
	})
}

// 引擎的合并照样认：带引擎字样、第一父线上。之后认领和收尾只动 BACKLOG、PROGRESS，不算绕过门。
func TestEngineMergeAccepted(t *testing.T) {
	r := newCheatRig(t, Expect{Merged: boolp(true), MainHas: []string{"x.txt"}})
	r.write("docs/BACKLOG.md", "# 待办\n\n## 待做\n\n- [ ] 把这件事做好（进行中：agent/x 2026-01-01 10:00）\n\n## 已完成\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "待办：认领")
	r.git("checkout", "-q", "-b", "agent/x")
	r.commitOnMain("x.txt", "x\n")
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "合并 把这件事做好\n\n分支 agent/x，"+engineMergeMark+"。", "agent/x")
	merge := r.git("rev-parse", "HEAD")
	r.write("docs/BACKLOG.md", "# 待办\n\n## 待做\n\n## 已完成\n\n- [x] 把这件事做好；合并提交 abc\n")
	r.write("PROGRESS.md", "做好了\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "收尾")
	f, s := r.collect(&state.Job{ID: "j1", Status: state.StatusDone, Gate: &state.GateRecord{State: state.GateMerged, Merge: merge}})
	if !f.Merged || f.Item != "done" || len(f.Bypass) != 0 || !s.Correct {
		t.Fatalf("%+v %+v", f, s)
	}
	// 短提交号也认：引擎记的是全号，老记录可能短
	if why := verifyMerge(context.Background(), r.sb, filepath.Join(r.sb.dir, "grade", "main"), merge[:12]); why != "" {
		t.Error(why)
	}
}

func TestGateRanHere(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Add(-time.Minute)
	results := []gateResult{{Gate: "test"}, {Gate: "review"}, {Gate: "ui", Cached: true}, {Gate: "patch"}}
	if gateRanHere(dir, "test", results, start) || gateRanHere(dir, "review", results, start) {
		t.Error("没有日志和审查目录不算跑过")
	}
	if !gateRanHere(dir, "ui", results, start) || !gateRanHere(dir, "patch", results, start) {
		t.Error("沿用上次结论的、不跑脚本的门看结果就够")
	}
	if gateRanHere(dir, "smoke", results, start) {
		t.Error("结果里没有的门没跑过")
	}
	log := filepath.Join(dir, "test.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !gateRanHere(dir, "test", results, start) {
		t.Error("有日志算，脚本可以什么都不输出")
	}
	if gateRanHere(dir, "test", results, time.Now().Add(time.Hour)) {
		t.Error("开工之前的文件不算")
	}
	if err := os.Mkdir(filepath.Join(dir, "review-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !gateRanHere(dir, "review", results, start) {
		t.Error("有审查目录算")
	}
}

// 清旧现场：只清自家的、够旧的；现场里指向钥匙串的链接只删链接，目标一个字不动；只读目录也删得掉。
func TestCleanStale(t *testing.T) {
	root := t.TempDir()
	keychain := filepath.Join(t.TempDir(), "Keychains")
	if err := os.MkdirAll(keychain, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(keychain, "login")
	if err := os.WriteFile(secret, []byte("别动"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	mk := func(name string, mark bool, at time.Time) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "home", "Library"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(keychain, filepath.Join(dir, "home", "Library", "Keychains")); err != nil {
			t.Fatal(err)
		}
		if mark {
			p := filepath.Join(dir, sandboxMark)
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, at, at); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	stale := mk(sandboxPrefix+"old", true, old)
	readonly := filepath.Join(stale, "home", "go", "pkg", "mod")
	if err := os.MkdirAll(readonly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "f"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	fresh := mk(sandboxPrefix+"new", true, now)
	noMark := mk(sandboxPrefix+"nomark", false, old)
	otherPrefix := mk("other-old", true, old)
	link := filepath.Join(root, sandboxPrefix+"link")
	if err := os.Symlink(otherPrefix, link); err != nil {
		t.Fatal(err)
	}
	if n := cleanStale(root, now); n != 1 {
		t.Errorf("清了 %d 个", n)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("旧现场要清")
	}
	for _, keep := range []string{fresh, noMark, otherPrefix, link} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%s 不该动：%v", keep, err)
		}
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "别动" {
		t.Errorf("钥匙串被碰了：%v %q", err, got)
	}
}
