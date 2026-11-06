package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
)

// stubBackend 只声明权限值：这个包的测试没有注册真后端。
type stubBackend struct{ name string }

func (s stubBackend) Name() string                      { return s.name }
func (s stubBackend) Caps() backend.Caps                { return backend.Caps{} }
func (s stubBackend) EnvKeep() []string                 { return nil }
func (s stubBackend) Auth(context.Context) backend.Auth { return backend.Auth{} }
func (s stubBackend) Probe(context.Context, backend.ProbeEnv) (backend.Probe, error) {
	return backend.Probe{}, nil
}
func (s stubBackend) Build(backend.Request) (backend.Invocation, []backend.Degradation, error) {
	return backend.Invocation{}, nil, nil
}
func (s stubBackend) Parse(backend.JobRef, backend.Exit) backend.Result { return backend.Result{} }
func (s stubBackend) LastActivity(backend.JobRef) (time.Time, bool)     { return time.Time{}, false }
func (s stubBackend) CanResume(string) bool                             { return false }
func (s stubBackend) RecordedModel([]string) string                     { return "" }
func (s stubBackend) PermissionModes() []string {
	modes := []string{"default", "full-" + s.name}
	if s.name == backend.Default { // Default() 的默认值，读配置时要过
		modes = append(modes, "bypassPermissions")
	}
	return modes
}
func (s stubBackend) FullPermission() string { return "full-" + s.name }
func (s stubBackend) PermissionNote() string { return "" }

func registerStub(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(backend.Register(name, func(backend.Options) backend.Backend { return stubBackend{name} }))
}

func loadJSON(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestForBackendDoesNotLeakDefaults(t *testing.T) {
	// 手写 {"backend":"xxx"} 缺 model：默认的 opus、high、bypassPermissions 都不能传给别的后端
	c, err := loadJSON(t, `{"backend":"xxx"}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"model": c.Model, "effort": c.Effort, "permission": c.PermissionMode, "router": c.RouterModel, "suggest": c.SuggestModel, "sense": c.SenseModel} {
		if v != "" {
			t.Errorf("默认后端不是 Claude，%s 不该有默认值：%q", name, v)
		}
	}
	if got := c.ForBackend("xxx"); got.Model != "" || got.Effort != "" || got.PermissionMode != "" {
		t.Errorf("%+v", got)
	}

	// 老配置不写 backend：默认值照旧
	c, err = loadJSON(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "opus" || c.Effort != "high" || c.PermissionMode != "bypassPermissions" || c.RouterModel != "sonnet" {
		t.Errorf("老配置的默认值变了：%+v", c)
	}
	if got := c.ForBackend(""); got.Model != "opus" || got.Effort != "high" || got.PermissionMode != "bypassPermissions" {
		t.Errorf("%+v", got)
	}
	// project_backends：默认后端的不串给项目的后端
	if got := c.ForBackend("xxx"); got.Model != "" || got.Effort != "" || got.PermissionMode != "" || got.RouterModel != "" || got.LowRiskModel != "" {
		t.Errorf("Claude 的默认串给了别的后端：%+v", got)
	}

	// 顶层写了的，是默认后端自己的
	c, err = loadJSON(t, `{"backend":"xxx","model":"m1","effort":"low"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ForBackend("xxx"); got.Model != "m1" || got.Effort != "low" {
		t.Errorf("%+v", got)
	}
	// 反过来，默认后端的也不串给 Claude：老任务记录（后端名空）算 Claude
	if got := c.ForBackend(""); got.Model != "" || got.Effort != "" {
		t.Errorf("%+v", got)
	}

	// backends 里写的优先
	c, err = loadJSON(t, `{"model":"opus","project_backends":{"p":"xxx"},"backends":{"xxx":{"model":"m2","effort":"e2","permission":"p2"},"claude":{"effort":"max"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ForBackend("xxx"); got.Model != "m2" || got.Effort != "e2" || got.PermissionMode != "p2" {
		t.Errorf("%+v", got)
	}
	if got := c.ForBackend("claude"); got.Model != "opus" || got.Effort != "max" {
		t.Errorf("%+v", got)
	}
}

func TestBackendPath(t *testing.T) {
	c := Default()
	if c.BackendPath("") != "claude" || c.BackendPath("claude") != "claude" || c.BackendPath("codex") != "codex" {
		t.Error(c.BackendPath(""), c.BackendPath("codex"))
	}
	c.ClaudePath = "/x/claude"
	c.Backends = map[string]BackendConfig{"codex": {Path: "/y/codex"}}
	if c.BackendPath("claude") != "/x/claude" || c.BackendPath("codex") != "/y/codex" {
		t.Error(c.BackendPath("claude"), c.BackendPath("codex"))
	}
	c.Backends["claude"] = BackendConfig{Path: "/z/claude"}
	if c.BackendPath("") != "/z/claude" {
		t.Error(c.BackendPath(""))
	}
	c = Default()
	c.SetBackendPath("", "/x/claude")
	c.SetBackendPath("codex", "/y/codex")
	if c.ClaudePath != "/x/claude" || c.Backends["codex"].Path != "/y/codex" {
		t.Errorf("%+v", c)
	}
}

func TestBackendsInUse(t *testing.T) {
	c := Default()
	if got := strings.Join(c.BackendsInUse(), ","); got != "claude" {
		t.Error(got)
	}
	c.Backend = "b"
	c.ProjectBackends = map[string]string{"p1": "a", "p2": "b", "p3": "a"}
	if got := strings.Join(c.BackendsInUse(), ","); got != "a,b" {
		t.Error(got)
	}
}

func TestPermissionDeclaredByBackend(t *testing.T) {
	registerStub(t, "st1")
	if _, err := loadJSON(t, `{"backend":"st1","permission_mode":"full-st1"}`); err != nil {
		t.Errorf("后端声明过的值要过：%v", err)
	}
	_, err := loadJSON(t, `{"backend":"st1","permission_mode":"bypassPermissions"}`)
	if err == nil || !strings.Contains(err.Error(), "permission_mode") || !strings.Contains(err.Error(), "full-st1") {
		t.Errorf("别家的值要报错并说能写什么：%v", err)
	}
	if _, err := loadJSON(t, `{"backends":{"st1":{"permission":"nope"}}}`); err == nil || !strings.Contains(err.Error(), "backends.st1.permission") {
		t.Errorf("%v", err)
	}
	// 没注册的后端（写错了，status 和 doctor 说明）查不了，不在读配置时挡
	if _, err := loadJSON(t, `{"backend":"unknown-x","permission_mode":"anything"}`); err != nil {
		t.Errorf("%v", err)
	}
}

func TestPermissionProblem(t *testing.T) {
	registerStub(t, "st2")
	registerStub(t, "claude") // 这个包的测试没有注册真后端
	cases := []struct {
		name, body, backend string
		want                []string // 空表示没问题
	}{
		{"老配置不写", `{}`, "", nil},
		{"默认 Claude 写 default 只提示", `{"permission_mode":"default"}`, "", nil},
		{"默认后端没写", `{"backend":"st2"}`, "st2", []string{"后端 st2", "agentq start", "permission_mode=full-st2"}},
		{"默认后端写 default", `{"backend":"st2","permission_mode":"default"}`, "st2", []string{"permission_mode=full-st2"}},
		{"默认后端配好了", `{"backend":"st2","permission_mode":"full-st2"}`, "st2", nil},
		{"项目后端没配，不沿用顶层的", `{"permission_mode":"default","project_backends":{"p":"st2"}}`, "st2", []string{"backends.st2.permission=full-st2"}},
		{"项目后端配好了", `{"project_backends":{"p":"st2"},"backends":{"st2":{"permission":"full-st2"}}}`, "st2", nil},
		{"默认别家、项目 Claude", `{"backend":"st2","permission_mode":"full-st2","project_backends":{"p":"claude"}}`, "", []string{"backends.claude.permission=full-claude"}},
		{"不声明的后端查不了", `{"backend":"unknown-x"}`, "unknown-x", nil},
	}
	for _, c := range cases {
		cfg, err := loadJSON(t, c.body)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		got := cfg.PermissionProblem(c.backend)
		if (got == "") != (len(c.want) == 0) {
			t.Errorf("%s：%q", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s：%q 缺 %q", c.name, got, w)
			}
		}
		if (len(cfg.PermissionProblems()) == 0) != (len(c.want) == 0) {
			t.Errorf("%s：%v", c.name, cfg.PermissionProblems())
		}
	}
}

func TestValidateBackendsKeys(t *testing.T) {
	for _, body := range []string{
		`{"backends":{"a/b":{"model":"m"}}}`,
		`{"backends":{"a":{"model":"-x"}}}`,
		`{"backends":{"a":{"effort":"-x"}}}`,
		`{"backends":{"a":{"path":"-x"}}}`,
	} {
		if _, err := loadJSON(t, body); err == nil {
			t.Errorf("%s 应该报错", body)
		}
	}
}

// 老配置读写字节不变：没写 backends 就不出现这个键。
func TestBackendsOmittedWhenEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "backends") {
		t.Errorf("%s", b)
	}
}
