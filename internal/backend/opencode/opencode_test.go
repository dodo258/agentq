package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
)

func TestBuildNew(t *testing.T) {
	b := New(backend.Options{Path: "/x/opencode"})
	inv, degraded, err := b.Build(backend.Request{Prompt: "做事", Dir: "/p", Label: "agentq-demo", Model: "anthropic/m", Effort: "high", Permission: permAuto, BudgetUSD: 5, Images: []string{"/p/a.png"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--format", "json", "--model", "anthropic/m", "--variant", "high", "--auto", "--file=/p/a.png"}
	if !slices.Equal(inv.Args, want) || inv.Path != "/x/opencode" || inv.Dir != "/p" {
		t.Fatalf("%q", inv.Args)
	}
	if inv.Stdin != "做事" || strings.Contains(strings.Join(inv.Args, " "), "做事") {
		t.Fatalf("提示词走标准输入，不进参数：%q %q", inv.Stdin, inv.Args)
	}
	if !slices.Contains(inv.EnvKeep, "OPENCODE_CONFIG") || slices.Contains(inv.EnvKeep, "OPENAI_API_KEY") {
		t.Fatalf("%q", inv.EnvKeep)
	}
	if len(degraded) != 1 || degraded[0].Cap != "hard_budget" {
		t.Fatalf("没有美元硬上限要说：%+v", degraded)
	}
}

func TestBuildEmptyPassesNothing(t *testing.T) {
	inv, _, err := New(backend.Options{}).Build(backend.Request{Prompt: "x", Permission: permDefault})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inv.Args, []string{"run", "--format", "json"}) || inv.Path != Name {
		t.Fatalf("%q %s", inv.Args, inv.Path)
	}
}

func TestBuildResume(t *testing.T) {
	inv, _, err := New(backend.Options{}).Build(backend.Request{Prompt: "x", Resume: "ses_1", Permission: permAuto})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inv.Args, "--session=ses_1") {
		t.Fatalf("%q", inv.Args)
	}
}

// 审查进程：--auto、提示词里要 JSON；CLI 没有 schema 参数。
func TestBuildReview(t *testing.T) {
	schema := `{"type":"object"}`
	inv, degraded, err := New(backend.Options{}).Build(backend.Request{Prompt: "审", Profile: backend.ProfileReview, Schema: schema, Effort: "medium", Must: backend.MustReview})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inv.Args, "--auto") || !strings.Contains(inv.Stdin, schema) || !strings.HasPrefix(inv.Stdin, "审") {
		t.Fatalf("%q %q", inv.Args, inv.Stdin)
	}
	if !slices.ContainsFunc(degraded, func(d backend.Degradation) bool { return d.Cap == "schema" }) {
		t.Fatalf("靠提示词要说：%+v", degraded)
	}
}

// 安全边界没真实验证过：要它们的任务不启动，不靠提示词假装。
func TestBuildRefusesUnverifiedBoundaries(t *testing.T) {
	b := New(backend.Options{})
	for _, m := range []backend.Must{backend.MustFollowup, backend.MustPatrol, backend.MustDiscuss, backend.MustProfile, backend.MustSchema, backend.MustNoExternalMCP} {
		if _, _, err := b.Build(backend.Request{Prompt: "x", Must: m}); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("Must %d 应该拒绝：%v", m, err)
		}
	}
	if _, _, err := b.Build(backend.Request{Prompt: "x", Must: backend.MustMerge}); err != nil {
		t.Errorf("待做和 new 能跑：%v", err)
	}
	if _, _, err := b.Build(backend.Request{Prompt: "x", Must: backend.MustImages}); err != nil {
		t.Errorf("--file 能附图片：%v", err)
	}
}

func parseFixture(t *testing.T, name, stderr string) backend.Result {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, resultFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if stderr != "" {
		if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte(stderr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(backend.Options{}).Parse(backend.JobRef{Dir: dir}, backend.Exit{At: exitAt})
}

var exitAt = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func TestParseSuccess(t *testing.T) {
	r := parseFixture(t, "success.jsonl", "")
	if !r.HaveTerminal || r.Failed || r.Text != "做好了" || r.SessionID != "ses_abc123" || r.Turns != 1 || r.LimitText || r.Stop != backend.StopNone {
		t.Fatalf("%+v", r)
	}
	if u := r.Usage; u == nil || u.Input != 100 || u.Output != 25 || u.CacheRead != 40 || u.CacheWrite != 1 || u.CostUSD != 0.25 || u.CostUnknown {
		t.Fatalf("%+v", u)
	}
	if r.UsageCumulative {
		t.Fatal("每次运行的事件流只算这一次")
	}
}

func TestParseToolStepsStructured(t *testing.T) {
	r := parseFixture(t, "tool_steps.jsonl", "")
	if !r.HaveTerminal || r.Failed || r.Turns != 2 || r.Usage.CostUSD < 0.29 || r.Usage.Input != 200 {
		t.Fatalf("%+v %+v", r, r.Usage)
	}
	if !strings.Contains(string(r.Structured), `"verdict":"pass"`) {
		t.Fatalf("%s", r.Structured)
	}
}

func TestParseLimit(t *testing.T) {
	r := parseFixture(t, "limit_429.jsonl", "")
	if !r.HaveTerminal || !r.Failed || !r.LimitStructured || !r.LimitText || r.HTTPStatus != 429 || !r.ResetAt.Equal(exitAt.Add(10*time.Minute)) {
		t.Fatalf("%+v", r)
	}
	r = parseFixture(t, "limit_text.jsonl", "")
	if !r.Failed || r.LimitStructured || !r.LimitText || !r.ResetAt.IsZero() {
		t.Fatalf("只有文字的不算结构化：%+v", r)
	}
}

func TestParseAuth(t *testing.T) {
	for _, name := range []string{"auth_error.jsonl", "auth_401.jsonl"} {
		if r := parseFixture(t, name, ""); r.Stop != backend.StopAuth || !r.Failed || !r.HaveTerminal {
			t.Fatalf("%s %+v", name, r)
		}
	}
	r := parseFixture(t, "", "Error: Unauthorized\n") // 没开工，错误在 stderr
	if r.Stop != backend.StopAuth || !r.HaveTerminal || !r.Failed {
		t.Fatalf("%+v", r)
	}
}

// 失败的结果不产出结构化输出：前面那条消息可能写着 pass。
func TestParseFailedHasNoStructured(t *testing.T) {
	r := parseFixture(t, "server_500.jsonl", "")
	if !r.Failed || r.Structured != nil || r.Text != "Internal" || r.HTTPStatus != 500 || r.LimitText {
		t.Fatalf("%+v", r)
	}
}

// 没有终态：丢了最后的 step_finish、循环还没结束、被杀、没输出，都不猜成功。
func TestParseNoTerminal(t *testing.T) {
	for _, name := range []string{"lost_finish.jsonl", "unknown_finish.jsonl", "truncated.jsonl", ""} {
		if r := parseFixture(t, name, "some log\n"); r.HaveTerminal || r.Failed || r.Structured != nil {
			t.Fatalf("%q %+v", name, r)
		}
	}
	r := parseFixture(t, "lost_finish.jsonl", "")
	if r.SessionID == "" || r.Usage == nil || r.Usage.Input != 100 {
		t.Fatalf("会话 id 和前面的用量还在：%+v", r)
	}
}

// 模型正文说到 rate limit、429 不算撞限：只看错误通道。
func TestParseModelTextIsNotLimit(t *testing.T) {
	r := parseFixture(t, "model_says_limit.jsonl", "")
	if r.LimitText || r.LimitStructured || r.Failed || !r.HaveTerminal {
		t.Fatalf("%+v", r)
	}
}

func TestParseLastStepWins(t *testing.T) {
	r := parseFixture(t, "resume_after_error.jsonl", "")
	if r.Failed || !r.HaveTerminal || r.Text != "接着做好了" || r.HTTPStatus != 0 {
		t.Fatalf("%+v", r)
	}
}

// 金额是 0（订阅、本地模型）算未知，不当成没花钱。
func TestCostZeroIsUnknown(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"step_finish","timestamp":1,"sessionID":"s","part":{"reason":"stop","cost":0,"tokens":{"input":5,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}}` + "\n"
	os.WriteFile(filepath.Join(dir, resultFile), []byte(line), 0o644)
	b := New(backend.Options{})
	if r := b.Parse(backend.JobRef{Dir: dir}, backend.Exit{}); r.Usage == nil || !r.Usage.CostUnknown {
		t.Fatalf("%+v", r.Usage)
	}
	if u := b.(*Backend).LiveUsage(backend.JobRef{Dir: dir}); u == nil || !u.CostUnknown || u.Input != 5 {
		t.Fatalf("%+v", u)
	}
}

func TestLiveUsageMidway(t *testing.T) {
	dir := t.TempDir()
	b, _ := os.ReadFile(filepath.Join("testdata", "lost_finish.jsonl"))
	os.WriteFile(filepath.Join(dir, resultFile), b, 0o644)
	u := New(backend.Options{}).(*Backend).LiveUsage(backend.JobRef{Dir: dir})
	if u == nil || u.Input != 100 || u.CostUSD != 0.25 {
		t.Fatalf("%+v", u)
	}
	if u := New(backend.Options{}).(*Backend).LiveUsage(backend.JobRef{Dir: t.TempDir()}); u != nil {
		t.Fatalf("还没输出是 nil：%+v", u)
	}
}

func TestToolEvents(t *testing.T) {
	dir := t.TempDir()
	b, _ := os.ReadFile(filepath.Join("testdata", "tool_steps.jsonl"))
	os.WriteFile(filepath.Join(dir, resultFile), b, 0o644)
	main, subs, err := New(backend.Options{}).(backend.ToolLog).ToolEvents(backend.JobRef{Dir: dir}, true)
	if err != nil || subs != nil {
		t.Fatal(err, subs)
	}
	var names []string
	for _, e := range main {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"Bash", "Edit", "Agent", "Other"}) || main[0].Cmd != "git commit -m x" || main[0].At.UnixMilli() != 1763000001000 {
		t.Fatalf("%+v", main)
	}
	// 格式变了说看不懂，不当成没动静
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, resultFile), []byte(`{"type":"x"}`+"\n"), 0o644)
	if _, _, err := New(backend.Options{}).(backend.ToolLog).ToolEvents(backend.JobRef{Dir: empty}, false); err == nil {
		t.Fatal("应该报格式变了")
	}
}

func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbe(t *testing.T) {
	home := t.TempDir()
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	b := New(backend.Options{Home: home})
	p := fakeCLI(t, `case "$1" in --version) echo 1.19.5 ;; run) echo "--format --session --auto" ;; esac`)
	r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: p, Env: env})
	if err != nil || r.Version != "1.19.5" || !r.Headless || r.Authenticated {
		t.Fatalf("%+v %v", r, err)
	}
	old := fakeCLI(t, `case "$1" in --version) echo 0.1 ;; run) echo "--format --session" ;; esac`)
	if _, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: old, Env: env}); err == nil || !strings.Contains(err.Error(), "--auto") {
		t.Fatalf("老版本要说缺什么：%v", err)
	}
	if _, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: filepath.Join(home, "nosuch"), Env: env}); !errors.Is(err, backend.ErrNoCLI) {
		t.Fatalf("%v", err)
	}
}

func TestLocations(t *testing.T) {
	b := New(backend.Options{}).(*Backend)
	if got := b.SkillsDir("/h"); got != "/h/.agents/skills" {
		t.Error(got)
	}
	if file, shadow := b.RulesFile("/h"); file != "/h/.config/opencode/AGENTS.md" || shadow != "" {
		t.Error(file, shadow)
	}
	if b.Auth(context.Background()).State != backend.AuthUnknown || b.Caps().AuthProbe {
		t.Error("认证查不了")
	}
	if b.CanResume("ses_1") {
		t.Error("查不了就当接不上")
	}
	if b.RecordedModel([]string{"run", "--model", "p/m"}) != "p/m" || b.RecordedModel([]string{"run"}) != "" {
		t.Error("RecordedModel")
	}
}
