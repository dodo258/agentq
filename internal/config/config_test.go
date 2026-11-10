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
	// 没有 per_project：用新默认 2；没有夜班、复盘的键：默认关，超时写死 6h/4h
	if cfg.MaxParallel != 3 || cfg.Timeout.Duration != 30*time.Minute || !cfg.Paused || cfg.PerProject != 2 || !cfg.TickAfterJob {
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

// 写死的键：老配置里写着也不起作用，新配置不写出来。
func TestHardwiredKeysIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"job_budget_usd": 0, "night_budget_usd": 7.5, "progress_autostop": false, "low_risk_model": "haiku", "breaker_streak": 100,
		"tick_after_job": false, "patrol_analyze": false, "router_enabled": true, "model_policy": "auto"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("老配置里写着删掉的键要照样读：%v", err)
	}
	d := Default()
	if got.JobBudgetUSD != 150 || got.NightBudgetUSD != 10 || got.LowRiskModel != "sonnet" || got.BreakerStreak != d.BreakerStreak ||
		!got.TickAfterJob || !got.PatrolEnabled {
		t.Errorf("写死的值被配置文件改了：%+v", got)
	}
	b, _ := json.Marshal(d)
	for _, key := range []string{"job_budget_usd", "breaker_streak", "tick_after_job", "low_risk_model", "stall_after", "discuss_wait", "verify_timeout"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("写死的键不该写进配置：%s", key)
		}
	}
}

// retention_days 照读：0（不删）和别的值都留着，负数报错。写死了会悄悄删掉老用户的记录。
func TestRetentionDaysConfigurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"retention_days": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays != 0 {
		t.Errorf("retention_days=0（不删）要照读：%d", got.RetentionDays)
	}
	if err := os.WriteFile(path, []byte(`{"retention_days": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("负数应报错")
	}
	if d := Default(); d.RetentionDays != 90 {
		t.Errorf("默认 90 天：%d", d.RetentionDays)
	}
}

// 按活分配：默认后端是 Claude、model 非空时用写死的分配；model 跟随 CLI 或别家后端时全空，照 model、effort。
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
	roles := func(c Config) string {
		return c.DiscussModel + c.ReviewModel + c.ReviewEffort + c.PatrolModel + c.FollowupModel + c.HeavyEffort + c.LowRiskModel + c.LowRiskEffort + c.LightReviewModel
	}
	for name, c := range map[string]Config{"新建": fresh, "读回来": reload} {
		if c.Model != "" || c.Effort != "" || roles(c) != "" {
			t.Errorf("%s：不该有模型名：%+v", name, c)
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
		if c.DiscussModel != "sonnet" || c.ReviewModel != "sonnet" || c.ReviewEffort != "high" || c.PatrolModel != "sonnet" || c.FollowupModel != "sonnet" ||
			c.HeavyEffort != "xhigh" || c.LowRiskModel != "sonnet" || c.LowRiskEffort != "medium" || c.LightReviewModel != "sonnet" {
			t.Errorf("%s：Claude 写了模型照写死的分配：%+v", name, c)
		}
	}
	for _, raw := range []string{`{"backend":"codex"}`, `{"model":"","effort":""}`} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(path); err != nil || roles(c) != "" || c.UsageStopSessionPct != 70 || c.UsageStopWeekPct != 90 {
			t.Errorf("%s：按活分配要全空：%+v %v", raw, c, err)
		}
	}
	if f := Default().Fresh(); roles(f) != "" {
		t.Errorf("新配置跟随 CLI：%+v", f)
	}
	if m, e := (Config{Model: "opus", Effort: "high"}).Role("", "medium"); m != "opus" || e != "high" {
		t.Errorf("没配专用模型照 model、effort：%s %s", m, e)
	}
	if got := (Config{Model: "claude-opus-5-5"}).PinnedModels(); len(got) != 1 {
		t.Errorf("钉了版本号的要找出来：%v", got)
	}
	if got := (Config{Model: "opus"}).PinnedModels(); len(got) != 0 {
		t.Errorf("别名不算：%v", got)
	}
	for _, bad := range []string{`{"usage_stop_session_pct":0}`, `{"usage_stop_week_pct":98}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s 应报错", bad)
		}
	}
	if ModelLabel("") == "" || ModelLabel("x") != "x" {
		t.Error(ModelLabel(""))
	}
}
