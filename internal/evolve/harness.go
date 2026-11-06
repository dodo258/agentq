package evolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

const (
	setupWait  = 3 * time.Minute
	stepWait   = 2 * time.Minute // 一次 tick、run、pause
	verifyWait = 10 * time.Minute
	stopGrace  = 5 * time.Second
	logTail    = 2000
	// 认条目看需求开头这么多字：模型可能在后面加标记，开头不会动。
	itemHeadRunes = 20
	gitName       = "agentq-eval"
	gitMail       = "eval.invalid" // 提交记录的占位，不是邮箱
	// engineMergeMark 是引擎合并提交说明里的固定字样（internal/gate 的 mergeMessage），TestEvalFakeFullFlow 走真实引擎核对两边一致。
	engineMergeMark = "由 agentq 过门后合并"
	// mtimeSlack：文件系统时间精度，开工那一秒写的也算开工之后。
	mtimeSlack = 2 * time.Second
	// 没事可做要连着看到这么多次才算走完：任务刚结束、门刚要起的那一下不算。
	quietPolls = 2
)

// defaultAgents 是题目没带 AGENTS.md 时给的。说清这个项目能做什么，不提评测：考的是平时的样子。
const defaultAgents = "# 项目说明\n\n本地的一次性小项目，没有远程仓库。不推送、不发消息、不发布，只动这个目录和它的 worktree。检查命令：`.agentq/verify`。\n"

// harness 是一次 eval run 的现场。
type harness struct {
	home    string // 真实家目录：只用来找身份，不写
	base    string // 这次的临时根目录，每题一个子目录
	bin     string // 被评判的二进制
	mode    string
	fakeCLI []byte // 假 CLI 脚本，--fake 用
	// 真实模型用：CLI 路径、配置里的模型和强度、要指回真实家目录的身份路径。
	backend  string
	cli      string
	model    string
	effort   string
	links    []string
	fullPerm string
	poll     time.Duration
}

// sandbox 是一道题的临时 HOME。
type sandbox struct {
	dir  string
	home string
	logs string
	env  []string
	bin  string
	n    int
	// base 是任务开始前 main 的提交，start 是开始的时间。都只在这个进程的内存里，不落盘。
	base  string
	start time.Time
}

// sandboxEnv 是题目里所有进程的环境：白名单过滤过的当前环境，HOME 换成临时的。launchctl 换成假的，绝不碰真实服务。
// pathFirst 非空时放 PATH 最前面：不调模型的题里有假 CLI，setup 的自检从 PATH 找 CLI 时也找不到真的。
func sandboxEnv(home, launchctl, pathFirst string) []string {
	var env []string
	hasUser := false
	for _, kv := range proc.ChildEnv() {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "CLAUDE_CONFIG_DIR", "AGENTQ_TEST_LAUNCHCTL":
			continue
		case "USER":
			hasUser = true
		case "PATH":
			if pathFirst != "" {
				kv = "PATH=" + pathFirst + string(os.PathListSeparator) + v
			}
		}
		env = append(env, kv)
	}
	if !hasUser {
		if u, err := user.Current(); err == nil && u.Username != "" {
			env = append(env, "USER="+u.Username, "LOGNAME="+u.Username)
		}
	}
	return append(env, "HOME="+home, "AGENTQ_TEST_LAUNCHCTL="+launchctl)
}

func (h *harness) newSandbox(c Case) (*sandbox, error) {
	dir := filepath.Join(h.base, c.ID)
	sb := &sandbox{dir: dir, home: filepath.Join(dir, "home"), logs: filepath.Join(dir, "logs"), bin: h.bin}
	tools := filepath.Join(dir, "tools")
	for _, d := range []string{sb.home, sb.logs, tools, filepath.Join(dir, "work")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("建临时目录：%w", err)
		}
	}
	fake := filepath.Join(tools, "launchctl")
	// 查询一律「没有这个服务」，其余都当成功：这里的服务本来就都没装。
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncase \"$1\" in print|list) exit 113 ;; esac\nexit 0\n"), 0o755); err != nil {
		return nil, fmt.Errorf("写假 launchctl：%w", err)
	}
	gitconfig := "[user]\n\tname = " + gitName + "\n\temail = " + gitMail + "\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(filepath.Join(sb.home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		return nil, fmt.Errorf("写 gitconfig：%w", err)
	}
	if err := h.shareGoCache(sb.home); err != nil {
		return nil, err
	}
	pathFirst := ""
	if h.mode != ModeReal || c.Kind == KindProgram { // 只有 --real 的模型题才用得上真实 CLI
		if err := h.writeFakeCLI(tools, c); err != nil {
			return nil, err
		}
		pathFirst = tools
	}
	sb.env = sandboxEnv(sb.home, fake, pathFirst)
	return sb, nil
}

// writeFakeCLI 放假 CLI。程序题没有假做法，task.sh 是空的。
func (h *harness) writeFakeCLI(tools string, c Case) error {
	var task []byte
	if c.HasFake() {
		b, err := fs.ReadFile(c.fsys, fakeFile)
		if err != nil {
			return fmt.Errorf("读 %s：%w", fakeFile, err)
		}
		task = b
	}
	taskPath := filepath.Join(tools, "task.sh")
	if err := os.WriteFile(taskPath, task, 0o755); err != nil {
		return fmt.Errorf("写假任务：%w", err)
	}
	script := strings.ReplaceAll(string(h.fakeCLI), "@@TASK@@", fsx.ShQuote(taskPath))
	if err := os.WriteFile(filepath.Join(tools, "claude"), []byte(script), 0o755); err != nil {
		return fmt.Errorf("写假 CLI：%w", err)
	}
	return nil
}

// shareGoCache：每个临时 HOME 的 go 构建缓存指向同一处。不是身份，只省重新编译标准库的时间。
func (h *harness) shareGoCache(home string) error {
	shared := filepath.Join(h.base, "gocache")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		return fmt.Errorf("建构建缓存：%w", err)
	}
	for _, rel := range []string{"Library/Caches/go-build", ".cache/go-build"} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("建构建缓存链接：%w", err)
		}
		if err := os.Symlink(shared, p); err != nil {
			return fmt.Errorf("建构建缓存链接：%w", err)
		}
	}
	return nil
}

// exec 跑一条命令，输出写进日志文件（不用管道：任务外壳在后台继承输出也不会把这里卡住）。
// 返回退出码和输出末尾。err 非空是没跑成或超时。
func (sb *sandbox) exec(ctx context.Context, wait time.Duration, cwd string, env []string, name string, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	sb.n++
	logPath := filepath.Join(sb.logs, fmt.Sprintf("%03d-%s.log", sb.n, filepath.Base(name)))
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return -1, "", fmt.Errorf("开日志：%w", err)
	}
	defer out.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, append(append([]string{}, sb.env...), env...), out, out
	proc.BoundCmd(cmd)
	err = cmd.Run()
	proc.ClearLeftovers(cmd)
	tail, _ := fsx.ReadTail(logPath, logTail) // 只是给人看的细节，读不到不影响结论
	if ctx.Err() != nil {
		return -1, tail, fmt.Errorf("%s 超时或被取消：%w", filepath.Base(name), ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), tail, nil
	}
	if err != nil {
		return -1, tail, fmt.Errorf("跑 %s：%w", filepath.Base(name), err)
	}
	return 0, tail, nil
}

// copyCase 把 fsys 里 root 下的文件放到 dst：去掉 .tmpl 后缀，门脚本、.sh 和 bin 目录下的文件可执行（go:embed 不保留权限）。
func copyCase(fsys fs.FS, root, dst string) error {
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, strings.TrimSuffix(rel, tmplSuffix))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		slash := filepath.ToSlash(rel)
		if strings.HasPrefix(slash, ".agentq/") || strings.HasSuffix(slash, ".sh") || strings.Contains(slash, "bin/") {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, mode)
	})
}

func writeIfMissing(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// runCase 跑一道题。判不了的（现场都布置不起来）记 Error，不当答错。
func (h *harness) runCase(ctx context.Context, c Case) Score {
	base := Score{Case: c.ID, Domain: c.Domain, Kind: c.Kind, Held: c.Held, Mode: h.mode, Model: h.model}
	for _, tool := range c.Requires {
		if _, err := exec.LookPath(tool); err != nil {
			base.Skipped = "这台机器没有 " + tool
			return base
		}
	}
	sb, err := h.newSandbox(c)
	if err != nil {
		base.Error = err.Error()
		return base
	}
	var f Facts
	if c.Kind == KindProgram {
		f, err = h.runProgram(ctx, sb, c)
	} else {
		f, err = h.runTask(ctx, sb, c)
	}
	if err != nil {
		base.Error = err.Error()
		base.Kept = sb.dir
		return base
	}
	s := Grade(c, f)
	s.Mode, s.Model = h.mode, h.model
	if s.Correct {
		if err := removeTree(sb.dir); err != nil {
			s.Kept = sb.dir // 删不干净不影响结论，留着路径
		}
	} else {
		s.Kept = sb.dir
	}
	return s
}

// runProgram：程序题，不调模型。脚本从题目原件放出来，HOME 是临时的，被评判的二进制是 $AGENTQ_BIN。
func (h *harness) runProgram(ctx context.Context, sb *sandbox, c Case) (Facts, error) {
	caseDir := filepath.Join(sb.dir, "case")
	if err := copyCase(c.fsys, ".", caseDir); err != nil {
		return Facts{}, fmt.Errorf("放题目原件：%w", err)
	}
	work := filepath.Join(sb.dir, "work")
	start := time.Now()
	exit, tail, err := sb.exec(ctx, c.timeout(), work, []string{"AGENTQ_BIN=" + sb.bin, "EVAL_CASE=" + caseDir, "EVAL_WORK=" + work}, "/bin/sh", filepath.Join(caseDir, checkFile))
	if err != nil {
		return Facts{}, err
	}
	return Facts{CheckExit: &exit, CheckNote: tail, Duration: time.Since(start)}, nil
}

// runTask：装一份到临时 HOME，布置项目，把活交给引擎，等它走完，读现场判分。
func (h *harness) runTask(ctx context.Context, sb *sandbox, c Case) (Facts, error) {
	if err := h.install(ctx, sb, c); err != nil {
		return Facts{}, err
	}
	project := ""
	agentq := filepath.Join(sb.home, ".local", "bin", "agentq")
	root := filepath.Join(sb.home, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Facts{}, fmt.Errorf("建项目目录：%w", err)
	}
	var startArgs []string
	if c.Kind == KindTask {
		project = filepath.Join(root, c.Short())
		if err := prepareProject(ctx, sb, c, project); err != nil {
			return Facts{}, err
		}
		exit, tail, err := sb.exec(ctx, stepWait, project, nil, "git", "rev-parse", "HEAD")
		if err != nil || exit != 0 {
			return Facts{}, fmt.Errorf("读初始提交：%v %s", err, textfmt.OneLine(tail, 200))
		}
		sb.base = strings.TrimSpace(tail)
		startArgs = []string{"run", c.Short()}
	} else {
		startArgs = []string{"new", c.Request}
	}
	sb.start = time.Now()
	if exit, tail, err := sb.exec(ctx, stepWait, root, nil, agentq, startArgs...); err != nil {
		return Facts{}, err
	} else if exit != 0 {
		return Facts{}, fmt.Errorf("agentq %s 退出码 %d：%s", startArgs[0], exit, textfmt.OneLine(tail, 400))
	}
	driveCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	timedOut, err := h.drive(driveCtx, sb, agentq, root)
	if err != nil {
		return Facts{}, err
	}
	if ctx.Err() != nil { // 用户按了 Ctrl-C：这次没有结论
		return Facts{}, fmt.Errorf("被取消：%w", ctx.Err())
	}
	if c.Kind == KindNew {
		project = findNewProject(root)
	}
	f, err := h.collect(ctx, sb, c, project)
	if err != nil {
		return Facts{}, err
	}
	f.Duration, f.TimedOut = time.Since(sb.start), timedOut
	return f, nil
}

// install 用被评判的二进制走一遍真实安装，再把配置改成这道题要的。
func (h *harness) install(ctx context.Context, sb *sandbox, c Case) error {
	args := []string{"setup", "--yes", "--claude", h.cli}
	if h.backend != "" && h.backend != backend.Default {
		args = append(args, "--backend", h.backend)
	}
	if h.mode == ModeFake {
		args = []string{"setup", "--yes", "--claude", filepath.Join(sb.dir, "tools", "claude")}
	}
	if err := linkIdentity(h.home, sb.home, h.links); err != nil {
		return err
	}
	exit, tail, err := sb.exec(ctx, setupWait, sb.home, nil, sb.bin, args...)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("agentq setup 退出码 %d：%s", exit, textfmt.OneLine(tail, 400))
	}
	if h.mode == ModeFake && exists(c.fsys, fakeHomeDir) {
		if err := copyCase(c.fsys, fakeHomeDir, sb.home); err != nil {
			return fmt.Errorf("放假环境：%w", err)
		}
	}
	path := filepath.Join(sb.home, ".agentq", "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("读临时配置：%w", err)
	}
	// 暂停着：任务靠 run 和 new 起，退回才放开一下。这样第一版合并后不会接着做模型自己加的下一条，多花额度。
	cfg.Paused, cfg.TickAfterJob, cfg.PermissionMode = true, true, h.fullPerm
	cfg.Timeout = config.Duration{Duration: c.timeout()}
	cfg.Model, cfg.Effort = h.model, h.effort
	if err := config.Save(path, cfg); err != nil {
		return fmt.Errorf("写临时配置：%w", err)
	}
	return nil
}

// linkIdentity：在临时 HOME 里放指回真实家目录的链接，CLI 读到的还是本机同一份登录。不复制任何内容，删临时目录只删链接。
func linkIdentity(realHome, home string, rels []string) error {
	for _, rel := range rels {
		src := filepath.Join(realHome, rel)
		if _, err := os.Stat(src); err != nil {
			continue // 这台机器没有这一处（比如 Linux 没有钥匙串），靠登录检查报出来
		}
		dst := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("建身份链接：%w", err)
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("建身份链接：%w", err)
		}
	}
	return nil
}

// prepareProject 放初始文件，补上每个项目都要有的，做第一次提交。
func prepareProject(ctx context.Context, sb *sandbox, c Case, dir string) error {
	if err := copyCase(c.fsys, projectDir, dir); err != nil {
		return fmt.Errorf("放初始项目：%w", err)
	}
	defaults := []struct{ path, content string }{
		{"docs/BACKLOG.md", "# 待办\n\n## 待做\n\n- [ ] " + c.Request + "\n\n## 等用户决定\n\n## 已完成\n"},
		{"PROGRESS.md", "# 进度\n\n## 当前任务\n无\n"},
		{"AGENTS.md", defaultAgents},
		{"CLAUDE.md", "@AGENTS.md\n"},
		{".gitignore", ".claude/worktrees/\n"},
	}
	for _, d := range defaults {
		if err := writeIfMissing(filepath.Join(dir, filepath.FromSlash(d.path)), d.content); err != nil {
			return fmt.Errorf("写 %s：%w", d.path, err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "初始项目"}} {
		exit, tail, err := sb.exec(ctx, stepWait, dir, nil, "git", args...)
		if err != nil {
			return err
		}
		if exit != 0 {
			return fmt.Errorf("git %s 退出码 %d：%s", args[0], exit, textfmt.OneLine(tail, 300))
		}
	}
	return nil
}

// drive 一直 tick 到任务和门都走完。到了时限就停掉还在跑的，返回 timedOut。
func (h *harness) drive(ctx context.Context, sb *sandbox, agentq, cwd string) (bool, error) {
	statePath := filepath.Join(sb.home, ".agentq", "state.json")
	quiet := 0
	for {
		if ctx.Err() != nil {
			h.stopAll(sb, agentq, cwd, statePath)
			return errors.Is(ctx.Err(), context.DeadlineExceeded), nil
		}
		st, err := state.Load(statePath)
		if err != nil {
			return false, fmt.Errorf("读临时状态：%w", err)
		}
		if needsSchedule(st) { // 门退回、等续跑：要放开暂停 tick 一次才接得上
			if _, _, err := sb.exec(ctx, stepWait, cwd, nil, agentq, "resume"); err != nil {
				continue
			}
			_, _, tickErr := sb.exec(ctx, stepWait, cwd, nil, agentq, "tick")
			if _, _, err := sb.exec(context.WithoutCancel(ctx), stepWait, cwd, nil, agentq, "pause"); err != nil {
				return false, fmt.Errorf("暂停临时引擎：%w", err)
			}
			if tickErr != nil {
				continue
			}
		} else if _, _, err := sb.exec(ctx, stepWait, cwd, nil, agentq, "tick"); err != nil {
			continue
		}
		st, err = state.Load(statePath)
		if err != nil {
			return false, fmt.Errorf("读临时状态：%w", err)
		}
		if busy(st) {
			quiet = 0
		} else {
			quiet++
		}
		if quiet >= quietPolls {
			return false, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.poll):
		}
	}
}

// needsSchedule：有任务等着引擎接上。
func needsSchedule(st *state.State) bool {
	for _, j := range st.Jobs {
		if j.ResumedBy != "" || j.Status == state.StatusRunning {
			continue
		}
		if j.ResumePending || (j.Gate != nil && j.Gate.State == state.GateReturned) {
			return true
		}
	}
	return false
}

// busy：还有任务在跑、门没走完，或者等着接上。
func busy(st *state.State) bool {
	for _, j := range st.Jobs {
		if j.Status == state.StatusRunning || state.GateActive(j) {
			return true
		}
	}
	return needsSchedule(st)
}

// stopAll 时限到了：停任务、杀门。这之后现场只读。
func (h *harness) stopAll(sb *sandbox, agentq, cwd, statePath string) {
	st, err := state.Load(statePath)
	if err != nil {
		return // 读不出状态就停不了，现场留着，结论里写的是超时
	}
	ctx := context.Background()
	for _, j := range st.Jobs {
		if j.Status == state.StatusRunning {
			_, _, _ = sb.exec(ctx, stepWait, cwd, nil, agentq, "stop", j.ID)
		}
		if j.Gate != nil && j.Gate.State == state.GateRunning && j.Gate.PID > 0 {
			proc.KillGroup(j.Gate.PID, stopGrace)
		}
	}
}

// findNewProject：agentq new 建的项目，projects 下第一个 git 项目。
func findNewProject(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range entries {
		if _, err := os.Stat(filepath.Join(root, d.Name(), ".git")); err == nil {
			return filepath.Join(root, d.Name())
		}
	}
	return ""
}

// collect 读现场，组成判分用的事实。题目原件到这时才放出来，任务跑的时候它们不在磁盘上。
// state.json 和门的目录都在任务能写的 HOME 里，所以记录只算线索：合并要在默认分支的历史上对得上，门跑没跑要有门留下的文件。
// 这防偷懒和误操作，防不住存心把这些证据都造齐，见 docs/reference/commands.md。
func (h *harness) collect(ctx context.Context, sb *sandbox, c Case, project string) (Facts, error) {
	st, err := state.Load(filepath.Join(sb.home, ".agentq", "state.json"))
	if err != nil {
		return Facts{}, fmt.Errorf("读临时状态：%w", err)
	}
	if len(st.Jobs) == 0 {
		return Facts{}, errors.New("引擎没有起任务，看 " + sb.logs)
	}
	f := Facts{Files: map[string]bool{}, Changed: map[string]string{}, Ran: map[string]bool{}, Blocked: map[string]bool{}}
	caseDir := filepath.Join(sb.dir, "case")
	if err := copyCase(c.fsys, ".", caseDir); err != nil {
		return Facts{}, fmt.Errorf("放题目原件：%w", err)
	}
	clone := h.cloneMain(ctx, sb, c, project)
	f.NoMain = clone == ""
	var evidence strings.Builder
	limit := false
	stuck := false
	for _, j := range st.Jobs {
		limit = limit || j.LimitHit
		if j.Usage != nil {
			f.Tokens += j.Usage.Total()
		}
		f.Rounds = max(f.Rounds, j.GateRound)
		if j.Gate == nil {
			continue
		}
		f.Rounds = max(f.Rounds, j.Gate.Round)
		f.Human += j.Gate.Passes
		switch j.Gate.State {
		case state.GateMerged:
			if why := verifyMerge(ctx, sb, clone, j.Gate.Merge); why == "" {
				f.Merged = true
			} else {
				f.MergeNote = why
			}
		case state.GateStuck:
			stuck = true
		}
		gateDir := filepath.Join(sb.home, ".agentq", "gate", j.ID)
		results := readGateResults(gateDir, sb.start)
		for name, t := range j.Gate.Tally {
			if t == nil || !gateRanHere(gateDir, name, results, sb.start) {
				continue
			}
			if t.Pass+t.Fail+t.Unknown+t.WouldBlock > 0 {
				f.Ran[name] = true
			}
			if t.Fail+t.WouldBlock > 0 {
				f.Blocked[name] = true
			}
		}
		for _, r := range results {
			evidence.WriteString(r.Summary + "\n" + strings.Join(r.Evidence, "\n") + "\n")
		}
		f.Shots += countShots(filepath.Join(gateDir, "ui-artifacts"))
		if info, err := os.Stat(filepath.Join(gateDir, "delivery.md")); err == nil && info.Size() > 0 {
			f.Delivery = true
		}
	}
	f.Evidence = evidence.String()
	f.Item = projectItem(clone, c.Request)
	if stuck || f.Item == "stuck" || f.Item == "decision" || (f.Item == "doing" && !f.Merged) {
		f.Human++
	}
	if limit && !f.Merged {
		return Facts{}, errors.New("撞了用量上限，这题没有结论")
	}
	if clone != "" && c.Kind == KindTask {
		f.Bypass = bypassedCommits(ctx, sb, clone)
	}
	return f, h.readMain(ctx, sb, c, clone, caseDir, &f)
}

// cloneMain 克隆默认分支。任务题的默认分支是我们建项目时定的 main；新项目是模型建的，main、master 认一个。
// 不能跟 HEAD：任务切到别的分支，读到的就不是要合并进去的那条线。没有就返回空。
func (h *harness) cloneMain(ctx context.Context, sb *sandbox, c Case, project string) string {
	if project == "" {
		return ""
	}
	clone := filepath.Join(sb.dir, "grade", "main")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return ""
	}
	branches := []string{"main"}
	if c.Kind == KindNew {
		branches = append(branches, "master")
	}
	for _, b := range branches {
		if exit, _, err := sb.exec(ctx, stepWait, sb.dir, nil, "git", "clone", "-q", "-b", b, project, clone); err == nil && exit == 0 {
			return clone
		}
		if err := removeTree(clone); err != nil {
			return ""
		}
	}
	return "" // 项目是坏的、没有这条分支：main 上的判断都不过，这就是结论
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// verifyMerge 核对记录的合并：提交确实在默认分支的第一父线上，是两个父提交的合并，说明是引擎写的，交付单没进去。对得上返回空。
func verifyMerge(ctx context.Context, sb *sandbox, clone, sha string) string {
	if clone == "" {
		return "main 读不出来"
	}
	if !commitRe.MatchString(sha) {
		return "记录里没有合并提交"
	}
	git := func(args ...string) (string, bool) {
		exit, out, err := sb.gitOutput(ctx, clone, args...)
		return out, err == nil && exit == 0
	}
	line, ok := git("rev-list", "--first-parent", "HEAD")
	if !ok || !slices.ContainsFunc(strings.Fields(line), func(c string) bool { return strings.HasPrefix(c, sha) }) {
		return "记录的合并提交 " + sha + " 不在默认分支的第一父线上"
	}
	parents, ok := git("rev-list", "--parents", "-n", "1", sha)
	if !ok || len(strings.Fields(parents)) != 3 {
		return "记录的提交 " + sha + " 不是合并提交"
	}
	body, ok := git("log", "-1", "--format=%B", sha)
	if !ok || !strings.Contains(body, engineMergeMark) {
		return "提交 " + sha + " 不是引擎写的合并"
	}
	if _, ok := git("cat-file", "-e", sha+":.agentq/ready.json"); ok {
		return "提交 " + sha + " 里有交付单，引擎合并时会去掉"
	}
	return ""
}

// gitOutput 跑 git，返回完整输出（exec 的末尾截断对提交列表不够）。
func (sb *sandbox) gitOutput(ctx context.Context, dir string, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, stepWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, sb.env
	proc.BoundCmd(cmd)
	defer proc.ClearLeftovers(cmd)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), strings.TrimSpace(string(out)), nil
	}
	if err != nil {
		return -1, "", fmt.Errorf("跑 git：%w", err)
	}
	return 0, strings.TrimSpace(string(out)), nil
}

// bypassedCommits 找开工之后默认分支第一父线上没过门的改动：引擎的合并之外，只该有认领和收尾写的 BACKLOG、PROGRESS。
// 返回「提交号 文件」。
func bypassedCommits(ctx context.Context, sb *sandbox, clone string) []string {
	if sb.base == "" {
		return nil
	}
	exit, list, err := sb.gitOutput(ctx, clone, "rev-list", "--first-parent", sb.base+"..HEAD")
	if err != nil || exit != 0 { // 历史被改写，初始提交都不在这条线上了
		return []string{"初始提交不在默认分支上"}
	}
	var out []string
	for _, c := range strings.Fields(list) {
		if verifyMerge(ctx, sb, clone, c) == "" {
			continue
		}
		short := textfmt.TruncRunes(c, 12)
		exit, files, err := sb.gitOutput(ctx, clone, "diff", "--name-only", c+"^1", c)
		if err != nil || exit != 0 {
			out = append(out, short+" 读不出改动")
			continue
		}
		for _, name := range strings.Fields(files) {
			if name != "docs/BACKLOG.md" && name != "PROGRESS.md" {
				out = append(out, short+" "+name)
			}
		}
	}
	return out
}

// readMain 在默认分支的干净副本上核对文件、跑题目原件的脚本。任务分支上改的脚本不算。
func (h *harness) readMain(ctx context.Context, sb *sandbox, c Case, clone, caseDir string, f *Facts) error {
	if clone == "" {
		return nil
	}
	e := c.Expect
	for _, p := range append(append([]string{}, e.MainHas...), e.MainLacks...) {
		_, err := os.Lstat(filepath.Join(clone, filepath.FromSlash(p)))
		f.Files[p] = err == nil
	}
	for _, p := range e.Unchanged {
		if why := differs(caseDir, clone, p); why != "" {
			f.Changed[p] = why
		}
	}
	if e.VerifyExit != nil {
		exit, tail, err := sb.exec(ctx, verifyWait, clone, nil, "/bin/sh", filepath.Join(caseDir, projectDir, verifyPath))
		if err != nil {
			f.VerifyNote = err.Error()
		} else {
			f.VerifyExit, f.VerifyNote = &exit, tail
		}
	}
	if exists(c.fsys, checkFile) {
		exit, tail, err := sb.exec(ctx, verifyWait, clone, []string{"EVAL_CASE=" + caseDir, "EVAL_PROJECT=" + clone}, "/bin/sh", filepath.Join(caseDir, checkFile))
		if err != nil {
			f.CheckNote = err.Error()
		} else {
			f.CheckExit, f.CheckNote = &exit, tail
		}
	}
	return nil
}

// differs 比 main 上的文件和题目原件。一样返回空。
func differs(caseDir, clone, rel string) string {
	want, err := os.ReadFile(filepath.Join(caseDir, projectDir, filepath.FromSlash(rel)))
	if err != nil {
		return "题目原件里没有这个文件"
	}
	got, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(rel)))
	if err != nil {
		return "main 上没了"
	}
	if !bytes.Equal(want, got) {
		return "内容变了"
	}
	return ""
}

// gateResult 是门留下的 last.json 里的一道门。
type gateResult struct {
	Gate     string   `json:"gate"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
	Cached   bool     `json:"cached"`
}

// readGateResults 读这个任务门的结果。开工之前的文件不算。
func readGateResults(gateDir string, start time.Time) []gateResult {
	path := filepath.Join(gateDir, "last.json")
	if !writtenSince(path, start) {
		return nil // 这个任务没走到门的结果
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r struct {
		Results []gateResult `json:"results"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return r.Results
}

func writtenSince(path string, start time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && !info.ModTime().Before(start.Add(-mtimeSlack))
}

// gateLogs 是跑脚本的门留下的日志。
var gateLogs = map[string]string{"test": "test.log", "smoke": "smoke.log", "ui": "ui.log"}

// gateRanHere 对证一道门真的跑过：门的结果里有它，跑脚本的门还要有日志（脚本可以什么都不输出，空的也算），审查门要有审查进程的目录。
// 沿用上次结论（Cached）的门这次没跑模型，不要目录。
func gateRanHere(gateDir, name string, results []gateResult, start time.Time) bool {
	i := slices.IndexFunc(results, func(r gateResult) bool { return r.Gate == name })
	if i < 0 {
		return false
	}
	if results[i].Cached {
		return true
	}
	if log, ok := gateLogs[name]; ok {
		return writtenSince(filepath.Join(gateDir, log), start)
	}
	if name == "review" {
		dirs, err := filepath.Glob(filepath.Join(gateDir, "review-*"))
		return err == nil && len(dirs) > 0 && writtenSince(dirs[0], start)
	}
	return true
}

func countShots(dir string) int {
	n := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, d := range entries {
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			n++
		}
	}
	return n
}

// projectItem：项目都没建出来就是 gone。路径不能空着拼：会读到运行 eval 的当前目录下的 BACKLOG。
func projectItem(project, request string) string {
	if project == "" {
		return "gone"
	}
	return itemState(filepath.Join(project, "docs", "BACKLOG.md"), request)
}

// itemState 读条目最后的状态。认条目靠需求原文开头，模型可能在后面加标记。
func itemState(path, request string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "gone"
	}
	text := string(raw)
	head := textfmt.TruncRunes(request, itemHeadRunes)
	for _, it := range backlog.ParseBacklog(text) {
		if strings.Contains(it.Text, head) {
			switch {
			case it.Stuck:
				return "stuck"
			case it.InProgress:
				return "doing"
			}
			return "todo"
		}
	}
	section := ""
	for _, line := range strings.Split(text, "\n") {
		if t, ok := strings.CutPrefix(line, "## "); ok {
			section = t
			continue
		}
		if !strings.Contains(line, head) {
			continue
		}
		switch {
		case backlog.IsDecisionSection(section):
			return "decision"
		case strings.HasPrefix(section, "已完成"):
			return "done"
		}
	}
	return "gone"
}

// 现场的临时目录：前缀和标记文件都对才算自家的。
const (
	sandboxPrefix = "aq-"
	sandboxMark   = ".agentq-eval"
	// staleAfter：留下的现场过了这么久，下次 run 清掉。
	staleAfter = 7 * 24 * time.Hour
)

// removeTree 删目录，不跟软链接：现场里有指向钥匙串的链接，删的只是链接。
// go 的模块缓存是只读的，先把目录改成可写，否则删不掉。WalkDir 不进软链接，不会改到链接指向的目录。
func removeTree(dir string) error {
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) // 改不了的话下面的 RemoveAll 会报
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return os.RemoveAll(dir)
}

// cleanStale 清 root 下超过 7 天的自家现场（kill -9、没通过留下的）。别的东西一律不碰：
// 前缀不对、不是真目录（软链接）、没有标记文件、标记还新的都留着。返回清了几个。
func cleanStale(root string, now time.Time) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), sandboxPrefix) || !e.IsDir() { // DirEntry 不跟链接，软链接不是目录
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := os.Lstat(filepath.Join(dir, sandboxMark))
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < staleAfter {
			continue
		}
		if removeTree(dir) == nil {
			n++
		}
	}
	return n
}
