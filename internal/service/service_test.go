package service

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"agentq/internal/config"
	"agentq/internal/env"
	"agentq/internal/testkit"
)

// launchd、systemd 的服务文件和保护脚本全文。家目录写死成假路径，Linux、macOS 上都能生成。
func TestGoldenServiceFiles(t *testing.T) {
	a := &Service{Env: &env.Env{Home: "/srv/agentq-home", Cfg: config.Default()}}
	section := func(b *bytes.Buffer, name, content string) {
		fmt.Fprintf(b, "==== %s\n%s\n", name, content)
	}
	var plist bytes.Buffer
	section(&plist, "plist", a.PlistContent())
	section(&plist, "routine night", a.RoutinePlistContent(config.Routines[config.KindNight]))
	section(&plist, "routine review", a.RoutinePlistContent(config.Routines[config.KindReview]))
	section(&plist, "listen", a.ListenPlistContent())
	for _, kind := range []string{"backup", "watchdog"} {
		section(&plist, "aux "+kind, AuxiliaryService(a.Home, a.Cfg, kind, 3600).Plist)
	}
	testkit.GoldenCompare(t, "launchd.txt", plist.Bytes())

	// 取上次构建时间的命令 macOS、Linux 不一样，它的值由根目录的 TestDarwinDefaultManagerIsLaunchd、TestLinuxDefaultManagerIsSystemd 查。
	testkit.GoldenCompare(t, "guard.sh", []byte(strings.ReplaceAll(a.GuardContent(), guardPrevTimeCmd, "<GUARD_PREV_TIME_CMD>")))

	m := SystemdManager{a: a}
	var units bytes.Buffer
	specs := m.Specs()
	specs = append(specs, m.AuxSpec("backup", 3600), m.AuxSpec("watchdog", 3600))
	for _, s := range specs {
		for _, f := range s.Files {
			section(&units, f.Path, f.Content)
		}
	}
	testkit.GoldenCompare(t, "systemd.txt", units.Bytes())
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote(`a b"c\d%e$f`); got != `"a b\"c\\d%%e$$f"` {
		t.Errorf("ExecStart 转义：%s", got)
	}
	if got := systemdEnvQuote(`A=b "c" 100% $x`); got != `"A=b \"c\" 100%% $x"` {
		t.Errorf("Environment 转义（$ 不展开）：%s", got)
	}
}

func TestParseSystemdShow(t *testing.T) {
	units := parseSystemdShow("warning: x\nId=a.service\nLoadState=loaded\nResult=exit-code\nExecMainStatus=7\n\nId=b.timer\nLoadState=not-found\n")
	if units["a.service"]["Result"] != "exit-code" || units["a.service"]["ExecMainStatus"] != "7" || units["b.timer"]["LoadState"] != "not-found" {
		t.Errorf("解析不对：%+v", units)
	}
	for _, tc := range []struct {
		result, status, want string
	}{{"success", "0", "0"}, {"", "0", "0"}, {"exit-code", "7", "7"}, {"signal", "9", "-9"}, {"core-dump", "11", "-11"}, {"timeout", "0", "1"}, {"start-limit-hit", "0", "1"}, {"exit-code", "0", "1"}} {
		if got := systemdExitCode(tc.result, tc.status); got != tc.want {
			t.Errorf("Result=%s ExecMainStatus=%s：%s，想要 %s", tc.result, tc.status, got, tc.want)
		}
	}
}

func TestLinuxOSVersion(t *testing.T) {
	if got := ParseOSRelease("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\n"); got != "Ubuntu 24.04.3 LTS" {
		t.Errorf("%q", got)
	}
	if got := ParseOSRelease("NAME=Arch\n"); got != "" {
		t.Errorf("没有 PRETTY_NAME 返回空：%q", got)
	}
}

// 检查环境要带 USER：真实 launchd 给，macOS 上 CLI 靠它从钥匙串取订阅登录。少了它 start 会把登录好的用户判成没登录。
func TestLaunchdEnvHasUser(t *testing.T) {
	a := &Service{Env: &env.Env{Home: t.TempDir()}}
	want := []string{"HOME", "PATH", "USER", "LOGNAME"}
	got := a.LaunchdEnv()
	if len(got) != len(want) {
		t.Fatalf("只带这几个：%v", got)
	}
	for i, k := range want {
		if !strings.HasPrefix(got[i], k+"=") || got[i] == k+"=" {
			t.Errorf("缺 %s：%v", k, got)
		}
	}
}
