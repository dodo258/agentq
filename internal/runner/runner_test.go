package runner

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func TestTaskModel(t *testing.T) {
	fixed := config.Config{Model: "big", LowRiskModel: "small"}
	auto := config.Config{Model: "big", LowRiskModel: "small", ModelPolicy: config.ModelPolicyAuto}
	autoNoLow := config.Config{Model: "big", ModelPolicy: config.ModelPolicyAuto}
	follow := config.Config{ModelPolicy: config.ModelPolicyAuto}
	cases := []struct {
		name        string
		cfg         config.Config
		kind, item  string
		light, want string
	}{
		{"fixed 普通", fixed, "", "加导出", "", "big"},
		{"fixed 低风险", fixed, "", "加测试（低风险） —— x", "", "small"},
		{"fixed 没配 low_risk 不换", config.Config{Model: "big"}, "", "加测试（低风险）", "light", "big"},
		{"fixed 不看后端的轻模型", config.Config{Model: "big"}, "", "文档：改 README", "light", "big"},
		{"fixed 文档前缀不算", fixed, "", "文档：改 README", "", "big"},
		{"fixed 重写不降级，低风险标记也压不过", fixed, "", "重写调度（低风险）", "", "big"},
		{"fixed 留空", config.Config{}, "", "加导出", "light", ""},
		{"auto 普通不降级", auto, "", "加导出", "light", "big"},
		{"auto 低风险用 low_risk", auto, "", "加测试（低风险）", "light", "small"},
		{"auto 没配 low_risk 用后端的", autoNoLow, "", "加测试（低风险）", "light", "light"},
		{"auto 后端也没有就不换", autoNoLow, "", "加测试（低风险）", "", "big"},
		{"auto 文档前缀", autoNoLow, "", "文档：改 README —— 补一节", "light", "light"},
		{"auto docs 前缀", autoNoLow, "", "docs: fix typo", "light", "light"},
		{"auto 重写不降级", auto, "", "重写调度模块（低风险）", "light", "big"},
		{"auto 英文重构不降级", auto, "", "（低风险）Refactor scheduler", "light", "big"},
		{"auto 审查不降级", auto, "", "文档：审查 README", "light", "big"},
		{"auto 正文提到重写不算", auto, "", "加测试（低风险） —— 不是重写", "light", "small"},
		{"auto 标题中间提到文档不算", autoNoLow, "", "改文档生成器核心", "light", "big"},
		{"auto 夜班不降级", auto, config.KindNight, "加测试（低风险）", "light", "big"},
		{"auto 复盘不降级", auto, config.KindReview, "（低风险）", "light", "big"},
		{"auto 对话不降级", auto, state.KindChat, "（低风险）", "light", "big"},
		{"auto 留空时普通活跟随 CLI", follow, "", "加导出", "light", ""},
		{"auto 留空时轻活用后端的", follow, "", "加测试（低风险）", "light", "light"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := TaskModel(c.cfg, c.kind, c.item, c.light); got != c.want {
				t.Fatalf("%q，想要 %q", got, c.want)
			}
		})
	}
}

// 按活分配强度和专用模型：重活用 heavy_effort，低风险用 low_risk_effort，复盘、巡逻分析用各自的模型，没配的照 model、effort。
func TestTaskModelEffort(t *testing.T) {
	cfg := config.Config{Model: "opus", Effort: "high", HeavyEffort: "xhigh", LowRiskModel: "sonnet", LowRiskEffort: "medium",
		ReviewModel: "sonnet", ReviewEffort: "high", PatrolModel: "sonnet"}
	auto := cfg
	auto.ModelPolicy = config.ModelPolicyAuto
	bare := config.Config{Model: "opus", Effort: "high"}
	for _, c := range []struct {
		name       string
		cfg        config.Config
		kind, item string
		model, eff string
	}{
		{"普通", cfg, "", "加导出", "opus", "high"},
		{"重写用 xhigh", cfg, "", "重写调度模块", "opus", "xhigh"},
		{"英文重构用 xhigh", cfg, "", "Refactor scheduler", "opus", "xhigh"},
		{"迁移", cfg, "", "迁移配置格式", "opus", "xhigh"},
		{"低风险", cfg, "", "加测试（低风险）", "sonnet", "medium"},
		{"auto 重活", auto, "", "重写调度（低风险）", "opus", "xhigh"},
		{"fixed 重活压过低风险", cfg, "", "重构解析（低风险）", "opus", "xhigh"},
		{"标题提到审查不算长任务", cfg, "", "修审查门超时", "opus", "high"},
		{"难撤回用 xhigh", cfg, "", "改表结构（难撤回）", "opus", "xhigh"},
		{"跟随 CLI 时重活也不单传强度", config.Config{HeavyEffort: "xhigh"}, "", "重写调度", "", ""},
		{"auto 文档", auto, "", "文档：改 README", "sonnet", "medium"},
		{"复盘", cfg, config.KindReview, "", "sonnet", "high"},
		{"巡逻分析", cfg, state.KindPatrol, "", "sonnet", "medium"},
		{"夜班照主模型", cfg, config.KindNight, "重写调度", "opus", "high"},
		{"对话照主模型", cfg, state.KindChat, "", "opus", "high"},
		{"没配的重活照 effort", bare, "", "重写调度", "opus", "high"},
		{"没配的复盘照主模型", bare, config.KindReview, "", "opus", "high"},
		{"没配的巡逻照主模型", bare, state.KindPatrol, "", "opus", "high"},
	} {
		if m, e := TaskModel(c.cfg, c.kind, c.item, ""); m != c.model || e != c.eff {
			t.Errorf("%s：%s %s，想要 %s %s", c.name, m, e, c.model, c.eff)
		}
	}
}

// 空配置不带模型和强度参数；写了的照传。
func TestTaskRequestModelArgs(t *testing.T) {
	has := func(args []string, flag string) bool { return slices.Contains(args, flag) }
	empty := testkit.ClaudeArgv(t, TaskRequest(config.Default().Fresh(), "P", "demo", "t", "", "s", 0))
	if has(empty, "--model") || has(empty, "--effort") {
		t.Fatalf("新配置不该传：%v", empty)
	}
	set := testkit.ClaudeArgv(t, TaskRequest(config.Default(), "P", "demo", "t", "", "s", 0))
	if !slices.Contains(set, "opus") || !slices.Contains(set, "high") || !has(set, "--model") || !has(set, "--effort") {
		t.Fatalf("老配置照传：%v", set)
	}
}

func TestAutoStuckJob(t *testing.T) {
	for text, want := range map[string]string{
		"甲 —— x（卡住：子代理失控，子代理 25 个，其中 0 个再派子代理，任务 1104-120000-demo，分支 agent/x）": "1104-120000-demo",
		"甲（卡住：子代理失控，子代理 25 个，其中 8 个再派子代理，任务 1104-120000-demo）":                 "1104-120000-demo",
		"甲 —— x（卡住：续跑后又撞预算上限 $150，任务 1104-120000-demo-2，分支 agent/x）":           "1104-120000-demo-2",
		"甲 —— x（卡住：续跑后又撞预算上限 预算，任务 1104-120000-demo）  ":                        "1104-120000-demo",
		"甲 —— x（卡住：两次没过）":                               "",
		"甲 —— x（卡住：子代理失控这事我来看）":                         "",
		"甲 —— x（卡住：子代理失控，用户觉得算了）":                       "",
		"甲 —— 说明（卡住：子代理失控，任务 1104-120000-demo）这种写法怎么解析": "",
		"甲 —— x（进行中：agent/x 10-10）":                     "",
	} {
		if got := autoStuckJob(text); got != want {
			t.Errorf("%q：%q，要 %q", text, got, want)
		}
	}
}

func TestParseProcs(t *testing.T) {
	got := parseProcs("  12   12 Ss   /bin/sh /x/claude -p hi\n  13  12 Z  (sleep)\nnot a line\n 14 12 R+ sleep 30\n")
	if len(got) != 2 || got[0].pid != 12 || got[0].pgid != 12 || got[0].command != "/bin/sh /x/claude -p hi" || got[1].pid != 14 {
		t.Fatalf("%+v", got)
	}
}

func TestBuildTakeoverPrompt(t *testing.T) {
	p := BuildTakeoverPrompt("/d", "甲（进行中：agent/x 10-10）", backlog.OrphanFacts{Branch: "agent/x", Worktree: "/d/.claude/worktrees/x", Ahead: 2})
	for _, want := range []string{"agent/x", "/d/.claude/worktrees/x", "从现状接着做", "不重做", "agent-task", "分支可以合并了没有", "ready.json"} {
		if !strings.Contains(p, want) {
			t.Errorf("应写明 %q：%s", want, p)
		}
	}
	p = BuildTakeoverPrompt("/d", "甲", backlog.OrphanFacts{Branch: "agent/x"})
	if !strings.Contains(p, "worktree 已不在") {
		t.Errorf("没有 worktree 时要说明：%s", p)
	}
}

func TestSummaryParagraph(t *testing.T) {
	cases := map[string]string{
		"做了很多事。\n\n合并了：是\n提交 abc1234\n\n":        "合并了：是 提交 abc1234",
		"做完了。\n\n已合并，提交 abc1234。\n\n另外顺手记了一条待做。": "已合并，提交 abc1234。",
		"做了一半。\n\n继续等通知。":                        "继续等通知。",
		// 前面段落写了提交号、最后是顺带提到合并的审查结论：取带提交号的段落
		"已经合并，提交号 `aaa1111`（PR #1）。\n\n**做了什么**\n- 加了一个功能\n\n第二轮审查结论是可以合并。\n\n**怎么验收**\n1. 看 PR #1":                     "已经合并，提交号 `aaa1111`（PR #1）。",
		"做完了。\n\n合并提交 bbb2222（ccc3333 + bbb2222），只改 README。\n\n审查人说可以合并。":                                               "合并提交 bbb2222（ccc3333 + bbb2222），只改 README。",
		"做完了。\n\n**合并提交**：`abc1234`\n\n审查结论是可以合并。":                                                                      "**合并提交**：`abc1234`",
		"Re-checked.\n\n**Merge result:** merged into main, merge commit `eee5555`.\n\n**How to check:** open the app.": "**Merge result:** merged into main, merge commit `eee5555`.",
		// 不是固定写法的号（大写、太短、后面还连着字母、没有「合并提交/提交号」）不算，照旧取最后一个说到合并的段落
		"提交号 3CCBA86E 跑了测试。\n\n提交号 abc12 太短。\n\n合并提交 abc1234x 不对。\n\n分支 abc1234 留着。\n\n没合并：测试没过。": "没合并：测试没过。",
	}
	for in, want := range cases {
		if got := summaryParagraph(in); got != want {
			t.Errorf("%q 得到 %q，应为 %q", in, got, want)
		}
	}
}

func TestBranchContract(t *testing.T) {
	if got := BranchContract("main"); got != "" {
		t.Fatalf("叫 main 时提示词不变：%q", got)
	}
	got := BranchContract("master")
	for _, want := range []string{"默认分支是 master", "任务分支从 master 开", "从 master 合入最新", "合进 master"} {
		if !strings.Contains(got, want) {
			t.Errorf("没有 %q：%s", want, got)
		}
	}
}

func TestBranchNoteOnMaster(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "master")
	git("commit", "-q", "--allow-empty", "-m", "a")
	git("branch", "agent/empty")
	git("checkout", "-q", "-b", "agent/ahead")
	git("commit", "-q", "--allow-empty", "-m", "b")
	git("checkout", "-q", "master")
	for text, want := range map[string]string{
		"甲（进行中：agent/ahead 2026-10-10 03:00）": "（分支 agent/ahead 比 master 多 1 个提交，没合并）",
		"甲（进行中：agent/empty 2026-10-10 03:00）": "（分支 agent/empty 没有 master 之外的提交）",
	} {
		if got := BranchNote(dir, text); got != want {
			t.Errorf("%q 得到 %q，应为 %q", text, got, want)
		}
	}
	git("branch", "-q", "-m", "master", "trunk")
	if got := BranchNote(dir, "甲（进行中：agent/ahead 2026-10-10 03:00）"); got != "（分支 agent/ahead）" {
		t.Errorf("认不出默认分支时只说分支名：%q", got)
	}
}

func TestBranchNote(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "a")
	git("branch", "agent/empty")
	git("checkout", "-q", "-b", "agent/ahead")
	git("commit", "-q", "--allow-empty", "-m", "b")
	git("checkout", "-q", "main")
	cases := map[string]string{
		"甲（进行中：agent/ahead 2026-10-10 03:00）": "（分支 agent/ahead 比 main 多 1 个提交，没合并）",
		"甲（进行中：agent/empty 2026-10-10 03:00）": "（分支 agent/empty 没有 main 之外的提交）",
		"甲（进行中：agent/gone 2026-10-10 03:00）":  "（分支 agent/gone 不存在）",
		"甲（进行中：-x 2026-10-10）":                "",
		"甲（进行中）":                              "",
	}
	for text, want := range cases {
		if got := BranchNote(dir, text); got != want {
			t.Errorf("%q 得到 %q，应为 %q", text, got, want)
		}
	}
}

func TestBuildPromptAndArgs(t *testing.T) {
	cfg := config.Default()
	p := BuildPrompt("/p/alpha", "条目 —— 验收")
	if !strings.Contains(p, "把 /p/alpha 的 docs/BACKLOG.md「待做」里的这一条做到分支可合并：条目 —— 验收。") ||
		!strings.HasPrefix(p, "你在无人值守模式下工作") || !strings.Contains(p, "~/.claude/skills/agent-task/SKILL.md") {
		t.Errorf("prompt 不对：%s", p)
	}
	argv := func(resume, session, title string) []string {
		return testkit.ClaudeArgv(t, TaskRequest(cfg, p, "alpha", title, resume, session, 0))
	}
	args := argv("", "", "一二三四五六七八九十一二三四五六七八九十多出来")
	joined := strings.Join(args, " ")
	if args[0] != "-p" {
		t.Errorf("新任务不应带 -r：%s", joined)
	}
	if r := argv("", "sid-new", "t"); r[0] != "--session-id" || r[1] != "sid-new" || r[2] != "-p" {
		t.Errorf("新任务应以 --session-id <会话> 开头：%v", r[:3])
	}
	if r := argv("sid-1", "sid-new", "t"); r[0] != "-r" || r[1] != "sid-1" || r[2] != "-p" {
		t.Errorf("续跑应以 -r <会话> 开头、不带 --session-id：%v", r[:3])
	}
	for _, want := range []string{"--model opus", "--effort high", "--permission-mode bypassPermissions", "--output-format json",
		"--disallowed-tools Monitor", "--name agentq alpha 一二三四五六七八九十一二三四五六七八九十"} {
		if !strings.Contains(joined, want) {
			t.Errorf("参数缺 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "多出来") {
		t.Error("--name 里标题应截到 20 字")
	}
}

// 续跑、接手、发版准备、回复续跑都走 ProjectSlot：名额够了还要看范围，没有条目的当没写范围。
func TestProjectSlot(t *testing.T) {
	cfg := config.Default() // max_parallel 2、per_project 2
	st := &state.State{Jobs: []*state.Job{{Project: "a", Status: state.StatusRunning, Item: "甲（范围：x/）"}}}
	cases := []struct {
		project, item string
		ok            bool
	}{
		{"a", "乙（范围：y/）", true},
		{"a", "乙（范围：x/z）", false},
		{"a", "乙", false},
		{"a", "", false},
		{"b", "", true},
	}
	for _, c := range cases {
		if ok, why := ProjectSlot(st, cfg, c.project, c.item); ok != c.ok {
			t.Errorf("%s %q：%v %s", c.project, c.item, ok, why)
		}
	}
	st.Jobs = append(st.Jobs, &state.Job{Project: "b", Status: state.StatusRunning, Item: "丙（范围：q/）"})
	if ok, why := ProjectSlot(st, cfg, "a", "乙（范围：y/）"); ok || !strings.Contains(why, "max_parallel") {
		t.Errorf("全局满了先说名额：%v %s", ok, why)
	}
}
