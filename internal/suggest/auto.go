package suggest

import (
	"fmt"
	"slices"

	"agentq/internal/backlog"
	"agentq/internal/notify"
	"agentq/internal/textfmt"
)

// 建议没回就自动做：写进「## 建议」满 suggest_auto_after（默认 24 小时，0 表示一直等）还没回「做 N」「不做 N」的，
// tick 照「做 N」同一条路移进「待做」末尾，推一条通知。一次 tick 一个项目最多移 1 条（编号小的先），不一下子塞满待做。
// 不动的：不做清单里的；用户删掉的（索引里已作废）；exclude_projects 里的项目（不在 ActiveProjects 里）。

// AutoDoDue 由 tick 在接活之前调用，移进去的这次就能被挑中。出错只记日志。
func (a *Service) AutoDoDue() {
	after := a.Cfg.SuggestAutoAfter.Duration
	if after <= 0 {
		return
	}
	store, err := a.LiveSuggestions(true)
	if err != nil {
		a.Logf("建议：读不了建议索引，这次不自动做：%v", err)
		return
	}
	if len(store.Items) == 0 {
		return
	}
	projects, _ := a.Backlog.ActiveProjects() // 列表不全只是这次少看几个项目
	byName := map[string]backlog.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	items := slices.Clone(store.Items)
	slices.SortFunc(items, func(x, y SuggestEntry) int { return x.N - y.N })
	wait := textfmt.FormatAge(after)
	now := a.Now()
	picked := map[string]bool{}
	rejected := map[string]map[string]bool{}
	for _, e := range items {
		p, ok := byName[e.Project]
		if !ok || picked[e.Project] || now.Sub(e.CreatedAt) < after {
			continue
		}
		if rejected[p.Name] == nil {
			rejected[p.Name] = a.NotDoingKeys(p)
		}
		if rejected[p.Name][backlog.TitleKey(e.Title)] {
			continue
		}
		picked[e.Project] = true
		done, _, failed := a.moveToTodo([]int{e.N}, "自动做", "（建议 "+wait+"没回，按默认开工）", wait+"没回，按默认做了")
		if len(done) == 0 {
			if len(failed) > 0 {
				a.Logf("建议：建议 %d %s没回，但这次没移进待做，下次 tick 再试：%s", e.N, wait, failed[0])
			}
			continue
		}
		a.Notify.Notify(notify.Notice{Cat: "suggest", Key: fmt.Sprintf("suggest-auto:%d", e.N),
			Text:  fmt.Sprintf("建议 %d %s没回，已按默认开工，不要就回停 / 不做", e.N, wait),
			Lines: []string{textfmt.TruncRunes(e.Project+"　"+e.Title, 56)}})
	}
}
