package onboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentq/internal/testkit"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 每种项目类型一个夹具，看判断出的命令。
func TestDetectVerify(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string // verifySummary 的结果，空表示判断不了
		setup string // 第一条准备命令
	}{
		{"go", map[string]string{"go.mod": "module x\n"}, "go test ./...", ""},
		{"node npm", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`}, "CI=1 npm test", "npm install"},
		{"node npm 有锁", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "package-lock.json": "{}"}, "CI=1 npm test", "npm install"},
		{"node pnpm", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`, "pnpm-lock.yaml": ""}, "CI=1 pnpm test", "pnpm install"},
		{"node yarn", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`, "yarn.lock": ""}, "CI=1 yarn test", "yarn install"},
		{"node bun", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`, "bun.lockb": ""}, "CI=1 bun run test", "bun install"},
		{"node packageManager", map[string]string{"package.json": `{"packageManager":"pnpm@9.0.0","scripts":{"test":"vitest"}}`}, "CI=1 pnpm test", "pnpm install"},
		{"node 没有 test", map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`}, "", ""},
		{"node 占位 test", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, "", ""},
		{"node 坏 JSON", map[string]string{"package.json": `{`}, "", ""},
		{"rust", map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"}, "cargo test", ""},
		{"rust workspace", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"a\"]\n"}, "cargo test --workspace", ""},
		{"python uv", map[string]string{"pyproject.toml": "[dependency-groups]\ndev = [\"pytest\"]\n", "uv.lock": ""}, "uv run pytest", "uv sync"},
		{"python poetry", map[string]string{"pyproject.toml": "[tool.poetry.group.dev.dependencies]\npytest = \"*\"\n", "poetry.lock": ""}, "poetry run pytest", "poetry install --no-interaction"},
		{"python 包", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n[project.optional-dependencies]\ntest = [\"pytest\"]\n"}, `"$venv/bin/python" -m pytest`, `venv="$(mktemp -d)"`},
		{"python 没提 pytest", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"}, "", ""},
		{"python 只有工具配置", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\naddopts = \"-q\"\n"}, "", ""},
		{"swift", map[string]string{"Package.swift": "// swift-tools-version:5.9\n", "Tests/xTests/a.swift": ""}, "swift test", ""},
		{"swift 没有测试", map[string]string{"Package.swift": "// swift-tools-version:5.9\n"}, "", ""},
		{"swift 只给 iOS", map[string]string{"Package.swift": "platforms: [.iOS(.v17)]\n", "Tests/xTests/a.swift": ""}, "", ""},
		{"swift 也给 macOS", map[string]string{"Package.swift": "platforms: [.iOS(.v17), .macOS(.v14)]\n", "Tests/xTests/a.swift": ""}, "swift test", ""},
		{"什么都没有", map[string]string{"README.md": "x\n"}, "", ""},
		{"两种都有", map[string]string{"go.mod": "module x\n", "package.json": `{"scripts":{"test":"vitest"}}`}, "go test ./...；CI=1 npm test", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, c.files)
			steps := detectVerify(dir)
			if got := verifySummary(steps); got != c.want {
				t.Fatalf("%q，想要 %q", got, c.want)
			}
			if c.setup != "" && (len(steps) == 0 || len(steps[0].Setup) == 0 || steps[0].Setup[0] != c.setup) {
				t.Fatalf("准备命令：%+v", steps)
			}
		})
	}
}

func TestPythonExtra(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n[project.optional-dependencies]\ndocs = [\"a\"]\ndev = [\"pytest\"]\n[tool.x]\ntest = 1\n"})
	steps := detectVerify(dir)
	if len(steps) != 1 || !strings.Contains(strings.Join(steps[0].Setup, "\n"), `-e '.[dev]' pytest`) {
		t.Fatalf("%+v", steps)
	}
}

// 真跑生成的脚本：假工具记下参数；工具没有、依赖装不上是 125；检查失败原样退出。
func runScript(t *testing.T, script string, tools map[string]string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho \""+name+" $* CI=$CI\" >> \"$LOG\"\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "verify")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "log")
	cmd := exec.Command(path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "LOG=" + log}
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	return code, string(b)
}

func TestVerifyScriptRuns(t *testing.T) {
	node := []verifyStep{{Source: "package.json", Need: []string{"npm"}, Setup: []string{"npm install"}, Command: []string{"CI=1 npm test"}}}
	t.Run("通过", func(t *testing.T) {
		code, log := runScript(t, verifyScript(node), map[string]string{"npm": "exit 0"})
		if code != 0 || !strings.Contains(log, "npm install CI=\n") || !strings.Contains(log, "npm test CI=1\n") {
			t.Fatalf("%d\n%s", code, log)
		}
	})
	t.Run("检查没过", func(t *testing.T) {
		code, _ := runScript(t, verifyScript(node), map[string]string{"npm": `[ "$1" = install ] || exit 3`})
		if code != 3 {
			t.Fatal(code)
		}
	})
	t.Run("依赖装不上", func(t *testing.T) {
		code, log := runScript(t, verifyScript(node), map[string]string{"npm": `[ "$1" != install ] || exit 1`})
		if code != 125 || strings.Contains(log, "npm test") {
			t.Fatalf("%d\n%s", code, log)
		}
	})
	t.Run("工具没有", func(t *testing.T) {
		missing := []verifyStep{{Source: "x", Need: []string{"agentq-no-such-tool"}, Command: []string{"agentq-no-such-tool test"}}}
		code, _ := runScript(t, verifyScript(missing), nil)
		if code != 125 {
			t.Fatal(code)
		}
	})
	t.Run("几种都跑，前一种没过就停", func(t *testing.T) {
		both := []verifyStep{{Source: "go.mod", Need: []string{"go"}, Command: []string{"go test ./..."}}, {Source: "Cargo.toml", Need: []string{"cargo"}, Command: []string{"cargo test"}}}
		code, log := runScript(t, verifyScript(both), map[string]string{"go": "exit 2", "cargo": "exit 0"})
		if code != 2 || strings.Contains(log, "cargo") {
			t.Fatalf("%d\n%s", code, log)
		}
		code, log = runScript(t, verifyScript(both), map[string]string{"go": "exit 0", "cargo": "exit 0"})
		if code != 0 || !strings.Contains(log, "cargo test") {
			t.Fatalf("%d\n%s", code, log)
		}
	})
	t.Run("Python 临时虚拟环境清掉", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n[project.optional-dependencies]\ntest = [\"pytest\"]\n"})
		steps := detectVerify(dir)
		fake := `v=""; for a in "$@"; do v="$a"; done
mkdir -p "$v/bin"
printf '#!/bin/sh\nexit 0\n' > "$v/bin/pip"
printf '#!/bin/sh\necho "$0" >> "$LOG"\nexit 0\n' > "$v/bin/python"
chmod +x "$v/bin/pip" "$v/bin/python"`
		code, log := runScript(t, verifyScript(steps), map[string]string{"python3": fake})
		if code != 0 {
			t.Fatalf("%d\n%s", code, log)
		}
		lines := strings.Split(strings.TrimSpace(log), "\n")
		venvPython := lines[len(lines)-1]
		if !strings.HasSuffix(venvPython, "/bin/python") {
			t.Fatalf("没跑到虚拟环境的 python：\n%s", log)
		}
		if _, err := os.Stat(venvPython); !os.IsNotExist(err) {
			t.Fatalf("临时目录没清掉：%s %v", venvPython, err)
		}
	})
}

// 审查实测过的几种「不是提交的问题」：依赖下不来、没有包、没有测试，都不能退出成没通过。真问题照常退出。
func TestVerifyScriptNotCommitProblems(t *testing.T) {
	script := func(files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		writeFiles(t, dir, files)
		steps := detectVerify(dir)
		if len(steps) != 1 {
			t.Fatalf("%+v", steps)
		}
		return verifyScript(steps)
	}
	goScript := script(map[string]string{"go.mod": "module x\n"})
	rust := script(map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"})
	node := script(map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`})
	uv := script(map[string]string{"pyproject.toml": "[dependency-groups]\ndev = [\"pytest\"]\n", "uv.lock": ""})
	poetry := script(map[string]string{"pyproject.toml": "pytest = \"*\"\n", "poetry.lock": ""})
	swift := script(map[string]string{"Package.swift": "x\n", "Tests/a.swift": ""})
	goFake := func(list, test string) string {
		return `case "$1" in mod) exit 0;; list) ` + list + `;; test) ` + test + `;; esac`
	}
	cases := []struct {
		name, script string
		tools        map[string]string
		want         int
		notRun       string
	}{
		{"Go 通过", goScript, map[string]string{"go": goFake("echo x", "exit 0")}, 0, ""},
		{"Go 测试没过", goScript, map[string]string{"go": goFake("echo x", "exit 7")}, 7, ""},
		{"Go 依赖下不来", goScript, map[string]string{"go": `[ "$2" != download ] || exit 1`}, 125, "go test"},
		{"Go 清单写坏", goScript, map[string]string{"go": `[ "$2" != edit ] || exit 1`}, 1, "go test"},
		{"Go 工具本身坏了", goScript, map[string]string{"go": `exit 2`}, 125, "go test"},
		{"Go 没有包", goScript, map[string]string{"go": goFake("exit 0", "exit 1")}, 125, "go test"},
		{"Rust 通过", rust, map[string]string{"cargo": "exit 0"}, 0, ""},
		{"Rust 依赖下不来", rust, map[string]string{"cargo": `[ "$1" != fetch ] || exit 101`}, 125, "cargo test"},
		{"Rust 清单写坏", rust, map[string]string{"cargo": `[ "$1" != metadata ] || exit 101`}, 101, "cargo test"},
		{"Rust 没装工具链", rust, map[string]string{"cargo": `exit 1`}, 125, "cargo test"},
		{"Node 清单写坏", node, map[string]string{"npm": "exit 0", "node": `exit 1`}, 1, "npm"},
		{"Node 依赖下不来", node, map[string]string{"npm": `[ "$1" != install ] || exit 1`, "node": `exit 0`}, 125, "npm test"},
		{"Rust 测试没过", rust, map[string]string{"cargo": `[ "$1" != test ] || exit 101`}, 101, ""},
		{"Swift 依赖下不来", swift, map[string]string{"swift": `[ "$1" != package ] || exit 1`}, 125, "swift test"},
		{"uv 没有测试", uv, map[string]string{"uv": `[ "$1" != run ] || exit 5`}, 0, ""},
		{"uv 测试没过", uv, map[string]string{"uv": `[ "$1" != run ] || exit 1`}, 1, ""},
		{"poetry 没有测试", poetry, map[string]string{"poetry": `[ "$1" != run ] || exit 5`}, 0, ""},
		{"poetry 用法错", poetry, map[string]string{"poetry": `[ "$1" != run ] || exit 4`}, 4, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, log := runScript(t, c.script, c.tools)
			if code != c.want {
				t.Fatalf("退出码 %d，想要 %d\n%s", code, c.want, log)
			}
			if c.notRun != "" && strings.Contains(log, c.notRun) {
				t.Fatalf("不该跑到 %s：\n%s", c.notRun, log)
			}
		})
	}
}

// 真的 go：一个包都没有、依赖不在缓存又不通网（GOPROXY=off），都是 125。
func TestVerifyScriptRealGo(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("这台机器没有 go")
	}
	run := func(t *testing.T, files map[string]string) (int, string) {
		t.Helper()
		dir := t.TempDir()
		writeFiles(t, dir, files)
		path := filepath.Join(t.TempDir(), "verify")
		if err := os.WriteFile(path, []byte(verifyScript(detectVerify(dir))), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(path)
		cmd.Dir = dir
		home := t.TempDir()
		cmd.Env = []string{"PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "HOME=" + home, "GOPATH=" + filepath.Join(home, "gopath"), "GOPROXY=off", "GOTOOLCHAIN=local", "GOCACHE=" + filepath.Join(home, "cache"), "GOFLAGS=-modcacherw"}
		testkit.GoTelemetryOff(t, goBin, cmd.Env)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, string(out)
	}
	t.Run("没有包", func(t *testing.T) {
		code, out := run(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n", "README.md": "x\n"})
		if code != 125 {
			t.Fatalf("%d\n%s", code, out)
		}
	})
	t.Run("依赖下不来", func(t *testing.T) {
		code, out := run(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n\nrequire example.com/nope v1.0.0\n", "a.go": "package x\n"})
		if code != 125 {
			t.Fatalf("%d\n%s", code, out)
		}
	})
	t.Run("go.mod 写坏是没通过", func(t *testing.T) {
		code, out := run(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n\nbroken line\n", "a.go": "package x\n"})
		if code == 0 || code == 125 {
			t.Fatalf("%d\n%s", code, out)
		}
	})
	t.Run("语法错误还是没通过", func(t *testing.T) {
		code, out := run(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n", "a.go": "package x\nfunc {\n"})
		if code == 0 || code == 125 {
			t.Fatalf("%d\n%s", code, out)
		}
	})
}

// 脚本里变量紧跟中文要写 ${变量}，和仓库里脚本的规矩一样；能被 sh 解析。
func TestVerifyScriptShape(t *testing.T) {
	script := verifyScript([]verifyStep{{Source: "go.mod", Need: []string{"go"}, Command: []string{"go test ./..."}}})
	if !strings.HasPrefix(script, "#!/bin/sh\n") || !strings.Contains(script, "按 go.mod 生成") {
		t.Fatal(script)
	}
	if strings.Contains(script, "$1，") {
		t.Fatal(script)
	}
	path := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
