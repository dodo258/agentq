package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
)

func jobWith(t *testing.T, fixture string) backend.JobRef {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resultFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return backend.JobRef{ID: "j", Dir: dir}
}

func TestBuildArgs(t *testing.T) {
	b := New(backend.Options{Path: "/x/codex"})
	cases := []struct {
		name string
		req  backend.Request
		want []string
	}{
		{"留空的都不传", backend.Request{Prompt: "做", Dir: "/p", Label: "agentq p 标题"},
			[]string{"exec", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-"}},
		{"完全权限和模型", backend.Request{Prompt: "做", Model: "gpt-x", Effort: "high", Permission: permFull},
			[]string{"exec", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`, "-m", "gpt-x", "-c", `model_reasoning_effort="high"`, "-"}},
		{"default 不传沙箱", backend.Request{Prompt: "做", Permission: permDefault},
			[]string{"exec", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-"}},
		{"续跑：resume 没有 -s，沙箱走 -c", backend.Request{Prompt: "接着", Resume: "01a1adf5-1a35", Permission: permFull},
			[]string{"exec", "resume", "01a1adf5-1a35", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`, "-"}},
		{"审查：一次性 worktree 用 workspace-write，不看配置的权限", backend.Request{Prompt: "审", Profile: backend.ProfileReview, Permission: permFull, Effort: "medium"},
			[]string{"exec", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-c", `sandbox_mode="workspace-write"`, "-c", `model_reasoning_effort="medium"`, "-"}},
		{"附加目录", backend.Request{Prompt: "做", AddDirs: []string{"/a b", "/c"}},
			[]string{"exec", "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-c", `sandbox_workspace_write.writable_roots=["/a b","/c"]`, "-"}},
	}
	for _, c := range cases {
		inv, _, err := b.Build(c.req)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if !slices.Equal(inv.Args, c.want) {
			t.Errorf("%s：\n得到 %q\n应为 %q", c.name, inv.Args, c.want)
		}
		if inv.Stdin != c.req.Prompt || inv.Path != "/x/codex" || !slices.Equal(inv.EnvKeep, []string{"CODEX_HOME"}) {
			t.Errorf("%s：stdin=%q path=%s keep=%v", c.name, inv.Stdin, inv.Path, inv.EnvKeep)
		}
		if strings.Contains(strings.Join(inv.Args, " ")+strings.Join(inv.Record, " "), c.req.Prompt) {
			t.Errorf("%s：提示词进了参数或记录：%q", c.name, inv.Args)
		}
		if b.RecordedModel(inv.Record) != c.req.Model {
			t.Errorf("%s：认出的模型 %q", c.name, b.RecordedModel(inv.Record))
		}
	}
}

func TestBuildSchemaIsStrict(t *testing.T) {
	work := t.TempDir()
	b := New(backend.Options{})
	schema := `{"type":"object","properties":{"verdict":{"type":"string"},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}}},"required":["verdict","findings"]}`
	inv, _, err := b.Build(backend.Request{Prompt: "审", Schema: schema, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(inv.Args, "--output-schema")
	if i < 0 || inv.Args[i+1] != filepath.Join(work, schemaFile) {
		t.Fatalf("%q", inv.Args)
	}
	raw, err := os.ReadFile(inv.Args[i+1])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// 实测不严格的 schema 接口直接 400（testdata/schema_400.jsonl）
	if got["additionalProperties"] != false {
		t.Errorf("顶层没有 additionalProperties=false：%s", raw)
	}
	if req := got["required"].([]any); len(req) != 3 {
		t.Errorf("properties 都要进 required：%v", req)
	}
	items := got["properties"].(map[string]any)["findings"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Errorf("里层也要：%v", items)
	}
	if _, _, err := b.Build(backend.Request{Prompt: "审", Schema: schema}); err == nil {
		t.Error("没有落盘目录应该报错，不能丢掉 schema")
	}
	if _, _, err := b.Build(backend.Request{Prompt: "审", Schema: "{坏的", WorkDir: work}); err == nil {
		t.Error("schema 不是 JSON 应该报错")
	}
}

func TestBuildRefusesMustItCannotDo(t *testing.T) {
	b := New(backend.Options{})
	for _, m := range []backend.Must{backend.MustFollowup, backend.MustPatrol} {
		if _, _, err := b.Build(backend.Request{Prompt: "x", Must: m}); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("Must %b 应该拒绝：%v", m, err)
		}
	}
	if lacks := b.Caps().Lacks(backend.MustMerge); len(lacks) != 0 {
		t.Errorf("要合并的任务能做：%v", lacks)
	}
}

func TestBuildDegrades(t *testing.T) {
	_, deg, err := New(backend.Options{}).Build(backend.Request{Prompt: "x", BudgetUSD: 5, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	var caps []string
	for _, d := range deg {
		caps = append(caps, d.Cap)
	}
	if !slices.Contains(caps, "hard_budget") || !slices.Contains(caps, "max_turns") {
		t.Errorf("没有美元和回合上限要如实说：%v", deg)
	}
}

func TestParseFixtures(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	exit := backend.Exit{At: time.Date(2027, 3, 1, 10, 0, 0, 0, loc)}
	cases := []struct {
		fixture string
		check   func(t *testing.T, r backend.Result)
	}{
		{"success.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || r.Failed || r.Text != "ok" || r.SessionID != "01a1adf5-1a35-7091-9748-0a21efb18c8d" {
				t.Errorf("%+v", r)
			}
			if r.Usage == nil || r.UsageCumulative || !r.Usage.CostUnknown || r.Usage.Input != 27902-12288 || r.Usage.CacheRead != 12288 || r.Usage.Output != 5 {
				t.Errorf("用量 %+v", r.Usage)
			}
			if r.Structured != nil {
				t.Errorf("不是 JSON 不算结构化：%s", r.Structured)
			}
		}},
		{"resume.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || r.Failed || r.Text != "again" || r.SessionID == "" || r.UsageCumulative {
				t.Errorf("%+v", r)
			}
		}},
		{"auth_401.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || !r.Failed || r.Stop != backend.StopAuth || r.HTTPStatus != 401 || r.LimitText {
				t.Errorf("%+v", r)
			}
		}},
		{"schema_400.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || !r.Failed || r.HTTPStatus != 400 || r.Stop != backend.StopNone || r.LimitText {
				t.Errorf("%+v", r)
			}
		}},
		{"schema_ok.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || r.Failed || string(r.Structured) != `{"verdict":"pass","summary":"ok","findings":[]}` {
				t.Errorf("%+v %s", r, r.Structured)
			}
		}},
		{"killed_midway.jsonl", func(t *testing.T, r backend.Result) {
			if r.HaveTerminal || r.Failed || r.SessionID == "" || r.LimitText || r.Stop != backend.StopNone {
				t.Errorf("中途被杀：不猜成功，也不猜失败原因；会话 id 要留着续跑：%+v", r)
			}
		}},
		{"limit_try_at.jsonl", func(t *testing.T, r backend.Result) {
			if !r.HaveTerminal || !r.Failed || !r.LimitText || r.LimitStructured || r.Stop != backend.StopNone {
				t.Errorf("%+v", r)
			}
			if want := time.Date(2027, 3, 1, 15, 6, 0, 0, loc); !r.ResetAt.Equal(want) {
				t.Errorf("重置时间 %v，应为 %v", r.ResetAt, want)
			}
		}},
		{"limit_date.jsonl", func(t *testing.T, r backend.Result) {
			if want := time.Date(2027, 3, 2, 15, 6, 0, 0, loc); !r.LimitText || !r.ResetAt.Equal(want) {
				t.Errorf("%+v，应为 %v", r, want)
			}
		}},
		{"limit_429.jsonl", func(t *testing.T, r backend.Result) {
			if !r.LimitStructured || !r.LimitText || r.HTTPStatus != 429 || !r.ResetAt.IsZero() {
				t.Errorf("%+v", r)
			}
		}},
	}
	b := New(backend.Options{})
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) { c.check(t, b.Parse(jobWith(t, c.fixture), exit)) })
	}
}

func writeResult(t *testing.T, text, stderr string) backend.JobRef {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resultFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte(stderr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return backend.JobRef{ID: "j", Dir: dir}
}

func TestParseEdges(t *testing.T) {
	b := New(backend.Options{})
	t.Run("文件都没有", func(t *testing.T) {
		if r := b.Parse(backend.JobRef{Dir: t.TempDir()}, backend.Exit{}); r.HaveTerminal {
			t.Errorf("%+v", r)
		}
	})
	t.Run("续跑 id 不存在：只有 stderr", func(t *testing.T) {
		r := b.Parse(writeResult(t, "", "Error: thread/resume: thread/resume failed: no rollout found for thread id x (code -32600)\n"), backend.Exit{})
		if r.HaveTerminal || r.LimitText || r.Stop != backend.StopNone {
			t.Errorf("%+v", r)
		}
	})
	t.Run("最后一行写了一半", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"thread.started","thread_id":"t1"}`+"\n"+`{"type":"turn.compl`, ""), backend.Exit{})
		if r.HaveTerminal || r.SessionID != "t1" {
			t.Errorf("%+v", r)
		}
	})
	t.Run("模型正文说撞限不算", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"You've hit your usage limit. refresh token 401 Unauthorized"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
		if !r.HaveTerminal || r.Failed || r.LimitText || r.LimitStructured || r.Stop != backend.StopNone || r.HTTPStatus != 0 {
			t.Errorf("%+v", r)
		}
	})
	t.Run("error 事件是最后一个事件", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"error","message":"stream disconnected"}
`, ""), backend.Exit{})
		if !r.HaveTerminal || !r.Failed || r.Text != "stream disconnected" {
			t.Errorf("%+v", r)
		}
	})
	t.Run("重连提示后面接着做完", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 503)"}
{"type":"item.completed","item":{"type":"agent_message","text":"好了"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
		if !r.HaveTerminal || r.Failed || r.Text != "好了" || r.HTTPStatus != 0 {
			t.Errorf("%+v", r)
		}
	})
	t.Run("只有重连提示就断了：没有终态", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 503)"}
`, ""), backend.Exit{})
		if r.HaveTerminal {
			t.Errorf("%+v", r)
		}
	})
	t.Run("钩子把模型叫回来的多轮：看最后一轮，用量加起来", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"一"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":2}}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"二"}}
{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
		if !r.HaveTerminal || r.Text != "二" || r.Usage.Input != 14 || r.Usage.Output != 4 || r.Usage.CacheRead != 1 {
			t.Errorf("%+v %+v", r, r.Usage)
		}
	})
	t.Run("一轮做完又开了一轮没结束：没有终态", func(t *testing.T) {
		r := b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":2}}
{"type":"turn.started"}
`, ""), backend.Exit{})
		if r.HaveTerminal {
			t.Errorf("%+v", r)
		}
	})
	t.Run("没有终态、stderr 说撞限", func(t *testing.T) {
		r := b.Parse(writeResult(t, "", "You've hit your usage limit. Try again later.\n"), backend.Exit{})
		if r.HaveTerminal || !r.LimitText {
			t.Errorf("%+v", r)
		}
	})
	t.Run("没有终态、stderr 说没登录：认得出认证失败", func(t *testing.T) {
		r := b.Parse(writeResult(t, "", "Error: Not logged in\n"), backend.Exit{})
		if r.Stop != backend.StopAuth || !r.HaveTerminal {
			t.Errorf("%+v", r)
		}
	})
}

// 前面有一条写着 pass 的消息，结尾的失败事件没带文字：也是失败，结构化输出不算数。过审查门靠它。
func TestParseFailureWithoutText(t *testing.T) {
	const pass = `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\",\"findings\":[]}"}}
`
	for _, tail := range []string{
		`{"type":"turn.failed"}`,
		`{"type":"turn.failed","error":{"message":""}}`,
		`{"type":"turn.failed","error":{"detail":"x"}}`,
		`{"type":"error"}`,
		`{"type":"error","message":""}`,
		`{"type":"turn.failed","error":"字符串"}`,
		`{"type":"turn.failed","error":{"message":5}}`,
		`{"type":"error","message":{"x":1}}`,
	} {
		r := New(backend.Options{}).Parse(writeResult(t, pass+tail+"\n", ""), backend.Exit{})
		if !r.HaveTerminal || !r.Failed {
			t.Errorf("%s：要算失败：%+v", tail, r)
		}
		if r.Structured != nil {
			t.Errorf("%s：失败的结果不产出结构化输出：%s", tail, r.Structured)
		}
		if strings.TrimSpace(r.Text) == "" || strings.Contains(r.Text, "verdict") {
			t.Errorf("%s：要给说明，不能拿前面的消息当文字：%q", tail, r.Text)
		}
	}
}

// 失败后不会因为后面多出一条 turn.completed 翻成成功；重新开一轮才算。
func TestParseFailureStays(t *testing.T) {
	b := New(backend.Options{})
	r := b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"turn.failed"}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
	if !r.Failed {
		t.Errorf("%+v", r)
	}
	r = b.Parse(writeResult(t, `{"type":"turn.started"}
{"type":"turn.failed"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"好了"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
	if r.Failed || r.Text != "好了" {
		t.Errorf("%+v", r)
	}
}

// 结构化输出只看最后一轮：这一轮没说话，不能用上一轮的 pass。
func TestParseStructuredOnlyLastTurn(t *testing.T) {
	r := New(backend.Options{}).Parse(writeResult(t, `{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\"}"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
{"type":"turn.started"}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}
`, ""), backend.Exit{})
	if r.Structured != nil {
		t.Errorf("%s", r.Structured)
	}
}

func TestParse403IsNotAuth(t *testing.T) {
	r := New(backend.Options{}).Parse(writeResult(t, `{"type":"turn.started"}
{"type":"turn.failed","error":{"message":"unexpected status 403 Forbidden: model not available"}}
`, ""), backend.Exit{})
	if !r.HaveTerminal || !r.Failed || r.Stop != backend.StopNone || r.HTTPStatus != 403 {
		t.Errorf("403 走普通失败：%+v", r)
	}
}

func TestToolEventsTooBig(t *testing.T) {
	job := writeResult(t, `{"type":"thread.started","thread_id":"t1"}`+"\n", "")
	if err := os.Truncate(filepath.Join(job.Dir, resultFile), maxToolLogBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(backend.Options{}).(backend.ToolLog).ToolEvents(job, true); err == nil {
		t.Error("太大的事件流当未知")
	}
}

func TestParseResetTimeEdges(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2027, 3, 1, 23, 50, 0, 0, loc)
	if got, ok := parseResetTime("Try again at 12:10 AM.", now); !ok || !got.Equal(time.Date(2027, 3, 2, 0, 11, 0, 0, loc)) {
		t.Errorf("跨午夜算明天：%v %v", got, ok)
	}
	if got, ok := parseResetTime("or try again at 11:30 PM.", now); !ok || !got.Equal(time.Date(2027, 3, 1, 23, 31, 0, 0, loc)) {
		t.Errorf("刚过去不到一小时算今天：%v %v", got, ok)
	}
	for _, s := range []string{"Try again later.", "try again at 25:99 PM", "Try again at Mar 2nd, 2020 3:05 PM.", "Try again at Mar 2nd, 2029 3:05 PM."} {
		if got, ok := parseResetTime(s, now); ok {
			t.Errorf("%q 不该解析出 %v", s, got)
		}
	}
}

func TestToolEventsAndActivity(t *testing.T) {
	b := New(backend.Options{})
	job := jobWith(t, "killed_midway.jsonl")
	if _, ok := b.LastActivity(job); !ok {
		t.Fatal("读得到事件流就判断得了")
	}
	log := b.(backend.ToolLog)
	main, subs, err := log.ToolEvents(job, true)
	if err != nil || subs != nil {
		t.Fatalf("%v %v", err, subs)
	}
	if len(main) != 2 || main[0].Name != "Bash" || main[1].Name != "Bash" || !strings.Contains(main[1].Cmd, "git commit") {
		t.Fatalf("%+v", main)
	}
	if main[0].At.IsZero() {
		t.Error("事件时间取首次看到的时刻，不能是零值")
	}
	// 格式变了：当未知，不报信号
	if _, _, err := log.ToolEvents(writeResult(t, `{"type":"session.new"}`+"\n", ""), true); err == nil {
		t.Error("认不出的事件流要报错")
	}
	if _, _, err := log.ToolEvents(backend.JobRef{Dir: t.TempDir()}, true); err == nil {
		t.Error("没有事件流要报错")
	}
	if at, ok := b.LastActivity(backend.JobRef{Dir: t.TempDir()}); !ok || !at.IsZero() {
		t.Errorf("还没有动静：%v %v", at, ok)
	}
}

func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuth(t *testing.T) {
	cases := []struct {
		name, body string
		want       backend.AuthState
	}{
		{"已登录", "echo 'Logged in using ChatGPT'\n", backend.AuthIn},
		{"登录信息打在 stderr", "echo 'Logged in using ChatGPT' >&2\n", backend.AuthIn},
		{"没登录", "echo 'Not logged in'\nexit 1\n", backend.AuthOut},
		{"退 0 也说没登录", "echo 'Not logged in'\n", backend.AuthOut},
		{"报错", "echo boom\nexit 2\n", backend.AuthError},
		{"看不懂", "echo hello\n", backend.AuthError},
	}
	for _, c := range cases {
		got := New(backend.Options{Path: fakeCLI(t, c.body)}).Auth(context.Background())
		if got.State != c.want {
			t.Errorf("%s：%+v", c.name, got)
		}
	}
	if got := New(backend.Options{Path: "/不存在/codex"}).Auth(context.Background()); got.State != backend.AuthError {
		t.Errorf("CLI 不在：%+v", got)
	}
}

func TestProbe(t *testing.T) {
	good := `case "$1 $2" in
"--version "*) echo 'codex-cli 0.166.0' ;;
"exec --help") echo '--json --output-schema resume' ;;
"login status") echo 'Logged in using ChatGPT' ;;
esac
`
	env := func(dirs ...string) []string {
		return []string{"PATH=" + strings.Join(append(dirs, "/usr/bin", "/bin"), ":"), "HOME=" + t.TempDir()}
	}
	b := New(backend.Options{})
	home := t.TempDir()
	t.Run("都好", func(t *testing.T) {
		path := fakeCLI(t, good)
		r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(filepath.Dir(path))})
		if err != nil || r.Path != path || r.Version != "codex-cli 0.166.0" || !r.Headless || !r.Authenticated || r.AuthMethod != "ChatGPT" {
			t.Errorf("%+v %v", r, err)
		}
	})
	t.Run("配置指定路径", func(t *testing.T) {
		path := fakeCLI(t, good)
		if r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: path, Env: env()}); err != nil || r.Path != path {
			t.Errorf("%+v %v", r, err)
		}
	})
	t.Run("没装", func(t *testing.T) {
		if _, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env()}); !errors.Is(err, backend.ErrNoCLI) {
			t.Errorf("%v", err)
		}
	})
	t.Run("桌面端里带的不算", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ChatGPT.app", "Contents", "Resources")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"+good), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(dir)}); !errors.Is(err, backend.ErrNoCLI) {
			t.Errorf("%v", err)
		}
	})
	t.Run("没登录", func(t *testing.T) {
		path := fakeCLI(t, strings.Replace(good, "echo 'Logged in using ChatGPT'", "echo 'Not logged in'; exit 1", 1))
		r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(filepath.Dir(path))})
		if !errors.Is(err, backend.ErrNotLoggedIn) || r.Path != path || r.Authenticated {
			t.Errorf("%+v %v", r, err)
		}
	})
	t.Run("不是 Codex", func(t *testing.T) {
		path := fakeCLI(t, "echo other\n")
		if _, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(filepath.Dir(path))}); !errors.Is(err, backend.ErrNoCLI) {
			t.Errorf("%v", err)
		}
	})
	t.Run("缺无头参数", func(t *testing.T) {
		path := fakeCLI(t, strings.Replace(good, "--output-schema", "", 1))
		r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(filepath.Dir(path))})
		if err == nil || r.Headless {
			t.Errorf("%+v %v", r, err)
		}
	})
	t.Run("API key 登录：只记种类，密钥片段不进结果", func(t *testing.T) {
		path := fakeCLI(t, strings.Replace(good, "echo 'Logged in using ChatGPT'", "echo 'Logged in using an API key - sk-abc***'", 1))
		r, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Env: env(filepath.Dir(path))})
		if err != nil || r.AuthMethod != "API key" {
			t.Errorf("%+v %v", r, err)
		}
	})
}

func TestCanResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	day := filepath.Join(home, ".codex", "sessions", "2026", "11", "06")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "01a1adf5-1a35-7091-9748-0a21efb18c8d"
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-11-06T07-04-45-"+id+".jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b := New(backend.Options{Home: home})
	if !b.CanResume(id) {
		t.Error("会话文件在就能接上")
	}
	for _, bad := range []string{"", "01a1adf5-0000", "../../x", "a*b", id + "/.."} {
		if b.CanResume(bad) {
			t.Errorf("%q 不该接得上", bad)
		}
	}
	other := t.TempDir()
	t.Setenv("CODEX_HOME", other)
	if b.CanResume(id) {
		t.Error("CODEX_HOME 换了就去那里找")
	}
}

func TestEnvKeepOnlyLoginLocation(t *testing.T) {
	keep := New(backend.Options{}).EnvKeep()
	if !slices.Equal(keep, []string{"CODEX_HOME"}) {
		t.Errorf("ChatGPT 登录只要 CODEX_HOME，API key 会换收费入口：%v", keep)
	}
}

func TestRulesAndSkillPaths(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	b := New(backend.Options{})
	file, shadow := b.(backend.RulesBlock).RulesFile("/h")
	if file != "/h/.codex/AGENTS.md" || shadow != "/h/.codex/AGENTS.override.md" {
		t.Errorf("%s %s", file, shadow)
	}
	t.Setenv("CODEX_HOME", "/elsewhere")
	if file, _ := b.(backend.RulesBlock).RulesFile("/h"); file != "/elsewhere/AGENTS.md" {
		t.Error(file)
	}
	if d := b.(backend.SkillHome).SkillsDir("/h"); d != "/h/.agents/skills" {
		t.Error(d)
	}
	p := b.(backend.Permissions)
	if !slices.Contains(p.PermissionModes(), p.FullPermission()) || p.FullPermission() != "danger-full-access" {
		t.Errorf("%v %s", p.PermissionModes(), p.FullPermission())
	}
	if !strings.Contains(p.PermissionNote(), "OpenAI") {
		t.Error("同意时要说代码发给谁")
	}
	if _, ok := b.(backend.GlobalRules); ok {
		t.Error("Codex 没有规则目录，不能走目录方式")
	}
}
