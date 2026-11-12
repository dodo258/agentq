package patrol

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// backlogDirtyAfter：BACKLOG 有没提交的改动超过这么久才报。短的多半是用户或任务正在改。
const backlogDirtyAfter = time.Hour

// CheckDirtyBacklogs：BACKLOG 有没提交的改动时，commitBacklog 一直拒绝，agentq 写不进这个项目的 BACKLOG（去标记、标卡住、拍板、过门收尾）。
// 可能是用户的改动，也可能是进程写完文件、提交前被杀了，分不清，不自动提交，报出来让用户定。别的项目不受影响：拒绝是当场返回的。
func (a *Service) CheckDirtyBacklogs(now time.Time, fail func(prefix, note string)) []PatrolIssue {
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		fail("backlog-dirty:", "扫描项目目录失败："+err.Error())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // 巡逻时拿着 tick.lock：git 挂住也不能一直等
	defer cancel()
	var out []PatrolIssue
	for _, p := range projects {
		rel, err := backlog.BacklogRepoPath(p)
		if err != nil {
			continue // 没有 BACKLOG，或指向仓库外：commitBacklog 那边会说
		}
		st, err := a.Gitx.RoutineGit(ctx, p.Dir, "status", "--porcelain", "--", rel)
		if err != nil {
			if ctx.Err() != nil {
				fail("backlog-dirty:", "查 BACKLOG 有没有没提交的改动超时了")
				return out
			}
			continue // 仓库坏了：不是这一条要报的
		}
		if st == "" {
			continue
		}
		fi, err := os.Stat(p.BacklogPath())
		if err != nil {
			continue
		}
		age := now.Sub(fi.ModTime())
		if age < backlogDirtyAfter {
			continue
		}
		out = append(out, PatrolIssue{Fingerprint: "backlog-dirty:" + p.Name, Check: "backlog",
			Title:    fmt.Sprintf("%s 的 BACKLOG 有没提交的改动，已经 %s", p.Name, textfmt.FormatAge(age)),
			Evidence: []string{"文件 " + p.BacklogPath() + " 最后修改于 " + textfmt.WhenCN(fi.ModTime()), "git status：" + textfmt.OneLine(st, 100)},
			Action: "agentq 不自动提交（可能是你的改动），这期间写不进这个项目的 BACKLOG（去标记、标卡住、拍板、过门收尾），别的项目不受影响。" +
				"在 " + p.Dir + " 看 git diff -- " + rel + "：要留就提交，不要就 git checkout -- " + rel})
	}
	return out
}

// checkLaunchd：launchctl list 里前缀是 launchdPrefix 的服务，上次退出码不是 0 的报出来，证据带它日志的末尾；
// agentq 装着时，开关打开的夜班、复盘定点服务不在列表里也报。
func (a *Service) checkLaunchd(r *PatrolReport, fail func(prefix, note string)) []PatrolIssue {
	m := a.Service.Svc()
	w := m.Words()
	states, err := m.List()
	if err != nil {
		fail("launchd:", "读不了 "+w.Name+" 服务列表（"+w.List+"）："+textfmt.OneLine(err.Error(), 200))
		return nil
	}
	var res []PatrolIssue
	loaded := map[string]bool{}
	r.launchd = loaded
	listenPID := ""
	for _, s := range states {
		if !strings.HasPrefix(s.Label, a.Service.LaunchdPrefix()) {
			continue
		}
		pid, code, label := s.Pid, s.Code, s.Label
		loaded[label] = true
		if label == a.Service.ListenLabel() {
			listenPID = pid
		}
		if code == "0" || code == "-" {
			continue
		}
		ev := w.List + "：" + label + " 上次退出码 " + code
		if n, err := strconv.Atoi(code); err == nil && n < 0 {
			ev += fmt.Sprintf("（被信号 %d 终止）", -n)
		}
		if pid != "-" {
			ev += "，现在在跑（pid " + pid + "）"
		}
		evs := []string{ev}
		if t := m.LogTail(label); t != "" {
			evs = append(evs, t)
		}
		res = append(res, PatrolIssue{
			Fingerprint: "launchd:" + label, Check: "launchd", Anomaly: true, // 不带退出码：从 1 变成 2 不算恢复再出问题
			Title:    w.Name + " 服务 " + label + " 上次退出码 " + code,
			Evidence: evs,
			Action:   "没自动处理：按日志查原因；修好后它下次运行退出码为 0，巡逻就记为已恢复",
		})
	}
	// Telegram 监听是常驻服务：配了 Telegram 时没装上、或装着却没在跑（KeepAlive 拉不起来），用户发消息就没人秒回
	if c, _ := a.Telegram.TelegramCreds(); c != nil && a.Cfg.NotifyEnabled && loaded[a.Service.ServiceLabel()] {
		label := a.Service.ListenLabel()
		switch {
		case !loaded[label]:
			res = append(res, PatrolIssue{
				Fingerprint: "launchd:" + label + ":missing", Check: "launchd", Anomaly: true,
				Title:    "Telegram 监听 " + label + " 没装上",
				Evidence: []string{w.List + " 里有 " + a.Service.ServiceLabel() + "，没有 " + label + "，而 Telegram 配好了；现在靠 tick 每 5 分钟兜底收消息"},
				Action:   "没自动处理：在终端执行 " + a.Service.ServiceInstallCommand() + " 装上（可重复执行）",
			})
		case listenPID == "-":
			res = append(res, PatrolIssue{
				Fingerprint: "launchd:" + label + ":down", Check: "launchd", Anomaly: true,
				Title:    "Telegram 监听 " + label + " 没在跑",
				Evidence: []string{w.List + "：" + label + " 没有 pid（" + w.KeepAlive + " 没把它拉起来）；现在靠 tick 每 5 分钟兜底收消息"},
				Action:   "没自动处理：看 ~/.agentq/launchd.log 里「监听：」开头的几行，再执行 " + a.Service.ServiceInstallCommand() + " 重新装上",
			})
		}
	}
	if loaded[a.Service.ServiceLabel()] {
		for _, kind := range []string{config.KindNight, config.KindReview} {
			r := config.Routines[kind]
			if label := a.Service.RoutineLabel(kind); r.Enabled(a.Cfg) && !loaded[label] {
				res = append(res, PatrolIssue{
					Fingerprint: "launchd:" + label + ":missing", Check: "launchd", Anomaly: true,
					Title:    r.Name + "的定点服务 " + label + " 没装上",
					Evidence: []string{w.List + " 里有 " + a.Service.ServiceLabel() + "，没有 " + label + "，而 " + r.Key + "=true"},
					Action:   "没自动处理：在终端执行 " + a.Service.ServiceInstallCommand() + " 重新装上（可重复执行）",
				})
			}
		}
	}
	return res
}

// autopush.log 的行：「2026-10-10 12:21:35 FAIL /…/agentq/.git: 报错」「2026-10-11 11:52:10 ok /…/agentq/.git」。
var autopushRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (FAIL|ok) (.+?)(?:: (.*))?$`)

// checkAutopush：配置了 autopush_log 才查。这个日志最近 patrolWindow 里有 FAIL、之后同仓库还没推成功的仓库，报最近一段连续失败
// （中间没有 ok 的一串 FAIL，指纹带这一段第一次 FAIL 的时间：成功过之后再 FAIL 是新问题）。之后又推成功（ok）的一段算已恢复，
// 不报，指纹记进 r.pushOK。
func (a *Service) checkAutopush(now time.Time, r *PatrolReport, fail func(prefix, note string)) []PatrolIssue {
	if a.Cfg.AutopushLog == "" {
		return nil
	}
	path := fsx.ExpandHome(a.Home, a.Cfg.AutopushLog)
	text, err := fsx.ReadTail(path, 256<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		fail("autopush:", "读不了 "+path+"："+err.Error())
		return nil
	}
	type repoFails struct {
		n       int
		start   time.Time // 这一段连续失败的第一次 FAIL
		last    string
		lastAt  time.Time
		okAfter time.Time
	}
	repos := map[string]*repoFails{}
	var order []string
	r.pushOK = map[string]bool{}
	fp := func(repo string, start time.Time) string {
		return "autopush:" + repo + "@" + start.Format("2006-01-02T15:04:05")
	}
	for _, line := range strings.Split(text, "\n") {
		m := autopushRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], now.Location())
		if err != nil {
			continue
		}
		rf := repos[m[3]]
		if m[2] == "FAIL" {
			if t.After(now.Add(time.Minute)) {
				continue
			}
			// 先不按 24 小时窗口筛：一段连续失败的起点要从它真正的第一次 FAIL 算，不随窗口往后移（不然指纹每小时一变、每小时重报）。
			// 一段失败长过读到的 256KB 尾巴（两千多行）时起点仍会移，可以接受。
			if rf == nil {
				rf = &repoFails{}
				repos[m[3]] = rf
				order = append(order, m[3])
			}
			if rf.n == 0 || !rf.okAfter.IsZero() { // 第一次，或成功推送过之后又失败：新的一段
				if rf.n > 0 {
					r.pushOK[fp(m[3], rf.start)] = true
				}
				rf.n, rf.start = 0, t
			}
			rf.n++
			rf.last, rf.lastAt, rf.okAfter = strings.TrimSpace(line), t, time.Time{}
		} else if rf != nil && t.After(rf.lastAt) {
			rf.okAfter = t
		}
	}
	var res []PatrolIssue
	for _, repo := range order {
		rf := repos[repo]
		if !rf.okAfter.IsZero() { // 之后推成功了：已恢复
			r.pushOK[fp(repo, rf.start)] = true
			continue
		}
		if now.Sub(rf.lastAt) > patrolWindow { // 最近一段的最后一次 FAIL 也在 24 小时前：不算新 FAIL
			continue
		}
		name := filepath.Base(repo)
		if name == ".git" {
			name = filepath.Base(filepath.Dir(repo))
		}
		res = append(res, PatrolIssue{
			Fingerprint: fp(repo, rf.start), Check: "autopush", Anomaly: true,
			Title:    "自动备份推送失败：" + name,
			Evidence: []string{fmt.Sprintf("%s 里 %s 起连续 %d 次 FAIL，之后还没推成功，最后一次：%s", path, textfmt.WhenCN(rf.start), rf.n, textfmt.OneLine(rf.last, 200))},
			Action:   "没自动处理：每天 05:30 的定时备份会再推一次，仍失败会写在当天的 *-backup.md",
		})
	}
	return res
}

// 备份脚本（~/.claude/scripts/backup-push.sh）的固定写法：第一行「…，**N 个失败**」，失败的仓库一行「- 名字：**推送失败**：报错」。
var (
	backupFailRe     = regexp.MustCompile(`\*\*(\d+) 个失败\*\*`)
	backupFailLineRe = regexp.MustCompile(`^- .+：\*\*推送失败\*\*`)
)

// checkBackupReport：当天的 <日期>-backup.md 第一行写着「**N 个失败**」就报，证据带失败的那几行。
func (a *Service) checkBackupReport(now time.Time, fail func(prefix, note string)) []PatrolIssue {
	day := now.Format("2006-01-02")
	path := filepath.Join(a.Home, ".claude", "nightly", day+"-backup.md")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		fail("backup:", "读不了 "+path+"："+err.Error())
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	m := backupFailRe.FindStringSubmatch(lines[0])
	if m == nil || m[1] == "0" {
		return nil
	}
	evs := []string{path + " 第一行：" + textfmt.OneLine(lines[0], 200)}
	for _, l := range lines[1:] {
		if backupFailLineRe.MatchString(strings.TrimSpace(l)) && len(evs) < 4 {
			evs = append(evs, textfmt.OneLine(l, 200))
		}
	}
	return []PatrolIssue{{
		Fingerprint: "backup:" + day, Check: "backup", Anomaly: true,
		Title:    "今天的备份有 " + m[1] + " 个仓库推送失败",
		Evidence: evs,
		Action:   "没自动处理：明天 05:30 会再推一次；急的话手动执行 ~/.claude/scripts/backup-push.sh",
	}}
}

// CheckJobs：最近 patrolWindow 里失败、超时、中断的任务（撞用量上限的、等着自动续跑或已经续跑的不算，巡逻分析、Telegram 对话和任务箱自己的不算），
// 和会话记录超过 Config.StallAfter 没增长的运行中任务。
func (a *Service) CheckJobs(st *state.State, now time.Time) []PatrolIssue {
	var res []PatrolIssue
	for _, j := range st.Jobs {
		if j.Kind == state.KindPatrol || state.IsChatKind(j.Kind) { // 对话任务没做成时已经直接回了用户
			continue
		}
		if j.Kind == state.KindNew && j.Status != state.StatusRunning { // 结束时已经直接推了（noticeNew），在跑时照样看卡住
			continue
		}
		if j.Kind == state.KindDiscuss && j.Status != state.StatusRunning { // 可有可无的后台活，结果已写进当天汇总；在跑时照样看卡住
			continue
		}
		name := j.Project + "「" + textfmt.TruncRunes(j.Title, 40) + "」"
		if j.Status == state.StatusRunning {
			idle, ok := a.Runner.IdleFor(j)
			if !ok || idle < a.Cfg.StallAfter.Duration {
				continue
			}
			res = append(res, PatrolIssue{
				Fingerprint: "stall:" + j.ID, Check: "stall", Anomaly: !a.Backlog.Excluded(j.Project), quiet: true,
				Title: "任务疑似卡住：" + name,
				Evidence: []string{fmt.Sprintf("任务 %s 已运行 %s，会话记录 %s 没增长（超过 %s 算卡住），pid %d",
					j.ID, now.Sub(j.StartedAt).Round(time.Minute), textfmt.FormatIdle(idle), textfmt.FormatDuration(a.Cfg.StallAfter.Duration), j.PID)},
				Action: fmt.Sprintf("没自动处理：不会自动杀，运行满 %s 才终止；要停就 agentq stop %s", textfmt.FormatDuration(a.Runner.JobTimeout(j)), j.ID),
				jobID:  j.ID,
			})
			continue
		}
		if j.EndedAt == nil || now.Sub(*j.EndedAt) > patrolWindow {
			continue
		}
		switch j.Status {
		case state.StatusFailed, state.StatusTimeout, state.StatusInterrupted:
		default:
			continue
		}
		if j.LimitHit || j.ResumePending || j.ResumedBy != "" {
			continue
		}
		if j.BudgetHit || j.AutoStop != "" { // 结束时已经推过用户，写了卡住或续跑，不再为它起分析
			continue
		}
		if j.Transient && j.Kind == "" { // 上游瞬时错误，会自己按短退避重试；夜班、复盘、对话不重试，照报
			continue
		}
		is := PatrolIssue{
			Fingerprint: "job:" + j.ID, Check: "job", Anomaly: !a.Backlog.Excluded(j.Project),
			Title: "任务" + state.StatusLabel(j.Status) + "：" + name,
			Evidence: []string{fmt.Sprintf("任务 %s，%s 启动，%s 结束：%s", j.ID, textfmt.WhenCN(j.StartedAt),
				textfmt.WhenCN(*j.EndedAt), textfmt.OneLine(j.Summary, 200)), "详情：agentq logs " + j.ID},
			Action: "没自动处理：原因看 agentq logs " + j.ID,
			jobID:  j.ID,
		}
		if j.Kind == "" {
			is.job = j
		}
		res = append(res, is)
	}
	return res
}

// TakeOverTodo：中断/失败/超时的待做任务，BACKLOG 里那一条还标着进行中、也没有别的任务在接手时，
// 在那个项目 BACKLOG「待做」末尾写一条接手的待做并提交，返回处理说明。接手的待做自己又没做完时不再写（免得一层套一层），
// 暂停时只报不写。第二个返回值：已经有人接手（写了待做、或已有任务在接手），不用推给用户。
func (a *Service) TakeOverTodo(st *state.State, j *state.Job) (string, bool) {
	if j.Takeover {
		return "这一条是 tick 接手的孤儿条目，只接手一次，没做完就标卡住，不再写接手待做；原因看 agentq logs " + j.ID, false
	}
	if strings.HasPrefix(j.Title, backlog.TakeOverPrefix) {
		return "这一条本身是巡逻写的接手待做，又没做完，不再写新的接手条目，等你处理；原因看 agentq logs " + j.ID, false
	}
	if a.Backlog.Excluded(j.Project) {
		return "项目在 exclude_projects 里，只报不写接手的待做，没有人会自动接手；要接着做就 agentq run " + j.Project + "；原因看 agentq logs " + j.ID, false
	}
	if a.Cfg.Paused {
		return "已暂停（paused=true），只报不写接手的待做；原因看 agentq logs " + j.ID, false
	}
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	items, err := p.Items()
	if err != nil {
		return "没写接手的待做：读不了 BACKLOG：" + err.Error(), false
	}
	it, ok := runner.InProgressItem(items, j.Fingerprint)
	if !ok {
		return "BACKLOG 里这一条已不在进行中，不用接手；原因看 agentq logs " + j.ID, false
	}
	if a.Runner.TakeoverNow(j) { // 再写一条接手待做反而挡住 tick 的接手：新项目第一版只挑第一条，孤儿扫描见到接手待做也让开
		return "续跑不了，下一次 tick 直接接手这一条，不另写接手待做；原因看 agentq logs " + j.ID, true
	}
	for _, o := range st.Jobs {
		if o == j || !state.SameItem(o, j.Project, j.Fingerprint) {
			continue
		}
		if o.Status == state.StatusRunning || o.ResumePending || (o.EndedAt != nil && o.EndedAt.After(*j.EndedAt)) {
			return "之后已有任务 " + o.ID + " 在接手这一条，没写待做", true
		}
	}
	title := backlog.TakeOverPrefix + textfmt.TruncRunes(j.Title, 30) + "」"
	// 不能出现「（进行中」和括号卡住标记，否则这一条自己会被当成进行中或卡住、永远不会被挑
	line := fmt.Sprintf("- [ ] %s —— 巡逻发现任务 %s 于 %s %s，原条目「%s」还标着进行中%s。看 git 状态、worktree 和分支现状，"+
		"按 agent-task 从实际进度接着把原条目的分支做到可合并，写 ready.json；认领这一条时写原条目的分支，引擎合并后把两条一起移到「已完成」；做不下去就去掉原条目的进行中标记、"+
		"按 agent-task 换成卡住标记写明原因。验收：原条目不再标进行中（已完成或标了卡住）；任务记录 %s",
		title, j.ID, textfmt.WhenCN(*j.EndedAt), state.StatusLabel(j.Status), j.Title, runner.BranchNote(j.ProjectDir, it.Text), a.JobDir(j.ID))
	if msg, err := a.Backlog.CommitTodo(p, j.ID, line, "待办："+title+"（巡逻）"); err != nil {
		return "没写接手的待做：" + err.Error(), false
	} else if msg != "" {
		return msg, true // 已经写过接手的待做
	}
	a.Logf("巡逻：在 %s 的 BACKLOG 写了待做「%s」", p.Name, title)
	return "已在 " + p.Name + " 的 BACKLOG「待做」末尾写了「" + title + "」并提交，agentq 会接着做", true
}

// checkUsage：state 里缓存的订阅额度（tick 领活前读的，巡逻不另读）到了 usageStopAt。
func (a *Service) checkUsage(st *state.State, now time.Time) []PatrolIssue {
	u := st.UsageOf(a.Cfg.Backend)
	lvl, until, why := backend.UsageLevel(u, now, a.Cfg.UsagePolicy())
	if lvl < backend.UsageStop {
		return nil
	}
	fp := "usage"
	for _, w := range u.Windows {
		if w.Counts(a.Cfg.Model) && w.UsedAt(now) >= a.Cfg.UsagePolicy().StopAt(w) {
			reset := "?"
			if w.ResetsAt != nil {
				reset = w.ResetsAt.Format("2006-01-02T15:04")
			}
			fp += ":" + w.Label + "@" + reset
		}
	}
	return []PatrolIssue{{
		Fingerprint: fp, Check: "usage", quiet: true,
		Title:    "订阅额度到了后台上限：" + why,
		Evidence: []string{"额度：" + backend.UsageDescribe(u, now, a.Cfg.UsagePolicy()) + "（" + textfmt.WhenCN(u.CheckedAt) + " 读取）"},
		Action:   backend.UsageGateNote(lvl, until, why),
	}}
}

// CheckReports：patrolReportHour 点以后，开关打开的复盘还没写当天早报（<日期>.md）、夜班还没写报告（<日期>-night.md）。
// 正在跑的不报；今天到点时开关还关着的不报。
func (a *Service) CheckReports(st *state.State, now time.Time) []PatrolIssue {
	if now.Hour() < patrolReportHour {
		return nil
	}
	day := now.Format("2006-01-02")
	var res []PatrolIssue
	for _, x := range []struct{ kind, what string }{{config.KindReview, "早报"}, {config.KindNight, "夜班报告"}} {
		r := config.Routines[x.kind]
		if !r.Enabled(a.Cfg) || a.RoutineOffOn(x.kind, now) { // 到点时开关关着、后来才打开的，今天本来就没有
			continue
		}
		path := filepath.Join(a.ReportDir(), day+r.Report+".md")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		var last *state.Job
		running := false
		for _, j := range st.Jobs {
			if j.Kind != x.kind || j.StartedAt.Format("2006-01-02") != day {
				continue
			}
			if j.Status == state.StatusRunning {
				running = true
			}
			last = j
		}
		if running {
			continue
		}
		evs := []string{fmt.Sprintf("%s 不存在（%s 查）", path, now.Format("15:04"))}
		action := fmt.Sprintf("没自动处理：要补就在终端执行 agentq %s", x.kind)
		if last != nil {
			evs = append(evs, fmt.Sprintf("今天的%s任务 %s %s：%s", r.Name, last.ID, state.StatusLabel(last.Status), textfmt.OneLine(last.Summary, 150)))
			action = "没自动处理：原因看 agentq logs " + last.ID + "；要补就在终端执行 agentq " + x.kind
		} else {
			evs = append(evs, fmt.Sprintf("今天没有%s任务的记录（%s 每天 %s 叫醒，看 %s）", r.Name, a.Service.Svc().Words().Name, r.At(), a.LaunchdLogPath()))
		}
		res = append(res, PatrolIssue{
			Fingerprint: "report:" + x.kind + ":" + day, Check: "report", Anomaly: true,
			Title:    fmt.Sprintf("%02d:00 后还没有当天的%s", patrolReportHour, x.what),
			Evidence: evs,
			Action:   action,
		})
	}
	return res
}

// checkDecisions：各项目 BACKLOG「等用户决定」里的条目（「- [ ]」开头），指纹按标题算，条目一直在就只报一次。
// 带等条件标记的跳过（前置条件没满足，现在不用用户回复）。推送由 tick 每 5 分钟看一遍（internal/notify/notify.go watchDecisions），巡逻只记汇总。
func (a *Service) checkDecisions(fail func(prefix, note string)) []PatrolIssue {
	all, fails := a.Notify.Decisions()
	decs := notify.Askable(all)
	for _, f := range fails {
		fail("decide:", f)
	}
	var res []PatrolIssue
	for _, d := range decs {
		res = append(res, PatrolIssue{
			Fingerprint: d.Fingerprint, Check: "decide",
			Title:    d.Project + " 等你决定：" + textfmt.TruncRunes(d.Title, 60),
			Evidence: []string{d.Path + "「等用户决定」：" + textfmt.OneLine(d.Text, 200)},
			Action:   "要你回复：在会话里或 Telegram 上说选哪个（或直接改那一条），agent 会按决定接着做",
		})
	}
	return res
}

// checkDisk：HOME 所在磁盘剩余低于 patrolDiskMin。
func (a *Service) checkDisk(fail func(prefix, note string)) []PatrolIssue {
	free, total, err := a.diskFree(a.Home)
	if err != nil {
		fail("disk", "读不了磁盘剩余空间："+err.Error())
		return nil
	}
	if free >= patrolDiskMin {
		return nil
	}
	gb := func(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) }
	return []PatrolIssue{{
		Fingerprint: "disk", Check: "disk", Anomaly: true,
		Title:    "磁盘剩余不到 " + strconv.Itoa(patrolDiskMin>>30) + " GB",
		Evidence: []string{a.Home + " 所在磁盘剩余 " + gb(free) + " GB（共 " + gb(total) + " GB）"},
		Action:   "没自动处理：清理不用的大文件（Xcode 的 DerivedData、旧模拟器和归档常占很多）",
	}}
}

func (a *Service) diskFree(path string) (free, total uint64, err error) {
	if a.DiskFree != nil {
		return a.DiskFree(path)
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	return s.Bavail * uint64(s.Bsize), s.Blocks * uint64(s.Bsize), nil
}
