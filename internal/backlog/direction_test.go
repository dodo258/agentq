package backlog

import (
	"strings"
	"testing"
)

func TestDirectionMarks(t *testing.T) {
	plain := "做主页（大方向） —— 要什么"
	mark := DirectionMark("用户定：纸墨风", 100)
	if mark != "（方向：用户定：纸墨风）" {
		t.Fatalf("%s", mark)
	}
	if Direction(plain) != "" || Direction(plain+mark) != "用户定：纸墨风" {
		t.Errorf("%q", Direction(plain+mark))
	}
	if !DiscussItem(plain) || !DiscussItem("（大方向）做主页") || DiscussItem("做主页 —— 要不要（大方向）再想想") {
		t.Error("只认标题开头、结尾和行尾")
	}
	// 写进方向之后：标题不变，（低风险）、（成品）这些行尾标记还认得出
	if got := ItemTitle(plain + mark); got != "做主页（大方向）" {
		t.Errorf("标题：%q", got)
	}
	if got := ItemTitle("没有破折号（大方向）" + mark); got != "没有破折号（大方向）" {
		t.Errorf("没破折号的标题不带方向：%q", got)
	}
	if !LowRiskItem("改文案 —— 这样改（低风险）"+mark) || !ProductItem("主页 —— 这样（成品）"+mark) {
		t.Error("行尾标记被方向挡住了")
	}
	if !DiscussItem("做主页（大方向）" + mark) {
		t.Error("标记还在")
	}
	// 方向带着分隔符和标记的说法，不能变成别的标记
	item := "做主页（大方向） —— 要什么" + DirectionMark("走 Rust（要快）——（卡住：x）（进行中：y）（等条件：z）（低风险）\n## 已完成", 200)
	it := ParseBacklog("## 待做\n\n- [ ] " + item + "\n")[0]
	if it.Stuck || it.InProgress || LowRiskItem(it.Text) || strings.Contains(it.Text, "\n") {
		t.Errorf("%+v", it)
	}
	if _, ok := WaitCondition(it.Text); ok {
		t.Errorf("%s", it.Text)
	}
	if !strings.HasPrefix(Direction(it.Text), "走 Rust(要快)") {
		t.Errorf("%q", Direction(it.Text))
	}
	if n := len([]rune(DirectionMark(strings.Repeat("长", 500), 50))); n > 60 {
		t.Errorf("要截断：%d", n)
	}
}
