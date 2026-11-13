// Package runner 管任务从启动到收尾的全过程：启动、续跑、超时、熔断、预算、额度门。启动参数和按任务种类的收尾动作属于上层模块，走 Hooks 调。
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/gitx"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是任务生命周期的模块实例。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Gitx    *gitx.Service
	Ledger  *ledger.Service
	Notify  *notify.Service
	Gate    *gate.Service
	// UsageText 为空时让后端读额度（QuotaReader）；测试里替换掉。
	UsageText func() (string, error)
	// Hooks：任务生命周期往上调各功能的入口，app.Wire 接好。
	Hooks Hooks
}

const chatTimeout = 20 * time.Minute

// run/stop 改 state.json 前要拿和 tick 同一把锁；tick 收尾时可能等进程组退出，最多等这么久。
const CmdLockWait = 60 * time.Second

func (a *Service) LockForCommand() (*fsx.FileLock, error) {
	lock, err := fsx.AcquireLock(a.LockPath(), CmdLockWait)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, fmt.Errorf("等了 %s 还拿不到 %s，可能有 tick 卡住了", CmdLockWait, a.LockPath())
	}
	return lock, nil
}

type RunResult struct {
	Job   *state.Job
	Level int // 启动时的额度档位，cmdRun 据此提示
	Until time.Time
	Why   string
	Usage *backend.UsageInfo
}

// RunProject 是 run 本身（调用方拿着 tick.lock）：遵守上限、用量冷却和登录检查，额度 ≥usageRefuseAt 时拒绝。
func (a *Service) RunProject(st *state.State, project, keyword string) (RunResult, error) {
	name := a.Cfg.ProjectBackend(project)
	if until := st.CooldownOf(name); a.Now().Before(until) {
		return RunResult{}, fmt.Errorf("用量冷却中，到 %s 前不启动", textfmt.WhenCN(until))
	}
	dir := a.ProjectDir(project)
	if !gitx.IsGitMain(dir) {
		return RunResult{}, fmt.Errorf("%s 不是 git 主目录（projects_root=%s）", dir, a.ProjectsRoot())
	}
	if ok, why := state.Capacity(st, a.Cfg, project); !ok {
		return RunResult{}, fmt.Errorf("已满，不启动：%s", why)
	}
	p := backlog.Project{Name: project, Dir: dir}
	items, err := p.Items()
	if err != nil {
		return RunResult{}, err
	}
	if orig := a.resumableFor(st, items, project, keyword); orig != nil { // 中断了的条目（门退回、暂停时没人续跑、项目被排除）先接着做，和 tick 先续跑一样
		r, err := a.ManualGateOn(st, name)
		if err != nil {
			return r, err
		}
		orig.Manual = true
		r.Job, err = a.LaunchResume(st, orig)
		if r.Job != nil { // 续跑改了原任务的记录（ResumedBy、ResumePending），启动时只存了一次
			if err := state.Save(a.StatePath(), st); err != nil {
				a.Logf("写状态文件失败（任务 %s 已续跑，记录没存下）：%v", orig.ID, err)
			}
		}
		return r, err
	}
	it, ok := a.Pick(st, items, keyword, false)
	if !ok {
		why := "已标进行中、卡住、正在跑或依赖没做完的不算"
		if n := len(RunningItems(st, project)); n > 0 {
			why += fmt.Sprintf("；项目已有 %d 个任务在跑，同项目再起一条要它和在跑的都写了（范围：…）且不重叠（per_project=%d）", n, a.Cfg.PerProject)
		}
		if keyword != "" {
			return RunResult{}, fmt.Errorf("%s 的「待做」里没有包含「%s」、可以做的条目（%s）", p.BacklogPath(), keyword, why)
		}
		return RunResult{}, fmt.Errorf("%s 的「待做」里没有可以做的条目（%s）", p.BacklogPath(), why)
	}
	r, err := a.ManualGateOn(st, name)
	if err != nil {
		return r, err
	}
	r.Job, err = a.launch(st, p, it, true)
	if name := filepath.Base(dir); err == nil && st.HeldProject(name) { // 用户亲手启动了，之后 tick 也照常接着做
		a.Hooks.ReleaseHeld(st, name)
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("写状态文件失败（%s 的保留还记着，meta.json 已解除）：%v", name, err)
		}
	}
	return r, err
}

// resumableFor：用户点名的项目里能用同一会话接着做的中断任务。是这一条最近的一次任务、中断了、还没人续跑、条目仍标着进行中且包含 keyword、
// 后端能续跑。门退回的、暂停时等着的、exclude_projects 里不自动续跑的都算。没有就是 nil。
func (a *Service) resumableFor(st *state.State, items []backlog.Item, project, keyword string) *state.Job {
	latest := map[string]*state.Job{}
	for _, j := range st.Jobs {
		if j.Project == project && j.Kind == "" && j.Fingerprint != "" {
			if o := latest[j.Fingerprint]; o == nil || !j.StartedAt.Before(o.StartedAt) {
				latest[j.Fingerprint] = j
			}
		}
	}
	for _, it := range items {
		j := latest[it.Fingerprint]
		switch {
		case j == nil, j.Status != state.StatusInterrupted, j.ResumedBy != "", j.SessionID == "", j.SessionInRoot(), state.GateActive(j):
			continue
		case keyword != "" && !strings.Contains(it.Text, keyword):
			continue
		case !a.BackendFor(j.Backend).Caps().Resume || st.RunningFor(project, it.Fingerprint) != nil:
			continue
		}
		if _, ok := InProgressItem(items, it.Fingerprint); ok {
			return j
		}
	}
	return nil
}

// ManualGate：用户手动启动前的最后一关：登录检查，额度 ≥usageRefuseAt 时拒绝。通过时返回额度档位，调用方填上 Job。
func (a *Service) ManualGate(st *state.State) (RunResult, error) {
	return a.ManualGateOn(st, a.Cfg.Backend)
}

// ManualGateOn 同上，看这个后端。
func (a *Service) ManualGateOn(st *state.State, name string) (RunResult, error) {
	if ok, why := a.CheckLoginOn(name); !ok {
		return RunResult{}, errors.New(why)
	}
	u := a.UsageOn(st, name)
	lvl, until, why := backend.UsageLevel(u, a.Now(), a.Cfg.ForBackend(name).UsagePolicy())
	if lvl >= backend.UsageRefuse {
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("写状态文件失败（额度读数没存下）：%v", err)
		}
		return RunResult{}, errors.New(backend.UsageGateNote(lvl, until, why))
	}
	return RunResult{Level: lvl, Until: until, Why: why, Usage: u}, nil
}

// StopByID 按 id（或前缀）停任务（调用方拿着 tick.lock）。agentq stop 和 Telegram 的「停 <任务id>」都走这里，都是用户亲手停的。
func (a *Service) StopByID(st *state.State, id string) (string, error) {
	j, err := st.Find(id)
	if err != nil {
		return "", err
	}
	if j.Status != state.StatusRunning {
		return "", fmt.Errorf("任务 %s 已经%s，不用停", j.ID, state.StatusLabel(j.Status))
	}
	return a.StopJob(st, j)
}

// StopJob 是 stop 本身（调用方拿着 tick.lock，任务在跑）：杀进程组并收尾；进程已经结束的按结果收尾。
func (a *Service) StopJob(st *state.State, j *state.Job) (string, error) {
	if !a.adoptPID(j) && !a.bootChanged(j) {
		// 不知道 pid 不等于进程不在了（见 joblost.go）：按任务找，找到才杀，确认没有才收尾。stop 拿着 tick.lock，不会有启动正在进行。
		pid, err := a.FindLostClaude(j)
		if err != nil {
			return "", fmt.Errorf("任务 %s 没停成：还不知道 %s 的 pid，查进程也失败了（%v）。它还算在跑、占着名额，过一会儿再停", j.ID, a.BackendFor(j.Backend).Name(), err)
		}
		if pid == 0 {
			return a.finishStopped(st, j, "没有进程在跑")
		}
		j.PID = pid
		a.Ledger.WriteMeta(j)
	}
	switch a.JobLive(j) {
	case proc.ProcGone:
		if a.bootChanged(j) {
			a.FinishRebooted(st, j)
		} else {
			a.FinishFromResult(st, j)
		}
		return fmt.Sprintf("任务 %s 的进程已经结束，已按结果收尾：%s", j.ID, state.StatusLabel(j.Status)), state.Save(a.StatePath(), st)
	case proc.ProcUnsure: // 杀错了是别人的进程：不杀，这个任务也不再占着名额
		msg := fmt.Sprintf("pid %d 还在但认不出是不是这个任务的，没杀；要确认就看 ps，确实是就自己 kill", j.PID)
		a.Finish(st, j, state.StatusKilled, "被 agentq stop 停止："+msg)
		return fmt.Sprintf("任务 %s 已收尾，%s", j.ID, msg), state.Save(a.StatePath(), st)
	}
	hard := proc.KillGroup(j.PID, proc.KillGrace)
	how := "SIGTERM"
	if hard {
		how = "SIGTERM 后 SIGKILL"
	}
	return a.finishStopped(st, j, how)
}

// finishStopped：进程已经没了，按用户停掉收尾。how 写进摘要和回复。
func (a *Service) finishStopped(st *state.State, j *state.Job, how string) (string, error) {
	if j.Kind == state.KindNew { // 已经杀干净了才列：用户不要它接着做，留下的目录 tick 不自动挑
		j.HeldProjects = a.Hooks.NewDirs(st, j)
	}
	a.Finish(st, j, state.StatusKilled, "被 agentq stop 停止（"+how+"）")
	if err := state.Save(a.StatePath(), st); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("已停止任务 %s（%s）", j.ID, how)
	if j.Kind == state.KindNew {
		msg += a.Hooks.NewDirsNote(st, j, state.StatusKilled)
	}
	if n := a.stoppedRetryNote(st, j); n != "" {
		msg += "；" + n
	}
	return msg, nil
}

// Hooks 是任务生命周期往上调各功能模块的入口：功能模块在它上层，编译时不能依赖，由组装层接好。
// 不是按 Kind 查表的多态：哪种任务调哪个、先调谁，判断还写在调用的地方（start、finishFromResult、finish）。
type Hooks struct {
	// 启动：各种任务自己的参数和文件。
	ChatFilesDir    func(id string) string
	FollowupRequest func(cfg config.Config, prompt, project, title, session, filesDir string) backend.Request
	DiscussRequest  func(cfg config.Config, prompt, project, session string) backend.Request
	PMRequest       func(cfg config.Config, prompt, project, session string) backend.Request
	// ConstraintsNote：这个项目学到的约束（门拦过、确认过的事），接在新任务合同末尾。没接或返回空时提示词一字不变。
	ConstraintsNote func(project string) string

	// 定结果：各种任务自己怎么算做完了。
	MissingReport  func(j *state.Job) string
	CheckNewResult func(j *state.Job, res backend.Result, last string) (string, string)

	// 收尾：结束后各种任务自己要做的事。
	NewDirs       func(st *state.State, j *state.Job) []string
	NewDirsNote   func(st *state.State, j *state.Job, status string) string
	ReleaseHeld   func(st *state.State, name string)
	RequeueThread func(j *state.Job) bool
	ReplyChat     func(j *state.Job)
	NoticeNew     func(j *state.Job)
	FinishDiscuss func(st *state.State, j *state.Job)
	FinishPM      func(st *state.State, j *state.Job)
}

// JobOnItem：任务做的是这一条。标题也比：agent 改过条目正文时指纹会变。
func JobOnItem(j *state.Job, p backlog.Project, it backlog.Item) bool {
	return state.SameItem(j, p.Name, it.Fingerprint) || (j.Kind == "" && j.Project == p.Name && j.Title == it.Title)
}

// LastTaskJob 返回这一条最近启动的待做任务。标题也比，理由同 orphanCovered。
func LastTaskJob(st *state.State, p backlog.Project, it backlog.Item) *state.Job {
	var last *state.Job
	for _, j := range st.Jobs {
		if j.Kind == "" && JobOnItem(j, p, it) && (last == nil || j.StartedAt.After(last.StartedAt)) {
			last = j
		}
	}
	return last
}

const (
	patrolAnalyzeTimeout = 30 * time.Minute
	pmTimeout            = 40 * time.Minute // 产品经理调研：读项目、搜几轮网页，回合和预算先收住，这里兜底
)

// JobTimeout：夜班、复盘用各自的超时（一跑就是几个小时），巡逻分析、Telegram 对话、产品经理调研各有固定超时，待做任务用 timeout。
func (a *Service) JobTimeout(j *state.Job) time.Duration {
	switch j.Kind {
	case state.KindDiscuss: // 靠回合上限收住，后端没有就用短超时
		if !a.BackendFor(j.Backend).Caps().MaxTurns {
			return noTurnsTimeout
		}
	}
	switch j.Kind {
	case config.KindNight:
		return a.Cfg.NightTimeout.Duration
	case config.KindReview:
		return a.Cfg.ReviewTimeout.Duration
	case state.KindPatrol:
		return patrolAnalyzeTimeout
	case state.KindChat, state.KindFollowup:
		return chatTimeout
	case state.KindDiscuss:
		return noTurnsTimeout
	case state.KindPM:
		return pmTimeout
	}
	return a.Cfg.Timeout.Duration
}

// noTurnsTimeout：后端不能限回合时，靠回合收住的短任务的超时。时间不等于回合，只防失控。
const noTurnsTimeout = 10 * time.Minute

// CheckStalls：运行中的任务会话记录超过 Config.StallAfter 没增长，就记日志、在当天汇总写一行「疑似卡死」，
// 同一次卡住只报一次（记录又长起来后清掉标记）。不杀：杀由 timeout 管，用户看到了可以自己 stop。
func (a *Service) CheckStalls(st *state.State) {
	for _, j := range st.Running() {
		idle, ok := a.IdleFor(j)
		if !ok {
			continue
		}
		stalled := idle >= a.Cfg.StallAfter.Duration
		switch {
		case stalled && !j.StallNoted:
			j.StallNoted = true
			a.Ledger.WriteMeta(j)
			msg := fmt.Sprintf("疑似卡死：任务 %s（%s「%s」）会话记录 %s 没增长；不自动杀，运行满 %s 才终止，要停就 agentq stop %s",
				j.ID, j.Project, textfmt.TruncRunes(j.Title, 40), textfmt.FormatIdle(idle), textfmt.FormatDuration(a.JobTimeout(j)), j.ID)
			a.Logf("%s", msg)
			a.Ledger.AppendNightlyLine(a.Now(), msg)
		case !stalled && j.StallNoted:
			j.StallNoted = false
			a.Ledger.WriteMeta(j)
			a.Logf("任务 %s 的会话记录又在增长了，解除疑似卡死", j.ID)
		}
	}
}

// IdleFor 返回任务多久没动静：后端说的最后活动时刻（比如会话记录的修改时间），没有或比启动时间早（续跑接着写旧会话）就从启动时算起。
// 后端判断不了（旧版本启动的任务没有会话 id）时返回 false。
func (a *Service) IdleFor(j *state.Job) (time.Duration, bool) {
	t, ok := a.BackendFor(j.Backend).LastActivity(a.Ledger.JobRef(j))
	if !ok {
		return 0, false
	}
	if t.Before(j.StartedAt) {
		t = j.StartedAt
	}
	return a.Now().Sub(t), true
}

// EnforceTimeouts 杀掉运行超过超时（待做任务 timeout，夜班、复盘各自的 NightTimeout、ReviewTimeout）的任务进程组。
func (a *Service) EnforceTimeouts(st *state.State) int {
	n := 0
	for _, j := range st.Running() {
		if j.Status != state.StatusRunning {
			continue // 同 reap
		}
		if j.PID <= 0 { // 还在启动，reap 管它
			continue
		}
		limit := a.JobTimeout(j)
		if a.Now().Sub(j.StartedAt) < limit {
			continue
		}
		switch a.JobLive(j) {
		case proc.ProcGone:
			a.FinishFromResult(st, j)
			n++
			continue
		case proc.ProcUnsure: // 超时了也不杀：认不出的可能是别人的进程。放开名额，留一句让人看
			msg := fmt.Sprintf("运行超过 %s，pid %d 还在但认不出是不是这个任务的，没杀；要确认就看 ps", textfmt.FormatDuration(limit), j.PID)
			a.Logf("任务 %s：%s", j.ID, msg)
			a.Finish(st, j, state.StatusTimeout, msg)
			n++
			continue
		}
		hard := proc.KillGroup(j.PID, proc.KillGrace)
		how := "SIGTERM"
		if hard {
			how = "SIGTERM 后 SIGKILL"
		}
		a.Logf("任务 %s 运行超过 %s，已对进程组 %d 发 %s", j.ID, textfmt.FormatDuration(limit), j.PID, how)
		a.Finish(st, j, state.StatusTimeout, "运行超过 "+textfmt.FormatDuration(limit)+"，已终止（"+how+"）")
		n++
	}
	return n
}

// ProjectSlot：还能不能在 project 起一个做 item（条目原文）的任务。先看全局和单项目名额（state.Capacity）；
// 同项目已有任务在跑时还要范围不重叠（backlog.ParallelBlock）。没有条目的传空串，当没写范围。
func ProjectSlot(st *state.State, cfg config.Config, project, item string) (bool, string) {
	if ok, why := state.Capacity(st, cfg, project); !ok {
		return false, why
	}
	if why := ScopeBlocked(st, cfg, project, item); why != "" {
		return false, why
	}
	return true, ""
}

// ScopeBlocked：名额还有、只是和同项目在跑的任务范围冲突时返回原因，否则空。续跑、接手被它挡住时，项目这次不挑新条目，免得写了范围的新条目一直插队。
func ScopeBlocked(st *state.State, cfg config.Config, project, item string) string {
	if ok, _ := state.Capacity(st, cfg, project); !ok {
		return ""
	}
	running := RunningItems(st, project)
	if why := backlog.ParallelBlock(backlog.Item{Scope: backlog.ItemScope(item)}, running); why != "" {
		return fmt.Sprintf("项目 %s 已有 %d 个任务在跑，%s", project, len(running), why)
	}
	return ""
}

// RunningItems：这个项目里占名额、在跑的任务记的条目原文。没有条目的（新项目）是空串，当没写范围。
func RunningItems(st *state.State, project string) []string {
	var out []string
	for _, j := range st.Tasks() {
		if j.Project == project {
			out = append(out, j.Item)
		}
	}
	return out
}

// InProgressItem 找「待做」里指纹为 fp、标着进行中的条目。同时标了卡住的不算：那是 agent 有意放下的，不是中断。
func InProgressItem(items []backlog.Item, fp string) (backlog.Item, bool) {
	for _, it := range items {
		if it.Fingerprint == fp && it.InProgress && !it.Stuck {
			return it, true
		}
	}
	return backlog.Item{}, false
}

// Pick 取第一条可做的条目：没标进行中/卡住、没有同指纹任务在跑、（honorRetry 时）不在 retry_after 内、包含 keyword、
// 依赖都做完了；同项目已有任务在跑时还要范围和它们都不重叠（见 backlog.ParallelBlock）。不满足的跳过，下一条照挑。
func (a *Service) Pick(st *state.State, items []backlog.Item, keyword string, honorRetry bool) (backlog.Item, bool) {
	for _, it := range items {
		if !it.Eligible() || st.RunningFor(it.Project, it.Fingerprint) != nil {
			continue
		}
		if len(it.DepsWaiting) > 0 || backlog.ParallelBlock(it, RunningItems(st, it.Project)) != "" {
			continue
		}
		if keyword != "" && !strings.Contains(it.Text, keyword) {
			continue
		}
		if honorRetry && state.RetryBlocked(st, it.Project, it.Fingerprint, a.Now(), a.Cfg.RetryAfter.Duration) {
			continue
		}
		return it, true
	}
	return backlog.Item{}, false
}

// RetryNote：这个可挑的条目最近一次结束后还在 retry_after 里时，说到几点才会再自动接、现在就做用哪条命令；不用等返回空。
func (a *Service) RetryNote(st *state.State, it backlog.Item) string {
	if !it.Eligible() || st.RunningFor(it.Project, it.Fingerprint) != nil {
		return ""
	}
	at, last := state.RetryAt(st, it.Project, it.Fingerprint, a.Cfg.RetryAfter.Duration)
	if last == nil || !a.Now().Before(at) {
		return ""
	}
	run := "agentq run " + it.Project
	if kw := it.RunKeyword(); kw != "" {
		run += ` "` + kw + `"`
	}
	return fmt.Sprintf("「%s」上次%s（%s），%s 后才会再自动接；现在就做：%s",
		textfmt.TruncRunes(it.Title, 30), state.StatusLabel(last.Status), textfmt.WhenCN(*last.EndedAt), textfmt.WhenCN(at), run)
}

// stoppedRetryNote：刚停掉的待做任务，它的条目还在「待做」里可挑时的 RetryNote。
func (a *Service) stoppedRetryNote(st *state.State, j *state.Job) string {
	if j.Kind != "" || j.Fingerprint == "" || j.ProjectDir == "" {
		return ""
	}
	items, err := backlog.Project{Name: j.Project, Dir: j.ProjectDir}.Items()
	if err != nil {
		return ""
	}
	for _, it := range items {
		if it.Fingerprint == j.Fingerprint {
			return a.RetryNote(st, it)
		}
	}
	return ""
}

// CheckLogin 是启动前的登录检查：默认后端（配置写错了也在这里挡）。
func (a *Service) CheckLogin() (bool, string) { return a.checkAuth(a.DefaultBackend()) }

// PermissionBlocked：这个后端要权限配置却没有，这次不起任务（不起进程，不计连环失败）。同样的原因日志只记一次，配好了清掉记录，status 和 doctor 一直说。
func (a *Service) PermissionBlocked(name string) bool {
	if name == "" {
		name = backend.Default
	}
	path := filepath.Join(a.DataDir(), "backend-permission", name)
	why := a.Cfg.PermissionProblem(name)
	if why == "" {
		os.Remove(path)
		return false
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != why {
		a.Logf("%s", why)
		if err := fsx.WriteFileAtomic(path, []byte(why)); err != nil {
			a.Logf("记不下这条提醒（下次 tick 还会说）：%v", err)
		}
	}
	return true
}

// CheckLoginOn 是这个后端的登录检查，后端名空表示 Claude（和 Job.Backend 一样）。接回别的后端的任务时用。
func (a *Service) CheckLoginOn(name string) (bool, string) { return a.checkAuth(a.BackendFor(name)) }

// CheckReady：默认后端做得到 m，登录也没问题。做不到的说明为什么这个任务关着。
func (a *Service) CheckReady(m backend.Must) (bool, string) {
	if why := a.Lacks(m); why != "" {
		return false, why
	}
	return a.CheckLogin()
}

// Lacks：默认后端做不到 m 时返回原因，做得到返回空。
func (a *Service) Lacks(m backend.Must) string { return a.lacksOn(a.DefaultBackend(), m) }

// LacksOn：这个后端做不到 m 时返回原因，后端名空表示 Claude（和 Job.Backend 一样）。任务要跑在原任务的后端上时用它，不看默认后端。
func (a *Service) LacksOn(name string, m backend.Must) string {
	return a.lacksOn(a.BackendFor(name), m)
}

func (a *Service) lacksOn(be backend.Backend, m backend.Must) string {
	if !backend.Known(be.Name()) {
		return "" // 未知后端由 CheckLogin 说
	}
	if why := be.Caps().Lacks(m); len(why) > 0 {
		return "后端 " + be.Name() + " " + strings.Join(why, "、") + "，这个任务不启动"
	}
	return ""
}

// defaultBackendName：默认后端记成空，和 Job.Backend 的写法一样。
func (a *Service) defaultBackendName() string {
	if a.Cfg.Backend == backend.Default {
		return ""
	}
	return a.Cfg.Backend
}

// checkAuth 是启动前的登录检查。后端查不了时照常启动（AuthUnknown），除非它刚因为认证失败被暂停过（PauseAuth）。
func (a *Service) checkAuth(be backend.Backend) (bool, string) {
	auth := be.Auth(context.Background())
	switch {
	case auth.Blocked():
		return false, auth.Detail
	case auth.State == backend.AuthIn && !auth.Unverified: // 查得到说已登录，暂停记录就过时了
		os.Remove(a.AuthPausePath(be.Name()))
		return true, ""
	}
	if until, ok := a.AuthPausedUntil(be.Name()); ok {
		return false, fmt.Sprintf("后端 %s 上次认证失败，到 %s 前不启动；重新登录后删掉 %s 可以提前恢复", be.Name(), textfmt.WhenCN(until), a.Tilde(a.AuthPausePath(be.Name())))
	}
	return true, ""
}

// ReportStarted：手动启动后告诉用户启动了什么、当前有什么限制。cmd 是命令名（run、new）。
func (a *Service) ReportStarted(st *state.State, r RunResult, cmd string) {
	j, lvl, until, why, u := r.Job, r.Level, r.Until, r.Why, r.Usage
	if j.ResumeOf != "" {
		fmt.Fprintf(a.Out, "已接着做中断的任务 %s（新任务 %s，同一会话）：%s「%s」pid %d\n", j.ResumeOf, j.ID, j.Project, j.Title, j.PID)
	} else {
		fmt.Fprintf(a.Out, "已启动任务 %s：%s「%s」pid %d\n", j.ID, j.Project, j.Title, j.PID)
	}
	switch lvl {
	case backend.UsageStop:
		fmt.Fprintf(a.Out, "（%s）\n", backend.UsageGateNote(lvl, until, why))
	case backend.UsageUnknown:
		fmt.Fprintf(a.Out, "（额度未知，照常启动：%s）\n", textfmt.OneLine(u.Err, 120))
	}
	if a.Cfg.Paused {
		more := ""
		if j.Manual {
			more = "，门退回或中断了 tick 也会接着做这一条"
		}
		fmt.Fprintf(a.Out, "（当前已暂停自动接活，%s 不受影响；收尾、过门仍由 tick 做%s）\n", cmd, more)
	}
	if now := a.Now(); now.Before(st.BreakerUntil) {
		fmt.Fprintf(a.Out, "（连环失败熔断中，到 %s 前 tick 不自动接活，%s 不受影响）\n", textfmt.WhenCN(st.BreakerUntil), cmd)
	}
	fmt.Fprintf(a.Out, "看进度：agentq status；看结果：agentq logs %s\n", j.ID)
}

// RoutineBlocked：夜班、复盘启动前的检查（用量冷却、projects_root、登录、订阅额度 ≥limit），不能启动时返回原因。
// 读到的额度存进 st，调用方负责保存状态。
func (a *Service) RoutineBlocked(st *state.State, limit int) string {
	if until := st.CooldownOf(a.Cfg.Backend); a.Now().Before(until) {
		return "用量冷却中，到 " + textfmt.WhenCN(until)
	}
	dir := a.ProjectsRoot()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Sprintf("projects_root %s 不是目录", dir)
	}
	return a.BackendBlocked(st, a.Cfg.Backend, limit)
}

// BackendBlocked：这个后端现在不能起后台活的原因（用量冷却、登录、订阅额度 ≥limit），能起返回空。后端名空表示 Claude。
// 读到的额度存进 st，调用方负责保存状态。
func (a *Service) BackendBlocked(st *state.State, name string, limit int) string {
	if until := st.CooldownOf(name); a.Now().Before(until) {
		return "用量冷却中，到 " + textfmt.WhenCN(until)
	}
	if ok, why := a.CheckLoginOn(name); !ok {
		return why
	}
	if lvl, until, why := backend.UsageLevel(a.UsageOn(st, name), a.Now(), a.Cfg.ForBackend(name).UsagePolicy()); lvl >= limit {
		if err := state.Save(a.StatePath(), st); err != nil {
			a.Logf("写状态文件失败（额度读数没存下）：%v", err)
		}
		return backend.UsageGateNote(lvl, until, why)
	}
	return ""
}

// SchedNote：status「调度：」后面的那句（Telegram「状态」的回答也用）。配置决定了接不了活的项目一直要说，暂停、冷却盖不住它。
func (a *Service) SchedNote(st *state.State, stErr error, now time.Time, ulvl int, uuntil time.Time, uwhy string) string {
	note := a.schedNote(st, stErr, now, ulvl, uuntil, uwhy)
	problems := append(backend.ConfigProblems(a.Cfg.Backend, a.Cfg.ProjectBackends), a.Cfg.PermissionProblems()...)
	if stErr == nil && st != nil {
		problems = append(problems, st.OtherLimitNotes(a.Cfg, now)...)
	}
	if len(problems) == 0 {
		return note
	}
	if note == "正常" {
		return strings.Join(problems, "；")
	}
	return note + "；" + strings.Join(problems, "；")
}

func (a *Service) schedNote(st *state.State, stErr error, now time.Time, ulvl int, uuntil time.Time, uwhy string) string {
	switch {
	case stErr == nil && !backend.Known(a.Cfg.Backend):
		return backend.Unsupported(a.Cfg.Backend) + "，不接活；改 config.json 的 backend，agentq doctor 看详情"
	case stErr != nil:
		if errors.Is(stErr, state.ErrStateCorrupt) {
			return fmt.Sprintf("状态文件损坏，下次 tick 会备份它、按各任务的 meta.json 重建并收尾（那次不启动新任务）：%v", stErr)
		}
		return fmt.Sprintf("状态文件读不出，tick 不会启动新任务：%v", stErr)
	case a.Cfg.Paused:
		return "已暂停（paused=true），tick 只收尾不启动；agentq resume 恢复"
	case now.Before(st.CooldownOf(a.Cfg.Backend)):
		when := textfmt.WhenCN(st.CooldownOf(a.Cfg.Backend))
		if len(a.Cfg.BackendsInUse()) > 1 { // 混用时说清楚是谁：别的后端的项目还在接活
			return "后端 " + a.DefaultBackend().Name() + " 用量冷却中，到 " + when
		}
		return "用量冷却中，到 " + when
	case ulvl >= backend.UsageStop:
		return backend.UsageGateNote(ulvl, uuntil, uwhy)
	case now.Before(st.BreakerUntil):
		return fmt.Sprintf("连环失败熔断中，到 %s（原因见当天汇总 %s）", textfmt.WhenCN(st.BreakerUntil), a.NightlyPath(now))
	}
	return "正常"
}
