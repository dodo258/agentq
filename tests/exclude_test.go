package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bl "agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
	"agentq/internal/textfmt"
)

const excludeBacklog = "# 待办\n\n## 待做\n\n- [ ] 碰不得的活 —— 细节\n\n## 等用户决定\n\n- [ ] 要不要改 —— 选哪个\n\n## 已完成\n"

func excludeEnv(t *testing.T, cfg map[string]any) (*testEnv, bl.Project, bl.Project) {
	t.Helper()
	full := map[string]any{"exclude_projects": []string{"secret"}, "paused": false}
	for k, v := range cfg {
		full[k] = v
	}
	e, _ := sgEnv(t, full)
	secret := e.sgProject("secret", excludeBacklog)
	open := e.sgProject("open", "# 待办\n\n## 待做\n\n- [ ] 能做的活 SLEEP=1\n\n## 已完成\n")
	return e, secret, open
}

// tick 不接被排除项目的活，别的项目照常；进行中标记也不碰。
func TestExcludeTickLeavesProjectAlone(t *testing.T) {
	e, secret, _ := excludeEnv(t, map[string]any{"suggest_enabled": false})
	stale := strings.Replace(excludeBacklog, "- [ ] 碰不得的活 —— 细节", "- [ ] 碰不得的活 —— 细节（进行中：agent/x 2020-01-01 10:00）", 1)
	testkit.WriteInstall(t, secret.BacklogPath(), stale, 0o644)
	testkit.GitT(t, e.home, secret.Dir, "commit", "-q", "-am", "claim")
	head := testkit.GitT(t, e.home, secret.Dir, "rev-parse", "HEAD")
	e.mustRun("tick")
	e.waitShells()
	var projects []string
	for _, j := range e.state().Jobs {
		projects = append(projects, j.Project)
	}
	if strings.Join(projects, ",") != "open" {
		t.Fatalf("只该接 open：%v", projects)
	}
	e.mustRun("tick")
	testkit.AssertBytes(t, secret.BacklogPath(), stale)
	if got := testkit.GitT(t, e.home, secret.Dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("不该往被排除的项目提交")
	}
}

// agentq run 明确指定时仍可跑，提示一句；tick 之后不接着续跑它。
func TestExcludeExplicitRun(t *testing.T) {
	e, secret, _ := excludeEnv(t, map[string]any{"suggest_enabled": false, "paused": true})
	testkit.WriteInstall(t, secret.BacklogPath(), strings.Replace(excludeBacklog, "碰不得的活 —— 细节", "碰不得的活 INTERRUPT", 1), 0o644)
	testkit.GitT(t, e.home, secret.Dir, "commit", "-q", "-am", "x")
	out := e.mustRun("run", "secret")
	testkit.AssertContains(t, out, "exclude_projects", "已启动任务")
	e.waitShells()
	if err := config.SetPaused(filepath.Join(e.home, ".agentq", "config.json"), false); err != nil {
		t.Fatal(err)
	}
	e.mustRun("tick")
	e.mustRun("tick")
	n := 0
	for _, j := range e.state().Jobs {
		if j.Project == "secret" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("中断后 tick 不该自动续跑被排除的项目：%d 个任务", n)
	}
}

// status 列出被排除的项目，不把它算进待做。
func TestExcludeStatus(t *testing.T) {
	e, _, _ := excludeEnv(t, map[string]any{"suggest_enabled": false, "paused": true})
	out := e.mustRun("status")
	testkit.AssertContains(t, out, "exclude_projects", "secret", "open：")
	if strings.Contains(out, "secret：") {
		t.Fatalf("不该算进待做：\n%s", out)
	}
	// 没配时不多一行
	e2, _ := sgEnv(t, map[string]any{"suggest_enabled": false, "paused": true})
	e2.sgProject("open", excludeBacklog)
	if out := e2.mustRun("status"); strings.Contains(out, "exclude_projects") {
		t.Fatalf("没配不该提：\n%s", out)
	}
}

// 闲时找活：提示词里没有它，模型硬写也写不进去；点名看它时拒绝。
func TestExcludeSuggest(t *testing.T) {
	e, secret, open := excludeEnv(t, nil)
	testkit.WriteInstall(t, open.BacklogPath(), "# 待办\n\n## 待做\n\n## 已完成\n", 0o644)
	testkit.GitT(t, e.home, open.Dir, "commit", "-q", "-am", "empty")
	before := testkit.ReadFile(t, secret.BacklogPath())
	e.suggestOut(suggest.SuggestItem{Project: "secret", Title: "硬写进去", Why: "因为", Benefit: "好", Risk: "无", Kind: "test"},
		suggest.SuggestItem{Project: "open", Title: "正常写进去", Why: "因为", Benefit: "好", Risk: "无", Kind: "test"})
	e.mustRun("suggest")
	e.waitShells()
	e.mustRun("tick")
	testkit.AssertBytes(t, secret.BacklogPath(), before)
	testkit.AssertContains(t, testkit.ReadFile(t, open.BacklogPath()), "正常写进去")
	logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	if len(logs) == 0 {
		t.Fatal("假 claude 没被拉起")
	}
	for _, f := range logs {
		if b := testkit.ReadFile(t, f); strings.Contains(b, "ARG[-p]") && strings.Contains(b, "碰不得的活") {
			t.Fatalf("提示词里不该有被排除项目的内容：%s", textfmt.TruncRunes(b, 400))
		}
	}
	out, code := e.run("suggest", "secret")
	if code == 0 {
		t.Fatalf("点名看被排除的项目应拒绝：\n%s", out)
	}
	testkit.AssertContains(t, out, "secret")
}

// 主动感知：不读被排除项目对应的 App，不往里写建议。对照：不排除时能读到。
func TestExcludeSense(t *testing.T) {
	run := func(exclude []string) string {
		s := newSenseEnv(t, map[string]any{"exclude_projects": exclude})
		s.alpha()
		return s.mustRun("sense", "--dry-run")
	}
	if got := run(nil); !strings.Contains(got, "alpha") {
		t.Fatalf("对照：不排除时该读到 alpha：\n%s", got)
	}
	if got := run([]string{"alpha"}); strings.Contains(got, "alpha") {
		t.Fatalf("排除后不该读：\n%s", got)
	}
}

func excludeUnitApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	a, _, _, out := onboardApp(t)
	a.Cfg.ExcludeProjects = []string{"secret"}
	for _, name := range []string{"secret", "open"} {
		dir := onboardRepo(t, a, name, true)
		testkit.WriteInstall(t, filepath.Join(dir, "docs", "BACKLOG.md"), excludeBacklog, 0o644)
		onboardGit(t, a.Home, dir, "add", ".")
		onboardGit(t, a.Home, dir, "commit", "-q", "-m", "backlog")
	}
	return a, out
}

// 决定项、Router、接入和加待做：被排除的项目不进自动流程；明确写的 add 和 init 照做并提示。
func TestExcludeDecisionsRouterAndHints(t *testing.T) {
	a, out := excludeUnitApp(t)
	decs, fails := a.Notify.Decisions()
	if len(fails) != 0 || len(decs) != 1 || decs[0].Project != "open" {
		t.Fatalf("决定项只该有 open：%+v %v", decs, fails)
	}
	if _, err := a.Chat.RouterProject("secret"); err == nil || !strings.Contains(err.Error(), "exclude_projects") {
		t.Fatalf("Router 不该动它：%v", err)
	}
	if _, err := a.Chat.RouterProject("open"); err != nil {
		t.Fatal(err)
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil || len(projects) != 1 || projects[0].Name != "open" {
		t.Fatalf("%+v %v", projects, err)
	}
	if !a.Backlog.Excluded("secret") || a.Backlog.Excluded("open") {
		t.Fatal("excluded 不对")
	}
	out.Reset()
	if err := a.Onboard.CmdAdd([]string{"明确要加", "--project", "secret"}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "已加到 secret", "exclude_projects")
	out.Reset()
	if err := a.Onboard.CmdInit([]string{filepath.Join(a.ProjectsRoot(), "secret")}); err != nil {
		t.Fatal(err)
	}
	testkit.AssertContains(t, out.String(), "exclude_projects")
	out.Reset()
	a.Onboard.StartReport()
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("汇报不该说会接它：%s", out)
	}
}

// 所有自动写 BACKLOG 的路都走 commitBacklog，它就是硬约束的关口；明确点名的 add 不受影响。
func TestExcludeCommitBacklogRefuses(t *testing.T) {
	a, _ := excludeUnitApp(t)
	p := bl.Project{Name: "secret", Dir: filepath.Join(a.ProjectsRoot(), "secret")}
	before := testkit.ReadFile(t, p.BacklogPath())
	err := a.Backlog.CommitBacklog(p, "x", func(old string) (string, error) { return old + "x\n", nil })
	if err == nil || !strings.Contains(err.Error(), "exclude_projects") {
		t.Fatalf("应拒绝：%v", err)
	}
	testkit.AssertBytes(t, p.BacklogPath(), before)
	open := bl.Project{Name: "open", Dir: filepath.Join(a.ProjectsRoot(), "open")}
	if err := a.Backlog.CommitBacklog(open, "x", func(old string) (string, error) { return old + "x\n", nil }); err != nil {
		t.Fatalf("别的项目不受影响：%v", err)
	}
}

// 任务结束后的收尾不写被排除项目：卡住标记、追问的待做。
func TestExcludeJobBookkeepingLeavesBacklog(t *testing.T) {
	a, _ := excludeUnitApp(t)
	dir := filepath.Join(a.ProjectsRoot(), "secret")
	p := bl.Project{Name: "secret", Dir: dir}
	backlog := "# 待办\n\n## 待做\n\n- [ ] 碰不得的活 —— 细节（进行中：agent/x 2026-10-10 10:00）\n\n## 已完成\n\n- [x] 做完的 —— 合并提交 abc1234\n"
	testkit.WriteInstall(t, p.BacklogPath(), backlog, 0o644)
	onboardGit(t, a.Home, dir, "commit", "-q", "-am", "x")
	before := onboardGit(t, a.Home, dir, "rev-parse", "HEAD")
	items, err := p.Items()
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	j := &Job{ID: "1104-000020-secret", Project: "secret", ProjectDir: dir, Title: items[0].Title, Item: items[0].Text, Fingerprint: items[0].Fingerprint, Status: state.StatusInterrupted, StuckPending: "撞了两次预算"}
	os.MkdirAll(a.JobDir(j.ID), 0o755)
	st := &State{Jobs: []*Job{j}}
	a.Runner.RetryStuckMarks(st)
	if j.StuckPending != "" {
		t.Error("不写了就不要每次 tick 再试")
	}
	if got := a.Chat.FollowupTodo(&Job{ID: "1104-000021-secret", ResumeOf: j.ID, Project: "secret", ThreadDir: dir}, "再做一件"); !strings.Contains(got, "exclude_projects") {
		t.Errorf("追问的待做不写：%s", got)
	}
	testkit.AssertBytes(t, p.BacklogPath(), backlog)
	if onboardGit(t, a.Home, dir, "rev-parse", "HEAD") != before {
		t.Error("不该提交")
	}
}

// Router 发文件的路径校验是代码：被排除项目里的文件不发。
func TestExcludeSendFileRefused(t *testing.T) {
	a, _ := excludeUnitApp(t)
	secret := filepath.Join(a.ProjectsRoot(), "secret", "README.md")
	if _, err := a.Chat.CheckSendFile(secret); err == nil || !strings.Contains(err.Error(), "exclude_projects") {
		t.Fatalf("应拒绝：%v", err)
	}
	if _, err := a.Chat.CheckSendFile(filepath.Join(a.ProjectsRoot(), "open", "README.md")); err != nil {
		t.Fatalf("别的项目能发：%v", err)
	}
}

// 真流程：明确跑的任务中断后，tick 和巡逻都不往被排除项目提交，等着的续跑也放掉并说明。
func TestExcludeInterruptedRunEndToEnd(t *testing.T) {
	e, secret, _ := excludeEnv(t, map[string]any{"suggest_enabled": false, "paused": true})
	testkit.WriteInstall(t, secret.BacklogPath(), strings.Replace(excludeBacklog, "碰不得的活 —— 细节", "碰不得的活 INTERRUPT", 1), 0o644)
	testkit.GitT(t, e.home, secret.Dir, "commit", "-q", "-am", "x")
	e.mustRun("run", "secret")
	e.waitShells()
	if err := config.SetPaused(filepath.Join(e.home, ".agentq", "config.json"), false); err != nil {
		t.Fatal(err)
	}
	head := testkit.GitT(t, e.home, secret.Dir, "rev-parse", "HEAD")
	e.mustRun("tick")
	e.mustRun("patrol")
	e.mustRun("tick")
	if got := testkit.GitT(t, e.home, secret.Dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("不该往被排除的项目提交：%s", testkit.GitT(t, e.home, secret.Dir, "log", "-1", "--format=%s"))
	}
	for _, j := range e.state().Jobs {
		if j.Project != "secret" {
			continue
		}
		if j.ResumePending {
			t.Errorf("续跑不会发生，不能一直挂着：%+v", j.ID)
		}
		testkit.AssertContains(t, j.Summary, "exclude_projects")
	}
}

// 模型做的事靠提示词写明跳过：夜班、复盘、对话、Router、小事都要写；SECURITY 和配置参考写明这是软约束。
func TestExcludeProjectsPromptsAndDocs(t *testing.T) {
	for name, text := range map[string]string{"夜班": nightPrompt, "复盘": reviewPrompt, "对话": chat.ChatPromptHead, "Router": chat.RouterHead, "小事": chat.QuickPromptHead} {
		if !strings.Contains(text, "exclude_projects") {
			t.Errorf("%s 的提示词没写跳过 exclude_projects", name)
		}
	}
	if skill := testkit.ReadFile(t, filepath.Join("skills", "nightly-review", "SKILL.md")); !strings.Contains(skill, "exclude_projects") {
		t.Error("复盘技能没写跳过 exclude_projects")
	}
	for _, f := range []string{"SECURITY.md", filepath.Join("docs", "reference", "config.md")} {
		b := testkit.ReadFile(t, f)
		if !strings.Contains(b, "exclude_projects") || !strings.Contains(b, "软约束") {
			t.Errorf("%s 要写明 exclude_projects 对模型是软约束", f)
		}
	}
}
