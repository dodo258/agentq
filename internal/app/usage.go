package app

import (
	"slices"
	"strings"

	"agentq/internal/evolve"
	"agentq/internal/notify"
)

// 用法文字只在这里写一份：agentq help 按表拼出全部，agentq <命令> --help 取这条命令的那几行再加参数说明。

// cmdHelp 是一条命令的用法。
type cmdHelp struct {
	names  []string // 认这些命令名
	lines  string   // agentq help 里的那几行，原样
	detail string   // 只在 --help 时跟在后面：参数说明（缩进开头），或模块自己那份详细用法
}

type usageSection struct {
	head string
	cmds []cmdHelp
}

const usageHead = "agentq：后台调度器，把各项目 docs/BACKLOG.md「待做」交给 claude -p 做到合并\n"

const usageTail = `自然语言需求：在 Claude Code 或 Codex 里直接说，dispatch 技能判断当场做还是交后台；agentq 不是自然语言聊天命令。
配置 ~/.agentq/config.json，任务记录 ~/.agentq/jobs/<id>/，每日汇总 ~/.claude/nightly/<日期>-agentq.md
`

var usageSections = []usageSection{
	{head: "上手：装好就在运行，在 Claude Code 或 Codex 里直接说需求，下面这些由派活技能替你跑。", cmds: []cmdHelp{
		{names: []string{"init"},
			lines:  `  agentq init [目录]              把项目接入：检查目录，生成缺的 docs/BACKLOG.md、AGENTS.md 等并提交（默认当前目录）`,
			detail: `  [目录]          要接入的项目目录，不写就是当前目录`},
		{names: []string{"add"},
			lines: `  agentq add "<需求>" [--project 名字]  往当前项目（或 --project）的「待做」末尾加一条并提交；不是 Git 仓库就先 git init 接入`,
			detail: `  "<需求>"        一段需求，用引号包成一个参数；以 - 开头时写在 -- 后面：agentq add -- "<需求>"
  --project 名字  加到这个项目（agentq projects 里的目录名），不写就是当前目录的项目`},
		{names: []string{"start"},
			lines: `  agentq start [--accept-bypass]  打开后台：检查、确认权限、装调度服务、取消暂停，立刻接活（可重复运行）
  agentq start --welcome [--paused]  一键脚本装完用：装完即运行（安装就是同意，只告知），--paused 只装不跑
  agentq start --check            只读：后台能不能接活，第一行是 ready、not-running、paused、off、need-cli 或 skills-only（派活技能用）`,
			detail: `  --accept-bypass  同意把 permission_mode 改成完全权限（不在终端里、没法问你时要带）
  --welcome        一键安装脚本用；可加 --paused 只装不跑
  --check          只读检查，不改任何东西；不能和其他参数一起用`},
		{names: []string{"env"},
			lines: `  agentq env status|on|off        通用工程规则：看状态、装上、关掉（只删没改过的文件）`,
			detail: `  status  看规则装没装、哪些文件改过
  on      装上
  off     关掉，只删没改过的文件`},
		{names: []string{"version", "--version"},
			lines:  `  agentq version                  版本号（--version 同义）`,
			detail: `  不带参数`},
	}},
	{head: "其他命令：", cmds: []cmdHelp{
		{names: []string{"tick"},
			lines:  `  agentq tick                     一次调度：收尾、超时处理、挑活启动（launchd 每 5 分钟调用，每个任务结束时也会调用一次）`,
			detail: `  不带参数（--after <任务id> 是任务外壳在任务结束后用的）`},
		{names: []string{"run"},
			lines: `  agentq run <项目名> [条目关键字]  立刻为该项目启动一个任务（不看暂停，仍遵守上限；订阅额度 ≥98% 时拒绝）`,
			detail: `  <项目名>      agentq projects 里的目录名
  [条目关键字]  挑「待做」里含这段文字的第一条，不写就挑第一条能做的`},
		{names: []string{"new"},
			lines: `  agentq new [--direction "方向"] "<一句话>"  从零做出新项目第一版：按 new-project 建项目，把第一条待做做到合并，结果写当天汇总并推送（配了 Telegram 先问一次大方向，等 2 小时没回按默认，--direction 已经定了就不问；不看暂停，占名额；一句话不超过 500 字；额度 ≥98% 时拒绝）`,
			detail: `  --direction "方向"  大方向已经定了就写在这里，不再问
  "<一句话>"          想做的东西，一个参数，不以 - 开头`},
		{names: []string{"discuss"},
			lines: `  agentq discuss [answer <编号> "<回答>"]  在商量大方向的条目：列出问题和默认答案，或回答（回「随便」就按默认）；标了（大方向）的条目开工前问一次，等 2 小时没回按默认，discuss_enabled=false 关掉`,
			detail: `  不带参数                    列出在商量的条目、问题和默认答案
  answer <编号> "<回答>"      回答这一条；<编号> 是列表每行开头那个`},
		{names: []string{"status"},
			lines:  `  agentq status                   订阅额度、运行中的任务、各项目待做条数、最近结束的任务`,
			detail: `  不带参数，只读`},
		{names: []string{"projects"},
			lines:  `  agentq projects                 列出所有项目（projects_root 下的和 extra_projects 登记的），每行：目录名、路径，被排除的多一列`,
			detail: `  不带参数，只读`},
		{names: []string{"stats"},
			lines:  `  agentq stats [天数]              最近 N 天（默认 7）按任务种类、项目、模型统计次数、成败、token 和折算费用，按门统计通过、失败、判不了、warn 档会挡下多少、退回和放行；最后是最近 7 天的一次满意率、返工率和每条的依据（只读）`,
			detail: `  [天数]  正整数，默认 7`},
		{names: []string{"backlog"},
			lines: `  agentq backlog archive <项目名|--all> [--dry-run]  把 BACKLOG「已完成」的旧条目移到 docs/BACKLOG-archive.md（前 20 条和 14 天内合并的留下；不自动提交）`,
			detail: `  archive    现在只有这一个子命令
  <项目名>   只整理这个项目；--all 整理全部项目，两者选一
  --dry-run  只列出会移走的条目，不改文件`},
		{names: []string{"patrol"},
			lines:  `  agentq patrol                   立刻巡逻一次：launchd 服务、备份推送、任务失败和卡住、额度、早报和夜班报告、等拍板条目、磁盘（tick 每小时自动巡逻一次）`,
			detail: `  不带参数`},
		{names: []string{"gate"},
			lines: `  agentq gate report|pass|return|rerun|abandon <任务id> [--reason "原因"]  过门：看报告；放行这一次卡在门上的任务（原因必写）；带意见退回接着改（意见必写）；再审一次（不改代码重新跑门，不算退回轮数，同一提交最多 2 次）；放弃卡住的任务或新项目的第一版，分支留着`,
			detail: `  report <任务id>                    看门的报告（只读）
  pass <任务id> --reason "原因"      放行这一次
  return <任务id> --reason "意见"    带意见退回，同一会话接着改
  rerun <任务id>                     不改代码再跑一次门
  abandon <任务id>                   放弃，分支留着
  <任务id> 见 agentq status`},
		{names: []string{"eval"},
			lines:  `  agentq eval list|run|baseline|compare  评测集：给 agentq 自己打分。run 默认只跑不调模型的程序题，--fake 用假 CLI，--real 才调模型（周额度 ≥85% 拒绝）；compare 和基线比，任一题或任一领域变差就报（agentq eval 看详细用法）`,
			detail: evolve.EvalUsage},
		{names: []string{"evolve"},
			lines:  `  agentq evolve signals|constraints <项目名>|upgrades|apply  自我进化：看 tick 从门结果、任务记录、巡逻收到的信号和提案；看项目学到的约束（确认过的门拦截，新任务合同末尾带上）；看自己升级、回滚的记录；apply 现在就照规则升级或回滚一次`,
			detail: evolve.EvolveUsage},
		{names: []string{"health"},
			lines:  `  agentq health [项目名]           最近 30 天每个模块的改动次数、修复次数、行数，反复修的模块，两周返工率（只读，只看 git，不调模型）`,
			detail: `  [项目名]  只看这个项目，不写看全部项目`},
		{names: []string{"logs"},
			lines:  `  agentq logs <任务id>             结果摘要和 stderr 末尾`,
			detail: `  <任务id>  见 agentq status`},
		{names: []string{"stop"},
			lines:  `  agentq stop <任务id>             杀掉该任务的进程组`,
			detail: `  <任务id>  见 agentq status`},
		{names: []string{"pause", "resume"},
			lines:  `  agentq pause | resume           暂停 / 恢复自动接活`,
			detail: `  不带参数`},
		{names: []string{"night"},
			lines:  `  agentq night                    立刻执行一次夜班（prompts/night.md）；launchd 每天 01:00 叫醒，night_enabled=true 才执行`,
			detail: `  不带参数（--scheduled 是定点服务叫醒时用的：开关没打开就只记一行日志）`},
		{names: []string{"review"},
			lines:  `  agentq review                   立刻执行一次复盘（prompts/review.md）；launchd 每天 06:30 叫醒，review_enabled=true 才执行`,
			detail: `  不带参数（--scheduled 是定点服务叫醒时用的：开关没打开就只记一行日志）`},
		{names: []string{"notify"},
			lines:  `  agentq notify setup|test|send|file|status  推送：配 Telegram、发测试消息、记一条要推的消息、马上发一个文件、看推送情况（agentq notify 看详细用法）`,
			detail: notify.NotifyUsage},
		{names: []string{"listen"},
			lines:  `  agentq listen                   常驻收你的 Telegram 消息（长轮询，随到随回；launchd 的 KeepAlive 服务调用）`,
			detail: `  不带参数`},
		{names: []string{"install", "uninstall"},
			lines:  `  agentq install | uninstall      装上 / 卸下 launchd 服务（每 5 分钟 tick 一次、夜班和复盘两个定点任务、常驻的 Telegram 监听）`,
			detail: `  不带参数；带参数时 install 同 agentq setup，uninstall 同 agentq remove`},
	}},
	{head: "安装与维护：", cmds: []cmdHelp{
		{names: []string{"setup"},
			lines: `  agentq setup [--yes] [--scope full|skills] [--services] [--listen] [--backup] [--watchdog] [--extras]`,
			detail: `  --yes                  确认执行计划里的改动；不带只打印计划，不改
  --quiet                只打印警告和说明，失败时再打印详情
  --scope full|skills    full 装全部（默认），skills 只装技能
  --backend 名字         新安装用的后端：claude（默认）、codex 或 pi；已有配置不改
  --backend-path 路径    CLI 的路径（--claude 同义）
  --plan                 只准备并打印计划，不启用
  --services             装 tick、夜班、复盘服务
  --listen               装 Telegram 监听服务（要和 --services 一起）
  --backup               装私有 Git 备份服务
  --watchdog             装本机看门狗服务
  --extras               带上可选的第三方技能
  --defer-engine         CLI 没装或没登录也先装上（install.sh 用）`},
		{names: []string{"update"},
			lines: `  agentq update [--version 版本] [--proxy 地址] [--mirror 前缀]  升级：取发布地址上的 install.sh，校验后运行`,
			detail: `  --version 版本   要升到的版本，默认 latest；main 走源码
  --proxy 地址     下载走的代理（默认取 AGENTQ_PROXY）
  --mirror 前缀    GitHub 下载镜像前缀，校验文件不走镜像（默认取 AGENTQ_MIRROR）
  --check          只看发布上的最新版本（对过校验），不装
  --saved-net      沿用一键安装时用过的 --proxy、--mirror（自动更新用）`},
		{names: []string{"doctor"},
			lines:  `  agentq doctor                  只读安装、CLI 和配置检查`,
			detail: `  不带参数，只读`},
		{names: []string{"rollback"},
			lines:  `  agentq rollback --yes           回到上一完整安装代，不回滚运行数据`,
			detail: `  --yes  必须带：确认执行；不带就拒绝，只说明会改什么`},
		{names: []string{"remove"},
			lines: `  agentq remove --yes             卸载受管入口和服务，保留数据及恢复材料`,
			detail: `  --yes    必须带：确认执行；不带就拒绝，只说明会改什么
  --quiet  只打印警告和说明，失败时再打印详情`},
		{names: []string{"restore"},
			lines:  `  agentq restore --yes            恢复中断事务或最近一次卸载`,
			detail: `  --yes  必须带：确认执行；不带就拒绝，只说明会改什么`},
	}},
}

// Usage 是 agentq help 的全文。
func Usage() string {
	var b strings.Builder
	b.WriteString(usageHead)
	for _, s := range usageSections {
		b.WriteString("\n" + s.head + "\n")
		for _, c := range s.cmds {
			b.WriteString(c.lines + "\n")
		}
	}
	b.WriteString("\n" + usageTail)
	return b.String()
}

// CommandHelp 是 agentq <命令> --help 的输出；不是列在用法里的命令返回 false。
func CommandHelp(name string) (string, bool) {
	for _, s := range usageSections {
		for _, c := range s.cmds {
			if slices.Contains(c.names, name) {
				head := "参数：\n"
				if !strings.HasPrefix(c.detail, " ") { // eval、evolve、notify 带的是模块自己那份详细用法，自带标题
					head = ""
				}
				return "用法：\n" + c.lines + "\n\n" + head + strings.TrimRight(c.detail, "\n") + "\n", true
			}
		}
	}
	return "", false
}

// Commands 是用法里列出的全部命令名，测试遍历用。
func Commands() []string {
	var out []string
	for _, s := range usageSections {
		for _, c := range s.cmds {
			out = append(out, c.names...)
		}
	}
	return out
}

// WantsHelp：参数里有 -h 或 --help（-- 之后的是正文，不算）。
func WantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-h", "--help":
			return true
		}
	}
	return false
}
