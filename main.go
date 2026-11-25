// agentq：说一句需求，做到合进主分支。
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"agentq/internal/claude"
	"agentq/internal/codex"
	"agentq/internal/flow"
	"agentq/internal/lock"
	"agentq/internal/order"
	"agentq/internal/repo"
	"agentq/internal/tell"
	"agentq/internal/todo"
)

const usage = `agentq：说一句需求，做到合进主分支

  agentq do "<需求>"      在当前项目开工（-C <目录> 指定项目，需求写 - 从标准输入读）
  agentq todo             看当前项目的待办；后面写关键字就做那一条，写 all 就能做的全做、做完接着做
  agentq ls               看进度（-a 连做完的一起看）
  agentq log <编号>       看一条的交接单
  agentq stop <编号>      停掉
  agentq retry <编号>     卡住、停了的从那一步接着做
  agentq resume           把被打断的接着做（每 5 分钟自动跑一次）
  agentq brief            发早报：昨天做好的、卡住的、用了多少
  agentq install          装上：派活技能、自动接着做、早上 8 点早报，配了 Telegram 就收消息（uninstall 卸掉）
  agentq version          版本号
`

// version 发布时用 -ldflags "-X main.version=v…" 换掉。
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "agentq："+err.Error())
		os.Exit(1)
	}
}

func store() (*order.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// 路径换成绝对的：后台进程和服务不在现在这个目录。程序只写了名字的去 PATH 里找，不动
	for _, k := range []string{"AGENTQ_HOME", "AGENTQ_PROJECTS", "AGENTQ_TELEGRAM", "AGENTQ_CLAUDE", "AGENTQ_CODEX"} {
		if v := os.Getenv(k); strings.Contains(v, "/") || v != "" && k != "AGENTQ_CLAUDE" && k != "AGENTQ_CODEX" {
			p, err := filepath.Abs(v)
			if err = errors.Join(err, os.Setenv(k, p)); err != nil {
				return nil, err
			}
		}
	}
	root := env("AGENTQ_HOME", filepath.Join(home, ".agentq2"))
	return &order.Store{Root: root}, os.MkdirAll(root, 0o700)
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || strings.HasSuffix(args[0], "-h") || strings.HasSuffix(args[0], "help") {
		fmt.Fprint(out, usage)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(out, version)
		return nil
	}
	s, err := store()
	if err != nil {
		return err
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "do":
		return do(s, args, out)
	case "todo":
		return todos(s, args, out)
	case "ls":
		list(s, len(args) > 0 && args[0] == "-a", out)
		return nil
	case "log", "stop", "retry", "_run":
		if len(args) != 1 {
			return errors.New("要写编号，agentq ls 看编号")
		}
		o, err := s.Load(args[0])
		if err != nil {
			return err
		}
		return map[string]func(*order.Store, *order.Order, io.Writer) error{"log": show, "stop": halt, "retry": retry, "_run": work}[cmd](s, o, out)
	case "resume":
		if err := teller(s).Flush(); err != nil {
			logf("%v", err)
		}
		for _, o := range s.All() {
			if !o.Over() && !o.Running() {
				if err := spawn(s, o, out); err != nil {
					return err
				}
			}
		}
		return nil
	case "listen":
		return listen(s)
	case "brief":
		text := brief(s, time.Now())
		fmt.Fprintln(out, text)
		return errors.Join(os.WriteFile(filepath.Join(s.Root, "brief.md"), []byte(text+"\n"), 0o600), teller(s).Say(text))
	case "install":
		return install(s, out)
	case "uninstall":
		return uninstall(s.Root, out)
	}
	return fmt.Errorf("没有这个命令：%s\n%s", cmd, usage)
}

func do(s *order.Store, args []string, out io.Writer) error {
	top, args, err := where(args)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(string(b))
	}
	if text == "" {
		return errors.New(`写一句需求：agentq do "……"`)
	}
	return begin(s, top, text, "", false, out)
}

// where 是 -C <目录> 的仓库主目录，没写就是当前目录的。
func where(args []string) (string, []string, error) {
	dir := "."
	if len(args) >= 2 && args[0] == "-C" {
		dir, args = args[1], args[2:]
	}
	top, err := repo.Top(dir)
	return top, args, err
}

func begin(s *order.Store, top, text, from string, chain bool, out io.Writer) error {
	if runner().Path == "" {
		return errors.New(noClaude)
	}
	main, err := repo.MainBranch(top)
	if err != nil {
		return err
	}
	o, err := s.New(top, main, text)
	if err != nil {
		return err
	}
	if err := o.Update(func() { o.Todo, o.Chain = from, chain }); err != nil {
		return err
	}
	return spawn(s, o, out)
}

// todos 看待办，或者从待办开工：一条，或者能做的全做。正在做、卡住、等别的做完的跳过。
func todos(s *order.Store, args []string, out io.Writer) error {
	top, args, err := where(args)
	if err != nil {
		return err
	}
	// 几条同时做完都来接，一次只一个在挑
	held, err := lock.Flock(filepath.Join(s.Root, "todo.lock"), true)
	if err != nil {
		return err
	}
	defer held.Close()
	main, err := repo.MainBranch(top)
	if err != nil {
		return err
	}
	text, err := repo.Git(top, "show", main+":"+todo.File) // 只认提交过的：做的时候看到的就是它
	if err != nil {
		return fmt.Errorf("%s 上没有 %s", main, todo.File)
	}
	if d, _ := repo.Git(top, "status", "--porcelain", "--", todo.File); d != "" {
		fmt.Fprintln(out, todo.File+" 有没提交的改动，先按提交过的来")
	}
	items := todo.List(text)
	key, all := strings.Join(args, " "), len(args) == 1 && args[0] == "all"
	doing := map[string]string{}
	for _, o := range s.All() {
		// 点名的可以重做；不点名的，做过、停过的不自己再来一遍
		if o.Dir == top && o.Todo != "" && (!o.Over() || o.State == order.Stuck || all || key == "") {
			doing[o.Todo] = o.State + "（" + o.ID + "）"
		}
	}
	n := 0
	for _, it := range items {
		state := doing[it.Title]
		switch {
		case state != "":
		case it.Stuck && (all || key == ""): // 点名的照做：用户看过了
			state = "卡住"
		case it.Wait != "":
			state = "等「" + it.Wait + "」"
		}
		if key == "" {
			fmt.Fprintf(out, "%s  %s\n", it.Title, state)
		}
		if key == "" || state != "" || !all && !strings.Contains(it.Title, key) {
			continue
		}
		if err := begin(s, top, it.Text, it.Title, all, out); err != nil {
			return err
		}
		if n++; !all {
			return nil
		}
	}
	if key != "" && n == 0 {
		fmt.Fprintln(out, "待办里没有现在能开工的")
	}
	return nil
}

// retry 让卡住、停了的从那一步接着做。
func retry(s *order.Store, o *order.Order, out io.Writer) error {
	held, err := lock.Flock(filepath.Join(s.Root, "todo.lock"), true) // 和挑待办的错开
	if err != nil {
		return err
	}
	defer held.Close()
	if o.Running() {
		return errors.New("这条正在做")
	}
	for _, x := range s.All() {
		if o.Todo != "" && x.ID != o.ID && x.Dir == o.Dir && x.Todo == o.Todo && x.State != order.Stopped {
			return fmt.Errorf("同一条待办已经有 %s（%s）", x.ID, x.State)
		}
	}
	if o.State == order.Done {
		return errors.New("这条已经做完了")
	}
	if o.Plan != nil && o.Plan.Stop != "" { // 用户看过了，重新出方案
		o.Plan = nil
	}
	if err := o.Update(func() { o.State, o.Why, o.Ended = order.Building, "", time.Time{} }); err != nil {
		return err
	}
	return spawn(s, o, out)
}

// spawn 起一个独立进程把这条做到底。
func spawn(s *order.Store, o *order.Order, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(o.WorkDir(), "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(self, "_run", o.ID)
	cmd.Dir, cmd.Stdout, cmd.Stderr = s.Root, log, log
	cmd.Env = append(claude.Own(), "AGENTQ_HOME="+s.Root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	fmt.Fprintf(out, "%s 开工：%s「%s」\n", o.ID, o.Project, claude.Short(o.Text, 50))
	return nil
}

// work 是那个独立进程。
func work(s *order.Store, o *order.Order, _ io.Writer) error {
	lock, err := o.Lock()
	if err != nil || lock == nil { // 没拿到：另一个进程在做
		return err
	}
	defer lock.Close()
	if o, err = s.Load(o.ID); err != nil || o.Over() {
		return err
	}
	reap(o)
	if err := o.Update(func() { o.PID = os.Getpid() }); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	t := teller(s)
	f := &flow.Flow{
		Claude: runner(), Lead: env("AGENTQ_LEAD", "opus"), Code: env("AGENTQ_CODE", "sonnet"), Last: env("AGENTQ_LAST", "fable"),
		Codex: codex.Auditor{Path: env("AGENTQ_CODEX", claude.Find("codex")), Model: env("AGENTQ_AUDIT", "gpt-6.2-astra"), Effort: "xhigh", Timeout: 3 * time.Hour, Env: claude.Env()},
		Log:   logf,
		Say: func(text string) {
			logf("消息：%s", strings.ReplaceAll(text, "\n", " ／ "))
			if err := t.Say(text); err != nil {
				logf("%v", err)
			}
		},
		Next: func(o *order.Order) {
			if err := todos(s, []string{"-C", o.Dir, "all"}, os.Stdout); err != nil {
				logf("接着做待办：%v", err)
			}
		},
		Locks: filepath.Join(s.Root, "locks"),
	}
	return f.Run(ctx, o)
}

func logf(format string, a ...any) {
	fmt.Printf("%s %s\n", time.Now().Format("01-02 15:04:05"), fmt.Sprintf(format, a...))
}

const noClaude = "没找到 claude：先装 Claude Code 并登录"

func runner() claude.Runner {
	return claude.Runner{Path: env("AGENTQ_CLAUDE", claude.Find("claude")), Timeout: 4 * time.Hour}
}

func teller(s *order.Store) tell.Teller {
	home, _ := os.UserHomeDir()
	return tell.Teller{Out: filepath.Join(s.Root, "outbox"), File: env("AGENTQ_TELEGRAM", telegramFile(s.Root, home)), API: env("AGENTQ_TELEGRAM_API", "https://api.telegram.org")}
}

// telegramFile 是 Telegram 密钥的默认位置：在数据目录里，没有就用旧版写好的。数据目录换了就不看旧的，试用不会发到真的 Telegram。
func telegramFile(root, home string) string {
	file, old := filepath.Join(root, "telegram.env"), filepath.Join(home, ".agentq", "secrets", "telegram.env")
	if _, err := os.Stat(file); err == nil || filepath.Clean(root) != filepath.Join(home, ".agentq2") {
		return file
	}
	if _, err := os.Stat(old); err != nil {
		return file
	}
	return old
}

// listen 一直收消息，每条起一个会话：是需求就开工，问的就答。
func listen(s *order.Store) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	root := env("AGENTQ_PROJECTS", filepath.Join(home, "projects"))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	t, r := teller(s), runner()
	return t.Listen(ctx, filepath.Join(s.Root, "inbox"), logf, func(file, text string, again bool) {
		logf("收到：%s", claude.Short(text, 100))
		if again {
			text += "\n（这条上次可能处理过一半：先用 ls -a 看有没有已经开工，有就不再派，只告诉用户。）"
		}
		go func() {
			out := filepath.Join(s.Root, "chat-"+time.Now().Format("0102-150405.000"))
			rep, err := claude.Reply{}, os.MkdirAll(root, 0o755) // 新用户还没有这个目录，会话起不来
			if err == nil {
				rep, err = r.Run(ctx, claude.Req{Dir: root, Prompt: flow.ChatPrompt(self, text), Effort: "medium", Model: env("AGENTQ_CODE", "sonnet"), Name: "agentq 对话", Out: out, Log: logf})
			}
			if ctx.Err() != nil { // 被停了：消息留着，下次接着处理
				return
			}
			answer := rep.Text
			if err != nil {
				answer = "🔴 这条没处理成：" + err.Error()
			}
			if err := errors.Join(t.Say(answer), os.Remove(file)); err != nil {
				logf("%v", err)
			}
		}()
	})
}

func env(k, def string) string { return cmp.Or(os.Getenv(k), def) }

func list(s *order.Store, all bool, out io.Writer) {
	n := 0
	for _, o := range s.All() {
		if !all && (o.State == order.Done || o.State == order.Stopped) && time.Since(o.Ended) > 24*time.Hour {
			continue
		}
		n++
		end := cmp.Or(o.Ended, time.Now())
		state := o.State
		if !o.Over() && !o.Running() {
			state += "（断了，等接着做）"
		}
		fmt.Fprintf(out, "%s  %s  %s「%s」 %s  %.1fM", o.ID, state, o.Project, flow.Title(o), end.Sub(o.Started).Round(time.Minute), float64(o.Tokens)/1e6)
		if o.Why != "" {
			fmt.Fprintf(out, "\n    %s", claude.Short(o.Why, 160))
		}
		fmt.Fprintln(out)
	}
	if n == 0 {
		fmt.Fprintln(out, "没有在做的。agentq do \"<需求>\" 开工")
	}
}

// brief 是早报：过去一天做好的、卡住的、还在做的，各项目用了多少。
func brief(s *order.Store, now time.Time) string {
	parts := map[string]string{}
	used := map[string]float64{}
	var total float64
	for _, o := range s.All() {
		for _, u := range o.Used {
			if now.Sub(u.At) < 24*time.Hour {
				used[o.Project] += float64(u.Tokens) / 1e6
				total += float64(u.Tokens) / 1e6
			}
		}
		if o.Over() && now.Sub(o.Ended) > 24*time.Hour && o.State != order.Stuck {
			continue
		}
		line := fmt.Sprintf("\n· %s「%s」", o.Project, flow.Title(o))
		switch o.State {
		case order.Done:
			parts["🟢 做好了"] += line + " " + o.Ended.Sub(o.Started).Round(time.Minute).String()
		case order.Stuck:
			parts["🔴 卡住等你"] += line + " " + claude.Short(o.Why, 80) + "（" + o.ID + "）"
		case order.Stopped:
		default:
			parts["🔵 在做"] += line + " " + o.State
		}
	}
	text := "☀️ 早报 " + now.Format("01-02")
	for _, k := range []string{"🔴 卡住等你", "🟢 做好了", "🔵 在做"} {
		if parts[k] != "" {
			text += "\n\n" + k + parts[k]
		}
	}
	if len(used) == 0 && len(parts) == 0 {
		return text + "\n\n昨天没有派活"
	}
	text += fmt.Sprintf("\n\n用了 %.1fM token：", total)
	for _, p := range slices.Sorted(maps.Keys(used)) {
		text += fmt.Sprintf(" %s %.1fM", p, used[p])
	}
	return text
}

// show 打印交接单：谁接到什么、交回什么、在哪被打回。
func show(_ *order.Store, o *order.Order, out io.Writer) error {
	fmt.Fprintf(out, "%s  %s  %s\n需求：%s\n", o.ID, o.State, o.Dir, o.Text)
	if o.Why != "" {
		fmt.Fprintln(out, o.Why)
	}
	b, err := os.ReadFile(filepath.Join(o.WorkDir(), "HANDOFF.md"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "%s完整输出在 %s\n", b, o.WorkDir())
	return nil
}

// reap 杀掉这条需求上一个进程留下的会话和测试：它被杀时它们可能还活着，接着做会变成两份。
func reap(o *order.Order) {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pgid=,command=").Output()
	if err != nil {
		logf("查不了留下的进程：%v", err)
		return
	}
	trees := regexp.QuoteMeta(filepath.Join(o.WorkDir(), "trees"))
	check, talk := regexp.MustCompile(trees+`/\S*\.agentq/verify`), regexp.MustCompile(`--name agentq `+o.ID+` |exec -C `+trees)
	for _, l := range strings.Split(string(out), "\n") {
		var pgid int
		if n, _ := fmt.Sscan(l, &pgid); n == 1 && pgid > 1 && pgid != syscall.Getpgrp() && (check.MatchString(l) || pgid == o.PID && talk.MatchString(l)) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
}

// halt 停掉：先让进程自己收，10 秒不走就连会话一起杀。
func halt(s *order.Store, o *order.Order, out io.Writer) error {
	if o.Running() && o.PID > 1 {
		_ = syscall.Kill(-o.PID, syscall.SIGTERM)
		for i := 0; i < 100 && o.Running(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if o.Running() {
			_ = syscall.Kill(-o.PID, syscall.SIGKILL)
		}
	}
	reap(o)
	o, err := s.Load(o.ID)
	if err != nil {
		return err
	}
	if o.Over() {
		fmt.Fprintf(out, "%s 已经是「%s」\n", o.ID, o.State)
		return nil
	}
	fmt.Fprintf(out, "%s 停了，分支和 worktree 留着，agentq retry %s 接着做\n", o.ID, o.ID)
	return o.Update(func() { o.State, o.Ended = order.Stopped, time.Now() })
}
