package tests

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/patrol"
	"agentq/internal/state"
)

// 巡逻的单元测试：进程内跑 runPatrol，launchctl、磁盘、分析任务都换成假的，HOME 和 projects_root 是临时目录。

type patrolEnv struct {
	t        *testing.T
	a        *App
	out      *bytes.Buffer
	root     string
	now      time.Time
	launchd  string // launchctl list 的输出
	lderr    error
	free     uint64
	analyzed []string // 每次启动分析时的 prompt
	st       *State
}

func newPatrolEnv(t *testing.T) *patrolEnv {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	// 接手待做要在临时仓库里提交：临时 HOME 下的 git 配置给出身份，也没有全局钩子
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = t\n\temail = t@t\n[commit]\n\tgpgsign = false\n"), 0o644)
	var out bytes.Buffer
	a, err := newLegacyConfigApp(t, home, &out)
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg.ProjectsRoot = root
	a.Cfg.NightEnabled, a.Cfg.ReviewEnabled = true, true
	e := &patrolEnv{t: t, a: a, out: &out, root: root, free: 100 << 30, st: &State{},
		now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)}
	e.launchd = "PID\tStatus\tLabel\n-\t0\t" + patrol.LaunchdPrefix + "agentq\n-\t0\t" + patrol.LaunchdPrefix + "agentq.night\n-\t0\t" + patrol.LaunchdPrefix + "agentq.review\n" +
		"123\t1\tcom.apple.something\n"
	a.Now = func() time.Time { return e.now }
	a.Service.Launchctl = func(args ...string) ([]byte, error) {
		if len(args) != 1 || args[0] != "list" {
			t.Fatalf("巡逻只该执行 launchctl list，收到 %v", args)
		}
		return []byte(e.launchd), e.lderr
	}
	a.Patrol.DiskFree = func(string) (uint64, uint64, error) { return e.free, 500 << 30, nil }
	a.Patrol.AnalyzePatrol = func(st *State, prompt string) (*Job, error) {
		e.analyzed = append(e.analyzed, prompt)
		return &Job{ID: "fake-analysis"}, nil
	}
	// 当天早报和夜班报告都在
	e.write(e.nightlyFile(""), "早报\n")
	e.write(e.nightlyFile("-night"), "夜班\n")
	return e
}

func (e *patrolEnv) nightlyFile(suffix string) string {
	return filepath.Join(e.a.Home, ".claude", "nightly", e.now.Format("2006-01-02")+suffix+".md")
}

func (e *patrolEnv) write(path, text string) {
	e.t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *patrolEnv) patrol() *patrol.PatrolReport {
	e.t.Helper()
	ps, err := e.a.Patrol.LoadPatrol()
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := e.a.Patrol.RunPatrol(e.st, ps)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// 集成：agentq patrol 发现异常 → 带证据写汇总、经任务外壳启动一次分析（假 claude）；分析结束后 tick 收尾，结论整段写进汇总；
// 再巡逻不重报、不再分析；status 显示巡逻。launchctl list 是真的（只读），所以只断言自己造的问题。
func TestIntegrationPatrol(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"patrol_enabled": true})
	day := time.Now().Format("2006-01-02")
	os.MkdirAll(filepath.Join(e.home, ".claude", "nightly"), 0o755)
	os.WriteFile(filepath.Join(e.home, ".claude", "nightly", day+"-backup.md"),
		[]byte("备份：1 个仓库已推到 GitHub 私有备份，**1 个失败**\n\n- alpha：**推送失败**：cannot lock ref\n"), 0o644)

	out := e.mustRun("patrol")
	if !strings.Contains(out, "今天的备份有 1 个仓库推送失败") || !strings.Contains(out, "证据：") ||
		!strings.Contains(out, "分析：已启动一次 claude -p 只读分析") {
		t.Fatalf("agentq patrol：\n%s", out)
	}
	var j *Job
	for _, x := range e.state().Jobs {
		if x.Kind == state.KindPatrol {
			j = x
		}
	}
	if j == nil || j.Project != patrol.PatrolJobName || j.ProjectDir != e.root {
		t.Fatalf("应有一个巡逻分析任务：%+v", e.state().Jobs)
	}
	prompt, _ := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if !strings.HasPrefix(string(prompt), patrol.PatrolPromptHead) || !strings.Contains(string(prompt), "- alpha：**推送失败**：cannot lock ref") {
		t.Errorf("分析 prompt：\n%s", prompt)
	}
	if !strings.Contains(e.nightly(), "巡逻发现：今天的备份有 1 个仓库推送失败；证据：") {
		t.Errorf("汇总应有巡逻发现：\n%s", e.nightly())
	}
	waitExit(t, j.PID, 15*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j.PID)))
	if !strings.Contains(string(fakeLog), "ARG[--name]\nARG[agentq 巡逻分析 agentq patrol 分析]") || !strings.Contains(string(fakeLog), "ARG[--max-budget-usd]\nARG[2]") {
		t.Errorf("假 claude 收到的参数：\n%s", fakeLog)
	}

	out = e.mustRun("tick")
	if strings.Contains(out, "巡逻：") {
		t.Errorf("刚巡逻过，tick 不该再巡逻：\n%s", out)
	}
	if j := e.jobByTitle("agentq patrol 分析"); j.Status != state.StatusDone || j.Summary != "做完了。 已合并，提交 abc1234；用户看 README。" {
		t.Errorf("分析任务应跑完、摘要是整段结论：%+v", j)
	}
	if n := e.nightly(); !strings.Contains(n, "巡逻分析「agentq patrol 分析」跑完：做完了。 已合并，提交 abc1234") {
		t.Errorf("汇总应有分析结论：\n%s", n)
	}

	out = e.mustRun("patrol")
	if !strings.Contains(out, "新 0 个") || strings.Contains(out, "分析：") || !strings.Contains(out, "之前报过、还没消失") {
		t.Errorf("再巡逻不该重报、不该再分析：\n%s", out)
	}
	if n := e.nightly(); strings.Count(n, "巡逻发现：今天的备份") != 1 {
		t.Errorf("汇总不该重复：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "巡逻：上次 ") {
		t.Errorf("status 应显示巡逻：\n%s", out)
	}
}

// 集成：tick 里巡逻一次，紧接着的 tick 不巡逻（patrol.json 记了时间）。
func TestIntegrationTickPatrols(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"patrol_enabled": true})
	out := e.mustRun("tick")
	if !strings.Contains(out, "巡逻：") {
		t.Fatalf("第一次 tick 应巡逻：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "patrol.json")); err != nil {
		t.Errorf("应写 patrol.json：%v", err)
	}
	if out := e.mustRun("tick"); strings.Contains(out, "巡逻：") {
		t.Errorf("一小时内不该再巡逻：\n%s", out)
	}
}
