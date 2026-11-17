package tests

import (
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 说明书检查（自动发版前 .agentq/release --check 跑的就是这两个用例，平时完整检查也跑）：
// 主页中英文写的命令和选项在 --help 里都有；两种语言、主页和支持矩阵里的版本号、支持范围对得上。

// manualSnippets：文档里的代码块逐行，加上正文里的行内代码。
func manualSnippets(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
	var out []string
	for _, m := range fence.FindAllStringSubmatch(string(b), -1) {
		out = append(out, strings.Split(m[1], "\n")...)
	}
	for _, m := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(fence.ReplaceAllString(string(b), ""), -1) {
		out = append(out, m[1])
	}
	return out
}

var (
	manualCmd  = regexp.MustCompile(`(?:^|[\s(|;&])agentq\s+([a-z][a-z_-]*)([^#|;&]*)`)
	manualSh   = regexp.MustCompile(`(?:install\.sh|sh -s --)([^#|;&]*)`)
	manualFlag = regexp.MustCompile(`(?:^|\s)(--[a-z][a-z0-9-]*)`)
)

func TestManualCommandsInHelp(t *testing.T) {
	help := map[string]string{}
	helpOf := func(args ...string) (string, bool) {
		key := strings.Join(args, " ")
		if h, ok := help[key]; ok {
			return h, h != "\x00"
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		help[key] = string(out)
		if err != nil {
			help[key] = "\x00"
		}
		return help[key], err == nil
	}
	checked := 0
	for _, name := range []string{"README.md", "README.en.md"} {
		for _, sn := range manualSnippets(t, name) {
			for _, m := range manualCmd.FindAllStringSubmatch(sn, -1) {
				checked++
				h, ok := helpOf(agentqBin, m[1], "--help")
				if !ok {
					t.Errorf("%s 写了 `agentq %s`，可它不是 agentq 的命令（agentq %s --help 没过）", name, m[1], m[1])
					continue
				}
				for _, f := range manualFlag.FindAllStringSubmatch(m[2], -1) {
					if !strings.Contains(h, f[1]) {
						t.Errorf("%s 写了 `agentq %s %s`，agentq %s --help 里没有 %s", name, m[1], f[1], m[1], f[1])
					}
				}
			}
			for _, m := range manualSh.FindAllStringSubmatch(sn, -1) {
				h, ok := helpOf("sh", "install.sh", "--help")
				if !ok {
					t.Fatal("sh install.sh --help 没过")
				}
				for _, f := range manualFlag.FindAllStringSubmatch(m[1], -1) {
					if !strings.Contains(h, f[1]) {
						t.Errorf("%s 写了安装参数 %s，sh install.sh --help 里没有", name, f[1])
					}
				}
			}
		}
	}
	if checked < 20 {
		t.Errorf("只查到 %d 处命令，文档的写法可能变了，检查跟着改", checked)
	}
}

var manualVersion = regexp.MustCompile(`\bv?\d+\.\d+\.\d+\b`)

func versionsIn(s string) []string {
	v := manualVersion.FindAllString(s, -1)
	slices.Sort(v)
	return slices.Compact(v)
}

// supportRows：主页「支持范围」一节表格里每一行写到的版本号。
func supportRows(t *testing.T, name, heading string) [][]string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	_, sec, ok := strings.Cut(string(b), "\n## "+heading+"\n")
	if !ok {
		t.Fatalf("%s 没有「## %s」一节", name, heading)
	}
	sec, _, _ = strings.Cut(sec, "\n## ")
	var rows [][]string
	for _, l := range strings.Split(sec, "\n") {
		if strings.HasPrefix(l, "| **") {
			rows = append(rows, versionsIn(l))
		}
	}
	return rows
}

func TestManualVersionsMatch(t *testing.T) {
	zh, en := supportRows(t, "README.md", "支持范围"), supportRows(t, "README.en.md", "Support")
	if len(zh) != len(en) || len(zh) == 0 {
		t.Fatalf("主页支持范围的行数对不上：中文 %d，英文 %d", len(zh), len(en))
	}
	for i := range zh {
		if !slices.Equal(zh[i], en[i]) {
			t.Errorf("主页支持范围第 %d 行的版本号对不上：中文 %v，英文 %v", i+1, zh[i], en[i])
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mzh, men := versionsIn(read("docs/reference/support-matrix.md")), versionsIn(read("docs/reference/support-matrix.en.md"))
	if !slices.Equal(mzh, men) {
		t.Errorf("支持矩阵中英文写的版本号对不上：中文 %v，英文 %v", mzh, men)
	}
	for _, name := range []string{"README.md", "README.en.md"} {
		for _, v := range versionsIn(read(name)) {
			if !slices.Contains(mzh, v) {
				t.Errorf("%s 写了 %s，支持矩阵里没有", name, v)
			}
		}
	}
}
