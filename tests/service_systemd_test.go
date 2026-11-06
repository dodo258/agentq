package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentq/internal/config"
	"agentq/internal/fsx"
	"agentq/internal/notify"
	"agentq/internal/patrol"
	"agentq/internal/proc"
	"agentq/internal/service"
	"agentq/internal/testkit"
)

// fakeSystemd 是一个很小的 systemd --user：按 unit 目录里的文件判断 daemon-reload 后能看到哪些 unit，记 enable、active 和调用。
// 绝不执行真实的 systemctl。fail 按第一个参数注入失败；unit 表填上次运行的结果，模拟 show 的输出。
type fakeSystemd struct {
	dir      string
	calls    []string
	known    map[string]bool // daemon-reload 时看到的 unit
	enabled  map[string]bool
	active   map[string]bool
	mainPID  map[string]string
	status   map[string]string
	result   map[string]string
	fail     map[string]error
	failUnit map[string]bool // 对这些 unit 的 enable 失败
}

func newFakeSystemd(home string) *fakeSystemd {
	return &fakeSystemd{dir: filepath.Join(home, ".config", "systemd", "user"), known: map[string]bool{}, enabled: map[string]bool{},
		active: map[string]bool{}, mainPID: map[string]string{}, status: map[string]string{}, result: map[string]string{}, fail: map[string]error{}, failUnit: map[string]bool{}}
}

func (f *fakeSystemd) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) == 0 {
		return nil, errors.New("no args")
	}
	if e := f.fail[args[0]]; e != nil {
		return []byte("Failed to connect to bus: No medium found\n"), e
	}
	switch args[0] {
	case "show-environment", "reset-failed":
	case "daemon-reload":
		f.known = map[string]bool{}
		entries, _ := os.ReadDir(f.dir)
		for _, e := range entries {
			f.known[e.Name()] = true
		}
	case "enable":
		unit := args[len(args)-1]
		if !f.known[unit] {
			return []byte("Failed to enable unit: Unit file " + unit + " does not exist.\n"), errors.New("exit status 1")
		}
		if f.failUnit[unit] {
			delete(f.failUnit, unit) // 只失败一次，事务回滚时能装回去
			return []byte("injected enable failure\n"), errors.New("exit status 1")
		}
		f.enabled[unit] = true
		f.active[unit] = true
	case "disable":
		unit := args[len(args)-1]
		delete(f.enabled, unit)
		delete(f.active, unit)
	case "stop":
		delete(f.active, args[len(args)-1])
	case "show":
		var b strings.Builder
		for _, unit := range args[1:] {
			if strings.HasPrefix(unit, "--") {
				continue
			}
			load, file := "not-found", ""
			if f.known[unit] {
				load, file = "loaded", "disabled"
			}
			if f.enabled[unit] {
				file = "enabled"
			}
			act := "inactive"
			if f.active[unit] {
				act = "active"
			}
			pid, status, result := f.mainPID[unit], f.status[unit], f.result[unit]
			if pid == "" {
				pid = "0"
			}
			if status == "" {
				status = "0"
			}
			if result == "" {
				result = "success"
			}
			fmt.Fprintf(&b, "Id=%s\nLoadState=%s\nActiveState=%s\nUnitFileState=%s\nMainPID=%s\nExecMainStatus=%s\nResult=%s\n\n", unit, load, act, file, pid, status, result)
		}
		return []byte(b.String()), nil
	default:
		return nil, fmt.Errorf("fakeSystemd 没想到会收到 %v", args)
	}
	return nil, nil
}

func (f *fakeSystemd) verbs() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "show ") {
			out = append(out, c)
		}
	}
	return out
}

func newSystemdApp(t *testing.T) (*App, *fakeSystemd, *bytes.Buffer) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "it's a 100% home")
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeSystemd(home)
	a.Service.Systemctl = f.run
	a.Service.Loginctl = func(args ...string) ([]byte, error) { return []byte("no\n"), nil }
	a.Service.Launchctl = func(args ...string) ([]byte, error) {
		t.Fatalf("systemd 下碰了 launchctl：%v", args)
		return nil, nil
	}
	return a, f, &out
}

func unitNamed(t *testing.T, s service.ServiceSpec, suffix string) string {
	t.Helper()
	for _, file := range s.Files {
		if strings.HasSuffix(file.Path, suffix) {
			return file.Content
		}
	}
	t.Fatalf("%s 没有 %s 文件：%v", s.Label, suffix, s.Files)
	return ""
}

func TestSystemdUnits(t *testing.T) {
	a, _, _ := newSystemdApp(t)
	m := a.Service.Svc()
	specs := m.Specs()
	if len(specs) != 4 || specs[0].Label != a.Service.ServiceLabel() || specs[1].Label != a.Service.RoutineLabel(config.KindNight) || specs[2].Label != a.Service.RoutineLabel(config.KindReview) || specs[3].Label != a.Service.ListenLabel() {
		t.Fatalf("服务顺序不对：%+v", specs)
	}
	dir := filepath.Join(a.Home, ".config", "systemd", "user")
	for _, s := range specs {
		for _, f := range s.Files {
			if filepath.Dir(f.Path) != dir {
				t.Errorf("%s 不在用户 unit 目录", f.Path)
			}
		}
	}
	// 家目录带空格、单引号、%：ExecStart、Environment 的转义，日志路径的 %%
	qHome := strings.ReplaceAll(a.Home, "%", "%%")
	tick := unitNamed(t, specs[0], ".service")
	for _, want := range []string{
		"Type=oneshot\n",
		"StartLimitIntervalSec=0\n", // CI 真实验收撞过 start-limit-hit
		// 不能杀同组：任务是 tick 拉起来的后台进程，默认的 control-group 会在 tick 退出时一起杀掉
		"KillMode=process\n",
		`ExecStart="/bin/sh" "` + qHome + `/.agentq/tick-guard.sh"`,
		`Environment="HOME=` + qHome + `"`,
		`Environment="PATH=` + strings.ReplaceAll(a.Service.LaunchdPATH(), "%", "%%") + `"`,
		"StandardOutput=append:" + qHome + "/.agentq/launchd.log\n",
		"StandardError=append:" + qHome + "/.agentq/launchd.log\n",
	} {
		if !strings.Contains(tick, want) {
			t.Errorf("tick.service 少了 %q：\n%s", want, tick)
		}
	}
	if strings.Contains(tick, "[Install]") {
		t.Errorf("tick.service 由 timer 叫醒，不该自己 enable：\n%s", tick)
	}
	timer := unitNamed(t, specs[0], ".timer")
	for _, want := range []string{"OnCalendar=*:0/5\n", "OnActiveSec=0\n", "WantedBy=timers.target\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("tick.timer 少了 %q：\n%s", want, timer)
		}
	}
	night := unitNamed(t, specs[1], ".timer")
	if !strings.Contains(night, "OnCalendar=*-*-* 01:00:00\n") || strings.Contains(night, "OnActiveSec") || strings.Contains(night, "Persistent") {
		t.Errorf("夜班定点、装上时不跑、错过由 tick 补：\n%s", night)
	}
	nightSvc := unitNamed(t, specs[1], ".service")
	if !strings.Contains(nightSvc, `"night" "--scheduled"`) || !strings.Contains(nightSvc, "KillMode=process\n") {
		t.Errorf("夜班 service：\n%s", nightSvc)
	}
	if !strings.Contains(unitNamed(t, specs[2], ".timer"), "OnCalendar=*-*-* 06:30:00\n") {
		t.Errorf("复盘时间不对")
	}
	if len(specs[3].Files) != 1 {
		t.Errorf("listen 是常驻服务，只有一个 service：%+v", specs[3].Files)
	}
	listen := unitNamed(t, specs[3], ".service")
	for _, want := range []string{"Restart=always\n", "RestartSec=10\n", "KillMode=process\n", "WantedBy=default.target\n", "StartLimitIntervalSec=0\n", `"listen"`} {
		if !strings.Contains(listen, want) {
			t.Errorf("listen.service 少了 %q：\n%s", want, listen)
		}
	}
	backup := m.AuxSpec("backup", 86400)
	if bt := unitNamed(t, backup, ".timer"); !strings.Contains(bt, "OnActiveSec=86400s\n") || !strings.Contains(bt, "OnUnitActiveSec=86400s\n") {
		t.Errorf("backup timer：\n%s", bt)
	}
	if bs := unitNamed(t, backup, ".service"); !strings.Contains(bs, `"maintenance" "backup"`) || !strings.Contains(bs, "/.agentq/backup.log") {
		t.Errorf("backup service：\n%s", bs)
	}
	if ws := unitNamed(t, m.AuxSpec("watchdog", 1800), ".service"); !strings.Contains(ws, `ExecStart="/bin/sh" "`) || !strings.Contains(ws, "support/scripts/watchdog.sh") {
		t.Errorf("watchdog service：\n%s", ws)
	}
}

func TestSystemdInstallUninstall(t *testing.T) {
	a, f, out := newSystemdApp(t)
	if err := a.Service.CmdInstall(); err == nil || !strings.Contains(err.Error(), "没找到可执行的") {
		t.Fatalf("没有二进制时应报错：%v", err)
	}
	if len(f.calls) != 1 || f.calls[0] != "show-environment" {
		t.Fatalf("没有二进制时不该动服务：%v", f.calls)
	}
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	f.calls = nil
	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	specs := a.Service.Svc().Specs()
	for _, s := range specs {
		for _, file := range s.Files {
			b, err := os.ReadFile(file.Path)
			if err != nil || string(b) != file.Content {
				t.Errorf("%s 没写好：%v", file.Path, err)
			}
		}
	}
	if b, err := os.ReadFile(a.Service.GuardPath()); err != nil || string(b) != a.Service.GuardContent() {
		t.Errorf("保护脚本没写好：%v", err)
	}
	want := []string{
		"show-environment", "daemon-reload",
		"enable --now " + a.Service.ServiceLabel() + ".timer",
		"enable --now " + a.Service.RoutineLabel(config.KindNight) + ".timer",
		"enable --now " + a.Service.RoutineLabel(config.KindReview) + ".timer",
		"enable --now " + a.Service.ListenLabel() + ".service",
		"show-environment", // 装完的提示走 doctor，只读
	}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Fatalf("调用不对：\n got %q\nwant %q", got, want)
	}
	for _, text := range []string{"systemd --user", "loginctl enable-linger", "每 5 分钟", a.Service.ListenLabel()} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("输出少了 %q：\n%s", text, out)
		}
	}
	if strings.Contains(strings.ReplaceAll(out.String(), "launchd.log", ""), "launchd") {
		t.Errorf("Linux 的输出不该提 launchd（日志文件沿用老名字除外）：\n%s", out)
	}

	// 可重复执行：先把旧的卸下，再装
	f.calls = nil
	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.verbs(), "disable --now "+a.Service.ServiceLabel()+".timer") || !slices.Contains(f.verbs(), "stop "+a.Service.ServiceLabel()+".service") || !slices.Contains(f.verbs(), "disable --now "+a.Service.ListenLabel()+".service") {
		t.Errorf("重装要先卸下：%v", f.verbs())
	}

	f.calls = nil
	out.Reset()
	if err := a.Service.CmdUninstall(); err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		for _, file := range s.Files {
			if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
				t.Errorf("%s 没删掉", file.Path)
			}
		}
		if a.Service.Svc().IsLoaded(s.Label) {
			t.Errorf("%s 还装着", s.Label)
		}
	}
	if len(f.enabled) != 0 || len(f.active) != 0 {
		t.Errorf("还有 unit 开着：%v %v", f.enabled, f.active)
	}
	if v := f.verbs(); v[len(v)-1] != "daemon-reload" {
		t.Errorf("删完 unit 文件要 daemon-reload：%v", v)
	}
	if strings.Contains(strings.ReplaceAll(out.String(), "launchd.log", ""), "launchd") {
		t.Errorf("Linux 的输出不该提 launchd（日志文件沿用老名字除外）：\n%s", out)
	}
}

func TestSystemdUnavailableIsExplicit(t *testing.T) {
	a, f, _ := newSystemdApp(t)
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	f.fail["show-environment"] = errors.New("exit status 1")
	err := a.Service.CmdInstall()
	if err == nil {
		t.Fatal("systemctl --user 不可用时不能静默装")
	}
	for _, want := range []string{"systemctl --user", "No medium found", "WSL", "systemd=true", "XDG_RUNTIME_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里少了 %q：%v", want, err)
		}
	}
	if _, e := os.Stat(a.Service.GuardPath()); !os.IsNotExist(e) {
		t.Errorf("不可用时不该写任何文件")
	}
	if err := a.Service.CmdUninstall(); err == nil || !strings.Contains(err.Error(), "WSL") {
		t.Errorf("卸载也要明说：%v", err)
	}
}

func TestSystemdInstallEnableFails(t *testing.T) {
	a, f, _ := newSystemdApp(t)
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	f.failUnit[a.Service.RoutineLabel(config.KindNight)+".timer"] = true
	err := a.Service.CmdInstall()
	if err == nil || !strings.Contains(err.Error(), "injected enable failure") || !strings.Contains(err.Error(), "systemctl enable") {
		t.Fatalf("enable 失败要带出 systemctl 的输出：%v", err)
	}
}

func TestSystemdReloadFails(t *testing.T) {
	a, f, _ := newSystemdApp(t)
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	f.fail["daemon-reload"] = errors.New("exit status 1")
	if err := a.Service.CmdInstall(); err == nil || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("reload 失败要报错：%v", err)
	}
	if len(f.enabled) != 0 {
		t.Errorf("reload 失败不该接着 enable：%v", f.enabled)
	}
}

// 引擎全流程：装（带备份和看门狗）、doctor、升级、回滚、失败回滚、卸载、恢复。
func TestSetupSystemdLifecycle(t *testing.T) {
	i, o := installFixture(t)
	var out bytes.Buffer
	i.Out = &out
	f := newFakeSystemd(i.Home)
	i.Systemctl = f.run
	i.Loginctl = func(args ...string) ([]byte, error) { return []byte("no\n"), nil }
	i.Launchctl = func(args ...string) ([]byte, error) { t.Fatalf("碰了 launchctl：%v", args); return nil, nil }
	o.Services, o.Backup, o.Watchdog = true, true, true
	mustInstall(t, i, o)
	r := mustReceipt(t, i)
	if len(r.Services) != 5 || len(f.enabled) != 5 {
		t.Fatalf("服务：%v %v", r.Services, f.enabled)
	}
	unitDir := filepath.Join(i.Home, ".config", "systemd", "user")
	for _, label := range r.Services {
		if _, ok := r.Owned[filepath.Join(unitDir, label+".service")]; !ok {
			t.Errorf("收据没记 %s.service", label)
		}
		if _, ok := r.Owned[filepath.Join(unitDir, label+".timer")]; !ok {
			t.Errorf("收据没记 %s.timer", label)
		}
		if st, err := os.Stat(filepath.Join(unitDir, label+".service")); err != nil || st.Mode().Perm() != 0600 {
			t.Errorf("unit 文件权限：%v %v", st, err)
		}
	}
	if !strings.Contains(out.String(), "loginctl enable-linger") {
		t.Errorf("装完要提示 linger：\n%s", out.String())
	}
	if slices.Contains(f.calls, "enable-linger") {
		t.Errorf("不能擅自 enable-linger：%v", f.calls)
	}

	out.Reset()
	if e := i.Doctor(); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"Linger is off", "loginctl enable-linger", "macOS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor 少了 %q：\n%s", want, out.String())
		}
	}

	// doctor 发现服务没开
	delete(f.enabled, r.Services[0]+".timer")
	delete(f.active, r.Services[0]+".timer")
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), r.Services[0]) {
		t.Fatalf("doctor 应报服务没开：%v", e)
	}
	f.enabled[r.Services[0]+".timer"] = true
	f.active[r.Services[0]+".timer"] = true
	// doctor 发现 systemctl 不可用
	f.fail["show-environment"] = errors.New("exit status 1")
	if e := i.Doctor(); e == nil || !strings.Contains(e.Error(), "WSL") {
		t.Fatalf("doctor 应明说不可用：%v", e)
	}
	delete(f.fail, "show-environment")

	// 升级保留所选服务，先停后起
	first := r
	o.Services, o.Backup, o.Watchdog = false, false, false
	mustInstall(t, i, o)
	second := mustReceipt(t, i)
	if second.Generation == first.Generation || len(f.enabled) != 5 {
		t.Fatalf("升级后服务：%v", f.enabled)
	}
	// 升级中 enable 失败：回滚到升级前，服务还在
	f.failUnit[first.Services[0]+".timer"] = true
	if e := i.Setup(o); e == nil {
		t.Fatal("应当失败")
	}
	if got := mustReceipt(t, i); got.Generation != second.Generation {
		t.Fatalf("失败后没回到原来的代：%s", got.Generation)
	}
	if _, e := os.Stat(i.PendingPath()); !os.IsNotExist(e) {
		t.Fatalf("事务没收尾：%v", e)
	}
	if len(f.enabled) != 5 {
		t.Errorf("失败回滚后服务应全部恢复：%v", f.enabled)
	}

	if e := i.Rollback(true); e != nil {
		t.Fatal(e)
	}
	if got := mustReceipt(t, i); got.Generation != first.Generation || len(f.enabled) != 5 {
		t.Fatalf("回滚后：%s %v", got.Generation, f.enabled)
	}

	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if len(f.enabled) != 0 || len(f.active) != 0 {
		t.Fatalf("卸载后还开着：%v %v", f.enabled, f.active)
	}
	if entries, _ := os.ReadDir(unitDir); len(entries) != 0 {
		t.Fatalf("unit 文件没删干净：%v", entries)
	}
	if f.calls[len(f.calls)-1] != "daemon-reload" {
		t.Errorf("卸载要 daemon-reload：%v", f.calls[len(f.calls)-3:])
	}

	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
	if len(f.enabled) != 5 {
		t.Fatalf("恢复后服务：%v", f.enabled)
	}
	if e := i.Doctor(); e != nil {
		t.Fatal(e)
	}
}

// 没选服务的安装在没有 systemd 的机器上（容器、WSL 没开）也要能跑通：一次 systemctl 都不调。
func TestSetupWithoutServicesNeedsNoSystemd(t *testing.T) {
	i, o := installFixture(t)
	f := newFakeSystemd(i.Home)
	f.fail["show-environment"] = errors.New("exit status 1")
	f.fail["daemon-reload"] = errors.New("exit status 1")
	f.fail["show"] = errors.New("exit status 1")
	i.Systemctl = f.run
	i.Loginctl = func(args ...string) ([]byte, error) { return nil, errors.New("no loginctl") }
	mustInstall(t, i, o)
	if len(f.calls) != 0 {
		t.Fatalf("没选服务不该碰 systemctl：%v", f.calls)
	}
	if e := i.Doctor(); e != nil {
		t.Fatal(e)
	}
	if e := i.Remove(true); e != nil {
		t.Fatal(e)
	}
	if e := i.Restore(true); e != nil {
		t.Fatal(e)
	}
	if len(f.calls) != 0 {
		t.Fatalf("没选服务不该碰 systemctl：%v", f.calls)
	}
}

func TestSetupServicesRefusedWhenSystemdUnavailable(t *testing.T) {
	i, o := installFixture(t)
	f := newFakeSystemd(i.Home)
	f.fail["show-environment"] = errors.New("exit status 1")
	i.Systemctl = f.run
	o.Services = true
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "WSL") {
		t.Fatalf("应明确报错：%v", e)
	}
	if _, e := os.Stat(i.ReceiptPath()); !os.IsNotExist(e) {
		t.Fatalf("不可用时不该落收据：%v", e)
	}
	if _, e := os.Stat(filepath.Join(i.Home, ".config", "systemd")); !os.IsNotExist(e) {
		t.Fatalf("不可用时不该写 unit：%v", e)
	}
}

func TestSetupSystemdRefusesForeignUnit(t *testing.T) {
	i, o := installFixture(t)
	f := newFakeSystemd(i.Home)
	i.Systemctl = f.run
	i.Loginctl = func(args ...string) ([]byte, error) { return []byte("yes\n"), nil }
	o.Services = true
	foreign := filepath.Join(f.dir, config.LaunchdLabel+".service")
	testkit.WriteInstall(t, foreign, "[Service]\nExecStart=/bin/true\n", 0o600)
	if e := i.Setup(o); e == nil || !strings.Contains(e.Error(), "not owned by agentq") {
		t.Fatalf("别人的同名 unit 不能覆盖：%v", e)
	}
	testkit.AssertBytes(t, foreign, "[Service]\nExecStart=/bin/true\n")
}

func TestSystemdDoctorLinger(t *testing.T) {
	a, f, _ := newSystemdApp(t)
	testkit.WriteInstall(t, a.BinPath(), "#!/bin/sh\n", 0o755)
	if err := a.Service.CmdInstall(); err != nil {
		t.Fatal(err)
	}
	labels := []string{a.Service.ServiceLabel()}
	a.Service.Loginctl = func(args ...string) ([]byte, error) {
		if !slices.Contains(args, "--property=Linger") {
			t.Errorf("loginctl 只读 Linger：%v", args)
		}
		return []byte("yes\n"), nil
	}
	notes, err := a.Service.Svc().Doctor(labels)
	if err != nil || strings.Contains(strings.Join(notes, "\n"), "Linger is off") {
		t.Errorf("linger 开着时不用提示：%v %v", notes, err)
	}
	a.Service.Loginctl = func(args ...string) ([]byte, error) { return nil, errors.New("no loginctl") }
	notes, err = a.Service.Svc().Doctor(labels)
	if err != nil || !strings.Contains(strings.Join(notes, "\n"), "Cannot read linger") {
		t.Errorf("查不到 linger 要如实说：%v %v", notes, err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "linger") }) {
		t.Errorf("systemctl 不该收到 linger 相关调用：%v", f.calls)
	}
}

// 巡逻走同一接口：把 systemctl show 的结果读成和 launchctl list 一样的判断。
func systemdPatrolEnv(t *testing.T) (*patrolEnv, *fakeSystemd) {
	t.Helper()
	e := newPatrolEnv(t)
	f := newFakeSystemd(e.a.Home)
	e.a.Service.Launchctl = nil
	e.a.Service.Systemctl = f.run
	for _, s := range e.a.Service.Svc().Specs()[:3] {
		for _, file := range s.Files {
			e.write(file.Path, file.Content)
		}
	}
	f.run("daemon-reload")
	for _, s := range e.a.Service.Svc().Specs()[:3] {
		f.run("enable", "--now", s.Label+".timer")
	}
	f.calls = nil
	return e, f
}

func TestSystemdPatrolHealthy(t *testing.T) {
	e, f := systemdPatrolEnv(t)
	r := e.patrol()
	for _, is := range r.New {
		if is.Check == "launchd" {
			t.Errorf("都正常时不该报：%+v", is)
		}
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "show ") {
			t.Errorf("巡逻只读状态，收到 %q", c)
		}
	}
	if len(f.calls) == 0 {
		t.Errorf("巡逻应读 systemctl show")
	}
}

func TestSystemdPatrolFailures(t *testing.T) {
	e, f := systemdPatrolEnv(t)
	label := e.a.Service.ServiceLabel()
	logp := filepath.Join(e.a.Home, ".agentq", "launchd.log")
	e.write(logp, "第一行\ntick 出错了\n")
	f.status[label+".service"], f.result[label+".service"] = "3", "exit-code"
	f.status[e.a.Service.RoutineLabel(config.KindReview)+".service"], f.result[e.a.Service.RoutineLabel(config.KindReview)+".service"] = "9", "signal"
	r := e.patrol()
	var tick, review *patrol.PatrolIssue
	for k := range r.New {
		switch r.New[k].Fingerprint {
		case "launchd:" + label:
			tick = &r.New[k]
		case "launchd:" + e.a.Service.RoutineLabel(config.KindReview):
			review = &r.New[k]
		}
	}
	if tick == nil || !strings.Contains(tick.Title, "systemd --user 服务 "+label+" 上次退出码 3") || !strings.Contains(strings.Join(tick.Evidence, "\n"), "tick 出错了") {
		t.Fatalf("tick 退出码 3 要带日志证据：%+v", r.New)
	}
	if strings.Contains(tick.Title, "launchd") || strings.Contains(strings.Join(tick.Evidence, "\n"), "launchctl") {
		t.Errorf("Linux 的证据不该提 launchd：%+v", tick)
	}
	if review == nil || !strings.Contains(strings.Join(review.Evidence, "\n"), "被信号 9 终止") {
		t.Fatalf("复盘被信号杀要写明：%+v", r.New)
	}
	// 修好后下次退出码回 0：算已恢复
	delete(f.status, label+".service")
	delete(f.result, label+".service")
	delete(f.status, e.a.Service.RoutineLabel(config.KindReview)+".service")
	delete(f.result, e.a.Service.RoutineLabel(config.KindReview)+".service")
	r = e.patrol()
	if len(r.New) != 0 {
		t.Fatalf("恢复后不该再报：%+v", r.New)
	}
	if len(r.Gone) != 2 {
		t.Errorf("两条都算恢复：%+v", r.Gone)
	}
}

func TestSystemdPatrolMissingRoutineAndListen(t *testing.T) {
	e, f := systemdPatrolEnv(t)
	night := e.a.Service.RoutineLabel(config.KindNight)
	delete(f.enabled, night+".timer")
	delete(f.active, night+".timer")
	r := e.patrol()
	found := false
	for _, is := range r.New {
		if is.Fingerprint == "launchd:"+night+":missing" {
			found = true
			if !strings.Contains(strings.Join(is.Evidence, "\n"), "systemctl --user show") {
				t.Errorf("证据来源：%+v", is)
			}
		}
	}
	if !found {
		t.Fatalf("夜班定点服务没开要报：%+v", r.New)
	}
	e.a.Cfg.NotifyEnabled = true
	testkit.WriteInstall(t, e.a.Telegram.TelegramEnvPath(), "TELEGRAM_BOT_TOKEN=1:x\nTELEGRAM_CHAT_ID=1\n", 0o600)
	states, err := e.a.Service.Svc().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Label == e.a.Service.ListenLabel() {
			t.Errorf("没装的 listen 不该在列表里：%+v", s)
		}
	}
	// 装上 listen 但没在跑
	for _, s := range e.a.Service.Svc().Specs()[3:] {
		for _, file := range s.Files {
			e.write(file.Path, file.Content)
		}
	}
	f.run("daemon-reload")
	f.run("enable", "--now", e.a.Service.ListenLabel()+".service")
	states, _ = e.a.Service.Svc().List()
	var got service.ServiceState
	for _, s := range states {
		if s.Label == e.a.Service.ListenLabel() {
			got = s
		}
	}
	if got.Pid != "-" {
		t.Errorf("MainPID=0 应读成没在跑：%+v", got)
	}
	f.mainPID[e.a.Service.ListenLabel()+".service"] = "4242"
	states, _ = e.a.Service.Svc().List()
	for _, s := range states {
		if s.Label == e.a.Service.ListenLabel() && s.Pid != "4242" {
			t.Errorf("pid 没读出来：%+v", s)
		}
	}
}

func TestSystemdPatrolListFailure(t *testing.T) {
	e, f := systemdPatrolEnv(t)
	f.fail["show"] = errors.New("exit status 1")
	r := e.patrol()
	found := false
	for _, n := range r.Notes {
		if strings.Contains(n, "systemd --user") && strings.Contains(n, "No medium found") {
			found = true
		}
	}
	if !found {
		t.Fatalf("读不了状态要记进 Notes：%+v", r.Notes)
	}
}

func TestSystemdLogTail(t *testing.T) {
	a, _, _ := newSystemdApp(t)
	m := a.Service.Svc()
	if got := m.LogTail("nope"); got != "" {
		t.Errorf("没有 unit 时返回空：%q", got)
	}
	spec := m.Specs()[0]
	for _, f := range spec.Files {
		testkit.WriteInstall(t, f.Path, f.Content, 0o600)
	}
	testkit.WriteInstall(t, a.LaunchdLogPath(), "1\n2\n3\n4\n", 0o600)
	got := m.LogTail(spec.Label)
	if !strings.Contains(got, a.LaunchdLogPath()) || !strings.Contains(got, "2") || strings.Contains(got, "1 ") {
		t.Errorf("日志末尾 3 行：%q", got)
	}
}

// 真实命令的调用方式：--user 在最前，超时、找不到命令都有明确错误。用 PATH 里的假 systemctl，不碰真实的。
func TestRunSystemctlUsesUserManager(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	testkit.WriteInstall(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\necho \"$@\" > "+fsx.ShQuote(rec)+"\necho \"XDG=$XDG_RUNTIME_DIR\" >> "+fsx.ShQuote(rec)+"\necho out\n", 0o755)
	t.Setenv("PATH", dir)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	out, err := service.RunSystemctl("show-environment")
	if err != nil || !strings.Contains(string(out), "out") {
		t.Fatalf("%v %s", err, out)
	}
	b, _ := os.ReadFile(rec)
	if !strings.HasPrefix(string(b), "--user show-environment\n") || !strings.Contains(string(b), "XDG=/run/user/test") {
		t.Errorf("参数：%s", b)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := service.RunSystemctl("show-environment"); err == nil || !strings.Contains(err.Error(), "systemctl") {
		t.Errorf("没有 systemctl 要明说：%v", err)
	}
}

func TestProcEnvMarked(t *testing.T) {
	root := t.TempDir()
	testkit.WriteInstall(t, filepath.Join(root, "10", "environ"), "A=1\x00AGENTQ_JOB=other\x00", 0o600)
	testkit.WriteInstall(t, filepath.Join(root, "11", "environ"), "AGENTQ_JOB=job-1\x00B=2\x00", 0o600)
	if !proc.ProcEnvMarked(root, []string{"10", "11"}, "AGENTQ_JOB=job-1") {
		t.Errorf("11 带着标记")
	}
	if proc.ProcEnvMarked(root, []string{"10"}, "AGENTQ_JOB=job-1") {
		t.Errorf("10 是别的任务")
	}
	if proc.ProcEnvMarked(root, []string{"10"}, "AGENTQ_JOB=othe") {
		t.Errorf("不能按前缀认")
	}
	if proc.ProcEnvMarked(root, []string{"99"}, "AGENTQ_JOB=job-1") {
		t.Errorf("读不了（已退出、别的用户）就当没有，宁可不杀")
	}
}

// 本机通知后备：有 notify-send 就调，没有或发不出（无桌面）只写日志，不让推送队列卡住。
func TestNotifySendFallback(t *testing.T) {
	a, _, _ := newSystemdApp(t)
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	testkit.WriteInstall(t, filepath.Join(dir, "notify-send"), "#!/bin/sh\nfor a in \"$@\"; do echo \"[$a]\"; done > "+fsx.ShQuote(rec)+"\n", 0o755)
	t.Setenv("PATH", dir)
	if err := notify.NotifySendOrLog(a.Notify, "标题", "正文"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rec)
	if !strings.Contains(string(b), "[标题]") || !strings.Contains(string(b), "[正文]") {
		t.Errorf("参数：%s", b)
	}

	testkit.WriteInstall(t, filepath.Join(dir, "notify-send"), "#!/bin/sh\necho 'cannot open display' >&2\nexit 1\n", 0o755)
	err := notify.NotifySendOrLog(a.Notify, "发不出", "正文2")
	if !errors.Is(err, notify.ErrLocalNotify) || !strings.Contains(err.Error(), "cannot open display") {
		t.Errorf("发不出要让调用方知道没送达：%v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := notify.NotifySendOrLog(a.Notify, "没有工具", "正文3"); err != nil {
		t.Errorf("没有 notify-send 时只写日志：%v", err)
	}
	log := a.Out.(*bytes.Buffer).String()
	for _, want := range []string{"没有工具", "正文3", "notify-send"} {
		if !strings.Contains(log, want) {
			t.Errorf("日志少了 %q：\n%s", want, log)
		}
	}
}

// 后台进程没有会话总线的环境：notify-send 要能拿到；已有的不覆盖；socket 不在不编。
func TestNotifySendEnv(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	if env := notify.NotifySendEnv([]string{"XDG_RUNTIME_DIR=/r"}, yes); !slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/r/bus") {
		t.Errorf("%v", env)
	}
	if env := notify.NotifySendEnv([]string{"XDG_RUNTIME_DIR=/r", "DBUS_SESSION_BUS_ADDRESS="}, yes); !slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/r/bus") {
		t.Errorf("空值不算有：%v", env)
	}
	if env := notify.NotifySendEnv([]string{"XDG_RUNTIME_DIR=/r", "DBUS_SESSION_BUS_ADDRESS=unix:path=/x"}, yes); slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/r/bus") {
		t.Errorf("已有的不覆盖：%v", env)
	}
	if env := notify.NotifySendEnv([]string{"XDG_RUNTIME_DIR=/r"}, no); slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "DBUS_") }) {
		t.Errorf("socket 不在不设：%v", env)
	}
	uid := strconv.Itoa(os.Getuid())
	if env := notify.NotifySendEnv([]string{"A=1"}, yes); !slices.Contains(env, "XDG_RUNTIME_DIR=/run/user/"+uid) || !slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+uid+"/bus") {
		t.Errorf("都缺时都补：%v", env)
	}

	// 真的传给了 notify-send
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	runtime := filepath.Join(dir, "run")
	testkit.WriteInstall(t, filepath.Join(runtime, "bus"), "", 0o600)
	testkit.WriteInstall(t, filepath.Join(dir, "notify-send"), "#!/bin/sh\necho \"$DBUS_SESSION_BUS_ADDRESS\" > "+fsx.ShQuote(rec)+"\n", 0o755)
	t.Setenv("PATH", dir)
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	a, _, _ := newSystemdApp(t)
	if err := notify.NotifySendOrLog(a.Notify, "标题", "正文"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rec); strings.TrimSpace(string(b)) != "unix:path="+filepath.Join(runtime, "bus") {
		t.Errorf("notify-send 收到的总线地址：%q", b)
	}
	// 只给 notify-send：claude 的白名单不放
	if proc.ChildEnvKeep["DBUS_SESSION_BUS_ADDRESS"] || proc.ChildEnvKeep["XDG_RUNTIME_DIR"] {
		t.Errorf("白名单不该变")
	}
}

// notify-send 发不出：消息不算送达、留在队列；一小时内不再试，日志不刷；能发了就送出去。
func TestNotifySendFailureKeepsQueue(t *testing.T) {
	a, _, _ := newSystemdApp(t)
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := func(code int) {
		testkit.WriteInstall(t, filepath.Join(dir, "notify-send"), fmt.Sprintf("#!/bin/sh\necho x >> %s\necho 'no session bus' >&2\nexit %d\n", fsx.ShQuote(count), code), 0o755)
	}
	calls := func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}
	script(1)
	t.Setenv("PATH", dir)
	a.Notify.MacNotify = func(title, body string) error { return notify.NotifySendOrLog(a.Notify, title, body) }
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	a.Now = func() time.Time { return now }
	s := &notify.State{Pending: []notify.Notice{{Cat: "alert", Key: "k", Text: "要紧的事", At: now}}}

	a.Notify.FlushNotify(s, nil, nil, nil)
	if calls() != 1 || len(s.Pending) != 1 || !s.LastSent["alert"].IsZero() || s.Count != 0 {
		t.Fatalf("没送达不能算发出：调用 %d，队列 %d，%+v", calls(), len(s.Pending), s)
	}
	if !strings.Contains(s.LastError, "no session bus") {
		t.Errorf("错误要记下：%q", s.LastError)
	}
	logBefore := a.Out.(*bytes.Buffer).String()
	if strings.Count(logBefore, "发送失败") != 1 {
		t.Errorf("记一行：\n%s", logBefore)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Minute)
		a.Notify.FlushNotify(s, nil, nil, nil)
	}
	if calls() != 1 || a.Out.(*bytes.Buffer).String() != logBefore {
		t.Errorf("一小时内不再试、不刷日志：调用 %d\n%s", calls(), a.Out)
	}
	now = now.Add(time.Hour)
	a.Notify.FlushNotify(s, nil, nil, nil)
	if calls() != 2 || len(s.Pending) != 1 {
		t.Errorf("到点再试：调用 %d，队列 %d", calls(), len(s.Pending))
	}
	script(0)
	now = now.Add(2 * time.Hour)
	a.Notify.FlushNotify(s, nil, nil, nil)
	if calls() != 3 || len(s.Pending) != 0 || s.Count != 1 {
		t.Errorf("能发了就送出去：调用 %d，队列 %d，%+v", calls(), len(s.Pending), s)
	}
}
