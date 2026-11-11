package runner

import (
	"os"
	"strings"
	"testing"
)

// agent-task 第 6 节写的两种卡住标记，引擎都要认得：普通的能再接、取得到分支；等用户的不再接。
func TestRetakeMarkFormatsMatchSkill(t *testing.T) {
	b, err := os.ReadFile("../../skills/agent-task/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := string(b)
	for _, want := range []string{"`（卡住：<卡在哪，错误原文摘要>，分支 <分支名>）`", "`（卡住：等用户：<哪一类>，<要用户定什么>，分支 <分支名>）`", "先查", "官方文档", "上游 issue"} {
		if !strings.Contains(skill, want) {
			t.Fatalf("技能缺 %s", want)
		}
	}
	cases := []struct {
		line, branch string
		wait         bool
	}{
		{"- [ ] 甲 —— x（卡住：测试一直超时 exit 1，分支 agent/1111-a）", "agent/1111-a", false},
		{"- [ ] 甲 —— x（卡住：等用户：付费，要开通 API 账号，分支 agent/1111-a）", "agent/1111-a", true},
		{"- [ ] 甲 —— x（卡住：等 runner 重写）", "", true},
		{"- [ ] 甲 —— x（卡住：和不做清单冲突：不做小程序）", "", true},
		{"- [ ] 甲 —— x（卡住：不用改：main 上已经有了）", "", true},
	}
	for _, c := range cases {
		m := retakeMarkRe.FindStringSubmatch(c.line)
		if m == nil {
			t.Fatalf("认不出 %s", c.line)
		}
		reason := strings.TrimSpace(m[1])
		if waitsUser(reason) != c.wait {
			t.Errorf("%s：等用户 %v", c.line, !c.wait)
		}
		branch := ""
		if b := markBranchRe.FindStringSubmatch(reason); b != nil {
			branch = b[1]
		}
		if branch != c.branch {
			t.Errorf("%s：分支 %q", c.line, branch)
		}
	}
	if retakeMarkRe.MatchString("- [ ] 正文提到卡住：不算 —— x") {
		t.Error("正文提到卡住不是标记")
	}
}
