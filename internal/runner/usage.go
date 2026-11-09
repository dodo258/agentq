package runner

import (
	"context"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/notify"
	"agentq/internal/state"
)

// ReadUsage 读一次默认后端的额度（不看缓存）。
func (a *Service) ReadUsage(now time.Time) *backend.UsageInfo {
	return a.ReadUsageOn(a.Cfg.Backend, now)
}

// ReadUsageOn 读一次这个后端的额度（不看缓存）。测试可以换掉 UsageText。
func (a *Service) ReadUsageOn(name string, now time.Time) *backend.UsageInfo {
	u := &backend.UsageInfo{CheckedAt: now}
	q, ok := a.BackendFor(name).(backend.QuotaReader)
	if !ok {
		u.Err = "这个后端没有额度查询"
		return u
	}
	read := a.UsageText
	if read == nil {
		read = func() (string, error) { return q.QuotaText(context.Background()) }
	}
	text, err := read()
	if err == nil {
		u.Windows, err = q.ParseQuota(text, now)
	}
	if err != nil {
		u.Err = err.Error()
	}
	return u
}

// Usage 返回默认后端的当前额度，见 UsageOn。
func (a *Service) Usage(st *state.State) *backend.UsageInfo { return a.UsageOn(st, a.Cfg.Backend) }

// UsageOn 返回这个后端的当前额度：缓存过期才重读并存进 st（调用方负责保存 st）。每天第一次读到、或档位变了，
// 记日志并在当天汇总写一行。name 空是 Claude（和 Job.Backend 一样）。
func (a *Service) UsageOn(st *state.State, name string) *backend.UsageInfo {
	now := a.Now()
	old := st.UsageOf(name, a.Cfg.Backend)
	if !old.Stale(now) {
		return old
	}
	u := a.ReadUsageOn(name, now)
	prevLevel, prevDay := backend.UsageOK, ""
	if old != nil {
		prevLevel, prevDay = old.NotedLevel, old.NotedDay
	}
	u.NotedLevel, u.NotedDay = prevLevel, prevDay
	st.SetUsageOf(name, a.Cfg.Backend, u)
	pol := a.Cfg.ForBackend(name).UsagePolicy()
	lvl, until, why := backend.UsageLevel(u, now, pol)
	day := now.Format("2006-01-02")
	if lvl == prevLevel && day == prevDay {
		return u
	}
	u.NotedLevel, u.NotedDay = lvl, day
	head, key := "额度：", "usage:"+day
	if be := a.BackendFor(name).Name(); be != a.DefaultBackend().Name() {
		head, key = "额度（"+be+"）：", "usage:"+be+":"+day
	}
	msg := head + backend.UsageDescribe(u, now, pol)
	switch {
	case lvl >= backend.UsageStop:
		msg += "；" + backend.UsageGateNote(lvl, until, why)
	case lvl == backend.UsageOK && prevLevel >= backend.UsageStop && prevDay != "":
		msg += "；已恢复，照常接活"
	}
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(now, msg)
	if lvl >= backend.UsageStop {
		a.Notify.Notify(notify.Notice{Cat: "alert", Key: key, Text: "订阅" + strings.TrimSuffix(head, "：") + "到了后台上限：" + why, Lines: []string{backend.UsageGateNote(lvl, until, why)}})
	}
	return u
}
