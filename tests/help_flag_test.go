package tests

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/app"
)

// 列在 agentq help 里的全部命令。加命令时这里和用法表一起加。
var helpCommands = []string{
	"init", "add", "start", "env", "version", "--version",
	"tick", "run", "new", "discuss", "status", "projects", "stats", "backlog", "patrol", "gate", "eval", "evolve", "health",
	"logs", "stop", "pause", "resume", "night", "review", "notify", "listen", "install", "uninstall",
	"setup", "update", "doctor", "rollback", "remove", "restore",
}

// treeSnapshot 记下目录下每个文件的内容哈希，比较前后有没有写过东西。
func treeSnapshot(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				snap[p] = "dir"
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			snap[p] = fmt.Sprintf("%x", sha256.Sum256(b))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snap
}

// 每个子命令（含二级命令）见到 -h、--help 都只打印这条命令的用法、退出码 0，什么都不做：
// 不写 BACKLOG、不起任务、不改 state.json，临时 HOME 和 projects_root 下一个字节都不变。
func TestHelpFlagEveryCommand(t *testing.T) {
	t.Parallel()
	if got := app.Commands(); !slices.Equal(got, helpCommands) {
		t.Fatalf("用法表里的命令 %v，和这里列的 %v 对不上", got, helpCommands)
	}
	e := newEnv(t, nil)
	p := e.sgProject("demo", "# 待办\n\n## 待做\n\n- [ ] HELPTODO 做一件事\n")
	e.mustRun("pause")
	e.mustRun("tick") // 暂停着 tick 不起任务，只写出 state.json：先有一份，才看得出改没改
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "state.json")); err != nil {
		t.Fatal(err)
	}
	e.dir = p.Dir // add、init 默认用当前目录
	full := e.mustRun("help")
	before := treeSnapshot(t, e.home, e.root)

	var runs [][]string
	for _, c := range helpCommands {
		runs = append(runs, []string{c, "--help"}, []string{c, "-h"})
	}
	runs = append(runs, // 二级命令和带了别的参数的写法
		[]string{"gate", "report", "--help"}, []string{"gate", "pass", "x", "-h"},
		[]string{"backlog", "archive", "--help"}, []string{"backlog", "archive", "demo", "--help"},
		[]string{"eval", "run", "--help"}, []string{"evolve", "signals", "-h"}, []string{"evolve", "apply", "--help"},
		[]string{"notify", "send", "--help"}, []string{"env", "on", "--help"}, []string{"discuss", "answer", "--help"},
		[]string{"run", "demo", "--help"}, []string{"run", "demo", "HELPTODO", "-h"}, []string{"add", "做点别的", "--help"},
		[]string{"new", "--direction", "x", "--help"}, []string{"start", "--accept-bypass", "--help"},
		[]string{"install", "--yes", "--help"}, []string{"setup", "--yes", "--help"}, []string{"remove", "--yes", "-h"},
	)
	for _, args := range runs {
		out, code := e.run(args...)
		if code != 0 {
			t.Errorf("agentq %s 退出码 %d：\n%s", strings.Join(args, " "), code, out)
			continue
		}
		name := strings.TrimPrefix(args[0], "--")
		if !strings.HasPrefix(out, "用法：\n") || !strings.Contains(out, name) {
			t.Errorf("agentq %s 没打印这条命令的用法：\n%s", strings.Join(args, " "), out)
			continue
		}
		// 和 agentq help 里那几行一字不差：用法文字只有一份
		lines, _, _ := strings.Cut(strings.TrimPrefix(out, "用法：\n"), "\n\n")
		if !strings.Contains(full, lines+"\n") {
			t.Errorf("agentq %s 的用法行和 agentq help 里的不一致：\n%s", strings.Join(args, " "), lines)
		}
	}
	if after := treeSnapshot(t, e.home, e.root); !maps.Equal(before, after) {
		t.Errorf("--help 写了文件：\n前 %v\n后 %v", before, after)
	}
	if n := e.claudeRuns(); n != 0 {
		t.Errorf("--help 起了 %d 个任务", n)
	}
}

// -- 后面的是正文：agentq add -- --help 照常加一条需求，不当成要看用法。
func TestHelpFlagAfterDoubleDash(t *testing.T) {
	e := newEnv(t, nil)
	p := e.sgProject("demo", "# 待办\n\n## 待做\n\n")
	e.dir = p.Dir
	out := e.mustRun("add", "--", "--help")
	if strings.HasPrefix(out, "用法：") {
		t.Fatalf("-- 后面的 --help 被当成看用法：\n%s", out)
	}
	if b := backlogOf(t, p); !strings.Contains(b, "- [ ] --help") {
		t.Fatalf("需求没加进待做：\n%s", b)
	}
}
