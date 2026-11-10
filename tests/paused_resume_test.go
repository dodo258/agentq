package tests

import (
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// 暂停时你亲手 run 的条目中断了（门退回同理）：tick 照样用同一会话接着做，不用先 resume。
func TestPausedManualRunResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n- [ ] 别碰 —— 细节\n")
	e.mustRun("pause")
	out := e.mustRun("run", "alpha", "中断一")
	testkit.AssertContains(t, out, "已启动任务", "门退回或中断了 tick 也会接着做这一条")
	waitExit(t, e.jobByTitle("中断一").PID, 10*time.Second)
	out = e.mustRun("tick")
	testkit.AssertContains(t, out, "本次收尾 1，启动 1", "续跑任务", "已暂停", "接着做你手动启动的 1 条")
	st := e.state()
	if len(st.Jobs) != 2 {
		t.Fatalf("暂停时只该续跑手动启动的那条，不挑新条目：%+v", st.Jobs)
	}
	orig, res := st.Jobs[0], st.Jobs[1]
	if !orig.Manual || !res.Manual || res.ResumeOf != orig.ID || orig.ResumedBy != res.ID || res.Status != state.StatusRunning {
		t.Fatalf("续跑应接原任务并沿用手动标记：%+v\n%+v", orig, res)
	}
	waitExit(t, res.PID, 10*time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "本次收尾 1，启动 0") {
		t.Fatalf("续跑做完后只收尾：\n%s", out)
	}
}

// tick 自动启动的条目中断后暂停了：tick 不续跑；run 点名这一条就用同一会话接着做，不再报「没有可以做的条目」。
func TestPausedAutoJobRunResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	testkit.MkProject(t, e.root, "alpha", "- [ ] 中断一 —— INTERRUPT SLEEP=1\n")
	e.mustRun("tick")
	orig := e.jobByTitle("中断一")
	if orig.Manual {
		t.Fatalf("tick 启动的不算手动：%+v", orig)
	}
	waitExit(t, orig.PID, 10*time.Second)
	e.mustRun("pause")
	out := e.mustRun("tick")
	testkit.AssertContains(t, out, "本次收尾 1，启动 0", "已暂停")
	if strings.Contains(out, "手动启动") {
		t.Fatalf("没有手动启动的任务时不提：\n%s", out)
	}
	out = e.mustRun("run", "alpha", "中断")
	testkit.AssertContains(t, out, "已接着做中断的任务 "+orig.ID)
	st := e.state()
	if len(st.Jobs) != 2 || st.Jobs[1].ResumeOf != orig.ID || st.Jobs[0].ResumedBy != st.Jobs[1].ID || !st.Jobs[1].Manual || st.Jobs[1].SessionID != "s-int" {
		t.Fatalf("run 应续跑原任务：%+v\n%+v", *st.Jobs[0], *st.Jobs[len(st.Jobs)-1])
	}
	waitExit(t, st.Jobs[1].PID, 10*time.Second)
	e.mustRun("tick")
	if out, code := e.run("run", "alpha", "中断"); code == 0 {
		t.Fatalf("续跑过的任务不该再被 run 接一次：\n%s", out)
	}
}
