package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentq/internal/testkit"
)

// fakeSource 写一个假的 agentq 源码目录：每次运行把 HOME 和参数追加到 $AGENTQ_FAKE_REC，tick 返回 tickCode。
// body 非空时直接用它当 main.go（用来造编译错误）。
func fakeSource(t *testing.T, tickCode int, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body == "" {
		body = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	f, _ := os.OpenFile(os.Getenv("AGENTQ_FAKE_REC"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	fmt.Fprintf(f, "%s %s\n", os.Getenv("HOME"), strings.Join(os.Args[1:], " "))
	f.Close()
	if len(os.Args) > 1 && os.Args[1] == "tick" {
		os.Exit(` + strconv.Itoa(tickCode) + `)
	}
}
`
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fake\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(body), 0o644)
	return dir
}

// runUpdate 用临时 HOME 执行 scripts/update.sh；GOCACHE 沿用本机的，免得每次从头编译标准库。
func runUpdate(t *testing.T, home, src string) (int, string) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("没有 go")
	}
	cache, err := exec.Command(goBin, "env", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "scripts/update.sh", src)
	cmd.Env = []string{
		"HOME=" + home, "PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "TMPDIR=" + t.TempDir(),
		"GOCACHE=" + strings.TrimSpace(string(cache)), "GOTOOLCHAIN=local", "GOFLAGS=",
		"AGENTQ_FAKE_REC=" + filepath.Join(home, "rec"),
	}
	testkit.GoTelemetryOff(t, goBin, cmd.Env)
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}

// updateHome 准备一个装着旧版 agentq、保护脚本失败计数为 1 的临时 HOME。
func updateHome(t *testing.T) (home, bin, fails string) {
	home = t.TempDir()
	bin = filepath.Join(home, ".local", "bin", "agentq")
	fails = filepath.Join(home, ".agentq", "guard-fails")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.MkdirAll(filepath.Dir(fails), 0o755)
	os.WriteFile(bin, []byte("OLD"), 0o755)
	os.WriteFile(fails, []byte("1\n"), 0o644)
	return
}

// 新版本自检失败（tick 非 0）或编译不过：不替换、不留 agentq.prev、不动失败计数。
func TestUpdateSelfCheckFailKeepsOld(t *testing.T) {
	for name, src := range map[string]string{
		"自检失败": fakeSource(t, 3, ""),
		"编译失败": fakeSource(t, 0, "package main\nfunc main() { x }\n"),
	} {
		t.Run(name, func(t *testing.T) {
			home, bin, fails := updateHome(t)
			code, out := runUpdate(t, home, src)
			if code == 0 || !strings.Contains(out, name) || !strings.Contains(out, "没有替换") {
				t.Fatalf("应失败并说明没有替换：%d %s", code, out)
			}
			if testkit.ReadFile(t, bin) != "OLD" {
				t.Error("agentq 不应被替换")
			}
			if exists(bin + ".prev") {
				t.Error("不应留 agentq.prev")
			}
			if testkit.ReadFile(t, fails) != "1\n" {
				t.Error("失败计数不应变")
			}
		})
	}
}

// 自检通过：用临时 HOME 跑过 status 和 tick，旧版本留成 agentq.prev，新的装上，失败计数清零。
func TestUpdateInstallsAndKeepsPrev(t *testing.T) {
	home, bin, fails := updateHome(t)
	code, out := runUpdate(t, home, fakeSource(t, 0, ""))
	if code != 0 || !strings.Contains(out, "自检通过") {
		t.Fatalf("应成功：%d %s", code, out)
	}
	if testkit.ReadFile(t, bin+".prev") != "OLD" {
		t.Error("旧版本应留成 agentq.prev")
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 || testkit.ReadFile(t, bin) == "OLD" {
		t.Errorf("新版本没装上：%v", err)
	}
	if exists(fails) {
		t.Error("失败计数应清零")
	}
	rec := strings.Split(strings.TrimSpace(testkit.ReadFile(t, filepath.Join(home, "rec"))), "\n")
	if len(rec) != 2 || !strings.HasSuffix(rec[0], " status") || !strings.HasSuffix(rec[1], " tick") {
		t.Fatalf("自检应依次跑 status、tick：%q", rec)
	}
	for _, r := range rec {
		if strings.HasPrefix(r, home+" ") {
			t.Errorf("自检不应用真实 HOME：%s", r)
		}
	}
	// 再装一次：agentq.prev 换成刚才装上的那个
	cur := testkit.ReadFile(t, bin)
	if code, out := runUpdate(t, home, fakeSource(t, 0, "")); code != 0 {
		t.Fatalf("再装应成功：%s", out)
	}
	if testkit.ReadFile(t, bin+".prev") != cur {
		t.Error("agentq.prev 应是上一次装的版本")
	}
}

// 第一次安装（还没有 agentq）：直接装上，没有 agentq.prev。
func TestUpdateFirstInstall(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "agentq")
	if code, out := runUpdate(t, home, fakeSource(t, 0, "")); code != 0 {
		t.Fatalf("应成功：%s", out)
	}
	if !exists(bin) || exists(bin+".prev") {
		t.Error("应装上且没有 agentq.prev")
	}
}

// 现在装着的版本在 launchd 下已经失败过（guard-fails 非空）且已有 agentq.prev：保留原来的 prev，不拿可能坏的版本覆盖它。
func TestUpdateKeepsPrevWhenCurrentFailing(t *testing.T) {
	home, bin, fails := updateHome(t)
	os.WriteFile(bin+".prev", []byte("GOOD"), 0o755)
	code, out := runUpdate(t, home, fakeSource(t, 0, ""))
	if code != 0 || !strings.Contains(out, "保留原来的") {
		t.Fatalf("应成功并说明保留 prev：%d %s", code, out)
	}
	if testkit.ReadFile(t, bin+".prev") != "GOOD" || testkit.ReadFile(t, bin) == "OLD" || exists(fails) {
		t.Error("应装上新版本、保留原来的 agentq.prev、清零计数")
	}
}
