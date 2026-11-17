package gate_test

import (
	"strings"
	"testing"

	"agentq/internal/gate"
)

const changelogBase = "# 更新记录\n\n## 未发布\n\n- 旧条目\n\n## v0.1.0\n\n- 第一版\n"

// mainFile 是默认分支上这个文件现在的内容。
func (f *fix) mainFile(name string) string { return f.git("show", "refs/heads/main:"+name) }

func passed(res gate.RunResult, name string) bool {
	for _, r := range res.Results {
		if r.Gate == name && r.Outcome == gate.Pass {
			return true
		}
	}
	return false
}

// 冲突只在 CHANGELOG.md、DECISIONS.md：自动合好、两边条目都在、没有重复的版本标题，门接着跑完合进 main。
func TestAppendOnlyConflictMerges(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"CHANGELOG.md": changelogBase, "docs/DECISIONS.md": "# 决定\n\n## 11-01 起步\n- 用 Go\n"})
	j := f.claim("做事", "agent/x", map[string]string{
		"a.txt":             "任务的改动\n",
		"CHANGELOG.md":      strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n- 任务的条目\n", 1),
		"docs/DECISIONS.md": "# 决定\n\n## 11-01 起步\n- 用 Go\n\n## 11-16 任务的决定\n- 任务选了 A\n",
	}, ready("agent/x"))
	f.commitMain(map[string]string{
		"CHANGELOG.md":      strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n- main 的条目\n", 1),
		"docs/DECISIONS.md": "# 决定\n\n## 11-01 起步\n- 用 Go\n\n## 11-16 main 的决定\n- main 选了 B\n",
	})
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("只追加的记录文件冲突要自动合：%+v", res)
	}
	if !passed(res, "test") {
		t.Errorf("自动合好后门要接着跑：%+v", res.Results)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "CHANGELOG.md") {
		t.Errorf("报告要写明自动合了哪些文件：%q", res.Notes)
	}
	want := "# 更新记录\n\n## 未发布\n\n- main 的条目\n- 任务的条目\n- 旧条目\n\n## v0.1.0\n\n- 第一版\n"
	if got := f.mainFile("CHANGELOG.md") + "\n"; got != want {
		t.Errorf("CHANGELOG 合出来不对：\n%s", got)
	}
	dec := f.mainFile("docs/DECISIONS.md")
	if !strings.Contains(dec, "- main 选了 B\n\n## 11-16 任务的决定\n- 任务选了 A") {
		t.Errorf("DECISIONS 两边的都要在、默认分支的在前：\n%s", dec)
	}
	if f.mainFile("a.txt") != "任务的改动" {
		t.Error("任务的代码改动要进 main")
	}
}

// 代码文件也冲突：照旧退回，记录文件不替任务合。
func TestAppendOnlyWithCodeConflictFails(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"CHANGELOG.md": changelogBase})
	j := f.claim("做事", "agent/x", map[string]string{
		"same.txt":     "分支的",
		"CHANGELOG.md": strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n- 任务的条目\n", 1),
	}, ready("agent/x"))
	f.commitMain(map[string]string{
		"same.txt":     "main 的",
		"CHANGELOG.md": strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n- main 的条目\n", 1),
	})
	before := f.main()
	res := f.run(j)
	if res.Outcome != "fail" || !strings.Contains(res.Reason, "same.txt") {
		t.Fatalf("代码文件冲突要退回：%+v", res)
	}
	if f.main() != before {
		t.Error("冲突不能进 main")
	}
}

// 两边改了记录文件里同一行原有的内容：不是追加，退回任务自己合。
func TestAppendOnlyEditedLineFails(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"CHANGELOG.md": changelogBase})
	j := f.claim("做事", "agent/x", map[string]string{"CHANGELOG.md": strings.Replace(changelogBase, "- 旧条目", "- 任务改的旧条目", 1)}, ready("agent/x"))
	f.commitMain(map[string]string{"CHANGELOG.md": strings.Replace(changelogBase, "- 旧条目", "- main 改的旧条目", 1)})
	res := f.run(j)
	if res.Outcome != "fail" || !strings.Contains(res.Reason, "CHANGELOG.md") {
		t.Fatalf("改了原有的行要退回：%+v", res)
	}
}

// 默认分支刚发了版（「未发布」的条目挪到新版本标题下），任务在「未发布」下加了一条：任务的条目留在「未发布」，不被挪进新版本。
func TestAppendOnlyAfterReleaseStamp(t *testing.T) {
	f := newFix(t, fixBacklog)
	f.commitMain(map[string]string{"CHANGELOG.md": changelogBase})
	j := f.claim("做事", "agent/x", map[string]string{"CHANGELOG.md": strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n- 任务的条目\n", 1)}, ready("agent/x"))
	f.commitMain(map[string]string{"CHANGELOG.md": strings.Replace(changelogBase, "## 未发布\n\n", "## 未发布\n\n## v0.2.0\n\n", 1)})
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	want := "# 更新记录\n\n## 未发布\n\n- 任务的条目\n\n## v0.2.0\n\n- 旧条目\n\n## v0.1.0\n\n- 第一版\n"
	if got := f.mainFile("CHANGELOG.md") + "\n"; got != want {
		t.Errorf("任务的条目要留在「未发布」：\n%s", got)
	}
}

// 两边都新开了同名的版本标题：并到一个标题下，不出两个「## 未发布」。
func TestAppendOnlySameNewHeading(t *testing.T) {
	f := newFix(t, fixBacklog)
	base := "# 更新记录\n\n## v0.1.0\n\n- 第一版\n"
	f.commitMain(map[string]string{"CHANGELOG.md": base})
	j := f.claim("做事", "agent/x", map[string]string{"CHANGELOG.md": strings.Replace(base, "## v0.1.0", "## 未发布\n\n- 任务的条目\n\n## v0.1.0", 1)}, ready("agent/x"))
	f.commitMain(map[string]string{"CHANGELOG.md": strings.Replace(base, "## v0.1.0", "## 未发布\n\n- main 的条目\n\n## v0.1.0", 1)})
	res := f.run(j)
	if res.Outcome != "merged" {
		t.Fatalf("%+v", res)
	}
	got := f.mainFile("CHANGELOG.md")
	if strings.Count(got, "## 未发布") != 1 || !strings.Contains(got, "## 未发布\n\n- main 的条目\n- 任务的条目\n\n## v0.1.0") {
		t.Errorf("同名标题要并成一个：\n%s", got)
	}
}
