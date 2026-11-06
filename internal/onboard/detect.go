package onboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// verifyStep 是一种项目类型的检查：从什么文件判断的、要哪些命令、怎么跑。
type verifyStep struct {
	Source  string   // 依据的文件
	Need    []string // 要有的命令，没有就算跑不了（退出码 125），不算没通过
	Check   []string // 不走网络地读一遍项目清单。写坏了是提交的问题，要算没通过，所以放在 Setup 前面
	Setup   []string // 准备依赖，失败同样算跑不了：网络不通不等于提交有问题
	Command []string // 真正的检查
	NoTests int      // 工具用这个退出码说「没有测试」时算通过，0 表示没有这种退出码
}

// detectVerify 按项目根目录的文件判断检查命令。不调模型，判断不了就不写：
// 写错了比没有更糟，测试门没过会退回任务，要花额度。一个仓库有几种就都跑。
func detectVerify(dir string) []verifyStep {
	var steps []verifyStep
	for _, detect := range []func(string) (verifyStep, bool){detectGo, detectNode, detectRust, detectPython, detectSwift} {
		if s, ok := detect(dir); ok {
			steps = append(steps, s)
		}
	}
	return steps
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func detectGo(dir string) (verifyStep, bool) {
	if !fileExists(dir, "go.mod") {
		return verifyStep{}, false
	}
	return verifyStep{Source: "go.mod", Need: []string{"go"}, Check: []string{
		"go version >/dev/null 2>&1 || cannot", // 工具本身坏了（比如 GOROOT 不对），后面报错不能算到提交头上
		"go mod edit -json >/dev/null",
	}, Setup: []string{
		"[ -d vendor ] || go mod download",
		"go list -e ./... 2>/dev/null | grep -q .", // 一个包都没有时 go test 退出 1。-e：包里有语法错误还要列出来，交给 go test 判失败
	}, Command: []string{"go test ./..."}}, true
}

// detectNode 只认 package.json 里写了的 scripts.test。npm init 生成的占位脚本总是失败，不算。
func detectNode(dir string) (verifyStep, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return verifyStep{}, false
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
		Scripts        struct {
			Test string `json:"test"`
		} `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return verifyStep{}, false
	}
	test := strings.TrimSpace(pkg.Scripts.Test)
	if test == "" || strings.Contains(test, "no test specified") {
		return verifyStep{}, false
	}
	pm := "npm"
	switch {
	case strings.HasPrefix(pkg.PackageManager, "pnpm"), fileExists(dir, "pnpm-lock.yaml"):
		pm = "pnpm"
	case strings.HasPrefix(pkg.PackageManager, "yarn"), fileExists(dir, "yarn.lock"):
		pm = "yarn"
	case strings.HasPrefix(pkg.PackageManager, "bun"), fileExists(dir, "bun.lock"), fileExists(dir, "bun.lockb"):
		pm = "bun"
	}
	run := pm + " test"
	if pm == "bun" {
		run = "bun run test" // bun test 是它自带的测试器，不走 scripts.test
	}
	// CI=1：vitest、jest 这类默认进监听模式的工具，没有它会一直不退出
	return verifyStep{Source: "package.json", Need: []string{pm}, Check: []string{
		`! command -v node >/dev/null 2>&1 || node -e "JSON.parse(require('fs').readFileSync('package.json','utf8'))"`,
	}, Setup: []string{pm + " install"}, Command: []string{"CI=1 " + run}}, true
}

func detectRust(dir string) (verifyStep, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return verifyStep{}, false
	}
	cmd := "cargo test"
	if tomlSections(string(b))["workspace"] {
		cmd = "cargo test --workspace"
	}
	return verifyStep{Source: "Cargo.toml", Need: []string{"cargo"}, Check: []string{
		"cargo --version >/dev/null 2>&1 || cannot", // rustup 没装工具链时 cargo 也在，只是跑不了
		"cargo metadata --no-deps --format-version 1 >/dev/null",
	}, Setup: []string{"cargo fetch"}, Command: []string{cmd}}, true
}

// detectPython 要看到 pytest 才算：pyproject.toml 也可能只是别的工具的配置。依赖走项目自己的工具，没有就在临时虚拟环境里装，不碰系统 Python。
func detectPython(dir string) (verifyStep, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil || !strings.Contains(string(b), "pytest") {
		return verifyStep{}, false
	}
	text := string(b)
	switch {
	case fileExists(dir, "uv.lock"):
		return verifyStep{Source: "pyproject.toml", Need: []string{"uv"}, Setup: []string{"uv sync"}, Command: []string{"uv run pytest"}, NoTests: pytestNoTests}, true
	case fileExists(dir, "poetry.lock"):
		return verifyStep{Source: "pyproject.toml", Need: []string{"poetry"}, Setup: []string{"poetry install --no-interaction"}, Command: []string{"poetry run pytest"}, NoTests: pytestNoTests}, true
	case !tomlSections(text)["project"]:
		return verifyStep{}, false // 不是能装的包，依赖在哪说不清
	}
	target := "."
	if extra := pythonTestExtra(text); extra != "" {
		target = ".[" + extra + "]"
	}
	return verifyStep{
		Source: "pyproject.toml",
		Need:   []string{"python3"},
		Setup: []string{
			`venv="$(mktemp -d)"`,
			`trap 'rm -rf "$venv"' EXIT`,
			`python3 -m venv "$venv"`,
			`"$venv/bin/pip" install --quiet -e '` + target + `' pytest`,
		},
		Command: []string{`"$venv/bin/python" -m pytest`},
		NoTests: pytestNoTests,
	}, true
}

// pytestNoTests 是 pytest 收集不到测试时的退出码（https://docs.pytest.org/en/stable/reference/exit-codes.html）。项目还没写测试不是这次提交的问题。
const pytestNoTests = 5

// detectSwift：没有测试目标时 swift test 会报错；只给 iOS 的包在这台机器上编不过，都算判断不了。
func detectSwift(dir string) (verifyStep, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "Package.swift"))
	if err != nil {
		return verifyStep{}, false
	}
	if fi, err := os.Stat(filepath.Join(dir, "Tests")); err != nil || !fi.IsDir() {
		return verifyStep{}, false
	}
	text := string(b)
	if strings.Contains(text, ".iOS(") && !strings.Contains(text, ".macOS(") {
		return verifyStep{}, false
	}
	return verifyStep{Source: "Package.swift", Need: []string{"swift"}, Setup: []string{"swift package resolve"}, Command: []string{"swift test"}}, true
}

var tomlSectionRe = regexp.MustCompile(`(?m)^\s*\[([^\[\]]+)\]\s*(#.*)?$`)

// tomlSections 是文件里出现的小节名。标准库没有 TOML，这里只认小节标题这一种固定格式。
func tomlSections(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range tomlSectionRe.FindAllStringSubmatch(text, -1) {
		out[strings.TrimSpace(m[1])] = true
	}
	return out
}

// pythonTestExtra 找 [project.optional-dependencies] 里写测试依赖的那组。
func pythonTestExtra(text string) string {
	in := false
	found := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if tomlSectionRe.MatchString(line) {
			in = strings.Contains(t, "project.optional-dependencies")
			continue
		}
		if name, _, ok := strings.Cut(t, "="); in && ok {
			found[strings.TrimSpace(name)] = true
		}
	}
	for _, name := range []string{"test", "tests", "dev"} {
		if found[name] {
			return name
		}
	}
	return ""
}

// verifyScript 把判断结果写成脚本。退出码 125（gate.ScriptCannotRun）表示跑不了：工具没有、依赖装不上，不算提交有问题。
// 服务的 PATH 比终端短，这里补上常见的工具位置，放在后面，用户配的优先。
func verifyScript(steps []verifyStep) string {
	var sources []string
	for _, s := range steps {
		sources = append(sources, s.Source)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# agentq 按 %s 生成的检查命令，可以随便改，agentq 不会覆盖。\n", strings.Join(sources, "、"))
	b.WriteString("# 合并前在一次性 worktree 的合并态上跑：退出码 0 算通过，125 算跑不了（不算提交有问题，等你看），其余算没通过，退回任务。\n")
	b.WriteString("set -e\nexport PATH=\"$PATH:$HOME/.local/bin:$HOME/.cargo/bin:$HOME/go/bin:/opt/homebrew/bin:/usr/local/bin\"\n\n")
	b.WriteString("need() {\n\tcommand -v \"$1\" >/dev/null 2>&1 && return 0\n\techo \"找不到 ${1}，这次检查跑不了。工具不在常见位置就改这个脚本。\" >&2\n\texit 125\n}\n")
	b.WriteString("cannot() {\n\techo \"准备这一步没成（依赖下不来，或者没有可测的东西），这次检查跑不了。\" >&2\n\texit 125\n}\n")
	for _, s := range steps {
		b.WriteString("\n")
		for _, n := range s.Need {
			fmt.Fprintf(&b, "need %s\n", n)
		}
		for _, line := range s.Check {
			b.WriteString(line + "\n")
		}
		for _, line := range s.Setup {
			fmt.Fprintf(&b, "%s || cannot\n", line)
		}
		for _, line := range s.Command {
			if s.NoTests != 0 {
				fmt.Fprintf(&b, "%s || { rc=$?; [ \"$rc\" -eq %d ] || exit \"$rc\"; }\n", line, s.NoTests)
				continue
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// verifySummary 是给人看的那一句。
func verifySummary(steps []verifyStep) string {
	var cmds []string
	for _, s := range steps {
		cmds = append(cmds, s.Command...)
	}
	return strings.Join(cmds, "；")
}
