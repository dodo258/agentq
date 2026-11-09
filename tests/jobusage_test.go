package tests

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/env"
	"agentq/internal/ledger"
	"agentq/internal/state"
)

func TestCostFooter(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	day := time.Date(2026, 10, 11, 12, 0, 0, 0, loc)
	at := func(h int) *time.Time { t := time.Date(2026, 10, 11, h, 0, 0, 0, loc); return &t }
	st := &State{Jobs: []*Job{
		{ID: "j1", Project: "a", Title: "最少", EndedAt: at(1), Usage: &backend.JobUsage{Input: 100, CacheRead: 800, CacheWrite: 100, CostUSD: 9}},
		{ID: "j2", Project: "b", Title: "最多", EndedAt: at(2), Usage: &backend.JobUsage{Input: 0, CacheRead: 3000, CostUSD: 1}},
		{ID: "j3", Project: "c", Title: "次多", EndedAt: at(3), Usage: &backend.JobUsage{CacheRead: 100, CacheWrite: 1900, Output: 5, CostUSD: 5}},
		{ID: "j4", Project: "d", Title: "第三", EndedAt: at(4), Usage: &backend.JobUsage{CacheRead: 1500, CostUSD: 2}},
		{ID: "j5", Project: "e", Title: "缺用量", EndedAt: at(5)},
		{ID: "j6", Project: "f", Title: "昨天的", EndedAt: ptime(day.Add(-24 * time.Hour)), Usage: &backend.JobUsage{CacheRead: 1, CostUSD: 99}},
		{ID: "j7", Project: "g", Title: "还在跑", Status: state.StatusRunning},
	}}
	a := wireApp(&App{Env: &env.Env{}})
	f := a.Ledger.CostFooter(st, day)
	// 按 token 排，不按金额（j1 金额最高但 token 最少）；命中率 = (800+3000+100+1500) / (那四项 + 缓存写 100+1900 + 输入 100) = 5400/7500
	for _, want := range []string{ledger.CostBegin, "## 当天用量", "合计 4 个任务", "缓存命中率 72.0%",
		"1. b「最多」token 3.0K；j2", "2. c「次多」token 2.0K；j3", "3. d「第三」token 1.5K；j4", ledger.CostEnd} {
		if !strings.Contains(f, want) {
			t.Errorf("花费小结缺 %q：\n%s", want, f)
		}
	}
	for _, bad := range []string{"最少", "昨天的", "缺用量", "4. ", "$"} {
		if strings.Contains(f, bad) {
			t.Errorf("花费小结不应有 %q：\n%s", bad, f)
		}
	}
	if f := a.Ledger.CostFooter(&State{Jobs: []*Job{{ID: "x", EndedAt: at(1)}}}, day); f != "" {
		t.Errorf("当天没有记了用量的任务时不写小结：%q", f)
	}
}

// 小结一直在汇总末尾：新行插在它前面，保护脚本用 >> 追加在它后面的行也挪到它前面，重写小结不丢行。
func TestNightlyCostFooterStaysLast(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 11, 9, 30, 0, 0, time.Local)
	a := wireApp(&App{Env: &env.Env{Home: home, Now: func() time.Time { return now }, Out: &bytes.Buffer{}}})
	end := now
	st := &State{Jobs: []*Job{{ID: "j1", Project: "a", Title: "甲", EndedAt: &end, Usage: &backend.JobUsage{CacheRead: 9, Input: 1, CostUSD: 1}}}}
	path := a.NightlyPath(now)

	a.Ledger.AppendNightlyLine(now, "第一行")
	a.Ledger.UpdateCostFooter(st, now)
	a.Ledger.AppendNightlyLine(now, "第二行")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("- 09:31 已自动回滚：保护脚本追加的\n")
	f.Close()
	st.Jobs = append(st.Jobs, &Job{ID: "j2", Project: "b", Title: "乙", EndedAt: &end, Usage: &backend.JobUsage{CacheRead: 0, Input: 20, CostUSD: 3}})
	a.Ledger.UpdateCostFooter(st, now)
	a.Ledger.AppendNightlyLine(now, "第三行")

	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Count(s, ledger.CostBegin) != 1 || strings.Count(s, "# agentq 任务汇总") != 1 {
		t.Fatalf("应只有一个标题、一个小结：\n%s", s)
	}
	want := []string{"# agentq 任务汇总 2026-10-11", "- 09:30 第一行", "- 09:30 第二行", "已自动回滚：保护脚本追加的", "- 09:30 第三行", ledger.CostBegin, "合计 2 个任务", "1. b「乙」", "2. a「甲」", ledger.CostEnd}
	pos := 0
	for _, w := range want {
		i := strings.Index(s[pos:], w)
		if i < 0 {
			t.Fatalf("缺 %q 或顺序不对：\n%s", w, s)
		}
		pos += i + len(w)
	}
	if strings.TrimSpace(s[pos:]) != "" {
		t.Errorf("小结后面不应还有内容：\n%s", s)
	}
	if strings.Contains(s, "\n\n\n") {
		t.Errorf("不应有连续空行：\n%s", s)
	}
}

// 续跑（claude -r）的 result.json 里 modelUsage、total_cost_usd 是整个会话累计的：减掉上一个有结果的同会话任务。
func TestReadJobUsageResume(t *testing.T) {
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir()}})
	write := func(id, raw string) {
		d := a.JobDir(id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d+"/result.json", []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := func(in, out, cr, cw int, cost float64) string {
		return fmt.Sprintf(`{"total_cost_usd":%g,"modelUsage":{"m":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d}}}`, cost, in, out, cr, cw)
	}
	// 数字照实测：第一次 2/418/10873/24510 $0.00521993，-r 续跑后 4/422/46256/25149 $0.00570376
	write("orig", res(2, 418, 10873, 24510, 0.00521993))
	write("r1", `{"type":"result","is_error":true,"result":"崩了"}`) // 续跑一次没留下用量
	write("r2", res(4, 422, 46256, 25149, 0.00570376))
	write("odd", res(1, 1, 1, 1, 0.001)) // 比上一次还小：不是累计的，照原样记
	st := &State{Jobs: []*Job{{ID: "orig"}, {ID: "r1", ResumeOf: "orig"}, {ID: "r2", ResumeOf: "r1"}, {ID: "odd", ResumeOf: "orig"}, {ID: "gone", ResumeOf: "nosuch"}}}
	write("gone", res(5, 5, 5, 5, 0.5))

	j := st.Jobs[2]
	a.Ledger.ReadJobUsage(st, j)
	if j.Usage == nil || j.Usage.Input != 2 || j.Usage.Output != 4 || j.Usage.CacheRead != 35383 || j.Usage.CacheWrite != 639 ||
		fmt.Sprintf("%.8f", j.Usage.CostUSD) != "0.00048383" {
		t.Errorf("续跑应只记这次进程的：%+v", j.Usage)
	}
	for id, want := range map[string]backend.JobUsage{"orig": {Input: 2, Output: 418, CacheRead: 10873, CacheWrite: 24510, CostUSD: 0.00521993}, "odd": {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, CostUSD: 0.001}, "gone": {Input: 5, Output: 5, CacheRead: 5, CacheWrite: 5, CostUSD: 0.5}} {
		j, _ := st.Find(id)
		a.Ledger.ReadJobUsage(st, j)
		if j.Usage == nil || *j.Usage != want {
			t.Errorf("%s：得到 %+v，应为 %+v", id, j.Usage, want)
		}
	}
	a.Ledger.ReadJobUsage(st, st.Jobs[1])
	if j := st.Jobs[1]; j.Usage != nil {
		t.Errorf("没有用量的结果不记：%+v", j.Usage)
	}
}
