package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend/claude"
	"agentq/internal/backlog"
	"agentq/internal/config"
	"agentq/internal/runner"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 真实历史 result.json 里的文案（~/.agentq/jobs 只读提取，没有项目名和路径）。
const (
	real403Text = "Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key instead, or ask your admin to enable access"
	real422Text = "API Error: 422 格式转换错误: Responses upstream service_unavailable_error: Our servers are currently overloaded. Please try again later."
)

// 官方文档 code.claude.com/docs/en/errors 写的文案。
const (
	doc500Text = "API Error: 500 Internal server error. This is a server-side issue, usually temporary — try again in a moment."
	doc502Text = "API Error: 502 Bad Gateway"
	doc529Text = "API Error: Repeated 529 Overloaded errors. The API is at capacity — this is usually temporary. Try again in a moment."
)

func parseResult(t *testing.T, s string) claude.Result {
	t.Helper()
	var r claude.Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPIStatus(t *testing.T) {
	cases := map[string]int{
		`{"api_error_status":422}`:     422,
		`{"api_error_status":"429"}`:   429,
		`{"api_error_status":null}`:    0,
		`{}`:                           0,
		`{"api_error_status":"oops"}`:  0,
		`{"api_error_status":{"a":1}}`: 0,
		`{"api_error_status":42.5}`:    0,
	}
	for in, want := range cases {
		if got := parseResult(t, in).ApiStatus(); got != want {
			t.Errorf("%s：得到 %d，应为 %d", in, got, want)
		}
	}
}

func TestHitUsageLimit(t *testing.T) {
	result := func(fields map[string]any) string {
		b, _ := json.Marshal(fields)
		return string(b)
	}
	yes := []map[string]any{
		{"is_error": true, "api_error_status": 429, "num_turns": 50},                                // 结构化字段，没有字样
		{"is_error": true, "api_error_status": "429", "result": ""},                                 // 字符串写法
		{"is_error": true, "api_error_status": 429, "result": "Request rejected (429)"},             // 官方文案
		{"is_error": true, "result": "Request rejected (429)"},                                      // 没有状态码，字样兜底
		{"is_error": true, "result": "API Error: 429 too many requests"},                            // 同上
		{"is_error": true, "result": "spend limit reached"},                                         // 官方文案（网关）
		{"is_error": true, "result": "You've hit your session limit · resets 3:45pm"},               // 官方文案
		{"is_error": false, "num_turns": 1, "result": "You've hit your weekly limit"},               // 回合很少
		{"is_error": true, "api_error_status": 400, "result": "You have reached your usage limits"}, // 别的状态码也认字样
	}
	no := []map[string]any{
		{"is_error": false, "num_turns": 40, "result": "修好了 rate limit 重试"},                   // 正常做了很多回合
		{"is_error": true, "api_error_status": 403, "result": real403Text},                    // 真实 403
		{"is_error": true, "api_error_status": 422, "result": real422Text},                    // 真实 422
		{"is_error": true, "api_error_status": 529, "result": doc529Text},                     // 官方 529
		{"is_error": true, "subtype": "error_max_budget_usd", "result": "rate limit 的代码改到一半"}, // 自己设的上限
		{"is_error": true, "subtype": "error_max_turns", "result": "hit your limit"},          // 同上
		{"is_error": true, "result": "出错了：测试失败"},
	}
	for _, f := range yes {
		s := result(f)
		if !parseResult(t, s).HitUsageLimit() {
			t.Errorf("%s 应算撞上限", s)
		}
	}
	for _, f := range no {
		s := result(f)
		if parseResult(t, s).HitUsageLimit() {
			t.Errorf("%s 不应算撞上限", s)
		}
	}
}

// finishResult：造一个刚结束的待做任务，按给定的 result.json 收尾，返回这条任务。
func finishResult(t *testing.T, a *App, st *State, j *Job, result map[string]any) *Job {
	t.Helper()
	if j.ID == "" {
		j.ID = "t-" + strings.ReplaceAll(a.Now().Format("150405.000000000"), ".", "-")
	}
	if j.ProjectDir == "" {
		j.ProjectDir = t.TempDir()
	}
	j.Status, j.StartedAt = state.StatusRunning, a.Now().Add(-10*time.Second)
	if j.Fingerprint == "" && j.Kind == "" {
		j.Fingerprint = "fp"
	}
	os.MkdirAll(a.JobDir(j.ID), 0o755)
	b, _ := json.Marshal(result)
	if err := os.WriteFile(filepath.Join(a.JobDir(j.ID), "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	st.Jobs = append(st.Jobs, j)
	a.Runner.FinishFromResult(st, j)
	return j
}

func transientApp(t *testing.T) (*App, *time.Time) {
	t.Helper()
	a, _ := newTestApp(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	a.Now = func() time.Time { return now }
	a.Cfg.NotifyEnabled = false
	return a, &now
}

func TestFinishClassifiesAPIErrors(t *testing.T) {
	type want struct {
		status            string
		transient, limit  bool
		streak            int
		cooldown, summary string
	}
	cases := []struct {
		name   string
		result map[string]any
		want   want
	}{
		{"真实 422 没花钱", map[string]any{"is_error": true, "subtype": "success", "api_error_status": 422, "num_turns": 1, "total_cost_usd": 0, "result": real422Text, "session_id": "s"},
			want{status: state.StatusFailed, transient: true, summary: "15m 后重试"}},
		{"真实 422 花了 14 美元", map[string]any{"is_error": true, "subtype": "success", "api_error_status": 422, "num_turns": 110, "total_cost_usd": 14.2, "result": real422Text, "session_id": "s"},
			want{status: state.StatusFailed, streak: 1}},
		{"真实 403 不重试", map[string]any{"is_error": true, "subtype": "success", "api_error_status": 403, "num_turns": 1, "total_cost_usd": 0, "result": real403Text, "session_id": "s"},
			want{status: state.StatusFailed, streak: 1}},
		{"官方 500", map[string]any{"is_error": true, "api_error_status": 500, "num_turns": 1, "total_cost_usd": 0.01, "result": doc500Text},
			want{status: state.StatusFailed, transient: true}},
		{"官方 502", map[string]any{"is_error": true, "api_error_status": 502, "num_turns": 1, "result": doc502Text},
			want{status: state.StatusFailed, transient: true}},
		{"官方 529", map[string]any{"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 0, "result": doc529Text},
			want{status: state.StatusFailed, transient: true}},
		{"408 走普通失败", map[string]any{"is_error": true, "api_error_status": 408, "num_turns": 1, "total_cost_usd": 0, "result": "API Error: 408"},
			want{status: state.StatusFailed, streak: 1}},
		{"409 走普通失败", map[string]any{"is_error": true, "api_error_status": 409, "num_turns": 1, "total_cost_usd": 0, "result": "API Error: 409"},
			want{status: state.StatusFailed, streak: 1}},
		{"429 没有字样也算撞上限", map[string]any{"is_error": true, "api_error_status": 429, "num_turns": 30, "total_cost_usd": 3, "result": ""},
			want{status: state.StatusFailed, limit: true, summary: "撞到用量上限"}},
		{"没有费用字段但回合很多", map[string]any{"is_error": true, "api_error_status": 500, "num_turns": 50, "result": doc500Text},
			want{status: state.StatusFailed, streak: 1}},
		{"费用刚好到门槛不算", map[string]any{"is_error": true, "api_error_status": 500, "num_turns": 1, "total_cost_usd": runner.TransientMaxCostUSD, "result": doc500Text},
			want{status: state.StatusFailed, streak: 1}},
		{"状态码读不懂", map[string]any{"is_error": true, "api_error_status": map[string]any{"x": 1}, "num_turns": 1, "total_cost_usd": 0, "result": "怪"},
			want{status: state.StatusFailed, streak: 1}},
		{"is_error 为假不算失败", map[string]any{"is_error": false, "api_error_status": 500, "num_turns": 40, "total_cost_usd": 0, "result": "已合并，提交 abc1234"},
			want{status: state.StatusDone}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := transientApp(t)
			st := &State{}
			j := finishResult(t, a, st, &Job{Project: "p", Title: "甲"}, c.result)
			if j.Status != c.want.status || j.Transient != c.want.transient || j.LimitHit != c.want.limit || st.FailStreak != c.want.streak {
				t.Fatalf("status=%s transient=%v limit=%v streak=%d，应为 %+v；摘要：%s", j.Status, j.Transient, j.LimitHit, st.FailStreak, c.want, j.Summary)
			}
			if c.want.limit != !st.CooldownUntil.IsZero() {
				t.Fatalf("冷却应%v：%v", c.want.limit, st.CooldownUntil)
			}
			if !strings.Contains(j.Summary, c.want.summary) {
				t.Fatalf("摘要应含 %q：%s", c.want.summary, j.Summary)
			}
		})
	}
}

// 续跑的 total_cost_usd 是整个会话累计的：减掉上次才是这次花的（真实历史里有 1 回合累计 $2.5 的结果）。
func TestTransientCostSubtractsResumedSession(t *testing.T) {
	a, _ := transientApp(t)
	st := &State{}
	prev := finishResult(t, a, st, &Job{ID: "t-prev", Project: "p", Title: "甲"}, map[string]any{
		"is_error": true, "api_error_status": 400, "num_turns": 20, "total_cost_usd": 2.5, "result": "别的错"})
	j := finishResult(t, a, st, &Job{ID: "t-next", Project: "p", Title: "甲", ResumeOf: prev.ID}, map[string]any{
		"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 2.5, "result": doc529Text})
	if !j.Transient {
		t.Fatalf("续跑这次没花钱，应算瞬时错误：%s", j.Summary)
	}
}

// 条目已标进行中（续跑时上游出错）：pick 挑不到它，摘要不能说多久后重试。
func TestTransientInProgressNoRetryPromise(t *testing.T) {
	a, _ := transientApp(t)
	root := t.TempDir()
	dir := testkit.MkProject(t, root, "demo", "- [ ] 甲 —— 说明（进行中：agent/x 2026-11-04 10:00）\n")
	items, _ := backlog.Project{Dir: dir}.Items()
	st := &State{}
	j := finishResult(t, a, st, &Job{Project: "demo", ProjectDir: dir, Title: "甲", Fingerprint: items[0].Fingerprint}, map[string]any{
		"is_error": true, "api_error_status": 500, "num_turns": 1, "total_cost_usd": 0, "result": doc500Text})
	if !j.Transient || strings.Contains(j.Summary, "后重试") || !strings.Contains(j.Summary, "仍标进行中") {
		t.Fatalf("应算瞬时错误、不承诺重试：%+v", j)
	}
}

// 夜班、复盘、对话没有条目可重试：不计连环失败，但不说重试。
func TestTransientRoutineNotCounted(t *testing.T) {
	a, _ := transientApp(t)
	st := &State{BreakerLevel: 1}
	j := finishResult(t, a, st, &Job{Kind: config.KindNight, Project: "夜班", Title: "agentq night", Item: "x"}, map[string]any{
		"is_error": true, "api_error_status": 500, "num_turns": 1, "total_cost_usd": 0, "result": doc500Text})
	if !j.Transient || st.FailStreak != 0 || !st.BreakerUntil.IsZero() || strings.Contains(j.Summary, "后重试") {
		t.Fatalf("不该计连环失败、不该说重试：%+v state=%+v", j, st)
	}
}

// 15 分钟、1 小时各重试一次，之后回到 retry_after。
func TestTransientBackoffSchedule(t *testing.T) {
	a, now := transientApp(t)
	st := &State{}
	it := backlog.Item{Text: "甲", Title: "甲", Fingerprint: "fp"}
	fail := func(id string) *Job {
		return finishResult(t, a, st, &Job{ID: id, Project: "p", Title: "甲"}, map[string]any{
			"is_error": true, "api_error_status": 422, "num_turns": 1, "total_cost_usd": 0, "result": real422Text})
	}
	can := func() bool {
		_, ok := a.Runner.Pick(st, []backlog.Item{it}, "", true)
		return ok
	}
	for i, step := range []struct {
		id      string
		wait    time.Duration
		note    string
		isTrans bool
	}{
		{"t-1", 15 * time.Minute, "15m 后重试", true},
		{"t-2", time.Hour, "1h 后重试", true},
		{"t-3", a.Cfg.RetryAfter.Duration, "已重试 2 次", false},
	} {
		j := fail(step.id)
		if j.Transient != step.isTrans || !strings.Contains(j.Summary, step.note) {
			t.Fatalf("第 %d 次：transient=%v 摘要：%s", i+1, j.Transient, j.Summary)
		}
		*now = now.Add(step.wait - time.Minute)
		if can() {
			t.Fatalf("第 %d 次失败后 %s 不该能挑", i+1, step.wait-time.Minute)
		}
		*now = now.Add(2 * time.Minute)
		if !can() {
			t.Fatalf("第 %d 次失败后 %s 应能挑", i+1, step.wait+time.Minute)
		}
	}
	if st.FailStreak != 1 {
		t.Fatalf("重试用完那次才计连环失败：%d", st.FailStreak)
	}
}

// 三个真实 403 秒退照样熔断（认证、权限错误不重试）；三个瞬时错误不熔断。
func TestBreakerAuthVsTransient(t *testing.T) {
	a, _ := transientApp(t)
	st := &State{}
	for i, fp := range []string{"a", "b", "c"} {
		j := finishResult(t, a, st, &Job{ID: "t-tr-" + fp, Project: "p", Title: fp, Fingerprint: fp}, map[string]any{
			"is_error": true, "api_error_status": 529, "num_turns": 1, "total_cost_usd": 0, "result": doc529Text})
		if !j.Transient || st.FailStreak != 0 || !st.BreakerUntil.IsZero() {
			t.Fatalf("第 %d 个瞬时错误不该计数：%+v", i+1, st)
		}
	}
	for _, fp := range []string{"d", "e", "f"} {
		finishResult(t, a, st, &Job{ID: "t-auth-" + fp, Project: "p", Title: fp, Fingerprint: fp}, map[string]any{
			"is_error": true, "api_error_status": 403, "num_turns": 1, "total_cost_usd": 0, "result": real403Text})
	}
	if st.BreakerUntil.IsZero() {
		t.Fatalf("三个 403 秒退应熔断：%+v", st)
	}
}

// 老状态文件没有新字段：正常读，当零值；新字段零值不写进文件。
func TestStateCompatNewFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	old := `{"jobs":[{"id":"x","project":"p","project_dir":"/d","item":"i","title":"t","fingerprint":"f","pid":1,"started_at":"2026-10-11T00:00:00+08:00","status":"failed"}],"cooldown_until":"0001-01-01T00:00:00Z","breaker_until":"0001-01-01T00:00:00Z"}`
	os.WriteFile(p, []byte(old), 0o644)
	st, err := state.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if j := st.Jobs[0]; j.Takeover || j.Transient {
		t.Fatalf("缺字段应为零值：%+v", j)
	}
	if err := state.Save(p, st); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "takeover") || strings.Contains(string(b), "transient") {
		t.Fatalf("零值不该写进状态文件：%s", b)
	}
	st.Jobs[0].Takeover, st.Jobs[0].Transient = true, true
	if err := state.Save(p, st); err != nil {
		t.Fatal(err)
	}
	st2, err := state.Load(p)
	if err != nil || !st2.Jobs[0].Takeover || !st2.Jobs[0].Transient {
		t.Fatalf("新字段应能存取：%v %+v", err, st2)
	}
}
