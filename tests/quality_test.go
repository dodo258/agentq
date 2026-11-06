package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/ledger"
	"agentq/internal/quality"
)

// qualityRepo 在 dir 建一个 git 仓库，commit 按给定时间提交并返回短提交号。
func qualityRepo(t *testing.T, dir string) func(msg string, at time.Time) string {
	t.Helper()
	git := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(nil, "init", "-q", "-b", "main")
	return func(msg string, at time.Time) string {
		d := at.Format(time.RFC3339)
		git([]string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}, "commit", "-q", "--allow-empty", "-m", msg)
		return git(nil, "rev-parse", "--short=7", "HEAD")
	}
}

// 有意见、无意见、判断不了三种，加上正文顺带提到「验收」「的意见」的反例、本条自己分支上的提交、窗口外的旧提交。
func TestQuality(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.Local) }
	commit := qualityRepo(t, filepath.Join(root, "p"))

	old := commit("很早以前的功能", day(1, 10)) // 窗口外
	commit("待办：认领 A", day(8, 9))
	a1 := commit("A 实现", day(8, 10))
	a2 := commit("A 审查后修改\n\n按审查改 "+a1+" 的注释", day(8, 11)) // 说明里引用的是本条自己分支上的提交
	commit("待办：A 已合并", day(8, 12))
	commit("待办：认领 B", day(9, 9))
	b1 := commit("B 修回归", day(9, 10))
	commit("待办：认领 C", day(10, 9))
	c1 := commit("C 按验收意见改", day(10, 10))
	d1 := commit("D 修旧问题", day(10, 11))
	e1 := commit("E 跨项目", day(10, 12))
	f1 := commit("F 正文提到验收", day(10, 13))
	g1 := commit("G 第一次", day(10, 14))
	g2 := commit("G 第二次", day(10, 15))
	commit("待办：认领 I", day(11, 8))
	i1 := commit("I 修 B\n\n修 "+b1+" 合进来的回归", day(11, 9)) // 返工写在提交说明里
	j1 := commit("J 改旧的", day(11, 10))

	backlog := `# 待办

## 待做

- [ ] D 的后续 —— 验收「D 修旧问题」的意见：再改一处
- [ ] 补 B —— 验收「B 修上次的回归」的意见
- [ ] 同名的意见 —— 验收「G 同名」的意见
- [ ] K —— 先验收正文提到验收的意见再说；验收「E 跨项目和 F 正文提到验收」的意见
- [ ] 模板 —— 之后没有出现「验收 <该条目> 的意见」类新条目
<!-- - [x] 标题 —— 合并提交 abc1234；验收：去哪看 -->

## 已完成

- [x] A 新功能 —— 合并提交 ` + a2 + `（` + a1 + ` + ` + a2 + `）；验收：看 ` + a1 + `
- [x] B 修上次的回归问题 —— 修上次合并提交 ` + a2 + ` 改出来的问题；合并提交 ` + b1 + `
- [x] C 的改动 —— 验收「A 新功能」的意见；合并提交 ` + c1 + `
- [x] D 修旧问题 —— ` + old + ` 引入的；合并提交 ` + d1 + `；验收：和 ` + b1 + ` 时的样子对照
- [x] E 跨项目 —— 参考别的仓库 deadbe1 的写法；合并提交 ` + e1 + `
- [x] F 正文提到验收 —— 验收：用例覆盖有意见/无意见，CI 运行 1000000001 全绿；不是「验收 <该条目> 的意见」；合并提交 ` + f1 + `
- [x] G 同名 —— 合并提交 ` + g1 + `
- [x] G 同名 —— 合并提交 ` + g2 + `
- [x] I 修 B 的回归 —— 合并提交 ` + i1 + `
- [x] J 改旧的 —— 验收「很早以前」的意见；合并提交 ` + j1 + `
- [x] H 改写历史前的 —— 合并提交 abc1234
- [x] 很早以前 —— 合并提交 ` + old + `
- [x] 只是定了个决定 —— 按推荐 A 定
`
	os.MkdirAll(filepath.Join(root, "p", "docs"), 0o755)
	if err := os.WriteFile(filepath.Join(root, "p", "docs", "BACKLOG.md"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cfg := config.Default()
	cfg.ProjectsRoot = root
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Cfg: cfg, Now: func() time.Time { return now }, Out: &out}})
	r := a.Quality.Quality(now)
	if len(r.Projects) != 1 {
		t.Fatalf("应有 1 个项目：%+v", r.Projects)
	}
	p := r.Projects[0]
	got := map[string]quality.QualityItem{}
	for _, it := range p.Items {
		got[it.Title+"@"+it.Commit] = it
	}
	want := []struct {
		title, commit string
		sat, rework   int
		why           string
	}{
		{"A 新功能", a2, 0, 0, "「C 的改动」"},          // 有意见；引用的 a1 是自己分支上的
		{"B 修上次的回归问题", b1, 0, 1, "引用了 " + a2},   // 意见里只写了标题的一部分；返工：引用 7 天内的 a2
		{"C 的改动", c1, 1, 1, "是「A 新功能」"},         // 无意见；返工：是刚合并条目的验收意见
		{"D 修旧问题", d1, 0, 0, "没有引用 7 天内"},       // 意见在待做里也算；引用的是窗口外的旧提交
		{"E 跨项目", e1, 1, -1, "deadbe1 在仓库里找不到"}, // 返工判断不了
		{"F 正文提到验收", f1, 1, 0, "没有验收意见"},        // 正文顺带提到「验收」「的意见」不算，K 那样「验收」后面没有空白的、意见里的标题只是包含 F 的也不算
		{"G 同名", g1, -1, 0, "不止一条"},             // 意见对得上两条同名的：判断不了
		{"G 同名", g2, -1, 0, "不止一条"},
		{"I 修 B 的回归", i1, 1, 1, "引用了 " + b1 + "（10-09 周五 10:00「B 修上次的回归问题」）"}, // 返工写在提交说明里
		{"J 改旧的", j1, 1, 0, "没有引用 7 天内"},                                      // 是窗口外旧条目的验收意见：不算返工
	}
	if len(p.Items) != len(want) {
		t.Errorf("窗口里应有 %d 条，得到 %d 条：%+v", len(want), len(p.Items), p.Items)
	}
	for _, w := range want {
		it, ok := got[w.title+"@"+w.commit]
		if !ok {
			t.Errorf("缺少「%s」%s", w.title, w.commit)
			continue
		}
		if it.Satisfied != w.sat || it.Rework != w.rework {
			t.Errorf("「%s」一次满意 %d、返工 %d，应为 %d、%d（%s / %s）", w.title, it.Satisfied, it.Rework, w.sat, w.rework, it.SatisfyWhy, it.ReworkWhy)
		}
		if !strings.Contains(it.SatisfyWhy+"；"+it.ReworkWhy, w.why) {
			t.Errorf("「%s」依据应含 %q：%s / %s", w.title, w.why, it.SatisfyWhy, it.ReworkWhy)
		}
	}
	if len(p.NoTime) != 1 || !strings.Contains(p.NoTime[0], "abc1234") {
		t.Errorf("找不到的合并提交应只记 H 一条（注释里的模板不算）：%v", p.NoTime)
	}
	// 满意：C E F I J 是，A B D 否，G 两条判断不了；返工：B C I 是，A D F G G J 否，E 判断不了
	if s := quality.StatsText(p.Items); s != "一次满意率 62%（5/8），判断不了 2 条不计；返工率 33%（3/9），判断不了 1 条不计" {
		t.Errorf("统计行：%s", s)
	}

	if err := a.Quality.CmdQuality(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"合并 10 条：一次满意率 62%（5/8）", "各项目 BACKLOG 里的验收意见共 6 处", "p：合并 10 条", "另有 1 条合并提交在仓库里找不到", "一次满意：判断不了", "返工：是（是「A 新功能」"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("agentq quality 输出缺 %q：\n%s", s, out.String())
		}
	}
}

// 当天汇总末尾的小结带上一次满意率和返工率；当天没有用量记录时只有这一节。
func TestQualityInNightlyFooter(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	commit := qualityRepo(t, filepath.Join(root, "p"))
	m := commit("功能", now.Add(-time.Hour))
	os.MkdirAll(filepath.Join(root, "p", "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "p", "docs", "BACKLOG.md"), []byte("## 已完成\n\n- [x] 功能 —— 合并提交 "+m+"\n"), 0o644)
	cfg := config.Default()
	cfg.ProjectsRoot = root
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir(), Cfg: cfg, Now: func() time.Time { return now }, Out: &bytes.Buffer{}}})
	end := now

	a.Ledger.UpdateCostFooter(&State{Jobs: []*Job{{ID: "j0", EndedAt: &end}}}, now)
	b, _ := os.ReadFile(a.NightlyPath(now))
	s := string(b)
	if !strings.Contains(s, ledger.CostBegin) || strings.Contains(s, "## 当天花费") || !strings.Contains(s, "## 一次满意率和返工率") ||
		!strings.Contains(s, "- p：合并 1 条，一次满意率 100%（1/1）；返工率 0%（0/1）") {
		t.Fatalf("没有用量记录时小结只有一次满意率这一节：\n%s", s)
	}

	st := &State{Jobs: []*Job{{ID: "j1", Project: "p", Title: "甲", EndedAt: &end, Usage: &backend.JobUsage{Input: 1, CostUSD: 1}}}}
	a.Ledger.UpdateCostFooter(st, now)
	a.Ledger.AppendNightlyLine(now, "之后的一行")
	b, _ = os.ReadFile(a.NightlyPath(now))
	s = string(b)
	i, k, q, e := strings.Index(s, "之后的一行"), strings.Index(s, "## 当天花费"), strings.Index(s, "## 一次满意率和返工率"), strings.Index(s, ledger.CostEnd)
	if strings.Count(s, ledger.CostBegin) != 1 || !(i >= 0 && i < k && k < q && q < e) {
		t.Fatalf("应依次是普通行、当天花费、一次满意率和返工率、结束标记：\n%s", s)
	}
}
