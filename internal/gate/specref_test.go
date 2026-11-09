package gate_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentq/internal/gate"
)

func specFixture(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# 方案\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSpecRefs(t *testing.T) {
	dir := specFixture(t, "docs/specs/1101-a.md", "docs/specs/1108-website.md", "docs/specs/1001-old.md", "docs/specs/0901-older.md", "docs/BACKLOG.md")
	outside := filepath.Join(filepath.Dir(dir), "secret.md")
	os.WriteFile(outside, []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "docs", "link.md")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, item string
		want       []string
	}{
		{"写了路径用写的", "首屏 —— 按方案 `docs/specs/1101-a.md` 第 2 节做", []string{"docs/specs/1101-a.md"}},
		{"没写路径退回最新三份", "首屏 —— 按方案「页面结构」第 2 节做", []string{"docs/specs/1108-website.md", "docs/specs/1101-a.md", "docs/specs/1001-old.md"}},
		{"不提方案就没有", "修个错别字", nil},
		{"不存在、越界、链接、BACKLOG 都不算", "见 docs/specs/none.md、../secret.md、docs/link.md、docs/BACKLOG.md", nil},
		{"去重", "docs/specs/1101-a.md 和 ./docs/specs/1101-a.md", []string{"docs/specs/1101-a.md"}},
	} {
		if got := gate.SpecRefs(dir, c.item, nil); !slices.Equal(got, c.want) {
			t.Errorf("%s：%v，要 %v", c.name, got, c.want)
		}
	}
	// 这次改动里的文件不算方案：任务改的 .md、任务新加或改了的方案都不能拿来引导审查
	changed := []string{"docs/specs/1108-website.md", "docs/guide.md"}
	os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("x"), 0o644)
	if got := gate.SpecRefs(dir, "更新 docs/guide.md 的安装步骤", changed); got != nil {
		t.Errorf("条目要改的 .md 不是方案：%v", got)
	}
	if got := gate.SpecRefs(dir, "按方案第 2 节", changed); !slices.Equal(got, []string{"docs/specs/1101-a.md", "docs/specs/1001-old.md", "docs/specs/0901-older.md"}) {
		t.Errorf("退回时跳过这次改过的方案：%v", got)
	}
	if got := gate.SpecRefs(t.TempDir(), "按方案做", nil); got != nil {
		t.Errorf("没有 docs/specs 就没有：%v", got)
	}
}

// 11-08 官网：界面审查没读到方案，对方案定下的元素提了意见。两个审查提示词都要带上方案路径。
func TestReviewPromptsCarrySpec(t *testing.T) {
	dir := specFixture(t, "docs/specs/1108-website.md")
	c := gate.Candidate{Dir: dir, Item: "首屏 —— 按方案「页面结构」第 2 节做（方向：用户定：极简工具风）", Files: []string{"index.html"}}
	ui := gate.UIPrompt(c, "/shots", []string{"a.png"}, nil)
	for _, want := range []string{"docs/specs/1108-website.md", "照着做了就不提意见", "开工前定的大方向：用户定：极简工具风"} {
		if !strings.Contains(ui, want) {
			t.Errorf("界面审查缺 %q：\n%s", want, ui)
		}
	}
	if r := gate.ReviewPrompt(c); !strings.Contains(r, "docs/specs/1108-website.md") || !strings.Contains(r, "背离了方案哪一节") {
		t.Errorf("代码审查缺方案：\n%s", r)
	}
	c.Item = "修个错别字"
	if ui := gate.UIPrompt(c, "/shots", []string{"a.png"}, nil); strings.Contains(ui, "条目依据的方案") {
		t.Errorf("没提方案不写这一行：\n%s", ui)
	}
}
