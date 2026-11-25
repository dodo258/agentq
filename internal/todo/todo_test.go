package todo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const text = `# 待办

## 待做

<!-- - [ ] 标题 —— 例子 -->
- [ ] 讲解区：加标签 —— 说明（范围：a/）（进行中：agent/x 11-25）
- [ ] 抵扣价 —— 说明（依赖：讲解区、做过的）
- [ ] 难的 —— 说明（卡住：等用户）

## 等用户决定

- [ ] 不算

## 已完成

- [x] 以前的 —— 好
`

func TestListAndTick(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, File), []byte(text), 0o644)
	got := List(text)
	if len(got) != 3 || got[0].Title != "讲解区：加标签" || got[0].Wait != "" || got[0].Stuck || got[1].Wait != "讲解区：加标签" || !got[2].Stuck {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got[0].Text, "进行中：agent/x") {
		t.Errorf("做到一半的分支要留给接手的：%s", got[0].Text)
	}
	if ok, err := Tick(dir, "讲解区：加标签", "看标签页"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, File))
	want := `## 已完成

- [x] 讲解区：加标签 —— 看标签页
- [x] 以前的 —— 好
`
	if s := string(b); s[len(s)-len(want):] != want {
		t.Errorf("%s", b)
	}
	if got := List(string(b)); len(got) != 2 || got[0].Wait != "" {
		t.Errorf("等的做完了就能开工：%+v", got)
	}
	if err := Add(dir, []string{"二期 —— 做 （依赖：抵扣价）", "抵扣价 —— 同名的不加"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, File))
	if got := List(string(b)); len(got) != 3 || got[2].Title != "二期" || got[2].Wait != "抵扣价" {
		t.Errorf("%+v", got)
	}
	empty := t.TempDir()
	if err := Add(empty, []string{"甲 —— 做"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(empty, File)); len(List(string(b))) != 1 {
		t.Errorf("%s", b)
	}
	// 打完勾还是干净的 Markdown：没有连着的空行，标题前后空着，末尾一个换行
	for before, want := range map[string]string{
		"## 待做\n\n- [ ] 甲 —— 做\n\n## 已完成\n":                "## 待做\n\n## 已完成\n\n- [x] 甲 —— 好\n",
		"## 待做\n\n- [ ] 甲 —— 做\n- [ ] 乙\n\n## 已完成\n":       "## 待做\n\n- [ ] 乙\n\n## 已完成\n\n- [x] 甲 —— 好\n",
		"## 待做\n\n- [ ] 甲\n":                               "## 待做\n\n## 已完成\n\n- [x] 甲 —— 好\n",
		"## 待做\n\n- [ ] 甲\n\n## 已完成\n\n- [x] 丙\n\n## 别的\n": "## 待做\n\n## 已完成\n\n- [x] 甲 —— 好\n- [x] 丙\n\n## 别的\n",
		"## 待做\n\n- [ ] 甲\n\n## 已完成\n\n## 别的\n\n- 说明\n":    "## 待做\n\n## 已完成\n\n- [x] 甲 —— 好\n\n## 别的\n\n- 说明\n",
		"## 待做\n\n- [ ] 甲\n\n## 已完成\n- [x] 丙\n":            "## 待做\n\n## 已完成\n\n- [x] 甲 —— 好\n- [x] 丙\n",
		"## 已完成\n\n- [x] 丙\n\n## 待做\n\n- [ ] 甲\n\n## 别的\n": "## 已完成\n\n- [x] 甲 —— 好\n- [x] 丙\n\n## 待做\n\n## 别的\n",
	} {
		os.WriteFile(filepath.Join(dir, File), []byte(before), 0o644)
		if ok, err := Tick(dir, "甲", "好"); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, File)); string(b) != want {
			t.Errorf("%q\n得 %q\n要 %q", before, b, want)
		}
	}
	if ok, _ := Tick(dir, "没有的", ""); ok {
		t.Error("没有的不算")
	}
}
