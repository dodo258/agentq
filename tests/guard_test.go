package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/fsx"
	"agentq/internal/testkit"
)

// guardEnv 准备临时 HOME（路径里带空格和单引号，验证 sh 转义）、生成保护脚本、放假的 agentq 和 agentq.prev。
// 假 agentq 的退出码读 $HOME/code，agentq.prev 总是返回 0。
func guardEnv(t *testing.T, withPrev bool) (*App, func() (int, string)) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "it's home")
	var out bytes.Buffer
	a, err := newApp(home, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(a.Service.GuardPath(), []byte(a.Service.GuardContent())); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(a.BinPath()), 0o755)
	os.WriteFile(a.BinPath(), []byte("#!/bin/sh\n# NEW\nexit $(cat \"$HOME/code\")\n"), 0o755)
	if withPrev {
		os.WriteFile(a.BinPath()+".prev", []byte("#!/bin/sh\n# PREV\nexit 0\n"), 0o755)
	}
	setCode(t, home, "0")
	run := func() (int, string) {
		cmd := exec.Command("/bin/sh", a.Service.GuardPath())
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		b, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, string(b)
	}
	return a, run
}

func setCode(t *testing.T, home, code string) {
	if err := os.WriteFile(filepath.Join(home, "code"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// 正常时不回滚：一直返回 0，或失败 1 次后又成功（计数清零），agentq 都不换，汇总不写。
func TestGuardNoRollbackWhenHealthy(t *testing.T) {
	a, run := guardEnv(t, true)
	for i := 0; i < 3; i++ {
		if code, out := run(); code != 0 {
			t.Fatalf("第 %d 次应返回 0：%d %s", i+1, code, out)
		}
	}
	if exists(a.Service.GuardFailsPath()) {
		t.Error("一直成功时不应有失败计数")
	}
	setCode(t, a.Home, "1")
	if code, out := run(); code != 1 || !strings.Contains(out, "连续第 1 次非 0") {
		t.Fatalf("失败 1 次：%d %s", code, out)
	}
	setCode(t, a.Home, "0")
	if code, _ := run(); code != 0 || exists(a.Service.GuardFailsPath()) {
		t.Fatalf("成功后计数应清零：%d", code)
	}
	setCode(t, a.Home, "1")
	if code, out := run(); code != 1 || !strings.Contains(out, "连续第 1 次非 0") {
		t.Fatalf("清零后再失败应从 1 计：%d %s", code, out)
	}
	if !strings.Contains(testkit.ReadFile(t, a.BinPath()), "# NEW") || !exists(a.BinPath()+".prev") {
		t.Error("不应回滚")
	}
	if exists(a.NightlyPath(a.Now())) {
		t.Error("不应写汇总")
	}
}

// 替换后连续 2 次失败：agentq.prev 换回来，坏的留成 agentq.bad，当天汇总写「已自动回滚」；换回的版本正常运行。
func TestGuardRollsBackAfterTwoFailures(t *testing.T) {
	a, run := guardEnv(t, true)
	setCode(t, a.Home, "3")
	if code, out := run(); code != 3 || strings.Contains(out, "回滚") {
		t.Fatalf("第 1 次失败不应回滚：%d %s", code, out)
	}
	code, out := run()
	if code != 3 || !strings.Contains(out, "已自动回滚") {
		t.Fatalf("第 2 次失败应回滚：%d %s", code, out)
	}
	if !strings.Contains(testkit.ReadFile(t, a.BinPath()), "# PREV") {
		t.Error("agentq 应换成 agentq.prev")
	}
	if exists(a.BinPath() + ".prev") {
		t.Error("agentq.prev 换回后不应还在")
	}
	if !strings.Contains(testkit.ReadFile(t, a.BinPath()+".bad"), "# NEW") {
		t.Error("出错的版本应留成 agentq.bad")
	}
	if exists(a.Service.GuardFailsPath()) {
		t.Error("回滚后计数应清零")
	}
	n := testkit.ReadFile(t, a.NightlyPath(a.Now()))
	if !strings.HasPrefix(n, "# agentq 任务汇总 "+a.Now().Format("2006-01-02")+"\n\n- ") ||
		!strings.Contains(n, "已自动回滚：agentq tick 连续 2 次非 0 退出（最后一次退出码 3）") ||
		!strings.Contains(n, ".prev（"+a.Now().Format("2006-01-02")) {
		t.Errorf("汇总：%s", n)
	}
	if code, out := run(); code != 0 {
		t.Errorf("换回的版本应正常：%d %s", code, out)
	}
}

// 没有 agentq.prev：第 2 次失败只写一行报警，不换；之后再失败不重复写。
func TestGuardNoPrev(t *testing.T) {
	a, run := guardEnv(t, false)
	setCode(t, a.Home, "1")
	for i := 0; i < 3; i++ {
		run()
	}
	n := testkit.ReadFile(t, a.NightlyPath(a.Now()))
	if strings.Count(n, "没有可换回") != 1 || strings.Contains(n, "已自动回滚") {
		t.Errorf("汇总：%s", n)
	}
	if !strings.Contains(testkit.ReadFile(t, a.BinPath()), "# NEW") || exists(a.BinPath()+".bad") {
		t.Error("没有 agentq.prev 时不应动 agentq")
	}
}
