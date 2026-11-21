package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/gitx"
	"agentq/internal/notify"
	"agentq/internal/textfmt"
)

// subtractRepo 在 projects_root 下造一个真 git 项目（main 分支，有 BACKLOG），第一个提交在 at。
func subtractRepo(t *testing.T, e *notifyEnv, name string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(e.root, name)
	e.write(filepath.Join(dir, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n## 已完成\n")
	subtractGit(t, e, dir, time.Time{}, "init", "-q", "-b", "main")
	subtractCommit(t, e, dir, at, "初始")
	return dir
}

// subtractGit 跑 git，提交时间和作者时间都钉在 at（零值不钉）。
func subtractGit(t *testing.T, e *notifyEnv, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = gitx.GitEnv(e.a.Home)
	if !at.IsZero() {
		d := at.Format(time.RFC3339)
		cmd.Env = append(cmd.Env, "GIT_COMMITTER_DATE="+d, "GIT_AUTHOR_DATE="+d)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// subtractCommit 提交工作区全部改动，返回短提交号。
func subtractCommit(t *testing.T, e *notifyEnv, dir string, at time.Time, msg string) string {
	t.Helper()
	subtractGit(t, e, dir, at, "add", "-A")
	subtractGit(t, e, dir, at, "commit", "-q", "-m", msg)
	return subtractGit(t, e, dir, time.Time{}, "rev-parse", "--short", "HEAD")
}

func nLines(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(prefix + strings.Repeat("x", i%7) + "\n")
	}
	return b.String()
}

// morningOn 推 day 那天的早报（09:00，早报文件已写完），返回这条消息的纯文本；没推出就失败。
func (e *notifyEnv) morningOn(day time.Time) string {
	e.t.Helper()
	e.now = day
	e.at(9, 0)
	e.report(e.now, "早报第一行\n")
	before := e.tg.count()
	e.flush()
	got := e.tg.texts()
	if len(got) != before+1 || !strings.Contains(got[len(got)-1], "早报") {
		e.t.Fatalf("%s 应推一条早报：%q", day.Format("01-02"), got[before:])
	}
	return textfmt.HTMLToPlain(got[len(got)-1])
}

// 周一早报多一节「本周做的减法」：过去 7 天合进默认分支的净删明显、删文件、少依赖的提交，带提交号和标题；
// 7 天前的、加东西的、「待办：」记账提交不算；跳过 exclude_projects；不是周一没有这一节。
func TestMorningWeeklySubtract(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	monday := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 10, 0, 0, 0, time.Local) }

	dir := subtractRepo(t, e, "demo", day(1))
	e.write(filepath.Join(dir, "big.txt"), nLines("line", 300))
	e.write(filepath.Join(dir, "docs", "BACKLOG.md"), "# 待办\n\n## 已完成\n\n"+nLines("- [x] 旧条目", 80))
	e.write(filepath.Join(dir, "go.mod"), "module demo\n\ngo 1.22\n\nrequire (\n\texample.com/keep v1.0.0\n\texample.com/gone v1.2.0\n\texample.com/ind v0.1.0 // indirect\n)\n")
	subtractCommit(t, e, dir, day(1), "加一堆东西")
	e.write(filepath.Join(dir, "big.txt"), nLines("line", 200))
	tooOld := subtractCommit(t, e, dir, day(2), "七天前删的 100 行")
	e.write(filepath.Join(dir, "big.txt"), nLines("line", 80))
	cut := subtractCommit(t, e, dir, day(10), "砍掉大段旧逻辑")
	e.write(filepath.Join(dir, "go.mod"), "module demo\n\ngo 1.22\n\nrequire (\n\texample.com/keep v1.0.0\n\texample.com/new v1.0.0\n)\n")
	dep := subtractCommit(t, e, dir, day(11), "换掉 gone 依赖")
	e.write(filepath.Join(dir, "feature.txt"), nLines("feat", 40))
	added := subtractCommit(t, e, dir, day(11), "加新功能")
	e.write(filepath.Join(dir, "docs", "BACKLOG.md"), "# 待办\n")
	bookkeeping := subtractCommit(t, e, dir, day(11), "待办：归档")
	// 引擎那样的合并：分支上两个提交各删 30 行，合并提交按和第一父的差算一条
	subtractGit(t, e, dir, time.Time{}, "checkout", "-q", "-b", "agent/x")
	e.write(filepath.Join(dir, "big.txt"), nLines("line", 50))
	side1 := subtractCommit(t, e, dir, day(11), "分支上删 30 行")
	e.write(filepath.Join(dir, "big.txt"), nLines("line", 20))
	subtractCommit(t, e, dir, day(11), "分支上再删 30 行")
	subtractGit(t, e, dir, time.Time{}, "checkout", "-q", "main")
	subtractGit(t, e, dir, day(11), "merge", "-q", "--no-ff", "-m", "合并 精简分支", "agent/x")
	merge := subtractGit(t, e, dir, time.Time{}, "rev-parse", "--short", "HEAD")

	// tools：删了 4 个小文件，只列前三条，第四条进「另有」
	tools := subtractRepo(t, e, "tools", day(1))
	for _, f := range []string{"a.sh", "b.sh", "c.sh", "d.sh"} {
		e.write(filepath.Join(tools, f), "echo "+f+"\n")
	}
	subtractCommit(t, e, tools, day(1), "加脚本")
	var rm []string
	for _, f := range []string{"a.sh", "b.sh", "c.sh", "d.sh"} {
		os.Remove(filepath.Join(tools, f))
		rm = append(rm, subtractCommit(t, e, tools, day(11), "删掉 "+f))
	}

	skip := subtractRepo(t, e, "skipme", day(1))
	e.write(filepath.Join(skip, "big.txt"), nLines("line", 200))
	subtractCommit(t, e, skip, day(1), "加")
	os.Remove(filepath.Join(skip, "big.txt"))
	skipped := subtractCommit(t, e, skip, day(11), "被排除项目里的减法")
	e.a.Cfg.ExcludeProjects = []string{"skipme"}

	sunday := e.morningOn(time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local))
	if strings.Contains(sunday, "本周做的减法") {
		t.Fatalf("不是周一不该有这一节：\n%s", sunday)
	}
	m := e.morningOn(monday)
	i := strings.Index(m, "✂️ 本周做的减法")
	if i < 0 {
		t.Fatalf("周一早报缺「本周做的减法」：\n%s", m)
	}
	sec := m[i:]
	if j := strings.Index(sec, "📊"); j > 0 {
		sec = sec[:j]
	}
	// 按净删多少排，每个项目最多三条：合并提交按和第一父的差算一条（净删 60）
	for _, want := range []string{
		"demo　" + cut + " 砍掉大段旧逻辑（净删 120 行）",
		"demo　" + merge + " 合并 精简分支（净删 60 行）",
		"demo　" + dep + " 换掉 gone 依赖（净删 1 行；少依赖 example.com/gone）",
		"tools　" + rm[3] + " 删掉 d.sh（净删 1 行；删 d.sh）",
		"tools　" + rm[2] + " 删掉 c.sh（净删 1 行；删 c.sh）",
		"tools　" + rm[1] + " 删掉 b.sh（净删 1 行；删 b.sh）",
		"另有 1 条（1 个项目）",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("缺 %q：\n%s", want, sec)
		}
	}
	for _, not := range []string{tooOld, added, bookkeeping, side1, skipped, rm[0], "skipme", "example.com/ind", notify.NoSubtract} {
		if strings.Contains(sec, not) {
			t.Errorf("不该有 %q：\n%s", not, sec)
		}
	}
}

// 7 天里只加东西（或没项目）：这一节只有一句「本周没有做减法」。
func TestMorningWeeklySubtractNone(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, true)
	dir := subtractRepo(t, e, "demo", time.Date(2026, 10, 9, 10, 0, 0, 0, time.Local))
	e.write(filepath.Join(dir, "a.txt"), nLines("a", 100))
	subtractCommit(t, e, dir, time.Date(2026, 10, 10, 10, 0, 0, 0, time.Local), "加功能")
	m := e.morningOn(time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local))
	i := strings.Index(m, "✂️ 本周做的减法")
	if i < 0 {
		t.Fatalf("周一早报缺「本周做的减法」：\n%s", m)
	}
	sec := strings.TrimSpace(m[i+len("✂️ 本周做的减法"):])
	if !strings.HasPrefix(sec, notify.NoSubtract+"\n") || strings.Contains(m, "demo　") {
		t.Fatalf("没有减法只写一句：\n%s", m)
	}
}
