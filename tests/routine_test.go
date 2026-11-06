package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentq/internal/routine"
	"agentq/internal/state"
	"agentq/internal/testkit"
)

func (e *testEnv) routineJob(kind string) *Job {
	e.t.Helper()
	var hit *Job
	for _, j := range e.state().Jobs {
		if j.Kind == kind {
			hit = j
		}
	}
	if hit == nil {
		e.t.Fatalf("没有 %s 任务", kind)
	}
	return hit
}

// 开关没打开时 launchd 叫醒：只记一行日志、退出码 0，不启动 claude、不写汇总。
func TestIntegrationRoutineDisabled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for kind, key := range map[string]string{"night": "night_enabled", "review": "review_enabled"} {
		out := e.mustRun(kind, "--scheduled")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 1 || !strings.Contains(out, key+"=false") || !strings.Contains(out, "不执行") {
			t.Errorf("%s --scheduled 应只记一行日志：\n%s", kind, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, ".agentq", "state.json")); !os.IsNotExist(err) {
		t.Errorf("没执行时不应写状态文件：%v", err)
	}
	if logs, _ := filepath.Glob(filepath.Join(e.fakeLg, "*.txt")); len(logs) != 0 {
		t.Errorf("不应启动 claude：%v", logs)
	}
	if n := e.nightly(); n != "" {
		t.Errorf("不应写汇总：\n%s", n)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "没开：夜班、复盘、巡逻、推送") || strings.Contains(out, "night_enabled") {
		t.Errorf("status 应把没开的功能收成一行，不露配置键名：\n%s", out)
	}
	e.run("patrol") // 手动巡逻过，即使没开自动巡逻，结果也要留在 status 里
	if out := e.mustRun("status"); !strings.Contains(out, "巡逻：tick 不自动巡逻") || !strings.Contains(out, "没开：夜班、复盘、推送") {
		t.Errorf("手动巡逻后 status 应显示结果：\n%s", out)
	}
	if out, code := e.run("night", "--now"); code != 2 {
		t.Errorf("不认识的参数应报用法错：%d\n%s", code, out)
	}
}

// 打开后定点叫醒：用和任务一样的参数、白名单环境启动 claude，在 projects_root 下执行说明文件；
// 不占并发名额、同一种不重复启动；结束后照常收尾、写汇总、logs 能看结果。手动执行不看开关。
func TestIntegrationRoutineRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true, "max_parallel": 1})
	day := filepath.Join(e.home, ".claude", "nightly", time.Now().Format("2006-01-02"))
	e.extra = []string{"AGENTQ_FAKE_SLEEP=3", "AGENTQ_FAKE_WRITE=" + day + "-night.md " + day + ".md"}
	testkit.MkProject(t, e.root, "alpha", "- [ ] 甲一 —— x\n")

	out := e.mustRun("night", "--scheduled")
	if !strings.Contains(out, "夜班已启动") || !strings.Contains(out, "超时 6h") {
		t.Fatalf("夜班应启动：\n%s", out)
	}
	j := e.routineJob("night")
	if j.Status != state.StatusRunning || j.Project != "夜班" || j.ProjectDir != e.root || j.SessionID == "" {
		t.Errorf("夜班任务记录：%+v", j)
	}
	prompt, _ := os.ReadFile(filepath.Join(jobHome(j, e), "prompt.txt"))
	if !strings.HasPrefix(string(prompt), routine.RoutinePromptPrefix) || !strings.Contains(string(prompt), nightPrompt) ||
		!strings.Contains(string(prompt), "agentq resume") {
		t.Errorf("prompt 应是前言加 prompts/night.md：\n%s", prompt)
	}

	// 同一种已经在跑：不重复启动
	if out := e.mustRun("night"); !strings.Contains(out, "已经在跑") {
		t.Errorf("夜班在跑时不应再启动：\n%s", out)
	}
	// 不占并发名额（max_parallel=1 时待做照样能启动），status 里另列、不算进「运行中」
	if out := e.mustRun("tick"); !strings.Contains(out, "运行中 1/1，本次收尾 0，启动 1") {
		t.Errorf("夜班不应占名额：\n%s", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "定点任务在跑：夜班，任务 "+j.ID) || !strings.Contains(out, "运行中（1/1）") {
		t.Errorf("status：\n%s", out)
	}
	// 复盘没打开，手动执行照样跑，并提示开关
	out = e.mustRun("review")
	if !strings.Contains(out, "复盘已启动") || !strings.Contains(out, "review_enabled=false") || !strings.Contains(out, "超时 4h") {
		t.Errorf("手动复盘应启动并提示开关：\n%s", out)
	}
	r := e.routineJob("review")
	rp, _ := os.ReadFile(filepath.Join(jobHome(r, e), "prompt.txt"))
	if !strings.Contains(string(rp), reviewPrompt) {
		t.Errorf("复盘 prompt 应含 prompts/review.md：\n%s", rp)
	}

	waitExit(t, j.PID, 15*time.Second)
	fakeLog, _ := os.ReadFile(filepath.Join(e.fakeLg, fmt.Sprintf("%d.txt", j.PID)))
	for _, want := range []string{"ARG[--session-id]", "ARG[--model]\nARG[opus]", "ARG[--effort]\nARG[high]",
		"ARG[--permission-mode]\nARG[bypassPermissions]", "ARG[--output-format]\nARG[json]",
		"ARG[--disallowed-tools]\nARG[Monitor]", "ARG[--name]\nARG[agentq 夜班 agentq night]",
		"PWD[" + testkit.RealPath(t, e.root) + "]", "ENV[AGENTQ_JOB=" + j.ID + "]"} {
		if !strings.Contains(string(fakeLog), want) {
			t.Errorf("假 claude 收到的参数缺 %q：\n%s", want, fakeLog)
		}
	}
	if strings.Contains(string(fakeLog), "ENV[CLAUDE") {
		t.Errorf("CLAUDE* 环境变量不应传给夜班：\n%s", fakeLog)
	}
	waitExit(t, r.PID, 15*time.Second)
	for _, tk := range e.state().Tasks() {
		waitExit(t, tk.PID, 15*time.Second)
	}
	e.mustRun("tick")
	if j := e.routineJob("night"); j.Status != state.StatusDone || !strings.Contains(j.Summary, "已合并，提交 abc1234") {
		t.Errorf("夜班应跑完（不做中断核验）：%+v", j)
	}
	n := e.nightly()
	if !strings.Contains(n, "夜班「agentq night」跑完：") || !strings.Contains(n, "复盘「agentq review」跑完：") ||
		!strings.Contains(n, "日志 "+jobHome(j, e)) {
		t.Errorf("当天汇总应有夜班和复盘：\n%s", n)
	}
	if out := e.mustRun("logs", j.ID); !strings.Contains(out, "跑完") || !strings.Contains(out, "结果全文") {
		t.Errorf("logs：\n%s", out)
	}
	if st := e.state(); len(st.Running()) != 0 {
		t.Errorf("都应已收尾：%+v", st.Jobs)
	}
}

// 夜班、复盘超过各自的超时（night_timeout 1s、review_timeout 2s，待做的 timeout 不管它们）：tick 杀掉整个进程组，记超时、写汇总。
func TestIntegrationRoutineTimeout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true, "night_timeout": "1s", "review_timeout": "2s", "timeout": "1h"})
	e.extra = []string{"AGENTQ_FAKE_SLEEP=30"}
	e.mustRun("night", "--scheduled")
	e.mustRun("review")
	j, r := e.routineJob("night"), e.routineJob("review")
	time.Sleep(1500 * time.Millisecond)
	out := e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1") || !strings.Contains(out, "运行超过 1s") {
		t.Errorf("tick 应按 night_timeout 杀掉夜班、复盘还没到时间：\n%s", out)
	}
	if got := e.routineJob("night"); got.Status != state.StatusTimeout {
		t.Errorf("夜班应为 timeout：%+v", got)
	}
	if err := syscall.Kill(-j.PID, 0); err == nil {
		t.Errorf("夜班的进程组 %d 应已被杀掉", j.PID)
	}
	time.Sleep(time.Second)
	if out := e.mustRun("tick"); !strings.Contains(out, "运行超过 2s") {
		t.Errorf("tick 应按 review_timeout 杀掉复盘：\n%s", out)
	}
	if err := syscall.Kill(-r.PID, 0); err == nil {
		t.Errorf("复盘的进程组 %d 应已被杀掉", r.PID)
	}
	n := e.nightly()
	if !strings.Contains(n, "夜班「agentq night」超时：运行超过 1s") || !strings.Contains(n, "复盘「agentq review」超时：运行超过 2s") {
		t.Errorf("当天汇总应有夜班、复盘超时：\n%s", n)
	}
}

// 夜班正常退出却没写出夜班报告：按中断记（多半是等通知时提前退出了），不续跑。
func TestIntegrationRoutineNoReport(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true})
	e.mustRun("night", "--scheduled")
	j := e.routineJob("night")
	waitExit(t, j.PID, 10*time.Second)
	e.mustRun("tick")
	got := e.routineJob("night")
	if got.Status != state.StatusInterrupted || got.ResumePending || !strings.Contains(got.Summary, "没写出报告") ||
		!strings.Contains(got.Summary, "-night.md") {
		t.Errorf("没写报告应记中断、不续跑：%+v", got)
	}
	if n := e.nightly(); !strings.Contains(n, "夜班「agentq night」中断：正常退出但没写出报告") {
		t.Errorf("当天汇总：\n%s", n)
	}
}

// 定点叫醒但没登录：不启动，记日志并在当天汇总写一行（早报能看到夜班没跑）。
func TestIntegrationRoutineNotLoggedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"review_enabled": true})
	e.extra = []string{"AGENTQ_FAKE_LOGGED_OUT=1"}
	out, code := e.run("review", "--scheduled")
	if code == 0 || !strings.Contains(out, "复盘没有启动") || !strings.Contains(out, "未登录") {
		t.Errorf("没登录时应拒绝：%d\n%s", code, out)
	}
	if n := e.nightly(); !strings.Contains(n, "复盘没有启动：终端版 Claude 未登录") {
		t.Errorf("当天汇总应记一行：\n%s", n)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
}

// catchUpAt：之后执行的 agentq 都拨到 2026-10-05（本地时区）的 hhmm（AGENTQ_TEST_NOW），其余环境变量不动。
func catchUpAt(e *testEnv, hhmm string) {
	at, err := time.ParseInLocation("2006-01-02 15:04", catchUpDay+" "+hhmm, time.Local)
	if err != nil {
		e.t.Fatal(err)
	}
	var keep []string
	for _, kv := range e.extra {
		if !strings.HasPrefix(kv, "AGENTQ_TEST_NOW=") {
			keep = append(keep, kv)
		}
	}
	e.extra = append(keep, "AGENTQ_TEST_NOW="+at.Format(time.RFC3339))
}

const catchUpDay = "2026-10-05"

func (e *testEnv) catchUpNightly() string {
	b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "nightly", catchUpDay+"-agentq.md"))
	return string(b)
}

func (e *testEnv) routineJobs(kind string) []*Job {
	var out []*Job
	for _, j := range e.state().Jobs {
		if j.Kind == kind {
			out = append(out, j)
		}
	}
	return out
}

// 关机错过了 01:00 夜班和 06:30 复盘，06:50 开机：tick 先补夜班，夜班在跑时复盘先不补；夜班跑完下一次 tick 补复盘。
// 补跑的各在当天汇总写一行「开机后补跑」，之后的 tick 不重复补（跑着、跑完都不补）。
func TestIntegrationRoutineCatchUp(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true, "review_enabled": true})
	day := filepath.Join(e.home, ".claude", "nightly", catchUpDay)
	e.extra = []string{"AGENTQ_FAKE_SLEEP=2", "AGENTQ_FAKE_WRITE=" + day + "-night.md"} // 两个任务都只写夜班报告，复盘那个记中断
	catchUpAt(e, "06:50")

	out := e.mustRun("tick")
	if !strings.Contains(out, "夜班开机后补跑：今天 01:00 那次没跑") || !strings.Contains(out, "复盘：今天 06:30 那次没跑，夜班还在跑，等它跑完再补") {
		t.Fatalf("应补夜班、复盘等夜班：\n%s", out)
	}
	nights := e.routineJobs("night")
	if len(nights) != 1 || nights[0].Status != state.StatusRunning || nights[0].Title != "agentq night（开机后补跑）" ||
		nights[0].StartedAt.Format("2006-01-02 15:04") != catchUpDay+" 06:50" || len(e.routineJobs("review")) != 0 {
		t.Fatalf("应只启动一个补跑夜班：%+v", e.state().Jobs)
	}
	prompt, _ := os.ReadFile(filepath.Join(jobHome(nights[0], e), "prompt.txt"))
	if !strings.HasPrefix(string(prompt), routine.RoutinePromptPrefix+routine.CatchUpNightNote) || !strings.Contains(string(prompt), nightPrompt) {
		t.Errorf("补跑的 prompt 应是前言、补跑说明加夜班说明：\n%s", prompt)
	}
	// 夜班还在跑：不再补夜班
	if out := e.mustRun("tick"); strings.Contains(out, "开机后补跑") || len(e.routineJobs("night")) != 1 {
		t.Errorf("夜班在跑时不应再补：\n%s", out)
	}
	waitExit(t, nights[0].PID, 15*time.Second)

	catchUpAt(e, "07:20")
	out = e.mustRun("tick")
	if !strings.Contains(out, "本次收尾 1") || !strings.Contains(out, "复盘开机后补跑：今天 06:30 那次没跑") {
		t.Fatalf("夜班跑完后应补复盘：\n%s", out)
	}
	if j := e.routineJobs("night")[0]; j.Status != state.StatusDone {
		t.Errorf("夜班应跑完：%+v", j)
	}
	reviews := e.routineJobs("review")
	if len(reviews) != 1 || reviews[0].Title != "agentq review（开机后补跑）" {
		t.Fatalf("应启动一个补跑复盘：%+v", e.state().Jobs)
	}
	waitExit(t, reviews[0].PID, 15*time.Second)
	// 两个都跑完，同一天之后的 tick 都不再补
	for _, hhmm := range []string{"07:30", "23:50"} {
		catchUpAt(e, hhmm)
		if out := e.mustRun("tick"); strings.Contains(out, "补跑") {
			t.Errorf("%s 不应再补：\n%s", hhmm, out)
		}
	}
	if n := len(e.routineJobs("night")) + len(e.routineJobs("review")); n != 2 {
		t.Errorf("同一天只补一次：%+v", e.state().Jobs)
	}
	n := e.catchUpNightly()
	if strings.Count(n, "开机后补跑：") != 2 || !strings.Contains(n, "- 06:50 夜班开机后补跑：今天 01:00 那次没跑（没有当天的任务记录，也没有 "+catchUpDay+"-night.md），任务 ") ||
		!strings.Contains(n, "- 07:20 复盘开机后补跑：今天 06:30 那次没跑（没有当天的任务记录，也没有 "+catchUpDay+".md），任务 ") ||
		!strings.Contains(n, "夜班「agentq night（开机后补跑）」跑完") {
		t.Errorf("当天汇总：\n%s", n)
	}
}

// 已经跑过不补：当天有这一种的任务记录（手动跑的也算），或有它要写的报告（别处跑的）；定点时刻后 15 分钟内、开关关着都不补。
func TestIntegrationRoutineCatchUpAlreadyRan(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true, "review_enabled": true})
	day := filepath.Join(e.home, ".claude", "nightly", catchUpDay)
	e.extra = []string{"AGENTQ_FAKE_SLEEP=1"}
	// 01:10：夜班定点时刻刚过，launchd 那次可能还在查登录、读额度，不补
	catchUpAt(e, "01:10")
	if out := e.mustRun("tick"); strings.Contains(out, "补跑") {
		t.Errorf("定点时刻后 15 分钟内不应补：\n%s", out)
	}
	// 夜班报告已经在了（不是 agentq 跑的），复盘 06:00 被手动跑过（任务记录在今天）
	if err := os.MkdirAll(filepath.Dir(day), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(day+"-night.md", []byte("report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catchUpAt(e, "06:00")
	e.mustRun("review")
	r := e.routineJob("review")
	waitExit(t, r.PID, 15*time.Second)
	catchUpAt(e, "07:00")
	if out := e.mustRun("tick"); strings.Contains(out, "补跑") || !strings.Contains(out, "本次收尾 1") {
		t.Errorf("已经跑过的不应补：\n%s", out)
	}
	if got := e.routineJob("review"); got.Status != state.StatusInterrupted || len(e.routineJobs("review")) != 1 || len(e.routineJobs("night")) != 0 {
		t.Errorf("复盘只该有手动那一次（没写报告记中断，也不补）、夜班没有任务：%+v", e.state().Jobs)
	}
	if n := e.catchUpNightly(); strings.Contains(n, "补跑") {
		t.Errorf("不应写补跑：\n%s", n)
	}

	// 开关关着：错过了也不补
	off := newEnv(t, nil)
	catchUpAt(off, "07:00")
	if out := off.mustRun("tick"); strings.Contains(out, "补跑") {
		t.Errorf("开关关着不应补：\n%s", out)
	}
}

// 09:00 才开机：夜班已过 08:00 不补，复盘照补；补复盘没能启动（未登录）只记日志，登录后下次 tick 再补。
func TestIntegrationRoutineCatchUpLate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true, "review_enabled": true})
	e.extra = []string{"AGENTQ_FAKE_SLEEP=1", "AGENTQ_FAKE_LOGGED_OUT=1"}
	catchUpAt(e, "09:00")
	out := e.mustRun("tick")
	if strings.Contains(out, "夜班") || !strings.Contains(out, "复盘：今天 06:30 那次没跑，开机后补跑没有启动：终端版 Claude 未登录") {
		t.Fatalf("没登录时复盘补跑应只记日志、夜班不补：\n%s", out)
	}
	if st := e.state(); len(st.Jobs) != 0 {
		t.Errorf("不应有任务：%+v", st.Jobs)
	}
	e.extra = []string{"AGENTQ_FAKE_SLEEP=1"}
	catchUpAt(e, "09:05")
	out = e.mustRun("tick")
	if strings.Contains(out, "夜班") || !strings.Contains(out, "复盘开机后补跑：今天 06:30 那次没跑") {
		t.Fatalf("过 08:00 夜班不补、复盘照补：\n%s", out)
	}
	if len(e.routineJobs("night")) != 0 || len(e.routineJobs("review")) != 1 {
		t.Errorf("应只补复盘：%+v", e.state().Jobs)
	}
	if n := e.catchUpNightly(); strings.Contains(n, "夜班") || strings.Count(n, "复盘开机后补跑") != 1 {
		t.Errorf("当天汇总：\n%s", n)
	}
	waitExit(t, e.routineJob("review").PID, 15*time.Second)
}

// 前一天 23:30 手动跑的夜班过了 0 点还在跑：今天没有夜班记录，但不能再补一个（两个夜班会各合并、各传一次 TestFlight）。
func TestIntegrationRoutineCatchUpOvernight(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"night_enabled": true})
	e.extra = []string{"AGENTQ_FAKE_SLEEP=30"}
	at, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-04 23:30", time.Local)
	e.extra = append(e.extra, "AGENTQ_TEST_NOW="+at.Format(time.RFC3339))
	e.mustRun("night")
	j := e.routineJob("night")
	catchUpAt(e, "01:20")
	if out := e.mustRun("tick"); strings.Contains(out, "补跑") || len(e.routineJobs("night")) != 1 {
		t.Errorf("前一天的夜班还在跑时不应补跑：\n%s\n%+v", out, e.state().Jobs)
	}
	e.mustRun("stop", j.ID)
}
