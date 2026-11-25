// Package todo 读写项目的待办 docs/BACKLOG.md：「## 待做」里的 - [ ]，做完挪到「## 已完成」。
package todo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// File 是待办文件。
const File = "docs/BACKLOG.md"

// Item 是一条待办。
type Item struct {
	// Text 是原文，写着做到一半的分支也留着。
	Title, Text string
	// Wait 是它等的那条，空的是能开工。
	Wait string
	// Stuck：标了卡住、等人。
	Stuck bool
}

var (
	depRe  = regexp.MustCompile(`[（(]\s*依赖\s*[：:]\s*([^）)]*)[）)]`)
	noteRe = regexp.MustCompile(`（(进行中|接着做)[^）]*）`)
)

// Title 是一条的标题：「——」前面、括号标记之外的字。
func Title(text string) string {
	t, _, _ := strings.Cut(noteRe.ReplaceAllString(text, ""), "——")
	if i := strings.IndexAny(t, "（"); i > 0 && strings.Contains(t[i:], "：") {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// List 列出待办文件 text 里「待做」的。
func List(text string) []Item {
	var out []Item
	inTodo := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "## ") {
			inTodo = strings.HasPrefix(l, "## 待做")
		}
		if text, ok := strings.CutPrefix(l, "- [ ] "); ok && inTodo {
			out = append(out, Item{Title: Title(text), Text: text, Stuck: strings.Contains(text, "（卡住")})
		}
	}
	for i := range out {
		for _, m := range depRe.FindAllStringSubmatch(out[i].Text, -1) {
			for _, d := range strings.FieldsFunc(m[1], func(r rune) bool { return strings.ContainsRune(",，、", r) }) {
				if j := slices.IndexFunc(out, func(x Item) bool { return strings.Contains(x.Title, strings.TrimSpace(d)) }); j >= 0 && j != i {
					out[i].Wait = out[j].Title
				}
			}
		}
	}
	return out
}

// Add 在「待做」末尾加几条，已经有同名的不加。
func Add(dir string, items []string) error {
	p := filepath.Join(dir, File)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(b)
	if !strings.Contains(text, "\n## 待做") && !strings.HasPrefix(text, "## 待做") {
		text = strings.TrimRight(text, "\n") + "\n\n## 待做\n\n## 已完成\n"
	}
	head, rest, _ := strings.Cut(text, "## 待做")
	body, tail := rest, ""
	if i := strings.Index(rest, "\n## "); i >= 0 {
		body, tail = rest[:i], rest[i:]
	}
	have := List(text)
	for _, it := range items {
		if !slices.ContainsFunc(have, func(x Item) bool { return x.Title == Title(it) }) {
			body = strings.TrimRight(body, "\n") + "\n- [ ] " + strings.Join(strings.Fields(it), " ") + "\n"
		}
	}
	return errors.Join(os.MkdirAll(filepath.Dir(p), 0o755), os.WriteFile(p, []byte(strings.TrimLeft(head, "\n")+"## 待做"+body+tail), 0o644))
}

// Tick 把标题是 title 的那条打勾，挪到「已完成」最前面。没有这条（改了名、人手打过勾）返回 false。
func Tick(dir, title, note string) (bool, error) {
	p := filepath.Join(dir, File)
	b, err := os.ReadFile(p)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(b), "\n")
	i := slices.IndexFunc(lines, func(l string) bool {
		text, ok := strings.CutPrefix(l, "- [ ] ")
		return ok && Title(text) == title
	})
	if i < 0 {
		return false, nil
	}
	lines = slices.Delete(lines, i, i+1)
	if i > 0 && i < len(lines) && lines[i-1] == "" && lines[i] == "" { // 最后一条挪走，不留两个连着的空行
		lines = slices.Delete(lines, i, i+1)
	}
	done := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "## 已完成") })
	if done < 0 {
		lines = strings.Split(strings.TrimRight(strings.Join(lines, "\n"), "\n")+"\n\n## 已完成\n", "\n")
		done = len(lines) - 2
	}
	lines = slices.Insert(lines, done+1, "", fmt.Sprintf("- [x] %s —— %s", title, note))
	if done+4 < len(lines) && lines[done+3] == "" && strings.HasPrefix(lines[done+4], "- ") {
		lines = slices.Delete(lines, done+3, done+4)
	}
	return true, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
}
