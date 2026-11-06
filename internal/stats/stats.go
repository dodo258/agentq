// Package stats 是 agentq stats：只读统计最近 N 天任务的次数、成败、token 和折算费用。
package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/ledger"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是任务统计的模块实例。
type Service struct {
	*env.Env
	Ledger *ledger.Service
}

// agentq stats [天数]：只读，按任务种类、项目、模型统计最近 N 天的次数、成败、token 和折算费用（按 API 标价折算，实际走订阅额度）。
// 数据来自 jobs/*/meta.json 和 result.json（主动感知的在 sense/runs/，见 jobOutput）。同一会话续跑时 result.json 是累计的，用 readJobUsage 减掉上一次，不重复计。
// 模型取启动参数里的模型（一个任务算一个模型，子代理用的便宜模型也算在它头上）。
const (
	statsDefaultDays = 7
	statsMaxDays     = 3650
)

// statsRow 是一组的合计。
type statsRow struct {
	Count, OK, Bad, Other, Running int
	NoUsage                        int // 没有用量记录的（没算进 token 和费用）
	Usage                          backend.JobUsage
}

func (r *statsRow) add(j *state.Job) {
	r.Count++
	switch {
	case j.Status == state.StatusRunning:
		r.Running++
	case j.Status == state.StatusDone && !state.VerifyFailed(j) && !state.GateUnmerged(j):
		r.OK++
	case j.Status == state.StatusDone || j.Status == state.StatusFailed || j.Status == state.StatusTimeout:
		r.Bad++ // 合并后核验失败、门没过不算成
	default:
		r.Other++ // 中断、用户停掉
	}
	if j.Usage == nil {
		r.NoUsage++
		return
	}
	r.Usage.Input += j.Usage.Input
	r.Usage.Output += j.Usage.Output
	r.Usage.CacheRead += j.Usage.CacheRead
	r.Usage.CacheWrite += j.Usage.CacheWrite
	r.Usage.CostUSD += j.Usage.CostUSD
}

// statsResult 是算好的结果，和打印分开，方便测。
type statsResult struct {
	Days                  int
	Total                 statsRow
	ByKind, ByProj, ByMod map[string]*statsRow
	BadMeta, BadResult    int
	Gates                 gateStats
}

// gateStats 是按门的统计。升级前的记录没有次数（NoTally），退回、放行这些还能数。
type gateStats struct {
	Tally                                                             map[string]*state.GateTally
	Jobs, Merged, Returned, Stuck, Passes, Abandoned, NoTally, Reruns int
}

func (g *gateStats) add(j *state.Job) {
	r := j.Gate
	if r == nil {
		return
	}
	g.Jobs++
	switch r.State {
	case state.GateMerged:
		g.Merged++
	case state.GateReturned:
		g.Returned++
	case state.GateStuck:
		g.Stuck++
	case state.GateAbandoned:
		g.Abandoned++
	}
	g.Passes += r.Passes
	g.Reruns += r.Reruns
	if len(r.Tally) == 0 {
		g.NoTally++
		return
	}
	if g.Tally == nil {
		g.Tally = map[string]*state.GateTally{}
	}
	for name, t := range r.Tally {
		if t == nil {
			continue
		}
		x := g.Tally[name]
		if x == nil {
			x = &state.GateTally{}
			g.Tally[name] = x
		}
		x.Pass += t.Pass
		x.Fail += t.Fail
		x.Unknown += t.Unknown
		x.WouldBlock += t.WouldBlock
		x.Deferred += t.Deferred
		x.Usage = backend.AddUsage(x.Usage, t.Usage)
	}
}

func (g *gateStats) print(w *strings.Builder) {
	if g.Jobs == 0 {
		return
	}
	fmt.Fprintf(w, "\n按门（%d 次过门记录，合并 %d，退回 %d，卡住 %d，放弃 %d，用户放行 %d 次，再审 %d 次）：\n", g.Jobs, g.Merged, g.Returned, g.Stuck, g.Abandoned, g.Passes, g.Reruns)
	names := make([]string, 0, len(g.Tally))
	for n := range g.Tally {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := g.Tally[n]
		s := fmt.Sprintf("  %s　通过 %d　失败 %d　判不了 %d", n, t.Pass, t.Fail, t.Unknown)
		if t.WouldBlock > 0 {
			s += fmt.Sprintf("　warn 档如果阻塞会挡下 %d", t.WouldBlock)
		}
		if t.Deferred > 0 {
			s += fmt.Sprintf("　排回重跑 %d", t.Deferred)
		}
		if t.Usage != nil {
			s += "　token " + textfmt.FmtTokens(t.Usage.Total())
		}
		fmt.Fprintln(w, s)
	}
	if g.NoTally > 0 {
		fmt.Fprintf(w, "  %d 条记录是升级前跑的，没有按门的次数\n", g.NoTally)
	}
}

var statsKindNames = map[string]string{
	"":                 "待做任务",
	config.KindNight:   "夜班",
	config.KindReview:  "复盘",
	state.KindPatrol:   "巡逻分析",
	state.KindChat:     "对话",
	state.KindFollowup: "追问",
	state.KindRouter:   "Router",
	state.KindQuick:    "小任务",
	state.KindInbox:    "任务箱",
	state.KindNew:      "新项目",
	state.KindSuggest:  "闲时找活",
	state.KindSense:    "主动感知",
	state.KindRelease:  "发版准备",
	state.KindDiscuss:  "商量大方向",
}

func statsKindName(kind string) string {
	if n, ok := statsKindNames[kind]; ok {
		return n
	}
	return kind
}

// jobModel 让任务的后端从启动参数里认模型。有启动参数但没写模型是跟随 CLI；meta.json 里没有参数就是未知。
func (a *Service) jobModel(j *state.Job) string {
	if m := a.BackendFor(j.Backend).RecordedModel(j.Args); m != "" {
		return m
	}
	if len(j.Args) > 0 {
		return config.ModelLabel("")
	}
	return "未知"
}

// ComputeStats：时间窗按结束时间算（还在跑的按启动时间）。上一次在窗外的续跑也要减掉它的累计数，所以用全部记录去算用量。
func (a *Service) ComputeStats(days int) (*statsResult, error) {
	jobs, badMeta, err := a.Ledger.LoadJobMetas()
	if err != nil {
		return nil, err
	}
	res := &statsResult{Days: days, BadMeta: badMeta, ByKind: map[string]*statsRow{}, ByProj: map[string]*statsRow{}, ByMod: map[string]*statsRow{}}
	all := &state.State{Jobs: jobs}
	since := a.Now().Add(-time.Duration(days) * 24 * time.Hour)
	pick := func(m map[string]*statsRow, k string) *statsRow {
		if m[k] == nil {
			m[k] = &statsRow{}
		}
		return m[k]
	}
	for _, j := range jobs {
		at := j.StartedAt
		if j.EndedAt != nil {
			at = *j.EndedAt
		}
		if at.Before(since) {
			continue
		}
		a.loadUsage(all, j, res)
		res.Gates.add(j)
		for _, r := range []*statsRow{&res.Total, pick(res.ByKind, statsKindName(j.Kind)), pick(res.ByProj, j.Project), pick(res.ByMod, a.jobModel(j))} {
			r.add(j)
		}
	}
	return res, nil
}

// statsResultMax：结果文件超过这么大就不解析，用收尾时记下的用量。正常的结果只有几 KB；几十 MB 的要从头解析好几遍，-race 下一条 stats 能走一分多钟。
const statsResultMax = 8 << 20

// loadUsage 把任务的用量填进 j.Usage。结果文件没有用量（读不了、半截、太大）时用收尾时记下的；两边都没有就算没有用量记录。
func (a *Service) loadUsage(all *state.State, j *state.Job, res *statsResult) {
	path := a.Ledger.JobOutput(j.ID, "result.json")
	if fi, err := os.Stat(path); err == nil && fi.Size() > statsResultMax {
		res.BadResult++
		return
	}
	saved := j.Usage
	raw, rerr := os.ReadFile(path)
	switch {
	case rerr != nil:
		if !errors.Is(rerr, fs.ErrNotExist) {
			res.BadResult++
		}
	case j.Status != state.StatusRunning && len(strings.TrimSpace(string(raw))) > 0 && !json.Valid(raw):
		res.BadResult++
	}
	a.Ledger.ReadJobUsage(all, j)
	switch {
	case j.Usage == nil:
		j.Usage = saved
	case j.Gate != nil: // 门调模型花的在结果文件里没有，加回去
		j.Usage = backend.AddUsage(j.Usage, j.Gate.Usage)
	}
}

func (r *statsRow) line(total float64) string {
	s := fmt.Sprintf("%d 次　成 %d　败 %d", r.Count, r.OK, r.Bad)
	if r.Other > 0 {
		s += fmt.Sprintf("　中断 %d", r.Other)
	}
	if r.Running > 0 {
		s += fmt.Sprintf("　在跑 %d", r.Running)
	}
	s += fmt.Sprintf("　token %s　约 $%.2f", textfmt.FmtTokens(r.Usage.Total()), r.Usage.CostUSD)
	if total > 0 {
		s += fmt.Sprintf("　%.0f%%", r.Usage.CostUSD*100/total)
	}
	return s
}

func (res *statsResult) print(w *strings.Builder) {
	fmt.Fprintf(w, "最近 %d 天：%s\n", res.Days, res.Total.line(0))
	total := res.Total.Usage.CostUSD
	for _, sec := range []struct {
		head string
		rows map[string]*statsRow
	}{{"按种类", res.ByKind}, {"按项目", res.ByProj}, {"按模型", res.ByMod}} {
		if len(sec.rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s：\n", sec.head)
		names := make([]string, 0, len(sec.rows))
		for n := range sec.rows {
			names = append(names, n)
		}
		sort.Slice(names, func(i, k int) bool {
			x, y := sec.rows[names[i]], sec.rows[names[k]]
			if x.Usage.CostUSD != y.Usage.CostUSD {
				return x.Usage.CostUSD > y.Usage.CostUSD
			}
			if x.Count != y.Count {
				return x.Count > y.Count
			}
			return names[i] < names[k]
		})
		for _, n := range names {
			fmt.Fprintf(w, "  %s　%s\n", n, sec.rows[n].line(total))
		}
	}
	res.Gates.print(w)
	var notes []string
	if n := res.Total.NoUsage; n > 0 {
		notes = append(notes, fmt.Sprintf("%d 个任务没有用量记录，没算进 token 和费用", n))
	}
	if res.BadMeta > 0 {
		notes = append(notes, fmt.Sprintf("跳过 %d 个读不了的 meta.json", res.BadMeta))
	}
	if res.BadResult > 0 {
		notes = append(notes, fmt.Sprintf("%d 个 result.json 读不了，用量按收尾时记下的算", res.BadResult))
	}
	notes = append(notes, "费用按 API 标价折算，实际走订阅额度；最后一列是费用占比")
	fmt.Fprintf(w, "\n%s\n", strings.Join(notes, "；"))
}

// CmdStats 是 agentq stats [天数]。只读：不拿锁、不写文件。
func (a *Service) CmdStats(args []string) error {
	days := statsDefaultDays
	if len(args) == 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 || n > statsMaxDays {
			return fmt.Errorf("天数要是 1 到 %d 的整数：%q", statsMaxDays, args[0])
		}
		days = n
	}
	res, err := a.ComputeStats(days)
	if err != nil {
		return err
	}
	var b strings.Builder
	res.print(&b)
	fmt.Fprint(a.Out, b.String())
	return nil
}
