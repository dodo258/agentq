package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agentq/internal/fsx"
)

func TestBudgetConfig(t *testing.T) {
	cfg := Default()
	if cfg.NightBudgetUSD != 10 || cfg.ReviewBudgetUSD != 15 || cfg.PatrolBudgetUSD != 2 || cfg.JobBudgetUSD != 150 || !cfg.ProgressAutostop || cfg.LowRiskModel != "" {
		t.Errorf("默认值：%+v", cfg)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	// 老配置没有这些键：沿用默认值
	os.WriteFile(path, []byte(`{"max_parallel": 3}`), 0o644)
	got, err := Load(path)
	if err != nil || got.JobBudgetUSD != 150 || got.NightBudgetUSD != 10 || !got.ProgressAutostop || got.LowRiskModel != "" {
		t.Errorf("%v %+v", err, got)
	}
	os.WriteFile(path, []byte(`{"job_budget_usd": 0, "night_budget_usd": 7.5, "progress_autostop": false, "low_risk_model": "haiku"}`), 0o644)
	got, err = Load(path)
	if err != nil || got.JobBudgetUSD != 0 || got.NightBudgetUSD != 7.5 || got.ProgressAutostop || got.LowRiskModel != "haiku" {
		t.Errorf("%v %+v", err, got)
	}
	os.WriteFile(path, []byte(`{"job_budget_usd": -1}`), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "不能是负数") {
		t.Errorf("负数应报错：%v", err)
	}
	// 新配置写全这些键（setup 就是把这份写出去）
	b, _ := json.Marshal(Default())
	for _, key := range []string{`"night_budget_usd":10`, `"review_budget_usd":15`, `"patrol_budget_usd":2`, `"job_budget_usd":150`, `"progress_autostop":true`, `"low_risk_model":""`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("缺 %s：%s", key, b)
		}
	}
}

// 配置校验：空名字、带路径的拒绝；不存在的项目不算错（可能以后才有）。
func TestExcludeConfigValidate(t *testing.T) {
	for _, bad := range []string{"", " ", "a/b", "..", "."} {
		c := Default()
		c.ExcludeProjects = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "exclude_projects") {
			t.Errorf("%q 应拒绝：%v", bad, err)
		}
	}
	c := Default()
	c.ExcludeProjects = []string{"not-there-yet"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// 默认空，新配置不多写这个键
	home := t.TempDir()
	p := filepath.Join(home, "c.json")
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || strings.Contains(string(b), "exclude_projects") {
		t.Fatalf("%v %s", err, b)
	}
}

func TestSenseConfig(t *testing.T) {
	d := Default()
	if d.SenseEnabled || d.SenseModel != "sonnet" || d.SenseBudgetUSD != 1 {
		t.Fatalf("默认值：%+v", d)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	for name, cfg := range map[string]string{
		"预算 0":   `{"sense_budget_usd":0}`,
		"预算负数":   `{"sense_budget_usd":-1}`,
		"预算太大":   `{"sense_budget_usd":5000}`,
		"模型像个选项": `{"sense_enabled":true,"sense_model":"--tools"}`,
	} {
		os.WriteFile(path, []byte(cfg), 0o644)
		if _, err := Load(path); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
	os.WriteFile(path, []byte(`{"launchd_label":"io.agentq.agentq"}`), 0o644)
	if c, err := Load(path); err != nil || c.SenseEnabled {
		t.Errorf("老配置兼容：%+v %v", c, err)
	}
}

func TestRegressSetJSONKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`{"paused":false,"a":1}`, `{"paused":true,"a":1}`},
		{`{"a":{"paused":false},"paused": false }`, `{"a":{"paused":false},"paused": true }`},
		{`{"paused":false,"paused":false}`, `{"paused":true,"paused":true}`},
		{`{}`, "{\n  \"paused\": true}"},
		{`{"a":1}`, "{\n  \"paused\": true,\"a\":1}"},
	} {
		got, err := SetJSONKey([]byte(tc.in), "paused", "true")
		if err != nil || string(got) != tc.want {
			t.Errorf("%s → %s（%v），想要 %s", tc.in, got, err, tc.want)
		}
		if !json.Valid(got) {
			t.Errorf("%s 改完不是 JSON：%s", tc.in, got)
		}
	}
	for _, bad := range []string{"", "null", "[]", "{坏了", `{"a":1}x`, "3"} {
		if _, err := SetJSONKey([]byte(bad), "paused", "true"); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// 读完之后文件被手改了：不能换上去（否则手改的内容就没了），重读再改。
func TestRegressPauseDetectsHandEdit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"paused":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 手改：编辑器常见的两种写法，换一个新文件、原地写
	if err := fsx.WriteFileAtomic(path, []byte(`{"paused":false,"hand":1}`)); err != nil {
		t.Fatal(err)
	}
	if same, err := replaceIfUnchanged(path, before, []byte(`{"paused":true}`)); err != nil || same {
		t.Fatalf("文件换过了，不该换上去：%v %v", same, err)
	}
	before, _ = os.Stat(path)
	if err := os.WriteFile(path, []byte(`{"paused":false,"hand":12}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if same, err := replaceIfUnchanged(path, before, []byte(`{"paused":true}`)); err != nil || same {
		t.Fatalf("文件原地改过了，不该换上去：%v %v", same, err)
	}
	if err := SetPaused(path, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"paused":true,"hand":12}` {
		t.Fatalf("要在手改之后的内容上改：%s", b)
	}
	// 文件不是合法 JSON：不写，报错
	if err := os.WriteFile(path, []byte(`{坏了`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetPaused(path, false); err == nil {
		t.Fatal("坏文件应报错")
	}
	if b, _ := os.ReadFile(path); string(b) != `{坏了` {
		t.Fatalf("坏文件不该动：%s", b)
	}
}

func TestSuggestConfig(t *testing.T) {
	c := Default()
	if c.SuggestEnabled || c.SuggestModel != "sonnet" || c.SuggestBudgetUSD != 1 {
		t.Errorf("默认值：%+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SuggestBudgetUSD = 0
	if c.Validate() == nil {
		t.Error("预算 0 应拒绝")
	}
	c.SuggestBudgetUSD = 1
	c.SuggestEnabled, c.SuggestModel = true, ""
	if err := c.Validate(); err != nil {
		t.Errorf("模型留空是跟随 CLI：%v", err)
	}
	c.SuggestModel = "--help"
	if c.Validate() == nil {
		t.Error("模型不能以 - 开头")
	}
	// 旧配置文件没有这些键：沿用默认值，关着
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"max_parallel":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.SuggestEnabled || got.SuggestModel != "sonnet" || got.SuggestBudgetUSD != 1 {
		t.Errorf("旧配置：%+v %v", got, err)
	}
}

// 模型和强度：新配置留空并写进文件，读回来还是空；老配置缺键和写了的都照旧。
func TestModelDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	fresh, err := Load(path) // 文件不存在时写一份
	if err != nil {
		t.Fatal(err)
	}
	reload, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Config{"新建": fresh, "读回来": reload} {
		if c.Model != "" || c.Effort != "" || c.RouterModel != "" || c.SuggestModel != "" || c.SenseModel != "" {
			t.Errorf("%s：不该有模型名：%+v", name, c)
		}
		if c.ModelPolicy != "" {
			t.Errorf("%s：默认 fixed，不写这个键：%q", name, c.ModelPolicy)
		}
	}
	for name, raw := range map[string][2]string{
		"老配置缺键": {`{}`, "opus"},
		"写了就照用": {`{"model":"sonnet","effort":"low"}`, "sonnet"},
	} {
		if err := os.WriteFile(path, []byte(raw[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil || c.Model != raw[1] || c.Effort == "" {
			t.Errorf("%s：%+v %v", name, c, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"model_policy":"auto"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.ModelPolicy != ModelPolicyAuto {
		t.Errorf("%+v %v", c, err)
	}
	if err := os.WriteFile(path, []byte(`{"model_policy":"cheap"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("不认识的 model_policy 应报错")
	}
	for name, raw := range map[string]string{
		"Router 留空": `{"router_enabled":true,"router_model":""}`,
		"找活留空":      `{"suggest_enabled":true,"suggest_model":""}`,
		"感知留空":      `{"sense_enabled":true,"sense_model":""}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("%s 是跟随 CLI，不算错：%v", name, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"router_enabled":true,"router_model":"--tools"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("router_model 不能像个选项")
	}
	if ModelLabel("") == "" || ModelLabel("x") != "x" {
		t.Error(ModelLabel(""))
	}
}

// 登记只加这一个键：别的字节不动，重复登记不变，同时登记不丢。
func TestAddExtraProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := "{\n  \"unknown\": [1,  2],\n\n  \"paused\": true\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddExtraProject(path, "/a/one"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"unknown": [1,  2],`) || !strings.Contains(string(b), `"extra_projects": ["/a/one"]`) {
		t.Fatalf("%s", b)
	}
	if err := AddExtraProject(path, "/a/one"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, d := range []string{"/a/two", "/a/three", "/a/four"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AddExtraProject(path, d); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExtraProjects) != 4 || !slices.Contains(cfg.ExtraProjects, "/a/one") || !slices.Contains(cfg.ExtraProjects, "/a/four") || !cfg.Paused {
		t.Fatalf("%v", cfg.ExtraProjects)
	}
	// 文件没了、不是 JSON：报错，不写
	if err := AddExtraProject(filepath.Join(t.TempDir(), "没有.json"), "/x"); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddExtraProject(path, "/x"); err == nil {
		t.Fatal("坏文件应报错")
	}
	if b, _ := os.ReadFile(path); string(b) != "{" {
		t.Fatalf("坏文件不该动：%s", b)
	}
}

func TestExtraProjectDir(t *testing.T) {
	c := Config{ExtraProjects: []string{"/a/one", "/b/one", "/c/two"}}
	if got := c.ExtraProjectDir("one"); got != "/a/one" {
		t.Fatal(got)
	}
	if got := c.ExtraProjectDir("nope"); got != "" {
		t.Fatal(got)
	}
}

func TestConfigDefaultsAndPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout.Duration != 4*time.Hour || cfg.MaxParallel != 2 || cfg.Model != "" {
		t.Errorf("默认值不对：%+v", cfg)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"timeout": "4h"`) || !strings.Contains(string(b), `"projects_root": "~/projects"`) {
		t.Errorf("应写出默认配置，得到：\n%s", b)
	}
	os.WriteFile(path, []byte(`{"max_parallel": 3, "timeout": "30m", "paused": true}`), 0o644)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// 旧配置文件没有 tick_after_job：沿用默认的开；没有夜班、复盘的键：默认关，超时 6h/4h
	if cfg.MaxParallel != 3 || cfg.Timeout.Duration != 30*time.Minute || !cfg.Paused || cfg.PerProject != 1 || !cfg.TickAfterJob {
		t.Errorf("部分配置应覆盖对应键、其余用默认：%+v", cfg)
	}
	if cfg.NightEnabled || cfg.ReviewEnabled || cfg.NightTimeout.Duration != 6*time.Hour || cfg.ReviewTimeout.Duration != 4*time.Hour {
		t.Errorf("夜班、复盘默认应关闭、超时 6h/4h：%+v", cfg)
	}
	os.WriteFile(path, []byte(`{"max_parallel": 0}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("max_parallel=0 应报错")
	}
	os.WriteFile(path, []byte(`{"timeout": 5}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("时长写成数字应报错")
	}
}
