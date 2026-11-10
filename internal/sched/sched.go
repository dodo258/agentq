// Package sched 编排一次 tick 的固定流程（收尾、超时、补跑、挑活、巡逻、推送、清理），调用顺序就是行为。
package sched

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/chat"
	"agentq/internal/config"
	"agentq/internal/discuss"
	"agentq/internal/env"
	"agentq/internal/evolve"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/inbox"
	"agentq/internal/ledger"
	"agentq/internal/newproj"
	"agentq/internal/notify"
	"agentq/internal/orphan"
	"agentq/internal/patrol"
	"agentq/internal/release"
	"agentq/internal/routine"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/telegram"
	"agentq/internal/textfmt"
)

// Service 是调度的模块实例：tick 的流程和每天一次的磁盘清理。
type Service struct {
	*env.Env
	Backlog  *backlog.Service
	Chat     *chat.Service
	Discuss  *discuss.Service
	Evolve   *evolve.Service
	Gate     *gate.Service
	Inbox    *inbox.Service
	Ledger   *ledger.Service
	Newproj  *newproj.Service
	Notify   *notify.Service
	Orphan   *orphan.Service
	Patrol   *patrol.Service
	Release  *release.Service
	Routine  *routine.Service
	Runner   *runner.Service
	Suggest  *suggest.Service
	Telegram *telegram.Service
}

// NotifyTick：tick 末尾调用。看「等用户决定」有没有新条目；agentq listen 没在跑时兜底收用户发来的 Telegram 消息；
// 收件箱里有消息、上一个对话任务结束了就启动下一个；把到点的消息发出去。
// 出任何错都只记日志，不能让 tick 非 0 退出（连续 2 次会触发自动回滚）。
func (a *Service) NotifyTick(st *state.State) {
	if !a.Cfg.NotifyEnabled {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			a.Logf("推送：出错，本次不推送，不影响 tick：%v", p)
		}
	}()
	if c, _ := a.Telegram.TelegramCreds(); c != nil {
		a.Chat.PollIfNoListener(c) // 自己拿 notify.lock，附件下载在锁外
	}
	a.Release.ReleaseTick() // 关着或没有要处理的什么都不做；联网，放在拿 notify.lock 之前
	err := a.Notify.WithNotify(func(s *notify.State) error {
		c, credErr := a.Telegram.TelegramCreds()
		if credErr != nil {
			a.Notify.NotifyErr(s, "Telegram 凭据用不了（推送先弹本机通知）："+credErr.Error())
		}
		a.Notify.WatchDecisions(s)
		if c != nil {
			a.Chat.RetryReplies(s, c)
			a.Chat.ServeInbox(s, st, c)
		}
		a.Notify.FlushNotify(s, st, c, credErr)
		return nil
	})
	if err != nil {
		a.Logf("推送：%v", err)
	}
	a.Notify.SendQueuedFiles()
}

// Tick：①收尾 ②超时处理 ③补跑今天错过的夜班、复盘（见 internal/routine/routine.go）④挑活启动 ⑤离上次巡逻满一小时就巡逻一次（见 internal/patrol/patrol.go）⑥推送和收 Telegram 消息（见 internal/notify/notify.go）。
// after 非空表示是任务外壳在该任务结束后触发的：拿不到锁时等一会儿（最多 cmdLockWait）而不是跳过，
// 否则正在跑的那次 tick 已经收尾过、没看到这个任务结束，空出的位置要等下一次定时 tick。
func (a *Service) Tick(after string) error {
	if after != "" && !a.Cfg.TickAfterJob {
		return nil
	}
	name, wait := "tick", time.Duration(0)
	if after != "" {
		name, wait = "tick（任务 "+after+" 结束后）", runner.CmdLockWait
	}
	lock, err := fsx.AcquireLock(a.LockPath(), wait)
	if err != nil {
		a.Logf("%s：拿不到 %s：%v", name, a.LockPath(), err)
		return env.ErrLogged
	}
	if lock == nil {
		if after != "" {
			a.Logf("%s：等了 %s 还拿不到 %s，本次跳过，等下次定时 tick", name, runner.CmdLockWait, a.LockPath())
		} else {
			a.Logf("%s：上一个 tick 还没结束，本次跳过", name)
		}
		return nil
	}
	defer lock.Close()

	st, err := state.Load(a.StatePath())
	recovered := false
	if errors.Is(err, state.ErrStateCorrupt) {
		st, err = a.recoverState(err)
		recovered = err == nil
	}
	if err != nil {
		a.Logf("%s：状态文件读不出，当作未知，本次不收尾也不启动：%v", name, err)
		return env.ErrLogged
	}
	a.clampClock(st)
	finished := a.Runner.Reap(st)
	finished += a.Runner.EnforceTimeouts(st)
	a.Runner.CheckStalls(st)
	a.Runner.CheckProgress(st)
	a.Runner.CheckLiveBudget(st)
	a.Gate.MigrateVerify(st)
	a.Gate.Collect(st)
	a.Discuss.Collect(st)
	a.Runner.RetryStuckMarks(st)
	a.Inbox.ReconcileInbox(st)
	a.Newproj.DropGoneHeld(st)
	st.Prune(a.Now())
	if err := state.Save(a.StatePath(), st); err != nil {
		a.Logf("%s：写状态文件失败，本次不启动：%v", name, err)
		return env.ErrLogged
	}

	if !recovered {
		a.Routine.CatchUpRoutines(st)
	}
	a.reloadPaused()
	gated := 0
	if !recovered {
		gated = a.Gate.StartQueued(st)
		a.startDecidedNews(st)
	}
	var orphans []orphan.OrphanJob
	if !recovered && !a.Cfg.Paused {
		orphans = a.Orphan.ScanOrphans(st)
		// 下面冷却、熔断时不再存状态；扫描记下的等待记录和已报标记要留住
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("%s：写状态文件失败：%v", name, err)
		}
	}
	started, note := 0, ""
	now := a.Now()
	switch {
	case recovered:
		note = "状态文件损坏后按 meta.json 重建，本次只收尾不启动"
	case a.Cfg.Paused:
		note = pausedNote
		if !slices.ContainsFunc(st.PendingResumes(), func(j *state.Job) bool { return j.Manual }) {
			break
		}
		// 你亲手启动的条目被门退回、中断了：暂停只管自动接活，这一条照样接着做完
		var why string
		started, why = a.schedule(st, nil, true)
		if started > 0 {
			note += fmt.Sprintf("；接着做你手动启动的 %d 条", started)
		}
		if why != "" {
			note += "；" + why
		}
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("%s：写状态文件失败：%v", name, err)
		}
	case st.AllCooling(a.Cfg, now):
		note = "用量冷却中，到 " + textfmt.WhenCN(st.CooldownUntil) + " 前不启动"
	case now.Before(st.BreakerUntil):
		note = "连环失败熔断中，到 " + textfmt.WhenCN(st.BreakerUntil) + " 前不启动"
	default:
		started, note = a.schedule(st, orphans, false)
		// 续跑会改被续跑任务的记录（ResumedBy、ResumePending），启动时只存了一次
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("%s：写状态文件失败：%v", name, err)
		}
		if started == 0 && a.Cfg.SuggestEnabled && !a.Cfg.Paused { // 关着时这一行什么都不做
			if n := a.Suggest.MaybeSuggest(st); n != "" {
				note = strings.TrimPrefix(note+"；"+n, "；")
			}
		}
	}
	msg := fmt.Sprintf("%s：运行中 %d/%d，本次收尾 %d，启动 %d", name, len(st.Tasks()), a.Cfg.MaxParallel, finished, started)
	if gated > 0 {
		msg += fmt.Sprintf("，过门 %d", gated)
	}
	if note != "" {
		msg += "；" + note
	}
	a.Logf("%s", msg)
	a.Patrol.PatrolIfDue(st)
	a.Evolve.CollectIfDue() // 自我进化的信号：只读本机文件，不调模型
	a.NotifyTick(st)
	if !recovered {
		a.retention(st) // 不调模型，删不掉只记日志
	}
	return nil
}

// startDecidedNews：商量定了的新项目开工。和 agentq new 一样不看暂停，占名额，遵守冷却和额度；起不来下次 tick 再试。
func (a *Service) startDecidedNews(st *state.State) {
	for _, r := range a.Discuss.DecidedNew() {
		res, err := a.Newproj.StartDirected(st, r.Idea, r.DirectionText())
		switch {
		case errors.Is(err, newproj.ErrSameIdea):
			a.Discuss.NewStarted(r, "")
		case err != nil:
			a.Discuss.NewFailed(r, err)
		default:
			a.Discuss.NewStarted(r, res.Job.ID)
			a.Logf("启动任务 %s：新项目「%s」pid %d", res.Job.ID, res.Job.Title, res.Job.PID)
		}
	}
}

const pausedNote = "已暂停（paused=true），只收尾不启动"

// reloadPaused：配置是进程启动时读的，到这里可能已经过了很久（等锁、杀不理 SIGTERM 的进程组、查登录和额度），这期间用户可能 pause 了，接活之前以文件为准。
// 文件不存在就不动（config.Load 会写默认配置）。读不出就是不知道有没有暂停，这次按暂停算，下次 tick 启动时读不出会报错。
func (a *Service) reloadPaused() {
	if _, err := os.Stat(a.ConfigPath()); errors.Is(err, os.ErrNotExist) {
		return
	}
	cfg, err := config.Load(a.ConfigPath())
	if err != nil {
		a.Logf("配置读不出，这次 tick 不接活：%v", err)
		a.Cfg.Paused = true
		return
	}
	a.Cfg.Paused = cfg.Paused
}

// clampClock：时钟跳到未来又跳回来后，记录里的时间都在「未来」。不收拾的话冷却、熔断、等待要等到那一天，超时永远不生效，名额永远占着。
// 只收拾不可能正常出现的值：冷却（提示的重置时间最长一周）、熔断、条目等待超过上限的，和发生在未来的任务时间。
func (a *Service) clampClock(st *state.State) {
	now := a.Now()
	var fixed []string
	limit := now.Add(max(8*24*time.Hour, a.Cfg.LimitCooldown.Duration) + time.Hour)
	if st.CooldownUntil.After(limit) {
		st.CooldownUntil = now.Add(a.Cfg.LimitCooldown.Duration)
		fixed = append(fixed, "用量冷却")
	}
	for name, l := range st.BackendLimits {
		if l.CooldownUntil.After(limit) {
			l.CooldownUntil = now.Add(a.Cfg.LimitCooldown.Duration)
			fixed = append(fixed, "后端 "+name+" 的用量冷却")
		}
	}
	if st.BreakerUntil.After(now.Add(a.Cfg.BreakerMax.Duration + time.Hour)) {
		st.BreakerUntil = now.Add(a.Cfg.BreakerCooldown.Duration)
		fixed = append(fixed, "熔断")
	}
	for fp, until := range st.ItemHold {
		if until.After(now.Add(a.Cfg.RetryAfter.Duration + time.Hour)) {
			st.ItemHold[fp] = now.Add(a.Cfg.RetryAfter.Duration)
			fixed = append(fixed, "条目等待")
		}
	}
	for _, j := range st.Jobs {
		changed := false
		if j.StartedAt.After(now.Add(time.Hour)) {
			j.StartedAt, changed = now, true
		}
		if j.EndedAt != nil && j.EndedAt.After(now.Add(time.Hour)) {
			t := now
			j.EndedAt, changed = &t, true
		}
		if changed {
			a.Ledger.WriteMeta(j)
			fixed = append(fixed, "任务 "+j.ID)
		}
	}
	if len(fixed) > 0 {
		a.Logf("时钟跳变：记录里有未来的时间（现在 %s），已改回现在算：%s", now.Format("2006-01-02 15:04"), strings.Join(fixed, "、"))
	}
}

// metaReadUnknown：这个读错误可能是暂时的，内容也许是好的。不存在、meta.json 是目录、任务目录是普通文件这些改不好的，算记录丢了。
func metaReadUnknown(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// recoverState 在 state.json 内容损坏时调用：先把坏文件备份成 state.json.bad-<时间>，再按 jobs 目录下各任务的
// meta.json 重建任务列表（运行中的任务照常收尾和超时，进程在不在仍靠 ps 核对命令行里的 agentq 标记），
// 写日志和当天汇总报警。用量冷却只能按撞上限任务的结束时间 + limit_cooldown 估回来，熔断记录丢失。
// 重建的状态由调用方照常保存；保存失败时坏文件还在，下次 tick 再备份重建一次。备份不了就返回错误，当作未知。
func (a *Service) recoverState(cause error) (*state.State, error) {
	raw, err := os.ReadFile(a.StatePath())
	if err != nil {
		return nil, fmt.Errorf("%v；备份时读不出：%w", cause, err)
	}
	// 权限、IO 这类暂时性的读错误：任务可能好好的，不重建也不杀，当未知。放在备份之前：不然每次 tick 多一份坏文件
	metaDirs, _ := os.ReadDir(filepath.Join(a.DataDir(), "jobs"))
	for _, d := range metaDirs {
		if _, err := os.ReadFile(filepath.Join(a.JobDir(d.Name()), "meta.json")); metaReadUnknown(err) {
			return nil, fmt.Errorf("%v；任务 %s 的 meta.json 读不了：%w", cause, d.Name(), err)
		}
	}
	bak := a.StatePath() + ".bad-" + a.Now().Format("20060102-150405")
	if err := fsx.WriteFileAtomic(bak, raw); err != nil {
		return nil, fmt.Errorf("%v；备份到 %s 失败：%w", cause, bak, err)
	}
	st := &state.State{}
	dirs, _ := os.ReadDir(filepath.Join(a.DataDir(), "jobs"))
	var bad, killed []string
	for _, d := range dirs {
		id := d.Name()
		b, err := os.ReadFile(filepath.Join(a.JobDir(id), "meta.json"))
		if metaReadUnknown(err) { // 上面查过之后才坏的
			return nil, fmt.Errorf("%v；任务 %s 的 meta.json 读不了：%w", cause, id, err)
		}
		var j state.Job
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID != id || j.Status == "" {
			// 没记录的任务进程不能留着（见 KillLostJob）。查不了就当未知：不启动，下次 tick 再来
			n, killErr := a.Runner.KillLostJob(id)
			if killErr != nil {
				return nil, fmt.Errorf("%v；查任务 %s 还有没有进程失败：%w", cause, id, killErr)
			}
			if n > 0 {
				killed = append(killed, fmt.Sprintf("%s（%d 个）", id, n))
			}
			if n > 0 || !errors.Is(err, os.ErrNotExist) { // 目录在、meta.json 没有、也没有进程：不是任务目录
				bad = append(bad, id)
			}
			continue
		}
		st.Jobs = append(st.Jobs, &j)
		if j.LimitHit && j.EndedAt != nil {
			st.ExtendCooldown(j.Backend, a.Cfg.Backend, j.EndedAt.Add(a.Cfg.LimitCooldown.Duration))
		}
	}
	sort.SliceStable(st.Jobs, func(i, k int) bool { return st.Jobs[i].StartedAt.Before(st.Jobs[k].StartedAt) })
	msg := fmt.Sprintf("状态文件损坏：%v；已备份为 %s，按 %d 个任务的 meta.json 重建（运行中 %d 个，照常收尾和超时）",
		cause, bak, len(st.Jobs), len(st.Running()))
	if len(bad) > 0 {
		msg += fmt.Sprintf("；%d 个任务的 meta.json 读不出，没算进去：%s", len(bad), strings.Join(bad, "、"))
	}
	if len(killed) > 0 {
		msg += "；其中还活着的进程已杀掉（没有记录就没人收尾，条目还会再被接一次）：" + strings.Join(killed, "、")
	}
	msg += "；熔断记录丢了，用量冷却按撞上限任务的结束时间估；本次 tick 不启动新任务"
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	return st, nil
}

// branchBlocked：这个项目现在接不了这一条。recorded 是任务记下的分支，新任务是空的，按项目现认。放在查登录和额度之前：接不了就不用查。同样的原因只记一次，status 一直写着。
func (a *Service) branchBlocked(st *state.State, name, dir, recorded string, notes *[]string) bool {
	err := a.branchErr(st, name, dir, recorded)
	if err == nil {
		return false
	}
	key := name + "|" + recorded
	if msg := err.Error(); st.BranchBlocked[key] != msg {
		if st.BranchBlocked == nil {
			st.BranchBlocked = map[string]string{}
		}
		st.BranchBlocked[key] = msg
		*notes = append(*notes, name+" "+msg)
	}
	return true
}

func (a *Service) branchErr(st *state.State, name, dir, recorded string) error {
	if recorded == "" {
		return a.Runner.ProjectBlock(st, name, dir)
	}
	_, err := a.Runner.JobBranch(dir, recorded)
	return err
}

// clearBranchBlocked：好了的（切回去了、项目不看了）不再记着，下次再挡住还要记一行。
func (a *Service) clearBranchBlocked(st *state.State, projects []backlog.Project) {
	for key := range st.BranchBlocked {
		cut := strings.LastIndex(key, "|")
		if cut < 0 { // 这个版本写的都有分隔符
			delete(st.BranchBlocked, key)
			continue
		}
		name, recorded := key[:cut], key[cut+1:]
		i := slices.IndexFunc(projects, func(p backlog.Project) bool { return p.Name == name })
		if i < 0 || a.branchErr(st, name, projects[i].Dir, recorded) == nil {
			delete(st.BranchBlocked, key)
		}
	}
}

// schedule 按项目名字母序挑活，每个项目每次最多启动 1 个。返回启动个数和备注。
// manualOnly：暂停时只续跑用户亲手启动的任务（Job.Manual），不接孤儿、不挑新条目；冷却、登录、额度、名额、熔断照样管。
func (a *Service) schedule(st *state.State, orphans []orphan.OrphanJob, manualOnly bool) (int, string) {
	if ok, why := state.Capacity(st, a.Cfg, ""); !ok {
		return 0, why
	}
	projects, scanErr := a.Backlog.ActiveProjects()
	if scanErr != nil && len(projects) == 0 {
		return 0, "扫描项目目录失败：" + scanErr.Error()
	}
	started := 0
	notReady := false
	var notes []string
	if scanErr != nil { // projects_root 出了问题，extra_projects 的项目照跑，这句每次都说
		notes = append(notes, "扫描项目目录失败："+scanErr.Error())
	}
	if scanErr == nil {
		a.clearBranchBlocked(st, projects)
	}
	// ready：真要启动时才查这个后端的冷却、登录和额度（额度读一次缓存 usageCacheFor）。额度 ≥usageStopAt 就不再启动；读不出当未知照常启动。每个后端一次 tick 查一次。
	def := a.Cfg.Backend
	if def == "" {
		def = backend.Default
	}
	blocked := map[string]bool{}
	ready := func(name string) bool {
		if name == "" {
			name = backend.Default
		}
		whose := "" // 只有默认后端时话不变
		if name != def {
			whose = "后端 " + name + "："
		}
		seen, checked := blocked[name]
		if !checked {
			seen = false
			if until := st.CooldownOf(name, def); a.Now().Before(until) {
				seen = true
				notes = append(notes, "后端 "+name+" 用量冷却中，到 "+textfmt.WhenCN(until)+" 前不启动")
			} else if ok, why := a.Runner.CheckLoginOn(name); !ok {
				seen = true
				notes = append(notes, why)
			} else {
				switch lvl, until, why := backend.UsageLevel(a.Runner.UsageOn(st, name), a.Now(), a.Cfg.ForBackend(name).UsagePolicy()); {
				case lvl >= backend.UsageStop:
					seen = true
					notes = append(notes, whose+backend.UsageGateNote(lvl, until, why))
				case lvl == backend.UsageUnknown:
					notes = append(notes, whose+"额度未知，照常启动："+textfmt.OneLine(st.UsageOf(name, def).Err, 120))
				}
			}
			blocked[name] = seen
		}
		if seen {
			return false
		}
		// 登录和额度检查可能要一分多钟，前一个任务启动也要时间：每次真要启动前再看一次有没有暂停
		a.reloadPaused()
		if a.Cfg.Paused && !manualOnly {
			notes = append(notes, pausedNote)
			return false
		}
		return true
	}
	// done：这个后端接不了活时这次 tick 是不是到此为止。只有一种后端、或者暂停了就是；混用时别的后端的项目接着看。
	mixed := len(a.Cfg.BackendsInUse()) > 1
	done := func(name string) bool {
		if name == "" {
			name = backend.Default
		}
		return (a.Cfg.Paused && !manualOnly) || (name == def && !mixed)
	}
	// 先续跑中断的任务（它们占着 BACKLOG 的进行中标记，不续跑就一直没人管），续跑过的项目这次不再挑新条目。
	resumed := map[string]bool{}
	// 这次 tick 里刚启动就失败的任务可能触发熔断，熔断了就不再启动
	tripped := false
	breaker := func() bool {
		if !tripped && a.Now().Before(st.BreakerUntil) {
			tripped = true
			notes = append(notes, "连环失败熔断，到 "+textfmt.WhenCN(st.BreakerUntil)+" 前不启动")
		}
		return tripped
	}
	for _, orig := range st.PendingResumes() {
		if manualOnly && !orig.Manual {
			continue
		}
		if orig.Kind == state.KindInbox { // 任务箱没有 BACKLOG 条目，排在项目待做之后，在下面接
			continue
		}
		if scanErr != nil && !slices.ContainsFunc(projects, func(p backlog.Project) bool { return p.Name == orig.Project }) {
			continue // 它的项目现在看不到：留着等 projects_root 回来，不算中断了结
		}
		if a.Backlog.Excluded(orig.Project) { // 不自动续跑，也别一直挂着：放掉后巡逻照常报这个中断
			orig.ResumePending = false
			orig.Summary += "；项目在 exclude_projects 里，不自动续跑，要接着做就 agentq run " + orig.Project
			a.Ledger.WriteMeta(orig)
			a.Logf("任务 %s：%s 在 exclude_projects 里，不自动续跑", orig.ID, orig.Project)
			continue
		}
		if ok, _ := state.Capacity(st, a.Cfg, ""); !ok || breaker() {
			break
		}
		if a.Runner.PermissionBlocked(orig.Backend) { // 配好了再续跑，记录留着
			continue
		}
		if orig.Kind == state.KindNew { // 撞预算的 new：没有 BACKLOG 条目，核对不了进行中标记，用同一会话接着做一次
			if !ready(orig.Backend) {
				if done(orig.Backend) {
					return started, strings.Join(notes, "；")
				}
				continue
			}
			j, err := a.Newproj.ResumeNew(st, orig)
			if err != nil {
				notes = append(notes, "新项目续跑失败："+err.Error())
				continue
			}
			a.Logf("续跑任务 %s（接 %s，会话 %s）：%s「%s」pid %d", j.ID, orig.ID, orig.SessionID, j.Project, j.Title, j.PID)
			started++
			continue
		}
		if runner.ScopeBlocked(st, a.Cfg, orig.Project, orig.Item) != "" { // 先等它续跑：这次不让同项目写了范围的新条目插队，原因在 status
			resumed[orig.Project] = true
			continue
		}
		if ok, _ := state.Capacity(st, a.Cfg, orig.Project); !ok || resumed[orig.Project] {
			continue
		}
		if a.branchBlocked(st, orig.Project, orig.ProjectDir, orig.DefaultBranch, &notes) { // 续跑记录留着，切回去接着做。先看分支再读 BACKLOG：切走了工作目录里是别的分支的
			continue
		}
		items, err := backlog.Project{Dir: orig.ProjectDir}.Items()
		if err != nil {
			notes = append(notes, orig.Project+" 的 BACKLOG 读不出，暂不续跑："+err.Error())
			continue
		}
		if _, ok := runner.InProgressItem(items, orig.Fingerprint); !ok || st.RunningFor(orig.Project, orig.Fingerprint) != nil {
			orig.ResumePending = false
			a.Logf("任务 %s 的条目已不在进行中或已有任务在做，不再续跑", orig.ID)
			continue
		}
		if !ready(orig.Backend) {
			if done(orig.Backend) {
				return started, strings.Join(notes, "；")
			}
			continue
		}
		j, err := a.Runner.LaunchResume(st, orig)
		resumed[orig.Project] = true
		if err != nil {
			notes = append(notes, orig.Project+" 续跑失败："+err.Error())
			continue
		}
		a.Logf("续跑任务 %s（接 %s，会话 %s）：%s「%s」pid %d", j.ID, orig.ID, orig.SessionID, j.Project, j.Title, j.PID)
		started++
	}
	if manualOnly {
		return started, strings.Join(notes, "；")
	}
	if a.Chat.RepliesWaiting() { // tick 末尾的 notifyTick 会接上它
		notes = append(notes, "有你的回复在等名额，这次不接新条目")
		return started, strings.Join(notes, "；")
	}
	// 接手孤儿条目（见 internal/orphan/orphan.go）：先于挑新条目，占名额，受额度和熔断限制；扫描后条目变了（用户刚动过）就不接手。
	for _, o := range orphans {
		if ok, _ := state.Capacity(st, a.Cfg, ""); !ok || breaker() {
			break
		}
		if runner.ScopeBlocked(st, a.Cfg, o.Project.Name, o.Item.Text) != "" { // 同上：先等接手，这次不挑新条目
			resumed[o.Project.Name] = true
			continue
		}
		if ok, _ := state.Capacity(st, a.Cfg, o.Project.Name); !ok || resumed[o.Project.Name] {
			continue
		}
		items, err := o.Project.Items()
		if err != nil {
			notes = append(notes, o.Project.Name+" 的 BACKLOG 读不出，暂不接手："+err.Error())
			continue
		}
		if it, ok := runner.InProgressItem(items, o.Item.Fingerprint); !ok || it.Text != o.Item.Text {
			continue
		}
		if a.Runner.PermissionBlocked(a.Cfg.ProjectBackend(o.Project.Name)) {
			continue
		}
		if a.branchBlocked(st, o.Project.Name, o.Project.Dir, runner.TakeoverRecorded(st, o.Project, o.Item), &notes) {
			continue
		}
		if name := a.Cfg.ProjectBackend(o.Project.Name); !ready(name) {
			if done(name) {
				return started, strings.Join(notes, "；")
			}
			continue
		}
		j, err := a.Orphan.LaunchTakeover(st, o)
		resumed[o.Project.Name] = true
		if err != nil {
			notes = append(notes, o.Project.Name+" 接手失败："+err.Error())
			continue
		}
		a.Logf("接手任务 %s：%s「%s」分支 %s pid %d", j.ID, j.Project, j.Title, o.Facts.Branch, j.PID)
		started++
	}
projects:
	for _, p := range projects {
		if ok, _ := state.Capacity(st, a.Cfg, ""); !ok || breaker() {
			break
		}
		if ok, _ := state.Capacity(st, a.Cfg, p.Name); !ok || resumed[p.Name] {
			continue
		}
		if newproj.NewProjectBusy(st, p.Name) { // 可能是 new 任务正在建的项目，第一条还没认领，或者第一版还没合并，别抢
			notes = append(notes, p.Name+" 是新项目任务启动后才出现的，等它结束再挑")
			continue
		}
		if st.HeldProject(p.Name) {
			notes = append(notes, p.Name+" 是停掉的新项目，不自动做，要接着做就 agentq run "+p.Name)
			continue
		}
		// 第一版还挡着时先看分支：切走了工作目录里是别的分支的 BACKLOG，第一条看着「不在了」，会误放开；日志也只说切分支这个真原因
		if runner.FirstVersionPending(st, p.Name) && a.branchBlocked(st, p.Name, p.Dir, "", &notes) {
			continue
		}
		items, err := p.Items()
		if err != nil {
			notes = append(notes, p.Name+" 的 BACKLOG 读不出："+err.Error())
			continue
		}
		if hold, held := runner.FirstVersionHold(st, p, items); held { // 第一版没合并：只有第一条能挑
			a.Runner.NoteFirstHold(st, p, hold)
			items = []backlog.Item{hold.Item}
		}
		name := a.Cfg.ProjectBackend(p.Name)
		// 同项目可以连挑，直到 per_project 或全局上限：第二条起要写了范围、和在跑的都不重叠（Pick 判），没写范围的照旧一次一条。
		for n := 0; ; n++ {
			if n > 0 {
				if ok, _ := state.Capacity(st, a.Cfg, p.Name); !ok || breaker() {
					break
				}
			}
			var it backlog.Item
			ok, verdict := false, discuss.Proceed
			for { // 在等商量的跳过，同项目下一条照常：不挡别的条目
				if it, ok = a.Runner.Pick(st, items, "", true); !ok {
					break
				}
				if a.Runner.PermissionBlocked(name) || a.branchBlocked(st, p.Name, p.Dir, "", &notes) {
					ok = false
					break
				}
				if it, verdict = a.Discuss.Hold(st, p, it, func() bool { return ready(name) }); verdict != discuss.Wait {
					break
				}
				waited := it
				items = slices.DeleteFunc(slices.Clone(items), func(x backlog.Item) bool { return x.Title == waited.Title })
			}
			if verdict == discuss.NotReady || ok && !ready(name) {
				if done(name) {
					notReady = true
					break projects
				}
				break
			}
			if !ok {
				break
			}
			j, err := a.Runner.Launch(st, p, it)
			if errors.Is(err, runner.ErrModelWindow) { // 只挡用这个模型的条目：同项目下一条照挑
				notes = append(notes, p.Name+"「"+textfmt.TruncRunes(it.Title, 20)+"」"+err.Error())
				skipped := it
				items = slices.DeleteFunc(slices.Clone(items), func(x backlog.Item) bool { return x.Title == skipped.Title })
				continue
			}
			if err != nil {
				notes = append(notes, p.Name+" 启动失败："+err.Error())
				break
			}
			a.Logf("启动任务 %s：%s「%s」pid %d", j.ID, p.Name, j.Title, j.PID)
			started++
		}
	}
	// 任务箱排在项目待做之后：项目都接完还有名额才接，同一时间最多一个。
	if ok, _ := state.Capacity(st, a.Cfg, inbox.InboxJobName); ok && !notReady && !breaker() && !state.HasRunning(st, state.KindInbox) && !a.Runner.PermissionBlocked(a.Cfg.ProjectBackend(inbox.InboxJobName)) {
		n, note := a.Inbox.ScheduleInbox(st, func() bool { return ready(a.Cfg.ProjectBackend(inbox.InboxJobName)) })
		started += n
		if note != "" {
			notes = append(notes, note)
		}
	}
	return started, strings.Join(notes, "；")
}

func (a *Service) CmdSetPaused(paused bool) error {
	if err := config.SetPaused(a.ConfigPath(), paused); err != nil {
		return err
	}
	a.Cfg.Paused = paused
	if paused {
		fmt.Fprintln(a.Out, "已暂停：tick 只收尾不启动新任务（agentq run 仍可手动启动）")
	} else {
		fmt.Fprintln(a.Out, "已恢复：下次 tick 起照常接活")
	}
	return nil
}
