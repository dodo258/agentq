package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 公开文档里的相对链接和图片都要真的存在，也不能指向 private_paths：公开候选里没有那些文件。代码块里的不算链接。
func TestReadmeLinksExist(t *testing.T) {
	re := regexp.MustCompile(`\]\(([^)\s]+)\)|(?:src|srcset)="([^"]+)"`)
	fence := regexp.MustCompile("(?s)```.*?```")
	code := regexp.MustCompile("`[^`\n]*`")
	public := map[string]bool{}
	files := publicFiles(t)
	for _, p := range files {
		public[p] = true
	}
	checked := 0
	for _, name := range files {
		if !strings.HasSuffix(name, ".md") && name != "llms.txt" {
			continue
		}
		if strings.HasPrefix(name, "tests/") || strings.HasPrefix(name, "evals/") {
			continue
		}
		checked++
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := code.ReplaceAllString(fence.ReplaceAllString(string(b), ""), "")
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			link := m[1] + m[2]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "#") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			path, _, _ := strings.Cut(link, "#")
			target := filepath.ToSlash(filepath.Join(filepath.Dir(name), path))
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s 链接到不存在的 %s", name, link)
			} else if fi, err := os.Stat(target); err == nil && !fi.IsDir() && !public[target] {
				t.Errorf("%s 链接到不公开的 %s", name, link)
			}
		}
	}
	if checked < 15 {
		t.Errorf("只查了 %d 个文档", checked)
	}
}

// 两种语言的主页要讲同一件事：结构对得上，同一批配图都用上。
func TestReadmeLanguagesMatch(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	zh, en := read("README.md"), read("README.en.md")
	headings := regexp.MustCompile(`(?m)^#{2,3} `)
	if a, b := len(headings.FindAllString(zh, -1)), len(headings.FindAllString(en, -1)); a != b {
		t.Errorf("标题数不一样：中文 %d，英文 %d", a, b)
	}
	for _, img := range []string{"logo-dark.svg", "hero-dark.png", "telegram-dark.png"} {
		if !strings.Contains(zh, img) || !strings.Contains(en, img) {
			t.Errorf("两种语言都要用 %s", img)
		}
	}
	if !strings.Contains(zh, "flow-zh-dark.svg") || !strings.Contains(en, "flow-en-dark.svg") {
		t.Error("流程图要用各自语言的版本")
	}
}

func prepareDir(t *testing.T, readme string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("scripts", "prepare-public.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "prepare-public.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runPrepare(dir string, args ...string) (string, int) {
	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dir, "scripts", "prepare-public.sh")}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out) + err.Error(), -1
	}
	return string(out), 0
}

func TestPreparePublic(t *testing.T) {
	dir := prepareDir(t, strings.Join([]string{
		"curl https://github.com/<owner>/agentq/install.sh",
		"<!-- public-badges: [![Stars](https://img.shields.io/github/stars/<owner>/agentq)](x) -->",
		"before",
		"<!-- pre-release:start -->",
		"公开之前先 clone",
		"<!-- pre-release:end -->",
		"after",
		`<source srcset="docs/brand/readme/a-dark.png">`,
		`<img src="docs/brand/readme/a-light.png" alt="a">`,
		"",
	}, "\n"))
	out, code := runPrepare(dir, "someone/agentq-oss")
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"curl https://github.com/someone/agentq-oss/install.sh",
		"[![Stars](https://img.shields.io/github/stars/someone/agentq-oss)](x)",
		"before",
		"after",
		`<source srcset="https://raw.githubusercontent.com/someone/agentq-oss/main/docs/brand/readme/a-dark.png">`,
		`<img src="https://raw.githubusercontent.com/someone/agentq-oss/main/docs/brand/readme/a-light.png" alt="a">`,
		"",
	}, "\n")
	if string(b) != want {
		t.Errorf("结果不对：\n%s", b)
	}
}

func TestPreparePublicRefusesLeftovers(t *testing.T) {
	dir := prepareDir(t, "把 `<owner>` 换成仓库所在的账号\n")
	out, code := runPrepare(dir, "someone/agentq")
	if code != 1 || !strings.Contains(out, "README.md:1") {
		t.Errorf("还剩 <owner> 应退出码 1 并列出位置：%d\n%s", code, out)
	}
	for _, bad := range []string{"noslash", "a/b/c", "-x/y", "a b/c"} {
		if out, code := runPrepare(dir, bad); code != 2 {
			t.Errorf("%q 应报用法错：%d\n%s", bad, code, out)
		}
	}
}
