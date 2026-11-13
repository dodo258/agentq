package runner

import (
	"fmt"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// TransientMaxCostUSD：这次进程花的钱不到它才算「几乎没花钱」（真实历史里没开工的是 $0，做到一半出错的是 $14）。
const TransientMaxCostUSD = 0.10

// limitFromStructuredOnly：商量大方向、产品经理调研的结果文字里可能有项目文件或网页的内容：「rate limit」这种字样不能算撞上限，否则别人写一句话就能停掉全部任务。只认结构化字段。
func limitFromStructuredOnly(kind string) bool {
	return state.SideKind(kind)
}

// jobHitLimit：这个任务是不是撞了用量上限。
func (a *Service) jobHitLimit(j *state.Job, r backend.Result) bool {
	if limitFromStructuredOnly(j.Kind) {
		return r.LimitStructured
	}
	return r.LimitStructured || r.LimitText
}

// noteTransient 判断这次失败是不是上游瞬时错误（backend.Result.Upstream），返回这次的状态和写进摘要开头的说明；不是返回 failed 和空。
// 做到一半（条目仍标进行中）、能用同一会话接着做的：记中断，和被 SIGTERM 打断一样续跑，见 resumeUpstream。
// 其余几乎没花钱的：设 j.Transient（不计连环失败），按短退避重试；花了钱（重试帮不上）的照普通失败。
// 重试、续跑用完的那次不设 Transient，照普通失败计数，上游长时间故障时熔断还能起作用。
func (a *Service) noteTransient(st *state.State, j *state.Job, res backend.Result) (string, string) {
	if !res.Upstream() {
		return state.StatusFailed, ""
	}
	head := fmt.Sprintf("上游瞬时错误（HTTP %d）", res.HTTPStatus)
	if j.Kind == "" && j.Fingerprint != "" {
		if status, note := a.resumeUpstream(st, j, head); note != "" {
			return status, note
		}
	}
	a.Ledger.ReadJobUsage(st, j)
	costKnown := j.Usage != nil && !j.Usage.CostUnknown
	if costKnown && j.Usage.CostUSD >= TransientMaxCostUSD || !costKnown && res.Turns > backend.LimitScanTurns { // 没有金额的后端退回只看回合数
		return state.StatusFailed, ""
	}
	if j.Kind != "" || j.Fingerprint == "" { // 夜班、复盘、对话没有条目可重试
		j.Transient = true
		return state.StatusFailed, head + "，不计连环失败"
	}
	used := state.TransientStreak(st, j.Project, j.Fingerprint)
	if used >= len(state.TransientBackoff) {
		return state.StatusFailed, fmt.Sprintf("%s，已重试 %d 次，回到 retry_after", head, used)
	}
	j.Transient = true
	items, err := backlog.Project{Dir: j.ProjectDir}.Items()
	if err != nil {
		return state.StatusFailed, head + "；核验 BACKLOG 失败：" + err.Error()
	}
	if _, ok := InProgressItem(items, j.Fingerprint); ok {
		return state.StatusFailed, head + "；BACKLOG 里这一条仍标进行中，不自动重试，等接手"
	}
	return state.StatusFailed, fmt.Sprintf("%s，%s 后重试（第 %d/%d 次）", head, textfmt.FormatDuration(state.TransientDelay(used+1, a.Cfg.RetryAfter.Duration)), used+1, len(state.TransientBackoff))
}

// resumeUpstream：做到一半撞上游瞬时错误（条目仍标进行中，任务的改动都在分支和 worktree 里），后端能续会话就记中断、用同一会话续跑，
// 和被 SIGTERM 打断走同一条路（ResumePending，下次 tick 续跑）。续跑次数和短退避重试共用 TransientBackoff 的上限，用完照普通失败。
// 不是做到一半、或接不回会话的返回空说明，由调用方照旧处理。
func (a *Service) resumeUpstream(st *state.State, j *state.Job, head string) (string, string) {
	if !a.BackendFor(j.Backend).Caps().Resume || j.SessionID == "" || j.SessionInRoot() {
		return "", ""
	}
	items, err := a.jobItems(j)
	if err != nil {
		return "", ""
	}
	it, ok := InProgressItem(items, j.Fingerprint)
	if !ok {
		return "", ""
	}
	head += "，做到一半（BACKLOG 里这一条仍标进行中" + BranchNote(j.ProjectDir, it.Text) + "）"
	used := state.TransientStreak(st, j.Project, j.Fingerprint)
	if used >= len(state.TransientBackoff) {
		return state.StatusFailed, fmt.Sprintf("%s；已经连续 %d 次上游瞬时错误，不再续跑，按失败算", head, used+1)
	}
	j.Transient, j.ResumePending = true, true
	return state.StatusInterrupted, fmt.Sprintf("%s；记中断，用同一会话续跑（第 %d/%d 次）", head, used+1, len(state.TransientBackoff))
}
