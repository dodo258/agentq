package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/backend"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jobWithBytes(t *testing.T, b []byte, stderr string) backend.JobRef {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resultFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte(stderr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return backend.JobRef{ID: "j", Dir: dir}
}

func jobWith(t *testing.T, name string) backend.JobRef {
	t.Helper()
	return jobWithBytes(t, fixture(t, name), "")
}

func TestBuildArgs(t *testing.T) {
	b := New(backend.Options{Path: "/x/pi"})
	cases := []struct {
		name string
		req  backend.Request
		want []string
	}{
		{"留空的都不传，权限不变成参数", backend.Request{Prompt: "做", Dir: "/p", Permission: permFull},
			[]string{"-p", "--mode", "json"}},
		{"新会话、模型、强度、显示名", backend.Request{Prompt: "做", Session: "0a1b-2c", Model: "anthropic/claude-x", Effort: "xhigh", Label: "agentq p 标题"},
			[]string{"-p", "--mode", "json", "--session-id", "0a1b-2c", "--model", "anthropic/claude-x", "--thinking", "xhigh", "--name", "agentq p 标题"}},
		{"续跑用 --session，不再预设 id", backend.Request{Prompt: "接着", Resume: "0a1b-2c", Session: "zzz"},
			[]string{"-p", "--mode", "json", "--session", "0a1b-2c"}},
		{"审查不信任候选分支上的 .pi", backend.Request{Prompt: "审", Profile: backend.ProfileReview, Effort: "medium"},
			[]string{"-p", "--mode", "json", "--thinking", "medium", "--no-approve"}},
		{"图片用 @路径，带逗号也行", backend.Request{Prompt: "看图", Images: []string{"/s/a, b.png", "/s/c.png"}},
			[]string{"-p", "--mode", "json", "@/s/a, b.png", "@/s/c.png"}},
	}
	for _, c := range cases {
		inv, _, err := b.Build(c.req)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if !slices.Equal(inv.Args, c.want) {
			t.Errorf("%s：\n得到 %q\n应为 %q", c.name, inv.Args, c.want)
		}
		if inv.Stdin != c.req.Prompt || inv.Path != "/x/pi" || !slices.Equal(inv.EnvKeep, envKeep) {
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

// API Key 不放行：放了任务就可能改走别的收费入口。
func TestEnvKeepHasNoKeys(t *testing.T) {
	for _, k := range envKeep {
		if strings.Contains(k, "KEY") || strings.Contains(k, "TOKEN") {
			t.Errorf("%s", k)
		}
	}
}

func TestBuildSchemaByPrompt(t *testing.T) {
	b := New(backend.Options{})
	schema := `{"type":"object"}`
	inv, _, err := b.Build(backend.Request{Prompt: "审", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Stdin != "审"+backend.SchemaPrompt(schema) {
		t.Errorf("%q", inv.Stdin)
	}
	// runner 已经接过要求的不再接一遍
	inv, _, _ = b.Build(backend.Request{Prompt: "审" + backend.SchemaPrompt(schema), Schema: schema})
	if strings.Count(inv.Stdin, "JSON Schema") != 1 {
		t.Errorf("%q", inv.Stdin)
	}
}

// 只读和无工具 pi 做不到（read 不限位置），要它们的任务不启动，不偷偷换成能读密钥的样子。
func TestBuildRefusesProfiles(t *testing.T) {
	b := New(backend.Options{})
	for _, m := range []backend.Must{backend.MustFollowup, backend.MustDiscuss, backend.MustPatrol, backend.MustSchema} {
		if _, _, err := b.Build(backend.Request{Prompt: "x", Profile: backend.ProfileReadOnly, Must: m}); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("%v：%v", m, err)
		}
	}
	for _, m := range []backend.Must{backend.MustMerge, backend.MustImages} {
		if _, _, err := b.Build(backend.Request{Prompt: "x", Must: m}); err != nil {
			t.Errorf("%v：%v", m, err)
		}
	}
	_, deg, _ := b.Build(backend.Request{Prompt: "x", BudgetUSD: 5, MaxTurns: 3})
	if len(deg) != 2 || !strings.Contains(deg[0].Note, "tick") {
		t.Errorf("预算靠 tick 读运行中用量，回合上限不生效：%+v", deg)
	}
}

func TestParseSuccess(t *testing.T) {
	b := New(backend.Options{})
	r := b.Parse(jobWith(t, "success.jsonl"), backend.Exit{})
	if !r.HaveTerminal || r.Failed || r.Text != "pong" || r.SessionID != "agentq-test-1" || r.Turns != 1 || r.Structured != nil {
		t.Fatalf("%+v", r)
	}
	if r.Usage == nil || r.Usage.Input != 3529 || r.Usage.Output != 3 || r.Usage.CostUnknown || r.Usage.CostUSD <= 0 || r.UsageCumulative {
		t.Errorf("用量 %+v", r.Usage)
	}
	if r.LimitText || r.LimitStructured || r.Stop != backend.StopNone || r.HTTPStatus != 0 {
		t.Errorf("%+v", r)
	}
}

// 续跑：事件流里只有这一次的消息，用量是这一次的，会话 id 不变。
func TestParseResume(t *testing.T) {
	r := New(backend.Options{}).Parse(jobWith(t, "resume.jsonl"), backend.Exit{})
	if !r.HaveTerminal || r.Failed || r.Text != "pong" || r.SessionID != "agentq-test-1" || r.Usage == nil || r.Usage.Output > 10 {
		t.Fatalf("%+v %+v", r, r.Usage)
	}
}

func TestParseToolUseAndJSON(t *testing.T) {
	b := New(backend.Options{})
	r := b.Parse(jobWith(t, "tool_bash.jsonl"), backend.Exit{})
	if !r.HaveTerminal || r.Failed || r.Text != "done" || r.Turns != 2 {
		t.Fatalf("%+v", r)
	}
	r = b.Parse(jobWith(t, "json_final.jsonl"), backend.Exit{})
	if r.Failed || string(r.Structured) != `{"verdict":"pass","summary":"ok","findings":[]}` {
		t.Fatalf("最后一条整条是 JSON 才算结构化输出：%+v", r)
	}
}

func TestWholeJSON(t *testing.T) {
	for text, ok := range map[string]bool{
		`{"a":1}`:                      true,
		"```json\n{\"a\":1}\n```":      true,
		"```\n{\"a\":1}\n```":          true,
		"结论如下：{\"a\":1}":               false,
		"{\"a\":1}\n还有一句":              false,
		"```json\n{\"a\":1}\n```\n多一句": false,
		"[1]":                          false,
	} {
		if _, got := wholeJSON(text); got != ok {
			t.Errorf("%q：%v", text, got)
		}
	}
}

// 模型报错、撞限、认证失败：退出码是 0，只在 message_end 的 stopReason 和 errorMessage 里（实测）。
func TestParseErrors(t *testing.T) {
	b := New(backend.Options{})
	cases := []struct {
		file   string
		status int
		limit  bool
		stop   backend.Stop
	}{
		{"error_429.jsonl", 429, true, backend.StopNone},
		{"error_401.jsonl", 401, false, backend.StopAuth},
		{"auth_401_real.jsonl", 401, false, backend.StopAuth},
		{"error_500_retry.jsonl", 500, false, backend.StopNone},
		{"stream_cut_retry.jsonl", 0, false, backend.StopNone},
	}
	for _, c := range cases {
		r := b.Parse(jobWith(t, c.file), backend.Exit{})
		if !r.HaveTerminal || !r.Failed || r.HTTPStatus != c.status || r.LimitStructured != c.limit || r.LimitText != c.limit || r.Stop != c.stop || r.Text == "" || r.Structured != nil {
			t.Errorf("%s：%+v", c.file, r)
		}
	}
}

func TestParseLength(t *testing.T) {
	r := New(backend.Options{}).Parse(jobWith(t, "length.jsonl"), backend.Exit{})
	if !r.HaveTerminal || !r.Failed || !strings.Contains(r.Text, "截断") || !strings.Contains(r.Text, "partial ans") {
		t.Fatalf("%+v", r)
	}
}

// 中止：进程被杀没有 agent_settled（实测 SIGTERM），不猜成功；agent_settled.aborted 为真算失败。
func TestParseAborted(t *testing.T) {
	b := New(backend.Options{})
	r := b.Parse(jobWith(t, "killed_sigterm.jsonl"), backend.Exit{})
	if r.HaveTerminal || r.SessionID != "abort-TERM" {
		t.Fatalf("被杀的没有终态，会话 id 还在（续跑要它）：%+v", r)
	}
	ok := fixture(t, "success.jsonl")
	aborted := strings.Replace(string(ok), `"stopReason":"stop"`, `"stopReason":"aborted"`, -1)
	aborted = strings.Replace(aborted, `{"type":"agent_settled","aborted":false}`, `{"type":"agent_settled","aborted":true}`, 1)
	r = b.Parse(jobWithBytes(t, []byte(aborted), ""), backend.Exit{})
	if !r.HaveTerminal || !r.Failed || !strings.Contains(r.Text, "中止") {
		t.Fatalf("%+v", r)
	}
	onlyFlag := strings.Replace(string(ok), `{"type":"agent_settled","aborted":false}`, `{"type":"agent_settled","aborted":true}`, 1)
	if r = b.Parse(jobWithBytes(t, []byte(onlyFlag), ""), backend.Exit{}); !r.Failed {
		t.Fatalf("aborted 为真就不算成功：%+v", r)
	}
}

// 丢了最后的事件、最后一行只写了一半：没有终态，不猜成功。
func TestParseTruncated(t *testing.T) {
	b := New(backend.Options{})
	lines := strings.SplitAfter(strings.TrimRight(string(fixture(t, "success.jsonl")), "\n"), "\n")
	noSettled := strings.Join(lines[:len(lines)-1], "")
	if strings.Contains(noSettled, "agent_settled") {
		t.Fatal("夹具变了")
	}
	if r := b.Parse(jobWithBytes(t, []byte(noSettled), ""), backend.Exit{}); r.HaveTerminal || r.Failed || r.SessionID != "agentq-test-1" {
		t.Errorf("丢最后事件：%+v", r)
	}
	half := noSettled + lines[len(lines)-1][:10]
	if r := b.Parse(jobWithBytes(t, []byte(half), ""), backend.Exit{}); r.HaveTerminal {
		t.Errorf("半行：%+v", r)
	}
	if r := b.Parse(backend.JobRef{Dir: t.TempDir()}, backend.Exit{}); r.HaveTerminal || r.Usage != nil {
		t.Errorf("没有输出：%+v", r)
	}
}

// 没有凭据：只有 session 头，stderr 说 No API key found，退出码 1（实测）。认成认证失败，有终态。
func TestParseNoAPIKey(t *testing.T) {
	j := jobWithBytes(t, fixture(t, "no_api_key.jsonl"), string(fixture(t, "no_api_key.stderr")))
	r := New(backend.Options{}).Parse(j, backend.Exit{})
	if !r.HaveTerminal || !r.Failed || r.Stop != backend.StopAuth || !strings.Contains(r.Text, "No API key found") {
		t.Fatalf("%+v", r)
	}
}

// 撞限只看错误通道：模型正文里写「usage limit」不算。
func TestParseLimitIgnoresModelText(t *testing.T) {
	ok := strings.Replace(string(fixture(t, "success.jsonl")), `"text":"pong"`, `"text":"429: You have hit your usage limit"`, -1)
	r := New(backend.Options{}).Parse(jobWithBytes(t, []byte(ok), ""), backend.Exit{})
	if r.Failed || r.LimitText || r.LimitStructured || r.HTTPStatus != 0 {
		t.Fatalf("%+v", r)
	}
}

// 正常结束时 stderr 不当错误通道：扩展往 stderr 打的「rate limit」「not logged in」不让成功的任务变成撞限或认证失败。
func TestParseSuccessIgnoresStderr(t *testing.T) {
	r := New(backend.Options{}).Parse(jobWithBytes(t, fixture(t, "success.jsonl"), "ext: rate limit 429\next: not logged in, 401\n"), backend.Exit{})
	if !r.HaveTerminal || r.Failed || r.LimitText || r.LimitStructured || r.Stop != backend.StopNone || r.HTTPStatus != 0 {
		t.Fatalf("%+v", r)
	}
}

// 价目表里没有这个模型时金额一直是 0：当未知，不当免费。
func TestUsageZeroCostIsUnknown(t *testing.T) {
	r := New(backend.Options{}).Parse(jobWith(t, "error_500_retry.jsonl"), backend.Exit{})
	if r.Usage == nil || r.Usage.Input != 0 {
		t.Fatalf("%+v", r.Usage)
	}
	free := strings.NewReplacer(`"total":0.00053115`, `"total":0`, `"input":0.0005293499999999999`, `"input":0`, `"output":1.8e-06`, `"output":0`).Replace(string(fixture(t, "success.jsonl")))
	r = New(backend.Options{}).Parse(jobWithBytes(t, []byte(free), ""), backend.Exit{})
	if r.Usage == nil || !r.Usage.CostUnknown {
		t.Fatalf("%+v", r.Usage)
	}
}

func TestLiveUsageAndToolEvents(t *testing.T) {
	b := &Backend{}
	j := jobWith(t, "tool_bash.jsonl")
	u := b.LiveUsage(j)
	if u == nil || u.CostUSD <= 0 || u.Output == 0 {
		t.Fatalf("%+v", u)
	}
	main, subs, err := b.ToolEvents(j, true)
	if err != nil || subs != nil || len(main) != 1 || main[0].Name != "Bash" || main[0].Cmd != "echo hi" || main[0].At.IsZero() {
		t.Fatalf("%+v %v %v", main, subs, err)
	}
	if _, _, err := b.ToolEvents(jobWithBytes(t, []byte(`{"type":"agent_start"}`+"\n"), ""), false); err == nil {
		t.Error("没有 session 头要报格式变了")
	}
}

func TestRulesFileFollowsWhatPiReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	dir := filepath.Join(home, ".pi", "agent")
	b := &Backend{o: backend.Options{Home: home}}
	file, shadow := b.RulesFile(home)
	if file != filepath.Join(dir, "AGENTS.md") || shadow != filepath.Join(dir, "AGENTS.override.md") {
		t.Fatalf("%s %s", file, shadow)
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("我的\n"), 0o644)
	if file, _ = b.RulesFile(home); filepath.Base(file) != "CLAUDE.md" {
		t.Errorf("用户只有 CLAUDE.md 时放进去，新建 AGENTS.md 会让它不再被读：%s", file)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "~/cfg")
	if file, _ = b.RulesFile(home); file != filepath.Join(home, "cfg", "AGENTS.md") {
		t.Errorf("%s", file)
	}
}

func TestCanResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	b := New(backend.Options{Home: home})
	sess := filepath.Join(home, ".pi", "agent", "sessions", "--tmp-proj--")
	os.MkdirAll(sess, 0o755)
	os.WriteFile(filepath.Join(sess, "2026-11-11T12-39-28-234Z_0a1b-2c.jsonl"), []byte("{}\n"), 0o644)
	if !b.CanResume("0a1b-2c") || b.CanResume("0a1b") || b.CanResume("*") || b.CanResume("../x") || b.CanResume("") {
		t.Error("按完整 id 找会话文件")
	}
	flat := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", flat)
	os.WriteFile(filepath.Join(flat, "2026-11-11T12-39-28-234Z_x9.jsonl"), []byte("{}\n"), 0o644)
	if !b.CanResume("x9") || b.CanResume("0a1b-2c") {
		t.Error("PI_CODING_AGENT_SESSION_DIR 下直接放")
	}
}

// fakePi 写一个假 pi：--version、--help、auth check 按参数回答。
func fakePi(t *testing.T, authJSON string, authExit int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 1.5.1 ;;\n--help) echo '--mode --session-id --no-approve' ;;\nauth) echo \"$*\" >> \"$0.calls\"; echo '" + authJSON + "'; exit " + string(rune('0'+authExit)) + " ;;\nesac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSettings(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAuth(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	home := t.TempDir()
	ctx := context.Background()
	ready := fakePi(t, `{"status":"ready","provider":"magpie","authType":"api_key"}`, 0)
	if a := New(backend.Options{Home: home, Path: ready}).Auth(ctx); a.State != backend.AuthUnknown {
		t.Errorf("没选过模型来源：查不了，照常启动：%+v", a)
	}
	writeSettings(t, home, `{"defaultProvider":"magpie","defaultModel":"m"}`)
	if a := New(backend.Options{Home: home, Path: ready}).Auth(ctx); a.State != backend.AuthIn || !a.Unverified {
		t.Errorf("%+v", a)
	}
	if b, _ := os.ReadFile(ready + ".calls"); !strings.Contains(string(b), "auth check --provider magpie --json") {
		t.Errorf("%s", b)
	}
	out := fakePi(t, `{"status":"not_ready","provider":"magpie","reason":"credentials_not_configured"}`, 1)
	if a := New(backend.Options{Home: home, Path: out}).Auth(ctx); a.State != backend.AuthOut || !strings.Contains(a.Detail, "pi auth login magpie") {
		t.Errorf("%+v", a)
	}
	bad := fakePi(t, `???`, 0)
	if a := New(backend.Options{Home: home, Path: bad}).Auth(ctx); a.State != backend.AuthError {
		t.Errorf("%+v", a)
	}
}

// backends.pi.model 写成「提供商/模型」：查的是这个模型的来源，交给 pi 按模型认；只写模型名还查默认来源。
func TestAuthFollowsConfiguredModel(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	home := t.TempDir()
	ctx := context.Background()
	writeSettings(t, home, `{"defaultProvider":"magpie","defaultModel":"m"}`)
	out := fakePi(t, `{"status":"not_ready","provider":"openrouter","reason":"credentials_not_configured"}`, 1)
	a := New(backend.Options{Home: home, Path: out, Model: "qwen/qwen3-coder"}).Auth(ctx)
	if a.State != backend.AuthOut || !strings.Contains(a.Detail, "qwen/qwen3-coder") || !strings.Contains(a.Detail, "pi auth login openrouter") {
		t.Errorf("%+v", a)
	}
	if b, _ := os.ReadFile(out + ".calls"); strings.TrimSpace(string(b)) != "auth check --model qwen/qwen3-coder --json" {
		t.Errorf("要按配置的模型查：%s", b)
	}
	ready := fakePi(t, `{"status":"ready","provider":"magpie","authType":"api_key"}`, 0)
	if a := New(backend.Options{Home: home, Path: ready, Model: "sonnet"}).Auth(ctx); a.State != backend.AuthIn {
		t.Errorf("%+v", a)
	}
	if b, _ := os.ReadFile(ready + ".calls"); strings.TrimSpace(string(b)) != "auth check --provider magpie --json" {
		t.Errorf("只写模型名查默认来源：%s", b)
	}
	bad := fakePi(t, `{"status":"invalid","provider":"nosuch/x","reason":"invalid_state"}`, 2)
	if a := New(backend.Options{Home: home, Path: bad, Model: "nosuch/x"}).Auth(ctx); a.State != backend.AuthOut || !strings.Contains(a.Detail, "认不出配置的模型 nosuch/x") {
		t.Errorf("%+v", a)
	}
	// 没选过默认来源也能按配置的模型查
	other := t.TempDir()
	if a := New(backend.Options{Home: other, Path: ready, Model: "magpie/m"}).Auth(ctx); a.State != backend.AuthIn {
		t.Errorf("%+v", a)
	}
}

func TestProbe(t *testing.T) {
	home := t.TempDir()
	ready := fakePi(t, `{"status":"ready","provider":"magpie","authType":"api_key"}`, 0)
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	b := New(backend.Options{Home: home})
	p, err := b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: ready, Env: env})
	if !errors.Is(err, backend.ErrNotLoggedIn) || !p.Headless || p.Version != "pi 1.5.1" {
		t.Fatalf("没选模型来源算没登录：%+v %v", p, err)
	}
	writeSettings(t, home, `{"defaultProvider":"magpie"}`)
	p, err = b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: ready, Env: env})
	if err != nil || !p.Authenticated || p.AuthMethod != "magpie" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err = b.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: filepath.Join(home, "nope"), Env: env}); !errors.Is(err, backend.ErrNoCLI) {
		t.Fatal(err)
	}
}
