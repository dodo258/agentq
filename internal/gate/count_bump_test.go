package gate_test

import (
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/gate"
	"agentq/internal/testkit"
)

// backend-count-hotspot 的检查：测试里把写死的「有几项」跟着改了个数才报；改了期望值、新加的数、黄金文件、产品代码不报。
func TestCountBumpScript(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("capabilities", "count-bump.sh"))
	if err != nil {
		t.Fatal(err)
	}
	evalTest := "package tests\n\nfunc TestEval(t *testing.T) {\n\tif !strings.Contains(out, \"要跑 40 题（程序题 30，模型题 10）\") {\n\t}\n}\n"
	cases := []struct {
		name        string
		base, after map[string]string
		exit        int
		out         string
	}{
		{"题数跟着改", map[string]string{"tests/eval_test.go": evalTest},
			map[string]string{"tests/eval_test.go": strings.Replace(evalTest, "40 题（程序题 30", "41 题（程序题 31", 1)},
			1, "tests/eval_test.go:4：if !strings.Contains(out, \"要跑 41 题（程序题 31，模型题 10）\") {（40,30,10 → 41,31,10）"},
		{"len 写死", map[string]string{"cmds/cmds_test.go": "package cmds\nfunc T() {\n\tif len(Names) != 2 {\n\t}\n}\n"},
			map[string]string{"cmds/cmds_test.go": "package cmds\nfunc T() {\n\tif len(Names) != 3 {\n\t}\n}\n"},
			1, "cmds/cmds_test.go:3"},
		{"TS toHaveLength", map[string]string{"web/list.test.ts": "expect(items).toHaveLength(4)\n"},
			map[string]string{"web/list.test.ts": "expect(items).toHaveLength(5)\n"},
			1, "web/list.test.ts:1"},
		{"改期望值不算", map[string]string{"a/a_test.go": "package a\nfunc T() {\n\tif got != 30*time.Second {\n\t}\n}\n"},
			map[string]string{"a/a_test.go": "package a\nfunc T() {\n\tif got != 60*time.Second {\n\t}\n}\n"},
			0, "测试里没有跟着改写死的数量"},
		{"新加一行带数不算", map[string]string{"a/a_test.go": "package a\n"},
			map[string]string{"a/a_test.go": "package a\nfunc T() {\n\tif len(x) != 3 {\n\t}\n}\n"},
			0, "测试里没有跟着改写死的数量"},
		{"产品代码不算", map[string]string{"a/a.go": "package a\nconst maxCount = 3\n"},
			map[string]string{"a/a.go": "package a\nconst maxCount = 4\n"},
			0, "测试里没有跟着改写死的数量"},
		{"黄金文件不算", map[string]string{"tests/testdata/out.go": "// 3 题\n"},
			map[string]string{"tests/testdata/out.go": "// 4 题\n"},
			0, "测试里没有跟着改写死的数量"},
		{"改成数出来不算", map[string]string{"cmds/cmds_test.go": "package cmds\nfunc T() {\n\tif len(Names) != 2 {\n\t}\n}\n"},
			map[string]string{"cmds/cmds_test.go": "package cmds\nfunc T() {\n\tif len(Names) != len(want) {\n\t}\n}\n"},
			0, "测试里没有跟着改写死的数量"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
			writeFiles(t, dir, c.base)
			testkit.GitT(t, home, dir, "add", "-A")
			testkit.GitT(t, home, dir, "commit", "-q", "-m", "基点")
			base := strings.TrimSpace(testkit.GitT(t, home, dir, "rev-parse", "HEAD"))
			writeFiles(t, dir, c.after)
			testkit.GitT(t, home, dir, "add", "-A")
			testkit.GitT(t, home, dir, "commit", "-q", "-m", "改动")
			code, out := runGuard(t, script, home, dir, base)
			if code != c.exit || !strings.Contains(out, c.out) {
				t.Fatalf("退出码 %d（要 %d），输出：\n%s\n要含：%s", code, c.exit, out, c.out)
			}
		})
	}

	t.Run("基点不是提交判不了", func(t *testing.T) {
		home, dir := t.TempDir(), t.TempDir()
		testkit.GitT(t, home, dir, "init", "-q", "-b", "main")
		if code, out := runGuard(t, script, home, dir, "0000000000000000000000000000000000000000"); code != 125 {
			t.Fatalf("退出码 %d，要 125：%s", code, out)
		}
	})
}

// 清单里 backend-count-hotspot 的启用条件：这次改了测试代码才用，只改产品代码或文档不用。
func TestCountHotspotWhen(t *testing.T) {
	caps, err := gate.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	var w *gate.CapWhen
	for i := range caps {
		if caps[i].Name == "backend-count-hotspot" {
			w = &caps[i].When
		}
	}
	if w == nil {
		t.Fatal("清单里没有 backend-count-hotspot")
	}
	for _, f := range []string{"tests/eval_test.go", "cmds/cmds_test.go", "pkg/test_x.py", "web/a.spec.ts", "src/__tests__/a.js", "AppTests/AppTests.swift", "tests/prune.rs"} {
		if !gate.CapApplies(*w, nil, []string{"main.go", f}) {
			t.Errorf("改了 %s 要用上", f)
		}
	}
	for _, f := range []string{"main.go", "docs/a.md", "internal/gate/process.go", "web/App.tsx"} {
		if gate.CapApplies(*w, nil, []string{f}) {
			t.Errorf("只改 %s 不该用", f)
		}
	}
}
