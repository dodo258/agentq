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

// limitFromStructuredOnly：商量大方向的结果文字里可能有项目文件的内容：「rate limit」这种字样不能算撞上限，否则别人写一句话就能停掉全部任务。只认结构化字段。
func limitFromStructuredOnly(kind string) bool {
	return kind == state.KindDiscuss
}

// jobHitLimit：这个任务是不是撞了用量上限。
func (a *Service) jobHitLimit(j *state.Job, r backend.Result) bool {
	if limitFromStructuredOnly(j.Kind) {
		return r.LimitStructured
	}
	return r.LimitStructured || r.LimitText
}

// noteTransient 判断这次失败是不是可以短退避的瞬时错误：是就设 j.Transient（不计连环失败），返回写进摘要开头的说明。
// 不是瞬时错误、或花了钱（做到一半才出错，重试帮不上）返回空。重试用完的那次不设 Transient，照普通失败计数，
// 上游长时间故障时熔断还能起作用。
func (a *Service) noteTransient(st *state.State, j *state.Job, res backend.Result) string {
	code := res.HTTPStatus
	if !backend.TransientStatus(code) {
		return ""
	}
	a.Ledger.ReadJobUsage(st, j)
	costKnown := j.Usage != nil && !j.Usage.CostUnknown
	if costKnown && j.Usage.CostUSD >= TransientMaxCostUSD || !costKnown && res.Turns > backend.LimitScanTurns { // 没有金额的后端退回只看回合数
		return ""
	}
	head := fmt.Sprintf("上游瞬时错误（HTTP %d）", code)
	if j.Kind != "" || j.Fingerprint == "" { // 夜班、复盘、对话没有条目可重试
		j.Transient = true
		return head + "，不计连环失败"
	}
	used := state.TransientStreak(st, j.Project, j.Fingerprint)
	if used >= len(state.TransientBackoff) {
		return fmt.Sprintf("%s，已重试 %d 次，回到 retry_after", head, used)
	}
	j.Transient = true
	items, err := backlog.Project{Dir: j.ProjectDir}.Items()
	if err != nil {
		return head + "；核验 BACKLOG 失败：" + err.Error()
	}
	if _, ok := InProgressItem(items, j.Fingerprint); ok {
		return head + "；BACKLOG 里这一条仍标进行中，不自动重试，等接手"
	}
	return fmt.Sprintf("%s，%s 后重试（第 %d/%d 次）", head, textfmt.FormatDuration(state.TransientDelay(used+1, a.Cfg.RetryAfter.Duration)), used+1, len(state.TransientBackoff))
}
