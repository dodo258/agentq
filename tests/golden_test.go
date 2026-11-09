package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/notify"
	"agentq/internal/proc"
	"agentq/internal/release"
	"agentq/internal/runner"
	"agentq/internal/sense"
	"agentq/internal/state"
	"agentq/internal/suggest"
	"agentq/internal/testkit"
)

// 模块化的行为锁：文件格式、argv、服务文件、帮助和 status 逐字节和 tests/testdata/golden 比。
// 搬代码不许改这些文件；真要改行为时用
// AGENTQ_GOLDEN_UPDATE=1 go test -run '^TestGolden' ./... 重新生成，审 diff 再提交。

// goldenTime 是黄金文件里的基准时刻。
var goldenTime = time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)

// filler 把一个值的所有可导出字段填满。字符串带字段路径，加字段时黄金文件和断言都会变。
type filler struct{ n int }

// goldenOptional 是老文件没有的键：为空时省略，黄金文件要证明老用户的字节不变，所以不填它们。写出来的样子由 TestGoldenBackendField 查。
var goldenOptional = map[string]bool{"AutopushLog": true, "BackendLimits": true, "Backend": true, "Jobs[].Backend": true, "ProjectBackends": true, "Backends": true, "Usage.CostUnknown": true, "Jobs[].Usage.CostUnknown": true,
	"Gate": true, "Jobs[].Gate": true, "GateRound": true, "Jobs[].GateRound": true, "GateScope": true, "Jobs[].GateScope": true, "GateScopeFrom": true, "Jobs[].GateScopeFrom": true, "GateContract": true, "Jobs[].GateContract": true, "Pending[].Card.GateJob": true, "Cards.value.GateJob": true,
	"DiscussKey": true, "Jobs[].DiscussKey": true, "Direction": true, "Jobs[].Direction": true, "Pending[].Card.DiscussKey": true, "Cards.value.DiscussKey": true}

func goldenSkip(path string) bool {
	_, rest, _ := strings.Cut(path, ".")
	return goldenOptional[rest]
}

func (f *filler) fill(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if goldenSkip(path) {
		return
	}
	f.n++
	switch v.Type() {
	case reflect.TypeFor[time.Time]():
		v.Set(reflect.ValueOf(goldenTime.Add(time.Duration(f.n) * time.Hour)))
		return
	case reflect.TypeFor[time.Duration]():
		v.SetInt(int64(time.Duration(f.n) * time.Minute))
		return
	case reflect.TypeFor[json.RawMessage]():
		v.SetBytes([]byte(`{"raw":` + fmt.Sprint(f.n) + `}`))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(path + "-" + fmt.Sprint(f.n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(f.n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(f.n) + 0.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(t, v.Elem(), path)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		f.fill(t, s.Index(0), path+"[]")
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		f.fill(t, k, path+".key")
		f.fill(t, e, path+".value")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if sf.IsExported() {
				f.fill(t, v.Field(i), path+"."+sf.Name)
			}
		}
	default:
		t.Fatalf("%s：填不了 %s，给 filler 加一种", path, v.Type())
	}
}

// assertNoZero：读回来的值里任何一个可导出字段是零值，就是有字段没进文件（漏标 json 或被 omitempty 吃掉）。
func assertNoZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if goldenSkip(path) {
		return
	}
	if v.IsZero() {
		t.Errorf("%s 写出再读回来是零值", path)
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		assertNoZero(t, v.Elem(), path)
	case reflect.Slice:
		for i := range v.Len() {
			assertNoZero(t, v.Index(i), path+"[]")
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			assertNoZero(t, v.MapIndex(k), path+".value")
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			if sf := v.Type().Field(i); sf.IsExported() {
				assertNoZero(t, v.Field(i), path+"."+sf.Name)
			}
		}
	}
}

func goldenFull[T any](t *testing.T, name string) *T {
	t.Helper()
	v := new(T)
	(&filler{}).fill(t, reflect.ValueOf(v).Elem(), name)
	return v
}

// goldenRoundTrip：真保存函数写出的字节要和黄金文件一样，读回来再写一遍还是这些字节，并且没有丢字段。
func goldenRoundTrip[T any](t *testing.T, name string, got []byte) {
	t.Helper()
	testkit.GoldenCompare(t, name, got)
	back := new(T)
	if err := json.Unmarshal(got, back); err != nil {
		t.Fatal(err)
	}
	assertNoZero(t, reflect.ValueOf(back).Elem(), strings.TrimSuffix(name, ".json"))
	again, err := json.MarshalIndent(back, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(again, '\n'), got) {
		t.Errorf("%s 读回来再写一遍变了：\n%s", name, again)
	}
}

func goldenApp(t *testing.T) *App {
	t.Helper()
	return wireApp(&App{Env: &env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: func() time.Time { return goldenTime }, Out: &bytes.Buffer{}}})
}

// 全字段填满的状态文件，走真保存函数。
func TestGoldenFileFormats(t *testing.T) {
	t.Run("meta", func(t *testing.T) {
		a, j := goldenApp(t), goldenFull[Job](t, "Job")
		j.ID = "0315-100000-demo"
		a.Ledger.WriteMeta(j)
		goldenRoundTrip[Job](t, "meta.json", testkit.ReadBytes(t, filepath.Join(a.JobDir(j.ID), "meta.json")))
	})
	t.Run("state", func(t *testing.T) {
		a := goldenApp(t)
		if err := state.Save(a.StatePath(), goldenFull[State](t, "State")); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[State](t, "state.json", testkit.ReadBytes(t, a.StatePath()))
	})
	t.Run("notify", func(t *testing.T) {
		a := goldenApp(t)
		if err := a.Notify.SaveNotify(goldenFull[notify.State](t, "NotifyState")); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[notify.State](t, "notify.json", testkit.ReadBytes(t, a.Notify.NotifyPath()))
	})
	t.Run("config", func(t *testing.T) {
		a := goldenApp(t)
		if err := config.Save(a.ConfigPath(), *goldenFull[config.Config](t, "Config")); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[config.Config](t, "config.json", testkit.ReadBytes(t, a.ConfigPath()))
	})
	t.Run("suggestions", func(t *testing.T) {
		a, full := goldenApp(t), goldenFull[suggest.SuggestStore](t, "suggestStore")
		if err := a.Suggest.WithSuggest(func(s *suggest.SuggestStore) error { *s = *full; return nil }); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[suggest.SuggestStore](t, "suggestions.json", testkit.ReadBytes(t, a.Suggest.SuggestionsPath()))
	})
	t.Run("sense", func(t *testing.T) {
		a, full := goldenApp(t), goldenFull[sense.SenseStore](t, "senseStore")
		if err := a.Sense.WithSense(func(s *sense.SenseStore) error { *s = *full; return nil }); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[sense.SenseStore](t, "sense.json", testkit.ReadBytes(t, a.Sense.SensePath()))
	})
	t.Run("release", func(t *testing.T) {
		a, full := goldenApp(t), goldenFull[release.ReleaseStore](t, "releaseStore")
		if err := a.Release.WithRelease(func(s *release.ReleaseStore) error { *s = *full; return nil }); err != nil {
			t.Fatal(err)
		}
		goldenRoundTrip[release.ReleaseStore](t, "release.json", testkit.ReadBytes(t, a.Release.ReleasePath()))
	})
}

// Job.Backend：为空时省略，老 meta 读出来是默认后端；非默认后端才写，读回来不变。
func TestGoldenBackendField(t *testing.T) {
	b, err := json.Marshal(Job{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "backend") {
		t.Errorf("Backend 为空时应省略：%s", b)
	}
	b, err = json.Marshal(Job{ID: "x", Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"backend":"fake"`) {
		t.Errorf("Backend 没写出去：%s", b)
	}
	var old Job
	if err := json.Unmarshal([]byte(`{"id":"x","status":"done"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Backend != "" {
		t.Errorf("老 meta 不该有后端：%q", old.Backend)
	}
	a := goldenApp(t)
	if got := a.BackendFor(old.Backend).Name(); got != backend.Default {
		t.Errorf("老 meta 应按默认后端读，得到 %s", got)
	}
	// cost_unknown：只有 token 没有金额。filler 跳过了它，键名和往返只有这里查
	u := backend.JobUsage{Input: 1}
	if b, err := json.Marshal(u); err != nil || strings.Contains(string(b), "cost_unknown") {
		t.Errorf("CostUnknown 为假时应省略：%s %v", b, err)
	}
	u.CostUnknown = true
	b, err = json.Marshal(u)
	if err != nil || !strings.Contains(string(b), `"cost_unknown":true`) {
		t.Fatalf("CostUnknown 没写出去：%s %v", b, err)
	}
	var back backend.JobUsage
	if err := json.Unmarshal(b, &back); err != nil || !back.CostUnknown {
		t.Errorf("CostUnknown 读回来丢了：%+v %v", back, err)
	}
	// 配置的两个键：同样被 filler 跳过
	cfgBytes, err := json.Marshal(config.Config{Backend: "fake", ProjectBackends: map[string]string{"demo": "fake"}})
	if err != nil || !strings.Contains(string(cfgBytes), `"backend":"fake"`) || !strings.Contains(string(cfgBytes), `"project_backends":{"demo":"fake"}`) {
		t.Errorf("配置的后端键：%s %v", cfgBytes, err)
	}
	var cfgBack config.Config
	if err := json.Unmarshal(cfgBytes, &cfgBack); err != nil || cfgBack.Backend != "fake" || cfgBack.ProjectBackends["demo"] != "fake" {
		t.Errorf("配置的后端键读回来丢了：%+v %v", cfgBack, err)
	}
}

// 老格式样本：字段少、带这个版本不认识的键。读入再写出的结果要和黄金文件一样，老用户的文件读法不能变。
func TestGoldenOldFormats(t *testing.T) {
	input := func(t *testing.T, name, dst string) {
		t.Helper()
		testkit.WriteInstall(t, dst, string(testkit.ReadBytes(t, filepath.Join("tests", "testdata", "golden", "old", name+".input.json"))), 0o600)
	}
	output := func(t *testing.T, name string, got []byte) {
		t.Helper()
		testkit.GoldenCompare(t, filepath.Join("old", name+".output.json"), got)
	}
	t.Run("state", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "state", a.StatePath())
		st, err := state.Load(a.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Save(a.StatePath(), st); err != nil {
			t.Fatal(err)
		}
		output(t, "state", testkit.ReadBytes(t, a.StatePath()))
	})
	t.Run("config", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "config", a.ConfigPath())
		cfg, err := config.Load(a.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := config.Save(a.ConfigPath(), cfg); err != nil {
			t.Fatal(err)
		}
		output(t, "config", testkit.ReadBytes(t, a.ConfigPath()))
	})
	t.Run("notify", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "notify", a.Notify.NotifyPath())
		s, err := a.Notify.LoadNotify()
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Notify.SaveNotify(s); err != nil {
			t.Fatal(err)
		}
		output(t, "notify", testkit.ReadBytes(t, a.Notify.NotifyPath()))
	})
	t.Run("suggestions", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "suggestions", a.Suggest.SuggestionsPath())
		if err := a.Suggest.WithSuggest(func(s *suggest.SuggestStore) error { return nil }); err != nil {
			t.Fatal(err)
		}
		output(t, "suggestions", testkit.ReadBytes(t, a.Suggest.SuggestionsPath()))
	})
	t.Run("sense", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "sense", a.Sense.SensePath())
		if err := a.Sense.WithSense(func(s *sense.SenseStore) error { return nil }); err != nil {
			t.Fatal(err)
		}
		output(t, "sense", testkit.ReadBytes(t, a.Sense.SensePath()))
	})
	t.Run("release", func(t *testing.T) {
		a := goldenApp(t)
		input(t, "release", a.Release.ReleasePath())
		if err := a.Release.WithRelease(func(s *release.ReleaseStore) error { return nil }); err != nil {
			t.Fatal(err)
		}
		output(t, "release", testkit.ReadBytes(t, a.Release.ReleasePath()))
	})
}

// 默认配置写出来的样子：老配置靠 defaultConfig 的语义，新配置（fresh）是装好后用户看到的文件。
func TestGoldenDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := config.Save(p, config.Default()); err != nil {
		t.Fatal(err)
	}
	testkit.GoldenCompare(t, "config-default.json", testkit.ReadBytes(t, p))
	p = filepath.Join(dir, "fresh.json")
	if _, err := config.Load(p); err != nil { // 文件不存在时写一份
		t.Fatal(err)
	}
	testkit.GoldenCompare(t, "config-fresh.json", testkit.ReadBytes(t, p))
}

// 白名单一个键都不能多、不能少：多了可能把 ANTHROPIC_API_KEY 这类东西传给任务。
func TestGoldenChildEnvKeep(t *testing.T) {
	want := []string{
		"ALL_PROXY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "HOME", "HTTPS_PROXY", "HTTP_PROXY", "LANG", "LOGNAME", "NO_PROXY", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_DB", "PATH",
		"SHELL", "SSH_AUTH_SOCK", "TMPDIR", "USER", "all_proxy", "http_proxy", "https_proxy", "no_proxy",
	}
	var got []string
	for k, v := range proc.ChildEnvKeep {
		if !v {
			t.Errorf("%s 写成了 false", k)
		}
		got = append(got, k)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("childEnvKeep 变了：\n想要 %v\n实际 %v", want, got)
	}
}

var (
	goldenSessionRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	goldenJobIDRe   = regexp.MustCompile(`\d{4}-\d{6}-demo`)
)

// goldenStart 用真的 a.start 起一个任务（真任务外壳 + 假 claude），返回假 claude 记下的参数和工作目录，以及整个日志。
func goldenStart(t *testing.T, cfg map[string]any, j *Job, resume string, prepare func(a *App, e *testEnv, j *Job)) (string, string) {
	t.Helper()
	e := newEnv(t, cfg)
	var out bytes.Buffer
	a, err := newApp(e.home, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv("AGENTQ_FAKE_LOG", e.fakeLg)
	old := runner.JobExecutable
	runner.JobExecutable = func() (string, error) { return agentqBin, nil }
	t.Cleanup(func() { runner.JobExecutable = old })
	j.ProjectDir = testkit.MkProject(t, e.root, "demo", "- [ ] 修登录\n")
	if prepare != nil {
		prepare(a, e, j)
	}
	st := &State{}
	if _, err := a.Runner.Start(st, j, "GOLDEN-PROMPT", resume); err != nil {
		t.Fatalf("启动失败：%v\n%s", err, out.String())
	}
	e.waitShells()
	logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt"))
	if len(logs) != 1 {
		t.Fatalf("假 claude 日志 %d 份：%v\n%s", len(logs), logs, out.String())
	}
	log := string(testkit.ReadBytes(t, logs[0]))
	args, _, ok := strings.Cut(log, "PPID[")
	if !ok {
		t.Fatalf("假 claude 日志不完整：\n%s", log)
	}
	// macOS 的临时目录经过软链接，工作目录是解析后的路径。解析后的放前面先换。
	realHome, _ := filepath.EvalSymlinks(e.home)
	realRoot, _ := filepath.EvalSymlinks(e.root)
	norm := strings.NewReplacer(realHome, "$HOME", realRoot, "$ROOT", e.home, "$HOME", e.root, "$ROOT", e.fakeLg, "$FAKELOG").Replace(args)
	norm = goldenSessionRe.ReplaceAllString(norm, "<SESSION>")
	norm = goldenJobIDRe.ReplaceAllString(norm, "<JOB>")
	return norm, log
}

// 每种任务传给 claude 的参数。runner 拆依赖时改的就是这里，所以走真的启动路径，不只是拼参数的函数。
func TestGoldenArgv(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		job     Job
		resume  string
		prepare func(a *App, e *testEnv, j *Job)
	}{
		{name: "task", job: Job{Project: "demo", Title: "修登录", Item: "- [ ] 修登录"}},
		{name: "task-resume", job: Job{Project: "demo", Title: "修登录", Item: "- [ ] 修登录"}, resume: "s-old"},
		{name: "task-low-risk", cfg: map[string]any{"low_risk_model": "haiku"}, job: Job{Project: "demo", Title: "改错字", Item: "- [ ] 改错字（低风险）"}},
		{name: state.KindInbox, job: Job{Kind: state.KindInbox, Project: "demo", Title: "查资料"}},
		{name: state.KindInbox + "-resume", job: Job{Kind: state.KindInbox, Project: "demo", Title: "查资料"}, resume: "s-old"},
		{name: state.KindNew, job: Job{Kind: state.KindNew, Project: "demo", Title: "做个小工具"}},
		{name: state.KindRelease, job: Job{Kind: state.KindRelease, Project: "demo", Title: "发版准备"}},
		{name: config.KindNight, job: Job{Kind: config.KindNight, Project: "demo", Title: "agentq night"}},
		{name: config.KindReview, job: Job{Kind: config.KindReview, Project: "demo", Title: "agentq review"}},
		{name: state.KindSense, job: Job{Kind: state.KindSense, Project: "demo", Title: "主动感知"}},
		{name: state.KindSuggest, job: Job{Kind: state.KindSuggest, Project: "demo", Title: "闲时找活"}},
		{name: state.KindPatrol, job: Job{Kind: state.KindPatrol, Project: "demo", Title: "巡逻分析"}},
		{name: state.KindChat, job: Job{Kind: state.KindChat, Project: "demo", Title: "对话"}},
		{name: state.KindQuick, job: Job{Kind: state.KindQuick, Project: "demo", Title: "快问"}},
		{name: state.KindRouter, job: Job{Kind: state.KindRouter, Project: "demo", Title: "看消息"}},
		{name: state.KindFollowup, job: Job{Kind: state.KindFollowup, Project: "demo", Title: "接着问"}, resume: "s-old"},
		{name: state.KindFollowup + "-files", job: Job{Kind: state.KindFollowup, Project: "demo", Title: "接着问"}, resume: "s-old", prepare: func(a *App, e *testEnv, j *Job) {
			j.ID = "0315-100000-demo"
			if err := os.MkdirAll(a.Chat.ChatFilesDir(j.ID), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	var all bytes.Buffer
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := c.job
			got, _ := goldenStart(t, c.cfg, &j, c.resume, c.prepare)
			fmt.Fprintf(&all, "==== %s\n%s\n", c.name, got)
		})
	}
	if !t.Failed() {
		testkit.GoldenCompare(t, "argv.txt", all.Bytes())
	}
}

// 真正到了假 claude 手里的环境变量：白名单全部设上，再放几个不该过去的。
func TestGoldenChildEnvAtClaude(t *testing.T) {
	for k := range proc.ChildEnvKeep {
		switch k {
		case "HOME", "PATH":
		case "TMPDIR":
			t.Setenv(k, os.TempDir())
		default:
			t.Setenv(k, "golden")
		}
	}
	t.Setenv("LC_GOLDEN", "keep")
	t.Setenv("AGENTQ_GOLDEN", "keep")
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "GOFLAGS", "TELEGRAM_BOT_TOKEN"} {
		t.Setenv(k, "drop")
	}
	j := &Job{Project: "demo", Title: "修登录", Item: "- [ ] 修登录"}
	_, log := goldenStart(t, nil, j, "", nil)
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^ENV\[([A-Za-z0-9_]+)=`).FindAllStringSubmatch(log, -1) {
		name := m[1]
		switch name {
		case "PWD", "OLDPWD", "SHLVL", "_": // sh 自己加的
			continue
		}
		// 外面可能带着别的 LC_*、AGENTQ_*：这两类是按前缀放行的，只记这里设的。
		if (strings.HasPrefix(name, "LC_") && name != "LC_GOLDEN") ||
			(strings.HasPrefix(name, "AGENTQ_") && name != "AGENTQ_GOLDEN" && name != "AGENTQ_JOB" && name != "AGENTQ_FAKE_LOG") {
			continue
		}
		seen[name] = true
	}
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	testkit.GoldenCompare(t, "child-env.txt", []byte(strings.Join(names, "\n")+"\n"))
}

// 帮助全文。
func TestGoldenHelp(t *testing.T) {
	e := newEnv(t, nil)
	out, code := e.run("help")
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	testkit.GoldenCompare(t, "help.txt", []byte(out))
}

// 固定时间、固定状态下 status 的全文。假 launchctl：两个平台都走同一份文字。
func TestGoldenStatus(t *testing.T) {
	e := newEnv(t, map[string]any{"suggest_enabled": true, "sense_enabled": true, "release_assist_enabled": true, "exclude_projects": []string{"private-one"}})
	launchctl := filepath.Join(t.TempDir(), "launchctl")
	testkit.WriteInstall(t, launchctl, "#!/bin/sh\nexit 0\n", 0o755)
	e.extra = append(e.extra, "TZ=UTC", "AGENTQ_TEST_NOW=2026-03-15T10:00:00Z", "AGENTQ_TEST_LAUNCHCTL="+launchctl)
	testkit.MkProject(t, e.root, "demo", "- [ ] 修登录\n- [ ] 改文案（进行中：agent/x 2026-03-15 09:00）\n- [ ] 卡着的（卡住：等人）\n")
	testkit.MkProject(t, e.root, "empty", "")
	testkit.MkProject(t, e.root, "private-one", "- [ ] 不该显示\n")
	started := goldenTime.Add(-2*time.Hour - 30*time.Minute)
	ended := goldenTime.Add(-time.Hour)
	st := &State{
		Jobs: []*Job{
			{ID: "0315-073000-demo", Project: "demo", ProjectDir: filepath.Join(e.root, "demo"), Title: "改文案", Item: "- [ ] 改文案", PID: 99999998, StartedAt: started, Status: state.StatusRunning, ResumeOf: "0315-050000-demo"},
			{ID: "0315-073001-夜班", Kind: config.KindNight, Project: "夜班", Title: "agentq night", PID: 99999999, StartedAt: started, Status: state.StatusRunning},
			{ID: "0315-050000-demo", Project: "demo", ProjectDir: filepath.Join(e.root, "demo"), Title: "修登录", Item: "- [ ] 修登录", StartedAt: started, EndedAt: &ended, Status: state.StatusDone, Summary: "做完了"},
			{ID: "0315-060000-demo", Project: "demo", ProjectDir: filepath.Join(e.root, "demo"), Title: "失败的", Item: "- [ ] 失败的", StartedAt: started, EndedAt: &ended, Status: state.StatusFailed, Summary: "出错了"},
		},
		Usage: &backend.UsageInfo{CheckedAt: goldenTime},
	}
	if err := state.Save(filepath.Join(e.home, ".agentq", "state.json"), st); err != nil {
		t.Fatal(err)
	}
	out, code := e.run("status")
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	testkit.GoldenCompare(t, "status.txt", []byte(strings.NewReplacer(e.home, "$HOME", e.root, "$ROOT").Replace(out)))
}
