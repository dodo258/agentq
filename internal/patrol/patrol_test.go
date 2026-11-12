package patrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	bl "agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit"
	"agentq/internal/testkit/stack"
)

// 巡逻：被排除项目的任务失败照常报（记录在 ~/.agentq，这个任务是你明确跑的），但不写接手待做、不提交、不说会接着做，也不起模型分析。
func TestExcludePatrolDoesNotTouchBacklog(t *testing.T) {
	e := newPatrolFixture(t)
	home := e.a.Home
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	testkit.GitT(t, home, proj, "init", "-q", "-b", "main")
	orig := "修登录页 —— 要什么"
	backlog := "# 待办\n\n## 待做\n\n- [ ] " + orig + "（进行中：agent/1011-login 10-11 09:00）\n\n## 已完成\n"
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), backlog)
	testkit.GitT(t, home, proj, "add", ".")
	testkit.GitT(t, home, proj, "commit", "-q", "-m", "init")
	head := testkit.GitT(t, home, proj, "rev-parse", "HEAD")
	e.a.Cfg.ExcludeProjects = []string{"p"}
	j := &state.Job{ID: "1011-090000-p", Project: "p", ProjectDir: proj, Title: "修登录页", Fingerprint: bl.Fingerprint("- [ ] " + orig), Status: state.StatusTimeout,
		Summary: "运行超过 4h，已终止", StartedAt: e.now.Add(-5 * time.Hour), EndedAt: patrolAgo(e.now, time.Hour)}
	e.st.Jobs = []*state.Job{j}
	r := e.patrol()
	is := patrolFind(t, r.New, "job:"+j.ID)
	patrolAssertContains(t, is.Action, "exclude_projects", "只报不写") // 巡逻连 BACKLOG 都不读，不是读了写不进去
	if strings.Contains(is.Action, "agentq 会接着做") {
		t.Errorf("不该说会接着做：%s", is.Action)
	}
	if is.handled {
		t.Error("没人接手，不能算已处理：要推给你")
	}
	if got := testkit.GitT(t, home, proj, "rev-parse", "HEAD"); got != head {
		t.Error("不该提交")
	}
	testkit.AssertBytes(t, filepath.Join(proj, "docs", "BACKLOG.md"), backlog)
	if len(e.analyzed) != 0 {
		t.Errorf("不该为被排除的项目起模型分析：%v", e.analyzed)
	}
}

// 巡逻新发现推「巡逻发现」：写了接手待做的、拍板和额度不推，基础设施类（看门狗推）不推；看门狗查不到的（服务没装上、缺夜班报告）照推。
func TestPatrolNotifies(t *testing.T) {
	e := newPatrolFixture(t)
	r := &PatrolReport{New: []PatrolIssue{
		{Fingerprint: "disk", Check: "disk", Title: "磁盘剩余不到 20 GB", Evidence: []string{"剩 5 GB"}, Action: "清理"},
		{Fingerprint: "backup:x", Check: "backup", Title: "备份失败"},
		{Fingerprint: "autopush:/p/a/.git@x", Check: "autopush", Title: "自动备份推送失败：a"},
		{Fingerprint: "launchd:x", Check: "launchd", Title: "launchd 服务 x 上次退出码 1"},
		{Fingerprint: "launchd:x.listen:down", Check: "launchd", Title: "Telegram 监听没在跑"},
		{Fingerprint: "report:review:x", Check: "report", Title: "08:00 后还没有当天的早报"},
		{Fingerprint: "launchd:x.night:missing", Check: "launchd", Title: "夜班的定点服务没装上"},
		{Fingerprint: "report:night:x", Check: "report", Title: "08:00 后还没有当天的夜班报告"},
		{Fingerprint: "job:2", Check: "job", Title: "任务失败：p「甲」", Evidence: []string{"任务 2 出错了"}, Action: "看日志"},
		{Fingerprint: "job:1", Check: "job", Title: "任务中断", handled: true},
		{Fingerprint: "decide:x", Check: "decide", Title: "拍板"},
		{Fingerprint: "usage", Check: "usage", Title: "额度"},
	}}
	e.a.notifyPatrol(r)
	s, err := e.a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, n := range s.Pending {
		texts = append(texts, n.Text)
	}
	if strings.Join(texts, "|") != "夜班的定点服务没装上|08:00 后还没有当天的夜班报告|任务失败：p「甲」" || s.Pending[2].Lines[0] != "任务 2 出错了" {
		t.Fatalf("应只推没装上、缺夜班报告和任务失败：%+v", s.Pending)
	}
}

// wirePatrol 建模块实例。
func wirePatrol(e *env.Env) *Service {
	k := stack.New(e)
	return &Service{Env: e, Backlog: k.Backlog, Gitx: k.Gitx, Ledger: k.Ledger, Notify: k.Notify, Runner: k.Runner, Service: k.Service, Telegram: k.Telegram}
}

// patrolFixture 进程内跑 runPatrol，launchctl、磁盘、分析任务都换成假的。
type patrolFixture struct {
	t        *testing.T
	a        *Service
	out      *bytes.Buffer
	root     string
	now      time.Time
	launchd  string // launchctl list 的输出
	lderr    error
	free     uint64
	analyzed []string // 每次启动分析时的 prompt
	st       *state.State
}

func newPatrolFixture(t *testing.T) *patrolFixture {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	// 本机装了 govulncheck 时不能查真的：默认放一个什么都查不出的假的，漏洞用例自己再放
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "govulncheck"), []byte("#!/bin/sh\necho '{\"config\":{}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	// 接手待做要在临时仓库里提交：临时 HOME 下的 git 配置给出身份，也没有全局钩子
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	var out bytes.Buffer
	if err := os.MkdirAll(filepath.Join(home, ".agentq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agentq", "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	en, err := env.New(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a := wirePatrol(en)
	a.Cfg.ProjectsRoot = root
	a.Cfg.AutopushLog = "~/.claude/state/autopush.log"
	a.Cfg.NightEnabled, a.Cfg.ReviewEnabled = true, true
	e := &patrolFixture{t: t, a: a, out: &out, root: root, free: 100 << 30, st: &state.State{},
		now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)}
	e.launchd = "PID\tStatus\tLabel\n-\t0\t" + LaunchdPrefix + "agentq\n-\t0\t" + LaunchdPrefix + "agentq.night\n-\t0\t" + LaunchdPrefix + "agentq.review\n" +
		"123\t1\tcom.apple.something\n"
	a.Now = func() time.Time { return e.now }
	a.Service.Launchctl = func(args ...string) ([]byte, error) {
		if len(args) != 1 || args[0] != "list" {
			t.Fatalf("巡逻只该执行 launchctl list，收到 %v", args)
		}
		return []byte(e.launchd), e.lderr
	}
	a.DiskFree = func(string) (uint64, uint64, error) { return e.free, 500 << 30, nil }
	a.AnalyzePatrol = func(st *state.State, prompt string) (*state.Job, error) {
		e.analyzed = append(e.analyzed, prompt)
		return &state.Job{ID: "fake-analysis"}, nil
	}
	// 当天早报和夜班报告都在
	e.write(e.nightlyFile(""), "早报\n")
	e.write(e.nightlyFile("-night"), "夜班\n")
	return e
}

func (e *patrolFixture) nightlyFile(suffix string) string {
	return filepath.Join(e.a.Home, ".claude", "nightly", e.now.Format("2006-01-02")+suffix+".md")
}

func (e *patrolFixture) write(path, text string) {
	e.t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *patrolFixture) patrol() *PatrolReport {
	e.t.Helper()
	ps, err := e.a.LoadPatrol()
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := e.a.RunPatrol(e.st, ps)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *patrolFixture) summary() string {
	b, _ := os.ReadFile(e.a.NightlyPath(e.now))
	return string(b)
}

func patrolFPs(list []PatrolIssue) []string {
	var out []string
	for _, is := range list {
		out = append(out, is.Fingerprint)
	}
	sort.Strings(out)
	return out
}

func patrolFind(t *testing.T, list []PatrolIssue, fp string) PatrolIssue {
	t.Helper()
	for _, is := range list {
		if is.Fingerprint == fp {
			return is
		}
	}
	t.Fatalf("没有指纹 %s 的问题，现有 %v", fp, patrolFPs(list))
	return PatrolIssue{}
}

func patrolAgo(now time.Time, d time.Duration) *time.Time { t := now.Add(-d); return &t }

func patrolAssertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("没有 %q：\n%s", w, got)
		}
	}
}

// 每类检查都放上「看着像、其实没问题」的情况：一个都不该报，不启动分析，汇总不写。
func TestPatrolNoIssues(t *testing.T) {
	e := newPatrolFixture(t)
	n := e.now
	e.write(filepath.Join(e.a.Home, ".claude", "state", "autopush.log"),
		"2026-10-10 11:00:00 FAIL /p/agentq/.git: 25 小时前的旧 FAIL\n2026-10-11 11:52:10 ok /p/agentq/.git\nFAIL 不是固定格式的行\n"+
			// 网络抖了一下：连续失败，之后同仓库推成功了，算已恢复
			"2026-10-11 11:20:00 FAIL /p/sb/.git: SSL_ERROR_SYSCALL\n2026-10-11 11:21:00 FAIL /p/sb/.git: SSL_ERROR_SYSCALL\n2026-10-11 11:30:00 ok /p/sb/.git\n")
	e.write(e.nightlyFile("-backup"), "备份：7 个仓库已推到 GitHub 私有备份\n\n- agentq：已推送（上次推送失败过，今天好了）\n")
	e.st.Jobs = []*state.Job{
		{ID: "done", Project: "p", Status: state.StatusDone, StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "killed", Project: "p", Status: state.StatusKilled, StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "limit", Project: "p", Status: state.StatusFailed, LimitHit: true, StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "resume", Project: "p", Status: state.StatusInterrupted, ResumePending: true, StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "resumed", Project: "p", Status: state.StatusInterrupted, ResumedBy: "x", StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "old", Project: "p", Status: state.StatusFailed, StartedAt: n.Add(-30 * time.Hour), EndedAt: patrolAgo(n, 25*time.Hour)},
		{ID: "own", Project: PatrolJobName, Kind: state.KindPatrol, Status: state.StatusFailed, StartedAt: n.Add(-time.Hour), EndedAt: patrolAgo(n, time.Minute)},
		// 运行中、会话记录 1 分钟前还在长
		{ID: "busy", Project: "p", Status: state.StatusRunning, SessionID: "s-busy", StartedAt: n.Add(-2 * time.Hour)},
	}
	tr := filepath.Join(e.a.Home, ".claude", "projects", "-x", "s-busy.jsonl")
	e.write(tr, "{}\n")
	os.Chtimes(tr, n.Add(-time.Minute), n.Add(-time.Minute))
	e.st.SetUsageOf("", &backend.UsageInfo{CheckedAt: n.Add(-time.Hour), Windows: []backend.UsageWindow{
		{Label: "session", Used: 50},
		{Label: "week", Used: 95, ResetsAt: patrolAgo(n, time.Hour)}, // 已经过了重置时刻
		{Label: "week (Fable)", Scope: "Fable", Used: 99},            // 只限别的模型
	}})
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n- [ ] 甲 —— 等用户决定的事写在下面\n\n## 等用户决定\n\n<!-- - [ ] 注释里的模板 -->\n\n## 已完成\n\n- [x] 乙\n")

	r := e.patrol()
	if len(r.New)+len(r.Old)+len(r.Gone) != 0 || len(r.Notes) != 0 {
		t.Fatalf("不该有问题：新 %v 旧 %v 恢复 %v 提示 %v", patrolFPs(r.New), patrolFPs(r.Old), patrolFPs(r.Gone), r.Notes)
	}
	if len(e.analyzed) != 0 || r.Analysis != "" {
		t.Errorf("没有异常不该分析：%q", r.Analysis)
	}
	if s := e.summary(); s != "" {
		t.Errorf("没有问题不该写汇总：\n%s", s)
	}
	ps, _ := e.a.LoadPatrol()
	if !ps.LastRun.Equal(n) || len(ps.Issues) != 0 {
		t.Errorf("patrol.json：%+v", ps)
	}
	if got := r.logLine(); got != "巡逻：没有发现问题" {
		t.Errorf("日志行：%s", got)
	}

	// 08:00 前不查早报、夜班报告；开关关着的不查
	os.Remove(e.nightlyFile(""))
	os.Remove(e.nightlyFile("-night"))
	e.st.Jobs, e.st.BackendLimits = nil, nil // 时间往前拨了，免得它们变成 24 小时内的
	os.Remove(filepath.Join(e.a.Home, ".claude", "state", "autopush.log"))
	e.now = time.Date(2026, 10, 11, 7, 59, 0, 0, time.Local)
	if r := e.patrol(); len(r.New) != 0 {
		t.Errorf("08:00 前不该报缺报告：%v", patrolFPs(r.New))
	}
	e.now = time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local)
	e.a.Cfg.NightEnabled, e.a.Cfg.ReviewEnabled = false, false
	if r := e.patrol(); len(r.New) != 0 {
		t.Errorf("开关关着不该报缺报告：%v", patrolFPs(r.New))
	}
	// 开着、但正在跑
	e.a.Cfg.NightEnabled = true
	e.st.Jobs = append(e.st.Jobs, &state.Job{ID: "night", Kind: config.KindNight, Status: state.StatusRunning, StartedAt: e.now.Add(-time.Hour)})
	if r := e.patrol(); len(r.New) != 0 {
		t.Errorf("夜班还在跑不该报缺报告：%v", patrolFPs(r.New))
	}
}

// 每类检查都有问题：逐条带证据报出来、写进汇总和 patrol.json，有异常就启动一次分析（分析里不含拍板和额度）。
// 推给用户的只有任务那几条和看门狗查不到的（定点服务没装上、缺夜班报告）：基础设施类（launchd 退出码、推送、备份、早报、磁盘）
// 只进汇总，拍板、额度各有自己的推送。
func TestPatrolFindsEachCheck(t *testing.T) {
	e := newPatrolFixture(t)
	n := e.now
	e.launchd = "PID\tStatus\tLabel\n-\t1\t" + LaunchdPrefix + "backup\n-\t0\t" + LaunchdPrefix + "agentq\n-\t0\t" + LaunchdPrefix + "agentq.review\n77\t-9\t" + LaunchdPrefix + "keep\n"
	// backup 服务的 plist 和日志：证据里带日志末尾
	logp := filepath.Join(e.a.Home, "backup.log")
	e.write(logp, "第一行\n备份：5 个仓库已推到 GitHub 私有备份，**1 个失败**\n")
	e.write(e.a.Service.PlistPathFor(LaunchdPrefix+"backup"), `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>`+LaunchdPrefix+`backup</string><key>StandardErrorPath</key><string>`+logp+`</string></dict></plist>
`)
	e.write(filepath.Join(e.a.Home, ".claude", "state", "autopush.log"),
		"2026-10-11 09:00:00 FAIL /p/agentq/.git: 提示： 如果您希望先与远程变更合并\n2026-10-11 09:30:00 ok /p/agentq/.git\n"+
			"2026-10-11 10:00:00 FAIL /p/other/.git: cannot lock ref\n2026-10-11 10:05:00 FAIL /p/other/.git: cannot lock ref again\n")
	e.write(e.nightlyFile("-backup"), "备份：5 个仓库已推到 GitHub 私有备份，**1 个失败**；没配备份的： x\n\n- agentq：已推送\n- .claude：**推送失败**：cannot lock ref\n")
	os.Remove(e.nightlyFile(""))
	os.Remove(e.nightlyFile("-night"))
	e.st.Jobs = []*state.Job{
		{ID: "f1", Project: "p", Title: "失败的", Status: state.StatusFailed, Summary: "出错了：测试失败", StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "t1", Project: "p", Title: "超时的", Status: state.StatusTimeout, Summary: "运行超过 4h", StartedAt: n.Add(-5 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "i1", Project: "p", Title: "中断的", Status: state.StatusInterrupted, ResumeOf: "i0", Summary: "续跑后仍中断", StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "r1", Kind: config.KindReview, Project: "复盘", Title: "agentq review", Status: state.StatusInterrupted, Summary: "正常退出但没写出报告", StartedAt: n.Add(-5 * time.Hour), EndedAt: patrolAgo(n, 4*time.Hour)},
		{ID: "s1", Project: "p", Title: "卡住的", Status: state.StatusRunning, SessionID: "s-stall", StartedAt: n.Add(-2 * time.Hour), PID: 4242},
	}
	e.st.SetUsageOf("", &backend.UsageInfo{CheckedAt: n.Add(-5 * time.Minute), Windows: []backend.UsageWindow{
		{Label: "session", Used: 20}, {Label: "week", Used: 93, ResetsAt: func() *time.Time { t := n.Add(48 * time.Hour); return &t }()},
	}})
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), "# 待办\n\n## 待做\n\n## 等用户决定\n\n- [ ] 要不要改写历史 —— A 不改写；B 改写；推荐 A；分支 agent/x\n\n## 已完成\n")
	e.free = 10 << 30

	r := e.patrol()
	want := []string{
		"autopush:/p/other/.git@2026-10-11T10:00:00", "backup:2026-10-11", // agentq 那一段之后推成功了，不报
		"decide:p:", "disk", "job:f1", "job:i1", "job:r1", "job:t1",
		"launchd:" + LaunchdPrefix + "agentq.night:missing", "launchd:" + LaunchdPrefix + "backup", "launchd:" + LaunchdPrefix + "keep",
		"report:night:2026-10-11", "report:review:2026-10-11", "stall:s1", "usage:week@2026-10-13T12:00",
	}
	got := patrolFPs(r.New)
	if len(got) != len(want) {
		t.Fatalf("问题\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("问题\n got %v\nwant %v", got, want)
		}
	}
	backupEvidence := "**1 个失败**" // 日志末尾
	if runtime.GOOS != "darwin" {
		backupEvidence = "上次退出码 1" // 从 plist 取日志路径靠 plutil，只有 macOS 有
	}
	checks := []struct{ fp, want string }{
		{"launchd:" + LaunchdPrefix + "backup", backupEvidence},
		{"launchd:" + LaunchdPrefix + "keep", "上次退出码 -9（被信号 9 终止），现在在跑（pid 77）"},
		{"launchd:" + LaunchdPrefix + "agentq.night:missing", "night_enabled=true"},
		{"autopush:/p/other/.git@2026-10-11T10:00:00", "10-11 周日 10:00 起连续 2 次 FAIL，之后还没推成功，最后一次：2026-10-11 10:05:00 FAIL /p/other/.git: cannot lock ref again"},
		{"backup:2026-10-11", "- .claude：**推送失败**：cannot lock ref"},
		{"job:f1", "出错了：测试失败"},
		{"job:r1", "正常退出但没写出报告"},
		{"stall:s1", "会话记录 120 分钟 没增长"},
		{"usage:week@2026-10-13T12:00", "本周 93%"},
		{"report:review:2026-10-11", "今天的复盘任务 r1 中断"},
		{"report:night:2026-10-11", "今天没有夜班任务的记录"},
		{"disk", "剩余 10.0 GB"},
	}
	for _, c := range checks {
		is := patrolFind(t, r.New, c.fp)
		if !strings.Contains(strings.Join(is.Evidence, "\n"), c.want) {
			t.Errorf("%s 的证据里应有 %q：%q", c.fp, c.want, is.Evidence)
		}
		if is.Action == "" {
			t.Errorf("%s 应写处理说明", c.fp)
		}
	}
	// 汇总：每个新问题一行，带证据；疑似卡住和额度 tick 自己写过，不重复写
	s := e.summary()
	if c := strings.Count(s, "巡逻发现："); c != len(want)-2 || strings.Contains(s, "巡逻发现：任务疑似卡住") || strings.Contains(s, "巡逻发现：订阅额度") {
		t.Errorf("汇总应有 %d 行巡逻发现，实际 %d：\n%s", len(want)-2, c, s)
	}
	if !strings.Contains(s, "巡逻发现：今天的备份有 1 个仓库推送失败；证据：") {
		t.Errorf("汇总内容：\n%s", s)
	}
	// 额度到了后台上限：不启动分析
	if len(e.analyzed) != 0 || !strings.Contains(r.Analysis, "订阅额度到了后台上限") {
		t.Errorf("额度到 90%% 不该分析（%d 次）：%q", len(e.analyzed), r.Analysis)
	}
	ps, _ := e.a.LoadPatrol()
	if len(ps.Issues) != len(want) || ps.AnalysisCount != 0 {
		t.Errorf("patrol.json：%d 个问题，分析 %d 次", len(ps.Issues), ps.AnalysisCount)
	}
	ns, err := e.a.Notify.LoadNotify()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, n := range ns.Pending {
		keys = append(keys, n.Key)
		if n.Security {
			t.Errorf("巡逻推送不该有安全问题：%+v", n)
		}
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "patrol:job:f1,patrol:job:i1,patrol:job:r1,patrol:job:t1,patrol:launchd:"+LaunchdPrefix+"agentq.night:missing,"+
		"patrol:report:night:2026-10-11,patrol:stall:s1" {
		t.Errorf("只推任务的问题，基础设施类只进汇总：%s", got)
	}
}

// 带「（等条件：…）」的拍板条目巡逻不报；正文顺带提到「等条件」的照常报；报过的条目后来加上标记，悄悄去掉、不写「已处理」。
func TestPatrolSkipsWaitCondition(t *testing.T) {
	e := newPatrolFixture(t)
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	backlog := filepath.Join(proj, "docs", "BACKLOG.md")
	e.write(backlog, "## 等用户决定\n\n- [ ] v4 切换发布 —— 推荐发布（等条件：前面 7 条都合并）\n- [ ] 选 A 还是 B —— 写明等条件：的用法；推荐 A\n")
	r := e.patrol()
	var decide []PatrolIssue
	for _, is := range r.New {
		if is.Check == "decide" {
			decide = append(decide, is)
		}
	}
	if len(decide) != 1 || !strings.Contains(decide[0].Title, "选 A 还是 B") {
		t.Fatalf("只报没带标记的那条：%+v", decide)
	}
	e.now = e.now.Add(2 * time.Hour)
	e.write(backlog, "## 等用户决定\n\n- [ ] v4 切换发布 —— 推荐发布（等条件：前面 7 条都合并）\n- [ ] 选 A 还是 B —— 推荐 A（等条件：用户先看完效果图）\n")
	r = e.patrol()
	for _, is := range r.Gone {
		if is.Check == "decide" && is.gone != "" {
			t.Errorf("加上等条件标记不算已处理：%+v", is)
		}
	}
	if strings.Contains(e.summary(), "等用户决定的已处理") {
		t.Errorf("汇总不该写已处理：\n%s", e.summary())
	}
	if ps, _ := e.a.LoadPatrol(); ps != nil {
		for _, is := range ps.Issues {
			if is.Check == "decide" {
				t.Errorf("patrol.json 里不该再有它：%v", patrolFPs(ps.Issues))
			}
		}
	}
}

// 同一问题不重报；消失了记已恢复；再出现重新报。检查本身没做成时，那一类上次的问题照旧记着、不算恢复。
func TestPatrolNoRepeat(t *testing.T) {
	e := newPatrolFixture(t)
	e.launchd += "-\t1\t" + LaunchdPrefix + "backup\n"
	e.free = 10 << 30
	r := e.patrol()
	if got := patrolFPs(r.New); strings.Join(got, ",") != "disk,launchd:"+LaunchdPrefix+"backup" {
		t.Fatalf("第一次：%v", got)
	}
	before := e.summary()

	e.now = e.now.Add(time.Hour)
	r = e.patrol()
	if len(r.New) != 0 || len(r.Old) != 2 || len(r.Gone) != 0 {
		t.Fatalf("第二次不该重报：新 %v 旧 %v", patrolFPs(r.New), patrolFPs(r.Old))
	}
	if !r.Old[0].FirstSeen.Equal(e.now.Add(-time.Hour)) {
		t.Errorf("首次发现时间应保留：%v", r.Old[0].FirstSeen)
	}
	if e.summary() != before || len(e.analyzed) != 1 {
		t.Errorf("重复的问题不该再写汇总或分析（分析 %d 次）：\n%s", len(e.analyzed), e.summary())
	}

	// launchctl 读不了：launchd 那条照旧记着；磁盘恢复了：算已恢复
	e.lderr = errors.New("exit status 5")
	e.free = 100 << 30
	r = e.patrol()
	if len(r.Gone) != 1 || r.Gone[0].Fingerprint != "disk" || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "launchctl list") {
		t.Fatalf("恢复 %v 提示 %v", patrolFPs(r.Gone), r.Notes)
	}
	if !strings.Contains(e.summary(), "巡逻：已恢复 1 个之前报过的问题：磁盘剩余不到 20 GB") {
		t.Errorf("汇总应记已恢复：\n%s", e.summary())
	}
	ps, _ := e.a.LoadPatrol()
	if got := patrolFPs(ps.Issues); strings.Join(got, ",") != "launchd:"+LaunchdPrefix+"backup" {
		t.Errorf("检查没做成的那类应照旧记着：%v", got)
	}

	// 磁盘又不够了：重新报
	e.lderr = nil
	e.free = 1 << 30
	r = e.patrol()
	if got := patrolFPs(r.New); strings.Join(got, ",") != "disk" {
		t.Errorf("恢复后再出现应重新报：%v", got)
	}
	if strings.Count(e.summary(), "巡逻发现：磁盘剩余不到 20 GB") != 2 {
		t.Errorf("汇总应有两次磁盘发现：\n%s", e.summary())
	}
}

// 分析：只在有新异常时启动；每天最多 3 次，第二天重新算；暂停、冷却、熔断、额度到 90%、开关关着、上一次还在跑都不启动。
func TestPatrolAnalyzeLimits(t *testing.T) {
	e := newPatrolFixture(t)
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), "## 等用户决定\n\n- [ ] 选 A 还是 B\n")
	if r := e.patrol(); len(r.New) != 1 || len(e.analyzed) != 0 || r.Analysis != "" {
		t.Fatalf("只有拍板条目不该分析：%v %q", patrolFPs(r.New), r.Analysis)
	}
	code := 1
	next := func() *PatrolReport { // 每次换一个服务失败，就是一个新异常
		code++
		e.launchd = "-\t1\t" + LaunchdPrefix + "x" + strconv.Itoa(code) + "\n"
		e.now = e.now.Add(time.Hour)
		return e.patrol()
	}
	r := next()
	if len(e.analyzed) != 1 || !strings.Contains(r.Analysis, "今天第 1/3 次") {
		t.Fatalf("有新异常应分析：%q", r.Analysis)
	}
	// prompt：只读、带新异常和证据，不含已经报过的拍板条目
	if p := e.analyzed[0]; !strings.Contains(p, "只读分析") || !strings.Contains(p, "1. launchd 服务 "+LaunchdPrefix+"x2 上次退出码 1\n   证据：launchctl list：") ||
		strings.Contains(p, "拍板") {
		t.Errorf("分析 prompt：\n%s", p)
	}
	if !strings.Contains(e.summary(), "巡逻：已启动一次 claude -p 只读分析 1 个新异常的原因（任务 fake-analysis，今天第 1/3 次）") {
		t.Errorf("汇总应记启动了分析：\n%s", e.summary())
	}
	for i := 2; i <= 3; i++ {
		if r := next(); len(e.analyzed) != i || !strings.Contains(r.Analysis, "今天第 "+strconv.Itoa(i)+"/3 次") {
			t.Fatalf("第 %d 次：分析 %d 次，%q", i, len(e.analyzed), r.Analysis)
		}
	}
	if r := next(); len(e.analyzed) != 3 || !strings.Contains(r.Analysis, "今天已经分析过 3 次") {
		t.Fatalf("第 4 次应不再分析：%q", r.Analysis)
	}
	e.now = time.Date(2026, 10, 12, 0, 30, 0, 0, time.Local)
	if r := next(); len(e.analyzed) != 4 || !strings.Contains(r.Analysis, "今天第 1/3 次") {
		t.Fatalf("第二天应重新计数：%q", r.Analysis)
	}

	block := []struct {
		why   string
		set   func()
		unset func()
	}{
		{"patrol_enabled=false", func() { e.a.Cfg.PatrolEnabled = false }, func() { e.a.Cfg.PatrolEnabled = true }},
		{"已暂停", func() { e.a.Cfg.Paused = true }, func() { e.a.Cfg.Paused = false }},
		{"用量冷却中", func() { e.st.ExtendCooldown("", e.now.Add(2*time.Hour)) }, func() { delete(e.st.BackendLimits, "claude") }},
		{"连环失败熔断中", func() { e.st.BreakerUntil = e.now.Add(2 * time.Hour) }, func() { e.st.BreakerUntil = time.Time{} }},
		{"订阅额度到了后台上限", func() {
			e.st.SetUsageOf("", &backend.UsageInfo{CheckedAt: e.now, Windows: []backend.UsageWindow{{Label: "session", Used: 91}}})
		}, func() { e.st.SetUsageOf("", nil) }},
		{"上一次分析还在跑", func() {
			e.st.Jobs = []*state.Job{{ID: "pa", Kind: state.KindPatrol, Status: state.StatusRunning, StartedAt: e.now}}
		}, func() { e.st.Jobs = nil }},
	}
	for _, b := range block {
		b.set()
		if r := next(); len(e.analyzed) != 4 || !strings.Contains(r.Analysis, b.why) {
			t.Errorf("%s：不该分析（%d 次）：%q", b.why, len(e.analyzed), r.Analysis)
		}
		b.unset()
	}
	// 启动失败：不计次数
	e.a.AnalyzePatrol = func(*state.State, string) (*state.Job, error) { return nil, errors.New("终端版 Claude 未登录") }
	if r := next(); !strings.Contains(r.Analysis, "没启动 claude -p 分析：终端版 Claude 未登录") {
		t.Errorf("启动失败：%q", r.Analysis)
	}
	if ps, _ := e.a.LoadPatrol(); ps.AnalysisCount != 1 {
		t.Errorf("启动失败不计次数：%d", ps.AnalysisCount)
	}
}

// 新项目第一版被门退回（会话在 projects_root，续跑不了）：tick 下次直接接手，巡逻不写接手待做——
// 写了反而挡住：第一版只挑第一条，孤儿扫描见到接手待做也让开（11-12 repo-tidy 因此卡住）。
func TestPatrolTakeOverTodoLeavesFirstVersionToTick(t *testing.T) {
	e := newPatrolFixture(t)
	home := e.a.Home
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	testkit.GitT(t, home, proj, "init", "-q", "-b", "main")
	orig := "第一条 —— 要什么；验收：看 README"
	backlog := "# 待办\n\n## 待做\n\n- [ ] " + orig + "（进行中：agent/1112-first 11-12 16:49）（方向：用户定：随便）\n- [ ] 第二条\n\n## 已完成\n"
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), backlog)
	testkit.GitT(t, home, proj, "add", ".")
	testkit.GitT(t, home, proj, "commit", "-q", "-m", "init")
	j := &state.Job{ID: "1112-164509-新项目", Project: "p", ProjectDir: proj, Title: "第一条", Fingerprint: bl.Fingerprint("- [ ] " + orig + "（方向：用户定：随便）"),
		Status: state.StatusInterrupted, NewReport: &state.NewReport{Root: e.root}, Summary: "门没过，退回第 1 轮",
		StartedAt: e.now.Add(-time.Hour), EndedAt: patrolAgo(e.now, 10*time.Minute)}
	e.st.Jobs = []*state.Job{j}
	is := patrolFind(t, e.patrol().New, "job:"+j.ID)
	if !strings.Contains(is.Action, "下一次 tick 直接接手") {
		t.Fatalf("处理：%s", is.Action)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "docs", "BACKLOG.md")); string(b) != backlog {
		t.Fatalf("不该写接手待做：\n%s", b)
	}
}

// 中断/失败/超时的待做任务：BACKLOG 里那一条还标着进行中，就在「待做」末尾写一条接手的待做并提交；
// 新条目自己能被挑中（不含进行中、卡住标记）；不重复写；有别的任务在接手、BACKLOG 有没提交的改动时不写。
func TestPatrolTakeOverTodo(t *testing.T) {
	e := newPatrolFixture(t)
	home := e.a.Home
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	testkit.GitT(t, home, proj, "init", "-q", "-b", "main")
	orig := "修登录页 —— 要什么；验收：正文提到卡住和进行中这两个词"
	backlog := "# 待办\n\n## 待做\n\n从上往下做。\n\n- [ ] " + orig + "（进行中：agent/1011-login 10-11 09:00）\n- [ ] 别的条目\n<!-- - [ ] 模板 -->\n\n## 已完成\n"
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), backlog)
	testkit.GitT(t, home, proj, "add", ".")
	testkit.GitT(t, home, proj, "commit", "-q", "-m", "init")
	fp := bl.Fingerprint("- [ ] " + orig)
	j := &state.Job{ID: "1011-090000-p", Project: "p", ProjectDir: proj, Title: "修登录页", Fingerprint: fp, Status: state.StatusTimeout,
		Summary: "运行超过 4h，已终止", StartedAt: e.now.Add(-5 * time.Hour), EndedAt: patrolAgo(e.now, time.Hour)}
	e.st.Jobs = []*state.Job{j}

	r := e.patrol()
	is := patrolFind(t, r.New, "job:"+j.ID)
	if !strings.Contains(is.Action, "已在 p 的 BACKLOG「待做」末尾写了「接手中断的「修登录页」」并提交") {
		t.Fatalf("处理：%s", is.Action)
	}
	b, _ := os.ReadFile(filepath.Join(proj, "docs", "BACKLOG.md"))
	items := bl.ParseBacklog(string(b))
	if len(items) != 3 || items[1].Title != "别的条目" {
		t.Fatalf("接手的待做应放在「待做」最后一个条目之后：\n%s", b)
	}
	if it := items[2]; !it.Eligible() || !strings.HasPrefix(it.Title, "接手中断的「修登录页」") ||
		!strings.Contains(it.Text, j.ID) || !strings.Contains(it.Text, "（分支 agent/1011-login 不存在）") {
		t.Errorf("接手的待做：%+v", it)
	}
	if !items[0].InProgress {
		t.Errorf("原条目的进行中标记不该动")
	}
	if st := testkit.GitT(t, home, proj, "status", "--porcelain"); st != "" {
		t.Errorf("应已提交：%s", st)
	}
	if msg := testkit.GitT(t, home, proj, "log", "-1", "--format=%s"); msg != "待办：接手中断的「修登录页」（巡逻）" {
		t.Errorf("提交说明：%s", msg)
	}
	if !strings.Contains(e.summary(), "处理：已在 p 的 BACKLOG") {
		t.Errorf("汇总应带处理说明：\n%s", e.summary())
	}

	// patrol.json 丢了，同一个任务又被当成新问题：BACKLOG 里已经有它，不再写
	os.Remove(e.a.PatrolPath())
	r = e.patrol()
	if is := patrolFind(t, r.New, "job:"+j.ID); !strings.Contains(is.Action, "已经有提到任务 "+j.ID) {
		t.Errorf("不该重复写：%s", is.Action)
	}
	if b2, _ := os.ReadFile(filepath.Join(proj, "docs", "BACKLOG.md")); string(b2) != string(b) {
		t.Errorf("BACKLOG 不该再变")
	}

	// 另一个任务：之后已有任务在接手
	j2 := &state.Job{ID: "1011-100000-p", Project: "p", ProjectDir: proj, Title: "修登录页", Fingerprint: fp, Status: state.StatusFailed,
		StartedAt: e.now.Add(-3 * time.Hour), EndedAt: patrolAgo(e.now, 2*time.Hour)}
	e.st.Jobs = append(e.st.Jobs, j2)
	if is := patrolFind(t, e.patrol().New, "job:"+j2.ID); !strings.Contains(is.Action, "之后已有任务 "+j.ID+" 在接手") {
		t.Errorf("有后来的任务时不该写：%s", is.Action)
	}

	// BACKLOG 有没提交的改动：不写、不动文件
	j3 := &state.Job{ID: "1011-110000-p", Project: "p", ProjectDir: proj, Title: "修登录页", Fingerprint: fp, Status: state.StatusInterrupted,
		StartedAt: e.now.Add(-time.Hour), EndedAt: patrolAgo(e.now, time.Minute)}
	e.st.Jobs = append(e.st.Jobs, j3)
	dirty := string(b) + "用户正在改\n"
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), dirty)
	if is := patrolFind(t, e.patrol().New, "job:"+j3.ID); !strings.Contains(is.Action, "BACKLOG 有没提交的改动") {
		t.Errorf("BACKLOG 有改动时不该写：%s", is.Action)
	}
	if b3, _ := os.ReadFile(filepath.Join(proj, "docs", "BACKLOG.md")); string(b3) != dirty {
		t.Errorf("BACKLOG 有改动时不该动它")
	}
	testkit.GitT(t, home, proj, "checkout", "--", "docs/BACKLOG.md")

	// 接手的待做自己又没做完：只报不写（不一层套一层）
	takeFp := bl.Fingerprint("- [ ] " + items[2].Text)
	j4 := &state.Job{ID: "1011-113000-p", Project: "p", ProjectDir: proj, Title: items[2].Title, Fingerprint: takeFp, Status: state.StatusTimeout,
		StartedAt: e.now.Add(-time.Hour), EndedAt: patrolAgo(e.now, 2*time.Minute)}
	e.st.Jobs = append(e.st.Jobs, j4)
	if is := patrolFind(t, e.patrol().New, "job:"+j4.ID); !strings.Contains(is.Action, "不再写新的接手条目") {
		t.Errorf("接手的待做失败不该再写：%s", is.Action)
	}
	// 暂停时只报不写
	e.a.Cfg.Paused = true
	j5 := &state.Job{ID: "1011-114000-p", Project: "p", ProjectDir: proj, Title: "修登录页", Fingerprint: fp, Status: state.StatusFailed,
		StartedAt: e.now.Add(-time.Hour), EndedAt: patrolAgo(e.now, time.Minute)}
	e.st.Jobs = append(e.st.Jobs, j5)
	if is := patrolFind(t, e.patrol().New, "job:"+j5.ID); !strings.Contains(is.Action, "已暂停（paused=true），只报不写") {
		t.Errorf("暂停时不该写：%s", is.Action)
	}
	if b4, _ := os.ReadFile(filepath.Join(proj, "docs", "BACKLOG.md")); string(b4) != string(b) {
		t.Errorf("接手失败、暂停时 BACKLOG 都不该变：\n%s", b4)
	}
}

// 写接手待做时和别的会话共用 with-lock.sh 的目录锁：持有进程不在了的旧锁清掉照常写，有人拿着就等到超时、不写；写完把锁放掉。
func TestPatrolTakeOverLock(t *testing.T) {
	e := newPatrolFixture(t)
	home := e.a.Home
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	testkit.GitT(t, home, proj, "init", "-q", "-b", "main")
	e.write(filepath.Join(proj, "docs", "BACKLOG.md"), "## 待做\n\n- [ ] 甲（进行中：agent/x 10-11 09:00）\n")
	testkit.GitT(t, home, proj, "add", ".")
	testkit.GitT(t, home, proj, "commit", "-q", "-m", "init")
	lock := filepath.Join(home, ".claude", "state", "locks", "p-main")
	job := func(id string) *state.Job {
		return &state.Job{ID: id, Project: "p", ProjectDir: proj, Title: "甲", Fingerprint: bl.Fingerprint("- [ ] 甲"), Status: state.StatusFailed,
			StartedAt: e.now.Add(-time.Hour), EndedAt: patrolAgo(e.now, time.Minute)}
	}

	// 有人拿着（持有进程就是测试自己，活着）：等到超时，不写
	old := bl.MainLockWait
	bl.MainLockWait = 300 * time.Millisecond
	defer func() { bl.MainLockWait = old }()
	os.MkdirAll(lock, 0o755)
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	e.st.Jobs = []*state.Job{job("j-held")}
	if is := patrolFind(t, e.patrol().New, "job:j-held"); !strings.Contains(is.Action, "还拿不到锁") {
		t.Errorf("锁被拿着时不该写：%s", is.Action)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("别人的锁不该被清掉：%v", err)
	}

	// 持有进程已经不在：清掉旧锁照常写，写完放掉
	dead := exec.Command("/usr/bin/true")
	dead.Run()
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(dead.Process.Pid)+"\n"), 0o644)
	e.st.Jobs = []*state.Job{job("j-stale")}
	if is := patrolFind(t, e.patrol().New, "job:j-stale"); !strings.Contains(is.Action, "并提交") {
		t.Errorf("旧锁应清掉照常写：%s", is.Action)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("写完应放掉锁：%v", err)
	}
}

// 消失的问题：真的好了才记「已恢复」（推送失败之后同仓库推成功了也算），拍板条目记「已处理」；过了 24 小时窗口的任务失败、
// 推送失败，前一天的报告、卡住的任务已结束，都悄悄去掉，不写「已恢复」。
func TestPatrolGone(t *testing.T) {
	e := newPatrolFixture(t)
	n := e.now
	push := filepath.Join(e.a.Home, ".claude", "state", "autopush.log")
	e.write(push, "2026-10-11 09:00:00 FAIL /p/a/.git: x\n")
	e.write(e.nightlyFile("-backup"), "备份：1 个仓库已推到 GitHub 私有备份，**1 个失败**\n")
	os.Remove(e.nightlyFile("-night"))
	proj := filepath.Join(e.root, "p")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	backlog := filepath.Join(proj, "docs", "BACKLOG.md")
	e.write(backlog, "## 等用户决定\n\n- [ ] 选 A 还是 B\n")
	e.st.Jobs = []*state.Job{
		{ID: "f1", Project: "q", Title: "失败的", Status: state.StatusFailed, StartedAt: n.Add(-2 * time.Hour), EndedAt: patrolAgo(n, time.Hour)},
		{ID: "s1", Project: "q", Title: "卡住会好的", Status: state.StatusRunning, SessionID: "s-1", StartedAt: n.Add(-2 * time.Hour)},
		{ID: "s2", Project: "q", Title: "卡住后结束的", Status: state.StatusRunning, SessionID: "s-2", StartedAt: n.Add(-2 * time.Hour)},
	}
	r := e.patrol()
	if got := strings.Join(patrolFPs(r.New), ","); got != "autopush:/p/a/.git@2026-10-11T09:00:00,backup:2026-10-11,decide:p:"+patrolFPs(r.New)[2][len("decide:p:"):]+",job:f1,report:night:2026-10-11,stall:s1,stall:s2" {
		t.Fatalf("第一次：%s", got)
	}

	// 同一天：备份报告重写成功了 → 已恢复；拍板条目挪走了 → 已处理；s1 又在长 → 已恢复；s2 结束了 → 悄悄去掉；
	// 推送成功过 → 旧的一段已恢复，之后又 FAIL → 新的一段是新问题
	e.now = n.Add(2 * time.Hour)
	e.write(e.nightlyFile("-backup"), "备份：2 个仓库已推到 GitHub 私有备份\n")
	e.write(backlog, "## 等用户决定\n\n## 待做\n\n- [ ] 选 A 还是 B —— 用户选了 A\n")
	tr := filepath.Join(e.a.Home, ".claude", "projects", "-x", "s-1.jsonl")
	e.write(tr, "{}\n")
	os.Chtimes(tr, e.now.Add(-time.Minute), e.now.Add(-time.Minute))
	e.st.Jobs[2].Status, e.st.Jobs[2].EndedAt = state.StatusDone, patrolAgo(e.now, time.Minute)
	f, _ := os.OpenFile(push, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("2026-10-11 09:30:00 ok /p/a/.git\n2026-10-11 13:00:00 FAIL /p/a/.git: 又失败了\n")
	f.Close()
	r = e.patrol()
	if got := strings.Join(patrolFPs(r.New), ","); got != "autopush:/p/a/.git@2026-10-11T13:00:00" {
		t.Errorf("恢复后再 FAIL 应是新问题：%s", got)
	}
	var gone []string
	for _, is := range r.Gone {
		gone = append(gone, is.gone+" "+is.Fingerprint)
	}
	sort.Strings(gone)
	if got := strings.Join(gone, ","); !strings.HasPrefix(got, "已处理 decide:p:") ||
		!strings.HasSuffix(got, ",已恢复 autopush:/p/a/.git@2026-10-11T09:00:00,已恢复 backup:2026-10-11,已恢复 stall:s1") {
		t.Errorf("消失的：%s", got)
	}
	sum := e.summary()
	if !strings.Contains(sum, "巡逻：已恢复 3 个之前报过的问题：自动备份推送失败：a；今天的备份有 1 个仓库推送失败；任务疑似卡住：q「卡住会好的」") ||
		!strings.Contains(sum, "巡逻：1 条等用户决定的已处理（不在那一节了）：p 等你决定：选 A 还是 B") ||
		strings.Contains(sum, "卡住后结束的」\n") || strings.Count(sum, "巡逻：已恢复") != 1 {
		t.Errorf("汇总：\n%s", sum)
	}

	// 过了 24 小时、换了一天：任务失败、前一天缺夜班报告、推送失败都只是过期，不写已恢复
	e.now = time.Date(2026, 10, 12, 14, 0, 0, 0, time.Local)
	e.st.Jobs[1].Status, e.st.Jobs[1].EndedAt = state.StatusDone, patrolAgo(e.now, time.Hour)
	e.write(e.nightlyFile(""), "早报\n")
	e.write(e.nightlyFile("-night"), "夜班\n")
	r = e.patrol()
	if len(r.New) != 0 || len(r.Gone) != 0 {
		t.Errorf("过期的不该算恢复：新 %v 恢复 %v", patrolFPs(r.New), patrolFPs(r.Gone))
	}
	if ps, _ := e.a.LoadPatrol(); len(ps.Issues) != 0 {
		t.Errorf("过期的应从 patrol.json 去掉：%v", patrolFPs(ps.Issues))
	}
	if strings.Contains(e.summary(), "巡逻：已恢复") {
		t.Errorf("不该多写已恢复：\n%s", e.summary())
	}
}

// tick 里的巡逻：开着才巡逻，离上次满一小时才再巡逻；时钟往回拨过也巡逻。
func TestPatrolIfDue(t *testing.T) {
	e := newPatrolFixture(t)
	runs := func() int { return strings.Count(e.out.String(), "巡逻：") }
	e.a.PatrolIfDue(e.st)
	if runs() != 1 {
		t.Fatalf("第一次应巡逻：\n%s", e.out)
	}
	e.now = e.now.Add(59 * time.Minute)
	e.a.PatrolIfDue(e.st)
	if runs() != 1 {
		t.Fatalf("不满一小时不该巡逻：\n%s", e.out)
	}
	e.now = e.now.Add(time.Minute)
	e.a.PatrolIfDue(e.st)
	if runs() != 2 {
		t.Fatalf("满一小时应巡逻：\n%s", e.out)
	}
	e.now = e.now.Add(-3 * time.Hour)
	e.a.PatrolIfDue(e.st)
	if runs() != 3 {
		t.Fatalf("时钟往回拨过应巡逻：\n%s", e.out)
	}
	e.a.Cfg.PatrolEnabled = false
	e.now = e.now.Add(5 * time.Hour)
	e.a.PatrolIfDue(e.st)
	if runs() != 3 {
		t.Fatalf("patrol_enabled=false 不该巡逻：\n%s", e.out)
	}
	if s := e.a.PatrolStatus(); !strings.Contains(s, "patrol_enabled=false") || !strings.Contains(s, "没有问题") {
		t.Errorf("status 行：%s", s)
	}
	// patrol.json 坏了：备份、从空的开始
	os.WriteFile(e.a.PatrolPath(), []byte("{坏"), 0o644)
	e.a.Cfg.PatrolEnabled = true
	e.a.PatrolIfDue(e.st)
	if bad, _ := filepath.Glob(e.a.PatrolPath() + ".bad-*"); len(bad) != 1 || runs() < 4 {
		t.Errorf("坏文件应备份后照常巡逻：%v\n%s", bad, e.out)
	}
}

// 第 2 轮审查的反例：推送连续失败超过 24 小时不随窗口重报；最近一段的最后一次 FAIL 在 24 小时前不报；
// 补跑的复盘还没写出早报时不算恢复；某个额度窗口重置了、别的仍 ≥90% 不算恢复；服务卸掉了不算恢复、退出码回到 0 才算。
func TestPatrolGoneEdges(t *testing.T) {
	e := newPatrolFixture(t)
	push := filepath.Join(e.a.Home, ".claude", "state", "autopush.log")
	start := time.Date(2026, 10, 10, 6, 0, 0, 0, time.Local)
	var b strings.Builder
	b.WriteString("2026-10-08 10:00:00 FAIL /p/old/.git: 两天前\n")
	for i := 0; i < 40; i++ {
		b.WriteString(start.Add(time.Duration(i)*time.Hour).Format("2006-01-02 15:04:05") + " FAIL /p/a/.git: non-fast-forward\n")
	}
	e.write(push, b.String())
	os.Remove(e.nightlyFile(""))
	e.launchd += "-\t1\t" + LaunchdPrefix + "backup\n-\t1\t" + LaunchdPrefix + "gone\n"
	e.st.SetUsageOf("", &backend.UsageInfo{CheckedAt: start.Add(29 * time.Hour), Windows: []backend.UsageWindow{
		{Label: "session", Used: 95, ResetsAt: func() *time.Time { t := start.Add(30*time.Hour + 30*time.Minute); return &t }()},
		{Label: "week", Used: 92, ResetsAt: func() *time.Time { t := start.Add(100 * time.Hour); return &t }()},
	}})
	e.now = start.Add(30 * time.Hour) // 10-11 12:00
	r := e.patrol()
	want := "autopush:/p/a/.git@2026-10-10T06:00:00,launchd:" + LaunchdPrefix + "backup,launchd:" + LaunchdPrefix + "gone,report:review:2026-10-11,usage:session@2026-10-11T12:30:week@2026-10-14T10:00"
	if got := strings.Join(patrolFPs(r.New), ","); got != want {
		t.Fatalf("第一次\n got %s\nwant %s", got, want)
	}

	// 一小时后：推送还在失败（起点不变，不重报）；复盘在补跑、早报还没写出来；会话窗口重置了、本周仍 92%；
	// gone 服务卸掉了；backup 退出码回到 0
	e.now = e.now.Add(time.Hour)
	e.st.Jobs = []*state.Job{{ID: "rv", Kind: config.KindReview, Status: state.StatusRunning, StartedAt: e.now.Add(-10 * time.Minute)}}
	e.launchd = strings.Replace(strings.Replace(e.launchd, "-\t1\t"+LaunchdPrefix+"gone\n", "", 1), "-\t1\t"+LaunchdPrefix+"backup", "-\t0\t"+LaunchdPrefix+"backup", 1)
	r = e.patrol()
	if got := strings.Join(patrolFPs(r.New), ","); got != "usage:week@2026-10-14T10:00" {
		t.Errorf("只有本周额度是新指纹：%s", got)
	}
	var gone []string
	for _, is := range r.Gone {
		gone = append(gone, is.gone+" "+is.Fingerprint)
	}
	if got := strings.Join(gone, ","); got != "已恢复 launchd:"+LaunchdPrefix+"backup" {
		t.Errorf("只有退出码回到 0 的服务算恢复：%s", got)
	}
	ps, _ := e.a.LoadPatrol()
	if got := strings.Join(patrolFPs(ps.Issues), ","); got != "autopush:/p/a/.git@2026-10-10T06:00:00,report:review:2026-10-11,usage:week@2026-10-14T10:00" {
		t.Errorf("补跑中的早报应照旧记着：%s", got)
	}
	if s := e.summary(); strings.Count(s, "巡逻发现：自动备份推送失败") != 1 || strings.Contains(s, "已恢复 1 个之前报过的问题：08:00") ||
		strings.Contains(s, "订阅额度快用完") {
		t.Errorf("汇总：\n%s", s)
	}

	// 早报写出来了：这才算恢复
	e.now = e.now.Add(time.Hour)
	e.st.Jobs[0].Status, e.st.Jobs[0].EndedAt = state.StatusDone, patrolAgo(e.now, time.Minute)
	e.write(e.nightlyFile(""), "早报\n")
	r = e.patrol()
	if len(r.Gone) != 1 || r.Gone[0].Fingerprint != "report:review:2026-10-11" || r.Gone[0].gone != goneFixed || len(r.New) != 0 {
		t.Errorf("早报写出来后应算恢复：新 %v 恢复 %v", patrolFPs(r.New), patrolFPs(r.Gone))
	}
}

// patrol.json 的键：只有带标签的字段落盘，内部标记（job、jobID、quiet、handled、gone）不写，字段顺序也固定。
func TestPatrolJSONKeys(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	is := PatrolIssue{Fingerprint: "f", Check: "c", Title: "t", Evidence: []string{"e"}, Action: "a", Anomaly: true, FirstSeen: at, LastSeen: at,
		job: &state.Job{ID: "j"}, jobID: "j", quiet: true, handled: true, gone: "g"}
	b, err := json.Marshal(is)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fingerprint":"f","check":"c","title":"t","evidence":["e"],"action":"a","anomaly":true,"first_seen":"2026-10-11T12:00:00Z","last_seen":"2026-10-11T12:00:00Z"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

// 没配 autopush_log 就不查：日志在默认位置也不读。
func TestPatrolAutopushOffByDefault(t *testing.T) {
	e := newPatrolFixture(t)
	e.a.Cfg.AutopushLog = ""
	e.write(filepath.Join(e.a.Home, ".claude", "state", "autopush.log"), "2026-10-11 09:00:00 FAIL /p/a/.git: x\n")
	for _, is := range e.patrol().New {
		if is.Check == "autopush" {
			t.Errorf("没配路径不该报：%+v", is)
		}
	}
}
