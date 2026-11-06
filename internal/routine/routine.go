// Package routine 是定点的夜班和复盘：launchd 定点叫醒或开机后补跑，确认没事就写占位报告不起模型。
package routine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/inbox"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/runner"
	"agentq/internal/sense"
	"agentq/internal/service"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是夜班、复盘的模块实例。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Gitx    *gitx.Service
	Inbox   *inbox.Service
	Ledger  *ledger.Service
	Notify  *notify.Service
	Runner  *runner.Service
	Sense   *sense.Service
	Service *service.Service
	// RoutineText 是夜班、复盘的说明文件全文（kind → 文本）。embed 在根目录，由 app.Wire 注入。
	RoutineText map[string]string
}

// MissingReport：正常退出的夜班/复盘没写出说明要求的报告（启动那天或今天的都算）时返回报告路径，
// 多半是中途退出了（LESSONS：-p 在等通知时提前退出却记成跑完）；写了就返回空。
// 只算这次任务写的：跳过时留的占位报告不算，修改时间早于任务启动的也不算。否则这次没跑完却记成跑完，下次又被当成没事跳过。
func (a *Service) MissingReport(j *state.Job) string {
	r := config.Routines[j.Kind]
	path := func(t time.Time) string {
		return filepath.Join(a.ReportDir(), t.Format("2006-01-02")+r.Report+".md")
	}
	for _, t := range []time.Time{j.StartedAt, a.Now()} {
		if reportWrittenSince(path(t), j.StartedAt) {
			return ""
		}
	}
	return path(a.Now())
}

// reportWrittenSince：报告存在、不是占位报告、修改时间不早于 since（文件系统可能只精确到秒，since 按秒取整）。
func reportWrittenSince(path string, since time.Time) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.ModTime().Before(since.Truncate(time.Second)) {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && !isIdleReport(string(b))
}

func routineJob(r config.Routine, dir, note string) *state.Job {
	return &state.Job{Kind: r.Kind, Project: r.Name, ProjectDir: dir, Item: r.File, Title: "agentq " + r.Kind + note}
}

// RoutinePromptPrefix：说明文件是照搬桌面端定时任务的，那边回合结束不会退出；claude -p 回合一结束进程就退出，
// 等夜班结束、等任务跑完都得在前台等（实测 -p 下前台 Bash 单独 sleep 很久会被拦，until 循环可以；Monitor 已禁用）。
const RoutinePromptPrefix = "你由 agentq 在无人值守模式下用 claude -p 启动：回合一结束进程就退出，后台命令、子代理通知都会被丢下。" +
	"一切等待都在前台做完：用前台 Bash 的 until 循环等条件（如 `until <条件>; do sleep 30; done`，单次 Bash 最长 10 分钟，到点再开一次），" +
	"不用后台命令、Monitor 或「等通知」。照下面的说明做完再结束，最后一段用一两句话写做了什么、结果写在哪。\n\n"

func (a *Service) routinePrompt(r config.Routine) string {
	return RoutinePromptPrefix + a.RoutineText[r.Kind]
}

// CmdRoutine 启动夜班或复盘。scheduled 表示是 launchd 定点叫醒的：开关没打开时只记一行日志就退出；
// 用户手动执行不看开关。定点叫醒时先过 routineIdle：确认没事就不起 claude，写一份占位报告（见 internal/routine/routine.go）；手动执行不看。
// 启动前不看暂停（夜班不改暂停开关）和连环失败熔断（夜班、复盘不是待做；它们秒退照样计数），
// 仍遵守用量冷却和登录检查；订阅额度 ≥usageStopAt 时定点叫醒不启动，手动执行到 ≥usageRefuseAt 才拒绝（同 tick 和 run）。
// 同一种已经在跑就不重复启动。定点叫醒没能启动时在当天汇总记一行，早报能看到。
func (a *Service) CmdRoutine(kind string, scheduled bool) error {
	r := config.Routines[kind]
	if scheduled && kind == config.KindReview {
		a.Sense.BeforeReview() // 主动感知接在复盘前面；关着时什么都不做，出事也不影响复盘
	}
	if scheduled && !r.Enabled(a.Cfg) {
		a.Logf("%s：%s=false，%s 定点叫醒但不执行（切换步骤见 README「夜班和复盘」）", r.Name, r.Key, a.Service.Svc().Words().Name)
		return nil
	}
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf("%s没有启动：%s", r.Name, fmt.Sprintf(format, args...))
		a.Logf("%s", msg)
		if scheduled {
			a.Ledger.AppendNightlyLine(a.Now(), msg)
		}
		return env.ErrLogged
	}
	lock, err := a.Runner.LockForCommand()
	if err != nil {
		return fail("%v", err)
	}
	defer lock.Close()
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fail("状态文件读不出，当作未知（下次 tick 会处理）：%v", err)
	}
	a.Runner.Reap(st)
	a.Runner.EnforceTimeouts(st)
	if err := state.Save(a.StatePath(), st); err != nil {
		return fail("写状态文件失败：%v", err)
	}
	for _, j := range st.Running() {
		if j.Kind == kind {
			a.Logf("%s已经在跑（任务 %s，%s 启动），不重复启动", r.Name, j.ID, textfmt.WhenCN(j.StartedAt))
			return nil
		}
	}
	if scheduled && a.routineIdle(st, r) {
		return nil
	}
	limit := backend.UsageRefuse
	if scheduled {
		limit = backend.UsageStop
	}
	if why := a.Runner.RoutineBlocked(st, limit); why != "" {
		return fail("%s", why)
	}
	mark, markErr := a.routineSnapshotNow(kind)
	j, err := a.Runner.Start(st, routineJob(r, a.ProjectsRoot(), ""), a.routinePrompt(r), "")
	if err != nil {
		if j != nil { // 已经记成失败的任务、写过汇总
			a.Logf("%s没有启动：%v", r.Name, err)
			return env.ErrLogged
		}
		return fail("%v", err)
	}
	a.noteRoutineMark(st, r, j, mark, markErr)
	a.Logf("%s已启动：任务 %s，pid %d，超时 %s；看进度 agentq status，看结果 agentq logs %s",
		r.Name, j.ID, j.PID, textfmt.FormatDuration(a.Runner.JobTimeout(j)), j.ID)
	if !scheduled && !r.Enabled(a.Cfg) {
		fmt.Fprintf(a.Out, "（%s=false：%s %s 定点叫醒时不执行，手动执行不受影响）\n", r.Key, a.Service.Svc().Words().Name, r.At())
	}
	return nil
}

// catchUpGrace：定点时刻过后这么久还没有当天的记录才算错过——launchd 定点叫醒的那次要先查登录、读额度，过一会儿才记进状态。
const catchUpGrace = 15 * time.Minute

// catchUpBefore：错过的夜班只在这个钟点前补跑（再晚用户已经起床，夜班合并、上传的活不该挤到白天）；复盘当天任何时候都补。
var catchUpBefore = map[string]int{config.KindNight: 8, config.KindReview: 24}

// RoutineMissed：打开了的夜班或复盘今天的定点时刻已过、今天既没有这一种的任务记录（不论结果、手动还是补跑），
// 也没有它要写的报告（可能是别处跑的），且还在补跑窗口内时，返回 true 和今天的报告路径。
func (a *Service) RoutineMissed(st *state.State, r config.Routine, now time.Time) (bool, string) {
	if !r.Enabled(a.Cfg) {
		return false, ""
	}
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	at := time.Date(y, m, d, r.Hour, r.Minute, 0, 0, now.Location())
	if now.Before(at.Add(catchUpGrace)) || now.Hour() >= catchUpBefore[r.Kind] {
		return false, ""
	}
	for _, j := range st.Jobs {
		if j.Kind == r.Kind && !j.StartedAt.Before(day) {
			return false, ""
		}
	}
	report := filepath.Join(a.ReportDir(), day.Format("2006-01-02")+r.Report+".md")
	if _, err := os.Stat(report); err == nil {
		return false, ""
	}
	return true, report
}

// CatchUpRoutines：tick 补跑今天错过的夜班、复盘。launchd 的定点任务在关机、断电或还没登录时错过就不会再叫醒，
// 开机自动登录后由 tick 发现并补一次；补跑的任务今天有了记录，同一天不会再补。和定点叫醒一样不看暂停和熔断，
// 遵守用量冷却、登录和额度（≥usageStopAt 不启动），没能启动只记日志、下次 tick 再试。
// 复盘要汇总夜班报告，夜班还在跑时先不补复盘。
func (a *Service) CatchUpRoutines(st *state.State) {
	now := a.Now()
	for _, kind := range []string{config.KindNight, config.KindReview} {
		r := config.Routines[kind]
		missed, report := a.RoutineMissed(st, r, now)
		if !missed || state.HasRunning(st, kind) { // 前一天开始、过了 0 点还在跑的不算今天的记录，也不能再起一个
			continue
		}
		if a.Runner.PermissionBlocked(a.Cfg.Backend) { // 配好了再补，不然每次 tick 都重判一遍闲时、重记一遍原因
			return
		}
		if kind == config.KindReview && a.Cfg.SenseEnabled {
			a.Sense.SenseCatchUp = true // 定点时刻接在复盘前面的主动感知也错过了
		}
		if kind == config.KindReview && state.HasRunning(st, config.KindNight) {
			a.Logf("%s：今天 %s 那次没跑，夜班还在跑，等它跑完再补", r.Name, r.At())
			continue
		}
		if a.routineIdle(st, r) { // 写了占位报告，今天不会再算错过
			continue
		}
		if why := a.Runner.RoutineBlocked(st, backend.UsageStop); why != "" {
			a.Logf("%s：今天 %s 那次没跑，开机后补跑没有启动：%s；下次 tick 再试", r.Name, r.At(), why)
			continue
		}
		prompt := a.routinePrompt(r)
		if kind == config.KindNight {
			prompt = RoutinePromptPrefix + CatchUpNightNote + a.RoutineText[r.Kind]
		}
		mark, markErr := a.routineSnapshotNow(kind)
		j, err := a.Runner.Start(st, routineJob(r, a.ProjectsRoot(), "（开机后补跑）"), prompt, "")
		if err != nil {
			a.Logf("%s：开机后补跑没有启动：%v", r.Name, err) // 已记成失败的任务时，今天有了记录，不再补
			continue
		}
		a.noteRoutineMark(st, r, j, mark, markErr)
		msg := fmt.Sprintf("%s开机后补跑：今天 %s 那次没跑（没有当天的任务记录，也没有 %s），任务 %s",
			r.Name, r.At(), filepath.Base(report), j.ID)
		a.Logf("%s，pid %d", msg, j.PID)
		a.Ledger.AppendNightlyLine(now, msg)
	}
}

// CatchUpNightNote：补跑时早上已经不远了，写明收工时刻，免得一直占到超时。
const CatchUpNightNote = "这次是开机后补跑（关机或断电错过了 01:00 那次）。到今天 08:00 就收工写报告，没做完的留给下次。\n\n"

const (
	routineProbeWait = 45 * time.Second // 一次判断里 git、gh 一共最多多久（调用方拿着 tick.lock，run、stop 最多等 60 秒）
	ghCallWait       = 15 * time.Second
	lessonsFile      = "LESSONS.md"
	ghNoIssue        = "none"
	// idleReportMark：占位报告的最后一行。任务写的报告会盖掉它，有它就不是任务写的（见 missingReport）。
	idleReportMark = "<!-- agentq 占位报告，没起模型 -->"
)

// projectHeads 读各项目 HEAD。有一个读不出（没有提交、仓库坏了）就算查不出。
func (a *Service) projectHeads(ctx context.Context) (map[string]string, error) {
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return nil, fmt.Errorf("扫描项目目录：%w", err)
	}
	heads := map[string]string{}
	for _, p := range projects {
		h, err := a.Gitx.RoutineGit(ctx, p.Dir, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil || h == "" {
			return nil, fmt.Errorf("读 %s 的 HEAD：%w", p.Name, err)
		}
		heads[p.Name] = h
	}
	return heads, nil
}

// ghLatestIssue 查仓库最新的 issue 更新时间（只读）。gh 不存在、报错、超时、输出看不懂都返回错误，调用方算查不出。
func (a *Service) ghLatestIssue(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghCallWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "list", "--repo", repo, "--state", "all", "--limit", "1",
		"--search", "sort:updated-desc", "--json", "updatedAt")
	cmd.Env = append(gitx.GitEnv(a.Home), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	proc.BoundCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh issue list --repo %s：%w", repo, err)
	}
	var list []struct {
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("gh issue list --repo %s 的输出看不懂：%w", repo, err)
	}
	if len(list) == 0 {
		return ghNoIssue, nil
	}
	if list[0].UpdatedAt == "" {
		return "", fmt.Errorf("gh issue list --repo %s 的输出没有 updatedAt", repo)
	}
	return list[0].UpdatedAt, nil
}

// projectIssues 查授权项目（夜班提示词的范围：有 github 远程的主 Git 仓库）的最新 issue。没有 github 远程的项目没有要查的。
func (a *Service) projectIssues(ctx context.Context) (map[string]string, error) {
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return nil, fmt.Errorf("扫描项目目录：%w", err)
	}
	issues := map[string]string{}
	for _, p := range projects {
		remotes, err := a.Gitx.RoutineGit(ctx, p.Dir, "remote", "-v")
		if err != nil {
			return nil, fmt.Errorf("读 %s 的远程：%w", p.Name, err)
		}
		for _, repo := range backlog.GithubRepos(remotes) {
			if _, ok := issues[repo]; ok {
				continue
			}
			t, err := a.ghLatestIssue(ctx, repo)
			if err != nil {
				return nil, err
			}
			issues[repo] = t
		}
	}
	return issues, nil
}

// routineSnapshot 看现状，作为这一次的记录。查不出返回错误：不留记录，下次照跑。
func (a *Service) routineSnapshot(ctx context.Context, kind string) (*state.RoutineMark, error) {
	heads, err := a.projectHeads(ctx)
	if err != nil {
		return nil, err
	}
	m := &state.RoutineMark{At: a.Now(), Heads: heads}
	if m.Issues, err = a.projectIssues(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (a *Service) routineSnapshotNow(kind string) (*state.RoutineMark, error) {
	ctx, cancel := context.WithTimeout(context.Background(), routineProbeWait)
	defer cancel()
	return a.routineSnapshot(ctx, kind)
}

// noteRoutineMark：任务启动成功后存下记录。查不出现状时去掉旧记录，下次照跑。
func (a *Service) noteRoutineMark(st *state.State, r config.Routine, j *state.Job, mark *state.RoutineMark, markErr error) {
	if markErr != nil || mark == nil {
		a.Logf("%s：这次没记下现状，下次定点叫醒照跑：%v", r.Name, markErr)
		delete(st.RoutineMarks, r.Kind)
	} else {
		if st.RoutineMarks == nil {
			st.RoutineMarks = map[string]*state.RoutineMark{}
		}
		mark.JobID = j.ID
		st.RoutineMarks[r.Kind] = mark
	}
	if err := state.Save(a.StatePath(), st); err != nil {
		a.Logf("%s：写状态文件失败（现状记录没存下，下次定点叫醒照跑）：%v", r.Name, err)
	}
}

// headsChanged 和上次的记录比，返回变了的说明；没变（或变的只有 LESSONS.md）返回空。
func (a *Service) headsChanged(ctx context.Context, old, cur map[string]string) string {
	for name := range old {
		if _, ok := cur[name]; !ok {
			return "项目 " + name + " 不在了"
		}
	}
	for name, head := range cur {
		prev, ok := old[name]
		if !ok {
			return "多了项目 " + name
		}
		if prev == head {
			continue
		}
		dir := a.ProjectDir(name)
		// 不用 --name-only 的两点 diff：改了又改回来时净差是空的，但确实有人动过。逐个提交看，重命名不合并
		files, err := a.Gitx.RoutineGit(ctx, dir, "log", "--format=", "--name-only", "--no-renames", prev+".."+head)
		if err != nil || files == "" { // 旧提交没了、回退了、空提交：说不清，算变了
			return "项目 " + name + " 的 HEAD 变了"
		}
		for _, f := range strings.Fields(files) {
			if f != lessonsFile {
				return "项目 " + name + " 的 HEAD 变了"
			}
		}
	}
	return ""
}

// routineBusy 返回这次要照跑的原因；空表示确认没事。ctx 管 git 和 gh 的总时长，超时算查不出。
func (a *Service) routineBusy(ctx context.Context, st *state.State, r config.Routine) string {
	now := a.Now()
	mark := st.RoutineMarks[r.Kind]
	if mark == nil {
		return "没有上次的记录"
	}
	if now.Before(mark.At) {
		return "时钟比上次" + r.Name + "早，说不清"
	}
	var last *state.Job
	for _, j := range st.Jobs {
		if j.Kind == r.Kind && (last == nil || j.StartedAt.After(last.StartedAt)) {
			last = j
		}
	}
	switch {
	case last == nil || last.ID != mark.JobID:
		return "上次" + r.Name + "的任务记录对不上"
	case last.Status != state.StatusDone || last.LimitHit:
		return "上次" + r.Name + "没跑完（" + state.StatusLabel(last.Status) + "）"
	}
	if len(st.Running()) > 0 {
		return "有任务在跑"
	}
	if why := a.RoutineKindBusy(st, r, mark); why != "" {
		return why
	}
	heads, err := a.projectHeads(ctx)
	if err != nil {
		return "查不出：" + err.Error()
	}
	if why := a.headsChanged(ctx, mark.Heads, heads); why != "" {
		return why
	}
	issues, err := a.projectIssues(ctx)
	if err != nil {
		return "查不出：" + err.Error()
	}
	if !maps.Equal(issues, mark.Issues) {
		return "有新 issue"
	}
	return ""
}

// RoutineKindBusy：不用 git、gh 就能判断的那部分。复盘：上次复盘之后有任务结束。夜班：待做、等用户决定、任务箱有条目，或有任务等着续跑。
func (a *Service) RoutineKindBusy(st *state.State, r config.Routine, mark *state.RoutineMark) string {
	if r.Kind == config.KindReview {
		for _, j := range st.Jobs {
			// 闲时找活、主动感知不算：写了东西 HEAD 就变了（复盘照跑），没写就没什么可复盘的；算的话它们每天都跑，复盘就永远跳不过
			if j.Kind != config.KindReview && j.Kind != state.KindSuggest && j.Kind != state.KindSense && j.Kind != state.KindDiscuss && j.EndedAt != nil && j.EndedAt.After(mark.At) {
				return "上次复盘之后有任务结束（" + j.ID + "）"
			}
		}
		return ""
	}
	if len(st.PendingResumes()) > 0 {
		return "有任务等着续跑"
	}
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		return "查不出：扫描项目目录：" + err.Error()
	}
	for _, p := range projects {
		items, err := p.Items()
		if err != nil {
			return "查不出：" + p.Name + " 的 BACKLOG：" + err.Error()
		}
		if len(items) > 0 {
			return p.Name + " 的「待做」有条目"
		}
	}
	items, err := a.Inbox.ReadInboxItems()
	if err != nil {
		return "查不出：任务箱：" + err.Error()
	}
	if len(items) > 0 {
		return "任务箱有条目"
	}
	decs, fails := a.Notify.Decisions()
	if len(fails) > 0 {
		return "查不出：" + fails[0]
	}
	if len(decs) > 0 {
		return decs[0].Project + " 的「等用户决定」有条目"
	}
	return ""
}

// idleReportLine：占位报告的第一行，早报摘要会带上（reportFirstLine）。
func idleReportLine(r config.Routine) string {
	if r.Kind == config.KindReview {
		return "今天没有要复盘的：上次复盘之后没有任务结束，各项目 HEAD 没变（只改 LESSONS.md 的提交不算），没有新 issue，没起模型。"
	}
	return "今晚没有夜班要做的：待做、等用户决定都是空的，各项目 HEAD 没变（只改 LESSONS.md 的提交不算），没有新 issue，没起模型。"
}

// WriteIdleReport 写占位报告。今天已经有报告（手动跑的、别处跑的）就不动它。
func (a *Service) WriteIdleReport(r config.Routine) error {
	path := filepath.Join(a.ReportDir(), a.Now().Format("2006-01-02")+r.Report+".md")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsx.WriteFileAtomic(path, []byte(idleReportLine(r)+"\n\n要跑一次就在终端执行 agentq "+r.Kind+"。\n\n"+idleReportMark+"\n"))
}

// isIdleReport：最后一个非空行是占位标记。任务写报告时整个盖掉，或者接在后面写，标记都不在最后了。
func isIdleReport(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1]) == idleReportMark
}

// routineIdle：确认没事时写好占位报告、记一行日志，返回真（调用方不起 claude）。其他情况（包括占位报告写不了）返回假，照跑。
func (a *Service) routineIdle(st *state.State, r config.Routine) bool {
	ctx, cancel := context.WithTimeout(context.Background(), routineProbeWait)
	defer cancel()
	why := a.routineBusy(ctx, st, r)
	if why == "" {
		err := a.WriteIdleReport(r)
		if err == nil {
			a.Logf("%s：确认没有新东西，跳过，不起 %s（手动 agentq %s 不受影响）", r.Name, a.DefaultBackend().Name(), r.Kind)
			return true
		}
		why = "占位报告写不了：" + err.Error()
	}
	a.Logf("%s：照跑：%s", r.Name, why)
	return false
}
