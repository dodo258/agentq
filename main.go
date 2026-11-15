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

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, app.Usage())
		return 2
	}
	cmd, rest := args[0], args[1:]
	if text, ok := app.CommandHelp(cmd); ok && app.WantsHelp(rest) { // 子命令 --help：只打印用法，什么都不做
		fmt.Fprint(stdout, text)
		return 0
	}
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
			fmt.Fprintf(stderr, "agentq %s：参数个数不对\n\n%s", cmd, app.Usage())
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
		fmt.Fprint(stdout, app.Usage())
		return 0
	}
	want := map[string][2]int{ // 参数个数下限、上限
		"tick": {0, 0}, "run": {1, 2}, "new": {1, 3}, "discuss": {0, 3}, "status": {0, 0}, "logs": {1, 1},
		"stop": {1, 1}, "pause": {0, 0}, "resume": {0, 0}, "install": {0, 0}, "uninstall": {0, 0},
		"night": {0, 0}, "review": {0, 0}, "backlog": {2, 3}, "stats": {0, 1}, "patrol": {0, 0}, "notify": {0, 30}, "listen": {0, 0},
		"init": {0, 1}, "add": {1, 4}, "start": {0, 2}, "env": {1, 1}, "projects": {0, 0}, "gate": {0, 4}, "health": {0, 1}, "eval": {0, 30}, "evolve": {0, 2}, app.GateProcToken: {0, 1},
	}
	n, ok := want[cmd]
	if !ok {
		fmt.Fprintf(stderr, "agentq：不认识的命令 %q\n\n%s", cmd, app.Usage())
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
		fmt.Fprintf(stderr, "agentq %s：参数个数不对\n\n%s", cmd, app.Usage())
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
	a.Evolve.Version = versionString() // 评测记哪一代跑的；自己升级认现在装的是哪个提交
	switch cmd {
	case app.GateProcToken: // 过门的后台进程，内部用，不列在用法里
		return a.RunGate(rest, stderr)
	case "gate":
		err = a.Gate.CmdGate(rest)
	case "health":
		err = a.Gate.CmdHealth(rest)
	case "eval":
		err = a.Evolve.CmdEval(rest)
	case "evolve":
		err = a.Evolve.CmdEvolve(rest)
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
	case "status":
		err = a.CmdStatus()
	case "projects":
		err = a.CmdProjects()
	case "stats":
		if err = a.Stats.CmdStats(rest); err == nil {
			fmt.Fprintln(a.Out)
			err = a.Quality.CmdQuality()
		}
	case "backlog":
		err = a.CmdBacklog(context.Background(), rest)
	case "patrol":
		err = a.Patrol.CmdPatrol()
	case "notify":
		err = a.CmdNotify(rest)
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
