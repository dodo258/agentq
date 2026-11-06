package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDecideOptions(t *testing.T) {
	for _, c := range []struct {
		in     string
		labels string
		rec    string
	}{
		{"上哪个 ——（选项：A 做法一｜B 做法二；推荐 A）", "AB", "A"},
		{"（选项：A 一｜B 二｜C 三；推荐 C）上哪个", "ABC", "C"},
		{"上哪个（选项：A 用 Go（标准库）｜B 用 Rust；推荐 B）后面还有字", "AB", "B"},
		{"上哪个（选项：A 一｜B 二｜C 三｜D 四｜E 五｜F 六；推荐 F）", "ABCDEF", "F"},
		// 只认这一种：其余一律照旧推文字
		{"没有选项", "", ""},
		{"正文顺带提到选项：A 或 B，推荐 A", "", ""},
		{"（选项：A 只有一个；推荐 A）", "", ""},
		{"（选项：A 一｜B 二；推荐 C）", "", ""},
		{"（选项：A 一｜A 二；推荐 A）", "", ""},
		{"（选项：A 一｜B 二）", "", ""},
		{"(选项：A 一｜B 二；推荐 A)", "", ""},
		{"（选项:A 一｜B 二；推荐 A）", "", ""},
		{"（选项：A 一|B 二；推荐 A）", "", ""},
		{"（选项：A 一｜B 二；建议 A）", "", ""},
		{"（选项：a 一｜b 二；推荐 a）", "", ""},
		{"（选项：A一｜B二；推荐 A）", "", ""},
		{"（选项：A ｜B 二；推荐 B）", "", ""},
		{"（选项：A 一｜B 二；推荐 A）（选项：A 三｜B 四；推荐 B）", "", ""},
		{"（选项：A 一｜B 二｜C 三｜D 四｜E 五｜F 六｜G 七；推荐 A）", "", ""},
	} {
		opts, rec, ok := ParseDecideOptions(c.in)
		if ok != (c.labels != "") {
			t.Errorf("%q → ok=%v", c.in, ok)
			continue
		}
		var got strings.Builder
		for _, o := range opts {
			got.WriteString(o.Label)
			if o.Text == "" {
				t.Errorf("%q：空做法", c.in)
			}
		}
		if got.String() != c.labels || rec != c.rec {
			t.Errorf("%q → %s %s", c.in, got.String(), rec)
		}
	}
	opts, _, _ := ParseDecideOptions("上哪个（选项：A 用 Go（标准库）｜B 用 Rust；推荐 B）")
	if opts[0].Text != "用 Go（标准库）" {
		t.Errorf("%+v", opts)
	}
}

// 行内写和下一行缩进写都认；缩进的选项只接在紧挨的条目后面；标题指纹不受选项影响。
func TestScanDecisionsOptions(t *testing.T) {
	root := t.TempDir()
	write := func(body string) {
		t.Helper()
		dir := filepath.Join(root, "demo")
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
		if err := os.WriteFile(filepath.Join(dir, "docs", "BACKLOG.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("## 等用户决定\n\n- [ ] 第一件 —— 说明（选项：A 一｜B 二；推荐 A）\n- [ ] 第二件 —— 说明\n  补一句说明\n  （选项：A 三｜B 四；推荐 B）\n- [ ] 第三件\n\n  （选项：A 五｜B 六；推荐 A）\n## 已完成\n  （选项：A 七｜B 八；推荐 A）\n")
	decs, fails := ScanDecisions(root)
	if len(fails) != 0 || len(decs) != 3 {
		t.Fatalf("%+v %v", decs, fails)
	}
	if decs[0].Title != "第一件" || decs[1].Title != "第二件" || decs[2].Title != "第三件" {
		t.Errorf("%+v", decs)
	}
	if _, rec, ok := ParseDecideOptions(decs[1].Text); !ok || rec != "B" {
		t.Errorf("下一行的选项要认：%q", decs[1].Text)
	}
	if strings.Contains(decs[2].Text, "选项") {
		t.Errorf("空行隔开的不算这一条：%q", decs[2].Text)
	}
	// 选项写在标题里：去掉选项后指纹和没写选项时一样
	write("## 等用户决定\n\n- [ ] 同一件事\n")
	plain, _ := ScanDecisions(root)
	write("## 等用户决定\n\n- [ ] 同一件事（选项：A 一｜B 二；推荐 A）\n")
	with, _ := ScanDecisions(root)
	if plain[0].Fingerprint != with[0].Fingerprint || with[0].Title != "同一件事" {
		t.Errorf("%+v %+v", plain[0], with[0])
	}
}

func TestPruneCards(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	s := &State{Cards: map[string]*DecideCard{
		"old":  {At: now.Add(-CardKeep - time.Hour)},
		"new":  {At: now},
		"null": nil,
	}}
	s.PruneCards(now)
	if len(s.Cards) != 1 || s.Cards["new"] == nil {
		t.Errorf("%+v", s.Cards)
	}
	for i := range cardMax + 5 {
		s.Cards["x"+string(rune('a'+i%26))+strings.Repeat("y", i)] = &DecideCard{At: now.Add(-time.Duration(i) * time.Minute)}
	}
	s.PruneCards(now)
	if len(s.Cards) != cardMax {
		t.Errorf("%d", len(s.Cards))
	}
	if s.Cards["new"] == nil {
		t.Error("留最新的")
	}
}

func TestQuietEnd(t *testing.T) {
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 30, 0, 0, time.Local) }
	morning := func(d int) time.Time { return time.Date(2026, 11, d, 8, 0, 0, 0, time.Local) }
	if got := QuietEnd(at(5, 12)); !got.Equal(at(5, 12)) {
		t.Errorf("白天就是现在：%v", got)
	}
	if got := QuietEnd(at(5, 23)); !got.Equal(morning(6)) {
		t.Errorf("夜里是第二天早上：%v", got)
	}
	if got := QuietEnd(at(6, 3)); !got.Equal(morning(6)) {
		t.Errorf("凌晨是当天早上：%v", got)
	}
}
