package backend

import (
	"fmt"
	"strings"
)

// Caps 是后端声明的能力，调度层只看这个决定怎么降级。额度查询、工具调用记录、运行中用量是可选接口（QuotaReader、ToolLog、UsageStream），不在这里。
type Caps struct {
	Resume        bool // 能接回原会话，运行中就能拿到会话 id。会话 id 到结束才有的后端（中途被杀接不上）也算没有
	PresetSession bool // 启动时能指定会话 id；没有就从输出里取，取到之前被杀走接手
	HardBudget    bool // CLI 强制美元上限
	MaxTurns      bool // CLI 强制回合上限
	Schema        bool // CLI 强制结构化输出
	// SchemaByPrompt：CLI 不强制，但最后一条消息取得到，审查门能靠提示词要 JSON 再校验。输出不合格重试一次，再不行判不了，不放行：
	// 格式靠不住只会多判几次不了，放不过东西。别的要 MustSchema 的任务不算，它们靠强制的结构化输出防注入。
	SchemaByPrompt bool
	ToolProfiles   bool // CLI 强制只读、无工具
	DenyRules      bool // CLI 强制拒绝规则
	NoExternalMCP  bool // 能隔离用户配的 MCP
	Usage          bool // 结束时给 token 用量
	Cost           bool // 结束时给费用
	LimitSignal    bool // 撞限有信号（结构化字段，或错误通道的文字）
	AuthProbe      bool // 不调模型能查登录
	Skills         bool // 能从目录发现技能
	ReviewProfile  bool // 能无人值守地在 worktree 里跑工具（ProfileReview）：引擎的审查进程要它
	ImageInput     bool // 模型看得到本地图片（Request.Images）：界面门的模型部分要它
	Monitor        bool // CLI 有后台通知工具（Monitor），提示词要叮嘱别用
	NoArgvLabel    bool // 命令行放不了进程标记（Request.Label）：收尾靠环境里的 AGENTQ_JOB 认进程，认不出时不杀也不重复启动
}

// 各功能的硬要求。写在这里而不是各功能里：doctor 要列出这个后端关掉了哪些任务，setup 在功能模块下层。
const (
	MustMerge    = MustReview
	MustRouter   = MustProfile | MustDeny | MustSchema | MustNoExternalMCP // 输入里有项目内容，可能被注入后读密钥
	MustSuggest  = MustRouter                                              // 同上
	MustSense    = MustProfile | MustSchema | MustNoExternalMCP            // 输入是外部评价和 issue 原文，靠提示词约束不安全
	MustRelease  = MustDeny | MustSchema                                   // 拒绝规则是发版的安全边界
	MustFollowup = MustProfile | MustNoExternalMCP                         // 结构化输出能靠提示词补，见 ExtractJSON
	MustPatrol   = MustProfile
	MustDiscuss  = MustProfile | MustNoExternalMCP // 输入里有条目原文，生成问题不用任何工具；结构化输出能靠提示词补
)

// CanReview：引擎能在这个后端上起独立审查进程。
func (c Caps) CanReview() bool { return (c.Schema || c.SchemaByPrompt) && c.ReviewProfile }

// Lacks 列出 m 里这个后端做不到的。
func (c Caps) Lacks(m Must) []string {
	var why []string
	if m&MustProfile != 0 && !c.ToolProfiles {
		why = append(why, "不能强制只读或无工具")
	}
	if m&MustDeny != 0 && !c.DenyRules {
		why = append(why, "不能强制拒绝规则")
	}
	if m&MustSchema != 0 && !c.Schema {
		why = append(why, "不能强制结构化输出")
	}
	if m&MustNoExternalMCP != 0 && !c.NoExternalMCP {
		why = append(why, "不能隔离用户配的 MCP")
	}
	if m&MustReview != 0 && !c.CanReview() {
		why = append(why, "引擎起不了审查进程（要结构化输出和 ProfileReview），合并前的独立审查做不了")
	}
	if m&MustImages != 0 && !c.ImageInput {
		why = append(why, "模型看不到本地图片，界面门的审查做不了")
	}
	return why
}

// CheckMust 看 Request 的硬要求这个后端做不做得到，做不到返回带原因的错误（errors.Is ErrUnsupported）。Runner.Start 启动前统一调一次，各后端的 Build 开头也调，多一道。
func CheckMust(b Backend, req Request) error {
	c := b.Caps()
	why := c.Lacks(req.Must)
	if req.Resume != "" && !c.Resume {
		why = append(why, "不能接回原会话")
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("%w：后端 %s %s", ErrUnsupported, b.Name(), strings.Join(why, "、"))
}

// Degrade 列出 req 里不在 Must、这个后端又做不到的要求。各后端的 Build 可以直接返回它，再加上自己知道的。
func Degrade(b Backend, req Request) []Degradation {
	c := b.Caps()
	var out []Degradation
	if req.BudgetUSD > 0 && !c.HardBudget {
		out = append(out, Degradation{"hard_budget", budgetNote(b)})
	}
	if req.MaxTurns > 0 && !c.MaxTurns {
		out = append(out, Degradation{"max_turns", "回合上限不生效，用短超时兜底"})
	}
	if req.Schema != "" && !c.Schema {
		out = append(out, Degradation{"schema", "结构化输出靠提示词要求，校验不过当这次没结果"})
	}
	if (req.Profile == ProfileReadOnly || req.Profile == ProfileNoTools) && !c.ToolProfiles {
		out = append(out, Degradation{"tool_profiles", "只读和无工具只靠提示词约束"})
	}
	if req.Deny != DenyNone && !c.DenyRules {
		out = append(out, Degradation{"deny_rules", "拒绝规则不生效"})
	}
	if req.NoExternalMCP && !c.NoExternalMCP {
		out = append(out, Degradation{"no_external_mcp", "用户配的 MCP 没隔离"})
	}
	return out
}

func budgetNote(b Backend) string {
	if _, ok := b.(UsageStream); ok {
		return "job_budget_usd 不是硬上限：tick 读运行中的用量，超了才停，最多滞后一个 tick（5 分钟）"
	}
	return "job_budget_usd 不生效，只有超时兜底"
}
