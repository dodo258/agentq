package backlog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 依赖、范围只认括号标记；正文和 dispatch 模板里不带括号的「范围：」「依赖」不算。
func TestDepsScopeMarks(t *testing.T) {
	cases := []struct {
		text        string
		deps, scope []string
	}{
		{"加导出（依赖：存储层）（依赖：接口）（范围：internal/gate/, docs/specs/）", []string{"存储层", "接口"}, []string{"internal/gate/", "docs/specs/"}},
		{"加导出 —— x（范围：a/、b.go，./c/）(范围: `d/`)", nil, []string{"a/", "b.go", "c/", "d/"}},
		{"加导出（范围：.）", nil, []string{""}},
		{"加导出（范围：/abs/x）（依赖：  ）", nil, []string{"abs/x"}},
		// 反例：顺带提到不算
		{"加导出 —— 要什么；验收：看；范围：已知页面/文件；理解成：……", nil, nil},
		{"加导出 —— 依赖：存储层先做完，范围：a/", nil, nil},
		{"讲讲依赖注入和范围查询 —— 正文提到依赖、范围", nil, nil},
		{"加导出 ——（依赖说明见文档）（范围说明：没有）", nil, nil},
	}
	for _, c := range cases {
		if got := ItemDeps(c.text); !slices.Equal(got, c.deps) {
			t.Errorf("%q 依赖：%q，想要 %q", c.text, got, c.deps)
		}
		if got := ItemScope(c.text); !slices.Equal(got, c.scope) {
			t.Errorf("%q 范围：%q，想要 %q", c.text, got, c.scope)
		}
	}
	// 标记不进标题，也不挡（成品）这类行尾标记
	if got := ItemTitle("加导出（成品）（依赖：存储层）（范围：a/）"); got != "加导出（成品）" {
		t.Errorf("标题：%q", got)
	}
	if !ProductItem("加导出 —— x（成品）（范围：a/）") {
		t.Error("行尾（成品）后面跟范围标记也要认")
	}
}

func TestScopesOverlap(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"internal/gate/"}, []string{"internal/runner/"}, false},
		{[]string{"internal/"}, []string{"internal/gate/x.go"}, true},
		{[]string{"a.go"}, []string{"a.go"}, true},
		{[]string{"x/", "docs/"}, []string{"y/", "docs/a.md"}, true},
		{[]string{""}, []string{"y/"}, true}, // 整个仓库
	}
	for _, c := range cases {
		if _, _, got := ScopesOverlap(c.a, c.b); got != c.want {
			t.Errorf("%q %q：%v", c.a, c.b, got)
		}
	}
}

func TestParallelBlock(t *testing.T) {
	it := func(text string) Item { return ParseBacklog("## 待做\n\n- [ ] " + text + "\n")[0] }
	cases := []struct {
		item    string
		running []string
		ok      bool
	}{
		{"甲", nil, true}, // 没有在跑的：照旧
		{"甲（范围：a/）", []string{"乙（范围：b/）"}, true},               // 都写了、不重叠
		{"甲", []string{"乙（范围：b/）"}, false},                     // 自己没写
		{"甲（范围：a/）", []string{"乙"}, false},                     // 在跑的没写
		{"甲（范围：a/）", []string{""}, false},                      // 在跑的没有条目（任务箱、新项目）
		{"甲（范围：a/）", []string{"乙（范围：b/）", "丙（范围：a/x）"}, false}, // 和其中一个重叠
		{"甲 —— 范围：a/", []string{"乙（范围：b/）"}, false},            // 正文写的不算范围
	}
	for _, c := range cases {
		if why := ParallelBlock(it(c.item), c.running); (why == "") != c.ok {
			t.Errorf("%q 和 %q：%q", c.item, c.running, why)
		}
	}
}

func TestDepsResolve(t *testing.T) {
	text := `# 待办

## 待做

- [ ] 接口层（依赖：存储层）（范围：api/）
- [ ] 存储层 —— 先做（范围：store/）
- [ ] 前端（依赖：接口层）（依赖：设计稿）
- [ ] 写错了（依赖：不存在的活）
- [ ] 依赖自己（依赖：依赖自己）
- [ ] 正文提到 —— 依赖：存储层

## 等用户决定

- [ ] 设计稿 —— 等你定

## 已完成

- [x] 基础设施 —— 合并提交 abc1234
`
	items := ParseBacklog(text)
	by := map[string]Item{}
	for _, it := range items {
		by[it.Title] = it
	}
	check := func(title string, waiting, missing []string) {
		t.Helper()
		it := by[title]
		if !slices.Equal(it.DepsWaiting, waiting) || !slices.Equal(it.DepsMissing, missing) {
			t.Errorf("%s：等 %q 找不到 %q", title, it.DepsWaiting, it.DepsMissing)
		}
	}
	check("接口层", []string{"存储层"}, nil)
	check("存储层", nil, nil)
	check("前端", []string{"接口层", "设计稿"}, nil) // 别的节里没勾的也算在等
	check("写错了", []string{"不存在的活"}, []string{"不存在的活"})
	check("依赖自己", []string{"依赖自己"}, []string{"依赖自己"}) // 自己不算
	check("正文提到", nil, nil)
	if n := by["写错了"].DepNote(); !strings.Contains(n, "依赖『不存在的活』找不到") {
		t.Errorf("说明：%q", n)
	}
	if by["接口层"].DepNote() != "" {
		t.Error("只是在等的不用说")
	}

	// 存储层做完移到「已完成」：接口层的依赖满足
	done := strings.Replace(text, "- [ ] 存储层 —— 先做（范围：store/）\n", "", 1) + "- [x] 存储层 —— 先做（范围：store/）；合并提交 def5678\n"
	for _, it := range ParseBacklog(done) {
		if it.Title == "接口层" && len(it.DepsWaiting) != 0 {
			t.Errorf("存储层做完了还在等：%+v", it)
		}
	}

	// 已经归档的也算做完
	dir := t.TempDir()
	p := Project{Name: "demo", Dir: dir}
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(p.BacklogPath(), []byte("## 待做\n\n- [ ] 新活（依赖：老活）\n\n## 已完成\n"), 0o644)
	got, _ := p.Items()
	if len(got[0].DepsMissing) != 1 {
		t.Fatalf("没归档时老活找不到：%+v", got[0])
	}
	os.WriteFile(p.ArchivePath(), []byte(ArchiveHead+"- [x] 老活 —— 合并提交 1234567\n"), 0o644)
	got, _ = p.Items()
	if len(got[0].DepsWaiting) != 0 || got[0].Project != "demo" {
		t.Errorf("归档里的算做完：%+v", got[0])
	}
}
