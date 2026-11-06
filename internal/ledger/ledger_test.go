package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitCostFooterEdges(t *testing.T) {
	// 被改坏只剩开始标记：原样当正文
	s := "# agentq 任务汇总 2026-10-11\n\n- 01:00 甲\n" + CostBegin + "\n- 02:00 乙\n"
	if body, f := splitCostFooter(s); body != s || f != "" {
		t.Errorf("只剩开始标记时不应丢内容：%q %q", body, f)
	}
	// 接着写两次新小结，坏掉的开始标记后面那行也一直在
	bad := filepath.Join(t.TempDir(), "bad.md")
	os.WriteFile(bad, []byte(s), 0o644)
	for _, f := range []string{CostBegin + "\n一\n" + CostEnd + "\n", CostBegin + "\n二\n" + CostEnd + "\n"} {
		if err := rewriteNightly(bad, "# 标题\n\n", "", &f); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(bad); !strings.Contains(string(b), "- 02:00 乙") || strings.Contains(string(b), "\n一\n") || !strings.HasSuffix(string(b), "二\n"+CostEnd+"\n") {
		t.Errorf("重写两次后应保留「乙」、只剩新小结：%q", b)
	}
	// 文件开头就是小结、后面有保护脚本追加的行：补上标题
	path := filepath.Join(t.TempDir(), "n.md")
	os.WriteFile(path, []byte(CostBegin+"\n旧\n"+CostEnd+"\n- 03:00 已自动回滚\n"), 0o644)
	nf := "新小结\n"
	if err := rewriteNightly(path, "# 标题\n\n", "- 04:00 丙\n", &nf); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "# 标题\n\n- 03:00 已自动回滚\n- 04:00 丙\n\n新小结\n" {
		t.Errorf("得到 %q", b)
	}
}
