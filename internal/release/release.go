// Package release 是公开项目的自动发版：tick 判什么时候发，起脱离 tick 的后台进程跑项目自己的 .agentq/release，发完推送、写后续待做。
//
// 怎么发归项目：.agentq/release --check <版本> 只做说明书检查，不对外写；.agentq/release <版本> 真发（导出、同步公开仓、打 tag、等发布工作流、
// 对校验值、临时 HOME 试装），退出码 0 之后 agentq 再核对公开仓真有这个 tag 和已发布的 Release，都有才算发成。
// 公开仓名由环境变量 AGENTQ_RELEASE_REPO 给（来自 config.json 的 release_repos）。
//
// 什么时候发归 agentq，全满足才发：release_repos 登记了这个项目；项目 AGENTS.md 或 CLAUDE.md 里有「- 自动发版：」开头的规则；
// 主目录在默认分支、.agentq/release 能执行；CHANGELOG.md「## 未发布」有条目；self_project 另要本机这一代观察期过了没变差、
// 默认分支上比它新的只有文档；离上次发成和公开仓上次正式发布都满 7 天；公开仓最新提交 CI 绿；这个版本没失败过；没有任务在跑或在过门。
// 只差最后一条时进入「等发版」：tick 不接新活、不做自动升级，等在跑和在过门的做完就发（任务几乎不停，干等空闲永远等不到）。
// 版本号按「未发布」的条目定：全是「修复：」升补丁号，否则升小版本号。
//
// 后台进程先跑说明书检查：不过就往项目「待做」写一条先修、推一条，这次不发（主目录有新提交后再试）。过了把 CHANGELOG 的「未发布」记成这个版本并提交，
// 再跑 .agentq/release。失败（包括退出码 0 但公开仓没有 tag 或 Release）停在原地：推一条、记下这个版本，以后不再试它（同一个版本不重试）；发成推一条（版本、Release 链接、主要变化），
// self_project 发成后往 site_project 的「待做」写一条「对齐新版本」。这一版带出去的条目写了公开仓 issue 链接的，起草回复推给用户点「发」（issues.go）。
// 发版进行中、等发版时 tick 不接新活、不升级。
package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"agentq/internal/backlog"
	"agentq/internal/env"
	"agentq/internal/evolve"
	"agentq/internal/fsx"
	"agentq/internal/gitx"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/state"
	"agentq/internal/textfmt"
)

// ProcToken 是发版后台进程的内部命令名（agentq _release <项目> <版本>），不列在用法里。
const ProcToken = "_release"

const (
	// minGap：两次发版至少隔这么久（按本机上次发成和公开仓上次正式发布两头算）。
	minGap = 7 * 24 * time.Hour
	// ghEvery：本机条件都满足后，同一个项目最多这么久联网查一次公开仓。
	ghEvery = time.Hour
	// checkWait、releaseWait：说明书检查、真发各最多跑多久，到了杀整个进程组、算失败。
	checkWait   = 30 * time.Minute
	releaseWait = 2 * time.Hour
	// startGrace：tick 记下「发版中」到后台进程拿到锁之间的空档，这么久之内没人拿锁也算在发。
	startGrace = time.Minute
)

// Record.State 的值。
const (
	stRunning = "running"
	stDone    = "done"
	stFailed  = "failed"
	stDocs    = "docs" // 说明书检查没过
)

// Record 是 ~/.agentq/release/<项目>.json：这个项目最近一次发版的情况。
type Record struct {
	Version string    `json:"version,omitempty"` // 最近一次要发的版本
	State   string    `json:"state,omitempty"`
	At      time.Time `json:"at,omitzero"` // 最近一次开始的时间
	// Head：说明书检查没过时主目录的提交；有了新提交才再试。
	Head string `json:"head,omitempty"`
	Why  string `json:"why,omitempty"`
	// Released：最近一次发成的时间。
	Released time.Time `json:"released,omitzero"`
	// Failed：发失败过的版本，不再试。处理好后从这里删掉就会再试。
	Failed []string `json:"failed,omitempty"`
	// CheckedAt：上次联网查公开仓的时间；Note：上次不发的原因（变了才记日志）。
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Note      string    `json:"note,omitempty"`
	// Wait：等发版的版本（别的条件都满足了、只差有任务在跑或在过门）；空的是没在等。
	// Tag：那次联网查到的公开仓最新正式版本（还没发过是空的），一小时内不再查，版本号按它和「未发布」现算。
	Wait string `json:"wait,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Service 是自动发版的模块实例。
type Service struct {
	*env.Env
	Backlog *backlog.Service
	Evolve  *evolve.Service
	Gitx    *gitx.Service
	Notify  *notify.Service
	// spawn 起后台进程，pause 是两次核对之间的等；测试换掉它们。
	spawn func(name, version string) error
	pause func(time.Duration)
}

func (s *Service) dir() string                   { return filepath.Join(s.DataDir(), "release") }
func (s *Service) recordPath(name string) string { return filepath.Join(s.dir(), name+".json") }
func (s *Service) lockPath(name string) string   { return filepath.Join(s.dir(), name+".lock") }
func (s *Service) logPath(name string) string    { return filepath.Join(s.dir(), name+".log") }

func (s *Service) load(name string) (Record, error) {
	var r Record
	b, err := os.ReadFile(s.recordPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err != nil {
		return r, fmt.Errorf("读 %s：%w", s.recordPath(name), err)
	}
	return r, nil
}

func (s *Service) save(name string, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err == nil {
		err = os.MkdirAll(s.dir(), 0o700)
	}
	if err == nil {
		err = fsx.WriteFileAtomic(s.recordPath(name), append(b, '\n'))
	}
	if err != nil {
		return fmt.Errorf("写 %s：%w", s.recordPath(name), err)
	}
	return nil
}

// running：这个项目的发版后台进程拿着锁，或刚记下「发版中」还没来得及拿锁。
func (s *Service) running(name string, r Record) bool {
	if _, _, held := fsx.LockHolder(s.lockPath(name)); held {
		return true
	}
	return r.State == stRunning && s.Now().Sub(r.At) < startGrace
}

// Busy：有项目正在发版时返回给 tick 日志的一句话，tick 这次不接新活、不升级（发版要拿最新的默认分支打包，中途不能有合并）。
func (s *Service) Busy() string {
	for _, name := range slices.Sorted(maps.Keys(s.Cfg.ReleaseRepos)) {
		if r, err := s.load(name); err == nil && s.running(name, r) {
			return busyNote(name, r.Version)
		}
	}
	return ""
}

func busyNote(name, version string) string {
	return "正在自动发版 " + name + " " + version + "，发完再接新活"
}

// Waiting：上次 tick 判下来有项目在等发版。tick 看升级之前问：在等就不升级（换了这一代，观察期要从头来，等的就白等了）。
// 记录只有 StartIfDue 写，等的条件变了它会清掉。
func (s *Service) Waiting() bool {
	for name := range s.Cfg.ReleaseRepos {
		if r, err := s.load(name); err == nil && r.Wait != "" {
			return true
		}
	}
	return false
}

// StatusLine：agentq status 的一行：正在发版、等发版；都没有是空的。st 是 nil（状态读不了）时只看正在发版。
func (s *Service) StatusLine(st *state.State) string {
	if b := s.Busy(); b != "" {
		return "发版：" + b
	}
	if st == nil {
		return ""
	}
	for _, name := range slices.Sorted(maps.Keys(s.Cfg.ReleaseRepos)) {
		if r, err := s.load(name); err == nil && r.Wait != "" {
			return waitNote(name, r.Wait, st.ActiveJobs())
		}
	}
	return ""
}

func waitNote(name, version string, n int) string {
	if n == 0 {
		return "等发版：在跑和在过门的都做完了，下次 tick 开始发 " + name + " " + version
	}
	return fmt.Sprintf("等发版：停接新活，等 %d 个任务做完（发 %s %s）", n, name, version)
}

// StartIfDue：tick 挑活之前调用（拿着 tick.lock）。release_repos 登记的项目里，第一个条件全满足的起后台进程去发；一次最多起一个。
// 返回给 tick 日志的一句话：正在发、刚起了发版、在等发版时不是空的，tick 这次不接新活。
// 不发的原因变了才记一行日志。出错只记日志，不影响 tick。
func (s *Service) StartIfDue(st *state.State) (note string) {
	if len(s.Cfg.ReleaseRepos) == 0 {
		return ""
	}
	if b := s.Busy(); b != "" {
		return b
	}
	defer func() {
		if p := recover(); p != nil {
			s.Logf("自动发版：出错，不影响 tick：%v", p)
			note = ""
		}
	}()
	projects, _ := s.Backlog.AllProjects() // 扫描不全时找得到的照常判
	for _, name := range slices.Sorted(maps.Keys(s.Cfg.ReleaseRepos)) {
		r, err := s.load(name)
		if err != nil {
			s.Logf("自动发版 %s：发版记录读不了，先不发：%v", name, err)
			continue
		}
		i := slices.IndexFunc(projects, func(p backlog.Project) bool { return p.Name == name })
		if i < 0 {
			if r.Wait != "" { // 项目找不到了：不再等它
				r.Wait = ""
				if err := s.save(name, r); err != nil {
					s.Logf("自动发版 %s：%v", name, err)
				}
			}
			continue
		}
		if r.State == stRunning { // 没在跑却还记着「发版中」：后台进程被杀了（关机、重启），按失败算，不重试这个版本
			r = s.failed(name, r, "发版进程中途没了（关机、重启或被杀），没做完")
		}
		before := r
		why, version, n := s.due(context.Background(), st, projects[i], &r)
		if why == "" {
			// 发完情况就变了（记了失败、说明书没过）：下次 tick 重新联网查，不沿用这次的结果
			r.State, r.Version, r.At, r.Why, r.Note, r.CheckedAt, r.Wait, r.Tag = stRunning, version, s.Now(), "", "", time.Time{}, "", ""
			if err := s.save(name, r); err != nil {
				s.Logf("自动发版 %s：%v，这次不发", name, err)
				return ""
			}
			if err := s.start(name, version); err != nil {
				s.failed(name, r, "起不来发版进程："+err.Error())
				return ""
			}
			s.Logf("自动发版 %s：开始发 %s，日志 %s", name, version, s.logPath(name))
			return busyNote(name, version)
		}
		switch {
		case n > 0 && before.Wait == "":
			s.Logf("自动发版 %s：%s 别的条件都满足了，只差有 %d 个任务在跑或在过门：等发版，停接新活、不升级，做完就发", name, version, n)
		case n == 0 && why != before.Note:
			s.Logf("自动发版 %s：这次不发：%s", name, why)
		}
		r.Note = why
		if !equalRecord(before, r) {
			if err := s.save(name, r); err != nil {
				s.Logf("自动发版 %s：%v", name, err)
			}
		}
		if n > 0 {
			return why
		}
	}
	return ""
}

func equalRecord(a, b Record) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// ruleLine：项目规则里允许自动发版的那一行，固定格式「- 自动发版：」开头（正文顺带提到不算）。
var ruleLine = regexp.MustCompile(`(?m)^\s*[-*]\s*(自动发版|Auto[- ]release)\s*[：:]`)

// ruleOn：项目 AGENTS.md 或 CLAUDE.md 写了自动发版的规则。
func ruleOn(dir string) bool {
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil && ruleLine.Match(b) {
			return true
		}
	}
	return false
}

func scriptPath(dir string) string { return filepath.Join(dir, ".agentq", "release") }

// due 判这个项目现在发不发：发就返回空原因和要发的版本。先看本机，都满足了才联网（每 ghEvery 最多一次）；最后看有没有任务在跑或在过门，
// 只差这一条时 waiting 是在跑和在过门的个数，记下 r.Wait（等发版），一小时内不再联网，版本号按记下的 r.Tag 现算。会改 r 的 CheckedAt、Wait、Tag。
func (s *Service) due(ctx context.Context, st *state.State, p backlog.Project, r *Record) (why, version string, waiting int) {
	now := s.Now()
	wasWaiting := r.Wait != ""
	r.Wait = "" // 每次重判，条件都还满足才记回去
	if cur, def, ok := s.Backlog.OnDefault(p); !ok {
		return backlog.NotOnDefaultReason(cur, def), "", 0
	}
	if !ruleOn(p.Dir) {
		return "项目 AGENTS.md、CLAUDE.md 里没有「- 自动发版：」开头的规则", "", 0
	}
	if fi, err := os.Stat(scriptPath(p.Dir)); err != nil || fi.Mode()&0o111 == 0 {
		return "没有能执行的 .agentq/release", "", 0
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, "CHANGELOG.md"))
	entries := Unreleased(string(b))
	if err != nil || len(entries) == 0 {
		return "CHANGELOG.md「## 未发布」里没有条目", "", 0
	}
	if p.Name == s.Cfg.SelfProject {
		if why := s.observed(p); why != "" {
			return why, "", 0
		}
	}
	if !r.Released.IsZero() && now.Sub(r.Released) < minGap {
		return "离上次发成（" + textfmt.WhenCN(r.Released) + "）不到 7 天", "", 0
	}
	// 一小时内查过：没过的原因照旧，下次再查；过了（在等发版）就不再查公开仓和 CI
	fresh := !r.CheckedAt.IsZero() && now.Sub(r.CheckedAt) < ghEvery && !r.CheckedAt.After(now)
	if fresh && !wasWaiting {
		return r.Note, "", 0
	}
	repo := s.Cfg.ReleaseRepos[p.Name]
	if !fresh {
		r.CheckedAt = now
		tag, at, err := s.latestRelease(ctx, repo)
		if err != nil {
			return "查不了公开仓 " + repo + " 的发布：" + err.Error(), "", 0
		}
		if tag != "" && now.Sub(at) < minGap {
			return "公开仓上次发布 " + tag + "（" + textfmt.WhenCN(at.Local()) + "）不到 7 天", "", 0
		}
		r.Tag = tag
	}
	version, err = NextVersion(r.Tag, entries)
	if err != nil {
		return err.Error(), "", 0
	}
	if slices.Contains(r.Failed, version) {
		return "上次发 " + version + " 没成，停在原地等你处理（日志 " + s.logPath(p.Name) + "；处理好后从 " + s.recordPath(p.Name) + " 的 failed 里删掉它）", "", 0
	}
	if r.State == stDocs && r.Version == version {
		head, err := s.Gitx.Git(p.Dir, "rev-parse", "HEAD")
		if err != nil || head == r.Head || docsTodoOpen(p, version) {
			return version + " 的说明书检查没过，等「待做」里那一条修好合并", "", 0
		}
	}
	if !fresh {
		why, err = s.ciGreen(ctx, repo)
		if err != nil {
			return "查不了公开仓 " + repo + " 的 CI：" + err.Error(), "", 0
		}
		if why != "" {
			return why, "", 0
		}
	}
	if n := st.ActiveJobs(); n > 0 {
		r.Wait = version
		return waitNote(p.Name, version, n), version, n
	}
	return "", version, 0
}

// observed：self_project 要本机这一代观察期过了没变差，默认分支上比它新的只能是文档（Markdown），不然发出去的是没在本机观察过的代码。
func (s *Service) observed(p backlog.Project) string {
	commit, why := s.Evolve.ObservedCommit()
	if commit == "" {
		return why
	}
	out, err := s.Gitx.Git(p.Dir, "diff", "--name-only", commit, "HEAD")
	if err != nil {
		return "比不了本机这一代 " + commit + " 和默认分支：" + textfmt.OneLine(out, 120)
	}
	n := 0
	for _, f := range strings.Fields(out) {
		if !strings.HasSuffix(f, ".md") {
			n++
		}
	}
	if n > 0 {
		return fmt.Sprintf("默认分支比本机这一代 %s 多改了 %d 个文件，等装上、观察期过了再发", commit, n)
	}
	return ""
}

// start 起脱离 tick 的 agentq _release <项目> <版本>：自己的进程组，tick 退出不带走它。
func (s *Service) start(name, version string) error {
	if s.spawn != nil {
		return s.spawn(name, version)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到自己：%w", err)
	}
	logf, err := os.OpenFile(s.logPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, ProcToken, name, version)
	cmd.Dir = s.dir()
	cmd.Env = proc.ChildEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // tick 还活着时不留僵尸，退出后交给 launchd 回收
	return nil
}

// failed：这个版本没发成。记下、推一条，以后不再试它。
func (s *Service) failed(name string, r Record, why string) Record {
	r.State, r.Why = stFailed, why
	if r.Version != "" && !slices.Contains(r.Failed, r.Version) {
		r.Failed = append(r.Failed, r.Version)
	}
	if err := s.save(name, r); err != nil {
		s.Logf("自动发版 %s：%v", name, err)
	}
	s.Logf("自动发版 %s：%s 没发成：%s", name, r.Version, why)
	s.Notify.Notify(notify.Notice{Cat: "alert", Key: "release:" + name + ":" + r.Version,
		Text:  "自动发版 " + name + " " + r.Version + " 没成，停在原地，这个版本不再自动试",
		Lines: []string{textfmt.OneLine(why, 300), "日志：" + s.Tilde(s.logPath(name))}})
	return r
}

// Run 是后台进程 agentq _release <项目> <版本>：说明书检查、记 CHANGELOG、真发、收尾。同一个项目同时只有一个。
func (s *Service) Run(args []string) error {
	if len(args) != 2 || !versionRe.MatchString(args[1]) {
		return errors.New("用法：agentq " + ProcToken + " <项目> <vX.Y.Z>")
	}
	name, version := args[0], args[1]
	repo := s.Cfg.ReleaseRepos[name]
	if repo == "" {
		return errors.New(name + " 不在 release_repos 里")
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return err
	}
	lock, err := fsx.AcquireLock(s.lockPath(name), 0)
	if err != nil {
		return err
	}
	if lock == nil {
		fmt.Fprintln(s.Out, name+" 已经在发版")
		return nil
	}
	defer lock.Close()
	r, err := s.load(name)
	if err != nil {
		return err
	}
	r.State, r.Version, r.At = stRunning, version, s.Now()
	projects, _ := s.Backlog.AllProjects()
	i := slices.IndexFunc(projects, func(p backlog.Project) bool { return p.Name == name })
	if i < 0 {
		s.failed(name, r, "找不到项目 "+name)
		return env.ErrLogged
	}
	p := projects[i]
	ctx := context.Background()
	if out, err := s.script(ctx, p, repo, checkWait, "--check", version); err != nil {
		s.docsFailed(p, r, out, err)
		return env.ErrLogged
	}
	entries, err := s.stamp(p, version)
	if err != nil {
		s.failed(name, r, "CHANGELOG 记成 "+version+" 没成："+err.Error())
		return env.ErrLogged
	}
	if out, err := s.script(ctx, p, repo, releaseWait, version); err != nil {
		s.failed(name, r, fmt.Sprintf(".agentq/release %s：%v；%s", version, err, out))
		return env.ErrLogged
	}
	// 退出码 0 只是脚本自己说的：公开仓真有 tag 和 Release 才算发成，才推通知、写后续待做
	if why := s.unpublished(ctx, repo, version); why != "" {
		s.failed(name, r, fmt.Sprintf(".agentq/release %s 退出码是 0，但%s", version, why))
		return env.ErrLogged
	}
	r.State, r.Why, r.Released = stDone, "", s.Now()
	if err := s.save(name, r); err != nil {
		s.Logf("自动发版 %s：%v", name, err)
	}
	s.released(p, repo, version, entries)
	s.issueReplies(p, repo, version, r.Failed)
	return nil
}

// script 跑项目的 .agentq/release，输出追加进发版日志，返回最后几行。
func (s *Service) script(ctx context.Context, p backlog.Project, repo string, wait time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath(p.Dir), args...)
	cmd.Dir = p.Dir
	cmd.Env = append(proc.ChildEnv(), "AGENTQ_RELEASE_REPO="+repo)
	proc.BoundCmd(cmd)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(s.Out, &buf)
	cmd.Stderr = cmd.Stdout
	s.Logf("自动发版 %s：跑 .agentq/release %s", p.Name, strings.Join(args, " "))
	err := cmd.Run()
	defer proc.ClearLeftovers(cmd)
	if ctx.Err() != nil {
		err = fmt.Errorf("跑了 %s 还没完，杀掉了", textfmt.FormatDuration(wait))
	}
	return tail(buf.String(), 8), err
}

// tail：最后 n 个非空行，拼成一行。
func tail(out string, n int) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return textfmt.OneLine(strings.Join(lines, "；"), 600)
}

// itemText：写进 BACKLOG 的外来文字去掉会被当成条目格式的东西（分隔符、状态标记的全角括号）。
func itemText(s string) string {
	return strings.NewReplacer("——", "—", "（", "(", "）", ")").Replace(textfmt.OneLine(s, 400))
}

func docsKey(version string) string { return "发版前说明书检查没过（" + version + "）" }

// docsTodoOpen：说明书检查没过写的那一条还在「待做」里。
func docsTodoOpen(p backlog.Project, version string) bool {
	items, err := p.Items()
	return err != nil || slices.ContainsFunc(items, func(it backlog.Item) bool { return it.Title == docsKey(version) })
}

// docsFailed：说明书检查没过。往项目「待做」写一条先修（同一个版本只写一条）、推一条，这次不发；
// 那一条做完（不在「待做」里了）、主目录也有了新提交才再试。
func (s *Service) docsFailed(p backlog.Project, r Record, out string, err error) {
	key := docsKey(r.Version)
	line := "- [ ] " + key + " —— 自动发版 " + r.Version + " 前跑 `.agentq/release --check " + r.Version + "` 没过，这次没发。报的是：" + itemText(out) +
		"。要什么：把 README 等说明书和 `--help`、版本号、支持矩阵对齐；验收：`.agentq/release --check " + r.Version + "` 退出码 0。"
	note := "写进了「待做」，修好合并后再发"
	werr := s.Backlog.CommitBacklogNamed(p, "待做：自动发版 "+r.Version+" 前说明书检查没过", func(old string) (string, error) {
		if strings.Contains(old, key) {
			return old, nil
		}
		text, ok := backlog.InsertTodo(old, line)
		if !ok {
			return "", errors.New("BACKLOG 里没有「## 待做」一节")
		}
		return text, nil
	})
	if werr != nil {
		note = "待做没写成（" + textfmt.OneLine(werr.Error(), 120) + "），要你手动记一条"
	}
	head, _ := s.Gitx.Git(p.Dir, "rev-parse", "HEAD") // 写完待做再记：那次提交不算「有了新提交」
	r.State, r.Head, r.Why = stDocs, head, textfmt.OneLine(err.Error()+"；"+out, 400)
	if err := s.save(p.Name, r); err != nil {
		s.Logf("自动发版 %s：%v", p.Name, err)
	}
	s.Logf("自动发版 %s：%s 的说明书检查没过，这次不发；%s", p.Name, r.Version, note)
	s.Notify.Notify(notify.Notice{Cat: "alert", Key: "release-docs:" + p.Name + ":" + r.Version,
		Text:  "自动发版 " + p.Name + " " + r.Version + " 前说明书检查没过，这次没发；" + note,
		Lines: []string{textfmt.OneLine(out, 300), "日志：" + s.Tilde(s.logPath(p.Name))}})
}

// stamp 拿着 <项目>-main 锁把 CHANGELOG 的「未发布」记成这个版本，只提交这个文件。返回这一版的条目。
func (s *Service) stamp(p backlog.Project, version string) ([]string, error) {
	unlock, err := s.Backlog.LockMain(p.Name+"-main", backlog.MainLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if cur, def, ok := s.Backlog.OnDefault(p); !ok {
		return nil, errors.New(backlog.NotOnDefaultReason(cur, def))
	}
	if out, err := s.Gitx.Git(p.Dir, "status", "--porcelain", "--", "CHANGELOG.md"); err != nil || out != "" {
		return nil, fmt.Errorf("CHANGELOG.md 有没提交的改动或查不了 git 状态（%s）", textfmt.OneLine(out, 100))
	}
	path := filepath.Join(p.Dir, "CHANGELOG.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries := Unreleased(string(b))
	text, err := Stamp(string(b), version)
	if err != nil {
		return nil, err
	}
	if err := fsx.WriteKeepMode(path, []byte(text)); err != nil {
		return nil, err
	}
	if out, err := s.Gitx.Git(p.Dir, "commit", "-q", "-m", stampSubject(version), "--", "CHANGELOG.md"); err != nil {
		_ = fsx.WriteKeepMode(path, b)
		return nil, fmt.Errorf("提交失败，CHANGELOG 已改回：%v %s", err, textfmt.OneLine(out, 200))
	}
	return entries, nil
}

// released：发成了。推一条（版本、Release 链接、主要变化）；self_project 往官网项目写一条「对齐新版本」。
func (s *Service) released(p backlog.Project, repo, version string, entries []string) {
	link := "https://github.com/" + repo + "/releases/tag/" + version
	lines := []string{link}
	for i, e := range entries {
		if i == 5 {
			lines = append(lines, fmt.Sprintf("还有 %d 条，见 CHANGELOG", len(entries)-5))
			break
		}
		lines = append(lines, "· "+textfmt.OneLine(e, 80))
	}
	s.Logf("自动发版 %s：发了 %s：%s", p.Name, version, link)
	s.Notify.Notify(notify.Notice{Cat: "release", Key: "release:" + p.Name + ":" + version, Text: "发布了 " + p.Name + " " + version, Lines: lines})
	if p.Name != s.Cfg.SelfProject || s.Cfg.SiteProject == "" {
		return
	}
	site, err := s.Backlog.AllProjects()
	i := slices.IndexFunc(site, func(q backlog.Project) bool { return q.Name == s.Cfg.SiteProject })
	if i < 0 {
		s.Logf("自动发版 %s：找不到官网项目 %s（%v），没写「对齐新版本」", p.Name, s.Cfg.SiteProject, err)
		return
	}
	key := "对齐 " + p.Name + " 新版本 " + version
	var changes []string
	for _, e := range entries {
		changes = append(changes, textfmt.OneLine(e, 60))
	}
	line := "- [ ] " + key + " —— " + p.Name + " 发了 " + version + "（" + link + "）。要什么：官网上写的版本号、安装命令、功能说明对齐这一版，主要变化：" +
		itemText(strings.Join(changes, "；")) + "；验收：官网写的版本和功能和 Release 说明一致。"
	werr := s.Backlog.CommitBacklog(site[i], "待做：对齐 "+p.Name+" "+version, func(old string) (string, error) {
		if strings.Contains(old, key) {
			return old, nil
		}
		text, ok := backlog.InsertTodo(old, line)
		if !ok {
			return "", errors.New("BACKLOG 里没有「## 待做」一节")
		}
		return text, nil
	})
	if werr != nil {
		s.Logf("自动发版 %s：官网项目 %s 的「对齐新版本」没写成：%v", p.Name, s.Cfg.SiteProject, werr)
	}
}
