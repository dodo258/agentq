package backend

import (
	"fmt"
	"slices"
	"strings"
)

// Row 是能力表的一行。Note 是缺了的时候怎么办。
type Row struct {
	Name string
	Have bool
	Note string
}

// Rows 是这个后端的能力表，对应调研 §4.4 的每一行。
func Rows(b Backend) []Row {
	c := b.Caps()
	_, quota := b.(QuotaReader)
	_, tools := b.(ToolLog)
	_, perms := b.(Permissions)
	_, setup := b.(CLISetup)
	_, ruleDir := b.(GlobalRules)
	_, ruleBlock := b.(RulesBlock)
	rows := []Row{
		{"续跑", c.Resume, "中断后接不回原会话，由接手任务从分支现状接着做：丢对话上下文要重新读，未提交的改动还在 worktree"},
		{"预设会话 id", c.PresetSession, "从输出里取会话 id，取到之前被杀就走接手"},
		{"美元上限", c.HardBudget, budgetNote(b)},
		{"回合上限", c.MaxTurns, "短任务用短超时兜底"},
		{"结构化输出", c.Schema, "追问靠提示词要 JSON 并校验，校验不过当没结果；其余要它的任务不启动"},
		{"只读和无工具", c.ToolProfiles, "要它的任务不启动"},
		{"隔离 MCP", c.NoExternalMCP, "要它的任务不启动"},
		{"用量", c.Usage, "status 和当天汇总写未知，合计注明几个任务没有用量"},
		{"费用", c.Cost, "只有 token 没有金额，美元预算上限查不了"},
		{"额度查询", quota, "不能提前不领活，只在撞限后冷却"},
		{"撞限信号", c.LimitSignal, "认不出撞限，落到普通失败和熔断"},
		{"登录探测", c.AuthProbe, "照常启动，第一次因认证失败才暂停这个后端并通知你"},
		{"工具调用记录", tools, "看进展的失控、无改动、重复信号记未知不报，只留疑似卡死"},
		{"技能目录", c.Skills, "技能发现不了，提示词要写完整路径"},
		{"引擎审查", c.CanReview(), "引擎起不了审查进程，合并前的独立审查做不了，待做和 new 不接活"},
		{"图片输入", c.ImageInput, "模型看不到截图，界面门的模型部分判不了，不放行"},
		{"用户级规则", ruleDir || ruleBlock, "环境层不装，通用工程规则到不了这个后端"},
		{"权限写法", perms, "permission_mode 的值不查写法"},
		{"带你安装和登录", setup, "缺 CLI 或没登录时只报错，告诉你官方命令"},
	}
	if c.SchemaByPrompt && !c.Schema {
		rows = append(rows, Row{"审查结论的格式", false, "靠提示词要 JSON，不是 CLI 强制：不合格重试一次，再不行判不了，不放行"})
	}
	if c.NoArgvLabel {
		rows = append(rows, Row{"命令行进程标记", false, "靠环境里的 AGENTQ_JOB 认进程；认不出时不杀也不重复启动，超时后只收尾"})
	}
	return rows
}

// Task 是依赖硬能力的任务。
type Task struct {
	Name     string
	Must     Must
	Fallback string
}

// Tasks 列出能被后端能力关掉的任务。
var Tasks = []Task{
	{"待做和 new（要合并）", MustMerge, "不接活"},
	{"大方向商量", MustDiscuss, "不问，按默认开工"},
	{"追问", MustFollowup, "回退成普通对话任务"},
	{"巡逻分析", MustPatrol, "巡逻照常报，不起分析"},
	{"界面门的模型部分", MustImages, "判不了，不放行"},
}

// Describe 是 doctor 给人看的一段：当前后端、版本、认证、能力表和降级说明。perr 是 Probe 的错误，CLI 没装或没登录都如实写。
func Describe(b Backend, p Probe, perr error) string {
	var s strings.Builder
	if !Known(b.Name()) {
		fmt.Fprintf(&s, "后端：%s（%s，不接活）\n%v\n", b.Name(), unknownWord(b.Name()), perr)
		return s.String()
	}
	switch {
	case p.Path != "":
		fmt.Fprintf(&s, "后端：%s %s（%s）\n", b.Name(), p.Version, p.Path)
	default:
		fmt.Fprintf(&s, "后端：%s，CLI 没找到\n", b.Name())
	}
	switch {
	case p.Authenticated && p.AuthMethod != "":
		fmt.Fprintf(&s, "认证：已登录（%s）\n", p.AuthMethod)
	case p.Authenticated:
		s.WriteString("认证：已登录\n")
	case perr != nil && p.Path != "":
		s.WriteString("认证：没登录或查不到\n")
	case perr != nil:
		s.WriteString("认证：CLI 没找到，没查\n")
	default:
		s.WriteString("认证：查不了，照常启动，第一次因认证失败才暂停\n")
	}
	if perr != nil {
		fmt.Fprintf(&s, "问题：%v\n", perr)
	}
	s.WriteString("能力：\n")
	for _, r := range Rows(b) {
		if r.Have {
			fmt.Fprintf(&s, "  %s：有\n", r.Name)
		} else {
			fmt.Fprintf(&s, "  %s：无，%s\n", r.Name, r.Note)
		}
	}
	var off []string
	for _, t := range Tasks {
		if lacks := b.Caps().Lacks(t.Must); len(lacks) > 0 {
			off = append(off, t.Name+"（"+strings.Join(lacks, "、")+"，"+t.Fallback+"）")
		}
	}
	if len(off) == 0 {
		s.WriteString("关掉的任务：无\n")
	} else {
		s.WriteString("关掉的任务：" + strings.Join(off, "；") + "\n")
	}
	return s.String()
}

// unknownWord 是没注册的后端在一句话里的说法。
func unknownWord(name string) string {
	if NotYet(name) {
		return "暂不支持，以后再加"
	}
	return "未知"
}

// Limits 是这个后端缺的能力，一行一条，给 agentq start 提醒用。什么都不缺返回空。
func Limits(b Backend) []string {
	var out []string
	for _, r := range Rows(b) {
		if !r.Have {
			out = append(out, r.Name+"："+r.Note)
		}
	}
	return out
}

// ConfigProblems 列出配置决定了接不了活的：项目指定的后端没注册；默认后端或项目的后端做不了要合并的任务。
// 默认后端没注册不在这里，调用方单独说。status 和 doctor 共用。
func ConfigProblems(def string, projects map[string]string) []string {
	var out []string
	if Known(def) {
		d := Get(def, Options{})
		if lacks := d.Caps().Lacks(MustMerge); len(lacks) > 0 {
			out = append(out, "默认后端 "+d.Name()+" "+strings.Join(lacks, "、")+"：待做和 new 不接活")
		}
	}
	names := make([]string, 0, len(projects))
	for p := range projects {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		b := projects[p]
		if !Known(b) {
			out = append(out, fmt.Sprintf("项目 %s 的后端 %s %s，这个项目不接活（现有：%v）", p, b, unknownWord(b), Names()))
			continue
		}
		if lacks := Get(b, Options{}).Caps().Lacks(MustMerge); len(lacks) > 0 {
			out = append(out, "项目 "+p+" 的后端 "+b+" "+strings.Join(lacks, "、")+"：这个项目的待做和 new 不接活")
		}
	}
	return out
}
