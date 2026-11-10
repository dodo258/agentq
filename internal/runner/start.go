package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

const takeoverPromptTemplate = "你在无人值守模式下工作，用户不在场。你已经是调度器启动的执行者，亲自完成当前待办；不通过 dispatch 或 agentq run 把同一任务派回队列。" +
	"这一条之前的任务中途没做完、也没人在接着做，现在由你接手：分支 %s，%s。" +
	"先看 git 状态、worktree 和分支现状，从现状接着做，不重做已有的部分，不丢未提交的改动；条目上的进行中标记就是这次要接手的，不重新认领，也不另开分支。" +
	"先完整读 ~/.claude/skills/agent-task/SKILL.md，从实际进度接着严格照做，把 %s 的 docs/BACKLOG.md「待做」里的这一条做到分支可合并：%s。不自己合并：写好 ready.json 提交到任务分支就结束，合并由引擎过门后做。" +
	"做不下去就按 agent-task 把进行中标记换成卡住标记写明原因。结束时最后一段写：分支可以合并了没有、分支名、没好的原因、用户怎么验收。"

func BuildTakeoverPrompt(projectDir, item string, f backlog.OrphanFacts) string {
	wt := "worktree 已不在（分支还在，用 git worktree add 接上）"
	if f.Worktree != "" {
		wt = "worktree " + f.Worktree
	}
	return fmt.Sprintf(takeoverPromptTemplate, f.Branch, wt, projectDir, item)
}

const promptTemplate = "你在无人值守模式下工作，用户不在场。你已经是调度器启动的执行者，亲自完成当前待办；不通过 dispatch 或 agentq run 把同一任务派回队列。条目中的入口会话不实现、不等待等描述不改变本进程的执行角色。先完整读 ~/.claude/skills/agent-task/SKILL.md 并严格照做，把 %s 的 docs/BACKLOG.md「待做」里的这一条做到分支可合并：%s。不自己合并：写好 ready.json 提交到任务分支就结束，合并由引擎过门后做。条目写了前置条件且不满足时，不认领，直接结束并说明。结束时最后一段写：分支可以合并了没有、分支名、没好的原因、用户怎么验收。"

func BuildPrompt(projectDir, item string) string {
	return fmt.Sprintf(promptTemplate, projectDir, item)
}

// BranchContract 是默认分支不叫 main 时接在提示词后面的话。叫 main 时是空的，提示词一字不变。
func BranchContract(def string) string {
	if def == "main" {
		return ""
	}
	return fmt.Sprintf("\n\n这个项目的默认分支是 %[1]s，不是 main。agent-task 里说的 main 都指 %[1]s：任务分支从 %[1]s 开，交付前从 %[1]s 合入最新，引擎过门后合进 %[1]s。", def)
}

// ErrBranchBlocked：这个项目现在接不了任务：认不出默认分支，或者主目录不在要的分支上。不算失败，不计连环失败，条目和续跑记录都留着，好了接着做。
var ErrBranchBlocked = errors.New("不接任务")

// ProjectBranch 认项目现在的默认分支，主目录要在它上面。能不能接新任务看这个，新任务记下这个结果。
// 主目录在别的分支时不起：BACKLOG 从工作目录读，认领提交到这里，任务跑完引擎才发现合不进去，额度白花了。
func (a *Service) ProjectBranch(dir string) (string, error) {
	def, err := a.Gitx.DefaultBranch(dir)
	if err != nil {
		return "", fmt.Errorf("%w：%w", ErrBranchBlocked, err)
	}
	return def, a.checkHead(dir, def, "默认分支是 "+def)
}

// JobBranch：续跑、接手用这条任务记下的分支，主目录要在它上面，不现认。没记下的（新任务、老任务）按 ProjectBranch。
func (a *Service) JobBranch(dir, recorded string) (string, error) {
	switch {
	case recorded == "":
		return a.ProjectBranch(dir)
	case a.Gitx.OnBranch(dir, recorded):
		return recorded, nil
	case !a.Gitx.HasBranch(dir, recorded):
		return "", fmt.Errorf("%w：%s 这条任务基于 %s 分支，现在查不到了，不换分支接着做。分支改了名就改回去", ErrBranchBlocked, dir, recorded)
	}
	return recorded, a.checkHead(dir, recorded, "任务基于 "+recorded)
}

func (a *Service) checkHead(dir, want, why string) error {
	if a.Gitx.OnBranch(dir, want) {
		return nil
	}
	where := "不在任何分支上"
	if head, err := a.Gitx.Git(dir, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		if cur, ok := strings.CutPrefix(head, "refs/heads/"); ok {
			where = "在 " + cur + " 分支"
		}
	}
	return fmt.Errorf("%w：%s 的主目录%s，%s。切回去就接着做：git -C %s switch %s", ErrBranchBlocked, dir, where, why, fsx.ShQuote(dir), want)
}

// ProjectBlock：这个项目现在能不能接新任务，不能时说为什么。有任务在等它记下的分支（排队过门、等续跑）时，提示写那条分支：现认出来的可能不是它，照着切就走反了。
func (a *Service) ProjectBlock(st *state.State, project, dir string) error {
	_, err := a.ProjectBranch(dir)
	if err == nil {
		return nil
	}
	if want := waitingBranch(st, project); want != "" && !a.Gitx.OnBranch(dir, want) {
		if werr := a.checkHead(dir, want, "有任务在等 "+want); werr != nil {
			return werr
		}
	}
	return err
}

// waitingBranch：这个项目最早一条在等主目录的任务记下的分支。
func waitingBranch(st *state.State, project string) string {
	if st == nil {
		return ""
	}
	for _, j := range st.Jobs {
		if j.Project == project && j.DefaultBranch != "" && (state.GateActive(j) || j.ResumePending) {
			return j.DefaultBranch
		}
	}
	return ""
}

// ResumeBlockLines：status 里等续跑、但主目录不在它记下的分支上的。
func (a *Service) ResumeBlockLines(st *state.State) []string {
	var out []string
	for _, j := range st.PendingResumes() {
		if why := ScopeBlocked(st, a.Cfg, j.Project, j.Item); j.Kind == "" && why != "" {
			out = append(out, fmt.Sprintf("等续跑：%s「%s」，任务 %s：%s；这个项目先不起新条目", j.Project, textfmt.TruncRunes(j.Title, 30), j.ID, why))
		}
		if j.DefaultBranch == "" {
			continue
		}
		if _, err := a.JobBranch(j.ProjectDir, j.DefaultBranch); err != nil {
			out = append(out, fmt.Sprintf("等续跑：%s「%s」，任务 %s：%v", j.Project, textfmt.TruncRunes(j.Title, 30), j.ID, err))
		}
	}
	return out
}

// TakeoverRecorded：接手这一条沿用上一个任务记下的分支。
func TakeoverRecorded(st *state.State, p backlog.Project, it backlog.Item) string {
	if last := LastTaskJob(st, p, it); last != nil {
		return last.DefaultBranch
	}
	return ""
}

// jobBacklog 读这条任务那条分支上的 BACKLOG。主目录在它上面读工作目录（任务可能还没提交）；切走了读提交里的，别的分支的 BACKLOG 里没有这一条。记下的分支没了，只能读工作目录。
func (a *Service) jobBacklog(j *state.Job) (string, error) {
	p := backlog.Project{Name: j.Project, Dir: j.ProjectDir}
	if j.DefaultBranch != "" && !a.Gitx.OnBranch(j.ProjectDir, j.DefaultBranch) && a.Gitx.HasBranch(j.ProjectDir, j.DefaultBranch) {
		rel, err := backlog.BacklogRepoPath(p)
		if err != nil {
			rel = "docs/BACKLOG.md" // 主目录这条分支上可能没有这个文件
		}
		out, err := a.Gitx.Git(j.ProjectDir, "show", "refs/heads/"+j.DefaultBranch+":"+rel)
		if err != nil {
			return "", fmt.Errorf("读 %s 上的 BACKLOG：%w %s", j.DefaultBranch, err, textfmt.OneLine(out, 100))
		}
		return out, nil
	}
	b, err := os.ReadFile(p.BacklogPath())
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

// jobItems 是 jobBacklog 解析出来的待做。
func (a *Service) jobItems(j *state.Job) ([]backlog.Item, error) {
	text, err := a.jobBacklog(j)
	if err != nil {
		return nil, err
	}
	items := backlog.ParseBacklog(text)
	for i := range items {
		items[i].Project = j.Project
	}
	return items, nil
}

const resumePromptTemplate = "你仍是当前待办的执行者，亲自接着完成，不通过 dispatch 或 agentq run 重新派发同一任务。上次的进程在等后台任务或通知时退出了，后台的活（子代理、Monitor、后台命令）已经不在了，不要再等它们。先看 git 状态、worktree 和分支现状、%s 的 docs/BACKLOG.md 里这一条的标记，从实际进度接着按 ~/.claude/skills/agent-task/SKILL.md 把这一条做到分支可合并：%s。不自己合并：写好 ready.json 提交到任务分支就结束，合并由引擎过门后做。一切等待都在前台等完。结束时最后一段写：分支可以合并了没有、分支名、没好的原因、用户怎么验收。"

func BuildResumePrompt(projectDir, item string) string {
	return fmt.Sprintf(resumePromptTemplate, projectDir, item)
}

// jobIDPart：任务 id 里的项目名部分。id 要能直接复制去用（agentq logs、「停 <id>」），也要能在 ps 的环境里认出来（见 groupMarked），
// 还要放得进文件名（255 字节，前面有时间，后面可能有序号）。
func jobIDPart(project string) string {
	s := strings.ReplaceAll(proc.PsSafe(project), " ", "_")
	for len(s) > 200 {
		r := []rune(s)
		s = string(r[:len(r)-1])
	}
	return s
}

func (a *Service) NewJobID(project string) (string, error) {
	if err := os.MkdirAll(filepath.Join(a.DataDir(), "jobs"), 0o755); err != nil {
		return "", err
	}
	base := a.Now().Format("0102-150405") + "-" + jobIDPart(project)
	id := base
	for i := 2; ; i++ {
		err := os.Mkdir(a.JobDir(id), 0o755)
		if err == nil {
			return id, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

// Launch 为条目新开一个任务。
func (a *Service) Launch(st *state.State, p backlog.Project, it backlog.Item) (*state.Job, error) {
	return a.launch(st, p, it, false)
}

// launch：manual 是用户亲手启动的（agentq run、Telegram「做」），记在任务上，续跑沿用，暂停时 tick 也接着续跑。
func (a *Service) launch(st *state.State, p backlog.Project, it backlog.Item, manual bool) (*state.Job, error) {
	f, handoff := a.handoffFacts(st, p, it)
	recorded := ""
	if handoff { // 接着上一个任务的分支做，基点也接着
		recorded = TakeoverRecorded(st, p, it)
	}
	def, err := a.JobBranch(p.Dir, recorded)
	if err != nil {
		return nil, err
	}
	contract := BranchContract(def)
	title := it.Title
	if title == "" {
		title = it.Text
	}
	j := &state.Job{Project: p.Name, ProjectDir: p.Dir, Item: it.Text, Title: title, Fingerprint: it.Fingerprint, DefaultBranch: def, Manual: manual}
	if handoff {
		j.Summary = "用户去掉了卡住标记，从分支 " + f.Branch + " 接着做"
		j.Handoff, j.StuckBranch, j.StuckWorktree = true, f.Branch, f.Worktree
		note := a.GateTakeoverNote(st, j, p, it)
		j, err := a.Start(st, j, BuildTakeoverPrompt(p.Dir, it.Text, f)+contract+handoffNote+note+directionNote(it.Text), "")
		if err == nil {
			a.Ledger.AppendNightly(j)
		}
		return j, err
	}
	j.NewReport = firstReport(st, p, it)
	return a.Start(st, j, BuildPrompt(p.Dir, it.Text)+contract+a.Gate.PatchNote(p.Dir, def, it.Text)+directionNote(it.Text)+a.constraintsNote(p.Name), "")
}

// constraintsNote：项目学到的约束，接在新任务合同末尾。没接钩子或没有约束时是空的，提示词一字不变。
func (a *Service) constraintsNote(project string) string {
	if a.Hooks.ConstraintsNote == nil {
		return ""
	}
	return a.Hooks.ConstraintsNote(project)
}

// LaunchResume 用中断任务的会话（claude -r）续跑一次，记成一个新任务。
func (a *Service) LaunchResume(st *state.State, orig *state.Job) (*state.Job, error) {
	extra := ""
	if orig.BudgetHit {
		extra = budgetResumeNote
	}
	if orig.Gate != nil && orig.Gate.State == state.GateReturned {
		extra = a.Gate.ReturnNote(orig)
	}
	return a.ResumeJob(st, orig, "", extra)
}

// ResumeJob 是续跑本身；id 非空时用建好的任务目录，extra 接在续跑 prompt 后面（用户回复了这个任务时写他说的话）。
func (a *Service) ResumeJob(st *state.State, orig *state.Job, id, extra string) (*state.Job, error) {
	def, err := a.JobBranch(orig.ProjectDir, orig.DefaultBranch)
	if err != nil {
		return nil, err
	}
	contract := BranchContract(def)
	j := &state.Job{
		ID:            id,
		DefaultBranch: def,
		Project:       orig.Project, ProjectDir: orig.ProjectDir, Item: orig.Item, Title: orig.Title,
		Fingerprint: orig.Fingerprint, SessionID: orig.SessionID, ResumeOf: orig.ID, Takeover: orig.Takeover, Backend: orig.Backend, NewReport: orig.NewReport.Next(), Manual: orig.Manual,
		Summary: "上次中断（任务 " + orig.ID + "），已用同一会话 " + orig.SessionID + " 续跑",
	}
	carryGate(j, orig)
	pending, stuck := orig.ResumePending, orig.StuckPending
	orig.ResumePending, orig.StuckPending = false, "" // 接着做了，卡住标记不用再写（用户回复接回时）
	j, err = a.Start(st, j, BuildResumePrompt(orig.ProjectDir, orig.Item)+contract+extra, orig.SessionID)
	if j != nil {
		orig.ResumedBy = j.ID
		a.Ledger.WriteMeta(orig)
	} else { // 一个进程都没起：下次还要接着续跑
		orig.ResumePending, orig.StuckPending = pending, stuck
	}
	if err == nil {
		a.Ledger.AppendNightly(j) // 汇总里记一行「续跑中」，结束时再记一行结果
	}
	return j, err
}

// directionNote：条目定了大方向时接在提示词后面的话。没定时是空的，提示词一字不变。
func directionNote(item string) string {
	if backlog.Direction(item) == "" {
		return ""
	}
	return "\n\n条目里的（方向：…）是开工前定的大方向，按它做，细节自己定；要偏离就写进方案和 ready.json 的 progress。"
}

// carryGate：退回的任务接着做算下一轮，带上退回时定的范围和上一轮的分支尖（scope 门比较它之后的改动）；其余的续跑还是这一轮。
func carryGate(j, orig *state.Job) {
	if g := orig.Gate; g != nil && g.State == state.GateReturned {
		j.GateRound, j.GateScope, j.GateScopeFrom = g.Round+1, nil, ""
		if len(g.Scope) > 0 && g.Head != "" {
			j.GateScope, j.GateScopeFrom = slices.Clone(g.Scope), g.Head
		}
		return
	}
	j.GateRound, j.GateScope, j.GateScopeFrom = orig.GateRound, nil, ""
	if orig.Gate == nil { // 这一轮做到一半中断的续跑：范围照旧。卡在门上的（用户去掉卡住标记重做）不再限定
		j.GateScope, j.GateScopeFrom = slices.Clone(orig.GateScope), orig.GateScopeFrom
	}
}

// GateTakeoverNote：接手任务接的是门退回的任务时，提示词后面要多门的报告，轮数接着算。
func (a *Service) GateTakeoverNote(st *state.State, j *state.Job, p backlog.Project, it backlog.Item) string {
	last := LastTaskJob(st, p, it)
	j.NewReport = firstReport(st, p, it)
	if last == nil || last.Gate == nil || (last.Gate.State != state.GateReturned && last.Gate.State != state.GateStuck) {
		return ""
	}
	carryGate(j, last)
	return a.Gate.ReturnNote(last)
}

// monitorWords：CLI 没有 Monitor 这个工具时，提示词里就别提它。
var monitorWords = strings.NewReplacer("后台命令、Monitor 或", "后台命令或", "（子代理、Monitor、后台命令）", "（子代理、后台命令）")

// hostWords：提示词写的是 Claude 的说法和默认位置，按这次的后端和实际的报告目录说。Claude 上字节不变。
func (a *Service) hostWords(prompt string, be backend.Backend) string {
	if be.Name() != backend.Default {
		prompt = strings.ReplaceAll(prompt, "claude -p", be.Name())
	}
	return strings.ReplaceAll(prompt, "~/.claude/nightly", a.Tilde(a.ReportDir()))
}

// skillPaths：提示词写的是 Claude 的技能目录，别的后端换成它自己的，让它读得到。没声明的不动。
func skillPaths(prompt string, be backend.Backend) string {
	h, ok := be.(backend.SkillHome)
	if !ok {
		return prompt
	}
	const home = "/home"
	rel := strings.TrimPrefix(h.SkillsDir(home), home+"/")
	if rel == ".claude/skills" {
		return prompt
	}
	return strings.ReplaceAll(prompt, "~/.claude/skills/", "~/"+rel+"/")
}

// jobBackend 定新任务用哪个后端，默认后端记成空（老文件字节不变）。接回会话的跟着那个会话走，不跟随之后的配置，跨后端只能走接手。
func (a *Service) jobBackend(st *state.State, project, resume string) string {
	name := a.Cfg.ProjectBackend(project)
	if resume != "" {
		for _, o := range st.Jobs {
			if o.SessionID == resume {
				name = o.Backend
				break
			}
		}
	}
	if name == backend.Default {
		return ""
	}
	return name
}

// Start 建任务目录、经任务外壳启动 claude（resume 非空时续跑那个会话），并立刻把任务写进 state.json。
// 外壳启动失败、或启动后写 state.json 失败（这时先杀掉刚启动的 claude）时任务记成 failed，返回这条记录和错误；
// 还没建出任务记录就失败（后端做不到这个任务的硬要求也算）时返回 nil。
func (a *Service) Start(st *state.State, j *state.Job, prompt, resume string) (*state.Job, error) {
	if j.Backend == "" {
		j.Backend = a.jobBackend(st, j.Project, resume)
	}
	if j.Backend != a.defaultBackendName() { // 默认后端调用方查过了，按项目换的后端在这里查
		if ok, why := a.checkAuth(a.BackendFor(j.Backend)); !ok {
			return nil, errors.New(why)
		}
	}
	// BACKLOG 是用户的文件，可能有不是 UTF-8 的字节。它们进了 argv，macOS 的 ps 输出会变乱并截断，看不到进程标记，活着的任务就成了死的（proc.ProcAlive）。
	prompt = strings.ToValidUTF8(prompt, string(utf8.RuneError))
	id := j.ID // 对话任务先建好任务目录（放用户发来的附件）再启动，id 已经有了
	fresh := id == ""
	if fresh {
		var err error
		if id, err = a.NewJobID(j.Project); err != nil {
			return nil, err
		}
	}
	dir := a.JobDir(id)
	be := a.BackendFor(j.Backend)
	caps := be.Caps()
	if !caps.Monitor {
		prompt = monitorWords.Replace(prompt)
	}
	prompt = a.hostWords(skillPaths(prompt, be), be)
	a.LocksDir() // 默认后端不是 Claude 时建好：技能里的 with-lock.sh 靠这个目录和引擎锁到一处
	session := ""
	if resume == "" && caps.PresetSession {
		// 预先指定会话 id：tick 据此找会话记录判断卡死；中途被杀、重启时也能续跑（续跑沿用原会话，id 不变）
		var err error
		if session, err = backend.NewSessionID(); err != nil {
			return nil, err
		}
		j.SessionID = session
	}
	bcfg := a.Cfg.ForBackend(j.Backend) // 这个后端自己的模型、强度、权限，别家的默认不串
	cfg := bcfg
	light := ""
	if l, ok := be.(backend.LightModeler); ok {
		light = l.LightModel()
	}
	cfg.Model, cfg.Effort = TaskModel(cfg, j.Kind, j.Item, light) // 新开、续跑、接手都走这里，判断一样
	req := TaskRequest(cfg, prompt, j.Project, j.Title, resume, session, a.JobBudget(j.Kind))
	switch j.Kind {
	case state.KindFollowup:
		files := a.Hooks.ChatFilesDir(id)
		if _, err := os.Stat(files); err != nil {
			files = ""
		}
		req = a.Hooks.FollowupRequest(bcfg, prompt, j.Project, j.Title, resume, files)
	case state.KindRouter:
		req = a.Hooks.RouterRequest(bcfg, a.Home, prompt, session)
	case state.KindInbox:
		req = a.Hooks.InboxRequest(bcfg, prompt, j.Project, j.Title, resume, session, a.Hooks.OutboxDir())
	case state.KindSuggest:
		req = a.Hooks.SuggestRequest(bcfg, prompt, session, j.ProjectDir)
	case state.KindSense:
		req = a.Hooks.SenseRequest(bcfg, prompt, session)
	case state.KindDiscuss:
		req = a.Hooks.DiscussRequest(bcfg, prompt, j.Project, session)
	case state.KindRelease:
		req = a.Hooks.ReleaseRequest(bcfg, req)
	}
	req.Dir, req.WorkDir = j.ProjectDir, dir
	if j.Kind == "" || j.Kind == state.KindNew { // 这两种要合并，合并前的独立审查是硬前提
		req.Must |= backend.MustMerge
	}
	j.GateContract = j.GateContract || j.Kind == ""
	schema := ""
	if req.Schema != "" && !caps.Schema && req.Must&backend.MustSchema == 0 { // CLI 不能强制：提示词要求，读结果时校验（Ledger.ReadResult）
		schema = req.Schema
		req.Prompt += backend.SchemaPrompt(schema)
	}
	var inv backend.Invocation
	var degraded []backend.Degradation
	var err error
	if backend.Known(be.Name()) { // 这里统一查一次：新后端的 Build 漏调，外部内容任务也不会静默跑在没有限制的后端上。没注册的后端让 Build 说
		err = backend.CheckMust(be, req)
	}
	if err == nil && req.Profile == backend.ProfileFull { // 只读的追问、审查不用写文件，不看这个
		if why := a.Cfg.PermissionProblem(j.Backend); why != "" {
			err = errors.New(why)
		}
	}
	if err == nil {
		if why := a.modelWindowStop(st, j, req.Model, bcfg.Model); why != "" {
			err = fmt.Errorf("%w：%s", ErrModelWindow, why)
		}
	}
	if err == nil {
		inv, degraded, err = be.Build(req)
	}
	if err != nil {
		if fresh {
			os.RemoveAll(dir) // 一个进程都没起：每次 tick 都会走到这里，不留空目录
		}
		if !errors.Is(err, ErrModelWindow) { // 额度挡住的每次 tick 都会走到，原因在 tick 那一行，不再单记
			a.Logf("任务没启动（%s「%s」）：%v", j.Project, textfmt.TruncRunes(j.Title, 40), err)
		}
		return nil, err
	}
	for _, d := range degraded {
		a.Logf("任务 %s（后端 %s）：%s", id, be.Name(), d.Note)
	}
	if schema != "" {
		if err := os.WriteFile(filepath.Join(dir, backend.SchemaFile), []byte(schema), 0o644); err != nil {
			return nil, err
		}
	}
	for _, list := range [][]string{inv.Args, inv.Record} { // 标题进进程标记，同样不能带坏字节；各种任务的参数到这里都拼好了
		for i := range list {
			list[i] = strings.ToValidUTF8(list[i], string(utf8.RuneError))
		}
	}
	outDir, promptText := dir, req.Prompt
	if j.Kind == state.KindSense { // 提示词里有外部原文：任务目录 Router 能读，真的放到 ~/.agentq/sense/，输出也写到那边
		outDir, promptText = a.Ledger.SenseRunDir(id), senseJobPlaceholder
		if err := a.Hooks.WriteSenseRunFile(id, "prompt.txt", []byte(prompt+"\n")); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(promptText+"\n"), 0o644); err != nil {
		return nil, err
	}
	j.ID, j.StartedAt, j.Status, j.BootTime = id, a.Now(), state.StatusRunning, a.CurBoot()
	j.Args = append([]string{inv.Path}, inv.Record...)
	// 先记下「正在启动」再起进程：起 claude 和写 state.json 之间进程被杀（SIGKILL、断电）时，
	// 下次 tick 按这个记录和外壳写的 claude.pid 认领（见 adoptStarting），不会以为没人在做又起一个。记不下就不起。
	a.Ledger.WriteMeta(j)
	if err := a.MarkStarting(id); err != nil {
		j.Status = ""
		if fresh {
			os.RemoveAll(dir) // 一个进程都没起：每次 tick 都会走到这里，不留空目录
		}
		err = fmt.Errorf("记不下正在启动的任务，没启动：%w", err)
		a.Logf("任务 %s：%v", id, err)
		return nil, err
	}
	defer a.clearStarting(id)
	self, err := JobExecutable()
	var pid int
	if err == nil {
		pid, err = StartJob(self, outDir, inv, a.LaunchdLogPath())
	}
	if errors.Is(err, ErrShellLost) {
		// 外壳没了，claude 可能已经在跑：确认不在了才能算启动失败（见 joblost.go）。查不了就先记运行中、不知道 pid，reap 接着查。
		found, ferr := a.FindLostClaude(j)
		switch {
		case ferr != nil:
			a.Logf("任务 %s：%v；查不了 claude 还在不在：%v，先当它在，下次 tick 再查", id, err, ferr)
			pid, err = 0, nil
		case found > 0:
			a.Logf("任务 %s：%v，claude 还在跑（pid %d），接回来继续看着", id, err, found)
			pid, err = found, nil
		default:
			err = fmt.Errorf("%w；已确认这个任务没有进程在跑", err)
		}
	}
	if err != nil {
		j.Status = state.StatusFailed
		now := a.Now()
		j.EndedAt = &now
		j.Summary = "启动失败：" + err.Error()
		st.Jobs = append(st.Jobs, j)
		a.Ledger.WriteMeta(j)
		a.Ledger.AppendNightly(j)
		a.noteOutcome(st, j)
		a.noticeEnded(j)
		_ = state.Save(a.StatePath(), st)
		return j, err
	}
	j.PID = pid
	st.Jobs = append(st.Jobs, j)
	a.Ledger.WriteMeta(j)
	if err := state.Save(a.StatePath(), st); err != nil {
		// 状态里没有它，以后的 tick 就不会收尾、不会超时杀它，也不知道它占着位置：宁可马上杀掉
		how := "SIGTERM"
		if proc.KillGroup(pid, proc.KillGrace) {
			how = "SIGTERM 后 SIGKILL"
		}
		err = fmt.Errorf("启动后写状态文件失败，已杀掉刚启动的 claude（pid %d，%s）：%w", pid, how, err)
		a.Logf("任务 %s：%v", id, err)
		a.Finish(st, j, state.StatusFailed, err.Error())
		return j, err
	}
	return j, nil
}

// JobExecutable：任务外壳用哪个程序。测试里换成会在报 pid 前死掉的替身。
var JobExecutable = os.Executable

func (a *Service) StartingDir() string { return filepath.Join(a.DataDir(), "starting") }

func (a *Service) MarkStarting(id string) error {
	return fsx.WriteFileAtomic(filepath.Join(a.StartingDir(), id), nil)
}

func (a *Service) clearStarting(id string) { os.Remove(filepath.Join(a.StartingDir(), id)) }

// AdoptStarting：上一个启动方（tick、run、listen）起任务时被杀了，留下了「正在启动」的记录：任务没进 state.json，claude 可能已经在跑。
// 按 meta.json 把它接回来，由 reap 照常看着。meta.json 读不出的没法管，只记日志。
// 记录要等接回来的任务真的存进 state.json（下一次看到它已经在里面）才清：接回来的这次 tick 也可能被杀。
func (a *Service) AdoptStarting(st *state.State) {
	ents, err := os.ReadDir(a.StartingDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		id := e.Name()
		known := false
		for _, j := range st.Jobs {
			known = known || j.ID == id
		}
		if known {
			a.clearStarting(id)
			continue
		}
		var j state.Job
		b, err := os.ReadFile(filepath.Join(a.JobDir(id), "meta.json"))
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID != id || j.Status != state.StatusRunning {
			a.Logf("任务 %s 启动时启动方被杀了，meta.json 读不出，接不回来：%v", id, err)
			a.clearStarting(id)
			continue
		}
		st.Jobs = append(st.Jobs, &j)
		a.Logf("任务 %s 启动时启动方被杀了，接回来继续看着", id)
	}
}

// adoptPID：接回来的任务还不知道 pid 时，从外壳写的 claude.pid 认领。返回 false 表示还不知道。
func (a *Service) adoptPID(j *state.Job) bool {
	if j.PID > 0 {
		return true
	}
	b, err := os.ReadFile(a.Ledger.JobOutput(j.ID, ClaudePidFile))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	j.PID = pid
	a.Ledger.WriteMeta(j)
	return true
}

// startingUp：还不知道 pid，但启动还在外壳报 pid 的时限之内（时钟往回跳也算过了时限）。
func (a *Service) startingUp(j *state.Job) bool {
	d := a.Now().Sub(j.StartedAt)
	return d >= 0 && d < ShellReportWait
}

// TaskModel 选这个任务的模型和强度（docs/specs 按活分配模型；官方定位：Opus 长任务和大重构，Sonnet 范围清楚的日常活和知识工作）。
// 复盘用 review_model、review_effort，巡逻分析用 patrol_model、medium；夜班、对话、new 这些用 model、effort。
// 普通待做任务（kind 为空）：
// 重写、重构、迁移、审查、审计（backlog.NoDowngradeItem）不降模型，（低风险）标记也压不过；其中重写、重构、迁移和（难撤回）（backlog.HeavyItem）强度用 heavy_effort。
// fixed：其余带「（低风险）」标记且配了 low_risk_model 的换成它和 low_risk_effort。
// auto：能用轻模型的活（backlog.LightItem）用 low_risk_model，没配就用后端说的轻模型 light。
// model 为空（跟随 CLI）时强度也跟随，不单给重活传 heavy_effort。
func TaskModel(cfg config.Config, kind, item, light string) (model, effort string) {
	switch kind {
	case config.KindReview:
		return cfg.Role(cfg.ReviewModel, cfg.ReviewEffort)
	case state.KindPatrol:
		return cfg.Role(cfg.PatrolModel, "medium")
	case "":
	default:
		return cfg.Model, cfg.Effort
	}
	if backlog.NoDowngradeItem(item) || backlog.HeavyItem(item) {
		if backlog.HeavyItem(item) && cfg.HeavyEffort != "" && cfg.Model != "" {
			return cfg.Model, cfg.HeavyEffort
		}
		return cfg.Model, cfg.Effort
	}
	low := func(m string) (string, string) {
		if cfg.LowRiskEffort != "" {
			return m, cfg.LowRiskEffort
		}
		return m, cfg.Effort
	}
	if cfg.ModelPolicy != config.ModelPolicyAuto {
		if cfg.LowRiskModel != "" && backlog.LowRiskItem(item) {
			return low(cfg.LowRiskModel)
		}
		return cfg.Model, cfg.Effort
	}
	if !backlog.LightItem(item) {
		return cfg.Model, cfg.Effort
	}
	if cfg.LowRiskModel != "" {
		return low(cfg.LowRiskModel)
	}
	if light != "" {
		return low(light)
	}
	return cfg.Model, cfg.Effort
}

// TaskRequest 是能动手的任务（待做、new、夜班、复盘、对话、任务箱）共用的启动意图。resume 非空时接回那个会话，否则 session 是新会话的 id。
func TaskRequest(cfg config.Config, prompt, project, title, resume, session string, budget float64) backend.Request {
	return backend.Request{
		Prompt: prompt, Label: proc.NameMarker(project) + textfmt.TruncRunes(title, 20), Resume: resume, Session: session,
		Model: cfg.Model, Effort: cfg.Effort, Permission: cfg.PermissionMode, BudgetUSD: budget,
	}
}
