package runner

import (
	"fmt"
	"os"
	"time"

	"agentq/internal/config"
	"agentq/internal/notify"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// RecordOutcome 按刚结束的任务（运行了 ran）更新连环失败计数，需要熔断时设好 BreakerUntil，返回是否熔断、熔断多久。
// 秒退 = 没跑完（失败、中断、启动失败）且运行不到 Config.BreakerQuick。用户 stop 的、撞用量上限的、撞预算的、自动停的、机器重启中断的、上游瞬时错误的不算，
// 也不清零（上限有自己的冷却，瞬时错误有自己的退避）。其余结束（跑完、跑了一阵才失败、超时）说明不是系统性故障，计数和翻倍都清零；
// 跑完的还解除正在生效的熔断（比如用户修好后用 run 试跑成功了）。
func RecordOutcome(st *state.State, cfg config.Config, j *state.Job, ran time.Duration, now time.Time) (bool, time.Duration) {
	if j.Status == state.StatusKilled || j.LimitHit || j.Rebooted || j.Transient || j.BudgetHit || j.AutoStop != "" {
		return false, 0
	}
	// 商量大方向是可有可无的后台活：它失败不说明待做任务有问题，更不该因此停掉真正的活
	if j.Kind == state.KindDiscuss {
		return false, 0
	}
	// 夜班、复盘、new 只计秒退：它们跑完不说明待做任务的问题好了，不清零、不解除熔断
	if j.Kind != "" && (j.Status == state.StatusDone || ran >= cfg.BreakerQuick.Duration) {
		return false, 0
	}
	if j.Status == state.StatusDone {
		st.BreakerUntil = time.Time{}
	}
	if j.Status == state.StatusDone || ran >= cfg.BreakerQuick.Duration {
		st.FailStreak, st.BreakerLevel = 0, 0
		return false, 0
	}
	// 熔断中结束的（熔断前就启动的、或用户用 run 启动的）不再计，免得一次故障连续加码
	if now.Before(st.BreakerUntil) {
		return false, 0
	}
	st.FailStreak++
	if st.BreakerLevel == 0 && st.FailStreak < cfg.BreakerStreak {
		return false, 0
	}
	d := cfg.BreakerCooldown.Duration
	for i := 0; i < st.BreakerLevel && d < cfg.BreakerMax.Duration; i++ {
		d *= 2
	}
	if d > cfg.BreakerMax.Duration {
		d = cfg.BreakerMax.Duration
	}
	st.BreakerUntil = now.Add(d)
	st.BreakerLevel++
	st.FailStreak = 0
	return true, d
}

// noteOutcome 在任务结束时调用：更新熔断计数，熔断了就记日志并在当天汇总写一行报警，跑完解除熔断时也记一行。
func (a *Service) noteOutcome(st *state.State, j *state.Job) {
	level, active := st.BreakerLevel, a.Now().Before(st.BreakerUntil)
	tripped, d := RecordOutcome(st, a.Cfg, j, a.ExitedAt(j).Sub(j.StartedAt), a.Now())
	if active && st.BreakerUntil.IsZero() {
		msg := fmt.Sprintf("熔断解除：任务 %s（%s「%s」）跑完，恢复自动接活", j.ID, j.Project, textfmt.TruncRunes(j.Title, 40))
		a.Logf("%s", msg)
		a.Ledger.AppendNightlyLine(a.Now(), msg)
	}
	if !tripped {
		return
	}
	why := fmt.Sprintf("连续 %d 个任务都在 %s 内结束且没做完", a.Cfg.BreakerStreak, textfmt.FormatDuration(a.Cfg.BreakerQuick.Duration))
	if level > 0 {
		why = "熔断过后第一个任务又秒退，冷却时长翻倍（上限 " + textfmt.FormatDuration(a.Cfg.BreakerMax.Duration) + "）"
	}
	msg := fmt.Sprintf("熔断：%s，暂停启动新任务 %s，到 %s；最近一个是任务 %s（%s「%s」%s：%s），查原因看 agentq logs %s",
		why, textfmt.FormatDuration(d), textfmt.WhenCN(st.BreakerUntil), j.ID, j.Project, textfmt.TruncRunes(j.Title, 40),
		state.StatusLabel(j.Status), textfmt.OneLine(j.Summary, 120), j.ID)
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	a.Notify.Notify(notify.Notice{Cat: "alert", Key: "breaker", Text: "连环失败熔断：暂停接活到 " + textfmt.WhenCN(st.BreakerUntil),
		Lines: []string{why, fmt.Sprintf("最近一个：%s「%s」%s，看 agentq logs %s", j.Project, textfmt.TruncRunes(j.Title, 30), state.StatusLabel(j.Status), j.ID)}})
}

// ExitedAt 估计 claude 实际退出的时间：result.json 非空时取它的修改时间（--output-format json 只在退出时写一次）；
// 否则用收尾时间。收尾可能比退出晚好几分钟（等锁、只靠定时 tick 时），拿收尾时间算会把秒退算成跑了一阵。
// 空的 result.json 和 stderr.log 不能用：它们启动时就建好了，修改时间是启动时刻，超时或被杀的任务会被算成秒退。
func (a *Service) ExitedAt(j *state.Job) time.Time {
	t := a.Now()
	if j.EndedAt != nil {
		t = *j.EndedAt
	}
	fi, err := os.Stat(a.Ledger.JobOutput(j.ID, "result.json"))
	if err == nil && fi.Size() > 0 && fi.ModTime().After(j.StartedAt) && fi.ModTime().Before(t) {
		return fi.ModTime()
	}
	return t
}
