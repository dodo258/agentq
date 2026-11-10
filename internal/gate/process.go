package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// Service 是质量门的模块实例。SelfPath 非空时用它启动后台进程（测试进程自己不是 agentq），真实运行用 os.Executable。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Gitx    *gitx.Service
	Ledger  *ledger.Service
	Notify  *notify.Service
	// SelfPath 非空时用它启动后台进程。
	SelfPath string
}

const (
	// ProcToken 是后台进程的内部命令名，也是 tick 认进程的标记。
	ProcToken = "_gate"
	// ReadyFile 是任务提交到分支上的交付单。合并时引擎把它去掉，不进 main。
	ReadyFile = ".agentq/ready.json"
	// MaxRounds：同一任务最多退回几轮。
	MaxRounds = 2
	// maxCrashes：_gate 没留下结果就没了这么多次，算判不了。
	maxCrashes = 3
	// maxRebuilds：一次过门里 main 动过时最多重建几次候选，再动就排回队尾。
	maxRebuilds = 3
	// mergeLockWait：排队等 <项目>-main 锁最多多久。
	mergeLockWait = 5 * time.Minute
	// MaxUpstream：上游 5xx 时排回去重跑几次，间隔见 state.TransientBackoff（15 分钟、1 小时）。加上第一次一共调 3 次模型，再不行判不了。不多试：没花钱也要有个头。
	MaxUpstream = 2
	// MaxReruns：同一提交用户最多要几次再审。
	MaxReruns = 2
	// deadlineRuns：期限按几次超时算：测试门最多跑 4 次脚本，评测门 2 次（构建、评测），成品门 1 次，审查最多 2 次，界面门脚本 1 次加审查最多 2 次。
	deadlineRuns = 12
)

// 进程的结论。
const (
	outMerged  = "merged"  // 进 main 了
	outFail    = "fail"    // 门没过，或者合并有冲突：退回任务
	outUnknown = "unknown" // 判不了：不放行，等用户定
	outWait    = "wait"    // 门过了，主目录挡着：保留结论，下次 tick 重试
	outDefer   = "defer"   // 撞限、认证：过一会儿重跑
	outBypass  = "bypass"  // 分支已经在 main 上，不是引擎合的：没过门
	outHold    = "hold"    // 门没挡，改错了难撤回或影响面广又没有确定性检查：不合并，等用户拍板
)

// Plan 是 tick 交给后台进程的任务说明，在拿不到 state.json 的进程里够用。
type Plan struct {
	JobID       string `json:"job_id"`
	Project     string `json:"project"`
	ProjectDir  string `json:"project_dir"`
	Title       string `json:"title"`
	Item        string `json:"item"`
	Fingerprint string `json:"fingerprint"`
	Backend     string `json:"backend,omitempty"`
	Branch      string `json:"branch"`
	// DefaultBranch 是这条任务记下的分支：基点、候选、合并、收尾都走它。老任务没有，Process 开头现认一次，这次过门里不变。
	DefaultBranch string `json:"default_branch,omitempty"`
	// Head 是这次过门看的分支尖：分支被收尾删掉之后靠它认出已经合过了；用户放行时只合这个提交。
	Head     string `json:"head,omitempty"`
	Ready    Ready  `json:"ready"`
	Round    int    `json:"round,omitempty"`
	Override string `json:"override,omitempty"`
	// Product：要跑成品门，过了生成交付单。
	Product bool `json:"product,omitempty"`
	// Rerun：用户要的再审，不沿用旧结论。Upstream：前面因为上游 5xx 重跑了几次。
	Rerun    bool `json:"rerun,omitempty"`
	Upstream int  `json:"upstream,omitempty"`
	// Timeout 是每个脚本、每次审查的超时。
	Timeout string `json:"timeout"`
	// Scope、ScopeFrom：退回时定的范围和上一轮的分支尖，scope 门用。不是退回的轮次为空。
	Scope     []string `json:"scope,omitempty"`
	ScopeFrom string   `json:"scope_from,omitempty"`
}

// RunResult 是后台进程写的 result.json。tick 看到它就算这次过门结束了：worktree 已经清完。
type RunResult struct {
	Outcome string   `json:"outcome"`
	Reason  string   `json:"reason,omitempty"`
	Merge   string   `json:"merge,omitempty"`
	Base    string   `json:"base,omitempty"`
	Head    string   `json:"head,omitempty"`
	Tree    string   `json:"tree,omitempty"`
	Results []Result `json:"results,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	WrapErr string   `json:"wrap_err,omitempty"`
	// WaitBranch 非空：outWait 是因为主目录不在这个分支上，切过去就行。WaitChanges：是因为主目录有和这次改动重叠的未提交改动。都没有就是别的原因，提示里不让用户动手。
	WaitBranch  string `json:"wait_branch,omitempty"`
	WaitChanges bool   `json:"wait_changes,omitempty"`
	// Usage 是这次过门调模型花的，main 动过重建的那几次也算。
	Usage *backend.JobUsage `json:"usage,omitempty"`
	// Tally 是这次过门每道门跑的次数和用量，main 动过重建的那几次也算。
	Tally map[string]*state.GateTally `json:"tally,omitempty"`
	// Off 是这次关着的门。
	Off []string `json:"off,omitempty"`
	// Risk 是这次的风险档位和依据，建出候选之后才有。
	Risk *RiskVerdict `json:"risk,omitempty"`
	// Scope：没过（outFail）时引擎算的下一轮范围，见 scope.go。
	Scope []string `json:"scope,omitempty"`
}

// Dir 是这个任务过门的目录。
func (a *Service) Dir(id string) string { return filepath.Join(a.DataDir(), "gate", id) }

// Worktree 是过门目录里的一次性 worktree。
func Worktree(dir string) string { return filepath.Join(dir, "wt") }

func gateMark(id string) string { return "gate-" + id }

func (a *Service) selfPath() (string, error) {
	if a.SelfPath != "" {
		return a.SelfPath, nil
	}
	return os.Executable()
}

// git 不跑钩子：只是临时检出和建候选，不是用户的提交。返回标准输出，失败时错误里带标准错误。
func (a *Service) git(ctx context.Context, dir string, args ...string) (string, error) {
	return a.gitIn(ctx, dir, nil, args...)
}

func (a *Service) gitIn(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitWait)
	defer cancel()
	// quotepath 关掉：不然中文文件名输出成带引号的八进制，拿去查、去撤都对不上
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = gitx.GitEnv(a.Home)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	proc.BoundCmd(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimRight(string(out), "\n"), fmt.Errorf("git %s：%w %s", args[0], err, textfmt.OneLine(stderr.String(), 200))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (a *Service) deps() Deps {
	return Deps{Env: a.Env, Git: a.git, Script: RunScript, CLI: RunCLI, Self: a.selfPath}
}

// RunShell 是后台进程本身：agentq _gate <任务id>。结果写完才退出；被 SIGTERM 时清干净 worktree 就走，不写结果，tick 按进程没了重跑。
func (a *Service) RunShell(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "agentq _gate：内部命令，由 tick 在任务写了 ready.json 之后调用")
		return 2
	}
	dir := a.Dir(args[0])
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	var p Plan
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	timeout, terr := time.ParseDuration(p.Timeout)
	if err != nil || terr != nil || timeout <= 0 {
		fmt.Fprintf(stderr, "agentq _gate：读不了 %s：%v %v\n", filepath.Join(dir, "plan.json"), err, terr)
		return 2
	}
	lock, err := fsx.AcquireLock(filepath.Join(dir, "lock"), 0)
	if err != nil || lock == nil { // 上一个 tick 起的还在：它写了结果就算数
		return 0
	}
	defer lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	res := a.Process(ctx, dir, p, timeout)
	if ctx.Err() != nil {
		return 1
	}
	if err := writeResult(dir, res); err != nil {
		fmt.Fprintf(stderr, "%s 门的结果写不了（下次 tick 按进程没了处理）：%v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		return 1
	}
	lock.Close()
	stop()
	if a.Cfg.TickAfterJob { // 让排队的门和待做马上接上；结果已经写好，失败也只是等下一次定时 tick
		if self, err := os.Executable(); err == nil {
			err = syscall.Exec(self, []string{self, "tick", "--after", gateMark(p.JobID)}, os.Environ())
			fmt.Fprintf(stderr, "%s 过门结束后执行 agentq tick 失败（等下次定时 tick）：%v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		}
	}
	return 0
}

func writeResult(dir string, res RunResult) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(dir, "result.json"), append(b, '\n'))
}

// Process 走一次过门：建候选、跑门、合并。main 中途动过就用新 main 重建。不管怎么结束，返回前都清掉 worktree。
func (a *Service) Process(ctx context.Context, dir string, p Plan, timeout time.Duration) RunResult {
	if p.DefaultBranch == "" { // 只有升级前排队的老任务
		def, err := a.Gitx.DefaultBranch(p.ProjectDir)
		if err != nil {
			why := textfmt.OneLine(err.Error(), 200)
			return RunResult{Outcome: outUnknown, Reason: why, Results: []Result{{Gate: "merge", Outcome: Unknown, Summary: why, Level: LevelError}}}
		}
		p.DefaultBranch = def
	}
	wt := Worktree(dir)
	defer func() {
		if err := RemoveWorktree(p.ProjectDir, wt); err != nil {
			a.Logf("门：任务 %s 的 worktree 没清干净，下次 tick 再清：%v", p.JobID, err)
		}
	}()
	var last RunResult
	var usage *backend.JobUsage
	tally := map[string]*state.GateTally{}
	for attempt := 0; attempt < maxRebuilds; attempt++ {
		if err := RemoveWorktree(p.ProjectDir, wt); err != nil {
			return RunResult{Outcome: outUnknown, Reason: "上次的 worktree 清不掉：" + textfmt.OneLine(err.Error(), 200), Usage: usage, Tally: tally}
		}
		c, res, ok := a.build(ctx, dir, p)
		if !ok {
			if res.Outcome == outFail {
				res.Scope = a.returnScope(ctx, p.ProjectDir, res.Head, res.Results, nil)
			}
			addTally(tally, res.Results)
			res.Usage, res.Tally = usage, tally
			return res
		}
		c.Timeout, c.Fresh, c.Upstream = timeout, p.Rerun, p.Upstream
		risk := a.classify(ctx, c)
		c.Risk, c.Light = risk.Level, risk.Light
		results, notes, off, blocked := a.runGates(ctx, dir, p, c)
		addTally(tally, results)
		for i := range results {
			usage = backend.AddUsage(usage, results[i].Usage)
			results[i].Usage = nil
		}
		last = RunResult{Base: c.Base, Head: c.Head, Tree: c.Tree, Results: results, Notes: notes, Usage: usage, Tally: tally, Off: off, Risk: risk}
		if blocked != "" {
			if blocked == outFail {
				last.Scope = a.returnScope(ctx, p.ProjectDir, c.Merge, results, c.Files)
			}
			last.Outcome, last.Reason = blocked, summarize(results)
			if d := deferred(results); d != "" {
				last.Reason = d
			}
			return last
		}
		if hold := holdReason(p, risk, results); hold != "" {
			last.Outcome, last.Reason = outHold, hold
			return last
		}
		out, moved := a.integrate(ctx, dir, p, c)
		if moved {
			last.Outcome, last.Reason = outWait, p.DefaultBranch+" 一直在动，排回队尾"
			continue
		}
		last.Outcome, last.Reason, last.Merge, last.WrapErr = out.Outcome, out.Reason, out.Merge, out.WrapErr
		last.WaitBranch, last.WaitChanges = out.WaitBranch, out.WaitChanges
		return last
	}
	return last
}

// addTally 记一次过门每道门的结论。放行不跑门，单算；这个 tree 上次的结果没跑，不算。
func addTally(tally map[string]*state.GateTally, results []Result) {
	for _, r := range results {
		if r.Cached || r.Gate == overrideGate {
			continue
		}
		t := tally[r.Gate]
		if t == nil {
			t = &state.GateTally{}
			tally[r.Gate] = t
		}
		switch {
		case r.Defer != "":
			t.Deferred++
		case r.Outcome == Pass:
			t.Pass++
		case r.Outcome == Fail:
			t.Fail++
		default:
			t.Unknown++
		}
		if r.Defer == "" && r.Level == LevelWarn && r.Outcome != Pass {
			t.WouldBlock++
		}
		t.Usage = backend.AddUsage(t.Usage, r.Usage)
	}
}

func deferred(results []Result) string {
	for _, r := range results {
		if r.Defer != "" {
			return r.Defer
		}
	}
	return ""
}

// build 在一次性 worktree 里把任务分支合进最新 main，得到候选提交 M。ready.json 不进 M。
// 第三个返回值为假时第二个是结论：有冲突算失败、分支没了判不了、已经合过了就只补收尾。
func (a *Service) build(ctx context.Context, dir string, p Plan) (Candidate, RunResult, bool) {
	head := p.Head
	fail := func(outcome, why string) (Candidate, RunResult, bool) {
		return Candidate{}, RunResult{Outcome: outcome, Reason: why, Head: head, Results: []Result{{Gate: "merge", Outcome: outcomeOf(outcome), Summary: why, Level: LevelError}}}, false
	}
	conflict := func(why string, files []string) (Candidate, RunResult, bool) {
		c, res, ok := fail(outFail, why)
		res.Results[0].Files = files
		return c, res, ok
	}
	def := p.DefaultBranch
	base, err := a.git(ctx, p.ProjectDir, "rev-parse", "--verify", "refs/heads/"+def)
	if err != nil {
		return fail(outUnknown, "这条任务基于 "+def+" 分支，查不到了，不换分支合："+err.Error())
	}
	if merge, ok := a.engineMerged(ctx, dir, p); ok {
		// 上一个门进程合并后、写结果前被杀了：不再合并，补收尾
		res := RunResult{Outcome: outMerged, Merge: merge, Base: base, Head: head, Reason: "上一个门进程已经合并，补收尾"}
		if tree, err := a.git(ctx, p.ProjectDir, "rev-parse", merge+"^{tree}"); err == nil { // 门的结果它没写出去：按 tree 留着，报告和交付单要用
			if b, err := os.ReadFile(passedPath(dir, tree)); err == nil {
				var rec passedRecord
				if json.Unmarshal(b, &rec) == nil {
					res.Results, res.Notes, res.Off, res.Tree = rec.Results, rec.Notes, rec.Off, tree
				}
			}
		}
		if err := a.WrapUp(p, merge); err != nil {
			res.WrapErr = textfmt.OneLine(err.Error(), 300)
		}
		return Candidate{}, res, false
	}
	if p.Override == "" || p.Head == "" { // 用户放行只合他看到的那个提交，之后又提交的不带
		tip, err := a.git(ctx, p.ProjectDir, "rev-parse", "--verify", "refs/heads/"+p.Branch)
		if err == nil {
			head = tip
		} else if !a.onBranch(p.ProjectDir, def, p.Head) { // 分支没了又不在默认分支上：可能是用户扔掉的，记下的提交不能再合
			return fail(outUnknown, "任务分支 "+p.Branch+" 查不了："+err.Error())
		}
	}
	if a.onBranch(p.ProjectDir, def, head) {
		if p.Override != "" { // 用户看过了，认这个合并：只补收尾
			merge := a.mergeOf(ctx, p.ProjectDir, def, head)
			res := RunResult{Outcome: outMerged, Merge: merge, Base: base, Head: head, Reason: "用户放行：分支已经在 " + def + " 上"}
			if err := a.WrapUp(p, merge); err != nil {
				res.WrapErr = textfmt.OneLine(err.Error(), 300)
			}
			return Candidate{}, res, false
		}
		return Candidate{}, RunResult{Outcome: outBypass, Base: base, Head: head, Reason: bypassReason(def, head)}, false
	}
	if _, err := a.git(ctx, p.ProjectDir, "cat-file", "-e", head+"^{commit}"); err != nil {
		return fail(outUnknown, "任务分支 "+p.Branch+" 的提交查不了："+err.Error())
	}
	wt := Worktree(dir)
	if _, err := a.git(ctx, p.ProjectDir, "worktree", "add", "--detach", "--quiet", wt, base); err != nil {
		return fail(outUnknown, "建 worktree 失败："+err.Error())
	}
	ident := a.identity(ctx, p.ProjectDir)
	if _, err := a.git(ctx, wt, append(ident, "merge", "--no-ff", "--no-commit", "--no-edit", head)...); err != nil {
		if ctx.Err() != nil {
			return fail(outUnknown, "门进程被停了")
		}
		names, ferr := a.git(ctx, wt, "diff", "--name-only", "--diff-filter=U")
		if ferr != nil || names == "" {
			return fail(outUnknown, "合并没成："+err.Error())
		}
		// 默认分支上残留的 ready.json 和任务的撞了不算冲突：两份都不进 M，下面就删
		conflicts := slices.DeleteFunc(strings.Split(names, "\n"), func(f string) bool { return f == ReadyFile })
		if len(conflicts) > 0 {
			return conflict("和最新 "+def+" 有冲突："+textfmt.OneLine(strings.Join(conflicts, "、"), 300)+"；从最新 "+def+" 合入，解决后再写 ready.json", conflicts)
		}
	}
	if _, err := a.git(ctx, wt, "rm", "-q", "-f", "--ignore-unmatch", ReadyFile); err != nil {
		return fail(outUnknown, "去掉 "+ReadyFile+" 失败："+err.Error())
	}
	if _, err := a.gitIn(ctx, wt, []byte(a.mergeMessage(ctx, p, base, head)), append(ident, "commit", "-q", "--allow-empty", "-F", "-")...); err != nil {
		return fail(outUnknown, "提交候选失败："+err.Error())
	}
	merge, err := a.git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return fail(outUnknown, "查候选失败："+err.Error())
	}
	tree, err := a.git(ctx, wt, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return fail(outUnknown, "查候选失败："+err.Error())
	}
	names, err := a.git(ctx, wt, "diff", "--name-only", "--no-renames", base, merge)
	if err != nil {
		return fail(outUnknown, "查改动失败："+err.Error())
	}
	var files []string
	if names != "" {
		for _, f := range strings.Split(names, "\n") {
			if f != ReadyFile { // 默认分支上残留的那份由 M 顺手删掉，不算任务的改动
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 && p.Override == "" {
		return fail(outFail, "任务分支相对 "+def+" 没有改动：改动没提交，或者这一条不用改。没提交就提交；核实后确实不用改的，按 agent-task 把进行中标记换成（卡住：不用改：原因），不写 ready.json")
	}
	return Candidate{JobID: p.JobID, Project: p.Project, ProjectDir: p.ProjectDir, Title: p.Title, Item: p.Item, Backend: p.Backend, Branch: p.Branch, DefaultBranch: def,
		Head: head, Base: base, Merge: merge, Tree: tree, PatchID: a.patchID(ctx, wt, base, merge), Dir: wt, WorkDir: dir, Files: files,
		Type: p.Ready.Type, Ready: p.Ready, Round: p.Round, Product: p.Product, Scope: p.Scope, ScopeFrom: p.ScopeFrom}, RunResult{}, true
}

func outcomeOf(out string) Outcome {
	if out == outFail {
		return Fail
	}
	return Unknown
}

// identity：用户没配 git 身份时给候选提交一个，不然提交不了。
func (a *Service) identity(ctx context.Context, dir string) []string {
	if out, err := a.git(ctx, dir, "config", "user.email"); err == nil && out != "" {
		return []string{"-c", "commit.gpgsign=false"}
	}
	return []string{"-c", "commit.gpgsign=false", "-c", "user.name=agentq", "-c", "user.email=agentq@localhost"}
}

// mergeMessage：任务的提交说明带进合并提交：质量统计靠它认返工，也要留得下跑了哪些验证。
func (a *Service) mergeMessage(ctx context.Context, p Plan, base, head string) string {
	body, _ := a.git(ctx, p.ProjectDir, "log", "--format=%B", base+".."+head)
	body = textfmt.TruncRunes(strings.TrimSpace(body), 3000)
	msg := "合并 " + textfmt.OneLine(p.Title, 60) + "\n\n分支 " + p.Branch + "，由 agentq 过门后合并。\n\n" + body + "\n"
	if slices.Contains(ReadyTypes, p.Ready.Type) { // ready.json 不进默认分支，补丁门和 health 靠这一行认修复和重写
		msg += "\n" + TypeTrailer + ": " + p.Ready.Type + "\n"
	}
	return msg
}

// patchID：任务改动的指纹，算不出返回空，审查就不沿用。
func (a *Service) patchID(ctx context.Context, wt, base, merge string) string {
	diff, err := a.git(ctx, wt, "diff", "--binary", base, merge)
	if err != nil || diff == "" {
		return ""
	}
	// --verbatim 不忽略空白：Python、YAML 里改缩进就是改语义，不能沿用旧结论。老版本 git 没有这个参数：报错，返回空，只是不沿用
	out, err := a.gitIn(ctx, wt, []byte(diff+"\n"), "patch-id", "--verbatim")
	if err != nil {
		return ""
	}
	id, _, _ := strings.Cut(out, " ")
	return id
}

// jobBranch：任务记下的分支。老任务没有才现认。
func (a *Service) jobBranch(j *state.Job) (string, error) {
	if j.DefaultBranch != "" {
		return j.DefaultBranch, nil
	}
	return a.Gitx.DefaultBranch(j.ProjectDir)
}

// OnJobBranch：提交已经是这条任务那条分支的祖先。认不出算不在。
func (a *Service) OnJobBranch(j *state.Job, commit string) bool {
	def, err := a.jobBranch(j)
	if err != nil {
		a.Logf("门：%v", err)
		return false
	}
	return a.onBranch(j.ProjectDir, def, commit)
}

// onBranch：提交已经是 branch 的祖先。
func (a *Service) onBranch(dir, branch, commit string) bool {
	if commit == "" {
		return false
	}
	_, err := a.git(context.Background(), dir, "merge-base", "--is-ancestor", commit, "refs/heads/"+branch)
	return err == nil
}

func bypassReason(def, head string) string {
	return "任务分支 " + textfmt.TruncRunes(head, 12) + " 已经在 " + def + " 上，不是引擎合的，没过门"
}

// mergingIntent 是合并前落的记录：之后看到分支在 main 上，靠它分清是引擎合的还是别人合的。
type mergingIntent struct {
	Merge string `json:"merge"`
	Head  string `json:"head"`
}

func intentPath(dir string) string { return filepath.Join(dir, "merging.json") }

// engineMerged：上一个门进程记下要合的候选，它已经在默认分支上。
func (a *Service) engineMerged(ctx context.Context, dir string, p Plan) (string, bool) {
	b, err := os.ReadFile(intentPath(dir))
	if err != nil {
		return "", false
	}
	var in mergingIntent
	if json.Unmarshal(b, &in) != nil || !gitx.SafeCommitRe.MatchString(in.Merge) || !a.onBranch(p.ProjectDir, p.DefaultBranch, in.Merge) {
		return "", false
	}
	return in.Merge, true
}

// mergeOf 找把分支尖带进默认分支的那个提交：沿它的第一父走，最早包含它的。找不到就用分支尖。
func (a *Service) mergeOf(ctx context.Context, dir, def, head string) string {
	out, err := a.git(ctx, dir, "rev-list", "--ancestry-path", "--merges", "--reverse", head+"..refs/heads/"+def)
	if err != nil || out == "" {
		return head
	}
	first, _, _ := strings.Cut(out, "\n")
	return first
}

// passedRecord 记下这个 tree 上门都过了：主目录挡着合并时下次不用再跑一遍。
type passedRecord struct {
	Results []Result `json:"results"`
	Notes   []string `json:"notes,omitempty"`
	Off     []string `json:"off,omitempty"`
}

// overrideGate 是用户放行时结果里的那一项：不是一道门。
const overrideGate = "override"

func passedPath(dir, tree string) string { return filepath.Join(dir, "passed-"+tree+".json") }

// runGates 依次跑各门。返回结果、说明、关着的门和挡住合并的结论（outFail、outUnknown、outDefer，空表示放行）。
// error 档的门没过就不再跑后面的：审查要花额度，测试都没过不用审。
func (a *Service) runGates(ctx context.Context, dir string, p Plan, c Candidate) ([]Result, []string, []string, string) {
	if p.Override != "" {
		return []Result{{Gate: overrideGate, Outcome: Pass, Summary: "用户放行：" + p.Override, Level: LevelError}}, nil, nil, ""
	}
	if b, err := os.ReadFile(passedPath(dir, c.Tree)); err == nil && !p.Rerun {
		var rec passedRecord
		if json.Unmarshal(b, &rec) == nil {
			for i := range rec.Results {
				rec.Results[i].Cached = true
			}
			return rec.Results, append(rec.Notes, "这个 tree 的门上次都过了，没再跑"), rec.Off, ""
		}
	}
	levels, notes := a.levelsAt(ctx, p.ProjectDir, c.Base)
	deps := a.deps()
	var results []Result
	var off []string
	for _, reg := range registered() {
		lvl := levelOf(reg.name, levels)
		if c.Risk == RiskBroad && lvl == LevelWarn && slices.Contains(deterministicGates, reg.name) {
			lvl = LevelError
			notes = append(notes, reg.name+" 门这次按 error 算：影响面广")
		}
		if lvl == LevelOff {
			notes = append(notes, reg.name+" 门关着")
			off = append(off, reg.name)
			continue
		}
		g := reg.build(deps)
		if !g.AppliesTo(c) {
			notes = append(notes, reg.name+" 门不适用这次改动")
			continue
		}
		c.Prior = results
		if err := a.resetWorktree(ctx, c); err != nil {
			results = append(results, Result{Gate: reg.name, Outcome: Unknown, Summary: "还原 worktree 失败：" + err.Error(), Level: lvl})
			if lvl == LevelError {
				return results, notes, off, outUnknown
			}
			continue
		}
		r := g.Run(ctx, c)
		r.Gate, r.Level = reg.name, lvl
		results = append(results, r)
		if r.Defer != "" {
			return results, notes, off, outDefer
		}
		if lvl == LevelError && r.Outcome != Pass {
			if r.Outcome == Fail {
				return results, notes, off, outFail
			}
			return results, notes, off, outUnknown
		}
	}
	saved := slices.Clone(results)
	for i := range saved {
		saved[i].Usage = nil // 用量只算一次
	}
	if b, err := json.Marshal(passedRecord{saved, notes, off}); err == nil {
		if err := fsx.WriteFileAtomic(passedPath(dir, c.Tree), b); err != nil {
			a.Logf("门：任务 %s 记不下过门结果：%v", p.JobID, err)
		}
	}
	return results, notes, off, ""
}

// resetWorktree 把一次性 worktree 还原成候选提交：上一道门可能改了文件。被忽略的文件（构建缓存）留着。
func (a *Service) resetWorktree(ctx context.Context, c Candidate) error {
	if _, err := a.git(ctx, c.Dir, "reset", "-q", "--hard", c.Merge); err != nil {
		return err
	}
	_, err := a.git(ctx, c.Dir, "clean", "-q", "-f", "-d")
	return err
}

// integrate 拿 <项目>-main 锁合并：main 还在基点就快进，在同一把锁内写收尾。第二个返回值为真表示 main 动过，要重建。
func (a *Service) integrate(ctx context.Context, dir string, p Plan, c Candidate) (RunResult, bool) {
	unlock, err := a.lockMain(ctx, p.Project)
	if err != nil {
		return RunResult{Outcome: outWait, Reason: textfmt.OneLine(err.Error(), 200)}, false
	}
	defer unlock()
	def := p.DefaultBranch
	cur, err := a.git(ctx, p.ProjectDir, "rev-parse", "--verify", "refs/heads/"+def)
	if err != nil {
		return RunResult{Outcome: outWait, Reason: "查不了 " + def + "：" + textfmt.OneLine(err.Error(), 200)}, false
	}
	if cur != c.Base {
		return RunResult{}, true
	}
	if br, err := a.git(ctx, p.ProjectDir, "symbolic-ref", "--short", "HEAD"); err != nil || br != def {
		return RunResult{Outcome: outWait, WaitBranch: def, Reason: "主目录不在 " + def + "（" + textfmt.OneLine(br, 60) + "）"}, false
	}
	intent, err := json.Marshal(mergingIntent{Merge: c.Merge, Head: c.Head})
	if err == nil {
		err = fsx.WriteFileAtomic(intentPath(dir), intent)
	}
	if err != nil {
		return RunResult{Outcome: outWait, Reason: "记不下要合并的提交，这次不合：" + textfmt.OneLine(err.Error(), 200)}, false
	}
	// 这一步跑用户的钩子：它是真的合并。不碰和这次改动没重叠的未提交改动，有重叠 git 会拒绝。
	out, err := a.Gitx.Git(p.ProjectDir, "merge", "--ff-only", "--quiet", c.Merge)
	if err != nil && a.onBranch(p.ProjectDir, def, c.Merge) { // 默认分支已经前进，报错的是合并之后的钩子（比如跑超时）：合并算数
		a.Logf("门：任务 %s 已经进 %s，合并命令报错：%s", p.JobID, def, textfmt.OneLine(err.Error()+" "+out, 300))
		err = nil
	}
	if err != nil {
		if rerr := os.Remove(intentPath(dir)); rerr != nil {
			a.Logf("门：任务 %s 的合并记录删不掉：%v", p.JobID, rerr)
		}
		if now, nerr := a.git(ctx, p.ProjectDir, "rev-parse", "--verify", "refs/heads/"+def); nerr == nil && now != c.Base {
			return RunResult{}, true // 不走锁的提交抢在前面了
		}
		return RunResult{Outcome: outWait, WaitChanges: a.dirtyOverlap(ctx, p.ProjectDir, c.Files), Reason: "主目录挡着合并：" + textfmt.OneLine(out, 300)}, false
	}
	res := RunResult{Outcome: outMerged, Merge: c.Merge}
	if err := a.writeRecords(p, c.Merge); err != nil {
		res.WrapErr = textfmt.OneLine(err.Error(), 300)
		return res, false
	}
	a.dropBranch(p)
	return res, false
}

// dirtyOverlap：主目录没提交的改动（含未跟踪的文件）里有这次要合进来的文件。git 的报错文字随语言变，不靠它。
func (a *Service) dirtyOverlap(ctx context.Context, dir string, files []string) bool {
	out, err := a.git(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return false
	}
	for _, entry := range strings.Split(out, "\x00") {
		if len(entry) > 3 && entry[2] == ' ' {
			entry = entry[3:] // 改名那一项的第二段是旧名字，没有状态前缀
		}
		if slices.Contains(files, entry) {
			return true
		}
	}
	return false
}

// lockMain 排队拿 <项目>-main 锁，等的时候也听得见 SIGTERM。
func (a *Service) lockMain(ctx context.Context, project string) (func(), error) {
	deadline := time.Now().Add(mergeLockWait)
	for {
		unlock, err := a.Backlog.LockMain(project+"-main", 2*time.Second)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return unlock, err
		}
	}
}

// WrapUp 写收尾：BACKLOG 移到「已完成」、PROGRESS 记一句，再删任务的 worktree 和分支。可以重复调用，上次没写上的 tick 用它补。
func (a *Service) WrapUp(p Plan, merge string) error {
	unlock, err := a.Backlog.LockMain(p.Project+"-main", backlog.MainLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.writeRecords(p, merge); err != nil {
		return err
	}
	a.dropBranch(p)
	return nil
}

// writeRecords 在锁内更新 BACKLOG 和 PROGRESS，提交到一个提交里。已经写过就什么都不做。BACKLOG 没了不算错：没东西要更新。
func (a *Service) writeRecords(p Plan, merge string) error {
	project := backlog.Project{Name: p.Project, Dir: p.ProjectDir}
	// 主目录切走了：工作目录和 HEAD 上是别的分支的 BACKLOG，里面没有这一条，下面会当写过了。等切回来再写。
	if p.DefaultBranch != "" && !a.Gitx.OnBranch(p.ProjectDir, p.DefaultBranch) {
		return fmt.Errorf("主目录不在 %s", p.DefaultBranch)
	}
	raw, err := os.ReadFile(project.BacklogPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	short := textfmt.TruncRunes(merge, 12)
	done := backlog.MergedLine{Commit: short, Acceptance: backlog.CleanTodoText(p.Ready.Acceptance, 300)}
	target := func(text string) string { return backlog.MarkMerged(text, p.Fingerprint, p.Title, p.Branch, done) }
	// 写过没写过按提交算，不按工作区：提交时被杀（比如 pre-commit 跑到一半），文件改了却没提交，不能当写过了，留着还挡后面的 BACKLOG 写入
	rel, err := backlog.BacklogRepoPath(project)
	if err != nil {
		return err
	}
	ctx := context.Background()
	committed, err := a.git(ctx, p.ProjectDir, "show", "HEAD:"+rel)
	if err != nil {
		return fmt.Errorf("读提交里的 BACKLOG：%w", err)
	}
	if target(committed+"\n") == committed+"\n" {
		return nil
	}
	if strings.TrimRight(string(raw), "\n") == strings.TrimRight(target(committed+"\n"), "\n") { // 上次写了没提交上：撤回重来
		if _, err := a.git(ctx, p.ProjectDir, "checkout", "-q", "HEAD", "--", rel); err != nil {
			return fmt.Errorf("撤回上次没提交的 BACKLOG：%w", err)
		}
		if b, err := os.ReadFile(filepath.Join(p.ProjectDir, "PROGRESS.md")); err == nil && strings.Contains(string(b), "（"+short+"）") {
			if _, err := a.git(ctx, p.ProjectDir, "checkout", "-q", "HEAD", "--", "PROGRESS.md"); err != nil {
				return fmt.Errorf("撤回上次没提交的 PROGRESS：%w", err)
			}
		}
	}
	progressPath := filepath.Join(p.ProjectDir, "PROGRESS.md")
	var progressOld []byte
	extra := []string{}
	restore := func() {
		if progressOld != nil {
			_ = fsx.WriteKeepMode(progressPath, progressOld)
		}
	}
	if a.progressWritable(p.ProjectDir) {
		if old, err := os.ReadFile(progressPath); err == nil {
			line := a.Now().Format("2006-01-02") + " " + backlog.CleanTodoText(p.Ready.Progress, 200) + "（" + short + "）"
			if text := noteProgress(string(old), line, short); text != string(old) {
				if err := fsx.WriteKeepMode(progressPath, []byte(text)); err != nil {
					return err
				}
				progressOld = old
				extra = append(extra, "PROGRESS.md")
			}
		}
	}
	edit := func(old string) (string, error) { return target(old), nil }
	msg := "待办：「" + textfmt.TruncRunes(p.Title, 40) + "」已合并 " + short
	if p.DefaultBranch != "" {
		err = a.Backlog.CommitBacklogOn(project, p.DefaultBranch, msg, edit, extra...)
	} else { // 老任务 tick 补写收尾：现认
		err = a.Backlog.CommitBacklogLocked(project, msg, edit, extra...)
	}
	if err != nil {
		restore()
		return err
	}
	// 上面看过 HEAD 到真正提交之间，用户可能切了分支：记录落到别的分支上，这边条目还标着进行中。看默认分支上的，没写上就报错，下次 tick 补写
	if p.DefaultBranch != "" {
		landed, err := a.git(ctx, p.ProjectDir, "show", "refs/heads/"+p.DefaultBranch+":"+rel)
		if err != nil {
			return fmt.Errorf("读 %s 上的 BACKLOG：%w", p.DefaultBranch, err)
		}
		if target(landed+"\n") != landed+"\n" {
			return fmt.Errorf("提交时主目录切走了，记录没落在 %s 上", p.DefaultBranch)
		}
	}
	return nil
}

// progressWritable：有 PROGRESS.md、进了版本库、没有没提交的改动。不满足就不写：别人的交接单不能被我们顺带提交掉。
func (a *Service) progressWritable(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, "PROGRESS.md"))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	ctx := context.Background()
	if _, err := a.git(ctx, dir, "ls-files", "--error-unmatch", "--", "PROGRESS.md"); err != nil {
		return false
	}
	out, err := a.git(ctx, dir, "status", "--porcelain", "--", "PROGRESS.md")
	return err == nil && out == ""
}

const (
	progressSection = "## 最近合并"
	progressKeep    = 8
)

// noteProgress 往「最近合并」一节最前面加一行，只留最近几条（PROGRESS 要短）。这一节 agentq 自己管，别的节不动。已经写过（提交号一样）就不写。
func noteProgress(text, line, commit string) string {
	if strings.Contains(text, "（"+commit+"）") {
		return text
	}
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == progressSection {
			at = i
			break
		}
	}
	if at < 0 {
		return strings.TrimRight(text, "\n") + "\n\n" + progressSection + "\n- " + line + "\n"
	}
	end := at + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "- ") {
		end++
	}
	old := lines[at+1 : end]
	if len(old) >= progressKeep {
		old = old[:progressKeep-1]
	}
	out := append(append([]string{}, lines[:at+1]...), "- "+line)
	out = append(out, old...)
	return strings.Join(append(out, lines[end:]...), "\n")
}

// dropBranch 删任务的 worktree 和分支。worktree 里有没提交的东西就都留着，不强删。
func (a *Service) dropBranch(p Plan) {
	ctx := context.Background()
	ref := "refs/heads/" + p.Branch
	if list, err := a.git(ctx, p.ProjectDir, "worktree", "list", "--porcelain"); err == nil {
		if wt := gitx.WorktreeOf(list, ref); wt != "" {
			if _, err := a.git(ctx, p.ProjectDir, "worktree", "remove", wt); err != nil {
				a.Logf("门：任务 %s 的 worktree %s 有没提交的东西或删不了，留着，分支 %s 也留着：%v", p.JobID, wt, p.Branch, err)
				return
			}
		}
	}
	if _, err := a.git(ctx, p.ProjectDir, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return
	}
	if _, err := a.git(ctx, p.ProjectDir, "branch", "-q", "-d", p.Branch); err != nil {
		a.Logf("门：任务 %s 的分支 %s 没删掉：%v", p.JobID, p.Branch, err)
	}
}

// PlanOf 是这个任务交给门进程的说明。
func PlanOf(a *Service, j *state.Job) Plan {
	return Plan{JobID: j.ID, Project: j.Project, ProjectDir: j.ProjectDir, Title: j.Title, Item: j.Item, Fingerprint: j.Fingerprint, Backend: j.Backend,
		DefaultBranch: j.DefaultBranch, Branch: j.Gate.Branch, Head: j.Gate.Head, Ready: j.Gate.Ready, Round: j.Gate.Round, Override: j.Gate.Override, Product: j.NewReport != nil || backlog.ProductItem(j.Item), Rerun: j.Gate.Rerun, Upstream: j.Gate.Upstream,
		Timeout: a.Cfg.VerifyTimeout.Duration.String(), Scope: j.GateScope, ScopeFrom: j.GateScopeFrom}
}
