package backend

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentq/internal/textfmt"
)

// JobUsage 是一个任务花了多少（Backend.Parse 读出来的）。和 internal/runner/usage.go 的订阅额度不是一回事。
type JobUsage struct {
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	// CostUnknown：后端只给 token 不给金额，CostUSD 没有意义。
	CostUnknown bool `json:"cost_unknown,omitempty"`
}

// AddUsage 把 u 加到 sum 上，返回新的合计，两个都不改。nil 是没有。
func AddUsage(sum, u *JobUsage) *JobUsage {
	if u == nil {
		return sum
	}
	out := *u
	if sum != nil {
		out.Input += sum.Input
		out.Output += sum.Output
		out.CacheRead += sum.CacheRead
		out.CacheWrite += sum.CacheWrite
		out.CostUSD += sum.CostUSD
		out.CostUnknown = out.CostUnknown || sum.CostUnknown
	}
	return &out
}

func (u JobUsage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Minus 返回 u - p；有一项变小（不是累计的）就返回 false。
func (u JobUsage) Minus(p JobUsage) (JobUsage, bool) {
	d := JobUsage{Input: u.Input - p.Input, Output: u.Output - p.Output, CacheRead: u.CacheRead - p.CacheRead,
		CacheWrite: u.CacheWrite - p.CacheWrite, CostUSD: u.CostUSD - p.CostUSD, CostUnknown: u.CostUnknown}
	ok := d.Input >= 0 && d.Output >= 0 && d.CacheRead >= 0 && d.CacheWrite >= 0 && d.CostUSD >= 0
	return d, ok
}

const UsageCacheFor = 10 * time.Minute // 读一次管这么久

// 到了重置时刻再等这么久才算恢复：本机时钟可能比服务端快一点，刚到点就启动会撞上限。
const UsageResetSlack = time.Minute

type UsageWindow struct {
	// Label 是原文冒号前的部分去掉 "Current "：session、week、week (Fable)。
	Label string `json:"label"`
	// Scope 是周窗口括号里的模型（如 Fable）；「all models」和会话窗口为空。只限某个模型的窗口，
	// 任务用的不是这个模型就不计入（照样显示）。
	Scope    string     `json:"scope,omitempty"`
	Used     float64    `json:"used"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

type UsageInfo struct {
	CheckedAt time.Time     `json:"checked_at"`
	Windows   []UsageWindow `json:"windows,omitempty"`
	// Err 非空表示这次没读出来（命令失败、输出看不懂），档位当「未知」，照常跑。
	Err string `json:"err,omitempty"`
	// 上次写进当天汇总的日期和档位：每天第一次读到、或档位变了才写一行。
	NotedDay   string `json:"noted_day,omitempty"`
	NotedLevel int    `json:"noted_level,omitempty"`
}

// Counts：这个窗口是否计入。只限某个模型的周窗口（如「week (Fable)」），只有任务用的模型（config 的 model）
// 名字里含这个模型名（括号里的第一个词）才算；会话窗口和「all models」总算。
func (w UsageWindow) Counts(model string) bool {
	if w.Scope == "" {
		return true
	}
	f := strings.Fields(strings.ToLower(w.Scope))
	return len(f) > 0 && strings.Contains(strings.ToLower(model), f[0])
}

// used 返回 now 时的已用百分比：过了重置时刻（再加 usageResetSlack）就当已重置，算 0。
func (w UsageWindow) UsedAt(now time.Time) float64 {
	if w.ResetsAt != nil && !now.Before(w.ResetsAt.Add(UsageResetSlack)) {
		return 0
	}
	return w.Used
}

func (w UsageWindow) Name() string {
	if w.Label == "session" {
		return "会话"
	}
	if w.Scope != "" {
		return "本周（" + w.Scope + "）"
	}
	return "本周"
}

// Stale：没读过、超过 usageCacheFor、时钟往回拨过，或读完之后有窗口过了重置时刻（要重新读拿新数）。
func (u *UsageInfo) Stale(now time.Time) bool {
	if u == nil || now.Before(u.CheckedAt) || now.Sub(u.CheckedAt) >= UsageCacheFor {
		return true
	}
	for _, w := range u.Windows {
		if w.ResetsAt != nil && w.ResetsAt.After(u.CheckedAt) && !now.Before(w.ResetsAt.Add(UsageResetSlack)) {
			return true
		}
	}
	return false
}

func BudgetLabel(usd float64) string {
	if usd <= 0 {
		return "预算"
	}
	return "$" + strconv.FormatFloat(usd, 'f', -1, 64)
}

// short：status 里用的「token 6.75M，约 $4.61」。
func JobUsageShort(u JobUsage) string {
	if u.CostUnknown {
		return "token " + textfmt.FmtTokens(u.Total()) + "，费用未知"
	}
	return fmt.Sprintf("token %s，约 $%.2f", textfmt.FmtTokens(u.Total()), u.CostUSD)
}

// detail：汇总里用的「token 6.75M（输入 122、输出 69.8K、缓存读 6.39M、缓存写 292.3K），约 $4.61」。
func JobUsageDetail(u JobUsage) string {
	if u.CostUnknown {
		return fmt.Sprintf("token %s（输入 %s、输出 %s、缓存读 %s、缓存写 %s），费用未知", textfmt.FmtTokens(u.Total()),
			textfmt.FmtTokens(u.Input), textfmt.FmtTokens(u.Output), textfmt.FmtTokens(u.CacheRead), textfmt.FmtTokens(u.CacheWrite))
	}
	return fmt.Sprintf("token %s（输入 %s、输出 %s、缓存读 %s、缓存写 %s），约 $%.2f", textfmt.FmtTokens(u.Total()),
		textfmt.FmtTokens(u.Input), textfmt.FmtTokens(u.Output), textfmt.FmtTokens(u.CacheRead), textfmt.FmtTokens(u.CacheWrite), u.CostUSD)
}

// TransientStatus：上游暂时出问题的状态码：5xx 和 422（真实历史里 422 是中转站转上游过载）。
// 429 走用量上限；408、409 和 401、403 这类认证、权限错误走普通失败，不重试。
func TransientStatus(code int) bool {
	return code == 422 || (code >= 500 && code <= 599)
}

// LimitScanTurns：回合数不超过这个的结果，才可能是一开始就撞了上限（瞬时错误判断「没开工」也用它）。
const LimitScanTurns = 3

// 额度门槛。到 UsageStopAt 就不领新活，到 usageRefuseAt 手动启动也拒绝。
const (
	UsageStopAt   = 90 // 任一计入的窗口到这个百分比：tick 不再启动新任务（正在跑的不动）
	usageRefuseAt = 98 // 到这个百分比：agentq run 也拒绝
)

// 额度档位。
const (
	UsageUnknown = -1 // 读不出或看不懂：照常跑
	UsageOK      = 0
	UsageStop    = 1 // ≥usageStopAt：tick 不启动新任务
	UsageRefuse  = 2 // ≥usageRefuseAt：run 也拒绝
)

// level 返回 now 时的档位；挡住启动时 until 是最晚恢复的时刻（零值 = 没写重置时间，要等下次读），
// why 列出挡住的窗口。
func UsageLevel(u *UsageInfo, now time.Time, model string) (lvl int, until time.Time, why string) {
	if u == nil || u.Err != "" || len(u.Windows) == 0 {
		return UsageUnknown, time.Time{}, ""
	}
	var parts []string
	noReset := false
	for _, w := range u.Windows {
		p := w.UsedAt(now)
		if !w.Counts(model) || p < UsageStopAt {
			continue
		}
		l := UsageStop
		if p >= usageRefuseAt {
			l = UsageRefuse
		}
		if l > lvl {
			lvl = l
		}
		parts = append(parts, fmt.Sprintf("%s %s%%", w.Name(), textfmt.FmtPct(p)))
		if w.ResetsAt == nil {
			noReset = true
		} else if r := w.ResetsAt.Add(UsageResetSlack); r.After(until) {
			until = r
		}
	}
	if noReset {
		until = time.Time{}
	}
	return lvl, until, strings.Join(parts, "、")
}

// describe：各窗口用量和重置时间一行，status、当天汇总、日志共用。
func UsageDescribe(u *UsageInfo, now time.Time, model string) string {
	if u == nil {
		return "还没读过"
	}
	if u.Err != "" {
		return "未知（" + u.Err + "），照常接活"
	}
	var parts []string
	for _, w := range u.Windows {
		s := w.Name() + " " + textfmt.FmtPct(w.Used) + "%"
		switch {
		case w.ResetsAt == nil:
			s += "（重置时间没读出）"
		case !now.Before(w.ResetsAt.Add(UsageResetSlack)):
			s += "（" + textfmt.WhenCN(*w.ResetsAt) + " 已重置）"
		default:
			s += "（" + textfmt.WhenCN(*w.ResetsAt) + " 重置）"
		}
		if !w.Counts(model) {
			used := "任务用 " + model
			if model == "" { // 跟随 CLI：不知道实际用哪个模型，撞了由撞限冷却兜住
				used = "任务跟随 CLI 选的模型"
			}
			s += "（只限 " + w.Scope + "，" + used + "，不计入）"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "；")
}

// gateNote：挡住启动时的说明。
func UsageGateNote(lvl int, until time.Time, why string) string {
	at := "，等下次读额度（" + textfmt.FormatDuration(UsageCacheFor) + " 一次）"
	if !until.IsZero() {
		at = "，到 " + textfmt.WhenCN(until) + " 重置后自动恢复"
	}
	if lvl >= UsageRefuse {
		return fmt.Sprintf("订阅额度快用完（%s，≥%d%%）：不启动新任务，agentq run 也拒绝%s", why, usageRefuseAt, at)
	}
	return fmt.Sprintf("订阅额度快用完（%s，≥%d%%）：不启动新任务（正在跑的不动），agentq run 仍可用%s", why, UsageStopAt, at)
}
