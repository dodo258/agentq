// Package backend 是 CLI 后端的接口和注册表，以及各后端共用的类型（用量、额度、工具调用、进展信号）。
// 调度层只看这里的类型，谁家 CLI 的参数和输出格式都留在各自的实现包里。怎么接一个新后端见 docs/backends.md。
package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Backend 是一个 CLI 后端。调度层起任务、读结果、看进展、查登录都只走它。
type Backend interface {
	// Name 是注册名，也是 config 的 backend 键和 Job.Backend 的值。
	Name() string
	// Caps 声明能力。调度层按它降级，缺了硬前提的任务 Build 拒绝。
	Caps() Caps
	// EnvKeep 是这个后端额外放行的环境变量（登录和配置位置）；公共部分在 proc。
	EnvKeep() []string
	// Probe 给安装和 doctor 用：CLI 在哪、什么版本、能不能无头跑、认证状态。能查的都填进 Probe，再返回第一个挡路的错误。
	Probe(ctx context.Context, env ProbeEnv) (Probe, error)
	// Auth 是启动任务前的登录检查。查不了的后端返回 AuthUnknown，调度层照常启动。
	Auth(ctx context.Context) Auth
	// Build 把意图翻译成一次启动。Must 里做不到的返回错误（任务不启动，Runner.Start 也会先查一次），其余做不到的写进 Degradation。
	Build(req Request) (Invocation, []Degradation, error)
	// Parse 读任务目录里保存的输出，归一成 Result。读不出终态时 HaveTerminal 为假，不猜成功。
	Parse(job JobRef, exit Exit) Result
	// LastActivity 是这个任务最后一次有动静的时刻，判断疑似卡死用。ok 为假表示判断不了；时间可以是零值（还没有动静）。
	LastActivity(job JobRef) (time.Time, bool)
	// CanResume：这个会话现在还能接上（有续跑能力、会话记录还在）。
	CanResume(session string) bool
	// RecordedModel 从 Job.Args 认出当时用的模型，统计用；认不出返回空。
	RecordedModel(args []string) string
}

// QuotaReader 是可选能力：不调模型就能读订阅额度。没实现的后端额度当未知，照常跑。
// 读和解析分开：QuotaText 是唯一动 CLI 的一步。
type QuotaReader interface {
	QuotaText(ctx context.Context) (string, error)
	ParseQuota(text string, now time.Time) ([]UsageWindow, error)
}

// IdentityLinker 是可选能力：在别的 HOME 里运行时，指回真实家目录哪些路径就能用上本机已有的登录（相对 HOME）。
// 只放软链接，不复制任何内容，也不放写的时候会被原子替换的文件：替换掉链接，真实家目录那份就旧了。agentq eval 的 --real 用。
type IdentityLinker interface {
	IdentityLinks() []string
}

// ToolLog 是可选能力：能读出任务的工具调用。没实现的后端「看进展」的信号记未知、不报，只留疑似卡死。
// withSubs 为假时不读子代理记录。Name 要是规范名，见 ToolEvent。
type ToolLog interface {
	ToolEvents(job JobRef, withSubs bool) (main []ToolEvent, subs [][]ToolEvent, err error)
}

// Profile 是这次运行能用哪些工具。
type Profile int

const (
	ProfileFull     Profile = iota // 改文件、跑命令、提交，权限看 Request.Permission
	ProfileReview                  // 能跑测试和写文件，工作目录是用完就丢的 worktree（合并前的审查进程）
	ProfileReadOnly                // 只能读文件
	ProfileNoTools                 // 没有工具（输入是外部原文的任务）
)

// DenyPolicy 是一组由 CLI 强制的拒绝规则，按用途命名，具体规则写法在各后端。
type DenyPolicy int

const (
	DenyNone        DenyPolicy = iota
	DenySecrets                // 不读 .env、secrets 和主动感知的原始数据
	DenyStoreSubmit            // 不碰 App Store 提交审核的接口
)

// Must 是必须真生效的要求：做不到 Build 就返回错误，任务不启动。不写进 Must 的要求做不到只产生 Degradation。
type Must uint

const (
	MustProfile       Must = 1 << iota // Profile 由 CLI 强制
	MustDeny                           // Deny 由 CLI 强制
	MustSchema                         // Schema 由 CLI 强制
	MustNoExternalMCP                  // 不接用户配的 MCP
	MustReview                         // 引擎能起独立审查进程（Schema、ReviewProfile）
	MustImages                         // 模型看得到本地图片（界面门）
)

// Request 是一次启动的意图，不是参数。字段留空表示不要求，后端不传对应参数。
type Request struct {
	Prompt string
	// Dir 是工作目录。
	Dir string
	// Label 是进程标记（proc.NameMarker 开头），后端要放进命令行，收尾时靠它确认 pid 没被复用。命令行放不了的声明 Caps.NoArgvLabel，靠 AGENTQ_JOB 认。
	Label string
	// Resume 非空时接回这个会话；Session 是预设的新会话 id（Caps.PresetSession 时才有）。
	Resume, Session string
	// Model、Effort 为空表示跟随 CLI 自己的设置。
	Model, Effort string
	// Permission 是权限模式，空表示用 CLI 默认（只读任务靠默认模式下的读文件边界，不能传）。
	Permission    string
	Profile       Profile
	NoExternalMCP bool
	Deny          DenyPolicy
	// Schema 是结构化输出要满足的 JSON Schema。
	Schema string
	// MaxTurns、BudgetUSD 为 0 表示不限。
	MaxTurns  int
	BudgetUSD float64
	// AddDirs 是工作目录外还要能读写的目录。
	AddDirs []string
	// Images 是要模型看的本地图片（绝对路径），也都在 Dir 或 AddDirs 里。能读文件就看得到的后端不用传，要显式附上的后端在 Build 里附。
	Images []string
	Must   Must
	// WorkDir 是这次运行的落盘目录（放 result.json 那个）。后端要给 CLI 传文件（比如 schema）时放这里。
	WorkDir string
}

// Invocation 是一次启动：跑什么、在哪跑、留什么记录。
type Invocation struct {
	Path string
	Args []string
	// Record 是写进 meta.json 的参数（提示词换成占位，不重复存一大段）。
	Record []string
	Dir    string
	// EnvKeep 是这次传给 CLI 的额外环境变量，等于 Backend.EnvKeep。
	EnvKeep []string
	// Stdin 是喂给 CLI 标准输入的内容，空的就是 /dev/null。提示词走标准输入的后端用它，不进 ps，也不受 argv 长度限制。
	Stdin string
}

// Degradation 是一项要求没真生效：写进日志，doctor 也列出来。
type Degradation struct {
	Cap  string // Caps 里的能力名
	Note string // 给人看的后果
}

// Stop 是 CLI 自己设的上限或认证问题导致的结束。撞用量上限不在这里，见 Result 的 Limit 字段。
type Stop string

const (
	StopNone      Stop = ""
	StopMaxBudget Stop = "max_budget"
	StopMaxTurns  Stop = "max_turns" // 回合和 CLI 自带的其他次数上限
	StopAuth      Stop = "auth"
)

// SelfLimit：撞的是任务自己设的上限。
func (s Stop) SelfLimit() bool { return s == StopMaxBudget || s == StopMaxTurns }

// Result 是归一后的结果。
type Result struct {
	// HaveTerminal：读到了终态。为假时只有 Usage、LimitText、ResetAt 可能有值，调度层只看退出和 BACKLOG，不猜成功。
	HaveTerminal bool
	// Failed：后端说这次是报错结束的。
	Failed    bool
	Stop      Stop
	Text      string
	SessionID string
	// Turns 为 0 表示后端没给。
	Turns int
	// HTTPStatus 是上游错误码，0 表示没有。
	HTTPStatus int
	// Structured 是结构化输出。
	Structured json.RawMessage
	// Background：最后一个回合是后台通知触发的，不是任务自己走完的。
	Background bool
	// Usage 为 nil 表示后端没给，不是 0。UsageCumulative：续跑时是整个会话的累计，要减上一次。
	Usage           *JobUsage
	UsageCumulative bool
	// LimitStructured 是结构化字段说撞了用量上限；LimitText 是错误通道的文字里有撞限字样。
	// 结果可能带外部内容的任务只认前者，防别人写一句话停掉全部任务。
	LimitStructured bool
	LimitText       bool
	// ResetAt 是文字里读出的重置时刻，零值表示读不出（调用方按 limit_cooldown 冷却）。
	ResetAt time.Time
}

// JobRef 指向一个任务的落盘材料。
type JobRef struct {
	ID string
	// Dir 是放 result.json、stderr.log 的目录。
	Dir       string
	SessionID string
	StartedAt time.Time
}

// Exit 是进程结束的情况。At 是退出时刻（解析相对时间的重置提示用），零值表示调用方不关心。
type Exit struct {
	At time.Time
}

// AuthState 是登录检查的结论。
type AuthState int

const (
	AuthUnknown AuthState = iota // 这个后端查不了：照常启动，第一次因认证失败再停
	AuthIn
	AuthOut
	AuthError // 查了但没得出结论：不启动
)

// Auth 是登录检查的结果。Detail 是不启动时给用户看的原因。
type Auth struct {
	State  AuthState
	Detail string
	// Unverified：AuthIn 只代表本地有凭据，不代表凭据有效。这样的「已登录」不解除认证失败的暂停，到时间或用户删掉暂停记录才恢复。
	Unverified bool
}

// Blocked：这个结论挡启动。
func (a Auth) Blocked() bool { return a.State == AuthOut || a.State == AuthError }

// Probe 找不到 CLI、CLI 没登录时用 %w 包这两个。安装和启动靠它们决定是带用户去装、去登录，还是直接报错。
var (
	ErrNoCLI       = errors.New("CLI not found")
	ErrNotLoggedIn = errors.New("CLI not logged in")
)

// CLISetup 是可选能力：这个后端的官方安装和登录。没有这个能力的后端，缺 CLI 时只报错。
type CLISetup interface {
	CLIHelp() CLIHelp
	// DesktopInstalled：有没有同一家的桌面端。只用来把话说清楚，从不调用它。
	DesktopInstalled(home string) bool
	InstallCLI(ctx context.Context, stdout, stderr io.Writer) error
	LoginCLI(ctx context.Context, path string, in io.Reader, stdout, stderr io.Writer) error
}

// GlobalRules 是可选能力：这个后端有用户级规则目录，前台会话和后台任务都读得到。没有这个能力的后端，环境层不装。
type GlobalRules interface {
	// RulesPaths 返回规则目录，和用户自己的全局说明文件（有内容就算用户已有规则）。
	RulesPaths(home string) (dir string, instructions []string)
}

// RulesBlock 是可选能力：这个后端没有规则目录，只有一个用户级说明文件。环境层在里面放一段带标记的块，别的字节不动。
type RulesBlock interface {
	// RulesFile 是放块的文件；shadow 是存在就让这个文件失效的文件（读不到块，环境层要说明）。
	RulesFile(home string) (file, shadow string)
}

// Permissions 是可选能力：这个后端的 permission_mode 有哪些值。不声明的后端不查写法。
type Permissions interface {
	// PermissionModes 是合法的值。
	PermissionModes() []string
	// FullPermission 是无人值守任务要用的值：能改文件、跑命令、提交。
	FullPermission() string
	// PermissionNote 是 agentq start 问同意时多说的话：代码发给谁、放开了什么。
	PermissionNote() string
}

// SkillHome 是可选能力：这个后端从哪发现用户级技能。安装引擎把技能投影到这里，提示词里的路径也换成它。
type SkillHome interface {
	SkillsDir(home string) string
}

// ResumeHint 是可选能力：用户想自己接着看这个会话时运行的命令，agentq logs 里说。没有就只写会话 id。
type ResumeHint interface {
	ResumeCommand(session string) string
}

// LightModeler 是可选能力：这个后端里适合做低风险活的模型。model_policy=auto 又没配 low_risk_model 时才用；没有这个能力就不降级。
type LightModeler interface {
	LightModel() string
}

// CLIHelp 是给用户看的名字和官方命令。
type CLIHelp struct {
	Name, Command, Desktop, Install, Login string
	Alternatives                           []string
}

// ProbeEnv 是探测用的环境：Env 是完整的环境变量（安装时要按目标环境查，不用当前 shell），Explicit 是配置里指定的路径。
type ProbeEnv struct {
	Home     string
	Explicit string
	Env      []string
}

// Probe 是探测结果，JSON 字段是 agentq setup、doctor 输出的一部分。
type Probe struct {
	Path          string `json:"cli_path"`
	Version       string `json:"cli_version"`
	Headless      bool   `json:"headless"`
	Authenticated bool   `json:"authenticated"`
	AuthMethod    string `json:"auth_method"`
}

// Options 是建后端实例要的东西。实例每次用时现建（配置运行中会重载），不要在里面存状态。
type Options struct {
	Home string
	// Path 是配置里写的 CLI 路径，已展开 ~。
	Path string
}
