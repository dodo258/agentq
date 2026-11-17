package tests

import (
	"strings"
	"testing"

	"agentq/internal/testkit"
)

// self_project 排最后：别的项目还有没卡住的待做时，tick 不起 agentq 自己项目的条目；别的项目只剩卡住的，才起它的。
func TestSelfProjectLast(t *testing.T) {
	t.Parallel()
	started := func(e *testEnv, title string) bool {
		for _, j := range e.state().Jobs {
			if j.Title == title {
				return true
			}
		}
		return false
	}

	e := newEnv(t, map[string]any{"self_project": "alpha", "max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 自己一 —— SLEEP=1\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙一 —— SLEEP=1\n")
	out := e.mustRun("tick")
	if !strings.Contains(out, "alpha 是 agentq 自己的项目，别的项目还有待做，先做别的") || !started(e, "乙一") || started(e, "自己一") {
		t.Fatalf("别的项目有活时只该起 beta，并说明 alpha 在等：\n%s", out)
	}

	e = newEnv(t, map[string]any{"self_project": "alpha", "max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 自己一 —— SLEEP=1\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙一 —— 做不了（卡住：等用户）\n")
	out = e.mustRun("tick")
	if strings.Contains(out, "先做别的") || !started(e, "自己一") {
		t.Fatalf("别的项目只剩卡住的，该起 alpha 的条目：\n%s", out)
	}
}

// 别的项目只剩挑不出来的条目（依赖没做完）时不算有活，照样起 self_project 的。
func TestSelfProjectNotStarvedByBlockedItems(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"self_project": "alpha", "max_parallel": 4})
	testkit.MkProject(t, e.root, "alpha", "- [ ] 自己一 —— SLEEP=1\n")
	testkit.MkProject(t, e.root, "beta", "- [ ] 乙一 —— 要等前面（依赖：根本没有这一条）\n")
	out := e.mustRun("tick")
	if strings.Contains(out, "先做别的") {
		t.Fatalf("beta 只剩依赖没做完的条目，不该挡住 alpha：\n%s", out)
	}
	for _, j := range e.state().Jobs {
		if j.Title == "自己一" {
			return
		}
	}
	t.Fatalf("alpha 的条目没起：\n%s", out)
}
