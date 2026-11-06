package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/backend/claude"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/runner"
	"agentq/internal/state"
)

var progressStart = time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local)

func at(min int) time.Time { return progressStart.Add(time.Duration(min) * time.Minute) }

func tools(n int, min int, name string) []backend.ToolEvent {
	out := make([]backend.ToolEvent, n)
	for i := range out {
		out[i] = backend.ToolEvent{At: at(min), Name: name}
	}
	return out
}

func hitSignals(hits []backend.ProgressHit) string {
	var s []string
	for _, h := range hits {
		s = append(s, h.Signal)
	}
	return strings.Join(s, ",")
}

func TestEvalProgressRunaway(t *testing.T) {
	main := []backend.ToolEvent{{At: at(29), Name: "Write"}}
	logsOf := func(n int) [][]backend.ToolEvent {
		var subs [][]backend.ToolEvent
		for range n {
			subs = append(subs, []backend.ToolEvent{{At: at(28), Name: "Read"}})
		}
		return subs
	}
	if got := hitSignals(backend.EvalProgress(main, logsOf(24), at(0), at(30))); got != "" {
		t.Errorf("24 个日志不该报：%s", got)
	}
	h := backend.EvalProgress(main, logsOf(25), at(0), at(30))
	if hitSignals(h) != backend.SigRunaway || !strings.Contains(h[0].Evidence, "子代理 25 个，其中 0 个再派子代理") {
		t.Errorf("25 个日志应报：%+v", h)
	}
	nestedOf := func(n int) [][]backend.ToolEvent {
		subs := logsOf(n)
		for i := range subs {
			subs[i] = []backend.ToolEvent{{At: at(28), Name: "Agent"}, {At: at(28), Name: "Task"}} // 一个日志只算一次
		}
		return subs
	}
	if got := hitSignals(backend.EvalProgress(main, nestedOf(7), at(0), at(30))); got != "" {
		t.Errorf("7 个再派不该报：%s", got)
	}
	if got := hitSignals(backend.EvalProgress(main, nestedOf(8), at(0), at(30))); got != backend.SigRunaway {
		t.Errorf("8 个再派应报：%s", got)
	}
	// 开始之前的日志（续跑接着写旧会话）不算
	if got := hitSignals(backend.EvalProgress(main, logsOf(30), at(29), at(30))); got != "" {
		t.Errorf("续跑之前的子代理不该算：%s", got)
	}
}

func TestEvalProgressNoChange(t *testing.T) {
	read := func(min int) backend.ToolEvent { return backend.ToolEvent{At: at(min), Name: "Read"} }
	main := []backend.ToolEvent{read(88)}
	if got := hitSignals(backend.EvalProgress(main, nil, at(0), at(89))); got != "" {
		t.Errorf("89 分钟不该报：%s", got)
	}
	if got := hitSignals(backend.EvalProgress(main, nil, at(0), at(90))); got != backend.SigNoChange {
		t.Errorf("90 分钟应报：%s", got)
	}
	// 主会话、子代理的改动和 git commit 都算有改动
	for name, ev := range map[string]struct {
		main []backend.ToolEvent
		subs [][]backend.ToolEvent
	}{
		"主会话 Edit":     {main: []backend.ToolEvent{{At: at(1), Name: "Edit"}, read(88)}},
		"主会话 commit":   {main: []backend.ToolEvent{{At: at(1), Name: "Bash", Cmd: "cd x && git -C y commit -m z"}, read(88)}},
		"子代理 Write":    {main: []backend.ToolEvent{read(88)}, subs: [][]backend.ToolEvent{{{At: at(1), Name: "Write"}}}},
		"子代理 commit":   {main: []backend.ToolEvent{read(88)}, subs: [][]backend.ToolEvent{{{At: at(1), Name: "Bash", Cmd: "git commit -am x"}}}},
		"NotebookEdit": {main: []backend.ToolEvent{{At: at(1), Name: "NotebookEdit"}, read(88)}},
	} {
		if got := hitSignals(backend.EvalProgress(ev.main, ev.subs, at(0), at(90))); got != "" {
			t.Errorf("%s 距今 89 分钟不该报：%s", name, got)
		}
	}
	// git status 不算改动
	if got := hitSignals(backend.EvalProgress([]backend.ToolEvent{{At: at(1), Name: "Bash", Cmd: "git status"}, read(88)}, nil, at(0), at(90))); got != backend.SigNoChange {
		t.Errorf("git status 不算改动：%s", got)
	}
}

func TestEvalProgressRepeat(t *testing.T) {
	sh := func(cmd string) backend.ToolEvent { return backend.ToolEvent{At: at(29), Name: "Bash", Cmd: cmd} }
	var main []backend.ToolEvent
	for range 5 {
		main = append(main, sh("go   test\n ./..."))
	}
	if got := hitSignals(backend.EvalProgress(main, nil, at(0), at(30))); got != "" {
		t.Errorf("5 次不该报：%s", got)
	}
	six := append(append([]backend.ToolEvent(nil), main...), sh("go test ./..."))
	h := backend.EvalProgress(six, nil, at(0), at(30))
	if hitSignals(h) != backend.SigRepeat || h[0].Evidence != "同一条命令跑了 6 次，中间没改文件" {
		t.Errorf("压缩空白后 6 次应报：%+v", h)
	}
	// Edit 打断计数
	broken := append(append([]backend.ToolEvent(nil), main...), backend.ToolEvent{At: at(29), Name: "Edit"}, sh("go test ./..."))
	if got := hitSignals(backend.EvalProgress(broken, nil, at(0), at(30))); got != "" {
		t.Errorf("Edit 打断后不该报：%s", got)
	}
	// 别的命令、别的工具不打断
	mixed := append(append([]backend.ToolEvent(nil), main...), sh("ls"), backend.ToolEvent{At: at(29), Name: "Read"}, sh("go test ./..."))
	if got := hitSignals(backend.EvalProgress(mixed, nil, at(0), at(30))); got != backend.SigRepeat {
		t.Errorf("别的命令不打断：%s", got)
	}
}

func TestEvalProgressMainIdle(t *testing.T) {
	main := []backend.ToolEvent{{At: at(5), Name: "Edit"}}
	subs := [][]backend.ToolEvent{{{At: at(40), Name: "Read"}}}
	if got := hitSignals(backend.EvalProgress(main, subs, at(0), at(49))); got != "" {
		t.Errorf("44 分钟不该报：%s", got)
	}
	h := backend.EvalProgress(main, subs, at(0), at(50))
	if hitSignals(h) != backend.SigMainIdle || !strings.Contains(h[0].Evidence, "45 分钟") {
		t.Errorf("45 分钟应报：%+v", h)
	}
	// 子代理也停了 45 分钟：完全静止，归疑似卡死
	if got := hitSignals(backend.EvalProgress(main, [][]backend.ToolEvent{{{At: at(5), Name: "Read"}}}, at(0), at(50))); got != "" {
		t.Errorf("没有子代理在动不该报：%s", got)
	}
	if got := hitSignals(backend.EvalProgress(main, nil, at(0), at(50))); got != "" {
		t.Errorf("没有子代理不该报：%s", got)
	}
	if got := hitSignals(backend.EvalProgress(main, [][]backend.ToolEvent{{{At: at(40), Name: "Read"}}}, at(0), at(85))); got != "" {
		t.Errorf("子代理停了 45 分钟不该报：%s", got)
	}
}

// 证据要写进汇总和推送：命令里可能有密钥，一点都不带出去。
func TestProgressEvidenceHidesCommand(t *testing.T) {
	for _, cmd := range []string{
		`/usr/bin/curl -H "X: PARAM2" https://x.example/a`,
		`A=PARAM1 /usr/bin/curl https://x.example/a`,
		`A="X PARAM1" curl https://x.example/a`,
		`A='X PARAM1' ./deploy.sh`,
	} {
		var main []backend.ToolEvent
		for range backend.RepeatCommand {
			main = append(main, backend.ToolEvent{At: at(29), Name: "Bash", Cmd: cmd})
		}
		h := backend.EvalProgress(main, nil, at(0), at(30))
		if len(h) != 1 || h[0].Evidence != "同一条命令跑了 6 次，中间没改文件" {
			t.Errorf("%s：%+v", cmd, h)
		}
	}
}

// 写一个假的会话记录。events 里每一项写成一条 assistant 记录。
func writeSession(t *testing.T, path string, events []backend.ToolEvent, extra ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("不是 JSON 的行\n")
	b.WriteString(`{"type":"queue-operation"}` + "\n")
	b.WriteString(`{"type":"user","timestamp":"2026-11-04T01:00:00Z","message":{"content":"有 tool_use 字样的字符串"}}` + "\n")
	for _, e := range events {
		rec := map[string]any{"type": "assistant", "timestamp": e.At.Format(time.RFC3339Nano), "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "说明"},
			map[string]any{"type": "tool_use", "name": e.Name, "input": map[string]any{"command": e.Cmd}},
		}}}
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	for _, x := range extra {
		b.WriteString(x)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func progressJob(a *App, kind string, startedAgo time.Duration) *Job {
	return &Job{ID: "1104-000000-p" + kind, Project: "p", Title: "做事", Kind: kind, Status: state.StatusRunning, PID: 1,
		SessionID: "sess-" + kind, StartedAt: a.Now().Add(-startedAgo)}
}

func sessionPath(a *App, id string) string {
	return filepath.Join(a.Home, ".claude", "projects", "-p", id+".jsonl")
}

// 真实格式走一遍：命中→日志、汇总、推送队列（带 JobID）、status 标出，只报不杀，同一信号不重报，meta 里记下。
func TestCheckProgressReportsOnce(t *testing.T) {
	a, out := newTestApp(t)
	now := time.Now()
	j := progressJob(a, "", 2*time.Hour)
	if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession(t, sessionPath(a, j.SessionID), []backend.ToolEvent{{At: now.Add(-2 * time.Hour), Name: "Read"}, {At: now.Add(-time.Minute), Name: "Read"}})
	st := &State{Jobs: []*Job{j}}
	a.Runner.CheckProgress(st)
	if len(j.ProgressNoted) != 1 || j.ProgressNoted[0] != backend.SigNoChange {
		t.Fatalf("应报无改动：%v；日志：%s", j.ProgressNoted, out.String())
	}
	if j.Status != state.StatusRunning {
		t.Errorf("只报不杀：%s", j.Status)
	}
	if !strings.Contains(out.String(), "不自动杀") || !strings.Contains(out.String(), "agentq stop "+j.ID) {
		t.Errorf("日志：%s", out.String())
	}
	night, err := os.ReadFile(a.NightlyPath(a.Now()))
	if err != nil || !strings.Contains(string(night), "看进展") {
		t.Errorf("当天汇总没写：%v %s", err, night)
	}
	s, err := a.Notify.LoadNotify()
	if err != nil || len(s.Pending) != 1 || s.Pending[0].JobID != j.ID || s.Pending[0].Cat != "patrol" {
		t.Errorf("推送队列：%v %+v", err, s)
	}
	if !strings.Contains(runner.ProgressLabel(j), "无改动") {
		t.Errorf("status 标注：%s", runner.ProgressLabel(j))
	}
	meta, err := os.ReadFile(filepath.Join(a.JobDir(j.ID), "meta.json"))
	if err != nil || !strings.Contains(string(meta), backend.SigNoChange) {
		t.Errorf("meta 没记下：%v", err)
	}
	// 再跑不重报
	out.Reset()
	a.Runner.CheckProgress(st)
	s, _ = a.Notify.LoadNotify()
	if out.Len() != 0 || len(s.Pending) != 1 {
		t.Errorf("同一信号不该重报：%s %+v", out.String(), s.Pending)
	}
	// 多了一个新信号才报新的
	sub := filepath.Join(a.Home, ".claude", "projects", "-p", j.SessionID, "subagents")
	for i := range backend.RunawayLogs {
		writeSession(t, filepath.Join(sub, fmt.Sprintf("agent-%d.jsonl", i)), []backend.ToolEvent{{At: now.Add(-time.Minute), Name: "Read"}})
	}
	a.Runner.CheckProgress(st)
	if len(j.ProgressNoted) != 2 || j.ProgressNoted[1] != backend.SigRunaway || !strings.Contains(out.String(), "子代理 25 个") {
		t.Errorf("应只多报子代理失控：%v %s", j.ProgressNoted, out.String())
	}
}

func TestCheckProgressSkips(t *testing.T) {
	a, out := newTestApp(t)
	now := time.Now()
	old := []backend.ToolEvent{{At: now.Add(-3 * time.Hour), Name: "Read"}}
	st := &State{}
	for _, kind := range []string{config.KindNight, config.KindReview, state.KindPatrol, state.KindChat} {
		j := progressJob(a, kind, 3*time.Hour)
		writeSession(t, sessionPath(a, j.SessionID), old)
		st.Jobs = append(st.Jobs, j)
	}
	young := progressJob(a, "", 29*time.Minute)
	writeSession(t, sessionPath(a, young.SessionID), []backend.ToolEvent{{At: now.Add(-29 * time.Minute), Name: "Read"}})
	st.Jobs = append(st.Jobs, young)
	done := progressJob(a, "", 3*time.Hour)
	done.ID, done.SessionID, done.Status = "1104-done", "sess-done", state.StatusDone
	writeSession(t, sessionPath(a, done.SessionID), old)
	st.Jobs = append(st.Jobs, done)
	a.Runner.CheckProgress(st)
	for _, j := range st.Jobs {
		if len(j.ProgressNoted) != 0 {
			t.Errorf("%s（kind=%q）不该看：%v", j.ID, j.Kind, j.ProgressNoted)
		}
	}
	if out.Len() != 0 {
		t.Errorf("不该有日志：%s", out.String())
	}
}

// 读不了、认不出、超限：记一条日志（只记一次），不报任何信号。
func TestCheckProgressUnknown(t *testing.T) {
	big := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(claude.SessionLogMaxBytes + 1); err != nil {
			t.Fatal(err)
		}
	}
	for name, setup := range map[string]struct {
		prepare func(t *testing.T, a *App, j *Job)
		want    string
	}{
		"没有会话记录": {func(t *testing.T, a *App, j *Job) {}, "找不到主会话记录"},
		"认不出格式": {func(t *testing.T, a *App, j *Job) {
			writeSession(t, sessionPath(a, j.SessionID), nil, `{"kind":"tool_use","name":"Bash"}`+"\n")
		}, "认不出工具调用"},
		"主会话超限": {func(t *testing.T, a *App, j *Job) { big(t, sessionPath(a, j.SessionID)) }, "文件超过上限"},
		"子代理超限": {func(t *testing.T, a *App, j *Job) {
			writeSession(t, sessionPath(a, j.SessionID), []backend.ToolEvent{{At: j.StartedAt, Name: "Read"}})
			big(t, filepath.Join(a.Home, ".claude", "projects", "-p", j.SessionID, "subagents", "agent-1.jsonl"))
		}, "子代理记录"},
	} {
		t.Run(name, func(t *testing.T) {
			a, out := newTestApp(t)
			j := progressJob(a, "", 3*time.Hour)
			if err := os.MkdirAll(a.JobDir(j.ID), 0o755); err != nil {
				t.Fatal(err)
			}
			setup.prepare(t, a, j)
			st := &State{Jobs: []*Job{j}}
			a.Runner.CheckProgress(st)
			if !strings.Contains(out.String(), "当作未知") || !strings.Contains(out.String(), setup.want) {
				t.Errorf("日志：%s", out.String())
			}
			out.Reset()
			a.Runner.CheckProgress(st)
			if out.Len() != 0 {
				t.Errorf("只记一次：%s", out.String())
			}
			if runner.ProgressLabel(j) != "" {
				t.Errorf("未知不该标信号：%s", runner.ProgressLabel(j))
			}
			if s, err := a.Notify.LoadNotify(); err != nil || len(s.Pending) != 0 {
				t.Errorf("未知不该推送：%v %+v", err, s.Pending)
			}
			if j.Status != state.StatusRunning {
				t.Errorf("不该动任务：%s", j.Status)
			}
		})
	}
}

// 尾部写了一半的行（任务正在写）、不是数组的 content、时间读不出的记录都跳过。
func TestParseSessionLogTolerates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeSession(t, path, []backend.ToolEvent{{At: at(1), Name: "Bash", Cmd: "ls"}},
		`{"type":"assistant","timestamp":"坏","message":{"content":[{"type":"tool_use","name":"Read"}]}}`+"\n",
		`{"type":"assistant","timestamp":"2026-11-04T01:00:00Z","message":{"content":"tool_use"}}`+"\n",
		`{"type":"assistant","timestamp":"2026-11-04T01:00:00Z","message":{"content":[{"type":"tool_use"`)
	got, err := claude.ParseSessionLog(path)
	if err != nil || len(got) != 1 || got[0].Name != "Bash" || got[0].Cmd != "ls" || !got[0].At.Equal(at(1)) {
		t.Errorf("%v %+v", err, got)
	}
}

// 真实历史回测（只读 ~/.agentq/jobs 和 ~/.claude/projects）：AGENTQ_PROGRESS_BACKTEST=1 才跑。
// 按时间每 5 分钟回放一次，和 tick 看到的一样。
func TestProgressBacktest(t *testing.T) {
	if os.Getenv("AGENTQ_PROGRESS_BACKTEST") != "1" {
		t.Skip("设 AGENTQ_PROGRESS_BACKTEST=1 才跑")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := wireApp(&App{Env: &env.Env{Home: home, Now: time.Now, Out: os.Stdout}})
	metas, err := filepath.Glob(filepath.Join(home, ".agentq", "jobs", "*", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id           string
		logs, nested int
	}
	var rows []row
	tasks, long, unknown := 0, 0, 0
	for _, m := range metas {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			t.Fatal(err)
		}
		if j.Kind != "" || j.EndedAt == nil || !claude.SafeSessionRe.MatchString(j.SessionID) {
			continue
		}
		tasks++
		mainPath, subPaths := claude.SessionFiles(a.Home, j.SessionID)
		if mainPath == "" {
			unknown++
			continue
		}
		main, err := claude.ParseSessionLog(mainPath)
		if err != nil {
			unknown++
			t.Logf("%s：%v", j.ID, err)
			continue
		}
		var subs [][]backend.ToolEvent
		r := row{id: j.ID}
		for _, p := range subPaths {
			s, err := claude.ParseSessionLog(p)
			if err != nil {
				t.Fatalf("%s：%v", p, err)
			}
			subs = append(subs, s)
			var in []backend.ToolEvent
			for _, e := range s {
				if !e.At.Before(j.StartedAt) {
					in = append(in, e)
				}
			}
			if len(in) > 0 {
				r.logs++
			}
			for _, e := range in {
				if backend.IsAgentTool(e.Name) {
					r.nested++
					break
				}
			}
		}
		rows = append(rows, r)
		if j.EndedAt.Sub(j.StartedAt) < runner.ProgressMinRun {
			continue
		}
		long++
		seen := map[string]string{}
		for now := j.StartedAt.Add(runner.ProgressMinRun); !now.After(*j.EndedAt); now = now.Add(5 * time.Minute) {
			for _, h := range backend.EvalProgress(main, subs, j.StartedAt, now) {
				if seen[h.Signal] == "" {
					seen[h.Signal] = fmt.Sprintf("运行 %s 时：%s", now.Sub(j.StartedAt).Round(time.Minute), h.Evidence)
				}
			}
		}
		for sig, why := range seen {
			t.Logf("命中 %s %s（%s）%s", j.ID, sig, j.Status, why)
		}
	}
	sort.Slice(rows, func(i, k int) bool { return rows[i].logs > rows[k].logs })
	t.Logf("待做任务 %d 个，跑满 30 分钟 %d 个，记录读不了 %d 个", tasks, long, unknown)
	for i, r := range rows {
		if i == 5 {
			break
		}
		t.Logf("子代理日志最多：%s %d/%d", r.id, r.logs, r.nested)
	}
}
