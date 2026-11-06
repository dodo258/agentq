package gate_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gate"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// commitAt 在 dir 上提交 files，提交时间是 ago 之前。
func (f *fix) commitAt(dir string, ago time.Duration, msg string, files map[string]string) {
	f.t.Helper()
	for name, text := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	testkit.GitT(f.t, f.home, dir, "add", "-A")
	when := time.Now().Add(-ago).Format(time.RFC3339)
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+f.home, "GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("提交：%v %s", err, out)
	}
}

// levels 在默认分支上写门的档位。
func (f *fix) levels(body string) {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.dir, gate.GatesFile), []byte(body), 0o644)
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "调档位")
}

// patched 造一个反复修的模块：core/ 30 天内修过两次。
func patched(f *fix) {
	f.t.Helper()
	f.commitAt(f.dir, 20*24*time.Hour, "加 core", map[string]string{"core/a.go": "package core\n\nvar A = 1\n"})
	f.commitAt(f.dir, 10*24*time.Hour, "修复：A 算错", map[string]string{"core/a.go": "package core\n\nvar A = 2\n"})
	f.commitAt(f.dir, 5*24*time.Hour, "fix: A 还是不对", map[string]string{"core/a.go": "package core\n\nvar A = 3\n"})
}

func fixReady(branch string) gate.Ready {
	return gate.Ready{Branch: branch, Type: "fix", Acceptance: "看 core", Progress: "修好了"}
}

func TestModuleOf(t *testing.T) {
	cases := map[string]string{
		"main.go":                 "main.go",
		"internal/gate/gate.go":   "internal/gate",
		"src/components/Foo.tsx":  "src/components",
		"中文目录/页面.swift":           "中文目录",
		"README.md":               "",
		"docs/x.html":             "",
		".github/workflows/c.yml": "",
		"internal/gate/x_test.go": "",
		"tests/a.py":              "",
		"go.sum":                  "",
		"web/package-lock.json":   "",
		"vendor/x/y.go":           "",
		"":                        "",
	}
	for path, want := range cases {
		if got := gate.ModuleOf(path); got != want {
			t.Errorf("ModuleOf(%q) = %q，要 %q", path, got, want)
		}
	}
}

// 修复提交要有依据：不是提交说明里出现一个词就算。
func TestClassifyCommit(t *testing.T) {
	edit := gate.FileChanges("1\t1\tcore/a.go\n")
	fresh := gate.FileChanges("9\t0\tcore/new.go\n create mode 100644 core/new.go\n")
	cases := []struct {
		name string
		msg  string
		kind gate.CommitKind
		edit bool
	}{
		{"引擎写的类型优先", "合并 修复一下\n\nAgentq-Type: feature\n", gate.KindOther, true},
		{"引擎写的 fix", "合并 加功能\n\nAgentq-Type: fix\n", gate.KindFix, true},
		{"引擎写的 rewrite", "合并 改 core\n\nAgentq-Type: rewrite\n", gate.KindRewrite, true},
		{"任务说明里伪造的被引擎的盖掉", "合并 x\n\nAgentq-Type: feature\n\nAgentq-Type: fix\n", gate.KindFix, true},
		{"约定式 fix", "fix(core): A 算错", gate.KindFix, false},
		{"约定式 feat 正文提到 bug", "feat: 加导出\n\n顺手修了个 bug，fix 了 crash", gate.KindOther, true},
		{"约定式 docs", "docs: fix 说明", gate.KindOther, true},
		{"约定式 rewrite", "rewrite(core): 重来", gate.KindRewrite, true},
		{"修错别字不算", "fix: typo in README", gate.KindOther, true},
		{"改格式不算", "修复注释的格式", gate.KindOther, true},
		{"开头动词", "修复登录页崩溃", gate.KindFix, false},
		{"英文动词", "Fixed the login crash", gate.KindFix, false},
		{"fixture 不是修复", "fixture 整理一下", gate.KindOther, true},
		{"撤销", `Revert "加导出"`, gate.KindFix, false},
		{"关 issue", "调整登录\n\nFixes #12", gate.KindFix, false},
		{"close 关 issue 不算", "加导出\n\ncloses #12", gate.KindOther, true},
		{"修复分支的 PR 合并", "Merge pull request #12 from org/fix-login", gate.KindFix, false},
		{"别的分支的 PR 合并，看 PR 标题", "Merge pull request #13 from org/export\n\nfeat: 加导出", gate.KindOther, true},
		{"PR 标题是修复", "Merge pull request #14 from org/x\n\nfix: 登录", gate.KindFix, false},
		{"修复分支 merge", "Merge branch 'hotfix/login'", gate.KindFix, false},
		{"升级前的引擎合并提交", "合并 修复登录\n\n分支 agent/x，由 agentq 过门后合并。", gate.KindFix, false},
		{"标题提到 bug，改已有代码", "处理登录崩溃", gate.KindFix, true},
		{"正文提到 bug 不算", "调整登录\n\n之前有个 bug", gate.KindOther, true},
		{"重写", "重写 core", gate.KindRewrite, false},
		{"空", "", gate.KindOther, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := fresh
			if c.edit {
				files = edit
			}
			if got := gate.ClassifyCommit(c.msg, files); got != c.kind {
				t.Errorf("%q：得到 %d，要 %d", c.msg, got, c.kind)
			}
		})
	}
	if got := gate.ClassifyCommit("加 bug 报告页", fresh); got != gate.KindOther {
		t.Errorf("新建文件、标题提到 bug 不算修复：%d", got)
	}
	if got := gate.ClassifyCommit("加 bug 报告页", edit); got != gate.KindFix {
		t.Errorf("改已有代码、标题提到 bug 算修复：%d", got)
	}
}

// 验收：warn 不挡，报告里记下来。
func TestPatchGateWarnDoesNotBlock(t *testing.T) {
	f := newFix(t, fixBacklog)
	patched(f)
	j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "package core\n\nvar A = 4\n"}, fixReady("agent/x"))
	before := f.main()
	res := f.run(j)
	if res.Outcome != "merged" || f.main() == before {
		t.Fatalf("warn 不挡：%+v", res)
	}
	r, ok := resultOf(res, "patch")
	if !ok || r.Outcome != gate.Fail || r.Level != gate.LevelWarn || !strings.Contains(r.Summary, "core") {
		t.Fatalf("要记下第三次打补丁：%+v", res.Results)
	}
	if !strings.Contains(strings.Join(r.Evidence, "\n"), "A 还是不对") {
		t.Errorf("证据要有提交：%+v", r.Evidence)
	}
	f.collect(j, res)
	if !strings.Contains(f.out.String(), "patch") {
		t.Errorf("warn 没过要记日志：%s", f.out.String())
	}
}

// 验收：error 退回，合同改成重写该模块；改成 rewrite 再来就过。
func TestPatchGateErrorReturnsForRewrite(t *testing.T) {
	f := newFix(t, fixBacklog)
	patched(f)
	f.levels(`{"levels":{"review":"off","patch":"error"}}`)
	j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "package core\n\nvar A = 4\n"}, fixReady("agent/x"))
	before := f.main()
	res := f.run(j)
	if res.Outcome != "fail" || f.main() != before {
		t.Fatalf("error 档要退回：%+v", res)
	}
	f.collect(j, res)
	if j.Gate.State != state.GateReturned || j.Status != state.StatusInterrupted {
		t.Fatalf("%+v", j.Gate)
	}
	note := f.a.ReturnNote(j)
	for _, want := range []string{"第三次打补丁", "重写报告里点名的模块", "type 写 rewrite", "core"} {
		if !strings.Contains(note, want) {
			t.Errorf("合同缺 %q：\n%s", want, note)
		}
	}

	wt := filepath.Join(f.dir, ".claude", "worktrees", "x")
	r := fixReady("agent/x")
	r.Type = "rewrite"
	f.commit(wt, map[string]string{"core/a.go": "package core\n\nconst A = 4\n"}, r)
	got, head, found, err := f.a.ReadReady(f.dir, "agent/x")
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	f.a.Enqueue(j, got, head)
	res = f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("重写不算补丁：%+v", res)
	}
	if _, ok := resultOf(res, "patch"); ok {
		t.Errorf("重写不跑补丁门：%+v", res.Results)
	}
	if !strings.Contains(f.git("log", "-1", "--format=%B", res.Merge), "Agentq-Type: rewrite") {
		t.Errorf("合并提交要记类型：%s", f.git("log", "-1", "--format=%B", res.Merge))
	}
}

// 被合同要求重写，只改 type 不改代码：tree 没变，之前的审查结论不能沿用，要按重写再审。
func TestRewriteTypeOnlyIsReviewedAgain(t *testing.T) {
	f := newReviewFix(t, fixBacklog, testkit.AllCaps(), reply("pass"))
	fail := true
	register(t, fakeGate{name: "后面的门", applies: true, run: func(gate.Candidate) gate.Result {
		if fail {
			return gate.Result{Outcome: gate.Fail, Summary: "挡一下"}
		}
		return gate.Result{Outcome: gate.Pass}
	}}, gate.LevelError)
	j := f.job(newFiles())
	if res := f.run(j); res.Outcome != "fail" || f.callCount() != 1 {
		t.Fatalf("%+v %d", res, f.callCount())
	}
	r := ready("agent/x")
	r.Type = "rewrite"
	f.commit(filepath.Join(f.dir, ".claude", "worktrees", "x"), nil, r)
	got, head, found, err := f.a.ReadReady(f.dir, "agent/x")
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	f.a.Enqueue(j, got, head)
	fail = false
	if res := f.run(j); res.Outcome != "merged" || f.callCount() != 2 {
		t.Fatalf("要再审一次：%+v %d", res, f.callCount())
	}
	if args := testkit.ReadFile(t, f.args); !strings.Contains(args, "这是重写合同") {
		t.Errorf("提示词要说是重写：%s", args)
	}
}

// 重写过的模块从零算：重写之前的修复不算进第三次。撤掉「重写封顶」这条用例就要失败。
func TestPatchGateAfterRewrite(t *testing.T) {
	f := newFix(t, fixBacklog)
	patched(f)
	f.levels(`{"levels":{"review":"off","patch":"error"}}`)
	f.commitAt(f.dir, 2*24*time.Hour, "rewrite(core): 重来", map[string]string{"core/a.go": "package core\n\nconst A = 3\n"})
	j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "package core\n\nconst A = 4\n"}, fixReady("agent/x"))
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("重写之后第一次修复不是第三次：%+v", res)
	}
}

// 边界：两次修复不到门槛；窗口之外的不算；改的是别的模块不算；只改测试不算；类型不是 fix 不跑。
func TestPatchGateBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fix)
		files map[string]string
		typ   string
		fails bool
		ran   bool
	}{
		{"修过一次", func(f *fix) {
			f.commitAt(f.dir, 5*24*time.Hour, "fix: 第一次", map[string]string{"core/a.go": "1\n"})
		}, map[string]string{"core/a.go": "2\n"}, "fix", false, true},
		{"修过两次是第三次", func(f *fix) { patched(f) }, map[string]string{"core/a.go": "9\n"}, "fix", true, true},
		{"窗口之外", func(f *fix) {
			f.commitAt(f.dir, 50*24*time.Hour, "fix: 很久以前", map[string]string{"core/a.go": "1\n"})
			f.commitAt(f.dir, 40*24*time.Hour, "fix: 也很久", map[string]string{"core/a.go": "2\n"})
		}, map[string]string{"core/a.go": "3\n"}, "fix", false, true},
		{"改别的模块", func(f *fix) { patched(f) }, map[string]string{"other/b.go": "1\n"}, "fix", false, true},
		{"只改测试", func(f *fix) { patched(f) }, map[string]string{"core/a_test.go": "package core\n"}, "fix", false, true},
		{"不是修复", func(f *fix) { patched(f) }, map[string]string{"core/a.go": "9\n"}, "feature", false, false},
		{"文档", func(f *fix) { patched(f) }, map[string]string{"core/x.md": "9\n"}, "docs", false, false},
		{"正文提到 fix 的功能提交不算修过", func(f *fix) {
			f.commitAt(f.dir, 5*24*time.Hour, "feat: 甲\n\nfix 了一点", map[string]string{"core/a.go": "1\n"})
			f.commitAt(f.dir, 4*24*time.Hour, "feat: 乙\n\nfix 了一点", map[string]string{"core/a.go": "2\n"})
		}, map[string]string{"core/a.go": "3\n"}, "fix", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFix(t, fixBacklog)
			c.setup(f)
			f.levels(`{"levels":{"review":"off","test":"off","patch":"error"}}`)
			r := fixReady("agent/x")
			r.Type = c.typ
			j := f.claim("做事", "agent/x", c.files, r)
			res := f.run(j)
			if (res.Outcome == "fail") != c.fails {
				t.Fatalf("%+v", res)
			}
			if _, ok := resultOf(res, "patch"); ok != c.ran {
				t.Errorf("要不要跑：%v，结果 %+v", c.ran, res.Results)
			}
		})
	}
}

// 派活前看的是任务记下的分支：别的分支上的修复不算。
func TestPatchNoteUsesRecordedBranch(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.git("branch", "trunk")
	patched(f) // 修复都在 main 上
	if note := f.a.PatchNote(f.dir, "trunk", "修 core"); note != "" {
		t.Errorf("trunk 上没修过：%q", note)
	}
	if note := f.a.PatchNote(f.dir, "main", "修 core"); note == "" {
		t.Error("main 上修过")
	}
}

// health 认不出默认分支：明确说，别的项目照看。
func TestCmdHealthNoDefaultBranch(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.git("branch", "-m", "main", "trunk")
	var out bytes.Buffer
	f.a.Out = &out
	if err := f.a.CmdHealth([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "demo：认不出默认分支") || !strings.Contains(out.String(), "main、master") {
		t.Errorf("%s", out.String())
	}
}

// 默认分支叫 master 也一样：看的是 Plan 里认好的分支。
func TestPatchGateOnMaster(t *testing.T) {
	f := masterFix(t)
	patched(f)
	f.levels(`{"levels":{"review":"off","patch":"error"}}`)
	j := f.claim("做事", "agent/x", map[string]string{"core/a.go": "9\n"}, fixReady("agent/x"))
	if res := f.run(j); res.Outcome != "fail" {
		t.Fatalf("%+v", res)
	}
}

// 查不了历史算判不了，不算过。
func TestPatchGateUnknownOnGitError(t *testing.T) {
	f := newFix(t, fixBacklog)
	var got gate.Result
	d := gate.Deps{Env: f.a.Env, Git: func(ctx context.Context, dir string, args ...string) (string, error) {
		return "", fmt.Errorf("git 坏了")
	}}
	got = gate.PatchGate(d).Run(context.Background(), gate.Candidate{Type: "fix", ProjectDir: f.dir, Base: "HEAD", Files: []string{"core/a.go"}})
	if got.Outcome != gate.Unknown || !strings.Contains(got.Summary, "git 坏了") {
		t.Fatalf("%+v", got)
	}
}

// 合并提交按相对第一父算一次：任务分支上的小提交不重复计，分支上的「修复」提交也不算主线的修复。
func TestModuleStatsFirstParent(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.git("checkout", "-q", "-b", "side")
	f.commitAt(f.dir, 3*24*time.Hour, "fix: 分支上第一次", map[string]string{"core/a.go": "1\n"})
	f.commitAt(f.dir, 3*24*time.Hour, "fix: 分支上第二次", map[string]string{"core/a.go": "2\n"})
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--no-ff", "-m", "合并 加功能\n\n"+gate.TypeTrailer+": feature\n", "side")
	stats := gate.ModuleStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour))
	if len(stats) != 1 || stats[0].Module != "core" || stats[0].Changes != 1 || stats[0].Fixes != 0 {
		t.Fatalf("%+v", stats)
	}
}

// 派活前：条目提到反复修的模块才加话；条目没提、关着门、查不了都不加。
func TestPatchNote(t *testing.T) {
	f := newFix(t, fixBacklog)
	patched(f)
	if note := f.a.PatchNote(f.dir, "main", "修 core 模块的算错"); !strings.Contains(note, "core（2 次）") || !strings.Contains(note, "重写流程") {
		t.Errorf("提到了要加：%q", note)
	}
	if note := f.a.PatchNote(f.dir, "main", "改 core/a.go 的算错"); note == "" {
		t.Error("写了路径要加")
	}
	for _, item := range []string{"修登录", "hardcore 模式", "core2 的事"} {
		if note := f.a.PatchNote(f.dir, "main", item); note != "" {
			t.Errorf("%q 没提到 core：%q", item, note)
		}
	}
	if note := f.a.PatchNote(filepath.Join(f.dir, "没有这个目录"), "main", "修 core"); note != "" {
		t.Errorf("查不了：%q", note)
	}
	f.levels(`{"levels":{"patch":"off"}}`)
	if note := f.a.PatchNote(f.dir, "main", "修 core"); note != "" {
		t.Errorf("门关着不加：%q", note)
	}
}

// 验收：临时仓库造反复修的文件，agentq health 能报出；重写之后的不算；根目录文件按文件算。
func TestCmdHealth(t *testing.T) {
	f := newFix(t, fixBacklog)
	patched(f)
	f.commitAt(f.dir, 24*time.Hour, "fix: 改根目录", map[string]string{"main.go": "package main\n"})
	var out bytes.Buffer
	f.a.Out = &out
	if err := f.a.CmdHealth([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"demo（main，最近 30 天）", "core\t3\t2\t", "已修过 2 次，再修要重写", "main.go\t1\t1\t", "两周返工率：", "还在校准"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "BACKLOG") || strings.Contains(text, ".agentq") {
		t.Errorf("文档和门脚本不算模块：\n%s", text)
	}
	if err := f.a.CmdHealth([]string{"没有"}); err == nil || !strings.Contains(err.Error(), "没有项目 没有") {
		t.Errorf("%v", err)
	}
	out.Reset()
	if err := f.a.CmdHealth(nil); err != nil || !strings.Contains(out.String(), "demo（") {
		t.Errorf("不带参数看全部：%v\n%s", err, out.String())
	}
}

// 两周返工率：两周内改写刚加的行算返工，改写窗口之前就有的行、过了两周才改的不算。
func TestReworkStats(t *testing.T) {
	f := newFix(t, "# 待办\n\n## 待做\n\n## 已完成\n")
	f.commitAt(f.dir, 90*24*time.Hour, "老代码", map[string]string{"core/old.go": "1\n2\n3\n"})
	f.commitAt(f.dir, 29*24*time.Hour, "加十行", map[string]string{"core/a.go": "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"})
	f.commitAt(f.dir, 25*24*time.Hour, "两周内改两行", map[string]string{"core/a.go": "a\nb\nC\nD\ne\nf\ng\nh\ni\nj\n"})
	f.commitAt(f.dir, 2*24*time.Hour, "过了两周改一行", map[string]string{"core/a.go": "A\nb\nC\nD\ne\nf\ng\nh\ni\nj\n"})
	f.commitAt(f.dir, 2*24*time.Hour, "改老代码", map[string]string{"core/old.go": "1\nX\n3\n"})
	f.commitAt(f.dir, 2*24*time.Hour, "文档不算", map[string]string{"docs/x.md": "1\n"})
	r := gate.ReworkStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour))
	// 新增：10（加十行）+2（改两行）+1+1（老代码那一行）；返工：只有两周内改的那 2 行
	if r.Added != 14 || r.Reworked != 2 {
		t.Fatalf("%+v", r)
	}
}

// 改名不算改写；插入、删除的行号换算对：前面插入之后再改后面的行。
func TestReworkRenameAndOffsets(t *testing.T) {
	f := newFix(t, "# 待办\n\n## 待做\n\n## 已完成\n")
	f.commitAt(f.dir, 10*24*time.Hour, "加", map[string]string{"core/a.go": "1\n2\n3\n4\n5\n"})
	f.git("mv", "core/a.go", "core/b.go")
	f.commitAt(f.dir, 9*24*time.Hour, "改名", nil)
	f.commitAt(f.dir, 8*24*time.Hour, "头上插一行再删掉尾巴", map[string]string{"core/b.go": "0\n1\n2\n3\n4\n"})
	r := gate.ReworkStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour))
	if r.Added != 6 || r.Reworked != 1 {
		t.Fatalf("改名不算、删一行算：%+v", r)
	}
}

// 用户的 git 配置不能改变解析：diff.noprefix 下 diff 头没有 a/、b/。文件末尾没换行的 \\ 行不算内容。
func TestReworkUserConfig(t *testing.T) {
	f := newFix(t, "# 待办\n\n## 待做\n\n## 已完成\n")
	f.commitAt(f.dir, 10*24*time.Hour, "加", map[string]string{"core/a.go": "1\n2\n3"})
	f.commitAt(f.dir, 9*24*time.Hour, "改尾巴", map[string]string{"core/a.go": "1\n2\nX"})
	want := gate.ReworkStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour))
	if want.Added != 4 || want.Reworked != 1 {
		t.Fatalf("%+v", want)
	}
	cfg := filepath.Join(f.home, ".gitconfig")
	old := testkit.ReadFile(t, cfg)
	os.WriteFile(cfg, []byte(old+"[diff]\n\tnoprefix = true\n"), 0o644)
	if got := gate.ReworkStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour)); got != want {
		t.Fatalf("配了 noprefix：%+v，要 %+v", got, want)
	}
}

// 内容长得像 diff 头的行不能认错。
func TestReworkContentLooksLikeHeader(t *testing.T) {
	f := newFix(t, "# 待办\n\n## 待做\n\n## 已完成\n")
	f.commitAt(f.dir, 10*24*time.Hour, "加", map[string]string{"core/a.go": "@@ -1,99 +1,99 @@\n++ b/x\n-- a/x\ndiff --git a/x b/x\n"})
	f.commitAt(f.dir, 9*24*time.Hour, "改", map[string]string{"core/a.go": "@@ -1,99 +1,99 @@\n++ b/y\n-- a/x\ndiff --git a/x b/x\n"})
	r := gate.ReworkStats(t, f.a, f.dir, "refs/heads/main", time.Now().Add(-30*24*time.Hour))
	if r.Added != 5 || r.Reworked != 1 {
		t.Fatalf("%+v", r)
	}
}
