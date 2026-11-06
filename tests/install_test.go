package tests

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/config"
)

// fakeLaunchctl 记录调用；loaded 记各服务（按 label）当前是否装着。绝不执行真实的 launchctl。
type fakeLaunchctl struct {
	calls  []string
	loaded map[string]bool
}

func (f *fakeLaunchctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.loaded == nil {
		f.loaded = map[string]bool{}
	}
	label := filepath.Base(args[len(args)-1]) // print/bootout 的参数是 gui/<uid>/<label>，bootstrap 的是 plist 路径
	switch args[0] {
	case "print":
		if !f.loaded[label] {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
	case "bootstrap":
		f.loaded[strings.TrimSuffix(label, ".plist")] = true
	case "bootout":
		f.loaded[label] = false
	}
	return nil, nil
}

func newInstallApp(t *testing.T) (*App, *fakeLaunchctl, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLaunchctl{}
	a.Service.Launchctl = f.run
	return a, f, &out
}

func TestPlistContent(t *testing.T) {
	a, _, _ := newInstallApp(t)
	base := a.Home
	a.Home = filepath.Join(base, "a&b") // 路径里的特殊字符要转义
	h := base + "/a&amp;b"
	p := a.Service.PlistContent()
	for _, want := range []string{
		"<string>io.agentq.agentq</string>",
		"<string>/bin/sh</string>\n\t\t<string>" + h + "/.agentq/tick-guard.sh</string>\n\t</array>",
		"<key>StartInterval</key>\n\t<integer>300</integer>",
		"<key>RunAtLoad</key>\n\t<true/>",
		h + "/.local/bin:" + h + "/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
		"<key>StandardOutPath</key>\n\t<string>" + h + "/.agentq/launchd.log</string>",
		"<key>StandardErrorPath</key>\n\t<string>" + h + "/.agentq/launchd.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺：%s\n---\n%s", want, p)
		}
	}
	// 用系统的 plutil 校验格式
	f := filepath.Join(t.TempDir(), "x.plist")
	os.WriteFile(f, []byte(p), 0o644)
	if out, err := plutilLint(f); err != nil {
		t.Errorf("plutil -lint 不通过：%v %s", err, out)
	}
}

func TestInstallUninstall(t *testing.T) {
	a, f, out := newInstallApp(t)
	if err := a.Service.CmdInstall(); err == nil || !strings.Contains(err.Error(), "scripts/update.sh") {
		t.Fatalf("没有二进制时应提示先构建：%v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("没有二进制时不应调用 launchctl：%v", f.calls)
	}
	os.MkdirAll(filepath.Dir(a.BinPath()), 0o755)
	os.WriteFile(a.BinPath(), []byte("#!/bin/sh\n"), 0o755)
	a.Cfg.Paused = true

	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.Service.PlistPath())
	if err != nil || string(b) != a.Service.PlistContent() {
		t.Fatalf("plist 没写对：%v", err)
	}
	if b, err := os.ReadFile(a.Service.GuardPath()); err != nil || string(b) != a.Service.GuardContent() {
		t.Fatalf("保护脚本没写对：%v", err)
	}
	labels := []string{"io.agentq.agentq", "io.agentq.agentq.night", "io.agentq.agentq.review", "io.agentq.agentq.listen"}
	for _, s := range a.Service.Services() {
		if b, err := os.ReadFile(a.Service.PlistPathFor(s.Label)); err != nil || string(b) != s.Plist {
			t.Errorf("%s 的 plist 没写对：%v", s.Label, err)
		}
	}
	calls := func(boot bool, ops ...string) []string { // 每个服务依次 print、[bootout]、[bootstrap]
		var want []string
		for _, l := range labels {
			target := a.Service.LaunchdDomain() + "/" + l
			want = append(want, "print "+target)
			for _, op := range ops {
				if op == "bootout" {
					want = append(want, "bootout "+target)
				}
			}
			if boot {
				want = append(want, "bootstrap "+a.Service.LaunchdDomain()+" "+a.Service.PlistPathFor(l))
			}
		}
		return want
	}
	if want := calls(true); strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("首次安装调用：\n got %v\nwant %v", f.calls, want)
	}
	for _, s := range []string{"已安装", "agentq resume", "io.agentq.agentq.night", "夜班 01:00 关", "复盘 06:30 关"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("输出缺 %q：%s", s, out.String())
		}
	}

	// 重复安装：每个服务先 bootout 再 bootstrap
	f.calls = nil
	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	if want := calls(true, "bootout"); strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("重复安装调用：\n got %v\nwant %v", f.calls, want)
	}

	// 卸载：每个服务 bootout、删 plist，保留 ~/.agentq
	f.calls = nil
	if err := a.Service.CmdUninstall(); err != nil {
		t.Fatal(err)
	}
	for _, l := range labels {
		if _, err := os.Stat(a.Service.PlistPathFor(l)); !os.IsNotExist(err) {
			t.Errorf("%s 的 plist 应已删除", l)
		}
	}
	if _, err := os.Stat(a.ConfigPath()); err != nil {
		t.Error("配置应保留")
	}
	if want := calls(false, "bootout"); strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("卸载调用：\n got %v\nwant %v", f.calls, want)
	}
	// 没装时卸载也不报错
	f.calls = nil
	if err := a.Service.CmdUninstall(); err != nil {
		t.Errorf("没装时卸载不应报错：%v", err)
	}
}

// 夜班、复盘的 plist：每天 01:00 / 06:30 执行 agentq night|review --scheduled，没有 RunAtLoad，PATH、日志同 tick。
func TestRoutinePlistContent(t *testing.T) {
	a, _, _ := newInstallApp(t)
	for kind, at := range map[string][2]string{config.KindNight: {"1", "0"}, config.KindReview: {"6", "30"}} {
		p := a.Service.RoutinePlistContent(config.Routines[kind])
		for _, want := range []string{
			"<string>io.agentq.agentq." + kind + "</string>",
			"<array>\n\t\t<string>" + a.Home + "/.local/bin/agentq</string>\n\t\t<string>" + kind + "</string>\n\t\t<string>--scheduled</string>\n\t</array>",
			"<key>StartCalendarInterval</key>\n\t<dict>\n\t\t<key>Hour</key>\n\t\t<integer>" + at[0] + "</integer>\n\t\t<key>Minute</key>\n\t\t<integer>" + at[1] + "</integer>",
			"<string>" + a.Service.LaunchdPATH() + "</string>",
			"<key>StandardOutPath</key>\n\t<string>" + a.Home + "/.agentq/launchd.log</string>",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s 的 plist 缺：%s\n---\n%s", kind, want, p)
			}
		}
		if strings.Contains(p, "RunAtLoad") || strings.Contains(p, "StartInterval<") {
			t.Errorf("%s 的 plist 不应在装上时就执行：\n%s", kind, p)
		}
		f := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(f, []byte(p), 0o644)
		if out, err := plutilLint(f); err != nil {
			t.Errorf("%s 的 plist plutil -lint 不通过：%v %s", kind, err, out)
		}
	}
}

// Telegram 监听的 plist：常驻执行 agentq listen，装上就跑，挂了 launchd 拉起；PATH、日志同 tick。
func TestListenPlistContent(t *testing.T) {
	a, _, _ := newInstallApp(t)
	p := a.Service.ListenPlistContent()
	for _, want := range []string{
		"<string>io.agentq.agentq.listen</string>",
		"<array>\n\t\t<string>" + a.Home + "/.local/bin/agentq</string>\n\t\t<string>listen</string>\n\t</array>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<string>" + a.Service.LaunchdPATH() + "</string>",
		"<key>StandardOutPath</key>\n\t<string>" + a.Home + "/.agentq/launchd.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("listen 的 plist 缺：%s\n---\n%s", want, p)
		}
	}
	f := filepath.Join(t.TempDir(), "x.plist")
	os.WriteFile(f, []byte(p), 0o644)
	if out, err := plutilLint(f); err != nil {
		t.Errorf("listen 的 plist plutil -lint 不通过：%v %s", err, out)
	}
}

// 没替换 Launchctl 且 HOME 不是真实家目录时，拒绝调真实 launchctl。
func TestInstallRefusesRealLaunchctlWithFakeHome(t *testing.T) {
	a, _, _ := newInstallApp(t)
	a.Service.Launchctl = nil
	os.MkdirAll(filepath.Dir(a.BinPath()), 0o755)
	os.WriteFile(a.BinPath(), []byte("#!/bin/sh\n"), 0o755)
	for name, f := range map[string]func() error{"install": a.Service.CmdInstall, "uninstall": a.Service.CmdUninstall} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "不调用真实 "+a.Service.Svc().Words().Tool) {
			t.Errorf("%s 应拒绝：%v", name, err)
		}
	}
	if _, err := os.Stat(a.Service.PlistPath()); !os.IsNotExist(err) {
		t.Error("拒绝时不应写 plist")
	}
}

func TestInstallBootstrapFailure(t *testing.T) {
	a, _, _ := newInstallApp(t)
	os.MkdirAll(filepath.Dir(a.BinPath()), 0o755)
	os.WriteFile(a.BinPath(), []byte("#!/bin/sh\n"), 0o755)
	a.Service.Launchctl = func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			return nil, errors.New("not loaded")
		}
		return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
	}
	err := a.Service.CmdInstall()
	if err == nil || !strings.Contains(err.Error(), "Input/output error") || !strings.Contains(err.Error(), "再执行一次 agentq install") {
		t.Errorf("bootstrap 失败应带出 launchctl 的输出：%v", err)
	}
}

func TestServiceLabelFromLocalConfig(t *testing.T) {
	a, f, _ := newInstallApp(t)
	a.Cfg.LaunchdLabel = "org.example.task-queue"
	if err := a.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Service.LaunchdPrefix() != "org.example." {
		t.Fatal(a.Service.LaunchdPrefix())
	}
	os.MkdirAll(filepath.Dir(a.BinPath()), 0o755)
	os.WriteFile(a.BinPath(), []byte("#!/bin/sh\n"), 0o755)
	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	for _, svc := range a.Service.Services() {
		if !strings.HasPrefix(svc.Label, a.Cfg.LaunchdLabel) || !strings.Contains(svc.Plist, "<string>"+svc.Label+"</string>") {
			t.Fatalf("configured label not preserved: %s", svc.Label)
		}
	}
	for _, call := range f.calls {
		if strings.Contains(call, config.LaunchdLabel) {
			t.Fatalf("default label used for existing service: %s", call)
		}
	}
	for _, label := range []string{"", "queue", "org.example/bad", "org.example bad", "org.example.<broken>", "org.example.&bad", "org..example", "org.example.\x00"} {
		a.Cfg.LaunchdLabel = label
		if a.Cfg.Validate() == nil {
			t.Fatalf("invalid label accepted: %q", label)
		}
	}
}
