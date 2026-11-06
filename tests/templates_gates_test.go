package tests

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/gate"
)

// 门脚本示例：随安装分发（编进二进制、锁定声明里有），语法对，写明约定。真实项目上没跑过，这里只查这些。
func TestGateTemplates(t *testing.T) {
	assets := builtinAssets()
	var lock struct {
		Bundled struct {
			Templates []string `json:"templates"`
		} `json:"bundledAssets"`
	}
	raw, err := os.ReadFile(filepath.Join("dependencies", "claude.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	wants := map[string][]string{
		"ui-playwright.sh": {gate.ArtifactsEnv, "125", "playwright"},
		"ui-simctl.sh":     {gate.ArtifactsEnv, "125", "simctl"},
		"smoke.sh":         {"125", "退出码 0"},
	}
	for name, words := range wants {
		rel := "templates/gates/" + name
		fi, err := os.Stat(filepath.FromSlash(rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s 要有可执行权限", rel)
		}
		if out, err := exec.Command("sh", "-n", filepath.FromSlash(rel)).CombinedOutput(); err != nil {
			t.Errorf("%s 语法不对：%v %s", rel, err, out)
		}
		body, err := fs.ReadFile(assets.Install, rel)
		if err != nil {
			t.Fatalf("%s 没编进二进制：%v", rel, err)
		}
		for _, w := range words {
			if !strings.Contains(string(body), w) {
				t.Errorf("%s 缺 %q", rel, w)
			}
		}
		if !slices.Contains(lock.Bundled.Templates, rel) {
			t.Errorf("锁定声明缺 %s：%v", rel, lock.Bundled.Templates)
		}
	}
}
