package tests

import (
	"strings"
	"testing"

	"agentq/internal/env"
	"agentq/internal/proc"
	"agentq/internal/service"
)

// macOS 不带假命令时必须还是 launchd：这是这次拆分的第一验收条件。
func TestDarwinDefaultManagerIsLaunchd(t *testing.T) {
	a := wireApp(&App{Env: &env.Env{Home: t.TempDir()}})
	m, ok := a.Service.Svc().(service.LaunchdManager)
	if !ok {
		t.Fatalf("macOS 默认应是 launchd：%T", a.Service.Svc())
	}
	if w := m.Words(); w.Name != "launchd" || w.Tool != "launchctl" || w.List != "launchctl list" {
		t.Errorf("%+v", w)
	}
	if !strings.Contains(a.Service.GuardContent(), `prev_time=$(stat -f '%Sm' -t '%Y-%m-%d %H:%M' "$bin.prev" 2>/dev/null)`) {
		t.Errorf("保护脚本应保持原样：\n%s", a.Service.GuardContent())
	}
	if proc.PsLocale != "en_US.UTF-8" {
		t.Errorf("ps 的区域：%s", proc.PsLocale)
	}
}
