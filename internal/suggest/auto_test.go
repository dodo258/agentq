package suggest

import (
	"os"
	"strings"
	"testing"
	"time"

	bl "agentq/internal/backlog"
	"agentq/internal/testkit"
)

// fakeClock 让测试拨时间：写建议用 a.Now()，自动做也按 a.Now() 算满没满。
func fakeClock(a *Service) func(d time.Duration) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local)
	a.Env.Now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func todoTitles(t *testing.T, p bl.Project) []string {
	t.Helper()
	var out []string
	for _, it := range bl.ParseBacklog(suggestBacklogOf(t, p)) {
		out = append(out, it.Title)
	}
	return out
}

func (a *Service) autoNotices(t *testing.T) []string {
	t.Helper()
	ns, err := a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns.Pending {
		if strings.HasPrefix(n.Key, "suggest-auto:") {
			out = append(out, n.Text+"｜"+strings.Join(n.Lines, "｜"))
		}
	}
	return out
}

// 满 24 小时移进待做并推通知；不满不动；一个项目一次只移一条，别的项目各移一条；下次 tick 接着移下一条。
func TestAutoDoDue(t *testing.T) {
	a, root := newSuggestApp(t)
	tick := fakeClock(a)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	beta := suggestRepo(t, a, root, "beta", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "建议甲", ""), suggestItemOf("alpha", "建议乙", ""), suggestItemOf("beta", "建议丙", ""))
	na := suggestCommits(t, a, alpha)

	tick(23*time.Hour + 59*time.Minute)
	a.AutoDoDue()
	if got := todoTitles(t, alpha); len(got) != 0 || suggestCommits(t, a, alpha) != na || len(a.autoNotices(t)) != 0 {
		t.Fatalf("不满 24 小时不动：待做 %v，通知 %v", got, a.autoNotices(t))
	}

	tick(time.Minute)
	a.AutoDoDue()
	if got := todoTitles(t, alpha); len(got) != 1 || got[0] != "建议甲" {
		t.Fatalf("满 24 小时，alpha 一次只移编号小的一条：%v\n%s", got, suggestBacklogOf(t, alpha))
	}
	if got := todoTitles(t, beta); len(got) != 1 || got[0] != "建议丙" {
		t.Errorf("beta 也移一条：%v", got)
	}
	ab := suggestBacklogOf(t, alpha)
	if strings.Contains(ab, "建议甲 —— 要做的事；好处：好处；风险：风险（建议") || !strings.Contains(ab, "建议乙 —— 要做的事；好处：好处；风险：风险（建议 2）") {
		t.Errorf("移进待做时去掉编号标记，乙还留在建议里：\n%s", ab)
	}
	if suggestCommits(t, a, alpha) != na+1 || testkit.GitT(t, a.Home, alpha.Dir, "status", "--porcelain") != "" {
		t.Error("移一条提交一次，工作区干净")
	}
	if msg := testkit.GitT(t, a.Home, alpha.Dir, "log", "-1", "--format=%s"); !strings.Contains(msg, "建议 24 小时没回，按默认开工") {
		t.Errorf("提交说明：%s", msg)
	}
	ns := a.autoNotices(t)
	if len(ns) != 2 || !strings.Contains(strings.Join(ns, "\n"), "建议 1 24 小时没回，已按默认开工，不要就回停 / 不做｜alpha　建议甲") ||
		!strings.Contains(strings.Join(ns, "\n"), "建议 3 24 小时没回，已按默认开工") {
		t.Errorf("每移一条推一条：%v", ns)
	}
	if s := a.suggestMustStore(t); len(s.Items) != 1 || s.Items[0].N != 2 {
		t.Errorf("移走的从索引里去掉：%+v", s.Items)
	}

	a.AutoDoDue()
	if got := todoTitles(t, alpha); len(got) != 2 || got[1] != "建议乙" {
		t.Errorf("下次 tick 接着移下一条，放在待做末尾：%v", got)
	}
	a.AutoDoDue()
	if suggestCommits(t, a, alpha) != na+2 || len(a.autoNotices(t)) != 3 {
		t.Error("都移完了就不再动")
	}
}

// 回过「不做」的、不做清单里的、用户删掉的、排除项目里的不动；配置 0 一直等。
func TestAutoDoDueLeavesAlone(t *testing.T) {
	a, root := newSuggestApp(t)
	tick := fakeClock(a)
	alpha := suggestRepo(t, a, root, "alpha", suggestBacklogText)
	beta := suggestRepo(t, a, root, "beta", suggestBacklogText)
	gamma := suggestRepo(t, a, root, "gamma", suggestBacklogText)
	a.seed(t, suggestItemOf("alpha", "回过不做", ""), suggestItemOf("alpha", "清单里有", ""), suggestItemOf("alpha", "被删了", ""),
		suggestItemOf("beta", "排除项目的", ""), suggestItemOf("gamma", "一直等", ""))
	if msg := a.RejectSuggestions([]int{1}, "不做 1"); !strings.Contains(msg, "已写进「不做」清单") {
		t.Fatalf("准备：%s", msg)
	}
	nd := suggestMust(os.ReadFile(alpha.NotDoingPath()))
	os.WriteFile(alpha.NotDoingPath(), append(nd, []byte("\n## 清单里有\n")...), 0o644)
	ab := strings.Replace(suggestBacklogOf(t, alpha), "- [ ] 被删了 —— 要做的事；好处：好处；风险：风险（建议 3）\n", "", 1)
	os.WriteFile(alpha.BacklogPath(), []byte(ab), 0o644)
	testkit.GitT(t, a.Home, alpha.Dir, "commit", "-qam", "用户整理")
	a.Cfg.ExcludeProjects = []string{"beta"}

	a.Cfg.SuggestAutoAfter.Duration = 0
	tick(72 * time.Hour)
	a.AutoDoDue()
	if got := todoTitles(t, gamma); len(got) != 0 {
		t.Errorf("配置 0 不动：%v", got)
	}

	a.Cfg.SuggestAutoAfter.Duration = 24 * time.Hour
	na, nb := suggestCommits(t, a, alpha), suggestCommits(t, a, beta)
	for range 3 {
		a.AutoDoDue()
	}
	if got := todoTitles(t, alpha); len(got) != 0 || suggestCommits(t, a, alpha) != na {
		t.Errorf("回过不做、不做清单里、被删的都不动：%v\n%s", got, suggestBacklogOf(t, alpha))
	}
	if !strings.Contains(suggestBacklogOf(t, alpha), "清单里有 —— 要做的事；好处：好处；风险：风险（建议 2）") {
		t.Error("不做清单里的留在「建议」里")
	}
	if got := todoTitles(t, beta); len(got) != 0 || suggestCommits(t, a, beta) != nb || !strings.Contains(suggestBacklogOf(t, beta), "排除项目的 —— 要做的事") {
		t.Errorf("排除项目不动：%v", got)
	}
	if got := todoTitles(t, gamma); len(got) != 1 || got[0] != "一直等" {
		t.Errorf("配置改回 24 小时，gamma 照常移：%v", got)
	}
	if ns := a.autoNotices(t); len(ns) != 1 || !strings.Contains(ns[0], "建议 5 ") {
		t.Errorf("只推移了的那一条：%v", ns)
	}
}

// 写建议时的通知末尾说多久不回就自动做；配置 0 不说。
func TestSuggestNoticeAutoTail(t *testing.T) {
	a, _ := newSuggestApp(t)
	written := []string{"3　alpha　建议甲"}
	got := a.SuggestNotice(written)
	if last := got[len(got)-1]; last != "回复「做 3」移进待做，「不做 N」以后不再提；24 小时不回就自动做" {
		t.Errorf("默认 24 小时：%q", last)
	}
	a.Cfg.SuggestAutoAfter.Duration = 0
	got = a.SuggestNotice(written)
	if last := got[len(got)-1]; strings.Contains(last, "自动做") {
		t.Errorf("配置 0 不说自动做：%q", last)
	}
}
