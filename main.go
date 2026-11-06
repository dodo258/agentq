// agentq：launchd 每 5 分钟叫醒一次，把各项目 BACKLOG「待做」交给独立的 claude -p 进程按 agent-task 做到分支可合并，再由引擎过门合并。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"agentq/internal/app"
	"agentq/internal/chat"
	"agentq/internal/env"
	"agentq/internal/onboard"
	"agentq/internal/runner"
	"agentq/internal/setup"
	"agentq/internal/textfmt"
)

// version 由发行构建用 -ldflags "-X main.version=<版本>" 写入；本地构建是 dev。
var version = "dev"

// repo 由发行构建用 -ldflags "-X main.repo=<owner/repo>" 写入，agentq update 用；源码构建是空的。
var repo = ""

// versionString：本地构建带上 Go 记下的提交号，能分清装的是哪份源码。
func versionString() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return version
	}
	return version + "-" + textfmt.TruncRunes(rev, 7) + dirty
}

const usage = `agentq：后台调度器，把各项目 docs/BACKLOG.md「待做」交给 claude -p 做到合并

上手：
  agentq init [目录]              把项目接入：检查目录，生成缺的 docs/BACKLOG.md、AGENTS.md 等并提交（默认当前目录）
  agentq add "<需求>" [--project 名字]  往当前项目（或 --project）的「待做」末尾加一条并提交
  agentq start [--accept-bypass]  打开后台：检查、确认权限、装调度服务、取消暂停，立刻接活（可重复运行）
  agentq start --welcome [--assume-yes]  一键脚本用的第一次启动：讲清楚，按回车同意才启动
  agentq env status|on|off        通用工程规则：看状态、装上、关掉（只删没改过的文件）
  agentq version                  版本号（--version 同义）

其他命令：
  agentq tick                     一次调度：收尾、超时处理、挑活启动（launchd 每 5 分钟调用，每个任务结束时也会调用一次）
  agentq run <项目名> [条目关键字]  立刻为该项目启动一个任务（不看暂停，仍遵守上限；订阅额度 ≥98% 时拒绝）
  agentq new [--direction "方向"] "<一句话>"  从零做出新项目第一版：按 new-project 建项目，把第一条待做做到合并，结果写当天汇总并推送（配了 Telegram 先问一次大方向，等 discuss_wait 没回按默认，--direction 已经定了就不问；不看暂停，占名额；一句话不超过 500 字；额度 ≥98% 时拒绝）
  agentq discuss [answer <编号> "<回答>"]  在商量大方向的条目：列出问题和默认答案，或回答（回「随便」就按默认）；标了（大方向）的条目开工前问一次，配 discuss_enabled、discuss_wait
  agentq status                   订阅额度、运行中的任务、任务箱和各项目待做条数、最近结束的任务
  agentq projects                 列出所有项目（projects_root 下的和 extra_projects 登记的），每行：目录名、路径，被排除的多一列
  agentq inbox [add "<内容>"]      通用任务箱：列出，或放一条不属于任何项目的活（调研、写东西、查资料、整理文件）；项目待做之后、有名额时接，产出写到 ~/.agentq/outbox/
  agentq suggest [项目名]          手动跑一次闲时找活：只读地看各项目（或指定的一个）找值得做的活，低风险的写进「待做」、会改产品的写进「## 建议」并推送一条，回「做 1 3」移进「待做」（不看 suggest_enabled、暂停和当天次数，占用当天记录；额度 ≥98% 时拒绝）
  agentq sense [--dry-run]         手动跑一次主动感知：读 App Store 评价、TestFlight 反馈和 GitHub issue，有新内容才起模型判断，真问题写进「## 建议」，回「做 N」移进「待做」（--dry-run 只读、列读到多少条，不写文件不调模型；不看 sense_enabled；额度 ≥98% 时不判断）
  agentq release check [项目名]    只读显示发版条件：最新构建已处理且满 3 天、之后没有崩溃反馈、没有未处理的验收意见、距上次提交 ≥14 天、没有进行中的审核提交（提交审核只能回复那条 📦 提示消息说「发」，没有命令能提交）
  agentq quality                  最近 7 天的一次满意率、返工率和每条的依据（扫各项目 BACKLOG「已完成」和 git 历史）
  agentq stats [天数]              最近 N 天（默认 7）按任务种类、项目、模型统计次数、成败、token 和折算费用，按门统计通过、失败、判不了、warn 档会挡下多少、退回和放行（只读）
  agentq backlog archive <项目名|--all> [--dry-run]  把 BACKLOG「已完成」的旧条目移到 docs/BACKLOG-archive.md（前 20 条和 14 天内合并的留下；不自动提交）
  agentq patrol                   立刻巡逻一次：launchd 服务、备份推送、任务失败和卡住、额度、早报和夜班报告、等拍板条目、磁盘（tick 每小时自动巡逻一次）
  agentq gate report|pass|rerun|abandon <任务id> [--reason "原因"]  过门：看报告；放行这一次卡在门上的任务（原因必写）；再审一次（不改代码重新跑门，不算退回轮数，同一提交最多 2 次）；放弃卡住的任务或新项目的第一版，分支留着
  agentq eval list|run|baseline|compare  评测集：给 agentq 自己打分。run 默认只跑不调模型的程序题，--fake 用假 CLI，--real 才调模型（周额度 ≥85% 拒绝）；compare 和基线比，任一题或任一领域变差就报（agentq eval 看详细用法）
  agentq health [项目名]           最近 30 天每个模块的改动次数、修复次数、行数，反复修的模块，两周返工率（只读，只看 git，不调模型）
  agentq logs <任务id>             结果摘要和 stderr 末尾
  agentq stop <任务id>             杀掉该任务的进程组
  agentq pause | resume           暂停 / 恢复自动接活
  agentq night                    立刻执行一次夜班（prompts/night.md）；launchd 每天 01:00 叫醒，night_enabled=true 才执行
  agentq review                   立刻执行一次复盘（prompts/review.md）；launchd 每天 06:30 叫醒，review_enabled=true 才执行
  agentq notify setup|test|send|file|status  推送：配 Telegram、发测试消息、记一条要推的消息、马上发一个文件、看推送情况（agentq notify 看详细用法）
  agentq listen                   常驻收你的 Telegram 消息（长轮询，随到随回；launchd 的 KeepAlive 服务调用）
  agentq install | uninstall      装上 / 卸下 launchd 服务（每 5 分钟 tick 一次、夜班和复盘两个定点任务、常驻的 Telegram 监听）

安装与维护：
  agentq setup [--yes] [--scope full|skills] [--services] [--listen] [--backup] [--watchdog] [--extras]
  agentq update [--version 版本] [--proxy 地址] [--mirror 前缀]  升级：取发布地址上的 install.sh，校验后运行
  agentq doctor                  只读安装、CLI 和配置检查
  agentq rollback --yes           回到上一完整安装代，不回滚运行数据
  agentq remove --yes             卸载受管入口和服务，保留数据及恢复材料
  agentq restore --yes            恢复中断事务或最近一次卸载

自然语言需求：在项目目录运行 claude，按 dispatch 派活；agentq 不是自然语言聊天命令。
配置 ~/.agentq/config.json，任务记录 ~/.agentq/jobs/<id>/，每日汇总 ~/.claude/nightly/<日期>-agentq.md
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	if cmd == "install" && len(rest) > 0 {
		return setup.RunInstaller(installAssets, "setup", rest, stdout, stderr)
	}
	if cmd == "uninstall" && len(rest) > 0 {
		return setup.RunInstaller(installAssets, "remove", rest, stdout, stderr)
	}

	if cmd == "setup" || cmd == "doctor" || cmd == "rollback" || cmd == "remove" || cmd == "restore" {
		return setup.RunInstaller(installAssets, cmd, rest, stdout, stderr)
	}
	if cmd == "update" {
		return setup.RunUpdate(repo, rest, stdout, stderr)
	}
	if cmd == "version" || cmd == "--version" {
		if len(rest) != 0 {
			fmt.Fprintf(stderr, "agentq %s：参数个数不对\n\n%s", cmd, usage)
			return 2
		}
		fmt.Fprintln(stdout, "agentq", versionString())
		return 0
	}
	if cmd == "maintenance" {
		return setup.RunMaintenance(rest, stdout, stderr)
	}
	if cmd == "_job" { // 任务外壳，内部用，不列在用法里
		return runner.RunJobShell(rest, stderr)
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	want := map[string][2]int{ // 参数个数下限、上限
		"tick": {0, 0}, "run": {1, 2}, "new": {1, 3}, "discuss": {0, 3}, "suggest": {0, 1}, "sense": {0, 1}, "release": {1, 2}, "status": {0, 0}, "logs": {1, 1},
		"stop": {1, 1}, "pause": {0, 0}, "resume": {0, 0}, "install": {0, 0}, "uninstall": {0, 0},
		"night": {0, 0}, "review": {0, 0}, "quality": {0, 0}, "backlog": {2, 3}, "stats": {0, 1}, "patrol": {0, 0}, "notify": {0, 30}, "listen": {0, 0}, "inbox": {0, 30},
		"init": {0, 1}, "add": {1, 4}, "start": {0, 2}, "env": {1, 1}, "projects": {0, 0}, "gate": {0, 4}, "health": {0, 1}, "eval": {0, 30}, app.GateProcToken: {0, 1},
	}
	n, ok := want[cmd]
	if !ok {
		fmt.Fprintf(stderr, "agentq：不认识的命令 %q\n\n%s", cmd, usage)
		return 2
	}
	after := "" // tick --after <任务id>：任务外壳在任务结束后调用
	if cmd == "tick" && len(rest) == 2 && rest[0] == "--after" && rest[1] != "" {
		after, rest = rest[1], nil
	}
	scheduled := false // night/review --scheduled：launchd 定点叫醒，开关没打开就只记一行日志
	if (cmd == "night" || cmd == "review") && len(rest) == 1 && rest[0] == "--scheduled" {
		scheduled, rest = true, nil
	}
	if len(rest) < n[0] || len(rest) > n[1] || (cmd == "new" && len(rest) == 2) { // new 是一句话，或 --direction 方向 一句话
		fmt.Fprintf(stderr, "agentq %s：参数个数不对\n\n%s", cmd, usage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "agentq：找不到 HOME：", err)
		return 1
	}
	a, err := app.New(home, stdout, builtinAssets())
	if err != nil {
		if cmd == "tick" || cmd == "listen" || scheduled {
			// launchd 日志里要带时间，方便对上是哪一次 tick
			env.ForLog(stdout).Logf("%s：%v", cmd, err)
			if cmd == "listen" {
				time.Sleep(chat.ListenIdleWait) // KeepAlive 会马上再拉起它：歇一会儿再退出，免得配置写坏时每 10 秒刷一行
			}
		} else {
			fmt.Fprintln(stderr, "agentq：", err)
		}
		return 1
	}
	switch cmd {
	case app.GateProcToken: // 过门的后台进程，内部用，不列在用法里
		return a.RunGate(rest, stderr)
	case "gate":
		err = a.Gate.CmdGate(rest)
	case "health":
		err = a.Gate.CmdHealth(rest)
	case "eval":
		a.Evolve.Version = versionString()
		err = a.Evolve.CmdEval(rest)
	case "init":
		err = a.Onboard.CmdInit(rest)
	case "add":
		err = a.Onboard.CmdAdd(rest)
	case "start":
		err = a.Onboard.CmdStart(rest, onboard.StartOptions{In: os.Stdin, Interactive: onboard.IsTerminal(os.Stdin)})
	case "env":
		err = a.Onboard.CmdEnv(rest)
	case "tick":
		err = a.Sched.Tick(after)
		a.Sense.AfterTick()
	case "run":
		kw := ""
		if len(rest) == 2 {
			kw = rest[1]
		}
		err = a.CmdRun(rest[0], kw)
	case "new":
		err = a.Newproj.CmdNew(rest)
	case "discuss":
		err = a.Discuss.CmdDiscuss(rest)
	case "suggest":
		project := ""
		if len(rest) == 1 {
			project = rest[0]
		}
		err = a.Suggest.CmdSuggest(project)
	case "sense":
		err = a.Sense.CmdSense(rest)
	case "release":
		err = a.Release.CmdRelease(rest)
	case "status":
		err = a.CmdStatus()
	case "projects":
		err = a.CmdProjects()
	case "quality":
		err = a.Quality.CmdQuality()
	case "stats":
		err = a.Stats.CmdStats(rest)
	case "backlog":
		err = a.CmdBacklog(context.Background(), rest)
	case "patrol":
		err = a.Patrol.CmdPatrol()
	case "notify":
		err = a.CmdNotify(rest)
	case "inbox":
		err = a.Inbox.CmdInbox(rest)
	case "listen":
		err = a.Chat.CmdListen()
	case "logs":
		err = a.CmdLogs(rest[0])
	case "stop":
		err = a.CmdStop(rest[0])
	case "pause":
		err = a.Sched.CmdSetPaused(true)
	case "resume":
		err = a.Sched.CmdSetPaused(false)
	case "night", "review":
		err = a.Routine.CmdRoutine(cmd, scheduled)
	case "install":
		err = a.Service.CmdInstall()
	case "uninstall":
		err = a.Service.CmdUninstall()
	}
	if err != nil {
		if !errors.Is(err, env.ErrLogged) {
			fmt.Fprintln(stderr, "agentq：", err)
		}
		return 1
	}
	return 0
}
