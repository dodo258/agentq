package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentq/internal/state"
	"agentq/internal/testkit"
)

// jobsByTitle：同一条的所有任务，按启动先后。
func (e *testEnv) jobsByTitle(title string) []*Job {
	e.t.Helper()
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Title == title {
			out = append(out, j)
		}
	}
	return out
}

// retakeRun：起第一次，等它标卡住退出，再 tick 收尾。返回收尾那次 tick 的输出。
func retakeRun(t *testing.T, e *testEnv, title string) string {
	t.Helper()
	e.mustRun("tick")
	waitExit(t, e.jobByTitle(title).PID, 20*time.Second)
	return e.mustRun("tick")
}

// 第一次卡住、第二次能过：引擎去掉卡住标记、用新会话从上次的分支再接一次，合同带上次的原因和说明、要求先查资料换思路；
// 第二次写了 ready.json，过门合并。全程不用人，中间不推「卡住了」。
func TestIntegrationStuckRetakeThenMerged(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "难的活"
	dir := gateRepo(t, e, "demo", "- [ ] "+title+" —— STUCKRETAKE RETAKEOK")
	retakeRun(t, e, title)
	jobs := e.jobsByTitle(title)
	if len(jobs) != 2 {
		t.Fatalf("第一次卡住后应自动再接一次：%d 个任务\n%s", len(jobs), e.backlogOf(dir))
	}
	first, second := jobs[0], jobs[1]
	if first.Retake != "测试一直超时，分支 agent/t-rt" || first.StuckBranch != "agent/t-rt" || second.RetakeOf != first.ID || !second.Handoff {
		t.Fatalf("第一次 %+v\n第二次 %+v", first, second)
	}
	if strings.Contains(macLines(t, mac), "卡住") {
		t.Fatalf("会自动再接，这时不推：\n%s", macLines(t, mac))
	}
	prompt := testkit.ReadFile(t, filepath.Join(e.home, ".agentq", "jobs", second.ID, "prompt.txt"))
	testkit.AssertContains(t, prompt, "分支 agent/t-rt", "任务 "+first.ID, "测试一直超时", "第一次试了加超时和拆测试", "先查再动手", "官方文档", "上游 issue", "换一种上次没试过的做法", "不另开分支")
	if strings.Contains(prompt, "用户去掉了") {
		t.Fatalf("不是用户去掉的标记：\n%s", prompt)
	}
	j := e.tickUntilGateJob(second.ID)
	if j.Gate.State != state.GateMerged {
		t.Fatalf("第二次应过门合并：%+v %s", j.Gate, j.Summary)
	}
	got := macLines(t, mac)
	testkit.AssertContains(t, got, "demo「难的活」做完了，已合并")
	if strings.Contains(got, "卡住") {
		t.Fatalf("全程不该推卡住：\n%s", got)
	}
	if b := e.backlogOf(dir); strings.Contains(b, "- [ ] "+title) {
		t.Fatalf("合并后条目应移到已完成：\n%s", b)
	}
}

// 两次都卡住：只推一条带两次尝试摘要的通知，不接第三次。
func TestIntegrationStuckRetakeTwiceNotifiesOnce(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "死活"
	dir := gateRepo(t, e, "demo", "- [ ] "+title+" —— STUCKRETAKE")
	retakeRun(t, e, title)
	jobs := e.jobsByTitle(title)
	if len(jobs) != 2 {
		t.Fatalf("应自动再接一次：%d 个任务", len(jobs))
	}
	waitExit(t, jobs[1].PID, 20*time.Second)
	e.mustRun("tick")
	for range 2 { // 再 tick 几次也不接第三次、不重复推
		e.mustRun("tick")
	}
	if jobs := e.jobsByTitle(title); len(jobs) != 2 {
		t.Fatalf("不接第三次：%d 个任务", len(jobs))
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：上游接口一直 500，分支 agent/t-rt）") {
		t.Fatalf("第二次的卡住标记留着等用户：\n%s", b)
	}
	got := macLines(t, mac)
	testkit.AssertContains(t, got, "demo「死活」再接一次还是卡住了",
		"第一次（任务 "+jobs[0].ID+"）", "测试一直超时", "第一次试了加超时和拆测试",
		"第二次（任务 "+jobs[1].ID+"）", "上游接口一直 500", "查到上游 issue 12", "分支 agent/t-rt 留着")
	if n := strings.Count(got, "卡住了"); n != 1 {
		t.Fatalf("只推一条：%d\n%s", n, got)
	}
}

// 必须等用户定的卡住（付费、授权、账号、产品方向）不自动再接，直接推给用户。
func TestIntegrationStuckWaitUserNoRetake(t *testing.T) {
	t.Parallel()
	e, mac := noticeEnv(t)
	title := "要付费的活"
	dir := gateRepo(t, e, "demo", "- [ ] "+title+" —— STUCKRETAKE STUCKUSER")
	retakeRun(t, e, title)
	e.mustRun("tick")
	if jobs := e.jobsByTitle(title); len(jobs) != 1 || jobs[0].Retake != "" {
		t.Fatalf("等用户定的不再接：%+v", jobs)
	}
	if b := e.backlogOf(dir); !strings.Contains(b, "（卡住：等用户：付费") {
		t.Fatalf("标记留着：\n%s", b)
	}
	testkit.AssertContains(t, macLines(t, mac), "demo「要付费的活」卡住了", "等用户：付费")
	if _, err := os.Stat(filepath.Join(e.home, "retake-prompts.txt")); err == nil {
		t.Fatal("不该起再接的任务")
	}
}

// tickUntilGateJob 是按任务 id 的 tickUntilGate：同一条有好几个任务时用。
func (e *testEnv) tickUntilGateJob(id string) *Job {
	e.t.Helper()
	find := func() *Job {
		for _, j := range e.state().Jobs {
			if j.ID == id {
				return j
			}
		}
		e.t.Fatalf("没有任务 %s", id)
		return nil
	}
	waitExit(e.t, find().PID, 20*time.Second)
	e.mustRun("tick")
	j := find()
	if j.Gate == nil || j.Gate.State != state.GateRunning {
		e.t.Fatalf("应该在过门：%s %+v；%s", j.Status, j.Gate, j.Summary)
	}
	waitFor(e.t, 30*time.Second, "门的结果", func() bool {
		_, err := os.Stat(e.gateResult(j))
		return err == nil
	})
	e.mustRun("tick")
	return find()
}
