package stats

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/state"
	"agentq/internal/testkit/stack"
)

// wireStats 建模块实例。
func wireStats(e *env.Env) *Service {
	return &Service{Env: e, Ledger: stack.New(e).Ledger}
}

type statsFixture struct {
	t   *testing.T
	a   *Service
	now time.Time
}

func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	a := wireStats(&env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: time.Now, Out: &bytes.Buffer{}})
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	a.Now = func() time.Time { return now }
	return &statsFixture{t: t, a: a, now: now}
}

// job 写一个任务的 meta.json；result 非空时写 result.json。
func (f *statsFixture) job(j state.Job, result string) {
	f.t.Helper()
	if j.Status == "" {
		j.Status = state.StatusDone
	}
	if j.Project == "" {
		j.Project = "demo"
	}
	if j.StartedAt.IsZero() {
		j.StartedAt = f.now.Add(-time.Hour)
	}
	if j.EndedAt == nil && j.Status != state.StatusRunning {
		ended := f.now.Add(-time.Minute)
		j.EndedAt = &ended
	}
	dir := f.a.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if result != "" {
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(result), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func costResult(cost float64, input int64) string {
	b, _ := json.Marshal(map[string]any{"total_cost_usd": cost, "modelUsage": map[string]any{"m": map[string]any{"inputTokens": input, "costUSD": cost}}})
	return string(b)
}

func (f *statsFixture) compute(days int) *statsResult {
	f.t.Helper()
	res, err := f.a.ComputeStats(days)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func approx(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// 结果文件大得不正常：不解析，用收尾时记下的用量。暴力测试种子 905 撞上过：两个 50MB 的结果，-race 下 stats 超过 60 秒。
func TestStatsSkipsHugeResult(t *testing.T) {
	f := newStatsFixture(t)
	saved := &backend.JobUsage{Input: 7, CostUSD: 3}
	huge := costResult(99, 999) + strings.Repeat(" ", statsResultMax)
	f.job(state.Job{ID: "a", Usage: saved}, huge)
	f.job(state.Job{ID: "b"}, costResult(1, 10))
	res := f.compute(7)
	if !approx(res.Total.Usage.CostUSD, 4) || res.Total.Usage.Input != 17 || res.BadResult != 1 {
		t.Fatalf("%+v %d", res.Total, res.BadResult)
	}
}

// 启动参数没写模型是跟随 CLI；老记录没有参数才是未知。
func TestStatsModelFollowsCLI(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "a", Args: []string{"claude", "-p", "<见 prompt.txt>"}}, costResult(1, 10))
	f.job(state.Job{ID: "b"}, costResult(1, 10))
	res := f.compute(7)
	if res.ByMod[config.ModelLabel("")] == nil || res.ByMod[config.ModelLabel("")].Count != 1 || res.ByMod["未知"] == nil || res.ByMod["未知"].Count != 1 {
		t.Errorf("%+v", res.ByMod)
	}
}

func TestStatsGroups(t *testing.T) {
	f := newStatsFixture(t)
	opus := []string{"claude", "--model", "opus"}
	f.job(state.Job{ID: "a", Project: "alpha", Args: opus}, costResult(10, 100))
	f.job(state.Job{ID: "b", Project: "alpha", Args: opus, Status: state.StatusFailed}, costResult(2, 20))
	f.job(state.Job{ID: "c", Project: "beta", Args: []string{"claude", "--model", "sonnet"}, Kind: config.KindNight, Status: state.StatusTimeout}, costResult(1, 10))
	f.job(state.Job{ID: "d", Project: "beta", Args: opus, Status: state.StatusInterrupted}, costResult(1, 10))
	f.job(state.Job{ID: "e", Project: "alpha", Args: opus, Status: state.StatusKilled}, "")
	f.job(state.Job{ID: "f", Project: "alpha", Args: opus, Status: state.StatusRunning}, "")
	f.job(state.Job{ID: "g", Project: "alpha", Args: opus, Verify: &state.MergeVerify{Failed: true}}, costResult(1, 10))
	res := f.compute(7)
	tt := res.Total
	if tt.Count != 7 || tt.OK != 1 || tt.Bad != 3 || tt.Other != 2 || tt.Running != 1 {
		t.Fatalf("%+v", tt)
	}
	if !approx(tt.Usage.CostUSD, 15) || tt.Usage.Input != 150 || tt.NoUsage != 2 {
		t.Errorf("%+v", tt)
	}
	if res.ByKind["待做任务"].Count != 6 || res.ByKind["夜班"].Count != 1 {
		t.Errorf("%+v", res.ByKind)
	}
	if res.ByProj["alpha"].Count != 5 || res.ByProj["beta"].Count != 2 {
		t.Errorf("%+v", res.ByProj)
	}
	if res.ByMod["opus"].Count != 6 || res.ByMod["sonnet"].Count != 1 {
		t.Errorf("%+v", res.ByMod)
	}
	var b strings.Builder
	res.print(&b)
	out := b.String()
	for _, want := range []string{"最近 7 天：7 次　成 1　败 3　中断 2　在跑 1", "按种类：", "按项目：", "按模型：", "待做任务　6 次", "alpha　5 次", "sonnet　1 次", "约 $15.00", "2 个任务没有用量记录"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q：\n%s", want, out)
		}
	}
	// 费用占比，贵的在前
	if !strings.Contains(out, "alpha　5 次　成 1　败 2　中断 1　在跑 1　token 130　约 $13.00　87%") {
		t.Errorf("%s", out)
	}
	if strings.Index(out, "alpha　") > strings.Index(out, "beta　") {
		t.Errorf("贵的在前：%s", out)
	}
}

// 同一会话续跑时 result.json 是累计的：减掉上一次，总数等于最后一次的累计数。
func TestStatsResumeNotDoubleCounted(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "orig", SessionID: "s", Status: state.StatusInterrupted}, costResult(4, 100))
	f.job(state.Job{ID: "resume1", SessionID: "s", ResumeOf: "orig", Status: state.StatusInterrupted}, costResult(6, 150))
	f.job(state.Job{ID: "resume2", SessionID: "s", ResumeOf: "resume1"}, costResult(9, 200))
	res := f.compute(7)
	if !approx(res.Total.Usage.CostUSD, 9) || res.Total.Usage.Input != 200 {
		t.Errorf("重复计了：%+v", res.Total)
	}
	// 上一次在时间窗外：这一次仍然只算增量
	old := f.now.Add(-30 * 24 * time.Hour)
	f2 := newStatsFixture(t)
	f2.job(state.Job{ID: "orig", SessionID: "s", EndedAt: &old, StartedAt: old, Status: state.StatusInterrupted}, costResult(4, 100))
	f2.job(state.Job{ID: "resume1", SessionID: "s", ResumeOf: "orig"}, costResult(6, 150))
	res = f2.compute(7)
	if res.Total.Count != 1 || !approx(res.Total.Usage.CostUSD, 2) {
		t.Errorf("%+v", res.Total)
	}
}

// 记录被改坏成环：不能卡住。
func TestStatsResumeCycle(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "x", ResumeOf: "y"}, costResult(1, 1))
	f.job(state.Job{ID: "y", ResumeOf: "z"}, "")
	f.job(state.Job{ID: "z", ResumeOf: "y"}, "")
	f.job(state.Job{ID: "self", ResumeOf: "self"}, costResult(1, 1))
	done := make(chan *statsResult, 1)
	go func() {
		res, _ := f.a.ComputeStats(7)
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil || res.Total.Count != 4 {
			t.Errorf("%+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("卡住了")
	}
}

func TestStatsWindow(t *testing.T) {
	f := newStatsFixture(t)
	inside := f.now.Add(-6*24*time.Hour - 23*time.Hour)
	outside := f.now.Add(-7*24*time.Hour - time.Hour)
	f.job(state.Job{ID: "in", EndedAt: &inside}, costResult(1, 1))
	f.job(state.Job{ID: "out", EndedAt: &outside}, costResult(100, 1))
	if res := f.compute(7); res.Total.Count != 1 || !approx(res.Total.Usage.CostUSD, 1) {
		t.Errorf("%+v", res.Total)
	}
	if res := f.compute(30); res.Total.Count != 2 {
		t.Errorf("%+v", res.Total)
	}
	if res := f.compute(1); res.Total.Count != 0 {
		t.Errorf("%+v", res.Total)
	}
	// 还在跑的按启动时间算
	started := f.now.Add(-48 * time.Hour)
	f.job(state.Job{ID: "run", Status: state.StatusRunning, StartedAt: started}, "")
	if res := f.compute(1); res.Total.Running != 0 {
		t.Errorf("%+v", res.Total)
	}
	if res := f.compute(3); res.Total.Running != 1 {
		t.Errorf("%+v", res.Total)
	}
}

// 坏文件跳过并计数；result.json 坏了但收尾时记过用量的，用记下的。
func TestStatsBadFiles(t *testing.T) {
	f := newStatsFixture(t)
	f.job(state.Job{ID: "ok"}, costResult(1, 1))
	f.job(state.Job{ID: "badresult", Usage: &backend.JobUsage{CostUSD: 2, Input: 5}}, "{半截")
	f.job(state.Job{ID: "badresult2"}, "{半截")
	for name, content := range map[string]string{"badmeta": "{坏了", "empty": "", "mismatch": `{"id":"别的","status":"done"}`, "nostatus": `{"id":"nostatus"}`} {
		dir := f.a.JobDir(name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "meta.json"), []byte(content), 0o644)
	}
	res := f.compute(7)
	if res.BadMeta != 4 || res.BadResult != 2 || res.Total.Count != 3 || res.Total.NoUsage != 1 || !approx(res.Total.Usage.CostUSD, 3) {
		t.Errorf("%+v %+v", res, res.Total)
	}
	var b strings.Builder
	res.print(&b)
	if !strings.Contains(b.String(), "跳过 4 个读不了的 meta.json") || !strings.Contains(b.String(), "2 个 result.json 读不了") {
		t.Errorf("%s", b.String())
	}
}

func TestStatsEmptyAndArgs(t *testing.T) {
	f := newStatsFixture(t)
	if err := f.a.CmdStats(nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"0", "-1", "abc", "1.5", "99999"} {
		if err := f.a.CmdStats([]string{bad}); err == nil {
			t.Errorf("%q 要报错", bad)
		}
	}
	if err := f.a.CmdStats([]string{"30"}); err != nil {
		t.Error(err)
	}
}

func TestStatsKindNamesComplete(t *testing.T) {
	for _, k := range []string{config.KindNight, config.KindReview, state.KindPatrol, state.KindChat, state.KindFollowup, state.KindRouter, state.KindQuick, state.KindInbox, state.KindNew, state.KindSuggest, state.KindSense, state.KindRelease} {
		if statsKindNames[k] == "" {
			t.Errorf("%s 没有名字", k)
		}
	}
	if statsKindName("没见过") != "没见过" {
		t.Error("没见过的原样显示")
	}
}

// 门调模型花的算在任务头上：结果文件里没有，重读之后要加回去；结果文件读不了时收尾记下的已经含了，不重复算。
func TestStatsCountsGateUsage(t *testing.T) {
	f := newStatsFixture(t)
	gateUsage := &backend.JobUsage{Input: 5, CostUSD: 1}
	f.job(state.Job{ID: "a", Project: "alpha", Gate: &state.GateRecord{State: state.GateMerged, Usage: gateUsage}}, costResult(10, 100))
	res := f.compute(7)
	if !approx(res.Total.Usage.CostUSD, 11) || res.Total.Usage.Input != 105 {
		t.Errorf("%+v", res.Total.Usage)
	}
}
