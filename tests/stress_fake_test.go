package tests

// 暴力测试用的假 claude：编译成真的可执行文件（不是系统自带程序，ps -E 看得到它的环境变量，和真 claude 一样）。
// 每次启动在 $HOME/fake/calls.log 记一行，按 prompt 里的 ITEM-<号> 和 MODE=<方式> 决定怎么演。
// 同一条目两个活进程、并发超限，当场写进 $HOME/fake/violations.log。
const stressFakeSrc = `package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	home  = os.Getenv("HOME")
	fdir  = filepath.Join(home, "fake")
	tokRe = regexp.MustCompile("ITEM-[a-z0-9]+")
	modRe = regexp.MustCompile("MODE=([a-z0-9]+)")
	defRe = regexp.MustCompile("默认分支是 ([^，]+)，")
	// defaultBranch 只信提示词：合同没写清，master 项目开不出分支，暴力测试就会挂。
	defaultBranch = "main"
)

func appendLine(name, line string) {
	f, err := os.OpenFile(filepath.Join(fdir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}

func rec(ev string, kv map[string]any) {
	kv["ev"], kv["pid"], kv["ppid"], kv["t"] = ev, os.Getpid(), os.Getppid(), time.Now().UnixMilli()
	pg, _ := syscall.Getpgid(0)
	kv["pgid"] = pg
	b, _ := json.Marshal(kv)
	appendLine("calls.log", string(b))
}

func arg(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func classify(args []string, prompt string) string {
	switch {
	case strings.Contains(prompt, "大方向商量进程"): // 放最前：提示词里带着条目原文
		return "discuss"
	case strings.Contains(prompt, "审查门的独立审查进程"):
		return "review"
	case strings.Contains(prompt, "界面门的独立审查进程"):
		return "uireview"
	case strings.Contains(prompt, "这一条做到分支可合并："):
		return "task"
	case strings.Contains(prompt, "从零做出一个新项目"):
		return "new"
	case strings.Contains(prompt, "在 Telegram 上发来的消息"):
		return "chat"
	}
	return "other"
}

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "Z")
}

// checkLive 记下自己，看同一条目、同一项目、全局有没有超限。
func checkLive(tok string) func() {
	project := filepath.Base(mustWd())
	dir := filepath.Join(fdir, "live")
	os.MkdirAll(dir, 0o755)
	me := filepath.Join(dir, fmt.Sprintf("%d", os.Getpid()))
	os.WriteFile(me, []byte(tok+"\n"+project+"\n"), 0o644)
	maxPar, perProj := 0, 0
	if b, err := os.ReadFile(filepath.Join(fdir, "limits")); err == nil {
		fmt.Sscan(string(b), &maxPar, &perProj)
	}
	ents, _ := os.ReadDir(dir)
	total, inProject := 1, 1
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !alive(pid) {
			continue
		}
		f := strings.SplitN(string(b), "\n", 3)
		if len(f) < 2 {
			continue
		}
		total++
		if f[1] == project {
			inProject++
		}
		if f[0] == tok {
			appendLine("violations.log", fmt.Sprintf("DUP 条目 %s 同时有两个活进程：%d 和 %d", tok, pid, os.Getpid()))
		}
	}
	if maxPar > 0 && total > maxPar {
		appendLine("violations.log", fmt.Sprintf("OVER 全局并发 %d 超过 max_parallel=%d（pid %d %s）", total, maxPar, os.Getpid(), tok))
	}
	if perProj > 0 && inProject > perProj {
		appendLine("violations.log", fmt.Sprintf("OVER 项目 %s 并发 %d 超过 per_project=%d（pid %d %s）", project, inProject, perProj, os.Getpid(), tok))
	}
	return func() { os.Remove(me) }
}

// hangForever：真的挂住。不能用 select{}：没有别的 goroutine 时 Go 报 deadlock 直接退出。
func hangForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func mustWd() string {
	d, _ := os.Getwd()
	return d
}

// withMainLock 按 lockMain 的约定占 <项目>-main 锁改 BACKLOG（真 agent 用 with-lock.sh，同一把锁）。
func withMainLock(fn func()) {
	root := filepath.Join(home, ".claude", "state", "locks")
	if fi, err := os.Stat(filepath.Join(home, ".agentq", "locks")); err == nil && fi.IsDir() { // 和 with-lock.sh 同一条规矩
		root = filepath.Join(home, ".agentq", "locks")
	}
	dir := filepath.Join(root, filepath.Base(mustWd())+"-main")
	os.MkdirAll(filepath.Dir(dir), 0o755)
	deadline := time.Now().Add(20 * time.Second)
	for os.Mkdir(dir, 0o755) != nil {
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && syscall.Kill(pid, 0) == syscall.ESRCH {
			os.RemoveAll(dir)
			continue
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	defer os.RemoveAll(dir)
	fn()
}

// lineKey：条目行里认这一条的那段文字。巡逻写的「接手中断的「条目 ITEM-x」」也要认。
func lineKey(tok string) string { return tok }

func editBacklog(tok string, fn func(line string) (keep string, extra string)) {
	withMainLock(func() {
		path := filepath.Join("docs", "BACKLOG.md")
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		lines := strings.Split(string(b), "\n")
		extra := ""
		out := lines[:0:0]
		for _, l := range lines {
			if strings.HasPrefix(l, "- [ ]") && strings.Contains(l, lineKey(tok)) {
				k, e := fn(l)
				extra += e
				if k != "" {
					out = append(out, k)
				}
				continue
			}
			out = append(out, l)
		}
		text := strings.Join(out, "\n")
		if extra != "" {
			text = strings.TrimRight(text, "\n") + "\n" + extra
		}
		if text == string(b) {
			return
		}
		real := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			real = r // 软链接：改指向的文件，和编辑器一样
		}
		tmp := real + ".faketmp"
		if os.WriteFile(tmp, []byte(text), 0o644) != nil || os.Rename(tmp, real) != nil {
			return
		}
		git := exec.Command("git", "commit", "-q", "-m", "fake "+tok, "--", real)
		git.Run()
	})
}

func branchOf(tok string) string { return "agent/t-" + tok }

func claim(tok string) {
	editBacklog(tok, func(l string) (string, string) {
		if strings.Contains(l, "（进行中") {
			return l, ""
		}
		return l + "（进行中：" + branchOf(tok) + " " + time.Now().Format("2006-01-02 15:04") + "）", ""
	})
}

func git(dir string, args ...string) error {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
}

// worktreeFor：任务的 worktree，没有就从默认分支开，分支还在就接上。
func worktreeFor(tok string) string {
	main := mustWd()
	wt := filepath.Join(main, ".claude", "worktrees", "t-"+tok)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return wt
	}
	git(main, "worktree", "prune")
	if git(main, "worktree", "add", "-q", wt, branchOf(tok)) != nil {
		git(main, "worktree", "add", "-q", wt, "-b", branchOf(tok), defaultBranch)
	}
	return wt
}

// commitReady 在任务分支上提交改动和 ready.json，不合并。这是做完一条的全部。
func commitReady(tok, typ string, edit func(wt string)) {
	claim(tok)
	wt := worktreeFor(tok)
	edit(wt)
	os.MkdirAll(filepath.Join(wt, ".agentq"), 0o755)
	r, _ := json.Marshal(map[string]string{"branch": branchOf(tok), "type": typ, "acceptance": "看 " + tok, "progress": "做好了 " + tok})
	os.WriteFile(filepath.Join(wt, ".agentq", "ready.json"), r, 0o644)
	git(wt, "add", "-A")
	git(wt, "commit", "-q", "-m", "做 "+tok)
	rec("ready", map[string]any{"tok": tok})
}

func markDone(tok string) {
	commitReady(tok, "feature", func(wt string) {
		f, err := os.OpenFile(filepath.Join(wt, "work-"+tok+".txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			fmt.Fprintln(f, os.Getpid())
			f.Close()
		}
	})
}

// sharedFile 改一个所有 conflict 条目都改的文件：谁后过门谁冲突。退回后从 main 合入、用自己的内容解决。
func sharedFile(tok, resume string) {
	commitReady(tok, "feature", func(wt string) {
		if resume != "" {
			git(wt, "merge", "-q", "-X", "ours", "--no-edit", defaultBranch)
		}
		os.WriteFile(filepath.Join(wt, "shared.txt"), []byte(tok+" "+strconv.Itoa(os.Getpid())+"\n"), 0o644)
		os.MkdirAll(filepath.Join(wt, "web"), 0o755) // 改到界面文件：界面门也在队列里跑（项目没脚本，判不了，默认不挡）
		os.WriteFile(filepath.Join(wt, "web", tok+".css"), []byte(tok+"\n"), 0o644)
	})
}

// codexMode：这次是按 codex exec 被调的。结果写成事件流，提示词从标准输入来。
var (
	codexMode bool
	threadID  string
)

func emit(v map[string]any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func codexStart(resume string) {
	threadID = resume
	if threadID == "" {
		threadID = fmt.Sprintf("01a1%04x-0000-7000-8000-%012x", os.Getpid()&0xffff, time.Now().UnixNano()&0xffffffffffff)
	}
	emit(map[string]any{"type": "thread.started", "thread_id": threadID})
	emit(map[string]any{"type": "turn.started"})
}

func codexRes(fields map[string]any) {
	text, _ := fields["result"].(string)
	if failed, _ := fields["is_error"].(bool); failed {
		if strings.Contains(text, "session limit") {
			text = "You\u2019ve hit your usage limit. Try again later."
		}
		if c, ok := fields["api_error_status"].(int); ok {
			text = fmt.Sprintf("unexpected status %d: %s", c, text)
		}
		emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": text}})
		return
	}
	if so, ok := fields["structured_output"]; ok {
		b, _ := json.Marshal(so)
		text = string(b)
	}
	emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": text}})
	emit(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 40, "output_tokens": 7, "reasoning_output_tokens": 3}})
}

// piMode：这次是按 pi -p --mode json 被调的。第一行 session 头，结果是 message_end 加 agent_settled，提示词从标准输入来。
var piMode bool

func piStart(session string) {
	if session == "" {
		session = fmt.Sprintf("pi-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	emit(map[string]any{"type": "session", "version": 3, "id": session, "cwd": mustWd()})
	emit(map[string]any{"type": "agent_start"})
	emit(map[string]any{"type": "turn_start"})
}

func piRes(fields map[string]any) {
	text, _ := fields["result"].(string)
	msg := map[string]any{"role": "assistant", "stopReason": "stop",
		"usage": map[string]any{"input": 100, "output": 7, "cacheRead": 40, "cacheWrite": 0, "totalTokens": 147, "cost": map[string]any{"total": 0.01}}}
	if failed, _ := fields["is_error"].(bool); failed {
		if strings.Contains(text, "session limit") {
			text = "429: {\"message\":\"You exceeded your current quota, please check your plan and billing details.\"}"
		} else if c, ok := fields["api_error_status"].(int); ok {
			text = fmt.Sprintf("%d: {\"message\":%q}", c, text)
		}
		msg["stopReason"], msg["errorMessage"], msg["content"] = "error", text, []any{}
		msg["usage"] = map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0, "cost": map[string]any{"total": 0}}
	} else {
		if so, ok := fields["structured_output"]; ok {
			b, _ := json.Marshal(so)
			text = string(b)
		}
		msg["content"] = []any{map[string]any{"type": "text", "text": text}}
	}
	emit(map[string]any{"type": "message_end", "message": msg})
	emit(map[string]any{"type": "turn_end", "message": msg, "toolResults": []any{}})
	emit(map[string]any{"type": "agent_end", "messages": []any{msg}, "willRetry": false})
	emit(map[string]any{"type": "agent_settled", "aborted": false})
}

func res(fields map[string]any) {
	if piMode {
		piRes(fields)
		return
	}
	if codexMode {
		codexRes(fields)
		return
	}
	base := map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s-" + strconv.Itoa(os.Getpid()), "num_turns": 7, "result": ""}
	for k, v := range fields {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	os.Stdout.Write(append(b, '\n'))
}

func okText() string { return "做完了。\n\n分支可以合并了；用户看 README。" }

func childMain(ignoreTerm bool) {
	if ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	rec("child", map[string]any{"ignore": ignoreTerm})
	time.Sleep(10 * time.Minute)
}

func spawnChild(ignoreTerm bool) {
	self, _ := os.Executable()
	mode := "__child"
	if ignoreTerm {
		mode = "__childterm"
	}
	c := exec.Command(self, mode) // 同一进程组，不等它，stdout 不接：真 claude 留下的后台子进程就是这样
	c.Start()
	time.Sleep(50 * time.Millisecond)
}

// hangHoldingPipe：自己挂住，子进程占着 stdout：只杀自己的话，读输出的一方还是等不到 EOF。
func hangHoldingPipe() {
	self, _ := os.Executable()
	c := exec.Command(self, "__child")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	c.Start()
	hangForever()
}

func taskMode(mode, tok, resume string) {
	switch mode {
	case "ok", "":
		time.Sleep(time.Duration(os.Getpid()%300) * time.Millisecond)
		claim(tok)
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "slow":
		claim(tok)
		time.Sleep(time.Duration(2500+os.Getpid()%3000) * time.Millisecond)
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "hang":
		hangForever()
	case "claimhang":
		claim(tok)
		hangForever()
	case "hangterm":
		signal.Ignore(syscall.SIGTERM)
		hangForever()
	case "quick":
		os.Exit(0)
	case "big":
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 50; i++ {
			os.Stdout.Write(chunk)
		}
	case "bigjson":
		claim(tok)
		markDone(tok)
		res(map[string]any{"result": strings.Repeat("合并", 8<<20) + "\n\n" + okText()})
	case "half":
		os.Stdout.WriteString("{\"type\":\"result\",\"is_err")
	case "nonjson":
		os.Stdout.WriteString("Error: something went sideways\n")
	case "exit3":
		res(map[string]any{"is_error": true, "result": "出错了"})
		os.Exit(3)
	case "orphan":
		spawnChild(false)
		claim(tok)
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "orphanterm":
		spawnChild(true)
		claim(tok)
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "badturns":
		res(map[string]any{"num_turns": "seven", "result": okText()})
	case "badapi":
		res(map[string]any{"api_error_status": map[string]any{"code": 500}, "is_error": true, "result": "boom"})
	case "badstruct":
		res(map[string]any{"structured_output": 42, "result": okText()})
	case "badresult":
		res(map[string]any{"result": []int{1, 2}})
	case "badorigin":
		res(map[string]any{"origin": "x", "result_index": "y", "result": okText()})
	case "claimdie":
		claim(tok)
		os.Exit(1)
	case "conflict":
		sharedFile(tok, resume)
		res(map[string]any{"result": okText()})
	case "uifile": // 改到界面文件
		commitReady(tok, "feature", func(wt string) {
			os.MkdirAll(filepath.Join(wt, "web"), 0o755)
			os.WriteFile(filepath.Join(wt, "web", tok+".tsx"), []byte("export default 1\n"), 0o644)
		})
		res(map[string]any{"result": okText()})
	case "addverify": // 默认分支上还没有检查命令：按合同先补上再交（放弃第一版后的下一条就是这样）
		commitReady(tok, "feature", func(wt string) {
			os.MkdirAll(filepath.Join(wt, ".agentq"), 0o755)
			os.WriteFile(filepath.Join(wt, ".agentq", "verify"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
			os.WriteFile(filepath.Join(wt, "work-"+tok+".txt"), []byte("x\n"), 0o644)
		})
		res(map[string]any{"result": okText()})
	case "noready": // 接手新项目的第一条：没做成，退出时分支上没有 ready.json
		wt := filepath.Join(mustWd(), ".claude", "worktrees", "first")
		git(wt, "rm", "-q", "-f", ".agentq/ready.json")
		git(wt, "commit", "-q", "-m", "还没好")
		res(map[string]any{"result": "还是没做好。"})
	case "fixcore": // 又修 core：第一次按修复交，被退回（合同改成重写）后按重写交
		typ := "fix"
		if resume != "" {
			typ = "rewrite"
		}
		commitReady(tok, typ, func(wt string) {
			os.MkdirAll(filepath.Join(wt, "core"), 0o755)
			os.WriteFile(filepath.Join(wt, "core", "a.go"), []byte("package core\n\n// "+tok+" "+strconv.Itoa(os.Getpid())+"\n"), 0o644)
		})
		res(map[string]any{"result": okText()})
	case "selfmerge": // 不听合同：写了 ready.json 又自己合并
		markDone(tok)
		withMainLock(func() { git(mustWd(), "merge", "-q", "--no-edit", branchOf(tok)) })
		res(map[string]any{"result": okText()})
	case "claimwait":
		if resume == "" {
			claim(tok)
			res(map[string]any{"result": "审查人还在复跑测试，继续等通知。"})
			return
		}
		markDone(tok)
		res(map[string]any{"result": "接着做完了。\n\n" + okText()})
	case "limit":
		res(map[string]any{"is_error": true, "result": "You've hit your session limit", "num_turns": 1})
	case "limitmid":
		if resume == "" {
			claim(tok)
			res(map[string]any{"is_error": true, "result": "You've hit your session limit", "num_turns": 12})
			return
		}
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "budget":
		if resume == "" {
			claim(tok)
			res(map[string]any{"subtype": "error_max_budget_usd", "is_error": true, "total_cost_usd": 150.01})
			os.Exit(1)
		}
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "e529":
		res(map[string]any{"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 0, "result": "API Error: Repeated 529 Overloaded errors."})
	case "e401":
		res(map[string]any{"is_error": true, "api_error_status": 401, "result": "Unauthorized: Missing bearer or basic authentication in header"})
	case "e403":
		res(map[string]any{"is_error": true, "api_error_status": 403, "num_turns": 1, "total_cost_usd": 0, "result": "Your organization has disabled Claude"})
	case "stderrflood":
		line := []byte(strings.Repeat("noise ", 1000) + "\n")
		for i := 0; i < 900; i++ {
			os.Stderr.Write(line)
		}
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "selfkill":
		os.Stdout.WriteString("{\"type\":\"res")
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "killtick":
		// 把启动自己的 tick 杀掉（外壳的父进程）：这时 tick 还没把任务写进状态文件
		out, _ := exec.Command("/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(os.Getppid())).Output()
		if gp, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && gp > 1 && resume == "" {
			syscall.Kill(gp, syscall.SIGKILL)
		}
		claim(tok)
		time.Sleep(4 * time.Second)
		markDone(tok)
		res(map[string]any{"result": okText()})
	case "killshell":
		syscall.Kill(os.Getppid(), syscall.SIGKILL)
		time.Sleep(1500 * time.Millisecond)
		markDone(tok)
		res(map[string]any{"result": okText()})
	default:
		appendLine("violations.log", "假 claude 不认识的 MODE="+mode)
	}
}

// reviewKind：引擎的审查进程。$HOME/fake/reviewmode 决定回什么，没有就通过。
func reviewKind() {
	b, _ := os.ReadFile(filepath.Join(fdir, "reviewmode"))
	switch strings.TrimSpace(string(b)) {
	case "block":
		res(map[string]any{"result": "有问题", "structured_output": map[string]any{"verdict": "block", "summary": "有问题", "findings": []any{map[string]any{"file": ".agentq/verify", "line": 1, "severity": "blocker", "evidence": "假的问题", "repro": "假的复现"}}}})
	case "noevidence":
		res(map[string]any{"result": "有问题", "structured_output": map[string]any{"verdict": "block", "summary": "说不清", "findings": []any{map[string]any{"file": "编的.go", "line": 0, "severity": "blocker", "evidence": "", "repro": ""}}}})
	case "bad":
		os.Stdout.WriteString("{\"type\":\"result\",\"is_error\":false,\"result\":\"不是 JSON\"}\n")
	case "crash":
		os.Stdout.WriteString("{\"type\":\"res")
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "limit":
		res(map[string]any{"is_error": true, "result": "You've hit your session limit", "num_turns": 1})
	case "hang":
		hangForever()
	case "failbare", "failempty", "faildetail", "errbare": // 先说通过，再报失败，失败事件没带文字
		emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": "{\"verdict\":\"pass\",\"summary\":\"没问题\",\"findings\":[]}"}})
		switch strings.TrimSpace(string(b)) {
		case "failbare":
			emit(map[string]any{"type": "turn.failed"})
		case "failempty":
			emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": ""}})
		case "faildetail":
			emit(map[string]any{"type": "turn.failed", "error": map[string]any{"detail": "x"}})
		default:
			emit(map[string]any{"type": "error"})
		}
	case "e529": // 上游 5xx：引擎退避后重跑，连续几次才判不了
		res(map[string]any{"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 0, "result": "API Error: Repeated 529 Overloaded errors."})
	case "reject": // schema 被接口拒绝：真实的 400
		res(map[string]any{"is_error": true, "api_error_status": 400, "result": "Invalid schema for response_format"})
	default:
		res(map[string]any{"result": "审过了", "structured_output": map[string]any{"verdict": "pass", "summary": "没问题", "findings": []any{}}})
	}
}

// uiReviewKind：界面门的审查进程。$HOME/fake/uimode 决定回什么，没有就通过。
func uiReviewKind() {
	b, _ := os.ReadFile(filepath.Join(fdir, "uimode"))
	verdict := func(v, skill string, findings ...any) {
		res(map[string]any{"result": "看过了", "structured_output": map[string]any{"verdict": v, "skill": skill, "summary": "看过截图了", "findings": append([]any{}, findings...)}})
	}
	switch strings.TrimSpace(string(b)) {
	case "block":
		verdict("block", "loaded", map[string]any{"screenshot": "home.png", "kind": "broken", "severity": "blocker", "evidence": "白屏"})
	case "opinion":
		verdict("block", "loaded", map[string]any{"screenshot": "home.png", "kind": "opinion", "severity": "blocker", "evidence": "不好看"})
	case "fakeshot":
		verdict("block", "loaded", map[string]any{"screenshot": "编的.png", "kind": "broken", "severity": "blocker", "evidence": "白屏"})
	case "missing":
		verdict("unknown", "missing")
	case "bad":
		os.Stdout.WriteString("{\"type\":\"result\",\"is_error\":false,\"result\":\"不是 JSON\"}\n")
	case "crash":
		os.Stdout.WriteString("{\"type\":\"res")
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "limit":
		res(map[string]any{"is_error": true, "result": "You've hit your session limit", "num_turns": 1})
	case "hang":
		hangForever()
	default:
		verdict("pass", "loaded")
	}
}

// discussKind：生成大方向问题。$HOME/fake/discussmode 决定回什么，没有就一个问题。
func discussKind() {
	b, _ := os.ReadFile(filepath.Join(fdir, "discussmode"))
	switch strings.TrimSpace(string(b)) {
	case "bad":
		os.Stdout.WriteString("{\"type\":\"result\",\"is_error\":false,\"result\":\"不是 JSON\"}\n")
	case "crash":
		os.Stdout.WriteString("{\"type\":\"res")
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "hang":
		hangForever()
	case "e529":
		res(map[string]any{"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 0, "result": "API Error: Repeated 529 Overloaded errors."})
	case "none":
		res(map[string]any{"structured_output": map[string]any{"questions": []any{}, "directions": []any{}}})
	default:
		res(map[string]any{"structured_output": map[string]any{"questions": []any{map[string]any{"text": "想要什么感觉", "default": "简洁"}}, "directions": []any{"纸墨风", "终端风"}}})
	}
}

// otherKind：对话、Router、闲时找活等。$HOME/fake/defmode 决定回什么。
func otherKind(kind, mode, prompt string) {
	switch mode {
	case "quick":
		return
	case "half":
		os.Stdout.WriteString("{\"type\":\"result\",\"res")
		return
	case "nonjson":
		os.Stdout.WriteString("not json at all\n")
		return
	case "badstruct":
		res(map[string]any{"structured_output": 7, "result": "x"})
		return
	case "badstruct2":
		res(map[string]any{"structured_output": map[string]any{"reply": 3, "actions": "no", "items": "no"}, "result": "x"})
		return
	case "exit3":
		res(map[string]any{"is_error": true, "result": "出错了"})
		os.Exit(3)
	case "limit":
		res(map[string]any{"is_error": true, "result": "You've hit your session limit", "num_turns": 1})
		return
	case "hang":
		hangForever()
	case "long":
		res(map[string]any{"result": strings.Repeat("很长的回答<b>加粗</b>&amp;\n", 5000), "structured_output": map[string]any{"reply": strings.Repeat("很长<b>的</b>回答\n", 5000), "actions": []any{}, "items": []any{}}})
		return
	case "html":
		res(map[string]any{"result": "<b>没关的标签 <script>alert(1)</script> & 1 < 2", "structured_output": map[string]any{"reply": "<b>没关的 <script> & 1 < 2", "actions": []any{}, "items": []any{}}})
		return
	}
	switch kind {
	case "new":
		// 建项目，认领第一条，门脚本和 ready.json 提交到任务分支，不合并：和别的条目走同一条路
		// FIRSTSTUCK：第一条门过不了，提示词里的两个 ITEM 号是第一条（MODE=noready，接手也做不成）和后面的条目
		name := "new-" + strconv.Itoa(os.Getpid())
		firstLine, verify := "第一条", "exit 0"
		rest := ""
		if toks := tokRe.FindAllString(prompt, 2); strings.Contains(prompt, "FIRSTSTUCK") && len(toks) == 2 {
			firstLine = "条目 " + toks[0] + " —— MODE=noready 验收：看着办"
			rest = "- [ ] 条目 " + toks[1] + " —— MODE=addverify 验收：看着办\n"
			verify = "exit 1"
		}
		os.MkdirAll(filepath.Join(name, "docs"), 0o755)
		os.WriteFile(filepath.Join(name, ".gitignore"), []byte(".claude/\n"), 0o644)
		os.WriteFile(filepath.Join(name, "docs", "BACKLOG.md"), []byte("# 待办\n\n## 待做\n\n- [ ] "+firstLine+"（进行中：agent/first 2026-01-01 10:00）\n"+rest+"\n## 已完成\n"), 0o644)
		gitIn := func(dir string, a ...string) {
			c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, a...)...)
			c.Dir = dir
			c.Run()
		}
		gitIn(name, "init", "-q", "-b", "main")
		gitIn(name, "add", "-A")
		gitIn(name, "commit", "-q", "-m", "init")
		gitIn(name, "worktree", "add", "-q", filepath.Join(".claude", "worktrees", "first"), "-b", "agent/first")
		wt := filepath.Join(name, ".claude", "worktrees", "first")
		os.MkdirAll(filepath.Join(wt, ".agentq"), 0o755)
		os.WriteFile(filepath.Join(wt, ".agentq", "verify"), []byte("#!/bin/sh\n"+verify+"\n"), 0o755)
		os.WriteFile(filepath.Join(wt, ".agentq", "smoke"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		os.WriteFile(filepath.Join(wt, "first.txt"), []byte("x\n"), 0o644)
		os.WriteFile(filepath.Join(wt, ".agentq", "ready.json"), []byte("{\"branch\":\"agent/first\",\"type\":\"feature\",\"acceptance\":\"看 first.txt\",\"progress\":\"第一版\"}\n"), 0o644)
		gitIn(wt, "add", "-A")
		gitIn(wt, "commit", "-q", "-m", "第一版")
		res(map[string]any{"num_turns": 40, "result": "做完了。\n\n项目路径：" + filepath.Join(mustWd(), name) + "\n补的假设：无\n第一版去哪看：README\n任务分支：agent/first"})
	case "chat":
		res(map[string]any{"result": "📊 <b>状态 · 今天</b>\n\n📊 收到了\n<i>12:00 · agentq 对话</i>"})
	default:
		for _, f := range strings.Fields(os.Getenv("AGENTQ_STRESS_REPORTS")) {
			os.MkdirAll(filepath.Dir(f), 0o755)
			os.WriteFile(f, []byte("report\n"), 0o644)
		}
		res(map[string]any{"result": okText()})
	}
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "__child" {
		childMain(false)
		return
	}
	if len(args) == 1 && args[0] == "__childterm" {
		childMain(true)
		return
	}
	if len(args) == 1 && args[0] == "--version" { // setup 探测用
		fmt.Println("2.1.296 (Claude Code)")
		return
	}
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("--print --output-format --session-id --effort")
		return
	}
	os.MkdirAll(fdir, 0o755)
	if len(args) > 1 && args[0] == "auth" && args[1] == "check" { // pi auth check --provider <p> --json
		rec("auth", map[string]any{"args": strings.Join(args, " ")})
		b, _ := os.ReadFile(filepath.Join(fdir, "authmode"))
		if strings.TrimSpace(string(b)) == "out" {
			fmt.Println("{\"status\":\"not_ready\",\"provider\":\"fakep\",\"reason\":\"credentials_not_configured\"}")
			os.Exit(1)
		}
		fmt.Println("{\"status\":\"ready\",\"provider\":\"fakep\",\"authType\":\"api_key\"}")
		return
	}
	if len(args) > 0 && args[0] == "auth" {
		rec("auth", map[string]any{})
		b, _ := os.ReadFile(filepath.Join(fdir, "authmode"))
		switch strings.TrimSpace(string(b)) {
		case "out":
			fmt.Println("{\"loggedIn\": false}")
		case "garbage":
			fmt.Println("???")
		case "fail":
			os.Exit(7)
		case "hang":
			hangHoldingPipe()
		case "leave":
			fmt.Println("{\"loggedIn\": true}")
			self, _ := os.Executable()
			c := exec.Command(self, "__child")
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			c.Start()
			return
		default:
			fmt.Println("{\"loggedIn\": true}")
		}
		return
	}
	if len(args) > 1 && args[0] == "login" {
		rec("auth", map[string]any{})
		b, _ := os.ReadFile(filepath.Join(fdir, "authmode"))
		switch strings.TrimSpace(string(b)) {
		case "out":
			fmt.Println("Not logged in")
			os.Exit(1)
		case "fail":
			os.Exit(7)
		case "hang":
			hangHoldingPipe()
		default:
			fmt.Println("Logged in using ChatGPT")
		}
		return
	}
	prompt := arg(args, "-p")
	resume := arg(args, "-r")
	if len(args) > 0 && args[0] == "exec" {
		codexMode = true
		in, _ := io.ReadAll(os.Stdin)
		prompt = string(in)
		if prompt == "" || args[len(args)-1] != "-" {
			appendLine("violations.log", "假 codex 没从标准输入收到提示词："+strings.Join(args, " "))
		}
		if len(args) > 2 && args[1] == "resume" {
			resume = args[2]
		}
		codexStart(resume)
		if d := os.Getenv("AGENTQ_FAKE_LOG"); d != "" { // 集成测试靠它等外壳退出（waitShells）
			os.WriteFile(filepath.Join(d, strconv.Itoa(os.Getpid())+".txt"), []byte(fmt.Sprintf("PPID[%d]\n", os.Getppid())), 0o644)
		}
	}
	if len(args) > 2 && args[0] == "-p" && args[1] == "--mode" && args[2] == "json" {
		piMode = true
		in, _ := io.ReadAll(os.Stdin)
		prompt = string(in)
		if prompt == "" {
			appendLine("violations.log", "假 pi 没从标准输入收到提示词："+strings.Join(args, " "))
		}
		resume = arg(args, "--session")
		session := resume
		if session == "" {
			session = arg(args, "--session-id")
		}
		piStart(session)
		if d := os.Getenv("AGENTQ_FAKE_LOG"); d != "" {
			os.WriteFile(filepath.Join(d, strconv.Itoa(os.Getpid())+".txt"), []byte(fmt.Sprintf("PPID[%d]\n", os.Getppid())), 0o644)
		}
	}
	if m := defRe.FindStringSubmatch(prompt); m != nil {
		defaultBranch = m[1]
	}
	if prompt == "/usage" {
		rec("usage", map[string]any{})
		b, _ := os.ReadFile(filepath.Join(fdir, "usagemode"))
		switch strings.TrimSpace(string(b)) {
		case "hang":
			hangHoldingPipe()
		case "leave":
			self, _ := os.Executable()
			c := exec.Command(self, "__child")
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			c.Start()
		}
		fmt.Println("{\"type\":\"result\",\"is_error\":false,\"result\":\"You are currently using your subscription to power your Claude Code usage\"}")
		return
	}
	kind := classify(args, prompt)
	tok, mode := "", ""
	if kind == "task" {
		i := strings.Index(prompt, "这一条做到分支可合并：")
		rest := prompt[i:]
		tok = tokRe.FindString(rest)
		if m := modRe.FindStringSubmatch(rest); m != nil {
			mode = m[1]
		}
	} else if b, err := os.ReadFile(filepath.Join(fdir, "defmode")); err == nil {
		mode = strings.TrimSpace(string(b))
	}
	start := map[string]any{"kind": kind, "tok": tok, "mode": mode, "resume": resume, "takeover": strings.Contains(prompt, "现在由你接手"), "job": os.Getenv("AGENTQ_JOB"), "budget": arg(args, "--max-budget-usd")}
	if codexMode || piMode {
		start["args"] = strings.Join(args, " ")
		start["prompt_head"] = prompt[:min(len(prompt), 400)]
		var login []string
		for _, n := range []string{"CODEX_HOME", "CODEX_API_KEY", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "PI_CODING_AGENT_DIR", "DEEPSEEK_API_KEY"} {
			if os.Getenv(n) != "" {
				login = append(login, n)
			}
		}
		start["login_env"] = login
	}
	rec("start", start)
	var done func()
	if kind == "task" {
		done = checkLive(tok)
		defer done()
		taskMode(mode, tok, resume)
	} else if kind == "review" {
		reviewKind()
	} else if kind == "uireview" {
		uiReviewKind()
	} else if kind == "discuss" {
		discussKind()
	} else {
		otherKind(kind, mode, prompt)
	}
	rec("end", map[string]any{"kind": kind, "tok": tok})
}
`
