package tests

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

	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/state"
)

func doneLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- [x] 条目%d —— 正文\n", i)
	}
	return b.String()
}

var noCommit = func(string) (time.Time, bool) { return time.Time{}, false }

func TestPlanArchiveKeepsFirst20(t *testing.T) {
	now := time.Now()
	head := "# 待办\n\n## 待做\n\n- [ ] 没做 —— 合并提交 aaaaaaa\n<!-- - [x] 注释里的 -->\n\n## 以后再说\n\n- [x] 别的节 —— x\n\n"
	tail := "\n<!-- 模板\n- [x] 跨行注释里的\n-->\n\n## 另一节\n\n- [x] 也不动\n"
	in := head + "## 已完成\n\n" + doneLines(23) + tail
	p := bl.PlanArchive(in, "", noCommit, now)
	if p.Kept != 20 || p.Moved != 3 {
		t.Fatalf("应保留 20 移走 3：%+v", p)
	}
	if want := head + "## 已完成\n\n" + doneLines(20) + tail; p.Backlog != want {
		t.Errorf("其他节和注释应字节不变：\n%s", p.Backlog)
	}
	if !strings.HasPrefix(p.Archive, bl.ArchiveHead+"- [x] 条目21 —— 正文\n- [x] 条目22 —— 正文\n- [x] 条目23 —— 正文\n") {
		t.Errorf("归档内容或顺序不对：\n%s", p.Archive)
	}
	// 幂等：再跑一次什么都不移
	again := bl.PlanArchive(p.Backlog, p.Archive, noCommit, now)
	if again.Moved != 0 || again.Backlog != p.Backlog {
		t.Errorf("第二次不应再动：%+v", again)
	}
	// 刚好 20 条不动
	if p := bl.PlanArchive("## 已完成\n"+doneLines(20), "", noCommit, now); p.Moved != 0 || p.Archive != "" {
		t.Errorf("20 条不该归档：%+v", p)
	}
}

// 14 天内多留（含刚好 14 天）；超过 14 天、没写合并提交、提交查不到的都移走；查不到时间的验收意见留下。
func TestPlanArchiveWindow(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	times := map[string]time.Time{
		"aaaaaa1": now.Add(-bl.ArchiveKeepFor),
		"aaaaaa2": now.Add(-bl.ArchiveKeepFor - time.Minute),
		"aaaaaa3": now.Add(-time.Hour),
	}
	lookup := func(h string) (time.Time, bool) { c, ok := times[h]; return c, ok }
	in := "## 已完成\n" + doneLines(20) +
		"- [x] 刚好14天 —— 合并提交 aaaaaa1\n" +
		"- [x] 超过14天 —— 合并提交 aaaaaa2\n" +
		"- [x] 需求提到合并提交 aaaaaa3 —— 本条合并提交 aaaaaa2\n" + // 只认最后一处
		"- [x] 没写号 —— 正文\n" +
		"- [x] 找不到的号 —— 合并提交 bbbbbbb\n" +
		"- [x] 意见没有号 —— 验收 某条 的意见\n" +
		"- [x] 意见找不到号 —— 验收 某条 的意见；合并提交 bbbbbbb\n" +
		"- [x] 旧意见 —— 验收 某条 的意见；合并提交 aaaaaa2\n" +
		"- [X] 大写新的 —— 合并提交 aaaaaa3\n"
	p := bl.PlanArchive(in, "", lookup, now)
	for _, s := range []string{"刚好14天", "意见没有号", "意见找不到号", "大写新的"} {
		if !strings.Contains(p.Backlog, s) || strings.Contains(p.Archive, s) {
			t.Errorf("%s 应留下", s)
		}
	}
	for _, s := range []string{"超过14天", "需求提到合并提交", "没写号", "找不到的号", "旧意见"} {
		if strings.Contains(p.Backlog, s) || !strings.Contains(p.Archive, s) {
			t.Errorf("%s 应移走", s)
		}
	}
}

// 续行跟条目走；空行、注释不算续行；合并提交写在续行里也认；CRLF 原样；放到已有归档第一条之前。
func TestPlanArchiveContinuationAndExisting(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	recent := now.Add(-time.Hour)
	lookup := func(h string) (time.Time, bool) {
		if h == "ccccccc" {
			return recent, true
		}
		return time.Time{}, false
	}
	in := "## 已完成\n" + doneLines(20) +
		"- [x] 旧条目\n  - 续行一\n\t- 续行二\n\n  <!-- 注释不跟 -->\n" +
		"- [x] 续行里有号\n  - 合并提交 ccccccc\n" +
		"- [x] CRLF 条目\r\n  - 续行\r\n" +
		"- [x] 没有换行结尾"
	old := "# 待办归档\n\n说明\n\n- [x] 更旧的\n"
	p := bl.PlanArchive(in, old, lookup, now)
	want := "# 待办归档\n\n说明\n\n- [x] 旧条目\n  - 续行一\n\t- 续行二\n- [x] CRLF 条目\r\n  - 续行\r\n- [x] 没有换行结尾\n- [x] 更旧的\n"
	if p.Archive != want {
		t.Errorf("归档：\n%q", p.Archive)
	}
	wantB := "## 已完成\n" + doneLines(20) + "\n  <!-- 注释不跟 -->\n- [x] 续行里有号\n  - 合并提交 ccccccc\n"
	if p.Backlog != wantB {
		t.Errorf("BACKLOG：\n%q", p.Backlog)
	}
	// 归档文件没有条目、也没有结尾换行时接在后面
	if got := bl.PrependArchive("# 头", []string{"- [x] a\n"}); got != "# 头\n- [x] a\n" {
		t.Errorf("%q", got)
	}
}

func archiveGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// archiveFixture 在临时 HOME 下建项目 p，写好 BACKLOG 并提交。
func archiveFixture(t *testing.T, backlog string) (*App, *bytes.Buffer, bl.Project) {
	t.Helper()
	home := t.TempDir()
	var out bytes.Buffer
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: &out, Cfg: config.Default()}})
	a.Cfg.ProjectsRoot = filepath.Join(home, "projects")
	if err := os.MkdirAll(a.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p := bl.Project{Name: "p", Dir: filepath.Join(a.Cfg.ProjectsRoot, "p")}
	qualityRepo(t, p.Dir)
	writeBacklogCommit(t, p, backlog)
	return a, &out, p
}

func writeBacklogCommit(t *testing.T, p bl.Project, backlog string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.BacklogPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.BacklogPath(), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	archiveGit(t, p.Dir, "add", "docs/BACKLOG.md")
	archiveGit(t, p.Dir, "commit", "-q", "-m", "backlog")
}

func TestCmdBacklogArchive(t *testing.T) {
	ctx := context.Background()
	backlog := "## 待做\n\n## 已完成\n" + doneLines(22)
	a, out, p := archiveFixture(t, backlog)
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(b)
	}

	// --dry-run 只打印
	if err := a.CmdBacklog(ctx, []string{"archive", "p", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if read(p.BacklogPath()) != backlog || read(p.ArchivePath()) != "" {
		t.Error("--dry-run 不该改文件")
	}
	if !strings.Contains(out.String(), "移走 2 条") || !strings.Contains(out.String(), "--dry-run") {
		t.Errorf("dry-run 输出：%s", out.String())
	}

	// 未提交改动：拒绝，文件不动；--dry-run 报会被拒绝
	dirty := backlog + "\n"
	if err := os.WriteFile(p.BacklogPath(), []byte(dirty), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "未提交") {
		t.Errorf("有未提交改动应拒绝：%v", err)
	}
	if read(p.BacklogPath()) != dirty || read(p.ArchivePath()) != "" {
		t.Error("拒绝时不该改文件")
	}
	out.Reset()
	if err := a.CmdBacklog(ctx, []string{"archive", "p", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "实际执行会被拒绝") {
		t.Errorf("dry-run 应说明会被拒绝：%s", out.String())
	}
	archiveGit(t, p.Dir, "checkout", "--", "docs/BACKLOG.md")

	// 有任务在跑：拒绝；别的项目的任务、夜班不算
	st := &State{Jobs: []*Job{{ID: "j1", Project: "p", Status: state.StatusRunning}}}
	if err := state.Save(a.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "正在跑") {
		t.Errorf("有任务在跑应拒绝：%v", err)
	}
	st.Jobs = []*Job{{ID: "j2", Project: "q", Status: state.StatusRunning}, {ID: "j3", Project: "p", Kind: "night", Status: state.StatusRunning}, {ID: "j4", Project: "p", Status: state.StatusDone}}
	if err := state.Save(a.StatePath(), st); err != nil {
		t.Fatal(err)
	}

	// 真执行：移走 2 条，不提交
	out.Reset()
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(p.ArchivePath()), "条目22") || strings.Contains(read(p.BacklogPath()), "条目21") {
		t.Errorf("归档结果不对：\n%s", read(p.ArchivePath()))
	}
	if n := archiveGit(t, p.Dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("不该自动提交，提交数 %s", n)
	}
	// 没提交就再跑：归档文件有改动，拒绝
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "未提交") {
		t.Errorf("归档结果没提交时应拒绝：%v", err)
	}
	// 提交后再跑：幂等，文件不变
	archiveGit(t, p.Dir, "add", "docs")
	archiveGit(t, p.Dir, "commit", "-q", "-m", "归档")
	b1, b2 := read(p.BacklogPath()), read(p.ArchivePath())
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err != nil {
		t.Fatal(err)
	}
	if read(p.BacklogPath()) != b1 || read(p.ArchivePath()) != b2 {
		t.Error("再跑一次不该变")
	}
	if s := archiveGit(t, p.Dir, "status", "--porcelain"); s != "" {
		t.Errorf("再跑一次不该留下改动：%s", s)
	}
}

func TestCmdBacklogArchiveMarks(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	mark := func(at time.Time) string {
		return "## 待做\n\n从上往下做，标「（进行中：分支名 时间）」。\n\n- [ ] 做着 —— x（进行中：agent/x " + at.Format("2006-01-02 15:04") + "）\n\n## 已完成\n" + doneLines(21)
	}
	a, out, p := archiveFixture(t, mark(now.Add(-time.Hour)))
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "进行中") {
		t.Errorf("新的进行中标记应拒绝：%v", err)
	}
	writeBacklogCommit(t, p, "## 待做\n\n- [ ] 做着（进行中：agent/x 今天下午）\n\n## 已完成\n"+doneLines(21))
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "进行中") {
		t.Errorf("读不出时间的标记应拒绝：%v", err)
	}
	writeBacklogCommit(t, p, mark(now.Add(-bl.ArchiveMarkFresh-time.Minute)))
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err != nil {
		t.Errorf("过期标记不该拒绝：%v", err)
	}
	if !strings.Contains(out.String(), "1 个进行中标记超过 24h") {
		t.Errorf("应说明过期标记：%s", out.String())
	}
}

func TestCmdBacklogArchiveArgs(t *testing.T) {
	ctx := context.Background()
	a, out, p := archiveFixture(t, "## 已完成\n"+doneLines(21))
	for _, args := range [][]string{{"x", "p"}, {"archive", "p", "--all"}, {"archive", "p", "q"}, {"archive", "--force", "p"}} {
		if err := a.CmdBacklog(ctx, args); err == nil {
			t.Errorf("%v 应报错", args)
		}
	}
	if err := a.CmdBacklog(ctx, []string{"archive", "没有这个"}); err == nil || !strings.Contains(err.Error(), "没有项目") {
		t.Errorf("项目不存在：%v", err)
	}
	// --all：没有 BACKLOG 的项目跳过
	qualityRepo(t, filepath.Join(a.Cfg.ProjectsRoot, "没有待办"))
	if err := a.CmdBacklog(ctx, []string{"archive", "--all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.ArchivePath()); err != nil {
		t.Error("--all 应处理 p")
	}
	if strings.Contains(out.String(), "没有待办：") {
		t.Errorf("没有 BACKLOG 的项目应跳过：%s", out.String())
	}
	// 损坏的 state.json 当未知，不归档
	if err := os.WriteFile(a.StatePath(), []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.CmdBacklog(ctx, []string{"archive", "p"}); err == nil || !strings.Contains(err.Error(), "未知") {
		t.Errorf("状态文件坏了应拒绝：%v", err)
	}
}

// 同一份 fixture 归档前后 agentq quality 输出相同：窗口内条目（含有验收意见的）都在 14 天保留范围里，
// 指向窗口内条目、自身没有合并号的验收意见排在 20 条之外也不移走。
func TestArchiveKeepsQualityOutput(t *testing.T) {
	now := time.Now()
	day := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }
	a, out, p := archiveFixture(t, "placeholder\n")
	a.Now = func() time.Time { return now } // quality 的第一行带到分钟的当前时间：两次之间跨分钟不算归档改了输出
	commit := qualityRepo(t, p.Dir)
	var done strings.Builder
	for i := 1; i <= 20; i++ {
		h := commit(fmt.Sprintf("新条目 %d", i), day(1).Add(time.Duration(i)*time.Minute)) // 时间各不相同：quality 同一时刻的条目顺序不固定
		fmt.Fprintf(&done, "- [x] 新条目%d —— 合并提交 %s\n", i, h)
	}
	fmt.Fprintf(&done, "- [x] 窗口内有意见 —— 合并提交 %s\n", commit("窗口内", day(3)))
	done.WriteString("- [x] 对它的意见 —— 验收 窗口内有意见 的意见；没改\n")
	fmt.Fprintf(&done, "- [x] 十天前 —— 合并提交 %s\n", commit("十天前", day(10)))
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&done, "- [x] 很旧%d —— 合并提交 %s\n", i, commit(fmt.Sprintf("很旧 %d", i), day(30+i)))
	}
	done.WriteString("- [x] 没写号的旧条目 —— 正文\n")
	writeBacklogCommit(t, p, "## 待做\n\n- [ ] 下一条 —— x\n\n## 已完成\n\n"+done.String())

	quality := func() string {
		out.Reset()
		if err := a.Quality.CmdQuality(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	before := quality()
	if !strings.Contains(before, "合并 21 条") || !strings.Contains(before, "一次满意：否（有验收意见") {
		t.Fatalf("fixture 没覆盖到：\n%s", before)
	}
	out.Reset()
	if err := a.CmdBacklog(context.Background(), []string{"archive", "p"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "移走 6 条") {
		t.Fatalf("归档输出：%s", out.String())
	}
	if after := quality(); after != before {
		t.Errorf("归档改变了 quality 输出：\n前：\n%s\n后：\n%s", before, after)
	}
}
