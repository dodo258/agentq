// Package config 定义 config.json 的全部配置项，以及夜班、复盘的定时表。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/fsx"
	"agentq/internal/textfmt"
)

// Duration 在 config.json 里写成 "4h"、"30m" 这样的字符串。
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(textfmt.FormatDuration(d.Duration))
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("时长要写成字符串，如 \"4h\"：%w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Config struct {
	LaunchdLabel   string   `json:"launchd_label"`
	ProjectsRoot   string   `json:"projects_root"`
	ClaudePath     string   `json:"claude_path"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	MaxParallel    int      `json:"max_parallel"`
	PerProject     int      `json:"per_project"`
	Timeout        Duration `json:"timeout"`
	RetryAfter     Duration `json:"retry_after"`
	LimitCooldown  Duration `json:"limit_cooldown"`
	Paused         bool     `json:"paused"`
	// TickAfterJob：任务结束时马上 tick 一次，让排队的待做接上；关掉就只靠 launchd 每 5 分钟的定时 tick。
	TickAfterJob bool `json:"tick_after_job"`
	// 连环失败熔断：连续 BreakerStreak 个任务都在 BreakerQuick 内结束且没做完，就暂停启动 BreakerCooldown；
	// 熔断过后第一个任务又秒退，时长翻倍，最多 BreakerMax。
	BreakerStreak   int      `json:"breaker_streak"`
	BreakerQuick    Duration `json:"breaker_quick"`
	BreakerCooldown Duration `json:"breaker_cooldown"`
	BreakerMax      Duration `json:"breaker_max"`
	// StallAfter：运行中的任务会话记录这么久没增长，就在 status 和当天汇总里标「疑似卡死」（只提醒，不杀）。
	StallAfter Duration `json:"stall_after"`
	// 夜班、复盘：launchd 每天 01:00、06:30 叫醒 agentq night/review，开关为假时只记一行日志不执行。
	// 夜班一跑就是几个小时，所以超时单独配。
	NightEnabled  bool     `json:"night_enabled"`
	ReviewEnabled bool     `json:"review_enabled"`
	NightTimeout  Duration `json:"night_timeout"`
	ReviewTimeout Duration `json:"review_timeout"`
	// 巡逻：tick 每小时纯脚本检查一圈（见 internal/patrol/patrol.go）；PatrolAnalyze 打开时，有新异常就启动一次 claude -p 分析原因（每天最多 3 次）。
	PatrolEnabled bool `json:"patrol_enabled"`
	PatrolAnalyze bool `json:"patrol_analyze"`
	// 推送：要用户知道的事推到 Telegram（没配就弹本机通知），tick 顺带收用户发来的 Telegram 消息（见 internal/notify/notify.go）。
	NotifyEnabled bool `json:"notify_enabled"`
	// Router：新消息由只读的 Router 处理，动作由 agentq 校验后执行（见 internal/chat/router.go）。关着时对话流程和原来完全一样。
	RouterEnabled bool   `json:"router_enabled"`
	RouterModel   string `json:"router_model"`
	// 按活分配模型和强度（docs/reference/config.md「按活分配模型」）：配了专用模型的那类调用用它，没配照 model、effort。
	// 默认按官方定位：路由用 haiku，商量大方向、复盘、巡逻分析、答问用 sonnet；都写别名，新模型出来自动跟上。
	DiscussModel  string `json:"discuss_model"`
	ReviewModel   string `json:"review_model"`
	ReviewEffort  string `json:"review_effort"`
	PatrolModel   string `json:"patrol_model"`
	FollowupModel string `json:"followup_model"`
	// HeavyEffort：重写、重构、迁移、审计这类长任务的强度（模型照 model）。空就照 effort。
	HeavyEffort string `json:"heavy_effort"`
	// 预算兜底（美元，--max-budget-usd，按单次运行算）：只防失控，额度按正常工作量留足，撞到后保留成果（见 internal/runner/budget.go）。0 表示不传这个参数。
	NightBudgetUSD  float64 `json:"night_budget_usd"`
	ReviewBudgetUSD float64 `json:"review_budget_usd"`
	PatrolBudgetUSD float64 `json:"patrol_budget_usd"`
	// AutopushLog：外部自动推送脚本的日志路径（可写 ~），巡逻读它报推送失败。默认空，不查。
	AutopushLog  string  `json:"autopush_log,omitempty"`
	JobBudgetUSD float64 `json:"job_budget_usd"`
	// LightReviewModel：轻量流程（改动小、局部、好撤回，见 internal/gate/light.go）时审查和界面审查用的模型，强度照旧 medium。空就照 model。
	// 写别名（sonnet），不写带版本号的模型 ID：新模型出来别名自动跟上。审查不用 haiku 这一档。
	LightReviewModel string `json:"light_review_model"`
	// 后台给你留额度：计入的会话窗口（5 小时）、周窗口用到这个百分比，tick 就不领新活（正在跑的不动），等那个窗口重置。
	// 会话默认 70，留 30% 给你自己用；周默认 90。agentq run 不受它管，到 98% 才拒绝。
	UsageStopSessionPct float64 `json:"usage_stop_session_pct"`
	UsageStopWeekPct    float64 `json:"usage_stop_week_pct"`
	// ProgressAutostop：看进展的「子代理失控」信号命中时自动停掉任务（见 internal/runner/budget.go）；关掉时和其他信号一样只报不停。
	ProgressAutostop bool `json:"progress_autostop"`
	// LowRiskModel、LowRiskEffort：非空时，条目标题带「（低风险）」标记的待做任务用这个模型和强度（见 runner.TaskModel）；空的时候照 model、effort。
	LowRiskModel  string `json:"low_risk_model"`
	LowRiskEffort string `json:"low_risk_effort"`
	// ModelPolicy：fixed（默认，不写也是）照 model 和 low_risk_model；auto 按任务类型选，见 runner.TaskModel。降级省额度却可能做坏，所以默认不开。
	ModelPolicy string `json:"model_policy,omitempty"`
	// 闲时找活（suggest_enabled，默认关）：所有项目没有待做、额度宽裕时，每天最多一次起只读的 claude -p 找值得做的活（见 internal/suggest/suggest.go）。
	// 关着时不起任何进程，行为和原来完全一样。
	SuggestEnabled   bool    `json:"suggest_enabled"`
	SuggestModel     string  `json:"suggest_model"`
	SuggestBudgetUSD float64 `json:"suggest_budget_usd"`
	// 主动感知（sense_enabled，默认关）：定点复盘前读 App Store 评价、TestFlight 反馈和 GitHub issue，有新内容才起没有工具的 claude -p 判断（见 internal/sense/sense.go）。
	SenseEnabled   bool    `json:"sense_enabled"`
	SenseModel     string  `json:"sense_model"`
	SenseBudgetUSD float64 `json:"sense_budget_usd"`
	// 发版助手（release_assist_enabled，默认关）：条件都满足时起任务准备提交审核，用户回复「发」才提交（见 internal/release/release.go）。
	ReleaseAssistEnabled bool    `json:"release_assist_enabled"`
	ReleaseBudgetUSD     float64 `json:"release_budget_usd"`
	// 自我进化（见 internal/evolve/signals.go）：evolve_signals_enabled 时 tick 每小时从门结果、任务记录、巡逻里收信号，只读本机文件，不调模型；
	// learned_constraints_enabled 时确认过的门拦截记成项目约束，接在新任务合同末尾。self_project 是 agentq 源码所在的项目名，
	// 配了就把 agentq 自身反复出现的问题写成那个项目「## 建议」里的提案，没配只写 ~/.agentq/evolve/proposals.md。
	EvolveSignalsEnabled      bool   `json:"evolve_signals_enabled"`
	LearnedConstraintsEnabled bool   `json:"learned_constraints_enabled"`
	SelfProject               string `json:"self_project,omitempty"`
	// VerifyAfterMerge 是老配置键，合并后核验已经并进质量门（见 internal/gate），它不再起作用，留着是为了已有配置读写字节不变。
	// VerifyTimeout 是门里每个脚本、每次调模型的超时，超时算判不了。
	VerifyAfterMerge bool     `json:"verify_after_merge"`
	VerifyTimeout    Duration `json:"verify_timeout"`
	// ExcludeProjects：projects_root 下这些项目 tick、闲时找活、主动感知都不碰（可能是公开仓库或不想交出去的）；agentq run 明确指定时仍可跑。
	ExcludeProjects []string `json:"exclude_projects,omitempty"`
	// ExtraProjects：projects_root 外的项目，绝对路径。agentq add 在外面的仓库里运行时自动登记。项目名还是目录名，exclude_projects 优先。
	ExtraProjects []string `json:"extra_projects,omitempty"`
	// RetentionDays：任务记录这些运行数据留多少天，超过的 tick 每天清一次（见 internal/sched/retention.go）。0 表示不删。
	RetentionDays int `json:"retention_days"`
	// 大方向商量（discuss_enabled，默认开）：标了（大方向）的条目和 agentq new 开工前问一次，等 discuss_wait 没回就按默认（见 internal/discuss）。
	DiscussEnabled bool     `json:"discuss_enabled"`
	DiscussWait    Duration `json:"discuss_wait"`
	// Backend：用哪个 CLI 后端，不写就是 claude，已有配置不用动。写了不认识的名字不回退：任务不启动，status 和 doctor 说明。
	Backend string `json:"backend,omitempty"`
	// ProjectBackends：按项目换后端。放用户配置，不放项目仓库：项目文件可能来自别人，不能让它决定代码发给哪家。
	ProjectBackends map[string]string `json:"project_backends,omitempty"`
	// Backends：每个后端自己的路径、模型、强度、权限。顶层的 model、effort、permission_mode 这些是默认后端的，别的后端只看这里，见 ForBackend。
	Backends map[string]BackendConfig `json:"backends,omitempty"`
}

// BackendConfig 是 backends 下一个后端的配置。空的项跟随 CLI 自己的设置，CLI 路径空的时候按后端名在 PATH 里找。
type BackendConfig struct {
	Path       string `json:"path,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
}

// model_policy 的取值。
const (
	ModelPolicyFixed = "fixed"
	ModelPolicyAuto  = "auto"
)

// ModelLabel 是给人看的模型名：空的就是跟随 CLI。
func ModelLabel(model string) string {
	if model == "" {
		return "跟随 CLI"
	}
	return model
}

// ProjectBackend 是这个项目用的后端名，空表示默认后端。
func (c Config) ProjectBackend(project string) string {
	if b := c.ProjectBackends[project]; b != "" {
		return b
	}
	return c.Backend
}

// ExtraProjectDir 是 extra_projects 里这个名字的目录，没有就是空。同名的第一个算数（add 不会登记出同名的，手写的才可能）。
func (c Config) ExtraProjectDir(name string) string {
	for _, dir := range c.ExtraProjects {
		if filepath.Base(dir) == name {
			return dir
		}
	}
	return ""
}

// BackendPath 是这个后端的 CLI 路径：backends.<名字>.path，Claude 还有老键 claude_path，其余按名字在 PATH 里找。
func (c Config) BackendPath(name string) string {
	if name == "" {
		name = backend.Default
	}
	if p := c.Backends[name].Path; p != "" {
		return p
	}
	if name == backend.Default {
		return c.ClaudePath
	}
	return name
}

// SetBackendPath 记下这个后端的 CLI 路径。Claude 写老键 claude_path，已有配置的样子不变。
func (c *Config) SetBackendPath(name, path string) {
	if name == "" || name == backend.Default {
		c.ClaudePath = path
		return
	}
	if c.Backends == nil {
		c.Backends = map[string]BackendConfig{}
	}
	s := c.Backends[name]
	s.Path = path
	c.Backends[name] = s
}

// BackendsInUse 是配置用到的后端：默认的和 project_backends 里的，去重排好序。
func (c Config) BackendsInUse() []string {
	def := c.Backend
	if def == "" {
		def = backend.Default
	}
	names := []string{def}
	for _, b := range c.ProjectBackends {
		if !slices.Contains(names, b) {
			names = append(names, b)
		}
	}
	slices.Sort(names)
	return names
}

// PermissionProblem：这个后端要权限配置却没有，任务改不了文件，起了也是白花额度，说明要写什么。没问题返回空。后端名空表示 Claude（和 Job.Backend 一样）。
// 默认后端是 Claude 时不算：老配置不写就是完全权限，default 这个值一直是只提示照跑，老用户行为不变。后端不声明权限值的查不了，也不算。
func (c Config) PermissionProblem(name string) string {
	if name == "" {
		name = backend.Default
	}
	def := c.Backend
	if def == "" {
		def = backend.Default
	}
	if name == backend.Default && def == backend.Default {
		return ""
	}
	p, ok := backend.Get(name, backend.Options{}).(backend.Permissions)
	if !ok {
		return ""
	}
	if mode := c.ForBackend(name).PermissionMode; mode != "" && mode != "default" {
		return ""
	}
	how := "确认过风险后运行 agentq start（会先问你），或在 config.json 写 permission_mode=" + p.FullPermission()
	if name != def {
		how = "确认过风险后在 config.json 写 backends." + name + ".permission=" + p.FullPermission()
	}
	return "后端 " + name + " 的权限没放开，任务改不了文件，不起任务：" + how
}

// PermissionProblems 是配置用到的后端里权限没配的，给 status 和 doctor。
func (c Config) PermissionProblems() []string {
	var out []string
	for _, name := range c.BackendsInUse() {
		if why := c.PermissionProblem(name); why != "" {
			out = append(out, why)
		}
	}
	return out
}

// claudeDefaultKeys 是默认值只对 Claude 有意义的键，前三个是 model、effort、permission_mode（ClearClaudeDefaults 靠这个顺序）。默认后端不是 Claude、配置又没写它们时要留空，不能把 opus 传给别家。
var claudeDefaultKeys = []string{"model", "effort", "permission_mode", "router_model", "suggest_model", "sense_model", "light_review_model",
	"discuss_model", "review_model", "review_effort", "patrol_model", "followup_model", "heavy_effort", "low_risk_model", "low_risk_effort"}

// clearRoleModels 清掉按活分配的模型和强度（照 model、effort）：新配置跟随 CLI，别家后端不用 Claude 的别名。
func (c *Config) clearRoleModels() {
	c.DiscussModel, c.ReviewModel, c.ReviewEffort, c.PatrolModel, c.FollowupModel = "", "", "", "", ""
	c.HeavyEffort, c.LowRiskEffort = "", ""
}

// Role：配了专用模型就用它和给的强度，没配照 model、effort。
func (c Config) Role(model, effort string) (string, string) {
	if model == "" {
		return c.Model, c.Effort
	}
	return model, effort
}

// pinnedModelRe：带版本号的 Claude 模型 ID（claude-opus-5-5、claude-sonnet-4-5-20250929）。写了它就不会随新模型更新。
var pinnedModelRe = regexp.MustCompile(`(?i)^claude-[a-z]+-\d`)

// PinnedModels：配置里钉死版本号的模型键，doctor 提醒改成别名。
func (c Config) PinnedModels() []string {
	var out []string
	for _, kv := range []struct{ key, val string }{{"model", c.Model}, {"router_model", c.RouterModel}, {"suggest_model", c.SuggestModel}, {"sense_model", c.SenseModel},
		{"light_review_model", c.LightReviewModel}, {"low_risk_model", c.LowRiskModel}, {"discuss_model", c.DiscussModel}, {"review_model", c.ReviewModel},
		{"patrol_model", c.PatrolModel}, {"followup_model", c.FollowupModel}} {
		if pinnedModelRe.MatchString(kv.val) {
			out = append(out, kv.key+"="+kv.val)
		}
	}
	return out
}

// ForBackend 返回这个后端看到的配置：模型、强度、权限只传给配了它们的后端，别家的默认不串。
// 顶层的这些键是默认后端的（后端名空表示 Claude，老任务记录就是这样），backends.<名字> 里写了的优先。
// UsagePolicy：判断额度用的模型和后台上限。
func (c Config) UsagePolicy() backend.UsagePolicy {
	return backend.UsagePolicy{Model: c.Model, Session: c.UsageStopSessionPct, Week: c.UsageStopWeekPct}
}

func (c Config) ForBackend(name string) Config {
	if name == "" {
		name = backend.Default
	}
	def := c.Backend
	if def == "" {
		def = backend.Default
	}
	out := c
	if name != def {
		out.Model, out.Effort, out.PermissionMode, out.LowRiskModel = "", "", "", ""
		out.RouterModel, out.SuggestModel, out.SenseModel, out.LightReviewModel = "", "", "", ""
		out.clearRoleModels()
	}
	s := c.Backends[name]
	if s.Model != "" {
		out.Model = s.Model
	}
	if s.Effort != "" {
		out.Effort = s.Effort
	}
	if s.Permission != "" {
		out.PermissionMode = s.Permission
	}
	return out
}

func Default() Config {
	return Config{
		LaunchdLabel:   LaunchdLabel,
		ProjectsRoot:   "~/projects",
		ClaudePath:     "claude",
		Model:          "opus",
		Effort:         "high",
		PermissionMode: "bypassPermissions",
		MaxParallel:    2,
		PerProject:     2,
		Timeout:        Duration{4 * time.Hour},
		RetryAfter:     Duration{12 * time.Hour},
		LimitCooldown:  Duration{time.Hour},
		TickAfterJob:   true,

		BreakerStreak:   3,
		BreakerQuick:    Duration{3 * time.Minute},
		BreakerCooldown: Duration{30 * time.Minute},
		BreakerMax:      Duration{4 * time.Hour},
		StallAfter:      Duration{45 * time.Minute},

		NightTimeout:  Duration{6 * time.Hour},
		ReviewTimeout: Duration{4 * time.Hour},

		PatrolEnabled: true,
		PatrolAnalyze: true,

		NotifyEnabled: true,

		RouterModel: "haiku",

		DiscussModel:  "sonnet",
		ReviewModel:   "sonnet",
		ReviewEffort:  "high",
		PatrolModel:   "sonnet",
		FollowupModel: "sonnet",
		HeavyEffort:   "xhigh",
		LowRiskModel:  "sonnet",
		LowRiskEffort: "medium",

		NightBudgetUSD:  10,
		ReviewBudgetUSD: 15,
		PatrolBudgetUSD: 2,
		JobBudgetUSD:    150,

		LightReviewModel:    "sonnet",
		UsageStopSessionPct: 70,
		UsageStopWeekPct:    90,

		ProgressAutostop: true,
		SuggestModel:     "sonnet",
		SuggestBudgetUSD: 1,

		SenseModel:     "sonnet",
		SenseBudgetUSD: 1,

		ReleaseBudgetUSD: 30,

		EvolveSignalsEnabled:      true,
		LearnedConstraintsEnabled: true,

		VerifyAfterMerge: true,
		VerifyTimeout:    Duration{30 * time.Minute},

		RetentionDays: 90,

		DiscussEnabled: true,
		DiscussWait:    Duration{2 * time.Hour},
	}
}

// Fresh 是新建配置时要显式写下的默认：暂停，后台开关全关，权限 default，模型和强度留空（跟随用户在 CLI 里选的，后端不传参数）。安装器和「配置文件不存在时跑普通命令」共用这一份。
// 已有配置缺的键不走这里：老配置靠 defaultConfig 的语义，不能悄悄变。
func (c Config) Fresh() Config {
	c.Model, c.Effort = "", ""
	c.RouterModel, c.SuggestModel, c.SenseModel, c.LightReviewModel = "", "", "", ""
	c.LowRiskModel = ""
	c.clearRoleModels()
	c.Paused = true
	c.NightEnabled = false
	c.ReviewEnabled = false
	c.PatrolEnabled = false
	c.PatrolAnalyze = false
	c.NotifyEnabled = false
	c.PermissionMode = "default"
	return c
}

func (c Config) Validate() error {
	var bad []string
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+$`).MatchString(c.LaunchdLabel) {
		bad = append(bad, "launchd_label 要是点分隔的字母、数字、下划线或连字符")
	}
	if c.ProjectsRoot == "" {
		bad = append(bad, "projects_root 为空")
	}
	if c.ClaudePath == "" {
		bad = append(bad, "claude_path 为空")
	}
	if bad1 := permissionProblem(c.Backend, c.PermissionMode); bad1 != "" {
		bad = append(bad, "permission_mode "+bad1)
	}
	if c.MaxParallel < 1 {
		bad = append(bad, "max_parallel 至少为 1")
	}
	if c.PerProject < 1 {
		bad = append(bad, "per_project 至少为 1")
	}
	if c.Timeout.Duration <= 0 || c.RetryAfter.Duration <= 0 || c.LimitCooldown.Duration <= 0 || c.StallAfter.Duration <= 0 ||
		c.NightTimeout.Duration <= 0 || c.ReviewTimeout.Duration <= 0 {
		bad = append(bad, "timeout、retry_after、limit_cooldown、stall_after、night_timeout、review_timeout 都要大于 0")
	}
	if c.BreakerStreak < 1 {
		bad = append(bad, "breaker_streak 至少为 1")
	}
	if c.BreakerQuick.Duration <= 0 || c.BreakerCooldown.Duration <= 0 || c.BreakerMax.Duration < c.BreakerCooldown.Duration {
		bad = append(bad, "breaker_quick、breaker_cooldown 要大于 0，breaker_max 不能小于 breaker_cooldown")
	}
	if c.NightBudgetUSD < 0 || c.ReviewBudgetUSD < 0 || c.PatrolBudgetUSD < 0 || c.JobBudgetUSD < 0 {
		bad = append(bad, "night_budget_usd、review_budget_usd、patrol_budget_usd、job_budget_usd 不能是负数（0 表示不限）")
	}
	if !(c.UsageStopSessionPct >= 1 && c.UsageStopSessionPct <= 97) || !(c.UsageStopWeekPct >= 1 && c.UsageStopWeekPct <= 97) {
		bad = append(bad, "usage_stop_session_pct、usage_stop_week_pct 要在 1 到 97 之间（到 98% 连 agentq run 都拒绝）")
	}
	if c.ModelPolicy != "" && c.ModelPolicy != ModelPolicyFixed && c.ModelPolicy != ModelPolicyAuto {
		bad = append(bad, "model_policy 只能是 fixed 或 auto")
	}
	if c.RouterEnabled && strings.HasPrefix(c.RouterModel, "-") {
		bad = append(bad, "router_model 不能以 - 开头")
	}
	for _, kv := range [][2]string{{"discuss_model", c.DiscussModel}, {"review_model", c.ReviewModel}, {"review_effort", c.ReviewEffort}, {"patrol_model", c.PatrolModel},
		{"followup_model", c.FollowupModel}, {"heavy_effort", c.HeavyEffort}, {"low_risk_model", c.LowRiskModel}, {"low_risk_effort", c.LowRiskEffort}} {
		if strings.HasPrefix(kv[1], "-") {
			bad = append(bad, kv[0]+" 不能以 - 开头")
		}
	}
	if strings.HasPrefix(c.LightReviewModel, "-") {
		bad = append(bad, "light_review_model 不能以 - 开头")
	}
	if strings.Contains(strings.ToLower(c.LightReviewModel), "haiku") {
		bad = append(bad, "light_review_model 不能用 haiku：审查是合并前最后一道把关，最低用 sonnet 这一档")
	}
	if c.SuggestEnabled && strings.HasPrefix(c.SuggestModel, "-") {
		bad = append(bad, "suggest_model 不能以 - 开头")
	}
	if !(c.SuggestBudgetUSD > 0 && c.SuggestBudgetUSD <= 1000) {
		bad = append(bad, "suggest_budget_usd 要在 0 到 1000 之间（美元，不含 0）")
	}
	if c.SenseEnabled && strings.HasPrefix(c.SenseModel, "-") {
		bad = append(bad, "sense_model 不能以 - 开头")
	}
	if !(c.SenseBudgetUSD > 0 && c.SenseBudgetUSD <= 1000) {
		bad = append(bad, "sense_budget_usd 要在 0 到 1000 之间（美元，不含 0）")
	}
	if !(c.ReleaseBudgetUSD > 0 && c.ReleaseBudgetUSD <= 1000) {
		bad = append(bad, "release_budget_usd 要在 0 到 1000 之间（美元，不含 0）")
	}
	if c.VerifyTimeout.Duration <= 0 {
		bad = append(bad, "verify_timeout 要大于 0")
	}
	for _, name := range c.ExcludeProjects {
		if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			bad = append(bad, "exclude_projects 里要写项目目录名，不能是空的，也不能带路径")
			break
		}
	}
	if c.SelfProject != "" && (strings.TrimSpace(c.SelfProject) == "" || c.SelfProject == "." || c.SelfProject == ".." || strings.ContainsAny(c.SelfProject, `/\`)) {
		bad = append(bad, "self_project 要写项目目录名，不能带路径")
	}
	for _, dir := range c.ExtraProjects {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == string(filepath.Separator) {
			bad = append(bad, "extra_projects 里要写项目目录的绝对路径")
			break
		}
	}
	backendName := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	if c.Backend != "" && !backendName.MatchString(c.Backend) {
		bad = append(bad, "backend 只能是字母、数字、下划线或连字符")
	}
	for name, b := range c.ProjectBackends {
		if !backendName.MatchString(b) || strings.ContainsAny(name, `/\`) {
			bad = append(bad, "project_backends 要写 项目目录名 到 后端名")
			break
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Backends)) {
		s := c.Backends[name]
		if !backendName.MatchString(name) {
			bad = append(bad, "backends 的键只能是字母、数字、下划线或连字符")
		}
		if strings.HasPrefix(s.Model, "-") || strings.HasPrefix(s.Effort, "-") || strings.HasPrefix(s.Path, "-") {
			bad = append(bad, "backends."+name+" 的 path、model、effort 不能以 - 开头")
		}
		if why := permissionProblem(name, s.Permission); why != "" {
			bad = append(bad, "backends."+name+".permission "+why)
		}
	}
	if c.DiscussWait.Duration <= 0 {
		bad = append(bad, "discuss_wait 要大于 0")
	}
	if c.RetentionDays < 0 {
		bad = append(bad, "retention_days 不能是负数（0 表示不删）")
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "；"))
	}
	return nil
}

// permissionProblem：权限值由后端自己声明。没注册的后端（写错了，status 和 doctor 会说）和不声明的后端查不了，放过。
func permissionProblem(name, mode string) string {
	if mode == "" || !backend.Known(name) {
		return ""
	}
	p, ok := backend.Get(name, backend.Options{}).(backend.Permissions)
	if !ok || slices.Contains(p.PermissionModes(), mode) {
		return ""
	}
	return fmt.Sprintf("不受支持：后端 %s 只认 %s", backend.Get(name, backend.Options{}).Name(), strings.Join(p.PermissionModes(), "、"))
}

// Load 读配置；文件不存在就写出一份和安装器一致的（见 fresh）。已有配置缺的键沿用默认值。
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg = cfg.Fresh()
		return cfg, Save(path, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("配置文件 %s 格式不对：%w", path, err)
	}
	if err := cfg.ClearClaudeDefaults(b); err != nil {
		return cfg, fmt.Errorf("配置文件 %s 格式不对：%w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("配置文件 %s：%w", path, err)
	}
	return cfg, nil
}

// ClearClaudeDefaults：文件里没写的模型类键不沿用 Claude 的默认值，两种情况：
// 默认后端不是 Claude（不能把 opus、sonnet 传给别家）；文件明写了 "model": ""（跟随 CLI，安装器新建的就是这样：按活分配的别名也不传，跟着 CLI 走）。
// 写了的是用户自己配的，留着。
func (c *Config) ClearClaudeDefaults(raw []byte) error {
	var written map[string]json.RawMessage
	if err := json.Unmarshal(raw, &written); err != nil {
		return err
	}
	fields := map[string]*string{"model": &c.Model, "effort": &c.Effort, "permission_mode": &c.PermissionMode,
		"router_model": &c.RouterModel, "suggest_model": &c.SuggestModel, "sense_model": &c.SenseModel, "light_review_model": &c.LightReviewModel,
		"discuss_model": &c.DiscussModel, "review_model": &c.ReviewModel, "review_effort": &c.ReviewEffort, "patrol_model": &c.PatrolModel,
		"followup_model": &c.FollowupModel, "heavy_effort": &c.HeavyEffort, "low_risk_model": &c.LowRiskModel, "low_risk_effort": &c.LowRiskEffort}
	keys := claudeDefaultKeys
	if c.Backend == "" || c.Backend == backend.Default {
		if m, ok := written["model"]; !ok || string(m) != `""` {
			return nil
		}
		keys = claudeDefaultKeys[3:] // model、effort、permission_mode 文件里写了什么就是什么
	}
	for _, key := range keys {
		if _, ok := written[key]; !ok {
			*fields[key] = ""
		}
	}
	return nil
}

func Save(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'))
}

// SetPaused 只改 config.json 里 paused 这一个键（见 setConfigKey）。文件不存在时按默认值写一份。
func SetPaused(path string, paused bool) error {
	return SetKey(path, "paused", strconv.FormatBool(paused), func() error {
		cfg := Default().Fresh()
		cfg.Paused = paused
		return Save(path, cfg)
	})
}

// AddExtraProject 把目录加进 extra_projects，已有就不动。基于文件当前的内容算新值，两个 add 同时登记不会互相盖掉。
func AddExtraProject(path, dir string) error {
	return updateKey(path, "extra_projects", func(raw []byte) (string, error) {
		var cur struct {
			Extra []string `json:"extra_projects"`
		}
		if err := json.Unmarshal(raw, &cur); err != nil {
			return "", err
		}
		if !slices.Contains(cur.Extra, dir) {
			cur.Extra = append(cur.Extra, dir)
		}
		b, err := json.Marshal(cur.Extra)
		return string(b), err
	}, func() error { return fmt.Errorf("%s 不见了", path) })
}

// SetKey 只改 config.json 里 key 这一个键（value 已经是 JSON）：读当前文件的原始内容，改完原子写回，别的键（包括这个版本不认识的）、顺序、排版都不动。
// 不用 saveConfig：它存的是进程启动时解析的结果，会盖掉这期间手改的内容、丢掉不认识的键。
// 写之前再读一次，文件变了就重来（手改不拿锁，只能这样把窗口压到最小）。文件不是合法的 JSON 对象时不写，报错。文件不存在时交给 missing。
func SetKey(path, key, value string, missing func() error) error {
	return updateKey(path, key, func([]byte) (string, error) { return value, nil }, missing)
}

// updateKey 是 SetKey 的本体：新值由 compute 按文件当前的原始内容算。
func updateKey(path, key string, compute func(raw []byte) (string, error), missing func() error) error {
	lock, err := fsx.AcquireLock(path+".lock", 10*time.Second)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("等了 10 秒还拿不到 %s.lock", path)
	}
	defer lock.Close()
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		before, statErr := os.Stat(path)
		old, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return missing()
		}
		if err != nil {
			return err
		}
		value, err := compute(old)
		if err != nil {
			return fmt.Errorf("配置文件 %s：%w", path, err)
		}
		edited, err := SetJSONKey(old, key, value)
		if err != nil {
			return fmt.Errorf("配置文件 %s：%w", path, err)
		}
		if bytes.Equal(edited, old) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		same, err := replaceIfUnchanged(path, before, edited)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("配置文件 %s 一直在变，没改", path)
}

// replaceIfUnchanged 把 data 写进同目录临时文件，确认 path 还是读它之前那个文件（before）才换上去。权限沿用原文件。
// 比较放在写完、落盘之后，紧贴着改名：fsync 很慢，放前面窗口就大了。
func replaceIfUnchanged(path string, before os.FileInfo, data []byte) (bool, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // 换上去之后这个名字已经没了
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, before.Mode().Perm())
	}
	if err != nil {
		return false, err
	}
	cur, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !os.SameFile(cur, before) || cur.Size() != before.Size() || !cur.ModTime().Equal(before.ModTime()) {
		return false, nil
	}
	return true, os.Rename(tmp, path)
}

// SetJSONKey 把 JSON 对象里最外层 key 的值换成 value（已经是 JSON），别的字节一个不动；没有这个键就加在最前面。不是合法的 JSON 对象返回错误。
func SetJSONKey(raw []byte, key, value string) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, errors.New("不是合法的 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("不是 JSON 对象")
	}
	open := int(dec.InputOffset())
	type span struct{ from, to int }
	var hits []span
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if k == key {
			end := int(dec.InputOffset())
			hits = append(hits, span{end - len(v), end})
		}
	}
	if len(hits) == 0 {
		sep := ","
		if len(bytes.TrimSpace(raw[open:])) == 1 { // 空对象：只剩右括号
			sep = ""
		}
		return []byte(string(raw[:open]) + "\n  " + strconv.Quote(key) + ": " + value + sep + string(raw[open:])), nil
	}
	var out []byte
	at := 0
	for _, h := range hits { // 同一个键写了两遍：都改，免得解析时后面的赢
		out = append(append(out, raw[at:h.from]...), value...)
		at = h.to
	}
	return append(out, raw[at:]...), nil
}

const LaunchdLabel = "io.agentq.agentq"

const (
	KindNight  = "night"
	KindReview = "review"
)

type Routine struct {
	Kind   string // 命令名，也记在 Job.Kind
	Name   string // 汇总、status 里显示的名字，也当 Job.Project
	Key    string // config.json 里的开关
	File   string // 仓库里的说明文件
	Hour   int    // launchd 定点叫醒的时刻
	Minute int
	Report string // 说明要求写出的报告：~/.claude/nightly/<日期><report>.md
}

var Routines = map[string]Routine{
	KindNight:  {Kind: KindNight, Name: "夜班", Key: "night_enabled", File: "prompts/night.md", Hour: 1, Minute: 0, Report: "-night"},
	KindReview: {Kind: KindReview, Name: "复盘", Key: "review_enabled", File: "prompts/review.md", Hour: 6, Minute: 30, Report: ""},
}

func (r Routine) Enabled(cfg Config) bool {
	if r.Kind == KindNight {
		return cfg.NightEnabled
	}
	return cfg.ReviewEnabled
}

func (r Routine) At() string { return fmt.Sprintf("%02d:%02d", r.Hour, r.Minute) }
