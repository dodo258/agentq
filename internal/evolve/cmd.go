package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/fsx"
	"agentq/internal/gate"
	"agentq/internal/ledger"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/suggest"
	"agentq/internal/textfmt"
)

// RealQuotaStop：周额度到这个比例就不跑真实模型，留给正事。
const RealQuotaStop = 85.0

const (
	fakePoll = 200 * time.Millisecond
	realPoll = 15 * time.Second
	// quotaWait：读额度最多等多久，读不了就照跑并提示。
	quotaWait = 90 * time.Second
	latestRun = "latest"
	runIDForm = "20060102-150405"
)

// Service 是 agentq eval 和自我进化的信号、约束、提案（agentq evolve）。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Gate    *gate.Service
	Ledger  *ledger.Service
	Notify  *notify.Service
	Suggest *suggest.Service
	// Evals 是编译进二进制的评测集，根下有 evals/。评判用运行中这一代的原件，任务分支上改的题不算。
	Evals fs.FS
	// Version 记进结果，比较时能分清是哪一代跑的。
	Version string
}

// Run 是一次 eval run 的结果，存在 runs/<id>/results.json。
type Run struct {
	ID      string    `json:"id"`
	Mode    string    `json:"mode"`
	At      time.Time `json:"at"`
	Version string    `json:"version,omitempty"`
	Backend string    `json:"backend,omitempty"`
	Scores  []Score   `json:"scores"`
}

// Baseline 是存下来的基线。
type Baseline struct {
	Updated time.Time `json:"updated"`
	Scores  []Score   `json:"scores"`
}

const evalUsage = `agentq eval：评测集，给 agentq 自己打分
  agentq eval list                         列题；留出集只显示数量
  agentq eval run [题号…] [--fake|--real] [--bin 路径]
      默认只跑不调模型的程序题；--fake 用假 CLI 走完全部题的流程；--real 才调模型（周额度 ≥85% 拒绝）
      --bin：评判这个 agentq，不写就评判自己
  agentq eval baseline [run]               把一次结果并进基线（默认最近一次）
  agentq eval compare [run]                和基线比（默认最近一次）；任一题或任一领域变差就报，退出码 1
  agentq eval judge --bin 路径 [--out 文件] [题号…]
      评测门用：拿这一代的题和判分评判别的 agentq（--fake 跑法，含程序题），和基线比，结论写进 --out；
      结果不进 runs/，不会被 eval baseline 当成最近一次；变差退出码 1
`

func (s *Service) evalDir() string      { return filepath.Join(s.DataDir(), "evals") }
func (s *Service) runsDir() string      { return filepath.Join(s.evalDir(), "runs") }
func (s *Service) heldDir() string      { return filepath.Join(s.evalDir(), "heldout") }
func (s *Service) baselinePath() string { return filepath.Join(s.evalDir(), "baseline.json") }

// CmdEval 是命令入口。tick 永远不走这里。
func (s *Service) CmdEval(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(s.Out, evalUsage)
		return errors.New("要写子命令")
	}
	rest := args[1:]
	switch args[0] {
	case "list":
		if len(rest) != 0 {
			return errors.New("eval list 不带参数")
		}
		return s.cmdList()
	case "run":
		return s.cmdRun(rest)
	case "baseline":
		return s.cmdBaseline(rest)
	case "compare":
		return s.cmdCompare(rest)
	case "judge":
		return s.cmdJudge(rest)
	}
	fmt.Fprint(s.Out, evalUsage)
	return fmt.Errorf("不认识的子命令 %q", args[0])
}

// loadCases 读公开题和留出题。留出目录不在是正常的。
func (s *Service) loadCases() (public, held []Case, err error) {
	sub, err := fs.Sub(s.Evals, "evals")
	if err != nil {
		return nil, nil, fmt.Errorf("读评测集：%w", err)
	}
	public, err = LoadCases(sub, false)
	if err != nil {
		return nil, nil, err
	}
	if _, serr := os.Stat(s.heldDir()); serr != nil {
		return public, nil, nil
	}
	held, err = LoadCases(os.DirFS(s.heldDir()), true)
	if err != nil {
		return nil, nil, fmt.Errorf("留出集：%w", err)
	}
	return public, held, nil
}

func kindWord(k string) string {
	if k == KindProgram {
		return "程序题"
	}
	return "模型题"
}

func (s *Service) cmdList() error {
	public, held, err := s.loadCases()
	if err != nil {
		return err
	}
	for _, c := range public {
		what := c.Request
		if what == "" {
			what = "（不调模型）"
		}
		fmt.Fprintf(s.Out, "%-28s %-9s %s  %s\n", c.ID, c.Domain, kindWord(c.Kind), textfmt.OneLine(what, 70))
	}
	fmt.Fprintf(s.Out, "公开 %d 题；留出集 %d 题（内容不显示）\n", len(public), len(held))
	return nil
}

type runOptions struct {
	mode  string
	names []string
	bin   string
}

func parseRunArgs(args []string) (runOptions, error) {
	o := runOptions{mode: ModeProgram}
	fake, real := false, false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--fake":
			fake = true
		case a == "--real":
			real = true
		case a == "--bin":
			if i+1 >= len(args) {
				return o, errors.New("--bin 要写路径")
			}
			i++
			o.bin = args[i]
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("不认识的参数 %s", a)
		default:
			o.names = append(o.names, a)
		}
	}
	switch {
	case fake && real:
		return o, errors.New("--fake 和 --real 只能选一个")
	case fake:
		o.mode = ModeFake
	case real:
		o.mode = ModeReal
	}
	return o, nil
}

// planRun 按跑法挑出这次要跑的题。named 是用户点名了：点名的题跑不了要说出来。
func planRun(cases []Case, mode string) (run, skipped []Case) {
	for _, c := range cases {
		switch {
		case c.Kind == KindProgram:
			run = append(run, c)
		case mode == ModeReal:
			run = append(run, c)
		case mode == ModeFake && c.HasFake():
			run = append(run, c)
		default:
			skipped = append(skipped, c)
		}
	}
	return run, skipped
}

func callsModel(cases []Case) bool {
	return slices.ContainsFunc(cases, func(c Case) bool { return c.Kind != KindProgram })
}

// weekQuota 读周额度。over 为真是到线了；note 非空是读不了，照跑但要提示。
func weekQuota(ctx context.Context, q backend.QuotaReader, now time.Time, model string) (used float64, over bool, note string) {
	ctx, cancel := context.WithTimeout(ctx, quotaWait)
	defer cancel()
	text, err := q.QuotaText(ctx)
	if err != nil {
		return 0, false, "读不了额度（" + textfmt.OneLine(err.Error(), 200) + "），照跑"
	}
	windows, err := q.ParseQuota(text, now)
	if err != nil {
		return 0, false, "读不了额度（" + textfmt.OneLine(err.Error(), 200) + "），照跑"
	}
	found := false
	for _, w := range windows {
		if !strings.HasPrefix(w.Label, "week") || !w.Counts(model) {
			continue
		}
		found = true
		used = max(used, w.UsedAt(now))
	}
	if !found {
		return 0, false, "额度输出里没有周额度，照跑"
	}
	return used, used >= RealQuotaStop, ""
}

// prepareReal 跑真实模型前的检查：周额度、登录、怎么在临时 HOME 里用上本机的登录。一个进程的额度都不花。
func (s *Service) prepareReal(ctx context.Context, h *harness) error {
	be := s.DefaultBackend()
	h.backend = be.Name()
	linker, ok := be.(backend.IdentityLinker)
	if !ok {
		return fmt.Errorf("后端 %s 还不支持 --real：没声明怎么在临时 HOME 里用上本机的登录", be.Name())
	}
	if q, ok := be.(backend.QuotaReader); ok {
		used, over, note := weekQuota(ctx, q, s.Now(), s.Cfg.ForBackend(be.Name()).Model)
		switch {
		case note != "":
			fmt.Fprintln(s.Out, note)
		case over:
			return fmt.Errorf("周额度已用 %.0f%%，到了 %.0f%% 的线，不跑 --real", used, RealQuotaStop)
		default:
			fmt.Fprintf(s.Out, "周额度已用 %.0f%%\n", used)
		}
	} else {
		fmt.Fprintln(s.Out, "这个后端读不了额度，照跑")
	}
	probe, err := be.Probe(ctx, backend.ProbeEnv{Home: s.Home, Explicit: s.Cfg.BackendPath(be.Name()), Env: proc.ChildEnv()})
	if err != nil {
		return fmt.Errorf("真实 CLI 不能用：%w", err)
	}
	cfg := s.Cfg.ForBackend(be.Name())
	h.cli, h.model, h.effort, h.links = probe.Path, cfg.Model, cfg.Effort, linker.IdentityLinks()
	if p, ok := be.(backend.Permissions); ok {
		h.fullPerm = p.FullPermission()
	}
	h.poll = realPoll
	return nil
}

func (s *Service) cmdRun(args []string) error {
	o, err := parseRunArgs(args)
	if err != nil {
		fmt.Fprint(s.Out, evalUsage)
		return err
	}
	public, held, err := s.loadCases()
	if err != nil {
		return err
	}
	all := append(append([]Case{}, public...), held...)
	picked, err := Select(all, o.names)
	if err != nil {
		return err
	}
	cases, skipped := planRun(picked, o.mode)
	for _, c := range skipped {
		if len(o.names) > 0 {
			fmt.Fprintf(s.Out, "%s 要调模型，没跑：加 --real 调模型，或者 --fake 用假 CLI\n", c.ID)
		}
	}
	if len(o.names) == 0 && len(skipped) > 0 {
		fmt.Fprintf(s.Out, "还有 %d 道模型题没跑：加 --real 调模型，或者 --fake 用假 CLI\n", len(skipped))
	}
	if len(cases) == 0 {
		return errors.New("没有要跑的题")
	}
	bin, err := resolveBin(o.bin)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := s.newHarness(bin, o.mode)
	backendWord := "假 CLI"
	switch o.mode {
	case ModeProgram:
		backendWord = "不用后端"
	case ModeReal:
		backendWord = s.DefaultBackend().Name()
	}
	fmt.Fprintf(s.Out, "要跑 %d 题（%s），后端：%s，调模型：%s\n", len(cases), countKinds(cases), backendWord, yesNo(o.mode == ModeReal && callsModel(cases)))
	if o.mode == ModeReal && callsModel(cases) {
		if err := s.prepareReal(ctx, h); err != nil {
			return err
		}
	}
	run := Run{Mode: o.mode, At: s.Now(), Version: s.Version}
	if o.mode == ModeReal {
		run.Backend = h.backend
	}
	if run.Scores, err = s.runCases(ctx, h, cases); err != nil {
		return err
	}
	if run.ID, err = s.saveRun(run); err != nil {
		return err
	}
	printSummary(s, run.Scores)
	fmt.Fprintf(s.Out, "结果：%s\n", s.Tilde(filepath.Join(s.runsDir(), run.ID, "results.json")))
	for _, sc := range run.Scores {
		if sc.Skipped == "" && !sc.Correct {
			return env.ErrLogged
		}
	}
	return nil
}

// resolveBin 是被评判的二进制的真实路径，空的是自己。
func resolveBin(bin string) (string, error) {
	var err error
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return "", fmt.Errorf("找不到自己：%w", err)
		}
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return "", fmt.Errorf("--bin：%w", err)
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return "", fmt.Errorf("--bin：%w", err)
	}
	return bin, nil
}

func (s *Service) newHarness(bin, mode string) *harness {
	h := &harness{home: s.Home, bin: bin, mode: mode, poll: fakePoll}
	if perm, ok := backend.Get(backend.Default, backend.Options{Home: s.Home}).(backend.Permissions); ok {
		h.fullPerm = perm.FullPermission()
	}
	return h
}

// runCases 在一个新的临时根目录里逐题跑、逐题打印，跑完清现场。题目和假 CLI 都取自运行中这一代。
func (s *Service) runCases(ctx context.Context, h *harness, cases []Case) ([]Score, error) {
	sub, err := fs.Sub(s.Evals, "evals")
	if err != nil {
		return nil, fmt.Errorf("读评测集：%w", err)
	}
	if h.fakeCLI, err = fs.ReadFile(sub, "_fake/claude.sh"); err != nil {
		return nil, fmt.Errorf("读假 CLI：%w", err)
	}
	if n := cleanStale(os.TempDir(), s.Now()); n > 0 {
		fmt.Fprintf(s.Out, "清掉了 %d 个超过 7 天的旧现场\n", n)
	}
	h.base, err = os.MkdirTemp("", sandboxPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("建临时目录：%w", err)
	}
	if h.base, err = filepath.EvalSymlinks(h.base); err != nil { // macOS 的临时目录是软链接，git 和引擎认的是真实路径
		return nil, fmt.Errorf("建临时目录：%w", err)
	}
	if err := os.WriteFile(filepath.Join(h.base, sandboxMark), nil, 0o600); err != nil {
		return nil, fmt.Errorf("写现场标记：%w", err)
	}
	labels := heldIndex(cases)
	var scores []Score
	for _, c := range cases {
		score := h.runCase(ctx, c)
		if c.Kind == KindProgram {
			score.Mode, score.Model = ModeProgram, ""
		}
		printScore(s, score, labels)
		scores = append(scores, score)
	}
	h.cleanup(scores)
	return scores, nil
}

// cleanup：都通过就整个删；有留现场的只删构建缓存，现场路径在结果里。
func (h *harness) cleanup(scores []Score) {
	for _, s := range scores {
		if s.Kept != "" {
			_ = removeTree(filepath.Join(h.base, "gocache")) // 留现场时缓存没用，删不掉也不影响结论
			return
		}
	}
	_ = removeTree(h.base) // 删不掉的下次 run 按时间清
}

func countKinds(cases []Case) string {
	prog := 0
	for _, c := range cases {
		if c.Kind == KindProgram {
			prog++
		}
	}
	return fmt.Sprintf("程序题 %d，模型题 %d", prog, len(cases)-prog)
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func heldIndex(cases []Case) map[string]string {
	var ids []string
	for _, c := range cases {
		if c.Held {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	out := map[string]string{}
	for i, id := range ids {
		out[id] = fmt.Sprintf("留出题 %d", i+1)
	}
	return out
}

// printScore 打印一题。留出题只打结论，不打细节：输出可能进任务能读到的日志。
func printScore(s *Service, sc Score, labels map[string]string) {
	name := sc.Case
	if l, ok := labels[sc.Case]; ok {
		name = l
	}
	if sc.Skipped != "" {
		fmt.Fprintf(s.Out, "%-28s 没跑：%s\n", name, sc.Skipped)
		return
	}
	if sc.Error != "" {
		fmt.Fprintf(s.Out, "%-28s 判不了", name)
		if !sc.Held {
			fmt.Fprintf(s.Out, "：%s", textfmt.OneLine(sc.Error, 400))
		}
		fmt.Fprintln(s.Out)
		return
	}
	verdict := "没过"
	if sc.Correct {
		verdict = "通过"
	}
	domain := sc.Domain
	if sc.Held {
		domain = HeldDomain
	}
	fmt.Fprintf(s.Out, "%-28s %-9s %s %d 分  合并 %s  退回 %d  要人 %d  token %d  耗时 %s\n", name, domain, verdict, sc.Points, yesNo(sc.Merged), sc.Rounds, sc.Human, sc.Tokens, sc.Duration.Round(time.Second))
	if sc.Held {
		return
	}
	for _, ck := range sc.Checks {
		if !ck.Pass {
			fmt.Fprintf(s.Out, "    没过：%s：%s\n", ck.Name, textfmt.OneLine(ck.Detail, 300))
		}
	}
	if sc.Kept != "" {
		fmt.Fprintf(s.Out, "    现场留着：%s\n", sc.Kept)
	}
}

func printSummary(s *Service, scores []Score) {
	fmt.Fprintln(s.Out, "按领域：")
	for _, d := range Summarize(scores) {
		line := fmt.Sprintf("  %-9s %d 题，通过 %d，平均 %.1f 分，平均退回 %.1f 轮，token %d，耗时 %s", d.Domain, d.Cases, d.Correct, d.Points, d.Rounds, d.Tokens, d.Duration.Round(time.Second))
		if d.Unjudged > 0 {
			line += fmt.Sprintf("，%d 题没结论", d.Unjudged)
		}
		fmt.Fprintln(s.Out, line)
	}
}

// saveRun 写结果，时间撞了加序号。
func (s *Service) saveRun(run Run) (string, error) {
	if err := os.MkdirAll(s.runsDir(), 0o700); err != nil {
		return "", fmt.Errorf("建结果目录：%w", err)
	}
	base := s.Now().Format(runIDForm)
	id := base
	for i := 2; ; i++ {
		err := os.Mkdir(filepath.Join(s.runsDir(), id), 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("建结果目录：%w", err)
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	run.ID = id
	for i := range run.Scores {
		run.Scores[i].Run = id
	}
	raw, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return "", fmt.Errorf("写结果：%w", err)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(s.runsDir(), id, "results.json"), append(raw, '\n')); err != nil {
		return "", fmt.Errorf("写结果：%w", err)
	}
	return id, nil
}

// loadRun 读一次结果。name 空或 latest 是最近一次。
func (s *Service) loadRun(name string) (Run, error) {
	if name == "" || name == latestRun {
		entries, err := os.ReadDir(s.runsDir())
		if err != nil || len(entries) == 0 {
			return Run{}, errors.New("还没有跑过：agentq eval run")
		}
		name = entries[len(entries)-1].Name() // 目录名按时间排，ReadDir 已经排好
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return Run{}, fmt.Errorf("结果名不对：%s", name)
	}
	raw, err := os.ReadFile(filepath.Join(s.runsDir(), name, "results.json"))
	if err != nil {
		return Run{}, fmt.Errorf("读结果 %s：%w", name, err)
	}
	var run Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return Run{}, fmt.Errorf("结果 %s 坏了：%w", name, err)
	}
	return run, nil
}

func (s *Service) loadBaseline() (Baseline, bool, error) {
	raw, err := os.ReadFile(s.baselinePath())
	if errors.Is(err, fs.ErrNotExist) {
		return Baseline{}, false, nil
	}
	if err != nil {
		return Baseline{}, false, fmt.Errorf("读基线：%w", err)
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return Baseline{}, false, fmt.Errorf("基线坏了：%w", err) // 不当成没有：否则下一次 baseline 把它盖掉
	}
	return b, true, nil
}

func oneOptional(args []string, what string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	}
	return "", fmt.Errorf("eval %s 最多写一次结果的名字", what)
}

func (s *Service) cmdBaseline(args []string) error {
	name, err := oneOptional(args, "baseline")
	if err != nil {
		return err
	}
	run, err := s.loadRun(name)
	if err != nil {
		return err
	}
	base, _, err := s.loadBaseline()
	if err != nil {
		return err
	}
	unjudged := 0
	for _, sc := range run.Scores {
		if !sc.judged() {
			unjudged++
		}
	}
	base.Scores = MergeBaseline(base.Scores, run.Scores)
	base.Updated = s.Now()
	raw, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return fmt.Errorf("写基线：%w", err)
	}
	if err := os.MkdirAll(s.evalDir(), 0o700); err != nil {
		return fmt.Errorf("写基线：%w", err)
	}
	if err := fsx.WriteFileAtomic(s.baselinePath(), append(raw, '\n')); err != nil {
		return fmt.Errorf("写基线：%w", err)
	}
	fmt.Fprintf(s.Out, "基线更新：%s 的 %d 题并进去，现在共 %d 题", run.ID, len(run.Scores)-unjudged, len(base.Scores))
	if unjudged > 0 {
		fmt.Fprintf(s.Out, "；%d 题没结论，没进基线", unjudged)
	}
	fmt.Fprintf(s.Out, "（%s）\n", s.Tilde(s.baselinePath()))
	return nil
}

func (s *Service) cmdCompare(args []string) error {
	name, err := oneOptional(args, "compare")
	if err != nil {
		return err
	}
	run, err := s.loadRun(name)
	if err != nil {
		return err
	}
	base, ok, err := s.loadBaseline()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("还没有基线：agentq eval baseline")
	}
	cmp := Compare(base.Scores, run.Scores)
	fmt.Fprintf(s.Out, "%s 和基线比了 %d 题\n", run.ID, cmp.Compared)
	printComparison(s, cmp)
	if len(cmp.Worse) > 0 {
		fmt.Fprintf(s.Out, "变差了 %d 处\n", len(cmp.Worse))
		return env.ErrLogged
	}
	if cmp.Compared == 0 {
		return errors.New("一题都没比上：跑法或题目和基线对不上")
	}
	fmt.Fprintln(s.Out, "没有变差")
	return nil
}

func printComparison(s *Service, cmp Comparison) {
	for _, l := range cmp.Better {
		fmt.Fprintln(s.Out, "变好：", l)
	}
	for _, l := range cmp.Notes {
		fmt.Fprintln(s.Out, "注意：", l)
	}
	for _, l := range cmp.Worse {
		fmt.Fprintln(s.Out, "变差：", l)
	}
}
