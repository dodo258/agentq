package evolve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// 修好就用上（自己长大的最后一步，开关和 selfgrow.go 同一个）：
//   - 目标：self_project 里引擎合并、评测门过了（Tally["eval"].Pass > 0）的最新合并提交，是现在这一代的后代、没被回滚过。
//     只认源码构建的这一代（版本 dev-<提交>）和安装引擎装的（有 receipt.json）；发行版走 agentq update。
//   - 空闲才动：有任务在跑就不接新活（sched 看 SelfUpdate 的 hold），等它们做完。动手的是脱离 tick 的 agentq evolve apply
//     （自己的进程组，服务被安装引擎卸载重装时不受影响）：在临时克隆里按这一代同样的构建标记 go build，再用新程序 setup --yes，
//     和 scripts/bootstrap.sh 同一条路：新的一代装在新目录，旧的一代留着，状态、配置不动。
//   - 第一次升级前先在真实服务上演练一次整代回滚：用这一代自己再装一代、再 rollback 回来（两代代码一样，中间跑 tick 也无妨）。
//   - 观察期：升级后开始的 observeTasks 个任务结束，和升级前最近 baselineTasks 个比（失败或超时的比例、过门轮数）。
//     变差就 rollback 回上一代、推通知，写一条待做去修；带着回滚过的提交的版本不再升，直到修它的那一条合并（或用户自己装上）。
//     observeMaxDays 天还凑不够，按已有的判。有要做的事时不接新活，最多挡 holdMax。
//   - 每次演练、升级、留下、回滚、失败记一行 ~/.agentq/evolve/upgrades.jsonl，agentq evolve upgrades 看。
const (
	observeTasks   = 3
	baselineTasks  = 5
	observeMaxDays = 3
	upgradeRetry   = 6 * time.Hour
	gitStepWait    = 2 * time.Minute
	buildWait      = 15 * time.Minute
	versionWait    = 30 * time.Second
	// tickWait：动安装引擎前最多等前一个 tick 跑完（巡逻里的漏洞检查最长 5 分钟）。
	tickWait = 10 * time.Minute
	// holdMax：有要做的升级或回滚时最多挡这么久的新活，等不到空闲就先接活。
	holdMax = 2 * time.Hour
	// worseBad：观察期里失败或超时至少这么多个、比例又比升级前高，才算变差（一两个偶然失败不回滚）。
	worseBad = 2
	// worseRounds：平均过门轮数比升级前多这么多就算变差。
	worseRounds = 1.0
)

// 升级记录的种类。
const (
	EvDrill    = "drill" // 演练整代回滚通过
	EvUpgrade  = "upgrade"
	EvKept     = "kept"     // 观察期没变差，留下
	EvRollback = "rollback" // 观察期变差，回滚了；带着这个提交的版本不再升，等修它的合并
	EvFail     = "fail"     // 演练、升级或回滚没做成（演练的 Commit 是空的）
)

var evWords = map[string]string{EvDrill: "演练整代回滚", EvUpgrade: "升级", EvKept: "观察期没变差", EvRollback: "回滚", EvFail: "没做成"}

// UpgradeEvent 是 upgrades.jsonl 的一行。
type UpgradeEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// From、To 是版本（dev-<提交>）；Commit 是升到的（或回滚掉的）完整提交，Job 是合并它的任务。
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Commit string `json:"commit,omitempty"`
	Job    string `json:"job,omitempty"`
	// Release：自动更新（autoupdate.go）的记录，是这件事说的那个发行版本：要装的、装上后留下或回滚掉的。自己长大的记录是空的。
	Release string `json:"release,omitempty"`
	Why     string `json:"why,omitempty"`
}

var devVersionRe = regexp.MustCompile(`^dev-([0-9a-f]{7,40})(\+dirty)?$`)

func (s *Service) upgradesPath() string    { return filepath.Join(s.evolveDir(), "upgrades.jsonl") }
func (s *Service) upgradeLockPath() string { return filepath.Join(s.evolveDir(), "upgrade.lock") }
func (s *Service) upgradeLogPath() string  { return filepath.Join(s.evolveDir(), "upgrade.log") }

// receiptPath 是安装引擎的记录（internal/setup 的 Installer.ReceiptPath）：有它才是安装引擎装的，才能整代升级和回滚。
func (s *Service) receiptPath() string {
	return filepath.Join(s.Home, ".local", "share", "agentq", "receipt.json")
}

// LoadUpgrades 读升级记录。文件不存在是还没有；坏行跳过（记录只追加，坏一行不影响别的）。
func (s *Service) LoadUpgrades() ([]UpgradeEvent, error) {
	f, err := os.Open(s.upgradesPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", s.upgradesPath(), err)
	}
	defer f.Close()
	var out []UpgradeEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev UpgradeEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Kind != "" {
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读 %s：%w", s.upgradesPath(), err)
	}
	return out, nil
}

// appendUpgrade 追加一行（一次写完，几个进程同时追加也不会交错），当天汇总也记一行。
func (s *Service) appendUpgrade(ev UpgradeEvent) {
	b, err := json.Marshal(ev)
	if err == nil {
		err = os.MkdirAll(s.evolveDir(), 0o700)
	}
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(s.upgradesPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, err = f.Write(append(b, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		s.Logf("自己长大：升级记录没写上（%s）：%v", eventLine(ev), err)
	}
	s.Ledger.AppendNightlyLine(ev.At, "自己长大："+eventLine(ev))
}

func eventLine(ev UpgradeEvent) string {
	var b strings.Builder
	b.WriteString(evWords[ev.Kind])
	switch {
	case ev.From != "" && ev.To != "":
		fmt.Fprintf(&b, " %s → %s", ev.From, ev.To)
	case ev.To != "":
		b.WriteString(" " + ev.To)
	}
	if ev.Commit != "" {
		b.WriteString("（提交 " + short(ev.Commit))
		if ev.Job != "" {
			b.WriteString("，任务 " + ev.Job + " 合并的")
		}
		b.WriteString("）")
	}
	if ev.Why != "" {
		b.WriteString("：" + ev.Why)
	}
	return b.String()
}

// upgradePlan 是这次要做的事。Action 空就是什么都不做；Note 给 tick 日志看。
type upgradePlan struct {
	Action string // EvUpgrade、EvRollback
	Commit string
	Job    string
	// Release：自动更新要装的发行版本，或要回滚掉的（现在这一代）；自己长大的是空的。
	Release string
	Why     string
	Drill   bool // 升级前先演练整代回滚
	Note    string
}

// observation 是观察期的数：任务数、失败或超时的个数、过门轮数的和。
type observation struct{ N, Bad, Rounds int }

func (o observation) badRate() float64 {
	if o.N == 0 {
		return 0
	}
	return float64(o.Bad) / float64(o.N)
}

func (o observation) avgRounds() float64 {
	if o.N == 0 {
		return 0
	}
	return float64(o.Rounds) / float64(o.N)
}

func (o observation) String() string {
	return fmt.Sprintf("%d 个任务，失败或超时 %d 个，平均过门退回 %.1f 轮", o.N, o.Bad, o.avgRounds())
}

// countable：算进观察的任务。只算 BACKLOG 的待做、正常结束的；撞额度、撞预算、机器重启、上游抖动、用户停掉的不算。
func countable(j *state.Job) bool {
	if j.Kind != "" || j.EndedAt == nil || j.LimitHit || j.BudgetHit || j.Rebooted || j.Transient {
		return false
	}
	switch j.Status {
	case state.StatusDone, state.StatusFailed, state.StatusTimeout:
		return true
	}
	return false
}

func observe(jobs []*state.Job) observation {
	var o observation
	for _, j := range jobs {
		o.N++
		if j.Status == state.StatusFailed || j.Status == state.StatusTimeout {
			o.Bad++
		}
		o.Rounds += j.GateRound
	}
	return o
}

// windows：升级前结束的最近 baselineTasks 个，和升级后才开始、已结束的（按开始时间，最多 observeTasks 个）。
func windows(jobs []*state.Job, at time.Time) (before, after observation) {
	var pre, post []*state.Job
	for _, j := range jobs {
		if !countable(j) {
			continue
		}
		switch {
		case j.StartedAt.After(at):
			post = append(post, j)
		case j.EndedAt.Before(at):
			pre = append(pre, j)
		}
	}
	sort.SliceStable(pre, func(a, b int) bool { return pre[a].EndedAt.After(*pre[b].EndedAt) })
	sort.SliceStable(post, func(a, b int) bool { return post[a].StartedAt.Before(post[b].StartedAt) })
	if len(pre) > baselineTasks {
		pre = pre[:baselineTasks]
	}
	if len(post) > observeTasks {
		post = post[:observeTasks]
	}
	return observe(pre), observe(post)
}

// worse：观察期比升级前差。说不出来就是没变差。
func worse(before, after observation) (bool, string) {
	why := "升级前 " + before.String() + "；升级后 " + after.String()
	if after.Bad >= worseBad && after.badRate() > before.badRate() {
		return true, why
	}
	if after.N > 0 && after.avgRounds() >= before.avgRounds()+worseRounds {
		return true, why
	}
	return false, why
}

// openUpgrade：最近一次升级，后面还没有留下或回滚、装着的还是它（用户自己换过版本就不观察了）。没有返回 nil。
func openUpgrade(evs []UpgradeEvent, version string) *UpgradeEvent {
	for i := len(evs) - 1; i >= 0; i-- {
		switch evs[i].Kind {
		case EvKept, EvRollback:
			return nil
		case EvUpgrade:
			if evs[i].To != version {
				return nil
			}
			return &evs[i]
		}
	}
	return nil
}

// recentFail：这个提交（空的是演练）或发行版本 upgradeRetry 内没做成过，先不再试。
func recentFail(evs []UpgradeEvent, commit, release string, now time.Time) bool {
	return slices.ContainsFunc(evs, func(ev UpgradeEvent) bool {
		return ev.Kind == EvFail && ev.Commit == commit && ev.Release == release && now.Sub(ev.At) < upgradeRetry && !ev.At.After(now)
	})
}

// evalPassed：这个任务引擎合并过、评测门过了。
func evalPassed(j *state.Job) bool {
	g := j.Gate
	if g == nil || g.State != state.GateMerged || g.Merge == "" {
		return false
	}
	t := g.Tally["eval"]
	return t != nil && t.Pass > 0
}

func endedAt(j *state.Job) time.Time {
	if j.EndedAt != nil {
		return *j.EndedAt
	}
	return j.StartedAt
}

func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := runGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// carriesRollback：候选带着回滚过的提交（是它的后代），现在这一代又没有它（用户自己装上了就算放行），
// 也还没有修它的那一条合并进来（selfgrow.go 回滚后写的待做，Fixes[RollbackKey]）。返回挡住它的那个提交。
func carriesRollback(ctx context.Context, dir, c, cur string, evs []UpgradeEvent, fixes map[string][]string) string {
	for _, ev := range evs {
		if ev.Kind != EvRollback || ev.Commit == "" || !isAncestor(ctx, dir, ev.Commit, c) || isAncestor(ctx, dir, ev.Commit, cur) {
			continue
		}
		if !slices.ContainsFunc(fixes[RollbackKey(ev.Commit)], func(f string) bool { return isAncestor(ctx, dir, f, c) }) {
			return ev.Commit
		}
	}
	return ""
}

// plan 定这次要不要升级、回滚：配了 self_project 的看自己长大，发行版看自动更新（autoupdate.go）。观察期满了没变差会在这里记「留下」。只读 git，不改东西。
func (s *Service) plan(ctx context.Context, st *state.State, now time.Time) upgradePlan {
	switch {
	case s.Cfg.Paused:
		return upgradePlan{}
	case s.SelfGrowOn():
		return s.selfPlan(ctx, st, now)
	case s.AutoUpdateOn():
		return s.releasePlan(st, now)
	}
	return upgradePlan{}
}

// observing 看最近一次升级的观察期：还在观察或要回滚时 done 为真，pl 就是这次的结果；观察期满了没变差记「留下」，返回的记录带上这一行。
func (s *Service) observing(evs []UpgradeEvent, st *state.State, now time.Time) (pl upgradePlan, out []UpgradeEvent, done bool) {
	up := openUpgrade(evs, s.Version)
	if up == nil {
		return upgradePlan{}, evs, false
	}
	before, after := windows(st.Jobs, up.At)
	if after.N < observeTasks && now.Sub(up.At) < observeMaxDays*24*time.Hour {
		return upgradePlan{Note: fmt.Sprintf("升级到 %s 后观察中：%d/%d 个任务", s.Version, after.N, observeTasks)}, evs, true
	}
	bad, why := worse(before, after)
	switch {
	case bad && recentFail(evs, up.Commit, up.Release, now):
		return upgradePlan{Note: "回滚 " + s.Version + " 没做成，" + upgradeRetry.String() + " 内不再试"}, evs, true
	case bad:
		return upgradePlan{Action: EvRollback, Commit: up.Commit, Job: up.Job, Release: up.Release, Why: why}, evs, true
	}
	ev := UpgradeEvent{At: now, Kind: EvKept, To: s.Version, Commit: up.Commit, Job: up.Job, Release: up.Release, Why: why}
	s.appendUpgrade(ev)
	return upgradePlan{}, append(evs, ev), false
}

// selfPlan：自己长大要不要升级、回滚。
func (s *Service) selfPlan(ctx context.Context, st *state.State, now time.Time) upgradePlan {
	m := devVersionRe.FindStringSubmatch(s.Version)
	if m == nil {
		return upgradePlan{} // 发行版、认不出提交的构建：走 agentq update
	}
	if _, err := os.Stat(s.receiptPath()); err != nil {
		return upgradePlan{} // 不是安装引擎装的，没有整代可换
	}
	p, ok := s.selfProjectAny()
	if !ok {
		return upgradePlan{}
	}
	evs, err := s.LoadUpgrades()
	if err != nil {
		return upgradePlan{Note: "升级记录读不了，先不升级：" + err.Error()}
	}
	pl, evs, done := s.observing(evs, st, now)
	if done {
		return pl
	}
	sig, err := s.LoadSignals()
	if err != nil {
		return upgradePlan{Note: "信号文件读不了，先不升级：" + err.Error()}
	}
	cur := m[1]
	var cands []*state.Job
	for _, j := range st.Jobs {
		if j.Project == p.Name && evalPassed(j) {
			cands = append(cands, j)
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return endedAt(cands[a]).After(endedAt(cands[b])) })
	note := ""
	for _, j := range cands {
		c := j.Gate.Merge
		if strings.HasPrefix(c, cur) || isAncestor(ctx, p.Dir, c, cur) {
			break // 最新的已经装着；再往前的都装过了
		}
		if !isAncestor(ctx, p.Dir, cur, c) {
			continue // 岔路上的：不换过去
		}
		if b := carriesRollback(ctx, p.Dir, c, cur, evs, sig.Fixes); b != "" {
			note = "合并提交 " + short(c) + " 带着回滚过的 " + short(b) + "，等修它的那一条合并（或你自己装上）再升"
			continue
		}
		if recentFail(evs, c, "", now) {
			return upgradePlan{Note: "升级到 " + short(c) + " 没做成，" + upgradeRetry.String() + " 内不再试"}
		}
		drill := !slices.ContainsFunc(evs, func(ev UpgradeEvent) bool { return ev.Kind == EvDrill })
		if drill && recentFail(evs, "", "", now) {
			return upgradePlan{Note: "整代回滚演练没过，" + upgradeRetry.String() + " 内不再试，先不升级"}
		}
		return upgradePlan{Action: EvUpgrade, Commit: c, Job: j.ID, Drill: drill}
	}
	return upgradePlan{Note: note}
}

// activeJobs：在跑的任务和正在过门、排队过门的任务。安装引擎会卸载重装服务、换掉程序，这些都做完才动。
func activeJobs(st *state.State) int {
	n := 0
	for _, j := range st.Jobs {
		if j.Status == state.StatusRunning || j.Gate != nil && (j.Gate.State == state.GateQueued || j.Gate.State == state.GateRunning) {
			n++
		}
	}
	return n
}

// holdMark 记下这次升级（或回滚）从什么时候开始挡着接新活，超过 holdMax 就放开。
type holdMark struct {
	Key   string    `json:"key"`
	Since time.Time `json:"since"`
}

func (s *Service) holdPath() string { return filepath.Join(s.evolveDir(), "hold.json") }

// heldFor：这件事已经挡了多久；第一次挡（或 restart）就从现在算。
func (s *Service) heldFor(key string, now time.Time, restart bool) time.Duration {
	var h holdMark
	if b, err := os.ReadFile(s.holdPath()); !restart && err == nil && json.Unmarshal(b, &h) == nil && h.Key == key && !h.Since.After(now) {
		return now.Sub(h.Since)
	}
	if b, err := json.Marshal(holdMark{Key: key, Since: now}); err == nil {
		if err := os.MkdirAll(s.evolveDir(), 0o700); err == nil {
			_ = fsx.WriteFileAtomic(s.holdPath(), b) // 记不下就每次从现在算：最多是一直挡到空闲，和没有上限一样
		}
	}
	return 0
}

// SelfUpdate：tick 在挑活之前调用（拿着 tick.lock）。有要做的升级或回滚时 hold 为真：这次不接新活，
// 没有任务在跑、没有在过门的就起 agentq evolve apply 去做。挡了 holdMax 还没空闲就放开接活，等下次自然空闲。出错只记日志，不影响 tick。
func (s *Service) SelfUpdate(st *state.State) (hold bool, note string) {
	defer func() {
		if p := recover(); p != nil {
			s.Logf("自己长大：看升级出错，不影响 tick：%v", p)
			hold, note = false, ""
		}
	}()
	now := s.Now()
	ctx := context.Background()
	if s.AutoUpdateOn() && !s.Cfg.Paused {
		s.checkRelease(ctx, now)
	}
	pl := s.plan(ctx, st, now)
	if pl.Action == "" {
		_ = os.Remove(s.holdPath())
		return false, pl.Note
	}
	what := "自己长大：升级到 " + short(pl.Commit)
	switch {
	case pl.Release != "" && pl.Action == EvRollback:
		what = "自动更新：回滚 " + s.Version
	case pl.Release != "":
		what = "自动更新：装 " + pl.Release
	case pl.Action == EvRollback:
		what = "自己长大：回滚 " + s.Version
	}
	key := pl.Action + ":" + pl.Commit + pl.Release
	_, _, applying := fsx.LockHolder(s.upgradeLockPath())
	n := activeJobs(st)
	if !applying && n == 0 { // 空闲：现在就做，挡新活从这时算起
		if err := s.spawnApply(); err != nil {
			s.Logf("自己长大：起不来 agentq evolve apply：%v", err)
			return false, ""
		}
		s.heldFor(key, now, true)
		return true, what + "：已开始（agentq evolve apply，日志 " + s.Tilde(s.upgradeLogPath()) + "），这次不接新活"
	}
	if s.heldFor(key, now, false) >= holdMax {
		return false, fmt.Sprintf("%s，已经挡了 %s 新活还没空闲，先照常接活，下次空闲再做", what, holdMax)
	}
	if applying {
		return true, what + "：正在做，这次不接新活"
	}
	return true, fmt.Sprintf("%s：等 %d 个在跑或在过门的任务做完，这次不接新活", what, n)
}

// runGit 在 dir 跑 git（不跑钩子、有超时）。
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitStepWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir, cmd.Env = dir, proc.ChildEnv()
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s：%w：%s", strings.Join(args, " "), err, textfmt.OneLine(string(out), 200))
	}
	return strings.TrimSpace(string(out)), nil
}
