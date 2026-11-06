package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// ReadyTypes 是 ready.json 的 type 能写的值。
var ReadyTypes = []string{"fix", "feature", "rewrite", "docs"}

// ReadReady 读任务分支上的 ready.json。found 为假是还没写；写了但不合格返回错误，任务要改了再写。
func (a *Service) ReadReady(dir, branch string) (Ready, string, bool, error) {
	var r Ready
	if branch == "" || strings.HasPrefix(branch, "-") {
		return r, "", false, nil
	}
	ctx := context.Background()
	head, err := a.git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return r, "", false, nil
	}
	if _, err := a.git(ctx, dir, "cat-file", "-e", head+":"+ReadyFile); err != nil {
		return r, head, false, nil
	}
	raw, err := a.git(ctx, dir, "show", head+":"+ReadyFile)
	if err != nil {
		return r, head, false, fmt.Errorf("读 %s：%w", ReadyFile, err)
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, head, false, fmt.Errorf("%s 不是合法的 JSON：%w", ReadyFile, err)
	}
	switch {
	case r.Branch != "" && r.Branch != branch:
		return r, head, false, fmt.Errorf("%s 写的分支 %s 不是任务分支 %s", ReadyFile, r.Branch, branch)
	case !slices.Contains(ReadyTypes, r.Type):
		return r, head, false, fmt.Errorf("%s 的 type %q 只能是 %s", ReadyFile, r.Type, strings.Join(ReadyTypes, "、"))
	case strings.TrimSpace(r.Acceptance) == "":
		return r, head, false, fmt.Errorf("%s 没写 acceptance（用户怎么验收）", ReadyFile)
	case strings.TrimSpace(r.Progress) == "":
		return r, head, false, fmt.Errorf("%s 没写 progress（给 PROGRESS 的一句话）", ReadyFile)
	}
	r.Branch = branch
	return r, head, true, nil
}

// Enqueue 把任务排进门队列（收尾时调用，调用方拿着 tick.lock）。
func (a *Service) Enqueue(j *state.Job, r Ready, head string) {
	j.Gate = &state.GateRecord{State: state.GateQueued, Branch: r.Branch, Head: head, Round: j.GateRound, Ready: r, QueuedAt: a.Now()}
	a.Logf("门：任务 %s（%s「%s」）分支 %s 可合并，排队过门（第 %d 轮）", j.ID, j.Project, textfmt.TruncRunes(j.Title, 40), r.Branch, j.GateRound)
}

// Bypassed：任务写了 ready.json，分支却已经在默认分支上（自己合并了，或别人合的）。没过门，不能记成过门合并：标卡住，发卡片等用户定。默认分支上残留的 ready.json 顺手删掉，不然连累后面的任务。
func (a *Service) Bypassed(st *state.State, j *state.Job, r Ready, head string) {
	j.Gate = &state.GateRecord{State: state.GateStuck, Branch: r.Branch, Head: head, Round: j.GateRound, Ready: r, QueuedAt: a.Now()}
	def, err := a.jobBranch(j)
	if err != nil { // 能走到这里说明刚认出来过：理由里的名字不要紧
		def = "默认分支"
	}
	a.stuck(st, j, bypassReason(def, head))
	a.dropStrayReady(j)
}

// dropStrayReady 在 <项目>-main 锁内删掉默认分支上的 ready.json，只提交这一个文件。做不成（主目录不在默认分支、这个文件有没提交的改动）只记日志：下一次引擎合并也会带掉。
func (a *Service) dropStrayReady(j *state.Job) {
	ctx := context.Background()
	def, err := a.jobBranch(j)
	if err != nil {
		a.Logf("门：默认分支上残留的 %s 这次没删：%v", ReadyFile, err)
		return
	}
	if _, err := a.git(ctx, j.ProjectDir, "cat-file", "-e", "refs/heads/"+def+":"+ReadyFile); err != nil {
		return
	}
	unlock, err := a.Backlog.LockMain(j.Project+"-main", backlog.MainLockWait)
	if err != nil {
		a.Logf("门：%s 上残留的 %s 这次没删：%v", def, ReadyFile, err)
		return
	}
	defer unlock()
	if br, err := a.git(ctx, j.ProjectDir, "symbolic-ref", "--short", "HEAD"); err != nil || br != def {
		a.Logf("门：%s 上残留的 %s 这次没删：主目录不在 %s", def, ReadyFile, def)
		return
	}
	if out, err := a.git(ctx, j.ProjectDir, "status", "--porcelain", "--", ReadyFile); err != nil || out != "" {
		a.Logf("门：%s 上残留的 %s 这次没删：有没提交的改动", def, ReadyFile)
		return
	}
	if out, err := a.Gitx.Git(j.ProjectDir, "rm", "-q", "-f", "--", ReadyFile); err != nil {
		a.Logf("门：%s 上残留的 %s 删不掉：%s", def, ReadyFile, textfmt.OneLine(out, 200))
		return
	}
	if out, err := a.Gitx.Git(j.ProjectDir, "commit", "-q", "-m", "待办：去掉 "+def+" 上残留的 "+ReadyFile, "--", ReadyFile); err != nil {
		a.Logf("门：%s 上残留的 %s 提交不了：%s", def, ReadyFile, textfmt.OneLine(out, 200))
	}
}

// start 起后台进程。它在自己的进程组里，出了什么事都不影响 tick。
func (a *Service) start(j *state.Job) error {
	self, err := a.selfPath()
	if err != nil {
		return err
	}
	dir := a.Dir(j.ID)
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(PlanOf(a, j), "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, "plan.json"), b); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "result.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	logf, err := os.OpenFile(a.LaunchdLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, ProcToken, j.ID)
	cmd.Dir = dir
	cmd.Env = proc.ChildEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // 本进程还活着时不留僵尸，退出后交给 launchd 回收
	now := a.Now()
	g := j.Gate
	g.State, g.PID, g.StartedAt, g.BootTime = state.GateRunning, cmd.Process.Pid, now, a.CurBoot()
	g.Deadline = now.Add(a.Cfg.VerifyTimeout.Duration*deadlineRuns + 2*gitWait + mergeLockWait + 2*proc.KillGrace)
	a.Logf("门：任务 %s（%s，分支 %s）开始过门", j.ID, j.Project, g.Branch)
	return nil
}

// StartQueued 给排队的任务起门进程：每个项目同时一个，计入 max_parallel。暂停时照样跑：过门是收尾，手动 run 的任务也要合并；冷却时不跑，审查要调模型。
func (a *Service) StartQueued(st *state.State) int {
	now := a.Now()
	busy := map[string]bool{}
	for _, j := range st.Gating() {
		busy[j.ProjectDir] = true
	}
	var queue []*state.Job
	for _, j := range st.Jobs {
		if j.Gate != nil && j.Gate.State == state.GateQueued {
			queue = append(queue, j)
		}
	}
	sort.SliceStable(queue, func(i, k int) bool { return queue[i].Gate.QueuedAt.Before(queue[k].Gate.QueuedAt) })
	started := 0
	for _, j := range queue {
		if now.Before(st.CooldownOf(j.Backend, a.Cfg.Backend)) { // 审查用任务的后端，别的后端在冷却不挡它
			continue
		}
		// 时钟跳回来之后 NotBefore 还在很远的未来：不让它一直等
		if nb := j.Gate.NotBefore; now.Before(nb) && nb.Before(now.Add(a.Cfg.LimitCooldown.Duration+time.Hour)) {
			continue
		}
		if busy[j.ProjectDir] {
			continue
		}
		if ok, _ := state.Capacity(st, a.Cfg, ""); !ok {
			break
		}
		if err := a.start(j); err != nil {
			a.crashed(st, j, "门进程起不来："+textfmt.OneLine(err.Error(), 200))
			continue
		}
		busy[j.ProjectDir] = true
		started++
		a.Ledger.WriteMeta(j)
		if err := state.Save(a.StatePath(), st); err != nil { // 下次 tick 会多起一个，靠任务目录的锁只有一个真跑
			a.Logf("门：写状态文件失败：%v", err)
		}
	}
	return started
}

// procHeld：后台进程还拿着过门目录的锁。比 pid 准：进程死了锁就没了，pid 可能被复用。
func procHeld(dir string) bool {
	lock, err := fsx.AcquireLock(filepath.Join(dir, "lock"), 0)
	if err != nil {
		return false
	}
	if lock == nil {
		return true
	}
	lock.Close()
	return false
}

func readResult(dir string) (RunResult, bool) {
	var res RunResult
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil || json.Unmarshal(b, &res) != nil || res.Outcome == "" {
		return RunResult{}, false
	}
	return res, true
}

// Collect 是 tick 里的一步：先落用户的决定，再收门进程的结果，补写之前没写上的收尾。没有过门记录的任务什么都不做。
func (a *Service) Collect(st *state.State) {
	for _, j := range st.Jobs {
		a.applyDecision(st, j) // 放弃能处理没有过门记录的任务
		if j.Gate == nil {
			continue
		}
		switch {
		case j.Gate.State == state.GateRunning:
			a.collectOne(st, j)
		case j.Gate.State == state.GateMerged && j.Gate.WrapErr != "":
			a.retryWrap(j)
		}
		a.retryClean(j)
	}
}

func (a *Service) collectOne(st *state.State, j *state.Job) {
	g := j.Gate
	dir := a.Dir(j.ID)
	res, ok := readResult(dir)
	why := "门进程没了，也没留下结果（被杀或机器重启过）"
	if !ok && procHeld(dir) {
		if a.Now().Before(g.Deadline) {
			return
		}
		// 期限是墙上时钟：Mac 合盖睡了很久、后台进程自己卡住都会过期，说明不了任务有问题
		why = "门进程过了 " + textfmt.WhenCN(g.Deadline) + " 还没结束，已终止（机器睡过或进程卡住）"
		if proc.ProcAlive(g.PID, ProcToken+" "+j.ID) {
			proc.KillGroup(g.PID, proc.KillGrace)
		}
		res, ok = readResult(dir)
	}
	if !ok {
		a.killLeftovers(j, dir)
		if procHeld(dir) { // 杀不掉：期限过了也不重起第二个
			a.Logf("门：任务 %s 的门进程杀不掉，下次 tick 再看", j.ID)
			return
		}
		a.clean(j)
		a.crashed(st, j, why)
		return
	}
	a.clean(j)
	if err := os.Rename(filepath.Join(dir, "result.json"), filepath.Join(dir, "last.json")); err != nil {
		a.Logf("门：任务 %s 的结果归不了档：%v", j.ID, err)
	}
	g.PID, g.Crashes = 0, 0
	a.addUsage(j, res)
	mergeTally(g, res.Tally)
	if res.Outcome != outDefer {
		g.Upstream = 0
	}
	g.Report = textfmt.OneLine(res.Reason, 300)
	if res.Head != "" {
		g.Head = res.Head // 用户看报告、放行，看的都是这个提交
	}
	switch res.Outcome {
	case outMerged:
		a.merged(st, j, res)
	case outBypass:
		a.stuck(st, j, res.Reason)
		a.dropStrayReady(j)
	case outFail:
		a.failed(st, j, res)
	case outWait:
		a.waitMain(j, res)
	case outDefer:
		a.deferRun(st, j, res)
	default:
		a.stuck(st, j, res.Reason)
	}
	a.Ledger.WriteMeta(j)
}

// mergeTally：这次过门的次数记到任务上。
func mergeTally(g *state.GateRecord, tally map[string]*state.GateTally) {
	for name, t := range tally {
		if g.Tally == nil {
			g.Tally = map[string]*state.GateTally{}
		}
		x := g.Tally[name]
		if x == nil {
			x = &state.GateTally{}
			g.Tally[name] = x
		}
		x.Pass += t.Pass
		x.Fail += t.Fail
		x.Unknown += t.Unknown
		x.WouldBlock += t.WouldBlock
		x.Deferred += t.Deferred
		x.Usage = backend.AddUsage(x.Usage, t.Usage)
	}
}

// addUsage：门调模型的用量算在任务头上，当天花费才对得上。Gate.Usage 单记一份：stats 会按结果文件重读任务自己的用量。
func (a *Service) addUsage(j *state.Job, res RunResult) {
	j.Usage = backend.AddUsage(j.Usage, res.Usage)
	j.Gate.Usage = backend.AddUsage(j.Gate.Usage, res.Usage)
}

func (a *Service) merged(st *state.State, j *state.Job, res RunResult) {
	g := j.Gate
	g.State, g.Merge, g.WrapErr, g.Rerun = state.GateMerged, res.Merge, res.WrapErr, false
	short := textfmt.TruncRunes(res.Merge, 12)
	j.Summary = "合并提交 " + short + "；" + j.Summary
	msg := fmt.Sprintf("门：%s「%s」过门合并，合并提交 %s", j.Project, textfmt.TruncRunes(j.Title, 40), short)
	if g.Override != "" {
		msg += "（用户放行：" + textfmt.OneLine(g.Override, 100) + "）"
	}
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	for _, r := range res.Results { // warn 档只记录：这里就是记录
		if r.Level == LevelWarn && r.Outcome != Pass {
			note := fmt.Sprintf("门：%s「%s」%s 门（warn）%s：%s", j.Project, textfmt.TruncRunes(j.Title, 40), r.Gate, r.Outcome, textfmt.OneLine(r.Summary, 200))
			a.Logf("%s", note)
			a.Ledger.AppendNightlyLine(a.Now(), note)
		}
	}
	if len(res.Off) > 0 {
		a.Ledger.AppendNightlyLine(a.Now(), fmt.Sprintf("门：%s「%s」合并时这些门关着：%s", j.Project, textfmt.TruncRunes(j.Title, 40), strings.Join(res.Off, "、")))
	}
	lines := []string{"合并提交 " + short}
	if accept := textfmt.OneLine(g.Ready.Acceptance, 200); accept != "" {
		lines = append(lines, "验收："+accept)
	}
	if j.NewReport != nil {
		j.NewReport.Commit = short
		lines = append(lines, "项目　"+j.NewReport.Path)
		if assume := textfmt.OneLine(j.NewReport.Assumptions, 300); assume != "" {
			lines = append(lines, "补的假设　"+assume)
		}
	}
	cat, text := "done", j.Project+"「"+textfmt.TruncRunes(j.Title, 30)+"」做完了，已合并"
	if j.NewReport != nil {
		text = "新项目第一版合并了：" + j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」"
	}
	if smoke, ran := resultNamed(res.Results, "smoke"); ran {
		if smoke.Outcome == Pass {
			if view, path := a.deliver(j, res, short); path != "" {
				text = j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」可以交付了"
				if j.NewReport != nil {
					text = "新项目第一版可以交付了：" + j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」"
				}
				lines = append(lines, "怎么打开看："+textfmt.OneLine(view, 200), "交付单 "+a.Tilde(path))
			}
		} else { // warn 档走到这里：合并了，但烟测没过，不算交付
			why := "烟测没过"
			if smoke.Outcome == Unknown {
				why = "烟测判不了"
			}
			cat, text = "alert", j.Project+"「"+textfmt.TruncRunes(j.Title, 30)+"」合并了，"+why+"，没有交付单"
			lines = append(lines, textfmt.OneLine(smoke.Summary, 200))
		}
	}
	a.Notify.Notify(notify.Notice{Cat: cat, Key: notify.TaskEndKey(j.Project, j.Fingerprint), JobID: j.ID, Text: text, Lines: lines})
	if res.WrapErr != "" {
		a.Logf("门：任务 %s 已经合并，BACKLOG 没写上，下次 tick 再写：%s", j.ID, res.WrapErr)
	}
}

func resultNamed(results []Result, name string) (Result, bool) {
	for _, r := range results {
		if r.Gate == name {
			return r, true
		}
	}
	return Result{}, false
}

// DeliveryFile 是交付单，在任务的过门目录里。
const DeliveryFile = "delivery.md"

// deliver 写交付单：做了什么、怎么打开看。只有成品门过了才写。返回怎么打开看和文件路径，写不了路径是空，只记日志，不影响已经合并的结果。
func (a *Service) deliver(j *state.Job, res RunResult, short string) (string, string) {
	view := j.Gate.Ready.Acceptance
	if j.NewReport != nil && j.NewReport.View != "" {
		view = j.NewReport.View
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 交付单：%s「%s」\n\n合并提交 %s，%s\n\n## 做了什么\n\n%s\n\n条目：%s\n", j.Project, textfmt.OneLine(j.Title, 80), short, a.Now().Format("2006-01-02 15:04"), textfmt.OneLine(j.Gate.Ready.Progress, 300), textfmt.OneLine(j.Item, 500))
	if names, err := a.git(context.Background(), j.ProjectDir, "diff", "--name-only", res.Merge+"^1", res.Merge); err == nil && names != "" {
		files := strings.Split(names, "\n")
		fmt.Fprintf(&b, "改动 %d 个文件：%s\n", len(files), textfmt.OneLine(strings.Join(files, "、"), 500))
	}
	fmt.Fprintf(&b, "\n## 怎么打开看\n\n%s\n", textfmt.OneLine(view, 500))
	if accept := textfmt.OneLine(j.Gate.Ready.Acceptance, 500); accept != "" && accept != textfmt.OneLine(view, 500) {
		fmt.Fprintf(&b, "\n验收：%s\n", accept)
	}
	if j.NewReport != nil && j.NewReport.Assumptions != "" {
		fmt.Fprintf(&b, "\n## 补的假设\n\n%s\n", textfmt.OneLine(j.NewReport.Assumptions, 500))
	}
	path := filepath.Join(a.Dir(j.ID), DeliveryFile)
	if err := fsx.WriteFileAtomic(path, []byte(b.String())); err != nil {
		a.Logf("门：任务 %s 的交付单写不了：%v", j.ID, err)
		return view, ""
	}
	return view, path
}

// retryWrap 补写收尾。同样的原因只记一次日志。
func (a *Service) retryWrap(j *state.Job) {
	err := a.WrapUp(PlanOf(a, j), j.Gate.Merge)
	msg := ""
	if err != nil {
		msg = textfmt.OneLine(err.Error(), 300)
	}
	if msg == j.Gate.WrapErr {
		return
	}
	j.Gate.WrapErr = msg
	if msg != "" {
		a.Logf("门：任务 %s 的收尾还是没写上，下次 tick 再写：%s", j.ID, msg)
	}
	a.Ledger.WriteMeta(j)
}

// failed：门没过。还有轮数就退回任务接着改，用完了等用户定。
func (a *Service) failed(st *state.State, j *state.Job, res RunResult) {
	g := j.Gate
	be := a.BackendFor(j.Backend)
	first := j.SessionInRoot() // 新项目第一版的会话在 projects_root，换了目录接不回去：和不能续跑的后端一样，由接手任务接着做
	// 用户放行之后还没过（只会是合并冲突）：条目标着卡住，退回没人接得上，再让用户定。再审没过同理，也不算退回轮数
	if g.Override != "" || g.Rerun || g.Round >= MaxRounds || a.Backlog.Excluded(j.Project) || (be.Caps().Resume && j.SessionID == "" && !first) {
		a.stuck(st, j, res.Reason)
		return
	}
	g.State = state.GateReturned
	j.Status = state.StatusInterrupted           // 这一条其实没做完：续跑和接手都认这个状态
	j.ResumePending = be.Caps().Resume && !first // 不能续跑的后端由接手任务从分支现状接着做
	j.Summary = fmt.Sprintf("门没过，退回第 %d 轮：%s", g.Round+1, textfmt.OneLine(res.Reason, 200))
	msg := fmt.Sprintf("门：%s「%s」没过，退回第 %d 轮（最多 %d 轮）：%s", j.Project, textfmt.TruncRunes(j.Title, 40), g.Round+1, MaxRounds, textfmt.OneLine(res.Reason, 200))
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
}

// waitMain：门都过了，主目录挡着（有重叠的改动、不在默认分支、默认分支一直在动）。排回队尾，门的结论按 tree 留着，隔一个定时 tick 再试：
// 门进程占名额，主目录挡几天也不能让它每次 tick 都把名额占掉，待做还要接着做。只通知一次。
func (a *Service) waitMain(j *state.Job, res RunResult) {
	g := j.Gate
	g.State, g.QueuedAt, g.NotBefore = state.GateQueued, a.Now(), a.Now().Add(waitRetry)
	if g.WaitNoted {
		return
	}
	g.WaitNoted = true
	msg := fmt.Sprintf("门：%s「%s」门都过了，还没合并：%s；下次 tick 重试", j.Project, textfmt.TruncRunes(j.Title, 40), textfmt.OneLine(res.Reason, 200))
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	name := j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」门过了"
	n := notify.Notice{Cat: "alert", Key: "gate-wait:" + j.ID, JobID: j.ID, Lines: []string{textfmt.OneLine(res.Reason, 200)}}
	switch {
	case res.WaitBranch != "":
		n.Text = name + "，主目录不在 " + res.WaitBranch
		n.Lines = append(n.Lines, "到 "+a.Tilde(j.ProjectDir)+" 执行 git switch "+res.WaitBranch+"，下次 tick 自动合并")
	case res.WaitChanges:
		n.Text = name + "，主目录的改动挡着合并"
		n.Lines = append(n.Lines, "主目录有和这次改动重叠的未提交改动，不会覆盖。提交或还原后下次 tick 自动合并")
	default:
		n.Text = name + "，这次没合并上"
		n.Lines = append(n.Lines, "下次 tick 自动再试；详情 agentq gate report "+j.ID)
	}
	a.Notify.Notify(n)
}

// deferRun：撞了用量上限或认证失败，不算门的结论，冷却过后重跑。撞限走全局冷却，和任务撞限一样。
func (a *Service) deferRun(st *state.State, j *state.Job, res RunResult) {
	g := j.Gate
	now := a.Now()
	until := now.Add(a.Cfg.LimitCooldown.Duration)
	limit, auth, upstream := false, false, false
	for _, r := range res.Results {
		if r.Defer == "" {
			continue
		}
		limit, auth, upstream = r.Limit, r.Auth, r.Upstream
		if r.ResetAt.After(now) {
			until = r.ResetAt
		}
	}
	if upstream {
		until = now.Add(state.TransientDelay(g.Upstream+1, a.Cfg.RetryAfter.Duration))
		g.Upstream++
	}
	if auth { // 和任务撞上认证失败一样：暂停接活并告诉用户，不然他不知道为什么一直没合并
		be := a.BackendFor(j.Backend)
		until = a.PauseAuth(be.Name())
		a.Notify.Notify(notify.AuthPaused(be.Name(), j.ID, a.Tilde(a.AuthPausePath(be.Name())), until))
	}
	g.State, g.QueuedAt = state.GateQueued, now
	if limit { // 撞限走这个后端的冷却，不用自己等：提示的重置时间可能在几天后
		st.ExtendCooldown(j.Backend, a.Cfg.Backend, until)
	} else {
		g.NotBefore = until
	}
	a.Logf("门：任务 %s 这次判不了，到 %s 再跑：%s", j.ID, textfmt.WhenCN(until), textfmt.OneLine(res.Reason, 200))
}

// crashed：门进程没留下结果。worktree 一次性，排回去重跑；重来多次还这样就算判不了。
func (a *Service) crashed(st *state.State, j *state.Job, why string) {
	g := j.Gate
	g.Crashes++
	g.PID = 0
	if g.Crashes >= maxCrashes {
		a.stuck(st, j, fmt.Sprintf("门进程连续 %d 次没留下结果，最后一次：%s", g.Crashes, why))
		a.Ledger.WriteMeta(j)
		return
	}
	g.State, g.QueuedAt = state.GateQueued, a.Now()
	a.Logf("门：任务 %s：%s，重跑（第 %d 次）", j.ID, why, g.Crashes)
	a.Ledger.WriteMeta(j)
}

// stuck：判不了或轮数用完。保留分支，条目标卡住（下次 RetryStuckMarks 写），发决定卡片。同项目下一条照常做。
func (a *Service) stuck(st *state.State, j *state.Job, why string) {
	g := j.Gate
	g.State, g.Override, g.Rerun = state.GateStuck, "", false
	g.Report = textfmt.OneLine(why, 300)
	j.StuckBranch = g.Branch
	j.StuckPending = "门没过：" + markSafe(why) + "，任务 " + j.ID
	j.Summary = "门没过，分支 " + g.Branch + " 留着：" + textfmt.OneLine(why, 200)
	msg := fmt.Sprintf("门：%s「%s」没过，等你定：%s", j.Project, textfmt.TruncRunes(j.Title, 40), textfmt.OneLine(why, 200))
	a.Logf("%s", msg)
	a.Ledger.AppendNightlyLine(a.Now(), msg)
	options := []notify.DecideOption{{Label: "A", Text: "放行"}, {Label: "B", Text: "退回"}, {Label: "C", Text: "放弃"}}
	how := "放行：合并这一次；退回：任务再改一轮；放弃：标卡住，不再管"
	if rerunRefusal(j) == "" {
		options = append(options, notify.DecideOption{Label: "D", Text: "再审一次"})
		how += "；再审一次：不改代码重新跑门，不算退回轮数"
	}
	a.Notify.Notify(notify.Notice{Cat: "decide", Key: "gate:" + j.ID, JobID: j.ID,
		Text:  j.Project + "　「" + textfmt.TruncRunes(j.Title, 40) + "」门没过",
		Lines: []string{textfmt.OneLine(why, 200), "分支 " + g.Branch + " 留着；详情 agentq gate report " + j.ID, how},
		Card:  &notify.DecideCard{Project: j.Project, Fingerprint: j.Fingerprint, Title: j.Title, Recommend: "B", GateJob: j.ID, Options: options}})
}

// rerunRefusal：这个任务现在不能再审的原因，能是空。同一提交有次数上限：判不了多半是外面的原因，一直点也是一直花额度。
func rerunRefusal(j *state.Job) string {
	g := j.Gate
	switch {
	case g == nil || g.State != state.GateStuck:
		return "不是卡在门上"
	case g.Head == "":
		return "没有可审的提交"
	case g.RerunHead == g.Head && g.Reruns >= MaxReruns:
		return fmt.Sprintf("提交 %s 已经再审过 %d 次", textfmt.TruncRunes(g.Head, 12), g.Reruns)
	}
	return ""
}

// markSafe：写进卡住标记的文字。原因里可能有模型写的话和全角括号：一个「）」就把标记截断，后面的话留在条目正文里，退回时认不回这一条。
func markSafe(why string) string {
	return backlog.CleanTodoText(strings.NewReplacer("（", "(", "）", ")").Replace(why), 150)
}

// clean 清这次过门的 worktree。清不掉下次 tick 再清。
func (a *Service) clean(j *state.Job) {
	if err := RemoveWorktree(j.ProjectDir, Worktree(a.Dir(j.ID))); err != nil {
		j.Gate.CleanErr = textfmt.OneLine(err.Error(), 200)
		a.Logf("门：任务 %s 的 worktree 没清干净，下次 tick 再清：%v", j.ID, err)
	}
}

// retryClean：结果收了但 worktree 还在（当时没清干净）。同样的错只记一次日志。
func (a *Service) retryClean(j *state.Job) {
	if j.Gate.State == state.GateRunning {
		return
	}
	wt := Worktree(a.Dir(j.ID))
	if _, err := os.Lstat(wt); err != nil {
		return
	}
	err := RemoveWorktree(j.ProjectDir, wt)
	msg := ""
	if err != nil {
		msg = textfmt.OneLine(err.Error(), 200)
	}
	if msg == j.Gate.CleanErr {
		return
	}
	j.Gate.CleanErr = msg
	if msg != "" {
		a.Logf("门：任务 %s 的 worktree 还是没清干净，下次 tick 再清：%s", j.ID, msg)
	}
	a.Ledger.WriteMeta(j)
}

// killLeftovers：门进程被 SIGKILL 后脚本可能还在跑。机器重启过的组号不可信，不动；组里有带标记的进程才杀。
func (a *Service) killLeftovers(j *state.Job, dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "pgid"))
	if err != nil {
		return
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pgid <= 0 || !proc.GroupAlive(pgid) {
		return
	}
	cur := a.CurBoot()
	if d := cur - j.Gate.BootTime; j.Gate.BootTime != 0 && cur != 0 && (d > proc.BootSlack || d < -proc.BootSlack) {
		return
	}
	marked := proc.ProcAlive(pgid, Worktree(dir))
	if !marked {
		marked, _ = proc.GroupMarked(pgid, gateMark(j.ID))
	}
	if !marked {
		a.Logf("门：任务 %s 的进程组 %d 还有进程，但没有门的标记，不清理", j.ID, pgid)
		return
	}
	proc.KillGroup(pgid, proc.KillGrace)
	a.Logf("门：任务 %s 的脚本在门进程没了之后还在跑，已终止进程组 %d", j.ID, pgid)
}

const waitRetry = 4 * time.Minute

// 用户的决定。卡片和命令都只写一个文件，下次 tick 落：tick 拿着锁，这里不用再抢，listen 里点按钮也不会和 tick 互等。
const (
	DecidePass    = "pass"
	DecideReturn  = "return"
	DecideAbandon = "abandon"
	DecideRerun   = "rerun"
)

type decision struct {
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Decide 记下用户对卡住任务的决定。
func (a *Service) Decide(id, action, reason string) error {
	b, err := json.Marshal(decision{Action: action, Reason: reason, At: a.Now()})
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(a.Dir(id), "decision.json"), b)
}

func (a *Service) applyDecision(st *state.State, j *state.Job) {
	path := filepath.Join(a.Dir(j.ID), "decision.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	drop := func() {
		if err := os.Remove(path); err != nil {
			a.Logf("门：任务 %s 的决定文件删不掉：%v", j.ID, err)
		}
	}
	var d decision
	if err := json.Unmarshal(b, &d); err != nil {
		a.Logf("门：任务 %s 的决定读不了，丢掉：%v", j.ID, err)
		drop()
		return
	}
	if d.Action == DecideAbandon {
		if why := abandonRefusal(st, j); why != "" {
			a.Logf("门：任务 %s：%s，放弃不生效", j.ID, why)
			drop()
			return
		}
		if err := a.abandon(st, j); err != nil { // 主目录不在默认分支、BACKLOG 有改动：决定留着，下次 tick 再做。用户已经看到「已定」了，不能丢
			a.Logf("门：任务 %s 放弃还没做成，下次 tick 再试：%s", j.ID, textfmt.OneLine(err.Error(), 200))
			return
		}
		drop()
		a.Ledger.WriteMeta(j)
		return
	}
	g := j.Gate
	if g == nil || (g.State != state.GateStuck && g.State != state.GateAbandoned) {
		a.Logf("门：任务 %s 已经%s，决定 %s 不生效", j.ID, state.JobLabel(j), d.Action)
		drop()
		return
	}
	title := textfmt.TruncRunes(j.Title, 40)
	switch d.Action {
	case DecidePass:
		if why := passRefusal(j); why != "" { // 比如放弃时才造的门记录：没人看过要合哪个提交
			a.Logf("门：任务 %s：%s，放行不生效", j.ID, why)
			drop()
			return
		}
		g.State, g.Override, g.QueuedAt, g.Crashes = state.GateQueued, d.Reason, a.Now(), 0
		g.NotBefore, g.Upstream = time.Time{}, 0
		g.Passes++
		j.StuckPending = ""
		j.Summary = "用户放行，合并分支 " + g.Branch + " 的提交 " + textfmt.TruncRunes(g.Head, 12) + "：" + textfmt.OneLine(d.Reason, 150)
		a.audit(fmt.Sprintf("放行 %s %s「%s」分支 %s 提交 %s：%s", j.ID, j.Project, title, g.Branch, g.Head, textfmt.OneLine(d.Reason, 300)))
	case DecideRerun:
		if why := rerunRefusal(j); why != "" {
			a.Logf("门：任务 %s：%s，再审不生效", j.ID, why)
			a.Notify.Notify(notify.Notice{Cat: "alert", Key: "gate-rerun:" + j.ID, JobID: j.ID, Text: j.Project + "「" + textfmt.TruncRunes(j.Title, 30) + "」没有再审：" + why,
				Lines: []string{"还能放行、退回或放弃；详情 agentq gate report " + j.ID}})
			drop()
			return
		}
		if g.RerunHead != g.Head {
			g.RerunHead, g.Reruns = g.Head, 0
		}
		g.Reruns++
		g.State, g.Rerun, g.QueuedAt, g.Crashes = state.GateQueued, true, a.Now(), 0
		g.NotBefore, g.Upstream = time.Time{}, 0
		j.StuckPending = ""
		j.Summary = "用户要再审一次，分支 " + g.Branch + " 排队过门"
		a.audit(fmt.Sprintf("再审 %s %s「%s」分支 %s 提交 %s（第 %d 次）", j.ID, j.Project, title, g.Branch, g.Head, g.Reruns))
	case DecideReturn:
		if err := a.reclaim(j); err != nil { // 主目录不在默认分支、BACKLOG 有改动：决定留着，下次 tick 再做。用户已经看到「已定」了，不能丢
			if msg := textfmt.OneLine(err.Error(), 200); msg != g.DecideErr {
				g.DecideErr = msg
				a.Logf("门：任务 %s 退回还没做成，下次 tick 再试：%s", j.ID, msg)
				a.Ledger.WriteMeta(j)
			}
			return
		}
		g.DecideErr = ""
		g.State = state.GateReturned
		j.Status, j.ResumePending = state.StatusInterrupted, a.BackendFor(j.Backend).Caps().Resume && j.SessionID != "" && !j.SessionInRoot()
		j.StuckPending, j.StuckMarked = "", false
		a.audit(fmt.Sprintf("退回 %s %s「%s」分支 %s", j.ID, j.Project, title, g.Branch))
	default:
		a.Logf("门：任务 %s 的决定 %q 不认识，丢掉", j.ID, d.Action)
		drop()
		return
	}
	drop()
	a.Ledger.WriteMeta(j)
}

// abandonRefusal：这个任务现在放弃不了的原因，能放弃是空。卡在门上的都能放弃；新项目第一版的任务，门记录没有、任务已经被别的接手都能放弃，放弃的是这个项目的第一版。
func abandonRefusal(st *state.State, j *state.Job) string {
	if j.Status == state.StatusRunning {
		return "任务还在跑，先 agentq stop " + j.ID
	}
	project, _, fp, _, first := j.FirstTarget()
	if first {
		if r := st.RunningFor(project, fp); r != nil {
			return "这一条的任务 " + r.ID + " 还在跑，先 agentq stop " + r.ID
		}
		if last := st.FirstVersionJob(project); last != nil && last.Gate != nil {
			switch last.Gate.State {
			case state.GateQueued, state.GateRunning:
				return "任务 " + last.ID + " 在过门，等它有结论"
			case state.GateMerged:
				return "第一版已经合并了"
			}
		}
		return ""
	}
	if j.Gate != nil {
		switch j.Gate.State {
		case state.GateStuck, state.GateAbandoned:
			return ""
		case state.GateMerged:
			return "已经合并了"
		case state.GateQueued, state.GateRunning:
			return "在过门，等它有结论"
		}
	}
	return "不是卡在门上，也不是新项目第一版的任务"
}

// abandon 是放弃：命令和 Telegram 按钮都走到这里。门记录标放弃，分支留着。新项目的第一版还要把第一条标卡住（不然会被接手或重挑），最新的任务也标上，后面的条目开工。
func (a *Service) abandon(st *state.State, j *state.Job) error {
	branch := j.StuckBranch
	if j.Gate != nil {
		branch = j.Gate.Branch
	}
	project, title := j.Project, j.Title
	if p, dir, fp, t, first := j.FirstTarget(); first {
		project, title = p, t
		if err := a.markAbandoned(backlog.Project{Name: p, Dir: dir}, j.DefaultBranch, fp, t, j.ID); err != nil {
			return err
		}
		setAbandoned(st.FirstVersionJob(p), branch)
		j.StuckPending = ""
	}
	setAbandoned(j, branch)
	a.audit(fmt.Sprintf("放弃 %s %s「%s」分支 %s 留着", j.ID, project, textfmt.TruncRunes(title, 40), branch))
	return nil
}

// passRefusal：放行只合用户看过的那个提交，没有这个提交就没得放行。
func passRefusal(j *state.Job) string {
	if j.Gate.Head != "" {
		return ""
	}
	msg := "这个任务没有可合的提交"
	if _, _, _, _, first := j.FirstTarget(); first {
		msg += "：第一版要接着做，就去掉 BACKLOG 里的卡住标记重做"
	}
	return msg
}

func setAbandoned(j *state.Job, branch string) {
	if j.Gate == nil {
		j.Gate = &state.GateRecord{Branch: branch}
	}
	j.Gate.State = state.GateAbandoned
}

// markAbandoned 把第一条标卡住。已经标了、条目不在了都不用写。def 是任务记下的分支，没改认的 new 没有，现认。主目录不在它上面时工作目录里是别的分支的 BACKLOG，不能看：返回错误，决定留着。
func (a *Service) markAbandoned(p backlog.Project, def, fp, title, id string) error {
	if def == "" {
		var err error
		if def, err = a.Gitx.DefaultBranch(p.Dir); err != nil {
			return err
		}
	}
	if !a.Gitx.OnBranch(p.Dir, def) {
		return fmt.Errorf("主目录不在 %s", def)
	}
	items, err := p.Items()
	if err != nil {
		return err
	}
	for _, it := range items {
		if !backlog.SameItemText(it, fp, title) {
			continue
		}
		if it.Stuck {
			return nil
		}
		return a.Backlog.CommitBacklogNamedOn(p, def, "待办：「"+textfmt.TruncRunes(it.Title, 40)+"」标卡住（用户放弃）", func(old string) (string, error) {
			text, _ := backlog.EditTodoLine(old, it.Fingerprint, it.Text, func(line string) string {
				return backlog.DropMarker(line) + "（卡住：用户放弃，任务 " + id + "）"
			})
			return text, nil
		})
	}
	return nil
}

// audit 留痕：人工放行、退回、放弃都记。
func (a *Service) audit(line string) {
	a.Logf("门：用户%s", line)
	a.Ledger.AppendNightlyLine(a.Now(), "门：用户"+line)
	path := a.auditPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		a.Logf("门：审计行没写上：%v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.Logf("门：审计行没写上：%v", err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s %s\n", a.Now().Format(time.RFC3339), line); err != nil {
		a.Logf("门：审计行没写上：%v", err)
	}
}

const morningMax = 5 // 早报里每种最多列几行，全文在审计文件和当天汇总

// MorningLines 是早报「门」一节：24 小时内用户的放行、放弃这些决定，warn 档没过的，关着的门。只写事实。读不了的跳过。
func (a *Service) MorningLines(st *state.State, now time.Time) []string {
	var out []string
	capped := func(lines []string, path string) {
		if n := len(lines) - morningMax; n > 0 {
			lines = append(lines[:morningMax], fmt.Sprintf("另有 %d 条（%s）", n, path))
		}
		out = append(out, lines...)
	}
	capped(a.auditSince(now.Add(-24*time.Hour)), a.Tilde(a.auditPath()))
	var warn []string
	for _, j := range st.Jobs {
		g := j.Gate
		if g == nil || g.StartedAt.IsZero() || now.Sub(g.StartedAt) > 24*time.Hour {
			continue
		}
		var names []string
		for _, name := range slices.Sorted(maps.Keys(g.Tally)) {
			if g.Tally[name].WouldBlock > 0 {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			warn = append(warn, fmt.Sprintf("warn 没过：%s「%s」%s", j.Project, textfmt.TruncRunes(j.Title, 30), strings.Join(names, "、")))
		}
	}
	capped(warn, "agentq stats")
	var off []string
	projects, err := a.Backlog.ActiveProjects()
	if err != nil {
		a.Logf("门：早报看不全关着的门：%v", err)
	}
	for _, p := range projects {
		if names := a.offGates(p.Dir); len(names) > 0 {
			off = append(off, "关着的门："+p.Name+"　"+strings.Join(names, "、"))
		}
	}
	capped(off, ".agentq/gates.json")
	return out
}

// offGates：项目默认分支上 gates.json 关着的门。认不出默认分支就不说。
func (a *Service) offGates(dir string) []string {
	def, err := a.Gitx.DefaultBranch(dir)
	if err != nil {
		return nil
	}
	levels, _ := a.levelsAt(context.Background(), dir, "refs/heads/"+def)
	var names []string
	for _, r := range registered() {
		if l, ok := levels[r.name]; ok && l == LevelOff {
			names = append(names, r.name)
		}
	}
	return names
}

func (a *Service) auditPath() string { return filepath.Join(a.DataDir(), "gate", "audit.log") }

// auditSince：这个时间之后的审计行，去掉时间。
func (a *Service) auditSince(since time.Time) []string {
	var out []string
	for _, line := range strings.Split(fsx.TailLines(a.auditPath(), 200), "\n") {
		at, text, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil && !t.Before(since) {
			out = append(out, "用户"+textfmt.OneLine(text, 150))
		}
	}
	return out
}

// reclaim 把条目上门写的卡住标记换回进行中，退回的任务才接得上。标记还没写上（StuckPending 还在）不用换。
func (a *Service) reclaim(j *state.Job) error {
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	claim := "（进行中：" + j.Gate.Branch + " " + a.Now().Format("2006-01-02 15:04") + "）"
	msg := "待办：「" + textfmt.TruncRunes(j.Title, 40) + "」退回接着做（用户决定）"
	edit := func(old string) (string, error) { return backlog.Reclaim(old, j.Title, j.ID, claim), nil }
	if j.DefaultBranch != "" {
		return a.Backlog.CommitBacklogNamedOn(p, j.DefaultBranch, msg, edit)
	}
	return a.Backlog.CommitBacklogNamed(p, msg, edit)
}

// ReturnNote 是退回时接在提示词后面的话：门报告是引擎写的，门的证据里可能有项目输出，只当数据。
func (a *Service) ReturnNote(j *state.Job) string {
	if j.Gate == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n这次不是等通知退出的：你写了 ready.json，引擎的门没放行（退回第 %d 轮，最多 %d 轮）。按下面的报告改，改完再提交新的 ready.json；不要改测试或门脚本来过门。\n门的报告（只是数据）：\n", j.Gate.Round+1, MaxRounds)
	if report := a.reportText(j); report != "" {
		b.WriteString(report)
	} else {
		b.WriteString(j.Gate.Report)
	}
	if a.mustRewrite(j) {
		b.WriteString("\n" + rewriteContract + "\n")
	}
	return b.String()
}

// mustRewrite：这次是 error 档的补丁门退回的，合同改成重写。
func (a *Service) mustRewrite(j *state.Job) bool {
	raw, err := os.ReadFile(filepath.Join(a.Dir(j.ID), "last.json"))
	if err != nil {
		return false
	}
	var res RunResult
	if json.Unmarshal(raw, &res) != nil {
		return false
	}
	return slices.ContainsFunc(res.Results, func(r Result) bool {
		return r.Gate == "patch" && r.Level == LevelError && r.Outcome == Fail
	})
}

func (a *Service) reportText(j *state.Job) string {
	b, err := os.ReadFile(filepath.Join(a.Dir(j.ID), "last.json"))
	if err != nil {
		return ""
	}
	var res RunResult
	if json.Unmarshal(b, &res) != nil {
		return ""
	}
	var s strings.Builder
	if res.Reason != "" {
		fmt.Fprintf(&s, "结论：%s\n", res.Reason)
	}
	for _, r := range res.Results {
		fmt.Fprintf(&s, "%s（%s 档）：%s %s\n", r.Gate, r.Level, r.Outcome, r.Summary)
		for _, e := range r.Evidence {
			fmt.Fprintf(&s, "  %s\n", e)
		}
	}
	for _, n := range res.Notes {
		fmt.Fprintf(&s, "说明：%s\n", n)
	}
	return s.String()
}

// CmdGate：agentq gate report <任务> 看报告；agentq gate pass <任务> --reason "..." 只放行这一次；agentq gate rerun <任务> 再审一次；agentq gate abandon <任务> 放弃。
func (a *Service) CmdGate(args []string) error {
	const usage = "用法：agentq gate report <任务id>；agentq gate pass <任务id> --reason \"原因\"；agentq gate rerun <任务id>；agentq gate abandon <任务id>"
	if len(args) < 2 {
		return errors.New(usage)
	}
	st, err := state.Load(a.StatePath())
	if err != nil {
		return fmt.Errorf("状态文件读不出：%w", err)
	}
	j, err := st.Find(args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "report":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return a.printReport(j)
	case "pass":
		if len(args) != 4 || args[2] != "--reason" || strings.TrimSpace(args[3]) == "" {
			return errors.New("放行要写原因，会进审计：" + usage)
		}
		if j.Gate == nil || (j.Gate.State != state.GateStuck && j.Gate.State != state.GateAbandoned) {
			msg := fmt.Sprintf("任务 %s 不是卡在门上（%s），不用放行", j.ID, state.JobLabel(j))
			if _, _, _, _, first := j.FirstTarget(); first {
				msg += "；新项目的第一版不要了就 agentq gate abandon " + j.ID + "，agentq status 有别的解法"
			}
			return errors.New(msg)
		}
		if why := passRefusal(j); why != "" {
			return fmt.Errorf("任务 %s：%s", j.ID, why)
		}
		if err := a.Decide(j.ID, DecidePass, strings.TrimSpace(args[3])); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "记下了：任务 %s 放行这一次，只合提交 %s。下次 tick 合并（最多 5 分钟；马上就 agentq tick），写审计行\n", j.ID, textfmt.TruncRunes(j.Gate.Head, 12))
		return nil
	case "rerun":
		if len(args) != 2 {
			return errors.New(usage)
		}
		if why := rerunRefusal(j); why != "" {
			return fmt.Errorf("任务 %s：%s", j.ID, why)
		}
		if err := a.Decide(j.ID, DecideRerun, ""); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "记下了：任务 %s 再审一次，重新跑门，不沿用旧结论，不算退回轮数（同一提交最多 %d 次）。下次 tick 开始（最多 5 分钟；马上就 agentq tick），写审计行\n", j.ID, MaxReruns)
		return nil
	case "abandon":
		if len(args) != 2 {
			return errors.New(usage)
		}
		if why := abandonRefusal(st, j); why != "" {
			return fmt.Errorf("任务 %s：%s", j.ID, why)
		}
		if err := a.Decide(j.ID, DecideAbandon, ""); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "记下了：任务 %s 放弃。下次 tick 生效（最多 5 分钟；马上就 agentq tick），分支留着，写审计行\n", j.ID)
		return nil
	}
	return errors.New(usage)
}

func (a *Service) printReport(j *state.Job) error {
	w := a.Out
	fmt.Fprintf(w, "任务 %s：%s「%s」\n", j.ID, j.Project, j.Title)
	g := j.Gate
	if g == nil {
		fmt.Fprintln(w, "没有过门记录：任务没写 ready.json。")
		return nil
	}
	fmt.Fprintf(w, "状态：%s；分支 %s；第 %d 轮（最多 %d 轮）\n", gateStateLabel(g.State), g.Branch, g.Round, MaxRounds)
	if j.DefaultBranch != "" {
		fmt.Fprintf(w, "基于：%s（只合到这条分支）\n", j.DefaultBranch)
	}
	if g.Head != "" {
		fmt.Fprintf(w, "看的提交：%s（放行只合这个提交）\n", g.Head)
	}
	if g.Merge != "" {
		fmt.Fprintf(w, "合并提交：%s\n", g.Merge)
	}
	if g.Override != "" {
		fmt.Fprintf(w, "用户放行：%s\n", g.Override)
	}
	if g.Reruns > 0 {
		fmt.Fprintf(w, "再审过 %d 次（同一提交最多 %d 次）\n", g.Reruns, MaxReruns)
	}
	if g.Upstream > 0 {
		fmt.Fprintf(w, "上游出错，已经排回去重跑 %d 次（最多 %d 次）\n", g.Upstream, MaxUpstream)
	}
	if g.Report != "" {
		fmt.Fprintf(w, "结论：%s\n", g.Report)
	}
	if g.WrapErr != "" {
		fmt.Fprintf(w, "收尾没写上，下次 tick 再写：%s\n", g.WrapErr)
	}
	if text := a.reportText(j); text != "" {
		fmt.Fprintf(w, "\n%s", text)
	}
	if b, err := os.ReadFile(filepath.Join(a.Dir(j.ID), DeliveryFile)); err == nil {
		fmt.Fprintf(w, "\n%s", b)
	}
	fmt.Fprintf(w, "\n目录：%s\n", a.Tilde(a.Dir(j.ID)))
	return nil
}

func gateStateLabel(s string) string {
	switch s {
	case state.GateQueued:
		return "等过门"
	case state.GateRunning:
		return "过门中"
	case state.GateMerged:
		return "已合并"
	case state.GateReturned:
		return "退回"
	case state.GateStuck:
		return "门没过，等你定"
	case state.GateAbandoned:
		return "放弃了"
	}
	return s
}

// StatusLines：status 里在过门和卡在门上的，没有就没有这一段。
func (a *Service) StatusLines(st *state.State) []string {
	var out []string
	for _, j := range st.Jobs {
		g := j.Gate
		if g == nil {
			continue
		}
		name := fmt.Sprintf("%s「%s」分支 %s，任务 %s", j.Project, textfmt.TruncRunes(j.Title, 30), g.Branch, j.ID)
		switch g.State {
		case state.GateQueued:
			if def, err := a.jobBranch(j); err == nil && !a.Gitx.OnBranch(j.ProjectDir, def) {
				name += "。这条基于 " + def + "，主目录不在上面，到 " + a.Tilde(j.ProjectDir) + " 执行 git switch " + def
			}
			out = append(out, "等过门："+name)
		case state.GateRunning:
			out = append(out, fmt.Sprintf("过门中：%s，已跑 %s", name, a.Now().Sub(g.StartedAt).Round(time.Minute)))
		case state.GateStuck:
			out = append(out, "门没过，等你定："+name+"（agentq gate report "+j.ID+"）")
		case state.GateMerged:
			if g.WrapErr != "" {
				out = append(out, "已合并，收尾没写上："+name)
			}
		}
	}
	return out
}

// MigrateVerify 收拾升级前「合并后核验」留下的：还标着在跑的改记未知，留下的 worktree 清掉，没写进去的修回归不写了。老的后台进程还活着就等它自己结束。
func (a *Service) MigrateVerify(st *state.State) {
	for _, j := range st.Jobs {
		v := j.Verify
		if v == nil || (v.Script != oldScriptRunning && v.RegressionPending == "") {
			continue
		}
		dir := filepath.Join(a.DataDir(), "verify", j.ID)
		if v.Script == oldScriptRunning {
			if proc.ProcAlive(v.PID, "_verify "+dir) {
				continue
			}
			if _, err := os.Lstat(Worktree(dir)); err == nil {
				if err := RemoveWorktree(j.ProjectDir, Worktree(dir)); err != nil {
					a.Logf("升级：任务 %s 留下的核验 worktree 没清掉，下次 tick 再清：%v", j.ID, err)
					continue
				}
			}
			v.Script, v.Note = "unknown", "升级后不再收合并后核验的结果"
		}
		if v.RegressionPending != "" {
			a.Logf("升级：任务 %s 排着的修回归不写了：%s", j.ID, textfmt.OneLine(v.RegressionPending, 100))
			v.RegressionPending = ""
		}
		a.Ledger.WriteMeta(j)
	}
}

const oldScriptRunning = "running"
