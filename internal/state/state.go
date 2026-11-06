// Package state 定义任务记录：state.json、meta.json 的结构、读写和查询。Job 是共享黑板，字段顺序和标签决定 JSON 字节，不能动。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/textfmt"
)

const KindChat = "chat"

// SortJobsByEnd：结束得晚的在前。
func SortJobsByEnd(jobs []*Job) {
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].EndedAt.After(*jobs[k].EndedAt) })
}

const KindInbox = "inbox"

const KindNew = "new"

// NewReport 是结果最后一段写的四项；Path 核对后换成真实路径。Commit 是引擎合并后填的合并提交。
type NewReport struct {
	Path        string `json:"path"`
	Assumptions string `json:"assumptions"`
	View        string `json:"view"`
	Commit      string `json:"commit"`
	Branch      string `json:"branch,omitempty"`
	// Root：这个任务的会话是在 projects_root 建的（new 任务本身）。接手、续跑的任务不带。
	Root string `json:"root,omitempty"`
}

// Next 是接着做第一版的任务带的报告：还是第一版，会话是它自己的。
func (r *NewReport) Next() *NewReport {
	if r == nil {
		return nil
	}
	next := *r
	next.Root = ""
	return &next
}

// SessionInRoot：会话是在 projects_root 建的，项目目录里接不回去：续跑不了，只读追问要回 projects_root。
func (j *Job) SessionInRoot() bool { return j.NewReport != nil && j.NewReport.Root != "" }

// FirstItem：new 任务结尾报了项目，第一条却没做到分支可合并（任务没改认，还是 new）。记下是哪个项目的哪一条：第一版合并之前，这个项目后面的条目不开工。
type FirstItem struct {
	Project     string `json:"project"`
	Dir         string `json:"dir"`
	Fingerprint string `json:"fingerprint"`
	Title       string `json:"title"`
	Branch      string `json:"branch,omitempty"`
	View        string `json:"view,omitempty"`
	Assumptions string `json:"assumptions,omitempty"`
}

// FirstTarget：这个任务在做哪个新项目的第一版：改认过的任务（包括接手、续跑的）和没改认但记了第一条的 new。升级前自己合并的 new（Kind 还是 new、有 NewReport）不算。
func (j *Job) FirstTarget() (project, dir, fingerprint, title string, ok bool) {
	switch {
	case j.FirstItem != nil:
		return j.FirstItem.Project, j.FirstItem.Dir, j.FirstItem.Fingerprint, j.FirstItem.Title, true
	case j.Kind == "" && j.NewReport != nil:
		return j.Project, j.ProjectDir, j.Fingerprint, j.Title, true
	}
	return "", "", "", "", false
}

// NewProjectBusy：name 是在跑或等着接着做的 new 任务启动后才出现的项目。它的条目还归那个任务管，不挑、不当孤儿。
func (st *State) NewProjectBusy(name string) bool {
	for _, j := range st.Jobs {
		if j.Kind == KindNew && (j.Status == StatusRunning || j.ResumePending) && !slices.Contains(j.ProjectsBefore, name) {
			return true
		}
	}
	return false
}

// FirstVersionJob：这个项目第一版最新的任务，没有就是空。
func (st *State) FirstVersionJob(project string) *Job {
	var last *Job
	for _, j := range st.Jobs {
		if p, _, _, _, ok := j.FirstTarget(); ok && p == project && (last == nil || j.StartedAt.After(last.StartedAt)) {
			last = j
		}
	}
	return last
}

// HeldProject：name 是用户停掉的 new 任务留下的目录时为真，tick 不挑它的待做。
func (st *State) HeldProject(name string) bool {
	return slices.ContainsFunc(st.Jobs, func(j *Job) bool { return slices.Contains(j.HeldProjects, name) })
}

const KindPatrol = "patrol"

const KindRelease = "release"

const KindRouter = "router"

const KindQuick = "quick"

func HasRunning(st *State, kind string) bool {
	for _, j := range st.Running() {
		if j.Kind == kind {
			return true
		}
	}
	return false
}

// RoutineMark 是夜班、复盘上次启动前看到的现状。记在启动时，不记在结束时：跑的过程中别人的改动下次才看得到。
type RoutineMark struct {
	JobID string    `json:"job_id"`
	At    time.Time `json:"at"`
	// Heads：项目名 → HEAD。
	Heads map[string]string `json:"heads"`
	// Issues：仓库（owner/name）→ 最新 issue 的更新时间，没有 issue 是 ghNoIssue。
	Issues map[string]string `json:"issues,omitempty"`
}

const KindSense = "sense"

// KindDiscuss：开工前生成大方向问题的一次性任务（见 internal/discuss）。不占名额，只调一次模型。
const KindDiscuss = "discuss"

const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
	StatusKilled  = "killed"
	// StatusInterrupted：claude 正常退出（is_error 为假），但这一条其实没做完（BACKLOG 仍标进行中，或结果里没写合并情况）。
	StatusInterrupted = "interrupted"
)

// 结束的任务在 state.json 里保留多久（retry_after 和 status 只需要最近的）。
const KeepEndedJobs = 30 * 24 * time.Hour

type Job struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	ProjectDir string `json:"project_dir"`
	// DefaultBranch：这条任务基于的分支，起任务时认一次，和提示词、认领、开分支是同一个值。续跑、接手、退回沿用，过门的基点、合并、收尾都走它，不再现认。老任务没有，才现认。
	DefaultBranch string     `json:"default_branch,omitempty"`
	Item          string     `json:"item"`
	Title         string     `json:"title"`
	Fingerprint   string     `json:"fingerprint"`
	PID           int        `json:"pid"`
	StartedAt     time.Time  `json:"started_at"`
	Status        string     `json:"status"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	Summary       string     `json:"summary,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	// LimitHit 为真表示这次是撞了用量上限，不算一次「试过」，冷却过后可以再试。
	LimitHit bool     `json:"limit_hit,omitempty"`
	Args     []string `json:"args,omitempty"`
	// Backend：任务用的后端。空表示默认后端（老记录都是），老文件字节不变。续跑用原后端，不跟随之后的配置。
	Backend string `json:"backend,omitempty"`
	// ResumePending：中断了、等下一次能启动时用同一会话续跑；ResumedBy 是续跑任务的 id。
	ResumePending bool   `json:"resume_pending,omitempty"`
	ResumedBy     string `json:"resumed_by,omitempty"`
	// ResumeOf 非空表示这个任务是续跑，值是被续跑的任务 id。续跑任务再中断就不再续跑。
	ResumeOf string `json:"resume_of,omitempty"`
	// BootTime 是启动任务时的开机时间（kern.boottime 的秒数，0 = 没记）。和现在不一致说明中途重启过：
	// 记下的 pid 和进程组号可能已经被别的程序占用，不能再拿来判断或清理。
	BootTime int64 `json:"boot_time,omitempty"`
	// Rebooted：任务因为机器重启而中断（不是 agent 的问题，不计连环失败）。
	Rebooted bool `json:"rebooted,omitempty"`
	// Takeover：接手孤儿「进行中」条目的任务（见 internal/orphan/orphan.go）。同一指纹只接手 1 次，靠这条记录判断；没做完不续跑，下次 tick 标卡住。
	Takeover bool `json:"takeover,omitempty"`
	// Transient：上游瞬时错误且几乎没花钱（见 internal/runner/transient.go），不计连环失败，按短退避重试。
	Transient bool `json:"transient,omitempty"`
	// StopNoted：用户停掉的任务标记还留着，已经记过日志（见 internal/orphan/orphan.go）。为真时这条记录不过期，免得 30 天后条目被自动接手。
	StopNoted bool `json:"stop_noted,omitempty"`
	// StallNoted：已经在汇总里报过「疑似卡死」，会话记录再长起来之前不重复报。
	StallNoted bool `json:"stall_noted,omitempty"`
	// ProgressNoted：checkProgress 已经报过的信号（见 internal/runner/progress.go），同一信号不重报。
	ProgressNoted []string `json:"progress_noted,omitempty"`
	// Kind 非空表示不是 BACKLOG 里的待做：不核验中断、不续跑。任务箱（inbox）和 new 占并发名额，其余不占，见 takesSlot。
	Kind string `json:"kind,omitempty"`
	// Output：任务箱任务（Kind=inbox）的产出文件路径；InboxSynced：结果已写回 inbox.md，没写回去的下次 tick 再写。
	Output      string `json:"output,omitempty"`
	InboxSynced bool   `json:"inbox_synced,omitempty"`
	// ProjectsBefore：new 任务启动时 projects_root 下已有的名字。之后才出现的目录才可能是它建的（见 internal/newproj/newproj.go）。
	ProjectsBefore []string `json:"projects_before,omitempty"`
	// NewReport：new 任务核对通过的结尾四项。有它的任务是新项目第一版（Kind 已经改回空），过成品门；接手的任务也带着。
	NewReport *NewReport `json:"new_report,omitempty"`
	// FirstItem：见 FirstItem 类型。FirstNote：第一版挡着后面的条目，日志里已经说过的原因，同样的不再说。
	FirstItem *FirstItem `json:"first_item,omitempty"`
	FirstNote string     `json:"first_note,omitempty"`
	// HeldProjects：用户停掉 new 任务时 projects_root 下多出的目录，tick 不自动挑（见 internal/newproj/newproj.go）。记在任务上，状态文件损坏时随 meta.json 重建。
	HeldProjects []string `json:"held_projects,omitempty"`
	// FromThread：用户回复消息接回原会话启动的（见 internal/chat/threads.go），秒退时消息放回收件箱按新消息处理。
	FromThread bool `json:"from_thread,omitempty"`
	// ThreadFP、ThreadDir：只读续跑（Kind=followup）来源条目的指纹和项目目录。它自己不写 Fingerprint，免得算成这一条试过一次。
	ThreadFP  string `json:"thread_fp,omitempty"`
	ThreadDir string `json:"thread_dir,omitempty"`
	// BudgetHit：撞的是 agentq 设的预算（--max-budget-usd，见 internal/runner/budget.go）。不算失败，不计连环失败，已经推过，巡逻不再报。
	BudgetHit bool `json:"budget_hit,omitempty"`
	// AutoStop：agentq 按信号自动停的（值是信号名，见 internal/runner/budget.go）。不是用户 stop：状态记中断，不用 statusKilled。
	AutoStop string `json:"auto_stop,omitempty"`
	// StuckPending：要写进 BACKLOG 的卡住原因，写上就清空；主目录一时提交不了时下次 tick 再写，这期间这一条不接手、不重挑。
	StuckPending string `json:"stuck_pending,omitempty"`
	// StuckMarked：agentq 自己写上了卡住标记（见 writeStuckMark）。之后条目又能被挑到，就是用户去掉了标记：不等 retry_after，从记下的分支接着做。
	StuckMarked bool `json:"stuck_marked,omitempty"`
	// Handoff：用户去掉卡住标记后，从 StuckBranch 接着做的任务。它认领之前就结束（5xx、秒退）时，下一个任务还要接这个分支；
	// 结束时看到认领过（条目标了进行中、卡住，或已不在待做）就清掉，之后由它自己的标记管。
	Handoff bool `json:"handoff,omitempty"`
	// StillListed：报告做完了，条目却还在「待做」（没标进行中）。连续两次就标卡住，不再按 retry_after 一遍遍重做（见 noteStillListed）。
	StillListed bool `json:"still_listed,omitempty"`
	// StuckBranch、StuckWorktree：放下时这一条的分支和 worktree（Handoff 的任务是接手的来源分支）。标记去掉后分支名就只有这里有了。
	StuckBranch   string `json:"stuck_branch,omitempty"`
	StuckWorktree string `json:"stuck_worktree,omitempty"`
	// 闲时找活任务（Kind=suggest，见 internal/suggest/suggest.go）：SuggestProjects 是启动时交给模型看的项目（结果里的 project 只能是它们）；
	// Manual 是 agentq suggest 手动启动的；Suggest 是核对并写进 BACKLOG 之后的结果。
	SuggestProjects []string `json:"suggest_projects,omitempty"`
	Manual          bool     `json:"manual,omitempty"`
	// SuggestSnaps：启动时各项目的样子；输出合格、处理完之后才存成“上次生成时的样子”（见 applySuggestions）。
	SuggestSnaps map[string]SuggestSnap `json:"suggest_snaps,omitempty"`
	Suggest      *SuggestResult         `json:"suggest,omitempty"`
	// Usage 是结束时从 result.json 读到的用量（token、费用估算），结果文件里没有就为空。
	Usage *backend.JobUsage `json:"usage,omitempty"`
	// Verify：升级前的「合并后核验」留下的记录，只读。新任务不写，见 MergeVerify。
	Verify *MergeVerify `json:"verify,omitempty"`
	// Gate：任务写了 ready.json 之后过门的记录（见 internal/gate）。没写 ready.json 的任务为空。
	Gate *GateRecord `json:"gate,omitempty"`
	// GateRound：这个任务是门退回的第几轮，0 是第一次做。
	GateRound int `json:"gate_round,omitempty"`
	// GateContract：按「任务只做到分支可合并」的合同启动的。升级前启动的任务没有，它们可能自己合并，收尾时不算绕过门。
	GateContract bool `json:"gate_contract,omitempty"`
	// DiscussKey：Kind=discuss 的任务在给哪一条生成问题，见 internal/discuss。
	DiscussKey string `json:"discuss_key,omitempty"`
	// Direction：agentq new 商量定的大方向，进提示词，撞预算接着做时还要用。条目的方向写在条目上，不用这个。
	Direction string `json:"direction,omitempty"`
}

type State struct {
	Jobs          []*Job    `json:"jobs"`
	CooldownUntil time.Time `json:"cooldown_until"`
	// 连环失败熔断：BreakerUntil 前不启动新任务；FailStreak 是连续秒退的任务数；
	// BreakerLevel 是熔断后还没有任务正常结束过的次数，大于 0 时再秒退一个就直接熔断，时长按它翻倍。
	BreakerUntil time.Time `json:"breaker_until"`
	FailStreak   int       `json:"fail_streak,omitempty"`
	BreakerLevel int       `json:"breaker_level,omitempty"`
	// ItemHold：holdKey（项目|指纹；老状态文件里只有指纹，到期自然清掉）→ 这个时间前 tick 不挑（孤儿标记去掉后多半是活已合并，见 internal/orphan/orphan.go）。agentq run 不受影响。
	ItemHold map[string]time.Time `json:"item_hold,omitempty"`
	// BranchBlocked：「项目|任务记下的分支」（新任务时分支是空的）→ 现在接不了任务的原因（认不出默认分支、主目录不在要的分支）。同样的原因日志只记一次，好了就删。
	BranchBlocked map[string]string `json:"branch_blocked,omitempty"`
	// Usage 是最近一次读到的订阅额度（claude -p /usage），缓存 usageCacheFor。
	Usage *backend.UsageInfo `json:"usage,omitempty"`
	// BackendLimits：默认后端之外的后端的冷却和额度（后端名 → 记录）。默认后端的还在 cooldown_until、usage：老文件读出来就是默认后端的，只用一种后端时这个键不出现。
	BackendLimits map[string]*BackendLimit `json:"backend_limits,omitempty"`
	// RoutineMarks：夜班、复盘上次启动时看到的现状（kind → 记录，见 internal/routine/routine.go）。没有记录就照跑。
	RoutineMarks map[string]*RoutineMark `json:"routine_marks,omitempty"`
}

// BackendLimit 是一个后端自己的冷却和额度。
type BackendLimit struct {
	CooldownUntil time.Time          `json:"cooldown_until,omitzero"`
	Usage         *backend.UsageInfo `json:"usage,omitempty"`
}

// limitName：后端名空是 Claude（和 Job.Backend 一样）。
func limitName(name string) string {
	if name == "" {
		return backend.Default
	}
	return name
}

// CooldownOf 是这个后端的用量冷却，def 是配置的默认后端。
func (s *State) CooldownOf(name, def string) time.Time {
	if limitName(name) == limitName(def) {
		return s.CooldownUntil
	}
	if l := s.BackendLimits[limitName(name)]; l != nil {
		return l.CooldownUntil
	}
	return time.Time{}
}

// ExtendCooldown 把这个后端的冷却推到 until，已经更晚就不动。
func (s *State) ExtendCooldown(name, def string, until time.Time) {
	if !until.After(s.CooldownOf(name, def)) {
		return
	}
	if limitName(name) == limitName(def) {
		s.CooldownUntil = until
		return
	}
	s.backendLimit(name).CooldownUntil = until
}

// UsageOf 是这个后端最近读到的额度。
func (s *State) UsageOf(name, def string) *backend.UsageInfo {
	if limitName(name) == limitName(def) {
		return s.Usage
	}
	if l := s.BackendLimits[limitName(name)]; l != nil {
		return l.Usage
	}
	return nil
}

// SetUsageOf 存这个后端读到的额度。
func (s *State) SetUsageOf(name, def string, u *backend.UsageInfo) {
	if limitName(name) == limitName(def) {
		s.Usage = u
		return
	}
	s.backendLimit(name).Usage = u
}

func (s *State) backendLimit(name string) *BackendLimit {
	if s.BackendLimits == nil {
		s.BackendLimits = map[string]*BackendLimit{}
	}
	l := s.BackendLimits[limitName(name)]
	if l == nil {
		l = &BackendLimit{}
		s.BackendLimits[limitName(name)] = l
	}
	return l
}

// AllCooling：配置用到的后端都在冷却，这次没有能接活的。
func (s *State) AllCooling(cfg config.Config, now time.Time) bool {
	for _, name := range cfg.BackendsInUse() {
		if !now.Before(s.CooldownOf(name, cfg.Backend)) {
			return false
		}
	}
	return true
}

// OtherLimitNotes 是默认后端之外、配置用到的后端现在接不了活的原因，每个一句。
func (s *State) OtherLimitNotes(cfg config.Config, now time.Time) []string {
	var out []string
	for _, name := range cfg.BackendsInUse() {
		if name == limitName(cfg.Backend) {
			continue
		}
		if until := s.CooldownOf(name, cfg.Backend); now.Before(until) {
			out = append(out, "后端 "+name+" 用量冷却中，到 "+textfmt.WhenCN(until))
			continue
		}
		if lvl, until, why := backend.UsageLevel(s.UsageOf(name, cfg.Backend), now, cfg.ForBackend(name).Model); lvl >= backend.UsageStop {
			out = append(out, "后端 "+name+"："+backend.UsageGateNote(lvl, until, why))
		}
	}
	return out
}

// OtherUsageLines 是默认后端之外、配置用到的后端的额度，每个一句。只用一种后端时没有。
func (s *State) OtherUsageLines(cfg config.Config, now time.Time) []string {
	var out []string
	for _, name := range cfg.BackendsInUse() {
		if name != limitName(cfg.Backend) {
			out = append(out, name+"："+backend.UsageDescribe(s.UsageOf(name, cfg.Backend), now, cfg.ForBackend(name).Model))
		}
	}
	return out
}

// ErrStateCorrupt：state.json 读得出但内容坏了（不是没权限、读不了）。tick 遇到它会备份坏文件、按各任务的 meta.json 重建。
var ErrStateCorrupt = errors.New("状态文件内容损坏")

// Load：文件不存在是正常的第一次运行，返回空状态；存在但读不出返回错误，调用方据此当作「未知」。
// 内容坏了的错误包着 errStateCorrupt。
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w：%s 格式不对：%w", ErrStateCorrupt, path, err)
	}
	for _, j := range st.Jobs {
		if j == nil {
			return nil, fmt.Errorf("%w：%s 里有空任务记录", ErrStateCorrupt, path)
		}
	}
	// saveState 一定写 jobs。没有这个键的（null、{}）不是状态文件：当成空状态存回去，运行中的任务就全忘了
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil || keys["jobs"] == nil {
		return nil, fmt.Errorf("%w：%s 里没有 jobs", ErrStateCorrupt, path)
	}
	return &st, nil
}

func Save(path string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'))
}

func (st *State) Running() []*Job {
	var out []*Job
	for _, j := range st.Jobs {
		if j.Status == StatusRunning {
			out = append(out, j)
		}
	}
	return out
}

func (st *State) Find(id string) (*Job, error) {
	var hits []*Job
	for _, j := range st.Jobs {
		if j.ID == id {
			return j, nil
		}
		if len(id) >= 4 && len(j.ID) > len(id) && j.ID[:len(id)] == id {
			hits = append(hits, j)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("没有任务 %s（agentq status 看任务 id）", id)
	default:
		return nil, fmt.Errorf("任务 id 前缀 %s 对应多个任务，请写完整", id)
	}
}

// Prune 去掉结束超过 keepEndedJobs 的记录。还留着目录的（HeldProjects）不去：去掉了 tick 就会开始做用户停掉的项目。门上卡住等用户定的也不去：去掉了 gate pass、gate abandon 都找不到它。
func (st *State) Prune(now time.Time) {
	kept := st.Jobs[:0]
	for _, j := range st.Jobs {
		if j.Status != StatusRunning && j.EndedAt != nil && now.Sub(*j.EndedAt) > KeepEndedJobs && !j.StopNoted && len(j.HeldProjects) == 0 && !GateActive(j) && !GateStuckNow(j) {
			continue
		}
		kept = append(kept, j)
	}
	st.Jobs = kept
	for fp, until := range st.ItemHold {
		if !now.Before(until) {
			delete(st.ItemHold, fp)
		}
	}
}

// TakesSlot：占并发名额、算「在做的活」的任务：BACKLOG 待做、任务箱、new 和发版准备。夜班、复盘、巡逻分析、对话、闲时找活、主动感知不算。
func TakesSlot(kind string) bool {
	return kind == "" || kind == KindInbox || kind == KindNew || kind == KindRelease
}

// Tasks 返回运行中占名额的任务（待做、任务箱和 new，不含夜班、复盘）。
func (st *State) Tasks() []*Job {
	var out []*Job
	for _, j := range st.Running() {
		if TakesSlot(j.Kind) {
			out = append(out, j)
		}
	}
	return out
}

// Capacity 判断全局和单项目上限，返回能否再为 project 启动一个任务及原因。夜班、复盘不占名额：
// 夜班一跑就是几个小时，就是在等这些名额上的任务做完。
func Capacity(st *State, cfg config.Config, project string) (bool, string) {
	total, inProject := 0, 0
	for _, j := range st.Tasks() {
		total++
		if j.Project == project {
			inProject++
		}
	}
	gating := len(st.Gating()) // 门进程计入全局上限，不算项目的
	if total+gating >= cfg.MaxParallel {
		if gating > 0 {
			return false, fmt.Sprintf("全局已有 %d 个任务在跑、%d 个在过门（上限 max_parallel=%d）", total, gating, cfg.MaxParallel)
		}
		return false, fmt.Sprintf("全局已有 %d 个任务在跑（上限 max_parallel=%d）", total, cfg.MaxParallel)
	}
	if inProject >= cfg.PerProject {
		return false, fmt.Sprintf("项目 %s 已有 %d 个任务在跑（上限 per_project=%d）", project, inProject, cfg.PerProject)
	}
	return true, ""
}

// PendingResumes 返回等着续跑的中断任务，按记录顺序。
func (st *State) PendingResumes() []*Job {
	var out []*Job
	for _, j := range st.Jobs {
		if j.Status == StatusInterrupted && j.ResumePending {
			out = append(out, j)
		}
	}
	return out
}

// HoldKey：ItemHold 的键。不同项目里一样文字的条目指纹相同，要连项目一起记。
func HoldKey(project, fp string) string { return project + "|" + fp }

// itemHeld：这一条在等待期内。老状态文件的键只有指纹，照样认；不知道项目（测试、任务箱）时哪个项目的都算。
func (st *State) itemHeld(project, fp string, now time.Time) bool {
	if until, ok := st.ItemHold[fp]; ok && now.Before(until) {
		return true
	}
	if project != "" {
		until, ok := st.ItemHold[HoldKey(project, fp)]
		return ok && now.Before(until)
	}
	for k, until := range st.ItemHold {
		if strings.HasSuffix(k, "|"+fp) && now.Before(until) {
			return true
		}
	}
	return false
}

// SameItem：任务 j 做的是 project 里指纹为 fp 的条目。任何一边没写项目就只比指纹（任务箱的指纹带前缀，不会撞）。
func SameItem(j *Job, project, fp string) bool {
	return j.Fingerprint == fp && (project == "" || j.Project == "" || j.Project == project)
}

// RunningFor 返回这个项目里指纹相同且还在跑的任务。
func (st *State) RunningFor(project, fp string) *Job {
	for _, j := range st.Running() {
		if SameItem(j, project, fp) {
			return j
		}
	}
	return nil
}

// RetryBlocked：同一条目最近一次结束（撞用量上限的不算）距今不足 retryAfter 时为真；那次是上游瞬时错误时按短退避算（见 retryWait）。
// 调用方只拿「待做」里没标进行中的条目来问，所以「那次之后条目仍在待做里未标进行中」已经成立。
func RetryBlocked(st *State, project, fp string, now time.Time, retryAfter time.Duration) bool {
	if st.itemHeld(project, fp, now) {
		return true
	}
	var last *Job
	for _, j := range st.Jobs {
		if !SameItem(j, project, fp) {
			continue
		}
		if j.StuckPending != "" { // 卡住标记还没写上：不能当普通条目再挑
			return true
		}
		if j.Status == StatusRunning || j.EndedAt == nil || j.LimitHit {
			continue
		}
		if last == nil || j.EndedAt.After(*last.EndedAt) {
			last = j
		}
	}
	if last != nil && last.StuckMarked { // 用户去掉了 agentq 写的卡住标记才挑得到这里：他已经表态了
		return false
	}
	return last != nil && now.Sub(*last.EndedAt) < retryWait(st, last, retryAfter)
}

const KindSuggest = "suggest"

// SuggestSnap 是上次生成时一个项目的样子：git HEAD 和「以后再说」一节的内容摘要。
type SuggestSnap struct {
	Head  string `json:"head"`
	Later string `json:"later"`
}

// SuggestResult 是 Job.Suggest：核对并写进 BACKLOG 之后的结果，存在任务记录里。
type SuggestResult struct {
	Candidates int      `json:"candidates"`
	Todo       []string `json:"todo,omitempty"`     // 进「待做」的，「项目：标题」
	Proposed   []string `json:"proposed,omitempty"` // 进「建议」的，「编号　项目　标题」
	Skipped    []string `json:"skipped,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	// NewFailed：Failed 里这次该推送的（同一项目同样的原因之前推过就不在里面）。
	NewFailed []string `json:"new_failed,omitempty"`
}

func (r SuggestResult) Written() int { return len(r.Todo) + len(r.Proposed) }

func (r SuggestResult) Summary() string {
	s := fmt.Sprintf("收到 %d 条候选，写进「待做」%d 条（低风险）、「建议」%d 条", r.Candidates, len(r.Todo), len(r.Proposed))
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf("；跳过 %d 条（%s）", len(r.Skipped), textfmt.OneLine(strings.Join(r.Skipped, "；"), 200))
	}
	if len(r.Failed) > 0 {
		s += "；没写进去：" + textfmt.OneLine(strings.Join(r.Failed, "；"), 200)
	}
	return s
}

const KindFollowup = "followup"

// IsChatKind：回答要发给用户的几种任务。
func IsChatKind(kind string) bool {
	return kind == KindChat || kind == KindFollowup || kind == KindRouter || kind == KindQuick
}

func StatusLabel(s string) string {
	switch s {
	case StatusRunning:
		return "运行中"
	case StatusDone:
		return "跑完"
	case StatusFailed:
		return "失败"
	case StatusTimeout:
		return "超时"
	case StatusKilled:
		return "已停止"
	case StatusInterrupted:
		return "中断"
	}
	return s
}

// JobLabel 在 statusLabel 之外区分出正在续跑的任务。
func JobLabel(j *Job) string {
	if j.Status == StatusRunning && j.ResumeOf != "" {
		return "续跑中"
	}
	if j.Status == StatusRunning && j.Takeover {
		return "接手中"
	}
	if VerifyFailed(j) {
		return "合并后核验失败"
	}
	if j.Gate != nil {
		switch j.Gate.State {
		case GateQueued:
			return "等过门"
		case GateRunning:
			return "过门中"
		case GateStuck:
			return "门没过"
		case GateAbandoned:
			return "门没过，已放弃"
		}
	}
	return StatusLabel(j.Status)
}

// CommitRe：「合并提交」「提交号」「merge commit」后面紧跟的 7–40 位小写十六进制提交号（中间可以有空白、冒号、反引号、加粗的星号）。
// 只认这个固定写法，正文顺带提到「合并」的段落（如「审查结论是可以合并」）不算。
var CommitRe = regexp.MustCompile("(合并提交|提交号|(?i:merge commit))[\\s:：是为`*]*([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")

// TransientBackoff：第 1、2 次重试前各等多久；用完回到 retry_after。
var TransientBackoff = []time.Duration{15 * time.Minute, time.Hour}

// TransientStreak：这个指纹最近连续几个瞬时错误（撞上限的不算、也不打断）。
func TransientStreak(st *State, project, fp string) int {
	if fp == "" {
		return 0
	}
	var ended []*Job
	for _, j := range st.Jobs {
		if SameItem(j, project, fp) && j.Status != StatusRunning && j.EndedAt != nil && !j.LimitHit {
			ended = append(ended, j)
		}
	}
	sort.SliceStable(ended, func(i, k int) bool { return ended[i].EndedAt.After(*ended[k].EndedAt) })
	n := 0
	for _, j := range ended {
		if !j.Transient {
			break
		}
		n++
	}
	return n
}

// TransientDelay：已经连续失败 n 次（n ≥ 1）后等多久，不比 retry_after 长。
func TransientDelay(n int, retryAfter time.Duration) time.Duration {
	if n > len(TransientBackoff) {
		n = len(TransientBackoff)
	}
	if d := TransientBackoff[n-1]; d < retryAfter {
		return d
	}
	return retryAfter
}

// retryWait：同一条目上次（last）结束后要等多久才能再挑。
func retryWait(st *State, last *Job, retryAfter time.Duration) time.Duration {
	if !last.Transient {
		return retryAfter
	}
	n := TransientStreak(st, last.Project, last.Fingerprint)
	if n < 1 {
		n = 1 // 没有指纹的瞬时错误不进挑活，这里只是兜底
	}
	return TransientDelay(n, retryAfter)
}

// 过门的状态。
const (
	GateQueued    = "queued"    // 排着队
	GateRunning   = "running"   // _gate 进程在跑
	GateMerged    = "merged"    // 进 main 了
	GateReturned  = "returned"  // 退回任务接着改
	GateStuck     = "stuck"     // 判不了或退回用完了，分支留着，等用户定
	GateAbandoned = "abandoned" // 用户放弃，分支留着
)

// GateReady 是任务写的 ready.json：做到分支可合并的交付单。
type GateReady struct {
	Branch     string `json:"branch"`
	Type       string `json:"type"`       // fix、feature、rewrite、docs
	Acceptance string `json:"acceptance"` // 用户怎么验收，写进 BACKLOG「已完成」
	Progress   string `json:"progress"`   // 给 PROGRESS 的一句话
}

// GateRecord 记在任务上，随 meta.json 落盘：状态文件损坏重建后接着走。
type GateRecord struct {
	State    string    `json:"state"`
	Branch   string    `json:"branch"`
	Head     string    `json:"head,omitempty"` // 排队时的分支尖
	Round    int       `json:"round,omitempty"`
	Ready    GateReady `json:"ready"`
	QueuedAt time.Time `json:"queued_at,omitzero"`
	// NotBefore：这之前不重跑（审查撞了用量上限、认证失败）。
	NotBefore time.Time `json:"not_before,omitzero"`
	// Override：用户放行的理由，有它就不跑门，只合并。
	Override string `json:"override,omitempty"`
	// 后台进程：PID 是 _gate 自己（新进程组），Deadline 过了还在就杀。BootTime 不一致时 pid 和组号不可信，不清理进程组。
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Deadline  time.Time `json:"deadline,omitzero"`
	BootTime  int64     `json:"boot_time,omitempty"`
	// Crashes：_gate 没留下结果就没了几次。到上限算判不了，不一直重来。
	Crashes int `json:"crashes,omitempty"`
	// Merge：进 main 的合并提交。Report：最后一次的一句话结论。
	Merge  string `json:"merge,omitempty"`
	Report string `json:"report,omitempty"`
	// WrapErr：已经进 main，BACKLOG 和 PROGRESS 没写上的原因，下次 tick 再写。
	WrapErr string `json:"wrap_err,omitempty"`
	// WaitNoted：主目录挡着合并，已经通知过一次。
	WaitNoted bool `json:"wait_noted,omitempty"`
	// CleanErr：worktree 没清干净的原因，只记一次日志。
	CleanErr string `json:"clean_err,omitempty"`
	// DecideErr：用户的决定还没做成的原因（决定留着，下次 tick 再做），只记一次日志。
	DecideErr string `json:"decide_err,omitempty"`
	// Usage：门调模型花的（审查），已经加进 Job.Usage。
	Usage *backend.JobUsage `json:"usage,omitempty"`
	// Tally：这个任务记录上每道门跑了多少次、什么结论，agentq stats 和早报看。升级前的记录没有。
	Tally map[string]*GateTally `json:"tally,omitempty"`
	// Passes：用户放行了几次。
	Passes int `json:"passes,omitempty"`
	// Upstream：连续几次因为上游 5xx 没判出来、排回去重跑。有了别的结论就清零。
	Upstream int `json:"upstream,omitempty"`
	// Rerun：这次是用户要的再审：不沿用旧结论，没过还是等用户定，不消耗退回的轮数。
	// RerunHead、Reruns：再审过的提交和次数，同一提交有上限。
	Rerun     bool   `json:"rerun,omitempty"`
	RerunHead string `json:"rerun_head,omitempty"`
	Reruns    int    `json:"reruns,omitempty"`
}

// GateTally 是一道门跑的次数。
type GateTally struct {
	Pass    int `json:"pass,omitempty"`
	Fail    int `json:"fail,omitempty"`
	Unknown int `json:"unknown,omitempty"`
	// WouldBlock：warn 档没过的次数：如果是 error 档会挡下。
	WouldBlock int `json:"would_block,omitempty"`
	// Deferred：撞限、认证、上游 5xx，这次不算结论，过一会儿重跑。
	Deferred int `json:"deferred,omitempty"`
	// Usage：这道门调模型花的。
	Usage *backend.JobUsage `json:"usage,omitempty"`
}

// GateActive：过门还没走完，这一条还归门管：不接手、不算孤儿。
func GateActive(j *Job) bool {
	if j.Gate == nil {
		return false
	}
	switch j.Gate.State {
	case GateQueued, GateRunning:
		return true
	case GateMerged:
		return j.Gate.WrapErr != ""
	}
	return false
}

// GateStuckNow：卡在门上等用户定。
func GateStuckNow(j *Job) bool { return j.Gate != nil && j.Gate.State == GateStuck }

// GateUnmerged：任务跑完了，但这次的活没进 main（在排队、在过门、门没过、放弃了）。退回的已经改记中断。
func GateUnmerged(j *Job) bool {
	return j.Gate != nil && j.Gate.State != GateMerged && j.Gate.State != GateReturned
}

// Gating 返回 _gate 进程在跑的任务。
func (st *State) Gating() []*Job {
	var out []*Job
	for _, j := range st.Jobs {
		if j.Gate != nil && j.Gate.State == GateRunning {
			out = append(out, j)
		}
	}
	return out
}

// MergeVerify 是升级前「合并后核验」写在任务上的记录。那条路已经没了，老记录还要读得出、写回去字节不变，到期随任务记录一起清掉。
type MergeVerify struct {
	Commit string `json:"commit"`
	OnMain string `json:"on_main"` // yes、no、unknown
	Review string `json:"review"`  // yes、no、unknown
	Script string `json:"script"`  // none、running、passed、failed、timeout、unknown
	// Failed：已合并（或报告已合并）但核验失败。任务状态不动，显示成「合并后核验失败」。
	Failed bool   `json:"failed,omitempty"`
	Note   string `json:"note,omitempty"`
	// 后台进程：PID 是 _verify 自己（新进程组），Deadline 过了还在就杀。BootTime 不一致时 pid 和组号不可信，不清理进程组。
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Deadline  time.Time `json:"deadline,omitzero"`
	BootTime  int64     `json:"boot_time,omitempty"`
	// Regression：写进「待做」的修回归标题（到上限时是 capped）；RegressionPending：还没写进去的那一行，下次 tick 再写；RegressionErr：上次没写进去的原因，只记一次日志。
	Regression        string `json:"regression,omitempty"`
	RegressionPending string `json:"regression_pending,omitempty"`
	RegressionErr     string `json:"regression_err,omitempty"`
	// CleanErr：worktree 没清干净的原因，只记一次日志（collectVerifies 每次 tick 再清）。
	CleanErr string `json:"clean_err,omitempty"`
}

// VerifyFailed：标了「合并后核验失败」的任务。
func VerifyFailed(j *Job) bool { return j.Verify != nil && j.Verify.Failed }
