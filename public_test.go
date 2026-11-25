package main

import (
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// found 是 re 在 text 里抓到的第一组，去重排好。
func found(re, text string) []string {
	var out []string
	for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// 能公开：README 和程序对得上，跟踪的文件里没有个人痕迹。
func TestPublic(t *testing.T) {
	ls, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	code := read("install.sh")
	for _, f := range strings.Split(strings.Trim(string(ls), "\x00"), "\x00") {
		if strings.HasPrefix(f, "docs/specs/") { // 开发记录，不进公开快照
			continue
		}
		files = append(files, f)
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			code += read(f)
		}
	}

	commands := append(found(`(?m)^  agentq (\w+)`, usage), found(`（(\w+) 卸掉`, usage)...)
	slices.Sort(commands)
	vars := found(`\b(AGENTQ_[A-Z]+(?:_[A-Z]+)*)\b`, code)
	curl := found(`(?m)^#\s+(curl .*)$`, code)
	if len(commands) < 11 || len(vars) < 12 || len(curl) != 1 {
		t.Fatalf("没认全：%v %v %v", commands, vars, curl)
	}
	sections := map[string]int{}
	for _, f := range []string{"README.md", "README.en.md"} {
		text := read(f)
		if got := found("(?m)^\\| `agentq (\\w+)", text); !slices.Equal(got, commands) {
			t.Errorf("%s 的命令表和 --help 对不上：\n%v\n%v", f, got, commands)
		}
		if got := found(`\b(AGENTQ_[A-Z]+(?:_[A-Z]+)*)\b`, text); !slices.Equal(got, vars) {
			t.Errorf("%s 的环境变量和代码对不上：\n%v\n%v", f, got, vars)
		}
		if !strings.Contains(text, "\n"+curl[0]+"\n") {
			t.Errorf("%s 的安装命令和 install.sh 里的不一样：%s", f, curl[0])
		}
		if first, _, _ := strings.Cut(usage, "\n"); !strings.Contains(text, first) {
			t.Errorf("%s 没写 --help 的第一行", f)
		}
		sections[f] = strings.Count(text, "\n## ")
	}
	if sections["README.md"] != sections["README.en.md"] {
		t.Errorf("两份 README 章节不对等：%v", sections)
	}
	if !strings.Contains(read("README.md"), "\n## 从旧版怎么换\n") {
		t.Error("install.sh 指到 README 的「从旧版怎么换」，没有这一节")
	}

	// 拼起来写，免得自己命中
	private := regexp.MustCompile("主" + "人" + `|/Users` + `/[A-Za-z0-9]|[\w.%+-]+@` + `[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	for _, f := range files {
		for i, l := range strings.Split(read(f), "\n") {
			for _, hit := range private.FindAllString(l, -1) {
				if !strings.Contains(hit, "noreply") {
					t.Errorf("%s:%d 不该公开：%s", f, i+1, hit)
				}
			}
		}
	}
}
