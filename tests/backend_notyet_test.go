package tests

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/backend"
	"agentq/internal/config"
	"agentq/internal/setup"
)

// opencode 以后再加：安装时 --backend opencode 如实拒绝、什么都不写；配置里写了它，doctor 说暂不支持，不当成写错，也不换成别的后端。
func TestNotYetBackendOpencode(t *testing.T) {
	i, o := installFixture(t)
	o.Backend = "opencode"
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "opencode 暂不支持，以后再加") {
		t.Fatalf("要如实拒绝：%v", e)
	}
	if exists(filepath.Join(i.Home, ".agentq", "config.json")) || exists(filepath.Join(i.Home, ".local", "share", "agentq", "receipt.json")) {
		t.Fatal("拒绝了就什么都不写")
	}
	o.Backend = ""
	mustInstall(t, i, o)

	var out bytes.Buffer
	i.Out = &out
	save := func(def string, projects map[string]string) {
		t.Helper()
		cfg, _, err := setup.ReadInstallConfig(i.Home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Backend, cfg.ProjectBackends = def, projects
		if err := config.Save(filepath.Join(i.Home, ".agentq", "config.json"), cfg); err != nil {
			t.Fatal(err)
		}
		out.Reset()
	}
	save("opencode", nil)
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "opencode 暂不支持，以后再加") || !strings.Contains(out.String(), "后端：opencode（暂不支持，以后再加，不接活）") {
		t.Errorf("默认后端写了 opencode 要报：%v\n%s", e, out.String())
	}
	save("", map[string]string{"demo": "opencode"})
	if e := i.Doctor(); e == nil || !strings.Contains(out.String(), "项目 demo 的后端 opencode 暂不支持，以后再加") {
		t.Errorf("项目的后端写了 opencode 要报：%v\n%s", e, out.String())
	}

	if backend.Known("opencode") {
		t.Fatal("opencode 不该注册")
	}
	if _, _, err := backend.Get("opencode", backend.Options{}).Build(backend.Request{}); err == nil || !strings.Contains(err.Error(), "opencode 暂不支持，以后再加") {
		t.Errorf("启动要拒绝：%v", err)
	}
	if got := backend.Unsupported("nosuch"); !strings.Contains(got, "未知后端") {
		t.Errorf("写错的还是未知：%s", got)
	}
}
