package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 巡逻看进展：tick 解析运行中任务的会话记录，用代码判断是不是在空转，不调模型。「子代理失控」命中时自动停（progress_autostop，见 budget.go），其余只报不杀。
// 阈值来自对历史任务的回测，依据有限，先写成常量：
//   - 子代理失控：日志数和再派子代理数都明显超过正常任务才算；
//   - 无改动：任务类最长 59 分钟（只算主会话），放宽到 90 分钟；
//   - 原地重复：正常任务不会命中，夜班的循环等待会，所以夜班不看；
//   - 主会话空等：20 分钟会误报正常任务，放宽到 45 分钟。
//
// 工具调用来自后端的 ToolLog（有的是 CLI 的内部格式，官方不保证）：读不了、认不出就当未知，记一条日志，不报信号。
const (
	ProgressMinRun      = 30 * time.Minute
	progressUnknownNote = "unknown" // ProgressNoted 里记「已记过未知」，同一个任务只记一条日志
)

// jobProgress 读任务的工具调用并判断信号。后端不提供、读不了、认不出返回错误，调用方当未知。
func (a *Service) jobProgress(j *state.Job, now time.Time) ([]backend.ProgressHit, error) {
	log, ok := a.BackendFor(j.Backend).(backend.ToolLog)
	if !ok {
		return nil, errors.New("这个后端不提供工具调用记录")
	}
	main, subs, err := log.ToolEvents(a.Ledger.JobRef(j), true)
	if err != nil {
		return nil, err
	}
	return backend.EvalProgress(main, subs, j.StartedAt, now), nil
}

func Noted(j *state.Job, signal string) bool {
	for _, s := range j.ProgressNoted {
		if s == signal {
			return true
		}
	}
	return false
}

// CheckProgress：运行满 progressMinRun 的待做任务（夜班、复盘、巡逻分析、对话这些本来就可能长时间循环或等待，不看）
// 命中信号就写日志、当天汇总和推送队列；同一信号不重报。除了「子代理失控」（见 autoStopRunaway），都只报不杀。
func (a *Service) CheckProgress(st *state.State) {
	now := a.Now()
	for _, j := range st.Tasks() {
		if now.Sub(j.StartedAt) < ProgressMinRun {
			continue
		}
		hits, err := a.jobProgress(j, now)
		if err != nil {
			if !Noted(j, progressUnknownNote) {
				j.ProgressNoted = append(j.ProgressNoted, progressUnknownNote)
				a.Ledger.WriteMeta(j)
				a.Logf("看进展：任务 %s 的会话记录读不了，当作未知，不报信号：%v", j.ID, err)
			}
			continue
		}
		changed := false
		for _, h := range hits {
			// 进程已经结束的不用杀，这次照常报，下次 tick 按结果收尾
			if h.Signal == backend.SigRunaway && a.JobLive(j) == proc.ProcOurs {
				if !Noted(j, h.Signal) {
					j.ProgressNoted = append(j.ProgressNoted, h.Signal)
				}
				a.autoStopRunaway(st, j, h)
				changed = false // finish 写过 meta.json
				break
			}
			if Noted(j, h.Signal) {
				continue
			}
			j.ProgressNoted = append(j.ProgressNoted, h.Signal)
			changed = true
			msg := fmt.Sprintf("看进展：任务 %s（%s「%s」）%s：%s；不自动杀，运行满 %s 才终止，要停就 agentq stop %s",
				j.ID, j.Project, textfmt.TruncRunes(j.Title, 40), backend.ProgressNames[h.Signal], h.Evidence, textfmt.FormatDuration(a.JobTimeout(j)), j.ID)
			a.Logf("%s", msg)
			a.Ledger.AppendNightlyLine(now, msg)
			a.Notify.Notify(notify.Notice{Cat: "patrol", Key: "progress:" + j.ID + ":" + h.Signal, JobID: j.ID,
				Text:  fmt.Sprintf("%s「%s」%s", j.Project, textfmt.TruncRunes(j.Title, 30), backend.ProgressNames[h.Signal]),
				Lines: []string{h.Evidence, "要停就 agentq stop " + j.ID}})
		}
		if changed {
			a.Ledger.WriteMeta(j)
		}
	}
}

// ProgressLabel：status 里标出已经报过的信号，没有返回空。
func ProgressLabel(j *state.Job) string {
	var names []string
	for _, s := range j.ProgressNoted {
		if n := backend.ProgressNames[s]; n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "，看进展：" + strings.Join(names, "、") + "（不会自动杀；要停就 agentq stop " + j.ID + "）"
}
