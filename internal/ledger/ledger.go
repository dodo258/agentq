// Package ledger 记任务账本：meta.json 落盘、当天汇总、花费小结。
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/quality"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是记账的模块实例。
type Service struct {
	*env.Env
	Quality *quality.Service
}

// ReadResult 按任务记下的后端读结果。
func (a *Service) ReadResult(j *state.Job) backend.Result {
	res := a.BackendFor(j.Backend).Parse(a.JobRef(j), backend.Exit{})
	if schema, err := os.ReadFile(a.JobOutput(j.ID, backend.SchemaFile)); err == nil && len(res.Structured) == 0 { // CLI 不能强制结构化输出：从文字里取并校验，不合格当这次没结果
		if raw, ok := backend.ExtractJSON(res.Text, string(schema)); ok {
			res.Structured = raw
		} else {
			res.Text = ""
		}
	}
	return res
}

// JobRef：这个任务的落盘材料在哪。
func (a *Service) JobRef(j *state.Job) backend.JobRef {
	return backend.JobRef{ID: j.ID, Dir: a.JobOutput(j.ID, ""), SessionID: j.SessionID, StartedAt: j.StartedAt}
}

// ReadJobUsage 读任务的结果记下用量；后端没给就不记。
// 用量是整个会话累计的后端（续跑时），要减掉上一个有结果的同会话任务的累计数，才是这次进程花的。
func (a *Service) ReadJobUsage(st *state.State, j *state.Job) {
	res := a.ReadResult(j)
	j.Usage = res.Usage
	if j.Usage == nil || !res.UsageCumulative {
		return
	}
	seen := map[string]bool{j.ID: true} // 记录被改坏成环时不能一直转
	for p := j; p.ResumeOf != "" && !seen[p.ResumeOf]; {
		prev, err := st.Find(p.ResumeOf)
		if err != nil {
			return
		}
		seen[prev.ID] = true
		if u := a.ReadResult(prev).Usage; u != nil {
			if d, ok := j.Usage.Minus(*u); ok {
				j.Usage = &d
			}
			return
		}
		p = prev
	}
}

// 当天汇总末尾的小结（当天用量、一次满意率和返工率）夹在这两行之间，每有任务结束就重写；其余行插在它前面。
// 标记文字沿用最初的「当天花费」，改了会认不出已有汇总里的小结。
const (
	CostBegin = "<!-- agentq 当天花费：开始（自动生成，每有任务结束重写） -->"
	CostEnd   = "<!-- agentq 当天花费：结束 -->"
)

// CostFooter 按 state 里 day 那天结束、记了用量的任务写花费小结；一个都没有返回空。
func (a *Service) CostFooter(st *state.State, day time.Time) string {
	y, m, d := day.Date()
	var jobs []*state.Job
	var sum backend.JobUsage
	noUsage := 0 // 后端不给的：合计里要注明，不能当 0
	for _, j := range st.Jobs {
		if j.EndedAt == nil {
			continue
		}
		if jy, jm, jd := j.EndedAt.In(day.Location()).Date(); jy != y || jm != m || jd != d {
			continue
		}
		if j.Usage == nil {
			if !a.BackendFor(j.Backend).Caps().Usage {
				noUsage++
			}
			continue
		}
		jobs = append(jobs, j)
		sum.Input += j.Usage.Input
		sum.Output += j.Usage.Output
		sum.CacheRead += j.Usage.CacheRead
		sum.CacheWrite += j.Usage.CacheWrite
	}
	note := ""
	if noUsage > 0 {
		note += fmt.Sprintf("；另有 %d 个任务没有用量（后端不提供）", noUsage)
	}
	if len(jobs) == 0 {
		if noUsage == 0 {
			return ""
		}
		return fmt.Sprintf("%s\n\n## 当天用量\n\n- %d 个任务没有用量（后端不提供）\n%s\n", CostBegin, noUsage, CostEnd)
	}
	sort.SliceStable(jobs, func(i, k int) bool {
		return jobs[i].Usage.Total() > jobs[k].Usage.Total()
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n## 当天用量\n\n", CostBegin)
	fmt.Fprintf(&b, "- 合计 %d 个任务（没有用量记录的不算）：%s%s\n", len(jobs), backend.JobUsageDetail(sum), note)
	hit := "—"
	if den := sum.CacheRead + sum.CacheWrite + sum.Input; den > 0 {
		hit = fmt.Sprintf("%.1f%%", float64(sum.CacheRead)*100/float64(den))
	}
	fmt.Fprintf(&b, "- 缓存命中率 %s（缓存读 /（缓存读+缓存写+输入））\n", hit)
	b.WriteString("- 用量最多的 3 个任务：\n")
	for i, j := range jobs {
		if i == 3 {
			break
		}
		fmt.Fprintf(&b, "  %d. %s「%s」%s；%s\n", i+1, j.Project, textfmt.TruncRunes(j.Title, 40), backend.JobUsageShort(*j.Usage), j.ID)
	}
	b.WriteString(CostEnd + "\n")
	return b.String()
}

// splitCostFooter 把汇总拆成花费小结之外的内容（小结之后被追加的行接在后面）和小结本身。
func splitCostFooter(s string) (body, footer string) {
	// 取最后一个结束标记和它前面最近的开始标记：被改坏、多出来的开始标记留在正文里，不连带吞掉后面的行
	k := strings.LastIndex(s, CostEnd)
	i := strings.LastIndex(s[:max(k, 0)], CostBegin)
	if i < 0 || k < 0 {
		return s, ""
	}
	end := k + len(CostEnd)
	after := strings.TrimPrefix(s[end:], "\n")
	return strings.TrimRight(s[:i], "\n") + "\n" + after, s[i:end] + "\n"
}

// rewriteNightly 重写 path：body 之后追加 line（可以为空），再接 footer。footer 为 nil 表示沿用文件里原有的小结。
func rewriteNightly(path, header, line string, footer *string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	body, old := splitCostFooter(string(raw))
	if body = strings.TrimLeft(body, "\n"); !strings.HasPrefix(body, "# ") {
		body = header + body
	}
	if footer == nil {
		footer = &old
	}
	out := body + line
	if *footer != "" {
		out += "\n" + *footer
	}
	return fsx.WriteFileAtomic(path, []byte(out))
}

// UpdateCostFooter 重写 t 那天汇总末尾的小结：当天用量（那天还没有记了用量的任务就没有这一节）和最近 7 天的一次满意率、返工率。
func (a *Service) UpdateCostFooter(st *state.State, t time.Time) {
	f := a.CostFooter(st, t)
	if q := a.Quality.QualitySection(t); q != "" {
		if f == "" {
			f = CostBegin + "\n\n" + q + CostEnd + "\n"
		} else {
			f = strings.Replace(f, CostEnd, "\n"+q+CostEnd, 1)
		}
	}
	if f == "" {
		return
	}
	path := a.NightlyPath(t)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = rewriteNightly(path, nightlyHeader(t), "", &f)
	}
	if err != nil {
		a.Logf("警告：写当天汇总 %s 的小结失败：%v", path, err)
	}
}

func (a *Service) SenseDir() string { return filepath.Join(a.DataDir(), "sense") }

// 判断任务的提示词、批次和输出都带外部文字，放 ~/.agentq/sense/runs/<任务id>/（目录 700、文件 600）。
// jobs/<id>/ 是 Router 能读、send_file 能发的地方，那边只留占位和 meta（摘要里没有原文）。
func (a *Service) SenseRunDir(id string) string { return filepath.Join(a.SenseDir(), "runs", id) }

// JobOutput：任务输出文件的路径。判断任务在 sense/runs/ 下，其余在任务目录。
func (a *Service) JobOutput(id, name string) string {
	if fi, err := os.Stat(a.SenseRunDir(id)); err == nil && fi.IsDir() {
		return filepath.Join(a.SenseRunDir(id), name)
	}
	return filepath.Join(a.JobDir(id), name)
}

// LoadJobMetas 读所有 meta.json，坏的跳过并计数。
func (a *Service) LoadJobMetas() ([]*state.Job, int, error) {
	metas, err := filepath.Glob(filepath.Join(a.DataDir(), "jobs", "*", "meta.json"))
	if err != nil {
		return nil, 0, fmt.Errorf("列任务目录：%w", err)
	}
	var jobs []*state.Job
	bad := 0
	for _, m := range metas {
		b, err := os.ReadFile(m)
		var j state.Job
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID != filepath.Base(filepath.Dir(m)) || j.Status == "" {
			bad++
			continue
		}
		jobs = append(jobs, &j)
	}
	return jobs, bad, nil
}

func (a *Service) WriteMeta(j *state.Job) {
	b, err := json.MarshalIndent(j, "", "  ")
	if err == nil {
		err = fsx.WriteFileAtomic(filepath.Join(a.JobDir(j.ID), "meta.json"), append(b, '\n'))
	}
	if err != nil {
		a.Logf("警告：写 %s 的 meta.json 失败：%v", j.ID, err)
	}
}

// AppendNightly 给 ~/.claude/nightly/<日期>-agentq.md 追加这个任务的一行，每晚复盘会读。
func (a *Service) AppendNightly(j *state.Job) {
	t := a.Now()
	if j.EndedAt != nil {
		t = *j.EndedAt
	}
	cost := ""
	if j.Usage != nil {
		cost = "；用量 " + backend.JobUsageDetail(*j.Usage)
	} else if j.EndedAt != nil && !a.BackendFor(j.Backend).Caps().Usage {
		cost = "；用量未知"
	}
	a.AppendNightlyLine(t, fmt.Sprintf("%s「%s」%s：%s%s；日志 %s",
		j.Project, textfmt.TruncRunes(j.Title, 40), state.JobLabel(j), textfmt.OneLine(j.Summary, 300), cost, a.JobDir(j.ID)))
}

// AppendNightlyLine 给 t 那天的汇总追加一行「- 时:分 text」，文件是新的就先写标题。
func (a *Service) AppendNightlyLine(t time.Time, text string) {
	path := a.NightlyPath(t)
	line := fmt.Sprintf("- %s %s\n", t.Format("15:04"), text)
	// 末尾有花费小结时，新行插在小结前面（整个文件重写）
	if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), CostBegin) {
		if err := rewriteNightly(path, nightlyHeader(t), line, nil); err != nil {
			a.Logf("警告：写当天汇总 %s 失败：%v", path, err)
		}
		return
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	if err != nil {
		a.Logf("警告：写当天汇总 %s 失败：%v", path, err)
		return
	}
	defer f.Close()
	var b strings.Builder
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		b.WriteString(nightlyHeader(t))
	}
	b.WriteString(line)
	if _, err := f.WriteString(b.String()); err != nil {
		a.Logf("警告：写当天汇总 %s 失败：%v", path, err)
	}
}

func nightlyHeader(t time.Time) string {
	return fmt.Sprintf("# agentq 任务汇总 %s\n\n", t.Format("2006-01-02"))
}
