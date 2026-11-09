package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/config"
	"agentq/internal/testkit"
)

// 走真的启动：新配置的 model、effort 是空的，argv 不带；auto 的轻活带后端给的模型，重写类不带。已有配置的快照由 TestGoldenArgv（argv.txt）锁着。
func TestStartArgvFollowsCLI(t *testing.T) {
	empty := map[string]any{"model": "", "effort": ""}
	t.Run("空配置", func(t *testing.T) {
		got, _ := goldenStart(t, empty, &Job{Project: "demo", Item: "修登录", Title: "修登录"}, "", nil)
		for _, flag := range []string{"--model", "--effort"} {
			if strings.Contains(got, flag) {
				t.Fatalf("不该有 %s：\n%s", flag, got)
			}
		}
	})
	t.Run("auto 轻活", func(t *testing.T) {
		cfg := map[string]any{"model": "", "effort": "", "model_policy": "auto"}
		got, _ := goldenStart(t, cfg, &Job{Project: "demo", Item: "补测试（低风险）", Title: "补测试"}, "", nil)
		testkit.AssertContains(t, got, "--model", "sonnet")
	})
	t.Run("auto 重写不降级", func(t *testing.T) {
		cfg := map[string]any{"model": "opus", "effort": "", "model_policy": "auto", "low_risk_model": "cheap"}
		got, _ := goldenStart(t, cfg, &Job{Project: "demo", Item: "重写调度（低风险）", Title: "重写调度"}, "", nil)
		testkit.AssertContains(t, got, "opus")
		if strings.Contains(got, "cheap") {
			t.Fatalf("重写类不降级：\n%s", got)
		}
	})
	t.Run("fixed 写了的照用", func(t *testing.T) {
		cfg := map[string]any{"model": "my-model", "effort": "low"}
		got, _ := goldenStart(t, cfg, &Job{Project: "demo", Item: "修登录", Title: "修登录"}, "", nil)
		testkit.AssertContains(t, got, "my-model", "low")
	})
}

// 配置文件不存在时写出来的就是跟随 CLI，不写 Claude 的模型名。
func TestFreshConfigHasNoModelNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	text := testkit.ReadFile(t, path)
	testkit.AssertContains(t, text, `"model": ""`, `"effort": ""`, `"router_model": ""`, `"suggest_model": ""`, `"sense_model": ""`, `"light_review_model": ""`)
	for _, name := range []string{"opus", "sonnet"} {
		if strings.Contains(text, name) {
			t.Fatalf("写死了 %s：\n%s", name, text)
		}
	}
}
