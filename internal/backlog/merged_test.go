package backlog

import (
	"strings"
	"testing"
)

func TestMarkMerged(t *testing.T) {
	m := MergedLine{Commit: "abc123456789", Acceptance: "看 README"}
	fp := Fingerprint("- [ ] 做事 —— 要什么")
	body := "# 待办\n\n## 待做\n\n- [ ] 做事 —— 要什么（进行中：agent/x 2026-11-05 10:00）\n  续行也走\n- [ ] 别的\n\n## 等用户决定\n\n## 已完成\n\n- [x] 旧 —— 合并提交 1111111\n"
	got := MarkMerged(body, fp, "做事", "agent/x", m)
	want := "# 待办\n\n## 待做\n\n- [ ] 别的\n\n## 等用户决定\n\n## 已完成\n\n- [x] 做事 —— 要什么；合并提交 abc123456789；验收：看 README\n  续行也走\n\n- [x] 旧 —— 合并提交 1111111\n"
	if got != want {
		t.Errorf("\n%q\n%q", got, want)
	}
	if MarkMerged(got, fp, "做事", "agent/x", m) != got {
		t.Error("重复调用不再改")
	}
	cases := map[string]struct{ body, fp, title, branch, wants, not string }{
		"标了卡住的（指纹变了）": {
			"## 待做\n\n- [ ] 做事 —— 要什么（卡住：门没过，任务 j1，分支 agent/x）\n\n## 已完成\n", fp, "做事", "", "- [x] 做事 —— 要什么；合并提交", "- [ ]",
		},
		"标题里有卡住标记": {
			"## 待做\n\n- [ ] 做事（卡住：门没过，任务 j1）\n\n## 已完成\n", "别的指纹", "做事", "", "- [x] 做事 —— 合并提交", "- [ ]",
		},
		"正文留着（用户的决定、引用的提交）": {
			"## 待做\n\n- [ ] 做事 —— 修 abc1234 的回归（用户决定：A 做法一）（进行中：agent/x 10-10 03:00）\n\n## 已完成\n", "x", "做事", "", "- [x] 做事 —— 修 abc1234 的回归（用户决定：A 做法一）；合并提交 abc123456789", "（进行中",
		},
		"已经被用户移走了也记一行": {
			"## 待做\n\n## 已完成\n", fp, "做事", "agent/x", "- [x] 做事 —— 合并提交", "- [ ]",
		},
		"没有已完成一节": {
			"## 待做\n\n- [ ] 做事 —— 要什么\n", fp, "做事", "", "## 已完成\n\n- [x] 做事 —— 要什么；合并提交", "- [ ]",
		},
		"注释里的不算": {
			"## 待做\n\n<!-- - [ ] 做事 -->\n\n## 已完成\n", fp, "做事", "", "<!-- - [ ] 做事 -->", "",
		},
		"已完成的注释说明留着": {
			"## 待做\n\n- [ ] 做事\n\n## 已完成\n\n<!-- - [x] 标题 —— 合并提交 abc1234 -->\n", "x", "做事", "", "<!-- - [x] 标题", "- [ ]",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := MarkMerged(c.body, c.fp, c.title, c.branch, m)
			if !strings.Contains(got, c.wants) || (c.not != "" && strings.Contains(got, c.not)) {
				t.Errorf("%s", got)
			}
			if strings.Count(got, "\n## 已完成") > 1 {
				t.Errorf("多了一节：%s", got)
			}
		})
	}
	// 尾巴卡在没关的注释里：写在注释前面，第二次还写到同一节，不多出一节
	open := "## 待做\n\n- [ ] 甲\n- [ ] 乙\n<!-- 没关的注释\n- [ ] 注释里的\n"
	got = MarkMerged(open, "x", "甲", "", MergedLine{Commit: "c1"})
	got = MarkMerged(got, "x", "乙", "", MergedLine{Commit: "c2"})
	if strings.Count(got, "## 已完成") != 1 || !strings.Contains(got, "- [x] 甲 —— 合并提交 c1") || !strings.Contains(got, "- [x] 乙 —— 合并提交 c2") {
		t.Errorf("%s", got)
	}
	if i, k := strings.Index(got, "合并提交 c2"), strings.Index(got, "<!-- 没关"); i < 0 || i > k {
		t.Errorf("要在注释前面：%s", got)
	}
	if !strings.Contains(got, "<!-- 没关的注释\n- [ ] 注释里的\n") {
		t.Errorf("注释不动：%s", got)
	}
	// 同一分支的另一条（巡逻写的接手待做）一起走，别的分支的不动
	two := "## 待做\n\n- [ ] 接手中断的「甲」 —— 说明（进行中：agent/x 10-10 03:00）\n- [ ] 甲 —— 原条目（进行中：agent/x 10-09 03:00）\n- [ ] 乙（进行中：agent/y 10-09 03:00）\n\n## 已完成\n"
	got = MarkMerged(two, "没这个指纹", "接手中断的「甲」", "agent/x", m)
	if strings.Contains(got, "- [ ] 甲") || strings.Contains(got, "- [ ] 接手") || !strings.Contains(got, "- [ ] 乙") || strings.Count(got, "合并提交 abc123456789") != 2 {
		t.Errorf("%s", got)
	}
	// 模型写的验收文字进来之前已经洗过：不写成新的一节
	if strings.Contains(MarkMerged("## 待做\n\n## 已完成\n", fp, "做事", "", MergedLine{Commit: "c", Acceptance: CleanTodoText("x\n## 已完成\n（进行中：y）", 100)}), "\n## 已完成\n\n- [x] 做事 —— 合并提交 c；验收：x ## 已完成 (进行中：y）\n") == false {
		t.Errorf("验收文字要压成一行、改掉标记")
	}
}

func TestReclaim(t *testing.T) {
	body := "## 待做\n\n- [ ] 做事 —— 要什么（卡住：门没过：测试没过，任务 j1，分支 agent/x）\n- [ ] 别的（卡住：门没过，任务 j2）\n"
	got := Reclaim(body, "做事", "j1", "（进行中：agent/x 2026-11-05 10:00）")
	want := "## 待做\n\n- [ ] 做事 —— 要什么（进行中：agent/x 2026-11-05 10:00）\n- [ ] 别的（卡住：门没过，任务 j2）\n"
	if got != want {
		t.Errorf("\n%q\n%q", got, want)
	}
	if Reclaim(body, "做事", "别的任务", "（进行中：x）") != body {
		t.Error("任务对不上不动")
	}
	if Reclaim("## 待做\n\n- [ ] 做事（卡住：用户写的）\n", "做事", "j1", "（进行中：x）") != "## 待做\n\n- [ ] 做事（卡住：用户写的）\n" {
		t.Error("用户写的不动")
	}
}

// 同标题的另一条不能一起关：按指纹和分支认，标题只作兜底且只对上一条才算。
func TestMarkMergedSameTitle(t *testing.T) {
	m := MergedLine{Commit: "abc123456789"}
	fp := Fingerprint("- [ ] 修登录 —— 手机号登录")
	body := "## 待做\n\n- [ ] 修登录 —— 手机号登录（进行中：agent/x 10-10 03:00）\n- [ ] 修登录 —— 邮箱登录也要做，还没做\n\n## 已完成\n"
	got := MarkMerged(body, fp, "修登录", "agent/x", m)
	if !strings.Contains(got, "- [ ] 修登录 —— 邮箱登录也要做，还没做") || !strings.Contains(got, "- [x] 修登录 —— 手机号登录；合并提交") {
		t.Errorf("%s", got)
	}
	// 标了卡住：指纹按去掉卡住标记算，同样只关这一条
	stuck := "## 待做\n\n- [ ] 修登录 —— 手机号登录（卡住：门没过：判不了，任务 j1，分支 agent/x）\n- [ ] 修登录 —— 邮箱\n\n## 已完成\n"
	got = MarkMerged(stuck, fp, "修登录", "agent/x", m)
	if !strings.Contains(got, "- [ ] 修登录 —— 邮箱") || !strings.Contains(got, "- [x] 修登录 —— 手机号登录；合并提交") || strings.Contains(got, "（卡住") {
		t.Errorf("%s", got)
	}
	// 指纹变了（任务改过正文）、标记还写着分支：认分支
	got = MarkMerged("## 待做\n\n- [ ] 修登录 —— 改过的正文（进行中：agent/x 10-10 03:00）\n- [ ] 修登录 —— 邮箱\n\n## 已完成\n", fp, "修登录", "agent/x", m)
	if !strings.Contains(got, "- [ ] 修登录 —— 邮箱") || !strings.Contains(got, "- [x] 修登录 —— 改过的正文") {
		t.Errorf("%s", got)
	}
	// 只有标题对得上，两条：认不出就都不动，记一行
	two := "## 待做\n\n- [ ] 修登录 —— 甲\n- [ ] 修登录 —— 乙\n\n## 已完成\n"
	got = MarkMerged(two, "x", "修登录", "", m)
	if !strings.Contains(got, "- [ ] 修登录 —— 甲") || !strings.Contains(got, "- [ ] 修登录 —— 乙") || !strings.Contains(got, "- [x] 修登录 —— 合并提交") {
		t.Errorf("%s", got)
	}
	// 标题只对上一条：兜底
	got = MarkMerged("## 待做\n\n- [ ] 修登录 —— 改过\n- [ ] 别的\n\n## 已完成\n", "x", "修登录", "", m)
	if strings.Contains(got, "- [ ] 修登录") {
		t.Errorf("%s", got)
	}
	// 一模一样的两行：只关一条
	got = MarkMerged("## 待做\n\n- [ ] 修登录 —— 手机号登录\n- [ ] 修登录 —— 手机号登录\n\n## 已完成\n", fp, "修登录", "", m)
	if strings.Count(got, "- [ ] 修登录 —— 手机号登录") != 1 {
		t.Errorf("%s", got)
	}
}
